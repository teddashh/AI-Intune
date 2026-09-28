package store

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func restoreDrillTestRequest(key, requestFill string) OperatorRestoreDrillRequest {
	backup := RestoreDrillBackup{
		Name: "clawctl-20260911T120000Z-before-test.sqlite", SizeBytes: 4096,
		ModifiedAt: time.Date(2026, 9, 11, 11, 59, 0, 123, time.UTC),
		SHA256:     "sha256:" + strings.Repeat("a", 64),
	}
	previewDigest := "sha256:" + strings.Repeat("b", 64)
	return OperatorRestoreDrillRequest{
		PreviewDigest: previewDigest, Confirm: RestoreDrillConfirmation(backup.Name),
		Reason: "quarterly recovery verification", IdempotencyKey: key,
		RequestDigest: "sha256:" + strings.Repeat(requestFill, 64),
		Prepared:      RestoreDrillPrepared{Backup: backup, LiveExpectedAtPreview: 3, CurrentPreviewDigest: previewDigest},
		Audit: AuditEntry{SourceAddr: "100.64.0.7", AuthSubject: "tailscale-user:42",
			AuthCapability: "example.com/cap/clawctl-admin", SourceKind: "operator-api"},
	}
}

func TestOperatorRestoreDrillEnqueueReplayAndWorkerTransitions(t *testing.T) {
	st := newTestStore(t)
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	st.nowFn = func() time.Time { return now }
	req := restoreDrillTestRequest("restore-drill-1", "c")
	created, err := st.ApplyOperatorRestoreDrill(req)
	if err != nil || created.Replayed || !created.Audited || created.Operation.State != RestoreDrillQueued {
		t.Fatalf("created=%+v err=%v", created, err)
	}
	claim, err := st.ClaimRestoreDrillOperation(created.Operation.OperationID, false)
	if err != nil || claim.Operation.State != RestoreDrillRunning || claim.Operation.Attempt != 1 || claim.RunToken == "" {
		t.Fatalf("claim=%+v err=%v", claim, err)
	}
	newest := now.Add(-time.Hour)
	completed, err := st.SucceedRestoreDrillOperation(created.Operation.OperationID, claim.RunToken, RestoreDrillWorkerResult{
		Backup: req.Prepared.Backup, Machines: 4, Expected: 3, LiveExpected: 3,
		NewestCheckinAt: &newest, DurationMilliseconds: 125,
	})
	if err != nil || completed.State != RestoreDrillSucceeded || completed.Phase != RestoreDrillPhaseComplete ||
		completed.Machines == nil || *completed.Machines != 4 || completed.FinishedAt == nil {
		t.Fatalf("completed=%+v err=%v", completed, err)
	}
	replay, err := st.ApplyOperatorRestoreDrill(req)
	if err != nil || !replay.Replayed || !replay.Audited || !reflect.DeepEqual(replay.Operation, completed) {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}
	listed, err := st.ListRestoreDrillOperations(10)
	if err != nil || listed.Total != 1 || len(listed.Items) != 1 || !reflect.DeepEqual(listed.Items[0], completed) {
		t.Fatalf("listed=%+v err=%v", listed, err)
	}
	var receipts, audits int
	_ = st.db.QueryRow(`SELECT COUNT(*) FROM operator_idempotency WHERE idempotency_key=?`, req.IdempotencyKey).Scan(&receipts)
	_ = st.db.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE action=? AND idempotency_key=?`, AuditRestoreDrill, req.IdempotencyKey).Scan(&audits)
	if receipts != 1 || audits != 2 {
		t.Fatalf("receipts=%d audits=%d", receipts, audits)
	}
}

func TestReplayOperatorRestoreDrillOnlyReadsTheLedger(t *testing.T) {
	st := newTestStore(t)
	st.nowFn = func() time.Time { return time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC) }
	req := restoreDrillTestRequest("restore-drill-replay-only", "8")

	result, replayed, err := st.ReplayOperatorRestoreDrill(req)
	if err != nil || replayed || result != (OperatorRestoreDrillResult{}) {
		t.Fatalf("miss result=%+v replayed=%t err=%v", result, replayed, err)
	}
	var audits, receipts int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM audit_log`).Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM operator_idempotency`).Scan(&receipts); err != nil {
		t.Fatal(err)
	}
	if audits != 0 || receipts != 0 {
		t.Fatalf("miss audits=%d receipts=%d", audits, receipts)
	}

	created, err := st.ApplyOperatorRestoreDrill(req)
	if err != nil {
		t.Fatal(err)
	}
	replayReq := req
	replayReq.Prepared = RestoreDrillPrepared{}
	replay, replayed, err := st.ReplayOperatorRestoreDrill(replayReq)
	if err != nil || !replayed || !replay.Replayed || replay.Operation.OperationID != created.Operation.OperationID {
		t.Fatalf("replay=%+v replayed=%t err=%v", replay, replayed, err)
	}
	var invalidAudits int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE subject='restore drill idempotency cache'`).Scan(&invalidAudits); err != nil {
		t.Fatal(err)
	}
	if invalidAudits != 0 {
		t.Fatalf("invalidAudits=%d", invalidAudits)
	}
	var reason, subject, outcome string
	if err := st.db.QueryRow(`SELECT COALESCE(reason,''), subject, outcome FROM audit_log WHERE action=? AND idempotency_key=? AND detail LIKE ?`,
		AuditRestoreDrill, req.IdempotencyKey, OperatorIdempotencyReplayPrefix+"%").Scan(&reason, &subject, &outcome); err != nil {
		t.Fatal(err)
	}
	if reason != req.Reason || subject != req.Prepared.Backup.Name || outcome != outcomeOf(true) {
		t.Fatalf("replay audit reason=%q subject=%q outcome=%q", reason, subject, outcome)
	}
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM audit_log`).Scan(&audits); err != nil {
		t.Fatal(err)
	}

	invalidDigest := replayReq
	invalidDigest.RequestDigest = "not-a-digest"
	result, replayed, err = st.ReplayOperatorRestoreDrill(invalidDigest)
	if err != nil || replayed || result != (OperatorRestoreDrillResult{}) {
		t.Fatalf("invalid digest result=%+v replayed=%t err=%v", result, replayed, err)
	}
	var auditsAfter int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM audit_log`).Scan(&auditsAfter); err != nil {
		t.Fatal(err)
	}
	if auditsAfter != audits {
		t.Fatalf("audits before invalid digest=%d after=%d", audits, auditsAfter)
	}
}

// ⚠⚠ 回放必須只依帳本上記下的事實，不得依賴呼叫端當下看到的外部世界。
func TestRestoreDrillReplayIgnoresANewerBackupInTheRequest(t *testing.T) {
	st := newTestStore(t)
	st.nowFn = func() time.Time { return time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC) }
	req := restoreDrillTestRequest("restore-drill-newer-backup", "9")
	created, err := st.ApplyOperatorRestoreDrill(req)
	if err != nil {
		t.Fatal(err)
	}

	replayReq := req
	replayReq.Prepared.Backup = RestoreDrillBackup{
		Name: "clawctl-20260912T120000Z-before-test.sqlite", SizeBytes: 8192,
		ModifiedAt: time.Date(2026, 9, 12, 11, 59, 0, 456, time.UTC),
		SHA256:     "sha256:" + strings.Repeat("d", 64),
	}
	replayReq.Prepared.CurrentPreviewDigest = "sha256:" + strings.Repeat("e", 64)
	replay, err := st.ApplyOperatorRestoreDrill(replayReq)
	if err != nil || !replay.Replayed || replay.Operation.OperationID != created.Operation.OperationID {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}

	var invalidAudits, successfulReplays int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE action=? AND idempotency_key=? AND subject=?`,
		AuditRestoreDrill, req.IdempotencyKey, "restore drill idempotency cache").Scan(&invalidAudits); err != nil {
		t.Fatal(err)
	}
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE action=? AND idempotency_key=? AND outcome='ok' AND detail LIKE ?`,
		AuditRestoreDrill, req.IdempotencyKey, OperatorIdempotencyReplayPrefix+"%").Scan(&successfulReplays); err != nil {
		t.Fatal(err)
	}
	if invalidAudits != 0 || successfulReplays != 1 {
		t.Fatalf("invalidAudits=%d successfulReplays=%d", invalidAudits, successfulReplays)
	}
}

func TestOperatorRestoreDrillRejectsActiveAndReplaysOriginalDecision(t *testing.T) {
	st := newTestStore(t)
	st.nowFn = func() time.Time { return time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC) }
	first := restoreDrillTestRequest("restore-drill-active-1", "d")
	if _, err := st.ApplyOperatorRestoreDrill(first); err != nil {
		t.Fatal(err)
	}
	second := restoreDrillTestRequest("restore-drill-active-2", "e")
	_, err := st.ApplyOperatorRestoreDrill(second)
	var rejected *OperatorRequestError
	if !errors.As(err, &rejected) || rejected.Code != OperatorCodeRestoreDrillActive || !rejected.Audited || rejected.Replayed {
		t.Fatalf("first rejection=%T %+v", err, err)
	}
	_, err = st.ApplyOperatorRestoreDrill(second)
	if !errors.As(err, &rejected) || rejected.Code != OperatorCodeRestoreDrillActive || !rejected.Replayed || !rejected.Audited {
		t.Fatalf("replay rejection=%T %+v", err, err)
	}
	conflict := second
	conflict.RequestDigest = "sha256:" + strings.Repeat("f", 64)
	_, err = st.ApplyOperatorRestoreDrill(conflict)
	if !errors.As(err, &rejected) || rejected.Code != OperatorCodeIdempotencyConflict || !rejected.Audited {
		t.Fatalf("conflict=%T %+v", err, err)
	}
}

// ⚠⚠ 被拒回放同樣不得依賴呼叫端當下看到的備份。
func TestRestoreDrillRejectionReplayIgnoresANewerBackupInTheRequest(t *testing.T) {
	st := newTestStore(t)
	st.nowFn = func() time.Time { return time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC) }
	first := restoreDrillTestRequest("restore-drill-rejected-newer-1", "2")
	if _, err := st.ApplyOperatorRestoreDrill(first); err != nil {
		t.Fatal(err)
	}
	second := restoreDrillTestRequest("restore-drill-rejected-newer-2", "3")
	_, err := st.ApplyOperatorRestoreDrill(second)
	var rejected *OperatorRequestError
	if !errors.As(err, &rejected) || rejected.Code != OperatorCodeRestoreDrillActive || !rejected.Audited || rejected.Replayed {
		t.Fatalf("first rejection=%T %+v", err, err)
	}

	replayReq := second
	replayReq.Prepared.Backup = RestoreDrillBackup{
		Name: "clawctl-20260912T120000Z-before-test.sqlite", SizeBytes: 8192,
		ModifiedAt: time.Date(2026, 9, 12, 11, 59, 0, 456, time.UTC),
		SHA256:     "sha256:" + strings.Repeat("d", 64),
	}
	replayReq.Prepared.CurrentPreviewDigest = "sha256:" + strings.Repeat("e", 64)
	_, err = st.ApplyOperatorRestoreDrill(replayReq)
	if !errors.As(err, &rejected) || rejected.Code != OperatorCodeRestoreDrillActive || !rejected.Replayed || !rejected.Audited {
		t.Fatalf("replay rejection=%T %+v", err, err)
	}

	var invalidAudits, rejectionReplays int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE action=? AND idempotency_key=? AND subject=?`,
		AuditRestoreDrill, second.IdempotencyKey, "restore drill idempotency cache").Scan(&invalidAudits); err != nil {
		t.Fatal(err)
	}
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE action=? AND idempotency_key=? AND detail LIKE ?`,
		AuditRestoreDrill, second.IdempotencyKey, OperatorIdempotencyReplayPrefix+"%").Scan(&rejectionReplays); err != nil {
		t.Fatal(err)
	}
	if invalidAudits != 0 || rejectionReplays != 1 {
		t.Fatalf("invalidAudits=%d rejectionReplays=%d", invalidAudits, rejectionReplays)
	}
}

func TestOperatorRestoreDrillStartupFenceRevokesOldClaim(t *testing.T) {
	st := newTestStore(t)
	st.nowFn = func() time.Time { return time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC) }
	req := restoreDrillTestRequest("restore-drill-fence", "1")
	created, err := st.ApplyOperatorRestoreDrill(req)
	if err != nil {
		t.Fatal(err)
	}
	first, err := st.ClaimRestoreDrillOperation(created.Operation.OperationID, false)
	if err != nil {
		t.Fatal(err)
	}
	if fenced, err := st.FenceRunningRestoreDrillOperations(); err != nil || fenced != 1 {
		t.Fatalf("fenced=%d err=%v", fenced, err)
	}
	if _, err := st.FailRestoreDrillOperation(created.Operation.OperationID, first.RunToken,
		"RESTORE_DRILL_TEST", "old worker"); !errors.Is(err, ErrRestoreDrillClaimLost) {
		t.Fatalf("old token err=%v", err)
	}
	second, err := st.ClaimRestoreDrillOperation(created.Operation.OperationID, true)
	if err != nil || second.RunToken == first.RunToken || second.Operation.Attempt != 2 {
		t.Fatalf("second=%+v err=%v", second, err)
	}
	failed, err := st.FailRestoreDrillOperation(created.Operation.OperationID, second.RunToken,
		"RESTORE_DRILL_TEST", "verification failed")
	if err != nil || failed.State != RestoreDrillFailed || failed.ErrorCode == nil || *failed.ErrorCode != "RESTORE_DRILL_TEST" {
		t.Fatalf("failed=%+v err=%v", failed, err)
	}
}

func TestOperatorRestoreDrillCorruptCacheFailsClosed(t *testing.T) {
	st := newTestStore(t)
	st.nowFn = func() time.Time { return time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC) }
	req := restoreDrillTestRequest("restore-drill-corrupt", "2")
	if _, err := st.ApplyOperatorRestoreDrill(req); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`DELETE FROM audit_log WHERE action=? AND idempotency_key=?`, AuditRestoreDrill, req.IdempotencyKey); err != nil {
		t.Fatal(err)
	}
	result, err := st.ApplyOperatorRestoreDrill(req)
	if !errors.Is(err, ErrRestoreDrillCacheInvalid) || !result.Audited {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	var invalidAudits, operations int
	_ = st.db.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE action=? AND detail=?`, AuditRestoreDrill, restoreDrillCacheInvalid).Scan(&invalidAudits)
	_ = st.db.QueryRow(`SELECT COUNT(*) FROM restore_drill_operations`).Scan(&operations)
	if invalidAudits != 1 || operations != 1 {
		t.Fatalf("invalidAudits=%d operations=%d", invalidAudits, operations)
	}
}

// expected <= machines 是跨欄算術；SQLite 的 CHECK 只管各欄範圍，無法約束兩欄
// 之間的關係。讀取端只有 validRestoreDrillRecord 會比較兩者，而 Web 與
// internal/operator 同進程，不經 operatorclient 的鏡像驗證，因此畫面上沒有第二道防線。
func TestRestoreDrillExpectedAboveMachinesIsRefusedOnRead(t *testing.T) {
	st := newTestStore(t)
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	st.nowFn = func() time.Time { return now }
	req := restoreDrillTestRequest("restore-drill-expected-above-machines", "c")
	created, err := st.ApplyOperatorRestoreDrill(req)
	if err != nil {
		t.Fatal(err)
	}
	claim, err := st.ClaimRestoreDrillOperation(created.Operation.OperationID, false)
	if err != nil {
		t.Fatal(err)
	}
	newest := now.Add(-time.Hour)
	_, err = st.SucceedRestoreDrillOperation(
		created.Operation.OperationID,
		claim.RunToken,
		RestoreDrillWorkerResult{
			Backup: req.Prepared.Backup, Machines: 4, Expected: 3, LiveExpected: 3,
			NewestCheckinAt: &newest, DurationMilliseconds: 125,
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	id := created.Operation.OperationID
	op, err := st.GetRestoreDrillOperation(id)
	if err != nil || op.Expected == nil || op.Machines == nil ||
		*op.Expected != 3 || *op.Machines != 4 {
		t.Fatalf("starting operation=%+v err=%v; starting point must be expected 3 below "+
			"machines 4, or the later refusal cannot be attributed to that arithmetic invariant",
			op, err)
	}
	if _, err := st.DB().Exec(
		`UPDATE restore_drill_operations SET expected=5 WHERE operation_id=?`, id,
	); err != nil {
		t.Fatalf("corrupting expected above machines: %v; if SQLite rejects this update, "+
			"the test measures a schema CHECK and leaves validRestoreDrillRecord unguarded", err)
	}

	_, err = st.GetRestoreDrillOperation(id)
	if !errors.Is(err, ErrRestoreDrillCorrupt) {
		t.Errorf("GetRestoreDrillOperation err=%v, want %v; the Web drill page could print "+
			"backup expected 5 beside backup machines 4 and treat an arithmetically impossible "+
			"result as evidence of a passed restore drill", err, ErrRestoreDrillCorrupt)
	}
	_, err = st.ListRestoreDrillOperations(10)
	if !errors.Is(err, ErrRestoreDrillCorrupt) {
		t.Errorf("ListRestoreDrillOperations err=%v, want %v; list and detail pages share the "+
			"same scan, so pinning only detail would let the list keep printing the corrupt row",
			err, ErrRestoreDrillCorrupt)
	}
}
