package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// VerifyResult is the outcome of one self-verification build.
type VerifyResult struct {
	Target  Target
	Command string
	Dir     string
	OK      bool
	Skipped bool
	Output  string
	Err     error
	Elapsed time.Duration
}

// buildVerifier proves that the generated files compile, using the real Go
// toolchain and the real module, so that build tags, import paths and module
// requirements are all checked exactly as a consumer would see them.
//
// The generated files are written to a throwaway package directory inside the
// source module (so that go.mod/go.sum and the module cache apply), built once
// per target, and then removed. Nothing outside that temporary directory is
// touched.
type buildVerifier struct{}

func (buildVerifier) Verify(ctx context.Context, files []File, cfg *Config) ([]VerifyResult, error) {
	if len(files) == 0 {
		return nil, errors.New("nothing to verify")
	}
	modDir := files[0].ModuleDir
	if modDir == "" {
		return nil, errors.New("cannot verify: no go.mod found above the package " +
			"(run inside a module, or drop -verify)")
	}
	if fi, err := os.Stat(modDir); err != nil || !fi.IsDir() {
		return nil, fmt.Errorf("cannot verify: module directory %q is not accessible", modDir)
	}

	tmp, err := os.MkdirTemp(modDir, "extractverify-")
	if err != nil {
		return nil, fmt.Errorf("cannot verify: creating temp package in %s: %w", modDir, err)
	}
	keep := cfg.Verify.Keep
	defer func() {
		if !keep {
			os.RemoveAll(tmp)
		}
	}()

	for i, f := range files {
		name := fmt.Sprintf("zz_extract_%02d_%s", i, sanitizeFileName(f.Name))
		if err := os.WriteFile(filepath.Join(tmp, name), f.Content, 0o644); err != nil {
			return nil, fmt.Errorf("cannot verify: %w", err)
		}
	}
	// The build runs inside the temporary package directory, so the package
	// pattern is simply ".".
	pkgArg := "."
	// extracting from a package named main is normal, but
	// `go build` insists that the package defines func main. The generated
	// subset usually contains only selected declarations. Compile package main
	// with `go test -c` instead: it type-checks the package and builds a test
	// binary without requiring a user main function.
	pkgName := files[0].PkgName

	targets := verifyTargets(files, cfg)
	results := make([]VerifyResult, 0, len(targets))
	for _, job := range targets {
		res := runGo(ctx, tmp, pkgArg, job.Target, cfg, pkgName, job.Required)
		if cfg.Verify.Keep {
			res.Dir = tmp
		}
		results = append(results, res)
	}
	return results, nil
}

type verifyJob struct {
	Target   Target
	Required bool
}

// verifyTargets decides what to build: every distinct target of the emitted
// files, or just the host target when AllTargets is off.
func verifyTargets(files []File, cfg *Config) []verifyJob {
	if !cfg.Verify.AllTargets {
		host := hostTarget()
		return []verifyJob{{Target: host, Required: targetListed(files, host)}}
	}
	seen := map[string]bool{}
	var out []verifyJob
	for _, f := range files {
		for _, t := range f.Targets {
			if seen[t.String()] {
				continue
			}
			seen[t.String()] = true
			out = append(out, verifyJob{Target: t, Required: true})
		}
	}
	if len(out) == 0 {
		out = []verifyJob{{Target: hostTarget(), Required: false}}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Target.String() < out[j].Target.String() })
	return out
}

// targetListed reports whether any output file claims to cover t.
func targetListed(files []File, t Target) bool {
	for _, f := range files {
		for _, have := range f.Targets {
			if have.String() == t.String() {
				return true
			}
		}
	}
	return false
}

func runGo(ctx context.Context, dir, pkgArg string, t Target, cfg *Config, pkgName string, required bool) VerifyResult {
	res := VerifyResult{Target: t, Command: cfg.Verify.Command}
	bin, err := exec.LookPath("go")
	if err != nil {
		res.Err = errors.New("go toolchain not found in PATH")
		res.Output = res.Err.Error()
		return res
	}
	args := []string{cfg.Verify.Command}
	if cfg.Verify.Command == "build" && pkgName == "main" {
		// see buildVerifier.Verify. Keep Command as "build" in
		// the report: this is still the build verification the user asked for.
		args = []string{"test", "-c", "-vet=off", "-o", filepath.Join(dir, "zz_verify_bin"), pkgArg}
	} else {
		if cfg.Verify.Command == "vet" {
			args = append(args, "-copylocks=false")
		}
		args = append(args, pkgArg)
	}

	cctx, cancel := context.WithTimeout(ctx, cfg.Verify.Timeout)
	defer cancel()

	begin := time.Now()
	cmd := exec.CommandContext(cctx, bin, args...)
	cmd.Dir = dir
	cmd.Env = mergeEnv(os.Environ(), t.Env())
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err = cmd.Run()
	res.Elapsed = time.Since(begin)
	res.Output = strings.TrimSpace(buf.String())

	switch {
	case cctx.Err() == context.DeadlineExceeded:
		res.Err = fmt.Errorf("timed out after %s", cfg.Verify.Timeout)
		res.Output = strings.TrimSpace(res.Output + "\nverification timed out")
	case err != nil:
		res.Err = err
		if strings.Contains(res.Output, "build constraints exclude all Go files") {
			// only skip targets that were not promised by the
			// emitter. If File.Targets says this target is covered, an empty
			// build is a verification failure (typically a bad file name suffix
			// or an over-restrictive //go:build expression).
			if required {
				res.OK = false
				res.Skipped = false
				res.Err = fmt.Errorf("no generated file covers %s", t.String())
				res.Output = strings.TrimSpace(
					"no generated file covers " + t.String() + "\n" + res.Output)
				return res
			}
			res.Skipped = true
			res.OK = true
			res.Output = "skipped: no generated file applies to " + t.String()
			res.Err = nil
			return res
		}
	default:
		res.OK = true
	}
	return res
}

func sanitizeFileName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}
