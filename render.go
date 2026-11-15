package main

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/build/constraint"
	"go/format"
	"go/token"
	"go/types"
	"sort"
	"strconv"
	"strings"
)

// File is one rendered output file.
type File struct {
	Name       string // base name, e.g. user_ext_linux_amd64.go
	Slug       string // build-matrix suffix used in the name
	Path       string // where it was written ("" before write)
	PkgName    string
	ModuleDir  string   // module root, needed by the verifier
	GoVersion  string   // "go" directive of that module
	Constraint string   // //go:build expression, "" for a portable file
	Targets    []Target // targets this file covers
	Content    []byte
	// Warnings are collected by stageEmit. Keeping diagnostics on the file
	// avoids writing directly to os.Stderr from a library emitter.
	Warnings []string
}

type taggedEmitter struct{}

// variant is one fully rendered target.
type variant struct {
	sel     *Selection
	decls   []*Decl
	paths   []string
	canon   map[string]string
	texts   map[string]string // decl key -> rendered text
	derived string            // //go:build expression implied by the sources
	text    string            // whole formatted file
}

// ============================== emitter ==============================
func (taggedEmitter) Emit(ctx context.Context, sels []*Selection, cfg *Config) ([]File, error) {
	if len(sels) == 0 {
		return nil, errors.New("nothing to emit")
	}
	variants := make([]*variant, 0, len(sels))
	for _, sel := range sels {
		v, err := renderVariant(sel, sels, cfg)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", sel.Target, err)
		}
		variants = append(variants, v)
	}
	var files []File
	var err error
	if cfg.SplitByPackage {
		files, err = emitByPackage(variants, cfg)
	} else {
		files, err = emitVariantSet(variants, variants[0].sel.PkgName, cfg)
	}
	if err != nil {
		return nil, err
	}
	// reject duplicate names before the writer can silently
	// overwrite one generated file with another.
	if err := ensureUniqueFileNames(files); err != nil {
		return nil, err
	}
	return files, nil
}

func renderVariant(sel *Selection, all []*Selection, cfg *Config) (*variant, error) {
	if len(sel.Decls) == 0 {
		// -allow-partial must not produce an empty package-only
		// file. An empty selection means every requested name is missing.
		return nil, fmt.Errorf("no declarations selected")
	}
	decls := sel.sortedDecls()
	paths := collectPaths(decls)
	canon := newCanon(paths, declNames(decls), all, cfg)
	texts, err := renderTexts(decls, canon, cfg)
	if err != nil {
		return nil, err
	}
	text, err := assemble(sel.PkgName, decls, texts, paths, canon, cfg)
	if err != nil {
		return nil, err
	}
	return &variant{
		sel:     sel,
		decls:   decls,
		paths:   paths,
		canon:   canon,
		texts:   texts,
		derived: andConstraints(declConstraints(decls)),
		text:    text,
	}, nil
}

// declConstraints lists the non empty source constraints of a declaration set.
func declConstraints(decls []*Decl) []string {
	var out []string
	for _, d := range decls {
		if d.Constraint != "" {
			out = append(out, d.Constraint)
		}
	}
	return out
}

// orConstraints ORs expressions that are each sufficient on their own
// (targets that rendered the same text). Declarations that share one file
// are not alternatives; those use andConstraints.
func orConstraints(in []string) string {
	// an empty constraint means "available everywhere", so it
	// is the identity for OR, not something to drop. "" || "linux" is "".
	// Dropping it would restrict targets where the declaration is portable.
	for _, c := range in {
		if strings.TrimSpace(c) == "" {
			return ""
		}
	}
	return joinConstraints(in, " || ")
}

// andConstraints ANDs constraints of declarations emitted into the same file.
// An empty constraint means the declaration exists on every target and is
// left out. Using OR here would compile a linux-only decl on amd64-only targets.
func andConstraints(in []string) string {
	return joinConstraints(in, " && ")
}

func joinConstraints(in []string, op string) string {
	if len(in) == 0 {
		return ""
	}
	seen := map[string]bool{}
	var uniq []string
	for _, c := range in {
		c = strings.TrimSpace(c)
		if c == "" || seen[c] {
			continue
		}
		seen[c] = true
		uniq = append(uniq, c)
	}
	sort.Strings(uniq)
	if len(uniq) == 1 {
		return uniq[0]
	}
	parts := make([]string, 0, len(uniq))
	for _, c := range uniq {
		parts = append(parts, parenExpr(c))
	}
	return strings.Join(parts, op)
}

// emitSingle renders one portable file (no build constraints).
func emitSingle(variants []*variant, prefix string, cfg *Config) ([]File, error) {
	first := variants[0]
	meta := func(f *File, sel *Selection) {
		f.ModuleDir = sel.Prog.ModuleDir
		f.GoVersion = sel.Prog.GoVersion
	}
	allTargets := make([]Target, 0, len(variants))
	for _, v := range variants {
		allTargets = append(allTargets, v.sel.Target)
	}
	// 单 variant 分支
	if len(variants) == 1 {
		expr := first.derived
		if expr == "" && cfg.KeepTags {
			expr = constraintFor(allTargets)
		}
		f := File{
			PkgName: first.sel.PkgName, Targets: allTargets,
			Constraint: expr,
			Content:    []byte(withConstraint(annotate(first.text, allTargets), expr)),
		}
		meta(&f, first.sel)
		f.Name = fileName(cfg, prefix, "")
		return []File{f}, nil
	}

	// merging is only meaningful when every target has the same
	// declaration set. If one target adds LinuxOnly and another adds
	// WindowsOnly, no single //go:build expression (and no single text) can
	// represent both; AND would produce an impossible constraint.
	if !variantsIdentical(variants) {
		if diff := declKeySetDiff(variants); diff != "" {
			return nil, fmt.Errorf(
				"-split=none cannot merge target-specific declarations (%s); "+
					"use -split=auto or -split=target", diff)
		}
	}
	decls := mergeVariantDecls(variants)
	paths := collectPaths(decls)
	canon := newCanon(paths, declNames(decls), variants2sels(variants), cfg)
	texts, err := renderTexts(decls, canon, cfg)
	if err != nil {
		return nil, err
	}
	text, err := assemble(first.sel.PkgName, decls, texts, paths, canon, cfg)
	if err != nil {
		return nil, err
	}

	var warnings []string
	// A merged file is only correct if no declaration renders differently per
	// target. Report every divergence instead of silently picking one.
	if divergent := divergences(variants); len(divergent) > 0 {
		warnings = append(warnings, fmt.Sprintf(
			"-split=none merged %d declaration(s) that differ per target "+
				"(the %s version was kept): %s", len(divergent), first.sel.Target,
			strings.Join(divergent, ", ")))
	}

	// 多 variant 合并分支
	targets := make([]Target, 0, len(variants))
	for _, v := range variants {
		targets = append(targets, v.sel.Target)
	}
	f := File{PkgName: first.sel.PkgName, Targets: targets, Warnings: warnings}
	// 文本相同：每个 variant 的约束（声明交集）都是充分条件，用 OR。
	// 文本不同却合成一个文件时，这些声明同时存在，必须 AND，不能再 OR 放宽。
	var constraint string
	if variantsIdentical(variants) {
		constraint = orConstraints(variantDerived(variants))
	} else {
		constraint = andConstraints(declConstraints(decls))
	}
	if constraint == "" && cfg.KeepTags {
		constraint = constraintFor(targets)
	}
	f.Constraint = constraint
	f.Content = []byte(withConstraint(annotate(text, targets), f.Constraint))
	f.ModuleDir = first.sel.Prog.ModuleDir
	f.GoVersion = first.sel.Prog.GoVersion
	f.Name = fileName(cfg, prefix, "")
	return []File{f}, nil
}

func variantDerived(vs []*variant) []string {
	out := make([]string, 0, len(vs))
	for _, v := range vs {
		// include empty strings: orConstraints treats them as universal.
		out = append(out, v.derived)
	}
	return out
}

func variantsIdentical(vs []*variant) bool {
	if len(vs) == 0 {
		return true
	}
	for _, v := range vs[1:] {
		if v.text != vs[0].text {
			return false
		}
	}
	return true
}

// divergences lists declarations whose text is not identical across targets.
func divergences(variants []*variant) []string {
	seen := map[string][]string{}
	var order []string
	for _, v := range variants {
		for key, text := range v.texts {
			if _, ok := seen[key]; !ok {
				seen[key] = append(seen[key], v.sel.Target.String()+"\x00"+text)
				order = append(order, key)
				continue
			}
			if !sameText(seen[key], text) {
				seen[key] = append(seen[key], v.sel.Target.String()+"\x00"+text)
			}
		}
	}
	var out []string
	for _, key := range order {
		if len(seen[key]) > 1 {
			name := key
			if i := strings.LastIndex(name, "::"); i >= 0 {
				name = name[i+2:]
			}
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return uniqueSorted(out)
}

func sameText(entries []string, text string) bool {
	for _, e := range entries {
		if _, t, ok := strings.Cut(e, "\x00"); ok && t == text {
			return true
		}
	}
	return false
}

func emitPerTarget(variants []*variant, prefix string, cfg *Config) ([]File, error) {
	type group struct {
		text    string
		targets []Target
		sel     *Selection
		derived []string
	}
	var groups []*group
	for _, v := range variants {
		var g *group
		for _, cand := range groups {
			if cand.text == v.text {
				g = cand
				break
			}
		}
		if g == nil {
			g = &group{text: v.text, sel: v.sel}
			groups = append(groups, g)
		}
		g.targets = append(g.targets, v.sel.Target)
		// Keep empty constraints: they mean "portable", which is the identity
		// for OR and must not be silently turned into a platform restriction.
		g.derived = append(g.derived, v.derived)
	}

	full := len(groups) == 1 && len(groups[0].targets) == len(variants)
	var osCount map[string]int
	if cfg.ShortNames {
		osCount = map[string]int{}
		for _, g := range groups {
			seen := map[string]bool{}
			for _, t := range g.targets {
				if seen[t.GOOS] {
					continue
				}
				seen[t.GOOS] = true
				osCount[t.GOOS]++
			}
		}
	}

	var files []File
	for _, g := range groups {
		expr := orConstraints(g.derived)
		// If every variant is portable, keep the historical behavior of
		// tagging a target-specific group with its requested targets. If g.derived
		// mixes "" with a concrete constraint, the empty value is universal and
		// expr is intentionally empty.
		if len(g.derived) == 0 || constraintsAllEmpty(g.derived) {
			expr = constraintFor(g.targets)
		}
		slug := slugFor(g.targets, full, osCount)
		content := withConstraint(annotate(g.text, g.targets), expr)
		f := File{
			Name:       fileName(cfg, prefix, slug),
			Slug:       slug,
			PkgName:    g.sel.PkgName,
			ModuleDir:  g.sel.Prog.ModuleDir,
			GoVersion:  g.sel.Prog.GoVersion,
			Constraint: expr,
			Targets:    g.targets,
			Content:    []byte(content),
		}
		files = append(files, f)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })
	return files, nil
}

func variants2sels(vs []*variant) []*Selection {
	out := make([]*Selection, 0, len(vs))
	for _, v := range vs {
		out = append(out, v.sel)
	}
	return out
}

// coverageNotes explains which platforms the emitted files are limited to.
func coverageNotes(files []File) []string {
	var notes []string
	constrained := 0
	for _, f := range files {
		if f.Constraint != "" {
			constrained++
		}
	}
	switch {
	case len(files) > 1 && constrained == len(files):
		notes = append(notes, fmt.Sprintf(
			"%d files with //go:build constraints: the result only builds on the requested targets",
			len(files)))
	case len(files) > 1 && constrained > 0:
		notes = append(notes, fmt.Sprintf(
			"%d of %d files carry //go:build constraints; portable files remain available on other targets",
			constrained, len(files)))
	case len(files) == 1 && files[0].Constraint != "":
		notes = append(notes, fmt.Sprintf(
			"single file tagged %q: the result only builds on that target", files[0].Constraint))
	}
	return notes
}

func constraintFor(targets []Target) string {
	if len(targets) == 1 {
		return targets[0].Constraint()
	}
	parts := make([]string, 0, len(targets))
	for _, t := range targets {
		parts = append(parts, t.Constraint())
	}
	sort.Strings(parts)
	return strings.Join(parts, " || ")
}

func slugFor(targets []Target, full bool, osCount map[string]int) string {
	// -short-names: when every target in the group shares one GOOS and that
	// GOOS appears in exactly one group, the arch suffix is redundant. The
	// file name then implies "GOOS" only, which is never narrower than the
	// //go:build line rendered from the sources (see emitPerTarget).
	if osCount != nil && len(targets) > 0 {
		goosSet := map[string]bool{}
		for _, t := range targets {
			goosSet[t.GOOS] = true
		}
		if len(goosSet) == 1 {
			for goos := range goosSet {
				if osCount[goos] == 1 {
					return goos
				}
			}
		}
	}
	if len(targets) == 1 {
		return targets[0].slug()
	}
	if full {
		return "all"
	}
	slugs := make([]string, 0, len(targets))
	for _, t := range targets {
		slugs = append(slugs, t.slug())
	}
	sort.Strings(slugs)
	s := strings.Join(slugs, "-")

	// Go derives implicit GOOS/GOARCH constraints from the
	// last underscore segment of a file name. "linux_arm64-windows_amd64"
	// would silently become amd64-only. Mark multi-target groups explicitly.
	if len(targets) > 1 && slugEndsWithFilenameTag(s) {
		s += "_multi"
	}
	if len(s) > 64 {
		// Keep a stable suffix; truncating in the middle can create a name that
		// ends in a known tag or collides with another group.
		s = s[:48] + "_" + shortHash(s)
	}
	return s
}

func withConstraint(text, expr string) string {
	if expr == "" {
		return text
	}
	head := "//go:build " + expr + "\n"
	// Modules declaring go < 1.17 only understand the legacy form, so emit both
	// lines whenever the expression can be converted.
	if parsed, err := constraint.Parse("//go:build " + expr); err == nil {
		if plus, err := constraint.PlusBuildLines(parsed); err == nil {
			for _, line := range plus {
				head += line + "\n"
			}
		}
	}
	return head + "\n" + text
}

// annotate adds the covered targets to the generated header, after grouping.
func annotate(text string, targets []Target) string {
	if len(targets) == 0 {
		return text
	}
	names := make([]string, 0, len(targets))
	for _, t := range targets {
		names = append(names, t.String())
	}
	sort.Strings(names)
	line := "// Targets: " + strings.Join(names, ", ") + "\n"
	if i := strings.Index(text, generatedHeader); i == 0 {
		return generatedHeader + line + text[len(generatedHeader):]
	}
	return line + text
}

// generatedHeader 是覆盖保护（write.go）和 Targets 行插入点的共同标记。
// 不写这行的话，第二次运行会把自己的输出当成手写文件并拒绝覆盖。
const generatedHeader = "// Code generated by vexport; DO NOT EDIT.\n"

// fileName decides the base name of an output file.
func fileName(cfg *Config, pkgName, slug string) string {
	base := pkgName
	// package names may end in _linux/_amd64. A file named
	// foo_linux.go gets an implicit GOOS constraint even if its content is
	// portable; add a neutral segment to keep the name honest.
	if hasFilenameTagSuffix(base) {
		base += "_gen"
	}
	base += cfg.Suffix
	if slug != "" {
		base += "_" + slug
	}
	return base + ".go"
}

// ============================== assembly ==============================

// renderTexts renders every declaration once.
func renderTexts(decls []*Decl, canon map[string]string, cfg *Config) (map[string]string, error) {
	out := make(map[string]string, len(decls))
	for _, d := range decls {
		text, err := declText(d, canon, cfg)
		if err != nil {
			return nil, err
		}
		out[d.key()] = strings.TrimRight(text, " \t\r\n")
	}
	return out, nil
}

// assemble renders header, package clause, imports and body, then gofmts it.
func assemble(pkgName string, decls []*Decl, texts map[string]string, paths []string,
	canon map[string]string, cfg *Config,
) (string, error) {
	if !validIdent(pkgName) {
		return "", fmt.Errorf("invalid package name %q", pkgName)
	}
	var body strings.Builder
	emitted := map[string]bool{}
	for _, d := range decls {
		if emitted[d.key()] {
			continue // one emission unit, one copy
		}
		text := texts[d.key()]
		if text == "" {
			continue
		}
		emitted[d.key()] = true
		body.WriteString(text)
		body.WriteString("\n\n")
	}
	src := body.String()

	var head strings.Builder
	head.WriteString(generatedHeader)
	head.WriteString("\n")
	head.WriteString("package ")
	head.WriteString(pkgName)
	head.WriteString("\n\n")

	var imports strings.Builder
	specs := buildImports(paths, canon, src, cfg)
	if len(specs) > 0 {
		imports.WriteString("import (\n")
		for _, sp := range specs {
			if sp.Alias == "" {
				imports.WriteString("\t")
				imports.WriteString(strconv.Quote(sp.Path))
				imports.WriteString("\n")
				continue
			}
			imports.WriteString("\t")
			imports.WriteString(sp.Alias)
			imports.WriteString(" ")
			imports.WriteString(strconv.Quote(sp.Path))
			imports.WriteString("\n")
		}
		imports.WriteString(")\n\n")
	}

	out, err := format.Source([]byte(head.String() + imports.String() + src))
	if err != nil {
		if cfg.Verbose {
			return "", fmt.Errorf("generated code does not parse: %w\n----\n%s", err,
				head.String()+imports.String()+src)
		}
		return "", fmt.Errorf("generated code does not parse: %w", err)
	}
	return string(out), nil
}

type importSpec struct{ Path, Alias string }

// buildImports renders the import block. An import whose alias never appears
// as a qualifier in the body is dropped: it would not compile, and it can only
// come from a reference that lived outside the copied range.
func buildImports(paths []string, canon map[string]string, body string, cfg *Config) []importSpec {
	used := usedQualifiers(body)
	var out []importSpec
	for _, p := range paths {
		alias := canon[p]
		if alias == "" {
			continue
		}
		if used != nil {
			if !used[alias] {
				continue
			}
		} else if !strings.Contains(body, alias+".") {
			continue
		}
		np := applyRewrites(cfg, p)
		sp := importSpec{Path: np, Alias: alias}
		if alias == importNameHint(np) {
			sp.Alias = ""
		}
		out = append(out, sp)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		return out[i].Alias < out[j].Alias
	})
	return out
}

func pathBase(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

// importNameHint 从 import path 猜默认包名：跳过 /vN 主版本后缀。
// 例：example.com/a/v2 → a；example.com/foo → foo。
func importNameHint(p string) string {
	base := pathBase(p)
	if isMajorVersion(base) {
		if i := strings.LastIndex(p, "/"); i > 0 {
			return pathBase(p[:i])
		}
	}
	return base
}

func isMajorVersion(s string) bool {
	if len(s) < 2 || s[0] != 'v' {
		return false
	}
	for _, c := range s[1:] {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// ============================== aliases ==============================

// collectPaths lists the import paths still needed by these declarations.
func collectPaths(decls []*Decl) []string {
	set := map[string]bool{}
	for _, d := range decls {
		for _, r := range d.Refs {
			if r.Kind == refStrip || r.Path == "" {
				continue
			}
			set[r.Path] = true
		}
	}
	out := make([]string, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

func declNames(decls []*Decl) map[string]bool {
	out := map[string]bool{}
	for _, d := range decls {
		if d.Kind == "method" {
			continue
		}
		out[d.OutName] = true
		// sibling names of a multi-name spec/iota block are
		// package-level identifiers too. An import alias must not shadow them.
		for _, n := range d.siblingNames() {
			out[n] = true
		}
	}
	return out
}

// newCanon picks one alias per import path, avoiding clashes with declaration
// names, with Go keywords and with other aliases.
func newCanon(paths []string, taken map[string]bool, sels []*Selection, cfg *Config) map[string]string {
	used := map[string]bool{}
	for n := range taken {
		used[n] = true
	}
	// an import alias lives in file scope. It must not shadow
	// package-level names that already exist in the target package (including
	// declarations that were not selected) or predeclared identifiers.
	for _, sel := range sels {
		if sel == nil || sel.Prog == nil || sel.Prog.Root == nil || sel.Prog.Root.Idx == nil {
			continue
		}
		for n := range sel.Prog.Root.Idx.byName {
			used[n] = true
		}
	}
	for _, n := range types.Universe.Names() {
		used[n] = true
	}
	canon := map[string]string{}
	for _, p := range paths {
		np := applyRewrites(cfg, p)
		name := ""
		for _, sel := range sels {
			if raw, ok := sel.Prog.All[p]; ok && raw != nil && raw.Name != "" {
				name = raw.Name
				break
			}
		}
		if name == "" {
			name = importNameHint(np)
		}
		if !validIdent(name) {
			name = "pkg"
		}
		cand := name
		for i := 2; used[cand]; i++ {
			cand = name + itoa(i)
		}
		used[cand] = true
		canon[p] = cand
	}
	return canon
}

func applyRewrites(cfg *Config, p string) string {
	for _, r := range cfg.rewrites {
		if np, ok := r.Apply(p); ok {
			return np
		}
	}
	return p
}

// ============================== declaration text ==============================

type edit struct {
	off  int
	size int
	text string
}

// declText copies the source range of one declaration and applies the recorded
// edits: dropping inlined qualifiers, renaming import aliases, qualifying dot
// imports and applying conflict renames.
func declText(d *Decl, canon map[string]string, cfg *Config) (string, error) {
	prog := d.pkg().Prog
	fset := d.Idx.fset
	src := prog.src(d.Node.Pos())
	if src == nil {
		return "", fmt.Errorf("%s: source unavailable", d.Name)
	}

	var start, end token.Pos
	prefix := ""
	// lead is a doc comment that has to be written in front of the copied text
	// instead of being part of it (see the grouped GenDecl case below).
	lead := ""
	if gd, ok := d.Node.(*ast.GenDecl); ok && d.Spec != nil {
		start, end = specStart(d.Spec, gd), d.Spec.End()
		switch gd.Tok {
		case token.TYPE:
			prefix = "type "
		case token.VAR:
			prefix = "var "
		case token.CONST:
			prefix = "const "
		}
		// A doc comment written above "const (" / "type (" belongs to the
		// GenDecl, not to its first spec. specStart reports it so that
		// splitting a group does not drop the comment, but the group header
		// itself must never be part of the copied range: the text would start
		// inside "const (", the "const " prefix would land behind the opening
		// parenthesis and the file would no longer parse. Copy the comment
		// only and start the body at the spec.
		if gd.Doc != nil && gd.Lparen.IsValid() && len(gd.Specs) > 0 &&
			gd.Specs[0] == d.Spec && start < gd.TokPos {
			lead = string(src[fset.Position(gd.Doc.Pos()).Offset:fset.Position(gd.TokPos).Offset])
			start = d.Spec.Pos()
		}
	} else {
		start, end = docStart(d.Node), d.Node.End()
	}
	s0 := fset.Position(start).Offset
	e0 := fset.Position(end).Offset
	if s0 < 0 || e0 > len(src) || s0 >= e0 {
		return "", fmt.Errorf("%s: bad source range [%d:%d]", d.Name, s0, e0)
	}
	off := func(p token.Pos) int { return fset.Position(p).Offset }

	var edits []edit
	if prefix != "" {
		edits = append(edits, edit{off: off(d.Spec.Pos()), text: prefix})
	}
	for _, r := range d.Refs {
		switch r.Kind {
		case refStrip:
			lo := off(r.Pos)
			hi := off(r.End)
			if hi <= lo || hi > e0 {
				return "", fmt.Errorf("%s: cannot locate %q qualifier", d.Name, r.Local)
			}
			// Only delete the qualifier if the source really reads "pkg." here.
			got := strings.ReplaceAll(string(src[lo:hi]), " ", "")
			if got != r.Local+"." {
				return "", fmt.Errorf("%s: unexpected qualifier text %q (want %q)",
					d.Name, string(src[lo:hi]), r.Local+".")
			}
			edits = append(edits, edit{off: lo, size: hi - lo})
		case refAlias:
			want := canon[r.Path]
			if want != "" && want != r.Local {
				edits = append(edits, edit{off: off(r.Pos), size: len(r.Local), text: want})
			}
		case refDot:
			if want := canon[r.Path]; want != "" {
				edits = append(edits, edit{off: off(r.Pos), size: 0, text: want + "."})
			}
		}
	}
	if rens := d.selRenames(); len(rens) > 0 {
		ast.Inspect(scopeNode(d), func(n ast.Node) bool {
			id, ok := n.(*ast.Ident)
			if !ok {
				return true
			}
			if id.Name == "_" {
				return true
			}
			// An embedded field identifier plays two roles at once: it defines
			// the field and it uses the type. go/types records the first role
			// in Defs (the field object) and the second in Uses (the type), so
			// both have to be consulted. This is what makes the rename reach
			// the caller sites, not just the declaration.
			for _, obj := range [2]types.Object{d.Idx.info.Defs[id], d.Idx.info.Uses[id]} {
				want, ok := callerRename(obj, rens)
				if !ok || want == id.Name {
					continue
				}
				edits = append(edits, edit{off: off(id.Pos()), size: len(id.Name), text: want})
				break
			}
			return true
		})
	}

	sort.SliceStable(edits, func(i, j int) bool {
		if edits[i].off != edits[j].off {
			return edits[i].off < edits[j].off
		}
		return edits[i].size < edits[j].size
	})

	var b strings.Builder
	if lead != "" {
		b.WriteString(strings.TrimRight(lead, " \t\r\n"))
		b.WriteString("\n")
	}
	cur := s0
	for _, ed := range edits {
		if ed.off < s0 || ed.off+ed.size > e0 || ed.off < cur {
			continue
		}
		b.Write(src[cur:ed.off])
		b.WriteString(ed.text)
		cur = ed.off + ed.size
	}
	b.Write(src[cur:e0])
	return b.String(), nil
}

func scopeNode(d *Decl) ast.Node {
	if d.Spec != nil {
		return d.Spec
	}
	return d.Node
}

func (d *Decl) selRenames() map[types.Object]string {
	if d.sel == nil {
		return nil
	}
	return d.sel.renames
}

// ============================== caller rewrite ==============================
//
// When -follow inlines a declaration from another package and -conflict
// renames it, every reference to that declaration has to follow the new name.
// The qualifier removal (refStrip) only drops "pkg.", so the name itself is
// supplied here: this is the caller rewrite.

// callerRename returns the name an identifier must use in the output when it
// refers to an inlined declaration that was renamed.
func callerRename(obj types.Object, rens map[types.Object]string) (string, bool) {
	if obj == nil {
		return "", false
	}
	if want, ok := rens[obj]; ok {
		return want, true
	}
	return embeddedFieldRename(obj, rens)
}

// embeddedFieldRename handles the indirect case: an embedded struct field is
// named after its type, so renaming the type also renames the field. All three
// forms end up at the same field object, which is not the renamed type object
// and therefore invisible to a plain rens lookup:
//
//	type Holder struct{ model.User }   // field declaration
//	h.User.Name                        // promoted access
//	Holder{User: u}                    // struct literal key
//
// The rule only applies when the field really is derived from the type name:
// an embedded alias (type X = model.User, struct{ X }) keeps the name X.
func embeddedFieldRename(obj types.Object, rens map[types.Object]string) (string, bool) {
	v, ok := obj.(*types.Var)
	if !ok || !v.Embedded() || !v.IsField() {
		return "", false
	}
	named := namedOf(v.Type())
	if named == nil || named.Obj() == nil || named.Obj().Name() != v.Name() {
		return "", false
	}
	want, ok := rens[named.Obj()]
	return want, ok
}

// namedOf unwraps pointers and returns the named type, if any.
func namedOf(t types.Type) *types.Named {
	for {
		p, ok := t.(*types.Pointer)
		if !ok {
			break
		}
		t = p.Elem()
	}
	named, ok := t.(*types.Named)
	if !ok {
		return nil
	}
	return named
}

func docStart(n ast.Node) token.Pos {
	switch t := n.(type) {
	case *ast.FuncDecl:
		if t.Doc != nil {
			return t.Doc.Pos()
		}
	case *ast.GenDecl:
		if t.Doc != nil {
			return t.Doc.Pos()
		}
	}
	return n.Pos()
}

func specStart(spec ast.Spec, gd *ast.GenDecl) token.Pos {
	switch t := spec.(type) {
	case *ast.TypeSpec:
		if t.Doc != nil {
			return t.Doc.Pos()
		}
	case *ast.ValueSpec:
		if t.Doc != nil {
			return t.Doc.Pos()
		}
	}
	// a doc comment on "type ( ... )" belongs to the GenDecl,
	// not to the individual TypeSpec. Attach it to the first spec only, so
	// splitting a grouped declaration does not silently drop the comment.
	if gd != nil && gd.Doc != nil && len(gd.Specs) > 0 && gd.Specs[0] == spec {
		return gd.Doc.Pos()
	}
	return spec.Pos()
}

// declPos is a stable, file-local sort key for a declaration.
type declPos struct {
	file   string
	offset int
}

// declFileOffset turns a declaration's token.Pos into a key that survives a
// different FileSet base layout. Only the file name and the offset inside that
// file are stable; the raw Pos is not.
func declFileOffset(d *Decl) (declPos, bool) {
	if d == nil || d.Idx == nil || d.Idx.fset == nil || !d.OrdPos.IsValid() {
		return declPos{}, false
	}
	p := d.Idx.fset.Position(d.OrdPos)
	if !p.IsValid() || p.Filename == "" {
		return declPos{}, false
	}
	return declPos{file: p.Filename, offset: p.Offset}, true
}

func sortDecls(decls []*Decl) {
	sort.SliceStable(decls, func(i, j int) bool {
		a, b := decls[i], decls[j]
		if a.Kind != b.Kind {
			return rank(a.Kind) < rank(b.Kind)
		}
		// const 只能引用源码里更早的常量。按名字排序会把 A = Z+1 排到 Z 前面，编不过。
		//
		// 不能直接比 token.Pos：Pos 是 FileSet 里的全局偏移，而 go/packages
		// 并行解析同一个包的文件，每个文件的 base 取决于解析完成的先后，
		// 于是跨文件的 Pos 顺序每次运行都可能不同，输出顺序（以及报错行号）
		// 也就跟着漂。这里按「文件名 + 文件内偏移」排序：同一文件内保持
		// 源码顺序，跨文件保持稳定顺序。
		if af, aok := declFileOffset(a); aok {
			if bf, bok := declFileOffset(b); bok {
				if af.file != bf.file {
					return af.file < bf.file
				}
				if af.offset != bf.offset {
					return af.offset < bf.offset
				}
			}
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		return a.pkg().Path < b.pkg().Path
	})
}

func uniqueSorted(in []string) []string {
	if len(in) == 0 {
		return in
	}
	sorted := make([]string, len(in))
	copy(sorted, in)
	sort.Strings(sorted)
	out := sorted[:1]
	for _, s := range sorted[1:] {
		if s != out[len(out)-1] {
			out = append(out, s)
		}
	}
	return out
}

// emitVariantSet 对一组 variant 做「identical 判断 + emitSingle/emitPerTarget」。
// Emit 用 variants[0].sel.PkgName 作为 prefix，行为与旧版一致；
// emitByPackage 用来源包名，从而实现按包拆分。
func emitVariantSet(variants []*variant, prefix string, cfg *Config) ([]File, error) {
	identical := true
	for _, v := range variants[1:] {
		if v.text != variants[0].text {
			identical = false
			break
		}
	}
	split := cfg.Split
	if split == SplitAuto {
		if identical {
			split = SplitNone
		} else {
			split = SplitTarget
		}
	}
	if split == SplitNone || identical {
		return emitSingle(variants, prefix, cfg)
	}
	return emitPerTarget(variants, prefix, cfg)
}

// emitByPackage 按来源包把一个 Selection 的声明拆成若干子 variant，
// 每个来源包独立跑一次 emitVariantSet。package 子句仍是目标包，
// 只有文件名前缀用来源包名，避免不同包互相覆盖。
func emitByPackage(variants []*variant, cfg *Config) ([]File, error) {
	type group struct {
		prefix string
		subs   []*variant
	}
	groups := map[string]*group{}
	var order []string

	//  package names are not unique. Reserve the target package
	// name first, then disambiguate any inlined package with the same name (or
	// another package that happens to share a name) using a stable path hash.
	rootPath, rootName := "", ""
	if len(variants) > 0 && variants[0].sel != nil && variants[0].sel.Prog != nil &&
		variants[0].sel.Prog.Root != nil {
		rootPath = variants[0].sel.Prog.Root.Path
		rootName = variants[0].sel.Prog.Root.Name
	}
	if rootName == "" && len(variants) > 0 {
		rootName = variants[0].sel.PkgName
	}
	usedPrefix := map[string]bool{rootName: true}

	for _, v := range variants {
		byPkg := map[string][]*Decl{}
		for _, d := range v.decls {
			p := d.pkg().Path
			byPkg[p] = append(byPkg[p], d)
		}
		// 按 import path 顺序遍历：包名可能重复，前缀消歧（usedPrefix）
		// 依赖访问顺序，map 的随机迭代会让文件内容/前缀在两次运行间变化。
		pkgs := make([]string, 0, len(byPkg))
		for p := range byPkg {
			pkgs = append(pkgs, p)
		}
		sort.Strings(pkgs)
		for _, p := range pkgs {
			ds := byPkg[p]
			g := groups[p]
			if g == nil {
				prefix := ds[0].pkg().Name
				if p != rootPath {
					base := prefix
					for i := 1; usedPrefix[prefix]; i++ {
						prefix = base + "_" + shortHash(p)
						if i > 1 {
							prefix += "_" + itoa(i)
						}
					}
					usedPrefix[prefix] = true
				}
				g = &group{prefix: prefix}
				groups[p] = g
				order = append(order, p)
			}
			sub, err := subsetVariant(v, ds, cfg)
			if err != nil {
				return nil, fmt.Errorf("%s/%s: %w", v.sel.Target, p, err)
			}
			g.subs = append(g.subs, sub)
		}
	}

	var files []File
	for _, p := range order {
		g := groups[p]
		fs, err := emitVariantSet(g.subs, g.prefix, cfg)
		if err != nil {
			return nil, err
		}
		files = append(files, fs...)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })
	return files, nil
}

// subsetVariant 只保留 decls 中的声明，并按它们自己的 import 集合重新渲染。
// canon 按子集重算，保证 alias 不与这组声明的名字冲突（不同文件可以有不同的
// import alias，所以跨包组的 canon 不一致是合法的）。
func subsetVariant(v *variant, decls []*Decl, cfg *Config) (*variant, error) {
	paths := collectPaths(decls)
	canon := newCanon(paths, declNames(decls), []*Selection{v.sel}, cfg)
	texts, err := renderTexts(decls, canon, cfg)
	if err != nil {
		return nil, err
	}
	text, err := assemble(v.sel.PkgName, decls, texts, paths, canon, cfg)
	if err != nil {
		return nil, err
	}
	return &variant{
		sel:     v.sel,
		decls:   decls,
		paths:   paths,
		canon:   canon,
		texts:   texts,
		derived: andConstraints(declConstraints(decls)),
		text:    text,
	}, nil
}
