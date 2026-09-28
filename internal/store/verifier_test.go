package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/deploy"
	"github.com/teddashh/AI-Intune/internal/model"
)

const verifierTestDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func mintAgentBearer(t *testing.T, s *Store, displayName string) (string, string) {
	t.Helper()
	enrollToken, err := s.CreateEnrollToken(displayName, time.Hour)
	if err != nil {
		t.Fatalf("create enrollment token: %v", err)
	}
	machineID, bearer, err := s.RedeemEnrollToken(enrollToken, model.EnrollRequest{
		SchemaVersion: model.SchemaVersion,
		EnrollToken:   enrollToken,
		Hostname:      displayName,
		OS:            "linux",
		Arch:          "amd64",
		UnixUser:      "tester",
	}, deployTestNow)
	if err != nil {
		t.Fatalf("redeem enrollment token: %v", err)
	}
	return machineID, bearer
}

func independentRequest(verifierID, jobID string) IndependentVerificationRequest {
	return IndependentVerificationRequest{
		VerifierID: verifierID, JobID: jobID, RuleID: "health",
		Command: "GET /health", ExitCode: 0, StdoutExcerpt: "ok",
		ObservedDigest: verifierTestDigest, Passed: true, VerifiedAt: deployTestNow,
	}
}

// newJobForDeployTestOnMachine scopes the desired state to the job's own
// machine, unlike newJobForDeployTest which always scopes to "machine-a".
func newJobForDeployTestOnMachine(t *testing.T, s *Store, machineID string) string {
	t.Helper()
	desiredID, rev, err := s.CreateDesiredState("machine", machineID, "app", "openclaw", `{}`, "測試者")
	if err != nil {
		t.Fatalf("create desired state for %q: %v", machineID, err)
	}
	jobID, err := s.CreateJob(machineID, desiredID, rev, NewJob{})
	if err != nil {
		t.Fatalf("create job for %q: %v", machineID, err)
	}
	return jobID
}

func setJobDigest(t *testing.T, s *Store, jobID, digest string) {
	t.Helper()
	if _, err := s.DB().Exec(`UPDATE jobs SET artifact_digest=? WHERE job_id=?`, digest, jobID); err != nil {
		t.Fatalf("set job digest: %v", err)
	}
}

func TestVerifierKindsAndFailureDomains(t *testing.T) {
	s := newDeployTestStore(t)
	registerDeployMachine(t, s, "active-peer")
	registerDeployMachine(t, s, "hub-host")
	registerDeployMachine(t, s, "retired-peer")
	if err := s.RetireMachine("retired-peer", deployTestNow); err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name, kind, domain, hubMachineID string
		wantErr                          bool
	}{
		{"fleet active machine", VerifierKindFleetPeerAgent, "active-peer", "", false},
		{"fleet missing machine", VerifierKindFleetPeerAgent, "missing-peer", "", true},
		{"fleet retired machine", VerifierKindFleetPeerAgent, "retired-peer", "", true},
		{"fleet ignores hub resolution", VerifierKindFleetPeerAgent, "active-peer", "hub-host", false},

		// ⚠ The literal "hub" is only honest when the Hub host is absent from the
		// registry. Accepting it while the host IS enrolled would let the prober
		// pass the write-time comparison against its own co-located endpoint.
		{"hub literal while unenrolled", VerifierKindHubProber, VerifierUnenrolledHubDomain, "", false},
		{"hub literal while enrolled", VerifierKindHubProber, VerifierUnenrolledHubDomain, "hub-host", true},
		{"hub resolved machine", VerifierKindHubProber, "hub-host", "hub-host", false},
		{"hub other machine while enrolled", VerifierKindHubProber, "active-peer", "hub-host", true},
		{"hub machine while unenrolled", VerifierKindHubProber, "hub-host", "", true},
		{"hub unresolved machine", VerifierKindHubProber, "missing-hub", "missing-hub", true},

		{"external free text", VerifierKindExternalJobRunner, "awx-west-1", "", false},
		{"unknown kind", "same_host_unit", "active-peer", "", true},
		{"empty domain", VerifierKindExternalJobRunner, "", "", true},
		{"surrounding whitespace", VerifierKindExternalJobRunner, " awx", "", true},
		{"format character", VerifierKindExternalJobRunner, "awx\u200b", "", true},
		{"oversized external", VerifierKindExternalJobRunner, strings.Repeat("x", 257), "", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, _, err := s.RegisterVerifier(test.kind,
				"verifier-"+strings.ReplaceAll(test.name, " ", "-"), test.domain, test.hubMachineID)
			if test.wantErr && !errors.Is(err, ErrInvalidVerifier) {
				t.Fatalf("error=%v want ErrInvalidVerifier", err)
			}
			if !test.wantErr && err != nil {
				t.Fatalf("register: %v", err)
			}
		})
	}
}

// A hub_prober registered against the enrolled Hub host must not be able to
// write evidence about that host, for the same reason and through the same
// comparison that stops a peer verifying itself.
func TestHubProberCannotVerifyItsOwnCoLocatedEndpoint(t *testing.T) {
	s := newDeployTestStore(t)
	registerDeployMachine(t, s, "machine-a")
	registerDeployMachine(t, s, "hub-host")
	prober, _, err := s.RegisterVerifier(VerifierKindHubProber, "hub-prober", "hub-host", "hub-host")
	if err != nil {
		t.Fatal(err)
	}
	hubJob := newJobForDeployTestOnMachine(t, s, "hub-host")
	if err := s.RecordIndependentVerification(independentRequest(prober.VerifierID, hubJob)); !errors.Is(err, ErrVerifierNotEligible) {
		t.Fatalf("hub prober wrote evidence about its own host: %v", err)
	}
	if got := countRows(t, s, `SELECT COUNT(*) FROM verification_results WHERE job_id=?`, hubJob); got != 0 {
		t.Fatalf("rejected hub prober still wrote %d rows", got)
	}
	otherJob := newJobForDeployTest(t, s, "machine-a")
	if err := s.RecordIndependentVerification(independentRequest(prober.VerifierID, otherJob)); err != nil {
		t.Fatalf("hub prober could not verify a different machine: %v", err)
	}
}

func TestVerifierCredentialPlanesAndSecretReadExclusion(t *testing.T) {
	s := newDeployTestStore(t)
	_, machineBearer := mintAgentBearer(t, s, "machine-auth")
	verifier, verifierBearer, err := s.RegisterVerifier(
		VerifierKindExternalJobRunner, "verifier-auth", "awx-auth", "")
	if err != nil {
		t.Fatal(err)
	}
	if machineBearer == verifierBearer {
		t.Fatal("independently minted machine and verifier bearers are identical")
	}
	if _, err := s.AuthenticateVerifier(machineBearer); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("machine bearer verifier auth error=%v want ErrUnauthorized", err)
	}
	if _, err := s.AuthenticateAgent(verifierBearer); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("verifier bearer machine auth error=%v want ErrUnauthorized", err)
	}
	authed, err := s.AuthenticateVerifier(verifierBearer)
	if err != nil || authed.VerifierID != verifier.VerifierID {
		t.Fatalf("verifier auth=%+v err=%v", authed, err)
	}
	detail, err := s.GetVerifier(verifier.VerifierID)
	if err != nil {
		t.Fatal(err)
	}
	list, err := s.ListVerifiers()
	if err != nil || len(list) != 1 {
		t.Fatalf("list=%+v err=%v", list, err)
	}
	for name, value := range map[string]any{"register": verifier, "get": detail, "list": list, "authenticate": authed} {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), verifierBearer) || strings.Contains(fmt.Sprintf("%+v", value), verifierBearer) {
			t.Fatalf("%s read path exposed minted verifier secret", name)
		}
	}
	var storedHash string
	if err := s.DB().QueryRow(`SELECT credential_hash FROM verifiers WHERE verifier_id=?`, verifier.VerifierID).
		Scan(&storedHash); err != nil {
		t.Fatal(err)
	}
	if storedHash == verifierBearer || storedHash != hashToken(verifierBearer) {
		t.Fatalf("credential persistence is not hash-only: %q", storedHash)
	}
}

func TestRevokeVerifierUsesRevisionCASAndPreservesRegistryRow(t *testing.T) {
	s := newDeployTestStore(t)
	verifier, _, err := s.RegisterVerifier(
		VerifierKindExternalJobRunner, "cas-verifier", "awx-cas", "")
	if err != nil {
		t.Fatal(err)
	}
	if verifier.Revision != 1 {
		t.Fatalf("initial revision=%d want 1", verifier.Revision)
	}
	if err := s.RevokeVerifier(verifier.VerifierID, verifier.Revision+1, deployTestNow); !errors.Is(err, ErrPreconditionFailed) || !errors.Is(err, ErrVerifierRevisionConflict) {
		t.Fatalf("stale revision error=%v want verifier precondition failure", err)
	}
	unchanged, err := s.GetVerifier(verifier.VerifierID)
	if err != nil || unchanged.RevokedAt != nil || unchanged.Revision != verifier.Revision {
		t.Fatalf("stale CAS mutated verifier: %+v err=%v", unchanged, err)
	}
	if err := s.RevokeVerifier(verifier.VerifierID, verifier.Revision, deployTestNow); err != nil {
		t.Fatal(err)
	}
	revoked, err := s.GetVerifier(verifier.VerifierID)
	if err != nil || revoked.RevokedAt == nil || !revoked.RevokedAt.Equal(deployTestNow) ||
		revoked.Revision != verifier.Revision+1 {
		t.Fatalf("revoked verifier=%+v err=%v", revoked, err)
	}
	list, err := s.ListVerifiers()
	if err != nil || len(list) != 1 || list[0].VerifierID != verifier.VerifierID || list[0].RevokedAt == nil {
		t.Fatalf("revoked registry row not preserved in list: %+v err=%v", list, err)
	}
}

func TestIndependentVerificationEligibilityIsEnforcedByInsert(t *testing.T) {
	for _, kind := range []string{
		VerifierKindFleetPeerAgent, VerifierKindHubProber, VerifierKindExternalJobRunner,
	} {
		t.Run(kind, func(t *testing.T) {
			s := newDeployTestStore(t)
			registerDeployMachine(t, s, "target-machine")
			jobID := newJobForDeployTest(t, s, "target-machine")
			setJobDigest(t, s, jobID, verifierTestDigest)
			verifier, _, err := s.RegisterVerifier(kind, "same-domain-"+kind, "target-machine", "target-machine")
			if err != nil {
				t.Fatal(err)
			}
			if err := s.RecordIndependentVerification(independentRequest(verifier.VerifierID, jobID)); !errors.Is(err, ErrVerifierNotEligible) {
				t.Fatalf("error=%v want ErrVerifierNotEligible", err)
			}
			if got := countRows(t, s, `SELECT COUNT(*) FROM verification_results WHERE job_id=?`, jobID); got != 0 {
				t.Fatalf("same-domain verifier wrote %d rows", got)
			}
			if got, err := s.GetVerifier(verifier.VerifierID); err != nil || got.LastSeenAt != nil {
				t.Fatalf("rejected write changed last_seen_at: verifier=%+v err=%v", got, err)
			}
		})
	}

	s := newDeployTestStore(t)
	verifier, _, err := s.RegisterVerifier(VerifierKindExternalJobRunner, "missing-job", "awx", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RecordIndependentVerification(independentRequest(verifier.VerifierID, "missing-job")); !errors.Is(err, ErrJobNotFound) || errors.Is(err, ErrVerifierNotEligible) {
		t.Fatalf("missing job error=%v want only ErrJobNotFound", err)
	}
	registerDeployMachine(t, s, "existing-target")
	jobID := newJobForDeployTest(t, s, "existing-target")
	if err := s.RecordIndependentVerification(independentRequest("missing-verifier", jobID)); !errors.Is(err, ErrVerifierNotEligible) || errors.Is(err, ErrJobNotFound) {
		t.Fatalf("missing verifier error=%v want only ErrVerifierNotEligible", err)
	}
}

func TestRevokedVerifierCannotWriteAndExistingEvidenceIsProducerRevoked(t *testing.T) {
	s := newDeployTestStore(t)
	registerDeployMachine(t, s, "machine-a")
	registerDeployMachine(t, s, "peer-machine")
	jobID := newJobForDeployTest(t, s, "machine-a")
	setJobDigest(t, s, jobID, verifierTestDigest)
	verifier, bearer, err := s.RegisterVerifier(
		VerifierKindFleetPeerAgent, "peer-verifier", "peer-machine", "")
	if err != nil {
		t.Fatal(err)
	}
	request := independentRequest(verifier.VerifierID, jobID)
	if err := s.RecordIndependentVerification(request); err != nil {
		t.Fatal(err)
	}
	afterWrite, err := s.GetVerifier(verifier.VerifierID)
	if err != nil || afterWrite.LastSeenAt == nil || !afterWrite.LastSeenAt.Equal(deployTestNow) {
		t.Fatalf("successful write did not update last_seen_at: %+v err=%v", afterWrite, err)
	}
	if err := s.RevokeVerifier(verifier.VerifierID, verifier.Revision, deployTestNow.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AuthenticateVerifier(bearer); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("revoked bearer auth error=%v want ErrUnauthorized", err)
	}
	if err := s.RecordIndependentVerification(request); !errors.Is(err, ErrVerifierNotEligible) {
		t.Fatalf("revoked write error=%v want ErrVerifierNotEligible", err)
	}
	if got := countRows(t, s, `SELECT COUNT(*) FROM verification_results WHERE job_id=?`, jobID); got != 1 {
		t.Fatalf("revoked verifier changed evidence row count to %d", got)
	}
	rows, err := s.JobVerifications(jobID)
	if err != nil || len(rows) != 1 {
		t.Fatalf("job verifications=%+v err=%v", rows, err)
	}
	row := rows[0]
	if row.MachineID != "machine-a" || row.ProducerKind != VerifierKindFleetPeerAgent ||
		row.ProducerID != verifier.VerifierID || row.EvidenceRole != JobVerificationRoleIndependent ||
		row.Authority != JobVerificationAuthorityVerifierBearer || !row.ProvenanceRecorded ||
		row.VerifierID != verifier.VerifierID || row.ObservedDigest != verifierTestDigest ||
		!row.ReceivedAt.Equal(deployTestNow) {
		t.Fatalf("independent provenance mismatch: %+v", row)
	}
	evidence, err := s.JobReadEvidence(jobID, 10)
	if err != nil {
		t.Fatalf("store evidence read rejected independent row: %v", err)
	}
	// The executor page must stay empty: an independent producer never occupies
	// the page the deployment gate's evidence is read from.
	if len(evidence.Verifications) != 0 {
		t.Fatalf("independent row leaked into the executor page: %+v", evidence.Verifications)
	}
	if len(evidence.Independent) != 1 || evidence.Independent[0].VerifierID != verifier.VerifierID {
		t.Fatalf("independent page=%+v", evidence.Independent)
	}
	if evidence.Verifiers[verifier.VerifierID].State() != VerifierStateRevoked {
		t.Fatalf("revoked producer identity missing from evidence: %+v", evidence.Verifiers)
	}
	// The store's own verdict covers every row, not just this page.
	if evidence.IndependentVerdict != IndependentProducerRevoked ||
		evidence.IndependentLiveProducers != 0 {
		t.Fatalf("store verdict=%q live=%d", evidence.IndependentVerdict, evidence.IndependentLiveProducers)
	}
	verdict := EvaluateIndependentVerdict(verifierTestDigest, "", nil, []IndependentVerdictInput{{
		ObservedDigest: row.ObservedDigest, ReceivedAt: row.ReceivedAt,
		Passed: row.Passed, ProducerRevoked: true,
	}})
	if verdict != IndependentProducerRevoked || verdict == IndependentAbsent || verdict == IndependentPassed {
		t.Fatalf("revoked producer verdict=%q", verdict)
	}
}

func TestIndependentObservedDigestValidation(t *testing.T) {
	s := newDeployTestStore(t)
	registerDeployMachine(t, s, "digest-target")
	jobID := newJobForDeployTest(t, s, "digest-target")
	setJobDigest(t, s, jobID, verifierTestDigest)
	verifier, _, err := s.RegisterVerifier(VerifierKindExternalJobRunner, "digest-verifier", "awx", "")
	if err != nil {
		t.Fatal(err)
	}

	valid := []string{"", verifierTestDigest}
	for _, digest := range valid {
		request := independentRequest(verifier.VerifierID, jobID)
		request.RuleID = "valid-" + fmt.Sprint(len(digest))
		request.ObservedDigest = digest
		if err := s.RecordIndependentVerification(request); err != nil {
			t.Fatalf("valid digest %q rejected: %v", digest, err)
		}
	}
	invalid := []string{
		"sha256:", "sha256:abc", "SHA256:" + strings.Repeat("a", 64),
		"sha256:" + strings.Repeat("A", 64), "sha256:" + strings.Repeat("g", 64),
		verifierTestDigest + " ", strings.Repeat("a", 64),
	}
	for _, digest := range invalid {
		request := independentRequest(verifier.VerifierID, jobID)
		request.ObservedDigest = digest
		if err := s.RecordIndependentVerification(request); !errors.Is(err, ErrInvalidJobEvidence) {
			t.Fatalf("invalid digest %q error=%v want ErrInvalidJobEvidence", digest, err)
		}
	}
	if got := countRows(t, s, `SELECT COUNT(*) FROM verification_results WHERE job_id=?`, jobID); got != len(valid) {
		t.Fatalf("digest validation stored %d rows want %d", got, len(valid))
	}
}

func TestIndependentObservedVersionIsBoundToPassedCurrentReleaseRule(t *testing.T) {
	s := newDeployTestStore(t)
	registerDeployMachine(t, s, "version-target")
	jobID := newJobForDeployTest(t, s, "version-target")
	verifier, _, err := s.RegisterVerifier(VerifierKindExternalJobRunner,
		"version-verifier", "version-peer", "")
	if err != nil {
		t.Fatal(err)
	}
	valid := independentRequest(verifier.VerifierID, jobID)
	valid.RuleID = model.IndependentRuleOpenClawCurrentRelease
	valid.ObservedVersion = "2026.9.2"
	if err := s.RecordIndependentVerification(valid); err != nil {
		t.Fatalf("valid observed version rejected: %v", err)
	}
	var storedVersion string
	if err := s.DB().QueryRow(`SELECT observed_version FROM verification_results WHERE job_id=?`, jobID).
		Scan(&storedVersion); err != nil {
		t.Fatal(err)
	}
	if storedVersion != valid.ObservedVersion {
		t.Fatalf("stored observed_version=%q want %q", storedVersion, valid.ObservedVersion)
	}
	for _, test := range []struct {
		name, ruleID, version string
		passed                bool
	}{
		{"missing from passed current release", model.IndependentRuleOpenClawCurrentRelease, "", true},
		{"version on another rule", model.IndependentRuleOpenClawGatewayHTTP, "2026.9.2", true},
		{"version on failed current release", model.IndependentRuleOpenClawCurrentRelease, "2026.9.2", false},
		{"slash", model.IndependentRuleOpenClawCurrentRelease, "releases/2026.9.2", true},
		{"control", model.IndependentRuleOpenClawCurrentRelease, "2026.9.2\nnext", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := independentRequest(verifier.VerifierID, jobID)
			request.RuleID, request.ObservedVersion, request.Passed = test.ruleID, test.version, test.passed
			if err := s.RecordIndependentVerification(request); !errors.Is(err, ErrInvalidJobEvidence) {
				t.Fatalf("error=%v want ErrInvalidJobEvidence", err)
			}
		})
	}
}

func TestEvaluateIndependentVerdictAllDistinctWithExactPrecedence(t *testing.T) {
	terminalAt := deployTestNow
	after := deployTestNow.Add(time.Second)
	before := deployTestNow.Add(-time.Second)
	match := verifierTestDigest
	mismatch := "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	tests := []struct {
		name     string
		expected string
		terminal *time.Time
		rows     []IndependentVerdictInput
		want     IndependentVerdict
	}{
		{"absent", "", &terminalAt, nil, IndependentAbsent},
		{"producer revoked before mismatch", "", &terminalAt, []IndependentVerdictInput{{ObservedDigest: mismatch, ReceivedAt: after, Passed: true, ProducerRevoked: true}}, IndependentProducerRevoked},
		{"digest mismatch before stale and failed", "", &terminalAt, []IndependentVerdictInput{{ObservedDigest: mismatch, ReceivedAt: before, Passed: false}}, IndependentDigestMismatch},
		{"release mismatch before stale", "2026.9.2", &terminalAt, []IndependentVerdictInput{{RuleID: model.IndependentRuleOpenClawCurrentRelease, ObservedVersion: "2026.9.1", ReceivedAt: before, Passed: true}}, IndependentReleaseMismatch},
		{"stale before unreported", "2026.9.2", &terminalAt, []IndependentVerdictInput{{RuleID: model.IndependentRuleOpenClawCurrentRelease, ReceivedAt: terminalAt, Passed: true}}, IndependentStale},
		{"failed before unreported", "2026.9.2", &terminalAt, []IndependentVerdictInput{{RuleID: model.IndependentRuleOpenClawCurrentRelease, ReceivedAt: after, Passed: true}, {ReceivedAt: after, Passed: false}}, IndependentFailed},
		{"release unreported", "2026.9.2", &terminalAt, []IndependentVerdictInput{{RuleID: model.IndependentRuleOpenClawCurrentRelease, ReceivedAt: after, Passed: true}}, IndependentReleaseUnreported},
		{"passed", "2026.9.2", &terminalAt, []IndependentVerdictInput{{RuleID: model.IndependentRuleOpenClawCurrentRelease, ObservedVersion: "2026.9.2", ReceivedAt: after, Passed: true}}, IndependentPassed},
		{"unfinished is not stale", "", nil, []IndependentVerdictInput{{ObservedDigest: match, ReceivedAt: before, Passed: false}}, IndependentFailed},
	}
	seen := make(map[IndependentVerdict]bool)
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := EvaluateIndependentVerdict(match, test.expected, test.terminal, test.rows)
			if got != test.want {
				t.Fatalf("verdict=%q want %q", got, test.want)
			}
			seen[got] = true
		})
	}
	for _, verdict := range []IndependentVerdict{
		IndependentAbsent, IndependentProducerRevoked, IndependentDigestMismatch,
		IndependentReleaseMismatch, IndependentStale, IndependentFailed,
		IndependentReleaseUnreported, IndependentPassed,
	} {
		if !seen[verdict] {
			t.Fatalf("verdict %q was not reached distinctly", verdict)
		}
	}
	if IndependentDigestMismatch == IndependentFailed || IndependentDigestMismatch == IndependentPassed ||
		EvaluateIndependentVerdict(match, "", &terminalAt, nil) == IndependentPassed {
		t.Fatal("digest mismatch or absent collapsed into a pass/fail verdict")
	}
}

// A revoked verifier keeps its row so its evidence stays attributable, which
// means its display name stays taken. The caller must be told that in a form it
// can act on, not through a raw constraint error.
func TestRegisterVerifierReportsTakenDisplayNameIncludingRevoked(t *testing.T) {
	s := newDeployTestStore(t)
	first, _, err := s.RegisterVerifier(VerifierKindExternalJobRunner, "awx-verifier", "awx-1", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.RegisterVerifier(VerifierKindExternalJobRunner, "awx-verifier", "awx-2", ""); !errors.Is(err, ErrVerifierNameTaken) {
		t.Fatalf("duplicate live name error=%v want ErrVerifierNameTaken", err)
	}
	if err := s.RevokeVerifier(first.VerifierID, first.Revision, deployTestNow.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.RegisterVerifier(VerifierKindExternalJobRunner, "awx-verifier", "awx-2", ""); !errors.Is(err, ErrVerifierNameTaken) {
		t.Fatalf("duplicate revoked name error=%v want ErrVerifierNameTaken", err)
	}
	if got := countRows(t, s, `SELECT COUNT(*) FROM verifiers`); got != 1 {
		t.Fatalf("rejected duplicates left %d registry rows", got)
	}
}

// TestDeploymentIndependentVerdictsDecideEachTargetSeparately proves the
// deployment aggregate reaches the same precedence as the per-job read model
// and reaches it per target. One passing target next to one digest clash must
// not average into a single deployment-wide answer, and a target with no
// independent row must stay absent from the map rather than be given a verdict.
func TestDeploymentIndependentVerdictsDecideEachTargetSeparately(t *testing.T) {
	s := rolloutStore(t)
	addRolloutMachine(t, s, "cnode", "samplehub1", true)
	addRolloutMachine(t, s, "onode", "sampleagent2", true)
	addRolloutMachine(t, s, "peer", "peer1", true)

	d, jobs, err := s.CreateDeployment(NewDeployment{
		Channel: "canary", ResourceKind: "openclaw", ResourceID: "openclaw", Spec: rolloutTestOpenClawSpec,
		BatchSize: 3, CreatedBy: "independent-rollup-test",
		Targets: []NewDeploymentTarget{
			{MachineID: "cnode", BatchNo: 1}, {MachineID: "onode", BatchNo: 1}, {MachineID: "peer", BatchNo: 1},
		},
	})
	if err != nil || len(jobs) != 3 {
		t.Fatalf("create deployment: deployment=%+v jobs=%d err=%v", d, len(jobs), err)
	}
	byMachine := make(map[string]Job, len(jobs))
	for _, job := range jobs {
		byMachine[job.MachineID] = job
		setJobDigest(t, s, job.JobID, verifierTestDigest)
	}

	// The verifier's failure domain is peer1, so it may write for cnode and
	// onode and is structurally barred from its own machine's job. That third
	// target is the "no independent row" case, and it is a real one.
	verifier, _, err := s.RegisterVerifier(VerifierKindFleetPeerAgent, "peer-verifier", "peer", "")
	if err != nil {
		t.Fatalf("register peer verifier: %v", err)
	}
	if err := s.RecordIndependentVerification(independentRequest(verifier.VerifierID, byMachine["cnode"].JobID)); err != nil {
		t.Fatalf("record cnode evidence: %v", err)
	}
	cnodeUnit := independentRequest(verifier.VerifierID, byMachine["cnode"].JobID)
	cnodeUnit.RuleID = "unit_state"
	cnodeUnit.Command = "systemctl show"
	if err := s.RecordIndependentVerification(cnodeUnit); err != nil {
		t.Fatalf("record second cnode rule: %v", err)
	}
	clash := independentRequest(verifier.VerifierID, byMachine["onode"].JobID)
	clash.ObservedDigest = "sha256:" + strings.Repeat("b", 64)
	if err := s.RecordIndependentVerification(clash); err != nil {
		t.Fatalf("record onode evidence: %v", err)
	}
	if err := s.RecordIndependentVerification(independentRequest(verifier.VerifierID, byMachine["peer"].JobID)); !errors.Is(err, ErrVerifierNotEligible) {
		t.Fatalf("verifier wrote for its own failure domain: %v", err)
	}

	verdicts, err := s.DeploymentIndependentVerdicts(d.DeploymentID)
	if err != nil {
		t.Fatalf("deployment independent verdicts: %v", err)
	}
	if len(verdicts) != 2 {
		t.Fatalf("target without evidence was given a verdict: %+v", verdicts)
	}
	passed := verdicts[byMachine["cnode"].JobID]
	if passed.Verdict != IndependentPassed || passed.Rows != 2 || passed.LiveProducers != 1 {
		t.Fatalf("cnode verdict=%+v", passed)
	}
	mismatch := verdicts[byMachine["onode"].JobID]
	if mismatch.Verdict != IndependentDigestMismatch || mismatch.LiveProducers != 1 {
		t.Fatalf("onode verdict=%+v", mismatch)
	}
	if _, given := verdicts[byMachine["peer"].JobID]; given {
		t.Fatalf("co-located target appeared in the rollup: %+v", verdicts)
	}

	// Revoking the only producer must not erase the rows; it must change what
	// they are worth. Both targets fall to producer_revoked, including the one
	// whose row said passed.
	current, err := s.GetVerifier(verifier.VerifierID)
	if err != nil {
		t.Fatalf("read verifier: %v", err)
	}
	if err := s.RevokeVerifier(verifier.VerifierID, current.Revision, deployTestNow); err != nil {
		t.Fatalf("revoke verifier: %v", err)
	}
	revoked, err := s.DeploymentIndependentVerdicts(d.DeploymentID)
	if err != nil {
		t.Fatalf("deployment independent verdicts after revoke: %v", err)
	}
	if len(revoked) != 2 {
		t.Fatalf("revocation dropped evidence rows: %+v", revoked)
	}
	wantRows := map[string]int{byMachine["cnode"].JobID: 2, byMachine["onode"].JobID: 1}
	for jobID, rows := range wantRows {
		got := revoked[jobID]
		if got.Verdict != IndependentProducerRevoked || got.Rows != rows || got.LiveProducers != 0 {
			t.Fatalf("job %s after revoke=%+v", jobID, got)
		}
	}
}

// TestDeploymentIndependentVerdictsUseHubReceiptForStaleness pins the freshness
// clock to the Hub's received_at. A verifier that reports a time after the job
// finished has still only been heard from before it finished.
func TestDeploymentIndependentVerdictsUseHubReceiptForStaleness(t *testing.T) {
	s := rolloutStore(t)
	addRolloutMachine(t, s, "cnode", "samplehub1", true)
	addRolloutMachine(t, s, "peer", "peer1", true)
	d, jobs, err := s.CreateDeployment(NewDeployment{
		Channel: "canary", ResourceKind: "openclaw", ResourceID: "openclaw", Spec: rolloutTestOpenClawSpec,
		BatchSize: 2, CreatedBy: "independent-stale-test",
		Targets: []NewDeploymentTarget{{MachineID: "cnode", BatchNo: 1}, {MachineID: "peer", BatchNo: 1}},
	})
	if err != nil || len(jobs) != 2 {
		t.Fatalf("create deployment: %+v err=%v", jobs, err)
	}
	var cnodeJob Job
	for _, job := range jobs {
		if job.MachineID == "cnode" {
			cnodeJob = job
		}
	}
	setJobDigest(t, s, cnodeJob.JobID, verifierTestDigest)

	verifier, _, err := s.RegisterVerifier(VerifierKindFleetPeerAgent, "peer-verifier", "peer", "")
	if err != nil {
		t.Fatalf("register peer verifier: %v", err)
	}
	request := independentRequest(verifier.VerifierID, cnodeJob.JobID)
	request.VerifiedAt = deployTestNow.Add(72 * time.Hour)
	if err := s.RecordIndependentVerification(request); err != nil {
		t.Fatalf("record evidence: %v", err)
	}
	// The job finishes after the Hub took receipt, so the evidence describes
	// something earlier than the end state no matter what the producer claims.
	if _, err := s.DB().Exec(`UPDATE jobs SET state=?,terminal_at=? WHERE job_id=?`,
		deploy.Succeeded, fmtTime(time.Now().UTC().Add(time.Hour)), cnodeJob.JobID); err != nil {
		t.Fatalf("finish job: %v", err)
	}

	verdicts, err := s.DeploymentIndependentVerdicts(d.DeploymentID)
	if err != nil {
		t.Fatalf("deployment independent verdicts: %v", err)
	}
	if got := verdicts[cnodeJob.JobID]; got.Verdict != IndependentStale || got.LiveProducers != 1 {
		t.Fatalf("producer clock decided freshness: %+v", got)
	}
}
