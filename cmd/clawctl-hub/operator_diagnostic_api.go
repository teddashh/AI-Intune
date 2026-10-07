package main

import (
	"errors"
	"log"
	"mime"
	"net/http"
	"strconv"

	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/store"
)

type diagnosticNoopPreviewOperatorRequest struct {
	ExecutionTimeoutSeconds int `json:"execution_timeout_seconds"`
}

type diagnosticNoopOperatorRequest struct {
	ExecutionTimeoutSeconds int    `json:"execution_timeout_seconds"`
	ConfirmDisplayName      string `json:"confirm_display_name"`
	PreviewDigest           string `json:"preview_digest"`
	Reason                  string `json:"reason"`
}

func (h *hub) handlePreviewOperatorDiagnosticNoop(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.ForceQuery || r.URL.RawQuery != "" {
		writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "diagnostic noop preview 不接受 query parameters")
		return
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeErr(w, http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE", "Content-Type 必須是 application/json")
		return
	}
	var body diagnosticNoopPreviewOperatorRequest
	if rejection := decodeOperatorBody(w, r, &body); rejection != nil {
		writeErr(w, rejection.Status, rejection.Code, rejection.Detail)
		return
	}
	result, err := operator.New(h.store).PreviewDiagnosticNoop(operator.DiagnosticNoopPreviewRequest{
		MachineID: r.PathValue("id"), ExecutionTimeoutSeconds: body.ExecutionTimeoutSeconds,
	})
	if err != nil {
		writeOperatorDiagnosticNoopError(w, err, "preview", r.PathValue("id"))
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *hub) handleCreateOperatorDiagnosticNoop(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	actor := operatorActor(r)
	if r.URL.ForceQuery || r.URL.RawQuery != "" {
		h.rejectOperatorDiagnosticNoopTransport(w, r, actor, http.StatusBadRequest, "BAD_REQUEST",
			"diagnostic noop apply 不接受 query parameters")
		return
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		h.rejectOperatorDiagnosticNoopTransport(w, r, actor, http.StatusUnsupportedMediaType,
			"UNSUPPORTED_MEDIA_TYPE", "Content-Type 必須是 application/json")
		return
	}
	var body diagnosticNoopOperatorRequest
	if rejection := decodeOperatorBody(w, r, &body); rejection != nil {
		h.rejectOperatorDiagnosticNoopTransport(w, r, actor, rejection.Status, rejection.Code, rejection.Detail)
		return
	}
	result, err := operator.New(h.store).CreateDiagnosticNoop(operator.DiagnosticNoopRequest{
		MachineID: r.PathValue("id"), ExecutionTimeoutSeconds: body.ExecutionTimeoutSeconds,
		ConfirmDisplayName: body.ConfirmDisplayName, PreviewDigest: body.PreviewDigest,
		Reason: body.Reason, IdempotencyKey: r.Header.Get("Idempotency-Key"), Actor: actor,
	})
	if err != nil {
		writeOperatorDiagnosticNoopError(w, err, "apply", r.PathValue("id"))
		return
	}
	status := http.StatusCreated
	if result.Replayed {
		status = http.StatusOK
		w.Header().Set("Idempotency-Replayed", "true")
	}
	w.Header().Set("Location", "/v1/operator/jobs/"+result.JobID)
	w.Header().Set("ETag", `"diagnostic-noop-revision-`+strconv.FormatInt(int64(result.Revision), 10)+`"`)
	writeJSON(w, status, result)
}

func writeOperatorDiagnosticNoopError(w http.ResponseWriter, err error, operation, machineID string) {
	status, code, detail := operator.HTTPError(err)
	if errors.Is(err, store.ErrNotFound) {
		status, code, detail = http.StatusNotFound, store.OperatorCodeMachineNotFound, "找不到這台機器"
	}
	var rejection *store.OperatorRequestError
	if errors.As(err, &rejection) && rejection.Replayed {
		w.Header().Set("Idempotency-Replayed", "true")
	}
	if status == http.StatusInternalServerError {
		log.Printf("operator diagnostic noop %s failed machine=%q: %v", operation, machineID, err)
	}
	writeErr(w, status, code, detail)
}

func (h *hub) rejectOperatorDiagnosticNoopTransport(w http.ResponseWriter, r *http.Request,
	actor operator.Actor, status int, code, detail string,
) {
	machineID := r.PathValue("id")
	subject := machineID
	if machine, err := h.store.GetMachine(machineID); err == nil {
		subject = machine.DisplayName
	}
	h.recordOperatorTransportRejection(r, actor, store.AuditEntry{
		Action: store.AuditDiagnosticNoop, MachineID: machineID, Subject: subject,
	}, code, detail)
	writeErr(w, status, code, detail)
}
