package store

import (
	"testing"
	"time"
)

func intentVersionsByKey(t *testing.T, s *Store) map[string]InstallIntentVersion {
	t.Helper()
	rows, err := s.FleetIntentVersions()
	if err != nil {
		t.Fatalf("fleet intent versions: %v", err)
	}
	out := make(map[string]InstallIntentVersion, len(rows))
	for _, row := range rows {
		key := row.ResourceKind + ":" + row.ResourceID + "@" + row.Version
		if _, duplicate := out[key]; duplicate {
			t.Fatalf("同一個（資源, 版本）回了兩列：%s", key)
		}
		out[key] = row
	}
	return out
}

// 這是這個讀取器存在的全部理由。FleetInstallIntents 只回「現在」，而畫面上要講
// 「這個 Hub 沒有指派過這一版」的時候，問的是整條歷史。一份 profile 點名
// openclaw 2026.9.2，現行意圖裡沒有這一版——那句話只能由這裡來回答。
func TestAVersionThatWasSupersededIsStillAVersionWeAssigned(t *testing.T) {
	s := newTestStore(t)
	at := time.Date(2026, 9, 6, 15, 0, 0, 0, time.UTC)
	mustIntent(t, s, "machine", "samplehub1", "openclaw", "openclaw", openclawSpec("2026.5.26"), at)
	mustIntent(t, s, "machine", "samplehub1", "openclaw", "openclaw", openclawSpec("2026.6.6"), at.Add(time.Minute))

	got := intentVersionsByKey(t, s)
	if _, ok := got["openclaw:openclaw@2026.5.26"]; !ok {
		t.Fatalf("被取代的那一版不見了，這個讀取器就跟現行意圖沒有兩樣：%+v", got)
	}
	if _, ok := got["openclaw:openclaw@2026.6.6"]; !ok {
		t.Fatalf("最後那一版不見了：%+v", got)
	}
}

// 一版被點名幾次就算幾次，而且第一次與最後一次都留著：一個只在半年前被指派過
// 一次的版本，跟一個上禮拜還在指派的版本，要做的事不一樣。
func TestAVersionCountsEveryIntentThatNamedItAndKeepsBothEnds(t *testing.T) {
	s := newTestStore(t)
	first := time.Date(2026, 3, 1, 8, 0, 0, 0, time.UTC)
	last := time.Date(2026, 9, 6, 15, 0, 0, 0, time.UTC)
	mustIntent(t, s, "machine", "samplehub1", "openclaw", "openclaw", openclawSpec("2026.6.6"), first)
	mustIntent(t, s, "machine", "sampleagent2", "openclaw", "openclaw", openclawSpec("2026.6.6"), first.Add(time.Hour))
	mustIntent(t, s, "channel", "canary", "openclaw", "openclaw", openclawSpec("2026.6.6"), last)

	row, ok := intentVersionsByKey(t, s)["openclaw:openclaw@2026.6.6"]
	if !ok {
		t.Fatal("那一版不見了")
	}
	if row.Intents != 3 {
		t.Errorf("點名次數=%d，帳本上有 3 列", row.Intents)
	}
	if !row.FirstAt.Equal(first) {
		t.Errorf("第一次被點名=%s，帳本是 %s", row.FirstAt.Format(time.RFC3339), first.Format(time.RFC3339))
	}
	if !row.LastAt.Equal(last) {
		t.Errorf("最後一次被點名=%s，帳本是 %s", row.LastAt.Format(time.RFC3339), last.Format(time.RFC3339))
	}
}

// rowid 的順序不等於時間的順序。一次還原或一次 migration 之後兩者可以不一致，
// 而「第一次被點名是什麼時候」被講錯的時候，畫面上看不出來。
func TestTheFirstTimeAVersionWasNamedIsTheEarliestNotTheFirstRow(t *testing.T) {
	s := newTestStore(t)
	late := time.Date(2026, 9, 6, 15, 0, 0, 0, time.UTC)
	early := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	mustIntent(t, s, "machine", "samplehub1", "openclaw", "openclaw", openclawSpec("2026.6.6"), late)
	mustIntent(t, s, "machine", "sampleagent2", "openclaw", "openclaw", openclawSpec("2026.6.6"), early)

	row := intentVersionsByKey(t, s)["openclaw:openclaw@2026.6.6"]
	if !row.FirstAt.Equal(early) {
		t.Errorf("第一次被點名=%s，最早的那一列是 %s", row.FirstAt.Format(time.RFC3339), early.Format(time.RFC3339))
	}
	if !row.LastAt.Equal(late) {
		t.Errorf("最後一次被點名=%s，最晚的那一列是 %s", row.LastAt.Format(time.RFC3339), late.Format(time.RFC3339))
	}
}

// 診斷用的空工作單不是安裝意圖，理由跟 FleetInstallIntents 一樣。正式庫 22 列
// 裡有 10 列是 noop，收進來的話「這個 Hub 指派過哪些版本」會多出一個沒有版號
// 的答案。
func TestADiagnosticNoopIsNotAVersionWeAssigned(t *testing.T) {
	s := newTestStore(t)
	at := time.Date(2026, 9, 10, 9, 0, 0, 0, time.UTC)
	mustIntent(t, s, "machine", "sampleagent1", "openclaw", "openclaw", `{"kind":"noop"}`, at)

	if rows, err := s.FleetIntentVersions(); err != nil || len(rows) != 0 {
		t.Fatalf("只下過診斷的帳本回了 %d 列（err=%v）：%+v", len(rows), err, rows)
	}
}

// 一筆「指派過但沒說哪一版」回答不了「有沒有指派過這一版」。它不是這裡的答案，
// 而且不准被寫成一個空版號的答案——那一列會讓「沒有指派過任何版本」看起來像
// 「指派過一個叫做空字串的版本」。
func TestAnIntentThatNamesNoVersionIsNotAVersion(t *testing.T) {
	s := newTestStore(t)
	at := time.Date(2026, 9, 6, 15, 0, 0, 0, time.UTC)
	mustIntent(t, s, "machine", "sampleagent4", "hermes-agent", "hermes-1.4.2", `{"kind":"hermes-agent"}`, at)

	rows, err := s.FleetIntentVersions()
	if err != nil {
		t.Fatalf("fleet intent versions: %v", err)
	}
	for _, row := range rows {
		if row.Version == "" {
			t.Fatalf("生出了一個空版號的答案：%+v", row)
		}
	}
	if len(rows) != 0 {
		t.Fatalf("沒講版號的意圖被當成一版：%+v", rows)
	}
}

// 解不開的 spec 不算，而且不准把同一個資源上讀得開的那幾筆一起吃掉。
func TestAnUnreadableSpecDoesNotSwallowTheVersionsAroundIt(t *testing.T) {
	s := newTestStore(t)
	at := time.Date(2026, 9, 6, 15, 0, 0, 0, time.UTC)
	mustIntent(t, s, "machine", "sampleagent3", "openclaw", "openclaw", openclawSpec("2026.6.10"), at)
	mustIntent(t, s, "machine", "sampleagent3", "openclaw", "openclaw", `not json`, at.Add(time.Minute))

	got := intentVersionsByKey(t, s)
	if len(got) != 1 {
		t.Fatalf("回了 %d 列：%+v", len(got), got)
	}
	if _, ok := got["openclaw:openclaw@2026.6.10"]; !ok {
		t.Fatalf("讀得開的那一版被吃掉了：%+v", got)
	}
}

// 兩個資源各自保有自己的版本。收攏的鍵少了資源那一半，openclaw 的 2026.6.6 會
// 跟另一個套件的 2026.6.6 併成一列，而畫面上會說「這個 Hub 指派過 hermes 的
// 2026.6.6」——一句沒有人下過的指令。
func TestTwoResourcesNeverShareOneVersionRow(t *testing.T) {
	s := newTestStore(t)
	at := time.Date(2026, 9, 6, 15, 0, 0, 0, time.UTC)
	mustIntent(t, s, "machine", "samplehub1", "openclaw", "openclaw", openclawSpec("2026.6.6"), at)
	mustIntent(t, s, "machine", "samplehub1", "openclaw", "openclaw-canary",
		`{"kind":"openclaw","version":"2026.6.6","artifact":{"sha256":"`+
			"40784b514e5e0e3a82b4da484ca0e3f0e6d8e9b2c1a4f7d3e0b6c9a2f5d8e1b4"+`"}}`, at)

	got := intentVersionsByKey(t, s)
	if len(got) != 2 {
		t.Fatalf("兩個資源的同一版被併成 %d 列：%+v", len(got), got)
	}
	for _, key := range []string{"openclaw:openclaw@2026.6.6", "openclaw:openclaw-canary@2026.6.6"} {
		if row := got[key]; row.Intents != 1 {
			t.Errorf("%s 的點名次數=%d，帳本上是 1 列", key, row.Intents)
		}
	}
}

// 順序每次都一樣。一份會被拿去比對的清單如果每次順序不同，讀的人分不出「這一版
// 是新出現的」與「它只是換了位置」。
func TestAssignedVersionsComeBackInOneFixedOrder(t *testing.T) {
	s := newTestStore(t)
	at := time.Date(2026, 9, 6, 15, 0, 0, 0, time.UTC)
	mustIntent(t, s, "machine", "samplehub1", "openclaw", "openclaw", openclawSpec("2026.6.6"), at)
	mustIntent(t, s, "machine", "sampleagent2", "openclaw", "openclaw", openclawSpec("2026.5.20"), at)
	mustIntent(t, s, "machine", "sampleagent3", "openclaw", "openclaw-canary", openclawSpec("2026.6.10"), at)

	want := []string{
		"openclaw:openclaw@2026.5.20",
		"openclaw:openclaw@2026.6.6",
		"openclaw:openclaw-canary@2026.6.10",
	}
	for round := 0; round < 3; round++ {
		rows, err := s.FleetIntentVersions()
		if err != nil {
			t.Fatalf("fleet intent versions: %v", err)
		}
		if len(rows) != len(want) {
			t.Fatalf("第 %d 次回了 %d 列：%+v", round+1, len(rows), rows)
		}
		for index, row := range rows {
			if got := row.ResourceKind + ":" + row.ResourceID + "@" + row.Version; got != want[index] {
				t.Fatalf("第 %d 次的第 %d 列是 %s，應該是 %s", round+1, index+1, got, want[index])
			}
		}
	}
}

// 沒有任何一筆 desired_state 的帳本，回的是零列，不是錯誤。
func TestAnEmptyLedgerHasNoAssignedVersionsAndNoError(t *testing.T) {
	s := newTestStore(t)
	rows, err := s.FleetIntentVersions()
	if err != nil {
		t.Fatalf("空帳本回錯誤：%v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("空帳本回了 %d 列：%+v", len(rows), rows)
	}
}
