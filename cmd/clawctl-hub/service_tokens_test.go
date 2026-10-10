package main

import (
	"github.com/teddashh/AI-Intune/internal/totp"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/operatorauth"
	"github.com/teddashh/AI-Intune/internal/store"
)

func TestServiceTokenBoundary(t *testing.T) {
	st := boundaryStore(t)
	actor, err := st.CreateFirstAdmin("admin", "a sufficiently long password")
	if err != nil {
		t.Fatal(err)
	}
	token, secret, err := st.CreateServiceToken(store.ServiceToken{Name: "agent", Scope: "operate", ExpiresAt: time.Now().Add(time.Hour), Allowlist: []string{"GET /v1/operator/machines", "POST /v1/operator/test"}, SourceCIDRs: []string{"192.0.2.0/24"}}, actor.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	calls := 0
	handler := func(w http.ResponseWriter, r *http.Request) {
		calls++
		p, ok := operatorauth.PrincipalFromContext(r.Context())
		if !ok || p.StableSubject() != "service:agent" {
			t.Error("wrong principal")
		}
		w.WriteHeader(204)
	}
	policies := map[string]operatorRoutePolicy{}
	for _, route := range []string{"GET /v1/operator/machines", "GET /v1/operator/jobs", "POST /v1/operator/test", "POST /v1/operator/admin", "GET /account/service-tokens"} {
		policy := operatorRoutePolicy{operatorauth.View, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked}
		if strings.HasPrefix(route, "POST") {
			policy.Permission = operatorauth.Operate
		}
		if strings.Contains(route, "admin") {
			policy.Permission = operatorauth.Admin
		}
		if strings.Contains(route, "/account/") {
			policy.Representation = operatorHTML
			policy.SourceKind = operator.SourceKindWeb
			policy.Permission = operatorauth.Admin
		}
		policies[route] = policy
		mux.HandleFunc(route, handler)
	}
	human := &boundaryAuthorizer{}
	b := newOperatorBoundary(mux, human, st, policies, testOperatorAuthority)
	request := func(method, path, peer, origin string) int {
		r := newBoundaryRequest(method, path, nil)
		r.RemoteAddr = peer
		r.Header.Set("Authorization", "Bearer "+secret)
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		w := httptest.NewRecorder()
		b.ServeHTTP(w, r)
		return w.Code
	}
	for _, c := range []struct {
		method, path, peer, origin string
		status                     int
	}{{"GET", "/v1/operator/machines", "192.0.2.1:1234", "", 204}, {"GET", "/v1/operator/jobs", "192.0.2.1:1234", "", 401}, {"GET", "/v1/operator/machines", "198.51.100.1:1234", "", 401}, {"POST", "/v1/operator/admin", "192.0.2.1:1234", "", 401}, {"POST", "/v1/operator/test", "192.0.2.1:1234", "https://other.example.com", 403}, {"GET", "/account/service-tokens", "192.0.2.1:1234", "", 403}} {
		if got := request(c.method, c.path, c.peer, c.origin); got != c.status {
			t.Fatalf("%s %s: %d", c.method, c.path, got)
		}
	}
	if calls != 1 {
		t.Fatalf("unauthorized handler calls: %d", calls)
	}
	if len(human.snapshotCalls()) != 1 {
		t.Fatal("HTML did not use human auth")
	}
	audits, err := st.Audit("", 100)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range audits {
		if a.Action == "service-token-request" && a.AuthSubject == "service:agent" {
			found = true
		}
		if strings.Contains(a.Detail, secret) {
			t.Fatal("audit leaked secret")
		}
	}
	if !found {
		t.Fatal("missing service audit")
	}
	if err = st.RevokeServiceToken(token.ID, actor.AccountID); err != nil {
		t.Fatal(err)
	}
	if got := request("GET", "/v1/operator/machines", "192.0.2.1:1234", ""); got != 401 {
		t.Fatalf("revoked: %d", got)
	}
}

func TestServiceTokenFailureRateLimit(t *testing.T) {
	st := boundaryStore(t)
	mux := http.NewServeMux()
	route := "GET /v1/operator/machines"
	mux.HandleFunc(route, func(http.ResponseWriter, *http.Request) { t.Fatal("invalid token reached handler") })
	policy := operatorRoutePolicy{operatorauth.View, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked}
	b := newOperatorBoundary(mux, nil, st, map[string]operatorRoutePolicy{route: policy}, testOperatorAuthority)
	for i := 0; i < 6; i++ {
		r := newBoundaryRequest("GET", "/v1/operator/machines", nil)
		r.RemoteAddr = "192.0.2.1:1234"
		r.Header.Set("Authorization", "Bearer cst_invalid")
		w := httptest.NewRecorder()
		b.ServeHTTP(w, r)
		want := 401
		if i == 5 {
			want = 429
		}
		if w.Code != want {
			t.Fatalf("attempt %d: %d", i, w.Code)
		}
	}
}

func TestServiceTokenManagementPlaintextOnce(t *testing.T) {
	h, st := localAccountHandler(t)
	actor, err := st.CreateFirstAdmin("alice", usersPassword)
	if err != nil {
		t.Fatal(err)
	}
	if w := accountRequest(h, "GET", "/account/service-tokens", "", adminSession(t, st)); w.Code != 303 {
		t.Fatalf("unenrolled session: %d", w.Code)
	}
	factor, err := st.BeginTOTPEnrollment(actor.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	enroll, _ := totp.Code(factor, time.Now().Unix()/30-1)
	if _, err = st.ConfirmTOTP(actor.AccountID, enroll); err != nil {
		t.Fatal(err)
	}
	cookie := adminSession(t, st)
	code, _ := totp.Code(factor, time.Now().Unix()/30)
	fields := url.Values{"name": {"agent"}, "scope": {"view"}, "expires_at": {time.Now().Add(time.Hour).UTC().Format(time.RFC3339)}, "password": {usersPassword}, "code": {code}, "allowlist": {"GET /v1/operator/machines"}}
	w := accountRequest(h, "POST", "/account/service-tokens/create", fields.Encode(), cookie)
	if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("create status: %d", w.Code)
	}
	secret := regexp.MustCompile(`cst_[a-f0-9]{64}`).FindString(w.Body.String())
	if secret == "" || strings.Count(w.Body.String(), secret) != 1 {
		t.Fatal("creation did not show exactly one secret")
	}
	page := accountRequest(h, "GET", "/account/service-tokens", "", cookie)
	if page.Code != 200 || strings.Contains(page.Body.String(), secret) {
		t.Fatal("listing disclosed secret or failed")
	}
	tokens, err := st.ListServiceTokens()
	if err != nil || len(tokens) != 1 {
		t.Fatal("missing token")
	}
	// A bearer alone does not authorize token management or human security pages.
	for _, path := range []string{"/account/service-tokens", "/account/security", "/account/users"} {
		r := httptest.NewRequest("GET", "https://hub.example.com"+path, nil)
		r.Header.Set("Authorization", "Bearer "+secret)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if rec.Code != 303 {
			t.Fatalf("bearer HTML %s: %d", path, rec.Code)
		}
	}
	fresh, _ := totp.Code(factor, time.Now().Unix()/30+1)
	revoke := url.Values{"id": {tokens[0].ID}, "password": {usersPassword}, "code": {fresh}}
	w = accountRequest(h, "POST", "/account/service-tokens/revoke", revoke.Encode(), cookie)
	if w.Code != 200 {
		t.Fatalf("revoke: %d", w.Code)
	}
	if _, err = st.AuthenticateServiceToken(secret, "GET /v1/operator/machines", "192.0.2.1", "view"); err == nil {
		t.Fatal("revocation not immediate")
	}
}

func TestServiceTokenManagementRejectsAdminScope(t *testing.T) {
	h, st := localAccountHandler(t)
	actor, err := st.CreateFirstAdmin("alice", usersPassword)
	if err != nil {
		t.Fatal(err)
	}
	factor, err := st.BeginTOTPEnrollment(actor.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	enroll, _ := totp.Code(factor, time.Now().Unix()/30-1)
	if _, err = st.ConfirmTOTP(actor.AccountID, enroll); err != nil {
		t.Fatal(err)
	}
	cookie := adminSession(t, st)
	code, _ := totp.Code(factor, time.Now().Unix()/30)
	fields := url.Values{"name": {"agent"}, "scope": {"admin"}, "expires_at": {time.Now().Add(time.Hour).UTC().Format(time.RFC3339)}, "password": {usersPassword}, "code": {code}}
	w := accountRequest(h, "POST", "/account/service-tokens/create", fields.Encode(), cookie)
	if w.Code != 400 {
		t.Fatalf("admin create: %d", w.Code)
	}
	tokens, err := st.ListServiceTokens()
	if err != nil || len(tokens) != 0 {
		t.Fatal("admin service token stored")
	}
}
