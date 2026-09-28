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

// Pointer revisions preserve the wire distinction between a missing
// precondition and an explicit revision zero. DesiredState intentionally keeps
// its zero value: the operator service gives missing/empty and unknown values
// the same stable BAD_LIFECYCLE domain classification.
type machineLifecyclePreviewOperatorRequest struct {
	DesiredState     operator.MachineLifecycleState `json:"desired_state"`
	ExpectedRevision *int64                         `json:"expected_revision"`
}

type machineLifecycleOperatorRequest struct {
	DesiredState       operator.MachineLifecycleState `json:"desired_state"`
	ExpectedRevision   *int64                         `json:"expected_revision"`
	ConfirmDisplayName string                         `json:"confirm_display_name"`
	PreviewDigest      string                         `json:"preview_digest"`
	Reason             string                         `json:"reason"`
}

func (h *hub) handleGetOperatorMachineLifecycle(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.ForceQuery || r.URL.RawQuery != "" {
		writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "machine lifecycle 不接受 query parameters")
		return
	}
	result, err := operator.New(h.store).MachineLifecycle(r.PathValue("id"))
	if err != nil {
		writeOperatorMachineLifecycleError(w, err, "read", r.PathValue("id"))
		return
	}
	w.Header().Set("ETag", fmt.Sprintf(`"lifecycle-revision-%d"`, result.LifecycleRevision))
	writeJSON(w, http.StatusOK, result)
}

func (h *hub) handlePreviewOperatorMachineLifecycle(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.ForceQuery || r.URL.RawQuery != "" {
		writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "machine lifecycle preview 不接受 query parameters")
		return
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeErr(w, http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE",
			"Content-Type 必須是 application/json")
		return
	}
	var body machineLifecyclePreviewOperatorRequest
	if rejection := decodeOperatorBody(w, r, &body); rejection != nil {
		writeErr(w, rejection.Status, rejection.Code, rejection.Detail)
		return
	}
	result, err := operator.New(h.store).PreviewMachineLifecycle(operator.MachineLifecyclePreviewRequest{
		MachineID: r.PathValue("id"), DesiredState: body.DesiredState,
		ExpectedRevision: body.ExpectedRevision,
	})
	if err != nil {
		writeOperatorMachineLifecycleError(w, err, "preview", r.PathValue("id"))
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *hub) handlePutOperatorMachineLifecycle(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	actor := operatorActor(r)
	if r.URL.ForceQuery || r.URL.RawQuery != "" {
		h.rejectOperatorMachineLifecycleTransport(w, r, actor, http.StatusBadRequest, "BAD_REQUEST",
			"machine lifecycle apply 不接受 query parameters")
		return
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		h.rejectOperatorMachineLifecycleTransport(w, r, actor,
			http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE",
			"Content-Type 必須是 application/json")
		return
	}
	var body machineLifecycleOperatorRequest
	if rejection := decodeOperatorBody(w, r, &body); rejection != nil {
		h.rejectOperatorMachineLifecycleTransport(w, r, actor,
			rejection.Status, rejection.Code, rejection.Detail)
		return
	}
	result, err := operator.New(h.store).ChangeMachineLifecycle(operator.MachineLifecycleRequest{
		MachineID: r.PathValue("id"), DesiredState: body.DesiredState,
		ExpectedRevision: body.ExpectedRevision, ConfirmDisplayName: body.ConfirmDisplayName,
		PreviewDigest: body.PreviewDigest, Reason: body.Reason,
		IdempotencyKey: r.Header.Get("Idempotency-Key"), Actor: actor,
	})
	if err != nil {
		writeOperatorMachineLifecycleError(w, err, "apply", r.PathValue("id"))
		return
	}
	if result.Replayed {
		w.Header().Set("Idempotency-Replayed", "true")
	}
	w.Header().Set("ETag", fmt.Sprintf(`"lifecycle-revision-%d"`, result.LifecycleRevision))
	writeJSON(w, http.StatusOK, result)
}

func writeOperatorMachineLifecycleError(w http.ResponseWriter, err error, operation, machineID string) {
	status, code, detail := operator.HTTPError(err)
	if errors.Is(err, store.ErrNotFound) {
		status, code, detail = http.StatusNotFound, store.OperatorCodeMachineNotFound, "找不到這台機器"
	}
	var rejection *store.OperatorRequestError
	if errors.As(err, &rejection) && rejection.Replayed {
		w.Header().Set("Idempotency-Replayed", "true")
	}
	if status == http.StatusInternalServerError {
		log.Printf("operator machine lifecycle %s 失敗 machine=%q: %v", operation, machineID, err)
	}
	writeErr(w, status, code, detail)
}

// Malformed bytes have no trustworthy desired lifecycle direction. Record a
// fixed machine-lifecycle transport classification, never let caller bytes pick
// retire versus restore in audit, and leave the idempotency key reusable after
// the caller corrects the transport.
func (h *hub) rejectOperatorMachineLifecycleTransport(w http.ResponseWriter, r *http.Request,
	actor operator.Actor, status int, code, detail string,
) {
	machineID := r.PathValue("id")
	subject := machineID
	if machine, err := h.store.GetMachine(machineID); err == nil {
		subject = machine.DisplayName
	}
	h.recordOperatorTransportRejection(r, actor, store.AuditEntry{
		Action: store.AuditMachineLifecycle, MachineID: machineID, Subject: subject,
	}, code, detail)
	writeErr(w, status, code, detail)
}
