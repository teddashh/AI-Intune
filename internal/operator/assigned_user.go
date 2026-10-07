package operator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"

	"github.com/teddashh/AI-Intune/internal/store"
	"github.com/teddashh/AI-Intune/internal/tailnet"
)

type MachineAssignedUserRequest struct {
	MachineID          string
	UserID             string
	ExpectedRevision   *int64
	ConfirmDisplayName string
	IdempotencyKey     string
	Actor              Actor
}

type MachineAssignedUserResult = store.OperatorMachineAssignedUserResult

func (s *Service) MachineAssignedUser(machineID string) (MachineAssignedUserResult, error) {
	m, err := s.store.GetMachine(machineID)
	if err != nil {
		return MachineAssignedUserResult{}, err
	}
	return MachineAssignedUserResult{
		MachineID: m.MachineID, DisplayName: m.DisplayName,
		PreviousUserID: m.AssignedUserID, PreviousUserLogin: m.AssignedUserLogin,
		UserID: m.AssignedUserID, UserLogin: m.AssignedUserLogin,
		Revision: m.AssignedUserRevision,
	}, nil
}

func (s *Service) ChangeMachineAssignedUser(ctx context.Context, req MachineAssignedUserRequest) (MachineAssignedUserResult, error) {
	// Digest the operator literal before any directory lookup. Missing user id
	// stays invalid and must never alias explicit "none" on replay.
	digest := AssignedUserSemanticDigest(req)
	login := ""
	var rosterErr error
	if req.UserID != store.AssignedUserNone && req.UserID != "" {
		login, rosterErr = s.assignedUserLogin(ctx, req.UserID)
		if rosterErr != nil {
			login = ""
		}
	}

	before, _ := s.store.GetMachine(req.MachineID)
	auditBase := auditFromActor(req.Actor)
	auditBase.IdempotencyKey = req.IdempotencyKey
	auditBase.RequestDigest = digest
	result, err := s.store.ApplyOperatorMachineAssignedUser(store.OperatorMachineAssignedUserRequest{
		MachineID:          req.MachineID,
		UserID:             req.UserID,
		UserLogin:          login,
		ExpectedRevision:   req.ExpectedRevision,
		ConfirmDisplayName: req.ConfirmDisplayName,
		IdempotencyKey:     req.IdempotencyKey,
		RequestDigest:      digest,
		Audit:              auditBase,
	})
	if rosterErr != nil && !assignedUserDecisionStands(result, err) {
		err = rosterErr
		result = MachineAssignedUserResult{}
	}

	targetID, targetLogin := "", ""
	if req.UserID != store.AssignedUserNone {
		targetID = req.UserID
		targetLogin = login
	}
	entry := auditFromActor(req.Actor)
	entry.Action = store.AuditMachineAssignedUser
	entry.MachineID = req.MachineID
	entry.Subject = req.MachineID
	entry.IdempotencyKey = req.IdempotencyKey
	entry.RequestDigest = digest
	entry.OK = err == nil
	if before.DisplayName != "" {
		entry.Subject = before.DisplayName
		entry.Reason = assignedUserLabel(before.AssignedUserID, before.AssignedUserLogin) + " → " + assignedUserLabel(targetID, targetLogin)
	}
	if err == nil {
		entry.Subject = result.DisplayName
		entry.Reason = assignedUserLabel(result.PreviousUserID, result.PreviousUserLogin) + " → " + assignedUserLabel(result.UserID, result.UserLogin)
		if result.Replayed {
			entry.Detail = store.OperatorIdempotencyReplayPrefix + "沒有再次改 state 或 revision"
		}
	} else {
		entry.Detail = err.Error()
		var rejection *store.OperatorRequestError
		if errors.As(err, &rejection) && rejection.Replayed {
			entry.Detail = store.OperatorIdempotencyReplayPrefix + "原判決：" + entry.Detail
		}
		if errors.Is(err, store.ErrIdempotencyConflict) {
			entry.Detail = "idempotency conflict：" + entry.Detail
		}
	}
	alreadyAudited := result.Audited
	var rejection *store.OperatorRequestError
	if errors.As(err, &rejection) {
		alreadyAudited = rejection.Audited
	}
	if !alreadyAudited {
		if auditErr := s.store.RecordAudit(entry); auditErr != nil {
			log.Printf("operator audit write failed action=%s subject=%s: %v",
				entry.Action, entry.Subject, auditErr)
		}
	}
	s.endClosedTerminalSessions(err, result.ClosedSessionIDs)
	return result, err
}

// AssignedUserSemanticDigest binds the operator literal. Login is resolved
// from the directory and is not part of the request identity.
func AssignedUserSemanticDigest(req MachineAssignedUserRequest) string {
	body := struct {
		UserID             string `json:"user_id"`
		ExpectedRevision   *int64 `json:"expected_revision"`
		ConfirmDisplayName string `json:"confirm_display_name"`
	}{req.UserID, req.ExpectedRevision, req.ConfirmDisplayName}
	raw, _ := json.Marshal(body)
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func (s *Service) assignedUserLogin(ctx context.Context, userID string) (string, error) {
	if strings.HasPrefix(userID, "local:") {
		login, err := s.store.LocalAccountLogin(strings.TrimPrefix(userID, "local:"))
		if err != nil {
			return "", &store.OperatorRequestError{Code: store.OperatorCodeAssignedUserNotInRoster, Detail: "Local account is unavailable"}
		}
		return login, nil
	}
	if s.tailnetSource == nil {
		return "", assignedUserSourceError("沒有 tailnet 來源，不能核對要指派的使用者")
	}
	status := s.tailnetSource.Get(ctx)
	if !status.Available {
		detail := strings.TrimSpace(status.Unavailable)
		if detail == "" {
			detail = "沒有 tailnet 來源，不能核對要指派的使用者"
		}
		return "", assignedUserSourceError(detail)
	}
	directory := tailnet.Users(status)
	if !directory.Available {
		detail := strings.TrimSpace(directory.Unavailable)
		if detail == "" {
			detail = strings.TrimSpace(status.Unavailable)
		}
		if detail == "" {
			detail = "沒有 tailnet 來源，不能核對要指派的使用者"
		}
		return "", assignedUserSourceError(detail)
	}
	for _, user := range directory.Users {
		if user.UserID != userID {
			continue
		}
		login := strings.TrimSpace(user.Login)
		if login == "" {
			return "", &store.OperatorRequestError{
				Code:   store.OperatorCodeBadAssignedUser,
				Detail: "使用者 " + userID + " 沒有 login，不能指派",
			}
		}
		return login, nil
	}
	return "", &store.OperatorRequestError{
		Code:   store.OperatorCodeAssignedUserNotInRoster,
		Detail: fmt.Sprintf("tailnet 上沒有擁有機器的使用者 %s；重新讀取 tailnet 後再指派", userID),
	}
}

func assignedUserSourceError(detail string) error {
	return &store.OperatorRequestError{Code: store.OperatorCodeTailnetSourceUnavailable, Detail: detail}
}

func assignedUserDecisionStands(result MachineAssignedUserResult, err error) bool {
	if err == nil || result.Replayed || result.Audited {
		return true
	}
	var rejection *store.OperatorRequestError
	return errors.As(err, &rejection) && (rejection.Replayed || rejection.Audited)
}

func assignedUserLabel(id, login string) string {
	if login != "" {
		return login
	}
	if id != "" {
		return id
	}
	return "未指派"
}
