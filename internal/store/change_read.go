package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/teddashh/AI-Intune/internal/model"
)

// Change reads deliberately do not expose store.Change. That legacy type is a
// presentation sentence built from raw agent evidence. This boundary instead
// preserves the source coordinates and raw observation payloads for an
// operator-owned, explicitly safe projection.

var (
	ErrInvalidChangeRead       = errors.New("store: invalid change read request")
	ErrChangeReadTraversalGone = errors.New("store: change read traversal invalidated by retention")
	ErrChangeReadTooBroad      = errors.New("store: change read is too broad")
	ErrChangeReadBusy          = errors.New("store: change reader is busy")
)

const (
	MaxChangeReadEndpointKeys           = 10_000
	MaxChangeReadTransitionRows         = 10_000
	MaxChangeReadObservationWindowRows  = 250_000
	MaxChangeReadTimestampAuditRows     = 250_000
	MaxChangeReadCandidateMetadataBytes = 32 << 20
	MaxChangeReadEndpointPayloadBytes   = 32 << 20
	maxConcurrentChangeReads            = 2
)

type ChangeReadSource string

const (
	ChangeReadSourceRegistryCreated   ChangeReadSource = "registry_created"
	ChangeReadSourceRegistryLifecycle ChangeReadSource = "registry_lifecycle"
	ChangeReadSourceStateHistory      ChangeReadSource = "state_history"
	ChangeReadSourceObservation       ChangeReadSource = "observation"
)

const (
	ChangeReadKindRegistry  = "registry"
	ChangeReadKindState     = "state"
	ChangeReadSubjectState  = "state"
	ChangeReadSubjectLife   = "lifecycle"
	ChangeReadRegistered    = "registered"
	ChangeReadRetired       = "retired"
	changeReadMetaTracked   = "registry_lifecycle_tracked_from"
	changeReadMetaComplete  = "registry_lifecycle_history_complete"
	changeReadStateTracked  = "state_transition_tracked_from"
	changeReadStateComplete = "state_transition_history_complete"
	changeReadRetentionKind = "observed_state"
)

// ChangeReadSourceKey is unique within one SQLite ledger generation. Hidden
// rowids are not durable across VACUUM or restore, so a public cursor must be
// discarded after either operation rather than treating this as a global ID.
type ChangeReadSourceKey struct {
	Source   ChangeReadSource `json:"source"`
	Sequence int64            `json:"sequence"`
}

// ChangeReadCeilings freezes every append source used to compose a traversal.
// New source rows cannot enter a later page. RetentionLog is different: prune
// deletes rows below the other ceilings, so any change to that sequence makes
// a continuation stale and is rejected rather than returning a shifted page.
type ChangeReadCeilings struct {
	Registry          int64 `json:"registry"`
	RegistryLifecycle int64 `json:"registry_lifecycle"`
	StateHistory      int64 `json:"state_history"`
	ObservedState     int64 `json:"observed_state"`
	RetentionLog      int64 `json:"retention_log"`
}

type ChangeReadRequest struct {
	// The comparison window is (From, To]. Observation endpoint values are the
	// latest Hub-received rows at or before each endpoint.
	From time.Time
	To   time.Time

	// Exact filters are pushed into every applicable source query. Empty Kinds
	// means all canonical kinds; MachineID and Subject empty mean unfiltered.
	MachineID string
	Kinds     []string
	Subject   string

	// Nil starts a traversal and captures ceilings in the same read snapshot.
	// A continuation passes back the exact first-page value.
	Ceilings *ChangeReadCeilings
}

var changeReadCanonicalKinds = []string{
	ChangeReadKindRegistry,
	ChangeReadKindState,
	KindIdentity,
	KindCredential,
	KindCLITool,
	KindSystemd,
	KindOpenClaw,
}

type changeReadSelection struct {
	machineID           string
	subject             string
	kinds               map[string]bool
	observationKinds    []string
	windowRowLimit      int
	timestampAuditLimit int
	metadataByteLimit   int
	endpointKeyLimit    int
	payloadByteLimit    int
}

func newChangeReadSelection(request ChangeReadRequest) (changeReadSelection, error) {
	selection := changeReadSelection{
		machineID:           request.MachineID,
		subject:             request.Subject,
		kinds:               make(map[string]bool, len(changeReadCanonicalKinds)),
		windowRowLimit:      MaxChangeReadObservationWindowRows,
		timestampAuditLimit: MaxChangeReadTimestampAuditRows,
		metadataByteLimit:   MaxChangeReadCandidateMetadataBytes,
		endpointKeyLimit:    MaxChangeReadEndpointKeys,
		payloadByteLimit:    MaxChangeReadEndpointPayloadBytes,
	}
	if len(request.Kinds) == 0 {
		for _, kind := range changeReadCanonicalKinds {
			selection.kinds[kind] = true
		}
	} else {
		canonical := make(map[string]bool, len(changeReadCanonicalKinds))
		for _, kind := range changeReadCanonicalKinds {
			canonical[kind] = true
		}
		for _, kind := range request.Kinds {
			if !canonical[kind] {
				return changeReadSelection{}, fmt.Errorf("%w: unknown change kind %q", ErrInvalidChangeRead, kind)
			}
			if selection.kinds[kind] {
				return changeReadSelection{}, fmt.Errorf("%w: duplicate change kind %q", ErrInvalidChangeRead, kind)
			}
			selection.kinds[kind] = true
		}
	}
	for _, kind := range []string{KindIdentity, KindCredential, KindCLITool, KindSystemd, KindOpenClaw} {
		if selection.kinds[kind] {
			selection.observationKinds = append(selection.observationKinds, kind)
		}
	}
	return selection, nil
}

func (selection changeReadSelection) readsFixedSubject(kind, subject string) bool {
	return selection.kinds[kind] && (selection.subject == "" || selection.subject == subject)
}

// ChangeReadCoverage says what the ledger can prove, not what the configured
// retention policy intended to keep. ObservationPrunedBefore comes from the
// durable retention_log rows that actually deleted observed_state evidence.
type ChangeReadCoverage struct {
	Complete                         bool       `json:"complete"`
	ObservationHistoryPruned         bool       `json:"observation_history_pruned"`
	ObservationRowsPruned            int64      `json:"observation_rows_pruned"`
	ObservationPrunedBefore          *time.Time `json:"observation_pruned_before"`
	LastObservationPrunedAt          *time.Time `json:"last_observation_pruned_at"`
	RegistryLifecycleHistoryComplete bool       `json:"registry_lifecycle_history_complete"`
	RegistryLifecycleTrackedFrom     *time.Time `json:"registry_lifecycle_tracked_from"`
	StateTransitionHistoryComplete   bool       `json:"state_transition_history_complete"`
	StateTransitionTrackedFrom       *time.Time `json:"state_transition_tracked_from"`
	Issues                           []string   `json:"issues"`
}

// ChangeReadRecord is persistence evidence, not an HTTP DTO. FromPayload and
// ToPayload can contain account identifiers, host identity, paths, malformed
// JSON, and arbitrary agent text. json:"-" is a second line of defence: only
// an operator-owned allowlist may turn them into presentation data.
type ChangeReadRecord struct {
	Key         ChangeReadSourceKey `json:"key"`
	MachineID   string              `json:"machine_id"`
	DisplayName string              `json:"display_name"`
	Kind        string              `json:"kind"`
	Subject     string              `json:"subject"`

	From      string `json:"-"`
	To        string `json:"-"`
	FromKnown bool   `json:"from_known"`

	// HubAt is always a Hub-owned coordinate: registry created_at, lifecycle
	// occurred_at, state entered_at, or observation received_at.
	HubAt      time.Time  `json:"hub_at"`
	MeasuredAt *time.Time `json:"measured_at"`

	FromPayload string   `json:"-"`
	ToPayload   string   `json:"-"`
	Issues      []string `json:"issues"`
}

type ChangeReadResult struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`

	Ceilings ChangeReadCeilings `json:"ceilings"`
	Coverage ChangeReadCoverage `json:"coverage"`

	// MalformedTimestamps counts malformed Hub coordinates in the selected
	// sources below the frozen ceilings, plus malformed agent measured_at on an
	// endpoint actually used for comparison. UnplaceableTimestamps is the Hub
	// coordinate subset, whose row cannot honestly be put in or out of the
	// requested window. A malformed endpoint measured_at remains placeable by
	// received_at and is retained with an item issue.
	MalformedTimestamps   int `json:"malformed_timestamps"`
	UnplaceableTimestamps int `json:"unplaceable_timestamps"`

	Records []ChangeReadRecord `json:"records"`
}

func (s *Store) ReadChanges(request ChangeReadRequest) (ChangeReadResult, error) {
	return s.ReadChangesContext(context.Background(), request)
}

func (s *Store) acquireChangeRead() bool {
	select {
	case s.changeReadSlots <- struct{}{}:
		return true
	default:
		return false
	}
}

func (s *Store) releaseChangeRead() {
	<-s.changeReadSlots
}

func (s *Store) ReadChangesContext(ctx context.Context, request ChangeReadRequest) (ChangeReadResult, error) {
	if ctx == nil {
		return ChangeReadResult{}, fmt.Errorf("%w: context is required", ErrInvalidChangeRead)
	}
	request.From = request.From.UTC()
	request.To = request.To.UTC()
	if request.From.IsZero() || request.To.IsZero() {
		return ChangeReadResult{}, fmt.Errorf("%w: from and to are required", ErrInvalidChangeRead)
	}
	if request.From.Nanosecond() != 0 || request.To.Nanosecond() != 0 {
		return ChangeReadResult{}, fmt.Errorf("%w: from and to must use second precision", ErrInvalidChangeRead)
	}
	if !request.From.Before(request.To) {
		return ChangeReadResult{}, fmt.Errorf("%w: from must be before to", ErrInvalidChangeRead)
	}
	if request.Ceilings != nil && !validChangeReadCeilings(*request.Ceilings) {
		return ChangeReadResult{}, fmt.Errorf("%w: source ceiling is negative", ErrInvalidChangeRead)
	}
	selection, err := newChangeReadSelection(request)
	if err != nil {
		return ChangeReadResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return ChangeReadResult{}, err
	}
	if !s.acquireChangeRead() {
		return ChangeReadResult{}, ErrChangeReadBusy
	}
	defer s.releaseChangeRead()

	tx, err := s.rdb.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return ChangeReadResult{}, fmt.Errorf("store: begin change read: %w", err)
	}
	defer tx.Rollback()

	current, err := currentChangeReadCeilings(ctx, tx)
	if err != nil {
		return ChangeReadResult{}, err
	}
	ceilings := current
	if request.Ceilings != nil {
		ceilings = *request.Ceilings
		if ceilings.Registry > current.Registry ||
			ceilings.RegistryLifecycle > current.RegistryLifecycle ||
			ceilings.StateHistory > current.StateHistory ||
			ceilings.ObservedState > current.ObservedState ||
			ceilings.RetentionLog > current.RetentionLog {
			return ChangeReadResult{}, fmt.Errorf("%w: source ceiling is outside this ledger", ErrInvalidChangeRead)
		}
		if ceilings.RetentionLog != current.RetentionLog {
			return ChangeReadResult{}, ErrChangeReadTraversalGone
		}
	}
	result := ChangeReadResult{
		From: request.From, To: request.To, Ceilings: ceilings,
		Records: make([]ChangeReadRecord, 0),
	}
	readRegistry := selection.readsFixedSubject(ChangeReadKindRegistry, ChangeReadSubjectLife)
	readState := selection.readsFixedSubject(ChangeReadKindState, ChangeReadSubjectState)
	readObservations := len(selection.observationKinds) > 0
	coverage, malformed, err := readChangeCoverage(ctx, tx, request.From, ceilings.RetentionLog, selection)
	if err != nil {
		return ChangeReadResult{}, err
	}
	result.Coverage = coverage
	result.MalformedTimestamps += malformed

	remainingTransitions := MaxChangeReadTransitionRows
	appendTransitions := func(records []ChangeReadRecord, malformed, unplaceable int) {
		result.Records = append(result.Records, records...)
		result.MalformedTimestamps += malformed
		result.UnplaceableTimestamps += unplaceable
		remainingTransitions -= len(records)
	}
	var records []ChangeReadRecord
	var unplaceable int
	if readRegistry {
		records, malformed, unplaceable, err = readRegistryCreationChanges(
			ctx, tx, request.From, request.To, ceilings.Registry, selection.machineID,
			selection.timestampAuditLimit, remainingTransitions)
		if err != nil {
			return ChangeReadResult{}, err
		}
		appendTransitions(records, malformed, unplaceable)

		records, malformed, unplaceable, err = readRegistryLifecycleChanges(
			ctx, tx, request.From, request.To, ceilings.RegistryLifecycle,
			selection.machineID, selection.timestampAuditLimit, remainingTransitions)
		if err != nil {
			return ChangeReadResult{}, err
		}
		appendTransitions(records, malformed, unplaceable)
	}
	if readState {
		records, malformed, unplaceable, err = readStateChanges(
			ctx, tx, request.From, request.To, ceilings.StateHistory,
			selection.machineID, selection.timestampAuditLimit, remainingTransitions)
		if err != nil {
			return ChangeReadResult{}, err
		}
		appendTransitions(records, malformed, unplaceable)
	}

	if readObservations {
		records, malformed, unplaceable, err = readObservationEndpointChanges(
			ctx, tx, request.From, request.To, ceilings.ObservedState,
			result.Coverage.ObservationHistoryPruned, selection)
		if err != nil {
			return ChangeReadResult{}, err
		}
		result.Records = append(result.Records, records...)
		result.MalformedTimestamps += malformed
		result.UnplaceableTimestamps += unplaceable
	}
	if err := populateChangeReadDisplayNames(ctx, tx, ceilings.Registry, result.Records); err != nil {
		return ChangeReadResult{}, err
	}

	if result.UnplaceableTimestamps > 0 {
		result.Coverage.Complete = false
		addChangeCoverageIssue(&result.Coverage, "unplaceable_timestamps")
	}
	if result.MalformedTimestamps > result.UnplaceableTimestamps {
		addChangeCoverageIssue(&result.Coverage, "malformed_evidence_timestamps")
	}
	if result.Coverage.Issues == nil {
		result.Coverage.Issues = []string{}
	}

	sort.Slice(result.Records, func(i, j int) bool {
		left, right := result.Records[i], result.Records[j]
		if !left.HubAt.Equal(right.HubAt) {
			return left.HubAt.After(right.HubAt)
		}
		if left.Key.Source != right.Key.Source {
			return changeReadSourceRank(left.Key.Source) < changeReadSourceRank(right.Key.Source)
		}
		if left.Key.Sequence != right.Key.Sequence {
			return left.Key.Sequence > right.Key.Sequence
		}
		if left.MachineID != right.MachineID {
			return left.MachineID < right.MachineID
		}
		if left.Kind != right.Kind {
			return left.Kind < right.Kind
		}
		return left.Subject < right.Subject
	})
	for i := range result.Records {
		if result.Records[i].Issues == nil {
			result.Records[i].Issues = []string{}
		}
	}

	if err := tx.Commit(); err != nil {
		return ChangeReadResult{}, fmt.Errorf("store: finish change read: %w", err)
	}
	return result, nil
}

func validChangeReadCeilings(ceilings ChangeReadCeilings) bool {
	return ceilings.Registry >= 0 && ceilings.RegistryLifecycle >= 0 &&
		ceilings.StateHistory >= 0 && ceilings.ObservedState >= 0 &&
		ceilings.RetentionLog >= 0
}

func currentChangeReadCeilings(ctx context.Context, tx dbTx) (ChangeReadCeilings, error) {
	var result ChangeReadCeilings
	queries := []struct {
		label string
		query string
		value *int64
	}{
		{"registry", `SELECT COALESCE(MAX(rowid),0) FROM machine_registry`, &result.Registry},
		{"registry lifecycle", `SELECT COALESCE(MAX(event_id),0) FROM machine_registry_lifecycle_events`, &result.RegistryLifecycle},
		{"state history", `SELECT COALESCE(MAX(event_id),0) FROM machine_state_transition_events`, &result.StateHistory},
		{"observed state", `SELECT COALESCE(MAX(rowid),0) FROM observed_state`, &result.ObservedState},
		{"retention log", `SELECT COALESCE(MAX(prune_id),0) FROM retention_log`, &result.RetentionLog},
	}
	for _, item := range queries {
		if err := tx.QueryRowContext(ctx, item.query).Scan(item.value); err != nil {
			return ChangeReadCeilings{}, fmt.Errorf("store: read change %s ceiling: %w", item.label, err)
		}
	}
	return result, nil
}

func readChangeCoverage(ctx context.Context, tx dbTx, from time.Time, retentionCeiling int64, selection changeReadSelection) (ChangeReadCoverage, int, error) {
	// Completeness is scoped to the selected sources. A skipped source is true
	// (not applicable), so downstream projections do not manufacture an
	// unrelated partial warning for a credential-only or state-only query.
	coverage := ChangeReadCoverage{
		Complete: true, RegistryLifecycleHistoryComplete: true,
		StateTransitionHistoryComplete: true, Issues: []string{},
	}
	malformed := 0
	if selection.readsFixedSubject(ChangeReadKindRegistry, ChangeReadSubjectLife) {
		complete, tracked, invalid, issues, err := readTrackedChangeCoverage(
			ctx, tx, from, changeReadMetaTracked, changeReadMetaComplete, "registry lifecycle", "registry_lifecycle")
		if err != nil {
			return ChangeReadCoverage{}, 0, err
		}
		coverage.RegistryLifecycleHistoryComplete = complete
		coverage.RegistryLifecycleTrackedFrom = tracked
		malformed += invalid
		for _, issue := range issues {
			coverage.Complete = false
			addChangeCoverageIssue(&coverage, issue)
		}
	}
	if selection.readsFixedSubject(ChangeReadKindState, ChangeReadSubjectState) {
		complete, tracked, invalid, issues, err := readTrackedChangeCoverage(
			ctx, tx, from, changeReadStateTracked, changeReadStateComplete, "state transition", "state_transition")
		if err != nil {
			return ChangeReadCoverage{}, 0, err
		}
		coverage.StateTransitionHistoryComplete = complete
		coverage.StateTransitionTrackedFrom = tracked
		malformed += invalid
		for _, issue := range issues {
			coverage.Complete = false
			addChangeCoverageIssue(&coverage, issue)
		}
	}
	if len(selection.observationKinds) == 0 {
		return coverage, malformed, nil
	}

	rows, err := tx.QueryContext(ctx, `
SELECT at, older_than, rows_deleted
  FROM retention_log
 WHERE prune_id <= ? AND table_name = ? AND rows_deleted > 0
 ORDER BY prune_id`, retentionCeiling, changeReadRetentionKind)
	if err != nil {
		return ChangeReadCoverage{}, 0, fmt.Errorf("store: read observation retention coverage: %w", err)
	}
	defer rows.Close()
	var newestBoundary, lastPrunedAt time.Time
	for rows.Next() {
		coverage.ObservationHistoryPruned = true
		var rawAt, rawBoundary string
		var deleted int64
		if err := rows.Scan(&rawAt, &rawBoundary, &deleted); err != nil {
			return ChangeReadCoverage{}, 0, fmt.Errorf("store: scan observation retention coverage: %w", err)
		}
		coverage.ObservationRowsPruned += deleted
		if parsed, ok := parseChangeReadHubTime(rawAt); !ok {
			malformed++
			addChangeCoverageIssue(&coverage, "observation_retention_at_invalid")
		} else if lastPrunedAt.IsZero() || parsed.After(lastPrunedAt) {
			lastPrunedAt = parsed
		}
		if parsed, ok := parseChangeReadHubTime(rawBoundary); !ok {
			malformed++
			addChangeCoverageIssue(&coverage, "observation_retention_boundary_invalid")
		} else if newestBoundary.IsZero() || parsed.After(newestBoundary) {
			newestBoundary = parsed
		}
	}
	if err := rows.Err(); err != nil {
		return ChangeReadCoverage{}, 0, fmt.Errorf("store: finish observation retention coverage: %w", err)
	}
	if !newestBoundary.IsZero() {
		coverage.ObservationPrunedBefore = &newestBoundary
	}
	if !lastPrunedAt.IsZero() {
		coverage.LastObservationPrunedAt = &lastPrunedAt
	}
	if coverage.ObservationHistoryPruned {
		coverage.Complete = false
		addChangeCoverageIssue(&coverage, "observation_history_pruned")
	}
	return coverage, malformed, nil
}

func readTrackedChangeCoverage(ctx context.Context, tx dbTx, from time.Time, trackedKey, completeKey, label, issuePrefix string) (bool, *time.Time, int, []string, error) {
	var trackedRaw string
	issues := make([]string, 0, 2)
	malformed := 0
	var tracked *time.Time
	if err := tx.QueryRowContext(ctx, `SELECT value FROM schema_meta WHERE key=?`, trackedKey).Scan(&trackedRaw); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return false, nil, 0, nil, fmt.Errorf("store: read %s coverage start: %w", label, err)
		}
		issues = append(issues, issuePrefix+"_tracking_unknown")
	} else if parsed, ok := parseChangeReadHubTime(trackedRaw); !ok {
		malformed++
		issues = append(issues, issuePrefix+"_tracking_invalid")
	} else {
		tracked = &parsed
	}

	complete := false
	var completeRaw string
	if err := tx.QueryRowContext(ctx, `SELECT value FROM schema_meta WHERE key=?`, completeKey).Scan(&completeRaw); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return false, nil, 0, nil, fmt.Errorf("store: read %s coverage status: %w", label, err)
		}
		issues = append(issues, issuePrefix+"_history_unknown")
	} else {
		switch completeRaw {
		case "1":
			complete = true
		case "0":
			complete = tracked != nil && !from.Before(*tracked)
		default:
			issues = append(issues, issuePrefix+"_history_invalid")
		}
	}
	if !complete {
		issues = append(issues, issuePrefix+"_history_incomplete")
	}
	return complete, tracked, malformed, issues, nil
}

func addChangeCoverageIssue(coverage *ChangeReadCoverage, issue string) {
	for _, existing := range coverage.Issues {
		if existing == issue {
			return
		}
	}
	coverage.Issues = append(coverage.Issues, issue)
}

func populateChangeReadDisplayNames(ctx context.Context, tx dbTx, ceiling int64, records []ChangeReadRecord) error {
	const batchSize = 400 // safely below SQLite's conservative bind limit
	seen := make(map[string]bool, len(records))
	machineIDs := make([]string, 0, len(records))
	for _, record := range records {
		if record.MachineID != "" && !seen[record.MachineID] {
			seen[record.MachineID] = true
			machineIDs = append(machineIDs, record.MachineID)
		}
	}
	names := make(map[string]string, len(machineIDs))
	for offset := 0; offset < len(machineIDs); offset += batchSize {
		end := min(offset+batchSize, len(machineIDs))
		batch := machineIDs[offset:end]
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(batch)), ",")
		query := `SELECT machine_id,display_name FROM machine_registry
 WHERE rowid <= ? AND machine_id IN (` + placeholders + `)`
		args := make([]any, 0, len(batch)+1)
		args = append(args, ceiling)
		for _, machineID := range batch {
			args = append(args, machineID)
		}
		rows, err := tx.QueryContext(ctx, query, args...)
		if err != nil {
			return fmt.Errorf("store: read change display names: %w", err)
		}
		for rows.Next() {
			var machineID, displayName string
			if err := rows.Scan(&machineID, &displayName); err != nil {
				rows.Close()
				return fmt.Errorf("store: scan change display name: %w", err)
			}
			names[machineID] = displayName
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return fmt.Errorf("store: finish change display names: %w", err)
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("store: close change display names: %w", err)
		}
	}
	for i := range records {
		if displayName, ok := names[records[i].MachineID]; ok {
			records[i].DisplayName = displayName
		}
	}
	return nil
}

// auditMalformedChangeHubTimes deliberately reads only indexed metadata, never
// payload or free-text evidence. Malformed Hub coordinates cannot safely be
// constrained by an indexed time window, so every matching row below the
// frozen ceiling is counted rather than silently disappearing from coverage.
func auditMalformedChangeHubTimes(ctx context.Context, tx dbTx, query string, args []any, label string, maxRows int) (int, map[string]bool, error) {
	query += ` LIMIT ?`
	args = append(args, maxRows+1)
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return 0, nil, fmt.Errorf("store: audit %s timestamps: %w", label, err)
	}
	defer rows.Close()
	malformed := 0
	invalidMachines := make(map[string]bool)
	visited := 0
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return 0, nil, err
		}
		visited++
		if visited > maxRows {
			return 0, nil, changeReadTooBroad("timestamp audit rows", maxRows)
		}
		var machineID, rawAt string
		if err := rows.Scan(&machineID, &rawAt); err != nil {
			return 0, nil, fmt.Errorf("store: scan %s timestamp: %w", label, err)
		}
		if _, ok := parseChangeReadHubTime(rawAt); !ok {
			malformed++
			invalidMachines[machineID] = true
		}
	}
	if err := rows.Err(); err != nil {
		return 0, nil, fmt.Errorf("store: finish %s timestamp audit: %w", label, err)
	}
	return malformed, invalidMachines, nil
}

func readRegistryCreationChanges(ctx context.Context, tx dbTx, from, to time.Time, ceiling int64, machineFilter string, auditLimit, maxRows int) ([]ChangeReadRecord, int, int, error) {
	auditQuery := `SELECT machine_id,created_at FROM machine_registry`
	if machineFilter == "" {
		auditQuery += ` INDEXED BY ix_registry_change_read`
	}
	auditQuery += ` WHERE rowid <= ?`
	auditArgs := []any{ceiling}
	if machineFilter != "" {
		auditQuery += ` AND machine_id = ?`
		auditArgs = append(auditArgs, machineFilter)
	}
	malformed, _, err := auditMalformedChangeHubTimes(ctx, tx, auditQuery, auditArgs, "registry created_at", auditLimit)
	if err != nil {
		return nil, 0, 0, err
	}

	query := `
SELECT rowid,machine_id,display_name,created_at
  FROM machine_registry`
	if machineFilter == "" {
		query += ` INDEXED BY ix_registry_change_read`
	}
	query += `
 WHERE rowid <= ? AND created_at > ? AND created_at <= ?
   AND ` + changeReadCanonicalTimeSQL("created_at")
	args := []any{ceiling, fmtTime(from), fmtTime(to)}
	if machineFilter != "" {
		query += ` AND machine_id = ?`
		args = append(args, machineFilter)
	}
	query += ` ORDER BY created_at DESC,machine_id LIMIT ?`
	args = append(args, maxRows+1)
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("store: read registry creation changes: %w", err)
	}
	defer rows.Close()
	records := make([]ChangeReadRecord, 0)
	for rows.Next() {
		var sequence int64
		var machineID, displayName, rawAt string
		if err := rows.Scan(&sequence, &machineID, &displayName, &rawAt); err != nil {
			return nil, 0, 0, fmt.Errorf("store: scan registry creation change: %w", err)
		}
		at, ok := parseChangeReadHubTime(rawAt)
		if !ok {
			continue // Already counted by the metadata audit above.
		}
		if len(records) >= maxRows {
			return nil, 0, 0, changeReadTooBroad("transition rows", MaxChangeReadTransitionRows)
		}
		records = append(records, ChangeReadRecord{
			Key:       ChangeReadSourceKey{Source: ChangeReadSourceRegistryCreated, Sequence: sequence},
			MachineID: machineID, DisplayName: displayName,
			Kind: ChangeReadKindRegistry, Subject: ChangeReadSubjectLife,
			To: ChangeReadRegistered, FromKnown: false, HubAt: at, Issues: []string{},
		})
	}
	if err := rows.Err(); err != nil {
		return nil, 0, 0, fmt.Errorf("store: finish registry creation changes: %w", err)
	}
	return records, malformed, malformed, nil
}

func readRegistryLifecycleChanges(ctx context.Context, tx dbTx, from, to time.Time, ceiling int64, machineFilter string, auditLimit, maxRows int) ([]ChangeReadRecord, int, int, error) {
	auditQuery := `SELECT machine_id,occurred_at FROM machine_registry_lifecycle_events INDEXED BY `
	if machineFilter == "" {
		auditQuery += `ix_registry_lifecycle_read`
	} else {
		auditQuery += `ix_registry_lifecycle_change_machine`
	}
	auditQuery += ` WHERE event_id <= ?`
	auditArgs := []any{ceiling}
	if machineFilter != "" {
		auditQuery += ` AND machine_id = ?`
		auditArgs = append(auditArgs, machineFilter)
	}
	malformed, _, err := auditMalformedChangeHubTimes(ctx, tx, auditQuery, auditArgs, "registry lifecycle occurred_at", auditLimit)
	if err != nil {
		return nil, 0, 0, err
	}

	query := `
SELECT event_id,machine_id,event_type,occurred_at
  FROM machine_registry_lifecycle_events INDEXED BY `
	if machineFilter == "" {
		query += `ix_registry_lifecycle_read`
	} else {
		query += `ix_registry_lifecycle_change_machine`
	}
	query += `
 WHERE event_id <= ? AND occurred_at > ? AND occurred_at <= ?
   AND ` + changeReadCanonicalTimeSQL("occurred_at")
	args := []any{ceiling, fmtTime(from), fmtTime(to)}
	if machineFilter != "" {
		query += ` AND machine_id = ?`
		args = append(args, machineFilter)
	}
	query += ` ORDER BY occurred_at DESC,event_id DESC LIMIT ?`
	args = append(args, maxRows+1)
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("store: read registry lifecycle changes: %w", err)
	}
	defer rows.Close()
	records := make([]ChangeReadRecord, 0)
	for rows.Next() {
		var sequence int64
		var machineID, eventType, rawAt string
		if err := rows.Scan(&sequence, &machineID, &eventType, &rawAt); err != nil {
			return nil, 0, 0, fmt.Errorf("store: scan registry lifecycle change: %w", err)
		}
		at, ok := parseChangeReadHubTime(rawAt)
		if !ok {
			continue
		}
		if len(records) >= maxRows {
			return nil, 0, 0, changeReadTooBroad("transition rows", MaxChangeReadTransitionRows)
		}
		record := ChangeReadRecord{
			Key:       ChangeReadSourceKey{Source: ChangeReadSourceRegistryLifecycle, Sequence: sequence},
			MachineID: machineID,
			Kind:      ChangeReadKindRegistry, Subject: ChangeReadSubjectLife,
			FromKnown: true, HubAt: at, Issues: []string{},
		}
		switch eventType {
		case ChangeReadRetired:
			record.From, record.To = ChangeReadRegistered, ChangeReadRetired
		case ChangeReadRegistered:
			record.From, record.To = ChangeReadRetired, ChangeReadRegistered
		default:
			record.To = eventType
			record.FromKnown = false
			record.Issues = append(record.Issues, "unknown_registry_lifecycle_event")
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, 0, fmt.Errorf("store: finish registry lifecycle changes: %w", err)
	}
	return records, malformed, malformed, nil
}

func readStateChanges(ctx context.Context, tx dbTx, from, to time.Time, ceiling int64, machineFilter string, auditLimit, maxRows int) ([]ChangeReadRecord, int, int, error) {
	auditQuery := `SELECT machine_id,entered_at FROM machine_state_transition_events INDEXED BY `
	if machineFilter == "" {
		auditQuery += `ix_state_transition_change_read`
	} else {
		auditQuery += `ix_state_transition_change_machine`
	}
	auditQuery += ` WHERE event_id <= ?`
	auditArgs := []any{ceiling}
	if machineFilter != "" {
		auditQuery += ` AND machine_id = ?`
		auditArgs = append(auditArgs, machineFilter)
	}
	malformed, invalidMachines, err := auditMalformedChangeHubTimes(ctx, tx, auditQuery, auditArgs, "state entered_at", auditLimit)
	if err != nil {
		return nil, 0, 0, err
	}

	query := `SELECT event_id,machine_id,state,entered_at FROM machine_state_transition_events INDEXED BY `
	if machineFilter == "" {
		query += `ix_state_transition_change_read`
	} else {
		query += `ix_state_transition_change_machine`
	}
	query += `
 WHERE event_id <= ? AND entered_at > ? AND entered_at <= ?
   AND ` + changeReadCanonicalTimeSQL("entered_at")
	args := []any{ceiling, fmtTime(from), fmtTime(to)}
	if machineFilter != "" {
		query += ` AND machine_id = ?`
		args = append(args, machineFilter)
	}
	query += ` ORDER BY entered_at DESC,event_id DESC LIMIT ?`
	args = append(args, maxRows+1)
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("store: read state changes: %w", err)
	}
	defer rows.Close()
	records := make([]ChangeReadRecord, 0)
	for rows.Next() {
		var sequence int64
		var machineID, value, rawAt string
		if err := rows.Scan(&sequence, &machineID, &value, &rawAt); err != nil {
			return nil, 0, 0, fmt.Errorf("store: scan state change: %w", err)
		}
		at, ok := parseChangeReadHubTime(rawAt)
		if !ok {
			continue
		}
		if len(records) >= maxRows {
			return nil, 0, 0, changeReadTooBroad("transition rows", MaxChangeReadTransitionRows)
		}
		record := ChangeReadRecord{
			Key:       ChangeReadSourceKey{Source: ChangeReadSourceStateHistory, Sequence: sequence},
			MachineID: machineID,
			Kind:      ChangeReadKindState, Subject: ChangeReadSubjectState,
			To: value, HubAt: at, Issues: []string{},
		}
		if invalidMachines[machineID] {
			record.Issues = append(record.Issues, "state_history_has_unplaceable_timestamp")
		} else {
			var previous, rawPreviousAt string
			err := tx.QueryRowContext(ctx, `
SELECT state,entered_at
  FROM machine_state_transition_events INDEXED BY ix_state_transition_change_machine
 WHERE event_id <= ? AND machine_id = ?
   AND (entered_at < ? OR (entered_at = ? AND event_id < ?))
 ORDER BY entered_at DESC,event_id DESC
 LIMIT 1`, ceiling, machineID, rawAt, rawAt, sequence).Scan(&previous, &rawPreviousAt)
			switch {
			case errors.Is(err, sql.ErrNoRows):
			case err != nil:
				return nil, 0, 0, fmt.Errorf("store: read state predecessor: %w", err)
			default:
				if _, valid := parseChangeReadHubTime(rawPreviousAt); valid {
					record.From = previous
					record.FromKnown = true
				} else {
					record.Issues = append(record.Issues, "state_history_has_unplaceable_timestamp")
				}
			}
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, 0, fmt.Errorf("store: finish state changes: %w", err)
	}
	return records, malformed, malformed, nil
}

type changeReadObservationKey struct {
	machineID string
	kind      string
	subject   string
}

type changeReadObservation struct {
	sequence     int64
	payload      string
	receivedAt   time.Time
	measuredAt   *time.Time
	measuredBad  bool
	payloadValid bool
	fingerprint  string
}

type changeReadObservationEndpoints struct {
	before    changeReadObservation
	hasBefore bool
	after     changeReadObservation
	hasAfter  bool
}

func readObservationEndpointChanges(ctx context.Context, tx dbTx, from, to time.Time, ceiling int64, historyPruned bool, selection changeReadSelection) ([]ChangeReadRecord, int, int, error) {
	filterSQL, filterArgs := changeReadObservationFilterSQL(selection)
	indexName := changeReadObservationIndex(selection)
	auditArgs := append([]any{ceiling}, filterArgs...)
	malformed, _, err := auditMalformedChangeHubTimes(ctx, tx,
		`SELECT machine_id,received_at FROM observed_state INDEXED BY `+indexName+
			` WHERE rowid <= ?`+filterSQL,
		auditArgs, "observation received_at", selection.timestampAuditLimit)
	if err != nil {
		return nil, 0, 0, err
	}
	unplaceable := malformed

	query := `
SELECT rowid,machine_id,kind,subject,received_at
  FROM observed_state INDEXED BY ` + indexName + `
 WHERE rowid <= ?` + filterSQL + `
   AND received_at > ? AND received_at <= ?
   AND ` + changeReadCanonicalTimeSQL("received_at") + `
 LIMIT ?`
	args := append([]any{ceiling}, filterArgs...)
	args = append(args, fmtTime(from), fmtTime(to), selection.windowRowLimit+1)
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("store: read observation window candidates: %w", err)
	}
	endpoints := make(map[changeReadObservationKey]changeReadObservationEndpoints)
	windowRows := 0
	metadataBytes := int64(0)
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			rows.Close()
			return nil, 0, 0, err
		}
		windowRows++
		if windowRows > selection.windowRowLimit {
			rows.Close()
			return nil, 0, 0, changeReadTooBroad("observation window rows", selection.windowRowLimit)
		}
		var candidate changeReadObservation
		var key changeReadObservationKey
		var rawReceived string
		if err := rows.Scan(&candidate.sequence, &key.machineID, &key.kind, &key.subject, &rawReceived); err != nil {
			rows.Close()
			return nil, 0, 0, fmt.Errorf("store: scan observation window candidate: %w", err)
		}
		if err := reserveChangeReadBytes(&metadataBytes,
			int64(len(key.machineID)+len(key.kind)+len(key.subject)+len(rawReceived)),
			selection.metadataByteLimit, "observation candidate metadata bytes"); err != nil {
			rows.Close()
			return nil, 0, 0, err
		}
		received, ok := parseChangeReadHubTime(rawReceived)
		if !ok {
			continue // Counted by the metadata audit; never place it in the window.
		}
		candidate.receivedAt = received
		pair, exists := endpoints[key]
		if !exists && len(endpoints) >= selection.endpointKeyLimit {
			rows.Close()
			return nil, 0, 0, changeReadTooBroad("observation endpoint keys", selection.endpointKeyLimit)
		}
		if !pair.hasAfter || laterChangeObservation(candidate, pair.after) {
			pair.after, pair.hasAfter = candidate, true
		}
		endpoints[key] = pair
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, 0, 0, fmt.Errorf("store: finish observation window candidates: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, 0, 0, fmt.Errorf("store: close observation window candidates: %w", err)
	}

	keys := make([]changeReadObservationKey, 0, len(endpoints))
	for key := range endpoints {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].machineID != keys[j].machineID {
			return keys[i].machineID < keys[j].machineID
		}
		if keys[i].kind != keys[j].kind {
			return keys[i].kind < keys[j].kind
		}
		return keys[i].subject < keys[j].subject
	})

	payloadBytes := int64(0)
	records := make([]ChangeReadRecord, 0, len(keys))
	for _, key := range keys {
		if err := ctx.Err(); err != nil {
			return nil, 0, 0, err
		}
		pair := endpoints[key]
		pair.after, err = loadChangeReadObservationByRowID(
			ctx, tx, pair.after, &payloadBytes, selection.payloadByteLimit)
		if err != nil {
			return nil, 0, 0, err
		}
		if pair.after.measuredBad {
			malformed++
		}

		pair.before, pair.hasBefore, err = loadChangeReadObservationBaseline(
			ctx, tx, ceiling, from, key, &payloadBytes, selection.payloadByteLimit)
		if err != nil {
			return nil, 0, 0, err
		}
		if pair.hasBefore && pair.before.measuredBad {
			malformed++
		}

		pair.after.fingerprint, pair.after.payloadValid = changeReadObservationFingerprint(key.kind, pair.after.payload)
		if pair.hasBefore {
			pair.before.fingerprint, pair.before.payloadValid = changeReadObservationFingerprint(key.kind, pair.before.payload)
		}
		if pair.hasBefore && pair.before.fingerprint == pair.after.fingerprint {
			continue
		}
		record := ChangeReadRecord{
			Key:       ChangeReadSourceKey{Source: ChangeReadSourceObservation, Sequence: pair.after.sequence},
			MachineID: key.machineID, Kind: key.kind, Subject: key.subject,
			FromKnown: pair.hasBefore,
			HubAt:     pair.after.receivedAt, MeasuredAt: pair.after.measuredAt,
			ToPayload: pair.after.payload, Issues: []string{},
		}
		if pair.hasBefore {
			record.FromPayload = pair.before.payload
			if !pair.before.payloadValid {
				record.Issues = append(record.Issues, "from_payload_malformed")
			}
			if pair.before.measuredBad {
				record.Issues = append(record.Issues, "from_measured_at_invalid")
			}
		} else if historyPruned {
			record.Issues = append(record.Issues, "from_possibly_pruned")
		}
		if !pair.after.payloadValid {
			record.Issues = append(record.Issues, "to_payload_malformed")
		}
		if pair.after.measuredBad {
			record.Issues = append(record.Issues, "to_measured_at_invalid")
		}
		records = append(records, record)
	}
	return records, malformed, unplaceable, nil
}

func changeReadObservationFilterSQL(selection changeReadSelection) (string, []any) {
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(selection.observationKinds)), ",")
	query := ` AND kind IN (` + placeholders + `)`
	args := make([]any, 0, len(selection.observationKinds)+2)
	for _, kind := range selection.observationKinds {
		args = append(args, kind)
	}
	if selection.machineID != "" {
		query += ` AND machine_id = ?`
		args = append(args, selection.machineID)
	}
	if selection.subject != "" {
		query += ` AND subject = ?`
		args = append(args, selection.subject)
	}
	return query, args
}

func changeReadObservationIndex(selection changeReadSelection) string {
	if selection.machineID != "" && selection.subject != "" {
		return "ix_observed_prune"
	}
	if selection.machineID != "" {
		return "ix_observed_changes_machine"
	}
	if selection.subject != "" {
		return "ix_observed_changes_subject"
	}
	return "ix_observed_changes"
}

func loadChangeReadObservationByRowID(ctx context.Context, tx dbTx, observation changeReadObservation, payloadBytes *int64, payloadLimit int) (changeReadObservation, error) {
	var rawMeasured string
	var bytes int64
	if err := tx.QueryRowContext(ctx, `
SELECT length(CAST(payload AS BLOB)),measured_at
  FROM observed_state
 WHERE rowid = ?`, observation.sequence).Scan(&bytes, &rawMeasured); err != nil {
		return changeReadObservation{}, fmt.Errorf("store: read observation endpoint metadata: %w", err)
	}
	if err := reserveChangeReadPayload(payloadBytes, bytes, payloadLimit); err != nil {
		return changeReadObservation{}, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT payload FROM observed_state WHERE rowid = ?`, observation.sequence).
		Scan(&observation.payload); err != nil {
		return changeReadObservation{}, fmt.Errorf("store: read observation endpoint payload: %w", err)
	}
	setChangeReadMeasured(&observation, rawMeasured)
	return observation, nil
}

func loadChangeReadObservationBaseline(ctx context.Context, tx dbTx, ceiling int64, from time.Time, key changeReadObservationKey, payloadBytes *int64, payloadLimit int) (changeReadObservation, bool, error) {
	var result changeReadObservation
	var rawMeasured, rawReceived string
	var bytes int64
	var endpointAt sql.NullString
	if err := tx.QueryRowContext(ctx, `
SELECT MAX(received_at)
  FROM observed_state INDEXED BY ix_observed_prune
 WHERE rowid <= ? AND machine_id = ? AND kind = ? AND subject = ?
	   AND received_at <= ? AND `+changeReadCanonicalTimeSQL("received_at"),
		ceiling, key.machineID, key.kind, key.subject, fmtTime(from)).Scan(&endpointAt); err != nil {
		return changeReadObservation{}, false, fmt.Errorf("store: read observation baseline coordinate: %w", err)
	}
	if !endpointAt.Valid {
		return changeReadObservation{}, false, nil
	}
	if _, ok := parseChangeReadHubTime(endpointAt.String); !ok {
		return changeReadObservation{}, false, nil
	}
	err := tx.QueryRowContext(ctx, `
SELECT rowid,length(CAST(payload AS BLOB)),measured_at,received_at
  FROM observed_state INDEXED BY ix_observed_prune
 WHERE rowid <= ? AND machine_id = ? AND kind = ? AND subject = ?
   AND received_at = ?
 ORDER BY rowid DESC
 LIMIT 1`, ceiling, key.machineID, key.kind, key.subject, endpointAt.String).
		Scan(&result.sequence, &bytes, &rawMeasured, &rawReceived)
	if err != nil {
		return changeReadObservation{}, false, fmt.Errorf("store: read observation baseline metadata: %w", err)
	}
	var ok bool
	result.receivedAt, ok = parseChangeReadHubTime(rawReceived)
	if !ok {
		// The SQL predicate is only a syntactic fast path. The strict Go parser is
		// authoritative and must never turn malformed evidence into a coordinate.
		return changeReadObservation{}, false, nil
	}
	if err := reserveChangeReadPayload(payloadBytes, bytes, payloadLimit); err != nil {
		return changeReadObservation{}, false, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT payload FROM observed_state WHERE rowid = ?`, result.sequence).
		Scan(&result.payload); err != nil {
		return changeReadObservation{}, false, fmt.Errorf("store: read observation baseline payload: %w", err)
	}
	setChangeReadMeasured(&result, rawMeasured)
	return result, true, nil
}

func setChangeReadMeasured(observation *changeReadObservation, raw string) {
	measured := parseTime(raw)
	if measured.IsZero() {
		observation.measuredBad = true
		return
	}
	measured = measured.UTC()
	observation.measuredAt = &measured
}

func reserveChangeReadPayload(total *int64, bytes int64, limit int) error {
	return reserveChangeReadBytes(total, bytes, limit, "observation endpoint payload bytes")
}

func reserveChangeReadBytes(total *int64, bytes int64, limit int, dimension string) error {
	if bytes < 0 || limit < 0 || *total > int64(limit) || bytes > int64(limit)-*total {
		return changeReadTooBroad(dimension, limit)
	}
	*total += bytes
	return nil
}

func changeReadCanonicalTimeSQL(column string) string {
	return `length(` + column + `) = 20 AND ` + column +
		` GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z'`
}

func changeReadTooBroad(dimension string, limit int) error {
	return fmt.Errorf("%w: %s exceed %d; narrow machine_id, kind, subject, or time window",
		ErrChangeReadTooBroad, dimension, limit)
}

// The change reader has its own comparison projection. The legacy
// summarizeObservation function produces presentation sentences and
// intentionally omits fields that the operator API can now display (for
// example identity OS/arch and CLI reachability). Reusing it here would make
// real native field changes disappear. Conversely, hashing the whole payload
// would turn arbitrary notes, logs, OpenClaw run summaries and volatile PIDs
// into noisy operator changes.
type changeReadIdentityFingerprint struct {
	Hostname      string
	OS            string
	Kernel        string
	Arch          string
	UnixUser      string
	MachineIDHint string
	BootID        string
	TailscaleIP   string
	LingerEnabled bool
}

type changeReadCredentialFingerprint struct {
	Provider        string
	Status          model.CredStatus
	ExpiresAt       *time.Time
	ActiveAccountID string
	AccountCount    int
}

type changeReadCLIToolFingerprint struct {
	Name               string
	Present            bool
	OnPath             bool
	PresentEvidence    string
	Path               string
	RealPath           string
	PathSource         string
	DaemonReach        string
	DaemonPath         string
	VersionReported    string
	VersionPackageJSON string
	SourcesDisagree    bool
	RunningExe         string
	RunningScript      string
}

// ⚠ Measured 與 Reason 刻意不進指紋：前者會在 agent 升級時為每個 unit 製造假變更，
// 後者會因 stderr 原文波動而製造新事件。Present 的變化仍保留，交由投影層表達「沒有量到」。
type changeReadUnitFingerprint struct {
	Name                 string
	Present              bool
	ActiveState          string
	SubState             string
	ActiveEnterTimestamp *time.Time
	NRestarts            int
}

type changeReadOpenClawFingerprint struct {
	Present         bool
	CLIVersion      string
	GatewayVersion  string
	UpstreamVersion string
	Install         *changeReadOpenClawInstallFingerprint
	DB              *changeReadOpenClawDBFingerprint
}

type changeReadOpenClawInstallFingerprint struct {
	UnitFound          bool
	UnitPath           string
	DropInPaths        []string
	ExecStart          string
	KillMode           string
	NRestarts          *int
	ActiveEnterAt      *time.Time
	NodePath           string
	RunningDir         string
	GatewayArgs        []string
	RunningDirExists   bool
	RunningDirOwner    string
	RunningDirWritable *bool
	RunningDirVersion  string
	ProcessIndexJS     string
	ProcessMatchesUnit *bool
	NodeVersion        string
	NpmPath            string
	NpmVersion         string
	ReleasesDir        string
	ReleasesPresent    bool
	CurrentLink        string
}

type changeReadOpenClawDBFingerprint struct {
	Present bool
	Layout  string
	Path    string
	Support model.SupportLevel
	FoundAt []string
}

func changeReadObservationFingerprint(kind, payload string) (string, bool) {
	var selected any
	switch kind {
	case KindIdentity:
		identity, ok := unmarshalInto[model.Identity](payload)
		if !ok {
			return changeReadMalformedFingerprint(payload), false
		}
		selected = changeReadIdentityFingerprint{
			Hostname: identity.Hostname, OS: identity.OS, Kernel: identity.Kernel, Arch: identity.Arch,
			UnixUser: identity.UnixUser, MachineIDHint: identity.MachineIDHint,
			BootID: identity.BootID, TailscaleIP: identity.TailscaleIP,
			LingerEnabled: identity.LingerEnabled,
		}
	case KindCredential:
		credential, ok := unmarshalInto[model.Credential](payload)
		if !ok {
			return changeReadMalformedFingerprint(payload), false
		}
		selected = changeReadCredentialFingerprint{
			Provider: credential.Provider, Status: credential.Status,
			ExpiresAt:       changeReadUTCTime(credential.ExpiresAt),
			ActiveAccountID: credential.ActiveAccountID, AccountCount: credential.AccountCount,
		}
	case KindCLITool:
		tool, ok := unmarshalInto[model.CLITool](payload)
		if !ok {
			return changeReadMalformedFingerprint(payload), false
		}
		selected = changeReadCLIToolFingerprint{
			Name: tool.Name, Present: tool.Present, OnPath: tool.OnPath,
			PresentEvidence: tool.PresentEvidence, Path: tool.Path, RealPath: tool.RealPath,
			PathSource: tool.PathSource, DaemonReach: tool.DaemonReach, DaemonPath: tool.DaemonPath,
			VersionReported: tool.VersionReported, VersionPackageJSON: tool.VersionPackageJSON,
			SourcesDisagree: tool.SourcesDisagree, RunningExe: tool.RunningExe,
			RunningScript: tool.RunningScript,
		}
	case KindSystemd:
		unit, ok := unmarshalInto[model.Unit](payload)
		if !ok {
			return changeReadMalformedFingerprint(payload), false
		}
		selected = changeReadUnitFingerprint{
			Name: unit.Name, Present: unit.Present, ActiveState: unit.ActiveState,
			SubState: unit.SubState, ActiveEnterTimestamp: changeReadUTCTime(unit.ActiveEnterTimestamp),
			NRestarts: unit.NRestarts,
		}
	case KindOpenClaw:
		openclaw, ok := unmarshalInto[model.OpenClaw](payload)
		if !ok {
			return changeReadMalformedFingerprint(payload), false
		}
		fingerprint := changeReadOpenClawFingerprint{
			Present:    openclaw.Present,
			CLIVersion: openclaw.CLIVersion, GatewayVersion: openclaw.GatewayVersion,
			UpstreamVersion: openclaw.UpstreamVersion,
		}
		if install := openclaw.Install; install != nil {
			fingerprint.Install = &changeReadOpenClawInstallFingerprint{
				UnitFound: install.UnitFound, UnitPath: install.UnitPath,
				DropInPaths: sortedChangeReadStrings(install.DropInPaths),
				ExecStart:   install.ExecStart, KillMode: install.KillMode, NRestarts: install.NRestarts,
				ActiveEnterAt: changeReadUTCTime(install.ActiveEnterAt), NodePath: install.NodePath,
				RunningDir: install.RunningDir, GatewayArgs: append([]string(nil), install.GatewayArgs...),
				RunningDirExists: install.RunningDirExists, RunningDirOwner: install.RunningDirOwner,
				RunningDirWritable: install.RunningDirWritable, RunningDirVersion: install.RunningDirVersion,
				ProcessIndexJS: install.ProcessIndexJS, ProcessMatchesUnit: install.ProcessMatchesUnit,
				NodeVersion: install.NodeVersion, NpmPath: install.NpmPath, NpmVersion: install.NpmVersion,
				ReleasesDir: install.ReleasesDir, ReleasesPresent: install.ReleasesPresent,
				CurrentLink: install.CurrentLink,
			}
		}
		if database := openclaw.DB; database != nil {
			fingerprint.DB = &changeReadOpenClawDBFingerprint{
				Present: database.Present, Layout: database.Layout, Path: database.Path,
				Support: database.Support, FoundAt: sortedChangeReadStrings(database.FoundAt),
			}
		}
		selected = fingerprint
	default:
		return changeReadMalformedFingerprint(payload), false
	}

	canonical, err := json.Marshal(selected)
	if err != nil {
		// All selected fields above are JSON-safe. Keep the fallback bounded and
		// conservative in case a future model type violates that invariant.
		return changeReadMalformedFingerprint(payload), false
	}
	digest := sha256.Sum256(canonical)
	return "valid\x00" + string(digest[:]), true
}

func changeReadMalformedFingerprint(payload string) string {
	// Malformed payload remains observable and comparable without retaining a
	// second, potentially multi-megabyte copy in the comparison key. The raw
	// payload itself remains on the selected endpoint record.
	digest := sha256.Sum256([]byte(payload))
	return "malformed\x00" + string(digest[:])
}

func changeReadUTCTime(value *time.Time) *time.Time {
	if value == nil || value.IsZero() {
		return nil
	}
	utc := value.UTC()
	return &utc
}

func sortedChangeReadStrings(values []string) []string {
	if values == nil {
		return nil
	}
	result := append([]string(nil), values...)
	sort.Strings(result)
	return result
}

func laterChangeObservation(left, right changeReadObservation) bool {
	if !left.receivedAt.Equal(right.receivedAt) {
		return left.receivedAt.After(right.receivedAt)
	}
	return left.sequence > right.sequence
}

func inChangeReadWindow(at, from, to time.Time) bool {
	return at.After(from) && !at.After(to)
}

func parseChangeReadHubTime(raw string) (time.Time, bool) {
	parsed := parseTime(raw)
	if parsed.IsZero() || parsed.Nanosecond() != 0 || fmtTime(parsed) != raw {
		return time.Time{}, false
	}
	return parsed.UTC(), true
}

func changeReadSourceRank(source ChangeReadSource) int {
	switch source {
	case ChangeReadSourceStateHistory:
		return 0
	case ChangeReadSourceRegistryLifecycle:
		return 1
	case ChangeReadSourceRegistryCreated:
		return 2
	case ChangeReadSourceObservation:
		return 3
	default:
		return 4
	}
}

// setMachineLifecycle keeps the current registry projection and its
// append-only transition evidence in one writer transaction. Repeating the
// already-current value is a true no-op: it preserves the first transition
// timestamp and does not manufacture another event.
func (s *Store) setMachineLifecycle(machineID string, retired bool, now time.Time) error {
	operation := "unretire"
	if retired {
		operation = "retire"
	}
	tx, err := s.beginWrite(context.Background(), "set_machine_lifecycle")
	if err != nil {
		return fmt.Errorf("store: %s begin: %w", operation, err)
	}
	defer tx.Rollback()

	var rawCreated string
	var current sql.NullString
	var lifecycleRevision int64
	if err := tx.QueryRow(`SELECT created_at,retired_at,lifecycle_revision FROM machine_registry WHERE machine_id=?`, machineID).
		Scan(&rawCreated, &current, &lifecycleRevision); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return fmt.Errorf("store: %s read lifecycle: %w", operation, err)
	}
	if lifecycleRevision < 0 || lifecycleRevision >= MaxMachineLifecycleRevision {
		return ErrLifecycleRevisionLimit
	}
	if retired == current.Valid {
		return nil
	}
	if lifecycleRevision >= MaxMachineLifecycleRevision-1 {
		return ErrLifecycleRevisionLimit
	}
	now, err = canonicalMachineLifecycleTime(now)
	if err != nil {
		return fmt.Errorf("store: %s lifecycle time: %w", operation, err)
	}
	if err := validateMachineLifecycleTimeTx(tx, machineID, rawCreated, current, now); err != nil {
		return fmt.Errorf("store: %s lifecycle time: %w", operation, err)
	}

	eventType := ChangeReadRegistered
	var result sql.Result
	if retired {
		eventType = ChangeReadRetired
		result, err = tx.Exec(`UPDATE machine_registry SET retired_at=?,lifecycle_revision=lifecycle_revision+1
		 WHERE machine_id=? AND retired_at IS NULL AND lifecycle_revision=?`, fmtTime(now), machineID, lifecycleRevision)
	} else {
		result, err = tx.Exec(`UPDATE machine_registry SET retired_at=NULL,lifecycle_revision=lifecycle_revision+1
		 WHERE machine_id=? AND retired_at IS NOT NULL AND lifecycle_revision=?`, machineID, lifecycleRevision)
	}
	if err != nil {
		return fmt.Errorf("store: %s projection: %w", operation, err)
	}
	if changed, err := result.RowsAffected(); err != nil || changed != 1 {
		return fmt.Errorf("store: %s lifecycle changed concurrently", operation)
	}
	if retired {
		if _, err := closeOpenAgentSessionsForMachine(tx, machineID, now, AgentSessionCloseReasonMachineRetired); err != nil {
			return fmt.Errorf("store: close sessions after machine retirement: %w", err)
		}
	}
	if _, err := tx.Exec(`
INSERT INTO machine_registry_lifecycle_events(machine_id,event_type,occurred_at)
VALUES(?,?,?)`, machineID, eventType, fmtTime(now)); err != nil {
		return fmt.Errorf("store: %s lifecycle evidence: %w", operation, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: %s commit: %w", operation, err)
	}
	return nil
}

func canonicalMachineLifecycleTime(value time.Time) (time.Time, error) {
	if value.IsZero() {
		return time.Time{}, errors.New("time is required")
	}
	return value.UTC().Truncate(time.Second), nil
}

func validateMachineLifecycleTimeTx(tx dbTx, machineID, rawCreated string, currentRetired sql.NullString, at time.Time) error {
	created, ok := parseChangeReadHubTime(rawCreated)
	if !ok {
		return errors.New("created_at is invalid")
	}
	if at.Before(created) {
		return errors.New("time cannot be before created_at")
	}
	if currentRetired.Valid {
		retiredAt, ok := parseChangeReadHubTime(currentRetired.String)
		if !ok {
			return errors.New("retired_at is invalid")
		}
		if at.Before(retiredAt) {
			return errors.New("time cannot be before current retired_at")
		}
	}
	var rawLatest string
	err := tx.QueryRow(`
SELECT occurred_at
  FROM machine_registry_lifecycle_events
 WHERE machine_id = ?
 ORDER BY event_id DESC
 LIMIT 1`, machineID).Scan(&rawLatest)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read latest transition: %w", err)
	}
	latest, ok := parseChangeReadHubTime(rawLatest)
	if !ok {
		return errors.New("latest transition time is invalid")
	}
	if at.Before(latest) {
		return errors.New("time cannot be before latest transition")
	}
	return nil
}
