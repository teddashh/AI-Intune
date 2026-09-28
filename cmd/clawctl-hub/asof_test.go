package main

import (
	"testing"
	"time"
)

// `--as-of 2026-09-20` 要被讀成 2026-09-20 00:00 **UTC**。
//
// ⚠⚠ 這一條看起來像在測 time.Parse，其實在測時區。
// prune 圈的每一個時間欄都是 Hub 自己的 UTC 鐘（pruneJob.cutCol 那段講了
// 為什麼不能用機器自己的鐘）。如果這裡用 time.ParseInLocation 配本地時區，
// 在 samplehub1（EDT，UTC−4）上 `--as-of 2026-09-20` 會變成 09-20 04:00 UTC ——
// 只差四小時，但心跳是每兩分鐘一顆，那是一百多列的差距。
//
// 而它出錯的方式特別難看：答案永遠「差不多對」。沒有人會因為
// 3655 變成 3781 而察覺有問題，直到有人拿這個數字去對帳。
func TestAsOfIsReadAsUTCNotLocalTime(t *testing.T) {
	got, err := parseAsOf("2026-09-20")
	if err != nil {
		t.Fatalf("parseAsOf: %v", err)
	}
	want := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("parseAsOf(\"2026-09-20\") = %s，要的是 %s（UTC）",
			got.Format(time.RFC3339), want.Format(time.RFC3339))
	}
	if got.Location() != time.UTC {
		t.Errorf("時區是 %v，不是 UTC —— prune 圈的全部是 UTC", got.Location())
	}
}

func TestAsOfAcceptsFullRFC3339AndNormalisesToUTC(t *testing.T) {
	// 帶時區的輸入要被換算，不是被截掉。
	got, err := parseAsOf("2026-09-20T00:00:00-04:00")
	if err != nil {
		t.Fatalf("parseAsOf: %v", err)
	}
	want := time.Date(2026, 9, 20, 4, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("= %s，要的是 %s", got.Format(time.RFC3339), want.Format(time.RFC3339))
	}
}

// ⚠ 讀不懂的時候要**明確地失敗**，不可以默默地當成「現在」。
//
// 這是這個 repo 裡反覆出現的那個形狀：一個把壞輸入吞掉、
// 回一個看起來正常的預設值的解析器，會讓 `--as-of 下個月` 印出
// 一份跟今天一模一樣的報告 —— 而人會把那份報告讀成「未來也沒事要清」。
func TestAsOfRefusesWhatItCannotUnderstand(t *testing.T) {
	for _, in := range []string{
		"下個月",
		"+30d",       // 刻意不支援相對時間：貼進工單三天後意思就變了
		"2026-13-01", // 不存在的月份
		"09-20-2026", // 美式順序
		"2026/09/20", // 斜線
		"",           // 空字串（呼叫端理論上擋掉了，但這裡也不能通過）
		"1788436819", // unix 秒 —— 看起來很像一個時間
	} {
		if got, err := parseAsOf(in); err == nil {
			t.Errorf("parseAsOf(%q) 竟然通過了，回 %s —— "+
				"讀不懂就要吵，不能安靜地給一個看起來正常的答案",
				in, got.Format(time.RFC3339))
		}
	}
}
