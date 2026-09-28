package operator

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/model"
	"github.com/teddashh/AI-Intune/internal/restoredrill"
	"github.com/teddashh/AI-Intune/internal/store"
)

func operatorRestoreDrillStore(t *testing.T, path string, machines int) *store.Store {
	t.Helper()
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < machines; index++ {
		token, err := st.CreateEnrollToken("operator-restore-machine", time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := st.RedeemEnrollToken(token, model.EnrollRequest{
			SchemaVersion: model.SchemaVersion, EnrollToken: token, Hostname: "operator-restore-machine",
			OS: "linux", Arch: "amd64", UnixUser: "operator",
		}, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	return st
}

func operatorRestoreDrillBackup(t *testing.T, directory string, machines int) string {
	t.Helper()
	source := filepath.Join(t.TempDir(), "source.sqlite")
	st := operatorRestoreDrillStore(t, source, machines)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "clawctl-20260911T120000Z-before-test.sqlite")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRestoreDrillServicePreviewEnqueueRunAndReplay(t *testing.T) {
	live := operatorRestoreDrillStore(t, filepath.Join(t.TempDir(), "live.sqlite"), 2)
	t.Cleanup(func() { _ = live.Close() })
	directory := filepath.Join(t.TempDir(), "backups")
	operatorRestoreDrillBackup(t, directory, 1)
	stamp := filepath.Join(t.TempDir(), "restore-drill.stamp")
	service := New(live)
	completedAt := time.Date(2026, 9, 11, 12, 30, 0, 0, time.UTC)
	service.ConfigureRestoreDrill(restoredrill.Runner{
		BackupsDir: directory, StampPath: stamp, Live: live, Now: func() time.Time { return completedAt },
	})
	preview, err := service.PreviewRestoreDrill(t.Context(), time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))
	if err != nil || preview.Backup.Name == "" || preview.LiveExpected != 2 || preview.PreviewDigest == "" {
		t.Fatalf("preview=%+v err=%v", preview, err)
	}
	req := RestoreDrillApplyRequest{
		PreviewDigest: preview.PreviewDigest, Confirm: preview.Confirmation,
		Reason: "quarterly recovery verification", IdempotencyKey: "operator-restore-drill-1",
		Actor: Actor{SourceAddr: "100.64.0.7", SourceKind: SourceKindOperatorAPI},
	}
	created, err := service.ApplyRestoreDrill(t.Context(), req)
	if err != nil || created.Operation.OperationID == "" || created.Operation.State != store.RestoreDrillQueued || created.Replayed {
		t.Fatalf("created=%+v err=%v", created, err)
	}
	processed, err := service.RunQueuedRestoreDrillOperations(t.Context())
	if err != nil || processed != 1 {
		t.Fatalf("processed=%d err=%v", processed, err)
	}
	completed, err := service.RestoreDrillOperation(created.Operation.OperationID)
	if err != nil || completed.State != store.RestoreDrillSucceeded || completed.Machines == nil || *completed.Machines != 1 ||
		completed.Expected == nil || *completed.Expected != 1 || completed.LiveExpected == nil || *completed.LiveExpected != 2 {
		t.Fatalf("completed=%+v err=%v", completed, err)
	}
	if stamped, ok := restoredrill.ReadStamp(stamp); !ok || !stamped.Equal(completedAt) {
		t.Fatalf("stamp=%s ok=%t", stamped, ok)
	}
	replay, err := service.ApplyRestoreDrill(t.Context(), req)
	if err != nil || !replay.Replayed || replay.Operation.OperationID == "" ||
		replay.Operation.OperationID != completed.OperationID ||
		replay.Operation.State != store.RestoreDrillSucceeded {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}
}

// ⚠⚠ 回放必須只依帳本上記下的事實，不得依賴呼叫端當下看到的外部世界。
func TestRestoreDrillServiceReplaysAfterANewerBackupAppears(t *testing.T) {
	live := operatorRestoreDrillStore(t, filepath.Join(t.TempDir(), "live.sqlite"), 2)
	t.Cleanup(func() { _ = live.Close() })
	directory := filepath.Join(t.TempDir(), "backups")
	operatorRestoreDrillBackup(t, directory, 1)
	service := New(live)
	service.ConfigureRestoreDrill(restoredrill.Runner{
		BackupsDir: directory, StampPath: filepath.Join(t.TempDir(), "restore-drill.stamp"), Live: live,
	})
	preview, err := service.PreviewRestoreDrill(t.Context(), time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	req := RestoreDrillApplyRequest{
		PreviewDigest: preview.PreviewDigest, Confirm: preview.Confirmation,
		Reason: "quarterly recovery verification", IdempotencyKey: "operator-restore-drill-newer-backup",
		Actor: Actor{SourceAddr: "100.64.0.7", SourceKind: SourceKindOperatorAPI},
	}
	created, err := service.ApplyRestoreDrill(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}

	newerSource := operatorRestoreDrillBackup(t, t.TempDir(), 3)
	raw, err := os.ReadFile(newerSource)
	if err != nil {
		t.Fatal(err)
	}
	newerPath := filepath.Join(directory, "clawctl-20260912T120000Z-before-test.sqlite")
	if err := os.WriteFile(newerPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	newerTime := time.Now().Add(time.Hour)
	if err := os.Chtimes(newerPath, newerTime, newerTime); err != nil {
		t.Fatal(err)
	}

	replay, err := service.ApplyRestoreDrill(t.Context(), req)
	if err != nil || !replay.Replayed || replay.Operation.OperationID != created.Operation.OperationID {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}
}

// ⚠⚠ 被拒回放同樣不得依賴呼叫端當下看到的備份。
func TestRestoreDrillServiceReplaysARejectionAfterANewerBackupAppears(t *testing.T) {
	live := operatorRestoreDrillStore(t, filepath.Join(t.TempDir(), "live.sqlite"), 2)
	t.Cleanup(func() { _ = live.Close() })
	directory := filepath.Join(t.TempDir(), "backups")
	operatorRestoreDrillBackup(t, directory, 1)
	service := New(live)
	service.ConfigureRestoreDrill(restoredrill.Runner{
		BackupsDir: directory, StampPath: filepath.Join(t.TempDir(), "restore-drill.stamp"), Live: live,
	})
	preview, err := service.PreviewRestoreDrill(t.Context(), time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	first := RestoreDrillApplyRequest{
		PreviewDigest: preview.PreviewDigest, Confirm: preview.Confirmation,
		Reason: "quarterly recovery verification", IdempotencyKey: "operator-restore-drill-rejected-newer-1",
		Actor: Actor{SourceAddr: "100.64.0.7", SourceKind: SourceKindOperatorAPI},
	}
	if _, err := service.ApplyRestoreDrill(t.Context(), first); err != nil {
		t.Fatal(err)
	}
	second := first
	second.IdempotencyKey = "operator-restore-drill-rejected-newer-2"
	_, err = service.ApplyRestoreDrill(t.Context(), second)
	var rejected *store.OperatorRequestError
	if !errors.As(err, &rejected) || rejected.Code != store.OperatorCodeRestoreDrillActive || rejected.Replayed {
		t.Fatalf("first rejection=%T %+v", err, err)
	}

	newerSource := operatorRestoreDrillBackup(t, t.TempDir(), 3)
	raw, err := os.ReadFile(newerSource)
	if err != nil {
		t.Fatal(err)
	}
	newerPath := filepath.Join(directory, "clawctl-20260912T120000Z-before-test.sqlite")
	if err := os.WriteFile(newerPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	newerTime := time.Now().Add(time.Hour)
	if err := os.Chtimes(newerPath, newerTime, newerTime); err != nil {
		t.Fatal(err)
	}

	_, err = service.ApplyRestoreDrill(t.Context(), second)
	if !errors.As(err, &rejected) || rejected.Code != store.OperatorCodeRestoreDrillActive || !rejected.Replayed {
		t.Fatalf("replay rejection=%T %+v", err, err)
	}
}

// ⚠⚠ preview 失敗不得覆蓋帳本上已經做出的判決。
func TestRestoreDrillServiceReplaysWhenARetryPreviewFails(t *testing.T) {
	live := operatorRestoreDrillStore(t, filepath.Join(t.TempDir(), "live.sqlite"), 2)
	t.Cleanup(func() { _ = live.Close() })
	directory := filepath.Join(t.TempDir(), "backups")
	operatorRestoreDrillBackup(t, directory, 1)
	service := New(live)
	service.ConfigureRestoreDrill(restoredrill.Runner{
		BackupsDir: directory, StampPath: filepath.Join(t.TempDir(), "restore-drill.stamp"), Live: live,
	})
	preview, err := service.PreviewRestoreDrill(t.Context(), time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	req := RestoreDrillApplyRequest{
		PreviewDigest: preview.PreviewDigest, Confirm: preview.Confirmation,
		Reason: "quarterly recovery verification", IdempotencyKey: "operator-restore-drill-preview-timeout",
		Actor: Actor{SourceAddr: "100.64.0.7", SourceKind: SourceKindOperatorAPI},
	}
	created, err := service.ApplyRestoreDrill(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	replay, err := service.ApplyRestoreDrill(ctx, req)
	if err != nil || !replay.Replayed || replay.Operation.OperationID != created.Operation.OperationID {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}

	var failedAudits, receipts int
	if err := live.DB().QueryRow(`SELECT COUNT(*) FROM audit_log WHERE idempotency_key=? AND detail='還原演練目前無法建立'`,
		req.IdempotencyKey).Scan(&failedAudits); err != nil {
		t.Fatal(err)
	}
	if err := live.DB().QueryRow(`SELECT COUNT(*) FROM operator_idempotency WHERE idempotency_key=?`,
		req.IdempotencyKey).Scan(&receipts); err != nil {
		t.Fatal(err)
	}
	if failedAudits != 0 || receipts != 1 {
		t.Fatalf("failedAudits=%d receipts=%d", failedAudits, receipts)
	}
}

func TestRestoreDrillServiceAuditsAFailedPreviewWithoutConsumingTheKey(t *testing.T) {
	live := operatorRestoreDrillStore(t, filepath.Join(t.TempDir(), "live.sqlite"), 1)
	t.Cleanup(func() { _ = live.Close() })
	directory := filepath.Join(t.TempDir(), "backups")
	operatorRestoreDrillBackup(t, directory, 1)
	service := New(live)
	service.ConfigureRestoreDrill(restoredrill.Runner{
		BackupsDir: directory, StampPath: filepath.Join(t.TempDir(), "restore-drill.stamp"), Live: live,
	})
	req := RestoreDrillApplyRequest{
		PreviewDigest: "sha256:" + strings.Repeat("a", 64), Confirm: "VERIFY backup.sqlite",
		Reason: "quarterly recovery verification", IdempotencyKey: "operator-restore-drill-preview-canceled-miss",
		Actor: Actor{SourceAddr: "100.64.0.7", SourceKind: SourceKindOperatorAPI},
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := service.ApplyRestoreDrill(ctx, req); !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}

	var failedAudits, receipts int
	if err := live.DB().QueryRow(`SELECT COUNT(*) FROM audit_log WHERE idempotency_key=? AND outcome='failed' AND detail='還原演練目前無法建立'`,
		req.IdempotencyKey).Scan(&failedAudits); err != nil {
		t.Fatal(err)
	}
	if err := live.DB().QueryRow(`SELECT COUNT(*) FROM operator_idempotency WHERE idempotency_key=?`,
		req.IdempotencyKey).Scan(&receipts); err != nil {
		t.Fatal(err)
	}
	if failedAudits != 1 || receipts != 0 {
		t.Fatalf("failedAudits=%d receipts=%d", failedAudits, receipts)
	}
}

func TestRestoreDrillServicePersistsTypedFailureWithoutStamp(t *testing.T) {
	live := operatorRestoreDrillStore(t, filepath.Join(t.TempDir(), "live.sqlite"), 1)
	t.Cleanup(func() { _ = live.Close() })
	directory := filepath.Join(t.TempDir(), "backups")
	operatorRestoreDrillBackup(t, directory, 0)
	stamp := filepath.Join(t.TempDir(), "stamp")
	service := New(live)
	service.ConfigureRestoreDrill(restoredrill.Runner{BackupsDir: directory, StampPath: stamp, Live: live})
	preview, err := service.PreviewRestoreDrill(t.Context(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	created, err := service.ApplyRestoreDrill(t.Context(), RestoreDrillApplyRequest{
		PreviewDigest: preview.PreviewDigest, Confirm: preview.Confirmation, Reason: "verify empty backup handling",
		IdempotencyKey: "operator-restore-drill-empty", Actor: Actor{SourceKind: SourceKindOperatorAPI},
	})
	if err != nil {
		t.Fatal(err)
	}
	if processed, err := service.RunQueuedRestoreDrillOperations(t.Context()); err != nil || processed != 1 {
		t.Fatalf("processed=%d err=%v", processed, err)
	}
	failed, err := service.RestoreDrillOperation(created.Operation.OperationID)
	if err != nil || failed.State != store.RestoreDrillFailed || failed.ErrorCode == nil ||
		*failed.ErrorCode != RestoreDrillFailureEmptyRegistry {
		t.Fatalf("failed=%+v err=%v", failed, err)
	}
	if _, ok := restoredrill.ReadStamp(stamp); ok {
		t.Fatal("failed drill wrote completion stamp")
	}
}

func TestRestoreDrillServiceAuditsApplyWhenNoBackupIsAvailable(t *testing.T) {
	live := operatorRestoreDrillStore(t, filepath.Join(t.TempDir(), "live.sqlite"), 1)
	t.Cleanup(func() { _ = live.Close() })
	service := New(live)
	service.ConfigureRestoreDrill(restoredrill.Runner{
		BackupsDir: filepath.Join(t.TempDir(), "missing-backups"),
		StampPath:  filepath.Join(t.TempDir(), "stamp"),
		Live:       live,
	})
	req := RestoreDrillApplyRequest{
		PreviewDigest: "sha256:untrusted", Confirm: "VERIFY absent.sqlite",
		Reason: "verify missing backup handling", IdempotencyKey: "operator-restore-drill-no-backup",
		Actor: Actor{SourceAddr: "100.64.0.8", SourceKind: SourceKindOperatorAPI},
	}
	if _, err := service.ApplyRestoreDrill(t.Context(), req); !errors.Is(err, restoredrill.ErrNoBackup) {
		t.Fatalf("err=%v", err)
	}
	audit, err := live.Audit("", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(audit) != 1 || audit[0].Action != store.AuditRestoreDrill || audit[0].OK ||
		audit[0].IdempotencyKey != req.IdempotencyKey || audit[0].RequestDigest != RestoreDrillSemanticDigest(req) ||
		audit[0].Detail != "還原演練目前無法建立" {
		t.Fatalf("audit=%+v", audit)
	}
}
