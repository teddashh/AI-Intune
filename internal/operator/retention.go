package operator

import (
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/teddashh/AI-Intune/internal/store"
)

type RetentionStatus = store.OperatorRetentionStatus
type RetentionPolicy = store.OperatorRetentionPolicy
type RetentionPrunePreview = store.OperatorPrunePreview
type RetentionPruneResult = store.OperatorPruneResult

type RetentionPrunePreviewRequest struct {
	EvaluatedAt time.Time
	Policy      store.RetentionPolicy
}

type RetentionPruneApplyRequest struct {
	EvaluatedAt      time.Time
	Policy           RetentionPolicy
	ExpectedRevision int64
	Confirm          string
	PreviewDigest    string
	Reason           string
	IdempotencyKey   string
	Actor            Actor
}

type RetentionTransportRejectionCode string

const (
	RetentionTransportRejectionEvaluatedAtInvalid RetentionTransportRejectionCode = "RETENTION_FORM_EVALUATED_AT_INVALID"
	RetentionTransportRejectionCoordinatesInvalid RetentionTransportRejectionCode = "RETENTION_FORM_COORDINATES_INVALID"
)

type RetentionTransportRejectionRequest struct {
	Code           RetentionTransportRejectionCode
	IdempotencyKey string
	Actor          Actor
}

func (s *Service) RetentionStatus(policy store.RetentionPolicy, now time.Time) (RetentionStatus, error) {
	return s.store.OperatorRetentionStatus(now, policy)
}

func (s *Service) PreviewRetentionPrune(req RetentionPrunePreviewRequest) (RetentionPrunePreview, error) {
	return s.store.PreviewOperatorPrune(req.EvaluatedAt, req.Policy)
}

func (s *Service) ApplyRetentionPrune(req RetentionPruneApplyRequest) (RetentionPruneResult, error) {
	storeReq := store.OperatorPruneRequest{
		EvaluatedAt: req.EvaluatedAt, Policy: req.Policy,
		ExpectedRevision: req.ExpectedRevision, Confirm: req.Confirm,
		PreviewDigest: req.PreviewDigest, Reason: req.Reason,
		IdempotencyKey: req.IdempotencyKey,
		Audit:          auditFromActor(req.Actor),
	}
	storeReq.RequestDigest = store.OperatorPruneSemanticDigest(storeReq)
	result, err := s.store.ApplyOperatorPrune(storeReq)
	if err == nil {
		return result, nil
	}
	var rejection *store.OperatorRequestError
	alreadyAudited := result.Audited
	if errors.As(err, &rejection) {
		alreadyAudited = rejection.Audited
	}
	if !alreadyAudited {
		entry := auditFromActor(req.Actor)
		entry.Action, entry.Subject, entry.Reason = store.AuditRetentionPrune, "retention", req.Reason
		entry.IdempotencyKey, entry.RequestDigest = req.IdempotencyKey, storeReq.RequestDigest
		entry.OK, entry.Detail = false, err.Error()
		if auditErr := s.store.RecordAudit(entry); auditErr != nil {
			log.Printf("operator retention audit failed: %v", auditErr)
		}
	}
	return result, err
}

// RecordRetentionTransportRejection owns the audit shape for a Web form that
// could not become a canonical retention request. It does not create an
// idempotency receipt or persist unparsed coordinates.
func (s *Service) RecordRetentionTransportRejection(req RetentionTransportRejectionRequest) error {
	switch req.Code {
	case RetentionTransportRejectionEvaluatedAtInvalid,
		RetentionTransportRejectionCoordinatesInvalid:
	default:
		return fmt.Errorf("operator: invalid retention transport rejection code %q", req.Code)
	}
	entry := auditFromActor(req.Actor)
	entry.Action = store.AuditRetentionPrune
	entry.Subject = "retention"
	entry.IdempotencyKey = req.IdempotencyKey
	entry.Detail = store.OperatorTransportRejectionPrefix + string(req.Code) + ": canonical request digest 無法取得"
	return s.store.RecordAudit(entry)
}
