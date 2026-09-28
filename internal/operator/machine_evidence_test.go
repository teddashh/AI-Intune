package operator

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/model"
	"github.com/teddashh/AI-Intune/internal/store"
)

// ⚠⚠ TestMachineEvidenceWireKeysAreAnExactSet 只守 operator wire 的結構鍵集合，不是內容掃描器：
//   - 抓不到 journal example、run summary、credential note 與各種 *Reason 自由文字裡的
//     路徑、PID 或密鑰；DTO 本來就公開宣告 EvidenceTextPathRedactedByHub=false。
//   - 抓不到新鍵語意上是不是主機座標；它只會抓到「有新鍵」。要過關必須修改
//     allowlist，那次 review 才是決定是否維持 *Excluded=true、是否改 schema version 的地方。
//   - 抓不到鍵在不同層之間搬家；這是把所有 object key 收成單一扁平集合的代價。
//   - 抓不到旗標在形狀不變時被改成 false；那條由 internal/operatorclient/machine_evidence.go
//     的既有斷言守住。
//   - store／model 內部仍持有主機座標；它們是投影的輸入，不是 operator wire。
//
// 這裡故意用扁平集合，守的是「整份 wire 有沒有長出新欄位」，不是每個子結構各自的形狀。
func TestMachineEvidenceWireKeysAreAnExactSet(t *testing.T) {
	st := newOperatorJobReadStore(t)
	now := time.Date(2026, 9, 16, 14, 0, 0, 0, time.UTC)
	if err := st.UpsertMachine(store.Machine{
		MachineID: "machine-wire-keys", DisplayName: "wire-keys", Expected: true, CreatedAt: now.Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	writable, matches := true, true
	restarts := 2
	expiresAt, fileMTime := now.Add(8*time.Hour), now.Add(-time.Hour)
	if err := st.RecordObservation("machine-wire-keys", model.ObservationBatch{
		SchemaVersion: model.SchemaVersion,
		MeasuredAt:    now.Add(-2 * time.Minute),
		Systemd: []model.Unit{{
			Name: "openclaw.service", Present: true, Measured: true, Reason: "unit reason",
			ActiveState: "active", SubState: "running", ActiveEnterTimestamp: machineEvidenceTimePtr(now.Add(-time.Hour)), NRestarts: 1,
		}},
		Journals: []model.UnitJournal{{
			Unit: "openclaw.service", WindowSec: 3600, Lines: 4, Truncated: true, Shapes: 1,
			Top: []model.JournalShape{{Count: 4, Example: "journal example"}},
		}},
		OpenClaw: model.OpenClaw{
			Present: true, Reason: "openclaw reason", CLIVersion: "1.2.3", CLIVersionRaw: "openclaw 1.2.3",
			GatewayVersion: "1.2.2", UpstreamVersion: "1.2.4", CrashBundles: 1,
			Install: &model.OpenClawInstall{
				UnitFound: true, UnitReason: "unit found", DropInPaths: []string{"/private/drop-in"}, ExecStart: "/private/openclaw",
				KillMode: "control-group", NRestarts: &restarts, ActiveEnterAt: machineEvidenceTimePtr(now.Add(-time.Hour)),
				NodePath: "/private/node", NodeVersion: "v24", NodeVersionReason: "node reason",
				NpmPath: "/private/npm", NpmVersion: "11", NpmReason: "npm reason",
				RunningDir: "/private/running", RunningDirExists: true, RunningDirWritable: &writable,
				RunningDirVersion: "1.2.2", RunningDirReason: "running reason", ProcessIndexJS: "/private/index.js",
				ProcessMatchesUnit: &matches, ProcessReason: "process reason", ReleasesDir: "/private/releases",
				ReleasesPresent: true, CurrentLink: "/private/current", DiskFreeBytes: 1024, DiskFreeMeasured: true,
				DiskFreeReason: "disk reason",
			},
			DB: &model.OpenClawDB{
				Present: true, Reason: "db reason", Layout: "consolidated", Path: "/private/openclaw.db",
				FoundAt: []string{"/private/other.db"}, LastTaskEndedAt: machineEvidenceTimePtr(now.Add(-time.Hour)), TaskRunRows: 3,
				TaskStatusCount: map[string]int{"ok": 3}, LastCronRunAt: machineEvidenceTimePtr(now.Add(-time.Hour)),
				CronRunLogRows: 2, CronStatusCount: map[string]int{"ok": 2}, CronJobsTotal: 2, CronJobsTotalMeasured: true,
				CronJobsEnabled: 1, CronJobsEnabledMeasured: true, NextCronRunAt: machineEvidenceTimePtr(now.Add(time.Hour)),
				CronJobsOverdue: 1, CronJobsScheduleMeasured: true, TerminalOutcomePopulated: 1,
				OccupancyRowsSeen: 1, Occupancy: []model.OccupancyEvidence{{
					Source: "cron_run_logs", JobID: "private-job", At: now.Add(-time.Minute), Provider: "openai", AgentID: "agent",
				}},
				RecentSummaries: []model.RunSummary{{JobID: "job", At: now.Add(-time.Minute), Status: "ok", Summary: "summary"}},
			},
		},
		CLITools: []model.CLITool{{
			Name: "openclaw", Present: true, OnPath: true, PresentEvidence: "path", Path: "/private/bin/openclaw",
			PathSource: model.PathSourceLogin, PathReason: "path reason", DaemonReach: model.DaemonReachSame,
			VersionReported: "1.2.3", VersionRaw: "openclaw 1.2.3", VersionPackageJSON: "1.2.3", VersionReason: "version reason",
			SourcesDisagree: true, RunningPID: 123, RunningScript: "/private/index.js", ProcessScan: model.ProcessScanComplete,
			Support: model.SupportOK,
		}, {
			Name: "other", Present: true, ProcessScan: model.ProcessScanComplete, RunningReason: "running reason",
		}},
		Credentials: []model.Credential{{
			Provider: "codex", Status: model.CredConfigured, ExpiresAt: &expiresAt, LastRefresh: &fileMTime,
			FileMTime: &fileMTime, VerifiedAt: &fileMTime, VerificationMethod: model.VerifyLiveRequest,
			ActiveAccountID: "private-account", AccountCount: 2, Note: "credential note", LastError: "credential error",
		}},
	}, now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}

	result, err := New(st).MachineEvidence(MachineEvidenceRequest{MachineID: "machine-wire-keys"}, now)
	if err != nil {
		t.Fatal(err)
	}
	// Credential peer 是 store 由跨機器 refresh 歷史衍生；上面其餘區段都是真實 store
	// 投影，只有這個子結構在真實頁面為底之上補齊，讓遞迴會走進 peer array element。
	result.Credentials.Items[0].Peers = []MachineCredentialPeer{{
		DisplayName: EvidenceText{Text: "peer"}, RefreshesSeen: 1, FileMTime: &fileMTime,
	}}

	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var document any
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	got := make(map[string]bool)
	collectMachineEvidenceJSONKeys(document, got)
	want := []string{
		"account_count", "active_account_selected", "active_enter_at", "active_enter_timestamp", "active_state",
		"agents", "agents_total", "agents_truncated", "authority", "bytes",
		"cli_openclaw_text_redacted_by_agent", "cli_openclaw_text_redacted_by_hub", "cli_tool_producer", "cli_tools",
		"cli_version", "cli_version_raw", "cli_version_sources_collapsed", "count", "crash_bundles",
		"credential_active_account_id_excluded", "credential_peer_is_same_ticket", "credential_producer",
		"credential_remote_validation_performed", "credential_secret_fields_excluded", "credential_status_is_session_validity",
		"credential_text_redacted_by_agent", "credentials", "cron_jobs_enabled", "cron_jobs_enabled_measured",
		"cron_jobs_overdue", "cron_jobs_schedule_measured", "cron_jobs_total", "cron_jobs_total_measured",
		"cron_run_log_rows", "cron_statuses", "current_release_linked", "daemon_reach", "db", "db_invalid",
		"db_observed", "decoded", "disclosure", "disk_free_bytes", "disk_free_measured", "disk_free_reason",
		"display_name", "drop_in_count", "err", "errors", "evaluated_at", "evidence_text_path_redacted_by_hub",
		"example", "expires_at", "file_mtime", "gateway_version", "host_path_fields_excluded", "independent_verifier",
		"install", "install_invalid", "invalid", "issues", "items", "job_id", "journal_producer",
		"journal_secret_shapes_redacted_by_agent", "journals", "kill_mode", "kind", "last_cron_run_at", "last_error",
		"last_received_at", "last_refresh", "last_run_at", "last_task_ended_at", "layout", "lifetime_seconds", "limit",
		"lines", "lines_are_lower_bound", "machine_id", "max_bytes", "max_credential_peers", "max_field_bytes",
		"max_occupancy_agents", "max_shapes", "measured", "measured_at", "n_restarts", "name", "next_cron_run_at",
		"node_version", "node_version_reason", "note", "npm_observed", "npm_reason", "npm_version",
		"observation_invalid", "observed", "observed_at", "occupancy", "occupancy_aggregated_by_hub",
		"occupancy_errors_categorized", "occupancy_event_details_excluded", "occupancy_process_state_used",
		"occupancy_producer", "occupancy_profile_id_excluded", "occupancy_provider_normalized", "occupancy_relay",
		"on_path", "openclaw", "openclaw_database_locations_excluded", "openclaw_producer", "path_reason", "path_source",
		"peers", "peers_total", "peers_truncated", "present", "present_evidence", "process_id_fields_excluded",
		"process_matches_unit", "process_observed", "process_reason", "process_scan", "provider",
		"raw_version_text_parsed_by_hub", "read_failed", "reason", "received_at", "refreshes_seen",
		"release_layout_observed", "releases_present", "reported_at", "rows_seen", "rows_without_provider",
		"run_summaries", "run_summary_producer", "run_summary_relay", "run_summary_secret_shapes_redacted_by_agent",
		"running_directory_exists", "running_directory_observed", "running_directory_reason", "running_directory_writable",
		"running_reason", "running_relationship", "running_relationship_derived_from_paths", "running_version", "runs",
		"schema_version", "shapes", "source", "status", "status_is_outcome", "sub_state", "summary", "support",
		"systemd_main_pid_excluded", "systemd_producer", "systemd_state_is_work_outcome", "systemd_units",
		"task_run_rows", "task_statuses", "terminal_outcome_populated", "terminal_outcome_recorded", "text", "top",
		"top_truncated", "total", "truncated", "undecodable", "unit", "unit_found", "unit_reason",
		"units_without_journal", "units_without_journal_total", "units_without_journal_truncated",
		"unknown_location_count", "upstream_version", "verification_method", "verified_at", "version_package_json",
		"version_raw", "version_reason", "version_reported", "version_sources_disagree", "watched_for_seconds", "window_seconds",
	}
	assertMachineEvidenceJSONKeySet(t, got, want)
}

func collectMachineEvidenceJSONKeys(value any, keys map[string]bool) {
	switch value := value.(type) {
	case map[string]any:
		for key, child := range value {
			keys[key] = true
			collectMachineEvidenceJSONKeys(child, keys)
		}
	case []any:
		for _, child := range value {
			collectMachineEvidenceJSONKeys(child, keys)
		}
	}
}

func assertMachineEvidenceJSONKeySet(t *testing.T, got map[string]bool, want []string) {
	t.Helper()
	wanted := make(map[string]bool, len(want))
	for _, key := range want {
		wanted[key] = true
	}
	var extra, missing []string
	for key := range got {
		if !wanted[key] {
			extra = append(extra, key)
		}
	}
	for key := range wanted {
		if !got[key] {
			missing = append(missing, key)
		}
	}
	sort.Strings(extra)
	sort.Strings(missing)
	if len(extra) != 0 || len(missing) != 0 {
		t.Fatalf("多出來的鍵=%v\n少掉的鍵=%v", extra, missing)
	}
}

func TestMachineEvidenceProjectionScrubsTruncatesAndLabels(t *testing.T) {
	st := newOperatorJobReadStore(t)
	evaluatedAt := time.Date(2026, 9, 9, 15, 0, 0, 0, time.FixedZone("offset", -4*60*60))
	receivedAt := evaluatedAt.Add(-time.Minute)
	measuredAt := evaluatedAt.Add(-2 * time.Minute)
	if err := st.UpsertMachine(store.Machine{
		MachineID: "machine-evidence", DisplayName: "evidence-machine", Expected: true,
		CreatedAt: evaluatedAt.Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	rawOversize := strings.Repeat("s", MachineEvidenceMaxFieldBytes+17)
	rawControlHeavy := strings.Repeat("\x00", 6000)
	rawExample := "before\x1b[31m\rafter"
	credentialMTime := evaluatedAt.Add(-time.Hour)
	credentialExpiry := credentialMTime.Add(8 * time.Hour)
	installWritable := false
	processMatches := false
	installRestarts := 4
	batch := model.ObservationBatch{
		SchemaVersion: model.SchemaVersion,
		MeasuredAt:    measuredAt,
		Systemd: []model.Unit{
			{Name: "control.service", Present: true, Measured: true, Reason: "bus\x1b failure", ActiveState: "active\x1b", SubState: "running\n",
				ActiveEnterTimestamp: machineEvidenceTimePtr(evaluatedAt.Add(-time.Hour)), NRestarts: 3, MainPID: 987654},
			{Name: "example.service", Present: true, ActiveState: "active", SubState: "running"},
			{Name: "undecodable.service", Present: true},
			{Name: "missing.service", Present: true},
		},
		Journals: []model.UnitJournal{
			{
				Unit: "example.service", WindowSec: 3600, Lines: 12, Truncated: true, Shapes: 11,
				Top: append([]model.JournalShape{{Count: 7, Example: rawExample}},
					repeatedJournalShapes(10, "extra")...),
			},
			{Unit: "control.service", Err: "\x01"},
		},
		Credentials: []model.Credential{{
			Provider: "codex", Status: model.CredConfigured, ExpiresAt: &credentialExpiry,
			FileMTime: &credentialMTime, VerificationMethod: model.VerifyFileParse,
			ActiveAccountID: "PRIVATE_ACCOUNT_ID", AccountCount: 3,
			Note: strings.Repeat("n", 4113), LastError: "permission denied\nretry\x1b[31m",
		}},
		OpenClaw: model.OpenClaw{
			Present: true, Reason: "diagnostic references /FREE_TEXT/PATH and pid 111",
			CLIVersion: "2026.9.1", CLIVersionRaw: "openclaw 2026.9.1\n",
			GatewayVersion: "2026.8.9", UpstreamVersion: "2026.8.8", CrashBundles: 2,
			Install: &model.OpenClawInstall{
				UnitFound: true, UnitPath: "/PRIVATE/UNIT", DropInPaths: []string{"/PRIVATE/DROPIN"},
				ExecStart: "/PRIVATE/NODE /PRIVATE/INDEX", KillMode: "control-group", NRestarts: &installRestarts,
				ActiveEnterAt: machineEvidenceTimePtr(evaluatedAt.Add(-time.Hour)), MainPID: 765432,
				NodePath: "/PRIVATE/NODE", NodeVersion: "v24.1", NpmPath: "/PRIVATE/NPM", NpmVersion: "11.2",
				RunningDir: "/PRIVATE/RUNNING", RunningDirExists: true, RunningDirOwner: "PRIVATE_OWNER",
				RunningDirWritable: &installWritable, RunningDirVersion: "2026.8.9",
				ProcessIndexJS: "/PRIVATE/INDEX", ProcessMatchesUnit: &processMatches,
				ReleasesDir: "/PRIVATE/RELEASES", ReleasesPresent: true, CurrentLink: "/PRIVATE/CURRENT",
				DiskFreeBytes: 1234, DiskFreeMeasured: true,
			},
			DB: &model.OpenClawDB{
				Present: true, Layout: "consolidated", Path: "/PRIVATE/OPENCLAW_DB",
				FoundAt: []string{"/PRIVATE/UNKNOWN_DB"}, TaskRunRows: 7,
				TaskStatusCount: map[string]int{"ok": 5, "custom": 2}, CronStatusCount: map[string]int{"ok": 3},
				CronRunLogRows: 3, CronJobsTotal: 2, CronJobsTotalMeasured: true,
				CronJobsEnabled: 1, CronJobsEnabledMeasured: true, TerminalOutcomePopulated: 0,
				OccupancyRowsSeen: 3, OccupancyRowsNoProvider: 1,
				Occupancy: []model.OccupancyEvidence{
					{Source: "cron_run_logs", JobID: "PRIVATE_JOB_A", At: evaluatedAt.Add(-3 * time.Minute),
						Provider: "openai", AgentID: "main", SessionKey: "PRIVATE_SESSION_A"},
					{Source: "cron_run_logs", JobID: "PRIVATE_JOB_B", At: evaluatedAt.Add(-2 * time.Minute),
						Provider: "openai", AgentID: "worker\x1b", SessionKey: "PRIVATE_SESSION_B", ErrorText: "PRIVATE_RUN_ERROR"},
				},
				RecentSummaries: []model.RunSummary{
					{JobID: "job-newline", At: evaluatedAt.Add(-time.Hour), Status: "ok", Summary: "line one\nline two"},
					{JobID: "job-oversize", Status: "custom", Summary: rawOversize},
					{JobID: "job-controls", Status: "ok", Summary: rawControlHeavy},
				},
			}},
		CLITools: []model.CLITool{
			{Name: "alpha", Present: true, OnPath: true, PresentEvidence: "path",
				Path: "/PRIVATE/CLI_PATH", RealPath: "/PRIVATE/CLI_REAL", PathSource: model.PathSourceLogin,
				DaemonReach: model.DaemonReachShadowed, DaemonPath: "/PRIVATE/CLI_DAEMON",
				VersionReported: "1.2.3", VersionPackageJSON: "1.2.4", SourcesDisagree: true,
				RunningPID: 456789, RunningScript: "/PRIVATE/CLI_RUNNING"},
			{Name: "bravo", Present: true, OnPath: true, PresentEvidence: "path",
				Path:       "/PRIVATE/BRAVO_PATH",
				VersionRaw: "nightly\nraw", VersionReason: "unknown version form", Support: model.SupportUnsupported},
		},
	}
	if err := st.RecordObservation("machine-evidence", batch, receivedAt); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`INSERT INTO observed_state
 (observation_id,machine_id,measured_at,received_at,kind,subject,payload,source)
 VALUES(?,?,?,?,?,?,?,?)`, "operator-invalid-cli", "machine-evidence",
		measuredAt.UTC().Format(time.RFC3339), receivedAt.UTC().Format(time.RFC3339),
		store.KindCLITool, "broken", `{"name":"other","present":true}`, store.SourceAgentMeasurement); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`INSERT INTO observed_state
 (observation_id,machine_id,measured_at,received_at,kind,subject,payload,source)
 VALUES(?,?,?,?,?,?,?,?)`, "operator-incoherent-cli", "machine-evidence",
		measuredAt.UTC().Format(time.RFC3339), receivedAt.UTC().Format(time.RFC3339),
		store.KindCLITool, "unsupported-no-raw",
		`{"name":"unsupported-no-raw","present":true,"on_path":true,"present_evidence":"path","path":"/PRIVATE/UNSUPPORTED","support":"unsupported"}`,
		store.SourceAgentMeasurement); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`INSERT INTO observed_state
 (observation_id,machine_id,measured_at,received_at,kind,subject,payload,source)
 VALUES(?,?,?,?,?,?,?,?)`, "operator-undecodable-journal", "machine-evidence",
		measuredAt.UTC().Format(time.RFC3339), receivedAt.UTC().Format(time.RFC3339),
		store.KindJournal, "undecodable.service", `{"unit":`, store.SourceAgentMeasurement); err != nil {
		t.Fatal(err)
	}

	result, err := New(st).MachineEvidence(MachineEvidenceRequest{MachineID: "machine-evidence"}, evaluatedAt)
	if err != nil {
		t.Fatal(err)
	}
	if MachineEvidenceSchemaVersion != 6 || result.SchemaVersion != MachineEvidenceSchemaVersion || result.MachineID != "machine-evidence" ||
		result.DisplayName != "evidence-machine" || result.EvaluatedAt.Location() != time.UTC {
		t.Fatalf("evidence envelope=%+v", result)
	}
	disclosure := result.Disclosure
	if disclosure.Limit != MachineEvidenceDefaultLimit || disclosure.MaxFieldBytes != MachineEvidenceMaxFieldBytes ||
		disclosure.MaxShapes != MachineEvidenceMaxShapes ||
		disclosure.MaxCredentialPeers != MachineEvidenceMaxCredentialPeers ||
		disclosure.MaxOccupancyAgents != MachineEvidenceMaxOccupancyAgents || disclosure.IndependentVerifier ||
		disclosure.StatusIsOutcome || disclosure.TerminalOutcomeRecorded ||
		disclosure.SystemdStateIsWorkOutcome || !disclosure.SystemdMainPIDExcluded ||
		!disclosure.HostPathFieldsExcluded || !disclosure.ProcessIDFieldsExcluded ||
		!disclosure.OpenClawDatabaseLocationsExcluded || disclosure.EvidenceTextPathRedactedByHub ||
		disclosure.CLIOpenClawTextRedactedByAgent || disclosure.CLIOpenClawTextRedactedByHub ||
		disclosure.CLIVersionSourcesCollapsed || disclosure.RawVersionTextParsedByHub ||
		!disclosure.RunningRelationshipDerivedFromPaths ||
		disclosure.CredentialStatusIsSessionValidity || disclosure.CredentialRemoteValidationPerformed ||
		!disclosure.CredentialActiveAccountIDExcluded || !disclosure.CredentialSecretFieldsExcluded ||
		disclosure.CredentialTextRedactedByAgent || disclosure.CredentialPeerIsSameTicket ||
		!disclosure.OccupancyAggregatedByHub || disclosure.OccupancyProcessStateUsed ||
		disclosure.OccupancyProviderNormalized || disclosure.OccupancyErrorsCategorized ||
		!disclosure.OccupancyProfileIDExcluded || !disclosure.OccupancyEventDetailsExcluded ||
		!disclosure.JournalSecretShapesRedactedByAgent || disclosure.RunSummarySecretShapesRedactedByAgent {
		t.Fatalf("disclosure=%+v", disclosure)
	}
	assertMachineEvidenceProducer(t, disclosure.JournalProducer, MachineEvidenceProducerObserverAgent,
		MachineEvidenceAuthorityMachineBearer, result)
	assertMachineEvidenceProducer(t, disclosure.SystemdProducer, MachineEvidenceProducerObserverAgent,
		MachineEvidenceAuthorityMachineBearer, result)
	assertMachineEvidenceProducer(t, disclosure.OpenClawProducer, MachineEvidenceProducerObserverAgent,
		MachineEvidenceAuthorityMachineBearer, result)
	assertMachineEvidenceProducer(t, disclosure.CLIToolProducer, MachineEvidenceProducerObserverAgent,
		MachineEvidenceAuthorityMachineBearer, result)
	assertMachineEvidenceProducer(t, disclosure.CredentialProducer, MachineEvidenceProducerObserverAgent,
		MachineEvidenceAuthorityMachineBearer, result)
	assertMachineEvidenceProducer(t, disclosure.OccupancyProducer, MachineEvidenceProducerOpenClawTask,
		"", result)
	assertMachineEvidenceProducer(t, disclosure.OccupancyRelay, MachineEvidenceProducerObserverAgent,
		MachineEvidenceAuthorityMachineBearer, result)
	assertMachineEvidenceProducer(t, disclosure.RunSummaryProducer, MachineEvidenceProducerOpenClawTask,
		"", result)
	assertMachineEvidenceProducer(t, disclosure.RunSummaryRelay, MachineEvidenceProducerObserverAgent,
		MachineEvidenceAuthorityMachineBearer, result)
	if result.SystemdUnits.Total != 4 || result.SystemdUnits.Truncated || result.SystemdUnits.Invalid != 0 ||
		len(result.SystemdUnits.Items) != 4 || !sort.SliceIsSorted(result.SystemdUnits.Items, func(i, j int) bool {
		return result.SystemdUnits.Items[i].Name.Text < result.SystemdUnits.Items[j].Name.Text
	}) {
		t.Fatalf("systemd units=%+v", result.SystemdUnits)
	}
	controlUnit := machineSystemdUnitByName(t, result.SystemdUnits.Items, "control.service")
	if !controlUnit.Present || !controlUnit.Measured || controlUnit.Reason == nil ||
		controlUnit.Reason.Text != "bus� failure" ||
		controlUnit.ActiveState.Text != "active�" || controlUnit.SubState.Text != "running�" ||
		controlUnit.NRestarts != 3 || controlUnit.ActiveEnterTimestamp == nil ||
		controlUnit.ActiveEnterTimestamp.Location() != time.UTC || controlUnit.MeasuredAt.Location() != time.UTC ||
		controlUnit.ReceivedAt.Location() != time.UTC ||
		strings.Join(controlUnit.ActiveState.Issues, ",") != boundedTextIssueControlOrFormatReplaced ||
		strings.Join(controlUnit.SubState.Issues, ",") != boundedTextIssueControlOrFormatReplaced ||
		strings.Join(controlUnit.Reason.Issues, ",") != boundedTextIssueControlOrFormatReplaced {
		t.Fatalf("control systemd unit=%+v", controlUnit)
	}
	if strings.Contains(fmt.Sprintf("%+v", result), "987654") {
		t.Fatal("operator machine evidence retained volatile MainPID")
	}
	if !result.OpenClaw.Observed || !result.OpenClaw.Decoded || !result.OpenClaw.Present ||
		result.OpenClaw.Reason == nil || !strings.Contains(result.OpenClaw.Reason.Text, "/FREE_TEXT/PATH") ||
		result.OpenClaw.CLIVersion == nil || result.OpenClaw.CLIVersion.Text != "2026.9.1" ||
		result.OpenClaw.CLIVersionRaw == nil || result.OpenClaw.Install == nil || result.OpenClaw.DB == nil ||
		result.OpenClaw.Install.DropInCount != 1 || result.OpenClaw.Install.ProcessMatchesUnit == nil ||
		*result.OpenClaw.Install.ProcessMatchesUnit || result.OpenClaw.DB.UnknownLocationCount != 1 ||
		len(result.OpenClaw.DB.TaskStatuses.Items) != 2 {
		t.Fatalf("typed OpenClaw=%+v", result.OpenClaw)
	}
	if result.CLITools.Total != 2 || result.CLITools.Truncated || result.CLITools.Invalid != 2 ||
		len(result.CLITools.Items) != 2 || result.CLITools.Items[0].Name.Text != "alpha" ||
		result.CLITools.Items[0].RunningRelationship != "different" ||
		result.CLITools.Items[1].VersionRaw == nil || result.CLITools.Items[1].VersionRaw.Text != "nightly\nraw" {
		t.Fatalf("typed CLI tools=%+v", result.CLITools)
	}
	if result.Credentials.Total != 1 || result.Credentials.Truncated || result.Credentials.Invalid != 0 ||
		len(result.Credentials.Items) != 1 {
		t.Fatalf("credentials=%+v", result.Credentials)
	}
	credential := result.Credentials.Items[0]
	if credential.Provider.Text != "codex" || credential.Status != model.CredConfigured ||
		credential.VerificationMethod != model.VerifyFileParse || !credential.ActiveAccountSelected ||
		credential.AccountCount != 3 || credential.LifetimeSeconds != int64((8*time.Hour)/time.Second) ||
		credential.MeasuredAt.Location() != time.UTC || credential.ReceivedAt.Location() != time.UTC ||
		credential.Note == nil || !credential.Note.Truncated || credential.LastError == nil ||
		credential.LastError.Text != "permission denied\nretry�[31m" || credential.Peers == nil {
		t.Fatalf("credential=%+v", credential)
	}
	if result.Occupancy.Total != 1 || result.Occupancy.Truncated || result.Occupancy.Invalid != 0 ||
		!result.Occupancy.Observed || !result.Occupancy.DBObserved || result.Occupancy.ObservationInvalid ||
		result.Occupancy.RowsSeen != 3 || result.Occupancy.RowsWithoutProvider != 1 ||
		result.Occupancy.ObservedAt == nil || len(result.Occupancy.Items) != 1 {
		t.Fatalf("occupancy=%+v", result.Occupancy)
	}
	occupancy := result.Occupancy.Items[0]
	if occupancy.Provider.Text != "openai" || occupancy.Source.Text != "cron_run_logs" || occupancy.Runs != 2 ||
		occupancy.Errors != 1 || occupancy.AgentsTotal != 2 || occupancy.AgentsTruncated ||
		len(occupancy.Agents) != 2 || occupancy.Agents[0].Text != "worker�" ||
		occupancy.LastRunAt.Location() != time.UTC || occupancy.LastReceivedAt.Location() != time.UTC {
		t.Fatalf("occupancy item=%+v", occupancy)
	}
	for _, forbidden := range []string{
		"PRIVATE_ACCOUNT_ID", "PRIVATE_JOB_A", "PRIVATE_JOB_B", "PRIVATE_SESSION_A", "PRIVATE_SESSION_B", "PRIVATE_RUN_ERROR",
		"/PRIVATE/UNIT", "/PRIVATE/DROPIN", "/PRIVATE/NODE", "/PRIVATE/INDEX", "/PRIVATE/NPM",
		"/PRIVATE/RUNNING", "PRIVATE_OWNER", "/PRIVATE/RELEASES", "/PRIVATE/CURRENT",
		"/PRIVATE/OPENCLAW_DB", "/PRIVATE/UNKNOWN_DB", "/PRIVATE/CLI_PATH", "/PRIVATE/CLI_REAL",
		"/PRIVATE/CLI_DAEMON", "/PRIVATE/CLI_RUNNING", "/PRIVATE/BRAVO_PATH", "/PRIVATE/UNSUPPORTED", "765432", "456789",
	} {
		if strings.Contains(fmt.Sprintf("%+v", result), forbidden) {
			t.Fatalf("operator machine evidence retained excluded detail %q", forbidden)
		}
	}

	if result.RunSummaries.Total != 3 || result.RunSummaries.Truncated ||
		!result.RunSummaries.Observed || !result.RunSummaries.DBObserved ||
		len(result.RunSummaries.Items) != 3 || result.RunSummaries.ObservedAt == nil ||
		result.RunSummaries.ObservedAt.MeasuredAt.Location() != time.UTC ||
		result.RunSummaries.ObservedAt.ReceivedAt.Location() != time.UTC ||
		!result.RunSummaries.ObservedAt.MeasuredAt.Equal(measuredAt) ||
		!result.RunSummaries.ObservedAt.ReceivedAt.Equal(receivedAt) {
		t.Fatalf("run summaries=%+v", result.RunSummaries)
	}
	newline := result.RunSummaries.Items[0]
	if newline.Summary.Text != "line one\nline two" || len(newline.Summary.Issues) != 0 ||
		newline.ReportedAt == nil || !newline.ReportedAt.Equal(batch.OpenClaw.DB.RecentSummaries[0].At) {
		t.Fatalf("newline summary=%+v", newline)
	}
	oversize := result.RunSummaries.Items[1]
	if oversize.ReportedAt != nil || !oversize.Summary.Truncated ||
		oversize.Summary.Bytes <= len(oversize.Summary.Text) ||
		strings.Join(oversize.Summary.Issues, ",") != boundedTextIssueTruncated {
		t.Fatalf("oversize summary=%+v text_bytes=%d", oversize, len(oversize.Summary.Text))
	}
	controlHeavy := result.RunSummaries.Items[2].Summary
	if !controlHeavy.Truncated || controlHeavy.Bytes >= len(controlHeavy.Text) ||
		strings.Join(controlHeavy.Issues, ",") != "control_or_format_replaced,truncated" {
		t.Fatalf("control-heavy summary=%+v text_bytes=%d", controlHeavy, len(controlHeavy.Text))
	}

	if result.Journals.Total != 2 || result.Journals.Truncated || result.Journals.Undecodable != 1 ||
		len(result.Journals.Items) != 2 || result.Journals.UnitsWithoutJournalTotal != 1 ||
		len(result.Journals.UnitsWithoutJournal) != 1 ||
		result.Journals.UnitsWithoutJournal[0].Text != "missing.service" ||
		result.Journals.UnitsWithoutJournalTruncated {
		t.Fatalf("journals=%+v", result.Journals)
	}
	if !sort.SliceIsSorted(result.Journals.Items, func(i, j int) bool {
		return result.Journals.Items[i].Unit.Text < result.Journals.Items[j].Unit.Text
	}) {
		t.Fatalf("journal items are not sorted: %+v", result.Journals.Items)
	}
	control := machineJournalByUnit(t, result.Journals.Items, "control.service")
	if !control.ReadFailed || control.Err == nil || control.Err.Text != "�" ||
		strings.Join(control.Err.Issues, ",") != boundedTextIssueControlOrFormatReplaced {
		t.Fatalf("control-only raw error did not report read failure before scrubbing: %+v", control)
	}
	example := machineJournalByUnit(t, result.Journals.Items, "example.service")
	if example.ReadFailed || example.Err != nil || example.WindowSeconds != 3600 || example.Lines != 12 ||
		!example.LinesAreLowerBound || example.Shapes != 11 || len(example.Top) != MachineEvidenceMaxShapes ||
		!example.TopTruncated || example.MeasuredAt.Location() != time.UTC || example.ReceivedAt.Location() != time.UTC {
		t.Fatalf("example journal=%+v", example)
	}
	if example.Top[0].Example.Text != "before�[31m�after" || example.Top[0].Example.Truncated ||
		strings.Join(example.Top[0].Example.Issues, ",") != boundedTextIssueControlOrFormatReplaced {
		t.Fatalf("journal escape/CR example=%+v", example.Top[0].Example)
	}
	assertEvidenceText(t, example.Unit, 256, len("example.service"), false, nil)
}

func TestMachineEvidenceLimitAndRequestValidation(t *testing.T) {
	st := newOperatorJobReadStore(t)
	now := time.Date(2026, 9, 9, 16, 0, 0, 0, time.UTC)
	if err := st.UpsertMachine(store.Machine{
		MachineID: "machine-limit", DisplayName: "limit-machine", CreatedAt: now.Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	batch := model.ObservationBatch{
		SchemaVersion: model.SchemaVersion, MeasuredAt: now.Add(-time.Minute),
		Systemd:  []model.Unit{{Name: "a.service"}, {Name: "b.service"}},
		Journals: []model.UnitJournal{{Unit: "a.service"}, {Unit: "b.service"}},
		OpenClaw: model.OpenClaw{DB: &model.OpenClawDB{RecentSummaries: []model.RunSummary{
			{JobID: "one"}, {JobID: "two"},
		}, TaskStatusCount: map[string]int{"alpha": 1, "bravo": 1}}},
		CLITools: []model.CLITool{{Name: "alpha"}, {Name: "bravo"}},
	}
	if err := st.RecordObservation("machine-limit", batch, now.Add(-30*time.Second)); err != nil {
		t.Fatal(err)
	}
	result, err := New(st).MachineEvidence(MachineEvidenceRequest{MachineID: "machine-limit", Limit: 1}, now)
	if err != nil || result.Disclosure.Limit != 1 || result.SystemdUnits.Total != 2 ||
		result.CLITools.Total != 2 || len(result.CLITools.Items) != 1 || !result.CLITools.Truncated ||
		result.OpenClaw.DB == nil || result.OpenClaw.DB.TaskStatuses.Total != 2 ||
		len(result.OpenClaw.DB.TaskStatuses.Items) != 1 || !result.OpenClaw.DB.TaskStatuses.Truncated ||
		len(result.SystemdUnits.Items) != 1 || !result.SystemdUnits.Truncated ||
		result.RunSummaries.Total != 2 ||
		len(result.RunSummaries.Items) != 1 || !result.RunSummaries.Truncated ||
		result.Journals.Total != 2 || len(result.Journals.Items) != 1 || !result.Journals.Truncated {
		t.Fatalf("limited result=%+v err=%v", result, err)
	}
	for _, request := range []MachineEvidenceRequest{
		{},
		{MachineID: " machine-limit"},
		{MachineID: "machine\nlimit"},
		{MachineID: string([]byte{'m', 0xff})},
		{MachineID: strings.Repeat("m", 257)},
		{MachineID: "machine-limit", Limit: -1},
		{MachineID: "machine-limit", Limit: store.MaxMachineEvidencePageSize + 1},
	} {
		if err := ValidateMachineEvidenceRequest(request); !errors.Is(err, ErrInvalidMachineRead) {
			t.Errorf("validation request=%+v error=%v want ErrInvalidMachineRead", request, err)
		}
		if _, err := New(st).MachineEvidence(request, now); !errors.Is(err, ErrInvalidMachineRead) {
			t.Errorf("service request=%+v error=%v want ErrInvalidMachineRead", request, err)
		}
	}
	if _, err := New(st).MachineEvidence(MachineEvidenceRequest{MachineID: "missing"}, now); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing machine error=%v want store.ErrNotFound", err)
	}
}

// TestMachineEvidenceReportsUnitsWithoutJournalTruncationOnItsOwn 守住這份清單本身就是
// Hub 沒有在觀測的東西；它被截斷卻不說，等於把一份不完整的漏網名單當成完整的交出去。
// 這個旗標會一路走到 Web 的裝置頁與 CLI 的 machines evidence 輸出。既有的 limit 測試
// 給每個 unit 都配了 journal，所以這份清單永遠是空的；它截斷了六個分頁，就是截不到
// 這一個。這支測試刻意讓 Journals 那一份不截斷，只讓這一份截斷，因為兩個旗標接在
// 一起的錯誤，只有在它們該不一樣的時候才看得見。
// 這支釘不到排序本身：把 store 那邊的 sort.Strings 拿掉，全樹仍然全綠，因為來源順序
// 剛好已經是排好的；這裡斷言的是分頁取的是清單開頭，不是排序。
// Web 裝置頁上那一行「未收集 journal 的 unit 超過顯示上限」是另一層，不在這支測試的
// 範圍內。
func TestMachineEvidenceReportsUnitsWithoutJournalTruncationOnItsOwn(t *testing.T) {
	st := newOperatorJobReadStore(t)
	now := time.Date(2026, 9, 9, 16, 15, 0, 0, time.UTC)
	if err := st.UpsertMachine(store.Machine{
		MachineID: "machine-unjournaled", DisplayName: "unjournaled-machine",
		CreatedAt: now.Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	batch := model.ObservationBatch{
		SchemaVersion: model.SchemaVersion,
		MeasuredAt:    now.Add(-time.Minute),
		Systemd: []model.Unit{
			{Name: "a.service"},
			{Name: "b.service"},
			{Name: "c.service"},
		},
		Journals: []model.UnitJournal{{Unit: "a.service"}},
	}
	if err := st.RecordObservation("machine-unjournaled", batch, now.Add(-30*time.Second)); err != nil {
		t.Fatal(err)
	}

	result, err := New(st).MachineEvidence(MachineEvidenceRequest{
		MachineID: "machine-unjournaled",
		Limit:     1,
	}, now)
	if err != nil {
		t.Fatalf("取得機器證據時不應失敗：%v；journals=%+v", err, result.Journals)
	}
	if result.Journals.Truncated {
		t.Fatalf("journal 清單只有一筆，不應被標成已截斷：%+v", result.Journals)
	}
	if !result.Journals.UnitsWithoutJournalTruncated {
		t.Fatalf("沒收到 journal 的 unit 被截掉了卻沒有說，操作員會以為畫面上那份「沒在觀測的 unit」清單是完整的：%+v", result.Journals)
	}
	if result.Journals.UnitsWithoutJournalTotal != 2 ||
		len(result.Journals.UnitsWithoutJournal) != 1 ||
		result.Journals.UnitsWithoutJournal[0].Text != "b.service" {
		t.Fatalf("沒收到 journal 的 unit 分頁不是排序清單的開頭，操作員無從知道自己看到的是哪一批：%+v", result.Journals)
	}
}

func TestMachineEvidenceCarriesObservationWithoutDB(t *testing.T) {
	st := newOperatorJobReadStore(t)
	now := time.Date(2026, 9, 9, 16, 30, 0, 0, time.UTC)
	if err := st.UpsertMachine(store.Machine{
		MachineID: "machine-no-db", DisplayName: "no-db", CreatedAt: now.Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordObservation("machine-no-db", model.ObservationBatch{
		SchemaVersion: model.SchemaVersion, MeasuredAt: now.Add(-time.Minute),
		OpenClaw: model.OpenClaw{Present: true},
	}, now.Add(-30*time.Second)); err != nil {
		t.Fatal(err)
	}
	result, err := New(st).MachineEvidence(MachineEvidenceRequest{MachineID: "machine-no-db"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if !result.RunSummaries.Observed || result.RunSummaries.DBObserved ||
		result.RunSummaries.ObservedAt == nil || result.RunSummaries.Total != 0 ||
		len(result.RunSummaries.Items) != 0 {
		t.Fatalf("run summary observation without DB=%+v", result.RunSummaries)
	}
}

func TestMachineEvidenceSeparatesInvalidOpenClawSubsections(t *testing.T) {
	st := newOperatorJobReadStore(t)
	now := time.Date(2026, 9, 9, 16, 45, 0, 0, time.UTC)
	if err := st.UpsertMachine(store.Machine{
		MachineID: "machine-invalid-openclaw", DisplayName: "invalid-openclaw", CreatedAt: now.Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	negative := -1
	if err := st.RecordObservation("machine-invalid-openclaw", model.ObservationBatch{
		SchemaVersion: model.SchemaVersion, MeasuredAt: now.Add(-time.Minute),
		OpenClaw: model.OpenClaw{Present: true,
			Install: &model.OpenClawInstall{NRestarts: &negative},
			DB:      &model.OpenClawDB{TaskRunRows: -1}},
	}, now.Add(-30*time.Second)); err != nil {
		t.Fatal(err)
	}
	result, err := New(st).MachineEvidence(MachineEvidenceRequest{MachineID: "machine-invalid-openclaw"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if !result.OpenClaw.Observed || !result.OpenClaw.Decoded || !result.OpenClaw.Present ||
		!result.OpenClaw.InstallInvalid || result.OpenClaw.Install != nil ||
		!result.OpenClaw.DBInvalid || result.OpenClaw.DB != nil {
		t.Fatalf("invalid OpenClaw subsections=%+v", result.OpenClaw)
	}
}

func TestMachineEvidenceProjectsCredentialGraceStatus(t *testing.T) {
	st := newOperatorJobReadStore(t)
	now := time.Date(2026, 9, 16, 12, 0, 0, 500, time.UTC)
	if err := st.UpsertMachine(store.Machine{
		MachineID: "credential-grace", DisplayName: "credential-grace", CreatedAt: now.Add(-24 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	expires := func(overdue time.Duration) *time.Time {
		value := now.Add(-overdue)
		return &value
	}
	mtime := func(expiry *time.Time, lifetime time.Duration) *time.Time {
		value := expiry.Add(-lifetime)
		return &value
	}
	inGraceExpiry := expires(2 * time.Hour)
	outOfGraceExpiry := expires(8 * time.Hour)
	unknownLifetimeExpiry := expires(time.Hour)
	configuredExpiry := now.Add(8 * time.Hour)
	cappedExpiry := expires(24 * time.Hour)
	credentials := []model.Credential{
		{Provider: "in-grace", Status: model.CredExpired, ExpiresAt: inGraceExpiry, FileMTime: mtime(inGraceExpiry, 8*time.Hour)},
		{Provider: "out-of-grace", Status: model.CredExpired, ExpiresAt: outOfGraceExpiry, FileMTime: mtime(outOfGraceExpiry, 8*time.Hour)},
		{Provider: "no-lifetime", Status: model.CredExpired, ExpiresAt: unknownLifetimeExpiry},
		{Provider: "configured", Status: model.CredConfigured, ExpiresAt: &configuredExpiry},
		{Provider: "capped", Status: model.CredExpired, ExpiresAt: cappedExpiry, FileMTime: mtime(cappedExpiry, 10*24*time.Hour)},
	}
	if err := st.RecordObservation("credential-grace", model.ObservationBatch{
		SchemaVersion: model.SchemaVersion, MeasuredAt: now.Add(-time.Minute), Credentials: credentials,
	}, now.Add(-30*time.Second)); err != nil {
		t.Fatal(err)
	}
	result, err := New(st).MachineEvidence(MachineEvidenceRequest{MachineID: "credential-grace"}, now)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]MachineCredentialGraceStatus{
		"in-grace": MachineCredentialGraceInGrace, "out-of-grace": MachineCredentialGraceExceeded,
		"no-lifetime": MachineCredentialGraceExceeded, "configured": MachineCredentialGraceNotApplicable,
		"capped": MachineCredentialGraceExceeded,
	}
	if len(result.Credentials.Items) != len(want) {
		t.Fatalf("credentials=%+v", result.Credentials)
	}
	for _, credential := range result.Credentials.Items {
		if credential.GraceStatus != want[credential.Provider.Text] {
			t.Errorf("%s grace status = %q, want %q", credential.Provider.Text, credential.GraceStatus, want[credential.Provider.Text])
		}
	}
}

func repeatedJournalShapes(count int, prefix string) []model.JournalShape {
	shapes := make([]model.JournalShape, 0, count)
	for i := 0; i < count; i++ {
		shapes = append(shapes, model.JournalShape{Count: i + 1, Example: prefix})
	}
	return shapes
}

func machineJournalByUnit(t *testing.T, journals []MachineJournal, unit string) MachineJournal {
	t.Helper()
	for _, journal := range journals {
		if journal.Unit.Text == unit {
			return journal
		}
	}
	t.Fatalf("journal %q absent from %+v", unit, journals)
	return MachineJournal{}
}

func machineSystemdUnitByName(t *testing.T, units []MachineSystemdUnit, name string) MachineSystemdUnit {
	t.Helper()
	for _, unit := range units {
		if unit.Name.Text == name {
			return unit
		}
	}
	t.Fatalf("systemd unit %q absent from %+v", name, units)
	return MachineSystemdUnit{}
}

func machineEvidenceTimePtr(value time.Time) *time.Time { return &value }

func assertMachineEvidenceProducer(t *testing.T, got MachineEvidenceProducer, kind, authority string,
	result MachineEvidenceResult,
) {
	t.Helper()
	if got.Kind != kind || got.Authority != authority || got.MachineID != result.MachineID ||
		got.DisplayName != result.DisplayName {
		t.Fatalf("producer=%+v want kind=%q authority=%q machine=%q display=%q", got, kind, authority,
			result.MachineID, result.DisplayName)
	}
}
