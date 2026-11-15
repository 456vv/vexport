package main

import (
	"context"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"os/exec"
	"strings"
	"testing"
)

// ============================== caller rewrite ==============================

// typeCheck is a helper for tests that need real go/types objects.
func typeCheck(t *testing.T, src string) (*types.Package, types.Info) {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "p.go", src, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	info := types.Info{
		Defs:  map[*ast.Ident]types.Object{},
		Uses:  map[*ast.Ident]types.Object{},
		Types: map[ast.Expr]types.TypeAndValue{},
	}
	conf := types.Config{Importer: importer.Default()}
	pkg, err := conf.Check("p", fset, []*ast.File{f}, &info)
	if err != nil {
		t.Fatalf("type check: %v", err)
	}
	return pkg, info
}

func embeddedFields(t *testing.T, pkg *types.Package, name string) map[string]*types.Var {
	t.Helper()
	obj := pkg.Scope().Lookup(name)
	if obj == nil {
		t.Fatalf("type %s not found", name)
	}
	st, ok := obj.Type().Underlying().(*types.Struct)
	if !ok {
		t.Fatalf("%s is not a struct", name)
	}
	out := map[string]*types.Var{}
	for i := 0; i < st.NumFields(); i++ {
		f := st.Field(i)
		if f.Embedded() {
			out[f.Name()] = f
		}
	}
	return out
}

// TestEmbeddedFieldRename pins the rule that makes a rename reach the call
// sites: an embedded field is named after its type, so renaming the type
// renames the field object too. An embedded alias keeps its own name.
func TestEmbeddedFieldRename(t *testing.T) {
	pkg, _ := typeCheck(t, `package p

type U struct{ Name string }

type H struct{ U }

type Alias = U

type HA struct{ Alias }
`)
	renames := map[types.Object]string{pkg.Scope().Lookup("U"): "ModelU"}
	plain := map[types.Object]string{}

	field := embeddedFields(t, pkg, "H")["U"]
	if field == nil {
		t.Fatal("H must embed U")
	}
	if got, ok := embeddedFieldRename(field, renames); !ok || got != "ModelU" {
		t.Fatalf("embedded U = %q, %v; want ModelU, true", got, ok)
	}
	if _, ok := embeddedFieldRename(field, plain); ok {
		t.Fatal("no rename without a rule")
	}

	// An embedded alias is named after the alias, so it must not move.
	aliasField := embeddedFields(t, pkg, "HA")["Alias"]
	if got, ok := embeddedFieldRename(aliasField, renames); ok {
		t.Fatalf("embedded alias must stay put, got %q", got)
	}

	// Non embedded fields and plain objects are never rewritten.
	if _, ok := embeddedFieldRename(pkg.Scope().Lookup("U"), renames); ok {
		t.Fatal("a type object is not an embedded field")
	}
	if _, ok := callerRename(nil, renames); ok {
		t.Fatal("nil object must not rename")
	}
	if got, ok := callerRename(pkg.Scope().Lookup("U"), renames); !ok || got != "ModelU" {
		t.Fatalf("direct rename = %q, %v", got, ok)
	}
}

// TestFollowRewritesCallers runs the real pipeline over the demo fixture and
// checks that renaming an inlined type also rewrites the call sites that use
// its embedded field.
func TestFollowRewritesCallers(t *testing.T) {
	needGoToolchain(t)
	cfg := DefaultConfig()
	cfg.Dir, cfg.Pattern = "testdata/demo", "./account"
	cfg.Names = []string{"Holder", "NewHolder", "Holder.Label"}
	cfg.Follow = []string{"module"}
	cfg.Conflict = ConflictPrefix
	cfg.DryRun = true
	if err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}
	st, err := New(cfg).Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(st.Files) != 1 {
		t.Fatalf("files = %d, want 1", len(st.Files))
	}
	src := string(st.Files[0].Content)
	for _, want := range []string{
		"type Holder struct {\n\tModelUser\n", // the embedded field is renamed
		"type ModelUser struct {",             // the inlined type
		"h.ModelUser.Name",                    // promoted access
		"Holder{ModelUser: u",                 // struct literal key
		"func NewHolder(u ModelUser)",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("generated code misses %q:\n%s", want, src)
		}
	}
	// The old field name must be gone: it would not compile.
	for _, bad := range []string{"Holder{User:", "h.User."} {
		if strings.Contains(src, bad) {
			t.Errorf("generated code still contains %q:\n%s", bad, src)
		}
	}
	// The target package's own User is not selected, so it is not copied; the
	// clash that forced the rename is reported instead.
	if strings.Contains(src, "type User struct") {
		t.Errorf("the target package's own User must not be copied:\n%s", src)
	}
	if !containsName(st.Notes, "renamed model.User to ModelUser") {
		t.Errorf("missing rename note: %v", st.Notes)
	}
}

// TestConflictKeepKeepsWholePackage checks the other half of the caller
// rewrite: when a package cannot be inlined because of a name clash, nothing
// of it may be copied, and the references that used to be bare (a dot import,
// or a sibling declaration) have to be qualified again.
func TestConflictKeepKeepsWholePackage(t *testing.T) {
	needGoToolchain(t)
	cfg := DefaultConfig()
	cfg.Dir, cfg.Pattern = "testdata/demo", "./user"
	cfg.Names = []string{"Svc"}
	cfg.Follow = []string{"module"}
	cfg.DryRun = true
	if err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}
	st, err := New(cfg).Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(st.Files) != 1 {
		t.Fatalf("files = %d, want 1", len(st.Files))
	}
	src := string(st.Files[0].Content)
	for _, want := range []string{
		"map[model.ID]*model.User", // store's dot imports are qualified again
		"model.NewUser(",           // model stays an import
		"model.ErrNotFound",
		"func (m *Memory) Get(id model.ID) (*model.User, error)",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("generated code misses %q:\n%s", want, src)
		}
	}
	// Nothing of the clashing package may be copied, and local variables must
	// never be qualified as if they were package members.
	for _, bad := range []string{
		"func NewUser(", "type ModelUser ", "var ErrNotFound",
		"model.m", "model.id", "model.ok", "model.u",
	} {
		if strings.Contains(src, bad) {
			t.Errorf("generated code contains %q:\n%s", bad, src)
		}
	}
	found := false
	for _, n := range st.Notes {
		if strings.Contains(n, "demo/internal/model stays an import") {
			found = true
		}
		if strings.Contains(n, "it is unexported") {
			t.Errorf("unexpected unexported note: %s", n)
		}
	}
	if !found {
		t.Errorf("missing blocked package note: %v", st.Notes)
	}
}

// ============================== parallel load ==============================

func TestLoadWorkers(t *testing.T) {
	cases := []struct {
		jobs, targets, want int
	}{
		{0, 1, 1},
		{0, 3, 3},  // auto, bounded by the number of targets
		{1, 4, 1},  // forced sequential
		{8, 4, 4},  // never more workers than targets
		{-5, 4, 1}, // defensive clamp; Normalize rejects this earlier
	}
	for _, c := range cases {
		cfg := DefaultConfig()
		cfg.Jobs = c.jobs
		if got := cfg.loadWorkers(c.targets); got != c.want {
			t.Errorf("loadWorkers(jobs=%d, targets=%d) = %d, want %d",
				c.jobs, c.targets, got, c.want)
		}
	}
}

func TestNormalizeRejectsNegativeJobs(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Pattern, cfg.Names, cfg.Jobs = "./x", []string{"Y"}, -1
	if err := cfg.Normalize(); err == nil {
		t.Fatal("negative -jobs must be rejected")
	}
}

// TestParallelLoadKeepsTargetOrder checks that loading several targets at once
// still yields one program per target, in -targets order.
func TestParallelLoadKeepsTargetOrder(t *testing.T) {
	needGoToolchain(t)
	cfg := DefaultConfig()
	cfg.Dir, cfg.Pattern = "testdata/demo", "./internal/model"
	cfg.Names = []string{"Kind"}
	cfg.Jobs = 3
	targets := []string{"windows/amd64", "linux/amd64", "darwin/arm64"}
	ts, err := ParseTargets([]string{strings.Join(targets, ",")})
	if err != nil {
		t.Fatal(err)
	}
	cfg.Targets = ts
	if err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}
	progs, err := packagesLoader{}.Load(context.Background(), cfg)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(progs) != len(targets) {
		t.Fatalf("programs = %d, want %d", len(progs), len(targets))
	}
	for i, prog := range progs {
		if prog == nil || prog.Root == nil {
			t.Fatalf("program %d is empty", i)
		}
		if prog.Target.String() != targets[i] {
			t.Errorf("program %d targets %s, want %s", i, prog.Target, targets[i])
		}
		// Every target is its own world: positions must not be shared.
		if i > 0 && prog.Fset == progs[0].Fset {
			t.Errorf("program %d shares a FileSet with program 0", i)
		}
	}
}

// TestLoadStopsOnBadTarget checks that one broken target fails the stage and
// does not leave a half loaded matrix behind.
func TestLoadStopsOnBadTarget(t *testing.T) {
	needGoToolchain(t)
	cfg := DefaultConfig()
	cfg.Dir, cfg.Pattern = "testdata/demo", "./internal/model"
	cfg.Names = []string{"Kind"}
	cfg.Jobs = 2
	cfg.Targets = []Target{{GOOS: "nostuchos", GOARCH: "amd64"}, hostTarget()}
	if err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}
	if _, err := (packagesLoader{}).Load(context.Background(), cfg); err == nil {
		t.Fatal("expected the load stage to fail")
	}
}

// needGoToolchain skips tests that shell out to the Go toolchain.
func needGoToolchain(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("needs the go toolchain")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not in PATH")
	}
}
