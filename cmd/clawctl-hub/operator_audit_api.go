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
	"github.com/teddashh/AI-Intune/internal/store"
)

func (h *hub) handleListOperatorAuditEvents(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	request, err := parseOperatorAuditListRequest(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "BAD_REQUEST", err.Error())
		return
	}
	result, err := operator.New(h.store).ListAuditContext(r.Context(), request, time.Now().UTC())
	if err != nil {
		if errors.Is(err, operator.ErrInvalidAuditRead) {
			writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "audit filter 或 cursor 不合法")
			return
		}
		log.Printf("讀取 operator audit events 失敗: %v", err)
		writeErr(w, http.StatusInternalServerError, "INTERNAL", "讀取 audit events 失敗")
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func parseOperatorAuditListRequest(r *http.Request) (operator.AuditListRequest, error) {
	if r == nil || r.URL == nil {
		return operator.AuditListRequest{}, errors.New("audit request 不完整")
	}
	if r.URL.ForceQuery && r.URL.RawQuery == "" {
		return operator.AuditListRequest{}, errors.New("audit list 不接受空的 query marker")
	}
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return operator.AuditListRequest{}, errors.New("audit query 編碼不合法")
	}
	allowed := map[string]bool{
		"machine_id": true, "action": true, "outcome": true, "principal": true,
		"capability": true, "source_kind": true, "correlation": true,
		"from": true, "to": true, "denials": true, "limit": true, "cursor": true,
	}
	for key := range values {
		if !allowed[key] {
			return operator.AuditListRequest{}, fmt.Errorf("audit list 不接受 query parameter %q", key)
		}
	}
	request := operator.AuditListRequest{}
	for _, field := range []struct {
		name   string
		max    int
		target *string
	}{
		{"machine_id", 256, &request.MachineID},
		{"principal", 512, &request.Principal},
		{"capability", 512, &request.Capability},
		{"source_kind", 128, &request.SourceKind},
		{"correlation", 256, &request.Correlation},
		{"cursor", 2048, &request.Cursor},
	} {
		value, err := oneOperatorAuditQueryValue(values, field.name, field.max)
		if err != nil {
			return operator.AuditListRequest{}, err
		}
		*field.target = value
	}
	if raw, present := values["action"]; present {
		if len(raw) == 0 || len(raw) > store.AuditActionCount() {
			return operator.AuditListRequest{}, errors.New("audit action 數量不合法")
		}
		seen := make(map[store.AuditAction]bool, len(raw))
		for _, value := range raw {
			action := store.AuditAction(value)
			if !store.IsKnownAuditAction(action) || seen[action] {
				return operator.AuditListRequest{}, errors.New("audit action 必須是 canonical action 且不可重複")
			}
			seen[action] = true
			request.Actions = append(request.Actions, action)
		}
	}
	outcome, err := oneOperatorAuditQueryValue(values, "outcome", 16)
	if err != nil {
		return operator.AuditListRequest{}, err
	}
	request.Outcome = store.AuditOutcome(outcome)
	if request.Outcome != "" && request.Outcome != store.AuditOutcomeOK && request.Outcome != store.AuditOutcomeFailed {
		return operator.AuditListRequest{}, errors.New("audit outcome 必須是 ok 或 failed")
	}
	denials, err := oneOperatorAuditQueryValue(values, "denials", 16)
	if err != nil {
		return operator.AuditListRequest{}, err
	}
	request.Denials = operator.AuditDenialMode(denials)
	if request.Denials != "" && request.Denials != operator.AuditDenialsSampled && request.Denials != operator.AuditDenialsAll {
		return operator.AuditListRequest{}, errors.New("audit denials 必須是 sampled 或 all")
	}
	for _, field := range []struct {
		name   string
		target **time.Time
	}{
		{"from", &request.From},
		{"to", &request.To},
	} {
		raw, err := oneOperatorAuditQueryValue(values, field.name, 64)
		if err != nil {
			return operator.AuditListRequest{}, err
		}
		if raw == "" {
			continue
		}
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil || parsed.Nanosecond() != 0 {
			return operator.AuditListRequest{}, fmt.Errorf("audit %s 必須是 second-precision RFC3339", field.name)
		}
		parsed = parsed.UTC()
		*field.target = &parsed
	}
	if request.From != nil && request.To != nil && request.From.After(*request.To) {
		return operator.AuditListRequest{}, errors.New("audit from 不可晚於 to")
	}
	if raw, present := values["limit"]; present {
		if len(raw) != 1 || raw[0] == "" || raw[0] != strings.TrimSpace(raw[0]) {
			return operator.AuditListRequest{}, errors.New("audit limit 必須只出現一次且不可為空")
		}
		request.Limit, err = strconv.Atoi(raw[0])
		if err != nil || request.Limit < 1 || request.Limit > operator.MaxAuditReadLimit {
			return operator.AuditListRequest{}, fmt.Errorf("audit limit 必須介於 1 與 %d", operator.MaxAuditReadLimit)
		}
	}
	return request, nil
}

func oneOperatorAuditQueryValue(values url.Values, key string, maxBytes int) (string, error) {
	raw, present := values[key]
	if !present {
		return "", nil
	}
	if len(raw) != 1 || raw[0] == "" || raw[0] != strings.TrimSpace(raw[0]) || len(raw[0]) > maxBytes {
		return "", fmt.Errorf("audit %s 必須只出現一次、不可為空或含首尾空白", key)
	}
	return raw[0], nil
}
