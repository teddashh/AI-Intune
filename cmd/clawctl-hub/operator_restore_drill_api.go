package main

import (
	"errors"
	"log"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/restoredrill"
	"github.com/teddashh/AI-Intune/internal/store"
)

type restoreDrillPreviewBody struct{}

type restoreDrillApplyBody struct {
	PreviewDigest *string `json:"preview_digest"`
	Confirm       *string `json:"confirm"`
	Reason        *string `json:"reason"`
}

func (h *hub) handlePreviewOperatorRestoreDrill(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.ForceQuery || r.URL.RawQuery != "" {
		writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "還原演練預覽不接受 query parameters")
		return
	}
	if !operatorJSONContentType(r) {
		writeErr(w, http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE", "Content-Type 必須是 application/json")
		return
	}
	var body restoreDrillPreviewBody
	if rejection := decodeOperatorBody(w, r, &body); rejection != nil {
		writeErr(w, rejection.Status, rejection.Code, rejection.Detail)
		return
	}
	result, err := h.retentionOperator().PreviewRestoreDrill(r.Context(), time.Now().UTC())
	if err != nil {
		writeRestoreDrillDependencyError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *hub) handleCreateOperatorRestoreDrill(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	actor := operatorActor(r)
	if r.URL.ForceQuery || r.URL.RawQuery != "" {
		h.rejectOperatorRestoreDrillTransport(w, r, actor, http.StatusBadRequest, "BAD_REQUEST", "還原演練建立不接受 query parameters")
		return
	}
	if !operatorJSONContentType(r) {
		h.rejectOperatorRestoreDrillTransport(w, r, actor, http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE", "Content-Type 必須是 application/json")
		return
	}
	var body restoreDrillApplyBody
	if rejection := decodeOperatorBody(w, r, &body); rejection != nil {
		h.rejectOperatorRestoreDrillTransport(w, r, actor, rejection.Status, rejection.Code, rejection.Detail)
		return
	}
	if body.PreviewDigest == nil || body.Confirm == nil || body.Reason == nil {
		h.rejectOperatorRestoreDrillTransport(w, r, actor, http.StatusBadRequest, "BAD_REQUEST", "還原演練建立必須明列所有欄位")
		return
	}
	result, err := h.retentionOperator().ApplyRestoreDrill(r.Context(), operator.RestoreDrillApplyRequest{
		PreviewDigest: *body.PreviewDigest, Confirm: *body.Confirm, Reason: *body.Reason,
		IdempotencyKey: r.Header.Get("Idempotency-Key"), Actor: actor,
	})
	if err != nil {
		var rejection *store.OperatorRequestError
		if errors.As(err, &rejection) && rejection.Replayed {
			w.Header().Set("Idempotency-Replayed", "true")
		}
		if errors.Is(err, restoredrill.ErrNoBackup) || errors.Is(err, restoredrill.ErrUnsafeBackup) ||
			errors.Is(err, restoredrill.ErrVerification) {
			writeRestoreDrillDependencyError(w, err)
			return
		}
		status, code, detail := operator.HTTPError(err)
		if status == http.StatusInternalServerError {
			log.Printf("operator restore drill create failed: %v", err)
		}
		writeErr(w, status, code, detail)
		return
	}
	status := http.StatusAccepted
	if result.Replayed {
		status = http.StatusOK
		w.Header().Set("Idempotency-Replayed", "true")
	}
	writeJSON(w, status, result)
}

func (h *hub) handleListOperatorRestoreDrills(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	limit, err := restoreDrillListLimit(r.URL)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "還原演練 operation limit 不合法")
		return
	}
	result, err := h.retentionOperator().RestoreDrillOperations(limit)
	if err != nil {
		log.Printf("operator restore drill list failed: %v", err)
		writeErr(w, http.StatusInternalServerError, "INTERNAL", "讀取還原演練 operations 失敗")
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *hub) handleGetOperatorRestoreDrill(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.ForceQuery || r.URL.RawQuery != "" {
		writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "還原演練 operation detail 不接受 query parameters")
		return
	}
	result, err := h.retentionOperator().RestoreDrillOperation(r.PathValue("id"))
	if errors.Is(err, store.ErrRestoreDrillNotFound) {
		writeErr(w, http.StatusNotFound, "RESTORE_DRILL_NOT_FOUND", "找不到指定的還原演練 operation")
		return
	}
	if err != nil {
		log.Printf("operator restore drill detail failed: %v", err)
		writeErr(w, http.StatusInternalServerError, "INTERNAL", "讀取還原演練 operation 失敗")
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func operatorJSONContentType(r *http.Request) bool {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	return err == nil && mediaType == "application/json"
}

func restoreDrillListLimit(u *url.URL) (int, error) {
	if u == nil || (u.ForceQuery && u.RawQuery == "") {
		return 0, errors.New("invalid query")
	}
	values, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return 0, err
	}
	for key := range values {
		if key != "limit" {
			return 0, errors.New("unknown query")
		}
	}
	if _, ok := values["limit"]; !ok {
		return 0, nil
	}
	if len(values["limit"]) != 1 || values["limit"][0] == "" {
		return 0, errors.New("invalid limit")
	}
	limit, err := strconv.Atoi(values["limit"][0])
	if err != nil || limit < 1 || limit > store.MaxRestoreDrillReadLimit {
		return 0, errors.New("invalid limit")
	}
	return limit, nil
}

func writeRestoreDrillDependencyError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, restoredrill.ErrNoBackup):
		writeErr(w, http.StatusConflict, operator.RestoreDrillFailureNoBackup, "目前沒有可供驗證的備份")
	case errors.Is(err, restoredrill.ErrUnsafeBackup):
		writeErr(w, http.StatusServiceUnavailable, operator.RestoreDrillFailureUnsafeBackup, "最新備份目前不可安全開啟")
	default:
		log.Printf("operator restore drill dependency failed: %v", err)
		writeErr(w, http.StatusServiceUnavailable, "RESTORE_DRILL_UNAVAILABLE", "還原演練目前無法使用")
	}
}

func (h *hub) rejectOperatorRestoreDrillTransport(w http.ResponseWriter, r *http.Request, actor operator.Actor,
	status int, code, detail string,
) {
	h.recordOperatorTransportRejection(r, actor, store.AuditEntry{
		Action: store.AuditRestoreDrill, Subject: "restore drill",
	}, code, detail)
	writeErr(w, status, code, detail)
}
