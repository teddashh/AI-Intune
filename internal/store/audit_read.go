package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"modernc.org/sqlite"
)

const MaxAuditReadPageSize = 100

var ErrInvalidAuditRead = errors.New("store: invalid audit read request")

type AuditOutcome string

const (
	AuditOutcomeOK     AuditOutcome = "ok"
	AuditOutcomeFailed AuditOutcome = "failed"
)

// allAuditActions is the read side of the audit contract: it decides which
// filters the console offers and which rows are labelled `unknown_action`.
//
// ⚠ Every AuditAction declared in audit.go belongs here. An action the Hub
// writes but this list omits still reaches the operator — flagged as evidence
// the product does not recognise. `verification-assign` shipped that way and
// every 派工 row in the live ledger carried `unknown_action` until it was
// added. TestEveryDeclaredAuditActionIsReadable keeps the two in step.
var allAuditActions = [...]AuditAction{
	AuditConnect,
	AuditRetire,
	AuditUnretire,
	AuditEnrollToken,
	AuditRevokeToken,
	AuditEnrollmentLimit,
	AuditDeploymentCreate,
	AuditDeploymentRetry,
	AuditDeploymentContinue,
	AuditDeploymentSkipFailedBatch,
	AuditDeploymentAbandon,
	AuditArtifactFetch,
	AuditCatalogManifest,
	AuditMachineProfile,
	AuditMachineProfileAssign,
	AuditMachineChannel,
	AuditMachineLifecycle,
	AuditMachineRename,
	AuditMachineNotes,
	AuditDiagnosticNoop,
	AuditDeviceSync,
	AuditTailnetPeerIgnore,
	AuditRetentionPrune,
	AuditRestoreDrill,
	AuditVerifierRegister,
	AuditVerifierRevoke,
	AuditVerificationAssign,
	AuditSettingPolicy,
	AuditSettingAssign,
	AuditCompliancePolicy,
	AuditComplianceAssign,
	AuditMaintenanceProfile,
	AuditMaintenanceDryRun,
	AuditMaintenanceCanary,
	AuditMaintenanceContinue,
	AuditMaintenanceAbandon,
	AuditOperatorDenied,
}

// AuditActions returns a copy so callers cannot mutate validation or cursor
// canonicalization process-wide.
func AuditActions() []AuditAction {
	return append([]AuditAction(nil), allAuditActions[:]...)
}

func AuditActionCount() int { return len(allAuditActions) }

func IsKnownAuditAction(action AuditAction) bool {
	for _, known := range allAuditActions {
		if action == known {
			return true
		}
	}
	return false
}

// AuditReadPosition is a durable writer-order coordinate. Audit timestamps are
// evidence fields and can be equal or malformed in a legacy row; audit_id is
// the only value that can page the append sequence without skipping a record.
type AuditReadPosition struct {
	AuditID int64
}

// AuditReadFilter is the typed persistence boundary for operator audit reads.
// Every string filter is exact-match. Principal deliberately matches either
// the authenticated stable subject or the legacy Tailscale owner field; the
// returned row still keeps those two evidence sources separate.
type AuditReadFilter struct {
	MachineID       string
	Actions         []AuditAction
	Outcome         AuditOutcome
	Principal       string
	Capability      string
	SourceKind      string
	Correlation     string
	From            *time.Time
	To              *time.Time
	Limit           int
	CreationCeiling *int64
	After           *AuditReadPosition

	// SampleOperatorDenials reserves the default console/API traversal for
	// domain actions. Zero means include every persisted denial. A caller can
	// still request all denials explicitly; the durable denial ring remains the
	// independent storage bound.
	SampleOperatorDenials int
}

// AuditReadRecord preserves malformed legacy evidence without converting an
// unknown outcome to false or an invalid timestamp to year one. Issues names
// are stable classification values consumed by the safe operator projection.
type AuditReadRecord struct {
	AuditID int64
	At      *time.Time
	Action  string
	Outcome *AuditOutcome

	MachineID      string
	Subject        string
	Reason         string
	IdempotencyKey string
	RequestDigest  string

	SourceAddr     string
	WhoNode        string
	WhoUser        string
	WhoUnavailable string
	UserAgent      string

	AuthSubject      string
	AuthNodeID       string
	AuthCapability   string
	AuthMethod       string
	AuthDecision     string
	BoundaryDecision string
	SourceKind       string
	Detail           string

	Issues []string
}

func (r AuditReadRecord) IsOperatorReplay() bool {
	return IsCanonicalOperatorAction(AuditAction(r.Action)) &&
		strings.HasPrefix(r.Detail, OperatorIdempotencyReplayPrefix)
}

func (r AuditReadRecord) IsOperatorTransportRejection() bool {
	return IsCanonicalOperatorAction(AuditAction(r.Action)) &&
		strings.HasPrefix(r.Detail, OperatorTransportRejectionPrefix)
}

type AuditReadPage struct {
	MatchedTotal            int
	Total                   int
	Succeeded               int
	Failed                  int
	UnknownOutcome          int
	OperatorDenialsTotal    int
	OperatorDenialsIncluded int
	Items                   []AuditReadRecord
	CreationCeiling         int64
	Next                    *AuditReadPosition
}

// ListAuditReads returns one transaction-consistent, creation-ceiling keyset
// page. New writes cannot move or duplicate domain rows in a cursor traversal.
// The optional denial sample is selected from the same ceiling on every page.
// Its durable ring remains bounded, so enough later denial writes can delete an
// older sampled denial between requests; the public consistency label states
// that exception rather than claiming a full database snapshot.
func (s *Store) ListAuditReads(filter AuditReadFilter) (AuditReadPage, error) {
	return s.ListAuditReadsContext(context.Background(), filter)
}

func (s *Store) ListAuditReadsContext(ctx context.Context, filter AuditReadFilter) (AuditReadPage, error) {
	if ctx == nil {
		return AuditReadPage{}, fmt.Errorf("%w: context is required", ErrInvalidAuditRead)
	}
	if err := validateAuditReadFilter(filter); err != nil {
		return AuditReadPage{}, err
	}
	tx, err := s.rdb.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return AuditReadPage{}, fmt.Errorf("store: begin audit list read: %w", err)
	}
	defer tx.Rollback()

	var currentCeiling int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(audit_id), 0) FROM audit_log`).Scan(&currentCeiling); err != nil {
		return AuditReadPage{}, fmt.Errorf("store: read audit creation ceiling: %w", err)
	}
	ceiling := currentCeiling
	if filter.CreationCeiling != nil {
		ceiling = *filter.CreationCeiling
		if ceiling < 0 || ceiling > currentCeiling {
			return AuditReadPage{}, fmt.Errorf("%w: creation ceiling is outside this ledger", ErrInvalidAuditRead)
		}
	}

	where, baseArgs := auditReadWhere(filter, ceiling)
	cte, cteArgs := auditReadCTE(where, baseArgs, filter.SampleOperatorDenials)
	countQuery := cte + `
SELECT
 (SELECT COUNT(*) FROM matching),
 (SELECT COUNT(*) FROM visible),
 COALESCE((SELECT SUM(CASE WHEN outcome='ok' THEN 1 ELSE 0 END) FROM visible),0),
 COALESCE((SELECT SUM(CASE WHEN outcome='failed' THEN 1 ELSE 0 END) FROM visible),0),
 COALESCE((SELECT SUM(CASE WHEN outcome NOT IN ('ok','failed') THEN 1 ELSE 0 END) FROM visible),0),
 (SELECT COUNT(*) FROM matching WHERE action=?),
 (SELECT COUNT(*) FROM visible WHERE action=?)`
	countArgs := append(append([]any{}, cteArgs...), string(AuditOperatorDenied), string(AuditOperatorDenied))
	page := AuditReadPage{CreationCeiling: ceiling}
	if err := tx.QueryRowContext(ctx, countQuery, countArgs...).Scan(
		&page.MatchedTotal, &page.Total, &page.Succeeded, &page.Failed,
		&page.UnknownOutcome, &page.OperatorDenialsTotal, &page.OperatorDenialsIncluded,
	); err != nil {
		return AuditReadPage{}, fmt.Errorf("store: count filtered audit rows: %w", err)
	}

	itemQuery := cte + `
SELECT audit_id, at, action, COALESCE(machine_id,''), subject, COALESCE(reason,''),
       COALESCE(idempotency_key,''), COALESCE(request_digest,''),
       source_addr, COALESCE(who_node,''), COALESCE(who_user,''),
       COALESCE(who_unavailable,''), COALESCE(user_agent,''),
       COALESCE(auth_subject,''), COALESCE(auth_node_id,''), COALESCE(auth_capability,''),
       COALESCE(auth_method,''), COALESCE(auth_decision,''), COALESCE(boundary_decision,''),
       COALESCE(source_kind,''), outcome, COALESCE(detail,'')
  FROM visible`
	itemArgs := append([]any{}, cteArgs...)
	if filter.After != nil {
		itemQuery += ` WHERE audit_id < ?`
		itemArgs = append(itemArgs, filter.After.AuditID)
	}
	itemQuery += ` ORDER BY audit_id DESC LIMIT ?`
	itemArgs = append(itemArgs, filter.Limit+1)
	rows, err := tx.QueryContext(ctx, itemQuery, itemArgs...)
	if err != nil {
		return AuditReadPage{}, fmt.Errorf("store: list filtered audit rows: %w", err)
	}
	items := make([]AuditReadRecord, 0, filter.Limit+1)
	for rows.Next() {
		record, err := scanAuditReadRecord(rows)
		if err != nil {
			_ = rows.Close()
			return AuditReadPage{}, err
		}
		items = append(items, record)
	}
	if err := rows.Close(); err != nil {
		return AuditReadPage{}, fmt.Errorf("store: close filtered audit rows: %w", err)
	}
	if err := rows.Err(); err != nil {
		return AuditReadPage{}, fmt.Errorf("store: finish filtered audit rows: %w", err)
	}
	if len(items) > filter.Limit {
		items = items[:filter.Limit]
		page.Next = &AuditReadPosition{AuditID: items[len(items)-1].AuditID}
	}
	page.Items = items
	if err := tx.Commit(); err != nil {
		return AuditReadPage{}, fmt.Errorf("store: commit audit list read: %w", err)
	}
	return page, nil
}

func validateAuditReadFilter(filter AuditReadFilter) error {
	if filter.Limit < 1 || filter.Limit > MaxAuditReadPageSize {
		return fmt.Errorf("%w: limit must be between 1 and %d", ErrInvalidAuditRead, MaxAuditReadPageSize)
	}
	if filter.SampleOperatorDenials < 0 || filter.SampleOperatorDenials > OperatorDenialAuditLimit {
		return fmt.Errorf("%w: denial sample is outside the durable ring", ErrInvalidAuditRead)
	}
	for name, field := range map[string]struct {
		value string
		max   int
	}{
		"machine_id":  {filter.MachineID, 256},
		"principal":   {filter.Principal, 512},
		"capability":  {filter.Capability, 512},
		"source_kind": {filter.SourceKind, 128},
		"correlation": {filter.Correlation, 256},
	} {
		if field.value != "" && !validAuditReadText(field.value, field.max) {
			return fmt.Errorf("%w: %s is not a canonical exact-match value", ErrInvalidAuditRead, name)
		}
	}
	if filter.Outcome != "" && filter.Outcome != AuditOutcomeOK && filter.Outcome != AuditOutcomeFailed {
		return fmt.Errorf("%w: outcome is not canonical", ErrInvalidAuditRead)
	}
	seenActions := make(map[AuditAction]bool, len(filter.Actions))
	for _, action := range filter.Actions {
		if !IsKnownAuditAction(action) || seenActions[action] {
			return fmt.Errorf("%w: unknown or duplicate action %q", ErrInvalidAuditRead, action)
		}
		seenActions[action] = true
	}
	for name, value := range map[string]*time.Time{"from": filter.From, "to": filter.To} {
		if value != nil && (value.IsZero() || value.Nanosecond() != 0) {
			return fmt.Errorf("%w: %s must use non-zero second precision", ErrInvalidAuditRead, name)
		}
	}
	if filter.From != nil && filter.To != nil && filter.From.After(*filter.To) {
		return fmt.Errorf("%w: from must not be after to", ErrInvalidAuditRead)
	}
	if filter.CreationCeiling != nil && *filter.CreationCeiling < 0 {
		return fmt.Errorf("%w: creation ceiling must be non-negative", ErrInvalidAuditRead)
	}
	if filter.After != nil {
		if filter.CreationCeiling == nil || filter.After.AuditID <= 0 ||
			filter.After.AuditID > *filter.CreationCeiling {
			return fmt.Errorf("%w: incomplete continuation position", ErrInvalidAuditRead)
		}
	}
	return nil
}

func validAuditReadText(value string, maxBytes int) bool {
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

func auditReadWhere(filter AuditReadFilter, ceiling int64) (string, []any) {
	clauses := []string{"a.audit_id <= ?"}
	args := []any{ceiling}
	if filter.MachineID != "" {
		clauses = append(clauses, "a.machine_id = ?")
		args = append(args, filter.MachineID)
	}
	if len(filter.Actions) > 0 {
		placeholders := make([]string, len(filter.Actions))
		for i, action := range filter.Actions {
			placeholders[i] = "?"
			args = append(args, string(action))
		}
		clauses = append(clauses, "a.action IN ("+strings.Join(placeholders, ",")+")")
	}
	if filter.Outcome != "" {
		clauses = append(clauses, "a.outcome = ?")
		args = append(args, string(filter.Outcome))
	}
	if filter.Principal != "" {
		clauses = append(clauses, "(a.auth_subject = ? OR a.who_user = ?)")
		args = append(args, filter.Principal, filter.Principal)
	}
	if filter.Capability != "" {
		clauses = append(clauses, "a.auth_capability = ?")
		args = append(args, filter.Capability)
	}
	if filter.SourceKind != "" {
		clauses = append(clauses, "a.source_kind = ?")
		args = append(args, filter.SourceKind)
	}
	if filter.Correlation != "" {
		clauses = append(clauses, "(a.idempotency_key = ? OR a.request_digest = ?)")
		args = append(args, filter.Correlation, filter.Correlation)
	}
	if filter.From != nil {
		clauses = append(clauses, "clawctl_audit_unix_seconds(a.at) >= ?")
		args = append(args, filter.From.UTC().Unix())
	}
	if filter.To != nil {
		// Request windows have second precision. A fractional ledger instant in
		// the same second is later than the exact inclusive endpoint.
		clauses = append(clauses, `(clawctl_audit_unix_seconds(a.at) < ? OR
 (clawctl_audit_unix_seconds(a.at) = ? AND clawctl_audit_nanosecond(a.at) = 0))`)
		args = append(args, filter.To.UTC().Unix(), filter.To.UTC().Unix())
	}
	return strings.Join(clauses, " AND "), args
}

func auditReadCTE(where string, baseArgs []any, denialSample int) (string, []any) {
	args := append([]any{}, baseArgs...)
	if denialSample == 0 {
		return `WITH matching AS (
 SELECT * FROM audit_log a WHERE ` + where + `
), visible AS (
 SELECT * FROM matching
)`, args
	}
	args = append(args, string(AuditOperatorDenied), denialSample, string(AuditOperatorDenied))
	return `WITH matching AS (
 SELECT * FROM audit_log a WHERE ` + where + `
), denial_ids AS (
 SELECT audit_id FROM matching WHERE action=? ORDER BY audit_id DESC LIMIT ?
), visible AS (
 SELECT * FROM matching
  WHERE action<>? OR audit_id IN (SELECT audit_id FROM denial_ids)
)`, args
}

type auditReadScanner interface{ Scan(...any) error }

func scanAuditReadRecord(row auditReadScanner) (AuditReadRecord, error) {
	var record AuditReadRecord
	var rawAt, outcome string
	if err := row.Scan(
		&record.AuditID, &rawAt, &record.Action, &record.MachineID, &record.Subject, &record.Reason,
		&record.IdempotencyKey, &record.RequestDigest,
		&record.SourceAddr, &record.WhoNode, &record.WhoUser, &record.WhoUnavailable, &record.UserAgent,
		&record.AuthSubject, &record.AuthNodeID, &record.AuthCapability, &record.AuthMethod,
		&record.AuthDecision, &record.BoundaryDecision, &record.SourceKind, &outcome, &record.Detail,
	); err != nil {
		return AuditReadRecord{}, fmt.Errorf("store: scan audit read row: %w", err)
	}
	if record.AuditID <= 0 {
		return AuditReadRecord{}, fmt.Errorf("store: audit read has invalid writer sequence %d", record.AuditID)
	}
	if parsed := parseTime(rawAt); parsed.IsZero() {
		record.Issues = append(record.Issues, "invalid_at")
	} else {
		parsed = parsed.UTC()
		record.At = &parsed
	}
	if !IsKnownAuditAction(AuditAction(record.Action)) {
		record.Issues = append(record.Issues, "unknown_action")
	}
	switch AuditOutcome(outcome) {
	case AuditOutcomeOK, AuditOutcomeFailed:
		parsed := AuditOutcome(outcome)
		record.Outcome = &parsed
	default:
		record.Issues = append(record.Issues, "unknown_outcome")
	}
	if record.Subject == "" {
		record.Issues = append(record.Issues, "empty_subject")
	}
	if record.SourceAddr == "" {
		record.Issues = append(record.Issues, "empty_source_addr")
	}
	if record.Issues == nil {
		record.Issues = []string{}
	}
	return record, nil
}

var _ auditReadScanner = (*sql.Rows)(nil)

func init() {
	sqlite.MustRegisterDeterministicScalarFunction("clawctl_audit_unix_seconds", 1, auditReadUnixSeconds)
	sqlite.MustRegisterDeterministicScalarFunction("clawctl_audit_nanosecond", 1, auditReadNanosecond)
}

func auditReadUnixSeconds(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
	parsed, ok := auditReadSQLTime(args)
	if !ok {
		return nil, nil
	}
	return parsed.Unix(), nil
}

func auditReadNanosecond(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
	parsed, ok := auditReadSQLTime(args)
	if !ok {
		return nil, nil
	}
	return int64(parsed.Nanosecond()), nil
}

func auditReadSQLTime(args []driver.Value) (time.Time, bool) {
	if len(args) != 1 {
		return time.Time{}, false
	}
	var raw string
	switch value := args[0].(type) {
	case string:
		raw = value
	case []byte:
		raw = string(value)
	default:
		return time.Time{}, false
	}
	parsed := parseTime(raw)
	return parsed, !parsed.IsZero()
}
