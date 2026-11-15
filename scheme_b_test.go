package main

import (
	"context"
	"go/build"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// TestVerifyMainPackageWithoutMain covers the package main case: the generated
// subset usually has no func main, so `go build` would fail even though the
// extracted declarations type-check.
func TestVerifyMainPackageWithoutMain(t *testing.T) {
	dir := writeModule(t, map[string]string{
		"main.go": "package main\n\nfunc Exported() int { return 1 }\n",
	})
	extract(t, dir, func(cfg *Config) {
		cfg.Names = []string{"Exported"}
		cfg.Verify.Enabled = true
		cfg.Verify.AllTargets = false
	})
}

// TestVerifyRequiredTargetDoesNotSkip protects File.Targets: a target that the
// emitter claims to cover must not be reported as a successful skip when no
// generated file applies to it.
func TestVerifyRequiredTargetDoesNotSkip(t *testing.T) {
	dir := writeModule(t, map[string]string{
		"p.go": "//go:build windows\n\npackage p\n\nfunc F() {}\n",
	})
	cfg := DefaultConfig()
	cfg.Verify.Command = "build"
	cfg.Verify.Timeout = 30 * time.Second
	target := Target{GOOS: "linux", GOARCH: "amd64"}

	required := runGo(context.Background(), dir, ".", target, cfg, "p", true)
	if required.OK || required.Skipped || required.Err == nil {
		t.Fatalf("required missing target must fail, got %+v", required)
	}
	optional := runGo(context.Background(), dir, ".", target, cfg, "p", false)
	if !optional.OK || !optional.Skipped {
		t.Fatalf("optional missing target should skip, got %+v", optional)
	}
}

// TestMatchFileForRespectsCgo covers -targets auto: a cgo file must follow the
// host toolchain's cgo setting instead of being forced invisible.
func TestMatchFileForRespectsCgo(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "p.go")
	if err := os.WriteFile(path, []byte("//go:build cgo\n\npackage p\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := matchFileFor(Target{GOOS: runtime.GOOS, GOARCH: runtime.GOARCH}, path)
	if got != build.Default.CgoEnabled {
		t.Fatalf("matchFileFor(cgo) = %v, want host CgoEnabled %v", got, build.Default.CgoEnabled)
	}
}
