//go:build unix

package probe

import (
	"context"

	"github.com/teddashh/AI-Intune/internal/model"
)

func collectUnitJournals(ctx context.Context, units []model.Unit) []model.UnitJournal {
	out := make([]model.UnitJournal, 0, len(units))
	for _, u := range units {
		if !u.Present {
			continue
		}
		out = append(out, readJournal(ctx, u.Name))
	}
	return out
}
