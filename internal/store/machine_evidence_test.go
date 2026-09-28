package store

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/model"
)

func TestMachineReadEvidenceRejectsValuesOutsideClosedDomains(t *testing.T) {
	type evidenceKind string
	const (
		cliToolKind    evidenceKind = "cli_tool"
		credentialKind evidenceKind = "credential"
		openClawDBKind evidenceKind = "openclaw_db"
	)
	type testCase struct {
		name   string
		kind   evidenceKind
		valid  bool
		mutate func(*model.ObservationBatch)
	}

	var cases []testCase
	addCLIToolCases := func(rule string, values []string, set func(*model.CLITool, string)) {
		cases = append(cases, testCase{
			name: "reject_cli_tool_" + rule, kind: cliToolKind,
			mutate: func(batch *model.ObservationBatch) { set(&batch.CLITools[0], "outside-domain") },
		})
		for _, value := range values {
			value := value
			cases = append(cases, testCase{
				name: "accept_cli_tool_" + rule + "_" + fmt.Sprintf("%q", value),
				kind: cliToolKind, valid: true,
				mutate: func(batch *model.ObservationBatch) { set(&batch.CLITools[0], value) },
			})
		}
	}
	addCredentialCases := func(rule string, values []string, set func(*model.Credential, string)) {
		cases = append(cases, testCase{
			name: "reject_credential_" + rule, kind: credentialKind,
			mutate: func(batch *model.ObservationBatch) { set(&batch.Credentials[0], "outside-domain") },
		})
		for _, value := range values {
			value := value
			cases = append(cases, testCase{
				name: "accept_credential_" + rule + "_" + fmt.Sprintf("%q", value),
				kind: credentialKind, valid: true,
				mutate: func(batch *model.ObservationBatch) { set(&batch.Credentials[0], value) },
			})
		}
	}
	addOpenClawDBCases := func(rule string, values []string, set func(*model.OpenClawDB, string)) {
		cases = append(cases, testCase{
			name: "reject_openclaw_db_" + rule, kind: openClawDBKind,
			mutate: func(batch *model.ObservationBatch) { set(batch.OpenClaw.DB, "outside-domain") },
		})
		for _, value := range values {
			value := value
			cases = append(cases, testCase{
				name: "accept_openclaw_db_" + rule + "_" + fmt.Sprintf("%q", value),
				kind: openClawDBKind, valid: true,
				mutate: func(batch *model.ObservationBatch) { set(batch.OpenClaw.DB, value) },
			})
		}
	}

	addCLIToolCases("present_evidence", []string{"", "path", "process"},
		func(tool *model.CLITool, value string) {
			tool.PresentEvidence = value
			if value == "path" {
				tool.OnPath = true
			} else if value == "process" {
				tool.OnPath = false
			}
		})
	addCLIToolCases("path_source", []string{"", model.PathSourceLogin, model.PathSourceDaemon},
		func(tool *model.CLITool, value string) { tool.PathSource = value })
	addCLIToolCases("daemon_reach",
		[]string{"", model.DaemonReachSame, model.DaemonReachShadowed, model.DaemonReachMissing},
		func(tool *model.CLITool, value string) { tool.DaemonReach = value })
	addCLIToolCases("process_scan",
		[]string{"", model.ProcessScanComplete, model.ProcessScanRestricted, model.ProcessScanUnavailable},
		func(tool *model.CLITool, value string) { tool.ProcessScan = value })
	addCLIToolCases("support", []string{"", string(model.SupportOK), string(model.SupportUnsupported)},
		func(tool *model.CLITool, value string) {
			tool.Support = model.SupportLevel(value)
			if tool.Support == model.SupportUnsupported {
				tool.VersionRaw = "unrecognized version output"
			}
		})

	addCredentialCases("status", []string{
		string(model.CredAbsent), string(model.CredConfigured), string(model.CredExpiresSoon),
		string(model.CredExpired), string(model.CredUnknown), string(model.CredFailed),
	}, func(credential *model.Credential, value string) { credential.Status = model.CredStatus(value) })
	addCredentialCases("verification_method",
		[]string{"", string(model.VerifyFileParse), string(model.VerifyLiveRequest)},
		func(credential *model.Credential, value string) {
			credential.VerificationMethod = model.VerifyMethod(value)
			credential.VerifiedAt = nil
			if credential.VerificationMethod == model.VerifyLiveRequest {
				verifiedAt := time.Date(2026, 9, 9, 11, 58, 0, 0, time.UTC)
				credential.VerifiedAt = &verifiedAt
			}
		})

	addOpenClawDBCases("support", []string{"", string(model.SupportOK), string(model.SupportUnsupported)},
		func(db *model.OpenClawDB, value string) {
			db.Support = model.SupportLevel(value)
			if db.Support == model.SupportUnsupported {
				db.Present = false
				db.FoundAt = []string{"~/.openclaw/unknown.sqlite"}
			}
		})
	addOpenClawDBCases("layout", []string{"", "consolidated", "split"},
		func(db *model.OpenClawDB, value string) { db.Layout = value })

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newTestStore(t)
			now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
			machineID := mustEnroll(t, st, "closed-domain", now.Add(-time.Hour))
			batch := healthyBatch(now.Add(-time.Minute))
			tc.mutate(&batch)
			if err := st.RecordObservation(machineID, batch, now.Add(-30*time.Second)); err != nil {
				t.Fatal(err)
			}
			evidence, err := st.MachineReadEvidence(machineID, now, 10)
			if err != nil {
				t.Fatal(err)
			}

			switch tc.kind {
			case cliToolKind:
				wantInvalid, wantRows := 1, 0
				if tc.valid {
					wantInvalid, wantRows = 0, 1
				}
				if evidence.CLIToolsInvalid != wantInvalid || len(evidence.CLITools) != wantRows {
					t.Fatalf("CLI tools invalid=%d rows=%d; want %d/%d", evidence.CLIToolsInvalid,
						len(evidence.CLITools), wantInvalid, wantRows)
				}
			case credentialKind:
				wantInvalid, wantRows := 1, 0
				if tc.valid {
					wantInvalid, wantRows = 0, 1
				}
				if evidence.CredentialsInvalid != wantInvalid || len(evidence.Credentials) != wantRows {
					t.Fatalf("credentials invalid=%d rows=%d; want %d/%d", evidence.CredentialsInvalid,
						len(evidence.Credentials), wantInvalid, wantRows)
				}
			case openClawDBKind:
				if !evidence.OpenClawDBObserved {
					t.Fatal("raw OpenClaw DB observation was not recorded")
				}
				if tc.valid {
					if evidence.OpenClaw.DBInvalid || evidence.OpenClaw.DB == nil {
						t.Fatalf("valid OpenClaw DB was rejected: %+v", evidence.OpenClaw)
					}
				} else if !evidence.OpenClaw.DBInvalid || evidence.OpenClaw.DB != nil {
					t.Fatalf("invalid OpenClaw DB was retained: %+v", evidence.OpenClaw)
				}
			}
		})
	}
}

func TestMachineReadEvidenceBoundsRawTextAndStatesMissingJournal(t *testing.T) {
	st := newTestStore(t)
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	machineID := mustEnroll(t, st, "machine-evidence", now.Add(-time.Hour))
	batch := healthyBatch(now.Add(-time.Minute))
	batch.OpenClaw.DB.RecentSummaries = []model.RunSummary{
		{JobID: "job-z", At: now.Add(-time.Hour), Status: "ok", Summary: "first\nsummary"},
		{JobID: "job-a", At: now.Add(-2 * time.Hour), Status: "failed", Summary: "second summary"},
		{JobID: "job-m", At: now.Add(-3 * time.Hour), Status: "custom", Summary: "third summary"},
	}
	batch.Systemd = []model.Unit{
		{Name: "alpha.service", Present: true, ActiveState: "active", SubState: "running",
			ActiveEnterTimestamp: ptrTime(now.Add(-10 * time.Minute)), NRestarts: 2, MainPID: 987654},
		{Name: "bravo.service", Present: true},
		{Name: "charlie.service", Present: true},
		{Name: "missing.service", Present: true},
	}
	batch.Journals = []model.UnitJournal{
		{Unit: "charlie.service", Lines: 3, Top: []model.JournalShape{{Count: 3, Example: "charlie raw"}}},
		{Unit: "alpha.service", Lines: 1, Top: []model.JournalShape{{Count: 1, Example: "alpha raw"}}},
		{Unit: "bravo.service", Lines: 2, Top: []model.JournalShape{{Count: 2, Example: "bravo raw"}}},
	}
	receivedAt := now.Add(-30 * time.Second)
	if err := st.RecordObservation(machineID, batch, receivedAt); err != nil {
		t.Fatal(err)
	}

	evidence, err := st.MachineReadEvidence(machineID, now, 2)
	if err != nil {
		t.Fatal(err)
	}
	if evidence.MachineID != machineID || evidence.DisplayName != "machine-evidence" {
		t.Fatalf("machine identity=%q/%q", evidence.MachineID, evidence.DisplayName)
	}
	if evidence.RunSummariesTotal != 3 || len(evidence.RunSummaries) != 2 || !evidence.RunSummariesTruncated {
		t.Fatalf("run summaries total=%d len=%d truncated=%v", evidence.RunSummariesTotal,
			len(evidence.RunSummaries), evidence.RunSummariesTruncated)
	}
	if evidence.RunSummaries[0].JobID != "job-z" || evidence.RunSummaries[1].JobID != "job-a" ||
		evidence.RunSummaries[0].Summary != "first\nsummary" || evidence.RunSummaries[1].Status != "failed" {
		t.Fatalf("run summaries changed or reordered: %+v", evidence.RunSummaries)
	}
	if evidence.RunSummaryObservedAt == nil ||
		!evidence.OpenClawObserved || !evidence.OpenClawDBObserved ||
		!evidence.RunSummaryObservedAt.MeasuredAt.Equal(batch.MeasuredAt) ||
		!evidence.RunSummaryObservedAt.ReceivedAt.Equal(receivedAt) {
		t.Fatalf("run summary observation clock=%+v", evidence.RunSummaryObservedAt)
	}
	if evidence.JournalsTotal != 3 || len(evidence.Journals) != 2 || !evidence.JournalsTruncated {
		t.Fatalf("journals total=%d len=%d truncated=%v", evidence.JournalsTotal,
			len(evidence.Journals), evidence.JournalsTruncated)
	}
	if evidence.SystemdUnitsTotal != 4 || len(evidence.SystemdUnits) != 2 ||
		!evidence.SystemdUnitsTruncated || evidence.SystemdUnitsInvalid != 0 ||
		evidence.SystemdUnits[0].Name != "alpha.service" || evidence.SystemdUnits[1].Name != "bravo.service" ||
		evidence.SystemdUnits[0].NRestarts != 2 || evidence.SystemdUnits[0].ActiveEnterTimestamp == nil ||
		!evidence.SystemdUnits[0].MeasuredAt.Equal(batch.MeasuredAt) ||
		!evidence.SystemdUnits[0].ReceivedAt.Equal(receivedAt) {
		t.Fatalf("typed systemd units=%+v total=%d invalid=%d truncated=%v", evidence.SystemdUnits,
			evidence.SystemdUnitsTotal, evidence.SystemdUnitsInvalid, evidence.SystemdUnitsTruncated)
	}
	if strings.Contains(fmt.Sprintf("%+v", evidence), "987654") {
		t.Fatal("typed machine evidence retained volatile MainPID")
	}
	if evidence.Journals[0].Unit != "alpha.service" || evidence.Journals[0].Top[0].Example != "alpha raw" ||
		evidence.Journals[1].Unit != "bravo.service" {
		t.Fatalf("journals not sorted or verbatim: %+v", evidence.Journals)
	}
	if evidence.UnitsWithoutJournalTotal != 1 || evidence.UnitsWithoutJournalTruncated ||
		len(evidence.UnitsWithoutJournal) != 1 || evidence.UnitsWithoutJournal[0] != "missing.service" {
		t.Fatalf("units without journal=%+v total=%d truncated=%v", evidence.UnitsWithoutJournal,
			evidence.UnitsWithoutJournalTotal, evidence.UnitsWithoutJournalTruncated)
	}
	for _, unit := range evidence.UnitsWithoutJournal {
		if unit == "charlie.service" {
			t.Fatal("journal dropped by the page limit was reported as not collected")
		}
	}
}

func TestMachineReadEvidenceDistinguishesOpenClawObservationWithoutDB(t *testing.T) {
	st := newTestStore(t)
	now := time.Date(2026, 9, 9, 12, 30, 0, 0, time.UTC)
	machineID := mustEnroll(t, st, "openclaw-no-db", now.Add(-time.Hour))
	batch := healthyBatch(now.Add(-time.Minute))
	batch.OpenClaw.DB = nil
	receivedAt := now.Add(-30 * time.Second)
	if err := st.RecordObservation(machineID, batch, receivedAt); err != nil {
		t.Fatal(err)
	}
	evidence, err := st.MachineReadEvidence(machineID, now, 10)
	if err != nil {
		t.Fatal(err)
	}
	if !evidence.OpenClawObserved || evidence.OpenClawDBObserved || evidence.RunSummaryObservedAt == nil ||
		!evidence.RunSummaryObservedAt.MeasuredAt.Equal(batch.MeasuredAt) ||
		!evidence.RunSummaryObservedAt.ReceivedAt.Equal(receivedAt) || evidence.RunSummariesTotal != 0 {
		t.Fatalf("openclaw observation state=%+v", evidence)
	}
}

func TestMachineReadEvidenceBoundsCredentialsAndOccupancyWithoutSecrets(t *testing.T) {
	st := newTestStore(t)
	now := time.Date(2026, 9, 9, 13, 30, 0, 0, time.UTC)
	machineID := mustEnroll(t, st, "credential-target", now.Add(-24*time.Hour))
	peerID := mustEnroll(t, st, "credential-peer", now.Add(-24*time.Hour))

	for i, age := range []time.Duration{3 * time.Hour, time.Hour} {
		mtime := now.Add(-age)
		expires := mtime.Add(8 * time.Hour)
		peerBatch := healthyBatch(now.Add(-age + time.Minute))
		peerBatch.Credentials = []model.Credential{{
			Provider: "alpha", Status: model.CredConfigured, ExpiresAt: &expires, FileMTime: &mtime,
		}}
		if err := st.RecordObservation(peerID, peerBatch, now.Add(-age+time.Duration(i+2)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}

	mtime := now.Add(-2 * time.Hour)
	expires := now.Add(6 * time.Hour)
	targetBatch := healthyBatch(now.Add(-time.Minute))
	targetBatch.Credentials = []model.Credential{
		{Provider: "zulu", Status: model.CredAbsent},
		{Provider: "alpha", Status: model.CredConfigured, ExpiresAt: &expires, FileMTime: &mtime,
			VerificationMethod: model.VerifyFileParse, ActiveAccountID: "PRIVATE_ACCOUNT_ID", AccountCount: 3,
			Note: "operator note", LastError: "operator error"},
	}
	targetBatch.OpenClaw.DB.OccupancyRowsSeen = 4
	targetBatch.OpenClaw.DB.OccupancyRowsNoProvider = 1
	targetBatch.OpenClaw.DB.Occupancy = []model.OccupancyEvidence{
		{Source: "cron_run_logs", JobID: "SECRET_JOB_A", At: now.Add(-3 * time.Minute),
			Provider: "z-provider", AgentID: "recent-agent", SessionKey: "PRIVATE_SESSION_A", ErrorText: "PRIVATE_ERROR_A"},
		{Source: "cron_run_logs", JobID: "SECRET_JOB_B", At: now.Add(-2 * time.Minute),
			Provider: "a-provider", AgentID: "first-agent", SessionKey: "PRIVATE_SESSION_B"},
		{Source: "cron_runs_jsonl", JobID: "SECRET_JOB_C", At: now.Add(-time.Minute),
			Provider: "a-provider", AgentID: "second-agent", SessionKey: "PRIVATE_SESSION_C"},
	}
	receivedAt := now.Add(-30 * time.Second)
	if err := st.RecordObservation(machineID, targetBatch, receivedAt); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`INSERT INTO observed_state
 (observation_id,machine_id,measured_at,received_at,kind,subject,payload,source)
 VALUES(?,?,?,?,?,?,?,?)`, "invalid-credential", machineID, fmtTime(now.Add(-time.Minute)),
		fmtTime(receivedAt), KindCredential, "broken", `{"provider":"other","status":"configured"}`,
		SourceAgentMeasurement); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`INSERT INTO ticket_occupancy_observation
 (observation_id,machine_id,provider,measured_at,received_at,occupant_evidence,process_alive,source)
 VALUES(?,?,?,?,?,?,0,?)`, "invalid-occupancy", machineID, "", fmtTime(now.Add(-time.Minute)),
		fmtTime(receivedAt), "PRIVATE_INVALID_SESSION", ""); err != nil {
		t.Fatal(err)
	}

	evidence, err := st.MachineReadEvidence(machineID, now, 2)
	if err != nil {
		t.Fatal(err)
	}
	if evidence.CredentialsTotal != 2 || evidence.CredentialsTruncated || evidence.CredentialsInvalid != 1 ||
		len(evidence.Credentials) != 2 || evidence.Credentials[0].Provider != "alpha" ||
		evidence.Credentials[1].Provider != "zulu" {
		t.Fatalf("credential page=%+v total=%d invalid=%d truncated=%v", evidence.Credentials,
			evidence.CredentialsTotal, evidence.CredentialsInvalid, evidence.CredentialsTruncated)
	}
	credential := evidence.Credentials[0]
	if !credential.ActiveAccountSelected || credential.AccountCount != 3 || credential.Note != "operator note" ||
		credential.LastError != "operator error" || credential.PeersTotal != 1 || credential.PeersTruncated ||
		len(credential.Peers) != 1 || credential.Peers[0].DisplayName != "credential-peer" ||
		credential.Peers[0].RefreshesSeen != 1 {
		t.Fatalf("credential detail=%+v", credential)
	}
	if evidence.OccupancyTotal != 3 || !evidence.OccupancyTruncated || evidence.OccupancyInvalid != 1 ||
		len(evidence.Occupancy) != 2 || evidence.Occupancy[0].Provider != "a-provider" ||
		evidence.Occupancy[0].Source != "cron_run_logs" || evidence.Occupancy[1].Source != "cron_runs_jsonl" ||
		!evidence.OccupancyObserved || !evidence.OccupancyDBObserved || evidence.OccupancyObservedAt == nil ||
		evidence.OccupancyRowsSeen != 4 || evidence.OccupancyRowsSkipped != 1 {
		t.Fatalf("occupancy page=%+v envelope=%+v", evidence.Occupancy, evidence)
	}
	printed := fmt.Sprintf("%+v", evidence)
	for _, forbidden := range []string{
		"PRIVATE_ACCOUNT_ID", "PRIVATE_SESSION_A", "PRIVATE_SESSION_B", "PRIVATE_SESSION_C",
		"PRIVATE_INVALID_SESSION", "SECRET_JOB_A", "SECRET_JOB_B", "SECRET_JOB_C", "PRIVATE_ERROR_A",
	} {
		if strings.Contains(printed, forbidden) {
			t.Fatalf("typed machine evidence retained excluded detail %q", forbidden)
		}
	}
}

func TestMachineReadEvidenceCountsUndecodableJournalWithoutCallingItUncollected(t *testing.T) {
	st := newTestStore(t)
	now := time.Date(2026, 9, 9, 12, 45, 0, 0, time.UTC)
	machineID := mustEnroll(t, st, "undecodable-journal", now.Add(-time.Hour))
	batch := healthyBatch(now.Add(-2 * time.Minute))
	batch.Systemd = []model.Unit{{Name: "broken.service", Present: true}}
	batch.Journals = nil
	if err := st.RecordObservation(machineID, batch, now.Add(-90*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`INSERT INTO observed_state
 (observation_id,machine_id,measured_at,received_at,kind,subject,payload,source)
 VALUES(?,?,?,?,?,?,?,?)`, "invalid-journal-observation", machineID, fmtTime(now.Add(-time.Minute)),
		fmtTime(now.Add(-30*time.Second)), KindJournal, "broken.service", `{"unit":`, SourceAgentMeasurement); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`INSERT INTO observed_state
 (observation_id,machine_id,measured_at,received_at,kind,subject,payload,source)
 VALUES(?,?,?,?,?,?,?,?)`, "invalid-systemd-observation", machineID, fmtTime(now.Add(-time.Minute)),
		fmtTime(now.Add(-20*time.Second)), KindSystemd, "broken.service",
		`{"name":"other.service","present":true}`, SourceAgentMeasurement); err != nil {
		t.Fatal(err)
	}
	evidence, err := st.MachineReadEvidence(machineID, now, 10)
	if err != nil {
		t.Fatal(err)
	}
	if evidence.JournalsUndecodable != 1 || len(evidence.Journals) != 0 ||
		evidence.SystemdUnitsInvalid != 1 || len(evidence.SystemdUnits) != 0 ||
		len(evidence.UnitsWithoutJournal) != 0 {
		t.Fatalf("undecodable journal evidence=%+v", evidence)
	}
}

func TestMachineReadEvidenceCannotCarryEnrollmentTokenOrCredentialSecrets(t *testing.T) {
	st := newTestStore(t)
	now := time.Date(2026, 9, 9, 13, 0, 0, 0, time.UTC)
	st.nowFn = func() time.Time { return now }
	token, err := st.CreateEnrollToken("pending-evidence", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if token == "" {
		t.Fatal("test setup produced an empty enrollment token")
	}
	var machineID string
	if err := st.DB().QueryRow(`SELECT used_by FROM enrollment_tokens WHERE token_hash = ?`, hashToken(token)).Scan(&machineID); err != nil {
		t.Fatal(err)
	}
	batch := healthyBatch(now.Add(time.Minute))
	batch.Credentials = []model.Credential{{
		Provider: "RAW_CREDENTIAL_SENTINEL", Status: model.CredConfigured,
		ActiveAccountID: "PRIVATE_ACCOUNT_SENTINEL", AccountCount: 2,
	}}
	if err := st.RecordObservation(machineID, batch, now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	evidence, err := st.MachineReadEvidence(machineID, now.Add(3*time.Minute), 1)
	if err != nil {
		t.Fatal(err)
	}
	printed := fmt.Sprintf("%+v", evidence)
	if strings.Contains(printed, token) {
		t.Fatalf("machine evidence exposed pending enrollment token %q", token)
	}
	if !strings.Contains(printed, "RAW_CREDENTIAL_SENTINEL") {
		t.Fatalf("machine evidence lost the non-secret credential provider: %s", printed)
	}
	if strings.Contains(printed, "PRIVATE_ACCOUNT_SENTINEL") {
		t.Fatalf("machine evidence exposed the active account identifier: %s", printed)
	}
	if len(evidence.Credentials) != 1 || !evidence.Credentials[0].ActiveAccountSelected ||
		evidence.Credentials[0].AccountCount != 2 {
		t.Fatalf("machine evidence lost safe credential selector facts: %+v", evidence.Credentials)
	}
}

func TestMachineReadEvidenceValidatesIdentityAndLimit(t *testing.T) {
	st := newTestStore(t)
	now := time.Date(2026, 9, 9, 14, 0, 0, 0, time.UTC)
	if _, err := st.MachineReadEvidence("", now, 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("empty machine error=%v want ErrNotFound", err)
	}
	if _, err := st.MachineReadEvidence("missing", now, 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing machine error=%v want ErrNotFound", err)
	}
	machineID := mustEnroll(t, st, "limit-evidence", now.Add(-time.Hour))
	if _, err := st.MachineReadEvidence(machineID, now, MaxMachineEvidencePageSize+1); err == nil {
		t.Fatal("oversized limit was accepted")
	}
	if evidence, err := st.MachineReadEvidence(machineID, now, 0); err != nil || evidence.MachineID != machineID {
		t.Fatalf("default limit evidence=%+v err=%v", evidence, err)
	}
}

// TestMachineReadEvidenceRefusesAToolThatIsNotInstalledWhileItsProcessIsRunning
// 釘的是 machine_evidence.go:594 的 processObserved disjunct。
//
// 這一列走的是真正的產品寫入路徑：RecordObservation（store.go:1666-1679）
// 對 CLITool 的欄位零檢查，HTTP handler cmd/clawctl-hub/api.go:220-236 也只比對
// SchemaVersion。所以一支舊的／壞掉的 agent 送得出這一列，Hub 會原樣收下。
//
// 看得見：通過 validator 的列會進 Items；internal/web/templates/machine.html:425
// 的第一個分支是 {{if not .Present}}沒有安裝{{end}}，而證據欄的四個分支
// （SourcesDisagree／unsupported／PresentEvidence=="process"／OnPath）這一列一個都不中，
// 所以網頁會畫成「沒有安裝」配一格空白證據。CLI（machinescmd.go:619-631）
// 會同時印 PRESENT: false 和 PROCESS_OBSERVED: true，自相矛盾但至少看得見；
// 網頁把矛盾折掉了。
//
// ⚠ 實測七臂盤：把 :594、:595、:596、:597 四條一致性子句逐條換成 false，
// 全樹 go test ./... -count=1 四條全綠——這支 validator 的一致性檢查
// 在下這一刀之前沒有任何看守者。
//
// ⚠ 實測隔離盤（這一刀落地後，全樹 go test ./... -count=1）：
// 把 :594 整條換成 false，本測試紅、others=[]；
// 只把 :594 的 || processObserved 那一支拿掉，本測試一樣紅、others=[]。
// 這支測試是 :594 唯一的看守者，而且精確釘在 processObserved 那一支。
// 對照組：關掉 :595、:596、:597 三條，本測試全綠——沒有順手焊到它們，
// 那三條到現在仍然沒有看守者，刻意沒補。
// 對照組：刪掉 PresentEvidence 的封閉值域 switch，本測試綠，
// 只有 TestMachineReadEvidenceRejectsValuesOutsideClosedDomains 紅——
// 這一刀沒有在重守已經有人守的東西。
// ⚠ 反例：把 machineReadCLIProcessObserved 的 tool.RunningPID > 0 拿掉，
// 本測試紅，但 TestAToolRunningWithoutBeingOnPathSaysSoOnScreen 與
// closed-domain 的 accept_cli_tool_present_evidence_"" 也紅。
// 那一臂本來就有人守，不能算成這一刀的隔離證據。
//
// ⚠ 對照組：把 internal/probe/probe.go:1702 的 t.PresentEvidence = "path" 拿掉，
// TestAToolOnPathIsPresentEvenWhenNotRunning 紅；把 :1730 的
// t.PresentEvidence = "process" 拿掉，TestARunningToolIsNeverReportedAsNotInstalled 紅。
// 同一個謊在 agent 產生端有人守，在 Hub 讀取端沒有。Hub 不該相信 agent。
//
// ⚠ :595 的 tool.OnPath && !tool.Present 被 :594 的第一個 disjunct 完全涵蓋，
// 造不出獨有見證，所以它永遠量不出看守者。那是冗餘子句，不是缺口，刻意不補。
//
// ⚠ model.go:918-920 那句「present=true 的時候不准留白」是產生端的規矩。
// 讀取側刻意容忍 Present=true, PresentEvidence==""：present_evidence 跟 on_path
// 同一輪才加進 CLITool，舊觀測本來就沒有這一欄
// （見 internal/web/web_test.go:3095-3137 手寫的舊 JSON），在讀取側強制那個方向
// 會把合法的歷史觀測從 Items 踢成 Invalid。這一刀刻意不碰那個方向。
func TestMachineReadEvidenceRefusesAToolThatIsNotInstalledWhileItsProcessIsRunning(t *testing.T) {
	now := time.Date(2026, 9, 9, 15, 0, 0, 0, time.UTC)

	run := func(t *testing.T, tool model.CLITool, wantInvalid, wantItems int) {
		t.Helper()
		st := newTestStore(t)
		machineID := mustEnroll(t, st, "cli-tool-process-consistency", now.Add(-time.Hour))
		batch := healthyBatch(now.Add(-2 * time.Minute))
		batch.CLITools[0] = tool
		if err := st.RecordObservation(machineID, batch, now.Add(-time.Minute)); err != nil {
			t.Fatal(err)
		}
		evidence, err := st.MachineReadEvidence(machineID, now, 10)
		if err != nil {
			t.Fatal(err)
		}
		if evidence.CLIToolsInvalid != wantInvalid || len(evidence.CLITools) != wantItems {
			t.Fatalf("CLIToolsInvalid=%d len(CLITools)=%d, want %d and %d",
				evidence.CLIToolsInvalid, len(evidence.CLITools), wantInvalid, wantItems)
		}
		if wantItems == 1 && evidence.CLITools[0].Present {
			t.Fatalf("CLITools[0].Present=%t, want false", evidence.CLITools[0].Present)
		}
	}

	t.Run("consistent not installed observation is accepted", func(t *testing.T) {
		run(t, model.CLITool{Name: "openclaw", Present: false}, 0, 1)
	})
	t.Run("running process contradicts not installed observation", func(t *testing.T) {
		run(t, model.CLITool{Name: "openclaw", Present: false, RunningPID: 4242}, 1, 0)
	})
}

// TestMachineReadEvidenceRefusesAProcessEvidenceToolWithNoProcessObserved
// 釘的是 machine_evidence.go:597 的 !processObserved 那一支，它是這一條
// 唯一的獨有見證（另外兩支會同時觸發 :594）。
//
// 這一列跟上面那支測試方向相反：127 釘的是「說沒裝、但在跑」，
// 這一支釘的是「說在跑、但什麼都沒掃到」。
//
// 看得見（這一支比 127 更兇）：internal/web/templates/machine.html:426
// 版本欄會畫琥珀色的「在跑，但不在 PATH 上」，:437 證據欄會畫
// 「process 存在；PATH 無此工具」。兩句都是正面斷言，而那一列
// 自己的 process 掃描是空的。CLI（machinescmd.go:619-631）會印
// PRESENT_EVIDENCE: "process" 配 PROCESS_OBSERVED: false。
//
// 可達性不是竄改 SQL：走 RecordObservation（store.go:1666-1679），
// 真正的產品寫入路徑，對 CLITool 欄位零檢查；
// HTTP handler cmd/clawctl-hub/api.go:220-236 只比對 SchemaVersion。
// 正確的 agent 做不出這一列（probe.go:1726-1730 設 "process" 的那個分支
// 一定同時設好 running 欄位），但舊的／壞掉的 agent 做得出來。
//
// ⚠ 實測：把 :594、:595、:596、:597 四條逐條換成 false，
// 全樹 go test ./... -count=1 在第 127 刀落地之後重量，
// :597 那一格仍然全綠——這一條在下這一刀之前沒有任何看守者。
//
// ⚠ 實測隔離盤（這一刀落地後，全樹 go test ./... -count=1）：
// 把 :597 整條換成 false，本測試紅、127 那支綠、others=[]；
// 只把 :597 的 || !processObserved 那一支拿掉，結果完全相同。
// 對照組：關掉 :594，本測試綠、只有 127 那支紅——
// 127 與 128 互相獨立，沒有任何一支在替另一支擋。
// 對照組：關掉 :596，兩支全綠、others=[]（:596 至今仍無看守者）。
// 對照組：刪掉 PresentEvidence 的封閉值域 switch，兩支全綠，
// 只有 TestMachineReadEvidenceRejectsValuesOutsideClosedDomains 紅。
// ⚠ 反例：把 machineReadCLIProcessObserved 的 tool.RunningPID > 0 拿掉，
// 127 與 128 都紅，且 TestAToolRunningWithoutBeingOnPathSaysSoOnScreen
// 與 closed-domain 的 accept_cli_tool_present_evidence_"" 也紅。
// 那一臂本來就有人守，不能算成這一刀的隔離證據。
//
// ⚠ :596（path 的伴隨條件）到現在仍然沒有看守者，刻意沒補：
// 它的獨有見證是 Present=true, OnPath=false, PresentEvidence="path"
// 或 Present=true, OnPath=true, PresentEvidence="path", Path=""，
// 網頁證據欄對前者什麼都不畫，價值比這一條低。
func TestMachineReadEvidenceRefusesAProcessEvidenceToolWithNoProcessObserved(t *testing.T) {
	now := time.Date(2026, 9, 9, 15, 30, 0, 0, time.UTC)

	t.Run("process evidence with an observed process is accepted", func(t *testing.T) {
		st := newTestStore(t)
		machineID := mustEnroll(t, st, "cli-tool-process-evidence-consistent", now.Add(-time.Hour))
		batch := healthyBatch(now.Add(-2 * time.Minute))
		batch.CLITools[0] = model.CLITool{Name: "openclaw", Present: true, PresentEvidence: "process", RunningPID: 4242}
		if err := st.RecordObservation(machineID, batch, now.Add(-time.Minute)); err != nil {
			t.Fatal(err)
		}
		evidence, err := st.MachineReadEvidence(machineID, now, 10)
		if err != nil {
			t.Fatal(err)
		}
		if evidence.CLIToolsInvalid != 0 || len(evidence.CLITools) != 1 {
			t.Fatalf("CLIToolsInvalid=%d len(CLITools)=%d, want 0 and 1",
				evidence.CLIToolsInvalid, len(evidence.CLITools))
		}
		if evidence.CLITools[0].PresentEvidence != "process" {
			t.Fatalf("CLITools[0].PresentEvidence=%q, want %q", evidence.CLITools[0].PresentEvidence, "process")
		}
		if !evidence.CLITools[0].ProcessObserved {
			t.Fatalf("CLITools[0].ProcessObserved=%t, want true", evidence.CLITools[0].ProcessObserved)
		}
	})

	t.Run("process evidence without an observed process is rejected", func(t *testing.T) {
		st := newTestStore(t)
		machineID := mustEnroll(t, st, "cli-tool-process-evidence-inconsistent", now.Add(-time.Hour))
		batch := healthyBatch(now.Add(-2 * time.Minute))
		batch.CLITools[0] = model.CLITool{Name: "openclaw", Present: true, PresentEvidence: "process"}
		if err := st.RecordObservation(machineID, batch, now.Add(-time.Minute)); err != nil {
			t.Fatal(err)
		}
		evidence, err := st.MachineReadEvidence(machineID, now, 10)
		if err != nil {
			t.Fatal(err)
		}
		if evidence.CLIToolsInvalid != 1 || len(evidence.CLITools) != 0 {
			t.Fatalf("CLIToolsInvalid=%d len(CLITools)=%d, want 1 and 0",
				evidence.CLIToolsInvalid, len(evidence.CLITools))
		}
	})
}

// TestMachineReadEvidenceRefusesAVersionConflictFlagThatContradictsItsSources
// 釘的是 machine_evidence.go:599 的 tool.SourcesDisagree != sourcesDisagree。
//
// ⚠ 實測：把這一條改成 tool.SourcesDisagree != sourcesDisagree && false
// （要用 && false，直接換成 false 會讓 sourcesDisagree 變成 declared and not used
// 而編譯失敗），全樹 go test ./... -count=1 全綠。這一條在下這一刀之前
// 沒有任何看守者。
//
// 為什麼補兩格而不是一格：這是對稱比對，兩個方向各有獨有見證，
// 而且是兩種不同的謊。有人若把它「簡化」成單方向（只擋編造、或只擋藏起），
// 其中一格就會靜靜放行。
//
// 兩種謊在網頁上長得不一樣（internal/web/templates/machine.html:427、:433-436）：
// 編造衝突會畫琥珀色的「來源互相矛盾」，底下卻印出兩個一模一樣的版號；
// 藏起衝突則會若無其事地只顯示 VersionReported 那一個版號，把套件檔說的
// 另一個版號整個吞掉。後者是危險的那一個。
//
// 可達性不是竄改 SQL：SourcesDisagree 是 model.CLITool 的 payload 欄位，
// 走 RecordObservation（store.go:1666-1679），對 CLITool 欄位零檢查；
// HTTP handler cmd/clawctl-hub/api.go:220-236 只比對 SchemaVersion。
//
// ⚠ 實測隔離盤（這一刀落地後，全樹 go test ./... -count=1）：
// 把 :599 換成 tool.SourcesDisagree != sourcesDisagree && false，
// 本測試兩格都紅、others=[]。這一對是這一條唯一的看守者。
// 窄化成 tool.SourcesDisagree && !sourcesDisagree（只擋編造衝突）：
// 只有 two_different_versions_with_no_conflict_flag_are_rejected 紅。
// 窄化成 !tool.SourcesDisagree && sourcesDisagree（只擋藏起衝突）：
// 只有 a_conflict_flag_with_two_identical_versions_is_rejected 紅。
// 這兩臂就是補兩格而不是一格的理由：任何把對稱比對改成單方向的
// 「簡化」，都只會被其中一格擋下。
// 對照組：關掉 :594，本測試兩格都綠，只有第 127 刀那支紅。
// 對照組：關掉 :596，全樹全綠——那一條至今仍無看守者，刻意沒補。
func TestMachineReadEvidenceRefusesAVersionConflictFlagThatContradictsItsSources(t *testing.T) {
	now := time.Date(2026, 9, 9, 16, 0, 0, 0, time.UTC)
	run := func(t *testing.T, tool model.CLITool, wantInvalid, wantItems int) {
		t.Helper()
		st := newTestStore(t)
		machineID := mustEnroll(t, st, "cli-tool-version-conflict", now.Add(-time.Hour))
		batch := healthyBatch(now.Add(-2 * time.Minute))
		batch.CLITools[0] = tool
		if err := st.RecordObservation(machineID, batch, now.Add(-time.Minute)); err != nil {
			t.Fatal(err)
		}
		evidence, err := st.MachineReadEvidence(machineID, now, 10)
		if err != nil {
			t.Fatal(err)
		}
		if evidence.CLIToolsInvalid != wantInvalid || len(evidence.CLITools) != wantItems {
			t.Fatalf("CLIToolsInvalid=%d len(CLITools)=%d, want %d and %d",
				evidence.CLIToolsInvalid, len(evidence.CLITools), wantInvalid, wantItems)
		}
		if wantItems == 1 && !evidence.CLITools[0].SourcesDisagree {
			t.Fatalf("CLITools[0].SourcesDisagree=%t, want true", evidence.CLITools[0].SourcesDisagree)
		}
	}

	t.Run("a flagged conflict backed by two different versions is accepted", func(t *testing.T) {
		run(t, model.CLITool{Name: "openclaw", Present: true, Path: "/usr/bin/openclaw",
			VersionReported: "2026.6.1", VersionPackageJSON: "2026.6.2",
			SourcesDisagree: true, RunningPID: 4242}, 0, 1)
	})
	t.Run("a conflict flag with two identical versions is rejected", func(t *testing.T) {
		run(t, model.CLITool{Name: "openclaw", Present: true, Path: "/usr/bin/openclaw",
			VersionReported: "2026.6.1", VersionPackageJSON: "2026.6.1",
			SourcesDisagree: true, RunningPID: 4242}, 1, 0)
	})
	t.Run("two different versions with no conflict flag are rejected", func(t *testing.T) {
		run(t, model.CLITool{Name: "openclaw", Present: true, Path: "/usr/bin/openclaw",
			VersionReported: "2026.6.1", VersionPackageJSON: "2026.6.2",
			SourcesDisagree: false, RunningPID: 4242}, 1, 0)
	})
}

// TestMachineReadEvidenceRefusesACredentialWhoseProofDoesNotMatchItsMethod
// 釘的是 store/machine_evidence.go:650 那條雙向等價式，規矩出處是
// model.go:845-862「把『我讀了檔案』跟『我驗過了』分開」。
//
// ⚠ 實測（全樹 go test ./... -count=1，下刀前）六臂全空：
// 把那條等價式整條關掉、只留「有時間戳但方法不是 live」那一向、
// 只留「方法是 live 但沒時間戳」那一向、store 揭露旗標的產生端
// （machine_evidence.go:388）、以及 operatorclient/machine_evidence.go:546-547
// 那兩項，全部都是全樹全綠。
//
// 下刀後同六臂重量，兩個方向各有**獨有見證**：
//   - 整條關掉 → 這一支的兩格拒收都紅。
//   - 只留「有時間戳但方法不是 live」那一向 → 只有「方法說驗過了卻沒有時間戳」紅。
//   - 只留「方法是 live 但沒時間戳」那一向 → 只有「有時間戳但方法只是讀檔案」紅。
//     三臂都是 others=[]。任何把這條等價式窄化成單向的「簡化」，
//     都會被剩下那一格擋住。
//   - 另外三臂（store 揭露旗標的產生端、operatorclient 那兩項）**仍然全空**。
//     那是隔壁的臂，這一刀沒有涵蓋。
//
// ⚠ 既有的封閉值域測試（machine_evidence_test.go:102-107）刻意把這一對
// 保持一致：它設 VerifyLiveRequest 的時候會順手補上 VerifiedAt，
// 不是的時候就把它設成 nil。它繞開了這條規矩，所以量不到。
//
// 可達：internal/store/store.go:1763 的憑證 UPSERT 把 agent 送來的
// verification_method 與 verified_at 原樣寫進去，cmd/clawctl-hub/api.go:220-236
// 的 ingest 只比對 SchemaVersion。Hub 這一側的讀取檢查是唯一站在那裡的東西。
//
// 看得見：internal/web/templates/machine.html:387 是
// {{if .VerifiedAt}}<span class="st green">… 驗過</span>{{else}}…從未驗證{{end}}。
// 一筆帶著偽造時間戳、方法卻只是 file_parse 的憑證，會在那一格拿到一個綠色的
// 「驗過」——而那正是 model.go:830 說的「一張被踢掉的票，它的 auth.json
// 看起來跟一張好票一模一樣」。CLI 走 cmd/clawctl-hub/machinescmd.go:647。
func TestMachineReadEvidenceRefusesACredentialWhoseProofDoesNotMatchItsMethod(t *testing.T) {
	now := time.Date(2026, 9, 9, 16, 0, 0, 0, time.UTC)
	verifiedAt := time.Date(2026, 9, 9, 15, 30, 0, 0, time.UTC)
	run := func(t *testing.T, credential model.Credential, wantInvalid, wantItems int) {
		t.Helper()
		st := newTestStore(t)
		machineID := mustEnroll(t, st, "credential-proof-method", now.Add(-time.Hour))
		batch := healthyBatch(now.Add(-2 * time.Minute))
		batch.Credentials = []model.Credential{credential}
		if err := st.RecordObservation(machineID, batch, now.Add(-time.Minute)); err != nil {
			t.Fatal(err)
		}
		evidence, err := st.MachineReadEvidence(machineID, now, 10)
		if err != nil {
			t.Fatal(err)
		}
		if evidence.CredentialsInvalid != wantInvalid || len(evidence.Credentials) != wantItems {
			t.Fatalf("CredentialsInvalid=%d len(Credentials)=%d, want %d and %d",
				evidence.CredentialsInvalid, len(evidence.Credentials), wantInvalid, wantItems)
		}
	}

	t.Run("一個 live_request 配著它的時間戳是可以接受的", func(t *testing.T) {
		run(t, model.Credential{Provider: "claude", Status: model.CredConfigured,
			VerificationMethod: model.VerifyLiveRequest, VerifiedAt: &verifiedAt}, 0, 1)
	})
	t.Run("有時間戳但方法只是讀檔案，拒收", func(t *testing.T) {
		run(t, model.Credential{Provider: "claude", Status: model.CredConfigured,
			VerificationMethod: model.VerifyFileParse, VerifiedAt: &verifiedAt}, 1, 0)
	})
	t.Run("方法說驗過了卻沒有時間戳，拒收", func(t *testing.T) {
		run(t, model.Credential{Provider: "claude", Status: model.CredConfigured,
			VerificationMethod: model.VerifyLiveRequest, VerifiedAt: nil}, 1, 0)
	})
}

// TestMachineReadEvidenceDisclosesWhetherARemoteCheckWasActuallyMade
// 釘的是 store/machine_evidence.go:388 那個揭露旗標的產生端。
//
// ⚠ 實測（全樹 go test ./... -count=1，下刀前）：把那一條整條短路掉，
// 全樹全綠。旗標永遠是 false，沒有任何測試發現。
//
// ⚠ 兩格的來歷不一樣，別把它們當成同一個發現：
//   - 「真的打過請求的」那一格是這一刀的理由。把產生端短路掉之後
//     **只有它紅**（others=[]）。
//   - 「只讀了檔案的」那一格是**順帶覆蓋**。把旗標改成無條件 true，
//     除了它以外還有四支紅：TestMachineEvidenceClientAcceptsCanonicalAndRejectsIncoherent、
//     TestMachineEvidenceProjectionScrubsTruncatesAndLabels、
//     TestMachinesEvidenceCLIQuotesClosedValueFields、
//     TestMachinesEvidenceCLIUsesHTTPEmitsJSONAndEscapesMultilineText。
//     留著它是為了讓這一支自己說得完整，不是因為那個方向有缺口。
//     順帶守到的覆蓋會隨別人重構消失，這一格是把它釘在原地。
//
// ⚠ 那一行的 && credential.VerifiedAt != nil 是冗餘的：
// :376 有 continue，所以走到 :388 的每一列都已經通過
// validMachineReadCredential，而 :650 那條雙向等價式已經保證
// 「有 VerifiedAt ⟺ 方法是 live_request」。實測把那半拿掉一樣全樹全綠。
// 這一刀不替冗餘補測試——留著那半是防禦深度，不是缺口。
//
// 隔壁的臂由 operatorclient 那邊守：machine_evidence.go:179-180 兩項，
// 兩個方向各有自己的案例（credential claims remote validation 與
// credential proves a remote check the disclosure denies）。
// 那是 client 不相信 Hub；這一支是 Hub 自己對自己的算法負責，兩邊分開量。
//
// 看得見：cmd/clawctl-hub/machinescmd.go:519 的揭露區塊會把這個旗標印出來。
// 一個說「沒做過遠端驗證」的揭露，配上 machine.html:387 一個綠色的「驗過」，
// 是同一頁上兩句互相矛盾的話。
func TestMachineReadEvidenceDisclosesWhetherARemoteCheckWasActuallyMade(t *testing.T) {
	now := time.Date(2026, 9, 9, 16, 0, 0, 0, time.UTC)
	verifiedAt := time.Date(2026, 9, 9, 15, 30, 0, 0, time.UTC)
	run := func(t *testing.T, credential model.Credential) MachineReadEvidence {
		t.Helper()
		st := newTestStore(t)
		machineID := mustEnroll(t, st, "credential-remote-validation-disclosure", now.Add(-time.Hour))
		batch := healthyBatch(now.Add(-2 * time.Minute))
		batch.Credentials = []model.Credential{credential}
		if err := st.RecordObservation(machineID, batch, now.Add(-time.Minute)); err != nil {
			t.Fatal(err)
		}
		evidence, err := st.MachineReadEvidence(machineID, now, 10)
		if err != nil {
			t.Fatal(err)
		}
		return evidence
	}

	t.Run("真的打過請求的憑證會讓揭露說驗過了", func(t *testing.T) {
		evidence := run(t, model.Credential{Provider: "claude", Status: model.CredConfigured,
			VerificationMethod: model.VerifyLiveRequest, VerifiedAt: &verifiedAt})
		if evidence.CredentialsInvalid != 0 || len(evidence.Credentials) != 1 {
			t.Fatalf("CredentialsInvalid=%d len(Credentials)=%d, want 0 and 1",
				evidence.CredentialsInvalid, len(evidence.Credentials))
		}
		if !evidence.CredentialRemoteValidationPerformed {
			t.Fatal("畫面上那個綠色的「驗過」跟這個揭露旗標必須講同一件事")
		}
	})

	t.Run("只讀了檔案的憑證不會讓揭露說驗過了", func(t *testing.T) {
		evidence := run(t, model.Credential{Provider: "claude", Status: model.CredConfigured,
			VerificationMethod: model.VerifyFileParse})
		if evidence.CredentialsInvalid != 0 || len(evidence.Credentials) != 1 {
			t.Fatalf("CredentialsInvalid=%d len(Credentials)=%d, want 0 and 1",
				evidence.CredentialsInvalid, len(evidence.Credentials))
		}
		if evidence.CredentialRemoteValidationPerformed {
			t.Fatal("只讀了本機檔案就宣稱做過遠端驗證，是這張表上最貴的一句謊")
		}
	})
}
