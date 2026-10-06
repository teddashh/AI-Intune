package state

import (
	"strings"
	"testing"
	"time"
)

// freshArtifact 是一條「檔案很新」的產出物事實 —— 讓測試只講事件流那一件事。
// ⚠ 刻意讓新鮮度**通過**：這樣任何降級都只可能來自事件流。
func freshArtifact(now time.Time, ev *EventFact) ArtifactFact {
	mt := now.Add(-time.Minute)
	return ArtifactFact{
		Unit:     "openclaw-watcher.service",
		Artifact: "/home/ubuntu/.openclaw/workspace/evolution-journal.jsonl",
		Why:      "watcher 抓到基準線被改就會一直喊，而沒有人聽得見它喊什麼",
		MaxAge:   15 * time.Minute,
		Checked:  true, Exists: true, ModTime: &mt,
		Events: ev,
	}
}

func msgsOf(j Judgement, kind string) string {
	var b strings.Builder
	for _, f := range findingsOf(j, kind) {
		b.WriteString(f.Message)
		b.WriteString("\n")
	}
	return b.String()
}

// ⚠⚠ 這是 Ted 要的那個狀態機的核心測試：sampleagent2 的 watcher 檔案是新的
// （它活得好好的），但它在喊 baseline_hash_mismatch —— 這台必須是 Degraded。
// 在這個功能之前，這台是全綠的。
func TestActiveNotOKEventDegradesEvenWhenArtifactIsFresh(t *testing.T) {
	now := time.Date(2026, 9, 4, 21, 0, 0, 0, time.UTC)
	last := now.Add(-90 * time.Second)
	f := baseFacts(now)
	f.Artifacts = []ArtifactFact{freshArtifact(now, &EventFact{
		Window: time.Hour, Measured: true,
		Declared: []EventCount{{Type: "baseline_hash_mismatch", Count: 48, LastAt: &last}},
	})}

	j := Derive(f)
	if j.State != Degraded {
		t.Fatalf("事件流在喊 baseline_hash_mismatch，狀態卻是 %s", j.State)
	}
	if !strings.Contains(j.Reason, "baseline_hash_mismatch") {
		t.Fatalf("理由要講出是哪個事件：%q", j.Reason)
	}
	// why 必須原封不動出現 —— 收到告警的人才知道這件事為什麼重要。
	if !strings.Contains(j.Reason, "沒有人聽得見它喊什麼") {
		t.Fatalf("理由要帶上宣告時寫的 why：%q", j.Reason)
	}
}

// ⚠⚠ 這一條擋的是這個專案最容易犯的那種錯：把「狀態」寫成「變化」。
// 同一個穩定的失敗連判兩次，第二次一定要跟第一次一樣紅。
// 實測 sampleagent2 八天每小時 48/48/49/49/48/48/49 —— 完全平的，
// 任何「跟上次比」的機制在這裡都是靜止的。
func TestAStableFailureStaysRedEveryTime(t *testing.T) {
	now := time.Date(2026, 9, 4, 21, 0, 0, 0, time.UTC)
	for i, count := range []int{48, 48, 49, 49, 48, 48, 49} {
		last := now.Add(-time.Minute)
		f := baseFacts(now)
		f.Artifacts = []ArtifactFact{freshArtifact(now, &EventFact{
			Window: time.Hour, Measured: true,
			Declared: []EventCount{{Type: "baseline_hash_mismatch", Count: count, LastAt: &last}},
		})}
		if j := Derive(f); j.State != Degraded {
			t.Fatalf("第 %d 輪（%d 次）狀態是 %s —— 一個穩定的失敗必須每一輪都紅",
				i+1, count, j.State)
		}
	}
}

// 宣告過但這個窗口裡沒出現 → 不降級，但措辭不准講成「沒問題」。
func TestQuietEventStreamDoesNotClaimHealth(t *testing.T) {
	now := time.Date(2026, 9, 4, 21, 0, 0, 0, time.UTC)
	f := baseFacts(now)
	f.Artifacts = []ArtifactFact{freshArtifact(now, &EventFact{
		Window: time.Hour, Measured: true,
		Declared: []EventCount{{Type: "baseline_hash_mismatch", Count: 0}},
	})}

	j := Derive(f)
	if j.State != Online {
		t.Fatalf("沒有不 OK 的事件就不該降級，得到 %s", j.State)
	}
	m := msgsOf(j, "workload")
	if !strings.Contains(m, "沒有回報") {
		t.Fatalf("要講「沒有回報」：%q", m)
	}
	// ⚠ 措辭界線：我們知道的只是它沒寫出這幾個名字，不是它沒問題。
	for _, banned := range []string{"沒有問題", "一切正常", "健康"} {
		if strings.Contains(m, banned) {
			t.Fatalf("安靜不等於沒事，不准說「%s」：%q", banned, m)
		}
	}
}

// ⚠⚠ 一種新長出來的失敗事件，在只看宣告名單的世界裡是完全隱形的 ——
// 而它恰好最可能是還沒有人想到的那一種。它要看得見，但不替人判它是壞事。
func TestUndeclaredEventsAreVisibleButAdvisory(t *testing.T) {
	now := time.Date(2026, 9, 4, 21, 0, 0, 0, time.UTC)
	last := now.Add(-2 * time.Minute)
	f := baseFacts(now)
	f.Artifacts = []ArtifactFact{freshArtifact(now, &EventFact{
		Window: time.Hour, Measured: true,
		Declared: []EventCount{{Type: "baseline_hash_mismatch", Count: 0}},
		Undeclared: []EventCount{
			{Type: "hermes_release_review_pending", Count: 3, LastAt: &last},
			{Type: "watcher_heartbeat", Count: 60, LastAt: &last},
		},
		UndeclaredTotal: 2,
	})}

	j := Derive(f)
	if j.State != Online {
		t.Fatalf("沒宣告過的事件不該自己降級（那就變成 clawctl 在猜），得到 %s", j.State)
	}
	var found bool
	for _, fi := range findingsOf(j, "workload") {
		if strings.Contains(fi.Message, "hermes_release_review_pending") {
			found = true
			if !fi.Advisory {
				t.Fatal("沒宣告過的事件要是 advisory")
			}
		}
	}
	if !found {
		t.Fatalf("沒宣告過的事件必須看得見：%q", msgsOf(j, "workload"))
	}
}

func TestUndeclaredTruncationIsDisclosed(t *testing.T) {
	now := time.Date(2026, 9, 4, 21, 0, 0, 0, time.UTC)
	f := baseFacts(now)
	f.Artifacts = []ArtifactFact{freshArtifact(now, &EventFact{
		Window: time.Hour, Measured: true,
		Declared:        []EventCount{{Type: "boom", Count: 0}},
		Undeclared:      []EventCount{{Type: "a", Count: 9}, {Type: "b", Count: 8}},
		UndeclaredTotal: 11,
	})}
	m := msgsOf(Derive(f), "workload")
	// ⚠ 有上限就一定要講總數，不然截斷會偽裝成「就這些」。
	if !strings.Contains(m, "9 種") {
		t.Fatalf("要講出還有幾種沒列出來：%q", m)
	}
}

// ⚠ 撞到讀取上限時次數是**下界**。把下界寫成總數，人會拿它去對帳然後對不起來。
func TestPartialWindowSaysAtLeast(t *testing.T) {
	now := time.Date(2026, 9, 4, 21, 0, 0, 0, time.UTC)
	last := now.Add(-time.Minute)
	f := baseFacts(now)
	f.Artifacts = []ArtifactFact{freshArtifact(now, &EventFact{
		Window: time.Hour, Measured: true, Partial: true,
		Declared: []EventCount{{Type: "baseline_hash_mismatch", Count: 40, LastAt: &last}},
	})}
	j := Derive(f)
	if !strings.Contains(j.Reason, "≥40") {
		t.Fatalf("沒讀滿窗口時要寫成下界：%q", j.Reason)
	}
	if strings.Contains(j.Reason, "×40") {
		t.Fatalf("下界不准寫成總數：%q", j.Reason)
	}
}

// ⚠⚠ 欄位名寫錯 → 每一行都讀不懂 → 這條規則永遠是安靜的。
// 這是最惡毒的假綠燈，它必須自己吵出來。
func TestMalformedLinesAreSurfaced(t *testing.T) {
	now := time.Date(2026, 9, 4, 21, 0, 0, 0, time.UTC)
	f := baseFacts(now)
	f.Artifacts = []ArtifactFact{freshArtifact(now, &EventFact{
		Window: time.Hour, Measured: true, Malformed: 1440,
		Declared: []EventCount{{Type: "baseline_hash_mismatch", Count: 0}},
	})}
	m := msgsOf(Derive(f), "workload")
	if !strings.Contains(m, "1440") || !strings.Contains(m, "無法解析") {
		t.Fatalf("無法解析的行數要講出來：%q", m)
	}
}

func TestEventStreamReadErrorIsNotSilent(t *testing.T) {
	now := time.Date(2026, 9, 4, 21, 0, 0, 0, time.UTC)
	f := baseFacts(now)
	f.Artifacts = []ArtifactFact{freshArtifact(now, &EventFact{
		Window: time.Hour, Measured: true, Err: "permission denied",
	})}
	j := Derive(f)
	if j.State != Degraded {
		t.Fatalf("事件流讀不到不是「沒事」，得到 %s", j.State)
	}
	if !strings.Contains(msgsOf(j, "workload"), "permission denied") {
		t.Fatal("要把讀不到的原因講出來")
	}
}

// ⚠ 宣告了要讀事件流、但 agent 還沒回報（舊版 agent）→ 不降級，
// 而且不准重複講。ArtifactFact 那邊已經為「還沒量到」發過一則了。
func TestDeclaredButNotYetMeasuredSaysItOnce(t *testing.T) {
	now := time.Date(2026, 9, 4, 21, 0, 0, 0, time.UTC)
	f := baseFacts(now)
	a := freshArtifact(now, &EventFact{Window: time.Hour, Measured: false})
	a.Checked = false // agent 是舊版，整條規則都沒量
	a.Exists, a.ModTime = false, nil
	f.Artifacts = []ArtifactFact{a}

	j := Derive(f)
	if j.State != Online {
		t.Fatalf("還沒量到不是失敗，得到 %s", j.State)
	}
	// ⚠ 只數提到這條規則的 finding —— 這一段裡本來就有別的 workload finding
	// （「最近一次跑完是 …」），把它們一起數進來會讓這個測試在錯的地方失敗。
	var mine []Finding
	for _, fi := range findingsOf(j, "workload") {
		if strings.Contains(fi.Message, "evolution-journal.jsonl") {
			mine = append(mine, fi)
		}
	}
	if len(mine) != 1 {
		t.Fatalf("「還沒量到」只該講一次，得到 %d 則：%q", len(mine), msgsOf(j, "workload"))
	}
	if !mine[0].Advisory {
		t.Fatal("還沒量到要是 advisory")
	}
}

// 一條沒有宣告 Events 的規則，不該產生任何事件相關的 finding。
func TestArtifactWithoutEventSpecIsUnaffected(t *testing.T) {
	now := time.Date(2026, 9, 4, 21, 0, 0, 0, time.UTC)
	f := baseFacts(now)
	f.Artifacts = []ArtifactFact{freshArtifact(now, nil)}
	j := Derive(f)
	if j.State != Online {
		t.Fatalf("要 Healthy，得到 %s", j.State)
	}
	if m := msgsOf(j, "workload"); strings.Contains(m, "事件") {
		t.Fatalf("沒宣告事件流就不該講事件：%q", m)
	}
}
