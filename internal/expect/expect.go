// Package expect 載入「人宣告的期望」—— 每個 unit 做完一輪應該留下什麼。
//
// ⚠⚠ 這個 package 存在的理由，是 clawctl **不准自己猜**什麼算成功。
// 見 model.Expectation 的註解與 docs/PRODUCT.md 地基二。
package expect

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/teddashh/AI-Intune/internal/model"
)

// Set 是載入結果。
//
// ⚠⚠ 它有三種狀態，而**把後兩種混在一起是這個檔案最容易犯的錯**：
//
//  1. 沒有設定檔        → Configured=false，Rules 空
//  2. 有設定檔、是空的  → Configured=true，Rules 空
//  3. 有設定檔、讀壞了  → Err 非空
//
// 三種在「Rules 是空的」這件事上長得一模一樣，而它們的意思完全不同：
// 第一種是「還沒有人宣告過任何期望」，第三種是「有人宣告了，而我沒讀到」。
// 畫面上把第三種顯示成第一種，就是這個專案花了四十八次在學的那件事。
type Set struct {
	Configured bool
	Path       string
	Rules      []model.Expectation
	Err        string
}

type file struct {
	Expectations []model.Expectation `json:"expectations"`

	// Comment 存在的唯一理由是讓 JSON 設定檔能寫註解（JSON 沒有註解語法），
	// 而 DisallowUnknownFields 會擋掉沒宣告的鍵。
	// ⚠ 讀進來就丟掉，永遠不用它 —— 這一格是給人看的，不是給程式看的。
	// 值得為此加一個欄位：這份設定打錯一個欄位名的後果是「永遠安靜」，
	// 而一份能在旁邊寫清楚自己在幹嘛的設定，比較不容易被打錯。
	Comment any `json:"_comment,omitempty"`
}

// Load 讀設定檔。path 是空字串代表沒有設定，這**不是**錯誤。
func Load(path string) *Set {
	s := &Set{Path: path}
	if strings.TrimSpace(path) == "" {
		return s
	}
	s.Configured = true

	raw, err := os.ReadFile(path)
	if err != nil {
		// ⚠ 檔案不存在也算錯：路徑是**明確設定**的，設了卻讀不到是設定錯誤，
		// 不是「沒有期望」。安靜地回空集合會讓打錯的路徑長得像「一切正常」。
		s.Err = fmt.Sprintf("cannot read expectation config %s: %v", path, err)
		return s
	}
	var f file
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields() // 打錯的欄位名要出聲，不要被安靜忽略
	if err := dec.Decode(&f); err != nil {
		s.Err = fmt.Sprintf("failed to parse expectation config %s: %v", path, err)
		return s
	}
	for i, r := range f.Expectations {
		if err := validate(r); err != nil {
			s.Err = fmt.Sprintf("expectation config %s rule %d is invalid: %v", path, i+1, err)
			return s
		}
	}
	s.Rules = f.Expectations
	return s
}

func validate(r model.Expectation) error {
	switch {
	case strings.TrimSpace(r.Machine) == "":
		return fmt.Errorf("machine is empty (expected machine name or \"*\")")
	case strings.TrimSpace(r.Unit) == "":
		return fmt.Errorf("unit is empty")
	case strings.TrimSpace(r.Artifact) == "":
		return fmt.Errorf("artifact is empty")
	case !filepath.IsAbs(r.Artifact):
		// ⚠ 相對路徑會相對於 agent 的工作目錄解析 —— 那個目錄不是宣告的人
		// 心裡想的那一個，而且不同機器不一樣。一個「看起來對、實際指到別處」
		// 的路徑比一個明顯的錯誤更難查。
		return fmt.Errorf("artifact %q is not an absolute path", r.Artifact)
	case r.MaxAgeSeconds <= 0:
		return fmt.Errorf("max_age_seconds must be greater than 0")
	case strings.TrimSpace(r.Why) == "":
		// ⚠ 這一條是刻意的。一條說不出「為什麼」的期望，半年後沒有人敢刪
		// 也沒有人敢信，它會變成一個永遠紅著、而所有人都學會忽略的東西。
		return fmt.Errorf("why cannot be empty")
	}
	if r.Events != nil {
		if err := validateEvents(r.Events); err != nil {
			return fmt.Errorf("events: %w", err)
		}
	}
	return nil
}

func validateEvents(e *model.EventSpec) error {
	switch {
	case strings.TrimSpace(e.TsField) == "":
		// ⚠ 三個欄位名都不給預設值。猜錯欄位名的結果是「一種事件都沒讀到」——
		// 一個乾淨、安靜、完全錯誤的答案。寧可整份設定拒絕掉。
		return fmt.Errorf("ts_field is empty (name of the timestamp field in that JSONL)")
	case strings.TrimSpace(e.TypeField) == "":
		return fmt.Errorf("type_field is empty (name of the event name field in that JSONL)")
	case e.WindowSeconds <= 0:
		return fmt.Errorf("window_seconds must be greater than 0")
	case len(e.NotOK) == 0:
		// ⚠ 空的 not_ok 技術上跑得動（只會列 undeclared），但那幾乎一定是
		// 寫到一半忘了填。半套設定比沒有設定更危險 —— 讓它自己講出來。
		return fmt.Errorf("not_ok is empty: no event names declared, this rule will never degrade anything")
	}
	for i, n := range e.NotOK {
		if strings.TrimSpace(n) == "" {
			return fmt.Errorf("not_ok[%d] is an empty string", i)
		}
	}
	return nil
}

// For 回傳套用在某台機器上的期望，順序穩定。
func (s *Set) For(machine string) []model.Expectation {
	if s == nil {
		return nil
	}
	var out []model.Expectation
	for _, r := range s.Rules {
		if r.Machine == "*" || r.Machine == machine {
			out = append(out, r)
		}
	}
	// ⚠ 穩定排序：這個列表會走進 checkin 回應，順序跳動會讓每次回應都不一樣，
	// 之後想做「設定有沒有變」的比對時會永遠是「變了」。
	sort.Slice(out, func(i, j int) bool {
		if out[i].Unit != out[j].Unit {
			return out[i].Unit < out[j].Unit
		}
		return out[i].Artifact < out[j].Artifact
	})
	return out
}
