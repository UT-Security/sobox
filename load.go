package main

import (
	"bytes"
	"debug/elf"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/erikgeiser/ar"
	demangle "github.com/ianlancetaylor/demangle"
)

var exposed = []string{
	"_lfi_retfn",
	"_lfi_pause",
	"_lfi_thread_create",
	"_lfi_thread_destroy",
	"moz_arena_malloc",
	"moz_arena_calloc",
	"moz_arena_realloc",
	"free",
}

func IsExport(sym string, exports map[string]bool) bool {
	dsym := demangle.Filter(sym)
	_, after, found := strings.Cut(dsym, " ")

	if strings.HasPrefix(dsym, "js::") || strings.HasPrefix(dsym, "JS::") || strings.HasPrefix(dsym, "JS_") || strings.Contains(dsym, "ProfilingStack") || strings.Contains(dsym, "JSStructuredCloneData") || strings.Contains(dsym, "JSAutoRealm") || strings.Contains(dsym, "JSAutoStructuredCloneBuffer") || strings.Contains(dsym, "JSErrorReport") || strings.Contains(dsym, "JSErrorNotes") || strings.Contains(dsym, "JSAutoNullableRealm") || strings.Contains(dsym, "JSPrincipalsWithOps") {
		return true
	} else if found && (strings.HasPrefix(after, "js::") || strings.HasPrefix(after, "JS::") || strings.HasPrefix(after, "JS_")) {
		return true
	}

	return false
}

func ObjGetExports(file *elf.File, es map[string]bool) []string {
	syms, err := file.Symbols()
	if err != nil {
		fatal(err)
	}
	var exports []string
	for _, sym := range syms {
		if IsExport(sym.Name, es) && ((elf.ST_BIND(sym.Info) == elf.STB_GLOBAL || elf.ST_BIND(sym.Info) == elf.STB_WEAK) && elf.ST_TYPE(sym.Info) == elf.STT_FUNC && sym.Section != elf.SHN_UNDEF) {
			if sym.Name == "_init" || sym.Name == "_fini" {
				// Musl inserts these symbols on shared libraries, but after we
				// compile the stub they will be linked internally, and should
				// not be exported.
				continue
			}
			exports = append(exports, sym.Name)
		}
	}
	return exports
}

func DynamicGetExports(dynlib *os.File, es map[string]bool) []string {
	f, err := elf.NewFile(dynlib)
	if err != nil {
		fatal(err)
	}
	return ObjGetExports(f, es)
}

func StaticGetExports(staticlib *os.File, es map[string]bool) []string {
	r, err := ar.NewReader(staticlib)
	if err != nil {
		fatal(err)
	}
	var exports []string
	for {
		_, err := r.Next()
		if err != nil {
			break
		}
		data, err := io.ReadAll(r)
		if err != nil {
			continue
		}
		b := bytes.NewReader(data)
		ef, err := elf.NewFile(b)
		if err != nil {
			continue
		}
		exports = append(exports, ObjGetExports(ef, es)...)
		// ObjGetStackArgs(ef, es)
	}
	return exports
}

type StackArg struct {
	Offset uint64
	Size   uint64
}

func ObjGetStackArgs(file *elf.File, es map[string]bool) map[string][]StackArg {
	sec := file.Section(".stack_args")
	if sec == nil {
		return nil
	}
	entries := sec.Size / 8
	b := make([]byte, 8)
	for i := 0; i < int(entries); i++ {
		sec.ReadAt(b, int64(i*8))
		val := binary.LittleEndian.Uint64(b)
		fmt.Println(file, val)
	}
	return nil
}
