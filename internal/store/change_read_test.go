package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/model"
	"github.com/teddashh/AI-Intune/internal/state"
)

func TestReadChangesUsesHubTimeAndRetainsMalformedEvidence(t *testing.T) {
	st := newTestStore(t)
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	machineID := "change-clock-machine"
	if err := st.UpsertMachine(Machine{
		MachineID: machineID, DisplayName: "clock-machine", Expected: true,
		CreatedAt: now.Add(-48 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	configured := mustChangePayload(t, model.Credential{
		Provider: "claude", Status: model.CredConfigured,
	})
	expired := mustChangePayload(t, model.Credential{
		Provider: "claude", Status: model.CredExpired,
		ActiveAccountID: "account-secret-must-stay-in-store",
	})
	failed := mustChangePayload(t, model.Credential{
		Provider: "claude", Status: model.CredFailed,
	})
	insertChangeObservation(t, st, "clock-before", machineID, KindCredential, "claude",
		configured, now.Add(365*24*time.Hour).Format(time.RFC3339), now.Add(-2*time.Hour).Format(time.RFC3339))
	insertChangeObservation(t, st, "clock-after", machineID, KindCredential, "claude",
		expired, now.Add(-365*24*time.Hour).Format(time.RFC3339), now.Add(-30*time.Minute).Format(time.RFC3339))
	// This row has an old agent timestamp but reached the Hub after the fixed
	// evaluation endpoint. It must not leak into the result.
	insertChangeObservation(t, st, "future-receipt", machineID, KindCredential, "claude",
		failed, now.Add(-2*365*24*time.Hour).Format(time.RFC3339), now.Add(time.Hour).Format(time.RFC3339))

	insertChangeObservation(t, st, "malformed-payload", machineID, KindCredential, "grok",
		`{"status":`, now.Add(-20*time.Minute).Format(time.RFC3339), now.Add(-20*time.Minute).Format(time.RFC3339))
	insertChangeObservation(t, st, "malformed-measured", machineID, KindCredential, "codex",
		configured, "not-a-measured-time", now.Add(-10*time.Minute).Format(time.RFC3339))
	insertChangeObservation(t, st, "malformed-received", machineID, KindCredential, "gemini",
		configured, now.Add(-5*time.Minute).Format(time.RFC3339), "not-a-received-time")

	result, err := st.ReadChanges(ChangeReadRequest{From: now.Add(-time.Hour), To: now})
	if err != nil {
		t.Fatalf("read changes: %v", err)
	}
	if result.MalformedTimestamps != 2 || result.UnplaceableTimestamps != 1 {
		t.Fatalf("timestamp counts malformed=%d unplaceable=%d, want 2/1",
			result.MalformedTimestamps, result.UnplaceableTimestamps)
	}
	if result.Coverage.Complete || !containsString(result.Coverage.Issues, "unplaceable_timestamps") {
		t.Fatalf("unplaceable evidence was not reflected in coverage: %+v", result.Coverage)
	}

	claude := changeRecordByKey(t, result.Records, KindCredential, "claude")
	if !claude.FromKnown || claude.FromPayload != configured || claude.ToPayload != expired ||
		!claude.HubAt.Equal(now.Add(-30*time.Minute)) || claude.MeasuredAt == nil ||
		!claude.MeasuredAt.Equal(now.Add(-365*24*time.Hour)) {
		t.Fatalf("Hub-time endpoint delta = %+v", claude)
	}
	if strings.Contains(claude.ToPayload, `"failed"`) {
		t.Fatalf("future-received evidence leaked into endpoint: %s", claude.ToPayload)
	}

	grok := changeRecordByKey(t, result.Records, KindCredential, "grok")
	if grok.FromKnown || grok.ToPayload != `{"status":` ||
		!containsString(grok.Issues, "to_payload_malformed") {
		t.Fatalf("malformed payload was hidden or normalized: %+v", grok)
	}
	codex := changeRecordByKey(t, result.Records, KindCredential, "codex")
	if codex.MeasuredAt != nil || !containsString(codex.Issues, "to_measured_at_invalid") {
		t.Fatalf("malformed measured_at projection = %+v", codex)
	}
	for _, record := range result.Records {
		if record.Subject == "gemini" {
			t.Fatalf("unplaceable received_at was placed in the window: %+v", record)
		}
	}

	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	for _, unsafe := range []string{"account-secret-must-stay-in-store", `\"status\":`, "not-a-measured-time"} {
		if strings.Contains(string(encoded), unsafe) {
			t.Fatalf("raw observation evidence crossed json boundary: %s", encoded)
		}
	}
}

func TestReadChangesLifecycleIsAppendOnlyAndNoopsDoNotInventEvents(t *testing.T) {
	st := newTestStore(t)
	now := time.Date(2026, 9, 8, 14, 0, 0, 0, time.UTC)
	machineID := "lifecycle-machine"
	if err := st.UpsertMachine(Machine{
		MachineID: machineID, DisplayName: "lifecycle", Expected: true,
		CreatedAt: now.Add(-10 * time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.RetireMachine(machineID, now.Add(-8*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := st.RetireMachine(machineID, now.Add(-7*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := st.UnretireMachine(machineID, now.Add(-6*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := st.UnretireMachine(machineID, now.Add(-5*time.Minute)); err != nil {
		t.Fatal(err)
	}

	if got := countRows(t, st, `SELECT COUNT(*) FROM machine_registry_lifecycle_events WHERE machine_id=?`, machineID); got != 2 {
		t.Fatalf("lifecycle event count=%d, want two real transitions", got)
	}
	machine, err := st.GetMachine(machineID)
	if err != nil || machine.RetiredAt != nil {
		t.Fatalf("unretired projection machine=%+v err=%v", machine, err)
	}

	result, err := st.ReadChanges(ChangeReadRequest{From: now.Add(-time.Hour), To: now})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Coverage.RegistryLifecycleHistoryComplete {
		t.Fatalf("fresh ledger lifecycle coverage=%+v", result.Coverage)
	}
	var lifecycle []ChangeReadRecord
	for _, record := range result.Records {
		if record.Kind == ChangeReadKindRegistry && record.Subject == ChangeReadSubjectLife {
			lifecycle = append(lifecycle, record)
		}
	}
	if len(lifecycle) != 3 {
		t.Fatalf("registry lifecycle records=%d: %+v", len(lifecycle), lifecycle)
	}
	created := changeRecordBySource(t, lifecycle, ChangeReadSourceRegistryCreated)
	if created.FromKnown || created.From != "" || created.To != ChangeReadRegistered {
		t.Fatalf("synthetic creation record=%+v", created)
	}
	retired := changeRecordByTransition(t, lifecycle, ChangeReadRegistered, ChangeReadRetired)
	registered := changeRecordByTransition(t, lifecycle, ChangeReadRetired, ChangeReadRegistered)
	if !retired.FromKnown || !registered.FromKnown || retired.Key.Sequence == registered.Key.Sequence {
		t.Fatalf("lifecycle transitions retired=%+v registered=%+v", retired, registered)
	}
}

func TestMachineLifecycleProjectionAndEvidenceAreAtomic(t *testing.T) {
	st := newTestStore(t)
	now := time.Date(2026, 9, 8, 15, 0, 0, 0, time.UTC)
	machineID := "atomic-lifecycle"
	if err := st.UpsertMachine(Machine{
		MachineID: machineID, DisplayName: "atomic", Expected: true, CreatedAt: now.Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`
CREATE TRIGGER reject_lifecycle_evidence
BEFORE INSERT ON machine_registry_lifecycle_events
BEGIN SELECT RAISE(ABORT, 'blocked lifecycle evidence'); END`); err != nil {
		t.Fatal(err)
	}
	if err := st.RetireMachine(machineID, now); err == nil || !strings.Contains(err.Error(), "lifecycle evidence") {
		t.Fatalf("retire with rejected evidence error=%v", err)
	}
	machine, err := st.GetMachine(machineID)
	if err != nil || machine.RetiredAt != nil {
		t.Fatalf("failed evidence left projection changed: machine=%+v err=%v", machine, err)
	}
	if got := countRows(t, st, `SELECT COUNT(*) FROM machine_registry_lifecycle_events`); got != 0 {
		t.Fatalf("failed lifecycle transaction left %d events", got)
	}
}

func TestMachineLifecycleRejectsInvalidOrBackdatedTransitions(t *testing.T) {
	st := newTestStore(t)
	created := time.Date(2026, 9, 8, 15, 15, 0, 0, time.UTC)
	machineID := "ordered-lifecycle"
	mustUpsertChangeMachine(t, st, machineID, created.Add(day))
	if err := st.RetireMachine(machineID, time.Time{}); err == nil {
		t.Fatal("retire accepted zero transition time")
	}
	if err := st.RetireMachine(machineID, created.Add(-time.Second)); err == nil || !strings.Contains(err.Error(), "created_at") {
		t.Fatalf("retire accepted time before creation: %v", err)
	}
	retiredAt := created.Add(time.Minute)
	if err := st.RetireMachine(machineID, retiredAt.Add(900*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if err := st.UnretireMachine(machineID, retiredAt.Add(-time.Second)); err == nil || !strings.Contains(err.Error(), "retired_at") {
		t.Fatalf("unretire accepted backdated time: %v", err)
	}
	machine, err := st.GetMachine(machineID)
	if err != nil || machine.RetiredAt == nil || !machine.RetiredAt.Equal(retiredAt) {
		t.Fatalf("rejected unretire changed projection: machine=%+v err=%v", machine, err)
	}
	unretiredAt := retiredAt.Add(time.Minute)
	if err := st.UnretireMachine(machineID, unretiredAt); err != nil {
		t.Fatal(err)
	}
	if err := st.RetireMachine(machineID, retiredAt); err == nil || !strings.Contains(err.Error(), "latest transition") {
		t.Fatalf("re-retire accepted backdated time: %v", err)
	}
	if got := countRows(t, st, `SELECT COUNT(*) FROM machine_registry_lifecycle_events WHERE machine_id=?`, machineID); got != 2 {
		t.Fatalf("rejected lifecycle writes left %d events, want 2", got)
	}
}

func TestUpsertMachineAppendsOnlyRealLifecycleTransitions(t *testing.T) {
	st := newTestStore(t)
	createdAt := time.Date(2026, 9, 8, 15, 30, 0, 0, time.UTC)
	tooEarly := createdAt.Add(-time.Second)
	if err := st.UpsertMachine(Machine{
		MachineID: "invalid-retired-upsert", DisplayName: "invalid", Expected: true,
		CreatedAt: createdAt, RetiredAt: &tooEarly,
	}); err == nil || !strings.Contains(err.Error(), "before created_at") {
		t.Fatalf("new machine accepted impossible retirement time: %v", err)
	}
	if _, err := st.GetMachine("invalid-retired-upsert"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("invalid new machine was partially inserted: %v", err)
	}

	machineID := "upsert-lifecycle"
	retiredAt := createdAt.Add(time.Minute)
	if err := st.UpsertMachine(Machine{
		MachineID: machineID, DisplayName: "retired at creation", Expected: true,
		CreatedAt: createdAt, RetiredAt: &retiredAt,
	}); err != nil {
		t.Fatal(err)
	}
	if got := countRows(t, st, `SELECT COUNT(*) FROM machine_registry_lifecycle_events WHERE machine_id=?`, machineID); got != 1 {
		t.Fatalf("new retired machine lifecycle rows=%d, want 1", got)
	}

	// A roster edit while already retired must preserve the actual transition
	// coordinate even if a caller supplies another non-nil timestamp.
	machine, err := st.GetMachine(machineID)
	if err != nil {
		t.Fatal(err)
	}
	laterRetiredAt := retiredAt.Add(time.Hour)
	machine.DisplayName = "edited while retired"
	machine.RetiredAt = &laterRetiredAt
	if err := st.UpsertMachine(machine); err != nil {
		t.Fatal(err)
	}
	machine, err = st.GetMachine(machineID)
	if err != nil || machine.RetiredAt == nil || !machine.RetiredAt.Equal(retiredAt) || machine.DisplayName != "edited while retired" {
		t.Fatalf("retired roster edit changed lifecycle projection: machine=%+v err=%v", machine, err)
	}
	if got := countRows(t, st, `SELECT COUNT(*) FROM machine_registry_lifecycle_events WHERE machine_id=?`, machineID); got != 1 {
		t.Fatalf("same-state retired edit invented lifecycle row: %d", got)
	}

	unretiredAt := retiredAt.Add(2 * time.Hour)
	st.nowFn = func() time.Time { return unretiredAt }
	machine.RetiredAt = nil
	if err := st.UpsertMachine(machine); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertMachine(machine); err != nil {
		t.Fatal(err)
	}
	reRetiredAt := unretiredAt.Add(time.Hour)
	machine.RetiredAt = &reRetiredAt
	if err := st.UpsertMachine(machine); err != nil {
		t.Fatal(err)
	}

	rows, err := st.DB().Query(`
SELECT event_type,occurred_at
  FROM machine_registry_lifecycle_events
 WHERE machine_id=?
 ORDER BY event_id`, machineID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var gotTypes []string
	var gotTimes []time.Time
	for rows.Next() {
		var kind, rawAt string
		if err := rows.Scan(&kind, &rawAt); err != nil {
			t.Fatal(err)
		}
		gotTypes = append(gotTypes, kind)
		gotTimes = append(gotTimes, parseTime(rawAt))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotTypes, []string{ChangeReadRetired, ChangeReadRegistered, ChangeReadRetired}) ||
		!reflect.DeepEqual(gotTimes, []time.Time{retiredAt, unretiredAt, reRetiredAt}) {
		t.Fatalf("upsert lifecycle events types=%v times=%v", gotTypes, gotTimes)
	}
}

func TestUpsertMachineLifecycleEvidenceFailureRollsBackRosterEdit(t *testing.T) {
	st := newTestStore(t)
	now := time.Date(2026, 9, 8, 15, 45, 0, 0, time.UTC)
	machineID := "atomic-upsert-lifecycle"
	if err := st.UpsertMachine(Machine{
		MachineID: machineID, DisplayName: "before", Expected: true, CreatedAt: now.Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`
CREATE TRIGGER reject_upsert_lifecycle_evidence
BEFORE INSERT ON machine_registry_lifecycle_events
BEGIN SELECT RAISE(ABORT, 'blocked upsert lifecycle evidence'); END`); err != nil {
		t.Fatal(err)
	}
	retiredAt := now
	if err := st.UpsertMachine(Machine{
		MachineID: machineID, DisplayName: "after", Expected: true,
		CreatedAt: now.Add(-time.Hour), RetiredAt: &retiredAt,
	}); err == nil || !strings.Contains(err.Error(), "lifecycle evidence") {
		t.Fatalf("upsert with rejected evidence error=%v", err)
	}
	machine, err := st.GetMachine(machineID)
	if err != nil || machine.DisplayName != "before" || machine.RetiredAt != nil {
		t.Fatalf("failed lifecycle evidence partially edited roster: machine=%+v err=%v", machine, err)
	}
}

func TestReadChangesStateTransitionsKeepPredecessorsAndUnknownFirst(t *testing.T) {
	st := newTestStore(t)
	now := time.Date(2026, 9, 8, 16, 0, 0, 0, time.UTC)
	knownID := "state-known"
	firstID := "state-first"
	for _, id := range []string{knownID, firstID} {
		if err := st.UpsertMachine(Machine{
			MachineID: id, DisplayName: id, Expected: true, CreatedAt: now.Add(-day),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.RecordStateTransition(knownID, state.Online, "baseline", now.Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordStateTransition(knownID, state.Degraded, "degraded", now.Add(-30*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordStateTransition(knownID, state.Online, "recovered", now.Add(-10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordStateTransition(firstID, state.Unreachable, "first verdict", now.Add(-5*time.Minute)); err != nil {
		t.Fatal(err)
	}

	result, err := st.ReadChanges(ChangeReadRequest{From: now.Add(-time.Hour), To: now})
	if err != nil {
		t.Fatal(err)
	}
	var known []ChangeReadRecord
	var first ChangeReadRecord
	for _, record := range result.Records {
		if record.Key.Source != ChangeReadSourceStateHistory {
			continue
		}
		if record.MachineID == knownID {
			known = append(known, record)
		} else if record.MachineID == firstID {
			first = record
		}
	}
	if len(known) != 2 {
		t.Fatalf("known state transitions=%+v", known)
	}
	for _, record := range known {
		if !record.FromKnown {
			t.Fatalf("state predecessor was lost: %+v", record)
		}
	}
	if first.Key.Sequence == 0 || first.FromKnown || first.From != "" || first.To != string(state.Unreachable) {
		t.Fatalf("first state transition invented predecessor: %+v", first)
	}
}

func TestReadChangesStateCeilingSurvivesSameSecondProjectionMutation(t *testing.T) {
	st := newTestStore(t)
	from := time.Date(2026, 9, 8, 16, 30, 0, 0, time.UTC)
	machineID := "state-frozen-same-second"
	mustUpsertChangeMachine(t, st, machineID, from)
	if err := st.RecordStateTransition(machineID, state.Online, "baseline", from.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	at := from.Add(time.Minute)
	if err := st.RecordStateTransition(machineID, state.Degraded, "first same-second value", at); err != nil {
		t.Fatal(err)
	}
	request := ChangeReadRequest{From: from, To: from.Add(time.Hour), MachineID: machineID, Kinds: []string{ChangeReadKindState}}
	first, err := st.ReadChanges(request)
	if err != nil {
		t.Fatal(err)
	}
	degraded := changeRecordByTransition(t, first.Records, string(state.Online), string(state.Degraded))
	if err := st.RecordStateTransition(machineID, state.Online, "later same-second value", at); err != nil {
		t.Fatal(err)
	}
	request.Ceilings = &first.Ceilings
	frozen, err := st.ReadChanges(request)
	if err != nil {
		t.Fatal(err)
	}
	if got := changeRecordBySource(t, frozen.Records, ChangeReadSourceStateHistory); got.Key != degraded.Key || got.To != string(state.Degraded) {
		t.Fatalf("same-second projection mutation changed frozen evidence: first=%+v frozen=%+v", degraded, got)
	}
	request.Ceilings = nil
	fresh, err := st.ReadChanges(request)
	if err != nil {
		t.Fatal(err)
	}
	online := changeRecordByTransition(t, fresh.Records, string(state.Degraded), string(state.Online))
	if !online.FromKnown || !online.HubAt.Equal(at) || online.Key.Sequence <= degraded.Key.Sequence {
		t.Fatalf("same-second append-only transition=%+v after=%+v", online, degraded)
	}
}

func TestReadChangesCeilingsExcludeLaterBackdatedWritesAndRejectPrune(t *testing.T) {
	st := newTestStore(t)
	now := time.Date(2026, 9, 8, 17, 0, 0, 0, time.UTC)
	machineID := "ceiling-machine"
	if err := st.UpsertMachine(Machine{
		MachineID: machineID, DisplayName: "ceiling", Expected: true, CreatedAt: now.Add(-2 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	configured := mustChangePayload(t, model.Credential{Provider: "claude", Status: model.CredConfigured})
	expired := mustChangePayload(t, model.Credential{Provider: "claude", Status: model.CredExpired})
	insertChangeObservation(t, st, "ceiling-before", machineID, KindCredential, "claude", configured,
		now.Add(-2*time.Hour).Format(time.RFC3339), now.Add(-2*time.Hour).Format(time.RFC3339))
	insertChangeObservation(t, st, "ceiling-after", machineID, KindCredential, "claude", expired,
		now.Add(-30*time.Minute).Format(time.RFC3339), now.Add(-30*time.Minute).Format(time.RFC3339))
	request := ChangeReadRequest{From: now.Add(-time.Hour), To: now}
	first, err := st.ReadChanges(request)
	if err != nil {
		t.Fatal(err)
	}
	firstKeys := changeReadKeys(first.Records)

	failed := mustChangePayload(t, model.Credential{Provider: "claude", Status: model.CredFailed})
	insertChangeObservation(t, st, "late-backdated", machineID, KindCredential, "claude", failed,
		now.Add(-5*time.Minute).Format(time.RFC3339), now.Add(-5*time.Minute).Format(time.RFC3339))
	if err := st.UpsertMachine(Machine{
		MachineID: "late-registry", DisplayName: "late", Expected: true, CreatedAt: now.Add(-10 * time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	request.Ceilings = &first.Ceilings
	frozen, err := st.ReadChanges(request)
	if err != nil {
		t.Fatal(err)
	}
	if got := changeReadKeys(frozen.Records); !reflect.DeepEqual(got, firstKeys) {
		t.Fatalf("later rows changed frozen traversal:\nfirst=%v\nagain=%v", firstKeys, got)
	}
	claude := changeRecordByKey(t, frozen.Records, KindCredential, "claude")
	if claude.ToPayload != expired {
		t.Fatalf("late backdated observation entered frozen endpoint: %s", claude.ToPayload)
	}

	if _, err := st.DB().Exec(`
INSERT INTO retention_log(at,table_name,rows_deleted,older_than,kept_newest)
VALUES(?,?,?,?,0)`, fmtTime(now), "observed_state", 0, fmtTime(now.Add(-30*day))); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ReadChanges(request); !errors.Is(err, ErrChangeReadTraversalGone) {
		t.Fatalf("continuation after prune generation error=%v", err)
	}
}

func TestReadChangesReportsRetentionCoverageAtFrozenCeiling(t *testing.T) {
	st := newTestStore(t)
	now := time.Date(2026, 9, 8, 18, 0, 0, 0, time.UTC)
	if err := st.UpsertMachine(Machine{
		MachineID: "pruned-baseline", DisplayName: "pruned-baseline", Expected: true, CreatedAt: now.Add(-day),
	}); err != nil {
		t.Fatal(err)
	}
	insertChangeObservation(t, st, "pruned-current", "pruned-baseline", KindIdentity, "identity",
		mustChangePayload(t, model.Identity{OS: "debian", Arch: "amd64"}),
		now.Add(-30*time.Minute).Format(time.RFC3339), now.Add(-30*time.Minute).Format(time.RFC3339))
	for _, row := range []struct {
		at      time.Time
		older   time.Time
		deleted int
	}{
		{now.Add(-2 * time.Hour), now.Add(-30 * day), 7},
		{now.Add(-time.Hour), now.Add(-20 * day), 3},
	} {
		if _, err := st.DB().Exec(`
INSERT INTO retention_log(at,table_name,rows_deleted,older_than,kept_newest)
VALUES(?,?,?,?,0)`, fmtTime(row.at), "observed_state", row.deleted, fmtTime(row.older)); err != nil {
			t.Fatal(err)
		}
	}
	result, err := st.ReadChanges(ChangeReadRequest{From: now.Add(-day), To: now})
	if err != nil {
		t.Fatal(err)
	}
	coverage := result.Coverage
	if coverage.Complete || !coverage.ObservationHistoryPruned || coverage.ObservationRowsPruned != 10 ||
		coverage.ObservationPrunedBefore == nil || !coverage.ObservationPrunedBefore.Equal(now.Add(-20*day)) ||
		coverage.LastObservationPrunedAt == nil || !coverage.LastObservationPrunedAt.Equal(now.Add(-time.Hour)) ||
		!containsString(coverage.Issues, "observation_history_pruned") {
		t.Fatalf("retention coverage=%+v", coverage)
	}
	identity := changeRecordByKey(t, result.Records, KindIdentity, "identity")
	if identity.FromKnown || !containsString(identity.Issues, "from_possibly_pruned") {
		t.Fatalf("pruned missing baseline was presented as known: %+v", identity)
	}
}

func TestReadChangesUsesOpenClosedWindowAndStableSameSecondTieBreak(t *testing.T) {
	st := newTestStore(t)
	from := time.Date(2026, 9, 8, 18, 0, 0, 0, time.UTC)
	to := from.Add(time.Hour)
	machineID := "window-boundary"
	if err := st.UpsertMachine(Machine{
		MachineID: machineID, DisplayName: "window-boundary", Expected: true, CreatedAt: from.Add(-day),
	}); err != nil {
		t.Fatal(err)
	}
	configured := mustChangePayload(t, model.Credential{Provider: "codex", Status: model.CredConfigured})
	expired := mustChangePayload(t, model.Credential{Provider: "codex", Status: model.CredExpired})
	failed := mustChangePayload(t, model.Credential{Provider: "codex", Status: model.CredFailed})
	insertChangeObservation(t, st, "at-from", machineID, KindCredential, "codex", configured,
		from.Format(time.RFC3339), from.Format(time.RFC3339))
	insertChangeObservation(t, st, "at-to-first", machineID, KindCredential, "codex", expired,
		to.Format(time.RFC3339), to.Format(time.RFC3339))
	insertChangeObservation(t, st, "at-to-last", machineID, KindCredential, "codex", failed,
		to.Format(time.RFC3339), to.Format(time.RFC3339))
	insertChangeObservation(t, st, "after-to", machineID, KindCredential, "codex", configured,
		to.Add(time.Second).Format(time.RFC3339), to.Add(time.Second).Format(time.RFC3339))

	if err := st.RecordStateTransition(machineID, state.Online, "at lower boundary", from); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordStateTransition(machineID, state.Degraded, "at upper boundary", to); err != nil {
		t.Fatal(err)
	}

	result, err := st.ReadChanges(ChangeReadRequest{From: from, To: to})
	if err != nil {
		t.Fatal(err)
	}
	credential := changeRecordByKey(t, result.Records, KindCredential, "codex")
	if !credential.FromKnown || credential.FromPayload != configured || credential.ToPayload != failed ||
		!credential.HubAt.Equal(to) {
		t.Fatalf("observation boundary endpoints=%+v", credential)
	}
	var states []ChangeReadRecord
	for _, record := range result.Records {
		if record.Key.Source == ChangeReadSourceStateHistory {
			states = append(states, record)
		}
	}
	if len(states) != 1 || !states[0].FromKnown || states[0].From != string(state.Online) ||
		states[0].To != string(state.Degraded) || !states[0].HubAt.Equal(to) {
		t.Fatalf("state boundary transitions=%+v", states)
	}
	if len(result.Records) < 2 || result.Records[0].Key.Source != ChangeReadSourceStateHistory ||
		result.Records[1].Key.Source != ChangeReadSourceObservation {
		t.Fatalf("same-second source order=%+v", result.Records)
	}
}

func TestReadChangesRejectsFractionalHubCoordinatesAsUnplaceable(t *testing.T) {
	st := newTestStore(t)
	now := time.Date(2026, 9, 8, 19, 30, 0, 0, time.UTC)
	machineID := "fractional-hub-time"
	if err := st.UpsertMachine(Machine{
		MachineID: machineID, DisplayName: "fractional", Expected: true, CreatedAt: now.Add(-day),
	}); err != nil {
		t.Fatal(err)
	}
	fractional := now.Add(-time.Minute).Add(500 * time.Millisecond).Format(time.RFC3339Nano)
	zeroFractional := now.Add(-2*time.Minute).Format("2006-01-02T15:04:05") + ".000Z"
	insertChangeObservation(t, st, "fractional-received", machineID, KindIdentity, "identity",
		mustChangePayload(t, model.Identity{OS: "debian"}), now.Add(-time.Minute).Format(time.RFC3339), fractional)
	insertChangeObservation(t, st, "zero-fractional-received", machineID, KindIdentity, "identity-zero",
		mustChangePayload(t, model.Identity{OS: "debian"}), now.Add(-2*time.Minute).Format(time.RFC3339), zeroFractional)
	if _, err := st.DB().Exec(`
INSERT INTO machine_state_history(machine_id,state,reason,entered_at)
VALUES(?,?,?,?)`, machineID, state.Online, "fractional hub coordinate", fractional); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`
INSERT INTO machine_state_history(machine_id,state,reason,entered_at)
VALUES(?,?,?,?)`, machineID, state.Degraded, "zero fractional hub coordinate", zeroFractional); err != nil {
		t.Fatal(err)
	}

	result, err := st.ReadChanges(ChangeReadRequest{From: now.Add(-time.Hour), To: now})
	if err != nil {
		t.Fatal(err)
	}
	if result.MalformedTimestamps != 4 || result.UnplaceableTimestamps != 4 || result.Coverage.Complete ||
		!containsString(result.Coverage.Issues, "unplaceable_timestamps") {
		t.Fatalf("fractional Hub timestamps were not explicit: %+v", result)
	}
	for _, record := range result.Records {
		if record.MachineID == machineID {
			t.Fatalf("fractional Hub timestamp was placed in result: %+v", record)
		}
	}
}

func TestReadChangesMarksMigratedRegistryLifecycleCoverage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "migrated.sqlite")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	if err := st.UpsertMachine(Machine{
		MachineID: "legacy-machine", DisplayName: "legacy", Expected: true, CreatedAt: now.Add(-day),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`DELETE FROM schema_meta WHERE key IN (?,?)`, changeReadMetaTracked, changeReadMetaComplete); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`DROP TABLE machine_registry_lifecycle_events`); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	st, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	result, err := st.ReadChanges(ChangeReadRequest{From: now.Add(-2 * day), To: now.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if result.Coverage.RegistryLifecycleHistoryComplete || result.Coverage.RegistryLifecycleTrackedFrom == nil ||
		!containsString(result.Coverage.Issues, "registry_lifecycle_history_incomplete") {
		t.Fatalf("migrated lifecycle coverage=%+v", result.Coverage)
	}
}

func TestReadChangesBackfillsStateEventsAndMarksMigratedCoverage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "migrated-state.sqlite")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	mustUpsertChangeMachine(t, st, "legacy-state", now)
	if err := st.RecordStateTransition("legacy-state", state.Degraded, "legacy", now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`DROP TRIGGER append_state_transition_insert`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`DROP TRIGGER append_state_transition_update`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`DROP TABLE machine_state_transition_events`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`DELETE FROM schema_meta WHERE key IN (?,?)`, changeReadStateTracked, changeReadStateComplete); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	result, err := st.ReadChanges(ChangeReadRequest{
		From: now.Add(-2 * time.Hour), To: now, Kinds: []string{ChangeReadKindState},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Coverage.StateTransitionHistoryComplete || result.Coverage.StateTransitionTrackedFrom == nil ||
		!containsString(result.Coverage.Issues, "state_transition_history_incomplete") {
		t.Fatalf("migrated state coverage=%+v", result.Coverage)
	}
	if got := countRows(t, st, `SELECT COUNT(*) FROM machine_state_transition_events`); got != 1 {
		t.Fatalf("state backfill rows=%d, want 1", got)
	}
	if _, err := st.DB().Exec(string(schemaSQL)); err != nil {
		t.Fatal(err)
	}
	if got := countRows(t, st, `SELECT COUNT(*) FROM machine_state_transition_events`); got != 1 {
		t.Fatalf("idempotent state backfill rows=%d, want 1", got)
	}
}

func TestReadChangesCoverageOnlyReflectsSelectedSources(t *testing.T) {
	st := newTestStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	if _, err := st.DB().Exec(`UPDATE schema_meta SET value='0' WHERE key IN (?,?)`, changeReadMetaComplete, changeReadStateComplete); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`
INSERT INTO retention_log(at,table_name,rows_deleted,older_than,kept_newest)
VALUES(?,?,?,?,0)`, fmtTime(now), changeReadRetentionKind, 1, fmtTime(now.Add(-day))); err != nil {
		t.Fatal(err)
	}
	stateOnly, err := st.ReadChanges(ChangeReadRequest{From: now.Add(-day), To: now, Kinds: []string{ChangeReadKindState}})
	if err != nil {
		t.Fatal(err)
	}
	if stateOnly.Coverage.ObservationHistoryPruned || !stateOnly.Coverage.RegistryLifecycleHistoryComplete ||
		stateOnly.Coverage.StateTransitionHistoryComplete {
		t.Fatalf("state query inherited unrelated coverage: %+v", stateOnly.Coverage)
	}
	credentialOnly, err := st.ReadChanges(ChangeReadRequest{From: now.Add(-day), To: now, Kinds: []string{KindCredential}})
	if err != nil {
		t.Fatal(err)
	}
	if !credentialOnly.Coverage.ObservationHistoryPruned ||
		!credentialOnly.Coverage.RegistryLifecycleHistoryComplete ||
		!credentialOnly.Coverage.StateTransitionHistoryComplete ||
		containsString(credentialOnly.Coverage.Issues, "registry_lifecycle_history_incomplete") ||
		containsString(credentialOnly.Coverage.Issues, "state_transition_history_incomplete") {
		t.Fatalf("observation query coverage included other sources: %+v", credentialOnly.Coverage)
	}
}

func TestReadChangesValidatesRequestBeforeQuery(t *testing.T) {
	st := newTestStore(t)
	now := time.Date(2026, 9, 8, 19, 0, 0, 0, time.UTC)
	negative := ChangeReadCeilings{Registry: -1}
	for _, test := range []struct {
		name string
		ctx  context.Context
		req  ChangeReadRequest
	}{
		{"nil context", nil, ChangeReadRequest{From: now.Add(-day), To: now}},
		{"missing from", context.Background(), ChangeReadRequest{To: now}},
		{"same endpoint", context.Background(), ChangeReadRequest{From: now, To: now}},
		{"fractional endpoint", context.Background(), ChangeReadRequest{From: now.Add(-day), To: now.Add(time.Nanosecond)}},
		{"negative ceiling", context.Background(), ChangeReadRequest{From: now.Add(-day), To: now, Ceilings: &negative}},
		{"unknown kind", context.Background(), ChangeReadRequest{From: now.Add(-day), To: now, Kinds: []string{"not-canonical"}}},
		{"duplicate kind", context.Background(), ChangeReadRequest{From: now.Add(-day), To: now, Kinds: []string{KindIdentity, KindIdentity}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := st.ReadChangesContext(test.ctx, test.req); !errors.Is(err, ErrInvalidChangeRead) {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestReadChangesPushesExactFiltersAndSkipsUnselectedSources(t *testing.T) {
	st := newTestStore(t)
	now := time.Date(2026, 9, 8, 19, 15, 0, 0, time.UTC)
	for _, machineID := range []string{"filter-target", "filter-noise"} {
		if err := st.UpsertMachine(Machine{
			MachineID: machineID, DisplayName: machineID, Expected: true, CreatedAt: now.Add(-day),
		}); err != nil {
			t.Fatal(err)
		}
	}
	configured := mustChangePayload(t, model.Credential{Provider: "codex", Status: model.CredConfigured})
	expired := mustChangePayload(t, model.Credential{Provider: "codex", Status: model.CredExpired})
	insertChangeObservation(t, st, "filter-before", "filter-target", KindCredential, "wanted",
		configured, fmtTime(now.Add(-2*time.Hour)), fmtTime(now.Add(-2*time.Hour)))
	insertChangeObservation(t, st, "filter-after", "filter-target", KindCredential, "wanted",
		expired, fmtTime(now.Add(-time.Minute)), fmtTime(now.Add(-time.Minute)))
	insertChangeObservation(t, st, "filter-other-subject", "filter-target", KindCredential, "other",
		expired, fmtTime(now.Add(-time.Minute)), "malformed-other-subject")
	insertChangeObservation(t, st, "filter-other-machine", "filter-noise", KindCredential, "wanted",
		expired, fmtTime(now.Add(-time.Minute)), "malformed-other-machine")
	if _, err := st.DB().Exec(`
INSERT INTO machine_state_history(machine_id,state,reason,entered_at)
VALUES(?,?,?,?)`, "filter-noise", state.Online, "unselected malformed state", "malformed-state-time"); err != nil {
		t.Fatal(err)
	}

	result, err := st.ReadChanges(ChangeReadRequest{
		From: now.Add(-time.Hour), To: now, MachineID: "filter-target",
		Kinds: []string{KindCredential}, Subject: "wanted",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.MalformedTimestamps != 0 || result.UnplaceableTimestamps != 0 || len(result.Records) != 1 {
		t.Fatalf("exact filtered result leaked other evidence: %+v", result)
	}
	record := result.Records[0]
	if record.MachineID != "filter-target" || record.Kind != KindCredential || record.Subject != "wanted" {
		t.Fatalf("exact filtered record=%+v", record)
	}

	// A registry-only lookup for an absent machine must not even audit state or
	// observation sources. Their deliberately malformed coordinates above are
	// an observable tripwire for an accidental source scan.
	result, err = st.ReadChanges(ChangeReadRequest{
		From: now.Add(-time.Hour), To: now, MachineID: "does-not-exist",
		Kinds: []string{ChangeReadKindRegistry}, Subject: ChangeReadSubjectLife,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Records) != 0 || result.MalformedTimestamps != 0 || result.UnplaceableTimestamps != 0 {
		t.Fatalf("absent registry lookup scanned unselected sources: %+v", result)
	}
}

func TestReadChangesObservationBoundsFailClosedBeforePayload(t *testing.T) {
	now := time.Date(2026, 9, 8, 19, 20, 0, 0, time.UTC)

	t.Run("raw rows including repeated key", func(t *testing.T) {
		st := newTestStore(t)
		mustUpsertChangeMachine(t, st, "raw-cap", now)
		large := strings.Repeat("x", 4096)
		for i := 0; i < 3; i++ {
			insertChangeObservation(t, st, fmt.Sprintf("raw-cap-%d", i), "raw-cap", KindCredential, "codex",
				large, fmtTime(now.Add(time.Duration(i+1)*time.Second)), fmtTime(now.Add(time.Duration(i+1)*time.Second)))
		}
		selection := mustChangeReadSelection(t, ChangeReadRequest{
			MachineID: "raw-cap", Kinds: []string{KindCredential}, Subject: "codex",
		})
		selection.windowRowLimit = 2
		selection.payloadByteLimit = 0
		_, _, _, err := readObservationEndpointsForTest(t, st, now, now.Add(time.Minute), selection)
		if !errors.Is(err, ErrChangeReadTooBroad) || !strings.Contains(err.Error(), "window rows") {
			t.Fatalf("same-key raw row cap error=%v", err)
		}
	})

	t.Run("distinct endpoint keys", func(t *testing.T) {
		st := newTestStore(t)
		mustUpsertChangeMachine(t, st, "key-cap", now)
		payload := mustChangePayload(t, model.Identity{OS: "debian"})
		for i, subject := range []string{"one", "two"} {
			insertChangeObservation(t, st, fmt.Sprintf("key-cap-%d", i), "key-cap", KindIdentity, subject,
				payload, fmtTime(now.Add(time.Duration(i+1)*time.Second)), fmtTime(now.Add(time.Duration(i+1)*time.Second)))
		}
		selection := mustChangeReadSelection(t, ChangeReadRequest{MachineID: "key-cap", Kinds: []string{KindIdentity}})
		selection.endpointKeyLimit = 1
		selection.payloadByteLimit = 0
		_, _, _, err := readObservationEndpointsForTest(t, st, now, now.Add(time.Minute), selection)
		if !errors.Is(err, ErrChangeReadTooBroad) || !strings.Contains(err.Error(), "endpoint keys") {
			t.Fatalf("endpoint key cap error=%v", err)
		}
	})

	t.Run("endpoint payload bytes", func(t *testing.T) {
		st := newTestStore(t)
		mustUpsertChangeMachine(t, st, "payload-cap", now)
		payload := mustChangePayload(t, model.Credential{
			Provider: "codex", Status: model.CredConfigured, Note: strings.Repeat("x", 512),
		})
		insertChangeObservation(t, st, "payload-cap-current", "payload-cap", KindCredential, "codex",
			payload, fmtTime(now.Add(time.Second)), fmtTime(now.Add(time.Second)))
		selection := mustChangeReadSelection(t, ChangeReadRequest{
			MachineID: "payload-cap", Kinds: []string{KindCredential}, Subject: "codex",
		})
		selection.payloadByteLimit = len(payload) - 1
		_, _, _, err := readObservationEndpointsForTest(t, st, now, now.Add(time.Minute), selection)
		if !errors.Is(err, ErrChangeReadTooBroad) || !strings.Contains(err.Error(), "payload bytes") {
			t.Fatalf("endpoint payload cap error=%v", err)
		}
	})

	t.Run("candidate metadata bytes", func(t *testing.T) {
		st := newTestStore(t)
		mustUpsertChangeMachine(t, st, "metadata-cap", now)
		payload := mustChangePayload(t, model.Identity{OS: "debian"})
		insertChangeObservation(t, st, "metadata-cap-current", "metadata-cap", KindIdentity,
			strings.Repeat("s", 256), payload, fmtTime(now.Add(time.Second)), fmtTime(now.Add(time.Second)))
		selection := mustChangeReadSelection(t, ChangeReadRequest{MachineID: "metadata-cap", Kinds: []string{KindIdentity}})
		selection.metadataByteLimit = 32
		_, _, _, err := readObservationEndpointsForTest(t, st, now, now.Add(time.Minute), selection)
		if !errors.Is(err, ErrChangeReadTooBroad) || !strings.Contains(err.Error(), "candidate metadata") {
			t.Fatalf("candidate metadata cap error=%v", err)
		}
	})

	t.Run("timestamp audit rows", func(t *testing.T) {
		st := newTestStore(t)
		mustUpsertChangeMachine(t, st, "audit-cap", now)
		payload := mustChangePayload(t, model.Identity{OS: "debian"})
		for i := 0; i < 3; i++ {
			at := now.Add(-time.Duration(i+1) * time.Hour)
			insertChangeObservation(t, st, fmt.Sprintf("audit-cap-%d", i), "audit-cap", KindIdentity,
				"identity", payload, fmtTime(at), fmtTime(at))
		}
		selection := mustChangeReadSelection(t, ChangeReadRequest{MachineID: "audit-cap", Kinds: []string{KindIdentity}})
		selection.timestampAuditLimit = 2
		_, _, _, err := readObservationEndpointsForTest(t, st, now, now.Add(time.Minute), selection)
		if !errors.Is(err, ErrChangeReadTooBroad) || !strings.Contains(err.Error(), "timestamp audit rows") {
			t.Fatalf("timestamp audit cap error=%v", err)
		}
	})
}

func TestReadChangesObservationCostIgnoresPreWindowPayloadHistory(t *testing.T) {
	st := newTestStore(t)
	from := time.Date(2026, 9, 8, 19, 25, 0, 0, time.UTC)
	machineID := "bounded-history"
	mustUpsertChangeMachine(t, st, machineID, from)
	for i := 0; i < 20; i++ {
		payload := mustChangePayload(t, model.Credential{
			Provider: "codex", Status: model.CredConfigured,
			Note: strings.Repeat(string(rune('a'+i)), 1024),
		})
		at := from.Add(-time.Duration(20-i) * time.Minute)
		insertChangeObservation(t, st, fmt.Sprintf("history-%02d", i), machineID, KindCredential, "codex",
			payload, fmtTime(at), fmtTime(at))
	}
	currentPayload := mustChangePayload(t, model.Credential{
		Provider: "codex", Status: model.CredExpired, Note: strings.Repeat("z", 1024),
	})
	insertChangeObservation(t, st, "history-current", machineID, KindCredential, "codex",
		currentPayload, fmtTime(from.Add(time.Minute)), fmtTime(from.Add(time.Minute)))

	selection := mustChangeReadSelection(t, ChangeReadRequest{
		MachineID: machineID, Kinds: []string{KindCredential}, Subject: "codex",
	})
	// The retained raw history is over 20 KiB. Only the latest baseline and
	// current endpoint are loaded, so a 3 KiB endpoint budget is sufficient.
	selection.payloadByteLimit = 3 << 10
	records, malformed, unplaceable, err := readObservationEndpointsForTest(
		t, st, from, from.Add(time.Hour), selection)
	if err != nil {
		t.Fatalf("bounded endpoint read loaded pre-window history: %v", err)
	}
	if malformed != 0 || unplaceable != 0 || len(records) != 1 || !records[0].FromKnown {
		t.Fatalf("bounded history endpoints=%+v malformed=%d unplaceable=%d", records, malformed, unplaceable)
	}
}

func TestReadChangesTransitionCapReturnsNoPartialResult(t *testing.T) {
	st := newTestStore(t)
	from := time.Date(2026, 9, 8, 19, 27, 0, 0, time.UTC)
	machineID := "transition-cap"
	mustUpsertChangeMachine(t, st, machineID, from)
	if err := st.RecordStateTransition(machineID, state.Online, "one", from.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordStateTransition(machineID, state.Degraded, "two", from.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	tx, err := st.DB().BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	ceilings, err := currentChangeReadCeilings(context.Background(), tx)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := readStateChanges(context.Background(), tx, from, from.Add(time.Hour),
		ceilings.StateHistory, machineID, MaxChangeReadTimestampAuditRows, 1); !errors.Is(err, ErrChangeReadTooBroad) {
		t.Fatalf("transition cap error=%v", err)
	}
}

func TestReadChangesQueryPlansUseBoundedIndexes(t *testing.T) {
	st := newTestStore(t)
	canonical := changeReadCanonicalTimeSQL("received_at")
	broadWindow := explainChangeReadPlan(t, st, `
SELECT rowid,machine_id,kind,subject,received_at
  FROM observed_state INDEXED BY ix_observed_changes
 WHERE rowid <= ? AND kind IN (?) AND received_at > ? AND received_at <= ?
   AND `+canonical+`
 LIMIT ?`, 1, KindCredential, "2026-09-08T00:00:00Z", "2026-09-09T00:00:00Z", 2)
	if !strings.Contains(broadWindow, "COVERING INDEX ix_observed_changes") {
		t.Fatalf("broad observation window is not covering/indexed: %s", broadWindow)
	}
	machineWindow := explainChangeReadPlan(t, st, `
SELECT rowid,machine_id,kind,subject,received_at
  FROM observed_state INDEXED BY ix_observed_changes_machine
 WHERE rowid <= ? AND kind IN (?) AND machine_id = ?
   AND received_at > ? AND received_at <= ? AND `+canonical+`
 LIMIT ?`, 1, KindCredential, "machine", "2026-09-08T00:00:00Z", "2026-09-09T00:00:00Z", 2)
	if !strings.Contains(machineWindow, "COVERING INDEX ix_observed_changes_machine") {
		t.Fatalf("machine observation window is not covering/indexed: %s", machineWindow)
	}
	subjectWindow := explainChangeReadPlan(t, st, `
SELECT rowid,machine_id,kind,subject,received_at
  FROM observed_state INDEXED BY ix_observed_changes_subject
 WHERE rowid <= ? AND kind IN (?) AND subject = ? AND received_at > ? AND received_at <= ?
 LIMIT ?`, 1, KindCredential, "codex", "2026-09-08T00:00:00Z", "2026-09-09T00:00:00Z", 2)
	if !strings.Contains(subjectWindow, "COVERING INDEX ix_observed_changes_subject") {
		t.Fatalf("subject observation window is not covering/indexed: %s", subjectWindow)
	}
	baselineCoordinate := explainChangeReadPlan(t, st, `
SELECT MAX(received_at)
  FROM observed_state INDEXED BY ix_observed_prune
 WHERE rowid <= ? AND machine_id = ? AND kind = ? AND subject = ? AND received_at <= ?`,
		1, "machine", KindCredential, "codex", "2026-09-08T00:00:00Z")
	if !strings.Contains(baselineCoordinate, "COVERING INDEX ix_observed_prune") {
		t.Fatalf("observation baseline coordinate is not covering: %s", baselineCoordinate)
	}
	baseline := explainChangeReadPlan(t, st, `
SELECT rowid,length(CAST(payload AS BLOB)),measured_at,received_at
  FROM observed_state INDEXED BY ix_observed_prune
	WHERE rowid <= ? AND machine_id = ? AND kind = ? AND subject = ? AND received_at = ?
 ORDER BY rowid DESC LIMIT 1`, 1, "machine", KindCredential, "codex", "2026-09-08T00:00:00Z")
	if !strings.Contains(baseline, "INDEX ix_observed_prune") || strings.Contains(baseline, "TEMP B-TREE") {
		t.Fatalf("observation baseline has an unbounded same-second sort: %s", baseline)
	}
	stateWindow := explainChangeReadPlan(t, st, `
SELECT event_id,machine_id,state,entered_at
  FROM machine_state_transition_events INDEXED BY ix_state_transition_change_read
 WHERE event_id <= ? AND entered_at > ? AND entered_at <= ?
	ORDER BY entered_at DESC,event_id DESC LIMIT ?`, 1, "2026-09-08T00:00:00Z", "2026-09-09T00:00:00Z", 2)
	if !strings.Contains(stateWindow, "INDEX ix_state_transition_change_read") {
		t.Fatalf("state window is not indexed: %s", stateWindow)
	}
}

func TestReadChangesBusyAndCancellationAreTyped(t *testing.T) {
	st := newTestStore(t)
	now := time.Date(2026, 9, 8, 19, 29, 0, 0, time.UTC)
	if !st.acquireChangeRead() || !st.acquireChangeRead() {
		t.Fatal("could not occupy both change reader slots")
	}
	defer func() {
		for len(st.changeReadSlots) > 0 {
			st.releaseChangeRead()
		}
	}()
	if _, err := st.ReadChanges(ChangeReadRequest{From: now.Add(-time.Hour), To: now}); !errors.Is(err, ErrChangeReadBusy) {
		t.Fatalf("saturated reader error=%v", err)
	}
	st.releaseChangeRead()
	st.releaseChangeRead()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := st.ReadChangesContext(ctx, ChangeReadRequest{From: now.Add(-time.Hour), To: now}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled reader error=%v", err)
	}
	if _, err := st.ReadChanges(ChangeReadRequest{From: now.Add(-time.Hour), To: now}); err != nil {
		t.Fatalf("reader slot leaked after errors: %v", err)
	}
}

func TestChangeReadObservationFingerprintTracksOnlyOperatorEvidence(t *testing.T) {
	now := time.Date(2026, 9, 8, 20, 0, 0, 0, time.UTC)
	one := 1
	trueValue := true

	tests := []struct {
		name  string
		kind  string
		from  any
		to    any
		equal bool
	}{
		{
			name: "identity safe OS field", kind: KindIdentity,
			from: model.Identity{Hostname: "host", OS: "debian", Kernel: "6.1", Arch: "amd64"},
			to:   model.Identity{Hostname: "host", OS: "ubuntu", Kernel: "6.1", Arch: "amd64"},
		},
		{
			name: "identity redacted host coordinate", kind: KindIdentity,
			from: model.Identity{Hostname: "old", OS: "debian", MachineIDHint: "id"},
			to:   model.Identity{Hostname: "new", OS: "debian", MachineIDHint: "id"},
		},
		{
			name: "credential redacted account coordinate", kind: KindCredential,
			from: model.Credential{Provider: "codex", Status: model.CredConfigured, ActiveAccountID: "one", AccountCount: 2},
			to:   model.Credential{Provider: "codex", Status: model.CredConfigured, ActiveAccountID: "two", AccountCount: 2},
		},
		{
			name: "credential free text excluded", kind: KindCredential, equal: true,
			from: model.Credential{Provider: "codex", Status: model.CredUnknown, Note: "old note", LastError: "old error"},
			to:   model.Credential{Provider: "codex", Status: model.CredUnknown, Note: "new note", LastError: "new error"},
		},
		{
			name: "credential instant canonicalized", kind: KindCredential, equal: true,
			from: model.Credential{Provider: "codex", Status: model.CredConfigured, ExpiresAt: timePointer(now)},
			to:   model.Credential{Provider: "codex", Status: model.CredConfigured, ExpiresAt: timePointer(now.In(time.FixedZone("offset", -4*60*60)))},
		},
		{
			name: "CLI safe reachability fields", kind: KindCLITool,
			from: model.CLITool{Name: "codex", Present: true, OnPath: true, DaemonReach: model.DaemonReachSame},
			to:   model.CLITool{Name: "codex", Present: true, OnPath: false, DaemonReach: model.DaemonReachMissing},
		},
		{
			name: "CLI redacted execution coordinate", kind: KindCLITool,
			from: model.CLITool{Name: "codex", Present: true, Path: "/old", RealPath: "/real"},
			to:   model.CLITool{Name: "codex", Present: true, Path: "/new", RealPath: "/real"},
		},
		{
			name: "CLI volatile PID and free reason excluded", kind: KindCLITool, equal: true,
			from: model.CLITool{Name: "codex", Present: true, RunningPID: 10, RunningReason: "old"},
			to:   model.CLITool{Name: "codex", Present: true, RunningPID: 99, RunningReason: "new"},
		},
		{
			name: "systemd active-enter evidence", kind: KindSystemd,
			from: model.Unit{Name: "agent.service", Present: true, ActiveState: "active", ActiveEnterTimestamp: timePointer(now.Add(-time.Hour)), MainPID: 10},
			to:   model.Unit{Name: "agent.service", Present: true, ActiveState: "active", ActiveEnterTimestamp: timePointer(now), MainPID: 99},
		},
		{
			name: "systemd volatile PID excluded", kind: KindSystemd, equal: true,
			from: model.Unit{Name: "agent.service", Present: true, ActiveState: "active", MainPID: 10},
			to:   model.Unit{Name: "agent.service", Present: true, ActiveState: "active", MainPID: 99},
		},
		{
			name: "systemd 未量到的 Measured 與 Reason 不進指紋", kind: KindSystemd, equal: true,
			from: model.Unit{Name: "agent.service", Reason: "Failed to connect to bus"},
			to:   model.Unit{Name: "agent.service", Reason: "Transport endpoint is not connected"},
		},
		{
			name: "systemd 存在與未量到的 Present 仍進指紋", kind: KindSystemd,
			from: model.Unit{Name: "agent.service", Present: true, Measured: true},
			to:   model.Unit{Name: "agent.service", Reason: "Failed to connect to bus"},
		},
		{
			name: "OpenClaw redacted install coordinate", kind: KindOpenClaw,
			from: model.OpenClaw{Present: true, Install: &model.OpenClawInstall{UnitFound: true, RunningDir: "/old", NRestarts: &one, RunningDirWritable: &trueValue}},
			to:   model.OpenClaw{Present: true, Install: &model.OpenClawInstall{UnitFound: true, RunningDir: "/new", NRestarts: &one, RunningDirWritable: &trueValue}},
		},
		{
			name: "OpenClaw run and volatile evidence excluded", kind: KindOpenClaw, equal: true,
			from: model.OpenClaw{Present: true, Reason: "old free reason", CrashBundles: 1, Install: &model.OpenClawInstall{UnitFound: true, MainPID: 10, DiskFreeBytes: 100, DiskFreeMeasured: true}, DB: &model.OpenClawDB{Present: true, RecentSummaries: []model.RunSummary{{JobID: "job", Summary: "old log"}}}},
			to:   model.OpenClaw{Present: true, Reason: "new free reason", CrashBundles: 2, Install: &model.OpenClawInstall{UnitFound: true, MainPID: 99, DiskFreeBytes: 200, DiskFreeMeasured: true}, DB: &model.OpenClawDB{Present: true, RecentSummaries: []model.RunSummary{{JobID: "job", Summary: "new log"}}}},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			from, fromValid := changeReadObservationFingerprint(test.kind, mustChangePayload(t, test.from))
			to, toValid := changeReadObservationFingerprint(test.kind, mustChangePayload(t, test.to))
			if !fromValid || !toValid {
				t.Fatalf("typed payload classified malformed: from=%t to=%t", fromValid, toValid)
			}
			if got := from == to; got != test.equal {
				t.Fatalf("fingerprint equal=%t, want %t", got, test.equal)
			}
		})
	}

	largeMalformed := strings.Repeat("x", 4<<20) + "{"
	fingerprint, valid := changeReadObservationFingerprint(KindCredential, largeMalformed)
	if valid || len(fingerprint) != len("malformed\x00")+sha256.Size || strings.Contains(fingerprint, strings.Repeat("x", 64)) {
		t.Fatalf("malformed fingerprint was not bounded: valid=%t bytes=%d", valid, len(fingerprint))
	}
}

const day = 24 * time.Hour

func timePointer(value time.Time) *time.Time { return &value }

func mustUpsertChangeMachine(t *testing.T, st *Store, machineID string, now time.Time) {
	t.Helper()
	if err := st.UpsertMachine(Machine{
		MachineID: machineID, DisplayName: machineID, Expected: true, CreatedAt: now.Add(-day),
	}); err != nil {
		t.Fatal(err)
	}
}

func mustChangeReadSelection(t *testing.T, request ChangeReadRequest) changeReadSelection {
	t.Helper()
	selection, err := newChangeReadSelection(request)
	if err != nil {
		t.Fatal(err)
	}
	return selection
}

func readObservationEndpointsForTest(t *testing.T, st *Store, from, to time.Time, selection changeReadSelection) ([]ChangeReadRecord, int, int, error) {
	t.Helper()
	ctx := context.Background()
	tx, err := st.DB().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	ceilings, err := currentChangeReadCeilings(ctx, tx)
	if err != nil {
		t.Fatal(err)
	}
	return readObservationEndpointChanges(ctx, tx, from, to, ceilings.ObservedState, false, selection)
}

func explainChangeReadPlan(t *testing.T, st *Store, query string, args ...any) string {
	t.Helper()
	rows, err := st.DB().Query(`EXPLAIN QUERY PLAN `+query, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var details []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		details = append(details, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return strings.Join(details, "; ")
}

func insertChangeObservation(t *testing.T, st *Store, observationID, machineID, kind, subject, payload, measuredAt, receivedAt string) {
	t.Helper()
	if _, err := st.DB().Exec(`
INSERT INTO observed_state(observation_id,machine_id,measured_at,received_at,kind,subject,payload,source)
VALUES(?,?,?,?,?,?,?,?)`, observationID, machineID, measuredAt, receivedAt, kind, subject, payload, SourceAgentMeasurement); err != nil {
		t.Fatalf("insert observation %s: %v", observationID, err)
	}
}

func mustChangePayload(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func changeRecordByKey(t *testing.T, records []ChangeReadRecord, kind, subject string) ChangeReadRecord {
	t.Helper()
	for _, record := range records {
		if record.Kind == kind && record.Subject == subject {
			return record
		}
	}
	t.Fatalf("missing change record %s/%s in %+v", kind, subject, records)
	return ChangeReadRecord{}
}

func changeRecordBySource(t *testing.T, records []ChangeReadRecord, source ChangeReadSource) ChangeReadRecord {
	t.Helper()
	for _, record := range records {
		if record.Key.Source == source {
			return record
		}
	}
	t.Fatalf("missing change source %s in %+v", source, records)
	return ChangeReadRecord{}
}

func changeRecordByTransition(t *testing.T, records []ChangeReadRecord, from, to string) ChangeReadRecord {
	t.Helper()
	for _, record := range records {
		if record.From == from && record.To == to {
			return record
		}
	}
	t.Fatalf("missing change transition %s -> %s in %+v", from, to, records)
	return ChangeReadRecord{}
}

func changeReadKeys(records []ChangeReadRecord) []ChangeReadSourceKey {
	keys := make([]ChangeReadSourceKey, len(records))
	for i := range records {
		keys[i] = records[i].Key
	}
	return keys
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
