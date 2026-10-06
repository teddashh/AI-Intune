//go:build windows

package probe

import (
	"context"
	"errors"

	"github.com/teddashh/AI-Intune/internal/model"
)

const windowsTaskQueryScript = `$t=Get-ScheduledTask -TaskPath '\clawctl\' -TaskName 'clawctl-agent' -ErrorAction SilentlyContinue; if($null -eq $t){ 'found=0'; exit 0 }; $i=Get-ScheduledTaskInfo -InputObject $t; $run=$i.LastRunTime; if($run -eq [DateTime]::MinValue -or $run.Year -lt 2000){ $stamp='0001-01-01T00:00:00Z' } else { $stamp=$run.ToUniversalTime().ToString('yyyy-MM-ddTHH:mm:ssZ') }; 'found=1'; 'state='+[int]$t.State; 'lastrun='+$stamp`

func collectUnits(ctx context.Context, names []string) []model.Unit {
	var (
		agentView windowsTaskView
		agentErr  error
		queried   bool
	)
	units := make([]model.Unit, 0, len(names))
	for _, name := range names {
		if name != windowsAgentTaskUnit {
			units = append(units, windowsObservedUnit(name, windowsTaskView{}, nil))
			continue
		}
		if !queried {
			agentView, agentErr = queryWindowsAgentTask(ctx)
			queried = true
		}
		units = append(units, windowsObservedUnit(name, agentView, agentErr))
	}
	return units
}

func queryWindowsAgentTask(ctx context.Context) (windowsTaskView, error) {
	out, stderr, err := run(ctx, cmdTimeout, "powershell.exe",
		"-NoProfile", "-NonInteractive", "-Command", windowsTaskQueryScript)
	if err != nil {
		return windowsTaskView{}, errors.New(commandFailure("Get-ScheduledTask clawctl-agent", out, stderr, err))
	}
	view, parseErr := parseWindowsTaskReport(out)
	if parseErr != nil {
		return windowsTaskView{}, parseErr
	}
	return view, nil
}
