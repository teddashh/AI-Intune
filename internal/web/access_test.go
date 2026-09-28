package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/teddashh/AI-Intune/internal/operatorauth"
	"github.com/teddashh/AI-Intune/internal/tailnet"
)

func requestWithCapabilities(method, path string, granted operatorauth.CapabilityNames) *http.Request {
	req := httptest.NewRequest(method, path, nil)
	req.RemoteAddr = "100.100.10.20:4321"
	return operatorauth.WithPrincipal(req, operatorauth.Principal{
		SourceAddr: "100.100.10.20", NodeStableID: "node-stable-1",
		DeviceName: "operator-laptop.example.ts.net.", TailnetUserID: "42",
		TailnetUserLogin: "operator@example.com", AuthMethod: operatorauth.AuthMethodLocalAPI,
		GrantedCapabilities: granted,
	})
}

func renderWithCapabilities(t *testing.T, s *Server, path string, granted operatorauth.CapabilityNames) string {
	t.Helper()
	mux := http.NewServeMux()
	s.Routes(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, requestWithCapabilities(http.MethodGet, path, granted))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "</html>") {
		t.Fatalf("GET %s status=%d body=%s", path, rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

func TestAccessViewUsesExactCapabilitiesWithoutInheritance(t *testing.T) {
	names, err := operatorauth.NamesForPrefix("example.com/cap/clawctl")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name                 string
		granted              operatorauth.CapabilityNames
		view, operate, admin bool
	}{
		{name: "view only", granted: operatorauth.CapabilityNames{View: names.View}, view: true},
		{name: "operate only", granted: operatorauth.CapabilityNames{Operate: names.Operate}, operate: true},
		{name: "admin only", granted: operatorauth.CapabilityNames{Admin: names.Admin}, admin: true},
		{name: "all explicit", granted: names, view: true, operate: true, admin: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			access := accessFromRequest(requestWithCapabilities(http.MethodGet, "/", test.granted))
			if !access.Known || access.CanView != test.view || access.CanOperate != test.operate || access.CanAdmin != test.admin {
				t.Fatalf("access=%+v", access)
			}
		})
	}
}

func TestUIShowsVerifiedTailnetIdentityAndHidesUnauthorizedControls(t *testing.T) {
	s, st := newServer(t)
	id := onlineMachine(t, st, "access-machine")
	names, err := operatorauth.NamesForPrefix("example.com/cap/clawctl")
	if err != nil {
		t.Fatal(err)
	}

	viewHome := renderWithCapabilities(t, s, "/", operatorauth.CapabilityNames{View: names.View})
	for _, want := range []string{"Tailscale 已驗證", "operator@example.com", "view", "operator-laptop.example.ts.net."} {
		if !strings.Contains(viewHome, want) {
			t.Errorf("view top bar missing %q", want)
		}
	}
	if strings.Contains(viewHome, `action="/enrollments/preview"`) {
		t.Fatal("view-only identity was shown an enroll form")
	}
	if strings.Contains(viewHome, "開 enroll 票需要") || strings.Contains(viewHome, "不顯示表單") {
		t.Fatal("view-only page exposed an unavailable-action explanation")
	}

	viewMachine := renderWithCapabilities(t, s, "/machines/"+id, operatorauth.CapabilityNames{View: names.View})
	for _, forbidden := range []string{`action="/machines/` + id + `/channel"`, `action="/machines/` + id + `/lifecycle-preview"`} {
		if strings.Contains(viewMachine, forbidden) {
			t.Errorf("view-only machine page exposed admin form %s", forbidden)
		}
	}

	all := renderWithCapabilities(t, s, "/machines/"+id, names)
	for _, required := range []string{`action="/machines/` + id + `/channel"`, `action="/machines/` + id + `/lifecycle-preview"`} {
		if !strings.Contains(all, required) {
			t.Errorf("fully granted machine page missing form %s", required)
		}
	}

	viewLifecycle := renderWithCapabilities(t, s, "/machines/lifecycle", operatorauth.CapabilityNames{View: names.View})
	if strings.Contains(viewLifecycle, `action="/machines/`+id+`/lifecycle-preview"`) ||
		strings.Contains(viewLifecycle, "capability") {
		t.Fatal("view-only lifecycle submenu exposed unavailable control copy")
	}
	adminLifecycle := renderWithCapabilities(t, s, "/machines/lifecycle", names)
	if !strings.Contains(adminLifecycle, `action="/machines/`+id+`/lifecycle-preview"`) {
		t.Fatal("fully granted lifecycle submenu is missing canonical preview control")
	}

	s.SetTailnetStatus(tailnet.Status{
		Available: true,
		Self:      tailnet.Peer{StableID: "node-unmanaged", Hostname: "unmanaged"},
	})
	viewTailnet := renderWithCapabilities(t, s, "/settings/tailnet", operatorauth.CapabilityNames{View: names.View})
	if strings.Contains(viewTailnet, `action="/settings/tailnet/peer-ignore-preview"`) || strings.Contains(viewTailnet, "capability") {
		t.Fatal("view-only Tailnet page exposed unavailable admin control or explanation")
	}
	adminTailnet := renderWithCapabilities(t, s, "/settings/tailnet", names)
	if !strings.Contains(adminTailnet, `action="/settings/tailnet/peer-ignore-preview"`) {
		t.Fatal("fully granted Tailnet page is missing canonical preview control")
	}
}
