package main

import (
	"go/ast"
	"go/parser"
	"go/token"
)

// usedQualifiers parses the assembled declaration body and records identifier
// names used on the left side of a selector. This is more reliable than
// strings.Contains: it ignores comments and strings, and it accepts legal
// whitespace such as "pkg . Sel".
//
// A nil result means parsing failed; the caller falls back to the old textual
// check so a malformed body still produces a useful parse error later.
func usedQualifiers(body string) map[string]bool {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "generated_body.go", "package p\n"+body, 0)
	if err != nil {
		return nil
	}
	out := map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if id, ok := sel.X.(*ast.Ident); ok {
			out[id.Name] = true
		}
		return true
	})
	return out
}
