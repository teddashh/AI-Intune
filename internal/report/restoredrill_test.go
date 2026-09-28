package report

import (
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/state"
)

func quietFleet(now time.Time) Input {
	return Input{Now: now, Since: now.Add(-24 * time.Hour), Expected: 1,
		Machines: []Machine{{ID: "a", DisplayName: "samplehub1", State: state.Online, StateSince: now.Add(-48 * time.Hour)}}}
}

// PHASES D 表：「90 天沒做還原演練，不敢動 Hub」。從來沒做過比太久沒做更要講。
func TestRestoreDrillNagsWhenNeverDoneOrOverdue(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)

	in := quietFleet(now)
	in.RestoreDrill = RestoreDrill{Tracked: true, Done: false}
	if got := Render(in); !strings.Contains(got, "restore drill overdue") || !strings.Contains(got, "尚無還原演練紀錄") {
		t.Errorf("從來沒做過要催：\n%s", got)
	}

	in.RestoreDrill = RestoreDrill{Tracked: true, Done: true, At: now.Add(-91 * 24 * time.Hour)}
	if got := Render(in); !strings.Contains(got, "restore drill overdue") || !strings.Contains(got, "91 天前") {
		t.Errorf("91 天要催、要講幾天：\n%s", got)
	}
}

// 反面：做過而且還沒過期，不催；Hub 沒在看章，也不催（不知道≠沒做過）。
func TestRestoreDrillStaysQuietWhenFreshOrUntracked(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	in := quietFleet(now)
	in.RestoreDrill = RestoreDrill{Tracked: true, Done: true, At: now.Add(-30 * 24 * time.Hour)}
	if got := Render(in); strings.Contains(got, "restore drill") {
		t.Errorf("30 天前做過卻在催：\n%s", got)
	}
	in.RestoreDrill = RestoreDrill{}
	if got := Render(in); strings.Contains(got, "restore drill") || !strings.Contains(got, "alive; 0 changes") {
		t.Errorf("沒在看章卻在催、或吃掉了 alive 那一行：\n%s", got)
	}
}
