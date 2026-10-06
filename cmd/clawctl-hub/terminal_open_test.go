package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/teddashh/AI-Intune/internal/agentlink"
	"github.com/teddashh/AI-Intune/internal/agentrelay"
	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/operatorauth"
	"github.com/teddashh/AI-Intune/internal/web"
)

type terminalOpenProbeLink struct{}

func (terminalOpenProbeLink) Send(agentrelay.Downstream) error { return nil }
func (terminalOpenProbeLink) Close(string)                     {}

func TestServeInstallsTerminalLinks(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var serve *ast.FuncDecl
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Recv == nil && fn.Name.Name == "serve" {
			serve = fn
		}
	}
	if serve == nil {
		t.Fatal("main.go has no serve function")
	}
	installed := false
	ast.Inspect(serve.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "SetTerminalLinks" || len(call.Args) != 1 {
			return true
		}
		arg, ok := call.Args[0].(*ast.SelectorExpr)
		ident, identOK := arg.X.(*ast.Ident)
		if ok && identOK && ident.Name == "h" && arg.Sel.Name == "agentLinks" {
			installed = true
		}
		return true
	})
	if !installed {
		t.Fatal("serve does not install h.agentLinks with SetTerminalLinks")
	}
}

func TestServeInstallsTerminalSessionCloser(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var serve *ast.FuncDecl
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Recv == nil && fn.Name.Name == "serve" {
			serve = fn
		}
	}
	if serve == nil {
		t.Fatal("main.go has no serve function")
	}
	installed := false
	ast.Inspect(serve.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) != 1 {
			return true
		}
		name, ok := call.Fun.(*ast.Ident)
		arg, argOK := call.Args[0].(*ast.Ident)
		if ok && argOK && name.Name == "installOperatorTerminalSessionCloser" && arg.Name == "h" {
			installed = true
		}
		return true
	})
	if !installed {
		t.Fatal("serve does not install the terminal session closer on h")
	}
}

func TestTerminalOpenRouteRejectsCrossOriginBeforeTheHandler(t *testing.T) {
	want := operatorRoutePolicy{operatorauth.Operate, operatorHTML, operator.SourceKindWeb, operatorSecurityLocked}
	if got := operatorRoutePolicies["POST /machines/{id}/terminals"]; got != want {
		t.Fatalf("policy=%+v, want %+v", got, want)
	}

	st := boundaryStore(t)
	machineMux := http.NewServeMux()
	h := &hub{store: st, agentLinks: agentlink.New()}
	h.machineAndPublicRoutes(machineMux)
	machine := enrollViaHTTP(t, machineMux, st, "terminal-open")
	if _, err := st.DB().Exec(`UPDATE machine_registry SET assigned_user_id=?, assigned_user_login=? WHERE machine_id=?`,
		"42", "operator@example.com", machine.id); err != nil {
		t.Fatal(err)
	}
	if _, err := h.agentLinks.Attach(machine.id, terminalOpenProbeLink{}); err != nil {
		t.Fatal(err)
	}
	ui, err := web.New(st, "hub")
	if err != nil {
		t.Fatal(err)
	}
	ui.SetTerminalLinks(h.agentLinks)
	authorizer := &boundaryAuthorizer{allow: true}
	handler, err := newHubHTTPHandler(h, ui, authorizer, testOperatorAuthority)
	if err != nil {
		t.Fatal(err)
	}

	form := url.Values{"session_id": {"boundary-session"}, "idempotency_key": {"boundary-key"}}
	req := newBoundaryRequest(http.MethodPost, "/machines/"+machine.id+"/terminals", strings.NewReader(form.Encode()))
	req.RemoteAddr = "100.100.10.20:4321"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	req.Header.Set("Origin", "http://evil.example")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), csrfDecisionCode) {
		t.Fatalf("cross-origin status=%d body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "沒有開啟終端") {
		t.Fatalf("cross-origin reached the open handler: %s", rec.Body.String())
	}
	calls := authorizer.snapshotCalls()
	if len(calls) != 1 || calls[0] != operatorauth.Operate {
		t.Fatalf("auth calls=%v, want one operate check", calls)
	}
	var sessions int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM agent_sessions`).Scan(&sessions); err != nil || sessions != 0 {
		t.Fatalf("cross-origin wrote %d sessions, err=%v", sessions, err)
	}
}
