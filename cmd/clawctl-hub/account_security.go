package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"html/template"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/teddashh/AI-Intune/internal/clientip"
	"github.com/teddashh/AI-Intune/internal/localauth"
	"github.com/teddashh/AI-Intune/internal/operatorauth"
	"github.com/teddashh/AI-Intune/internal/store"
	"github.com/teddashh/AI-Intune/internal/totp"
)

type pendingLogin struct {
	account    store.HubAccount // includes the durable password-verification generation
	addr, next string
	expires    time.Time
}
type pendingLogins struct {
	mu      sync.Mutex
	entries map[[32]byte]pendingLogin
	now     func() time.Time
}

// Every pending-login table in this process, so disabling or re-enabling an
// account can drop its password-verified MFA challenges.
var pendingLoginTables struct {
	mu     sync.Mutex
	tables []*pendingLogins
}

// Pending logins bind to the full resolved client IP, including behind trusted proxies.
func newPendingLogins() *pendingLogins {
	p := &pendingLogins{entries: make(map[[32]byte]pendingLogin), now: time.Now}
	pendingLoginTables.mu.Lock()
	pendingLoginTables.tables = append(pendingLoginTables.tables, p)
	pendingLoginTables.mu.Unlock()
	return p
}

// purgePendingLoginsForAccount removes every in-flight MFA challenge for the account.
func purgePendingLoginsForAccount(accountID string) {
	if accountID == "" {
		return
	}
	pendingLoginTables.mu.Lock()
	tables := append([]*pendingLogins(nil), pendingLoginTables.tables...)
	pendingLoginTables.mu.Unlock()
	for _, p := range tables {
		p.mu.Lock()
		for k, v := range p.entries {
			if v.account.AccountID == accountID {
				delete(p.entries, k)
			}
		}
		p.mu.Unlock()
	}
}
func (p *pendingLogins) add(a store.HubAccount, addr, next string) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(b)
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	for k, v := range p.entries {
		if !now.Before(v.expires) {
			delete(p.entries, k)
		}
	}
	if len(p.entries) >= 4096 {
		return "", fmt.Errorf("too many pending logins")
	}
	p.entries[sha256.Sum256([]byte(token))] = pendingLogin{a, addr, next, now.Add(5 * time.Minute)}
	return token, nil
}
func (p *pendingLogins) get(token, addr string) (pendingLogin, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	v, ok := p.entries[sha256.Sum256([]byte(token))]
	return v, ok && v.addr == addr && p.now().Before(v.expires)
}

// consume is the single atomic winner after factor verification. A failed factor
// leaves the challenge available, but concurrent valid factors cannot issue two sessions.
func (p *pendingLogins) consume(token, addr string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	key := sha256.Sum256([]byte(token))
	v, ok := p.entries[key]
	if !ok || v.addr != addr || !p.now().Before(v.expires) {
		return false
	}
	delete(p.entries, key)
	return true
}

var mfaForm = template.Must(template.New("mfa").Parse(`<!doctype html><html lang="en"><meta charset="utf-8"><title>Second factor · clawctl</title><h1>Second factor</h1><p role="alert">{{.Error}}</p><form method="post" action="/login/mfa"><label>Authenticator or recovery code <input name="code" autocomplete="one-time-code" required maxlength="32"></label><button>Sign in</button></form></html>`))

func renderMFAForm(w http.ResponseWriter, message string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = mfaForm.Execute(w, struct{ Error string }{message})
}

var securityForm = template.Must(template.New("security").Parse(`<!doctype html><html lang="en"><meta charset="utf-8"><title>Account security · clawctl</title><a href="/">Hub</a><a href="/account/service-tokens">Service tokens</a> <a href="/account/users">Admin users</a><h1>Account security</h1>{{if .Warning}}<p role="alert" style="padding:12px 16px;border:1px solid #d29200;background:#fff4ce;color:#8a3b00">WARNING: {{.Warning}}</p>{{end}}<p>MFA enabled: {{.Enabled}}</p>{{if .Secret}}<p>Add this secret to your authenticator: <code>{{.Secret}}</code></p><p><code>{{.URI}}</code></p><a href="{{.URI}}">Authenticator URI (copy link)</a><form method="post" action="/account/security/totp/confirm"><label>6-digit code <input name="code" required maxlength="6"></label><button>Enable MFA</button></form>{{else}}{{if .Enabled}}<form method="post" action="/account/security/recovery-codes/regenerate"><label>Current password <input type="password" name="password" required maxlength="256"></label><label>Authenticator code <input name="code" required maxlength="6" inputmode="numeric" autocomplete="one-time-code"></label><button>Regenerate recovery codes</button></form>{{if .Required}}<p>MFA is required. Only host recovery can remove it.</p>{{else}}<form method="post" action="/account/security/totp/disable"><label>Current password <input type="password" name="password" required maxlength="256"></label><label>Authenticator or recovery code <input name="code" required maxlength="32"></label><button>Disable MFA</button></form>{{end}}{{else}}<form method="post" action="/account/security/totp/begin"><button>Set up authenticator</button></form>{{end}}{{end}}{{if .Codes}}<h2>Recovery codes — save these now</h2><p>Previous recovery codes no longer work. Each code works once. These codes will only be shown once.</p><pre>{{range .Codes}}{{.}}
{{end}}</pre><a href="/">I saved my recovery codes — continue to Hub</a>{{end}}{{if or .Enabled (not .Required)}}<form method="post" action="/account/security/password"><label>Current password <input type="password" name="password" required maxlength="256"></label><label>New password <input type="password" name="new_password" required minlength="12" maxlength="256"></label><button>Change password and revoke other sessions</button></form>{{end}}<form method="post" action="/logout"><button>Sign out</button></form></html>`))
var securityPatterns = []string{"GET /account/security", "POST /account/security/totp/begin", "POST /account/security/totp/confirm", "POST /account/security/totp/disable", "POST /account/security/password", "POST /account/security/recovery-codes/regenerate"}

func registerSecurityRoutes(mux *http.ServeMux, st *store.Store, authority string, resolver clientip.Resolver, cloud ...cloudBoundaryConfig) []string {
	required := len(cloud) > 0 && cloud[0].requireMFA
	limiter := newIPLimiter(10, 5)
	handler := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		p, ok := operatorauth.PrincipalFromContext(r.Context())
		if !ok || p.AuthMethod != operatorauth.AuthMethodLocalAccountSession {
			http.NotFound(w, r)
			return
		}
		id := strings.TrimPrefix(p.StableSubject(), "local-user:")
		enabled, err := st.MFAEnabled(id)
		if err != nil {
			http.Error(w, "Account unavailable", 503)
			return
		}
		data := struct {
			Enabled  bool
			Required bool
			Warning  string
			Secret   string
			URI      template.URL
			Codes    []string
		}{Enabled: enabled, Required: required}
		if len(cloud) > 0 && !required {
			data.Warning = mfaDisabledWarning
		}
		begin := func() {
			var secret string
			secret, err = st.BeginTOTPEnrollment(id)
			if err == nil {
				var groups []string
				for i := 0; i < len(secret); i += 4 {
					groups = append(groups, secret[i:i+4])
				}
				data.Secret = strings.Join(groups, " ")
				data.URI = template.URL(totp.URI(secret, p.TailnetUserLogin, authority))
			}
		}
		if r.Method == "GET" && !enabled && (required || r.URL.Query().Get("enroll") == "1") {
			begin()
			if err != nil {
				http.Error(w, "Enrollment unavailable", 503)
				return
			}
		}
		ip := resolver.Resolve(r)
		metadata := store.AuditEntry{SourceAddr: ip, UserAgent: r.UserAgent()}
		if r.Method == "POST" {
			if required && r.URL.Path == "/account/security/totp/disable" {
				http.Error(w, "MFA is required; only the host CLI reset-admin-password --disable-mfa can remove it", 400)
				return
			}
			if !limiter.allow(ip) {
				http.Error(w, "Too many attempts", 429)
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, 4096)
			if r.ParseForm() != nil {
				http.Error(w, "Invalid form", 400)
				return
			}
			switch r.URL.Path {
			case "/account/security/totp/begin":
				begin()
			case "/account/security/totp/confirm":
				var c *http.Cookie
				c, err = r.Cookie(localauth.CookieName(len(cloud) > 0 && cloud[0].public.Scheme() == "https"))
				if err == nil {
					data.Codes, err = st.ConfirmTOTPForSession(id, r.PostForm.Get("code"), c.Value, metadata)
				}
				data.Enabled = err == nil
			case "/account/security/totp/disable", "/account/security/password", "/account/security/recovery-codes/regenerate":
				var a store.HubAccount
				a, err = st.VerifyPassword(p.TailnetUserLogin, r.PostForm.Get("password"), ip, metadata)
				if err == nil && a.AccountID != id {
					err = store.ErrAccountAuth
				}
				if err == nil && r.URL.Path == "/account/security/recovery-codes/regenerate" {
					data.Codes, err = st.RegenerateRecoveryCodes(id, ip, r.PostForm.Get("code"), metadata)
				}
				if err == nil && r.URL.Path == "/account/security/totp/disable" {
					err = st.VerifySecondFactor(id, ip, r.PostForm.Get("code"), metadata)
					if err == nil {
						err = st.DisableMFA(id, metadata)
						data.Enabled = false
					}
				}
				if err == nil && r.URL.Path == "/account/security/password" {
					c, e := r.Cookie(localauth.CookieName(true))
					if e != nil {
						c, e = r.Cookie(localauth.CookieName(false))
					}
					if e != nil {
						err = e
					} else {
						err = st.ChangePassword(id, r.PostForm.Get("new_password"), c.Value)
					}
				}
			}
			if err != nil {
				http.Error(w, "Unable to update account security; check your credentials and password requirements", 400)
				return
			}
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = securityForm.Execute(w, data)
	}
	for _, pattern := range securityPatterns {
		mux.HandleFunc(pattern, handler)
	}
	patterns := append(append([]string(nil), securityPatterns...), registerUserRoutes(mux, st, resolver, cloud...)...)
	return append(patterns, registerServiceTokenRoutes(mux, st)...)
}
