package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"

	appcatalog "github.com/teddashh/AI-Intune/internal/catalog"
	"github.com/teddashh/AI-Intune/internal/deploy"
	"github.com/teddashh/AI-Intune/internal/model"
	"github.com/teddashh/AI-Intune/internal/operator"
)

func TestCatalogCLIMultiPackageRecoveryPreservesDependencyJobs(t *testing.T) {
	for _, arch := range []string{"arm64", "amd64"} {
		for _, scenario := range []struct {
			name   string
			json   bool
			failed bool
		}{
			{name: "json", json: true}, {name: "text"},
			{name: "json-failure", json: true, failed: true}, {name: "text-failure", failed: true},
		} {
			t.Run(arch+"/"+scenario.name, func(t *testing.T) {
				// The current standard OpenClaw manifest supports Linux only.
				f, node, deps, traffic := catalogAssignmentRecoveryPlatformFixture(t, "linux", arch, 1)
				readDeps := deps
				app := writeJobTestArtifact(t, f.artifactsDir, "2026.9.2", "multipackage-recovery")
				runCatalogCLI(t, deps, "package", "add", "--artifact", app.SHA256,
					"--node-runtime-version", node.Version, "--confirm", "openclaw@"+app.Version, "--reason", "approve pinned app")
				runCatalogCLI(t, deps, "profile", "publish", "--profile", "pinned-app@1",
					"--package", "openclaw@"+app.Version, "--confirm", "pinned-app@1", "--reason", "approve pinned dependency profile")
				path := filepath.Join(t.TempDir(), "private", "assignment.json")
				const key = "cli-multipackage-recovery"
				args := []string{"assign", "--machine", f.machine.id, "--profile", "pinned-app@1",
					"--confirm-name", "catalog-cli-mac", "--reason", "approve pinned app and runtime",
					"--idempotency-key", key, "--recovery-file", path}
				var committed operator.MachineProfileAssignmentResult
				var original []byte
				var digest string
				for attempt := 0; attempt < 3; attempt++ {
					argv := append([]string(nil), args...)
					if scenario.json {
						argv = append(argv, "--json")
					}
					var out, errOut bytes.Buffer
					var output io.Writer = &out
					if attempt == 1 {
						writer := newCatalogInterruptedOutput(t, &out)
						if !scenario.json {
							writer.fullWrites = 2 // summary and Node delivered; OpenClaw interrupted
						}
						output = writer
					}
					err := runCatalogCommandWithDeps(t.Context(), argv, output, &errOut, deps)
					calls := traffic.snapshot()
					if len(calls) != attempt+1 {
						t.Fatalf("assignment HTTP calls=%d want=%d", len(calls), attempt+1)
					}
					call := calls[attempt]
					var response operator.MachineProfileAssignmentResult
					if decodeErr := json.Unmarshal([]byte(call.response), &response); decodeErr != nil {
						t.Fatal(decodeErr)
					}
					wantStatus := http.StatusOK
					if attempt == 0 {
						wantStatus = http.StatusCreated
						committed = response
						if committed.Replayed || committed.AlreadyAssigned || committed.ProfileID != "pinned-app" ||
							committed.ProfileRevision != 1 || committed.AssignmentRevision != 1 ||
							committed.Target != (appcatalog.Platform{OS: "linux", Arch: arch}) {
							t.Fatal("initial assignment did not preserve the requested profile and platform")
						}
					}
					want := committed
					want.Replayed = attempt > 0
					if call.status != wantStatus || call.key != key || call.request != calls[0].request ||
						call.path != "/v1/operator/machines/"+f.machine.id+"/profile-assignments" || !reflect.DeepEqual(response, want) {
						t.Fatal("recovery changed the original request or committed dependency jobs")
					}
					assertCatalogMultiPackageRecoveryGraph(t, f, node, app, committed)
					if attempt < 2 {
						if attempt == 0 && (err == nil || out.Len() != 0) {
							t.Fatalf("lost committed response must return an error without output: %v", err)
						}
						if attempt == 1 && (!errors.Is(err, syscall.EPIPE) || out.Len() == 0) {
							t.Fatalf("interrupted recovery must retain the remaining job output: %v", err)
						}
						if !strings.Contains(errOut.String(), "replay clawctl-hub catalog recover --recovery-file ") {
							t.Fatal("failed output lost the recovery command")
						}
						raw, readErr := os.ReadFile(path)
						if readErr != nil {
							t.Fatal(readErr)
						}
						if attempt == 0 {
							original = raw
							document, decodeErr := decodeCatalogRecovery(raw)
							if decodeErr != nil || document.IdempotencyKey != key || document.Assignment == nil ||
								document.Assignment.ProfileID != "pinned-app" || document.Assignment.ProfileRevision != 1 {
								t.Fatal("lost response did not retain the pinned assignment receipt")
							}
							digest = document.RequestDigest
						} else if !bytes.Equal(raw, original) {
							t.Fatal("interrupted second job line changed the recovery receipt")
						}
					} else {
						if err != nil {
							t.Fatalf("final recovery: %v", err)
						}
						jobIDs := []string{committed.Packages[0].JobID, committed.Packages[1].JobID}
						if scenario.json {
							var result operator.MachineProfileAssignmentResult
							if json.Unmarshal(out.Bytes(), &result) != nil || !reflect.DeepEqual(result, want) {
								t.Fatal("JSON recovery lost a dependency job or changed its pin")
							}
						} else {
							wantText := fmt.Sprintf("assigned pinned-app@1 to catalog-cli-mac jobs=2 replayed=true\njob 0 node-runtime@%s %s\njob 1 openclaw@%s %s\n",
								node.Version, jobIDs[0], app.Version, jobIDs[1])
							if out.String() != wantText {
								t.Fatalf("recovered dependency order=%q want=%q", out.String(), wantText)
							}
							lines := strings.Split(out.String(), "\n")
							jobIDs = []string{strings.Fields(lines[1])[3], strings.Fields(lines[2])[3]}
						}
						for position, jobID := range jobIDs {
							assertCatalogRecoveredJobReadable(t, readDeps, committed.MachineID, committed.Packages[position], jobID)
						}
						if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
							t.Fatalf("complete two-job output did not clear recovery: %v", statErr)
						}
					}
					deps = machineCommandDeps{newOperatorClient: deps.newOperatorClient}
					args = []string{"recover", "--recovery-file", path}
				}
				if traffic.previewCount() != 1 {
					t.Fatal("multi-package recovery repeated assignment preview")
				}
				assertCatalogAssignmentRecoveryAudit(t, f, key, digest, 2)
				lease := assertCatalogRecoveredDependencyDelivery(t, f, committed)
				completed, attempts := committed, 1
				if scenario.failed {
					assertCatalogRecoveredDependencyFailure(t, f, readDeps, committed, lease)
					assertCatalogMultiPackageRecoveryGraph(t, f, node, app, committed)
					completed = retryCatalogFailedProfile(t, f, readDeps, committed, traffic)
					attempts = 2
				} else {
					appLease := assertCatalogRecoveredDependencyRelease(t, f, readDeps, committed, lease)
					completeCatalogOpenClawFixture(t, f, readDeps, committed, appLease)
					assertCatalogMultiPackageRecoveryGraph(t, f, node, app, committed)
				}
				assertCatalogSatisfiedProfile(t, f, readDeps, completed, traffic, attempts)
			})
		}
	}
}

func assertCatalogRecoveredDependencyDelivery(t *testing.T, f jobsFixture, result operator.MachineProfileAssignmentResult) model.JobLeaseResponse {
	t.Helper()
	nodeID, appID := result.Packages[0].JobID, result.Packages[1].JobID
	blocked := requestJobAPI(t, f.mux, http.MethodPost, "/v1/jobs/"+appID+"/claims", f.machine.token, struct{}{})
	assertAPIError(t, blocked, http.StatusConflict, model.ErrJobStateConflict)
	app, err := f.store.JobForMachine(appID, f.machine.id)
	if err != nil || app.State != deploy.NotStarted || app.LeaseToken != "" || app.LeaseExpiresAt != nil {
		t.Fatal("dependent job was claimed before the recovered Node prerequisite succeeded")
	}
	next := requestJobAPI(t, f.mux, http.MethodGet, "/v1/jobs/next", f.machine.token, nil)
	var job model.JobResponse
	if next.Code != http.StatusOK || json.Unmarshal(next.Body.Bytes(), &job) != nil || job.JobID != nodeID {
		t.Fatalf("first delivery after recovery did not select the original Node prerequisite: status=%d job=%s", next.Code, job.JobID)
	}
	lease := claimJobViaHTTP(t, f, nodeID)
	next = requestJobAPI(t, f.mux, http.MethodGet, "/v1/jobs/next", f.machine.token, nil)
	if next.Code != http.StatusNoContent {
		t.Fatalf("dependent job became deliverable while Node was only claimed: status=%d", next.Code)
	}
	return lease
}

func assertCatalogMultiPackageRecoveryGraph(t *testing.T, f jobsFixture, node, app artifactSidecar, result operator.MachineProfileAssignmentResult) {
	t.Helper()
	if result.MachineID != f.machine.id {
		t.Fatalf("MachineID: got %q want %q", result.MachineID, f.machine.id)
	}
	if len(result.Packages) != 2 {
		t.Fatalf("Packages: got %d want 2", len(result.Packages))
	}
	nodePkg, appPkg := result.Packages[0], result.Packages[1]
	if nodePkg.Position != 0 || nodePkg.PackageID != "node-runtime" || nodePkg.PackageVersion != node.Version ||
		nodePkg.ArtifactDigest != "sha256:"+node.SHA256 || nodePkg.Revision != 1 || nodePkg.Direct || len(nodePkg.PrerequisitePackages) != 0 {
		t.Fatalf("position 0: got %+v want node-runtime@%s sha256:%s revision 1 indirect no prereqs", nodePkg, node.Version, node.SHA256)
	}
	if appPkg.Position != 1 || appPkg.PackageID != "openclaw" || appPkg.PackageVersion != app.Version ||
		appPkg.ArtifactDigest != "sha256:"+app.SHA256 || appPkg.Revision != 1 || !appPkg.Direct ||
		!reflect.DeepEqual(appPkg.PrerequisitePackages, []string{"node-runtime@" + node.Version}) {
		t.Fatalf("position 1: got %+v want openclaw@%s sha256:%s revision 1 direct prereqs [node-runtime@%s]", appPkg, app.Version, app.SHA256, node.Version)
	}
	if nodePkg.JobID == "" || appPkg.JobID == "" || nodePkg.JobID == appPkg.JobID {
		t.Fatalf("JobID: got %q, %q want nonempty distinct", nodePkg.JobID, appPkg.JobID)
	}
	if nodePkg.DesiredID == "" || appPkg.DesiredID == "" || nodePkg.DesiredID == appPkg.DesiredID {
		t.Fatalf("DesiredID: got %q, %q want nonempty distinct", nodePkg.DesiredID, appPkg.DesiredID)
	}
	for _, pkg := range result.Packages {
		job, err := f.store.JobForMachine(pkg.JobID, f.machine.id)
		if err != nil {
			t.Fatalf("JobForMachine(%q, %q): %v", pkg.JobID, f.machine.id, err)
		}
		if job.DesiredID != pkg.DesiredID || job.ArtifactDigest != pkg.ArtifactDigest || job.Revision != pkg.Revision {
			t.Fatalf("job %s no longer matches its original desired state, artifact or revision", pkg.JobID)
		}
	}
	nodeEdges, err := f.store.JobPrerequisites(nodePkg.JobID)
	if err != nil {
		t.Fatalf("JobPrerequisites(%q): %v", nodePkg.JobID, err)
	}
	if len(nodeEdges) != 0 {
		t.Fatalf("node-runtime edges: got %+v want none", nodeEdges)
	}
	appEdges, err := f.store.JobPrerequisites(appPkg.JobID)
	if err != nil {
		t.Fatalf("JobPrerequisites(%q): %v", appPkg.JobID, err)
	}
	if len(appEdges) != 1 || appEdges[0].JobID != appPkg.JobID ||
		appEdges[0].PrerequisiteJobID != nodePkg.JobID || appEdges[0].Position != 0 {
		t.Fatalf("openclaw edges: got %+v want [{JobID:%s PrerequisiteJobID:%s Position:0}]", appEdges, appPkg.JobID, nodePkg.JobID)
	}
	var assignments, desired, jobs int
	if err := f.store.DB().QueryRow(`SELECT
 (SELECT COUNT(*) FROM machine_profile_assignments WHERE machine_id=?),
 (SELECT COUNT(*) FROM desired_state WHERE scope_type='machine' AND scope_id=?),
 (SELECT COUNT(*) FROM jobs WHERE machine_id=?)`, f.machine.id, f.machine.id, f.machine.id).
		Scan(&assignments, &desired, &jobs); err != nil {
		t.Fatalf("counts: %v", err)
	}
	if assignments != 1 || desired != 2 || jobs != 2 {
		t.Fatalf("counts: got assignments=%d desired=%d jobs=%d want 1, 2, 2", assignments, desired, jobs)
	}
}
