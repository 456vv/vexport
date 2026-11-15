package main

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

func TestParseTarget(t *testing.T) {
	cases := []struct {
		in   string
		want Target
	}{
		{"host", hostTarget()},
		{"", hostTarget()},
		{"linux/amd64", Target{GOOS: "linux", GOARCH: "amd64"}},
		{"windows", Target{GOOS: "windows", GOARCH: hostTarget().GOARCH}},
		{"linux/arm64+integration,dbg", Target{
			GOOS: "linux", GOARCH: "arm64",
			Tags: []string{"integration", "dbg"},
		}},
	}
	for _, c := range cases {
		got, err := ParseTarget(c.in)
		if err != nil {
			t.Fatalf("ParseTarget(%q): %v", c.in, err)
		}
		if got.GOOS != c.want.GOOS || got.GOARCH != c.want.GOARCH {
			t.Errorf("ParseTarget(%q) = %v, want %v", c.in, got, c.want)
		}
	}
	for _, bad := range []string{"linux/am d64", "li-nux/amd64", "linux/arm64+not a tag"} {
		if _, err := ParseTarget(bad); err == nil {
			t.Errorf("ParseTarget(%q) should fail", bad)
		}
	}
}

func TestConstraintExpression(t *testing.T) {
	got := Target{GOOS: "linux", GOARCH: "amd64", Tags: []string{"cgo"}}.Constraint()
	if got != "(linux && amd64 && cgo)" {
		t.Fatalf("constraint = %q", got)
	}
	if s := (Target{GOOS: "linux", GOARCH: "amd64"}).slug(); s != "linux_amd64" {
		t.Fatalf("slug = %q", s)
	}
}

func TestMatchImport(t *testing.T) {
	cases := []struct {
		pattern, path string
		want          bool
	}{
		{"example.com/x/**", "example.com/x", true},
		{"example.com/x/**", "example.com/x/a/b", true},
		{"example.com/x/**", "example.com/xy", false},
		{"example.com/*", "example.com/x", true},
		{"example.com/*", "example.com/x/y", false},
	}
	for _, c := range cases {
		if got := matchImport(c.pattern, c.path); got != c.want {
			t.Errorf("matchImport(%q, %q) = %v, want %v", c.pattern, c.path, got, c.want)
		}
	}
}

func TestFollowSetWants(t *testing.T) {
	f := &followSet{module: "demo", moduleAll: true, maxDepth: 8}
	if !f.wants("demo/internal/model", false) {
		t.Error("module rule should match a package in the module")
	}
	if f.wants("fmt", true) {
		t.Error("std must not be followed without -follow-std")
	}
	if (&followSet{module: "demo", moduleAll: true, maxDepth: 0}).wants("demo/x", false) {
		t.Error("depth 0 must disable following")
	}
	if !(&followSet{std: true, maxDepth: 8}).wants("fmt", true) {
		t.Error("std true allow std packages")
	}
	neg := &followSet{module: "demo", moduleAll: true, exclude: []string{"demo/internal/x/**"}, maxDepth: 8}
	if neg.wants("demo/internal/x/y", false) {
		t.Error("exclusion must win")
	}
}

func TestRewriteApply(t *testing.T) {
	exact := Rewrite{From: "a/b", To: "c/d"}
	if got, ok := exact.Apply("a/b"); !ok || got != "c/d" {
		t.Fatalf("exact rewrite = %q %v", got, ok)
	}
	if _, ok := exact.Apply("a/b/c"); ok {
		t.Fatal("exact rewrite must not match a child path")
	}
	prefix := Rewrite{From: "a/", To: "c/"}
	if got, ok := prefix.Apply("a/b/c"); !ok || got != "c/b/c" {
		t.Fatalf("prefix rewrite = %q %v", got, ok)
	}
	rules, err := parseRewrites([]string{"short/=x/", "longer/path/=y/"})
	if err != nil {
		t.Fatal(err)
	}
	if rules[0].From != "longer/path/" {
		t.Fatalf("longest rule must come first, got %q", rules[0].From)
	}
}

func TestOrConstraints(t *testing.T) {
	if got := orConstraints(nil); got != "" {
		t.Fatalf("empty = %q", got)
	}
	if got := orConstraints([]string{"linux", "linux"}); got != "linux" {
		t.Fatalf("dedupe = %q", got)
	}
	got := orConstraints([]string{"linux", "windows", "darwin"})
	if got != "darwin || linux || windows" {
		t.Fatalf("sorted or = %q", got)
	}
	got = orConstraints([]string{"linux && amd64", "windows"})
	if got != "(linux && amd64) || windows" {
		t.Fatalf("parens = %q", got)
	}
}

func TestFileConstraint(t *testing.T) {
	cases := []struct{ name, src, want string }{
		{"model.go", "package p\n", ""},
		{"x_linux.go", "package p\n", "linux"},
		{"x_windows_amd64.go", "package p\n", "windows && amd64"},
		{"x.go", "//go:build cgo && linux\n\npackage p\n", "cgo && linux"},
		// the build line already states the file name tag: no duplication
		{"x_linux.go", "//go:build linux\n\npackage p\n", "linux"},
		{"x_linux.go", "//go:build cgo || purego\n\npackage p\n", "(cgo || purego) && linux"},
	}
	for _, c := range cases {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, c.name, c.src, parser.ParseComments)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got := fileConstraint(fset, f); got != c.want {
			t.Errorf("fileConstraint(%s) = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestConstBlockIsOneUnit(t *testing.T) {
	src := `package p

const (
	A Kind = iota + 1
	B
	C
)
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "p.go", src, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	gd, ok := f.Decls[0].(*ast.GenDecl)
	if !ok {
		t.Fatal("want a GenDecl")
	}
	if !constWhole(gd) {
		t.Fatal("iota block must be copied whole")
	}
	if name := constBlockName(gd); name != "A" {
		t.Fatalf("constBlockName = %q, want A", name)
	}
}

func TestImportNameHint(t *testing.T) {
	if got := importNameHint("example.com/a/v2"); got != "a" {
		t.Fatalf("importNameHint(v2) = %q, want a", got)
	}
	if got := importNameHint("example.com/foo"); got != "foo" {
		t.Fatalf("importNameHint(foo) = %q", got)
	}
	if got := pathBase("example.com/a/v2"); got != "v2" {
		t.Fatalf("pathBase still last segment = %q", got)
	}
}

// ============================== pipeline contract ==============================

type fakeLoader struct{ calls *[]string }

func (f fakeLoader) Load(ctx context.Context, cfg *Config) ([]*Program, error) {
	*f.calls = append(*f.calls, "load")
	return []*Program{{Target: Target{GOOS: "test", GOARCH: "test"}}}, nil
}

type fakeResolver struct{ calls *[]string }

func (f fakeResolver) Resolve(ctx context.Context, p *Program, cfg *Config) (*Selection, error) {
	*f.calls = append(*f.calls, "resolve")
	return &Selection{Prog: p, Target: p.Target, PkgName: "fake"}, nil
}

type fakeEmitter struct{ calls *[]string }

func (f fakeEmitter) Emit(ctx context.Context, s []*Selection, cfg *Config) ([]File, error) {
	*f.calls = append(*f.calls, "emit")
	return []File{{Name: "fake_ext.go", PkgName: "fake", Content: []byte("package fake\n")}}, nil
}

type fakeVerifier struct {
	calls *[]string
	ok    bool
}

func (f fakeVerifier) Verify(ctx context.Context, files []File, cfg *Config) ([]VerifyResult, error) {
	*f.calls = append(*f.calls, "verify")
	return []VerifyResult{{Target: hostTarget(), Command: "build", OK: f.ok}}, nil
}

type fakeWriter struct{ calls *[]string }

func (f fakeWriter) Write(ctx context.Context, files []File, cfg *Config) error {
	*f.calls = append(*f.calls, "write")
	return nil
}

func TestPipelineStageOrder(t *testing.T) {
	var calls []string
	cfg := DefaultConfig()
	cfg.Pattern, cfg.Names = "./x", []string{"Y"}
	cfg.Verify.Enabled = true
	p := New(cfg,
		WithLoader(fakeLoader{&calls}),
		WithResolver(fakeResolver{&calls}),
		WithEmitter(fakeEmitter{&calls}),
		WithVerifier(fakeVerifier{&calls, true}),
		WithWriter(fakeWriter{&calls}),
	)
	if _, err := p.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	want := "load,resolve,emit,verify,write"
	if got := strings.Join(calls, ","); got != want {
		t.Fatalf("stage order = %q, want %q", got, want)
	}
}

func TestPipelineStopsAfterStage(t *testing.T) {
	var calls []string
	cfg := DefaultConfig()
	cfg.Pattern, cfg.Names = "./x", []string{"Y"}
	p := New(cfg,
		WithLoader(fakeLoader{&calls}),
		WithResolver(fakeResolver{&calls}),
		WithEmitter(fakeEmitter{&calls}),
		WithWriter(fakeWriter{&calls}),
	)
	p.StopAfter = "resolve"
	st, err := p.Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := strings.Join(calls, ","); got != "load,resolve" {
		t.Fatalf("stage order = %q", got)
	}
	if len(st.Files) != 0 {
		t.Fatal("emit must not run")
	}
}

func TestPipelineMiddleware(t *testing.T) {
	var calls []string
	cfg := DefaultConfig()
	cfg.Pattern, cfg.Names = "./x", []string{"Y"}
	p := New(cfg,
		WithLoader(fakeLoader{&calls}),
		WithResolver(fakeResolver{&calls}),
		WithEmitter(fakeEmitter{&calls}),
		WithWriter(fakeWriter{&calls}),
	)
	var wrapped []string
	p.Use(func(next StageFunc) StageFunc {
		return func(ctx context.Context, st *State) error {
			wrapped = append(wrapped, "mw")
			return next(ctx, st)
		}
	})
	if _, err := p.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(wrapped) != 4 {
		t.Fatalf("middleware ran %d times, want 4", len(wrapped))
	}
}

func TestPipelineVerifyFailureIsReported(t *testing.T) {
	var calls []string
	cfg := DefaultConfig()
	cfg.Pattern, cfg.Names = "./x", []string{"Y"}
	cfg.Verify.Enabled = true
	p := New(cfg,
		WithLoader(fakeLoader{&calls}),
		WithResolver(fakeResolver{&calls}),
		WithEmitter(fakeEmitter{&calls}),
		WithVerifier(fakeVerifier{&calls, false}),
		WithWriter(fakeWriter{&calls}),
	)
	_, err := p.Run(context.Background())
	if err == nil {
		t.Fatal("expected a verification error")
	}
	if classify(nil, err) != exitVerify {
		t.Fatalf("exit code = %d, want %d", classify(nil, err), exitVerify)
	}
	for _, c := range calls {
		if c == "write" {
			t.Fatal("write must not run after a failed verification")
		}
	}
}
