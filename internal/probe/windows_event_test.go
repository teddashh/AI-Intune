package probe

import (
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/model"
)

func TestWindowsUnitJournalsSkipAbsentUnits(t *testing.T) {
	got := windowsUnitJournals([]model.Unit{
		{Name: windowsAgentTaskUnit, Present: false, Measured: true},
		{Name: "openclaw-gateway.service", Present: false, Measured: true},
	}, func() ([]string, bool, error) {
		t.Fatal("absent units must not query Event Log")
		return nil, false, nil
	})
	if len(got) != 0 {
		t.Fatalf("journals=%+v", got)
	}
}

func TestWindowsAgentJournalQuietIsNotAFailedRead(t *testing.T) {
	got := windowsUnitJournals([]model.Unit{
		{Name: windowsAgentTaskUnit, Present: true, Measured: true},
	}, func() ([]string, bool, error) {
		return nil, false, nil
	})
	if len(got) != 1 || got[0].Unit != windowsAgentTaskUnit || got[0].Lines != 0 ||
		got[0].Err != "" || got[0].WindowSec != int(journalWindow.Seconds()) {
		t.Fatalf("quiet journal=%+v", got)
	}
	assertNoVerdictFields(t, got[0])
}

func TestWindowsAgentJournalFailedReadIsNotQuiet(t *testing.T) {
	got := windowsUnitJournals([]model.Unit{
		{Name: windowsAgentTaskUnit, Present: true, Measured: true},
	}, func() ([]string, bool, error) {
		return nil, false, errors.New("Get-WinEvent clawctl-agent 失敗：access denied")
	})
	if len(got) != 1 || got[0].Err == "" || got[0].Lines != 0 {
		t.Fatalf("failed read=%+v", got)
	}
	if !strings.Contains(got[0].Err, "access denied") {
		t.Fatalf("err=%q", got[0].Err)
	}
	assertNoVerdictFields(t, got[0])
}

func TestWindowsAgentJournalSummarizesTaskEventsWithoutHealthJudgement(t *testing.T) {
	lines := []string{
		"2026-09-21T12:00:00Z 100 Task Scheduler started \"\\clawctl\\clawctl-agent\" instance \"{11111111-1111-1111-1111-111111111111}\" .",
		"2026-09-21T12:00:01Z 200 Action started",
		"2026-09-21T12:00:01Z 200 Action started",
		"",
	}
	got := windowsUnitJournals([]model.Unit{
		{Name: windowsAgentTaskUnit, Present: true, Measured: true},
	}, func() ([]string, bool, error) {
		return lines, false, nil
	})
	if len(got) != 1 || got[0].Lines != 3 || got[0].Shapes != 2 || got[0].Err != "" || got[0].Truncated {
		t.Fatalf("journal=%+v", got)
	}
	if len(got[0].Top) == 0 || got[0].Top[0].Count != 2 {
		t.Fatalf("repeated action did not group: %+v", got[0].Top)
	}
	assertNoVerdictFields(t, got[0])
}

func TestWindowsAgentJournalRedactsSecretsInEventText(t *testing.T) {
	got := windowsUnitJournals([]model.Unit{
		{Name: windowsAgentTaskUnit, Present: true, Measured: true},
	}, func() ([]string, bool, error) {
		return []string{`2026-09-21T12:00:00Z 201 Authorization: Bearer FakeBearerTokenForTestingOnly123`}, false, nil
	})
	if len(got) != 1 || len(got[0].Top) != 1 {
		t.Fatalf("journal=%+v", got)
	}
	if strings.Contains(got[0].Top[0].Example, "FakeBearerTokenForTestingOnly123") {
		t.Fatalf("secret survived: %q", got[0].Top[0].Example)
	}
}

func TestWindowsPresentNonAgentUnitHasNoInventedLog(t *testing.T) {
	queried := false
	got := windowsUnitJournals([]model.Unit{
		{Name: "openclaw-gateway.service", Present: true, Measured: true},
	}, func() ([]string, bool, error) {
		queried = true
		return []string{"should not be used"}, false, nil
	})
	if queried {
		t.Fatal("non-agent unit queried the agent Event Log")
	}
	if len(got) != 1 || got[0].Err == "" || got[0].Lines != 0 {
		t.Fatalf("non-agent journal=%+v", got)
	}
}

func TestWindowsAgentJournalTruncationIsAdmitted(t *testing.T) {
	got := windowsUnitJournals([]model.Unit{
		{Name: windowsAgentTaskUnit, Present: true, Measured: true},
	}, func() ([]string, bool, error) {
		return []string{"one", "two"}, true, nil
	})
	if len(got) != 1 || !got[0].Truncated || got[0].Lines != 2 {
		t.Fatalf("truncated=%+v", got)
	}
}

func TestSplitWindowsEventLinesUsesJournalCap(t *testing.T) {
	body := strings.Repeat("event line\n", journalMaxLines)
	lines, truncated := splitWindowsEventLines(body)
	if !truncated || countNonEmpty(lines) != journalMaxLines {
		t.Fatalf("truncated=%t nonempty=%d", truncated, countNonEmpty(lines))
	}
	short, truncated := splitWindowsEventLines("one\n\n")
	if truncated || countNonEmpty(short) != 1 {
		t.Fatalf("short truncated=%t nonempty=%d", truncated, countNonEmpty(short))
	}
}

func TestWindowsAgentEventQueryScriptPinsTaskWindowAndCap(t *testing.T) {
	script := windowsAgentEventQueryScript()
	for _, want := range []string{
		windowsTaskEventLog,
		`\clawctl\clawctl-agent`,
		"Get-WinEvent",
		"timediff(@SystemTime) <= " + strconv.Itoa(int(journalWindow/time.Millisecond)),
		strconv.Itoa(journalMaxLines),
		"NoMatchingEventsFound",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("script missing %q: %s", want, script)
		}
	}
	for _, banned := range []string{"journalctl", "LocalSystem", "Healthy", "ErrorCount"} {
		if strings.Contains(script, banned) {
			t.Fatalf("script contains %q", banned)
		}
	}
}

func TestWindowsAgentJournalQueryIsSharedAcrossDuplicateAgentUnits(t *testing.T) {
	calls := 0
	got := windowsUnitJournals([]model.Unit{
		{Name: windowsAgentTaskUnit, Present: true, Measured: true},
		{Name: windowsAgentTaskUnit, Present: true, Measured: true},
	}, func() ([]string, bool, error) {
		calls++
		return []string{"2026-09-21T12:00:00Z 100 started"}, false, nil
	})
	if calls != 1 || len(got) != 2 || got[0].Lines != 1 || got[1].Lines != 1 {
		t.Fatalf("calls=%d journals=%+v", calls, got)
	}
}

func TestWindowsEventTimestampCanonicalizesToOneShape(t *testing.T) {
	j := summarize(windowsAgentTaskUnit, []string{
		"2026-09-21T12:00:00Z 100 started",
		"2026-09-21T12:00:05Z 100 started",
	}, time.Hour, false)
	if j.Lines != 2 || j.Shapes != 1 {
		t.Fatalf("shapes=%d lines=%d", j.Shapes, j.Lines)
	}
}
