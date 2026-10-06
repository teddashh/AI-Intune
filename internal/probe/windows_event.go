package probe

import (
	"fmt"
	"strings"
	"time"

	"github.com/teddashh/AI-Intune/internal/model"
)

const windowsTaskEventLog = "Microsoft-Windows-TaskScheduler/Operational"

func windowsAgentEventQueryScript() string {
	return fmt.Sprintf(`$ErrorActionPreference='Stop'; $xpath='*[System[TimeCreated[timediff(@SystemTime) <= %d]] and EventData[Data[@Name="TaskName"]="\clawctl\clawctl-agent" or Data[@Name="TaskName"]="clawctl-agent"]]'; $events=@(); try { $events=@(Get-WinEvent -LogName '%s' -FilterXPath $xpath -ErrorAction Stop) } catch { if ($_.FullyQualifiedErrorId -notmatch 'NoMatchingEventsFound' -and $_.Exception.Message -notmatch 'No events were found') { [Console]::Error.WriteLine($_.Exception.Message); exit 1 } }; $n=0; foreach ($e in ($events | Sort-Object TimeCreated)) { if ($n -ge %d) { break }; $text=([string]$e.Message -replace '[\r\n]+',' ').Trim(); if ($text -eq '') { $text='Event '+[string]$e.Id }; Write-Output ($e.TimeCreated.ToUniversalTime().ToString('yyyy-MM-ddTHH:mm:ssZ')+' '+[string]$e.Id+' '+$text); $n++ }`,
		int(journalWindow/time.Millisecond), windowsTaskEventLog, journalMaxLines)
}

func windowsUnitJournals(units []model.Unit, query func() (lines []string, truncated bool, err error)) []model.UnitJournal {
	out := make([]model.UnitJournal, 0, len(units))
	var (
		lines    []string
		trunc    bool
		queryErr error
		queried  bool
	)
	for _, unit := range units {
		if !unit.Present {
			continue
		}
		if unit.Name != windowsAgentTaskUnit {
			out = append(out, summarizeErr(unit.Name, "這個 unit 沒有 Windows 記錄來源"))
			continue
		}
		if !queried {
			lines, trunc, queryErr = query()
			queried = true
		}
		if queryErr != nil {
			out = append(out, summarizeErr(unit.Name, queryErr.Error()))
			continue
		}
		out = append(out, summarize(unit.Name, lines, journalWindow, trunc))
	}
	return out
}

func splitWindowsEventLines(stdout string) ([]string, bool) {
	lines := strings.Split(stdout, "\n")
	return lines, countNonEmpty(lines) >= journalMaxLines
}
