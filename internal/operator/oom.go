package operator

import (
	"time"

	"github.com/teddashh/AI-Intune/internal/model"
)

type OOMKillResult struct {
	SchemaVersion int             `json:"schema_version"`
	EvaluatedAt   time.Time       `json:"evaluated_at"`
	Items         []model.OOMKill `json:"items"`
}

// MachineOOMKills is the same bounded read used by the human UI and operator API.
func (s *Service) MachineOOMKills(id string, now time.Time) (OOMKillResult, error) {
	if _, err := s.store.GetMachine(id); err != nil {
		return OOMKillResult{}, err
	}
	events, err := s.store.RecentOOMKills(id, now)
	return OOMKillResult{SchemaVersion: 1, EvaluatedAt: now, Items: events}, err
}
