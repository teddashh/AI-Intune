package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/model"
	"github.com/teddashh/AI-Intune/internal/operatorclient"
)

func TestJobCreateCLIUsesHTTPPreviewApplyAndReplay(t *testing.T) {
	f := observedOperatorFixture(t)
	recordDiagnosticCheckin(t, f)
	var mu sync.Mutex
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls = append(calls, r.Method+" "+r.URL.Path)
		mu.Unlock()
		f.mux.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	base, deps := machineHTTPTestDeps(t, server)
	deps.validateDirectPath = func(string) error {
		t.Fatal("HTTP job create inspected direct DB")
		return nil
	}
	missingDB := filepath.Join(t.TempDir(), "must-not-exist.sqlite")
	t.Setenv("CLAWCTL_DB", missingDB)

	var previewOut, errOut bytes.Buffer
	if err := runJobCreateCommandWithDeps(t.Context(), []string{
		"--hub-url", base, "--kind", "noop", "--machine", f.machine.id,
		"--timeout", "90", "--preview", "--json",
	}, &previewOut, &errOut, deps); err != nil {
		t.Fatalf("preview: %v stderr=%s", err, errOut.String())
	}
	var preview operatorclient.DiagnosticNoopPreviewResponse
	if err := json.Unmarshal(previewOut.Bytes(), &preview); err != nil || preview.PreviewDigest == "" ||
		preview.MachineID != f.machine.id || preview.ExecutionTimeoutSeconds != 90 {
		t.Fatalf("preview=%+v err=%v body=%s", preview, err, previewOut.String())
	}
	if countJobCreateRows(t, f, "desired_state") != 0 || countJobCreateRows(t, f, "jobs") != 0 {
		t.Fatal("preview mutated job ledger")
	}

	args := []string{
		"--hub-url", base, "--kind", "noop", "--machine", f.machine.id,
		"--timeout", "90", "--reason", "verify protocol", "--confirm-name", "cnode-operator",
		"--idempotency-key", "job-cli-key", "--preview-digest", preview.PreviewDigest, "--json",
	}
	var createOut bytes.Buffer
	if err := runJobCreateCommandWithDeps(t.Context(), args, &createOut, &errOut, deps); err != nil {
		t.Fatalf("create: %v stderr=%s", err, errOut.String())
	}
	var created operatorclient.DiagnosticNoopResponse
	if err := json.Unmarshal(createOut.Bytes(), &created); err != nil || created.Replayed || created.JobID == "" {
		t.Fatalf("created=%+v err=%v body=%s", created, err, createOut.String())
	}
	var replayOut bytes.Buffer
	if err := runJobCreateCommandWithDeps(t.Context(), args, &replayOut, &errOut, deps); err != nil {
		t.Fatalf("replay: %v stderr=%s", err, errOut.String())
	}
	var replayed operatorclient.DiagnosticNoopResponse
	if err := json.Unmarshal(replayOut.Bytes(), &replayed); err != nil || !replayed.Replayed || replayed.JobID != created.JobID {
		t.Fatalf("replayed=%+v err=%v body=%s", replayed, err, replayOut.String())
	}
	if countJobCreateRows(t, f, "desired_state") != 1 || countJobCreateRows(t, f, "jobs") != 1 {
		t.Fatal("replay duplicated job evidence")
	}
	if _, err := os.Stat(missingDB); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("HTTP mode touched direct DB: %v", err)
	}
	mu.Lock()
	gotCalls := append([]string(nil), calls...)
	mu.Unlock()
	basePath := "/v1/operator/machines/" + f.machine.id
	want := []string{
		http.MethodPost + " " + basePath + "/diagnostic-noop-preview",
		http.MethodPost + " " + basePath + "/diagnostic-noop-jobs",
		http.MethodPost + " " + basePath + "/diagnostic-noop-jobs",
	}
	if strings.Join(gotCalls, "|") != strings.Join(want, "|") {
		t.Fatalf("calls=%v want=%v", gotCalls, want)
	}
}

func TestJobCreateCLIAutoPreviewReturnsRetryCoordinates(t *testing.T) {
	f := observedOperatorFixture(t)
	recordDiagnosticCheckin(t, f)
	server := httptest.NewServer(f.mux)
	t.Cleanup(server.Close)
	base, deps := machineHTTPTestDeps(t, server)
	var out, errOut bytes.Buffer
	err := runJobCreateCommandWithDeps(t.Context(), []string{
		"--hub-url", base, "--kind", "noop", "--machine", f.machine.id,
		"--timeout", "60", "--reason", "verify protocol", "--confirm-name", "cnode-operator",
	}, &out, &errOut, deps)
	if err != nil {
		t.Fatalf("auto apply: %v stderr=%s", err, errOut.String())
	}
	for _, want := range []string{"created job=", "request_key=", "preview_digest=", "config_changed=false"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("receipt missing %q: %s", want, out.String())
		}
	}
	if !strings.HasPrefix(errOut.String(), "preview machine=cnode-operator") {
		t.Fatalf("preview output=%q", errOut.String())
	}
}

func TestJobCreateCLIExplicitDBUsesFencedOperatorService(t *testing.T) {
	dbPath, machineID := directDBFixture(t)
	st, err := openExisting(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := st.RecordCheckin(machineID, model.Checkin{
		SchemaVersion: model.SchemaVersion, SentAt: now,
		AgentVersion: "direct-test", BootID: "direct-boot", AgentSeq: 1,
	}, now); err != nil {
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
	checks := 0
	deps.verifyHubStopped = func(context.Context, string) error { checks++; return nil }
	var out, errOut bytes.Buffer
	if err := runJobCreateCommandWithDeps(t.Context(), []string{
		"--db", dbPath, "--kind", "noop", "--machine", machineID, "--preview", "--json",
	}, &out, &errOut, deps); err != nil {
		t.Fatalf("direct preview: %v stderr=%s", err, errOut.String())
	}
	if checks != 1 || !strings.Contains(out.String(), `"preview_digest"`) {
		t.Fatalf("checks=%d output=%s", checks, out.String())
	}
}

func TestJobCreateCLIRejectsRemovedWriterFlagsBeforeTransport(t *testing.T) {
	deps := productionMachineCommandDeps()
	deps.discoverHubURL = func() (string, error) {
		t.Fatal("removed flag reached transport")
		return "", nil
	}
	var out, errOut bytes.Buffer
	err := runJobCreateCommandWithDeps(t.Context(), []string{
		"--kind", "noop", "--machine", "machine-1", "--preview", "--spec", `{"kind":"noop"}`,
	}, &out, &errOut, deps)
	if err == nil || !strings.Contains(err.Error(), "--spec is not supported") || out.Len() != 0 {
		t.Fatalf("err=%v out=%q", err, out.String())
	}
}

func countJobCreateRows(t *testing.T, f jobsFixture, table string) int {
	t.Helper()
	var count int
	if err := f.store.DB().QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}
