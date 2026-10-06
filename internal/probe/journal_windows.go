//go:build windows

package probe

import (
	"context"
	"errors"

	"github.com/teddashh/AI-Intune/internal/model"
)

func collectUnitJournals(ctx context.Context, units []model.Unit) []model.UnitJournal {
	return windowsUnitJournals(units, func() ([]string, bool, error) {
		return queryWindowsAgentEvents(ctx)
	})
}

func queryWindowsAgentEvents(ctx context.Context) ([]string, bool, error) {
	out, stderr, err := run(ctx, journalCmdTimeout, "powershell.exe",
		"-NoProfile", "-NonInteractive", "-Command", windowsAgentEventQueryScript())
	if err != nil {
		return nil, false, errors.New(commandFailure("Get-WinEvent clawctl-agent", out, stderr, err))
	}
	lines, truncated := splitWindowsEventLines(out)
	return lines, truncated, nil
}
