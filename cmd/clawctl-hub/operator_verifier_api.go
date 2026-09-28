package main

import (
	"errors"
	"log"
	"mime"
	"net/http"
	"strconv"
	"time"

	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/store"
)

type verifierPreviewOperatorRequest struct {
	Kind          string `json:"kind"`
	DisplayName   string `json:"display_name"`
	FailureDomain string `json:"failure_domain"`
}

type verifierCreateOperatorRequest struct {
	Kind          string `json:"kind"`
	DisplayName   string `json:"display_name"`
	FailureDomain string `json:"failure_domain"`
	PreviewDigest string `json:"preview_digest"`
	Reason        string `json:"reason"`
}

type verifierRevocationPreviewOperatorRequest struct{}

type verifierRevocationOperatorRequest struct {
	ExpectedRevision   *int64 `json:"expected_revision"`
	ConfirmDisplayName string `json:"confirm_display_name"`
	PreviewDigest      string `json:"preview_digest"`
	Reason             string `json:"reason"`
}

// verifierOperatorResponse spells out the two wire shapes. RecoveryAction is
// always present so a strict client can tell an intentionally empty fresh
// value from a response produced by older code. Credential is the only
// optional field and must be absent on replay.
type verifierOperatorResponse struct {
	VerifierID       string    `json:"verifier_id"`
	Kind             string    `json:"kind"`
	DisplayName      string    `json:"display_name"`
	FailureDomain    string    `json:"failure_domain"`
	CreatedAt        time.Time `json:"created_at"`
	Revision         int64     `json:"revision"`
	PreviewDigest    string    `json:"preview_digest"`
	Credential       string    `json:"credential,omitempty"`
	SecretAvailable  bool      `json:"secret_available"`
	Replayed         bool      `json:"replayed"`
	RecoveryRequired bool      `json:"recovery_required"`
	RecoveryAction   string    `json:"recovery_action"`
}

func (h *hub) handleListOperatorVerifiers(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.ForceQuery || r.URL.RawQuery != "" {
		writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "verifier list 目前不接受 query parameters")
		return
	}
	result, err := operator.New(h.store).Verifiers()
	if err != nil {
		log.Printf("讀取 operator verifier list 失敗: %v", err)
		writeErr(w, http.StatusInternalServerError, "INTERNAL", "讀取 verifier list 失敗")
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *hub) handleGetOperatorVerifier(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.ForceQuery || r.URL.RawQuery != "" {
		writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "verifier detail 目前不接受 query parameters")
		return
	}
	result, err := operator.New(h.store).VerifierDetail(r.PathValue("id"))
	if err != nil {
		status, code, detail := operator.HTTPError(err)
		if status == http.StatusInternalServerError {
			log.Printf("讀取 operator verifier detail 失敗 verifier=%q: %v", r.PathValue("id"), err)
		}
		writeErr(w, status, code, detail)
		return
	}
	w.Header().Set("ETag", `"verifier-revision-`+strconv.FormatInt(result.Item.Revision, 10)+`"`)
	writeJSON(w, http.StatusOK, result)
}

func (h *hub) handlePreviewOperatorVerifier(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeErr(w, http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE",
			"Content-Type 必須是 application/json")
		return
	}
	var body verifierPreviewOperatorRequest
	if rejection := decodeOperatorBody(w, r, &body); rejection != nil {
		writeErr(w, rejection.Status, rejection.Code, rejection.Detail)
		return
	}
	result, err := operator.New(h.store).PreviewVerifier(operator.VerifierPreviewRequest{
		Kind: body.Kind, DisplayName: body.DisplayName,
		FailureDomain: body.FailureDomain, HubHost: h.hubHost,
	})
	if err != nil {
		status, code, detail := operator.HTTPError(err)
		if status == http.StatusInternalServerError {
			log.Printf("operator verifier preview 失敗: %v", err)
		}
		writeErr(w, status, code, detail)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *hub) handleCreateOperatorVerifier(w http.ResponseWriter, r *http.Request) {
	// Capture the boundary-authenticated actor once. Transport failures and the
	// canonical service path must retain the same authority evidence.
	actor := operatorActor(r)
	w.Header().Set("Cache-Control", "no-store")
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		h.rejectOperatorVerifierTransport(w, r, actor, http.StatusUnsupportedMediaType,
			"UNSUPPORTED_MEDIA_TYPE", "Content-Type 必須是 application/json")
		return
	}
	var body verifierCreateOperatorRequest
	if rejection := decodeOperatorBody(w, r, &body); rejection != nil {
		h.rejectOperatorVerifierTransport(w, r, actor,
			rejection.Status, rejection.Code, rejection.Detail)
		return
	}
	result, err := operator.New(h.store).RegisterVerifier(operator.VerifierCreateRequest{
		Kind: body.Kind, DisplayName: body.DisplayName, FailureDomain: body.FailureDomain,
		HubHost: h.hubHost, PreviewDigest: body.PreviewDigest, Reason: body.Reason,
		IdempotencyKey: r.Header.Get("Idempotency-Key"), Actor: actor,
	})
	if err != nil {
		status, code, detail := operator.HTTPError(err)
		if status == http.StatusInternalServerError {
			log.Printf("operator verifier register 失敗: %v", err)
		}
		var rejection *store.OperatorRequestError
		if errors.As(err, &rejection) && rejection.Replayed {
			w.Header().Set("Idempotency-Replayed", "true")
		}
		writeErr(w, status, code, detail)
		return
	}

	response := verifierOperatorResponse{
		VerifierID: result.VerifierID, Kind: result.Kind, DisplayName: result.DisplayName,
		FailureDomain: result.FailureDomain, CreatedAt: result.CreatedAt,
		Revision: result.Revision, PreviewDigest: result.PreviewDigest,
		Credential: result.Credential, SecretAvailable: result.SecretAvailable,
		Replayed: result.Replayed, RecoveryRequired: result.RecoveryRequired,
		RecoveryAction: result.RecoveryAction,
	}
	if result.Replayed {
		// Fail closed if a future domain regression tries to combine replay and
		// credential delivery. Never serialize the secret even on this error path.
		response.Credential = ""
		if result.Credential != "" || result.SecretAvailable || !result.RecoveryRequired ||
			result.RecoveryAction != store.OperatorVerifierRecoveryRevokeAndRegister {
			log.Printf("operator verifier replay result violated redaction contract")
			writeErr(w, http.StatusInternalServerError, "INTERNAL", "控制面操作失敗")
			return
		}
		w.Header().Set("Idempotency-Replayed", "true")
		writeJSON(w, http.StatusOK, response)
		return
	}
	if result.Credential == "" || !result.SecretAvailable ||
		result.RecoveryRequired || result.RecoveryAction != "" {
		log.Printf("operator verifier fresh result violated one-time secret contract")
		writeErr(w, http.StatusInternalServerError, "INTERNAL", "控制面操作失敗")
		return
	}
	writeJSON(w, http.StatusCreated, response)
}

func (h *hub) handlePreviewOperatorVerifierRevocation(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeErr(w, http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE",
			"Content-Type 必須是 application/json")
		return
	}
	var body verifierRevocationPreviewOperatorRequest
	if rejection := decodeOperatorBody(w, r, &body); rejection != nil {
		writeErr(w, rejection.Status, rejection.Code, rejection.Detail)
		return
	}
	result, err := operator.New(h.store).PreviewVerifierRevocation(
		operator.VerifierRevocationPreviewRequest{VerifierID: r.PathValue("id")})
	if err != nil {
		status, code, detail := operator.HTTPError(err)
		if status == http.StatusInternalServerError {
			log.Printf("operator verifier revocation preview 失敗 verifier=%s: %v", r.PathValue("id"), err)
		}
		writeErr(w, status, code, detail)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *hub) handleCreateOperatorVerifierRevocation(w http.ResponseWriter, r *http.Request) {
	actor := operatorActor(r)
	w.Header().Set("Cache-Control", "no-store")
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		h.rejectOperatorVerifierRevocationTransport(w, r, actor,
			http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE",
			"Content-Type 必須是 application/json")
		return
	}
	var body verifierRevocationOperatorRequest
	if rejection := decodeOperatorBody(w, r, &body); rejection != nil {
		h.rejectOperatorVerifierRevocationTransport(w, r, actor,
			rejection.Status, rejection.Code, rejection.Detail)
		return
	}
	result, err := operator.New(h.store).RevokeVerifier(operator.VerifierRevocationRequest{
		VerifierID: r.PathValue("id"), ExpectedRevision: body.ExpectedRevision,
		ConfirmDisplayName: body.ConfirmDisplayName, PreviewDigest: body.PreviewDigest,
		Reason: body.Reason, IdempotencyKey: r.Header.Get("Idempotency-Key"), Actor: actor,
	})
	if err != nil {
		status, code, detail := operator.HTTPError(err)
		if status == http.StatusInternalServerError {
			log.Printf("operator verifier revocation 失敗 verifier=%s: %v", r.PathValue("id"), err)
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

// rejectOperatorVerifierTransport records a transport rejection without
// occupying an idempotency key.
func (h *hub) rejectOperatorVerifierTransport(w http.ResponseWriter, r *http.Request,
	actor operator.Actor, status int, code, detail string,
) {
	// display_name only exists in the rejected body, so registration has no
	// honest subject; unlike revocation's path ID, leave it empty for the
	// console's typed unreadable-subject state.
	h.recordOperatorTransportRejection(r, actor, store.AuditEntry{
		Action: store.AuditVerifierRegister, Subject: "",
	}, code, detail)
	writeErr(w, status, code, detail)
}

func (h *hub) rejectOperatorVerifierRevocationTransport(w http.ResponseWriter, r *http.Request,
	actor operator.Actor, status int, code, detail string,
) {
	h.recordOperatorTransportRejection(r, actor, store.AuditEntry{
		Action: store.AuditVerifierRevoke, Subject: r.PathValue("id"),
	}, code, detail)
	writeErr(w, status, code, detail)
}
