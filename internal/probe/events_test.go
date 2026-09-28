package probe

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/model"
)

var evNow = time.Date(2026, 9, 4, 21, 0, 0, 0, time.UTC)

// writeEvents 寫一個 JSONL 檔並回它的路徑。
func writeEvents(t *testing.T, lines ...string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "events.jsonl")
	body := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func evLine(ts, name string) string {
	return fmt.Sprintf(`{"ts":%q,"event":%q,"actor":"watcher"}`, ts, name)
}

func evRule(path string, notOK []string, window int) model.Expectation {
	return model.Expectation{
		Machine: "*", Unit: "w.service", Artifact: path,
		MaxAgeSeconds: 900, Why: "測試",
		Events: &model.EventSpec{
			TsField: "ts", TypeField: "event", NotOK: notOK, WindowSeconds: window,
		},
	}
}

func readOne(t *testing.T, r model.Expectation) model.EventStream {
	t.Helper()
	out := CheckEvents([]model.Expectation{r}, evNow)
	if len(out) != 1 {
		t.Fatalf("要 1 筆，得到 %d", len(out))
	}
	return out[0]
}

func declaredCount(s model.EventStream, name string) (int, bool) {
	for _, d := range s.Declared {
		if d.Type == name {
			return d.Count, true
		}
	}
	return 0, false
}

// --- 沒有 EventSpec 的規則完全不該被讀 ---

func TestCheckEventsSkipsRulesWithoutSpec(t *testing.T) {
	r := evRule("/nonexistent/nope.jsonl", []string{"bad"}, 3600)
	r.Events = nil
	if out := CheckEvents([]model.Expectation{r}, evNow); len(out) != 0 {
		t.Fatalf("沒宣告事件流的規則不該產生量測，得到 %d 筆", len(out))
	}
}

// --- 基本計數與窗口 ---

func TestEventsCountsWithinWindow(t *testing.T) {
	p := writeEvents(t,
		evLine("2026-09-04T20:30:00Z", "baseline_hash_mismatch"), // 窗口內
		evLine("2026-09-04T20:45:00Z", "baseline_hash_mismatch"), // 窗口內
		evLine("2026-09-04T17:00:00Z", "baseline_hash_mismatch"), // 4 小時前，窗口外
		evLine("2026-09-04T20:50:00Z", "watcher_heartbeat"),
	)
	s := readOne(t, evRule(p, []string{"baseline_hash_mismatch"}, 3600))

	if n, ok := declaredCount(s, "baseline_hash_mismatch"); !ok || n != 2 {
		t.Fatalf("窗口內應該是 2 次，得到 %d（found=%v）", n, ok)
	}
	if s.Declared[0].LastAt == nil || !s.Declared[0].LastAt.Equal(
		time.Date(2026, 9, 4, 20, 45, 0, 0, time.UTC)) {
		t.Fatalf("LastAt 要是窗口內最後一次：%v", s.Declared[0].LastAt)
	}
	if len(s.Undeclared) != 1 || s.Undeclared[0].Type != "watcher_heartbeat" {
		t.Fatalf("watcher_heartbeat 該落在 undeclared：%+v", s.Undeclared)
	}
}

// ⚠ 這一條擋的是最惡劣的假綠燈：宣告過但這次沒出現的事件名**必須在列表裡**，
// 否則「這種錯誤沒發生」跟「我根本沒在看這種錯誤」會長得一模一樣。
func TestEventsDeclaredButAbsentStillListed(t *testing.T) {
	p := writeEvents(t, evLine("2026-09-04T20:30:00Z", "watcher_heartbeat"))
	s := readOne(t, evRule(p, []string{"baseline_hash_mismatch", "deploy_failed"}, 3600))

	if len(s.Declared) != 2 {
		t.Fatalf("兩個宣告過的名字都要在，得到 %+v", s.Declared)
	}
	for _, d := range s.Declared {
		if d.Count != 0 {
			t.Fatalf("%s 不該有次數：%d", d.Type, d.Count)
		}
		if d.LastAt != nil {
			t.Fatalf("%s 沒出現過就不該有 LastAt", d.Type)
		}
	}
}

// --- 欄位名寫錯：必須吵，不准安靜 ---

// ⚠⚠ 這是這整個功能最危險的失敗模式：欄位名打錯 → 一種事件都讀不到 →
// 畫面一片乾淨。它必須以 Malformed 的形式大聲講出來。
func TestEventsWrongFieldNamesAreLoud(t *testing.T) {
	p := writeEvents(t,
		evLine("2026-09-04T20:30:00Z", "baseline_hash_mismatch"),
		evLine("2026-09-04T20:31:00Z", "baseline_hash_mismatch"),
	)
	r := evRule(p, []string{"baseline_hash_mismatch"}, 3600)
	r.Events.TypeField = "kind" // 真實欄位叫 event
	s := readOne(t, r)

	if n, _ := declaredCount(s, "baseline_hash_mismatch"); n != 0 {
		t.Fatalf("欄位名錯了不該讀到任何事件，得到 %d", n)
	}
	if s.Malformed != 2 {
		t.Fatalf("欄位名錯了要把每一行算成讀不懂，Malformed=%d 要 2", s.Malformed)
	}
	if s.CoveredFrom != nil {
		t.Fatalf("一行都沒解析成功就不該宣稱涵蓋了任何時間：%v", s.CoveredFrom)
	}
}

func TestEventsBadTimestampCountsMalformed(t *testing.T) {
	p := writeEvents(t,
		`{"ts":"not-a-time","event":"baseline_hash_mismatch"}`,
		`{"event":"baseline_hash_mismatch"}`, // 沒有 ts
		`not json at all`,
		evLine("2026-09-04T20:30:00Z", "baseline_hash_mismatch"),
	)
	s := readOne(t, evRule(p, []string{"baseline_hash_mismatch"}, 3600))
	if s.Malformed != 3 {
		t.Fatalf("Malformed 要 3，得到 %d", s.Malformed)
	}
	if n, _ := declaredCount(s, "baseline_hash_mismatch"); n != 1 {
		t.Fatalf("好的那一行還是要算到，得到 %d", n)
	}
}

func TestEventsAcceptsUnixSeconds(t *testing.T) {
	ts := evNow.Add(-10 * time.Minute).Unix()
	p := writeEvents(t, fmt.Sprintf(`{"ts":%d,"event":"boom"}`, ts))
	s := readOne(t, evRule(p, []string{"boom"}, 3600))
	if n, _ := declaredCount(s, "boom"); n != 1 {
		t.Fatalf("unix 秒也要收，得到 %d（malformed=%d）", n, s.Malformed)
	}
}

// --- tail 讀取：截斷、半行、涵蓋範圍 ---

// ⚠ 從檔案中間開始讀，第一行幾乎一定是斷的。它必須被丟掉而不是算成
// Malformed —— 否則每一次讀取都會謊報一行壞資料。
func TestEventsTailDropsPartialFirstLine(t *testing.T) {
	var lines []string
	for i := 0; i < 400; i++ {
		lines = append(lines, evLine(
			evNow.Add(-time.Duration(400-i)*time.Second).Format(time.RFC3339), "boom"))
	}
	p := writeEvents(t, lines...)

	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	// 故意切在一行的中間。
	raw, truncated, err := tailBytes(p, fi.Size()/2+7)
	if err != nil {
		t.Fatal(err)
	}
	if !truncated {
		t.Fatal("讀不到整份檔案時 truncated 要是 true")
	}
	got, _ := splitTailLines(raw, truncated, eventsMaxLines)
	for i, ln := range got {
		if s := strings.TrimSpace(string(ln)); s != "" && !strings.HasPrefix(s, "{") {
			t.Fatalf("第 %d 行不是完整的 JSON 開頭：%q", i, s)
		}
	}
}

// ⚠ 沒截斷的時候**不准**丟第一行 —— 那會讓小檔案每次都少算一筆。
func TestEventsUntruncatedKeepsFirstLine(t *testing.T) {
	p := writeEvents(t,
		evLine("2026-09-04T20:30:00Z", "boom"),
		evLine("2026-09-04T20:31:00Z", "boom"),
	)
	s := readOne(t, evRule(p, []string{"boom"}, 3600))
	if n, _ := declaredCount(s, "boom"); n != 2 {
		t.Fatalf("整份讀得完就不該丟行，得到 %d", n)
	}
	if s.Truncated {
		t.Fatal("整份讀得完不該標 Truncated")
	}
}

func TestEventsLineCapMarksDiscardedPrefixAsTruncated(t *testing.T) {
	lines := make([]string, 0, eventsMaxLines+1)
	// 壞事件刻意放在仍屬窗口、但會被 20k 行上限丟掉的前綴。
	lines = append(lines, evLine(evNow.Add(-time.Minute).Format(time.RFC3339), "boom"))
	for i := 0; i < eventsMaxLines; i++ {
		lines = append(lines, evLine(evNow.Add(-time.Second).Format(time.RFC3339), "ok"))
	}
	p := writeEvents(t, lines...)
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() >= eventsMaxBytes {
		t.Fatalf("fixture 必須只撞行數上限，不撞 byte cap：size=%d", fi.Size())
	}
	s := readOne(t, evRule(p, []string{"boom"}, 3600))
	if n, _ := declaredCount(s, "boom"); n != 0 {
		t.Fatalf("fixture 的前綴沒有被行數 cap 丟掉：boom=%d", n)
	}
	if !s.Truncated {
		t.Fatal("行數 cap 丟掉仍在窗口內的前綴，卻宣稱 Truncated=false")
	}
}

// ⚠⚠ CoveredFrom 是「這次真的看到多久以前」。沒有它，一個被讀取上限
// 截掉的計數看起來會跟真的總數一模一樣。
func TestEventsReportsCoveredFrom(t *testing.T) {
	oldest := evNow.Add(-50 * time.Minute)
	p := writeEvents(t,
		evLine(oldest.Format(time.RFC3339), "boom"),
		evLine(evNow.Add(-5*time.Minute).Format(time.RFC3339), "boom"),
	)
	s := readOne(t, evRule(p, []string{"boom"}, 3600))
	if s.CoveredFrom == nil || !s.CoveredFrom.Equal(oldest) {
		t.Fatalf("CoveredFrom 要是讀到的最舊一筆 %v，得到 %v", oldest, s.CoveredFrom)
	}
}

// CoveredFrom 記的是**讀到的**最舊一筆，跟窗口無關 ——
// 窗口外的行也要算進涵蓋範圍，那正是「我看得夠遠」的證據。
func TestEventsCoveredFromIncludesOutOfWindow(t *testing.T) {
	oldest := evNow.Add(-10 * time.Hour)
	p := writeEvents(t,
		evLine(oldest.Format(time.RFC3339), "boom"),
		evLine(evNow.Add(-5*time.Minute).Format(time.RFC3339), "boom"),
	)
	s := readOne(t, evRule(p, []string{"boom"}, 3600))
	if s.CoveredFrom == nil || !s.CoveredFrom.Equal(oldest) {
		t.Fatalf("窗口外的行也要算進 CoveredFrom：%v", s.CoveredFrom)
	}
	if n, _ := declaredCount(s, "boom"); n != 1 {
		t.Fatalf("但窗口外的行不算次數，得到 %d", n)
	}
}

// --- undeclared：上限與總數 ---

func TestEventsUndeclaredSortedAndCapped(t *testing.T) {
	var lines []string
	// 造 12 種沒宣告過的事件，第 i 種出現 i+1 次。
	for i := 0; i < 12; i++ {
		for j := 0; j <= i; j++ {
			lines = append(lines, evLine(
				evNow.Add(-time.Duration(j+1)*time.Minute).Format(time.RFC3339),
				fmt.Sprintf("kind_%02d", i)))
		}
	}
	p := writeEvents(t, lines...)
	s := readOne(t, evRule(p, []string{"baseline_hash_mismatch"}, 3600))

	if s.UndeclaredTotal != 12 {
		t.Fatalf("UndeclaredTotal 要是截斷前的種類數 12，得到 %d", s.UndeclaredTotal)
	}
	if len(s.Undeclared) != eventsMaxUndeclared {
		t.Fatalf("要截到 %d 種，得到 %d", eventsMaxUndeclared, len(s.Undeclared))
	}
	// 次數多的在前。
	for i := 1; i < len(s.Undeclared); i++ {
		if s.Undeclared[i-1].Count < s.Undeclared[i].Count {
			t.Fatalf("要按次數由多到少：%+v", s.Undeclared)
		}
	}
	if s.Undeclared[0].Type != "kind_11" {
		t.Fatalf("最多的那種要在最前面，得到 %s", s.Undeclared[0].Type)
	}
}

// --- 安全 ---

// 事件名來自別人的檔案，會被存起來、被畫到頁面上 —— 一樣要遮。
func TestEventsRedactsEventName(t *testing.T) {
	p := writeEvents(t, fmt.Sprintf(`{"ts":%q,"event":"leak_bot123456789:AAHfakefakefakefakefakefakefake"}`,
		evNow.Add(-time.Minute).Format(time.RFC3339)))
	s := readOne(t, evRule(p, []string{"nope"}, 3600))
	if len(s.Undeclared) != 1 {
		t.Fatalf("要有一種 undeclared：%+v", s.Undeclared)
	}
	if strings.Contains(s.Undeclared[0].Type, "AAHfakefake") {
		t.Fatalf("事件名沒有被遮：%q", s.Undeclared[0].Type)
	}
}

// 一般的事件名不該被遮壞 —— 遮太多跟遮太少一樣糟。
func TestEventsLeavesNormalNamesAlone(t *testing.T) {
	names := []string{"baseline_hash_mismatch", "hermes_release_review_pending",
		"watcher_heartbeat", "deploy_baseline_started", "pigface_intervention"}
	var lines []string
	for _, n := range names {
		lines = append(lines, evLine(evNow.Add(-time.Minute).Format(time.RFC3339), n))
	}
	p := writeEvents(t, lines...)
	s := readOne(t, evRule(p, names, 3600))
	for i, d := range s.Declared {
		if strings.Contains(d.Type, "REDACT") {
			t.Fatalf("第 %d 個正常事件名被遮壞了：%q", i, d.Type)
		}
	}
	if len(s.Declared) != len(names) {
		t.Fatalf("要 %d 個，得到 %d", len(names), len(s.Declared))
	}
}

// --- 讀不到 ---

func TestEventsMissingFileReportsErr(t *testing.T) {
	s := readOne(t, evRule(filepath.Join(t.TempDir(), "nope.jsonl"), []string{"boom"}, 3600))
	if s.Err == "" {
		t.Fatal("檔案不存在要有 Err")
	}
	// ⚠ 「產出物在不在」是 artifact.go 的職責，這裡不該再宣稱任何計數。
	if len(s.Declared) != 0 || len(s.Undeclared) != 0 {
		t.Fatalf("讀不到就不該有任何計數：%+v / %+v", s.Declared, s.Undeclared)
	}
}

func TestEventsDirectoryIsAnError(t *testing.T) {
	s := readOne(t, evRule(t.TempDir(), []string{"boom"}, 3600))
	if s.Err == "" {
		t.Fatal("指到目錄要有 Err")
	}
}

// --- 結構上的界線 ---

// ⚠⚠ 跟 journal.go 同一條規矩：agent 回報事實，不回報判決。
// 這個型別上一旦長出 Healthy / Failed / Severity 這種欄位，
// 「哪些事件算不 OK 由人宣告」那條界線就從結構上破了。
func TestEventStreamHasNoVerdictFields(t *testing.T) {
	banned := []string{"healthy", "ok", "failed", "severity", "status", "state",
		"errorcount", "alert", "degraded", "passed"}
	for _, typ := range []reflect.Type{
		reflect.TypeOf(model.EventStream{}),
		reflect.TypeOf(model.EventSummary{}),
	} {
		for i := 0; i < typ.NumField(); i++ {
			name := strings.ToLower(typ.Field(i).Name)
			for _, b := range banned {
				if name == b {
					t.Fatalf("%s 長出了判決欄位 %s —— 判決是 Hub 的事，不是 agent 的",
						typ.Name(), typ.Field(i).Name)
				}
			}
		}
	}
}

// TestEventsAgainstRealCapture 拿**真的**檔案跑一遍。
//
// ⚠⚠ 這個專案的地基二寫著「自證不算數」。上面每一個測試用的都是我自己造的
// 輸入 —— 拿自己造的世界去驗自己寫的程式，最多只證明我前後一致。
// 這一條驗的是真實的資料形狀。取樣方式：
//
//	ssh sampleagent2 'tail -c 400000 ~/.openclaw/workspace/evolution-journal.jsonl' > /tmp/real-events.jsonl
//	CLAWCTL_EVENTS_CAPTURE=/tmp/real-events.jsonl go test ./internal/probe/ -run RealCapture -v
func TestEventsAgainstRealCapture(t *testing.T) {
	path := os.Getenv("CLAWCTL_EVENTS_CAPTURE")
	if path == "" {
		t.Skip("沒有 CLAWCTL_EVENTS_CAPTURE，跳過（這條要真檔案）")
	}
	// now 取檔案裡最新的一筆 —— 用 time.Now() 的話這個測試會隨時間腐爛。
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var newest time.Time
	for _, ln := range strings.Split(string(body), "\n") {
		var rec struct {
			TS string `json:"ts"`
		}
		if jsonUnmarshalQuiet([]byte(ln), &rec) && rec.TS != "" {
			if ts, err := time.Parse(time.RFC3339, rec.TS); err == nil && ts.After(newest) {
				newest = ts
			}
		}
	}
	if newest.IsZero() {
		t.Fatal("這個檔案裡一筆時間都讀不出來 —— 取樣壞了")
	}
	now := newest.Add(time.Minute)

	r := evRule(path, []string{"baseline_hash_mismatch"}, 3600)
	full := readEvents(r, now)

	n, ok := declaredCount(full, "baseline_hash_mismatch")
	if !ok {
		t.Fatal("宣告過的事件名不見了")
	}
	if n == 0 {
		t.Fatal("真實檔案的最後一小時裡 baseline_hash_mismatch 應該不是 0 —— 這台喊了 81 天")
	}
	// ⚠ tail -c 取樣一定會切斷第一行；除此之外不該有讀不懂的行。
	if full.Malformed > 1 {
		t.Fatalf("真實資料有 %d 行讀不懂（取樣的半行只該有 1 行）", full.Malformed)
	}
	if full.CoveredFrom == nil {
		t.Fatal("要講得出涵蓋到多久以前")
	}
	var sawHeartbeat bool
	for _, u := range full.Undeclared {
		if u.Type == "watcher_heartbeat" {
			sawHeartbeat = true
		}
	}
	if !sawHeartbeat {
		t.Fatalf("watcher_heartbeat 該落在 undeclared：%+v", full.Undeclared)
	}
	t.Logf("真實資料：baseline_hash_mismatch=%d、涵蓋到 %s、undeclared=%d 種、malformed=%d",
		n, full.CoveredFrom.Format(time.RFC3339), full.UndeclaredTotal, full.Malformed)
}

func jsonUnmarshalQuiet(b []byte, v any) bool { return json.Unmarshal(b, v) == nil }
