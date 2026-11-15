package main

import (
	"go/ast"
	"go/build/constraint"
	"go/token"
	"go/types"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Ref is one source edit that has to be applied when a declaration is moved:
// it either deletes an import qualifier (the package is inlined), adds one (a
// dot import becomes qualified) or renames an import alias.
type Ref struct {
	Kind  refKind
	Pos   token.Pos
	End   token.Pos // exclusive; NoPos means Pos + len(Local)
	Local string    // text as written in the source
	Path  string    // import path the identifier belongs to
}

type refKind int

const (
	refDot   refKind = iota // bare ident from a dot import: needs "alias."
	refAlias                // import alias: may need renaming to the canonical alias
	refStrip                // "pkg." prefix of a package that is being inlined
)

// Decl is one extractable declaration.
//
// A Decl instance belongs to exactly one Program (positions are per load), and
// therefore to at most one Selection; Resolve mutates OutName, Refs and sel.
type Decl struct {
	Kind    string // type|func|method|var|const
	Name    string // methods are "Type.Method"
	OutName string // name in the output, after conflict resolution
	Unit    string // emission unit; non empty when several names share one block
	// Constraint is the //go:build expression of the source file, if any. It is
	// the truth about where the declaration exists, and drives output tags.
	Constraint string
	File       *ast.File
	FName      string
	Node       ast.Node
	Spec       ast.Spec
	OrdPos     token.Pos
	Obj        types.Object
	Idx        *Index
	Refs       []Ref
	sel        *Selection
}

// key identifies a declaration across packages. Declarations that must be
// emitted as one unit (an iota const block) share a key, so that selecting two
// constants of the same block does not copy the block twice.
func (d *Decl) key() string {
	if d.Unit != "" {
		return d.Idx.pkg.Path + "::" + d.Unit
	}
	return d.Idx.pkg.Path + "::" + d.Name
}

// siblingNames 是这条声明的输出里还会定义的其他标识符。
// 多名 ValueSpec 和整段 iota 块共用一个 key，只发射一次。
func (d *Decl) siblingNames() []string {
	var idents []*ast.Ident
	if vs, ok := d.Spec.(*ast.ValueSpec); ok {
		idents = vs.Names
	} else if gd, ok := d.Node.(*ast.GenDecl); ok && d.Spec == nil && d.Unit != "" {
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			idents = append(idents, vs.Names...)
		}
	}
	var out []string
	for _, n := range idents {
		if n == nil || n.Name == "_" || n.Name == d.Name {
			continue
		}
		out = append(out, n.Name)
	}
	return out
}

func (d *Decl) pkg() *Package { return d.Idx.pkg }
func (d *Decl) qualifiedName() string {
	if d.Idx.pkg.Prog.Root != nil && d.Idx.pkg.Path != d.Idx.pkg.Prog.Root.Path {
		return d.Idx.pkg.Name + "." + d.Name
	}
	return d.Name
}

// ============================== index ==============================

// Index maps the package level objects of one package back to their
// declaration, so that a types.Object can be turned into "the source text
// that defines it".
type Index struct {
	pkg      *Package
	fset     *token.FileSet
	info     *types.Info
	byPos    map[token.Pos]*Decl
	types    map[string]*Decl
	funcs    map[string]*Decl
	vars     map[string]*Decl
	meths    map[string]map[string]*Decl
	ctors    map[string][]string
	byName   map[string]*Decl
	dotFiles map[*ast.File]map[string]bool
	fileCons map[*ast.File]string
	order    []*Decl
}

func buildIndex(pkg *Package, info *types.Info) *Index {
	ix := &Index{
		pkg:      pkg,
		fset:     pkg.Prog.Fset,
		info:     info,
		byPos:    map[token.Pos]*Decl{},
		types:    map[string]*Decl{},
		funcs:    map[string]*Decl{},
		vars:     map[string]*Decl{},
		meths:    map[string]map[string]*Decl{},
		ctors:    map[string][]string{},
		byName:   map[string]*Decl{},
		dotFiles: map[*ast.File]map[string]bool{},
	}
	ix.scan()
	return ix
}

func (ix *Index) scan() {
	for _, f := range ix.pkg.Files {
		fname := ix.fset.File(f.Pos()).Name()
		for _, d := range f.Decls {
			switch d := d.(type) {
			case *ast.FuncDecl:
				obj := ix.info.Defs[d.Name]
				if obj == nil || d.Name.Name == "_" {
					continue
				}
				name := d.Name.Name
				var dd *Decl
				if d.Recv != nil && len(d.Recv.List) > 0 {
					recv := receiverName(d.Recv.List[0].Type)
					if recv == "" {
						continue
					}
					name = recv + "." + d.Name.Name
					dd = &Decl{
						Kind: "method", Name: name, File: f, FName: fname,
						Node: d, OrdPos: obj.Pos(), Obj: obj, Idx: ix, OutName: name,
					}
					if ix.meths[recv] == nil {
						ix.meths[recv] = map[string]*Decl{}
					}
					ix.meths[recv][d.Name.Name] = dd
				} else {
					dd = &Decl{
						Kind: "func", Name: name, File: f, FName: fname,
						Node: d, OrdPos: obj.Pos(), Obj: obj, Idx: ix, OutName: name,
					}
					ix.funcs[name] = dd
					ix.byName[name] = dd
				}
				ix.byPos[obj.Pos()] = dd
				ix.order = append(ix.order, dd)

			case *ast.GenDecl:
				split := len(d.Specs) > 1 || d.Lparen != token.NoPos
				for _, spec := range d.Specs {
					switch ts := spec.(type) {
					case *ast.TypeSpec:
						obj := ix.info.Defs[ts.Name]
						if obj == nil || ts.Name.Name == "_" {
							continue
						}
						dd := &Decl{
							Kind: "type", Name: ts.Name.Name, File: f, FName: fname,
							Node: d, OrdPos: obj.Pos(), Obj: obj, Idx: ix, OutName: ts.Name.Name,
						}
						if split {
							dd.Spec = ts
						}
						ix.types[ts.Name.Name] = dd
						ix.byName[ts.Name.Name] = dd
						ix.byPos[obj.Pos()] = dd
						ix.order = append(ix.order, dd)
					case *ast.ValueSpec:
						kind := "var"
						if d.Tok == token.CONST {
							kind = "const"
						}
						whole := d.Tok == token.CONST && constWhole(d)
						for _, n := range ts.Names {
							obj := ix.info.Defs[n]
							if obj == nil || n.Name == "_" {
								continue
							}
							dd := &Decl{
								Kind: kind, Name: n.Name, File: f, FName: fname,
								Node: d, OrdPos: obj.Pos(), Obj: obj, Idx: ix, OutName: n.Name,
							}
							if split {
								dd.Spec = ts
							}
							if whole {
								// An iota block is copied as a whole and is one
								// emission unit, whatever constant is selected.
								// The key must be stable across loads: token.Pos
								// differs per target, the first name does not.
								dd.Spec = nil
								dd.Unit = "constblock:" + constBlockName(d)
							} else if len(ts.Names) > 1 {
								// var a, b = ... 是一条 spec。每个名字各算一份的话，
								// 同一段源码会被原样输出两遍，声明重复。
								// FIX(P0-RI-03): 第一个名字可能是 "_"，不能作为 key。
								dd.Unit = kind + "spec:" + firstNonBlankName(ts.Names)
							}
							ix.vars[n.Name] = dd
							ix.byName[n.Name] = dd
							ix.byPos[obj.Pos()] = dd
							ix.order = append(ix.order, dd)
						}
					}
				}
			}
		}
	}

	// Constructors: a function whose result type mentions a package type.
	for name, fd := range ix.funcs {
		fn, ok := fd.Node.(*ast.FuncDecl)
		if !ok || fn.Type == nil || fn.Type.Results == nil {
			continue
		}
		seen := map[string]bool{}
		for _, res := range fn.Type.Results.List {
			for _, t := range typeNames(res.Type) {
				if seen[t] {
					continue
				}
				seen[t] = true
				if _, ok := ix.types[t]; ok {
					ix.ctors[t] = append(ix.ctors[t], name)
				}
			}
		}
	}
	for t := range ix.ctors {
		sort.Strings(ix.ctors[t])
	}
	sort.Slice(ix.order, func(i, j int) bool { return ix.order[i].OrdPos < ix.order[j].OrdPos })
	for _, d := range ix.order {
		d.Constraint = ix.constraintOf(d.File)
	}
}

// constraintOf returns the //go:build expression that governs a source file,
// combining the //go:build line with GOOS/GOARCH file name suffixes.
func (ix *Index) constraintOf(f *ast.File) string {
	if ix.fileCons == nil {
		ix.fileCons = map[*ast.File]string{}
	}
	if c, ok := ix.fileCons[f]; ok {
		return c
	}
	c := fileConstraint(ix.fset, f)
	ix.fileCons[f] = c
	return c
}

// fileConstraint derives the constraint of a file from its name and its
// //go:build line. The result is "" for a portable file.
func fileConstraint(fset *token.FileSet, f *ast.File) string {
	tf := fset.File(f.Pos())
	if tf == nil {
		return ""
	}
	base := filepath.Base(tf.Name())
	expr := headerConstraint(f.Comments, f.Package)
	var parts []string
	if expr != nil {
		parts = append(parts, expr.String())
	}
	for _, tag := range filenameTags(base) {
		// 文件名后缀是额外的合取条件。只在约束已经必然包含该 tag 时才省略；
		// tag 仅仅出现在 || 或 ! 里不算，否则 _linux.go 会被标成 darwin || linux。
		if expr != nil && constraintRequires(expr, tag) {
			continue
		}
		parts = append(parts, tag)
	}
	if len(parts) > 1 {
		for i, p := range parts {
			parts[i] = parenExpr(p)
		}
	}
	return strings.Join(parts, " && ")
}

// headerConstraint 读取 package 子句之前的构建约束。
// 有 //go:build 时忽略 // +build（与 go 命令一致）；否则所有 // +build 行求交。
func headerConstraint(comments []*ast.CommentGroup, pkgPos token.Pos) constraint.Expr {
	var legacy []constraint.Expr
	for _, cg := range comments {
		if cg.Pos() > pkgPos {
			break
		}
		for _, c := range cg.List {
			if constraint.IsGoBuild(c.Text) {
				expr, err := constraint.Parse(c.Text)
				if err != nil {
					return nil
				}
				return expr
			}
			if constraint.IsPlusBuild(c.Text) {
				if expr, err := constraint.Parse(c.Text); err == nil {
					legacy = append(legacy, expr)
				}
			}
		}
	}
	return andExprs(legacy)
}

func andExprs(exprs []constraint.Expr) constraint.Expr {
	var e constraint.Expr
	for _, x := range exprs {
		if x == nil {
			continue
		}
		if e == nil {
			e = x
			continue
		}
		e = &constraint.AndExpr{X: e, Y: x}
	}
	return e
}

// constraintRequires 报告 expr 是否在每个可满足赋值下都带有 tag。
// 合取一边要求即可；析取必须两边都要求；否定不算要求这个 tag。
func constraintRequires(expr constraint.Expr, tag string) bool {
	switch e := expr.(type) {
	case *constraint.TagExpr:
		return e.Tag == tag
	case *constraint.AndExpr:
		return constraintRequires(e.X, tag) || constraintRequires(e.Y, tag)
	case *constraint.OrExpr:
		return constraintRequires(e.X, tag) && constraintRequires(e.Y, tag)
	default:
		return false
	}
}

// filenameTags extracts the GOOS/GOARCH suffix of a file name.
func filenameTags(base string) []string {
	name := strings.TrimSuffix(base, ".go")
	name = strings.TrimSuffix(name, "_test")
	segs := strings.Split(name, "_")
	var tags []string
	if n := len(segs); n >= 2 {
		if knownArch[segs[n-1]] {
			tags = append(tags, segs[n-1])
			if n >= 3 && knownOS[segs[n-2]] {
				tags = append([]string{segs[n-2]}, tags...)
			}
		} else if knownOS[segs[n-1]] {
			tags = append(tags, segs[n-1])
		}
	}
	return tags
}

func parenExpr(expr string) string {
	if strings.ContainsAny(expr, "&|") {
		return "(" + expr + ")"
	}
	return expr
}

var knownOS = map[string]bool{
	"aix": true, "android": true, "darwin": true, "dragonfly": true, "freebsd": true,
	"hurd": true, "illumos": true, "ios": true, "js": true, "linux": true,
	"netbsd": true, "openbsd": true, "plan9": true, "solaris": true, "wasip1": true,
	"windows": true, "zos": true,
}

var knownArch = map[string]bool{
	"386": true, "amd64": true, "amd64p32": true, "arm": true, "arm64": true,
	"loong64": true, "mips": true, "mips64": true, "mips64le": true, "mipsle": true,
	"ppc64": true, "ppc64le": true, "riscv64": true, "s390x": true, "wasm": true,
}

// byObject finds the declaration of a package level object.
func (ix *Index) byObject(obj types.Object) *Decl {
	if obj == nil {
		return nil
	}
	if d, ok := ix.byPos[obj.Pos()]; ok {
		return d
	}
	if f, ok := obj.(*types.Func); ok {
		if sig, ok := f.Type().(*types.Signature); ok && sig.Recv() != nil {
			if recv := recvTypeName(sig.Recv().Type()); recv != "" {
				return ix.meths[recv][f.Name()]
			}
		}
	}
	return nil
}

func (ix *Index) lookup(name string) (*Decl, bool) {
	if i := strings.LastIndex(name, "."); i > 0 {
		if ms, ok := ix.meths[name[:i]]; ok {
			if d, ok := ms[name[i+1:]]; ok {
				return d, true
			}
		}
	}
	if d, ok := ix.types[name]; ok {
		return d, true
	}
	if d, ok := ix.funcs[name]; ok {
		return d, true
	}
	if d, ok := ix.vars[name]; ok {
		return d, true
	}
	return nil, false
}

func (ix *Index) exported() []*Decl {
	var out []*Decl
	for n, d := range ix.byName {
		if ast.IsExported(n) {
			out = append(out, d)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return rank(out[i].Kind) < rank(out[j].Kind)
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// receivers returns the receiver type name of a method declaration.
func (d *Decl) receiver() string {
	if d.Kind != "method" {
		return ""
	}
	if i := strings.LastIndex(d.Name, "."); i > 0 {
		return d.Name[:i]
	}
	return ""
}

// isDotImported reports whether the file dot-imports the given path.
func (ix *Index) isDotImported(f *ast.File, path string) bool {
	set := ix.dotFiles[f]
	if set == nil {
		set = map[string]bool{}
		for _, imp := range f.Imports {
			if imp.Name == nil || imp.Name.Name != "." {
				continue
			}
			if p, err := strconv.Unquote(imp.Path.Value); err == nil {
				set[p] = true
			}
		}
		ix.dotFiles[f] = set
	}
	return set[path]
}

// ============================== ast helpers ==============================

func convertFiles(files []*ast.File) []*ast.File { return files }

func receiverName(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.StarExpr:
		return receiverName(t.X)
	case *ast.IndexExpr:
		return receiverName(t.X)
	case *ast.IndexListExpr:
		return receiverName(t.X)
	case *ast.ParenExpr:
		return receiverName(t.X)
	}
	return ""
}

func recvTypeName(t types.Type) string {
	if p, ok := t.(*types.Pointer); ok {
		t = p.Elem()
	}
	if n, ok := t.(*types.Named); ok && n.Obj() != nil {
		return n.Obj().Name()
	}
	return ""
}

func typeNames(expr ast.Expr) []string {
	var out []string
	var walk func(ast.Expr)
	walk = func(e ast.Expr) {
		switch t := e.(type) {
		case nil:
			return
		case *ast.Ident:
			out = append(out, t.Name)
		case *ast.StarExpr:
			walk(t.X)
		case *ast.IndexExpr:
			walk(t.X)
		case *ast.IndexListExpr:
			walk(t.X)
		case *ast.ParenExpr:
			walk(t.X)
		case *ast.ArrayType:
			walk(t.Elt)
		case *ast.Ellipsis:
			walk(t.Elt)
		case *ast.MapType:
			walk(t.Key)
			walk(t.Value)
		case *ast.ChanType:
			walk(t.Value)
		case *ast.FuncType:
			walkFieldList(t.Params, walk)
			walkFieldList(t.Results, walk)
		case *ast.StructType:
			walkFieldList(t.Fields, walk)
		case *ast.InterfaceType:
			walkFieldList(t.Methods, walk)
		}
	}
	walk(expr)
	return out
}

func walkFieldList(list *ast.FieldList, walk func(ast.Expr)) {
	if list == nil {
		return
	}
	for _, f := range list.List {
		walk(f.Type)
	}
}

// constBlockName returns a position independent name for an iota block.
// FIX(P0-RI-03): a leading "_" is not unique; two blocks that start with
// "_ = iota" must not share the same emission key.
func constBlockName(gd *ast.GenDecl) string {
	for _, spec := range gd.Specs {
		if vs, ok := spec.(*ast.ValueSpec); ok {
			if name := firstNonBlankName(vs.Names); name != "" {
				return name
			}
		}
	}
	return "anon"
}

func firstNonBlankName(names []*ast.Ident) string {
	for _, n := range names {
		if n != nil && n.Name != "_" {
			return n.Name
		}
	}
	return "anon"
}

// hasInit reports whether a package contains an init function. Inlining a
// package without its init body changes program behavior, so the resolver
// keeps such a package as an import unless it is the root package.
func (ix *Index) hasInit() bool {
	_, ok := ix.funcs["init"]
	return ok
}

// constWhole reports whether an iota-using const block must be copied whole.
func constWhole(gd *ast.GenDecl) bool {
	for _, spec := range gd.Specs {
		vs, ok := spec.(*ast.ValueSpec)
		if !ok {
			continue
		}
		if len(vs.Values) == 0 {
			return true
		}
		for _, v := range vs.Values {
			found := false
			ast.Inspect(v, func(n ast.Node) bool {
				if id, ok := n.(*ast.Ident); ok && id.Name == "iota" {
					found = true
				}
				return !found
			})
			if found {
				return true
			}
		}
	}
	return false
}

func rank(k string) int {
	switch k {
	case "type":
		return 0
	case "const":
		return 1
	case "var":
		return 2
	case "func":
		return 3
	case "method":
		return 4
	}
	return 5
}
