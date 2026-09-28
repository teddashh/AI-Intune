package main

import (
	"errors"
	"log"
	"mime"
	"net/http"

	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/store"
)

type verificationAssignmentPreviewOperatorRequest struct {
	JobID string `json:"job_id"`
}

type verificationAssignmentOperatorRequest struct {
	JobID               string `json:"job_id"`
	ConfirmVerifierName string `json:"confirm_verifier_name"`
	PreviewDigest       string `json:"preview_digest"`
	Reason              string `json:"reason"`
}

func (h *hub) handlePreviewOperatorVerificationAssignment(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeErr(w, http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE",
			"Content-Type 必須是 application/json")
		return
	}
	var body verificationAssignmentPreviewOperatorRequest
	if rejection := decodeOperatorBody(w, r, &body); rejection != nil {
		writeErr(w, rejection.Status, rejection.Code, rejection.Detail)
		return
	}
	result, err := operator.New(h.store).PreviewVerificationAssignment(
		operator.VerificationAssignmentPreviewRequest{
			VerifierID: r.PathValue("id"), JobID: body.JobID,
		})
	if err != nil {
		status, code, detail := operator.HTTPError(err)
		if status == http.StatusInternalServerError {
			log.Printf("operator verification assignment preview 失敗: %v", err)
		}
		writeErr(w, status, code, detail)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *hub) handleCreateOperatorVerificationAssignment(w http.ResponseWriter, r *http.Request) {
	actor := operatorActor(r)
	w.Header().Set("Cache-Control", "no-store")
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		h.rejectOperatorVerificationAssignmentTransport(w, r, actor,
			http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE",
			"Content-Type 必須是 application/json")
		return
	}
	var body verificationAssignmentOperatorRequest
	if rejection := decodeOperatorBody(w, r, &body); rejection != nil {
		h.rejectOperatorVerificationAssignmentTransport(w, r, actor,
			rejection.Status, rejection.Code, rejection.Detail)
		return
	}
	result, err := operator.New(h.store).AssignVerification(operator.VerificationAssignmentRequest{
		VerifierID: r.PathValue("id"), JobID: body.JobID,
		ConfirmVerifierName: body.ConfirmVerifierName, PreviewDigest: body.PreviewDigest,
		Reason: body.Reason, IdempotencyKey: r.Header.Get("Idempotency-Key"), Actor: actor,
	})
	if err != nil {
		status, code, detail := operator.HTTPError(err)
		if status == http.StatusInternalServerError {
			log.Printf("operator verification assignment 失敗: %v", err)
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
		writeJSON(w, http.StatusOK, result)
		return
	}
	writeJSON(w, http.StatusCreated, result)
}

// rejectOperatorVerificationAssignmentTransport records a transport rejection
// without occupying an idempotency key.
func (h *hub) rejectOperatorVerificationAssignmentTransport(w http.ResponseWriter, r *http.Request,
	actor operator.Actor, status int, code, detail string,
) {
	h.recordOperatorTransportRejection(r, actor, store.AuditEntry{
		Action: store.AuditVerificationAssign, Subject: r.PathValue("id"),
	}, code, detail)
	writeErr(w, status, code, detail)
}
