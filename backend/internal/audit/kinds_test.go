package audit

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestEveryKindConstantIsKnown keeps the constants and the IsKind set equal.
func TestEveryKindConstantIsKnown(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "kinds.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, d := range file.Decls {
		g, ok := d.(*ast.GenDecl)
		if !ok || g.Tok != token.CONST {
			continue
		}
		for _, sp := range g.Specs {
			v := sp.(*ast.ValueSpec)
			k, err := strconv.Unquote(v.Values[0].(*ast.BasicLit).Value)
			if err != nil || !IsKind(k) {
				t.Errorf("constant %s = %q is not in the kind set", v.Names[0].Name, k)
			}
			n++
		}
	}
	if n != len(kinds) {
		t.Errorf("%d constants, %d kinds in the set", n, len(kinds))
	}
	for _, bad := range []string{"", "tool", "Tool_call", "tool_call ", "nope"} {
		if IsKind(bad) {
			t.Errorf("IsKind(%q)", bad)
		}
	}
}

// TestEmittersUseTheKindConstants fails when production code appends an
// audit event with a literal kind, which the API's kind filter would not
// know: an audit.Event literal's Kind, the kind argument of the helpers that
// build events and a variable named kind in a file that uses this package
// must be one of the constants.
func TestEmittersUseTheKindConstants(t *testing.T) {
	// helper name -> index of its kind argument
	helpers := map[string]int{"audit": 2, "appendAudit": 2, "revokeLocked": 3}
	err := filepath.WalkDir("..", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "storetest" || d.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		importsAudit := false
		for _, im := range file.Imports {
			importsAudit = importsAudit || strings.HasSuffix(im.Path.Value, `/internal/audit"`)
		}
		literal := func(e ast.Expr) bool {
			_, ok := e.(*ast.BasicLit)
			return ok
		}
		isEvent := func(e ast.Expr) bool {
			if s, ok := e.(*ast.SelectorExpr); ok {
				x, ok := s.X.(*ast.Ident)
				return ok && x.Name == "audit" && s.Sel.Name == "Event"
			}
			return false
		}
		checkEvent := func(c *ast.CompositeLit) {
			for _, el := range c.Elts {
				if kv, ok := el.(*ast.KeyValueExpr); ok {
					if k, ok := kv.Key.(*ast.Ident); ok && k.Name == "Kind" && literal(kv.Value) {
						t.Errorf("%s: audit event with a literal kind", fset.Position(kv.Pos()))
					}
				}
			}
		}
		ast.Inspect(file, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.CompositeLit:
				if isEvent(n.Type) {
					checkEvent(n)
				}
				if at, ok := n.Type.(*ast.ArrayType); ok && isEvent(at.Elt) {
					for _, el := range n.Elts {
						if c, ok := el.(*ast.CompositeLit); ok && c.Type == nil {
							checkEvent(c)
						}
					}
				}
			case *ast.CallExpr:
				if s, ok := n.Fun.(*ast.SelectorExpr); ok {
					if i, ok := helpers[s.Sel.Name]; ok && len(n.Args) > i && literal(n.Args[i]) {
						t.Errorf("%s: %s with a literal kind", fset.Position(n.Pos()), s.Sel.Name)
					}
				}
			case *ast.AssignStmt:
				if !importsAudit {
					break
				}
				for i, l := range n.Lhs {
					if id, ok := l.(*ast.Ident); ok && id.Name == "kind" && i < len(n.Rhs) && literal(n.Rhs[i]) {
						t.Errorf("%s: kind assigned a literal", fset.Position(n.Pos()))
					}
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
