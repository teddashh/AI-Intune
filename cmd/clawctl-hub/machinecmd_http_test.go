package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/teddashh/AI-Intune/internal/ledgerlock"
	"github.com/teddashh/AI-Intune/internal/operatorclient"
	"github.com/teddashh/AI-Intune/internal/store"
)

func TestMachineRenameCLIHTTPPreviewApplyAndReplayNeverTouchDB(t *testing.T) {
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

	var previewOut, errOut bytes.Buffer
	err := runMachineCommandWithDeps(t.Context(), []string{
		"rename", "--hub-url", base, "--machine", f.machine.id,
		"--set", "cnode-renamed-cli", "--preview", "--json",
	}, &previewOut, &errOut, deps)
	if err != nil {
		t.Fatalf("preview CLI=%v stderr=%s", err, errOut.String())
	}
	var preview store.OperatorMachineRenamePreviewResult
	if err := json.Unmarshal(previewOut.Bytes(), &preview); err != nil ||
		preview.CurrentDisplayName != "cnode-operator" || preview.DisplayName != "cnode-renamed-cli" ||
		preview.PreviewDigest == "" {
		t.Fatalf("preview=%+v decode=%v body=%s", preview, err, previewOut.String())
	}
	var humanPreview bytes.Buffer
	if err := runMachineCommandWithDeps(t.Context(), []string{
		"rename", "--hub-url", base, "--machine", f.machine.id,
		"--set", "cnode-renamed-cli", "--preview",
	}, &humanPreview, &errOut, deps); err != nil ||
		!strings.Contains(humanPreview.String(), "name-based expectations will use the new name") ||
		!strings.Contains(humanPreview.String(), "machine ID, agent, and hostname on the machine remain unchanged") {
		t.Fatalf("human preview=%q err=%v stderr=%s", humanPreview.String(), err, errOut.String())
	}
	args := []string{
		"rename", "--hub-url", base, "--machine", f.machine.id, "--set", "cnode-renamed-cli",
		"--reason", "align registry label", "--confirm-name", "cnode-operator",
		"--idempotency-key", "cli-rename-replay", "--preview-digest", preview.PreviewDigest, "--json",
	}
	var freshOut bytes.Buffer
	if err := runMachineCommandWithDeps(t.Context(), args, &freshOut, &errOut, deps); err != nil {
		t.Fatalf("apply CLI=%v stderr=%s", err, errOut.String())
	}
	var fresh store.OperatorMachineRenameResult
	if err := json.Unmarshal(freshOut.Bytes(), &fresh); err != nil || fresh.Replayed ||
		fresh.MachineID != f.machine.id || fresh.DisplayName != "cnode-renamed-cli" {
		t.Fatalf("fresh=%+v decode=%v body=%s", fresh, err, freshOut.String())
	}
	var replayOut bytes.Buffer
	if err := runMachineCommandWithDeps(t.Context(), args, &replayOut, &errOut, deps); err != nil {
		t.Fatalf("replay CLI=%v stderr=%s", err, errOut.String())
	}
	var replay store.OperatorMachineRenameResult
	if err := json.Unmarshal(replayOut.Bytes(), &replay); err != nil || !replay.Replayed ||
		!replay.AppliedAt.Equal(fresh.AppliedAt) {
		t.Fatalf("replay=%+v decode=%v body=%s", replay, err, replayOut.String())
	}
	if _, err := os.Stat(missingDB); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rename CLI touched direct DB: %v", err)
	}
	wantPath := "/v1/operator/machines/" + f.machine.id
	mu.Lock()
	gotCalls := append([]string(nil), calls...)
	mu.Unlock()
	wantCalls := []string{
		"POST " + wantPath + "/display-name-preview",
		"POST " + wantPath + "/display-name-preview",
		"PUT " + wantPath + "/display-name",
		"PUT " + wantPath + "/display-name",
	}
	if len(gotCalls) != len(wantCalls) {
		t.Fatalf("calls=%v want=%v", gotCalls, wantCalls)
	}
	for index := range wantCalls {
		if gotCalls[index] != wantCalls[index] {
			t.Fatalf("calls=%v want=%v", gotCalls, wantCalls)
		}
	}
	entries, err := f.store.Audit(f.machine.id, 10)
	if err != nil || len(entries) != 2 || entries[0].UserAgent != operatorclient.UserAgent ||
		entries[1].UserAgent != operatorclient.UserAgent {
		t.Fatalf("audit=%+v err=%v", entries, err)
	}
}

func TestMachineNotesCLIHTTPPreviewApplyReplayAndClearNeverTouchDB(t *testing.T) {
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

	var previewOut, errOut bytes.Buffer
	err := runMachineCommandWithDeps(t.Context(), []string{
		"notes", "--hub-url", base, "--machine", f.machine.id,
		"--set", "GPU runner", "--preview", "--json",
	}, &previewOut, &errOut, deps)
	var preview store.OperatorMachineNotesPreviewResult
	if err != nil || json.Unmarshal(previewOut.Bytes(), &preview) != nil ||
		preview.CurrentNotes != "" || preview.Notes != "GPU runner" || preview.PreviewDigest == "" {
		t.Fatalf("preview=%+v err=%v stderr=%s body=%s", preview, err, errOut.String(), previewOut.String())
	}
	args := []string{
		"notes", "--hub-url", base, "--machine", f.machine.id, "--set", "GPU runner",
		"--reason", "record machine purpose", "--confirm-name", "cnode-operator",
		"--idempotency-key", "cli-notes-replay", "--preview-digest", preview.PreviewDigest, "--json",
	}
	var freshOut bytes.Buffer
	if err := runMachineCommandWithDeps(t.Context(), args, &freshOut, &errOut, deps); err != nil {
		t.Fatalf("apply CLI=%v stderr=%s", err, errOut.String())
	}
	var fresh store.OperatorMachineNotesResult
	if err := json.Unmarshal(freshOut.Bytes(), &fresh); err != nil || fresh.Replayed ||
		!fresh.NotesPresent || strings.Contains(freshOut.String(), "GPU runner") {
		t.Fatalf("fresh=%+v decode=%v body=%s", fresh, err, freshOut.String())
	}
	var replayOut bytes.Buffer
	if err := runMachineCommandWithDeps(t.Context(), args, &replayOut, &errOut, deps); err != nil {
		t.Fatalf("replay CLI=%v stderr=%s", err, errOut.String())
	}
	var replay store.OperatorMachineNotesResult
	if err := json.Unmarshal(replayOut.Bytes(), &replay); err != nil || !replay.Replayed ||
		!replay.AppliedAt.Equal(fresh.AppliedAt) {
		t.Fatalf("replay=%+v decode=%v body=%s", replay, err, replayOut.String())
	}
	var clearPreviewOut bytes.Buffer
	if err := runMachineCommandWithDeps(t.Context(), []string{
		"notes", "--hub-url", base, "--machine", f.machine.id, "--set", "", "--preview", "--json",
	}, &clearPreviewOut, &errOut, deps); err != nil {
		t.Fatalf("clear preview CLI=%v stderr=%s", err, errOut.String())
	}
	var clearPreview store.OperatorMachineNotesPreviewResult
	if err := json.Unmarshal(clearPreviewOut.Bytes(), &clearPreview); err != nil ||
		clearPreview.CurrentNotes != "GPU runner" || clearPreview.Notes != "" || clearPreview.PreviewDigest == "" {
		t.Fatalf("clear preview=%+v decode=%v body=%s", clearPreview, err, clearPreviewOut.String())
	}
	var clearOut bytes.Buffer
	if err := runMachineCommandWithDeps(t.Context(), []string{
		"notes", "--hub-url", base, "--machine", f.machine.id, "--set", "",
		"--reason", "clear retired purpose", "--confirm-name", "cnode-operator",
		"--idempotency-key", "cli-notes-clear", "--preview-digest", clearPreview.PreviewDigest, "--json",
	}, &clearOut, &errOut, deps); err != nil {
		t.Fatalf("clear CLI=%v stderr=%s", err, errOut.String())
	}
	var cleared store.OperatorMachineNotesResult
	if err := json.Unmarshal(clearOut.Bytes(), &cleared); err != nil || cleared.NotesPresent || !cleared.PreviousNotesPresent {
		t.Fatalf("cleared=%+v decode=%v body=%s", cleared, err, clearOut.String())
	}
	machine, err := f.store.GetMachine(f.machine.id)
	if err != nil || machine.Notes != "" {
		t.Fatalf("machine=%+v err=%v", machine, err)
	}
	if _, err := os.Stat(missingDB); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("notes CLI touched direct DB: %v", err)
	}
	wantPath := "/v1/operator/machines/" + f.machine.id
	mu.Lock()
	gotCalls := append([]string(nil), calls...)
	mu.Unlock()
	wantCalls := []string{
		"POST " + wantPath + "/notes-preview",
		"PUT " + wantPath + "/notes",
		"PUT " + wantPath + "/notes",
		"POST " + wantPath + "/notes-preview",
		"PUT " + wantPath + "/notes",
	}
	if len(gotCalls) != len(wantCalls) {
		t.Fatalf("calls=%v want=%v", gotCalls, wantCalls)
	}
	for index := range wantCalls {
		if gotCalls[index] != wantCalls[index] {
			t.Fatalf("calls=%v want=%v", gotCalls, wantCalls)
		}
	}
	entries, err := f.store.Audit(f.machine.id, 10)
	if err != nil || len(entries) != 3 || strings.Contains(entries[0].Detail, "GPU runner") ||
		strings.Contains(entries[1].Detail, "GPU runner") || strings.Contains(entries[2].Detail, "GPU runner") {
		t.Fatalf("audit=%+v err=%v", entries, err)
	}
}

func TestMachineNotesCLIRequiresExplicitSetBeforeNetwork(t *testing.T) {
	var hits int
	deps := productionMachineCommandDeps()
	deps.newOperatorClient = func(string) (*operatorclient.Client, error) {
		hits++
		return nil, errors.New("network should not be reached")
	}
	var out, errOut bytes.Buffer
	err := runMachineCommandWithDeps(t.Context(), []string{
		"notes", "--hub-url", "http://100.64.0.9:8787", "--machine", "machine-1", "--preview",
	}, &out, &errOut, deps)
	if err == nil || !strings.Contains(err.Error(), "--set are required") || hits != 0 {
		t.Fatalf("err=%v hits=%d stderr=%s", err, hits, errOut.String())
	}
}

func machineHTTPTestDeps(t *testing.T, server *httptest.Server) (string, machineCommandDeps) {
	t.Helper()
	u, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	authority := net.JoinHostPort("100.64.0.9", u.Port())
	base := "http://" + authority
	deps := productionMachineCommandDeps()
	deps.newOperatorClient = func(got string) (*operatorclient.Client, error) {
		if got != base {
			return nil, errors.New("test received unexpected operator authority")
		}
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.Proxy = nil
		dialer := &net.Dialer{}
		transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
			if address == authority {
				address = server.Listener.Addr().String()
			}
			return dialer.DialContext(ctx, network, address)
		}
		return operatorclient.NewWithHTTPClient(base, &http.Client{Transport: transport})
	}
	return base, deps
}

func TestMachineChannelCLIHTTPModeUsesRealOperatorRoutes(t *testing.T) {
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

	missingDB := t.TempDir() + "/must-not-be-created.sqlite"
	t.Setenv("CLAWCTL_DB", missingDB)
	var out, errOut bytes.Buffer
	err := runMachineCommandWithDeps(t.Context(), []string{
		"channel", "--hub-url", base, "--machine", f.machine.id, "--set", "canary",
		"--confirm-name", "cnode-operator",
	}, &out, &errOut, deps)
	if err != nil {
		t.Fatalf("HTTP CLI: %v; stderr=%s", err, errOut.String())
	}
	if !strings.Contains(out.String(), "HTTP operator API") ||
		!strings.Contains(out.String(), "cnode-operator") || !strings.Contains(out.String(), "ETag") {
		t.Fatalf("HTTP mode output=%q", out.String())
	}
	if _, err := os.Stat(missingDB); !os.IsNotExist(err) {
		t.Fatalf("HTTP mode touched direct DB path: %v", err)
	}
	wantPath := "/v1/operator/machines/" + f.machine.id + "/channel"
	mu.Lock()
	gotCalls := append([]string(nil), calls...)
	mu.Unlock()
	if len(gotCalls) != 2 || gotCalls[0] != "GET "+wantPath || gotCalls[1] != "PUT "+wantPath {
		t.Fatalf("HTTP CLI calls=%v", gotCalls)
	}
	entries, err := f.store.Audit(f.machine.id, 10)
	if err != nil || len(entries) == 0 || entries[0].UserAgent != operatorclient.UserAgent {
		t.Fatalf("HTTP CLI provenance audit=%+v err=%v", entries, err)
	}
}

func TestMachineChannelCLIDiscoversHTTPByDefaultAndNeverTouchesDB(t *testing.T) {
	f := observedOperatorFixture(t)
	server := httptest.NewServer(f.mux)
	defer server.Close()
	base, deps := machineHTTPTestDeps(t, server)
	missingDB := t.TempDir() + "/must-not-be-created.sqlite"
	t.Setenv("CLAWCTL_DB", missingDB)
	deps.discoverHubURL = func() (string, error) { return base, nil }
	deps.verifyHubStopped = func(context.Context, string) error {
		t.Fatal("normal HTTP mode checked the local Hub unit")
		return nil
	}
	var out, errOut bytes.Buffer
	err := runMachineCommandWithDeps(t.Context(), []string{
		"channel", "--machine", f.machine.id, "--set", "canary",
		"--confirm-name", "cnode-operator",
	}, &out, &errOut, deps)
	if err != nil || !strings.Contains(out.String(), "HTTP operator API") {
		t.Fatalf("default HTTP mode err=%v out=%q stderr=%q", err, out.String(), errOut.String())
	}
	if _, err := os.Stat(missingDB); !os.IsNotExist(err) {
		t.Fatalf("default HTTP mode touched direct DB path: %v", err)
	}
}

func TestMachineChannelCLIDiscoveryFailureNeverFallsBackToDB(t *testing.T) {
	missingDB := t.TempDir() + "/must-not-be-created.sqlite"
	t.Setenv("CLAWCTL_DB", missingDB)
	deps := machineCommandDeps{
		discoverHubURL: func() (string, error) { return "", errors.New("discovery unavailable") },
		verifyHubStopped: func(context.Context, string) error {
			t.Fatal("discovery failure fell through to direct DB")
			return nil
		},
	}
	var out, errOut bytes.Buffer
	err := runMachineCommandWithDeps(t.Context(), []string{
		"channel", "--machine", "machine-id", "--set", "canary", "--confirm-name", "cnode",
	}, &out, &errOut, deps)
	if err == nil || !strings.Contains(err.Error(), "discovery unavailable") {
		t.Fatalf("discovery error=%v", err)
	}
	if _, err := os.Stat(missingDB); !os.IsNotExist(err) {
		t.Fatalf("discovery failure created fallback DB: %v", err)
	}
}

func TestMachineChannelCLIExplicitHubURLOverridesDiscovery(t *testing.T) {
	f := observedOperatorFixture(t)
	server := httptest.NewServer(f.mux)
	defer server.Close()
	base, deps := machineHTTPTestDeps(t, server)
	deps.discoverHubURL = func() (string, error) {
		t.Fatal("explicit --hub-url invoked discovery")
		return "", nil
	}
	deps.verifyHubStopped = func(context.Context, string) error {
		t.Fatal("explicit HTTP mode checked the local Hub unit")
		return nil
	}
	var out, errOut bytes.Buffer
	err := runMachineCommandWithDeps(t.Context(), []string{
		"channel", "--hub-url", base, "--machine", f.machine.id, "--set", "canary",
		"--confirm-name", "cnode-operator",
	}, &out, &errOut, deps)
	if err != nil {
		t.Fatalf("explicit HTTP mode: %v; stderr=%s", err, errOut.String())
	}
}

func TestMachineChannelCLIExplicitInvalidHubURLNeverFallsBack(t *testing.T) {
	missingDB := filepath.Join(t.TempDir(), "must-not-be-created.sqlite")
	t.Setenv("CLAWCTL_DB", missingDB)
	deps := productionMachineCommandDeps()
	deps.discoverHubURL = func() (string, error) {
		t.Fatal("selected explicit URL fell through to discovery")
		return "", nil
	}
	deps.validateDirectPath = func(string) error {
		t.Fatal("selected explicit URL fell through to direct DB")
		return nil
	}
	deps.validateDirectLedger = func(string) error {
		t.Fatal("selected explicit URL inspected a direct ledger")
		return nil
	}
	var out, errOut bytes.Buffer
	err := runMachineCommandWithDeps(t.Context(), []string{
		"channel", "--hub-url", "http://127.0.0.1:8787", "--machine", "machine-id",
		"--set", "canary", "--confirm-name", "machine",
	}, &out, &errOut, deps)
	if err == nil || !strings.Contains(err.Error(), "Tailscale IP") {
		t.Fatalf("invalid explicit URL error=%v", err)
	}
	if _, err := os.Stat(missingDB); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid explicit URL touched DB: %v", err)
	}
}

func TestMachineChannelCLIExplicitDBRequiresStoppedHubBeforeOpening(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(dir, "existing.sqlite")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	deps := productionMachineCommandDeps()
	deps.discoverHubURL = func() (string, error) {
		t.Fatal("explicit --db invoked HTTP discovery")
		return "", nil
	}
	deps.verifyHubStopped = func(context.Context, string) error { return errors.New("Hub 還在 active/running") }
	deps.openDirectDB = func(string) (*store.Store, error) {
		t.Fatal("active Hub reached Store.Open")
		return nil, nil
	}
	var out, errOut bytes.Buffer
	err = runMachineCommandWithDeps(t.Context(), []string{
		"channel", "--db", dbPath, "--machine", "machine-id", "--set", "canary",
		"--confirm-name", "cnode",
	}, &out, &errOut, deps)
	if err == nil || !strings.Contains(err.Error(), "active/running") {
		t.Fatalf("active Hub direct DB error=%v", err)
	}
	guard, lockErr := ledgerlock.AcquireDirect(dbPath)
	if lockErr != nil {
		t.Fatalf("rejected direct DB leaked locks: %v", lockErr)
	}
	_ = guard.Close()
}

func TestOperatorClientRealRoutesParseReplayETagAndAPIErrors(t *testing.T) {
	f := observedOperatorFixture(t)
	server := httptest.NewServer(f.mux)
	defer server.Close()
	base, deps := machineHTTPTestDeps(t, server)
	client, err := deps.newOperatorClient(base)
	if err != nil {
		t.Fatal(err)
	}
	state, err := client.GetMachineChannel(t.Context(), f.machine.id)
	if err != nil || state.Revision != 0 || state.Meta.ETagRevision != 0 || state.Meta.ETag == "" {
		t.Fatalf("GET state=%+v err=%v", state, err)
	}
	req := operatorclient.MachineChannelRequest{
		Channel: "canary", ExpectedRevision: 0, ConfirmDisplayName: state.DisplayName,
	}
	first, err := client.PutMachineChannel(t.Context(), f.machine.id, "client-contract-replay", req)
	if err != nil || first.Revision != 1 || first.Replayed || first.Meta.ETagRevision != 1 {
		t.Fatalf("first PUT=%+v err=%v", first, err)
	}
	replay, err := client.PutMachineChannel(t.Context(), f.machine.id, "client-contract-replay", req)
	if err != nil || !replay.Replayed || !replay.Meta.IdempotencyReplayed || replay.Meta.ETagRevision != 1 {
		t.Fatalf("replay PUT=%+v err=%v", replay, err)
	}
	none, err := client.PutMachineChannel(t.Context(), f.machine.id, "client-contract-none",
		operatorclient.MachineChannelRequest{
			Channel: "none", ExpectedRevision: 1, ConfirmDisplayName: state.DisplayName,
		})
	if err != nil || none.Channel != "" || none.PreviousChannel != "canary" || none.Revision != 2 {
		t.Fatalf("canonical none PUT=%+v err=%v", none, err)
	}

	_, err = client.PutMachineChannel(t.Context(), f.machine.id, "client-contract-error",
		operatorclient.MachineChannelRequest{
			Channel: "stable", ExpectedRevision: 2, ConfirmDisplayName: "wrong-name",
		})
	var apiErr *operatorclient.APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusBadRequest ||
		apiErr.Code != "CONFIRMATION_MISMATCH" || apiErr.Message == "" {
		t.Fatalf("structured API error=%T %+v", err, apiErr)
	}
}

func TestMachineChannelCLIRejectsExplicitHTTPAndDBModesTogether(t *testing.T) {
	var out, errOut bytes.Buffer
	err := runMachineCommand(t.Context(), []string{
		"channel", "--hub-url", "http://127.0.0.1:1", "--db", t.TempDir() + "/hub.db",
		"--machine", "abc", "--set", "canary", "--confirm-name", "cnode-operator",
	}, &out, &errOut)
	if err == nil || !strings.Contains(err.Error(), "--hub-url") || !strings.Contains(err.Error(), "--db") {
		t.Fatalf("mixed mode error=%v", err)
	}
}

func TestMachineChannelCLIHTTPMissingSetReturnsStableAPIErrorWithoutMutation(t *testing.T) {
	f := observedOperatorFixture(t)
	server := httptest.NewServer(f.mux)
	defer server.Close()
	base, deps := machineHTTPTestDeps(t, server)
	var out, errOut bytes.Buffer
	err := runMachineCommandWithDeps(t.Context(), []string{
		"channel", "--hub-url", base, "--machine", f.machine.id,
		"--confirm-name", "cnode-operator",
	}, &out, &errOut, deps)
	var apiErr *operatorclient.APIError
	if !errors.As(err, &apiErr) || apiErr.Code != "BAD_CHANNEL" || apiErr.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing --set error=%T %v API=%+v", err, err, apiErr)
	}
	if out.Len() != 0 {
		t.Fatalf("missing --set printed success: %q", out.String())
	}
	m, getErr := f.store.GetMachine(f.machine.id)
	if getErr != nil || m.Channel != "" || m.ChannelRevision != 0 {
		t.Fatalf("missing HTTP --set mutated machine=%+v err=%v", m, getErr)
	}
}

func TestMachineChannelHelpNamesHTTPAndDirectModes(t *testing.T) {
	var out, errOut bytes.Buffer
	err := runMachineCommand(t.Context(), []string{"channel", "-h"}, &out, &errOut)
	if !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("help error=%v", err)
	}
	for _, want := range []string{
		"--hub-url", "HTTP operator API", "--db", "direct DB", "machine_id",
		"--confirm-name", "--idempotency-key", "--expected-revision",
	} {
		if !strings.Contains(errOut.String(), want) {
			t.Errorf("help missing %q: %s", want, errOut.String())
		}
	}
}

func TestMachineChannelCLIRequiresOperatorSuppliedConfirmation(t *testing.T) {
	f := observedOperatorFixture(t)
	server := httptest.NewServer(f.mux)
	defer server.Close()
	base, deps := machineHTTPTestDeps(t, server)
	var out, errOut bytes.Buffer
	err := runMachineCommandWithDeps(t.Context(), []string{
		"channel", "--hub-url", base, "--machine", f.machine.id, "--set", "canary",
	}, &out, &errOut, deps)
	if err == nil || !strings.Contains(err.Error(), "--confirm-name is required") {
		t.Fatalf("missing confirmation error=%v", err)
	}
	m, getErr := f.store.GetMachine(f.machine.id)
	if getErr != nil || m.Channel != "" || m.ChannelRevision != 0 {
		t.Fatalf("missing confirmation mutated machine=%+v err=%v", m, getErr)
	}
}

func TestMachineChannelCLIWrongExpectedNameDoesNotMutateResolvedID(t *testing.T) {
	f := observedOperatorFixture(t)
	server := httptest.NewServer(f.mux)
	defer server.Close()
	base, deps := machineHTTPTestDeps(t, server)
	var out, errOut bytes.Buffer
	err := runMachineCommandWithDeps(t.Context(), []string{
		"channel", "--hub-url", base, "--machine", f.machine.id, "--set", "canary",
		"--confirm-name", "the-machine-I-meant",
	}, &out, &errOut, deps)
	var apiErr *operatorclient.APIError
	if !errors.As(err, &apiErr) || apiErr.Code != "CONFIRMATION_MISMATCH" {
		t.Fatalf("wrong expected name error=%T %v API=%+v", err, err, apiErr)
	}
	m, getErr := f.store.GetMachine(f.machine.id)
	if getErr != nil || m.Channel != "" || m.ChannelRevision != 0 {
		t.Fatalf("wrong expected name mutated machine=%+v err=%v", m, getErr)
	}
}

func TestMachineChannelCLIExplicitRetryInputsReplaySameRequest(t *testing.T) {
	f := observedOperatorFixture(t)
	server := httptest.NewServer(f.mux)
	defer server.Close()
	base, deps := machineHTTPTestDeps(t, server)
	args := []string{
		"channel", "--hub-url", base, "--machine", f.machine.id, "--set", "canary",
		"--confirm-name", "cnode-operator", "--idempotency-key", "cli-explicit-retry",
		"--expected-revision", "0",
	}
	for attempt := 1; attempt <= 2; attempt++ {
		var out, errOut bytes.Buffer
		if err := runMachineCommandWithDeps(t.Context(), args, &out, &errOut, deps); err != nil {
			t.Fatalf("attempt %d: %v; stderr=%s", attempt, err, errOut.String())
		}
		if attempt == 2 && !strings.Contains(out.String(), "idempotency replay") {
			t.Fatalf("second attempt did not expose replay: %q", out.String())
		}
	}
	m, err := f.store.GetMachine(f.machine.id)
	if err != nil || m.Channel != "canary" || m.ChannelRevision != 1 {
		t.Fatalf("replayed CLI request changed revision twice: machine=%+v err=%v", m, err)
	}
}

func TestMachineChannelCLIReplayedRejectionSaysItIsTheOldVerdict(t *testing.T) {
	f := observedOperatorFixture(t)
	server := httptest.NewServer(f.mux)
	defer server.Close()
	base, deps := machineHTTPTestDeps(t, server)
	args := []string{
		"channel", "--hub-url", base, "--machine", f.machine.id, "--set", "canary",
		"--confirm-name", "wrong-name", "--idempotency-key", "cli-rejected-retry",
		"--expected-revision", "0",
	}
	for attempt := 1; attempt <= 2; attempt++ {
		var out, errOut bytes.Buffer
		err := runMachineCommandWithDeps(t.Context(), args, &out, &errOut, deps)
		var apiErr *operatorclient.APIError
		if !errors.As(err, &apiErr) || apiErr.Code != "CONFIRMATION_MISMATCH" {
			t.Fatalf("attempt %d error=%T %v API=%+v", attempt, err, err, apiErr)
		}
		if attempt == 1 && (apiErr.Replayed || strings.Contains(err.Error(), "this is the original verdict")) {
			t.Fatalf("first rejection mislabeled as replay: %v", err)
		}
		if attempt == 2 && (!apiErr.Replayed || !strings.Contains(err.Error(), "this is the original verdict")) {
			t.Fatalf("replayed rejection not identified as old verdict: %v API=%+v", err, apiErr)
		}
	}
	m, err := f.store.GetMachine(f.machine.id)
	if err != nil || m.Channel != "" || m.ChannelRevision != 0 {
		t.Fatalf("rejected replay mutated machine=%+v err=%v", m, err)
	}
}

func TestMachineChannelCLIRetryInputsMustBePaired(t *testing.T) {
	for _, args := range [][]string{
		{"channel", "--machine", "id", "--set", "canary", "--confirm-name", "cnode", "--idempotency-key", "key"},
		{"channel", "--machine", "id", "--set", "canary", "--confirm-name", "cnode", "--expected-revision", "0"},
	} {
		var out, errOut bytes.Buffer
		err := runMachineCommand(t.Context(), args, &out, &errOut)
		if err == nil || !strings.Contains(err.Error(), "must be provided together") {
			t.Errorf("unpaired retry args=%v error=%v", args, err)
		}
	}
}

func TestMachineChannelCLIRejectsTrailingArgumentBeforeHTTPMutation(t *testing.T) {
	f := observedOperatorFixture(t)
	var hits int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		f.mux.ServeHTTP(w, r)
	}))
	defer server.Close()
	base, deps := machineHTTPTestDeps(t, server)

	var out, errOut bytes.Buffer
	err := runMachineCommandWithDeps(t.Context(), []string{
		"channel", "--hub-url", base, "--machine", f.machine.id, "--set", "stable",
		"--confirm-name", "cnode-operator", "none",
	}, &out, &errOut, deps)
	if err == nil || !strings.Contains(err.Error(), "positional arguments") {
		t.Fatalf("trailing argument error=%v", err)
	}
	if hits != 0 {
		t.Fatalf("invalid CLI reached HTTP operator API %d times", hits)
	}
	m, getErr := f.store.GetMachine(f.machine.id)
	if getErr != nil || m.Channel != "" || m.ChannelRevision != 0 {
		t.Fatalf("trailing argument mutated machine=%+v err=%v", m, getErr)
	}
}

func TestMachineCLIUnknownOrMissingSubcommandIsNotSuccess(t *testing.T) {
	for _, argv := range [][]string{nil, {"chanel"}} {
		var out, errOut bytes.Buffer
		err := runMachineCommand(t.Context(), argv, &out, &errOut)
		if err == nil || errors.Is(err, flag.ErrHelp) {
			t.Errorf("argv=%v returned success/help for invalid invocation: %v", argv, err)
		}
		if !strings.Contains(errOut.String(), "Usage:") {
			t.Errorf("argv=%v omitted usage: %q", argv, errOut.String())
		}
	}

	var out, errOut bytes.Buffer
	if err := runMachineCommand(t.Context(), []string{"--help"}, &out, &errOut); !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("explicit help error=%v", err)
	}
}
