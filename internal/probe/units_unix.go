//go:build unix

package probe

import (
	"context"

	"github.com/teddashh/AI-Intune/internal/model"
)

func collectUnits(ctx context.Context, names []string) []model.Unit {
	units := make([]model.Unit, 0, len(names))
	unixTS := true // 先試 --timestamp=unix，舊版 systemd 不認得就整輪退回人類格式
	for _, name := range names {
		u, ok := showUnit(ctx, name, unixTS)
		if !ok && unixTS {
			unixTS = false
			u, _ = showUnit(ctx, name, false)
		}
		units = append(units, u)
	}
	return units
}
