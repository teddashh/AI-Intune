package web

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/store"
)

const auditWebPageLimit = 100

type auditPageView struct {
	Result operator.AuditListResult

	MachineID   string
	Actions     []auditActionFilter
	Outcome     string
	Principal   string
	Capability  string
	SourceKind  string
	Correlation string
	From        string
	To          string
	Denials     string
	Limit       int

	HasFilters    bool
	NextHref      string
	FirstPageHref string
}

type auditActionFilter struct {
	Value    string
	Selected bool
}

// auditPage is an HTML adapter over the same safe read service used by the
// operator JSON API and official CLI. It must not scan or normalize audit rows
// independently: malformed legacy evidence belongs in the DTO's explicit
// null/Issues representation.
func (s *Server) auditPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	request, err := parseAuditPageRequest(r)
	if err != nil {
		http.Error(w, "audit filter 或 cursor 不合法", http.StatusBadRequest)
		return
	}
	now := time.Now().UTC()
	result, err := s.operator.ListAuditContext(r.Context(), request, now)
	if err != nil {
		if errors.Is(err, operator.ErrInvalidAuditRead) {
			http.Error(w, "audit filter 或 cursor 不合法", http.StatusBadRequest)
			return
		}
		log.Printf("failed to read audit page: %v", err)
		http.Error(w, "讀取動作紀錄失敗", http.StatusInternalServerError)
		return
	}

	selected := make(map[store.AuditAction]bool, len(request.Actions))
	for _, action := range request.Actions {
		selected[action] = true
	}
	view := &auditPageView{
		Result: result, MachineID: request.MachineID,
		Outcome: string(request.Outcome), Principal: request.Principal, Capability: request.Capability,
		SourceKind: request.SourceKind, Correlation: request.Correlation,
		From: auditWebTime(request.From), To: auditWebTime(request.To),
		Denials: string(result.Denials.Mode), Limit: request.Limit,
		HasFilters: request.MachineID != "" || len(request.Actions) > 0 || request.Outcome != "" ||
			request.Principal != "" || request.Capability != "" || request.SourceKind != "" ||
			request.Correlation != "" || request.From != nil || request.To != nil ||
			request.Denials == operator.AuditDenialsAll || request.Limit != auditWebPageLimit,
	}
	for _, action := range store.AuditActions() {
		view.Actions = append(view.Actions, auditActionFilter{Value: string(action), Selected: selected[action]})
	}
	if result.NextCursor != nil {
		view.NextHref = auditWebURL(request, *result.NextCursor)
	}
	if request.Cursor != "" {
		view.FirstPageHref = auditWebURL(request, "")
	}

	// ⚠ Title 不帶「· clawctl」—— base.html 已經接了。
	s.render(w, r, "audit.html", page{
		Title: "稽核記錄", Nav: "audit", Now: now.Local().Format("2006-01-02 15:04"), Audit: view,
	})
}

func parseAuditPageRequest(r *http.Request) (operator.AuditListRequest, error) {
	if r == nil || r.URL == nil {
		return operator.AuditListRequest{}, errors.New("missing request URL")
	}
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return operator.AuditListRequest{}, errors.New("invalid query encoding")
	}
	allowed := map[string]bool{
		"machine": true, "machine_id": true, "action": true, "outcome": true,
		"principal": true, "capability": true, "source_kind": true, "correlation": true,
		"from": true, "to": true, "denials": true, "limit": true, "cursor": true,
	}
	for key := range values {
		if !allowed[key] {
			return operator.AuditListRequest{}, errors.New("unknown query field")
		}
	}
	if _, legacy := values["machine"]; legacy {
		if _, canonical := values["machine_id"]; canonical {
			return operator.AuditListRequest{}, errors.New("ambiguous machine filter")
		}
	}
	request := operator.AuditListRequest{Limit: auditWebPageLimit}
	outcome := ""
	fields := []struct {
		name   string
		target *string
	}{
		{"outcome", &outcome}, {"principal", &request.Principal}, {"capability", &request.Capability},
		{"source_kind", &request.SourceKind}, {"correlation", &request.Correlation}, {"cursor", &request.Cursor},
	}
	if _, ok := values["machine_id"]; ok {
		fields = append(fields, struct {
			name   string
			target *string
		}{"machine_id", &request.MachineID})
	} else {
		fields = append(fields, struct {
			name   string
			target *string
		}{"machine", &request.MachineID})
	}
	for i := range fields {
		value, err := oneAuditWebValue(values, fields[i].name)
		if err != nil {
			return operator.AuditListRequest{}, err
		}
		*fields[i].target = value
	}
	request.Outcome = store.AuditOutcome(outcome)
	if raw, present := values["action"]; present {
		seen := make(map[store.AuditAction]bool, len(raw))
		for _, value := range raw {
			action := store.AuditAction(value)
			if value == "" || !store.IsKnownAuditAction(action) || seen[action] {
				return operator.AuditListRequest{}, errors.New("invalid action filter")
			}
			seen[action] = true
			request.Actions = append(request.Actions, action)
		}
	}
	if value, err := oneAuditWebValue(values, "denials"); err != nil {
		return operator.AuditListRequest{}, err
	} else if value != "" {
		request.Denials = operator.AuditDenialMode(value)
	} else {
		request.Denials = operator.AuditDenialsSampled
	}
	if value, err := oneAuditWebValue(values, "limit"); err != nil {
		return operator.AuditListRequest{}, err
	} else if value != "" {
		request.Limit, err = strconv.Atoi(value)
		if err != nil || request.Limit < 1 || request.Limit > operator.MaxAuditReadLimit {
			return operator.AuditListRequest{}, errors.New("invalid limit")
		}
	}
	for _, field := range []struct {
		name   string
		target **time.Time
	}{
		{"from", &request.From}, {"to", &request.To},
	} {
		value, err := oneAuditWebValue(values, field.name)
		if err != nil {
			return operator.AuditListRequest{}, err
		}
		if value == "" {
			continue
		}
		parsed, err := time.Parse(time.RFC3339, value)
		if err != nil || parsed.Nanosecond() != 0 {
			return operator.AuditListRequest{}, errors.New("invalid time filter")
		}
		parsed = parsed.UTC()
		*field.target = &parsed
	}
	return request, nil
}

func oneAuditWebValue(values url.Values, name string) (string, error) {
	raw, present := values[name]
	if !present {
		return "", nil
	}
	if len(raw) != 1 {
		return "", fmt.Errorf("duplicate %s", name)
	}
	return raw[0], nil
}

func auditWebTime(value *time.Time) string {
	if value == nil {
		return ""
	}
	return value.UTC().Format(time.RFC3339)
}

func auditWebURL(request operator.AuditListRequest, cursor string) string {
	values := make(url.Values)
	if request.MachineID != "" {
		values.Set("machine_id", request.MachineID)
	}
	for _, action := range request.Actions {
		values.Add("action", string(action))
	}
	for _, field := range []struct{ name, value string }{
		{"outcome", string(request.Outcome)}, {"principal", request.Principal},
		{"capability", request.Capability}, {"source_kind", request.SourceKind},
		{"correlation", request.Correlation}, {"from", auditWebTime(request.From)},
		{"to", auditWebTime(request.To)}, {"denials", string(request.Denials)},
	} {
		if field.value != "" {
			values.Set(field.name, field.value)
		}
	}
	values.Set("limit", strconv.Itoa(request.Limit))
	if cursor != "" {
		values.Set("cursor", cursor)
	}
	encoded := values.Encode()
	if encoded == "" {
		return "/audit"
	}
	return "/audit?" + encoded
}
