package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/teddashh/AI-Intune/internal/localauth"
	"github.com/teddashh/AI-Intune/internal/operatorauth"
	"github.com/teddashh/AI-Intune/internal/store"
	"github.com/teddashh/AI-Intune/internal/web"
)

type sessionFirstAuthorizer struct {
	session  *localauth.Authorizer
	fallback operatorRequestAuthorizer
}

func (a sessionFirstAuthorizer) Authorize(r *http.Request, p operatorauth.Permission) (*http.Request, operatorauth.Decision) {
	// Never fall back after an invalid session cookie.
	if _, err := r.Cookie(localauth.CookieName(a.session.Secure)); err == nil || a.fallback == nil {
		return a.session.Authorize(r, p)
	}
	return a.fallback.Authorize(r, p)
}

func withSessionAuthorizer(st *store.Store, mode authMode, secure bool, prefix string, fallback operatorRequestAuthorizer) (operatorRequestAuthorizer, error) {
	if mode == authModeTailscale {
		return fallback, nil
	}
	a, err := localauth.New(st, secure, prefix)
	if err != nil {
		return nil, err
	}
	if mode == authModeLocal {
		fallback = nil
	}
	return sessionFirstAuthorizer{a, fallback}, nil
}

var accountRoutePatterns = []string{"GET /setup", "POST /setup", "GET /login", "POST /login", "POST /logout"}

func isAccountRoute(pattern string) bool {
	for _, p := range accountRoutePatterns {
		if p == pattern {
			return true
		}
	}
	return false
}

func safeLoginNext(next string) string {
	u, err := url.Parse(next)
	if err != nil || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.ContainsAny(next, "\\\r\n") || u.Host != "" || u.IsAbs() {
		return "/"
	}
	return next
}

func registerAccountRoutes(mux *http.ServeMux, st *store.Store, ui *web.Server, authority string, cloud ...cloudBoundaryConfig) []string {
	boundary := newOperatorBoundary(http.NewServeMux(), nil, st, nil, authority, cloud...)
	limiter := newIPLimiter(10, 5)
	csrf := http.NewCrossOriginProtection()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if boundary.authMode() != authModeLocal && boundary.authMode() != authModeBoth {
			http.NotFound(w, r)
			return
		}
		boundary.writeSecurityHeaders(w, r, operatorSecurityLocked)
		if !boundary.cloud.public.MatchesAuthority(r.Host) {
			http.Error(w, "Unexpected Host", 421)
			return
		}
		if err := csrf.Check(r); err != nil {
			http.Error(w, "Cross-origin request rejected", 403)
			return
		}
		metadata := store.AuditEntry{SourceAddr: r.RemoteAddr, UserAgent: r.UserAgent()}
		setup := r.URL.Path == "/setup"
		if setup {
			n, err := st.CountAccounts()
			if err != nil {
				http.Error(w, "Authentication unavailable", 503)
				return
			}
			if n != 0 {
				http.NotFound(w, r)
				return
			}
		}
		secure := boundary.cloud.public.Scheme() == "https"
		cookie := func(token string, maxAge int) {
			http.SetCookie(w, &http.Cookie{Name: localauth.CookieName(secure), Value: token, Path: "/", HttpOnly: true, Secure: secure, SameSite: http.SameSiteLaxMode, MaxAge: maxAge})
		}
		if r.URL.Path == "/logout" {
			if c, err := r.Cookie(localauth.CookieName(secure)); err == nil {
				if err = st.RevokeSession(c.Value, metadata); err != nil {
					http.Error(w, "Logout unavailable", 503)
					return
				}
			}
			cookie("", -1)
			http.Redirect(w, r, "/login", 303)
			return
		}
		next := safeLoginNext(r.URL.Query().Get("next"))
		if r.Method == http.MethodGet {
			ui.RenderAccountForm(w, setup, next, "")
			return
		}
		if !limiter.allow(r.RemoteAddr) {
			w.Header().Set("Retry-After", "6")
			http.Error(w, "Too many attempts", 429)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 4096)
		if err := r.ParseForm(); err != nil {
			http.Error(w, "Invalid form", 400)
			return
		}
		next = safeLoginNext(r.Form.Get("next"))
		var account store.HubAccount
		var err error
		if setup {
			account, err = st.CreateFirstAdmin(r.PostForm.Get("username"), r.PostForm.Get("password"), metadata)
		} else {
			account, err = st.VerifyPassword(r.PostForm.Get("username"), r.PostForm.Get("password"), metadata)
		}
		if err != nil {
			if setup && errors.Is(err, store.ErrAdminExists) {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(401)
			message := "Invalid username or password"
			if setup {
				message = "Unable to create admin. Use a lowercase username (3–64 letters, digits, dots, underscores or hyphens) and a password of 12–256 bytes."
			}
			ui.RenderAccountForm(w, setup, next, message)
			return
		}
		token, err := st.CreateSession(account, r.RemoteAddr, r.UserAgent())
		if err != nil {
			http.Error(w, "Session unavailable", 503)
			return
		}
		cookie(token, 7*24*60*60)
		if setup {
			next = "/"
		}
		http.Redirect(w, r, next, 303)
	})
	for _, p := range accountRoutePatterns {
		mux.Handle(p, handler)
	}
	return append([]string(nil), accountRoutePatterns...)
}

// Limits use only the TCP peer. Behind a reverse proxy all clients share its
// bucket; X-Forwarded-For is deliberately not trusted. Idle buckets are pruned,
// and the map has a hard cap so arbitrary source IPs cannot grow memory forever.
type ipBucket struct {
	tokens float64
	at     time.Time
}
type ipLimiter struct {
	mu               sync.Mutex
	buckets          map[string]ipBucket
	perSecond, burst float64
}

func newIPLimiter(perMinute, burst int) *ipLimiter {
	return &ipLimiter{buckets: make(map[string]ipBucket), perSecond: float64(perMinute) / 60, burst: float64(burst)}
}
func (l *ipLimiter) allow(remote string) bool {
	ip, _, err := net.SplitHostPort(remote)
	if err != nil {
		ip = remote
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	b, ok := l.buckets[ip]
	if !ok {
		if len(l.buckets) >= 4096 {
			for k, v := range l.buckets {
				if now.Sub(v.at) > 10*time.Minute {
					delete(l.buckets, k)
				}
			}
			if len(l.buckets) >= 4096 {
				return false
			}
		}
		b = ipBucket{l.burst, now}
	}
	b.tokens = min(l.burst, b.tokens+now.Sub(b.at).Seconds()*l.perSecond)
	b.at = now
	allowed := b.tokens >= 1
	if allowed {
		b.tokens--
	}
	l.buckets[ip] = b
	return allowed
}
func (l *ipLimiter) wrap(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !l.allow(r.RemoteAddr) {
			w.Header().Set("Retry-After", "2")
			http.Error(w, "Too many attempts", 429)
			return
		}
		next(w, r)
	}
}

func runResetAdminPassword(args []string, in io.Reader) error {
	fs := flag.NewFlagSet("reset-admin-password", flag.ContinueOnError)
	db := fs.String("db", "", "path to existing Hub database")
	username := fs.String("username", "", "admin username")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *db == "" || *username == "" || fs.NArg() != 0 {
		return fmt.Errorf("reset-admin-password requires --db PATH --username U")
	}
	data, err := io.ReadAll(io.LimitReader(in, 259))
	if err != nil {
		return err
	}
	if len(data) > 258 {
		return fmt.Errorf("password must be 12–256 bytes")
	}
	password := strings.TrimSuffix(strings.TrimSuffix(string(data), "\n"), "\r")
	st, err := openExisting(*db)
	if err != nil {
		return err
	}
	defer st.Close()
	return st.ResetAdminPassword(*username, password)
}
