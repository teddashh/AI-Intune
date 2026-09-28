package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/teddashh/AI-Intune/internal/artifact"
	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/operatorauth"
	"github.com/teddashh/AI-Intune/internal/operatorclient"
	"golang.org/x/sys/unix"
)

func TestDeploymentCreateDefaultRecoverySurvivesCommitThenDisconnectAndReplays(t *testing.T) {
	f, record := deploymentMutationFixture(t, 1)
	stateRoot := os.Getenv("XDG_STATE_HOME")
	var previewCalls, applyCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/operator/deployments/preview" {
			previewCalls.Add(1)
		}
		if r.URL.Path == "/v1/operator/deployments" {
			attempt := applyCalls.Add(1)
			if attempt == 1 {
				// Commit through the real handler, then return a deliberately truncated
				// 201 body. Receiving response headers prevents net/http from retrying
				// the POST, while the client still cannot know whether it committed.
				recorded := httptest.NewRecorder()
				f.mux.ServeHTTP(recorded, verifiedOperatorRequest(r, operatorauth.Admin))
				if recorded.Code != http.StatusCreated {
					t.Errorf("committed response=%d body=%s", recorded.Code, recorded.Body.String())
				}
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Content-Length", "4096")
				w.WriteHeader(http.StatusCreated)
				_, _ = w.Write([]byte("{"))
				return
			}
		}
		f.mux.ServeHTTP(w, verifiedOperatorRequest(r, operatorauth.Admin))
	}))
	defer server.Close()
	base, deps := machineHTTPTestDeps(t, server)
	originalNewClient := deps.newOperatorClient
	var clientURLs []string
	deps.newOperatorClient = func(raw string) (*operatorclient.Client, error) {
		clientURLs = append(clientURLs, raw)
		if raw != base+"/" && raw != base {
			return nil, errors.New("test received unexpected operator URL")
		}
		return originalNewClient(base)
	}
	deps.discoverHubURL = func() (string, error) { return base + "/", nil }
	deps.validateDirectPath = func(string) error {
		t.Fatal("HTTP recovery inspected direct DB")
		return nil
	}
	missingDB := filepath.Join(t.TempDir(), "must-not-exist.sqlite")
	t.Setenv("CLAWCTL_DB", missingDB)
	const privateReason = "PRIVATE_$(must-not-run)_RECOVERY_REASON"
	args := []string{
		"create", "--channel", "canary", "--version", record.Version,
		// Deliberately omit --artifact. Recovery must preserve the empty request
		// field rather than substitute the selected SHA printed by preview.
		"--batch", "1", "--timeout", "600", "--confirm-channel", "canary",
		"--confirm-version", record.Version, "--reason", privateReason, "--json",
	}
	var out, errOut bytes.Buffer
	err := runDeploymentMutationCommandWithDeps(t.Context(), args, &out, &errOut, deps)
	if err == nil || out.Len() != 0 {
		t.Fatalf("commit-disconnect err=%v stdout=%q stderr=%q", err, out.String(), errOut.String())
	}
	for _, unsafe := range []string{privateReason, record.TarballURL, record.FetchedBy, `"spec"`, "\x1b"} {
		if strings.Contains(out.String(), unsafe) || strings.Contains(errOut.String(), unsafe) || strings.Contains(err.Error(), unsafe) {
			t.Fatalf("ambiguous output exposed %q: err=%q stdout=%q stderr=%q", unsafe, err, out.String(), errOut.String())
		}
	}
	if !strings.Contains(errOut.String(), "recovery_file") ||
		!strings.Contains(errOut.String(), "replay instruction: clawctl-hub deployment create") {
		t.Fatalf("missing recovery instruction: %s", errOut.String())
	}

	recoveryDir := filepath.Join(stateRoot, "clawctl", "deployment-recovery")
	dirInfo, err := os.Stat(recoveryDir)
	if err != nil || dirInfo.Mode().Perm() != 0o700 {
		t.Fatalf("recovery dir info=%v err=%v", dirInfo, err)
	}
	entries, err := os.ReadDir(recoveryDir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("recovery entries=%v err=%v", entries, err)
	}
	recoveryPath := filepath.Join(recoveryDir, entries[0].Name())
	fileInfo, err := os.Lstat(recoveryPath)
	if err != nil || !fileInfo.Mode().IsRegular() || fileInfo.Mode().Perm() != 0o600 {
		t.Fatalf("recovery file info=%v err=%v", fileInfo, err)
	}
	document, exists, err := loadDeploymentRecoveryIfExists(recoveryPath)
	if err != nil || !exists || document.Reason != privateReason ||
		document.Planning.ArtifactSHA256 != "" || document.Transport.HubURL != base {
		t.Fatalf("recovery document=%+v exists=%t err=%v", document, exists, err)
	}
	var deployments int
	if err := f.store.DB().QueryRow(`SELECT COUNT(*) FROM deployments`).Scan(&deployments); err != nil || deployments != 1 {
		t.Fatalf("ambiguous commit deployments=%d err=%v", deployments, err)
	}

	out.Reset()
	errOut.Reset()
	if err := runDeploymentMutationCommandWithDeps(t.Context(), []string{
		"create", "--recovery-file", recoveryPath, "--json",
	}, &out, &errOut, deps); err != nil {
		t.Fatalf("recovery replay: %v stdout=%q stderr=%q", err, out.String(), errOut.String())
	}
	var replay deploymentMutationCLIResult
	if err := json.Unmarshal(out.Bytes(), &replay); err != nil || !replay.Result.Replayed ||
		replay.Result.Action != "create" {
		t.Fatalf("replay=%+v decode=%v stdout=%s", replay, err, out.String())
	}
	assertSafeDeploymentCLIJSON(t, out.Bytes(), privateReason, record.TarballURL, record.FetchedBy)
	if strings.Contains(errOut.String(), privateReason) {
		t.Fatalf("replay stderr exposed reason: %s", errOut.String())
	}
	if _, err := os.Lstat(recoveryPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("validated replay did not remove recovery file: %v", err)
	}
	if previewCalls.Load() != 1 || applyCalls.Load() != 2 {
		t.Fatalf("HTTP calls preview=%d apply=%d", previewCalls.Load(), applyCalls.Load())
	}
	if !reflect.DeepEqual(clientURLs, []string{base + "/", base}) {
		t.Fatalf("discovery/recovery URLs=%v, want raw then normalized", clientURLs)
	}
	if _, err := os.Stat(missingDB); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("HTTP recovery touched fallback DB: %v", err)
	}
}

func TestDeploymentRecoveryRejectsUnsafeModeAndNeverOverwrites(t *testing.T) {
	inputs := deploymentRecoveryTestInputs()
	document := deploymentRecoveryFromInputs(inputs)
	raw, err := marshalDeploymentRecovery(document)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("unsafe mode", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.Chmod(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, "recovery.json")
		if err := os.WriteFile(path, raw, 0o644); err != nil {
			t.Fatal(err)
		}
		var out, errOut bytes.Buffer
		err := runDeploymentMutationCommandWithDeps(t.Context(), []string{
			"create", "--recovery-file", path,
		}, &out, &errOut, machineCommandDeps{
			discoverHubURL: func() (string, error) {
				t.Fatal("unsafe recovery reached discovery")
				return "", nil
			},
		})
		if err == nil || !strings.Contains(err.Error(), "0600") || out.Len() != 0 || errOut.Len() != 0 {
			t.Fatalf("unsafe mode err=%v stdout=%q stderr=%q", err, out.String(), errOut.String())
		}
	})

	t.Run("race no clobber", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.Chmod(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, "recovery.json")
		poison := []byte("PRIVATE_POISON\n\x1b[31m")
		inputs := deploymentRecoveryTestInputs()
		inputs.PreviewDigest = ""
		inputs.IdempotencyKey = ""
		inputs.RecoveryFile = path
		calledApply := false
		operations := deploymentMutationOperations{
			source: "test",
			previewCreate: func(operator.DeploymentCreatePreviewRequest) (operator.DeploymentCreatePreviewResult, error) {
				if err := os.WriteFile(path, poison, 0o600); err != nil {
					t.Fatal(err)
				}
				return operator.DeploymentCreatePreviewResult{PreviewDigest: recoveryTestPreviewDigest()}, nil
			},
			apply: func(deploymentMutationInputs) (operator.DeploymentMutationResult, error) {
				calledApply = true
				return operator.DeploymentMutationResult{}, nil
			},
		}
		var out, errOut bytes.Buffer
		err := executeDeploymentMutation(inputs, operations, &out, &errOut)
		if err == nil || calledApply {
			t.Fatalf("no-clobber err=%v called_apply=%t", err, calledApply)
		}
		got, readErr := os.ReadFile(path)
		if readErr != nil || !bytes.Equal(got, poison) {
			t.Fatalf("existing file overwritten got=%q err=%v", got, readErr)
		}
		if strings.Contains(out.String(), string(poison)) || strings.Contains(errOut.String(), string(poison)) ||
			strings.Contains(err.Error(), "PRIVATE_POISON") {
			t.Fatalf("poison leaked err=%q stdout=%q stderr=%q", err, out.String(), errOut.String())
		}
	})
}

func TestDeploymentExplicitCoordinatesCreateDefaultRecoveryBeforeApply(t *testing.T) {
	stateRoot := t.TempDir()
	if err := os.Chmod(stateRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_STATE_HOME", stateRoot)
	inputs := deploymentRecoveryTestInputs()
	calledPreview := false
	operations := deploymentMutationOperations{
		source: "HTTP operator API",
		previewCreate: func(operator.DeploymentCreatePreviewRequest) (operator.DeploymentCreatePreviewResult, error) {
			calledPreview = true
			return operator.DeploymentCreatePreviewResult{}, nil
		},
		apply: func(deploymentMutationInputs) (operator.DeploymentMutationResult, error) {
			return operator.DeploymentMutationResult{}, io.ErrUnexpectedEOF
		},
	}
	var out, errOut bytes.Buffer
	err := executeDeploymentMutation(inputs, operations, &out, &errOut)
	if err == nil || calledPreview || out.Len() != 0 {
		t.Fatalf("explicit-coordinate err=%v preview=%t stdout=%q", err, calledPreview, out.String())
	}
	entries, readErr := os.ReadDir(filepath.Join(stateRoot, "clawctl", "deployment-recovery"))
	if readErr != nil || len(entries) != 1 {
		t.Fatalf("default recovery entries=%v err=%v", entries, readErr)
	}
}

func TestDeploymentRecoveryStaysUntilResultIsWritten(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "recovery.json")
	inputs := deploymentRecoveryTestInputs()
	inputs.RecoveryFile = path
	operations := deploymentMutationOperations{
		source: "HTTP operator API",
		apply: func(deploymentMutationInputs) (operator.DeploymentMutationResult, error) {
			return operator.DeploymentMutationResult{Action: "create", Replayed: true}, nil
		},
	}
	err := executeDeploymentMutation(inputs, operations, failingDeploymentWriter{}, io.Discard)
	if err == nil {
		t.Fatal("result writer failure was ignored")
	}
	if _, exists, loadErr := loadDeploymentRecoveryIfExists(path); loadErr != nil || !exists {
		t.Fatalf("writer failure removed recovery exists=%t err=%v", exists, loadErr)
	}
}

func TestDeploymentRecoveryDigestBindsKeyAndTransport(t *testing.T) {
	document := deploymentRecoveryFromInputs(deploymentRecoveryTestInputs())
	for _, tc := range []struct {
		name   string
		tamper func(*deploymentMutationRecovery)
	}{
		{name: "idempotency key", tamper: func(got *deploymentMutationRecovery) {
			got.IdempotencyKey = "different-replay-key"
		}},
		{name: "HTTP authority", tamper: func(got *deploymentMutationRecovery) {
			got.Transport.HubURL = "http://100.64.0.10:8787"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tampered := document
			tc.tamper(&tampered)
			raw, err := marshalDeploymentRecovery(tampered)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := decodeDeploymentRecovery(raw); err == nil || !strings.Contains(err.Error(), "digest") {
				t.Fatalf("tampered canonical receipt accepted: %v", err)
			}
		})
	}
}

func TestDeploymentRecoveryRejectsZeroOpenedBatch(t *testing.T) {
	revision, opened := int64(0), 0
	document := deploymentRecoveryFromInputs(deploymentMutationInputs{
		Action: "continue", DeploymentID: strings.Repeat("d", 32), ConfirmChannel: "canary",
		Reason: "continue", IdempotencyKey: "zero-opened-batch-recovery",
		PreviewDigest: recoveryTestPreviewDigest(), ExpectedControlRevision: &revision,
		ExpectedOpenedBatch: &opened,
		RecoveryTransport:   deploymentRecoveryTransport{Mode: deploymentRecoveryHTTP, HubURL: "http://100.64.0.9:8787"},
	})
	if err := validateDeploymentRecoveryDocument(document); err == nil {
		t.Fatal("recovery file accepted expected_opened_batch=0")
	}
}

func TestDeploymentRecoveryRoundTripsEveryMutationAction(t *testing.T) {
	revision, opened := int64(7), 3
	deploymentID := strings.Repeat("d", 32)
	cases := []deploymentMutationInputs{
		deploymentRecoveryTestInputs(),
		{
			Action: "continue", DeploymentID: deploymentID, ConfirmChannel: "canary",
			Reason: "continue private reason", IdempotencyKey: "continue-recovery-key",
			PreviewDigest: recoveryTestPreviewDigest(), ExpectedControlRevision: &revision,
			ExpectedOpenedBatch: &opened,
			RecoveryTransport:   deploymentRecoveryTransport{Mode: deploymentRecoveryHTTP, HubURL: "http://100.64.0.9:8787"},
		},
		{
			Action: "retry", DeploymentID: deploymentID, ConfirmChannel: "stable", ConfirmVersion: "2026.9.8",
			Reason: "retry private reason", IdempotencyKey: "retry-recovery-key",
			PreviewDigest: recoveryTestPreviewDigest(), ExpectedControlRevision: &revision,
			ExpectedOpenedBatch: &opened,
			RecoveryTransport:   deploymentRecoveryTransport{Mode: deploymentRecoveryHTTP, HubURL: "http://100.64.0.9:8787"},
		},
		{
			Action: "abandon", DeploymentID: deploymentID, ConfirmDeploymentID: deploymentID,
			Reason: "abandon private reason", IdempotencyKey: "abandon-recovery-key",
			PreviewDigest: recoveryTestPreviewDigest(), ExpectedControlRevision: &revision,
			ExpectedOpenedBatch: &opened,
			RecoveryTransport:   deploymentRecoveryTransport{Mode: deploymentRecoveryDirectDB, DBPath: "/var/lib/clawctl/hub.sqlite"},
		},
	}
	for _, original := range cases {
		t.Run(original.Action, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.Chmod(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, original.Action+".json")
			document := deploymentRecoveryFromInputs(original)
			if err := validateDeploymentRecoveryDocument(document); err != nil {
				t.Fatalf("validate source document: %v", err)
			}
			if err := writeDeploymentRecoveryNoClobber(path, document); err != nil {
				t.Fatal(err)
			}
			parsed, transport, err := parseDeploymentMutationCommand([]string{
				original.Action, "--recovery-file", path, "--json",
			}, io.Discard)
			if err != nil {
				t.Fatalf("parse replay: %v", err)
			}
			if !parsed.RecoveryLoaded || parsed.RecoveryFile != path || !parsed.JSON ||
				!reflect.DeepEqual(deploymentRecoveryFromInputs(parsed), document) {
				t.Fatalf("round trip changed canonical request: parsed=%+v document=%+v", parsed, document)
			}
			if original.RecoveryTransport.Mode == deploymentRecoveryHTTP {
				if !transport.explicit["hub-url"] || transport.hubURL != original.RecoveryTransport.HubURL ||
					transport.explicit["db"] {
					t.Fatalf("HTTP transport changed: %+v", transport)
				}
			} else if !transport.explicit["db"] || transport.dbPath != original.RecoveryTransport.DBPath ||
				transport.explicit["hub-url"] {
				t.Fatalf("direct transport changed: %+v", transport)
			}
		})
	}
}

func TestDeploymentRecoveryLoadedFileIsReverifiedImmediatelyBeforeApply(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "recovery.json")
	inputs := deploymentRecoveryTestInputs()
	inputs.RecoveryFile = path
	document := deploymentRecoveryFromInputs(inputs)
	if err := writeDeploymentRecoveryNoClobber(path, document); err != nil {
		t.Fatal(err)
	}
	inputs.RecoveryLoaded = true
	calledApply := false
	operations := deploymentMutationOperations{
		source: "HTTP operator API",
		apply: func(deploymentMutationInputs) (operator.DeploymentMutationResult, error) {
			calledApply = true
			return operator.DeploymentMutationResult{}, nil
		},
	}
	errOut := &deploymentRecoveryRemovingWriter{path: path}
	err := executeDeploymentMutation(inputs, operations, io.Discard, errOut)
	if err == nil || calledApply || !strings.Contains(err.Error(), "apply 前") {
		t.Fatalf("changed loaded receipt err=%v called_apply=%t", err, calledApply)
	}
}

func TestDefaultDeploymentRecoveryPathHashesUntrustedKey(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_STATE_HOME", root)
	const key = "../../outside/$(PRIVATE_KEY_MUST_NOT_APPEAR)"
	path, err := defaultDeploymentRecoveryPath("create", key)
	if err != nil {
		t.Fatal(err)
	}
	wantDir := filepath.Join(root, "clawctl", "deployment-recovery")
	if filepath.Dir(path) != wantDir || strings.Contains(path, key) || strings.Contains(path, "PRIVATE_KEY") {
		t.Fatalf("untrusted key shaped recovery path: %q", path)
	}
	base := strings.TrimSuffix(filepath.Base(path), ".json")
	if len(base) != 64 || !artifact.ValidSHA256Hex(base) {
		t.Fatalf("recovery basename is not fixed SHA-256: %q", filepath.Base(path))
	}
}

func TestDefaultDeploymentRecoveryPathAllowsExistingSameOwnerLocalAncestor(t *testing.T) {
	home := t.TempDir()
	if err := os.Chmod(home, 0o750); err != nil {
		t.Fatal(err)
	}
	local := filepath.Join(home, ".local")
	state := filepath.Join(local, "state")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(local, 0o775); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(state, 0o700); err != nil {
		t.Fatal(err)
	}
	oldXDG, hadXDG := os.LookupEnv("XDG_STATE_HOME")
	if err := os.Unsetenv("XDG_STATE_HOME"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if hadXDG {
			_ = os.Setenv("XDG_STATE_HOME", oldXDG)
		} else {
			_ = os.Unsetenv("XDG_STATE_HOME")
		}
	})
	t.Setenv("HOME", home)
	path, err := defaultDeploymentRecoveryPath("create", "home-fallback-key")
	if err != nil {
		t.Fatalf("HOME fallback rejected existing same-owner .local mode 0775: %v", err)
	}
	if filepath.Dir(path) != filepath.Join(state, "clawctl", "deployment-recovery") {
		t.Fatalf("HOME fallback path=%q", path)
	}
}

func TestDefaultDeploymentRecoveryPathDurablyCreatesFreshNestedStateRoot(t *testing.T) {
	base := t.TempDir()
	if err := os.Chmod(base, 0o700); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(base, "fresh", "nested-state")
	t.Setenv("XDG_STATE_HOME", root)
	var syncCalls int
	path, err := defaultDeploymentRecoveryPathWithSync("create", "durable-key", func(fd int) error {
		syncCalls++
		return unix.Fsync(fd)
	})
	if err != nil {
		t.Fatal(err)
	}
	wantDir := filepath.Join(root, "clawctl", "deployment-recovery")
	if filepath.Dir(path) != wantDir {
		t.Fatalf("recovery path=%q want parent=%q", path, wantDir)
	}
	for _, dir := range []string{filepath.Join(base, "fresh"), root, filepath.Join(root, "clawctl"), wantDir} {
		info, err := os.Lstat(dir)
		if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
			t.Fatalf("durable directory %s info=%v err=%v", dir, info, err)
		}
	}
	// Each of four new directories is synced itself and then published by
	// syncing its parent. Existing components are also re-synced so a retry
	// repairs an earlier invocation that created an entry but failed its sync.
	if syncCalls < 8 {
		t.Fatalf("directory sync calls=%d, want at least 8", syncCalls)
	}
}

func TestDefaultDeploymentRecoveryPathFailsClosedOnDirectorySyncError(t *testing.T) {
	base := t.TempDir()
	if err := os.Chmod(base, 0o700); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(base, "fresh-state")
	t.Setenv("XDG_STATE_HOME", root)
	want := errors.New("injected directory sync failure")
	var calls int
	_, err := defaultDeploymentRecoveryPathWithSync("retry", "durable-key", func(int) error {
		calls++
		if _, statErr := os.Lstat(root); statErr == nil {
			return want
		}
		return nil
	})
	if err == nil || calls == 0 {
		t.Fatalf("sync fault err=%v calls=%d", err, calls)
	}
	if _, statErr := os.Lstat(root); statErr != nil {
		t.Fatalf("fault did not leave the just-created directory for retry: %v", statErr)
	}
	var retrySyncs int
	if _, err := defaultDeploymentRecoveryPathWithSync("retry", "durable-key", func(fd int) error {
		retrySyncs++
		return unix.Fsync(fd)
	}); err != nil {
		t.Fatalf("retry did not repair/sync existing directory: %v", err)
	}
	if retrySyncs == 0 {
		t.Fatal("retry skipped all durability syncs")
	}
}

func TestDeploymentRecoveryRetryRepairsReceiptParentSyncFailureBeforeApply(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "recovery.json")
	document := deploymentRecoveryFromInputs(deploymentRecoveryTestInputs())
	want := errors.New("injected receipt parent sync failure")
	if err := writeDeploymentRecoveryNoClobberWithSync(path, document, func(int) error { return want }); err == nil {
		t.Fatal("receipt publication ignored parent sync failure")
	}
	if _, exists, err := loadDeploymentRecoveryIfExists(path); err != nil || !exists {
		t.Fatalf("failed publication did not leave a valid no-clobber receipt exists=%t err=%v", exists, err)
	}
	if err := ensureDeploymentRecovery(path, document, false); err != nil {
		t.Fatalf("retry could not adopt exact receipt: %v", err)
	}
	var barrierSyncs int
	if err := durablyVerifyDeploymentRecoveryWithSync(path, document, func(fd int) error {
		barrierSyncs++
		return unix.Fsync(fd)
	}); err != nil {
		t.Fatalf("retry durability barrier: %v", err)
	}
	if barrierSyncs != 2 {
		t.Fatalf("pre-apply barrier syncs=%d, want receipt+parent", barrierSyncs)
	}
	barrierSyncs = 0
	if err := durablyVerifyDeploymentRecoveryWithSync(path, document, func(int) error {
		barrierSyncs++
		return want
	}); err == nil || barrierSyncs != 1 {
		t.Fatalf("failed barrier err=%v syncs=%d; apply must remain blocked", err, barrierSyncs)
	}
}

func TestRemoveDeploymentRecoveryDoesNotDeleteSwappedFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "recovery.json")
	saved := filepath.Join(dir, "verified-original.json")
	document := deploymentRecoveryFromInputs(deploymentRecoveryTestInputs())
	if err := writeDeploymentRecoveryNoClobber(path, document); err != nil {
		t.Fatal(err)
	}
	unrelated := []byte("UNRELATED_PRIVATE_FILE\n")
	err := removeDeploymentRecoveryWithHook(path, document, func() {
		if err := os.Rename(path, saved); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, unrelated, 0o600); err != nil {
			t.Fatal(err)
		}
	})
	if err == nil || !strings.Contains(err.Error(), "unlink 前改變") {
		t.Fatalf("swapped recovery cleanup err=%v", err)
	}
	got, readErr := os.ReadFile(path)
	if readErr != nil || !bytes.Equal(got, unrelated) {
		t.Fatalf("cleanup deleted/replaced unrelated file got=%q err=%v", got, readErr)
	}
	if _, exists, loadErr := loadDeploymentRecoveryIfExists(saved); loadErr != nil || !exists {
		t.Fatalf("verified original unexpectedly removed exists=%t err=%v", exists, loadErr)
	}
}

type failingDeploymentWriter struct{}

func (failingDeploymentWriter) Write([]byte) (int, error) {
	return 0, errors.New("test output unavailable")
}

type deploymentRecoveryRemovingWriter struct {
	path string
}

func (w *deploymentRecoveryRemovingWriter) Write(p []byte) (int, error) {
	if strings.Contains(string(p), "canonical retry:") {
		if err := os.Remove(w.path); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

func deploymentRecoveryTestInputs() deploymentMutationInputs {
	return deploymentMutationInputs{
		Action: "create",
		Planning: operator.DeploymentCreatePreviewRequest{
			Channel: "canary", Version: "2026.9.8", BatchSize: 1, ExecutionTimeoutSeconds: 600,
		},
		ConfirmChannel: "canary", ConfirmVersion: "2026.9.8", Reason: "private recovery test reason",
		IdempotencyKey: "cli-deployment-create-recovery-test", PreviewDigest: recoveryTestPreviewDigest(), JSON: true,
		RecoveryTransport: deploymentRecoveryTransport{Mode: deploymentRecoveryHTTP, HubURL: "http://100.64.0.9:8787"},
	}
}

func recoveryTestPreviewDigest() string {
	return "sha256:" + strings.Repeat("a", 64)
}
