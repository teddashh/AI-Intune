package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/model"
	"github.com/teddashh/AI-Intune/internal/operatorauth"
)

func catalogCLIFixture(t *testing.T) (jobsFixture, artifactSidecar, artifactSidecar, *httptest.Server, machineCommandDeps) {
	t.Helper()
	f := newJobsFixture(t, "catalog-cli-target")
	(&hub{store: f.store, artifactsDir: f.artifactsDir}).operatorRoutes(f.mux)
	node := writeCatalogAPINodeArtifact(t, f.artifactsDir, "24.21.0")
	openclaw := writeJobTestArtifact(t, f.artifactsDir, "2026.9.2", "catalog-cli-openclaw")
	openclaw.EnginesNode = ">=24.15.0 <25"
	if err := writeArtifactSidecar(f.artifactsDir, openclaw); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mux.ServeHTTP(w, verifiedOperatorRequest(r, operatorauth.Admin))
	}))
	t.Cleanup(server.Close)
	base, deps := machineHTTPTestDeps(t, server)
	deps.discoverHubURL = func() (string, error) { return base, nil }
	stateRoot := t.TempDir()
	if err := os.Chmod(stateRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_STATE_HOME", stateRoot)
	return f, node, openclaw, server, deps
}

func runCatalogCLI(t *testing.T, deps machineCommandDeps, args ...string) (string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	if err := runCatalogCommandWithDeps(t.Context(), args, &out, &errOut, deps); err != nil {
		t.Fatalf("catalog %v: %v; stderr=%s", args, err, errOut.String())
	}
	return out.String(), errOut.String()
}

func TestCatalogCLIStandardStoreProfileAndAssignment(t *testing.T) {
	f, node, openclaw, _, deps := catalogCLIFixture(t)
	const reason = "PRIVATE_CATALOG_CLI_REASON"

	preview, _ := runCatalogCLI(t, deps, "package", "add", "--artifact", node.SHA256, "--preview")
	if !strings.Contains(preview, "package node-runtime@24.21.0") || !strings.Contains(preview, "dependencies none") {
		t.Fatalf("node preview=%s", preview)
	}
	nodeOut, nodeErr := runCatalogCLI(t, deps, "package", "add", "--artifact", node.SHA256,
		"--confirm", "node-runtime@24.21.0", "--reason", reason)
	if !strings.Contains(nodeOut, "published node-runtime@24.21.0") || strings.Contains(nodeOut+nodeErr, reason) {
		t.Fatalf("node output=%q stderr=%q", nodeOut, nodeErr)
	}
	openOut, _ := runCatalogCLI(t, deps, "package", "add", "--artifact", openclaw.SHA256,
		"--node-runtime-version", node.Version, "--confirm", "openclaw@"+openclaw.Version, "--reason", reason)
	if !strings.Contains(openOut, "published openclaw@"+openclaw.Version) {
		t.Fatalf("openclaw output=%q", openOut)
	}
	profileOut, _ := runCatalogCLI(t, deps, "profile", "publish", "--profile", "openclaw-standard@1",
		"--package", "openclaw@"+openclaw.Version, "--confirm", "openclaw-standard@1", "--reason", reason)
	if !strings.Contains(profileOut, "published openclaw-standard@1") {
		t.Fatalf("profile output=%q", profileOut)
	}
	jobsEnabled := true
	if err := f.store.RecordCheckin(f.machine.id, model.Checkin{
		SchemaVersion: model.SchemaVersion, SentAt: jobsTestNow, AgentVersion: "test",
		BootID: "boot-catalog-cli", AgentSeq: 1, AgentStartedAt: jobsTestNow.Add(-time.Hour),
		JobsEnabled: &jobsEnabled,
	}, jobsTestNow); err != nil {
		t.Fatal(err)
	}
	assignmentPreview, _ := runCatalogCLI(t, deps, "assign", "--machine", f.machine.id,
		"--profile", "openclaw-standard@1", "--preview")
	if !strings.Contains(assignmentPreview, "create_jobs 2") ||
		!strings.Contains(assignmentPreview, "package 1 node-runtime@24.21.0") ||
		!strings.Contains(assignmentPreview, "package 2 openclaw@"+openclaw.Version) ||
		!strings.Contains(assignmentPreview, "blockers none") {
		t.Fatalf("assignment preview=%q", assignmentPreview)
	}
	assignmentOut, _ := runCatalogCLI(t, deps, "assign", "--machine", f.machine.id,
		"--profile", "openclaw-standard@1", "--confirm-name", "catalog-cli-target", "--reason", reason)
	if !strings.Contains(assignmentOut, "assigned openclaw-standard@1 to catalog-cli-target jobs=2") {
		t.Fatalf("assignment output=%q", assignmentOut)
	}

	packageList, _ := runCatalogCLI(t, deps, "package", "list")
	profileList, _ := runCatalogCLI(t, deps, "profile", "list")
	if !strings.Contains(packageList, "node-runtime@24.21.0") || !strings.Contains(packageList, "openclaw@"+openclaw.Version) ||
		!strings.Contains(profileList, "openclaw-standard@1") {
		t.Fatalf("package list=%q profile list=%q", packageList, profileList)
	}

	entries, err := os.ReadDir(filepath.Join(os.Getenv("XDG_STATE_HOME"), "clawctl", "deployment-recovery"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("completed catalog receipts=%v err=%v", entries, err)
	}
	var jobs int
	if err := f.store.DB().QueryRow(`SELECT COUNT(*) FROM jobs WHERE machine_id=?`, f.machine.id).Scan(&jobs); err != nil || jobs != 2 {
		t.Fatalf("profile jobs=%d err=%v", jobs, err)
	}
}

func TestCatalogCLIRecoveryReplaysExactCommittedRequestWithoutArtifact(t *testing.T) {
	f := newJobsFixture(t, "catalog-cli-recovery")
	(&hub{store: f.store, artifactsDir: f.artifactsDir}).operatorRoutes(f.mux)
	node := writeCatalogAPINodeArtifact(t, f.artifactsDir, "24.21.0")
	mutations := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/v1/operator/catalog-manifests" {
			mutations++
			if mutations == 1 {
				recorder := httptest.NewRecorder()
				f.mux.ServeHTTP(recorder, verifiedOperatorRequest(r, operatorauth.Admin))
				if recorder.Code != http.StatusCreated {
					t.Fatalf("hidden commit status=%d body=%s", recorder.Code, recorder.Body.String())
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusInternalServerError)
				_ = json.NewEncoder(w).Encode(map[string]string{"code": "INTERNAL", "message": "request outcome available by replay"})
				return
			}
		}
		f.mux.ServeHTTP(w, verifiedOperatorRequest(r, operatorauth.Admin))
	}))
	defer server.Close()
	base, deps := machineHTTPTestDeps(t, server)
	deps.discoverHubURL = func() (string, error) { return base, nil }
	stateRoot := t.TempDir()
	if err := os.Chmod(stateRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_STATE_HOME", stateRoot)

	var out, errOut bytes.Buffer
	err := runCatalogCommandWithDeps(t.Context(), []string{"package", "add", "--artifact", node.SHA256,
		"--confirm", "node-runtime@24.21.0", "--reason", "approve runtime"}, &out, &errOut, deps)
	if err == nil || !strings.Contains(errOut.String(), "recovery_file ") {
		t.Fatalf("ambiguous mutation err=%v stdout=%q stderr=%q", err, out.String(), errOut.String())
	}
	line := strings.Split(strings.TrimSpace(errOut.String()), "\n")[0]
	quotedPath := strings.TrimPrefix(line, "recovery_file ")
	path, unquoteErr := strconv.Unquote(quotedPath)
	if unquoteErr != nil {
		t.Fatalf("recovery path %q: %v", quotedPath, unquoteErr)
	}
	info, statErr := os.Stat(path)
	if statErr != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("recovery receipt path=%q mode=%v err=%v", path, info, statErr)
	}
	if err := os.Remove(filepath.Join(f.artifactsDir, node.SHA256+".tgz")); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	errOut.Reset()
	if err := runCatalogCommandWithDeps(t.Context(), []string{"recover", "--recovery-file", path},
		&out, &errOut, deps); err != nil {
		t.Fatalf("catalog recover: %v stderr=%s", err, errOut.String())
	}
	if !strings.Contains(out.String(), "replayed=true") || mutations != 2 {
		t.Fatalf("recovery output=%q mutations=%d", out.String(), mutations)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("completed recovery receipt remains: %v", err)
	}
}

func TestCatalogCLIRequiresExactConfirmations(t *testing.T) {
	_, node, _, _, deps := catalogCLIFixture(t)
	var out, errOut bytes.Buffer
	err := runCatalogCommandWithDeps(t.Context(), []string{"package", "add", "--artifact", node.SHA256,
		"--confirm", "node-runtime@wrong", "--reason", "approve runtime"}, &out, &errOut, deps)
	if err == nil || !strings.Contains(err.Error(), "--confirm must be node-runtime@24.21.0") {
		t.Fatalf("confirmation err=%v", err)
	}
	stateRoot := os.Getenv("XDG_STATE_HOME")
	if entries, readErr := os.ReadDir(stateRoot); readErr == nil && len(entries) != 0 {
		t.Fatalf("rejected confirmation created recovery state: %v", entries)
	}
}
