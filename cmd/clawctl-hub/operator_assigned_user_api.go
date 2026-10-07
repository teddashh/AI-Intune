package main

import (
	"errors"
	"fmt"
	"log"
	"mime"
	"net/http"

	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/store"
)

type machineAssignedUserOperatorRequest struct {
	UserID             string `json:"user_id"`
	ExpectedRevision   *int64 `json:"expected_revision"`
	ConfirmDisplayName string `json:"confirm_display_name"`
}

// controlPlaneService is the operator service for lifecycle and assigned-user
// handlers. A Hub process uses the service it installed. Otherwise the
// handler builds one for this request.
func (h *hub) controlPlaneService() *operator.Service {
	if h.operatorService != nil {
		return h.operatorService
	}
	if h.tailnet != nil {
		return operator.NewWithTailnet(h.store, h.tailnet)
	}
	return operator.NewWithTailnet(h.store, nil)
}

func (h *hub) handleGetOperatorMachineAssignedUser(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	result, err := h.controlPlaneService().MachineAssignedUser(r.PathValue("id"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, store.OperatorCodeMachineNotFound, "找不到這台機器")
			return
		}
		log.Printf("failed to read operator machine assigned user machine=%s: %v", r.PathValue("id"), err)
		writeErr(w, http.StatusInternalServerError, "INTERNAL", "讀取機器指派使用者失敗")
		return
	}
	w.Header().Set("ETag", fmt.Sprintf(`"assigned-user-revision-%d"`, result.Revision))
	writeJSON(w, http.StatusOK, result)
}

func (h *hub) handlePutOperatorMachineAssignedUser(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	actor := operatorActor(r)
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		h.rejectOperatorMachineAssignedUserTransport(w, r, actor, http.StatusUnsupportedMediaType,
			"UNSUPPORTED_MEDIA_TYPE", "Content-Type 必須是 application/json")
		return
	}
	var body machineAssignedUserOperatorRequest
	if rejection := decodeOperatorBody(w, r, &body); rejection != nil {
		h.rejectOperatorMachineAssignedUserTransport(w, r, actor,
			rejection.Status, rejection.Code, rejection.Detail)
		return
	}
	result, err := h.controlPlaneService().ChangeMachineAssignedUser(r.Context(), operator.MachineAssignedUserRequest{
		MachineID:          r.PathValue("id"),
		UserID:             body.UserID,
		ExpectedRevision:   body.ExpectedRevision,
		ConfirmDisplayName: body.ConfirmDisplayName,
		IdempotencyKey:     r.Header.Get("Idempotency-Key"),
		Actor:              actor,
	})
	if err != nil {
		status, code, detail := operator.HTTPError(err)
		if code == store.OperatorCodeTailnetSourceUnavailable {
			detail = "使用者名冊：來源不可用"
		}
		if code == store.OperatorCodeAssignedUserNotInRoster {
			detail = "使用者不在名冊；請重新選擇使用者"
		}
		if status == http.StatusInternalServerError {
			log.Printf("operator machine assigned user failed machine=%s: %v", r.PathValue("id"), err)
		}
		var rejection *store.OperatorRequestError
		if errors.As(err, &rejection) && rejection.Replayed {
			w.Header().Set("Idempotency-Replayed", "true")
		}
		writeErr(w, status, code, detail)
		return
	}
	if result.Replayed {
		w.Header().Set("Idempotency-Replayed", "true")
	}
	w.Header().Set("ETag", fmt.Sprintf(`"assigned-user-revision-%d"`, result.Revision))
	writeJSON(w, http.StatusOK, result)
}

func (h *hub) rejectOperatorMachineAssignedUserTransport(w http.ResponseWriter, r *http.Request,
	actor operator.Actor, status int, code, detail string,
) {
	machineID := r.PathValue("id")
	subject := machineID
	if machine, err := h.store.GetMachine(machineID); err == nil {
		subject = machine.DisplayName
	}
	h.recordOperatorTransportRejection(r, actor, store.AuditEntry{
		Action: store.AuditMachineAssignedUser, MachineID: machineID, Subject: subject,
	}, code, detail)
	writeErr(w, status, code, detail)
}
