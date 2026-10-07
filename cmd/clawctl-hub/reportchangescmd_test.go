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
	"sync/atomic"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/operatorclient"
	"github.com/teddashh/AI-Intune/internal/state"
	"github.com/teddashh/AI-Intune/internal/store"
)

// TestChangeValueSummaryPrintsUnknownForUnmeasuredSystemd 釘住 CLI 與 machines evidence 使用相同的 unknown 詞彙。
func TestChangeValueSummaryPrintsUnknownForUnmeasuredSystemd(t *testing.T) {
	measured := false
	if got := changeValueSummary(operator.ChangeKindSystemd, &operator.ChangeValue{Measured: &measured}); got != "present=unknown" {
		t.Fatalf("未量到的 systemd 摘要=%q，預期 present=unknown", got)
	}
}

func TestReportChangesCLIHTTPAcceptsSparseFirstObservationFromRealRoute(t *testing.T) {
	f := observedOperatorFixture(t)
	offset := time.FixedZone("EDT", -4*60*60)
	fromInput := jobsTestNow.Add(-time.Hour).In(offset).Format(time.RFC3339)
	toInput := jobsTestNow.Add(time.Hour).In(offset).Format(time.RFC3339)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		query := r.URL.Query()
		if r.Method != http.MethodGet || r.URL.Path != "/v1/operator/changes" ||
			query.Get("machine_id") != f.machine.id || strings.Join(query["kind"], ",") != operator.ChangeKindOpenClaw ||
			query.Get("subject") != "openclaw" ||
			query.Get("from") != jobsTestNow.Add(-time.Hour).Format(time.RFC3339) ||
			query.Get("to") != jobsTestNow.Add(time.Hour).Format(time.RFC3339) || query.Get("limit") != "10" {
			t.Errorf("request=%s %s query=%v", r.Method, r.URL.Path, query)
		}
		f.mux.ServeHTTP(w, r)
	}))
	defer server.Close()
	base, deps := machineHTTPTestDeps(t, server)
	deps.discoverHubURL = func() (string, error) {
		t.Fatal("explicit --hub-url invoked discovery")
		return "", nil
	}
	deps.verifyHubStopped = func(context.Context, string) error {
		t.Fatal("HTTP changes read entered direct DB fence")
		return nil
	}
	missingDB := filepath.Join(t.TempDir(), "must-not-be-created.sqlite")
	t.Setenv("CLAWCTL_DB", missingDB)

	var out, errOut bytes.Buffer
	err := runReportChangesCommandWithDeps(t.Context(), []string{
		"--hub-url", base, "--machine", f.machine.id, "--kind", operator.ChangeKindOpenClaw,
		"--subject", "openclaw",
		"--from", fromInput, "--to", toInput, "--limit", "10", "--json",
	}, &out, &errOut, deps)
	if err != nil {
		t.Fatalf("HTTP report changes: %v; stderr=%s", err, errOut.String())
	}
	var result operator.ChangeListResult
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatalf("decode output: %v; output=%s", err, out.String())
	}
	if result.MatchedTotal != 1 || len(result.Items) != 1 || result.Items[0].Before != nil ||
		result.Items[0].After == nil || result.Items[0].After.Present == nil || !*result.Items[0].After.Present ||
		result.Items[0].After.CLIVersion != nil ||
		strings.Join(result.Items[0].ChangedFields, ",") != "present" ||
		strings.Join(result.Items[0].RedactedFields, ",") != "absence_reason,installation_details" {
		t.Fatalf("sparse first observation=%+v", result)
	}
	if calls.Load() != 1 {
		t.Fatalf("HTTP calls=%d, want 1", calls.Load())
	}
	if _, err := os.Stat(missingDB); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("HTTP mode touched direct DB: %v", err)
	}
}

func TestReportChangesCLIValidatesEveryInputBeforeTransportSelection(t *testing.T) {
	from := "2026-09-08T12:00:00Z"
	to := "2026-09-08T13:00:00Z"
	tests := []struct {
		name string
		args []string
	}{
		{name: "positional", args: []string{"unexpected"}},
		{name: "empty scalar", args: []string{"--machine", ""}},
		{name: "duplicate scalar", args: []string{"--machine", "one", "--machine", "two"}},
		{name: "mixed transport", args: []string{"--hub-url", "http://100.64.0.9:8787", "--db", "/tmp/x"}},
		{name: "unknown kind", args: []string{"--kind", "unknown"}},
		{name: "duplicate kind", args: []string{"--kind", "state", "--kind", "state"}},
		{name: "bad time", args: []string{"--from", "2026-09-08"}},
		{name: "fractional time", args: []string{"--from", "2026-09-08T12:00:00.001Z"}},
		{name: "noncanonical utc offset", args: []string{"--from", "2026-09-08T12:00:00+00:00"}},
		{name: "reversed window", args: []string{"--from", to, "--to", from}},
		{name: "oversized window", args: []string{"--from", "2026-08-01T00:00:00Z", "--to", to}},
		{name: "zero limit", args: []string{"--limit", "0"}},
		{name: "large limit", args: []string{"--limit", "101"}},
		{name: "signed limit", args: []string{"--limit", "+1"}},
		{name: "leading zero limit", args: []string{"--limit", "01"}},
		{name: "bad cursor", args: []string{"--cursor", "not-a-cursor"}},
		{name: "private subject", args: []string{"--subject", "alice@example.com"}},
		{name: "control text", args: []string{"--subject", "bad\nsubject"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			deps := machineCommandDeps{
				discoverHubURL: func() (string, error) {
					t.Fatal("invalid CLI reached discovery")
					return "", nil
				},
				newOperatorClient: func(string) (*operatorclient.Client, error) {
					t.Fatal("invalid CLI constructed an HTTP transport")
					return nil, nil
				},
				validateDirectPath: func(string) error {
					t.Fatal("invalid CLI inspected a DB path")
					return nil
				},
			}
			var out, errOut bytes.Buffer
			if err := runReportChangesCommandWithDeps(t.Context(), test.args, &out, &errOut, deps); err == nil {
				t.Fatalf("accepted args=%q output=%q stderr=%q", test.args, out.String(), errOut.String())
			}
		})
	}
}

func TestReportChangesCLIDiscoveryFailureNeverFallsBackToDB(t *testing.T) {
	missingDB := filepath.Join(t.TempDir(), "must-not-be-created.sqlite")
	t.Setenv("CLAWCTL_DB", missingDB)
	deps := machineCommandDeps{
		discoverHubURL: func() (string, error) { return "", errors.New("discovery unavailable") },
		validateDirectPath: func(string) error {
			t.Fatal("discovery failure inspected direct DB")
			return nil
		},
		verifyHubStopped: func(context.Context, string) error {
			t.Fatal("discovery failure entered direct DB fence")
			return nil
		},
	}
	var out, errOut bytes.Buffer
	err := runReportChangesCommandWithDeps(t.Context(), nil, &out, &errOut, deps)
	if err == nil || !strings.Contains(err.Error(), "discovery unavailable") {
		t.Fatalf("discovery error=%v", err)
	}
	if _, err := os.Stat(missingDB); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("discovery failure created fallback DB: %v", err)
	}
}

func TestReportChangesCLIDiscoversHTTPByDefaultWithoutTouchingDB(t *testing.T) {
	f := observedOperatorFixture(t)
	server := httptest.NewServer(f.mux)
	defer server.Close()
	base, deps := machineHTTPTestDeps(t, server)
	var discoveries atomic.Int32
	deps.discoverHubURL = func() (string, error) {
		discoveries.Add(1)
		return base, nil
	}
	deps.verifyHubStopped = func(context.Context, string) error {
		t.Fatal("default HTTP mode entered direct DB fence")
		return nil
	}
	missingDB := filepath.Join(t.TempDir(), "must-not-be-created.sqlite")
	t.Setenv("CLAWCTL_DB", missingDB)
	var out, errOut bytes.Buffer
	if err := runReportChangesCommandWithDeps(t.Context(), []string{
		"--machine", f.machine.id, "--from", jobsTestNow.Add(-time.Hour).Format(time.RFC3339),
		"--to", jobsTestNow.Add(time.Hour).Format(time.RFC3339), "--json",
	}, &out, &errOut, deps); err != nil {
		t.Fatalf("discovered HTTP report: %v; stderr=%s", err, errOut.String())
	}
	if discoveries.Load() != 1 || !strings.Contains(out.String(), `"schema_version": 2`) {
		t.Fatalf("discoveries=%d output=%s", discoveries.Load(), out.String())
	}
	if _, err := os.Stat(missingDB); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("default HTTP mode touched direct DB: %v", err)
	}
}

func TestReportChangesCLIDirectPagesWithExactMachineIDAndStoppedFence(t *testing.T) {
	dbPath, machineID := directDBFixture(t)
	st, err := openExisting(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC().Truncate(time.Second).Add(-10 * time.Minute)
	if err := st.RecordStateTransition(machineID, state.Online, "baseline", base); err != nil {
		_ = st.Close()
		t.Fatal(err)
	}
	if err := st.RecordStateTransition(machineID, state.Degraded, "changed", base.Add(time.Minute)); err != nil {
		_ = st.Close()
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
	var stoppedChecks atomic.Int32
	deps.verifyHubStopped = func(_ context.Context, got string) error {
		if got != dbPath {
			t.Fatalf("stopped proof path=%q want=%q", got, dbPath)
		}
		stoppedChecks.Add(1)
		return nil
	}
	run := func(cursor string) operator.ChangeListResult {
		t.Helper()
		args := []string{
			"--db", dbPath, "--machine", machineID, "--kind", operator.ChangeKindState,
			"--limit", "1", "--json",
		}
		if cursor != "" {
			args = append(args, "--cursor", cursor)
		}
		var out, errOut bytes.Buffer
		if err := runReportChangesCommandWithDeps(t.Context(), args, &out, &errOut, deps); err != nil {
			t.Fatalf("direct report changes: %v; stderr=%s", err, errOut.String())
		}
		var result operator.ChangeListResult
		if err := json.Unmarshal(out.Bytes(), &result); err != nil {
			t.Fatalf("decode direct result: %v; output=%s", err, out.String())
		}
		return result
	}
	first := run("")
	if first.MatchedTotal != 2 || len(first.Items) != 1 || first.NextCursor == nil {
		t.Fatalf("first direct page=%+v", first)
	}
	second := run(*first.NextCursor)
	if len(second.Items) != 1 || second.NextCursor != nil ||
		second.Items[0].ChangeID == first.Items[0].ChangeID ||
		!second.EvaluatedAt.Equal(first.EvaluatedAt) || second.CreationCeilings != first.CreationCeilings {
		t.Fatalf("second direct page=%+v first=%+v", second, first)
	}
	if stoppedChecks.Load() != 2 {
		t.Fatalf("stopped-service checks=%d, want 2", stoppedChecks.Load())
	}
}

func TestReportChangesCLIExplicitDBRejectsRunningHubBeforeOpen(t *testing.T) {
	dbPath, _ := directDBFixture(t)
	deps := productionMachineCommandDeps()
	deps.verifyHubStopped = func(context.Context, string) error { return errors.New("Hub active/running") }
	deps.openDirectDB = func(string) (*store.Store, error) {
		t.Fatal("running Hub reached Store.Open")
		return nil, nil
	}
	var out, errOut bytes.Buffer
	err := runReportChangesCommandWithDeps(t.Context(), []string{"--db", dbPath}, &out, &errOut, deps)
	if err == nil || !strings.Contains(err.Error(), "active/running") {
		t.Fatalf("running Hub error=%v", err)
	}
}

func TestReportChangesHumanOutputExplainsSemanticsAndKeepsCursorRaw(t *testing.T) {
	f := observedOperatorFixture(t)
	if err := f.store.RecordStateTransition(f.machine.id, state.Online, "baseline", jobsTestNow.Add(-10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := f.store.RecordStateTransition(f.machine.id, state.Degraded, "changed", jobsTestNow.Add(-9*time.Minute)); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(f.mux)
	defer server.Close()
	hubURL, deps := machineHTTPTestDeps(t, server)
	deps.discoverHubURL = func() (string, error) {
		t.Fatal("explicit --hub-url invoked discovery")
		return "", nil
	}
	deps.verifyHubStopped = func(context.Context, string) error {
		t.Fatal("HTTP cursor traversal entered direct DB fence")
		return nil
	}
	baseArgs := []string{
		"--hub-url", hubURL, "--machine", f.machine.id, "--kind", "state", "--limit", "1",
		"--from", jobsTestNow.Add(-time.Hour).Format(time.RFC3339),
		"--to", jobsTestNow.Add(time.Hour).Format(time.RFC3339),
	}
	run := func(args []string) string {
		t.Helper()
		var out, errOut bytes.Buffer
		if err := runReportChangesCommandWithDeps(t.Context(), args, &out, &errOut, deps); err != nil {
			t.Fatalf("human HTTP report: %v; stderr=%s", err, errOut.String())
		}
		return out.String()
	}
	text := run(baseArgs)
	for _, wanted := range []string{
		"fixed_window_rowid_ceilings_with_retention_mutability", "(from,to]", "endpoint_delta",
		"Coverage: ", "kind_counts apply the same filter", "transition", "window_comparison",
		"compares observation window endpoints only", "next cursor: ",
	} {
		if !strings.Contains(text, wanted) {
			t.Errorf("human output misses %q: %s", wanted, text)
		}
	}
	line := ""
	for _, candidate := range strings.Split(text, "\n") {
		if strings.HasPrefix(candidate, "next cursor: ") {
			line = candidate
		}
	}
	cursor := strings.TrimPrefix(line, "next cursor: ")
	if cursor == "" || strings.ContainsAny(cursor, " \t\"") || line != "next cursor: "+cursor {
		t.Fatalf("cursor was not raw-copyable: %q", line)
	}
	second := run([]string{
		"--hub-url", hubURL, "--machine", f.machine.id, "--kind", "state", "--limit", "1",
		"--cursor", cursor,
	})
	if strings.Contains(second, cursor) || !strings.Contains(second, "transition") {
		t.Fatalf("raw cursor did not advance HTTP traversal: %s", second)
	}
}

func TestReportChangesMaintenanceExceptionDoesNotChangeBareReport(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "local.sqlite")
	t.Setenv("CLAWCTL_DB", dbPath)
	if err := os.WriteFile(upgradeMaintenanceMarker(dbPath), []byte(upgradeMaintenanceContents), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := rejectTopLevelCLIWhileUpgradeMaintenance("report", []string{
		"report", "changes", "--hub-url", "http://100.64.0.9:8787",
	}); err != nil {
		t.Fatalf("remote report changes was coupled to local marker: %v", err)
	}
	for _, argv := range [][]string{{"report"}, {"report", "--json", "changes"}} {
		if err := rejectTopLevelCLIWhileUpgradeMaintenance("report", argv); err == nil {
			t.Fatalf("bare report semantics bypassed maintenance marker argv=%q", argv)
		}
	}
	if err := rejectDBWhileUpgradeMaintenance(dbPath); err == nil {
		t.Fatal("direct report changes exact target bypassed maintenance marker")
	}
}
