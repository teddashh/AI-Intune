package report

import (
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/state"
)

func at(base time.Time, offset time.Duration) time.Time { return base.Add(offset) }

// 這幾支守的是 §5.24 裡那個真實的誤導：
//
//	sampleagent2 失聯 → 降級：grok 的登入在 3 天前就過期了
//	sampleagent4 失聯 → 降級：claude 的登入在 14 天前就過期了
//
// 兩行都字面正確，而成因是 Hub 自己在四分鐘前重啟過。早報把觀測者的
// 問題寫成了被觀測者的問題 —— 而人會照著它去查那幾台好機器。
func TestSimultaneousUnreachablesAreReportedAsOneHubEvent(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	in := Input{
		Now: now, Since: now.Add(-24 * time.Hour), Expected: 4,
		Machines: []Machine{
			// ⚠ StateSince 跟 UnreachableAt 刻意**不相等**。
			//
			// 第一版的 fixture 讓它們相等（因為程式讀的是 StateSince），
			// 於是九支測試全綠、實機一次都沒觸發過。真實資料裡這兩個數字
			// 差七個半小時。fixture 寫成「兩個欄位剛好一樣」的時候，
			// 它驗證的是我對世界的假設，不是世界。
			{ID: "a", DisplayName: "samplehub1", State: state.Degraded, Reason: "grok 過期",
				Changed: true, PreviousState: state.Unreachable, StateSince: at(now, -4*time.Hour),
				UnreachableAt: []time.Time{at(now, -4*time.Hour-time.Minute)}},
			{ID: "b", DisplayName: "sampleagent2", State: state.Degraded, Reason: "claude 過期",
				Changed: true, PreviousState: state.Unreachable, StateSince: at(now, -4*time.Hour+time.Minute),
				UnreachableAt: []time.Time{at(now, -4*time.Hour-30*time.Second)}},
			{ID: "c", DisplayName: "sampleagent4", State: state.Degraded, Reason: "claude 過期",
				Changed: true, PreviousState: state.Unreachable, StateSince: at(now, -4*time.Hour+90*time.Second),
				UnreachableAt: []time.Time{at(now, -4*time.Hour)}},
			{ID: "d", DisplayName: "sampleagent3", State: state.Online, StateSince: at(now, -20*time.Hour)},
		},
	}
	got := Render(in)

	if !strings.Contains(got, "Hub 自己沒在聽") {
		t.Errorf("三台在 90 秒內一起從失聯恢復，早報沒有指出這通常是 Hub 的問題。\n實際輸出：\n%s", got)
	}
	// ⚠ 名字不能省。「3 台一起失聯」不能行動，列出名字才能。
	for _, n := range []string{"samplehub1", "sampleagent2", "sampleagent4"} {
		if !strings.Contains(got, n) {
			t.Errorf("合併之後 %s 的名字不見了 —— 把機器藏起來正是這個產品要修的 bug\n%s", n, got)
		}
	}
	// 而且不可以再逐台重複講一次「失聯 → 降級」。
	if n := strings.Count(got, "失聯 → "); n != 0 {
		t.Errorf("合併過了還留著 %d 行逐台的「失聯 →」——"+
			"那是把一件事拆回三件事\n%s", n, got)
	}
}

// ⚠⚠ 反面：真的只有一台失聯，不准被講成 Hub 的問題。
//
// 沒有這一條的話，把判準寫成「永遠成立」也會讓上面那支通過 ——
// 而那會把每一次真的單機故障都甩鍋給 Hub，比原本的 bug 更糟。
func TestASingleUnreachableIsStillTheMachinesProblem(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	in := Input{
		Now: now, Since: now.Add(-24 * time.Hour), Expected: 4,
		Machines: []Machine{
			{ID: "a", DisplayName: "samplehub1", State: state.Degraded, Reason: "grok 過期",
				Changed: true, PreviousState: state.Unreachable, StateSince: at(now, -4*time.Hour),
				UnreachableAt: []time.Time{at(now, -4*time.Hour-time.Minute)}},
			{ID: "b", DisplayName: "sampleagent2", State: state.Online, StateSince: at(now, -20*time.Hour)},
			{ID: "c", DisplayName: "sampleagent4", State: state.Online, StateSince: at(now, -20*time.Hour)},
			{ID: "d", DisplayName: "sampleagent3", State: state.Online, StateSince: at(now, -20*time.Hour)},
		},
	}
	got := Render(in)
	if strings.Contains(got, "Hub 自己沒在聽") {
		t.Errorf("只有一台失聯卻甩鍋給 Hub —— 那會讓真的單機故障沒有人去查\n%s", got)
	}
	if !strings.Contains(got, "samplehub1") {
		t.Errorf("那一台的名字必須還在\n%s", got)
	}
}

// ⚠⚠ 現在還黑著的，不可以講成「又恢復」。
//
// 這支測試對應的 bug 是我自己寫的：合併那一行寫死了「一起失聯又恢復」，
// 於是三台**現在都還連不上**的機器，被寫成事情已經過去了。整則早報是：
//
//	0/4 報到
//	samplehub1、sampleagent2、sampleagent4 在 2 分鐘內一起失聯又恢復 —— …
//
// 第一行說沒有半台在報到，第二行說恢復了。人會信第二行，因為它比較具體。
func TestMachinesStillDarkAreNotDescribedAsRecovered(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	in := Input{
		Now: now, Since: now.Add(-24 * time.Hour), Expected: 4,
		Machines: []Machine{
			{ID: "a", DisplayName: "samplehub1", State: state.Unreachable, Reason: "沒回應",
				Changed: true, PreviousState: state.Online, StateSince: at(now, -20*time.Minute),
				UnreachableAt: []time.Time{at(now, -20*time.Minute)}},
			{ID: "b", DisplayName: "sampleagent2", State: state.Unreachable, Reason: "沒回應",
				Changed: true, PreviousState: state.Online, StateSince: at(now, -19*time.Minute),
				UnreachableAt: []time.Time{at(now, -19*time.Minute)}},
			{ID: "c", DisplayName: "sampleagent4", State: state.Unreachable, Reason: "沒回應",
				Changed: true, PreviousState: state.Online, StateSince: at(now, -19*time.Minute),
				UnreachableAt: []time.Time{at(now, -19*time.Minute)}},
			{ID: "d", DisplayName: "sampleagent3", State: state.Online, StateSince: at(now, -20*time.Hour)},
		},
	}
	got := Render(in)
	if strings.Contains(got, "恢復") {
		t.Errorf("三台現在都還連不上，早報卻說「恢復」——"+
			"讀的人會以為事情過去了，而機隊正在黑著\n%s", got)
	}
	if !strings.Contains(got, "還連不上") {
		t.Errorf("合併那一行沒有講出「現在還連不上」——"+
			"逐台的行已經被合併掉了，這行不講就沒有人講了\n%s", got)
	}
}

// 混合：一群一起失聯，但只有一部分回來了。
//
// 「又恢復」跟「都還連不上」兩句話在這裡都是錯的，而且錯得很難看見 ——
// 沒有這一條的話，用 `len(stillDown) > 0` 當「全部都還黑著」也會讓上面綠燈。
func TestPartialRecoveryNamesWhoIsStillDark(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	in := Input{
		Now: now, Since: now.Add(-24 * time.Hour), Expected: 4,
		Machines: []Machine{
			{ID: "a", DisplayName: "samplehub1", State: state.Online,
				Changed: true, PreviousState: state.Unreachable, StateSince: at(now, -20*time.Minute),
				UnreachableAt: []time.Time{at(now, -25*time.Minute)}},
			{ID: "b", DisplayName: "sampleagent2", State: state.Unreachable, Reason: "沒回應",
				Changed: true, PreviousState: state.Online, StateSince: at(now, -24*time.Minute),
				UnreachableAt: []time.Time{at(now, -24*time.Minute)}},
			{ID: "c", DisplayName: "sampleagent4", State: state.Online,
				Changed: true, PreviousState: state.Unreachable, StateSince: at(now, -19*time.Minute),
				UnreachableAt: []time.Time{at(now, -24*time.Minute)}},
			{ID: "d", DisplayName: "sampleagent3", State: state.Online, StateSince: at(now, -20*time.Hour)},
		},
	}
	got := Render(in)
	if !strings.Contains(got, "sampleagent2 到現在還連不上") {
		t.Errorf("三台一起失聯、只有 sampleagent2 沒回來，早報必須點名它\n%s", got)
	}
	if strings.Contains(got, "samplehub1 到現在") || strings.Contains(got, "sampleagent4 到現在") {
		t.Errorf("已經回來的機器被列進「還連不上」\n%s", got)
	}
}

// 中文夾阿拉伯數字的空格。這不是美觀問題 ——
// 「在 3 分鐘 內」跟「在2 分鐘內」都出現過，都是同一行字串拼錯。
func TestTheTimeSpanReadsLikeChinese(t *testing.T) {
	for _, c := range []struct{ d, want string }{
		{"0s", "在同一秒"},
		{"45s", "在 45 秒內"},
		{"60s", "在 1 分鐘內"}, // ⚠ 剛好一分鐘不是「2 分鐘內」
		{"90s", "在 2 分鐘內"},
		{"120s", "在 2 分鐘內"}, // 無條件 +1 會在這裡講成 3 分鐘
	} {
		d, err := time.ParseDuration(c.d)
		if err != nil {
			t.Fatal(err)
		}
		if got := withinPhrase(d); got != c.want {
			t.Errorf("withinPhrase(%s) = %q，要 %q", c.d, got, c.want)
		}
	}
}

// 隔得夠遠的兩次失聯是兩件事，不可以併成一件。
func TestUnreachablesFarApartAreNotOneEvent(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	in := Input{
		Now: now, Since: now.Add(-24 * time.Hour), Expected: 3,
		Machines: []Machine{
			{ID: "a", DisplayName: "samplehub1", State: state.Degraded, Reason: "grok 過期",
				Changed: true, PreviousState: state.Unreachable, StateSince: at(now, -9*time.Hour),
				UnreachableAt: []time.Time{at(now, -9*time.Hour-time.Minute)}},
			{ID: "b", DisplayName: "sampleagent2", State: state.Degraded, Reason: "claude 過期",
				Changed: true, PreviousState: state.Unreachable, StateSince: at(now, -2*time.Hour),
				UnreachableAt: []time.Time{at(now, -2*time.Hour-time.Minute)}},
			{ID: "c", DisplayName: "sampleagent4", State: state.Online, StateSince: at(now, -20*time.Hour)},
		},
	}
	if got := Render(in); strings.Contains(got, "Hub 自己沒在聽") {
		t.Errorf("相隔 7 小時的兩次失聯被併成同一件事\n%s", got)
	}
}

// 比例判準：大機隊裡的兩台同時失聯，不足以指控觀測者。
func TestTwoOutOfManyIsNotEnoughToBlameTheHub(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	in := Input{Now: now, Since: now.Add(-24 * time.Hour), Expected: 12}
	for i := 0; i < 2; i++ {
		in.Machines = append(in.Machines, Machine{
			ID: string(rune('a' + i)), DisplayName: "bad" + string(rune('1'+i)),
			State: state.Degraded, Reason: "x", Changed: true,
			PreviousState: state.Unreachable, StateSince: at(now, -4*time.Hour),
			UnreachableAt: []time.Time{at(now, -4*time.Hour-time.Minute)},
		})
	}
	for i := 2; i < 12; i++ {
		in.Machines = append(in.Machines, Machine{
			ID: string(rune('a' + i)), DisplayName: "ok" + string(rune('0'+i)),
			State: state.Online, StateSince: at(now, -20*time.Hour),
		})
	}
	if got := Render(in); strings.Contains(got, "Hub 自己沒在聽") {
		t.Errorf("12 台裡有 2 台同時失聯就指控 Hub —— 那個比例不構成證據\n%s", got)
	}
}

// 分母陷阱：10 台裡的 4 台同時黑掉，**不是**全機隊事件。
//
// ⚠ 這一條專門守分母的定義。把分母換成 header() 裡那個 `reported`
// （Online+Degraded+IdentityConflict，不含 Unreachable）的話：
// 分子 4 台漲上去的同時分母縮成 6，4*2 >= 6 成立 —— 綠燈變紅燈的方向反了。
// 同一個檔案裡有兩個都叫「有在報到」的數字，遲早有人會把它們合併。
func TestFourOutOfTenIsNotAFleetWideEvent(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	in := Input{Now: now, Since: now.Add(-24 * time.Hour), Expected: 10}
	for i := 0; i < 4; i++ {
		in.Machines = append(in.Machines, Machine{
			ID: string(rune('a' + i)), DisplayName: "dark" + string(rune('1'+i)),
			State: state.Unreachable, Reason: "沒回應", Changed: true,
			PreviousState: state.Online, StateSince: at(now, -20*time.Minute),
			UnreachableAt: []time.Time{at(now, -20*time.Minute)},
		})
	}
	for i := 4; i < 10; i++ {
		in.Machines = append(in.Machines, Machine{
			ID: string(rune('a' + i)), DisplayName: "ok" + string(rune('0'+i)),
			State: state.Online, StateSince: at(now, -20*time.Hour),
		})
	}
	if got := Render(in); strings.Contains(got, "Hub 自己沒在聽") {
		t.Errorf("10 台裡 4 台同時黑掉就指控 Hub —— 那不是全機隊事件，"+
			"而剩下 6 台好好的正是反證\n%s", got)
	}
}

// 但退役的機器不算在分母裡：五台退了三台，剩下兩台一起黑掉就是全滅。
func TestRetiredMachinesAreNotInTheDenominator(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	in := Input{
		Now: now, Since: now.Add(-24 * time.Hour), Expected: 5,
		Machines: []Machine{
			{ID: "a", DisplayName: "samplehub1", State: state.Unreachable, Reason: "沒回應",
				Changed: true, PreviousState: state.Online, StateSince: at(now, -20*time.Minute),
				UnreachableAt: []time.Time{at(now, -20*time.Minute)}},
			{ID: "b", DisplayName: "sampleagent2", State: state.Unreachable, Reason: "沒回應",
				Changed: true, PreviousState: state.Online, StateSince: at(now, -19*time.Minute),
				UnreachableAt: []time.Time{at(now, -19*time.Minute)}},
			{ID: "c", DisplayName: "gone1", State: state.NeverReported, StateSince: at(now, -90*24*time.Hour)},
			{ID: "d", DisplayName: "gone2", State: state.NeverReported, StateSince: at(now, -90*24*time.Hour)},
			{ID: "e", DisplayName: "gone3", State: state.NeverReported, StateSince: at(now, -90*24*time.Hour)},
		},
	}
	if got := Render(in); !strings.Contains(got, "Hub 自己沒在聽") {
		t.Errorf("活著的兩台一起黑掉，卻因為名冊上還掛著三台退役機器就不算全滅\n%s", got)
	}
}

// 小機隊的比例陷阱：**兩台裡的一台**。
//
// 比例判準在這裡是放行的（1*2 >= 2），擋下它的是「這一群至少要 2 台」。
// 沒有這一條的話，機隊越小、單機故障越容易被講成「Hub 自己沒在聽」。
//
// ⚠ 這個註解的第一版是錯的。我寫著「這個情境只有 len(cand) < 2 那道關卡
// 守得住」，然後突變測試證明**拿掉那道關卡，這支測試照樣是綠的** ——
// 因為 len(best) <= len(cand)，後面那道一定會先攔下來。那道關卡是死的，
// 已經拿掉。留這段話是因為：一個「看起來多一層防護」的判斷式，
// 比沒有它更危險 —— 它讓人以為這裡有兩道防線，而其實只有一道。
func TestOneOutOfTwoIsStillTheMachinesProblem(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	in := Input{
		Now: now, Since: now.Add(-24 * time.Hour), Expected: 2,
		Machines: []Machine{
			{ID: "a", DisplayName: "samplehub1", State: state.Degraded, Reason: "grok 過期",
				Changed: true, PreviousState: state.Unreachable, StateSince: at(now, -4*time.Hour),
				UnreachableAt: []time.Time{at(now, -4*time.Hour-time.Minute)}},
			{ID: "b", DisplayName: "sampleagent2", State: state.Online, StateSince: at(now, -20*time.Hour)},
		},
	}
	if got := Render(in); strings.Contains(got, "Hub 自己沒在聽") {
		t.Errorf("兩台裡的一台失聯就指控 Hub —— 一台機器不會因為機隊小就變成觀測者的錯\n%s", got)
	}
}

// ⚠⚠⚠ 這一支是用 2026-09-04 15:47 真實資料庫裡的值寫的，而它是這整段
// 工作真正的驗收 —— 上面那些 fixture 全部綠燈的時候，實機一次都沒觸發過。
//
// 資料庫裡有兩次乾淨的同時失聯：
//
//	00:46:15 sampleagent2 / 00:46:15 sampleagent4 / 00:46:45 samplehub1   ← 三台，30 秒內
//	08:15:45 samplehub1 / 08:16:45 sampleagent2                     ← 兩台，60 秒內
//
// 而當時的早報寫的是「sampleagent2 失聯 → 降級」「sampleagent4 失聯 → 降級」兩行，
// 一次都沒有合併。原因是判準讀 StateSince（現在這個狀態何時開始）而不是
// 真正的失聯時刻：sampleagent4 的 StateSince 指著 00:46、sampleagent2 的指著 08:17，
// 差七個半小時。
//
// 兩次眨眼要分別成一行，而且**不可以**併成一行 —— 併起來會在時間上說謊。
func TestTheRealNinthOfSeptemberFleetData(t *testing.T) {
	p := func(s string) time.Time {
		v, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	now := p("2026-09-04T15:47:00Z")
	in := Input{
		Now: now, Since: now.Add(-24 * time.Hour), Expected: 5,
		Machines: []Machine{
			{ID: "1", DisplayName: "samplehub1", State: state.Online, StateSince: p("2026-09-04T14:56:52Z"),
				Changed: true, PreviousState: state.Degraded,
				UnreachableAt: []time.Time{p("2026-09-04T00:46:45Z"), p("2026-09-04T08:15:45Z")}},
			{ID: "2", DisplayName: "sampleagent1", State: state.NeverReported, StateSince: p("2026-09-03T06:04:44Z")},
			{ID: "3", DisplayName: "sampleagent3", State: state.Degraded, Reason: "claude 登入已過期",
				StateSince: p("2026-09-03T06:05:44Z")},
			{ID: "4", DisplayName: "sampleagent4", State: state.Degraded, Reason: "claude 的登入在 14 天前就過期了",
				StateSince: p("2026-09-04T00:46:45Z"), Changed: true, PreviousState: state.Unreachable,
				UnreachableAt: []time.Time{p("2026-09-04T00:46:15Z")}},
			{ID: "5", DisplayName: "sampleagent2", State: state.Degraded, Reason: "grok 的登入在 3 天前就過期了",
				StateSince: p("2026-09-04T08:17:15Z"), Changed: true, PreviousState: state.Unreachable,
				UnreachableAt: []time.Time{p("2026-09-04T00:46:15Z"), p("2026-09-04T08:16:45Z")}},
		},
	}
	got := Render(in)

	// 兩次眨眼 = 兩行，不是一行也不是三行。
	if n := strings.Count(got, "Hub 自己沒在聽"); n != 2 {
		t.Errorf("資料庫裡有兩次同時失聯（00:46 三台、08:15 兩台），早報寫了 %d 行\n%s", n, got)
	}
	// ⚠⚠ 同秒失聯的順序必須穩定 —— sampleagent2 跟 sampleagent4 在實機上就是同一秒。
	//
	// sort.Slice 不保證相等元素的相對順序。不釘住的話，早報會在
	// 「sampleagent4、sampleagent2」跟「sampleagent2、sampleagent4」之間亂跳，
	// 而一則每天換句話說的早報，讀的人會以為機隊每天都在變。
	for i := 0; i < 20; i++ {
		if again := Render(in); again != got {
			t.Fatalf("同一份輸入 render 兩次不一樣 —— 早報每天會自己換句話說\n第一次：\n%s\n第 %d 次：\n%s",
				got, i+2, again)
		}
	}
	if !strings.Contains(got, "sampleagent2、sampleagent4") {
		t.Errorf("同秒失聯沒有按名字排出穩定順序\n%s", got)
	}
	// 每一行都要有時刻 —— 沒有的話兩行長得一樣，讀的人會以為早報在重複。
	for _, want := range []string{"00:46 起", "08:15 起"} {
		if !strings.Contains(got, want) {
			t.Errorf("少了 %q 那一次\n%s", want, got)
		}
	}
	// ⚠⚠ 而 sampleagent2 的 grok 憑證是**真的**過期了。
	// 第一版把整台跳過，等於因為觀測者出過包就把真的壞消息吞掉。
	if !strings.Contains(got, "grok 的登入在 3 天前就過期了") {
		t.Errorf("sampleagent2 的 grok 憑證真的過期了，這件事跟 Hub 有沒有在聽無關，不可以被吞掉\n%s", got)
	}
	// 但那個誤導人的箭頭要消失。
	if strings.Contains(got, "失聯 → ") {
		t.Errorf("還留著「失聯 →」—— 那個箭頭把 Hub 的失明講成了機器的病史\n%s", got)
	}
}

// ⚠⚠ 一台機器在窗口內抖兩次，不是兩台機器。
//
// 這一條是突變測試逼出來的：把去重拿掉，上面十支測試**沒有一支變紅**，
// 因為沒有任何一個 fixture 有「同一台在五分鐘內失聯兩次」。
// 而那正是最容易發生的真實情況 —— 一台網路爛的機器就長這樣。
//
// 沒有去重的話，早報會寫「samplehub1、samplehub1 在 2 分鐘內一起失聯 ——
// 同時失聯通常是 Hub 自己沒在聽」：一句語法通順的胡話，
// 而且它把一台機器自己的毛病栽到觀測者頭上。
func TestOneMachineFlappingTwiceIsNotTwoMachines(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	in := Input{
		Now: now, Since: now.Add(-24 * time.Hour), Expected: 2,
		Machines: []Machine{
			{ID: "a", DisplayName: "samplehub1", State: state.Degraded, Reason: "網路很爛",
				Changed: true, PreviousState: state.Unreachable, StateSince: at(now, -10*time.Minute),
				UnreachableAt: []time.Time{
					at(now, -14*time.Minute),
					at(now, -12*time.Minute), // 兩分鐘後又抖一次
				}},
			{ID: "b", DisplayName: "sampleagent2", State: state.Online, StateSince: at(now, -20*time.Hour)},
		},
	}
	got := Render(in)
	if strings.Contains(got, "Hub 自己沒在聽") {
		t.Errorf("一台機器抖兩次被算成兩台一起失聯 —— 那是把它自己的毛病栽給觀測者\n%s", got)
	}
	if strings.Contains(got, "samplehub1、samplehub1") {
		t.Errorf("同一個名字出現兩次 —— 一句語法通順的胡話\n%s", got)
	}
	if !strings.Contains(got, "samplehub1") {
		t.Errorf("那一台的名字必須還在，而且問題要算在它自己頭上\n%s", got)
	}
}

// ⚠⚠ 早報是早上八點送的，窗口往回 24 小時 ——
// 所以任何比現在晚的鐘點都是**昨天**的，而那是窗口的一半。
//
// 實機第一次印出來是「20:46 起」，指的是前一天晚上，
// 而字面上沒有任何東西這樣說。一個看起來精確、實際上少了一半資訊的時刻，
// 比沒有時刻更糟：它會讓人拿著錯的時間去翻 log。
func TestATimeFromYesterdaySaysSo(t *testing.T) {
	loc := time.FixedZone("EDT", -4*3600)
	now := time.Date(2026, 9, 4, 8, 0, 0, 0, loc)
	for _, c := range []struct {
		what string
		at   time.Time
		want string
	}{
		{"今天凌晨", time.Date(2026, 9, 4, 4, 15, 0, 0, loc), "04:15"},
		{"昨晚", time.Date(2026, 9, 3, 20, 46, 0, 0, loc), "昨天 20:46"},
		{"更久以前（--since 拉長時）", time.Date(2026, 9, 1, 9, 0, 0, 0, loc), "9/1 09:00"},
		// ⚠ 存進資料庫的是 UTC。轉不轉時區在這一行看得出來：
		// 2026-09-04T00:46Z = 前一天晚上 20:46 EDT。
		{"UTC 進來要轉成本地", time.Date(2026, 9, 4, 0, 46, 0, 0, time.UTC), "昨天 20:46"},
	} {
		if got := clockPhrase(c.at, now); got != c.want {
			t.Errorf("%s：clockPhrase = %q，要 %q", c.what, got, c.want)
		}
	}
}
