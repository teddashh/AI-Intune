package operatorclient

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/teddashh/AI-Intune/internal/deploy"
	"github.com/teddashh/AI-Intune/internal/operator"
)

func (c *Client) Jobs(ctx context.Context, request operator.JobListRequest) (operator.JobListResult, error) {
	query, effectiveLimit, err := encodeJobListQuery(request)
	if err != nil {
		return operator.JobListResult{}, err
	}
	path := "/v1/operator/jobs"
	if encoded := query.Encode(); encoded != "" {
		path += "?" + encoded
	}
	req, err := c.newOperatorRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return operator.JobListResult{}, err
	}
	response, err := c.doRaw(req)
	if err != nil {
		return operator.JobListResult{}, err
	}
	if response.status != http.StatusOK {
		return operator.JobListResult{}, fmt.Errorf(
			"operator client: job list returned unexpected success status HTTP %d", response.status)
	}
	if err := validateJobReadHeaders(response.header); err != nil {
		return operator.JobListResult{}, err
	}
	var result operator.JobListResult
	if err := decodeStrictJSONDocument(response.body, "job list", &result); err != nil {
		return operator.JobListResult{}, err
	}
	if err := validateJobListResult(result, request, effectiveLimit); err != nil {
		return operator.JobListResult{}, err
	}
	return result, nil
}

func (c *Client) Job(ctx context.Context, jobID string) (operator.JobDetailResult, error) {
	if err := validateJobReadIdentifier("job_id", jobID, 256); err != nil ||
		strings.Contains(jobID, "/") || jobID == "." || jobID == ".." {
		return operator.JobDetailResult{}, errors.New(
			"operator client: job_id cannot be empty, contain control characters, dot segments, or slashes")
	}
	path := "/v1/operator/jobs/" + url.PathEscape(jobID)
	req, err := c.newOperatorRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return operator.JobDetailResult{}, err
	}
	response, err := c.doRaw(req)
	if err != nil {
		return operator.JobDetailResult{}, err
	}
	if response.status != http.StatusOK {
		return operator.JobDetailResult{}, fmt.Errorf(
			"operator client: job detail returned unexpected success status HTTP %d", response.status)
	}
	if err := validateJobReadHeaders(response.header); err != nil {
		return operator.JobDetailResult{}, err
	}
	var result operator.JobDetailResult
	if err := decodeStrictJSONDocument(response.body, "job detail", &result); err != nil {
		return operator.JobDetailResult{}, err
	}
	if err := validateJobDetailResult(result, jobID); err != nil {
		return operator.JobDetailResult{}, err
	}
	return result, nil
}

func encodeJobListQuery(request operator.JobListRequest) (url.Values, int, error) {
	query := make(url.Values)
	for _, field := range []struct {
		name  string
		value string
		max   int
	}{
		{"machine_id", request.MachineID, 256},
		{"deployment_id", request.DeploymentID, 256},
		{"resource_kind", request.ResourceKind, 128},
		{"resource_id", request.ResourceID, 256},
		{"cursor", request.Cursor, 2048},
	} {
		if field.value == "" {
			continue
		}
		if err := validateJobReadIdentifier(field.name, field.value, field.max); err != nil {
			return nil, 0, err
		}
		query.Set(field.name, field.value)
	}
	if request.ResourceID != "" && request.ResourceKind == "" {
		return nil, 0, errors.New("operator client: resource_id requires resource_kind")
	}
	limit := request.Limit
	if limit == 0 {
		limit = operator.DefaultJobReadLimit
	} else {
		if limit < 1 || limit > operator.MaxJobReadLimit {
			return nil, 0, fmt.Errorf("operator client: job list limit must be between 1 and %d", operator.MaxJobReadLimit)
		}
		query.Set("limit", strconv.Itoa(limit))
	}
	seen := make(map[deploy.JobState]bool, len(request.States))
	for _, state := range request.States {
		if !deploy.IsKnownJobState(state) || seen[state] {
			return nil, 0, fmt.Errorf("operator client: invalid or duplicate job state %q", state)
		}
		seen[state] = true
		query.Add("state", string(state))
	}
	return query, limit, nil
}

func validateJobReadIdentifier(name, value string, maxBytes int) error {
	if value == "" || value != strings.TrimSpace(value) || len(value) > maxBytes {
		return fmt.Errorf("operator client: %s cannot be empty, too long, or contain leading/trailing whitespace", name)
	}
	for _, char := range value {
		if unicode.IsControl(char) || unicode.Is(unicode.Cf, char) {
			return fmt.Errorf("operator client: %s cannot contain control or invisible formatting characters", name)
		}
	}
	return nil
}

func validateJobReadHeaders(header http.Header) error {
	if err := validateJSONNoStoreResponse(header); err != nil {
		return err
	}
	if replayed, err := responseReplayEvidenceFromHeader(header); err != nil {
		return err
	} else if replayed {
		return errors.New("operator client: job read cannot be an idempotency replay")
	}
	if values := header.Values("ETag"); len(values) != 0 {
		return errors.New("operator client: composite job read must not carry an ETag")
	}
	return nil
}

func validateJobListResult(result operator.JobListResult, request operator.JobListRequest, limit int) error {
	if result.SchemaVersion != operator.JobReadSchemaVersion {
		return fmt.Errorf("operator client: unsupported job read schema_version %d", result.SchemaVersion)
	}
	if err := validateJobEvaluationTime(result.EvaluatedAt); err != nil {
		return err
	}
	if result.Items == nil || result.StateCounts == nil {
		return errors.New("operator client: job list arrays must not be null")
	}
	if result.Total < 0 || result.Total < len(result.Items) || len(result.Items) > limit {
		return errors.New("operator client: inconsistent job list total or page size")
	}
	if err := validateJobStateCounts(result.StateCounts, result.Total, request.States); err != nil {
		return err
	}
	if result.NextCursor != nil {
		if err := validateJobReadIdentifier("next_cursor", *result.NextCursor, 2048); err != nil {
			return err
		}
		if len(result.Items) != limit {
			return errors.New("operator client: job list cursor is inconsistent with page size")
		}
		if result.Total <= len(result.Items) || *result.NextCursor == request.Cursor {
			return errors.New("operator client: job list cursor cannot advance this traversal")
		}
	}
	if request.Cursor == "" && (result.Total > len(result.Items)) != (result.NextCursor != nil) {
		return errors.New("operator client: first job page has inconsistent continuation evidence")
	}
	seen := make(map[string]bool, len(result.Items))
	pageStateCounts := make(map[deploy.JobState]int, len(deploy.AllJobStates))
	for i, item := range result.Items {
		if err := validateJobSummary(item, result.EvaluatedAt); err != nil {
			return fmt.Errorf("operator client: job list item %d: %w", i, err)
		}
		if seen[item.JobID] {
			return fmt.Errorf("operator client: duplicate job_id %q", item.JobID)
		}
		seen[item.JobID] = true
		pageStateCounts[item.State]++
		if request.MachineID != "" && item.MachineID != request.MachineID {
			return errors.New("operator client: job item does not match machine_id filter")
		}
		if request.DeploymentID != "" && (item.DeploymentID == nil || *item.DeploymentID != request.DeploymentID) {
			return errors.New("operator client: job item does not match deployment_id filter")
		}
		if request.ResourceKind != "" && item.ResourceKind != request.ResourceKind {
			return errors.New("operator client: job item does not match resource_kind filter")
		}
		if request.ResourceID != "" && item.ResourceID != request.ResourceID {
			return errors.New("operator client: job item does not match resource_id filter")
		}
		if len(request.States) > 0 && !jobStateSelected(item.State, request.States) {
			return errors.New("operator client: job item does not match state filter")
		}
		if i > 0 && !jobSummaryBefore(result.Items[i-1], item) {
			return errors.New("operator client: job list is not in canonical keyset order")
		}
	}
	for i, state := range deploy.AllJobStates {
		if pageStateCounts[state] > result.StateCounts[i].Count {
			return errors.New("operator client: job list state counts contradict page items")
		}
	}
	if result.Total == 0 && (len(result.Items) != 0 || result.NextCursor != nil) {
		return errors.New("operator client: empty job list carries items or a cursor")
	}
	return nil
}

func validateJobDetailResult(result operator.JobDetailResult, jobID string) error {
	if result.SchemaVersion != operator.JobReadSchemaVersion {
		return fmt.Errorf("operator client: unsupported job read schema_version %d", result.SchemaVersion)
	}
	if err := validateJobEvaluationTime(result.EvaluatedAt); err != nil {
		return err
	}
	if err := validateJobSummary(result.Item, result.EvaluatedAt); err != nil {
		return err
	}
	if result.Item.JobID != jobID {
		return errors.New("operator client: job detail response job_id does not match request")
	}
	if result.Desired.DesiredID != result.Item.DesiredID ||
		result.Desired.ResourceKind != result.Item.ResourceKind ||
		result.Desired.ResourceID != result.Item.ResourceID ||
		result.Desired.Revision != result.Item.Revision ||
		(result.Desired.ScopeType != "machine" && result.Desired.ScopeType != "channel") ||
		(result.Desired.ScopeType == "machine" && result.Desired.ScopeID != result.Item.MachineID) ||
		strings.TrimSpace(result.Desired.ScopeID) == "" || result.Desired.CreatedAt.IsZero() {
		return errors.New("operator client: job detail desired metadata is inconsistent")
	}
	if err := validateJobReadIdentifier("desired.scope_id", result.Desired.ScopeID, 256); err != nil {
		return err
	}
	if err := requireUTCJobTime(result.Desired.CreatedAt, "desired.created_at"); err != nil {
		return err
	}
	if result.Events.Items == nil || result.Verifications.Items == nil {
		return errors.New("operator client: job detail evidence arrays must not be null")
	}
	if result.Events.Total != result.Item.EventCount ||
		!validBoundedJobEvidencePage(result.Events.Total, len(result.Events.Items), result.Events.Truncated) {
		return errors.New("operator client: job event page totals are inconsistent")
	}
	if result.Verifications.Total != result.Item.VerificationTotal ||
		result.Verifications.Passed != result.Item.VerificationPassed ||
		result.Verifications.Failed != result.Item.VerificationFailed ||
		!validBoundedJobEvidencePage(result.Verifications.Total,
			len(result.Verifications.Items), result.Verifications.Truncated) {
		return errors.New("operator client: job verification page totals are inconsistent")
	}
	seenEventIDs := make(map[string]bool, len(result.Events.Items))
	for i, event := range result.Events.Items {
		if validateJobReadIdentifier("event.event_id", event.EventID, 256) != nil || event.Seq < 0 ||
			!validJobEventPhase(event.Phase) || event.OccurredAt.IsZero() || event.ReceivedAt.IsZero() {
			return errors.New("operator client: job detail contains invalid event metadata")
		}
		if err := requireUTCJobTime(event.OccurredAt, "event.occurred_at"); err != nil {
			return err
		}
		if err := requireUTCJobTime(event.ReceivedAt, "event.received_at"); err != nil {
			return err
		}
		if seenEventIDs[event.EventID] {
			return errors.New("operator client: job detail contains a duplicate event_id")
		}
		seenEventIDs[event.EventID] = true
		if i > 0 {
			previous := result.Events.Items[i-1]
			if event.Seq <= previous.Seq {
				return errors.New("operator client: job events are not in canonical order")
			}
		}
	}
	returnedPassed, returnedFailed := 0, 0
	seenVerificationIDs := make(map[string]bool, len(result.Verifications.Items))
	for i, verification := range result.Verifications.Items {
		if validateJobReadIdentifier("verification.verification_id", verification.VerificationID, 256) != nil ||
			verification.ReportedVerifiedAt.IsZero() {
			return errors.New("operator client: job detail contains invalid verification metadata")
		}
		if err := requireUTCJobTime(verification.ReportedVerifiedAt, "verification.reported_verified_at"); err != nil {
			return err
		}
		if seenVerificationIDs[verification.VerificationID] {
			return errors.New("operator client: job detail contains a duplicate verification_id")
		}
		seenVerificationIDs[verification.VerificationID] = true
		if verification.Passed {
			returnedPassed++
		} else {
			returnedFailed++
		}
		if i > 0 {
			previous := result.Verifications.Items[i-1]
			if verification.ReportedVerifiedAt.Before(previous.ReportedVerifiedAt) ||
				(verification.ReportedVerifiedAt.Equal(previous.ReportedVerifiedAt) &&
					verification.VerificationID <= previous.VerificationID) {
				return errors.New("operator client: job verifications are not in canonical order")
			}
		}
	}
	if (!result.Verifications.Truncated &&
		(returnedPassed != result.Verifications.Passed || returnedFailed != result.Verifications.Failed)) ||
		(result.Verifications.Truncated &&
			(returnedPassed > result.Verifications.Passed || returnedFailed > result.Verifications.Failed)) {
		return errors.New("operator client: returned verification outcomes exceed totals")
	}
	return nil
}

func validBoundedJobEvidencePage(total, returned int, truncated bool) bool {
	expected := total
	if expected > operator.JobDetailEvidenceLimit {
		expected = operator.JobDetailEvidenceLimit
	}
	return total >= 0 && returned == expected &&
		truncated == (total > operator.JobDetailEvidenceLimit)
}

func validateJobSummary(item operator.JobSummary, evaluatedAt time.Time) error {
	for _, field := range []struct {
		name, value string
		max         int
	}{
		{"job.job_id", item.JobID, 256},
		{"job.machine_id", item.MachineID, 256},
		{"job.display_name", item.DisplayName, 256},
		{"job.desired_id", item.DesiredID, 256},
		{"job.resource_kind", item.ResourceKind, 128},
		{"job.resource_id", item.ResourceID, 256},
	} {
		if err := validateJobReadIdentifier(field.name, field.value, field.max); err != nil {
			return err
		}
	}
	if strings.Contains(item.JobID, "/") || item.JobID == "." || item.JobID == ".." {
		return errors.New("operator client: job.job_id is not route-safe")
	}
	if item.Revision < 0 || item.ExecutionTimeoutSeconds <= 0 || item.CreatedAt.IsZero() ||
		!deploy.IsKnownJobState(item.State) {
		return errors.New("job summary is missing canonical identity or state fields")
	}
	if item.DeploymentID != nil {
		if err := validateJobReadIdentifier("job.deployment_id", *item.DeploymentID, 256); err != nil {
			return err
		}
	}
	if err := requireUTCJobTime(item.CreatedAt, "job.created_at"); err != nil {
		return err
	}
	for name, value := range map[string]*time.Time{
		"job.terminal_at": item.TerminalAt, "job.lease_expires_at": item.LeaseExpiresAt,
		"job.last_event_received_at": item.LastEventReceivedAt,
	} {
		if value != nil {
			if value.IsZero() {
				return fmt.Errorf("operator client: %s is zero", name)
			}
			if err := requireUTCJobTime(*value, name); err != nil {
				return err
			}
		}
	}
	switch {
	case item.ArtifactDigest == nil && item.ArtifactDigestStatus != operator.JobArtifactDigestNotRecorded:
		return errors.New("operator client: missing artifact digest contradicts artifact_digest_status")
	case item.ArtifactDigest != nil && item.ArtifactDigestStatus != operator.JobArtifactDigestRecorded:
		return errors.New("operator client: artifact digest contradicts artifact_digest_status")
	case item.ArtifactDigest != nil && !validOperatorJobDigest(*item.ArtifactDigest):
		return errors.New("operator client: job summary has a non-canonical artifact digest")
	}
	if item.EventCount < 0 || item.VerificationTotal < 0 || item.VerificationPassed < 0 ||
		item.VerificationFailed < 0 || item.VerificationPassed+item.VerificationFailed != item.VerificationTotal ||
		(item.EventCount == 0) != (item.LastEventReceivedAt == nil) {
		return errors.New("operator client: job summary evidence totals are inconsistent")
	}
	if item.State == deploy.Succeeded && (item.VerificationTotal == 0 || item.VerificationFailed != 0) {
		return errors.New("operator client: succeeded job lacks an all-passing verification set")
	}
	if (item.State == deploy.Failed && item.Irreversible) ||
		(item.State == deploy.ManualIntervention && !item.Irreversible) {
		return errors.New("operator client: failed job state contradicts irreversible")
	}
	if deploy.IsTerminal(item.State) {
		if item.TerminalAt == nil || item.LeaseStatus != operator.JobLeaseNone || item.LeaseExpiresAt != nil {
			return errors.New("operator client: terminal job has contradictory terminal/lease metadata")
		}
		return nil
	}
	if item.TerminalAt != nil {
		return errors.New("operator client: nonterminal job carries terminal_at")
	}
	switch item.State {
	case deploy.NotStarted:
		if item.LeaseStatus != operator.JobLeaseUnclaimed || item.LeaseExpiresAt != nil {
			return errors.New("operator client: unclaimed job has contradictory lease metadata")
		}
	case deploy.Claimed, deploy.Running, deploy.Verifying:
		switch item.LeaseStatus {
		case operator.JobLeaseUnknown:
			if item.LeaseExpiresAt != nil {
				return errors.New("operator client: unknown lease unexpectedly has expiry evidence")
			}
		case operator.JobLeaseActive:
			if item.LeaseExpiresAt == nil || !item.LeaseExpiresAt.After(evaluatedAt) {
				return errors.New("operator client: active lease is not after evaluated_at")
			}
		case operator.JobLeaseExpired:
			if item.LeaseExpiresAt == nil || item.LeaseExpiresAt.After(evaluatedAt) {
				return errors.New("operator client: expired lease contradicts evaluated_at")
			}
		default:
			return errors.New("operator client: active-state job has invalid lease status")
		}
	default:
		return errors.New("operator client: unknown nonterminal job state")
	}
	return nil
}

func validateJobStateCounts(counts []operator.JobStateCount, total int, selected []deploy.JobState) error {
	if len(counts) != len(deploy.AllJobStates) {
		return errors.New("operator client: job state counts are not exhaustive")
	}
	sum := 0
	for i, state := range deploy.AllJobStates {
		if counts[i].State != state || counts[i].Count < 0 {
			return errors.New("operator client: job state counts are not canonical")
		}
		if len(selected) > 0 && !jobStateSelected(state, selected) && counts[i].Count != 0 {
			return errors.New("operator client: job state counts contradict state filter")
		}
		sum += counts[i].Count
	}
	if sum != total {
		return errors.New("operator client: job state counts do not match total")
	}
	return nil
}

func jobStateSelected(candidate deploy.JobState, selected []deploy.JobState) bool {
	for _, state := range selected {
		if candidate == state {
			return true
		}
	}
	return false
}

func jobSummaryBefore(previous, current operator.JobSummary) bool {
	if !previous.CreatedAt.Equal(current.CreatedAt) {
		return previous.CreatedAt.After(current.CreatedAt)
	}
	if previous.Revision != current.Revision {
		return previous.Revision > current.Revision
	}
	return previous.JobID > current.JobID
}

func validJobEventPhase(phase operator.JobEventPhase) bool {
	switch phase {
	case operator.JobEventStart, operator.JobEventFinish, operator.JobEventRejected, operator.JobEventOther:
		return true
	default:
		return false
	}
}

func validOperatorJobDigest(value string) bool {
	if len(value) != len("sha256:")+64 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	for _, char := range value[len("sha256:"):] {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

func validateJobEvaluationTime(value time.Time) error {
	if value.IsZero() {
		return errors.New("operator client: job read evaluated_at is missing")
	}
	return requireUTCJobTime(value, "evaluated_at")
}

func requireUTCJobTime(value time.Time, label string) error {
	if _, offset := value.Zone(); offset != 0 {
		return fmt.Errorf("operator client: %s must be UTC", label)
	}
	return nil
}
