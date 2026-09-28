package operatorclient

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/store"
)

// A full ticket page can contain 100 providers, each with bounded machine,
// agent, and error evidence. Eight MiB covers that declared shape while still
// rejecting an accidental ledger dump before decoding it.
const maxTicketResponseBytes int64 = 8 << 20

// Tickets reads the bounded ticket-usage projection through the authenticated
// operator API. It deliberately never exposes the legacy Store report type.
func (c *Client) Tickets(ctx context.Context, request operator.TicketReadRequest) (operator.TicketReadResult, error) {
	normalized, err := operator.NormalizeTicketReadRequest(request)
	if err != nil {
		return operator.TicketReadResult{}, fmt.Errorf("operator client: invalid tickets request: %w", err)
	}
	query := make(url.Values)
	if request.Days != 0 {
		query.Set("days", strconv.Itoa(normalized.Days))
	}
	if normalized.ProviderRef != "" {
		query.Set("provider_ref", normalized.ProviderRef)
	}
	path := "/v1/operator/tickets"
	if encoded := query.Encode(); encoded != "" {
		path += "?" + encoded
	}
	req, err := c.newOperatorRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return operator.TicketReadResult{}, err
	}
	response, err := c.doRawWithLimit(req, maxTicketResponseBytes, "8 MiB")
	if err != nil {
		return operator.TicketReadResult{}, err
	}
	if response.status != http.StatusOK {
		return operator.TicketReadResult{}, fmt.Errorf(
			"operator client: tickets returned unexpected success status HTTP %d", response.status)
	}
	if err := validateTicketReadHeaders(response.header); err != nil {
		return operator.TicketReadResult{}, err
	}
	var result operator.TicketReadResult
	if err := decodeStrictJSONDocument(response.body, "tickets", &result); err != nil {
		return operator.TicketReadResult{}, err
	}
	if err := validateTicketReadResult(result, normalized); err != nil {
		return operator.TicketReadResult{}, err
	}
	return result, nil
}

func (c *Client) ListTickets(ctx context.Context, request operator.TicketReadRequest) (operator.TicketReadResult, error) {
	return c.Tickets(ctx, request)
}

func validateTicketReadHeaders(header http.Header) error {
	if err := validateJSONNoStoreResponse(header); err != nil {
		return err
	}
	if replayed, err := responseReplayEvidenceFromHeader(header); err != nil {
		return err
	} else if replayed {
		return errors.New("operator client: tickets read cannot be an idempotency replay")
	}
	if len(header.Values("ETag")) != 0 {
		return errors.New("operator client: composite tickets read must not carry an ETag")
	}
	return nil
}

func validateTicketReadResult(result operator.TicketReadResult, request operator.TicketReadRequest) error {
	if result.SchemaVersion != operator.TicketReadSchemaVersion || result.Consistency != operator.TicketReadConsistency {
		return fmt.Errorf("operator client: unsupported tickets schema_version %d or consistency %q",
			result.SchemaVersion, result.Consistency)
	}
	if err := validateTicketTime(result.EvaluatedAt, "evaluated_at", true); err != nil {
		return err
	}
	if err := validateTicketWindow(result.Window, result.EvaluatedAt, request.Days); err != nil {
		return err
	}
	if result.ProviderRef != request.ProviderRef {
		return errors.New("operator client: tickets provider_ref contradicts request")
	}
	if result.Items == nil || result.Coverage.Issues == nil {
		return errors.New("operator client: tickets arrays must not be null")
	}
	if result.Limits != (operator.TicketReadLimits{
		MaximumDays: operator.MaxTicketReadDays, MaximumCandidateRows: store.TicketReadMaxRows,
		MaximumCandidateBytes: store.TicketReadMaxBytes, MaximumProviders: operator.MaxTicketProviders,
		MaximumMachinesPerProvider: operator.MaxTicketMachines,
		MaximumAgentsPerProvider:   operator.MaxTicketAgents,
		MaximumErrorsPerProvider:   operator.MaxTicketErrors,
	}) {
		return errors.New("operator client: tickets limits contradict the declared contract")
	}
	if result.Disclosure != (operator.TicketReadDisclosure{
		ProviderIdentity:    "upstream_provider_text_not_ticket_profile",
		ErrorClassification: "raw_error_text_not_429_401_classification",
		RunOutcome:          "completed_run_not_verified_success",
		SchedulingRule:      "reporting_rate_at_least_80_percent",
	}) {
		return errors.New("operator client: tickets disclosure contract is invalid")
	}
	if err := validateTicketRoster(result.Roster); err != nil {
		return err
	}
	if result.CandidateRows < 0 || result.CandidateRows > result.Limits.MaximumCandidateRows ||
		result.CandidateBytes < 0 || result.CandidateBytes > result.Limits.MaximumCandidateBytes ||
		result.Total < 0 || result.Total > result.CandidateRows || result.MatchedTotal < 0 ||
		result.MatchedTotal > result.Total || result.SkippedNoProvider < 0 {
		return errors.New("operator client: tickets counts exceed their declared bounds")
	}
	if request.ProviderRef == "" {
		if result.MatchedTotal != result.Total {
			return errors.New("operator client: unfiltered tickets matched_total is inconsistent")
		}
	} else if result.MatchedTotal > 1 {
		return errors.New("operator client: filtered tickets result contains multiple providers")
	}
	wantItems := result.MatchedTotal
	if wantItems > result.Limits.MaximumProviders {
		wantItems = result.Limits.MaximumProviders
	}
	if len(result.Items) != wantItems || result.Truncated != (result.MatchedTotal > len(result.Items)) {
		return errors.New("operator client: tickets item count or truncation evidence is inconsistent")
	}
	if err := validateTicketCoverage(result.Coverage, result.CandidateRows); err != nil {
		return err
	}

	seen := make(map[string]bool, len(result.Items))
	for index, item := range result.Items {
		if err := validateTicketProviderItem(item, result.Window, request.ProviderRef); err != nil {
			return fmt.Errorf("operator client: tickets item %d: %w", index, err)
		}
		if seen[item.ProviderRef] {
			return errors.New("operator client: tickets contains a duplicate provider_ref")
		}
		seen[item.ProviderRef] = true
		if index > 0 && result.Items[index-1].Runs < item.Runs {
			return errors.New("operator client: tickets providers are not ordered by descending runs")
		}
	}
	return nil
}

func validateTicketWindow(window operator.TicketReadWindow, evaluatedAt time.Time, days int) error {
	if err := validateTicketTime(window.From, "window.from", true); err != nil {
		return err
	}
	if err := validateTicketTime(window.To, "window.to", true); err != nil {
		return err
	}
	if !window.To.Equal(evaluatedAt) || !window.From.Equal(window.To.Add(-time.Duration(days)*24*time.Hour)) ||
		window.Boundary != "[from,to]" || window.TimeBasis != "hub_received_at" || window.Days != days ||
		window.MaximumSeconds != int64(store.TicketReadMaxWindow/time.Second) {
		return errors.New("operator client: tickets fixed window contract is invalid")
	}
	return nil
}

func validateTicketRoster(roster operator.TicketRoster) error {
	if roster.Expected < 0 || roster.Reporting < 0 || roster.Reporting > roster.Expected {
		return errors.New("operator client: tickets roster counts are invalid")
	}
	rate := 0
	if roster.Expected > 0 {
		rate = roster.Reporting * 100 / roster.Expected
	}
	if roster.ReportingRatePercent != rate || roster.SchedulingEligible != (rate >= 80) {
		return errors.New("operator client: tickets roster rate or scheduling rule is inconsistent")
	}
	return nil
}

func validateTicketCoverage(coverage operator.TicketReadCoverage, candidateRows int) error {
	if coverage.MalformedMeasuredAtRows < 0 || coverage.MalformedMeasuredAtRows > candidateRows ||
		coverage.InvalidOpenClawJSONRows < 0 {
		return errors.New("operator client: tickets coverage counts are invalid")
	}
	want := []string{}
	if coverage.MalformedMeasuredAtRows > 0 {
		want = append(want, "malformed_measured_at_excluded_from_time_fields")
	}
	if coverage.InvalidOpenClawJSONRows > 0 {
		want = append(want, "invalid_openclaw_json_excluded_from_skipped_count")
	}
	if !equalStringLists(coverage.Issues, want) {
		return errors.New("operator client: tickets coverage issues contradict counts")
	}
	return nil
}

func validateTicketProviderItem(item operator.TicketProviderItem, window operator.TicketReadWindow, filter string) error {
	if !validSHA256Digest(item.ProviderRef) || filter != "" && item.ProviderRef != filter {
		return errors.New("provider_ref is invalid or contradicts the filter")
	}
	if err := validateEvidenceText(item.Provider, 256, "tickets.provider", evidenceTextFieldPolicy{256, evidenceTextStrict}); err != nil {
		return err
	}
	if len(item.Provider.Issues) == 0 && item.ProviderRef != operator.TicketProviderRef(item.Provider.Text) {
		return errors.New("provider_ref does not bind the represented provider")
	}
	if err := validateTicketEvidenceList(item.Machines, operator.MaxTicketMachines, "machines"); err != nil {
		return err
	}
	if err := validateTicketEvidenceList(item.Agents, operator.MaxTicketAgents, "agents"); err != nil {
		return err
	}
	if item.Runs < 1 || item.Machines.Total < 1 || item.ErrorRuns < 0 || item.ErrorRuns > item.Runs || item.PeakPerHour < 0 ||
		item.PeakPerHour > item.Runs || item.Errors == nil || item.ErrorVariantsTotal < 0 ||
		item.ErrorVariantsTotal < len(item.Errors) || len(item.Errors) > operator.MaxTicketErrors ||
		item.ErrorsTruncated != (item.ErrorVariantsTotal > len(item.Errors)) {
		return errors.New("provider counts or evidence arrays are invalid")
	}
	if item.LastRunAt != nil {
		if err := validateTicketTime(*item.LastRunAt, "last_run_at", false); err != nil {
			return err
		}
	}
	if item.PeakHour != nil {
		if err := validateTicketTime(*item.PeakHour, "peak_hour", true); err != nil {
			return err
		}
		if item.PeakHour.Minute() != 0 || item.PeakHour.Second() != 0 {
			return errors.New("peak_hour is not aligned to an hour")
		}
	}
	if (item.PeakPerHour == 0) != (item.PeakHour == nil) ||
		(item.LastRunAt == nil) != (item.PeakHour == nil) {
		return errors.New("provider time evidence is inconsistent")
	}
	countedErrors := 0
	for index, evidence := range item.Errors {
		if evidence.Count < 1 {
			return errors.New("error evidence has a non-positive count")
		}
		if err := validateEvidenceText(evidence.Text, 2048, "tickets.error", evidenceTextFieldPolicy{2048, evidenceTextBlock}); err != nil {
			return err
		}
		if index > 0 && item.Errors[index-1].Count < evidence.Count {
			return errors.New("error evidence is not ordered by descending count")
		}
		if countedErrors > item.ErrorRuns-evidence.Count {
			return errors.New("error evidence exceeds error_runs")
		}
		countedErrors += evidence.Count
	}
	if !item.ErrorsTruncated && countedErrors != item.ErrorRuns {
		return errors.New("complete error evidence does not sum to error_runs")
	}
	_ = window // Agent-measured timestamps are evidence and may legitimately fall outside receive time.
	return nil
}

func validateTicketEvidenceList(list operator.TicketEvidenceList, limit int, name string) error {
	if list.Total < 0 || list.Items == nil || len(list.Items) > limit || len(list.Items) > list.Total ||
		list.Truncated != (list.Total > len(list.Items)) {
		return fmt.Errorf("operator client: tickets %s list is inconsistent", name)
	}
	for index, item := range list.Items {
		if err := validateEvidenceText(item, 256, "tickets."+name, evidenceTextFieldPolicy{256, evidenceTextStrict}); err != nil {
			return err
		}
		if index > 0 && len(list.Items[index-1].Issues) == 0 && len(item.Issues) == 0 &&
			list.Items[index-1].Text >= item.Text {
			return fmt.Errorf("operator client: tickets %s are not unique and sorted", name)
		}
	}
	return nil
}

func validateTicketTime(value time.Time, name string, secondPrecision bool) error {
	if value.IsZero() {
		return fmt.Errorf("operator client: tickets %s is missing", name)
	}
	if _, offset := value.Zone(); offset != 0 {
		return fmt.Errorf("operator client: tickets %s must be UTC", name)
	}
	if secondPrecision && value.Nanosecond() != 0 {
		return fmt.Errorf("operator client: tickets %s must use second precision", name)
	}
	return nil
}
