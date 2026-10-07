package main

import (
	"bytes"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/store"
	"github.com/teddashh/AI-Intune/internal/totp"
)

func unsetMFARequirement(t *testing.T) {
	t.Helper()
	t.Setenv("CLAWCTL_REQUIRE_MFA", "")
	if err := os.Unsetenv("CLAWCTL_REQUIRE_MFA"); err != nil {
		t.Fatal(err)
	}
}

func TestMFAEnforcementConfiguration(t *testing.T) {
	for _, mode := range []authMode{authModeLocal, authModeBoth, authModeTailscale} {
		t.Run(string(mode), func(t *testing.T) {
			unsetMFARequirement(t)
			required, err := mfaEnforcement(mode, nil)
			if err != nil || required != (mode != authModeTailscale) {
				t.Fatal(required, err)
			}
			for _, raw := range []string{"0", "false", "off", "1", "true", "on", "TRUE", "OFF", "", "yes", "2", " true "} {
				t.Run(fmt.Sprintf("value=%q", raw), func(t *testing.T) {
					t.Setenv("CLAWCTL_REQUIRE_MFA", raw)
					var logs bytes.Buffer
					required, err := mfaEnforcement(mode, log.New(&logs, "", 0).Printf)
					if mode == authModeTailscale {
						if err != nil || required || logs.Len() != 0 {
							t.Fatal(required, err, logs.String())
						}
						return
					}
					switch strings.ToLower(raw) {
					case "0", "false", "off":
						if err != nil || required || !strings.Contains(logs.String(), "WARNING: "+mfaDisabledWarning) {
							t.Fatal(required, err, logs.String())
						}
					case "1", "true", "on":
						if err != nil || !required || logs.Len() != 0 {
							t.Fatal(required, err, logs.String())
						}
					default:
						if err == nil || !strings.Contains(err.Error(), "CLAWCTL_REQUIRE_MFA") {
							t.Fatal("invalid setting accepted", raw)
						}
						t.Setenv("CLAWCTL_PUBLIC_URL", "https://hub.example.com")
						if _, err = cloudConfiguration(mode, "127.0.0.1:8787"); err == nil {
							t.Fatal("startup accepted invalid MFA setting")
						}
					}
				})
			}
		})
	}
}

func enrollmentSecret(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	body := w.Body.String()
	at := strings.Index(body, "secret=")
	if w.Code != 200 || at < 0 || !strings.Contains(body, "otpauth://totp/") || !strings.Contains(body, `action="/account/security/totp/confirm"`) {
		t.Fatal(w.Code, body)
	}
	return body[at+7 : at+7+32]
}

func TestSetupRequiredMFAEnrollment(t *testing.T) {
	for _, mode := range []authMode{authModeLocal, authModeBoth} {
		t.Run(string(mode), func(t *testing.T) {
			unsetMFARequirement(t)
			h, st := localAccountHandlerForMode(t, mode)
			w := accountRequest(h, "POST", "/setup", "username=admin&password=a+long+test+password&setup_code=abcdefghijklmnop", nil)
			if w.Code != 303 || w.Header().Get("Location") != "/account/security?enroll=1" {
				t.Fatal(w.Code, w.Header(), w.Body.String())
			}
			session := w.Result().Cookies()[0]
			for _, path := range []string{"/", "/machines", "/settings/tailnet"} {
				w = accountRequest(h, "GET", path, "", session)
				if w.Code != 303 || w.Header().Get("Location") != "/account/security" {
					t.Fatal(path, w.Code, w.Header())
				}
			}
			w = accountRequest(h, "GET", "/v1/operator/machines", "", session)
			if w.Code != 403 || !strings.Contains(w.Body.String(), "MFA_ENROLLMENT_REQUIRED") {
				t.Fatal(w.Code, w.Body.String())
			}
			w = accountRequest(h, "GET", "/account/security?enroll=1", "", session)
			secret := enrollmentSecret(t, w)
			// Refreshing and explicitly beginning again preserve the scanned secret.
			if refreshed := enrollmentSecret(t, accountRequest(h, "GET", "/account/security", "", session)); refreshed != secret {
				t.Fatal("refresh changed secret")
			}
			if begun := enrollmentSecret(t, accountRequest(h, "POST", "/account/security/totp/begin", "", session)); begun != secret {
				t.Fatal("begin changed secret")
			}
			code, err := totp.Code(secret, time.Now().Unix()/30)
			if err != nil {
				t.Fatal(err)
			}
			w = accountRequest(h, "POST", "/account/security/totp/confirm", "code="+code, session)
			if w.Code != 200 || !strings.Contains(w.Body.String(), "save these now") || !strings.Contains(w.Body.String(), "continue to Hub") {
				t.Fatal(w.Code, w.Body.String())
			}
			at := strings.Index(w.Body.String(), "<pre>")
			codes := strings.Fields(strings.SplitN(w.Body.String()[at+5:], "</pre>", 2)[0])
			if len(codes) != 10 {
				t.Fatal(codes)
			}
			w = accountRequest(h, "GET", "/account/security", "", session)
			for _, code := range codes {
				if strings.Contains(w.Body.String(), code) {
					t.Fatal("recovery code shown twice")
				}
			}
			if strings.Contains(w.Body.String(), `action="/account/security/totp/disable"`) {
				t.Fatal("disable form shown under enforcement")
			}
			w = accountRequest(h, "POST", "/account/security/totp/disable", "password=a+long+test+password&code="+codes[0], session)
			if w.Code != 400 || !strings.Contains(w.Body.String(), "only the host CLI") {
				t.Fatal(w.Code, w.Body.String())
			}
			a, err := st.LookupSession(session.Value)
			if err != nil {
				t.Fatal(err)
			}
			if enabled, err := st.MFAEnabled(a.AccountID); !enabled || err != nil {
				t.Fatal("disable changed MFA", enabled, err)
			}
			w = accountRequest(h, "GET", "/", "", session)
			if w.Code != 200 || strings.Contains(w.Body.String(), mfaDisabledWarning) {
				t.Fatal(w.Code, w.Body.String())
			}
			// Confirmation cannot re-display the recovery codes.
			w = accountRequest(h, "POST", "/account/security/totp/confirm", "code="+code, session)
			if w.Code != 400 {
				t.Fatal("confirmation replay accepted", w.Code)
			}
		})
	}
}

func TestMFAOptOutPasswordOnlyAndWarnings(t *testing.T) {
	for _, mode := range []authMode{authModeLocal, authModeBoth} {
		for _, raw := range []string{"0", "false", "off"} {
			t.Run(string(mode)+"/"+raw, func(t *testing.T) {
				t.Setenv("CLAWCTL_REQUIRE_MFA", raw)
				var logs bytes.Buffer
				if required, err := mfaEnforcement(mode, log.New(&logs, "", 0).Printf); required || err != nil || !strings.Contains(logs.String(), "WARNING: "+mfaDisabledWarning) {
					t.Fatal(required, err, logs.String())
				}
				h, st := localAccountHandlerForMode(t, mode)
				if _, err := st.CreateFirstAdmin("admin", "a long test password"); err != nil {
					t.Fatal(err)
				}
				w := accountRequest(h, "POST", "/login", "username=admin&password=a+long+test+password", nil)
				if w.Code != 303 || w.Header().Get("Location") != "/" {
					t.Fatal(w.Code, w.Header())
				}
				session := w.Result().Cookies()[0]
				for range 2 {
					for _, path := range []string{"/", "/account/security"} {
						w = accountRequest(h, "GET", path, "", session)
						if w.Code != 200 || !strings.Contains(w.Body.String(), mfaDisabledWarning) {
							t.Fatal(path, w.Code, w.Body.String())
						}
					}
				}
				w = accountRequest(h, "GET", "/v1/operator/machines", "", session)
				if w.Code != 200 {
					t.Fatal(w.Code, w.Body.String())
				}
			})
		}
	}
}

func TestBootstrapAndHostRecoveryForceEnrollment(t *testing.T) {
	unsetMFARequirement(t)
	h, st := localAccountHandler(t)
	var seq int
	var name, path string
	if err := st.DB().QueryRow(`PRAGMA database_list`).Scan(&seq, &name, &path); err != nil {
		t.Fatal(err)
	}
	args := []string{"--db", path, "--username", "admin"}
	if err := runBootstrapAdmin(args, strings.NewReader("a long test password")); err != nil {
		t.Fatal(err)
	}
	w := accountRequest(h, "POST", "/login", "username=admin&password=a+long+test+password", nil)
	if w.Code != 303 || w.Header().Get("Location") != "/account/security" {
		t.Fatal(w.Code, w.Header())
	}
	session := w.Result().Cookies()[0]
	secret := enrollmentSecret(t, accountRequest(h, "GET", "/account/security", "", session))
	code, _ := totp.Code(secret, time.Now().Unix()/30)
	if w = accountRequest(h, "POST", "/account/security/totp/confirm", "code="+code, session); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if err := runResetAdminPassword(append(args, "--disable-mfa"), strings.NewReader("a replacement password")); err != nil {
		t.Fatal(err)
	}
	if _, err := st.LookupSession(session.Value); err == nil {
		t.Fatal("host recovery kept old session")
	}
	w = accountRequest(h, "POST", "/login", "username=admin&password=a+replacement+password", nil)
	if w.Code != 303 || w.Header().Get("Location") != "/account/security" {
		t.Fatal(w.Code, w.Header())
	}
	session = w.Result().Cookies()[0]
	replacement := enrollmentSecret(t, accountRequest(h, "GET", "/account/security", "", session))
	if replacement == secret {
		t.Fatal("host recovery reused factor")
	}
	w = accountRequest(h, "POST", "/logout", "", session)
	if w.Code != 303 || w.Header().Get("Location") != "/login" {
		t.Fatal("logout blocked before enrollment", w.Code)
	}
}

func TestMFAPendingLoginTrustedProxyBinding(t *testing.T) {
	for _, header := range []string{"X-Forwarded-For", "Fly-Client-IP"} {
		t.Run(header, func(t *testing.T) {
			t.Setenv("CLAWCTL_TRUSTED_PROXIES", "10.0.0.0/8")
			t.Setenv("CLAWCTL_CLIENT_IP_HEADER", header)
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
			request := func(peer, ip, path, body string, cookie *http.Cookie) *httptest.ResponseRecorder {
				r := httptest.NewRequest("POST", "https://hub.example.com"+path, strings.NewReader(body))
				r.RemoteAddr = peer
				r.Header.Set(header, ip)
				r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				if cookie != nil {
					r.AddCookie(cookie)
				}
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				return w
			}
			w := request("10.0.0.1:1234", "192.0.2.1", "/login", "username=admin&password=a+long+test+password", nil)
			if w.Code != 200 {
				t.Fatal(w.Code, w.Body.String())
			}
			pending := w.Result().Cookies()[0]
			// Another client behind the same trusted proxy cannot use the cookie.
			w = request("10.0.0.1:1234", "192.0.2.2", "/login/mfa", "code="+codes[0], pending)
			if w.Code != 401 {
				t.Fatal("pending accepted different resolved IP", w.Code)
			}
			// A bad factor is stored under the resolved client, not the proxy.
			w = request("10.0.0.2:5678", "192.0.2.1", "/login/mfa", "code=bad", pending)
			if w.Code != 401 {
				t.Fatal(w.Code)
			}
			var failures int
			if err := st.DB().QueryRow(`SELECT failed_attempts FROM hub_login_failures WHERE account_id=? AND client_ip=?`, a.AccountID, "192.0.2.1").Scan(&failures); err != nil || failures != 1 {
				t.Fatal(failures, err)
			}
			// Changing trusted proxy peers/ports keeps the same resolved-client binding.
			w = request("10.0.0.3:9999", "192.0.2.1", "/login/mfa", "code="+codes[0], pending)
			if w.Code != 303 {
				t.Fatal(w.Code, w.Body.String())
			}
			var source string
			if err := st.DB().QueryRow(`SELECT source_addr FROM hub_sessions WHERE account_id=?`, a.AccountID).Scan(&source); err != nil || source != "192.0.2.1" {
				t.Fatal(source, err)
			}
			if err := st.DB().QueryRow(`SELECT source_addr FROM audit_log WHERE action=? AND auth_subject=? ORDER BY at DESC LIMIT 1`, store.AuditMFAFailed, "local-user:"+a.AccountID).Scan(&source); err != nil || source != "192.0.2.1" {
				t.Fatal(source, err)
			}
		})
	}
}

func TestRequiredMFAGatesEveryOperatorRoute(t *testing.T) {
	unsetMFARequirement(t)
	h, st := localAccountHandler(t)
	if _, err := st.CreateFirstAdmin("admin", "a long test password"); err != nil {
		t.Fatal(err)
	}
	w := accountRequest(h, "POST", "/login", "username=admin&password=a+long+test+password", nil)
	if w.Code != 303 {
		t.Fatal(w.Code, w.Body.String())
	}
	session := w.Result().Cookies()[0]
	placeholder := regexp.MustCompile(`\{[^}]+\}`)
	for pattern, policy := range operatorRoutePolicies {
		if isSecurityEnrollmentRoute(pattern) {
			continue
		}
		method, path, _ := strings.Cut(pattern, " ")
		path = strings.ReplaceAll(path, "{$}", "")
		path = placeholder.ReplaceAllString(path, "test")
		w = accountRequest(h, method, path, "", session)
		want := 403
		if policy.Representation == operatorHTML {
			want = 303
		}
		if w.Code != want {
			t.Errorf("%s: got %d, want %d: %s", pattern, w.Code, want, w.Body.String())
		}
		if want == 303 && w.Header().Get("Location") != "/account/security" {
			t.Errorf("%s: redirect = %s", pattern, w.Header().Get("Location"))
		}
	}
}

func TestMFAEnrollmentPOSTsRejectCrossOrigin(t *testing.T) {
	unsetMFARequirement(t)
	h, st := localAccountHandler(t)
	if _, err := st.CreateFirstAdmin("admin", "a long test password"); err != nil {
		t.Fatal(err)
	}
	w := accountRequest(h, "POST", "/login", "username=admin&password=a+long+test+password", nil)
	session := w.Result().Cookies()[0]
	for _, path := range []string{"/login/mfa", "/account/security/totp/begin", "/account/security/totp/confirm"} {
		r := httptest.NewRequest("POST", "https://hub.example.com"+path, strings.NewReader("code=123456"))
		r.RemoteAddr = "192.0.2.1:1234"
		r.Header.Set("Origin", "https://attacker.example")
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.AddCookie(session)
		w = httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 403 {
			t.Errorf("%s: %d %s", path, w.Code, w.Body.String())
		}
	}
}
