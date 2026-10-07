package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/operatorclient"
	"github.com/teddashh/AI-Intune/internal/store"
)

func TestAuditCLIUsesHTTPFirstWithCanonicalFiltersAndNoMutation(t *testing.T) {
	f := observedOperatorFixture(t)
	at := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	if err := f.store.RecordAudit(store.AuditEntry{
		At: at, Action: store.AuditMachineChannel, MachineID: f.machine.id,
		Subject: "cnode-operator", Reason: "planned", IdempotencyKey: "audit-cli-key",
		RequestDigest: "sha256:audit-cli", SourceAddr: "100.64.0.7", WhoUser: "owner@example.com",
		AuthSubject: "tailscale-user:42", AuthCapability: "example.com/cap/clawctl-view",
		SourceKind: "operator-api", OK: false,
	}); err != nil {
		t.Fatal(err)
	}
	before := auditAPIRowCount(t, f.store)
	type observedRequest struct {
		method, path, userAgent string
		query                   url.Values
	}
	var mu sync.Mutex
	var calls []observedRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls = append(calls, observedRequest{r.Method, r.URL.Path, r.UserAgent(), r.URL.Query()})
		mu.Unlock()
		f.mux.ServeHTTP(w, r)
	}))
	defer server.Close()
	base, deps := machineHTTPTestDeps(t, server)
	deps.discoverHubURL = func() (string, error) { return base, nil }
	deps.validateDirectPath = func(string) error {
		t.Fatal("normal audit read inspected a direct DB")
		return nil
	}
	deps.verifyHubStopped = func(context.Context, string) error {
		t.Fatal("normal audit read checked the local Hub unit")
		return nil
	}
	missingDB := filepath.Join(t.TempDir(), "must-not-exist.sqlite")
	t.Setenv("CLAWCTL_DB", missingDB)

	var out, errOut bytes.Buffer
	err := runAuditCommandWithDeps(t.Context(), []string{
		"list", "--json", "--machine", f.machine.id, "--action", "machine-channel",
		"--outcome", "failed", "--principal", "tailscale-user:42",
		"--capability", "example.com/cap/clawctl-view", "--source-kind", "operator-api",
		"--correlation", "audit-cli-key", "--from", "2026-09-08T08:00:00-04:00",
		"--to", "2026-09-08T12:00:00Z", "--denials", "all", "--limit", "1",
	}, &out, &errOut, deps)
	if err != nil {
		t.Fatalf("HTTP audit list: %v; stderr=%s", err, errOut.String())
	}
	var result operator.AuditListResult
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatalf("decode audit CLI JSON: %v; body=%s", err, out.String())
	}
	if result.SchemaVersion != operator.AuditReadSchemaVersion || result.Consistency != operator.AuditReadConsistency ||
		result.Total != 1 || len(result.Items) != 1 || result.Items[0].AuditID <= 0 ||
		result.Items[0].Outcome == nil || *result.Items[0].Outcome != store.AuditOutcomeFailed {
		t.Fatalf("audit CLI envelope=%+v", result)
	}
	if after := auditAPIRowCount(t, f.store); after != before {
		t.Fatalf("audit CLI GET mutated ledger: %d -> %d", before, after)
	}
	if _, err := os.Stat(missingDB); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("HTTP audit read touched default DB: %v", err)
	}

	mu.Lock()
	gotCalls := append([]observedRequest(nil), calls...)
	mu.Unlock()
	if len(gotCalls) != 1 || gotCalls[0].method != http.MethodGet ||
		gotCalls[0].path != "/v1/operator/audit-events" || gotCalls[0].userAgent != operatorclient.UserAgent {
		t.Fatalf("audit HTTP calls=%+v", gotCalls)
	}
	query := gotCalls[0].query
	if query.Get("machine_id") != f.machine.id || query.Get("outcome") != "failed" ||
		query.Get("principal") != "tailscale-user:42" || query.Get("capability") != "example.com/cap/clawctl-view" ||
		query.Get("source_kind") != "operator-api" || query.Get("correlation") != "audit-cli-key" ||
		query.Get("from") != "2026-09-08T12:00:00Z" || query.Get("to") != "2026-09-08T12:00:00Z" ||
		query.Get("denials") != "all" || query.Get("limit") != "1" ||
		len(query["action"]) != 1 || query["action"][0] != "machine-channel" {
		t.Fatalf("audit query=%v", query)
	}
}

func TestAuditCLIExplicitDBUsesFencedOperatorService(t *testing.T) {
	dbPath, machineID := directDBFixture(t)
	st, err := openExisting(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.RecordAudit(store.AuditEntry{
		Action: store.AuditRetire, MachineID: machineID, Subject: "direct-machine",
		SourceAddr: "local-test", OK: true,
	}); err != nil {
		_ = st.Close()
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	deps := productionMachineCommandDeps()
	deps.discoverHubURL = func() (string, error) {
		t.Fatal("explicit --db invoked Hub discovery")
		return "", nil
	}
	deps.newOperatorClient = func(string) (*operatorclient.Client, error) {
		t.Fatal("explicit --db constructed HTTP client")
		return nil, nil
	}
	stoppedChecks := 0
	deps.verifyHubStopped = func(_ context.Context, gotPath string) error {
		stoppedChecks++
		if gotPath != dbPath {
			t.Fatalf("stopped-service proof path=%q want=%q", gotPath, dbPath)
		}
		return nil
	}
	var out, errOut bytes.Buffer
	if err := runAuditCommandWithDeps(t.Context(), []string{
		"--db", dbPath, "--machine", "direct-machine", "--action", "retire", "--denials", "all", "--json",
	}, &out, &errOut, deps); err != nil {
		t.Fatalf("direct audit list: %v; stderr=%s", err, errOut.String())
	}
	var result operator.AuditListResult
	if err := json.Unmarshal(out.Bytes(), &result); err != nil || result.Total != 1 ||
		len(result.Items) != 1 || result.Items[0].MachineID == nil || *result.Items[0].MachineID != machineID {
		t.Fatalf("direct audit result=%+v err=%v body=%s", result, err, out.String())
	}
	if stoppedChecks != 1 {
		t.Fatalf("stopped-service checks=%d want=1", stoppedChecks)
	}
}

func TestAuditCLINeverImplicitlyFallsBackToDB(t *testing.T) {
	missingDB := filepath.Join(t.TempDir(), "must-not-exist.sqlite")
	t.Setenv("CLAWCTL_DB", missingDB)
	deps := machineCommandDeps{
		discoverHubURL: func() (string, error) { return "", errors.New("discovery unavailable") },
		validateDirectPath: func(string) error {
			t.Fatal("discovery failure inspected direct DB")
			return nil
		},
		verifyHubStopped: func(context.Context, string) error {
			t.Fatal("discovery failure checked local Hub")
			return nil
		},
	}
	var out, errOut bytes.Buffer
	err := runAuditCommandWithDeps(t.Context(), nil, &out, &errOut, deps)
	if err == nil || !strings.Contains(err.Error(), "discovery unavailable") ||
		out.Len() != 0 {
		t.Fatalf("error=%v stdout=%q stderr=%q", err, out.String(), errOut.String())
	}
	if _, err := os.Stat(missingDB); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed discovery created fallback DB: %v", err)
	}
}

func TestAuditCLIRejectsInvalidArgumentsBeforeIO(t *testing.T) {
	var calls atomic.Int32
	deps := machineCommandDeps{
		discoverHubURL: func() (string, error) {
			calls.Add(1)
			return "http://100.64.0.9:8787", nil
		},
		newOperatorClient: func(string) (*operatorclient.Client, error) {
			calls.Add(1)
			return nil, errors.New("must not construct client")
		},
		validateDirectPath: func(string) error {
			calls.Add(1)
			return errors.New("must not inspect DB")
		},
	}
	for _, test := range []struct {
		name string
		args []string
	}{
		{"positional", []string{"show"}},
		{"unknown action", []string{"--action", "future-action"}},
		{"duplicate action", []string{"--action", "retire", "--action", "retire"}},
		{"bad outcome", []string{"--outcome", "maybe"}},
		{"bad denial mode", []string{"--denials", "none"}},
		{"zero limit", []string{"--limit", "0"}},
		{"oversized limit", []string{"--limit", "101"}},
		{"empty machine", []string{"--machine="}},
		{"padded principal", []string{"--principal", " user"}},
		{"control capability", []string{"--capability", "cap\nability"}},
		{"bad from", []string{"--from", "2026-09-08"}},
		{"fractional from", []string{"--from", "2026-09-08T12:00:00.1Z"}},
		{"reverse window", []string{"--from", "2026-09-09T00:00:00Z", "--to", "2026-09-08T00:00:00Z"}},
		{"oversized cursor", []string{"--cursor", strings.Repeat("a", 2049)}},
		{"mixed transport", []string{"--hub-url", "http://100.64.0.9:8787", "--db", "/tmp/not-used"}},
		{"duplicate scalar", []string{"--outcome", "ok", "--outcome", "failed"}},
		{"duplicate boolean", []string{"--json", "--json"}},
		{"unknown flag", []string{"--raw"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			if err := runAuditCommandWithDeps(t.Context(), test.args, &out, &errOut, deps); err == nil {
				t.Fatalf("invalid args %q were accepted", test.args)
			}
			if out.Len() != 0 {
				t.Fatalf("invalid args wrote stdout=%q", out.String())
			}
		})
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("invalid audit arguments reached I/O %d times", got)
	}
	var out, help bytes.Buffer
	if err := runAuditCommandWithDeps(t.Context(), []string{"--help"}, &out, &help, deps); !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("help error=%v", err)
	}
	for _, want := range []string{"audit [list]", "--hub-url", "--db", "--denials", "latest 50"} {
		if !strings.Contains(help.String(), want) {
			t.Fatalf("audit help missing %q: %s", want, help.String())
		}
	}
	if calls.Load() != 0 || out.Len() != 0 {
		t.Fatalf("help reached I/O=%d stdout=%q", calls.Load(), out.String())
	}
}

func TestAuditCLIHumanOutputIsTerminalSafeAndExplicitAboutUnknownEvidence(t *testing.T) {
	at := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	unsafe := "value\r\n\x1b]2;owned\a"
	result := operator.AuditListResult{
		SchemaVersion: operator.AuditReadSchemaVersion, Consistency: operator.AuditReadConsistency,
		EvaluatedAt: at, MatchedTotal: 2, Total: 1, UnknownOutcome: 1,
		Denials: operator.AuditDenials{Mode: operator.AuditDenialsSampled, Matched: 1, Omitted: 1},
		Items: []operator.AuditEvent{{AuditID: 7, Action: unsafe, Subject: unsafe, SourceAddr: unsafe,
			Issues: []string{"invalid_at", "unknown_action", "unknown_outcome"}, AlteredFields: []string{}}},
	}
	var out bytes.Buffer
	if err := writeAuditList(&out, result, false, "HTTP operator API"); err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(out.String(), "\x1b\r\a") || !strings.Contains(out.String(), `\x1b`) ||
		!strings.Contains(out.String(), `\r\n`) || !strings.Contains(out.String(), "unknown (invalid ledger time)") ||
		!strings.Contains(out.String(), "unknown outcome 1") || !strings.Contains(out.String(), "omitted 1") ||
		!strings.Contains(out.String(), operator.AuditReadConsistency) {
		t.Fatalf("unsafe or ambiguous audit output=%q", out.String())
	}
}

func TestAuditCLIMaintenanceRoutingKeepsHTTPRemoteAndDirectFenced(t *testing.T) {
	dbPath, _ := directDBFixture(t)
	t.Setenv("CLAWCTL_DB", dbPath)
	if err := os.WriteFile(upgradeMaintenanceMarker(dbPath), []byte(upgradeMaintenanceContents), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, argv := range [][]string{
		{"audit", "--hub-url", "http://100.64.0.9:8787"},
		{"audit", "list", "--db", dbPath},
	} {
		if err := rejectTopLevelCLIWhileUpgradeMaintenance("audit", argv); err != nil {
			t.Fatalf("audit read transport was coupled to default marker argv=%q: %v", argv, err)
		}
	}
	deps := productionMachineCommandDeps()
	deps.verifyHubStopped = func(context.Context, string) error {
		t.Fatal("exact-target maintenance refusal reached stopped proof")
		return nil
	}
	var out, errOut bytes.Buffer
	err := runAuditCommandWithDeps(t.Context(), []string{"--db", dbPath}, &out, &errOut, deps)
	if err == nil || !strings.Contains(err.Error(), "direct DB") || out.Len() != 0 {
		t.Fatalf("direct audit read bypassed exact marker: err=%v stdout=%q stderr=%q", err, out.String(), errOut.String())
	}
}

func TestTheAuditCLINamesTheStateWhenARowHasNoReadableSubject(t *testing.T) {
	const readableSubject = "verifier-under-audit"
	result := operator.AuditListResult{
		SchemaVersion: operator.AuditReadSchemaVersion,
		Consistency:   operator.AuditReadConsistency,
		EvaluatedAt:   time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC),
		Items: []operator.AuditEvent{
			{AuditID: 1, Action: string(store.AuditVerifierRegister), Subject: ""},
			{AuditID: 2, Action: string(store.AuditVerifierRevoke), Subject: readableSubject},
		},
	}
	var out bytes.Buffer
	if err := writeAuditList(&out, result, false, "HTTP operator API"); err != nil {
		t.Fatal(err)
	}
	if got, want := out.String(), "undecodable subject"; !strings.Contains(got, want) {
		t.Errorf("got audit output %q, expected it to contain %q；審計表把沒有對象的那一列印成一個空欄，讀表的人分不出「這列沒有對象」與「這格漏印了」，他會去查一個其實不存在的對象。", got, want)
	}
	if got := out.String(); !strings.Contains(got, readableSubject) {
		t.Errorf("got audit output %q, expected it to contain %q；把有對象的列也蓋成「對象無法判讀」，稽核者會失去他本來讀得到的對象名。", got, readableSubject)
	}
}
