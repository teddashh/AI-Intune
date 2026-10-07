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
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/ledgerlock"
	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/store"
)

func TestEnrollTokenCLIHTTPFreshThenRedactedReplay(t *testing.T) {
	f := observedOperatorFixture(t)
	server := httptest.NewServer(f.mux)
	defer server.Close()
	base, deps := machineHTTPTestDeps(t, server)
	missingDB := filepath.Join(t.TempDir(), "must-not-be-created.sqlite")
	t.Setenv("CLAWCTL_DB", missingDB)
	args := []string{
		"--hub-url", base, "--ttl", "2h", "--reason", "HTTP CLI acceptance",
		"new-http-machine",
	}

	var firstOut, firstErr bytes.Buffer
	if err := runEnrollTokenCommandWithDeps(t.Context(), args, &firstOut, &firstErr, deps); err != nil {
		t.Fatalf("fresh HTTP enrollment: %v; stderr=%s", err, firstErr.String())
	}
	token := strings.TrimSpace(firstOut.String())
	if len(token) != 43 || strings.Contains(firstErr.String(), token) {
		t.Fatalf("secret delivery was not exactly stdout once: token_len=%d stderr_contains=%t stderr=%q",
			len(token), strings.Contains(firstErr.String(), token), firstErr.String())
	}
	if !strings.Contains(firstErr.String(), "added to roster") {
		t.Fatalf("fresh stderr lacks effect/recovery coordinate: %q", firstErr.String())
	}
	if !strings.Contains(firstErr.String(), "./install-agent.sh --hub ") ||
		!strings.Contains(firstErr.String(), "./install-agent-macos.sh --hub ") ||
		!strings.Contains(firstErr.String(), `.\install-agent-windows.ps1 --hub `) ||
		strings.Contains(firstErr.String(), "--token '") {
		t.Fatalf("fresh stderr lacks secret-free bootstrap command: %q", firstErr.String())
	}
	keyMatch := regexp.MustCompile(`request key: (cli-enroll-token-[a-f0-9]{32})`).FindStringSubmatch(firstErr.String())
	digestMatch := regexp.MustCompile(`preview-digest=(sha256:[a-f0-9]{64})`).FindStringSubmatch(firstErr.String())
	if len(keyMatch) != 2 || len(digestMatch) != 2 {
		t.Fatalf("fresh stderr lacks exact retry coordinates: %q", firstErr.String())
	}
	key, previewDigest := keyMatch[1], digestMatch[1]
	if _, err := os.Stat(missingDB); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("HTTP mode touched direct DB: %v", err)
	}

	replayArgs := []string{
		"--hub-url", base, "--ttl", "2h", "--reason", "HTTP CLI acceptance",
		"--idempotency-key", key, "--preview-digest", previewDigest, "new-http-machine",
	}
	var replayOut, replayErr bytes.Buffer
	err := runEnrollTokenCommandWithDeps(t.Context(), replayArgs, &replayOut, &replayErr, deps)
	if err == nil || !strings.Contains(err.Error(), "token cannot be redisplayed") ||
		!strings.Contains(err.Error(), key) {
		t.Fatalf("replay error=%v stderr=%q", err, replayErr.String())
	}
	if replayOut.Len() != 0 || strings.Contains(replayErr.String(), token) || strings.Contains(err.Error(), token) {
		t.Fatalf("replay leaked/re-emitted token: stdout=%q stderr=%q err=%v", replayOut.String(), replayErr.String(), err)
	}

	var machineID string
	if err := f.store.DB().QueryRow(`SELECT machine_id FROM machine_registry WHERE display_name='new-http-machine'`).Scan(&machineID); err != nil {
		t.Fatal(err)
	}
	var registryRows, tokenRows, ledgerRows int
	if err := f.store.DB().QueryRow(`SELECT COUNT(*) FROM machine_registry WHERE display_name='new-http-machine'`).Scan(&registryRows); err != nil {
		t.Fatal(err)
	}
	if err := f.store.DB().QueryRow(`SELECT COUNT(*) FROM enrollment_tokens WHERE used_by=?`, machineID).Scan(&tokenRows); err != nil {
		t.Fatal(err)
	}
	if err := f.store.DB().QueryRow(`SELECT COUNT(*) FROM operator_idempotency WHERE idempotency_key=?`, key).Scan(&ledgerRows); err != nil {
		t.Fatal(err)
	}
	if registryRows != 1 || tokenRows != 1 || ledgerRows != 1 {
		t.Fatalf("replay duplicated state: registry=%d token=%d ledger=%d", registryRows, tokenRows, ledgerRows)
	}
	var cached string
	if err := f.store.DB().QueryRow(`SELECT response_json FROM operator_idempotency WHERE idempotency_key=?`, key).Scan(&cached); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(cached, token) || strings.Contains(cached, "enrollment_token") {
		t.Fatalf("idempotency receipt retained secret: %s", cached)
	}
	entries, err := f.store.Audit(machineID, 10)
	if err != nil || len(entries) != 2 || !entries[0].IsOperatorReplay() || entries[0].SourceKind != operator.SourceKindOperatorAPI {
		t.Fatalf("HTTP enrollment audit=%+v err=%v", entries, err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Detail, token) || strings.Contains(entry.Reason, token) {
			t.Fatalf("audit retained enrollment token: %+v", entry)
		}
	}
}

func TestEnrollTokenCLIPreviewDoesNotCreateState(t *testing.T) {
	f := observedOperatorFixture(t)
	server := httptest.NewServer(f.mux)
	defer server.Close()
	base, deps := machineHTTPTestDeps(t, server)
	var beforeMachines, beforeLedger, beforeAudit int
	for query, target := range map[string]*int{
		`SELECT COUNT(*) FROM machine_registry`:     &beforeMachines,
		`SELECT COUNT(*) FROM operator_idempotency`: &beforeLedger,
		`SELECT COUNT(*) FROM audit_log`:            &beforeAudit,
	} {
		if err := f.store.DB().QueryRow(query).Scan(target); err != nil {
			t.Fatal(err)
		}
	}

	var out, errOut bytes.Buffer
	if err := runEnrollTokenCommandWithDeps(t.Context(), []string{
		"--hub-url", base, "--preview", "--ttl", "1h", "preview-machine",
	}, &out, &errOut, deps); err != nil {
		t.Fatalf("preview: %v stderr=%q", err, errOut.String())
	}
	if !strings.Contains(out.String(), "preview-digest=sha256:") || !strings.Contains(out.String(), "enter denominator") {
		t.Fatalf("preview output=%q", out.String())
	}
	var afterMachines, afterLedger, afterAudit int
	for query, target := range map[string]*int{
		`SELECT COUNT(*) FROM machine_registry`:     &afterMachines,
		`SELECT COUNT(*) FROM operator_idempotency`: &afterLedger,
		`SELECT COUNT(*) FROM audit_log`:            &afterAudit,
	} {
		if err := f.store.DB().QueryRow(query).Scan(target); err != nil {
			t.Fatal(err)
		}
	}
	if afterMachines != beforeMachines || afterLedger != beforeLedger || afterAudit != beforeAudit {
		t.Fatalf("preview mutated state: machines %d→%d ledger %d→%d audit %d→%d",
			beforeMachines, afterMachines, beforeLedger, afterLedger, beforeAudit, afterAudit)
	}
}

// TestEnrollTokenCLIPreviewSaysItWillBeRefusedAtTheLimit 釘的是一句話會不會說謊。
//
// ⚠ 到了註冊上限，terminal 上那一行預覽如果還在講「會新增一台 expected machine 並立即
// 進分母」，它描述的就是一個不會發生的結果——下一個動作會被擋下來，而看過那句話的人
// 會把那次拒絕當成偶發失敗去重試。網頁在按鈕旁邊已經先講了，terminal 不能少講。
func TestEnrollTokenCLIPreviewSaysItWillBeRefusedAtTheLimit(t *testing.T) {
	f := observedOperatorFixture(t)
	server := httptest.NewServer(f.mux)
	defer server.Close()
	base, deps := machineHTTPTestDeps(t, server)

	current, err := f.store.EnrollmentLimit()
	if err != nil {
		t.Fatal(err)
	}
	max := strconv.Itoa(current.InDenominator)
	preview := limitPost(t, f, "/v1/operator/enrollment-limit/preview",
		`{"set":true,"max_machines":`+max+`}`, "")
	var previewed operator.EnrollmentLimitPreview
	if err := json.Unmarshal(preview.Body.Bytes(), &previewed); err != nil {
		t.Fatal(err)
	}
	applied := limitPost(t, f, "/v1/operator/enrollment-limit",
		`{"set":true,"max_machines":`+max+`,"expected_revision":0,"preview_digest":"`+
			previewed.PreviewDigest+`","reason":"cli preview copy"}`, "cli-preview-at-limit")
	if applied.Code != http.StatusOK {
		t.Fatalf("apply status=%d body=%s", applied.Code, applied.Body.String())
	}

	var out, errOut bytes.Buffer
	if err := runEnrollTokenCommandWithDeps(t.Context(), []string{
		"--hub-url", base, "--preview", "--ttl", "1h", "refused-machine",
	}, &out, &errOut, deps); err != nil {
		t.Fatalf("preview: %v stderr=%q", err, errOut.String())
	}
	line := out.String()
	if strings.Contains(line, "will add one") || strings.Contains(line, "enter denominator") {
		t.Fatalf("到上限的預覽仍在承諾它做不到的事：%q", line)
	}
	for _, want := range []string{
		"cannot be created now", "limit " + max + " machines", "retire unused machines", "preview-digest=sha256:",
	} {
		if !strings.Contains(line, want) {
			t.Fatalf("預覽少了 %q：%q", want, line)
		}
	}
}

func TestEnrollTokenCLIAllowsRevokeAsDisplayNameAfterFlagTerminator(t *testing.T) {
	f := observedOperatorFixture(t)
	server := httptest.NewServer(f.mux)
	defer server.Close()
	base, deps := machineHTTPTestDeps(t, server)
	var out, errOut bytes.Buffer
	if err := runEnrollTokenCommandWithDeps(t.Context(), []string{
		"--preview", "--hub-url", base, "--", "revoke",
	}, &out, &errOut, deps); err != nil {
		t.Fatalf("preview display name revoke: %v; stderr=%q", err, errOut.String())
	}
	if !strings.Contains(out.String(), "preview: revoke will add one expected machine") || errOut.Len() != 0 {
		t.Fatalf("flag terminator did not preserve display name revoke: stdout=%q stderr=%q", out.String(), errOut.String())
	}
	var machines int
	if err := f.store.DB().QueryRow(`SELECT COUNT(*) FROM machine_registry WHERE display_name='revoke'`).Scan(&machines); err != nil || machines != 0 {
		t.Fatalf("preview created revoke machine: rows=%d err=%v", machines, err)
	}
}

func TestEnrollTokenCLIDiscoveryFailureNeverFallsBackToDB(t *testing.T) {
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
	err := runEnrollTokenCommandWithDeps(t.Context(), []string{"new-machine"}, &out, &errOut, deps)
	if err == nil || !strings.Contains(err.Error(), "discovery unavailable") {
		t.Fatalf("discovery error=%v", err)
	}
	if _, err := os.Stat(missingDB); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("discovery failure created fallback DB: %v", err)
	}
}

func TestEnrollTokenCLIRetryInputsMustBePaired(t *testing.T) {
	deps := machineCommandDeps{
		discoverHubURL: func() (string, error) {
			t.Fatal("unpaired retry input reached discovery")
			return "", nil
		},
	}
	for _, args := range [][]string{
		{"--idempotency-key", "retry-key", "new-machine"},
		{"--preview-digest", "sha256:" + strings.Repeat("a", 64), "new-machine"},
	} {
		var out, errOut bytes.Buffer
		err := runEnrollTokenCommandWithDeps(t.Context(), args, &out, &errOut, deps)
		if err == nil || !strings.Contains(err.Error(), "must be provided together") || out.Len() != 0 {
			t.Fatalf("args=%v error=%v stdout=%q", args, err, out.String())
		}
	}
}

func TestEnrollTokenCLIExplicitDBUsesSharedStoppedServiceFence(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(dir, "clawctl.sqlite")
	st, err := store.Open(dbPath)
	if err != nil {
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
	deps.verifyHubStopped = func(context.Context, string) error { return nil }
	previewStore, err := openExisting(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	preview, err := previewStore.PreviewOperatorEnrollToken("direct-new-machine", int64(time.Hour/time.Second))
	if closeErr := previewStore.Close(); err == nil && closeErr != nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	args := []string{
		"--db", dbPath, "--ttl", "1h", "--reason", "break-glass test",
		"--idempotency-key", "direct-enrollment", "--preview-digest", preview.PreviewDigest,
		"direct-new-machine",
	}
	if err := runEnrollTokenCommandWithDeps(t.Context(), args, &out, &errOut, deps); err != nil {
		t.Fatalf("direct enrollment: %v stderr=%q", err, errOut.String())
	}
	token := strings.TrimSpace(out.String())
	if len(token) != 43 || strings.Contains(errOut.String(), token) {
		t.Fatalf("direct secret delivery token_len=%d stderr=%q", len(token), errOut.String())
	}

	st, err = openExisting(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var machineID string
	if err := st.DB().QueryRow(`SELECT machine_id FROM machine_registry WHERE display_name='direct-new-machine'`).Scan(&machineID); err != nil {
		t.Fatal(err)
	}
	entries, err := st.Audit(machineID, 10)
	if err != nil || len(entries) != 1 || entries[0].SourceKind != operator.SourceKindDirectDBCLI ||
		entries[0].WhoUnavailable == "" {
		t.Fatalf("direct enrollment audit=%+v err=%v", entries, err)
	}
}

func TestEnrollTokenCLIExplicitDBContentionRefusesBeforeSQLiteOpen(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(dir, "clawctl.sqlite")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	writer, err := ledgerlock.AcquireWriter(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()

	deps := productionMachineCommandDeps()
	deps.openDirectDB = func(string) (*store.Store, error) {
		t.Fatal("writer contention reached Store.Open")
		return nil, nil
	}
	var out, errOut bytes.Buffer
	err = runEnrollTokenCommandWithDeps(t.Context(), []string{
		"--db", dbPath, "--ttl", "1h", "blocked-machine",
	}, &out, &errOut, deps)
	if !errors.Is(err, ledgerlock.ErrContended) || out.Len() != 0 {
		t.Fatalf("writer contention error=%v stdout=%q", err, out.String())
	}
}
