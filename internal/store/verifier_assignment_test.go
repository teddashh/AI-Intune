package store

import (
	"errors"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/deploy"
	"github.com/teddashh/AI-Intune/internal/model"
)

func assignmentTestStore(t *testing.T) (*Store, *time.Time) {
	t.Helper()
	s := newTestStore(t)
	clock := deployTestNow
	s.nowFn = func() time.Time { return clock }
	return s, &clock
}

func finishAssignmentJob(t *testing.T, s *Store, jobID string, at time.Time) {
	t.Helper()
	if _, err := s.DB().Exec(`UPDATE jobs SET state=?,terminal_at=? WHERE job_id=?`,
		deploy.Succeeded, fmtTime(at), jobID); err != nil {
		t.Fatalf("finish job %q: %v", jobID, err)
	}
}

// assignJob runs the real preview/apply pair so the tests never construct a
// preview digest by hand. A test that mints its own digest would keep passing
// after preview and apply had drifted apart.
func assignJob(t *testing.T, s *Store, verifierID, jobID, key string) OperatorVerificationAssignmentResult {
	t.Helper()
	preview, err := s.PreviewOperatorVerificationAssignment(verifierID, jobID)
	if err != nil {
		t.Fatalf("preview assignment: %v", err)
	}
	result, err := s.ApplyOperatorVerificationAssignment(OperatorVerificationAssignmentRequest{
		VerifierID: verifierID, JobID: jobID, ConfirmVerifierName: preview.VerifierName,
		PreviewDigest: preview.PreviewDigest,
		Reason:        "測試派工", AssignedBy: "tailscale-user:1", IdempotencyKey: key,
		RequestDigest: "sha256:" + key,
	})
	if err != nil {
		t.Fatalf("apply assignment: %v", err)
	}
	// Audited is how this writer tells the operator service "the decision is
	// already in audit_log". A success that returns false does not lose a row —
	// it makes the service write a second, identical one, which is what the live
	// ledger carried for every assignment written before 2026-09-12.
	if !result.Audited {
		t.Fatal("成功的派工回報 Audited=false，service 會再寫一筆一模一樣的 audit")
	}
	return result
}

// TestPendingAssignmentsReturnOnlyWhatThisVerifierWasGiven is the reason this
// table exists. The verifier plane must never be able to enumerate work it was
// not handed, so a second verifier's assignment must be invisible even though
// the separation rule would make both of them eligible for both jobs.
func TestPendingAssignmentsReturnOnlyWhatThisVerifierWasGiven(t *testing.T) {
	s, _ := assignmentTestStore(t)
	for _, id := range []string{"cnode", "pnode", "onode"} {
		registerDeployMachine(t, s, id)
	}
	cnodeJob := newJobForDeployTestOnMachine(t, s, "cnode")
	pnodeJob := newJobForDeployTestOnMachine(t, s, "pnode")
	finishAssignmentJob(t, s, cnodeJob, deployTestNow)
	finishAssignmentJob(t, s, pnodeJob, deployTestNow)

	onode, _, err := s.RegisterVerifier(VerifierKindFleetPeerAgent, "onode-peer", "onode", "")
	if err != nil {
		t.Fatalf("register onode verifier: %v", err)
	}
	pnode, _, err := s.RegisterVerifier(VerifierKindFleetPeerAgent, "pnode-peer", "pnode", "")
	if err != nil {
		t.Fatalf("register pnode verifier: %v", err)
	}
	assignJob(t, s, onode.VerifierID, cnodeJob, "key-onode-cnode")

	pending, err := s.PendingVerificationAssignments(onode.VerifierID)
	if err != nil {
		t.Fatalf("pending for onode: %v", err)
	}
	if len(pending) != 1 || pending[0].JobID != cnodeJob || pending[0].MachineID != "cnode" ||
		pending[0].MachineName != "cnode" || !pending[0].Pending() {
		t.Fatalf("onode 應該只拿到 cnode 那一張單，得到 %+v", pending)
	}

	// pnode is eligible for cnodeJob under the separation rule and was not
	// given it. Eligibility must not be enough to see it.
	otherPending, err := s.PendingVerificationAssignments(pnode.VerifierID)
	if err != nil {
		t.Fatalf("pending for pnode: %v", err)
	}
	if len(otherPending) != 0 {
		t.Fatalf("沒有被指派的 verifier 不該看到任何工作單，得到 %+v", otherPending)
	}
}

// TestPendingAssignmentsWaitForTheJobToFinish keeps the hand-out from
// manufacturing stale evidence: anything reported before the executor finished
// describes an earlier state than the one being judged.
func TestPendingAssignmentsWaitForTheJobToFinish(t *testing.T) {
	s, _ := assignmentTestStore(t)
	registerDeployMachine(t, s, "cnode")
	registerDeployMachine(t, s, "onode")
	jobID := newJobForDeployTestOnMachine(t, s, "cnode")
	verifier, _, err := s.RegisterVerifier(VerifierKindFleetPeerAgent, "onode-peer", "onode", "")
	if err != nil {
		t.Fatal(err)
	}
	assignJob(t, s, verifier.VerifierID, jobID, "key-unfinished")

	pending, err := s.PendingVerificationAssignments(verifier.VerifierID)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("工作單還沒結束就不該派出去，得到 %+v", pending)
	}
	assignments, err := s.JobVerificationAssignments(jobID)
	if err != nil {
		t.Fatal(err)
	}
	if len(assignments) != 1 || assignments[0].JobTerminalAt != nil ||
		assignments[0].ReportedAt != nil || assignments[0].Pending() {
		t.Fatalf("工作單頁面應該看得到「已指派、等工作單結束」，得到 %+v", assignments)
	}

	finishAssignmentJob(t, s, jobID, deployTestNow)
	pending, err = s.PendingVerificationAssignments(verifier.VerifierID)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || !pending[0].Pending() {
		t.Fatalf("工作單結束之後應該派得出去，得到 %+v", pending)
	}
}

// TestEvidenceSatisfiesAnAssignmentAndOnlyEvidenceDoes pins the definition of
// done. Nothing but a recorded verification result may take an assignment off
// the hand-out.
func TestEvidenceSatisfiesAnAssignmentAndOnlyEvidenceDoes(t *testing.T) {
	s, clock := assignmentTestStore(t)
	registerDeployMachine(t, s, "cnode")
	registerDeployMachine(t, s, "onode")
	jobID := newJobForDeployTestOnMachine(t, s, "cnode")
	finishAssignmentJob(t, s, jobID, deployTestNow)
	verifier, _, err := s.RegisterVerifier(VerifierKindFleetPeerAgent, "onode-peer", "onode", "")
	if err != nil {
		t.Fatal(err)
	}
	assignJob(t, s, verifier.VerifierID, jobID, "key-satisfied")
	preview, err := s.PreviewOperatorVerificationAssignment(verifier.VerifierID, jobID)
	if err != nil || !preview.GrantsDeploymentGate {
		t.Fatalf("fleet-peer assignment preview=%+v err=%v", preview, err)
	}

	*clock = deployTestNow.Add(time.Minute)
	for i, ruleID := range []string{
		model.IndependentRuleOpenClawCurrentRelease,
		model.IndependentRuleOpenClawGatewayHTTP,
		model.IndependentRuleOpenClawUnitState,
	} {
		req := independentRequest(verifier.VerifierID, jobID)
		req.RuleID = ruleID
		if ruleID == model.IndependentRuleOpenClawCurrentRelease {
			req.ObservedVersion = "2026.9.2"
		}
		if err := s.RecordIndependentVerification(req); err != nil {
			t.Fatalf("record %s: %v", ruleID, err)
		}
		if i < 2 {
			pending, err := s.PendingVerificationAssignments(verifier.VerifierID)
			if err != nil || len(pending) != 1 {
				t.Fatalf("%d/3 部分回報不該讓派工消失：pending=%+v err=%v", i+1, pending, err)
			}
		}
	}
	pending, err := s.PendingVerificationAssignments(verifier.VerifierID)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("證據進來之後就不該再派同一張單，得到 %+v", pending)
	}
	assignments, err := s.JobVerificationAssignments(jobID)
	if err != nil {
		t.Fatal(err)
	}
	if len(assignments) != 1 || assignments[0].ReportedAt == nil ||
		!assignments[0].ReportedAt.Equal(deployTestNow.Add(time.Minute)) {
		t.Fatalf("工作單頁面應該看得到回報時間，得到 %+v", assignments)
	}

	// A second assignment made after that evidence is pending again: the old
	// row answers the old instruction, not the new one.
	*clock = deployTestNow.Add(2 * time.Minute)
	assignJob(t, s, verifier.VerifierID, jobID, "key-satisfied-again")
	pending, err = s.PendingVerificationAssignments(verifier.VerifierID)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("重新指派之後應該又是待回報，得到 %+v", pending)
	}
}

func TestNonGateVerifierAssignmentKeepsTheGenericOneRowCompletionRule(t *testing.T) {
	s, clock := assignmentTestStore(t)
	registerDeployMachine(t, s, "cnode")
	registerDeployMachine(t, s, "runner-domain")
	jobID := newJobForDeployTestOnMachine(t, s, "cnode")
	finishAssignmentJob(t, s, jobID, deployTestNow)
	verifier, _, err := s.RegisterVerifier(VerifierKindExternalJobRunner, "external runner", "runner-domain", "")
	if err != nil {
		t.Fatal(err)
	}
	preview, err := s.PreviewOperatorVerificationAssignment(verifier.VerifierID, jobID)
	if err != nil || preview.GrantsDeploymentGate {
		t.Fatalf("preview=%+v err=%v", preview, err)
	}
	assignJob(t, s, verifier.VerifierID, jobID, "key-nongate")
	*clock = deployTestNow.Add(time.Minute)
	if err := s.RecordIndependentVerification(independentRequest(verifier.VerifierID, jobID)); err != nil {
		t.Fatal(err)
	}
	pending, err := s.PendingVerificationAssignments(verifier.VerifierID)
	if err != nil || len(pending) != 0 {
		t.Fatalf("non-gate assignment pending=%+v err=%v", pending, err)
	}
}

// TestAssignmentRefusesTheSameFailureDomain keeps the separation rule from
// being discoverable only as a 403 on the evidence write.
func TestAssignmentRefusesTheSameFailureDomain(t *testing.T) {
	s, _ := assignmentTestStore(t)
	registerDeployMachine(t, s, "cnode")
	jobID := newJobForDeployTestOnMachine(t, s, "cnode")
	verifier, _, err := s.RegisterVerifier(VerifierKindFleetPeerAgent, "cnode-peer", "cnode", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.PreviewOperatorVerificationAssignment(verifier.VerifierID, jobID); !isOperatorCode(
		err, OperatorCodeVerificationAssignmentDomainConflict) {
		t.Fatalf("preview 應該以 domain conflict 拒絕，得到 %v", err)
	}
	_, err = s.ApplyOperatorVerificationAssignment(OperatorVerificationAssignmentRequest{
		VerifierID: verifier.VerifierID, JobID: jobID, PreviewDigest: "sha256:whatever",
		AssignedBy: "tailscale-user:1", IdempotencyKey: "key-same-domain",
		RequestDigest: "sha256:same-domain",
	})
	if !isOperatorCode(err, OperatorCodeVerificationAssignmentDomainConflict) {
		t.Fatalf("apply 應該以 domain conflict 拒絕，得到 %v", err)
	}
	var rows int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM verification_assignments`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("被拒絕的派工不可以留下列，得到 %d", rows)
	}
	var audited int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM audit_log WHERE action=? AND outcome<>'ok'`,
		string(AuditVerificationAssign)).Scan(&audited); err != nil {
		t.Fatal(err)
	}
	if audited != 1 {
		t.Fatalf("被拒絕的派工要留下 audit，得到 %d", audited)
	}
}

// TestRevokedVerifierKeepsItsAssignmentsOffTheHandout covers revocation landing
// after the assignment was made.
func TestRevokedVerifierKeepsItsAssignmentsOffTheHandout(t *testing.T) {
	s, clock := assignmentTestStore(t)
	registerDeployMachine(t, s, "cnode")
	registerDeployMachine(t, s, "onode")
	jobID := newJobForDeployTestOnMachine(t, s, "cnode")
	finishAssignmentJob(t, s, jobID, deployTestNow)
	verifier, _, err := s.RegisterVerifier(VerifierKindFleetPeerAgent, "onode-peer", "onode", "")
	if err != nil {
		t.Fatal(err)
	}
	assignJob(t, s, verifier.VerifierID, jobID, "key-revoked")

	*clock = deployTestNow.Add(time.Minute)
	if _, err := s.DB().Exec(`UPDATE verifiers SET revoked_at=? WHERE verifier_id=?`,
		fmtTime(*clock), verifier.VerifierID); err != nil {
		t.Fatal(err)
	}
	pending, err := s.PendingVerificationAssignments(verifier.VerifierID)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("撤銷後不該再派工，得到 %+v", pending)
	}
	// The assignment row survives: it is what explains why the job page shows a
	// producer that will never report.
	assignments, err := s.JobVerificationAssignments(jobID)
	if err != nil {
		t.Fatal(err)
	}
	if len(assignments) != 1 {
		t.Fatalf("撤銷不可以刪掉派工紀錄，得到 %+v", assignments)
	}
	if _, err := s.PreviewOperatorVerificationAssignment(verifier.VerifierID, jobID); !isOperatorCode(
		err, OperatorCodeVerifierAlreadyRevoked) {
		t.Fatalf("撤銷過的 verifier 不該能再被指派，得到 %v", err)
	}
}

func TestAssignmentRequiresAFreshPreviewAndTheTypedName(t *testing.T) {
	s, _ := assignmentTestStore(t)
	registerDeployMachine(t, s, "cnode")
	registerDeployMachine(t, s, "onode")
	jobID := newJobForDeployTestOnMachine(t, s, "cnode")
	verifier, _, err := s.RegisterVerifier(VerifierKindFleetPeerAgent, "onode-peer", "onode", "")
	if err != nil {
		t.Fatal(err)
	}
	preview, err := s.PreviewOperatorVerificationAssignment(verifier.VerifierID, jobID)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, digest, confirm, key, want string
	}{
		{"missing digest", "", verifier.DisplayName, "key-missing", OperatorCodePreviewRequired},
		{"stale digest", "sha256:0000", verifier.DisplayName, "key-stale",
			OperatorCodeVerificationAssignmentPreviewStale},
		{"no confirmation", preview.PreviewDigest, "", "key-unconfirmed",
			OperatorCodeConfirmationMismatch},
		// The verifier ID is not the name. A caller that pasted the identifier it
		// already had would otherwise confirm without reading anything.
		{"confirmed with the id", preview.PreviewDigest, verifier.VerifierID, "key-id-as-name",
			OperatorCodeConfirmationMismatch},
		{"another verifier's name", preview.PreviewDigest, "cnode-peer", "key-other-name",
			OperatorCodeConfirmationMismatch},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := s.ApplyOperatorVerificationAssignment(OperatorVerificationAssignmentRequest{
				VerifierID: verifier.VerifierID, JobID: jobID,
				ConfirmVerifierName: test.confirm, PreviewDigest: test.digest,
				AssignedBy: "tailscale-user:1", IdempotencyKey: test.key, RequestDigest: "sha256:" + test.key,
			})
			if !isOperatorCode(err, test.want) {
				t.Fatalf("預期 %s，得到 %v", test.want, err)
			}
		})
	}
	var rows int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM verification_assignments`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("被拒絕的 request 寫了 %d 列派工", rows)
	}
}

// TestAssignmentReplayReturnsTheSameRow proves the idempotency key cannot open a
// second assignment, and that a replay is refused when the ledger no longer
// backs the cached receipt.
func TestAssignmentReplayReturnsTheSameRow(t *testing.T) {
	s, _ := assignmentTestStore(t)
	registerDeployMachine(t, s, "cnode")
	registerDeployMachine(t, s, "onode")
	jobID := newJobForDeployTestOnMachine(t, s, "cnode")
	verifier, _, err := s.RegisterVerifier(VerifierKindFleetPeerAgent, "onode-peer", "onode", "")
	if err != nil {
		t.Fatal(err)
	}
	first := assignJob(t, s, verifier.VerifierID, jobID, "key-replay")
	if first.Replayed {
		t.Fatal("第一次不該是 replay")
	}
	preview, err := s.PreviewOperatorVerificationAssignment(verifier.VerifierID, jobID)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.ApplyOperatorVerificationAssignment(OperatorVerificationAssignmentRequest{
		VerifierID: verifier.VerifierID, JobID: jobID,
		ConfirmVerifierName: preview.VerifierName, PreviewDigest: preview.PreviewDigest,
		Reason: "測試派工", AssignedBy: "tailscale-user:1", IdempotencyKey: "key-replay",
		RequestDigest: "sha256:key-replay",
	})
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !second.Replayed || second.AssignmentID != first.AssignmentID {
		t.Fatalf("replay 應該回同一列，得到 %+v（原本 %+v）", second, first)
	}
	var rows int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM verification_assignments`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("replay 不可以多開一列，得到 %d", rows)
	}

	// Same key, different canonical body.
	_, err = s.ApplyOperatorVerificationAssignment(OperatorVerificationAssignmentRequest{
		VerifierID: verifier.VerifierID, JobID: jobID, PreviewDigest: preview.PreviewDigest,
		AssignedBy: "tailscale-user:1", IdempotencyKey: "key-replay", RequestDigest: "sha256:different",
	})
	if !isOperatorCode(err, OperatorCodeIdempotencyConflict) {
		t.Fatalf("同一把 key 換了 body 應該衝突，得到 %v", err)
	}

	// The receipt alone must not be able to confirm an assignment the ledger
	// does not have.
	if _, err := s.DB().Exec(`DELETE FROM verification_assignments WHERE assignment_id=?`,
		first.AssignmentID); err != nil {
		t.Fatal(err)
	}
	_, err = s.ApplyOperatorVerificationAssignment(OperatorVerificationAssignmentRequest{
		VerifierID: verifier.VerifierID, JobID: jobID, PreviewDigest: preview.PreviewDigest,
		Reason: "測試派工", AssignedBy: "tailscale-user:1", IdempotencyKey: "key-replay",
		RequestDigest: "sha256:key-replay",
	})
	if !isOperatorCode(err, OperatorCodeIdempotencyConflict) {
		t.Fatalf("ledger 沒有那一列時 replay 應該拒絕，得到 %v", err)
	}
}

func TestAssignmentRefusesAnUnknownJobOrVerifier(t *testing.T) {
	s, _ := assignmentTestStore(t)
	registerDeployMachine(t, s, "cnode")
	registerDeployMachine(t, s, "onode")
	jobID := newJobForDeployTestOnMachine(t, s, "cnode")
	verifier, _, err := s.RegisterVerifier(VerifierKindFleetPeerAgent, "onode-peer", "onode", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.PreviewOperatorVerificationAssignment("no-such-verifier", jobID); !isOperatorCode(
		err, OperatorCodeVerifierNotFound) {
		t.Fatalf("預期 verifier not found，得到 %v", err)
	}
	if _, err := s.PreviewOperatorVerificationAssignment(verifier.VerifierID, "no-such-job"); !isOperatorCode(
		err, OperatorCodeVerificationAssignmentJobNotFound) {
		t.Fatalf("預期 job not found，得到 %v", err)
	}
}

func isOperatorCode(err error, code string) bool {
	var rejection *OperatorRequestError
	return errors.As(err, &rejection) && rejection.Code == code
}
