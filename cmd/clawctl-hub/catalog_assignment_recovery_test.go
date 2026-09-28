package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	appcatalog "github.com/teddashh/AI-Intune/internal/catalog"
	"github.com/teddashh/AI-Intune/internal/model"
	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/operatorauth"
	"github.com/teddashh/AI-Intune/internal/operatorclient"
	"github.com/teddashh/AI-Intune/internal/store"
)

// Runs the CLI command dispatcher, recovery files, official client and Hub HTTP
// boundary together. Darwin identity/artifacts are fixtures; no Mac is executed.
func TestCatalogCLIAssignmentRecoveryReplaysCommittedDarwinJob(t *testing.T) {
	for _, arch := range []string{"arm64", "amd64"} {
		for _, lost := range []int{1, 2} {
			t.Run(fmt.Sprintf("%s/lost-responses=%d", arch, lost), func(t *testing.T) {
				f, node, deps, traffic := catalogAssignmentRecoveryFixture(t, arch, lost)
				path := filepath.Join(t.TempDir(), "private", "assignment.json")
				const key, reason = "cli-darwin-assignment", "approve exact Node runtime"
				args := []string{"assign", "--machine", f.machine.id, "--profile", "darwin-node@1",
					"--confirm-name", "catalog-cli-mac", "--reason", reason, "--idempotency-key", key,
					"--recovery-file", path, "--json"}
				var originalFile []byte
				var originalResult operator.MachineProfileAssignmentResult
				var requestDigest string
				for attempt := 0; attempt <= lost; attempt++ {
					var out, errOut bytes.Buffer
					err := runCatalogCommandWithDeps(t.Context(), args, &out, &errOut, deps)
					calls := traffic.snapshot()
					if len(calls) != attempt+1 {
						t.Fatalf("assignment HTTP calls=%d want=%d", len(calls), attempt+1)
					}
					call := calls[attempt]
					wantStatus := http.StatusOK
					if attempt == 0 {
						wantStatus = http.StatusCreated
						if err := json.Unmarshal([]byte(call.response), &originalResult); err != nil {
							t.Fatal(err)
						}
						if originalResult.Replayed || originalResult.AlreadyAssigned || originalResult.AssignmentID == "" ||
							originalResult.AssignmentRevision != 1 || originalResult.MachineID != f.machine.id ||
							originalResult.ProfileID != "darwin-node" || originalResult.ProfileRevision != 1 ||
							originalResult.Target != (appcatalog.Platform{OS: "darwin", Arch: arch}) || len(originalResult.Packages) != 1 {
							t.Fatal("initial committed assignment does not match the pinned Mac profile")
						}
					}
					if call.status != wantStatus || call.key != key || call.request != calls[0].request ||
						call.path != "/v1/operator/machines/"+f.machine.id+"/profile-assignments" {
						t.Fatalf("HTTP replay changed request/key or status: attempt=%d status=%d", attempt, call.status)
					}
					assertCatalogAssignmentRecoveryGraph(t, f, node, originalResult)
					if attempt < lost {
						var apiErr *operatorclient.APIError
						if err == nil || errors.As(err, &apiErr) || out.Len() != 0 ||
							!strings.Contains(errOut.String(), "replay clawctl-hub catalog recover --recovery-file ") {
							// Do not print the saved request or receipt contents.
							t.Fatalf("lost response must retain recovery instructions and return transport error: %v", err)
						}
						info, statErr := os.Stat(path)
						if statErr != nil || info.Mode().Perm() != 0o600 {
							t.Fatalf("private recovery file: %v", statErr)
						}
						parent, statErr := os.Stat(filepath.Dir(path))
						if statErr != nil || parent.Mode().Perm() != 0o700 {
							t.Fatalf("private recovery directory: %v", statErr)
						}
						got, readErr := os.ReadFile(path)
						if readErr != nil {
							t.Fatal(readErr)
						}
						if attempt == 0 {
							originalFile = got
							document, decodeErr := decodeCatalogRecovery(got)
							var request operator.MachineProfileAssignmentRequest
							if decodeErr != nil || json.Unmarshal([]byte(call.request), &request) != nil {
								t.Fatal("assignment recovery request is not canonical")
							}
							// The HTTP contract carries machine ID in the URL, not JSON.
							request.MachineID = f.machine.id
							requestDigest = operator.MachineProfileAssignmentSemanticDigest(request)
							if request.ProfileID != "darwin-node" || request.ProfileRevision != 1 ||
								request.ConfirmDisplayName != "catalog-cli-mac" || request.Reason != reason || request.PreviewDigest != originalResult.PreviewDigest ||
								!reflect.DeepEqual(document, newCatalogAssignmentRecovery("http://"+testOperatorAuthority, key, request)) {
								t.Fatal("recovery file does not preserve the exact assignment request/key/authority/digests")
							}
						} else if !bytes.Equal(got, originalFile) {
							t.Fatal("another lost response changed the recovery file")
						}
					} else {
						if err != nil {
							t.Fatalf("catalog recover: %v", err)
						}
						var result operator.MachineProfileAssignmentResult
						want := originalResult
						want.Replayed = true
						if json.Unmarshal(out.Bytes(), &result) != nil || !reflect.DeepEqual(result, want) {
							t.Fatal("CLI recovery output differs from the original committed assignment")
						}
						if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
							t.Fatalf("successful recovery must remove its local receipt: %v", err)
						}
					}
					// A new invocation receives only the path. Discovery must not be
					// consulted, and recovery must not regenerate the now-stale preview.
					deps = machineCommandDeps{
						newOperatorClient: deps.newOperatorClient,
						discoverHubURL:    func() (string, error) { return "", errors.New("unexpected recovery discovery") },
					}
					args = []string{"recover", "--recovery-file", path, "--json"}
				}
				if got := traffic.previewCount(); got != 1 {
					t.Fatalf("assignment previews=%d want=1 across assign and recovery", got)
				}
				assertCatalogAssignmentRecoveryAudit(t, f, key, requestDigest, lost)
			})
		}
	}
}

func catalogAssignmentRecoveryFixture(t *testing.T, arch string, lost int) (jobsFixture, artifactSidecar, machineCommandDeps, *catalogAssignmentTraffic) {
	t.Helper()
	return catalogAssignmentRecoveryPlatformFixture(t, "darwin", arch, lost)
}

func catalogAssignmentRecoveryPlatformFixture(t *testing.T, goos, arch string, lost int) (jobsFixture, artifactSidecar, machineCommandDeps, *catalogAssignmentTraffic) {
	t.Helper()
	f := newJobsFixture(t, "catalog-cli-mac")
	operatorMux := http.NewServeMux()
	(&hub{store: f.store, artifactsDir: f.artifactsDir}).operatorRoutes(operatorMux)
	authorizer := boundaryAuthorizeFunc(func(r *http.Request, permission operatorauth.Permission) (*http.Request, operatorauth.Decision) {
		principal := boundaryPrincipal(permission)
		principal.SourceAddr = r.RemoteAddr
		return operatorauth.WithPrincipal(r, principal), operatorauth.Decision{
			Allowed: true, HTTPStatus: http.StatusOK, Code: operatorauth.Authorized, Principal: principal,
		}
	})
	traffic := &catalogAssignmentTraffic{drop: lost}
	server := httptest.NewServer(traffic.handler(newOperatorBoundary(operatorMux, authorizer,
		f.store, operatorRoutePolicies, testOperatorAuthority)))
	t.Cleanup(server.Close)
	base := "http://" + testOperatorAuthority
	deps := machineCommandDeps{
		discoverHubURL: func() (string, error) { return base, nil },
		newOperatorClient: func(url string) (*operatorclient.Client, error) {
			if url != base {
				return nil, errors.New("unexpected operator recovery authority")
			}
			transport := http.DefaultTransport.(*http.Transport).Clone()
			transport.Proxy = nil
			// Prevent Go from transparently retrying idempotent POSTs over
			// reused connections; the CLI must actually observe the lost reply.
			transport.DisableKeepAlives = true
			transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
				if address != testOperatorAuthority {
					return nil, errors.New("unexpected operator recovery destination")
				}
				return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
			}
			t.Cleanup(transport.CloseIdleConnections)
			return operatorclient.NewWithHTTPClient(base, &http.Client{Transport: transport})
		},
	}
	stateRoot := t.TempDir()
	if err := os.Chmod(stateRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_STATE_HOME", stateRoot)
	node := writeCatalogAPINodeArtifact(t, f.artifactsDir, "24.21.0")
	runCatalogCLI(t, deps, "package", "add", "--artifact", node.SHA256,
		"--confirm", "node-runtime@"+node.Version, "--reason", "approve runtime")
	runCatalogCLI(t, deps, "profile", "publish", "--profile", "darwin-node@1",
		"--package", "node-runtime@"+node.Version, "--confirm", "darwin-node@1", "--reason", "approve Mac profile")
	enabled := true
	if err := f.store.RecordCheckin(f.machine.id, model.Checkin{
		SchemaVersion: model.SchemaVersion, SentAt: jobsTestNow, AgentVersion: "test",
		BootID: "boot-cli-mac", AgentSeq: 1, AgentStartedAt: jobsTestNow.Add(-time.Hour), JobsEnabled: &enabled,
	}, jobsTestNow); err != nil {
		t.Fatal(err)
	}
	osName := "macOS 15.7"
	if goos == "linux" {
		osName = "Linux"
	}
	if err := f.store.RecordObservation(f.machine.id, model.ObservationBatch{
		SchemaVersion: model.SchemaVersion, MeasuredAt: jobsTestNow,
		Identity: model.Identity{Hostname: "catalog-cli-mac", OS: osName, Arch: arch, UnixUser: "test"},
	}, jobsTestNow); err != nil {
		t.Fatal(err)
	}
	return f, node, deps, traffic
}

func assertCatalogAssignmentRecoveryGraph(t *testing.T, f jobsFixture, node artifactSidecar, result operator.MachineProfileAssignmentResult) {
	t.Helper()
	var assignments, desired, jobs int
	err := f.store.DB().QueryRow(`SELECT
 (SELECT COUNT(*) FROM machine_profile_assignments WHERE machine_id=?),
 (SELECT COUNT(*) FROM desired_state WHERE scope_type='machine' AND scope_id=?),
 (SELECT COUNT(*) FROM jobs WHERE machine_id=?)`, f.machine.id, f.machine.id, f.machine.id).
		Scan(&assignments, &desired, &jobs)
	if err != nil || assignments != 1 || desired != 1 || jobs != 1 {
		t.Fatalf("recovery graph assignments=%d desired=%d jobs=%d err=%v", assignments, desired, jobs, err)
	}
	pkg := result.Packages[0]
	job, err := f.store.JobForMachine(pkg.JobID, f.machine.id)
	if err != nil || pkg.PackageID != "node-runtime" || pkg.PackageVersion != node.Version ||
		pkg.ArtifactDigest != "sha256:"+node.SHA256 || job.ArtifactDigest != pkg.ArtifactDigest ||
		job.DesiredID != pkg.DesiredID || job.Revision != pkg.Revision || pkg.Revision != 1 {
		t.Fatal("recovery changed the pinned runtime or its original desired state/job")
	}
}

func assertCatalogAssignmentRecoveryAudit(t *testing.T, f jobsFixture, key, digest string, replays int) {
	t.Helper()
	entries, err := f.store.Audit(f.machine.id, 100)
	if err != nil {
		t.Fatal(err)
	}
	var originals, replayed int
	for _, entry := range entries {
		if entry.IdempotencyKey != key {
			continue
		}
		if entry.Action != store.AuditMachineProfileAssign || !entry.OK || entry.RequestDigest != digest ||
			entry.SourceKind != operator.SourceKindOperatorAPI || entry.UserAgent != operatorclient.UserAgent ||
			entry.AuthSubject != "tailscale-user:42" || entry.AuthNodeID != "node-stable-1" ||
			entry.AuthCapability != "example.com/cap/clawctl-admin" || entry.AuthMethod != operatorauth.AuthMethodLocalAPI ||
			entry.AuthDecision != string(operatorauth.Authorized) {
			t.Error("CLI recovery audit lost the request digest, operator identity or outcome")
		}
		if entry.IsOperatorReplay() {
			replayed++
		} else {
			originals++
		}
	}
	if originals != 1 || replayed != replays {
		t.Fatalf("CLI recovery audit originals=%d replayed=%d want 1/%d", originals, replayed, replays)
	}
}

type catalogAssignmentCall struct {
	request, response, key, path string
	status                       int
}

type catalogAssignmentTraffic struct {
	mu       sync.Mutex
	drop     int
	previews int
	calls    []catalogAssignmentCall
}

func (p *catalogAssignmentTraffic) snapshot() []catalogAssignmentCall {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]catalogAssignmentCall(nil), p.calls...)
}

func (p *catalogAssignmentTraffic) previewCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.previews
}

func (p *catalogAssignmentTraffic) handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/profile-assignment-preview") {
			p.mu.Lock()
			p.previews++
			p.mu.Unlock()
		}
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/profile-assignments") {
			next.ServeHTTP(w, r)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "request body unread", http.StatusInternalServerError)
			return
		}
		_ = r.Body.Close()
		r.Body = io.NopCloser(bytes.NewReader(body))
		rec := httptest.NewRecorder()
		next.ServeHTTP(rec, r)
		// Publish observations before closing the socket, so a returning CLI
		// call can inspect the committed response without racing this handler.
		p.mu.Lock()
		p.calls = append(p.calls, catalogAssignmentCall{
			request: string(body), response: rec.Body.String(), key: r.Header.Get("Idempotency-Key"), status: rec.Code,
			path: r.URL.Path,
		})
		drop := (rec.Code == http.StatusOK || rec.Code == http.StatusCreated) && len(p.calls) <= p.drop
		p.mu.Unlock()
		if !drop {
			for key, values := range rec.Header() {
				w.Header()[key] = append([]string(nil), values...)
			}
			w.WriteHeader(rec.Code)
			_, _ = w.Write(rec.Body.Bytes())
			return
		}
		hj, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "connection not hijackable", http.StatusInternalServerError)
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			http.Error(w, "connection hijack failed", http.StatusInternalServerError)
			return
		}
		_ = conn.Close()
	})
}
