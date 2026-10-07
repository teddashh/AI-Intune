package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/teddashh/AI-Intune/internal/localauth"
	"github.com/teddashh/AI-Intune/internal/operatorauth"
	"github.com/teddashh/AI-Intune/internal/store"
	"github.com/teddashh/AI-Intune/internal/totp"
)

type pendingLogin struct {
	account    store.HubAccount
	addr, next string
	expires    time.Time
}
type pendingLogins struct {
	mu      sync.Mutex
	entries map[[32]byte]pendingLogin
	now     func() time.Time
}

// Bind to the TCP peer IP, not its ephemeral port. Forwarded headers are ignored.
func pendingSource(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	return host
}
func newPendingLogins() *pendingLogins {
	return &pendingLogins{entries: make(map[[32]byte]pendingLogin), now: time.Now}
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
	p.entries[sha256.Sum256([]byte(token))] = pendingLogin{a, pendingSource(addr), next, now.Add(5 * time.Minute)}
	return token, nil
}
func (p *pendingLogins) get(token, addr string) (pendingLogin, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	v, ok := p.entries[sha256.Sum256([]byte(token))]
	return v, ok && v.addr == pendingSource(addr) && p.now().Before(v.expires)
}
func (p *pendingLogins) remove(token string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.entries, sha256.Sum256([]byte(token)))
}

var mfaForm = template.Must(template.New("mfa").Parse(`<!doctype html><html lang="en"><meta charset="utf-8"><title>Second factor · clawctl</title><h1>Second factor</h1><p role="alert">{{.Error}}</p><form method="post" action="/login/mfa"><label>Authenticator or recovery code <input name="code" autocomplete="one-time-code" required maxlength="32"></label><button>Sign in</button></form></html>`))

func renderMFAForm(w http.ResponseWriter, message string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = mfaForm.Execute(w, struct{ Error string }{message})
}

var securityForm = template.Must(template.New("security").Parse(`<!doctype html><html lang="en"><meta charset="utf-8"><title>Account security · clawctl</title><a href="/">Hub</a><h1>Account security</h1><p>MFA enabled: {{.Enabled}}</p>{{if .Secret}}<p>Add this secret to your authenticator: <code>{{.Secret}}</code></p><p><code>{{.URI}}</code></p><a href="{{.URI}}">Authenticator URI (copy link)</a><form method="post" action="/account/security/totp/confirm"><label>6-digit code <input name="code" required maxlength="6"></label><button>Enable MFA</button></form>{{else}}{{if .Enabled}}<form method="post" action="/account/security/totp/disable"><label>Current password <input type="password" name="password" required maxlength="256"></label><label>Authenticator or recovery code <input name="code" required maxlength="32"></label><button>Disable MFA</button></form>{{else}}<form method="post" action="/account/security/totp/begin"><button>Set up authenticator</button></form>{{end}}{{end}}{{if .Codes}}<h2>Recovery codes — save these now</h2><p>Each code works once. These codes will only be shown once.</p><pre>{{range .Codes}}{{.}}
{{end}}</pre>{{end}}<form method="post" action="/account/security/password"><label>Current password <input type="password" name="password" required maxlength="256"></label><label>New password <input type="password" name="new_password" required minlength="12" maxlength="256"></label><button>Change password and revoke other sessions</button></form></html>`))
var securityPatterns = []string{"GET /account/security", "POST /account/security/totp/begin", "POST /account/security/totp/confirm", "POST /account/security/totp/disable", "POST /account/security/password"}

func registerSecurityRoutes(mux *http.ServeMux, st *store.Store, authority string) []string {
	limiter := newIPLimiter(10, 5)
	handler := func(w http.ResponseWriter, r *http.Request) {
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
			Enabled bool
			Secret  string
			URI     template.URL
			Codes   []string
		}{Enabled: enabled}
		metadata := store.AuditEntry{SourceAddr: r.RemoteAddr, UserAgent: r.UserAgent()}
		if r.Method == "POST" {
			if !limiter.allow(r.RemoteAddr) {
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
			case "/account/security/totp/confirm":
				data.Codes, err = st.ConfirmTOTP(id, r.PostForm.Get("code"), metadata)
				data.Enabled = err == nil
			case "/account/security/totp/disable", "/account/security/password":
				var a store.HubAccount
				a, err = st.VerifyPassword(p.TailnetUserLogin, r.PostForm.Get("password"), r.RemoteAddr, metadata)
				if err == nil && a.AccountID != id {
					err = store.ErrAccountAuth
				}
				if err == nil && r.URL.Path == "/account/security/totp/disable" {
					err = st.VerifySecondFactor(id, r.PostForm.Get("code"), metadata)
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
	return securityPatterns
}
