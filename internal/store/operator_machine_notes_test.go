package store

import (
	"errors"
	"strings"
	"testing"
)

func notesRequest(machineID string, preview OperatorMachineNotesPreviewResult, key string) OperatorMachineNotesRequest {
	return OperatorMachineNotesRequest{
		MachineID: machineID, Notes: preview.Notes, ConfirmDisplayName: preview.DisplayName,
		PreviewDigest: preview.PreviewDigest, Reason: "record rack purpose", IdempotencyKey: key,
		RequestDigest: "sha256:" + sha256Hex(key),
		Audit: AuditEntry{
			SourceAddr: "100.64.0.20", AuthSubject: "tailscale-user:20",
			AuthCapability: "example.com/cap/clawctl-admin", SourceKind: "operator-api",
		},
	}
}

func TestOperatorMachineNotesPreviewApplyReplayAndClearWithoutCopyingTextToEvidence(t *testing.T) {
	st, machineID, _, _ := renameFixture(t, "notes-target")
	preview, err := st.PreviewOperatorMachineNotes(machineID, "GPU runner in rack two")
	if err != nil || preview.MachineID != machineID || preview.DisplayName != "notes-target" ||
		preview.CurrentNotes != "" || preview.Notes != "GPU runner in rack two" ||
		!preview.RegistryNotesChanged || !preview.MachineConfigurationUnchanged ||
		!preview.AgentUnaffected || preview.PreviewedAt.IsZero() || preview.PreviewDigest == "" {
		t.Fatalf("preview=%+v err=%v", preview, err)
	}
	var keys, audits int
	_ = st.db.QueryRow(`SELECT COUNT(*) FROM operator_idempotency`).Scan(&keys)
	_ = st.db.QueryRow(`SELECT COUNT(*) FROM audit_log`).Scan(&audits)
	if keys != 0 || audits != 0 {
		t.Fatalf("preview wrote keys=%d audits=%d", keys, audits)
	}

	req := notesRequest(machineID, preview, "notes-main-key")
	result, err := st.ApplyOperatorMachineNotes(req)
	if err != nil || result.MachineID != machineID || result.DisplayName != "notes-target" ||
		result.PreviousNotesPresent || !result.NotesPresent || result.Replayed || !result.Audited ||
		!result.RegistryNotesChanged || !result.MachineConfigurationUnchanged || !result.AgentUnaffected ||
		result.PreviewDigest != preview.PreviewDigest || !result.AppliedAt.Equal(st.nowFn().Truncate(0)) {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	machine, err := st.GetMachine(machineID)
	if err != nil || machine.Notes != preview.Notes {
		t.Fatalf("machine=%+v err=%v", machine, err)
	}
	var receipt, auditDetail string
	if err := st.db.QueryRow(`SELECT response_json FROM operator_idempotency WHERE idempotency_key=?`, req.IdempotencyKey).Scan(&receipt); err != nil {
		t.Fatal(err)
	}
	if err := st.db.QueryRow(`SELECT detail FROM audit_log WHERE action=?`, AuditMachineNotes).Scan(&auditDetail); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(receipt, preview.Notes) || strings.Contains(auditDetail, preview.Notes) ||
		auditDetail != "名冊備註已更新；機器設定與 agent 不變" {
		t.Fatalf("note leaked into durable evidence receipt=%q audit=%q", receipt, auditDetail)
	}
	replay, err := st.ApplyOperatorMachineNotes(req)
	if err != nil || !replay.Replayed || !replay.Audited || !replay.AppliedAt.Equal(result.AppliedAt) {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}

	clearPreview, err := st.PreviewOperatorMachineNotes(machineID, "")
	if err != nil || clearPreview.CurrentNotes != preview.Notes {
		t.Fatalf("clear preview=%+v err=%v", clearPreview, err)
	}
	cleared, err := st.ApplyOperatorMachineNotes(notesRequest(machineID, clearPreview, "notes-clear-key"))
	if err != nil || !cleared.PreviousNotesPresent || cleared.NotesPresent {
		t.Fatalf("clear=%+v err=%v", cleared, err)
	}
	machine, _ = st.GetMachine(machineID)
	if machine.Notes != "" {
		t.Fatalf("notes not cleared: %q", machine.Notes)
	}
}

func TestOperatorMachineNotesRejectsInvalidUnchangedConfirmationAndStalePreview(t *testing.T) {
	st, machineID, _, _ := renameFixture(t, "notes-guard")
	for _, value := range []string{
		" leading", "line\nbreak", "zero\u200bwidth", string([]byte{0xff}),
		strings.Repeat("x", OperatorMachineNotesMaxBytes+1),
	} {
		if _, err := st.PreviewOperatorMachineNotes(machineID, value); !errors.Is(err, ErrMachineNotesInvalid) {
			t.Fatalf("invalid %q: %v", value, err)
		}
	}
	if _, err := st.PreviewOperatorMachineNotes(machineID, ""); !errors.Is(err, ErrMachineNotesUnchanged) {
		t.Fatalf("unchanged=%v", err)
	}
	preview, err := st.PreviewOperatorMachineNotes(machineID, "desired")
	if err != nil {
		t.Fatal(err)
	}
	wrong := notesRequest(machineID, preview, "notes-wrong-confirm")
	wrong.ConfirmDisplayName = "another-machine"
	wrong.RequestDigest = "sha256:wrong-confirm"
	if _, err := st.ApplyOperatorMachineNotes(wrong); !errors.Is(err, ErrMachineNotesConfirmationMismatch) {
		t.Fatalf("confirmation=%v", err)
	}
	if _, err := st.db.Exec(`UPDATE machine_registry SET notes='changed elsewhere' WHERE machine_id=?`, machineID); err != nil {
		t.Fatal(err)
	}
	stale := notesRequest(machineID, preview, "notes-stale")
	if _, err := st.ApplyOperatorMachineNotes(stale); !errors.Is(err, ErrMachineNotesPreviewStale) {
		t.Fatalf("stale=%v", err)
	}
	machine, _ := st.GetMachine(machineID)
	if machine.Notes != "changed elsewhere" {
		t.Fatalf("stale request changed notes: %q", machine.Notes)
	}
}

func TestOperatorMachineNotesIsAtomicWithAuditAndWorksForRetiredMachine(t *testing.T) {
	st, machineID, _, _ := renameFixture(t, "notes-retired")
	revision := int64(0)
	lifecycle, err := st.PreviewOperatorMachineLifecycle(OperatorMachineLifecyclePreviewRequest{
		MachineID: machineID, DesiredState: MachineLifecycleRetired, ExpectedRevision: &revision,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ApplyOperatorMachineLifecycle(operatorLifecycleRequest(machineID, lifecycle, "retire-before-notes")); err != nil {
		t.Fatal(err)
	}
	preview, err := st.PreviewOperatorMachineNotes(machineID, "keep with retained history")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`DROP TABLE audit_log`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ApplyOperatorMachineNotes(notesRequest(machineID, preview, "notes-audit-fail")); err == nil ||
		!strings.Contains(err.Error(), "audit") {
		t.Fatalf("missing audit failure=%v", err)
	}
	machine, _ := st.GetMachine(machineID)
	var receipts int
	_ = st.db.QueryRow(`SELECT COUNT(*) FROM operator_idempotency`).Scan(&receipts)
	if machine.Notes != "" || machine.RetiredAt == nil || receipts != 1 {
		t.Fatalf("failed transaction machine=%+v receipts=%d", machine, receipts)
	}
}
