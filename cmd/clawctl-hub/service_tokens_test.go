package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/clientip"
	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/operatorauth"
	"github.com/teddashh/AI-Intune/internal/store"
	"github.com/teddashh/AI-Intune/internal/totp"
	"github.com/teddashh/AI-Intune/internal/web"
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
		policy := operatorRoutePolicy{operatorauth.View, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked, true}
		if strings.HasPrefix(route, "POST") {
			policy.Permission = operatorauth.Operate
		}
		if strings.Contains(route, "admin") {
			policy.ServiceTokenEligible = false
			policy.Permission = operatorauth.Admin
		}
		if strings.Contains(route, "/account/") {
			policy.ServiceTokenEligible = false
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
	policy := operatorRoutePolicy{operatorauth.View, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked, true}
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

func TestServiceTokenManifestEligibility(t *testing.T) {
	count := 0
	for route, policy := range operatorRoutePolicies {
		if !policy.ServiceTokenEligible {
			continue
		}
		count++
		if policy.Permission != operatorauth.View || policy.Representation != operatorJSON || policy.SourceKind != operator.SourceKindOperatorAPI || !strings.HasPrefix(route, "GET /v1/operator/") {
			t.Errorf("human-approval or non-read route eligible: %s", route)
		}
	}
	if count == 0 {
		t.Fatal("no read routes eligible")
	}
	// Leaving eligibility out of a new route must not confer token authority.
	future := operatorRoutePolicy{Permission: operatorauth.View, Representation: operatorJSON, SourceKind: operator.SourceKindOperatorAPI, SecurityProfile: operatorSecurityLocked}
	if future.ServiceTokenEligible {
		t.Fatal("new route eligible by default")
	}
	st := boundaryStore(t)
	actor, err := st.CreateFirstAdmin("admin", usersPassword)
	if err != nil {
		t.Fatal(err)
	}
	_, secret, err := st.CreateServiceToken(store.ServiceToken{Name: "agent", Scope: "operate", ExpiresAt: time.Now().Add(time.Hour), Allowlist: []string{"GET /v1/operator/future"}}, actor.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/operator/future", func(http.ResponseWriter, *http.Request) { t.Fatal("new route reached") })
	b := newOperatorBoundary(mux, nil, st, map[string]operatorRoutePolicy{"GET /v1/operator/future": future}, testOperatorAuthority)
	req := newBoundaryRequest("GET", "/v1/operator/future", nil)
	req.Header.Set("Authorization", "Bearer "+secret)
	rec := httptest.NewRecorder()
	b.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatalf("future route: %d", rec.Code)
	}
}

func TestServiceTokenCannotSelfApproveExistingRoutes(t *testing.T) {
	st := boundaryStore(t)
	actor, err := st.CreateFirstAdmin("admin", usersPassword)
	if err != nil {
		t.Fatal(err)
	}
	var allowlist []string
	mux := http.NewServeMux()
	calls := 0
	for route, policy := range operatorRoutePolicies {
		if policy.Representation == operatorJSON {
			allowlist = append(allowlist, route)
			mux.HandleFunc(route, func(http.ResponseWriter, *http.Request) { calls++ })
		}
	}
	_, secret, err := st.CreateServiceToken(store.ServiceToken{Name: "legacy", Scope: "operate", ExpiresAt: time.Now().Add(time.Hour), Allowlist: allowlist}, actor.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	b := newOperatorBoundary(mux, nil, st, operatorRoutePolicies, testOperatorAuthority)
	for route, policy := range operatorRoutePolicies {
		if policy.Representation != operatorJSON || policy.ServiceTokenEligible {
			continue
		}
		method, path, _ := strings.Cut(route, " ")
		path = regexp.MustCompile(`\{[^}]+\}`).ReplaceAllString(path, "test")
		req := newBoundaryRequest(method, path, nil)
		req.Header.Set("Authorization", "Bearer "+secret)
		// Independent request cases should not share the failure limiter budget.
		b.serviceAttempts = newIPLimiter(10, 5)
		rec := httptest.NewRecorder()
		b.ServeHTTP(rec, req)
		if rec.Code != 401 {
			t.Errorf("%s: %d", route, rec.Code)
		}
	}
	if calls != 0 {
		t.Fatalf("human-only routes called %d times", calls)
	}
}

func TestServiceTokenTrustedProxySource(t *testing.T) {
	st := boundaryStore(t)
	actor, err := st.CreateFirstAdmin("admin", usersPassword)
	if err != nil {
		t.Fatal(err)
	}
	_, secret, err := st.CreateServiceToken(store.ServiceToken{Name: "agent", Scope: "view", ExpiresAt: time.Now().Add(time.Hour), Allowlist: []string{"GET /v1/operator/machines"}, SourceCIDRs: []string{"192.0.2.0/24"}}, actor.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := clientip.Parse("203.0.113.0/24", "Fly-Client-IP")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/operator/machines", func(w http.ResponseWriter, r *http.Request) {
		p, _ := operatorauth.PrincipalFromContext(r.Context())
		if p.SourceAddr != "192.0.2.1" {
			t.Error("principal uses proxy address")
		}
		w.WriteHeader(204)
	})
	policy := operatorRoutePolicies["GET /v1/operator/machines"]
	b := newOperatorBoundary(mux, nil, st, map[string]operatorRoutePolicy{"GET /v1/operator/machines": policy}, testOperatorAuthority)
	b.clientIP = resolver
	request := func(peer, forwarded, auth string) int {
		req := newBoundaryRequest("GET", "/v1/operator/machines", nil)
		req.RemoteAddr = peer
		req.Header.Set("Fly-Client-IP", forwarded)
		req.Header.Set("Authorization", "Bearer "+auth)
		rec := httptest.NewRecorder()
		b.ServeHTTP(rec, req)
		return rec.Code
	}
	if got := request("203.0.113.1:1234", "192.0.2.1", secret); got != 204 {
		t.Fatalf("trusted proxy: %d", got)
	}
	if got := request("198.51.100.2:1234", "192.0.2.1", secret); got != 401 {
		t.Fatalf("spoofed header accepted: %d", got)
	}
	if got := request("203.0.113.1:1234", "198.51.100.3", secret); got != 401 {
		t.Fatalf("trusted proxy CIDR bypass: %d", got)
	}
	for i := 0; i < 6; i++ {
		want := 401
		if i == 5 {
			want = 429
		}
		if got := request("203.0.113.1:1234", "198.51.100.1", "cst_invalid"); got != want {
			t.Fatalf("client failure %d: %d", i, got)
		}
	}
	if got := request("203.0.113.1:1234", "192.0.2.1", secret); got != 204 {
		t.Fatalf("proxy-shared limiter: %d", got)
	}
	audits, err := st.Audit("", 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range audits {
		if a.AuthMethod == operatorauth.AuthMethodServiceToken && strings.HasPrefix(a.SourceAddr, "203.0.113.") {
			t.Fatal("audit recorded proxy")
		}
	}
	// Exercise the production constructor wiring, rather than only the fixture field.
	ui, err := web.New(st, "hub")
	if err != nil {
		t.Fatal(err)
	}
	h, err := newHubHTTPHandler(&hub{store: st, clientIP: resolver}, ui, nil, testOperatorAuthority)
	if err != nil {
		t.Fatal(err)
	}
	req := newBoundaryRequest("GET", "/v1/operator/machines", nil)
	req.RemoteAddr = "203.0.113.1:1234"
	req.Header.Set("Fly-Client-IP", "192.0.2.1")
	req.Header.Set("Authorization", "Bearer "+secret)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("configured resolver not injected: %d", rec.Code)
	}
}

func TestServiceTokenAuditUsesFinalStatusOnce(t *testing.T) {
	for _, tc := range []struct {
		name           string
		handler        http.HandlerFunc
		method, origin string
		ok             bool
	}{
		{"implicit success", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok")) }, "GET", "", true},
		{"empty success", func(http.ResponseWriter, *http.Request) {}, "GET", "", true},
		{"informational then failure", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(103); w.WriteHeader(500) }, "GET", "", false},
		{"first final wins", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204); w.WriteHeader(500) }, "GET", "", true},
		{"handler denial", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(400) }, "GET", "", false},
		{"handler error", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }, "GET", "", false},
		{"csrf denial", func(http.ResponseWriter, *http.Request) { t.Fatal("CSRF handler reached") }, "POST", "https://other.example.com", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := boundaryStore(t)
			actor, err := st.CreateFirstAdmin("admin", usersPassword)
			if err != nil {
				t.Fatal(err)
			}
			route := tc.method + " /v1/operator/test"
			_, secret, err := st.CreateServiceToken(store.ServiceToken{Name: "agent", Scope: "operate", ExpiresAt: time.Now().Add(time.Hour), Allowlist: []string{route}}, actor.AccountID)
			if err != nil {
				t.Fatal(err)
			}
			mux := http.NewServeMux()
			mux.HandleFunc(route, tc.handler)
			// A synthetic explicitly automated mutation exercises the CSRF wrapper; no
			// existing production mutation is eligible.
			permission := operatorauth.View
			if tc.method == "POST" {
				permission = operatorauth.Operate
			}
			policy := operatorRoutePolicy{permission, operatorJSON, operator.SourceKindOperatorAPI, operatorSecurityLocked, true}
			b := newOperatorBoundary(mux, nil, st, map[string]operatorRoutePolicy{route: policy}, testOperatorAuthority)
			req := newBoundaryRequest(tc.method, "/v1/operator/test", nil)
			req.Header.Set("Authorization", "Bearer "+secret)
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			rec := httptest.NewRecorder()
			b.ServeHTTP(rec, req)
			audits, err := st.Audit("", 100)
			if err != nil {
				t.Fatal(err)
			}
			count := 0
			for _, a := range audits {
				if a.AuthMethod == operatorauth.AuthMethodServiceToken {
					count++
					if a.AuthSubject != "service:agent" || a.OK != tc.ok {
						t.Errorf("wrong service audit outcome: %s %v", a.AuthSubject, a.OK)
					}
				}
			}
			if count != 1 {
				t.Fatalf("service request audited %d times", count)
			}
		})
	}
}

func TestServiceTokenManagementRejectsIneligibleAllowlist(t *testing.T) {
	for _, allow := range []string{"", "POST /v1/operator/deployments/{id}/continuations", "POST /v1/operator/machines/{id}/diagnostic-noop-jobs", "GET /v1/operator/not-registered", "GET  /v1/operator/machines"} {
		t.Run(allow, func(t *testing.T) {
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
			fields := url.Values{"name": {"agent"}, "scope": {"operate"}, "expires_at": {time.Now().Add(time.Hour).UTC().Format(time.RFC3339)}, "password": {usersPassword}, "code": {code}, "allowlist": {allow}}
			rec := accountRequest(h, "POST", "/account/service-tokens/create", fields.Encode(), cookie)
			if rec.Code != 400 {
				t.Fatalf("create ineligible allowlist: %d", rec.Code)
			}
			tokens, err := st.ListServiceTokens()
			if err != nil || len(tokens) != 0 {
				t.Fatal("ineligible service token created")
			}
		})
	}
}

func TestServiceTokenRateLimitedAuditWritesAreBounded(t *testing.T) {
	st := boundaryStore(t)
	mux := http.NewServeMux()
	route := "GET /v1/operator/machines"
	mux.HandleFunc(route, func(http.ResponseWriter, *http.Request) { t.Fatal("invalid token reached handler") })
	b := newOperatorBoundary(mux, nil, st, map[string]operatorRoutePolicy{route: operatorRoutePolicies[route]}, testOperatorAuthority)
	for i := 0; i < denialAuditBurst+20; i++ {
		req := newBoundaryRequest("GET", "/v1/operator/machines", nil)
		req.Header.Set("Authorization", "Bearer cst_invalid")
		rec := httptest.NewRecorder()
		b.ServeHTTP(rec, req)
		if i >= 5 && rec.Code != 429 {
			t.Fatalf("rate limit: %d", rec.Code)
		}
	}
	audits, err := st.Audit("", 100)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, a := range audits {
		if a.Action == store.AuditOperatorDenied {
			count++
		}
	}
	if count != denialAuditBurst {
		t.Fatalf("denial writes %d, want bounded burst %d", count, denialAuditBurst)
	}
}

func TestServiceTokenPanicAuditIsFailure(t *testing.T) {
	st := boundaryStore(t)
	actor, err := st.CreateFirstAdmin("admin", usersPassword)
	if err != nil {
		t.Fatal(err)
	}
	route := "GET /v1/operator/machines"
	_, secret, err := st.CreateServiceToken(store.ServiceToken{Name: "agent", Scope: "view", ExpiresAt: time.Now().Add(time.Hour), Allowlist: []string{route}}, actor.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc(route, func(http.ResponseWriter, *http.Request) { panic("test handler failure") })
	b := newOperatorBoundary(mux, nil, st, map[string]operatorRoutePolicy{route: operatorRoutePolicies[route]}, testOperatorAuthority)
	req := newBoundaryRequest("GET", "/v1/operator/machines", nil)
	req.Header.Set("Authorization", "Bearer "+secret)
	func() {
		defer func() {
			if recover() == nil {
				t.Error("handler panic was swallowed")
			}
		}()
		b.ServeHTTP(httptest.NewRecorder(), req)
	}()
	audits, err := st.Audit("", 100)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, a := range audits {
		if a.AuthMethod == operatorauth.AuthMethodServiceToken {
			count++
			if a.OK || !strings.Contains(a.Detail, "500") {
				t.Error("panic audited as success")
			}
		}
	}
	if count != 1 {
		t.Fatalf("panic audit count: %d", count)
	}
}
