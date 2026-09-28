package operator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/teddashh/AI-Intune/internal/store"
)

const (
	TicketReadSchemaVersion = 1
	TicketReadConsistency   = "fixed_inclusive_received_at_snapshot"
	DefaultTicketReadDays   = 7
	MaxTicketReadDays       = 30
	MaxTicketProviders      = 100
	MaxTicketMachines       = 50
	MaxTicketAgents         = 50
	MaxTicketErrors         = 5
)

var ErrInvalidTicketRead = errors.New("operator: invalid ticket read request")

type TicketReadRequest struct {
	Days        int
	ProviderRef string
}

type TicketReadWindow struct {
	From           time.Time `json:"from"`
	To             time.Time `json:"to"`
	Boundary       string    `json:"boundary"`
	TimeBasis      string    `json:"time_basis"`
	Days           int       `json:"days"`
	MaximumSeconds int64     `json:"maximum_seconds"`
}

type TicketRoster struct {
	Expected             int  `json:"expected"`
	Reporting            int  `json:"reporting"`
	ReportingRatePercent int  `json:"reporting_rate_percent"`
	SchedulingEligible   bool `json:"scheduling_eligible"`
}

type TicketEvidenceList struct {
	Total     int            `json:"total"`
	Truncated bool           `json:"truncated"`
	Items     []EvidenceText `json:"items"`
}

type TicketErrorEvidence struct {
	Text  EvidenceText `json:"text"`
	Count int          `json:"count"`
}

type TicketProviderItem struct {
	ProviderRef string             `json:"provider_ref"`
	Provider    EvidenceText       `json:"provider"`
	Machines    TicketEvidenceList `json:"machines"`
	Agents      TicketEvidenceList `json:"agents"`

	Runs        int        `json:"runs"`
	LastRunAt   *time.Time `json:"last_run_at"`
	PeakPerHour int        `json:"peak_per_hour"`
	PeakHour    *time.Time `json:"peak_hour"`

	ErrorRuns          int                   `json:"error_runs"`
	ErrorVariantsTotal int                   `json:"error_variants_total"`
	ErrorsTruncated    bool                  `json:"errors_truncated"`
	Errors             []TicketErrorEvidence `json:"errors"`
}

type TicketReadCoverage struct {
	MalformedMeasuredAtRows int      `json:"malformed_measured_at_rows"`
	InvalidOpenClawJSONRows int      `json:"invalid_openclaw_json_rows"`
	Issues                  []string `json:"issues"`
}

type TicketReadLimits struct {
	MaximumDays                int   `json:"maximum_days"`
	MaximumCandidateRows       int   `json:"maximum_candidate_rows"`
	MaximumCandidateBytes      int64 `json:"maximum_candidate_bytes"`
	MaximumProviders           int   `json:"maximum_providers"`
	MaximumMachinesPerProvider int   `json:"maximum_machines_per_provider"`
	MaximumAgentsPerProvider   int   `json:"maximum_agents_per_provider"`
	MaximumErrorsPerProvider   int   `json:"maximum_errors_per_provider"`
}

type TicketReadDisclosure struct {
	ProviderIdentity    string `json:"provider_identity"`
	ErrorClassification string `json:"error_classification"`
	RunOutcome          string `json:"run_outcome"`
	SchedulingRule      string `json:"scheduling_rule"`
}

type TicketReadResult struct {
	SchemaVersion     int                  `json:"schema_version"`
	Consistency       string               `json:"consistency"`
	EvaluatedAt       time.Time            `json:"evaluated_at"`
	Window            TicketReadWindow     `json:"window"`
	ProviderRef       string               `json:"provider_ref"`
	Roster            TicketRoster         `json:"roster"`
	CandidateRows     int                  `json:"candidate_rows"`
	CandidateBytes    int64                `json:"candidate_bytes"`
	Total             int                  `json:"total"`
	MatchedTotal      int                  `json:"matched_total"`
	Truncated         bool                 `json:"truncated"`
	Items             []TicketProviderItem `json:"items"`
	SkippedNoProvider int                  `json:"skipped_no_provider"`
	Coverage          TicketReadCoverage   `json:"coverage"`
	Limits            TicketReadLimits     `json:"limits"`
	Disclosure        TicketReadDisclosure `json:"disclosure"`
}

var (
	ticketProviderPolicy = evidenceTextPolicy{maxBytes: 256, policy: boundedTextStrict}
	ticketMachinePolicy  = evidenceTextPolicy{maxBytes: 256, policy: boundedTextStrict}
	ticketAgentPolicy    = evidenceTextPolicy{maxBytes: 256, policy: boundedTextStrict}
	ticketErrorPolicy    = evidenceTextPolicy{maxBytes: 2048, policy: boundedTextBlock}
)

func NormalizeTicketReadRequest(request TicketReadRequest) (TicketReadRequest, error) {
	if request.Days == 0 {
		request.Days = DefaultTicketReadDays
	}
	if request.Days < 1 || request.Days > MaxTicketReadDays {
		return TicketReadRequest{}, fmt.Errorf("%w: days must be between 1 and %d", ErrInvalidTicketRead, MaxTicketReadDays)
	}
	if request.ProviderRef != "" && !validTicketProviderRef(request.ProviderRef) {
		return TicketReadRequest{}, fmt.Errorf("%w: provider_ref must be a canonical sha256 reference", ErrInvalidTicketRead)
	}
	return request, nil
}

func ValidateTicketReadRequest(request TicketReadRequest) error {
	_, err := NormalizeTicketReadRequest(request)
	return err
}

func TicketProviderRef(provider string) string {
	digest := sha256.Sum256([]byte(provider))
	return "sha256:" + hex.EncodeToString(digest[:])
}

func validTicketProviderRef(value string) bool {
	if len(value) != len("sha256:")+sha256.Size*2 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	raw := strings.TrimPrefix(value, "sha256:")
	decoded, err := hex.DecodeString(raw)
	return err == nil && len(decoded) == sha256.Size && hex.EncodeToString(decoded) == raw
}

func (s *Service) ListTicketsContext(ctx context.Context, request TicketReadRequest, evaluatedAt time.Time) (TicketReadResult, error) {
	request, err := NormalizeTicketReadRequest(request)
	if err != nil {
		return TicketReadResult{}, err
	}
	if evaluatedAt.IsZero() {
		return TicketReadResult{}, fmt.Errorf("%w: evaluated_at is required", ErrInvalidTicketRead)
	}
	evaluatedAt = evaluatedAt.UTC().Truncate(time.Second)
	report, err := s.store.TicketLedgerContext(ctx, evaluatedAt, time.Duration(request.Days)*24*time.Hour)
	if err != nil {
		return TicketReadResult{}, err
	}

	result := TicketReadResult{
		SchemaVersion: TicketReadSchemaVersion,
		Consistency:   TicketReadConsistency,
		EvaluatedAt:   evaluatedAt,
		Window: TicketReadWindow{
			From: report.From, To: report.To, Boundary: "[from,to]", TimeBasis: "hub_received_at",
			Days: request.Days, MaximumSeconds: int64(store.TicketReadMaxWindow / time.Second),
		},
		ProviderRef: request.ProviderRef,
		Roster: TicketRoster{
			Expected: report.RosterSize, Reporting: report.ReportingSize,
			ReportingRatePercent: report.ReportingRate(), SchedulingEligible: report.EnoughToDiscussScheduling(),
		},
		CandidateRows: report.CandidateRows, CandidateBytes: report.CandidateBytes,
		Total: len(report.Rows), Items: []TicketProviderItem{},
		SkippedNoProvider: report.SkippedNoProvider,
		Coverage: TicketReadCoverage{
			MalformedMeasuredAtRows: report.MalformedMeasuredAt,
			InvalidOpenClawJSONRows: report.InvalidOpenClawJSON,
			Issues:                  []string{},
		},
		Limits: TicketReadLimits{
			MaximumDays: MaxTicketReadDays, MaximumCandidateRows: store.TicketReadMaxRows,
			MaximumCandidateBytes: store.TicketReadMaxBytes, MaximumProviders: MaxTicketProviders,
			MaximumMachinesPerProvider: MaxTicketMachines, MaximumAgentsPerProvider: MaxTicketAgents,
			MaximumErrorsPerProvider: MaxTicketErrors,
		},
		Disclosure: TicketReadDisclosure{
			ProviderIdentity:    "upstream_provider_text_not_ticket_profile",
			ErrorClassification: "raw_error_text_not_429_401_classification",
			RunOutcome:          "completed_run_not_verified_success",
			SchedulingRule:      "reporting_rate_at_least_80_percent",
		},
	}
	if report.MalformedMeasuredAt > 0 {
		result.Coverage.Issues = append(result.Coverage.Issues, "malformed_measured_at_excluded_from_time_fields")
	}
	if report.InvalidOpenClawJSON > 0 {
		result.Coverage.Issues = append(result.Coverage.Issues, "invalid_openclaw_json_excluded_from_skipped_count")
	}

	for _, row := range report.Rows {
		providerRef := TicketProviderRef(row.Provider)
		if request.ProviderRef != "" && providerRef != request.ProviderRef {
			continue
		}
		result.MatchedTotal++
		if len(result.Items) >= MaxTicketProviders {
			result.Truncated = true
			continue
		}
		item := TicketProviderItem{
			ProviderRef: providerRef,
			Provider:    projectEvidenceText(ticketProviderPolicy, row.Provider),
			Machines:    projectTicketEvidenceList(row.Machines, MaxTicketMachines, ticketMachinePolicy),
			Agents:      projectTicketEvidenceList(row.Agents, MaxTicketAgents, ticketAgentPolicy),
			Runs:        row.Runs, PeakPerHour: row.PeakPerHour, ErrorRuns: row.ErrorRuns,
			ErrorVariantsTotal: row.ErrorVariantsTotal,
			ErrorsTruncated:    row.ErrorVariantsTotal > len(row.Errors),
			Errors:             []TicketErrorEvidence{},
		}
		if !row.LastRunAt.IsZero() {
			value := row.LastRunAt.UTC()
			item.LastRunAt = &value
		}
		if !row.PeakHour.IsZero() {
			value := row.PeakHour.UTC()
			item.PeakHour = &value
		}
		for _, evidence := range row.Errors {
			item.Errors = append(item.Errors, TicketErrorEvidence{
				Text: projectEvidenceText(ticketErrorPolicy, evidence.Text), Count: evidence.Count,
			})
		}
		result.Items = append(result.Items, item)
	}
	return result, nil
}

func projectTicketEvidenceList(values []string, limit int, policy evidenceTextPolicy) TicketEvidenceList {
	values = append([]string(nil), values...)
	sort.Strings(values)
	result := TicketEvidenceList{Total: len(values), Items: []EvidenceText{}}
	if len(values) > limit {
		values = values[:limit]
		result.Truncated = true
	}
	for _, value := range values {
		result.Items = append(result.Items, projectEvidenceText(policy, value))
	}
	return result
}

func (result TicketReadResult) TotalRuns() int {
	total := 0
	for _, item := range result.Items {
		total += item.Runs
	}
	return total
}

func (result TicketReadResult) TotalErrorRuns() int {
	total := 0
	for _, item := range result.Items {
		total += item.ErrorRuns
	}
	return total
}

// TicketCSV renders only the bounded operator projection. Text cells are
// neutralized before CSV quoting so a provider-controlled leading character
// cannot become a spreadsheet formula when the export is opened.
// TicketCSV 是票證使用量的匯出。它走 ReportCSV，跟每一份匯出共用同一套欄位
// 檢查、同一種時間寫法與同一道公式防護。
// ⚠ 欄名寫的是「距上次跑完」不是「距上次成功」——我們沒有成功這個資料。
func TicketCSV(result TicketReadResult) ReportCSVDocument {
	doc := ReportCSVDocument{
		Filename: "ai-intune-ticket-usage.csv",
		Columns: []ReportCSVColumn{
			{Key: "provider_ref", Header: "provider_ref"},
			{Key: "provider", Header: "provider（上游文字）"},
			{Key: "machines", Header: "機器"},
			{Key: "agents", Header: "代理程式數"},
			{Key: "runs", Header: "跑完回合"},
			{Key: "peak_per_hour", Header: "尖峰每小時"},
			{Key: "peak_hour", Header: "尖峰時段（UTC）"},
			{Key: "since_last_run", Header: "距上次跑完"},
			{Key: "last_run_at", Header: "上次跑完（UTC）"},
			{Key: "error_runs", Header: "有錯誤的回合"},
			{Key: "top_error", Header: "最常見的錯誤原文"},
			{Key: "evidence_truncated", Header: "證據截斷"},
		},
	}
	for _, item := range result.Items {
		machines := make([]string, 0, len(item.Machines.Items))
		for _, machine := range item.Machines.Items {
			machines = append(machines, machine.Text)
		}
		since, topError := "", ""
		if item.LastRunAt != nil {
			since = humanTicketDuration(result.EvaluatedAt.Sub(item.LastRunAt.UTC()))
		}
		if len(item.Errors) > 0 {
			topError = fmt.Sprintf("%s ×%d", item.Errors[0].Text.Text, item.Errors[0].Count)
		}
		truncated := item.Provider.Truncated || item.Machines.Truncated || item.Agents.Truncated || item.ErrorsTruncated
		doc.Rows = append(doc.Rows, []string{
			item.ProviderRef, item.Provider.Text, strings.Join(machines, " "),
			fmt.Sprint(item.Agents.Total), fmt.Sprint(item.Runs), fmt.Sprint(item.PeakPerHour),
			ReportCSVOptionalTime(item.PeakHour), since, ReportCSVOptionalTime(item.LastRunAt),
			fmt.Sprint(item.ErrorRuns), topError, fmt.Sprint(truncated),
		})
	}
	return doc
}

func humanTicketDuration(value time.Duration) string {
	if value < 0 {
		return "agent 時間晚於 Hub 評估時間"
	}
	switch {
	case value < time.Hour:
		return fmt.Sprintf("%d 分鐘", int(value.Minutes()))
	case value < 48*time.Hour:
		return fmt.Sprintf("%d 小時", int(value.Hours()))
	default:
		return fmt.Sprintf("%d 天", int(value.Hours()/24))
	}
}
