package probe

import (
	"context"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/teddashh/AI-Intune/internal/model"
)

// ---------------------------------------------------------------------------
// unit 的 journal 摘要
//
// ⚠⚠ 先讀這一段再改這個檔案。
//
// 這個檔案**不判斷任何 unit 的好壞**，而且不准開始判斷。
//
// docs/PRODUCT.md 地基二否決過「掃 log 關鍵字，看到 429 / Unauthorized 才算壞」，
// 理由是：**最痛的那種錯，定義上就是 log 裡沒有 error**。一個關鍵字表會把
// 「安靜地做錯事」判成健康 —— 那正是這整個專案最想避免的假綠燈。
//
// 我 2026-09-04 本來就是要寫那張關鍵字表的。實測資料看起來還很有說服力：
// sampleagent2 的 openclaw-watcher 一小時 38 行全是 `machine-mission.md: FAILED`。
// 但我拿來當證據的那兩個字串，剛好就是 PRODUCT.md 用來舉例「這樣做是錯的」
// 的那兩個字串（429、Unauthorized）。
//
// 所以這裡只做兩件**沒有語意**的事：
//
//  1. 數行數 —— 一個 unit 在窗口裡講了幾句話。這是量測，不是評價。
//  2. 把行按「句型」分組 —— 只把時間戳、UUID、十六進位、數字換成佔位符，
//     其餘一個字都不動。這是為了讓 7180 行變成人讀得完的 16 種，
//     不是為了判斷哪一種是壞的。
//
// 誰是好句型誰是壞句型，交給看畫面的人。跟 store.MachineReadEvidence
// 的 RunSummaries 同一個規矩：
// 原文照搬，存在的唯一理由是讓人自己讀。
//
// ⚠ 為什麼一定要正規化才數得準（實測，2026-09-04）：
// sampleagent4 的 openclaw-gateway 一小時 7180 行，**相異行數也是 7180**。
// 看起來像「每一行都在講不同的事」，其實是因為那支程式會自己印時間戳，
// 所以每一行必然不同。正規化之後收斂成 16 種。
// 「相異率」量到的是「這支程式有沒有自己印時間戳」，不是它在做什麼。
//
// ⚠ 而且相異率低**也不代表壞**：同一輪實測裡 sampleagent2 的 clawctl-agent
// 正規化後也只有 1 種句型，它完全正常。重複不是病。
// ---------------------------------------------------------------------------

const (
	// journalWindow 是每次觀測回看多久。一小時讓「行數」讀起來就是「每小時多少行」。
	journalWindow = time.Hour

	// journalMaxLines 是單一 unit 單次讀取的行數上限。撞到上限要說出來
	// （Truncated），不可以讓一個被截斷的數字看起來像真的總數。
	journalMaxLines = 2000

	// journalTopShapes 是每個 unit 留幾種最常見的句型。
	// ⚠ 這個數字是**資料庫大小**的直接乘數：3 種 × 160 字 × 5 units × 4 台
	// × 每 10 分鐘一次 × 存 30 天 ≈ 43 MB。上一輪才因為占用證據存兩份讓
	// 資料庫 92% 是同一批東西，這裡先保守，部署後量了再說。
	journalTopShapes = 3

	// journalExampleMax 是原文範例的長度上限（位元組，切在 UTF-8 邊界上）。
	journalExampleMax = 160

	// journalCmdTimeout 比一般的 cmdTimeout 寬：journalctl 掃一小時的量
	// 在忙的機器上不是瞬間。實測 sampleagent4 一小時 7180 行。
	journalCmdTimeout = 20 * time.Second
)

// ---------------------------------------------------------------- 遮蔽

// redactors 是「看起來像密鑰就遮掉」的規則，**無條件套用**。
//
// ⚠ 不要因為「我剛剛看過，沒有密鑰」就把這一段拿掉。2026-09-04 我在 sampleagent4
// 的 openclaw-gateway 抽了 400 行，結果是 0 個疑似密鑰；接著把窗口拉到
// 全機隊 24 小時，變成 409 行。**那 400 行的乾淨答案是抽樣抽出來的，不是真的。**
// （後來查清楚 409 行全是 UUID，沒有真密鑰 —— 但那是這一次的結論，
// journal 的內容不歸我們管，下一次不一定。）
var redactors = []struct {
	re   *regexp.Regexp
	with string
}{
	// Telegram bot token：`bot123456789:AA...`。這是本專案告警鏈自己用的格式。
	{regexp.MustCompile(`bot[0-9]{6,}:[A-Za-z0-9_-]{20,}`), "bot<REDACTED>"},
	{regexp.MustCompile(`sk-(?:ant-)?[A-Za-z0-9_-]{20,}`), "sk-<REDACTED>"},
	{regexp.MustCompile(`(?i)bearer [A-Za-z0-9._-]{20,}`), "Bearer <REDACTED>"},
	{regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{30,}`), "<REDACTED>"},
	{regexp.MustCompile(`AIza[A-Za-z0-9_-]{30,}`), "<REDACTED>"},

	// 兜底：夠長的無斷點字串。
	// ⚠ 字元集裡**故意沒有 `/` 跟 `.`** —— 有的話
	// `/home/example-user/projects/AI-Intune/internal/probe` 這種路徑會整段被遮掉，
	// 那會把最有用的一半訊息一起殺死。
	{regexp.MustCompile(`[A-Za-z0-9_+=-]{40,}`), "<REDACTED>"},
}

// uuidRe 認得標準 UUID。它在 redact 裡的角色是**保護**，不是遮蔽。
var uuidRe = regexp.MustCompile(`\b[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}\b`)

func redact(s string) string {
	// ⚠⚠ 先把 UUID 收起來，遮完再放回去。
	//
	// 為什麼需要這一步（2026-09-04 被測試抓到）：我本來在註解裡寫
	// 「UUID 36 字，低於 40 的門檻，所以不會被兜底規則咬到」。
	// 那是拿 UUID **單獨**去比門檻算出來的，而真實資料裡 UUID 從來不單獨出現。
	// samplehub1 的 journal 裡長這樣：`runId=3abea9dc-f9b1-4937-939c-4a1b2c3d4e5f`
	// —— `runId=` 六個字加上去就是 42 字，而 `=` 跟 `-` 都在兜底規則的字元集裡，
	// 於是整串被遮成 `<REDACTED>`。
	//
	// 一個在真空裡成立的推理，碰到真實資料就不成立了。
	var saved []string
	s = uuidRe.ReplaceAllStringFunc(s, func(m string) string {
		saved = append(saved, m)
		// \x00 不在任何一條規則的字元集裡，所以這個佔位符會把長字串切斷，
		// 不會自己觸發兜底規則。
		return "\x00U" + strconv.Itoa(len(saved)-1) + "\x00"
	})
	for _, r := range redactors {
		s = r.re.ReplaceAllString(s, r.with)
	}
	for i, u := range saved {
		s = strings.ReplaceAll(s, "\x00U"+strconv.Itoa(i)+"\x00", u)
	}
	return s
}

// ---------------------------------------------------------------- 正規化

// ⚠ 順序有意義，由寬到窄：時間戳裡面有數字也有連字號，先換掉它，
// 否則會被後面的數字規則咬成碎片，同一個時間格式會分裂成好幾種句型。
var canonizers = []struct {
	re   *regexp.Regexp
	with string
}{
	{regexp.MustCompile(`\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:?\d{2})?`), "<TS>"},
	{regexp.MustCompile(`\b[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}\b`), "<UUID>"},
	{regexp.MustCompile(`\b[0-9a-fA-F]{8,}\b`), "<HEX>"},
	{regexp.MustCompile(`\d+`), "<N>"},
}

// canonical 把一行變成它的「句型」。
//
// ⚠⚠ 回傳值**只用來分組，永遠不要顯示給人看，也永遠不要存進資料庫**。
// 人要看的是原文（redact 過的）。句型是內部的分組鍵，它長得很醜而且會誤導：
// `<N>` 蓋掉的可能是「重試 3 次」也可能是「重試 30000 次」。
func canonical(s string) string {
	for _, c := range canonizers {
		s = c.re.ReplaceAllString(s, c.with)
	}
	return strings.TrimSpace(s)
}

// ---------------------------------------------------------------- 摘要

// summarize 把窗口內的原始行變成 model.UnitJournal。
//
// 這是這個檔案裡唯一有邏輯的地方，也是唯一值得測的地方 ——
// 讀 journalctl 那一段只是搬運。
func summarize(unit string, lines []string, window time.Duration, truncated bool) model.UnitJournal {
	j := model.UnitJournal{
		Unit:      unit,
		WindowSec: int(window.Seconds()),
		Truncated: truncated,
	}

	type acc struct {
		count int
		last  string // 最後一次出現的原文（已遮蔽）
	}
	groups := map[string]*acc{}

	for _, ln := range lines {
		// ⚠ 空行不算「一句話」。journalctl 的輸出尾端常有一個空行，
		// 把它算進去會讓每個 unit 的行數都固定多一。
		if strings.TrimSpace(ln) == "" {
			continue
		}
		j.Lines++

		clean := redact(ln)
		key := canonical(clean)
		g := groups[key]
		if g == nil {
			g = &acc{}
			groups[key] = g
		}
		g.count++
		// 留最後一次 —— 對著畫面查問題的人要的是最近的那一行，不是一小時前的。
		g.last = truncateRunes(clean, journalExampleMax)
	}
	j.Shapes = len(groups)

	top := make([]model.JournalShape, 0, len(groups))
	for _, g := range groups {
		top = append(top, model.JournalShape{Count: g.count, Example: g.last})
	}
	// ⚠ 次數相同要有穩定的順序，否則同一批資料每次算出來的排序不一樣，
	// 畫面會無故跳動、測試會偶爾紅。sort.Slice 不保證相等元素的相對次序，
	// 所以第二個鍵要能決勝負 —— 用原文字典序。
	// （上一輪同秒失聯的排序就是栽在這裡。）
	sort.Slice(top, func(a, b int) bool {
		if top[a].Count != top[b].Count {
			return top[a].Count > top[b].Count
		}
		return top[a].Example < top[b].Example
	})
	if len(top) > journalTopShapes {
		top = top[:journalTopShapes]
	}
	j.Top = top
	return j
}

// summarizeErr 是「這個 unit 的 journal 我們沒讀到」。
//
// ⚠⚠ 讀不到要出聲。安靜地回一個 Lines=0 的摘要，在畫面上會跟
// 「這個 unit 很安靜」長得一模一樣 —— 那是兩件完全相反的事，
// 一個是「它沒話說」，一個是「我們沒在聽」。
func summarizeErr(unit, msg string) model.UnitJournal {
	return model.UnitJournal{
		Unit:      unit,
		WindowSec: int(journalWindow.Seconds()),
		Err:       truncateRunes(redact(msg), journalExampleMax),
	}
}

// truncateRunes 切在 UTF-8 邊界上，不會把一個中文字切成半個。
//
// ⚠ 省略號要算進預算裡。「…」在 UTF-8 是 **3 個位元組**不是 1 個 ——
// 2026-09-04 第一版把它當 1 個算，160 的上限實際吐出 162。
func truncateRunes(s string, max int) string {
	if len(s) <= max {
		return s
	}
	const ellipsis = "…"
	r := []rune(s)
	for len(r) > 0 && len(string(r))+len(ellipsis) > max {
		r = r[:len(r)-1]
	}
	return string(r) + ellipsis
}

// ---------------------------------------------------------------- 讀取

// unitJournals 讀每個 present 的 unit 最近 journalWindow 的輸出並做摘要。
//
// ⚠ 只讀 Present 的 unit：沒裝的 unit 沒有現在式的 journal 可讀，
// 而為了它多跑一次 journalctl 是純浪費。
func unitJournals(ctx context.Context, units []model.Unit) []model.UnitJournal {
	out := make([]model.UnitJournal, 0, len(units))
	for _, u := range units {
		if !u.Present {
			continue
		}
		out = append(out, readJournal(ctx, u.Name))
	}
	return out
}

func readJournal(ctx context.Context, unit string) model.UnitJournal {
	// ⚠ unit 名字要帶 `.service`。實測 `journalctl --user -u openclaw-watcher`
	// 會回 "No entries"，而 `-u openclaw-watcher.service` 回 38 行 ——
	// 一個問錯問題得到的、看起來像「什麼都沒發生」的乾淨答案。
	stdout, stderr, err := run(ctx, journalCmdTimeout, "journalctl",
		"--user", "-u", unit,
		"--since", "-"+journalWindow.String(),
		"-n", strconv.Itoa(journalMaxLines),
		"--no-pager", "-o", "cat")
	if err != nil {
		msg := strings.TrimSpace(stderr)
		if msg == "" {
			msg = err.Error()
		}
		return summarizeErr(unit, msg)
	}
	lines := strings.Split(stdout, "\n")
	// 撞到 -n 上限就代表窗口裡可能還有更多，行數是下界不是總數。
	return summarize(unit, lines, journalWindow, countNonEmpty(lines) >= journalMaxLines)
}

func countNonEmpty(lines []string) int {
	n := 0
	for _, l := range lines {
		if strings.TrimSpace(l) != "" {
			n++
		}
	}
	return n
}
