package store

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/deploy"
)

func TestListJobReadsFiltersCountsAndPinsCreationCeiling(t *testing.T) {
	s := newDeployTestStore(t)
	registerDeployMachine(t, s, "machine-a")
	registerDeployMachine(t, s, "machine-b")

	alphaOne, alphaRevisionOne, err := s.CreateDesiredState(
		"machine", "machine-a", "package", "alpha", `{"kind":"noop"}`, "operator-a")
	if err != nil {
		t.Fatal(err)
	}
	alphaJobOne, err := s.CreateJob("machine-a", alphaOne, alphaRevisionOne, NewJob{})
	if err != nil {
		t.Fatal(err)
	}
	alphaTwo, alphaRevisionTwo, err := s.CreateDesiredState(
		"machine", "machine-a", "package", "alpha", `{"kind":"noop"}`, "operator-b")
	if err != nil {
		t.Fatal(err)
	}
	alphaJobTwo, err := s.CreateJob("machine-a", alphaTwo, alphaRevisionTwo, NewJob{
		ArtifactDigest: "sha256:" + repeatForJobReadTest("a", 64),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimJob(alphaJobTwo, "machine-a", deployTestNow, time.Hour); err != nil {
		t.Fatal(err)
	}
	betaDesired, betaRevision, err := s.CreateDesiredState(
		"machine", "machine-b", "package", "beta", `{"kind":"noop"}`, "operator-c")
	if err != nil {
		t.Fatal(err)
	}
	betaJob, err := s.CreateJob("machine-b", betaDesired, betaRevision, NewJob{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(`UPDATE jobs SET state=?, terminal_at=? WHERE job_id=?`,
		deploy.Failed, fmtTime(deployTestNow.Add(time.Minute)), betaJob); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(`INSERT INTO deployments
 (deployment_id,channel,desired_id,resource_kind,resource_id,revision,batch_size,state,created_at,created_by,finished_at)
 VALUES (?,?,?,?,?,?,?,?,?,?,?)`, "deployment-alpha", "canary", alphaTwo, "package", "alpha",
		alphaRevisionTwo, 1, "finished", fmtTime(deployTestNow), "operator-b", fmtTime(deployTestNow)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(`INSERT INTO deployment_targets
 (deployment_id,machine_id,batch_no,job_id) VALUES (?,?,?,?)`,
		"deployment-alpha", "machine-a", 1, alphaJobTwo); err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name      string
		filter    JobReadFilter
		wantTotal int
		wantJob   string
	}{
		{name: "machine", filter: JobReadFilter{MachineID: "machine-b", Limit: 10}, wantTotal: 1, wantJob: betaJob},
		{name: "state", filter: JobReadFilter{States: []deploy.JobState{deploy.Claimed}, Limit: 10}, wantTotal: 1, wantJob: alphaJobTwo},
		{name: "deployment", filter: JobReadFilter{DeploymentID: "deployment-alpha", Limit: 10}, wantTotal: 1, wantJob: alphaJobTwo},
		{name: "resource-kind", filter: JobReadFilter{ResourceKind: "package", Limit: 10}, wantTotal: 3},
		{name: "resource", filter: JobReadFilter{ResourceKind: "package", ResourceID: "alpha", Limit: 10}, wantTotal: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			page, err := s.ListJobReads(test.filter)
			if err != nil {
				t.Fatal(err)
			}
			if page.Total != test.wantTotal || len(page.Items) != test.wantTotal {
				t.Fatalf("total=%d items=%d want=%d", page.Total, len(page.Items), test.wantTotal)
			}
			assertCanonicalJobReadCounts(t, page.StateCounts, test.wantTotal)
			if test.wantJob != "" && page.Items[0].JobID != test.wantJob {
				t.Fatalf("job_id=%q want=%q", page.Items[0].JobID, test.wantJob)
			}
		})
	}

	first, err := s.ListJobReads(JobReadFilter{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if first.Total != 3 || len(first.Items) != 2 || first.Next == nil || first.CreationCeiling <= 0 {
		t.Fatalf("first page=%+v", first)
	}
	assertJobReadDescending(t, first.Items)
	original := map[string]bool{alphaJobOne: true, alphaJobTwo: true, betaJob: true}
	seen := make(map[string]bool, len(original))
	for _, item := range first.Items {
		seen[item.JobID] = true
	}

	alphaThree, alphaRevisionThree, err := s.CreateDesiredState(
		"machine", "machine-a", "package", "alpha", `{"kind":"noop"}`, "later-operator")
	if err != nil {
		t.Fatal(err)
	}
	createdAfterFirstPage, err := s.CreateJob("machine-a", alphaThree, alphaRevisionThree, NewJob{})
	if err != nil {
		t.Fatal(err)
	}
	// A writer can insert a back-dated row between pages. The per-traversal
	// rowid ceiling, not wall-clock ordering alone, keeps it out.
	if _, err := s.DB().Exec(`UPDATE jobs SET created_at=? WHERE job_id=?`,
		fmtTime(deployTestNow.Add(-24*time.Hour)), createdAfterFirstPage); err != nil {
		t.Fatal(err)
	}
	ceiling := first.CreationCeiling
	second, err := s.ListJobReads(JobReadFilter{
		Limit: 2, CreationCeiling: &ceiling, After: first.Next,
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.Total != 3 || len(second.Items) != 1 || second.Next != nil ||
		second.CreationCeiling != first.CreationCeiling {
		t.Fatalf("second page=%+v", second)
	}
	for _, item := range second.Items {
		if seen[item.JobID] {
			t.Fatalf("job %q repeated across pages", item.JobID)
		}
		seen[item.JobID] = true
	}
	if len(seen) != len(original) || seen[createdAfterFirstPage] {
		t.Fatalf("creation-ceiling traversal=%v original=%v late=%q", seen, original, createdAfterFirstPage)
	}
	for jobID := range original {
		if !seen[jobID] {
			t.Errorf("original job %q was skipped", jobID)
		}
	}
}

func TestListJobReadsReevaluatesMutableStateBetweenPages(t *testing.T) {
	s := newDeployTestStore(t)
	registerDeployMachine(t, s, "machine-live-page")
	for i := 0; i < 3; i++ {
		desiredID, revision, err := s.CreateDesiredState(
			"machine", "machine-live-page", "diagnostic", fmt.Sprintf("live-%d", i),
			`{"kind":"noop"}`, "test")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.CreateJob("machine-live-page", desiredID, revision, NewJob{}); err != nil {
			t.Fatal(err)
		}
	}
	first, err := s.ListJobReads(JobReadFilter{Limit: 1})
	if err != nil || first.Next == nil {
		t.Fatalf("first page=%+v err=%v", first, err)
	}
	ceiling := first.CreationCeiling
	preview, err := s.ListJobReads(JobReadFilter{Limit: 1, CreationCeiling: &ceiling, After: first.Next})
	if err != nil || len(preview.Items) != 1 {
		t.Fatalf("preview second page=%+v err=%v", preview, err)
	}
	changedJobID := preview.Items[0].JobID
	if _, err := s.DB().Exec(`UPDATE jobs SET state=? WHERE job_id=?`, deploy.Claimed, changedJobID); err != nil {
		t.Fatal(err)
	}
	second, err := s.ListJobReads(JobReadFilter{Limit: 1, CreationCeiling: &ceiling, After: first.Next})
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Items) != 1 || second.Items[0].JobID != changedJobID || second.Items[0].State != deploy.Claimed {
		t.Fatalf("second page did not re-evaluate mutable state: %+v", second)
	}
	counts := make(map[deploy.JobState]int, len(second.StateCounts))
	for _, count := range second.StateCounts {
		counts[count.State] = count.Count
	}
	if second.Total != 3 || counts[deploy.Claimed] != 1 || counts[deploy.NotStarted] != 2 {
		t.Fatalf("live counts total=%d counts=%v", second.Total, counts)
	}
}

func TestListJobReadsRejectsCursorCeilingFromTruncatedLedger(t *testing.T) {
	s := newDeployTestStore(t)
	registerDeployMachine(t, s, "machine-truncated-page")
	for i := 0; i < 2; i++ {
		desiredID, revision, err := s.CreateDesiredState(
			"machine", "machine-truncated-page", "diagnostic", fmt.Sprintf("truncate-%d", i),
			`{"kind":"noop"}`, "test")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.CreateJob("machine-truncated-page", desiredID, revision, NewJob{}); err != nil {
			t.Fatal(err)
		}
	}
	first, err := s.ListJobReads(JobReadFilter{Limit: 1})
	if err != nil || first.Next == nil {
		t.Fatalf("first page=%+v err=%v", first, err)
	}
	if _, err := s.DB().Exec(`DELETE FROM jobs WHERE rowid=?`, first.CreationCeiling); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ListJobReads(JobReadFilter{
		Limit: 1, CreationCeiling: &first.CreationCeiling, After: first.Next,
	}); !errors.Is(err, ErrInvalidJobRead) {
		t.Fatalf("truncated-ledger cursor error=%v want ErrInvalidJobRead", err)
	}
}

func TestJobReadDetailBoundsNativeEvidenceAndNeverReadsLeaseToken(t *testing.T) {
	s := newDeployTestStore(t)
	registerDeployMachine(t, s, "machine-detail")
	desiredID, revision, err := s.CreateDesiredState(
		"machine", "machine-detail", "diagnostic", "safe-read",
		`{"private_path":"/private/spec/path"}`, "RAW_CREATED_BY_SENTINEL")
	if err != nil {
		t.Fatal(err)
	}
	jobID, err := s.CreateJob("machine-detail", desiredID, revision, NewJob{
		ArtifactDigest: "sha256:" + repeatForJobReadTest("b", 64),
		Irreversible:   true, ExecutionTimeout: 123,
	})
	if err != nil {
		t.Fatal(err)
	}
	leaseToken, err := s.ClaimJob(jobID, "machine-detail", deployTestNow, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 5; i++ {
		at := deployTestNow.Add(time.Duration(i) * time.Second)
		if _, err := s.DB().Exec(`INSERT INTO job_events
 (event_id,job_id,seq,phase,occurred_at,received_at,payload) VALUES (?,?,?,?,?,?,?)`,
			fmt.Sprintf("event-%02d", i), jobID, i, fmt.Sprintf("phase-%d", i),
			fmtTime(at), fmtTime(at), fmt.Sprintf(`{"private":"event-%d"}`, i)); err != nil {
			t.Fatal(err)
		}
		if _, err := s.DB().Exec(`INSERT INTO verification_results
 (verification_id,job_id,machine_id,rule_id,command,exit_code,stdout_excerpt,stderr_excerpt,passed,verified_at)
 VALUES (?,?,?,?,?,?,?,?,?,?)`, fmt.Sprintf("verification-%02d", i), jobID, "machine-detail",
			fmt.Sprintf("RAW_RULE_%d", i), fmt.Sprintf("RAW_COMMAND_%d", i), i,
			fmt.Sprintf("RAW_STDOUT_%d", i), fmt.Sprintf("/private/stderr/%d", i), i%2 == 0,
			fmtTime(at)); err != nil {
			t.Fatal(err)
		}
	}

	safeDetail, err := s.JobReadDetail(jobID, 3)
	if err != nil {
		t.Fatal(err)
	}
	if safeDetail.Desired.Spec != "" || safeDetail.Desired.CreatedBy != "" {
		t.Fatalf("safe native detail materialized raw desired content: %+v", safeDetail.Desired)
	}
	for _, event := range safeDetail.Events {
		if event.Payload != "" || event.Phase != "other" {
			t.Fatalf("safe native detail materialized raw event content: %+v", event)
		}
	}
	for _, verification := range safeDetail.Verifications {
		if verification.RuleID != "" || verification.Command != "" ||
			verification.StdoutExcerpt != "" || verification.StderrExcerpt != "" {
			t.Fatalf("safe native detail materialized verifier raw content: %+v", verification)
		}
	}
	if safeDetail.Job.LeaseToken != "" {
		t.Fatalf("safe native detail retained lease token %q", safeDetail.Job.LeaseToken)
	}
	var persistedLease string
	if err := s.DB().QueryRow(`SELECT lease_token FROM jobs WHERE job_id=?`, jobID).Scan(&persistedLease); err != nil {
		t.Fatal(err)
	}
	if persistedLease != leaseToken {
		t.Fatalf("read changed persisted lease token: got %q want %q", persistedLease, leaseToken)
	}
	if safeDetail.Item.EventCount != 5 || len(safeDetail.Events) != 3 || !safeDetail.EventsTruncated ||
		safeDetail.Item.VerificationTotal != 5 || safeDetail.Item.VerificationPassed != 2 ||
		safeDetail.Item.VerificationFailed != 3 || len(safeDetail.Verifications) != 3 ||
		!safeDetail.VerificationsTruncated {
		t.Fatalf("bounded safe detail=%+v", safeDetail)
	}
}

func TestJobReadEvidenceReturnsBoundedRawEvidenceWithoutLeaseToken(t *testing.T) {
	s := newDeployTestStore(t)
	registerDeployMachine(t, s, "machine-evidence")
	spec := `{"private_path":"/private/evidence/spec"}`
	desiredID, revision, err := s.CreateDesiredState(
		"machine", "machine-evidence", "diagnostic", "evidence", spec, "PRIVATE_CREATOR")
	if err != nil {
		t.Fatal(err)
	}
	jobID, err := s.CreateJob("machine-evidence", desiredID, revision, NewJob{})
	if err != nil {
		t.Fatal(err)
	}
	leaseToken, err := s.ClaimJob(jobID, "machine-evidence", deployTestNow, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if leaseToken == "" {
		t.Fatal("claim returned an empty lease token")
	}
	for i := 1; i <= 3; i++ {
		at := deployTestNow.Add(time.Duration(i) * time.Second)
		if _, err := s.DB().Exec(`INSERT INTO job_events
 (event_id,job_id,seq,phase,occurred_at,received_at,payload) VALUES (?,?,?,?,?,?,?)`,
			fmt.Sprintf("evidence-event-%d", i), jobID, i, fmt.Sprintf("raw-phase-%d", i),
			fmtTime(at), fmtTime(at), fmt.Sprintf(`{"payload":%d}`, i)); err != nil {
			t.Fatal(err)
		}
		if _, err := s.DB().Exec(`INSERT INTO verification_results
 (verification_id,job_id,machine_id,rule_id,command,exit_code,stdout_excerpt,stderr_excerpt,passed,verified_at)
 VALUES (?,?,?,?,?,?,?,?,?,?)`, fmt.Sprintf("evidence-verification-%d", i), jobID,
			"machine-evidence", fmt.Sprintf("rule-%d", i), fmt.Sprintf("command-%d", i), i,
			fmt.Sprintf("stdout-%d", i), fmt.Sprintf("stderr-%d", i), i%2 == 1, fmtTime(at)); err != nil {
			t.Fatal(err)
		}
	}

	evidence, err := s.JobReadEvidence(jobID, 2)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(fmt.Sprintf("%+v", evidence), leaseToken) {
		t.Fatal("JobReadEvidence result exposed the lease token")
	}
	if evidence.Desired.Spec != spec || evidence.Desired.CreatedBy != "" {
		t.Fatalf("evidence desired=%+v", evidence.Desired)
	}
	if len(evidence.Events) != 2 || !evidence.EventsTruncated ||
		evidence.Events[0].Seq != 2 || evidence.Events[1].Seq != 3 ||
		evidence.Events[0].Phase != "raw-phase-2" || evidence.Events[0].Payload != `{"payload":2}` {
		t.Fatalf("bounded raw events=%+v truncated=%v", evidence.Events, evidence.EventsTruncated)
	}
	if len(evidence.Verifications) != 2 || !evidence.VerificationsTruncated ||
		evidence.Verifications[0].VerificationID != "evidence-verification-2" ||
		evidence.Verifications[1].VerificationID != "evidence-verification-3" ||
		evidence.Verifications[0].RuleID != "rule-2" ||
		evidence.Verifications[0].Command != "command-2" ||
		evidence.Verifications[0].StdoutExcerpt != "stdout-2" ||
		evidence.Verifications[0].StderrExcerpt != "stderr-2" {
		t.Fatalf("bounded raw verifications=%+v truncated=%v",
			evidence.Verifications, evidence.VerificationsTruncated)
	}
	var persistedLease string
	if err := s.DB().QueryRow(`SELECT lease_token FROM jobs WHERE job_id=?`, jobID).Scan(&persistedLease); err != nil {
		t.Fatal(err)
	}
	if persistedLease != leaseToken {
		t.Fatalf("evidence read changed or exposed lease token: got persisted %q want %q", persistedLease, leaseToken)
	}
	for _, limit := range []int{1, MaxJobReadPageSize} {
		if _, err := s.JobReadEvidence(jobID, limit); err != nil {
			t.Errorf("valid limit %d was rejected: %v", limit, err)
		}
	}

	for _, limit := range []int{0, MaxJobReadPageSize + 1} {
		if _, err := s.JobReadEvidence(jobID, limit); !errors.Is(err, ErrInvalidJobRead) {
			t.Errorf("limit %d error=%v want ErrInvalidJobRead", limit, err)
		}
	}
	if _, err := s.JobReadEvidence("missing", 1); !errors.Is(err, ErrJobNotFound) {
		t.Fatalf("missing evidence error=%v want ErrJobNotFound", err)
	}

	t.Run("desired metadata mismatch", func(t *testing.T) {
		corrupt := newDeployTestStore(t)
		registerDeployMachine(t, corrupt, "machine-desired-corrupt")
		desired, rev, err := corrupt.CreateDesiredState(
			"machine", "machine-desired-corrupt", "diagnostic", "corrupt", `{}`, "test")
		if err != nil {
			t.Fatal(err)
		}
		job, err := corrupt.CreateJob("machine-desired-corrupt", desired, rev, NewJob{})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := corrupt.DB().Exec(`UPDATE desired_state SET revision=revision+1 WHERE desired_id=?`, desired); err != nil {
			t.Fatal(err)
		}
		if _, err := corrupt.JobReadEvidence(job, 1); !errors.Is(err, ErrJobReadCorrupt) {
			t.Fatalf("mismatched desired error=%v want ErrJobReadCorrupt", err)
		}
	})

	t.Run("cross-machine verification", func(t *testing.T) {
		corrupt := newDeployTestStore(t)
		registerDeployMachine(t, corrupt, "machine-proof-owner")
		registerDeployMachine(t, corrupt, "machine-proof-other")
		desired, rev, err := corrupt.CreateDesiredState(
			"machine", "machine-proof-owner", "diagnostic", "proof", `{}`, "test")
		if err != nil {
			t.Fatal(err)
		}
		job, err := corrupt.CreateJob("machine-proof-owner", desired, rev, NewJob{})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := corrupt.DB().Exec(`INSERT INTO verification_results
 (verification_id,job_id,machine_id,rule_id,command,passed,verified_at)
 VALUES (?,?,?,?,?,?,?)`, "cross-machine", job, "machine-proof-other", "rule", "true", true,
			fmtTime(deployTestNow)); err != nil {
			t.Fatal(err)
		}
		if _, err := corrupt.JobReadEvidence(job, 1); !errors.Is(err, ErrJobReadCorrupt) {
			t.Fatalf("cross-machine verification error=%v want ErrJobReadCorrupt", err)
		}
	})
}

func TestJobReadsRejectInvalidRequestsAndCorruptLedger(t *testing.T) {
	s := newDeployTestStore(t)
	registerDeployMachine(t, s, "machine-corrupt")
	desiredID, revision, err := s.CreateDesiredState(
		"machine", "machine-corrupt", "diagnostic", "corrupt", `{"kind":"noop"}`, "test")
	if err != nil {
		t.Fatal(err)
	}
	jobID, err := s.CreateJob("machine-corrupt", desiredID, revision, NewJob{})
	if err != nil {
		t.Fatal(err)
	}
	ceiling := int64(0)
	validAfter := JobReadPosition{CreatedAt: deployTestNow, Revision: revision, JobID: jobID}
	nanosAfter := JobReadPosition{CreatedAt: deployTestNow.Add(time.Nanosecond), Revision: revision, JobID: jobID}
	for name, filter := range map[string]JobReadFilter{
		"zero-limit":           {Limit: 0},
		"large-limit":          {Limit: MaxJobReadPageSize + 1},
		"resource-id-alone":    {ResourceID: "corrupt", Limit: 1},
		"unknown-state":        {States: []deploy.JobState{"future"}, Limit: 1},
		"duplicate-state":      {States: []deploy.JobState{deploy.NotStarted, deploy.NotStarted}, Limit: 1},
		"whitespace-machine":   {MachineID: " machine-corrupt", Limit: 1},
		"after-no-ceiling":     {After: &validAfter, Limit: 1},
		"after-empty-position": {CreationCeiling: &ceiling, After: &JobReadPosition{}, Limit: 1},
		"after-nanoseconds":    {CreationCeiling: &ceiling, After: &nanosAfter, Limit: 1},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := s.ListJobReads(filter); !errors.Is(err, ErrInvalidJobRead) {
				t.Fatalf("error=%v want ErrInvalidJobRead", err)
			}
		})
	}
	negativeCeiling := int64(-1)
	if _, err := s.ListJobReads(JobReadFilter{Limit: 1, CreationCeiling: &negativeCeiling}); !errors.Is(err, ErrInvalidJobRead) {
		t.Fatalf("negative ceiling error=%v", err)
	}
	for _, target := range []string{"", " " + jobID} {
		if _, err := s.JobReadDetail(target, 1); !errors.Is(err, ErrInvalidJobRead) {
			t.Fatalf("detail target %q error=%v want ErrInvalidJobRead", target, err)
		}
	}
	if _, err := s.JobReadDetail(jobID, 0); !errors.Is(err, ErrInvalidJobRead) {
		t.Fatalf("zero evidence limit error=%v want ErrInvalidJobRead", err)
	}
	if _, err := s.JobReadDetail("missing", 1); !errors.Is(err, ErrJobNotFound) {
		t.Fatalf("missing detail error=%v want ErrJobNotFound", err)
	}
	if _, err := s.DB().Exec(`UPDATE desired_state SET revision=revision+1 WHERE desired_id=?`, desiredID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.JobReadDetail(jobID, 1); !errors.Is(err, ErrJobReadCorrupt) {
		t.Fatalf("mismatched desired revision error=%v want ErrJobReadCorrupt", err)
	}
	if _, err := s.ListJobReads(JobReadFilter{Limit: 1}); !errors.Is(err, ErrJobReadCorrupt) {
		t.Fatalf("mismatched desired revision list error=%v want ErrJobReadCorrupt", err)
	}
	if _, err := s.DB().Exec(`UPDATE desired_state SET revision=? WHERE desired_id=?`, revision, desiredID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(`UPDATE jobs SET lease_expires_at='not-a-time' WHERE job_id=?`, jobID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ListJobReads(JobReadFilter{Limit: 1}); !errors.Is(err, ErrJobReadCorrupt) {
		t.Fatalf("invalid lease timestamp error=%v want ErrJobReadCorrupt", err)
	}
	if _, err := s.DB().Exec(`UPDATE jobs SET lease_expires_at=NULL WHERE job_id=?`, jobID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(`UPDATE jobs SET created_at=? WHERE job_id=?`,
		deployTestNow.Add(500*time.Millisecond).Format(time.RFC3339Nano), jobID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ListJobReads(JobReadFilter{Limit: 1}); !errors.Is(err, ErrJobReadCorrupt) {
		t.Fatalf("noncanonical creation timestamp error=%v want ErrJobReadCorrupt", err)
	}
	if _, err := s.DB().Exec(`UPDATE jobs SET created_at=? WHERE job_id=?`, fmtTime(deployTestNow), jobID); err != nil {
		t.Fatal(err)
	}

	if _, err := s.DB().Exec(`UPDATE jobs SET state=? WHERE job_id=?`, "future-state", jobID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ListJobReads(JobReadFilter{Limit: 1}); !errors.Is(err, ErrJobReadCorrupt) {
		t.Fatalf("corrupt state list error=%v want ErrJobReadCorrupt", err)
	}
	if _, err := s.JobReadDetail(jobID, 1); !errors.Is(err, ErrJobReadCorrupt) {
		t.Fatalf("corrupt state detail error=%v want ErrJobReadCorrupt", err)
	}
}

func TestJobReadDetailRejectsNotStartedJobWithExecutorEvidence(t *testing.T) {
	for _, test := range []struct {
		name   string
		inject func(*testing.T, *Store, string, string)
	}{
		{"executor event only", func(t *testing.T, s *Store, jobID, machineID string) {
			at := deployTestNow.Add(time.Second)
			if _, err := s.DB().Exec(`INSERT INTO job_events
 (event_id,job_id,seq,phase,occurred_at,received_at,payload,
  producer_kind,producer_id,evidence_role,authority,provenance_recorded)
 VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`, "unclaimed-event", jobID, 1, "start",
				fmtTime(at), fmtTime(at), `{}`, JobEventProducerExecutorAgent, machineID,
				JobEventRoleExecutor, JobEventAuthorityMachineLease, true); err != nil {
				t.Fatal(err)
			}
		}},
		{"executor verification only", func(t *testing.T, s *Store, jobID, machineID string) {
			verifiedAt := deployTestNow.Add(time.Second)
			if _, err := s.DB().Exec(`INSERT INTO verification_results
 (verification_id,job_id,machine_id,rule_id,command,exit_code,passed,verified_at,
  producer_kind,producer_id,evidence_role,authority,provenance_recorded,received_at)
 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, "unclaimed-verification", jobID, machineID,
				"executor-rule", "true", 0, true, fmtTime(verifiedAt), JobVerificationProducerExecutorAgent,
				machineID, JobVerificationRoleExecutor, JobVerificationAuthorityMachineLease, true,
				fmtTime(verifiedAt)); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := newDeployTestStore(t)
			const machineID = "machine-unclaimed-executor-evidence"
			registerDeployMachine(t, s, machineID)
			desiredID, revision, err := s.CreateDesiredState(
				"machine", machineID, "diagnostic", "unclaimed-executor-evidence", `{}`, "test")
			if err != nil {
				t.Fatal(err)
			}
			jobID, err := s.CreateJob(machineID, desiredID, revision, NewJob{})
			if err != nil {
				t.Fatal(err)
			}
			test.inject(t, s, jobID, machineID)

			if detail, err := s.JobReadDetail(jobID, 10); !errors.Is(err, ErrJobReadCorrupt) {
				t.Fatalf("detail=%+v error=%v want ErrJobReadCorrupt", detail, err)
			}
			if page, err := s.ListJobReads(JobReadFilter{Limit: 10}); !errors.Is(err, ErrJobReadCorrupt) {
				t.Fatalf("list=%+v error=%v want ErrJobReadCorrupt", page, err)
			}
			if evidence, err := s.JobReadEvidence(jobID, 10); !errors.Is(err, ErrJobReadCorrupt) {
				t.Fatalf("evidence=%+v error=%v want ErrJobReadCorrupt", evidence, err)
			}
		})
	}
}

func TestJobReadDetailAllowsNotStartedJobWithIndependentVerification(t *testing.T) {
	s := newDeployTestStore(t)
	const machineID = "machine-unclaimed-independent-evidence"
	registerDeployMachine(t, s, machineID)
	desiredID, revision, err := s.CreateDesiredState(
		"machine", machineID, "diagnostic", "unclaimed-independent-evidence", `{}`, "test")
	if err != nil {
		t.Fatal(err)
	}
	jobID, err := s.CreateJob(machineID, desiredID, revision, NewJob{})
	if err != nil {
		t.Fatal(err)
	}
	verifier, _, err := s.RegisterVerifier(
		VerifierKindExternalJobRunner, "independent-reader", "awx-independent-reader", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RecordIndependentVerification(independentRequest(verifier.VerifierID, jobID)); err != nil {
		t.Fatal(err)
	}

	detail, err := s.JobReadDetail(jobID, 10)
	if err != nil {
		t.Fatalf("read not_started job with independent verification: %v", err)
	}
	if detail.Item.State != deploy.NotStarted || detail.Item.IndependentTotal != 1 ||
		detail.Item.EventCount != 0 || detail.Item.VerificationTotal != 0 {
		t.Fatalf("detail=%+v", detail)
	}
}

func TestJobReadEvidenceRejectsCorruptVerificationProvenance(t *testing.T) {
	for name, update := range map[string]string{
		"producer kind":              `producer_kind='controller_verifier'`,
		"producer identity":          `producer_id='machine-other'`,
		"evidence role":              `evidence_role='verifier'`,
		"authority":                  `authority='operator_session'`,
		"recorded without id":        `provenance_recorded=1,received_at='2026-09-06T12:00:00Z'`,
		"recorded without hub clock": `producer_id='machine-proof',provenance_recorded=1`,
		"legacy with hub clock":      `received_at='2026-09-06T12:00:00Z'`,
		"malformed hub clock":        `producer_id='machine-proof',provenance_recorded=1,received_at='not-a-time'`,
	} {
		t.Run(name, func(t *testing.T) {
			s := newDeployTestStore(t)
			registerDeployMachine(t, s, "machine-proof")
			jobID := newJobForDeployTest(t, s, "machine-proof")
			if _, err := s.DB().Exec(`INSERT INTO verification_results
 (verification_id,job_id,machine_id,rule_id,command,passed,verified_at)
 VALUES ('proof',?,?,?,?,1,?)`, jobID, "machine-proof", "health", "true",
				fmtTime(deployTestNow)); err != nil {
				t.Fatal(err)
			}
			if _, err := s.DB().Exec(`UPDATE verification_results SET ` + update + ` WHERE verification_id='proof'`); err != nil {
				t.Fatal(err)
			}
			if _, err := s.JobReadEvidence(jobID, 1); !errors.Is(err, ErrJobReadCorrupt) {
				t.Fatalf("error=%v want ErrJobReadCorrupt", err)
			}
		})
	}
}

func TestJobReadsRejectCrossMachineVerificationAndUnprovenSuccess(t *testing.T) {
	s := newDeployTestStore(t)
	registerDeployMachine(t, s, "machine-proof")
	registerDeployMachine(t, s, "machine-other")
	desiredID, revision, err := s.CreateDesiredState(
		"machine", "machine-proof", "diagnostic", "proof", `{"kind":"noop"}`, "test")
	if err != nil {
		t.Fatal(err)
	}
	jobID, err := s.CreateJob("machine-proof", desiredID, revision, NewJob{})
	if err != nil {
		t.Fatal(err)
	}
	insertVerification := func(id, machineID string, passed bool) {
		t.Helper()
		if _, err := s.DB().Exec(`INSERT INTO verification_results
 (verification_id,job_id,machine_id,rule_id,command,exit_code,passed,verified_at)
 VALUES (?,?,?,?,?,?,?,?)`, id, jobID, machineID, "health", "true", 0, passed,
			fmtTime(deployTestNow)); err != nil {
			t.Fatal(err)
		}
	}

	insertVerification("verification-other", "machine-other", true)
	if _, err := s.ListJobReads(JobReadFilter{Limit: 10}); !errors.Is(err, ErrJobReadCorrupt) {
		t.Fatalf("cross-machine list error=%v want ErrJobReadCorrupt", err)
	}
	if _, err := s.JobReadDetail(jobID, 10); !errors.Is(err, ErrJobReadCorrupt) {
		t.Fatalf("cross-machine detail error=%v want ErrJobReadCorrupt", err)
	}
	if _, err := s.DB().Exec(`DELETE FROM verification_results WHERE job_id=?`, jobID); err != nil {
		t.Fatal(err)
	}

	if _, err := s.DB().Exec(`UPDATE jobs SET state=?,terminal_at=? WHERE job_id=?`,
		deploy.Succeeded, fmtTime(deployTestNow), jobID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ListJobReads(JobReadFilter{Limit: 10}); !errors.Is(err, ErrJobReadCorrupt) {
		t.Fatalf("unproven success list error=%v want ErrJobReadCorrupt", err)
	}
	if _, err := s.JobReadDetail(jobID, 10); !errors.Is(err, ErrJobReadCorrupt) {
		t.Fatalf("unproven success detail error=%v want ErrJobReadCorrupt", err)
	}

	insertVerification("verification-pass", "machine-proof", true)
	if _, err := s.ListJobReads(JobReadFilter{Limit: 10}); err != nil {
		t.Fatalf("proven success was rejected: %v", err)
	}
	insertVerification("verification-fail", "machine-proof", false)
	if _, err := s.ListJobReads(JobReadFilter{Limit: 10}); !errors.Is(err, ErrJobReadCorrupt) {
		t.Fatalf("contradictory success error=%v want ErrJobReadCorrupt", err)
	}
}

func TestJobReadsRejectCrossMachineDesiredScopeAndRouteUnsafeIdentity(t *testing.T) {
	t.Run("machine desired scope", func(t *testing.T) {
		s := newDeployTestStore(t)
		registerDeployMachine(t, s, "machine-scope-owner")
		registerDeployMachine(t, s, "machine-scope-other")
		desiredID, revision, err := s.CreateDesiredState(
			"machine", "machine-scope-owner", "diagnostic", "scope", `{"kind":"noop"}`, "test")
		if err != nil {
			t.Fatal(err)
		}
		jobID, err := s.CreateJob("machine-scope-other", desiredID, revision, NewJob{})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.ListJobReads(JobReadFilter{Limit: 10}); !errors.Is(err, ErrJobReadCorrupt) {
			t.Fatalf("cross-machine desired list error=%v want ErrJobReadCorrupt", err)
		}
		if _, err := s.JobReadDetail(jobID, 10); !errors.Is(err, ErrJobReadCorrupt) {
			t.Fatalf("cross-machine desired detail error=%v want ErrJobReadCorrupt", err)
		}
	})

	t.Run("route-unsafe job id", func(t *testing.T) {
		s := newDeployTestStore(t)
		registerDeployMachine(t, s, "machine-route")
		desiredID, revision, err := s.CreateDesiredState(
			"machine", "machine-route", "diagnostic", "route", `{"kind":"noop"}`, "test")
		if err != nil {
			t.Fatal(err)
		}
		jobID, err := s.CreateJob("machine-route", desiredID, revision, NewJob{})
		if err != nil {
			t.Fatal(err)
		}
		unsafeJobID := "jobs/" + jobID
		if _, err := s.DB().Exec(`UPDATE jobs SET job_id=? WHERE job_id=?`, unsafeJobID, jobID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.ListJobReads(JobReadFilter{Limit: 10}); !errors.Is(err, ErrJobReadCorrupt) {
			t.Fatalf("route-unsafe list error=%v want ErrJobReadCorrupt", err)
		}
		if _, err := s.JobReadDetail(unsafeJobID, 10); !errors.Is(err, ErrInvalidJobRead) {
			t.Fatalf("route-unsafe detail error=%v want ErrInvalidJobRead", err)
		}
	})
}

func TestJobReadsRejectFailureStateThatContradictsIrreversibility(t *testing.T) {
	for _, test := range []struct {
		name         string
		state        deploy.JobState
		irreversible bool
	}{
		{name: "irreversible failed", state: deploy.Failed, irreversible: true},
		{name: "reversible manual intervention", state: deploy.ManualIntervention, irreversible: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := newDeployTestStore(t)
			registerDeployMachine(t, s, "machine-failure-invariant")
			desiredID, revision, err := s.CreateDesiredState(
				"machine", "machine-failure-invariant", "diagnostic", "failure", `{"kind":"noop"}`, "test")
			if err != nil {
				t.Fatal(err)
			}
			jobID, err := s.CreateJob("machine-failure-invariant", desiredID, revision,
				NewJob{Irreversible: test.irreversible})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.DB().Exec(`UPDATE jobs SET state=?,terminal_at=? WHERE job_id=?`,
				test.state, fmtTime(deployTestNow), jobID); err != nil {
				t.Fatal(err)
			}
			if _, err := s.ListJobReads(JobReadFilter{Limit: 10}); !errors.Is(err, ErrJobReadCorrupt) {
				t.Fatalf("list error=%v want ErrJobReadCorrupt", err)
			}
			if _, err := s.JobReadDetail(jobID, 10); !errors.Is(err, ErrJobReadCorrupt) {
				t.Fatalf("detail error=%v want ErrJobReadCorrupt", err)
			}
		})
	}
}

func TestJobReadSchemaIndexesExist(t *testing.T) {
	s := newDeployTestStore(t)
	for _, name := range []string{
		"ix_jobs_read_created", "ix_jobs_read_machine_created", "ix_jobs_read_state_created",
		"ix_jobs_machine_state", "ix_jobs_read_desired_created", "ix_desired_state_read_resource",
		"ix_job_events_read_received", "ix_job_events_read_seq",
		"ix_verifications_read_verified", "ix_deployment_targets_job",
	} {
		var count int
		if err := s.DB().QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name=?`, name).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Errorf("schema index %q count=%d want=1", name, count)
		}
	}
}

func assertCanonicalJobReadCounts(t *testing.T, counts []JobReadStateCount, total int) {
	t.Helper()
	if len(counts) != len(deploy.AllJobStates) {
		t.Fatalf("state count length=%d want=%d", len(counts), len(deploy.AllJobStates))
	}
	sum := 0
	for i, state := range deploy.AllJobStates {
		if counts[i].State != state || counts[i].Count < 0 {
			t.Fatalf("state_counts[%d]=%+v want canonical %q", i, counts[i], state)
		}
		sum += counts[i].Count
	}
	if sum != total {
		t.Fatalf("state count sum=%d want total=%d", sum, total)
	}
}

func assertJobReadDescending(t *testing.T, items []JobReadItem) {
	t.Helper()
	for i := 1; i < len(items); i++ {
		previous, current := items[i-1], items[i]
		ordered := previous.CreatedAt.After(current.CreatedAt) ||
			(previous.CreatedAt.Equal(current.CreatedAt) && previous.Revision > current.Revision) ||
			(previous.CreatedAt.Equal(current.CreatedAt) && previous.Revision == current.Revision &&
				previous.JobID > current.JobID)
		if !ordered {
			t.Fatalf("items are not strict descending keyset order at %d: previous=%+v current=%+v", i, previous, current)
		}
	}
}

func repeatForJobReadTest(value string, count int) string {
	result := ""
	for i := 0; i < count; i++ {
		result += value
	}
	return result
}

// 這一列除了 producer_id 以外每一欄都是產品寫入路徑親手寫的，所以拒絕只能
// 歸因於 :759 的 ProducerID == VerifierID。其餘七條 && 都還成立：kind 仍是
// fleet_peer_agent、authority 仍是 verifier_bearer、provenance_recorded 仍是
// 1、received_at 仍是 canonical、digest 仍合法。呼叫端 :726-730 也看不到
// producer_id。
//
// ⚠ 名冊 lookup（readJobEvidenceVerifiers:796）與 verdict 對帳（:500）都是用
// verifier_id 去 JOIN verifiers，不是用 producer_id，所以它們兩個都不會先
// 開火——這正是這一條沒有後備的原因。把 verifier_id 清空去偽造的話，測到的
// 會是那兩道而不是這一支。
//
// ⚠ 這條在畫面上看不到「producer_id」三個字：投影（operator/job_evidence.go
// :401-407）把 authority 與 evidence_role 硬編碼成 verifier_bearer /
// independent_verifier，kind 與顯示名從 verifiers 名冊查。被偽造的是**歸屬**
// ——operator 看到的那句「這個 verifier 獨立確認過」本身是假的。
// 同一支 validator 的 Authority／ProducerKind／ProvenanceRecorded 三條就沒有
// 這個性質：偽造它們之後投影出來的字一模一樣，測了是測鄰居丟掉的欄位。
func TestJobEvidenceRefusesIndependentEvidenceCreditedToAVerifierThatDidNotProduceIt(t *testing.T) {
	s := newDeployTestStore(t)
	registerDeployMachine(t, s, "machine-a")
	registerDeployMachine(t, s, "peer-machine")
	jobID := newJobForDeployTest(t, s, "machine-a")
	setJobDigest(t, s, jobID, verifierTestDigest)
	verifier, _, err := s.RegisterVerifier(
		VerifierKindFleetPeerAgent, "peer-verifier", "peer-machine", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RecordIndependentVerification(independentRequest(verifier.VerifierID, jobID)); err != nil {
		t.Fatal(err)
	}

	evidence, err := s.JobReadEvidence(jobID, 10)
	if err != nil || len(evidence.Independent) != 1 ||
		evidence.Independent[0].ProducerID != verifier.VerifierID ||
		evidence.Independent[0].VerifierID != verifier.VerifierID {
		t.Fatalf("沒有一份產品寫入路徑寫出來、producer 與掛名 verifier 一致的證據，"+
			"下面的拒絕就無法歸因於被改掉的 producer: evidence=%+v err=%v", evidence, err)
	}

	result, err := s.DB().Exec(
		`UPDATE verification_results SET producer_id=? WHERE job_id=? AND evidence_role=?`,
		"machine-a", jobID, JobVerificationRoleIndependent)
	if err != nil {
		t.Fatal(err)
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		t.Fatal(err)
	}
	if rowsAffected != 1 {
		t.Fatalf("篡改 producer_id 改到 %d 列，期望 1 列", rowsAffected)
	}

	_, err = s.JobReadEvidence(jobID, 10)
	if !errors.Is(err, ErrJobReadCorrupt) {
		t.Errorf("JobReadEvidence error=%v，期望 ErrJobReadCorrupt；那一列會進工作單的跨故障域證據段，"+
			"被投影成 %s 獨立確認過這張單，而帳本說寫它的是受測機器自己 —— "+
			"機器替自己作保會被讀成第二個判斷", err, verifier.VerifierID)
	}
}

// JobReadEvidence is the production reader behind Web/CLI/API independent
// verdicts. A pre-terminal independent failure plus a later Hub-received pass
// must read as passed here, matching the promotion gate. Mutating LiveFailed
// freshness is uniquely visible on this path: EvaluateIndependentVerdict is
// not what JobReadEvidence calls.
func TestJobReadEvidenceStaleFailureDoesNotLockFreshPass(t *testing.T) {
	terminalAt := deployTestNow
	tests := []struct {
		name   string
		failAt time.Time
		passAt time.Time
		want   IndependentVerdict
	}{
		{
			name:   "before terminal then fresh pass",
			failAt: terminalAt.Add(-10 * time.Minute),
			passAt: terminalAt.Add(time.Minute),
			want:   IndependentPassed,
		},
		{
			name:   "same second as terminal then fresh pass",
			failAt: terminalAt,
			passAt: terminalAt.Add(time.Second),
			want:   IndependentPassed,
		},
		{
			name:   "fresh failure still locks",
			failAt: terminalAt.Add(time.Second),
			passAt: terminalAt.Add(2 * time.Second),
			want:   IndependentFailed,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s := newDeployTestStore(t)
			clock := deployTestNow.Add(-time.Hour)
			s.nowFn = func() time.Time { return clock }
			registerDeployMachine(t, s, "machine-a")
			registerDeployMachine(t, s, "peer-machine")
			jobID := newJobForDeployTestOnMachine(t, s, "machine-a")
			setJobDigest(t, s, jobID, verifierTestDigest)
			if _, err := s.DB().Exec(`INSERT INTO verification_results
 (verification_id,job_id,machine_id,rule_id,command,exit_code,passed,verified_at)
 VALUES (?,?,?,?,?,?,?,?)`, "executor-pass", jobID, "machine-a", "health", "true", 0, true,
				fmtTime(terminalAt)); err != nil {
				t.Fatal(err)
			}
			if _, err := s.DB().Exec(`UPDATE jobs SET state=?,terminal_at=? WHERE job_id=?`,
				deploy.Succeeded, fmtTime(terminalAt), jobID); err != nil {
				t.Fatal(err)
			}
			verifier, _, err := s.RegisterVerifier(
				VerifierKindFleetPeerAgent, "peer-verifier-"+test.name, "peer-machine", "")
			if err != nil {
				t.Fatal(err)
			}

			clock = test.failAt
			fail := independentRequest(verifier.VerifierID, jobID)
			fail.Passed = false
			fail.VerifiedAt = test.failAt
			if err := s.RecordIndependentVerification(fail); err != nil {
				t.Fatal(err)
			}
			clock = test.passAt
			pass := independentRequest(verifier.VerifierID, jobID)
			pass.VerifiedAt = test.passAt
			if err := s.RecordIndependentVerification(pass); err != nil {
				t.Fatal(err)
			}

			evidence, err := s.JobReadEvidence(jobID, 10)
			if err != nil {
				t.Fatal(err)
			}
			if evidence.IndependentVerdict != test.want || evidence.IndependentLiveProducers != 1 ||
				len(evidence.Independent) != 2 {
				t.Fatalf("verdict=%q live=%d rows=%d want %q", evidence.IndependentVerdict,
					evidence.IndependentLiveProducers, len(evidence.Independent), test.want)
			}
		})
	}
}

// 這一列是串聯拒絕的產品路徑親手寫的（deploy.go:795-801），除了 producer_id
// 以外每一欄都沒動，所以拒絕只能歸因於 :675 的 ProducerID == "hub"。
// hub 臂另外三條都還成立：provenance_recorded 仍是 1、evidence_role 仍是
// scheduler、authority 仍是 dependency_graph。呼叫端 :654-657 不看 producer_id。
//
// ⚠ 跟 validJobVerificationProducer 那一刀相反：event 的投影**不**硬編碼。
// projectJobEventProducer（operator/job_evidence.go:466-476）把帳本的
// kind／producer_id／role／authority／provenance 原樣送出，而且 :468 只要
// kind 是 hub_scheduler 就把 DisplayName 換成 "Hub"。工作單頁事件表
// （web/templates/job.html:59）那一欄印的就是 DisplayName。所以這裡每一條子句
// 的偽造都看得見——不像 verification 那邊，authority 與 role 被投影蓋掉。
//
// ⚠ deploy_test.go:771-777 也斷言過這幾個欄位，但它走 JobEvents()
// （deploy.go 的讀法），**不經過** validJobEventProducer。那支守的是寫入端，
// 不是讀側 fail-closed；不能拿來當這一刀的隔離證據。
func TestJobEvidenceRefusesASchedulerEventTheHubDidNotWrite(t *testing.T) {
	s := newDeployTestStore(t)
	registerDeployMachine(t, s, "machine-a")
	rootDesired, rootRev := desiredForDeployTest(t, s)
	childDesired, childRev := desiredForDeployTest(t, s)
	root, err := s.CreateJob("machine-a", rootDesired, rootRev, NewJob{})
	if err != nil {
		t.Fatal(err)
	}
	child, err := s.CreateJob("machine-a", childDesired, childRev,
		NewJob{PrerequisiteJobIDs: []string{root}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(`UPDATE jobs SET state=?,terminal_at=? WHERE job_id=?`,
		deploy.Failed, fmtTime(deployTestNow), root); err != nil {
		t.Fatal(err)
	}
	if job, ok, err := s.NextJobForMachine("machine-a"); err != nil || ok {
		t.Fatalf("next=%+v ok=%t err=%v want empty after cascade", job, ok, err)
	}

	evidence, err := s.JobReadEvidence(child, 10)
	if err != nil || len(evidence.Events) != 1 ||
		evidence.Events[0].ProducerKind != JobEventProducerHubScheduler ||
		evidence.Events[0].ProducerID != "hub" {
		t.Fatalf("沒有一列產品路徑寫出來、真的由 hub 具名的 scheduler 事件，"+
			"下面的拒絕就無法歸因於被改掉的 producer_id: evidence=%+v err=%v", evidence, err)
	}

	result, err := s.DB().Exec(
		`UPDATE job_events SET producer_id=? WHERE job_id=? AND producer_kind=?`,
		"machine-a", child, JobEventProducerHubScheduler)
	if err != nil {
		t.Fatal(err)
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		t.Fatal(err)
	}
	if rowsAffected != 1 {
		t.Fatalf("篡改 producer_id 改到 %d 列，期望 1 列", rowsAffected)
	}

	_, err = s.JobReadEvidence(child, 10)
	if !errors.Is(err, ErrJobReadCorrupt) {
		t.Errorf("JobReadEvidence error=%v，期望 ErrJobReadCorrupt；"+
			"那一列會留在工作單時間軸上、來源印成 Hub，而帳本說寫它的是這台機器自己；"+
			"operator 會把一則機器自述當成 Hub 的排程判決", err)
	}
}

// 只改了 verifiers 那一列的 kind，證據列一個字都沒動，所以拒絕只能歸因於
// validVerifierRow（`verifier.go:561`）的 validVerifierKind(v.Kind)。
// verification_results.producer_kind 仍然是 fleet_peer_agent，
// validJobVerificationProducer 那一支的 validVerifierKind 不會開火。
//
// ⚠ 葉子普查雖記成 `guarded validVerifierRow`，卻拿不出是哪支測試紅；同一焊法
// 在下刀前的測試集合重跑仍全綠，所以那一行是單點偽陽性。呼叫點普查則把
// verifier.go:562-567 的六個呼叫全報成 UNWALKED；這個結論成立：這個聚合器
// 原本是完全沒被行使過的零看守者，每一條都沒被走到。六個葉子裡雖有五個在
// RegisterVerifier 的登記驗證路徑有人守，也不等於 job_read.go:801 這個呼叫點
// 有人守。葉子普查報 guarded 卻列不出紅掉的測試名字時，那一行不可信。
//
// ⚠ 這是第一一七刀的名冊那一側。那一刀守的是「證據列的 producer 就是它掛名
// 的 verifier」；這一刀守的是「那個 verifier 真的是一種 verifier」。
// 兩條都不成立時 operator 看到的都是同一句「有第二個判斷」。
func TestJobEvidenceRefusesAProducerRowThatIsNotAVerifierKind(t *testing.T) {
	s := newDeployTestStore(t)
	registerDeployMachine(t, s, "machine-a")
	registerDeployMachine(t, s, "peer-machine")
	jobID := newJobForDeployTest(t, s, "machine-a")
	setJobDigest(t, s, jobID, verifierTestDigest)
	verifier, _, err := s.RegisterVerifier(
		VerifierKindFleetPeerAgent, "peer-verifier", "peer-machine", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RecordIndependentVerification(independentRequest(verifier.VerifierID, jobID)); err != nil {
		t.Fatal(err)
	}

	evidence, err := s.JobReadEvidence(jobID, 10)
	producer, found := evidence.Verifiers[verifier.VerifierID]
	if err != nil || len(evidence.Independent) != 1 || !found ||
		producer.Kind != VerifierKindFleetPeerAgent {
		t.Fatalf("沒有一份名冊查得到、kind 合法的 producer 列，"+
			"下面的拒絕就無法歸因於被改掉的 kind: evidence=%+v err=%v", evidence, err)
	}

	result, err := s.DB().Exec(`UPDATE verifiers SET kind=? WHERE verifier_id=?`,
		JobVerificationProducerExecutorAgent, verifier.VerifierID)
	if err != nil {
		t.Fatal(err)
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		t.Fatal(err)
	}
	if rowsAffected != 1 {
		t.Fatalf("篡改 kind 改到 %d 列，期望 1 列", rowsAffected)
	}

	_, err = s.JobReadEvidence(jobID, 10)
	if !errors.Is(err, ErrJobReadCorrupt) {
		t.Errorf("JobReadEvidence error=%v，期望 ErrJobReadCorrupt；"+
			"跨故障域證據段的 producer kind 是從名冊查出來的，所以那一段會顯示成"+
			"這張單被一個 executor_agent「獨立」確認過——而 executor_agent 正是"+
			"受測機器自己的 kind，等於把機器自述印成第二個判斷", err)
	}
}
