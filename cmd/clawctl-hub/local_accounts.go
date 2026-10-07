package main

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/teddashh/AI-Intune/internal/clientip"
	"github.com/teddashh/AI-Intune/internal/localauth"
	"github.com/teddashh/AI-Intune/internal/operatorauth"
	"github.com/teddashh/AI-Intune/internal/store"
	"github.com/teddashh/AI-Intune/internal/web"
)

// Only the normalized code's digest survives startup; it is never persisted.
type setupCodeHash [sha256.Size]byte

func normalizeSetupCode(code string) string {
	return strings.ToUpper(strings.Map(func(r rune) rune {
		if r == '-' || unicode.IsSpace(r) {
			return -1
		}
		return r
	}, code))
}

func (h *setupCodeHash) matches(code string) bool {
	if h == nil {
		return false
	}
	sum := sha256.Sum256([]byte(normalizeSetupCode(code)))
	return subtle.ConstantTimeCompare(h[:], sum[:]) == 1
}

func initializeSetupCode(st *store.Store, config *cloudBoundaryConfig, printf func(string, ...any)) error {
	if config.mode != authModeLocal && config.mode != authModeBoth {
		return nil
	}
	n, err := st.CountAccounts()
	if err != nil || n != 0 {
		return err
	}
	code, provided := os.LookupEnv("CLAWCTL_SETUP_CODE")
	if provided {
		if len(normalizeSetupCode(code)) < 16 {
			return fmt.Errorf("CLAWCTL_SETUP_CODE must contain at least 16 characters excluding spaces and hyphens")
		}
	} else {
		var random [20]byte
		if _, err := rand.Read(random[:]); err != nil {
			return err
		}
		raw := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(random[:])
		var groups []string
		for i := 0; i < len(raw); i += 4 {
			groups = append(groups, raw[i:i+4])
		}
		code = strings.Join(groups, "-")
	}
	hash := setupCodeHash(sha256.Sum256([]byte(normalizeSetupCode(code))))
	config.setupCode = &hash
	if provided {
		printf("first-run setup: open %s/setup; the operator-provided setup code is required (valid until the first admin is created; restart the Hub to rotate it)", config.public.BaseURL())
	} else {
		printf("first-run setup: open %s/setup and enter setup code %s (valid until the first admin is created; restart the Hub to rotate it)", config.public.BaseURL(), code)
	}
	return nil
}

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

func registerAccountRoutes(mux *http.ServeMux, st *store.Store, ui *web.Server, authority string, resolver clientip.Resolver, cloud ...cloudBoundaryConfig) []string {
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
			http.Error(w, "Sign in at "+boundary.cloud.public.BaseURL(), 421)
			return
		}
		if err := csrf.Check(r); err != nil {
			http.Error(w, "Cross-origin request rejected", 403)
			return
		}
		ip := resolver.Resolve(r)
		metadata := store.AuditEntry{SourceAddr: ip, UserAgent: r.UserAgent()}
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
		if !limiter.allow(ip) {
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
		username := strings.ToLower(strings.TrimSpace(r.PostForm.Get("username")))
		if setup {
			if !boundary.cloud.setupCode.matches(r.PostForm.Get("setup_code")) {
				err = store.ErrAccountAuth
			} else {
				account, err = st.CreateFirstAdmin(username, r.PostForm.Get("password"), metadata)
			}
		} else {
			account, err = st.VerifyPassword(username, r.PostForm.Get("password"), ip, metadata)
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
				message = "Unable to create admin. Check the setup code and use a username (3–64 letters, digits, dots, underscores or hyphens) and a password of 12–256 bytes."
			}
			ui.RenderAccountForm(w, setup, next, message)
			return
		}
		token, err := st.CreateSession(account, ip, r.UserAgent())
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

// Limits use clientip.Key on resolved client IPs. Idle buckets are pruned,
// and the map has a hard cap so arbitrary source IPs cannot grow memory forever.
// Eviction admits new clients; an evicted attacker only regains a fresh burst.
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
	ip := clientip.Key(remote)
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
				var oldest string
				var at time.Time
				for k, v := range l.buckets {
					if oldest == "" || v.at.Before(at) {
						oldest, at = k, v.at
					}
				}
				delete(l.buckets, oldest)
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
func (l *ipLimiter) wrap(next http.HandlerFunc, resolver clientip.Resolver) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !l.allow(resolver.Resolve(r)) {
			w.Header().Set("Retry-After", "2")
			http.Error(w, "Too many attempts", 429)
			return
		}
		next(w, r)
	}
}

func runResetAdminPassword(args []string, in io.Reader) error {
	return runAdminPasswordCommand("reset-admin-password", args, in)
}

func runBootstrapAdmin(args []string, in io.Reader) error {
	return runAdminPasswordCommand("bootstrap-admin", args, in)
}

func runAdminPasswordCommand(command string, args []string, in io.Reader) error {
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	db := fs.String("db", "", "path to existing Hub database")
	username := fs.String("username", "", "admin username")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *db == "" || *username == "" || fs.NArg() != 0 {
		return fmt.Errorf("%s requires --db PATH --username U", command)
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
	if command == "bootstrap-admin" {
		_, err := st.CreateFirstAdmin(*username, password)
		return err
	}
	return st.ResetAdminPassword(*username, password)
}
