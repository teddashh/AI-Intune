package operator

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"

	"github.com/teddashh/AI-Intune/internal/store"
)

type MachineRenamePreviewRequest struct {
	MachineID   string
	DisplayName string
}

type MachineRenamePreviewResult = store.OperatorMachineRenamePreviewResult

type MachineRenameRequest struct {
	MachineID          string
	DisplayName        string
	ConfirmDisplayName string
	PreviewDigest      string
	Reason             string
	IdempotencyKey     string
	Actor              Actor
}

type MachineRenameResult = store.OperatorMachineRenameResult

func (s *Service) PreviewMachineRename(req MachineRenamePreviewRequest) (MachineRenamePreviewResult, error) {
	return s.store.PreviewOperatorMachineRename(req.MachineID, req.DisplayName)
}

func (s *Service) RenameMachine(req MachineRenameRequest) (MachineRenameResult, error) {
	digest := MachineRenameSemanticDigest(req)
	before, _ := s.store.GetMachine(req.MachineID)
	audit := auditFromActor(req.Actor)
	audit.Reason, audit.IdempotencyKey, audit.RequestDigest = req.Reason, req.IdempotencyKey, digest
	result, err := s.store.ApplyOperatorMachineRename(store.OperatorMachineRenameRequest{
		MachineID: req.MachineID, DisplayName: req.DisplayName,
		ConfirmDisplayName: req.ConfirmDisplayName, PreviewDigest: req.PreviewDigest,
		Reason: req.Reason, IdempotencyKey: req.IdempotencyKey, RequestDigest: digest, Audit: audit,
	})

	entry := auditFromActor(req.Actor)
	entry.Action, entry.MachineID = store.AuditMachineRename, req.MachineID
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
		entry.Detail = store.OperatorIdempotencyReplayPrefix + "沒有再次更改名冊顯示名稱"
	} else {
		entry.Detail = result.PreviousDisplayName + " → " + result.DisplayName
	}
	alreadyAudited := result.Audited
	var rejection *store.OperatorRequestError
	if errors.As(err, &rejection) {
		alreadyAudited = rejection.Audited
	}
	if !alreadyAudited {
		if auditErr := s.store.RecordAudit(entry); auditErr != nil {
			log.Printf("operator machine rename audit write failed machine=%s: %v", req.MachineID, auditErr)
		}
	}
	return result, err
}

func MachineRenameSemanticDigest(req MachineRenameRequest) string {
	body := struct {
		MachineID          string `json:"machine_id"`
		DisplayName        string `json:"display_name"`
		ConfirmDisplayName string `json:"confirm_display_name"`
		PreviewDigest      string `json:"preview_digest"`
		Reason             string `json:"reason"`
	}{req.MachineID, req.DisplayName, req.ConfirmDisplayName, req.PreviewDigest, req.Reason}
	raw, _ := json.Marshal(body)
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}
