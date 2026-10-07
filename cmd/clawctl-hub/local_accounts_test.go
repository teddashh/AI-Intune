package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/teddashh/AI-Intune/internal/localauth"
	"github.com/teddashh/AI-Intune/internal/operatorauth"
	"github.com/teddashh/AI-Intune/internal/store"
	"github.com/teddashh/AI-Intune/internal/web"
)

func localAccountHandler(t *testing.T) (http.Handler, *store.Store) {
	t.Helper()
	t.Setenv("CLAWCTL_PUBLIC_URL", "https://hub.example.com")
	config, err := cloudConfiguration(authModeLocal, "127.0.0.1:8787")
	if err != nil {
		t.Fatal(err)
	}
	st := boundaryStore(t)
	ui, err := web.New(st, "hub")
	if err != nil {
		t.Fatal(err)
	}
	auth, err := withSessionAuthorizer(st, authModeLocal, true, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := newHubHTTPHandler(&hub{store: st}, ui, auth, config.public.Authority(), config)
	if err != nil {
		t.Fatal(err)
	}
	return handler, st
}
func accountRequest(handler http.Handler, method, path, body string, cookie *http.Cookie) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "https://hub.example.com"+path, strings.NewReader(body))
	r.RemoteAddr = "192.0.2.1:1234"
	if method == "POST" {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if cookie != nil {
		r.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}
func TestLocalAccountLifecycle(t *testing.T) {
	h, st := localAccountHandler(t)
	for _, tt := range []struct {
		path     string
		status   int
		location string
	}{{"/", 303, "/setup"}, {"/v1/operator/machines", 401, ""}, {"/setup", 200, ""}, {"/login", 200, ""}} {
		w := accountRequest(h, "GET", tt.path, "", nil)
		if w.Code != tt.status || w.Header().Get("Location") != tt.location {
			t.Fatalf("%s: %d %s", tt.path, w.Code, w.Body.String())
		}
	}
	body := url.Values{"username": {"admin"}, "password": {"long admin password"}}.Encode()
	w := accountRequest(h, "POST", "/setup", body, nil)
	if w.Code != 303 {
		t.Fatalf("setup: %d %s", w.Code, w.Body.String())
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatal(cookies)
	}
	c := cookies[0]
	if c.Name != "__Host-clawctl_session" || !c.Secure || !c.HttpOnly || c.Path != "/" || c.Domain != "" || c.SameSite != http.SameSiteLaxMode {
		t.Fatalf("cookie flags: %+v", c)
	}
	for _, method := range []string{"GET", "POST"} {
		if w = accountRequest(h, method, "/setup", body, nil); w.Code != 404 {
			t.Fatal("setup stayed open", w.Code)
		}
	}
	w = accountRequest(h, "GET", "/", "", c)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Sign out") {
		t.Fatalf("dashboard: %d %s", w.Code, w.Body.String())
	}
	a, err := st.LookupSession(c.Value)
	if err != nil {
		t.Fatal(err)
	}
	authorizer, _ := localauth.New(st, true, "")
	r := httptest.NewRequest("GET", "https://hub.example.com/", nil)
	r.AddCookie(c)
	authed, decision := authorizer.Authorize(r, operatorauth.Admin)
	if authed == nil || decision.Principal.StableSubject() != "local-user:"+a.AccountID {
		t.Fatal(decision)
	}
	if w = accountRequest(h, "POST", "/logout", "", c); w.Code != 303 {
		t.Fatal(w.Code)
	}
	if _, err = st.LookupSession(c.Value); err == nil {
		t.Fatal("logout did not revoke")
	}
	w = accountRequest(h, "GET", "/", "", c)
	if w.Code != 303 || w.Header().Get("Location") != "/login?next=%2F" {
		t.Fatal(w.Code, w.Header())
	}
	// Login must reject a scheme-relative redirect and issue a new secure cookie.
	w = accountRequest(h, "POST", "/login", body+"&next=%2F%2Fevil", nil)
	if w.Code != 303 || w.Header().Get("Location") != "/" {
		t.Fatal(w.Code, w.Header())
	}
	if cs := w.Result().Cookies(); len(cs) != 1 || cs[0].Name != "__Host-clawctl_session" || !cs[0].Secure {
		t.Fatal(cs)
	}
}

func TestAccountPublicBoundary(t *testing.T) {
	h, _ := localAccountHandler(t)
	for _, tt := range []struct {
		path, host, origin string
		want               int
	}{{"/login", "evil.example", "", 421}, {"/setup", "evil.example", "", 421}, {"/login", "hub.example.com", "https://evil.example", 403}, {"/setup", "hub.example.com", "https://evil.example", 403}, {"/logout", "hub.example.com", "https://evil.example", 403}} {
		r := httptest.NewRequest("POST", "https://hub.example.com"+tt.path, strings.NewReader(""))
		r.Host = tt.host
		r.Header.Set("Origin", tt.origin)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tt.want {
			t.Fatalf("%+v: %d", tt, w.Code)
		}
	}
	// Invalid setup data is inexpensive but still spends the shared login/setup bucket.
	for i := 0; i < 6; i++ {
		w := accountRequest(h, "POST", "/setup", "username=x&password=short", nil)
		want := 401
		if i == 5 {
			want = 429
		}
		if w.Code != want {
			t.Fatalf("attempt %d: %d", i, w.Code)
		}
	}
	if w := accountRequest(h, "POST", "/login", "", nil); w.Code != 429 {
		t.Fatal("login bypassed limiter", w.Code)
	}
}

func TestAccountModePrincipalGate(t *testing.T) {
	for _, mode := range []authMode{authModeLocal, authModeBoth, authModeTailscale} {
		for _, method := range []string{operatorauth.AuthMethodLocalAccountSession, operatorauth.AuthMethodLocalAPI, "future-authenticator", "header"} {
			t.Run(string(mode)+"/"+method, func(t *testing.T) {
				r := httptest.NewRequest("GET", "http://hub/", nil)
				p := boundaryPrincipal(operatorauth.View)
				p.SourceAddr = r.RemoteAddr
				p.AuthMethod = method
				a := operatorauth.WithPrincipal(r, p)
				d := operatorauth.Decision{Allowed: true, HTTPStatus: 200, Code: operatorauth.Authorized, Principal: p}
				_, breach := validateAuthorizedResult(routingOf(r), a, d, operatorauth.View, mode)
				want := (method == operatorauth.AuthMethodLocalAccountSession && mode != authModeTailscale) || (method == operatorauth.AuthMethodLocalAPI && mode != authModeLocal)
				if (breach == "") != want {
					t.Fatalf("breach=%s want accepted=%t", breach, want)
				}
			})
		}
	}
}

func TestAccountModeComposition(t *testing.T) {
	st := boundaryStore(t)
	calls := 0
	fallback := boundaryAuthorizeFunc(func(r *http.Request, p operatorauth.Permission) (*http.Request, operatorauth.Decision) {
		calls++
		return nil, operatorauth.Decision{HTTPStatus: 401, Code: operatorauth.Unauthenticated}
	})
	for _, mode := range []authMode{authModeLocal, authModeBoth, authModeTailscale} {
		for _, cookie := range []bool{false, true} {
			calls = 0
			a, err := withSessionAuthorizer(st, mode, true, "", fallback)
			if err != nil {
				t.Fatal(err)
			}
			r := httptest.NewRequest("GET", "https://hub/", nil)
			if cookie {
				r.AddCookie(&http.Cookie{Name: "__Host-clawctl_session", Value: "invalid"})
			}
			a.Authorize(r, operatorauth.View)
			want := 0
			if mode == authModeTailscale || (mode == authModeBoth && !cookie) {
				want = 1
			}
			if calls != want {
				t.Fatalf("%s cookie=%t fallback calls=%d", mode, cookie, calls)
			}
		}
	}
}

func TestEnrollmentIPLimit(t *testing.T) {
	h, _ := localAccountHandler(t)
	for i := 0; i < 31; i++ {
		w := accountRequest(h, "POST", "/v1/enrollments", "{}", nil)
		if i < 30 && w.Code == 429 {
			t.Fatal("early limit")
		}
		if i == 30 && w.Code != 429 {
			t.Fatal("no limit", w.Code)
		}
	}
}

func TestSafeLoginNext(t *testing.T) {
	for _, tt := range []struct{ in, want string }{{"/machines?q=one", "/machines?q=one"}, {"//evil", "/"}, {"https://evil", "/"}, {"/\\evil", "/"}, {"", "/"}} {
		if got := safeLoginNext(tt.in); got != tt.want {
			t.Fatalf("%q: %q", tt.in, got)
		}
	}
}

func TestResetAdminPasswordCommand(t *testing.T) {
	st := boundaryStore(t)
	a, err := st.CreateFirstAdmin("admin", "long admin password")
	if err != nil {
		t.Fatal(err)
	}
	token, err := st.CreateSession(a, "", "")
	if err != nil {
		t.Fatal(err)
	}
	for range 5 {
		st.VerifyPassword("admin", "wrong")
	}
	// Use the existing database opened by the helper; recovery also works while
	// a process has it open, with SQLite serializing the account/session update.
	var seq int
	var name, path string
	if err = st.DB().QueryRow(`PRAGMA database_list`).Scan(&seq, &name, &path); err != nil {
		t.Fatal(err)
	}
	args := []string{"--db", path, "--username", "admin"}
	if command, err := classifyTopLevel(append([]string{"reset-admin-password"}, args...)); err != nil || command != "reset-admin-password" {
		t.Fatal(command, err)
	}
	if err = runResetAdminPassword(args, strings.NewReader("replacement password\n")); err != nil {
		t.Fatal(err)
	}
	if _, err = st.LookupSession(token); err == nil {
		t.Fatal("session survived reset")
	}
	if _, err = st.VerifyPassword("admin", "replacement password"); err != nil {
		t.Fatal("lockout not cleared", err)
	}
}
