package store

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/model"
)

const (
	identityLatchA = "aaaaaaaaaaaa"
	identityLatchB = "bbbbbbbbbbbb"
)

func recordIdentityLatchObservation(t *testing.T, s *Store, machineID, hint string, at time.Time) {
	t.Helper()
	b := healthyBatch(at)
	b.Identity.MachineIDHint = hint
	if err := s.RecordObservation(machineID, b, at); err != nil {
		t.Fatalf("record identity %q at %s: %v", hint, at, err)
	}
}

func identityLatchCounts(t *testing.T, s *Store, machineID string) map[string]int {
	t.Helper()
	rows, err := s.DB().Query(`SELECT hint,observation_count
		FROM machine_identity_hints WHERE machine_id=? ORDER BY hint`, machineID)
	if err != nil {
		t.Fatalf("query identity latch: %v", err)
	}
	defer rows.Close()
	got := make(map[string]int)
	for rows.Next() {
		var hint string
		var count int
		if err := rows.Scan(&hint, &count); err != nil {
			t.Fatalf("scan identity latch: %v", err)
		}
		got[hint] = count
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read identity latch: %v", err)
	}
	return got
}

// A conflict is a durable identity/security fact. A noisy original machine must
// not be able to push the clone's hint out of a bounded recent-observation window.
func TestIdentityConflictLatchSurvivesMoreThanOneHundredLaterOriginalHints(t *testing.T) {
	s := newTestStore(t)
	base := time.Date(2026, 9, 6, 9, 0, 0, 0, time.UTC)
	id := mustEnroll(t, s, "samplehub1", base.Add(-time.Hour))

	recordIdentityLatchObservation(t, s, id, identityLatchA, base)
	recordIdentityLatchObservation(t, s, id, identityLatchB, base.Add(time.Minute))
	for i := 0; i < 101; i++ {
		recordIdentityLatchObservation(t, s, id, identityLatchA,
			base.Add(time.Duration(i+2)*time.Minute))
	}

	// Put every raw identity row beyond a deliberately short (but still valid)
	// retention horizon. The latest A row is protected per observed_state group;
	// the older B row is not. Conflict must survive in the non-prunable latch.
	now := base.Add(10 * 24 * time.Hour)
	policy := DefaultRetention()
	policy.Observations = longestReadWindow + time.Hour
	report, err := s.Prune(now, policy, false)
	if err != nil {
		t.Fatalf("prune old identity observations: %v", err)
	}
	for _, count := range report.Counts {
		if count.Table == "machine_identity_hints" {
			t.Fatalf("identity latch appeared in prune report: %+v", count)
		}
	}
	if rawB := countRows(t, s, `SELECT COUNT(*) FROM observed_state
		WHERE machine_id=? AND kind=? AND payload LIKE ?`, id, KindIdentity, "%"+identityLatchB+"%"); rawB != 0 {
		t.Fatalf("retention left %d raw B identity rows; test did not exercise the durable latch", rawB)
	}

	facts, err := s.Facts(id, now)
	if err != nil {
		t.Fatalf("facts: %v", err)
	}
	if !facts.Conflict {
		t.Fatal("101 later A observations erased the latched A/B identity conflict")
	}
	detail, err := s.Detail(id, now)
	if err != nil {
		t.Fatalf("detail: %v", err)
	}
	seen := make(map[string]bool)
	for _, hint := range detail.IdentityHints {
		seen[hint.Hint] = true
	}
	if len(detail.IdentityHints) != 2 || !seen[identityLatchA] || !seen[identityLatchB] {
		t.Fatalf("detail identity hints = %+v, want durable A and B", detail.IdentityHints)
	}
	counts := identityLatchCounts(t, s, id)
	if counts[identityLatchA] != 102 || counts[identityLatchB] != 1 {
		t.Fatalf("identity latch counts = %#v, want A=102 B=1", counts)
	}
}

// Simulate upgrading a database whose append-only raw identity rows predate the
// projection. Open must backfill both hints, and its repeatable migration must
// not count those same retained rows again on every restart.
func TestOpenBackfillsLegacyIdentityLatchIdempotently(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open seed database: %v", err)
	}
	base := time.Date(2026, 9, 6, 10, 0, 0, 0, time.UTC)
	id := mustEnroll(t, s, "samplehub1", base.Add(-time.Hour))
	recordIdentityLatchObservation(t, s, id, identityLatchA, base)
	recordIdentityLatchObservation(t, s, id, identityLatchB, base.Add(time.Minute))
	if _, err := s.DB().Exec(`DROP TABLE machine_identity_hints`); err != nil {
		t.Fatalf("remove projection to model legacy DB: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close seed database: %v", err)
	}

	s, err = Open(path)
	if err != nil {
		t.Fatalf("open legacy database: %v", err)
	}
	firstCounts := identityLatchCounts(t, s, id)
	if firstCounts[identityLatchA] != 1 || firstCounts[identityLatchB] != 1 || len(firstCounts) != 2 {
		t.Fatalf("backfilled identity latch = %#v, want A=1 B=1", firstCounts)
	}
	facts, err := s.Facts(id, base.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("facts after backfill: %v", err)
	}
	if !facts.Conflict {
		t.Fatal("legacy A/B observations were backfilled but did not latch conflict")
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close first migrated database: %v", err)
	}

	s, err = Open(path)
	if err != nil {
		t.Fatalf("reopen migrated database: %v", err)
	}
	defer s.Close()
	secondCounts := identityLatchCounts(t, s, id)
	if secondCounts[identityLatchA] != 1 || secondCounts[identityLatchB] != 1 || len(secondCounts) != 2 {
		t.Fatalf("repeat Open double-counted identity backfill: first=%#v second=%#v", firstCounts, secondCounts)
	}
}

func TestIdentityLatchRepeatedSingleHintDoesNotCreateConflict(t *testing.T) {
	s := newTestStore(t)
	base := time.Date(2026, 9, 6, 11, 0, 0, 0, time.UTC)
	id := mustEnroll(t, s, "samplehub1", base.Add(-time.Hour))

	for i := 0; i < 125; i++ {
		recordIdentityLatchObservation(t, s, id, identityLatchA,
			base.Add(time.Duration(i)*time.Minute))
	}
	facts, err := s.Facts(id, base.Add(3*time.Hour))
	if err != nil {
		t.Fatalf("facts: %v", err)
	}
	if facts.Conflict {
		t.Fatal("repeated observations of one identity were treated as a conflict")
	}
	detail, err := s.Detail(id, base.Add(3*time.Hour))
	if err != nil {
		t.Fatalf("detail: %v", err)
	}
	if len(detail.IdentityHints) != 1 || detail.IdentityHints[0].Hint != identityLatchA {
		t.Fatalf("detail identity hints = %+v, want only A", detail.IdentityHints)
	}
	counts := identityLatchCounts(t, s, id)
	if len(counts) != 1 || counts[identityLatchA] != 125 {
		t.Fatalf("identity latch counts = %#v, want only A=125", counts)
	}
}

func TestSecondIdentityInHealthyBatchCannotCloseExplicitWorkloadFailure(t *testing.T) {
	s := rolloutStore(t)
	addRolloutMachine(t, s, "cnode", "samplehub1", false)
	base := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	if err := s.RecordCheckin("cnode", model.Checkin{SentAt: base, AgentStartedAt: base}, base); err != nil {
		t.Fatalf("first checkin: %v", err)
	}

	bad := healthyBatch(base)
	bad.Identity.MachineIDHint = identityLatchA
	bad.OpenClaw.Install.MainPID = 0
	stampCurrentWorkloadPolicy(t, s, "samplehub1", &bad)
	if err := s.RecordObservation("cnode", bad, base); err != nil {
		t.Fatalf("explicit workload failure: %v", err)
	}

	recoveryAt := base.Add(time.Minute)
	if err := s.RecordCheckin("cnode", model.Checkin{SentAt: recoveryAt, AgentStartedAt: base}, recoveryAt); err != nil {
		t.Fatalf("recovery checkin: %v", err)
	}
	otherwiseHealthy := healthyBatch(recoveryAt)
	otherwiseHealthy.Identity.MachineIDHint = identityLatchB
	stampCurrentWorkloadPolicy(t, s, "samplehub1", &otherwiseHealthy)
	if err := s.RecordObservation("cnode", otherwiseHealthy, recoveryAt); err != nil {
		t.Fatalf("conflicting healthy observation: %v", err)
	}

	var lastSeen string
	var open int
	if err := s.DB().QueryRow(`SELECT last_seen_at,open FROM canary_silent_failures
		WHERE machine_id='cnode' ORDER BY first_seen_at DESC LIMIT 1`).Scan(&lastSeen, &open); err != nil {
		t.Fatalf("read failure span: %v", err)
	}
	if open != 1 || lastSeen != fmtTime(base) {
		t.Fatalf("second identity closed/changed explicit failure: last=%q open=%d, want %q/1",
			lastSeen, open, fmtTime(base))
	}
	facts, err := s.Facts("cnode", recoveryAt)
	if err != nil {
		t.Fatalf("facts: %v", err)
	}
	if !facts.Conflict {
		t.Fatal("same healthy batch's second identity was not visible as a conflict")
	}
}

func TestIdentityProjectionFailureRollsBackWholeObservationBatch(t *testing.T) {
	s := newTestStore(t)
	base := time.Date(2026, 9, 6, 13, 0, 0, 0, time.UTC)
	id := mustEnroll(t, s, "samplehub1", base.Add(-time.Hour))
	before := countRows(t, s, `SELECT COUNT(*) FROM observed_state WHERE machine_id=?`, id)
	if _, err := s.DB().Exec(`DROP TABLE machine_identity_hints`); err != nil {
		t.Fatalf("drop identity projection: %v", err)
	}

	b := healthyBatch(base)
	b.Identity.MachineIDHint = identityLatchA
	err := s.RecordObservation(id, b, base)
	if err == nil || !strings.Contains(err.Error(), "machine_identity_hints") {
		t.Fatalf("missing identity projection accepted observation: %v", err)
	}
	if after := countRows(t, s, `SELECT COUNT(*) FROM observed_state WHERE machine_id=?`, id); after != before {
		t.Fatalf("identity projection failure left raw rows: before=%d after=%d", before, after)
	}
	machine, err := s.GetMachine(id)
	if err != nil {
		t.Fatalf("get machine after rollback: %v", err)
	}
	if machine.MachineIDHint != "" {
		t.Fatalf("identity projection failure partially filled registry hint %q", machine.MachineIDHint)
	}
}
