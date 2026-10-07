package expect

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "expectations.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// ⚠⚠ 這三種狀態在「Rules 是空的」上長得一模一樣，而意思完全不同。
// 這一支是整個 package 最重要的測試。
func TestTheThreeWaysOfHavingNoRulesAreDistinguishable(t *testing.T) {
	none := Load("")
	if none.Configured || none.Err != "" {
		t.Fatalf("沒設定應該是 Configured=false 且無錯，得到 %+v", none)
	}

	empty := Load(write(t, `{"expectations":[]}`))
	if !empty.Configured || empty.Err != "" || len(empty.Rules) != 0 {
		t.Fatalf("設了但空的應該是 Configured=true 且無錯，得到 %+v", empty)
	}

	// ⚠ 路徑是明確設定的，讀不到就是設定錯誤，不是「沒有期望」。
	missing := Load(filepath.Join(t.TempDir(), "nope.json"))
	if missing.Err == "" {
		t.Fatal("設定了一個不存在的路徑，卻安靜地當作沒有期望 —— 打錯的路徑會長得像一切正常")
	}

	broken := Load(write(t, `{"expectations":[`))
	if broken.Err == "" {
		t.Fatal("壞掉的 JSON 沒有出聲")
	}
}

// 打錯的欄位名要出聲。安靜忽略會讓一條「以為設好了」的期望永遠不生效。
func TestAMisspelledFieldIsRejected(t *testing.T) {
	s := Load(write(t, `{"expectations":[{"machine":"sampleagent2","unit":"u.service",
	  "artifact":"/tmp/x","max_age_secondz":900,"why":"x"}]}`))
	if s.Err == "" {
		t.Fatal("欄位名打錯卻被安靜忽略了")
	}
}

func TestInvalidRulesAreRefusedLoudly(t *testing.T) {
	cases := []struct{ name, json, want string }{
		{"相對路徑", `{"machine":"m","unit":"u","artifact":"rel/x","max_age_seconds":60,"why":"w"}`, "absolute path"},
		{"沒有 why", `{"machine":"m","unit":"u","artifact":"/tmp/x","max_age_seconds":60,"why":""}`, "why"},
		{"門檻是 0", `{"machine":"m","unit":"u","artifact":"/tmp/x","max_age_seconds":0,"why":"w"}`, "max_age_seconds"},
		{"沒有 unit", `{"machine":"m","unit":"","artifact":"/tmp/x","max_age_seconds":60,"why":"w"}`, "unit"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := Load(write(t, `{"expectations":[`+c.json+`]}`))
			if s.Err == "" {
				t.Fatalf("無效的規則被接受了")
			}
			if !strings.Contains(s.Err, c.want) {
				t.Fatalf("錯誤訊息沒講到 %q：%s", c.want, s.Err)
			}
		})
	}
}

// ⚠ 一條規則壞掉不可以只丟掉那一條。整份設定要一起拒絕 ——
// 否則你會得到一個「大部分有生效」的設定檔，而沒有人知道少了哪一條。
func TestOneBadRulePoisonsTheWholeFile(t *testing.T) {
	s := Load(write(t, `{"expectations":[
	  {"machine":"m","unit":"good.service","artifact":"/tmp/a","max_age_seconds":60,"why":"w"},
	  {"machine":"m","unit":"bad.service","artifact":"relative","max_age_seconds":60,"why":"w"}]}`))
	if s.Err == "" {
		t.Fatal("有一條壞的，整份卻被接受了")
	}
	if len(s.Rules) != 0 {
		t.Fatalf("拒絕之後不該還留著 %d 條規則 —— 半套設定比沒有設定更危險", len(s.Rules))
	}
}

func TestStarMatchesEveryMachineAndOrderIsStable(t *testing.T) {
	s := Load(write(t, `{"expectations":[
	  {"machine":"*","unit":"z.service","artifact":"/tmp/z","max_age_seconds":60,"why":"w"},
	  {"machine":"sampleagent2","unit":"a.service","artifact":"/tmp/a","max_age_seconds":60,"why":"w"},
	  {"machine":"sampleagent4","unit":"p.service","artifact":"/tmp/p","max_age_seconds":60,"why":"w"}]}`))
	if s.Err != "" {
		t.Fatal(s.Err)
	}
	got := s.For("sampleagent2")
	if len(got) != 2 {
		t.Fatalf("sampleagent2 應該吃到自己那條加上 *，得到 %d 條", len(got))
	}
	if got[0].Unit != "a.service" || got[1].Unit != "z.service" {
		t.Fatalf("順序不穩定：%s, %s", got[0].Unit, got[1].Unit)
	}
	if len(s.For("samplehub1")) != 1 {
		t.Fatal("samplehub1 應該只吃到 * 那一條")
	}
}

// nil 的 Set 不可以 panic —— Hub 還沒設定完就有人來查是正常的。
func TestNilSetIsSafe(t *testing.T) {
	var s *Set
	if got := s.For("anything"); got != nil {
		t.Fatalf("nil Set 應該回 nil，得到 %v", got)
	}
}

// ---------------------------------------------------------------- 事件流

const evBase = `{"expectations":[{"machine":"sampleagent2","unit":"w.service",
  "artifact":"/tmp/e.jsonl","max_age_seconds":900,"why":"因為",%s}]}`

func TestEventSpecLoadsWhenComplete(t *testing.T) {
	s := Load(write(t, fmt.Sprintf(evBase, `"events":{"ts_field":"ts","type_field":"event",
	  "not_ok":["baseline_hash_mismatch"],"window_seconds":3600}`)))
	if s.Err != "" {
		t.Fatalf("這份設定是好的，卻被拒絕：%s", s.Err)
	}
	if len(s.Rules) != 1 || s.Rules[0].Events == nil {
		t.Fatalf("EventSpec 沒讀進來：%+v", s.Rules)
	}
	if got := s.Rules[0].Events.NotOK; len(got) != 1 || got[0] != "baseline_hash_mismatch" {
		t.Fatalf("not_ok 讀錯：%v", got)
	}
}

// ⚠⚠ 每一條都是「會讓這條規則永遠安靜」的設定錯誤。
// 半套設定比沒有設定更危險 —— 一條規則無效就整份拒絕。
func TestBadEventSpecPoisonsTheWholeFile(t *testing.T) {
	for name, ev := range map[string]string{
		"沒有 ts_field":   `"events":{"type_field":"event","not_ok":["x"],"window_seconds":60}`,
		"沒有 type_field": `"events":{"ts_field":"ts","not_ok":["x"],"window_seconds":60}`,
		"window 是 0":    `"events":{"ts_field":"ts","type_field":"event","not_ok":["x"],"window_seconds":0}`,
		"window 是負的":    `"events":{"ts_field":"ts","type_field":"event","not_ok":["x"],"window_seconds":-1}`,
		"not_ok 是空的":    `"events":{"ts_field":"ts","type_field":"event","not_ok":[],"window_seconds":60}`,
		"not_ok 有空字串":   `"events":{"ts_field":"ts","type_field":"event","not_ok":["x",""],"window_seconds":60}`,
	} {
		t.Run(name, func(t *testing.T) {
			s := Load(write(t, fmt.Sprintf(evBase, ev)))
			if s.Err == "" {
				t.Fatal("這份設定該被拒絕，卻讀進去了")
			}
			// ⚠ 拒絕的時候一條規則都不准留下 —— 半份設定會讓人以為在監控。
			if len(s.Rules) != 0 {
				t.Fatalf("拒絕之後不該留下規則：%+v", s.Rules)
			}
			if !strings.Contains(s.Err, "events") {
				t.Fatalf("錯誤訊息要指出是 events 的問題：%q", s.Err)
			}
		})
	}
}

// 不宣告 events 的規則照舊 —— 事件流是可選的。
func TestExpectationWithoutEventsStillValid(t *testing.T) {
	s := Load(write(t, `{"expectations":[{"machine":"*","unit":"w.service",
	  "artifact":"/tmp/e.jsonl","max_age_seconds":900,"why":"因為"}]}`))
	if s.Err != "" {
		t.Fatalf("沒有 events 的規則該照舊通過：%s", s.Err)
	}
	if s.Rules[0].Events != nil {
		t.Fatal("沒宣告就該是 nil，不准給預設值")
	}
}

// ⚠ 打錯的欄位名要被 DisallowUnknownFields 擋下來，而不是安靜地變成零值。
func TestUnknownFieldInsideEventSpecIsRejected(t *testing.T) {
	s := Load(write(t, fmt.Sprintf(evBase, `"events":{"ts_field":"ts","type_field":"event",
	  "not_ok":["x"],"window_seconds":60,"notok":["typo"]}`)))
	if s.Err == "" {
		t.Fatal("events 裡的未知欄位該被拒絕 —— 打錯的鍵安靜歸零就是假綠燈")
	}
}

// JSON 沒有註解語法，所以 _comment 是刻意開的一格 —— 但它不准變成規則。
func TestCommentKeyIsAllowedButIgnored(t *testing.T) {
	s := Load(write(t, `{"_comment":["這一份設定在回答「它有沒有在做事」"],
	  "expectations":[{"machine":"*","unit":"w.service","artifact":"/tmp/e.jsonl",
	  "max_age_seconds":900,"why":"因為"}]}`))
	if s.Err != "" {
		t.Fatalf("_comment 該被接受：%s", s.Err)
	}
	if len(s.Rules) != 1 {
		t.Fatalf("註解不該影響規則數，得到 %d", len(s.Rules))
	}
}
