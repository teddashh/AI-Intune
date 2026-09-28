package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/store"
)

func newPendingEnrollmentForRevokeCLI(t *testing.T, st *store.Store, displayName string) string {
	t.Helper()
	machineID, _, err := st.CreateEnrollTokenFor(displayName, time.Hour)
	if err != nil {
		t.Fatalf("create pending enrollment ticket: %v", err)
	}
	return machineID
}

func assertEnrollTokenRevokeSafetyOutput(t *testing.T, output string) {
	t.Helper()
	if (!strings.Contains(output, "保留名冊") && !strings.Contains(output, "名冊保留")) ||
		!strings.Contains(output, "分母變化 0") ||
		!strings.Contains(output, "已啟用 agent credential 不受影響") {
		t.Fatalf("revoke output lacks explicit registry/denominator/active-credential impact: %q", output)
	}
}

func pendingEnrollmentRows(t *testing.T, st *store.Store, machineID string) int {
	t.Helper()
	var rows int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM enrollment_tokens
WHERE used_by=? AND used_at IS NULL`, machineID).Scan(&rows); err != nil {
		t.Fatalf("count pending enrollment tickets: %v", err)
	}
	return rows
}

func TestEnrollTokenRevokeCLIHTTPAutoPreviewFreshThenReplayAndConflict(t *testing.T) {
	f := observedOperatorFixture(t)
	machineID := newPendingEnrollmentForRevokeCLI(t, f.store, "pending-http-revoke")

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

	const reason = "ticket copied into the wrong channel"
	var firstOut, firstErr bytes.Buffer
	if err := runEnrollTokenCommandWithDeps(t.Context(), []string{
		"revoke", "--hub-url", base, "--reason", reason, machineID,
	}, &firstOut, &firstErr, deps); err != nil {
		t.Fatalf("fresh HTTP revoke: %v; stdout=%q stderr=%q", err, firstOut.String(), firstErr.String())
	}
	if !strings.HasPrefix(firstOut.String(), "revoked:") || !strings.HasPrefix(firstErr.String(), "preview:") {
		t.Fatalf("auto preview/fresh output stdout=%q stderr=%q", firstOut.String(), firstErr.String())
	}
	assertEnrollTokenRevokeSafetyOutput(t, firstOut.String())
	assertEnrollTokenRevokeSafetyOutput(t, firstErr.String())
	keyMatch := regexp.MustCompile(`idempotency-key=(cli-enroll-token-revoke-[a-f0-9]{32})`).FindStringSubmatch(firstOut.String())
	digestMatch := regexp.MustCompile(`preview-digest=(sha256:[a-f0-9]{64})`).FindStringSubmatch(firstOut.String())
	if len(keyMatch) != 2 || len(digestMatch) != 2 {
		t.Fatalf("fresh receipt lacks retry coordinates: %q", firstOut.String())
	}
	key, digest := keyMatch[1], digestMatch[1]
	if got := pendingEnrollmentRows(t, f.store, machineID); got != 0 {
		t.Fatalf("fresh revoke left %d pending ticket rows", got)
	}
	var registryRows, expected, retired int
	if err := f.store.DB().QueryRow(`SELECT COUNT(*), COALESCE(MAX(expected),0),
COALESCE(MAX(CASE WHEN retired_at IS NOT NULL THEN 1 ELSE 0 END),0)
FROM machine_registry WHERE machine_id=?`, machineID).Scan(&registryRows, &expected, &retired); err != nil {
		t.Fatal(err)
	}
	if registryRows != 1 || expected != 1 || retired != 0 {
		t.Fatalf("revoke changed registry/denominator membership: rows=%d expected=%d retired=%d",
			registryRows, expected, retired)
	}
	if _, err := os.Stat(missingDB); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("HTTP mode touched direct DB: %v", err)
	}

	retryArgs := []string{
		"revoke", "--hub-url", base, "--reason", reason,
		"--idempotency-key", key, "--preview-digest", digest, machineID,
	}
	var replayOut, replayErr bytes.Buffer
	if err := runEnrollTokenCommandWithDeps(t.Context(), retryArgs, &replayOut, &replayErr, deps); err != nil {
		t.Fatalf("successful replay: %v; stdout=%q stderr=%q", err, replayOut.String(), replayErr.String())
	}
	if !strings.HasPrefix(replayOut.String(), "replayed:") || replayErr.Len() != 0 {
		t.Fatalf("replay output stdout=%q stderr=%q", replayOut.String(), replayErr.String())
	}
	assertEnrollTokenRevokeSafetyOutput(t, replayOut.String())
	if got := pendingEnrollmentRows(t, f.store, machineID); got != 0 {
		t.Fatalf("replay recreated or re-deleted state: pending=%d", got)
	}
	var ledgerRows int
	if err := f.store.DB().QueryRow(
		`SELECT COUNT(*) FROM operator_idempotency WHERE idempotency_key=?`, key).Scan(&ledgerRows); err != nil {
		t.Fatal(err)
	}
	if ledgerRows != 1 {
		t.Fatalf("fresh/replay ledger rows=%d want 1", ledgerRows)
	}

	conflictArgs := append([]string(nil), retryArgs...)
	for i := range conflictArgs {
		if conflictArgs[i] == reason {
			conflictArgs[i] = "different semantic reason"
		}
	}
	var conflictOut, conflictErr bytes.Buffer
	err := runEnrollTokenCommandWithDeps(t.Context(), conflictArgs, &conflictOut, &conflictErr, deps)
	if err == nil || !strings.Contains(err.Error(), "IDEMPOTENCY_CONFLICT") || conflictOut.Len() != 0 {
		t.Fatalf("same-key changed-reason conflict err=%v stdout=%q stderr=%q",
			err, conflictOut.String(), conflictErr.String())
	}

	wantPath := "/v1/operator/machines/" + machineID + "/enrollment-token/"
	mu.Lock()
	gotCalls := append([]string(nil), calls...)
	mu.Unlock()
	wantCalls := []string{
		"POST " + wantPath + "revocation-preview",
		"POST " + wantPath + "revocations",
		"POST " + wantPath + "revocations",
		"POST " + wantPath + "revocations",
	}
	if strings.Join(gotCalls, "\n") != strings.Join(wantCalls, "\n") {
		t.Fatalf("HTTP revoke calls=%v want=%v", gotCalls, wantCalls)
	}
}

func TestEnrollTokenRevokeCLIPreviewDoesNotMutate(t *testing.T) {
	f := observedOperatorFixture(t)
	machineID := newPendingEnrollmentForRevokeCLI(t, f.store, "pending-preview-revoke")
	server := httptest.NewServer(f.mux)
	defer server.Close()
	base, deps := machineHTTPTestDeps(t, server)

	var beforeRegistry, beforeTokens, beforeLedger, beforeAudit int
	for query, target := range map[string]*int{
		`SELECT COUNT(*) FROM machine_registry`:     &beforeRegistry,
		`SELECT COUNT(*) FROM enrollment_tokens`:    &beforeTokens,
		`SELECT COUNT(*) FROM operator_idempotency`: &beforeLedger,
		`SELECT COUNT(*) FROM audit_log`:            &beforeAudit,
	} {
		if err := f.store.DB().QueryRow(query).Scan(target); err != nil {
			t.Fatal(err)
		}
	}

	var out, errOut bytes.Buffer
	if err := runEnrollTokenCommandWithDeps(t.Context(), []string{
		"revoke", "--preview", "--hub-url", base, machineID,
	}, &out, &errOut, deps); err != nil {
		t.Fatalf("HTTP revoke preview: %v; stdout=%q stderr=%q", err, out.String(), errOut.String())
	}
	if !strings.HasPrefix(out.String(), "preview:") || !strings.Contains(out.String(), "preview-digest=sha256:") || errOut.Len() != 0 {
		t.Fatalf("preview output stdout=%q stderr=%q", out.String(), errOut.String())
	}
	assertEnrollTokenRevokeSafetyOutput(t, out.String())

	var afterRegistry, afterTokens, afterLedger, afterAudit int
	for query, target := range map[string]*int{
		`SELECT COUNT(*) FROM machine_registry`:     &afterRegistry,
		`SELECT COUNT(*) FROM enrollment_tokens`:    &afterTokens,
		`SELECT COUNT(*) FROM operator_idempotency`: &afterLedger,
		`SELECT COUNT(*) FROM audit_log`:            &afterAudit,
	} {
		if err := f.store.DB().QueryRow(query).Scan(target); err != nil {
			t.Fatal(err)
		}
	}
	if afterRegistry != beforeRegistry || afterTokens != beforeTokens ||
		afterLedger != beforeLedger || afterAudit != beforeAudit ||
		pendingEnrollmentRows(t, f.store, machineID) != 1 {
		t.Fatalf("preview mutated state: registry %d→%d tokens %d→%d ledger %d→%d audit %d→%d",
			beforeRegistry, afterRegistry, beforeTokens, afterTokens,
			beforeLedger, afterLedger, beforeAudit, afterAudit)
	}
}

func TestEnrollTokenRevokeCLIRetryCoordinatesMustBePaired(t *testing.T) {
	deps := machineCommandDeps{
		discoverHubURL: func() (string, error) {
			t.Fatal("unpaired retry coordinates reached discovery")
			return "", nil
		},
	}
	for _, args := range [][]string{
		{"revoke", "--idempotency-key", "retry-key", "machine-1"},
		{"revoke", "--preview-digest", "sha256:" + strings.Repeat("a", 64), "machine-1"},
	} {
		var out, errOut bytes.Buffer
		err := runEnrollTokenCommandWithDeps(t.Context(), args, &out, &errOut, deps)
		if err == nil || !strings.Contains(err.Error(), "必須成對提供") || out.Len() != 0 {
			t.Fatalf("args=%v err=%v stdout=%q stderr=%q", args, err, out.String(), errOut.String())
		}
	}
}

func TestEnrollTokenRevokeCLIDiscoveryFailureNeverFallsBackToDB(t *testing.T) {
	missingDB := filepath.Join(t.TempDir(), "must-not-be-created.sqlite")
	t.Setenv("CLAWCTL_DB", missingDB)
	deps := machineCommandDeps{
		discoverHubURL: func() (string, error) { return "", errors.New("discovery unavailable") },
		openDirectDB: func(string) (*store.Store, error) {
			t.Fatal("discovery failure fell through to direct DB")
			return nil, nil
		},
	}
	var out, errOut bytes.Buffer
	err := runEnrollTokenCommandWithDeps(t.Context(), []string{
		"revoke", "machine-1",
	}, &out, &errOut, deps)
	if err == nil || !strings.Contains(err.Error(), "discovery unavailable") ||
		out.Len() != 0 {
		t.Fatalf("discovery error=%v stdout=%q stderr=%q", err, out.String(), errOut.String())
	}
	if _, err := os.Stat(missingDB); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("discovery failure created fallback DB: %v", err)
	}
}

func TestEnrollTokenRevokeCLIExplicitDBUsesSharedStoppedServiceFence(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(dir, "clawctl.sqlite")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	machineID := newPendingEnrollmentForRevokeCLI(t, st, "pending-direct-revoke")
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	deps := productionMachineCommandDeps()
	deps.discoverHubURL = func() (string, error) {
		t.Fatal("explicit --db invoked discovery")
		return "", nil
	}
	originalAcquire := deps.acquireDirect
	originalValidateLedger := deps.validateDirectLedger
	originalOpen := deps.openDirectDB
	acquired, ledgerValidated, stopped := false, false, false
	deps.acquireDirect = func(path string) (io.Closer, error) {
		guard, err := originalAcquire(path)
		if err == nil {
			acquired = true
		}
		return guard, err
	}
	deps.validateDirectLedger = func(path string) error {
		err := originalValidateLedger(path)
		if err == nil {
			ledgerValidated = true
		}
		return err
	}
	deps.verifyHubStopped = func(context.Context, string) error {
		stopped = true
		return nil
	}
	deps.openDirectDB = func(path string) (*store.Store, error) {
		if !acquired || !ledgerValidated || !stopped {
			t.Fatalf("writable DB open preceded shared safety fence: acquired=%t ledger=%t stopped=%t",
				acquired, ledgerValidated, stopped)
		}
		return originalOpen(path)
	}

	var out, errOut bytes.Buffer
	if err := runEnrollTokenCommandWithDeps(t.Context(), []string{
		"revoke", "--db", dbPath, "--reason", "break-glass ticket revoke", machineID,
	}, &out, &errOut, deps); err != nil {
		t.Fatalf("direct revoke: %v; stdout=%q stderr=%q", err, out.String(), errOut.String())
	}
	if !strings.HasPrefix(out.String(), "revoked:") || !strings.HasPrefix(errOut.String(), "preview:") {
		t.Fatalf("direct revoke output stdout=%q stderr=%q", out.String(), errOut.String())
	}
	assertEnrollTokenRevokeSafetyOutput(t, out.String())
	assertEnrollTokenRevokeSafetyOutput(t, errOut.String())

	st, err = openExisting(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if got := pendingEnrollmentRows(t, st, machineID); got != 0 {
		t.Fatalf("direct revoke left %d pending ticket rows", got)
	}
	entries, err := st.Audit(machineID, 10)
	if err != nil || len(entries) != 1 || entries[0].Action != store.AuditRevokeToken ||
		entries[0].SourceKind != operator.SourceKindDirectDBCLI || entries[0].WhoUnavailable == "" {
		t.Fatalf("direct revoke audit=%+v err=%v", entries, err)
	}
}
