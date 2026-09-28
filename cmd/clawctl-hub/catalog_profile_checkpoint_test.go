package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/teddashh/AI-Intune/internal/deploy"
	"github.com/teddashh/AI-Intune/internal/model"
	"github.com/teddashh/AI-Intune/internal/operator"
)

func completeCatalogOpenClawFixture(t *testing.T, f jobsFixture, deps machineCommandDeps,
	assignment operator.MachineProfileAssignmentResult, lease model.JobLeaseResponse,
) {
	t.Helper()
	app := assignment.Packages[1]
	desired, err := f.store.DesiredState(app.DesiredID)
	if err != nil {
		t.Fatal(err)
	}
	var spec model.OpenClawSpec
	if json.Unmarshal([]byte(desired.Spec), &spec) != nil || spec.Version != app.PackageVersion ||
		spec.Artifact == nil || "sha256:"+spec.Artifact.SHA256 != app.ArtifactDigest {
		t.Fatal("OpenClaw completion fixture lost the pinned spec")
	}
	prefix := "/v1/jobs/" + app.JobID
	start := requestJobAPI(t, f.mux, http.MethodPost, prefix+"/events", f.machine.token,
		model.JobEventRequest{LeaseToken: lease.LeaseToken, Seq: 1, Phase: "start", OccurredAt: jobsTestNow})
	if start.Code != http.StatusAccepted {
		t.Fatalf("OpenClaw start: status=%d", start.Code)
	}
	verification := model.JobVerificationRequest{
		LeaseToken: lease.LeaseToken, RuleID: "openclaw-version", Command: "openclaw --version",
		StdoutExcerpt: "openclaw " + spec.Version + "\n", Passed: true, VerifiedAt: jobsTestNow,
	}
	verified := requestJobAPI(t, f.mux, http.MethodPost, prefix+"/verifications", f.machine.token, verification)
	if verified.Code != http.StatusCreated {
		t.Fatalf("OpenClaw fixture evidence: status=%d", verified.Code)
	}
	finish := requestJobAPI(t, f.mux, http.MethodPost, prefix+"/events", f.machine.token,
		model.JobEventRequest{LeaseToken: lease.LeaseToken, Seq: 2, Phase: "finish", OccurredAt: jobsTestNow})
	if finish.Code != http.StatusAccepted {
		t.Fatalf("OpenClaw finish: status=%d", finish.Code)
	}
	for _, replay := range []bool{false, true} {
		complete := requestJobAPI(t, f.mux, http.MethodPost, prefix+"/complete", f.machine.token,
			model.JobCompleteRequest{LeaseToken: lease.LeaseToken})
		var state model.JobStateResponse
		if complete.Code != http.StatusOK || json.Unmarshal(complete.Body.Bytes(), &state) != nil ||
			state.State != string(deploy.Succeeded) || state.Replayed != replay {
			t.Fatalf("OpenClaw complete: status=%d state=%s replay=%t", complete.Code, state.State, state.Replayed)
		}
	}
	assertCatalogTerminalJobEvidence(t, deps, f, app, spec, []model.JobVerificationRequest{verification}, deploy.Succeeded)
}

func retryCatalogFailedProfile(t *testing.T, f jobsFixture, deps machineCommandDeps,
	original operator.MachineProfileAssignmentResult, traffic *catalogAssignmentTraffic,
) operator.MachineProfileAssignmentResult {
	t.Helper()
	oldNode, err := f.store.JobForMachine(original.Packages[0].JobID, f.machine.id)
	if err != nil {
		t.Fatal(err)
	}
	oldEvidence, err := f.store.JobVerifications(oldNode.JobID)
	if err != nil {
		t.Fatal(err)
	}
	oldApp, err := f.store.JobForMachine(original.Packages[1].JobID, f.machine.id)
	if err != nil {
		t.Fatal(err)
	}
	oldRejection, err := f.store.JobEvents(oldApp.JobID)
	if err != nil {
		t.Fatal(err)
	}
	const key = "cli-multipackage-reassign"
	result := assignCatalogCheckpointProfile(t, f, deps, key)
	if result.AlreadyAssigned || result.Replayed || result.AssignmentID == original.AssignmentID ||
		result.AssignmentRevision != original.AssignmentRevision+1 || len(result.Packages) != 2 {
		t.Fatal("reassignment did not create a new attempt of the same profile")
	}
	assertCatalogCheckpointPins(t, original, result)
	for i, pkg := range result.Packages {
		old := original.Packages[i]
		if pkg.JobID == "" || pkg.DesiredID == "" || pkg.JobID == old.JobID || pkg.DesiredID == old.DesiredID ||
			pkg.Revision != old.Revision+1 {
			t.Fatal("reassignment reused an old failed job or changed revision ordering")
		}
	}
	nodeEdges, err := f.store.JobPrerequisites(result.Packages[0].JobID)
	if err != nil || len(nodeEdges) != 0 {
		t.Fatal("retried Node acquired an unexpected prerequisite")
	}
	appEdges, err := f.store.JobPrerequisites(result.Packages[1].JobID)
	if err != nil || len(appEdges) != 1 || appEdges[0].PrerequisiteJobID != result.Packages[0].JobID || appEdges[0].Position != 0 {
		t.Fatal("retried OpenClaw does not depend on the new Node job")
	}
	assertCatalogCheckpointAudit(t, f, traffic, key)
	lease := assertCatalogRecoveredDependencyDelivery(t, f, result)
	appLease := assertCatalogRecoveredDependencyRelease(t, f, deps, result, lease)
	completeCatalogOpenClawFixture(t, f, deps, result, appLease)
	for i, want := range []deploy.JobState{deploy.Failed, deploy.Rejected} {
		job, err := f.store.JobForMachine(original.Packages[i].JobID, f.machine.id)
		if err != nil || job.State != want {
			t.Fatal("successful retry changed an original terminal job")
		}
		if i == 0 && !reflect.DeepEqual(job, oldNode) || i == 1 && !reflect.DeepEqual(job, oldApp) {
			t.Fatal("successful retry rewrote an original terminal job")
		}
	}
	rows, err := f.store.JobVerifications(oldNode.JobID)
	if err != nil || !reflect.DeepEqual(rows, oldEvidence) {
		t.Fatal("successful retry changed original failure evidence")
	}
	events, err := f.store.JobEvents(oldApp.JobID)
	if err != nil || !reflect.DeepEqual(events, oldRejection) {
		t.Fatal("successful retry changed the original dependency rejection")
	}
	assertCatalogDependencyRejection(t, f, deps, original.Packages[1], oldNode.JobID)
	return result
}

func assignCatalogCheckpointProfile(t *testing.T, f jobsFixture, deps machineCommandDeps, key string) operator.MachineProfileAssignmentResult {
	t.Helper()
	path := filepath.Join(t.TempDir(), "private", "assignment.json")
	out, _ := runCatalogCLI(t, deps, "assign", "--machine", f.machine.id, "--profile", "pinned-app@1",
		"--confirm-name", "catalog-cli-mac", "--reason", "confirm pinned profile convergence",
		"--idempotency-key", key, "--recovery-file", path, "--json")
	var response struct {
		Result operator.MachineProfileAssignmentResult `json:"result"`
	}
	if err := json.Unmarshal([]byte(out), &response); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("successful checkpoint assignment retained recovery file: %v", err)
	}
	return response.Result
}

func assertCatalogCheckpointPins(t *testing.T, original, result operator.MachineProfileAssignmentResult) {
	t.Helper()
	if result.MachineID != original.MachineID || result.ProfileID != original.ProfileID ||
		result.ProfileRevision != original.ProfileRevision || result.ProfileDigest != original.ProfileDigest ||
		result.Target != original.Target || len(result.Packages) != len(original.Packages) {
		t.Fatal("profile retry changed its identity, platform or pin")
	}
	for i, pkg := range result.Packages {
		old := original.Packages[i]
		if pkg.Position != old.Position || pkg.PackageID != old.PackageID || pkg.PackageVersion != old.PackageVersion ||
			pkg.ManifestDigest != old.ManifestDigest || pkg.ArtifactDigest != old.ArtifactDigest || pkg.SpecDigest != old.SpecDigest ||
			pkg.Direct != old.Direct || !reflect.DeepEqual(pkg.PrerequisitePackages, old.PrerequisitePackages) {
			t.Fatal("profile retry changed a package pin or prerequisite ordering")
		}
	}
}

func assertCatalogSatisfiedProfile(t *testing.T, f jobsFixture, deps machineCommandDeps,
	completed operator.MachineProfileAssignmentResult, traffic *catalogAssignmentTraffic, attempts int,
) {
	t.Helper()
	const key = "cli-multipackage-satisfied"
	result := assignCatalogCheckpointProfile(t, f, deps, key)
	assertCatalogCheckpointPins(t, completed, result)
	if !result.AlreadyAssigned || result.Replayed || result.AssignmentID != completed.AssignmentID ||
		result.AssignmentRevision != completed.AssignmentRevision {
		t.Fatal("completed profile created another assignment")
	}
	for i, pkg := range result.Packages {
		if pkg.JobID != completed.Packages[i].JobID || pkg.DesiredID != completed.Packages[i].DesiredID ||
			pkg.Revision != completed.Packages[i].Revision {
			t.Fatal("completed profile created another job or desired revision")
		}
	}
	assertCatalogCheckpointAudit(t, f, traffic, key)
	var assignments, desired, jobs int
	if err := f.store.DB().QueryRow(`SELECT
 (SELECT COUNT(*) FROM machine_profile_assignments WHERE machine_id=?),
 (SELECT COUNT(*) FROM desired_state WHERE scope_type='machine' AND scope_id=?),
 (SELECT COUNT(*) FROM jobs WHERE machine_id=?)`, f.machine.id, f.machine.id, f.machine.id).
		Scan(&assignments, &desired, &jobs); err != nil || assignments != attempts || desired != 2*attempts || jobs != 2*attempts {
		t.Fatalf("converged graph assignments=%d desired=%d jobs=%d err=%v", assignments, desired, jobs, err)
	}
	next := requestJobAPI(t, f.mux, http.MethodGet, "/v1/jobs/next", f.machine.token, nil)
	if next.Code != http.StatusNoContent {
		t.Fatalf("converged profile left runnable work: status=%d", next.Code)
	}
}

func assertCatalogCheckpointAudit(t *testing.T, f jobsFixture, traffic *catalogAssignmentTraffic, key string) {
	t.Helper()
	calls := traffic.snapshot()
	call := calls[len(calls)-1]
	var request operator.MachineProfileAssignmentRequest
	if call.key != key || json.Unmarshal([]byte(call.request), &request) != nil {
		t.Fatal("checkpoint did not use the expected operator assignment request")
	}
	request.MachineID = f.machine.id
	assertCatalogAssignmentRecoveryAudit(t, f, key, operator.MachineProfileAssignmentSemanticDigest(request), 0)
}
