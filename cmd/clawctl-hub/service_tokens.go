package main

import (
	"fmt"
	"html/template"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/teddashh/AI-Intune/internal/operatorauth"
	"github.com/teddashh/AI-Intune/internal/store"
)

var serviceTokenPatterns = []string{"GET /account/service-tokens", "POST /account/service-tokens/create", "POST /account/service-tokens/revoke"}
var serviceTokenForm = template.Must(template.New("service-tokens").Parse(`<!doctype html><html lang="en"><meta charset="utf-8"><title>Service tokens</title><a href="/account/security">Account security</a><h1>Service tokens</h1><p>{{.Message}}</p>{{if .Secret}}<p>Copy this token now. It will not be shown again.</p><code>{{.Secret}}</code>{{end}}<table><tr><th>ID</th><th>Name</th><th>Scope</th><th>Allowed routes</th><th>Source CIDRs</th><th>Created by</th><th>Created</th><th>Expires</th><th>Last used</th><th>Revoked</th></tr>{{range .Tokens}}<tr><td>{{.ID}}</td><td>{{.Name}}</td><td>{{.Scope}}</td><td>{{range .Allowlist}}{{.}}<br>{{end}}</td><td>{{range .SourceCIDRs}}{{.}}<br>{{end}}</td><td>{{.CreatedBy}}</td><td>{{.CreatedAt}}</td><td>{{.ExpiresAt}}</td><td>{{.LastUsedAt}}</td><td>{{.RevokedAt}}</td></tr>{{end}}</table><form method="post" action="/account/service-tokens/create"><input name="name" placeholder="Name" required maxlength="64"><select name="scope"><option>view</option><option>operate</option></select><input name="expires_at" placeholder="RFC3339 expiry, maximum 90 days" required><input name="allowlist" placeholder="Comma-separated route names"><input name="source_cidrs" placeholder="Comma-separated source CIDRs"><input type="password" name="password" placeholder="Current password" required><input name="code" placeholder="Authenticator code" required><button>Create</button></form><form method="post" action="/account/service-tokens/revoke"><input name="id" placeholder="Token ID" required><input type="password" name="password" placeholder="Current password" required><input name="code" placeholder="Authenticator code" required><button>Revoke</button></form></html>`))

func registerServiceTokenRoutes(mux *http.ServeMux, st *store.Store) []string {
	limiter := newIPLimiter(10, 5)
	handler := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		p, ok := operatorauth.PrincipalFromContext(r.Context())
		if !ok || p.AuthMethod != operatorauth.AuthMethodLocalAccountSession || !p.Has(operatorauth.Admin) {
			http.NotFound(w, r)
			return
		}
		actor := strings.TrimPrefix(p.StableSubject(), "local-user:")
		enabled, err := st.MFAEnabled(actor)
		if err != nil || !enabled {
			http.Error(w, "MFA required", 403)
			return
		}
		message, secret := "", ""
		if r.Method == "POST" {
			if !limiter.allow(p.SourceAddr) {
				http.Error(w, "Too many attempts", 429)
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, 8192)
			err = r.ParseForm()
			if err == nil {
				var a store.HubAccount
				a, err = st.VerifyPassword(p.TailnetUserLogin, r.PostForm.Get("password"), p.SourceAddr, store.AuditEntry{})
				if err == nil && a.AccountID != actor {
					err = store.ErrAccountAuth
				}
			}
			if err == nil {
				err = st.VerifyAdminTOTP(actor, p.SourceAddr, r.PostForm.Get("code"), store.AuditEntry{})
			}
			if err == nil {
				if strings.HasSuffix(r.URL.Path, "/create") {
					var expires time.Time
					expires, err = time.Parse(time.RFC3339, r.PostForm.Get("expires_at"))
					allow := splitServiceList(r.PostForm.Get("allowlist"))
					if len(allow) == 0 {
						err = store.ErrServiceToken
					}
					if err == nil {
						for _, route := range allow {
							policy, valid := operatorRoutePolicies[route]
							if !valid || !policy.ServiceTokenEligible || policy.Representation != operatorJSON || policy.Permission == operatorauth.Admin || !strings.Contains(route, " /v1/operator/") {
								err = store.ErrServiceToken
								break
							}
						}
					}
					if err == nil {
						_, secret, err = st.CreateServiceToken(store.ServiceToken{Name: r.PostForm.Get("name"), Scope: r.PostForm.Get("scope"), ExpiresAt: expires, Allowlist: allow, SourceCIDRs: splitServiceList(r.PostForm.Get("source_cidrs"))}, actor)
					}
				} else {
					err = st.RevokeServiceToken(r.PostForm.Get("id"), actor)
				}
			}
			if err != nil {
				message = "Unable to update service token"
				_ = st.RecordAudit(store.AuditEntry{Action: "service-token-management-denied", AuthSubject: p.StableSubject(), AuthMethod: p.AuthMethod, OK: false})
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				w.WriteHeader(400)
			}
		}
		tokens, err := st.ListServiceTokens()
		if err != nil {
			http.Error(w, "Unavailable", 503)
			return
		}
		if len(tokens) > 200 {
			tokens = tokens[:200]
			message += " Showing the latest 200 tokens; older entries are omitted."
		}
		_ = st.RecordAudit(store.AuditEntry{Action: "service-token-list", AuthSubject: p.StableSubject(), AuthMethod: p.AuthMethod, OK: true})
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = serviceTokenForm.Execute(w, struct {
			Tokens          []store.ServiceToken
			Message, Secret string
		}{tokens, message, secret})
	}
	for _, pattern := range serviceTokenPatterns {
		mux.HandleFunc(pattern, handler)
	}
	return serviceTokenPatterns
}
func splitServiceList(v string) []string {
	var out []string
	for _, s := range strings.Split(v, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// serviceResponseWriter records the first final status, including implicit 200s.
type serviceResponseWriter struct {
	http.ResponseWriter
	status int
}

func (w *serviceResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *serviceResponseWriter) WriteHeader(status int) {
	if status >= 100 && status < 200 && status != http.StatusSwitchingProtocols {
		w.ResponseWriter.WriteHeader(status)
		return
	}
	if w.status != 0 {
		return
	}
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}
func (w *serviceResponseWriter) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(data)
}

func (b *operatorBoundary) serveServiceToken(w http.ResponseWriter, r *http.Request, pattern string, policy operatorRoutePolicy) {
	source := b.clientIP.Resolve(r)
	response := &serviceResponseWriter{ResponseWriter: w}
	principal := operatorauth.Principal{AuthMethod: operatorauth.AuthMethodServiceToken, SourceAddr: source}
	defer func() {
		panicked := recover()
		if panicked != nil {
			defer func() { panic(panicked) }()
		}
		status := response.status
		if panicked != nil && status == 0 {
			status = http.StatusInternalServerError
		}
		if status == 0 {
			status = http.StatusOK
		}
		entry := store.AuditEntry{Action: "service-token-request", AuthSubject: principal.StableSubject(), AuthMethod: principal.AuthMethod, Subject: pattern, SourceAddr: source, OK: status < 400, Detail: fmt.Sprintf("HTTP status %d", status)}
		var err error
		if status >= 400 {
			entry.Action = store.AuditOperatorDenied
			// Bound denial write I/O as well as ledger size, including 429 floods.
			admission := b.denials.admit(true, source)
			if admission.suppressedUnsafe != 0 {
				b.persistSuppressedDenials(admission)
			}
			if !admission.persistUnsafe {
				return
			}
			err = b.store.RecordOperatorDenial(entry)
		} else {
			err = b.store.RecordAudit(entry)
		}
		if err != nil {
			log.Printf("service-token request audit unavailable: %v", err)
		}
	}()
	if !b.serviceAttempts.allowWithCost(source, false) {
		writeOperatorBoundaryError(response, operatorJSON, 429, "RATE_LIMITED", "Too many attempts")
		return
	}
	secret := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	required := policy.Permission.String()
	if !policy.ServiceTokenEligible {
		required = ""
	}
	token, err := b.store.AuthenticateServiceToken(secret, pattern, source, required)
	if token.Name != "" {
		principal.TailnetUserID = token.Name
		principal.TailnetUserLogin = "service:" + token.Name
		principal.NodeStableID = token.ID
	}
	if err != nil || !policy.ServiceTokenEligible || policy.Permission == operatorauth.Admin {
		b.serviceAttempts.allow(source)
		writeOperatorBoundaryError(response, operatorJSON, 401, "UNAUTHENTICATED", "Authentication required")
		return
	}
	principal.GrantedCapabilities.View = "view"
	if token.Scope == "operate" {
		principal.GrantedCapabilities.Operate = "operate"
	}
	principal.AuthorizedCapability = policy.Permission.String()
	b.csrfNext.ServeHTTP(response, operatorauth.WithPrincipal(r, principal))
}
