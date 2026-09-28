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
	"strings"
	"time"

	"github.com/teddashh/AI-Intune/internal/deploy"
	"github.com/teddashh/AI-Intune/internal/store"
)

const (
	JobReadSchemaVersion   = 1
	DefaultJobReadLimit    = 50
	MaxJobReadLimit        = store.MaxJobReadPageSize
	JobDetailEvidenceLimit = store.MaxJobReadPageSize
)

var ErrInvalidJobRead = errors.New("operator: invalid job read request")

type JobListRequest struct {
	MachineID    string
	States       []deploy.JobState
	DeploymentID string
	ResourceKind string
	ResourceID   string
	Limit        int
	Cursor       string
}

type JobStateCount struct {
	State deploy.JobState `json:"state"`
	Count int             `json:"count"`
}

type JobListResult struct {
	SchemaVersion int             `json:"schema_version"`
	EvaluatedAt   time.Time       `json:"evaluated_at"`
	Total         int             `json:"total"`
	StateCounts   []JobStateCount `json:"state_counts"`
	Items         []JobSummary    `json:"items"`
	NextCursor    *string         `json:"next_cursor"`

	page *store.JobReadPage
}

func (r JobListResult) StorePage() (page store.JobReadPage, ok bool) {
	if r.page == nil {
		return store.JobReadPage{}, false
	}
	return *r.page, true
}

type JobSummary struct {
	JobID                   string                  `json:"job_id"`
	MachineID               string                  `json:"machine_id"`
	DisplayName             string                  `json:"display_name"`
	DesiredID               string                  `json:"desired_id"`
	DeploymentID            *string                 `json:"deployment_id"`
	ResourceKind            string                  `json:"resource_kind"`
	ResourceID              string                  `json:"resource_id"`
	Revision                deploy.Revision         `json:"revision"`
	State                   deploy.JobState         `json:"state"`
	Irreversible            bool                    `json:"irreversible"`
	ExecutionTimeoutSeconds int                     `json:"execution_timeout_seconds"`
	ArtifactDigest          *string                 `json:"artifact_digest"`
	ArtifactDigestStatus    JobArtifactDigestStatus `json:"artifact_digest_status"`
	CreatedAt               time.Time               `json:"created_at"`
	TerminalAt              *time.Time              `json:"terminal_at"`
	LeaseStatus             JobLeaseStatus          `json:"lease_status"`
	LeaseExpiresAt          *time.Time              `json:"lease_expires_at"`
	EventCount              int                     `json:"event_count"`
	LastEventReceivedAt     *time.Time              `json:"last_event_received_at"`
	VerificationTotal       int                     `json:"verification_total"`
	VerificationPassed      int                     `json:"verification_passed"`
	VerificationFailed      int                     `json:"verification_failed"`
}

type JobArtifactDigestStatus string

const (
	JobArtifactDigestRecorded    JobArtifactDigestStatus = "recorded"
	JobArtifactDigestNotRecorded JobArtifactDigestStatus = "not_recorded"
)

type JobLeaseStatus string

const (
	JobLeaseUnclaimed JobLeaseStatus = "unclaimed"
	JobLeaseActive    JobLeaseStatus = "active"
	JobLeaseExpired   JobLeaseStatus = "expired"
	JobLeaseNone      JobLeaseStatus = "none"
	JobLeaseUnknown   JobLeaseStatus = "unknown"
)

type JobDetailResult struct {
	SchemaVersion int                        `json:"schema_version"`
	EvaluatedAt   time.Time                  `json:"evaluated_at"`
	Item          JobSummary                 `json:"item"`
	Desired       JobDesiredSummary          `json:"desired"`
	Events        JobEventSummaryPage        `json:"events"`
	Verifications JobVerificationSummaryPage `json:"verifications"`
}

type JobDesiredSummary struct {
	DesiredID    string          `json:"desired_id"`
	ScopeType    string          `json:"scope_type"`
	ScopeID      string          `json:"scope_id"`
	ResourceKind string          `json:"resource_kind"`
	ResourceID   string          `json:"resource_id"`
	Revision     deploy.Revision `json:"revision"`
	CreatedAt    time.Time       `json:"created_at"`
}

type JobEventSummaryPage struct {
	Total     int               `json:"total"`
	Truncated bool              `json:"truncated"`
	Items     []JobEventSummary `json:"items"`
}

type JobEventSummary struct {
	EventID    string        `json:"event_id"`
	Seq        int           `json:"seq"`
	Phase      JobEventPhase `json:"phase"`
	OccurredAt time.Time     `json:"occurred_at"`
	ReceivedAt time.Time     `json:"received_at"`
}

type JobEventPhase string

const (
	JobEventStart    JobEventPhase = "start"
	JobEventFinish   JobEventPhase = "finish"
	JobEventRejected JobEventPhase = "rejected"
	JobEventOther    JobEventPhase = "other"
)

type JobVerificationSummaryPage struct {
	Total     int                      `json:"total"`
	Passed    int                      `json:"passed"`
	Failed    int                      `json:"failed"`
	Truncated bool                     `json:"truncated"`
	Items     []JobVerificationSummary `json:"items"`
}

// ReportedVerifiedAt is the machine-supplied clock. The current ledger has no
// Hub received_at or producer/role identity for a verification, so this DTO
// deliberately does not call the record independent evidence.
type JobVerificationSummary struct {
	VerificationID     string    `json:"verification_id"`
	Passed             bool      `json:"passed"`
	ExitCode           *int      `json:"exit_code"`
	ReportedVerifiedAt time.Time `json:"reported_verified_at"`
}

func (s *Service) ListJobs(request JobListRequest, evaluatedAt time.Time) (JobListResult, error) {
	if evaluatedAt.IsZero() {
		return JobListResult{}, fmt.Errorf("%w: evaluated_at is required", ErrInvalidJobRead)
	}
	evaluatedAt = evaluatedAt.UTC()
	normalized, err := normalizeJobListRequest(request)
	if err != nil {
		return JobListResult{}, err
	}
	filterDigest := jobFilterDigest(normalized)
	storeFilter := store.JobReadFilter{
		MachineID: normalized.MachineID, States: normalized.States,
		DeploymentID: normalized.DeploymentID, ResourceKind: normalized.ResourceKind,
		ResourceID: normalized.ResourceID, Limit: normalized.Limit,
	}
	if normalized.Cursor != "" {
		cursor, err := decodeJobListCursor(normalized.Cursor, filterDigest)
		if err != nil {
			return JobListResult{}, err
		}
		storeFilter.CreationCeiling = &cursor.CreationCeiling
		storeFilter.After = &store.JobReadPosition{
			CreatedAt: cursor.CreatedAt, Revision: cursor.Revision, JobID: cursor.JobID,
		}
	}
	page, err := s.store.ListJobReads(storeFilter)
	if err != nil {
		if errors.Is(err, store.ErrInvalidJobRead) {
			return JobListResult{}, fmt.Errorf("%w: %v", ErrInvalidJobRead, err)
		}
		return JobListResult{}, err
	}
	result := JobListResult{
		SchemaVersion: JobReadSchemaVersion, EvaluatedAt: evaluatedAt,
		Total:       page.Total,
		StateCounts: make([]JobStateCount, 0, len(page.StateCounts)),
		Items:       make([]JobSummary, 0, len(page.Items)), page: &page,
	}
	for _, count := range page.StateCounts {
		result.StateCounts = append(result.StateCounts, JobStateCount{State: count.State, Count: count.Count})
	}
	for _, item := range page.Items {
		summary, err := projectJobSummary(item, evaluatedAt)
		if err != nil {
			return JobListResult{}, err
		}
		result.Items = append(result.Items, summary)
	}
	if page.Next != nil {
		encoded, err := encodeJobListCursor(jobListCursor{
			Version: JobReadSchemaVersion, FilterDigest: filterDigest,
			CreationCeiling: page.CreationCeiling, CreatedAt: page.Next.CreatedAt.UTC(),
			Revision: page.Next.Revision, JobID: page.Next.JobID,
		})
		if err != nil {
			return JobListResult{}, err
		}
		result.NextCursor = &encoded
	}
	return result, nil
}

func (s *Service) JobDetail(jobID string, evaluatedAt time.Time) (JobDetailResult, error) {
	return s.jobDetail(jobID, evaluatedAt)
}

func (s *Service) jobDetail(jobID string, evaluatedAt time.Time) (JobDetailResult, error) {
	if strings.TrimSpace(jobID) == "" || jobID != strings.TrimSpace(jobID) ||
		strings.Contains(jobID, "/") || jobID == "." || jobID == ".." || evaluatedAt.IsZero() {
		return JobDetailResult{}, fmt.Errorf("%w: job_id and evaluated_at are required", ErrInvalidJobRead)
	}
	evaluatedAt = evaluatedAt.UTC()
	detail, err := s.store.JobReadDetail(jobID, JobDetailEvidenceLimit)
	if err != nil {
		if errors.Is(err, store.ErrInvalidJobRead) {
			return JobDetailResult{}, fmt.Errorf("%w: %v", ErrInvalidJobRead, err)
		}
		return JobDetailResult{}, err
	}
	summary, err := projectJobSummary(detail.Item, evaluatedAt)
	if err != nil {
		return JobDetailResult{}, err
	}
	desired := JobDesiredSummary{
		DesiredID: detail.Desired.DesiredID, ScopeType: detail.Desired.ScopeType,
		ScopeID: detail.Desired.ScopeID, ResourceKind: detail.Desired.ResourceKind,
		ResourceID: detail.Desired.ResourceID, Revision: detail.Desired.Revision,
		CreatedAt: detail.Desired.CreatedAt.UTC(),
	}
	if desired.DesiredID == "" || (desired.ScopeType != "machine" && desired.ScopeType != "channel") ||
		desired.ScopeID == "" || desired.ResourceKind == "" || desired.ResourceID == "" ||
		(desired.ScopeType == "machine" && desired.ScopeID != summary.MachineID) ||
		desired.Revision < 0 || desired.CreatedAt.IsZero() {
		return JobDetailResult{}, fmt.Errorf("%w: desired-state metadata is invalid", store.ErrJobReadCorrupt)
	}
	result := JobDetailResult{
		SchemaVersion: JobReadSchemaVersion, EvaluatedAt: evaluatedAt, Item: summary,
		Desired: desired,
		Events: JobEventSummaryPage{
			Total: detail.Item.EventCount, Truncated: detail.EventsTruncated,
			Items: make([]JobEventSummary, 0, len(detail.Events)),
		},
		Verifications: JobVerificationSummaryPage{
			Total: detail.Item.VerificationTotal, Passed: detail.Item.VerificationPassed,
			Failed: detail.Item.VerificationFailed, Truncated: detail.VerificationsTruncated,
			Items: make([]JobVerificationSummary, 0, len(detail.Verifications)),
		},
	}
	for _, event := range detail.Events {
		result.Events.Items = append(result.Events.Items, JobEventSummary{
			EventID: event.EventID, Seq: event.Seq, Phase: safeJobEventPhase(event.Phase),
			OccurredAt: event.OccurredAt.UTC(), ReceivedAt: event.ReceivedAt.UTC(),
		})
	}
	for _, verification := range detail.Verifications {
		result.Verifications.Items = append(result.Verifications.Items, JobVerificationSummary{
			VerificationID: verification.VerificationID, Passed: verification.Passed,
			ExitCode: verification.ExitCode, ReportedVerifiedAt: verification.VerifiedAt.UTC(),
		})
	}
	return result, nil
}

func normalizeJobListRequest(request JobListRequest) (JobListRequest, error) {
	for name, value := range map[string]string{
		"machine_id": request.MachineID, "deployment_id": request.DeploymentID,
		"resource_kind": request.ResourceKind, "resource_id": request.ResourceID,
	} {
		if value != "" && value != strings.TrimSpace(value) {
			return JobListRequest{}, fmt.Errorf("%w: %s has surrounding whitespace", ErrInvalidJobRead, name)
		}
	}
	if request.ResourceID != "" && request.ResourceKind == "" {
		return JobListRequest{}, fmt.Errorf("%w: resource_id requires resource_kind", ErrInvalidJobRead)
	}
	if request.Limit == 0 {
		request.Limit = DefaultJobReadLimit
	}
	if request.Limit < 1 || request.Limit > MaxJobReadLimit {
		return JobListRequest{}, fmt.Errorf("%w: limit must be between 1 and %d", ErrInvalidJobRead, MaxJobReadLimit)
	}
	seen := make(map[deploy.JobState]bool, len(request.States))
	canonical := make([]deploy.JobState, 0, len(request.States))
	for _, state := range request.States {
		if !deploy.IsKnownJobState(state) || seen[state] {
			return JobListRequest{}, fmt.Errorf("%w: invalid or duplicate state %q", ErrInvalidJobRead, state)
		}
		seen[state] = true
	}
	for _, state := range deploy.AllJobStates {
		if seen[state] {
			canonical = append(canonical, state)
		}
	}
	request.States = canonical
	return request, nil
}

func projectJobSummary(item store.JobReadItem, evaluatedAt time.Time) (JobSummary, error) {
	if item.ArtifactDigest != nil && !validJobArtifactDigest(*item.ArtifactDigest) {
		return JobSummary{}, fmt.Errorf("%w: artifact digest is not canonical sha256", store.ErrJobReadCorrupt)
	}
	summary := JobSummary{
		JobID: item.JobID, MachineID: item.MachineID, DisplayName: item.DisplayName,
		DesiredID: item.DesiredID, DeploymentID: item.DeploymentID,
		ResourceKind: item.ResourceKind, ResourceID: item.ResourceID,
		Revision: item.Revision, State: item.State, Irreversible: item.Irreversible,
		ExecutionTimeoutSeconds: item.ExecutionTimeout, ArtifactDigest: item.ArtifactDigest,
		CreatedAt: item.CreatedAt.UTC(), TerminalAt: utcJobTime(item.TerminalAt),
		LeaseExpiresAt: utcJobTime(item.LeaseExpiresAt), EventCount: item.EventCount,
		LastEventReceivedAt: utcJobTime(item.LastEventReceivedAt),
		VerificationTotal:   item.VerificationTotal, VerificationPassed: item.VerificationPassed,
		VerificationFailed: item.VerificationFailed,
	}
	summary.ArtifactDigestStatus = JobArtifactDigestNotRecorded
	if summary.ArtifactDigest != nil {
		summary.ArtifactDigestStatus = JobArtifactDigestRecorded
	}
	summary.LeaseStatus = deriveJobLeaseStatus(item, evaluatedAt)
	return summary, nil
}

func deriveJobLeaseStatus(item store.JobReadItem, evaluatedAt time.Time) JobLeaseStatus {
	if deploy.IsTerminal(item.State) {
		return JobLeaseNone
	}
	if item.State == deploy.NotStarted {
		return JobLeaseUnclaimed
	}
	if item.LeaseExpiresAt == nil {
		return JobLeaseUnknown
	}
	if item.LeaseExpiresAt.After(evaluatedAt) {
		return JobLeaseActive
	}
	return JobLeaseExpired
}

func safeJobEventPhase(raw string) JobEventPhase {
	switch raw {
	case string(JobEventStart):
		return JobEventStart
	case string(JobEventFinish):
		return JobEventFinish
	case string(JobEventRejected):
		return JobEventRejected
	default:
		return JobEventOther
	}
}

func validJobArtifactDigest(value string) bool {
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

func utcJobTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	result := value.UTC()
	return &result
}

type jobListCursor struct {
	Version         int             `json:"v"`
	FilterDigest    string          `json:"filter_digest"`
	CreationCeiling int64           `json:"creation_ceiling"`
	CreatedAt       time.Time       `json:"created_at"`
	Revision        deploy.Revision `json:"revision"`
	JobID           string          `json:"job_id"`
}

func jobFilterDigest(request JobListRequest) string {
	body := struct {
		Version      int               `json:"v"`
		MachineID    string            `json:"machine_id"`
		States       []deploy.JobState `json:"states"`
		DeploymentID string            `json:"deployment_id"`
		ResourceKind string            `json:"resource_kind"`
		ResourceID   string            `json:"resource_id"`
	}{
		Version: JobReadSchemaVersion, MachineID: request.MachineID,
		States: request.States, DeploymentID: request.DeploymentID,
		ResourceKind: request.ResourceKind, ResourceID: request.ResourceID,
	}
	raw, _ := json.Marshal(body)
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func encodeJobListCursor(cursor jobListCursor) (string, error) {
	raw, err := json.Marshal(cursor)
	if err != nil {
		return "", fmt.Errorf("operator: encode job cursor: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func decodeJobListCursor(encoded, filterDigest string) (jobListCursor, error) {
	if len(encoded) == 0 || len(encoded) > 2048 {
		return jobListCursor{}, fmt.Errorf("%w: cursor length is invalid", ErrInvalidJobRead)
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || base64.RawURLEncoding.EncodeToString(raw) != encoded {
		return jobListCursor{}, fmt.Errorf("%w: cursor encoding is invalid", ErrInvalidJobRead)
	}
	var cursor jobListCursor
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cursor); err != nil {
		return jobListCursor{}, fmt.Errorf("%w: cursor document is invalid", ErrInvalidJobRead)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return jobListCursor{}, fmt.Errorf("%w: cursor has trailing JSON", ErrInvalidJobRead)
	}
	canonical, err := encodeJobListCursor(cursor)
	if err != nil || canonical != encoded {
		return jobListCursor{}, fmt.Errorf("%w: cursor is not canonical", ErrInvalidJobRead)
	}
	_, offset := cursor.CreatedAt.Zone()
	if cursor.Version != JobReadSchemaVersion || cursor.FilterDigest != filterDigest ||
		cursor.CreationCeiling < 0 || cursor.CreatedAt.IsZero() || offset != 0 ||
		cursor.Revision < 0 || strings.TrimSpace(cursor.JobID) == "" ||
		strings.Contains(cursor.JobID, "/") || cursor.JobID == "." || cursor.JobID == ".." {
		return jobListCursor{}, fmt.Errorf("%w: cursor does not match this query", ErrInvalidJobRead)
	}
	return cursor, nil
}
