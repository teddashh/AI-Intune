package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/deploy"
	"github.com/teddashh/AI-Intune/internal/model"
	"github.com/teddashh/AI-Intune/internal/state"
	"github.com/teddashh/AI-Intune/internal/store"
)

// /metrics 的測試。
//
// ⚠ 這一頁的失效模式跟畫面**不一樣**，所以測法也不一樣。
// Dashboard 壞掉的時候有人在看，少一段、排版跑掉、字是亂的，人看得出來。
// metrics 沒有人在看，只有規則在看 —— 而規則看不出「這條線不見了」跟
// 「這個值是 0」的差別。所以這裡大部分的斷言不是在測數字對不對，
// 是在測**該出現的東西有沒有出現**。

// --- 一：格式。

// TestMetricsAreParseableExposition
//
// cmd/clawctl-hub/metrics.go 開頭那句「格式本身很穩定，而下面
// TestMetricsAreParseableExposition 在守它」指的就是這一支。手寫 exposition
// format 換掉一棵相依樹，代價就是格式錯了沒有人會告訴你 —— Prometheus 遇到
// 解析錯誤是**整頁丟掉**，而抓不到東西在圖上跟「機隊很安靜」長得一樣。
//
// ⚠⚠ **這支測試自己驗自己，那不算數。** 下面 parseExposition 是我寫的，
// 它守的是我對格式的理解；如果我理解錯了，程式跟測試會一起錯，然後一起變綠。
// 真正的外部驗證是 ops/check-metrics.sh：它拿**真的 Prometheus 官方 parser**
// （prometheus_client 的 text parser）去讀真的端點吐出來的東西。
// 那支腳本不在 go test 裡，因為它需要一個跑著的 Hub 跟一個 Python 環境 ——
// 它是部署路徑的一部分，不是單元測試的一部分。見 docs/PHASE1.md §5.19。
func TestMetricsAreParseableExposition(t *testing.T) {
	mux, st := metricsFixture(t)
	defer st.Close()

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /metrics = %d：%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q，Prometheus 要 text/plain", ct)
	}

	fam := parseExposition(t, rec.Body.String())

	// ⚠ 每一個宣告過 TYPE 的指標都必須至少有一行 sample。
	//
	// 這一條在守一個真的寫出來過的 bug：原本 DeadSignals / DoubleAgents 那兩段
	// 寫的是 `if err == nil { line(...) }`，於是查詢失敗時 HELP 跟 TYPE 照印、
	// sample 不見、然後回 200。一個「格式完全正確、只是少了一條線」的回應，
	// 是這一頁最危險的輸出 —— 因為它會被當成抓成功。
	for name, f := range fam {
		if len(f.samples) == 0 {
			t.Errorf("%s 宣告了 # TYPE 但一行 sample 都沒有 —— "+
				"對 Prometheus 來說這個指標等於不存在", name)
		}
	}

	// 該有的指標一個都不能少。這份清單是刻意抄一份的：
	// ⚠ 少掉一個指標不會讓任何東西壞掉，它只會讓那條線安靜地消失。
	for _, want := range []string{
		"clawctl_build_info",
		"clawctl_machines_total",
		"clawctl_machines_retired",
		"clawctl_machines",
		"clawctl_machine_state",
		"clawctl_machine_last_checkin_age_seconds",
		"clawctl_machine_clock_skew_seconds",
		"clawctl_jobs",
		"clawctl_jobs_nonterminal",
		"clawctl_deployments",
		"clawctl_dead_signal_pipelines",
		"clawctl_double_agent_machines",
		"clawctl_double_agent_machines_ongoing",
		// ⚠ 產出物與憑證那幾個指標**不在**這份清單裡，因為它們是有條件的：
		// 沒有人宣告任何期望的時候，就是沒有東西要量，那時候整塊不該出現
		// （只有 TYPE 沒有 sample 等於不存在，見上面那一段）。
		// 但「沒宣告」跟「期望檔載入失敗」長得一樣，所以下面這一條是無條件的：
		// 它讓 0 是一個看得見的數字。見 TestExpectationMetricsAppearOnlyWhenDeclared。
		"clawctl_expectations_loaded",
		"clawctl_notify_configured",
		"clawctl_notify_consecutive_failures",
		"clawctl_db_busy_total",
		"clawctl_db_write_wait_seconds",
		"clawctl_db_write_hold_seconds",
	} {
		if _, ok := fam[want]; !ok {
			t.Errorf("整頁裡找不到 %s", want)
		}
	}

	// fixture 沒有任何宣告，所以這個數字必須是 0 —— 而且必須**在場**。
	if got := fam["clawctl_expectations_loaded"].samples[0].value; got != 0 {
		t.Errorf("fixture 沒有宣告，clawctl_expectations_loaded 應該是 0，得到 %v", got)
	}
}

func TestLegacyExpectedMetricIsAbsent(t *testing.T) {
	mux, st := metricsFixture(t)
	defer st.Close()

	if body := getMetrics(t, mux); strings.Contains(body, "clawctl_machines_expected") {
		t.Fatalf("/metrics 仍輸出 legacy clawctl_machines_expected：\n%s", body)
	}
}

func TestGrafanaUsesLifecycleDenominatorMetric(t *testing.T) {
	raw, err := os.ReadFile("../../ops/prometheus/grafana/clawctl.json")
	if err != nil {
		t.Fatalf("讀 Grafana dashboard：%v", err)
	}
	var dashboard struct {
		Panels []struct {
			Title   string `json:"title"`
			Targets []struct {
				Expr string `json:"expr"`
			} `json:"targets"`
		} `json:"panels"`
	}
	if err := json.Unmarshal(raw, &dashboard); err != nil {
		t.Fatalf("解析 Grafana dashboard：%v", err)
	}

	want := map[string]bool{
		"名冊（分母）":   false,
		"機隊在幾個版本上": false,
	}
	for _, panel := range dashboard.Panels {
		for _, target := range panel.Targets {
			if target.Expr == "clawctl_machines_expected" {
				t.Errorf("Grafana panel %q 仍查詢 legacy clawctl_machines_expected", panel.Title)
			}
			if _, ok := want[panel.Title]; ok && target.Expr == "clawctl_machines_total" {
				want[panel.Title] = true
			}
		}
	}
	for panel, found := range want {
		if !found {
			t.Errorf("Grafana panel %q 沒有查詢 lifecycle 分母 clawctl_machines_total", panel)
		}
	}
}

// TestTheSameSeriesIsNeverEmittedTwice
//
// ⚠ 重複的 series（同名同 label）會讓 Prometheus 把**整頁**判成解析錯誤，
// 不是只丟掉那一行。
//
// ⚠⚠ 這支測試是一個 bug 的墓碑，而且那個 bug 是它自己抓到的。
// 第一版的 machine label 用的是 display_name，看起來完全合理 ——
// 直到這支測試建了兩台都叫 ubuntu 的機器（名冊沒有、也不該有
// 「顯示名稱不重複」這條限制），輸出裡就出現了兩行一模一樣的
// `clawctl_machine_state{machine="ubuntu",state="NeverReported"}`。
//
// 那個形狀跟「名字裡有引號」一樣：**一台機器的名字有能力讓整個機隊的
// 監控消失**。差別是同名機器發生的機率高得多 —— 雲上開兩台 Ubuntu 就中了。
// 修法是 label 帶 machine_id（不會重複、也不會被改名），display_name 留著給人看。
func TestTheSameSeriesIsNeverEmittedTwice(t *testing.T) {
	mux, st := metricsFixture(t)
	defer st.Close()

	// 兩台機器，同一個顯示名稱。名冊允許這件事。
	for i := 0; i < 2; i++ {
		if _, err := st.CreateEnrollToken("ubuntu", time.Hour); err != nil {
			t.Fatalf("開票: %v", err)
		}
	}

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /metrics = %d", rec.Code)
	}

	seen := map[string]int{}
	for _, f := range parseExposition(t, rec.Body.String()) {
		for _, s := range f.samples {
			seen[s.key()]++
		}
	}
	for k, n := range seen {
		if n > 1 {
			t.Errorf("同一條 series 出現 %d 次：%s\n"+
				"⚠ Prometheus 會把整頁丟掉，不是只丟這一行。", n, k)
		}
	}
}

// --- 二：名冊是分母。

// TestAMachineThatNeverReportedIsStillOnTheMetricsPage
//
// ⚠⚠ 這是這一頁存在的理由那一條。開一張票就是一個人說出「我打算納管這台」，
// 從那一刻起它在分母裡。如果 /metrics 只吐「有回報的機器」，那麼一台
// 從來沒報到過的機器在 Prometheus 眼中**根本不存在** —— 而它正是最該被看到的
// 那一台。sampleagent1 在某台 client 上隱形七週就是這個形狀。
func TestAMachineThatNeverReportedIsStillOnTheMetricsPage(t *testing.T) {
	mux, st := metricsFixture(t)
	defer st.Close()

	// 只開票、不報到 —— 名冊上有，但一顆心跳都沒有。
	if _, err := st.CreateEnrollToken("never-showed-up", time.Hour); err != nil {
		t.Fatalf("開票: %v", err)
	}

	fam := parseExposition(t, getMetrics(t, mux))

	got := labelValues(fam["clawctl_machine_state"], "machine")
	if !contains(got, "never-showed-up") {
		t.Fatalf("從來沒回報過的機器不在 clawctl_machine_state 上 —— "+
			"它在 Prometheus 裡等於不存在。看到的是：%v", got)
	}

	// ⚠ 它的狀態不能是空字串。一台從沒報到的機器有明確的判定（NeverReported），
	// 空的 state label 會讓告警規則寫不出來，然後那台機器就沒人管了。
	for _, s := range fam["clawctl_machine_state"].samples {
		if s.labels["machine"] == "never-showed-up" && strings.TrimSpace(s.labels["state"]) == "" {
			t.Error("從沒回報過的機器 state label 是空的")
		}
	}

	// ⚠ 但它**不該**有 age 那一行 —— 沒有「最後一次心跳」可以減。
	// 這件事刻意留成「沒有那一行」而不是「age = 0」或「age = 很大」：
	// 一個編出來的年齡會讓告警看起來有在守它，實際上守的是一個假的數字。
	// help 字串有把這件事寫出來，而這裡把它釘住。
	if ages := labelValues(fam["clawctl_machine_last_checkin_age_seconds"], "machine"); contains(ages, "never-showed-up") {
		t.Error("從沒回報過的機器竟然有 last_checkin_age —— 那個數字是編的")
	}
}

func TestClockSkewIsOnlyEmittedAfterCheckin(t *testing.T) {
	mux, st := metricsFixture(t)
	defer st.Close()

	checkedIn := enrollViaHTTP(t, mux, st, "checked-in-with-skew")
	receivedAt := time.Now().UTC().Add(-time.Second).Truncate(time.Second)
	if err := st.RecordCheckin(checkedIn.id, model.Checkin{
		SentAt:         receivedAt.Add(7 * time.Second),
		AgentStartedAt: receivedAt,
	}, receivedAt); err != nil {
		t.Fatalf("寫入有偏移的 check-in：%v", err)
	}

	// 只開票、不報到；它在名冊裡，但沒有 sent_at 與 received_at 可以相減。
	if _, err := st.CreateEnrollToken("never-checked-in-for-skew", time.Hour); err != nil {
		t.Fatalf("開票: %v", err)
	}

	fam := parseExposition(t, getMetrics(t, mux))
	skews := fam["clawctl_machine_clock_skew_seconds"]

	// ⚠ 沒量過就整行不該存在；輸出 0 會讓告警以為它量過，而且時鐘剛好準。
	if machines := labelValues(skews, "machine"); contains(machines, "never-checked-in-for-skew") {
		t.Error("從沒回報過的機器竟然有 clock_skew —— 那個 0 是 Go 零值，不是量測結果")
	}

	// ⚠ 正向也要釘數值，否則守衛若寫成永遠不輸出，負向斷言仍然會綠。
	for _, s := range skews.samples {
		if s.labels["machine"] == "checked-in-with-skew" {
			if s.value != 7 {
				t.Fatalf("checked-in-with-skew 的 clock_skew = %v，想要 7", s.value)
			}
			return
		}
	}
	t.Fatal("有 check-in 的 checked-in-with-skew 沒有 clock_skew 這一行")
}

// TestEveryStateAppearsEvenWhenTheCountIsZero
//
// ⚠ state.AllStates 裡的每一個都要有一行，包含數量是 0 的。
// 只吐有值的那些，會讓「Degraded 這條線消失」跟「Degraded 是 0」在圖上
// 長得一樣 —— 而前者的意思是這個欄位壞了。
func TestEveryStateAppearsEvenWhenTheCountIsZero(t *testing.T) {
	mux, st := metricsFixture(t)
	defer st.Close()

	fam := parseExposition(t, getMetrics(t, mux))
	got := labelValues(fam["clawctl_machines"], "state")
	for _, s := range state.AllStates {
		if !contains(got, string(s)) {
			t.Errorf("狀態 %s 沒有出現在 clawctl_machines 上（數量是 0 也要出現）。看到的是：%v", s, got)
		}
	}
	if len(got) != len(state.AllStates) {
		t.Errorf("clawctl_machines 有 %d 行，state.AllStates 有 %d 個：%v",
			len(got), len(state.AllStates), got)
	}
}

func TestJobMetricsIncludeZerosNonterminalAndLastTerminal(t *testing.T) {
	mux, st := metricsFixture(t)
	defer st.Close()
	machines, err := st.ListMachines()
	if err != nil || len(machines) != 1 {
		t.Fatalf("取測試機器失敗：machines=%d err=%v", len(machines), err)
	}
	machineID := machines[0].MachineID
	desiredID, rev, err := st.CreateDesiredState("machine", machineID, "openclaw", "openclaw", `{}`, "metrics 測試")
	if err != nil {
		t.Fatalf("建立期望狀態失敗：%v", err)
	}
	if _, err := st.CreateJob(machineID, desiredID, rev, store.NewJob{}); err != nil {
		t.Fatalf("建立 not_started 工作單失敗：%v", err)
	}
	terminalID, err := st.CreateJob(machineID, desiredID, rev+1, store.NewJob{})
	if err != nil {
		t.Fatalf("建立終態工作單失敗：%v", err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	if _, err := st.ClaimJob(terminalID, machineID, now, time.Minute); err != nil {
		t.Fatalf("領取終態工作單失敗：%v", err)
	}
	if _, err := st.AdvanceJobByHub(terminalID, deploy.LeaseLost, now.Add(time.Second)); err != nil {
		t.Fatalf("把工作單收成 lease_expired 失敗：%v", err)
	}

	fam := parseExposition(t, getMetrics(t, mux))
	jobs := fam["clawctl_jobs"]
	if jobs == nil || len(jobs.samples) != 9 {
		t.Fatalf("clawctl_jobs 應有九個狀態，拿到 %+v", jobs)
	}
	counts := map[string]float64{}
	for _, sample := range jobs.samples {
		counts[sample.labels["state"]] = sample.value
	}
	if counts[string(deploy.NotStarted)] != 1 || counts[string(deploy.LeaseExpired)] != 1 || counts[string(deploy.Running)] != 0 {
		t.Errorf("工作單狀態數不符：%v", counts)
	}
	if got := single(t, fam, "clawctl_jobs_nonterminal"); got != 1 {
		t.Errorf("非終態工作單 = %v，預期 1", got)
	}
	last := fam["clawctl_job_last_terminal_timestamp_seconds"]
	if last == nil || len(last.samples) != 1 || last.samples[0].labels["state"] != string(deploy.LeaseExpired) ||
		last.samples[0].value != float64(now.Add(time.Second).Unix()) {
		t.Errorf("最後終態時間不符：%+v", last)
	}
}

func TestDeploymentMetricsIncludeStateZerosAndRunningStuckZero(t *testing.T) {
	mux, st := metricsFixture(t)
	defer st.Close()
	machines, _ := st.ListMachines()
	if _, err := st.DB().Exec(`UPDATE machine_registry SET channel='canary' WHERE machine_id=?`, machines[0].MachineID); err != nil {
		t.Fatal(err)
	}
	d, _, err := st.CreateDeployment(store.NewDeployment{
		Channel: "canary", ResourceKind: "openclaw", ResourceID: "openclaw", Spec: `{"kind":"openclaw","version":"2026.9.2"}`,
		BatchSize: 5, CreatedBy: "metrics", Targets: []store.NewDeploymentTarget{{MachineID: machines[0].MachineID, BatchNo: 1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	fam := parseExposition(t, getMetrics(t, mux))
	states := fam["clawctl_deployments"]
	if states == nil || len(states.samples) != 3 {
		t.Fatalf("deployment 三個 state（含 0）都要出現：%+v", states)
	}
	counts := map[string]float64{}
	for _, sample := range states.samples {
		counts[sample.labels["state"]] = sample.value
	}
	if counts[store.DeploymentRunning] != 1 || counts[store.DeploymentPaused] != 0 || counts[store.DeploymentFinished] != 0 {
		t.Fatalf("deployment state counts=%v", counts)
	}
	stuck := fam["clawctl_deployment_stuck_machines"]
	if stuck == nil || len(stuck.samples) != 1 || stuck.samples[0].labels["deployment_id"] != d.DeploymentID || stuck.samples[0].value != 0 {
		t.Fatalf("running deployment 的 stuck 真 0 不在：%+v", stuck)
	}
	if fam["clawctl_deployment_last_finished_timestamp_seconds"] != nil {
		t.Fatal("沒有 finished deployment 時不准編一個時間")
	}
}

func TestDeploymentLastFinishedMetricAppearsOnlyAfterFinish(t *testing.T) {
	mux, st := metricsFixture(t)
	defer st.Close()
	machines, _ := st.ListMachines()
	if _, err := st.DB().Exec(`UPDATE machine_registry SET channel='canary' WHERE machine_id=?`, machines[0].MachineID); err != nil {
		t.Fatal(err)
	}
	d, jobs, err := st.CreateDeployment(store.NewDeployment{
		Channel: "canary", ResourceKind: "openclaw", ResourceID: "openclaw", Spec: `{"kind":"openclaw","version":"2026.9.2"}`,
		BatchSize: 5, CreatedBy: "metrics", Targets: []store.NewDeploymentTarget{{MachineID: machines[0].MachineID, BatchNo: 1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`UPDATE jobs SET state=?,terminal_at=? WHERE job_id=?`,
		deploy.Succeeded, time.Now().UTC().Format(time.RFC3339), jobs[0].JobID); err != nil {
		t.Fatal(err)
	}
	// Metrics 仍要覆蓋歷史 stable 標籤；正式 CreateDeployment 已不能直寫 stable。
	tx, err := st.DB().Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(`UPDATE deployments SET channel='stable' WHERE deployment_id=?`, d.DeploymentID); err == nil {
		_, err = tx.Exec(`UPDATE desired_state SET scope_id='stable' WHERE desired_id=?`, d.DesiredID)
	}
	if err == nil {
		err = tx.Commit()
	} else {
		_ = tx.Rollback()
	}
	if err != nil {
		t.Fatal(err)
	}
	finished := time.Now().UTC().Truncate(time.Second)
	if changed, err := st.SetDeploymentState(d.DeploymentID, store.DeploymentRunning, store.DeploymentFinished, finished); err != nil || !changed {
		t.Fatalf("finish changed=%v err=%v", changed, err)
	}
	fam := parseExposition(t, getMetrics(t, mux))
	last := fam["clawctl_deployment_last_finished_timestamp_seconds"]
	if last == nil || len(last.samples) != 1 || last.samples[0].labels["channel"] != "stable" || last.samples[0].value != float64(finished.Unix()) {
		t.Fatalf("last finished=%+v", last)
	}
	if stuck := fam["clawctl_deployment_stuck_machines"]; stuck != nil {
		for _, sample := range stuck.samples {
			if sample.labels["deployment_id"] == d.DeploymentID {
				t.Fatal("finished deployment 不該留 stuck gauge")
			}
		}
	}
}

// --- 三：一台機器的名字不准弄壞整頁。

// TestAQuoteInAMachineNameCannotDiscardTheWholePage
//
// ⚠⚠ 顯示名稱是人打進名冊的自由文字。裡面出現一個引號，沒跳脫的話就會生出
// 一行語法錯誤的 metrics，而 Prometheus 遇到解析錯誤是**整頁丟掉** ——
// 於是一台名字裡有引號的機器，會讓整個機隊的監控安靜地消失。
// 一台機器的名字不該有能力做到這件事。
func TestAQuoteInAMachineNameCannotDiscardTheWholePage(t *testing.T) {
	mux, st := metricsFixture(t)
	defer st.Close()

	nasty := `he said "hi"` + "\n" + `and\then`
	if _, err := st.CreateEnrollToken(nasty, time.Hour); err != nil {
		t.Fatalf("開票: %v", err)
	}
	// 一台正常的機器，用來證明「整頁還在」而不只是「壞的那一行不見了」。
	if _, err := st.CreateEnrollToken("innocent-bystander", time.Hour); err != nil {
		t.Fatalf("開票: %v", err)
	}

	fam := parseExposition(t, getMetrics(t, mux)) // 解析失敗會在這裡 Fatal

	got := labelValues(fam["clawctl_machine_state"], "machine")
	if !contains(got, "innocent-bystander") {
		t.Error("旁邊那台無辜的機器不見了 —— 一個名字弄掉了別人的監控")
	}
	// 跳脫之後解回來要**還原成原本的字串**，不是被消音成別的東西。
	// 名字被默默改掉的話，人去 Prometheus 上搜自己那台機器會搜不到。
	if !contains(got, nasty) {
		t.Errorf("名字裡有引號的那台解回來不是原本的字串。看到的是：%q", got)
	}
}

func TestQuoteEscapesWhatPrometheusRequires(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{`plain`, `"plain"`},
		{`say "hi"`, `"say \"hi\""`},
		{`back\slash`, `"back\\slash"`},
		{"two\nlines", `"two\nlines"`},
		// ⚠ 這一格是 quote 為什麼用 strings.NewReplacer 而不是兩次 ReplaceAll。
		//
		// 寫成 ReplaceAll(s, `"`, `\"`) 再 ReplaceAll(s, `\`, `\\`) 的話，
		// 第二次會把第一次**剛加上去的**那個反斜線也跳脫掉：
		// `a\"b` 會變成 `a\\\\"b` —— 引號前面沒有反斜線了，它逃出來了。
		// 順序反過來寫也只是換一個輸入中招。NewReplacer 是單次由左至右掃描、
		// 不回頭重掃已經替換過的內容，所以它對這個陷阱免疫，
		// 而且**pair 的順序不影響結果**（2026-09-03 實測）。
		{`\"`, `"\\\""`},
	} {
		if got := quote(c.in); got != c.want {
			t.Errorf("quote(%q) = %s，想要 %s", c.in, got, c.want)
		}
	}
}

// --- 四：這一頁不准宣稱健康。

// TestMetricsNeverClaimsTheHubIsUp
//
// ⚠⚠⚠ Hub 掛掉的時候這個端點不是回傳壞消息，它是**連線失敗** ——
// 而連線失敗跟「你的網路有問題」「Prometheus 自己掛了」長得一模一樣。
// 所以這一頁上不准有 clawctl_up 這種東西：一個永遠等於 1 的健康旗標
// 是這個專案最想避免的那種告警 —— 它只在你不需要它的時候說話。
// 「Hub 死了沒」由外部死人之鐘回答（ops/deadman.sh）。自證不算數。
func TestMetricsNeverClaimsTheHubIsUp(t *testing.T) {
	mux, st := metricsFixture(t)
	defer st.Close()

	fam := parseExposition(t, getMetrics(t, mux))
	for name := range fam {
		for _, bad := range []string{"_up", "_healthy", "_ok", "_alive"} {
			if strings.HasSuffix(name, bad) || name == "clawctl"+bad {
				t.Errorf("%s：這一頁不准宣稱健康。Hub 說自己還在不算數 —— "+
					"它活著才吐得出這一頁，所以這個值永遠是 1。", name)
			}
		}
	}

	wants := map[string]string{
		"clawctl_dead_signal_pipelines": "偵測到停止更新的證據管線數",
		"clawctl_double_agent_machines": "偵測到 agent 啟動身分重疊的機器數",
	}
	for name, want := range wants {
		if h := fam[name].help; h != want {
			t.Errorf("%s HELP=%q want %q", name, h, want)
		}
	}
}

// --- 五：讀不到就要大聲壞掉。

// TestAFailedQueryIsA500NotAQuietlyShorterPage
//
// ⚠ 一頁「格式正確、數字很少」的 metrics 會被 Prometheus 當成抓成功，
// 於是「機隊有幾台」那條線會安靜地掉到 0 —— 而 0 台看起來就像機隊全滅，
// 或者更糟，看起來像一切正常沒東西要管。500 才是大聲的：
// Prometheus 自己的 up{} 會掉到 0，那是有規則在守的東西。
//
// ⚠⚠ **這支測試只走得到 Overview 那一條錯誤路徑。** DeadSignals 與
// DoubleAgents 的 500 沒有被釘住 —— 要讓它們失敗而 Overview 還活著，
// 需要從第二條連線去動 schema，而它們用的欄位跟 Overview 是同一批。
// 那兩段是照 review 改的，不是照測試改的。這裡寫下來，是因為
// 「沒有被測到」這件事本身要看得見（docs/PHASE1.md §5.19）。
func TestAFailedQueryIsA500NotAQuietlyShorterPage(t *testing.T) {
	mux, st := metricsFixture(t)
	st.Close() // ⚠ 故意的：之後每一個查詢都會失敗。

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))

	if rec.Code == http.StatusOK {
		t.Fatalf("資料庫掛了還回 200，內容是：\n%s", rec.Body.String())
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("想要 500，拿到 %d", rec.Code)
	}
	// ⚠ 身體裡不准有一頁「看起來像成功」的 metrics。
	if strings.Contains(rec.Body.String(), "clawctl_machines_total") {
		t.Error("500 的內文裡還是吐了機隊指標 —— 那會被當成抓到了一頁很空的機隊")
	}
}

// --- 六：真的有機器在跑的時候，數字要對得上。

// TestTheNumbersMatchTheRoster
//
// ⚠ 這一條防的不是「算錯」，是「兩邊各算各的」。分母的規則在這個專案裡
// 被寫過四次（Overview 在 Go 裡、週報在 SQL 裡、死人之鐘又一句 SQL、
// tailnet 對照第四個），而 /metrics 是第五個。所以這裡不重算一次，
// 是拿同一份 Overview 去比對 —— 兩個數字不一樣的時候，
// 看圖的人跟看畫面的人會吵一整天而且兩邊都覺得自己對。
func TestTheNumbersMatchTheRoster(t *testing.T) {
	mux, st := metricsFixture(t)
	defer st.Close()

	for _, n := range []string{"alpha", "bravo", "charlie"} {
		if _, err := st.CreateEnrollToken(n, time.Hour); err != nil {
			t.Fatalf("開票 %s: %v", n, err)
		}
	}
	ov, err := st.Overview(time.Now().UTC())
	if err != nil {
		t.Fatalf("overview: %v", err)
	}

	fam := parseExposition(t, getMetrics(t, mux))
	if got := single(t, fam, "clawctl_machines_total"); got != float64(ov.Total) {
		t.Errorf("clawctl_machines_total = %v，Overview 說 %d", got, ov.Total)
	}
	// 每台一行，一行都不能少。
	if got := len(fam["clawctl_machine_state"].samples); got != len(ov.Machines) {
		t.Errorf("clawctl_machine_state 有 %d 行，名冊上有 %d 台", got, len(ov.Machines))
	}
}

// TestCronJobMetricsCarryOnlyMeasuredFacts 守的是「已量到的排程分母沒有被吐給 Prometheus」這個錯。
// 這裡同時釘住啟用、停用、逾期與最後跑完時間的數字。
func TestCronJobMetricsCarryOnlyMeasuredFacts(t *testing.T) {
	mux, st := metricsFixture(t)
	defer st.Close()
	machines, err := st.ListMachines()
	if err != nil || len(machines) != 1 {
		t.Fatalf("取測試機器失敗：筆數 %d，錯誤 %v", len(machines), err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	lastRun := now.Add(-3 * time.Hour)
	b := model.ObservationBatch{
		SchemaVersion: model.SchemaVersion,
		MeasuredAt:    now,
		OpenClaw: model.OpenClaw{Present: true, DB: &model.OpenClawDB{
			Present:                  true,
			CronJobsTotal:            5,
			CronJobsTotalMeasured:    true,
			CronJobsEnabled:          2,
			CronJobsEnabledMeasured:  true,
			CronJobsOverdue:          1,
			CronJobsScheduleMeasured: true,
			LastCronRunAt:            &lastRun,
		}},
	}
	if err := st.RecordObservation(machines[0].MachineID, b, now); err != nil {
		t.Fatalf("寫入排程觀測失敗：%v", err)
	}

	fam := parseExposition(t, getMetrics(t, mux))
	jobs := fam["clawctl_openclaw_cron_jobs"]
	if jobs == nil || len(jobs.samples) != 2 {
		lines := 0
		if jobs != nil {
			lines = len(jobs.samples)
		}
		t.Fatalf("排程工作指標應有啟用與停用兩行，實際是 %d 行", lines)
	}
	got := map[string]float64{}
	for _, sample := range jobs.samples {
		got[sample.labels["state"]] = sample.value
		if sample.labels["machine"] == "" || sample.labels["machine_id"] == "" {
			t.Errorf("排程工作指標少了機器識別標籤：%v", sample.labels)
		}
	}
	if got["enabled"] != 2 || got["disabled"] != 3 {
		t.Errorf("排程工作數字 = %v，預期啟用 2、停用 3", got)
	}
	if value := single(t, fam, "clawctl_openclaw_cron_jobs_overdue"); value != 1 {
		t.Errorf("逾期排程工作 = %v，預期 1", value)
	}
	if value := single(t, fam, "clawctl_openclaw_last_cron_run_timestamp_seconds"); value != float64(lastRun.Unix()) {
		t.Errorf("最後跑完時間 = %.0f，預期 %d", value, lastRun.Unix())
	}
}

// TestCronJobMetricsOmitUnmeasuredMachinesBesideMeasuredOnes 守的是同一輪 scrape
// 裡，有量到與沒量到的機器不能被混成同一種答案。
//
// ⚠⚠ 內層閘只在混合機隊才會負載：全沒量到會被外層閘整塊省略，全量到則
// 內層條件永遠為真；外層閘已有 TestCronJobMetricsDoNotTurnUnknownIntoZero 守住。
// 吐 0 會讓一台我們讀不到的機器，長得跟真的宣告 0 個工作的機器一模一樣。
func TestCronJobMetricsOmitUnmeasuredMachinesBesideMeasuredOnes(t *testing.T) {
	tests := []struct {
		name string
		db   model.OpenClawDB
	}{
		{
			name: "jobs 與 schedule 都沒量到",
			db: model.OpenClawDB{
				Present:                  true,
				CronJobsTotalMeasured:    false,
				CronJobsEnabledMeasured:  false,
				CronJobsScheduleMeasured: false,
				CronJobsTotal:            0,
				CronJobsEnabled:          0,
				CronJobsOverdue:          0,
			},
		},
		{
			name: "jobs 量到但 schedule 沒量到",
			db: model.OpenClawDB{
				Present:                  true,
				CronJobsTotalMeasured:    true,
				CronJobsEnabledMeasured:  true,
				CronJobsScheduleMeasured: false,
				CronJobsTotal:            3,
				CronJobsEnabled:          2,
				CronJobsOverdue:          0,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mux, st := metricsFixture(t)
			defer st.Close()
			machines, err := st.ListMachines()
			if err != nil || len(machines) != 1 {
				t.Fatalf("取測試機器失敗：筆數 %d，錯誤 %v", len(machines), err)
			}
			const measured = "fixture-machine"
			const partlyMeasured = "cron-partly-measured"
			now := time.Now().UTC().Truncate(time.Second)
			if err := st.UpsertMachine(store.Machine{
				MachineID: partlyMeasured, DisplayName: partlyMeasured, Expected: true, CreatedAt: now,
			}); err != nil {
				t.Fatalf("建立第二台測試機器：%v", err)
			}

			measuredBatch := model.ObservationBatch{
				SchemaVersion: model.SchemaVersion,
				MeasuredAt:    now,
				OpenClaw: model.OpenClaw{Present: true, DB: &model.OpenClawDB{
					Present:                  true,
					CronJobsTotalMeasured:    true,
					CronJobsEnabledMeasured:  true,
					CronJobsScheduleMeasured: true,
					CronJobsTotal:            0,
					CronJobsEnabled:          0,
					CronJobsOverdue:          0,
				}},
			}
			if err := st.RecordObservation(machines[0].MachineID, measuredBatch, now); err != nil {
				t.Fatalf("寫入第一台排程觀測：%v", err)
			}
			partlyMeasuredBatch := model.ObservationBatch{
				SchemaVersion: model.SchemaVersion,
				MeasuredAt:    now,
				OpenClaw:      model.OpenClaw{Present: true, DB: &tt.db},
			}
			if err := st.RecordObservation(partlyMeasured, partlyMeasuredBatch, now); err != nil {
				t.Fatalf("寫入第二台排程觀測：%v", err)
			}

			fam := parseExposition(t, getMetrics(t, mux))
			jobsByMachine := map[string]map[string][]float64{}
			for _, s := range fam["clawctl_openclaw_cron_jobs"].samples {
				machine := s.labels["machine"]
				if jobsByMachine[machine] == nil {
					jobsByMachine[machine] = map[string][]float64{}
				}
				state := s.labels["state"]
				jobsByMachine[machine][state] = append(jobsByMachine[machine][state], s.value)
			}
			if got := jobsByMachine[measured]; len(got["enabled"]) != 1 || got["enabled"][0] != 0 ||
				len(got["disabled"]) != 1 || got["disabled"][0] != 0 {
				t.Errorf("第一台的排程工作 samples：%v", got)
			}
			if tt.db.CronJobsTotalMeasured {
				if got := jobsByMachine[partlyMeasured]; len(got["enabled"]) != 1 || got["enabled"][0] != 2 ||
					len(got["disabled"]) != 1 || got["disabled"][0] != 1 {
					t.Errorf("第二台的排程工作 samples：%v", got)
				}
			} else if got := jobsByMachine[partlyMeasured]; len(got) != 0 {
				t.Errorf("第二台的排程工作 samples：%v", got)
			}

			overdueByMachine := map[string][]float64{}
			for _, s := range fam["clawctl_openclaw_cron_jobs_overdue"].samples {
				machine := s.labels["machine"]
				overdueByMachine[machine] = append(overdueByMachine[machine], s.value)
			}
			if got := overdueByMachine[measured]; len(got) != 1 || got[0] != 0 {
				t.Errorf("第一台的逾期排程 samples：%v", got)
			}
			if got := overdueByMachine[partlyMeasured]; len(got) != 0 {
				t.Errorf("第二台的逾期排程 samples：%v", got)
			}
		})
	}
}

// TestCronJobMetricsDoNotTurnUnknownIntoZero 守的是「沒有 OpenClawDB 被吐成 0 個工作」這個錯。
// 不知道時三種指標都必須完全不出現，不能留一個看起來像答案的 0。
func TestCronJobMetricsDoNotTurnUnknownIntoZero(t *testing.T) {
	mux, st := metricsFixture(t)
	defer st.Close()

	body := getMetrics(t, mux)
	for _, name := range []string{
		"clawctl_openclaw_cron_jobs",
		"clawctl_openclaw_cron_jobs_overdue",
		"clawctl_openclaw_last_cron_run_timestamp_seconds",
	} {
		if strings.Contains(body, name) {
			t.Errorf("沒有 OpenClawDB 卻出現 %s：\n%s", name, body)
		}
	}
}

// TestOpenClawInstallMetricsCarryOnlyMeasuredFacts 釘住三條 install 指標的值與 labels。
func TestOpenClawInstallMetricsCarryOnlyMeasuredFacts(t *testing.T) {
	mux, st := metricsFixture(t)
	defer st.Close()
	machines, err := st.ListMachines()
	if err != nil || len(machines) != 1 {
		t.Fatalf("取測試機器失敗：筆數 %d，錯誤 %v", len(machines), err)
	}
	writable := false
	now := time.Now().UTC()
	b := model.ObservationBatch{
		SchemaVersion: model.SchemaVersion,
		MeasuredAt:    now,
		OpenClaw: model.OpenClaw{Present: true, Install: &model.OpenClawInstall{
			RunningDirWritable: &writable,
			NodePath:           "/home/example-user-c/.local/node24/bin/node",
			NodeVersion:        "v24.15.0",
			RunningDir:         "/home/example-user-c/.local/node24/lib/node_modules/openclaw",
			RunningDirVersion:  "2026.6.10",
		}},
	}
	if err := st.RecordObservation(machines[0].MachineID, b, now); err != nil {
		t.Fatalf("寫入安裝觀測失敗：%v", err)
	}

	fam := parseExposition(t, getMetrics(t, mux))
	w := fam["clawctl_openclaw_install_writable"]
	if w == nil || len(w.samples) != 1 || w.samples[0].value != 0 {
		t.Fatalf("writable 指標 = %+v，預期一行 0", w)
	}
	node := fam["clawctl_openclaw_node_info"]
	if node == nil || len(node.samples) != 1 || node.samples[0].value != 1 {
		t.Fatalf("node info 指標 = %+v", node)
	}
	if got := node.samples[0].labels; got["version"] != "v24.15.0" || got["path"] != "/home/example-user-c/.local/node24/bin/node" || got["machine_id"] == "" || got["machine"] == "" {
		t.Errorf("node info labels = %v", got)
	}
	running := fam["clawctl_openclaw_running_version_info"]
	if running == nil || len(running.samples) != 1 || running.samples[0].value != 1 {
		t.Fatalf("running version 指標 = %+v", running)
	}
	if got := running.samples[0].labels; got["version"] != "2026.6.10" || got["dir"] != "/home/example-user-c/.local/node24/lib/node_modules/openclaw" {
		t.Errorf("running version labels = %v", got)
	}
}

// TestOpenClawInstallMetricsDoNotTurnUnknownIntoZero 守的是舊 agent 沒送 Install
// 時整條 series 都不存在，不能用 0 假裝量過。
func TestOpenClawInstallMetricsDoNotTurnUnknownIntoZero(t *testing.T) {
	mux, st := metricsFixture(t)
	defer st.Close()
	body := getMetrics(t, mux)
	for _, name := range []string{
		"clawctl_openclaw_install_writable",
		"clawctl_openclaw_node_info",
		"clawctl_openclaw_running_version_info",
	} {
		if strings.Contains(body, name) {
			t.Errorf("沒有 Install 卻出現 %s：\n%s", name, body)
		}
	}
}

// --- fixture 與一個嚴格的 parser。

// metricsFixture 給一個 Hub，名冊上**已經有一台真的報到過的機器**。
//
// ⚠ 那台機器不是裝飾品，而且「報到過」也不是順手做的：
// 空名冊的時候「每台一行」的那三個指標一行都沒有，於是「sample 掉了」跟
// 「本來就沒有機器」在輸出上長得一樣，上面那條「宣告了 TYPE 就要有 sample」
// 的斷言會變成永遠問不出問題。
//
// ⚠⚠ 而且只開票不報到還不夠 —— last_checkin_age 只在真的收過心跳時才有那一行
// （這是刻意的，見 TestAMachineThatNeverReported...）。所以這裡走**真的 HTTP**
// 報到再送一顆心跳，讓那三個指標全部都有東西可以掉。
// 第一版這裡只開票，於是 age 那一行從頭到尾沒被任何測試看過。
// 空名冊自己有一支測試（TestAFreshHubWithNoMachinesStillServesAValidPage）。
func metricsFixture(t *testing.T) (*http.ServeMux, *store.Store) {
	t.Helper()
	mux, st := emptyMetricsFixture(t)
	m := enrollViaHTTP(t, mux, st, "fixture-machine")
	postCheckin(t, mux, m.token, http.StatusOK)
	return mux, st
}

func emptyMetricsFixture(t *testing.T) (*http.ServeMux, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	// 正式 Hub 在接第一顆 check-in 前一定先發布 process 真正載入的
	// expectations identity；fixture 也要保留同一條不變量。
	st.SetExpectations(nil)
	if err := st.PublishExpectationsPolicy(time.Now().UTC()); err != nil {
		t.Fatalf("publish workload policy: %v", err)
	}
	mux := http.NewServeMux()
	h := &hub{store: st}
	h.machineAndPublicRoutes(mux)
	mux.HandleFunc("GET /metrics", h.handleMetrics)
	return mux, st
}

// TestAFreshHubWithNoMachinesStillServesAValidPage
//
// ⚠ 剛裝好、名冊還是空的 Hub 是一個真的狀態，不是邊界情況 ——
// 而它剛好是「每台一行」的指標一行都沒有的那個狀態。
// 一頁在裝好第一天就解析失敗的 metrics，會讓人以為是自己的 Prometheus 設錯了。
//
// ⚠⚠ 這裡刻意**不**套用「宣告了 TYPE 就要有 sample」那條規則：
// 沒有機器的時候，沒有 per-machine 的 sample 是對的。
func TestAFreshHubWithNoMachinesStillServesAValidPage(t *testing.T) {
	mux, st := emptyMetricsFixture(t)
	defer st.Close()

	fam := parseExposition(t, getMetrics(t, mux)) // 解析失敗會 Fatal

	// 機隊層級的數字照樣要在，而且是 0 —— 不是「那一行不見了」。
	if got := single(t, fam, "clawctl_machines_total"); got != 0 {
		t.Errorf("空名冊的 clawctl_machines_total = %v，想要 0", got)
	}
	// 狀態還是要窮舉，全部 0。
	if got := len(fam["clawctl_machines"].samples); got != len(state.AllStates) {
		t.Errorf("空名冊時 clawctl_machines 有 %d 行，想要 %d 行（全部是 0）",
			got, len(state.AllStates))
	}
}

func getMetrics(t *testing.T, mux *http.ServeMux) string {
	t.Helper()
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /metrics = %d：%s", rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

type sample struct {
	name   string
	labels map[string]string
	value  float64
}

// key 是 Prometheus 眼中的「同一條線」：名字加上排序過的 labels。
func (s sample) key() string {
	keys := make([]string, 0, len(s.labels))
	for k := range s.labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(s.name)
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "%s=%q", k, s.labels[k])
	}
	b.WriteByte('}')
	return b.String()
}

type family struct {
	help, typ string
	samples   []sample
}

// parseExposition 是一個**故意很嚴格**的 exposition format parser。
//
// ⚠ 它不是為了跑得快或吃得下所有輸入，剛好相反：任何它不完全確定的東西
// 都要 Fatal。一個寬鬆的 parser 會把「我寫壞了」讀成「大概是這個意思吧」，
// 然後測試變綠而真的 Prometheus 把整頁丟掉。
//
// ⚠⚠ 它跟 metrics.go 是同一個人寫的，所以它證明不了格式對 —— 只證明
// 這兩份對格式的理解一致。真正的外部驗證見 ops/check-metrics.sh。
func parseExposition(t *testing.T, body string) map[string]*family {
	t.Helper()
	if body == "" {
		t.Fatal("/metrics 是空的")
	}
	if !strings.HasSuffix(body, "\n") {
		t.Error("最後一行沒有換行 —— exposition format 要求每一行都以 \\n 結尾")
	}

	out := map[string]*family{}
	get := func(name string) *family {
		if f, ok := out[name]; ok {
			return f
		}
		f := &family{}
		out[name] = f
		return f
	}

	// --- 分塊檢查。
	//
	// ⚠⚠ 這一段是後來補的，補的原因是它漏掉的那個 bug 被外面的人抓到了。
	//
	// 原本這個 parser 只照名字分桶，於是「HELP A / TYPE A / HELP B / TYPE B /
	// A 的 sample / B 的 sample」在它眼裡跟正確的排法一模一樣 —— 分桶的結果
	// 完全相同。但 exposition format 要求一個指標的所有行是**一整塊**
	// （"all lines for a given metric must be provided as one single group"），
	// 上面那種排法讓 B 的宣告落在 A 的 group 裡面。
	//
	// ops/check-metrics.sh 拿官方 parser 讀真的端點時抓到了：11 個指標被讀成
	// 25 個 family，而且**回的是 200**。一個「解析得動、但被拆成別的東西」
	// 的頁面，正是這個專案最怕的那種答案 —— 沒有壞消息，也沒有真的問過。
	//
	// 規則：每一行都屬於某個指標，同一個指標的行必須連續。
	lastFam := ""
	closed := map[string]bool{}
	touch := func(name string, lineNo int) {
		if name == lastFam {
			return
		}
		if closed[name] {
			t.Errorf("第 %d 行：%s 的行不連續 —— 中間插了別的指標。\n"+
				"⚠ exposition format 要求一個指標的 HELP／TYPE／sample 是一整塊。",
				lineNo, name)
		}
		if lastFam != "" {
			closed[lastFam] = true
		}
		lastFam = name
	}

	for i, ln := range strings.Split(body, "\n") {
		lineNo := i + 1
		if strings.TrimSpace(ln) == "" {
			continue
		}
		if strings.HasPrefix(ln, "#") {
			fields := strings.SplitN(ln, " ", 4)
			if len(fields) < 3 {
				continue // 一般註解
			}
			switch fields[1] {
			case "HELP":
				if len(fields) == 4 {
					touch(fields[2], lineNo)
					get(fields[2]).help = fields[3]
				}
			case "TYPE":
				touch(fields[2], lineNo)
				if len(fields) < 4 {
					t.Errorf("第 %d 行：# TYPE 沒有講型別：%s", lineNo, ln)
					continue
				}
				f := get(fields[2])
				// ⚠ 同一個指標宣告兩次 TYPE 會讓 Prometheus 整頁丟掉。
				// 這在「HELP/TYPE 寫在迴圈裡」的時候一寫就中。
				if f.typ != "" {
					t.Errorf("第 %d 行：%s 宣告了兩次 # TYPE", lineNo, fields[2])
				}
				f.typ = strings.TrimSpace(fields[3])
			}
			continue
		}

		s, err := parseSample(ln)
		if err != nil {
			t.Fatalf("第 %d 行解析失敗：%v\n  %s\n"+
				"⚠ Prometheus 遇到這個是整頁丟掉，不是丟掉這一行。", lineNo, err, ln)
		}
		famName := expositionFamily(out, s.name)
		touch(famName, lineNo)
		f, ok := out[famName]
		if !ok {
			t.Errorf("第 %d 行：%s 有 sample 卻沒有先宣告 # TYPE", lineNo, s.name)
			f = get(famName)
		}
		f.samples = append(f.samples, s)
	}
	return out
}

// expositionFamily keeps histogram _bucket, _sum and _count samples inside
// the family that declared TYPE histogram. The sample name itself stays
// intact so le labels remain part of the series identity.
func expositionFamily(out map[string]*family, sampleName string) string {
	if _, ok := out[sampleName]; ok {
		return sampleName
	}
	for _, suffix := range []string{"_bucket", "_sum", "_count"} {
		base, ok := strings.CutSuffix(sampleName, suffix)
		if !ok {
			continue
		}
		if f, exists := out[base]; exists && f.typ == "histogram" {
			return base
		}
	}
	return sampleName
}

func parseSample(ln string) (sample, error) {
	s := sample{labels: map[string]string{}}
	i := 0
	for i < len(ln) && isNameByte(ln[i], i == 0) {
		i++
	}
	if i == 0 {
		return s, fmt.Errorf("指標名稱是空的或第一個字元不合法")
	}
	s.name = ln[:i]

	if i < len(ln) && ln[i] == '{' {
		i++
		for {
			for i < len(ln) && ln[i] == ' ' {
				i++
			}
			if i < len(ln) && ln[i] == '}' {
				i++
				break
			}
			start := i
			for i < len(ln) && isNameByte(ln[i], i == start) {
				i++
			}
			if i == start {
				return s, fmt.Errorf("在位置 %d 期待 label 名稱", i)
			}
			key := ln[start:i]
			if i >= len(ln) || ln[i] != '=' {
				return s, fmt.Errorf("label %s 後面沒有 =", key)
			}
			i++
			if i >= len(ln) || ln[i] != '"' {
				return s, fmt.Errorf("label %s 的值沒有用引號括起來", key)
			}
			i++
			var v strings.Builder
			closed := false
			for i < len(ln) {
				c := ln[i]
				if c == '\\' {
					if i+1 >= len(ln) {
						return s, fmt.Errorf("label %s 的值以一個孤兒反斜線結尾", key)
					}
					switch ln[i+1] {
					case '\\':
						v.WriteByte('\\')
					case '"':
						v.WriteByte('"')
					case 'n':
						v.WriteByte('\n')
					default:
						return s, fmt.Errorf("label %s 的值裡有未知的跳脫 \\%c", key, ln[i+1])
					}
					i += 2
					continue
				}
				if c == '"' {
					i++
					closed = true
					break
				}
				v.WriteByte(c)
				i++
			}
			if !closed {
				return s, fmt.Errorf("label %s 的值沒有結束的引號", key)
			}
			if _, dup := s.labels[key]; dup {
				return s, fmt.Errorf("label %s 在同一行出現兩次", key)
			}
			s.labels[key] = v.String()
			if i < len(ln) && ln[i] == ',' {
				i++
				continue
			}
			if i < len(ln) && ln[i] == '}' {
				i++
				break
			}
			return s, fmt.Errorf("label %s 之後既不是 , 也不是 }", key)
		}
	}

	rest := strings.TrimSpace(ln[i:])
	if rest == "" {
		return s, fmt.Errorf("沒有值")
	}
	numField := rest
	if sp := strings.IndexByte(rest, ' '); sp >= 0 {
		numField = rest[:sp] // 後面是 timestamp
	}
	v, err := strconv.ParseFloat(numField, 64)
	if err != nil {
		return s, fmt.Errorf("值 %q 不是數字：%v", numField, err)
	}
	s.value = v
	return s, nil
}

func isNameByte(c byte, first bool) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c == '_', c == ':':
		return true
	case c >= '0' && c <= '9':
		return !first
	}
	return false
}

func labelValues(f *family, label string) []string {
	if f == nil {
		return nil
	}
	var out []string
	for _, s := range f.samples {
		out = append(out, s.labels[label])
	}
	sort.Strings(out)
	return out
}

func contains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}

func single(t *testing.T, fam map[string]*family, name string) float64 {
	t.Helper()
	f := fam[name]
	if f == nil || len(f.samples) != 1 {
		t.Fatalf("%s 應該剛好一行，實際上有 %d 行", name, len(fam[name].samples))
	}
	return f.samples[0].value
}

// TestAgentVersionIsAMetricSoTheHubCanBeAskedInsteadOfThePusher
//
// ⚠⚠ 這條指標存在的理由跟其他的不一樣：它不是為了畫圖，是為了**驗收**。
//
// ops/ansible/agent.yml 推完 binary 之後 Ansible 會回報 changed —— 那是推的人
// 自己說的。2026-09-03 的雙 agent 事故就是這個形狀：部署腳本說成功、
// 兩個 agent 都誠實地送心跳、Hub 上每一盞燈都是綠的。
// 這條讓「機器現在跑的是哪一版」變成 Hub 這個第三方回答得出來的問題。
func TestAgentVersionIsAMetricSoTheHubCanBeAskedInsteadOfThePusher(t *testing.T) {
	mux, st := metricsFixture(t)
	defer st.Close()

	fam := parseExposition(t, getMetrics(t, mux))
	f, ok := fam["clawctl_agent_info"]
	if !ok {
		t.Fatal("有報到過的機器沒有 clawctl_agent_info —— " +
			"那 ops/ansible/verify.sh 就只能相信 Ansible 說它 changed 了")
	}
	if got := labelValues(f, "version"); !contains(got, "test") {
		t.Fatalf("版本沒有出現在 version label 上，看到的是：%v", got)
	}
	// 值恆為 1（info-metric），跟 clawctl_expectation_info 同一個形狀。
	for _, s := range f.samples {
		if s.value != 1 {
			t.Errorf("info-metric 的值應該恆為 1，看到 %v", s.value)
		}
	}
}

// TestAMachineWithNoVersionHasNoLineRatherThanAnEmptyOne
//
// ⚠⚠ 這是這條指標最重要的性質，而且它是「少一行」而不是「多一個空值」。
//
// 沒報到過的機器、以及舊版 agent（不送 agent_version 那欄）落地都是空字串。
// 如果照吐成 version=""，Grafana 上會出現一列有機器名、版本欄是空白的 ——
// 那長得像一個答案。而它的意思是「不知道」。
//
// 兩者的差別在驗收的時候會咬人：verify.sh 問的是「這台是不是 X 版」，
// 一個 version="" 的 series 會誠實地回答「不是」，於是一台**根本沒回報過**
// 的機器會被讀成「推失敗了」—— 那是兩個要用不同方法處理的問題。
// 沒有那一行，查詢會少一個 series，`unless` 就問得出「誰連版本都沒有」。
func TestAMachineWithNoVersionHasNoLineRatherThanAnEmptyOne(t *testing.T) {
	mux, st := metricsFixture(t)
	defer st.Close()

	// 只開票、不報到 —— 名冊上有，但沒有任何版本可言。
	if _, err := st.CreateEnrollToken("never-showed-up", time.Hour); err != nil {
		t.Fatalf("開票: %v", err)
	}

	fam := parseExposition(t, getMetrics(t, mux))
	for _, s := range fam["clawctl_agent_info"].samples {
		if s.labels["machine"] == "never-showed-up" {
			t.Errorf("從沒回報過的機器有 clawctl_agent_info（version=%q）—— "+
				"那個版本是編的", s.labels["version"])
		}
		if strings.TrimSpace(s.labels["version"]) == "" {
			t.Errorf("%s 的 version label 是空的 —— "+
				"空字串在圖上長得像答案，實際是「不知道」", s.labels["machine"])
		}
	}

	// ⚠ 而它仍然要在 clawctl_machine_state 上（那條是名冊，這條是版本）。
	// 少的是版本，不是機器 —— 否則 verify.sh 會連「誰沒回答」都問不出來。
	if !contains(labelValues(fam["clawctl_machine_state"], "machine"), "never-showed-up") {
		t.Error("機器從名冊指標上消失了 —— 少的應該只有版本那一行")
	}
}

// TestAFreshHubEmitsNoAgentInfoTypeAtAll
//
// ⚠ 一個機器都還沒報到的 Hub 不可以印出「有 # TYPE、零筆 sample」的
// clawctl_agent_info —— 對 Prometheus 來說那個指標等於不存在，
// 但對讀 /metrics 原始頁面的人來說它長得像存在。那個落差比沒有更難查。
func TestAFreshHubEmitsNoAgentInfoTypeAtAll(t *testing.T) {
	mux, st := emptyMetricsFixture(t)
	defer st.Close()

	if body := getMetrics(t, mux); strings.Contains(body, "clawctl_agent_info") {
		t.Error("名冊還是空的，卻宣告了 clawctl_agent_info —— " +
			"有 TYPE 沒有 sample，讀原始頁面的人會以為它在，Prometheus 那邊卻沒有")
	}
}
