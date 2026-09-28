package operator

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"

	"github.com/teddashh/AI-Intune/internal/store"
)

type MachineNotesPreviewRequest struct {
	MachineID string
	Notes     string
}

type MachineNotesPreviewResult = store.OperatorMachineNotesPreviewResult

type MachineNotesRequest struct {
	MachineID          string
	Notes              string
	ConfirmDisplayName string
	PreviewDigest      string
	Reason             string
	IdempotencyKey     string
	Actor              Actor
}

type MachineNotesResult = store.OperatorMachineNotesResult

func (s *Service) PreviewMachineNotes(req MachineNotesPreviewRequest) (MachineNotesPreviewResult, error) {
	return s.store.PreviewOperatorMachineNotes(req.MachineID, req.Notes)
}

func (s *Service) UpdateMachineNotes(req MachineNotesRequest) (MachineNotesResult, error) {
	digest := MachineNotesSemanticDigest(req)
	before, _ := s.store.GetMachine(req.MachineID)
	audit := auditFromActor(req.Actor)
	audit.Reason, audit.IdempotencyKey, audit.RequestDigest = req.Reason, req.IdempotencyKey, digest
	result, err := s.store.ApplyOperatorMachineNotes(store.OperatorMachineNotesRequest{
		MachineID: req.MachineID, Notes: req.Notes,
		ConfirmDisplayName: req.ConfirmDisplayName, PreviewDigest: req.PreviewDigest,
		Reason: req.Reason, IdempotencyKey: req.IdempotencyKey, RequestDigest: digest, Audit: audit,
	})

	entry := auditFromActor(req.Actor)
	entry.Action, entry.MachineID = store.AuditMachineNotes, req.MachineID
	entry.Subject, entry.Reason = req.MachineID, req.Reason
	entry.IdempotencyKey, entry.RequestDigest, entry.OK = req.IdempotencyKey, digest, err == nil
	if before.DisplayName != "" {
		entry.Subject = before.DisplayName
	}
	if result.DisplayName != "" {
		entry.Subject = result.DisplayName
	}
	if err != nil {
		entry.Detail = err.Error()
	} else if result.Replayed {
		entry.Detail = store.OperatorIdempotencyReplayPrefix + "沒有再次更改名冊備註"
	} else if result.NotesPresent {
		entry.Detail = "名冊備註已更新；機器設定與 agent 不變"
	} else {
		entry.Detail = "名冊備註已清除；機器設定與 agent 不變"
	}
	alreadyAudited := result.Audited
	var rejection *store.OperatorRequestError
	if errors.As(err, &rejection) {
		alreadyAudited = rejection.Audited
	}
	if !alreadyAudited {
		if auditErr := s.store.RecordAudit(entry); auditErr != nil {
			log.Printf("operator machine notes audit 寫入失敗 machine=%s: %v", req.MachineID, auditErr)
		}
	}
	return result, err
}

func MachineNotesSemanticDigest(req MachineNotesRequest) string {
	body := struct {
		MachineID          string `json:"machine_id"`
		Notes              string `json:"notes"`
		ConfirmDisplayName string `json:"confirm_display_name"`
		PreviewDigest      string `json:"preview_digest"`
		Reason             string `json:"reason"`
	}{req.MachineID, req.Notes, req.ConfirmDisplayName, req.PreviewDigest, req.Reason}
	raw, _ := json.Marshal(body)
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}
