package store

import (
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
	"time"
)

func retentionFixture(t *testing.T) (*Store, time.Time, OperatorPrunePreview) {
	t.Helper()
	st := newTestStore(t)
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	st.nowFn = func() time.Time { return now }
	machineID := mustEnroll(t, st, "retention-machine", now.Add(-100*24*time.Hour))
	checkinAt(t, st, machineID, now.Add(-40*24*time.Hour))
	checkinAt(t, st, machineID, now.Add(-24*time.Hour))
	preview, err := st.PreviewOperatorPrune(now, DefaultRetention())
	if err != nil {
		t.Fatal(err)
	}
	if preview.TotalDeleted != 1 || preview.ExpectedRevision != 0 ||
		preview.Confirmation != "DELETE 1 ROWS" || len(preview.Counts) != len(pruneJobs) {
		t.Fatalf("preview=%+v", preview)
	}
	return st, now, preview
}

func TestOperatorRetentionPolicyRejectsDurationOverflow(t *testing.T) {
	policy := OperatorRetentionPolicy{
		ObservationsSeconds: math.MaxInt64, CheckinsSeconds: 14 * 24 * 60 * 60,
		OccupancySeconds: 400 * 24 * 60 * 60,
	}
	if _, err := policy.RetentionPolicy(); err == nil {
		t.Fatal("retention wire policy overflow was accepted")
	}
}

func retentionApplyRequest(preview OperatorPrunePreview) OperatorPruneRequest {
	req := OperatorPruneRequest{
		EvaluatedAt: preview.EvaluatedAt, Policy: preview.Policy,
		ExpectedRevision: preview.ExpectedRevision, Confirm: preview.Confirmation,
		PreviewDigest: preview.PreviewDigest, Reason: "scheduled evidence retention",
		IdempotencyKey: "retention-prune-1",
		Audit: AuditEntry{
			SourceAddr: "100.64.0.7", AuthSubject: "tailscale-user:42",
			AuthCapability: "example.com/cap/clawctl-admin", SourceKind: "operator-api",
		},
	}
	req.RequestDigest = OperatorPruneSemanticDigest(req)
	return req
}

func TestOperatorRetentionPreviewApplyAndReplayAreAtomic(t *testing.T) {
	st, _, preview := retentionFixture(t)
	req := retentionApplyRequest(preview)

	first, err := st.ApplyOperatorPrune(req)
	if err != nil || first.TotalDeleted != 1 || first.Revision != int64(len(pruneJobs)) ||
		first.Replayed || !first.Audited || first.KeptNewest != preview.KeptNewest {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	replay, err := st.ApplyOperatorPrune(req)
	if err != nil || !replay.Replayed || !replay.Audited || replay.Revision != first.Revision ||
		replay.TotalDeleted != first.TotalDeleted {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}

	var oldRows, receipts, auditRows, retentionRows int
	_ = st.db.QueryRow(`SELECT COUNT(*) FROM machine_checkins WHERE received_at < ?`,
		fmtTime(preview.EvaluatedAt.Add(-DefaultRetention().Checkins))).Scan(&oldRows)
	_ = st.db.QueryRow(`SELECT COUNT(*) FROM operator_idempotency WHERE idempotency_key=?`,
		req.IdempotencyKey).Scan(&receipts)
	_ = st.db.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE action=? AND idempotency_key=?`,
		AuditRetentionPrune, req.IdempotencyKey).Scan(&auditRows)
	_ = st.db.QueryRow(`SELECT COUNT(*) FROM retention_log WHERE at=?`,
		fmtTime(preview.EvaluatedAt)).Scan(&retentionRows)
	if oldRows != 0 || receipts != 1 || auditRows != 2 || retentionRows != len(pruneJobs) {
		t.Fatalf("old=%d receipts=%d audits=%d retention=%d", oldRows, receipts, auditRows, retentionRows)
	}
}

func TestOperatorRetentionStaleRejectionIsDurableAndDoesNotDelete(t *testing.T) {
	st, now, preview := retentionFixture(t)
	if _, err := st.Prune(now, DefaultRetention(), false); err != nil {
		t.Fatal(err)
	}
	req := retentionApplyRequest(preview)
	req.IdempotencyKey = "retention-stale"
	req.RequestDigest = OperatorPruneSemanticDigest(req)
	for attempt := 0; attempt < 2; attempt++ {
		_, err := st.ApplyOperatorPrune(req)
		var rejection *OperatorRequestError
		if !errors.As(err, &rejection) || rejection.Code != OperatorCodePreconditionFailed ||
			!rejection.Audited || rejection.Replayed != (attempt == 1) {
			t.Fatalf("attempt=%d rejection=%#v err=%v", attempt, rejection, err)
		}
	}
	var checkins int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM machine_checkins`).Scan(&checkins); err != nil || checkins != 1 {
		t.Fatalf("checkins=%d err=%v", checkins, err)
	}
}

func TestOperatorRetentionRejectsCorruptSuccessCacheClosed(t *testing.T) {
	st, _, preview := retentionFixture(t)
	req := retentionApplyRequest(preview)
	if _, err := st.ApplyOperatorPrune(req); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`UPDATE operator_idempotency SET response_json=? WHERE idempotency_key=?`,
		`{"receipt_version":"v1"}`, req.IdempotencyKey); err != nil {
		t.Fatal(err)
	}
	result, err := st.ApplyOperatorPrune(req)
	if err == nil || !result.Audited || !strings.Contains(err.Error(), "idempotency cache is invalid") {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestOperatorRetentionStatusDistinguishesNeverRunFromZeroRowRun(t *testing.T) {
	st := newTestStore(t)
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	before, err := st.OperatorRetentionStatus(now, DefaultRetention())
	if err != nil || before.HasRun || before.LastPruneAt != nil || before.Revision != 0 {
		t.Fatalf("before=%+v err=%v", before, err)
	}
	if _, err := st.Prune(now, DefaultRetention(), false); err != nil {
		t.Fatal(err)
	}
	after, err := st.OperatorRetentionStatus(now, DefaultRetention())
	if err != nil || !after.HasRun || after.LastPruneAt == nil || after.LastPruneRows != 0 ||
		after.Revision != int64(len(pruneJobs)) {
		t.Fatalf("after=%+v err=%v", after, err)
	}
}

// retention_log 那道後備是逐項存在性查詢：
// validateOperatorRetentionSuccessEvidence 對 receipt.Counts 的每一列去查
// 一筆對得上的紀錄，所以改數字、改表名、改邊界都擋得住，唯獨看不見
// 少一列。少掉的那一列只要 Deleted 與 Kept 都是 0，total_deleted 與
// kept_newest 也不會露餡。
//
// ⚠ 一定要拿掉**最後**那一列。拿掉中間任何一列會讓後面的 index 往前位移，
// validOperatorRetentionCounts 的 item.Table != job.table 就先開火了，
// 量到的會是表名那一條而不是長度那一條（紅燈盤實測：只拿掉長度子句時
// 這支仍然綠）。拿掉尾巴之後剩下每一格都還對齊 pruneJobs，
// len(counts) != len(pruneJobs) 是唯一還會開火的子句。
func TestRetentionReplayRefusesAReceiptThatDropsATable(t *testing.T) {
	st, _, preview := retentionFixture(t)
	req := retentionApplyRequest(preview)
	if _, err := st.ApplyOperatorPrune(req); err != nil {
		t.Fatal(err)
	}

	honest, err := st.ApplyOperatorPrune(req)
	if err != nil || !honest.Replayed || len(honest.Counts) != len(pruneJobs) {
		t.Fatalf("沒有一次會交回每一張表的誠實重放，下面的拒絕就無法歸因於少掉的那一列: "+
			"honest=%+v err=%v", honest, err)
	}

	var raw string
	if err := st.db.QueryRow(`SELECT response_json FROM operator_idempotency WHERE idempotency_key=?`,
		req.IdempotencyKey).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	receipt, err := decodeOperatorRetentionReceipt(raw)
	if err != nil {
		t.Fatal(err)
	}
	last := len(receipt.Counts) - 1
	if receipt.Counts[last].Deleted != 0 || receipt.Counts[last].Kept != 0 {
		t.Fatalf("fixture 的最後一列是 %+v，不是 0/0；拿掉它會先動到 total_deleted "+
			"或 kept_newest，量到的就會是別的子句", receipt.Counts[last])
	}
	droppedTable := receipt.Counts[last].Table
	counts := make([]PruneCount, 0, len(receipt.Counts)-1)
	counts = append(counts, receipt.Counts[:last]...)
	receipt.Counts = counts
	tampered, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`UPDATE operator_idempotency SET response_json=? WHERE idempotency_key=?`,
		tampered, req.IdempotencyKey); err != nil {
		t.Fatal(err)
	}

	result, err := st.ApplyOperatorPrune(req)
	if err == nil || !strings.Contains(err.Error(), "idempotency cache is invalid") {
		t.Errorf("got err=%v; want idempotency cache is invalid；operator 手上唯一一份不可逆刪除的紀錄會完全不提 "+
			"%s，讀起來就像保留政策從來沒有涵蓋它", err, droppedTable)
	}
	if !result.Audited || len(result.Counts) != 0 {
		t.Errorf("result=%+v; want Audited=true and no counts；被拒絕的重放不能交回一份殘缺的銷毀紀錄", result)
	}
}
