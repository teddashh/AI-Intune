package store

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func renameFixture(t *testing.T, name string) (*Store, string, string, time.Time) {
	t.Helper()
	st := newTestStore(t)
	issuedAt := time.Date(2026, 9, 13, 19, 0, 0, 0, time.UTC)
	st.nowFn = func() time.Time { return issuedAt }
	machineID, token, err := st.CreateEnrollTokenFor(name, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	st.nowFn = func() time.Time { return issuedAt.Add(10 * time.Minute) }
	return st, machineID, token, issuedAt
}

func renameRequest(machineID string, preview OperatorMachineRenamePreviewResult, key string) OperatorMachineRenameRequest {
	return OperatorMachineRenameRequest{
		MachineID: machineID, DisplayName: preview.DisplayName,
		ConfirmDisplayName: preview.CurrentDisplayName, PreviewDigest: preview.PreviewDigest,
		Reason: "align the registry label", IdempotencyKey: key,
		RequestDigest: "sha256:" + sha256Hex(key),
		Audit: AuditEntry{
			SourceAddr: "100.64.0.20", AuthSubject: "tailscale-user:20",
			AuthCapability: "example.com/cap/clawctl-admin", SourceKind: "operator-api",
		},
	}
}

func TestOperatorMachineRenamePreviewApplyAndReplayKeepIdentityAndPendingTicket(t *testing.T) {
	st, machineID, token, _ := renameFixture(t, "rename-before")
	preview, err := st.PreviewOperatorMachineRename(machineID, "rename-after")
	if err != nil || preview.MachineID != machineID || preview.CurrentDisplayName != "rename-before" ||
		preview.DisplayName != "rename-after" || preview.PreviewedAt.IsZero() ||
		!preview.MachineIDPreserved || !preview.AgentUnaffected || !preview.ExpectationKeyChanges ||
		!preview.PendingTokenLabelChanges || preview.PreviewDigest == "" {
		t.Fatalf("preview=%+v err=%v", preview, err)
	}
	var keys, audits int
	_ = st.db.QueryRow(`SELECT COUNT(*) FROM operator_idempotency`).Scan(&keys)
	_ = st.db.QueryRow(`SELECT COUNT(*) FROM audit_log`).Scan(&audits)
	if keys != 0 || audits != 0 {
		t.Fatalf("preview wrote keys=%d audits=%d", keys, audits)
	}

	req := renameRequest(machineID, preview, "rename-main-key")
	result, err := st.ApplyOperatorMachineRename(req)
	if err != nil || result.MachineID != machineID || result.PreviousDisplayName != "rename-before" ||
		result.DisplayName != "rename-after" || result.Replayed || !result.Audited ||
		!result.MachineIDPreserved || !result.AgentUnaffected || !result.ExpectationKeyChanged ||
		!result.PendingTokenLabelChanged || result.PreviewDigest != preview.PreviewDigest ||
		!result.AppliedAt.Equal(st.nowFn()) {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	machine, err := st.GetMachine(machineID)
	if err != nil || machine.MachineID != machineID || machine.DisplayName != "rename-after" {
		t.Fatalf("machine=%+v err=%v", machine, err)
	}
	var tokenName, tokenHash string
	if err := st.db.QueryRow(`SELECT display_name,token_hash FROM enrollment_tokens
	 WHERE used_by=? AND used_at IS NULL`, machineID).Scan(&tokenName, &tokenHash); err != nil {
		t.Fatal(err)
	}
	if tokenName != "rename-after" || tokenHash != hashToken(token) {
		t.Fatalf("pending ticket label/hash=%q/%q", tokenName, tokenHash)
	}
	entries, err := st.Audit(machineID, 10)
	if err != nil || len(entries) != 1 || entries[0].Action != AuditMachineRename || !entries[0].OK ||
		entries[0].Subject != "rename-after" || entries[0].Detail != "rename-before → rename-after" ||
		entries[0].IdempotencyKey != req.IdempotencyKey {
		t.Fatalf("audit=%+v err=%v", entries, err)
	}

	st.nowFn = func() time.Time { return result.AppliedAt.Add(24 * time.Hour) }
	replay, err := st.ApplyOperatorMachineRename(req)
	if err != nil || !replay.Replayed || !replay.Audited || replay.DisplayName != result.DisplayName ||
		!replay.AppliedAt.Equal(result.AppliedAt) {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM operator_idempotency`).Scan(&keys); err != nil || keys != 1 {
		t.Fatalf("receipt count=%d err=%v", keys, err)
	}
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE action=?`, AuditMachineRename).Scan(&audits); err != nil || audits != 2 {
		t.Fatalf("audit count=%d err=%v", audits, err)
	}
}

func TestOperatorMachineRenameRejectsDuplicateStaleAndMismatchedConfirmation(t *testing.T) {
	st, machineID, _, _ := renameFixture(t, "rename-target")
	otherID, _, err := st.CreateEnrollTokenFor("name-already-taken", time.Hour)
	if err != nil || otherID == machineID {
		t.Fatalf("second registry row id=%q err=%v", otherID, err)
	}
	if _, err := st.PreviewOperatorMachineRename(machineID, "name-already-taken"); !errors.Is(err, ErrMachineRenameNameTaken) {
		t.Fatalf("duplicate preview=%v", err)
	}
	if _, err := st.PreviewOperatorMachineRename(machineID, "rename-target"); !errors.Is(err, ErrMachineRenameUnchanged) {
		t.Fatalf("unchanged preview=%v", err)
	}

	preview, err := st.PreviewOperatorMachineRename(machineID, "rename-final")
	if err != nil {
		t.Fatal(err)
	}
	wrong := renameRequest(machineID, preview, "rename-wrong-confirm")
	wrong.ConfirmDisplayName = "some-other-name"
	wrong.RequestDigest = "sha256:wrong-confirm"
	if _, err := st.ApplyOperatorMachineRename(wrong); !errors.Is(err, ErrMachineRenameConfirmationMismatch) {
		t.Fatalf("wrong confirmation=%v", err)
	}
	machine, _ := st.GetMachine(machineID)
	if machine.DisplayName != "rename-target" {
		t.Fatalf("wrong confirmation changed name to %q", machine.DisplayName)
	}

	if _, err := st.db.Exec(`UPDATE machine_registry SET display_name='renamed-elsewhere' WHERE machine_id=?`, machineID); err != nil {
		t.Fatal(err)
	}
	stale := renameRequest(machineID, preview, "rename-stale")
	stale.ConfirmDisplayName = "renamed-elsewhere"
	stale.RequestDigest = "sha256:stale"
	if _, err := st.ApplyOperatorMachineRename(stale); !errors.Is(err, ErrMachineRenamePreviewStale) {
		t.Fatalf("stale preview=%v", err)
	}
	_, replayErr := st.ApplyOperatorMachineRename(stale)
	var replayed *OperatorRequestError
	if !errors.Is(replayErr, ErrMachineRenamePreviewStale) || !errors.As(replayErr, &replayed) ||
		!replayed.Replayed || !replayed.Audited {
		t.Fatalf("stale replay=%T %+v", replayErr, replayed)
	}
	machine, _ = st.GetMachine(machineID)
	if machine.DisplayName != "renamed-elsewhere" {
		t.Fatalf("stale preview changed name to %q", machine.DisplayName)
	}
}

func TestOperatorMachineRenameRechecksNameUniquenessAtApply(t *testing.T) {
	st, machineID, _, _ := renameFixture(t, "rename-race-target")
	preview, err := st.PreviewOperatorMachineRename(machineID, "claimed-after-preview")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.CreateEnrollTokenFor("claimed-after-preview", time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ApplyOperatorMachineRename(renameRequest(machineID, preview, "rename-name-race")); !errors.Is(err, ErrMachineRenameNameTaken) {
		t.Fatalf("apply accepted a name claimed after preview: %v", err)
	}
	machine, err := st.GetMachine(machineID)
	if err != nil || machine.DisplayName != "rename-race-target" {
		t.Fatalf("machine=%+v err=%v", machine, err)
	}
}

func TestOperatorMachineRenameRollsBackRegistryTicketAndReceiptWhenAuditFails(t *testing.T) {
	st, machineID, _, _ := renameFixture(t, "rename-atomic-before")
	preview, err := st.PreviewOperatorMachineRename(machineID, "rename-atomic-after")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`DROP TABLE audit_log`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ApplyOperatorMachineRename(renameRequest(machineID, preview, "rename-atomic-key")); err == nil || !strings.Contains(err.Error(), "audit") {
		t.Fatalf("missing audit failure=%v", err)
	}
	var registryName, ticketName string
	_ = st.db.QueryRow(`SELECT display_name FROM machine_registry WHERE machine_id=?`, machineID).Scan(&registryName)
	_ = st.db.QueryRow(`SELECT display_name FROM enrollment_tokens WHERE used_by=? AND used_at IS NULL`, machineID).Scan(&ticketName)
	var keys int
	_ = st.db.QueryRow(`SELECT COUNT(*) FROM operator_idempotency`).Scan(&keys)
	if registryName != "rename-atomic-before" || ticketName != "rename-atomic-before" || keys != 0 {
		t.Fatalf("failed transaction registry=%q ticket=%q receipts=%d", registryName, ticketName, keys)
	}
}

func TestOperatorMachineRenameRefusesCorruptCachedReceipt(t *testing.T) {
	st, machineID, _, _ := renameFixture(t, "rename-cache-before")
	preview, err := st.PreviewOperatorMachineRename(machineID, "rename-cache-after")
	if err != nil {
		t.Fatal(err)
	}
	req := renameRequest(machineID, preview, "rename-cache-key")
	result, err := st.ApplyOperatorMachineRename(req)
	if err != nil {
		t.Fatal(err)
	}
	result.MachineID = "different-machine"
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`UPDATE operator_idempotency SET response_json=? WHERE idempotency_key=?`,
		string(raw), req.IdempotencyKey); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ApplyOperatorMachineRename(req); err == nil || !strings.Contains(err.Error(), "cached machine rename receipt") {
		t.Fatalf("corrupt receipt replay=%v", err)
	}
	var audits int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE action=?`, AuditMachineRename).Scan(&audits); err != nil || audits != 1 {
		t.Fatalf("corrupt replay audit count=%d err=%v", audits, err)
	}
}

func TestOperatorMachineRenameAcceptsRetiredRegistryRows(t *testing.T) {
	st, machineID, _, _ := renameFixture(t, "retired-before")
	revision := int64(0)
	lifecycle, err := st.PreviewOperatorMachineLifecycle(OperatorMachineLifecyclePreviewRequest{
		MachineID: machineID, DesiredState: MachineLifecycleRetired, ExpectedRevision: &revision,
	})
	if err != nil {
		t.Fatal(err)
	}
	retired, err := st.ApplyOperatorMachineLifecycle(operatorLifecycleRequest(machineID, lifecycle, "retire-before-rename"))
	if err != nil || retired.RetiredAt == nil {
		t.Fatalf("retire=%+v err=%v", retired, err)
	}
	retiredAt := *retired.RetiredAt
	preview, err := st.PreviewOperatorMachineRename(machineID, "retired-after")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ApplyOperatorMachineRename(renameRequest(machineID, preview, "rename-retired-key")); err != nil {
		t.Fatal(err)
	}
	machine, err := st.GetMachine(machineID)
	if err != nil || machine.DisplayName != "retired-after" || machine.RetiredAt == nil || !machine.RetiredAt.Equal(retiredAt) {
		t.Fatalf("retired machine=%+v err=%v", machine, err)
	}
}
