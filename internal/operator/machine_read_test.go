package operator

import (
	"bytes"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/teddashh/AI-Intune/internal/expect"
	"github.com/teddashh/AI-Intune/internal/model"
	"github.com/teddashh/AI-Intune/internal/state"
	"github.com/teddashh/AI-Intune/internal/store"
)

func TestMachineReadProjectionKeepsUnknownAndRetiredExplicit(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	now := time.Now().UTC().Truncate(time.Second)
	neverID, _, err := st.CreateEnrollTokenFor("never-reported", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	reportingID, token, err := st.CreateEnrollTokenFor("reporting", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.RedeemEnrollToken(token, model.EnrollRequest{
		SchemaVersion: model.SchemaVersion, EnrollToken: token,
		Hostname: "private-host", UnixUser: "private-user", OS: "linux", Arch: "amd64",
	}, now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordCheckin(reportingID, model.Checkin{
		SchemaVersion: model.SchemaVersion, SentAt: now.Add(-30 * time.Second),
		AgentVersion: "v-test", AgentStartedAt: now.Add(-time.Hour), AgentSeq: 7,
	}, now.Add(-30*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordObservation(reportingID, model.ObservationBatch{
		SchemaVersion: model.SchemaVersion, MeasuredAt: now.Add(-20 * time.Second),
		OpenClaw: model.OpenClaw{Present: true},
	}, now.Add(-20*time.Second)); err != nil {
		t.Fatal(err)
	}
	retiredID, _, err := st.CreateEnrollTokenFor("retired", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	// CreateEnrollTokenFor uses the Store clock. Keep this lifecycle event at or
	// after that immutable registry creation coordinate.
	if err := st.RetireMachine(retiredID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	unmanagedID, unmanagedToken, err := st.CreateEnrollTokenFor("visible-unmanaged", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.RedeemEnrollToken(unmanagedToken, model.EnrollRequest{
		SchemaVersion: model.SchemaVersion, EnrollToken: unmanagedToken,
		Hostname: "unmanaged-private-host", UnixUser: "unmanaged-private-user", OS: "linux", Arch: "amd64",
	}, now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordCheckin(unmanagedID, model.Checkin{
		SchemaVersion: model.SchemaVersion, SentAt: now.Add(-15 * time.Second), AgentSeq: 1,
	}, now.Add(-15*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`UPDATE machine_registry SET expected = 0 WHERE machine_id = ?`, unmanagedID); err != nil {
		t.Fatal(err)
	}

	evaluatedAt := now.Add(987 * time.Millisecond)
	result, err := New(st).ListMachines(evaluatedAt)
	if err != nil {
		t.Fatal(err)
	}
	if result.SchemaVersion != MachineReadSchemaVersion || !result.EvaluatedAt.Equal(evaluatedAt) ||
		result.Total != 4 || result.Active != 3 || result.Retired != 1 ||
		result.Expected != 3 || result.Reporting != 2 || result.NextCursor != nil {
		t.Fatalf("list envelope=%+v", result)
	}
	if len(result.Items) != 4 || len(result.StateCounts) != len(state.AllStates) ||
		len(result.DenominatorStateCounts) != len(state.AllStates) {
		t.Fatalf("list lengths items=%d state=%d denominator=%d", len(result.Items),
			len(result.StateCounts), len(result.DenominatorStateCounts))
	}
	byID := make(map[string]MachineSummary, len(result.Items))
	for _, item := range result.Items {
		byID[item.MachineID] = item
	}
	never := byID[neverID]
	if never.State == nil || *never.State != state.NeverReported ||
		never.Reporting == nil || *never.Reporting || never.LastCheckinReceivedAt != nil ||
		never.LastObservationReceivedAt != nil {
		t.Fatalf("never-reported projection=%+v", never)
	}
	reporting := byID[reportingID]
	if reporting.Reporting == nil || !*reporting.Reporting || reporting.LastCheckinReceivedAt == nil ||
		reporting.LastObservationReceivedAt == nil {
		t.Fatalf("reporting projection=%+v", reporting)
	}
	retired := byID[retiredID]
	if retired.RetiredAt == nil || retired.State != nil ||
		retired.StateSince != nil || retired.Reporting != nil || retired.LastCheckinReceivedAt != nil ||
		retired.LastObservationReceivedAt != nil {
		t.Fatalf("retired projection=%+v", retired)
	}
	unmanaged := byID[unmanagedID]
	if unmanaged.Expected || unmanaged.State == nil ||
		unmanaged.Reporting == nil || !*unmanaged.Reporting {
		t.Fatalf("visible unmanaged projection=%+v", unmanaged)
	}
	if overview, ok := result.StoreOverview(); !ok || overview.Now.IsZero() {
		t.Fatal("trusted HTML overview bridge was not retained")
	}

	assertMachineReadJSONAllowlist(t, result)
	detail, err := New(st).MachineDetail(reportingID, now)
	if err != nil {
		t.Fatal(err)
	}
	connect, connectErr := New(st).MachineConnect(detail)
	if detail.Item.MachineID != reportingID || detail.Item.Reporting == nil || !*detail.Item.Reporting ||
		connectErr != nil || connect.MachineID != reportingID || detail.Judgement.Findings.Items == nil {
		t.Fatalf("detail=%+v", detail)
	}
	assertMachineReadJSONAllowlist(t, detail)
}

// store 層已有截斷測試；這裡防的是 operator 投影把旗標丟掉。
func TestMachineDetailProjectsCheckinTruncation(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	now := time.Now().UTC().Truncate(time.Second)
	machineID, token, err := st.CreateEnrollTokenFor("bounded-checkins", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.RedeemEnrollToken(token, model.EnrollRequest{
		SchemaVersion: model.SchemaVersion, EnrollToken: token,
		Hostname: "bounded-checkins", UnixUser: "operator", OS: "linux", Arch: "amd64",
	}, now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	oldest := now.Add(-time.Duration(store.DetailCheckinLimit) * time.Second)
	for i := 0; i <= store.DetailCheckinLimit; i++ {
		at := oldest.Add(time.Duration(i) * time.Second)
		if err := st.RecordCheckin(machineID, model.Checkin{
			SchemaVersion: model.SchemaVersion, SentAt: at, AgentVersion: "v3",
			BootID: "boot-one", AgentSeq: int64(i + 1), AgentStartedAt: now.Add(-time.Hour),
			UptimeSeconds: func() *int64 { v := int64(100 + i); return &v }(), DiskFreeBytes: 50, DiskTotalBytes: 100,
		}, at); err != nil {
			t.Fatal(err)
		}
	}

	result, err := New(st).MachineDetail(machineID, now)
	if err != nil {
		t.Fatal(err)
	}
	page := result.Checkins
	if !page.Truncated || page.RowsSeen != store.DetailCheckinLimit ||
		len(page.Items) != store.DetailCheckinLimit || page.Invalid != 0 {
		t.Fatalf("bounded checkins=%+v", page)
	}
}

func TestMachineReadUsesExactEvaluationInstantAndOmitsFreeFormEvidence(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	receivedAt := time.Now().UTC().Truncate(time.Second).Add(-time.Minute)
	machineID, token, err := st.CreateEnrollTokenFor("escape-\x1b]2;owned\x07", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.RedeemEnrollToken(token, model.EnrollRequest{
		SchemaVersion: model.SchemaVersion, EnrollToken: token,
		Hostname: "private-host", UnixUser: "private-user", OS: "linux", Arch: "amd64",
	}, receivedAt); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordCheckin(machineID, model.Checkin{
		SchemaVersion: model.SchemaVersion, SentAt: receivedAt,
		AgentVersion: "private-agent-version-\x1b[31m", AgentSeq: 1,
	}, receivedAt); err != nil {
		t.Fatal(err)
	}
	const privatePath = "/private/operator/evidence/sentinel"
	const privateWhy = "private expectation why sentinel"
	st.SetExpectations(&expect.Set{Configured: true, Rules: []model.Expectation{{
		Machine: "*", Unit: "private-unit", Artifact: privatePath,
		Why: privateWhy, MaxAgeSeconds: 60,
	}}})

	evaluatedAt := receivedAt.Add(state.CheckinInterval + state.UnreachableGrace + 500*time.Millisecond)
	result, err := New(st).ListMachines(evaluatedAt)
	if err != nil {
		t.Fatal(err)
	}
	if !result.EvaluatedAt.Equal(evaluatedAt) || len(result.Items) != 1 ||
		result.Items[0].State == nil || *result.Items[0].State != state.Unreachable ||
		result.Items[0].Reporting == nil || *result.Items[0].Reporting {
		t.Fatalf("exact boundary result=%+v", result)
	}
	if result.Items[0].DisplayName != "escape-�]2;owned�" ||
		strings.Join(result.Items[0].Issues, ",") != "display_name_control_or_format_replaced" ||
		strings.Join(result.Items[0].AlteredFields, ",") != "display_name" {
		t.Fatalf("unsafe display name projection=%+v", result.Items[0])
	}
	if overview, ok := result.StoreOverview(); !ok || len(overview.Machines) != 1 ||
		overview.Machines[0].DisplayName != result.Items[0].DisplayName {
		t.Fatalf("safe overview bridge=%+v available=%t", overview, ok)
	} else {
		for _, finding := range overview.Findings {
			if finding.DisplayName != result.Items[0].DisplayName {
				t.Fatalf("overview finding retained raw display name: %+v", finding)
			}
		}
	}
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	for _, sentinel := range [][]byte{[]byte(privatePath), []byte(privateWhy), []byte("private-agent-version")} {
		if bytes.Contains(raw, sentinel) {
			t.Fatalf("safe machine DTO leaked free-form sentinel %q: %s", sentinel, raw)
		}
	}
	assertMachineReadJSONAllowlist(t, result)
}

func TestMachineDetailProjectsTypedMonitorIdentityResourcesAndHistory(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	evaluatedAt := time.Date(2026, 9, 9, 18, 0, 0, 0, time.UTC)
	machineID, token, err := st.CreateEnrollTokenFor("typed-detail", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.RedeemEnrollToken(token, model.EnrollRequest{
		SchemaVersion: model.SchemaVersion, EnrollToken: token,
		Hostname: "enrolled-host", MachineIDHint: "registry-machine-id", UnixUser: "operator",
		OS: "linux", Arch: "amd64", AgentVersion: "old",
	}, evaluatedAt.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	age := int64(17)
	const artifactPath = "/private/typed-detail/proof.jsonl"
	st.SetExpectations(&expect.Set{Configured: true, Rules: []model.Expectation{{
		Machine: "typed-detail", Unit: "proof.service", Artifact: artifactPath,
		MaxAgeSeconds: 3600, Why: "independent verifier output",
		Events: &model.EventSpec{TsField: "at", TypeField: "kind", WindowSeconds: 1800,
			NotOK: []string{"verification_failed", "policy_mismatch"}},
	}}})
	receivedAt := evaluatedAt.Add(-30 * time.Second)
	if err := st.RecordCheckin(machineID, model.Checkin{
		SchemaVersion: model.SchemaVersion, SentAt: receivedAt.Add(5 * time.Second),
		AgentVersion: "agent-\x1b[31m-v3", BootID: "boot-one", AgentSeq: 42,
		AgentStartedAt: evaluatedAt.Add(-2 * time.Hour), UptimeSeconds: func() *int64 { v := int64(7200); return &v }(),
		DiskFreeBytes: 40 << 30, DiskTotalBytes: 100 << 30, ObservationAgeSeconds: &age,
	}, receivedAt); err != nil {
		t.Fatal(err)
	}
	observedAt := evaluatedAt.Add(-20 * time.Second)
	modifiedAt := evaluatedAt.Add(-time.Minute).In(time.FixedZone("offset", -4*60*60))
	lastAt := evaluatedAt.Add(-2 * time.Minute).In(time.FixedZone("offset", 9*60*60))
	coveredFrom := evaluatedAt.Add(-10 * time.Minute).In(time.FixedZone("offset", 2*60*60))
	if err := st.RecordObservation(machineID, model.ObservationBatch{
		SchemaVersion: model.SchemaVersion, MeasuredAt: observedAt.Add(2 * time.Second),
		Identity: model.Identity{
			Hostname: "host-\x1b]2;owned\x07", OS: "linux", Kernel: "6.8", Arch: "amd64",
			UnixUser: "example-user", MachineIDHint: "observed-machine-id", BootID: "boot-one",
			TailscaleIP: "100.64.0.9", LingerEnabled: true,
		},
		Resources: model.Resources{
			DiskFreeBytes: 40 << 30, DiskTotalBytes: 100 << 30,
			MemAvailableBytes: 8 << 30, MemTotalBytes: 16 << 30, CPUCount: 8, Load1m: func() *float64 { v := 0.5; return &v }(),
		},
		Artifacts: []model.ArtifactCheck{{
			Unit: "proof.service", Artifact: artifactPath, Exists: true, ModTime: &modifiedAt,
		}},
		Events: []model.EventStream{{
			Unit: "proof.service", Path: artifactPath, Truncated: true, CoveredFrom: &coveredFrom,
			Declared: []model.EventSummary{{Type: "verification_failed", Count: 1, LastAt: &lastAt},
				{Type: "policy_mismatch", Count: 0}},
			Undeclared:      []model.EventSummary{{Type: "new_enum", Count: 2, LastAt: &lastAt}},
			UndeclaredTotal: 1, Malformed: 1,
		}},
	}, observedAt); err != nil {
		t.Fatal(err)
	}
	const stateReason = "display only /private/state/path\nline \x1b[31m"
	if _, err := st.DB().Exec(`INSERT INTO machine_state_history(machine_id,state,reason,entered_at)
VALUES (?,?,?,?)`, machineID, string(state.IdentityConflict), stateReason,
		evaluatedAt.Add(-10*time.Minute).Format("2006-01-02T15:04:05Z")); err != nil {
		t.Fatal(err)
	}

	result, err := New(st).MachineDetail(machineID, evaluatedAt)
	if err != nil {
		t.Fatal(err)
	}
	if result.SchemaVersion != MachineDetailReadSchemaVersion || result.Disclosure.IndependentVerifier ||
		!result.Disclosure.LivenessUsesReceivedAt || result.Disclosure.AgentSentAtIsLiveness ||
		!result.Disclosure.ConnectCoordinatesExcluded || !result.Disclosure.PendingEnrollmentExcluded ||
		!result.Disclosure.ExpectationDisplayDefinitionsIncluded || !result.Disclosure.ExpectationConfigPathFieldExcluded ||
		!result.Disclosure.ExpectationParserFieldsExcluded || !result.Disclosure.ArtifactPathsIncluded ||
		result.Disclosure.ArtifactContentInspected || result.Disclosure.ArtifactFreshnessIsWorkOutcome ||
		!result.Disclosure.EventFailureTypesOperatorDeclared || result.Disclosure.EventContentKeywordScanning ||
		result.Disclosure.ExpectationTextPathRedacted || result.Disclosure.ExpectationTextSecretRedacted ||
		!result.Disclosure.JudgementDerivedByHub ||
		!result.Disclosure.JudgementMayUseUnverifiedInput || result.Disclosure.JudgementTextParsedByHub ||
		result.Disclosure.JudgementTextPathRedacted || result.Disclosure.JudgementTextSecretRedacted ||
		!result.Disclosure.KnownSecretFieldsExcluded || result.Disclosure.StateReasonParsedByHub ||
		result.Disclosure.StateReasonPathRedactedByHub || result.Disclosure.CompleteHistoryClaimed {
		t.Fatalf("detail disclosure=%+v", result.Disclosure)
	}
	if result.Item.State == nil || result.Judgement.State != *result.Item.State || !result.Judgement.AffectsFleetState ||
		result.Judgement.Reason.Text == "" || result.Judgement.Findings.Total == 0 ||
		result.Judgement.Findings.Invalid != 0 || result.Judgement.Findings.Truncated ||
		len(result.Judgement.Findings.Items) == 0 {
		t.Fatalf("judgement=%+v", result.Judgement)
	}
	if !result.Expectations.Configured || result.Expectations.ReadFailed || result.Expectations.Rules.Total != 1 ||
		len(result.Expectations.Rules.Items) != 1 {
		t.Fatalf("expectations=%+v", result.Expectations)
	}
	rule := result.Expectations.Rules.Items[0]
	modifiedOffset := 1
	if rule.Observation.ModifiedAt != nil {
		_, modifiedOffset = rule.Observation.ModifiedAt.Zone()
	}
	if rule.Unit.Text != "proof.service" || rule.Artifact.Text != artifactPath ||
		rule.Why.Text != "independent verifier output" || rule.MaxAgeSeconds != 3600 ||
		!rule.Observation.Observed || !rule.Observation.Decoded || rule.Observation.Invalid ||
		rule.Observation.Exists == nil || !*rule.Observation.Exists || rule.Observation.ModifiedAt == nil ||
		modifiedOffset != 0 || rule.Events == nil ||
		rule.Events.FailureTypes.Total != 2 || len(rule.Events.FailureTypes.Items) != 2 ||
		!rule.Events.Observed || !rule.Events.Decoded || rule.Events.Invalid ||
		rule.Events.Declared.Total != 2 || rule.Events.Undeclared.Total != 1 || rule.Events.Malformed != 1 {
		t.Fatalf("expectation rule=%+v", rule)
	}
	if result.Monitor.AgentVersion == nil || strings.Contains(result.Monitor.AgentVersion.Text, "\x1b") ||
		result.Monitor.DistinctAgentStarts1h == nil || *result.Monitor.DistinctAgentStarts1h != 1 ||
		result.Monitor.DistinctBootIDs1h == nil || *result.Monitor.DistinctBootIDs1h != 1 ||
		result.Monitor.ClockSkewSeconds == nil || *result.Monitor.ClockSkewSeconds != 5 {
		t.Fatalf("monitor=%+v", result.Monitor)
	}
	if result.Checkins.RowsSeen != 1 || result.Checkins.Invalid != 0 || result.Checkins.Truncated ||
		len(result.Checkins.Items) != 1 || result.Checkins.Items[0].AgentSeq == nil ||
		*result.Checkins.Items[0].AgentSeq != 42 || result.Checkins.Items[0].UptimeSeconds == nil ||
		*result.Checkins.Items[0].UptimeSeconds != 7200 || result.Checkins.Items[0].BootID == nil ||
		result.Checkins.Items[0].BootID.Text != "boot-one" {
		t.Fatalf("checkins=%+v", result.Checkins)
	}
	if !result.Identity.Observed || !result.Identity.Decoded || result.Identity.ObservedAt == nil ||
		result.Identity.Value == nil || strings.Contains(result.Identity.Value.Hostname.Text, "\x1b") ||
		result.Identity.Value.TailscaleIP == nil || result.Identity.Value.TailscaleIP.Text != "100.64.0.9" ||
		result.IdentityHints.Total != 2 || len(result.IdentityHints.Items) != 2 {
		t.Fatalf("identity=%+v hints=%+v", result.Identity, result.IdentityHints)
	}
	if !result.Resources.Observed || !result.Resources.Decoded || result.Resources.Invalid ||
		result.Resources.Value == nil || result.Resources.Value.CPUCount != 8 {
		t.Fatalf("resources=%+v", result.Resources)
	}
	if result.StateHistory.RowsSeen != 1 || len(result.StateHistory.Items) != 1 ||
		result.StateHistory.Items[0].Reason.Text != "display only /private/state/path\nline �[31m" {
		t.Fatalf("state history=%+v", result.StateHistory)
	}
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"agent_token", "agent_token_hash", `"connect":`, `"pending_token":`,
		`"path":`, `"argv":`} {
		if bytes.Contains(raw, []byte(forbidden)) {
			t.Fatalf("typed detail leaked structurally excluded field %q: %s", forbidden, raw)
		}
	}
	if !bytes.Contains(raw, []byte("/private/state/path")) {
		t.Fatalf("display-only reason was unexpectedly path-scanned or redacted: %s", raw)
	}
	// A partial nullable pair is corrupt evidence, not an unknown disk sample.
	if _, err := st.DB().Exec(`UPDATE machine_checkins SET disk_total_bytes=NULL WHERE machine_id=?`, machineID); err != nil {
		t.Fatal(err)
	}
	malformed, err := New(st).MachineDetail(machineID, evaluatedAt)
	if err != nil {
		t.Fatal(err)
	}
	if malformed.Checkins.RowsSeen != 1 || malformed.Checkins.Invalid != 1 ||
		len(malformed.Checkins.Items) != 0 {
		t.Fatalf("partial disk check-in was not isolated as invalid: %+v", malformed.Checkins)
	}
}

func TestMachineDetailBoundsRetiredJudgementWithoutScanningDisplayText(t *testing.T) {
	evaluatedAt := time.Date(2026, 9, 9, 19, 0, 0, 0, time.UTC)
	retiredAt := evaluatedAt.Add(-time.Hour)
	findings := []state.Finding{{Kind: "future-kind", Severity: 3, Message: "invalid shape"}}
	for range MachineDetailFindingLimit + 1 {
		findings = append(findings, state.Finding{
			Kind: "workload", Severity: 3,
			Message: "display only /private/expectation/path\nline \x1b[31m",
		})
	}
	detail := store.Detail{
		Machine: store.Machine{
			MachineID: "retired-machine", DisplayName: "retired", RetiredAt: &retiredAt,
		},
		State:            state.Unreachable,
		Reason:           "display only /private/reason/path\nline \x1b[31m",
		Findings:         findings,
		ExpectConfigured: true,
	}
	for index := range MachineDetailExpectationLimit + 1 {
		unit := strings.Repeat("u", index+1)
		fact := state.ArtifactFact{Unit: unit, Artifact: "/" + unit, Why: "bounded definition", MaxAge: time.Minute}
		if index == 0 {
			fact.Events = &state.EventFact{Window: time.Hour}
			for eventIndex := range MachineDetailEventTypeLimit + 1 {
				fact.Events.FailureTypes = append(fact.Events.FailureTypes, strings.Repeat("e", eventIndex+1))
			}
		}
		detail.Facts.Artifacts = append(detail.Facts.Artifacts, fact)
	}
	result := MachineDetailResult{
		EvaluatedAt: evaluatedAt,
		Item:        MachineSummary{MachineID: detail.Machine.MachineID, DisplayName: detail.Machine.DisplayName},
	}
	projectMachineDetailSections(&result, detail)

	page := result.Judgement.Findings
	if result.Judgement.AffectsFleetState || result.Judgement.State != state.Unreachable ||
		page.Total != MachineDetailFindingLimit+2 || page.Invalid != 1 || !page.Truncated ||
		len(page.Items) != MachineDetailFindingLimit {
		t.Fatalf("retired judgement=%+v", result.Judgement)
	}
	if !strings.Contains(result.Judgement.Reason.Text, "/private/reason/path") ||
		strings.Contains(result.Judgement.Reason.Text, "\x1b") ||
		!strings.Contains(page.Items[0].Message.Text, "/private/expectation/path") ||
		strings.Contains(page.Items[0].Message.Text, "\x1b") {
		t.Fatalf("display-only judgement text was scanned or left unsafe: reason=%+v message=%+v",
			result.Judgement.Reason, page.Items[0].Message)
	}
	expectations := result.Expectations.Rules
	if expectations.Total != MachineDetailExpectationLimit+1 || !expectations.Truncated ||
		len(expectations.Items) != MachineDetailExpectationLimit || expectations.Items[0].Events == nil ||
		expectations.Items[0].Events.FailureTypes.Total != MachineDetailEventTypeLimit+1 ||
		!expectations.Items[0].Events.FailureTypes.Truncated ||
		len(expectations.Items[0].Events.FailureTypes.Items) != MachineDetailEventTypeLimit {
		t.Fatalf("bounded expectations=%+v", expectations)
	}
}

func TestMachineListFiltersPagingCreationCeilingAndCursorBinding(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	evaluatedAt := time.Date(2026, 9, 8, 18, 0, 0, 0, time.UTC)
	for _, machine := range []store.Machine{
		{MachineID: "machine-a", DisplayName: "Alpha", Expected: true, CreatedAt: evaluatedAt.Add(-3 * time.Hour)},
		{MachineID: "machine-b", DisplayName: "Bravo", Expected: true, CreatedAt: evaluatedAt.Add(-2 * time.Hour)},
		{MachineID: "machine-c", DisplayName: "Charlie", Expected: false, Channel: "stable", CreatedAt: evaluatedAt.Add(-time.Hour)},
	} {
		if err := st.UpsertMachine(machine); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.DB().Exec(`UPDATE machine_registry SET channel='stable' WHERE machine_id='machine-c'`); err != nil {
		t.Fatal(err)
	}
	service := New(st)
	first, err := service.ListMachinesPage(MachineListRequest{Limit: 1}, evaluatedAt)
	if err != nil {
		t.Fatal(err)
	}
	if first.Total != 3 || first.MatchedTotal != 3 || len(first.Items) != 1 ||
		first.NextCursor == nil || first.CreationCeiling < 3 || first.Consistency != MachineReadConsistency {
		t.Fatalf("first page=%+v", first)
	}
	seen := map[string]bool{first.Items[0].MachineID: true}

	// This row deliberately sorts behind the first page. Its higher immutable
	// registry sequence must still keep it outside this traversal.
	if err := st.UpsertMachine(store.Machine{
		MachineID: "machine-inserted-late", DisplayName: "Late insert", Expected: true,
		CreatedAt: evaluatedAt.Add(-24 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	second, err := service.ListMachinesPage(MachineListRequest{Limit: 2, Cursor: *first.NextCursor}, evaluatedAt.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if second.Total != 3 || second.MatchedTotal != 3 || second.CreationCeiling != first.CreationCeiling ||
		len(second.Items) != 2 || second.NextCursor != nil {
		t.Fatalf("second page=%+v", second)
	}
	if overview, ok := second.StoreOverview(); !ok || overview.Total != 3 || len(overview.Machines) != 3 {
		t.Fatalf("creation-ceiling overview bridge=%+v available=%t", overview, ok)
	}
	for _, item := range second.Items {
		if seen[item.MachineID] || item.MachineID == "machine-inserted-late" {
			t.Fatalf("creation-ceiling traversal duplicated or admitted late row: %+v", item)
		}
		seen[item.MachineID] = true
	}
	if len(seen) != 3 {
		t.Fatalf("traversal saw ids=%v", seen)
	}
	if _, err := service.ListMachinesPage(MachineListRequest{
		Limit: 1, Cursor: *first.NextCursor, Reporting: MachineReportingTrue,
	}, evaluatedAt); !errors.Is(err, ErrInvalidMachineRead) {
		t.Fatalf("filter-mismatched cursor error=%v", err)
	}
	if _, err := service.ListMachinesPage(MachineListRequest{
		Limit: 1, Cursor: *first.NextCursor + "A",
	}, evaluatedAt); !errors.Is(err, ErrInvalidMachineRead) {
		t.Fatalf("non-canonical cursor error=%v", err)
	}

	filtered, err := service.ListMachinesPage(MachineListRequest{
		DisplayName: "Charlie", States: []state.State{state.NeverReported},
		Lifecycle: MachineLifecycleActive,
		Reporting: MachineReportingFalse, Channel: MachineChannelStable,
	}, evaluatedAt)
	if err != nil {
		t.Fatal(err)
	}
	if filtered.Total != 4 || filtered.MatchedTotal != 1 || len(filtered.Items) != 1 ||
		filtered.Items[0].MachineID != "machine-c" {
		t.Fatalf("combined filter=%+v", filtered)
	}
	if err := ValidateMachineListRequest(MachineListRequest{
		States: []state.State{state.Online, state.Online},
	}); !errors.Is(err, ErrInvalidMachineRead) {
		t.Fatalf("duplicate states validation error=%v", err)
	}
}

func TestMachineReadEvidenceUsesHubReceivedAtCutoff(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	evaluatedAt := time.Date(2026, 9, 8, 19, 0, 0, 0, time.UTC)
	machineID, token, err := st.CreateEnrollTokenFor("cutoff-machine", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.RedeemEnrollToken(token, model.EnrollRequest{
		SchemaVersion: model.SchemaVersion, EnrollToken: token,
		Hostname: "private-host", UnixUser: "private-user", OS: "linux", Arch: "amd64",
	}, evaluatedAt.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	oldReceived := evaluatedAt.Add(-30 * time.Second)
	futureReceived := evaluatedAt.Add(30 * time.Second)
	if err := st.RecordCheckin(machineID, model.Checkin{
		SchemaVersion: model.SchemaVersion, SentAt: oldReceived, AgentSeq: 1,
	}, oldReceived); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordObservation(machineID, model.ObservationBatch{
		SchemaVersion: model.SchemaVersion, MeasuredAt: oldReceived,
		Identity: model.Identity{MachineIDHint: "identity-a", LingerEnabled: true},
		OpenClaw: model.OpenClaw{Present: true},
	}, oldReceived); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordCheckin(machineID, model.Checkin{
		SchemaVersion: model.SchemaVersion, SentAt: futureReceived, AgentSeq: 2,
	}, futureReceived); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordObservation(machineID, model.ObservationBatch{
		SchemaVersion: model.SchemaVersion, MeasuredAt: futureReceived,
		Identity: model.Identity{MachineIDHint: "identity-b", LingerEnabled: true},
		OpenClaw: model.OpenClaw{Present: true},
	}, futureReceived); err != nil {
		t.Fatal(err)
	}
	// Update an already-known durable hint after the evaluation cutoff too.
	// Its aggregate cannot be reconstructed exactly as-of after raw retention,
	// so the typed projection must mark it invalid rather than clamp its clock.
	if err := st.RecordObservation(machineID, model.ObservationBatch{
		SchemaVersion: model.SchemaVersion, MeasuredAt: futureReceived,
		Identity: model.Identity{MachineIDHint: "identity-a", LingerEnabled: true},
		OpenClaw: model.OpenClaw{Present: true},
	}, futureReceived); err != nil {
		t.Fatal(err)
	}

	before, err := New(st).ListMachines(evaluatedAt)
	if err != nil {
		t.Fatal(err)
	}
	if len(before.Items) != 1 || before.Items[0].LastCheckinReceivedAt == nil ||
		!before.Items[0].LastCheckinReceivedAt.Equal(oldReceived) ||
		before.Items[0].LastObservationReceivedAt == nil ||
		!before.Items[0].LastObservationReceivedAt.Equal(oldReceived) ||
		before.Items[0].State == nil || *before.Items[0].State == state.IdentityConflict {
		t.Fatalf("future evidence leaked through cutoff: %+v", before.Items)
	}
	beforeDetail, err := New(st).MachineDetail(machineID, evaluatedAt)
	if err != nil {
		t.Fatal(err)
	}
	if len(beforeDetail.Checkins.Items) != 1 || !beforeDetail.Checkins.Items[0].ReceivedAt.Equal(oldReceived) ||
		beforeDetail.Identity.Value == nil || beforeDetail.Identity.Value.MachineIDHint.Text != "identity-a" ||
		beforeDetail.Identity.ObservedAt == nil || !beforeDetail.Identity.ObservedAt.ReceivedAt.Equal(oldReceived) ||
		beforeDetail.IdentityHints.Total != 1 || beforeDetail.IdentityHints.Invalid != 1 ||
		len(beforeDetail.IdentityHints.Items) != 0 {
		t.Fatalf("typed detail leaked evidence after cutoff: %+v", beforeDetail)
	}
	after, err := New(st).ListMachines(futureReceived.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if after.Items[0].LastCheckinReceivedAt == nil ||
		!after.Items[0].LastCheckinReceivedAt.Equal(futureReceived) ||
		after.Items[0].LastObservationReceivedAt == nil ||
		!after.Items[0].LastObservationReceivedAt.Equal(futureReceived) ||
		after.Items[0].State == nil || *after.Items[0].State != state.IdentityConflict {
		t.Fatalf("later evaluation did not admit evidence: %+v", after.Items)
	}
}

func TestSafeMachineDisplayNamePreservesAllAlterationEvidence(t *testing.T) {
	raw := string([]byte{0xff}) + "\u202e" + strings.Repeat("x", 300)
	got, issues := safeMachineDisplayName(raw)
	if len(got) > 256 || !utf8.ValidString(got) || strings.Join(issues, ",") !=
		"display_name_invalid_utf8,display_name_control_or_format_replaced,display_name_truncated" {
		t.Fatalf("safe display got=%q bytes=%d issues=%v", got, len(got), issues)
	}
	for _, char := range got {
		if unicode.IsControl(char) || unicode.Is(unicode.Cf, char) {
			t.Fatalf("safe display retained unsafe rune %U", char)
		}
	}
	unnamed, unnamedIssues := safeMachineDisplayName("   ")
	if unnamed != "(unnamed machine)" || strings.Join(unnamedIssues, ",") != "display_name_missing" {
		t.Fatalf("whitespace-only display projection=%q issues=%v", unnamed, unnamedIssues)
	}
}

func TestSafeMachineOverviewScrubsDerivedAndNestedDisplayNamesWithoutMutatingStoreData(t *testing.T) {
	unsafe := "peer<script>\u202e\apeer"
	safe := "peer<script>��peer"
	overview := store.Overview{
		Machines: []store.MachineRow{
			{
				Machine: store.Machine{RegistrySequence: 1, MachineID: "target", DisplayName: "target"},
				Reason:  "同儕 " + unsafe + " 的 session 還在續",
				Facts: state.Facts{Credentials: []state.CredFact{{Peers: []state.CredPeer{{
					DisplayName: unsafe, RefreshesSeen: 2,
				}}}}},
				Findings: []state.Finding{{Kind: "credential", Message: "同儕 " + unsafe + " 的 session 還在續"}},
			},
			{Machine: store.Machine{RegistrySequence: 2, MachineID: "peer", DisplayName: unsafe}},
		},
		Findings: []store.FleetFinding{{
			MachineID: "target", DisplayName: "target",
			Finding: state.Finding{Kind: "credential", Message: "同儕 " + unsafe + " 的 session 還在續"},
		}},
	}

	projected := safeMachineOverview(overview, 2)
	if got := projected.Machines[0].Reason; !strings.Contains(got, safe) || strings.ContainsAny(got, "\u202e\a") {
		t.Fatalf("safe machine reason=%q", got)
	}
	if got := projected.Machines[0].Findings[0].Message; !strings.Contains(got, safe) || strings.ContainsAny(got, "\u202e\a") {
		t.Fatalf("safe machine finding=%q", got)
	}
	if got := projected.Findings[0].Message; !strings.Contains(got, safe) || strings.ContainsAny(got, "\u202e\a") {
		t.Fatalf("safe fleet finding=%q", got)
	}
	if got := projected.Machines[0].Facts.Credentials[0].Peers[0].DisplayName; got != safe {
		t.Fatalf("safe nested credential peer=%q, want %q", got, safe)
	}
	if overview.Machines[0].Facts.Credentials[0].Peers[0].DisplayName != unsafe ||
		overview.Machines[0].Findings[0].Message != "同儕 "+unsafe+" 的 session 還在續" {
		t.Fatal("safe overview projection mutated Store-owned nested slices")
	}
}

func TestMachineReadRejectsMissingTargetAndZeroEvaluationTime(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	service := New(st)
	if _, err := service.ListMachines(time.Time{}); err == nil {
		t.Fatal("zero list evaluation time was accepted")
	}
	if _, err := service.MachineDetail("missing", time.Now()); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing detail error=%v", err)
	}
}

func TestCurrentStateSinceUsesNewestOpenSpan(t *testing.T) {
	newest := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	oldest := newest.Add(-time.Hour)
	got := currentStateSince([]store.StateSpan{
		{State: state.Degraded, EnteredAt: newest},
		{State: state.Degraded, EnteredAt: oldest},
	}, state.Degraded)
	if got == nil || !got.Equal(newest) {
		t.Fatalf("state_since=%v want newest %v", got, newest)
	}
	if got := currentStateSince([]store.StateSpan{
		{State: state.Degraded, EnteredAt: newest},
		{State: state.Online, EnteredAt: oldest},
	}, state.Online); got != nil {
		t.Fatalf("state_since=%v used an older matching open span after newest disagreed", got)
	}
}

func assertMachineReadJSONAllowlist(t *testing.T, value any) {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var document any
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	forbidden := map[string]bool{
		"agent_token": true, "agent_token_hash": true,
		"notes": true, "facts": true, "connect": true,
		"pending_token": true, "pending_enrollment": true, "credentials": true,
		"cli_tools": true, "journals": true, "run_summaries": true,
		"path": true, "argv": true, "account": true, "last_error": true,
	}
	if _, detail := value.(MachineDetailResult); !detail {
		for _, key := range []string{"hostname", "unix_user", "tailscale_ip", "machine_id_hint", "reason", "message", "agent_version"} {
			forbidden[key] = true
		}
	}
	var walk func(any)
	walk = func(node any) {
		switch typed := node.(type) {
		case map[string]any:
			for key, child := range typed {
				if forbidden[key] {
					t.Errorf("machine read JSON exposed forbidden key %q: %s", key, raw)
				}
				walk(child)
			}
		case []any:
			for _, child := range typed {
				walk(child)
			}
		}
	}
	walk(document)
}

func TestMachineDetailCarriesWhetherLingerWasMeasured(t *testing.T) {
	tests := []struct {
		name           string
		os             string
		machineIDHint  string
		lingerMeasured bool
	}{
		{name: "macOS 未量到", os: "darwin", machineIDHint: "mac-machine-id", lingerMeasured: false},
		{name: "Linux 量到但未開啟", os: "linux", machineIDHint: "linux-machine-id", lingerMeasured: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st, err := store.Open(filepath.Join(t.TempDir(), "hub.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()

			now := time.Now().UTC().Truncate(time.Second)
			machineID, token, err := st.CreateEnrollTokenFor(tt.name, time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := st.RedeemEnrollToken(token, model.EnrollRequest{
				SchemaVersion: model.SchemaVersion, EnrollToken: token,
				Hostname: tt.name, UnixUser: "operator", OS: tt.os, Arch: "amd64",
			}, now); err != nil {
				t.Fatal(err)
			}
			if err := st.RecordObservation(machineID, model.ObservationBatch{
				SchemaVersion: model.SchemaVersion, MeasuredAt: now,
				Identity: model.Identity{
					MachineIDHint: tt.machineIDHint,
					LingerEnabled: false, LingerMeasured: tt.lingerMeasured,
				},
			}, now); err != nil {
				t.Fatal(err)
			}

			detail, err := New(st).MachineDetail(machineID, now.Add(time.Second))
			if err != nil {
				t.Fatal(err)
			}
			if detail.Identity.Value == nil {
				t.Fatalf("身分投影沒有值；投影若不帶「有沒有量到」，CLI 就會把一台根本沒有 systemd linger 的 Mac 印成 linger_enabled: false")
			}
			if detail.Identity.Value.LingerMeasured != tt.lingerMeasured || detail.Identity.Value.LingerEnabled {
				t.Fatalf("linger 投影錯誤：got measured=%t enabled=%t，want measured=%t enabled=false；投影若不帶「有沒有量到」，CLI 就會把一台根本沒有 systemd linger 的 Mac 印成 linger_enabled: false",
					detail.Identity.Value.LingerMeasured, detail.Identity.Value.LingerEnabled, tt.lingerMeasured)
			}
		})
	}
}

func TestZeroTotalMemoryIsNotAMeasurementOfZero(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	now := time.Now().UTC().Truncate(time.Second)
	tests := []struct {
		name         string
		resources    model.Resources
		wantMeasured bool
	}{
		{
			name: "沒有 meminfo",
			resources: model.Resources{
				DiskFreeBytes: 1 << 30, DiskTotalBytes: 2 << 30, CPUCount: 8,
			},
			wantMeasured: false,
		},
		{
			name: "量到記憶體",
			resources: model.Resources{
				DiskFreeBytes: 1 << 30, DiskTotalBytes: 2 << 30,
				MemTotalBytes: 16 << 30, MemAvailableBytes: 8 << 30, CPUCount: 8,
			},
			wantMeasured: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			machineID, token, err := st.CreateEnrollTokenFor(tt.name, time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := st.RedeemEnrollToken(token, model.EnrollRequest{
				SchemaVersion: model.SchemaVersion, EnrollToken: token,
				Hostname: tt.name, UnixUser: "operator", OS: "linux", Arch: "amd64",
			}, now); err != nil {
				t.Fatal(err)
			}
			if err := st.RecordObservation(machineID, model.ObservationBatch{
				SchemaVersion: model.SchemaVersion, MeasuredAt: now, Resources: tt.resources,
			}, now); err != nil {
				t.Fatal(err)
			}

			detail, err := New(st).MachineDetail(machineID, now.Add(time.Second))
			if err != nil {
				t.Fatal(err)
			}
			if detail.Resources.Value == nil {
				t.Fatal("資源投影沒有值；如此無法分辨記憶體沒量到，網頁會把它畫成 0 B，operator 會誤以為機器記憶體耗盡")
			}
			got := detail.Resources.Value
			if got.MemMeasured != tt.wantMeasured || got.MemTotalBytes != tt.resources.MemTotalBytes {
				t.Fatalf("記憶體量測投影錯誤：got measured=%t total=%d，want measured=%t total=%d；沒量到若被畫成 0 B，operator 會誤以為機器記憶體耗盡",
					got.MemMeasured, got.MemTotalBytes, tt.wantMeasured, tt.resources.MemTotalBytes)
			}
		})
	}
}

func TestUnmeasuredLoadStaysUnknownInTheProjection(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	now := time.Now().UTC().Truncate(time.Second)
	zero := 0.0
	tests := []struct {
		name string
		load *float64
	}{
		{name: "沒量到", load: nil},
		{name: "真的閒置", load: &zero},
	}
	for _, tt := range tests {
		machineID, token, err := st.CreateEnrollTokenFor(tt.name, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := st.RedeemEnrollToken(token, model.EnrollRequest{
			SchemaVersion: model.SchemaVersion, EnrollToken: token,
			Hostname: tt.name, UnixUser: "operator", OS: "linux", Arch: "amd64",
		}, now); err != nil {
			t.Fatal(err)
		}
		if err := st.RecordObservation(machineID, model.ObservationBatch{
			SchemaVersion: model.SchemaVersion, MeasuredAt: now,
			Resources: model.Resources{DiskFreeBytes: 1, DiskTotalBytes: 2, CPUCount: 8, Load1m: tt.load},
		}, now); err != nil {
			t.Fatal(err)
		}
		detail, err := New(st).MachineDetail(machineID, now.Add(time.Second))
		if err != nil {
			t.Fatal(err)
		}
		if detail.Resources.Value == nil {
			t.Fatalf("%s 的資源投影沒有值；把沒量到畫成 0.00，operator 會以為那台 Mac 很閒", tt.name)
		}
		got := detail.Resources.Value.Load1m
		if tt.load == nil && got != nil {
			t.Fatalf("沒量到的負載投影成 %v；把沒量到畫成 0.00，operator 會以為那台 Mac 很閒", *got)
		}
		if tt.load != nil && (got == nil || *got != 0) {
			t.Fatalf("真的零負載投影成 %v；nil 與 0 混在一起就無法區分沒量到與真的閒置", got)
		}
	}
}
