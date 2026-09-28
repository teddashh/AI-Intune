package operator

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/teddashh/AI-Intune/internal/state"
	"github.com/teddashh/AI-Intune/internal/store"
)

const (
	MachineReadSchemaVersion     = 2
	MachineListReadSchemaVersion = MachineReadSchemaVersion
	// MachineDetailReadSchemaVersion advances independently from the fleet list:
	// v6 makes resource memory and load typed absences (mem_measured and nullable
	// load_1m), so old strict clients fail clearly on the version instead of null.
	MachineDetailReadSchemaVersion = 6
	MachineDetailIdentityHintLimit = 100
	MachineDetailFindingLimit      = 100
	MachineDetailExpectationLimit  = 50
	MachineDetailEventTypeLimit    = 20
	MachineDetailMaxTextBytes      = 4096
	// MachineReadConsistency freezes only the immutable registry creation key.
	// Health, lifecycle, channel and filter membership remain live on every
	// page; this is deliberately not described as a database snapshot.
	MachineReadConsistency  = "registry_creation_ceiling_with_live_health"
	DefaultMachineReadLimit = 50
	MaxMachineReadLimit     = 100
)

var ErrInvalidMachineRead = errors.New("operator: invalid machine read request")

type MachineLifecycleFilter string

const (
	MachineLifecycleAny     MachineLifecycleFilter = "any"
	MachineLifecycleActive  MachineLifecycleFilter = "active"
	MachineLifecycleRetired MachineLifecycleFilter = "retired"
)

type MachineReportingFilter string

const (
	MachineReportingAny     MachineReportingFilter = "any"
	MachineReportingTrue    MachineReportingFilter = "true"
	MachineReportingFalse   MachineReportingFilter = "false"
	MachineReportingUnknown MachineReportingFilter = "unknown"
)

type MachineChannelFilter string

const (
	MachineChannelAny    MachineChannelFilter = "any"
	MachineChannelNone   MachineChannelFilter = "none"
	MachineChannelCanary MachineChannelFilter = "canary"
	MachineChannelStable MachineChannelFilter = "stable"
)

type MachineListRequest struct {
	MachineID   string
	DisplayName string
	States      []state.State
	Lifecycle   MachineLifecycleFilter
	Reporting   MachineReportingFilter
	Channel     MachineChannelFilter
	Limit       int
	Cursor      string
}

type MachineReadPosition struct {
	CreatedAt time.Time `json:"created_at"`
	MachineID string    `json:"machine_id"`
}

// MachineListResult is an operator-owned, deliberately small projection. The
// registry and evidence structs must never be serialized directly: they carry
// host identity, network addresses, paths and free-form agent evidence that a
// fleet index does not need.
type MachineListResult struct {
	SchemaVersion          int                 `json:"schema_version"`
	Consistency            string              `json:"consistency"`
	EvaluatedAt            time.Time           `json:"evaluated_at"`
	CreationCeiling        int64               `json:"creation_ceiling"`
	Total                  int                 `json:"total"`
	MatchedTotal           int                 `json:"matched_total"`
	Active                 int                 `json:"active"`
	Retired                int                 `json:"retired"`
	Expected               int                 `json:"expected"`
	Reporting              int                 `json:"reporting"`
	StateCounts            []MachineStateCount `json:"state_counts"`
	DenominatorStateCounts []MachineStateCount `json:"denominator_state_counts"`
	Items                  []MachineSummary    `json:"items"`
	NextCursor             *string             `json:"next_cursor"`

	overview *store.Overview
}

// StoreOverview exposes the creation-ceiling-bounded in-process read used by
// trusted HTML. Registry display names use the same safe projection as Items;
// richer evidence remains available to the BFF. HTTP-decoded results report
// ok=false instead of silently looking like an empty fleet.
func (r MachineListResult) StoreOverview() (overview store.Overview, ok bool) {
	if r.overview == nil {
		return store.Overview{}, false
	}
	return *r.overview, true
}

type MachineStateCount struct {
	State state.State `json:"state"`
	Count int         `json:"count"`
}

// MachineSummary is the only registry/evidence shape allowed on the fleet
// JSON surface. Pointer fields encode unknown as null, never as an empty value
// that automation could misread as a measured zero.
type MachineSummary struct {
	MachineID                 string       `json:"machine_id"`
	DisplayName               string       `json:"display_name"`
	Expected                  bool         `json:"expected"`
	CreatedAt                 time.Time    `json:"created_at"`
	EnrolledAt                *time.Time   `json:"enrolled_at"`
	RetiredAt                 *time.Time   `json:"retired_at"`
	Channel                   *string      `json:"channel"`
	ChannelRevision           int64        `json:"channel_revision"`
	State                     *state.State `json:"state"`
	StateSince                *time.Time   `json:"state_since"`
	Reporting                 *bool        `json:"reporting"`
	LastCheckinReceivedAt     *time.Time   `json:"last_checkin_received_at"`
	LastObservationReceivedAt *time.Time   `json:"last_observation_received_at"`
	Issues                    []string     `json:"issues"`
	AlteredFields             []string     `json:"altered_fields"`

	registrySequence int64
}

type machineDetailTextField string

const (
	machineDetailAgentVersion     machineDetailTextField = "monitor.agent_version"
	machineDetailCheckinBootID    machineDetailTextField = "checkin.boot_id"
	machineDetailHostname         machineDetailTextField = "identity.hostname"
	machineDetailOS               machineDetailTextField = "identity.os"
	machineDetailKernel           machineDetailTextField = "identity.kernel"
	machineDetailArch             machineDetailTextField = "identity.arch"
	machineDetailUnixUser         machineDetailTextField = "identity.unix_user"
	machineDetailMachineIDHint    machineDetailTextField = "identity.machine_id_hint"
	machineDetailTailscaleIP      machineDetailTextField = "identity.tailscale_ip"
	machineDetailJudgementReason  machineDetailTextField = "judgement.reason"
	machineDetailFindingMessage   machineDetailTextField = "judgement.finding.message"
	machineDetailExpectationError machineDetailTextField = "expectations.error"
	machineDetailExpectationUnit  machineDetailTextField = "expectations.rule.unit"
	machineDetailExpectationPath  machineDetailTextField = "expectations.rule.artifact"
	machineDetailExpectationWhy   machineDetailTextField = "expectations.rule.why"
	machineDetailArtifactError    machineDetailTextField = "expectations.rule.observation.error"
	machineDetailEventType        machineDetailTextField = "expectations.rule.events.type"
	machineDetailEventError       machineDetailTextField = "expectations.rule.events.error"
	machineDetailStateReason      machineDetailTextField = "state_history.reason"
)

var machineDetailTextPolicies = map[machineDetailTextField]evidenceTextPolicy{
	machineDetailAgentVersion:     {maxBytes: 256, policy: boundedTextStrict},
	machineDetailCheckinBootID:    {maxBytes: 256, policy: boundedTextStrict},
	machineDetailHostname:         {maxBytes: 256, policy: boundedTextStrict},
	machineDetailOS:               {maxBytes: 256, policy: boundedTextStrict},
	machineDetailKernel:           {maxBytes: 256, policy: boundedTextStrict},
	machineDetailArch:             {maxBytes: 64, policy: boundedTextStrict},
	machineDetailUnixUser:         {maxBytes: 256, policy: boundedTextStrict},
	machineDetailMachineIDHint:    {maxBytes: 256, policy: boundedTextStrict},
	machineDetailTailscaleIP:      {maxBytes: 64, policy: boundedTextStrict},
	machineDetailJudgementReason:  {maxBytes: MachineDetailMaxTextBytes, policy: boundedTextBlock},
	machineDetailFindingMessage:   {maxBytes: MachineDetailMaxTextBytes, policy: boundedTextBlock},
	machineDetailExpectationError: {maxBytes: MachineDetailMaxTextBytes, policy: boundedTextBlock},
	machineDetailExpectationUnit:  {maxBytes: 256, policy: boundedTextStrict},
	machineDetailExpectationPath:  {maxBytes: MachineDetailMaxTextBytes, policy: boundedTextStrict},
	machineDetailExpectationWhy:   {maxBytes: MachineDetailMaxTextBytes, policy: boundedTextBlock},
	machineDetailArtifactError:    {maxBytes: MachineDetailMaxTextBytes, policy: boundedTextBlock},
	machineDetailEventType:        {maxBytes: 256, policy: boundedTextStrict},
	machineDetailEventError:       {maxBytes: MachineDetailMaxTextBytes, policy: boundedTextBlock},
	machineDetailStateReason:      {maxBytes: MachineDetailMaxTextBytes, policy: boundedTextBlock},
}

// MachineDetailDisclosure states the trust and omission boundary for the
// typed detail sections. Identity values are intentionally visible to an
// authenticated operator; actionable BAT coordinates remain excluded.
type MachineDetailDisclosure struct {
	CheckinLimit                          int                     `json:"checkin_limit"`
	CheckinWindowSeconds                  int64                   `json:"checkin_window_seconds"`
	StateHistoryLimit                     int                     `json:"state_history_limit"`
	IdentityHintLimit                     int                     `json:"identity_hint_limit"`
	FindingLimit                          int                     `json:"finding_limit"`
	ExpectationLimit                      int                     `json:"expectation_limit"`
	EventTypeLimit                        int                     `json:"event_type_limit"`
	MaxTextBytes                          int                     `json:"max_text_bytes"`
	ObserverProducer                      MachineEvidenceProducer `json:"observer_producer"`
	IndependentVerifier                   bool                    `json:"independent_verifier"`
	LivenessUsesReceivedAt                bool                    `json:"liveness_uses_received_at"`
	AgentSentAtIsLiveness                 bool                    `json:"agent_sent_at_is_liveness"`
	HostIdentityIncluded                  bool                    `json:"host_identity_included"`
	TailscaleIPIncluded                   bool                    `json:"tailscale_ip_included"`
	ConnectCoordinatesExcluded            bool                    `json:"connect_coordinates_excluded"`
	PendingEnrollmentExcluded             bool                    `json:"pending_enrollment_excluded"`
	ExpectationDisplayDefinitionsIncluded bool                    `json:"expectation_display_definitions_included"`
	ExpectationConfigPathFieldExcluded    bool                    `json:"expectation_config_path_field_excluded"`
	ExpectationParserFieldsExcluded       bool                    `json:"expectation_parser_fields_excluded"`
	ArtifactPathsIncluded                 bool                    `json:"artifact_paths_included"`
	ArtifactContentInspected              bool                    `json:"artifact_content_inspected"`
	ArtifactFreshnessIsWorkOutcome        bool                    `json:"artifact_freshness_is_work_outcome"`
	EventFailureTypesOperatorDeclared     bool                    `json:"event_failure_types_operator_declared"`
	EventContentKeywordScanning           bool                    `json:"event_content_keyword_scanning"`
	ExpectationTextPathRedacted           bool                    `json:"expectation_text_path_redacted_by_hub"`
	ExpectationTextSecretRedacted         bool                    `json:"expectation_text_secret_redacted_by_hub"`
	JudgementDerivedByHub                 bool                    `json:"judgement_derived_by_hub"`
	JudgementMayUseUnverifiedInput        bool                    `json:"judgement_may_use_unverified_input"`
	JudgementTextParsedByHub              bool                    `json:"judgement_text_parsed_by_hub"`
	JudgementTextPathRedacted             bool                    `json:"judgement_text_path_redacted_by_hub"`
	JudgementTextSecretRedacted           bool                    `json:"judgement_text_secret_redacted_by_hub"`
	KnownSecretFieldsExcluded             bool                    `json:"known_secret_fields_excluded"`
	StateReasonParsedByHub                bool                    `json:"state_reason_parsed_by_hub"`
	StateReasonPathRedactedByHub          bool                    `json:"state_reason_path_redacted_by_hub"`
	CompleteHistoryClaimed                bool                    `json:"complete_history_claimed"`
	IdentityHintsNonExpiring              bool                    `json:"identity_hints_non_expiring"`
}

type MachineMonitor struct {
	EverCheckedIn             bool          `json:"ever_checked_in"`
	LastCheckinReceivedAt     *time.Time    `json:"last_checkin_received_at"`
	CheckinIntervalSeconds    int64         `json:"checkin_interval_seconds"`
	ClockSkewSeconds          *int64        `json:"clock_skew_seconds"`
	AgentVersion              *EvidenceText `json:"agent_version"`
	DistinctAgentStarts1h     *int          `json:"distinct_agent_starts_1h"`
	DistinctBootIDs1h         *int          `json:"distinct_boot_ids_1h"`
	AgentUnitNRestarts        *int          `json:"agent_unit_n_restarts"`
	LingerEnabled             *bool         `json:"linger_enabled"`
	LastObservationReceivedAt *time.Time    `json:"last_observation_received_at"`
}

type MachineCheckinPage struct {
	RowsSeen  int              `json:"rows_seen"`
	Invalid   int              `json:"invalid"`
	Truncated bool             `json:"truncated"`
	Items     []MachineCheckin `json:"items"`
}

type MachineCheckin struct {
	SentAt                time.Time     `json:"sent_at"`
	ReceivedAt            time.Time     `json:"received_at"`
	AgentVersion          *EvidenceText `json:"agent_version"`
	BootID                *EvidenceText `json:"boot_id"`
	AgentSeq              *int64        `json:"agent_seq"`
	UptimeSeconds         *int64        `json:"uptime_seconds"`
	DiskFreeBytes         *int64        `json:"disk_free_bytes"`
	DiskTotalBytes        *int64        `json:"disk_total_bytes"`
	ObservationAgeSeconds *int64        `json:"observation_age_seconds"`
	ClockSkewSeconds      *int64        `json:"clock_skew_seconds"`
}

type MachineStateHistoryPage struct {
	RowsSeen  int                `json:"rows_seen"`
	Invalid   int                `json:"invalid"`
	Truncated bool               `json:"truncated"`
	Items     []MachineStateSpan `json:"items"`
}

type MachineStateSpan struct {
	State     state.State  `json:"state"`
	Reason    EvidenceText `json:"reason"`
	EnteredAt time.Time    `json:"entered_at"`
	LeftAt    *time.Time   `json:"left_at"`
}

type MachineIdentityEvidence struct {
	Observed   bool                  `json:"observed"`
	Decoded    bool                  `json:"decoded"`
	ObservedAt *MachineEvidenceClock `json:"observed_at"`
	Value      *MachineIdentity      `json:"value"`
}

type MachineIdentity struct {
	Hostname       EvidenceText  `json:"hostname"`
	OS             EvidenceText  `json:"os"`
	Kernel         EvidenceText  `json:"kernel"`
	Arch           EvidenceText  `json:"arch"`
	UnixUser       EvidenceText  `json:"unix_user"`
	MachineIDHint  EvidenceText  `json:"machine_id_hint"`
	BootID         EvidenceText  `json:"boot_id"`
	TailscaleIP    *EvidenceText `json:"tailscale_ip"`
	LingerEnabled  bool          `json:"linger_enabled"`
	LingerMeasured bool          `json:"linger_measured"`
}

type MachineIdentityHintPage struct {
	Total     int                   `json:"total"`
	Invalid   int                   `json:"invalid"`
	Truncated bool                  `json:"truncated"`
	Items     []MachineIdentityHint `json:"items"`
}

type MachineIdentityHint struct {
	Hint             EvidenceText `json:"hint"`
	FirstSeenAt      *time.Time   `json:"first_seen_at"`
	LastSeenAt       *time.Time   `json:"last_seen_at"`
	ObservationCount int          `json:"observation_count"`
	RegistryDeclared bool         `json:"registry_declared"`
}

type MachineResourceEvidence struct {
	Observed   bool                  `json:"observed"`
	Decoded    bool                  `json:"decoded"`
	Invalid    bool                  `json:"invalid"`
	ObservedAt *MachineEvidenceClock `json:"observed_at"`
	Value      *MachineResources     `json:"value"`
}

type MachineResources struct {
	DiskFreeBytes     int64    `json:"disk_free_bytes"`
	DiskTotalBytes    int64    `json:"disk_total_bytes"`
	MemTotalBytes     int64    `json:"mem_total_bytes"`
	MemAvailableBytes int64    `json:"mem_available_bytes"`
	CPUCount          int      `json:"cpu_count"`
	Load1m            *float64 `json:"load_1m"`

	// A machine with zero total memory cannot run the agent, so zero means memory was not measured.
	MemMeasured bool `json:"mem_measured"`
}

// MachineJudgement is a point-in-time Hub derivation. For retired machines it
// remains useful retained-evidence diagnosis, but AffectsFleetState is false
// and Item.State stays null.
type MachineJudgement struct {
	State             state.State        `json:"state"`
	Reason            EvidenceText       `json:"reason"`
	AffectsFleetState bool               `json:"affects_fleet_state"`
	Findings          MachineFindingPage `json:"findings"`
}

type MachineFindingPage struct {
	Total     int              `json:"total"`
	Invalid   int              `json:"invalid"`
	Truncated bool             `json:"truncated"`
	Items     []MachineFinding `json:"items"`
}

type MachineExpectationSection struct {
	Configured bool                   `json:"configured"`
	ReadFailed bool                   `json:"read_failed"`
	Error      *EvidenceText          `json:"error"`
	Rules      MachineExpectationPage `json:"rules"`
}

type MachineExpectationPage struct {
	Total     int                  `json:"total"`
	Invalid   int                  `json:"invalid"`
	Truncated bool                 `json:"truncated"`
	Items     []MachineExpectation `json:"items"`
}

type MachineExpectation struct {
	Unit          EvidenceText               `json:"unit"`
	Artifact      EvidenceText               `json:"artifact"`
	Why           EvidenceText               `json:"why"`
	MaxAgeSeconds int64                      `json:"max_age_seconds"`
	Observation   MachineArtifactObservation `json:"observation"`
	Events        *MachineExpectationEvents  `json:"events"`
}

type MachineArtifactObservation struct {
	Observed   bool                  `json:"observed"`
	Decoded    bool                  `json:"decoded"`
	Invalid    bool                  `json:"invalid"`
	ObservedAt *MachineEvidenceClock `json:"observed_at"`
	ReadFailed bool                  `json:"read_failed"`
	Error      *EvidenceText         `json:"error"`
	Exists     *bool                 `json:"exists"`
	ModifiedAt *time.Time            `json:"modified_at"`
}

type MachineExpectationEvents struct {
	WindowSeconds int64                 `json:"window_seconds"`
	FailureTypes  MachineEventTypePage  `json:"failure_types"`
	Observed      bool                  `json:"observed"`
	Decoded       bool                  `json:"decoded"`
	Invalid       bool                  `json:"invalid"`
	ObservedAt    *MachineEvidenceClock `json:"observed_at"`
	ReadFailed    bool                  `json:"read_failed"`
	Error         *EvidenceText         `json:"error"`
	Partial       bool                  `json:"partial"`
	CoveredFrom   *time.Time            `json:"covered_from"`
	Malformed     int                   `json:"malformed"`
	Declared      MachineEventCountPage `json:"declared"`
	Undeclared    MachineEventCountPage `json:"undeclared"`
}

type MachineEventTypePage struct {
	Total     int            `json:"total"`
	Invalid   int            `json:"invalid"`
	Truncated bool           `json:"truncated"`
	Items     []EvidenceText `json:"items"`
}

type MachineEventCountPage struct {
	Total     int                 `json:"total"`
	Invalid   int                 `json:"invalid"`
	Truncated bool                `json:"truncated"`
	Items     []MachineEventCount `json:"items"`
}

type MachineEventCount struct {
	Type   EvidenceText `json:"type"`
	Count  int          `json:"count"`
	LastAt *time.Time   `json:"last_at"`
}

// MachineDetailResult combines the safe fleet summary with bounded typed
// judgement, expectation, monitor, identity, resource and history evidence.
// Actionable BAT coordinates and pending tickets use separate operator
// contracts and remain excluded from this JSON shape.
type MachineDetailResult struct {
	SchemaVersion int                       `json:"schema_version"`
	EvaluatedAt   time.Time                 `json:"evaluated_at"`
	Item          MachineSummary            `json:"item"`
	Judgement     MachineJudgement          `json:"judgement"`
	Expectations  MachineExpectationSection `json:"expectations"`
	Disclosure    MachineDetailDisclosure   `json:"disclosure"`
	Monitor       MachineMonitor            `json:"monitor"`
	Checkins      MachineCheckinPage        `json:"checkins"`
	StateHistory  MachineStateHistoryPage   `json:"state_history"`
	Identity      MachineIdentityEvidence   `json:"identity"`
	IdentityHints MachineIdentityHintPage   `json:"identity_hints"`
	Resources     MachineResourceEvidence   `json:"resources"`

	// detail is an in-process source capability consumed only by MachineConnect.
	// It is unexported and omitted by JSON, so decoded detail cannot acquire the
	// actionable coordinate-bearing BFF projection.
	detail *store.Detail
}

// MachineFinding includes bounded display-only text. The Hub does not parse,
// path-redact, or secret-scan Message; known dedicated secret fields are kept
// out of the DTO structurally.
type MachineFinding struct {
	Kind     string       `json:"kind"`
	Severity int          `json:"severity"`
	Advisory bool         `json:"advisory"`
	Message  EvidenceText `json:"message"`
}

func (s *Service) ListMachines(asOf time.Time) (MachineListResult, error) {
	return s.ListMachinesPage(MachineListRequest{}, asOf)
}

func (s *Service) ListMachinesPage(request MachineListRequest, asOf time.Time) (MachineListResult, error) {
	evaluatedAt, err := machineEvaluationTime(asOf)
	if err != nil {
		return MachineListResult{}, fmt.Errorf("%w: %v", ErrInvalidMachineRead, err)
	}
	normalized, err := normalizeMachineListRequest(request)
	if err != nil {
		return MachineListResult{}, err
	}
	filterDigest := machineFilterDigest(normalized)
	var decodedCursor *machineListCursor
	if normalized.Cursor != "" {
		cursor, err := decodeMachineListCursor(normalized.Cursor, filterDigest)
		if err != nil {
			return MachineListResult{}, err
		}
		decodedCursor = &cursor
	}
	overview, err := s.store.Overview(evaluatedAt)
	if err != nil {
		return MachineListResult{}, err
	}
	all := machineSummariesFrom(overview, evaluatedAt)
	for _, item := range all {
		if item.registrySequence < 1 || item.CreatedAt.IsZero() ||
			validateMachineReadText("registry machine_id", item.MachineID, 256) != nil {
			return MachineListResult{}, fmt.Errorf("operator machine read: corrupt registry creation key")
		}
	}
	sort.SliceStable(all, func(i, j int) bool {
		return compareMachineReadPosition(machineReadPosition(all[i]), machineReadPosition(all[j])) < 0
	})

	var ceiling int64
	var after *MachineReadPosition
	if decodedCursor != nil {
		ceiling = decodedCursor.CreationCeiling
		after = &decodedCursor.After
	} else {
		for _, item := range all {
			ceiling = max(ceiling, item.registrySequence)
		}
	}

	eligible := make([]MachineSummary, 0, len(all))
	for _, item := range all {
		if item.registrySequence > ceiling {
			// The row was inserted after the first page's immutable registry
			// sequence ceiling, so it cannot enter this traversal.
			continue
		}
		eligible = append(eligible, item)
	}
	counts := make(map[state.State]int, len(state.AllStates))
	denominatorCounts := make(map[state.State]int, len(state.AllStates))
	result := MachineListResult{
		SchemaVersion: MachineListReadSchemaVersion, Consistency: MachineReadConsistency,
		EvaluatedAt: evaluatedAt, CreationCeiling: ceiling,
		Total: len(eligible), StateCounts: make([]MachineStateCount, 0, len(state.AllStates)),
		DenominatorStateCounts: make([]MachineStateCount, 0, len(state.AllStates)),
		Items:                  make([]MachineSummary, 0, normalized.Limit),
	}
	matched := make([]MachineSummary, 0, len(eligible))
	for _, item := range eligible {
		if item.RetiredAt != nil {
			result.Retired++
		} else {
			result.Active++
			counts[*item.State]++
			result.Expected++
			denominatorCounts[*item.State]++
			if item.Reporting != nil && *item.Reporting {
				result.Reporting++
			}
		}
		if machineMatchesFilters(item, normalized) {
			matched = append(matched, item)
		}
	}
	result.StateCounts = machineStateCounts(counts)
	result.DenominatorStateCounts = machineStateCounts(denominatorCounts)
	result.MatchedTotal = len(matched)

	remaining := matched[:0]
	for _, item := range matched {
		if after != nil && compareMachineReadPosition(machineReadPosition(item), *after) <= 0 {
			continue
		}
		remaining = append(remaining, item)
	}
	pageSize := min(normalized.Limit, len(remaining))
	result.Items = append(result.Items, remaining[:pageSize]...)
	if len(remaining) > pageSize {
		anchor := machineReadPosition(result.Items[len(result.Items)-1])
		encoded, err := encodeMachineListCursor(machineListCursor{
			Version: MachineListReadSchemaVersion, FilterDigest: filterDigest,
			CreationCeiling: ceiling, After: anchor,
		})
		if err != nil {
			return MachineListResult{}, err
		}
		result.NextCursor = &encoded
	}
	presentationOverview := safeMachineOverview(overview, ceiling)
	result.overview = &presentationOverview
	return result, nil
}

// ValidateMachineListRequest lets CLI adapters reject every malformed filter
// and cursor before discovery, locking or SQLite I/O. The service repeats this
// validation because transport adapters are not a trust boundary.
func ValidateMachineListRequest(request MachineListRequest) error {
	normalized, err := normalizeMachineListRequest(request)
	if err != nil {
		return err
	}
	if normalized.Cursor != "" {
		_, err = decodeMachineListCursor(normalized.Cursor, machineFilterDigest(normalized))
	}
	return err
}

func (s *Service) MachineDetail(machineID string, asOf time.Time) (MachineDetailResult, error) {
	if machineID == "" {
		return MachineDetailResult{}, store.ErrNotFound
	}
	evaluatedAt, err := machineEvaluationTime(asOf)
	if err != nil {
		return MachineDetailResult{}, err
	}
	detail, err := s.store.Detail(machineID, evaluatedAt)
	if err != nil {
		return MachineDetailResult{}, err
	}
	result := MachineDetailResult{
		SchemaVersion: MachineDetailReadSchemaVersion, EvaluatedAt: evaluatedAt,
		detail: &detail,
	}
	if detail.Machine.RetiredAt != nil {
		result.Item = machineSummary(detail.Machine, nil, nil, nil, nil, nil)
		projectMachineDetailSections(&result, detail)
		return result, nil
	}
	machineState := detail.State
	reporting := state.IsReportingAt(detail.Facts, evaluatedAt)
	result.Item = machineSummary(
		detail.Machine, &machineState, currentStateSince(detail.History, detail.State),
		&reporting,
		knownMachineTime(detail.Facts.LastCheckinReceived, detail.Facts.EverCheckedIn),
		knownMachineTime(detail.Facts.LastObservation, detail.Facts.HasObservation),
	)
	projectMachineDetailSections(&result, detail)
	return result, nil
}

func projectMachineDetailSections(result *MachineDetailResult, detail store.Detail) {
	producer := MachineEvidenceProducer{
		Kind: MachineEvidenceProducerObserverAgent, MachineID: result.Item.MachineID,
		DisplayName: result.Item.DisplayName, Authority: MachineEvidenceAuthorityMachineBearer,
	}
	result.Disclosure = MachineDetailDisclosure{
		CheckinLimit:                          store.DetailCheckinLimit,
		CheckinWindowSeconds:                  int64(store.DetailCheckinWindow / time.Second),
		StateHistoryLimit:                     store.DetailHistoryLimit,
		IdentityHintLimit:                     MachineDetailIdentityHintLimit,
		FindingLimit:                          MachineDetailFindingLimit,
		ExpectationLimit:                      MachineDetailExpectationLimit,
		EventTypeLimit:                        MachineDetailEventTypeLimit,
		MaxTextBytes:                          MachineDetailMaxTextBytes,
		ObserverProducer:                      producer,
		IndependentVerifier:                   false,
		LivenessUsesReceivedAt:                true,
		AgentSentAtIsLiveness:                 false,
		HostIdentityIncluded:                  true,
		TailscaleIPIncluded:                   true,
		ConnectCoordinatesExcluded:            true,
		PendingEnrollmentExcluded:             true,
		ExpectationDisplayDefinitionsIncluded: true,
		ExpectationConfigPathFieldExcluded:    true,
		ExpectationParserFieldsExcluded:       true,
		ArtifactPathsIncluded:                 true,
		ArtifactContentInspected:              false,
		ArtifactFreshnessIsWorkOutcome:        false,
		EventFailureTypesOperatorDeclared:     true,
		EventContentKeywordScanning:           false,
		ExpectationTextPathRedacted:           false,
		ExpectationTextSecretRedacted:         false,
		JudgementDerivedByHub:                 true,
		JudgementMayUseUnverifiedInput:        true,
		JudgementTextParsedByHub:              false,
		JudgementTextPathRedacted:             false,
		JudgementTextSecretRedacted:           false,
		KnownSecretFieldsExcluded:             true,
		StateReasonParsedByHub:                false,
		StateReasonPathRedactedByHub:          false,
		CompleteHistoryClaimed:                false,
		IdentityHintsNonExpiring:              true,
	}
	result.Judgement = MachineJudgement{
		State:             detail.State,
		Reason:            projectMachineDetailText(machineDetailJudgementReason, detail.Reason),
		AffectsFleetState: detail.Machine.RetiredAt == nil,
		Findings: MachineFindingPage{
			Total: len(detail.Findings),
			Items: make([]MachineFinding, 0, min(len(detail.Findings), MachineDetailFindingLimit)),
		},
	}
	for _, finding := range detail.Findings {
		if !validMachineFindingKind(finding.Kind) || finding.Severity < 1 || finding.Severity > 4 || finding.Message == "" {
			result.Judgement.Findings.Invalid++
			continue
		}
		if len(result.Judgement.Findings.Items) == MachineDetailFindingLimit {
			continue
		}
		result.Judgement.Findings.Items = append(result.Judgement.Findings.Items, MachineFinding{
			Kind: finding.Kind, Severity: finding.Severity, Advisory: finding.Advisory,
			Message: projectMachineDetailText(machineDetailFindingMessage, finding.Message),
		})
	}
	result.Judgement.Findings.Truncated = len(result.Judgement.Findings.Items)+result.Judgement.Findings.Invalid < result.Judgement.Findings.Total
	projectMachineExpectations(result, detail)
	monitor := MachineMonitor{CheckinIntervalSeconds: int64(detail.Facts.CheckinInterval / time.Second)}
	if monitor.CheckinIntervalSeconds <= 0 {
		monitor.CheckinIntervalSeconds = int64(state.CheckinInterval / time.Second)
	}
	monitor.EverCheckedIn = detail.Facts.EverCheckedIn
	if detail.Facts.EverCheckedIn {
		monitor.LastCheckinReceivedAt = timePointer(detail.Facts.LastCheckinReceived)
		skew := int64(detail.Facts.ClockSkew / time.Second)
		monitor.ClockSkewSeconds = &skew
	}
	monitor.AgentVersion = projectOptionalMachineDetailText(machineDetailAgentVersion, detail.Facts.AgentVersion)
	if detail.Facts.AgentRestartSignalObserved {
		value := detail.Facts.AgentRestarts1h
		monitor.DistinctAgentStarts1h = &value
	}
	if detail.Facts.BootIDSignalObserved {
		value := detail.Facts.BootIDChanges1h
		monitor.DistinctBootIDs1h = &value
	}
	if detail.Facts.AgentUnitSeen {
		value := detail.Facts.AgentNRestarts
		monitor.AgentUnitNRestarts = &value
	}
	if detail.Facts.LingerEnabled != nil {
		value := *detail.Facts.LingerEnabled
		monitor.LingerEnabled = &value
	}
	if detail.Facts.HasObservation {
		monitor.LastObservationReceivedAt = timePointer(detail.Facts.LastObservation)
	}
	result.Monitor = monitor

	result.Checkins = MachineCheckinPage{
		RowsSeen: len(detail.Checkins), Truncated: detail.CheckinsTruncated,
		Items: make([]MachineCheckin, 0, len(detail.Checkins)),
	}
	windowStart := result.EvaluatedAt.Add(-store.DetailCheckinWindow)
	for _, point := range detail.Checkins {
		if point.SentAt.IsZero() || point.ReceivedAt.IsZero() || point.ReceivedAt.After(result.EvaluatedAt) ||
			point.ReceivedAt.Before(windowStart) ||
			(point.AgentSeqObserved && point.AgentSeq < 0) ||
			(point.UptimeObserved && point.UptimeSeconds < 0) ||
			(point.DiskFreeObserved != point.DiskTotalObserved) ||
			(point.DiskFreeObserved && (point.DiskFreeBytes < 0 || point.DiskTotalBytes < 0 ||
				point.DiskFreeBytes > point.DiskTotalBytes)) ||
			(point.ObservationAgeSeconds != nil && *point.ObservationAgeSeconds < 0) {
			result.Checkins.Invalid++
			continue
		}
		item := MachineCheckin{
			SentAt: point.SentAt, ReceivedAt: point.ReceivedAt,
			AgentVersion:          projectOptionalMachineDetailText(machineDetailAgentVersion, point.AgentVersion),
			BootID:                projectOptionalMachineDetailText(machineDetailCheckinBootID, point.BootID),
			ObservationAgeSeconds: copyInt64Pointer(point.ObservationAgeSeconds),
		}
		if point.AgentSeqObserved {
			item.AgentSeq = int64Pointer(point.AgentSeq)
		}
		if point.UptimeObserved {
			item.UptimeSeconds = int64Pointer(point.UptimeSeconds)
		}
		if point.DiskFreeObserved {
			item.DiskFreeBytes = int64Pointer(point.DiskFreeBytes)
			item.DiskTotalBytes = int64Pointer(point.DiskTotalBytes)
		}
		if point.ClockSkewObserved {
			item.ClockSkewSeconds = int64Pointer(int64(point.ClockSkew / time.Second))
		}
		result.Checkins.Items = append(result.Checkins.Items, item)
	}

	result.StateHistory = MachineStateHistoryPage{
		RowsSeen: len(detail.History), Truncated: detail.HistoryTruncated,
		Items: make([]MachineStateSpan, 0, len(detail.History)),
	}
	for _, span := range detail.History {
		if !validMachineState(span.State) || span.EnteredAt.IsZero() || span.EnteredAt.After(result.EvaluatedAt) ||
			(span.LeftAt != nil && (span.LeftAt.IsZero() || span.LeftAt.Before(span.EnteredAt) ||
				span.LeftAt.After(result.EvaluatedAt))) {
			result.StateHistory.Invalid++
			continue
		}
		result.StateHistory.Items = append(result.StateHistory.Items, MachineStateSpan{
			State: span.State, Reason: projectMachineDetailText(machineDetailStateReason, span.Reason),
			EnteredAt: span.EnteredAt, LeftAt: copyTimePointer(span.LeftAt),
		})
	}

	result.IdentityHints = MachineIdentityHintPage{
		Total: len(detail.IdentityHints), Items: make([]MachineIdentityHint, 0, min(len(detail.IdentityHints), MachineDetailIdentityHintLimit)),
	}
	for _, hint := range detail.IdentityHints {
		observations := hint.Count
		if hint.FromRegistry {
			observations--
		}
		if observations < 0 || (observations > 0 && (hint.FirstSeen.IsZero() || hint.LastSeen.IsZero() ||
			hint.LastSeen.Before(hint.FirstSeen) || hint.FirstSeen.After(result.EvaluatedAt) ||
			hint.LastSeen.After(result.EvaluatedAt))) {
			result.IdentityHints.Invalid++
			continue
		}
		if len(result.IdentityHints.Items) == MachineDetailIdentityHintLimit {
			result.IdentityHints.Truncated = true
			continue
		}
		item := MachineIdentityHint{
			Hint:             projectMachineDetailText(machineDetailMachineIDHint, hint.Hint),
			ObservationCount: observations, RegistryDeclared: hint.FromRegistry,
		}
		if observations > 0 {
			item.FirstSeenAt = timePointer(hint.FirstSeen)
			item.LastSeenAt = timePointer(hint.LastSeen)
		}
		result.IdentityHints.Items = append(result.IdentityHints.Items, item)
	}

	result.Identity = MachineIdentityEvidence{
		Observed: detail.IdentityObserved, Decoded: detail.IdentityDecoded,
		ObservedAt: machineDetailClock(detail.IdentityObservedAt),
	}
	if detail.IdentityDecoded && detail.Identity != nil {
		identity := detail.Identity
		result.Identity.Value = &MachineIdentity{
			Hostname:       projectMachineDetailText(machineDetailHostname, identity.Hostname),
			OS:             projectMachineDetailText(machineDetailOS, identity.OS),
			Kernel:         projectMachineDetailText(machineDetailKernel, identity.Kernel),
			Arch:           projectMachineDetailText(machineDetailArch, identity.Arch),
			UnixUser:       projectMachineDetailText(machineDetailUnixUser, identity.UnixUser),
			MachineIDHint:  projectMachineDetailText(machineDetailMachineIDHint, identity.MachineIDHint),
			BootID:         projectMachineDetailText(machineDetailCheckinBootID, identity.BootID),
			TailscaleIP:    projectOptionalMachineDetailText(machineDetailTailscaleIP, identity.TailscaleIP),
			LingerEnabled:  identity.LingerEnabled,
			LingerMeasured: identity.LingerMeasured,
		}
	}
	result.Resources = MachineResourceEvidence{
		Observed: detail.ResourcesObserved, Decoded: detail.ResourcesDecoded,
		ObservedAt: machineDetailClock(detail.ResourcesObservedAt),
	}
	if detail.ResourcesDecoded && detail.Resources != nil {
		resources := detail.Resources
		// These checks keep both memory values non-negative and available <= total, so a zero total already implies zero available.
		if resources.DiskFreeBytes < 0 || resources.DiskTotalBytes < 0 || resources.DiskFreeBytes > resources.DiskTotalBytes ||
			resources.MemAvailableBytes < 0 || resources.MemTotalBytes < 0 ||
			resources.MemAvailableBytes > resources.MemTotalBytes || resources.CPUCount < 0 ||
			resources.Load1m != nil && (*resources.Load1m < 0 || math.IsNaN(*resources.Load1m) || math.IsInf(*resources.Load1m, 0)) {
			result.Resources.Invalid = true
		} else {
			result.Resources.Value = &MachineResources{
				DiskFreeBytes: resources.DiskFreeBytes, DiskTotalBytes: resources.DiskTotalBytes,
				MemTotalBytes: resources.MemTotalBytes, MemAvailableBytes: resources.MemAvailableBytes,
				CPUCount: resources.CPUCount, Load1m: resources.Load1m,
				MemMeasured: resources.MemTotalBytes > 0,
			}
		}
	}
}

func projectMachineExpectations(result *MachineDetailResult, detail store.Detail) {
	result.Expectations = MachineExpectationSection{
		Configured: detail.ExpectConfigured,
		ReadFailed: detail.ExpectErr != "",
		Rules: MachineExpectationPage{
			Items: make([]MachineExpectation, 0, min(len(detail.Facts.Artifacts), MachineDetailExpectationLimit)),
		},
	}
	if detail.ExpectErr != "" {
		result.Expectations.Error = projectOptionalMachineDetailText(machineDetailExpectationError, detail.ExpectErr)
		return
	}
	if !detail.ExpectConfigured {
		return
	}
	result.Expectations.Rules.Total = len(detail.Facts.Artifacts)
	for _, fact := range detail.Facts.Artifacts {
		if strings.TrimSpace(fact.Unit) == "" || !strings.HasPrefix(fact.Artifact, "/") ||
			strings.TrimSpace(fact.Why) == "" || fact.MaxAge < time.Second || fact.MaxAge%time.Second != 0 ||
			(fact.Events != nil && (fact.Events.Window < time.Second || fact.Events.Window%time.Second != 0 ||
				len(fact.Events.FailureTypes) == 0)) {
			result.Expectations.Rules.Invalid++
			continue
		}
		if len(result.Expectations.Rules.Items) == MachineDetailExpectationLimit {
			continue
		}
		item := MachineExpectation{
			Unit:          projectMachineDetailText(machineDetailExpectationUnit, fact.Unit),
			Artifact:      projectMachineDetailText(machineDetailExpectationPath, fact.Artifact),
			Why:           projectMachineDetailText(machineDetailExpectationWhy, fact.Why),
			MaxAgeSeconds: int64(fact.MaxAge / time.Second),
			Observation:   projectMachineArtifactObservation(fact, result.EvaluatedAt),
		}
		if fact.Events != nil {
			events := projectMachineExpectationEvents(*fact.Events, result.EvaluatedAt)
			item.Events = &events
		}
		result.Expectations.Rules.Items = append(result.Expectations.Rules.Items, item)
	}
	result.Expectations.Rules.Truncated = len(result.Expectations.Rules.Items)+result.Expectations.Rules.Invalid < result.Expectations.Rules.Total
}

func projectMachineArtifactObservation(fact state.ArtifactFact, evaluatedAt time.Time) MachineArtifactObservation {
	result := MachineArtifactObservation{
		Observed: fact.ObservationObserved,
		Decoded:  fact.ObservationDecoded,
		Invalid:  fact.ObservationInvalid,
	}
	if fact.ObservationObserved && validMachineObservationClock(fact.MeasuredAt, fact.ReceivedAt, evaluatedAt) {
		result.ObservedAt = &MachineEvidenceClock{MeasuredAt: fact.MeasuredAt.UTC(), ReceivedAt: fact.ReceivedAt.UTC()}
	} else if fact.ObservationObserved {
		result.Invalid = true
	}
	if result.Invalid {
		return result
	}
	if fact.ObservationDecoded && !fact.ObservationObserved {
		result.Invalid = true
		return result
	}
	if !fact.ObservationDecoded {
		return result
	}
	if !fact.Checked || (fact.Err != "" && (fact.Exists || fact.ModTime != nil)) ||
		(fact.Err == "" && !fact.Exists && fact.ModTime != nil) ||
		(fact.ModTime != nil && (fact.ModTime.IsZero() || fact.ModTime.After(evaluatedAt.Add(state.ClockSkewTolerance)))) {
		result.Invalid = true
		return result
	}
	if fact.Err != "" {
		result.ReadFailed = true
		result.Error = projectOptionalMachineDetailText(machineDetailArtifactError, fact.Err)
		return result
	}
	result.Exists = boolPointer(fact.Exists)
	if fact.ModTime != nil {
		result.ModifiedAt = copyUTCTimePointer(fact.ModTime)
	}
	return result
}

func projectMachineExpectationEvents(fact state.EventFact, evaluatedAt time.Time) MachineExpectationEvents {
	result := MachineExpectationEvents{
		WindowSeconds: int64(fact.Window / time.Second),
		FailureTypes:  projectMachineEventTypes(fact.FailureTypes),
		Observed:      fact.ObservationObserved,
		Decoded:       fact.ObservationDecoded,
		Invalid:       fact.ObservationInvalid,
		Declared:      emptyMachineEventCountPage(),
		Undeclared:    emptyMachineEventCountPage(),
	}
	if fact.ObservationObserved && validMachineObservationClock(fact.MeasuredAt, fact.ReceivedAt, evaluatedAt) {
		result.ObservedAt = &MachineEvidenceClock{MeasuredAt: fact.MeasuredAt.UTC(), ReceivedAt: fact.ReceivedAt.UTC()}
	} else if fact.ObservationObserved {
		result.Invalid = true
	}
	if result.Invalid {
		return result
	}
	if fact.ObservationDecoded && !fact.ObservationObserved {
		result.Invalid = true
		return result
	}
	if !fact.ObservationDecoded {
		return result
	}
	if !fact.Measured || fact.Malformed < 0 || fact.UndeclaredTotal < len(fact.Undeclared) ||
		(fact.Partial && fact.CoveredFrom == nil) ||
		(fact.CoveredFrom != nil && (fact.CoveredFrom.IsZero() || fact.CoveredFrom.After(evaluatedAt.Add(state.ClockSkewTolerance)))) {
		result.Invalid = true
		return result
	}
	if fact.Err != "" {
		if fact.Partial || fact.CoveredFrom != nil || fact.Malformed != 0 || len(fact.Declared) != 0 ||
			len(fact.Undeclared) != 0 || fact.UndeclaredTotal != 0 {
			result.Invalid = true
			return result
		}
		result.ReadFailed = true
		result.Error = projectOptionalMachineDetailText(machineDetailEventError, fact.Err)
		return result
	}
	result.Partial = fact.Partial
	result.CoveredFrom = copyUTCTimePointer(fact.CoveredFrom)
	result.Malformed = fact.Malformed
	result.Declared = projectMachineEventCounts(fact.Declared, len(fact.Declared), evaluatedAt)
	result.Undeclared = projectMachineEventCounts(fact.Undeclared, fact.UndeclaredTotal, evaluatedAt)
	return result
}

func projectMachineEventTypes(values []string) MachineEventTypePage {
	result := MachineEventTypePage{
		Total: len(values), Items: make([]EvidenceText, 0, min(len(values), MachineDetailEventTypeLimit)),
	}
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		if strings.TrimSpace(value) == "" {
			result.Invalid++
			continue
		}
		projected := projectMachineDetailText(machineDetailEventType, value)
		if seen[projected.Text] {
			result.Invalid++
			continue
		}
		seen[projected.Text] = true
		if len(result.Items) == MachineDetailEventTypeLimit {
			continue
		}
		result.Items = append(result.Items, projected)
	}
	result.Truncated = len(result.Items)+result.Invalid < result.Total
	return result
}

func emptyMachineEventCountPage() MachineEventCountPage {
	return MachineEventCountPage{Items: []MachineEventCount{}}
}

func projectMachineEventCounts(values []state.EventCount, total int, evaluatedAt time.Time) MachineEventCountPage {
	result := MachineEventCountPage{
		Total: total, Items: make([]MachineEventCount, 0, min(len(values), MachineDetailEventTypeLimit)),
	}
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		projectedType := projectMachineDetailText(machineDetailEventType, value.Type)
		if strings.TrimSpace(value.Type) == "" || seen[projectedType.Text] || value.Count < 0 ||
			(value.Count == 0) != (value.LastAt == nil) ||
			(value.LastAt != nil && (value.LastAt.IsZero() || value.LastAt.After(evaluatedAt.Add(state.ClockSkewTolerance)))) {
			result.Invalid++
			continue
		}
		seen[projectedType.Text] = true
		if len(result.Items) == MachineDetailEventTypeLimit {
			continue
		}
		result.Items = append(result.Items, MachineEventCount{
			Type:  projectedType,
			Count: value.Count, LastAt: copyUTCTimePointer(value.LastAt),
		})
	}
	result.Truncated = len(result.Items)+result.Invalid < result.Total
	return result
}

func validMachineObservationClock(measuredAt, receivedAt, evaluatedAt time.Time) bool {
	return !measuredAt.IsZero() && !receivedAt.IsZero() && !receivedAt.After(evaluatedAt)
}

func copyUTCTimePointer(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	utc := value.UTC()
	return &utc
}

func projectMachineDetailText(field machineDetailTextField, value string) EvidenceText {
	spec, ok := machineDetailTextPolicies[field]
	if !ok {
		panic("operator: missing machine detail text policy for " + string(field))
	}
	return projectEvidenceText(spec, value)
}

func projectOptionalMachineDetailText(field machineDetailTextField, value string) *EvidenceText {
	if value == "" {
		return nil
	}
	projected := projectMachineDetailText(field, value)
	return &projected
}

func machineDetailClock(value *store.ObservationClock) *MachineEvidenceClock {
	if value == nil {
		return nil
	}
	return &MachineEvidenceClock{MeasuredAt: value.MeasuredAt, ReceivedAt: value.ReceivedAt}
}

func validMachineState(candidate state.State) bool {
	for _, valid := range state.AllStates {
		if candidate == valid {
			return true
		}
	}
	return false
}

func validMachineFindingKind(kind string) bool {
	switch kind {
	case "agent", "clock", "config", "credential", "disk", "machine", "reachability", "workload":
		return true
	default:
		return false
	}
}

func timePointer(value time.Time) *time.Time { return &value }
func copyTimePointer(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	return timePointer(*value)
}
func int64Pointer(value int64) *int64 { return &value }
func copyInt64Pointer(value *int64) *int64 {
	if value == nil {
		return nil
	}
	return int64Pointer(*value)
}

func machineEvaluationTime(asOf time.Time) (time.Time, error) {
	if asOf.IsZero() {
		return time.Time{}, errors.New("operator machine read: evaluation time is required")
	}
	return asOf.UTC(), nil
}

func machineStateCounts(counts map[state.State]int) []MachineStateCount {
	result := make([]MachineStateCount, 0, len(state.AllStates))
	for _, machineState := range state.AllStates {
		result = append(result, MachineStateCount{State: machineState, Count: counts[machineState]})
	}
	return result
}

// machineSummariesFrom 把一次 Overview 讀成名冊上每一台的安全投影。
//
// ⚠ 它被機隊清單與註冊報告共用，不是各自照著 Overview 再組一次。兩邊各組一次的
// 話，同一台機器會在兩個頁面上有兩個「最後一次報到」——而其中一個是錯的。
func machineSummariesFrom(overview store.Overview, evaluatedAt time.Time) []MachineSummary {
	all := make([]MachineSummary, 0, len(overview.Machines)+len(overview.Retired))
	for _, row := range overview.Machines {
		machineState := row.State
		reporting := state.IsReportingAt(row.Facts, evaluatedAt)
		all = append(all, machineSummary(
			row.Machine, &machineState, row.StateSince, &reporting,
			knownMachineTime(row.Facts.LastCheckinReceived, row.Facts.EverCheckedIn),
			knownMachineTime(row.Facts.LastObservation, row.Facts.HasObservation),
		))
	}
	for _, machine := range overview.Retired {
		all = append(all, machineSummary(machine, nil, nil, nil, nil, nil))
	}
	return all
}

func machineSummary(machine store.Machine, machineState *state.State,
	stateSince *time.Time, reporting *bool, lastCheckin, lastObservation *time.Time,
) MachineSummary {
	displayName, displayIssues := safeMachineDisplayName(machine.DisplayName)
	summary := MachineSummary{
		MachineID: machine.MachineID, DisplayName: machine.DisplayName,
		Expected: machine.Expected, CreatedAt: machine.CreatedAt.UTC(),
		EnrolledAt: machineTime(machine.EnrolledAt), RetiredAt: machineTime(machine.RetiredAt),
		Channel: machineString(machine.Channel), ChannelRevision: machine.ChannelRevision,
		State: machineState, StateSince: machineTime(stateSince),
		Reporting: reporting, LastCheckinReceivedAt: machineTime(lastCheckin),
		LastObservationReceivedAt: machineTime(lastObservation),
		Issues:                    []string{}, AlteredFields: []string{},
		registrySequence: machine.RegistrySequence,
	}
	summary.DisplayName = displayName
	if len(displayIssues) > 0 {
		summary.Issues = append(summary.Issues, displayIssues...)
		summary.AlteredFields = append(summary.AlteredFields, "display_name")
	}
	return summary
}

func safeMachineDisplayName(value string) (string, []string) {
	issues := make([]string, 0, 3)
	if !utf8.ValidString(value) {
		value = strings.ToValidUTF8(value, "�")
		issues = append(issues, "display_name_invalid_utf8")
	}
	var out strings.Builder
	replacedFormat := false
	for _, char := range value {
		if unicode.IsControl(char) || unicode.Is(unicode.Cf, char) {
			out.WriteRune('�')
			replacedFormat = true
			continue
		}
		out.WriteRune(char)
	}
	if replacedFormat {
		issues = append(issues, "display_name_control_or_format_replaced")
	}
	value = out.String()
	if strings.TrimSpace(value) == "" {
		return "(unnamed machine)", append(issues, "display_name_missing")
	}
	if len(value) > 256 {
		value = truncateMachineUTF8(value, 256)
		issues = append(issues, "display_name_truncated")
	}
	return value, issues
}

type machineDisplayReplacement struct {
	raw  string
	safe string
}

func machineDisplayReplacements(overview store.Overview) []machineDisplayReplacement {
	seen := make(map[string]bool, len(overview.Machines)+len(overview.Retired))
	result := make([]machineDisplayReplacement, 0, len(seen))
	add := func(raw string) {
		safe, _ := safeMachineDisplayName(raw)
		// Replacing a whitespace-only name would replace ordinary sentence
		// spacing. Exact display-name fields still become the unnamed placeholder;
		// derived prose receives the generic control/format scrub below.
		if raw == safe || strings.TrimSpace(raw) == "" || seen[raw] {
			return
		}
		seen[raw] = true
		result = append(result, machineDisplayReplacement{raw: raw, safe: safe})
	}
	for _, machine := range overview.Machines {
		add(machine.DisplayName)
	}
	for _, machine := range overview.Retired {
		add(machine.DisplayName)
	}
	sort.SliceStable(result, func(i, j int) bool { return len(result[i].raw) > len(result[j].raw) })
	return result
}

func safeMachineDerivedText(value string, replacements []machineDisplayReplacement) string {
	for _, replacement := range replacements {
		value = strings.ReplaceAll(value, replacement.raw, replacement.safe)
	}
	value = strings.ToValidUTF8(value, "�")
	var out strings.Builder
	for _, char := range value {
		if unicode.IsControl(char) || unicode.Is(unicode.Cf, char) {
			out.WriteRune('�')
			continue
		}
		out.WriteRune(char)
	}
	return out.String()
}

func safeMachineFacts(facts state.Facts) state.Facts {
	result := facts
	result.Credentials = append([]state.CredFact(nil), facts.Credentials...)
	for i := range result.Credentials {
		result.Credentials[i].Peers = append([]state.CredPeer(nil), facts.Credentials[i].Peers...)
		for j := range result.Credentials[i].Peers {
			result.Credentials[i].Peers[j].DisplayName, _ = safeMachineDisplayName(
				result.Credentials[i].Peers[j].DisplayName,
			)
		}
	}
	return result
}

// safeMachineOverview keeps the trusted HTML bridge's evidence richness while
// ensuring every registry display-name occurrence uses the exact same safe
// presentation projection as the JSON summary. Clone slices before rewriting:
// callers must never mutate Store-owned read models through this bridge.
func safeMachineOverview(overview store.Overview, creationCeiling int64) store.Overview {
	replacements := machineDisplayReplacements(overview)
	result := overview
	result.Total, result.Expected = 0, 0
	result.Counts = make(map[state.State]int, len(state.AllStates))
	result.Machines = make([]store.MachineRow, 0, len(overview.Machines))
	result.Retired = make([]store.Machine, 0, len(overview.Retired))
	result.Findings = make([]store.FleetFinding, 0, len(overview.Findings))
	for _, machine := range overview.Machines {
		if machine.RegistrySequence <= creationCeiling {
			result.Machines = append(result.Machines, machine)
		}
	}
	for _, machine := range overview.Retired {
		if machine.RegistrySequence <= creationCeiling {
			result.Retired = append(result.Retired, machine)
		}
	}
	names := make(map[string]string, len(result.Machines)+len(result.Retired))
	for i := range result.Machines {
		safe, _ := safeMachineDisplayName(result.Machines[i].DisplayName)
		result.Machines[i].Machine.DisplayName = safe
		result.Machines[i].Reason = safeMachineDerivedText(result.Machines[i].Reason, replacements)
		result.Machines[i].Facts = safeMachineFacts(result.Machines[i].Facts)
		result.Machines[i].Findings = append([]state.Finding(nil), result.Machines[i].Findings...)
		for j := range result.Machines[i].Findings {
			result.Machines[i].Findings[j].Message = safeMachineDerivedText(
				result.Machines[i].Findings[j].Message, replacements,
			)
		}
		names[result.Machines[i].MachineID] = safe
		result.Total++
		result.Counts[result.Machines[i].State]++
		if result.Machines[i].InDenominator() {
			result.Expected++
		}
	}
	for i := range result.Retired {
		safe, _ := safeMachineDisplayName(result.Retired[i].DisplayName)
		result.Retired[i].DisplayName = safe
		names[result.Retired[i].MachineID] = safe
	}
	for _, finding := range overview.Findings {
		safe, ok := names[finding.MachineID]
		if !ok {
			continue
		}
		finding.DisplayName = safe
		finding.Message = safeMachineDerivedText(finding.Message, replacements)
		result.Findings = append(result.Findings, finding)
	}
	return result
}

func truncateMachineUTF8(value string, maxBytes int) string {
	if len(value) <= maxBytes {
		return value
	}
	value = value[:maxBytes]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}

func currentStateSince(history []store.StateSpan, current state.State) *time.Time {
	// Detail history is newest-first. Match Overview/openStateSpan and retain
	// that order even if a corrupted legacy ledger contains multiple open rows.
	for _, span := range history {
		if span.LeftAt != nil {
			continue
		}
		if span.State != current || span.EnteredAt.IsZero() {
			return nil
		}
		entered := span.EnteredAt.UTC()
		return &entered
	}
	return nil
}

func knownMachineTime(value time.Time, known bool) *time.Time {
	if !known || value.IsZero() {
		return nil
	}
	value = value.UTC()
	return &value
}

func machineTime(value *time.Time) *time.Time {
	if value == nil || value.IsZero() {
		return nil
	}
	result := value.UTC()
	return &result
}

func machineString(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func normalizeMachineListRequest(request MachineListRequest) (MachineListRequest, error) {
	for name, field := range map[string]struct {
		value string
		max   int
	}{
		"machine_id": {request.MachineID, 256}, "display_name": {request.DisplayName, 256},
		"cursor": {request.Cursor, 2048},
	} {
		if field.value != "" {
			if err := validateMachineReadText(name, field.value, field.max); err != nil {
				return MachineListRequest{}, err
			}
		}
	}
	if request.Limit == 0 {
		request.Limit = DefaultMachineReadLimit
	}
	if request.Limit < 1 || request.Limit > MaxMachineReadLimit {
		return MachineListRequest{}, fmt.Errorf("%w: limit must be between 1 and %d", ErrInvalidMachineRead, MaxMachineReadLimit)
	}
	if request.Lifecycle == "" {
		request.Lifecycle = MachineLifecycleAny
	}
	if request.Reporting == "" {
		request.Reporting = MachineReportingAny
	}
	if request.Channel == "" {
		request.Channel = MachineChannelAny
	}
	if request.Lifecycle != MachineLifecycleAny && request.Lifecycle != MachineLifecycleActive && request.Lifecycle != MachineLifecycleRetired {
		return MachineListRequest{}, fmt.Errorf("%w: invalid lifecycle %q", ErrInvalidMachineRead, request.Lifecycle)
	}
	if request.Reporting != MachineReportingAny && request.Reporting != MachineReportingTrue &&
		request.Reporting != MachineReportingFalse && request.Reporting != MachineReportingUnknown {
		return MachineListRequest{}, fmt.Errorf("%w: invalid reporting filter %q", ErrInvalidMachineRead, request.Reporting)
	}
	if request.Channel != MachineChannelAny && request.Channel != MachineChannelNone &&
		request.Channel != MachineChannelCanary && request.Channel != MachineChannelStable {
		return MachineListRequest{}, fmt.Errorf("%w: invalid channel filter %q", ErrInvalidMachineRead, request.Channel)
	}
	seen := make(map[state.State]bool, len(request.States))
	for _, candidate := range request.States {
		if !knownMachineState(candidate) || seen[candidate] {
			return MachineListRequest{}, fmt.Errorf("%w: invalid or duplicate state %q", ErrInvalidMachineRead, candidate)
		}
		seen[candidate] = true
	}
	canonical := make([]state.State, 0, len(seen))
	for _, candidate := range state.AllStates {
		if seen[candidate] {
			canonical = append(canonical, candidate)
		}
	}
	request.States = canonical
	return request, nil
}

func validateMachineReadText(name, value string, maxBytes int) error {
	if value == "" || value != strings.TrimSpace(value) || len(value) > maxBytes || !utf8.ValidString(value) {
		return fmt.Errorf("%w: %s is empty, oversized, invalid UTF-8, or has surrounding whitespace", ErrInvalidMachineRead, name)
	}
	for _, char := range value {
		if unicode.IsControl(char) || unicode.Is(unicode.Cf, char) {
			return fmt.Errorf("%w: %s contains control or formatting characters", ErrInvalidMachineRead, name)
		}
	}
	return nil
}

func knownMachineState(candidate state.State) bool {
	for _, known := range state.AllStates {
		if candidate == known {
			return true
		}
	}
	return false
}

func machineMatchesFilters(item MachineSummary, request MachineListRequest) bool {
	if request.MachineID != "" && item.MachineID != request.MachineID {
		return false
	}
	if request.DisplayName != "" && item.DisplayName != request.DisplayName {
		return false
	}
	retired := item.RetiredAt != nil
	switch request.Lifecycle {
	case MachineLifecycleActive:
		if retired {
			return false
		}
	case MachineLifecycleRetired:
		if !retired {
			return false
		}
	}
	if len(request.States) > 0 {
		if item.State == nil {
			return false
		}
		selected := false
		for _, candidate := range request.States {
			selected = selected || *item.State == candidate
		}
		if !selected {
			return false
		}
	}
	switch request.Reporting {
	case MachineReportingTrue:
		if item.Reporting == nil || !*item.Reporting {
			return false
		}
	case MachineReportingFalse:
		if item.Reporting == nil || *item.Reporting {
			return false
		}
	case MachineReportingUnknown:
		if item.Reporting != nil {
			return false
		}
	}
	switch request.Channel {
	case MachineChannelNone:
		if item.Channel != nil {
			return false
		}
	case MachineChannelCanary:
		if item.Channel == nil || *item.Channel != "canary" {
			return false
		}
	case MachineChannelStable:
		if item.Channel == nil || *item.Channel != "stable" {
			return false
		}
	}
	return true
}

func machineReadPosition(item MachineSummary) MachineReadPosition {
	return MachineReadPosition{CreatedAt: item.CreatedAt.UTC(), MachineID: item.MachineID}
}

// compareMachineReadPosition follows the response order: newest immutable
// registry creation time first, then machine_id descending as a total tie-break.
// A negative result means a sorts before b.
func compareMachineReadPosition(a, b MachineReadPosition) int {
	if !a.CreatedAt.Equal(b.CreatedAt) {
		if a.CreatedAt.After(b.CreatedAt) {
			return -1
		}
		return 1
	}
	return -strings.Compare(a.MachineID, b.MachineID)
}

type machineListCursor struct {
	Version         int                 `json:"v"`
	FilterDigest    string              `json:"filter_digest"`
	CreationCeiling int64               `json:"creation_ceiling"`
	After           MachineReadPosition `json:"after"`
}

func machineFilterDigest(request MachineListRequest) string {
	body := struct {
		Version     int                    `json:"v"`
		MachineID   string                 `json:"machine_id"`
		DisplayName string                 `json:"display_name"`
		States      []state.State          `json:"states"`
		Lifecycle   MachineLifecycleFilter `json:"lifecycle"`
		Reporting   MachineReportingFilter `json:"reporting"`
		Channel     MachineChannelFilter   `json:"channel"`
	}{
		Version: MachineListReadSchemaVersion, MachineID: request.MachineID,
		DisplayName: request.DisplayName, States: request.States,
		Lifecycle: request.Lifecycle, Reporting: request.Reporting, Channel: request.Channel,
	}
	raw, _ := json.Marshal(body)
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func encodeMachineListCursor(cursor machineListCursor) (string, error) {
	raw, err := json.Marshal(cursor)
	if err != nil {
		return "", fmt.Errorf("operator: encode machine cursor: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func decodeMachineListCursor(encoded, filterDigest string) (machineListCursor, error) {
	if len(encoded) == 0 || len(encoded) > 2048 {
		return machineListCursor{}, fmt.Errorf("%w: cursor length is invalid", ErrInvalidMachineRead)
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || base64.RawURLEncoding.EncodeToString(raw) != encoded {
		return machineListCursor{}, fmt.Errorf("%w: cursor encoding is invalid", ErrInvalidMachineRead)
	}
	var cursor machineListCursor
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cursor); err != nil {
		return machineListCursor{}, fmt.Errorf("%w: cursor document is invalid", ErrInvalidMachineRead)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return machineListCursor{}, fmt.Errorf("%w: cursor has trailing JSON", ErrInvalidMachineRead)
	}
	canonical, err := encodeMachineListCursor(cursor)
	if err != nil || canonical != encoded {
		return machineListCursor{}, fmt.Errorf("%w: cursor is not canonical", ErrInvalidMachineRead)
	}
	_, offset := cursor.After.CreatedAt.Zone()
	if cursor.Version != MachineListReadSchemaVersion || cursor.FilterDigest != filterDigest ||
		cursor.CreationCeiling < 1 || cursor.After.CreatedAt.IsZero() || offset != 0 ||
		validateMachineReadText("cursor machine_id", cursor.After.MachineID, 256) != nil {
		return machineListCursor{}, fmt.Errorf("%w: cursor does not match this query", ErrInvalidMachineRead)
	}
	return cursor, nil
}
