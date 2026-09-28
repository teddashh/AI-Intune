package operator

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"

	"github.com/teddashh/AI-Intune/internal/store"
)

type MachineLifecycleState = store.MachineLifecycleState
type MachineLifecycleBlocker = store.MachineLifecycleBlocker

const (
	MachineLifecycleStateActive  = store.MachineLifecycleActive
	MachineLifecycleStateRetired = store.MachineLifecycleRetired

	MachineLifecycleBlockerNonterminalJobs = store.MachineLifecycleBlockerNonterminalJobs
)

type MachineLifecycleReadResult = store.OperatorMachineLifecycleReadResult

type MachineLifecyclePreviewRequest struct {
	MachineID        string
	DesiredState     MachineLifecycleState
	ExpectedRevision *int64
}

type MachineLifecyclePreviewResult = store.OperatorMachineLifecyclePreviewResult

type MachineLifecycleRequest struct {
	MachineID          string
	DesiredState       MachineLifecycleState
	ExpectedRevision   *int64
	ConfirmDisplayName string
	PreviewDigest      string
	Reason             string
	IdempotencyKey     string
	Actor              Actor
}

type MachineLifecycleResult = store.OperatorMachineLifecycleResult

func (s *Service) MachineLifecycle(machineID string) (MachineLifecycleReadResult, error) {
	return s.store.OperatorMachineLifecycle(machineID)
}

func (s *Service) PreviewMachineLifecycle(req MachineLifecyclePreviewRequest) (MachineLifecyclePreviewResult, error) {
	return s.store.PreviewOperatorMachineLifecycle(store.OperatorMachineLifecyclePreviewRequest{
		MachineID: req.MachineID, DesiredState: req.DesiredState,
		ExpectedRevision: req.ExpectedRevision,
	})
}

// ChangeMachineLifecycle deliberately enters Store before doing mutable-state
// validation. A matching Idempotency-Key must replay its historical decision,
// even if the machine has since moved to another lifecycle state.
func (s *Service) ChangeMachineLifecycle(req MachineLifecycleRequest) (MachineLifecycleResult, error) {
	digest := MachineLifecycleSemanticDigest(req)
	before, _ := s.store.OperatorMachineLifecycle(req.MachineID)
	auditBase := auditFromActor(req.Actor)
	auditBase.Reason = req.Reason
	auditBase.IdempotencyKey = req.IdempotencyKey
	auditBase.RequestDigest = digest
	result, err := s.store.ApplyOperatorMachineLifecycle(store.OperatorMachineLifecycleRequest{
		MachineID: req.MachineID, DesiredState: req.DesiredState,
		ExpectedRevision: req.ExpectedRevision, ConfirmDisplayName: req.ConfirmDisplayName,
		PreviewDigest: req.PreviewDigest, Reason: req.Reason,
		IdempotencyKey: req.IdempotencyKey, RequestDigest: digest, Audit: auditBase,
	})

	entry := auditFromActor(req.Actor)
	entry.Action = store.AuditMachineLifecycle
	entry.MachineID = req.MachineID
	entry.Subject = req.MachineID
	entry.Reason = req.Reason
	entry.IdempotencyKey = req.IdempotencyKey
	entry.RequestDigest = digest
	entry.OK = err == nil
	if before.DisplayName != "" {
		entry.Subject = before.DisplayName
	}
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
		entry.Detail = store.OperatorIdempotencyReplayPrefix + "沒有再次改 lifecycle state、revision 或 transition ledger"
	} else if result.NoOp {
		entry.Detail = "lifecycle no-op；state、revision 與 transition ledger 都未改變"
	}
	alreadyAudited := result.Audited
	var rejection *store.OperatorRequestError
	if errors.As(err, &rejection) {
		alreadyAudited = rejection.Audited
	}
	if !alreadyAudited {
		if auditErr := s.store.RecordAudit(entry); auditErr != nil {
			log.Printf("operator lifecycle audit 寫入失敗 subject=%s: %v", entry.Subject, auditErr)
		}
	}
	return result, err
}

// MachineLifecycleSemanticDigest binds the path identity and every
// domain-significant request literal. Actor provenance is audit evidence, not
// part of the idempotent operation's meaning.
func MachineLifecycleSemanticDigest(req MachineLifecycleRequest) string {
	body := struct {
		MachineID          string                `json:"machine_id"`
		DesiredState       MachineLifecycleState `json:"desired_state"`
		ExpectedRevision   *int64                `json:"expected_revision"`
		ConfirmDisplayName string                `json:"confirm_display_name"`
		PreviewDigest      string                `json:"preview_digest"`
		Reason             string                `json:"reason"`
	}{
		req.MachineID, req.DesiredState, req.ExpectedRevision,
		req.ConfirmDisplayName, req.PreviewDigest, req.Reason,
	}
	raw, _ := json.Marshal(body)
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}
