package operator

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"github.com/teddashh/AI-Intune/internal/operatorauth"
	"github.com/teddashh/AI-Intune/internal/store"
)

func TestAgentSessionServiceUsesPrincipalStableUserID(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	machineID := enrollMachine(t, st, "samplehub1", false)
	if _, err := st.DB().Exec(`UPDATE machine_registry
	 SET assigned_user_id='u-1',assigned_user_login='ted@example.com' WHERE machine_id=?`, machineID); err != nil {
		t.Fatal(err)
	}
	service := New(st)
	principal := operatorauth.Principal{
		SourceAddr: "100.64.0.7:1234", NodeStableID: "node-1", DeviceName: "laptop.example.ts.net.",
		TailnetUserID: "u-1", TailnetUserLogin: "ted@example.com",
		AuthMethod: "tailscale-localapi", AuthorizedCapability: "operator",
	}
	result, err := service.OpenAgentSession(AgentSessionOpenRequest{
		SessionID: "session-ok", MachineID: machineID, IdempotencyKey: "service-open-ok",
		Principal: principal, Actor: Actor{SourceAddr: "forged", WhoUser: "forged@example.com", SourceKind: "test"},
	})
	if err != nil || result.OperatorTailnetUserID != "u-1" || result.OperatorTailnetUserLogin != "ted@example.com" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	principal.TailnetUserID = "u-2"
	_, err = service.OpenAgentSession(AgentSessionOpenRequest{
		SessionID: "session-no", MachineID: machineID, IdempotencyKey: "service-open-no",
		Principal: principal, Actor: Actor{SourceKind: "test"},
	})
	var rejection *store.OperatorRequestError
	if !errors.As(err, &rejection) || rejection.Code != store.OperatorCodeAgentSessionAssignedUserMismatch {
		t.Fatalf("same login with different principal ID err=%v", err)
	}
	var count int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM agent_sessions`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("session count=%d err=%v", count, err)
	}
}

func TestAgentSessionServicePrincipalPopulatesAudit(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	machineID := enrollMachine(t, st, "samplehub1", false)
	if _, err := st.DB().Exec(`UPDATE machine_registry
	 SET assigned_user_id='u-1',assigned_user_login='ted@example.com' WHERE machine_id=?`, machineID); err != nil {
		t.Fatal(err)
	}
	principal := operatorauth.Principal{
		SourceAddr: "100.64.0.7:1234", NodeStableID: "node-1", DeviceName: "laptop.example.ts.net.",
		TailnetUserID: "u-1", TailnetUserLogin: "ted@example.com",
		AuthMethod: "tailscale-localapi", AuthorizedCapability: "operator",
	}
	if _, err := New(st).OpenAgentSession(AgentSessionOpenRequest{
		SessionID: "session-1", MachineID: machineID, IdempotencyKey: "service-audit",
		Principal: principal, Actor: Actor{UserAgent: "test-agent", SourceKind: "test"},
	}); err != nil {
		t.Fatal(err)
	}
	var sourceAddr, whoNode, whoUser, authSubject, authNode, authMethod, authDecision string
	var authCapability sql.NullString
	if err := st.DB().QueryRow(`SELECT source_addr,who_node,who_user,auth_subject,auth_node_id,
	 auth_method,auth_decision,auth_capability FROM audit_log WHERE action=?`,
		store.AuditAgentSessionOpen).Scan(&sourceAddr, &whoNode, &whoUser, &authSubject,
		&authNode, &authMethod, &authDecision, &authCapability); err != nil {
		t.Fatal(err)
	}
	if sourceAddr != principal.SourceAddr || whoNode != principal.DeviceName ||
		whoUser != principal.TailnetUserLogin || authSubject != principal.StableSubject() ||
		authNode != principal.NodeStableID || authMethod != principal.AuthMethod ||
		authDecision != string(operatorauth.Authorized) || authCapability.String != principal.AuthorizedCapability {
		t.Fatalf("audit source=%q node=%q user=%q subject=%q auth_node=%q method=%q decision=%q capability=%q",
			sourceAddr, whoNode, whoUser, authSubject, authNode, authMethod, authDecision, authCapability.String)
	}
}

func TestAgentSessionServiceClosesAndListsOnlyOpenSessions(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	machineID := enrollMachine(t, st, "samplehub1", false)
	if _, err := st.DB().Exec(`UPDATE machine_registry
	 SET assigned_user_id='u-1',assigned_user_login='ted@example.com' WHERE machine_id=?`, machineID); err != nil {
		t.Fatal(err)
	}
	service := New(st)
	principal := operatorauth.Principal{
		SourceAddr: "100.64.0.7:1234", NodeStableID: "node-1", DeviceName: "laptop.example.ts.net.",
		TailnetUserID: "u-1", TailnetUserLogin: "ted@example.com", AuthMethod: "test",
	}
	for _, sessionID := range []string{"session-open", "session-close"} {
		if _, err := service.OpenAgentSession(AgentSessionOpenRequest{
			SessionID: sessionID, MachineID: machineID, IdempotencyKey: "open-" + sessionID,
			Principal: principal, Actor: Actor{SourceKind: "test"},
		}); err != nil {
			t.Fatal(err)
		}
	}
	closed, err := service.CloseAgentSession(AgentSessionCloseRequest{
		SessionID: "session-close", MachineID: machineID, Reason: "operator 離開終端",
		IdempotencyKey: "close-session", Principal: principal, Actor: Actor{SourceKind: "test"},
	})
	if err != nil || closed.ClosedAt == nil || closed.CloseReason != "operator 離開終端" {
		t.Fatalf("closed=%+v err=%v", closed, err)
	}
	open, err := service.ListOpenAgentSessions(machineID)
	if err != nil || len(open) != 1 || open[0].SessionID != "session-open" {
		t.Fatalf("open=%+v err=%v", open, err)
	}
}
