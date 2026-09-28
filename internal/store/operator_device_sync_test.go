package store

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/deploy"
	"github.com/teddashh/AI-Intune/internal/model"
)

func deviceSyncFixture(t *testing.T) (*Store, string, time.Time) {
	t.Helper()
	st := newTestStore(t)
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	st.nowFn = func() time.Time { return now }
	machineID := mustEnroll(t, st, "sync-cnode", now.Add(-time.Hour))
	checkin := healthyCheckin(now.Add(-time.Minute))
	checkin.DeviceSyncV1 = true
	if err := st.RecordCheckin(machineID, checkin, now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	return st, machineID, now
}

func deviceSyncRequest(machineID string, preview OperatorDeviceSyncPreviewResult, key string) OperatorDeviceSyncRequest {
	body := fmt.Sprintf("%s|%d|%s|%s", machineID, preview.ExecutionTimeoutSeconds, preview.PreviewDigest, key)
	digest := sha256.Sum256([]byte(body))
	return OperatorDeviceSyncRequest{
		MachineID: machineID, ExecutionTimeout: preview.ExecutionTimeoutSeconds,
		ConfirmDisplayName: preview.DisplayName, PreviewDigest: preview.PreviewDigest,
		Reason: "refresh device inventory", IdempotencyKey: key,
		RequestDigest: fmt.Sprintf("sha256:%x", digest), CreatedBy: "operator:tailscale-user:7",
		Audit: AuditEntry{SourceAddr: "100.64.0.7", AuthSubject: "tailscale-user:7",
			AuthCapability: "example.com/cap/clawctl-operate", SourceKind: "operator-api"},
	}
}

func appendDeviceSyncPhaseEvent(t *testing.T, st *Store, jobID, machineID, leaseToken string,
	seq int, phase string, at time.Time,
) {
	t.Helper()
	if err := st.AppendJobEvent(jobID, machineID, leaseToken, seq, phase, `{}`, at, at); err != nil {
		t.Fatal(err)
	}
}

func TestOperatorDeviceSyncPreviewApplyAndReplayAreAtomic(t *testing.T) {
	st, machineID, now := deviceSyncFixture(t)
	preview, err := st.PreviewOperatorDeviceSync(machineID, model.DeviceSyncDefaultTimeoutSeconds)
	if err != nil {
		t.Fatal(err)
	}
	if preview.MachineID != machineID || preview.DisplayName != "sync-cnode" ||
		preview.Kind != model.DeviceSyncJobKind || preview.ResourceKind != model.DeviceSyncResourceKind ||
		preview.ResourceID != model.DeviceSyncResourceID || preview.SpecDigest != operatorDeviceSyncSpecDigest() ||
		preview.ChangesMachineConfiguration || !preview.CreatesDesiredState || !preview.CreatesJob ||
		!preview.DeliveryRequiresJobsEnabled || preview.JobsEnabled == nil || !*preview.JobsEnabled ||
		preview.DeviceSyncV1 == nil || !*preview.DeviceSyncV1 || preview.LatestCheckinSentAt == nil ||
		preview.LatestCheckinReceivedAt == nil || preview.ActiveJobCount != 0 ||
		preview.CurrentResourceRevision != 0 || preview.PlannedRevision != 1 || len(preview.Blockers) != 0 ||
		preview.PreviewDigest == "" || !preview.PreviewedAt.Equal(now) {
		t.Fatalf("preview=%+v", preview)
	}
	req := deviceSyncRequest(machineID, preview, "device-sync-success")
	result, err := st.ApplyOperatorDeviceSync(req)
	if err != nil {
		t.Fatal(err)
	}
	if result.Replayed || !result.Audited || result.CreatedBy != req.CreatedBy || result.DesiredID == "" || result.JobID == "" ||
		result.Revision != 1 || result.PreviewDigest != preview.PreviewDigest || !result.CreatedAt.Equal(now) {
		t.Fatalf("result=%+v", result)
	}
	desired, err := st.DesiredState(result.DesiredID)
	if err != nil {
		t.Fatal(err)
	}
	job, err := st.Job(result.JobID)
	if err != nil {
		t.Fatal(err)
	}
	if desired.ScopeType != "machine" || desired.ScopeID != machineID ||
		desired.ResourceKind != model.DeviceSyncResourceKind || desired.ResourceID != model.DeviceSyncResourceID ||
		desired.Spec != model.DeviceSyncSpecJSON || desired.CreatedBy != req.CreatedBy || desired.Revision != result.Revision ||
		job.MachineID != machineID || job.DesiredID != result.DesiredID || job.State != deploy.NotStarted ||
		job.ArtifactDigest != operatorDeviceSyncSpecDigest() || job.Irreversible ||
		job.ExecutionTimeout != model.DeviceSyncDefaultTimeoutSeconds {
		t.Fatalf("desired=%+v job=%+v", desired, job)
	}
	replay, err := st.ApplyOperatorDeviceSync(req)
	if err != nil || !replay.Replayed || !replay.Audited || replay.JobID != result.JobID ||
		replay.DesiredID != result.DesiredID || replay.CreatedBy != req.CreatedBy {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}
	if countRows(t, st, `SELECT COUNT(*) FROM desired_state`) != 1 ||
		countRows(t, st, `SELECT COUNT(*) FROM jobs`) != 1 ||
		countRows(t, st, `SELECT COUNT(*) FROM operator_idempotency`) != 1 {
		t.Fatal("replay duplicated device-sync authority rows")
	}
	entries, err := st.Audit(machineID, 10)
	if err != nil || len(entries) != 2 || entries[0].Action != AuditDeviceSync ||
		!entries[0].IsOperatorReplay() || !entries[1].OK || entries[1].AuthSubject != "tailscale-user:7" {
		t.Fatalf("audit=%+v err=%v", entries, err)
	}
}

func TestOperatorDeviceSyncRejectsNoncanonicalCheckinProjection(t *testing.T) {
	tests := []struct {
		name   string
		insert func(*testing.T, *Store, string)
	}{
		{
			name: "offset received_at sorts before later UTC instant",
			insert: func(t *testing.T, st *Store, machineID string) {
				if _, err := st.DB().Exec(`INSERT INTO machine_checkins
					(machine_id,sent_at,received_at,jobs_enabled) VALUES (?,?,?,1)`,
					machineID, "2026-09-14T11:59:30Z", "2026-09-14T10:59:30-01:00"); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "space separated received_at sorts before later UTC instant",
			insert: func(t *testing.T, st *Store, machineID string) {
				if _, err := st.DB().Exec(`INSERT INTO machine_checkins
					(machine_id,sent_at,received_at,jobs_enabled) VALUES (?,?,?,1)`,
					machineID, "2026-09-14T11:59:31Z", "2026-09-14 11:59:30"); err != nil {
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
			st, machineID, _ := deviceSyncFixture(t)
			validPreview, err := st.PreviewOperatorDeviceSync(machineID, model.DeviceSyncDefaultTimeoutSeconds)
			if err != nil {
				t.Fatal(err)
			}
			test.insert(t, st, machineID)

			preview, err := st.PreviewOperatorDeviceSync(machineID, model.DeviceSyncDefaultTimeoutSeconds)
			if err == nil || err.Error() != "store: device sync target projection is invalid" {
				t.Errorf("preview=%+v err=%v, want invalid projection", preview, err)
			}
			result, err := st.ApplyOperatorDeviceSync(deviceSyncRequest(machineID, validPreview,
				"device-sync-noncanonical-"+test.name))
			if err == nil || err.Error() != "store: device sync target projection is invalid" {
				t.Errorf("apply=%+v err=%v, want invalid projection", result, err)
			}
			if countRows(t, st, `SELECT COUNT(*) FROM desired_state`) != 0 ||
				countRows(t, st, `SELECT COUNT(*) FROM jobs`) != 0 ||
				countRows(t, st, `SELECT COUNT(*) FROM operator_idempotency`) != 0 ||
				countRows(t, st, `SELECT COUNT(*) FROM audit_log WHERE action='device-sync'`) != 0 {
				t.Fatal("invalid projection wrote device-sync authority")
			}
		})
	}
}

func TestOperatorDeviceSyncCanonicalCheckinOrderingRemainsStable(t *testing.T) {
	t.Run("unselected history preserves preview digest", func(t *testing.T) {
		st, machineID, now := deviceSyncFixture(t)
		before, err := st.PreviewOperatorDeviceSync(machineID, model.DeviceSyncDefaultTimeoutSeconds)
		if err != nil {
			t.Fatal(err)
		}
		older := now.Add(-2 * time.Minute)
		if _, err := st.DB().Exec(`INSERT INTO machine_checkins
			(machine_id,sent_at,received_at,jobs_enabled) VALUES (?,?,?,1)`,
			machineID, fmtTime(older), fmtTime(older)); err != nil {
			t.Fatal(err)
		}
		after, err := st.PreviewOperatorDeviceSync(machineID, model.DeviceSyncDefaultTimeoutSeconds)
		if err != nil || after.PreviewDigest != before.PreviewDigest || after.DeviceSyncV1 == nil ||
			!*after.DeviceSyncV1 || len(after.Blockers) != 0 {
			t.Fatalf("before=%+v after=%+v err=%v", before, after, err)
		}
		if _, err := st.ApplyOperatorDeviceSync(deviceSyncRequest(machineID, after,
			"device-sync-canonical-history")); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("same second tie uses later rowid", func(t *testing.T) {
		st, machineID, now := deviceSyncFixture(t)
		receivedAt := now.Add(-time.Minute)
		sentAt := receivedAt.Add(time.Second)
		if _, err := st.DB().Exec(`INSERT INTO machine_checkins
			(machine_id,sent_at,received_at,jobs_enabled) VALUES (?,?,?,1)`,
			machineID, fmtTime(sentAt), fmtTime(receivedAt)); err != nil {
			t.Fatal(err)
		}
		preview, err := st.PreviewOperatorDeviceSync(machineID, model.DeviceSyncDefaultTimeoutSeconds)
		if err != nil || preview.LatestCheckinSentAt == nil || !preview.LatestCheckinSentAt.Equal(sentAt) ||
			preview.DeviceSyncV1 != nil || len(preview.Blockers) != 1 ||
			preview.Blockers[0] != OperatorDeviceSyncBlockerCapabilityUnconfirmed {
			t.Fatalf("preview=%+v err=%v", preview, err)
		}
		_, err = st.ApplyOperatorDeviceSync(deviceSyncRequest(machineID, preview,
			"device-sync-canonical-rowid-tie"))
		var rejection *OperatorRequestError
		if !errors.Is(err, ErrDeviceSyncCapabilityUnconfirmed) || !errors.As(err, &rejection) ||
			rejection.Code != OperatorCodeDeviceSyncCapabilityUnconfirmed || !rejection.Audited {
			t.Fatalf("rejection=%+v err=%v", rejection, err)
		}
	})
}

func TestOperatorDeviceSyncTypedGateRejectionsCreateNoIntent(t *testing.T) {
	tests := []struct {
		name        string
		prepare     func(*testing.T, *Store, string, time.Time)
		wantBlocker OperatorDeviceSyncBlocker
		wantCode    string
		wantError   error
	}{
		{
			name: "capability unconfirmed",
			prepare: func(t *testing.T, st *Store, machineID string, now time.Time) {
				checkin := healthyCheckin(now.Add(-30 * time.Second))
				if err := st.RecordCheckin(machineID, checkin, now.Add(-30*time.Second)); err != nil {
					t.Fatal(err)
				}
			},
			wantBlocker: OperatorDeviceSyncBlockerCapabilityUnconfirmed,
			wantCode:    OperatorCodeDeviceSyncCapabilityUnconfirmed, wantError: ErrDeviceSyncCapabilityUnconfirmed,
		},
		{
			name: "retired",
			prepare: func(t *testing.T, st *Store, machineID string, now time.Time) {
				if err := st.RetireMachine(machineID, now); err != nil {
					t.Fatal(err)
				}
			},
			wantBlocker: OperatorDeviceSyncBlockerRetired,
			wantCode:    OperatorCodeMachineRetired, wantError: ErrMachineRetired,
		},
		{
			name: "jobs disabled",
			prepare: func(t *testing.T, st *Store, machineID string, now time.Time) {
				disabled := false
				checkin := healthyCheckin(now.Add(-30 * time.Second))
				checkin.JobsEnabled, checkin.DeviceSyncV1 = &disabled, true
				if err := st.RecordCheckin(machineID, checkin, now.Add(-30*time.Second)); err != nil {
					t.Fatal(err)
				}
			},
			wantBlocker: OperatorDeviceSyncBlockerExecutionDisabled,
			wantCode:    OperatorCodeAgentExecutionDisabled, wantError: ErrAgentExecutionDisabled,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			st, machineID, now := deviceSyncFixture(t)
			test.prepare(t, st, machineID, now)
			preview, err := st.PreviewOperatorDeviceSync(machineID, model.DeviceSyncDefaultTimeoutSeconds)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, blocker := range preview.Blockers {
				found = found || blocker == test.wantBlocker
			}
			if !found {
				t.Fatalf("blockers=%v want %s", preview.Blockers, test.wantBlocker)
			}
			req := deviceSyncRequest(machineID, preview, "device-sync-reject-"+test.name)
			_, err = st.ApplyOperatorDeviceSync(req)
			var rejection *OperatorRequestError
			if !errors.Is(err, test.wantError) || !errors.As(err, &rejection) ||
				rejection.Code != test.wantCode || !rejection.Audited {
				t.Fatalf("rejection=%+v err=%v", rejection, err)
			}
			if countRows(t, st, `SELECT COUNT(*) FROM desired_state`) != 0 ||
				countRows(t, st, `SELECT COUNT(*) FROM jobs`) != 0 ||
				countRows(t, st, `SELECT COUNT(*) FROM operator_idempotency`) != 1 {
				t.Fatal("typed rejection wrote device-sync intent")
			}
		})
	}
}

func TestOperatorDeviceSyncRejectsPreviewAfterLatestCapabilityReceiptChanges(t *testing.T) {
	st, machineID, now := deviceSyncFixture(t)
	preview, err := st.PreviewOperatorDeviceSync(machineID, model.DeviceSyncDefaultTimeoutSeconds)
	if err != nil {
		t.Fatal(err)
	}
	checkin := healthyCheckin(now.Add(-10 * time.Second))
	checkin.DeviceSyncV1 = true
	if err := st.RecordCheckin(machineID, checkin, now.Add(-10*time.Second)); err != nil {
		t.Fatal(err)
	}
	_, err = st.ApplyOperatorDeviceSync(deviceSyncRequest(machineID, preview, "device-sync-stale"))
	var rejection *OperatorRequestError
	if !errors.Is(err, ErrDeviceSyncPreviewStale) || !errors.As(err, &rejection) ||
		rejection.Code != OperatorCodeDeviceSyncPreviewStale || !rejection.Audited {
		t.Fatalf("rejection=%+v err=%v", rejection, err)
	}
	if countRows(t, st, `SELECT COUNT(*) FROM desired_state`) != 0 ||
		countRows(t, st, `SELECT COUNT(*) FROM jobs`) != 0 {
		t.Fatal("stale preview left device-sync intent")
	}
}

func TestOperatorDeviceSyncWriterRollsBackEveryAuthorityRow(t *testing.T) {
	for _, table := range []string{"revision_counters", "desired_state", "jobs", "operator_idempotency", "audit_log"} {
		t.Run(table, func(t *testing.T) {
			st, machineID, _ := deviceSyncFixture(t)
			preview, err := st.PreviewOperatorDeviceSync(machineID, model.DeviceSyncDefaultTimeoutSeconds)
			if err != nil {
				t.Fatal(err)
			}
			trigger := fmt.Sprintf(`CREATE TRIGGER fail_device_sync_%s BEFORE INSERT ON %s
				BEGIN SELECT RAISE(ABORT, 'forced device-sync failure'); END`, table, table)
			if _, err := st.DB().Exec(trigger); err != nil {
				t.Fatal(err)
			}
			if _, err := st.ApplyOperatorDeviceSync(deviceSyncRequest(machineID, preview, "device-sync-atomic-"+table)); err == nil {
				t.Fatal("forced writer failure unexpectedly succeeded")
			}
			for _, authority := range []string{"revision_counters", "desired_state", "jobs", "operator_idempotency", "audit_log"} {
				if got := countRows(t, st, `SELECT COUNT(*) FROM `+authority); got != 0 {
					t.Fatalf("%s rows=%d after %s failure", authority, got, table)
				}
			}
		})
	}
}

func TestOperatorDeviceSyncIdempotencyConflictDoesNotCreateSecondJob(t *testing.T) {
	st, machineID, _ := deviceSyncFixture(t)
	preview, err := st.PreviewOperatorDeviceSync(machineID, model.DeviceSyncDefaultTimeoutSeconds)
	if err != nil {
		t.Fatal(err)
	}
	req := deviceSyncRequest(machineID, preview, "device-sync-conflict")
	if _, err := st.ApplyOperatorDeviceSync(req); err != nil {
		t.Fatal(err)
	}
	req.RequestDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if _, err := st.ApplyOperatorDeviceSync(req); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("conflict err=%v", err)
	}
	if countRows(t, st, `SELECT COUNT(*) FROM desired_state`) != 1 || countRows(t, st, `SELECT COUNT(*) FROM jobs`) != 1 {
		t.Fatal("idempotency conflict created a second device-sync intent")
	}
}

func TestOperatorDeviceSyncSecondWriterCannotReuseConsumedPreview(t *testing.T) {
	st, machineID, _ := deviceSyncFixture(t)
	preview, err := st.PreviewOperatorDeviceSync(machineID, model.DeviceSyncDefaultTimeoutSeconds)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ApplyOperatorDeviceSync(deviceSyncRequest(machineID, preview, "device-sync-writer-one")); err != nil {
		t.Fatal(err)
	}
	_, err = st.ApplyOperatorDeviceSync(deviceSyncRequest(machineID, preview, "device-sync-writer-two"))
	var rejection *OperatorRequestError
	if !errors.Is(err, ErrDeviceSyncPreviewStale) || !errors.As(err, &rejection) ||
		rejection.Code != OperatorCodeDeviceSyncPreviewStale || !rejection.Audited {
		t.Fatalf("second writer rejection=%+v err=%v", rejection, err)
	}
	if countRows(t, st, `SELECT COUNT(*) FROM revision_counters
		WHERE resource_scope='device:sync' AND current_revision=1`) != 1 ||
		countRows(t, st, `SELECT COUNT(*) FROM desired_state`) != 1 ||
		countRows(t, st, `SELECT COUNT(*) FROM jobs`) != 1 ||
		countRows(t, st, `SELECT COUNT(*) FROM operator_idempotency`) != 2 {
		t.Fatal("stale second writer changed device-sync authority")
	}
}

func TestOperatorDeviceSyncMissingRevisionCounterNeverReusesExistingRevision(t *testing.T) {
	st, machineID, now := deviceSyncFixture(t)
	firstPreview, err := st.PreviewOperatorDeviceSync(machineID, model.DeviceSyncDefaultTimeoutSeconds)
	if err != nil {
		t.Fatal(err)
	}
	firstRequest := deviceSyncRequest(machineID, firstPreview, "device-sync-before-missing-revision-counter")
	first, err := st.ApplyOperatorDeviceSync(firstRequest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ClaimJob(first.JobID, machineID, now.Add(time.Second), time.Minute); err != nil {
		t.Fatal(err)
	}
	terminalState, err := st.AdvanceJobByHub(first.JobID, deploy.LeaseLost, now.Add(2*time.Second))
	if err != nil || terminalState != deploy.LeaseExpired {
		t.Fatalf("first job terminal state=%q err=%v", terminalState, err)
	}

	resourceScope := model.DeviceSyncResourceKind + ":" + model.DeviceSyncResourceID
	deleted, err := st.DB().Exec(`DELETE FROM revision_counters WHERE resource_scope=?`, resourceScope)
	if err != nil {
		t.Fatal(err)
	}
	if affected, err := deleted.RowsAffected(); err != nil || affected != 1 {
		t.Fatalf("deleted revision counters=%d err=%v", affected, err)
	}

	st.nowFn = func() time.Time { return now.Add(3 * time.Second) }
	secondPreview, err := st.PreviewOperatorDeviceSync(machineID, model.DeviceSyncDefaultTimeoutSeconds)
	if err != nil {
		t.Fatal(err)
	}
	secondRequest := deviceSyncRequest(machineID, secondPreview, "device-sync-after-missing-revision-counter")
	second, secondErr := st.ApplyOperatorDeviceSync(secondRequest)

	rows, err := st.DB().Query(`SELECT desired_id FROM desired_state
		WHERE resource_kind=? AND resource_id=? AND revision=1 ORDER BY desired_id`,
		model.DeviceSyncResourceKind, model.DeviceSyncResourceID)
	if err != nil {
		t.Fatal(err)
	}
	var revisionOneDesiredIDs []string
	for rows.Next() {
		var desiredID string
		if err := rows.Scan(&desiredID); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		revisionOneDesiredIDs = append(revisionOneDesiredIDs, desiredID)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		t.Fatal(err)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	revisionOneJobs := countRows(t, st, `SELECT COUNT(*) FROM jobs j
		JOIN desired_state d ON d.desired_id=j.desired_id
		WHERE d.resource_kind=? AND d.resource_id=? AND d.revision=1`,
		model.DeviceSyncResourceKind, model.DeviceSyncResourceID)
	counterRevision := countRows(t, st, `SELECT COALESCE(MAX(current_revision),-1)
		FROM revision_counters WHERE resource_scope=?`, resourceScope)
	otherDesiredAtFirstRevision := countRows(t, st, `SELECT COUNT(*) FROM desired_state
		WHERE resource_kind=? AND resource_id=? AND revision=? AND desired_id<>?`,
		model.DeviceSyncResourceKind, model.DeviceSyncResourceID, first.Revision, first.DesiredID)
	revisionOneDesiredIDsSame := len(revisionOneDesiredIDs) > 1
	for _, desiredID := range revisionOneDesiredIDs[1:] {
		revisionOneDesiredIDsSame = revisionOneDesiredIDsSame && desiredID == revisionOneDesiredIDs[0]
	}

	replay, replayErr := st.ApplyOperatorDeviceSync(firstRequest)
	replayMatchesOriginal := replayErr == nil && replay.Replayed && replay.DesiredID == first.DesiredID &&
		replay.JobID == first.JobID && replay.Revision == first.Revision
	t.Logf("second preview: CurrentResourceRevision=%d PlannedRevision=%d Blockers=%v",
		secondPreview.CurrentResourceRevision, secondPreview.PlannedRevision, secondPreview.Blockers)
	t.Logf("second apply: result_revision=%d desired_id_set=%t job_id_set=%t err=%v",
		second.Revision, second.DesiredID != "", second.JobID != "", secondErr)
	t.Logf("desired_state revision=1: rows=%d two_rows_have_same_desired_id=%t",
		len(revisionOneDesiredIDs), revisionOneDesiredIDsSame)
	t.Logf("jobs pointing to device/sync revision=1 desired states: rows=%d", revisionOneJobs)
	t.Logf("first replay revision evidence: counter_revision=%d other_desired_same_revision=%d",
		counterRevision, otherDesiredAtFirstRevision)
	t.Logf("first idempotency key replay: matches_original_receipt=%t replayed=%t err=%v",
		replayMatchesOriginal, replay.Replayed, replayErr)

	if secondErr == nil && second.Revision <= first.Revision {
		t.Errorf("second creation reused revision: first=%d second=%d", first.Revision, second.Revision)
	}
	if len(revisionOneDesiredIDs) != 1 {
		t.Errorf("device/sync revision 1 desired states=%d, want 1", len(revisionOneDesiredIDs))
	}
	if revisionOneJobs != 1 {
		t.Errorf("jobs pointing to device/sync revision 1 desired states=%d, want 1", revisionOneJobs)
	}
	if secondErr == nil && !replayMatchesOriginal {
		t.Errorf("successful second creation changed original receipt replay: replay=%+v err=%v", replay, replayErr)
	}
}

func TestOperatorDeviceSyncCoversRemainingExecutionAndOccupancyGates(t *testing.T) {
	t.Run("execution unknown", func(t *testing.T) {
		st, machineID, now := deviceSyncFixture(t)
		checkin := healthyCheckin(now.Add(-30 * time.Second))
		checkin.JobsEnabled, checkin.DeviceSyncV1 = nil, true
		if err := st.RecordCheckin(machineID, checkin, now.Add(-30*time.Second)); err != nil {
			t.Fatal(err)
		}
		preview, err := st.PreviewOperatorDeviceSync(machineID, model.DeviceSyncDefaultTimeoutSeconds)
		if err != nil {
			t.Fatal(err)
		}
		if len(preview.Blockers) != 1 || preview.Blockers[0] != OperatorDeviceSyncBlockerExecutionUnknown {
			t.Fatalf("blockers=%v", preview.Blockers)
		}
		_, err = st.ApplyOperatorDeviceSync(deviceSyncRequest(machineID, preview, "device-sync-execution-unknown"))
		var rejection *OperatorRequestError
		if !errors.Is(err, ErrAgentExecutionUnknown) || !errors.As(err, &rejection) ||
			rejection.Code != OperatorCodeAgentExecutionUnknown || !rejection.Audited {
			t.Fatalf("rejection=%+v err=%v", rejection, err)
		}
	})

	t.Run("nonterminal job", func(t *testing.T) {
		st, machineID, _ := deviceSyncFixture(t)
		desiredID, revision, err := st.CreateDesiredState("machine", machineID, "test", "active", `{}`, "test")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.CreateJob(machineID, desiredID, revision, NewJob{}); err != nil {
			t.Fatal(err)
		}
		preview, err := st.PreviewOperatorDeviceSync(machineID, model.DeviceSyncDefaultTimeoutSeconds)
		if err != nil {
			t.Fatal(err)
		}
		if preview.ActiveJobCount != 1 || len(preview.Blockers) != 1 ||
			preview.Blockers[0] != OperatorDeviceSyncBlockerNonterminalJob {
			t.Fatalf("preview=%+v", preview)
		}
		_, err = st.ApplyOperatorDeviceSync(deviceSyncRequest(machineID, preview, "device-sync-active-job"))
		var rejection *OperatorRequestError
		if !errors.Is(err, ErrMachineActiveJob) || !errors.As(err, &rejection) ||
			rejection.Code != OperatorCodeMachineActiveJob || !rejection.Audited {
			t.Fatalf("rejection=%+v err=%v", rejection, err)
		}
		if countRows(t, st, `SELECT COUNT(*) FROM desired_state`) != 1 || countRows(t, st, `SELECT COUNT(*) FROM jobs`) != 1 {
			t.Fatal("occupancy rejection created device-sync intent")
		}
	})
}

func TestOperatorDeviceSyncRejectedDecisionIsAtomicAndImmutable(t *testing.T) {
	for _, table := range []string{"audit_log", "operator_idempotency"} {
		t.Run("rollback "+table, func(t *testing.T) {
			st, machineID, now := deviceSyncFixture(t)
			disabled := false
			checkin := healthyCheckin(now.Add(-30 * time.Second))
			checkin.JobsEnabled, checkin.DeviceSyncV1 = &disabled, true
			if err := st.RecordCheckin(machineID, checkin, now.Add(-30*time.Second)); err != nil {
				t.Fatal(err)
			}
			preview, err := st.PreviewOperatorDeviceSync(machineID, model.DeviceSyncDefaultTimeoutSeconds)
			if err != nil {
				t.Fatal(err)
			}
			trigger := fmt.Sprintf(`CREATE TRIGGER fail_device_sync_rejection_%s BEFORE INSERT ON %s
				BEGIN SELECT RAISE(ABORT, 'forced device-sync rejection failure'); END`, table, table)
			if _, err := st.DB().Exec(trigger); err != nil {
				t.Fatal(err)
			}
			_, err = st.ApplyOperatorDeviceSync(deviceSyncRequest(machineID, preview, "device-sync-reject-atomic-"+table))
			if err == nil || errors.Is(err, ErrAgentExecutionDisabled) {
				t.Fatalf("rejection authority failure err=%v", err)
			}
			if countRows(t, st, `SELECT COUNT(*) FROM audit_log`) != 0 ||
				countRows(t, st, `SELECT COUNT(*) FROM operator_idempotency`) != 0 ||
				countRows(t, st, `SELECT COUNT(*) FROM desired_state`) != 0 ||
				countRows(t, st, `SELECT COUNT(*) FROM jobs`) != 0 {
				t.Fatal("failed rejection left partial authority rows")
			}
		})
	}

	t.Run("replay keeps original rejection", func(t *testing.T) {
		st, machineID, now := deviceSyncFixture(t)
		disabled := false
		checkin := healthyCheckin(now.Add(-30 * time.Second))
		checkin.JobsEnabled, checkin.DeviceSyncV1 = &disabled, true
		if err := st.RecordCheckin(machineID, checkin, now.Add(-30*time.Second)); err != nil {
			t.Fatal(err)
		}
		preview, err := st.PreviewOperatorDeviceSync(machineID, model.DeviceSyncDefaultTimeoutSeconds)
		if err != nil {
			t.Fatal(err)
		}
		req := deviceSyncRequest(machineID, preview, "device-sync-rejected-replay")
		if _, err := st.ApplyOperatorDeviceSync(req); !errors.Is(err, ErrAgentExecutionDisabled) {
			t.Fatalf("first rejection err=%v", err)
		}
		enabled := true
		fixed := healthyCheckin(now.Add(-10 * time.Second))
		fixed.JobsEnabled, fixed.DeviceSyncV1 = &enabled, true
		if err := st.RecordCheckin(machineID, fixed, now.Add(-10*time.Second)); err != nil {
			t.Fatal(err)
		}
		_, err = st.ApplyOperatorDeviceSync(req)
		var replay *OperatorRequestError
		if !errors.Is(err, ErrAgentExecutionDisabled) || !errors.As(err, &replay) || !replay.Replayed || !replay.Audited {
			t.Fatalf("replay=%+v err=%v", replay, err)
		}
		if countRows(t, st, `SELECT COUNT(*) FROM operator_idempotency`) != 1 ||
			countRows(t, st, `SELECT COUNT(*) FROM audit_log`) != 2 ||
			countRows(t, st, `SELECT COUNT(*) FROM desired_state`) != 0 ||
			countRows(t, st, `SELECT COUNT(*) FROM jobs`) != 0 {
			t.Fatal("rejected replay acquired new device-sync meaning")
		}
	})
}

func TestOperatorDeviceSyncReplayFailsClosedOnTamperedEvidence(t *testing.T) {
	tests := []struct {
		name   string
		tamper func(*testing.T, *Store, OperatorDeviceSyncResult)
	}{
		{name: "receipt", tamper: func(t *testing.T, st *Store, _ OperatorDeviceSyncResult) {
			if _, err := st.DB().Exec(`UPDATE operator_idempotency SET response_json='{}'`); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "desired state", tamper: func(t *testing.T, st *Store, _ OperatorDeviceSyncResult) {
			if _, err := st.DB().Exec(`UPDATE desired_state SET spec='{}'`); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "creator provenance", tamper: func(t *testing.T, st *Store, _ OperatorDeviceSyncResult) {
			if _, err := st.DB().Exec(`UPDATE desired_state SET created_by='operator:tampered'`); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "original audit", tamper: func(t *testing.T, st *Store, _ OperatorDeviceSyncResult) {
			if _, err := st.DB().Exec(`DELETE FROM audit_log WHERE action='device-sync'`); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "audit subject", tamper: func(t *testing.T, st *Store, _ OperatorDeviceSyncResult) {
			if _, err := st.DB().Exec(`UPDATE audit_log SET subject='tampered' WHERE action='device-sync'`); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "job dependency", tamper: func(t *testing.T, st *Store, result OperatorDeviceSyncResult) {
			desiredID, revision, err := st.CreateDesiredState("machine", result.MachineID, "test", "prerequisite", `{}`, "test")
			if err != nil {
				t.Fatal(err)
			}
			prerequisiteID, err := st.CreateJob(result.MachineID, desiredID, revision, NewJob{})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := st.DB().Exec(`INSERT INTO job_dependencies
				(job_id,prerequisite_job_id,position) VALUES (?,?,0)`, result.JobID, prerequisiteID); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			st, machineID, _ := deviceSyncFixture(t)
			preview, err := st.PreviewOperatorDeviceSync(machineID, model.DeviceSyncDefaultTimeoutSeconds)
			if err != nil {
				t.Fatal(err)
			}
			req := deviceSyncRequest(machineID, preview, "device-sync-tamper-"+test.name)
			result, err := st.ApplyOperatorDeviceSync(req)
			if err != nil {
				t.Fatal(err)
			}
			test.tamper(t, st, result)
			beforeAudit := countRows(t, st, `SELECT COUNT(*) FROM audit_log`)
			beforeDesired := countRows(t, st, `SELECT COUNT(*) FROM desired_state`)
			beforeJobs := countRows(t, st, `SELECT COUNT(*) FROM jobs`)
			if _, err := st.ApplyOperatorDeviceSync(req); err == nil ||
				!strings.Contains(err.Error(), "idempotency cache invalid") {
				t.Fatalf("tampered replay err=%v", err)
			}
			if countRows(t, st, `SELECT COUNT(*) FROM desired_state`) != beforeDesired ||
				countRows(t, st, `SELECT COUNT(*) FROM jobs`) != beforeJobs ||
				countRows(t, st, `SELECT COUNT(*) FROM audit_log`) != beforeAudit {
				t.Fatal("tampered replay changed authority rows")
			}
		})
	}
}

func TestOperatorDeviceSyncReceiptRequiresExplicitCanonicalFields(t *testing.T) {
	offset := time.FixedZone("receipt-offset", 8*60*60)
	tests := []struct {
		name   string
		mutate func(*operatorDeviceSyncReceipt)
	}{
		{name: "blockers omitted", mutate: func(receipt *operatorDeviceSyncReceipt) {
			receipt.Blockers = nil
		}},
		{name: "created_at offset", mutate: func(receipt *operatorDeviceSyncReceipt) {
			receipt.CreatedAt = receipt.CreatedAt.In(offset)
		}},
		{name: "checkin sent_at offset", mutate: func(receipt *operatorDeviceSyncReceipt) {
			value := receipt.LatestCheckinSentAt.In(offset)
			receipt.LatestCheckinSentAt = &value
		}},
		{name: "checkin received_at offset", mutate: func(receipt *operatorDeviceSyncReceipt) {
			value := receipt.LatestCheckinReceivedAt.In(offset)
			receipt.LatestCheckinReceivedAt = &value
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			st, machineID, _ := deviceSyncFixture(t)
			preview, err := st.PreviewOperatorDeviceSync(machineID, model.DeviceSyncDefaultTimeoutSeconds)
			if err != nil {
				t.Fatal(err)
			}
			req := deviceSyncRequest(machineID, preview, "device-sync-canonical-"+test.name)
			if _, err := st.ApplyOperatorDeviceSync(req); err != nil {
				t.Fatal(err)
			}
			var raw string
			if err := st.DB().QueryRow(`SELECT response_json FROM operator_idempotency
				WHERE idempotency_key=?`, req.IdempotencyKey).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			receipt, err := decodeOperatorDeviceSyncReceipt(raw)
			if err != nil {
				t.Fatal(err)
			}
			test.mutate(&receipt)
			changed, err := json.Marshal(receipt)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := st.DB().Exec(`UPDATE operator_idempotency SET response_json=?
				WHERE idempotency_key=?`, string(changed), req.IdempotencyKey); err != nil {
				t.Fatal(err)
			}
			if _, err := st.DB().Exec(`UPDATE audit_log SET detail=?
				WHERE action='device-sync' AND idempotency_key=?`,
				operatorDeviceSyncSuccessAuditDetail(receipt), req.IdempotencyKey); err != nil {
				t.Fatal(err)
			}
			beforeAudit := countRows(t, st, `SELECT COUNT(*) FROM audit_log`)
			if _, err := st.ApplyOperatorDeviceSync(req); err == nil ||
				!strings.Contains(err.Error(), "idempotency cache invalid") {
				t.Errorf("noncanonical replay err=%v, expected rejection containing %q; "+
					"a noncanonical cached receipt was returned to the operator as the original decision",
					err, "idempotency cache invalid")
			}
			auditCount := countRows(t, st, `SELECT COUNT(*) FROM audit_log`)
			desiredCount := countRows(t, st, `SELECT COUNT(*) FROM desired_state`)
			jobCount := countRows(t, st, `SELECT COUNT(*) FROM jobs`)
			if auditCount != beforeAudit || desiredCount != 1 || jobCount != 1 {
				t.Errorf("noncanonical replay authority rows: audit=%d desired_state=%d jobs=%d, "+
					"expected audit=%d desired_state=1 jobs=1; the rejected replay still changed authority rows, "+
					"so replay was not read-only", auditCount, desiredCount, jobCount, beforeAudit)
			}
		})
	}
}

func TestOperatorDeviceSyncReceiptRequiresCausalHubCheckinTime(t *testing.T) {
	st, machineID, _ := deviceSyncFixture(t)
	preview, err := st.PreviewOperatorDeviceSync(machineID, model.DeviceSyncDefaultTimeoutSeconds)
	if err != nil {
		t.Fatal(err)
	}
	req := deviceSyncRequest(machineID, preview, "device-sync-future-hub-checkin")
	if _, err := st.ApplyOperatorDeviceSync(req); err != nil {
		t.Fatal(err)
	}
	var raw string
	if err := st.DB().QueryRow(`SELECT response_json FROM operator_idempotency
		WHERE idempotency_key=?`, req.IdempotencyKey).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	receipt, err := decodeOperatorDeviceSyncReceipt(raw)
	if err != nil {
		t.Fatal(err)
	}
	future := receipt.CreatedAt.Add(time.Second)
	receipt.LatestCheckinReceivedAt = &future
	changed, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`UPDATE operator_idempotency SET response_json=?
		WHERE idempotency_key=?`, string(changed), req.IdempotencyKey); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`UPDATE audit_log SET detail=?
		WHERE action='device-sync' AND idempotency_key=?`,
		operatorDeviceSyncSuccessAuditDetail(receipt), req.IdempotencyKey); err != nil {
		t.Fatal(err)
	}
	beforeAudit := countRows(t, st, `SELECT COUNT(*) FROM audit_log WHERE action='device-sync'`)
	if _, err := st.ApplyOperatorDeviceSync(req); err == nil ||
		!strings.Contains(err.Error(), "idempotency cache invalid") {
		t.Fatalf("replay with future Hub check-in err=%v", err)
	}
	if countRows(t, st, `SELECT COUNT(*) FROM audit_log WHERE action='device-sync'`) != beforeAudit ||
		countRows(t, st, `SELECT COUNT(*) FROM operator_idempotency`) != 1 ||
		countRows(t, st, `SELECT COUNT(*) FROM revision_counters WHERE resource_scope='device:sync'`) != 1 ||
		countRows(t, st, `SELECT COUNT(*) FROM desired_state`) != 1 ||
		countRows(t, st, `SELECT COUNT(*) FROM jobs`) != 1 {
		t.Fatal("future Hub check-in replay changed authority")
	}
}

// 能寫 Hub 資料庫的人可竄改收據事實並重算 digest 使其自洽；最後的防線是 request
// 帶入 operator 在 preview 時確認的 digest，而不是快取中的 digest。
func TestOperatorDeviceSyncReplayRejectsRecomputedPreviewDigest(t *testing.T) {
	st, machineID, _ := deviceSyncFixture(t)
	preview, err := st.PreviewOperatorDeviceSync(machineID, model.DeviceSyncDefaultTimeoutSeconds)
	if err != nil {
		t.Fatal(err)
	}
	req := deviceSyncRequest(machineID, preview, "device-sync-recomputed-preview-digest")
	if _, err := st.ApplyOperatorDeviceSync(req); err != nil {
		t.Fatal(err)
	}
	var raw string
	if err := st.DB().QueryRow(`SELECT response_json FROM operator_idempotency
		WHERE idempotency_key=?`, req.IdempotencyKey).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	receipt, err := decodeOperatorDeviceSyncReceipt(raw)
	if err != nil {
		t.Fatal(err)
	}
	changedSentAt := receipt.LatestCheckinSentAt.Add(-time.Second)
	receipt.LatestCheckinSentAt = &changedSentAt
	snapshot := operatorDeviceSyncSnapshot{
		MachineID: receipt.MachineID, DisplayName: receipt.DisplayName,
		LifecycleRevision: receipt.LifecycleRevision, JobsEnabled: receipt.JobsEnabled,
		DeviceSyncV1: receipt.DeviceSyncV1, LatestCheckinSentAt: receipt.LatestCheckinSentAt,
		LatestCheckinReceivedAt: receipt.LatestCheckinReceivedAt, ActiveJobCount: receipt.ActiveJobCount,
		CurrentResourceRevision: receipt.CurrentResourceRevision,
	}
	receipt.PreviewDigest = operatorDeviceSyncPreviewDigest(snapshot, receipt.OperatorDeviceSyncImpact)
	changed, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`UPDATE operator_idempotency SET response_json=?
		WHERE idempotency_key=?`, string(changed), req.IdempotencyKey); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`UPDATE audit_log SET detail=?
		WHERE action='device-sync' AND idempotency_key=?`,
		operatorDeviceSyncSuccessAuditDetail(receipt), req.IdempotencyKey); err != nil {
		t.Fatal(err)
	}
	beforeAudit := countRows(t, st, `SELECT COUNT(*) FROM audit_log`)
	if _, err := st.ApplyOperatorDeviceSync(req); err == nil || !strings.Contains(err.Error(), "idempotency cache invalid") {
		t.Errorf("recomputed-digest replay err=%v, expected rejection containing %q; "+
			"the operator received facts different from the preview they confirmed",
			err, "idempotency cache invalid")
	}
	auditCount := countRows(t, st, `SELECT COUNT(*) FROM audit_log`)
	desiredCount := countRows(t, st, `SELECT COUNT(*) FROM desired_state`)
	jobCount := countRows(t, st, `SELECT COUNT(*) FROM jobs`)
	if auditCount != beforeAudit || desiredCount != 1 || jobCount != 1 {
		t.Errorf("recomputed-digest replay authority rows: audit=%d desired_state=%d jobs=%d, "+
			"expected audit=%d desired_state=1 jobs=1; rejecting the forged receipt still altered "+
			"the operator's recorded sync state", auditCount, desiredCount, jobCount, beforeAudit)
	}
}

// 若快取收據由另一個協議世代寫成，這版 Hub 不保證理解其欄位語意；因此必須拒絕，
// 不能依目前欄位含義把它回給 operator。
func TestOperatorDeviceSyncReplayRejectsReceiptFromAnotherSchemaVersion(t *testing.T) {
	st, machineID, _ := deviceSyncFixture(t)
	preview, err := st.PreviewOperatorDeviceSync(machineID, model.DeviceSyncDefaultTimeoutSeconds)
	if err != nil {
		t.Fatal(err)
	}
	req := deviceSyncRequest(machineID, preview, "device-sync-other-schema-version")
	if _, err := st.ApplyOperatorDeviceSync(req); err != nil {
		t.Fatal(err)
	}
	var raw string
	if err := st.DB().QueryRow(`SELECT response_json FROM operator_idempotency
		WHERE idempotency_key=?`, req.IdempotencyKey).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	receipt, err := decodeOperatorDeviceSyncReceipt(raw)
	if err != nil {
		t.Fatal(err)
	}
	receipt.SchemaVersion = operatorDeviceSyncVersion + "-next"
	changed, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`UPDATE operator_idempotency SET response_json=?
		WHERE idempotency_key=?`, string(changed), req.IdempotencyKey); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`UPDATE audit_log SET detail=?
		WHERE action='device-sync' AND idempotency_key=?`,
		operatorDeviceSyncSuccessAuditDetail(receipt), req.IdempotencyKey); err != nil {
		t.Fatal(err)
	}
	beforeAudit := countRows(t, st, `SELECT COUNT(*) FROM audit_log`)
	if _, err := st.ApplyOperatorDeviceSync(req); err == nil || !strings.Contains(err.Error(), "idempotency cache invalid") {
		t.Errorf("foreign-schema replay err=%v, expected rejection containing %q; "+
			"the operator was shown a receipt whose field meanings this Hub version cannot guarantee",
			err, "idempotency cache invalid")
	}
	auditCount := countRows(t, st, `SELECT COUNT(*) FROM audit_log`)
	desiredCount := countRows(t, st, `SELECT COUNT(*) FROM desired_state`)
	jobCount := countRows(t, st, `SELECT COUNT(*) FROM jobs`)
	if auditCount != beforeAudit || desiredCount != 1 || jobCount != 1 {
		t.Errorf("foreign-schema replay authority rows: audit=%d desired_state=%d jobs=%d, "+
			"expected audit=%d desired_state=1 jobs=1; declining an unreadable receipt still mutated "+
			"the operator's authoritative records", auditCount, desiredCount, jobCount, beforeAudit)
	}
}

// operator_idempotency.created_at 記錄 Hub 何時做成並快取這個決定；它必須和收據同刻，
// 否則該列無法證明自己保存的是那一次決定。
func TestOperatorDeviceSyncReplayRejectsCacheRowTimeThatLeftItsReceipt(t *testing.T) {
	st, machineID, now := deviceSyncFixture(t)
	preview, err := st.PreviewOperatorDeviceSync(machineID, model.DeviceSyncDefaultTimeoutSeconds)
	if err != nil {
		t.Fatal(err)
	}
	req := deviceSyncRequest(machineID, preview, "device-sync-cache-row-time")
	if _, err := st.ApplyOperatorDeviceSync(req); err != nil {
		t.Fatal(err)
	}
	var cachedCreatedAt string
	if err := st.DB().QueryRow(`SELECT created_at FROM operator_idempotency
		WHERE idempotency_key=?`, req.IdempotencyKey).Scan(&cachedCreatedAt); err != nil {
		t.Fatal(err)
	}
	if cachedCreatedAt != fmtTime(now) {
		t.Errorf("cache created_at=%q, expected %q; "+
			"the operator's original decision is already attributed to the wrong instant",
			cachedCreatedAt, fmtTime(now))
	}
	if _, err := st.DB().Exec(`UPDATE operator_idempotency SET created_at=?
		WHERE idempotency_key=?`, fmtTime(now.Add(time.Second)), req.IdempotencyKey); err != nil {
		t.Fatal(err)
	}
	beforeAudit := countRows(t, st, `SELECT COUNT(*) FROM audit_log`)
	if _, err := st.ApplyOperatorDeviceSync(req); err == nil || !strings.Contains(err.Error(), "idempotency cache invalid") {
		t.Errorf("detached-cache-time replay err=%v, expected rejection containing %q; "+
			"the operator received a decision from a row that no longer proves when that decision was made",
			err, "idempotency cache invalid")
	}
	auditCount := countRows(t, st, `SELECT COUNT(*) FROM audit_log`)
	desiredCount := countRows(t, st, `SELECT COUNT(*) FROM desired_state`)
	jobCount := countRows(t, st, `SELECT COUNT(*) FROM jobs`)
	if auditCount != beforeAudit || desiredCount != 1 || jobCount != 1 {
		t.Errorf("detached-cache-time replay authority rows: audit=%d desired_state=%d jobs=%d, "+
			"expected audit=%d desired_state=1 jobs=1; refusing the provenance-broken row still changed "+
			"the operator's live device-sync authority", auditCount, desiredCount, jobCount, beforeAudit)
	}
}

func TestOperatorDeviceSyncReceiptBindsTypedConfirmation(t *testing.T) {
	st, machineID, _ := deviceSyncFixture(t)
	preview, err := st.PreviewOperatorDeviceSync(machineID, model.DeviceSyncDefaultTimeoutSeconds)
	if err != nil {
		t.Fatal(err)
	}
	req := deviceSyncRequest(machineID, preview, "device-sync-confirmed-display-name")
	if _, err := st.ApplyOperatorDeviceSync(req); err != nil {
		t.Fatal(err)
	}
	var raw string
	if err := st.DB().QueryRow(`SELECT response_json FROM operator_idempotency
		WHERE idempotency_key=?`, req.IdempotencyKey).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	receipt, err := decodeOperatorDeviceSyncReceipt(raw)
	if err != nil {
		t.Fatal(err)
	}
	receipt.DisplayName = "sync-tampered"
	changed, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`UPDATE operator_idempotency SET response_json=?
		WHERE idempotency_key=?`, string(changed), req.IdempotencyKey); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`UPDATE audit_log SET subject=?,detail=?
		WHERE action='device-sync' AND idempotency_key=?`, receipt.DisplayName,
		operatorDeviceSyncSuccessAuditDetail(receipt), req.IdempotencyKey); err != nil {
		t.Fatal(err)
	}
	beforeAudit := countRows(t, st, `SELECT COUNT(*) FROM audit_log WHERE action='device-sync'`)
	if _, err := st.ApplyOperatorDeviceSync(req); err == nil ||
		!strings.Contains(err.Error(), "idempotency cache invalid") {
		t.Fatalf("replay with changed confirmed display name err=%v", err)
	}
	if countRows(t, st, `SELECT COUNT(*) FROM audit_log WHERE action='device-sync'`) != beforeAudit ||
		countRows(t, st, `SELECT COUNT(*) FROM operator_idempotency`) != 1 ||
		countRows(t, st, `SELECT COUNT(*) FROM revision_counters WHERE resource_scope='device:sync'`) != 1 ||
		countRows(t, st, `SELECT COUNT(*) FROM desired_state`) != 1 ||
		countRows(t, st, `SELECT COUNT(*) FROM jobs`) != 1 {
		t.Fatal("changed typed confirmation replay changed authority")
	}
}

func TestOperatorDeviceSyncReceiptBindsPreviewDigestFacts(t *testing.T) {
	st, machineID, _ := deviceSyncFixture(t)
	preview, err := st.PreviewOperatorDeviceSync(machineID, model.DeviceSyncDefaultTimeoutSeconds)
	if err != nil {
		t.Fatal(err)
	}
	req := deviceSyncRequest(machineID, preview, "device-sync-preview-facts")
	if _, err := st.ApplyOperatorDeviceSync(req); err != nil {
		t.Fatal(err)
	}
	var raw string
	if err := st.DB().QueryRow(`SELECT response_json FROM operator_idempotency
		WHERE idempotency_key=?`, req.IdempotencyKey).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	receipt, err := decodeOperatorDeviceSyncReceipt(raw)
	if err != nil {
		t.Fatal(err)
	}
	changedSentAt := receipt.LatestCheckinSentAt.Add(time.Second)
	receipt.LatestCheckinSentAt = &changedSentAt
	changed, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`UPDATE operator_idempotency SET response_json=?
		WHERE idempotency_key=?`, string(changed), req.IdempotencyKey); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`UPDATE audit_log SET detail=?
		WHERE action='device-sync' AND idempotency_key=?`,
		operatorDeviceSyncSuccessAuditDetail(receipt), req.IdempotencyKey); err != nil {
		t.Fatal(err)
	}
	beforeAudit := countRows(t, st, `SELECT COUNT(*) FROM audit_log WHERE action='device-sync'`)
	if _, err := st.ApplyOperatorDeviceSync(req); err == nil ||
		!strings.Contains(err.Error(), "idempotency cache invalid") {
		t.Fatalf("replay with receipt facts detached from preview digest err=%v", err)
	}
	if countRows(t, st, `SELECT COUNT(*) FROM audit_log WHERE action='device-sync'`) != beforeAudit ||
		countRows(t, st, `SELECT COUNT(*) FROM operator_idempotency`) != 1 ||
		countRows(t, st, `SELECT COUNT(*) FROM revision_counters WHERE resource_scope='device:sync'`) != 1 ||
		countRows(t, st, `SELECT COUNT(*) FROM desired_state`) != 1 ||
		countRows(t, st, `SELECT COUNT(*) FROM jobs`) != 1 {
		t.Fatal("receipt facts detached from preview digest changed authority")
	}
}

func TestOperatorDeviceSyncReceiptBindsRequestCreator(t *testing.T) {
	st, machineID, _ := deviceSyncFixture(t)
	preview, err := st.PreviewOperatorDeviceSync(machineID, model.DeviceSyncDefaultTimeoutSeconds)
	if err != nil {
		t.Fatal(err)
	}
	req := deviceSyncRequest(machineID, preview, "device-sync-request-creator")
	result, err := st.ApplyOperatorDeviceSync(req)
	if err != nil {
		t.Fatal(err)
	}
	var raw string
	if err := st.DB().QueryRow(`SELECT response_json FROM operator_idempotency
		WHERE idempotency_key=?`, req.IdempotencyKey).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	receipt, err := decodeOperatorDeviceSyncReceipt(raw)
	if err != nil {
		t.Fatal(err)
	}
	receipt.CreatedBy = "operator:tampered"
	changed, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`UPDATE operator_idempotency SET response_json=?
		WHERE idempotency_key=?`, string(changed), req.IdempotencyKey); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`UPDATE desired_state SET created_by=? WHERE desired_id=?`,
		receipt.CreatedBy, result.DesiredID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`UPDATE audit_log SET detail=?
		WHERE action='device-sync' AND idempotency_key=?`,
		operatorDeviceSyncSuccessAuditDetail(receipt), req.IdempotencyKey); err != nil {
		t.Fatal(err)
	}
	beforeAudit := countRows(t, st, `SELECT COUNT(*) FROM audit_log WHERE action='device-sync'`)
	if _, err := st.ApplyOperatorDeviceSync(req); err == nil ||
		!strings.Contains(err.Error(), "idempotency cache invalid") {
		t.Fatalf("replay with changed request creator err=%v", err)
	}
	if countRows(t, st, `SELECT COUNT(*) FROM audit_log WHERE action='device-sync'`) != beforeAudit ||
		countRows(t, st, `SELECT COUNT(*) FROM operator_idempotency`) != 1 ||
		countRows(t, st, `SELECT COUNT(*) FROM desired_state WHERE created_by=?`, receipt.CreatedBy) != 1 ||
		countRows(t, st, `SELECT COUNT(*) FROM jobs`) != 1 {
		t.Fatal("changed request creator replay changed authority")
	}
}

func TestOperatorDeviceSyncReplayAuditFailurePreservesRecoverableDecision(t *testing.T) {
	t.Run("successful decision survives new blockers", func(t *testing.T) {
		st, machineID, now := deviceSyncFixture(t)
		preview, err := st.PreviewOperatorDeviceSync(machineID, model.DeviceSyncDefaultTimeoutSeconds)
		if err != nil {
			t.Fatal(err)
		}
		req := deviceSyncRequest(machineID, preview, "device-sync-success-recovery")
		original, err := st.ApplyOperatorDeviceSync(req)
		if err != nil {
			t.Fatal(err)
		}
		disabled := false
		checkin := healthyCheckin(now.Add(-10 * time.Second))
		checkin.JobsEnabled, checkin.DeviceSyncV1 = &disabled, false
		if err := st.RecordCheckin(machineID, checkin, now.Add(-10*time.Second)); err != nil {
			t.Fatal(err)
		}
		blocked, err := st.PreviewOperatorDeviceSync(machineID, model.DeviceSyncDefaultTimeoutSeconds)
		if err != nil || len(blocked.Blockers) != 3 ||
			blocked.Blockers[0] != OperatorDeviceSyncBlockerCapabilityUnconfirmed ||
			blocked.Blockers[1] != OperatorDeviceSyncBlockerExecutionDisabled ||
			blocked.Blockers[2] != OperatorDeviceSyncBlockerNonterminalJob {
			t.Fatalf("new preview=%+v err=%v", blocked, err)
		}
		if _, err := st.DB().Exec(`CREATE TRIGGER fail_device_sync_success_replay_audit
			BEFORE INSERT ON audit_log WHEN NEW.detail LIKE 'idempotency replay；%'
			BEGIN SELECT RAISE(ABORT, 'replay audit unavailable'); END`); err != nil {
			t.Fatal(err)
		}
		if _, err := st.ApplyOperatorDeviceSync(req); err == nil {
			t.Fatal("successful replay accepted without durable replay audit")
		}
		if countRows(t, st, `SELECT COUNT(*) FROM audit_log WHERE action='device-sync'`) != 1 ||
			countRows(t, st, `SELECT COUNT(*) FROM operator_idempotency`) != 1 ||
			countRows(t, st, `SELECT COUNT(*) FROM desired_state`) != 1 ||
			countRows(t, st, `SELECT COUNT(*) FROM jobs`) != 1 {
			t.Fatal("failed successful replay changed original authority")
		}
		if _, err := st.DB().Exec(`DROP TRIGGER fail_device_sync_success_replay_audit`); err != nil {
			t.Fatal(err)
		}
		replay, err := st.ApplyOperatorDeviceSync(req)
		if err != nil || !replay.Replayed || !replay.Audited || replay.JobID != original.JobID ||
			replay.DesiredID != original.DesiredID || replay.Revision != original.Revision {
			t.Fatalf("recovered replay=%+v err=%v", replay, err)
		}
		if countRows(t, st, `SELECT COUNT(*) FROM audit_log WHERE action='device-sync'`) != 2 ||
			countRows(t, st, `SELECT COUNT(*) FROM jobs`) != 1 {
			t.Fatal("recovered successful replay duplicated authority")
		}
	})

	t.Run("rejected decision survives replay audit failure", func(t *testing.T) {
		st, machineID, now := deviceSyncFixture(t)
		disabled := false
		checkin := healthyCheckin(now.Add(-10 * time.Second))
		checkin.JobsEnabled, checkin.DeviceSyncV1 = &disabled, true
		if err := st.RecordCheckin(machineID, checkin, now.Add(-10*time.Second)); err != nil {
			t.Fatal(err)
		}
		preview, err := st.PreviewOperatorDeviceSync(machineID, model.DeviceSyncDefaultTimeoutSeconds)
		if err != nil {
			t.Fatal(err)
		}
		req := deviceSyncRequest(machineID, preview, "device-sync-rejection-recovery")
		if _, err := st.ApplyOperatorDeviceSync(req); !errors.Is(err, ErrAgentExecutionDisabled) {
			t.Fatalf("original rejection err=%v", err)
		}
		if _, err := st.DB().Exec(`CREATE TRIGGER fail_device_sync_rejected_replay_audit
			BEFORE INSERT ON audit_log WHEN NEW.detail LIKE 'idempotency replay；%'
			BEGIN SELECT RAISE(ABORT, 'replay audit unavailable'); END`); err != nil {
			t.Fatal(err)
		}
		if _, err := st.ApplyOperatorDeviceSync(req); err == nil || errors.Is(err, ErrAgentExecutionDisabled) {
			t.Fatalf("rejected replay audit failure err=%v", err)
		}
		if countRows(t, st, `SELECT COUNT(*) FROM audit_log WHERE action='device-sync'`) != 1 ||
			countRows(t, st, `SELECT COUNT(*) FROM operator_idempotency`) != 1 ||
			countRows(t, st, `SELECT COUNT(*) FROM desired_state`) != 0 ||
			countRows(t, st, `SELECT COUNT(*) FROM jobs`) != 0 {
			t.Fatal("failed rejected replay changed original authority")
		}
		if _, err := st.DB().Exec(`DROP TRIGGER fail_device_sync_rejected_replay_audit`); err != nil {
			t.Fatal(err)
		}
		_, err = st.ApplyOperatorDeviceSync(req)
		var replay *OperatorRequestError
		if !errors.Is(err, ErrAgentExecutionDisabled) || !errors.As(err, &replay) || !replay.Replayed || !replay.Audited {
			t.Fatalf("recovered rejection=%+v err=%v", replay, err)
		}
		if countRows(t, st, `SELECT COUNT(*) FROM audit_log WHERE action='device-sync'`) != 2 ||
			countRows(t, st, `SELECT COUNT(*) FROM operator_idempotency`) != 1 {
			t.Fatal("recovered rejected replay duplicated decision")
		}
	})
}

func TestOperatorDeviceSyncReplayBindsRevisionAuthority(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate string
	}{
		{name: "missing counter fails closed", mutate: `DELETE FROM revision_counters WHERE resource_scope='device:sync'`},
		{name: "rolled back counter fails closed", mutate: `UPDATE revision_counters SET current_revision=0 WHERE resource_scope='device:sync'`},
	} {
		t.Run(test.name, func(t *testing.T) {
			st, machineID, _ := deviceSyncFixture(t)
			preview, err := st.PreviewOperatorDeviceSync(machineID, model.DeviceSyncDefaultTimeoutSeconds)
			if err != nil {
				t.Fatal(err)
			}
			req := deviceSyncRequest(machineID, preview, "device-sync-invalid-revision-counter-"+test.name)
			if _, err := st.ApplyOperatorDeviceSync(req); err != nil {
				t.Fatal(err)
			}
			if _, err := st.DB().Exec(test.mutate); err != nil {
				t.Fatal(err)
			}
			if _, err := st.ApplyOperatorDeviceSync(req); err == nil ||
				!strings.Contains(err.Error(), "idempotency cache invalid") {
				t.Fatalf("replay without revision authority err=%v", err)
			}
			if countRows(t, st, `SELECT COUNT(*) FROM audit_log WHERE action='device-sync'`) != 1 ||
				countRows(t, st, `SELECT COUNT(*) FROM operator_idempotency`) != 1 ||
				countRows(t, st, `SELECT COUNT(*) FROM desired_state`) != 1 ||
				countRows(t, st, `SELECT COUNT(*) FROM jobs`) != 1 {
				t.Fatal("invalid revision authority replay changed original decision")
			}
		})
	}

	t.Run("later revision preserves old receipt", func(t *testing.T) {
		st, machineID, _ := deviceSyncFixture(t)
		preview, err := st.PreviewOperatorDeviceSync(machineID, model.DeviceSyncDefaultTimeoutSeconds)
		if err != nil {
			t.Fatal(err)
		}
		req := deviceSyncRequest(machineID, preview, "device-sync-advanced-revision-counter")
		original, err := st.ApplyOperatorDeviceSync(req)
		if err != nil {
			t.Fatal(err)
		}
		if _, revision, err := st.CreateDesiredState("machine", machineID, model.DeviceSyncResourceKind,
			model.DeviceSyncResourceID, model.DeviceSyncSpecJSON, "operator:later"); err != nil || revision != 2 {
			t.Fatalf("later revision=%d err=%v", revision, err)
		}
		replay, err := st.ApplyOperatorDeviceSync(req)
		if err != nil || !replay.Replayed || !replay.Audited || replay.JobID != original.JobID ||
			replay.DesiredID != original.DesiredID || replay.Revision != original.Revision {
			t.Fatalf("replay after later revision=%+v err=%v", replay, err)
		}
		if countRows(t, st, `SELECT COUNT(*) FROM revision_counters
			WHERE resource_scope='device:sync' AND current_revision=2`) != 1 ||
			countRows(t, st, `SELECT COUNT(*) FROM desired_state`) != 2 ||
			countRows(t, st, `SELECT COUNT(*) FROM jobs`) != 1 ||
			countRows(t, st, `SELECT COUNT(*) FROM audit_log WHERE action='device-sync'`) != 2 {
			t.Fatal("valid later revision changed replay authority")
		}
	})
}

func TestOperatorDeviceSyncConflictAuditFailurePreservesOriginalDecision(t *testing.T) {
	st, machineID, _ := deviceSyncFixture(t)
	preview, err := st.PreviewOperatorDeviceSync(machineID, model.DeviceSyncDefaultTimeoutSeconds)
	if err != nil {
		t.Fatal(err)
	}
	req := deviceSyncRequest(machineID, preview, "device-sync-conflict-audit-recovery")
	original, err := st.ApplyOperatorDeviceSync(req)
	if err != nil {
		t.Fatal(err)
	}
	conflict := req
	conflict.RequestDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if _, err := st.DB().Exec(`CREATE TRIGGER fail_device_sync_conflict_audit
		BEFORE INSERT ON audit_log WHEN NEW.detail LIKE 'idempotency conflict：%'
		BEGIN SELECT RAISE(ABORT, 'conflict audit unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ApplyOperatorDeviceSync(conflict); err == nil || errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("conflict accepted without durable audit err=%v", err)
	}
	if countRows(t, st, `SELECT COUNT(*) FROM audit_log WHERE action='device-sync'`) != 1 ||
		countRows(t, st, `SELECT COUNT(*) FROM operator_idempotency`) != 1 ||
		countRows(t, st, `SELECT COUNT(*) FROM revision_counters WHERE resource_scope='device:sync'`) != 1 ||
		countRows(t, st, `SELECT COUNT(*) FROM desired_state`) != 1 ||
		countRows(t, st, `SELECT COUNT(*) FROM jobs`) != 1 {
		t.Fatal("failed conflict audit changed original decision")
	}
	if _, err := st.DB().Exec(`DROP TRIGGER fail_device_sync_conflict_audit`); err != nil {
		t.Fatal(err)
	}
	_, err = st.ApplyOperatorDeviceSync(conflict)
	var rejection *OperatorRequestError
	if !errors.Is(err, ErrIdempotencyConflict) || !errors.As(err, &rejection) || !rejection.Audited || rejection.Replayed {
		t.Fatalf("recovered conflict=%+v err=%v", rejection, err)
	}
	if countRows(t, st, `SELECT COUNT(*) FROM audit_log WHERE action='device-sync'`) != 2 ||
		countRows(t, st, `SELECT COUNT(*) FROM operator_idempotency`) != 1 ||
		countRows(t, st, `SELECT COUNT(*) FROM desired_state`) != 1 ||
		countRows(t, st, `SELECT COUNT(*) FROM jobs`) != 1 {
		t.Fatal("recovered conflict duplicated original decision")
	}
	replay, err := st.ApplyOperatorDeviceSync(req)
	if err != nil || !replay.Replayed || replay.JobID != original.JobID || replay.DesiredID != original.DesiredID {
		t.Fatalf("original decision after conflict=%+v err=%v", replay, err)
	}
}

func TestOperatorDeviceSyncRejectedReplayFailsClosedOnTamperedEvidence(t *testing.T) {
	for _, test := range []struct {
		name   string
		tamper string
	}{
		{name: "outcome", tamper: `UPDATE operator_idempotency SET outcome='ok'`},
		{name: "response shape", tamper: `UPDATE operator_idempotency SET response_json='{}'`},
		{name: "error code", tamper: `UPDATE operator_idempotency SET error_code='UNKNOWN'`},
		{name: "error detail", tamper: `UPDATE operator_idempotency SET error_detail='tampered'`},
		{name: "created at", tamper: `UPDATE operator_idempotency SET created_at='2026-09-14T08:00:00-04:00'`},
		{name: "original audit", tamper: `DELETE FROM audit_log WHERE action='device-sync'`},
		{name: "audit detail", tamper: `UPDATE audit_log SET detail='tampered' WHERE action='device-sync'`},
	} {
		t.Run(test.name, func(t *testing.T) {
			st, machineID, now := deviceSyncFixture(t)
			disabled := false
			checkin := healthyCheckin(now.Add(-10 * time.Second))
			checkin.JobsEnabled, checkin.DeviceSyncV1 = &disabled, true
			if err := st.RecordCheckin(machineID, checkin, now.Add(-10*time.Second)); err != nil {
				t.Fatal(err)
			}
			preview, err := st.PreviewOperatorDeviceSync(machineID, model.DeviceSyncDefaultTimeoutSeconds)
			if err != nil {
				t.Fatal(err)
			}
			req := deviceSyncRequest(machineID, preview, "device-sync-rejected-tamper-"+test.name)
			if _, err := st.ApplyOperatorDeviceSync(req); !errors.Is(err, ErrAgentExecutionDisabled) {
				t.Fatalf("original rejection err=%v", err)
			}
			if _, err := st.DB().Exec(test.tamper); err != nil {
				t.Fatal(err)
			}
			beforeAudit := countRows(t, st, `SELECT COUNT(*) FROM audit_log WHERE action='device-sync'`)
			if _, err := st.ApplyOperatorDeviceSync(req); err == nil ||
				errors.Is(err, ErrAgentExecutionDisabled) || !strings.Contains(err.Error(), "idempotency cache invalid") {
				t.Fatalf("tampered rejected replay err=%v", err)
			}
			if countRows(t, st, `SELECT COUNT(*) FROM audit_log WHERE action='device-sync'`) != beforeAudit ||
				countRows(t, st, `SELECT COUNT(*) FROM operator_idempotency`) != 1 ||
				countRows(t, st, `SELECT COUNT(*) FROM revision_counters`) != 0 ||
				countRows(t, st, `SELECT COUNT(*) FROM desired_state`) != 0 ||
				countRows(t, st, `SELECT COUNT(*) FROM jobs`) != 0 {
				t.Fatal("tampered rejected replay changed authority")
			}
		})
	}
}

func TestOperatorDeviceSyncReplayRequiresOriginalAuditProvenance(t *testing.T) {
	mutations := []struct {
		name   string
		column string
	}{
		{name: "source address", column: "source_addr"},
		{name: "tailnet node", column: "who_node"},
		{name: "tailnet user", column: "who_user"},
		{name: "tailnet lookup failure", column: "who_unavailable"},
		{name: "user agent", column: "user_agent"},
		{name: "auth subject", column: "auth_subject"},
		{name: "auth node", column: "auth_node_id"},
		{name: "auth capability", column: "auth_capability"},
		{name: "auth method", column: "auth_method"},
		{name: "auth decision", column: "auth_decision"},
		{name: "boundary decision", column: "boundary_decision"},
		{name: "source kind", column: "source_kind"},
	}
	outcomes := []struct {
		name     string
		rejected bool
	}{
		{name: "success"},
		{name: "rejection", rejected: true},
	}
	for _, outcome := range outcomes {
		for _, mutation := range mutations {
			t.Run(outcome.name+"/"+mutation.name, func(t *testing.T) {
				st, machineID, now := deviceSyncFixture(t)
				if outcome.rejected {
					disabled := false
					checkin := healthyCheckin(now.Add(-10 * time.Second))
					checkin.JobsEnabled, checkin.DeviceSyncV1 = &disabled, true
					if err := st.RecordCheckin(machineID, checkin, now.Add(-10*time.Second)); err != nil {
						t.Fatal(err)
					}
				}
				preview, err := st.PreviewOperatorDeviceSync(machineID, model.DeviceSyncDefaultTimeoutSeconds)
				if err != nil {
					t.Fatal(err)
				}
				req := deviceSyncRequest(machineID, preview,
					"device-sync-audit-provenance-"+outcome.name+"-"+mutation.name)
				_, err = st.ApplyOperatorDeviceSync(req)
				if outcome.rejected {
					if !errors.Is(err, ErrAgentExecutionDisabled) {
						t.Fatalf("original rejection err=%v", err)
					}
				} else if err != nil {
					t.Fatal(err)
				}
				query := fmt.Sprintf("UPDATE audit_log SET %s='tampered' WHERE action=? AND idempotency_key=?",
					mutation.column)
				if _, err := st.DB().Exec(query, string(AuditDeviceSync), req.IdempotencyKey); err != nil {
					t.Fatal(err)
				}
				beforeAudit := countRows(t, st, `SELECT COUNT(*) FROM audit_log WHERE action='device-sync'`)
				if _, err := st.ApplyOperatorDeviceSync(req); err == nil ||
					!strings.Contains(err.Error(), "idempotency cache invalid") {
					t.Fatalf("replay with tampered %s err=%v", mutation.column, err)
				}
				wantAuthorityRows := 1
				if outcome.rejected {
					wantAuthorityRows = 0
				}
				if countRows(t, st, `SELECT COUNT(*) FROM audit_log WHERE action='device-sync'`) != beforeAudit ||
					countRows(t, st, `SELECT COUNT(*) FROM operator_idempotency`) != 1 ||
					countRows(t, st, `SELECT COUNT(*) FROM desired_state`) != wantAuthorityRows ||
					countRows(t, st, `SELECT COUNT(*) FROM jobs`) != wantAuthorityRows {
					t.Fatal("tampered original audit provenance replay changed authority")
				}
			})
		}
	}
}

func TestOperatorDeviceSyncReplayRejectsTamperedJobAuthority(t *testing.T) {
	for _, test := range []struct {
		name   string
		tamper func(*testing.T, *Store, OperatorDeviceSyncResult, time.Time)
	}{
		{name: "machine", tamper: func(t *testing.T, st *Store, result OperatorDeviceSyncResult, now time.Time) {
			other := mustEnroll(t, st, "sync-other", now.Add(-time.Hour))
			if _, err := st.DB().Exec(`UPDATE jobs SET machine_id=? WHERE job_id=?`, other, result.JobID); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "desired link", tamper: func(t *testing.T, st *Store, result OperatorDeviceSyncResult, _ time.Time) {
			if _, err := st.DB().Exec(`UPDATE jobs SET desired_id=NULL WHERE job_id=?`, result.JobID); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "revision", tamper: func(t *testing.T, st *Store, result OperatorDeviceSyncResult, _ time.Time) {
			if _, err := st.DB().Exec(`UPDATE jobs SET revision=revision+1 WHERE job_id=?`, result.JobID); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "created at", tamper: func(t *testing.T, st *Store, result OperatorDeviceSyncResult, now time.Time) {
			if _, err := st.DB().Exec(`UPDATE jobs SET created_at=? WHERE job_id=?`, fmtTime(now.Add(time.Second)), result.JobID); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "artifact digest", tamper: func(t *testing.T, st *Store, result OperatorDeviceSyncResult, _ time.Time) {
			if _, err := st.DB().Exec(`UPDATE jobs SET artifact_digest=? WHERE job_id=?`,
				"sha256:"+strings.Repeat("a", 64), result.JobID); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "irreversible", tamper: func(t *testing.T, st *Store, result OperatorDeviceSyncResult, _ time.Time) {
			if _, err := st.DB().Exec(`UPDATE jobs SET irreversible=1 WHERE job_id=?`, result.JobID); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "execution timeout", tamper: func(t *testing.T, st *Store, result OperatorDeviceSyncResult, _ time.Time) {
			if _, err := st.DB().Exec(`UPDATE jobs SET execution_timeout=execution_timeout+1 WHERE job_id=?`, result.JobID); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			st, machineID, now := deviceSyncFixture(t)
			preview, err := st.PreviewOperatorDeviceSync(machineID, model.DeviceSyncDefaultTimeoutSeconds)
			if err != nil {
				t.Fatal(err)
			}
			req := deviceSyncRequest(machineID, preview, "device-sync-job-tamper-"+test.name)
			result, err := st.ApplyOperatorDeviceSync(req)
			if err != nil {
				t.Fatal(err)
			}
			test.tamper(t, st, result, now)
			beforeAudit := countRows(t, st, `SELECT COUNT(*) FROM audit_log WHERE action='device-sync'`)
			if _, err := st.ApplyOperatorDeviceSync(req); err == nil ||
				!strings.Contains(err.Error(), "idempotency cache invalid") {
				t.Fatalf("tampered job replay err=%v", err)
			}
			if countRows(t, st, `SELECT COUNT(*) FROM audit_log WHERE action='device-sync'`) != beforeAudit ||
				countRows(t, st, `SELECT COUNT(*) FROM operator_idempotency`) != 1 ||
				countRows(t, st, `SELECT COUNT(*) FROM revision_counters WHERE resource_scope='device:sync'`) != 1 ||
				countRows(t, st, `SELECT COUNT(*) FROM desired_state`) != 1 ||
				countRows(t, st, `SELECT COUNT(*) FROM jobs`) != 1 {
				t.Fatal("tampered job replay changed authority")
			}
		})
	}
}

func TestOperatorDeviceSyncReplayRejectsUnknownJobState(t *testing.T) {
	st, machineID, _ := deviceSyncFixture(t)
	preview, err := st.PreviewOperatorDeviceSync(machineID, model.DeviceSyncDefaultTimeoutSeconds)
	if err != nil {
		t.Fatal(err)
	}
	req := deviceSyncRequest(machineID, preview, "device-sync-unknown-job-state")
	result, err := st.ApplyOperatorDeviceSync(req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`UPDATE jobs SET state='tampered' WHERE job_id=?`, result.JobID); err != nil {
		t.Fatal(err)
	}
	beforeAudit := countRows(t, st, `SELECT COUNT(*) FROM audit_log WHERE action='device-sync'`)
	if _, err := st.ApplyOperatorDeviceSync(req); err == nil ||
		!strings.Contains(err.Error(), "idempotency cache invalid") {
		t.Fatalf("replay with unknown job state err=%v", err)
	}
	if countRows(t, st, `SELECT COUNT(*) FROM audit_log WHERE action='device-sync'`) != beforeAudit ||
		countRows(t, st, `SELECT COUNT(*) FROM operator_idempotency`) != 1 ||
		countRows(t, st, `SELECT COUNT(*) FROM revision_counters WHERE resource_scope='device:sync'`) != 1 ||
		countRows(t, st, `SELECT COUNT(*) FROM desired_state`) != 1 ||
		countRows(t, st, `SELECT COUNT(*) FROM jobs WHERE state='tampered'`) != 1 {
		t.Fatal("unknown job state replay changed device-sync authority")
	}
}

func TestOperatorDeviceSyncReplayRejectsIrreversibleFailureVerdict(t *testing.T) {
	st, machineID, now := deviceSyncFixture(t)
	preview, err := st.PreviewOperatorDeviceSync(machineID, model.DeviceSyncDefaultTimeoutSeconds)
	if err != nil {
		t.Fatal(err)
	}
	req := deviceSyncRequest(machineID, preview, "device-sync-irreversible-verdict")
	result, err := st.ApplyOperatorDeviceSync(req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`UPDATE jobs SET state=?,terminal_at=? WHERE job_id=?`,
		deploy.ManualIntervention, fmtTime(now.Add(time.Second)), result.JobID); err != nil {
		t.Fatal(err)
	}
	beforeAudit := countRows(t, st, `SELECT COUNT(*) FROM audit_log WHERE action='device-sync'`)
	if _, err := st.ApplyOperatorDeviceSync(req); err == nil ||
		!strings.Contains(err.Error(), "idempotency cache invalid") {
		t.Fatalf("irreversible failure verdict replay err=%v", err)
	}
	if countRows(t, st, `SELECT COUNT(*) FROM audit_log WHERE action='device-sync'`) != beforeAudit ||
		countRows(t, st, `SELECT COUNT(*) FROM operator_idempotency`) != 1 ||
		countRows(t, st, `SELECT COUNT(*) FROM desired_state`) != 1 ||
		countRows(t, st, `SELECT COUNT(*) FROM jobs WHERE state='manual_intervention' AND irreversible=0`) != 1 {
		t.Fatal("irreversible failure verdict replay changed device-sync authority")
	}
}

func TestOperatorDeviceSyncReplayRequiresSucceededVerificationAuthority(t *testing.T) {
	t.Run("succeeded without verification", func(t *testing.T) {
		st, machineID, now := deviceSyncFixture(t)
		preview, err := st.PreviewOperatorDeviceSync(machineID, model.DeviceSyncDefaultTimeoutSeconds)
		if err != nil {
			t.Fatal(err)
		}
		req := deviceSyncRequest(machineID, preview, "device-sync-unverified-success")
		result, err := st.ApplyOperatorDeviceSync(req)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.DB().Exec(`UPDATE jobs SET state=?,terminal_at=? WHERE job_id=?`,
			deploy.Succeeded, fmtTime(now.Add(time.Second)), result.JobID); err != nil {
			t.Fatal(err)
		}
		st.nowFn = func() time.Time { return now.Add(2 * time.Second) }
		beforeAudit := countRows(t, st, `SELECT COUNT(*) FROM audit_log WHERE action='device-sync'`)
		if _, err := st.ApplyOperatorDeviceSync(req); err == nil ||
			!strings.Contains(err.Error(), "idempotency cache invalid") {
			t.Fatalf("unverified success replay err=%v", err)
		}
		if countRows(t, st, `SELECT COUNT(*) FROM audit_log WHERE action='device-sync'`) != beforeAudit ||
			countRows(t, st, `SELECT COUNT(*) FROM operator_idempotency`) != 1 ||
			countRows(t, st, `SELECT COUNT(*) FROM verification_results`) != 0 ||
			countRows(t, st, `SELECT COUNT(*) FROM desired_state`) != 1 ||
			countRows(t, st, `SELECT COUNT(*) FROM jobs`) != 1 {
			t.Fatal("unverified success replay changed device-sync authority")
		}
	})

	t.Run("verified success", func(t *testing.T) {
		st, machineID, now := deviceSyncFixture(t)
		preview, err := st.PreviewOperatorDeviceSync(machineID, model.DeviceSyncDefaultTimeoutSeconds)
		if err != nil {
			t.Fatal(err)
		}
		req := deviceSyncRequest(machineID, preview, "device-sync-verified-success")
		result, err := st.ApplyOperatorDeviceSync(req)
		if err != nil {
			t.Fatal(err)
		}
		token, err := st.ClaimJob(result.JobID, machineID, now, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		appendDeviceSyncPhaseEvent(t, st, result.JobID, machineID, token, 1, "start", now.Add(time.Second))
		if _, err := st.AdvanceJobByAgent(result.JobID, machineID, token, deploy.Start, now.Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		st.nowFn = func() time.Time { return now.Add(2 * time.Second) }
		if err := st.RecordVerification(result.JobID, machineID, token, model.DeviceSyncVerificationRuleID,
			model.DeviceSyncVerificationCommand, 0, model.DeviceSyncVerificationStdout, "", true,
			now.Add(2*time.Second)); err != nil {
			t.Fatal(err)
		}
		appendDeviceSyncPhaseEvent(t, st, result.JobID, machineID, token, 2, "finish", now.Add(3*time.Second))
		if _, err := st.AdvanceJobByAgent(result.JobID, machineID, token, deploy.FinishWork, now.Add(3*time.Second)); err != nil {
			t.Fatal(err)
		}
		if _, err := st.MarkSucceededIfVerified(result.JobID, now.Add(4*time.Second)); err != nil {
			t.Fatal(err)
		}
		st.nowFn = func() time.Time { return now.Add(5 * time.Second) }
		replay, err := st.ApplyOperatorDeviceSync(req)
		if err != nil || !replay.Replayed || !replay.Audited || replay.JobID != result.JobID ||
			replay.DesiredID != result.DesiredID {
			t.Fatalf("verified success replay=%+v err=%v", replay, err)
		}
		if countRows(t, st, `SELECT COUNT(*) FROM audit_log WHERE action='device-sync'`) != 2 ||
			countRows(t, st, `SELECT COUNT(*) FROM verification_results WHERE job_id=? AND passed=1`, result.JobID) != 1 ||
			countRows(t, st, `SELECT COUNT(*) FROM desired_state`) != 1 ||
			countRows(t, st, `SELECT COUNT(*) FROM jobs WHERE state='succeeded'`) != 1 {
			t.Fatal("verified success replay changed device-sync authority")
		}
	})
}

func TestOperatorDeviceSyncReplayRequiresFixedSucceededVerification(t *testing.T) {
	st, machineID, now := deviceSyncFixture(t)
	preview, err := st.PreviewOperatorDeviceSync(machineID, model.DeviceSyncDefaultTimeoutSeconds)
	if err != nil {
		t.Fatal(err)
	}
	req := deviceSyncRequest(machineID, preview, "device-sync-wrong-verification")
	result, err := st.ApplyOperatorDeviceSync(req)
	if err != nil {
		t.Fatal(err)
	}
	token, err := st.ClaimJob(result.JobID, machineID, now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	appendDeviceSyncPhaseEvent(t, st, result.JobID, machineID, token, 1, "start", now.Add(time.Second))
	if _, err := st.AdvanceJobByAgent(result.JobID, machineID, token, deploy.Start, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordVerification(result.JobID, machineID, token, "noop", "true", 0,
		"unrelated verification passed", "", true, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	appendDeviceSyncPhaseEvent(t, st, result.JobID, machineID, token, 2, "finish", now.Add(3*time.Second))
	if _, err := st.AdvanceJobByAgent(result.JobID, machineID, token, deploy.FinishWork, now.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.MarkSucceededIfVerified(result.JobID, now.Add(4*time.Second)); err != nil {
		t.Fatal(err)
	}
	st.nowFn = func() time.Time { return now.Add(5 * time.Second) }
	beforeAudit := countRows(t, st, `SELECT COUNT(*) FROM audit_log WHERE action='device-sync'`)
	if _, err := st.ApplyOperatorDeviceSync(req); err == nil ||
		!strings.Contains(err.Error(), "idempotency cache invalid") {
		t.Fatalf("wrong verification replay err=%v", err)
	}
	if countRows(t, st, `SELECT COUNT(*) FROM audit_log WHERE action='device-sync'`) != beforeAudit ||
		countRows(t, st, `SELECT COUNT(*) FROM operator_idempotency`) != 1 ||
		countRows(t, st, `SELECT COUNT(*) FROM verification_results WHERE job_id=?`, result.JobID) != 1 ||
		countRows(t, st, `SELECT COUNT(*) FROM desired_state`) != 1 ||
		countRows(t, st, `SELECT COUNT(*) FROM jobs WHERE state='succeeded'`) != 1 {
		t.Fatal("wrong verification replay changed device-sync authority")
	}

	t.Run("fixed plus unrelated", func(t *testing.T) {
		st, machineID, now := deviceSyncFixture(t)
		preview, err := st.PreviewOperatorDeviceSync(machineID, model.DeviceSyncDefaultTimeoutSeconds)
		if err != nil {
			t.Fatal(err)
		}
		req := deviceSyncRequest(machineID, preview, "device-sync-mixed-verification")
		result, err := st.ApplyOperatorDeviceSync(req)
		if err != nil {
			t.Fatal(err)
		}
		token, err := st.ClaimJob(result.JobID, machineID, now, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		appendDeviceSyncPhaseEvent(t, st, result.JobID, machineID, token, 1, "start", now.Add(time.Second))
		if _, err := st.AdvanceJobByAgent(result.JobID, machineID, token, deploy.Start, now.Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		if err := st.RecordVerification(result.JobID, machineID, token,
			model.DeviceSyncVerificationRuleID, model.DeviceSyncVerificationCommand, 0,
			model.DeviceSyncVerificationStdout, "", true, now.Add(2*time.Second)); err != nil {
			t.Fatal(err)
		}
		if err := st.RecordVerification(result.JobID, machineID, token, "noop", "true", 0,
			"unrelated verification passed", "", true, now.Add(2*time.Second)); err != nil {
			t.Fatal(err)
		}
		appendDeviceSyncPhaseEvent(t, st, result.JobID, machineID, token, 2, "finish", now.Add(3*time.Second))
		if _, err := st.AdvanceJobByAgent(result.JobID, machineID, token,
			deploy.FinishWork, now.Add(3*time.Second)); err != nil {
			t.Fatal(err)
		}
		if _, err := st.MarkSucceededIfVerified(result.JobID, now.Add(4*time.Second)); err != nil {
			t.Fatal(err)
		}
		st.nowFn = func() time.Time { return now.Add(5 * time.Second) }
		beforeAudit := countRows(t, st, `SELECT COUNT(*) FROM audit_log WHERE action='device-sync'`)
		if _, err := st.ApplyOperatorDeviceSync(req); err == nil ||
			!strings.Contains(err.Error(), "idempotency cache invalid") {
			t.Fatalf("mixed verification replay err=%v", err)
		}
		if countRows(t, st, `SELECT COUNT(*) FROM audit_log WHERE action='device-sync'`) != beforeAudit ||
			countRows(t, st, `SELECT COUNT(*) FROM operator_idempotency`) != 1 ||
			countRows(t, st, `SELECT COUNT(*) FROM verification_results WHERE job_id=?`, result.JobID) != 2 ||
			countRows(t, st, `SELECT COUNT(*) FROM desired_state`) != 1 ||
			countRows(t, st, `SELECT COUNT(*) FROM jobs WHERE state='succeeded'`) != 1 {
			t.Fatal("mixed verification replay changed device-sync authority")
		}
	})
}

// ⚠ 重複 executor 證據現在只能在帳本層造出；ingress 已擋住相同規則的第二次寫入。
func TestOperatorDeviceSyncReplayRejectsDuplicateSucceededVerification(t *testing.T) {
	st, machineID, now := deviceSyncFixture(t)
	preview, err := st.PreviewOperatorDeviceSync(machineID, model.DeviceSyncDefaultTimeoutSeconds)
	if err != nil {
		t.Fatal(err)
	}
	req := deviceSyncRequest(machineID, preview, "device-sync-duplicate-verification")
	result, err := st.ApplyOperatorDeviceSync(req)
	if err != nil {
		t.Fatal(err)
	}
	token, err := st.ClaimJob(result.JobID, machineID, now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	appendDeviceSyncPhaseEvent(t, st, result.JobID, machineID, token, 1, "start", now.Add(time.Second))
	if _, err := st.AdvanceJobByAgent(result.JobID, machineID, token, deploy.Start, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordVerification(result.JobID, machineID, token,
		model.DeviceSyncVerificationRuleID, model.DeviceSyncVerificationCommand, 0,
		model.DeviceSyncVerificationStdout, "", true, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`INSERT INTO verification_results
 (verification_id,job_id,machine_id,rule_id,command,exit_code,stdout_excerpt,stderr_excerpt,
  passed,verified_at,producer_kind,producer_id,evidence_role,authority,provenance_recorded,received_at)
 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,1,?)`, "duplicate-device-sync-proof", result.JobID, machineID,
		model.DeviceSyncVerificationRuleID, model.DeviceSyncVerificationCommand, 0,
		model.DeviceSyncVerificationStdout, "", true, fmtTime(now.Add(2*time.Second)),
		JobVerificationProducerExecutorAgent, machineID, JobVerificationRoleExecutor,
		JobVerificationAuthorityMachineLease, fmtTime(now)); err != nil {
		t.Fatal(err)
	}
	appendDeviceSyncPhaseEvent(t, st, result.JobID, machineID, token, 2, "finish", now.Add(3*time.Second))
	if _, err := st.AdvanceJobByAgent(result.JobID, machineID, token,
		deploy.FinishWork, now.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.MarkSucceededIfVerified(result.JobID, now.Add(4*time.Second)); err != nil {
		t.Fatal(err)
	}
	st.nowFn = func() time.Time { return now.Add(5 * time.Second) }
	beforeAudit := countRows(t, st, `SELECT COUNT(*) FROM audit_log WHERE action='device-sync'`)
	if _, err := st.ApplyOperatorDeviceSync(req); err == nil ||
		!strings.Contains(err.Error(), "idempotency cache invalid") {
		t.Fatalf("duplicate verification replay err=%v", err)
	}
	if countRows(t, st, `SELECT COUNT(*) FROM audit_log WHERE action='device-sync'`) != beforeAudit ||
		countRows(t, st, `SELECT COUNT(*) FROM operator_idempotency`) != 1 ||
		countRows(t, st, `SELECT COUNT(*) FROM verification_results WHERE job_id=?`, result.JobID) != 2 ||
		countRows(t, st, `SELECT COUNT(*) FROM desired_state`) != 1 ||
		countRows(t, st, `SELECT COUNT(*) FROM jobs WHERE state='succeeded'`) != 1 {
		t.Fatal("duplicate verification replay changed authority")
	}
}

func TestOperatorDeviceSyncReplayRejectsMissingSucceededVerificationStderr(t *testing.T) {
	st, machineID, now := deviceSyncFixture(t)
	preview, err := st.PreviewOperatorDeviceSync(machineID, model.DeviceSyncDefaultTimeoutSeconds)
	if err != nil {
		t.Fatal(err)
	}
	req := deviceSyncRequest(machineID, preview, "device-sync-null-verification-stderr")
	result, err := st.ApplyOperatorDeviceSync(req)
	if err != nil {
		t.Fatal(err)
	}
	token, err := st.ClaimJob(result.JobID, machineID, now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	appendDeviceSyncPhaseEvent(t, st, result.JobID, machineID, token, 1, "start", now.Add(time.Second))
	if _, err := st.AdvanceJobByAgent(result.JobID, machineID, token, deploy.Start, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordVerification(result.JobID, machineID, token,
		model.DeviceSyncVerificationRuleID, model.DeviceSyncVerificationCommand, 0,
		model.DeviceSyncVerificationStdout, "", true, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	appendDeviceSyncPhaseEvent(t, st, result.JobID, machineID, token, 2, "finish", now.Add(3*time.Second))
	if _, err := st.AdvanceJobByAgent(result.JobID, machineID, token,
		deploy.FinishWork, now.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.MarkSucceededIfVerified(result.JobID, now.Add(4*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`UPDATE verification_results SET stderr_excerpt=NULL WHERE job_id=?`,
		result.JobID); err != nil {
		t.Fatal(err)
	}
	st.nowFn = func() time.Time { return now.Add(5 * time.Second) }
	beforeAudit := countRows(t, st, `SELECT COUNT(*) FROM audit_log WHERE action='device-sync'`)
	if _, err := st.ApplyOperatorDeviceSync(req); err == nil ||
		!strings.Contains(err.Error(), "idempotency cache invalid") {
		t.Fatalf("missing verification stderr replay err=%v", err)
	}
	if countRows(t, st, `SELECT COUNT(*) FROM audit_log WHERE action='device-sync'`) != beforeAudit ||
		countRows(t, st, `SELECT COUNT(*) FROM operator_idempotency`) != 1 ||
		countRows(t, st, `SELECT COUNT(*) FROM verification_results WHERE job_id=? AND stderr_excerpt IS NULL`,
			result.JobID) != 1 ||
		countRows(t, st, `SELECT COUNT(*) FROM desired_state`) != 1 ||
		countRows(t, st, `SELECT COUNT(*) FROM jobs WHERE state='succeeded'`) != 1 {
		t.Fatal("missing verification stderr replay changed authority")
	}
}

func TestOperatorDeviceSyncReplayRequiresSucceededEvidenceIdentity(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate string
	}{
		{name: "verification id", mutate: `UPDATE verification_results SET verification_id='' WHERE job_id=?`},
		{name: "event id", mutate: `UPDATE job_events SET event_id='' WHERE job_id=? AND seq=1`},
	} {
		t.Run(test.name, func(t *testing.T) {
			st, machineID, now := deviceSyncFixture(t)
			preview, err := st.PreviewOperatorDeviceSync(machineID, model.DeviceSyncDefaultTimeoutSeconds)
			if err != nil {
				t.Fatal(err)
			}
			req := deviceSyncRequest(machineID, preview, "device-sync-evidence-id-"+test.name)
			result, err := st.ApplyOperatorDeviceSync(req)
			if err != nil {
				t.Fatal(err)
			}
			token, err := st.ClaimJob(result.JobID, machineID, now, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			appendDeviceSyncPhaseEvent(t, st, result.JobID, machineID, token, 1, "start", now.Add(time.Second))
			if _, err := st.AdvanceJobByAgent(result.JobID, machineID, token,
				deploy.Start, now.Add(time.Second)); err != nil {
				t.Fatal(err)
			}
			if err := st.RecordVerification(result.JobID, machineID, token,
				model.DeviceSyncVerificationRuleID, model.DeviceSyncVerificationCommand, 0,
				model.DeviceSyncVerificationStdout, "", true, now.Add(2*time.Second)); err != nil {
				t.Fatal(err)
			}
			appendDeviceSyncPhaseEvent(t, st, result.JobID, machineID, token, 2, "finish", now.Add(3*time.Second))
			if _, err := st.AdvanceJobByAgent(result.JobID, machineID, token,
				deploy.FinishWork, now.Add(3*time.Second)); err != nil {
				t.Fatal(err)
			}
			if _, err := st.MarkSucceededIfVerified(result.JobID, now.Add(4*time.Second)); err != nil {
				t.Fatal(err)
			}
			if _, err := st.DB().Exec(test.mutate, result.JobID); err != nil {
				t.Fatal(err)
			}
			st.nowFn = func() time.Time { return now.Add(5 * time.Second) }
			beforeAudit := countRows(t, st, `SELECT COUNT(*) FROM audit_log WHERE action='device-sync'`)
			if _, err := st.ApplyOperatorDeviceSync(req); err == nil ||
				!strings.Contains(err.Error(), "idempotency cache invalid") {
				t.Fatalf("invalid %s replay err=%v", test.name, err)
			}
			if countRows(t, st, `SELECT COUNT(*) FROM audit_log WHERE action='device-sync'`) != beforeAudit ||
				countRows(t, st, `SELECT COUNT(*) FROM operator_idempotency`) != 1 ||
				countRows(t, st, `SELECT COUNT(*) FROM verification_results WHERE job_id=?`, result.JobID) != 1 ||
				countRows(t, st, `SELECT COUNT(*) FROM job_events WHERE job_id=?`, result.JobID) != 2 ||
				countRows(t, st, `SELECT COUNT(*) FROM desired_state`) != 1 ||
				countRows(t, st, `SELECT COUNT(*) FROM jobs WHERE state='succeeded'`) != 1 {
				t.Fatal("invalid succeeded evidence identity replay changed authority")
			}
		})
	}
}

func TestOperatorDeviceSyncReplayRequiresCanonicalVerificationTimes(t *testing.T) {
	for _, test := range []struct {
		name   string
		column string
		value  string
	}{
		{name: "invalid verified time", column: "verified_at", value: "invalid"},
		{name: "offset verified time", column: "verified_at", value: "2026-09-14T08:00:02-04:00"},
		{name: "invalid received time", column: "received_at", value: "invalid"},
		{name: "offset received time", column: "received_at", value: "2026-09-14T08:00:00-04:00"},
		{name: "received before creation", column: "received_at", value: "2026-09-14T11:59:59Z"},
		{name: "received after terminal", column: "received_at", value: "2026-09-14T12:00:05Z"},
	} {
		t.Run(test.name, func(t *testing.T) {
			st, machineID, now := deviceSyncFixture(t)
			preview, err := st.PreviewOperatorDeviceSync(machineID, model.DeviceSyncDefaultTimeoutSeconds)
			if err != nil {
				t.Fatal(err)
			}
			req := deviceSyncRequest(machineID, preview, "device-sync-verification-time-"+test.name)
			result, err := st.ApplyOperatorDeviceSync(req)
			if err != nil {
				t.Fatal(err)
			}
			token, err := st.ClaimJob(result.JobID, machineID, now, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			appendDeviceSyncPhaseEvent(t, st, result.JobID, machineID, token, 1, "start", now.Add(time.Second))
			if _, err := st.AdvanceJobByAgent(result.JobID, machineID, token,
				deploy.Start, now.Add(time.Second)); err != nil {
				t.Fatal(err)
			}
			if err := st.RecordVerification(result.JobID, machineID, token,
				model.DeviceSyncVerificationRuleID, model.DeviceSyncVerificationCommand, 0,
				model.DeviceSyncVerificationStdout, "", true, now.Add(2*time.Second)); err != nil {
				t.Fatal(err)
			}
			appendDeviceSyncPhaseEvent(t, st, result.JobID, machineID, token, 2, "finish", now.Add(3*time.Second))
			if _, err := st.AdvanceJobByAgent(result.JobID, machineID, token,
				deploy.FinishWork, now.Add(3*time.Second)); err != nil {
				t.Fatal(err)
			}
			if _, err := st.MarkSucceededIfVerified(result.JobID, now.Add(4*time.Second)); err != nil {
				t.Fatal(err)
			}
			query := fmt.Sprintf("UPDATE verification_results SET %s=? WHERE job_id=?", test.column)
			if _, err := st.DB().Exec(query, test.value, result.JobID); err != nil {
				t.Fatal(err)
			}
			st.nowFn = func() time.Time { return now.Add(6 * time.Second) }
			beforeAudit := countRows(t, st, `SELECT COUNT(*) FROM audit_log WHERE action='device-sync'`)
			if _, err := st.ApplyOperatorDeviceSync(req); err == nil ||
				!strings.Contains(err.Error(), "idempotency cache invalid") {
				t.Fatalf("invalid verification time replay err=%v", err)
			}
			if countRows(t, st, `SELECT COUNT(*) FROM audit_log WHERE action='device-sync'`) != beforeAudit ||
				countRows(t, st, `SELECT COUNT(*) FROM operator_idempotency`) != 1 ||
				countRows(t, st, `SELECT COUNT(*) FROM verification_results WHERE job_id=?`, result.JobID) != 1 ||
				countRows(t, st, `SELECT COUNT(*) FROM desired_state`) != 1 ||
				countRows(t, st, `SELECT COUNT(*) FROM jobs WHERE state='succeeded'`) != 1 {
				t.Fatal("invalid verification time replay changed device-sync authority")
			}
		})
	}
}

func TestOperatorDeviceSyncReplayRejectsCausallyReversedSucceededReceiptTimes(t *testing.T) {
	for _, test := range []struct {
		name                  string
		start, verify, finish time.Duration
	}{
		{name: "finish before start", start: 2 * time.Second, verify: 3 * time.Second, finish: time.Second},
		{name: "verification before start", start: 2 * time.Second, verify: time.Second, finish: 3 * time.Second},
		{name: "verification after finish", start: time.Second, verify: 3 * time.Second, finish: 2 * time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			st, machineID, now := deviceSyncFixture(t)
			preview, err := st.PreviewOperatorDeviceSync(machineID, model.DeviceSyncDefaultTimeoutSeconds)
			if err != nil {
				t.Fatal(err)
			}
			req := deviceSyncRequest(machineID, preview, "device-sync-reversed-receipt-time-"+test.name)
			result, err := st.ApplyOperatorDeviceSync(req)
			if err != nil {
				t.Fatal(err)
			}
			token, err := st.ClaimJob(result.JobID, machineID, now, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			appendDeviceSyncPhaseEvent(t, st, result.JobID, machineID, token, 1, "start", now.Add(time.Second))
			if _, err := st.AdvanceJobByAgent(result.JobID, machineID, token,
				deploy.Start, now.Add(time.Second)); err != nil {
				t.Fatal(err)
			}
			st.nowFn = func() time.Time { return now.Add(2 * time.Second) }
			if err := st.RecordVerification(result.JobID, machineID, token,
				model.DeviceSyncVerificationRuleID, model.DeviceSyncVerificationCommand, 0,
				model.DeviceSyncVerificationStdout, "", true, now.Add(2*time.Second)); err != nil {
				t.Fatal(err)
			}
			appendDeviceSyncPhaseEvent(t, st, result.JobID, machineID, token, 2, "finish", now.Add(3*time.Second))
			if _, err := st.AdvanceJobByAgent(result.JobID, machineID, token,
				deploy.FinishWork, now.Add(3*time.Second)); err != nil {
				t.Fatal(err)
			}
			if _, err := st.MarkSucceededIfVerified(result.JobID, now.Add(4*time.Second)); err != nil {
				t.Fatal(err)
			}
			if _, err := st.DB().Exec(`UPDATE job_events
				SET received_at=CASE seq WHEN 1 THEN ? WHEN 2 THEN ? END WHERE job_id=?`,
				fmtTime(now.Add(test.start)), fmtTime(now.Add(test.finish)), result.JobID); err != nil {
				t.Fatal(err)
			}
			if _, err := st.DB().Exec(`UPDATE verification_results SET received_at=? WHERE job_id=?`,
				fmtTime(now.Add(test.verify)), result.JobID); err != nil {
				t.Fatal(err)
			}
			st.nowFn = func() time.Time { return now.Add(6 * time.Second) }
			beforeAudit := countRows(t, st, `SELECT COUNT(*) FROM audit_log WHERE action='device-sync'`)
			if _, err := st.ApplyOperatorDeviceSync(req); err == nil ||
				!strings.Contains(err.Error(), "idempotency cache invalid") {
				t.Fatalf("causally reversed receipt times replay err=%v", err)
			}
			if countRows(t, st, `SELECT COUNT(*) FROM audit_log WHERE action='device-sync'`) != beforeAudit ||
				countRows(t, st, `SELECT COUNT(*) FROM operator_idempotency`) != 1 ||
				countRows(t, st, `SELECT COUNT(*) FROM desired_state`) != 1 ||
				countRows(t, st, `SELECT COUNT(*) FROM jobs`) != 1 {
				t.Fatal("causally reversed receipt times replay changed device-sync authority")
			}
		})
	}
}

func TestOperatorDeviceSyncReplayAllowsOrderedSucceededReceiptTimes(t *testing.T) {
	for _, test := range []struct {
		name                  string
		start, verify, finish time.Duration
	}{
		{name: "strictly increasing", start: time.Second, verify: 2 * time.Second, finish: 3 * time.Second},
		{name: "all equal", start: 2 * time.Second, verify: 2 * time.Second, finish: 2 * time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			st, machineID, now := deviceSyncFixture(t)
			preview, err := st.PreviewOperatorDeviceSync(machineID, model.DeviceSyncDefaultTimeoutSeconds)
			if err != nil {
				t.Fatal(err)
			}
			req := deviceSyncRequest(machineID, preview, "device-sync-ordered-receipt-time-"+test.name)
			result, err := st.ApplyOperatorDeviceSync(req)
			if err != nil {
				t.Fatal(err)
			}
			token, err := st.ClaimJob(result.JobID, machineID, now, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			appendDeviceSyncPhaseEvent(t, st, result.JobID, machineID, token, 1, "start", now.Add(time.Second))
			if _, err := st.AdvanceJobByAgent(result.JobID, machineID, token,
				deploy.Start, now.Add(time.Second)); err != nil {
				t.Fatal(err)
			}
			st.nowFn = func() time.Time { return now.Add(2 * time.Second) }
			if err := st.RecordVerification(result.JobID, machineID, token,
				model.DeviceSyncVerificationRuleID, model.DeviceSyncVerificationCommand, 0,
				model.DeviceSyncVerificationStdout, "", true, now.Add(2*time.Second)); err != nil {
				t.Fatal(err)
			}
			appendDeviceSyncPhaseEvent(t, st, result.JobID, machineID, token, 2, "finish", now.Add(3*time.Second))
			if _, err := st.AdvanceJobByAgent(result.JobID, machineID, token,
				deploy.FinishWork, now.Add(3*time.Second)); err != nil {
				t.Fatal(err)
			}
			if _, err := st.MarkSucceededIfVerified(result.JobID, now.Add(4*time.Second)); err != nil {
				t.Fatal(err)
			}
			if _, err := st.DB().Exec(`UPDATE job_events
				SET received_at=CASE seq WHEN 1 THEN ? WHEN 2 THEN ? END WHERE job_id=?`,
				fmtTime(now.Add(test.start)), fmtTime(now.Add(test.finish)), result.JobID); err != nil {
				t.Fatal(err)
			}
			if _, err := st.DB().Exec(`UPDATE verification_results SET received_at=? WHERE job_id=?`,
				fmtTime(now.Add(test.verify)), result.JobID); err != nil {
				t.Fatal(err)
			}
			st.nowFn = func() time.Time { return now.Add(6 * time.Second) }
			replay, err := st.ApplyOperatorDeviceSync(req)
			if err != nil || !replay.Replayed || !replay.Audited || replay.JobID != result.JobID ||
				replay.DesiredID != result.DesiredID {
				t.Fatalf("ordered receipt times replay=%+v err=%v", replay, err)
			}
			if countRows(t, st, `SELECT COUNT(*) FROM audit_log WHERE action='device-sync'`) != 2 ||
				countRows(t, st, `SELECT COUNT(*) FROM operator_idempotency`) != 1 ||
				countRows(t, st, `SELECT COUNT(*) FROM desired_state`) != 1 ||
				countRows(t, st, `SELECT COUNT(*) FROM jobs`) != 1 {
				t.Fatal("ordered receipt times replay changed device-sync authority")
			}
		})
	}
}

func TestOperatorDeviceSyncReplayRequiresSucceededEventTranscript(t *testing.T) {
	st, machineID, now := deviceSyncFixture(t)
	preview, err := st.PreviewOperatorDeviceSync(machineID, model.DeviceSyncDefaultTimeoutSeconds)
	if err != nil {
		t.Fatal(err)
	}
	req := deviceSyncRequest(machineID, preview, "device-sync-success-without-events")
	result, err := st.ApplyOperatorDeviceSync(req)
	if err != nil {
		t.Fatal(err)
	}
	token, err := st.ClaimJob(result.JobID, machineID, now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AdvanceJobByAgent(result.JobID, machineID, token, deploy.Start, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordVerification(result.JobID, machineID, token,
		model.DeviceSyncVerificationRuleID, model.DeviceSyncVerificationCommand, 0,
		model.DeviceSyncVerificationStdout, "", true, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AdvanceJobByAgent(result.JobID, machineID, token,
		deploy.FinishWork, now.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.MarkSucceededIfVerified(result.JobID, now.Add(4*time.Second)); err != nil {
		t.Fatal(err)
	}
	st.nowFn = func() time.Time { return now.Add(5 * time.Second) }
	beforeAudit := countRows(t, st, `SELECT COUNT(*) FROM audit_log WHERE action='device-sync'`)
	if _, err := st.ApplyOperatorDeviceSync(req); err == nil ||
		!strings.Contains(err.Error(), "idempotency cache invalid") {
		t.Fatalf("success without events replay err=%v", err)
	}
	if countRows(t, st, `SELECT COUNT(*) FROM audit_log WHERE action='device-sync'`) != beforeAudit ||
		countRows(t, st, `SELECT COUNT(*) FROM operator_idempotency`) != 1 ||
		countRows(t, st, `SELECT COUNT(*) FROM job_events WHERE job_id=?`, result.JobID) != 0 ||
		countRows(t, st, `SELECT COUNT(*) FROM verification_results WHERE job_id=?`, result.JobID) != 1 ||
		countRows(t, st, `SELECT COUNT(*) FROM desired_state`) != 1 ||
		countRows(t, st, `SELECT COUNT(*) FROM jobs WHERE state='succeeded'`) != 1 {
		t.Fatal("success without events replay changed device-sync authority")
	}
}

func TestOperatorDeviceSyncReplayRejectsTamperedSucceededEventTranscript(t *testing.T) {
	for _, test := range []struct {
		name   string
		tamper func(*testing.T, *Store, OperatorDeviceSyncResult, time.Time)
		rows   int
	}{
		{name: "wrong phase", rows: 2, tamper: func(t *testing.T, st *Store, result OperatorDeviceSyncResult, _ time.Time) {
			if _, err := st.DB().Exec(`UPDATE job_events SET phase='progress' WHERE job_id=? AND seq=1`, result.JobID); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "noncanonical payload", rows: 2, tamper: func(t *testing.T, st *Store, result OperatorDeviceSyncResult, _ time.Time) {
			if _, err := st.DB().Exec(`UPDATE job_events SET payload='{ }' WHERE job_id=? AND seq=2`, result.JobID); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "wrong producer", rows: 2, tamper: func(t *testing.T, st *Store, result OperatorDeviceSyncResult, _ time.Time) {
			if _, err := st.DB().Exec(`UPDATE job_events SET producer_id='tampered' WHERE job_id=? AND seq=1`, result.JobID); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "invalid occurred time", rows: 2, tamper: func(t *testing.T, st *Store, result OperatorDeviceSyncResult, _ time.Time) {
			if _, err := st.DB().Exec(`UPDATE job_events SET occurred_at='invalid' WHERE job_id=? AND seq=1`, result.JobID); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "received after terminal", rows: 2, tamper: func(t *testing.T, st *Store, result OperatorDeviceSyncResult, now time.Time) {
			if _, err := st.DB().Exec(`UPDATE job_events SET received_at=? WHERE job_id=? AND seq=2`,
				fmtTime(now.Add(5*time.Second)), result.JobID); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "extra event", rows: 3, tamper: func(t *testing.T, st *Store, result OperatorDeviceSyncResult, now time.Time) {
			if _, err := st.DB().Exec(`INSERT INTO job_events
				(event_id,job_id,seq,phase,occurred_at,received_at,payload,
				 producer_kind,producer_id,evidence_role,authority,provenance_recorded)
				VALUES (?,?,?,?,?,?,?,?,?,?,?,1)`, "extra-"+result.JobID, result.JobID, 3, "extra",
				fmtTime(now.Add(3*time.Second)), fmtTime(now.Add(3*time.Second)), `{}`,
				JobEventProducerExecutorAgent, result.MachineID, JobEventRoleExecutor,
				JobEventAuthorityMachineLease); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			st, machineID, now := deviceSyncFixture(t)
			preview, err := st.PreviewOperatorDeviceSync(machineID, model.DeviceSyncDefaultTimeoutSeconds)
			if err != nil {
				t.Fatal(err)
			}
			req := deviceSyncRequest(machineID, preview, "device-sync-event-transcript-"+test.name)
			result, err := st.ApplyOperatorDeviceSync(req)
			if err != nil {
				t.Fatal(err)
			}
			token, err := st.ClaimJob(result.JobID, machineID, now, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			appendDeviceSyncPhaseEvent(t, st, result.JobID, machineID, token, 1, "start", now.Add(time.Second))
			if _, err := st.AdvanceJobByAgent(result.JobID, machineID, token,
				deploy.Start, now.Add(time.Second)); err != nil {
				t.Fatal(err)
			}
			if err := st.RecordVerification(result.JobID, machineID, token,
				model.DeviceSyncVerificationRuleID, model.DeviceSyncVerificationCommand, 0,
				model.DeviceSyncVerificationStdout, "", true, now.Add(2*time.Second)); err != nil {
				t.Fatal(err)
			}
			appendDeviceSyncPhaseEvent(t, st, result.JobID, machineID, token, 2, "finish", now.Add(3*time.Second))
			if _, err := st.AdvanceJobByAgent(result.JobID, machineID, token,
				deploy.FinishWork, now.Add(3*time.Second)); err != nil {
				t.Fatal(err)
			}
			if _, err := st.MarkSucceededIfVerified(result.JobID, now.Add(4*time.Second)); err != nil {
				t.Fatal(err)
			}
			test.tamper(t, st, result, now)
			st.nowFn = func() time.Time { return now.Add(6 * time.Second) }
			beforeAudit := countRows(t, st, `SELECT COUNT(*) FROM audit_log WHERE action='device-sync'`)
			if _, err := st.ApplyOperatorDeviceSync(req); err == nil ||
				!strings.Contains(err.Error(), "idempotency cache invalid") {
				t.Fatalf("tampered success transcript replay err=%v", err)
			}
			if countRows(t, st, `SELECT COUNT(*) FROM audit_log WHERE action='device-sync'`) != beforeAudit ||
				countRows(t, st, `SELECT COUNT(*) FROM operator_idempotency`) != 1 ||
				countRows(t, st, `SELECT COUNT(*) FROM job_events WHERE job_id=?`, result.JobID) != test.rows ||
				countRows(t, st, `SELECT COUNT(*) FROM verification_results WHERE job_id=?`, result.JobID) != 1 ||
				countRows(t, st, `SELECT COUNT(*) FROM desired_state`) != 1 ||
				countRows(t, st, `SELECT COUNT(*) FROM jobs WHERE state='succeeded'`) != 1 {
				t.Fatal("tampered success transcript replay changed device-sync authority")
			}
		})
	}
}

func TestOperatorDeviceSyncReplayRejectsContradictoryJobLifecycle(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate string
	}{
		{name: "unclaimed job has lease", mutate: `UPDATE jobs SET lease_token='forged',lease_expires_at='2026-09-14T12:01:00Z' WHERE job_id=?`},
		{name: "unclaimed job has terminal", mutate: `UPDATE jobs SET terminal_at='2026-09-14T12:01:00Z' WHERE job_id=?`},
		{name: "claimed job lacks lease", mutate: `UPDATE jobs SET state='claimed' WHERE job_id=?`},
		{name: "claimed job has terminal", mutate: `UPDATE jobs SET state='claimed',lease_token='forged',lease_expires_at='2026-09-14T12:01:00Z',terminal_at='2026-09-14T12:01:00Z' WHERE job_id=?`},
		{name: "terminal job lacks terminal", mutate: `UPDATE jobs SET state='failed' WHERE job_id=?`},
		{name: "terminal job retains lease", mutate: `UPDATE jobs SET state='failed',terminal_at='2026-09-14T12:01:00Z',lease_token='forged',lease_expires_at='2026-09-14T12:01:00Z' WHERE job_id=?`},
	} {
		t.Run(test.name, func(t *testing.T) {
			st, machineID, _ := deviceSyncFixture(t)
			preview, err := st.PreviewOperatorDeviceSync(machineID, model.DeviceSyncDefaultTimeoutSeconds)
			if err != nil {
				t.Fatal(err)
			}
			req := deviceSyncRequest(machineID, preview, "device-sync-lifecycle-"+test.name)
			result, err := st.ApplyOperatorDeviceSync(req)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := st.DB().Exec(test.mutate, result.JobID); err != nil {
				t.Fatal(err)
			}
			beforeAudit := countRows(t, st, `SELECT COUNT(*) FROM audit_log WHERE action='device-sync'`)
			if _, err := st.ApplyOperatorDeviceSync(req); err == nil ||
				!strings.Contains(err.Error(), "idempotency cache invalid") {
				t.Fatalf("contradictory job lifecycle replay err=%v", err)
			}
			if countRows(t, st, `SELECT COUNT(*) FROM audit_log WHERE action='device-sync'`) != beforeAudit ||
				countRows(t, st, `SELECT COUNT(*) FROM operator_idempotency`) != 1 ||
				countRows(t, st, `SELECT COUNT(*) FROM desired_state`) != 1 ||
				countRows(t, st, `SELECT COUNT(*) FROM jobs`) != 1 {
				t.Fatal("contradictory lifecycle replay changed device-sync authority")
			}
		})
	}
}

func TestOperatorDeviceSyncReplayRejectsNotStartedJobWithLeaseBoundEvidence(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*testing.T, *Store, OperatorDeviceSyncResult, string, time.Time)
	}{
		{name: "job events only", mutate: func(t *testing.T, st *Store, result OperatorDeviceSyncResult, machineID string, now time.Time) {
			for seq, phase := range []string{"start", "finish"} {
				at := fmtTime(now.Add(time.Duration(seq+1) * time.Second))
				if _, err := st.DB().Exec(`INSERT INTO job_events
					(event_id,job_id,seq,phase,occurred_at,received_at,payload,
					 producer_kind,producer_id,evidence_role,authority,provenance_recorded)
					VALUES (?,?,?,?,?,?,?,?,?,?,?,1)`, fmt.Sprintf("forged-%s-%d", result.JobID, seq+1),
					result.JobID, seq+1, phase, at, at, `{}`, JobEventProducerExecutorAgent, machineID,
					JobEventRoleExecutor, JobEventAuthorityMachineLease); err != nil {
					t.Fatal(err)
				}
			}
		}},
		{name: "executor verification only", mutate: func(t *testing.T, st *Store, result OperatorDeviceSyncResult, machineID string, now time.Time) {
			if _, err := st.DB().Exec(`INSERT INTO verification_results
				(verification_id,job_id,machine_id,rule_id,command,exit_code,
				 stdout_excerpt,stderr_excerpt,passed,verified_at,producer_kind,
				 producer_id,evidence_role,authority,provenance_recorded,received_at)
				VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,1,?)`, "forged-verification-"+result.JobID,
				result.JobID, machineID, model.DeviceSyncVerificationRuleID, model.DeviceSyncVerificationCommand,
				0, model.DeviceSyncVerificationStdout, "", true, fmtTime(now.Add(2*time.Second)),
				JobVerificationProducerExecutorAgent, machineID, JobVerificationRoleExecutor,
				JobVerificationAuthorityMachineLease, fmtTime(now.Add(2*time.Second))); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			st, machineID, now := deviceSyncFixture(t)
			preview, err := st.PreviewOperatorDeviceSync(machineID, model.DeviceSyncDefaultTimeoutSeconds)
			if err != nil {
				t.Fatal(err)
			}
			req := deviceSyncRequest(machineID, preview, "device-sync-not-started-with-lease-evidence-"+test.name)
			result, err := st.ApplyOperatorDeviceSync(req)
			if err != nil {
				t.Fatal(err)
			}
			test.mutate(t, st, result, machineID, now)
			if countRows(t, st, `SELECT COUNT(*) FROM jobs WHERE job_id=? AND state='not_started'
				AND lease_token IS NULL AND lease_expires_at IS NULL AND terminal_at IS NULL`, result.JobID) != 1 {
				t.Fatal("corruption fixture did not preserve the not_started lifecycle")
			}
			beforeAudit := countRows(t, st, `SELECT COUNT(*) FROM audit_log WHERE action='device-sync'`)
			beforeIdempotency := countRows(t, st, `SELECT COUNT(*) FROM operator_idempotency`)
			beforeDesired := countRows(t, st, `SELECT COUNT(*) FROM desired_state`)
			beforeJobs := countRows(t, st, `SELECT COUNT(*) FROM jobs`)
			replay, err := st.ApplyOperatorDeviceSync(req)
			if err == nil || !strings.Contains(err.Error(), "idempotency cache invalid") {
				t.Fatalf("not_started job with lease-bound evidence replay=%+v err=%v", replay, err)
			}
			if countRows(t, st, `SELECT COUNT(*) FROM audit_log WHERE action='device-sync'`) != beforeAudit ||
				countRows(t, st, `SELECT COUNT(*) FROM operator_idempotency`) != beforeIdempotency ||
				countRows(t, st, `SELECT COUNT(*) FROM desired_state`) != beforeDesired ||
				countRows(t, st, `SELECT COUNT(*) FROM jobs`) != beforeJobs {
				t.Fatal("not_started evidence replay changed device-sync authority")
			}
		})
	}
}

func TestOperatorDeviceSyncReplayAllowsNotStartedJobWithIndependentVerification(t *testing.T) {
	st, machineID, now := deviceSyncFixture(t)
	preview, err := st.PreviewOperatorDeviceSync(machineID, model.DeviceSyncDefaultTimeoutSeconds)
	if err != nil {
		t.Fatal(err)
	}
	req := deviceSyncRequest(machineID, preview, "device-sync-not-started-with-independent-verification")
	result, err := st.ApplyOperatorDeviceSync(req)
	if err != nil {
		t.Fatal(err)
	}
	verifier, _, err := st.RegisterVerifier(VerifierKindExternalJobRunner,
		"device-sync-independent-verifier", "external-runner", "")
	if err != nil {
		t.Fatal(err)
	}
	verification := independentRequest(verifier.VerifierID, result.JobID)
	verification.VerifiedAt = now
	if err := st.RecordIndependentVerification(verification); err != nil {
		t.Fatal(err)
	}
	replay, err := st.ApplyOperatorDeviceSync(req)
	if err != nil || !replay.Replayed || !replay.Audited || replay.JobID != result.JobID ||
		replay.DesiredID != result.DesiredID {
		t.Fatalf("independently verified not_started replay=%+v err=%v", replay, err)
	}
	if countRows(t, st, `SELECT COUNT(*) FROM verification_results WHERE job_id=? AND evidence_role=?`,
		result.JobID, JobVerificationRoleIndependent) != 1 ||
		countRows(t, st, `SELECT COUNT(*) FROM audit_log WHERE action='device-sync'`) != 2 ||
		countRows(t, st, `SELECT COUNT(*) FROM operator_idempotency`) != 1 ||
		countRows(t, st, `SELECT COUNT(*) FROM desired_state`) != 1 ||
		countRows(t, st, `SELECT COUNT(*) FROM jobs`) != 1 {
		t.Fatal("independently verified replay changed device-sync authority")
	}
}

func TestOperatorDeviceSyncReplayAllowsClaimedJobLifecycle(t *testing.T) {
	st, machineID, now := deviceSyncFixture(t)
	preview, err := st.PreviewOperatorDeviceSync(machineID, model.DeviceSyncDefaultTimeoutSeconds)
	if err != nil {
		t.Fatal(err)
	}
	req := deviceSyncRequest(machineID, preview, "device-sync-claimed-lifecycle")
	result, err := st.ApplyOperatorDeviceSync(req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ClaimJob(result.JobID, machineID, now.Add(time.Second), time.Minute); err != nil {
		t.Fatal(err)
	}
	st.nowFn = func() time.Time { return now.Add(2 * time.Second) }
	replay, err := st.ApplyOperatorDeviceSync(req)
	if err != nil || !replay.Replayed || !replay.Audited || replay.JobID != result.JobID ||
		replay.DesiredID != result.DesiredID {
		t.Fatalf("claimed lifecycle replay=%+v err=%v", replay, err)
	}
	if countRows(t, st, `SELECT COUNT(*) FROM jobs WHERE job_id=? AND state='claimed'
		AND lease_token IS NOT NULL AND lease_expires_at IS NOT NULL AND terminal_at IS NULL`, result.JobID) != 1 ||
		countRows(t, st, `SELECT COUNT(*) FROM audit_log WHERE action='device-sync'`) != 2 ||
		countRows(t, st, `SELECT COUNT(*) FROM desired_state`) != 1 ||
		countRows(t, st, `SELECT COUNT(*) FROM jobs`) != 1 {
		t.Fatal("claimed lifecycle replay changed device-sync authority")
	}
}

func TestOperatorDeviceSyncReplayRequiresCanonicalLeaseToken(t *testing.T) {
	for _, test := range []struct {
		name  string
		token string
	}{
		{name: "invalid alphabet", token: "not+a/raw-token"},
		{name: "short token", token: "AA"},
		{name: "padded token", token: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="},
	} {
		t.Run(test.name, func(t *testing.T) {
			st, machineID, now := deviceSyncFixture(t)
			preview, err := st.PreviewOperatorDeviceSync(machineID, model.DeviceSyncDefaultTimeoutSeconds)
			if err != nil {
				t.Fatal(err)
			}
			req := deviceSyncRequest(machineID, preview, "device-sync-canonical-lease-token-"+test.name)
			result, err := st.ApplyOperatorDeviceSync(req)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := st.ClaimJob(result.JobID, machineID, now.Add(time.Second), time.Minute); err != nil {
				t.Fatal(err)
			}
			if _, err := st.DB().Exec(`UPDATE jobs SET lease_token=? WHERE job_id=?`, test.token, result.JobID); err != nil {
				t.Fatal(err)
			}
			beforeAudit := countRows(t, st, `SELECT COUNT(*) FROM audit_log WHERE action='device-sync'`)
			if _, err := st.ApplyOperatorDeviceSync(req); err == nil ||
				!strings.Contains(err.Error(), "idempotency cache invalid") {
				t.Fatalf("noncanonical lease token replay err=%v", err)
			}
			if countRows(t, st, `SELECT COUNT(*) FROM audit_log WHERE action='device-sync'`) != beforeAudit ||
				countRows(t, st, `SELECT COUNT(*) FROM operator_idempotency`) != 1 ||
				countRows(t, st, `SELECT COUNT(*) FROM desired_state`) != 1 ||
				countRows(t, st, `SELECT COUNT(*) FROM jobs`) != 1 {
				t.Fatal("noncanonical lease token replay changed device-sync authority")
			}
		})
	}
}

func TestOperatorDeviceSyncReplayRequiresCanonicalLifecycleTimes(t *testing.T) {
	for _, test := range []struct {
		name    string
		claimed bool
		mutate  string
	}{
		{
			name: "invalid lease expiry", claimed: true,
			mutate: `UPDATE jobs SET lease_expires_at='invalid' WHERE job_id=?`,
		},
		{
			name: "offset lease expiry", claimed: true,
			mutate: `UPDATE jobs SET lease_expires_at='2026-09-14T08:01:00-04:00' WHERE job_id=?`,
		},
		{name: "invalid terminal", mutate: `UPDATE jobs SET terminal_at='invalid' WHERE job_id=?`},
		{name: "offset terminal", mutate: `UPDATE jobs SET terminal_at='2026-09-14T08:01:00-04:00' WHERE job_id=?`},
	} {
		t.Run(test.name, func(t *testing.T) {
			st, machineID, now := deviceSyncFixture(t)
			preview, err := st.PreviewOperatorDeviceSync(machineID, model.DeviceSyncDefaultTimeoutSeconds)
			if err != nil {
				t.Fatal(err)
			}
			req := deviceSyncRequest(machineID, preview, "device-sync-canonical-lifecycle-"+test.name)
			result, err := st.ApplyOperatorDeviceSync(req)
			if err != nil {
				t.Fatal(err)
			}
			if test.claimed {
				if _, err := st.ClaimJob(result.JobID, machineID, now.Add(time.Second), time.Minute); err != nil {
					t.Fatal(err)
				}
			} else if err := st.FailJob(result.JobID, false, now.Add(time.Second)); err != nil {
				t.Fatal(err)
			}
			if _, err := st.DB().Exec(test.mutate, result.JobID); err != nil {
				t.Fatal(err)
			}
			beforeAudit := countRows(t, st, `SELECT COUNT(*) FROM audit_log WHERE action='device-sync'`)
			if _, err := st.ApplyOperatorDeviceSync(req); err == nil ||
				!strings.Contains(err.Error(), "idempotency cache invalid") {
				t.Fatalf("noncanonical lifecycle replay err=%v", err)
			}
			if countRows(t, st, `SELECT COUNT(*) FROM audit_log WHERE action='device-sync'`) != beforeAudit ||
				countRows(t, st, `SELECT COUNT(*) FROM operator_idempotency`) != 1 ||
				countRows(t, st, `SELECT COUNT(*) FROM desired_state`) != 1 ||
				countRows(t, st, `SELECT COUNT(*) FROM jobs`) != 1 {
				t.Fatal("noncanonical lifecycle replay changed device-sync authority")
			}
		})
	}
}

func TestOperatorDeviceSyncReplayRequiresCausalLifecycleTimes(t *testing.T) {
	for _, test := range []struct {
		name          string
		claimed       bool
		lifecycleTime time.Duration
	}{
		{name: "lease expiry before creation", claimed: true, lifecycleTime: -time.Second},
		{name: "lease expiry at creation", claimed: true},
		{name: "terminal before creation", lifecycleTime: -time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			st, machineID, now := deviceSyncFixture(t)
			preview, err := st.PreviewOperatorDeviceSync(machineID, model.DeviceSyncDefaultTimeoutSeconds)
			if err != nil {
				t.Fatal(err)
			}
			req := deviceSyncRequest(machineID, preview, "device-sync-causal-lifecycle-"+test.name)
			result, err := st.ApplyOperatorDeviceSync(req)
			if err != nil {
				t.Fatal(err)
			}
			if test.claimed {
				if _, err := st.ClaimJob(result.JobID, machineID, now.Add(time.Second), time.Minute); err != nil {
					t.Fatal(err)
				}
				if _, err := st.DB().Exec(`UPDATE jobs SET lease_expires_at=? WHERE job_id=?`,
					fmtTime(result.CreatedAt.Add(test.lifecycleTime)), result.JobID); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := st.FailJob(result.JobID, false, now.Add(time.Second)); err != nil {
					t.Fatal(err)
				}
				if _, err := st.DB().Exec(`UPDATE jobs SET terminal_at=? WHERE job_id=?`,
					fmtTime(result.CreatedAt.Add(test.lifecycleTime)), result.JobID); err != nil {
					t.Fatal(err)
				}
			}
			beforeAudit := countRows(t, st, `SELECT COUNT(*) FROM audit_log WHERE action='device-sync'`)
			if _, err := st.ApplyOperatorDeviceSync(req); err == nil ||
				!strings.Contains(err.Error(), "idempotency cache invalid") {
				t.Fatalf("noncausal lifecycle replay err=%v", err)
			}
			if countRows(t, st, `SELECT COUNT(*) FROM audit_log WHERE action='device-sync'`) != beforeAudit ||
				countRows(t, st, `SELECT COUNT(*) FROM operator_idempotency`) != 1 ||
				countRows(t, st, `SELECT COUNT(*) FROM desired_state`) != 1 ||
				countRows(t, st, `SELECT COUNT(*) FROM jobs`) != 1 {
				t.Fatal("noncausal lifecycle replay changed device-sync authority")
			}
		})
	}

	t.Run("terminal at creation", func(t *testing.T) {
		st, machineID, now := deviceSyncFixture(t)
		preview, err := st.PreviewOperatorDeviceSync(machineID, model.DeviceSyncDefaultTimeoutSeconds)
		if err != nil {
			t.Fatal(err)
		}
		req := deviceSyncRequest(machineID, preview, "device-sync-causal-terminal-boundary")
		result, err := st.ApplyOperatorDeviceSync(req)
		if err != nil {
			t.Fatal(err)
		}
		if err := st.FailJob(result.JobID, false, now); err != nil {
			t.Fatal(err)
		}
		replay, err := st.ApplyOperatorDeviceSync(req)
		if err != nil || !replay.Replayed || !replay.Audited || replay.JobID != result.JobID ||
			replay.DesiredID != result.DesiredID {
			t.Fatalf("same-second terminal replay=%+v err=%v", replay, err)
		}
		if countRows(t, st, `SELECT COUNT(*) FROM audit_log WHERE action='device-sync'`) != 2 ||
			countRows(t, st, `SELECT COUNT(*) FROM operator_idempotency`) != 1 ||
			countRows(t, st, `SELECT COUNT(*) FROM desired_state`) != 1 ||
			countRows(t, st, `SELECT COUNT(*) FROM jobs`) != 1 {
			t.Fatal("same-second terminal replay changed device-sync authority")
		}
	})
}

func TestOperatorDeviceSyncReplayRejectsTamperedDesiredStateAuthority(t *testing.T) {
	for _, test := range []struct {
		name   string
		column string
		value  any
	}{
		{name: "scope type", column: "scope_type", value: "channel"},
		{name: "scope id", column: "scope_id", value: "tampered-machine"},
		{name: "resource kind", column: "resource_kind", value: "tampered"},
		{name: "resource id", column: "resource_id", value: "tampered"},
		{name: "revision", column: "revision", value: 2},
		{name: "created at", column: "created_at", value: "2026-09-14T08:00:00-04:00"},
	} {
		t.Run(test.name, func(t *testing.T) {
			st, machineID, _ := deviceSyncFixture(t)
			preview, err := st.PreviewOperatorDeviceSync(machineID, model.DeviceSyncDefaultTimeoutSeconds)
			if err != nil {
				t.Fatal(err)
			}
			req := deviceSyncRequest(machineID, preview, "device-sync-desired-tamper-"+test.name)
			result, err := st.ApplyOperatorDeviceSync(req)
			if err != nil {
				t.Fatal(err)
			}
			query := fmt.Sprintf("UPDATE desired_state SET %s=? WHERE desired_id=?", test.column)
			if _, err := st.DB().Exec(query, test.value, result.DesiredID); err != nil {
				t.Fatal(err)
			}
			beforeAudit := countRows(t, st, `SELECT COUNT(*) FROM audit_log WHERE action='device-sync'`)
			if _, err := st.ApplyOperatorDeviceSync(req); err == nil ||
				!strings.Contains(err.Error(), "idempotency cache invalid") {
				t.Fatalf("tampered desired state replay err=%v", err)
			}
			if countRows(t, st, `SELECT COUNT(*) FROM audit_log WHERE action='device-sync'`) != beforeAudit ||
				countRows(t, st, `SELECT COUNT(*) FROM operator_idempotency`) != 1 ||
				countRows(t, st, `SELECT COUNT(*) FROM revision_counters WHERE resource_scope='device:sync'`) != 1 ||
				countRows(t, st, `SELECT COUNT(*) FROM desired_state`) != 1 ||
				countRows(t, st, `SELECT COUNT(*) FROM jobs`) != 1 {
				t.Fatal("tampered desired state replay changed authority")
			}
		})
	}
}

func TestOperatorDeviceSyncReplayRequiresExclusiveDesiredStateJob(t *testing.T) {
	st, machineID, _ := deviceSyncFixture(t)
	preview, err := st.PreviewOperatorDeviceSync(machineID, model.DeviceSyncDefaultTimeoutSeconds)
	if err != nil {
		t.Fatal(err)
	}
	req := deviceSyncRequest(machineID, preview, "device-sync-exclusive-desired-job")
	result, err := st.ApplyOperatorDeviceSync(req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateJob(machineID, result.DesiredID, result.Revision, NewJob{
		ArtifactDigest: operatorDeviceSyncSpecDigest(), ExecutionTimeout: model.DeviceSyncDefaultTimeoutSeconds,
	}); err != nil {
		t.Fatal(err)
	}
	beforeAudit := countRows(t, st, `SELECT COUNT(*) FROM audit_log WHERE action='device-sync'`)
	if _, err := st.ApplyOperatorDeviceSync(req); err == nil ||
		!strings.Contains(err.Error(), "idempotency cache invalid") {
		t.Fatalf("replay with second job for receipt desired state err=%v", err)
	}
	if countRows(t, st, `SELECT COUNT(*) FROM audit_log WHERE action='device-sync'`) != beforeAudit ||
		countRows(t, st, `SELECT COUNT(*) FROM operator_idempotency`) != 1 ||
		countRows(t, st, `SELECT COUNT(*) FROM revision_counters WHERE resource_scope='device:sync'`) != 1 ||
		countRows(t, st, `SELECT COUNT(*) FROM desired_state`) != 1 ||
		countRows(t, st, `SELECT COUNT(*) FROM jobs`) != 2 {
		t.Fatal("invalid replay changed device-sync authority")
	}
}

func TestOperatorDeviceSyncReplayRequiresStandaloneJobGraph(t *testing.T) {
	st, machineID, _ := deviceSyncFixture(t)
	preview, err := st.PreviewOperatorDeviceSync(machineID, model.DeviceSyncDefaultTimeoutSeconds)
	if err != nil {
		t.Fatal(err)
	}
	req := deviceSyncRequest(machineID, preview, "device-sync-standalone-job-graph")
	result, err := st.ApplyOperatorDeviceSync(req)
	if err != nil {
		t.Fatal(err)
	}
	desiredID, revision, err := st.CreateDesiredState("machine", machineID, "test", "dependent", `{}`, "test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateJob(machineID, desiredID, revision, NewJob{
		PrerequisiteJobIDs: []string{result.JobID},
	}); err != nil {
		t.Fatal(err)
	}
	beforeAudit := countRows(t, st, `SELECT COUNT(*) FROM audit_log WHERE action='device-sync'`)
	if _, err := st.ApplyOperatorDeviceSync(req); err == nil ||
		!strings.Contains(err.Error(), "idempotency cache invalid") {
		t.Fatalf("replay with dependent job err=%v", err)
	}
	if countRows(t, st, `SELECT COUNT(*) FROM audit_log WHERE action='device-sync'`) != beforeAudit ||
		countRows(t, st, `SELECT COUNT(*) FROM operator_idempotency`) != 1 ||
		countRows(t, st, `SELECT COUNT(*) FROM desired_state`) != 2 ||
		countRows(t, st, `SELECT COUNT(*) FROM jobs`) != 2 ||
		countRows(t, st, `SELECT COUNT(*) FROM job_dependencies WHERE prerequisite_job_id=?`, result.JobID) != 1 {
		t.Fatal("dependent job replay changed device-sync authority")
	}
}

func TestOperatorDeviceSyncReplayRequiresUniqueResourceRevision(t *testing.T) {
	st, machineID, _ := deviceSyncFixture(t)
	preview, err := st.PreviewOperatorDeviceSync(machineID, model.DeviceSyncDefaultTimeoutSeconds)
	if err != nil {
		t.Fatal(err)
	}
	req := deviceSyncRequest(machineID, preview, "device-sync-unique-resource-revision")
	result, err := st.ApplyOperatorDeviceSync(req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`DROP INDEX ` + desiredStateResourceRevisionIndex); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`INSERT INTO desired_state
		(desired_id,scope_type,scope_id,resource_kind,resource_id,revision,spec,created_at,created_by)
		SELECT ?,scope_type,scope_id,resource_kind,resource_id,revision,spec,created_at,created_by
		  FROM desired_state WHERE desired_id=?`, "shadow-"+result.DesiredID, result.DesiredID); err != nil {
		t.Fatal(err)
	}
	beforeAudit := countRows(t, st, `SELECT COUNT(*) FROM audit_log WHERE action='device-sync'`)
	if _, err := st.ApplyOperatorDeviceSync(req); err == nil ||
		!strings.Contains(err.Error(), "idempotency cache invalid") {
		t.Fatalf("replay with duplicate device-sync resource revision err=%v", err)
	}
	if countRows(t, st, `SELECT COUNT(*) FROM audit_log WHERE action='device-sync'`) != beforeAudit ||
		countRows(t, st, `SELECT COUNT(*) FROM operator_idempotency`) != 1 ||
		countRows(t, st, `SELECT COUNT(*) FROM revision_counters WHERE resource_scope='device:sync'`) != 1 ||
		countRows(t, st, `SELECT COUNT(*) FROM desired_state`) != 2 ||
		countRows(t, st, `SELECT COUNT(*) FROM jobs`) != 1 {
		t.Fatal("ambiguous resource revision replay changed device-sync authority")
	}
}

func TestOperatorDeviceSyncReplayBindsMachineLifecycleAuthority(t *testing.T) {
	t.Run("future receipt revision fails closed", func(t *testing.T) {
		st, machineID, _ := deviceSyncFixture(t)
		preview, err := st.PreviewOperatorDeviceSync(machineID, model.DeviceSyncDefaultTimeoutSeconds)
		if err != nil {
			t.Fatal(err)
		}
		req := deviceSyncRequest(machineID, preview, "device-sync-future-lifecycle")
		if _, err := st.ApplyOperatorDeviceSync(req); err != nil {
			t.Fatal(err)
		}
		var raw string
		if err := st.DB().QueryRow(`SELECT response_json FROM operator_idempotency
			WHERE idempotency_key=?`, req.IdempotencyKey).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		receipt, err := decodeOperatorDeviceSyncReceipt(raw)
		if err != nil {
			t.Fatal(err)
		}
		receipt.LifecycleRevision++
		changed, err := json.Marshal(receipt)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.DB().Exec(`UPDATE operator_idempotency SET response_json=?
			WHERE idempotency_key=?`, string(changed), req.IdempotencyKey); err != nil {
			t.Fatal(err)
		}
		if _, err := st.DB().Exec(`UPDATE audit_log SET detail=?
			WHERE action='device-sync' AND idempotency_key=?`,
			operatorDeviceSyncSuccessAuditDetail(receipt), req.IdempotencyKey); err != nil {
			t.Fatal(err)
		}
		if _, err := st.ApplyOperatorDeviceSync(req); err == nil ||
			!strings.Contains(err.Error(), "idempotency cache invalid") {
			t.Fatalf("future lifecycle replay err=%v", err)
		}
		if countRows(t, st, `SELECT COUNT(*) FROM audit_log WHERE action='device-sync'`) != 1 ||
			countRows(t, st, `SELECT COUNT(*) FROM operator_idempotency`) != 1 ||
			countRows(t, st, `SELECT COUNT(*) FROM desired_state`) != 1 ||
			countRows(t, st, `SELECT COUNT(*) FROM jobs`) != 1 {
			t.Fatal("future lifecycle replay changed authority")
		}
	})

	t.Run("later lifecycle revision preserves old receipt", func(t *testing.T) {
		st, machineID, now := deviceSyncFixture(t)
		preview, err := st.PreviewOperatorDeviceSync(machineID, model.DeviceSyncDefaultTimeoutSeconds)
		if err != nil {
			t.Fatal(err)
		}
		req := deviceSyncRequest(machineID, preview, "device-sync-later-lifecycle")
		original, err := st.ApplyOperatorDeviceSync(req)
		if err != nil {
			t.Fatal(err)
		}
		if err := st.FailJob(original.JobID, false, now.Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		if err := st.RetireMachine(machineID, now.Add(2*time.Second)); err != nil {
			t.Fatal(err)
		}
		st.nowFn = func() time.Time { return now.Add(3 * time.Second) }
		replay, err := st.ApplyOperatorDeviceSync(req)
		if err != nil || !replay.Replayed || !replay.Audited || replay.JobID != original.JobID ||
			replay.DesiredID != original.DesiredID || replay.LifecycleRevision != original.LifecycleRevision {
			t.Fatalf("replay after lifecycle advance=%+v err=%v", replay, err)
		}
		if countRows(t, st, `SELECT COUNT(*) FROM machine_registry
			WHERE machine_id=? AND lifecycle_revision=1 AND retired_at IS NOT NULL`, machineID) != 1 ||
			countRows(t, st, `SELECT COUNT(*) FROM machine_registry_lifecycle_events WHERE machine_id=?`, machineID) != 1 ||
			countRows(t, st, `SELECT COUNT(*) FROM audit_log WHERE action='device-sync'`) != 2 ||
			countRows(t, st, `SELECT COUNT(*) FROM desired_state`) != 1 ||
			countRows(t, st, `SELECT COUNT(*) FROM jobs`) != 1 {
			t.Fatal("later lifecycle replay duplicated original decision")
		}
	})
}

func TestOperatorDeviceSyncReceiptRejectsAmbiguousJSON(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(string) string
	}{
		{name: "duplicate field", mutate: func(raw string) string {
			return strings.Replace(raw, "{", `{"machine_id":"shadow",`, 1)
		}},
		{name: "unknown field", mutate: func(raw string) string {
			return strings.Replace(raw, "{", `{"unknown":true,`, 1)
		}},
		{name: "trailing document", mutate: func(raw string) string { return raw + ` {}` }},
	} {
		t.Run(test.name, func(t *testing.T) {
			st, machineID, _ := deviceSyncFixture(t)
			preview, err := st.PreviewOperatorDeviceSync(machineID, model.DeviceSyncDefaultTimeoutSeconds)
			if err != nil {
				t.Fatal(err)
			}
			req := deviceSyncRequest(machineID, preview, "device-sync-ambiguous-json-"+test.name)
			if _, err := st.ApplyOperatorDeviceSync(req); err != nil {
				t.Fatal(err)
			}
			var raw string
			if err := st.DB().QueryRow(`SELECT response_json FROM operator_idempotency
				WHERE idempotency_key=?`, req.IdempotencyKey).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			if _, err := st.DB().Exec(`UPDATE operator_idempotency SET response_json=?
				WHERE idempotency_key=?`, test.mutate(raw), req.IdempotencyKey); err != nil {
				t.Fatal(err)
			}
			if _, err := st.ApplyOperatorDeviceSync(req); err == nil ||
				!strings.Contains(err.Error(), "idempotency cache invalid") {
				t.Fatalf("ambiguous receipt replay err=%v", err)
			}
			if countRows(t, st, `SELECT COUNT(*) FROM audit_log WHERE action='device-sync'`) != 1 ||
				countRows(t, st, `SELECT COUNT(*) FROM operator_idempotency`) != 1 ||
				countRows(t, st, `SELECT COUNT(*) FROM revision_counters WHERE resource_scope='device:sync'`) != 1 ||
				countRows(t, st, `SELECT COUNT(*) FROM desired_state`) != 1 ||
				countRows(t, st, `SELECT COUNT(*) FROM jobs`) != 1 {
				t.Fatal("ambiguous receipt replay changed authority")
			}
		})
	}
}

func TestOperatorDeviceSyncReceiptRequiresCanonicalJSON(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(string) string
	}{
		{name: "leading whitespace", mutate: func(raw string) string { return " " + raw }},
		{name: "reordered field", mutate: func(raw string) string {
			withoutVersion := strings.TrimPrefix(raw, `{"schema_version":"v1",`)
			return "{" + strings.TrimSuffix(withoutVersion, "}") + `,"schema_version":"v1"}`
		}},
		{name: "equivalent string escape", mutate: func(raw string) string {
			return strings.Replace(raw, `"display_name":"sync-cnode"`, `"display_name":"sync\u002dcnode"`, 1)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			st, machineID, _ := deviceSyncFixture(t)
			preview, err := st.PreviewOperatorDeviceSync(machineID, model.DeviceSyncDefaultTimeoutSeconds)
			if err != nil {
				t.Fatal(err)
			}
			req := deviceSyncRequest(machineID, preview, "device-sync-canonical-json-"+test.name)
			if _, err := st.ApplyOperatorDeviceSync(req); err != nil {
				t.Fatal(err)
			}
			var raw string
			if err := st.DB().QueryRow(`SELECT response_json FROM operator_idempotency
				WHERE idempotency_key=?`, req.IdempotencyKey).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			changed := test.mutate(raw)
			if changed == raw {
				t.Fatal("test did not change receipt JSON")
			}
			if _, err := st.DB().Exec(`UPDATE operator_idempotency SET response_json=?
				WHERE idempotency_key=?`, changed, req.IdempotencyKey); err != nil {
				t.Fatal(err)
			}
			beforeAudit := countRows(t, st, `SELECT COUNT(*) FROM audit_log WHERE action='device-sync'`)
			if _, err := st.ApplyOperatorDeviceSync(req); err == nil ||
				!strings.Contains(err.Error(), "idempotency cache invalid") {
				t.Fatalf("noncanonical receipt replay err=%v", err)
			}
			if countRows(t, st, `SELECT COUNT(*) FROM audit_log WHERE action='device-sync'`) != beforeAudit ||
				countRows(t, st, `SELECT COUNT(*) FROM operator_idempotency`) != 1 ||
				countRows(t, st, `SELECT COUNT(*) FROM revision_counters WHERE resource_scope='device:sync'`) != 1 ||
				countRows(t, st, `SELECT COUNT(*) FROM desired_state`) != 1 ||
				countRows(t, st, `SELECT COUNT(*) FROM jobs`) != 1 {
				t.Fatal("noncanonical receipt replay changed authority")
			}
		})
	}
}

func TestOperatorDeviceSyncPreviewDigestDistinguishesConfirmedFacts(t *testing.T) {
	confirmed := true
	checkinAt := time.Date(2026, 9, 14, 11, 59, 0, 0, time.UTC)
	base := operatorDeviceSyncSnapshot{
		MachineID: "m-confirm-base", DisplayName: "samplehub1", LifecycleRevision: 4,
		Retired: false, JobsEnabled: &confirmed, DeviceSyncV1: &confirmed,
		LatestCheckinSentAt: &checkinAt, LatestCheckinReceivedAt: &checkinAt,
		ActiveJobCount: 0, CurrentResourceRevision: 6,
	}
	impact := OperatorDeviceSyncImpact{
		Kind: model.DeviceSyncJobKind, ResourceKind: model.DeviceSyncResourceKind,
		ResourceID: model.DeviceSyncResourceID, SpecDigest: "sha256:confirmed-spec",
	}

	machineIDChanged := base
	machineIDChanged.MachineID = "m-confirm-other"
	displayNameChanged := base
	displayNameChanged.DisplayName = "samplehub2"
	lifecycleRevisionChanged := base
	lifecycleRevisionChanged.LifecycleRevision = 5
	retiredChanged := base
	retiredChanged.Retired = true

	tests := []struct {
		name        string
		left, right operatorDeviceSyncSnapshot
		consequence string
	}{
		{
			name: "machine id", left: base, right: machineIDChanged,
			consequence: "machine_registry.display_name 沒有唯一索引，兩台機器可以同名；" +
				"確認碼若不綁機器，operator 在一台上看過並確認的 preview 會被拿去對另一台套用",
		},
		{
			name: "display name", left: base, right: displayNameChanged,
			consequence: "operator 確認畫面上看到的機器名字換掉之後，同一份確認碼仍然通用",
		},
		{
			name: "lifecycle revision", left: base, right: lifecycleRevisionChanged,
			consequence: "機器退役再復役（換了一個身分世代）之後，operator 先前的確認仍然有效",
		},
		{
			name: "retired", left: base, right: retiredChanged,
			consequence: "機器已退役這件事不再改變確認碼",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			leftDigest := operatorDeviceSyncPreviewDigest(test.left, impact)
			rightDigest := operatorDeviceSyncPreviewDigest(test.right, impact)
			if leftDigest == rightDigest {
				t.Errorf("actual digests=(%q, %q), expected different digests; consequence: %s",
					leftDigest, rightDigest, test.consequence)
			}
		})
	}
}

func TestOperatorDeviceSyncPreviewDigestEncodingIsFrozen(t *testing.T) {
	confirmed := true
	sentAt := time.Date(2026, 9, 14, 11, 58, 0, 0, time.UTC)
	receivedAt := time.Date(2026, 9, 14, 11, 59, 0, 0, time.UTC)
	snapshot := operatorDeviceSyncSnapshot{
		MachineID: "m-frozen", DisplayName: "frozen-cnode", LifecycleRevision: 7,
		Retired: true, JobsEnabled: &confirmed, DeviceSyncV1: &confirmed,
		LatestCheckinSentAt: &sentAt, LatestCheckinReceivedAt: &receivedAt,
		ActiveJobCount: 3, CurrentResourceRevision: 11,
	}
	impact := OperatorDeviceSyncImpact{
		Kind: model.DeviceSyncJobKind, ResourceKind: model.DeviceSyncResourceKind,
		ResourceID: model.DeviceSyncResourceID, SpecDigest: "sha256:frozen-spec",
		ExecutionTimeoutSeconds: 900, ChangesMachineConfiguration: true,
		CreatesDesiredState: true, CreatesJob: true, DeliveryRequiresJobsEnabled: true,
		JobsEnabled: &confirmed, DeviceSyncV1: &confirmed,
		LatestCheckinSentAt: &sentAt, LatestCheckinReceivedAt: &receivedAt,
		ActiveJobCount: 3, CurrentResourceRevision: 11, PlannedRevision: 12,
		Blockers: []OperatorDeviceSyncBlocker{OperatorDeviceSyncBlockerRetired},
	}

	// 這是確認碼編碼的契約值，改動它等於宣告所有既有的 operator 確認全部失效。
	const expected = "sha256:103cb397a4cda990bcc445195f6a2d628fb11094a1184c462a759615225e3f3b"
	actual := operatorDeviceSyncPreviewDigest(snapshot, impact)
	if actual != expected {
		t.Errorf("actual digest=%q, expected digest=%q; consequence: 確認碼的編碼改了，"+
			"operator 在舊編碼下取得的確認會靜默地被接受或拒絕，而畫面上看到的事實沒有任何變化", actual, expected)
	}
}

// 退役再復役會讓 lifecycle revision 走到 2；既有 fixture 每次都只走剛 enroll、世代為 0 的路徑。
func TestOperatorDeviceSyncReportsTheMachineGenerationItTargets(t *testing.T) {
	st, machineID, now := deviceSyncFixture(t)

	if err := st.RetireMachine(machineID, now.Add(-30*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := st.UnretireMachine(machineID, now.Add(-20*time.Minute)); err != nil {
		t.Fatal(err)
	}

	preview, err := st.PreviewOperatorDeviceSync(machineID, model.DeviceSyncDefaultTimeoutSeconds)
	if err != nil {
		t.Fatal(err)
	}
	if preview.LifecycleRevision != 2 {
		t.Errorf("actual lifecycle revision=%d, expected lifecycle revision=2; consequence: 顯示成 0 的話，"+
			"operator 會以為自己在對一台從未離開過機隊的機器動作", preview.LifecycleRevision)
	}

	req := deviceSyncRequest(machineID, preview, "device-sync-machine-generation")
	result, err := st.ApplyOperatorDeviceSync(req)
	if err != nil {
		t.Fatal(err)
	}
	if result.LifecycleRevision != 2 {
		t.Errorf("actual receipt lifecycle revision=%d, expected receipt lifecycle revision=2; consequence: 記錯的話，"+
			"這台機器退役前後的兩次同步在紀錄上分不開", result.LifecycleRevision)
	}
}

func TestOperatorDeviceSyncPreviewReportsTheRevisionItWillCreate(t *testing.T) {
	st, machineID, _ := deviceSyncFixture(t)

	// 直接把 revision counter 設成 1，不透過「同步另一台機器」——
	// 這支測試釘的是「preview 回報它讀到的那一列、收據建立它答應的號碼」，不是序號空間屬於整個機隊還是單一機器。
	resourceScope := model.DeviceSyncResourceKind + ":" + model.DeviceSyncResourceID
	if _, err := st.DB().Exec(`INSERT INTO revision_counters (resource_scope, current_revision) VALUES (?, ?)
		ON CONFLICT(resource_scope) DO UPDATE SET current_revision=excluded.current_revision`, resourceScope, 1); err != nil {
		t.Fatal(err)
	}

	preview, err := st.PreviewOperatorDeviceSync(machineID, model.DeviceSyncDefaultTimeoutSeconds)
	if err != nil {
		t.Fatal(err)
	}
	if preview.CurrentResourceRevision != deploy.Revision(1) {
		t.Errorf("actual current resource revision=%d, expected current resource revision=1; consequence: 回報成 0 的話，"+
			"operator 看到的起點跟帳本裡的不是同一個", preview.CurrentResourceRevision)
	}
	if preview.PlannedRevision != deploy.Revision(2) {
		t.Errorf("actual planned revision=%d, expected planned revision=2; consequence: 數字錯了，"+
			"operator 事後去 desired_state／jobs 對帳時會找錯那一列", preview.PlannedRevision)
	}

	req := deviceSyncRequest(machineID, preview, "device-sync-preview-revision")
	result, err := st.ApplyOperatorDeviceSync(req)
	if err != nil {
		t.Fatal(err)
	}
	if result.Revision != deploy.Revision(2) {
		t.Errorf("actual receipt revision=%d, expected receipt revision=2; consequence: 收據上的編號是 operator 之後唯一能用來指認這次同步的編號，"+
			"它必須就是確認畫面上答應的那一個", result.Revision)
	}
}
