package web

import (
	"github.com/teddashh/AI-Intune/internal/certificates"
	"github.com/teddashh/AI-Intune/internal/operatorauth"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestCertificatesHumanWorkflow(t *testing.T) {
	s, st := newServer(t)
	mux := http.NewServeMux()
	s.certificateRoutes(mux)
	request := func(method, path string, v url.Values, admin bool) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(v.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r = operatorauth.WithPrincipal(r, diskWebPrincipal(admin, false, ""))
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w
	}

	w := request("GET", "/tenant/certificates", nil, true)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Add the first endpoint") || !strings.Contains(w.Body.String(), "</html>") {
		t.Fatalf("empty UI: %d %s", w.Code, w.Body.String())
	}
	v := url.Values{"operation": {"save"}, "endpoint": {"example.test:443"}, "expected": {"example.test www.example.test"}, "owner": {"<script>owner</script>"}, "days": {"30 14 7"}}
	w = request("POST", "/tenant/certificates", v, true)
	if w.Code != 303 {
		t.Fatalf("save: %d %s", w.Code, w.Body.String())
	}
	items, _ := st.Certificates()
	if len(items) != 1 {
		t.Fatal(items)
	}
	m := items[0]
	w = request("GET", "/tenant/certificates?edit="+m.ID, nil, true)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Edit monitor") || strings.Contains(w.Body.String(), "<script>owner") {
		t.Fatal("edit or escaping failed")
	}
	v.Set("id", m.ID)
	v.Set("revision", strconv.FormatInt(m.Revision, 10))
	v.Set("notes", "Renew with example team")
	if w = request("POST", "/tenant/certificates", v, true); w.Code != 303 {
		t.Fatal(w.Code)
	}
	items, _ = st.Certificates()
	m = items[0]
	v = url.Values{"id": {m.ID}, "revision": {strconv.FormatInt(m.Revision, 10)}, "operation": {"probe"}}
	if w = request("POST", "/tenant/certificates", v, false); w.Code != 403 {
		t.Fatal("viewer mutated", w.Code)
	}
	if w = request("POST", "/tenant/certificates", v, true); w.Code != 303 {
		t.Fatal(w.Code)
	}
	items, _ = st.Certificates()
	v.Set("revision", strconv.FormatInt(items[0].Revision, 10))
	v.Set("operation", "delete")
	if w = request("POST", "/tenant/certificates", v, true); w.Code != 303 {
		t.Fatal(w.Code)
	}
	items, _ = st.Certificates()
	if len(items) != 0 {
		t.Fatal("delete failed")
	}

}

func TestCertificatePageEvidenceAndCapture(t *testing.T) {
	s, _ := newServer(t)
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	m := certificates.Monitor{ID: "example-monitor", Revision: 1, Config: certificates.Config{Endpoint: "example.test:443", Expected: []string{"example.test", "www.example.test"}, Owner: "Example service team", Notes: "Renew through the service issuer", Days: []int{30, 14, 7}}, Result: certificates.Result{AttemptedAt: now, ObservedAt: now, NotAfter: now.Add(6 * 24 * time.Hour), Issuer: "CN=Example Test CA", SANs: []string{"example.test"}, Gaps: []string{"www.example.test"}}}
	for _, name := range []string{"empty", "attention", "edit"} {
		v := &certificatesPage{Manage: true}
		v.Edit.Days = []int{30, 14, 7}
		if name != "empty" {
			v.Items = []certificates.Status{certificates.Evaluate(m, now)}
		}
		if name == "edit" {
			v.Edit = m
		}
		r := operatorauth.WithPrincipal(httptest.NewRequest("GET", "/tenant/certificates", nil), diskWebPrincipal(true, false, ""))
		w := httptest.NewRecorder()
		s.render(w, r, "certificates.html", page{Title: "Certificates", Nav: "tenant-certificates", Certificates: v})
		body := w.Body.String()
		if !strings.Contains(body, "</html>") || strings.Contains(body, "ZgotmplZ") {
			t.Fatal("incomplete page")
		}
		if name != "empty" && (!strings.Contains(body, "SAN gap") || !strings.Contains(body, "expires within 7 days") || !strings.Contains(body, "Ask the owner")) {
			t.Fatal("missing evidence or next action")
		}
		if dir := os.Getenv("CERTIFICATE_CAPTURE_DIR"); dir != "" {
			if err := os.MkdirAll(dir, 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, name+".html"), w.Body.Bytes(), 0644); err != nil {
				t.Fatal(err)
			}
		}
	}
}
