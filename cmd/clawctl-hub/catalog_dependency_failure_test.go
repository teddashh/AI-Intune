package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/teddashh/AI-Intune/internal/deploy"
	"github.com/teddashh/AI-Intune/internal/model"
	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/store"
)

func assertCatalogRecoveredDependencyFailure(t *testing.T, f jobsFixture, deps machineCommandDeps,
	assignment operator.MachineProfileAssignmentResult, lease model.JobLeaseResponse,
) {
	t.Helper()
	node, app := assignment.Packages[0], assignment.Packages[1]
	desired, err := f.store.DesiredState(node.DesiredID)
	if err != nil {
		t.Fatal(err)
	}
	var spec model.NodeRuntimeSpec
	if json.Unmarshal([]byte(desired.Spec), &spec) != nil || spec.Version != node.PackageVersion {
		t.Fatal("failed Node fixture lost its pinned spec")
	}
	prefix := "/v1/jobs/" + node.JobID
	start := requestJobAPI(t, f.mux, http.MethodPost, prefix+"/events", f.machine.token,
		model.JobEventRequest{LeaseToken: lease.LeaseToken, Seq: 1, Phase: "start", OccurredAt: jobsTestNow})
	if start.Code != http.StatusAccepted {
		t.Fatalf("failed Node start: status=%d", start.Code)
	}
	release := "/home/operator/.local/share/clawctl/node-runtime/releases/" + spec.Version
	failed := model.JobVerificationRequest{
		LeaseToken: lease.LeaseToken, RuleID: "node-runtime-activate-npm",
		Command:  release + "/bin/node " + release + "/lib/node_modules/npm/bin/npm-cli.js --version",
		ExitCode: 1, StdoutExcerpt: "invalid npm version\n", StderrExcerpt: "fixture npm failure", VerifiedAt: jobsTestNow,
	}
	verification := requestJobAPI(t, f.mux, http.MethodPost, prefix+"/verifications", f.machine.token, failed)
	if verification.Code != http.StatusCreated {
		t.Fatalf("failed Node evidence: status=%d", verification.Code)
	}
	finish := requestJobAPI(t, f.mux, http.MethodPost, prefix+"/events", f.machine.token,
		model.JobEventRequest{LeaseToken: lease.LeaseToken, Seq: 2, Phase: "finish", OccurredAt: jobsTestNow})
	if finish.Code != http.StatusAccepted {
		t.Fatalf("failed Node finish: status=%d", finish.Code)
	}
	for _, replay := range []bool{false, true} {
		complete := requestJobAPI(t, f.mux, http.MethodPost, prefix+"/complete", f.machine.token,
			model.JobCompleteRequest{LeaseToken: lease.LeaseToken})
		var result model.JobStateResponse
		if complete.Code != http.StatusOK || json.Unmarshal(complete.Body.Bytes(), &result) != nil ||
			result.State != string(deploy.Failed) || result.Replayed != replay {
			t.Fatalf("failed Node complete: status=%d state=%s replay=%t", complete.Code, result.State, result.Replayed)
		}
		next := requestJobAPI(t, f.mux, http.MethodGet, "/v1/jobs/next", f.machine.token, nil)
		if next.Code != http.StatusNoContent {
			t.Fatalf("failed dependency left a deliverable job: status=%d", next.Code)
		}
	}
	claim := requestJobAPI(t, f.mux, http.MethodPost, "/v1/jobs/"+app.JobID+"/claims", f.machine.token, struct{}{})
	var rejected model.JobStateResponse
	if claim.Code != http.StatusOK || json.Unmarshal(claim.Body.Bytes(), &rejected) != nil ||
		rejected.State != string(deploy.Rejected) || !rejected.Replayed {
		t.Fatal("claim of dependency-rejected job did not replay its terminal result")
	}
	assertCatalogTerminalJobEvidence(t, deps, f, node, spec, []model.JobVerificationRequest{failed}, deploy.Failed)
	assertCatalogDependencyRejection(t, f, deps, app, node.JobID)
}

func assertCatalogDependencyRejection(t *testing.T, f jobsFixture, deps machineCommandDeps, pkg store.OperatorMachineProfileAssignmentPackageResult, parentID string) {
	t.Helper()

	job, err := f.store.JobForMachine(pkg.JobID, f.machine.id)
	if err != nil || job.State != deploy.Rejected || job.TerminalAt == nil || job.LeaseToken != "" || job.LeaseExpiresAt != nil {
		t.Fatalf("rejected terminal job without lease: err=%v state=%s terminal=%t leased=%t", err, job.State, job.TerminalAt != nil, job.LeaseToken != "" || job.LeaseExpiresAt != nil)
	}

	events, err := f.store.JobEvents(pkg.JobID)
	if err != nil || len(events) != 1 || events[0].Seq != 1 || events[0].Phase != string(deploy.Rejected) ||
		events[0].ProducerKind != operator.JobEvidenceProducerHubScheduler || events[0].ProducerID != "hub" ||
		events[0].EvidenceRole != operator.JobEvidenceRoleScheduler || events[0].Authority != operator.JobEvidenceAuthorityDependencyGraph ||
		!events[0].ProvenanceRecorded {
		t.Fatalf("exactly one hub-scheduler rejected seq=1 event: err=%v count=%d", err, len(events))
	}
	ev := events[0]
	detail := fmt.Sprintf("prerequisite job %s ended in failed", parentID)

	action := "show"
	var out, errOut bytes.Buffer
	if err := runJobReadCommandWithDeps(t.Context(), []string{action, "--json", pkg.JobID}, &out, &errOut, deps); err != nil {
		t.Fatalf("show json: %v", err)
	}
	var shown operator.JobDetailResult
	if err := json.Unmarshal(out.Bytes(), &shown); err != nil {
		t.Fatalf("show json decode: %v", err)
	}
	item := shown.Item
	digestMismatch := item.ArtifactDigest == nil || *item.ArtifactDigest != pkg.ArtifactDigest
	if item.JobID != pkg.JobID || item.MachineID != f.machine.id || item.DesiredID != pkg.DesiredID || item.Revision != pkg.Revision || digestMismatch ||
		item.State != deploy.Rejected || item.TerminalAt == nil || !item.TerminalAt.Equal(*job.TerminalAt) ||
		item.LeaseStatus != operator.JobLeaseNone || item.EventCount != 1 || item.VerificationTotal != 0 {
		t.Fatalf("show json identity/state/lease/counts mismatch job=%s", pkg.JobID)
	}

	action = "evidence"
	out.Reset()
	errOut.Reset()
	if err := runJobReadCommandWithDeps(t.Context(), []string{action, "--json", pkg.JobID}, &out, &errOut, deps); err != nil {
		t.Fatalf("evidence json: %v", err)
	}
	var evidence operator.JobEvidenceResult
	if err := json.Unmarshal(out.Bytes(), &evidence); err != nil {
		t.Fatalf("evidence json decode: %v", err)
	}
	rej := evidence.Rejection
	if evidence.JobID != pkg.JobID || evidence.MachineID != f.machine.id || evidence.State != deploy.Rejected ||
		evidence.Desired.DesiredID != pkg.DesiredID || evidence.Desired.Revision != pkg.Revision ||
		evidence.Events.Total != 1 || len(evidence.Events.Items) != 1 ||
		evidence.Verifications.Total != 0 || len(evidence.Verifications.Items) != 0 ||
		rej == nil || rej.EventID != ev.EventID || rej.Seq != ev.Seq ||
		!rej.OccurredAt.Equal(ev.OccurredAt) || !rej.ReceivedAt.Equal(ev.ReceivedAt) ||
		!rej.PayloadDecodable || !rej.CodeKnown || !rej.HasDetail ||
		rej.Code.Text != string(deploy.DependencyFailed) || rej.Detail.Text != detail ||
		rej.Producer.Kind != operator.JobEvidenceProducerHubScheduler || rej.Producer.ProducerID != "hub" ||
		rej.Producer.EvidenceRole != operator.JobEvidenceRoleScheduler || rej.Producer.Authority != operator.JobEvidenceAuthorityDependencyGraph ||
		!rej.Producer.ProvenanceRecorded || evidence.Events.Items[0].Producer != rej.Producer {
		t.Fatalf("evidence json rejection mismatch job=%s", pkg.JobID)
	}

	for _, action := range []string{"show", "evidence"} {
		out.Reset()
		errOut.Reset()
		if err := runJobReadCommandWithDeps(t.Context(), []string{action, pkg.JobID}, &out, &errOut, deps); err != nil {
			t.Fatalf("%s text: %v", action, err)
		}
		text := out.String()
		if !strings.Contains(text, string(deploy.Rejected)) || !strings.Contains(text, pkg.JobID) {
			t.Fatalf("%s text missing rejected state or job id", action)
		}
		if action == "evidence" &&
			(!strings.Contains(text, "code="+terminalSafe(string(deploy.DependencyFailed))) ||
				!strings.Contains(text, "rejection detail: "+terminalSafe(detail)) ||
				!strings.Contains(text, `producer="hub_scheduler"/"hub"；role="scheduler"；authority="dependency_graph"；provenance_recorded=true`)) {
			t.Fatalf("evidence text missing rejection cause or scheduler attribution")
		}
	}
}
