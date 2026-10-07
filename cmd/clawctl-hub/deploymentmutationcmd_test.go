package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/deploy"
	"github.com/teddashh/AI-Intune/internal/model"
	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/operatorauth"
	"github.com/teddashh/AI-Intune/internal/operatorclient"
	"github.com/teddashh/AI-Intune/internal/store"
)

func TestDeploymentCreateCLIHTTPFreshAndCanonicalReplay(t *testing.T) {
	f, record := deploymentMutationFixture(t, 1)
	var mu sync.Mutex
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls = append(calls, r.Method+" "+r.URL.Path)
		mu.Unlock()
		f.mux.ServeHTTP(w, verifiedOperatorRequest(r, operatorauth.Admin))
	}))
	defer server.Close()
	base, deps := machineHTTPTestDeps(t, server)
	deps.discoverHubURL = func() (string, error) { return base, nil }
	deps.validateDirectPath = func(string) error {
		t.Fatal("HTTP deployment create inspected direct DB")
		return nil
	}
	missingDB := filepath.Join(t.TempDir(), "must-not-exist.sqlite")
	t.Setenv("CLAWCTL_DB", missingDB)
	const privateReason = "CLI_CREATE_PRIVATE_REASON"
	baseArgs := []string{
		"create", "--channel", "canary", "--version", record.Version,
		"--artifact", record.SHA256, "--batch", "1", "--timeout", "321", "--irreversible",
		"--confirm-channel", "canary", "--confirm-version", record.Version,
		"--reason", privateReason,
	}

	var out, errOut bytes.Buffer
	if err := runDeploymentMutationCommandWithDeps(t.Context(), append(append([]string{}, baseArgs...), "--json"),
		&out, &errOut, deps); err != nil {
		t.Fatalf("HTTP deployment create: %v; stderr=%s", err, errOut.String())
	}
	var envelope deploymentMutationCLIResult
	if err := json.Unmarshal(out.Bytes(), &envelope); err != nil || envelope.Result.Action != "create" ||
		envelope.Result.Replayed || envelope.Result.DeploymentID == "" || len(envelope.Result.Jobs) != 1 ||
		envelope.CreatePreview == nil || envelope.CreatePreview.Impact != 1 ||
		!validDeploymentPreviewDigest(envelope.PreviewDigest) || envelope.IdempotencyKey == "" {
		t.Fatalf("create envelope=%+v err=%v stdout=%s stderr=%s", envelope, err, out.String(), errOut.String())
	}
	assertSafeDeploymentCLIJSON(t, out.Bytes(), privateReason, record.TarballURL, record.FetchedBy)
	for _, raw := range [][]byte{out.Bytes(), errOut.Bytes()} {
		for _, forbidden := range []string{privateReason, record.TarballURL, record.FetchedBy, `"spec"`, `"created_by"`} {
			if bytes.Contains(raw, []byte(forbidden)) {
				t.Fatalf("create CLI output exposed %q: %s", forbidden, raw)
			}
		}
	}
	if !strings.Contains(errOut.String(), "canonical retry:") ||
		!strings.Contains(errOut.String(), envelope.IdempotencyKey) ||
		!strings.Contains(errOut.String(), envelope.PreviewDigest) {
		t.Fatalf("fresh create did not print replay coordinates: %s", errOut.String())
	}
	if _, err := os.Stat(missingDB); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("HTTP create touched default DB: %v", err)
	}

	if err := os.Remove(filepath.Join(f.artifactsDir, record.SHA256+".tgz")); err != nil {
		t.Fatal(err)
	}
	replayArgs := append(append([]string{}, baseArgs...),
		"--hub-url", base, "--idempotency-key", envelope.IdempotencyKey,
		"--preview-digest", envelope.PreviewDigest)
	out.Reset()
	errOut.Reset()
	if err := runDeploymentMutationCommandWithDeps(t.Context(), replayArgs, &out, &errOut, deps); err != nil {
		t.Fatalf("HTTP deployment create replay: %v; stderr=%s", err, errOut.String())
	}
	if !strings.Contains(out.String(), "idempotency replay") ||
		!strings.Contains(out.String(), envelope.IdempotencyKey) ||
		!strings.Contains(out.String(), envelope.PreviewDigest) {
		t.Fatalf("human replay receipt=%q", out.String())
	}
	mu.Lock()
	gotCalls := append([]string(nil), calls...)
	mu.Unlock()
	wantCalls := []string{
		"POST /v1/operator/deployments/preview",
		"POST /v1/operator/deployments",
		"POST /v1/operator/deployments",
	}
	if fmt.Sprint(gotCalls) != fmt.Sprint(wantCalls) {
		t.Fatalf("create calls=%v want=%v", gotCalls, wantCalls)
	}
	var deployments int
	if err := f.store.DB().QueryRow(`SELECT COUNT(*) FROM deployments`).Scan(&deployments); err != nil || deployments != 1 {
		t.Fatalf("deployment create replay wrote %d deployments err=%v", deployments, err)
	}
}

func TestDeploymentControlCLIHTTPFreshAndReplay(t *testing.T) {
	for _, action := range []string{"continue", "retry", "abandon"} {
		t.Run(action, func(t *testing.T) {
			machineCount := 2
			if action == "retry" {
				machineCount = 1
			}
			f, record := deploymentMutationFixture(t, machineCount)
			// Plain Continue refuses a failed batch. The continue case is the
			// legitimate expand after a succeeded opened batch; retry still needs
			// a failed terminal, and abandon stops a failed deployment.
			jobState := deploy.Failed
			if action == "continue" {
				jobState = deploy.Succeeded
			}
			parent, jobs := seedPausedDeploymentMutation(t, f, record, machineCount, jobState)
			var mu sync.Mutex
			var calls []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				calls = append(calls, r.Method+" "+r.URL.Path)
				mu.Unlock()
				f.mux.ServeHTTP(w, verifiedOperatorRequest(r, operatorauth.Admin))
			}))
			defer server.Close()
			base, deps := machineHTTPTestDeps(t, server)
			deps.discoverHubURL = func() (string, error) { return base, nil }
			deps.validateDirectPath = func(string) error {
				t.Fatal("HTTP deployment action inspected direct DB")
				return nil
			}
			const privateReason = "CLI_ACTION_PRIVATE_REASON"
			args := []string{action, parent.DeploymentID, "--reason", privateReason, "--json"}
			switch action {
			case "continue":
				args = append(args, "--confirm-channel", "canary")
			case "retry":
				args = append(args, "--confirm-channel", "canary", "--confirm-version", record.Version)
			case "abandon":
				args = append(args, "--confirm", parent.DeploymentID)
			}

			var out, errOut bytes.Buffer
			if err := runDeploymentMutationCommandWithDeps(t.Context(), args, &out, &errOut, deps); err != nil {
				t.Fatalf("HTTP deployment %s: %v; stderr=%s", action, err, errOut.String())
			}
			var envelope deploymentMutationCLIResult
			if err := json.Unmarshal(out.Bytes(), &envelope); err != nil || envelope.ActionPreview == nil ||
				envelope.ActionPreview.Action != action || envelope.Result.Action != action || envelope.Result.Replayed ||
				envelope.IdempotencyKey == "" || envelope.ExpectedControlRevision == nil ||
				envelope.ExpectedOpenedBatch == nil || *envelope.ExpectedControlRevision != 1 ||
				*envelope.ExpectedOpenedBatch != 1 {
				t.Fatalf("%s envelope=%+v err=%v stdout=%s stderr=%s", action, envelope, err, out.String(), errOut.String())
			}
			assertSafeDeploymentCLIJSON(t, out.Bytes(), privateReason, record.TarballURL, record.FetchedBy)
			if !strings.Contains(errOut.String(), "canonical retry:") ||
				!strings.Contains(errOut.String(), "expected-control-revision=1 expected-opened-batch=1") {
				t.Fatalf("%s preview/retry coordinates=%s", action, errOut.String())
			}

			replayArgs := []string{
				action, parent.DeploymentID, "--hub-url", base, "--reason", privateReason, "--json",
				"--idempotency-key", envelope.IdempotencyKey, "--preview-digest", envelope.PreviewDigest,
				"--expected-control-revision", "1", "--expected-opened-batch", "1",
			}
			switch action {
			case "continue":
				replayArgs = append(replayArgs, "--confirm-channel", "canary")
			case "retry":
				replayArgs = append(replayArgs, "--confirm-channel", "canary", "--confirm-version", record.Version)
			case "abandon":
				replayArgs = append(replayArgs, "--confirm", parent.DeploymentID)
			}
			if action != "abandon" {
				if err := os.Remove(filepath.Join(f.artifactsDir, record.SHA256+".tgz")); err != nil {
					t.Fatal(err)
				}
			}
			out.Reset()
			errOut.Reset()
			if err := runDeploymentMutationCommandWithDeps(t.Context(), replayArgs, &out, &errOut, deps); err != nil {
				t.Fatalf("HTTP deployment %s replay: %v; stderr=%s", action, err, errOut.String())
			}
			var replayed deploymentMutationCLIResult
			if err := json.Unmarshal(out.Bytes(), &replayed); err != nil || !replayed.Result.Replayed ||
				replayed.Result.DeploymentID != envelope.Result.DeploymentID || replayed.ActionPreview != nil {
				t.Fatalf("%s replay=%+v err=%v stdout=%s", action, replayed, err, out.String())
			}
			mu.Lock()
			gotCalls := append([]string(nil), calls...)
			mu.Unlock()
			previewLeaf := map[string]string{
				"continue": "continuation-preview", "retry": "retry-preview", "abandon": "abandonment-preview",
			}[action]
			applyLeaf := map[string]string{
				"continue": "continuations", "retry": "retries", "abandon": "abandonments",
			}[action]
			basePath := "/v1/operator/deployments/" + parent.DeploymentID + "/"
			wantCalls := []string{"POST " + basePath + previewLeaf, "POST " + basePath + applyLeaf, "POST " + basePath + applyLeaf}
			if fmt.Sprint(gotCalls) != fmt.Sprint(wantCalls) {
				t.Fatalf("%s calls=%v want=%v", action, gotCalls, wantCalls)
			}
			if action == "continue" && (envelope.Result.ControlRevision != 2 || envelope.Result.OpenedBatch != 2 || len(envelope.Result.Jobs) != 1) {
				t.Fatalf("continue result=%+v", envelope.Result)
			}
			if action == "retry" && (envelope.Result.RetryOf == nil || *envelope.Result.RetryOf != parent.DeploymentID ||
				envelope.Result.DeploymentID == parent.DeploymentID || len(envelope.Result.Jobs) != 1) {
				t.Fatalf("retry result=%+v parent jobs=%+v", envelope.Result, jobs)
			}
			if action == "abandon" && (envelope.Result.State != store.DeploymentFinished || len(envelope.Result.Jobs) != 0) {
				t.Fatalf("abandon result=%+v", envelope.Result)
			}
		})
	}
}

func TestDeploymentCreateCLIExplicitDBUsesFencedServiceAndReplays(t *testing.T) {
	dbPath, machineID := directDBFixture(t)
	t.Setenv("CLAWCTL_EXPECTATIONS", "")
	st, err := openExisting(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	if err := st.RecordCheckin(machineID, model.Checkin{SentAt: now, AgentStartedAt: now.Add(-time.Hour)}, now); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordObservation(machineID, model.ObservationBatch{
		SchemaVersion: model.SchemaVersion, MeasuredAt: now,
		OpenClaw: model.OpenClaw{Present: true, Install: &model.OpenClawInstall{NodeVersion: "24.15.0"}},
	}, now); err != nil {
		t.Fatal(err)
	}
	if err := st.SetMachineChannel(machineID, "canary"); err != nil {
		t.Fatal(err)
	}
	record := writeJobTestArtifact(t, artifactsDirFor(dbPath), "2026.9.8", "direct-cli-mutation")
	record.EnginesNode = ">=24.15.0 <25"
	if err := writeArtifactSidecar(artifactsDirFor(dbPath), record); err != nil {
		t.Fatal(err)
	}
	loadExpectations(st)
	if err := st.PublishExpectationsPolicy(now); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	deps := productionMachineCommandDeps()
	deps.discoverHubURL = func() (string, error) {
		t.Fatal("explicit --db invoked discovery")
		return "", nil
	}
	deps.newOperatorClient = func(string) (*operatorclient.Client, error) {
		t.Fatal("explicit --db constructed HTTP client")
		return nil, nil
	}
	var stopped atomic.Int32
	deps.verifyHubStopped = func(_ context.Context, got string) error {
		if got != dbPath {
			t.Fatalf("stopped proof path=%q want=%q", got, dbPath)
		}
		stopped.Add(1)
		return nil
	}
	const directReason = "DIRECT_CREATE_APPROVAL"
	args := []string{
		"create", "--db", dbPath, "--channel", "canary", "--version", record.Version,
		"--artifact", record.SHA256, "--batch", "1", "--timeout", "600",
		"--confirm-channel", "canary", "--confirm-version", record.Version,
		"--reason", directReason, "--json",
	}
	var out, errOut bytes.Buffer
	if err := runDeploymentMutationCommandWithDeps(t.Context(), args, &out, &errOut, deps); err != nil {
		t.Fatalf("direct deployment create: %v; stderr=%s", err, errOut.String())
	}
	var fresh deploymentMutationCLIResult
	if err := json.Unmarshal(out.Bytes(), &fresh); err != nil || fresh.Result.Replayed || fresh.Result.Action != "create" {
		t.Fatalf("direct fresh=%+v err=%v body=%s", fresh, err, out.String())
	}
	if strings.Contains(out.String(), directReason) || strings.Contains(errOut.String(), directReason) {
		t.Fatalf("direct create output exposed reason: stdout=%q stderr=%q", out.String(), errOut.String())
	}
	if err := os.Remove(filepath.Join(artifactsDirFor(dbPath), record.SHA256+".tgz")); err != nil {
		t.Fatal(err)
	}
	replayArgs := append(append([]string{}, args...),
		"--idempotency-key", fresh.IdempotencyKey, "--preview-digest", fresh.PreviewDigest)
	out.Reset()
	errOut.Reset()
	if err := runDeploymentMutationCommandWithDeps(t.Context(), replayArgs, &out, &errOut, deps); err != nil {
		t.Fatalf("direct deployment create replay: %v; stderr=%s", err, errOut.String())
	}
	var replay deploymentMutationCLIResult
	if err := json.Unmarshal(out.Bytes(), &replay); err != nil || !replay.Result.Replayed ||
		replay.Result.DeploymentID != fresh.Result.DeploymentID {
		t.Fatalf("direct replay=%+v err=%v body=%s", replay, err, out.String())
	}
	if got := stopped.Load(); got != 2 {
		t.Fatalf("stopped-service checks=%d want=2", got)
	}
	st, err = openExisting(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var deployments int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM deployments`).Scan(&deployments); err != nil || deployments != 1 {
		t.Fatalf("direct replay deployments=%d err=%v", deployments, err)
	}
	assertDirectDeploymentMutationAudits(t, st, store.AuditDeploymentCreate, directReason, fresh.IdempotencyKey)
}

func TestDeploymentControlCLIExplicitDBFreshAndReplay(t *testing.T) {
	for _, action := range []string{"continue", "retry", "abandon"} {
		t.Run(action, func(t *testing.T) {
			dbPath, deploymentID, record := directDeploymentControlFixture(t, action)
			deps := productionMachineCommandDeps()
			deps.discoverHubURL = func() (string, error) {
				t.Fatal("explicit --db invoked discovery")
				return "", nil
			}
			deps.newOperatorClient = func(string) (*operatorclient.Client, error) {
				t.Fatal("explicit --db constructed HTTP client")
				return nil, nil
			}
			var stopped atomic.Int32
			deps.verifyHubStopped = func(_ context.Context, got string) error {
				if got != dbPath {
					t.Fatalf("stopped proof path=%q want=%q", got, dbPath)
				}
				stopped.Add(1)
				return nil
			}
			const directReason = "DIRECT_CONTROL_APPROVAL"
			args := []string{action, deploymentID, "--db", dbPath, "--reason", directReason, "--json"}
			switch action {
			case "continue":
				args = append(args, "--confirm-channel", "canary")
			case "retry":
				args = append(args, "--confirm-channel", "canary", "--confirm-version", record.Version)
			case "abandon":
				args = append(args, "--confirm", deploymentID)
			}
			var out, errOut bytes.Buffer
			if err := runDeploymentMutationCommandWithDeps(t.Context(), args, &out, &errOut, deps); err != nil {
				t.Fatalf("direct deployment %s: %v; stderr=%s", action, err, errOut.String())
			}
			var fresh deploymentMutationCLIResult
			if err := json.Unmarshal(out.Bytes(), &fresh); err != nil || fresh.Result.Replayed ||
				fresh.Result.Action != action || fresh.ActionPreview == nil ||
				fresh.ExpectedControlRevision == nil || fresh.ExpectedOpenedBatch == nil {
				t.Fatalf("direct %s fresh=%+v err=%v body=%s", action, fresh, err, out.String())
			}
			if strings.Contains(out.String(), directReason) || strings.Contains(errOut.String(), directReason) {
				t.Fatalf("direct %s output exposed reason: stdout=%q stderr=%q", action, out.String(), errOut.String())
			}
			replayArgs := append(append([]string{}, args...),
				"--idempotency-key", fresh.IdempotencyKey, "--preview-digest", fresh.PreviewDigest,
				"--expected-control-revision", fmt.Sprint(*fresh.ExpectedControlRevision),
				"--expected-opened-batch", fmt.Sprint(*fresh.ExpectedOpenedBatch))
			if action != "abandon" {
				if err := os.Remove(filepath.Join(artifactsDirFor(dbPath), record.SHA256+".tgz")); err != nil {
					t.Fatal(err)
				}
			}
			out.Reset()
			errOut.Reset()
			if err := runDeploymentMutationCommandWithDeps(t.Context(), replayArgs, &out, &errOut, deps); err != nil {
				t.Fatalf("direct deployment %s replay: %v; stderr=%s", action, err, errOut.String())
			}
			var replay deploymentMutationCLIResult
			if err := json.Unmarshal(out.Bytes(), &replay); err != nil || !replay.Result.Replayed ||
				replay.Result.DeploymentID != fresh.Result.DeploymentID || replay.ActionPreview != nil {
				t.Fatalf("direct %s replay=%+v err=%v body=%s", action, replay, err, out.String())
			}
			if got := stopped.Load(); got != 2 {
				t.Fatalf("direct %s stopped checks=%d want=2", action, got)
			}
			st, err := openExisting(dbPath)
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			actionKind := map[string]store.AuditAction{
				"continue": store.AuditDeploymentContinue,
				"retry":    store.AuditDeploymentRetry,
				"abandon":  store.AuditDeploymentAbandon,
			}[action]
			assertDirectDeploymentMutationAudits(t, st, actionKind, directReason, fresh.IdempotencyKey)
		})
	}
}

func assertDirectDeploymentMutationAudits(t *testing.T, st *store.Store, action store.AuditAction,
	reason, idempotencyKey string,
) {
	t.Helper()
	entries, err := st.Audit("", 10)
	if err != nil || len(entries) != 2 {
		t.Fatalf("direct deployment audits=%+v err=%v", entries, err)
	}
	for i, entry := range entries {
		if entry.Action != action || !entry.OK || entry.Reason != reason ||
			entry.IdempotencyKey != idempotencyKey || entry.RequestDigest == "" ||
			entry.SourceKind != operator.SourceKindDirectDBCLI || entry.SourceAddr != "local-cli" ||
			entry.WhoUnavailable == "" || entry.WhoUser != "" || entry.AuthSubject != "" {
			t.Fatalf("direct deployment audit[%d]=%+v", i, entry)
		}
	}
	if !entries[0].IsOperatorReplay() || entries[1].IsOperatorReplay() {
		t.Fatalf("direct deployment replay audit evidence=%+v", entries)
	}
}

func TestDeploymentMutationCLIDirectDBHonorsExactMaintenanceFence(t *testing.T) {
	dbPath, _ := directDBFixture(t)
	if err := os.WriteFile(upgradeMaintenanceMarker(dbPath), []byte(upgradeMaintenanceContents), 0o600); err != nil {
		t.Fatal(err)
	}
	deps := productionMachineCommandDeps()
	deps.verifyHubStopped = func(context.Context, string) error {
		t.Fatal("maintenance rejection reached stopped-service probe")
		return nil
	}
	args := []string{
		"create", "--db", dbPath, "--channel", "canary", "--version", "2026.9.8",
		"--confirm-channel", "canary", "--confirm-version", "2026.9.8",
	}
	var out, errOut bytes.Buffer
	err := runDeploymentMutationCommandWithDeps(t.Context(), args, &out, &errOut, deps)
	if err == nil || !strings.Contains(err.Error(), "upgrade maintenance") || out.Len() != 0 {
		t.Fatalf("exact maintenance fence err=%v stdout=%q stderr=%q", err, out.String(), errOut.String())
	}
}

func TestDeploymentMutationCLINeverFallsBackToDB(t *testing.T) {
	missingDB := filepath.Join(t.TempDir(), "must-not-exist.sqlite")
	t.Setenv("CLAWCTL_DB", missingDB)
	for _, args := range [][]string{
		{"create", "--channel", "canary", "--version", "2026.9.8", "--confirm-channel", "canary", "--confirm-version", "2026.9.8"},
		{"continue", "deployment-id", "--confirm-channel", "canary"},
		{"retry", "deployment-id", "--confirm-channel", "canary", "--confirm-version", "2026.9.8"},
		{"abandon", "deployment-id", "--confirm", "deployment-id"},
	} {
		t.Run(args[0], func(t *testing.T) {
			deps := machineCommandDeps{
				discoverHubURL: func() (string, error) { return "", errors.New("discovery unavailable") },
				validateDirectPath: func(string) error {
					t.Fatal("discovery failure inspected direct DB")
					return nil
				},
			}
			var out, errOut bytes.Buffer
			err := runDeploymentMutationCommandWithDeps(t.Context(), args, &out, &errOut, deps)
			if err == nil || !strings.Contains(err.Error(), "discovery unavailable") ||
				out.Len() != 0 {
				t.Fatalf("args=%q err=%v stdout=%q stderr=%q", args, err, out.String(), errOut.String())
			}
		})
	}
	if _, err := os.Stat(missingDB); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed discovery created fallback DB: %v", err)
	}
}

func TestDeploymentMutationCLIRejectsInvalidArgumentsBeforeNetwork(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	defer server.Close()
	base, deps := machineHTTPTestDeps(t, server)
	deps.discoverHubURL = func() (string, error) {
		t.Fatal("invalid deployment mutation reached discovery")
		return "", nil
	}
	digest := "sha256:" + strings.Repeat("a", 64)
	validCreate := []string{
		"create", "--hub-url", base, "--channel", "canary", "--version", "2026.9.8",
		"--confirm-channel", "canary", "--confirm-version", "2026.9.8",
	}
	for _, test := range []struct {
		name string
		args []string
	}{
		{name: "missing action", args: nil},
		{name: "unknown action", args: []string{"destroy"}},
		{name: "create positional", args: append(append([]string{}, validCreate...), "extra")},
		{name: "create by spoof", args: append(append([]string{}, validCreate...), "--by", "operator")},
		{name: "create missing channel", args: []string{"create", "--hub-url", base, "--version", "2026.9.8", "--confirm-channel", "canary", "--confirm-version", "2026.9.8"}},
		{name: "create missing typed confirmation", args: []string{"create", "--hub-url", base, "--channel", "canary", "--version", "2026.9.8"}},
		{name: "create mismatched channel", args: []string{"create", "--hub-url", base, "--channel", "canary", "--version", "2026.9.8", "--confirm-channel", "stable", "--confirm-version", "2026.9.8"}},
		{name: "create mismatched version", args: []string{"create", "--hub-url", base, "--channel", "canary", "--version", "2026.9.8", "--confirm-channel", "canary", "--confirm-version", "2026.9.7"}},
		{name: "create partial retry group", args: append(append([]string{}, validCreate...), "--idempotency-key", "key")},
		{name: "create expected revision", args: append(append([]string{}, validCreate...), "--expected-control-revision", "0")},
		{name: "create empty artifact", args: append(append([]string{}, validCreate...), "--artifact", "")},
		{name: "create batch zero", args: append(append([]string{}, validCreate...), "--batch", "0")},
		{name: "create timeout large", args: append(append([]string{}, validCreate...), "--timeout", "86401")},
		{name: "control missing ID", args: []string{"continue", "--hub-url", base, "--confirm-channel", "canary"}},
		{name: "control extra ID", args: []string{"continue", "one", "--hub-url", base, "--confirm-channel", "canary", "two"}},
		{name: "control bad ID", args: []string{"continue", "one/two", "--hub-url", base, "--confirm-channel", "canary"}},
		{name: "continue missing confirm", args: []string{"continue", "one", "--hub-url", base}},
		{name: "continue version confirm", args: []string{"continue", "one", "--hub-url", base, "--confirm-channel", "canary", "--confirm-version", "2026.9.8"}},
		{name: "retry missing version confirm", args: []string{"retry", "one", "--hub-url", base, "--confirm-channel", "canary"}},
		{name: "retry planning flag", args: []string{"retry", "one", "--hub-url", base, "--confirm-channel", "canary", "--confirm-version", "2026.9.8", "--batch", "1"}},
		{name: "abandon short confirm", args: []string{"abandon", "deployment-full-id", "--hub-url", base, "--confirm", "deployment"}},
		{name: "abandon channel confirm", args: []string{"abandon", "one", "--hub-url", base, "--confirm", "one", "--confirm-channel", "canary"}},
		{name: "partial control retry group", args: []string{"continue", "one", "--hub-url", base, "--confirm-channel", "canary", "--idempotency-key", "key", "--preview-digest", digest}},
		{name: "invalid preview digest", args: []string{"continue", "one", "--hub-url", base, "--confirm-channel", "canary", "--idempotency-key", "key", "--preview-digest", "bad", "--expected-control-revision", "0", "--expected-opened-batch", "0"}},
		{name: "negative expected", args: []string{"continue", "one", "--hub-url", base, "--confirm-channel", "canary", "--idempotency-key", "key", "--preview-digest", digest, "--expected-control-revision=-1", "--expected-opened-batch", "0"}},
		{name: "empty idempotency key", args: []string{"continue", "one", "--hub-url", base, "--confirm-channel", "canary", "--idempotency-key", "", "--preview-digest", digest, "--expected-control-revision", "0", "--expected-opened-batch", "0"}},
		{name: "reason surrounding whitespace", args: append(append([]string{}, validCreate...), "--reason", " private")},
		{name: "reason too long", args: append(append([]string{}, validCreate...), "--reason", strings.Repeat("x", 501))},
		{name: "mixed transports", args: append(append([]string{}, validCreate...), "--db", "/not-used")},
	} {
		t.Run(test.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			if err := runDeploymentMutationCommandWithDeps(t.Context(), test.args, &out, &errOut, deps); err == nil {
				t.Fatalf("invalid args %q accepted; stdout=%q stderr=%q", test.args, out.String(), errOut.String())
			}
			if out.Len() != 0 {
				t.Fatalf("invalid args %q wrote stdout=%q", test.args, out.String())
			}
		})
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("invalid deployment mutations reached HTTP server %d times", got)
	}
}

func deploymentMutationFixture(t *testing.T, machineCount int) (jobsFixture, artifactSidecar) {
	t.Helper()
	stateRoot := t.TempDir()
	if err := os.Chmod(stateRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_STATE_HOME", stateRoot)
	f := newJobsFixture(t, "deployment-cli-target-1")
	(&hub{store: f.store, artifactsDir: f.artifactsDir}).operatorRoutes(f.mux)
	machines := []enrolled{f.machine}
	for i := 2; i <= machineCount; i++ {
		machines = append(machines, enrollViaHTTP(t, f.mux, f.store, fmt.Sprintf("deployment-cli-target-%d", i)))
	}
	now := time.Now().UTC().Truncate(time.Second)
	for _, machine := range machines {
		if err := f.store.RecordCheckin(machine.id, model.Checkin{SentAt: now, AgentStartedAt: now.Add(-time.Hour)}, now); err != nil {
			t.Fatal(err)
		}
		if err := f.store.RecordObservation(machine.id, model.ObservationBatch{
			SchemaVersion: model.SchemaVersion, MeasuredAt: now,
			OpenClaw: model.OpenClaw{Present: true, Install: &model.OpenClawInstall{NodeVersion: "24.15.0"}},
		}, now); err != nil {
			t.Fatal(err)
		}
		if err := f.store.SetMachineChannel(machine.id, "canary"); err != nil {
			t.Fatal(err)
		}
	}
	record := writeJobTestArtifact(t, f.artifactsDir, "2026.9.8", "deployment-cli-mutation")
	record.EnginesNode = ">=24.15.0 <25"
	record.FetchedBy = "private-fetch-user@private-host"
	record.TarballURL = "https://signed.example.invalid/openclaw.tgz?token=private"
	if err := writeArtifactSidecar(f.artifactsDir, record); err != nil {
		t.Fatal(err)
	}
	return f, record
}

func seedPausedDeploymentMutation(t *testing.T, f jobsFixture, record artifactSidecar,
	machineCount int, jobState deploy.JobState,
) (store.Deployment, []store.Job) {
	t.Helper()
	material, err := prepareOpenClawJob(f.artifactsDir, record.Version, record.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	machines, err := f.store.MachinesInChannel("canary")
	if err != nil || len(machines) != machineCount {
		t.Fatalf("channel machines=%+v err=%v", machines, err)
	}
	targets := make([]store.NewDeploymentTarget, 0, len(machines))
	for i, machine := range machines {
		batch := 1
		if machineCount > 1 {
			batch = i + 1
		}
		targets = append(targets, store.NewDeploymentTarget{MachineID: machine.MachineID, BatchNo: batch})
	}
	d, jobs, err := f.store.CreateDeployment(store.NewDeployment{
		Channel: "canary", ResourceKind: "openclaw", ResourceID: "openclaw", Spec: material.Spec,
		BatchSize: 1, CreatedBy: "test-only raw fixture", Targets: targets,
		Job: store.NewJob{ArtifactDigest: material.Digest, ExecutionTimeout: 600},
	})
	if err != nil || len(jobs) != 1 {
		t.Fatalf("seed deployment=%+v jobs=%+v err=%v", d, jobs, err)
	}
	terminalAt := time.Now().UTC().Truncate(time.Second)
	if _, err := f.store.DB().Exec(`UPDATE jobs SET state=?,terminal_at=? WHERE job_id=?`,
		jobState, terminalAt.Format(time.RFC3339Nano), jobs[0].JobID); err != nil {
		t.Fatal(err)
	}
	if changed, err := f.store.SetDeploymentState(d.DeploymentID, store.DeploymentRunning, store.DeploymentPaused, terminalAt); err != nil || !changed {
		t.Fatalf("pause deployment changed=%t err=%v", changed, err)
	}
	return d, jobs
}

func directDeploymentControlFixture(t *testing.T, action string) (string, string, artifactSidecar) {
	t.Helper()
	dbPath, firstMachineID := directDBFixture(t)
	t.Setenv("CLAWCTL_EXPECTATIONS", "")
	st, err := openExisting(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	machineIDs := []string{firstMachineID}
	if action != "retry" {
		second, _, err := st.CreateEnrollTokenFor("direct-control-target-2", time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		machineIDs = append(machineIDs, second)
	}
	now := time.Now().UTC().Truncate(time.Second)
	for _, machineID := range machineIDs {
		if err := st.RecordCheckin(machineID, model.Checkin{SentAt: now, AgentStartedAt: now.Add(-time.Hour)}, now); err != nil {
			t.Fatal(err)
		}
		if err := st.RecordObservation(machineID, model.ObservationBatch{
			SchemaVersion: model.SchemaVersion, MeasuredAt: now,
			OpenClaw: model.OpenClaw{Present: true, Install: &model.OpenClawInstall{NodeVersion: "24.15.0"}},
		}, now); err != nil {
			t.Fatal(err)
		}
		if err := st.SetMachineChannel(machineID, "canary"); err != nil {
			t.Fatal(err)
		}
	}
	record := writeJobTestArtifact(t, artifactsDirFor(dbPath), "2026.9.8", "direct-control-"+action)
	record.EnginesNode = ">=24.15.0 <25"
	if err := writeArtifactSidecar(artifactsDirFor(dbPath), record); err != nil {
		t.Fatal(err)
	}
	material, err := prepareOpenClawJob(artifactsDirFor(dbPath), record.Version, record.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	targets := make([]store.NewDeploymentTarget, 0, len(machineIDs))
	for i, machineID := range machineIDs {
		targets = append(targets, store.NewDeploymentTarget{MachineID: machineID, BatchNo: i + 1})
	}
	parent, jobs, err := st.CreateDeployment(store.NewDeployment{
		Channel: "canary", ResourceKind: "openclaw", ResourceID: "openclaw", Spec: material.Spec,
		BatchSize: 1, CreatedBy: "test-only direct fixture", Targets: targets,
		Job: store.NewJob{ArtifactDigest: material.Digest, ExecutionTimeout: 600},
	})
	if err != nil || len(jobs) != 1 {
		t.Fatalf("direct seed deployment=%+v jobs=%+v err=%v", parent, jobs, err)
	}
	// Artifact preparation and CreateDeployment can cross a wall-clock second
	// under -race. Anchor the synthetic terminal/pause lifecycle to the durable
	// deployment creation clock, never to the earlier check-in fixture clock.
	lifecycleAt := parent.CreatedAt.UTC().Truncate(time.Second)
	// Continue is the expand after a succeeded batch. Retry and abandon keep
	// the failed terminal: retry binds it, abandon stops without opening more.
	jobState := deploy.Failed
	if action == "continue" {
		jobState = deploy.Succeeded
	}
	if _, err := st.DB().Exec(`UPDATE jobs SET state=?,terminal_at=? WHERE job_id=?`,
		jobState, lifecycleAt.Format(time.RFC3339Nano), jobs[0].JobID); err != nil {
		t.Fatal(err)
	}
	if changed, err := st.SetDeploymentState(parent.DeploymentID, store.DeploymentRunning, store.DeploymentPaused, lifecycleAt); err != nil || !changed {
		t.Fatalf("direct pause changed=%t err=%v", changed, err)
	}
	loadExpectations(st)
	if err := st.PublishExpectationsPolicy(now); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	return dbPath, parent.DeploymentID, record
}
