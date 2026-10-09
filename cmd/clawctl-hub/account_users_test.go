package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/clientip"
	"github.com/teddashh/AI-Intune/internal/operatorauth"
	"github.com/teddashh/AI-Intune/internal/store"
	"github.com/teddashh/AI-Intune/internal/totp"
)

const usersPassword = "a long test password"

func userRequest(h http.Handler, action string, fields url.Values, cookie *http.Cookie) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "https://hub.example.com/account/users/"+action, strings.NewReader(fields.Encode()))
	r.RemoteAddr = "192.0.2.2:1234"
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.AddCookie(cookie)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func adminSession(t *testing.T, st *store.Store) *http.Cookie {
	t.Helper()
	a, err := st.VerifyPassword("alice", usersPassword, "192.0.2.1")
	if err != nil {
		t.Fatal(err)
	}
	token, err := st.CreateSession(a, "192.0.2.1", "")
	if err != nil {
		t.Fatal(err)
	}
	return &http.Cookie{Name: "__Host-clawctl_session", Value: token}
}
func TestUsersUIAndEmailLogin(t *testing.T) {
	t.Setenv("CLAWCTL_REQUIRE_MFA", "0")
	h, st := localAccountHandler(t)
	alice, err := st.CreateFirstAdmin("alice", usersPassword)
	if err != nil {
		t.Fatal(err)
	}
	cookie := adminSession(t, st)
	fields := url.Values{"username": {"bob"}, "email": {"BOB@Example.com"}, "initial_password": {usersPassword}, "password": {usersPassword}}
	if w := userRequest(h, "create", fields, cookie); w.Code != 303 {
		t.Fatal(w.Code, w.Body.String())
	}
	fields.Set("username", "carol")
	if w := userRequest(h, "create", fields, cookie); w.Code != 400 {
		t.Fatal("duplicate email", w.Code)
	}
	fields.Set("email", "invalid")
	if w := userRequest(h, "create", fields, cookie); w.Code != 400 {
		t.Fatal("invalid email", w.Code)
	}
	fields.Set("username", "alice")
	if w := userRequest(h, "disable", fields, cookie); w.Code != 400 {
		t.Fatal("disabled self", w.Code)
	}
	page := accountRequest(h, "GET", "/account/users", "", cookie)
	if page.Code != 200 || !strings.Contains(page.Body.String(), "bob@example.com") || strings.Contains(page.Body.String(), "argon2") || page.Header().Get("Cache-Control") != "no-store" {
		t.Fatal(page.Code, page.Body.String())
	}
	// Login uses a separate rate bucket and identical unknown/wrong responses.
	var failure string
	for _, identifier := range []string{"unknown", "unknown@example.com", "bob"} {
		w := accountRequest(h, "POST", "/login", url.Values{"username": {identifier}, "password": {"wrong password"}}.Encode(), nil)
		if w.Code != 401 {
			t.Fatal(w.Code)
		}
		if failure == "" {
			failure = w.Body.String()
		} else if failure != w.Body.String() {
			t.Fatal("different failure response")
		}
	}
	for _, identifier := range []string{"bob", " BoB@EXAMPLE.COM "} {
		w := accountRequest(h, "POST", "/login", url.Values{"username": {identifier}, "password": {usersPassword}}.Encode(), nil)
		if w.Code != 303 {
			t.Fatal(identifier, w.Code, w.Body.String())
		}
	}
	// Cross-origin mutation is rejected by the operator boundary.
	r := httptest.NewRequest("POST", "https://hub.example.com/account/users/enable", strings.NewReader(fields.Encode()))
	r.AddCookie(cookie)
	r.Header.Set("Origin", "https://other.example.com")
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("csrf", w.Code)
	}
	if err = st.MutateHubUser("disabled", "bob", "", "", alice.AccountID); err != nil {
		t.Fatal(err)
	}
	w = accountRequest(h, "POST", "/login", "username=bob&password=a+long+test+password", nil)
	// Prior five login attempts exhaust the route bucket; store verifies disabled below.
	if w.Code != 429 && (w.Code != 401 || w.Body.String() != failure) {
		t.Fatal(w.Code, w.Body.String())
	}
	if _, err = st.VerifyPassword("bob", usersPassword, "192.0.2.3"); err == nil {
		t.Fatal("disabled login")
	}
}
func TestUsersReauthentication(t *testing.T) {
	for _, mode := range []string{"password", "totp"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("CLAWCTL_REQUIRE_MFA", "0")
			h, st := localAccountHandler(t)
			alice, err := st.CreateFirstAdmin("alice", usersPassword)
			if err != nil {
				t.Fatal(err)
			}
			secret := ""
			code := ""
			if mode == "totp" {
				secret, err = st.BeginTOTPEnrollment(alice.AccountID)
				if err != nil {
					t.Fatal(err)
				}
				code, _ = totp.Code(secret, time.Now().Unix()/30)
				if _, err = st.ConfirmTOTP(alice.AccountID, code); err != nil {
					t.Fatal(err)
				}
			}
			cookie := adminSession(t, st)
			fields := url.Values{"username": {"bob"}, "initial_password": {usersPassword}, "email": {"bob@example.com"}}
			for _, password := range []string{"", "wrong password"} {
				fields.Set("password", password)
				w := userRequest(h, "create", fields, cookie)
				if w.Code != 400 {
					t.Fatal(w.Code)
				}
			}
			fields.Set("password", usersPassword)
			if mode == "totp" {
				for _, bad := range []string{"", "000000", code} {
					fields.Set("code", bad)
					w := userRequest(h, "create", fields, cookie)
					if w.Code != 400 {
						t.Fatal("factor accepted", bad, w.Code)
					}
				}
				// Use a fresh IP bucket after the five failures; advance factor state by one step.
				code, _ = totp.Code(secret, time.Now().Unix()/30+1)
				fields.Set("code", code)
				r := httptest.NewRequest("POST", "https://hub.example.com/account/users/create", strings.NewReader(fields.Encode()))
				r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				r.RemoteAddr = "192.0.2.3:1234"
				r.AddCookie(cookie)
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				if w.Code != 303 {
					t.Fatal(w.Code, w.Body.String())
				}
			} else {
				if w := userRequest(h, "create", fields, cookie); w.Code != 303 {
					t.Fatal(w.Code, w.Body.String())
				}
			}
			users, err := st.ListHubUsers()
			if err != nil || len(users) != 2 {
				t.Fatal(users, err)
			}
		})
	}
}
func TestNewAdminForcedEnrollment(t *testing.T) {
	for _, source := range []string{"ui", "cli"} {
		t.Run(source, func(t *testing.T) {
			unsetMFARequirement(t)
			h, st := localAccountHandler(t)
			alice, err := st.CreateFirstAdmin("alice", usersPassword)
			if err != nil {
				t.Fatal(err)
			}
			if source == "ui" {
				secret, err := st.BeginTOTPEnrollment(alice.AccountID)
				if err != nil {
					t.Fatal(err)
				}
				code, _ := totp.Code(secret, time.Now().Unix()/30)
				if _, err = st.ConfirmTOTP(alice.AccountID, code); err != nil {
					t.Fatal(err)
				}
				cookie := adminSession(t, st)
				code, _ = totp.Code(secret, time.Now().Unix()/30+1)
				fields := url.Values{"username": {"bob"}, "initial_password": {usersPassword}, "email": {"bob@example.com"}, "password": {usersPassword}, "code": {code}}
				if w := userRequest(h, "create", fields, cookie); w.Code != 303 {
					t.Fatal(w.Code, w.Body.String())
				}
			} else {
				var seq int
				var name, path string
				if err = st.DB().QueryRow(`PRAGMA database_list`).Scan(&seq, &name, &path); err != nil {
					t.Fatal(err)
				}
				if err = runAdminPasswordCommand("add-admin", []string{"--db", path, "--username", "bob", "--email", "bob@example.com"}, strings.NewReader(usersPassword+"\n")); err != nil {
					t.Fatal(err)
				}
			}
			w := accountRequest(h, "POST", "/login", "username=bob%40example.com&password=a+long+test+password", nil)
			if w.Code != 303 || w.Header().Get("Location") != "/account/security" {
				t.Fatal(w.Code, w.Header(), w.Body.String())
			}
			cookie := w.Result().Cookies()[0]
			for _, path := range []string{"/", "/account/users"} {
				w = accountRequest(h, "GET", path, "", cookie)
				if w.Code != 303 || w.Header().Get("Location") != "/account/security" {
					t.Fatal(path, w.Code)
				}
			}
			w = accountRequest(h, "GET", "/v1/operator/machines", "", cookie)
			if w.Code != 403 {
				t.Fatal(w.Code)
			}
			secret := enrollmentSecret(t, accountRequest(h, "GET", "/account/security", "", cookie))
			code, _ := totp.Code(secret, time.Now().Unix()/30)
			w = accountRequest(h, "POST", "/account/security/totp/confirm", "code="+code, cookie)
			if w.Code != 200 || !strings.Contains(w.Body.String(), "save these now") {
				t.Fatal(w.Code, w.Body.String())
			}
			if w = accountRequest(h, "GET", "/account/users", "", cookie); w.Code != 200 {
				t.Fatal(w.Code, w.Body.String())
			}
		})
	}
}

func TestEveryUserMutationRequiresReauthentication(t *testing.T) {
	for _, action := range []string{"create", "disable", "enable", "email", "rename"} {
		t.Run(action, func(t *testing.T) {
			t.Setenv("CLAWCTL_REQUIRE_MFA", "0")
			h, st := localAccountHandler(t)
			alice, err := st.CreateFirstAdmin("alice", usersPassword)
			if err != nil {
				t.Fatal(err)
			}
			if err = st.MutateHubUser("created", "bob", "bob@example.com", usersPassword, alice.AccountID); err != nil {
				t.Fatal(err)
			}
			if action == "enable" {
				if err = st.MutateHubUser("disabled", "bob", "", "", alice.AccountID); err != nil {
					t.Fatal(err)
				}
			}
			secret, err := st.BeginTOTPEnrollment(alice.AccountID)
			if err != nil {
				t.Fatal(err)
			}
			replay, _ := totp.Code(secret, time.Now().Unix()/30)
			codes, err := st.ConfirmTOTP(alice.AccountID, replay)
			if err != nil {
				t.Fatal(err)
			}
			cookie := adminSession(t, st)
			username := "bob"
			if action == "create" {
				username = "carol"
			}
			fields := url.Values{"username": {username}, "initial_password": {usersPassword}, "email": {"carol@example.com"}, "new_username": {"carol"}}
			before, err := st.ListHubUsers()
			if err != nil {
				t.Fatal(err)
			}
			for attempt, credentials := range [][2]string{{"", ""}, {"wrong password", ""}, {usersPassword, ""}, {usersPassword, "invalid"}, {usersPassword, replay}, {usersPassword, codes[0]}} {
				fields.Set("password", credentials[0])
				fields.Set("code", credentials[1])
				r := httptest.NewRequest("POST", "https://hub.example.com/account/users/"+action, strings.NewReader(fields.Encode()))
				r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				r.AddCookie(cookie)
				// Each credential check uses a distinct IP to isolate the mutation limiter.
				r.RemoteAddr = "192.0.2." + fmt.Sprint(10+attempt) + ":1234"
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				if w.Code != 400 {
					t.Fatal("credential attempt", attempt, w.Code)
				}
				after, err := st.ListHubUsers()
				if err != nil || !reflect.DeepEqual(before, after) {
					t.Fatal("failed reauth changed accounts", after, err)
				}
			}
		})
	}
}

func TestUsersTailscale404(t *testing.T) {
	st := boundaryStore(t)
	mux := http.NewServeMux()
	registerUserRoutes(mux, st, clientip.Resolver{})
	for _, pattern := range userPatterns {
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

func TestUsersMutationLifecycle(t *testing.T) {
	t.Setenv("CLAWCTL_REQUIRE_MFA", "0")
	h, st := localAccountHandler(t)
	if _, err := st.CreateFirstAdmin("alice", usersPassword); err != nil {
		t.Fatal(err)
	}
	cookie := adminSession(t, st)
	fields := url.Values{"username": {"bob"}, "initial_password": {usersPassword}, "password": {usersPassword}}
	post := func(action string) {
		t.Helper()
		w := userRequest(h, action, fields, cookie)
		if w.Code != 303 {
			t.Fatal(action, w.Code, w.Body.String())
		}
	}
	post("create")
	bob, err := st.VerifyPassword("bob", usersPassword, "192.0.2.3")
	if err != nil {
		t.Fatal(err)
	}
	token, err := st.CreateSession(bob, "192.0.2.3", "")
	if err != nil {
		t.Fatal(err)
	}
	fields.Set("email", " BOB@EXAMPLE.COM ")
	post("email")
	fields.Set("new_username", " CAROL ")
	post("rename")
	if a, err := st.LookupSession(token); err != nil || a.Username != "carol" {
		t.Fatal(a, err)
	}
	fields.Set("username", "carol")
	post("disable")
	if _, err := st.LookupSession(token); err == nil {
		t.Fatal("disable kept session")
	}
	post("enable")
	if _, err := st.VerifyPassword("carol", usersPassword, "192.0.2.3"); err != nil {
		t.Fatal(err)
	}
	users, err := st.ListHubUsers()
	if err != nil || len(users) != 2 || users[1].Email != "bob@example.com" || users[1].Disabled {
		t.Fatal(users, err)
	}
}
