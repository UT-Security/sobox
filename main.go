package main

import (
	"bufio"
	"debug/elf"
	_ "embed"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"text/template"

	"golang.org/x/exp/maps"
)

//go:embed embed/libinit.c
var libinit string

//go:embed embed/cbtrampolines.s
var cbtrampolines string

//go:embed embed/stub.s.in
var stub string

//go:embed embed/trampolines.s.in
var trampolines string

//go:embed embed/includes.c.in
var includes string

// Symbols inside the sandbox needed by libsobox
var sbxsyms = []string{
	"retfn",
	"moz_arena_malloc",
	"moz_arena_calloc",
	"moz_arena_realloc",
	"free",
}

func fatal(err ...interface{}) {
	fmt.Fprintln(os.Stderr, err...)
	os.Exit(1)
}

func run(command string, args ...string) {
	cmd := exec.Command(command, args...)
	log.Println(cmd)
	cmd.Stderr = os.Stderr
	cmd.Stdout = os.Stdout
	cmd.Stdin = os.Stdin
	err := cmd.Run()
	if err != nil {
		fatal(err)
	}
}

func ident(s string) string {
	rgx := regexp.MustCompile("[-./+]")
	return rgx.ReplaceAllString(s, "__")
}

func execTemplate(w io.Writer, name string, data string, vars map[string]any, funcs template.FuncMap) {
	tmpl := template.New(name)
	tmpl.Funcs(funcs)
	tmpl, err := tmpl.Parse(data)
	if err != nil {
		log.Fatal(err)
	}
	err = tmpl.Execute(w, vars)
	if err != nil {
		log.Fatal(err)
	}
}

func genIncludes(filemap map[string]string, w io.Writer) {
	files := maps.Keys(filemap)
	sort.Strings(files)

	execTemplate(w, "includes", includes, map[string]any{
		"files":  files,
		"nfiles": len(files),
	}, map[string]any{
		"ident": ident,
		"filename": func(s string) string {
			if name, ok := filemap[s]; ok {
				return name
			}
			log.Fatalf("no location given for file %s\n", s)
			return ""
		},
	})
}

func genTrampolines(symnames []string, w io.Writer) {
	execTemplate(w, "trampolines", trampolines, map[string]any{
		"syms":     symnames,
		"nsyms":    len(symnames),
		"sbxsyms":  sbxsyms,
		"nsbxsyms": len(sbxsyms),
	}, nil)
}

func genStub(symnames []string, w io.Writer) {
	execTemplate(w, "stub", stub, map[string]any{
		"syms":    symnames,
		"sbxsyms": sbxsyms,
	}, nil)
}

func getSoExports(ef *elf.File) []string {
	syms, err := ef.Symbols()
	if err != nil {
		log.Fatal(err)
	}

	var exports []elf.Symbol
	for _, sym := range syms {
		if elf.ST_BIND(sym.Info) == elf.STB_GLOBAL && elf.ST_TYPE(sym.Info) == elf.STT_FUNC && sym.Section != elf.SHN_UNDEF {
			if sym.Name == "_init" || sym.Name == "_fini" {
				// Musl inserts these symbols on shared libraries, but after we
				// compile the stub they will be linked internally, and should
				// not be exported.
				continue
			}
			exports = append(exports, sym)
		}
	}

	var exportnames []string
	for _, sym := range exports {
		exportnames = append(exportnames, sym.Name)
	}

	return exportnames
}

func getFileExports(symFile string) []string {
	f, err := os.Open(symFile)
	if err != nil {
		log.Fatal(err)
	}

	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Split(bufio.ScanLines)

	var exportnames []string

	for scanner.Scan() {
		exportnames = append(exportnames, scanner.Text())
	}

	return exportnames
}

func getname(solib string) string {
	before, _, _ := strings.Cut(solib, ".so")
	return before
}

func libname(solib string) string {
	_, after, _ := strings.Cut(getname(filepath.Base(solib)), "lib")
	return after
}

func getenv(varname string, defval string) string {
	v := os.Getenv(varname)
	if v != "" {
		return v
	}
	return defval
}

func main() {
	objmap := make(map[string]string)

	outflag := flag.String("o", "", "output file")

	flag.Func("map", "map object to file", func(s string) error {
		parts := strings.SplitN(s, "=", 2)
		if len(parts) != 2 {
			return fmt.Errorf("invalid format: %s", s)
		}
		objmap[parts[0]] = parts[1]
		return nil
	})
	
	symFileFlag := flag.String("s", "", "exported symbols file")

	flag.Parse()
	args := flag.Args()
	if len(args) <= 0 {
		log.Fatal("no input")
	}
	solib := args[0]
	f, err := os.Open(solib)
	if err != nil {
		log.Fatal(err)
	}
	ef, err := elf.NewFile(f)
	if err != nil {
		log.Fatal(err)
	}

	symFile := *symFileFlag

	var exportnames []string
	if symFile == "" {
		exportnames = getSoExports(ef)
	} else {
		exportnames = getFileExports(symFile)
	}
	
	gen := "gen"
	os.MkdirAll(gen, os.ModePerm)

	fstub, err := os.Create(filepath.Join(gen, "stub.s"))
	if err != nil {
		log.Fatal(err)
	}
	ftrampolines, err := os.Create(filepath.Join(gen, "trampolines.s"))
	if err != nil {
		log.Fatal(err)
	}
	fincludes, err := os.Create(filepath.Join(gen, "includes.c"))
	if err != nil {
		log.Fatal(err)
	}
	flibinit, err := os.Create(filepath.Join(gen, "libinit.c"))
	if err != nil {
		log.Fatal(err)
	}
	_, err = flibinit.WriteString(libinit)
	if err != nil {
		log.Fatal(err)
	}
	fcbtramp, err := os.Create(filepath.Join(gen, "cbtrampolines.s"))
	if err != nil {
		log.Fatal(err)
	}
	_, err = fcbtramp.WriteString(cbtrampolines)
	if err != nil {
		log.Fatal(err)
	}

	stubgen := filepath.Join(gen, "stub.elf")

	lficc := getenv("LFICC", "x86_64-lfi-linux-musl-gcc")

	genStub(exportnames, fstub)

	genTrampolines(exportnames, ftrampolines)

	fstub.Close()
	ftrampolines.Close()

	run(lficc, fstub.Name(), "-o", stubgen, "-L"+filepath.Dir(solib), "-l"+libname(solib), "-lstdc++")
	run("patchelf", "--set-interpreter", "ld-musl-x86_64.so.1", stubgen)
	objmap["stub"] = stubgen

	objs, err := ef.DynString(elf.DT_NEEDED)
	if err != nil {
		log.Fatal(err)
	}
	for _, obj := range objs {
		ok := false

		_, hasLinker := objmap["ld-musl-x86_64.so.1"]
		if obj == "libc.so" && hasLinker {
			ok = true
		}

		for _, mapped := range objmap {
			if strings.Contains(mapped, obj) {
				ok = true
			}
		}
		if !ok {
			log.Fatalf("error: library requires %s, but no object provided", obj)
		}
	}

	genIncludes(objmap, fincludes)

	fincludes.Close()

	//cc := getenv("CC", "gcc")

	out := *outflag
	if out == "" {
		out = getname(solib) + ".box.so"
	}

	//run(cc, fincludes.Name(), ftrampolines.Name(), flibinit.Name(), fcbtramp.Name(), "-I/home/abhishekcs/workspace/lfi-toolchains/lfi-repo-syscalls/lfi-toolchain/include", "-L/home/abhishekcs/workspace/lfi-toolchains/lfi-repo-syscalls/lfi-toolchain/lib/x86_64-linux-gnu/", "-llfix", "-shared", "-O2", "-fPIC", "-o", out)
}
