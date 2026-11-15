package main

import (
	"errors"
	"go/ast"
	"go/build/constraint"
	"go/parser"
	"go/token"
	"slices"
	"testing"
)

// ----------------------------------------------------------------
// Target.Env / ParseTargets 去重 / 等价性
// ----------------------------------------------------------------

// TestTargetEnv 验证 Env 的顺序与内容：GOOS、GOARCH、GOFLAGS 依次追加，
// 空字段不产生条目。
func TestTargetEnv(t *testing.T) {
	cases := []struct {
		name string
		t    Target
		want []string
	}{
		{"empty", Target{}, nil},
		{"goos only", Target{GOOS: "linux"}, []string{"GOOS=linux"}},
		{
			"full",
			Target{GOOS: "linux", GOARCH: "arm64", Tags: []string{"a", "b"}},
			[]string{"GOOS=linux", "GOARCH=arm64", "GOFLAGS=-tags=a,b"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := c.t.Env()
			if !slices.Equal(got, c.want) {
				t.Fatalf("Env() = %v, want %v", got, c.want)
			}
		})
	}
}

func TestParseTargetsDedup(t *testing.T) {
	t.Run("exact dup", func(t *testing.T) {
		ts, err := ParseTargets([]string{"linux/amd64", "linux/amd64"})
		if err != nil {
			t.Fatal(err)
		}
		if len(ts) != 1 {
			t.Fatalf("got %d entries, want 1", len(ts))
		}
	})
	t.Run("tag set semantic", func(t *testing.T) {
		ts, err := ParseTargets([]string{"linux/amd64+x,y", "linux/amd64+y,x"})
		if err != nil {
			t.Fatal(err)
		}
		// Tags are sorted before Target.String(), so tag order does not affect
		// target identity.
		if len(ts) != 1 {
			t.Fatalf("got %d entries, want 1 (tag order must not affect identity)", len(ts))
		}
	})
}

// ----------------------------------------------------------------
// Config.Normalize：幂等、重试、变更后重新校验
// ----------------------------------------------------------------

// TestConfigNormalizeRetry：失败的 Normalize 不能把 normalized 置为 true，
// 否则修复配置后永远无法通过。
func TestConfigNormalizeRetry(t *testing.T) {
	cfg := DefaultConfig()
	if err := cfg.Normalize(); err == nil {
		t.Fatal("expected failure on empty Names/Pattern")
	}
	cfg.Pattern, cfg.Names = "./x", []string{"Y"}
	if err := cfg.Normalize(); err != nil {
		t.Fatalf("retry should succeed, got %v", err)
	}
}

// ----------------------------------------------------------------
// parseRewrites / Rewrite.Apply 边界
// ----------------------------------------------------------------
func TestParseRewritesStable(t *testing.T) {
	rules, err := parseRewrites([]string{"a/=x/", "bb/=y/", "cc/=z/"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(rules, []Rewrite{{"bb/", "y/"}, {"cc/", "z/"}, {"a/", "x/"}}) {
		t.Fatalf("want longest first then stable order, got %v", rules)
	}
}

func TestRewriteApplyEdge(t *testing.T) {
	cases := []struct {
		name     string
		r        Rewrite
		in       string
		wantPath string
		wantOK   bool
	}{
		{"exact hit", Rewrite{"a/b", "c/d"}, "a/b", "c/d", true},
		{"exact miss-child", Rewrite{"a/b", "c/d"}, "a/b/c", "a/b/c", false},
		{"prefix hit", Rewrite{"a/", "x/"}, "a/b/c", "x/b/c", true},
		{"prefix miss", Rewrite{"a/", "x/"}, "a", "a", false},
		{"empty from", Rewrite{"", "z"}, "p", "p", false}, // From 空串不应匹配任何路径
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, ok := c.r.Apply(c.in)
			if p != c.wantPath || ok != c.wantOK {
				t.Fatalf("Apply(%q) = (%q,%v), want (%q,%v)", c.in, p, ok, c.wantPath, c.wantOK)
			}
		})
	}
}

// ----------------------------------------------------------------
// followSet.wants 的规则交互
// ----------------------------------------------------------------

func TestFollowSetInteractions(t *testing.T) {
	t.Run("exclude wins over include", func(t *testing.T) {
		f := &followSet{
			include:  []string{"example.com/**"},
			exclude:  []string{"example.com/internal/**"},
			maxDepth: 8,
		}
		if f.wants("example.com/internal/x", false) {
			t.Fatal("exclude must win")
		}
		if !f.wants("example.com/x", false) {
			t.Fatal("include must still apply elsewhere")
		}
	})
	t.Run("depth zero disables", func(t *testing.T) {
		f := &followSet{all: true, maxDepth: 0}
		if f.wants("anything", false) {
			t.Fatal("maxDepth 0 must disable following")
		}
	})
	t.Run("stdOK gates std", func(t *testing.T) {
		f := &followSet{std: true, maxDepth: 8}
		if !f.wants("fmt", true) {
			t.Fatal("std true allow std packages")
		}
	})
	t.Run("empty means nothing", func(t *testing.T) {
		f := &followSet{maxDepth: 8}
		if !f.empty() {
			t.Fatal("empty set must report empty")
		}
		if f.wants("example.com/x", false) {
			t.Fatal("empty set must not want anything")
		}
	})
}

// ----------------------------------------------------------------
// classify 退出码
// ----------------------------------------------------------------

func TestClassifyExitCodes(t *testing.T) {
	genErr := errors.New("boom")
	cases := []struct {
		name string
		st   *State
		err  error
		want int
	}{
		{"nil", nil, nil, exitOK},
		{"missing", nil, &MissingError{Names: []string{"X"}}, exitMissing},
		{"verify type", nil, &VerifyError{}, exitVerify},
		{"generic no stage", nil, genErr, exitLoad},
		{"generic load", &State{Stages: []StageTiming{{Name: "load"}}}, genErr, exitLoad},
		{"generic resolve", &State{Stages: []StageTiming{{Name: "resolve"}}}, genErr, exitLoad},
		{"generic emit", &State{Stages: []StageTiming{{Name: "emit"}}}, genErr, exitOutput},
		{"generic write", &State{Stages: []StageTiming{{Name: "write"}}}, genErr, exitOutput},
		{"generic verify stage", &State{Stages: []StageTiming{{Name: "verify"}}}, genErr, exitVerify},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := classify(c.st, c.err); got != c.want {
				t.Fatalf("classify = %d, want %d", got, c.want)
			}
		})
	}
}

// ----------------------------------------------------------------
// sanitizeFileName / mergeEnv / isStdPath
// ----------------------------------------------------------------

func TestSanitizeFileName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"pkg_ext_linux_amd64.go", "pkg_ext_linux_amd64.go"},
		{"a b/c.go", "a_b_c.go"},
		{"中文.go", "__.go"},
		{"a+b=c.go", "a_b_c.go"},
	}
	for _, c := range cases {
		if got := sanitizeFileName(c.in); got != c.want {
			t.Errorf("sanitizeFileName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestMergeEnv 记录 mergeEnv 的当前行为：overrides 中的键会从 base 中删除，
// base 内部重复键不合并（Go 的 os.Environ 一般不会有重复，但仍是观察点）。
func TestMergeEnv(t *testing.T) {
	base := []string{"A=1", "B=2", "A=3"}
	got := mergeEnv(base, []string{"B=4"})
	want := []string{"A=1", "A=3", "B=4"}
	if !slices.Equal(got, want) {
		t.Fatalf("mergeEnv = %v, want %v (base dup kept, override wins)", got, want)
	}
}

func TestIsStdPath(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"fmt", true},
		{"fmt/x", true},
		{"internal/x", true},
		{"example.com/x", false},
		{"golang.org/x", false},
		{"", true}, // 观察：空路径被当作 std
	}
	for _, c := range cases {
		if got := isStdPath(c.in); got != c.want {
			t.Errorf("isStdPath(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// ----------------------------------------------------------------
// filenameTags 边界
// ----------------------------------------------------------------

func TestFilenameTagsEdge(t *testing.T) {
	cases := []struct {
		base string
		want []string
	}{
		{"foo.go", nil},
		{"foo_test.go", nil},
		{"foo_linux.go", []string{"linux"}},
		{"foo_amd64.go", []string{"amd64"}},
		{"foo_linux_amd64.go", []string{"linux", "amd64"}},
		{"foo_test_linux.go", []string{"linux"}}, // _test 不在末尾
		{"foo_unix.go", nil},                     // unix 不是具体 GOOS
		{"foo_cgo.go", nil},                      // cgo 不是 GOOS/GOARCH
	}
	for _, c := range cases {
		got := filenameTags(c.base)
		if !slices.Equal(got, c.want) {
			t.Errorf("filenameTags(%q) = %v, want %v", c.base, got, c.want)
		}
	}
}

// ----------------------------------------------------------------
// constraintRequires / orConstraints / andConstraints
// ----------------------------------------------------------------

func parseConstraint(t *testing.T, s string) constraint.Expr {
	t.Helper()
	e, err := constraint.Parse("//go:build " + s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return e
}

func TestConstraintRequiresEdge(t *testing.T) {
	cases := []struct {
		name string
		expr string
		tag  string
		want bool
	}{
		{"tag match", "linux", "linux", true},
		{"tag mismatch", "linux", "windows", false},
		{"and", "linux && amd64", "linux", true},
		{"and other", "linux && amd64", "arm64", false},
		{"or both", "linux || linux", "linux", true},
		{"or one", "linux || windows", "windows", false},
		{"not", "!linux", "linux", false},
		{"complex neg", "!(linux && amd64)", "linux", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := constraintRequires(parseConstraint(t, c.expr), c.tag); got != c.want {
				t.Fatalf("constraintRequires(%q,%q) = %v, want %v", c.expr, c.tag, got, c.want)
			}
		})
	}
}

func TestOrAndConstraintsEdge(t *testing.T) {
	if got := orConstraints(nil); got != "" {
		t.Fatalf("empty or = %q", got)
	}
	if got := andConstraints(nil); got != "" {
		t.Fatalf("empty and = %q", got)
	}
	// 空约束表示“所有平台”，是 OR 的单位元；OR 遇到空串必须返回空。
	if got := orConstraints([]string{"", " ", "linux"}); got != "" {
		t.Fatalf("or with blank = %q, want universal empty constraint", got)
	}
	// OR 使用 ||，括号由 parenExpr 补齐
	if got := orConstraints([]string{"a && b", "c"}); got != "(a && b) || c" {
		t.Fatalf("or parens = %q", got)
	}
	// AND 使用 &&，同样会加括号
	if got := andConstraints([]string{"a || b", "c"}); got != "(a || b) && c" {
		t.Fatalf("and parens = %q", got)
	}
}

// ----------------------------------------------------------------
// constWhole / constBlockName 边界
// ----------------------------------------------------------------

func TestConstWholeEdge(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want bool
	}{
		{"plain", "package p\nconst A = 1\n", false},
		{"iota single", "package p\nconst A = iota\n", true},
		{"iota block", "package p\nconst (\nA = iota\nB\n)\n", true},
		{"explicit block", "package p\nconst (\nA = 1\nB = 2\n)\n", false},
		{"mixed block", "package p\nconst (\nA Kind = iota + 1\nB\nC = \"x\"\nD\n)\n", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, "p.go", c.src, parser.ParseComments)
			if err != nil {
				t.Fatal(err)
			}
			gd, ok := f.Decls[0].(*ast.GenDecl)
			if !ok {
				t.Fatalf("want GenDecl, got %T", f.Decls[0])
			}
			if got := constWhole(gd); got != c.want {
				t.Fatalf("constWhole = %v, want %v", got, c.want)
			}
		})
	}
}
