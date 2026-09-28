package operator

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/model"
	"github.com/teddashh/AI-Intune/internal/store"
)

func newOperatorChangeStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func addOperatorChangeMachine(t *testing.T, st *store.Store, id, name string, at time.Time) {
	t.Helper()
	if err := st.UpsertMachine(store.Machine{
		MachineID: id, DisplayName: name, Expected: true, CreatedAt: at,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestChangeReadUsesHubWindowAndProjectsSensitiveEvidence(t *testing.T) {
	st := newOperatorChangeStore(t)
	now := time.Date(2026, 9, 8, 18, 0, 0, 0, time.UTC)
	from := now.Add(-2 * time.Hour)
	addOperatorChangeMachine(t, st, "machine-change", "SampleHub\nControl", now.Add(-48*time.Hour))

	old := model.ObservationBatch{
		SchemaVersion: model.SchemaVersion, MeasuredAt: now.Add(72 * time.Hour),
		Identity:    model.Identity{Hostname: "secret-host-before", MachineIDHint: "secret-machine-id", OS: "linux", Kernel: "6.1", Arch: "amd64", LingerEnabled: true},
		Credentials: []model.Credential{{Provider: "claude", Status: model.CredConfigured, ActiveAccountID: "account-secret-before"}},
		CLITools:    []model.CLITool{{Name: "openclaw", Present: true, OnPath: true, VersionReported: "1.0.0", RealPath: "/secret/install", RunningExe: "/secret/runtime"}},
		OpenClaw:    model.OpenClaw{Present: false, Reason: "secret-free-form-reason"},
	}
	fresh := old
	fresh.MeasuredAt = now.Add(-72 * time.Hour)
	fresh.Credentials = []model.Credential{{Provider: "claude", Status: model.CredExpired, ActiveAccountID: "account-secret-after"}}
	if err := st.RecordObservation("machine-change", old, from.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	changedAt := now.Add(-time.Hour)
	if err := st.RecordObservation("machine-change", fresh, changedAt); err != nil {
		t.Fatal(err)
	}
	result, err := New(st).ListChanges(ChangeListRequest{
		MachineID: "machine-change", Kinds: []string{ChangeKindCredential}, Subject: "claude",
		From: &from, To: &now,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if result.Window.TimeBasis != "hub_received_at" || result.Window.Boundary != "(from,to]" ||
		result.Total != 1 || result.MatchedTotal != 1 || len(result.Items) != 1 {
		t.Fatalf("result=%+v", result)
	}
	for _, bucket := range result.KindCounts {
		want := 0
		if bucket.Kind == ChangeKindCredential {
			want = 1
		}
		if bucket.Count != want {
			t.Fatalf("filtered kind_counts=%+v", result.KindCounts)
		}
	}
	item := result.Items[0]
	if !item.ChangedAt.Equal(changedAt) || item.AgentMeasuredAt == nil || !item.AgentMeasuredAt.Equal(fresh.MeasuredAt) ||
		item.Semantics != "window_comparison" || item.BaselineStatus != "known" ||
		item.Before == nil || item.Before.Status == nil || *item.Before.Status != model.CredConfigured ||
		item.After == nil || item.After.Status == nil || *item.After.Status != model.CredExpired ||
		!containsChangeString(item.RedactedFields, "account_identity") ||
		!containsChangeString(item.ChangedFields, "status") {
		t.Fatalf("item=%+v", item)
	}
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{
		"account-secret-before", "account-secret-after", "secret-machine-id", "/secret/install",
		"/secret/runtime", "secret-free-form-reason", "active_account_id", "machine_id_hint",
	} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("safe DTO leaked %q: %s", forbidden, raw)
		}
	}
	if item.DisplayName != "SampleHub�Control" || !containsChangeString(item.AlteredFields, "display_name") {
		t.Fatalf("display projection=%q altered=%v", item.DisplayName, item.AlteredFields)
	}
}

func TestChangeReadCursorFreezesWindowAndAllSourceCeilings(t *testing.T) {
	st := newOperatorChangeStore(t)
	now := time.Date(2026, 9, 8, 19, 0, 0, 0, time.UTC)
	for i, id := range []string{"machine-a", "machine-b", "machine-c"} {
		addOperatorChangeMachine(t, st, id, strings.ToUpper(id), now.Add(time.Duration(-3+i)*time.Hour))
	}
	service := New(st)
	first, err := service.ListChanges(ChangeListRequest{Kinds: []string{ChangeKindRegistry}, Limit: 1}, now)
	if err != nil {
		t.Fatal(err)
	}
	if first.Total != 3 || first.MatchedTotal != 3 || len(first.Items) != 1 || first.NextCursor == nil {
		t.Fatalf("first=%+v", first)
	}
	firstID := first.Items[0].ChangeID
	addOperatorChangeMachine(t, st, "machine-late", "LATE", now.Add(-30*time.Minute))
	second, err := service.ListChanges(ChangeListRequest{
		Kinds: []string{ChangeKindRegistry}, Limit: 1, Cursor: *first.NextCursor,
	}, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if second.EvaluatedAt != first.EvaluatedAt || second.Window != first.Window ||
		second.CreationCeilings != first.CreationCeilings || second.Total != 3 ||
		len(second.Items) != 1 || second.Items[0].ChangeID == firstID ||
		(second.Items[0].MachineID != nil && *second.Items[0].MachineID == "machine-late") {
		t.Fatalf("cursor traversal shifted: first=%+v second=%+v", first, second)
	}
	if _, err := service.ListChanges(ChangeListRequest{
		MachineID: "machine-a", Kinds: []string{ChangeKindRegistry}, Cursor: *first.NextCursor,
	}, now); !errors.Is(err, ErrInvalidChangeRead) {
		t.Fatalf("filter-bound cursor err=%v", err)
	}
	mismatch := first.Window.From.Add(time.Second)
	if _, err := service.ListChanges(ChangeListRequest{
		Kinds: []string{ChangeKindRegistry}, From: &mismatch, Cursor: *first.NextCursor,
	}, now); !errors.Is(err, ErrInvalidChangeRead) {
		t.Fatalf("window-bound cursor err=%v", err)
	}
}

func TestChangeReadMakesMalformedPayloadAndRetentionAmbiguityExplicit(t *testing.T) {
	st := newOperatorChangeStore(t)
	now := time.Date(2026, 9, 8, 20, 0, 0, 0, time.UTC)
	from := now.Add(-time.Hour)
	addOperatorChangeMachine(t, st, "machine-malformed", "Malformed", now.Add(-48*time.Hour))
	if _, err := st.DB().Exec(`INSERT INTO retention_log
 (at,table_name,rows_deleted,older_than,kept_newest) VALUES (?,?,?,?,?)`,
		now.Add(-2*time.Hour).Format(time.RFC3339), "observed_state", 9,
		now.Add(-30*24*time.Hour).Format(time.RFC3339), 1); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`INSERT INTO observed_state
 (observation_id,machine_id,measured_at,received_at,kind,subject,payload,source)
 VALUES (?,?,?,?,?,?,?,?)`, "bad-change-payload", "machine-malformed",
		now.Add(-2*time.Hour).Format(time.RFC3339), now.Add(-30*time.Minute).Format(time.RFC3339),
		store.KindCredential, "claude", `{not-json-secret}`, store.SourceAgentMeasurement); err != nil {
		t.Fatal(err)
	}
	result, err := New(st).ListChanges(ChangeListRequest{
		Kinds: []string{ChangeKindCredential}, From: &from, To: &now,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if result.Coverage.ObservationHistory != "partial" || result.Coverage.ObservationRowsPruned != 9 ||
		len(result.Items) != 1 || result.Items[0].BaselineStatus != "possibly_pruned" ||
		result.Items[0].After != nil || !containsChangeString(result.Items[0].Issues, "after_payload_malformed") {
		t.Fatalf("result=%+v", result)
	}
	raw, _ := json.Marshal(result)
	if strings.Contains(string(raw), "not-json-secret") {
		t.Fatalf("malformed raw payload leaked: %s", raw)
	}
}

func TestChangeCoverageMarksOnlyUnreadSourcesNotApplicable(t *testing.T) {
	trackedFrom := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	prunedBefore := trackedFrom.Add(-24 * time.Hour)
	read := store.ChangeReadResult{Coverage: store.ChangeReadCoverage{
		ObservationHistoryPruned:         true,
		ObservationRowsPruned:            4,
		ObservationPrunedBefore:          &prunedBefore,
		LastObservationPrunedAt:          &trackedFrom,
		RegistryLifecycleHistoryComplete: false,
		RegistryLifecycleTrackedFrom:     &trackedFrom,
		StateTransitionHistoryComplete:   false,
		StateTransitionTrackedFrom:       &trackedFrom,
		Issues:                           []string{},
	}}
	for _, test := range []struct {
		name               string
		request            ChangeListRequest
		observationHistory string
		registryHistory    string
		stateHistory       string
	}{
		{
			name: "all sources", observationHistory: "partial",
			registryHistory: "partial_before_tracking_started", stateHistory: "partial_before_tracking_started",
		},
		{
			name: "observation only", request: ChangeListRequest{Kinds: []string{ChangeKindCredential}},
			observationHistory: "partial", registryHistory: ChangeCoverageNotApplicable,
			stateHistory: ChangeCoverageNotApplicable,
		},
		{
			name: "transitions only", request: ChangeListRequest{Kinds: []string{ChangeKindRegistry, ChangeKindState}},
			observationHistory: ChangeCoverageNotApplicable, registryHistory: "partial_before_tracking_started",
			stateHistory: "partial_before_tracking_started",
		},
		{
			name: "subject excludes registry", request: ChangeListRequest{
				Kinds: []string{ChangeKindRegistry, ChangeKindState}, Subject: "state",
			},
			observationHistory: ChangeCoverageNotApplicable, registryHistory: ChangeCoverageNotApplicable,
			stateHistory: "partial_before_tracking_started",
		},
		{
			name: "fixed subject excludes sole source", request: ChangeListRequest{
				Kinds: []string{ChangeKindRegistry}, Subject: "claude",
			},
			observationHistory: ChangeCoverageNotApplicable, registryHistory: ChangeCoverageNotApplicable,
			stateHistory: ChangeCoverageNotApplicable,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			coverage := changeCoverage(read, test.request)
			if coverage.ObservationHistory != test.observationHistory ||
				coverage.RegistryHistory != test.registryHistory || coverage.StateHistory != test.stateHistory {
				t.Fatalf("coverage=%+v", coverage)
			}
			if coverage.ObservationHistory == ChangeCoverageNotApplicable &&
				(coverage.ObservationRowsPruned != 0 || coverage.ObservationPrunedBefore != nil ||
					coverage.LastObservationPrunedAt != nil) {
				t.Fatalf("non-applicable observation retained source evidence: %+v", coverage)
			}
			if coverage.RegistryHistory == ChangeCoverageNotApplicable && coverage.RegistryHistoryStartedAt != nil {
				t.Fatalf("non-applicable registry retained source evidence: %+v", coverage)
			}
			if coverage.StateHistory == ChangeCoverageNotApplicable && coverage.StateHistoryStartedAt != nil {
				t.Fatalf("non-applicable state retained source evidence: %+v", coverage)
			}
		})
	}
}

func TestChangeReadRejectsInvalidRequestsBeforeStoreIO(t *testing.T) {
	now := time.Date(2026, 9, 8, 21, 0, 0, 0, time.UTC)
	tooOld := now.Add(-MaxChangeWindow - time.Second)
	future := now.Add(time.Second)
	requests := []ChangeListRequest{
		{Kinds: []string{"unknown"}},
		{Kinds: []string{ChangeKindState, ChangeKindState}},
		{MachineID: " bad"},
		{Subject: "bad\nsubject"},
		{Subject: "alice@example.com"},
		{Limit: MaxChangeReadLimit + 1},
		{From: &tooOld, To: &now},
		{To: &future},
		{Cursor: "not-base64url"},
	}
	for _, request := range requests {
		if _, err := New(nil).ListChangesContext(context.Background(), request, now); !errors.Is(err, ErrInvalidChangeRead) {
			t.Fatalf("request %+v err=%v", request, err)
		}
	}
	if _, err := New(nil).ListChangesContext(nil, ChangeListRequest{}, now); !errors.Is(err, ErrInvalidChangeRead) {
		t.Fatalf("nil context err=%v", err)
	}
	if _, err := New(nil).ListChanges(ChangeListRequest{}, time.Time{}); !errors.Is(err, ErrInvalidChangeRead) {
		t.Fatalf("zero evaluation err=%v", err)
	}
}

func TestChangeReadDoesNotProjectNegativeSystemdRestartEvidence(t *testing.T) {
	item := ChangeItem{Issues: []string{}, AlteredFields: []string{}}
	value := projectObservationValue(ChangeKindSystemd,
		`{"name":"clawctl-agent.service","present":true,"active_state":"active","sub_state":"running","n_restarts":-7}`,
		"after", &item)
	if value == nil || value.Present == nil || !*value.Present || value.Restarts != nil {
		t.Fatalf("systemd safe value=%+v", value)
	}
	if !containsChangeString(item.Issues, "after_restarts_not_canonical") {
		t.Fatalf("negative restart evidence issues=%v", item.Issues)
	}
}

// TestChangeReadProjectsSystemdMeasurementThreeStates 釘住 systemd 存在、不存在與本輪未量到的公開三態，並防止 Reason 外洩。
func TestChangeReadProjectsSystemdMeasurementThreeStates(t *testing.T) {
	tests := []struct {
		name   string
		before model.Unit
		after  model.Unit
		check  func(*testing.T, ChangeItem)
	}{
		{name: "本輪未量到", before: model.Unit{Name: "clawctl-agent.service", Present: true, Measured: true, ActiveState: "active", SubState: "running", NRestarts: 3}, after: model.Unit{Name: "clawctl-agent.service", Reason: "Failed to connect to bus"}, check: func(t *testing.T, item ChangeItem) {
			if item.After.Present != nil || item.After.Measured == nil || *item.After.Measured || item.After.Restarts != nil {
				t.Fatalf("未量到的投影不符三態契約：%+v", item.After)
			}
			if containsChangeString(item.Issues, "after_restarts_not_canonical") {
				t.Fatalf("未量到不應產生重啟計數器 issue：%v", item.Issues)
			}
		}},
		{name: "明確不存在", before: model.Unit{Name: "clawctl-agent.service", Present: true, Measured: true, NRestarts: 3}, after: model.Unit{Name: "clawctl-agent.service", Measured: true}, check: func(t *testing.T, item ChangeItem) {
			if item.After.Present == nil || *item.After.Present || item.After.Measured == nil || !*item.After.Measured {
				t.Fatalf("明確不存在的投影不符三態契約：%+v", item.After)
			}
		}},
		{name: "舊 agent 未量到", before: model.Unit{Name: "clawctl-agent.service", Present: true, Measured: true, NRestarts: 3}, after: model.Unit{Name: "clawctl-agent.service"}, check: func(t *testing.T, item ChangeItem) {
			if item.After.Present != nil || item.After.Measured == nil || *item.After.Measured || item.After.Restarts != nil {
				t.Fatalf("舊 agent 應判為未量到：%+v", item.After)
			}
		}},
		{name: "存在到存在", before: model.Unit{Name: "clawctl-agent.service", Present: true, Measured: true, ActiveState: "active", NRestarts: 3}, after: model.Unit{Name: "clawctl-agent.service", Present: true, Measured: true, ActiveState: "failed", NRestarts: 4}, check: func(t *testing.T, item ChangeItem) {
			if item.Before.Measured != nil || item.After.Measured != nil {
				t.Fatalf("present=true 不得投影 measured：before=%+v after=%+v", item.Before, item.After)
			}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			st := newOperatorChangeStore(t)
			now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
			from := now.Add(-2 * time.Hour)
			addOperatorChangeMachine(t, st, "machine-systemd", "Systemd Machine", now.Add(-24*time.Hour))
			for index, unit := range []model.Unit{test.before, test.after} {
				observation := model.ObservationBatch{SchemaVersion: model.SchemaVersion, MeasuredAt: from.Add(time.Duration(index) * time.Hour), Systemd: []model.Unit{unit}}
				if err := st.RecordObservation("machine-systemd", observation, from.Add(time.Duration(index)*time.Hour)); err != nil {
					t.Fatal(err)
				}
			}
			result, err := New(st).ListChanges(ChangeListRequest{MachineID: "machine-systemd", Kinds: []string{ChangeKindSystemd}, Subject: "clawctl-agent.service", From: &from, To: &now}, now)
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Items) != 1 || result.Items[0].Before == nil || result.Items[0].After == nil {
				t.Fatalf("預期一筆具前後值的變更，實際為：%+v", result)
			}
			test.check(t, result.Items[0])
			raw, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(raw), "Failed to connect to bus") {
				t.Fatalf("公開變更結果洩漏 Reason：%s", raw)
			}
		})
	}
}

func TestChangeReadUsesOpaqueSubjectForUnknownCredentialProvider(t *testing.T) {
	const privateSubject = "alice@example.com"
	item := ChangeItem{Issues: []string{}, AlteredFields: []string{}}
	got := safeChangeSubject(ChangeKindCredential, privateSubject, &item.Issues, &item.AlteredFields)
	if got != "(redacted subject)" {
		t.Fatalf("safe subject=%q", got)
	}
	if !containsChangeString(item.Issues, "subject_not_allowlisted") ||
		!containsChangeString(item.AlteredFields, "subject") {
		t.Fatalf("subject evidence issues=%v altered=%v", item.Issues, item.AlteredFields)
	}
	if safeChangeSubject(ChangeKindCredential, "claude", &item.Issues, &item.AlteredFields) != "claude" ||
		safeChangeSubject(ChangeKindCLITool, "agy", &item.Issues, &item.AlteredFields) != "agy" ||
		safeChangeSubject(ChangeKindSystemd, "clawctl-agent.service", &item.Issues, &item.AlteredFields) != "clawctl-agent.service" {
		t.Fatal("known subjects were not preserved")
	}
}

func TestChangeReadUnknownSubjectCannotInfluencePublicChangeID(t *testing.T) {
	const privateSubject = "alice@example.com"
	got := changeOpaqueID(string(store.ChangeReadSourceObservation), 17)
	oldPreimage := fmt.Sprintf("v1\x00%s\x00%d\x00%s\x00%s\x00%s\x00%s\x00%s",
		store.ChangeReadSourceObservation, 17, "machine-change", ChangeKindCredential,
		privateSubject, time.Time{}.UTC().Format(time.RFC3339),
		time.Date(2026, 9, 8, 20, 0, 0, 0, time.UTC).Format(time.RFC3339))
	oldDigest := sha256.Sum256([]byte(oldPreimage))
	if got == fmt.Sprintf("sha256:%x", oldDigest) || strings.Contains(got, privateSubject) {
		t.Fatalf("change ID retained private-subject preimage influence: %q", got)
	}
}

func TestChangeReadSubjectFilterUsesProjectedKindSpecificSubject(t *testing.T) {
	st := newOperatorChangeStore(t)
	now := time.Date(2026, 9, 8, 22, 0, 0, 0, time.UTC)
	addOperatorChangeMachine(t, st, "machine-subject-collision", "Subject Collision", now.Add(-48*time.Hour))
	observation := model.ObservationBatch{
		SchemaVersion: model.SchemaVersion, MeasuredAt: now.Add(-time.Minute),
		Credentials: []model.Credential{
			{Provider: "claude", Status: model.CredConfigured},
			{Provider: "state", Status: model.CredConfigured},
		},
		CLITools: []model.CLITool{{Name: "claude", Present: true, OnPath: true}},
		Systemd:  []model.Unit{{Name: "claude", Present: true, ActiveState: "active"}},
	}
	if err := st.RecordObservation("machine-subject-collision", observation, now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}

	credentialCollision, err := New(st).ListChanges(ChangeListRequest{
		MachineID: "machine-subject-collision", Kinds: []string{ChangeKindCredential},
		Subject: "state",
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if credentialCollision.Total != 0 || credentialCollision.MatchedTotal != 0 || len(credentialCollision.Items) != 0 {
		t.Fatalf("cross-kind subject collision satisfied public filter: %+v", credentialCollision)
	}

	mixed, err := New(st).ListChanges(ChangeListRequest{
		MachineID: "machine-subject-collision",
		Kinds:     []string{ChangeKindCredential, ChangeKindSystemd}, Subject: "claude",
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if mixed.Total != 1 || len(mixed.Items) != 1 || mixed.Items[0].Kind != ChangeKindCredential ||
		mixed.Items[0].Subject != "claude" {
		t.Fatalf("mixed kind subject filter escaped projection semantics: %+v", mixed)
	}

	unscoped, err := New(st).ListChanges(ChangeListRequest{
		MachineID: "machine-subject-collision", Subject: "claude",
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if unscoped.Total != 2 {
		t.Fatalf("unscoped public subject total=%d items=%+v", unscoped.Total, unscoped.Items)
	}
	for _, item := range unscoped.Items {
		if item.Subject != "claude" || (item.Kind != ChangeKindCredential && item.Kind != ChangeKindCLITool) {
			t.Fatalf("unscoped subject filter returned cross-kind collision: %+v", item)
		}
	}
}

func containsChangeString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
