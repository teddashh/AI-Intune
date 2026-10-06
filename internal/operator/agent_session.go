package operator

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/teddashh/AI-Intune/internal/operatorauth"
	"github.com/teddashh/AI-Intune/internal/store"
)

type AgentSessionOpenRequest struct {
	SessionID      string
	MachineID      string
	IdempotencyKey string
	Principal      operatorauth.Principal
	Actor          Actor
}

type AgentSessionCloseRequest struct {
	SessionID      string
	MachineID      string
	Reason         string
	IdempotencyKey string
	Principal      operatorauth.Principal
	Actor          Actor
}

type AgentSessionResult = store.AgentSessionResult

func (s *Service) OpenAgentSession(req AgentSessionOpenRequest) (AgentSessionResult, error) {
	return s.store.OpenAgentSession(store.OpenAgentSessionRequest{
		SessionID: req.SessionID, MachineID: req.MachineID,
		OperatorTailnetUserID:    req.Principal.TailnetUserID,
		OperatorTailnetUserLogin: req.Principal.TailnetUserLogin,
		IdempotencyKey:           req.IdempotencyKey, RequestDigest: AgentSessionOpenSemanticDigest(req),
		Audit: agentSessionAuditFromPrincipal(req.Principal, req.Actor),
	})
}

func (s *Service) CloseAgentSession(req AgentSessionCloseRequest) (AgentSessionResult, error) {
	return s.store.CloseAgentSession(store.CloseAgentSessionRequest{
		SessionID: req.SessionID, MachineID: req.MachineID,
		OperatorTailnetUserID:    req.Principal.TailnetUserID,
		OperatorTailnetUserLogin: req.Principal.TailnetUserLogin,
		Reason:                   req.Reason, IdempotencyKey: req.IdempotencyKey,
		RequestDigest: AgentSessionCloseSemanticDigest(req),
		Audit:         agentSessionAuditFromPrincipal(req.Principal, req.Actor),
	})
}

func (s *Service) ListOpenAgentSessions(machineID string) ([]AgentSessionResult, error) {
	return s.store.ListOpenAgentSessionsForMachine(machineID)
}

func AgentSessionOpenSemanticDigest(req AgentSessionOpenRequest) string {
	body := struct {
		SessionID             string `json:"session_id"`
		MachineID             string `json:"machine_id"`
		OperatorTailnetUserID string `json:"operator_tailnet_user_id"`
	}{req.SessionID, req.MachineID, req.Principal.TailnetUserID}
	return agentSessionSemanticDigest(body)
}

func AgentSessionCloseSemanticDigest(req AgentSessionCloseRequest) string {
	body := struct {
		SessionID             string `json:"session_id"`
		MachineID             string `json:"machine_id"`
		OperatorTailnetUserID string `json:"operator_tailnet_user_id"`
		Reason                string `json:"reason"`
	}{req.SessionID, req.MachineID, req.Principal.TailnetUserID, req.Reason}
	return agentSessionSemanticDigest(body)
}

func agentSessionSemanticDigest(body any) string {
	raw, _ := json.Marshal(body)
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func agentSessionAuditFromPrincipal(principal operatorauth.Principal, actor Actor) store.AuditEntry {
	audit := auditFromActor(actor)
	audit.SourceAddr = principal.SourceAddr
	audit.WhoNode = principal.DeviceName
	audit.WhoUser = principal.TailnetUserLogin
	audit.WhoUnavailable = ""
	audit.AuthSubject = principal.StableSubject()
	audit.AuthNodeID = principal.NodeStableID
	audit.AuthCapability = principal.AuthorizedCapability
	audit.AuthMethod = principal.AuthMethod
	if audit.AuthDecision == "" {
		audit.AuthDecision = string(operatorauth.Authorized)
	}
	return audit
}
