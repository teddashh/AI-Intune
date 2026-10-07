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

	"github.com/teddashh/AI-Intune/internal/deploy"
	"github.com/teddashh/AI-Intune/internal/model"
	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/store"
)

// A fully populated 100-result evidence page can contain three independently
// bounded excerpts per verification. JSON escaping can expand those strings,
// so the client retains a separate hard response cap for this endpoint.
const maxJobEvidenceResponseBytes int64 = 32 << 20

// jobEvidenceTextPolicies pins field bounds and text policies independently of
// the wire response. Block fields may contain line feeds and tabs only.
var jobEvidenceTextPolicies = map[string]evidenceTextFieldPolicy{
	"desired.spec":                {maxBytes: operator.JobEvidenceMaxFieldBytes, policy: evidenceTextBlock},
	"event.reported_phase":        {maxBytes: 64, policy: evidenceTextStrict},
	"rejection.code":              {maxBytes: 64, policy: evidenceTextStrict},
	"rejection.detail":            {maxBytes: operator.JobEvidenceMaxFieldBytes, policy: evidenceTextBlock},
	"verification.rule_id":        {maxBytes: 256, policy: evidenceTextStrict},
	"verification.command":        {maxBytes: operator.JobEvidenceMaxFieldBytes, policy: evidenceTextStrict},
	"verification.stdout_excerpt": {maxBytes: operator.JobEvidenceMaxFieldBytes, policy: evidenceTextBlock},
	"verification.stderr_excerpt": {maxBytes: operator.JobEvidenceMaxFieldBytes, policy: evidenceTextBlock},
	// The independent page carries the same evidence shape as the executor one
	// and is bounded identically. It is listed separately so a divergence shows
	// up as a rejected field rather than as one page silently adopting the
	// other's limits.
	"independent.rule_id":                 {maxBytes: 256, policy: evidenceTextStrict},
	"independent.command":                 {maxBytes: operator.JobEvidenceMaxFieldBytes, policy: evidenceTextStrict},
	"independent.stdout_excerpt":          {maxBytes: operator.JobEvidenceMaxFieldBytes, policy: evidenceTextBlock},
	"independent.stderr_excerpt":          {maxBytes: operator.JobEvidenceMaxFieldBytes, policy: evidenceTextBlock},
	"independent.producer.display_name":   {maxBytes: 256, policy: evidenceTextStrict},
	"independent.producer.failure_domain": {maxBytes: 256, policy: evidenceTextStrict},
}

func (c *Client) JobEvidence(ctx context.Context, jobID string, limit int) (operator.JobEvidenceResult, error) {
	if err := validateJobReadIdentifier("job_id", jobID, 256); err != nil ||
		strings.Contains(jobID, "/") || jobID == "." || jobID == ".." {
		return operator.JobEvidenceResult{}, errors.New(
			"operator client: job_id cannot be empty, contain control characters, dot segments, or slashes")
	}
	effectiveLimit := limit
	if effectiveLimit == 0 {
		effectiveLimit = operator.JobEvidenceDefaultLimit
	} else if effectiveLimit < 1 || effectiveLimit > operator.JobEvidenceDefaultLimit {
		return operator.JobEvidenceResult{}, fmt.Errorf(
			"operator client: job evidence limit must be between 1 and %d", operator.JobEvidenceDefaultLimit)
	}
	path := "/v1/operator/jobs/" + url.PathEscape(jobID) + "/evidence"
	if limit != 0 {
		path += "?limit=" + strconv.Itoa(limit)
	}
	req, err := c.newOperatorRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return operator.JobEvidenceResult{}, err
	}
	response, err := c.doRawWithLimit(req, maxJobEvidenceResponseBytes, "32 MiB")
	if err != nil {
		return operator.JobEvidenceResult{}, err
	}
	if response.status != http.StatusOK {
		return operator.JobEvidenceResult{}, fmt.Errorf(
			"operator client: job evidence returned unexpected success status HTTP %d", response.status)
	}
	if err := validateJobReadHeaders(response.header); err != nil {
		return operator.JobEvidenceResult{}, err
	}
	var result operator.JobEvidenceResult
	if err := decodeStrictJSONDocument(response.body, "job evidence", &result); err != nil {
		return operator.JobEvidenceResult{}, err
	}
	if err := validateJobEvidenceResult(result, jobID, effectiveLimit); err != nil {
		return operator.JobEvidenceResult{}, err
	}
	return result, nil
}

func validateJobEvidenceResult(result operator.JobEvidenceResult, jobID string, limit int) error {
	if result.SchemaVersion != operator.JobEvidenceSchemaVersion {
		return fmt.Errorf("operator client: unsupported job evidence schema_version %d", result.SchemaVersion)
	}
	if err := validateJobEvaluationTime(result.EvaluatedAt); err != nil {
		return err
	}
	if result.JobID != jobID || !deploy.IsKnownJobState(result.State) {
		return errors.New("operator client: job evidence identity or state is inconsistent")
	}
	for _, field := range []struct {
		name, value string
		max         int
	}{
		{"evidence.job_id", result.JobID, 256},
		{"evidence.machine_id", result.MachineID, 256},
		{"evidence.display_name", result.DisplayName, 256},
		{"desired.desired_id", result.Desired.DesiredID, 256},
		{"desired.scope_id", result.Desired.ScopeID, 256},
		{"desired.resource_kind", result.Desired.ResourceKind, 128},
		{"desired.resource_id", result.Desired.ResourceID, 256},
	} {
		if err := validateJobReadIdentifier(field.name, field.value, field.max); err != nil {
			return err
		}
	}
	if result.Disclosure.Limit != limit ||
		result.Disclosure.MaxFieldBytes != operator.JobEvidenceMaxFieldBytes ||
		!result.Disclosure.EventProvenanceRecordingEnabled ||
		result.Disclosure.VerificationProducerKind != operator.JobEvidenceProducerExecutorAgent ||
		!result.Disclosure.VerificationProvenanceRecordingEnabled ||
		!result.Disclosure.VerificationReceivedAtRecordingEnabled {
		return errors.New("operator client: job evidence disclosure metadata is inconsistent")
	}
	if (result.Desired.ScopeType != "machine" && result.Desired.ScopeType != "channel") ||
		(result.Desired.ScopeType == "machine" && result.Desired.ScopeID != result.MachineID) ||
		result.Desired.Revision < 0 || result.Desired.CreatedAt.IsZero() {
		return errors.New("operator client: job evidence desired metadata is inconsistent")
	}
	if err := requireUTCJobTime(result.Desired.CreatedAt, "desired.created_at"); err != nil {
		return err
	}
	if err := validateJobEvidenceText(result.Desired.Spec,
		result.Disclosure.MaxFieldBytes, "desired.spec"); err != nil {
		return err
	}
	if result.Events.Items == nil || result.Verifications.Items == nil {
		return errors.New("operator client: job evidence arrays must not be null")
	}
	if !validBoundedJobEvidencePageForLimit(
		result.Events.Total, len(result.Events.Items), result.Events.Truncated, limit) {
		return errors.New("operator client: job evidence event page totals are inconsistent")
	}
	seenEventIDs := make(map[string]bool, len(result.Events.Items))
	rejectedEvents := make(map[int]operator.JobEventEvidence)
	for i, event := range result.Events.Items {
		if validateJobReadIdentifier("event.event_id", event.EventID, 256) != nil || event.Seq < 0 ||
			!validJobEventPhase(event.Phase) || event.PayloadBytes < 0 ||
			event.OccurredAt.IsZero() || event.ReceivedAt.IsZero() {
			return errors.New("operator client: job evidence contains invalid event metadata")
		}
		if err := requireUTCJobTime(event.OccurredAt, "event.occurred_at"); err != nil {
			return err
		}
		if err := requireUTCJobTime(event.ReceivedAt, "event.received_at"); err != nil {
			return err
		}
		if err := validateJobEvidenceText(event.ReportedPhase,
			result.Disclosure.MaxFieldBytes, "event.reported_phase"); err != nil {
			return err
		}
		if err := validateJobEventEvidenceProducer(event.Producer, result); err != nil {
			return err
		}
		if seenEventIDs[event.EventID] || (i > 0 && event.Seq <= result.Events.Items[i-1].Seq) {
			return errors.New("operator client: job evidence events are duplicated or not strictly ordered")
		}
		seenEventIDs[event.EventID] = true
		if event.Phase == operator.JobEventRejected {
			rejectedEvents[event.Seq] = event
		}
	}
	if err := validateJobRejectionEvidence(result.Rejection, rejectedEvents,
		result.Disclosure.MaxFieldBytes); err != nil {
		return err
	}
	if result.Verifications.Passed < 0 || result.Verifications.Failed < 0 ||
		result.Verifications.Passed+result.Verifications.Failed != result.Verifications.Total ||
		!validBoundedJobEvidencePageForLimit(result.Verifications.Total,
			len(result.Verifications.Items), result.Verifications.Truncated, limit) {
		return errors.New("operator client: job evidence verification page totals are inconsistent")
	}
	seenVerificationIDs := make(map[string]bool, len(result.Verifications.Items))
	returnedPassed, returnedFailed := 0, 0
	for i, verification := range result.Verifications.Items {
		if validateJobReadIdentifier("verification.verification_id", verification.VerificationID, 256) != nil ||
			verification.ReportedVerifiedAt.IsZero() {
			return errors.New("operator client: job evidence contains invalid verification metadata")
		}
		if err := requireUTCJobTime(verification.ReportedVerifiedAt, "verification.reported_verified_at"); err != nil {
			return err
		}
		for _, field := range []struct {
			name string
			text operator.JobEvidenceText
		}{
			{"verification.rule_id", verification.RuleID},
			{"verification.command", verification.Command},
			{"verification.stdout_excerpt", verification.StdoutExcerpt},
			{"verification.stderr_excerpt", verification.StderrExcerpt},
		} {
			if err := validateJobEvidenceText(field.text,
				result.Disclosure.MaxFieldBytes, field.name); err != nil {
				return err
			}
		}
		if err := validateJobEvidenceProducer(verification.Producer, result,
			verification.ReceivedAt != nil); err != nil {
			return err
		}
		if verification.ReceivedAt != nil {
			if err := requireUTCJobTime(*verification.ReceivedAt, "verification.received_at"); err != nil {
				return err
			}
		}
		if seenVerificationIDs[verification.VerificationID] ||
			(i > 0 && verification.ReportedVerifiedAt.Before(
				result.Verifications.Items[i-1].ReportedVerifiedAt)) {
			return errors.New("operator client: job evidence verifications are duplicated or not ordered")
		}
		seenVerificationIDs[verification.VerificationID] = true
		if verification.Passed {
			returnedPassed++
		} else {
			returnedFailed++
		}
	}
	if (!result.Verifications.Truncated &&
		(returnedPassed != result.Verifications.Passed || returnedFailed != result.Verifications.Failed)) ||
		(result.Verifications.Truncated &&
			(returnedPassed > result.Verifications.Passed || returnedFailed > result.Verifications.Failed)) {
		return errors.New("operator client: returned job evidence outcomes contradict totals")
	}
	return validateJobIndependentEvidence(result, limit)
}

var knownIndependentVerdicts = map[string]bool{
	string(store.IndependentAbsent):            true,
	string(store.IndependentStale):             true,
	string(store.IndependentDigestMismatch):    true,
	string(store.IndependentReleaseMismatch):   true,
	string(store.IndependentReleaseUnreported): true,
	string(store.IndependentProducerRevoked):   true,
	string(store.IndependentPassed):            true,
	string(store.IndependentFailed):            true,
}

var knownJobAssignmentStates = map[string]bool{
	operator.JobAssignmentReported:        true,
	operator.JobAssignmentProducerRevoked: true,
	operator.JobAssignmentWaitingForJob:   true,
	operator.JobAssignmentAwaitingReport:  true,
}

// validateJobIndependentAssignments refuses a page whose assignment states
// disagree with the timestamps beside them. The state is the field an operator
// reads; a page that could render "awaiting_report" next to a report time would
// be telling two different stories at once.
func validateJobIndependentAssignments(section *operator.JobIndependentEvidence) error {
	seen := make(map[string]bool, len(section.Assignments))
	for _, assignment := range section.Assignments {
		if assignment.AssignmentID == "" || seen[assignment.AssignmentID] ||
			assignment.VerifierID == "" {
			return errors.New("operator client: job evidence contains invalid assignment identities")
		}
		seen[assignment.AssignmentID] = true
		if !knownJobAssignmentStates[assignment.State] {
			return errors.New("operator client: job evidence assignment state is unknown")
		}
		if err := requireUTCJobTime(assignment.AssignedAt, "independent.assignment.assigned_at"); err != nil {
			return err
		}
		if (assignment.ReportedAt != nil) != (assignment.State == operator.JobAssignmentReported) {
			return errors.New("operator client: job evidence assignment state contradicts its report time")
		}
		if assignment.ReportedAt != nil {
			if err := requireUTCJobTime(*assignment.ReportedAt,
				"independent.assignment.reported_at"); err != nil {
				return err
			}
			if assignment.ReportedAt.Before(assignment.AssignedAt) {
				return errors.New(
					"operator client: job evidence assignment was reported before it was made")
			}
		}
	}
	// An assignment is only satisfied by evidence, so a page that shows a report
	// without a single independent row has lost the row that satisfied it.
	if section.Total == 0 {
		for _, assignment := range section.Assignments {
			if assignment.State == operator.JobAssignmentReported {
				return errors.New(
					"operator client: job evidence reports an assignment with no independent evidence")
			}
		}
	}
	return nil
}

// validateJobIndependentEvidence enforces coherence, not prohibition. The
// disclosure flag claims somebody other than the executor can still speak for
// this job, and that claim must be backed by a live producer in the section.
func validateJobIndependentEvidence(result operator.JobEvidenceResult, limit int) error {
	section := result.Independent
	if section == nil {
		if result.Disclosure.IndependentVerifier {
			return errors.New("operator client: job evidence claims a second producer with no section")
		}
		return nil
	}
	if !knownIndependentVerdicts[section.Verdict] {
		return errors.New("operator client: job evidence independent verdict is unknown")
	}
	if section.Items == nil || section.Assignments == nil {
		return errors.New("operator client: job evidence arrays must not be null")
	}
	if err := validateJobIndependentAssignments(section); err != nil {
		return err
	}
	if section.LiveProducers < 0 || section.LiveProducers > section.Total ||
		!validBoundedJobEvidencePageForLimit(section.Total, len(section.Items),
			section.Truncated, limit) {
		return errors.New("operator client: job evidence independent page totals are inconsistent")
	}
	if result.Disclosure.IndependentVerifier != (section.LiveProducers > 0) {
		return errors.New("operator client: job evidence disclosure contradicts its live producers")
	}
	switch {
	case section.Total == 0 && (section.Verdict != string(store.IndependentAbsent) || len(section.Items) != 0):
		return errors.New("operator client: empty independent evidence must be absent")
	case section.Total > 0 && section.Verdict == string(store.IndependentAbsent):
		return errors.New("operator client: independent evidence cannot be absent and present")
	case section.LiveProducers == 0 && section.Total > 0 &&
		section.Verdict != string(store.IndependentProducerRevoked):
		return errors.New("operator client: independent evidence with no live producer must be producer_revoked")
	}
	if section.ArtifactDigest != "" && !validCanonicalSHA256(section.ArtifactDigest) {
		return errors.New("operator client: job evidence artifact digest is not canonical")
	}
	if section.ExpectedVersion != "" && !validJobIndependentVersion(
		"independent.expected_version", section.ExpectedVersion) {
		return errors.New("operator client: job evidence expected version is invalid")
	}
	if section.TerminalAt != nil {
		if err := requireUTCJobTime(*section.TerminalAt, "independent.terminal_at"); err != nil {
			return err
		}
	}
	seen := make(map[string]bool, len(section.Items))
	live := make(map[string]struct{}, len(section.Items))
	for _, row := range section.Items {
		if validateJobReadIdentifier("independent.verification_id", row.VerificationID, 256) != nil ||
			row.ReportedVerifiedAt.IsZero() || row.ReceivedAt.IsZero() || seen[row.VerificationID] {
			return errors.New("operator client: job evidence contains invalid independent metadata")
		}
		seen[row.VerificationID] = true
		for _, field := range []struct {
			name string
			at   time.Time
		}{
			{"independent.reported_verified_at", row.ReportedVerifiedAt},
			{"independent.received_at", row.ReceivedAt},
		} {
			if err := requireUTCJobTime(field.at, field.name); err != nil {
				return err
			}
		}
		for _, field := range []struct {
			name string
			text operator.JobEvidenceText
		}{
			{"independent.rule_id", row.RuleID},
			{"independent.command", row.Command},
			{"independent.stdout_excerpt", row.StdoutExcerpt},
			{"independent.stderr_excerpt", row.StderrExcerpt},
			{"independent.producer.display_name", row.Producer.DisplayName},
			{"independent.producer.failure_domain", row.Producer.FailureDomain},
		} {
			if err := validateJobEvidenceText(field.text,
				result.Disclosure.MaxFieldBytes, field.name); err != nil {
				return err
			}
		}
		if row.DigestReported != (row.ObservedDigest != "") ||
			(row.ObservedDigest != "" && !validCanonicalSHA256(row.ObservedDigest)) ||
			(row.DigestMatchesJob && (!row.DigestReported || row.ObservedDigest != section.ArtifactDigest)) {
			return errors.New("operator client: job evidence independent digest fields are inconsistent")
		}
		if row.VersionReported != (row.ObservedVersion != "") ||
			(row.ObservedVersion != "" && !validJobIndependentVersion(
				"independent.observed_version", row.ObservedVersion)) ||
			(row.ObservedVersion != "" && row.RuleID.Text != model.IndependentRuleOpenClawCurrentRelease) ||
			row.VersionMatchesJob != (row.VersionReported && section.ExpectedVersion != "" &&
				row.ObservedVersion == section.ExpectedVersion) {
			return errors.New("operator client: job evidence independent version fields are inconsistent")
		}
		if err := validateJobIndependentProducer(row.Producer, result); err != nil {
			return err
		}
		if row.Producer.State == store.VerifierStateActive {
			live[row.Producer.VerifierID] = struct{}{}
		}
	}
	if (!section.Truncated && len(live) != section.LiveProducers) ||
		(section.Truncated && len(live) > section.LiveProducers) {
		return errors.New("operator client: returned independent producers contradict totals")
	}
	return nil
}

func validJobIndependentVersion(name, value string) bool {
	return validateJobReadIdentifier(name, value, 128) == nil &&
		!strings.Contains(value, "/") && value != "." && value != ".."
}

func validateJobIndependentProducer(producer operator.JobIndependentProducer,
	result operator.JobEvidenceResult,
) error {
	if producer.Authority != operator.JobEvidenceAuthorityVerifierBearer ||
		producer.EvidenceRole != operator.JobEvidenceRoleIndependent ||
		validateJobReadIdentifier("independent.producer.verifier_id", producer.VerifierID, 256) != nil ||
		(producer.State != store.VerifierStateActive && producer.State != store.VerifierStateRevoked) {
		return errors.New("operator client: independent producer provenance is inconsistent")
	}
	switch producer.Kind {
	case store.VerifierKindFleetPeerAgent, store.VerifierKindHubProber, store.VerifierKindExternalJobRunner:
	default:
		return errors.New("operator client: independent producer kind is unknown")
	}
	// The separation rule is the whole reason this producer exists. A section
	// claiming the job's own machine as its failure domain is not independent.
	if producer.FailureDomain.Text == result.MachineID {
		return errors.New("operator client: independent producer shares the job's failure domain")
	}
	return nil
}

func validCanonicalSHA256(value string) bool {
	const prefix = "sha256:"
	if len(value) != len(prefix)+64 || !strings.HasPrefix(value, prefix) {
		return false
	}
	for _, char := range value[len(prefix):] {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

func validateJobEvidenceProducer(
	producer operator.JobEvidenceProducer,
	result operator.JobEvidenceResult,
	wantRecorded bool,
) error {
	if producer.Kind != operator.JobEvidenceProducerExecutorAgent ||
		producer.Authority != operator.JobEvidenceAuthorityMachineLease ||
		producer.EvidenceRole != operator.JobEvidenceRoleExecutor ||
		producer.ProducerID != result.MachineID || producer.DisplayName != result.DisplayName ||
		producer.ProvenanceRecorded != wantRecorded {
		return errors.New("operator client: job evidence producer provenance is inconsistent")
	}
	return nil
}

func validateJobEventEvidenceProducer(
	producer operator.JobEvidenceProducer,
	result operator.JobEvidenceResult,
) error {
	switch producer.Kind {
	case operator.JobEvidenceProducerExecutorAgent:
		if producer.Authority == operator.JobEvidenceAuthorityMachineLease &&
			producer.EvidenceRole == operator.JobEvidenceRoleExecutor &&
			producer.ProducerID == result.MachineID && producer.DisplayName == result.DisplayName {
			return nil
		}
	case operator.JobEvidenceProducerHubScheduler:
		if producer.Authority == operator.JobEvidenceAuthorityDependencyGraph &&
			producer.EvidenceRole == operator.JobEvidenceRoleScheduler &&
			producer.ProducerID == "hub" && producer.DisplayName == "Hub" &&
			producer.ProvenanceRecorded {
			return nil
		}
	}
	return errors.New("operator client: job event producer provenance is inconsistent")
}

func validateJobEvidenceText(value operator.JobEvidenceText, disclosureMaxBytes int, name string) error {
	field, ok := jobEvidenceTextPolicies[name]
	if !ok {
		return fmt.Errorf("operator client: %s has no text policy", name)
	}
	return validateEvidenceText(value, disclosureMaxBytes, name, field)
}

func validateJobRejectionEvidence(
	rejection *operator.JobRejectionEvidence,
	rejectedEvents map[int]operator.JobEventEvidence,
	disclosureMaxBytes int,
) error {
	if rejection == nil {
		return nil
	}
	event, found := rejectedEvents[rejection.Seq]
	if !found || rejection.EventID != event.EventID ||
		!rejection.OccurredAt.Equal(event.OccurredAt) ||
		!rejection.ReceivedAt.Equal(event.ReceivedAt) ||
		rejection.Producer != event.Producer {
		return errors.New("operator client: rejection does not correspond to a returned rejected event")
	}
	if err := requireUTCJobTime(rejection.OccurredAt, "rejection.occurred_at"); err != nil {
		return err
	}
	if err := requireUTCJobTime(rejection.ReceivedAt, "rejection.received_at"); err != nil {
		return err
	}
	if err := validateJobEvidenceText(rejection.Code, disclosureMaxBytes, "rejection.code"); err != nil {
		return err
	}
	if err := validateJobEvidenceText(rejection.Detail,
		disclosureMaxBytes, "rejection.detail"); err != nil {
		return err
	}
	if rejection.HasDetail != (rejection.Detail.Text != "") {
		return errors.New("operator client: rejection detail presence is inconsistent")
	}
	if !rejection.PayloadDecodable &&
		(rejection.Code.Text != "" || rejection.CodeKnown || rejection.HasDetail || rejection.Detail.Text != "") {
		return errors.New("operator client: undecodable rejection exposes decoded fields")
	}
	if rejection.CodeKnown &&
		(rejection.Code.Text == "" || !knownOperatorRejectionCode(rejection.Code.Text)) {
		return errors.New("operator client: rejection known-code claim is inconsistent")
	}
	return nil
}

func knownOperatorRejectionCode(value string) bool {
	switch deploy.RejectionCode(value) {
	case deploy.StaleRevision, deploy.DuplicateJobID, deploy.ArtifactHashMismatch,
		deploy.LeaseInvalid, deploy.IrreversibleMigration, deploy.PreconditionFailed,
		deploy.UnspecifiedModelSwitch, deploy.DependencyFailed:
		return true
	default:
		return false
	}
}

func validBoundedJobEvidencePageForLimit(total, returned int, truncated bool, limit int) bool {
	expected := total
	if expected > limit {
		expected = limit
	}
	return total >= 0 && returned == expected && truncated == (total > limit)
}
