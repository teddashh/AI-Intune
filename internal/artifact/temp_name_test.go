package artifact

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"
)

func TestArtifactTempPrefixesMatchCreateTemp(t *testing.T) {
	consts := stringConstants(t)
	used := tempPrefixesFromCreateCalls(t, consts)
	if len(used) == 0 {
		t.Fatal("no temp prefixes discovered from fetcher temp creation")
	}
	tempPrefixConsts := 0
	for name, value := range consts {
		if !strings.HasSuffix(name, "TempPrefix") {
			continue
		}
		tempPrefixConsts++
		if _, ok := used[value]; !ok {
			t.Fatalf("constant %s is not used to create a temp file", name)
		}
		if !isArtifactTempName(value + "interrupted.tmp") {
			t.Fatalf("constant %s prefix %q is not recognized by isArtifactTempName", name, value)
		}
	}
	if tempPrefixConsts == 0 {
		t.Fatal("no TempPrefix constants declared")
	}
	for prefix, at := range used {
		if !isArtifactTempName(prefix + "interrupted.tmp") {
			t.Fatalf("temp prefix %q created at %s is not recognized by isArtifactTempName", prefix, at)
		}
	}
}

func stringConstants(t *testing.T) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]string{}
	fset := token.NewFileSet()
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				values := spec.(*ast.ValueSpec)
				for i, ident := range values.Names {
					if i >= len(values.Values) {
						continue
					}
					literal, ok := values.Values[i].(*ast.BasicLit)
					if !ok || literal.Kind != token.STRING {
						continue
					}
					value, err := strconv.Unquote(literal.Value)
					if err != nil {
						t.Fatal(err)
					}
					found[ident.Name] = value
				}
			}
		}
	}
	return found
}

func tempPrefixesFromCreateCalls(t *testing.T, consts map[string]string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	used := map[string]string{}
	fset := token.NewFileSet()
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil || fn.Name == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok || len(call.Args) < 2 {
					return true
				}
				callName := astCallName(call.Fun)
				if callName != "CreateTemp" && callName != "writeSyncedTemp" {
					return true
				}
				if fn.Name.Name == "writeSyncedTemp" && callName == "CreateTemp" {
					if ident, ok := call.Args[1].(*ast.Ident); ok && ident.Name == "pattern" {
						return true
					}
				}
				prefix, ok := tempPatternPrefix(call.Args[1], consts)
				if !ok {
					t.Fatalf("%s: temp pattern is not a constant prefix", fset.Position(call.Pos()))
				}
				used[prefix] = fset.Position(call.Pos()).String()
				return true
			})
		}
	}
	return used
}

func astCallName(fun ast.Expr) string {
	switch typed := fun.(type) {
	case *ast.Ident:
		return typed.Name
	case *ast.SelectorExpr:
		return typed.Sel.Name
	default:
		return ""
	}
}

func tempPatternPrefix(expr ast.Expr, consts map[string]string) (string, bool) {
	for {
		paren, ok := expr.(*ast.ParenExpr)
		if !ok {
			break
		}
		expr = paren.X
	}
	bin, ok := expr.(*ast.BinaryExpr)
	if !ok || bin.Op != token.ADD {
		return "", false
	}
	literal, ok := bin.Y.(*ast.BasicLit)
	if !ok || literal.Kind != token.STRING {
		return "", false
	}
	suffix, err := strconv.Unquote(literal.Value)
	if err != nil || suffix != "*.tmp" {
		return "", false
	}
	switch left := bin.X.(type) {
	case *ast.Ident:
		value, ok := consts[left.Name]
		return value, ok
	case *ast.BasicLit:
		if left.Kind != token.STRING {
			return "", false
		}
		value, err := strconv.Unquote(left.Value)
		if err != nil {
			return "", false
		}
		return value, true
	default:
		return "", false
	}
}
