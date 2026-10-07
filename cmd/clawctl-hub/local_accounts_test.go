package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/teddashh/AI-Intune/internal/clientip"
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
	t.Setenv("CLAWCTL_SETUP_CODE", "abcd-efgh-ijkl-mnop")
	if err := initializeSetupCode(st, &config, func(string, ...any) {}); err != nil {
		t.Fatal(err)
	}
	ui, err := web.New(st, "hub")
	if err != nil {
		t.Fatal(err)
	}
	auth, err := withSessionAuthorizer(st, authModeLocal, true, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := clientip.Parse(os.Getenv("CLAWCTL_TRUSTED_PROXIES"), os.Getenv("CLAWCTL_CLIENT_IP_HEADER"))
	if err != nil {
		t.Fatal(err)
	}
	handler, err := newHubHTTPHandler(&hub{store: st, clientIP: resolver}, ui, auth, config.public.Authority(), config)
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
	for _, path := range []string{"/setup", "/login"} {
		w := accountRequest(h, "GET", path, "", nil)
		if strings.Contains(w.Body.String(), `pattern="[a-z0-9`) {
			t.Fatal("browser blocks username normalization")
		}
		if path == "/setup" && (!strings.Contains(w.Body.String(), `name="setup_code"`) || !strings.Contains(w.Body.String(), "Find the setup code in the Hub log")) {
			t.Fatal("missing setup code field/help")
		}
	}
	body := url.Values{"username": {"  AdMiN  "}, "password": {"long admin password"}, "setup_code": {" abcd efgh-ijkl-mnop "}}.Encode()
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
	if a.Username != "admin" {
		t.Fatalf("username not normalized: %q", a.Username)
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
	cs := w.Result().Cookies()
	if len(cs) != 1 || cs[0].Name != c.Name || !cs[0].Secure || !cs[0].HttpOnly || cs[0].Path != "/" || cs[0].Domain != "" || cs[0].SameSite != c.SameSite || cs[0].MaxAge != -1 || !strings.Contains(w.Header().Get("Set-Cookie"), "Max-Age=0") {
		t.Fatalf("logout cookie: %v", w.Header())
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
		st.VerifyPassword("admin", "wrong", "192.0.2.1")
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
	if _, err = st.VerifyPassword("admin", "replacement password", "192.0.2.1"); err != nil {
		t.Fatal("lockout not cleared", err)
	}
}

func TestSetupCodeRequired(t *testing.T) {
	h, st := localAccountHandler(t)
	for _, code := range []string{"", "wrong-code"} {
		body := url.Values{"username": {"admin"}, "password": {"long admin password"}, "setup_code": {code}}.Encode()
		w := accountRequest(h, "POST", "/setup", body, nil)
		if w.Code != 401 || !strings.Contains(w.Body.String(), "Unable to create admin.") || len(w.Result().Cookies()) != 0 {
			t.Fatalf("setup: %d %s", w.Code, w.Body.String())
		}
	}
	if n, err := st.CountAccounts(); err != nil || n != 0 {
		t.Fatal(n, err)
	}
	w := accountRequest(h, "POST", "/setup", "username=admin&password=long+admin+password&setup_code=abcdefghijklmnop", nil)
	if w.Code != 303 {
		t.Fatal(w.Code, w.Body.String())
	}
	if _, err := st.LookupSession(w.Result().Cookies()[0].Value); err != nil {
		t.Fatal(err)
	}
	assertSetupCodeNotPersisted(t, st, "abcd-efgh-ijkl-mnop")
}

func assertSetupCodeNotPersisted(t *testing.T, st *store.Store, code string) {
	t.Helper()
	var seq int
	var name, path string
	if err := st.DB().QueryRow(`PRAGMA database_list`).Scan(&seq, &name, &path); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(normalizeSetupCode(code)))
	// Check all SQLite storage, including audit records and uncheckpointed WAL data.
	for _, file := range []string{path, path + "-wal"} {
		data, err := os.ReadFile(file)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		for _, secret := range [][]byte{[]byte(code), []byte(normalizeSetupCode(code)), sum[:], []byte(hex.EncodeToString(sum[:]))} {
			if bytes.Contains(data, secret) {
				t.Fatalf("setup secret persisted in %s", file)
			}
		}
	}
}

func TestInitializeSetupCode(t *testing.T) {
	st := boundaryStore(t)
	t.Setenv("CLAWCTL_PUBLIC_URL", "https://hub.example.com")
	t.Setenv("CLAWCTL_SETUP_CODE", "placeholder")
	os.Unsetenv("CLAWCTL_SETUP_CODE")
	config, err := cloudConfiguration(authModeBoth, testOperatorAuthority)
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	logger := func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) }
	if err := initializeSetupCode(st, &config, logger); err != nil {
		t.Fatal(err)
	}
	pattern := regexp.MustCompile(`^first-run setup: open https://hub.example.com/setup and enter setup code ([A-Z2-7]{4}(?:-[A-Z2-7]{4}){7}) \(valid until the first admin is created; restart the Hub to rotate it\)$`)
	if len(lines) != 1 {
		t.Fatal(lines)
	}
	matches := pattern.FindStringSubmatch(lines[0])
	if len(matches) != 2 || !config.setupCode.matches(matches[1]) {
		t.Fatal(lines)
	}
	old := config.setupCode
	if err := initializeSetupCode(st, &config, logger); err != nil {
		t.Fatal(err)
	}
	if *old == *config.setupCode {
		t.Fatal("restart did not rotate code")
	}
	assertSetupCodeNotPersisted(t, st, matches[1])
	for _, code := range []string{"", "short", "----------------"} {
		t.Setenv("CLAWCTL_SETUP_CODE", code)
		if err := initializeSetupCode(st, &config, logger); err == nil {
			t.Fatal("accepted short code")
		}
	}
	code := "operator-secret-code-1234"
	t.Setenv("CLAWCTL_SETUP_CODE", code)
	lines = nil
	if err := initializeSetupCode(st, &config, logger); err != nil {
		t.Fatal(err)
	}
	if !config.setupCode.matches(code) || len(lines) != 1 || strings.Contains(lines[0], code) || !strings.Contains(lines[0], "operator-provided setup code is required") {
		t.Fatal(lines)
	}
	assertSetupCodeNotPersisted(t, st, code)
	if _, err := st.CreateFirstAdmin("admin", "long admin password"); err != nil {
		t.Fatal(err)
	}
	lines = nil
	config.setupCode = nil
	if err := initializeSetupCode(st, &config, logger); err != nil || len(lines) != 0 || config.setupCode != nil {
		t.Fatal(err, lines)
	}
}

func TestBootstrapAdminCommand(t *testing.T) {
	st := boundaryStore(t)
	var seq int
	var name, path string
	if err := st.DB().QueryRow(`PRAGMA database_list`).Scan(&seq, &name, &path); err != nil {
		t.Fatal(err)
	}
	args := []string{"--db", path, "--username", "admin"}
	if command, err := classifyTopLevel(append([]string{"bootstrap-admin"}, args...)); err != nil || command != "bootstrap-admin" {
		t.Fatal(command, err)
	}
	for _, password := range []string{"short", strings.Repeat("x", 259)} {
		if err := runBootstrapAdmin(args, strings.NewReader(password)); err == nil {
			t.Fatal("accepted invalid password")
		}
	}
	if err := runBootstrapAdmin(args, strings.NewReader("headless admin password\r\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := st.VerifyPassword("admin", "headless admin password", "192.0.2.1"); err != nil {
		t.Fatal(err)
	}
	if err := runBootstrapAdmin(args, strings.NewReader("another admin password")); !errors.Is(err, store.ErrAdminExists) {
		t.Fatal(err)
	}
	if n, err := st.CountAccounts(); err != nil || n != 1 {
		t.Fatal(n, err)
	}
}

func TestLoginResolvedIPBuckets(t *testing.T) {
	for _, trusted := range []bool{true, false} {
		t.Run(fmt.Sprint(trusted), func(t *testing.T) {
			proxies := ""
			if trusted {
				proxies = "10.0.0.0/8"
			}
			t.Setenv("CLAWCTL_TRUSTED_PROXIES", proxies)
			h, st := localAccountHandler(t)
			if _, err := st.CreateFirstAdmin("admin", "test admin password"); err != nil {
				t.Fatal(err)
			}
			request := func(ip string) int {
				r := httptest.NewRequest("POST", "https://hub.example.com/login", strings.NewReader("username=unknown&password=wrong"))
				r.RemoteAddr = "10.0.0.1:1234"
				r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				r.Header.Set("X-Forwarded-For", ip)
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				return w.Code
			}
			for range 5 {
				if code := request("192.0.2.1"); code != 401 {
					t.Fatal(code)
				}
			}
			if code := request("192.0.2.1"); code != 429 {
				t.Fatal(code)
			}
			want := 429
			if trusted {
				want = 401
			}
			if code := request("192.0.2.2"); code != want {
				t.Fatalf("got %d want %d", code, want)
			}
		})
	}
}
