package store

import (
	"errors"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/model"
)

func TestAgentReadinessReturnsLatestHubReceipt(t *testing.T) {
	s := newTestStore(t)
	machineID, _, err := s.CreateEnrollTokenFor("bootstrap-target", time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	empty, err := s.AgentReadiness(machineID)
	if err != nil || empty.MachineID != machineID || empty.LastCheckinReceivedAt != nil ||
		empty.AgentStartedAt != nil || empty.AgentVersion != "" || empty.JobsEnabled != nil ||
		empty.DeviceSyncV1 != nil || empty.IdentityReceivedAt != nil || empty.IdentityOS != "" ||
		empty.IdentityMeasuredAt != nil || empty.IdentityArch != "" {
		t.Fatalf("empty readiness=%+v err=%v", empty, err)
	}

	firstAt := time.Date(2026, 9, 10, 18, 0, 0, 0, time.UTC)
	secondAt := firstAt.Add(time.Minute)
	disabled, enabled := false, true
	if err := s.RecordCheckin(machineID, model.Checkin{
		SentAt: firstAt, AgentStartedAt: firstAt.Add(-time.Hour), AgentVersion: "old", JobsEnabled: &disabled,
	}, firstAt); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordCheckin(machineID, model.Checkin{
		SentAt: secondAt, AgentStartedAt: secondAt.Add(-time.Hour), AgentVersion: "new", JobsEnabled: &enabled,
		DeviceSyncV1: true,
	}, secondAt); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordObservation(machineID, model.ObservationBatch{
		MeasuredAt: secondAt,
		Identity:   model.Identity{OS: "macOS 15.7", Arch: "arm64"},
	}, secondAt.Add(time.Second)); err != nil {
		t.Fatal(err)
	}

	got, err := s.AgentReadiness(machineID)
	if err != nil || got.LastCheckinReceivedAt == nil || !got.LastCheckinReceivedAt.Equal(secondAt) ||
		got.AgentStartedAt == nil || !got.AgentStartedAt.Equal(secondAt.Add(-time.Hour)) ||
		got.AgentVersion != "new" || got.JobsEnabled == nil || !*got.JobsEnabled ||
		got.DeviceSyncV1 == nil || !*got.DeviceSyncV1 || got.IdentityReceivedAt == nil ||
		!got.IdentityReceivedAt.Equal(secondAt.Add(time.Second)) || got.IdentityOS != "macOS 15.7" ||
		got.IdentityArch != "arm64" {
		t.Fatalf("latest readiness=%+v err=%v", got, err)
	}
}

func TestAgentReadinessIdentityEvidenceComesFromStoredAgentMeasurement(t *testing.T) {
	s := newTestStore(t)
	at := time.Date(2026, 9, 20, 9, 30, 0, 0, time.UTC)
	machineID := mustEnroll(t, s, "mac-launchd", at)
	batch := model.ObservationBatch{
		MeasuredAt: at,
		Identity:   model.Identity{OS: "macOS 15.7", Arch: "x86_64"},
	}
	if err := s.RecordObservation(machineID, batch, at); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(`INSERT INTO observed_state
		(observation_id,machine_id,measured_at,received_at,kind,subject,payload,source)
		VALUES(?,?,?,?,?,?,?,?)`, "task-identity", machineID, fmtTime(at.Add(time.Second)),
		fmtTime(at.Add(time.Second)), KindIdentity, KindIdentity,
		`{"os":"forged","arch":"forged"}`, SourceTaskEvent); err != nil {
		t.Fatal(err)
	}

	got, err := s.AgentReadiness(machineID)
	if err != nil || got.IdentityReceivedAt == nil || !got.IdentityReceivedAt.Equal(at) ||
		got.IdentityOS != "macOS 15.7" || got.IdentityArch != "x86_64" {
		t.Fatalf("identity evidence=%+v err=%v", got, err)
	}
}

func TestAgentReadinessKeepsOldAgentDeviceSyncCapabilityUnreported(t *testing.T) {
	s := newTestStore(t)
	machineID := mustEnroll(t, s, "old-agent", time.Date(2026, 9, 14, 8, 0, 0, 0, time.UTC))
	at := time.Date(2026, 9, 14, 8, 1, 0, 0, time.UTC)
	checkin := healthyCheckin(at)
	if err := s.RecordCheckin(machineID, checkin, at); err != nil {
		t.Fatal(err)
	}
	got, err := s.AgentReadiness(machineID)
	if err != nil || got.DeviceSyncV1 != nil {
		t.Fatalf("old-agent readiness=%+v err=%v", got, err)
	}
}

func TestAgentReadinessUsesOnlyTheLatestCheckinCapability(t *testing.T) {
	s := newTestStore(t)
	machineID := mustEnroll(t, s, "capability-roll-back", time.Date(2026, 9, 14, 9, 0, 0, 0, time.UTC))
	firstAt := time.Date(2026, 9, 14, 9, 1, 0, 0, time.UTC)
	first := healthyCheckin(firstAt)
	first.DeviceSyncV1 = true
	if err := s.RecordCheckin(machineID, first, firstAt); err != nil {
		t.Fatal(err)
	}
	secondAt := firstAt.Add(time.Minute)
	if err := s.RecordCheckin(machineID, healthyCheckin(secondAt), secondAt); err != nil {
		t.Fatal(err)
	}
	got, err := s.AgentReadiness(machineID)
	if err != nil || got.DeviceSyncV1 != nil {
		t.Fatalf("older capability leaked into latest readiness=%+v err=%v", got, err)
	}
}

func TestCheckinAndDeviceSyncCapabilityCommitAtomically(t *testing.T) {
	s := newTestStore(t)
	machineID := mustEnroll(t, s, "capability-atomic", time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC))
	if _, err := s.DB().Exec(`CREATE TRIGGER reject_device_sync_capability
	BEFORE INSERT ON machine_job_capabilities
	BEGIN SELECT RAISE(ABORT, 'capability ledger unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 9, 14, 10, 1, 0, 0, time.UTC)
	checkin := healthyCheckin(at)
	checkin.DeviceSyncV1 = true
	if err := s.RecordCheckin(machineID, checkin, at); err == nil {
		t.Fatal("capability write failure accepted a partial checkin")
	}
	if countRows(t, s, `SELECT COUNT(*) FROM machine_checkins WHERE machine_id=?`, machineID) != 0 ||
		countRows(t, s, `SELECT COUNT(*) FROM machine_job_capabilities WHERE machine_id=?`, machineID) != 0 {
		t.Fatal("failed capability write left a partial checkin")
	}
}

func TestSameCheckinReplayWithdrawsDeviceSyncCapabilityAsOneReceipt(t *testing.T) {
	s := newTestStore(t)
	machineID := mustEnroll(t, s, "capability-withdraw", time.Date(2026, 9, 14, 11, 0, 0, 0, time.UTC))
	sentAt := time.Date(2026, 9, 14, 11, 1, 0, 0, time.UTC)
	firstReceivedAt := sentAt.Add(time.Second)
	checkin := healthyCheckin(sentAt)
	checkin.AgentSeq, checkin.AgentVersion, checkin.DeviceSyncV1 = 1, "capable", true
	if err := s.RecordCheckin(machineID, checkin, firstReceivedAt); err != nil {
		t.Fatal(err)
	}
	disabled := false
	checkin.AgentSeq, checkin.AgentVersion = 2, "withdrawn"
	checkin.JobsEnabled, checkin.DeviceSyncV1 = &disabled, false
	if err := s.RecordCheckin(machineID, checkin, firstReceivedAt.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	readiness, err := s.AgentReadiness(machineID)
	if err != nil || readiness.AgentVersion != "withdrawn" || readiness.JobsEnabled == nil ||
		*readiness.JobsEnabled || readiness.DeviceSyncV1 != nil || readiness.LastCheckinReceivedAt == nil ||
		!readiness.LastCheckinReceivedAt.Equal(firstReceivedAt) {
		t.Fatalf("readiness=%+v err=%v", readiness, err)
	}
	if countRows(t, s, `SELECT COUNT(*) FROM machine_checkins WHERE machine_id=?`, machineID) != 1 ||
		countRows(t, s, `SELECT COUNT(*) FROM machine_job_capabilities WHERE machine_id=?`, machineID) != 0 {
		t.Fatal("same-checkin replay left mixed capability receipt")
	}
	preview, err := s.PreviewOperatorDeviceSync(machineID, model.DeviceSyncDefaultTimeoutSeconds)
	if err != nil || preview.DeviceSyncV1 != nil || preview.JobsEnabled == nil || *preview.JobsEnabled ||
		len(preview.Blockers) != 2 || preview.Blockers[0] != OperatorDeviceSyncBlockerCapabilityUnconfirmed ||
		preview.Blockers[1] != OperatorDeviceSyncBlockerExecutionDisabled {
		t.Fatalf("withdrawn capability preview=%+v err=%v", preview, err)
	}
}

func TestAgentReadinessUnknownMachine(t *testing.T) {
	s := newTestStore(t)
	got, err := s.AgentReadiness("no-such-machine")
	if !errors.Is(err, ErrNotFound) || got != (AgentReadiness{}) {
		t.Fatalf("unknown machine readiness=%+v err=%v", got, err)
	}
}

func TestAgentReadinessKeepsNullJobsEnabledUnreported(t *testing.T) {
	s := newTestStore(t)
	at := time.Date(2026, 9, 14, 8, 2, 0, 0, time.UTC)
	machineID := mustEnroll(t, s, "null-jobs", at)
	checkin := healthyCheckin(at)
	checkin.JobsEnabled = nil
	if err := s.RecordCheckin(machineID, checkin, at); err != nil {
		t.Fatal(err)
	}
	got, err := s.AgentReadiness(machineID)
	if err != nil || got.JobsEnabled != nil || got.DeviceSyncV1 != nil {
		t.Fatalf("null jobs_enabled readiness=%+v err=%v", got, err)
	}
}

func TestAgentReadinessSameSecondTieUsesLaterRowid(t *testing.T) {
	s := newTestStore(t)
	at := time.Date(2026, 9, 14, 13, 0, 0, 0, time.UTC)
	machineID := mustEnroll(t, s, "rowid-tie", at)
	first := healthyCheckin(at)
	first.AgentVersion = "first"
	first.DeviceSyncV1 = true
	if err := s.RecordCheckin(machineID, first, at); err != nil {
		t.Fatal(err)
	}
	secondSent := at.Add(time.Second)
	second := healthyCheckin(secondSent)
	second.AgentVersion = "second"
	disabled := false
	second.JobsEnabled = &disabled
	if err := s.RecordCheckin(machineID, second, at); err != nil {
		t.Fatal(err)
	}
	got, err := s.AgentReadiness(machineID)
	if err != nil || got.AgentVersion != "second" || got.JobsEnabled == nil || *got.JobsEnabled ||
		got.DeviceSyncV1 != nil || got.LastCheckinReceivedAt == nil || !got.LastCheckinReceivedAt.Equal(at) {
		t.Fatalf("rowid tie readiness=%+v err=%v", got, err)
	}
}

func TestAgentReadinessRejectsNoncanonicalCheckinProjection(t *testing.T) {
	tests := []struct {
		name   string
		insert func(*testing.T, *Store, string)
	}{
		{
			name: "offset received_at sorts before later UTC instant",
			insert: func(t *testing.T, st *Store, machineID string) {
				older := time.Date(2026, 9, 14, 11, 59, 0, 0, time.UTC)
				checkin := healthyCheckin(older)
				checkin.DeviceSyncV1 = true
				if err := st.RecordCheckin(machineID, checkin, older); err != nil {
					t.Fatal(err)
				}
				if _, err := st.DB().Exec(`INSERT INTO machine_checkins
					(machine_id,sent_at,received_at,jobs_enabled) VALUES (?,?,?,0)`,
					machineID, "2026-09-14T11:59:30Z", "2026-09-14T10:59:30-01:00"); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "selected sent_at has offset",
			insert: func(t *testing.T, st *Store, machineID string) {
				const sentAt = "2026-09-14T12:59:30+01:00"
				if _, err := st.DB().Exec(`INSERT INTO machine_checkins
					(machine_id,sent_at,received_at,jobs_enabled) VALUES (?,?,?,1)`,
					machineID, sentAt, "2026-09-14T11:59:30Z"); err != nil {
					t.Fatal(err)
				}
				if _, err := st.DB().Exec(`INSERT INTO machine_job_capabilities
					(machine_id,sent_at,capability,supported) VALUES (?,?,?,1)`,
					machineID, sentAt, model.DeviceSyncJobKind); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s := newTestStore(t)
			machineID := mustEnroll(t, s, "readiness-noncanonical-"+test.name,
				time.Date(2026, 9, 14, 11, 0, 0, 0, time.UTC))
			test.insert(t, s, machineID)
			got, err := s.AgentReadiness(machineID)
			if err == nil || err.Error() != "store: agent readiness check-in projection is invalid" {
				t.Fatalf("readiness=%+v err=%v, want invalid projection", got, err)
			}
		})
	}
}

func TestFailedCapabilityReplacementPreservesPriorCheckinReceipt(t *testing.T) {
	tests := []struct {
		name         string
		trigger      string
		deviceSyncV1 bool
	}{
		{
			name: "withdraw delete",
			trigger: `CREATE TRIGGER reject_device_sync_capability_delete
				BEFORE DELETE ON machine_job_capabilities
				BEGIN SELECT RAISE(ABORT, 'capability delete unavailable'); END`,
		},
		{
			name:         "replace insert",
			deviceSyncV1: true,
			trigger: `CREATE TRIGGER reject_device_sync_capability_insert
				BEFORE INSERT ON machine_job_capabilities
				BEGIN SELECT RAISE(ABORT, 'capability insert unavailable'); END`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s := newTestStore(t)
			machineID := mustEnroll(t, s, "capability-replace-"+test.name,
				time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC))
			sentAt := time.Date(2026, 9, 14, 12, 1, 0, 0, time.UTC)
			firstReceivedAt := sentAt.Add(time.Second)
			checkin := healthyCheckin(sentAt)
			checkin.AgentSeq, checkin.AgentVersion, checkin.DeviceSyncV1 = 1, "original", true
			if err := s.RecordCheckin(machineID, checkin, firstReceivedAt); err != nil {
				t.Fatal(err)
			}
			if _, err := s.DB().Exec(test.trigger); err != nil {
				t.Fatal(err)
			}
			disabled := false
			checkin.AgentSeq, checkin.AgentVersion = 2, "replacement"
			checkin.JobsEnabled, checkin.DeviceSyncV1 = &disabled, test.deviceSyncV1
			if err := s.RecordCheckin(machineID, checkin, firstReceivedAt.Add(time.Minute)); err == nil {
				t.Fatal("capability replacement failure accepted a partial checkin")
			}
			readiness, err := s.AgentReadiness(machineID)
			if err != nil || readiness.AgentVersion != "original" || readiness.JobsEnabled == nil ||
				!*readiness.JobsEnabled || readiness.DeviceSyncV1 == nil || !*readiness.DeviceSyncV1 ||
				readiness.LastCheckinReceivedAt == nil || !readiness.LastCheckinReceivedAt.Equal(firstReceivedAt) {
				t.Fatalf("preserved readiness=%+v err=%v", readiness, err)
			}
			var seq int64
			if err := s.DB().QueryRow(`SELECT agent_seq FROM machine_checkins
				WHERE machine_id=? AND sent_at=?`, machineID, fmtTime(sentAt)).Scan(&seq); err != nil || seq != 1 {
				t.Fatalf("preserved agent_seq=%d err=%v", seq, err)
			}
			if countRows(t, s, `SELECT COUNT(*) FROM machine_job_capabilities
				WHERE machine_id=? AND sent_at=? AND capability=? AND supported=1`,
				machineID, fmtTime(sentAt), model.DeviceSyncJobKind) != 1 {
				t.Fatal("failed replacement changed prior capability receipt")
			}
			preview, err := s.PreviewOperatorDeviceSync(machineID, model.DeviceSyncDefaultTimeoutSeconds)
			if err != nil || preview.DeviceSyncV1 == nil || !*preview.DeviceSyncV1 ||
				preview.JobsEnabled == nil || !*preview.JobsEnabled || len(preview.Blockers) != 0 {
				t.Fatalf("preserved capability preview=%+v err=%v", preview, err)
			}
		})
	}
}
