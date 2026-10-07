package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/teddashh/AI-Intune/internal/artifact"
	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/store"
)

type artifactFetchPreviewOperatorRequest struct {
	Name    *string `json:"name"`
	Version *string `json:"version"`
}

type artifactFetchCreateOperatorRequest struct {
	Name           *string `json:"name"`
	Version        *string `json:"version"`
	PreviewDigest  *string `json:"preview_digest"`
	ConfirmName    *string `json:"confirm_name"`
	ConfirmVersion *string `json:"confirm_version"`
	Reason         *string `json:"reason"`
}

func (h *hub) handlePreviewOperatorArtifactFetch(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.ForceQuery || r.URL.RawQuery != "" {
		writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "artifact fetch preview 不接受 query parameters")
		return
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeErr(w, http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE", "Content-Type 必須是 application/json")
		return
	}
	var body artifactFetchPreviewOperatorRequest
	if rejection := decodeOperatorBody(w, r, &body); rejection != nil {
		writeErr(w, rejection.Status, rejection.Code, rejection.Detail)
		return
	}
	if body.Name == nil || body.Version == nil {
		writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "artifact fetch preview 必須明列 name 與 version")
		return
	}
	result, err := operator.NewWithArtifacts(h.store, h.artifactsDir).PreviewArtifactFetch(r.Context(),
		operator.ArtifactFetchPreviewRequest{Name: *body.Name, Version: *body.Version})
	if err != nil {
		writeOperatorArtifactFetchPreviewError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *hub) handleCreateOperatorArtifactFetch(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	actor := operatorActor(r)
	if r.URL.ForceQuery || r.URL.RawQuery != "" {
		h.rejectOperatorArtifactFetchTransport(w, r, actor, http.StatusBadRequest, "BAD_REQUEST",
			"artifact fetch create 不接受 query parameters")
		return
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		h.rejectOperatorArtifactFetchTransport(w, r, actor, http.StatusUnsupportedMediaType,
			"UNSUPPORTED_MEDIA_TYPE", "Content-Type 必須是 application/json")
		return
	}
	var body artifactFetchCreateOperatorRequest
	if rejection := decodeOperatorBody(w, r, &body); rejection != nil {
		h.rejectOperatorArtifactFetchTransport(w, r, actor, rejection.Status, rejection.Code, rejection.Detail)
		return
	}
	if body.Name == nil || body.Version == nil || body.PreviewDigest == nil ||
		body.ConfirmName == nil || body.ConfirmVersion == nil || body.Reason == nil {
		h.rejectOperatorArtifactFetchTransport(w, r, actor, http.StatusBadRequest, "BAD_REQUEST",
			"artifact fetch create 必須明列所有欄位")
		return
	}
	result, err := operator.NewWithArtifacts(h.store, h.artifactsDir).ApplyArtifactFetch(r.Context(),
		operator.ArtifactFetchApplyRequest{
			Name: *body.Name, Version: *body.Version, PreviewDigest: *body.PreviewDigest,
			ConfirmName: *body.ConfirmName, ConfirmVersion: *body.ConfirmVersion, Reason: *body.Reason,
			IdempotencyKey: r.Header.Get("Idempotency-Key"), Actor: actor,
		})
	if err != nil {
		writeOperatorArtifactFetchError(w, err)
		return
	}
	if result.Replayed {
		w.Header().Set("Idempotency-Replayed", "true")
	}
	writeJSON(w, http.StatusAccepted, result)
}

func (h *hub) handleListOperatorArtifactFetches(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	request, err := parseOperatorArtifactFetchListRequest(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "BAD_REQUEST", err.Error())
		return
	}
	result, err := h.store.ListArtifactFetchOperations(request)
	if err != nil {
		if errors.Is(err, store.ErrArtifactFetchInvalid) {
			writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "artifact fetch operation filter 不合法")
			return
		}
		log.Printf("failed to read artifact fetch operations: %v", err)
		writeErr(w, http.StatusInternalServerError, "INTERNAL", "讀取 artifact fetch operations 失敗")
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *hub) handleGetOperatorArtifactFetch(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.ForceQuery || r.URL.RawQuery != "" {
		writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "artifact fetch operation detail 不接受 query parameters")
		return
	}
	result, err := h.store.GetArtifactFetchOperation(r.PathValue("id"))
	if err != nil {
		if errors.Is(err, store.ErrArtifactFetchNotFound) {
			writeErr(w, http.StatusNotFound, "ARTIFACT_FETCH_NOT_FOUND", "找不到指定的 artifact fetch operation")
			return
		}
		log.Printf("failed to read artifact fetch operation operation=%q: %v", r.PathValue("id"), err)
		writeErr(w, http.StatusInternalServerError, "INTERNAL", "讀取 artifact fetch operation 失敗")
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func parseOperatorArtifactFetchListRequest(r *http.Request) (store.ArtifactFetchListRequest, error) {
	if r == nil || r.URL == nil {
		return store.ArtifactFetchListRequest{}, errors.New("artifact fetch operation list request 不完整")
	}
	if r.URL.ForceQuery && r.URL.RawQuery == "" {
		return store.ArtifactFetchListRequest{}, errors.New("artifact fetch operation list 不接受空 query marker")
	}
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return store.ArtifactFetchListRequest{}, errors.New("artifact fetch operation query 編碼不合法")
	}
	for key := range values {
		if key != "state" && key != "name" && key != "version" && key != "limit" {
			return store.ArtifactFetchListRequest{}, fmt.Errorf("artifact fetch operation list 不接受 query parameter %q", key)
		}
	}
	state, err := oneOperatorArtifactFetchQueryValue(values, "state", 32)
	if err != nil {
		return store.ArtifactFetchListRequest{}, err
	}
	name, err := oneOperatorArtifactFetchQueryValue(values, "name", 128)
	if err != nil {
		return store.ArtifactFetchListRequest{}, err
	}
	version, err := oneOperatorArtifactFetchQueryValue(values, "version", 128)
	if err != nil {
		return store.ArtifactFetchListRequest{}, err
	}
	request := store.ArtifactFetchListRequest{State: store.ArtifactFetchState(state), Name: name, Version: version}
	if request.State != "" && request.State != store.ArtifactFetchQueued && request.State != store.ArtifactFetchRunning &&
		request.State != store.ArtifactFetchSucceeded && request.State != store.ArtifactFetchFailed {
		return store.ArtifactFetchListRequest{}, errors.New("artifact fetch operation state 不合法")
	}
	if raw, present := values["limit"]; present {
		if len(raw) != 1 || raw[0] == "" || raw[0] != strings.TrimSpace(raw[0]) {
			return store.ArtifactFetchListRequest{}, errors.New("artifact fetch operation limit 必須只出現一次且不可為空")
		}
		request.Limit, err = strconv.Atoi(raw[0])
		if err != nil || request.Limit < 1 || request.Limit > store.MaxArtifactFetchReadLimit {
			return store.ArtifactFetchListRequest{}, fmt.Errorf("artifact fetch operation limit 必須介於 1 與 %d", store.MaxArtifactFetchReadLimit)
		}
	}
	return request, nil
}

func oneOperatorArtifactFetchQueryValue(values url.Values, key string, maxBytes int) (string, error) {
	raw, present := values[key]
	if !present {
		return "", nil
	}
	if len(raw) != 1 || raw[0] == "" || raw[0] != strings.TrimSpace(raw[0]) || len(raw[0]) > maxBytes {
		return "", fmt.Errorf("artifact fetch operation %s 必須只出現一次、不可為空或含首尾空白", key)
	}
	return raw[0], nil
}

func writeOperatorArtifactFetchPreviewError(w http.ResponseWriter, err error) {
	var upstream *artifact.UpstreamVersionError
	if errors.As(err, &upstream) {
		if sentence := upstream.OperatorSentence(); sentence != "" {
			writeErr(w, http.StatusConflict, "UPSTREAM_VERSION_UNAVAILABLE", sentence)
			return
		}
		writeErr(w, http.StatusBadGateway, "REGISTRY_RESPONSE_REJECTED", "registry metadata 未通過 intake policy")
		return
	}
	switch {
	case errors.Is(err, operator.ErrInvalidArtifactFetchPreview), errors.Is(err, artifact.ErrInvalidFetchRequest):
		writeErr(w, http.StatusBadRequest, "BAD_ARTIFACT_FETCH_REQUEST", "artifact name 或 exact version 不合法")
	case errors.Is(err, context.DeadlineExceeded):
		writeErr(w, http.StatusGatewayTimeout, "REGISTRY_TIMEOUT", "讀取 registry metadata 逾時")
	case errors.Is(err, artifact.ErrRegistryPolicy), errors.Is(err, artifact.ErrMetadataInvalid),
		errors.Is(err, artifact.ErrMetadataTooLarge):
		writeErr(w, http.StatusBadGateway, "REGISTRY_RESPONSE_REJECTED", "registry metadata 未通過 intake policy")
	default:
		log.Printf("failed to create artifact fetch preview: %v", err)
		writeErr(w, http.StatusInternalServerError, "INTERNAL", "建立 artifact fetch preview 失敗")
	}
}

func writeOperatorArtifactFetchError(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrArtifactFetchPrepareFailed) {
		writeErr(w, http.StatusBadGateway, store.OperatorCodeArtifactFetchPrepareFailed,
			"registry metadata 驗證失敗；request 未入列；使用相同 key 重試")
		return
	}
	status, code, detail := operator.HTTPError(err)
	var rejection *store.OperatorRequestError
	if errors.As(err, &rejection) && rejection.Replayed {
		w.Header().Set("Idempotency-Replayed", "true")
	}
	if status == http.StatusInternalServerError {
		log.Printf("failed to create artifact fetch operation: %v", err)
	}
	writeErr(w, status, code, detail)
}

func (h *hub) rejectOperatorArtifactFetchTransport(w http.ResponseWriter, r *http.Request,
	actor operator.Actor, status int, code, detail string,
) {
	h.recordOperatorTransportRejection(r, actor, store.AuditEntry{
		Action: store.AuditArtifactFetch, Subject: "artifact fetch request",
	}, code, detail)
	writeErr(w, status, code, detail)
}
