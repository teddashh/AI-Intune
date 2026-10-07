// Package localauth authenticates Hub-local operator sessions, independently of Tailscale.
package localauth

import (
	"net/http"

	"github.com/teddashh/AI-Intune/internal/operatorauth"
	"github.com/teddashh/AI-Intune/internal/store"
)

type Authorizer struct {
	Store  *store.Store
	Secure bool
	Names  operatorauth.CapabilityNames
}

func CookieName(secure bool) string {
	if secure {
		return "__Host-clawctl_session"
	}
	return "clawctl_session"
}

func New(st *store.Store, secure bool, prefix string) (*Authorizer, error) {
	names := operatorauth.CapabilityNames{View: "local/cap/clawctl-view", Operate: "local/cap/clawctl-operate", Admin: "local/cap/clawctl-admin"}
	if prefix != "" {
		var err error
		names, err = operatorauth.NamesForPrefix(prefix)
		if err != nil {
			return nil, err
		}
	}
	return &Authorizer{st, secure, names}, nil
}

func (a *Authorizer) Authorize(r *http.Request, required operatorauth.Permission) (*http.Request, operatorauth.Decision) {
	deny := operatorauth.Decision{HTTPStatus: 401, Code: operatorauth.Unauthenticated, Detail: "Operator authentication required"}
	cookie, err := r.Cookie(CookieName(a.Secure))
	if err != nil {
		return nil, deny
	}
	account, err := a.Store.LookupSession(cookie.Value)
	if err != nil {
		return nil, deny
	}
	p := operatorauth.Principal{SourceAddr: r.RemoteAddr, NodeStableID: "local-session", DeviceName: "browser", TailnetUserID: "local:" + account.AccountID, TailnetUserLogin: account.Username, TailnetDisplayName: account.Username, AuthMethod: operatorauth.AuthMethodLocalAccountSession, GrantedCapabilities: a.Names}
	switch required {
	case operatorauth.View:
		p.AuthorizedCapability = a.Names.View
	case operatorauth.Operate:
		p.AuthorizedCapability = a.Names.Operate
	case operatorauth.Admin:
		p.AuthorizedCapability = a.Names.Admin
	default:
		return nil, deny
	}
	return operatorauth.WithPrincipal(r, p), operatorauth.Decision{Allowed: true, HTTPStatus: 200, Code: operatorauth.Authorized, Principal: p}
}
