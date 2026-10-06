package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/model"
	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/operatorclient"
	"github.com/teddashh/AI-Intune/internal/state"
	"github.com/teddashh/AI-Intune/internal/store"
)

func TestMachinesCLIUsesDiscoveredOperatorAPIAndNeverTouchesDB(t *testing.T) {
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
	deps.discoverHubURL = func() (string, error) { return base, nil }
	deps.verifyHubStopped = func(context.Context, string) error {
		t.Fatal("normal machine read checked the local Hub unit")
		return nil
	}
	missingDB := t.TempDir() + "/must-not-exist.sqlite"
	t.Setenv("CLAWCTL_DB", missingDB)

	var out, errOut bytes.Buffer
	if err := runMachinesCommandWithDeps(t.Context(), nil, &out, &errOut, deps); err != nil {
		t.Fatalf("machines list: %v; stderr=%s", err, errOut.String())
	}
	if !strings.Contains(out.String(), "HTTP operator API") ||
		!strings.Contains(out.String(), f.machine.id) || !strings.Contains(out.String(), "分母") {
		t.Fatalf("list output=%q", out.String())
	}
	if _, err := os.Stat(missingDB); !os.IsNotExist(err) {
		t.Fatalf("HTTP list touched DB path: %v", err)
	}

	var detailOut bytes.Buffer
	if err := runMachinesCommandWithDeps(t.Context(), []string{
		"show", "--hub-url", base, "--json", f.machine.id,
	}, &detailOut, &errOut, deps); err != nil {
		t.Fatalf("machines show: %v; stderr=%s", err, errOut.String())
	}
	if !strings.Contains(detailOut.String(), `"machine_id": "`+f.machine.id+`"`) ||
		!strings.Contains(detailOut.String(), `"hostname"`) ||
		!strings.Contains(detailOut.String(), `"connect_coordinates_excluded": true`) ||
		!strings.Contains(detailOut.String(), `"message"`) ||
		!strings.Contains(detailOut.String(), `"known_secret_fields_excluded": true`) ||
		strings.Contains(detailOut.String(), `"connect"`) {
		t.Fatalf("detail JSON disclosure=%s", detailOut.String())
	}
	mu.Lock()
	gotCalls := append([]string(nil), calls...)
	mu.Unlock()
	wantDetail := "GET /v1/operator/machines/" + f.machine.id
	if len(gotCalls) != 2 || gotCalls[0] != "GET /v1/operator/machines" || gotCalls[1] != wantDetail {
		t.Fatalf("machine read calls=%v", gotCalls)
	}
}

func TestMachinesCLIAcceptsRealCrashLoopSeverityFour(t *testing.T) {
	f := observedOperatorFixture(t)
	now := time.Now().UTC().Truncate(time.Second)
	for i := 0; i < state.CrashLoopRestarts; i++ {
		at := now.Add(time.Duration(i-state.CrashLoopRestarts+1) * time.Minute)
		if err := f.store.RecordCheckin(f.machine.id, model.Checkin{
			SchemaVersion: model.SchemaVersion, SentAt: at,
			AgentVersion: "test", AgentStartedAt: at.Add(-time.Hour), AgentSeq: int64(i + 1),
		}, at); err != nil {
			t.Fatal(err)
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mux.ServeHTTP(w, r)
	}))
	defer server.Close()
	base, deps := machineHTTPTestDeps(t, server)

	var out, errOut bytes.Buffer
	if err := runMachinesCommandWithDeps(t.Context(), []string{
		"show", "--hub-url", base, "--json", f.machine.id,
	}, &out, &errOut, deps); err != nil {
		t.Fatalf("real crash-loop detail was rejected: %v; stderr=%s", err, errOut.String())
	}
	if !strings.Contains(out.String(), `"kind": "agent"`) ||
		!strings.Contains(out.String(), `"severity": 4`) ||
		!strings.Contains(out.String(), `"message"`) || strings.Contains(out.String(), `"facts"`) {
		t.Fatalf("crash-loop safe detail=%s", out.String())
	}
}

// operator API 已帶截斷旗標；這裡防的是 CLI 人類可讀輸出沒有把它說出來。
func TestMachinesCLIDisclosesTruncatedCheckins(t *testing.T) {
	f := observedOperatorFixture(t)
	now := time.Now().UTC().Truncate(time.Second)
	oldest := now.Add(-time.Duration(store.DetailCheckinLimit) * time.Second)
	for i := 0; i <= store.DetailCheckinLimit; i++ {
		at := oldest.Add(time.Duration(i) * time.Second)
		if err := f.store.RecordCheckin(f.machine.id, model.Checkin{
			SchemaVersion: model.SchemaVersion, SentAt: at, AgentVersion: "v3",
			BootID: "boot-one", AgentSeq: int64(i + 1), AgentStartedAt: now.Add(-time.Hour),
			UptimeSeconds: func() *int64 { v := int64(100 + i); return &v }(), DiskFreeBytes: 50, DiskTotalBytes: 100,
		}, at); err != nil {
			t.Fatal(err)
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mux.ServeHTTP(w, r)
	}))
	defer server.Close()
	base, deps := machineHTTPTestDeps(t, server)

	var out, errOut bytes.Buffer
	if err := runMachinesCommandWithDeps(t.Context(), []string{
		"show", "--hub-url", base, f.machine.id,
	}, &out, &errOut, deps); err != nil {
		t.Fatalf("truncated machine detail: %v; stderr=%s", err, errOut.String())
	}
	wantRows := fmt.Sprintf("rows_seen=%d", store.DetailCheckinLimit)
	if !strings.Contains(out.String(), wantRows) || !strings.Contains(out.String(), "truncated=true") {
		t.Fatalf("machine detail checkins output=%q", out.String())
	}
}

func TestMachinesCLIShowsDetailWhenAssignedUserUnavailable(t *testing.T) {
	f := observedOperatorFixture(t)
	const base = "http://100.64.0.9:8787"
	detailPath := "/v1/operator/machines/" + f.machine.id
	assignedPath := detailPath + "/assigned-user"
	// 固定明細回應，讓兩次輸出包含相同的評估時間。
	detail := operatorRequest(t, f.mux, http.MethodGet, detailPath, "", "")
	if detail.Code != http.StatusOK {
		t.Fatalf("讀取測試明細失敗：%d %s", detail.Code, detail.Body.String())
	}
	for _, jsonOutput := range []bool{false, true} {
		t.Run(fmt.Sprintf("JSON=%t", jsonOutput), func(t *testing.T) {
			args := []string{"show", "--hub-url", base}
			if jsonOutput {
				args = append(args, "--json")
			}
			args = append(args, f.machine.id)
			var baseline string
			for _, unavailable := range []bool{false, true} {
				client, calls := operatorClientForMuxWithoutListener(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == detailPath {
						w.Header().Set("Content-Type", detail.Header().Get("Content-Type"))
						w.Header().Set("Cache-Control", detail.Header().Get("Cache-Control"))
						_, _ = w.Write(detail.Body.Bytes())
						return
					}
					if unavailable && r.URL.Path == assignedPath {
						w.Header().Set("Cache-Control", "no-store")
						writeErr(w, http.StatusServiceUnavailable, "INTERNAL", "測試用原始錯誤")
						return
					}
					f.mux.ServeHTTP(w, r)
				}), base)
				deps := productionMachineCommandDeps()
				deps.newOperatorClient = func(string) (*operatorclient.Client, error) { return client, nil }
				var out, errOut bytes.Buffer
				if err := runMachinesCommandWithDeps(t.Context(), args, &out, &errOut, deps); err != nil {
					t.Fatalf("機器明細輸出失敗：%v；錯誤輸出=%s", err, errOut.String())
				}
				wantCalls := int64(2)
				if jsonOutput {
					wantCalls = 1
				}
				if calls.Load() != wantCalls || errOut.Len() != 0 {
					t.Fatalf("要求次數=%d，預期=%d；錯誤輸出=%s", calls.Load(), wantCalls, errOut.String())
				}
				if !unavailable {
					baseline = out.String()
					if !jsonOutput && !strings.Contains(baseline, "指派使用者: 未指派（版本 0）\n") {
						t.Fatalf("成功讀取時缺少指派狀態：%s", baseline)
					}
					continue
				}
				want := baseline
				if !jsonOutput {
					want = strings.Replace(baseline, "指派使用者: 未指派（版本 0）\n", "指派使用者: 無法取得\n", 1)
				}
				if out.String() != want {
					t.Fatalf("指派使用者無法取得時，明細或狀態詞不符：\n實際=%s\n預期=%s", out.String(), want)
				}
			}
		})
	}
}

func TestMachinesCLIForwardsCanonicalListFilters(t *testing.T) {
	f := observedOperatorFixture(t)
	var gotQuery url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		f.mux.ServeHTTP(w, r)
	}))
	defer server.Close()
	base, deps := machineHTTPTestDeps(t, server)

	var out, errOut bytes.Buffer
	err := runMachinesCommandWithDeps(t.Context(), []string{
		"list", "--hub-url", base, "--machine-id", f.machine.id,
		"--display-name", "cnode-operator", "--state", string(state.NeverReported),
		"--lifecycle", "active", "--reporting", "false",
		"--channel", "none", "--limit", "1", "--json",
	}, &out, &errOut, deps)
	if err != nil {
		t.Fatalf("filtered list: %v; stderr=%s", err, errOut.String())
	}
	for key, want := range map[string]string{
		"machine_id": f.machine.id, "display_name": "cnode-operator",
		"state": string(state.NeverReported), "lifecycle": "active",
		"reporting": "false", "channel": "none", "limit": "1",
	} {
		if values := gotQuery[key]; len(values) != 1 || values[0] != want {
			t.Fatalf("query %s=%v want %q; all=%v", key, values, want, gotQuery)
		}
	}
	if gotQuery.Has("expected") {
		t.Fatalf("removed expected filter reached HTTP: %v", gotQuery)
	}
	if !strings.Contains(out.String(), `"matched_total": 1`) ||
		!strings.Contains(out.String(), `"consistency": "`+operator.MachineReadConsistency+`"`) {
		t.Fatalf("filtered JSON=%s", out.String())
	}
}

func TestMachinesCLIDiscoveryFailureNeverFallsBackToDB(t *testing.T) {
	missingDB := t.TempDir() + "/must-not-exist.sqlite"
	t.Setenv("CLAWCTL_DB", missingDB)
	deps := machineCommandDeps{
		discoverHubURL: func() (string, error) { return "", errors.New("discovery unavailable") },
		validateDirectPath: func(string) error {
			t.Fatal("discovery failure inspected a direct DB")
			return nil
		},
	}
	var out, errOut bytes.Buffer
	err := runMachinesCommandWithDeps(t.Context(), nil, &out, &errOut, deps)
	if err == nil || !strings.Contains(err.Error(), "discovery unavailable") ||
		out.Len() != 0 {
		t.Fatalf("discovery error=%v stdout=%q stderr=%q", err, out.String(), errOut.String())
	}
	if _, err := os.Stat(missingDB); !os.IsNotExist(err) {
		t.Fatalf("discovery failure created DB: %v", err)
	}
}

func TestMachinesCLIExplicitDirectDBUsesFencedOperatorService(t *testing.T) {
	dbPath, machineID := directDBFixture(t)
	publishMachineReadTestPolicy(t, dbPath)
	deps := productionMachineCommandDeps()
	deps.discoverHubURL = func() (string, error) {
		t.Fatal("explicit --db invoked discovery")
		return "", nil
	}
	deps.newOperatorClient = func(string) (*operatorclient.Client, error) {
		t.Fatal("explicit --db created HTTP client")
		return nil, nil
	}
	deps.verifyHubStopped = func(context.Context, string) error { return nil }

	var out, errOut bytes.Buffer
	if err := runMachinesCommandWithDeps(t.Context(), []string{
		"--db", dbPath, "--machine-id", machineID, "--state", string(state.NeverReported),
		"--lifecycle", "active", "--limit", "1",
	}, &out, &errOut, deps); err != nil {
		t.Fatalf("direct list: %v; stderr=%s", err, errOut.String())
	}
	if !strings.Contains(out.String(), "direct DB operator service") || !strings.Contains(out.String(), machineID) {
		t.Fatalf("direct output=%q", out.String())
	}
}

func TestMachinesCLIExplicitDirectDBRejectsDifferentWorkloadPolicy(t *testing.T) {
	dbPath, _ := directDBFixture(t)
	publishMachineReadTestPolicy(t, dbPath)
	policyPath := t.TempDir() + "/different-expectations.json"
	if err := os.WriteFile(policyPath, []byte(`{"expectations":[{"machine":"*","unit":"private-unit","artifact":"/private/sentinel","why":"different policy","max_age_seconds":60}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAWCTL_EXPECTATIONS", policyPath)
	deps := productionMachineCommandDeps()
	deps.verifyHubStopped = func(context.Context, string) error { return nil }

	var out, errOut bytes.Buffer
	err := runMachinesCommandWithDeps(t.Context(), []string{"--db", dbPath}, &out, &errOut, deps)
	if err == nil || !errors.Is(err, store.ErrWorkloadPolicyIdentityMismatch) || out.Len() != 0 {
		t.Fatalf("policy mismatch error=%v stdout=%q stderr=%q", err, out.String(), errOut.String())
	}
}

func publishMachineReadTestPolicy(t *testing.T, dbPath string) {
	t.Helper()
	t.Setenv("CLAWCTL_EXPECTATIONS", "")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	loadExpectations(st)
	if err := st.PublishExpectationsPolicy(time.Now().UTC()); err != nil {
		_ = st.Close()
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestMachinesCLIRejectsAmbiguousArgumentsBeforeIO(t *testing.T) {
	var currentArgs []string
	deps := machineCommandDeps{
		discoverHubURL: func() (string, error) {
			t.Fatalf("invalid arguments reached discovery: %q", currentArgs)
			return "", nil
		},
	}
	for _, args := range [][]string{
		{"--hub-url", "http://100.64.0.1:8787", "--db", "/tmp/not-used"},
		{"list", "extra"},
		{"show"},
		{"show", "one", "two"},
		{"evidence"},
		{"evidence", "one", "two"},
		{"evidence", "--limit", "0", "machine-id"},
		{"evidence", "--limit", "101", "machine-id"},
		{"evidence", " machine-id"},
		{"evidence", "machine\nid"},
		{"--state", "not-a-state"},
		{"--state", string(state.Online), "--state", string(state.Online)},
		{"--lifecycle", "deleted"},
		{"--expected", "true"},
		{"--limit", "0"},
		{"--limit", "1", "--limit", "2"},
		{"--cursor", "not-base64url"},
		{"--display-name", ""},
		{"--display-name", "   "},
	} {
		currentArgs = args
		var out, errOut bytes.Buffer
		if err := runMachinesCommandWithDeps(t.Context(), args, &out, &errOut, deps); err == nil {
			t.Fatalf("args %q were accepted", args)
		}
		if out.Len() != 0 {
			t.Fatalf("args %q wrote stdout=%q", args, out.String())
		}
	}
}

func TestMachinesEvidenceCLIUsesHTTPEmitsJSONAndEscapesMultilineText(t *testing.T) {
	f := observedOperatorFixture(t)
	now := time.Now().UTC().Truncate(time.Second)
	if err := f.store.RecordObservation(f.machine.id, model.ObservationBatch{
		SchemaVersion: model.SchemaVersion, MeasuredAt: now.Add(-time.Minute),
		Credentials: []model.Credential{{
			Provider: "codex", Status: model.CredConfigured, VerificationMethod: model.VerifyFileParse,
			ActiveAccountID: "PRIVATE_ACCOUNT_ID", AccountCount: 2,
		}},
		Systemd: []model.Unit{{Name: "heard.service", Present: true, ActiveState: "active", SubState: "running",
			NRestarts: 2, MainPID: 987654}, {Name: "missing.service"}},
		Journals: []model.UnitJournal{{
			Unit: "heard.service", Lines: 1, Shapes: 1,
			Top: []model.JournalShape{{Count: 1, Example: "before\x1b[31m\rafter"}},
		}},
		OpenClaw: model.OpenClaw{Present: true, CLIVersion: "2026.9.1", GatewayVersion: "2026.9.0",
			Install: &model.OpenClawInstall{UnitFound: true, UnitPath: "/PRIVATE/UNIT", MainPID: 765432,
				NodePath: "/PRIVATE/NODE", NodeVersion: "v24.1", RunningDir: "/PRIVATE/RUNNING",
				ProcessIndexJS: "/PRIVATE/INDEX"},
			DB: &model.OpenClawDB{Present: true, Path: "/PRIVATE/DB", FoundAt: []string{"/PRIVATE/OTHER_DB"},
				TaskStatusCount: map[string]int{"ok": 1}, OccupancyRowsSeen: 1,
				Occupancy: []model.OccupancyEvidence{{
					Source: "cron_run_logs", JobID: "PRIVATE_JOB", At: now.Add(-time.Minute),
					Provider: "openai", AgentID: "main", SessionKey: "PRIVATE_SESSION",
				}},
				RecentSummaries: []model.RunSummary{{
					JobID: "job-cli", At: now.Add(-time.Hour), Status: "ok", Summary: "line one\nline two",
				}},
			}},
		CLITools: []model.CLITool{{Name: "openclaw", Present: true, OnPath: true, PresentEvidence: "path",
			Path: "/PRIVATE/CLI", RealPath: "/PRIVATE/CLI_REAL", RunningPID: 456789,
			RunningScript: "/PRIVATE/CLI_RUNNING", VersionReported: "2026.9.1",
			ProcessScan: model.ProcessScanComplete}},
	}, now.Add(-30*time.Second)); err != nil {
		t.Fatal(err)
	}
	base := "http://100.64.0.9:8787"
	client, requests := operatorClientForMuxWithoutListener(t, f.mux, base)
	deps := productionMachineCommandDeps()
	deps.discoverHubURL = func() (string, error) { return base, nil }
	deps.newOperatorClient = func(got string) (*operatorclient.Client, error) {
		if got != base {
			return nil, errors.New("unexpected machine evidence authority")
		}
		return client, nil
	}
	deps.validateDirectPath = func(string) error {
		t.Fatal("HTTP machine evidence inspected a direct DB")
		return nil
	}
	deps.verifyHubStopped = func(context.Context, string) error {
		t.Fatal("HTTP machine evidence checked the local Hub unit")
		return nil
	}
	missingDB := filepath.Join(t.TempDir(), "must-not-exist.sqlite")
	t.Setenv("CLAWCTL_DB", missingDB)

	var jsonOut, errOut bytes.Buffer
	if err := runMachinesCommandWithDeps(t.Context(), []string{
		"evidence", f.machine.id, "--json", "--limit", "1",
	}, &jsonOut, &errOut, deps); err != nil {
		t.Fatalf("HTTP machine evidence JSON: %v; stderr=%s", err, errOut.String())
	}
	var evidence operator.MachineEvidenceResult
	if err := json.Unmarshal(jsonOut.Bytes(), &evidence); err != nil || evidence.MachineID != f.machine.id ||
		evidence.Disclosure.Limit != 1 || len(evidence.SystemdUnits.Items) != 1 ||
		!evidence.OpenClaw.Decoded || evidence.OpenClaw.Install == nil || evidence.OpenClaw.DB == nil ||
		len(evidence.CLITools.Items) != 1 || !evidence.Disclosure.HostPathFieldsExcluded ||
		!evidence.Disclosure.ProcessIDFieldsExcluded || evidence.Disclosure.EvidenceTextPathRedactedByHub ||
		len(evidence.Credentials.Items) != 1 || !evidence.Disclosure.CredentialActiveAccountIDExcluded ||
		!evidence.Disclosure.CredentialSecretFieldsExcluded || !evidence.Disclosure.OccupancyAggregatedByHub ||
		!evidence.Disclosure.SystemdMainPIDExcluded || evidence.Disclosure.SystemdStateIsWorkOutcome ||
		len(evidence.RunSummaries.Items) != 1 ||
		!evidence.Disclosure.JournalSecretShapesRedactedByAgent ||
		evidence.Disclosure.RunSummarySecretShapesRedactedByAgent {
		t.Fatalf("machine evidence=%+v err=%v body=%s", evidence, err, jsonOut.String())
	}
	for _, forbidden := range []string{
		`"active_account_id"`, `"profile_id"`, `"session_key"`, `"token_hash"`,
		`"path":`, `"realpath":`, `"unit_path":`, `"main_pid":`, `"running_pid":`,
		"PRIVATE_ACCOUNT_ID", "PRIVATE_JOB", "PRIVATE_SESSION", "/PRIVATE/UNIT", "/PRIVATE/NODE",
		"/PRIVATE/RUNNING", "/PRIVATE/INDEX", "/PRIVATE/DB", "/PRIVATE/OTHER_DB", "/PRIVATE/CLI",
		"/PRIVATE/CLI_REAL", "/PRIVATE/CLI_RUNNING", "765432", "456789",
	} {
		if strings.Contains(jsonOut.String(), forbidden) {
			t.Fatalf("machine evidence JSON exposed excluded field %s: %s", forbidden, jsonOut.String())
		}
	}

	var textOut bytes.Buffer
	if err := runMachinesCommandWithDeps(t.Context(), []string{
		"evidence", "--hub-url", base, f.machine.id,
	}, &textOut, &errOut, deps); err != nil {
		t.Fatalf("HTTP machine evidence text: %v; stderr=%s", err, errOut.String())
	}
	text := textOut.String()
	for _, want := range []string{
		"journal_producer:", "systemd_producer:", "credential_producer:", "occupancy_producer:",
		"openclaw_producer:", "cli_tool_producer:", "host_path_fields_excluded: true",
		"process_id_fields_excluded: true", "evidence_text_path_redacted_by_hub: false",
		"cli_openclaw_text_redacted_by_agent: false", "cli_openclaw_text_redacted_by_hub: false",
		"openclaw: observed=true decoded=true present=true", "CLI_VERSION: \"2026.9.1\"",
		"cli tools: TOTAL 1", "RUNNING_RELATIONSHIP: \"different\"", "PROCESS_SCAN: \"complete\"",
		"occupancy_relay:", "run_summary_producer:", "run_summary_relay:",
		"credential_status_is_session_validity: false", "credential_active_account_id_excluded: true",
		"credential_secret_fields_excluded: true", "occupancy_aggregated_by_hub: true",
		"occupancy_process_state_used: false", "occupancy_provider_normalized: false",
		"occupancy_errors_categorized: false", "credentials: TOTAL 1", "occupancy: TOTAL 1",
		`PROVIDER: "codex"`, `PROVIDER: "openai"`, `SOURCE: "cron_run_logs"`, "ERRORS(unclassified): 0",
		"systemd_state_is_work_outcome: false", "systemd_main_pid_excluded: true",
		"systemd units: TOTAL 2", `STATE: "active"/"running"`, "N_RESTARTS: 2",
		"journal_secret_shapes_redacted_by_agent: true",
		"run_summary_secret_shapes_redacted_by_agent: false", "observed=true db_observed=true",
		`SUMMARY: "line one\nline two"`, "units_without_journal (not collected this round; not quiet):",
		`- "missing.service"`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("machine evidence text missing %q: %q", want, text)
		}
	}
	if strings.Contains(text, "line one\nline two") || strings.ContainsAny(text, "\x1b\r") {
		t.Fatalf("machine evidence text emitted unsafe raw controls or multiline summary: %q", text)
	}
	for _, private := range []string{"/PRIVATE/UNIT", "/PRIVATE/NODE", "/PRIVATE/RUNNING", "/PRIVATE/INDEX",
		"/PRIVATE/DB", "/PRIVATE/OTHER_DB", "/PRIVATE/CLI", "/PRIVATE/CLI_REAL", "/PRIVATE/CLI_RUNNING",
		"765432", "456789"} {
		if strings.Contains(text, private) {
			t.Fatalf("machine evidence text exposed host coordinate %q: %q", private, text)
		}
	}
	if requests.Load() != 2 {
		t.Fatalf("machine evidence HTTP requests=%d want=2", requests.Load())
	}
	emptyUnits := evidence
	emptyUnits.Journals.UnitsWithoutJournal = []operator.EvidenceText{}
	emptyUnits.Journals.UnitsWithoutJournalTotal = 0
	emptyUnits.Journals.UnitsWithoutJournalTruncated = false
	var emptyOut bytes.Buffer
	if err := writeMachineEvidence(&emptyOut, emptyUnits, false, "HTTP operator API"); err != nil ||
		!strings.Contains(emptyOut.String(), "units_without_journal (not collected this round; not quiet): TOTAL 0") {
		t.Fatalf("empty units_without_journal was hidden: err=%v output=%q", err, emptyOut.String())
	}
	truncatedUnits := evidence
	truncatedUnits.Journals.Truncated = false
	truncatedUnits.Journals.UnitsWithoutJournalTotal = 5
	truncatedUnits.Journals.UnitsWithoutJournalTruncated = true
	var truncatedOut bytes.Buffer
	if err := writeMachineEvidence(&truncatedOut, truncatedUnits, false, "HTTP operator API"); err != nil ||
		!strings.Contains(truncatedOut.String(),
			"units_without_journal (not collected this round; not quiet): TOTAL 5 truncated=true") {
		t.Fatalf("CLI 沒有說這份「沒在觀測的 unit」清單被截掉了，操作員會以為它是完整的：err=%v output=%q",
			err, truncatedOut.String())
	}
	if _, err := os.Stat(missingDB); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("HTTP machine evidence touched default DB: %v", err)
	}
}

func TestMachinesEvidenceCLIQuotesClosedValueFields(t *testing.T) {
	f := observedOperatorFixture(t)
	now := time.Now().UTC().Truncate(time.Second)
	if err := f.store.RecordObservation(f.machine.id, model.ObservationBatch{
		SchemaVersion: model.SchemaVersion,
		MeasuredAt:    now.Add(-time.Minute),
		CLITools: []model.CLITool{
			{Name: "empty-support"},
			{Name: "supported-tool", Support: model.SupportOK},
		},
		Credentials: []model.Credential{
			{Provider: "legacy", Status: model.CredConfigured},
			{Provider: "file", Status: model.CredConfigured, VerificationMethod: model.VerifyFileParse},
		},
	}, now.Add(-30*time.Second)); err != nil {
		t.Fatal(err)
	}

	base := "http://100.64.0.9:8787"
	client, _ := operatorClientForMuxWithoutListener(t, f.mux, base)
	deps := productionMachineCommandDeps()
	deps.newOperatorClient = func(got string) (*operatorclient.Client, error) {
		if got != base {
			return nil, errors.New("unexpected machine evidence authority")
		}
		return client, nil
	}

	var out, errOut bytes.Buffer
	if err := runMachinesCommandWithDeps(t.Context(), []string{
		"evidence", "--hub-url", base, f.machine.id,
	}, &out, &errOut, deps); err != nil {
		t.Fatalf("machine evidence text: %v; stderr=%s", err, errOut.String())
	}
	text := out.String()
	for _, want := range []string{
		`SUPPORT: ""`,
		`SUPPORT: "supported"`,
		`STATUS: "configured"`,
		`VERIFICATION_METHOD: ""`,
		`VERIFICATION_METHOD: "file_parse"`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("machine evidence text missing %q: %q", want, text)
		}
	}
	for _, forbidden := range []string{"SUPPORT: \n", "VERIFICATION_METHOD: \n"} {
		if strings.Contains(text, forbidden) {
			t.Errorf("machine evidence text contains unquoted empty value %q: %q", forbidden, text)
		}
	}
}

func TestMachinesEvidenceCLIExplicitDBUsesShowFence(t *testing.T) {
	dbPath, machineID := directDBFixture(t)
	deps := productionMachineCommandDeps()
	deps.discoverHubURL = func() (string, error) {
		t.Fatal("explicit machine evidence --db invoked discovery")
		return "", nil
	}
	deps.newOperatorClient = func(string) (*operatorclient.Client, error) {
		t.Fatal("explicit machine evidence --db constructed HTTP client")
		return nil, nil
	}
	deps.verifyHubStopped = func(context.Context, string) error {
		return errors.New("clawctl-hub.service is active")
	}
	var out, errOut bytes.Buffer
	err := runMachinesCommandWithDeps(t.Context(), []string{
		"evidence", "--db", dbPath, "--json", machineID,
	}, &out, &errOut, deps)
	if err == nil || !strings.Contains(err.Error(), "clawctl-hub.service is active") || out.Len() != 0 {
		t.Fatalf("live-Hub machine evidence fence error=%v stdout=%q", err, out.String())
	}
}

func TestWriteMachineEvidencePrintsSystemdPresenceThreeStates(t *testing.T) {
	reason := operator.EvidenceText{Text: "Failed to connect to bus"}
	evidence := operator.MachineEvidenceResult{SystemdUnits: operator.MachineSystemdUnitPage{
		Total: 3,
		Items: []operator.MachineSystemdUnit{
			{Name: operator.EvidenceText{Text: "present.service"}, Present: true, Measured: true},
			{Name: operator.EvidenceText{Text: "absent.service"}, Measured: true},
			{Name: operator.EvidenceText{Text: "unknown.service"}, Reason: &reason},
		},
	}}
	var out bytes.Buffer
	if err := writeMachineEvidence(&out, evidence, false, "test"); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	for _, tc := range []struct{ unit, present, reason string }{
		{"present.service", "true", ""},
		{"absent.service", "false", ""},
		{"unknown.service", "unknown", `"Failed to connect to bus"`},
	} {
		start := strings.Index(text, "UNIT: \""+tc.unit+"\"")
		if start < 0 {
			t.Fatalf("missing unit %q in %q", tc.unit, text)
		}
		section := text[start:]
		if next := strings.Index(section[len(tc.unit):], "\nUNIT: "); next >= 0 {
			section = section[:len(tc.unit)+next]
		}
		if !strings.Contains(section, "PRESENT: "+tc.present+"\n") ||
			(tc.reason != "" && !strings.Contains(section, "REASON: "+tc.reason+"\n")) ||
			(tc.reason == "" && strings.Contains(section, "REASON:")) {
			t.Errorf("unit %s section=%q", tc.unit, section)
		}
	}
}

func TestMachinesCLIHumanOutputEscapesTerminalControls(t *testing.T) {
	machineState := state.Online
	reporting := true
	nextCursor := "copyable_cursor-123"
	result := operator.MachineListResult{
		SchemaVersion: operator.MachineReadSchemaVersion,
		EvaluatedAt:   time.Date(2026, 9, 7, 12, 0, 0, 123, time.UTC),
		Total:         1, Active: 1, Expected: 1, Reporting: 1,
		Items: []operator.MachineSummary{{
			MachineID: "machine-\x1b[31m", DisplayName: "name\r\n\x1b]2;owned\x07",
			Expected: true, CreatedAt: time.Date(2026, 9, 7, 11, 0, 0, 0, time.UTC),
			State: &machineState, Reporting: &reporting,
		}},
		NextCursor: &nextCursor,
	}
	var out bytes.Buffer
	if err := writeMachineList(&out, result, false, "HTTP operator API"); err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(out.String(), "\x1b\r\x07") || !strings.Contains(out.String(), `\x1b`) ||
		!strings.Contains(out.String(), `\r\n`) ||
		!strings.Contains(out.String(), "2026-09-07T12:00:00.000000123Z") ||
		!strings.Contains(out.String(), "next cursor: copyable_cursor-123\n") ||
		strings.Contains(out.String(), `next cursor: "copyable_cursor-123"`) {
		t.Fatalf("human output did not safely quote controls: %q", out.String())
	}
	terminalError := terminalSafe("upstream\r\n\x1b]2;owned\x07")
	if strings.ContainsAny(terminalError, "\x1b\r\x07") || !strings.Contains(terminalError, `\x1b`) {
		t.Fatalf("CLI error boundary did not safely quote controls: %q", terminalError)
	}

	clockSkew := int64(3)
	agentSeq := int64(9)
	detail := operator.MachineDetailResult{
		SchemaVersion: operator.MachineDetailReadSchemaVersion,
		EvaluatedAt:   result.EvaluatedAt,
		Item:          result.Items[0],
		Judgement: operator.MachineJudgement{
			State: state.Online, AffectsFleetState: true,
			Reason: operator.EvidenceText{Text: "judgement\r\n\x1b[31m"},
			Findings: operator.MachineFindingPage{Total: 1, Items: []operator.MachineFinding{{
				Kind: "agent", Severity: 2, Advisory: true,
				Message: operator.EvidenceText{Text: "finding /private/path\r\n\x1b[31m"},
			}}},
		},
		Disclosure: operator.MachineDetailDisclosure{
			ObserverProducer:       operator.MachineEvidenceProducer{Kind: "observer\x1b[31m", Authority: "machine_bearer"},
			LivenessUsesReceivedAt: true, HostIdentityIncluded: true, TailscaleIPIncluded: true,
			ConnectCoordinatesExcluded: true, PendingEnrollmentExcluded: true,
			ExpectationDisplayDefinitionsIncluded: true, ExpectationConfigPathFieldExcluded: true,
			ExpectationParserFieldsExcluded: true, ArtifactPathsIncluded: true,
			EventFailureTypesOperatorDeclared: true,
		},
		Expectations: operator.MachineExpectationSection{Configured: true, Rules: operator.MachineExpectationPage{
			Total: 1, Items: []operator.MachineExpectation{{
				Unit:     operator.EvidenceText{Text: "unit\x1b[31m"},
				Artifact: operator.EvidenceText{Text: "/private/artifact\r\n\x1b[31m"},
				Why:      operator.EvidenceText{Text: "why\x1b[31m"}, MaxAgeSeconds: 60,
			}},
		}},
		Monitor: operator.MachineMonitor{CheckinIntervalSeconds: 120, ClockSkewSeconds: &clockSkew},
		Checkins: operator.MachineCheckinPage{Items: []operator.MachineCheckin{{
			SentAt: result.EvaluatedAt, ReceivedAt: result.EvaluatedAt,
			AgentVersion: &operator.EvidenceText{Text: "agent\x1b[31m"}, AgentSeq: &agentSeq,
		}}},
		Identity: operator.MachineIdentityEvidence{Observed: true, Decoded: true, Value: &operator.MachineIdentity{
			Hostname: operator.EvidenceText{Text: "host\r\n\x1b]2;owned\x07"},
		}},
		IdentityHints: operator.MachineIdentityHintPage{Items: []operator.MachineIdentityHint{}},
		Resources:     operator.MachineResourceEvidence{},
		StateHistory: operator.MachineStateHistoryPage{Items: []operator.MachineStateSpan{{
			State: state.Online, EnteredAt: result.EvaluatedAt,
			Reason: operator.EvidenceText{Text: "reason\r\n\x1b[31m"},
		}}},
	}
	out.Reset()
	if err := writeMachineDetail(&out, detail, false, "HTTP operator API", nil); err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(out.String(), "\x1b\r\x07") ||
		!strings.Contains(out.String(), "liveness_uses_received_at: true") ||
		!strings.Contains(out.String(), "connect_coordinates_excluded: true") ||
		!strings.Contains(out.String(), "expectation_display_definitions_included: true") ||
		!strings.Contains(out.String(), `/private/artifact\r\n\x1b`) ||
		!strings.Contains(out.String(), "state_reason_path_redacted_by_hub: false") ||
		!strings.Contains(out.String(), `judgement: state="Online" affects_fleet_state=true`) ||
		!strings.Contains(out.String(), `finding /private/path\r\n\x1b`) ||
		!strings.Contains(out.String(), `host\r\n\x1b`) {
		t.Fatalf("machine detail human output did not preserve typed disclosure safely: %q", out.String())
	}
}

func TestMachineDetailCLISaysUnknownWhenLingerWasNeverMeasured(t *testing.T) {
	tests := []struct {
		name           string
		lingerEnabled  bool
		lingerMeasured bool
		want           string
		forbid         string
	}{
		{
			name: "未量到時顯示 unknown", lingerEnabled: false, lingerMeasured: false,
			want: "linger_enabled: unknown\n", forbid: "linger_enabled: false",
		},
		{
			name: "量到但未開啟時顯示 false", lingerEnabled: false, lingerMeasured: true,
			want: "linger_enabled: false\n",
		},
		{
			name: "量到且已開啟時顯示 true", lingerEnabled: true, lingerMeasured: true,
			want: "linger_enabled: true\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			detail := operator.MachineDetailResult{
				SchemaVersion: operator.MachineDetailReadSchemaVersion,
				EvaluatedAt:   time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC),
				Checkins:      operator.MachineCheckinPage{Items: []operator.MachineCheckin{}},
				StateHistory:  operator.MachineStateHistoryPage{Items: []operator.MachineStateSpan{}},
				Identity: operator.MachineIdentityEvidence{Value: &operator.MachineIdentity{
					LingerEnabled: tt.lingerEnabled, LingerMeasured: tt.lingerMeasured,
				}},
				IdentityHints: operator.MachineIdentityHintPage{Items: []operator.MachineIdentityHint{}},
			}

			var out bytes.Buffer
			if err := writeMachineDetail(&out, detail, false, "direct store", nil); err != nil {
				t.Fatal(err)
			}
			got := out.String()
			if !strings.Contains(got, tt.want) {
				t.Fatalf("CLI 的 linger 輸出錯誤：找不到 %q，完整輸出為 %q；CLI 把未量到印成 false，會跟網頁上的「未知」互相打架，操作者會照 CLI 去查一台根本沒問題的機器", tt.want, got)
			}
			if tt.forbid != "" && strings.Contains(got, tt.forbid) {
				t.Fatalf("CLI 不該輸出 %q，完整輸出為 %q；CLI 把未量到印成 false，會跟網頁上的「未知」互相打架，操作者會照 CLI 去查一台根本沒問題的機器", tt.forbid, got)
			}
		})
	}
}

func TestMachineDetailCLISaysUnknownWhenMemoryWasNeverMeasured(t *testing.T) {
	tests := []struct {
		name      string
		resources operator.MachineResources
		want      []string
		forbid    string
	}{
		{
			name: "未量到時顯示 unknown",
			resources: operator.MachineResources{
				MemMeasured: false,
			},
			want:   []string{"mem_total_bytes: unknown", "mem_available_bytes: unknown"},
			forbid: "mem_total_bytes: 0",
		},
		{
			name: "量到時顯示實際數字",
			resources: operator.MachineResources{
				MemMeasured: true, MemTotalBytes: 16 << 30, MemAvailableBytes: 8 << 30,
			},
			want: []string{"mem_total_bytes: 17179869184", "mem_available_bytes: 8589934592"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			detail := operator.MachineDetailResult{
				SchemaVersion: operator.MachineDetailReadSchemaVersion,
				EvaluatedAt:   time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC),
				Checkins:      operator.MachineCheckinPage{Items: []operator.MachineCheckin{}},
				IdentityHints: operator.MachineIdentityHintPage{Items: []operator.MachineIdentityHint{}},
				Resources:     operator.MachineResourceEvidence{Value: &tt.resources},
				StateHistory:  operator.MachineStateHistoryPage{Items: []operator.MachineStateSpan{}},
			}

			var out bytes.Buffer
			if err := writeMachineDetail(&out, detail, false, "direct store", nil); err != nil {
				t.Fatal(err)
			}
			got := out.String()
			for _, want := range tt.want {
				if !strings.Contains(got, want) {
					t.Fatalf("CLI 記憶體輸出找不到 %q，完整輸出為 %q；把沒量到印成 0 B，operator 會誤以為那台機器記憶體耗盡", want, got)
				}
			}
			if tt.forbid != "" && strings.Contains(got, tt.forbid) {
				t.Fatalf("CLI 不該輸出 %q，完整輸出為 %q；把沒量到印成 0 B，operator 會誤以為那台機器記憶體耗盡", tt.forbid, got)
			}
		})
	}
}

func TestMachineDetailCLISaysUnknownWhenLoadWasNeverMeasured(t *testing.T) {
	zero, measured := 0.0, 1.25
	tests := []struct {
		name string
		load *float64
		want string
	}{
		{name: "沒量到", load: nil, want: "load_1m: unknown"},
		{name: "真的零負載", load: &zero, want: "load_1m: 0.00"},
		{name: "量到負載", load: &measured, want: "load_1m: 1.25"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			detail := operator.MachineDetailResult{
				SchemaVersion: operator.MachineDetailReadSchemaVersion,
				EvaluatedAt:   time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC),
				Checkins:      operator.MachineCheckinPage{Items: []operator.MachineCheckin{}},
				IdentityHints: operator.MachineIdentityHintPage{Items: []operator.MachineIdentityHint{}},
				Resources: operator.MachineResourceEvidence{Value: &operator.MachineResources{
					Load1m: tt.load,
				}},
				StateHistory: operator.MachineStateHistoryPage{Items: []operator.MachineStateSpan{}},
			}
			var out bytes.Buffer
			if err := writeMachineDetail(&out, detail, false, "direct store", nil); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.String(), tt.want) {
				t.Fatalf("CLI 負載輸出找不到 %q，完整輸出為 %q；把沒量到印成 0.00，operator 會以為那台 Mac 很閒", tt.want, out.String())
			}
		})
	}
}
