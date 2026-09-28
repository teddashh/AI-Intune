package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"reflect"
	"testing"

	"github.com/teddashh/AI-Intune/internal/deploy"
	"github.com/teddashh/AI-Intune/internal/model"
	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/store"
)

func assertCatalogRecoveredDependencyRelease(t *testing.T, f jobsFixture, deps machineCommandDeps,
	assignment operator.MachineProfileAssignmentResult, lease model.JobLeaseResponse,
) model.JobLeaseResponse {
	t.Helper()
	node, app := assignment.Packages[0], assignment.Packages[1]
	desired, err := f.store.DesiredState(node.DesiredID)
	if err != nil {
		t.Fatal(err)
	}
	var spec model.NodeRuntimeSpec
	if json.Unmarshal([]byte(desired.Spec), &spec) != nil || spec.Artifact == nil ||
		spec.Version != node.PackageVersion || "sha256:"+spec.Artifact.SHA256 != node.ArtifactDigest {
		t.Fatal("recovered Node spec does not match the pinned assignment")
	}
	prefix := "/v1/jobs/" + node.JobID
	start := requestJobAPI(t, f.mux, http.MethodPost, prefix+"/events", f.machine.token,
		model.JobEventRequest{LeaseToken: lease.LeaseToken, Seq: 1, Phase: "start", OccurredAt: jobsTestNow})
	if start.Code != http.StatusAccepted {
		t.Fatalf("Node start: status=%d", start.Code)
	}
	complete := model.JobCompleteRequest{LeaseToken: lease.LeaseToken}
	missing := requestJobAPI(t, f.mux, http.MethodPost, prefix+"/complete", f.machine.token, complete)
	assertAPIError(t, missing, http.StatusConflict, model.ErrNoVerification)
	assertCatalogDependencyStillWaiting(t, f, node.JobID, app.JobID)

	// Fixture evidence uses the executor's artifact/Node/npm record shape.
	// No commands or packages are executed by this Hub protocol test.
	release := "/home/operator/.local/share/clawctl/node-runtime/releases/" + spec.Version
	requests := []model.JobVerificationRequest{
		{RuleID: "node-runtime-activate-artifact", Command: "cat " + release + "/.clawctl-artifact-sha256", StdoutExcerpt: node.ArtifactDigest + "\n"},
		{RuleID: "node-runtime-activate-node", Command: release + "/bin/node --version", StdoutExcerpt: "v" + spec.Version + "\n"},
		{RuleID: "node-runtime-activate-npm", Command: release + "/bin/node " + release + "/lib/node_modules/npm/bin/npm-cli.js --version", StdoutExcerpt: "11.7.0\n"},
	}
	for i := range requests {
		requests[i].LeaseToken, requests[i].Passed, requests[i].VerifiedAt = lease.LeaseToken, true, jobsTestNow
		rec := requestJobAPI(t, f.mux, http.MethodPost, prefix+"/verifications", f.machine.token, requests[i])
		if rec.Code != http.StatusCreated {
			t.Fatalf("Node verification %s: status=%d", requests[i].RuleID, rec.Code)
		}
	}
	assertCatalogDependencyStillWaiting(t, f, node.JobID, app.JobID)
	finish := requestJobAPI(t, f.mux, http.MethodPost, prefix+"/events", f.machine.token,
		model.JobEventRequest{LeaseToken: lease.LeaseToken, Seq: 2, Phase: "finish", OccurredAt: jobsTestNow})
	if finish.Code != http.StatusAccepted {
		t.Fatalf("Node finish: status=%d", finish.Code)
	}
	appDesired, err := f.store.DesiredState(app.DesiredID)
	if err != nil {
		t.Fatal(err)
	}
	for _, replay := range []bool{false, true} {
		rec := requestJobAPI(t, f.mux, http.MethodPost, prefix+"/complete", f.machine.token, complete)
		var result model.JobStateResponse
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &result) != nil ||
			result.State != string(deploy.Succeeded) || result.Replayed != replay {
			t.Fatalf("Node complete: status=%d state=%s replay=%t want=%t", rec.Code, result.State, result.Replayed, replay)
		}
		next := requestJobAPI(t, f.mux, http.MethodGet, "/v1/jobs/next", f.machine.token, nil)
		var job model.JobResponse
		if next.Code != http.StatusOK || json.Unmarshal(next.Body.Bytes(), &job) != nil ||
			job.JobID != app.JobID || job.MachineID != f.machine.id || job.DesiredID != app.DesiredID ||
			job.Revision != int64(app.Revision) || job.ArtifactDigest != app.ArtifactDigest ||
			job.ResourceKind != "openclaw" || job.ResourceID != "openclaw" || job.State != string(deploy.NotStarted) {
			t.Fatalf("successful Node did not release the original pinned OpenClaw job: status=%d job=%s", next.Code, job.JobID)
		}
		var gotSpec, wantSpec model.OpenClawSpec
		if json.Unmarshal(job.Spec, &gotSpec) != nil || json.Unmarshal([]byte(appDesired.Spec), &wantSpec) != nil ||
			!reflect.DeepEqual(gotSpec, wantSpec) || gotSpec.Version != app.PackageVersion || gotSpec.Artifact == nil ||
			"sha256:"+gotSpec.Artifact.SHA256 != app.ArtifactDigest {
			t.Fatal("released OpenClaw job changed the original spec or pin")
		}
	}
	assertCatalogTerminalJobEvidence(t, deps, f, node, spec, requests, deploy.Succeeded)
	assertCatalogRecoveredJobReadable(t, deps, f.machine.id, app, app.JobID)
	appLease := claimJobViaHTTP(t, f, app.JobID)
	appJob, err := f.store.JobForMachine(app.JobID, f.machine.id)
	if err != nil || appJob.State != deploy.Claimed {
		t.Fatal("original OpenClaw job could not be claimed after Node succeeded")
	}
	next := requestJobAPI(t, f.mux, http.MethodGet, "/v1/jobs/next", f.machine.token, nil)
	if next.Code != http.StatusNoContent {
		t.Fatalf("completion replay left an extra deliverable job: status=%d", next.Code)
	}
	return appLease
}

func assertCatalogDependencyStillWaiting(t *testing.T, f jobsFixture, nodeID, appID string) {
	t.Helper()
	node, err := f.store.JobForMachine(nodeID, f.machine.id)
	if err != nil || node.State != deploy.Verifying || node.TerminalAt != nil {
		t.Fatal("Node must await Hub success before its dependent can run")
	}
	next := requestJobAPI(t, f.mux, http.MethodGet, "/v1/jobs/next", f.machine.token, nil)
	if next.Code != http.StatusNoContent {
		t.Fatalf("dependent job delivered before Hub judged Node successful: status=%d", next.Code)
	}
	claim := requestJobAPI(t, f.mux, http.MethodPost, "/v1/jobs/"+appID+"/claims", f.machine.token, struct{}{})
	assertAPIError(t, claim, http.StatusConflict, model.ErrJobStateConflict)
	app, err := f.store.JobForMachine(appID, f.machine.id)
	if err != nil || app.State != deploy.NotStarted || app.LeaseToken != "" || app.LeaseExpiresAt != nil {
		t.Fatal("waiting OpenClaw job acquired a lease before Node succeeded")
	}
}

func assertCatalogTerminalJobEvidence(t *testing.T, deps machineCommandDeps, f jobsFixture,
	pkg store.OperatorMachineProfileAssignmentPackageResult, spec any, requests []model.JobVerificationRequest, state deploy.JobState,
) {
	t.Helper()
	passed := 0
	for _, request := range requests {
		if request.Passed {
			passed++
		}
	}
	failed := len(requests) - passed
	var out, errOut bytes.Buffer
	if err := runJobReadCommandWithDeps(t.Context(), []string{"show", "--json", pkg.JobID}, &out, &errOut, deps); err != nil {
		t.Fatalf("terminal job show: %v", err)
	}
	var detail operator.JobDetailResult
	if err := json.Unmarshal(out.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	job := detail.Item
	if job.JobID != pkg.JobID || job.MachineID != f.machine.id || job.DesiredID != pkg.DesiredID ||
		job.Revision != pkg.Revision || job.State != state || job.ArtifactDigest == nil ||
		*job.ArtifactDigest != pkg.ArtifactDigest || job.TerminalAt == nil || !job.TerminalAt.Equal(jobsTestNow) ||
		job.LeaseStatus != operator.JobLeaseNone || job.EventCount != 2 || job.VerificationTotal != len(requests) ||
		job.VerificationPassed != passed || job.VerificationFailed != failed {
		t.Fatal("job show changed the terminal job identity, pin, terminal state or evidence counts")
	}
	ledger, err := f.store.JobForMachine(pkg.JobID, f.machine.id)
	if err != nil || ledger.State != state || ledger.LeaseToken != "" || ledger.LeaseExpiresAt != nil {
		t.Fatal("terminal job retained a lease or changed terminal state")
	}
	out.Reset()
	errOut.Reset()
	if err := runJobReadCommandWithDeps(t.Context(), []string{"evidence", "--json", pkg.JobID}, &out, &errOut, deps); err != nil {
		t.Fatalf("terminal job evidence: %v", err)
	}
	var evidence operator.JobEvidenceResult
	if err := json.Unmarshal(out.Bytes(), &evidence); err != nil {
		t.Fatal(err)
	}
	if evidence.JobID != pkg.JobID || evidence.MachineID != f.machine.id || evidence.State != state ||
		evidence.Desired.DesiredID != pkg.DesiredID || evidence.Desired.Revision != pkg.Revision ||
		evidence.Events.Total != 2 || evidence.Verifications.Total != len(requests) ||
		evidence.Verifications.Passed != passed || evidence.Verifications.Failed != failed ||
		len(evidence.Verifications.Items) != len(requests) {
		t.Fatal("job evidence changed the terminal job identity or recorded totals")
	}
	wantJSON, err := json.Marshal(spec)
	var gotSpec, wantSpec any
	if err != nil || json.Unmarshal(wantJSON, &wantSpec) != nil ||
		json.Unmarshal([]byte(evidence.Desired.Spec.Text), &gotSpec) != nil || !reflect.DeepEqual(gotSpec, wantSpec) {
		t.Fatal("job evidence changed the original job spec")
	}
	rows, err := f.store.JobVerifications(pkg.JobID)
	if err != nil || len(rows) != len(requests) {
		t.Fatalf("job evidence ledger count=%d err=%v", len(rows), err)
	}
	stored := make(map[string]store.JobVerification, len(rows))
	for _, row := range rows {
		stored[row.RuleID] = row
	}
	wanted := make(map[string]model.JobVerificationRequest, len(requests))
	for _, request := range requests {
		wanted[request.RuleID] = request
	}
	for _, item := range evidence.Verifications.Items {
		request, ok := wanted[item.RuleID.Text]
		row := stored[item.RuleID.Text]
		if !ok || item.VerificationID != row.VerificationID || item.Command.Text != request.Command ||
			item.StdoutExcerpt.Text != request.StdoutExcerpt || item.StderrExcerpt.Text != request.StderrExcerpt ||
			item.ExitCode == nil || *item.ExitCode != request.ExitCode || item.Passed != request.Passed ||
			!item.ReportedVerifiedAt.Equal(request.VerifiedAt) || item.ReceivedAt == nil || row.ReceivedAt.IsZero() ||
			!item.ReceivedAt.Equal(row.ReceivedAt) || item.Producer.Kind != operator.JobEvidenceProducerExecutorAgent ||
			item.Producer.ProducerID != f.machine.id || item.Producer.EvidenceRole != operator.JobEvidenceRoleExecutor ||
			item.Producer.Authority != operator.JobEvidenceAuthorityMachineLease || !item.Producer.ProvenanceRecorded {
			t.Fatalf("job evidence changed executor record %s", item.RuleID.Text)
		}
		delete(wanted, item.RuleID.Text)
	}
	if len(wanted) != 0 {
		t.Fatal("job evidence omitted an executor record")
	}
}
