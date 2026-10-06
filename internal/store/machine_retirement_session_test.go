package store

import (
	"slices"
	"strings"
	"testing"
	"time"
)

func TestRetiringMachineThroughRetireMachineClosesItsOpenSessions(t *testing.T) {
	st, machineID := retirementSessionFixture(t, "retire-machine")
	openAssignedUserSessions(t, st, machineID, "retire-machine-session")
	retiredAt := time.Date(2026, 9, 22, 11, 0, 0, 0, time.UTC)

	if err := st.RetireMachine(machineID, retiredAt); err != nil {
		t.Fatal(err)
	}
	assertSessionClosedByRetirement(t, st, "retire-machine-session", retiredAt)
}

func TestRetiringMachineThroughOperatorLifecycleClosesItsOpenSessions(t *testing.T) {
	st, machineID := retirementSessionFixture(t, "operator-lifecycle")
	openAssignedUserSessions(t, st, machineID, "operator-lifecycle-session")
	retiredAt := time.Date(2026, 9, 22, 11, 0, 0, 0, time.UTC)
	st.nowFn = func() time.Time { return retiredAt }

	result := retireThroughOperatorLifecycle(t, st, machineID, "operator-lifecycle-retire")
	if len(result.ClosedSessionIDs) != 1 || result.ClosedSessionIDs[0] != "operator-lifecycle-session" {
		t.Fatalf("closed session IDs=%v", result.ClosedSessionIDs)
	}
	assertSessionClosedByRetirement(t, st, "operator-lifecycle-session", retiredAt)
}

func TestRetiringMachineThroughUpsertMachineClosesItsOpenSessions(t *testing.T) {
	st, machineID := retirementSessionFixture(t, "upsert-machine")
	openAssignedUserSessions(t, st, machineID, "upsert-machine-session")
	retiredAt := time.Date(2026, 9, 22, 11, 0, 0, 0, time.UTC)
	machine, err := st.GetMachine(machineID)
	if err != nil {
		t.Fatal(err)
	}
	machine.RetiredAt = &retiredAt

	if err := st.UpsertMachine(machine); err != nil {
		t.Fatal(err)
	}
	assertSessionClosedByRetirement(t, st, "upsert-machine-session", retiredAt)
}

func TestRetiringMachineKeepsAnAlreadyClosedSessionsFirstClose(t *testing.T) {
	st, machineID := retirementSessionFixture(t, "first-close")
	openAssignedUserSessions(t, st, machineID, "already-closed")
	originalClosedAt := time.Date(2026, 9, 22, 10, 30, 0, 0, time.UTC)
	st.nowFn = func() time.Time { return originalClosedAt }
	const originalReason = "operator 已離開終端"
	if _, err := st.CloseAgentSession(closeAgentSessionRequest(
		"already-closed", machineID, "1", "assigned@example.com", originalReason, "close-before-retire")); err != nil {
		t.Fatal(err)
	}

	if err := st.RetireMachine(machineID, originalClosedAt.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	session := mustAgentSession(t, st, "already-closed")
	if session.ClosedAt == nil || !session.ClosedAt.Equal(originalClosedAt) || session.CloseReason != originalReason {
		t.Fatalf("already-closed session was overwritten: %+v", session)
	}
}

func TestUnretiringMachineNeitherClosesOpenSessionsNorReopensClosedSessions(t *testing.T) {
	st, machineID := retirementSessionFixture(t, "unretire")
	openAssignedUserSessions(t, st, machineID, "stays-closed", "stays-open")
	retiredAt := time.Date(2026, 9, 22, 11, 0, 0, 0, time.UTC)
	if err := st.RetireMachine(machineID, retiredAt); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`UPDATE agent_sessions SET closed_at=NULL,close_reason=NULL WHERE session_id=?`, "stays-open"); err != nil {
		t.Fatal(err)
	}

	if err := st.UnretireMachine(machineID, retiredAt.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if session := mustAgentSession(t, st, "stays-open"); session.ClosedAt != nil || session.CloseReason != "" {
		t.Fatalf("unretire closed an open session: %+v", session)
	}
	assertSessionClosedByRetirement(t, st, "stays-closed", retiredAt)
}

func TestUnretiringMachineThroughOperatorLifecycleNeitherClosesOpenSessionsNorReopensClosedSessions(t *testing.T) {
	st, machineID := retirementSessionFixture(t, "operator-unretire")
	openAssignedUserSessions(t, st, machineID, "operator-stays-closed", "operator-stays-open")
	retiredAt := time.Date(2026, 9, 22, 11, 0, 0, 0, time.UTC)
	st.nowFn = func() time.Time { return retiredAt }
	retireThroughOperatorLifecycle(t, st, machineID, "operator-unretire-retire")
	if _, err := st.DB().Exec(`UPDATE agent_sessions SET closed_at=NULL,close_reason=NULL WHERE session_id=?`, "operator-stays-open"); err != nil {
		t.Fatal(err)
	}

	st.nowFn = func() time.Time { return retiredAt.Add(time.Hour) }
	preview := lifecyclePreview(t, st, machineID, MachineLifecycleActive)
	result, err := st.ApplyOperatorMachineLifecycle(operatorLifecycleRequest(machineID, preview, "operator-unretire-restore"))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.ClosedSessionIDs) != 0 {
		t.Fatalf("unretire closed session IDs=%v", result.ClosedSessionIDs)
	}
	if session := mustAgentSession(t, st, "operator-stays-open"); session.ClosedAt != nil || session.CloseReason != "" {
		t.Fatalf("operator unretire closed an open session: %+v", session)
	}
	assertSessionClosedByRetirement(t, st, "operator-stays-closed", retiredAt)
}

func TestNoOpWritePreservingRetiredAtDoesNotCloseSessions(t *testing.T) {
	st, machineID := retirementSessionFixture(t, "retired-no-op")
	openAssignedUserSessions(t, st, machineID, "open-after-retirement")
	retiredAt := time.Date(2026, 9, 22, 11, 0, 0, 0, time.UTC)
	if err := st.RetireMachine(machineID, retiredAt); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`UPDATE agent_sessions SET closed_at=NULL,close_reason=NULL WHERE session_id=?`, "open-after-retirement"); err != nil {
		t.Fatal(err)
	}
	machine, err := st.GetMachine(machineID)
	if err != nil {
		t.Fatal(err)
	}
	machine.DisplayName = "retired-no-op-renamed"
	if err := st.UpsertMachine(machine); err != nil {
		t.Fatal(err)
	}

	if session := mustAgentSession(t, st, "open-after-retirement"); session.ClosedAt != nil || session.CloseReason != "" {
		t.Fatalf("no-op retired_at write closed a session: %+v", session)
	}
	got, err := st.GetMachine(machineID)
	if err != nil || got.RetiredAt == nil || !got.RetiredAt.Equal(retiredAt) {
		t.Fatalf("retired_at changed: machine=%+v err=%v", got, err)
	}
}

func TestRetiringMachineLeavesAnotherMachinesSessionsUntouched(t *testing.T) {
	st := newTestStore(t)
	targetID := agentSessionMachineAt(t, st, "retire-target", "1", "assigned@example.com",
		time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC))
	otherID := agentSessionMachineAt(t, st, "retire-other", "1", "assigned@example.com",
		time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC))
	openAssignedUserSessions(t, st, targetID, "target-session")
	openAssignedUserSessions(t, st, otherID, "other-session")
	retiredAt := time.Date(2026, 9, 22, 11, 0, 0, 0, time.UTC)

	if err := st.RetireMachine(targetID, retiredAt); err != nil {
		t.Fatal(err)
	}
	assertSessionClosedByRetirement(t, st, "target-session", retiredAt)
	if session := mustAgentSession(t, st, "other-session"); session.ClosedAt != nil || session.CloseReason != "" {
		t.Fatalf("other machine's session was changed: %+v", session)
	}
}

func TestReplayedRetireReceiptClosesNothing(t *testing.T) {
	st, machineID := retirementSessionFixture(t, "retire-replay")
	openAssignedUserSessions(t, st, machineID, "closed-by-original-retire")
	st.nowFn = func() time.Time { return time.Date(2026, 9, 22, 11, 0, 0, 0, time.UTC) }
	retirePreview := lifecyclePreview(t, st, machineID, MachineLifecycleRetired)
	retireRequest := operatorLifecycleRequest(machineID, retirePreview, "retire-replay-original")
	if _, err := st.ApplyOperatorMachineLifecycle(retireRequest); err != nil {
		t.Fatal(err)
	}

	st.nowFn = func() time.Time { return time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC) }
	restorePreview := lifecyclePreview(t, st, machineID, MachineLifecycleActive)
	if _, err := st.ApplyOperatorMachineLifecycle(operatorLifecycleRequest(machineID, restorePreview, "retire-replay-restore")); err != nil {
		t.Fatal(err)
	}
	openAssignedUserSessions(t, st, machineID, "opened-after-restore")

	replayed, err := st.ApplyOperatorMachineLifecycle(retireRequest)
	if err != nil {
		t.Fatal(err)
	}
	if !replayed.Replayed || len(replayed.ClosedSessionIDs) != 0 {
		t.Fatalf("replayed retire result=%+v", replayed)
	}
	if session := mustAgentSession(t, st, "opened-after-restore"); session.ClosedAt != nil || session.CloseReason != "" {
		t.Fatalf("replayed retire closed a session: %+v", session)
	}
}

func TestOperatorLifecycleResultReportsExactlyClosedSessionsInSessionIDOrder(t *testing.T) {
	st, machineID := retirementSessionFixture(t, "retire-result")
	openAssignedUserSessions(t, st, machineID, "session-z", "session-a", "session-m")
	st.nowFn = func() time.Time { return time.Date(2026, 9, 22, 10, 30, 0, 0, time.UTC) }
	if _, err := st.CloseAgentSession(closeAgentSessionRequest(
		"session-m", machineID, "1", "assigned@example.com", "operator 已關閉", "close-result-session")); err != nil {
		t.Fatal(err)
	}
	st.nowFn = func() time.Time { return time.Date(2026, 9, 22, 11, 0, 0, 0, time.UTC) }

	result := retireThroughOperatorLifecycle(t, st, machineID, "retire-result-apply")
	if want := []string{"session-a", "session-z"}; !slices.Equal(result.ClosedSessionIDs, want) {
		t.Fatalf("closed session IDs=%v, want %v", result.ClosedSessionIDs, want)
	}
}

func TestOperatorLifecycleClosedSessionIDsNeverEnterIdempotencyCache(t *testing.T) {
	st, machineID := retirementSessionFixture(t, "retire-cache")
	sessionIDs := []string{"cache-secret-session-b", "cache-secret-session-a"}
	openAssignedUserSessions(t, st, machineID, sessionIDs...)
	st.nowFn = func() time.Time { return time.Date(2026, 9, 22, 11, 0, 0, 0, time.UTC) }
	result := retireThroughOperatorLifecycle(t, st, machineID, "retire-cache-apply")
	if !slices.Equal(result.ClosedSessionIDs, []string{"cache-secret-session-a", "cache-secret-session-b"}) {
		t.Fatalf("closed session IDs=%v", result.ClosedSessionIDs)
	}

	var responseJSON string
	if err := st.DB().QueryRow(`SELECT response_json FROM operator_idempotency WHERE idempotency_key=?`,
		"retire-cache-apply").Scan(&responseJSON); err != nil {
		t.Fatal(err)
	}
	for _, sessionID := range sessionIDs {
		if strings.Contains(responseJSON, sessionID) {
			t.Fatalf("idempotency response contains closed session ID %q: %s", sessionID, responseJSON)
		}
	}
}

func retirementSessionFixture(t *testing.T, name string) (*Store, string) {
	t.Helper()
	st := newTestStore(t)
	machineID := agentSessionMachineAt(t, st, name, "1", "assigned@example.com",
		time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC))
	return st, machineID
}

func retireThroughOperatorLifecycle(t *testing.T, st *Store, machineID, key string) OperatorMachineLifecycleResult {
	t.Helper()
	preview := lifecyclePreview(t, st, machineID, MachineLifecycleRetired)
	result, err := st.ApplyOperatorMachineLifecycle(operatorLifecycleRequest(machineID, preview, key))
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func lifecyclePreview(t *testing.T, st *Store, machineID string, desired MachineLifecycleState) OperatorMachineLifecyclePreviewResult {
	t.Helper()
	machine, err := st.GetMachine(machineID)
	if err != nil {
		t.Fatal(err)
	}
	revision := machine.LifecycleRevision
	preview, err := st.PreviewOperatorMachineLifecycle(OperatorMachineLifecyclePreviewRequest{
		MachineID: machineID, DesiredState: desired, ExpectedRevision: &revision,
	})
	if err != nil {
		t.Fatal(err)
	}
	return preview
}

func assertSessionClosedByRetirement(t *testing.T, st *Store, sessionID string, retiredAt time.Time) {
	t.Helper()
	session := mustAgentSession(t, st, sessionID)
	if session.ClosedAt == nil || !session.ClosedAt.Equal(retiredAt.UTC().Truncate(time.Second)) ||
		session.CloseReason != AgentSessionCloseReasonMachineRetired {
		t.Fatalf("session was not closed by retirement: %+v", session)
	}
}
