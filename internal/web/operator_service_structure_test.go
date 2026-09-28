package web

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProductionWebHandlersReuseInjectedOperatorService(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || filepath.Ext(name) != ".go" || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok || len(call.Args) != 1 {
				return true
			}
			constructor, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, packageOK := constructor.X.(*ast.Ident)
			store, argumentOK := call.Args[0].(*ast.SelectorExpr)
			if !argumentOK {
				return true
			}
			receiver, receiverOK := store.X.(*ast.Ident)
			if packageOK && receiverOK && pkg.Name == "operator" && constructor.Sel.Name == "New" &&
				receiver.Name == "s" && store.Sel.Name == "store" {
				t.Errorf("%s creates a temporary operator service; production Web handlers must reuse s.operator",
					fset.Position(call.Pos()))
			}
			return true
		})
	}
}

func TestEnrollmentLimitWebAuditUsesInjectedOperatorService(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "enrollment_limit.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	foundOperatorBoundary := false
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		method, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		receiver, ok := method.X.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		server, ok := receiver.X.(*ast.Ident)
		if !ok || server.Name != "s" {
			return true
		}
		if receiver.Sel.Name == "store" && method.Sel.Name == "RecordAudit" {
			t.Errorf("%s writes enrollment-limit audit through s.store; use the injected operator service",
				fset.Position(call.Pos()))
		}
		if receiver.Sel.Name == "operator" && method.Sel.Name == "RecordEnrollmentLimitTransportRejection" {
			foundOperatorBoundary = true
		}
		return true
	})
	if !foundOperatorBoundary {
		t.Fatal("enrollment-limit Web handler does not cross the injected operator audit boundary")
	}
}

func TestRetentionWebAuditUsesInjectedOperatorService(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "actions.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	foundOperatorBoundary := false
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		method, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		receiver, ok := method.X.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		server, ok := receiver.X.(*ast.Ident)
		if !ok || server.Name != "s" {
			return true
		}
		if receiver.Sel.Name == "operator" && method.Sel.Name == "RecordRetentionTransportRejection" {
			foundOperatorBoundary = true
		}
		return true
	})
	if !foundOperatorBoundary {
		t.Fatal("retention Web handler does not cross the injected operator audit boundary")
	}
}

func TestDeploymentWebAuditUsesInjectedOperatorService(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "deployment_actions.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	foundOperatorBoundary := false
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		method, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		receiver, ok := method.X.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		server, ok := receiver.X.(*ast.Ident)
		if !ok || server.Name != "s" {
			return true
		}
		if receiver.Sel.Name == "operator" && method.Sel.Name == "RecordDeploymentTransportRejection" {
			foundOperatorBoundary = true
		}
		return true
	})
	if !foundOperatorBoundary {
		t.Fatal("deployment Web handler does not cross the injected operator audit boundary")
	}
}

func TestConnectWebAuditUsesInjectedOperatorService(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "actions.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	foundOperatorBoundary := false
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		method, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		receiver, ok := method.X.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		server, ok := receiver.X.(*ast.Ident)
		if !ok || server.Name != "s" {
			return true
		}
		if receiver.Sel.Name == "operator" && method.Sel.Name == "RecordMachineConnectAudit" {
			foundOperatorBoundary = true
		}
		return true
	})
	if !foundOperatorBoundary {
		t.Fatal("Connect Web handler does not cross the injected operator audit boundary")
	}
}

func TestConnectWebClassifiesEveryCanonicalAuditOutcome(t *testing.T) {
	want := map[string]int{
		"MachineConnectAuditDetailUnavailable":     1,
		"MachineConnectAuditProjectionUnavailable": 1,
		"MachineConnectAuditAddressUnavailable":    1,
		"MachineConnectAuditRedirected":            1,
	}
	found := make(map[string]int)
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "actions.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	ast.Inspect(file, func(node ast.Node) bool {
		selector, ok := node.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		pkg, ok := selector.X.(*ast.Ident)
		if ok && pkg.Name == "operator" {
			if _, audited := want[selector.Sel.Name]; audited {
				found[selector.Sel.Name]++
			}
		}
		return true
	})
	for outcome, count := range want {
		if found[outcome] != count {
			t.Errorf("actions.go uses %s %d times, want %d", outcome, found[outcome], count)
		}
	}
}

func TestProductionWebDirectAuditWritesMatchMeasuredDebt(t *testing.T) {
	want := map[string]int{}
	found := make(map[string]int)
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || filepath.Ext(name) != ".go" || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Body == nil {
				continue
			}
			ast.Inspect(function.Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				method, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || method.Sel.Name != "RecordAudit" {
					return true
				}
				receiver, ok := method.X.(*ast.SelectorExpr)
				if !ok || receiver.Sel.Name != "store" {
					return true
				}
				server, ok := receiver.X.(*ast.Ident)
				if !ok || server.Name != "s" {
					return true
				}
				key := name + ":" + function.Name.Name
				found[key]++
				if _, measured := want[key]; !measured {
					t.Errorf("%s has an unowned direct Web audit write", fset.Position(call.Pos()))
				}
				return true
			})
		}
	}
	for key, count := range want {
		if found[key] != count {
			t.Errorf("direct Web audit debt %s count=%d, want %d", key, found[key], count)
		}
	}
}
