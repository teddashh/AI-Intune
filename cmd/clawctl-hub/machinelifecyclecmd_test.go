package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/ledgerlock"
	"github.com/teddashh/AI-Intune/internal/operatorclient"
	"github.com/teddashh/AI-Intune/internal/store"
)

func lifecycleRetryCoordinates(t *testing.T, stderr string) (key, revision, digest string) {
	t.Helper()
	for _, field := range strings.Fields(stderr) {
		switch {
		case strings.HasPrefix(field, "idempotency-key="):
			key = strings.TrimPrefix(field, "idempotency-key=")
		case strings.HasPrefix(field, "expected-revision="):
			revision = strings.TrimPrefix(field, "expected-revision=")
		case strings.HasPrefix(field, "preview-digest="):
			digest = strings.TrimPrefix(field, "preview-digest=")
		}
	}
	if !strings.HasPrefix(key, "cli-machine-lifecycle-") || revision == "" ||
		!strings.HasPrefix(digest, "sha256:") {
		t.Fatalf("private retry coordinates missing or malformed: %q", stderr)
	}
	return key, revision, digest
}

func insertMachineLifecycleCLIOpenSession(t *testing.T, st *store.Store, machineID, sessionID string) {
	t.Helper()
	if _, err := st.DB().Exec(`INSERT INTO agent_sessions
		(session_id,machine_id,operator_tailnet_user_id,operator_tailnet_user_login,opened_at)
		VALUES (?,?,?,?,?)`, sessionID, machineID, "cli-user", "cli@example.com", time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
}

func TestMachineLifecycleCLIHTTPReadPreviewApplyAndExplicitRetry(t *testing.T) {
	f := observedOperatorFixture(t)
	var mu sync.Mutex
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls = append(calls, r.Method+" "+r.URL.Path)
		mu.Unlock()
		f.mux.ServeHTTP(w, r)
	}))
	defer server.Close()
	base, deps := machineHTTPTestDeps(t, server)
	missingDB := filepath.Join(t.TempDir(), "must-not-be-created.sqlite")
	t.Setenv("CLAWCTL_DB", missingDB)
	lifecyclePath := "/v1/operator/machines/" + f.machine.id + "/lifecycle"

	var readOut, readErr bytes.Buffer
	if err := runMachineCommandWithDeps(t.Context(), []string{
		"lifecycle", "--hub-url", base, "--machine", f.machine.id,
	}, &readOut, &readErr, deps); err != nil {
		t.Fatalf("HTTP lifecycle read: %v; stderr=%s", err, readErr.String())
	}
	for _, want := range []string{"HTTP operator API", "cnode-operator", "lifecycle=active", "revision=0", "ETag="} {
		if !strings.Contains(readOut.String(), want) {
			t.Fatalf("HTTP lifecycle read missing %q: %q", want, readOut.String())
		}
	}
	if strings.Contains(readOut.String(), "expected=") {
		t.Fatalf("HTTP lifecycle read exposed legacy expected: %q", readOut.String())
	}
	if readErr.Len() != 0 {
		t.Fatalf("HTTP lifecycle read wrote stderr: %q", readErr.String())
	}
	insertMachineLifecycleCLIOpenSession(t, f.store, f.machine.id, "cli-preview-open")

	var previewOut, previewErr bytes.Buffer
	if err := runMachineCommandWithDeps(t.Context(), []string{
		"lifecycle", "--hub-url", base, "--machine", f.machine.id,
		"--set", "retired", "--preview",
	}, &previewOut, &previewErr, deps); err != nil {
		t.Fatalf("HTTP lifecycle preview: %v; stderr=%s", err, previewErr.String())
	}
	for _, want := range []string{"HTTP operator API preview", "active → retired", "lifecycle-revision=0", "terminal sessions currently open=1", "blockers=none", "preview-digest=sha256:"} {
		if !strings.Contains(previewOut.String(), want) {
			t.Fatalf("HTTP lifecycle preview missing %q: %q", want, previewOut.String())
		}
	}
	if previewErr.Len() != 0 {
		t.Fatalf("preview exposed apply retry coordinates: %q", previewErr.String())
	}
	machine, err := f.store.GetMachine(f.machine.id)
	if err != nil || machine.RetiredAt != nil || machine.LifecycleRevision != 0 {
		t.Fatalf("preview changed lifecycle: machine=%+v err=%v", machine, err)
	}

	var applyOut, applyErr bytes.Buffer
	applyArgs := []string{
		"lifecycle", "--hub-url", base, "--machine", f.machine.id,
		"--set", "retired", "--confirm-name", "cnode-operator", "--reason", "planned CLI retirement",
	}
	if err := runMachineCommandWithDeps(t.Context(), applyArgs, &applyOut, &applyErr, deps); err != nil {
		t.Fatalf("HTTP lifecycle apply: %v; stderr=%s", err, applyErr.String())
	}
	if !strings.Contains(applyErr.String(), "terminal sessions currently open: 1；retiring ends any still open when applied.") {
		t.Fatalf("HTTP lifecycle apply omitted terminal-session notice: %q", applyErr.String())
	}
	for _, want := range []string{"HTTP operator API", "active → retired", "revision=1", "ETag=", "changed=true", "no-op=false", "replayed=false"} {
		if !strings.Contains(applyOut.String(), want) {
			t.Fatalf("HTTP lifecycle apply missing %q: %q", want, applyOut.String())
		}
	}
	key, revision, digest := lifecycleRetryCoordinates(t, applyErr.String())
	if revision != "0" {
		t.Fatalf("fresh apply retry revision=%q, want 0", revision)
	}

	var retryOut, retryErr bytes.Buffer
	retryArgs := append(append([]string(nil), applyArgs...),
		"--idempotency-key", key, "--expected-revision", revision, "--preview-digest", digest)
	if err := runMachineCommandWithDeps(t.Context(), retryArgs, &retryOut, &retryErr, deps); err != nil {
		t.Fatalf("HTTP lifecycle retry: %v; stderr=%s", err, retryErr.String())
	}
	if !strings.Contains(retryOut.String(), "replayed=true") || !strings.Contains(retryOut.String(), "revision=1") {
		t.Fatalf("HTTP lifecycle retry did not expose immutable replay: %q", retryOut.String())
	}
	retryKey, retryRevision, retryDigest := lifecycleRetryCoordinates(t, retryErr.String())
	if retryKey != key || retryRevision != revision || retryDigest != digest {
		t.Fatalf("retry coordinates changed: first=%q/%q/%q retry=%q/%q/%q",
			key, revision, digest, retryKey, retryRevision, retryDigest)
	}

	machine, err = f.store.GetMachine(f.machine.id)
	if err != nil || machine.RetiredAt == nil || machine.LifecycleRevision != 1 {
		t.Fatalf("HTTP apply/retry projection=%+v err=%v", machine, err)
	}
	var ledgerRows int
	if err := f.store.DB().QueryRow(
		`SELECT COUNT(*) FROM operator_idempotency WHERE idempotency_key=?`, key,
	).Scan(&ledgerRows); err != nil || ledgerRows != 1 {
		t.Fatalf("HTTP apply/retry ledger rows=%d err=%v", ledgerRows, err)
	}
	entries, err := f.store.Audit(f.machine.id, 10)
	if err != nil || len(entries) != 2 {
		t.Fatalf("HTTP apply/retry audit=%+v err=%v", entries, err)
	}
	for _, entry := range entries {
		if entry.Action != store.AuditMachineLifecycle || !entry.OK ||
			entry.UserAgent != operatorclient.UserAgent || entry.SourceKind != "operator-api" {
			t.Fatalf("HTTP lifecycle CLI audit provenance=%+v", entry)
		}
	}

	wantCalls := []string{
		"GET " + lifecyclePath,
		"GET " + lifecyclePath,
		"POST " + lifecyclePath + "-preview",
		"GET " + lifecyclePath,
		"POST " + lifecyclePath + "-preview",
		"PUT " + lifecyclePath,
		"PUT " + lifecyclePath,
		"GET " + lifecyclePath,
	}
	mu.Lock()
	gotCalls := append([]string(nil), calls...)
	mu.Unlock()
	if strings.Join(gotCalls, "\n") != strings.Join(wantCalls, "\n") {
		t.Fatalf("HTTP lifecycle CLI calls=%v, want %v", gotCalls, wantCalls)
	}
	if _, err := os.Stat(missingDB); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("HTTP lifecycle CLI touched fallback DB: %v", err)
	}
}

func TestMachineLifecycleCLIApplyOmitsTerminalSessionNoticeForZeroCountAndRestore(t *testing.T) {
	tests := []struct {
		name    string
		desired store.MachineLifecycleState
		prepare func(*testing.T, *jobsFixture)
	}{
		{name: "retire with zero open sessions", desired: store.MachineLifecycleRetired},
		{
			name: "restore with an open session", desired: store.MachineLifecycleActive,
			prepare: func(t *testing.T, f *jobsFixture) {
				if err := f.store.RetireMachine(f.machine.id, time.Now().UTC()); err != nil {
					t.Fatal(err)
				}
				insertMachineLifecycleCLIOpenSession(t, f.store, f.machine.id, "cli-restore-open")
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			f := observedOperatorFixture(t)
			if test.prepare != nil {
				test.prepare(t, &f)
			}
			server := httptest.NewServer(f.mux)
			defer server.Close()
			base, deps := machineHTTPTestDeps(t, server)
			var out, errOut bytes.Buffer
			err := runMachineCommandWithDeps(t.Context(), []string{
				"lifecycle", "--hub-url", base, "--machine", f.machine.id,
				"--set", string(test.desired), "--confirm-name", "cnode-operator", "--reason", "notice condition",
			}, &out, &errOut, deps)
			if err != nil {
				t.Fatalf("lifecycle apply: %v; stderr=%s", err, errOut.String())
			}
			if strings.Contains(errOut.String(), "terminal sessions") {
				t.Fatalf("lifecycle apply printed terminal-session notice: %q", errOut.String())
			}
		})
	}
}

func TestMachineLifecycleCLIDiscoversHTTPByDefaultAndNeverTouchesDB(t *testing.T) {
	f := observedOperatorFixture(t)
	server := httptest.NewServer(f.mux)
	defer server.Close()
	base, deps := machineHTTPTestDeps(t, server)
	discoveryCalls := 0
	deps.discoverHubURL = func() (string, error) {
		discoveryCalls++
		return base, nil
	}
	deps.verifyHubStopped = func(context.Context, string) error {
		t.Fatal("default HTTP lifecycle mode checked local Hub state")
		return nil
	}
	deps.openDirectDB = func(string) (*store.Store, error) {
		t.Fatal("default HTTP lifecycle mode opened direct DB")
		return nil, nil
	}
	missingDB := filepath.Join(t.TempDir(), "must-not-be-created.sqlite")
	t.Setenv("CLAWCTL_DB", missingDB)

	var out, errOut bytes.Buffer
	if err := runMachineCommandWithDeps(t.Context(), []string{
		"lifecycle", "--machine", f.machine.id,
	}, &out, &errOut, deps); err != nil {
		t.Fatalf("discovered lifecycle read: %v; stderr=%s", err, errOut.String())
	}
	if discoveryCalls != 1 || !strings.Contains(out.String(), "HTTP operator API") {
		t.Fatalf("default lifecycle discovery calls=%d output=%q", discoveryCalls, out.String())
	}
	if _, err := os.Stat(missingDB); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("default HTTP lifecycle mode touched DB: %v", err)
	}
}

func TestMachineLifecycleCLIFailuresNeverFallBackToDB(t *testing.T) {
	t.Run("discovery", func(t *testing.T) {
		missingDB := filepath.Join(t.TempDir(), "must-not-be-created.sqlite")
		t.Setenv("CLAWCTL_DB", missingDB)
		deps := machineCommandDeps{
			discoverHubURL: func() (string, error) { return "", errors.New("discovery unavailable") },
			newOperatorClient: func(string) (*operatorclient.Client, error) {
				t.Fatal("failed discovery constructed HTTP client")
				return nil, nil
			},
			verifyHubStopped: func(context.Context, string) error {
				t.Fatal("failed discovery fell through to direct DB")
				return nil
			},
			openDirectDB: func(string) (*store.Store, error) {
				t.Fatal("failed discovery opened direct DB")
				return nil, nil
			},
		}
		var out, errOut bytes.Buffer
		err := runMachineCommandWithDeps(t.Context(), []string{
			"lifecycle", "--machine", "machine-id",
		}, &out, &errOut, deps)
		if err == nil || !strings.Contains(err.Error(), "discovery unavailable") {
			t.Fatalf("lifecycle discovery error=%v", err)
		}
		if _, err := os.Stat(missingDB); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("lifecycle discovery failure created DB: %v", err)
		}
	})

	t.Run("HTTP response", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"code":"MAINTENANCE","message":"try later"}`))
		}))
		defer server.Close()
		base, deps := machineHTTPTestDeps(t, server)
		deps.verifyHubStopped = func(context.Context, string) error {
			t.Fatal("HTTP response failure fell through to direct DB")
			return nil
		}
		deps.openDirectDB = func(string) (*store.Store, error) {
			t.Fatal("HTTP response failure opened direct DB")
			return nil, nil
		}
		missingDB := filepath.Join(t.TempDir(), "must-not-be-created.sqlite")
		t.Setenv("CLAWCTL_DB", missingDB)
		var out, errOut bytes.Buffer
		err := runMachineCommandWithDeps(t.Context(), []string{
			"lifecycle", "--hub-url", base, "--machine", "machine-id",
		}, &out, &errOut, deps)
		var apiErr *operatorclient.APIError
		if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusServiceUnavailable ||
			apiErr.Code != "MAINTENANCE" {
			t.Fatalf("lifecycle HTTP failure=%T %v API=%+v", err, err, apiErr)
		}
		if _, err := os.Stat(missingDB); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("lifecycle HTTP failure touched DB: %v", err)
		}
	})
}

func TestMachineLifecycleCLIStopsAtPreviewBlocker(t *testing.T) {
	f := observedOperatorFixture(t)
	f.newJob(t, "sha256:a")
	var mu sync.Mutex
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls = append(calls, r.Method+" "+r.URL.Path)
		mu.Unlock()
		f.mux.ServeHTTP(w, r)
	}))
	defer server.Close()
	base, deps := machineHTTPTestDeps(t, server)

	var out, errOut bytes.Buffer
	err := runMachineCommandWithDeps(t.Context(), []string{
		"lifecycle", "--hub-url", base, "--machine", f.machine.id,
		"--set", "retired", "--confirm-name", "cnode-operator", "--reason", "blocked CLI retire",
	}, &out, &errOut, deps)
	if err == nil || !strings.Contains(err.Error(), "blocker") || !strings.Contains(err.Error(), "nonterminal_jobs") {
		t.Fatalf("lifecycle blocker error=%v", err)
	}
	if out.Len() != 0 || errOut.Len() != 0 {
		t.Fatalf("blocked lifecycle printed success/private coordinates: out=%q stderr=%q", out.String(), errOut.String())
	}
	machine, getErr := f.store.GetMachine(f.machine.id)
	if getErr != nil || machine.RetiredAt != nil || machine.LifecycleRevision != 0 {
		t.Fatalf("blocked lifecycle changed machine=%+v err=%v", machine, getErr)
	}
	var ledgerRows, auditRows int
	if err := f.store.DB().QueryRow(`SELECT COUNT(*) FROM operator_idempotency`).Scan(&ledgerRows); err != nil {
		t.Fatal(err)
	}
	if err := f.store.DB().QueryRow(`SELECT COUNT(*) FROM audit_log`).Scan(&auditRows); err != nil {
		t.Fatal(err)
	}
	if ledgerRows != 0 || auditRows != 0 {
		t.Fatalf("CLI sent blocked apply: idempotency=%d audit=%d", ledgerRows, auditRows)
	}
	wantPath := "/v1/operator/machines/" + f.machine.id + "/lifecycle"
	mu.Lock()
	gotCalls := append([]string(nil), calls...)
	mu.Unlock()
	wantCalls := []string{"GET " + wantPath, "POST " + wantPath + "-preview"}
	if strings.Join(gotCalls, "\n") != strings.Join(wantCalls, "\n") {
		t.Fatalf("blocked lifecycle calls=%v, want %v", gotCalls, wantCalls)
	}
}

func TestMachineLifecycleCLIHistoricalReplayReadsAuthoritativeCurrentState(t *testing.T) {
	f := observedOperatorFixture(t)
	var mu sync.Mutex
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls = append(calls, r.Method+" "+r.URL.Path)
		mu.Unlock()
		f.mux.ServeHTTP(w, r)
	}))
	defer server.Close()
	base, deps := machineHTTPTestDeps(t, server)
	baseArgs := []string{
		"lifecycle", "--hub-url", base, "--machine", f.machine.id,
		"--set", "retired", "--confirm-name", "cnode-operator", "--reason", "historical replay test",
	}
	var firstOut, firstErr bytes.Buffer
	if err := runMachineCommandWithDeps(t.Context(), baseArgs, &firstOut, &firstErr, deps); err != nil {
		t.Fatalf("initial lifecycle retire: %v; stderr=%s", err, firstErr.String())
	}
	key, revision, digest := lifecycleRetryCoordinates(t, firstErr.String())
	machine, err := f.store.GetMachine(f.machine.id)
	if err != nil || machine.RetiredAt == nil || machine.LifecycleRevision != 1 {
		t.Fatalf("initial lifecycle retire projection=%+v err=%v", machine, err)
	}
	if err := f.store.UnretireMachine(f.machine.id, machine.RetiredAt.Add(time.Second)); err != nil {
		t.Fatalf("establish later authoritative restore: %v", err)
	}
	machine, err = f.store.GetMachine(f.machine.id)
	if err != nil || machine.RetiredAt != nil || machine.LifecycleRevision != 2 {
		t.Fatalf("later authoritative restore projection=%+v err=%v", machine, err)
	}

	mu.Lock()
	calls = nil
	mu.Unlock()
	var replayOut, replayErr bytes.Buffer
	retryArgs := append(append([]string(nil), baseArgs...),
		"--idempotency-key", key, "--expected-revision", revision, "--preview-digest", digest)
	if err := runMachineCommandWithDeps(t.Context(), retryArgs, &replayOut, &replayErr, deps); err != nil {
		t.Fatalf("historical lifecycle replay: %v; stderr=%s", err, replayErr.String())
	}
	for _, want := range []string{
		"HTTP operator API historical replay receipt",
		"replayed=true",
		"HTTP operator API authoritative current:",
		"lifecycle=active",
		"revision=2",
	} {
		if !strings.Contains(replayOut.String(), want) {
			t.Fatalf("historical replay output missing %q: %q", want, replayOut.String())
		}
	}
	path := "/v1/operator/machines/" + f.machine.id + "/lifecycle"
	mu.Lock()
	gotCalls := append([]string(nil), calls...)
	mu.Unlock()
	wantCalls := []string{"PUT " + path, "GET " + path}
	if strings.Join(gotCalls, "\n") != strings.Join(wantCalls, "\n") {
		t.Fatalf("historical replay calls=%v, want receipt then authoritative GET %v", gotCalls, wantCalls)
	}
	machine, err = f.store.GetMachine(f.machine.id)
	if err != nil || machine.RetiredAt != nil || machine.LifecycleRevision != 2 {
		t.Fatalf("historical replay re-applied old desired state: machine=%+v err=%v", machine, err)
	}
}

func TestMachineLifecycleCLIJSONReplayIncludesAuthoritativeCurrent(t *testing.T) {
	f := observedOperatorFixture(t)
	server := httptest.NewServer(f.mux)
	defer server.Close()
	base, deps := machineHTTPTestDeps(t, server)
	baseArgs := []string{
		"lifecycle", "--hub-url", base, "--machine", f.machine.id,
		"--set", "retired", "--confirm-name", "cnode-operator", "--reason", "JSON historical replay",
	}
	var firstOut, firstErr bytes.Buffer
	if err := runMachineCommandWithDeps(t.Context(), baseArgs, &firstOut, &firstErr, deps); err != nil {
		t.Fatalf("initial JSON replay retire: %v; stderr=%s", err, firstErr.String())
	}
	key, revision, digest := lifecycleRetryCoordinates(t, firstErr.String())
	machine, err := f.store.GetMachine(f.machine.id)
	if err != nil || machine.RetiredAt == nil {
		t.Fatalf("initial JSON replay projection=%+v err=%v", machine, err)
	}
	if err := f.store.UnretireMachine(f.machine.id, machine.RetiredAt.Add(time.Second)); err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	retryArgs := append(append([]string(nil), baseArgs...),
		"--idempotency-key", key, "--expected-revision", revision, "--preview-digest", digest, "--json")
	if err := runMachineCommandWithDeps(t.Context(), retryArgs, &out, &errOut, deps); err != nil {
		t.Fatalf("JSON historical replay: %v; stderr=%s", err, errOut.String())
	}
	var result struct {
		State                store.MachineLifecycleState `json:"state"`
		LifecycleRevision    int64                       `json:"lifecycle_revision"`
		Replayed             bool                        `json:"replayed"`
		AuthoritativeCurrent *struct {
			State             store.MachineLifecycleState `json:"state"`
			LifecycleRevision int64                       `json:"lifecycle_revision"`
		} `json:"authoritative_current"`
	}
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatalf("decode lifecycle JSON replay: %v; output=%s", err, out.String())
	}
	if result.State != store.MachineLifecycleRetired || result.LifecycleRevision != 1 || !result.Replayed ||
		result.AuthoritativeCurrent == nil || result.AuthoritativeCurrent.State != store.MachineLifecycleActive ||
		result.AuthoritativeCurrent.LifecycleRevision != 2 {
		t.Fatalf("JSON replay confused receipt with current projection: %+v output=%s", result, out.String())
	}
}

func TestMachineLifecycleCLIReplayedRejectionReadsAuthoritativeCurrent(t *testing.T) {
	f := observedOperatorFixture(t)
	var mu sync.Mutex
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls = append(calls, r.Method+" "+r.URL.Path)
		mu.Unlock()
		f.mux.ServeHTTP(w, r)
	}))
	defer server.Close()
	base, deps := machineHTTPTestDeps(t, server)
	baseArgs := []string{
		"lifecycle", "--hub-url", base, "--machine", f.machine.id,
		"--set", "retired", "--confirm-name", "wrong-name", "--reason", "replayed rejection",
	}
	var firstOut, firstErr bytes.Buffer
	err := runMachineCommandWithDeps(t.Context(), baseArgs, &firstOut, &firstErr, deps)
	var firstAPI *operatorclient.APIError
	if !errors.As(err, &firstAPI) || firstAPI.Code != store.OperatorCodeConfirmationMismatch || firstAPI.Replayed {
		t.Fatalf("fresh lifecycle rejection=%T %v API=%+v", err, err, firstAPI)
	}
	if firstOut.Len() != 0 || strings.Contains(firstErr.String(), "authoritative current") {
		t.Fatalf("fresh rejection was presented as replay: out=%q stderr=%q", firstOut.String(), firstErr.String())
	}
	key, revision, digest := lifecycleRetryCoordinates(t, firstErr.String())
	if err := f.store.RetireMachine(f.machine.id, time.Now().UTC()); err != nil {
		t.Fatalf("establish later lifecycle state: %v", err)
	}

	mu.Lock()
	calls = nil
	mu.Unlock()
	var replayOut, replayErr bytes.Buffer
	retryArgs := append(append([]string(nil), baseArgs...),
		"--idempotency-key", key, "--expected-revision", revision, "--preview-digest", digest)
	err = runMachineCommandWithDeps(t.Context(), retryArgs, &replayOut, &replayErr, deps)
	var replayAPI *operatorclient.APIError
	if !errors.As(err, &replayAPI) || replayAPI.Code != store.OperatorCodeConfirmationMismatch || !replayAPI.Replayed ||
		!strings.Contains(err.Error(), "這是原判決") {
		t.Fatalf("replayed lifecycle rejection=%T %v API=%+v", err, err, replayAPI)
	}
	if replayOut.Len() != 0 || !strings.Contains(replayErr.String(), "HTTP operator API authoritative current:") ||
		!strings.Contains(replayErr.String(), "lifecycle=retired") || !strings.Contains(replayErr.String(), "revision=1") {
		t.Fatalf("replayed rejection omitted current state: out=%q stderr=%q", replayOut.String(), replayErr.String())
	}
	path := "/v1/operator/machines/" + f.machine.id + "/lifecycle"
	mu.Lock()
	gotCalls := append([]string(nil), calls...)
	mu.Unlock()
	wantCalls := []string{"PUT " + path, "GET " + path}
	if strings.Join(gotCalls, "\n") != strings.Join(wantCalls, "\n") {
		t.Fatalf("replayed rejection calls=%v, want %v", gotCalls, wantCalls)
	}
	machine, getErr := f.store.GetMachine(f.machine.id)
	if getErr != nil || machine.RetiredAt == nil || machine.LifecycleRevision != 1 {
		t.Fatalf("replayed rejection reevaluated old verdict: machine=%+v err=%v", machine, getErr)
	}
}

func TestMachineLifecycleCLIInputValidationStopsBeforeTransport(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "missing machine", args: []string{"lifecycle"}, want: "--machine 必填"},
		{name: "bad desired state", args: []string{"lifecycle", "--machine", "id", "--set", "deleted"}, want: "active 或 retired"},
		{name: "read with reason", args: []string{"lifecycle", "--machine", "id", "--reason", "why"}, want: "只讀模式不接受 --reason"},
		{name: "read with preview", args: []string{"lifecycle", "--machine", "id", "--preview"}, want: "只讀模式不接受 --preview"},
		{name: "apply missing confirmation", args: []string{"lifecycle", "--machine", "id", "--set", "retired", "--reason", "why"}, want: "--confirm-name 必填"},
		{name: "apply missing reason", args: []string{"lifecycle", "--machine", "id", "--set", "retired", "--confirm-name", "name"}, want: "--reason 必填"},
		{name: "preview with confirmation", args: []string{"lifecycle", "--machine", "id", "--set", "retired", "--preview", "--confirm-name", "x"}, want: "--preview 不接受"},
		{name: "preview with reason", args: []string{"lifecycle", "--machine", "id", "--set", "retired", "--preview", "--reason", "y"}, want: "--preview 不接受"},
		{name: "retry key only", args: []string{"lifecycle", "--machine", "id", "--set", "retired", "--idempotency-key", "key"}, want: "retry 必須同時提供"},
		{name: "retry digest only", args: []string{"lifecycle", "--machine", "id", "--set", "retired", "--preview-digest", "sha256:x"}, want: "retry 必須同時提供"},
		{name: "preview with retry coordinates", args: []string{"lifecycle", "--machine", "id", "--set", "retired", "--preview", "--idempotency-key", "key", "--expected-revision", "0", "--preview-digest", "sha256:x"}, want: "--preview 不接受"},
		{name: "mixed transport", args: []string{"lifecycle", "--hub-url", "http://100.64.0.9:8787", "--db", "/tmp/hub.db", "--machine", "id"}, want: "不可同時明示"},
		{name: "empty explicit URL", args: []string{"lifecycle", "--hub-url=", "--machine", "id"}, want: "不可為空"},
		{name: "empty explicit DB", args: []string{"lifecycle", "--db=", "--machine", "id"}, want: "不可為空"},
		{name: "positional", args: []string{"lifecycle", "--machine", "id", "extra"}, want: "positional arguments"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			deps := machineCommandDeps{
				discoverHubURL: func() (string, error) {
					t.Fatal("invalid lifecycle arguments reached discovery")
					return "", nil
				},
				newOperatorClient: func(string) (*operatorclient.Client, error) {
					t.Fatal("invalid lifecycle arguments constructed HTTP client")
					return nil, nil
				},
				validateDirectPath: func(string) error {
					t.Fatal("invalid lifecycle arguments reached direct DB")
					return nil
				},
			}
			var out, errOut bytes.Buffer
			err := runMachineCommandWithDeps(t.Context(), test.args, &out, &errOut, deps)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("invalid lifecycle args=%q err=%v, want %q", test.args, err, test.want)
			}
			if out.Len() != 0 {
				t.Fatalf("invalid lifecycle args printed success: %q", out.String())
			}
		})
	}
}

func TestMachineLifecycleCLIRejectsInvalidRetryCoordinatesBeforeDiscoveryOrDBFence(t *testing.T) {
	validDigest := "sha256:" + strings.Repeat("a", 64)
	retryArgs := func(key, revision, digest string) []string {
		return []string{
			"lifecycle", "--machine", "machine-id", "--set", "retired",
			"--confirm-name", "machine", "--reason", "input validation",
			"--idempotency-key=" + key, "--expected-revision=" + revision, "--preview-digest=" + digest,
		}
	}
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "negative revision before discovery", args: []string{
			"lifecycle", "--machine", "machine-id", "--set", "retired",
			"--confirm-name", "machine", "--reason", "input validation", "--expected-revision=-1",
		}, want: "expected-revision"},
		{name: "negative revision before direct fence", args: []string{
			"lifecycle", "--db", "/must/not/be/inspected.sqlite", "--machine", "machine-id", "--set", "retired",
			"--confirm-name", "machine", "--reason", "input validation", "--expected-revision=-1",
		}, want: "expected-revision"},
		{name: "empty key", args: retryArgs("", "0", validDigest), want: "不可為空"},
		{name: "empty digest", args: retryArgs("retry-key", "0", ""), want: "不可為空"},
		{name: "oversized key", args: retryArgs(strings.Repeat("k", 201), "0", validDigest), want: "200"},
		{name: "oversized digest", args: retryArgs("retry-key", "0", "sha256:"+strings.Repeat("a", 65)), want: "sha256"},
		{name: "malformed digest", args: retryArgs("retry-key", "0", "sha256:not-hex"), want: "sha256"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			deps := machineCommandDeps{
				discoverHubURL: func() (string, error) {
					t.Fatal("invalid lifecycle retry coordinates reached discovery")
					return "", nil
				},
				newOperatorClient: func(string) (*operatorclient.Client, error) {
					t.Fatal("invalid lifecycle retry coordinates constructed client")
					return nil, nil
				},
				validateDirectPath: func(string) error {
					t.Fatal("invalid lifecycle retry coordinates reached direct DB fence")
					return nil
				},
			}
			var out, errOut bytes.Buffer
			err := runMachineCommandWithDeps(t.Context(), test.args, &out, &errOut, deps)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("invalid retry args=%q err=%v, want %q", test.args, err, test.want)
			}
			if out.Len() != 0 || errOut.Len() != 0 {
				t.Fatalf("invalid retry coordinates wrote output: out=%q stderr=%q", out.String(), errOut.String())
			}
		})
	}
}

func TestMachineLifecycleCLIHelpNamesSafeModesAndRetryContract(t *testing.T) {
	var out, errOut bytes.Buffer
	err := runMachineCommand(t.Context(), []string{"lifecycle", "--help"}, &out, &errOut)
	if !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("lifecycle help error=%v", err)
	}
	for _, want := range []string{
		"--hub-url", "operator.json", "--db", "transport：HTTP", "active|retired",
		"--preview", "--reason", "--confirm-name", "--idempotency-key", "--expected-revision", "--preview-digest",
	} {
		if !strings.Contains(errOut.String(), want) {
			t.Errorf("lifecycle help missing %q: %s", want, errOut.String())
		}
	}
}

func TestMachineLifecycleCLIExplicitDirectDBIsFencedAndAudited(t *testing.T) {
	dbPath, machineID := directDBFixture(t)
	seed, err := openExisting(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	insertMachineLifecycleCLIOpenSession(t, seed, machineID, "direct-cli-open")
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}
	deps := productionMachineCommandDeps()
	deps.discoverHubURL = func() (string, error) {
		t.Fatal("explicit lifecycle --db invoked HTTP discovery")
		return "", nil
	}
	stoppedVerified := false
	deps.verifyHubStopped = func(_ context.Context, gotPath string) error {
		if gotPath != dbPath {
			t.Fatalf("lifecycle stopped proof DB=%q want=%q", gotPath, dbPath)
		}
		for name, acquire := range map[string]func(string) (*ledgerlock.Handle, error){
			"upgrade": ledgerlock.AcquireUpgrade,
			"writer":  ledgerlock.AcquireWriter,
		} {
			guard, err := acquire(dbPath)
			if guard != nil {
				_ = guard.Close()
			}
			if !errors.Is(err, ledgerlock.ErrContended) {
				t.Fatalf("lifecycle %s lock not held during stopped proof: %v", name, err)
			}
		}
		stoppedVerified = true
		return nil
	}
	deps.openDirectDB = func(path string) (*store.Store, error) {
		if !stoppedVerified {
			t.Fatal("lifecycle Store.Open ran before stopped proof")
		}
		if guard, err := ledgerlock.AcquireWriter(path); !errors.Is(err, ledgerlock.ErrContended) {
			if guard != nil {
				_ = guard.Close()
			}
			t.Fatalf("lifecycle writer lock not held at Store.Open: %v", err)
		}
		return openExisting(path)
	}

	directArgs := []string{
		"lifecycle", "--db", dbPath, "--machine", machineID, "--set", "retired",
		"--confirm-name", "direct-machine", "--reason", "direct lifecycle test",
	}
	var out, errOut bytes.Buffer
	if err := runMachineCommandWithDeps(t.Context(), directArgs, &out, &errOut, deps); err != nil {
		t.Fatalf("direct lifecycle: %v; stderr=%s", err, errOut.String())
	}
	if !strings.Contains(out.String(), "direct DB operator service") ||
		!strings.Contains(out.String(), "active → retired") || !strings.Contains(out.String(), "revision=1") {
		t.Fatalf("direct lifecycle output=%q", out.String())
	}
	key, revision, digest := lifecycleRetryCoordinates(t, errOut.String())
	if !strings.Contains(errOut.String(), "terminal sessions currently open: 1；retiring ends any still open when applied.") {
		t.Fatalf("direct lifecycle apply omitted terminal-session notice: %q", errOut.String())
	}
	if revision != "0" {
		t.Fatalf("direct lifecycle retry revision=%q, want 0", revision)
	}
	interim, err := openExisting(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	interimMachine, err := interim.GetMachine(machineID)
	if err != nil || interimMachine.RetiredAt == nil || interimMachine.LifecycleRevision != 1 {
		_ = interim.Close()
		t.Fatalf("direct lifecycle pre-restore projection=%+v err=%v", interimMachine, err)
	}
	if err := interim.UnretireMachine(machineID, interimMachine.RetiredAt.Add(time.Second)); err != nil {
		_ = interim.Close()
		t.Fatalf("direct lifecycle later restore: %v", err)
	}
	if err := interim.Close(); err != nil {
		t.Fatal(err)
	}
	var replayOut, replayErr bytes.Buffer
	retryArgs := append(append([]string(nil), directArgs...),
		"--idempotency-key", key, "--expected-revision", revision, "--preview-digest", digest)
	if err := runMachineCommandWithDeps(t.Context(), retryArgs, &replayOut, &replayErr, deps); err != nil {
		t.Fatalf("direct lifecycle replay: %v; stderr=%s", err, replayErr.String())
	}
	for _, want := range []string{
		"direct DB operator service historical replay receipt",
		"replayed=true",
		"direct DB operator service authoritative current:",
		"lifecycle=active",
		"revision=2",
	} {
		if !strings.Contains(replayOut.String(), want) {
			t.Fatalf("direct lifecycle replay missing %q: %q", want, replayOut.String())
		}
	}
	for name, acquire := range map[string]func(string) (*ledgerlock.Handle, error){
		"upgrade": ledgerlock.AcquireUpgrade,
		"writer":  ledgerlock.AcquireWriter,
	} {
		guard, err := acquire(dbPath)
		if err != nil {
			t.Fatalf("direct lifecycle leaked %s lock: %v", name, err)
		}
		_ = guard.Close()
	}

	st, err := openExisting(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	machine, err := st.GetMachine(machineID)
	if err != nil || machine.RetiredAt != nil || machine.LifecycleRevision != 2 {
		t.Fatalf("direct lifecycle projection=%+v err=%v", machine, err)
	}
	entries, err := st.Audit(machineID, 10)
	if err != nil || len(entries) != 2 {
		t.Fatalf("direct lifecycle audit=%+v err=%v", entries, err)
	}
	for _, entry := range entries {
		if entry.Action != store.AuditMachineLifecycle || !entry.OK ||
			entry.SourceAddr != "local-cli" || entry.SourceKind != "direct-db-cli" ||
			entry.UserAgent != "clawctl-hub machine lifecycle" || entry.WhoUnavailable == "" ||
			entry.Reason != "direct lifecycle test" || entry.RequestDigest == "" || entry.IdempotencyKey != key {
			t.Fatalf("direct lifecycle audit fields=%+v", entry)
		}
	}
}

func TestLegacyRetireAliasDelegatesOnlyToRetiredLifecycle(t *testing.T) {
	argv := []string{"--machine", "machine-id", "--confirm-name", "machine", "--reason", "planned"}
	translated, err := retireAliasArgs(argv)
	if err != nil {
		t.Fatal(err)
	}
	want := append([]string{"lifecycle", "--set", "retired"}, argv...)
	if strings.Join(translated, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("legacy retire translation=%q, want %q", translated, want)
	}
	for _, spelling := range [][]string{{"--set", "active"}, {"--set=active"}, {"-set", "active"}, {"-set=active"}} {
		if got, err := retireAliasArgs(spelling); err == nil || got != nil {
			t.Fatalf("legacy retire accepted desired-state override %q: translated=%q err=%v", spelling, got, err)
		}
	}

	source := readRepoFile(t, "main.go")
	start := strings.Index(source, "func cmdRetire(argv []string)")
	if start < 0 {
		t.Fatal("main.go is missing cmdRetire")
	}
	end := strings.Index(source[start:], "// cmdPrune")
	if end < 0 {
		t.Fatal("main.go is missing bounded cmdRetire implementation")
	}
	body := source[start : start+end]
	for _, want := range []string{
		`[]string{"lifecycle", "--set", "retired"}`,
		"runMachineCommand(context.Background()",
		`arg == "--set"`,
		`strings.HasPrefix(arg, "--set=")`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("legacy retire alias lost safe delegation anchor %q:\n%s", want, body)
		}
	}
	for _, forbidden := range []string{"mustOpen(", ".RetireMachine(", ".UnretireMachine(", "store.Open("} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("legacy retire alias regained direct mutation %q:\n%s", forbidden, body)
		}
	}
}

func TestLegacyRetireAliasRejectsOverrideAndOldPositionalBeforeDB(t *testing.T) {
	for _, test := range []struct {
		name string
		args []string
		want string
	}{
		{name: "desired state override", args: []string{"retire", "--set=active", "--machine", "id", "--confirm-name", "name", "--reason", "why"}, want: "retire alias 不接受 --set"},
		{name: "single dash desired state override", args: []string{"retire", "-set=active", "--machine", "id", "--confirm-name", "name", "--reason", "why"}, want: "retire alias 不接受 --set"},
		{name: "single dash split desired state override", args: []string{"retire", "-set", "active", "--machine", "id", "--confirm-name", "name", "--reason", "why"}, want: "retire alias 不接受 --set"},
		{name: "old positional", args: []string{"retire", "machine-id"}, want: "positional arguments"},
	} {
		t.Run(test.name, func(t *testing.T) {
			dbPath := filepath.Join(t.TempDir(), "must-not-exist.sqlite")
			rawArgs, err := json.Marshal(test.args)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestUnknownCommandProcessHelper$")
			cmd.Env = dispatchHelperEnv(dbPath, string(rawArgs))
			output, runErr := cmd.CombinedOutput()
			if ctx.Err() != nil {
				t.Fatalf("legacy retire alias did not fail promptly: %v\n%s", ctx.Err(), output)
			}
			var exitErr *exec.ExitError
			if !errors.As(runErr, &exitErr) || exitErr.ExitCode() == 0 {
				t.Fatalf("legacy retire args=%q exit=%v, want nonzero\n%s", test.args, runErr, output)
			}
			if !strings.Contains(string(output), test.want) {
				t.Fatalf("legacy retire args=%q missing %q:\n%s", test.args, test.want, output)
			}
			for _, path := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
				if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("legacy retire rejection touched DB %s: %v", path, err)
				}
			}
		})
	}
}
