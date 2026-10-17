package store

import (
	"encoding/json"
	"sort"
	"time"

	"github.com/teddashh/AI-Intune/internal/model"
)

// RecentOOMKills bounds both historical reads and event count; overlap is deduplicated.
func (s *Store) RecentOOMKills(machineID string, now time.Time) ([]model.OOMKill, error) {
	rows, err := s.rdb.Query(`SELECT payload FROM observed_state WHERE machine_id=? AND kind='oom' AND received_at>=? AND received_at<=? ORDER BY received_at DESC LIMIT 128`, machineID, fmtTime(now.Add(-2*time.Hour)), fmtTime(now))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.OOMKill{}
	seen := map[string]bool{}
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var events []model.OOMKill
		if json.Unmarshal([]byte(raw), &events) != nil {
			continue
		}
		for _, e := range events {
			if !model.ValidOOM(e, now) || e.Time.Before(now.Add(-time.Hour)) {
				continue
			}
			e.Time = e.Time.UTC()
			key, _ := json.Marshal(e)
			if !seen[string(key)] {
				out = append(out, e)
				seen[string(key)] = true
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Time.After(out[j].Time) })
	if len(out) > 64 {
		out = out[:64]
	}
	return out, rows.Err()
}
