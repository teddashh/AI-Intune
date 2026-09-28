package store

import (
	"testing"
	"time"
)

// mustIntent 走的是 profile assignment 那條路：createDesiredStateTx。
//
// ⚠ 不走 CreateDesiredState，因為那個公開包裝擋掉所有 managed catalog kind
// （openclaw 是其中之一），而正式庫裡每一筆 openclaw 意圖都是從部署或 profile
// 指派那條路寫進去的。要測的是讀的人看到什麼，所以寫進去的形狀必須跟正式庫一樣。
func mustIntent(t *testing.T, s *Store, scopeType, scopeID, resourceKind, resourceID, spec string, at time.Time) int64 {
	t.Helper()
	tx, err := s.DB().Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback()
	_, rev, err := createDesiredStateTx(tx, scopeType, scopeID, resourceKind, resourceID, spec, "test", at)
	if err != nil {
		t.Fatalf("create desired state %s:%s %s: %v", scopeType, scopeID, resourceID, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return int64(rev)
}

func openclawSpec(version string) string {
	return `{"kind":"openclaw","version":"` + version + `","artifact":{"sha256":"` +
		"40784b514e5e0e3a82b4da484ca0e3f0e6d8e9b2c1a4f7d3e0b6c9a2f5d8e1b4" + `"}}`
}

func intentsByScope(t *testing.T, s *Store) map[string]InstallIntent {
	t.Helper()
	rows, err := s.FleetInstallIntents()
	if err != nil {
		t.Fatalf("fleet install intents: %v", err)
	}
	out := make(map[string]InstallIntent, len(rows))
	for _, row := range rows {
		key := row.ScopeType + ":" + row.ScopeID + ":" + row.ResourceKind + ":" + row.ResourceID
		if _, duplicate := out[key]; duplicate {
			t.Fatalf("同一個 scope×資源回了兩列：%s", key)
		}
		out[key] = row
	}
	return out
}

func TestOnlyTheLastThingWeToldAScopeToInstallComesBack(t *testing.T) {
	s := newTestStore(t)
	at := time.Date(2026, 9, 6, 15, 0, 0, 0, time.UTC)
	mustIntent(t, s, "machine", "samplehub1", "openclaw", "openclaw", openclawSpec("2026.6.6"), at)
	mustIntent(t, s, "machine", "samplehub1", "openclaw", "openclaw", openclawSpec("2026.6.10"), at.Add(time.Minute))

	got := intentsByScope(t, s)
	intent, ok := got["machine:samplehub1:openclaw:openclaw"]
	if !ok {
		t.Fatalf("samplehub1 的安裝意圖不見了：%+v", got)
	}
	if intent.Version != "2026.6.10" {
		t.Errorf("回的不是最後一筆：version=%q", intent.Version)
	}
}

// 正式庫 2026-09-12 的形狀：四台的 machine scope 最後一筆都是診斷 noop，
// 而它們前面各有一筆真的安裝意圖。把 noop 當成意圖的話，這一頁會說
// 「Hub 最後叫這四台裝 noop」，而那是一句沒有人下過的指令。
func TestADiagnosticNoopDoesNotErasetheInstallItFollows(t *testing.T) {
	s := newTestStore(t)
	at := time.Date(2026, 9, 6, 16, 0, 0, 0, time.UTC)
	mustIntent(t, s, "machine", "sampleagent2", "openclaw", "openclaw", openclawSpec("2026.5.20"), at)
	mustIntent(t, s, "machine", "sampleagent2", "openclaw", "openclaw", `{"kind":"noop"}`, at.Add(time.Hour))

	intent, ok := intentsByScope(t, s)["machine:sampleagent2:openclaw:openclaw"]
	if !ok {
		t.Fatalf("診斷 noop 之後，sampleagent2 的安裝意圖整列不見了")
	}
	if intent.Kind != "openclaw" || intent.Version != "2026.5.20" {
		t.Errorf("noop 蓋掉了它前面那一筆安裝：kind=%q version=%q", intent.Kind, intent.Version)
	}
}

// 一個 scope 從頭到尾只有診斷，就是沒有安裝意圖 —— 不准回一列 kind="noop"
// 讓畫面上看起來「有指派」。
func TestAScopeThatOnlyEverGotDiagnosticsHasNoInstallIntent(t *testing.T) {
	s := newTestStore(t)
	at := time.Date(2026, 9, 10, 9, 0, 0, 0, time.UTC)
	mustIntent(t, s, "machine", "sampleagent1", "openclaw", "openclaw", `{"kind":"noop"}`, at)

	if intent, ok := intentsByScope(t, s)["machine:sampleagent1:openclaw:openclaw"]; ok {
		t.Errorf("只下過診斷的機器被講成有安裝意圖：%+v", intent)
	}
}

// 兩個 scope 各回自己的最後一筆，而且 revision 比得出先後 —— 這是 samplehub1 的形狀：
// machine scope 先被指派 2026.6.10，之後 canary channel 被指派 2026.5.26。
// 回的人要拿得到兩筆與兩個 revision，才有辦法照 agent MaxSeen 的同一條規則判誰贏。
func TestBothScopesComeBackWithComparableRevisions(t *testing.T) {
	s := newTestStore(t)
	at := time.Date(2026, 9, 6, 16, 46, 0, 0, time.UTC)
	machineRev := mustIntent(t, s, "machine", "samplehub1", "openclaw", "openclaw", openclawSpec("2026.6.10"), at)
	channelRev := mustIntent(t, s, "channel", "canary", "openclaw", "openclaw", openclawSpec("2026.5.26"), at.Add(2*time.Hour))

	got := intentsByScope(t, s)
	machine, okMachine := got["machine:samplehub1:openclaw:openclaw"]
	channel, okChannel := got["channel:canary:openclaw:openclaw"]
	if !okMachine || !okChannel {
		t.Fatalf("兩個 scope 沒有各回一列：%+v", got)
	}
	if machine.Revision != machineRev || channel.Revision != channelRev {
		t.Errorf("revision 對不上帳本：machine=%d/%d channel=%d/%d",
			machine.Revision, machineRev, channel.Revision, channelRev)
	}
	if channel.Revision <= machine.Revision {
		t.Errorf("兩個 scope 的號碼帶沒有共用，先後比不出來：machine=%d channel=%d",
			machine.Revision, channel.Revision)
	}
	if machine.Version != "2026.6.10" || channel.Version != "2026.5.26" {
		t.Errorf("兩個 scope 的版號串到一起了：machine=%q channel=%q", machine.Version, channel.Version)
	}
}

// stable 的形狀：channel 上掛著機器，卻一列意圖都沒有。那必須是「零列」，
// 不能因為別的 channel 有意圖就借過來填。
func TestAChannelThatWasNeverDeployedToHasNoRowAtAll(t *testing.T) {
	s := newTestStore(t)
	at := time.Date(2026, 9, 6, 18, 0, 0, 0, time.UTC)
	mustIntent(t, s, "channel", "canary", "openclaw", "openclaw", openclawSpec("2026.5.26"), at)

	if intent, ok := intentsByScope(t, s)["channel:stable:openclaw:openclaw"]; ok {
		t.Errorf("沒有被部署過的 channel 憑空長出意圖：%+v", intent)
	}
}

// 同一個 scope 上兩個資源不准互相蓋。
func TestTwoResourcesOnOneScopeKeepTheirOwnLastIntent(t *testing.T) {
	s := newTestStore(t)
	at := time.Date(2026, 9, 6, 15, 0, 0, 0, time.UTC)
	mustIntent(t, s, "machine", "samplehub1", "openclaw", "openclaw", openclawSpec("2026.6.10"), at)
	mustIntent(t, s, "machine", "samplehub1", "node-runtime", "node-24", `{"kind":"node-runtime","version":"24.8.0"}`, at.Add(time.Minute))

	got := intentsByScope(t, s)
	openclaw, okOpenClaw := got["machine:samplehub1:openclaw:openclaw"]
	node, okNode := got["machine:samplehub1:node-runtime:node-24"]
	if !okOpenClaw || !okNode {
		t.Fatalf("同一台上的兩個資源沒有各回一列：%+v", got)
	}
	if openclaw.Version != "2026.6.10" || node.Version != "24.8.0" {
		t.Errorf("兩個資源的最後一筆串到一起了：openclaw=%q node=%q", openclaw.Version, node.Version)
	}
}

// profile 指派會在同一台上寫進同一個 executor kind 的多個套件，
// 它們只差 resource_id。key 少掉 resource_id 的話，一台機器上裝的
// 兩個套件會只剩一個，而畫面上看起來像「另一個沒有被指派過」。
func TestTwoPackagesOfTheSameKindOnOneMachineKeepTheirOwnIntent(t *testing.T) {
	s := newTestStore(t)
	at := time.Date(2026, 9, 6, 15, 0, 0, 0, time.UTC)
	mustIntent(t, s, "machine", "samplehub1", "node-runtime", "node-24", `{"kind":"node-runtime","version":"24.8.0"}`, at)
	mustIntent(t, s, "machine", "samplehub1", "node-runtime", "node-22", `{"kind":"node-runtime","version":"22.14.0"}`, at.Add(time.Minute))

	got := intentsByScope(t, s)
	newer, okNewer := got["machine:samplehub1:node-runtime:node-24"]
	older, okOlder := got["machine:samplehub1:node-runtime:node-22"]
	if !okNewer || !okOlder {
		t.Fatalf("同一個 kind 的兩個套件沒有各回一列：%+v", got)
	}
	if newer.Version != "24.8.0" || older.Version != "22.14.0" {
		t.Errorf("兩個套件的意圖串到一起了：node-24=%q node-22=%q", newer.Version, older.Version)
	}
}

// 兩台機器對同一個資源各有自己的意圖。key 少掉 scope_id 的話，後指派的那一台
// 會把先指派的那一台整列吃掉 —— 而被吃掉的那一台會在畫面上變成「沒有指派」。
func TestTwoMachinesOnTheSameResourceDoNotShareOneIntent(t *testing.T) {
	s := newTestStore(t)
	at := time.Date(2026, 9, 6, 15, 37, 0, 0, time.UTC)
	mustIntent(t, s, "machine", "sampleagent3", "openclaw", "openclaw", openclawSpec("2026.6.10"), at)
	mustIntent(t, s, "machine", "sampleagent4", "openclaw", "openclaw", openclawSpec("2026.5.26"), at.Add(23*time.Minute))

	got := intentsByScope(t, s)
	sampleagent3, okSampleAgent3 := got["machine:sampleagent3:openclaw:openclaw"]
	sampleagent4, okSampleAgent4 := got["machine:sampleagent4:openclaw:openclaw"]
	if !okSampleAgent3 || !okSampleAgent4 {
		t.Fatalf("兩台機器沒有各回一列：%+v", got)
	}
	if sampleagent3.Version != "2026.6.10" || sampleagent4.Version != "2026.5.26" {
		t.Errorf("兩台的意圖串到一起了：sampleagent3=%q sampleagent4=%q", sampleagent3.Version, sampleagent4.Version)
	}
}

// 版號只能是 spec 自己講的。spec 沒講就是空字串 —— 不准拿 resource_id 或別的欄位補。
func TestAnIntentThatNamesNoVersionSaysNoVersion(t *testing.T) {
	s := newTestStore(t)
	at := time.Date(2026, 9, 6, 15, 0, 0, 0, time.UTC)
	mustIntent(t, s, "machine", "sampleagent4", "hermes-agent", "hermes-1.4.2", `{"kind":"hermes-agent"}`, at)

	intent, ok := intentsByScope(t, s)["machine:sampleagent4:hermes-agent:hermes-1.4.2"]
	if !ok {
		t.Fatalf("沒講版號的意圖整列不見了")
	}
	if intent.Version != "" {
		t.Errorf("spec 沒講版號，卻生出一個版號：%q", intent.Version)
	}
	if intent.Kind != "hermes-agent" {
		t.Errorf("kind 讀錯：%q", intent.Kind)
	}
}

// 解不開的 spec 不算安裝意圖，而且不准把它前面那一筆一起吃掉。
func TestAnUnreadableSpecDoesNotSwallowTheInstallBeforeIt(t *testing.T) {
	s := newTestStore(t)
	at := time.Date(2026, 9, 6, 15, 0, 0, 0, time.UTC)
	mustIntent(t, s, "machine", "sampleagent3", "openclaw", "openclaw", openclawSpec("2026.6.10"), at)
	mustIntent(t, s, "machine", "sampleagent3", "openclaw", "openclaw", `not json`, at.Add(time.Minute))

	intent, ok := intentsByScope(t, s)["machine:sampleagent3:openclaw:openclaw"]
	if !ok {
		t.Fatalf("解不開的 spec 把 sampleagent3 整列吃掉了")
	}
	if intent.Version != "2026.6.10" {
		t.Errorf("解不開的 spec 被當成最後一筆：version=%q", intent.Version)
	}
}

// 每一列都要帶得回帳本上那一筆，否則畫面上講的「誰在什麼時候指派的」無從查證。
func TestEveryIntentCarriesTheLedgerRowItCameFrom(t *testing.T) {
	s := newTestStore(t)
	at := time.Date(2026, 9, 6, 18, 25, 10, 0, time.UTC)
	mustIntent(t, s, "channel", "canary", "openclaw", "openclaw", openclawSpec("2026.5.26"), at)

	intent, ok := intentsByScope(t, s)["channel:canary:openclaw:openclaw"]
	if !ok {
		t.Fatalf("意圖不見了")
	}
	if intent.DesiredID == "" {
		t.Error("沒有 desired_id，查不回帳本那一列")
	}
	if !intent.CreatedAt.Equal(at) {
		t.Errorf("指派時間讀錯：%s，帳本是 %s", intent.CreatedAt.Format(time.RFC3339), at.Format(time.RFC3339))
	}
	if intent.CreatedBy != "test" {
		t.Errorf("指派人讀錯：%q", intent.CreatedBy)
	}
	if intent.ResourceKind != "openclaw" || intent.ResourceID != "openclaw" {
		t.Errorf("資源身分讀錯：%s:%s", intent.ResourceKind, intent.ResourceID)
	}
}

// 沒有任何一筆 desired_state 的帳本，回的是零列，不是錯誤。
func TestAnEmptyLedgerHasNoIntentsAndNoError(t *testing.T) {
	s := newTestStore(t)
	rows, err := s.FleetInstallIntents()
	if err != nil {
		t.Fatalf("空帳本回錯誤：%v", err)
	}
	if len(rows) != 0 {
		t.Errorf("空帳本回了 %d 列：%+v", len(rows), rows)
	}
}
