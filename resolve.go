package main

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"sort"
	"strings"
)

// Selection is the result of the resolve stage for one target: the seed
// declarations, everything they recursively depend on (including declarations
// inlined from other packages) and the source edits needed to move them.
type Selection struct {
	Prog    *Program
	Target  Target
	PkgName string
	Decls   []*Decl

	owner       map[string]*Decl
	seen        map[string]*Decl
	renames     map[types.Object]string
	extDeps     map[string]bool
	follow      *followSet
	cfg         *Config
	noteSeen    map[string]bool
	conflictErr error

	// kept lists the packages a declaration was left imported from, with the
	// reason; blocked lists the packages the resolver must not inline at all.
	kept    map[string]string
	blocked map[string]bool

	Warnings []string
	Notes    []string
	Missing  []string
}

// typeResolver is the default Resolver.
type typeResolver struct{}

// Resolve selects the dependency closure of -names for one target.
//
// It may run more than once. Inlining is only sound for a package as a whole:
// once one declaration of a package is copied, its copies have new type
// identity, so the copied declarations can no longer mix with the ones that
// stayed behind an import. When -conflict keep decides to leave a declaration
// imported, the whole package it belongs to becomes an import, and the closure
// is computed again without it. Each round blocks at least one package, so
// this terminates.
func (typeResolver) Resolve(ctx context.Context, prog *Program, cfg *Config) (*Selection, error) {
	blocked := map[string]bool{}
	reasons := map[string]string{}
	for round := 0; ; round++ {
		s, err := resolveOnce(ctx, prog, cfg, blocked)
		if err != nil {
			return nil, err
		}
		grew := false
		for p, reason := range s.kept {
			if blocked[p] {
				continue
			}
			blocked[p] = true
			reasons[p] = reason
			grew = true
		}
		if !grew {
			s.blockedPackages(reasons)
			return s, nil
		}
		if round > len(prog.All) {
			return nil, errors.New("resolve: inlining did not converge")
		}
	}
}

// blockedPackages reports the packages that stay imported although -follow
// matched them.
func (s *Selection) blockedPackages(reasons map[string]string) {
	paths := make([]string, 0, len(reasons))
	for p := range reasons {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		s.Notes = append(s.Notes, fmt.Sprintf(
			"%s stays an import: %s, and a package cannot be inlined partially",
			p, reasons[p]))
	}
}

// resolveOnce is one pass over the closure; blocked packages are never
// inlined.
func resolveOnce(ctx context.Context, prog *Program, cfg *Config, blocked map[string]bool) (*Selection, error) {
	follow, err := cfg.compileFollow(prog.ModulePath)
	if err != nil {
		return nil, err
	}
	s := &Selection{
		Prog:     prog,
		Target:   prog.Target,
		PkgName:  prog.Root.Name,
		owner:    map[string]*Decl{},
		seen:     map[string]*Decl{},
		renames:  map[types.Object]string{},
		extDeps:  map[string]bool{},
		noteSeen: map[string]bool{},
		kept:     map[string]string{},
		blocked:  blocked,
		follow:   follow,
		cfg:      cfg,
	}
	if err := s.seeds(); err != nil {
		return nil, err
	}
	if err := s.closure(); err != nil {
		return nil, err
	}
	if s.conflictErr != nil {
		return nil, s.conflictErr
	}
	if s.follow != nil && !s.follow.empty() {
		s.Notes = append(s.Notes, fmt.Sprintf("followed imports recursively (max depth %s)",
			depthText(s.follow.maxDepth)))
	}
	if n := s.inlinedCount(); n > 0 {
		s.Notes = append(s.Notes, fmt.Sprintf(
			"%d declaration(s) inlined from %d other package(s); code outside this set that "+
				"refers to them must use the new package", n, s.inlinedPackages()))
	}
	return s, nil
}

func depthText(d int) string {
	if d < 0 {
		return "unlimited"
	}
	return itoa(d)
}

type pending struct {
	d    *Decl
	hops int
}

// seeds resolves -names into declarations of the target package.
func (s *Selection) seeds() error {
	ix := s.Prog.Root.Idx
	var seeds []*Decl
	addSeed := func(d *Decl) {
		for _, have := range seeds {
			if have == d {
				return
			}
		}
		seeds = append(seeds, d)
	}
	for _, raw := range s.cfg.Names {
		for _, name := range splitList(raw) {
			if name == "*" {
				for _, d := range ix.exported() {
					addSeed(d)
				}
				continue
			}
			d, ok := ix.lookup(name)
			if !ok {
				if !containsName(s.Missing, name) {
					s.Missing = append(s.Missing, name)
				}
				continue
			}
			addSeed(d)
			// A selected method is useless without its receiver type.
			if d.Kind == "method" {
				if t, ok := ix.types[d.receiver()]; ok {
					addSeed(t)
				}
			}
		}
	}
	for _, d := range seeds {
		// use the same admission path as recursive dependencies,
		// so multi-name specs and iota blocks reserve every sibling name.
		// Constants of one iota block share a key: only the block is emitted.
		if _, added := s.admit(d); !added {
			if s.conflictErr != nil {
				return s.conflictErr
			}
		}
	}
	sort.SliceStable(s.Missing, func(i, j int) bool { return s.Missing[i] < s.Missing[j] })
	return nil
}

func (s *Selection) closure() error {
	queue := make([]pending, 0, len(s.Decls))
	for _, d := range s.Decls {
		queue = append(queue, pending{d: d})
	}
	for len(queue) > 0 {
		it := queue[0]
		queue = queue[1:]
		queue = append(queue, s.scan(it.d, it.hops)...)
		if s.conflictErr != nil {
			return s.conflictErr
		}
	}
	return nil
}

// ============================== scanning ==============================

// scan walks one declaration and returns the declarations it newly pulls in.
// It also records the source edits needed for identifiers it rewrites.
func (s *Selection) scan(d *Decl, hops int) []pending {
	node := d.Node
	if d.Spec != nil {
		node = d.Spec
	}
	ix := d.Idx
	info := ix.info

	// Selector Sel positions are field/method names, never package objects.
	selPos := map[token.Pos]bool{}
	ast.Inspect(node, func(n ast.Node) bool {
		if se, ok := n.(*ast.SelectorExpr); ok {
			selPos[se.Sel.Pos()] = true
		}
		return true
	})

	var out []pending
	// add admits a child exactly once and queues it for scanning. It reports
	// whether the child is inlined, i.e. whether the caller must drop the
	// qualifier it is looking at.
	add := func(child *Decl) bool {
		inlined, added := s.admit(child)
		if !inlined {
			return false
		}
		if added {
			next := hops
			// -follow-depth 按「跨包次数」计数。同包的方法、辅助声明不能另计一跳
			if child.pkg() != d.pkg() {
				next++
			}
			out = append(out, pending{d: child, hops: next})
		}
		return true
	}

	ast.Inspect(node, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.SelectorExpr:
			id, ok := x.X.(*ast.Ident)
			if !ok {
				return true
			}
			pn, ok := info.Uses[id].(*types.PkgName)
			if !ok {
				return true
			}
			obj := info.Uses[x.Sel]
			if obj == nil {
				return false
			}
			path := pn.Imported().Path()
			if dep, ok := s.crossCandidate(obj, path, hops); ok && add(dep) {
				// The package is inlined: drop the "pkg." qualifier.
				d.Refs = append(d.Refs, Ref{
					Kind:  refStrip,
					Pos:   id.Pos(),
					End:   x.Sel.Pos(),
					Local: id.Name,
					Path:  path,
				})
				return false
			}
			d.Refs = append(d.Refs, Ref{Kind: refAlias, Pos: id.Pos(), Local: id.Name, Path: path})
			s.noteExternal(path)
			return false

		case *ast.Ident:
			if selPos[x.Pos()] {
				return true
			}
			obj := info.Uses[x]
			if obj == nil {
				return true
			}
			if v, ok := obj.(*types.Var); ok && v.IsField() {
				return true
			}
			if obj.Pkg() == nil {
				return true // predeclared
			}
			if obj.Pkg() == ix.pkg.Raw.Types {
				dep := ix.byObject(obj)
				if dep == nil {
					return true // local variable, parameter, ...: nothing to move
				}
				if add(dep) {
					return true
				}
				// The declaration stays an import (name clash, depth limit),
				// so this bare name no longer means the same thing in the
				// output package: it has to be qualified again. This is the
				// mirror image of the refStrip rewrite below.
				if p := ix.pkg.Path; p != "" && p != s.Prog.Root.Path {
					if ast.IsExported(x.Name) {
						d.Refs = append(d.Refs, Ref{Kind: refDot, Pos: x.Pos(), Local: x.Name, Path: p})
					} else {
						s.noteOnce("unexported:"+p+"."+x.Name, fmt.Sprintf(
							"cannot keep %s.%s by name: it is unexported", ix.pkg.Name, x.Name))
					}
				}
				return true
			}
			path := obj.Pkg().Path()
			if dep, ok := s.crossCandidate(obj, path, hops); ok && add(dep) {
				// Dot imported and now inlined: the bare identifier stays bare.
				return true
			}
			if ix.isDotImported(d.File, path) {
				// Bare identifier from a dot import: it must gain a qualifier.
				d.Refs = append(d.Refs, Ref{Kind: refDot, Pos: x.Pos(), Local: x.Name, Path: path})
			}
			s.noteExternal(path)
			return true
		}
		return true
	})

	if d.Kind == "type" {
		if s.cfg.IncludeMethods {
			if ms, ok := ix.meths[d.Name]; ok {
				names := make([]string, 0, len(ms))
				for mn := range ms {
					names = append(names, mn)
				}
				sort.Strings(names)
				for _, mn := range names {
					add(ms[mn])
				}
			}
		}
		if s.cfg.IncludeCtors {
			for _, fn := range ix.ctors[d.Name] {
				if fd, ok := ix.funcs[fn]; ok {
					add(fd)
				}
			}
		}
	}
	return out
}

// crossCandidate maps an object from another package to the declaration that
// should be inlined, if that package is followed.
func (s *Selection) crossCandidate(obj types.Object, path string, hops int) (*Decl, bool) {
	if path == "" || path == s.Prog.Root.Path {
		return nil, false
	}
	if s.blocked[path] {
		// The package stays imported as a whole, so nothing from it may be
		// copied: the copies would not share type identity with the import.
		return nil, false
	}
	if s.follow == nil || !s.follow.wants(path, s.Prog.isStd(path)) {
		return nil, false
	}
	pkg := s.Prog.packageFor(path)
	if pkg == nil || pkg.Idx == nil {
		// once a package is partially inlined, an object that
		// cannot be indexed must be a hard error. Keeping the import while
		// other declarations are local creates two type identities.
		if s.packageInlined(path) {
			s.conflictErr = fmt.Errorf(
				"cannot inline %s.%s: package is already partially inlined and the declaration is unavailable",
				path, obj.Name())
		}
		return nil, false
	}
	d := pkg.Idx.byObject(obj)
	if d == nil {
		if s.packageInlined(path) {
			s.conflictErr = fmt.Errorf(
				"cannot inline %s.%s: declaration is not indexed although the package is already inlined",
				path, obj.Name())
		}
		return nil, false
	}
	// already-inlined declarations are reused regardless of
	// hops. A diamond dependency can reach the same declaration first through a
	// short path and later through a deep path; the deep path must not turn the
	// local copy back into an imported "pkg.X" reference.
	if _, ok := s.seen[d.key()]; ok {
		return d, true
	}
	// hops 是到达当前声明已经跨过的包数，再跟一个包就是 hops+1。
	if s.follow.maxDepth >= 0 && hops+1 > s.follow.maxDepth {
		s.noteOnce("depth:"+path, fmt.Sprintf(
			"follow depth %d reached: %s stays an import", s.follow.maxDepth, path))
		return nil, false
	}
	return d, true
}

func (s *Selection) packageInlined(path string) bool {
	for _, d := range s.Decls {
		if d.pkg().Path == path {
			return true
		}
	}
	return false
}

// admit adds a declaration to the selection, resolving its output name.
func (s *Selection) admit(d *Decl) (inlined, added bool) {
	if d == nil {
		return false, false
	}
	k := d.key()
	if _, ok := s.seen[k]; ok {
		return true, false
	}
	if s.cfg.MaxDecls > 0 && len(s.Decls) >= s.cfg.MaxDecls {
		if s.conflictErr == nil {
			s.conflictErr = fmt.Errorf("declaration limit %d reached while inlining %s; "+
				"narrow -names/-follow or raise -max-decls", s.cfg.MaxDecls, d.qualifiedName())
		}
		return false, false
	}
	// copying declarations without init() changes behavior.
	// Keep the package as an import instead of silently dropping side effects.
	if p := d.pkg(); p != nil && p != s.Prog.Root && p.Idx != nil && p.Idx.hasInit() {
		s.kept[p.Path] = "package has an init function"
		return false, false
	}
	// resolve all names of the emission unit together. A
	// sibling that loses a name conflict cannot be silently left in the source
	// text: it would be emitted a second time and fail to compile.
	name, names, keep, err := s.assignUnitNames(d)
	if err != nil {
		s.conflictErr = err
		return false, false
	}
	if keep {
		if p := d.pkg().Path; p != "" && p != s.Prog.Root.Path {
			s.kept[p] = "one of its declaration names is already taken"
		}
		return false, false
	}
	s.seen[k] = d
	d.OutName = name
	d.sel = s
	// A fresh admission starts with a clean edit list: Resolve may run several
	// rounds over the same Decl values.
	d.Refs = nil
	if name != d.Name && d.Obj != nil {
		s.renames[d.Obj] = name
	}
	for old, newName := range names {
		s.owner[newName] = d
		if old == d.Name || old == newName {
			continue
		}
		if obj := s.siblingObject(d, old); obj != nil {
			s.renames[obj] = newName
		}
	}
	s.Decls = append(s.Decls, d)
	return true, true
}

func (s *Selection) assignName(d *Decl) (name string, keep bool, err error) {
	if d.Kind == "method" {
		// Method names follow their receiver type, which is admitted separately.
		return d.Name, false, nil
	}
	base := d.Name
	if s.nameFree(base, d) {
		s.owner[base] = d
		return base, false, nil
	}
	switch s.cfg.Conflict {
	case ConflictError:
		return "", false, fmt.Errorf("name %q from %s clashes with %s",
			base, d.pkg().Path, s.ownerName(base))
	case ConflictPrefix:
		prefix := capitalize(d.pkg().Name)
		cand := prefix + base
		for i := 2; !s.nameFree(cand, d); i++ {
			cand = prefix + base + itoa(i)
		}
		s.owner[cand] = d
		s.Notes = append(s.Notes, fmt.Sprintf("renamed %s.%s to %s", d.pkg().Name, base, cand))
		return cand, false, nil
	default:
		s.noteOnce("clash:"+base, fmt.Sprintf("name clash on %q: %s.%s stays an import (%s)",
			base, d.pkg().Name, base, s.ownerName(base)))
		return "", true, nil
	}
}

func (s *Selection) ownerName(name string) string {
	if d, ok := s.owner[name]; ok {
		return d.pkg().Path
	}
	if d, ok := s.Prog.Root.Idx.byName[name]; ok {
		return d.pkg().Path + " (declared here, not selected)"
	}
	return "unknown"
}

// nameFree reports whether name can be used in the output package. Names that
// already exist in the target package are taken by default: the generated file
// is meant to live next to them.
func (s *Selection) nameFree(name string, d *Decl) bool {
	if o, ok := s.owner[name]; ok {
		return o == d
	}
	if td, ok := s.Prog.Root.Idx.byName[name]; ok && td != d {
		return false
	}
	return true
}

func (s *Selection) noteExternal(path string) {
	if path == "" || path == s.Prog.Root.Path {
		return
	}
	if s.Prog.isStd(path) {
		return // std is expected: every Go program imports it
	}
	if s.Prog.ModulePath != "" && (path == s.Prog.ModulePath ||
		strings.HasPrefix(path, s.Prog.ModulePath+"/")) {
		return
	}
	s.extDeps[path] = true
}

func (s *Selection) noteOnce(key, msg string) {
	if s.noteSeen[key] {
		return
	}
	s.noteSeen[key] = true
	s.Notes = append(s.Notes, msg)
}

func (s *Selection) inlinedCount() int {
	n := 0
	for _, d := range s.Decls {
		if d.pkg() != s.Prog.Root {
			n++
		}
	}
	return n
}

func (s *Selection) inlinedPackages() int {
	set := map[string]bool{}
	for _, d := range s.Decls {
		if d.pkg() != s.Prog.Root {
			set[d.pkg().Path] = true
		}
	}
	return len(set)
}

// ============================== reporting helpers ==============================

// ExternalDeps lists the packages the result still imports.
func (s *Selection) ExternalDeps() []string {
	out := make([]string, 0, len(s.extDeps))
	for p := range s.extDeps {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// Origin summarises where each declaration came from.
func (s *Selection) Origin() []string {
	var out []string
	for _, d := range s.sortedDecls() {
		origin := d.pkg().Path
		if d.pkg() == s.Prog.Root {
			origin = "(target)"
		}
		line := fmt.Sprintf("%-7s %-30s %s", d.Kind, d.Name, origin)
		if d.OutName != d.Name && d.Kind != "method" {
			line += "  -> " + d.OutName
		}
		out = append(out, line)
	}
	return out
}

func (s *Selection) sortedDecls() []*Decl {
	out := make([]*Decl, len(s.Decls))
	copy(out, s.Decls)
	sortDecls(out)
	return out
}
