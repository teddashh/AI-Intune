package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/expect"
	"github.com/teddashh/AI-Intune/internal/model"
)

// 期望的事實怎麼變成 metric。
//
// ⚠⚠ 這一組測試存在的理由：新加的九個指標**在既有的 fixture 裡一行都跑不到**。
// metricsFixture 沒有任何宣告，所以整包程式碼可以是壞的、整份測試還是綠的。
// 2026-09-05 加完之後 go test 全綠，而那個綠什麼都沒證明。
//
// 這一段搬家搬的是判斷的位置：門檻從 internal/state 搬到 Prometheus 的規則。
// 搬得過去的前提是**事實要吐得出來**，而且吐的時候不能弄丟 internal/state
// 那四個「不准安靜」—— 那些紀律現在是程式碼、被測試釘著，搬進 annotation
// 之後沒有人會自動幫我們守。所以先在這一側釘住。

// declaredFixture 建一台有宣告、而且真的量到了的機器。
func declaredFixture(t *testing.T, ev *model.EventStream, art model.ArtifactCheck, creds []model.Credential) map[string]*family {
	t.Helper()

	const machine = "fixture-machine"
	dir := t.TempDir()
	path := filepath.Join(dir, "expectations.json")
	body := map[string]any{"expectations": []map[string]any{{
		"machine":         machine,
		"unit":            art.Unit,
		"artifact":        art.Artifact,
		"max_age_seconds": 5400,
		"why":             "測試用的宣告",
		"events": map[string]any{
			"ts_field": "ts", "type_field": "event",
			"not_ok":         []string{"baseline_hash_mismatch", "openclaw_gateway_not_running"},
			"window_seconds": 3600,
		},
	}}}
	raw, _ := json.Marshal(body)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("寫期望檔：%v", err)
	}
	set := expect.Load(path)
	if set == nil || len(set.For(machine)) != 1 {
		t.Fatalf("期望檔沒載進來 —— 後面的斷言會在一個空世界裡通過")
	}

	mux, st := emptyMetricsFixture(t)
	t.Cleanup(func() { st.Close() })
	st.SetExpectations(set)
	if err := st.PublishExpectationsPolicy(time.Now().UTC()); err != nil {
		t.Fatalf("發布測試期望：%v", err)
	}

	m := enrollViaHTTP(t, mux, st, machine)
	postCheckin(t, mux, m.token, http.StatusOK)

	batch := model.ObservationBatch{
		SchemaVersion: model.SchemaVersion, MeasuredAt: time.Now().UTC(),
		Identity:    model.Identity{Hostname: "h", OS: "linux", Arch: "amd64", UnixUser: "u"},
		Artifacts:   []model.ArtifactCheck{art},
		Credentials: creds,
	}
	if ev != nil {
		batch.Events = []model.EventStream{*ev}
	}
	b, _ := json.Marshal(batch)
	post(t, mux, "/v1/observations:batch", m.token, b, http.StatusNoContent)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /metrics = %d：%s", rec.Code, rec.Body.String())
	}
	return parseExposition(t, rec.Body.String())
}

func mustSample(t *testing.T, fam map[string]*family, name string) *family {
	t.Helper()
	f, ok := fam[name]
	if !ok {
		t.Fatalf("整頁裡找不到 %s —— 一條不見的線在圖上跟「這個指標從來沒存在過」一模一樣", name)
	}
	if len(f.samples) == 0 {
		t.Fatalf("%s 有 TYPE 但沒有 sample —— 對 Prometheus 來說等於不存在", name)
	}
	return f
}

// ⚠⚠ 宣告過、但這個窗口裡一次都沒出現的事件名，Count=0 也要吐一行。
//
// 這是 internal/state 那四個「不准安靜」的第一個。少吐的話，
// 「這個事件沒發生」跟「這條規則根本沒在看」在 Prometheus 裡一模一樣 ——
// 而後者才是最痛的那種錯（PRODUCT.md 地基二：最痛的錯，log 裡沒有 error）。
func TestDeclaredEventWithZeroOccurrencesStillEmitsALine(t *testing.T) {
	now := time.Now().UTC()
	fam := declaredFixture(t,
		&model.EventStream{
			Unit: "openclaw-watcher.service", Path: "/tmp/j.jsonl",
			// 只有一個名字真的出現過；另一個宣告過但是 0。
			Declared: []model.EventSummary{
				{Type: "baseline_hash_mismatch", Count: 41, LastAt: &now},
				{Type: "openclaw_gateway_not_running", Count: 0},
			},
		},
		model.ArtifactCheck{
			Unit: "openclaw-watcher.service", Artifact: "/tmp/j.jsonl",
			Exists: true, ModTime: &now,
		}, nil)

	f := mustSample(t, fam, "clawctl_event_count")
	got := map[string]float64{}
	for _, s := range f.samples {
		got[s.labels["event"]] = s.value
	}
	if got["baseline_hash_mismatch"] != 41 {
		t.Errorf("baseline_hash_mismatch = %v，要 41", got["baseline_hash_mismatch"])
	}
	v, ok := got["openclaw_gateway_not_running"]
	if !ok {
		t.Errorf("宣告過但沒出現的事件名不見了 —— " +
			"「沒發生」跟「沒在看」在 Prometheus 裡會變成同一件事")
	} else if v != 0 {
		t.Errorf("openclaw_gateway_not_running = %v，要 0", v)
	}
}

// 門檻要跟事實一起走，否則規則那側寫不成一條通用的比較。
func TestTheDeclaredThresholdTravelsWithTheFact(t *testing.T) {
	now := time.Now().UTC()
	fam := declaredFixture(t, nil, model.ArtifactCheck{
		Unit: "openclaw-watcher.service", Artifact: "/tmp/j.jsonl",
		Exists: true, ModTime: &now,
	}, nil)

	age := mustSample(t, fam, "clawctl_artifact_age_seconds")
	max := mustSample(t, fam, "clawctl_artifact_max_age_seconds")
	if max.samples[0].value != 5400 {
		t.Errorf("max_age = %v，要 5400（expectations.json 裡宣告的那個數字）", max.samples[0].value)
	}
	// ⚠ 兩條線的 label 必須**完全一樣**，否則 PromQL 的
	// `age > on(...) max_age` 會比不起來 —— 而比不起來的結果是不告警，
	// 不是報錯。又一個乾淨的空答案。
	if age.samples[0].key() == "" || max.samples[0].key() == "" {
		t.Fatal("key 是空的")
	}
	a, m := age.samples[0].labels, max.samples[0].labels
	for _, k := range []string{"machine", "machine_id", "unit", "artifact"} {
		if a[k] != m[k] || a[k] == "" {
			t.Errorf("label %s 對不起來：age=%q max_age=%q —— "+
				"對不起來的兩條線比不出結果，而比不出來不會報錯，只會不告警", k, a[k], m[k])
		}
	}
}

// ⚠ 檔案不見 ≠ 檔案很舊，而且前者更嚴重。
// 只吐 age 的話，檔案不見會讓那條線消失，在圖上跟「這台沒宣告過」一樣。
func TestAMissingArtifactIsAZeroNotAnAbsentLine(t *testing.T) {
	fam := declaredFixture(t, nil, model.ArtifactCheck{
		Unit: "openclaw-watcher.service", Artifact: "/tmp/gone.jsonl",
		Exists: false,
	}, nil)

	p := mustSample(t, fam, "clawctl_artifact_present")
	if p.samples[0].value != 0 {
		t.Errorf("present = %v，檔案不在應該是 0", p.samples[0].value)
	}
	// 沒有 ModTime 就不該有年齡 —— 宣告了但沒量到 ≠ 通過，
	// 吐一個 0 會讓它看起來像剛剛才更新過。
	if f, ok := fam["clawctl_artifact_age_seconds"]; ok && len(f.samples) > 0 {
		t.Errorf("檔案不在卻吐了 age=%v —— 那會看起來像它剛剛更新過", f.samples[0].value)
	}
}

// 憑證過期是負秒數，不是一個「過期了」的旗標。判斷留給規則那側。
func TestCredentialExpiryIsSecondsNotAVerdict(t *testing.T) {
	past := time.Now().UTC().Add(-3 * time.Hour)
	fam := declaredFixture(t, nil, model.ArtifactCheck{
		Unit: "openclaw-watcher.service", Artifact: "/tmp/j.jsonl", Exists: false,
	}, []model.Credential{{Provider: "grok", Status: model.CredExpired, ExpiresAt: &past}})

	f := mustSample(t, fam, "clawctl_credential_expiry_seconds")
	if f.samples[0].labels["provider"] != "grok" {
		t.Fatalf("provider label = %q", f.samples[0].labels["provider"])
	}
	if v := f.samples[0].value; v > -3500 || v < -3*3600-120 {
		t.Errorf("expiry = %v，三小時前過期應該是大約 -10800", v)
	}
	// ⚠ 不准出現「健康」形狀的指標。要不要亮燈是規則那側的事。
	for _, banned := range []string{
		"clawctl_credential_expired", "clawctl_credential_ok",
		"clawctl_artifact_stale", "clawctl_healthy", "clawctl_up",
	} {
		if _, ok := fam[banned]; ok {
			t.Errorf("出現了 %s —— 這一頁只吐事實，判斷寫在告警規則那側才有人會去改它", banned)
		}
	}
}

// ⚠⚠ 欄位名打錯會讓整條規則永遠安靜 —— 每一個事件計數都是 0，
// 看起來跟「一切正常」完全一樣。所以 malformed 必須是一條看得見的線。
func TestMalformedLinesAreVisibleBecauseTheyMakeTheRuleSilent(t *testing.T) {
	now := time.Now().UTC()
	fam := declaredFixture(t,
		&model.EventStream{
			Unit: "openclaw-watcher.service", Path: "/tmp/j.jsonl",
			Declared: []model.EventSummary{
				{Type: "baseline_hash_mismatch", Count: 0},
				{Type: "openclaw_gateway_not_running", Count: 0},
			},
			Malformed: 1440, // 欄位名寫錯，每一行都解不開
		},
		model.ArtifactCheck{
			Unit: "openclaw-watcher.service", Artifact: "/tmp/j.jsonl",
			Exists: true, ModTime: &now,
		}, nil)

	f := mustSample(t, fam, "clawctl_event_malformed_lines")
	if f.samples[0].value != 1440 {
		t.Errorf("malformed = %v，要 1440", f.samples[0].value)
	}
	// 這個情境下每個事件計數都是 0。如果沒有 malformed 這條線，
	// 這一頁跟「一切正常」是**完全一樣的**。
	c := mustSample(t, fam, "clawctl_event_count")
	for _, s := range c.samples {
		if s.value != 0 {
			t.Fatalf("這個情境應該每個計數都是 0，%s = %v", s.labels["event"], s.value)
		}
	}
}

// 沒有人宣告任何期望的時候，那幾塊整塊不該出現 ——
// 但 clawctl_expectations_loaded 必須在，而且是 0。
//
// ⚠ 這兩件事要一起成立才有意義：
//   - 空的區塊出現＝一個只有 TYPE 沒有 sample 的指標，對 Prometheus 等於不存在
//   - 沒有那個 0＝「沒人宣告」跟「期望檔載入失敗」變成同一片空白，
//     而後者 2026-09-04 真的在 cmdReport / cmdMachines 上發生過
func TestExpectationMetricsAppearOnlyWhenDeclared(t *testing.T) {
	mux, st := metricsFixture(t)
	defer st.Close()

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	fam := parseExposition(t, rec.Body.String())

	for _, name := range []string{
		"clawctl_artifact_present", "clawctl_artifact_age_seconds",
		"clawctl_artifact_max_age_seconds", "clawctl_event_count",
		"clawctl_event_window_seconds", "clawctl_event_undeclared_types",
		"clawctl_event_malformed_lines", "clawctl_event_partial",
		"clawctl_credential_expiry_seconds",
	} {
		if _, ok := fam[name]; ok {
			t.Errorf("沒有任何宣告，卻出現了 %s 這一塊", name)
		}
	}

	f := mustSample(t, fam, "clawctl_expectations_loaded")
	if f.samples[0].value != 0 {
		t.Errorf("expectations_loaded = %v，要 0", f.samples[0].value)
	}
}

// parseExposition 只看得到「有沒有」，看不到「順序」。
// ⚠ 一個指標的 sample 必須是連續的一整塊 —— 這個檔案上面記著同一個 bug
// 已經發生過一次（11 個指標被官方 parser 讀成 25 個 family），
// 而我們自己的 parser 照名字分桶，看不到。所以這裡直接讀原文。
func TestEachMetricBlockIsContiguousInTheRawPage(t *testing.T) {
	now := time.Now().UTC()
	const machine = "fixture-machine"
	dir := t.TempDir()
	path := filepath.Join(dir, "e.json")
	raw, _ := json.Marshal(map[string]any{"expectations": []map[string]any{{
		"machine": machine, "unit": "u.service", "artifact": "/tmp/a",
		"max_age_seconds": 5400, "why": "測試",
		"events": map[string]any{"ts_field": "ts", "type_field": "event",
			"not_ok": []string{"x", "y"}, "window_seconds": 3600},
	}}})
	os.WriteFile(path, raw, 0o600)

	mux, st := emptyMetricsFixture(t)
	defer st.Close()
	st.SetExpectations(expect.Load(path))
	if err := st.PublishExpectationsPolicy(now); err != nil {
		t.Fatal(err)
	}

	// 兩台機器，讓每個指標都有多行 sample —— 一行的話交錯看不出來。
	for _, name := range []string{machine, "other-machine"} {
		m := enrollViaHTTP(t, mux, st, name)
		postCheckin(t, mux, m.token, http.StatusOK)
		b, _ := json.Marshal(model.ObservationBatch{
			SchemaVersion: model.SchemaVersion, MeasuredAt: now,
			Identity:  model.Identity{Hostname: "h", OS: "linux", Arch: "amd64", UnixUser: "u"},
			Artifacts: []model.ArtifactCheck{{Unit: "u.service", Artifact: "/tmp/a", Exists: true, ModTime: &now}},
			Events: []model.EventStream{{Unit: "u.service", Path: "/tmp/a",
				Declared: []model.EventSummary{{Type: "x", Count: 1}, {Type: "y", Count: 0}}}},
			Credentials: []model.Credential{{Provider: "grok", Status: model.CredExpired, ExpiresAt: &now}},
		})
		post(t, mux, "/v1/observations:batch", m.token, b, http.StatusNoContent)
	}

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))

	seen := map[string]bool{}
	var cur string
	for _, ln := range bytes.Split(rec.Body.Bytes(), []byte("\n")) {
		s := string(ln)
		if s == "" || s[0] == '#' {
			continue
		}
		name, _, _ := cutMetricName(s)
		if name == cur {
			continue
		}
		if seen[name] {
			t.Errorf("%s 的 sample 被別的指標打斷了 —— exposition format 要求一個指標的"+
				"所有 sample 是連續的一整塊，中斷會讓 Prometheus 把它讀成好幾個 family", name)
		}
		seen[name] = true
		cur = name
	}
}

func cutMetricName(line string) (name, labels string, ok bool) {
	for i := 0; i < len(line); i++ {
		switch line[i] {
		case '{', ' ':
			return line[:i], "", true
		}
	}
	return line, "", false
}

// ⚠⚠ 人在 expectations.json 裡寫的那句理由，必須原封不動到得了告警。
//
// clawctl 的紀律是「why 原封不動出現在總覽、詳細頁與早報的理由那一行」。
// 搬到 Prometheus 之後那句話走 info metric（值恆為 1，內容在 label 上），
// 規則用 group_left(why) 接回去。少了它，Prometheus 那側就只剩我編的
// 通用句子 —— 而通用句子答不出「為什麼這條規則存在」。
func TestTheHumanWrittenReasonSurvivesIntoMetrics(t *testing.T) {
	now := time.Now().UTC()
	fam := declaredFixture(t, nil, model.ArtifactCheck{
		Unit: "openclaw-watcher.service", Artifact: "/tmp/j.jsonl",
		Exists: true, ModTime: &now,
	}, nil)

	f := mustSample(t, fam, "clawctl_expectation_info")
	if v := f.samples[0].value; v != 1 {
		t.Errorf("info metric 的值要恆為 1，得到 %v", v)
	}
	if got := f.samples[0].labels["why"]; got != "測試用的宣告" {
		t.Errorf("why = %q，要「測試用的宣告」—— 這句話是人寫的，不准改寫也不准截斷", got)
	}
	// ⚠ group_left(why) 要接得起來，label 就必須跟事實那幾條完全對齊。
	age := mustSample(t, fam, "clawctl_artifact_age_seconds")
	for _, k := range []string{"machine", "machine_id", "unit", "artifact"} {
		if f.samples[0].labels[k] != age.samples[0].labels[k] {
			t.Errorf("label %s 對不起來 —— group_left(why) 會接不上，"+
				"而接不上的結果是 annotation 裡出現空白，不是報錯", k)
		}
	}
}
