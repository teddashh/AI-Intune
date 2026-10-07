package main

import (
	"errors"
	"log"
	"mime"
	"net/http"

	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/store"
)

type machineNotesPreviewOperatorRequest struct {
	Notes string `json:"notes"`
}

type machineNotesOperatorRequest struct {
	Notes              string `json:"notes"`
	ConfirmDisplayName string `json:"confirm_display_name"`
	PreviewDigest      string `json:"preview_digest"`
	Reason             string `json:"reason"`
}

func (h *hub) handlePreviewOperatorMachineNotes(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.ForceQuery || r.URL.RawQuery != "" {
		writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "machine notes preview 不接受 query parameters")
		return
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeErr(w, http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE", "Content-Type 必須是 application/json")
		return
	}
	var body machineNotesPreviewOperatorRequest
	if rejection := decodeOperatorBody(w, r, &body); rejection != nil {
		writeErr(w, rejection.Status, rejection.Code, rejection.Detail)
		return
	}
	result, err := operator.New(h.store).PreviewMachineNotes(operator.MachineNotesPreviewRequest{
		MachineID: r.PathValue("id"), Notes: body.Notes,
	})
	if err != nil {
		writeOperatorMachineNotesError(w, err, "preview", r.PathValue("id"))
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *hub) handlePutOperatorMachineNotes(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	actor := operatorActor(r)
	if r.URL.ForceQuery || r.URL.RawQuery != "" {
		h.rejectOperatorMachineNotesTransport(w, r, actor, http.StatusBadRequest, "BAD_REQUEST",
			"machine notes apply 不接受 query parameters")
		return
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		h.rejectOperatorMachineNotesTransport(w, r, actor, http.StatusUnsupportedMediaType,
			"UNSUPPORTED_MEDIA_TYPE", "Content-Type 必須是 application/json")
		return
	}
	var body machineNotesOperatorRequest
	if rejection := decodeOperatorBody(w, r, &body); rejection != nil {
		h.rejectOperatorMachineNotesTransport(w, r, actor, rejection.Status, rejection.Code, rejection.Detail)
		return
	}
	result, err := operator.New(h.store).UpdateMachineNotes(operator.MachineNotesRequest{
		MachineID: r.PathValue("id"), Notes: body.Notes,
		ConfirmDisplayName: body.ConfirmDisplayName, PreviewDigest: body.PreviewDigest,
		Reason: body.Reason, IdempotencyKey: r.Header.Get("Idempotency-Key"), Actor: actor,
	})
	if err != nil {
		writeOperatorMachineNotesError(w, err, "apply", r.PathValue("id"))
		return
	}
	if result.Replayed {
		w.Header().Set("Idempotency-Replayed", "true")
	}
	writeJSON(w, http.StatusOK, result)
}

func writeOperatorMachineNotesError(w http.ResponseWriter, err error, operation, machineID string) {
	status, code, detail := operator.HTTPError(err)
	if errors.Is(err, store.ErrNotFound) {
		status, code, detail = http.StatusNotFound, store.OperatorCodeMachineNotFound, "找不到這台機器"
	}
	var rejection *store.OperatorRequestError
	if errors.As(err, &rejection) && rejection.Replayed {
		w.Header().Set("Idempotency-Replayed", "true")
	}
	if status == http.StatusInternalServerError {
		log.Printf("operator machine notes %s failed machine=%q: %v", operation, machineID, err)
	}
	writeErr(w, status, code, detail)
}

func (h *hub) rejectOperatorMachineNotesTransport(w http.ResponseWriter, r *http.Request,
	actor operator.Actor, status int, code, detail string,
) {
	machineID := r.PathValue("id")
	subject := machineID
	if machine, err := h.store.GetMachine(machineID); err == nil {
		subject = machine.DisplayName
	}
	h.recordOperatorTransportRejection(r, actor, store.AuditEntry{
		Action: store.AuditMachineNotes, MachineID: machineID, Subject: subject,
	}, code, detail)
	writeErr(w, status, code, detail)
}
