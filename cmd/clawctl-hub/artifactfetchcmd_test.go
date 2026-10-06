package main

import (
	"bytes"
	"context"
	"encoding/base64"
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

	"github.com/teddashh/AI-Intune/internal/artifact"
	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/operatorclient"
	"github.com/teddashh/AI-Intune/internal/store"
)

func artifactFetchCLITestTime() time.Time {
	return time.Date(2026, 9, 8, 18, 0, 0, 0, time.UTC)
}

func artifactFetchCLITestDigest(fill string) string {
	return "sha256:" + strings.Repeat(fill, 64)
}

func artifactFetchCLITestIntegrity() string {
	return "sha512-" + base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x4a}, 64))
}

func artifactFetchCLITestPreview() operator.ArtifactFetchPreviewResult {
	engines := ">=22"
	return operator.ArtifactFetchPreviewResult{
		SchemaVersion: operator.ArtifactFetchPreviewSchemaVersion,
		PolicyVersion: artifact.FetchPolicyVersion, SourceKind: artifact.ArtifactSourceNPM,
		PreviewedAt: artifactFetchCLITestTime(),
		Name:        "openclaw", Version: "2026.9.8", RegistryOrigin: artifact.ProductionRegistryOrigin,
		SHA512Integrity: artifactFetchCLITestIntegrity(), EnginesNode: &engines,
		MaxBytes: artifact.DefaultArtifactMaxBytes, PreviewDigest: artifactFetchCLITestDigest("a"),
		EnqueueAllowed: true, Blockers: []string{},
	}
}

func artifactFetchCLITestOperation(state store.ArtifactFetchState) store.ArtifactFetchOperation {
	created := artifactFetchCLITestTime()
	operation := store.ArtifactFetchOperation{
		OperationID: "0123456789abcdef0123456789abcdef",
		Name:        "openclaw", Version: "2026.9.8", SourceKind: artifact.ArtifactSourceNPM,
		RegistryOrigin:  artifact.ProductionRegistryOrigin,
		SHA512Integrity: artifactFetchCLITestIntegrity(), EnginesNode: ">=22",
		IdentityDigest: artifactFetchCLITestDigest("b"), PreviewDigest: artifactFetchCLITestDigest("a"),
		MaxBytes: artifact.DefaultArtifactMaxBytes, State: store.ArtifactFetchQueued,
		Phase: store.ArtifactFetchPhaseQueued, CreatedAt: created, UpdatedAt: created,
	}
	switch state {
	case store.ArtifactFetchRunning:
		started := created.Add(time.Second)
		operation.State, operation.Phase = state, store.ArtifactFetchPhaseDownloading
		operation.ProgressBytes, operation.Attempt = 1024, 1
		operation.StartedAt, operation.UpdatedAt = &started, started
	case store.ArtifactFetchSucceeded:
		started, finished := created.Add(time.Second), created.Add(2*time.Second)
		sha, size := strings.Repeat("c", 64), int64(4096)
		operation.State, operation.Phase = state, store.ArtifactFetchPhaseComplete
		operation.ProgressBytes, operation.Attempt = size, 1
		operation.StartedAt, operation.FinishedAt, operation.UpdatedAt = &started, &finished, finished
		operation.ResultSHA256, operation.ResultSizeBytes = &sha, &size
	case store.ArtifactFetchFailed:
		started, finished := created.Add(time.Second), created.Add(2*time.Second)
		code, detail := operator.ArtifactFetchFailureIntegrityMismatch, "artifact bytes did not match the pinned integrity"
		operation.State, operation.Phase = state, store.ArtifactFetchPhaseVerifying
		operation.ProgressBytes, operation.Attempt = 4096, 1
		operation.StartedAt, operation.FinishedAt, operation.UpdatedAt = &started, &finished, finished
		operation.ErrorCode, operation.ErrorDetail = &code, &detail
	}
	return operation
}

func artifactFetchCLIResponseHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "private, no-store")
}

func TestArtifactFetchCLIHTTPPreviewEnqueueAndWaitsForTerminal(t *testing.T) {
	stateDir := t.TempDir()
	if err := os.Chmod(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_STATE_HOME", stateDir)
	const (
		privateReason = "PRIVATE_ARTIFACT_FETCH_REASON"
		privateKey    = "PRIVATE_ARTIFACT_FETCH_KEY"
		privateURL    = "https://registry.example/private.tgz?token=DO_NOT_PRINT"
	)
	var detailCalls atomic.Int32
	var mu sync.Mutex
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		artifactFetchCLIResponseHeaders(w)
		mu.Lock()
		calls = append(calls, r.Method+" "+r.URL.RequestURI())
		mu.Unlock()
		switch r.URL.Path {
		case "/v1/operator/artifact-fetches/preview":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body) != 2 ||
				body["name"] != "openclaw" || body["version"] != "2026.9.8" {
				t.Errorf("preview body=%v err=%v", body, err)
			}
			_ = json.NewEncoder(w).Encode(artifactFetchCLITestPreview())
		case "/v1/operator/artifact-fetches":
			if r.Method != http.MethodPost || r.Header.Get("Idempotency-Key") != privateKey {
				t.Errorf("create method=%s headers=%v", r.Method, r.Header)
			}
			var body map[string]json.RawMessage
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body) != 6 ||
				body["tarball_url"] != nil || body["run_token"] != nil || body["actor"] != nil {
				t.Errorf("create body=%v err=%v", body, err)
			}
			var reason string
			_ = json.Unmarshal(body["reason"], &reason)
			if reason != privateReason {
				t.Errorf("create reason=%q", reason)
			}
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(operator.ArtifactFetchApplyResult{
				Operation: artifactFetchCLITestOperation(store.ArtifactFetchQueued),
			})
		case "/v1/operator/artifact-fetches/0123456789abcdef0123456789abcdef":
			state := store.ArtifactFetchRunning
			if detailCalls.Add(1) >= 2 {
				state = store.ArtifactFetchSucceeded
			}
			_ = json.NewEncoder(w).Encode(artifactFetchCLITestOperation(state))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	base, deps := machineHTTPTestDeps(t, server)
	deps.discoverHubURL = func() (string, error) { return base, nil }
	deps.validateDirectPath = func(string) error {
		t.Fatal("normal artifact fetch inspected direct DB")
		return nil
	}
	deps.verifyHubStopped = func(context.Context, string) error {
		t.Fatal("normal artifact fetch checked stopped-service state")
		return nil
	}
	missingDB := filepath.Join(t.TempDir(), "must-not-exist.sqlite")
	t.Setenv("CLAWCTL_DB", missingDB)

	var out, errOut bytes.Buffer
	err := runArtifactFetchCommandWithDeps(t.Context(), []string{
		"openclaw@2026.9.8", "--confirm-version", "2026.9.8", "--reason", privateReason,
		"--idempotency-key", privateKey, "--wait", "--poll-interval", "1ms", "--json",
	}, &out, &errOut, deps)
	if err != nil {
		t.Fatalf("artifact fetch wait: %v; stdout=%s stderr=%s", err, out.String(), errOut.String())
	}
	var terminal store.ArtifactFetchOperation
	if err := json.Unmarshal(out.Bytes(), &terminal); err != nil || terminal.State != store.ArtifactFetchSucceeded ||
		terminal.ResultSHA256 == nil || terminal.OperationID != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("terminal=%+v err=%v stdout=%s", terminal, err, out.String())
	}
	if !strings.Contains(errOut.String(), "Artifact fetch（HTTP operator API）") ||
		!strings.Contains(errOut.String(), artifactFetchCLITestDigest("a")) ||
		!strings.Contains(errOut.String(), artifact.ProductionRegistryOrigin) {
		t.Fatalf("preview impact missing: %s", errOut.String())
	}
	for _, raw := range []string{out.String(), errOut.String()} {
		for _, forbidden := range []string{privateReason, privateKey, privateURL, "run_token", "tarball_url"} {
			if strings.Contains(raw, forbidden) {
				t.Fatalf("CLI output exposed %q: %s", forbidden, raw)
			}
		}
	}
	if _, err := os.Stat(missingDB); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("HTTP fetch touched default DB: %v", err)
	}
	mu.Lock()
	gotCalls := append([]string(nil), calls...)
	mu.Unlock()
	wantCalls := []string{
		"POST /v1/operator/artifact-fetches/preview",
		"POST /v1/operator/artifact-fetches",
		"GET /v1/operator/artifact-fetches/0123456789abcdef0123456789abcdef",
		"GET /v1/operator/artifact-fetches/0123456789abcdef0123456789abcdef",
	}
	if fmt.Sprint(gotCalls) != fmt.Sprint(wantCalls) {
		t.Fatalf("calls=%v want=%v", gotCalls, wantCalls)
	}
}

func TestArtifactFetchCLIRejectsManualPreviewDigestBeforeIO(t *testing.T) {
	var calls atomic.Int32
	deps := machineCommandDeps{
		discoverHubURL: func() (string, error) {
			calls.Add(1)
			return "", errors.New("must not discover")
		},
		newOperatorClient: func(string) (*operatorclient.Client, error) {
			calls.Add(1)
			return nil, errors.New("must not create client")
		},
		validateDirectPath: func(string) error {
			calls.Add(1)
			return errors.New("must not inspect DB")
		},
	}
	var out, errOut bytes.Buffer
	err := runArtifactFetchCommandWithDeps(t.Context(), []string{
		"openclaw@2026.9.8", "--confirm-version", "2026.9.8", "--reason", "same request",
		"--idempotency-key", "same-key", "--preview-digest", artifactFetchCLITestDigest("a"),
	}, &out, &errOut, deps)
	if err == nil || calls.Load() != 0 || out.Len() != 0 {
		t.Fatalf("manual digest err=%v calls=%d stdout=%q", err, calls.Load(), out.String())
	}
}

func TestArtifactFetchCLIAmbiguousRecoveryReceiptReplaysExactHTTPCreate(t *testing.T) {
	const (
		reason = "PRIVATE_RECOVERY_REASON"
		key    = "PRIVATE_RECOVERY_KEY"
	)
	var createCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		artifactFetchCLIResponseHeaders(w)
		switch r.URL.Path {
		case "/v1/operator/artifact-fetches/preview":
			_ = json.NewEncoder(w).Encode(artifactFetchCLITestPreview())
		case "/v1/operator/artifact-fetches":
			if r.Header.Get("Idempotency-Key") != key {
				t.Errorf("recovery key=%q", r.Header.Get("Idempotency-Key"))
			}
			var body struct {
				Reason string `json:"reason"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Reason != reason {
				t.Errorf("recovery body=%+v err=%v", body, err)
			}
			if createCalls.Add(1) == 1 {
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write([]byte(`{"code":"UNAVAILABLE","detail":"not inspected"}`))
				return
			}
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(operator.ArtifactFetchApplyResult{
				Operation: artifactFetchCLITestOperation(store.ArtifactFetchQueued),
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	base, deps := machineHTTPTestDeps(t, server)
	deps.discoverHubURL = func() (string, error) { return base, nil }
	recoveryDir := t.TempDir()
	if err := os.Chmod(recoveryDir, 0o700); err != nil {
		t.Fatal(err)
	}
	recoveryPath := filepath.Join(recoveryDir, "artifact-fetch-recovery.json")

	var firstOut, firstErr bytes.Buffer
	err := runArtifactFetchCommandWithDeps(t.Context(), []string{
		"openclaw@2026.9.8", "--confirm-version", "2026.9.8", "--reason", reason,
		"--idempotency-key", key, "--recovery-file", recoveryPath,
	}, &firstOut, &firstErr, deps)
	if err == nil || firstOut.Len() != 0 || !strings.Contains(firstErr.String(), "recovery_file") {
		t.Fatalf("first attempt err=%v stdout=%s stderr=%s", err, firstOut.String(), firstErr.String())
	}
	info, err := os.Lstat(recoveryPath)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("recovery receipt stat=%v err=%v", info, err)
	}
	raw, err := os.ReadFile(recoveryPath)
	if err != nil || !bytes.Contains(raw, []byte(reason)) || !bytes.Contains(raw, []byte(key)) {
		t.Fatalf("recovery receipt did not retain exact private request err=%v raw=%s", err, raw)
	}
	for _, output := range []string{firstOut.String(), firstErr.String()} {
		if strings.Contains(output, reason) || strings.Contains(output, key) {
			t.Fatalf("recovery coordinates leaked to terminal: %s", output)
		}
	}

	var replayOut, replayErr bytes.Buffer
	if err := runArtifactFetchCommandWithDeps(t.Context(), []string{
		"--recovery-file", recoveryPath, "--json",
	}, &replayOut, &replayErr, deps); err != nil {
		t.Fatalf("recovery replay: %v; stdout=%s stderr=%s", err, replayOut.String(), replayErr.String())
	}
	var apply operator.ArtifactFetchApplyResult
	if err := json.Unmarshal(replayOut.Bytes(), &apply); err != nil ||
		apply.Operation.State != store.ArtifactFetchQueued || createCalls.Load() != 2 {
		t.Fatalf("apply=%+v err=%v calls=%d", apply, err, createCalls.Load())
	}
	if _, err := os.Lstat(recoveryPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("confirmed enqueue did not remove recovery receipt: %v", err)
	}
	for _, output := range []string{replayOut.String(), replayErr.String()} {
		if strings.Contains(output, reason) || strings.Contains(output, key) {
			t.Fatalf("replay leaked private receipt: %s", output)
		}
	}
}

func TestArtifactFetchCLIDefinitiveRejectionRemovesRecoveryReceipt(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		artifactFetchCLIResponseHeaders(w)
		switch r.URL.Path {
		case "/v1/operator/artifact-fetches/preview":
			_ = json.NewEncoder(w).Encode(artifactFetchCLITestPreview())
		case "/v1/operator/artifact-fetches":
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"code":"ARTIFACT_FETCH_INVALID","message":"request rejected"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	base, deps := machineHTTPTestDeps(t, server)
	deps.discoverHubURL = func() (string, error) { return base, nil }
	recoveryDir := t.TempDir()
	if err := os.Chmod(recoveryDir, 0o700); err != nil {
		t.Fatal(err)
	}
	recoveryPath := filepath.Join(recoveryDir, "artifact-fetch-rejection.json")

	var out, errOut bytes.Buffer
	err := runArtifactFetchCommandWithDeps(t.Context(), []string{
		"openclaw@2026.9.8", "--confirm-version", "2026.9.8", "--reason", "intake release",
		"--idempotency-key", "definitive-rejection", "--recovery-file", recoveryPath,
	}, &out, &errOut, deps)
	var apiErr *operatorclient.APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusBadRequest ||
		apiErr.Code != "ARTIFACT_FETCH_INVALID" || out.Len() != 0 {
		t.Fatalf("rejection err=%v api=%+v stdout=%q", err, apiErr, out.String())
	}
	if _, err := os.Lstat(recoveryPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("definitive rejection retained recovery receipt: %v", err)
	}
}

func TestArtifactFetchCLIListAndShowUseOperationRoutes(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		artifactFetchCLIResponseHeaders(w)
		mu.Lock()
		calls = append(calls, r.Method+" "+r.URL.RequestURI())
		mu.Unlock()
		if r.URL.Path == "/v1/operator/artifact-fetches" {
			_ = json.NewEncoder(w).Encode(store.ArtifactFetchListResult{
				SchemaVersion: store.ArtifactFetchReadSchemaVersion,
				Consistency:   store.ArtifactFetchReadConsistency, EvaluatedAt: artifactFetchCLITestTime().Add(time.Minute),
				Total: 1, Items: []store.ArtifactFetchOperation{artifactFetchCLITestOperation(store.ArtifactFetchSucceeded)},
			})
			return
		}
		if r.URL.Path == "/v1/operator/artifact-fetches/0123456789abcdef0123456789abcdef" {
			_ = json.NewEncoder(w).Encode(artifactFetchCLITestOperation(store.ArtifactFetchSucceeded))
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	base, deps := machineHTTPTestDeps(t, server)
	deps.discoverHubURL = func() (string, error) { return base, nil }

	var listOut, showOut, errOut bytes.Buffer
	if err := runArtifactFetchCommandWithDeps(t.Context(), []string{
		"list", "--state", "succeeded", "--name", "openclaw", "--version", "2026.9.8", "--limit", "7", "--json",
	}, &listOut, &errOut, deps); err != nil {
		t.Fatalf("operation list: %v", err)
	}
	if err := runArtifactFetchCommandWithDeps(t.Context(), []string{
		"show", "0123456789abcdef0123456789abcdef", "--hub-url", base, "--json",
	}, &showOut, &errOut, deps); err != nil {
		t.Fatalf("operation show: %v", err)
	}
	var list store.ArtifactFetchListResult
	var detail store.ArtifactFetchOperation
	if err := json.Unmarshal(listOut.Bytes(), &list); err != nil || len(list.Items) != 1 {
		t.Fatalf("list=%+v err=%v", list, err)
	}
	if err := json.Unmarshal(showOut.Bytes(), &detail); err != nil || detail.State != store.ArtifactFetchSucceeded {
		t.Fatalf("detail=%+v err=%v", detail, err)
	}
	mu.Lock()
	got := append([]string(nil), calls...)
	mu.Unlock()
	want := []string{
		"GET /v1/operator/artifact-fetches?limit=7&name=openclaw&state=succeeded&version=2026.9.8",
		"GET /v1/operator/artifact-fetches/0123456789abcdef0123456789abcdef",
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("calls=%v want=%v", got, want)
	}
}

func TestArtifactFetchCLIRejectsUnsafeOrIncompleteRequestsBeforeIO(t *testing.T) {
	var calls atomic.Int32
	deps := machineCommandDeps{
		discoverHubURL: func() (string, error) {
			calls.Add(1)
			return "", errors.New("must not discover")
		},
		newOperatorClient: func(string) (*operatorclient.Client, error) {
			calls.Add(1)
			return nil, errors.New("must not create client")
		},
		validateDirectPath: func(string) error {
			calls.Add(1)
			return errors.New("must not inspect DB")
		},
	}
	valid := []string{
		"openclaw@2026.9.8", "--confirm-version", "2026.9.8", "--reason", "intake release",
		"--idempotency-key", "fetch-key",
	}
	for _, test := range []struct {
		name string
		args []string
	}{
		{name: "arbitrary registry", args: append(append([]string{}, valid...), "--registry", "https://evil.example")},
		{name: "arbitrary max", args: append(append([]string{}, valid...), "--max-bytes", "10")},
		{name: "spoofed actor", args: append(append([]string{}, valid...), "--by", "operator")},
		{name: "floating version", args: []string{"openclaw@latest", "--confirm-version", "latest", "--reason", "x", "--idempotency-key", "k"}},
		{name: "wrong package", args: []string{"other@1.2.3", "--confirm-version", "1.2.3", "--reason", "x", "--idempotency-key", "k"}},
		{name: "missing confirmation", args: []string{"openclaw@2026.9.8", "--reason", "x", "--idempotency-key", "k"}},
		{name: "wrong confirmation", args: []string{"openclaw@2026.9.8", "--confirm-version", "2026.9.9", "--reason", "x", "--idempotency-key", "k"}},
		{name: "missing reason", args: []string{"openclaw@2026.9.8", "--confirm-version", "2026.9.8", "--idempotency-key", "k"}},
		{name: "empty reason", args: []string{"openclaw@2026.9.8", "--confirm-version", "2026.9.8", "--reason=", "--idempotency-key", "k"}},
		{name: "missing key", args: []string{"openclaw@2026.9.8", "--confirm-version", "2026.9.8", "--reason", "x"}},
		{name: "bad digest", args: []string{"openclaw@2026.9.8", "--confirm-version", "2026.9.8", "--reason", "x", "--idempotency-key", "k", "--preview-digest", "sha256:ABC"}},
		{name: "mixed transport", args: append(append([]string{}, valid...), "--hub-url", "http://100.64.0.9:8787", "--db", "/tmp/no")},
		{name: "duplicate key", args: append(append([]string{}, valid...), "--idempotency-key", "other")},
		{name: "preview with mutation", args: []string{"openclaw@2026.9.8", "--preview", "--reason", "x"}},
		{name: "poll without wait", args: append(append([]string{}, valid...), "--poll-interval", "1s")},
		{name: "invalid list state", args: []string{"list", "--state", "done"}},
		{name: "show path", args: []string{"show", "../private"}},
		{name: "show list filter", args: []string{"show", "op", "--state", "failed"}},
		{name: "unknown", args: []string{"openclaw@2026.9.8", "--raw"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			if err := runArtifactFetchCommandWithDeps(t.Context(), test.args, &out, &errOut, deps); err == nil {
				t.Fatalf("invalid args %q were accepted", test.args)
			}
			if out.Len() != 0 {
				t.Fatalf("invalid args wrote stdout=%q", out.String())
			}
		})
	}
	if calls.Load() != 0 {
		t.Fatalf("invalid artifact fetch inputs reached I/O %d times", calls.Load())
	}
}

func TestArtifactFetchCLIDiscoveryFailureNeverFallsBackToDB(t *testing.T) {
	var directCalls atomic.Int32
	deps := machineCommandDeps{
		discoverHubURL: func() (string, error) { return "", errors.New("not configured") },
		validateDirectPath: func(string) error {
			directCalls.Add(1)
			return nil
		},
	}
	var out, errOut bytes.Buffer
	err := runArtifactFetchCommandWithDeps(t.Context(), []string{
		"openclaw@2026.9.8", "--confirm-version", "2026.9.8", "--reason", "release",
		"--idempotency-key", "key",
	}, &out, &errOut, deps)
	if err == nil || !strings.Contains(err.Error(), "not configured") || directCalls.Load() != 0 {
		t.Fatalf("discovery err=%v direct_calls=%d", err, directCalls.Load())
	}
}

func TestArtifactFetchCLIParsesNodeRuntimeAndWritesGenericSource(t *testing.T) {
	name, version, err := parseArtifactFetchCLITarget("claude-code@2.1.278")
	if err != nil || name != "claude-code" || version != "2.1.278" {
		t.Fatalf("claude target=%s@%s err=%v", name, version, err)
	}
	if _, _, err := parseArtifactFetchCLITarget("claude-code@latest"); err == nil {
		t.Fatal("accepted claude-code@latest")
	}
	name, version, err = parseArtifactFetchCLITarget("codex@0.155.1")
	if err != nil || name != "codex" || version != "0.155.1" {
		t.Fatalf("codex target=%s@%s err=%v", name, version, err)
	}
	if _, _, err := parseArtifactFetchCLITarget("codex@latest"); err == nil {
		t.Fatal("accepted codex@latest")
	}
	name, version, err = parseArtifactFetchCLITarget("grok@1.0.40")
	if err != nil || name != "grok" || version != "1.0.40" {
		t.Fatalf("grok target=%s@%s err=%v", name, version, err)
	}
	for _, target := range []string{"grok@latest", "grok@1.0", "grok@1.0.40-alpha.1", "grok@1.0.40-beta.1"} {
		if _, _, err := parseArtifactFetchCLITarget(target); err == nil {
			t.Fatalf("accepted grok target %q", target)
		}
	}
	name, version, err = parseArtifactFetchCLITarget("bat-server@3.2.10")
	if err != nil || name != "bat-server" || version != "3.2.10" {
		t.Fatalf("bat-server target=%s@%s err=%v", name, version, err)
	}
	for _, target := range []string{"bat-server@latest", "bat-server@v3.2.10", "bat-server@3.2.11-pre.4", "bat-server@3.2"} {
		if _, _, err := parseArtifactFetchCLITarget(target); err == nil {
			t.Fatalf("accepted bat-server target %q", target)
		}
	}
	name, version, err = parseArtifactFetchCLITarget("node-runtime@24.21.0")
	if err != nil || name != "node-runtime" || version != "24.21.0" {
		t.Fatalf("target=%s@%s err=%v", name, version, err)
	}
	for _, target := range []string{"node-runtime@v24.21.0", "node-runtime@24.21.0-rc.1", "node-runtime@24.21"} {
		if _, _, err := parseArtifactFetchCLITarget(target); err == nil {
			t.Fatalf("accepted node target %q", target)
		}
	}

	preview := artifactFetchCLITestPreview()
	preview.Name = "node-runtime"
	preview.Version = "24.21.0"
	preview.SourceKind = artifact.ArtifactSourceNode
	preview.RegistryOrigin = artifact.ProductionNodeDistributionOrigin
	preview.PolicyVersion = artifact.NodeRuntimeFetchPolicyVersion
	preview.EnginesNode = nil
	var out bytes.Buffer
	if err := writeArtifactFetchPreview(&out, preview, false, "test"); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	for _, want := range []string{
		`target: "node-runtime"@"24.21.0"`,
		`source kind: "` + artifact.ArtifactSourceNode + `"`,
		`source origin: "` + artifact.ProductionNodeDistributionOrigin + `"`,
		`source integrity: "` + preview.SHA512Integrity + `"`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("preview=%q missing %q", text, want)
		}
	}
	if strings.Contains(text, "registry origin") || strings.Contains(text, "sha512 integrity") {
		t.Fatalf("preview uses package-specific source labels: %q", text)
	}
	document := artifactFetchRecoveryFromInput(artifactFetchCLIInput{
		name: "node-runtime", version: "24.21.0", confirmVersion: "24.21.0",
		reason: "managed runtime", idempotencyKey: "node-runtime-test",
		previewDigest: preview.PreviewDigest,
	}, "http://100.64.200.2:8787")
	raw, err := marshalArtifactFetchRecovery(document)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeArtifactFetchRecovery(raw)
	if err != nil || decoded != document {
		t.Fatalf("decoded node recovery=%+v err=%v", decoded, err)
	}
}

func TestArtifactFetchCLIParsesHermesExactVersion(t *testing.T) {
	name, version, err := parseArtifactFetchCLITarget("hermes-agent@2026.9.7")
	if err != nil || name != "hermes-agent" || version != "2026.9.7" {
		t.Fatalf("target=%s@%s err=%v", name, version, err)
	}
	for _, target := range []string{"hermes-agent@v2026.9.7", "hermes-agent@latest", "hermes-agent@2026.9"} {
		if _, _, err := parseArtifactFetchCLITarget(target); err == nil {
			t.Fatalf("accepted Hermes target %q", target)
		}
	}
}

func TestWaitForArtifactFetchTerminalRejectsIdentitySwapAndCancellation(t *testing.T) {
	queued := artifactFetchCLITestOperation(store.ArtifactFetchQueued)
	swapped := artifactFetchCLITestOperation(store.ArtifactFetchRunning)
	swapped.IdentityDigest = artifactFetchCLITestDigest("d")
	if _, err := waitForArtifactFetchTerminal(t.Context(), queued, time.Millisecond,
		func(string) (store.ArtifactFetchOperation, error) { return swapped, nil }); err == nil {
		t.Fatal("poll accepted swapped operation identity")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := waitForArtifactFetchTerminal(ctx, queued, time.Millisecond,
		func(string) (store.ArtifactFetchOperation, error) { return queued, nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel err=%v", err)
	}
}
