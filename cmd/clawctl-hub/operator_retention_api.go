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

type retentionPrunePreviewBody struct {
	EvaluatedAt *time.Time                     `json:"evaluated_at,omitempty"`
	Policy      *store.OperatorRetentionPolicy `json:"policy,omitempty"`
}

type retentionPruneApplyBody struct {
	EvaluatedAt      time.Time                      `json:"evaluated_at"`
	Policy           *store.OperatorRetentionPolicy `json:"policy"`
	ExpectedRevision *int64                         `json:"expected_revision"`
	Confirm          string                         `json:"confirm"`
	PreviewDigest    string                         `json:"preview_digest"`
	Reason           string                         `json:"reason"`
}

func (h *hub) configuredRetentionPolicy() store.RetentionPolicy {
	if err := h.retention.Validate(); err == nil {
		return h.retention
	}
	return store.DefaultRetention()
}

func (h *hub) retentionOperator() *operator.Service {
	if h.operatorService != nil {
		return h.operatorService
	}
	return operator.New(h.store)
}

func (h *hub) handleGetOperatorRetention(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.ForceQuery || r.URL.RawQuery != "" {
		writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "Retention status 不接受 query parameters")
		return
	}
	result, err := h.retentionOperator().RetentionStatus(h.configuredRetentionPolicy(), time.Now().UTC())
	if err != nil {
		log.Printf("operator retention status failed: %v", err)
		writeErr(w, http.StatusInternalServerError, "INTERNAL", "讀取 retention 狀態失敗")
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *hub) handlePreviewOperatorRetentionPrune(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.ForceQuery || r.URL.RawQuery != "" {
		writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "Retention prune 預覽不接受 query parameters")
		return
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeErr(w, http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE", "Content-Type 必須是 application/json")
		return
	}
	var body retentionPrunePreviewBody
	if rejection := decodeOperatorBody(w, r, &body); rejection != nil {
		writeErr(w, rejection.Status, rejection.Code, rejection.Detail)
		return
	}
	evaluatedAt := time.Now().UTC().Truncate(time.Second)
	if body.EvaluatedAt != nil {
		evaluatedAt = body.EvaluatedAt.UTC()
	}
	policy := h.configuredRetentionPolicy()
	if body.Policy != nil {
		policy, err = body.Policy.RetentionPolicy()
		if err != nil {
			status, code, detail := operator.HTTPError(err)
			writeErr(w, status, code, detail)
			return
		}
	}
	result, err := h.retentionOperator().PreviewRetentionPrune(operator.RetentionPrunePreviewRequest{
		EvaluatedAt: evaluatedAt, Policy: policy,
	})
	if err != nil {
		status, code, detail := operator.HTTPError(err)
		if status == http.StatusInternalServerError {
			log.Printf("operator retention preview failed: %v", err)
		}
		writeErr(w, status, code, detail)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *hub) handleCreateOperatorRetentionPrune(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	actor := operatorActor(r)
	if r.URL.ForceQuery || r.URL.RawQuery != "" {
		h.rejectOperatorRetentionTransport(w, r, actor, http.StatusBadRequest, "BAD_REQUEST",
			"Retention prune 套用不接受 query parameters")
		return
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		h.rejectOperatorRetentionTransport(w, r, actor, http.StatusUnsupportedMediaType,
			"UNSUPPORTED_MEDIA_TYPE", "Content-Type 必須是 application/json")
		return
	}
	var body retentionPruneApplyBody
	if rejection := decodeOperatorBody(w, r, &body); rejection != nil {
		h.rejectOperatorRetentionTransport(w, r, actor, rejection.Status, rejection.Code, rejection.Detail)
		return
	}
	if body.Policy == nil || body.ExpectedRevision == nil {
		h.rejectOperatorRetentionTransport(w, r, actor, http.StatusPreconditionRequired,
			store.OperatorCodePreconditionRequired, "policy 與 expected_revision 不可省略")
		return
	}
	result, err := h.retentionOperator().ApplyRetentionPrune(operator.RetentionPruneApplyRequest{
		EvaluatedAt: body.EvaluatedAt, Policy: *body.Policy,
		ExpectedRevision: *body.ExpectedRevision, Confirm: body.Confirm,
		PreviewDigest: body.PreviewDigest, Reason: body.Reason,
		IdempotencyKey: r.Header.Get("Idempotency-Key"), Actor: actor,
	})
	if err != nil {
		status, code, detail := operator.HTTPError(err)
		var rejection *store.OperatorRequestError
		if errors.As(err, &rejection) && rejection.Replayed {
			w.Header().Set("Idempotency-Replayed", "true")
		}
		if status == http.StatusInternalServerError {
			log.Printf("operator retention apply failed: %v", err)
		}
		writeErr(w, status, code, detail)
		return
	}
	if result.Replayed {
		w.Header().Set("Idempotency-Replayed", "true")
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *hub) rejectOperatorRetentionTransport(w http.ResponseWriter, r *http.Request, actor operator.Actor,
	status int, code, detail string,
) {
	h.recordOperatorTransportRejection(r, actor, store.AuditEntry{
		Action: store.AuditRetentionPrune, Subject: "retention",
	}, code, detail)
	writeErr(w, status, code, detail)
}
