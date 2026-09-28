package operator

import (
	"net/http"

	"github.com/teddashh/AI-Intune/internal/operatorauth"
	"github.com/teddashh/AI-Intune/internal/store"
)

// ActorFromRequest converts the already-verified boundary principal into the
// domain/audit envelope. It performs no fallback lookup: one HTTP request must
// have one authority decision, and handlers must not ask a second identity
// source that could disagree with it.
func ActorFromRequest(r *http.Request, sourceKind string) Actor {
	principal, ok := operatorauth.PrincipalFromContext(r.Context())
	if !ok {
		return Actor{
			SourceAddr: r.RemoteAddr, UserAgent: r.UserAgent(), SourceKind: sourceKind,
			WhoUnavailable: "operator-principal-unavailable",
			AuthDecision:   string(operatorauth.AuthConfigurationInvalid),
		}
	}
	return Actor{
		SourceAddr: principal.SourceAddr, WhoNode: principal.DeviceName,
		WhoUser: principal.TailnetUserLogin, UserAgent: r.UserAgent(),
		AuthSubject: principal.StableSubject(), AuthNodeID: principal.NodeStableID,
		AuthCapability: principal.AuthorizedCapability, AuthMethod: principal.AuthMethod,
		AuthDecision: string(operatorauth.Authorized), SourceKind: sourceKind,
	}
}

// ApplyActor copies one request's verified authorization and transport
// evidence onto an audit row without changing its domain fields.
func ApplyActor(entry *store.AuditEntry, actor Actor) {
	entry.SourceAddr = actor.SourceAddr
	entry.WhoNode = actor.WhoNode
	entry.WhoUser = actor.WhoUser
	entry.WhoUnavailable = actor.WhoUnavailable
	entry.UserAgent = actor.UserAgent
	entry.AuthSubject = actor.AuthSubject
	entry.AuthNodeID = actor.AuthNodeID
	entry.AuthCapability = actor.AuthCapability
	entry.AuthMethod = actor.AuthMethod
	entry.AuthDecision = actor.AuthDecision
	entry.SourceKind = actor.SourceKind
}
