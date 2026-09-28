package operator

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
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/teddashh/AI-Intune/internal/store"
)

const (
	AuditReadSchemaVersion           = 1
	AuditReadConsistency             = "creation_ceiling_with_bounded_denials"
	DefaultAuditReadLimit            = 100
	MaxAuditReadLimit                = store.MaxAuditReadPageSize
	DefaultAuditOperatorDenialSample = 50
)

var ErrInvalidAuditRead = errors.New("operator: invalid audit read request")

type AuditDenialMode string

const (
	AuditDenialsSampled AuditDenialMode = "sampled"
	AuditDenialsAll     AuditDenialMode = "all"
)

type AuditListRequest struct {
	MachineID   string
	Actions     []store.AuditAction
	Outcome     store.AuditOutcome
	Principal   string
	Capability  string
	SourceKind  string
	Correlation string
	From        *time.Time
	To          *time.Time
	Denials     AuditDenialMode
	Limit       int
	Cursor      string
}

type AuditListResult struct {
	SchemaVersion    int          `json:"schema_version"`
	Consistency      string       `json:"consistency"`
	EvaluatedAt      time.Time    `json:"evaluated_at"`
	CreationCeiling  int64        `json:"creation_ceiling"`
	MatchedTotal     int          `json:"matched_total"`
	Total            int          `json:"total"`
	Succeeded        int          `json:"succeeded"`
	Failed           int          `json:"failed"`
	UnknownOutcome   int          `json:"unknown_outcome"`
	Denials          AuditDenials `json:"operator_denials"`
	Items            []AuditEvent `json:"items"`
	NextAfterAuditID *int64       `json:"next_after_audit_id"`
	NextCursor       *string      `json:"next_cursor"`
}

type AuditDenials struct {
	Mode     AuditDenialMode `json:"mode"`
	Matched  int             `json:"matched"`
	Included int             `json:"included"`
	Omitted  int             `json:"omitted"`
}

// AuditEvent is an explicit operator allowlist. It intentionally does not
// serialize store.AuditEntry: malformed legacy action/outcome/time evidence is
// represented as null plus Issues instead of being silently normalized.
type AuditEvent struct {
	AuditID int64               `json:"audit_id"`
	At      *time.Time          `json:"at"`
	Action  string              `json:"action"`
	Outcome *store.AuditOutcome `json:"outcome"`

	MachineID      *string `json:"machine_id"`
	Subject        string  `json:"subject"`
	Reason         *string `json:"reason"`
	IdempotencyKey *string `json:"idempotency_key"`
	RequestDigest  *string `json:"request_digest"`

	SourceAddr     string  `json:"source_addr"`
	WhoNode        *string `json:"who_node"`
	WhoUser        *string `json:"who_user"`
	WhoUnavailable *string `json:"who_unavailable"`
	UserAgent      *string `json:"user_agent"`

	AuthSubject      *string `json:"auth_subject"`
	AuthNodeID       *string `json:"auth_node_id"`
	AuthCapability   *string `json:"auth_capability"`
	AuthMethod       *string `json:"auth_method"`
	AuthDecision     *string `json:"auth_decision"`
	BoundaryDecision *string `json:"boundary_decision"`
	SourceKind       *string `json:"source_kind"`
	Detail           *string `json:"detail"`

	Replayed           bool     `json:"replayed"`
	TransportRejection bool     `json:"transport_rejection"`
	Issues             []string `json:"issues"`
	AlteredFields      []string `json:"altered_fields"`
}

func (e AuditEvent) IsOperatorReplay() bool { return e.Replayed }

func (e AuditEvent) IsOperatorTransportRejection() bool { return e.TransportRejection }

func (e AuditEvent) IsRateLimitedDenial() bool {
	return e.BoundaryDecision != nil && *e.BoundaryDecision == "OPERATOR_DENIALS_RATE_LIMITED"
}

func (e AuditEvent) IsDirectDBSource() bool {
	return e.SourceKind != nil && *e.SourceKind == "direct-db-cli"
}

func (e AuditEvent) OutcomeKnown() bool { return e.Outcome != nil }

func (e AuditEvent) Succeeded() bool {
	return e.Outcome != nil && *e.Outcome == store.AuditOutcomeOK
}

func (e AuditEvent) Failed() bool {
	return e.Outcome != nil && *e.Outcome == store.AuditOutcomeFailed
}

// WhoLabel keeps the existing console semantics while all raw evidence remains
// separately available on the DTO. It never invents an authenticated actor.
func (e AuditEvent) WhoLabel() string {
	switch {
	case e.WhoNode != nil && e.WhoUser != nil:
		return *e.WhoNode + "（" + *e.WhoUser + "）"
	case e.WhoNode != nil:
		return *e.WhoNode
	// Tailscale 授權不要求節點名字；問不到名字不等於使用者匿名。
	case e.WhoUser != nil:
		return firstNonEmpty(e.SourceAddr, "未知裝置") + "（" + *e.WhoUser + "）"
	case e.SourceAddr != "":
		if e.WhoUnavailable != nil {
			return e.SourceAddr + " —— " + *e.WhoUnavailable
		}
		return e.SourceAddr
	default:
		return "連來源位址都沒有"
	}
}

func (s *Service) ListAudit(request AuditListRequest, evaluatedAt time.Time) (AuditListResult, error) {
	return s.ListAuditContext(context.Background(), request, evaluatedAt)
}

func (s *Service) ListAuditContext(ctx context.Context, request AuditListRequest, evaluatedAt time.Time) (AuditListResult, error) {
	if ctx == nil {
		return AuditListResult{}, fmt.Errorf("%w: context is required", ErrInvalidAuditRead)
	}
	if evaluatedAt.IsZero() {
		return AuditListResult{}, fmt.Errorf("%w: evaluated_at is required", ErrInvalidAuditRead)
	}
	evaluatedAt = evaluatedAt.UTC()
	normalized, err := normalizeAuditListRequest(request)
	if err != nil {
		return AuditListResult{}, err
	}
	filterDigest := auditFilterDigest(normalized)
	storeFilter := store.AuditReadFilter{
		MachineID: normalized.MachineID, Actions: normalized.Actions, Outcome: normalized.Outcome,
		Principal: normalized.Principal, Capability: normalized.Capability,
		SourceKind: normalized.SourceKind, Correlation: normalized.Correlation,
		From: normalized.From, To: normalized.To, Limit: normalized.Limit,
	}
	if normalized.Denials == AuditDenialsSampled {
		storeFilter.SampleOperatorDenials = DefaultAuditOperatorDenialSample
	}
	if normalized.Cursor != "" {
		cursor, err := decodeAuditListCursor(normalized.Cursor, filterDigest)
		if err != nil {
			return AuditListResult{}, err
		}
		storeFilter.CreationCeiling = &cursor.CreationCeiling
		storeFilter.After = &store.AuditReadPosition{AuditID: cursor.AfterAuditID}
	}
	page, err := s.store.ListAuditReadsContext(ctx, storeFilter)
	if err != nil {
		if errors.Is(err, store.ErrInvalidAuditRead) {
			return AuditListResult{}, fmt.Errorf("%w: %v", ErrInvalidAuditRead, err)
		}
		return AuditListResult{}, err
	}
	result := AuditListResult{
		SchemaVersion: AuditReadSchemaVersion, Consistency: AuditReadConsistency,
		EvaluatedAt: evaluatedAt, CreationCeiling: page.CreationCeiling,
		MatchedTotal: page.MatchedTotal, Total: page.Total,
		Succeeded: page.Succeeded, Failed: page.Failed, UnknownOutcome: page.UnknownOutcome,
		Denials: AuditDenials{
			Mode: normalized.Denials, Matched: page.OperatorDenialsTotal,
			Included: page.OperatorDenialsIncluded,
			Omitted:  page.OperatorDenialsTotal - page.OperatorDenialsIncluded,
		},
		Items: make([]AuditEvent, 0, len(page.Items)),
	}
	for _, record := range page.Items {
		result.Items = append(result.Items, projectAuditEvent(record))
	}
	if page.Next != nil {
		nextAfter := page.Next.AuditID
		result.NextAfterAuditID = &nextAfter
		encoded, err := encodeAuditListCursor(auditListCursor{
			Version: AuditReadSchemaVersion, FilterDigest: filterDigest,
			CreationCeiling: page.CreationCeiling, AfterAuditID: page.Next.AuditID,
		})
		if err != nil {
			return AuditListResult{}, err
		}
		result.NextCursor = &encoded
	}
	return result, nil
}

func normalizeAuditListRequest(request AuditListRequest) (AuditListRequest, error) {
	for name, field := range map[string]struct {
		value string
		max   int
	}{
		"machine_id":  {request.MachineID, 256},
		"principal":   {request.Principal, 512},
		"capability":  {request.Capability, 512},
		"source_kind": {request.SourceKind, 128},
		"correlation": {request.Correlation, 256},
	} {
		if field.value != "" && !validAuditFilterText(field.value, field.max) {
			return AuditListRequest{}, fmt.Errorf("%w: %s is not canonical", ErrInvalidAuditRead, name)
		}
	}
	if request.Outcome != "" && request.Outcome != store.AuditOutcomeOK && request.Outcome != store.AuditOutcomeFailed {
		return AuditListRequest{}, fmt.Errorf("%w: outcome is not canonical", ErrInvalidAuditRead)
	}
	seen := make(map[store.AuditAction]bool, len(request.Actions))
	for _, action := range request.Actions {
		if !store.IsKnownAuditAction(action) || seen[action] {
			return AuditListRequest{}, fmt.Errorf("%w: action %q is unknown or repeated", ErrInvalidAuditRead, action)
		}
		seen[action] = true
	}
	request.Actions = make([]store.AuditAction, 0, len(seen))
	for _, action := range store.AuditActions() {
		if seen[action] {
			request.Actions = append(request.Actions, action)
		}
	}
	for name, value := range map[string]*time.Time{"from": request.From, "to": request.To} {
		if value != nil {
			if value.IsZero() || value.Nanosecond() != 0 {
				return AuditListRequest{}, fmt.Errorf("%w: %s must use non-zero second precision", ErrInvalidAuditRead, name)
			}
			utc := value.UTC()
			if name == "from" {
				request.From = &utc
			} else {
				request.To = &utc
			}
		}
	}
	if request.From != nil && request.To != nil && request.From.After(*request.To) {
		return AuditListRequest{}, fmt.Errorf("%w: from must not be after to", ErrInvalidAuditRead)
	}
	if request.Denials == "" {
		request.Denials = AuditDenialsSampled
	}
	if request.Denials != AuditDenialsSampled && request.Denials != AuditDenialsAll {
		return AuditListRequest{}, fmt.Errorf("%w: denials mode is not canonical", ErrInvalidAuditRead)
	}
	if request.Limit == 0 {
		request.Limit = DefaultAuditReadLimit
	}
	if request.Limit < 1 || request.Limit > MaxAuditReadLimit {
		return AuditListRequest{}, fmt.Errorf("%w: limit must be between 1 and %d", ErrInvalidAuditRead, MaxAuditReadLimit)
	}
	if request.Cursor != "" && !validAuditFilterText(request.Cursor, 2048) {
		return AuditListRequest{}, fmt.Errorf("%w: cursor is not canonical", ErrInvalidAuditRead)
	}
	return request, nil
}

func validAuditFilterText(value string, maxBytes int) bool {
	if value == "" || value != strings.TrimSpace(value) || len(value) > maxBytes || !utf8.ValidString(value) {
		return false
	}
	for _, char := range value {
		if unicode.IsControl(char) || unicode.Is(unicode.Cf, char) {
			return false
		}
	}
	return true
}

func projectAuditEvent(record store.AuditReadRecord) AuditEvent {
	event := AuditEvent{
		AuditID: record.AuditID, Replayed: record.IsOperatorReplay(),
		TransportRejection: record.IsOperatorTransportRejection(),
		Issues:             append([]string{}, record.Issues...), AlteredFields: []string{},
	}
	if record.At != nil {
		at := record.At.UTC()
		event.At = &at
	}
	if record.Outcome != nil {
		outcome := *record.Outcome
		event.Outcome = &outcome
	}
	event.Action = auditSafeText(record.Action, 128, "action", &event.AlteredFields)
	event.Subject = auditSafeText(record.Subject, 2048, "subject", &event.AlteredFields)
	event.SourceAddr = auditSafeText(record.SourceAddr, 512, "source_addr", &event.AlteredFields)
	event.MachineID = auditSafeTextPointer(record.MachineID, 512, "machine_id", &event.AlteredFields)
	event.Reason = auditSafeTextPointer(record.Reason, 2048, "reason", &event.AlteredFields)
	event.IdempotencyKey = auditSafeTextPointer(record.IdempotencyKey, 512, "idempotency_key", &event.AlteredFields)
	event.RequestDigest = auditSafeTextPointer(record.RequestDigest, 512, "request_digest", &event.AlteredFields)
	event.WhoNode = auditSafeTextPointer(record.WhoNode, 512, "who_node", &event.AlteredFields)
	event.WhoUser = auditSafeTextPointer(record.WhoUser, 512, "who_user", &event.AlteredFields)
	event.WhoUnavailable = auditSafeTextPointer(record.WhoUnavailable, 1024, "who_unavailable", &event.AlteredFields)
	event.UserAgent = auditSafeTextPointer(record.UserAgent, 512, "user_agent", &event.AlteredFields)
	event.AuthSubject = auditSafeTextPointer(record.AuthSubject, 512, "auth_subject", &event.AlteredFields)
	event.AuthNodeID = auditSafeTextPointer(record.AuthNodeID, 512, "auth_node_id", &event.AlteredFields)
	event.AuthCapability = auditSafeTextPointer(record.AuthCapability, 512, "auth_capability", &event.AlteredFields)
	event.AuthMethod = auditSafeTextPointer(record.AuthMethod, 256, "auth_method", &event.AlteredFields)
	event.AuthDecision = auditSafeTextPointer(record.AuthDecision, 256, "auth_decision", &event.AlteredFields)
	event.BoundaryDecision = auditSafeTextPointer(record.BoundaryDecision, 256, "boundary_decision", &event.AlteredFields)
	event.SourceKind = auditSafeTextPointer(record.SourceKind, 256, "source_kind", &event.AlteredFields)
	event.Detail = auditSafeTextPointer(record.Detail, 2048, "detail", &event.AlteredFields)
	return event
}

func auditSafeTextPointer(value string, maxBytes int, field string, altered *[]string) *string {
	if value == "" {
		return nil
	}
	result := auditSafeText(value, maxBytes, field, altered)
	return &result
}

func auditSafeText(value string, maxBytes int, field string, altered *[]string) string {
	result := value
	changed := false
	if !utf8.ValidString(result) {
		result = strings.ToValidUTF8(result, "�")
		changed = true
	}
	if strings.IndexFunc(result, func(char rune) bool {
		return unicode.IsControl(char) || unicode.Is(unicode.Cf, char)
	}) >= 0 {
		result = strings.Map(func(char rune) rune {
			if unicode.IsControl(char) || unicode.Is(unicode.Cf, char) {
				return '�'
			}
			return char
		}, result)
		changed = true
	}
	if len(result) > maxBytes {
		const marker = "…"
		budget := maxBytes - len(marker)
		if budget < 0 {
			budget = 0
		}
		cut := 0
		for index := range result {
			if index > budget {
				break
			}
			cut = index
		}
		if len(result) <= budget {
			cut = len(result)
		}
		result = result[:cut] + marker
		changed = true
	}
	if changed {
		*altered = append(*altered, field)
	}
	return result
}

type auditListCursor struct {
	Version         int    `json:"v"`
	FilterDigest    string `json:"filter_digest"`
	CreationCeiling int64  `json:"creation_ceiling"`
	AfterAuditID    int64  `json:"after_audit_id"`
}

func auditFilterDigest(request AuditListRequest) string {
	body := struct {
		Version     int                 `json:"v"`
		MachineID   string              `json:"machine_id"`
		Actions     []store.AuditAction `json:"actions"`
		Outcome     store.AuditOutcome  `json:"outcome"`
		Principal   string              `json:"principal"`
		Capability  string              `json:"capability"`
		SourceKind  string              `json:"source_kind"`
		Correlation string              `json:"correlation"`
		From        string              `json:"from"`
		To          string              `json:"to"`
		Denials     AuditDenialMode     `json:"denials"`
	}{
		Version: AuditReadSchemaVersion, MachineID: request.MachineID, Actions: request.Actions,
		Outcome: request.Outcome, Principal: request.Principal, Capability: request.Capability,
		SourceKind: request.SourceKind, Correlation: request.Correlation,
		From: auditCursorTime(request.From), To: auditCursorTime(request.To), Denials: request.Denials,
	}
	raw, _ := json.Marshal(body)
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func auditCursorTime(value *time.Time) string {
	if value == nil {
		return ""
	}
	return value.UTC().Format(time.RFC3339)
}

func encodeAuditListCursor(cursor auditListCursor) (string, error) {
	raw, err := json.Marshal(cursor)
	if err != nil {
		return "", fmt.Errorf("operator: encode audit cursor: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func decodeAuditListCursor(encoded, filterDigest string) (auditListCursor, error) {
	if len(encoded) == 0 || len(encoded) > 2048 {
		return auditListCursor{}, fmt.Errorf("%w: cursor length is invalid", ErrInvalidAuditRead)
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || base64.RawURLEncoding.EncodeToString(raw) != encoded {
		return auditListCursor{}, fmt.Errorf("%w: cursor encoding is invalid", ErrInvalidAuditRead)
	}
	var cursor auditListCursor
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cursor); err != nil {
		return auditListCursor{}, fmt.Errorf("%w: cursor document is invalid", ErrInvalidAuditRead)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return auditListCursor{}, fmt.Errorf("%w: cursor has trailing JSON", ErrInvalidAuditRead)
	}
	canonical, err := encodeAuditListCursor(cursor)
	if err != nil || canonical != encoded || cursor.Version != AuditReadSchemaVersion ||
		cursor.FilterDigest != filterDigest || cursor.CreationCeiling < 1 ||
		cursor.AfterAuditID < 1 || cursor.AfterAuditID > cursor.CreationCeiling {
		return auditListCursor{}, fmt.Errorf("%w: cursor does not match this query", ErrInvalidAuditRead)
	}
	return cursor, nil
}
