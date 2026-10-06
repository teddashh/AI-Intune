package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/model"
)

func TestOpenAddsAssignedUserColumnsToExistingRegistry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 22, 3, 0, 0, 0, time.UTC)
	id := mustEnroll(t, st, "samplehub1", now)
	if _, err := st.DB().Exec(`DROP TRIGGER ` + machineAssignedUserRevisionTrigger); err != nil {
		t.Fatal(err)
	}
	for _, column := range []string{"assigned_user_revision", "assigned_user_login", "assigned_user_id"} {
		if _, err := st.DB().Exec(`ALTER TABLE machine_registry DROP COLUMN ` + column); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	st, err = Open(path)
	if err != nil {
		t.Fatalf("舊名冊補指派欄位失敗：%v", err)
	}
	defer st.Close()
	have, err := columnSet(st.DB(), "machine_registry")
	if err != nil {
		t.Fatal(err)
	}
	for _, column := range []string{"assigned_user_id", "assigned_user_login", "assigned_user_revision"} {
		if !have[column] {
			t.Fatalf("缺 %s：%v", column, have)
		}
	}
	var userID, login sql.NullString
	var revision int64
	if err := st.DB().QueryRow(`SELECT assigned_user_id,assigned_user_login,assigned_user_revision
	 FROM machine_registry WHERE machine_id=?`, id).Scan(&userID, &login, &revision); err != nil {
		t.Fatal(err)
	}
	if userID.Valid || login.Valid || revision != 0 {
		t.Fatalf("既有列 id=%v login=%v revision=%d", userID, login, revision)
	}
	var triggerCount int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='trigger' AND name=?`,
		machineAssignedUserRevisionTrigger).Scan(&triggerCount); err != nil || triggerCount != 1 {
		t.Fatalf("assigned user revision trigger count=%d err=%v", triggerCount, err)
	}
}

func TestAssignedUserLedgerVariants(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	removeCheckpointedFixtureSidecars(t, path)
	if err := ValidateExistingLedger(path); err != nil {
		t.Fatalf("current registry: %v", err)
	}

	st, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`DROP TRIGGER ` + machineAssignedUserRevisionTrigger); err != nil {
		t.Fatal(err)
	}
	for _, column := range []string{"assigned_user_revision", "assigned_user_login", "assigned_user_id"} {
		if _, err := st.DB().Exec(`ALTER TABLE machine_registry DROP COLUMN ` + column); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.DB().Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	removeCheckpointedFixtureSidecars(t, path)
	if err := ValidateExistingLedger(path); err != nil {
		t.Fatalf("previous registry: %v", err)
	}
}

func TestAssignedUserStaleRevisionIsRejected(t *testing.T) {
	st := newTestStore(t)
	now := time.Date(2026, 9, 22, 3, 0, 0, 0, time.UTC)
	id := mustEnroll(t, st, "samplehub1", now)
	if _, err := applyAssignedUser(t, st, id, "samplehub1", "1000000000000001", "operator@example.com", "assign-1", 0); err != nil {
		t.Fatal(err)
	}
	_, err := applyAssignedUser(t, st, id, "samplehub1", "99", "other@example.com", "assign-stale", 0)
	var rejection *OperatorRequestError
	if !errors.As(err, &rejection) || rejection.Code != OperatorCodePreconditionFailed || !errors.Is(err, ErrPreconditionFailed) {
		t.Fatalf("stale revision err=%v", err)
	}
	assertAssignedUser(t, st, id, "1000000000000001", "operator@example.com", 1)
}

func TestAssignedUserRetiredMachineIsRejected(t *testing.T) {
	st := newTestStore(t)
	now := time.Date(2026, 9, 22, 3, 0, 0, 0, time.UTC)
	id := mustEnroll(t, st, "samplehub1", now)
	if _, err := applyAssignedUser(t, st, id, "samplehub1", "1000000000000001", "operator@example.com", "assign-1", 0); err != nil {
		t.Fatal(err)
	}
	if err := st.RetireMachine(id, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	_, err := applyAssignedUser(t, st, id, "samplehub1", "99", "other@example.com", "assign-retired", 1)
	var rejection *OperatorRequestError
	if !errors.As(err, &rejection) || rejection.Code != OperatorCodeMachineRetired || !errors.Is(err, ErrMachineRetired) {
		t.Fatalf("retired err=%v", err)
	}
	assertAssignedUser(t, st, id, "1000000000000001", "operator@example.com", 1)
}

func TestAssignedUserSameValueDoesNotAdvanceRevision(t *testing.T) {
	st := newTestStore(t)
	now := time.Date(2026, 9, 22, 3, 0, 0, 0, time.UTC)
	id := mustEnroll(t, st, "samplehub1", now)
	first, err := applyAssignedUser(t, st, id, "samplehub1", "1000000000000001", "operator@example.com", "assign-1", 0)
	if err != nil || first.Revision != 1 || first.Replayed {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	second, err := applyAssignedUser(t, st, id, "samplehub1", "1000000000000001", "operator@example.com", "assign-2", 1)
	if err != nil || second.Replayed || second.Revision != 1 {
		t.Fatalf("same value=%+v err=%v", second, err)
	}
	assertAssignedUser(t, st, id, "1000000000000001", "operator@example.com", 1)
}

func TestAssignedUserClearSetsNullAndAdvancesRevision(t *testing.T) {
	st := newTestStore(t)
	now := time.Date(2026, 9, 22, 3, 0, 0, 0, time.UTC)
	id := mustEnroll(t, st, "samplehub1", now)
	if _, err := applyAssignedUser(t, st, id, "samplehub1", "1000000000000001", "operator@example.com", "assign-1", 0); err != nil {
		t.Fatal(err)
	}
	cleared, err := applyAssignedUser(t, st, id, "samplehub1", AssignedUserNone, "", "clear-1", 1)
	if err != nil || cleared.Revision != 2 || cleared.UserID != "" || cleared.UserLogin != "" || cleared.PreviousUserLogin != "operator@example.com" {
		t.Fatalf("clear=%+v err=%v", cleared, err)
	}
	var userID, login sql.NullString
	var revision int64
	if err := st.DB().QueryRow(`SELECT assigned_user_id,assigned_user_login,assigned_user_revision
	 FROM machine_registry WHERE machine_id=?`, id).Scan(&userID, &login, &revision); err != nil {
		t.Fatal(err)
	}
	if userID.Valid || login.Valid || revision != 2 {
		t.Fatalf("cleared row id=%v login=%v revision=%d", userID, login, revision)
	}
	m, err := st.GetMachine(id)
	if err != nil || m.MachineID == "" {
		t.Fatal(err)
	}
	if m.AssignedUserID != "" || m.AssignedUserLogin != "" || m.AssignedUserRevision != 2 {
		t.Fatalf("machine=%+v", m)
	}
}

func TestAssignedUserAuditRollsBackWithTheWrite(t *testing.T) {
	st := newTestStore(t)
	now := time.Date(2026, 9, 22, 3, 0, 0, 0, time.UTC)
	id := mustEnroll(t, st, "samplehub1", now)
	if _, err := st.DB().Exec(`DROP TABLE audit_log`); err != nil {
		t.Fatal(err)
	}
	_, err := applyAssignedUser(t, st, id, "samplehub1", "1000000000000001", "operator@example.com", "audit-must-commit", 0)
	if err == nil || !strings.Contains(err.Error(), "audit") {
		t.Fatalf("missing audit table did not fail closed: %v", err)
	}
	var userID, login sql.NullString
	var revision int64
	if scanErr := st.DB().QueryRow(`SELECT assigned_user_id,assigned_user_login,assigned_user_revision
	 FROM machine_registry WHERE machine_id=?`, id).Scan(&userID, &login, &revision); scanErr != nil {
		t.Fatal(scanErr)
	}
	if userID.Valid || login.Valid || revision != 0 {
		t.Fatalf("write survived missing audit id=%v login=%v revision=%d", userID, login, revision)
	}
	var requests int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM operator_idempotency WHERE idempotency_key='audit-must-commit'`).Scan(&requests); err != nil {
		t.Fatal(err)
	}
	if requests != 0 {
		t.Fatalf("idempotency result committed without audit: %d", requests)
	}
}

func TestCheckinAndRosterEditDoNotResetAssignedUser(t *testing.T) {
	st := newTestStore(t)
	now := time.Date(2026, 9, 22, 3, 0, 0, 0, time.UTC)
	id := mustEnroll(t, st, "samplehub1", now)
	if _, err := applyAssignedUser(t, st, id, "samplehub1", "1000000000000001", "operator@example.com", "assign-1", 0); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertMachine(Machine{MachineID: id, DisplayName: "samplehub1-renamed", Expected: true}); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordCheckin(id, model.Checkin{SchemaVersion: model.SchemaVersion, SentAt: now}, now); err != nil {
		t.Fatal(err)
	}
	m, err := st.GetMachine(id)
	if err != nil {
		t.Fatal(err)
	}
	if m.DisplayName != "samplehub1-renamed" || m.AssignedUserID != "1000000000000001" ||
		m.AssignedUserLogin != "operator@example.com" || m.AssignedUserRevision != 1 {
		t.Fatalf("roster edit or check-in reset assignment: %+v", m)
	}
}

func TestLegacyAssignedUserWriterBumpsRevisionOnce(t *testing.T) {
	st := newTestStore(t)
	now := time.Date(2026, 9, 22, 3, 0, 0, 0, time.UTC)
	id := mustEnroll(t, st, "samplehub1", now)
	if _, err := applyAssignedUser(t, st, id, "samplehub1", "1000000000000001", "operator@example.com", "assign-1", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`UPDATE machine_registry SET assigned_user_id='99',assigned_user_login='other@example.com' WHERE machine_id=?`, id); err != nil {
		t.Fatal(err)
	}
	assertAssignedUser(t, st, id, "99", "other@example.com", 2)
	if _, err := st.DB().Exec(`UPDATE machine_registry SET display_name='cnode-renamed' WHERE machine_id=?`, id); err != nil {
		t.Fatal(err)
	}
	assertAssignedUser(t, st, id, "99", "other@example.com", 2)
	_, err := applyAssignedUser(t, st, id, "cnode-renamed", "7", "third@example.com", "stale-after-legacy", 1)
	var rejection *OperatorRequestError
	if !errors.As(err, &rejection) || rejection.Code != OperatorCodePreconditionFailed {
		t.Fatalf("stale request after legacy writer err=%v", err)
	}
	assertAssignedUser(t, st, id, "99", "other@example.com", 2)
}

func TestAssignedUserRejectsFreeTextIdentity(t *testing.T) {
	st := newTestStore(t)
	now := time.Date(2026, 9, 22, 3, 0, 0, 0, time.UTC)
	id := mustEnroll(t, st, "samplehub1", now)
	_, err := applyAssignedUser(t, st, id, "samplehub1", "ted", "operator@example.com", "free-text", 0)
	var rejection *OperatorRequestError
	if !errors.As(err, &rejection) || rejection.Code != OperatorCodeBadAssignedUser || !errors.Is(err, ErrBadAssignedUser) {
		t.Fatalf("free text err=%v", err)
	}
	assertAssignedUser(t, st, id, "", "", 0)
}

func applyAssignedUser(t *testing.T, st *Store, machineID, name, userID, login, key string, revision int64) (OperatorMachineAssignedUserResult, error) {
	t.Helper()
	return st.ApplyOperatorMachineAssignedUser(OperatorMachineAssignedUserRequest{
		MachineID: machineID, UserID: userID, UserLogin: login, ExpectedRevision: &revision,
		ConfirmDisplayName: name, IdempotencyKey: key, RequestDigest: "sha256:" + key,
		Audit: AuditEntry{SourceAddr: "local-test"},
	})
}

func assertAssignedUser(t *testing.T, st *Store, machineID, userID, login string, revision int64) {
	t.Helper()
	m, err := st.GetMachine(machineID)
	if err != nil {
		t.Fatal(err)
	}
	if m.AssignedUserID != userID || m.AssignedUserLogin != login || m.AssignedUserRevision != revision {
		t.Fatalf("assigned user=%s %s revision=%d, machine=%+v", userID, login, revision, m)
	}
}
