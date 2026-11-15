package main

import (
	"bytes"
	"context"
	"flag"
	"go/ast"
	"go/build/constraint"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func TestFilenameTagIsConjoined(t *testing.T) {
	// _amd64.go 里的 //go:build arm64 || amd64 不能丢掉文件名条件，
	// 否则这份只在 amd64 上存在的源码会被标成 arm64 也能编。
	got := mustFileConstraint(t, "foo_amd64.go", "//go:build arm64 || amd64\n\npackage p\nfunc F() {}\n")
	if !evalConstraint(t, got, "amd64") {
		t.Fatalf("amd64 should match %q", got)
	}
	if evalConstraint(t, got, "arm64") {
		t.Fatalf("arm64 must not match filename-tagged file, got %q", got)
	}
}

func TestPlusBuildUsedWhenNoGoBuild(t *testing.T) {
	got := mustFileConstraint(t, "f.go", "// +build linux\n\npackage p\nfunc F() {}\n")
	if !evalConstraint(t, got, "linux") || evalConstraint(t, got, "windows") {
		t.Fatalf("legacy +build ignored: %q", got)
	}

	// 同一文件里有 //go:build 时，// +build 必须作废。
	got = mustFileConstraint(t, "f.go", "//go:build windows\n// +build linux\n\npackage p\nfunc F() {}\n")
	if !evalConstraint(t, got, "windows") || evalConstraint(t, got, "linux") {
		t.Fatalf("go:build should win, got %q", got)
	}
}

func TestPlusBuildAtomsForDiscover(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.go")
	if err := os.WriteFile(path, []byte("// +build linux\n\npackage p\nfunc F() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pos, _ := fileConstraintAtoms(path)
	if !slices.Contains(pos, "linux") {
		t.Fatalf("discover atoms dropped // +build: %v", pos)
	}

	both := filepath.Join(dir, "g.go")
	src := "//go:build windows\n// +build linux\n\npackage p\nfunc F() {}\n"
	if err := os.WriteFile(both, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	pos, _ = fileConstraintAtoms(both)
	if slices.Contains(pos, "linux") || !slices.Contains(pos, "windows") {
		t.Fatalf("discover should ignore +build once //go:build is present: %v", pos)
	}
}

func TestCoexistingDeclsUseAnd(t *testing.T) {
	dir := writeModule(t, map[string]string{
		"f_linux.go": "//go:build linux\n\npackage p\n\nfunc L() int { return 1 }\n",
		"f_amd64.go": "//go:build amd64\n\npackage p\n\nfunc A() int { return 2 }\n",
	})
	st := extract(t, dir, func(cfg *Config) {
		cfg.Names = []string{"L", "A"}
		cfg.Targets = []Target{{GOOS: "linux", GOARCH: "amd64"}}
	})
	if len(st.Files) != 1 {
		t.Fatalf("files: %d", len(st.Files))
	}
	got := st.Files[0].Constraint
	if !evalConstraint(t, got, "linux", "amd64") {
		t.Fatalf("linux/amd64 should match %q", got)
	}
	if evalConstraint(t, got, "windows", "amd64") || evalConstraint(t, got, "linux", "arm64") {
		t.Fatalf("OR-ed constraint is too wide: %q", got)
	}
	if !strings.Contains(string(st.Files[0].Content), generatedMarker) {
		t.Fatalf("missing generated header:\n%s", st.Files[0].Content)
	}
}

func TestFollowDepthCountsPackageHops(t *testing.T) {
	dir := writeModule(t, map[string]string{
		"p/p.go": "package p\n\nimport \"example.com/m/b\"\n\nfunc Start() int { return b.Mid() }\n",
		"b/b.go": "package b\n\nimport \"example.com/m/c\"\n\nfunc Mid() int { return c.Leaf() }\n",
		"c/c.go": "package c\n\nfunc Leaf() int { return 1 }\n",
	})
	one := extract(t, dir, func(cfg *Config) {
		cfg.Pattern = "./p"
		cfg.Names = []string{"Start"}
		cfg.Follow = []string{"all"}
		cfg.FollowDepth = 1
	})
	got := declNamesOf(one)
	if slices.Contains(got, "Leaf") || !slices.Contains(got, "Mid") || !slices.Contains(got, "Start") {
		t.Fatalf("depth 1 = %v, want Start+Mid and not Leaf", got)
	}
	two := extract(t, dir, func(cfg *Config) {
		cfg.Pattern = "./p"
		cfg.Names = []string{"Start"}
		cfg.Follow = []string{"all"}
		cfg.FollowDepth = 2
	})
	got = declNamesOf(two)
	if !slices.Contains(got, "Leaf") {
		t.Fatalf("depth 2 = %v, want Leaf inlined", got)
	}
}

func TestConstKeepsSourceOrder(t *testing.T) {
	dir := writeModule(t, map[string]string{
		"p.go": "package p\n\nconst (\n\tZ = 1\n\tA = Z + 1\n)\n\nfunc Use() int { return A }\n",
	})
	st := extract(t, dir, func(cfg *Config) {
		cfg.Names = []string{"Use"}
	})
	src := string(st.Files[0].Content)
	if strings.Index(src, "Z") > strings.Index(src, "A = Z") {
		t.Fatalf("const reordered:\n%s", src)
	}
	mustTypecheck(t, src)
}

// TestGroupDocCommentKeepsHeaderOutOfTheCopy 回归：整组声明（const/type/var (...))
// 带 doc 注释时，注释属于 GenDecl，但拆分后只能带走注释本身。
// 曾经把 "const (" 这段头一起复制了，生成的是
//
//	// Maximum string length constants
//	const (
//		const LINE_LEN = 256
//
// 括号永远不闭合，后面每条声明都报 expected 'IDENT', found 'const'，
// emit 阶段直接失败，一个文件都写不出来（x/sys/windows 里就有这种组）。
func TestGroupDocCommentKeepsHeaderOutOfTheCopy(t *testing.T) {
	dir := writeModule(t, map[string]string{
		"p.go": "package p\n\n" +
			"// group doc for consts\nconst (\n\tLineLen = 256\n\tNameMax = LineLen + 4\n)\n\n" +
			"// group doc for types\ntype (\n\tHandle  uintptr\n\tDevInfo Handle\n)\n\n" +
			"// group doc for vars\nvar (\n\tErrA = 1\n\tErrB = 2\n)\n\n" +
			"func Use() int { return LineLen + NameMax + ErrA + ErrB }\n",
	})
	st := extract(t, dir, func(cfg *Config) {
		cfg.Names = []string{"*"}
	})
	src := string(st.Files[0].Content)
	mustTypecheck(t, src)

	for _, want := range []string{
		"// group doc for consts\nconst LineLen",
		"// group doc for types\ntype Handle",
		"// group doc for vars\nvar ErrA",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("generated code misses %q:\n%s", want, src)
		}
	}
	for _, bad := range []string{"const (", "type (", "var ("} {
		if strings.Contains(src, bad) {
			t.Errorf("split declaration kept the group header %q:\n%s", bad, src)
		}
	}
}

// TestSortDeclsIgnoresFileSetLayout 回归：go/packages 并行解析一个包的文件，
// FileSet 里的 base 因此每次运行都可能不同。按 token.Pos 排序会让输出顺序
// （以及报错行号）在两次相同调用之间变化，所以排序键必须是
// 「文件名 + 文件内偏移」。
func TestSortDeclsIgnoresFileSetLayout(t *testing.T) {
	layout := func(files ...string) []*Decl {
		fset := token.NewFileSet()
		var decls []*Decl
		for _, name := range files {
			f := fset.AddFile(name, -1, 64)
			base := f.Base()
			// 一个文件两条声明，偏移固定，只有 base 随添加顺序变化。
			for i, off := range []int{10, 30} {
				decls = append(decls, &Decl{
					Kind:   "type",
					Name:   strings.TrimSuffix(filepath.Base(name), ".go") + itoa(i),
					OrdPos: token.Pos(base + off),
					Idx:    &Index{fset: fset},
				})
			}
		}
		return decls
	}
	names := func(ds []*Decl) string {
		sortDecls(ds)
		out := make([]string, 0, len(ds))
		for _, d := range ds {
			out = append(out, d.Name)
		}
		return strings.Join(out, ",")
	}
	one := names(layout("pkg/a.go", "pkg/b.go"))
	two := names(layout("pkg/b.go", "pkg/a.go"))
	if one != two {
		t.Fatalf("declaration order depends on FileSet layout: %q vs %q", one, two)
	}
	if one != "a0,a1,b0,b1" {
		t.Fatalf("order = %q, want file order with source offsets", one)
	}
}

func TestMultiNameSpecEmittedOnce(t *testing.T) {
	dir := writeModule(t, map[string]string{
		"p.go": "package p\n\nvar a, b = 1, 2\n\nfunc F() int { return a + b }\n",
	})
	st := extract(t, dir, func(cfg *Config) {
		cfg.Names = []string{"F"}
	})
	src := string(st.Files[0].Content)
	if n := strings.Count(src, "a, b"); n != 1 {
		t.Fatalf("var a, b emitted %d times:\n%s", n, src)
	}
	mustTypecheck(t, src)
}

func TestOverwriteNeedsHeaderOrForce(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "p.go")
	body := []byte(generatedHeader + "\npackage p\n")
	cfg := &Config{Out: path}
	w := fileWriter{}
	files := []File{{Name: "p.go", Content: body}}
	if err := w.Write(context.Background(), files, cfg); err != nil {
		t.Fatal(err)
	}
	if err := w.Write(context.Background(), files, cfg); err != nil {
		t.Fatalf("generated file should be replaceable: %v", err)
	}
	if err := os.WriteFile(path, []byte("package p\nfunc hand() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := w.Write(context.Background(), files, cfg); err == nil {
		t.Fatal("hand-written file was overwritten without -force")
	}
	cfg.Force = true
	if err := w.Write(context.Background(), files, cfg); err != nil {
		t.Fatalf("-force: %v", err)
	}
}

func TestForceFlagBinds(t *testing.T) {
	cfg := DefaultConfig()
	fs := flag.NewFlagSet("extractor", flag.ContinueOnError)
	nv := bindFlags(fs, cfg)
	if err := fs.Parse([]string{"-force", "-pkg", "p", "-names", "T"}); err != nil {
		t.Fatal(err)
	}
	if err := nv.apply(cfg); err != nil {
		t.Fatal(err)
	}
	if !cfg.Force {
		t.Fatal("-force did not bind")
	}
}

func TestStdoutReportStaysOffSource(t *testing.T) {
	dir := writeModule(t, map[string]string{
		"p.go": "package p\n\nfunc F() int { return 1 }\n",
	})
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	var stdout, stderr bytes.Buffer
	code := cliMain([]string{"run", "-dir", dir, "-pkg", ".", "-names", "F", "-o", "-"}, &stdout, &stderr)
	w.Close()
	os.Stdout = old
	var src bytes.Buffer
	if _, err := io.Copy(&src, r); err != nil {
		t.Fatal(err)
	}
	if code != 0 {
		t.Fatalf("cli exit %d\nstderr: %s", code, stderr.String())
	}
	if !strings.Contains(src.String(), "package p") || strings.Contains(src.String(), "declaration(s)") {
		t.Fatalf("stdout is not clean Go:\n%s", src.String())
	}
	if strings.Contains(stdout.String(), "package p") {
		t.Fatalf("human report writer received source:\n%s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "declaration(s)") {
		t.Fatalf("report missing from stderr:\n%s", stderr.String())
	}
}

func mustFileConstraint(t *testing.T, name, src string) string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, name, src, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	return fileConstraint(fset, f)
}

func evalConstraint(t *testing.T, expr string, tags ...string) bool {
	t.Helper()
	e, err := constraint.Parse("//go:build " + expr)
	if err != nil {
		t.Fatalf("parse %q: %v", expr, err)
	}
	set := map[string]bool{}
	for _, tag := range tags {
		set[tag] = true
	}
	return e.Eval(func(tag string) bool { return set[tag] })
}

func writeModule(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	files["go.mod"] = "module example.com/m\n\ngo 1.25\n"
	for name, src := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func extract(t *testing.T, dir string, tune func(*Config)) *State {
	t.Helper()
	cfg := DefaultConfig()
	cfg.Dir = dir
	cfg.Pattern = "."
	cfg.DryRun = true
	cfg.IncludeMethods = false
	cfg.IncludeCtors = false
	cfg.Targets = []Target{{GOOS: runtime.GOOS, GOARCH: runtime.GOARCH}}
	tune(cfg)
	if err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}
	st, err := New(cfg).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func declNamesOf(st *State) []string {
	var out []string
	for _, sel := range st.Selections {
		for _, d := range sel.Decls {
			out = append(out, d.Name)
		}
	}
	return out
}

func mustTypecheck(t *testing.T, src string) {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "out.go", src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse: %v\n%s", err, src)
	}
	conf := types.Config{Importer: importer.Default()}
	if _, err := conf.Check("p", fset, []*ast.File{f}, nil); err != nil {
		t.Fatalf("typecheck: %v\n%s", err, src)
	}
}
