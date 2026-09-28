package operatorclient

import (
	"bytes"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/model"
	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/store"
)

func TestMachineEvidenceClientAcceptsCanonicalAndRejectsIncoherent(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Date(2026, 9, 9, 18, 0, 0, 0, time.UTC)
	const machineID = "machine-client-evidence"
	if err := st.UpsertMachine(store.Machine{
		MachineID: machineID, DisplayName: "client-evidence", Expected: true, CreatedAt: now.Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	shapes := make([]model.JournalShape, 0, operator.MachineEvidenceMaxShapes+1)
	for i := 0; i < operator.MachineEvidenceMaxShapes+1; i++ {
		shapes = append(shapes, model.JournalShape{Count: i + 1, Example: "example"})
	}
	peerID := "machine-client-peer"
	if err := st.UpsertMachine(store.Machine{
		MachineID: peerID, DisplayName: "client-peer", Expected: true, CreatedAt: now.Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	for i, age := range []time.Duration{3 * time.Hour, time.Hour} {
		mtime := now.Add(-age)
		expires := mtime.Add(8 * time.Hour)
		if err := st.RecordObservation(peerID, model.ObservationBatch{
			SchemaVersion: model.SchemaVersion, MeasuredAt: now.Add(-age + time.Minute),
			Credentials: []model.Credential{{
				Provider: "alpha", Status: model.CredConfigured, ExpiresAt: &expires, FileMTime: &mtime,
			}}}, now.Add(-age+time.Duration(i+2)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	mtime := now.Add(-2 * time.Hour)
	expires := mtime.Add(8 * time.Hour)
	writable := true
	matches := true
	restarts := 2
	batch := model.ObservationBatch{
		SchemaVersion: model.SchemaVersion, MeasuredAt: now.Add(-time.Minute),
		Credentials: []model.Credential{
			{Provider: "alpha", Status: model.CredConfigured, ExpiresAt: &expires, FileMTime: &mtime,
				VerificationMethod: model.VerifyFileParse, ActiveAccountID: "PRIVATE_ACCOUNT", AccountCount: 2,
				Note: "note", LastError: "error"},
			{Provider: "bravo", Status: model.CredAbsent},
		},
		Systemd: []model.Unit{{Name: "alpha.service", Present: true, Measured: true, Reason: "bus\x1b failure", ActiveState: "active", SubState: "running",
			ActiveEnterTimestamp: machineEvidenceClientTimePtr(now.Add(-time.Hour)), NRestarts: 2, MainPID: 987654}, {Name: "bravo.service"},
			{Name: "yankee.service"}, {Name: "zulu.service"}},
		Journals: []model.UnitJournal{
			{Unit: "alpha.service", WindowSec: 60, Lines: 3, Shapes: 11, Top: shapes},
			{Unit: "bravo.service", Err: "not heard"},
		},
		OpenClaw: model.OpenClaw{Present: true, CLIVersion: "2026.9.1", GatewayVersion: "2026.9.0",
			Install: &model.OpenClawInstall{UnitFound: true, NRestarts: &restarts,
				RunningDir: "/private/running", RunningDirExists: true, RunningDirWritable: &writable,
				ProcessIndexJS: "/private/index.js", ProcessMatchesUnit: &matches},
			DB: &model.OpenClawDB{
				Path: "/private/openclaw.sqlite", FoundAt: []string{"/private/other.sqlite"},
				TaskRunRows: 3, TaskStatusCount: map[string]int{"custom": 1, "ok": 2},
				OccupancyRowsSeen: 2,
				Occupancy: []model.OccupancyEvidence{
					{Source: "cron_run_logs", JobID: "job-occ-a", At: now.Add(-4 * time.Minute), Provider: "alpha", AgentID: "main"},
					{Source: "cron_runs_jsonl", JobID: "job-occ-b", At: now.Add(-2 * time.Minute), Provider: "zulu", AgentID: "worker"},
				},
				RecentSummaries: []model.RunSummary{
					{JobID: "job-a", At: now.Add(-3 * time.Minute), Status: "ok", Summary: "line one\nline two"},
					{JobID: "job-b", Status: "custom", Summary: "second"},
					{JobID: "job-c", Status: "ok", Summary: "third"},
				},
			}},
		CLITools: []model.CLITool{
			{Name: "alpha", Present: true, OnPath: true, PresentEvidence: "path", PathSource: model.PathSourceLogin,
				Path: "/private/alpha", RealPath: "/private/alpha-real", RunningPID: 42,
				RunningExe: "/private/alpha-real", VersionReported: "1.0.0",
				VersionPackageJSON: "1.0.1", SourcesDisagree: true},
			{Name: "bravo", Present: true, OnPath: true, PresentEvidence: "path",
				Path:       "/private/bravo",
				VersionRaw: "bravo nightly", Support: model.SupportUnsupported},
			{Name: "zulu", Present: false},
		},
	}
	if err := st.RecordObservation(machineID, batch, now.Add(-30*time.Second)); err != nil {
		t.Fatal(err)
	}
	canonical, err := operator.New(st).MachineEvidence(operator.MachineEvidenceRequest{
		MachineID: machineID, Limit: 2,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	canonicalJSON, err := json.Marshal(canonical)
	if err != nil {
		t.Fatal(err)
	}

	client, requests := jobEvidenceRecorderClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/operator/machines/"+machineID+"/evidence" || r.URL.RawQuery != "limit=2" {
			t.Errorf("request URL=%s", r.URL.String())
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(canonicalJSON)
	})
	got, err := client.MachineEvidence(t.Context(), machineID, 2)
	if err != nil || got.MachineID != machineID || got.Disclosure.Limit != 2 ||
		!got.OpenClaw.Decoded || got.OpenClaw.Install == nil || got.OpenClaw.DB == nil ||
		got.OpenClaw.DB.UnknownLocationCount != 1 || len(got.OpenClaw.DB.TaskStatuses.Items) != 2 ||
		len(got.CLITools.Items) != 2 || got.CLITools.Total != 3 || !got.CLITools.Truncated ||
		len(got.Credentials.Items) != 2 || got.Credentials.Items[0].PeersTotal != 1 ||
		len(got.Occupancy.Items) != 2 || got.Occupancy.RowsSeen != 2 ||
		len(got.SystemdUnits.Items) != 2 || got.SystemdUnits.Total != 4 || !got.SystemdUnits.Truncated ||
		len(got.RunSummaries.Items) != 2 || len(got.Journals.Items) != 2 ||
		len(got.Journals.UnitsWithoutJournal) != 2 {
		t.Fatalf("canonical result=%+v err=%v", got, err)
	}
	if requests.Load() != 1 {
		t.Fatalf("canonical request count=%d want=1", requests.Load())
	}
	if got.SchemaVersion != 6 || !got.SystemdUnits.Items[0].Measured || got.SystemdUnits.Items[0].Reason == nil ||
		got.SystemdUnits.Items[0].Reason.Text != "bus� failure" ||
		strings.Join(got.SystemdUnits.Items[0].Reason.Issues, ",") != "control_or_format_replaced" {
		t.Fatalf("strictly decoded systemd measurement=%+v schema=%d", got.SystemdUnits.Items[0], got.SchemaVersion)
	}

	tests := []struct {
		name   string
		mutate func(*operator.MachineEvidenceResult)
		raw    func([]byte) []byte
	}{
		{"schema version", func(v *operator.MachineEvidenceResult) { v.SchemaVersion++ }, nil},
		{"zero evaluated at", func(v *operator.MachineEvidenceResult) { v.EvaluatedAt = time.Time{} }, nil},
		{"empty machine id", func(v *operator.MachineEvidenceResult) { v.MachineID = "" }, nil},
		{"disclosure limit", func(v *operator.MachineEvidenceResult) { v.Disclosure.Limit = 0 }, nil},
		{"max field bytes", func(v *operator.MachineEvidenceResult) { v.Disclosure.MaxFieldBytes-- }, nil},
		{"max shapes", func(v *operator.MachineEvidenceResult) { v.Disclosure.MaxShapes-- }, nil},
		{"max credential peers", func(v *operator.MachineEvidenceResult) { v.Disclosure.MaxCredentialPeers-- }, nil},
		{"max occupancy agents", func(v *operator.MachineEvidenceResult) { v.Disclosure.MaxOccupancyAgents-- }, nil},
		{"independent verifier", func(v *operator.MachineEvidenceResult) { v.Disclosure.IndependentVerifier = true }, nil},
		{"status is outcome", func(v *operator.MachineEvidenceResult) { v.Disclosure.StatusIsOutcome = true }, nil},
		{"terminal outcome", func(v *operator.MachineEvidenceResult) { v.Disclosure.TerminalOutcomeRecorded = true }, nil},
		{"systemd work outcome", func(v *operator.MachineEvidenceResult) {
			v.Disclosure.SystemdStateIsWorkOutcome = true
		}, nil},
		{"systemd main pid included", func(v *operator.MachineEvidenceResult) {
			v.Disclosure.SystemdMainPIDExcluded = false
		}, nil},
		{"host path fields included", func(v *operator.MachineEvidenceResult) {
			v.Disclosure.HostPathFieldsExcluded = false
		}, nil},
		{"process ID fields included", func(v *operator.MachineEvidenceResult) {
			v.Disclosure.ProcessIDFieldsExcluded = false
		}, nil},
		{"OpenClaw DB locations included", func(v *operator.MachineEvidenceResult) {
			v.Disclosure.OpenClawDatabaseLocationsExcluded = false
		}, nil},
		{"text path redaction invented", func(v *operator.MachineEvidenceResult) {
			v.Disclosure.EvidenceTextPathRedactedByHub = true
		}, nil},
		{"CLI OpenClaw agent redaction invented", func(v *operator.MachineEvidenceResult) {
			v.Disclosure.CLIOpenClawTextRedactedByAgent = true
		}, nil},
		{"CLI OpenClaw Hub redaction invented", func(v *operator.MachineEvidenceResult) {
			v.Disclosure.CLIOpenClawTextRedactedByHub = true
		}, nil},
		{"CLI version sources collapsed", func(v *operator.MachineEvidenceResult) {
			v.Disclosure.CLIVersionSourcesCollapsed = true
		}, nil},
		{"raw version parsed", func(v *operator.MachineEvidenceResult) {
			v.Disclosure.RawVersionTextParsedByHub = true
		}, nil},
		{"running relationship not path-derived", func(v *operator.MachineEvidenceResult) {
			v.Disclosure.RunningRelationshipDerivedFromPaths = false
		}, nil},
		{"OpenClaw producer mismatch", func(v *operator.MachineEvidenceResult) {
			v.Disclosure.OpenClawProducer.MachineID = "other"
		}, nil},
		{"CLI producer mismatch", func(v *operator.MachineEvidenceResult) {
			v.Disclosure.CLIToolProducer.MachineID = "other"
		}, nil},
		{"systemd producer mismatch", func(v *operator.MachineEvidenceResult) {
			v.Disclosure.SystemdProducer.MachineID = "other"
		}, nil},
		{"credential producer mismatch", func(v *operator.MachineEvidenceResult) {
			v.Disclosure.CredentialProducer.MachineID = "other"
		}, nil},
		{"occupancy producer authority", func(v *operator.MachineEvidenceResult) {
			v.Disclosure.OccupancyProducer.Authority = operator.MachineEvidenceAuthorityMachineBearer
		}, nil},
		{"occupancy relay mismatch", func(v *operator.MachineEvidenceResult) {
			v.Disclosure.OccupancyRelay.MachineID = "other"
		}, nil},
		{"credential claims session validity", func(v *operator.MachineEvidenceResult) {
			v.Disclosure.CredentialStatusIsSessionValidity = true
		}, nil},
		{"credential claims remote validation", func(v *operator.MachineEvidenceResult) {
			v.Disclosure.CredentialRemoteValidationPerformed = true
		}, nil},
		// ⚠ 這一筆跟上面那一筆是同一條規矩的兩個方向：上面守「揭露說驗過、
		// 項目裡沒有」，這一筆守「揭露說沒驗、項目裡有」。實測每一項都只有
		// 自己那一筆紅（others=[]），互相換不掉。
		//
		// 兩行突變缺一不可。只改方法的話，這一筆會先被 :547（方法是 live 但
		// 沒時間戳）擋掉，量到的就不是 :179；時間也一定要從同一列身上拿，
		// 字面量會被 UTC／零值那一關先擋掉。實測把 :547 短路，這一筆仍然綠。
		{"credential proves a remote check the disclosure denies", func(v *operator.MachineEvidenceResult) {
			v.Credentials.Items[0].VerificationMethod = model.VerifyLiveRequest
			v.Credentials.Items[0].VerifiedAt = machineEvidenceClientTimePtr(v.Credentials.Items[0].MeasuredAt)
		}, nil},
		{"credential account id not excluded", func(v *operator.MachineEvidenceResult) {
			v.Disclosure.CredentialActiveAccountIDExcluded = false
		}, nil},
		{"credential secret fields not excluded", func(v *operator.MachineEvidenceResult) {
			v.Disclosure.CredentialSecretFieldsExcluded = false
		}, nil},
		{"occupancy uses process state", func(v *operator.MachineEvidenceResult) {
			v.Disclosure.OccupancyProcessStateUsed = true
		}, nil},
		{"occupancy normalizes provider", func(v *operator.MachineEvidenceResult) {
			v.Disclosure.OccupancyProviderNormalized = true
		}, nil},
		{"occupancy categorizes error", func(v *operator.MachineEvidenceResult) {
			v.Disclosure.OccupancyErrorsCategorized = true
		}, nil},
		{"journal redaction false", func(v *operator.MachineEvidenceResult) {
			v.Disclosure.JournalSecretShapesRedactedByAgent = false
		}, nil},
		{"summary redaction true", func(v *operator.MachineEvidenceResult) {
			v.Disclosure.RunSummarySecretShapesRedactedByAgent = true
		}, nil},
		{"openclaw invented authority", func(v *operator.MachineEvidenceResult) {
			v.Disclosure.RunSummaryProducer.Authority = operator.MachineEvidenceAuthorityMachineBearer
		}, nil},
		{"relay producer mismatch", func(v *operator.MachineEvidenceResult) {
			v.Disclosure.RunSummaryRelay.MachineID = "other"
		}, nil},
		{"credentials over limit", func(v *operator.MachineEvidenceResult) {
			v.Credentials.Items = append(v.Credentials.Items, v.Credentials.Items[0])
		}, nil},
		{"OpenClaw without observation clock", func(v *operator.MachineEvidenceResult) {
			v.OpenClaw.ObservedAt = nil
		}, nil},
		{"OpenClaw negative crash bundles", func(v *operator.MachineEvidenceResult) {
			v.OpenClaw.CrashBundles = -1
		}, nil},
		{"OpenClaw install relation without process", func(v *operator.MachineEvidenceResult) {
			v.OpenClaw.Install.ProcessObserved = false
		}, nil},
		{"OpenClaw install writable without directory", func(v *operator.MachineEvidenceResult) {
			v.OpenClaw.Install.RunningDirectoryObserved = false
		}, nil},
		{"OpenClaw database unknown layout", func(v *operator.MachineEvidenceResult) {
			v.OpenClaw.DB.Layout = &operator.EvidenceText{Text: "future"}
		}, nil},
		{"OpenClaw unsupported DB without unknown location", func(v *operator.MachineEvidenceResult) {
			v.OpenClaw.DB.Support = model.SupportUnsupported
			v.OpenClaw.DB.UnknownLocationCount = 0
		}, nil},
		{"OpenClaw status unsorted", func(v *operator.MachineEvidenceResult) {
			v.OpenClaw.DB.TaskStatuses.Items[0], v.OpenClaw.DB.TaskStatuses.Items[1] =
				v.OpenClaw.DB.TaskStatuses.Items[1], v.OpenClaw.DB.TaskStatuses.Items[0]
		}, nil},
		{"OpenClaw invalid path-like strict version", func(v *operator.MachineEvidenceResult) {
			v.OpenClaw.CLIVersion.Text = "version\n"
			v.OpenClaw.CLIVersion.Bytes = len("version\n")
		}, nil},
		{"CLI tools over limit", func(v *operator.MachineEvidenceResult) {
			v.CLITools.Items = append(v.CLITools.Items, v.CLITools.Items[0])
		}, nil},
		{"CLI tools unsorted", func(v *operator.MachineEvidenceResult) {
			v.CLITools.Items[0], v.CLITools.Items[1] = v.CLITools.Items[1], v.CLITools.Items[0]
		}, nil},
		{"CLI running relationship unknown", func(v *operator.MachineEvidenceResult) {
			v.CLITools.Items[0].RunningRelationship = "invented"
		}, nil},
		{"CLI process scan unknown", func(v *operator.MachineEvidenceResult) {
			v.CLITools.Items[0].ProcessScan = "invented"
		}, nil},
		{"CLI path evidence without on-path", func(v *operator.MachineEvidenceResult) {
			v.CLITools.Items[0].OnPath = false
		}, nil},
		{"CLI conflict flag lost", func(v *operator.MachineEvidenceResult) {
			v.CLITools.Items[0].SourcesDisagree = false
		}, nil},
		{"CLI unsupported without raw", func(v *operator.MachineEvidenceResult) {
			v.CLITools.Items[1].VersionRaw = nil
		}, nil},
		{"credentials unsorted", func(v *operator.MachineEvidenceResult) {
			v.Credentials.Items[0], v.Credentials.Items[1] = v.Credentials.Items[1], v.Credentials.Items[0]
		}, nil},
		{"credential unknown status", func(v *operator.MachineEvidenceResult) {
			v.Credentials.Items[0].Status = model.CredStatus("invented")
		}, nil},
		{"credential negative account count", func(v *operator.MachineEvidenceResult) {
			v.Credentials.Items[0].AccountCount = -1
		}, nil},
		{"credential zero measured at", func(v *operator.MachineEvidenceResult) {
			v.Credentials.Items[0].MeasuredAt = time.Time{}
		}, nil},
		{"credential peer zero refresh", func(v *operator.MachineEvidenceResult) {
			v.Credentials.Items[0].Peers[0].RefreshesSeen = 0
		}, nil},
		{"credential strict provider control", func(v *operator.MachineEvidenceResult) {
			v.Credentials.Items[0].Provider.Text = "alpha\n"
			v.Credentials.Items[0].Provider.Bytes = len("alpha\n")
		}, nil},
		{"credential note escape", func(v *operator.MachineEvidenceResult) {
			v.Credentials.Items[0].Note.Text = "note\x1b[31m"
			v.Credentials.Items[0].Note.Bytes = len(v.Credentials.Items[0].Note.Text)
		}, nil},
		{"occupancy over limit", func(v *operator.MachineEvidenceResult) {
			v.Occupancy.Items = append(v.Occupancy.Items, v.Occupancy.Items[0])
		}, nil},
		{"occupancy unsorted", func(v *operator.MachineEvidenceResult) {
			v.Occupancy.Items[0], v.Occupancy.Items[1] = v.Occupancy.Items[1], v.Occupancy.Items[0]
		}, nil},
		{"occupancy errors exceed runs", func(v *operator.MachineEvidenceResult) {
			v.Occupancy.Items[0].Errors = v.Occupancy.Items[0].Runs + 1
		}, nil},
		{"occupancy rows skipped exceed seen", func(v *operator.MachineEvidenceResult) {
			v.Occupancy.RowsWithoutProvider = v.Occupancy.RowsSeen + 1
		}, nil},
		{"occupancy unobserved with clock", func(v *operator.MachineEvidenceResult) {
			v.Occupancy.Observed = false
		}, nil},
		{"occupancy zero last received", func(v *operator.MachineEvidenceResult) {
			v.Occupancy.Items[0].LastReceivedAt = time.Time{}
		}, nil},
		{"occupancy agent control", func(v *operator.MachineEvidenceResult) {
			v.Occupancy.Items[0].Agents[0].Text = "main\n"
			v.Occupancy.Items[0].Agents[0].Bytes = len("main\n")
		}, nil},
		{"systemd invalid negative", func(v *operator.MachineEvidenceResult) {
			v.SystemdUnits.Invalid = -1
		}, nil},
		{"systemd over limit", func(v *operator.MachineEvidenceResult) {
			v.SystemdUnits.Items = append(v.SystemdUnits.Items, v.SystemdUnits.Items[0])
		}, nil},
		{"systemd unsorted", func(v *operator.MachineEvidenceResult) {
			v.SystemdUnits.Items[0], v.SystemdUnits.Items[1] = v.SystemdUnits.Items[1], v.SystemdUnits.Items[0]
		}, nil},
		{"systemd negative restarts", func(v *operator.MachineEvidenceResult) {
			v.SystemdUnits.Items[0].NRestarts = -1
		}, nil},
		{"systemd zero measured at", func(v *operator.MachineEvidenceResult) {
			v.SystemdUnits.Items[0].MeasuredAt = time.Time{}
		}, nil},
		{"systemd zero active enter", func(v *operator.MachineEvidenceResult) {
			zero := time.Time{}
			v.SystemdUnits.Items[0].ActiveEnterTimestamp = &zero
		}, nil},
		{"systemd state control", func(v *operator.MachineEvidenceResult) {
			v.SystemdUnits.Items[0].ActiveState.Text = "active\n"
			v.SystemdUnits.Items[0].ActiveState.Bytes = len("active\n")
		}, nil},
		{"systemd reason control", func(v *operator.MachineEvidenceResult) {
			v.SystemdUnits.Items[0].Reason.Text = "bus\x1b failure"
			v.SystemdUnits.Items[0].Reason.Bytes = len(v.SystemdUnits.Items[0].Reason.Text)
		}, nil},
		{"db observed without observation", func(v *operator.MachineEvidenceResult) {
			v.RunSummaries.Observed = false
			v.RunSummaries.ObservedAt = nil
		}, nil},
		{"unobserved with clock", func(v *operator.MachineEvidenceResult) {
			v.RunSummaries.Observed = false
			v.RunSummaries.DBObserved = false
			v.RunSummaries.Total = 0
			v.RunSummaries.Truncated = false
			v.RunSummaries.Items = []operator.MachineRunSummary{}
		}, nil},
		{"observed without clock", func(v *operator.MachineEvidenceResult) { v.RunSummaries.ObservedAt = nil }, nil},
		{"run summaries over limit", func(v *operator.MachineEvidenceResult) {
			v.RunSummaries.Items = append(v.RunSummaries.Items, v.RunSummaries.Items[0])
		}, nil},
		{"reported at zero", func(v *operator.MachineEvidenceResult) {
			zero := time.Time{}
			v.RunSummaries.Items[0].ReportedAt = &zero
		}, nil},
		{"text max bytes", func(v *operator.MachineEvidenceResult) { v.RunSummaries.Items[0].JobID.MaxBytes-- }, nil},
		{"text over bound", func(v *operator.MachineEvidenceResult) {
			v.RunSummaries.Items[0].Summary.Text = strings.Repeat("x", operator.MachineEvidenceMaxFieldBytes+1)
		}, nil},
		{"truncated issue mismatch", func(v *operator.MachineEvidenceResult) {
			v.RunSummaries.Items[0].Summary.Truncated = true
		}, nil},
		{"truncated zero bytes", func(v *operator.MachineEvidenceResult) {
			v.RunSummaries.Items[0].Summary.Truncated = true
			v.RunSummaries.Items[0].Summary.Bytes = 0
			v.RunSummaries.Items[0].Summary.Issues = []string{"truncated"}
		}, nil},
		{"issue-free bytes mismatch", func(v *operator.MachineEvidenceResult) {
			v.RunSummaries.Items[0].Status.Bytes++
		}, nil},
		{"issues unsorted", func(v *operator.MachineEvidenceResult) {
			v.RunSummaries.Items[0].Summary.Issues = []string{"truncated", "control_or_format_replaced"}
			v.RunSummaries.Items[0].Summary.Truncated = true
		}, nil},
		{"unknown issue", func(v *operator.MachineEvidenceResult) {
			v.RunSummaries.Items[0].Summary.Issues = []string{"invented"}
		}, nil},
		{"duplicate issues", func(v *operator.MachineEvidenceResult) {
			v.RunSummaries.Items[0].Summary.Issues = []string{"invalid_utf8", "invalid_utf8"}
		}, nil},
		{"strict text control", func(v *operator.MachineEvidenceResult) {
			v.RunSummaries.Items[0].Status.Text = "ok\n"
			v.RunSummaries.Items[0].Status.Bytes = 3
		}, nil},
		{"block text escape", func(v *operator.MachineEvidenceResult) {
			v.RunSummaries.Items[0].Summary.Text = "ok\x1b[31m"
			v.RunSummaries.Items[0].Summary.Bytes = len(v.RunSummaries.Items[0].Summary.Text)
		}, nil},
		{"journals over limit", func(v *operator.MachineEvidenceResult) {
			v.Journals.Items = append(v.Journals.Items, v.Journals.Items[0])
		}, nil},
		{"top over max shapes", func(v *operator.MachineEvidenceResult) {
			v.Journals.Items[0].Top = append(v.Journals.Items[0].Top, v.Journals.Items[0].Top[0])
		}, nil},
		{"read failed without err", func(v *operator.MachineEvidenceResult) {
			v.Journals.Items[0].ReadFailed = true
		}, nil},
		{"err without read failure", func(v *operator.MachineEvidenceResult) {
			v.Journals.Items[1].ReadFailed = false
		}, nil},
		{"zero journal measured at", func(v *operator.MachineEvidenceResult) {
			v.Journals.Items[0].MeasuredAt = time.Time{}
		}, nil},
		{"zero journal received at", func(v *operator.MachineEvidenceResult) {
			v.Journals.Items[0].ReceivedAt = time.Time{}
		}, nil},
		{"negative undecodable", func(v *operator.MachineEvidenceResult) { v.Journals.Undecodable = -1 }, nil},
		{"negative units total", func(v *operator.MachineEvidenceResult) {
			v.Journals.UnitsWithoutJournalTotal = -1
		}, nil},
		{"units total below returned", func(v *operator.MachineEvidenceResult) {
			v.Journals.UnitsWithoutJournalTotal = 1
		}, nil},
		{"units over limit", func(v *operator.MachineEvidenceResult) {
			v.Journals.UnitsWithoutJournal = append(v.Journals.UnitsWithoutJournal,
				v.Journals.UnitsWithoutJournal[0])
		}, nil},
		{"units unsorted", func(v *operator.MachineEvidenceResult) {
			v.Journals.UnitsWithoutJournal[0], v.Journals.UnitsWithoutJournal[1] =
				v.Journals.UnitsWithoutJournal[1], v.Journals.UnitsWithoutJournal[0]
		}, nil},
		{"unit overlaps journal", func(v *operator.MachineEvidenceResult) {
			v.Journals.UnitsWithoutJournal[0] = v.Journals.Items[0].Unit
		}, nil},
		// 這兩個 case 守的是「在場但為零」的時間。cmd/clawctl-hub 的
		// machineReadTime 只有 nil 才印 unknown，非 nil 一律 .UTC().Format——
		// 一個零值會印成 0001-01-01T00:00:00Z，operator 讀到的是一個看起來量過的
		// 時刻，而不是「沒量到」。非 UTC 那一臂刻意沒打：CLI 自己會 .UTC()，
		// 把守衛拿掉之後 operator 看到的字一模一樣。
		{"credential expires at present but zero", func(v *operator.MachineEvidenceResult) {
			v.Credentials.Items[0].ExpiresAt = machineEvidenceClientTimePtr(time.Time{})
		}, nil},
		{"OpenClaw observed clock zero", func(v *operator.MachineEvidenceResult) {
			v.OpenClaw.ObservedAt.MeasuredAt = time.Time{}
		}, nil},
		// 這兩格釘的是 machine_evidence.go:444 的
		// tool.MeasuredAt.IsZero() || tool.ReceivedAt.IsZero()。它們是上面那一族
		// （credential expires at present but zero、OpenClaw observed clock zero）漏掉的
		// 成員，不是設計選擇：cmd/clawctl-hub/machinescmd.go:632 對這兩個欄位直接
		// .UTC().Format(time.RFC3339Nano)，零值會印成 0001-01-01T00:00:00Z——
		// operator 讀到的是「一個看起來量過的時刻」，不是「沒量到」。後備不存在：
		// 同一個迴圈稍後會呼叫 requireUTCJobTime(tool.MeasuredAt,
		// "cli_tool.measured_at")，但那支函式（internal/operatorclient/jobs.go:505-510）
		// 只檢查時區 offset，而零值時間的 offset 正好是 0，所以它放行。實測 18 條
		// 子句普查（逐條換成 false，全樹 go test ./... -count=1）：這兩條都全綠，
		// 在這兩格之前沒有任何看守者。非 UTC 那一臂一樣刻意沒打，理由跟上面那一族
		// 相同：CLI 自己會 .UTC()，把守衛拿掉之後 operator 看到的字一模一樣。
		//
		// ⚠ 實測隔離盤（這兩格落地後，全樹 go test ./... -count=1）：
		// 關掉 MeasuredAt.IsZero()，只有 CLI_tool_measured_at_zero 紅；
		// 關掉 ReceivedAt.IsZero()，只有 CLI_tool_received_at_zero 紅。
		// 兩格互不相干，各自是自己那一條的唯一看守者。
		// 對照組：關掉 :449 的 RunningRelationship 一致性，兩格都綠，
		// 只有 CLI_running_relationship_without_an_observed_process 紅。
		// ⚠ 同一盤還量到：把整個
		// requireUTCJobTime(tool.MeasuredAt, "cli_tool.measured_at") 呼叫刪掉，
		// 全樹仍然全綠。它既不是這兩格的後備，自己也沒有任何看守者。
		// 刻意沒補：它唯一的獨有見證是非 UTC 時間，而 CLI 自己會 .UTC()，
		// operator 看到的字一模一樣。
		{"CLI tool measured at zero", func(v *operator.MachineEvidenceResult) {
			v.CLITools.Items[0].MeasuredAt = time.Time{}
		}, nil},
		{"CLI tool received at zero", func(v *operator.MachineEvidenceResult) {
			v.CLITools.Items[0].ReceivedAt = time.Time{}
		}, nil},
		// 這一格釘的是 machine_evidence.go:450 的
		// tool.SourcesDisagree && (tool.VersionReported == nil || tool.VersionPackageJSON == nil)。
		// canonical fixture 的 alpha（machine_evidence_test.go:90-93）本來就是
		// VersionReported="1.0.0"、VersionPackageJSON="1.0.1"、SourcesDisagree=true。
		// 把 package 那一邊拿掉之後，SourcesDisagree 仍然是 true，但只剩一個來源。
		// 這是單欄位的差別。
		//
		// 謊：cmd/clawctl-hub/machinescmd.go:628-629 會印
		// VERSION_PACKAGE_JSON: unknown（machineEvidenceOptionalText 對 nil 回 "unknown"，
		// 見 machinescmd.go:827-831）配上 VERSION_SOURCES_DISAGREE: true。
		// 宣稱兩個來源互相矛盾，而其中一個是 unknown——沒有東西可以互相矛盾。
		//
		// ⚠ 實測 18 條子句普查（逐條換成 false，全樹 go test ./... -count=1）：
		// 這一條全綠，在這個 case 之前沒有任何看守者。
		// ⚠ 另一支 VersionReported == nil 是同一條子句的另一個 limb，
		// 關掉這條子句兩邊都會放行，所以只補一格就夠，不另外湊一格。
		//
		// ⚠ Hub 側會自己擋：internal/store/machine_evidence.go:599 用的是
		// tool.SourcesDisagree != sourcesDisagree 的對稱相等比對，少一邊時
		// sourcesDisagree 算出來是 false，true != false 成立，列會被丟掉。
		// 所以這一條跟這張表其它 case 一樣，守的是「Hub 對 CLI 說謊」那個方向。
		//
		// ⚠ 實測隔離盤（這一格落地後，全樹 go test ./... -count=1）：
		// 關掉 :450，只有本 case 紅、沒有別的子測試紅。
		// 對照組：關掉 :451，本 case 綠，只有 CLI_conflict_flag_lost 紅；
		// 關掉 :449，本 case 綠，只有
		// CLI_running_relationship_without_an_observed_process 紅。
		// ⚠ 同一盤額外量到：把 store 側 :599 的對稱比對關掉
		// （用 && false，保留 sourcesDisagree 的讀取，否則會編譯失敗），
		// 全樹仍然全綠——Hub 側那一道雖然擋得住，自己卻沒有任何看守者。
		// 那是另一刀，走的是 RecordObservation 這條真正的 agent payload 路徑。
		{"CLI tool claims a conflict with only one source", func(v *operator.MachineEvidenceResult) {
			v.CLITools.Items[0].VersionPackageJSON = nil
		}, nil},
		// 這一族守的是「Hub 給了這個 CLI 不認得的列舉值」。同族的
		// process_scan、running_relationship、credential status 早就有人守，
		// 這六個是漏掉的成員，不是設計選擇。放行的話
		// cmd/clawctl-hub/machinescmd.go:617-632 會用 terminalSafe 把契約外的字
		// 逐字印給 operator 當證據。
		{"CLI present evidence unknown", func(v *operator.MachineEvidenceResult) {
			v.CLITools.Items[0].PresentEvidence = "invented"
		}, nil},
		{"CLI path source unknown", func(v *operator.MachineEvidenceResult) {
			v.CLITools.Items[0].PathSource = "invented"
		}, nil},
		{"CLI daemon reach unknown", func(v *operator.MachineEvidenceResult) {
			v.CLITools.Items[0].DaemonReach = "invented"
		}, nil},
		{"CLI support level unknown", func(v *operator.MachineEvidenceResult) {
			v.CLITools.Items[0].Support = model.SupportLevel("invented")
		}, nil},
		// 這裡釘的是 machine_evidence.go:449 的
		// RunningRelationship != "" ⇒ Present && ProcessObserved。
		//
		// ⚠ 實測 18 條子句普查（逐條換成 false，跑全樹 go test ./... -count=1）：
		// 這一條全綠，在這個 case 之前沒有任何看守者。同一次普查裡，值域那六條
		// （present_evidence／path_source／daemon_reach／process_scan／
		// running_relationship／support）都有人守，紅的是這支測試自己的 unknown
		// 那一族——值域有人守不等於一致性有人守。
		//
		// RunningRelationship 的封閉值域是 ""／"same"／"different"。"different"
		// 的意思是「正在跑的那個檔案跟 PATH 上那個不是同一個」——那是 shadowed
		// 安裝的警訊。宣稱它，卻同時說沒有觀測到任何 process，是一個沒有來源的正面
		// 斷言。cmd/clawctl-hub/machinescmd.go:619-631 會印
		// RUNNING_RELATIONSHIP: "different" 配 PROCESS_OBSERVED: false。
		//
		// ⚠ agent 送不出這個謊：model.CLITool 根本沒有 RunningRelationship 這個欄位。
		// 這個值是在 Hub 的投影層才誕生的（internal/store/machine_evidence.go:352
		// 的 machineReadCLIRunningRelationship(tool)），而那支函式 RealPath 或
		// running 為空就回 ""，所以誠實的 Hub 也造不出這一列。這一條守的是
		// 「Hub 對 CLI 說謊」那個方向——跟這張表其它每一個 case 同一個威脅模型。
		//
		// ⚠ 實測隔離盤（這個 case 落地後，全樹 go test ./... -count=1）：
		// 把 :449 這一條換成 false，只有這個 case 紅，沒有別的子測試紅。
		// 對照組：關掉 :445 的 !Present 無證據、:448 的 process 伴隨條件，
		// 兩格都全綠——那兩條至今仍無看守者，刻意沒補。
		// 對照組：關掉 :447 的 path 伴隨條件，本 case 綠，
		// 只有 CLI_path_evidence_without_on_path 紅。
		// 對照組：關掉 validMachineEvidenceRunningRelationship 值域檢查，
		// 本 case 綠，只有 CLI_running_relationship_unknown 紅——
		// 值域跟一致性是兩道不同的門，量的時候不能混。
		// ⚠ 有一臂不算數：把 :352 改成採信 tool.RunningRelationship 會編譯失敗
		// （model.CLITool 沒有這個欄位），那是編譯錯誤不是看守者。
		{"CLI running relationship without an observed process", func(v *operator.MachineEvidenceResult) {
			v.CLITools.Items[1].RunningRelationship = "different"
		}, nil},
		{"OpenClaw database layout unknown", func(v *operator.MachineEvidenceResult) {
			v.OpenClaw.DB.Layout = &operator.EvidenceText{
				Text: "invented", MaxBytes: 64, Bytes: len("invented"), Issues: []string{},
			}
		}, nil},
		{"credential verification method unknown", func(v *operator.MachineEvidenceResult) {
			v.Credentials.Items[0].VerificationMethod = model.VerifyMethod("invented")
		}, nil},
		// ⚠ 這一格的時間**一定要**從同一列身上拿。我第一次寫 time.Unix(1, 0)，
		// 把 :546 那一項短路掉之後全樹仍然全綠——它是被 UTC／零值時間那一關
		// 擋掉的，不是被「有時間戳但方法不是 live」擋掉的。改用該列自己的
		// MeasuredAt 之後，短路那一項就只有這一筆紅。換成任何字面量都會讓
		// 這一筆變回假見證。
		{"credential verified without a live request", func(v *operator.MachineEvidenceResult) {
			v.Credentials.Items[0].VerifiedAt = machineEvidenceClientTimePtr(v.Credentials.Items[0].MeasuredAt)
		}, nil},
		{"credential claims a live request with no proof", func(v *operator.MachineEvidenceResult) {
			v.Credentials.Items[0].VerificationMethod = model.VerifyLiveRequest
		}, nil},
		{"unknown field", nil, func(raw []byte) []byte {
			return append(raw[:len(raw)-1], []byte(`,"lease_token":"secret"}`)...)
		}},
		{"systemd main pid field", nil, func(raw []byte) []byte {
			return bytes.Replace(raw, []byte(`"present":true`), []byte(`"present":true,"main_pid":987654`), 1)
		}},
		{"credential active account id field", nil, func(raw []byte) []byte {
			return bytes.Replace(raw, []byte(`"active_account_selected":true`),
				[]byte(`"active_account_selected":true,"active_account_id":"secret"`), 1)
		}},
		{"occupancy profile id field", nil, func(raw []byte) []byte {
			return bytes.Replace(raw, []byte(`"runs":1`), []byte(`"runs":1,"profile_id":"secret"`), 1)
		}},
		{"CLI host path field", nil, func(raw []byte) []byte {
			return bytes.Replace(raw, []byte(`"present":true,"on_path":true`),
				[]byte(`"present":true,"on_path":true,"path":"/secret/bin"`), 1)
		}},
		{"OpenClaw database path field", nil, func(raw []byte) []byte {
			return bytes.Replace(raw, []byte(`"present":false,"reason":null,"layout":null`),
				[]byte(`"present":false,"path":"/secret/db","reason":null,"layout":null`), 1)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var value operator.MachineEvidenceResult
			if err := json.Unmarshal(canonicalJSON, &value); err != nil {
				t.Fatal(err)
			}
			if test.mutate != nil {
				test.mutate(&value)
			}
			raw, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			if test.raw != nil {
				raw = test.raw(raw)
			}
			client, _ := jobEvidenceRecorderClient(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Cache-Control", "no-store")
				_, _ = w.Write(raw)
			})
			if _, err := client.MachineEvidence(t.Context(), machineID, 2); err == nil {
				t.Fatal("incoherent machine evidence was accepted")
			}
		})
	}

	client, requests = jobEvidenceRecorderClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	for _, invalid := range []struct {
		machineID string
		limit     int
	}{{machineID, 101}, {" machine", 0}, {"machine\nid", 0}, {string([]byte{'m', 0xff}), 0},
		{strings.Repeat("m", 257), 0}} {
		if _, err := client.MachineEvidence(t.Context(), invalid.machineID, invalid.limit); err == nil {
			t.Fatalf("invalid request %+v was accepted", invalid)
		}
	}
	if requests.Load() != 0 {
		t.Fatalf("invalid inputs performed %d HTTP requests", requests.Load())
	}
}

func machineEvidenceClientTimePtr(value time.Time) *time.Time { return &value }
