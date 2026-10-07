package main

import (
	"errors"
	"log"
	"mime"
	"net/http"

	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/store"
)

type machineRenamePreviewOperatorRequest struct {
	DisplayName string `json:"display_name"`
}

type machineRenameOperatorRequest struct {
	DisplayName        string `json:"display_name"`
	ConfirmDisplayName string `json:"confirm_display_name"`
	PreviewDigest      string `json:"preview_digest"`
	Reason             string `json:"reason"`
}

func (h *hub) handlePreviewOperatorMachineRename(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.ForceQuery || r.URL.RawQuery != "" {
		writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "machine rename preview 不接受 query parameters")
		return
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeErr(w, http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE", "Content-Type 必須是 application/json")
		return
	}
	var body machineRenamePreviewOperatorRequest
	if rejection := decodeOperatorBody(w, r, &body); rejection != nil {
		writeErr(w, rejection.Status, rejection.Code, rejection.Detail)
		return
	}
	result, err := operator.New(h.store).PreviewMachineRename(operator.MachineRenamePreviewRequest{
		MachineID: r.PathValue("id"), DisplayName: body.DisplayName,
	})
	if err != nil {
		writeOperatorMachineRenameError(w, err, "preview", r.PathValue("id"))
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *hub) handlePutOperatorMachineRename(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	actor := operatorActor(r)
	if r.URL.ForceQuery || r.URL.RawQuery != "" {
		h.rejectOperatorMachineRenameTransport(w, r, actor, http.StatusBadRequest, "BAD_REQUEST",
			"machine rename apply 不接受 query parameters")
		return
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		h.rejectOperatorMachineRenameTransport(w, r, actor, http.StatusUnsupportedMediaType,
			"UNSUPPORTED_MEDIA_TYPE", "Content-Type 必須是 application/json")
		return
	}
	var body machineRenameOperatorRequest
	if rejection := decodeOperatorBody(w, r, &body); rejection != nil {
		h.rejectOperatorMachineRenameTransport(w, r, actor, rejection.Status, rejection.Code, rejection.Detail)
		return
	}
	result, err := operator.New(h.store).RenameMachine(operator.MachineRenameRequest{
		MachineID: r.PathValue("id"), DisplayName: body.DisplayName,
		ConfirmDisplayName: body.ConfirmDisplayName, PreviewDigest: body.PreviewDigest,
		Reason: body.Reason, IdempotencyKey: r.Header.Get("Idempotency-Key"), Actor: actor,
	})
	if err != nil {
		writeOperatorMachineRenameError(w, err, "apply", r.PathValue("id"))
		return
	}
	if result.Replayed {
		w.Header().Set("Idempotency-Replayed", "true")
	}
	writeJSON(w, http.StatusOK, result)
}

func writeOperatorMachineRenameError(w http.ResponseWriter, err error, operation, machineID string) {
	status, code, detail := operator.HTTPError(err)
	if errors.Is(err, store.ErrNotFound) {
		status, code, detail = http.StatusNotFound, store.OperatorCodeMachineNotFound, "找不到這台機器"
	}
	var rejection *store.OperatorRequestError
	if errors.As(err, &rejection) && rejection.Replayed {
		w.Header().Set("Idempotency-Replayed", "true")
	}
	if status == http.StatusInternalServerError {
		log.Printf("operator machine rename %s failed machine=%q: %v", operation, machineID, err)
	}
	writeErr(w, status, code, detail)
}

func (h *hub) rejectOperatorMachineRenameTransport(w http.ResponseWriter, r *http.Request,
	actor operator.Actor, status int, code, detail string,
) {
	machineID := r.PathValue("id")
	subject := machineID
	if machine, err := h.store.GetMachine(machineID); err == nil {
		subject = machine.DisplayName
	}
	h.recordOperatorTransportRejection(r, actor, store.AuditEntry{
		Action: store.AuditMachineRename, MachineID: machineID, Subject: subject,
	}, code, detail)
	writeErr(w, status, code, detail)
}
