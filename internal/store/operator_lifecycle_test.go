package store

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/deploy"
	"github.com/teddashh/AI-Intune/internal/model"
)

func newOperatorLifecycleFixture(t *testing.T, name string) (*Store, string, string, string, time.Time) {
	t.Helper()
	st := newTestStore(t)
	createdAt := time.Date(2026, 9, 8, 13, 0, 0, 0, time.UTC)
	st.nowFn = func() time.Time { return createdAt }
	enrollToken, err := st.CreateEnrollToken(name, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	machineID, agentToken, err := st.RedeemEnrollToken(enrollToken, model.EnrollRequest{
		SchemaVersion: model.SchemaVersion, EnrollToken: enrollToken,
		Hostname: name + "-host", OS: "linux", Arch: "amd64", UnixUser: "operator-test",
	}, createdAt.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	pendingToken := "pending-lifecycle-token-" + name
	if _, err := st.db.Exec(`INSERT INTO enrollment_tokens
	 (token_hash,display_name,created_at,expires_at,used_by)
	 VALUES (?,?,?,?,?)`, hashToken(pendingToken), name, fmtTime(createdAt.Add(2*time.Minute)),
		fmtTime(createdAt.Add(time.Hour)), machineID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`UPDATE machine_registry SET channel='canary',channel_revision=channel_revision+1
	 WHERE machine_id=?`, machineID); err != nil {
		t.Fatal(err)
	}
	st.nowFn = func() time.Time { return createdAt.Add(10 * time.Minute) }
	return st, machineID, agentToken, pendingToken, createdAt
}

func operatorLifecycleRequest(machineID string, preview OperatorMachineLifecyclePreviewResult, key string) OperatorMachineLifecycleRequest {
	revision := preview.LifecycleRevision
	return OperatorMachineLifecycleRequest{
		MachineID: machineID, DesiredState: preview.DesiredState, ExpectedRevision: &revision,
		ConfirmDisplayName: preview.DisplayName, PreviewDigest: preview.PreviewDigest,
		Reason: "operator-approved lifecycle change", IdempotencyKey: key,
		RequestDigest: "sha256:" + sha256Hex(key),
		Audit: AuditEntry{SourceAddr: "100.64.0.20", AuthSubject: "tailscale-user:20",
			AuthCapability: "example.com/cap/clawctl-admin", SourceKind: "operator-api"},
	}
}

func TestOperatorMachineLifecycleReadPreviewApplyReplayRestoreAndNoOp(t *testing.T) {
	st, machineID, agentToken, pendingToken, createdAt := newOperatorLifecycleFixture(t, "lifecycle-main")
	read, err := st.OperatorMachineLifecycle(machineID)
	if err != nil || read.State != MachineLifecycleActive || read.LifecycleRevision != 0 ||
		!read.InDenominator || read.Channel != "canary" || read.ChannelRevision != 1 ||
		!read.AgentCredentialPresent || !read.AgentAuthenticationAllowed ||
		read.PendingEnrollmentTokenCount != 1 || read.PendingEnrollmentTokenExpiredCount != 0 ||
		!read.PendingEnrollmentRedemptionAllowed || read.ActiveJobCount != 0 ||
		!read.RegistryRetained || !read.HistoryPreserved {
		t.Fatalf("read=%+v err=%v", read, err)
	}
	revision := read.LifecycleRevision
	preview, err := st.PreviewOperatorMachineLifecycle(OperatorMachineLifecyclePreviewRequest{
		MachineID: machineID, DesiredState: MachineLifecycleRetired, ExpectedRevision: &revision,
	})
	if err != nil || preview.PreviewDigest == "" || preview.CurrentState != MachineLifecycleActive ||
		preview.DesiredState != MachineLifecycleRetired || preview.DenominatorDelta != -1 ||
		!preview.InDenominatorBefore || preview.InDenominatorAfter ||
		!preview.AgentAuthenticationBefore || preview.AgentAuthenticationAfter ||
		!preview.PendingEnrollmentRedemptionBefore || preview.PendingEnrollmentRedemptionAfter ||
		len(preview.Blockers) != 0 || !preview.ChannelPreserved ||
		!preview.RegistryRetained || !preview.HistoryPreserved {
		t.Fatalf("preview=%+v err=%v", preview, err)
	}
	if got := countRows(t, st, `SELECT COUNT(*) FROM operator_idempotency`); got != 0 {
		t.Fatalf("preview wrote idempotency rows=%d", got)
	}
	if got := countRows(t, st, `SELECT COUNT(*) FROM audit_log`); got != 0 {
		t.Fatalf("preview wrote audits=%d", got)
	}

	req := operatorLifecycleRequest(machineID, preview, "lifecycle-retire")
	result, err := st.ApplyOperatorMachineLifecycle(req)
	if err != nil || result.Replayed || !result.Audited || !result.Changed || result.NoOp ||
		result.PreviousState != MachineLifecycleActive || result.State != MachineLifecycleRetired ||
		result.LifecycleRevision != 1 || result.TransitionEventID == nil || result.RetiredAt == nil ||
		!result.RetiredAt.Equal(st.nowFn()) || result.DenominatorDelta != -1 ||
		result.AgentAuthenticationAfter || result.PendingEnrollmentRedemptionAfter {
		t.Fatalf("retire result=%+v err=%v", result, err)
	}
	if _, err := st.AuthenticateAgent(agentToken); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("retired bearer auth=%v", err)
	}
	if _, _, err := st.RedeemEnrollToken(pendingToken, model.EnrollRequest{
		SchemaVersion: model.SchemaVersion, EnrollToken: pendingToken,
		Hostname: "blocked", OS: "linux", Arch: "amd64",
	}, st.nowFn()); !errors.Is(err, ErrTokenRetired) {
		t.Fatalf("retired pending redemption=%v", err)
	}
	machine, err := st.GetMachine(machineID)
	if err != nil || machine.LifecycleRevision != 1 || machine.Channel != "canary" ||
		machine.ChannelRevision != 1 || machine.RetiredAt == nil {
		t.Fatalf("retired projection=%+v err=%v", machine, err)
	}
	for query, want := range map[string]int{
		`SELECT COUNT(*) FROM machine_registry WHERE machine_id='` + machineID + `'`:                   1,
		`SELECT COUNT(*) FROM enrollment_tokens WHERE used_by='` + machineID + `' AND used_at IS NULL`: 1,
		`SELECT COUNT(*) FROM machine_registry_lifecycle_events WHERE machine_id='` + machineID + `'`:  1,
		`SELECT COUNT(*) FROM audit_log WHERE action='machine-lifecycle' AND outcome='ok'`:             1,
		`SELECT COUNT(*) FROM operator_idempotency WHERE operation LIKE 'machine-lifecycle:v1:%'`:      1,
	} {
		if got := countRows(t, st, query); got != want {
			t.Fatalf("query=%q got=%d want=%d", query, got, want)
		}
	}

	st.nowFn = func() time.Time { return createdAt.Add(20 * time.Minute) }
	replay, err := st.ApplyOperatorMachineLifecycle(req)
	if err != nil || !replay.Replayed || replay.LifecycleRevision != result.LifecycleRevision ||
		replay.TransitionEventID == nil || *replay.TransitionEventID != *result.TransitionEventID {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}
	if got := countRows(t, st, `SELECT COUNT(*) FROM machine_registry_lifecycle_events WHERE machine_id=?`, machineID); got != 1 {
		t.Fatalf("replay appended lifecycle event count=%d", got)
	}

	revision = replay.LifecycleRevision
	restorePreview, err := st.PreviewOperatorMachineLifecycle(OperatorMachineLifecyclePreviewRequest{
		MachineID: machineID, DesiredState: MachineLifecycleActive, ExpectedRevision: &revision,
	})
	if err != nil || restorePreview.DenominatorDelta != 1 || restorePreview.AgentAuthenticationAfter != true ||
		!restorePreview.PendingEnrollmentRedemptionAfter {
		t.Fatalf("restore preview=%+v err=%v", restorePreview, err)
	}
	restore, err := st.ApplyOperatorMachineLifecycle(operatorLifecycleRequest(machineID, restorePreview, "lifecycle-restore"))
	if err != nil || !restore.Changed || restore.NoOp || restore.State != MachineLifecycleActive ||
		restore.LifecycleRevision != 2 || restore.RetiredAt != nil || restore.TransitionEventID == nil {
		t.Fatalf("restore=%+v err=%v", restore, err)
	}
	if got, err := st.AuthenticateAgent(agentToken); err != nil || got != machineID {
		t.Fatalf("restored bearer got=%q err=%v", got, err)
	}
	historicalReplay, err := st.ApplyOperatorMachineLifecycle(req)
	if err != nil || !historicalReplay.Replayed || historicalReplay.State != MachineLifecycleRetired ||
		historicalReplay.LifecycleRevision != 1 {
		t.Fatalf("historical retire replay=%+v err=%v", historicalReplay, err)
	}
	machine, err = st.GetMachine(machineID)
	if err != nil || machine.RetiredAt != nil || machine.LifecycleRevision != 2 {
		t.Fatalf("historical replay re-applied transition machine=%+v err=%v", machine, err)
	}

	revision = restore.LifecycleRevision
	noOpPreview, err := st.PreviewOperatorMachineLifecycle(OperatorMachineLifecyclePreviewRequest{
		MachineID: machineID, DesiredState: MachineLifecycleActive, ExpectedRevision: &revision,
	})
	if err != nil {
		t.Fatal(err)
	}
	noOp, err := st.ApplyOperatorMachineLifecycle(operatorLifecycleRequest(machineID, noOpPreview, "lifecycle-noop"))
	if err != nil || noOp.Changed || !noOp.NoOp || noOp.LifecycleRevision != 2 || noOp.TransitionEventID != nil {
		t.Fatalf("no-op=%+v err=%v", noOp, err)
	}
	if got := countRows(t, st, `SELECT COUNT(*) FROM machine_registry_lifecycle_events WHERE machine_id=?`, machineID); got != 2 {
		t.Fatalf("no-op appended lifecycle event count=%d", got)
	}
}

func TestOperatorMachineLifecyclePreviewCountsOnlyThisMachinesOpenAgentSessions(t *testing.T) {
	st := newTestStore(t)
	now := time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC)
	targetID := agentSessionMachineAt(t, st, "lifecycle-session-target", "1", "assigned@example.com", now)
	otherID := agentSessionMachineAt(t, st, "lifecycle-session-other", "1", "assigned@example.com", now)
	openAssignedUserSessions(t, st, targetID, "target-open-a", "target-open-b", "target-closed")
	openAssignedUserSessions(t, st, otherID, "other-open")
	if _, err := st.CloseAgentSession(closeAgentSessionRequest(
		"target-closed", targetID, "1", "assigned@example.com", "operator 已關閉", "close-target-session")); err != nil {
		t.Fatal(err)
	}

	preview := lifecyclePreview(t, st, targetID, MachineLifecycleRetired)
	if preview.OpenAgentSessionCount != 2 {
		t.Fatalf("open agent session count=%d, want 2", preview.OpenAgentSessionCount)
	}
}

func TestOperatorMachineLifecycleDTOIgnoresLegacyExpected(t *testing.T) {
	st, machineID, _, _, _ := newOperatorLifecycleFixture(t, "lifecycle-legacy-false")
	if _, err := st.DB().Exec(`UPDATE machine_registry SET expected=0 WHERE machine_id=?`, machineID); err != nil {
		t.Fatal(err)
	}
	read, err := st.OperatorMachineLifecycle(machineID)
	if err != nil || !read.InDenominator {
		t.Fatalf("active lifecycle read=%+v err=%v", read, err)
	}
	revision := read.LifecycleRevision
	preview, err := st.PreviewOperatorMachineLifecycle(OperatorMachineLifecyclePreviewRequest{
		MachineID: machineID, DesiredState: MachineLifecycleRetired, ExpectedRevision: &revision,
	})
	if err != nil || !preview.InDenominatorBefore || preview.InDenominatorAfter || preview.DenominatorDelta != -1 {
		t.Fatalf("lifecycle preview=%+v err=%v", preview, err)
	}
	result, err := st.ApplyOperatorMachineLifecycle(operatorLifecycleRequest(machineID, preview, "lifecycle-legacy-false"))
	if err != nil || result.InDenominatorBefore != true || result.InDenominatorAfter || result.DenominatorDelta != -1 {
		t.Fatalf("lifecycle apply=%+v err=%v", result, err)
	}
	for name, value := range map[string]any{"read": read, "preview": preview, "apply": result} {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), `"expected"`) {
			t.Fatalf("%s lifecycle DTO exposed legacy expected: %s", name, raw)
		}
	}
	var receipt string
	if err := st.DB().QueryRow(`SELECT response_json FROM operator_idempotency WHERE idempotency_key=?`,
		"lifecycle-legacy-false").Scan(&receipt); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(receipt, `"expected"`) || !strings.Contains(receipt, `"schema_version":"v2"`) {
		t.Fatalf("new lifecycle receipt did not use canonical v2 shape: %s", receipt)
	}
}

func TestOperatorMachineLifecycleV1ReceiptRemainsStrictlyReplayable(t *testing.T) {
	appliedAt := time.Date(2026, 9, 8, 14, 0, 0, 0, time.UTC)
	retiredAt, eventID, legacyExpected := appliedAt, int64(17), true
	previewDigest := "sha256:" + strings.Repeat("a", 64)
	receipt := operatorMachineLifecycleReceipt{
		SchemaVersion: operatorMachineLifecycleLegacyVersion,
		MachineID:     "machine-1", DisplayName: "samplehub1",
		PreviousState: MachineLifecycleActive, State: MachineLifecycleRetired,
		LifecycleRevision: 5, Changed: true, NoOp: false,
		TransitionEventID: &eventID, RetiredAt: &retiredAt, AppliedAt: appliedAt,
		LegacyExpected: &legacyExpected,
		OperatorMachineLifecycleImpact: OperatorMachineLifecycleImpact{
			InDenominatorBefore: true, InDenominatorAfter: false, DenominatorDelta: -1,
			RegistryRetained: true, HistoryPreserved: true, Channel: "canary",
			ChannelRevision: 2, ChannelPreserved: true,
			AgentCredentialPresent: true, AgentAuthenticationBefore: true,
			PendingEnrollmentTokenCount: 1, PendingEnrollmentRedemptionBefore: true,
			Blockers: []MachineLifecycleBlocker{},
		},
		PreviewDigest: previewDigest,
	}
	raw, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeOperatorMachineLifecycleReceipt(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	revision := int64(4)
	request := OperatorMachineLifecycleRequest{
		MachineID: "machine-1", DesiredState: MachineLifecycleRetired,
		ExpectedRevision: &revision, PreviewDigest: previewDigest,
	}
	if err := validateOperatorMachineLifecycleReceipt(decoded, request, fmtTime(appliedAt)); err != nil {
		t.Fatalf("v1 lifecycle receipt no longer validates: %v", err)
	}
	reencoded, err := json.Marshal(decoded)
	if err != nil {
		t.Fatal(err)
	}
	if string(reencoded) != string(raw) || !strings.Contains(string(raw), `"expected":true`) {
		t.Fatalf("v1 lifecycle receipt did not preserve canonical bytes: before=%s after=%s", raw, reencoded)
	}
}

func TestOperatorMachineLifecycleReceiptJSONExcludesOpenAgentSessionCount(t *testing.T) {
	raw, err := json.Marshal(operatorMachineLifecycleReceipt{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "open_agent_session_count") {
		t.Fatalf("lifecycle receipt bound preview-time open session count: %s", raw)
	}
}

func TestOperatorMachineLifecycleActiveJobBlockerIsPreviewedAtomicAndReplayable(t *testing.T) {
	st, machineID, _, _, createdAt := newOperatorLifecycleFixture(t, "lifecycle-job")
	if _, err := st.db.Exec(`INSERT INTO jobs
	 (job_id,machine_id,revision,state,execution_timeout,created_at)
	 VALUES (?,?,?,?,?,?)`, "lifecycle-active-job", machineID, 1, deploy.Running, 900,
		fmtTime(createdAt.Add(5*time.Minute))); err != nil {
		t.Fatal(err)
	}
	revision := int64(0)
	preview, err := st.PreviewOperatorMachineLifecycle(OperatorMachineLifecyclePreviewRequest{
		MachineID: machineID, DesiredState: MachineLifecycleRetired, ExpectedRevision: &revision,
	})
	if err != nil || preview.ActiveJobCount != 1 || len(preview.Blockers) != 1 ||
		preview.Blockers[0] != MachineLifecycleBlockerNonterminalJobs {
		t.Fatalf("blocked preview=%+v err=%v", preview, err)
	}
	req := operatorLifecycleRequest(machineID, preview, "lifecycle-job-block")
	if _, err := st.ApplyOperatorMachineLifecycle(req); !errors.Is(err, ErrMachineActiveJob) {
		t.Fatalf("active-job apply=%v", err)
	}
	if got := countRows(t, st, `SELECT COUNT(*) FROM machine_registry_lifecycle_events WHERE machine_id=?`, machineID); got != 0 {
		t.Fatalf("blocked apply event count=%d", got)
	}
	if _, err := st.db.Exec(`UPDATE jobs SET state=?,terminal_at=? WHERE job_id=?`,
		deploy.Succeeded, fmtTime(st.nowFn()), "lifecycle-active-job"); err != nil {
		t.Fatal(err)
	}
	_, replayErr := st.ApplyOperatorMachineLifecycle(req)
	var rejection *OperatorRequestError
	if !errors.Is(replayErr, ErrMachineActiveJob) || !errors.As(replayErr, &rejection) || !rejection.Replayed {
		t.Fatalf("active-job rejection replay=%T %v", replayErr, replayErr)
	}
	if got := countRows(t, st, `SELECT COUNT(*) FROM machine_registry_lifecycle_events WHERE machine_id=?`, machineID); got != 0 {
		t.Fatalf("rejected replay event count=%d", got)
	}
}

func TestOperatorMachineLifecyclePreviewDigestBindsEveryAuthorityAndImpactCoordinate(t *testing.T) {
	retiredAt := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	base := operatorMachineLifecycleSnapshot{
		MachineID: "machine-1", DisplayName: "samplehub1", State: MachineLifecycleRetired,
		Revision: 4, RetiredAt: &retiredAt,
		Channel: "canary", ChannelRevision: 3,
		AgentCredential: true, agentCredentialHash: strings.Repeat("a", 64),
		PendingCount: 2, PendingExpiredCount: 1, pendingIdentityDigest: strings.Repeat("b", 64),
		ActiveJobCount: 0,
	}
	digest := func(snapshot operatorMachineLifecycleSnapshot, desired MachineLifecycleState) string {
		impact := operatorMachineLifecycleImpact(snapshot, desired)
		return operatorMachineLifecyclePreviewDigest(snapshot, desired, impact)
	}
	want := digest(base, MachineLifecycleActive)
	tests := []struct {
		name string
		edit func(*operatorMachineLifecycleSnapshot)
	}{
		{"machine id", func(s *operatorMachineLifecycleSnapshot) { s.MachineID += "x" }},
		{"display name", func(s *operatorMachineLifecycleSnapshot) { s.DisplayName += "x" }},
		{"current state", func(s *operatorMachineLifecycleSnapshot) { s.State = MachineLifecycleActive; s.RetiredAt = nil }},
		{"revision", func(s *operatorMachineLifecycleSnapshot) { s.Revision++ }},
		{"retired at", func(s *operatorMachineLifecycleSnapshot) { value := retiredAt.Add(time.Second); s.RetiredAt = &value }},
		{"channel", func(s *operatorMachineLifecycleSnapshot) { s.Channel = "stable" }},
		{"channel revision", func(s *operatorMachineLifecycleSnapshot) { s.ChannelRevision++ }},
		{"credential presence", func(s *operatorMachineLifecycleSnapshot) { s.AgentCredential = false }},
		{"credential identity", func(s *operatorMachineLifecycleSnapshot) { s.agentCredentialHash = strings.Repeat("c", 64) }},
		{"pending count", func(s *operatorMachineLifecycleSnapshot) { s.PendingCount++ }},
		{"pending expired", func(s *operatorMachineLifecycleSnapshot) { s.PendingExpiredCount++ }},
		{"pending identity", func(s *operatorMachineLifecycleSnapshot) { s.pendingIdentityDigest = strings.Repeat("d", 64) }},
		{"active jobs", func(s *operatorMachineLifecycleSnapshot) { s.ActiveJobCount++ }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			changed := base
			tc.edit(&changed)
			if got := digest(changed, MachineLifecycleActive); got == want {
				t.Fatalf("%s did not change preview digest %q", tc.name, got)
			}
		})
	}
	if got := digest(base, MachineLifecycleRetired); got == want {
		t.Fatalf("desired state did not change preview digest %q", got)
	}
}

func TestOperatorMachineLifecyclePreviewDigestDoesNotBindOpenAgentSessionCountToPreventDenialOfRevocation(t *testing.T) {
	snapshot := operatorMachineLifecycleSnapshot{
		MachineID: "machine-1", DisplayName: "samplehub1", State: MachineLifecycleActive,
		Revision: 4, AgentCredential: true, agentCredentialHash: strings.Repeat("a", 64),
		pendingIdentityDigest: strings.Repeat("b", 64), OpenAgentSessionCount: 1,
	}
	impact := operatorMachineLifecycleImpact(snapshot, MachineLifecycleRetired)
	want := operatorMachineLifecyclePreviewDigest(snapshot, MachineLifecycleRetired, impact)
	snapshot.OpenAgentSessionCount++
	got := operatorMachineLifecyclePreviewDigest(snapshot, MachineLifecycleRetired,
		operatorMachineLifecycleImpact(snapshot, MachineLifecycleRetired))
	if got != want {
		t.Fatalf("open agent session count changed preview digest: %q != %q", got, want)
	}
}

func TestOperatorMachineLifecyclePreviewDigestPendingIdentityIgnoresInsertionOrder(t *testing.T) {
	createdAt := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	expiresAt := createdAt.Add(time.Hour)
	now := createdAt.Add(10 * time.Minute)
	machineID := "machine-pending-order"
	displayName := "pending-order"
	hashes := []string{
		strings.Repeat("c", 64),
		strings.Repeat("a", 64),
		strings.Repeat("b", 64),
	}
	orders := [][]int{{0, 1, 2}, {1, 2, 0}}
	previews := make([]OperatorMachineLifecyclePreviewResult, len(orders))
	revision := int64(0)
	for i, order := range orders {
		st := newTestStore(t)
		st.nowFn = func() time.Time { return now }
		if _, err := st.db.Exec(`INSERT INTO machine_registry (machine_id,display_name,created_at)
		 VALUES (?,?,?)`, machineID, displayName, fmtTime(createdAt)); err != nil {
			t.Fatal(err)
		}
		for _, index := range order {
			if _, err := st.db.Exec(`INSERT INTO enrollment_tokens
			 (token_hash,display_name,created_at,expires_at,used_by)
			 VALUES (?,?,?,?,?)`, hashes[index], displayName, fmtTime(createdAt), fmtTime(expiresAt), machineID); err != nil {
				t.Fatal(err)
			}
		}
		preview, err := st.PreviewOperatorMachineLifecycle(OperatorMachineLifecyclePreviewRequest{
			MachineID: machineID, DesiredState: MachineLifecycleRetired, ExpectedRevision: &revision,
		})
		if err != nil {
			t.Fatal(err)
		}
		if preview.PendingEnrollmentTokenCount != int64(len(hashes)) {
			t.Fatalf("order %v pending token count=%d want=%d", order, preview.PendingEnrollmentTokenCount, len(hashes))
		}
		previews[i] = preview
	}
	if previews[0].PreviewDigest != previews[1].PreviewDigest {
		t.Fatalf("preview digest depends on pending-token insertion order: %q != %q",
			previews[0].PreviewDigest, previews[1].PreviewDigest)
	}
}

func TestOperatorMachineLifecycleRejectsUnsafeIntentAndReplaysDecision(t *testing.T) {
	tests := []struct {
		name string
		edit func(*OperatorMachineLifecycleRequest)
		want error
	}{
		{"reason", func(r *OperatorMachineLifecycleRequest) { r.Reason = " \t" }, ErrReasonRequired},
		{"confirmation", func(r *OperatorMachineLifecycleRequest) { r.ConfirmDisplayName = "wrong" }, ErrConfirmationMismatch},
		{"revision", func(r *OperatorMachineLifecycleRequest) { bad := int64(99); r.ExpectedRevision = &bad }, ErrPreconditionFailed},
		{"preview", func(r *OperatorMachineLifecycleRequest) { r.PreviewDigest = "" }, ErrPreviewRequired},
		{"stale preview", func(r *OperatorMachineLifecycleRequest) { r.PreviewDigest = "sha256:" + strings.Repeat("0", 64) }, ErrPreviewStale},
		{"bad state", func(r *OperatorMachineLifecycleRequest) { r.DesiredState = "deleted" }, ErrBadLifecycle},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			st, machineID, _, _, _ := newOperatorLifecycleFixture(t, "reject-"+strings.ReplaceAll(tc.name, " ", "-"))
			revision := int64(0)
			preview, err := st.PreviewOperatorMachineLifecycle(OperatorMachineLifecyclePreviewRequest{
				MachineID: machineID, DesiredState: MachineLifecycleRetired, ExpectedRevision: &revision,
			})
			if err != nil {
				t.Fatal(err)
			}
			req := operatorLifecycleRequest(machineID, preview, "reject-"+tc.name)
			tc.edit(&req)
			_, firstErr := st.ApplyOperatorMachineLifecycle(req)
			if !errors.Is(firstErr, tc.want) {
				t.Fatalf("first err=%v want=%v", firstErr, tc.want)
			}
			_, replayErr := st.ApplyOperatorMachineLifecycle(req)
			var rejection *OperatorRequestError
			if !errors.Is(replayErr, tc.want) || !errors.As(replayErr, &rejection) || !rejection.Replayed {
				t.Fatalf("replay err=%T %v", replayErr, replayErr)
			}
			m, err := st.GetMachine(machineID)
			if err != nil || m.RetiredAt != nil || m.LifecycleRevision != 0 {
				t.Fatalf("rejection mutated machine=%+v err=%v", m, err)
			}
		})
	}
}

func TestOperatorMachineLifecycleRollsBackOnLedgerFailure(t *testing.T) {
	for _, tc := range []struct {
		name    string
		trigger string
	}{
		{"audit", `CREATE TRIGGER fail_lifecycle_ledger BEFORE INSERT ON audit_log
		 WHEN NEW.action='machine-lifecycle' BEGIN SELECT RAISE(ABORT,'forced audit failure'); END`},
		{"receipt", `CREATE TRIGGER fail_lifecycle_ledger BEFORE INSERT ON operator_idempotency
		 WHEN NEW.operation LIKE 'machine-lifecycle:%' BEGIN SELECT RAISE(ABORT,'forced receipt failure'); END`},
		{"event", `CREATE TRIGGER fail_lifecycle_ledger BEFORE INSERT ON machine_registry_lifecycle_events
		 BEGIN SELECT RAISE(ABORT,'forced event failure'); END`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, machineID, _, _, _ := newOperatorLifecycleFixture(t, "rollback-"+tc.name)
			revision := int64(0)
			preview, err := st.PreviewOperatorMachineLifecycle(OperatorMachineLifecyclePreviewRequest{
				MachineID: machineID, DesiredState: MachineLifecycleRetired, ExpectedRevision: &revision,
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := st.db.Exec(tc.trigger); err != nil {
				t.Fatal(err)
			}
			if _, err := st.ApplyOperatorMachineLifecycle(operatorLifecycleRequest(machineID, preview, "rollback-"+tc.name)); err == nil {
				t.Fatal("apply unexpectedly succeeded")
			}
			m, err := st.GetMachine(machineID)
			if err != nil || m.RetiredAt != nil || m.LifecycleRevision != 0 {
				t.Fatalf("failed transaction mutated projection=%+v err=%v", m, err)
			}
			if got := countRows(t, st, `SELECT COUNT(*) FROM machine_registry_lifecycle_events WHERE machine_id=?`, machineID); got != 0 {
				t.Fatalf("failed transaction event count=%d", got)
			}
			if got := countRows(t, st, `SELECT COUNT(*) FROM operator_idempotency WHERE operation LIKE 'machine-lifecycle:%'`); got != 0 {
				t.Fatalf("failed transaction receipt count=%d", got)
			}
		})
	}
}

func TestOperatorMachineLifecycleCorruptCachedReceiptFailsClosed(t *testing.T) {
	st, machineID, _, _, _ := newOperatorLifecycleFixture(t, "lifecycle-corrupt")
	revision := int64(0)
	preview, err := st.PreviewOperatorMachineLifecycle(OperatorMachineLifecyclePreviewRequest{
		MachineID: machineID, DesiredState: MachineLifecycleRetired, ExpectedRevision: &revision,
	})
	if err != nil {
		t.Fatal(err)
	}
	req := operatorLifecycleRequest(machineID, preview, "lifecycle-corrupt-key")
	if _, err := st.ApplyOperatorMachineLifecycle(req); err != nil {
		t.Fatal(err)
	}
	var raw string
	if err := st.db.QueryRow(`SELECT response_json FROM operator_idempotency WHERE idempotency_key=?`,
		req.IdempotencyKey).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	duplicate := strings.TrimSuffix(raw, "}") + `,"machine_id":"` + machineID + `"}`
	if _, err := st.db.Exec(`UPDATE operator_idempotency SET response_json=? WHERE idempotency_key=?`,
		duplicate, req.IdempotencyKey); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ApplyOperatorMachineLifecycle(req); err == nil ||
		!strings.Contains(err.Error(), "idempotency cache invalid") {
		t.Fatalf("corrupt cache replay=%v", err)
	}
	if got := countRows(t, st, `SELECT COUNT(*) FROM audit_log WHERE idempotency_key=?`, req.IdempotencyKey); got != 1 {
		t.Fatalf("corrupt replay appended audit count=%d", got)
	}
}

func TestOperatorMachineLifecycleConcurrentPreviewOnlyOneTransitionWins(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.db")
	first, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Close() })
	createdAt := time.Date(2026, 9, 8, 16, 0, 0, 0, time.UTC)
	first.nowFn = func() time.Time { return createdAt }
	machineID, _, err := first.CreateEnrollTokenFor("concurrent-lifecycle", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })
	first.nowFn = func() time.Time { return createdAt.Add(time.Minute) }
	second.nowFn = first.nowFn
	revision := int64(0)
	preview, err := first.PreviewOperatorMachineLifecycle(OperatorMachineLifecyclePreviewRequest{
		MachineID: machineID, DesiredState: MachineLifecycleRetired, ExpectedRevision: &revision,
	})
	if err != nil {
		t.Fatal(err)
	}
	requests := []OperatorMachineLifecycleRequest{
		operatorLifecycleRequest(machineID, preview, "concurrent-lifecycle-a"),
		operatorLifecycleRequest(machineID, preview, "concurrent-lifecycle-b"),
	}
	stores := []*Store{first, second}
	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for i := range stores {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, err := stores[i].ApplyOperatorMachineLifecycle(requests[i])
			errs <- err
		}(i)
	}
	close(start)
	wg.Wait()
	close(errs)
	var successes, rejected int
	for err := range errs {
		if err == nil {
			successes++
		} else if errors.Is(err, ErrPreconditionFailed) {
			rejected++
		} else {
			t.Fatalf("unexpected concurrent result: %v", err)
		}
	}
	if successes != 1 || rejected != 1 {
		t.Fatalf("successes=%d rejected=%d", successes, rejected)
	}
	machine, err := first.GetMachine(machineID)
	if err != nil || machine.RetiredAt == nil || machine.LifecycleRevision != 1 {
		t.Fatalf("concurrent projection=%+v err=%v", machine, err)
	}
	if got := countRows(t, first, `SELECT COUNT(*) FROM machine_registry_lifecycle_events WHERE machine_id=?`, machineID); got != 1 {
		t.Fatalf("concurrent event count=%d", got)
	}
}

func TestOperatorMachineLifecycleRevisionExhaustionAllowsNoOpButRejectsTransition(t *testing.T) {
	st, machineID, _, _, _ := newOperatorLifecycleFixture(t, "lifecycle-revision-limit")
	if _, err := st.db.Exec(`DROP TRIGGER IF EXISTS ` + machineLifecycleRevisionOnlyTrigger); err != nil {
		t.Fatal(err)
	}
	limit := MaxMachineLifecycleRevision - 1
	if _, err := st.db.Exec(`UPDATE machine_registry SET lifecycle_revision=? WHERE machine_id=?`, limit, machineID); err != nil {
		t.Fatal(err)
	}
	if err := ensureMachineLifecycleRevisionTriggers(st.db); err != nil {
		t.Fatal(err)
	}

	read, err := st.OperatorMachineLifecycle(machineID)
	if err != nil || read.State != MachineLifecycleActive || read.LifecycleRevision != limit {
		t.Fatalf("read at revision limit=%+v err=%v", read, err)
	}
	noOpPreview, err := st.PreviewOperatorMachineLifecycle(OperatorMachineLifecyclePreviewRequest{
		MachineID: machineID, DesiredState: MachineLifecycleActive, ExpectedRevision: &limit,
	})
	if err != nil {
		t.Fatalf("no-op preview at revision limit: %v", err)
	}
	noOp, err := st.ApplyOperatorMachineLifecycle(operatorLifecycleRequest(machineID, noOpPreview, "lifecycle-limit-noop"))
	if err != nil || noOp.Changed || !noOp.NoOp || noOp.LifecycleRevision != limit {
		t.Fatalf("no-op at revision limit=%+v err=%v", noOp, err)
	}

	if _, err := st.PreviewOperatorMachineLifecycle(OperatorMachineLifecyclePreviewRequest{
		MachineID: machineID, DesiredState: MachineLifecycleRetired, ExpectedRevision: &limit,
	}); !errors.Is(err, ErrLifecycleRevisionLimit) {
		t.Fatalf("transition preview at revision limit=%v", err)
	}
	if got := countRows(t, st, `SELECT COUNT(*) FROM machine_registry_lifecycle_events WHERE machine_id=?`, machineID); got != 0 {
		t.Fatalf("revision-limit operations appended lifecycle event count=%d", got)
	}
}

func TestMachineLifecycleRevisionGuardMatchesMaxRevisionAndRejectsLimitTransition(t *testing.T) {
	st, machineID, _, _, createdAt := newOperatorLifecycleFixture(t, "lifecycle-trigger-limit")
	var triggerSQL string
	if err := st.db.QueryRow(`SELECT sql FROM sqlite_master WHERE type='trigger' AND name=?`,
		machineLifecycleRevisionGuardTrigger).Scan(&triggerSQL); err != nil {
		t.Fatal(err)
	}
	limit := MaxMachineLifecycleRevision - 1
	if want := strconv.FormatInt(limit, 10); !strings.Contains(triggerSQL, want) {
		t.Fatalf("revision guard trigger SQL does not contain Go limit %s: %s", want, triggerSQL)
	}

	setRevision := func(revision int64) {
		t.Helper()
		for _, trigger := range []string{machineLifecycleRevisionGuardTrigger, machineLifecycleRevisionOnlyTrigger} {
			if _, err := st.db.Exec(`DROP TRIGGER IF EXISTS ` + trigger); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := st.db.Exec(`UPDATE machine_registry SET retired_at=NULL,lifecycle_revision=? WHERE machine_id=?`,
			revision, machineID); err != nil {
			t.Fatal(err)
		}
		if err := ensureMachineLifecycleRevisionTriggers(st.db); err != nil {
			t.Fatal(err)
		}
	}
	setRevision(limit)
	retiredAt := fmtTime(createdAt.Add(time.Hour))
	if _, err := st.db.Exec(`UPDATE machine_registry
	 SET retired_at=?,lifecycle_revision=lifecycle_revision+1 WHERE machine_id=?`, retiredAt, machineID); err == nil ||
		!strings.Contains(err.Error(), "canonical revision and event writer") {
		t.Fatalf("transition at revision limit was not rejected by guard: %v", err)
	}

	setRevision(limit - 1)
	if _, err := st.db.Exec(`UPDATE machine_registry
	 SET retired_at=?,lifecycle_revision=lifecycle_revision+1 WHERE machine_id=?`, retiredAt, machineID); err != nil {
		t.Fatalf("transition below revision limit was rejected: %v", err)
	}
	machine, err := st.GetMachine(machineID)
	if err != nil || machine.LifecycleRevision != limit || machine.RetiredAt == nil {
		t.Fatalf("transition below revision limit machine=%+v err=%v", machine, err)
	}
}

func TestMachineLifecycleRevisionMigrationAndLegacyWriterGuard(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	createdAt := time.Date(2026, 9, 8, 17, 0, 0, 0, time.UTC)
	st.nowFn = func() time.Time { return createdAt }
	machineID, _, err := st.CreateEnrollTokenFor("lifecycle-migration", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for _, trigger := range []string{machineLifecycleRevisionGuardTrigger, machineLifecycleRevisionTrigger, machineLifecycleRevisionOnlyTrigger} {
		if _, err := st.db.Exec(`DROP TRIGGER IF EXISTS ` + trigger); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.db.Exec(`ALTER TABLE machine_registry DROP COLUMN lifecycle_revision`); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	machine, err := st.GetMachine(machineID)
	if err != nil || machine.LifecycleRevision != 0 {
		t.Fatalf("migrated machine=%+v err=%v", machine, err)
	}
	originalRevision := machine.LifecycleRevision
	if _, err := st.db.Exec(`UPDATE machine_registry
	 SET lifecycle_revision=lifecycle_revision+1 WHERE machine_id=?`, machineID); err == nil ||
		!strings.Contains(err.Error(), "cannot change without lifecycle state") {
		t.Fatalf("revision-only update was not rejected: %v", err)
	}
	if _, err := st.db.Exec(`UPDATE machine_registry
	 SET lifecycle_revision=lifecycle_revision WHERE machine_id=?`, machineID); err != nil {
		t.Fatalf("same-value revision update was rejected: %v", err)
	}
	machine, err = st.GetMachine(machineID)
	if err != nil || machine.LifecycleRevision != originalRevision {
		t.Fatalf("revision guard changed machine=%+v err=%v", machine, err)
	}
	retiredAt := createdAt.Add(time.Minute)
	if _, err := st.db.Exec(`UPDATE machine_registry SET retired_at=? WHERE machine_id=?`,
		fmtTime(retiredAt), machineID); err == nil || !strings.Contains(err.Error(), "canonical revision and event writer") {
		t.Fatalf("legacy retired_at-only update was not rejected: %v", err)
	}
	if err := st.RetireMachine(machineID, retiredAt); err != nil {
		t.Fatal(err)
	}
	machine, err = st.GetMachine(machineID)
	if err != nil || machine.LifecycleRevision != 1 || machine.RetiredAt == nil {
		t.Fatalf("canonical legacy helper machine=%+v err=%v", machine, err)
	}
	if got := countRows(t, st, `SELECT COUNT(*) FROM machine_registry_lifecycle_events WHERE machine_id=?`, machineID); got != 1 {
		t.Fatalf("canonical helper event count=%d", got)
	}
}

// validateOperatorMachineLifecycleReceipt（`:660-719`）**完全沒有提到**
// receipt.Channel 與 receipt.ChannelRevision，所以這兩欄唯一綁得住的東西是
// operatorMachineLifecycleSuccessAuditDetail（`:572-577`）印進 audit_log 的
// receipt_sha256——整包 receipt 的雜湊。
//
// ⚠ 這一刀釘的是**後備本身**，不是欄位檢查。實測：把 `:573` 的
// json.Marshal(receipt) 換成 json.Marshal(struct{}{})（雜湊不再綁 receipt），
// 整棵樹全綠；把 validateOperatorMachineLifecycleReceipt 焊成 return nil，
// 整棵樹也全綠。兩道都沒人守，所以任何一道被拿掉都不會有測試出聲。
//
// ⚠ TestOperatorMachineLifecycleCorruptCachedReceiptFailsClosed（`:475`）
// 名字像看守者但不是：它塞重複的 machine_id 鍵，死在
// rejectDuplicateLifecycleReceiptFields 的解碼階段，走不到欄位檢查或雜湊。
func TestOperatorMachineLifecycleReplayRefusesAReceiptThatRenamesTheChannel(t *testing.T) {
	st, machineID, _, _, _ := newOperatorLifecycleFixture(t, "lifecycle-channel")
	revision := int64(0)
	preview, err := st.PreviewOperatorMachineLifecycle(OperatorMachineLifecyclePreviewRequest{
		MachineID: machineID, DesiredState: MachineLifecycleRetired, ExpectedRevision: &revision,
	})
	if err != nil {
		t.Fatal(err)
	}
	req := operatorLifecycleRequest(machineID, preview, "lifecycle-channel-key")
	if _, err := st.ApplyOperatorMachineLifecycle(req); err != nil {
		t.Fatal(err)
	}
	honest, err := st.ApplyOperatorMachineLifecycle(req)
	if err != nil || !honest.Replayed || honest.Channel != "canary" || honest.ChannelRevision != 1 || !honest.ChannelPreserved {
		t.Fatalf("沒有一次會把真實 channel 交回來的誠實重放，下面的拒絕就無法歸因於被改掉的 channel: result=%+v err=%v", honest, err)
	}

	var raw string
	if err := st.db.QueryRow(`SELECT response_json FROM operator_idempotency WHERE idempotency_key=?`,
		req.IdempotencyKey).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	old := `"channel":"canary"`
	new := `"channel":"stable"`
	if count := strings.Count(raw, old); count != 1 {
		t.Fatalf("找不到唯一那一段 channel，換掉的可能不是我以為的那一處: count=%d response_json=%s", count, raw)
	}
	corrupt := strings.Replace(raw, old, new, 1)
	if _, err := st.db.Exec(`UPDATE operator_idempotency SET response_json=? WHERE idempotency_key=?`,
		corrupt, req.IdempotencyKey); err != nil {
		t.Fatal(err)
	}

	result, err := st.ApplyOperatorMachineLifecycle(req)
	if err == nil || !strings.Contains(err.Error(), "idempotency cache invalid") {
		t.Errorf("renamed-channel replay err=%v, want idempotency cache invalid; operator 手上那份退役回條會說這台機器當時在 stable，"+
			"實際是 canary；channel_preserved=true 於是變成保住了錯的那一條，日後復役會被期待回到 stable", err)
	}
	if result.Channel != "" {
		t.Errorf("被拒絕的重放不能把偽造的 channel 交回去: got channel=%q", result.Channel)
	}
}
