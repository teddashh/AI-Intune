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
	"unicode"
	"unicode/utf8"

	"github.com/teddashh/AI-Intune/internal/operator"
)

func (h *hub) handleListOperatorChanges(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	request, err := parseOperatorChangeListRequest(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "changes query 不合法")
		return
	}
	result, err := operator.New(h.store).ListChangesContext(r.Context(), request, time.Now().UTC())
	if err != nil {
		if !writeOperatorChangeReadError(w, err) {
			log.Printf("讀取 operator changes 失敗: %v", err)
			writeErr(w, http.StatusInternalServerError, "INTERNAL", "讀取 changes 失敗")
		}
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func writeOperatorChangeReadError(w http.ResponseWriter, err error) bool {
	switch {
	case errors.Is(err, operator.ErrInvalidChangeRead):
		writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "changes filter、window 或 cursor 不合法")
	case errors.Is(err, operator.ErrChangeReadTraversalGone):
		writeErr(w, http.StatusGone, "CHANGE_TRAVERSAL_GONE", "retention 已使這次 changes traversal 失效；請從第一頁重讀")
	case errors.Is(err, operator.ErrChangeReadTooBroad):
		writeErr(w, http.StatusUnprocessableEntity, "CHANGE_READ_TOO_BROAD", "changes 查詢超過安全成本上限；請縮短 window 或增加 machine、kind、subject filter")
	case errors.Is(err, operator.ErrChangeReadBusy):
		w.Header().Set("Retry-After", "1")
		writeErr(w, http.StatusTooManyRequests, "CHANGE_READ_BUSY", "changes reader 正忙；請稍後重試")
	case errors.Is(err, operator.ErrChangeReadTimedOut):
		w.Header().Set("Retry-After", "1")
		writeErr(w, http.StatusServiceUnavailable, "CHANGE_READ_TIMEOUT", "changes 查詢超時；請縮短 window 或增加 filter 後重試")
	default:
		return false
	}
	return true
}

func parseOperatorChangeListRequest(r *http.Request) (operator.ChangeListRequest, error) {
	if r == nil || r.URL == nil {
		return operator.ChangeListRequest{}, errors.New("changes request 不完整")
	}
	if r.URL.ForceQuery && r.URL.RawQuery == "" {
		return operator.ChangeListRequest{}, errors.New("changes 不接受空的 query marker")
	}
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return operator.ChangeListRequest{}, errors.New("changes query 編碼不合法")
	}
	allowed := map[string]bool{
		"machine_id": true, "kind": true, "subject": true, "from": true,
		"to": true, "limit": true, "cursor": true,
	}
	for key := range values {
		if !allowed[key] {
			return operator.ChangeListRequest{}, fmt.Errorf("changes 不接受 query parameter %q", key)
		}
	}
	request := operator.ChangeListRequest{}
	if request.MachineID, err = oneOperatorChangeQueryValue(values, "machine_id", 256); err != nil {
		return operator.ChangeListRequest{}, err
	}
	if request.Subject, err = oneOperatorChangeQueryValue(values, "subject", 256); err != nil {
		return operator.ChangeListRequest{}, err
	}
	if request.Cursor, err = oneOperatorChangeQueryValue(values, "cursor", 4096); err != nil {
		return operator.ChangeListRequest{}, err
	}
	if raw, present := values["kind"]; present {
		if len(raw) == 0 || len(raw) > len(operator.ChangeKinds()) {
			return operator.ChangeListRequest{}, errors.New("changes kind 數量不合法")
		}
		known := make(map[string]bool, len(operator.ChangeKinds()))
		for _, kind := range operator.ChangeKinds() {
			known[kind] = true
		}
		seen := make(map[string]bool, len(raw))
		for _, kind := range raw {
			if !known[kind] || seen[kind] {
				return operator.ChangeListRequest{}, errors.New("changes kind 必須是 canonical 且不可重複")
			}
			seen[kind] = true
			request.Kinds = append(request.Kinds, kind)
		}
	}
	if raw, present := values["limit"]; present {
		if len(raw) != 1 || !validOperatorChangeQueryText(raw[0], 3) {
			return operator.ChangeListRequest{}, errors.New("changes limit 必須只出現一次且為 canonical 整數")
		}
		request.Limit, err = strconv.Atoi(raw[0])
		if err != nil || strconv.Itoa(request.Limit) != raw[0] ||
			request.Limit < 1 || request.Limit > operator.MaxChangeReadLimit {
			return operator.ChangeListRequest{}, fmt.Errorf("changes limit 必須介於 1 與 %d", operator.MaxChangeReadLimit)
		}
	}
	for _, field := range []struct {
		name   string
		target **time.Time
	}{
		{"from", &request.From}, {"to", &request.To},
	} {
		raw, present := values[field.name]
		if !present {
			continue
		}
		if len(raw) != 1 || !validOperatorChangeQueryText(raw[0], 64) {
			return operator.ChangeListRequest{}, fmt.Errorf("changes %s 必須只出現一次且不可為空", field.name)
		}
		parsed, parseErr := time.Parse(time.RFC3339, raw[0])
		if parseErr != nil || parsed.Nanosecond() != 0 || parsed.Format(time.RFC3339) != raw[0] {
			return operator.ChangeListRequest{}, fmt.Errorf("changes %s 必須是 second-precision RFC3339", field.name)
		}
		parsed = parsed.UTC()
		*field.target = &parsed
	}
	if err := operator.ValidateChangeListRequest(request); err != nil {
		return operator.ChangeListRequest{}, err
	}
	return request, nil
}

func oneOperatorChangeQueryValue(values url.Values, name string, maxBytes int) (string, error) {
	raw, present := values[name]
	if !present {
		return "", nil
	}
	if len(raw) != 1 || !validOperatorChangeQueryText(raw[0], maxBytes) {
		return "", fmt.Errorf("changes %s 必須只出現一次、不可為空或含首尾空白", name)
	}
	return raw[0], nil
}

func validOperatorChangeQueryText(value string, maxBytes int) bool {
	if value == "" || len(value) > maxBytes || !utf8.ValidString(value) || strings.TrimSpace(value) != value {
		return false
	}
	for _, char := range value {
		if unicode.IsControl(char) || unicode.Is(unicode.Cf, char) {
			return false
		}
	}
	return true
}
