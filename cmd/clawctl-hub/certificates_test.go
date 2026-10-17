package main

import (
	"context"
	"encoding/json"
	"github.com/teddashh/AI-Intune/internal/certificates"
	"github.com/teddashh/AI-Intune/internal/operatorauth"
	"github.com/teddashh/AI-Intune/internal/web"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCertificateAPIAndBoundary(t *testing.T) {
	st := boundaryStore(t)
	h := &hub{store: st}
	ui, err := web.New(st, "")
	if err != nil {
		t.Fatal(err)
	}
	auth := &boundaryAuthorizer{allow: true}
	handler, err := newHubHTTPHandler(h, ui, auth, testOperatorAuthority)
	if err != nil {
		t.Fatal(err)
	}
	call := func(method, path, body string, origin string) *httptest.ResponseRecorder {
		t.Helper()
		r := newBoundaryRequest(method, path, strings.NewReader(body))
		r.RemoteAddr = boundaryPrincipal(operatorauth.View).SourceAddr + ":1234"
		r.Header.Set("Content-Type", "application/json")
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	w := call("POST", "/v1/operator/certificates", `{"endpoint":"example.test:443","expected":["example.test"],"owner":"Example team"}`, "")
	if w.Code != 200 {
		t.Fatalf("create %d %s", w.Code, w.Body.String())
	}
	items, _ := st.Certificates()
	if len(items) != 1 {
		t.Fatal(items)
	}
	id := items[0].ID
	w = call("GET", "/v1/operator/certificates/"+id, "", "")
	var out certificates.Status
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil || out.ID != id || len(out.Days) != 3 {
		t.Fatal(w.Code, w.Body.String())
	}
	for _, tc := range []struct {
		method, path, body string
		code               int
	}{
		{"PUT", "/v1/operator/certificates/" + id, `{"revision":1,"endpoint":"example.test:8443","expected":["www.example.test"],"days":[21,7]}`, 200},
		{"PUT", "/v1/operator/certificates/" + id, `{"revision":1,"endpoint":"example.test:443","expected":["example.test"]}`, 400},
		{"POST", "/v1/operator/certificates/" + id + "/probe", `{"revision":2}`, 200},
		{"POST", "/v1/operator/certificates", `{"endpoint":"user:password@example.test:443","expected":["example.test"]}`, 400},
		{"POST", "/v1/operator/certificates", `{"endpoint":"example.test:443","expected":["example.test"],"unknown":true}`, 400},
		{"GET", "/v1/operator/certificates?extra=1", "", 400},
	} {
		w = call(tc.method, tc.path, tc.body, "")
		if w.Code != tc.code {
			t.Fatalf("%s: %d %s", tc.path, w.Code, w.Body.String())
		}
	}
	w = call("DELETE", "/v1/operator/certificates/"+id, `{"revision":3}`, "https://other.example.test")
	if w.Code != 403 {
		t.Fatal("cross-origin delete accepted", w.Code)
	}
	if w = call("DELETE", "/v1/operator/certificates/"+id, `{"revision":3}`, ""); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w = call("GET", "/v1/operator/certificates/"+id, "", ""); w.Code != 404 {
		t.Fatal(w.Code)
	}
	audits, err := st.Audit("", 100)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, a := range audits {
		if strings.HasPrefix(string(a.Action), "certificate_") {
			n++
			if a.AuthSubject == "" || !a.OK {
				t.Fatal("missing actor or outcome")
			}
		}
	}
	if n != 4 {
		t.Fatalf("mutation audits: %d", n)
	}
	perms := auth.snapshotCalls()
	foundAdmin := false
	for _, p := range perms {
		if p == operatorauth.Admin {
			foundAdmin = true
		}
	}
	if !foundAdmin {
		t.Fatal("admin not required")
	}
	auth.mu.Lock()
	auth.allow = false
	auth.mu.Unlock()
	if w = call("GET", "/v1/operator/certificates", "", ""); w.Code != 403 {
		t.Fatal("unauthorized read accepted")
	}
}
func TestCertificateNotificationDedupAndThresholdChange(t *testing.T) {
	st := boundaryStore(t)
	h := &hub{store: st}
	c := certificates.Config{Endpoint: "example.test:443", Expected: []string{"example.test"}, Notes: "private operator note"}
	_, err := st.SaveCertificate("", 0, c)
	if err != nil {
		t.Fatal(err)
	}
	items, _ := st.Certificates()
	m := items[0]
	now := time.Now()
	m.Result = certificates.Result{ObservedAt: now, NotAfter: now.Add(13 * 24 * time.Hour)}
	status := certificates.Evaluate(m, now)
	h.notifyCertificate(status)
	h.notifyCertificate(status)
	kind := certificateNotifyKind(status)
	stats, err := st.NotifyKindStats(kind)
	if err != nil || stats.ConsecutiveFailures != 1 {
		t.Fatalf("stats %+v %v", stats, err)
	}
	m.Result.NotAfter = now.Add(6 * 24 * time.Hour)
	status = certificates.Evaluate(m, now)
	h.notifyCertificate(status)
	kind = certificateNotifyKind(status)
	stats, err = st.NotifyKindStats(kind)
	if err != nil || stats.ConsecutiveFailures != 1 {
		t.Fatalf("new threshold %+v %v", stats, err)
	}
}

func TestCertificatePeriodicProbeRespectsInterval(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("unexpected HTTP payload") }))
	defer server.Close()
	st := boundaryStore(t)
	h := &hub{store: st}
	_, err := st.SaveCertificate("", 0, certificates.Config{Endpoint: strings.TrimPrefix(server.URL, "https://"), Expected: []string{"example.test"}})
	if err != nil {
		t.Fatal(err)
	}
	h.probeCertificates(context.Background())
	items, err := st.Certificates()
	if err != nil || len(items) != 1 {
		t.Fatal(err)
	}
	first := items[0]
	if first.Result.ObservedAt.IsZero() || first.Result.VerificationError == "" || len(first.Result.Gaps) != 1 {
		t.Fatalf("missing probe evidence: %+v", first.Result)
	}
	h.probeCertificates(context.Background())
	items, _ = st.Certificates()
	if !items[0].Result.AttemptedAt.Equal(first.Result.AttemptedAt) {
		t.Fatal("probed again before interval")
	}
	if err = st.RequestCertificateProbe(first.ID, first.Revision); err != nil {
		t.Fatal(err)
	}
	h.probeCertificates(context.Background())
	items, _ = st.Certificates()
	if !items[0].Result.AttemptedAt.After(first.Result.AttemptedAt) {
		t.Fatal("manual probe did not run")
	}
}
