package main

import (
	"github.com/teddashh/AI-Intune/internal/clientip"
	"github.com/teddashh/AI-Intune/internal/operatorauth"
	"github.com/teddashh/AI-Intune/internal/store"
	"github.com/teddashh/AI-Intune/internal/totp"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestMFALogin(t *testing.T) {
	h, st := localAccountHandler(t)
	a, err := st.CreateFirstAdmin("admin", "a long test password")
	if err != nil {
		t.Fatal(err)
	}
	secret, err := st.BeginTOTPEnrollment(a.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	// Enroll using the previous skew step, leaving the current step for login.
	code, _ := totp.Code(secret, time.Now().Unix()/30-1)
	codes, err := st.ConfirmTOTP(a.AccountID, code)
	if err != nil {
		t.Fatal(err)
	}
	w := accountRequest(h, "POST", "/login/mfa", "code=123456", nil)
	if w.Code != 401 {
		t.Fatal(w.Code)
	}
	w = accountRequest(h, "POST", "/login", "username=admin&password=a+long+test+password", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Second factor") {
		t.Fatal(w.Code, w.Body.String())
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != "hub_pending_mfa" {
		t.Fatal(cookies)
	}
	pending := cookies[0]
	code, _ = totp.Code(secret, time.Now().Unix()/30)
	w = accountRequest(h, "POST", "/login/mfa", "code="+code, pending)
	if w.Code != 303 {
		t.Fatal(w.Code, w.Body.String())
	}
	w = accountRequest(h, "POST", "/login/mfa", "code="+url.QueryEscape(codes[0]), pending)
	if w.Code != 401 {
		t.Fatal("pending token reused", w.Code)
	}
}
func TestMFARecoveryLogin(t *testing.T) {
	h, st := localAccountHandler(t)
	a, err := st.CreateFirstAdmin("admin", "a long test password")
	if err != nil {
		t.Fatal(err)
	}
	secret, _ := st.BeginTOTPEnrollment(a.AccountID)
	code, _ := totp.Code(secret, time.Now().Unix()/30)
	codes, err := st.ConfirmTOTP(a.AccountID, code)
	if err != nil {
		t.Fatal(err)
	}
	w := accountRequest(h, "POST", "/login", "username=admin&password=a+long+test+password", nil)
	pending := w.Result().Cookies()[0]
	w = accountRequest(h, "POST", "/login/mfa", "code="+code, pending)
	if w.Code != 401 {
		t.Fatal("replay accepted", w.Code)
	}
	w = accountRequest(h, "POST", "/login/mfa", "code="+codes[0], pending)
	if w.Code != 303 {
		t.Fatal(w.Code, w.Body.String())
	}
	if err = st.VerifySecondFactor(a.AccountID, "192.0.2.1", codes[0]); err == nil {
		t.Fatal("recovery reused")
	}
}
func TestPendingLoginExpiryAndBinding(t *testing.T) {
	p := newPendingLogins()
	now := time.Now()
	p.now = func() time.Time { return now }
	token, err := p.add(store.HubAccount{AccountID: "id"}, "peer", "/")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := p.get(token, "other"); ok {
		t.Fatal("wrong source")
	}
	if _, ok := p.get(token, "peer"); !ok {
		t.Fatal("missing pending")
	}
	now = now.Add(5 * time.Minute)
	if _, ok := p.get(token, "peer"); ok {
		t.Fatal("expired pending accepted")
	}
}
func TestAccountSecurityEnableDisablePassword(t *testing.T) {
	t.Setenv("CLAWCTL_REQUIRE_MFA", "0")
	h, st := localAccountHandler(t)
	a, err := st.CreateFirstAdmin("admin", "a long test password")
	if err != nil {
		t.Fatal(err)
	}
	login := accountRequest(h, "POST", "/login", "username=admin&password=a+long+test+password", nil)
	session := login.Result().Cookies()[0]
	w := accountRequest(h, "GET", "/account/security", "", session)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	w = accountRequest(h, "POST", "/account/security/totp/begin", "", session)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	// Read the displayed URI, not internal DB state.
	body := w.Body.String()
	at := strings.Index(body, "secret=")
	if at < 0 {
		t.Fatal(body)
	}
	secret := body[at+7 : at+7+32]
	code, _ := totp.Code(secret, time.Now().Unix()/30)
	w = accountRequest(h, "POST", "/account/security/totp/confirm", "code="+code, session)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "save these now") {
		t.Fatal(w.Code, w.Body.String())
	}
	body = w.Body.String()
	at = strings.Index(body, "<pre>")
	recovery := body[at+5 : at+5+14]
	w = accountRequest(h, "GET", "/account/security", "", session)
	if strings.Contains(w.Body.String(), recovery) {
		t.Fatal("codes shown again")
	}
	w = accountRequest(h, "POST", "/account/security/totp/disable", "password=wrong&code="+recovery, session)
	if w.Code != 400 {
		t.Fatal(w.Code)
	}
	w = accountRequest(h, "POST", "/account/security/totp/disable", "password=a+long+test+password&code="+recovery, session)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	enabled, err := st.MFAEnabled(a.AccountID)
	if enabled || err != nil {
		t.Fatal(enabled, err)
	}
	other, err := st.CreateSession(a, "peer", "test")
	if err != nil {
		t.Fatal(err)
	}
	w = accountRequest(h, "POST", "/account/security/password", "password=a+long+test+password&new_password=another+long+password", session)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if _, err = st.LookupSession(other); err == nil {
		t.Fatal("other session survived")
	}
	if _, err = st.LookupSession(session.Value); err != nil {
		t.Fatal("current session revoked", err)
	}
}
func TestRequireMFA(t *testing.T) {
	t.Setenv("CLAWCTL_REQUIRE_MFA", "1")
	h, st := localAccountHandler(t)
	a, err := st.CreateFirstAdmin("admin", "a long test password")
	if err != nil {
		t.Fatal(err)
	}
	w := accountRequest(h, "POST", "/login", "username=admin&password=a+long+test+password", nil)
	session := w.Result().Cookies()[0]
	w = accountRequest(h, "GET", "/", "", session)
	if w.Code != 303 || w.Header().Get("Location") != "/account/security" {
		t.Fatal(w.Code, w.Header())
	}
	w = accountRequest(h, "GET", "/v1/operator/machines", "", session)
	if w.Code != 403 || !strings.Contains(w.Body.String(), "MFA_ENROLLMENT_REQUIRED") {
		t.Fatal(w.Code, w.Body.String())
	}
	w = accountRequest(h, "GET", "/account/security", "", session)
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	secret, _ := st.BeginTOTPEnrollment(a.AccountID)
	code, _ := totp.Code(secret, time.Now().Unix()/30)
	w = accountRequest(h, "POST", "/account/security/totp/confirm", "code="+code, session)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	w = accountRequest(h, "GET", "/", "", session)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
}
func TestSecurityTailscale404(t *testing.T) {
	st := boundaryStore(t)
	mux := http.NewServeMux()
	registerSecurityRoutes(mux, st, "host", clientip.Resolver{})
	for _, pattern := range securityPatterns {
		method, path, _ := strings.Cut(pattern, " ")
		r := httptest.NewRequest(method, path, nil)
		r = operatorauth.WithPrincipal(r, boundaryPrincipal(operatorauth.Admin))
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		if w.Code != 404 {
			t.Fatal(pattern, w.Code)
		}
	}
}

func TestMFABadCodesLockoutAcrossPendingLogins(t *testing.T) {
	h, st := localAccountHandler(t)
	a, err := st.CreateFirstAdmin("admin", "a long test password")
	if err != nil {
		t.Fatal(err)
	}
	secret, _ := st.BeginTOTPEnrollment(a.AccountID)
	code, _ := totp.Code(secret, time.Now().Unix()/30)
	codes, err := st.ConfirmTOTP(a.AccountID, code)
	if err != nil {
		t.Fatal(err)
	}
	request := func(peer, path, body string, cookie *http.Cookie) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "https://hub.example.com"+path, strings.NewReader(body))
		r.RemoteAddr = peer
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if cookie != nil {
			r.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	peer := "192.0.2.1:1234"
	password := "username=admin&password=a+long+test+password"
	first := request(peer, "/login", password, nil)
	second := request(peer, "/login", password, nil)
	if first.Code != 200 || second.Code != 200 {
		t.Fatal(first.Code, second.Code)
	}
	// Seed three failures to exercise the persisted lockout without exhausting
	// the separate five-request HTTP burst before the fifth bad factor.
	for range 3 {
		if err = st.VerifySecondFactor(a.AccountID, "192.0.2.1", "bad"); err == nil {
			t.Fatal("bad code accepted")
		}
	}
	for _, pending := range []*http.Cookie{first.Result().Cookies()[0], second.Result().Cookies()[0]} {
		if w := request(peer, "/login/mfa", "code=bad", pending); w.Code != 401 {
			t.Fatal(w.Code)
		}
	}
	w := request(peer, "/login/mfa", "code="+codes[0], second.Result().Cookies()[0])
	if w.Code != 401 {
		t.Fatal("locked recovery accepted", w.Code)
	}
	if _, err = st.VerifyPassword("admin", "a long test password", "192.0.2.1"); err == nil {
		t.Fatal("password cleared MFA lockout")
	}
	w = request("192.0.2.99:1234", "/login", password, nil)
	if w.Code != 200 {
		t.Fatal("other IP blocked", w.Code)
	}
	w = request("192.0.2.99:1234", "/login/mfa", "code="+codes[0], w.Result().Cookies()[0])
	if w.Code != 303 {
		t.Fatal("other IP could not complete MFA", w.Code, w.Body.String())
	}
}
func TestResetAdminPasswordDisableMFA(t *testing.T) {
	st := boundaryStore(t)
	a, err := st.CreateFirstAdmin("admin", "a long test password")
	if err != nil {
		t.Fatal(err)
	}
	secret, _ := st.BeginTOTPEnrollment(a.AccountID)
	code, _ := totp.Code(secret, time.Now().Unix()/30)
	if _, err = st.ConfirmTOTP(a.AccountID, code); err != nil {
		t.Fatal(err)
	}
	var seq int
	var name, path string
	if err = st.DB().QueryRow(`PRAGMA database_list`).Scan(&seq, &name, &path); err != nil {
		t.Fatal(err)
	}
	args := []string{"--db", path, "--username", "admin"}
	if err = runResetAdminPassword(args, strings.NewReader("a replacement password")); err != nil {
		t.Fatal(err)
	}
	if enabled, _ := st.MFAEnabled(a.AccountID); !enabled {
		t.Fatal("reset unexpectedly disabled MFA")
	}
	if err = runResetAdminPassword(append(args, "--disable-mfa"), strings.NewReader("another replacement password")); err != nil {
		t.Fatal(err)
	}
	if enabled, _ := st.MFAEnabled(a.AccountID); enabled {
		t.Fatal("host recovery preserved MFA")
	}
}

func TestPendingLoginIgnoresEphemeralPort(t *testing.T) {
	p := newPendingLogins()
	resolver := clientip.Resolver{}
	r := httptest.NewRequest("POST", "https://hub.example.com/login", nil)
	r.RemoteAddr = "192.0.2.1:1000"
	token, err := p.add(store.HubAccount{}, resolver.Resolve(r), "/")
	if err != nil {
		t.Fatal(err)
	}
	r.RemoteAddr = "192.0.2.1:2000"
	if _, ok := p.get(token, resolver.Resolve(r)); !ok {
		t.Fatal("ephemeral port broke pending login")
	}
}

func TestPendingLoginConcurrentConsumption(t *testing.T) {
	p := newPendingLogins()
	token, err := p.add(store.HubAccount{}, "192.0.2.1", "/")
	if err != nil {
		t.Fatal(err)
	}
	if p.consume(token, "192.0.2.2") {
		t.Fatal("different IP consumed challenge")
	}
	results := make(chan bool, 32)
	for range 32 {
		go func() { results <- p.consume(token, "192.0.2.1") }()
	}
	wins := 0
	for range 32 {
		if <-results {
			wins++
		}
	}
	if wins != 1 {
		t.Fatalf("consumption winners = %d", wins)
	}
	token, err = p.add(store.HubAccount{}, "192.0.2.1", "/")
	if err != nil {
		t.Fatal(err)
	}
	p.now = func() time.Time { return time.Now().Add(6 * time.Minute) }
	if p.consume(token, "192.0.2.1") {
		t.Fatal("expired challenge consumed")
	}
}

func TestConcurrentMFALoginIssuesOneSession(t *testing.T) {
	h, st := localAccountHandler(t)
	a, err := st.CreateFirstAdmin("admin", "a long test password")
	if err != nil {
		t.Fatal(err)
	}
	secret, err := st.BeginTOTPEnrollment(a.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	code, _ := totp.Code(secret, time.Now().Unix()/30)
	codes, err := st.ConfirmTOTP(a.AccountID, code)
	if err != nil {
		t.Fatal(err)
	}
	w := accountRequest(h, "POST", "/login", "username=admin&password=a+long+test+password", nil)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	pending := w.Result().Cookies()[0]
	results := make(chan *httptest.ResponseRecorder, 2)
	for _, recovery := range codes[:2] {
		go func(code string) { results <- accountRequest(h, "POST", "/login/mfa", "code="+code, pending) }(recovery)
	}
	successes := 0
	for range 2 {
		result := <-results
		if result.Code == 303 {
			successes++
		} else if result.Code != 401 {
			t.Fatal(result.Code, result.Body.String())
		}
	}
	if successes != 1 {
		t.Fatalf("successful completions = %d", successes)
	}
	var sessions int
	if err = st.DB().QueryRow(`SELECT count(*) FROM hub_sessions WHERE revoked_at IS NULL`).Scan(&sessions); err != nil || sessions != 1 {
		t.Fatal("issued sessions", sessions, err)
	}
}

func TestAccountSecurityRegenerateRecoveryCodes(t *testing.T) {
	for _, failure := range []string{"", "password", "totp", "recovery"} {
		t.Run(failure, func(t *testing.T) {
			t.Setenv("CLAWCTL_REQUIRE_MFA", "0")
			h, st := localAccountHandler(t)
			a, err := st.CreateFirstAdmin("admin", "a long test password")
			if err != nil {
				t.Fatal(err)
			}
			login := accountRequest(h, "POST", "/login", "username=admin&password=a+long+test+password", nil)
			session := login.Result().Cookies()[0]
			path := "/account/security/recovery-codes/regenerate"
			w := accountRequest(h, "GET", "/account/security", "", session)
			if strings.Contains(w.Body.String(), path) {
				t.Fatal("unenrolled form")
			}
			if failure == "" {
				w = accountRequest(h, "POST", path, "password=a+long+test+password&code=123456", session)
				if w.Code != 400 {
					t.Fatal(w.Code)
				}
			}
			secret, _ := st.BeginTOTPEnrollment(a.AccountID)
			code, _ := totp.Code(secret, time.Now().Unix()/30-1)
			old, err := st.ConfirmTOTPForSession(a.AccountID, code, session.Value)
			if err != nil {
				t.Fatal(err)
			}
			w = accountRequest(h, "GET", "/account/security", "", session)
			if !strings.Contains(w.Body.String(), path) {
				t.Fatal("missing enrolled form")
			}
			code, _ = totp.Code(secret, time.Now().Unix()/30)
			body := "password=a+long+test+password&code=" + code
			if failure == "password" {
				body = "password=wrong&code=" + code
			}
			if failure == "totp" {
				body = "password=a+long+test+password&code=bad"
			}
			if failure == "recovery" {
				body = "password=a+long+test+password&code=" + old[0]
			}
			if failure != "" {
				for range 5 {
					w = accountRequest(h, "POST", path, body, session)
					if w.Code != 400 {
						t.Fatal(w.Code)
					}
				}
				w = accountRequest(h, "POST", path, "password=a+long+test+password&code="+code, session)
				if w.Code != 400 && w.Code != 429 {
					t.Fatal("lockout bypass", w.Code)
				}
				if _, err := st.VerifyPassword("admin", "a long test password", "192.0.2.1"); err == nil {
					t.Fatal("password bypassed lockout")
				}
				var count int
				if err := st.DB().QueryRow(`SELECT count(*) FROM hub_account_recovery_codes WHERE account_id=? AND used_at IS NULL`, a.AccountID).Scan(&count); err != nil || count != 10 {
					t.Fatal("codes changed after failure", count, err)
				}
				if err := st.VerifySecondFactor(a.AccountID, "192.0.2.2", old[0]); err != nil {
					t.Fatal("old code consumed", err)
				}
				return
			}
			w = accountRequest(h, "POST", path, body, session)
			if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" {
				t.Fatal(w.Code, w.Header())
			}
			at := strings.Index(w.Body.String(), "<pre>")
			if at < 0 {
				t.Fatal("missing codes block")
			}
			fresh := strings.Fields(strings.SplitN(w.Body.String()[at+5:], "</pre>", 2)[0])
			if len(fresh) != 10 || !strings.Contains(w.Body.String(), "Previous recovery codes no longer work") {
				t.Fatal("missing codes")
			}
			for _, c := range old {
				if err := st.VerifySecondFactor(a.AccountID, "192.0.2.2", c); err == nil {
					t.Fatal("old code accepted")
				}
			}
			if err := st.VerifySecondFactor(a.AccountID, "192.0.2.3", fresh[0]); err != nil {
				t.Fatal(err)
			}
		})
	}
}
