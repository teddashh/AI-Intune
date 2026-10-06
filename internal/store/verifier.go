package store

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/teddashh/AI-Intune/internal/model"
)

const (
	VerifierKindFleetPeerAgent    = "fleet_peer_agent"
	VerifierKindHubProber         = "hub_prober"
	VerifierKindExternalJobRunner = "external_job_runner"

	maxVerifierDisplayNameBytes   = 256
	maxVerifierFailureDomainBytes = 256
)

func VerifierKindGrantsDeploymentGate(kind string) bool {
	return kind == VerifierKindFleetPeerAgent
}

var (
	ErrInvalidVerifier  = errors.New("store: verifier metadata is invalid")
	ErrVerifierNotFound = errors.New("store: verifier not found")
	// ErrVerifierNameTaken covers revoked rows too: a revoked verifier keeps its
	// row so the evidence it produced stays attributable, so its name stays
	// taken. Reusing it would make an evidence listing ambiguous to a reader.
	ErrVerifierNameTaken        = errors.New("store: a verifier already uses this display name")
	ErrVerifierNotEligible      = errors.New("store: verifier is not eligible for this job")
	ErrVerifierRevoked          = errors.New("store: verifier is revoked")
	ErrVerifierRevisionConflict = fmt.Errorf("%w: verifier revision does not match", ErrPreconditionFailed)

	// ErrVerifierUnauthorized wraps ErrUnauthorized so callers that only need
	// "unauthenticated" keep working, while anything that must never confuse the
	// two producer planes can tell a rejected verifier credential from a
	// rejected agent one. Handlers still return one opaque message to clients.
	ErrVerifierUnauthorized = fmt.Errorf("%w: not a registered, active verifier credential", ErrUnauthorized)
)

// Verifier is the public registry projection. It deliberately has no
// credential field: plaintext exists only in RegisterVerifier's fresh return,
// and credential_hash is never exposed through a Store read path.
type Verifier struct {
	VerifierID    string     `json:"verifier_id"`
	Kind          string     `json:"kind"`
	DisplayName   string     `json:"display_name"`
	FailureDomain string     `json:"failure_domain"`
	CreatedAt     time.Time  `json:"created_at"`
	RevokedAt     *time.Time `json:"revoked_at,omitempty"`
	LastSeenAt    *time.Time `json:"last_seen_at,omitempty"`
	Revision      int64      `json:"revision"`
}

const verifierColumns = `verifier_id,kind,display_name,failure_domain,created_at,
       revoked_at,last_seen_at,revision`

// RegisterVerifier creates a verifier identity and returns its randomly
// minted bearer once. There is intentionally no credential argument or
// alternate insertion path in this API.
//
// hubMachineID is the machine_id the Hub's own host resolves to in this
// registry, or "" when the Hub host is not enrolled. It exists so that a
// hub_prober's failure domain is expressed in the same namespace as
// jobs.machine_id: that is what lets the write-time rule stay a single
// comparison (verifiers.failure_domain <> jobs.machine_id) instead of a second
// special case that can drift. Callers must not pass a value they did not
// resolve against this registry.
func (s *Store) RegisterVerifier(kind, displayName, failureDomain, hubMachineID string) (Verifier, string, error) {
	if !validVerifierKind(kind) ||
		!validVerifierText(displayName, maxVerifierDisplayNameBytes) ||
		!validVerifierText(failureDomain, maxVerifierFailureDomainBytes) {
		return Verifier{}, "", ErrInvalidVerifier
	}

	tx, err := s.beginWrite(context.Background(), "register_verifier")
	if err != nil {
		return Verifier{}, "", fmt.Errorf("store: begin verifier registration: %w", err)
	}
	defer tx.Rollback()

	if err := validateVerifierFailureDomain(tx, kind, failureDomain, hubMachineID); err != nil {
		return Verifier{}, "", err
	}
	secret, err := newToken()
	if err != nil {
		return Verifier{}, "", err
	}
	createdAt := s.now().UTC().Truncate(time.Second)
	if !validJobEvidenceTime(createdAt) {
		return Verifier{}, "", ErrInvalidVerifier
	}
	verifier := Verifier{
		VerifierID: newID(), Kind: kind, DisplayName: displayName,
		FailureDomain: failureDomain, CreatedAt: createdAt, Revision: 1,
	}
	if _, err := tx.Exec(`
INSERT INTO verifiers
  (verifier_id,kind,display_name,failure_domain,credential_hash,created_at,revision)
VALUES (?,?,?,?,?,?,?)`, verifier.VerifierID, verifier.Kind, verifier.DisplayName,
		verifier.FailureDomain, hashToken(secret), fmtTime(verifier.CreatedAt), verifier.Revision); err != nil {
		if verifierDisplayNameTaken(tx, displayName) {
			return Verifier{}, "", ErrVerifierNameTaken
		}
		return Verifier{}, "", fmt.Errorf("store: register verifier: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Verifier{}, "", fmt.Errorf("store: commit verifier registration: %w", err)
	}
	return verifier, secret, nil
}

func verifierDisplayNameTaken(q verifierQueryRower, displayName string) bool {
	var present int
	return q.QueryRow(`SELECT 1 FROM verifiers WHERE display_name=?`, displayName).Scan(&present) == nil
}

type verifierQueryRower interface {
	QueryRow(query string, args ...any) *sql.Row
}

// VerifierUnenrolledHubDomain is the only failure domain a hub_prober may
// declare when the Hub's own host is absent from the registry. It is not a
// machine_id and can never equal one, so a prober carrying it is separated from
// every managed machine by construction.
const VerifierUnenrolledHubDomain = "hub"

func validateVerifierFailureDomain(q verifierQueryRower, kind, failureDomain, hubMachineID string) error {
	if kind == VerifierKindExternalJobRunner {
		return nil
	}
	if kind == VerifierKindHubProber {
		// ⚠ A hub_prober that declares the literal "hub" while the Hub host IS
		// enrolled would pass the write-time comparison against its own
		// co-located endpoint (HANDOFF-2026-09-11 §14.19) and verify the very
		// machine it runs on. Accepting the literal is therefore conditional on
		// the caller having found no registry row for the Hub host.
		if hubMachineID == "" {
			if failureDomain != VerifierUnenrolledHubDomain {
				return ErrInvalidVerifier
			}
			return nil
		}
		if failureDomain != hubMachineID {
			return ErrInvalidVerifier
		}
	}
	var present int
	err := q.QueryRow(`SELECT 1 FROM machine_registry
 WHERE machine_id=? AND retired_at IS NULL`, failureDomain).Scan(&present)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrInvalidVerifier
	}
	if err != nil {
		return fmt.Errorf("store: validate verifier failure domain: %w", err)
	}
	return nil
}

func validVerifierKind(kind string) bool {
	switch kind {
	case VerifierKindFleetPeerAgent, VerifierKindHubProber, VerifierKindExternalJobRunner:
		return true
	default:
		return false
	}
}

// validVerifierText intentionally mirrors operator.validateMachineReadText's
// character policy without importing the operator package back into store.
func validVerifierText(value string, maxBytes int) bool {
	if value == "" || value != strings.TrimSpace(value) || len(value) > maxBytes || !utf8.ValidString(value) {
		return false
	}
	for _, char := range value {
		if unicode.IsControl(char) || unicode.Is(unicode.Cf, char) {
			return false
		}
	}
	return true
}

func (s *Store) RevokeVerifier(verifierID string, expectedRevision int64, at time.Time) error {
	if !validJobReadStoredIdentifier(verifierID, 256) || expectedRevision < 1 || !validJobEvidenceTime(at) {
		return ErrInvalidVerifier
	}
	res, err := s.execWrite(context.Background(), "revoke_verifier", `
UPDATE verifiers
   SET revoked_at=?,revision=revision+1
 WHERE verifier_id=? AND revision=? AND revoked_at IS NULL`,
		fmtTime(at), verifierID, expectedRevision)
	if err != nil {
		return fmt.Errorf("store: revoke verifier: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: count revoked verifiers: %w", err)
	}
	if n == 1 {
		return nil
	}
	var revision int64
	var revokedAt sql.NullString
	err = s.rdb.QueryRow(`SELECT revision,revoked_at FROM verifiers WHERE verifier_id=?`, verifierID).
		Scan(&revision, &revokedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrVerifierNotFound
	}
	if err != nil {
		return fmt.Errorf("store: inspect verifier revocation: %w", err)
	}
	if revokedAt.Valid && revokedAt.String != "" {
		return ErrVerifierRevoked
	}
	return ErrVerifierRevisionConflict
}

func (s *Store) ListVerifiers() ([]Verifier, error) {
	rows, err := s.rdb.Query(`SELECT ` + verifierColumns + ` FROM verifiers
 ORDER BY display_name,verifier_id`)
	if err != nil {
		return nil, fmt.Errorf("store: list verifiers: %w", err)
	}
	defer rows.Close()
	var verifiers []Verifier
	for rows.Next() {
		verifier, err := scanVerifier(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan verifier: %w", err)
		}
		if !validVerifierRow(verifier) {
			return nil, fmt.Errorf("store: verifier %q row is invalid", verifier.VerifierID)
		}
		verifiers = append(verifiers, verifier)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: finish verifier list: %w", err)
	}
	return verifiers, nil
}

func (s *Store) GetVerifier(verifierID string) (Verifier, error) {
	verifier, err := scanVerifier(s.rdb.QueryRow(`SELECT `+verifierColumns+`
 FROM verifiers WHERE verifier_id=?`, verifierID))
	if errors.Is(err, sql.ErrNoRows) {
		return Verifier{}, ErrVerifierNotFound
	}
	if err != nil {
		return Verifier{}, fmt.Errorf("store: get verifier: %w", err)
	}
	if !validVerifierRow(verifier) {
		return Verifier{}, fmt.Errorf("store: verifier %q row is invalid", verifierID)
	}
	return verifier, nil
}

type verifierScanner interface {
	Scan(dest ...any) error
}

func scanVerifier(scanner verifierScanner) (Verifier, error) {
	var verifier Verifier
	var createdAt string
	var revokedAt, lastSeenAt sql.NullString
	if err := scanner.Scan(&verifier.VerifierID, &verifier.Kind, &verifier.DisplayName,
		&verifier.FailureDomain, &createdAt, &revokedAt, &lastSeenAt, &verifier.Revision); err != nil {
		return Verifier{}, err
	}
	verifier.CreatedAt = parseTime(createdAt)
	verifier.RevokedAt = parseTimePtr(revokedAt)
	verifier.LastSeenAt = parseTimePtr(lastSeenAt)
	return verifier, nil
}

// AuthenticateVerifier mirrors AuthenticateAgent: every active candidate hash
// is compared in constant time and a match never exits the scan early.
func (s *Store) AuthenticateVerifier(bearer string) (Verifier, error) {
	want := []byte(hashToken(bearer))
	rows, err := s.rdb.Query(`SELECT ` + verifierColumns + `,credential_hash FROM verifiers
 WHERE credential_hash<>'' AND revoked_at IS NULL`)
	if err != nil {
		return Verifier{}, fmt.Errorf("store: verifier auth: %w", err)
	}
	defer rows.Close()

	var found Verifier
	matched := false
	for rows.Next() {
		var verifier Verifier
		var createdAt, credentialHash string
		var revokedAt, lastSeenAt sql.NullString
		if err := rows.Scan(&verifier.VerifierID, &verifier.Kind, &verifier.DisplayName,
			&verifier.FailureDomain, &createdAt, &revokedAt, &lastSeenAt,
			&verifier.Revision, &credentialHash); err != nil {
			return Verifier{}, fmt.Errorf("store: verifier auth scan: %w", err)
		}
		verifier.CreatedAt = parseTime(createdAt)
		verifier.RevokedAt = parseTimePtr(revokedAt)
		verifier.LastSeenAt = parseTimePtr(lastSeenAt)
		if subtle.ConstantTimeCompare([]byte(credentialHash), want) == 1 {
			found = verifier
			matched = true
		}
		// Do not break: match position must not affect the scan length.
	}
	if err := rows.Err(); err != nil {
		return Verifier{}, fmt.Errorf("store: verifier auth rows: %w", err)
	}
	if !matched {
		return Verifier{}, ErrVerifierUnauthorized
	}
	return found, nil
}

type IndependentVerificationRequest struct {
	VerifierID      string
	JobID           string
	RuleID          string
	Command         string
	ExitCode        int
	StdoutExcerpt   string
	StderrExcerpt   string
	ObservedDigest  string
	ObservedVersion string
	Passed          bool
	VerifiedAt      time.Time
}

func (s *Store) RecordIndependentVerification(req IndependentVerificationRequest) error {
	receivedAt := s.now().UTC().Truncate(time.Second)
	if !validJobReadStoredIdentifier(req.RuleID, 256) ||
		len(req.Command) > maxJobVerificationCommandBytes ||
		len(req.StdoutExcerpt) > maxJobVerificationExcerptBytes ||
		len(req.StderrExcerpt) > maxJobVerificationExcerptBytes ||
		!validObservedDigest(req.ObservedDigest) ||
		!validIndependentObservedVersion(req.RuleID, req.Passed, req.ObservedVersion, true) ||
		!validJobEvidenceTime(req.VerifiedAt) || !validJobEvidenceTime(receivedAt) {
		return ErrInvalidJobEvidence
	}

	tx, err := s.beginWrite(context.Background(), "record_independent_verification")
	if err != nil {
		return fmt.Errorf("store: begin independent verification: %w", err)
	}
	defer tx.Rollback()

	// Eligibility is intentionally part of the INSERT source. There is no job
	// lease in this producer plane; revocation and failure-domain separation are
	// checked atomically with the evidence write.
	res, err := tx.Exec(`
INSERT INTO verification_results
  (verification_id,job_id,machine_id,rule_id,command,exit_code,
   stdout_excerpt,stderr_excerpt,passed,verified_at,producer_kind,
   producer_id,evidence_role,authority,provenance_recorded,received_at,
   observed_digest,observed_version,verifier_id)
SELECT ?,j.job_id,j.machine_id,?,?,?,?,?,?,?,v.kind,v.verifier_id,?,?,1,?,?,?,v.verifier_id
  FROM jobs AS j
  JOIN verifiers AS v ON v.verifier_id=?
 WHERE j.job_id=?
   AND v.revoked_at IS NULL
   AND v.failure_domain<>j.machine_id`, newID(), req.RuleID, req.Command, req.ExitCode,
		req.StdoutExcerpt, req.StderrExcerpt, req.Passed, fmtTime(req.VerifiedAt),
		JobVerificationRoleIndependent, JobVerificationAuthorityVerifierBearer,
		fmtTime(receivedAt), req.ObservedDigest, req.ObservedVersion, req.VerifierID, req.JobID)
	if err != nil {
		return fmt.Errorf("store: record independent verification: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: count independent verification results: %w", err)
	}
	if n != 1 {
		var jobPresent int
		err := tx.QueryRow(`SELECT 1 FROM jobs WHERE job_id=?`, req.JobID).Scan(&jobPresent)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrJobNotFound
		}
		if err != nil {
			return fmt.Errorf("store: inspect independent verification job: %w", err)
		}
		return ErrVerifierNotEligible
	}
	if _, err := tx.Exec(`UPDATE verifiers SET last_seen_at=?
 WHERE verifier_id=? AND revoked_at IS NULL`, fmtTime(receivedAt), req.VerifierID); err != nil {
		return fmt.Errorf("store: update verifier last seen: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit independent verification: %w", err)
	}
	return nil
}

func validObservedDigest(value string) bool {
	if value == "" {
		return true
	}
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

func validObservedVersion(value string) bool {
	return value == "" || (utf8.ValidString(value) && validJobReadStoredIdentifier(value, 128) &&
		!strings.Contains(value, "/") && value != "." && value != "..")
}

// validIndependentObservedVersion binds structured version evidence to the
// only rule that measures it. A passed current-release row written by the new
// contract must name the release; readers may still admit an empty value on a
// historical row so it can be shown as release_unreported instead of corrupt.
func validIndependentObservedVersion(ruleID string, passed bool, value string, requirePassedCurrent bool) bool {
	if !validObservedVersion(value) {
		return false
	}
	if ruleID != model.IndependentRuleOpenClawCurrentRelease {
		return value == ""
	}
	if !passed {
		return value == ""
	}
	return value != "" || !requirePassedCurrent
}

type IndependentVerdict string

const (
	IndependentAbsent            IndependentVerdict = "absent"
	IndependentStale             IndependentVerdict = "stale"
	IndependentDigestMismatch    IndependentVerdict = "digest_mismatch"
	IndependentReleaseMismatch   IndependentVerdict = "release_mismatch"
	IndependentReleaseUnreported IndependentVerdict = "release_unreported"
	IndependentProducerRevoked   IndependentVerdict = "producer_revoked"
	IndependentPassed            IndependentVerdict = "passed"
	IndependentFailed            IndependentVerdict = "failed"
)

// IndependentVerdictInput contains only the facts needed by the pure verdict
// function. ReceivedAt is the Hub clock; producer time is deliberately absent
// because it must never decide freshness.
type IndependentVerdictInput struct {
	ObservedDigest  string
	ObservedVersion string
	RuleID          string
	ReceivedAt      time.Time
	Passed          bool
	ProducerRevoked bool
}

// EvaluateIndependentVerdict applies the independent-evidence precedence in
// contract order and has no Store dependency.
// IndependentVerdictCounts is the reduced form of one job's independent rows.
// It exists because the verdict must cover every row while the rendered page
// stays bounded: the counts come from an unbounded SQL aggregate, the page
// does not, and both reach the same precedence through EvaluateIndependentVerdictCounts.
type IndependentVerdictCounts struct {
	Rows int
	// Live counts rows whose producer is not revoked. Everything below is a
	// subset of Live: a revoked producer's row decides nothing.
	Live int
	// LiveProducers counts distinct non-revoked verifier identities. One
	// verifier normally writes several rules per report; those rows add weight
	// to Rows and Live, but they do not become several producers.
	LiveProducers              int
	LiveDigestClashes          int
	LiveReleaseMismatches      int
	LiveFresh                  int
	LiveFailed                 int
	LiveFreshReleaseUnreported int
}

// EvaluateIndependentVerdictCounts holds the whole precedence. Nothing else may
// decide an independent verdict.
func EvaluateIndependentVerdictCounts(counts IndependentVerdictCounts) IndependentVerdict {
	switch {
	case counts.Rows == 0:
		return IndependentAbsent
	case counts.Live == 0:
		return IndependentProducerRevoked
	case counts.LiveDigestClashes > 0:
		return IndependentDigestMismatch
	case counts.LiveReleaseMismatches > 0:
		return IndependentReleaseMismatch
	case counts.LiveFresh == 0:
		// Every live row reached the Hub at or before the job finished, so none
		// of them can be describing the end state. A job with no terminal_at is
		// never stale: LiveFresh counts every live row in that case.
		return IndependentStale
	case counts.LiveFailed > 0:
		return IndependentFailed
	case counts.LiveFreshReleaseUnreported > 0:
		return IndependentReleaseUnreported
	default:
		return IndependentPassed
	}
}

// EvaluateIndependentVerdict reduces explicit rows and delegates. It is the
// entry point for tests and for any caller that already holds every row.
func EvaluateIndependentVerdict(artifactDigest, expectedVersion string, terminalAt *time.Time,
	rows []IndependentVerdictInput,
) IndependentVerdict {
	counts := IndependentVerdictCounts{Rows: len(rows)}
	for _, row := range rows {
		if row.ProducerRevoked {
			continue
		}
		counts.Live++
		if row.ObservedDigest != "" && row.ObservedDigest != artifactDigest {
			counts.LiveDigestClashes++
		}
		if expectedVersion != "" && row.RuleID == model.IndependentRuleOpenClawCurrentRelease &&
			row.Passed && row.ObservedVersion != "" && row.ObservedVersion != expectedVersion {
			counts.LiveReleaseMismatches++
		}
		fresh := terminalAt == nil || row.ReceivedAt.After(*terminalAt)
		if fresh {
			counts.LiveFresh++
		}
		if !row.Passed {
			counts.LiveFailed++
		}
		if fresh && expectedVersion != "" &&
			row.RuleID == model.IndependentRuleOpenClawCurrentRelease && row.Passed &&
			row.ObservedVersion == "" {
			counts.LiveFreshReleaseUnreported++
		}
	}
	return EvaluateIndependentVerdictCounts(counts)
}

// VerifierStateActive and VerifierStateRevoked are the two states a registry
// row can be in. A revoked row stays listed: the evidence it produced is still
// attributable and a reader must be able to see who produced it.
const (
	VerifierStateActive  = "active"
	VerifierStateRevoked = "revoked"
)

func (v Verifier) State() string {
	if v.RevokedAt != nil {
		return VerifierStateRevoked
	}
	return VerifierStateActive
}

// validVerifierRow fails closed on a row that could not have been written by
// RegisterVerifier or ApplyOperatorVerifier. A reader must never render text
// that did not pass the registration policy.
func validVerifierRow(v Verifier) bool {
	return validJobReadStoredIdentifier(v.VerifierID, 256) && validVerifierKind(v.Kind) &&
		validVerifierText(v.DisplayName, maxVerifierDisplayNameBytes) &&
		validVerifierText(v.FailureDomain, maxVerifierFailureDomainBytes) &&
		validJobEvidenceTime(v.CreatedAt) && v.Revision >= 1 &&
		(v.RevokedAt == nil || validJobEvidenceTime(*v.RevokedAt)) &&
		(v.LastSeenAt == nil || validJobEvidenceTime(*v.LastSeenAt))
}

// VerifierEvidenceCounts returns how many independent rows each verifier has
// produced, keyed by verifier_id. Verifiers that produced none are absent.
func (s *Store) VerifierEvidenceCounts() (map[string]int64, error) {
	rows, err := s.rdb.Query(`SELECT verifier_id,COUNT(*) FROM verification_results
 WHERE evidence_role=? AND verifier_id<>'' GROUP BY verifier_id`, JobVerificationRoleIndependent)
	if err != nil {
		return nil, fmt.Errorf("store: count verifier evidence: %w", err)
	}
	defer rows.Close()
	counts := make(map[string]int64)
	for rows.Next() {
		var verifierID string
		var count int64
		if err := rows.Scan(&verifierID, &count); err != nil {
			return nil, fmt.Errorf("store: scan verifier evidence count: %w", err)
		}
		counts[verifierID] = count
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: finish verifier evidence counts: %w", err)
	}
	return counts, nil
}

// VerifierJobCount is how many distinct jobs one verifier has written
// independent evidence for.
func (s *Store) VerifierJobCount(verifierID string) (int64, error) {
	var jobs int64
	if err := s.rdb.QueryRow(`SELECT COUNT(DISTINCT job_id) FROM verification_results
 WHERE evidence_role=? AND verifier_id=?`, JobVerificationRoleIndependent, verifierID).
		Scan(&jobs); err != nil {
		return 0, fmt.Errorf("store: count verifier jobs: %w", err)
	}
	return jobs, nil
}

// DeploymentIndependentVerdict is one target job's independent verdict inside a
// deployment. It carries the live producer count as well, because "passed" from
// one producer and "passed" from three are different amounts of evidence and
// the reader is entitled to tell them apart.
type DeploymentIndependentVerdict struct {
	JobID         string
	Verdict       IndependentVerdict
	Rows          int
	LiveProducers int
}

// DeploymentIndependentVerdicts returns a verdict per job of a deployment that
// has at least one independent row, keyed by job_id. Jobs with no independent
// row are absent from the map rather than mapped to IndependentAbsent: only the
// caller knows which targets have a job at all, and inventing a verdict for a
// target that was never opened would state something the ledger does not.
//
// The aggregate runs over every independent row of every job for the same
// reason readIndependentVerdictCounts does: a verdict that saw only part of the
// evidence could call a target passed while an unread row reported a clash.
func (s *Store) DeploymentIndependentVerdicts(deploymentID string) (map[string]DeploymentIndependentVerdict, error) {
	tx, err := s.beginWrite(context.Background(), "deployment_independent_verdicts")
	if err != nil {
		return nil, fmt.Errorf("store: begin deployment independent verdicts: %w", err)
	}
	defer tx.Rollback()
	rows, err := tx.Query(`
SELECT j.job_id,COALESCE(j.artifact_digest,''),j.terminal_at,
       d.resource_kind,d.resource_id,d.spec
  FROM deployment_targets AS t
  JOIN jobs AS j ON j.job_id=t.job_id
  JOIN desired_state AS d ON d.desired_id=j.desired_id
 WHERE t.deployment_id=?
   AND EXISTS (SELECT 1 FROM verification_results AS r
                WHERE r.job_id=j.job_id AND r.evidence_role=?)
 ORDER BY j.job_id`, deploymentID, JobVerificationRoleIndependent)
	if err != nil {
		return nil, fmt.Errorf("store: list deployment independent jobs: %w", err)
	}
	defer rows.Close()
	type independentJob struct {
		jobID, digest, resourceKind, resourceID, spec string
		terminalAt                                    *time.Time
	}
	jobs := make([]independentJob, 0)
	for rows.Next() {
		var job independentJob
		var terminalAt sql.NullString
		if err := rows.Scan(&job.jobID, &job.digest, &terminalAt, &job.resourceKind,
			&job.resourceID, &job.spec); err != nil {
			return nil, fmt.Errorf("store: scan deployment independent job: %w", err)
		}
		if terminalAt.Valid {
			parsed := parseTime(terminalAt.String)
			if parsed.IsZero() || terminalAt.String != fmtTime(parsed) {
				return nil, fmt.Errorf("%w: invalid deployment job terminal time", ErrJobReadCorrupt)
			}
			job.terminalAt = &parsed
		}
		jobs = append(jobs, job)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: finish deployment independent jobs: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("store: close deployment independent jobs: %w", err)
	}
	verdicts := make(map[string]DeploymentIndependentVerdict, len(jobs))
	for _, job := range jobs {
		expectedVersion := jobIndependentExpectedVersion(DesiredState{
			ResourceKind: job.resourceKind, ResourceID: job.resourceID, Spec: job.spec,
		})
		counts, err := readIndependentVerdictCounts(
			tx, job.jobID, job.digest, expectedVersion, job.terminalAt)
		if err != nil {
			return nil, err
		}
		verdicts[job.jobID] = DeploymentIndependentVerdict{
			JobID: job.jobID, Verdict: EvaluateIndependentVerdictCounts(counts),
			Rows: counts.Rows, LiveProducers: counts.LiveProducers,
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("store: commit deployment independent verdicts: %w", err)
	}
	return verdicts, nil
}
