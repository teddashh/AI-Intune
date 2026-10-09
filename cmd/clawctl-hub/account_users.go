package main

import (
	"html/template"
	"net/http"
	"strings"

	"github.com/teddashh/AI-Intune/internal/clientip"
	"github.com/teddashh/AI-Intune/internal/operatorauth"
	"github.com/teddashh/AI-Intune/internal/store"
)

var userPatterns = []string{"GET /account/users", "POST /account/users/create", "POST /account/users/disable", "POST /account/users/enable", "POST /account/users/email", "POST /account/users/rename"}
var userActions = map[string]string{"create": "created", "disable": "disabled", "enable": "enabled", "email": "email-changed", "rename": "renamed"}
var usersForm = template.Must(template.New("users").Parse(`<!doctype html><html lang="en"><meta charset="utf-8"><title>Admin users</title><a href="/account/security">Account security</a><h1>Admin users</h1><p role="alert">{{.Error}}</p><table><tr><th>Username</th><th>Email</th><th>Role</th><th>Status</th><th>MFA enrolled</th><th>Created</th></tr>{{range .Users}}<tr><td>{{.Username}}</td><td>{{.Email}}</td><td>{{.Role}}</td><td>{{if .Disabled}}disabled{{else}}active{{end}}</td><td>{{if .MFAEnrolled}}yes{{else}}no{{end}}</td><td>{{.CreatedAt}}</td></tr>{{end}}</table>{{range .Actions}}<h2>{{.}}</h2><form method="post" action="/account/users/{{.}}"><label>Username <input name="username" required maxlength="64"></label>{{if eq . "create"}}<label>Email (optional) <input name="email" maxlength="254"></label><label>Initial password <input type="password" name="initial_password" required minlength="12" maxlength="256"></label>{{end}}{{if eq . "email"}}<label>Email (empty clears) <input name="email" maxlength="254"></label>{{end}}{{if eq . "rename"}}<label>New username <input name="new_username" required maxlength="64"></label>{{end}}<label>Your current password <input type="password" name="password" required maxlength="256"></label><label>Your authenticator code <input name="code" maxlength="6" autocomplete="one-time-code"></label><button>{{.}}</button></form>{{end}}</html>`))

func registerUserRoutes(mux *http.ServeMux, st *store.Store, resolver clientip.Resolver, cloud ...cloudBoundaryConfig) []string {
	limiter := newIPLimiter(10, 5)
	handler := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		p, ok := operatorauth.PrincipalFromContext(r.Context())
		if !ok || p.AuthMethod != operatorauth.AuthMethodLocalAccountSession {
			http.NotFound(w, r)
			return
		}
		id := strings.TrimPrefix(p.StableSubject(), "local-user:")
		users, err := st.ListHubUsers()
		if err != nil {
			http.Error(w, "Account unavailable", 503)
			return
		}
		var actor store.HubAccount
		for _, a := range users {
			if a.AccountID == id {
				actor = a
			}
		}
		if actor.AccountID == "" || actor.Disabled || actor.Role != "admin" {
			http.NotFound(w, r)
			return
		}
		message := ""
		if r.Method == "POST" {
			action := userActions[strings.TrimPrefix(r.URL.Path, "/account/users/")]
			ip := resolver.Resolve(r)
			meta := store.AuditEntry{SourceAddr: ip, UserAgent: r.UserAgent()}
			auditFailure := func(detail string) {
				_ = st.RecordAudit(store.AuditEntry{Action: store.AuditAction("hub-user-" + action), AuthSubject: p.StableSubject(), AuthMethod: p.AuthMethod, Subject: r.PostForm.Get("username"), Detail: detail, OK: false, SourceAddr: ip, UserAgent: r.UserAgent()})
			}
			if !limiter.allow(ip) {
				auditFailure("rate limited")
				http.Error(w, "Too many attempts", 429)
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, 4096)
			err = r.ParseForm()
			if err == nil {
				var verified store.HubAccount
				verified, err = st.VerifyPassword(actor.Username, r.PostForm.Get("password"), ip, meta)
				if err == nil && verified.AccountID != id {
					err = store.ErrAccountAuth
				}
			}
			enabled := false
			if err == nil {
				enabled, err = st.MFAEnabled(id)
			}
			if err == nil && enabled {
				err = st.VerifyAdminTOTP(id, ip, r.PostForm.Get("code"), meta)
			}
			if err == nil && !enabled && len(cloud) > 0 && cloud[0].requireMFA {
				err = store.ErrAccountAuth
			}
			if err != nil {
				auditFailure("reauthentication or form rejected")
			} else {
				value := r.PostForm.Get("email")
				if action == "renamed" {
					value = r.PostForm.Get("new_username")
				}
				err = st.MutateHubUser(action, r.PostForm.Get("username"), value, r.PostForm.Get("initial_password"), id, meta)
				if err == nil && (action == "disabled" || action == "enabled") {
					// A challenge started before disable must not complete after re-enable.
					target := strings.ToLower(strings.TrimSpace(r.PostForm.Get("username")))
					for _, a := range users {
						if a.Username == target {
							purgePendingLoginsForAccount(a.AccountID)
						}
					}
				}
			}
			if err != nil {
				message = "Unable to update user; check credentials and user details"
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				w.WriteHeader(400)
			} else {
				http.Redirect(w, r, "/account/users", 303)
				return
			}
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = usersForm.Execute(w, struct {
			Users   []store.HubAccount
			Error   string
			Actions []string
		}{users, message, []string{"create", "disable", "enable", "email", "rename"}})
	}
	for _, pattern := range userPatterns {
		mux.HandleFunc(pattern, handler)
	}
	return userPatterns
}

// Boundary failures never parse an untrusted mutation body. The route and actor
// identify the rejected action without persisting credentials.
func recordUserBoundaryFailure(st *store.Store, r *http.Request, detail string) {
	if st == nil || r.Method != http.MethodPost {
		return
	}
	action, ok := userActions[strings.TrimPrefix(r.URL.Path, "/account/users/")]
	if !ok {
		return
	}
	p, ok := operatorauth.PrincipalFromContext(r.Context())
	if !ok || p.AuthMethod != operatorauth.AuthMethodLocalAccountSession {
		return
	}
	_ = st.RecordAudit(store.AuditEntry{Action: store.AuditAction("hub-user-" + action), AuthSubject: p.StableSubject(), AuthMethod: p.AuthMethod, Subject: r.URL.Path, Detail: detail, SourceAddr: p.SourceAddr, UserAgent: r.UserAgent(), OK: false})
}
