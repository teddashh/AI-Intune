package main

import (
	"errors"
	"log"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/store"
	"github.com/teddashh/AI-Intune/internal/tailnet"
)

type tailnetPeerIgnorePreviewBody struct {
	PeerID    string    `json:"peer_id"`
	Action    string    `json:"action"`
	ExpiresAt time.Time `json:"expires_at,omitempty"`
	Reason    string    `json:"reason"`
}

type tailnetPeerIgnoreApplyBody struct {
	Action           string    `json:"action"`
	ExpiresAt        time.Time `json:"expires_at,omitempty"`
	ExpectedRevision *int64    `json:"expected_revision"`
	ConfirmHostname  string    `json:"confirm_hostname"`
	PreviewDigest    string    `json:"preview_digest"`
	Reason           string    `json:"reason"`
}

func (h *hub) tailnetOperator() *operator.Service {
	if h.tailnet == nil {
		h.tailnet = tailnet.NewCache()
	}
	return operator.NewWithTailnet(h.store, h.tailnet)
}

func (h *hub) handleGetOperatorTailnet(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.ForceQuery || r.URL.RawQuery != "" {
		writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "Tailnet 清單不接受 query parameters")
		return
	}
	result, err := h.tailnetOperator().Tailnet(r.Context(), time.Now().UTC())
	if err != nil {
		log.Printf("operator tailnet read failed: %v", err)
		writeErr(w, http.StatusInternalServerError, "INTERNAL", "讀取 Tailnet 狀態失敗")
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *hub) handlePreviewOperatorTailnetPeerIgnore(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.ForceQuery || r.URL.RawQuery != "" {
		writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "Tailnet 規則預覽不接受 query parameters")
		return
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeErr(w, http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE", "Content-Type 必須是 application/json")
		return
	}
	var body tailnetPeerIgnorePreviewBody
	if rejection := decodeOperatorBody(w, r, &body); rejection != nil {
		writeErr(w, rejection.Status, rejection.Code, rejection.Detail)
		return
	}
	result, err := h.tailnetOperator().PreviewTailnetPeerIgnore(r.Context(), operator.TailnetPeerIgnorePreviewRequest{
		PeerID: body.PeerID, Action: body.Action, ExpiresAt: body.ExpiresAt, Reason: body.Reason,
	}, time.Now().UTC())
	if err != nil {
		status, code, detail := operator.HTTPError(err)
		if status == http.StatusInternalServerError {
			log.Printf("operator tailnet preview failed: %v", err)
		}
		writeErr(w, status, code, detail)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *hub) handlePutOperatorTailnetPeerIgnore(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	actor := operatorActor(r)
	if r.URL.ForceQuery || r.URL.RawQuery != "" {
		h.rejectOperatorTailnetTransport(w, r, actor, http.StatusBadRequest, "BAD_REQUEST",
			"Tailnet 規則套用不接受 query parameters")
		return
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		h.rejectOperatorTailnetTransport(w, r, actor, http.StatusUnsupportedMediaType,
			"UNSUPPORTED_MEDIA_TYPE", "Content-Type 必須是 application/json")
		return
	}
	var body tailnetPeerIgnoreApplyBody
	if rejection := decodeOperatorBody(w, r, &body); rejection != nil {
		h.rejectOperatorTailnetTransport(w, r, actor, rejection.Status, rejection.Code, rejection.Detail)
		return
	}
	if body.ExpectedRevision == nil {
		h.rejectOperatorTailnetTransport(w, r, actor, http.StatusPreconditionRequired,
			store.OperatorCodePreconditionRequired, "expected_revision 不可省略")
		return
	}
	result, err := h.tailnetOperator().ApplyTailnetPeerIgnore(r.Context(), operator.TailnetPeerIgnoreApplyRequest{
		TailnetPeerIgnorePreviewRequest: operator.TailnetPeerIgnorePreviewRequest{
			PeerID: strings.TrimSpace(r.PathValue("id")), Action: body.Action,
			ExpiresAt: body.ExpiresAt, Reason: body.Reason,
		},
		ExpectedRevision: *body.ExpectedRevision, ConfirmHostname: body.ConfirmHostname,
		PreviewDigest: body.PreviewDigest, IdempotencyKey: r.Header.Get("Idempotency-Key"),
		Actor: actor,
	}, time.Now().UTC())
	if err != nil {
		status, code, detail := operator.HTTPError(err)
		var rejection *store.OperatorRequestError
		if errors.As(err, &rejection) && rejection.Replayed {
			w.Header().Set("Idempotency-Replayed", "true")
		}
		if status == http.StatusInternalServerError {
			log.Printf("operator tailnet apply failed peer=%s: %v", r.PathValue("id"), err)
		}
		writeErr(w, status, code, detail)
		return
	}
	if result.Replayed {
		w.Header().Set("Idempotency-Replayed", "true")
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *hub) rejectOperatorTailnetTransport(w http.ResponseWriter, r *http.Request, actor operator.Actor,
	status int, code, detail string,
) {
	peerID := strings.TrimSpace(r.PathValue("id"))
	h.recordOperatorTransportRejection(r, actor, store.AuditEntry{
		Action: store.AuditTailnetPeerIgnore, Subject: peerID,
	}, code, detail)
	writeErr(w, status, code, detail)
}
