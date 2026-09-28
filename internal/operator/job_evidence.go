package operator

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/teddashh/AI-Intune/internal/deploy"
	"github.com/teddashh/AI-Intune/internal/model"
	"github.com/teddashh/AI-Intune/internal/store"
)

const (
	JobEvidenceSchemaVersion            = 6
	JobEvidenceMaxFieldBytes            = 16 << 10
	JobEvidenceDefaultLimit             = store.MaxJobReadPageSize
	JobEvidenceProducerExecutorAgent    = "executor_agent"
	JobEvidenceProducerHubScheduler     = "hub_scheduler"
	JobEvidenceAuthorityMachineLease    = "machine_bearer_lease"
	JobEvidenceAuthorityDependencyGraph = "dependency_graph"
	JobEvidenceRoleExecutor             = "executor"
	JobEvidenceRoleScheduler            = "scheduler"
	JobEvidenceRoleIndependent          = store.JobVerificationRoleIndependent
	JobEvidenceAuthorityVerifierBearer  = store.JobVerificationAuthorityVerifierBearer
	jobEvidenceReportedPhaseMaxBytes    = 64
	jobEvidenceRuleIDMaxBytes           = 256
	jobEvidenceRejectionCodeMaxBytes    = 64
)

type jobEvidenceTextField string

const (
	jobEvidenceDesiredSpec        jobEvidenceTextField = "desired.spec"
	jobEvidenceReportedPhase      jobEvidenceTextField = "event.reported_phase"
	jobEvidenceRejectionCode      jobEvidenceTextField = "rejection.code"
	jobEvidenceRejectionDetail    jobEvidenceTextField = "rejection.detail"
	jobEvidenceVerificationRuleID jobEvidenceTextField = "verification.rule_id"
	jobEvidenceVerificationCmd    jobEvidenceTextField = "verification.command"
	jobEvidenceVerificationStdout jobEvidenceTextField = "verification.stdout_excerpt"
	jobEvidenceVerificationStderr jobEvidenceTextField = "verification.stderr_excerpt"
	jobEvidenceVerifierName       jobEvidenceTextField = "independent.producer.display_name"
	jobEvidenceVerifierDomain     jobEvidenceTextField = "independent.producer.failure_domain"
)

const jobEvidenceVerifierTextMaxBytes = 256

// Assignment states. ⚠ These four are exhaustive and mutually exclusive; a
// reader must never have to infer a fifth from a missing timestamp.
const (
	JobAssignmentReported        = "reported"
	JobAssignmentProducerRevoked = "producer_revoked"
	JobAssignmentWaitingForJob   = "waiting_for_job"
	JobAssignmentAwaitingReport  = "awaiting_report"
)

// jobEvidenceTextPolicies is the complete field-to-policy mapping for this DTO.
var jobEvidenceTextPolicies = map[jobEvidenceTextField]evidenceTextPolicy{
	jobEvidenceDesiredSpec:        {maxBytes: JobEvidenceMaxFieldBytes, policy: boundedTextBlock},
	jobEvidenceReportedPhase:      {maxBytes: jobEvidenceReportedPhaseMaxBytes, policy: boundedTextStrict},
	jobEvidenceRejectionCode:      {maxBytes: jobEvidenceRejectionCodeMaxBytes, policy: boundedTextStrict},
	jobEvidenceRejectionDetail:    {maxBytes: JobEvidenceMaxFieldBytes, policy: boundedTextBlock},
	jobEvidenceVerificationRuleID: {maxBytes: jobEvidenceRuleIDMaxBytes, policy: boundedTextStrict},
	jobEvidenceVerificationCmd:    {maxBytes: JobEvidenceMaxFieldBytes, policy: boundedTextStrict},
	jobEvidenceVerificationStdout: {maxBytes: JobEvidenceMaxFieldBytes, policy: boundedTextBlock},
	jobEvidenceVerificationStderr: {maxBytes: JobEvidenceMaxFieldBytes, policy: boundedTextBlock},
	jobEvidenceVerifierName:       {maxBytes: jobEvidenceVerifierTextMaxBytes, policy: boundedTextStrict},
	jobEvidenceVerifierDomain:     {maxBytes: jobEvidenceVerifierTextMaxBytes, policy: boundedTextStrict},
}

type JobEvidenceRequest struct {
	JobID string
	Limit int
}

type JobEvidenceResult struct {
	SchemaVersion int                         `json:"schema_version"`
	EvaluatedAt   time.Time                   `json:"evaluated_at"`
	JobID         string                      `json:"job_id"`
	MachineID     string                      `json:"machine_id"`
	DisplayName   string                      `json:"display_name"`
	State         deploy.JobState             `json:"state"`
	Disclosure    JobEvidenceDisclosure       `json:"disclosure"`
	Desired       JobDesiredEvidence          `json:"desired"`
	Events        JobEventEvidencePage        `json:"events"`
	Rejection     *JobRejectionEvidence       `json:"rejection"`
	Verifications JobVerificationEvidencePage `json:"verifications"`
	Independent   *JobIndependentEvidence     `json:"independent"`
}

// JobIndependentEvidence is the second producer's page. Verdict is one of the
// eight store.IndependentVerdict values; six of them are not a pass or a rule
// failure, and each is stated as its own word.
type JobIndependentEvidence struct {
	Verdict         string                       `json:"verdict"`
	ArtifactDigest  string                       `json:"artifact_digest"`
	ExpectedVersion string                       `json:"expected_version"`
	TerminalAt      *time.Time                   `json:"terminal_at"`
	Total           int                          `json:"total"`
	Truncated       bool                         `json:"truncated"`
	LiveProducers   int                          `json:"live_producers"`
	Items           []JobIndependentVerification `json:"items"`
	// Assignments is what makes an absent verdict legible. Without it "absent"
	// cannot distinguish "nobody was asked to look" from "somebody was asked and
	// has not answered", and those call for opposite actions.
	Assignments []JobIndependentAssignment `json:"assignments"`
}

// JobIndependentAssignment states one operator instruction and whether it has
// been answered. It carries no rule and no command: an assignment names a job,
// and the verifier decides for itself what to measure.
type JobIndependentAssignment struct {
	AssignmentID  string          `json:"assignment_id"`
	VerifierID    string          `json:"verifier_id"`
	DisplayName   JobEvidenceText `json:"display_name"`
	FailureDomain JobEvidenceText `json:"failure_domain"`
	State         string          `json:"state"`
	AssignedAt    time.Time       `json:"assigned_at"`
	ReportedAt    *time.Time      `json:"reported_at"`
}

// JobIndependentVerification carries both clocks, labelled distinctly.
// docs/VERIFIER-TOPOLOGY.md §2.5 measured an 81-second skew between an agent
// clock and this Hub's; ordering and freshness use ReceivedAt.
type JobIndependentVerification struct {
	VerificationID     string                 `json:"verification_id"`
	RuleID             JobEvidenceText        `json:"rule_id"`
	Command            JobEvidenceText        `json:"command"`
	ExitCode           *int                   `json:"exit_code"`
	Passed             bool                   `json:"passed"`
	StdoutExcerpt      JobEvidenceText        `json:"stdout_excerpt"`
	StderrExcerpt      JobEvidenceText        `json:"stderr_excerpt"`
	ObservedDigest     string                 `json:"observed_digest"`
	DigestReported     bool                   `json:"digest_reported"`
	DigestMatchesJob   bool                   `json:"digest_matches_job"`
	ObservedVersion    string                 `json:"observed_version"`
	VersionReported    bool                   `json:"version_reported"`
	VersionMatchesJob  bool                   `json:"version_matches_job"`
	ReportedVerifiedAt time.Time              `json:"reported_verified_at"`
	ReceivedAt         time.Time              `json:"received_at"`
	Producer           JobIndependentProducer `json:"producer"`
}

type JobIndependentProducer struct {
	Kind          string          `json:"kind"`
	VerifierID    string          `json:"verifier_id"`
	DisplayName   JobEvidenceText `json:"display_name"`
	FailureDomain JobEvidenceText `json:"failure_domain"`
	Authority     string          `json:"authority"`
	EvidenceRole  string          `json:"evidence_role"`
	State         string          `json:"state"`
}

// JobEvidenceDisclosure describes the envelope-wide disclosure bounds.
// MaxFieldBytes is the largest single-field limit in this DTO, not the limit
// for every text field; each JobEvidenceText reports its own limit in MaxBytes.
type JobEvidenceDisclosure struct {
	Limit                                  int    `json:"limit"`
	MaxFieldBytes                          int    `json:"max_field_bytes"`
	EventProvenanceRecordingEnabled        bool   `json:"event_provenance_recording_enabled"`
	VerificationProducerKind               string `json:"verification_producer_kind"`
	IndependentVerifier                    bool   `json:"independent_verifier"`
	VerificationProvenanceRecordingEnabled bool   `json:"verification_provenance_recording_enabled"`
	VerificationReceivedAtRecordingEnabled bool   `json:"verification_received_at_recording_enabled"`
}

type JobEvidenceProducer struct {
	Kind               string `json:"kind"`
	ProducerID         string `json:"producer_id"`
	DisplayName        string `json:"display_name"`
	Authority          string `json:"authority"`
	EvidenceRole       string `json:"evidence_role"`
	ProvenanceRecorded bool   `json:"provenance_recorded"`
}

type JobDesiredEvidence struct {
	DesiredID    string          `json:"desired_id"`
	ScopeType    string          `json:"scope_type"`
	ScopeID      string          `json:"scope_id"`
	ResourceKind string          `json:"resource_kind"`
	ResourceID   string          `json:"resource_id"`
	Revision     deploy.Revision `json:"revision"`
	CreatedAt    time.Time       `json:"created_at"`
	Spec         JobEvidenceText `json:"spec"`
}

type JobEventEvidencePage struct {
	Total     int                `json:"total"`
	Truncated bool               `json:"truncated"`
	Items     []JobEventEvidence `json:"items"`
}

type JobEventEvidence struct {
	EventID       string              `json:"event_id"`
	Seq           int                 `json:"seq"`
	Phase         JobEventPhase       `json:"phase"`
	ReportedPhase JobEvidenceText     `json:"reported_phase"`
	OccurredAt    time.Time           `json:"occurred_at"`
	ReceivedAt    time.Time           `json:"received_at"`
	PayloadBytes  int                 `json:"payload_bytes"`
	Producer      JobEvidenceProducer `json:"producer"`
}

type JobRejectionEvidence struct {
	EventID          string              `json:"event_id"`
	Seq              int                 `json:"seq"`
	OccurredAt       time.Time           `json:"occurred_at"`
	ReceivedAt       time.Time           `json:"received_at"`
	PayloadDecodable bool                `json:"payload_decodable"`
	Code             JobEvidenceText     `json:"code"`
	CodeKnown        bool                `json:"code_known"`
	HasDetail        bool                `json:"has_detail"`
	Detail           JobEvidenceText     `json:"detail"`
	Producer         JobEvidenceProducer `json:"producer"`
}

type JobVerificationEvidencePage struct {
	Total     int                       `json:"total"`
	Passed    int                       `json:"passed"`
	Failed    int                       `json:"failed"`
	Truncated bool                      `json:"truncated"`
	Items     []JobVerificationEvidence `json:"items"`
}

type JobVerificationEvidence struct {
	VerificationID     string              `json:"verification_id"`
	RuleID             JobEvidenceText     `json:"rule_id"`
	Command            JobEvidenceText     `json:"command"`
	ExitCode           *int                `json:"exit_code"`
	Passed             bool                `json:"passed"`
	StdoutExcerpt      JobEvidenceText     `json:"stdout_excerpt"`
	StderrExcerpt      JobEvidenceText     `json:"stderr_excerpt"`
	ReportedVerifiedAt time.Time           `json:"reported_verified_at"`
	ReceivedAt         *time.Time          `json:"received_at"`
	Producer           JobEvidenceProducer `json:"producer"`
}

var knownJobEvidenceRejectionCodes = map[string]struct{}{
	string(deploy.StaleRevision):          {},
	string(deploy.DuplicateJobID):         {},
	string(deploy.ArtifactHashMismatch):   {},
	string(deploy.LeaseInvalid):           {},
	string(deploy.IrreversibleMigration):  {},
	string(deploy.PreconditionFailed):     {},
	string(deploy.UnspecifiedModelSwitch): {},
	string(deploy.DependencyFailed):       {},
}

func (s *Service) JobEvidence(request JobEvidenceRequest, evaluatedAt time.Time) (JobEvidenceResult, error) {
	if strings.TrimSpace(request.JobID) == "" || request.JobID != strings.TrimSpace(request.JobID) ||
		strings.Contains(request.JobID, "/") || request.JobID == "." || request.JobID == ".." ||
		evaluatedAt.IsZero() || request.Limit < 0 || request.Limit > JobEvidenceDefaultLimit {
		return JobEvidenceResult{}, fmt.Errorf("%w: job_id, evaluated_at, or limit is invalid", ErrInvalidJobRead)
	}
	limit := request.Limit
	if limit == 0 {
		limit = JobEvidenceDefaultLimit
	}
	assignments, err := s.store.JobVerificationAssignments(request.JobID)
	if err != nil {
		return JobEvidenceResult{}, err
	}
	evidence, err := s.store.JobReadEvidence(request.JobID, limit)
	if err != nil {
		if errors.Is(err, store.ErrInvalidJobRead) {
			return JobEvidenceResult{}, fmt.Errorf("%w: %v", ErrInvalidJobRead, err)
		}
		return JobEvidenceResult{}, err
	}
	summary, err := projectJobSummary(evidence.Item, evaluatedAt.UTC())
	if err != nil {
		return JobEvidenceResult{}, err
	}
	executorProducer := JobEvidenceProducer{
		Kind: JobEvidenceProducerExecutorAgent, ProducerID: summary.MachineID,
		DisplayName: summary.DisplayName, Authority: JobEvidenceAuthorityMachineLease,
		EvidenceRole: JobEvidenceRoleExecutor,
	}
	result := JobEvidenceResult{
		SchemaVersion: JobEvidenceSchemaVersion, EvaluatedAt: evaluatedAt.UTC(),
		JobID: summary.JobID, MachineID: summary.MachineID, DisplayName: summary.DisplayName,
		State: summary.State,
		Disclosure: JobEvidenceDisclosure{
			Limit: limit, MaxFieldBytes: JobEvidenceMaxFieldBytes,
			EventProvenanceRecordingEnabled:        true,
			VerificationProducerKind:               JobEvidenceProducerExecutorAgent,
			IndependentVerifier:                    false, // computed below
			VerificationProvenanceRecordingEnabled: true,
			VerificationReceivedAtRecordingEnabled: true,
		},
		Desired: JobDesiredEvidence{
			DesiredID: evidence.Desired.DesiredID, ScopeType: evidence.Desired.ScopeType,
			ScopeID: evidence.Desired.ScopeID, ResourceKind: evidence.Desired.ResourceKind,
			ResourceID: evidence.Desired.ResourceID, Revision: evidence.Desired.Revision,
			CreatedAt: evidence.Desired.CreatedAt.UTC(),
			Spec:      projectJobEvidenceText(jobEvidenceDesiredSpec, evidence.Desired.Spec),
		},
		Events: JobEventEvidencePage{
			Total: evidence.Item.EventCount, Truncated: evidence.EventsTruncated,
			Items: make([]JobEventEvidence, 0, len(evidence.Events)),
		},
		Verifications: JobVerificationEvidencePage{
			Total: evidence.Item.VerificationTotal, Passed: evidence.Item.VerificationPassed,
			Failed: evidence.Item.VerificationFailed, Truncated: evidence.VerificationsTruncated,
			Items: make([]JobVerificationEvidence, 0, len(evidence.Verifications)),
		},
	}
	for _, event := range evidence.Events {
		result.Events.Items = append(result.Events.Items, JobEventEvidence{
			EventID: event.EventID, Seq: event.Seq, Phase: safeJobEventPhase(event.Phase),
			ReportedPhase: projectJobEvidenceText(jobEvidenceReportedPhase, event.Phase),
			OccurredAt:    event.OccurredAt.UTC(), ReceivedAt: event.ReceivedAt.UTC(),
			PayloadBytes: len(event.Payload), Producer: projectJobEventProducer(event, summary),
		})
		if event.Phase == string(JobEventRejected) {
			result.Rejection = projectJobRejectionEvidence(event, summary)
		}
	}
	for _, verification := range evidence.Verifications {
		verificationProducer := executorProducer
		verificationProducer.ProvenanceRecorded = verification.ProvenanceRecorded
		if verification.ProducerID != "" {
			verificationProducer.ProducerID = verification.ProducerID
		}
		var receivedAt *time.Time
		if !verification.ReceivedAt.IsZero() {
			value := verification.ReceivedAt.UTC()
			receivedAt = &value
		}
		result.Verifications.Items = append(result.Verifications.Items, JobVerificationEvidence{
			VerificationID: verification.VerificationID,
			RuleID:         projectJobEvidenceText(jobEvidenceVerificationRuleID, verification.RuleID),
			Command:        projectJobEvidenceText(jobEvidenceVerificationCmd, verification.Command),
			ExitCode:       verification.ExitCode, Passed: verification.Passed,
			StdoutExcerpt:      projectJobEvidenceText(jobEvidenceVerificationStdout, verification.StdoutExcerpt),
			StderrExcerpt:      projectJobEvidenceText(jobEvidenceVerificationStderr, verification.StderrExcerpt),
			ReportedVerifiedAt: verification.VerifiedAt.UTC(), ReceivedAt: receivedAt,
			Producer: verificationProducer,
		})
	}
	independent, err := projectJobIndependentEvidence(evidence, assignments)
	if err != nil {
		return JobEvidenceResult{}, err
	}
	result.Independent = independent
	// The flag says "somebody other than the executor can still speak for this
	// job". A page whose every producer has been revoked does not qualify: its
	// verdict is producer_revoked, which is neither a pass nor a failure.
	result.Disclosure.IndependentVerifier = independent.LiveProducers > 0
	return result, nil
}

// projectJobIndependentEvidence always returns a section. A job with no
// independent rows gets verdict "absent" and zero items, so a reader is told
// that nothing independent exists rather than being shown only the executor's
// own account and left to assume it was corroborated.
func projectJobIndependentEvidence(evidence store.JobReadEvidence,
	assignments []store.VerificationAssignment,
) (*JobIndependentEvidence, error) {
	artifactDigest := ""
	if evidence.Item.ArtifactDigest != nil {
		artifactDigest = *evidence.Item.ArtifactDigest
	}
	section := &JobIndependentEvidence{
		ArtifactDigest: artifactDigest, ExpectedVersion: evidence.IndependentExpectedVersion,
		TerminalAt: evidence.Item.TerminalAt,
		Total:      evidence.Item.IndependentTotal, Truncated: evidence.IndependentTruncated,
		Items:       make([]JobIndependentVerification, 0, len(evidence.Independent)),
		Assignments: make([]JobIndependentAssignment, 0, len(assignments)),
	}
	for _, assignment := range assignments {
		section.Assignments = append(section.Assignments, JobIndependentAssignment{
			AssignmentID: assignment.AssignmentID, VerifierID: assignment.VerifierID,
			DisplayName:   projectJobEvidenceText(jobEvidenceVerifierName, assignment.VerifierName),
			FailureDomain: projectJobEvidenceText(jobEvidenceVerifierDomain, assignment.VerifierDomain),
			State:         jobIndependentAssignmentState(assignment),
			AssignedAt:    assignment.AssignedAt.UTC(), ReportedAt: assignment.ReportedAt,
		})
	}
	for _, row := range evidence.Independent {
		verifier, ok := evidence.Verifiers[row.VerifierID]
		if !ok {
			return nil, fmt.Errorf("%w: independent evidence has no producer identity", ErrInvalidJobRead)
		}
		section.Items = append(section.Items, JobIndependentVerification{
			VerificationID: row.VerificationID,
			RuleID:         projectJobEvidenceText(jobEvidenceVerificationRuleID, row.RuleID),
			Command:        projectJobEvidenceText(jobEvidenceVerificationCmd, row.Command),
			ExitCode:       row.ExitCode, Passed: row.Passed,
			StdoutExcerpt:  projectJobEvidenceText(jobEvidenceVerificationStdout, row.StdoutExcerpt),
			StderrExcerpt:  projectJobEvidenceText(jobEvidenceVerificationStderr, row.StderrExcerpt),
			ObservedDigest: row.ObservedDigest, DigestReported: row.ObservedDigest != "",
			DigestMatchesJob: row.ObservedDigest != "" && row.ObservedDigest == artifactDigest,
			ObservedVersion:  row.ObservedVersion, VersionReported: row.ObservedVersion != "",
			VersionMatchesJob: evidence.IndependentExpectedVersion != "" &&
				row.ObservedVersion == evidence.IndependentExpectedVersion,
			ReportedVerifiedAt: row.VerifiedAt.UTC(), ReceivedAt: row.ReceivedAt.UTC(),
			Producer: JobIndependentProducer{
				Kind: verifier.Kind, VerifierID: verifier.VerifierID,
				DisplayName:   projectJobEvidenceText(jobEvidenceVerifierName, verifier.DisplayName),
				FailureDomain: projectJobEvidenceText(jobEvidenceVerifierDomain, verifier.FailureDomain),
				Authority:     JobEvidenceAuthorityVerifierBearer,
				EvidenceRole:  JobEvidenceRoleIndependent, State: verifier.State(),
			},
		})
	}
	// Both come from the store's unbounded aggregate, so a truncated page can
	// never turn a digest clash or a revoked producer into a pass.
	section.Verdict = string(evidence.IndependentVerdict)
	section.LiveProducers = evidence.IndependentLiveProducers
	return section, nil
}

// jobIndependentAssignmentState is a typed state, not a sentence. Each value
// names the one thing a reader would do next: revoked has no producer left,
// waiting_for_job has nothing to measure yet, awaiting_report has somebody who
// owes an answer, reported has one.
func jobIndependentAssignmentState(assignment store.VerificationAssignment) string {
	switch {
	case assignment.ReportedAt != nil:
		return JobAssignmentReported
	case assignment.VerifierRevoked:
		return JobAssignmentProducerRevoked
	case assignment.JobTerminalAt == nil:
		return JobAssignmentWaitingForJob
	default:
		return JobAssignmentAwaitingReport
	}
}

func projectJobEvidenceText(field jobEvidenceTextField, value string) JobEvidenceText {
	policy, ok := jobEvidenceTextPolicies[field]
	if !ok {
		panic("missing job evidence text policy for " + field)
	}
	return projectEvidenceText(policy, value)
}

func projectJobRejectionEvidence(event store.JobEvent, summary JobSummary) *JobRejectionEvidence {
	result := &JobRejectionEvidence{
		EventID: event.EventID, Seq: event.Seq,
		OccurredAt: event.OccurredAt.UTC(), ReceivedAt: event.ReceivedAt.UTC(),
		Code:     projectJobEvidenceText(jobEvidenceRejectionCode, ""),
		Detail:   projectJobEvidenceText(jobEvidenceRejectionDetail, ""),
		Producer: projectJobEventProducer(event, summary),
	}
	var payload model.JobRejectEventPayload
	decoder := json.NewDecoder(bytes.NewReader([]byte(event.Payload)))
	if err := decoder.Decode(&payload); err != nil {
		return result
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return result
	}
	result.PayloadDecodable = true
	result.Code = projectJobEvidenceText(jobEvidenceRejectionCode, payload.RejectionCode)
	_, result.CodeKnown = knownJobEvidenceRejectionCodes[payload.RejectionCode]
	result.Detail = projectJobEvidenceText(jobEvidenceRejectionDetail, payload.Detail)
	result.HasDetail = result.Detail.Text != ""
	return result
}

func projectJobEventProducer(event store.JobEvent, summary JobSummary) JobEvidenceProducer {
	displayName := summary.DisplayName
	if event.ProducerKind == JobEvidenceProducerHubScheduler {
		displayName = "Hub"
	}
	return JobEvidenceProducer{
		Kind: event.ProducerKind, ProducerID: event.ProducerID, DisplayName: displayName,
		Authority: event.Authority, EvidenceRole: event.EvidenceRole,
		ProvenanceRecorded: event.ProvenanceRecorded,
	}
}
