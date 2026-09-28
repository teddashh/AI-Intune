package store

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/deploy"
	"github.com/teddashh/AI-Intune/internal/model"
	"github.com/teddashh/AI-Intune/internal/rollout"
)

// checkinAt 塞一顆心跳進去，sent_at 與 received_at 都用同一個時刻。
func checkinAt(t *testing.T, s *Store, machineID string, at time.Time) {
	t.Helper()
	if err := s.RecordCheckin(machineID, model.Checkin{
		SchemaVersion: model.SchemaVersion, SentAt: at,
		AgentVersion: "test", BootID: "boot-1", AgentSeq: 1,
	}, at); err != nil {
		t.Fatalf("checkin at %v: %v", at, err)
	}
}

// observeAt 塞一批觀測進去。
func observeAt(t *testing.T, s *Store, machineID string, at time.Time) {
	t.Helper()
	if err := s.RecordObservation(machineID, healthyBatch(at), at); err != nil {
		t.Fatalf("observe at %v: %v", at, err)
	}
}

// rowsFor 是「這台機器在這張表上還剩幾列」。
func rowsFor(t *testing.T, s *Store, table, machineID string) int {
	t.Helper()
	return countRows(t, s,
		fmt.Sprintf(`SELECT COUNT(*) FROM %s WHERE machine_id = ?`, table), machineID)
}

func TestPruneNeverDeletesWorkloadCoverageEvidence(t *testing.T) {
	s := rolloutStore(t)
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	finished := now.Add(-40 * 24 * time.Hour)
	digest := strings.Repeat("a", 64)
	s.nowFn = func() time.Time { return now }
	addRolloutMachine(t, s, "cnode", "samplehub1", false)
	canary, jobs := createPromoteDeployment(t, s, "canary", "2026.9.2", digest, "",
		[]NewDeploymentTarget{{MachineID: "cnode", BatchNo: 1}})
	setPromoteJobState(t, s, jobs["cnode"], deploy.Succeeded, finished)
	// 這支 retention regression 要測舊 ledger 形狀，不走新的 finish-readiness
	// product path；deployment/job 本身已是成功終態。
	if _, err := s.DB().Exec(`UPDATE deployments SET state=?,finished_at=? WHERE deployment_id=?`,
		DeploymentFinished, fmtTime(finished), canary.DeploymentID); err != nil {
		t.Fatal(err)
	}
	unknownAt := finished.Add(time.Minute)
	recordPromoteCoverageObservation(t, s, unknownAt, "2026.9.2", func(b *model.ObservationBatch) {
		b.CLITools = nil
	})
	recordPromoteCoverageObservation(t, s, now, "2026.9.2", nil)

	before := rowsFor(t, s, "workload_observation_evidence", "cnode")
	facts, err := s.PromoteFacts("2026.9.2", digest, now)
	if err != nil {
		t.Fatal(err)
	}
	if decision := rollout.PromoteGate(facts, now, time.UTC); decision.Allowed {
		t.Fatalf("test setup: historical unknown did not lock promote: %s", decision.Summary())
	}
	if _, err := s.Prune(now, DefaultRetention(), false); err != nil {
		t.Fatal(err)
	}
	if after := rowsFor(t, s, "workload_observation_evidence", "cnode"); after != before {
		t.Fatalf("prune deleted append-only workload coverage: %d -> %d", before, after)
	}
	var unknown int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM workload_observation_evidence
	 WHERE machine_id='cnode' AND verdict=?`, workloadWitnessUnknown).Scan(&unknown); err != nil || unknown != 1 {
		t.Fatalf("historical unknown was pruned: count=%d err=%v", unknown, err)
	}
	facts, err = s.PromoteFacts("2026.9.2", digest, now)
	if err != nil {
		t.Fatal(err)
	}
	if decision := rollout.PromoteGate(facts, now, time.UTC); decision.Allowed {
		t.Fatalf("prune washed historical unknown green: %s", decision.Summary())
	}
}

// ⚠⚠ 這是這個檔案裡唯一真正重要的一支測試。
//
// 一台四十天前死掉的機器，它身上**每一列**資料都在任何合理的保留期之外。
// 一個只看時間的 prune 會把它清得一乾二淨，然後畫面上那台機器會從
// 「40 天前最後一次回報」變成「從未回報」。
//
// 那正好是 sampleagent3 隱形七週的那個 bug —— 只是這次是我們自己動手做的，
// 而且會在每天固定時間、對每一台死掉的機器、自動做一次。
//
// ⚠ 死掉的機器是**最需要**留住那一筆的：活著的機器隨時會再送一筆新的，
// 死掉的那台永遠不會。prune 對它的傷害是永久的。
func TestPruneNeverTurnsADeadMachineIntoAMachineThatNeverReported(t *testing.T) {
	s := newTestStore(t)
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	died := now.Add(-40 * 24 * time.Hour)

	dead := mustEnroll(t, s, "long-gone", died.Add(-time.Hour))
	checkinAt(t, s, dead, died)
	observeAt(t, s, dead, died)

	// 先確認在 prune 之前，這一頁真的講得出「40 天前最後一次回報」。
	// ⚠ 沒有這一段的話，後面那個斷言可能從頭到尾都在對著一個空的 Detail 說話。
	before, err := s.Detail(dead, now)
	if err != nil {
		t.Fatalf("detail before: %v", err)
	}
	if !before.Facts.EverCheckedIn {
		t.Fatalf("設定就錯了：prune 之前這台就已經是「從未回報」，"+
			"這支測試接下來什麼都測不到（last=%v）", before.Facts.LastCheckinReceived)
	}

	rep, err := s.Prune(now, DefaultRetention(), false)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}

	after, err := s.Detail(dead, now)
	if err != nil {
		t.Fatalf("detail after: %v", err)
	}
	if !after.Facts.EverCheckedIn {
		t.Errorf("prune 把一台死掉的機器變成「從未回報」了。\n"+
			"這台 40 天前最後一次回報，現在畫面上會說它從來沒有存在過 ——\n"+
			"那是這個專案存在的理由裡，最不能自己犯的那一個。\n"+
			"報告：刪了 %d 列，留了 %d 列", rep.Total(), rep.KeptNewest)
	}
	if got := after.Facts.LastCheckinReceived; !got.Equal(died) {
		t.Errorf("最後回報時間被改掉了：要 %v，是 %v", died, got)
	}
	if rowsFor(t, s, "machine_checkins", dead) != 1 {
		t.Errorf("死掉的機器身上該剩下正好 1 顆心跳（最後那顆），剩 %d 顆",
			rowsFor(t, s, "machine_checkins", dead))
	}
	if rowsFor(t, s, "observed_state", dead) == 0 {
		t.Error("死掉的機器身上的觀測被清光了 —— 每一種 kind 的最後一筆都該留著")
	}
	// KeptNewest 是規則本身有在做事的證據，不是附帶資訊。
	if rep.KeptNewest == 0 {
		t.Error("KeptNewest 是 0，但這一輪明明有過界的最新筆被留下來 —— " +
			"那個計數壞了，於是畫面上會說「什麼都沒留」")
	}
}

// prune 要真的清得掉東西。
//
// ⚠ 一個什麼都不刪的 prune 會讓上面那支測試永遠是綠的。這兩支測試是一對，
// 少了任何一支，另一支都可以靠「整段註解掉」通過。
func TestPruneActuallyDeletesTheOldRowsItShould(t *testing.T) {
	s := newTestStore(t)
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	id := mustEnroll(t, s, "busy", now.Add(-60*24*time.Hour))

	// 60 天，每天一顆心跳 + 一批觀測。
	for d := 60; d >= 0; d-- {
		at := now.Add(-time.Duration(d) * 24 * time.Hour)
		checkinAt(t, s, id, at)
		observeAt(t, s, id, at)
	}
	beforeCheckins := rowsFor(t, s, "machine_checkins", id)
	beforeObs := rowsFor(t, s, "observed_state", id)

	// 預演先跑：它必須數出跟真的刪一樣的數字，而且一列都不准動。
	dry, err := s.Prune(now, DefaultRetention(), true)
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if rowsFor(t, s, "machine_checkins", id) != beforeCheckins {
		t.Fatalf("--dry-run 真的刪了東西 —— 那個旗標的全部價值就是它不刪")
	}
	if dry.Total() == 0 {
		t.Fatal("預演說沒有東西要刪，但這台身上有 60 天的資料")
	}

	real, err := s.Prune(now, DefaultRetention(), false)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if real.Total() != dry.Total() {
		t.Errorf("預演說要刪 %d 列，真的跑刪了 %d 列 —— "+
			"預演的全部價值就在於它跟真的跑一樣", dry.Total(), real.Total())
	}

	// 14 天保留期：60 天的心跳應該只剩 15 顆左右（含今天），加上被保護的最後一筆。
	gotCheckins := rowsFor(t, s, "machine_checkins", id)
	if gotCheckins >= beforeCheckins {
		t.Errorf("心跳一顆都沒被清掉（%d → %d）", beforeCheckins, gotCheckins)
	}
	if gotCheckins > 20 {
		t.Errorf("14 天保留期之後還剩 %d 顆心跳，太多了 —— 界線沒有生效", gotCheckins)
	}
	// 保留期內的那些一顆都不准少。
	if gotCheckins < 15 {
		t.Errorf("只剩 %d 顆心跳，但保留期內有 15 天 —— "+
			"界線切進了畫面正在讀的範圍", gotCheckins)
	}
	if rowsFor(t, s, "observed_state", id) >= beforeObs {
		t.Errorf("觀測一列都沒被清掉（%d 列）", beforeObs)
	}

	// 最新那一顆心跳必須是今天的，不是 14 天前的。
	d, err := s.Detail(id, now)
	if err != nil {
		t.Fatalf("detail: %v", err)
	}
	if got := d.Facts.LastCheckinReceived; !got.Equal(now) {
		t.Errorf("prune 之後最後回報變成 %v，應該還是 %v", got, now)
	}
}

// 固定工作原語能力是 exact check-in 的從屬資料，不是另一份永遠增長的名冊。
// prune 刪掉過期 check-in 時，FK cascade 必須在同一 transaction 把能力列一起拿掉；
// 最新 check-in 的能力仍要留下，否則 readiness 會把已部署的新 agent 看成舊 agent。
func TestCheckinCapabilityFollowsCheckinRetention(t *testing.T) {
	s := newTestStore(t)
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	id := mustEnroll(t, s, "capable", now.Add(-60*24*time.Hour))
	for _, age := range []time.Duration{60 * 24 * time.Hour, 40 * 24 * time.Hour, 0} {
		at := now.Add(-age)
		checkin := model.Checkin{
			SchemaVersion: model.SchemaVersion, SentAt: at, AgentVersion: "test",
			BootID: "boot-1", AgentSeq: 1, DeviceSyncV1: true,
		}
		if err := s.RecordCheckin(id, checkin, at); err != nil {
			t.Fatalf("checkin at %v: %v", at, err)
		}
	}
	if got := rowsFor(t, s, "machine_job_capabilities", id); got != 3 {
		t.Fatalf("prune 前能力列=%d, want 3", got)
	}
	if _, err := s.Prune(now, DefaultRetention(), false); err != nil {
		t.Fatal(err)
	}
	if got := rowsFor(t, s, "machine_checkins", id); got != 1 {
		t.Fatalf("prune 後 check-in=%d, want 1", got)
	}
	if got := rowsFor(t, s, "machine_job_capabilities", id); got != 1 {
		t.Fatalf("prune 後能力列=%d, want 1；能力沒有跟 exact check-in 同生同滅", got)
	}
	readiness, err := s.AgentReadiness(id)
	if err != nil {
		t.Fatal(err)
	}
	if readiness.DeviceSyncV1 == nil || !*readiness.DeviceSyncV1 {
		t.Fatalf("最新能力被 prune 拿掉：%+v", readiness)
	}
}

// 一台時鐘慢了兩年的機器，它剛剛送上來的資料不准被當成舊資料清掉。
//
// ⚠ 這支測試釘的是 cutCol 一定要是 received_at（Hub 的鐘），
// 不是機器自報的 measured_at / sent_at。
//
// ⚠⚠ 這支測試的第一個版本是**假綠的**，而且是在突變測試裡才抓到的：
// 它只塞一列資料進去。一列的時候，那一列必然是它那一組的最新一筆，
// 於是「不刪最新一筆」那條規則會無條件保護它 —— 不管 cutCol 用哪一欄。
// 測試通過了，但它證明的是別的東西。
//
// 所以這裡刻意塞**三列**：讓「最新一筆」的保護只夠救一列，
// 另外兩列的生死就完全取決於 cutCol 選了誰的鐘。
// 這是今天第二次遇到同一件事 ——「一個看起來在測 A 的斷言，其實在測 B」。
//
// ⚠ 這不是假設出來的邊角。schema.sql 對 machine_checkins.received_at 的註解
// 與 deadsignal.go:111 都已經為了同一件事寫過同一條規則。prune 是這條規則
// 後果最嚴重的地方：別的地方看錯窗口只是少講一句話，這裡是直接把證據刪掉。
func TestPruneUsesTheHubsClockNotTheMachines(t *testing.T) {
	s := newTestStore(t)
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	id := mustEnroll(t, s, "bad-clock", now.Add(-time.Hour))

	// 機器說「現在是 2024 年」，但這三筆全部是 Hub 剛剛收到的。
	skewed := now.Add(-2 * 365 * 24 * time.Hour)
	const batches = 3
	for i := 0; i < batches; i++ {
		sent := skewed.Add(time.Duration(i) * time.Minute)
		recv := now.Add(-time.Duration(batches-i) * time.Minute)
		if err := s.RecordCheckin(id, model.Checkin{
			SchemaVersion: model.SchemaVersion, SentAt: sent,
			AgentVersion: "test", BootID: "boot-1", AgentSeq: 1,
		}, recv); err != nil {
			t.Fatalf("checkin %d: %v", i, err)
		}
		if err := s.RecordObservation(id, healthyBatch(sent), recv); err != nil {
			t.Fatalf("observe %d: %v", i, err)
		}
	}

	beforeObs := rowsFor(t, s, "observed_state", id)
	// ⚠ 先確認這支測試有東西可以測：三列都不在保護範圍內的話，
	// 下面的斷言就又變成在測「最新一筆有沒有被保護」了。
	if n := rowsFor(t, s, "machine_checkins", id); n != batches {
		t.Fatalf("設定就錯了：塞了 %d 顆心跳只進去 %d 顆，"+
			"這支測試會退化成「只有一列」那個假綠的版本", batches, n)
	}

	if _, err := s.Prune(now, DefaultRetention(), false); err != nil {
		t.Fatalf("prune: %v", err)
	}

	if n := rowsFor(t, s, "machine_checkins", id); n != batches {
		t.Errorf("一台時鐘慢兩年的機器，剛送上來的心跳被當成舊資料刪掉了"+
			"（%d 顆剩 %d 顆）—— 時間窗要圈 Hub 的 received_at，"+
			"不是機器自報的 sent_at/measured_at", batches, n)
	}
	if n := rowsFor(t, s, "observed_state", id); n != beforeObs {
		t.Errorf("剛收到的觀測被當成兩年前的資料刪掉了（%d 列剩 %d 列）", beforeObs, n)
	}
}

// prune 不准碰名冊、audit、狀態歷史。
//
// ⚠ 這支測試不是在測 Prune 寫了什麼，是在測它**沒有**寫什麼。
// 一個「順手把退役很久的機器從名冊清掉」的 prune 會直接違反
// 「名冊是分母」—— 而那種修改看起來會非常合理。
// ⚠⚠ 這支測試的第一個版本是假綠的，而突變測試花了兩輪才問對問題。
//
// 第一輪：我在 Prune 裡加了一句「順手清掉退役超過一年的機器」，
// 測試還是綠的。以為是 machine_checkins 的 FK 擋住了，於是又加了一台
// 「從未報到、身上一列子資料都沒有」的機器 —— 還是綠的。
//
// 真正的原因量出來才知道：`enrollment_tokens.used_by` 也 REFERENCES 名冊
// （schema.sql:49），而**每一台機器都是從一張兌換掉的票生出來的**。
// 所以每一台機器身上永遠至少掛著一個 FK，registry 的 DELETE 一定會失敗：
//
//	constraint failed: FOREIGN KEY constraint failed (787)
//
// 也就是說「機器不會從名冊上消失」這件事，是 **schema 在保證**，不是 Prune。
// 那麼這支測試就不該假裝自己在測 Prune 的邏輯 —— 它該去釘住那個真正的保證，
// 而那個保證會被一種很合理的修改破壞掉：給那些 FK 加上 ON DELETE CASCADE。
// 加上去之後，一句 registry 的 DELETE 會安靜地把那台機器的每一絲痕跡帶走。
//
// ⚠ 一支測不到自己聲稱在測的東西的測試，比沒有測試更糟：它會讓人以為查過了。
func TestNothingCanMakeAMachineLeaveTheRoster(t *testing.T) {
	s := newTestStore(t)
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	old := now.Add(-3 * 365 * 24 * time.Hour)

	withData := mustEnroll(t, s, "ancient", old)
	checkinAt(t, s, withData, old)

	// ⚠ 這台三年前開了票、機器從來沒有裝上 agent。它身上一列子資料都沒有，
	// 所以任何「清掉舊機器」的 SQL 都會成功。它必須留在名冊上亮紅燈。
	neverCame := mustEnroll(t, s, "never-showed-up", old)

	for _, id := range []string{withData, neverCame} {
		if err := s.RetireMachine(id, now.Add(-2*365*24*time.Hour)); err != nil {
			t.Fatalf("retire %s: %v", id, err)
		}
		if err := s.RecordAudit(AuditEntry{
			At: old, Action: AuditRetire, MachineID: id,
			Subject: "ancient", SourceAddr: "100.64.200.2", OK: true,
		}); err != nil {
			t.Fatalf("audit: %v", err)
		}
	}
	// ⚠⚠ 這是這支測試真正的斷言：資料庫本身要拒絕讓一台機器離開名冊。
	//
	// 兩台都要試 —— 一台有子資料（被 machine_checkins 的 FK 擋住），
	// 一台從未報到（只剩 enrollment_tokens.used_by 擋著它）。
	// 第二台才是有意義的那個：它是那格「從未報到」紅燈，
	// 而那格紅燈是整個產品第一個要答對的問題。
	for _, id := range []string{withData, neverCame} {
		if _, err := s.db.Exec(
			`DELETE FROM machine_registry WHERE machine_id = ?`, id); err == nil {
			t.Errorf("一句 DELETE 就把 %s 從名冊上帶走了，資料庫沒有攔 —— "+
				"如果那些 FK 被加上 ON DELETE CASCADE，這一句會連帶清掉"+
				"它的心跳、觀測、狀態歷史。名冊是分母，"+
				"一台不在名冊上的機器沒有任何一個畫面會再提起它", id)
		}
	}

	if _, err := s.Prune(now, DefaultRetention(), false); err != nil {
		t.Fatalf("prune: %v", err)
	}

	for _, id := range []string{withData, neverCame} {
		var n int
		if err := s.db.QueryRow(
			`SELECT COUNT(*) FROM machine_registry WHERE machine_id = ?`, id).
			Scan(&n); err != nil {
			t.Fatalf("count registry: %v", err)
		}
		if n != 1 {
			t.Errorf("prune 把 %s 從名冊上刪掉了 —— 名冊是分母，一列都不准清。"+
				"一台不在名冊上的機器，沒有任何一個畫面會再提起它", id)
		}
		if err := s.db.QueryRow(
			`SELECT COUNT(*) FROM audit_log WHERE machine_id = ?`, id).
			Scan(&n); err != nil {
			t.Fatalf("count audit: %v", err)
		}
		if n != 1 {
			t.Errorf("prune 清掉了 %s 的 audit_log —— 那是人做過什麼的唯一紀錄", id)
		}
	}
}

// 一個切進讀取窗口的保留期，要被擋在門口，而且不准被偷偷改成合法值。
func TestRetentionPolicyRefusesToCutIntoAReadWindow(t *testing.T) {
	s := newTestStore(t)
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)

	bad := RetentionPolicy{
		Observations: 3 * 24 * time.Hour, // < TicketWindow(7d)
		Checkins:     14 * 24 * time.Hour,
		Occupancy:    400 * 24 * time.Hour,
	}
	if err := bad.Validate(); err == nil {
		t.Fatal("observations 只留 3 天、但票的報表讀 7 天，Validate 說沒問題")
	}
	if _, err := s.Prune(now, bad, false); err == nil {
		t.Error("Prune 收下了一個會切進讀取窗口的政策 —— " +
			"那會讓報表安靜地少講一段時間，然後它會說「這段時間沒事」")
	}
	if err := (RetentionPolicy{}).Validate(); err == nil {
		t.Error("全 0 的政策要被擋掉 —— 0 的意思在這裡是「全部刪掉」")
	}
	if err := DefaultRetention().Validate(); err != nil {
		t.Errorf("預設值自己不合法：%v", err)
	}
}

// prune 刪了東西就要留下痕跡。
//
// ⚠ 一個安靜的 prune 會把「我們刪掉了」偽裝成「從來沒發生過」——
// 這個專案已經在自己身上犯過八次這個錯（§5.6–§5.14），不要有第九次。
func TestPruneLeavesATraceAndDryRunDoesNot(t *testing.T) {
	s := newTestStore(t)
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	id := mustEnroll(t, s, "loud", now.Add(-60*24*time.Hour))
	for d := 60; d >= 0; d -= 2 {
		checkinAt(t, s, id, now.Add(-time.Duration(d)*24*time.Hour))
	}

	if _, _, ok, _ := s.LastPrune(); ok {
		t.Fatal("還沒 prune 過就說 prune 過了")
	}
	if _, err := s.Prune(now, DefaultRetention(), true); err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if _, _, ok, _ := s.LastPrune(); ok {
		t.Error("--dry-run 寫了一筆清理紀錄 —— 它什麼都沒刪，不該留下刪過的痕跡")
	}

	rep, err := s.Prune(now, DefaultRetention(), false)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	at, rows, ok, err := s.LastPrune()
	if err != nil {
		t.Fatalf("last prune: %v", err)
	}
	if !ok {
		t.Fatal("prune 刪了東西卻沒留紀錄 —— 「被清掉了」跟「從來沒有」就分不出來了")
	}
	if !at.Equal(now) {
		t.Errorf("清理紀錄的時間是 %v，應該是 %v", at, now)
	}
	if rows != rep.Total() {
		t.Errorf("報告說刪了 %d 列，紀錄裡是 %d 列", rep.Total(), rows)
	}
}

func TestPruneRollsBackDeletesWhenRetentionEvidenceCannotBeWritten(t *testing.T) {
	s := newTestStore(t)
	now := time.Date(2026, 9, 3, 13, 0, 0, 0, time.UTC)
	id := mustEnroll(t, s, "atomic-retention", now.Add(-60*24*time.Hour))
	for _, age := range []time.Duration{60 * 24 * time.Hour, 45 * 24 * time.Hour, time.Hour} {
		at := now.Add(-age)
		checkinAt(t, s, id, at)
		observeAt(t, s, id, at)
	}
	beforeObservations := rowsFor(t, s, "observed_state", id)
	beforeCheckins := rowsFor(t, s, "machine_checkins", id)
	read, err := s.ReadChanges(ChangeReadRequest{From: now.Add(-24 * time.Hour), To: now})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := s.DB().Exec(`
CREATE TRIGGER reject_retention_evidence
BEFORE INSERT ON retention_log
BEGIN SELECT RAISE(ABORT, 'blocked retention evidence'); END`); err != nil {
		t.Fatal(err)
	}
	report, err := s.Prune(now, DefaultRetention(), false)
	if err == nil || !strings.Contains(err.Error(), "retention log") || report.Total() == 0 {
		t.Fatalf("failed retention evidence result: report=%+v err=%v", report, err)
	}
	if got := rowsFor(t, s, "observed_state", id); got != beforeObservations {
		t.Fatalf("failed retention evidence deleted observations: %d -> %d", beforeObservations, got)
	}
	if got := rowsFor(t, s, "machine_checkins", id); got != beforeCheckins {
		t.Fatalf("failed retention evidence deleted checkins: %d -> %d", beforeCheckins, got)
	}
	if got := countRows(t, s, `SELECT COUNT(*) FROM retention_log`); got != 0 {
		t.Fatalf("failed retention transaction left %d log rows", got)
	}
	if _, err := s.ReadChanges(ChangeReadRequest{
		From: now.Add(-24 * time.Hour), To: now, Ceilings: &read.Ceilings,
	}); err != nil {
		t.Fatalf("rolled-back prune invalidated a fixed change traversal: %v", err)
	}
}

// pruneJobs 裡的每一個欄名都要真的存在。
//
// ⚠ SQLite 的欄名打錯是執行期才炸的，而 prune 一天只跑一次 ——
// 一個打錯的欄名可以安靜地待很久，然後在沒有人看的凌晨三點炸掉，
// 那一天什麼都沒清，而且沒有人會知道。
func TestEveryPruneJobNamesRealColumns(t *testing.T) {
	s := newTestStore(t)
	for _, j := range pruneJobs {
		cols := append([]string{j.cutCol}, j.newestCols...)
		cols = append(cols, "machine_id")
		for _, c := range cols {
			if _, err := s.db.Exec(fmt.Sprintf(
				`SELECT %s FROM %s LIMIT 0`, c, j.table)); err != nil {
				t.Errorf("%s.%s 不存在：%v", j.table, c, err)
			}
		}
		// groupBy 也要跑得起來 —— 它是手寫的字串，最容易打錯的就是它。
		if _, err := s.db.Exec(fmt.Sprintf(
			`SELECT COUNT(*) FROM %[1]s WHERE %[1]s.%[2]s < (SELECT MAX(o2.%[2]s) FROM %[1]s o2 WHERE %[3]s)`,
			j.table, j.cutCol, j.groupBy)); err != nil {
			t.Errorf("%s 的 groupBy 跑不起來：%v", j.table, err)
		}
		// ⚠ cutCol 一律是 Hub 的鐘。canary_silent_failures.last_seen_at 是 reconcile
		// 當下的 Hub 時鐘，不是機器自報時間。
		isHubClock := j.cutCol == "received_at" || (j.table == "canary_silent_failures" && j.cutCol == "last_seen_at")
		if !isHubClock {
			t.Errorf("%s 的 cutCol 是 %q —— 圈時間一律用 Hub 的 received_at，"+
				"用機器自報的時間會把一台時鐘慢的機器的新資料當舊資料刪掉",
				j.table, j.cutCol)
		}
	}
}

// prune 的每一句相關子查詢都必須是 index-only（COVERING INDEX）。
//
// ⚠⚠ 這支測試守的是一個**一年後才會發作**的問題，所以它必須現在就存在。
//
// prune 的條件是「過界 AND 不是這一組最新的一筆」，那個「最新」是一句
// 相關子查詢。子查詢沒有覆蓋索引的時候，它會為了算一個 MAX 去回表翻
// 那一組的每一列 —— 外層又是全表掃描，總成本是 N × G。
//
// 實測（samplehub1，2026-09-03）N=446，跑 7 毫秒，什麼問題都看不出來。
// 但成長率是 4.5 MB/天（約 3500 列/天），一年後 N 超過一百萬 ——
// 那時這句 SQL 會從 7 毫秒變成跑不完，而它是半夜自己跑的，
// 沒有人在看，而且失敗的後果是「什麼都沒清」，跟「沒東西要清」長得一樣。
//
// ⚠ EXPLAIN 出來的字裡有 SEARCH 沒有 COVERING，就是在回表。那兩個字是
// 這支測試唯一在看的東西。實測 ix_occupancy_prune 的欄位順序寫錯的時候，
// planner 會自己改去挑 ix_occupancy_machine，於是 provider 只能一列一列比對
// —— 那個錯誤除了 EXPLAIN 之外沒有任何地方看得出來。
func TestEveryPruneSubqueryIsIndexOnly(t *testing.T) {
	s := newTestStore(t)
	for _, j := range pruneJobs {
		del, _ := j.where()
		q := fmt.Sprintf(`EXPLAIN QUERY PLAN SELECT COUNT(*) FROM %s WHERE %s`, j.table, del)
		rows, err := s.db.Query(q, "2020-01-01T00:00:00Z")
		if err != nil {
			t.Fatalf("explain %s: %v", j.table, err)
		}
		var plans []string
		for rows.Next() {
			var id, parent, notused int
			var detail string
			if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
				rows.Close()
				t.Fatalf("scan plan: %v", err)
			}
			plans = append(plans, detail)
		}
		rows.Close()

		var inSub bool
		for _, d := range plans {
			if strings.Contains(d, "CORRELATED SCALAR SUBQUERY") {
				inSub = true
				continue
			}
			if !inSub {
				continue // 外層那一次掃描，一天一次，無所謂
			}
			if strings.Contains(d, "SEARCH") && !strings.Contains(d, "COVERING INDEX") {
				t.Errorf("%s 的「不刪最新一筆」子查詢會回表：\n  %s\n"+
					"完整計劃：\n  %s\n"+
					"少了覆蓋索引，這句 SQL 的成本是 N×G。現在資料少看不出來，"+
					"一年後它會在半夜跑不完，而「什麼都沒清」跟「沒東西要清」"+
					"在畫面上長得一模一樣。看 schema.sql 的 ix_%s_prune。",
					j.table, d, strings.Join(plans, "\n  "), j.table)
			}
		}
		if !inSub {
			t.Errorf("%s 的計劃裡沒有相關子查詢 —— "+
				"那表示「不刪最新一筆」那段條件不見了", j.table)
		}
	}
}
