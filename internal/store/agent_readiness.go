package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/teddashh/AI-Intune/internal/model"
)

// AgentReadiness is the latest Hub-owned receipt for one enrolled machine.
//
// JobsEnabled is a column of the same selected check-in as DeviceSyncV1, not an
// independent jobs projection. Mixing a parsed-instant max with TEXT-order
// capability binding would let those two fields describe two different rows, so
// latest-check-in selection is canonical-strict: every received_at for the
// machine must be UTC second-granularity canonical RFC3339, and the selected
// sent_at likewise. TEXT DESC is then exact. Violation fails closed as data
// integrity.
type AgentReadiness struct {
	MachineID              string
	LastCheckinReceivedAt  *time.Time
	AgentStartedAt         *time.Time
	AgentVersion           string
	JobsEnabled            *bool
	DeviceSyncV1           *bool
	MaintenanceDiskCleanV1 *bool
	IdentityReceivedAt     *time.Time
	IdentityMeasuredAt     *time.Time
	IdentityOS             string
	IdentityArch           string
}

func (s *Store) AgentReadiness(machineID string) (AgentReadiness, error) {
	if err := inspectAgentReadinessCheckinTimes(s.rdb, machineID); err != nil {
		return AgentReadiness{}, err
	}
	var result AgentReadiness
	var receivedAt, sentAt, agentStartedAt, agentVersion sql.NullString
	var jobsEnabled, deviceSyncV1, diskCleanV1 sql.NullInt64
	err := s.rdb.QueryRow(`
SELECT m.machine_id,c.received_at,c.sent_at,c.agent_started_at,c.agent_version,c.jobs_enabled,cap.supported,diskcap.supported
  FROM machine_registry m
  LEFT JOIN machine_checkins c ON c.rowid=(
    SELECT rowid FROM machine_checkins
     WHERE machine_id=m.machine_id
     ORDER BY received_at DESC,rowid DESC LIMIT 1
  )
	LEFT JOIN machine_job_capabilities cap
	  ON cap.machine_id=c.machine_id AND cap.sent_at=c.sent_at AND cap.capability=?
	LEFT JOIN machine_job_capabilities diskcap
	  ON diskcap.machine_id=c.machine_id AND diskcap.sent_at=c.sent_at AND diskcap.capability=?
	 WHERE m.machine_id=?`, model.DeviceSyncJobKind, model.MaintenanceDiskCleanCapability, machineID).Scan(
		&result.MachineID, &receivedAt, &sentAt, &agentStartedAt, &agentVersion, &jobsEnabled, &deviceSyncV1, &diskCleanV1)
	if errors.Is(err, sql.ErrNoRows) {
		return AgentReadiness{}, ErrNotFound
	}
	if err != nil {
		return AgentReadiness{}, fmt.Errorf("store: agent readiness: %w", err)
	}
	if sentAt.Valid {
		if _, canonical := parseCanonicalOperatorDeviceSyncStoredTime(sentAt.String); !canonical {
			return AgentReadiness{}, errors.New("store: agent readiness check-in projection is invalid")
		}
	}
	if receivedAt.Valid {
		at := parseTime(receivedAt.String)
		if at.IsZero() {
			return AgentReadiness{}, errors.New("store: agent readiness has invalid check-in time")
		}
		result.LastCheckinReceivedAt = &at
	}
	if agentStartedAt.Valid {
		at := parseTime(agentStartedAt.String)
		if at.IsZero() {
			return AgentReadiness{}, errors.New("store: agent readiness has invalid agent start time")
		}
		result.AgentStartedAt = &at
	}
	if agentVersion.Valid {
		result.AgentVersion = agentVersion.String
	}
	if jobsEnabled.Valid {
		enabled := jobsEnabled.Int64 == 1
		result.JobsEnabled = &enabled
	}
	if deviceSyncV1.Valid {
		supported := deviceSyncV1.Int64 == 1
		result.DeviceSyncV1 = &supported
	}
	if diskCleanV1.Valid {
		supported := diskCleanV1.Int64 == 1
		result.MaintenanceDiskCleanV1 = &supported
	}
	var identityPayload, identityReceivedAt, identityMeasuredAt string
	err = s.rdb.QueryRow(`
SELECT payload,received_at,measured_at FROM observed_state
 WHERE machine_id=? AND kind=? AND subject=? AND source=?
 ORDER BY received_at DESC,measured_at DESC,rowid DESC LIMIT 1`,
		machineID, KindIdentity, KindIdentity, SourceAgentMeasurement).
		Scan(&identityPayload, &identityReceivedAt, &identityMeasuredAt)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return AgentReadiness{}, fmt.Errorf("store: agent readiness identity evidence: %w", err)
	}
	if err == nil {
		at, canonical := parseCanonicalOperatorDeviceSyncStoredTime(identityReceivedAt)
		measuredAt, measuredCanonical := parseCanonicalOperatorDeviceSyncStoredTime(identityMeasuredAt)
		identity, decoded := unmarshalInto[model.Identity](identityPayload)
		if !canonical || !measuredCanonical || !decoded {
			return AgentReadiness{}, errors.New("store: agent readiness identity evidence is invalid")
		}
		result.IdentityReceivedAt = &at
		result.IdentityMeasuredAt = &measuredAt
		result.IdentityOS = identity.OS
		result.IdentityArch = identity.Arch
	}
	return result, nil
}

func inspectAgentReadinessCheckinTimes(db *sql.DB, machineID string) error {
	rows, err := db.Query(`SELECT received_at FROM machine_checkins WHERE machine_id=?`, machineID)
	if err != nil {
		return fmt.Errorf("store: inspect agent readiness check-in times: %w", err)
	}
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			rows.Close()
			return fmt.Errorf("store: inspect agent readiness check-in time: %w", err)
		}
		// Same Hub stored-time canonicality predicate as device-sync latest
		// check-in selection. Reused as-is so this compound receipt cannot
		// diverge from that test; extracting it would rewire operator_device_sync.go.
		if _, canonical := parseCanonicalOperatorDeviceSyncStoredTime(raw); !canonical {
			rows.Close()
			return errors.New("store: agent readiness check-in projection is invalid")
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("store: inspect agent readiness check-in times: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("store: close agent readiness check-in times: %w", err)
	}
	return nil
}
