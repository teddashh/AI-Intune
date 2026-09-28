// Package probe —— 結構化事件流。
//
// 這支東西讀一條 JSONL，把裡面的**事件名**分組數數，回報每種出現幾次、
// 最後一次是什麼時候。它不判過不過，判決在 Hub。
//
// ⚠⚠ 這跟 journal.go 明講不做的那件事只差一線，界線寫在 model.EventSpec 上：
// 讀的是驗證程式自己寫出來的**列舉欄位**，不是散文；哪些名字算不 OK
// **由人宣告**。而地基二真正的顧慮（「最痛的那種錯，log 裡沒有 error」）
// 是靠同一條規則的 MaxAgeSeconds 補的 —— 事件流停了，檔案就過期。
// **只做事件名、不做新鮮度，就退回成一張關鍵字表了。**
//
// 現實限制：sampleagent2 那個檔案 24.5 MB、12 萬行。整個讀進來不行，
// 所以這裡是 byte-bounded 的 tail read —— 而「只讀到尾巴」這件事
// 必須回報出去（CoveredFrom / Truncated），否則一個被截斷的計數
// 看起來會跟真的總數一模一樣。
package probe

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/teddashh/AI-Intune/internal/model"
)

const (
	// eventsMaxBytes 是從檔尾往回讀的上限。sampleagent2 的實測值：
	// 24 小時約 1440 行、293 KB，所以 2 MB 有約 7 倍餘裕。
	eventsMaxBytes = 2 << 20
	// eventsMaxLines 是第二道閘 —— 一行很短的檔案不該讓我們吃 2 MB 的行數。
	eventsMaxLines = 20000
	// eventsMaxUndeclared 是回報幾種沒宣告過的事件名。
	// ⚠ 有上限就一定要回報總數（UndeclaredTotal），不然截斷會偽裝成「就這些」。
	eventsMaxUndeclared = 8
)

// CheckEvents 對每一條有宣告 Events 的期望讀一次事件流。
func CheckEvents(exps []model.Expectation, now time.Time) []model.EventStream {
	out := make([]model.EventStream, 0, len(exps))
	for _, e := range exps {
		if e.Events == nil {
			continue
		}
		out = append(out, readEvents(e, now))
	}
	return out
}

func readEvents(e model.Expectation, now time.Time) model.EventStream {
	spec := e.Events
	s := model.EventStream{Unit: e.Unit, Path: e.Artifact}

	raw, truncated, err := tailBytes(e.Artifact, eventsMaxBytes)
	if err != nil {
		// ⚠ 「不存在」在這裡也只是一個錯誤字串。**產出物在不在**是
		// artifact.go 的職責，兩邊都講一次會讓同一件事被報兩遍。
		s.Err = err.Error()
		return s
	}
	s.Truncated = truncated

	window := time.Duration(spec.WindowSeconds) * time.Second
	since := now.Add(-window)

	notOK := make(map[string]bool, len(spec.NotOK))
	for _, n := range spec.NotOK {
		notOK[n] = true
	}

	type agg struct {
		count int
		last  time.Time
	}
	counts := map[string]*agg{}
	var coveredFrom time.Time

	lines, lineTruncated := splitTailLines(raw, truncated, eventsMaxLines)
	s.Truncated = s.Truncated || lineTruncated
	for _, ln := range lines {
		ln = bytes.TrimSpace(ln)
		if len(ln) == 0 {
			continue
		}
		var rec map[string]json.RawMessage
		if json.Unmarshal(ln, &rec) != nil {
			s.Malformed++
			continue
		}
		ts, ok := parseEventTime(rec[spec.TsField])
		if !ok {
			s.Malformed++
			continue
		}
		name, ok := parseEventName(rec[spec.TypeField])
		if !ok {
			s.Malformed++
			continue
		}
		// coveredFrom 記的是**讀到的**最舊一筆，跟窗口無關 ——
		// 它要回答的是「我這次到底看到了多久以前」。
		if coveredFrom.IsZero() || ts.Before(coveredFrom) {
			coveredFrom = ts
		}
		if ts.Before(since) {
			continue
		}
		a := counts[name]
		if a == nil {
			a = &agg{}
			counts[name] = a
		}
		a.count++
		if ts.After(a.last) {
			a.last = ts
		}
	}
	if !coveredFrom.IsZero() {
		c := coveredFrom.UTC()
		s.CoveredFrom = &c
	}

	// 宣告過的一律列出來，**沒出現的 Count=0 也要在**：
	// 不然「這種錯誤沒發生」跟「我沒在看這種錯誤」會長得一模一樣。
	for _, n := range spec.NotOK {
		sum := model.EventSummary{Type: n}
		if a := counts[n]; a != nil {
			sum.Count = a.count
			t := a.last.UTC()
			sum.LastAt = &t
		}
		s.Declared = append(s.Declared, sum)
	}
	sort.Slice(s.Declared, func(i, j int) bool { return s.Declared[i].Type < s.Declared[j].Type })

	for name, a := range counts {
		if notOK[name] {
			continue
		}
		t := a.last.UTC()
		s.Undeclared = append(s.Undeclared, model.EventSummary{Type: name, Count: a.count, LastAt: &t})
	}
	sort.Slice(s.Undeclared, func(i, j int) bool {
		if s.Undeclared[i].Count != s.Undeclared[j].Count {
			return s.Undeclared[i].Count > s.Undeclared[j].Count
		}
		return s.Undeclared[i].Type < s.Undeclared[j].Type
	})
	s.UndeclaredTotal = len(s.Undeclared)
	if len(s.Undeclared) > eventsMaxUndeclared {
		s.Undeclared = s.Undeclared[:eventsMaxUndeclared]
	}
	return s
}

// tailBytes 讀檔案最後 max 個 byte。第二個回傳值是「前面還有沒讀到的」。
func tailBytes(path string, max int64) ([]byte, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return nil, false, err
	}
	if fi.IsDir() {
		return nil, false, fmt.Errorf("%s 是一個目錄，不是事件流", path)
	}

	size := fi.Size()
	off, truncated := int64(0), false
	if size > max {
		off, truncated = size-max, true
	}
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return nil, false, err
	}
	buf, err := io.ReadAll(io.LimitReader(f, max))
	if err != nil {
		return nil, false, err
	}
	return buf, truncated, nil
}

// splitTailLines 切行並丟掉開頭那半行。
//
// ⚠ 從檔案中間開始讀，第一行幾乎一定是斷的。不丟掉它會多一筆 Malformed，
// 而那筆 Malformed 是我們自己切出來的 —— 那會讓每一次讀取都謊報一行壞資料。
func splitTailLines(raw []byte, truncated bool, maxLines int) ([][]byte, bool) {
	lines := bytes.Split(raw, []byte("\n"))
	if truncated && len(lines) > 0 {
		lines = lines[1:]
	}
	lineTruncated := len(lines) > maxLines
	if len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}
	return lines, lineTruncated
}

func parseEventName(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 {
		return "", false
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return "", false
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return "", false
	}
	// ⚠ 事件名會被存起來、被畫到頁面上。它來自別人的檔案，一樣要遮。
	// 正常的事件名不會被 redact 動到（沒有 40 字以上的連續 token）。
	return truncateRunes(redact(s), 80), true
}

// parseEventTime 收字串（RFC3339）或數字（unix 秒）。
// ⚠ 不猜其他格式。猜錯格式會得到一個「什麼事件都沒有」的乾淨答案 ——
// 這正是這個專案反覆撞到的那一種假的好消息。
func parseEventTime(raw json.RawMessage) (time.Time, bool) {
	if len(raw) == 0 {
		return time.Time{}, false
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			return t.UTC(), true
		}
		return time.Time{}, false
	}
	var n float64
	if json.Unmarshal(raw, &n) == nil && n > 0 {
		return time.Unix(int64(n), 0).UTC(), true
	}
	return time.Time{}, false
}
