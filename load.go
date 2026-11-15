package main

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"

	"golang.org/x/tools/go/packages"
)

// ============================== model ==============================

// Package wraps one loaded package and (lazily) its declaration index.
type Package struct {
	Path  string
	Name  string
	Std   bool
	Raw   *packages.Package
	Files []*ast.File
	Idx   *Index
	Prog  *Program
}

// Program is one loaded package graph, i.e. the world as seen from one build
// target. Positions from different targets are not comparable, which is why
// every target gets its own Program (and its own token.FileSet).
type Program struct {
	Target     Target
	Fset       *token.FileSet
	Root       *Package
	All        map[string]*packages.Package
	ByPath     map[string]*Package
	ModulePath string
	ModuleDir  string
	GoVersion  string
	SrcCache   map[string][]byte
	Warnings   []string
}

func (p *Program) src(pos token.Pos) []byte {
	f := p.Fset.File(pos)
	if f == nil {
		return nil
	}
	name := f.Name()
	if b, ok := p.SrcCache[name]; ok {
		return b
	}
	b, err := os.ReadFile(name)
	if err != nil {
		p.Warnings = append(p.Warnings, fmt.Sprintf("cannot read %s: %v", name, err))
		b = nil
	}
	p.SrcCache[name] = b
	return b
}

// packageFor returns the wrapper for an import path, indexing it on demand.
func (p *Program) packageFor(importPath string) *Package {
	if pkg, ok := p.ByPath[importPath]; ok {
		return pkg
	}
	raw, ok := p.All[importPath]
	// TypesInfo is needed by buildIndex; a package loaded from
	// export data only has Types but no Defs/Uses. Treat it as unavailable
	// instead of panicking later in scan().
	if !ok || raw == nil || raw.Types == nil || raw.TypesInfo == nil || len(raw.Syntax) == 0 {
		return nil
	}
	pkg := &Package{
		Path:  importPath,
		Name:  raw.Name,
		Std:   raw.Module == nil && isStdPath(importPath),
		Raw:   raw,
		Prog:  p,
		Files: convertFiles(raw.Syntax),
	}
	pkg.Idx = buildIndex(pkg, raw.TypesInfo)
	p.ByPath[importPath] = pkg
	return pkg
}

func (p *Program) isStd(importPath string) bool {
	if raw, ok := p.All[importPath]; ok && raw != nil {
		return raw.Module == nil && isStdPath(importPath)
	}
	return isStdPath(importPath)
}

// isStdPath is the standard library heuristic used by the go command: a first
// path element without a dot cannot be a module path.
func isStdPath(importPath string) bool {
	first, _, _ := strings.Cut(importPath, "/")
	return !strings.Contains(first, ".")
}

// ============================== loader ==============================

// packagesLoader loads one package graph per target.
//
// Targets are completely independent: every load has its own token.FileSet,
// its own type information and its own "go list" invocation, and nothing is
// shared between them. That makes the load stage embarrassingly parallel, so a
// five target matrix costs roughly one load instead of five. Results are still
// returned in the -targets order, so output stays deterministic.
type packagesLoader struct{}

func (packagesLoader) Load(ctx context.Context, cfg *Config) ([]*Program, error) {
	abs, err := filepath.Abs(cfg.Dir)
	if err != nil {
		return nil, err
	}
	if info, err := os.Stat(abs); err != nil || !info.IsDir() {
		return nil, fmt.Errorf("not a directory: %s", abs)
	}
	if len(cfg.Targets) == 0 {
		return nil, errors.New("no targets to load")
	}
	progs, err := loadTargets(ctx, cfg, abs)
	if err != nil {
		return nil, err
	}
	return progs, nil
}

// loadTargets loads every target, at most cfg.loadWorkers() at a time, and
// reports the failure of the lowest numbered target it could not load. A
// failure cancels the remaining loads; their cancellation errors are not
// reported as the cause.
func loadTargets(ctx context.Context, cfg *Config, dir string) ([]*Program, error) {
	targets := cfg.Targets
	progs := make([]*Program, len(targets))
	errs := make([]error, len(targets))

	// A failure makes every other load pointless: cancel them instead of
	// paying for the rest of the matrix.
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()

	queue := make(chan int)
	var wg sync.WaitGroup
	for i := 0; i < cfg.loadWorkers(len(targets)); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for idx := range queue {
				if cctx.Err() != nil {
					errs[idx] = cctx.Err()
					continue
				}
				progs[idx], errs[idx] = loadOne(cctx, cfg, dir, targets[idx])
				if errs[idx] != nil {
					cancel()
				}
			}
		}()
	}
	for i := range targets {
		queue <- i
	}
	close(queue)
	wg.Wait()

	var cancelled error
	for _, err := range errs {
		switch {
		case err == nil:
		case errors.Is(err, context.Canceled) && ctx.Err() == nil:
			// A sibling target failed: keep it as a fallback only.
			if cancelled == nil {
				cancelled = err
			}
		default:
			return nil, err
		}
	}
	if cancelled != nil {
		return nil, cancelled
	}
	return progs, nil
}

func loadOne(ctx context.Context, cfg *Config, dir string, t Target) (*Program, error) {
	env := mergeEnv(os.Environ(), t.Env())
	pcfg := &packages.Config{
		Context: ctx,
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedSyntax |
			packages.NeedTypes | packages.NeedTypesInfo | packages.NeedImports |
			packages.NeedModule | packages.NeedDeps,
		Dir:   dir,
		Env:   env,
		Tests: false,
	}
	pkgs, err := packages.Load(pcfg, cfg.Pattern)
	if err != nil {
		return nil, fmt.Errorf("load %s (%s): %w", cfg.Pattern, t, err)
	}
	if len(pkgs) == 0 {
		return nil, fmt.Errorf("pattern %q matched no package (%s)", cfg.Pattern, t)
	}
	if len(pkgs) > 1 {
		var names []string
		for _, p := range pkgs {
			names = append(names, p.PkgPath)
		}
		sort.Strings(names)
		return nil, fmt.Errorf("pattern %q matched %d packages (%s); pick one",
			cfg.Pattern, len(pkgs), strings.Join(names, ", "))
	}
	root := pkgs[0]

	// Type errors in the target package make the dependency graph unreliable.
	var errs []string
	for _, e := range root.Errors {
		errs = append(errs, e.Error())
	}
	if len(errs) > 0 {
		return nil, fmt.Errorf("loading %s (%s):\n\t%s", root.PkgPath, t, strings.Join(errs, "\n\t"))
	}

	prog := &Program{
		Target:   t,
		Fset:     root.Fset,
		All:      map[string]*packages.Package{},
		ByPath:   map[string]*Package{},
		SrcCache: map[string][]byte{},
	}
	collect(root, prog.All)

	rootPkg := &Package{
		Path:  root.PkgPath,
		Name:  root.Name,
		Std:   root.Module == nil && isStdPath(root.PkgPath),
		Raw:   root,
		Files: convertFiles(root.Syntax),
		Prog:  prog,
	}
	rootPkg.Idx = buildIndex(rootPkg, root.TypesInfo)
	prog.Root = rootPkg
	prog.ByPath[root.PkgPath] = rootPkg

	prog.ModulePath, prog.ModuleDir, prog.GoVersion = moduleInfo(root)

	// a dependency that -follow will inline must be free of
	// type errors. Warning and continuing produces copies with incomplete
	// types, and the failure only shows up much later in -verify (or never,
	// when verification is disabled).
	follow, ferr := cfg.compileFollow(prog.ModulePath)
	if ferr != nil {
		return nil, ferr
	}
	var followedErrs []string
	for path, dep := range prog.All {
		if dep == nil || path == root.PkgPath {
			continue
		}
		if len(dep.Errors) == 0 {
			continue
		}
		if follow != nil && follow.wants(path, prog.isStd(path)) {
			for _, e := range dep.Errors {
				followedErrs = append(followedErrs, fmt.Sprintf("%s: %v", path, e))
			}
			continue
		}
		for _, e := range dep.Errors {
			prog.Warnings = append(prog.Warnings,
				fmt.Sprintf("import %s does not type check cleanly: %v", path, e))
		}
	}
	if len(followedErrs) > 0 {
		sort.Strings(followedErrs)
		return nil, fmt.Errorf("followed dependencies do not type check (%s):\n\t%s",
			t, strings.Join(followedErrs, "\n\t"))
	}
	return prog, nil
}

func collect(p *packages.Package, into map[string]*packages.Package) {
	if p == nil {
		return
	}
	if _, ok := into[p.PkgPath]; ok {
		return
	}
	into[p.PkgPath] = p
	for _, dep := range p.Imports {
		collect(dep, into)
	}
}

func mergeEnv(base, overrides []string) []string {
	// Target.Env sets GOFLAGS=-tags=... . Replacing the whole
	// GOFLAGS value would drop -mod=vendor, -trimpath and similar user flags.
	// Preserve every non-tags flag and replace only the tags value.
	baseFlags := ""
	for _, kv := range base {
		if k, v, ok := strings.Cut(kv, "="); ok && k == "GOFLAGS" {
			baseFlags = v
		}
	}
	merged := make([]string, len(overrides))
	copy(merged, overrides)
	for i, kv := range merged {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k != "GOFLAGS" || baseFlags == "" {
			continue
		}
		merged[i] = "GOFLAGS=" + mergeGoFlags(baseFlags, v)
	}

	drop := map[string]bool{}
	for _, kv := range merged {
		k, _, _ := strings.Cut(kv, "=")
		drop[k] = true
	}
	out := make([]string, 0, len(base)+len(merged))
	for _, kv := range base {
		k, _, _ := strings.Cut(kv, "=")
		if drop[k] {
			continue
		}
		out = append(out, kv)
	}
	return append(out, merged...)
}

// mergeGoFlags removes an existing -tags flag from base and appends add.
// GOFLAGS is a whitespace separated list; the project only emits simple
// -tags=a,b values, so fields are sufficient here.
func mergeGoFlags(base, add string) string {
	fields := strings.Fields(base)
	out := make([]string, 0, len(fields)+1)
	for i := 0; i < len(fields); i++ {
		f := fields[i]
		if f == "-tags" {
			i++ // skip its value as well
			continue
		}
		if strings.HasPrefix(f, "-tags=") {
			continue
		}
		out = append(out, f)
	}
	if add = strings.TrimSpace(add); add != "" {
		out = append(out, add)
	}
	return strings.Join(out, " ")
}

var (
	reModule = regexp.MustCompile(`(?m)^\s*module\s+(\S+)\s*$`)
	reGoVer  = regexp.MustCompile(`(?m)^\s*go\s+(\d+\.\d+(?:\.\d+)?)\s*$`)
)

// moduleInfo finds the go.mod governing a package. It is needed by the
// verifier, which builds the generated files inside that module.
func moduleInfo(p *packages.Package) (modPath, modDir, goVer string) {
	if p.Module != nil {
		modPath, modDir = p.Module.Path, p.Module.Dir
	}
	if modDir == "" && len(p.GoFiles) > 0 {
		modDir = findModuleRoot(filepath.Dir(p.GoFiles[0]))
	}
	if modDir != "" {
		if b, err := os.ReadFile(filepath.Join(modDir, "go.mod")); err == nil {
			text := string(b)
			if m := reModule.FindStringSubmatch(text); m != nil && modPath == "" {
				modPath = m[1]
			}
			if m := reGoVer.FindStringSubmatch(text); m != nil {
				goVer = m[1]
			}
		}
	}
	return modPath, modDir, goVer
}

func findModuleRoot(dir string) string {
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}
