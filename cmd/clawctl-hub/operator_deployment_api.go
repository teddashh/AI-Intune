package main

import (
	"errors"
	"fmt"
	"log"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/store"
)

// deploymentCreatePreviewOperatorRequest uses pointers so the JSON boundary
// can distinguish an explicit zero/false from a missing field. The stable API
// requires callers to spell every planning input; defaults belong in a CLI or
// HTML adapter, not in an idempotency-sensitive wire document.
type deploymentCreatePreviewOperatorRequest struct {
	Channel                 *string `json:"channel"`
	Version                 *string `json:"version"`
	ArtifactSHA256          *string `json:"artifact_sha256"`
	BatchSize               *int    `json:"batch_size"`
	ExecutionTimeoutSeconds *int    `json:"execution_timeout_seconds"`
	Irreversible            *bool   `json:"irreversible"`
}

type deploymentCreateOperatorRequest struct {
	// Pointers preserve the wire distinction between an explicitly supplied
	// zero value and a missing field. Create apply is idempotency-sensitive, so
	// every planning input must have exactly one explicit JSON spelling before
	// the request is allowed to reach the Store ledger.
	Channel                 *string `json:"channel"`
	Version                 *string `json:"version"`
	ArtifactSHA256          *string `json:"artifact_sha256"`
	BatchSize               *int    `json:"batch_size"`
	ExecutionTimeoutSeconds *int    `json:"execution_timeout_seconds"`
	Irreversible            *bool   `json:"irreversible"`
	PreviewDigest           string  `json:"preview_digest"`
	ConfirmChannel          string  `json:"confirm_channel"`
	ConfirmVersion          string  `json:"confirm_version"`
	Reason                  string  `json:"reason"`
}

type deploymentActionPreviewOperatorRequest struct{}

type deploymentContinueOperatorRequest struct {
	PreviewDigest           string `json:"preview_digest"`
	ExpectedControlRevision *int64 `json:"expected_control_revision"`
	ExpectedOpenedBatch     *int   `json:"expected_opened_batch"`
	ConfirmChannel          string `json:"confirm_channel"`
	Reason                  string `json:"reason"`
}

type deploymentRetryOperatorRequest struct {
	PreviewDigest           string `json:"preview_digest"`
	ExpectedControlRevision *int64 `json:"expected_control_revision"`
	ExpectedOpenedBatch     *int   `json:"expected_opened_batch"`
	ConfirmChannel          string `json:"confirm_channel"`
	ConfirmVersion          string `json:"confirm_version"`
	Reason                  string `json:"reason"`
}

type deploymentAbandonOperatorRequest struct {
	PreviewDigest           string `json:"preview_digest"`
	ExpectedControlRevision *int64 `json:"expected_control_revision"`
	ExpectedOpenedBatch     *int   `json:"expected_opened_batch"`
	ConfirmDeploymentID     string `json:"confirm_deployment_id"`
	Reason                  string `json:"reason"`
}

func (h *hub) handleListOperatorDeployments(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	request, err := parseOperatorDeploymentListRequest(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "BAD_REQUEST", err.Error())
		return
	}
	result, err := operator.New(h.store).ListDeployments(request, time.Now().UTC())
	if err != nil {
		if errors.Is(err, operator.ErrInvalidDeploymentRead) {
			writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "deployment filter 或 cursor 不合法")
			return
		}
		log.Printf("讀取 operator deployment list 失敗: %v", err)
		writeErr(w, http.StatusInternalServerError, "INTERNAL", "讀取 deployment list 失敗")
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *hub) handleGetOperatorDeployment(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.ForceQuery || r.URL.RawQuery != "" {
		writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "deployment detail 不接受 query parameters")
		return
	}
	result, err := operator.New(h.store).DeploymentDetail(r.PathValue("id"), time.Now().UTC())
	if err != nil {
		switch {
		case errors.Is(err, store.ErrDeploymentNotFound):
			writeErr(w, http.StatusNotFound, "DEPLOYMENT_NOT_FOUND", "找不到這張 deployment")
		case errors.Is(err, operator.ErrInvalidDeploymentRead):
			writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "deployment_id 不合法")
		default:
			log.Printf("讀取 operator deployment detail 失敗 deployment=%q: %v", r.PathValue("id"), err)
			writeErr(w, http.StatusInternalServerError, "INTERNAL", "讀取 deployment detail 失敗")
		}
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *hub) handlePreviewOperatorDeploymentCreate(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.ForceQuery || r.URL.RawQuery != "" {
		writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "deployment preview 不接受 query parameters")
		return
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeErr(w, http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE",
			"Content-Type 必須是 application/json")
		return
	}
	var body deploymentCreatePreviewOperatorRequest
	if rejection := decodeOperatorBody(w, r, &body); rejection != nil {
		writeErr(w, rejection.Status, rejection.Code, rejection.Detail)
		return
	}
	if body.Channel == nil || body.Version == nil || body.ArtifactSHA256 == nil ||
		body.BatchSize == nil || body.ExecutionTimeoutSeconds == nil || body.Irreversible == nil {
		writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "deployment preview 必須明列所有 planning fields")
		return
	}
	if *body.BatchSize == 0 || *body.ExecutionTimeoutSeconds == 0 {
		writeErr(w, http.StatusBadRequest, "BAD_REQUEST",
			"deployment preview 的 batch_size 與 execution_timeout_seconds 必須是明確的非零值")
		return
	}
	result, err := operator.NewWithArtifacts(h.store, h.artifactsDir).PreviewDeploymentCreateContext(r.Context(),
		operator.DeploymentCreatePreviewRequest{
			Channel: *body.Channel, Version: *body.Version, ArtifactSHA256: *body.ArtifactSHA256,
			BatchSize: *body.BatchSize, ExecutionTimeoutSeconds: *body.ExecutionTimeoutSeconds,
			Irreversible: *body.Irreversible,
		}, time.Now().UTC())
	if err != nil {
		if errors.Is(err, operator.ErrInvalidDeploymentPreview) || errors.Is(err, store.ErrBadChannel) {
			writeErr(w, http.StatusBadRequest, "BAD_DEPLOYMENT_REQUEST",
				"deployment planning inputs 或 Hub artifact selection 不合法")
			return
		}
		log.Printf("建立 operator deployment preview 失敗: %v", err)
		writeErr(w, http.StatusInternalServerError, "INTERNAL", "建立 deployment preview 失敗")
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *hub) handleCreateOperatorDeployment(w http.ResponseWriter, r *http.Request) {
	actor := operatorActor(r)
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.ForceQuery || r.URL.RawQuery != "" {
		h.rejectOperatorDeploymentTransport(w, r, actor, store.AuditDeploymentCreate,
			"deployment create", http.StatusBadRequest, "BAD_REQUEST", "deployment create 不接受 query parameters")
		return
	}
	if !operatorDeploymentJSONMediaType(w, r, func(status int, code, detail string) {
		h.rejectOperatorDeploymentTransport(w, r, actor, store.AuditDeploymentCreate,
			"deployment create", status, code, detail)
	}) {
		return
	}
	var body deploymentCreateOperatorRequest
	if rejection := decodeOperatorBody(w, r, &body); rejection != nil {
		h.rejectOperatorDeploymentTransport(w, r, actor, store.AuditDeploymentCreate,
			"deployment create", rejection.Status, rejection.Code, rejection.Detail)
		return
	}
	if body.Channel == nil || body.Version == nil || body.ArtifactSHA256 == nil ||
		body.BatchSize == nil || body.ExecutionTimeoutSeconds == nil || body.Irreversible == nil {
		h.rejectOperatorDeploymentTransport(w, r, actor, store.AuditDeploymentCreate,
			"deployment create", http.StatusBadRequest, "BAD_REQUEST",
			"deployment create 必須明列所有 planning fields")
		return
	}
	if *body.BatchSize == 0 || *body.ExecutionTimeoutSeconds == 0 {
		h.rejectOperatorDeploymentTransport(w, r, actor, store.AuditDeploymentCreate,
			"deployment create", http.StatusBadRequest, "BAD_REQUEST",
			"deployment create 的 batch_size 與 execution_timeout_seconds 必須是明確的非零值")
		return
	}
	result, err := operator.NewWithArtifacts(h.store, h.artifactsDir).ApplyDeploymentCreateContext(r.Context(),
		operator.DeploymentCreateApplyRequest{
			DeploymentCreatePreviewRequest: operator.DeploymentCreatePreviewRequest{
				Channel: *body.Channel, Version: *body.Version, ArtifactSHA256: *body.ArtifactSHA256,
				BatchSize: *body.BatchSize, ExecutionTimeoutSeconds: *body.ExecutionTimeoutSeconds,
				Irreversible: *body.Irreversible,
			},
			PreviewDigest: body.PreviewDigest, ConfirmChannel: body.ConfirmChannel,
			ConfirmVersion: body.ConfirmVersion, Reason: body.Reason,
			IdempotencyKey: r.Header.Get("Idempotency-Key"), Actor: actor,
		})
	writeOperatorDeploymentMutation(w, result, err, http.StatusCreated, "create")
}

func (h *hub) handlePreviewOperatorDeploymentContinue(w http.ResponseWriter, r *http.Request) {
	h.previewOperatorDeploymentAction(w, r, "continue")
}

func (h *hub) handlePreviewOperatorDeploymentRetry(w http.ResponseWriter, r *http.Request) {
	h.previewOperatorDeploymentAction(w, r, "retry")
}

func (h *hub) handlePreviewOperatorDeploymentAbandon(w http.ResponseWriter, r *http.Request) {
	h.previewOperatorDeploymentAction(w, r, "abandon")
}

func (h *hub) previewOperatorDeploymentAction(w http.ResponseWriter, r *http.Request, action string) {
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.ForceQuery || r.URL.RawQuery != "" {
		writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "deployment action preview 不接受 query parameters")
		return
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeErr(w, http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE", "Content-Type 必須是 application/json")
		return
	}
	var body deploymentActionPreviewOperatorRequest
	if rejection := decodeOperatorBody(w, r, &body); rejection != nil {
		writeErr(w, rejection.Status, rejection.Code, rejection.Detail)
		return
	}
	service := operator.NewWithArtifacts(h.store, h.artifactsDir)
	now := time.Now().UTC()
	var result operator.DeploymentActionPreviewResult
	switch action {
	case "continue":
		result, err = service.PreviewDeploymentContinueContext(r.Context(), operator.DeploymentContinuePreviewRequest{DeploymentID: r.PathValue("id")}, now)
	case "retry":
		result, err = service.PreviewDeploymentRetryContext(r.Context(), operator.DeploymentRetryPreviewRequest{DeploymentID: r.PathValue("id")}, now)
	case "abandon":
		result, err = service.PreviewDeploymentAbandonContext(r.Context(), operator.DeploymentAbandonPreviewRequest{DeploymentID: r.PathValue("id")}, now)
	default:
		err = errors.New("unknown deployment action")
	}
	if err != nil {
		writeOperatorDeploymentError(w, err, "preview "+action)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *hub) handleContinueOperatorDeployment(w http.ResponseWriter, r *http.Request) {
	actor := operatorActor(r)
	w.Header().Set("Cache-Control", "no-store")
	if !h.prepareOperatorDeploymentMutationTransport(w, r, actor, store.AuditDeploymentContinue) {
		return
	}
	var body deploymentContinueOperatorRequest
	if rejection := decodeOperatorBody(w, r, &body); rejection != nil {
		h.rejectOperatorDeploymentTransport(w, r, actor, store.AuditDeploymentContinue,
			"deployment continue", rejection.Status, rejection.Code, rejection.Detail)
		return
	}
	result, err := operator.NewWithArtifacts(h.store, h.artifactsDir).ApplyDeploymentContinueContext(r.Context(),
		operator.DeploymentContinueApplyRequest{
			DeploymentID: r.PathValue("id"), PreviewDigest: body.PreviewDigest,
			ExpectedControlRevision: body.ExpectedControlRevision, ExpectedOpenedBatch: body.ExpectedOpenedBatch,
			ConfirmChannel: body.ConfirmChannel, Reason: body.Reason,
			IdempotencyKey: r.Header.Get("Idempotency-Key"), Actor: actor,
		})
	writeOperatorDeploymentMutation(w, result, err, http.StatusOK, "continue")
}

func (h *hub) handleRetryOperatorDeployment(w http.ResponseWriter, r *http.Request) {
	actor := operatorActor(r)
	w.Header().Set("Cache-Control", "no-store")
	if !h.prepareOperatorDeploymentMutationTransport(w, r, actor, store.AuditDeploymentRetry) {
		return
	}
	var body deploymentRetryOperatorRequest
	if rejection := decodeOperatorBody(w, r, &body); rejection != nil {
		h.rejectOperatorDeploymentTransport(w, r, actor, store.AuditDeploymentRetry,
			"deployment retry", rejection.Status, rejection.Code, rejection.Detail)
		return
	}
	result, err := operator.NewWithArtifacts(h.store, h.artifactsDir).ApplyDeploymentRetryContext(r.Context(),
		operator.DeploymentRetryApplyRequest{
			DeploymentID: r.PathValue("id"), PreviewDigest: body.PreviewDigest,
			ExpectedControlRevision: body.ExpectedControlRevision, ExpectedOpenedBatch: body.ExpectedOpenedBatch,
			ConfirmChannel: body.ConfirmChannel, ConfirmVersion: body.ConfirmVersion, Reason: body.Reason,
			IdempotencyKey: r.Header.Get("Idempotency-Key"), Actor: actor,
		})
	writeOperatorDeploymentMutation(w, result, err, http.StatusCreated, "retry")
}

func (h *hub) handleAbandonOperatorDeployment(w http.ResponseWriter, r *http.Request) {
	actor := operatorActor(r)
	w.Header().Set("Cache-Control", "no-store")
	if !h.prepareOperatorDeploymentMutationTransport(w, r, actor, store.AuditDeploymentAbandon) {
		return
	}
	var body deploymentAbandonOperatorRequest
	if rejection := decodeOperatorBody(w, r, &body); rejection != nil {
		h.rejectOperatorDeploymentTransport(w, r, actor, store.AuditDeploymentAbandon,
			"deployment abandon", rejection.Status, rejection.Code, rejection.Detail)
		return
	}
	result, err := operator.NewWithArtifacts(h.store, h.artifactsDir).ApplyDeploymentAbandonContext(r.Context(),
		operator.DeploymentAbandonApplyRequest{
			DeploymentID: r.PathValue("id"), PreviewDigest: body.PreviewDigest,
			ExpectedControlRevision: body.ExpectedControlRevision, ExpectedOpenedBatch: body.ExpectedOpenedBatch,
			ConfirmDeploymentID: body.ConfirmDeploymentID, Reason: body.Reason,
			IdempotencyKey: r.Header.Get("Idempotency-Key"), Actor: actor,
		})
	writeOperatorDeploymentMutation(w, result, err, http.StatusOK, "abandon")
}

func operatorDeploymentJSONMediaType(w http.ResponseWriter, r *http.Request,
	reject func(status int, code, detail string),
) bool {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		reject(http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE", "Content-Type 必須是 application/json")
		return false
	}
	return true
}

func (h *hub) prepareOperatorDeploymentMutationTransport(w http.ResponseWriter, r *http.Request,
	actor operator.Actor, auditAction store.AuditAction,
) bool {
	if r.URL.ForceQuery || r.URL.RawQuery != "" {
		h.rejectOperatorDeploymentTransport(w, r, actor, auditAction, "deployment action",
			http.StatusBadRequest, "BAD_REQUEST", "deployment action 不接受 query parameters")
		return false
	}
	return operatorDeploymentJSONMediaType(w, r, func(status int, code, detail string) {
		h.rejectOperatorDeploymentTransport(w, r, actor, auditAction, "deployment action", status, code, detail)
	})
}

func writeOperatorDeploymentMutation(w http.ResponseWriter, result operator.DeploymentMutationResult,
	err error, freshStatus int, action string,
) {
	if err != nil {
		writeOperatorDeploymentError(w, err, action)
		return
	}
	if result.Replayed {
		w.Header().Set("Idempotency-Replayed", "true")
		writeJSON(w, http.StatusOK, result)
		return
	}
	writeJSON(w, freshStatus, result)
}

func writeOperatorDeploymentError(w http.ResponseWriter, err error, action string) {
	status, code, detail := operator.HTTPError(err)
	if errors.Is(err, operator.ErrInvalidDeploymentAction) {
		status, code, detail = http.StatusBadRequest, "BAD_DEPLOYMENT_REQUEST", "deployment action request 不合法"
	} else if errors.Is(err, store.ErrDeploymentNotFound) {
		status, code, detail = http.StatusNotFound, store.OperatorCodeDeploymentNotFound, "找不到指定的 deployment"
	} else if code == store.OperatorCodeDeploymentConfirmationMismatch {
		// The same stable code covers create/continue/retry channel or version
		// confirmation and abandon's full deployment ID confirmation. Do not
		// expose the Store's abandon-specific wording on the other routes.
		detail = "deployment typed confirmation 與操作目標不符"
	}
	var rejection *store.OperatorRequestError
	if errors.As(err, &rejection) && rejection.Replayed {
		w.Header().Set("Idempotency-Replayed", "true")
	}
	if status == http.StatusInternalServerError {
		log.Printf("operator deployment %s 失敗: %v", action, err)
	}
	writeErr(w, status, code, detail)
}

// rejectOperatorDeploymentTransport records a transport rejection without
// occupying the idempotency ledger, so the same key remains usable after
// correction.
func (h *hub) rejectOperatorDeploymentTransport(w http.ResponseWriter, r *http.Request,
	actor operator.Actor, action store.AuditAction, subject string, status int, code, detail string,
) {
	h.recordOperatorTransportRejection(r, actor, store.AuditEntry{
		Action: action, Subject: subject,
	}, code, detail)
	writeErr(w, status, code, detail)
}

func parseOperatorDeploymentListRequest(r *http.Request) (operator.DeploymentListRequest, error) {
	if r == nil || r.URL == nil {
		return operator.DeploymentListRequest{}, errors.New("deployment list request 不完整")
	}
	if r.URL.ForceQuery && r.URL.RawQuery == "" {
		return operator.DeploymentListRequest{}, errors.New("deployment list 不接受空的 query marker")
	}
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return operator.DeploymentListRequest{}, errors.New("deployment list query 編碼不合法")
	}
	allowed := map[string]bool{"channel": true, "state": true, "stuck": true, "limit": true, "cursor": true}
	for key := range values {
		if !allowed[key] {
			return operator.DeploymentListRequest{}, fmt.Errorf("deployment list 不接受 query parameter %q", key)
		}
	}
	request := operator.DeploymentListRequest{}
	if request.Channel, err = oneOperatorDeploymentQueryValue(values, "channel", 16); err != nil {
		return operator.DeploymentListRequest{}, err
	}
	if request.Cursor, err = oneOperatorDeploymentQueryValue(values, "cursor", 2048); err != nil {
		return operator.DeploymentListRequest{}, err
	}
	if raw, present := values["state"]; present {
		seen := make(map[string]bool, len(raw))
		for _, state := range raw {
			if state == "" || state != strings.TrimSpace(state) || seen[state] {
				return operator.DeploymentListRequest{}, errors.New("deployment list state 必須非空、canonical 且不可重複")
			}
			seen[state] = true
			request.States = append(request.States, state)
		}
	}
	if raw, present := values["stuck"]; present {
		if len(raw) != 1 || (raw[0] != "true" && raw[0] != "false") {
			return operator.DeploymentListRequest{}, errors.New("deployment list stuck 只接受 true 或 false，且只能出現一次")
		}
		value := raw[0] == "true"
		request.Stuck = &value
	}
	if raw, present := values["limit"]; present {
		if len(raw) != 1 || raw[0] == "" || raw[0] != strings.TrimSpace(raw[0]) {
			return operator.DeploymentListRequest{}, errors.New("deployment list limit 必須只出現一次且不可為空")
		}
		limit, err := strconv.Atoi(raw[0])
		if err != nil {
			return operator.DeploymentListRequest{}, errors.New("deployment list limit 必須是十進位整數")
		}
		request.Limit = limit
	}
	return request, nil
}

func oneOperatorDeploymentQueryValue(values url.Values, key string, maxBytes int) (string, error) {
	raw, present := values[key]
	if !present {
		return "", nil
	}
	if len(raw) != 1 || raw[0] == "" || raw[0] != strings.TrimSpace(raw[0]) || len(raw[0]) > maxBytes {
		return "", fmt.Errorf("deployment list %s 必須只出現一次、不可為空或含首尾空白", key)
	}
	return raw[0], nil
}
