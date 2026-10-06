package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/teddashh/AI-Intune/internal/deploy"
	"github.com/teddashh/AI-Intune/internal/model"
)

const MaxJobReadPageSize = 100

var (
	ErrInvalidJobRead = errors.New("store: invalid job read request")
	ErrJobReadCorrupt = errors.New("store: job read ledger is inconsistent")
)

// JobReadPosition is the immutable keyset coordinate used after a returned
// row. CreationCeiling is carried separately so a later page cannot admit a
// newly inserted, back-dated job.
type JobReadPosition struct {
	CreatedAt time.Time
	Revision  deploy.Revision
	JobID     string
}

type JobReadFilter struct {
	MachineID       string
	States          []deploy.JobState
	DeploymentID    string
	ResourceKind    string
	ResourceID      string
	Limit           int
	CreationCeiling *int64
	After           *JobReadPosition
}

type JobReadStateCount struct {
	State deploy.JobState
	Count int
}

// JobReadItem deliberately has no lease token, desired spec, command or output
// fields. Store.Job is a machine-protocol persistence shape and is unsafe to
// marshal on an operator surface.
type JobReadItem struct {
	JobID               string
	MachineID           string
	DisplayName         string
	DesiredID           string
	DesiredScopeType    string
	DesiredScopeID      string
	DeploymentID        *string
	ResourceKind        string
	ResourceID          string
	Revision            deploy.Revision
	State               deploy.JobState
	LeaseExpiresAt      *time.Time
	ExecutionTimeout    int
	ArtifactDigest      *string
	Irreversible        bool
	CreatedAt           time.Time
	TerminalAt          *time.Time
	EventCount          int
	LastEventReceivedAt *time.Time
	VerificationTotal   int
	VerificationPassed  int
	VerificationFailed  int
	// IndependentTotal counts rows written by a registered verifier. It is
	// deliberately separate from VerificationTotal: the executor counts feed the
	// deployment gate's meaning, and an independent producer must not move them.
	IndependentTotal int
}

type JobReadPage struct {
	Total           int
	StateCounts     []JobReadStateCount
	Items           []JobReadItem
	CreationCeiling int64
	Next            *JobReadPosition
}

// JobReadDetail is one exact, single-transaction native detail. Job.LeaseToken
// is always cleared, and raw desired/event/verification content is never
// selected.
type JobReadDetail struct {
	Item                   JobReadItem
	Job                    Job
	Machine                Machine
	Desired                DesiredState
	Events                 []JobEvent
	EventsTruncated        bool
	Verifications          []JobVerification
	VerificationsTruncated bool
}

// JobReadEvidence is the bounded native evidence required to build the typed
// operator disclosure. It deliberately omits Job (and therefore lease_token)
// and never selects desired_state.created_by.
type JobReadEvidence struct {
	Item                   JobReadItem
	Desired                DesiredState
	Events                 []JobEvent
	EventsTruncated        bool
	Verifications          []JobVerification
	VerificationsTruncated bool
	// Independent rows are paged separately so an independent producer can
	// never crowd executor evidence out of the same bounded page.
	Independent          []JobVerification
	IndependentTruncated bool
	// Verifiers carries every producer referenced by Independent, revoked ones
	// included: evidence must stay attributable after its producer is revoked.
	Verifiers map[string]Verifier
	// IndependentVerdict covers every independent row of the job, including
	// rows past the page limit.
	IndependentVerdict IndependentVerdict
	// IndependentExpectedVersion is non-empty only for a canonical OpenClaw
	// desired state. It is the Hub-side value used to compare structured
	// current-release evidence; stdout is never parsed for this purpose.
	IndependentExpectedVersion string
	// IndependentLiveProducers counts distinct non-revoked verifier identities
	// over every row rather than the page.
	IndependentLiveProducers int
}

func (s *Store) ListJobReads(filter JobReadFilter) (JobReadPage, error) {
	if err := validateJobReadFilter(filter); err != nil {
		return JobReadPage{}, err
	}
	tx, err := s.beginWrite(context.Background(), "list_job_reads")
	if err != nil {
		return JobReadPage{}, fmt.Errorf("store: begin job list read: %w", err)
	}
	defer tx.Rollback()

	var currentCeiling int64
	if err := tx.QueryRow(`SELECT COALESCE(MAX(rowid), 0) FROM jobs`).Scan(&currentCeiling); err != nil {
		return JobReadPage{}, fmt.Errorf("store: read job creation ceiling: %w", err)
	}
	ceiling := currentCeiling
	if filter.CreationCeiling != nil {
		ceiling = *filter.CreationCeiling
		if ceiling < 0 || ceiling > currentCeiling {
			return JobReadPage{}, fmt.Errorf("%w: creation ceiling is outside this ledger", ErrInvalidJobRead)
		}
	}

	baseWhere, baseArgs := jobReadWhere(filter, ceiling, false)
	rows, err := tx.Query(`SELECT j.state, COUNT(*)
 FROM jobs j LEFT JOIN desired_state d ON d.desired_id=j.desired_id
 WHERE `+baseWhere+` GROUP BY j.state ORDER BY j.state`, baseArgs...)
	if err != nil {
		return JobReadPage{}, fmt.Errorf("store: count filtered jobs: %w", err)
	}
	counts := make(map[deploy.JobState]int, len(deploy.AllJobStates))
	total := 0
	for rows.Next() {
		var state deploy.JobState
		var count int
		if err := rows.Scan(&state, &count); err != nil {
			_ = rows.Close()
			return JobReadPage{}, fmt.Errorf("store: scan filtered job count: %w", err)
		}
		if !deploy.IsKnownJobState(state) || count < 0 {
			_ = rows.Close()
			return JobReadPage{}, fmt.Errorf("%w: unknown state %q or negative count", ErrJobReadCorrupt, state)
		}
		counts[state] = count
		total += count
	}
	if err := rows.Close(); err != nil {
		return JobReadPage{}, fmt.Errorf("store: close filtered job counts: %w", err)
	}
	if err := rows.Err(); err != nil {
		return JobReadPage{}, fmt.Errorf("store: finish filtered job counts: %w", err)
	}

	pageWhere, pageArgs := jobReadWhere(filter, ceiling, true)
	pageArgs = append(pageArgs, filter.Limit+1)
	itemRows, err := tx.Query(`SELECT `+jobReadItemColumns+`
 FROM jobs j
 LEFT JOIN machine_registry m ON m.machine_id=j.machine_id
 LEFT JOIN desired_state d ON d.desired_id=j.desired_id
 WHERE `+pageWhere+`
 ORDER BY j.created_at DESC, j.revision DESC, j.job_id DESC
 LIMIT ?`, pageArgs...)
	if err != nil {
		return JobReadPage{}, fmt.Errorf("store: list filtered jobs: %w", err)
	}
	items := make([]JobReadItem, 0, filter.Limit+1)
	for itemRows.Next() {
		item, err := scanJobReadItem(itemRows)
		if err != nil {
			_ = itemRows.Close()
			return JobReadPage{}, err
		}
		items = append(items, item)
	}
	if err := itemRows.Close(); err != nil {
		return JobReadPage{}, fmt.Errorf("store: close filtered jobs: %w", err)
	}
	if err := itemRows.Err(); err != nil {
		return JobReadPage{}, fmt.Errorf("store: finish filtered jobs: %w", err)
	}

	var next *JobReadPosition
	if len(items) > filter.Limit {
		items = items[:filter.Limit]
		last := items[len(items)-1]
		next = &JobReadPosition{CreatedAt: last.CreatedAt, Revision: last.Revision, JobID: last.JobID}
	}
	stateCounts := make([]JobReadStateCount, 0, len(deploy.AllJobStates))
	for _, state := range deploy.AllJobStates {
		stateCounts = append(stateCounts, JobReadStateCount{State: state, Count: counts[state]})
	}
	page := JobReadPage{
		Total: total, StateCounts: stateCounts, Items: items,
		CreationCeiling: ceiling, Next: next,
	}
	if err := tx.Commit(); err != nil {
		return JobReadPage{}, fmt.Errorf("store: commit job list read: %w", err)
	}
	return page, nil
}

func validateJobReadFilter(filter JobReadFilter) error {
	if filter.Limit < 1 || filter.Limit > MaxJobReadPageSize {
		return fmt.Errorf("%w: limit must be between 1 and %d", ErrInvalidJobRead, MaxJobReadPageSize)
	}
	for name, value := range map[string]string{
		"machine_id": filter.MachineID, "deployment_id": filter.DeploymentID,
		"resource_kind": filter.ResourceKind, "resource_id": filter.ResourceID,
	} {
		maxBytes := 256
		if name == "resource_kind" {
			maxBytes = 128
		}
		if value != "" && !validJobReadStoredIdentifier(value, maxBytes) {
			return fmt.Errorf("%w: %s is not a canonical identifier", ErrInvalidJobRead, name)
		}
	}
	if filter.ResourceID != "" && filter.ResourceKind == "" {
		return fmt.Errorf("%w: resource_id requires resource_kind", ErrInvalidJobRead)
	}
	seen := make(map[deploy.JobState]bool, len(filter.States))
	for _, state := range filter.States {
		if !deploy.IsKnownJobState(state) || seen[state] {
			return fmt.Errorf("%w: invalid or duplicate state %q", ErrInvalidJobRead, state)
		}
		seen[state] = true
	}
	if filter.After != nil {
		if filter.CreationCeiling == nil || filter.After.CreatedAt.IsZero() ||
			filter.After.CreatedAt.Nanosecond() != 0 || filter.After.Revision < 0 ||
			!validJobReadRouteIdentifier(filter.After.JobID, 256) {
			return fmt.Errorf("%w: incomplete continuation position", ErrInvalidJobRead)
		}
	}
	return nil
}

func jobReadWhere(filter JobReadFilter, ceiling int64, includeAfter bool) (string, []any) {
	clauses := []string{"j.rowid <= ?"}
	args := []any{ceiling}
	if filter.MachineID != "" {
		clauses = append(clauses, "j.machine_id = ?")
		args = append(args, filter.MachineID)
	}
	if len(filter.States) > 0 {
		placeholders := make([]string, len(filter.States))
		for i, state := range filter.States {
			placeholders[i] = "?"
			args = append(args, state)
		}
		clauses = append(clauses, "j.state IN ("+strings.Join(placeholders, ",")+")")
	}
	if filter.DeploymentID != "" {
		clauses = append(clauses, `EXISTS (SELECT 1 FROM deployment_targets ft
 WHERE ft.job_id=j.job_id AND ft.deployment_id=?)`)
		args = append(args, filter.DeploymentID)
	}
	if filter.ResourceKind != "" {
		clauses = append(clauses, "d.resource_kind = ?")
		args = append(args, filter.ResourceKind)
	}
	if filter.ResourceID != "" {
		clauses = append(clauses, "d.resource_id = ?")
		args = append(args, filter.ResourceID)
	}
	if includeAfter && filter.After != nil {
		created := fmtTime(filter.After.CreatedAt)
		clauses = append(clauses, `(j.created_at < ? OR
 (j.created_at = ? AND (j.revision < ? OR
  (j.revision = ? AND j.job_id < ?))))`)
		args = append(args, created, created, filter.After.Revision, filter.After.Revision, filter.After.JobID)
	}
	return strings.Join(clauses, " AND "), args
}

const jobReadItemColumns = `j.job_id,j.machine_id,m.display_name,j.desired_id,
 d.scope_type,d.scope_id,d.resource_kind,d.resource_id,d.revision,j.revision,j.state,j.lease_expires_at,
 j.execution_timeout,j.artifact_digest,j.irreversible,j.created_at,j.terminal_at,
 (SELECT MIN(dt.deployment_id) FROM deployment_targets dt WHERE dt.job_id=j.job_id),
 (SELECT COUNT(*) FROM deployment_targets dt WHERE dt.job_id=j.job_id),
 (SELECT COUNT(*) FROM job_events e WHERE e.job_id=j.job_id),
 (SELECT MAX(e.received_at) FROM job_events e WHERE e.job_id=j.job_id),
 (SELECT COUNT(*) FROM verification_results v
   WHERE v.job_id=j.job_id AND v.evidence_role='executor'),
 (SELECT COUNT(*) FROM verification_results v
   WHERE v.job_id=j.job_id AND v.evidence_role='executor' AND v.passed=1),
 (SELECT COUNT(*) FROM verification_results v
   WHERE v.job_id=j.job_id AND v.evidence_role='executor' AND v.passed=0),
 (SELECT COUNT(*) FROM verification_results v
   WHERE v.job_id=j.job_id AND v.machine_id<>j.machine_id),
 (SELECT COUNT(*) FROM verification_results v
   WHERE v.job_id=j.job_id AND v.evidence_role='independent_verifier')`

type jobReadScanner interface{ Scan(...any) error }

func scanJobReadItem(row jobReadScanner) (JobReadItem, error) {
	var item JobReadItem
	var displayName, desiredID, desiredScopeType, desiredScopeID, resourceKind, resourceID sql.NullString
	var state string
	var leaseExpires, digest, createdAt, terminalAt, deploymentID, lastEvent sql.NullString
	var desiredRevision sql.NullInt64
	var deploymentLinks, verificationMachineMismatches int
	if err := row.Scan(
		&item.JobID, &item.MachineID, &displayName, &desiredID,
		&desiredScopeType, &desiredScopeID, &resourceKind, &resourceID,
		&desiredRevision, &item.Revision, &state, &leaseExpires,
		&item.ExecutionTimeout, &digest, &item.Irreversible, &createdAt, &terminalAt,
		&deploymentID, &deploymentLinks, &item.EventCount, &lastEvent,
		&item.VerificationTotal, &item.VerificationPassed, &item.VerificationFailed,
		&verificationMachineMismatches, &item.IndependentTotal,
	); err != nil {
		return JobReadItem{}, err
	}
	item.DisplayName, item.DesiredID = displayName.String, desiredID.String
	item.DesiredScopeType, item.DesiredScopeID = desiredScopeType.String, desiredScopeID.String
	item.ResourceKind, item.ResourceID = resourceKind.String, resourceID.String
	item.State = deploy.JobState(state)
	item.LeaseExpiresAt = parseTimePtr(leaseExpires)
	item.CreatedAt, item.TerminalAt = parseTimeNull(createdAt), parseTimePtr(terminalAt)
	item.LastEventReceivedAt = parseTimePtr(lastEvent)
	if !desiredRevision.Valid || deploy.Revision(desiredRevision.Int64) != item.Revision {
		return JobReadItem{}, fmt.Errorf("%w: desired-state revision does not match job", ErrJobReadCorrupt)
	}
	if !createdAt.Valid || item.CreatedAt.IsZero() || createdAt.String != fmtTime(item.CreatedAt) ||
		(leaseExpires.Valid && (item.LeaseExpiresAt == nil || leaseExpires.String != fmtTime(*item.LeaseExpiresAt))) ||
		(terminalAt.Valid && (item.TerminalAt == nil || terminalAt.String != fmtTime(*item.TerminalAt))) ||
		(lastEvent.Valid && (item.LastEventReceivedAt == nil || lastEvent.String != fmtTime(*item.LastEventReceivedAt))) {
		return JobReadItem{}, fmt.Errorf("%w: job contains an invalid timestamp", ErrJobReadCorrupt)
	}
	if digest.Valid {
		value := digest.String
		item.ArtifactDigest = &value
	}
	if deploymentID.Valid {
		value := deploymentID.String
		item.DeploymentID = &value
	}
	if err := validateJobReadItem(item, deploymentLinks, verificationMachineMismatches); err != nil {
		return JobReadItem{}, err
	}
	return item, nil
}

func validateJobReadItem(item JobReadItem, deploymentLinks, verificationMachineMismatches int) error {
	switch {
	case !validJobReadRouteIdentifier(item.JobID, 256) ||
		!validJobReadStoredIdentifier(item.MachineID, 256) ||
		!validJobReadStoredIdentifier(item.DisplayName, 256):
		return fmt.Errorf("%w: job or machine identity is missing", ErrJobReadCorrupt)
	case !validJobReadStoredIdentifier(item.DesiredID, 256) ||
		!validJobReadStoredIdentifier(item.ResourceKind, 128) ||
		!validJobReadStoredIdentifier(item.ResourceID, 256):
		return fmt.Errorf("%w: desired-state relation is missing", ErrJobReadCorrupt)
	case (item.DesiredScopeType != "machine" && item.DesiredScopeType != "channel") ||
		!validJobReadStoredIdentifier(item.DesiredScopeID, 256):
		return fmt.Errorf("%w: desired-state scope is invalid", ErrJobReadCorrupt)
	case item.DesiredScopeType == "machine" && item.DesiredScopeID != item.MachineID:
		return fmt.Errorf("%w: machine-scoped desired state targets another machine", ErrJobReadCorrupt)
	case !deploy.IsKnownJobState(item.State):
		return fmt.Errorf("%w: unknown job state %q", ErrJobReadCorrupt, item.State)
	case item.Revision < 0 || item.ExecutionTimeout <= 0 || item.CreatedAt.IsZero():
		return fmt.Errorf("%w: invalid revision, timeout or creation time", ErrJobReadCorrupt)
	case deploymentLinks < 0 || deploymentLinks > 1:
		return fmt.Errorf("%w: job has %d deployment links", ErrJobReadCorrupt, deploymentLinks)
	case deploymentLinks == 1 && (item.DeploymentID == nil ||
		!validJobReadStoredIdentifier(*item.DeploymentID, 256)):
		return fmt.Errorf("%w: job has an invalid deployment identity", ErrJobReadCorrupt)
	case verificationMachineMismatches != 0:
		return fmt.Errorf("%w: job has verification evidence from another machine", ErrJobReadCorrupt)
	case item.EventCount < 0 || item.VerificationTotal < 0 ||
		item.VerificationPassed < 0 || item.VerificationFailed < 0 || item.IndependentTotal < 0:
		return fmt.Errorf("%w: negative evidence count", ErrJobReadCorrupt)
	case item.VerificationPassed+item.VerificationFailed != item.VerificationTotal:
		return fmt.Errorf("%w: verification outcome counts do not cover every result", ErrJobReadCorrupt)
	case item.State == deploy.Succeeded &&
		(item.VerificationTotal == 0 || item.VerificationFailed != 0):
		return fmt.Errorf("%w: succeeded job lacks an all-passing verification set", ErrJobReadCorrupt)
	case item.State == deploy.Failed && item.Irreversible:
		return fmt.Errorf("%w: irreversible job is incorrectly marked failed", ErrJobReadCorrupt)
	case item.State == deploy.ManualIntervention && !item.Irreversible:
		return fmt.Errorf("%w: reversible job is incorrectly marked manual intervention", ErrJobReadCorrupt)
	case item.EventCount == 0 && item.LastEventReceivedAt != nil:
		return fmt.Errorf("%w: empty event stream has a latest timestamp", ErrJobReadCorrupt)
	case item.EventCount > 0 && item.LastEventReceivedAt == nil:
		return fmt.Errorf("%w: event stream has no valid Hub receive timestamp", ErrJobReadCorrupt)
	case deploy.IsTerminal(item.State) && item.TerminalAt == nil:
		return fmt.Errorf("%w: terminal job has no terminal_at", ErrJobReadCorrupt)
	case !deploy.IsTerminal(item.State) && item.TerminalAt != nil:
		return fmt.Errorf("%w: nonterminal job has terminal_at", ErrJobReadCorrupt)
	case deploy.IsTerminal(item.State) && item.LeaseExpiresAt != nil:
		return fmt.Errorf("%w: terminal job retained lease metadata", ErrJobReadCorrupt)
	case item.State == deploy.NotStarted && item.LeaseExpiresAt != nil:
		return fmt.Errorf("%w: unclaimed job has lease metadata", ErrJobReadCorrupt)
	case item.State == deploy.NotStarted && (item.EventCount != 0 || item.VerificationTotal != 0):
		return fmt.Errorf("%w: unclaimed job has execution evidence", ErrJobReadCorrupt)
	}
	return nil
}

func validJobReadStoredIdentifier(value string, maxBytes int) bool {
	if value == "" || value != strings.TrimSpace(value) || len(value) > maxBytes {
		return false
	}
	for _, char := range value {
		if unicode.IsControl(char) || unicode.Is(unicode.Cf, char) {
			return false
		}
	}
	return true
}

func validJobReadRouteIdentifier(value string, maxBytes int) bool {
	return validJobReadStoredIdentifier(value, maxBytes) &&
		!strings.Contains(value, "/") && value != "." && value != ".."
}

// JobReadDetail returns safe metadata from one SQLite read transaction. It
// does not select lease_token, desired spec/creator, event payload, or verifier
// rule/command/output columns.
func (s *Store) JobReadDetail(jobID string, evidenceLimit int) (JobReadDetail, error) {
	return s.jobReadDetail(jobID, evidenceLimit)
}

// JobReadEvidence returns raw desired, event, and verification evidence from
// one read transaction. The result never includes a job lease token or the
// desired-state creator.
func (s *Store) JobReadEvidence(jobID string, limit int) (JobReadEvidence, error) {
	if err := validateJobReadDetailTarget(jobID, limit); err != nil {
		return JobReadEvidence{}, err
	}
	tx, err := s.beginWrite(context.Background(), "job_read_evidence")
	if err != nil {
		return JobReadEvidence{}, fmt.Errorf("store: begin job evidence read: %w", err)
	}
	defer tx.Rollback()

	item, desired, err := readJobItemAndDesired(tx, jobID, jobDesiredEvidence)
	if err != nil {
		return JobReadEvidence{}, err
	}
	events, eventsTruncated, err := readBoundedJobEvents(
		tx, item.JobID, item.MachineID, limit, item.EventCount, true)
	if err != nil {
		return JobReadEvidence{}, err
	}
	verifications, verificationsTruncated, err := readBoundedJobVerifications(
		tx, item.JobID, item.MachineID, JobVerificationRoleExecutor, limit, item.VerificationTotal, true)
	if err != nil {
		return JobReadEvidence{}, err
	}
	independent, independentTruncated, err := readBoundedJobVerifications(
		tx, item.JobID, item.MachineID, JobVerificationRoleIndependent, limit, item.IndependentTotal, true)
	if err != nil {
		return JobReadEvidence{}, err
	}
	verifiers, err := readJobEvidenceVerifiers(tx, independent)
	if err != nil {
		return JobReadEvidence{}, err
	}
	artifactDigest := ""
	if item.ArtifactDigest != nil {
		artifactDigest = *item.ArtifactDigest
	}
	expectedVersion := jobIndependentExpectedVersion(desired)
	verdictCounts, err := readIndependentVerdictCounts(
		tx, item.JobID, artifactDigest, expectedVersion, item.TerminalAt)
	if err != nil {
		return JobReadEvidence{}, err
	}
	if verdictCounts.Rows != item.IndependentTotal {
		return JobReadEvidence{}, fmt.Errorf("%w: independent evidence count does not match its producers",
			ErrJobReadCorrupt)
	}
	result := JobReadEvidence{
		Item: item, Desired: desired,
		Events: events, EventsTruncated: eventsTruncated,
		Verifications: verifications, VerificationsTruncated: verificationsTruncated,
		Independent: independent, IndependentTruncated: independentTruncated,
		Verifiers:                  verifiers,
		IndependentVerdict:         EvaluateIndependentVerdictCounts(verdictCounts),
		IndependentExpectedVersion: expectedVersion,
		IndependentLiveProducers:   verdictCounts.LiveProducers,
	}
	if err := tx.Commit(); err != nil {
		return JobReadEvidence{}, fmt.Errorf("store: commit job evidence read: %w", err)
	}
	return result, nil
}

func (s *Store) jobReadDetail(jobID string, evidenceLimit int) (JobReadDetail, error) {
	if err := validateJobReadDetailTarget(jobID, evidenceLimit); err != nil {
		return JobReadDetail{}, err
	}
	tx, err := s.beginWrite(context.Background(), "job_read_detail")
	if err != nil {
		return JobReadDetail{}, fmt.Errorf("store: begin job detail read: %w", err)
	}
	defer tx.Rollback()

	item, desired, err := readJobItemAndDesired(tx, jobID, jobDesiredSafe)
	if err != nil {
		return JobReadDetail{}, err
	}

	events, eventsTruncated, err := readBoundedJobEvents(
		tx, item.JobID, item.MachineID, evidenceLimit, item.EventCount, false)
	if err != nil {
		return JobReadDetail{}, err
	}
	verifications, verificationsTruncated, err := readBoundedJobVerifications(
		tx, item.JobID, item.MachineID, JobVerificationRoleExecutor, evidenceLimit, item.VerificationTotal, false)
	if err != nil {
		return JobReadDetail{}, err
	}
	job := Job{
		JobID: item.JobID, MachineID: item.MachineID, DesiredID: item.DesiredID,
		Revision: item.Revision, State: item.State, LeaseExpiresAt: item.LeaseExpiresAt,
		ExecutionTimeout: item.ExecutionTimeout, Irreversible: item.Irreversible,
		CreatedAt: item.CreatedAt, TerminalAt: item.TerminalAt,
	}
	if item.ArtifactDigest != nil {
		job.ArtifactDigest = *item.ArtifactDigest
	}
	detail := JobReadDetail{
		Item: item, Job: job,
		Machine: Machine{MachineID: item.MachineID, DisplayName: item.DisplayName},
		Desired: desired, Events: events, EventsTruncated: eventsTruncated,
		Verifications: verifications, VerificationsTruncated: verificationsTruncated,
	}
	if err := tx.Commit(); err != nil {
		return JobReadDetail{}, fmt.Errorf("store: commit job detail read: %w", err)
	}
	return detail, nil
}

func validateJobReadDetailTarget(jobID string, limit int) error {
	if !validJobReadRouteIdentifier(jobID, 256) || limit < 1 || limit > MaxJobReadPageSize {
		return fmt.Errorf("%w: invalid detail target or evidence limit", ErrInvalidJobRead)
	}
	return nil
}

type jobDesiredReadMode int

const (
	jobDesiredSafe jobDesiredReadMode = iota
	jobDesiredEvidence
)

func readJobItemAndDesired(tx dbTx, jobID string, mode jobDesiredReadMode) (JobReadItem, DesiredState, error) {
	item, err := scanJobReadItem(tx.QueryRow(`SELECT `+jobReadItemColumns+`
 FROM jobs j
 LEFT JOIN machine_registry m ON m.machine_id=j.machine_id
 LEFT JOIN desired_state d ON d.desired_id=j.desired_id
 WHERE j.job_id=?`, jobID))
	if errors.Is(err, sql.ErrNoRows) {
		return JobReadItem{}, DesiredState{}, ErrJobNotFound
	}
	if err != nil {
		return JobReadItem{}, DesiredState{}, fmt.Errorf("store: read job detail summary: %w", err)
	}
	desired, desiredCreatedAt, err := readJobDesired(tx, item.DesiredID, mode)
	if err != nil {
		return JobReadItem{}, DesiredState{}, fmt.Errorf("%w: read desired state: %v", ErrJobReadCorrupt, err)
	}
	desired.CreatedAt = parseTimeNull(desiredCreatedAt)
	if !desiredCreatedAt.Valid || desired.CreatedAt.IsZero() ||
		desiredCreatedAt.String != fmtTime(desired.CreatedAt) ||
		desired.DesiredID != item.DesiredID || desired.Revision != item.Revision ||
		desired.ResourceKind != item.ResourceKind || desired.ResourceID != item.ResourceID ||
		desired.ScopeType != item.DesiredScopeType || desired.ScopeID != item.DesiredScopeID {
		return JobReadItem{}, DesiredState{}, fmt.Errorf("%w: desired-state metadata changed within detail", ErrJobReadCorrupt)
	}
	return item, desired, nil
}

func readJobDesired(tx dbTx, desiredID string, mode jobDesiredReadMode) (DesiredState, sql.NullString, error) {
	var desired DesiredState
	var createdAt sql.NullString
	if mode == jobDesiredEvidence {
		err := tx.QueryRow(`SELECT desired_id,scope_type,scope_id,resource_kind,resource_id,
 revision,spec,created_at FROM desired_state WHERE desired_id=?`, desiredID).Scan(
			&desired.DesiredID, &desired.ScopeType, &desired.ScopeID, &desired.ResourceKind,
			&desired.ResourceID, &desired.Revision, &desired.Spec, &createdAt)
		return desired, createdAt, err
	}
	err := tx.QueryRow(`SELECT desired_id,scope_type,scope_id,resource_kind,resource_id,
 revision,created_at FROM desired_state WHERE desired_id=?`, desiredID).Scan(
		&desired.DesiredID, &desired.ScopeType, &desired.ScopeID, &desired.ResourceKind,
		&desired.ResourceID, &desired.Revision, &createdAt)
	return desired, createdAt, err
}

func readBoundedJobEvents(tx dbTx, jobID, machineID string, limit, total int, includeRaw bool) ([]JobEvent, bool, error) {
	columns := `event_id,job_id,seq,
 CASE WHEN phase IN ('start','finish','rejected') THEN phase ELSE 'other' END,
 occurred_at,received_at,producer_kind,producer_id,evidence_role,authority,provenance_recorded`
	if includeRaw {
		columns = "event_id,job_id,seq,phase,occurred_at,received_at,payload," +
			"producer_kind,producer_id,evidence_role,authority,provenance_recorded"
	}
	rows, err := tx.Query(`SELECT `+columns+`
 FROM job_events WHERE job_id=?
 ORDER BY seq DESC,received_at DESC,event_id DESC LIMIT ?`, jobID, limit)
	if err != nil {
		return nil, false, fmt.Errorf("store: read bounded job events: %w", err)
	}
	defer rows.Close()
	result := make([]JobEvent, 0, limit)
	for rows.Next() {
		var event JobEvent
		var occurredAt, receivedAt string
		destinations := []any{&event.EventID, &event.JobID, &event.Seq, &event.Phase,
			&occurredAt, &receivedAt}
		if includeRaw {
			destinations = append(destinations, &event.Payload)
		}
		destinations = append(destinations, &event.ProducerKind, &event.ProducerID,
			&event.EvidenceRole, &event.Authority, &event.ProvenanceRecorded)
		if err := rows.Scan(destinations...); err != nil {
			return nil, false, fmt.Errorf("store: scan bounded job event: %w", err)
		}
		event.OccurredAt, event.ReceivedAt = parseTime(occurredAt), parseTime(receivedAt)
		if !validJobReadStoredIdentifier(event.EventID, 256) || event.JobID != jobID || event.Seq < 0 ||
			event.OccurredAt.IsZero() || event.ReceivedAt.IsZero() ||
			occurredAt != fmtTime(event.OccurredAt) || receivedAt != fmtTime(event.ReceivedAt) ||
			!validJobEventProducer(event, machineID) {
			return nil, false, fmt.Errorf("%w: invalid job event metadata", ErrJobReadCorrupt)
		}
		result = append(result, event)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("store: finish bounded job events: %w", err)
	}
	reverseJobEvents(result)
	return result, total > len(result), nil
}

func validJobEventProducer(event JobEvent, machineID string) bool {
	switch event.ProducerKind {
	case JobEventProducerExecutorAgent:
		return event.ProducerID == machineID && event.EvidenceRole == JobEventRoleExecutor &&
			event.Authority == JobEventAuthorityMachineLease
	case JobEventProducerHubScheduler:
		return event.ProvenanceRecorded && event.ProducerID == "hub" &&
			event.EvidenceRole == JobEventRoleScheduler &&
			event.Authority == JobEventAuthorityDependencyGraph
	default:
		return false
	}
}

func readBoundedJobVerifications(tx dbTx, jobID, machineID, role string, limit, total int, includeRaw bool) ([]JobVerification, bool, error) {
	columns := "verification_id,job_id,machine_id,exit_code,passed,verified_at," +
		"producer_kind,producer_id,evidence_role,authority,provenance_recorded,received_at," +
		"observed_digest,observed_version,verifier_id"
	if includeRaw {
		columns = "verification_id,job_id,machine_id,rule_id,command,exit_code," +
			"stdout_excerpt,stderr_excerpt,passed,verified_at,producer_kind,producer_id," +
			"evidence_role,authority,provenance_recorded,received_at,observed_digest,observed_version,verifier_id"
	}
	rows, err := tx.Query(`SELECT `+columns+`
 FROM verification_results WHERE job_id=? AND evidence_role=?
 ORDER BY verified_at DESC,verification_id DESC LIMIT ?`, jobID, role, limit)
	if err != nil {
		return nil, false, fmt.Errorf("store: read bounded job verifications: %w", err)
	}
	defer rows.Close()
	result := make([]JobVerification, 0, limit)
	for rows.Next() {
		var verification JobVerification
		var exitCode sql.NullInt64
		var stdout, stderr, verifiedAt, receivedAt sql.NullString
		destinations := []any{&verification.VerificationID, &verification.JobID,
			&verification.MachineID}
		if includeRaw {
			destinations = append(destinations, &verification.RuleID, &verification.Command,
				&exitCode, &stdout, &stderr, &verification.Passed, &verifiedAt)
		} else {
			destinations = append(destinations, &exitCode, &verification.Passed, &verifiedAt)
		}
		destinations = append(destinations, &verification.ProducerKind, &verification.ProducerID,
			&verification.EvidenceRole, &verification.Authority, &verification.ProvenanceRecorded,
			&receivedAt, &verification.ObservedDigest, &verification.ObservedVersion,
			&verification.VerifierID)
		if err := rows.Scan(destinations...); err != nil {
			return nil, false, fmt.Errorf("store: scan bounded job verification: %w", err)
		}
		if exitCode.Valid {
			value := int(exitCode.Int64)
			verification.ExitCode = &value
		}
		verification.StdoutExcerpt, verification.StderrExcerpt = stdout.String, stderr.String
		verification.VerifiedAt = parseTimeNull(verifiedAt)
		verification.ReceivedAt = parseTimeNull(receivedAt)
		if !validJobReadStoredIdentifier(verification.VerificationID, 256) ||
			verification.JobID != jobID || verification.MachineID != machineID ||
			verification.EvidenceRole != role ||
			verification.VerifiedAt.IsZero() || verifiedAt.String != fmtTime(verification.VerifiedAt) ||
			(!verification.ReceivedAt.IsZero() && receivedAt.String != fmtTime(verification.ReceivedAt)) ||
			!validJobVerificationProducer(verification, machineID) {
			return nil, false, fmt.Errorf("%w: invalid verification metadata", ErrJobReadCorrupt)
		}
		result = append(result, verification)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("store: finish bounded job verifications: %w", err)
	}
	reverseJobVerifications(result)
	return result, total > len(result), nil
}

func validJobVerificationProducer(verification JobVerification, machineID string) bool {
	switch verification.EvidenceRole {
	case JobVerificationRoleExecutor:
		return verification.ProducerKind == JobVerificationProducerExecutorAgent &&
			verification.Authority == JobVerificationAuthorityMachineLease &&
			(verification.ProducerID == "" || verification.ProducerID == machineID) &&
			(!verification.ProvenanceRecorded || (verification.ProducerID == machineID &&
				!verification.ReceivedAt.IsZero())) &&
			(!verification.ProvenanceRecorded || !verification.ReceivedAt.IsZero()) &&
			(verification.ProvenanceRecorded || verification.ReceivedAt.IsZero()) &&
			verification.ObservedDigest == "" && verification.ObservedVersion == "" &&
			verification.VerifierID == ""
	case JobVerificationRoleIndependent:
		return validVerifierKind(verification.ProducerKind) &&
			verification.Authority == JobVerificationAuthorityVerifierBearer &&
			verification.ProvenanceRecorded && !verification.ReceivedAt.IsZero() &&
			verification.ProducerID != "" && verification.ProducerID == verification.VerifierID &&
			validObservedDigest(verification.ObservedDigest) &&
			validIndependentObservedVersion(verification.RuleID, verification.Passed,
				verification.ObservedVersion, false)
	default:
		return false
	}
}

func reverseJobEvents(items []JobEvent) {
	for left, right := 0, len(items)-1; left < right; left, right = left+1, right-1 {
		items[left], items[right] = items[right], items[left]
	}
}

func reverseJobVerifications(items []JobVerification) {
	for left, right := 0, len(items)-1; left < right; left, right = left+1, right-1 {
		items[left], items[right] = items[right], items[left]
	}
}

// readJobEvidenceVerifiers loads the producer identity for every independent
// row in one page. A row whose verifier row is missing is a corrupt ledger, not
// a row to render anonymously: evidence that cannot name its producer is not
// evidence.
func readJobEvidenceVerifiers(tx dbTx, rows []JobVerification) (map[string]Verifier, error) {
	if len(rows) == 0 {
		return nil, nil
	}
	verifiers := make(map[string]Verifier, len(rows))
	for _, row := range rows {
		if _, done := verifiers[row.VerifierID]; done {
			continue
		}
		verifier, err := scanVerifier(tx.QueryRow(`SELECT `+verifierColumns+`
 FROM verifiers WHERE verifier_id=?`, row.VerifierID))
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("%w: independent evidence has no producer row", ErrJobReadCorrupt)
		}
		if err != nil {
			return nil, fmt.Errorf("store: read independent evidence producer: %w", err)
		}
		if !validVerifierRow(verifier) {
			return nil, fmt.Errorf("%w: independent evidence producer row is invalid", ErrJobReadCorrupt)
		}
		verifiers[verifier.VerifierID] = verifier
	}
	return verifiers, nil
}

// readIndependentVerdictCounts aggregates over every independent row of one
// job, not over the bounded page. A verdict that only saw the first page could
// call a job passed while an unrendered row reported a digest clash.
func readIndependentVerdictCounts(tx dbTx, jobID, artifactDigest, expectedVersion string,
	terminalAt *time.Time,
) (IndependentVerdictCounts, error) {
	terminal := ""
	if terminalAt != nil {
		terminal = fmtTime(*terminalAt)
	}
	var counts IndependentVerdictCounts
	err := tx.QueryRow(`
SELECT COUNT(*),
       COALESCE(SUM(CASE WHEN v.revoked_at IS NULL THEN 1 ELSE 0 END),0),
	   COUNT(DISTINCT CASE WHEN v.revoked_at IS NULL THEN r.verifier_id END),
       COALESCE(SUM(CASE WHEN v.revoked_at IS NULL
         AND r.observed_digest<>'' AND r.observed_digest<>? THEN 1 ELSE 0 END),0),
       COALESCE(SUM(CASE WHEN v.revoked_at IS NULL AND ?<>''
         AND r.rule_id=? AND r.passed=1 AND r.observed_version<>''
         AND r.observed_version<>? THEN 1 ELSE 0 END),0),
       COALESCE(SUM(CASE WHEN v.revoked_at IS NULL
         AND (?='' OR r.received_at>?) THEN 1 ELSE 0 END),0),
	   COALESCE(SUM(CASE WHEN v.revoked_at IS NULL AND r.passed=0 THEN 1 ELSE 0 END),0),
       COALESCE(SUM(CASE WHEN v.revoked_at IS NULL AND ?<>''
         AND r.rule_id=? AND r.passed=1 AND r.observed_version=''
         AND (?='' OR r.received_at>?) THEN 1 ELSE 0 END),0)
  FROM verification_results AS r
  JOIN verifiers AS v ON v.verifier_id=r.verifier_id
 WHERE r.job_id=? AND r.evidence_role=?`,
		artifactDigest, expectedVersion, model.IndependentRuleOpenClawCurrentRelease,
		expectedVersion, terminal, terminal, expectedVersion,
		model.IndependentRuleOpenClawCurrentRelease, terminal, terminal,
		jobID, JobVerificationRoleIndependent).
		Scan(&counts.Rows, &counts.Live, &counts.LiveProducers, &counts.LiveDigestClashes,
			&counts.LiveReleaseMismatches, &counts.LiveFresh, &counts.LiveFailed,
			&counts.LiveFreshReleaseUnreported)
	if err != nil {
		return IndependentVerdictCounts{}, fmt.Errorf("store: aggregate independent verdict: %w", err)
	}
	return counts, nil
}

func jobIndependentExpectedVersion(desired DesiredState) string {
	if desired.ResourceKind != "openclaw" || desired.ResourceID != "openclaw" {
		return ""
	}
	var spec model.OpenClawSpec
	if json.Unmarshal([]byte(desired.Spec), &spec) != nil || spec.Kind != "openclaw" ||
		!validObservedVersion(spec.Version) || spec.Version == "" {
		return ""
	}
	return spec.Version
}
