// for memfd_create
#define _GNU_SOURCE
#include <assert.h>
#include <stdint.h>
#include <stdbool.h>
#include <stddef.h>
#include <stdio.h>
#include <string.h>
#include <unistd.h>
#include <stdlib.h>
#include <sys/mman.h>

#include "lfi.h"
#include "lfi_tux.h"

enum {
    PATH_MAX = 64,
};

struct FileData {
    uint8_t* start;
    uint8_t* end;
};

struct File {
    struct FileData* data;
    FILE* file;
};

static bool cbinit(struct LFIContext* ctx);

extern void sbx_cbtrampoline();

extern char* sbx_filenames[];
extern struct FileData sbx_filedata[];
extern size_t sbx_nfiles;

extern void* __lfi_trampotable[];
extern size_t __lfi_trampotable_size;
extern char* __lfi_trampolines;

extern void* __lfisym__lfi_pause;
extern void* __lfisym__lfi_thread_create;

static struct FileData*
findfiledata(const char* filename)
{
    for (size_t i = 0; i < sbx_nfiles; i++) {
        if (strncmp(filename, sbx_filenames[i], PATH_MAX) == 0)
            return &sbx_filedata[i];
    }
    return NULL;
}

static void
install_trampotable(void** table)
{
    for (size_t i = 0; i < __lfi_trampotable_size; i++) {
        __lfi_trampotable[i] = table[i];
    }
}

static struct HostFile*
fsopen(const char* filename, int flags, int mode)
{
    (void) flags, (void) mode;
    struct FileData* data = findfiledata(filename);
    if (data) {
        int fd = memfd_create("", 0);
        if (fd < 0)
            return NULL;
        if (ftruncate(fd, data->end - data->start) < 0)
            goto err;
        if (write(fd, data->start, data->end - data->start) < 0)
            goto err;
        lseek(fd, 0, SEEK_SET);
        return lfi_host_fdopen(fd);
err:
        close(fd);
        return NULL;
    }
    return NULL;
}

static struct Tux* tux;

__attribute__((constructor)) void
sbx_init(void)
{
    struct LFIPlatform* plat = lfi_new_plat(getpagesize());
    if (!plat) {
        fprintf(stderr, "sobox: error loading LFI: %s\n", lfi_strerror());
        exit(1);
    }

    tux = lfi_tux_new(plat, (struct TuxOptions) {
        .pagesize = getpagesize(),
        .stacksize = 2 * 1024 * 1024,
        .pause_on_exit = true,
        .fs = (struct TuxFS) {
            .open = fsopen,
        },
    });

    if (!tux) {
        fprintf(stderr, "sobox: error loading LFI: %s\n", lfi_strerror());
        exit(1);
    }

    struct FileData* stub = findfiledata("stub");
    if (!stub) {
        fprintf(stderr, "sobox: error loading LFI: could not find stub file\n");
        exit(1);
    }

    char* args[] = {"stub", NULL};
    struct TuxThread* p = lfi_tux_proc_new(tux, stub->start, stub->end - stub->start, 1, &args[0]);
    if (!p) {
        fprintf(stderr, "sobox: error creating LFI process: %s\n", lfi_strerror());
        exit(1);
    }

    bool ok = cbinit(lfi_tux_ctx(p));
    if (!ok) {
        fprintf(stderr, "sobox: failed to initialize callback entries\n");
        exit(1);
    }

    lfi_tux_soboxinit(tux, true);

    uint64_t r = lfi_tux_proc_run(p);
    if (r < 256) {
        fprintf(stderr, "sobox: failed to start LFI process\n");
        exit(1);
    }

    lfi_tux_soboxinit(tux, false);

    // TODO: validate the trampoline table (its location) before installing it,
    // since intalling it involves reading from sandbox memory. Possibly also
    // validate its contents (not entirely necessary if the trampoline uses
    // uses a safe bundle-aligned jump for entry, but probably still a good
    // idea).
    install_trampotable((void**) r);
}

void
sbx_setup(struct LFIContext* ctx)
{
    lfi_thread_init(__lfisym__lfi_thread_create, __lfisym__lfi_pause);
}

void*
sbx_stackpush(size_t n)
{
    (void) n;
    assert(!"unimplemented");
}

size_t
sbx_stackpop(size_t n)
{
    (void) n;
    assert(!"unimplemented");
}

enum {
    MAXCALLBACKS = 4096,
};

static void* callbacks[MAXCALLBACKS];

static ssize_t
cbfreeslot()
{
    for (ssize_t i = 0; i < MAXCALLBACKS; i++) {
        if (!callbacks[i])
            return i;
    }
    return -1;
}

static ssize_t
cbfind(void* fn)
{
    for (size_t i = 0; i < MAXCALLBACKS; i++) {
        if (callbacks[i] == fn)
            return i;
    }
    return -1;
}

struct CallbackEntry {
    uint8_t code[16];
    uint64_t target;
    uint64_t trampoline;
};

// Code for a callback trampoline.
static uint8_t cbtrampoline[16] = {
   0x4c, 0x8b, 0x15, 0x09, 0x00, 0x00, 0x00, // mov    0x9(%rip),%r10
   0xff, 0x25, 0x0b, 0x00, 0x00, 0x00,       // jmp    *0xb(%rip)
   0x0f, 0x01f, 0x00,                        // nop
};

static struct CallbackEntry* cbentries_alias;
static struct CallbackEntry* cbentries_box;

static bool
cbinit(struct LFIContext* ctx)
{
    int fd = memfd_create("", 0);
    if (fd < 0)
        return false;
    size_t size = MAXCALLBACKS * sizeof(struct CallbackEntry);
    int r = ftruncate(fd, size);
    if (r < 0)
        goto err;
    // Map callback entries outside the sandbox as read/write.
    void* aliasmap = mmap(NULL, size, PROT_READ | PROT_WRITE, MAP_SHARED, fd, 0);
    if (aliasmap == (void*) -1)
        goto err;
    cbentries_alias = (struct CallbackEntry*) aliasmap;
    // Fill in the code for each entry.
    for (size_t i = 0; i < MAXCALLBACKS; i++) {
        memcpy(&cbentries_alias[i].code, &cbtrampoline[0], sizeof(cbentries_alias[i].code));
    }
    struct HostFile* hf = lfi_host_fdopen(fd);
    assert(hf);
    // Share the mapping inside the sandbox as read/exec.
    lfiptr_t boxmap = lfi_as_mapany(lfi_ctx_as(ctx), size, PROT_READ | PROT_EXEC, MAP_SHARED, hf, 0);
    if (boxmap == (lfiptr_t) -1)
        goto err1;
    cbentries_box = (struct CallbackEntry*) boxmap;
    return true;
err1:
    munmap(aliasmap, size);
err:
    close(fd);
    return false;
}

void*
sbx_register_cb(void* fn, size_t stackframe)
{
    assert(fn);
    assert(cbfind(fn) == -1 && "fn is already registered as a callback");

    ssize_t slot = cbfreeslot();
    if (slot == -1)
        return NULL;

    // TODO: support non-zero stackframes: create a trampoline for 'stackframe'
    // if it does not exist
    if (stackframe != 0)
        return NULL;

    // write 'fn' into the 'target' field for the chosen slot.
    __atomic_store_n(&cbentries_alias[slot].target, (uint64_t) fn, __ATOMIC_SEQ_CST);
    // write the trampoline into the 'trampoline' field for the chosen slot
    __atomic_store_n(&cbentries_alias[slot].trampoline, (uint64_t) sbx_cbtrampoline, __ATOMIC_SEQ_CST);

    // Mark the slot as allocated.
    callbacks[slot] = fn;

    return &cbentries_box[slot].code[0];
}

void
sbx_unregister_cb(void* fn)
{
    ssize_t slot = cbfind(fn);
    if (slot == -1)
        return;
    callbacks[slot] = NULL;
    __atomic_store_n(&cbentries_alias[slot].target, 0, __ATOMIC_SEQ_CST);
    __atomic_store_n(&cbentries_alias[slot].trampoline, 0, __ATOMIC_SEQ_CST);
}

void*
sbx_addr(void* sym)
{
    const size_t trampoline_size = 16;
    for (size_t i = 0; i < __lfi_trampotable_size; i++) {
        if (&__lfi_trampolines[i * trampoline_size] == sym)
            return __lfi_trampotable[i];
    }
    return NULL;
}
