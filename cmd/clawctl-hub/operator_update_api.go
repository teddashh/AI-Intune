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

func (h *hub) handleGetOperatorUpdates(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	request, err := parseOperatorUpdateReadRequest(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "BAD_REQUEST", err.Error())
		return
	}
	result, err := operator.NewWithArtifacts(h.store, h.artifactsDir).Updates(request, time.Now().UTC())
	if err != nil {
		if errors.Is(err, operator.ErrInvalidUpdateRead) {
			writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "update filter 或 cursor 不合法")
			return
		}
		log.Printf("讀取 operator updates 失敗: %v", err)
		writeErr(w, http.StatusInternalServerError, "INTERNAL", "讀取 updates 失敗")
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func parseOperatorUpdateReadRequest(r *http.Request) (operator.UpdateReadRequest, error) {
	if r == nil || r.URL == nil {
		return operator.UpdateReadRequest{}, errors.New("updates request 不完整")
	}
	if r.URL.ForceQuery && r.URL.RawQuery == "" {
		return operator.UpdateReadRequest{}, errors.New("updates 不接受空的 query marker")
	}
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return operator.UpdateReadRequest{}, errors.New("updates query 編碼不合法")
	}
	for key := range values {
		if key != "channel" && key != "status" && key != "version" && key != "limit" && key != "cursor" {
			return operator.UpdateReadRequest{}, fmt.Errorf("updates 不接受 query parameter %q", key)
		}
	}
	channel, err := oneOperatorUpdateQueryValue(values, "channel", 16)
	if err != nil {
		return operator.UpdateReadRequest{}, err
	}
	if channel != "" && channel != "canary" && channel != "stable" {
		return operator.UpdateReadRequest{}, errors.New("updates channel 必須是 canary 或 stable")
	}
	status, err := oneOperatorUpdateQueryValue(values, "status", 64)
	if err != nil {
		return operator.UpdateReadRequest{}, err
	}
	if status != "" && status != string(operator.ArtifactAvailableUnverified) &&
		status != string(operator.ArtifactUnavailable) && status != string(operator.ArtifactInvalid) {
		return operator.UpdateReadRequest{}, errors.New("updates artifact status 不合法")
	}
	version, err := oneOperatorUpdateQueryValue(values, "version", 128)
	if err != nil {
		return operator.UpdateReadRequest{}, err
	}
	cursor, err := oneOperatorUpdateQueryValue(values, "cursor", 2048)
	if err != nil {
		return operator.UpdateReadRequest{}, err
	}
	request := operator.UpdateReadRequest{
		Channel: channel, Status: operator.ArtifactReadStatus(status), Version: version, Cursor: cursor,
	}
	if raw, present := values["limit"]; present {
		if len(raw) != 1 || raw[0] == "" || raw[0] != strings.TrimSpace(raw[0]) {
			return operator.UpdateReadRequest{}, errors.New("updates limit 必須只出現一次且不可為空")
		}
		request.Limit, err = strconv.Atoi(raw[0])
		if err != nil || request.Limit < 1 || request.Limit > operator.MaxArtifactReadLimit {
			return operator.UpdateReadRequest{}, fmt.Errorf("updates limit 必須介於 1 與 %d", operator.MaxArtifactReadLimit)
		}
	}
	return request, nil
}

func oneOperatorUpdateQueryValue(values url.Values, key string, maxBytes int) (string, error) {
	raw, present := values[key]
	if !present {
		return "", nil
	}
	if len(raw) != 1 || raw[0] == "" || raw[0] != strings.TrimSpace(raw[0]) || len(raw[0]) > maxBytes {
		return "", fmt.Errorf("updates %s 必須只出現一次、不可為空或含首尾空白", key)
	}
	return raw[0], nil
}
