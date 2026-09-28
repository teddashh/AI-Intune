package operatorclient

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/store"
)

// A legal 100-item page can exceed the general 1 MiB response cap. Every
// string in AuditEvent is bounded by the safe projection, writeJSON disables
// HTML escaping, and JSON escaping can at most double those bounded bytes;
// 4 MiB therefore leaves headroom while retaining a hard response limit.
const maxAuditResponseBytes int64 = 4 << 20

func (c *Client) AuditEvents(ctx context.Context, request operator.AuditListRequest) (operator.AuditListResult, error) {
	query, effectiveLimit, effectiveDenials, err := encodeAuditListQuery(request)
	if err != nil {
		return operator.AuditListResult{}, err
	}
	if request.Cursor != "" {
		if _, err := decodeAuditClientCursor(request.Cursor, auditClientFilterDigest(request, effectiveDenials)); err != nil {
			return operator.AuditListResult{}, fmt.Errorf("operator client: invalid audit cursor: %w", err)
		}
	}
	path := "/v1/operator/audit-events"
	if encoded := query.Encode(); encoded != "" {
		path += "?" + encoded
	}
	req, err := c.newOperatorRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return operator.AuditListResult{}, err
	}
	response, err := c.doRawWithLimit(req, maxAuditResponseBytes, "4 MiB")
	if err != nil {
		return operator.AuditListResult{}, err
	}
	if response.status != http.StatusOK {
		return operator.AuditListResult{}, fmt.Errorf(
			"operator client: audit list returned unexpected success status HTTP %d", response.status)
	}
	if err := validateAuditReadHeaders(response.header); err != nil {
		return operator.AuditListResult{}, err
	}
	var result operator.AuditListResult
	if err := decodeStrictJSONDocument(response.body, "audit list", &result); err != nil {
		return operator.AuditListResult{}, err
	}
	if err := validateAuditListResult(result, request, effectiveLimit, effectiveDenials); err != nil {
		return operator.AuditListResult{}, err
	}
	return result, nil
}

func encodeAuditListQuery(request operator.AuditListRequest) (url.Values, int, operator.AuditDenialMode, error) {
	query := make(url.Values)
	for _, field := range []struct {
		name  string
		value string
		max   int
	}{
		{"machine_id", request.MachineID, 256},
		{"principal", request.Principal, 512},
		{"capability", request.Capability, 512},
		{"source_kind", request.SourceKind, 128},
		{"correlation", request.Correlation, 256},
		{"cursor", request.Cursor, 2048},
	} {
		if field.value == "" {
			continue
		}
		if err := validateAuditQueryText(field.name, field.value, field.max); err != nil {
			return nil, 0, "", err
		}
		query.Set(field.name, field.value)
	}
	seenActions := make(map[store.AuditAction]bool, len(request.Actions))
	for _, action := range request.Actions {
		if !store.IsKnownAuditAction(action) || seenActions[action] {
			return nil, 0, "", fmt.Errorf("operator client: invalid or duplicate audit action %q", action)
		}
		seenActions[action] = true
		query.Add("action", string(action))
	}
	if request.Outcome != "" {
		if request.Outcome != store.AuditOutcomeOK && request.Outcome != store.AuditOutcomeFailed {
			return nil, 0, "", errors.New("operator client: audit outcome must be ok or failed")
		}
		query.Set("outcome", string(request.Outcome))
	}
	for _, field := range []struct {
		name  string
		value *time.Time
	}{
		{"from", request.From},
		{"to", request.To},
	} {
		if field.value == nil {
			continue
		}
		if field.value.IsZero() || field.value.Nanosecond() != 0 {
			return nil, 0, "", fmt.Errorf("operator client: audit %s must use non-zero second precision", field.name)
		}
		query.Set(field.name, field.value.UTC().Format(time.RFC3339))
	}
	if request.From != nil && request.To != nil && request.From.After(*request.To) {
		return nil, 0, "", errors.New("operator client: audit from must not be after to")
	}
	denials := request.Denials
	if denials == "" {
		denials = operator.AuditDenialsSampled
	} else if denials != operator.AuditDenialsSampled && denials != operator.AuditDenialsAll {
		return nil, 0, "", errors.New("operator client: audit denials must be sampled or all")
	}
	if request.Denials != "" {
		query.Set("denials", string(request.Denials))
	}
	limit := request.Limit
	if limit == 0 {
		limit = operator.DefaultAuditReadLimit
	} else {
		if limit < 1 || limit > operator.MaxAuditReadLimit {
			return nil, 0, "", fmt.Errorf("operator client: audit limit must be between 1 and %d", operator.MaxAuditReadLimit)
		}
		query.Set("limit", strconv.Itoa(limit))
	}
	return query, limit, denials, nil
}

func validateAuditQueryText(name, value string, maxBytes int) error {
	if value == "" || value != strings.TrimSpace(value) || len(value) > maxBytes || !utf8.ValidString(value) {
		return fmt.Errorf("operator client: audit %s is empty, oversized, invalid UTF-8, or has surrounding whitespace", name)
	}
	for _, char := range value {
		if unicode.IsControl(char) || unicode.Is(unicode.Cf, char) {
			return fmt.Errorf("operator client: audit %s contains control or formatting characters", name)
		}
	}
	return nil
}

func validateAuditReadHeaders(header http.Header) error {
	if err := validateJSONNoStoreResponse(header); err != nil {
		return err
	}
	if replayed, err := responseReplayEvidenceFromHeader(header); err != nil {
		return err
	} else if replayed {
		return errors.New("operator client: audit read cannot be an idempotency replay")
	}
	if values := header.Values("ETag"); len(values) != 0 {
		return errors.New("operator client: composite audit read must not carry an ETag")
	}
	return nil
}

func validateAuditListResult(result operator.AuditListResult, request operator.AuditListRequest,
	limit int, denialMode operator.AuditDenialMode,
) error {
	if result.SchemaVersion != operator.AuditReadSchemaVersion || result.Consistency != operator.AuditReadConsistency {
		return errors.New("operator client: unsupported audit schema or consistency contract")
	}
	if result.EvaluatedAt.IsZero() || result.EvaluatedAt.Location() != time.UTC {
		return errors.New("operator client: audit evaluated_at must be non-zero UTC")
	}
	if result.CreationCeiling < 0 || result.Items == nil || result.MatchedTotal < 0 || result.Total < 0 ||
		result.Total > result.MatchedTotal || result.Total < len(result.Items) || len(result.Items) > limit ||
		result.Succeeded < 0 || result.Failed < 0 || result.UnknownOutcome < 0 ||
		result.Succeeded > result.Total || result.Failed > result.Total-result.Succeeded ||
		result.UnknownOutcome != result.Total-result.Succeeded-result.Failed {
		return errors.New("operator client: inconsistent audit totals or page size")
	}
	denials := result.Denials
	if denials.Mode != denialMode || denials.Matched < 0 || denials.Included < 0 || denials.Omitted < 0 ||
		denials.Included > denials.Matched || denials.Omitted != denials.Matched-denials.Included ||
		denials.Matched > result.MatchedTotal ||
		denials.Included > result.Total || result.MatchedTotal-result.Total != denials.Omitted {
		return errors.New("operator client: inconsistent operator-denial sampling evidence")
	}
	if denialMode == operator.AuditDenialsAll && denials.Omitted != 0 {
		return errors.New("operator client: all-denials read omitted evidence")
	}
	if denialMode == operator.AuditDenialsSampled && denials.Included > operator.DefaultAuditOperatorDenialSample {
		return errors.New("operator client: sampled-denials read exceeded its fixed cap")
	}
	if len(request.Actions) > 0 {
		selectsDenials := auditActionSelected(store.AuditOperatorDenied, request.Actions)
		if !selectsDenials && (denials.Matched != 0 || denials.Included != 0 || denials.Omitted != 0) {
			return errors.New("operator client: denial aggregates contradict action filter")
		}
		if selectsDenials && len(request.Actions) == 1 &&
			(result.MatchedTotal != denials.Matched || result.Total != denials.Included) {
			return errors.New("operator client: denial-only totals contradict action filter")
		}
	}
	switch request.Outcome {
	case store.AuditOutcomeOK:
		if result.Succeeded != result.Total || result.Failed != 0 || result.UnknownOutcome != 0 {
			return errors.New("operator client: outcome aggregates contradict ok filter")
		}
	case store.AuditOutcomeFailed:
		if result.Failed != result.Total || result.Succeeded != 0 || result.UnknownOutcome != 0 {
			return errors.New("operator client: outcome aggregates contradict failed filter")
		}
	}
	expectedDigest := auditClientFilterDigest(request, denialMode)
	if request.Cursor != "" {
		current, err := decodeAuditClientCursor(request.Cursor, expectedDigest)
		if err != nil || current.CreationCeiling != result.CreationCeiling {
			return errors.New("operator client: audit response changed cursor creation ceiling")
		}
		if len(result.Items) > 0 && result.Items[0].AuditID >= current.AfterAuditID {
			return errors.New("operator client: audit response did not advance after cursor anchor")
		}
	}
	if (result.NextCursor == nil) != (result.NextAfterAuditID == nil) {
		return errors.New("operator client: audit continuation fields disagree")
	}
	if result.NextCursor != nil {
		if err := validateAuditQueryText("next_cursor", *result.NextCursor, 2048); err != nil {
			return err
		}
		cursor, err := decodeAuditClientCursor(*result.NextCursor, expectedDigest)
		if err != nil || len(result.Items) != limit || *result.NextCursor == request.Cursor ||
			cursor.CreationCeiling != result.CreationCeiling ||
			cursor.AfterAuditID != result.Items[len(result.Items)-1].AuditID ||
			*result.NextAfterAuditID != cursor.AfterAuditID {
			return errors.New("operator client: audit cursor cannot advance this traversal")
		}
	}
	if request.Cursor == "" && (result.Total > len(result.Items)) != (result.NextCursor != nil) {
		return errors.New("operator client: first audit page has inconsistent continuation evidence")
	}
	seenIDs := make(map[int64]bool, len(result.Items))
	pageSucceeded, pageFailed, pageUnknown, pageDenials := 0, 0, 0, 0
	for i, item := range result.Items {
		if err := validateAuditEvent(item, request); err != nil {
			return fmt.Errorf("operator client: audit item %d: %w", i, err)
		}
		if item.AuditID > result.CreationCeiling || seenIDs[item.AuditID] ||
			i > 0 && result.Items[i-1].AuditID <= item.AuditID {
			return errors.New("operator client: audit items are duplicate or not in writer order")
		}
		seenIDs[item.AuditID] = true
		if item.Action == string(store.AuditOperatorDenied) {
			pageDenials++
		}
		switch {
		case item.Succeeded():
			pageSucceeded++
		case item.Failed():
			pageFailed++
		default:
			pageUnknown++
		}
	}
	if pageSucceeded > result.Succeeded || pageFailed > result.Failed || pageUnknown > result.UnknownOutcome ||
		pageDenials > denials.Included ||
		request.Cursor == "" && result.NextCursor == nil && pageDenials != denials.Included {
		return errors.New("operator client: audit aggregate counts contradict page items")
	}
	return nil
}

func validateAuditEvent(item operator.AuditEvent, request operator.AuditListRequest) error {
	if item.AuditID <= 0 || item.Issues == nil || item.AlteredFields == nil {
		return errors.New("invalid writer sequence or null evidence arrays")
	}
	if err := validateAuditResponseText("action", item.Action, 128, false); err != nil {
		return err
	}
	if err := validateAuditResponseText("subject", item.Subject, 2048, false); err != nil {
		return err
	}
	if err := validateAuditResponseText("source_addr", item.SourceAddr, 512, false); err != nil {
		return err
	}
	for _, field := range []struct {
		name  string
		value *string
		max   int
	}{
		{"machine_id", item.MachineID, 512}, {"reason", item.Reason, 2048},
		{"idempotency_key", item.IdempotencyKey, 512}, {"request_digest", item.RequestDigest, 512},
		{"who_node", item.WhoNode, 512}, {"who_user", item.WhoUser, 512},
		{"who_unavailable", item.WhoUnavailable, 1024}, {"user_agent", item.UserAgent, 512},
		{"auth_subject", item.AuthSubject, 512}, {"auth_node_id", item.AuthNodeID, 512},
		{"auth_capability", item.AuthCapability, 512}, {"auth_method", item.AuthMethod, 256},
		{"auth_decision", item.AuthDecision, 256}, {"boundary_decision", item.BoundaryDecision, 256},
		{"source_kind", item.SourceKind, 256}, {"detail", item.Detail, 2048},
	} {
		if field.value != nil {
			if err := validateAuditResponseText(field.name, *field.value, field.max, true); err != nil {
				return err
			}
		}
	}
	issues, err := auditStringSet(item.Issues, map[string]bool{
		"invalid_at": true, "unknown_action": true, "unknown_outcome": true,
		"empty_subject": true, "empty_source_addr": true,
	})
	if err != nil {
		return err
	}
	knownAction := store.IsKnownAuditAction(store.AuditAction(item.Action))
	if issues["invalid_at"] != (item.At == nil) || issues["unknown_action"] != !knownAction ||
		issues["unknown_outcome"] != (item.Outcome == nil) || issues["empty_subject"] != (item.Subject == "") ||
		issues["empty_source_addr"] != (item.SourceAddr == "") {
		return errors.New("audit issues contradict represented evidence")
	}
	if item.At != nil && (item.At.IsZero() || item.At.Location() != time.UTC) {
		return errors.New("audit timestamp is not non-zero UTC")
	}
	if item.Outcome != nil && *item.Outcome != store.AuditOutcomeOK && *item.Outcome != store.AuditOutcomeFailed {
		return errors.New("audit outcome is not canonical")
	}
	allowedAltered := map[string]bool{
		"action": true, "subject": true, "source_addr": true, "machine_id": true, "reason": true,
		"idempotency_key": true, "request_digest": true, "who_node": true, "who_user": true,
		"who_unavailable": true, "user_agent": true, "auth_subject": true, "auth_node_id": true,
		"auth_capability": true, "auth_method": true, "auth_decision": true,
		"boundary_decision": true, "source_kind": true, "detail": true,
	}
	if _, err := auditStringSet(item.AlteredFields, allowedAltered); err != nil {
		return err
	}
	canonicalOperatorAction := store.IsCanonicalOperatorAction(store.AuditAction(item.Action))
	expectedReplay := canonicalOperatorAction && item.Detail != nil &&
		strings.HasPrefix(*item.Detail, store.OperatorIdempotencyReplayPrefix)
	if item.Replayed != expectedReplay {
		return errors.New("replay classification contradicts canonical evidence")
	}
	expectedTransportRejection := canonicalOperatorAction && item.Detail != nil &&
		strings.HasPrefix(*item.Detail, store.OperatorTransportRejectionPrefix)
	if item.TransportRejection != expectedTransportRejection {
		return errors.New("transport-rejection classification contradicts canonical evidence")
	}
	if request.MachineID != "" && (item.MachineID == nil || *item.MachineID != request.MachineID) {
		return errors.New("audit item does not match machine_id filter")
	}
	if len(request.Actions) > 0 && (!knownAction || !auditActionSelected(store.AuditAction(item.Action), request.Actions)) {
		return errors.New("audit item does not match action filter")
	}
	if request.Outcome != "" && (item.Outcome == nil || *item.Outcome != request.Outcome) {
		return errors.New("audit item does not match outcome filter")
	}
	if request.Principal != "" && !auditPointerEquals(item.AuthSubject, request.Principal) &&
		!auditPointerEquals(item.WhoUser, request.Principal) {
		return errors.New("audit item does not match principal filter")
	}
	if request.Capability != "" && !auditPointerEquals(item.AuthCapability, request.Capability) {
		return errors.New("audit item does not match capability filter")
	}
	if request.SourceKind != "" && !auditPointerEquals(item.SourceKind, request.SourceKind) {
		return errors.New("audit item does not match source_kind filter")
	}
	if request.Correlation != "" && !auditPointerEquals(item.IdempotencyKey, request.Correlation) &&
		!auditPointerEquals(item.RequestDigest, request.Correlation) {
		return errors.New("audit item does not match correlation filter")
	}
	if item.At == nil && (request.From != nil || request.To != nil) {
		return errors.New("audit item with invalid timestamp cannot match a time filter")
	}
	if item.At != nil && request.From != nil && item.At.Before(request.From.UTC()) ||
		item.At != nil && request.To != nil && item.At.After(request.To.UTC()) {
		return errors.New("audit item is outside requested time window")
	}
	return nil
}

func validateAuditResponseText(name, value string, maxBytes int, requireNonEmpty bool) error {
	if requireNonEmpty && value == "" {
		return fmt.Errorf("audit %s is unexpectedly empty", name)
	}
	if len(value) > maxBytes || !utf8.ValidString(value) {
		return fmt.Errorf("audit %s has an invalid safe projection", name)
	}
	for _, char := range value {
		if unicode.IsControl(char) || unicode.Is(unicode.Cf, char) {
			return fmt.Errorf("audit %s retained control or formatting characters", name)
		}
	}
	return nil
}

type auditClientCursor struct {
	Version         int    `json:"v"`
	FilterDigest    string `json:"filter_digest"`
	CreationCeiling int64  `json:"creation_ceiling"`
	AfterAuditID    int64  `json:"after_audit_id"`
}

func decodeAuditClientCursor(encoded, filterDigest string) (auditClientCursor, error) {
	if encoded == "" || len(encoded) > 2048 {
		return auditClientCursor{}, errors.New("invalid audit cursor length")
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || base64.RawURLEncoding.EncodeToString(raw) != encoded {
		return auditClientCursor{}, errors.New("invalid audit cursor encoding")
	}
	var cursor auditClientCursor
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cursor); err != nil {
		return auditClientCursor{}, errors.New("invalid audit cursor document")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return auditClientCursor{}, errors.New("invalid trailing audit cursor data")
	}
	canonical, err := json.Marshal(cursor)
	if err != nil || base64.RawURLEncoding.EncodeToString(canonical) != encoded ||
		cursor.Version != operator.AuditReadSchemaVersion || cursor.FilterDigest != filterDigest ||
		cursor.CreationCeiling < 1 || cursor.AfterAuditID < 1 || cursor.AfterAuditID > cursor.CreationCeiling {
		return auditClientCursor{}, errors.New("audit cursor does not match request")
	}
	return cursor, nil
}

func auditClientFilterDigest(request operator.AuditListRequest, denials operator.AuditDenialMode) string {
	selected := make(map[store.AuditAction]bool, len(request.Actions))
	for _, action := range request.Actions {
		selected[action] = true
	}
	actions := make([]store.AuditAction, 0, len(selected))
	for _, action := range store.AuditActions() {
		if selected[action] {
			actions = append(actions, action)
		}
	}
	body := struct {
		Version     int                      `json:"v"`
		MachineID   string                   `json:"machine_id"`
		Actions     []store.AuditAction      `json:"actions"`
		Outcome     store.AuditOutcome       `json:"outcome"`
		Principal   string                   `json:"principal"`
		Capability  string                   `json:"capability"`
		SourceKind  string                   `json:"source_kind"`
		Correlation string                   `json:"correlation"`
		From        string                   `json:"from"`
		To          string                   `json:"to"`
		Denials     operator.AuditDenialMode `json:"denials"`
	}{
		Version: operator.AuditReadSchemaVersion, MachineID: request.MachineID, Actions: actions,
		Outcome: request.Outcome, Principal: request.Principal, Capability: request.Capability,
		SourceKind: request.SourceKind, Correlation: request.Correlation,
		From: auditClientCursorTime(request.From), To: auditClientCursorTime(request.To), Denials: denials,
	}
	raw, _ := json.Marshal(body)
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func auditClientCursorTime(value *time.Time) string {
	if value == nil {
		return ""
	}
	return value.UTC().Format(time.RFC3339)
}

func auditStringSet(values []string, allowed map[string]bool) (map[string]bool, error) {
	result := make(map[string]bool, len(values))
	for _, value := range values {
		if !allowed[value] || result[value] {
			return nil, fmt.Errorf("invalid or duplicate audit classification %q", value)
		}
		result[value] = true
	}
	return result, nil
}

func auditActionSelected(value store.AuditAction, selected []store.AuditAction) bool {
	for _, candidate := range selected {
		if value == candidate {
			return true
		}
	}
	return false
}

func auditPointerEquals(value *string, want string) bool { return value != nil && *value == want }
