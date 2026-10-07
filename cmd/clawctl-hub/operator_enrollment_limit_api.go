package main

import (
	"errors"
	"log"
	"mime"
	"net/http"
	"time"

	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/store"
)

type enrollmentLimitPreviewBody struct {
	Set         bool `json:"set"`
	MaxMachines int  `json:"max_machines"`
}

type enrollmentLimitApplyBody struct {
	Set              bool   `json:"set"`
	MaxMachines      int    `json:"max_machines"`
	ExpectedRevision *int64 `json:"expected_revision"`
	PreviewDigest    string `json:"preview_digest"`
	Reason           string `json:"reason"`
}

// handleGetOperatorEnrollmentLimit says how many machines this Hub will still
// take, and what is stopping it if the answer is none.
func (h *hub) handleGetOperatorEnrollmentLimit(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.ForceQuery || r.URL.RawQuery != "" {
		writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "enrollment-limit 不接受 query parameters")
		return
	}
	result, err := h.operatorReportService().EnrollmentLimit(time.Now().UTC())
	if err != nil {
		log.Printf("failed to read operator enrollment limit: %v", err)
		writeErr(w, http.StatusInternalServerError, "INTERNAL", "讀取註冊上限失敗")
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *hub) handlePreviewOperatorEnrollmentLimit(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.ForceQuery || r.URL.RawQuery != "" {
		writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "註冊上限預覽不接受 query parameters")
		return
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeErr(w, http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE", "Content-Type 必須是 application/json")
		return
	}
	var body enrollmentLimitPreviewBody
	if rejection := decodeOperatorBody(w, r, &body); rejection != nil {
		writeErr(w, rejection.Status, rejection.Code, rejection.Detail)
		return
	}
	result, err := h.operatorReportService().PreviewEnrollmentLimit(operator.EnrollmentLimitPreviewRequest{
		Set: body.Set, MaxMachines: body.MaxMachines,
	})
	if err != nil {
		status, code, detail := operator.HTTPError(err)
		if status == http.StatusInternalServerError {
			log.Printf("operator enrollment limit preview failed: %v", err)
		}
		writeErr(w, status, code, detail)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *hub) handleSetOperatorEnrollmentLimit(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	actor := operatorActor(r)
	if r.URL.ForceQuery || r.URL.RawQuery != "" {
		h.rejectOperatorEnrollmentLimitTransport(w, r, actor, http.StatusBadRequest, "BAD_REQUEST",
			"註冊上限不接受 query parameters")
		return
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		h.rejectOperatorEnrollmentLimitTransport(w, r, actor, http.StatusUnsupportedMediaType,
			"UNSUPPORTED_MEDIA_TYPE", "Content-Type 必須是 application/json")
		return
	}
	var body enrollmentLimitApplyBody
	if rejection := decodeOperatorBody(w, r, &body); rejection != nil {
		h.rejectOperatorEnrollmentLimitTransport(w, r, actor, rejection.Status, rejection.Code, rejection.Detail)
		return
	}
	if body.ExpectedRevision == nil {
		h.rejectOperatorEnrollmentLimitTransport(w, r, actor, http.StatusPreconditionRequired,
			store.OperatorCodePreconditionRequired, "expected_revision 不可省略")
		return
	}
	result, err := h.operatorReportService().SetEnrollmentLimit(operator.EnrollmentLimitApplyRequest{
		EnrollmentLimitPreviewRequest: operator.EnrollmentLimitPreviewRequest{
			Set: body.Set, MaxMachines: body.MaxMachines,
		},
		Reason: body.Reason, ExpectedRevision: *body.ExpectedRevision, PreviewDigest: body.PreviewDigest,
		IdempotencyKey: r.Header.Get("Idempotency-Key"), Actor: actor,
	})
	if err != nil {
		status, code, detail := operator.HTTPError(err)
		var rejection *store.OperatorRequestError
		if errors.As(err, &rejection) && rejection.Replayed {
			w.Header().Set("Idempotency-Replayed", "true")
		}
		if status == http.StatusInternalServerError {
			log.Printf("operator enrollment limit apply failed: %v", err)
		}
		writeErr(w, status, code, detail)
		return
	}
	if result.Replayed {
		w.Header().Set("Idempotency-Replayed", "true")
	}
	writeJSON(w, http.StatusOK, result)
}

// rejectOperatorEnrollmentLimitTransport records the attempts that never became
// a canonical request, so "誰在改上限" has no gap at the transport edge.
func (h *hub) rejectOperatorEnrollmentLimitTransport(w http.ResponseWriter, r *http.Request,
	actor operator.Actor, status int, code, detail string,
) {
	h.recordOperatorTransportRejection(r, actor, store.AuditEntry{
		Action: store.AuditEnrollmentLimit, Subject: "註冊上限",
	}, code, detail)
	writeErr(w, status, code, detail)
}
