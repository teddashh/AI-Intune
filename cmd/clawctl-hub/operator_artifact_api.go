package main

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/teddashh/AI-Intune/internal/operator"
)

func (h *hub) handleListOperatorArtifacts(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	request, err := parseOperatorArtifactListRequest(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "BAD_REQUEST", err.Error())
		return
	}
	result, err := operator.NewWithArtifacts(h.store, h.artifactsDir).ListArtifacts(request, time.Now().UTC())
	if err != nil {
		if errors.Is(err, operator.ErrInvalidArtifactRead) {
			writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "artifact filter 或 cursor 不合法")
			return
		}
		log.Printf("failed to read operator artifact list: %v", err)
		writeErr(w, http.StatusInternalServerError, "INTERNAL", "讀取 artifact list 失敗")
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *hub) handleGetOperatorArtifact(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.ForceQuery || r.URL.RawQuery != "" {
		writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "artifact detail 不接受 query parameters")
		return
	}
	result, err := operator.NewWithArtifacts(h.store, h.artifactsDir).
		ArtifactDetail(r.Context(), r.PathValue("sha256"), time.Now().UTC())
	if err != nil {
		switch {
		case errors.Is(err, operator.ErrArtifactNotFound):
			writeErr(w, http.StatusNotFound, "ARTIFACT_NOT_FOUND", "找不到指定的 artifact")
		case errors.Is(err, operator.ErrInvalidArtifactRead):
			writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "artifact identity 不合法")
		default:
			log.Printf("failed to read operator artifact detail artifact=%q: %v", r.PathValue("sha256"), err)
			writeErr(w, http.StatusInternalServerError, "INTERNAL", "讀取 artifact detail 失敗")
		}
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func parseOperatorArtifactListRequest(r *http.Request) (operator.ArtifactListRequest, error) {
	if r == nil || r.URL == nil {
		return operator.ArtifactListRequest{}, errors.New("artifact list request 不完整")
	}
	if r.URL.ForceQuery && r.URL.RawQuery == "" {
		return operator.ArtifactListRequest{}, errors.New("artifact list 不接受空的 query marker")
	}
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return operator.ArtifactListRequest{}, errors.New("artifact list query 編碼不合法")
	}
	for key := range values {
		if key != "status" && key != "version" && key != "limit" && key != "cursor" {
			return operator.ArtifactListRequest{}, fmt.Errorf("artifact list 不接受 query parameter %q", key)
		}
	}
	status, err := oneOperatorArtifactQueryValue(values, "status", 64)
	if err != nil {
		return operator.ArtifactListRequest{}, err
	}
	version, err := oneOperatorArtifactQueryValue(values, "version", 128)
	if err != nil {
		return operator.ArtifactListRequest{}, err
	}
	cursor, err := oneOperatorArtifactQueryValue(values, "cursor", 2048)
	if err != nil {
		return operator.ArtifactListRequest{}, err
	}
	request := operator.ArtifactListRequest{
		Status: operator.ArtifactReadStatus(status), Version: version, Cursor: cursor,
	}
	if request.Status != "" && request.Status != operator.ArtifactAvailableUnverified &&
		request.Status != operator.ArtifactUnavailable && request.Status != operator.ArtifactInvalid {
		return operator.ArtifactListRequest{}, errors.New("artifact list status 不合法")
	}
	if raw, present := values["limit"]; present {
		if len(raw) != 1 || raw[0] == "" || raw[0] != strings.TrimSpace(raw[0]) {
			return operator.ArtifactListRequest{}, errors.New("artifact list limit 必須只出現一次且不可為空")
		}
		request.Limit, err = strconv.Atoi(raw[0])
		if err != nil {
			return operator.ArtifactListRequest{}, errors.New("artifact list limit 必須是十進位整數")
		}
		if request.Limit < 1 || request.Limit > operator.MaxArtifactReadLimit {
			return operator.ArtifactListRequest{}, fmt.Errorf("artifact list limit 必須介於 1 與 %d", operator.MaxArtifactReadLimit)
		}
	}
	return request, nil
}

func oneOperatorArtifactQueryValue(values url.Values, key string, maxBytes int) (string, error) {
	raw, present := values[key]
	if !present {
		return "", nil
	}
	if len(raw) != 1 || raw[0] == "" || raw[0] != strings.TrimSpace(raw[0]) || len(raw[0]) > maxBytes {
		return "", fmt.Errorf("artifact list %s 必須只出現一次、不可為空或含首尾空白", key)
	}
	return raw[0], nil
}
