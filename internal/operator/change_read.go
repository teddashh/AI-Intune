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
	"reflect"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/teddashh/AI-Intune/internal/model"
	"github.com/teddashh/AI-Intune/internal/state"
	"github.com/teddashh/AI-Intune/internal/store"
)

const (
	// ⚠ 公開 change DTO 的欄位或欄位語意改變時必須升版。cursor.Version 綁定同一個常數，
	// 所以升版也會刻意拒絕舊 cursor，要求用戶重新查詢。
	ChangeReadSchemaVersion = 2
	// A cursor freezes all source rowid ceilings and the comparison window. It
	// is deliberately not called a snapshot: retention may remove an old row,
	// and restoring or vacuuming a ledger invalidates rowid continuity.
	ChangeReadConsistency  = "fixed_window_rowid_ceilings_with_retention_mutability"
	DefaultChangeReadLimit = 50
	MaxChangeReadLimit     = 100
	DefaultChangeWindow    = 24 * time.Hour
	MaxChangeWindow        = 30 * 24 * time.Hour
	MaxChangeReadDuration  = 10 * time.Second
)

var (
	ErrInvalidChangeRead       = errors.New("operator: invalid change read request")
	ErrChangeReadTraversalGone = errors.New("operator: change traversal invalidated by retention")
	ErrChangeReadTooBroad      = errors.New("operator: change read is too broad")
	ErrChangeReadBusy          = errors.New("operator: change reader is busy")
	ErrChangeReadTimedOut      = errors.New("operator: change read timed out")
)

const (
	ChangeKindState      = "state"
	ChangeKindRegistry   = "registry"
	ChangeKindIdentity   = store.KindIdentity
	ChangeKindCredential = store.KindCredential
	ChangeKindCLITool    = store.KindCLITool
	ChangeKindSystemd    = store.KindSystemd
	ChangeKindOpenClaw   = store.KindOpenClaw

	ChangeCoverageNotApplicable = "not_applicable"
)

var allChangeKinds = []string{
	ChangeKindState,
	ChangeKindRegistry,
	ChangeKindIdentity,
	ChangeKindCredential,
	ChangeKindCLITool,
	ChangeKindSystemd,
	ChangeKindOpenClaw,
}

func ChangeKinds() []string { return append([]string(nil), allChangeKinds...) }

type ChangeListRequest struct {
	MachineID string
	Kinds     []string
	Subject   string
	From      *time.Time
	To        *time.Time
	Limit     int
	Cursor    string
}

type ChangeWindow struct {
	From           time.Time `json:"from"`
	To             time.Time `json:"to"`
	Boundary       string    `json:"boundary"`
	TimeBasis      string    `json:"time_basis"`
	MaximumSeconds int64     `json:"maximum_seconds"`
}

type ChangeCreationCeilings struct {
	Observations      int64 `json:"observations"`
	StateHistory      int64 `json:"state_history"`
	Registry          int64 `json:"registry"`
	RegistryLifecycle int64 `json:"registry_lifecycle"`
	RetentionLog      int64 `json:"retention_log"`
}

type ChangeKindCount struct {
	Kind  string `json:"kind"`
	Count int    `json:"count"`
}

type ChangeCoverage struct {
	ObservationComparison    string     `json:"observation_comparison"`
	ObservationHistory       string     `json:"observation_history"`
	ObservationRowsPruned    int64      `json:"observation_rows_pruned"`
	LastObservationPrunedAt  *time.Time `json:"last_observation_pruned_at"`
	ObservationPrunedBefore  *time.Time `json:"observation_pruned_before"`
	MalformedTimestampRows   int        `json:"malformed_timestamp_rows"`
	UnplaceableTimestampRows int        `json:"unplaceable_timestamp_rows"`
	RegistryHistory          string     `json:"registry_history"`
	RegistryHistoryStartedAt *time.Time `json:"registry_history_started_at"`
	StateHistory             string     `json:"state_history"`
	StateHistoryStartedAt    *time.Time `json:"state_history_started_at"`
	Issues                   []string   `json:"issues"`
}

type ChangeListResult struct {
	SchemaVersion    int                    `json:"schema_version"`
	Consistency      string                 `json:"consistency"`
	EvaluatedAt      time.Time              `json:"evaluated_at"`
	Window           ChangeWindow           `json:"window"`
	CreationCeilings ChangeCreationCeilings `json:"creation_ceilings"`
	Total            int                    `json:"total"`
	MatchedTotal     int                    `json:"matched_total"`
	KindCounts       []ChangeKindCount      `json:"kind_counts"`
	Coverage         ChangeCoverage         `json:"coverage"`
	Items            []ChangeItem           `json:"items"`
	NextCursor       *string                `json:"next_cursor"`
}

// ChangeValue is a fixed, typed allowlist shared by JSON, HTML and CLI. Fields
// irrelevant to an item's kind stay null. It intentionally has no hostname,
// account identifier, path, PID, argv, note, error, or free-form reason field;
// measured is a typed boolean, and free-form reason remains forbidden.
type ChangeValue struct {
	State     *state.State `json:"state"`
	Lifecycle *string      `json:"lifecycle"`
	Present   *bool        `json:"present"`
	// Measured 只在 systemd 且 present 為 false 或 null 時出現；它是區分
	//「systemctl 明確回答沒有這個 unit」與「這一輪沒量到」的唯一欄位。
	// ⚠ present=true 時一定是 nil。
	Measured  *bool             `json:"measured"`
	Status    *model.CredStatus `json:"status"`
	ExpiresAt *time.Time        `json:"expires_at"`

	OS            *string `json:"os"`
	Kernel        *string `json:"kernel"`
	Arch          *string `json:"arch"`
	LingerEnabled *bool   `json:"linger_enabled"`

	OnPath                 *bool   `json:"on_path"`
	VersionReported        *string `json:"version_reported"`
	VersionPackageJSON     *string `json:"version_package_json"`
	VersionSourcesDisagree *bool   `json:"version_sources_disagree"`
	DaemonReach            *string `json:"daemon_reach"`
	RunningInstallMismatch *bool   `json:"running_install_mismatch"`

	ActiveState *string `json:"active_state"`
	SubState    *string `json:"sub_state"`
	Restarts    *int    `json:"restarts"`

	CLIVersion      *string `json:"cli_version"`
	GatewayVersion  *string `json:"gateway_version"`
	UpstreamVersion *string `json:"upstream_version"`
}

type ChangeItem struct {
	ChangeID        string       `json:"change_id"`
	MachineRef      string       `json:"machine_ref"`
	MachineID       *string      `json:"machine_id"`
	DisplayName     string       `json:"display_name"`
	Kind            string       `json:"kind"`
	Subject         string       `json:"subject"`
	Semantics       string       `json:"semantics"`
	ChangedAt       time.Time    `json:"changed_at"`
	AgentMeasuredAt *time.Time   `json:"agent_measured_at"`
	Severity        *int         `json:"severity"`
	BaselineStatus  string       `json:"baseline_status"`
	Before          *ChangeValue `json:"before"`
	After           *ChangeValue `json:"after"`
	ChangedFields   []string     `json:"changed_fields"`
	RedactedFields  []string     `json:"redacted_fields"`
	Issues          []string     `json:"issues"`
	AlteredFields   []string     `json:"altered_fields"`

	position changeReadPosition
}

type changeReadPosition struct {
	At          time.Time `json:"at"`
	SourceRank  int       `json:"source_rank"`
	SourceRowID int64     `json:"source_rowid"`
	ChangeID    string    `json:"change_id"`
}

type changeListCursor struct {
	Version      int                    `json:"v"`
	FilterDigest string                 `json:"filter_digest"`
	EvaluatedAt  time.Time              `json:"evaluated_at"`
	From         time.Time              `json:"from"`
	To           time.Time              `json:"to"`
	Ceilings     ChangeCreationCeilings `json:"ceilings"`
	After        changeReadPosition     `json:"after"`
}

// ValidateChangeListRequest performs all validation that does not require a
// Hub clock or ledger. Official clients call it before discovery or DB I/O.
func ValidateChangeListRequest(request ChangeListRequest) error {
	normalized, err := normalizeChangeListRequest(request)
	if err != nil {
		return err
	}
	if normalized.Cursor == "" {
		return nil
	}
	cursor, err := decodeChangeListCursor(normalized.Cursor, changeFilterDigest(normalized))
	if err != nil {
		return err
	}
	if normalized.From != nil && !normalized.From.Equal(cursor.From) {
		return fmt.Errorf("%w: from does not match cursor window", ErrInvalidChangeRead)
	}
	if normalized.To != nil && !normalized.To.Equal(cursor.To) {
		return fmt.Errorf("%w: to does not match cursor window", ErrInvalidChangeRead)
	}
	return nil
}

func normalizeChangeListRequest(request ChangeListRequest) (ChangeListRequest, error) {
	for name, field := range map[string]struct {
		value string
		max   int
	}{
		"machine_id": {request.MachineID, 256},
		"subject":    {request.Subject, 256},
	} {
		if field.value != "" && !validChangeFilterText(field.value, field.max) {
			return ChangeListRequest{}, fmt.Errorf("%w: %s is not canonical", ErrInvalidChangeRead, name)
		}
	}
	if request.Subject != "" && !allowlistedChangeFilterSubject(request.Subject) {
		return ChangeListRequest{}, fmt.Errorf("%w: subject is not a public change subject", ErrInvalidChangeRead)
	}
	seen := make(map[string]bool, len(request.Kinds))
	for _, kind := range request.Kinds {
		if !isChangeKind(kind) || seen[kind] {
			return ChangeListRequest{}, fmt.Errorf("%w: kind %q is unknown or repeated", ErrInvalidChangeRead, kind)
		}
		seen[kind] = true
	}
	request.Kinds = make([]string, 0, len(seen))
	for _, kind := range allChangeKinds {
		if seen[kind] {
			request.Kinds = append(request.Kinds, kind)
		}
	}
	for name, value := range map[string]*time.Time{"from": request.From, "to": request.To} {
		if value == nil {
			continue
		}
		if value.IsZero() || value.Nanosecond() != 0 {
			return ChangeListRequest{}, fmt.Errorf("%w: %s must use non-zero second precision", ErrInvalidChangeRead, name)
		}
		utc := value.UTC()
		if name == "from" {
			request.From = &utc
		} else {
			request.To = &utc
		}
	}
	if request.From != nil && request.To != nil && !request.From.Before(*request.To) {
		return ChangeListRequest{}, fmt.Errorf("%w: from must be before to", ErrInvalidChangeRead)
	}
	if request.From != nil && request.To != nil && request.To.Sub(*request.From) > MaxChangeWindow {
		return ChangeListRequest{}, fmt.Errorf("%w: window exceeds %s", ErrInvalidChangeRead, MaxChangeWindow)
	}
	if request.Limit == 0 {
		request.Limit = DefaultChangeReadLimit
	}
	if request.Limit < 1 || request.Limit > MaxChangeReadLimit {
		return ChangeListRequest{}, fmt.Errorf("%w: limit must be between 1 and %d", ErrInvalidChangeRead, MaxChangeReadLimit)
	}
	if request.Cursor != "" && !validChangeFilterText(request.Cursor, 4096) {
		return ChangeListRequest{}, fmt.Errorf("%w: cursor is not canonical", ErrInvalidChangeRead)
	}
	return request, nil
}

func effectiveChangeWindow(request ChangeListRequest, evaluatedAt time.Time) (time.Time, time.Time, error) {
	evaluatedAt = evaluatedAt.UTC().Truncate(time.Second)
	to := evaluatedAt
	if request.To != nil {
		to = request.To.UTC()
	}
	from := to.Add(-DefaultChangeWindow)
	if request.From != nil {
		from = request.From.UTC()
	}
	if !from.Before(to) {
		return time.Time{}, time.Time{}, fmt.Errorf("%w: from must be before to", ErrInvalidChangeRead)
	}
	if to.After(evaluatedAt) {
		return time.Time{}, time.Time{}, fmt.Errorf("%w: to must not be after evaluated_at", ErrInvalidChangeRead)
	}
	if to.Sub(from) > MaxChangeWindow {
		return time.Time{}, time.Time{}, fmt.Errorf("%w: window exceeds %s", ErrInvalidChangeRead, MaxChangeWindow)
	}
	return from, to, nil
}

func validChangeFilterText(value string, maxBytes int) bool {
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

func isChangeKind(value string) bool {
	for _, kind := range allChangeKinds {
		if value == kind {
			return true
		}
	}
	return false
}

func changeFilterDigest(request ChangeListRequest) string {
	body := struct {
		Version   int      `json:"v"`
		MachineID string   `json:"machine_id"`
		Kinds     []string `json:"kinds"`
		Subject   string   `json:"subject"`
	}{ChangeReadSchemaVersion, request.MachineID, request.Kinds, request.Subject}
	raw, _ := json.Marshal(body)
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func encodeChangeListCursor(cursor changeListCursor) (string, error) {
	raw, err := json.Marshal(cursor)
	if err != nil {
		return "", fmt.Errorf("operator: encode change cursor: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func decodeChangeListCursor(encoded, filterDigest string) (changeListCursor, error) {
	if len(encoded) == 0 || len(encoded) > 4096 {
		return changeListCursor{}, fmt.Errorf("%w: cursor length is invalid", ErrInvalidChangeRead)
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || base64.RawURLEncoding.EncodeToString(raw) != encoded {
		return changeListCursor{}, fmt.Errorf("%w: cursor encoding is invalid", ErrInvalidChangeRead)
	}
	var cursor changeListCursor
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cursor); err != nil {
		return changeListCursor{}, fmt.Errorf("%w: cursor document is invalid", ErrInvalidChangeRead)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return changeListCursor{}, fmt.Errorf("%w: cursor has trailing JSON", ErrInvalidChangeRead)
	}
	canonical, encodeErr := encodeChangeListCursor(cursor)
	if encodeErr != nil || canonical != encoded || cursor.Version != ChangeReadSchemaVersion ||
		cursor.FilterDigest != filterDigest || cursor.EvaluatedAt.IsZero() ||
		cursor.EvaluatedAt.Nanosecond() != 0 || cursor.From.IsZero() || cursor.To.IsZero() ||
		cursor.From.Nanosecond() != 0 || cursor.To.Nanosecond() != 0 ||
		!cursor.From.Before(cursor.To) || cursor.To.After(cursor.EvaluatedAt) ||
		cursor.To.Sub(cursor.From) > MaxChangeWindow || !utcChangeTime(cursor.EvaluatedAt) ||
		!utcChangeTime(cursor.From) || !utcChangeTime(cursor.To) ||
		!validOperatorChangeCeilings(cursor.Ceilings) || !validChangePosition(cursor.After) ||
		!cursor.After.At.After(cursor.From) || cursor.After.At.After(cursor.To) {
		return changeListCursor{}, fmt.Errorf("%w: cursor does not match this query", ErrInvalidChangeRead)
	}
	return cursor, nil
}

func validChangePosition(position changeReadPosition) bool {
	if !strings.HasPrefix(position.ChangeID, "sha256:") || len(position.ChangeID) != len("sha256:")+64 {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(position.ChangeID, "sha256:"))
	return err == nil && !position.At.IsZero() && position.At.Nanosecond() == 0 && utcChangeTime(position.At) &&
		position.SourceRank >= 1 && position.SourceRank <= 3 && position.SourceRowID >= 1
}

func validOperatorChangeCeilings(value ChangeCreationCeilings) bool {
	return value.Observations >= 0 && value.StateHistory >= 0 && value.Registry >= 0 &&
		value.RegistryLifecycle >= 0 && value.RetentionLog >= 0
}

func utcChangeTime(value time.Time) bool {
	_, offset := value.Zone()
	return offset == 0
}

func compareChangePosition(a, b changeReadPosition) int {
	if !a.At.Equal(b.At) {
		if a.At.After(b.At) {
			return -1
		}
		return 1
	}
	if a.SourceRank != b.SourceRank {
		if a.SourceRank < b.SourceRank {
			return -1
		}
		return 1
	}
	if a.SourceRowID != b.SourceRowID {
		if a.SourceRowID > b.SourceRowID {
			return -1
		}
		return 1
	}
	return strings.Compare(a.ChangeID, b.ChangeID)
}

func changeOpaqueID(source string, rowID int64) string {
	// Source + rowid is already unique inside the ledger generation to which
	// the cursor ceilings belong. Keeping raw machine/subject evidence out of
	// the digest also prevents ChangeID from becoming a dictionary oracle for
	// a low-entropy account identifier that the public DTO deliberately hides.
	raw := fmt.Sprintf("v2\x00%s\x00%d", source, rowID)
	sum := sha256.Sum256([]byte(raw))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func machineOpaqueRef(machineID string) string {
	sum := sha256.Sum256([]byte("change-machine-v1\x00" + machineID))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func sortedUniqueStrings(values []string) []string {
	if len(values) == 0 {
		return []string{}
	}
	seen := make(map[string]bool, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value != "" && !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	sort.Strings(result)
	return result
}

// ListChanges is the shared read model for the Web BFF, JSON API, and direct
// break-glass CLI. The Store primitive supplies immutable source positions and
// raw payloads tagged json:"-"; projection into this allowlist happens here.
func (s *Service) ListChanges(request ChangeListRequest, evaluatedAt time.Time) (ChangeListResult, error) {
	return s.ListChangesContext(context.Background(), request, evaluatedAt)
}

func (s *Service) ListChangesContext(ctx context.Context, request ChangeListRequest, evaluatedAt time.Time) (ChangeListResult, error) {
	if ctx == nil {
		return ChangeListResult{}, fmt.Errorf("%w: context is required", ErrInvalidChangeRead)
	}
	if evaluatedAt.IsZero() {
		return ChangeListResult{}, fmt.Errorf("%w: evaluated_at is required", ErrInvalidChangeRead)
	}
	evaluatedAt = evaluatedAt.UTC().Truncate(time.Second)
	normalized, err := normalizeChangeListRequest(request)
	if err != nil {
		return ChangeListResult{}, err
	}
	filterDigest := changeFilterDigest(normalized)
	var decoded *changeListCursor
	var from, to time.Time
	var storeCeilings *store.ChangeReadCeilings
	if normalized.Cursor != "" {
		cursor, err := decodeChangeListCursor(normalized.Cursor, filterDigest)
		if err != nil {
			return ChangeListResult{}, err
		}
		if normalized.From != nil && !normalized.From.Equal(cursor.From) {
			return ChangeListResult{}, fmt.Errorf("%w: from does not match cursor window", ErrInvalidChangeRead)
		}
		if normalized.To != nil && !normalized.To.Equal(cursor.To) {
			return ChangeListResult{}, fmt.Errorf("%w: to does not match cursor window", ErrInvalidChangeRead)
		}
		decoded, from, to, evaluatedAt = &cursor, cursor.From, cursor.To, cursor.EvaluatedAt
		ceilings := storeChangeCeilings(cursor.Ceilings)
		storeCeilings = &ceilings
	} else {
		from, to, err = effectiveChangeWindow(normalized, evaluatedAt)
		if err != nil {
			return ChangeListResult{}, err
		}
	}

	readCtx, cancel := context.WithTimeout(ctx, MaxChangeReadDuration)
	defer cancel()
	read, err := s.store.ReadChangesContext(readCtx, store.ChangeReadRequest{
		From: from, To: to, MachineID: normalized.MachineID,
		Kinds: normalized.Kinds, Subject: normalized.Subject, Ceilings: storeCeilings,
	})
	if err != nil {
		switch {
		case errors.Is(err, store.ErrInvalidChangeRead):
			return ChangeListResult{}, fmt.Errorf("%w: %v", ErrInvalidChangeRead, err)
		case errors.Is(err, store.ErrChangeReadTraversalGone):
			return ChangeListResult{}, fmt.Errorf("%w: %v", ErrChangeReadTraversalGone, err)
		case errors.Is(err, store.ErrChangeReadTooBroad):
			return ChangeListResult{}, fmt.Errorf("%w: %v", ErrChangeReadTooBroad, err)
		case errors.Is(err, store.ErrChangeReadBusy):
			return ChangeListResult{}, fmt.Errorf("%w: %v", ErrChangeReadBusy, err)
		case errors.Is(err, context.DeadlineExceeded):
			return ChangeListResult{}, fmt.Errorf("%w: %v", ErrChangeReadTimedOut, err)
		}
		return ChangeListResult{}, err
	}
	ceilings := operatorChangeCeilings(read.Ceilings)
	if decoded != nil && ceilings != decoded.Ceilings {
		return ChangeListResult{}, errors.New("operator change read: store changed fixed source ceilings")
	}
	result := ChangeListResult{
		SchemaVersion: ChangeReadSchemaVersion, Consistency: ChangeReadConsistency,
		EvaluatedAt: evaluatedAt,
		Window: ChangeWindow{
			From: from, To: to, Boundary: "(from,to]", TimeBasis: "hub_received_at",
			MaximumSeconds: int64(MaxChangeWindow / time.Second),
		},
		CreationCeilings: ceilings,
		KindCounts:       make([]ChangeKindCount, 0, len(allChangeKinds)),
		Coverage:         changeCoverage(read, normalized), Items: make([]ChangeItem, 0, normalized.Limit),
	}

	all := make([]ChangeItem, 0, len(read.Records))
	counts := make(map[string]int, len(allChangeKinds))
	for _, record := range read.Records {
		if !changeMatches(record, normalized) {
			return ChangeListResult{}, errors.New("operator change read: store returned a record outside exact filters")
		}
		item, err := projectChangeRecord(record, read.Coverage)
		if err != nil {
			return ChangeListResult{}, err
		}
		// Subject is agent-controlled for observation rows. The Store pushes the
		// raw equality predicate into its index, but a value that collides with a
		// public subject for another kind (for example credential/provider=state)
		// is redacted by projection and must not satisfy the public exact filter.
		if normalized.Subject != "" && item.Subject != normalized.Subject {
			continue
		}
		all = append(all, item)
		counts[item.Kind]++
	}
	sort.SliceStable(all, func(i, j int) bool {
		return compareChangePosition(all[i].position, all[j].position) < 0
	})
	result.Total, result.MatchedTotal = len(all), len(all)
	for _, kind := range allChangeKinds {
		result.KindCounts = append(result.KindCounts, ChangeKindCount{Kind: kind, Count: counts[kind]})
	}
	remaining := all[:0]
	for _, item := range all {
		if decoded != nil && compareChangePosition(item.position, decoded.After) <= 0 {
			continue
		}
		remaining = append(remaining, item)
	}
	pageSize := min(normalized.Limit, len(remaining))
	result.Items = append(result.Items, remaining[:pageSize]...)
	if len(remaining) > pageSize {
		anchor := result.Items[len(result.Items)-1].position
		encoded, err := encodeChangeListCursor(changeListCursor{
			Version: ChangeReadSchemaVersion, FilterDigest: filterDigest,
			EvaluatedAt: evaluatedAt, From: from, To: to, Ceilings: ceilings, After: anchor,
		})
		if err != nil {
			return ChangeListResult{}, err
		}
		result.NextCursor = &encoded
	}
	return result, nil
}

func operatorChangeCeilings(value store.ChangeReadCeilings) ChangeCreationCeilings {
	return ChangeCreationCeilings{
		Observations: value.ObservedState, StateHistory: value.StateHistory,
		Registry: value.Registry, RegistryLifecycle: value.RegistryLifecycle,
		RetentionLog: value.RetentionLog,
	}
}

func storeChangeCeilings(value ChangeCreationCeilings) store.ChangeReadCeilings {
	return store.ChangeReadCeilings{
		ObservedState: value.Observations, StateHistory: value.StateHistory,
		Registry: value.Registry, RegistryLifecycle: value.RegistryLifecycle,
		RetentionLog: value.RetentionLog,
	}
}

func changeCoverage(read store.ChangeReadResult, request ChangeListRequest) ChangeCoverage {
	readObservations, readRegistry, readState := changeCoverageApplicability(request)
	coverage := ChangeCoverage{
		ObservationComparison:    "endpoint_delta",
		ObservationHistory:       ChangeCoverageNotApplicable,
		RegistryHistory:          ChangeCoverageNotApplicable,
		StateHistory:             ChangeCoverageNotApplicable,
		MalformedTimestampRows:   read.MalformedTimestamps,
		UnplaceableTimestampRows: read.UnplaceableTimestamps,
		Issues:                   append([]string(nil), read.Coverage.Issues...),
	}
	if readObservations {
		coverage.ObservationHistory = "complete"
		coverage.ObservationRowsPruned = read.Coverage.ObservationRowsPruned
		coverage.LastObservationPrunedAt = changeTimePointer(read.Coverage.LastObservationPrunedAt)
		coverage.ObservationPrunedBefore = changeTimePointer(read.Coverage.ObservationPrunedBefore)
		if read.Coverage.ObservationHistoryPruned {
			coverage.ObservationHistory = "partial"
		}
	}
	if readRegistry {
		coverage.RegistryHistory = "complete"
		coverage.RegistryHistoryStartedAt = changeTimePointer(read.Coverage.RegistryLifecycleTrackedFrom)
		if !read.Coverage.RegistryLifecycleHistoryComplete {
			coverage.RegistryHistory = "partial_before_tracking_started"
		}
	}
	if readState {
		coverage.StateHistory = "complete"
		coverage.StateHistoryStartedAt = changeTimePointer(read.Coverage.StateTransitionTrackedFrom)
		if !read.Coverage.StateTransitionHistoryComplete {
			coverage.StateHistory = "partial_before_tracking_started"
		}
	}
	// The Store returns the durable classification rather than arbitrary row
	// text. Keep the wire issue set deterministic and bounded.
	coverage.Issues = sortedUniqueStrings(coverage.Issues)
	return coverage
}

// changeCoverageApplicability mirrors the Store source selection. Kinds are
// normalized before this helper is called: an empty slice means all kinds.
func changeCoverageApplicability(request ChangeListRequest) (observations, registry, stateHistory bool) {
	selected := func(kind string) bool {
		if len(request.Kinds) == 0 {
			return true
		}
		for _, candidate := range request.Kinds {
			if candidate == kind {
				return true
			}
		}
		return false
	}
	for _, kind := range []string{
		ChangeKindIdentity, ChangeKindCredential, ChangeKindCLITool, ChangeKindSystemd, ChangeKindOpenClaw,
	} {
		if selected(kind) {
			observations = true
			break
		}
	}
	registry = selected(ChangeKindRegistry) && (request.Subject == "" || request.Subject == "lifecycle")
	stateHistory = selected(ChangeKindState) && (request.Subject == "" || request.Subject == "state")
	return observations, registry, stateHistory
}

func changeTimePointer(value *time.Time) *time.Time {
	if value == nil || value.IsZero() {
		return nil
	}
	utc := value.UTC()
	return &utc
}

func changeMatches(record store.ChangeReadRecord, request ChangeListRequest) bool {
	if request.MachineID != "" && request.MachineID != record.MachineID {
		return false
	}
	if request.Subject != "" && request.Subject != record.Subject {
		return false
	}
	if len(request.Kinds) > 0 {
		for _, kind := range request.Kinds {
			if kind == record.Kind {
				return true
			}
		}
		return false
	}
	return true
}

func projectChangeRecord(record store.ChangeReadRecord, coverage store.ChangeReadCoverage) (ChangeItem, error) {
	if record.Key.Sequence < 1 || record.HubAt.IsZero() || record.HubAt.Nanosecond() != 0 || !isChangeKind(record.Kind) {
		return ChangeItem{}, errors.New("operator change read: invalid store source position")
	}
	sourceRank, semantics := changeSourceSemantics(record.Key.Source)
	if sourceRank == 0 {
		return ChangeItem{}, errors.New("operator change read: unknown store source")
	}
	changedAt := record.HubAt.UTC()
	changeID := changeOpaqueID(string(record.Key.Source), record.Key.Sequence)
	item := ChangeItem{
		ChangeID: changeID, MachineRef: machineOpaqueRef(record.MachineID),
		DisplayName: record.DisplayName, Kind: record.Kind, Subject: record.Subject,
		Semantics: semantics, ChangedAt: changedAt,
		AgentMeasuredAt: changeTimePointer(record.MeasuredAt), BaselineStatus: "known",
		ChangedFields: []string{}, RedactedFields: []string{},
		Issues: append([]string{}, record.Issues...), AlteredFields: []string{},
		position: changeReadPosition{
			At: changedAt, SourceRank: sourceRank, SourceRowID: record.Key.Sequence, ChangeID: changeID,
		},
	}
	if validChangeFilterText(record.MachineID, 256) {
		machineID := record.MachineID
		item.MachineID = &machineID
	} else {
		item.Issues = append(item.Issues, "machine_id_not_representable")
		item.AlteredFields = append(item.AlteredFields, "machine_id")
	}
	item.DisplayName = scrubChangeText(item.DisplayName, 256, "display_name", &item.Issues, &item.AlteredFields)
	if strings.TrimSpace(item.DisplayName) == "" {
		item.DisplayName = "(unnamed machine)"
		item.Issues = append(item.Issues, "display_name_missing")
		item.AlteredFields = append(item.AlteredFields, "display_name")
	}
	item.Subject = safeChangeSubject(record.Kind, record.Subject, &item.Issues, &item.AlteredFields)

	switch record.Kind {
	case ChangeKindState, ChangeKindRegistry:
		item.Before = projectTransitionValue(record.Kind, record.From, record.FromKnown, "before", &item)
		item.After = projectTransitionValue(record.Kind, record.To, true, "after", &item)
		if !record.FromKnown {
			if record.Kind == ChangeKindRegistry {
				item.BaselineStatus = "not_registered"
			} else {
				item.BaselineStatus = "not_observed"
			}
		}
	case ChangeKindIdentity, ChangeKindCredential, ChangeKindCLITool, ChangeKindSystemd, ChangeKindOpenClaw:
		if record.FromKnown {
			item.Before = projectObservationValue(record.Kind, record.FromPayload, "before", &item)
			if item.Before == nil {
				item.BaselineStatus = "malformed"
			}
		} else if coverage.ObservationHistoryPruned {
			item.BaselineStatus = "possibly_pruned"
			item.Issues = append(item.Issues, "baseline_possibly_pruned")
		} else {
			item.BaselineStatus = "not_observed"
		}
		item.After = projectObservationValue(record.Kind, record.ToPayload, "after", &item)
		item.RedactedFields = redactedObservationFields(record)
	}
	item.ChangedFields = changedChangeFields(record.Kind, item.Before, item.After)
	if record.Kind == ChangeKindState && item.After != nil && item.After.State != nil {
		severity := item.After.State.Severity()
		item.Severity = &severity
	}
	item.Issues = sortedUniqueStrings(item.Issues)
	item.AlteredFields = sortedUniqueStrings(item.AlteredFields)
	item.RedactedFields = sortedUniqueStrings(item.RedactedFields)
	return item, nil
}

func changeSourceSemantics(source store.ChangeReadSource) (int, string) {
	switch source {
	case store.ChangeReadSourceStateHistory:
		return 1, "transition"
	case store.ChangeReadSourceRegistryCreated, store.ChangeReadSourceRegistryLifecycle:
		return 2, "transition"
	case store.ChangeReadSourceObservation:
		return 3, "window_comparison"
	default:
		return 0, ""
	}
}

func projectTransitionValue(kind, raw string, known bool, field string, item *ChangeItem) *ChangeValue {
	if !known {
		return nil
	}
	value := &ChangeValue{}
	switch kind {
	case ChangeKindState:
		candidate := state.State(raw)
		if !knownState(candidate) {
			item.Issues = append(item.Issues, field+"_state_not_canonical")
			return nil
		}
		value.State = &candidate
	case ChangeKindRegistry:
		if raw != "registered" && raw != "retired" {
			item.Issues = append(item.Issues, field+"_lifecycle_not_canonical")
			return nil
		}
		value.Lifecycle = stringPointer(raw)
	}
	return value
}

func knownState(value state.State) bool {
	for _, candidate := range state.AllStates {
		if value == candidate {
			return true
		}
	}
	return false
}

func projectObservationValue(kind, raw, field string, item *ChangeItem) *ChangeValue {
	value := &ChangeValue{}
	switch kind {
	case ChangeKindCredential:
		var credential model.Credential
		if json.Unmarshal([]byte(raw), &credential) != nil {
			item.Issues = append(item.Issues, field+"_payload_malformed")
			return nil
		}
		if knownCredentialStatus(credential.Status) {
			status := credential.Status
			value.Status = &status
		} else {
			item.Issues = append(item.Issues, field+"_credential_status_not_canonical")
		}
		value.ExpiresAt = changeTimePointer(credential.ExpiresAt)
	case ChangeKindCLITool:
		var tool model.CLITool
		if json.Unmarshal([]byte(raw), &tool) != nil {
			item.Issues = append(item.Issues, field+"_payload_malformed")
			return nil
		}
		value.Present, value.OnPath = boolPointer(tool.Present), boolPointer(tool.OnPath)
		value.VersionReported = safeOptionalChangeText(tool.VersionReported, 128, field+".version_reported", item)
		value.VersionPackageJSON = safeOptionalChangeText(tool.VersionPackageJSON, 128, field+".version_package_json", item)
		value.VersionSourcesDisagree = boolPointer(tool.SourcesDisagree)
		if tool.DaemonReach == "" || tool.DaemonReach == model.DaemonReachSame ||
			tool.DaemonReach == model.DaemonReachShadowed || tool.DaemonReach == model.DaemonReachMissing {
			value.DaemonReach = safeOptionalChangeText(tool.DaemonReach, 32, field+".daemon_reach", item)
		} else {
			item.Issues = append(item.Issues, field+"_daemon_reach_not_canonical")
		}
		mismatch := tool.RunningExe != "" && tool.RealPath != "" && tool.RunningExe != tool.RealPath
		value.RunningInstallMismatch = &mismatch
	case ChangeKindSystemd:
		var unit model.Unit
		if json.Unmarshal([]byte(raw), &unit) != nil {
			item.Issues = append(item.Issues, field+"_payload_malformed")
			return nil
		}
		// ⚠ Present=true 已經代表確實量到；舊 agent 的 Measured 零值無意義，這一格不得投影 measured。
		if unit.Present {
			value.Present = boolPointer(true)
		} else if unit.Measured {
			value.Present = boolPointer(false)
			value.Measured = boolPointer(true)
		} else {
			value.Measured = boolPointer(false)
		}
		value.ActiveState = safeOptionalChangeText(unit.ActiveState, 64, field+".active_state", item)
		value.SubState = safeOptionalChangeText(unit.SubState, 64, field+".sub_state", item)
		// ⚠ 這一輪沒有量到就沒有 restarts 可講；零值不是「量到 0 次」，
		// 也不是負數計數器證據，所以整段跳過。
		if unit.Present || unit.Measured {
			if unit.NRestarts < 0 {
				// Agent evidence is not trusted to satisfy the operator wire
				// contract. Preserve the malformed fact as an issue instead of
				// emitting a negative counter that official clients must reject.
				item.Issues = append(item.Issues, field+"_restarts_not_canonical")
			} else {
				value.Restarts = intPointer(unit.NRestarts)
			}
		}
	case ChangeKindOpenClaw:
		var openclaw model.OpenClaw
		if json.Unmarshal([]byte(raw), &openclaw) != nil {
			item.Issues = append(item.Issues, field+"_payload_malformed")
			return nil
		}
		value.Present = boolPointer(openclaw.Present)
		value.CLIVersion = safeOptionalChangeText(openclaw.CLIVersion, 128, field+".cli_version", item)
		value.GatewayVersion = safeOptionalChangeText(openclaw.GatewayVersion, 128, field+".gateway_version", item)
		value.UpstreamVersion = safeOptionalChangeText(openclaw.UpstreamVersion, 128, field+".upstream_version", item)
	case ChangeKindIdentity:
		var identity model.Identity
		if json.Unmarshal([]byte(raw), &identity) != nil {
			item.Issues = append(item.Issues, field+"_payload_malformed")
			return nil
		}
		value.OS = safeOptionalChangeText(identity.OS, 128, field+".os", item)
		value.Kernel = safeOptionalChangeText(identity.Kernel, 128, field+".kernel", item)
		value.Arch = safeOptionalChangeText(identity.Arch, 64, field+".arch", item)
		value.LingerEnabled = boolPointer(identity.LingerEnabled)
	default:
		return nil
	}
	return value
}

func knownCredentialStatus(value model.CredStatus) bool {
	switch value {
	case model.CredAbsent, model.CredConfigured, model.CredExpired, model.CredUnknown,
		model.CredFailed, model.CredExpiresSoon:
		return true
	default:
		return false
	}
}

func safeOptionalChangeText(value string, maxBytes int, field string, item *ChangeItem) *string {
	if value == "" {
		return nil
	}
	safe := scrubChangeText(value, maxBytes, field, &item.Issues, &item.AlteredFields)
	return &safe
}

func scrubChangeText(value string, maxBytes int, field string, issues, altered *[]string) string {
	value, _, textIssues := scrubBoundedText(value, maxBytes)
	issuePrefix := strings.ReplaceAll(field, ".", "_")
	for _, issue := range textIssues {
		*issues = append(*issues, issuePrefix+"_"+issue)
	}
	if len(textIssues) != 0 {
		*altered = append(*altered, field)
	}
	return value
}

func safeChangeSubject(kind, subject string, issues, altered *[]string) string {
	switch kind {
	case ChangeKindState:
		return "state"
	case ChangeKindRegistry:
		return "lifecycle"
	case ChangeKindIdentity:
		return "identity"
	case ChangeKindOpenClaw:
		return "openclaw"
	}
	if allowlistedChangeSubject(kind, subject) {
		return subject
	}
	// Credential providers, executable names, and unit names originate in
	// agent payloads. A merely printable token is not proof that it is a public
	// product identifier: an email/account ID is printable too. Unknown values
	// therefore become a constant redaction. An unkeyed digest would still be
	// a dictionary oracle for low-entropy account identifiers such as email
	// addresses. ChangeID remains available to identify this individual item.
	*issues = append(*issues, "subject_not_allowlisted")
	*altered = append(*altered, "subject")
	return "(redacted subject)"
}

func allowlistedChangeSubject(kind, subject string) bool {
	var values []string
	switch kind {
	case ChangeKindCredential:
		values = []string{"claude", "codex", "gemini", "grok", "openclaw"}
	case ChangeKindCLITool:
		values = []string{"agy", "claude", "codex", "gemini", "grok", "openclaw"}
	case ChangeKindSystemd:
		values = []string{
			"bat-server.service",
			"clawctl-agent.service",
			"hermes-gateway.service",
			"openclaw-gateway.service",
			"openclaw-watcher.service",
		}
	default:
		return false
	}
	for _, value := range values {
		if subject == value {
			return true
		}
	}
	return false
}

func allowlistedChangeFilterSubject(subject string) bool {
	for _, fixed := range []string{"state", "lifecycle", "identity", "openclaw"} {
		if subject == fixed {
			return true
		}
	}
	return allowlistedChangeSubject(ChangeKindCredential, subject) ||
		allowlistedChangeSubject(ChangeKindCLITool, subject) ||
		allowlistedChangeSubject(ChangeKindSystemd, subject)
}

func changedChangeFields(kind string, before, after *ChangeValue) []string {
	fields := changeFieldsForKind(kind)
	if before == nil && after == nil {
		return []string{}
	}
	var b, a reflect.Value
	if before != nil {
		b = reflect.ValueOf(*before)
	} else {
		b = reflect.Zero(reflect.TypeOf(ChangeValue{}))
	}
	if after != nil {
		a = reflect.ValueOf(*after)
	} else {
		a = reflect.Zero(reflect.TypeOf(ChangeValue{}))
	}
	typ := b.Type()
	changed := make([]string, 0, len(fields))
	allowed := make(map[string]bool, len(fields))
	for _, field := range fields {
		allowed[field] = true
	}
	for i := 0; i < b.NumField(); i++ {
		jsonName := strings.Split(typ.Field(i).Tag.Get("json"), ",")[0]
		if allowed[jsonName] && !reflect.DeepEqual(b.Field(i).Interface(), a.Field(i).Interface()) {
			changed = append(changed, jsonName)
		}
	}
	return sortedUniqueStrings(changed)
}

func changeFieldsForKind(kind string) []string {
	switch kind {
	case ChangeKindState:
		return []string{"state"}
	case ChangeKindRegistry:
		return []string{"lifecycle"}
	case ChangeKindIdentity:
		return []string{"arch", "kernel", "linger_enabled", "os"}
	case ChangeKindCredential:
		return []string{"expires_at", "status"}
	case ChangeKindCLITool:
		return []string{"daemon_reach", "on_path", "present", "running_install_mismatch", "version_package_json", "version_reported", "version_sources_disagree"}
	case ChangeKindSystemd:
		return []string{"active_state", "measured", "present", "restarts", "sub_state"}
	case ChangeKindOpenClaw:
		return []string{"cli_version", "gateway_version", "present", "upstream_version"}
	default:
		return []string{}
	}
}

func redactedObservationFields(record store.ChangeReadRecord) []string {
	switch record.Kind {
	case ChangeKindIdentity:
		return []string{"host_identity"}
	case ChangeKindCredential:
		return []string{"account_identity"}
	case ChangeKindCLITool:
		return []string{"execution_paths", "process_identity"}
	case ChangeKindSystemd:
		return []string{"process_identity"}
	case ChangeKindOpenClaw:
		return []string{"absence_reason", "installation_details"}
	}
	return []string{}
}

func boolPointer(value bool) *bool       { return &value }
func intPointer(value int) *int          { return &value }
func stringPointer(value string) *string { return &value }
