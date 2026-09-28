package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"

	"github.com/teddashh/AI-Intune/internal/deploy"
	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/operatorclient"
	"github.com/teddashh/AI-Intune/internal/store"
)

func TestCatalogCLIOutputInterruptionRetainsRecovery(t *testing.T) {
	t.Run("assignment", testCatalogAssignmentOutputRecovery)
	t.Run("publish", testCatalogPublishOutputRecovery)
}

func testCatalogAssignmentOutputRecovery(t *testing.T) {
	for _, arch := range []string{"arm64", "amd64"} {
		for _, format := range []string{"json", "text", "text-job-line"} {
			t.Run(arch+"/"+format, func(t *testing.T) {
				f, node, deps, traffic := catalogAssignmentRecoveryFixture(t, arch, 0)
				readDeps := deps
				path := filepath.Join(t.TempDir(), "private", "assignment.json")
				const key = "cli-output-interruption"
				args := []string{"assign", "--machine", f.machine.id, "--profile", "darwin-node@1",
					"--confirm-name", "catalog-cli-mac", "--reason", "approve pinned runtime",
					"--idempotency-key", key, "--recovery-file", path}
				var original []byte
				var committed operator.MachineProfileAssignmentResult
				var digest string
				for attempt := 0; attempt < 3; attempt++ {
					argv := append([]string(nil), args...)
					if format == "json" {
						argv = append(argv, "--json")
					}
					var out, errOut bytes.Buffer
					var output io.Writer = &out
					if attempt < 2 {
						// Accept a prefix, then let an actual closed pipe return EPIPE.
						writer := newCatalogInterruptedOutput(t, &out)
						if format == "text-job-line" {
							writer.fullWrites = 1 // summary delivered, job line interrupted
						}
						output = writer
					}
					err := runCatalogCommandWithDeps(t.Context(), argv, output, &errOut, deps)
					calls := traffic.snapshot()
					if len(calls) != attempt+1 {
						t.Fatalf("assignment POST count=%d want=%d", len(calls), attempt+1)
					}
					if attempt == 0 {
						if calls[0].status != 201 || json.Unmarshal([]byte(calls[0].response), &committed) != nil ||
							committed.Replayed || len(committed.Packages) != 1 {
							t.Fatal("initial assignment did not commit before output failed")
						}
					} else if calls[attempt].status != 200 || calls[attempt].request != calls[0].request ||
						calls[attempt].key != key || calls[attempt].path != calls[0].path {
						t.Fatal("output recovery did not replay the exact original HTTP request")
					}
					assertCatalogAssignmentRecoveryGraph(t, f, node, committed)
					if attempt < 2 {
						if !errors.Is(err, syscall.EPIPE) || out.Len() == 0 ||
							!strings.Contains(err.Error(), "操作已完成，結果輸出失敗") {
							t.Errorf("partial output must propagate broken pipe: bytes=%d err=%v", out.Len(), err)
						}
						if !strings.Contains(errOut.String(), "replay clawctl-hub catalog recover --recovery-file ") {
							t.Error("output interruption lost recovery instructions")
						}
						raw, readErr := os.ReadFile(path)
						if readErr != nil {
							t.Fatalf("output interruption removed recovery file: %v", readErr)
						}
						if attempt == 0 {
							original = raw
							document, decodeErr := decodeCatalogRecovery(raw)
							if decodeErr != nil || document.IdempotencyKey != key || document.Assignment == nil {
								t.Fatal("output interruption did not retain a canonical assignment receipt")
							}
							digest = document.RequestDigest
						} else if !bytes.Equal(raw, original) {
							t.Fatal("interrupted recovery changed its original receipt")
						}
					} else {
						if err != nil {
							t.Fatalf("final recovery: %v", err)
						}
						if format == "json" {
							var result operator.MachineProfileAssignmentResult
							want := committed
							want.Replayed = true
							if json.Unmarshal(out.Bytes(), &result) != nil || !reflect.DeepEqual(result, want) {
								t.Fatal("final JSON output changed the original committed assignment")
							}
						} else {
							pkg := committed.Packages[0]
							want := fmt.Sprintf("assigned darwin-node@1 to catalog-cli-mac jobs=1 replayed=true\njob %d node-runtime@%s %s\n",
								pkg.Position, node.Version, pkg.JobID)
							if out.String() != want {
								t.Fatalf("final recovery output=%q want=%q", out.String(), want)
							}
							jobID := strings.Fields(strings.Split(out.String(), "\n")[1])[3]
							assertCatalogRecoveredJobReadable(t, readDeps, committed.MachineID, pkg, jobID)
						}
						if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
							t.Fatalf("complete output did not clear recovery: %v", err)
						}
					}
					deps = machineCommandDeps{newOperatorClient: deps.newOperatorClient}
					args = []string{"recover", "--recovery-file", path}
				}
				if traffic.previewCount() != 1 {
					t.Fatal("output recovery repeated assignment preview")
				}
				assertCatalogAssignmentRecoveryAudit(t, f, key, digest, 2)
			})
		}
	}
}

func assertCatalogRecoveredJobReadable(t *testing.T, deps machineCommandDeps, machineID string, pkg store.OperatorMachineProfileAssignmentPackageResult, jobID string) {
	t.Helper()
	for _, action := range []string{"show", "evidence"} {
		var out, errOut bytes.Buffer
		if err := runJobReadCommandWithDeps(t.Context(), []string{action, "--json", jobID}, &out, &errOut, deps); err != nil {
			t.Fatalf("recovered job %s: %v", action, err)
		}
		if action == "show" {
			var result operator.JobDetailResult
			if err := json.Unmarshal(out.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			item := result.Item
			if item.JobID != pkg.JobID || item.MachineID != machineID || item.DesiredID != pkg.DesiredID ||
				item.Revision != pkg.Revision || item.ArtifactDigest == nil || *item.ArtifactDigest != pkg.ArtifactDigest ||
				item.State != deploy.NotStarted || item.TerminalAt != nil || item.VerificationTotal != 0 {
				t.Fatalf("recovered text job ID did not resolve to the original pinned, unexecuted job: %+v", item)
			}
		} else {
			var result operator.JobEvidenceResult
			if err := json.Unmarshal(out.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if result.JobID != pkg.JobID || result.MachineID != machineID ||
				result.Desired.DesiredID != pkg.DesiredID || result.Desired.Revision != pkg.Revision || result.State != deploy.NotStarted ||
				result.Events.Total != 0 || result.Verifications.Total != 0 || len(result.Verifications.Items) != 0 {
				t.Fatal("recovered text job ID returned evidence for a different or already executed job")
			}
		}
	}
}

func testCatalogPublishOutputRecovery(t *testing.T) {
	for _, action := range []string{"package", "profile"} {
		for _, format := range []string{"json", "text"} {
			t.Run(action+"/"+format, func(t *testing.T) {
				f, node, _, _, deps := catalogCLIFixture(t)
				path := filepath.Join(t.TempDir(), "private", "publish.json")
				const key = "cli-publish-output-interruption"
				identity, auditAction := "node-runtime@"+node.Version, store.AuditCatalogManifest
				args := []string{"package", "add", "--artifact", node.SHA256}
				if action == "profile" {
					runCatalogCLI(t, deps, "package", "add", "--artifact", node.SHA256,
						"--confirm", identity, "--reason", "approve runtime")
					args = []string{"profile", "publish", "--profile", "node-profile@1", "--package", identity}
					identity, auditAction = "node-profile@1", store.AuditMachineProfile
				}
				args = append(args, "--confirm", identity, "--reason", "approve pinned publication",
					"--idempotency-key", key, "--recovery-file", path)
				var original []byte
				var digest string
				for attempt := 0; attempt < 3; attempt++ {
					argv := append([]string(nil), args...)
					if format == "json" {
						argv = append(argv, "--json")
					}
					var out, errOut bytes.Buffer
					var output io.Writer = &out
					if attempt < 2 {
						output = newCatalogInterruptedOutput(t, &out)
					}
					err := runCatalogCommandWithDeps(t.Context(), argv, output, &errOut, deps)
					if attempt < 2 {
						if !errors.Is(err, syscall.EPIPE) || out.Len() == 0 ||
							!strings.Contains(err.Error(), "操作已完成，結果輸出失敗") {
							t.Errorf("publish output must propagate broken pipe: bytes=%d err=%v", out.Len(), err)
						}
						raw, readErr := os.ReadFile(path)
						if readErr != nil {
							t.Fatalf("publish output interruption removed recovery: %v", readErr)
						}
						if attempt == 0 {
							original = raw
							document, decodeErr := decodeCatalogRecovery(raw)
							if decodeErr != nil || document.IdempotencyKey != key || document.Action != "publish_"+action {
								t.Fatal("publish output did not retain a canonical receipt")
							}
							digest = document.RequestDigest
						} else if !bytes.Equal(raw, original) {
							t.Fatal("publish recovery changed the original receipt")
						}
					} else {
						if err != nil {
							t.Fatalf("publish recovery: %v", err)
						}
						if format == "json" && action == "package" {
							var result operator.CatalogManifestPublishResult
							if json.Unmarshal(out.Bytes(), &result) != nil || !result.Replayed ||
								result.Record.Manifest.ID != "node-runtime" || result.Record.Manifest.Version != node.Version ||
								result.Record.Manifest.Artifact.SHA256 != node.SHA256 {
								t.Fatal("package recovery changed the pinned publication")
							}
						} else if format == "json" {
							var result operator.MachineProfilePublishResult
							if json.Unmarshal(out.Bytes(), &result) != nil || !result.Replayed ||
								result.Record.Profile.ID != "node-profile" || result.Record.Profile.Revision != 1 ||
								len(result.Record.Profile.Packages) != 1 || result.Record.Profile.Packages[0].Version != node.Version {
								t.Fatal("profile recovery changed the pinned publication")
							}
						} else if !strings.HasPrefix(out.String(), "published "+identity+" digest=") ||
							!strings.HasSuffix(out.String(), " replayed=true\n") {
							t.Fatalf("publish recovery output=%q", out.String())
						}
						if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
							t.Fatalf("complete publication output did not clear recovery: %v", err)
						}
					}
					deps = machineCommandDeps{newOperatorClient: deps.newOperatorClient}
					args = []string{"recover", "--recovery-file", path}
				}
				entries, err := f.store.Audit("", 100)
				if err != nil {
					t.Fatal(err)
				}
				var originals, replays int
				for _, entry := range entries {
					if entry.IdempotencyKey != key {
						continue
					}
					if entry.Action != auditAction || !entry.OK || entry.RequestDigest != digest ||
						entry.SourceKind != operator.SourceKindOperatorAPI || entry.UserAgent != operatorclient.UserAgent {
						t.Error("publication recovery lost its request digest or audit outcome")
					}
					if entry.IsOperatorReplay() {
						replays++
					} else {
						originals++
					}
				}
				if originals != 1 || replays != 2 {
					t.Fatalf("publication audits original=%d replay=%d want 1/2", originals, replays)
				}
			})
		}
	}
}

type catalogInterruptedOutput struct {
	prefix     *bytes.Buffer
	pipe       *os.File
	fullWrites int
}

func newCatalogInterruptedOutput(t *testing.T, prefix *bytes.Buffer) *catalogInterruptedOutput {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = writer.Close() })
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	return &catalogInterruptedOutput{prefix: prefix, pipe: writer}
}

func (w *catalogInterruptedOutput) Write(p []byte) (int, error) {
	if w.fullWrites > 0 {
		w.fullWrites--
		return w.prefix.Write(p)
	}
	n := min(7, len(p))
	_, _ = w.prefix.Write(p[:n])
	written, err := w.pipe.Write(p[n:])
	return n + written, err
}
