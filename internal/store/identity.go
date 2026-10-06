package store

import (
	"database/sql"
	"fmt"
	"sort"
	"time"

	"github.com/teddashh/AI-Intune/internal/model"
)

// identityRowsQueryer is implemented by both *sql.DB and *sql.Tx. Keeping the
// projection reader shared is important: Facts and the in-transaction workload
// recovery check must use exactly the same identity-conflict evidence.
type identityRowsQueryer interface {
	Query(query string, args ...any) (*sql.Rows, error)
}

// recordIdentityHintTx latches every explicitly observed /etc/machine-id. There
// is deliberately no delete or replace path: a conflict is an identity/security
// fact, not a transient health sample. Retiring this registry identity and
// enrolling a new one is the existing explicit remediation.
func recordIdentityHintTx(tx dbTx, machineID, hint string, receivedAt time.Time) error {
	if hint == "" {
		return nil
	}
	at := fmtTime(receivedAt)
	_, err := tx.Exec(`INSERT INTO machine_identity_hints
 (machine_id,hint,first_seen_at,last_seen_at,observation_count)
 VALUES (?,?,?,?,1)
 ON CONFLICT(machine_id,hint) DO UPDATE SET
   first_seen_at=MIN(machine_identity_hints.first_seen_at,excluded.first_seen_at),
   last_seen_at=MAX(machine_identity_hints.last_seen_at,excluded.last_seen_at),
   observation_count=machine_identity_hints.observation_count+1`,
		machineID, hint, at, at)
	if err != nil {
		return fmt.Errorf("store: identity hint projection: %w", err)
	}
	return nil
}

type identityHintAggregate struct {
	machineID string
	hint      string
	first     time.Time
	last      time.Time
	count     int
}

// backfillIdentityHints migrates the identity observations retained by an old
// Hub into the non-expiring conflict latch. It is safe to run on every Open:
// MAX(existing count, retained-raw count) avoids double-counting, while MIN/MAX
// preserve evidence that is older than the raw-observation retention window.
func backfillIdentityHints(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin identity hint backfill: %w", err)
	}
	defer tx.Rollback()

	rows, err := tx.Query(`SELECT machine_id,payload,received_at
 FROM observed_state WHERE kind=?`, KindIdentity)
	if err != nil {
		return fmt.Errorf("read identity hint backfill: %w", err)
	}
	aggregates := make(map[string]*identityHintAggregate)
	for rows.Next() {
		var machineID, payload, received string
		if err := rows.Scan(&machineID, &payload, &received); err != nil {
			rows.Close()
			return fmt.Errorf("scan identity hint backfill: %w", err)
		}
		identity, ok := unmarshalInto[model.Identity](payload)
		if !ok || identity.MachineIDHint == "" {
			continue
		}
		at := parseTime(received)
		key := machineID + "\x00" + identity.MachineIDHint
		a := aggregates[key]
		if a == nil {
			a = &identityHintAggregate{
				machineID: machineID,
				hint:      identity.MachineIDHint,
				first:     at,
				last:      at,
			}
			aggregates[key] = a
		}
		a.count++
		if at.Before(a.first) {
			a.first = at
		}
		if at.After(a.last) {
			a.last = at
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("read identity hint backfill: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close identity hint backfill: %w", err)
	}

	keys := make([]string, 0, len(aggregates))
	for key := range aggregates {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		a := aggregates[key]
		if _, err := tx.Exec(`INSERT INTO machine_identity_hints
 (machine_id,hint,first_seen_at,last_seen_at,observation_count)
 VALUES (?,?,?,?,?)
 ON CONFLICT(machine_id,hint) DO UPDATE SET
   first_seen_at=MIN(machine_identity_hints.first_seen_at,excluded.first_seen_at),
   last_seen_at=MAX(machine_identity_hints.last_seen_at,excluded.last_seen_at),
   observation_count=MAX(machine_identity_hints.observation_count,excluded.observation_count)`,
			a.machineID, a.hint, fmtTime(a.first), fmtTime(a.last), a.count); err != nil {
			return fmt.Errorf("write identity hint backfill: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit identity hint backfill: %w", err)
	}
	return nil
}

func identityHintsFrom(q identityRowsQueryer, machineID, registryHint string) ([]IdentityHint, error) {
	return identityHintsFromAt(q, machineID, registryHint, time.Time{})
}

func identityHintsFromAt(q identityRowsQueryer, machineID, registryHint string, evaluatedAt time.Time) ([]IdentityHint, error) {
	query := `SELECT hint,first_seen_at,last_seen_at,observation_count
 FROM machine_identity_hints WHERE machine_id=?`
	args := []any{machineID}
	if !evaluatedAt.IsZero() {
		query += ` AND first_seen_at<=?`
		args = append(args, fmtTime(evaluatedAt))
	}
	query += ` ORDER BY last_seen_at DESC,hint`
	rows, err := q.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: identity hints: %w", err)
	}
	defer rows.Close()

	out := make([]IdentityHint, 0)
	registrySeen := false
	for rows.Next() {
		var hint, first, last string
		var count int
		if err := rows.Scan(&hint, &first, &last, &count); err != nil {
			return nil, fmt.Errorf("store: scan identity hints: %w", err)
		}
		row := IdentityHint{
			Hint:      hint,
			FirstSeen: parseTime(first),
			LastSeen:  parseTime(last),
			Count:     count,
		}
		if registryHint != "" && hint == registryHint {
			// Preserve the existing UI semantics: the registry declaration is
			// included in Count and marked on the same displayed row.
			row.Count++
			row.FromRegistry = true
			registrySeen = true
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: identity hints: %w", err)
	}
	if registryHint != "" && !registrySeen {
		out = append(out, IdentityHint{
			Hint: registryHint, Count: 1, FromRegistry: true,
		})
	}
	return out, nil
}

func identityConflictFrom(q identityRowsQueryer, machineID, registryHint string) (bool, error) {
	hints, err := identityHintsFrom(q, machineID, registryHint)
	if err != nil {
		return false, err
	}
	return len(hints) > 1, nil
}
