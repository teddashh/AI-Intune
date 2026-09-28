package operator

import (
	"errors"

	"github.com/teddashh/AI-Intune/internal/store"
)

type VerificationAssignmentPreviewRequest struct {
	VerifierID string
	JobID      string
}

type VerificationAssignmentPreviewResult = store.OperatorVerificationAssignmentPreviewResult

type VerificationAssignmentRequest struct {
	VerifierID          string
	JobID               string
	ConfirmVerifierName string
	PreviewDigest       string
	Reason              string
	IdempotencyKey      string
	Actor               Actor
}

type VerificationAssignmentResult = store.OperatorVerificationAssignmentResult

func (s *Service) PreviewVerificationAssignment(req VerificationAssignmentPreviewRequest) (
	VerificationAssignmentPreviewResult, error,
) {
	return s.store.PreviewOperatorVerificationAssignment(req.VerifierID, req.JobID)
}

// AssignVerification does not re-validate intent before entering Store: a
// matching idempotency key must replay its historical result after policy
// evolves. Store validates only on a cache miss, inside the writer transaction.
func (s *Service) AssignVerification(req VerificationAssignmentRequest) (VerificationAssignmentResult, error) {
	assignedBy, err := deploymentCreatedBy(req.Actor)
	if err != nil {
		return VerificationAssignmentResult{}, err
	}
	digest := VerificationAssignmentSemanticDigest(req)
	auditBase := auditFromActor(req.Actor)
	auditBase.Reason = req.Reason
	auditBase.IdempotencyKey = req.IdempotencyKey
	auditBase.RequestDigest = digest
	result, err := s.store.ApplyOperatorVerificationAssignment(store.OperatorVerificationAssignmentRequest{
		VerifierID: req.VerifierID, JobID: req.JobID,
		ConfirmVerifierName: req.ConfirmVerifierName, PreviewDigest: req.PreviewDigest,
		Reason: req.Reason, AssignedBy: assignedBy, IdempotencyKey: req.IdempotencyKey,
		RequestDigest: digest, Audit: auditBase,
	})

	entry := auditFromActor(req.Actor)
	entry.Action = store.AuditVerificationAssign
	entry.Subject = req.JobID
	entry.Reason = req.Reason
	entry.IdempotencyKey = req.IdempotencyKey
	entry.RequestDigest = digest
	entry.OK = err == nil
	if result.MachineID != "" {
		entry.MachineID = result.MachineID
	}
	if err != nil {
		entry.Detail = err.Error()
		var rejection *store.OperatorRequestError
		if errors.As(err, &rejection) && rejection.Replayed {
			entry.Detail = store.OperatorIdempotencyReplayPrefix + "原判決：" + entry.Detail
		}
		if errors.Is(err, store.ErrIdempotencyConflict) {
			entry.Detail = "idempotency conflict：" + entry.Detail
		}
	} else if result.Replayed {
		entry.Detail = store.OperatorIdempotencyReplayPrefix + "沒有再開一張派工；原本那一張還在"
	}
	recordOperatorFallbackAudit(s.store, entry, result.Audited, err)
	return result, err
}

// VerificationAssignmentSemanticDigest binds every domain-significant field,
// Reason included: changing it while reusing a key must conflict rather than
// replay an audit statement the caller did not send.
func VerificationAssignmentSemanticDigest(req VerificationAssignmentRequest) string {
	body := struct {
		VerifierID          string `json:"verifier_id"`
		JobID               string `json:"job_id"`
		ConfirmVerifierName string `json:"confirm_verifier_name"`
		PreviewDigest       string `json:"preview_digest"`
		Reason              string `json:"reason"`
	}{req.VerifierID, req.JobID, req.ConfirmVerifierName, req.PreviewDigest, req.Reason}
	return semanticDigestOf(body)
}
