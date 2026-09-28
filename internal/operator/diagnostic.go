package operator

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"strings"

	"github.com/teddashh/AI-Intune/internal/store"
)

type DiagnosticNoopPreviewRequest struct {
	MachineID               string
	ExecutionTimeoutSeconds int
}

type DiagnosticNoopPreviewResult = store.OperatorDiagnosticNoopPreviewResult

type DiagnosticNoopRequest struct {
	MachineID               string
	ExecutionTimeoutSeconds int
	ConfirmDisplayName      string
	PreviewDigest           string
	Reason                  string
	IdempotencyKey          string
	Actor                   Actor
}

type DiagnosticNoopResult = store.OperatorDiagnosticNoopResult

func (s *Service) PreviewDiagnosticNoop(req DiagnosticNoopPreviewRequest) (DiagnosticNoopPreviewResult, error) {
	return s.store.PreviewOperatorDiagnosticNoop(req.MachineID, req.ExecutionTimeoutSeconds)
}

// CreateDiagnosticNoop is the shared service boundary for Web, JSON and
// stopped-service CLI adapters. Actor provenance is not operation meaning, but
// the original principal/source is retained on the desired-state ledger and
// the atomic audit evidence.
func (s *Service) CreateDiagnosticNoop(req DiagnosticNoopRequest) (DiagnosticNoopResult, error) {
	digest := DiagnosticNoopSemanticDigest(req)
	auditBase := auditFromActor(req.Actor)
	auditBase.Reason = req.Reason
	auditBase.IdempotencyKey = req.IdempotencyKey
	auditBase.RequestDigest = digest
	result, err := s.store.ApplyOperatorDiagnosticNoop(store.OperatorDiagnosticNoopRequest{
		MachineID: req.MachineID, ExecutionTimeout: req.ExecutionTimeoutSeconds,
		ConfirmDisplayName: req.ConfirmDisplayName, PreviewDigest: req.PreviewDigest,
		Reason: req.Reason, IdempotencyKey: req.IdempotencyKey, RequestDigest: digest,
		CreatedBy: diagnosticNoopCreatedBy(req.Actor), Audit: auditBase,
	})

	entry := auditFromActor(req.Actor)
	entry.Action = store.AuditDiagnosticNoop
	entry.MachineID, entry.Subject = req.MachineID, req.MachineID
	entry.Reason, entry.IdempotencyKey, entry.RequestDigest = req.Reason, req.IdempotencyKey, digest
	entry.OK = err == nil
	if result.DisplayName != "" {
		entry.Subject = result.DisplayName
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
		entry.Detail = store.OperatorIdempotencyReplayPrefix + "replayed"
	}
	alreadyAudited := result.Audited
	var rejection *store.OperatorRequestError
	if errors.As(err, &rejection) {
		alreadyAudited = rejection.Audited
	}
	if !alreadyAudited {
		if auditErr := s.store.RecordAudit(entry); auditErr != nil {
			log.Printf("operator diagnostic noop audit 寫入失敗 subject=%s: %v", entry.Subject, auditErr)
		}
	}
	return result, err
}

func DiagnosticNoopSemanticDigest(req DiagnosticNoopRequest) string {
	body := struct {
		MachineID               string `json:"machine_id"`
		ExecutionTimeoutSeconds int    `json:"execution_timeout_seconds"`
		ConfirmDisplayName      string `json:"confirm_display_name"`
		PreviewDigest           string `json:"preview_digest"`
		Reason                  string `json:"reason"`
	}{req.MachineID, req.ExecutionTimeoutSeconds, req.ConfirmDisplayName, req.PreviewDigest, req.Reason}
	raw, _ := json.Marshal(body)
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func diagnosticNoopCreatedBy(actor Actor) string {
	for _, candidate := range []string{actor.AuthSubject, actor.WhoUser, actor.WhoNode, actor.SourceKind} {
		if value := strings.TrimSpace(candidate); value != "" {
			if len(value) > 470 {
				sum := sha256.Sum256([]byte(value))
				value = "oversized-provenance-sha256:" + hex.EncodeToString(sum[:])
			}
			return "operator:" + value
		}
	}
	return "operator:provenance-unavailable"
}
