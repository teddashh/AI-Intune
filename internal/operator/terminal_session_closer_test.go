package operator

import (
	"context"
	"database/sql"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/teddashh/AI-Intune/internal/store"
)

func TestChangeMachineLifecycleClosesTerminalSessionsAfterCommit(t *testing.T) {
	t.Run("two sessions once, replay closes nothing", func(t *testing.T) {
		st, service, machineID := newTerminalCloserService(t)
		openTwoTerminalSessions(t, st, machineID)
		calls := &[][]string{}
		service.SetTerminalSessionCloser(recordingTerminalCloser(t, st, calls))

		request := retireLifecycleRequest(t, service, machineID, "retire-live", 0)
		result, err := service.ChangeMachineLifecycle(request)
		if err != nil || result.Replayed || result.State != store.MachineLifecycleRetired {
			t.Fatalf("retire=%+v err=%v", result, err)
		}
		if len(*calls) != 1 || !reflect.DeepEqual((*calls)[0], []string{"session-a", "session-b"}) {
			t.Fatalf("closer calls=%v", *calls)
		}

		replay, err := service.ChangeMachineLifecycle(request)
		if err != nil || !replay.Replayed {
			t.Fatalf("replay=%+v err=%v", replay, err)
		}
		if len(*calls) != 1 {
			t.Fatalf("replay called the closer: %v", *calls)
		}
	})

	t.Run("stale revision closes nothing", func(t *testing.T) {
		st, service, machineID := newTerminalCloserService(t)
		openTwoTerminalSessions(t, st, machineID)
		calls := &[][]string{}
		service.SetTerminalSessionCloser(recordingTerminalCloser(t, st, calls))
		request := retireLifecycleRequest(t, service, machineID, "retire-stale", 0)
		request.ExpectedRevision = int64Ptr(4)

		if _, err := service.ChangeMachineLifecycle(request); err == nil {
			t.Fatal("stale revision was applied")
		}
		if len(*calls) != 0 {
			t.Fatalf("stale revision called the closer: %v", *calls)
		}
		if open := countOpenTerminalSessions(t, st, machineID); open != 2 {
			t.Fatalf("open sessions=%d", open)
		}
	})

	t.Run("nil closer does not panic", func(t *testing.T) {
		st, service, machineID := newTerminalCloserService(t)
		openTwoTerminalSessions(t, st, machineID)
		result, err := service.ChangeMachineLifecycle(retireLifecycleRequest(t, service, machineID, "retire-nil", 0))
		if err != nil || result.State != store.MachineLifecycleRetired {
			t.Fatalf("retire=%+v err=%v", result, err)
		}
		if open := countOpenTerminalSessions(t, st, machineID); open != 0 {
			t.Fatalf("open sessions=%d", open)
		}
	})
}

func TestChangeMachineAssignedUserClosesTerminalSessionsAfterCommit(t *testing.T) {
	t.Run("two sessions once, replay closes nothing", func(t *testing.T) {
		st, service, machineID := newAssignedTerminalCloserService(t)
		openTwoTerminalSessions(t, st, machineID)
		calls := &[][]string{}
		service.SetTerminalSessionCloser(recordingTerminalCloser(t, st, calls))
		request := reassignTerminalRequest(machineID, "reassign-live", assignedUserRevision(t, st, machineID))

		result, err := service.ChangeMachineAssignedUser(context.Background(), request)
		if err != nil || result.Replayed || result.UserID != "99" {
			t.Fatalf("reassign=%+v err=%v", result, err)
		}
		if len(*calls) != 1 || !reflect.DeepEqual((*calls)[0], []string{"session-a", "session-b"}) {
			t.Fatalf("closer calls=%v", *calls)
		}

		replay, err := service.ChangeMachineAssignedUser(context.Background(), request)
		if err != nil || !replay.Replayed {
			t.Fatalf("replay=%+v err=%v", replay, err)
		}
		if len(*calls) != 1 {
			t.Fatalf("replay called the closer: %v", *calls)
		}
	})

	t.Run("stale revision closes nothing", func(t *testing.T) {
		st, service, machineID := newAssignedTerminalCloserService(t)
		openTwoTerminalSessions(t, st, machineID)
		calls := &[][]string{}
		service.SetTerminalSessionCloser(recordingTerminalCloser(t, st, calls))

		if _, err := service.ChangeMachineAssignedUser(context.Background(), reassignTerminalRequest(machineID, "reassign-stale", assignedUserRevision(t, st, machineID)+4)); err == nil {
			t.Fatal("stale revision was applied")
		}
		if len(*calls) != 0 {
			t.Fatalf("stale revision called the closer: %v", *calls)
		}
		if open := countOpenTerminalSessions(t, st, machineID); open != 2 {
			t.Fatalf("open sessions=%d", open)
		}
	})

	t.Run("nil closer does not panic", func(t *testing.T) {
		st, service, machineID := newAssignedTerminalCloserService(t)
		openTwoTerminalSessions(t, st, machineID)
		result, err := service.ChangeMachineAssignedUser(context.Background(), reassignTerminalRequest(machineID, "reassign-nil", assignedUserRevision(t, st, machineID)))
		if err != nil || result.UserID != "99" {
			t.Fatalf("reassign=%+v err=%v", result, err)
		}
		if open := countOpenTerminalSessions(t, st, machineID); open != 0 {
			t.Fatalf("open sessions=%d", open)
		}
	})
}

func newTerminalCloserService(t *testing.T) (*store.Store, *Service, string) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st, New(st), enrollMachine(t, st, "closer-machine", false)
}

func newAssignedTerminalCloserService(t *testing.T) (*store.Store, *Service, string) {
	t.Helper()
	st, _, machineID := newTerminalCloserService(t)
	source := &scriptedTailnet{}
	source.statuses = append(source.statuses, assignedUserStatus("99", "next@example.com", "Next"))
	return st, NewWithTailnet(st, source), machineID
}

func openTwoTerminalSessions(t *testing.T, st *store.Store, machineID string) {
	t.Helper()
	if _, err := st.DB().Exec(`UPDATE machine_registry
		SET assigned_user_id='42', assigned_user_login='ted@example.com'
		WHERE machine_id=?`, machineID); err != nil {
		t.Fatal(err)
	}
	for _, sessionID := range []string{"session-a", "session-b"} {
		if _, err := st.OpenAgentSession(store.OpenAgentSessionRequest{
			SessionID: sessionID, MachineID: machineID,
			OperatorTailnetUserID: "42", OperatorTailnetUserLogin: "ted@example.com",
			IdempotencyKey: "open-" + sessionID, RequestDigest: "sha256:" + sessionID,
			Audit: store.AuditEntry{SourceAddr: "local-test", AuthMethod: "test"},
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func recordingTerminalCloser(t *testing.T, st *store.Store, calls *[][]string) func([]string) {
	t.Helper()
	return func(ids []string) {
		*calls = append(*calls, append([]string(nil), ids...))
		for _, id := range ids {
			var closedAt sql.NullString
			err := st.DB().QueryRow(`SELECT closed_at FROM agent_sessions WHERE session_id=?`, id).Scan(&closedAt)
			if err != nil || !closedAt.Valid {
				t.Errorf("session %s closed_at=%v err=%v inside closer", id, closedAt, err)
			}
		}
	}
}

func retireLifecycleRequest(t *testing.T, service *Service, machineID, key string, revision int64) MachineLifecycleRequest {
	t.Helper()
	preview, err := service.PreviewMachineLifecycle(MachineLifecyclePreviewRequest{
		MachineID: machineID, DesiredState: MachineLifecycleStateRetired, ExpectedRevision: &revision,
	})
	if err != nil {
		t.Fatal(err)
	}
	expected := revision
	return MachineLifecycleRequest{
		MachineID: machineID, DesiredState: MachineLifecycleStateRetired,
		ExpectedRevision: &expected, ConfirmDisplayName: preview.DisplayName,
		PreviewDigest: preview.PreviewDigest, Reason: "retire the live terminal",
		IdempotencyKey: key, Actor: Actor{SourceAddr: "local-test", SourceKind: SourceKindOperatorAPI},
	}
}

func reassignTerminalRequest(machineID, key string, revision int64) MachineAssignedUserRequest {
	expected := revision
	return MachineAssignedUserRequest{
		MachineID: machineID, UserID: "99", ExpectedRevision: &expected,
		ConfirmDisplayName: "closer-machine", IdempotencyKey: key,
		Actor: Actor{SourceAddr: "local-test", SourceKind: SourceKindOperatorAPI},
	}
}

func assignedUserRevision(t *testing.T, st *store.Store, machineID string) int64 {
	t.Helper()
	var revision int64
	if err := st.DB().QueryRow(`SELECT assigned_user_revision FROM machine_registry WHERE machine_id=?`, machineID).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	return revision
}

func countOpenTerminalSessions(t *testing.T, st *store.Store, machineID string) int {
	t.Helper()
	var open int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM agent_sessions
		WHERE machine_id=? AND closed_at IS NULL`, machineID).Scan(&open); err != nil {
		t.Fatal(err)
	}
	return open
}

func int64Ptr(value int64) *int64 { return &value }
