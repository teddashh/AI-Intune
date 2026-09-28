package operator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"time"

	"github.com/teddashh/AI-Intune/internal/restoredrill"
	"github.com/teddashh/AI-Intune/internal/store"
)

var ErrRestoreDrillUnavailable = errors.New("operator: restore drill is unavailable")

const (
	RestoreDrillFailureNoBackup      = "RESTORE_DRILL_NO_BACKUP"
	RestoreDrillFailureUnsafeBackup  = "RESTORE_DRILL_BACKUP_UNSAFE"
	RestoreDrillFailureBackupChanged = "RESTORE_DRILL_BACKUP_CHANGED"
	RestoreDrillFailureEmptyRegistry = "RESTORE_DRILL_EMPTY_REGISTRY"
	RestoreDrillFailureVerification  = "RESTORE_DRILL_VERIFICATION_FAILED"
	RestoreDrillFailureStamp         = "RESTORE_DRILL_STAMP_FAILED"
)

type RestoreDrillPreview struct {
	SchemaVersion int                      `json:"schema_version"`
	EvaluatedAt   time.Time                `json:"evaluated_at"`
	Backup        store.RestoreDrillBackup `json:"backup"`
	LiveExpected  int                      `json:"live_expected"`
	Confirmation  string                   `json:"confirmation"`
	PreviewDigest string                   `json:"preview_digest"`
}

type RestoreDrillApplyRequest struct {
	PreviewDigest  string
	Confirm        string
	Reason         string
	IdempotencyKey string
	Actor          Actor
}

func (s *Service) PreviewRestoreDrill(ctx context.Context, now time.Time) (RestoreDrillPreview, error) {
	if s == nil || s.restoreDrill == nil || ctx == nil {
		return RestoreDrillPreview{}, ErrRestoreDrillUnavailable
	}
	prepared, err := s.restoreDrill.Preview(ctx)
	if err != nil {
		return RestoreDrillPreview{}, err
	}
	preview := RestoreDrillPreview{
		SchemaVersion: 1, EvaluatedAt: now.UTC().Truncate(time.Second),
		Backup: restoreDrillStoreBackup(prepared.Backup), LiveExpected: prepared.LiveExpected,
	}
	preview.Confirmation = store.RestoreDrillConfirmation(preview.Backup.Name)
	preview.PreviewDigest = restoreDrillPreviewDigest(preview)
	return preview, nil
}

func (s *Service) ApplyRestoreDrill(ctx context.Context, req RestoreDrillApplyRequest) (store.OperatorRestoreDrillResult, error) {
	if s == nil || s.restoreDrill == nil || ctx == nil {
		return store.OperatorRestoreDrillResult{}, ErrRestoreDrillUnavailable
	}
	requestDigest := RestoreDrillSemanticDigest(req)
	current, err := s.PreviewRestoreDrill(ctx, time.Now())
	if err != nil {
		// ⚠ preview 失敗仍要先問帳本，避免把同一 request identity 的既有判決覆寫成全新失敗。
		replay, replayed, replayErr := s.store.ReplayOperatorRestoreDrill(store.OperatorRestoreDrillRequest{
			PreviewDigest: req.PreviewDigest, Confirm: req.Confirm, Reason: req.Reason,
			IdempotencyKey: req.IdempotencyKey, RequestDigest: requestDigest, Audit: auditFromActor(req.Actor),
		})
		if replayed || replayErr != nil {
			return replay, replayErr
		}
		entry := auditFromActor(req.Actor)
		entry.Action, entry.Subject, entry.Reason = store.AuditRestoreDrill, "restore drill", req.Reason
		entry.IdempotencyKey, entry.RequestDigest = req.IdempotencyKey, requestDigest
		entry.OK, entry.Detail = false, "還原演練目前無法建立"
		if auditErr := s.store.RecordAudit(entry); auditErr != nil {
			log.Printf("operator restore drill audit failed: %v", auditErr)
		}
		return store.OperatorRestoreDrillResult{}, err
	}
	storeReq := store.OperatorRestoreDrillRequest{
		PreviewDigest: req.PreviewDigest, Confirm: req.Confirm, Reason: req.Reason,
		IdempotencyKey: req.IdempotencyKey, Audit: auditFromActor(req.Actor),
		Prepared: store.RestoreDrillPrepared{
			Backup: current.Backup, LiveExpectedAtPreview: current.LiveExpected,
			CurrentPreviewDigest: current.PreviewDigest,
		},
	}
	storeReq.RequestDigest = requestDigest
	result, err := s.store.ApplyOperatorRestoreDrill(storeReq)
	if err == nil {
		return result, nil
	}
	var rejection *store.OperatorRequestError
	audited := result.Audited
	if errors.As(err, &rejection) {
		audited = rejection.Audited
	}
	if !audited {
		entry := auditFromActor(req.Actor)
		entry.Action, entry.Subject, entry.Reason = store.AuditRestoreDrill, "restore drill", req.Reason
		entry.IdempotencyKey, entry.RequestDigest = req.IdempotencyKey, storeReq.RequestDigest
		entry.OK, entry.Detail = false, "還原演練目前無法建立"
		if auditErr := s.store.RecordAudit(entry); auditErr != nil {
			log.Printf("operator restore drill audit failed: %v", auditErr)
		}
	}
	return result, err
}

func RestoreDrillSemanticDigest(req RestoreDrillApplyRequest) string {
	raw, _ := json.Marshal(struct {
		PreviewDigest string `json:"preview_digest"`
		Confirm       string `json:"confirm"`
		Reason        string `json:"reason"`
	}{req.PreviewDigest, req.Confirm, req.Reason})
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func restoreDrillPreviewDigest(preview RestoreDrillPreview) string {
	raw, _ := json.Marshal(struct {
		SchemaVersion int                      `json:"schema_version"`
		Backup        store.RestoreDrillBackup `json:"backup"`
		LiveExpected  int                      `json:"live_expected"`
		Confirmation  string                   `json:"confirmation"`
	}{preview.SchemaVersion, preview.Backup, preview.LiveExpected, preview.Confirmation})
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func restoreDrillStoreBackup(backup restoredrill.Backup) store.RestoreDrillBackup {
	return store.RestoreDrillBackup{
		Name: backup.Name, SizeBytes: backup.SizeBytes,
		ModifiedAt: backup.ModifiedAt, SHA256: backup.SHA256,
	}
}

func restoreDrillRunnerBackup(backup store.RestoreDrillBackup) restoredrill.Backup {
	return restoredrill.Backup{
		Name: backup.Name, SizeBytes: backup.SizeBytes,
		ModifiedAt: backup.ModifiedAt, SHA256: backup.SHA256,
	}
}

func (s *Service) RestoreDrillOperation(id string) (store.RestoreDrillOperation, error) {
	return s.store.GetRestoreDrillOperation(id)
}

func (s *Service) RestoreDrillOperations(limit int) (store.RestoreDrillListResult, error) {
	return s.store.ListRestoreDrillOperations(limit)
}

func (s *Service) RecoverRestoreDrillOperations(ctx context.Context) (int, error) {
	if s == nil || s.restoreDrill == nil || ctx == nil {
		return 0, ErrRestoreDrillUnavailable
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	return s.store.FenceRunningRestoreDrillOperations()
}

func (s *Service) RunQueuedRestoreDrillOperations(ctx context.Context) (int, error) {
	if s == nil || s.restoreDrill == nil || ctx == nil {
		return 0, ErrRestoreDrillUnavailable
	}
	processed := 0
	for _, state := range []store.RestoreDrillState{store.RestoreDrillRunning, store.RestoreDrillQueued} {
		ids, err := s.store.ListRestoreDrillOperationIDsForWorker(state)
		if err != nil {
			return processed, err
		}
		for _, id := range ids {
			if err := ctx.Err(); err != nil {
				return processed, err
			}
			if _, err := s.runRestoreDrillOperation(ctx, id, state == store.RestoreDrillRunning); err != nil {
				return processed, err
			}
			processed++
		}
	}
	return processed, nil
}

func (s *Service) runRestoreDrillOperation(ctx context.Context, id string, reclaim bool) (store.RestoreDrillOperation, error) {
	claim, err := s.store.ClaimRestoreDrillOperation(id, reclaim)
	if err != nil {
		return store.RestoreDrillOperation{}, err
	}
	result, runErr := s.restoreDrill.Run(ctx, restoreDrillRunnerBackup(claim.Operation.Backup))
	if runErr != nil {
		if ctx.Err() != nil {
			return claim.Operation, ctx.Err()
		}
		code, detail := classifyRestoreDrillFailure(runErr)
		return s.store.FailRestoreDrillOperation(id, claim.RunToken, code, detail)
	}
	var newest *time.Time
	if !result.NewestAt.IsZero() {
		value := result.NewestAt.UTC()
		newest = &value
	}
	milliseconds := result.Took.Milliseconds()
	if milliseconds < 0 {
		milliseconds = 0
	}
	return s.store.SucceedRestoreDrillOperation(id, claim.RunToken, store.RestoreDrillWorkerResult{
		Backup: restoreDrillStoreBackup(result.Backup), Machines: result.Machines,
		Expected: result.Expected, LiveExpected: result.LiveExpected,
		NewestCheckinAt: newest, DurationMilliseconds: milliseconds,
	})
}

func classifyRestoreDrillFailure(err error) (string, string) {
	switch {
	case errors.Is(err, restoredrill.ErrNoBackup):
		return RestoreDrillFailureNoBackup, "找不到可供驗證的備份"
	case errors.Is(err, restoredrill.ErrUnsafeBackup):
		return RestoreDrillFailureUnsafeBackup, "最新備份不是可安全開啟的 standalone regular file"
	case errors.Is(err, restoredrill.ErrBackupChanged):
		return RestoreDrillFailureBackupChanged, "預覽選定的備份已改變"
	case errors.Is(err, restoredrill.ErrEmptyRegistry):
		return RestoreDrillFailureEmptyRegistry, "備份可開啟但名冊為空"
	case errors.Is(err, restoredrill.ErrStamp):
		return RestoreDrillFailureStamp, "驗證完成但無法保存完成章"
	default:
		return RestoreDrillFailureVerification, "備份副本未通過還原驗證"
	}
}
