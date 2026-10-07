package operator

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/operatorauth"
	"github.com/teddashh/AI-Intune/internal/store"
	"github.com/teddashh/AI-Intune/internal/tailnet"
)

func TestChangeMachineAssignedUserRejectsMissingUserAndUnavailableSourceDifferently(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	id := enrollMachine(t, st, "samplehub1", false)
	revision := int64(0)
	req := MachineAssignedUserRequest{
		MachineID: id, UserID: "1000000000000001", ExpectedRevision: &revision,
		ConfirmDisplayName: "samplehub1", IdempotencyKey: "assign-ted",
		Actor: Actor{SourceAddr: "local-test"},
	}

	unavailable := &scriptedTailnet{statuses: []tailnet.Status{{Unavailable: "這台 Hub 上沒有 tailscale 指令，所以沒有第二份機器清單可以對照"}}}
	_, unavailableErr := NewWithTailnet(st, unavailable).ChangeMachineAssignedUser(context.Background(), req)
	var unavailableRejection *store.OperatorRequestError
	if !errors.As(unavailableErr, &unavailableRejection) ||
		unavailableRejection.Code != store.OperatorCodeTailnetSourceUnavailable ||
		!errors.Is(unavailableErr, store.ErrTailnetSourceUnavailable) ||
		unavailableRejection.Detail != unavailable.statuses[0].Unavailable {
		t.Fatalf("unavailable=%v", unavailableErr)
	}

	missing := &scriptedTailnet{statuses: []tailnet.Status{assignedUserStatus("99", "other@example.com", "Other")}}
	_, missingErr := NewWithTailnet(st, missing).ChangeMachineAssignedUser(context.Background(), req)
	var missingRejection *store.OperatorRequestError
	if !errors.As(missingErr, &missingRejection) ||
		missingRejection.Code != store.OperatorCodeAssignedUserNotInRoster ||
		!errors.Is(missingErr, store.ErrAssignedUserNotInRoster) {
		t.Fatalf("missing=%v", missingErr)
	}
	if errors.Is(unavailableErr, store.ErrAssignedUserNotInRoster) || errors.Is(missingErr, store.ErrTailnetSourceUnavailable) {
		t.Fatalf("unavailable=%v missing=%v", unavailableErr, missingErr)
	}
	if unavailableRejection.Code == missingRejection.Code || unavailableRejection.Detail == missingRejection.Detail {
		t.Fatalf("the two refusals are the same result: %s %q", unavailableRejection.Code, unavailableRejection.Detail)
	}
	machine, err := st.GetMachine(id)
	if err != nil || machine.AssignedUserID != "" || machine.AssignedUserRevision != 0 {
		t.Fatalf("refusal wrote assignment: %+v err=%v", machine, err)
	}

	present := &scriptedTailnet{statuses: []tailnet.Status{assignedUserStatus("1000000000000001", "operator@example.com", "Sample Operator")}}
	result, err := NewWithTailnet(st, present).ChangeMachineAssignedUser(context.Background(), req)
	if err != nil || result.Replayed || result.UserID != "1000000000000001" || result.UserLogin != "operator@example.com" || result.Revision != 1 {
		t.Fatalf("retry after the user appeared=%+v err=%v", result, err)
	}
}

func TestChangeMachineAssignedUserClearDoesNotNeedTailnet(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	id := enrollMachine(t, st, "samplehub1", false)
	revision := int64(0)
	source := &scriptedTailnet{statuses: []tailnet.Status{assignedUserStatus("1000000000000001", "operator@example.com", "Sample Operator")}}
	service := NewWithTailnet(st, source)
	assigned, err := service.ChangeMachineAssignedUser(context.Background(), MachineAssignedUserRequest{
		MachineID: id, UserID: "1000000000000001", ExpectedRevision: &revision,
		ConfirmDisplayName: "samplehub1", IdempotencyKey: "assign-ted",
		Actor: Actor{SourceAddr: "local-test"},
	})
	if err != nil || assigned.Revision != 1 {
		t.Fatalf("assign=%+v err=%v", assigned, err)
	}
	callsAfterAssign := source.calls
	clearedRevision := int64(1)
	cleared, err := New(st).ChangeMachineAssignedUser(context.Background(), MachineAssignedUserRequest{
		MachineID: id, UserID: store.AssignedUserNone, ExpectedRevision: &clearedRevision,
		ConfirmDisplayName: "samplehub1", IdempotencyKey: "clear-ted",
		Actor: Actor{SourceAddr: "local-test"},
	})
	if err != nil || cleared.Revision != 2 || cleared.UserID != "" || cleared.UserLogin != "" {
		t.Fatalf("clear=%+v err=%v", cleared, err)
	}
	if source.calls != callsAfterAssign {
		t.Fatalf("clear consulted tailnet: calls %d → %d", callsAfterAssign, source.calls)
	}
	machine, err := st.GetMachine(id)
	if err != nil || machine.AssignedUserID != "" || machine.AssignedUserLogin != "" || machine.AssignedUserRevision != 2 {
		t.Fatalf("machine=%+v err=%v", machine, err)
	}
}

func TestNilTailnetSourceIsNotAMissingUser(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	id := enrollMachine(t, st, "samplehub1", false)
	revision := int64(0)
	_, err = New(st).ChangeMachineAssignedUser(context.Background(), MachineAssignedUserRequest{
		MachineID: id, UserID: "1000000000000001", ExpectedRevision: &revision,
		ConfirmDisplayName: "samplehub1", IdempotencyKey: "no-source",
		Actor: Actor{SourceAddr: "local-test"},
	})
	var rejection *store.OperatorRequestError
	if !errors.As(err, &rejection) || rejection.Code != store.OperatorCodeTailnetSourceUnavailable ||
		errors.Is(err, store.ErrAssignedUserNotInRoster) {
		t.Fatalf("nil source=%v", err)
	}
}

type scriptedTailnet struct {
	statuses []tailnet.Status
	calls    int
}

func (s *scriptedTailnet) Get(context.Context) tailnet.Status {
	s.calls++
	if len(s.statuses) == 0 {
		return tailnet.Status{Unavailable: "沒有 tailnet 來源，不能核對要指派的使用者"}
	}
	status := s.statuses[0]
	if len(s.statuses) > 1 {
		s.statuses = s.statuses[1:]
	}
	return status
}

func assignedUserStatus(userID, login, display string) tailnet.Status {
	return tailnet.Status{
		Available: true, ObservedAt: time.Date(2026, 9, 22, 3, 0, 0, 0, time.UTC),
		Self: tailnet.Peer{StableID: "self", Hostname: "hub", UserID: userID, UserLogin: login},
		TailnetUsers: []tailnet.TailnetUser{{
			UserID: userID, Login: login, DisplayName: display,
		}},
	}
}

func TestLocalAccountAssignmentAndTerminalAudit(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	a, err := st.CreateFirstAdmin("admin", "long admin password")
	if err != nil {
		t.Fatal(err)
	}
	id := enrollMachine(t, st, "samplehub1", false)
	service := New(st)
	revision := int64(0)
	_, err = service.ChangeMachineAssignedUser(context.Background(), MachineAssignedUserRequest{MachineID: id, UserID: "local:" + a.AccountID, ExpectedRevision: &revision, ConfirmDisplayName: "samplehub1", IdempotencyKey: "assign-local", Actor: Actor{SourceAddr: "127.0.0.1"}})
	if err != nil {
		t.Fatal(err)
	}
	p := operatorauth.Principal{TailnetUserID: "local:" + a.AccountID, TailnetUserLogin: a.Username, AuthMethod: operatorauth.AuthMethodLocalAccountSession, SourceAddr: "127.0.0.1", NodeStableID: "local-session", AuthorizedCapability: "local/cap/clawctl-operate"}
	_, err = service.OpenAgentSession(AgentSessionOpenRequest{MachineID: id, SessionID: "local-terminal", IdempotencyKey: "local-open", Principal: p})
	if err != nil {
		t.Fatal(err)
	}
	var subject string
	if err = st.DB().QueryRow(`SELECT auth_subject FROM audit_log WHERE action=?`, store.AuditAgentSessionOpen).Scan(&subject); err != nil {
		t.Fatal(err)
	}
	if subject != "local-user:"+a.AccountID {
		t.Fatal(subject)
	}
	if _, err = st.AgentSessionForOperator(id, "local-terminal", a.AccountID); err == nil {
		t.Fatal("unprefixed identity matched local operator")
	}
	if _, err = st.AgentSessionForOperator(id, "local-terminal", p.TailnetUserID); err != nil {
		t.Fatal(err)
	}
}
