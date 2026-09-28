package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestTicketLedgerUsesInclusiveReceivedWindowAndFixedSnapshot(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	to := time.Date(2026, 9, 11, 16, 0, 0, 0, time.UTC)
	from := to.Add(-24 * time.Hour)
	machineID := "ticket-window-machine"
	if err := st.UpsertMachine(Machine{
		MachineID: machineID, DisplayName: "Ticket Window", Expected: true, CreatedAt: from.Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	insert := func(id, received, measured string) {
		t.Helper()
		if _, err := st.DB().Exec(`INSERT INTO ticket_occupancy_observation
(observation_id,machine_id,provider,measured_at,received_at,occupant_evidence,process_alive,source)
VALUES(?,?,?,?,?,?,0,?)`, id, machineID, "openai", measured, received, id, "cron_run_logs"); err != nil {
			t.Fatal(err)
		}
	}
	insert("at-from", from.Format(time.RFC3339), "malformed")
	insert("at-to", to.Format(time.RFC3339), to.Add(72*time.Hour).Format(time.RFC3339))
	insert("before-from", from.Add(-time.Second).Format(time.RFC3339), to.Format(time.RFC3339))
	insert("after-to", to.Add(time.Second).Format(time.RFC3339), to.Format(time.RFC3339))

	report, err := st.TicketLedgerContext(context.Background(), to, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if !report.From.Equal(from) || !report.To.Equal(to) || report.CandidateRows != 2 || len(report.Rows) != 1 ||
		report.Rows[0].Runs != 2 || report.MalformedMeasuredAt != 1 || report.Rows[0].PeakPerHour != 1 ||
		!report.Rows[0].LastRunAt.Equal(to.Add(72*time.Hour)) {
		t.Fatalf("report=%+v", report)
	}
}

func TestTicketLedgerDenominatorUsesActiveLifecycleOnly(t *testing.T) {
	st := newTestStore(t)
	now := time.Date(2026, 9, 11, 16, 0, 0, 0, time.UTC)
	for _, machine := range []Machine{
		{MachineID: "legacy-false", DisplayName: "Legacy false", Expected: false, CreatedAt: now.Add(-time.Hour)},
		{MachineID: "active", DisplayName: "Active", Expected: true, CreatedAt: now.Add(-time.Hour)},
		{MachineID: "retired", DisplayName: "Retired", Expected: true, CreatedAt: now.Add(-time.Hour)},
	} {
		if err := st.UpsertMachine(machine); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.RetireMachine("retired", now.Add(-30*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordCheckin("legacy-false", healthyCheckin(now.Add(-time.Minute)), now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}

	report, err := st.TicketLedger(now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if report.RosterSize != 2 || report.ReportingSize != 1 {
		t.Fatalf("ticket lifecycle denominator=%+v, want roster=2 reporting=1", report)
	}
}

func TestTicketLedgerRejectsInvalidWindows(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, test := range []struct {
		now    time.Time
		window time.Duration
	}{
		{window: time.Hour}, {now: time.Now(), window: 0},
		{now: time.Now(), window: TicketReadMaxWindow + time.Second},
	} {
		if _, err := st.TicketLedgerContext(context.Background(), test.now, test.window); !errors.Is(err, ErrInvalidTicketRead) {
			t.Fatalf("now=%v window=%v err=%v", test.now, test.window, err)
		}
	}
}

func TestTicketReceivedIndexIsInstalled(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var count int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM pragma_index_info('ix_occupancy_received')
WHERE (seqno=0 AND name='received_at') OR (seqno=1 AND name='observation_id')`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("ix_occupancy_received canonical columns=%d want 2", count)
	}
}
