package store

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/deploy"
)

func diagnosticNoopFixture(t *testing.T) (*Store, string, time.Time) {
	t.Helper()
	st := newTestStore(t)
	now := time.Date(2026, 9, 10, 1, 2, 3, 0, time.UTC)
	st.nowFn = func() time.Time { return now }
	machineID := mustEnroll(t, st, "diagnostic-cnode", now.Add(-time.Hour))
	if err := st.RecordCheckin(machineID, healthyCheckin(now.Add(-time.Minute)), now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	return st, machineID, now
}

func diagnosticNoopRequest(machineID string, preview OperatorDiagnosticNoopPreviewResult) OperatorDiagnosticNoopRequest {
	return OperatorDiagnosticNoopRequest{
		MachineID: machineID, ExecutionTimeout: preview.ExecutionTimeoutSeconds,
		ConfirmDisplayName: preview.DisplayName, PreviewDigest: preview.PreviewDigest,
		Reason: "prove claim/event/verification protocol", IdempotencyKey: "diagnostic-noop-key",
		RequestDigest: "sha256:diagnostic-request", CreatedBy: "operator:tailscale-user:7",
		Audit: AuditEntry{SourceAddr: "100.64.0.7", AuthSubject: "tailscale-user:7",
			AuthCapability: "example.com/cap/clawctl-operate", SourceKind: "operator-api"},
	}
}

func TestOperatorDiagnosticNoopPreviewApplyAndReplayAreAtomic(t *testing.T) {
	st, machineID, now := diagnosticNoopFixture(t)
	preview, err := st.PreviewOperatorDiagnosticNoop(machineID, OperatorDiagnosticNoopDefaultTimeout)
	if err != nil {
		t.Fatal(err)
	}
	if preview.MachineID != machineID || preview.DisplayName != "diagnostic-cnode" ||
		preview.Kind != OperatorDiagnosticNoopKind || preview.SpecDigest != operatorDiagnosticNoopSpecDigest() ||
		preview.ChangesMachineConfiguration || !preview.CreatesDesiredState || !preview.CreatesJob ||
		!preview.DeliveryRequiresJobsEnabled || !preview.EverReported ||
		preview.ActiveJobCount != 0 || preview.CurrentResourceRevision != 0 || preview.PlannedRevision != 1 ||
		len(preview.Blockers) != 0 || preview.PreviewDigest == "" || !preview.PreviewedAt.Equal(now) {
		t.Fatalf("preview=%+v", preview)
	}
	req := diagnosticNoopRequest(machineID, preview)
	result, err := st.ApplyOperatorDiagnosticNoop(req)
	if err != nil {
		t.Fatal(err)
	}
	if result.Replayed || !result.Audited || result.JobID == "" || result.DesiredID == "" ||
		result.Revision != 1 || result.PreviewDigest != preview.PreviewDigest || !result.CreatedAt.Equal(now) ||
		result.ChangesMachineConfiguration {
		t.Fatalf("result=%+v", result)
	}
	desired, err := st.DesiredState(result.DesiredID)
	if err != nil {
		t.Fatal(err)
	}
	job, err := st.Job(result.JobID)
	if err != nil {
		t.Fatal(err)
	}
	if desired.ScopeType != "machine" || desired.ScopeID != machineID ||
		desired.ResourceKind != OperatorDiagnosticNoopResourceKind || desired.ResourceID != OperatorDiagnosticNoopResourceID ||
		desired.Spec != OperatorDiagnosticNoopSpec || desired.Revision != result.Revision ||
		job.MachineID != machineID || job.DesiredID != result.DesiredID || job.State != deploy.NotStarted ||
		job.ArtifactDigest != operatorDiagnosticNoopSpecDigest() || job.Irreversible ||
		job.ExecutionTimeout != OperatorDiagnosticNoopDefaultTimeout {
		t.Fatalf("desired=%+v job=%+v", desired, job)
	}
	if got := countRows(t, st, `SELECT COUNT(*) FROM desired_state`); got != 1 {
		t.Fatalf("desired rows=%d", got)
	}
	replay, err := st.ApplyOperatorDiagnosticNoop(req)
	if err != nil || !replay.Replayed || !replay.Audited || replay.JobID != result.JobID || replay.DesiredID != result.DesiredID {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}
	if got := countRows(t, st, `SELECT COUNT(*) FROM desired_state`); got != 1 {
		t.Fatalf("replay created desired rows=%d", got)
	}
	if got := countRows(t, st, `SELECT COUNT(*) FROM jobs`); got != 1 {
		t.Fatalf("replay created jobs=%d", got)
	}
	entries, err := st.Audit(machineID, 10)
	if err != nil || len(entries) != 2 || entries[0].Action != AuditDiagnosticNoop || !entries[0].IsOperatorReplay() ||
		entries[1].AuthSubject != "tailscale-user:7" || !entries[1].OK {
		t.Fatalf("audit=%+v err=%v", entries, err)
	}
}

func TestOperatorDiagnosticNoopRejectsStalePreviewWithoutOrphanDesiredState(t *testing.T) {
	st, machineID, _ := diagnosticNoopFixture(t)
	preview, err := st.PreviewOperatorDiagnosticNoop(machineID, 60)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AllocateRevision(OperatorDiagnosticNoopResourceKind + ":" + OperatorDiagnosticNoopResourceID); err != nil {
		t.Fatal(err)
	}
	_, err = st.ApplyOperatorDiagnosticNoop(diagnosticNoopRequest(machineID, preview))
	if !errors.Is(err, ErrDiagnosticPreviewStale) {
		t.Fatalf("stale apply err=%v", err)
	}
	if countRows(t, st, `SELECT COUNT(*) FROM desired_state`) != 0 || countRows(t, st, `SELECT COUNT(*) FROM jobs`) != 0 {
		t.Fatal("stale diagnostic apply left desired state or job")
	}
	_, replayErr := st.ApplyOperatorDiagnosticNoop(diagnosticNoopRequest(machineID, preview))
	var replay *OperatorRequestError
	if !errors.Is(replayErr, ErrDiagnosticPreviewStale) || !errors.As(replayErr, &replay) || !replay.Replayed || !replay.Audited {
		t.Fatalf("stale replay=%v", replayErr)
	}
}

func TestOperatorDiagnosticNoopPreviewNamesEveryBlocker(t *testing.T) {
	st := newTestStore(t)
	now := time.Date(2026, 9, 10, 2, 0, 0, 0, time.UTC)
	st.nowFn = func() time.Time { return now }
	machineID, _, err := st.CreateEnrollTokenFor("blocked-diagnostic", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.RetireMachine(machineID, now); err != nil {
		t.Fatal(err)
	}
	preview, err := st.PreviewOperatorDiagnosticNoop(machineID, 60)
	if err != nil {
		t.Fatal(err)
	}
	if preview.EverReported || preview.JobsEnabled != nil || len(preview.Blockers) != 2 ||
		preview.Blockers[0] != OperatorDiagnosticNoopBlockerRetired ||
		preview.Blockers[1] != OperatorDiagnosticNoopBlockerNeverReported {
		t.Fatalf("blockers=%v", preview.Blockers)
	}
	req := diagnosticNoopRequest(machineID, preview)
	req.ConfirmDisplayName = preview.DisplayName
	_, err = st.ApplyOperatorDiagnosticNoop(req)
	if !errors.Is(err, ErrMachineRetired) {
		t.Fatalf("blocked apply err=%v", err)
	}
}

func TestOperatorDiagnosticNoopBlocksWhileAnotherJobIsActive(t *testing.T) {
	st, machineID, _ := diagnosticNoopFixture(t)
	desiredID, revision, err := st.CreateDesiredState("machine", machineID,
		OperatorDiagnosticNoopResourceKind, OperatorDiagnosticNoopResourceID,
		OperatorDiagnosticNoopSpec, "test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateJob(machineID, desiredID, revision, NewJob{
		ArtifactDigest: operatorDiagnosticNoopSpecDigest(), ExecutionTimeout: 60,
	}); err != nil {
		t.Fatal(err)
	}
	preview, err := st.PreviewOperatorDiagnosticNoop(machineID, 60)
	if err != nil {
		t.Fatal(err)
	}
	if preview.ActiveJobCount != 1 || len(preview.Blockers) != 1 ||
		preview.Blockers[0] != OperatorDiagnosticNoopBlockerNonterminalJob {
		t.Fatalf("preview=%+v", preview)
	}
	req := diagnosticNoopRequest(machineID, preview)
	req.ExecutionTimeout = 60
	if _, err := st.ApplyOperatorDiagnosticNoop(req); !errors.Is(err, ErrMachineActiveJob) {
		t.Fatalf("active-job apply err=%v", err)
	}
	if countRows(t, st, `SELECT COUNT(*) FROM jobs`) != 1 {
		t.Fatal("blocked diagnostic created another job")
	}
}

func TestOperatorDiagnosticNoopRequiresAgentExecutionState(t *testing.T) {
	disabled := false
	for _, tc := range []struct {
		name        string
		enabled     *bool
		wantBlocker OperatorDiagnosticNoopBlocker
		wantError   error
	}{
		{name: "unknown", wantBlocker: OperatorDiagnosticNoopBlockerExecutionUnknown, wantError: ErrAgentExecutionUnknown},
		{name: "disabled", enabled: &disabled, wantBlocker: OperatorDiagnosticNoopBlockerExecutionDisabled, wantError: ErrAgentExecutionDisabled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newTestStore(t)
			now := time.Date(2026, 9, 10, 3, 0, 0, 0, time.UTC)
			st.nowFn = func() time.Time { return now }
			machineID := mustEnroll(t, st, "execution-"+tc.name, now.Add(-time.Hour))
			checkin := healthyCheckin(now.Add(-time.Minute))
			checkin.JobsEnabled = tc.enabled
			if err := st.RecordCheckin(machineID, checkin, now.Add(-time.Minute)); err != nil {
				t.Fatal(err)
			}
			preview, err := st.PreviewOperatorDiagnosticNoop(machineID, 60)
			if err != nil {
				t.Fatal(err)
			}
			if len(preview.Blockers) != 1 || preview.Blockers[0] != tc.wantBlocker {
				t.Fatalf("preview=%+v", preview)
			}
			if tc.enabled == nil && preview.JobsEnabled != nil ||
				tc.enabled != nil && (preview.JobsEnabled == nil || *preview.JobsEnabled != *tc.enabled) {
				t.Fatalf("jobs_enabled=%v want=%v", preview.JobsEnabled, tc.enabled)
			}
			req := diagnosticNoopRequest(machineID, preview)
			req.ExecutionTimeout = 60
			if _, err := st.ApplyOperatorDiagnosticNoop(req); !errors.Is(err, tc.wantError) {
				t.Fatalf("apply err=%v want=%v", err, tc.wantError)
			}
			if countRows(t, st, `SELECT COUNT(*) FROM jobs`) != 0 {
				t.Fatal("blocked execution state created a job")
			}
		})
	}
}

func TestOperatorDiagnosticNoopUsesLatestCheckinInstantForJobsEnabled(t *testing.T) {
	st := newTestStore(t)
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	st.nowFn = func() time.Time { return now }
	machineID := mustEnroll(t, st, "diagnostic-checkin-order", now.Add(-time.Hour))
	if _, err := st.DB().Exec(`INSERT INTO machine_checkins
		(machine_id,sent_at,received_at,jobs_enabled) VALUES
		(?,?,?,1),(?,?,?,0)`,
		machineID, "2026-09-14T11:59:00Z", "2026-09-14T11:59:00Z",
		machineID, "2026-09-14T11:59:30Z", "2026-09-14T10:59:30-01:00"); err != nil {
		t.Fatal(err)
	}
	preview, err := st.PreviewOperatorDiagnosticNoop(machineID, 60)
	if err != nil {
		t.Fatal(err)
	}
	_, applyErr := st.ApplyOperatorDiagnosticNoop(diagnosticNoopRequest(machineID, preview))
	hasDisabled := len(preview.Blockers) == 1 &&
		preview.Blockers[0] == OperatorDiagnosticNoopBlockerExecutionDisabled
	desired := countRows(t, st, `SELECT COUNT(*) FROM desired_state`)
	jobs := countRows(t, st, `SELECT COUNT(*) FROM jobs`)
	if !hasDisabled || !errors.Is(applyErr, ErrAgentExecutionDisabled) || desired != 0 || jobs != 0 {
		t.Fatalf("blockers=%v apply err=%v desired rows=%d job rows=%d",
			preview.Blockers, applyErr, desired, jobs)
	}
}

func TestOperatorDiagnosticNoopRejectsUnparseableCheckinWithoutAuthority(t *testing.T) {
	st, machineID, _ := diagnosticNoopFixture(t)
	validPreview, err := st.PreviewOperatorDiagnosticNoop(machineID, 60)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`INSERT INTO machine_checkins
		(machine_id,sent_at,received_at,jobs_enabled) VALUES (?,?,?,0)`,
		machineID, "2026-09-14T11:59:00Z", "2026-09-14 11:59:00"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.PreviewOperatorDiagnosticNoop(machineID, 60); err == nil ||
		err.Error() != "store: diagnostic noop target projection is invalid" {
		t.Fatalf("preview err=%v, want invalid projection", err)
	}
	if _, err := st.ApplyOperatorDiagnosticNoop(diagnosticNoopRequest(machineID, validPreview)); err == nil ||
		err.Error() != "store: diagnostic noop target projection is invalid" {
		t.Fatalf("apply err=%v, want invalid projection", err)
	}
	for table, query := range map[string]string{
		"desired":     `SELECT COUNT(*) FROM desired_state`,
		"job":         `SELECT COUNT(*) FROM jobs`,
		"revision":    `SELECT COUNT(*) FROM revision_counters`,
		"idempotency": `SELECT COUNT(*) FROM operator_idempotency`,
		"audit":       `SELECT COUNT(*) FROM audit_log`,
	} {
		if got := countRows(t, st, query); got != 0 {
			t.Fatalf("invalid projection wrote %s authority rows=%d", table, got)
		}
	}
}

func TestOperatorDiagnosticNoopCanonicalHistoryPreservesPreviewDigest(t *testing.T) {
	st, machineID, now := diagnosticNoopFixture(t)
	before, err := st.PreviewOperatorDiagnosticNoop(machineID, 60)
	if err != nil {
		t.Fatal(err)
	}
	older := now.Add(-2 * time.Minute)
	if _, err := st.DB().Exec(`INSERT INTO machine_checkins
		(machine_id,sent_at,received_at,jobs_enabled) VALUES (?,?,?,0)`,
		machineID, fmtTime(older), fmtTime(older)); err != nil {
		t.Fatal(err)
	}
	after, err := st.PreviewOperatorDiagnosticNoop(machineID, 60)
	if err != nil || after.PreviewDigest != before.PreviewDigest || after.JobsEnabled == nil ||
		!*after.JobsEnabled || len(after.Blockers) != 0 ||
		after.CurrentResourceRevision != before.CurrentResourceRevision {
		t.Fatalf("before=%+v after=%+v err=%v", before, after, err)
	}
}

func TestOperatorDiagnosticNoopIdempotencyConflictDoesNotCreateSecondJob(t *testing.T) {
	st, machineID, _ := diagnosticNoopFixture(t)
	preview, err := st.PreviewOperatorDiagnosticNoop(machineID, 60)
	if err != nil {
		t.Fatal(err)
	}
	req := diagnosticNoopRequest(machineID, preview)
	req.ExecutionTimeout = 60
	if _, err := st.ApplyOperatorDiagnosticNoop(req); err != nil {
		t.Fatal(err)
	}
	changed := req
	changed.RequestDigest = "sha256:changed"
	_, err = st.ApplyOperatorDiagnosticNoop(changed)
	if !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("conflict err=%v", err)
	}
	if countRows(t, st, `SELECT COUNT(*) FROM jobs`) != 1 {
		t.Fatal("idempotency conflict created another job")
	}
}

func TestOperatorDiagnosticNoopMissingReasonReplaysPersistedRejection(t *testing.T) {
	st, machineID, _ := diagnosticNoopFixture(t)
	preview, err := st.PreviewOperatorDiagnosticNoop(machineID, 60)
	if err != nil {
		t.Fatal(err)
	}
	req := diagnosticNoopRequest(machineID, preview)
	req.ExecutionTimeout = 60
	req.Reason = ""
	req.RequestDigest = "sha256:missing-reason"
	for attempt := 0; attempt < 2; attempt++ {
		_, err := st.ApplyOperatorDiagnosticNoop(req)
		var rejection *OperatorRequestError
		if !errors.Is(err, ErrReasonRequired) || !errors.As(err, &rejection) ||
			!rejection.Audited || rejection.Replayed != (attempt == 1) {
			t.Fatalf("attempt=%d rejection=%+v err=%v", attempt, rejection, err)
		}
	}
	if countRows(t, st, `SELECT COUNT(*) FROM desired_state`) != 0 ||
		countRows(t, st, `SELECT COUNT(*) FROM jobs`) != 0 ||
		countRows(t, st, `SELECT COUNT(*) FROM audit_log`) != 2 {
		t.Fatal("rejected replay changed diagnostic ledger")
	}
}

// operator_diagnostic.go:571-588 的欄位驗證器對 receipt.DisplayName 只檢查非空，
// operator_diagnostic.go:590-612 的證據 SQL 也不碰 display name；只有
// operator_diagnostic.go:471-476 的 receipt_sha256 綁得住整包 receipt。
// 重放結果由 operator_diagnostic.go:478-485、552-554 從 receipt 投影，而
// cmd/clawctl-hub/jobcreatecmd.go:181-183、232-233、288 的 CLI 回條印的就是 result.DisplayName。
// ⚠ 隔離盤（./...、-count=1）：validateOperatorDiagnosticNoopReceipt 整支焊成
// return nil，本測試仍綠、others=[]；葉子普查記為 UNGUARDED，這支欄位驗證器
// 全樹無人守，本測試抓到的不是它。operatorDiagnosticNoopSuccessAuditDetail 裡的
// json.Marshal(receipt) 換成 json.Marshal(struct{}{})，本測試轉紅、others=[]；雜湊
// 變成常數、不再綁 receipt，但寫入端與驗證端仍一致，比對照樣通過，全樹只有
// 這一支會注意到後備停止綁 receipt。對照組拿掉欄位驗證器裡
// receipt.DisplayName == "" || 這一條，本測試仍綠、others=[]。方法論上，量「後備
// 有沒有人守」時，突變不能讓寫入端與驗證端一起變，否則兩邊會一致地移動、
// 什麼都測不到；必須讓 detail 停止綁 receipt 本身。
func TestOperatorDiagnosticNoopReplayRefusesAReceiptThatRenamesTheMachine(t *testing.T) {
	st, machineID, _ := diagnosticNoopFixture(t)
	preview, err := st.PreviewOperatorDiagnosticNoop(machineID, OperatorDiagnosticNoopDefaultTimeout)
	if err != nil {
		t.Fatal(err)
	}
	req := diagnosticNoopRequest(machineID, preview)
	result, err := st.ApplyOperatorDiagnosticNoop(req)
	if err != nil {
		t.Fatal(err)
	}

	replay, err := st.ApplyOperatorDiagnosticNoop(req)
	if err != nil || !replay.Replayed || replay.DisplayName != "diagnostic-cnode" {
		t.Fatalf("不改任何東西時重放本來就拿得到正確機器名，下面的拒絕才歸因得了偽造：replay=%+v err=%v", replay, err)
	}

	var raw string
	if err := st.DB().QueryRow(`SELECT response_json FROM operator_idempotency WHERE idempotency_key=?`, req.IdempotencyKey).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	const original = `"display_name":"diagnostic-cnode"`
	if count := strings.Count(raw, original); count != 1 {
		t.Fatalf("display_name 錨點不唯一，量到的會是別的東西：count=%d response_json=%q", count, raw)
	}
	forged := strings.Replace(raw, original, `"display_name":"diagnostic-pnode"`, 1)
	update, err := st.DB().Exec(`UPDATE operator_idempotency SET response_json=? WHERE idempotency_key=?`, forged, req.IdempotencyKey)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := update.RowsAffected()
	if err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("updated operator_idempotency rows=%d, want 1", rows)
	}

	result, err = st.ApplyOperatorDiagnosticNoop(req)
	if err == nil || !strings.Contains(err.Error(), operatorDiagnosticNoopCacheInvalid) {
		t.Fatalf("CLI 回條會把這次診斷印成在 diagnostic-pnode 上跑的，而實際跑的是 diagnostic-cnode：result=%+v err=%v", result, err)
	}
	if result.DisplayName != "" || result.Replayed {
		t.Fatalf("CLI 回條會把這次診斷印成在 diagnostic-pnode 上跑的，而實際跑的是 diagnostic-cnode：result=%+v err=%v", result, err)
	}
}
