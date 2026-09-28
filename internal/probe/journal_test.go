package probe

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/model"
)

// 這一批測試守的是一條**產品線**，不是一段程式：
// journal 摘要不准變成「掃 log 關鍵字判健康」。
// 見 journal.go 檔頭與 docs/PRODUCT.md 地基二。

// ---------------------------------------------------------------- 遮蔽

func TestSecretShapesNeverSurviveIntoTheExample(t *testing.T) {
	// ⚠ 這些是**假的**測試值，不是實機上的密鑰。實機 2026-09-04 掃過，
	// 24 小時內 409 個疑似命中全是 UUID，沒有真密鑰。
	cases := []struct {
		name, line, mustNotContain string
	}{
		{"telegram bot token",
			`sendMessage failed: https://api.telegram.org/bot123456789:AAFakeTokenValueForTestingOnly01/send`,
			"AAFakeTokenValueForTestingOnly01"},
		{"anthropic key",
			`auth error using sk-ant-api03-FakeKeyForTestingPurposesOnly99`,
			"FakeKeyForTestingPurposesOnly99"},
		{"bearer header",
			`retry with Authorization: Bearer FakeBearerTokenForTestingOnly123`,
			"FakeBearerTokenForTestingOnly123"},
		{"github token",
			`clone failed: ghp_FakeGithubTokenForTestingPurposes123456`,
			"ghp_FakeGithubTokenForTestingPurposes123456"},
		{"google key",
			`AIzaFakeGoogleApiKeyForTestingPurposes1234 rejected`,
			"AIzaFakeGoogleApiKeyForTestingPurposes1234"},
		{"long opaque blob",
			`state=QUJDREVGR0hJSktMTU5PUFFSU1RVVldYWVowMTIzNDU2Nzg5YWJjZGVm rejected`,
			"QUJDREVGR0hJSktMTU5PUFFSU1RVVldYWVowMTIzNDU2Nzg5YWJjZGVm"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			j := summarize("u.service", []string{c.line}, time.Hour, false)
			if len(j.Top) != 1 {
				t.Fatalf("想要 1 種句型，得到 %d", len(j.Top))
			}
			if strings.Contains(j.Top[0].Example, c.mustNotContain) {
				t.Fatalf("密鑰活著進了 Example：%q", j.Top[0].Example)
			}
			if !strings.Contains(j.Top[0].Example, "REDACTED") {
				t.Fatalf("沒有遮蔽的痕跡，可能整條規則沒生效：%q", j.Top[0].Example)
			}
		})
	}
}

// ⚠ 遮蔽太寬跟遮蔽太窄一樣糟：一行被遮到只剩 <REDACTED> 的訊息，
// 對著畫面查問題的人一個字都用不到。
func TestRedactionKeepsTheHalfOfTheLineThatIsUseful(t *testing.T) {
	line := `2026-09-04T13:06:06.237-04:00 [telegram] sendMessage failed for /home/example-user/projects/AI-Intune/internal/probe: 403 Forbidden`
	got := redact(line)
	for _, keep := range []string{"[telegram]", "sendMessage failed", "403 Forbidden",
		"/home/example-user/projects/AI-Intune/internal/probe"} {
		if !strings.Contains(got, keep) {
			t.Fatalf("遮蔽把有用的部分吃掉了，%q 不見了：\n%s", keep, got)
		}
	}
}

// UUID 不是密鑰。它們該由正規化收斂，不該被遮成 <REDACTED> ——
// 被遮掉的話畫面上會看到一堆 <REDACTED>，人會以為系統在藏東西。
func TestUUIDsAreNotTreatedAsSecrets(t *testing.T) {
	got := redact(`runId=3abea9dc-f9b1-4937-939c-4a1b2c3d4e5f failed`)
	if strings.Contains(got, "REDACTED") {
		t.Fatalf("UUID 被當成密鑰遮掉了：%s", got)
	}
}

// ---------------------------------------------------------------- 正規化

// 這一支釘住 2026-09-04 實測到的那個混淆：
// sampleagent4 的 openclaw-gateway 一小時 7180 行、相異也是 7180 ——
// 不是因為每行在講不同的事，是因為那支程式自己印時間戳。
func TestLinesThatDifferOnlyByTheirOwnTimestampAreOneShape(t *testing.T) {
	var lines []string
	for i := 0; i < 50; i++ {
		lines = append(lines, `2026-09-04T13:06:`+pad(i)+`.237-04:00 [telegram] [diag] spooled update `+
			itoaFast(482927845+i)+` failed; keeping for retry`)
	}
	j := summarize("openclaw-gateway.service", lines, time.Hour, false)
	if j.Lines != 50 {
		t.Fatalf("行數想要 50，得到 %d", j.Lines)
	}
	if j.Shapes != 1 {
		t.Fatalf("想要收斂成 1 種句型，得到 %d —— 正規化沒吃掉時間戳或流水號", j.Shapes)
	}
	if j.Top[0].Count != 50 {
		t.Fatalf("那一種句型想要 50 次，得到 %d", j.Top[0].Count)
	}
}

// ⚠ 反面：正規化不准把**真的不同**的事情併成一種。
// 全部併成一種的正規化能讓上面那支測試過，但它會讓畫面只剩一行。
func TestGenuinelyDifferentMessagesStayDifferentShapes(t *testing.T) {
	j := summarize("u.service", []string{
		`2026-09-04T13:06:06.237-04:00 [telegram] sendMessage failed: 403 Forbidden`,
		`2026-09-04T13:06:07.100-04:00 [telegram] [diag] spooled update failed; keeping for retry`,
		`2026-09-04T13:06:08.000-04:00 [diagnostics/memory] memory pressure normal`,
	}, time.Hour, false)
	if j.Shapes != 3 {
		t.Fatalf("三句不同的話想要 3 種句型，得到 %d", j.Shapes)
	}
}

// ---------------------------------------------------------------- 摘要

// 真實資料：sampleagent2 的 openclaw-watcher（AI-Intune 的前身），
// 2026-09-04 實測一小時 38 行，全部同一句。
// clawctl 的 unit 表對它整片綠燈：active/running、NRestarts=0、
// ActiveEnterTimestamp 停在 2026-06-15 不動。
func TestTheRealStuckWatcherIsVisibleAsOneShapeRepeating(t *testing.T) {
	lines := make([]string, 38)
	for i := range lines {
		lines[i] = "machine-mission.md: FAILED"
	}
	j := summarize("openclaw-watcher.service", lines, time.Hour, false)
	if j.Lines != 38 || j.Shapes != 1 {
		t.Fatalf("想要 38 行 / 1 種，得到 %d 行 / %d 種", j.Lines, j.Shapes)
	}
	if j.Top[0].Example != "machine-mission.md: FAILED" {
		t.Fatalf("原文被改動了：%q", j.Top[0].Example)
	}
	if j.Top[0].Count != 38 {
		t.Fatalf("想要 38 次，得到 %d", j.Top[0].Count)
	}
}

// ⚠⚠ 這一支是這整個檔案的重點，它守的是**不要退回關鍵字表**。
//
// sampleagent2 的 clawctl-agent 實測一小時 6 行、正規化後 1 種句型，完全正常；
// 同一台的 openclaw-watcher 38 行、1 種句型，是真的卡死。
// 兩者在「行數 / 句型數」這個比值上分不出來 —— 所以這個型別不准長出
// 「重複率高 = 壞」的欄位。誰壞誰好由看畫面的人決定。
func TestAHealthyRepetitiveUnitLooksTheSameAsAStuckOne(t *testing.T) {
	healthy := summarize("clawctl-agent.service", repeat(
		"clawctl-agent.service: Got notification message from PID 11037", 6), time.Hour, false)
	stuck := summarize("openclaw-watcher.service", repeat(
		"machine-mission.md: FAILED", 38), time.Hour, false)

	if healthy.Shapes != stuck.Shapes {
		t.Fatalf("這兩個在句型數上本來就該一樣（都是 1）：%d vs %d", healthy.Shapes, stuck.Shapes)
	}
	// 型別上不准有任何「誰比較健康」的答案。這一段用結構把它釘住：
	// 如果將來有人加了 Healthy / Severity / ErrorCount 之類的欄位，
	// 這句話就會變成謊，而下面那支反射測試會紅。
	// ⚠ 這句雖點名 ErrorCount，改用子字串前 helper 卻抓不到它：Healthy
	// 剛好與禁用詞相等所以攔得住，ErrorCount 不等於任何一項所以放行；
	// 註解對它自己的看守者曾經是假的。
	assertNoVerdictFields(t, healthy)
	assertNoVerdictFields(t, stuck)
}

// ---------------------------------------------------------------- 誠實

// 撞到行數上限時，Lines 是下界。它必須說出來，否則一個被截斷的數字
// 看起來會跟真的總數一模一樣。
func TestHittingTheLineCapIsAdmitted(t *testing.T) {
	j := summarize("u.service", repeat("x", 10), time.Hour, true)
	if !j.Truncated {
		t.Fatal("截斷了卻沒說")
	}
}

// ⚠ 讀不到 journal 跟「這個 unit 很安靜」在 Lines=0 上長得一模一樣。
// 分辨兩者的唯一憑據是 Err，所以 Err 不准被吞掉。
func TestAFailedReadIsNotTheSameAsAQuietUnit(t *testing.T) {
	quiet := summarize("u.service", nil, time.Hour, false)
	if quiet.Lines != 0 || quiet.Err != "" {
		t.Fatalf("安靜的 unit 想要 0 行且無錯，得到 %d 行 err=%q", quiet.Lines, quiet.Err)
	}
	// 讀取失敗那一路由 readJournal 產生，這裡直接檢查契約：
	// Err 非空時，Lines=0 不可以被讀成「很安靜」。
	failed := summarizeErr("u.service", "Failed to add match: Invalid argument")
	if failed.Err == "" {
		t.Fatal("讀取失敗卻沒有留下原因")
	}
	if failed.Lines != 0 {
		t.Fatalf("讀取失敗時不該報出行數，得到 %d", failed.Lines)
	}
}

// journalctl 的輸出尾端常有一個空行。把它算成一句話會讓每個 unit 的
// 行數都固定多一 —— 一個安靜的 unit 會永遠顯示「1 行」。
func TestTheTrailingBlankLineIsNotAMessage(t *testing.T) {
	j := summarize("u.service", []string{"only real line", ""}, time.Hour, false)
	if j.Lines != 1 {
		t.Fatalf("想要 1 行，得到 %d", j.Lines)
	}
	if j.Shapes != 1 {
		t.Fatalf("想要 1 種句型，得到 %d —— 空行變成了一種句型", j.Shapes)
	}
}

// 次數相同的句型要有穩定的排序，否則畫面會無故跳動、測試會偶爾紅。
// （上一輪同秒失聯的排序就是栽在 sort.Slice 不保證相等元素次序。）
func TestEqualCountsSortStably(t *testing.T) {
	lines := []string{"bbb", "aaa", "ccc"}
	var first string
	for i := 0; i < 20; i++ {
		j := summarize("u.service", lines, time.Hour, false)
		got := j.Top[0].Example + "|" + j.Top[1].Example + "|" + j.Top[2].Example
		if i == 0 {
			first = got
		} else if got != first {
			t.Fatalf("第 %d 次算出不同的排序：%q ≠ %q", i, got, first)
		}
	}
	if first != "aaa|bbb|ccc" {
		t.Fatalf("同次數時想要用原文字典序決勝負，得到 %q", first)
	}
}

// 只留前 N 種，但 Shapes 要講**全部**有幾種 —— 否則「16 種」會被截成「3 種」，
// 而人會以為這個 unit 只說過三種話。
func TestShapesCountsAllOfThemEvenWhenOnlyAFewAreKept(t *testing.T) {
	var lines []string
	for i := 0; i < 16; i++ {
		lines = append(lines, "distinct message "+string(rune('a'+i)))
	}
	j := summarize("u.service", lines, time.Hour, false)
	if j.Shapes != 16 {
		t.Fatalf("想要 16 種，得到 %d", j.Shapes)
	}
	if len(j.Top) != journalTopShapes {
		t.Fatalf("想要留 %d 種，得到 %d", journalTopShapes, len(j.Top))
	}
}

// 長行要切在 UTF-8 邊界上，不可以把一個中文字切成半個。
func TestLongLinesAreCutOnRuneBoundaries(t *testing.T) {
	long := strings.Repeat("這是一句很長的中文訊息", 40)
	j := summarize("u.service", []string{long}, time.Hour, false)
	got := j.Top[0].Example
	if len(got) > journalExampleMax {
		t.Fatalf("超過長度上限：%d > %d", len(got), journalExampleMax)
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("切掉了卻沒有省略號：%q", got)
	}
	if strings.ContainsRune(got, '�') {
		t.Fatalf("切在字元中間，出現了替換字元：%q", got)
	}
}

// ---------------------------------------------------------------- helpers

// verdictSubstrings 是判定語意的詞根；用子字串才能攔住 ErrorCount 這類複合欄位名。
var verdictSubstrings = []string{
	"healthy", "health", "status", "severity", "error",
	"failed", "failure", "verdict", "score", "level",
}

// verdictExactNames 是只可精確命中的短詞，避免一般欄位名被片段誤判。
var verdictExactNames = []string{"ok"}

// isVerdictFieldName 執行 internal/model/model.go:443-455 的「不准長出健康欄位」界線。
// 判定詞根採子字串比對，而非只比對完整名稱，才能涵蓋 ErrorCount 這類複合欄位名。
// ⚠ 在 model.UnitJournal 加 ErrorCount int：
// TestAHealthyRepetitiveUnitLooksTheSameAsAStuckOne 紅，全樹沒有別的紅。
// 改成子字串比對之前，同一個突變是全綠的。
// 對照組：加一個無害的 WindowEndSec int：全綠。
// 規則禁的是詞彙，不是欄位成長。
// 對照組：加一個 Token string：全綠。
// 這一格就是 ok 必須維持相等比對的證據。
// 把子字串改回相等：只有 TestVerdictFieldNameRuleCatchesCompoundNames 紅。
// 那支表格測試是這條規則本身唯一的看守者。
func isVerdictFieldName(name string) bool {
	name = strings.ToLower(name)
	for _, substring := range verdictSubstrings {
		if strings.Contains(name, substring) {
			return true
		}
	}
	for _, exactName := range verdictExactNames {
		// ok 只做相等比對，否則 Token、Lookback、Broken 等名稱都會被誤判。
		if name == exactName {
			return true
		}
	}
	return false
}

func TestVerdictFieldNameRuleCatchesCompoundNames(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  bool
	}{
		{name: "舊規則抓不到的 ErrorCount", input: "ErrorCount", want: true},
		{name: "Healthy", input: "Healthy", want: true},
		{name: "Severity", input: "Severity", want: true},
		{name: "複合詞 HealthScore", input: "HealthScore", want: true},
		{name: "OK 走精確比對", input: "OK", want: true},
		{name: "Truncated", input: "Truncated", want: false},
		{name: "Shapes", input: "Shapes", want: false},
		{name: "Err 是讀取失敗原因而非判決", input: "Err", want: false},
		{name: "Token 證明 ok 不可用子字串", input: "Token", want: false},
		{name: "WindowSec", input: "WindowSec", want: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := isVerdictFieldName(tc.input)
			if got != tc.want {
				t.Fatalf("isVerdictFieldName(%q) = %t，想要 %t；判定規則在命中與放行之間選錯了", tc.input, got, tc.want)
			}
		})
	}
}

// assertNoVerdictFields 用反射守住一條**設計上的**界線，不是一個值：
// model.UnitJournal 不准長出任何「這個 unit 好不好」的欄位。
//
// 為什麼值得寫成測試而不是只寫註解：這個檔案存在的全部理由，就是我
// 2026-09-04 差一點寫成關鍵字表。註解攔不住下一個人（或下一個我）——
// 加一個 ErrorCount 欄位是十秒鐘的事，而它會把 PRODUCT.md 地基二
// 否決過的東西悄悄放回來。
func assertNoVerdictFields(t *testing.T, j model.UnitJournal) {
	t.Helper()
	rt := reflect.TypeOf(j)
	for i := 0; i < rt.NumField(); i++ {
		if isVerdictFieldName(rt.Field(i).Name) {
			t.Fatalf("model.UnitJournal 長出了判定欄位 %q。"+
				"journal 不准判定 unit 的好壞 —— 見 journal.go 檔頭與 PRODUCT.md 地基二",
				rt.Field(i).Name)
		}
	}
}

func repeat(s string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = s
	}
	return out
}

func pad(i int) string {
	if i < 10 {
		return "0" + itoaFast(i)
	}
	return itoaFast(i % 60)
}

func itoaFast(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

// ---------------------------------------------------------------- 實機

// TestJournalAgainstRealCapture 拿**實機抓下來的原始 journal** 跑一遍摘要。
//
// ⚠⚠ 這支測試存在的理由：上一輪我寫了九支綠燈測試，結果在正式環境一次都沒觸發，
// 因為每一個 fixture 都是照著「我以為世界長什麼樣」造的。
// fixture 是我寫的，它不是第三方。
//
//	CLAWCTL_JOURNAL_CAPTURE=/tmp/sampleagent4.log go test ./internal/probe/ -run RealCapture -v
func TestJournalAgainstRealCapture(t *testing.T) {
	path := os.Getenv("CLAWCTL_JOURNAL_CAPTURE")
	if path == "" {
		t.Skip("設 CLAWCTL_JOURNAL_CAPTURE=<實機 journal 檔> 才跑")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(raw), "\n")
	j := summarize(filepath.Base(path), lines, time.Hour, false)

	t.Logf("%s：%d 行 → %d 種句型", filepath.Base(path), j.Lines, j.Shapes)
	for i, s := range j.Top {
		t.Logf("  [%d] ×%-5d %s", i+1, s.Count, s.Example)
	}

	if j.Lines == 0 {
		t.Fatal("實機檔案裡一行都沒讀到 —— 先確認抓檔那一步")
	}
	// 不變式，不是「我猜的數字」：
	if j.Shapes > j.Lines {
		t.Fatalf("句型數不可能多於行數：%d > %d", j.Shapes, j.Lines)
	}
	total := 0
	for _, s := range j.Top {
		total += s.Count
	}
	if total > j.Lines {
		t.Fatalf("前幾種句型的次數加起來超過總行數：%d > %d", total, j.Lines)
	}
	// ⚠ 實機原文裡不准有東西活著穿過遮蔽。
	for _, s := range j.Top {
		for _, re := range []*regexp.Regexp{
			regexp.MustCompile(`bot[0-9]{6,}:[A-Za-z0-9_-]{20,}`),
			regexp.MustCompile(`sk-(?:ant-)?[A-Za-z0-9_-]{20,}`),
			regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{30,}`),
		} {
			if re.MatchString(s.Example) {
				t.Fatalf("實機原文有密鑰形狀活著進了 Example")
			}
		}
	}
}
