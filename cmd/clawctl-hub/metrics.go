package main

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/teddashh/AI-Intune/internal/deploy"
	"github.com/teddashh/AI-Intune/internal/model"
	"github.com/teddashh/AI-Intune/internal/state"
	"github.com/teddashh/AI-Intune/internal/store"
)

// Prometheus 抓得走的那一頁。
//
// 為什麼是這一個、而且只有這一個外部整合：研究階段三份提案一致把
// osquery / Fleet / OTel / Loki / Grafana 全部判成「第一年不裝，只留介面」，
// 理由是它們看得懂 OS、看不懂 OAuth 過期與最後成功時間 —— 那剛好是這個
// 產品存在的理由（docs/SPEC.md、docs/PHASES.md、三份 proposal）。
// `/metrics` 是那份清單裡唯一一個**接了也不產生耦合**的：Prometheus 來抓，
// Hub 完全不需要知道它存在，不裝也不會少一塊。
//
// ⚠⚠ 刻意不引入 prometheus/client_golang。
// 這個檔案手寫 exposition format，理由跟「零 JavaScript build step」同一條：
// 複雜度的上限是「半夜一個人修得動」。為了十幾個 gauge 拉進一棵相依樹，
// 會讓 `CGO_ENABLED=0` 的靜態 binary 多背一堆它用不到的東西。
// 格式本身很穩定。
//
// ⚠ 守它的有兩層，而且第二層才算數：metrics_test.go 的
// TestMetricsAreParseableExposition 用一個嚴格的 parser 讀這一頁，但那個
// parser 跟這個檔案是同一個人寫的 —— 它證明得了兩邊一致，證明不了兩邊對。
// **自證不算數**：真的驗證是 ops/check-metrics.sh，它拿 Prometheus 官方的
// text parser 去讀真的跑著的 Hub（而且會先餵它一頁壞的，確認它真的會叫）。
//
// ⚠⚠⚠ **這一頁答不出「Hub 死了沒」。**
// Hub 掛掉的時候，這個端點不是回傳壞消息，它是**連線失敗** ——
// 而連線失敗跟「你的網路有問題」「Prometheus 自己掛了」長得一模一樣。
// 「Hub 死了」這個問題由外部死人之鐘回答（早報有沒有準時送到，
// ops/deadman.sh）。同 handleHealthz 的註解：Hub 說自己還在不算數。
//
// ⚠ 這裡只吐**事實**（數量、年齡、狀態），不吐「健康」。
// 沒有 clawctl_up、沒有 clawctl_healthy。要不要為某個年齡亮燈是
// 告警規則的事 —— 判斷寫在告警那一側，才有人會去改它。

func (h *hub) handleMetrics(w http.ResponseWriter, r *http.Request) {
	now := time.Now().UTC()
	var b strings.Builder

	gauge := func(name, help string) { fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s gauge\n", name, help, name) }
	line := func(name, labels string, v any) {
		if labels != "" {
			fmt.Fprintf(&b, "%s{%s} %v\n", name, labels, v)
			return
		}
		fmt.Fprintf(&b, "%s %v\n", name, v)
	}

	gauge("clawctl_build_info", "Hub 這個 binary 的版本；值恆為 1，版本在 label 上")
	line("clawctl_build_info", `version=`+quote(version), 1)
	// ⚠ 這不是「Hub 活著」。它只說這個 process 是幾點起來的 ——
	// changes() 一跳就是重啟過，而重啟前後那幾分鐘的「機器失聯」要打折看。
	// 零值（測試裡直接 new 的 hub）不吐，不要吐一個 1970 年。
	if !h.startedAt.IsZero() {
		gauge("clawctl_hub_started_timestamp_seconds", "Hub process 啟動時刻（unix 秒）")
		line("clawctl_hub_started_timestamp_seconds", "", h.startedAt.Unix())
	}

	// 還原演練：只吐「上一次是幾點」。從來沒做過就沒有這一行 ——
	// 規則那側用 absent() 接，因為「沒有這條線」正是要亮燈的狀況。
	if at, ok := readDrillStamp(h.drillStamp); ok {
		gauge("clawctl_restore_drill_timestamp_seconds", "上一次還原演練通過的時刻")
		line("clawctl_restore_drill_timestamp_seconds", "", at.Unix())
	}

	ov, err := h.store.Overview(now)
	if err != nil {
		// ⚠ 讀不到就回 500，**不要**回一頁只有 build_info 的漂亮輸出。
		// 一頁「格式正確、數字很少」的 metrics 會被 Prometheus 當成抓成功，
		// 於是「機隊有幾台」那條線會安靜地掉到 0 —— 而 0 台看起來
		// 就像機隊全滅，或者更糟，看起來像一切正常沒東西要管。
		http.Error(w, "讀取機隊狀態失敗: "+err.Error(), http.StatusInternalServerError)
		return
	}

	gauge("clawctl_machines_total", "名冊上的機器數（不含已退役）")
	line("clawctl_machines_total", "", ov.Total)
	gauge("clawctl_machines_retired", "已退役的機器數（離開分母，但不離開畫面）")
	line("clawctl_machines_retired", "", len(ov.Retired))

	// ⚠ 每一個狀態都要出現，包含數量為 0 的。
	// 只吐有值的那些，會讓「degraded 這條線消失」跟「degraded 是 0」
	// 在圖上長得一樣 —— 而前者的意思是這個欄位壞了。
	gauge("clawctl_machines", "Hub 各狀態的機器數")
	for _, s := range state.AllStates {
		line("clawctl_machines", "state="+quote(string(s)), ov.Counts[s])
	}

	// ⚠⚠ 名冊是分母：每一台都要有一行，**包含從來沒回報過的那些**。
	// 如果只吐「有回報的」機器，一台從沒報到過的機器在 Prometheus 裡
	// 根本不存在 —— 而它正是最該被看到的那一台。
	// ⚠⚠⚠ 身分是 machine_id，**不是** display_name。
	//
	// 名冊沒有、也不該有「顯示名稱不重複」這條限制：兩台雲上的機器都叫
	// ubuntu 是完全正常的事。但在 Prometheus 裡，series 的身分就是
	// 「名字＋所有 label」—— 兩台同名的機器會吐出**一模一樣的兩行**，
	// 而重複的 series 會讓 Prometheus 把**整頁**判成解析錯誤丟掉。
	// 於是機隊裡有兩台同名機器，就會讓整個機隊的監控安靜地消失。
	// 這跟名字裡有引號是同一個形狀的 bug，而且發生機率高得多。
	//
	// display_name 還是留著（machine label），因為 Grafana 上看 ID 沒有人看得懂。
	// ⚠ 順帶修掉的第二件事：display_name 是可以改的，machine_id 不會變 ——
	// 改名不該讓一條線斷掉、旁邊長出一條新的。
	// ⚠⚠ 每一個指標是**一整塊**：HELP、TYPE、然後它自己的 sample，中間
	// 不准插進別的指標。exposition format 要求 "all lines for a given metric
	// must be provided as one single group, with the optional HELP and TYPE
	// lines first"。
	//
	// 這裡原本三個 gauge() 一起寫在前面、三組 sample 跟在後面 —— 讀起來
	// 整齊，但那讓第二、三個指標的宣告落在第一個指標的 group **裡面**。
	// metrics_test.go 那個 parser 沒抓到（它照名字分桶，看不到順序），
	// 是 ops/check-metrics.sh 用官方 parser 讀真的端點時抓到的：
	// 11 個指標被讀成 25 個 family。⚠ 而它回的是 200、不是錯誤 ——
	// 一個「解析得動、但被拆成別的東西」的頁面。自證不算數那條。
	rows := machineMetrics(ov, now)

	gauge("clawctl_machine_state", "每一台機器目前的狀態；值恆為 1，狀態在 label 上")
	for _, m := range rows {
		line("clawctl_machine_state", m.idLabels()+",state="+quote(m.st), 1)
	}

	gauge("clawctl_machine_last_checkin_age_seconds", "距離最後一次收到心跳幾秒（從沒回報過的機器不會有這一行，看 clawctl_machine_state）")
	for _, m := range rows {
		if m.hasAge {
			line("clawctl_machine_last_checkin_age_seconds", m.idLabels(), int64(m.age.Seconds()))
		}
	}

	gauge("clawctl_machine_clock_skew_seconds", "機器自報時間減 Hub 收到時間；正值＝機器的時鐘比較快（從沒回報過的機器不會有這一行，看 clawctl_machine_state）")
	for _, m := range rows {
		if m.hasSkew {
			line("clawctl_machine_clock_skew_seconds", m.idLabels(), int64(m.skew.Seconds()))
		}
	}

	jobCounts, err := h.store.JobCounts()
	if err != nil {
		http.Error(w, "讀取工作單數量失敗: "+err.Error(), http.StatusInternalServerError)
		return
	}
	globalJobs := map[deploy.JobState]int{}
	nonterminalJobs := map[string]int{}
	for _, count := range jobCounts {
		globalJobs[count.State] += count.Count
		if !deploy.IsTerminal(count.State) {
			nonterminalJobs[count.MachineID] += count.Count
		}
	}
	// ⚠ 九個狀態每一個都要出現。這裡的 0 是 jobs 的 COUNT 算出的真 0，
	// 不是查不到；少一條線會讓「沒有」跟「指標壞了」在圖上長得一樣。
	gauge("clawctl_jobs", "帳本裡各狀態的工作單數；九個狀態都會出現，0 是實際計數")
	for _, jobState := range allJobStates {
		line("clawctl_jobs", "state="+quote(string(jobState)), globalJobs[jobState])
	}

	if len(rows) > 0 {
		gauge("clawctl_jobs_nonterminal", "每台機器尚未進入終態的工作單數；名冊上的機器即使是 0 也會出現")
		for _, m := range rows {
			line("clawctl_jobs_nonterminal", m.idLabels(), nonterminalJobs[m.id])
		}
	}

	lastTerminal, err := h.store.LastTerminalPerMachine()
	if err != nil {
		http.Error(w, "讀取工作單終態時間失敗: "+err.Error(), http.StatusInternalServerError)
		return
	}
	machineLabels := make(map[string]string, len(rows))
	for _, m := range rows {
		machineLabels[m.id] = m.idLabels()
	}
	terminalSamples := 0
	for _, terminal := range lastTerminal {
		if _, ok := machineLabels[terminal.MachineID]; ok {
			terminalSamples++
		}
	}
	if terminalSamples > 0 {
		gauge("clawctl_job_last_terminal_timestamp_seconds", "每台機器、每個終態最近一張工作單進入終態的時刻（unix 秒）；沒有該終態就不出現")
		for _, terminal := range lastTerminal {
			labels, ok := machineLabels[terminal.MachineID]
			if !ok {
				continue
			}
			line("clawctl_job_last_terminal_timestamp_seconds",
				labels+",state="+quote(string(terminal.State)), terminal.TerminalAt.Unix())
		}
	}

	deployments, err := h.store.ListDeployments(now)
	if err != nil {
		http.Error(w, "讀取 deployment 指標失敗: "+err.Error(), http.StatusInternalServerError)
		return
	}
	deploymentCounts := map[string]int{}
	nonFinished := 0
	lastFinished := map[string]time.Time{}
	for _, d := range deployments {
		deploymentCounts[d.State]++
		if d.State != store.DeploymentFinished {
			nonFinished++
		}
		if d.FinishedAt != nil && d.FinishedAt.After(lastFinished[d.Channel]) {
			lastFinished[d.Channel] = *d.FinishedAt
		}
	}
	// ⚠ 三個 state 都吐；0 是 COUNT 的真 0，不是 absent。
	gauge("clawctl_deployments", "deployment 帳本各狀態數；running、paused、finished 即使是 0 也會出現")
	for _, deploymentState := range []string{store.DeploymentRunning, store.DeploymentPaused, store.DeploymentFinished} {
		line("clawctl_deployments", "state="+quote(deploymentState), deploymentCounts[deploymentState])
	}
	if nonFinished > 0 {
		gauge("clawctl_deployment_stuck_machines", "每張未 finished deployment 的 stuck 機器數；0 是依 SPEC 15 分鐘門檻算出的真 0")
		for _, d := range deployments {
			if d.State != store.DeploymentFinished {
				line("clawctl_deployment_stuck_machines",
					"deployment_id="+quote(d.DeploymentID)+",channel="+quote(d.Channel), d.Stuck)
			}
		}
	}
	if len(lastFinished) > 0 {
		gauge("clawctl_deployment_last_finished_timestamp_seconds", "各 channel 最近一張 deployment finished 的 unix 時間；沒有就不出現")
		for _, channel := range []string{"canary", "stable"} {
			if at, ok := lastFinished[channel]; ok {
				line("clawctl_deployment_last_finished_timestamp_seconds", "channel="+quote(channel), at.Unix())
			}
		}
	}

	// agent 版本。info-metric：值恆為 1，版本在 label 上（跟 expectation_info 同一個形狀）。
	//
	// ⚠⚠ 這條存在的唯一理由是讓「推出去的東西有沒有真的落地」可以被第三方回答。
	// ops/ansible/agent.yml 推完之後 Ansible 會說 changed —— 那是**推的人自己說的**。
	// 這條是機器在下一次心跳裡自己說它現在是哪一版，而且要等 Hub 收到、
	// Prometheus 抓到才看得見。ops/ansible/verify.sh 驗的是這條，不是 Ansible 的回報。
	//
	// ⚠ 沒報到過、或舊版 agent 不送這欄的機器**整行不出現**，
	// 不會出現 version=""。一個空字串的 label 在 Grafana 上長得像一個答案，
	// 而它其實是「不知道」。少一行，查詢會少一個 series，那才問得出來。
	// （同理，一個 machine 都沒有版本時整塊省略 —— 有 # TYPE 沒有 sample
	//   對 Prometheus 來說等於這個指標不存在，那比沒有更難查。）
	nVer := 0
	for _, m := range rows {
		if m.ver != "" {
			nVer++
		}
	}
	if nVer > 0 {
		gauge("clawctl_agent_info", "每一台 agent 自報的版本；值恆為 1，版本在 version label 上。沒報到過、或舊 agent 不送這欄的機器不會有這一行")
		for _, m := range rows {
			if m.ver != "" {
				line("clawctl_agent_info", m.idLabels()+",version="+quote(m.ver), 1)
			}
		}
	}

	cron, err := cronMetrics(h.store, rows, now)
	if err != nil {
		http.Error(w, "讀取排程工作材料失敗: "+err.Error(), http.StatusInternalServerError)
		return
	}
	nJobs, nOverdue, nLastRun := 0, 0, 0
	for _, c := range cron {
		if c.jobsMeasured {
			nJobs++
		}
		if c.scheduleMeasured {
			nOverdue++
		}
		if c.lastRun != nil {
			nLastRun++
		}
	}
	// ⚠ 沒有 sample 就整塊省略。沒有資料庫、沒有 cron_jobs，或舊版本少欄位，
	// 都是「不知道」而不是 0；吐 0 會把它說成這台真的沒有工作。
	if nJobs > 0 {
		gauge("clawctl_openclaw_cron_jobs", "OpenClaw 宣告的排程工作數，啟用與停用分開列出；讀不到 cron_jobs 的機器不會有這一行")
		for _, c := range cron {
			if c.jobsMeasured {
				line("clawctl_openclaw_cron_jobs", c.labels+",state="+quote("enabled"), c.enabled)
				line("clawctl_openclaw_cron_jobs", c.labels+",state="+quote("disabled"), c.total-c.enabled)
			}
		}
	}
	if nOverdue > 0 {
		gauge("clawctl_openclaw_cron_jobs_overdue", "已啟用且下次執行時間已過的排程工作數；讀不到排程欄位的機器不會有這一行")
		for _, c := range cron {
			if c.scheduleMeasured {
				line("clawctl_openclaw_cron_jobs_overdue", c.labels, c.overdue)
			}
		}
	}
	if nLastRun > 0 {
		gauge("clawctl_openclaw_last_cron_run_timestamp_seconds", "OpenClaw 上一次排程工作跑完的時刻（unix 秒）；沒有跑完紀錄的機器不會有這一行")
		for _, c := range cron {
			if c.lastRun != nil {
				line("clawctl_openclaw_last_cron_run_timestamp_seconds", c.labels, c.lastRun.Unix())
			}
		}
	}

	installRows, err := openClawInstallMetrics(h.store, rows)
	if err != nil {
		http.Error(w, "讀取 OpenClaw 安裝材料失敗: "+err.Error(), http.StatusInternalServerError)
		return
	}
	nWritable, nNode, nRunningVersion := 0, 0, 0
	for _, install := range installRows {
		if install.install.RunningDirWritable != nil {
			nWritable++
		}
		if install.install.NodePath != "" && install.install.NodeVersion != "" {
			nNode++
		}
		if install.install.RunningDir != "" && install.install.RunningDirVersion != "" {
			nRunningVersion++
		}
	}
	// ⚠ 三條都只吐真的量到的 sample；舊 agent 的 nil、執行失敗的空字串，
	// 都是「不知道」而不是 0。把未知吐成 0 會把升級前置條件說成已確認失敗。
	if nWritable > 0 {
		gauge("clawctl_openclaw_install_writable", "agent 對 gateway 正在使用的 OpenClaw 套件目錄是否可寫；沒量到的機器不會有這一行")
		for _, install := range installRows {
			if install.install.RunningDirWritable != nil {
				v := 0
				if *install.install.RunningDirWritable {
					v = 1
				}
				line("clawctl_openclaw_install_writable", install.labels, v)
			}
		}
	}
	if nNode > 0 {
		gauge("clawctl_openclaw_node_info", "gateway ExecStart 使用的 node 路徑與該 binary 自報版本；值恆為 1，沒量到不出現")
		for _, install := range installRows {
			if install.install.NodePath != "" && install.install.NodeVersion != "" {
				line("clawctl_openclaw_node_info", install.labels+",version="+quote(install.install.NodeVersion)+",path="+quote(install.install.NodePath), 1)
			}
		}
	}
	if nRunningVersion > 0 {
		gauge("clawctl_openclaw_running_version_info", "gateway 正在使用的 OpenClaw 套件目錄與 package.json 版本；值恆為 1，沒量到不出現")
		for _, install := range installRows {
			if install.install.RunningDir != "" && install.install.RunningDirVersion != "" {
				line("clawctl_openclaw_running_version_info", install.labels+",version="+quote(install.install.RunningDirVersion)+",dir="+quote(install.install.RunningDir), 1)
			}
		}
	}

	// 證據體檢與重複 agent：兩個都是「只報壞消息」的清單，
	// ⚠ 所以 0 的意思是「沒抓到」，不是「證據是健康的」。help 字串要講出來。
	//
	// ⚠⚠ 這兩段的錯誤處理跟上面 Overview 一樣是 500，**不是**「算不出來就跳過」。
	// 原本這裡寫的是 `if err == nil { line(...) }`，於是查詢失敗時這一頁會
	// 帶著正確的 `# HELP`／`# TYPE`、少掉那一行 sample、然後回 200。
	// 那正好是同一個檔案上面三十行親手否決掉的東西：Prometheus 收到 200 就是
	// 抓成功，少掉的那條線在圖上跟「這個指標從來沒存在過」一模一樣 ——
	// 而 Dashboard 那邊可以容忍（`dup = nil`）是因為**有人在看那一頁**，
	// 少一段看得出來。metrics 沒有人在看，只有規則在看。
	dead, err := h.store.DeadSignals(now)
	if err != nil {
		http.Error(w, "讀取證據體檢失敗: "+err.Error(), http.StatusInternalServerError)
		return
	}
	dup, err := h.store.DoubleAgents(now)
	if err != nil {
		http.Error(w, "讀取重複 agent 失敗: "+err.Error(), http.StatusInternalServerError)
		return
	}

	gauge("clawctl_dead_signal_pipelines", "偵測到停止更新的證據管線數")
	line("clawctl_dead_signal_pipelines", "", len(dead))

	ongoing := 0
	for _, d := range dup {
		if d.Ongoing {
			ongoing++
		}
	}
	gauge("clawctl_double_agent_machines", "偵測到 agent 啟動身分重疊的機器數")
	line("clawctl_double_agent_machines", `window="24h"`, len(dup))
	gauge("clawctl_double_agent_machines_ongoing", "其中兩邊到現在都還在送心跳的機器數")
	line("clawctl_double_agent_machines_ongoing", "", ongoing)

	// ── 憑證與宣告的事實 ────────────────────────────────────────────
	//
	// 這一段是為了讓判決能搬到 Prometheus 的規則那側。上面那條
	// 「只吐事實、不吐健康」在這裡有一個具體的形狀：
	//
	//   吐 clawctl_artifact_age_seconds 跟 clawctl_artifact_max_age_seconds，
	//   **不吐** clawctl_artifact_stale。門檻跟事實一起走，規則就寫得成
	//   一條通用的 `age > max_age`，而不必為每一台機器各寫一條。
	//   人宣告的 max_age 還是留在 expectations.json —— 它只是跟著事實
	//   一起被搬到看得到它的地方。
	//
	// `why`（人在 expectations.json 裡寫的那句理由）走 info metric：
	// 值恆為 1，內容在 label 上 —— 跟 clawctl_build_info 把版本放 label 同一招。
	//
	// ⚠ 我原本寫「塞進 label 會炸基數」而沒有放進來，那個理由是錯的：
	//   一條宣告只有一個 why，三條宣告就是三條 series。基數爆炸講的是
	//   **值域不受控**的 label（例如沒人宣告過的事件名），不是長字串。
	//   代價是改寫 why 會長出一條新 series、舊的變 stale —— 那是 info
	//   metric 本來就有的行為，不是問題。
	//
	// 這一格重要的原因：clawctl 的紀律是「why 原封不動出現在告警的理由那一行」。
	// 沒有它，Prometheus 那側的 annotation 就只能寫通用句子，而那是退步。
	// 有了它，規則可以 group_left(why) 把人寫的那句話接回去。
	arts, creds := factRows(ov, now)

	var nChecked, nAged, nWindow, nEv, nEvCount int
	for _, a := range arts {
		if a.checked {
			nChecked++
		}
		if a.hasAge {
			nAged++
		}
		if a.ev != nil {
			nWindow++
			if a.ev.Measured {
				nEv++
				nEvCount += len(a.ev.Declared)
			}
		}
	}

	// ⚠⚠ 下面每一塊都是「沒有 sample 就整塊不要出現」。
	// 一個只有 # HELP / # TYPE、底下沒有任何 sample 的指標，對 Prometheus
	// 來說等於不存在（metrics_test.go 那條規則，2026-09-05 實地抓到）。
	//
	// 而這幾個指標**本來就可能是空的** —— 沒有人宣告任何期望的時候，
	// 就是沒有產出物要量。那種空是誠實的。
	//
	// 但它跟「期望檔載入失敗」長得**一模一樣**，而後者正是 2026-09-04
	// 在 cmdReport / cmdMachines 上真的發生過的 bug。所以另外吐一條
	// 永遠存在的 clawctl_expectations_loaded —— 讓 0 是一個看得見的數字，
	// 不是一片看不出差別的空白。
	gauge("clawctl_expectations_loaded", "名冊上總共有幾條產出物宣告在被量；0＝沒人宣告過，或期望檔沒載進來（兩者要靠設定檔本身分辨）")
	line("clawctl_expectations_loaded", "", len(arts))

	if len(creds) > 0 {
		gauge("clawctl_credential_expiry_seconds", "距離這個 provider 的登入過期還有幾秒；負值＝已經過期了。沒有到期時間的 provider 不會有這一行")
		for _, c := range creds {
			line("clawctl_credential_expiry_seconds", c.labels, int64(c.until.Seconds()))
		}
		// ⚠ 「過期」對短命的票是正常生命週期的一部分（claude 8h、grok 6h 閒置就過期，
		// 下次用到自己續）。要分「閒置」跟「續不回來」得靠壽命跟歷史，所以把
		// 這兩件事實吐出來，讓規則那一側自己算寬限 —— 判斷不在這裡。
		// 算不出壽命的票不吐那一行，規則要把「沒有壽命」當成「沒有寬限」。
		gauge("clawctl_credential_lifetime_seconds", "這種票的名目壽命（到期時間 − 檔案寫入時間）；過期超過這個長度還沒自己續，才算卡住。算不出來的不會有這一行")
		for _, c := range creds {
			if c.lifetime > 0 {
				line("clawctl_credential_lifetime_seconds", c.labels, int64(c.lifetime.Seconds()))
			}
		}
		gauge("clawctl_credential_refreshes_seen", "Hub 看著它的期間，憑證檔自己更新了幾次；0 要跟 clawctl_credential_watched_seconds 一起讀，才知道是「沒續」還是「才剛開始看」")
		for _, c := range creds {
			line("clawctl_credential_refreshes_seen", c.labels, c.refreshes)
		}
		gauge("clawctl_credential_watched_seconds", "Hub 看著這張票多久了（第一筆觀測到現在）")
		for _, c := range creds {
			line("clawctl_credential_watched_seconds", c.labels, int64(c.watched.Seconds()))
		}
	}

	// ⚠ 「檔案不在」跟「檔案很舊」是兩件事，而且前者更嚴重。
	// 只吐 age 的話，檔案不見會讓那條線消失 —— 在圖上跟「這台沒宣告過」
	// 一模一樣。所以 present 要單獨一條，而且 0 也要出現。
	if nChecked > 0 {
		gauge("clawctl_expectation_info", "人在 expectations.json 裡寫的理由；值恆為 1，理由在 why label 上（規則用 group_left(why) 接回去）")
		for _, a := range arts {
			if a.checked {
				line("clawctl_expectation_info", a.labels+",why="+quote(a.why), 1)
			}
		}
	}

	if nChecked > 0 {
		gauge("clawctl_artifact_present", "宣告產出物存在狀態；1=存在，0=不存在")
		for _, a := range arts {
			if a.checked {
				line("clawctl_artifact_present", a.labels, boolGauge(a.exists))
			}
		}
	}

	if nAged > 0 {
		gauge("clawctl_artifact_age_seconds", "產出物最後修改至 Hub 評估時間的秒數")
		for _, a := range arts {
			if a.hasAge {
				line("clawctl_artifact_age_seconds", a.labels, int64(a.age.Seconds()))
			}
		}
	}

	if nChecked > 0 {
		gauge("clawctl_artifact_max_age_seconds", "expectations.json 宣告的產出物最大年齡")
		for _, a := range arts {
			if a.checked {
				line("clawctl_artifact_max_age_seconds", a.labels, int64(a.maxAge.Seconds()))
			}
		}
	}

	if nWindow > 0 {
		gauge("clawctl_event_window_seconds", "事件流觀測窗口秒數")
		for _, a := range arts {
			if a.ev != nil {
				line("clawctl_event_window_seconds", a.labels, int64(a.ev.Window.Seconds()))
			}
		}
	}

	// ⚠⚠ 宣告過但這個窗口內沒出現的事件名，**Count=0 也要吐一行**。
	// 少吐的話，「這個事件沒發生」跟「這條規則根本沒在看」在圖上一模一樣。
	// 這是 internal/state 那四個「不准安靜」的第一個，搬過來還是要成立。
	if nEvCount > 0 {
		gauge("clawctl_event_count", "宣告為 not_ok 的事件在窗口內出現次數")
		for _, a := range arts {
			if a.ev == nil || !a.ev.Measured {
				continue
			}
			for _, e := range a.ev.Declared {
				line("clawctl_event_count", a.labels+",event="+quote(e.Type), e.Count)
			}
		}
	}

	// ⚠ 這裡刻意**只吐種類數，不吐名字**。沒人宣告過的事件名是自由文字，
	// 每次換一組就會長出一批新的 series —— 基數會隨時間漂走，
	// 而 Prometheus 的基數爆炸是會拖垮整個 TSDB 的那種故障。
	// 要看是哪些名字，去詳細頁；這裡只負責讓「有沒人宣告過的東西冒出來」看得見。
	if nEv > 0 {
		gauge("clawctl_event_undeclared_types", "未宣告事件類型數")
		for _, a := range arts {
			if a.ev != nil && a.ev.Measured {
				line("clawctl_event_undeclared_types", a.labels, a.ev.UndeclaredTotal)
			}
		}
	}

	// ⚠⚠ 這一條是整組裡最重要的。欄位名打錯（ts_field/type_field 寫錯）
	// 會讓那條規則**永遠安靜** —— 每一個事件計數都是 0，看起來像一切正常。
	if nEv > 0 {
		gauge("clawctl_event_malformed_lines", "無法解析的事件行數")
		for _, a := range arts {
			if a.ev != nil && a.ev.Measured {
				line("clawctl_event_malformed_lines", a.labels, a.ev.Malformed)
			}
		}
	}

	// ⚠ 撞到讀取上限時，計數是**下界**不是實數（詳細頁寫 ≥N 而不是 ×N）。
	// 一條 `count > 0` 的規則不受影響，但 `count > 50` 的規則會被騙。
	if nEv > 0 {
		gauge("clawctl_event_partial", "1 表示 clawctl_event_count 是窗口內的下界")
		for _, a := range arts {
			if a.ev != nil && a.ev.Measured {
				line("clawctl_event_partial", a.labels, boolGauge(a.ev.Partial))
			}
		}
	}

	if err := h.appendNotifyMetrics(&b); err != nil {
		http.Error(w, "讀取推播狀態失敗: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if h.store != nil {
		h.store.AppendDBMetrics(&b)
	}

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = w.Write([]byte(b.String()))
}

// appendNotifyMetrics writes the daily-report gauges. A success or attempt
// timestamp is omitted until that event has happened; consecutive failures
// and the configured flag are always present so an absent series is not
// mistaken for "zero failures" or "not configured".
func (h *hub) appendNotifyMetrics(b *strings.Builder) error {
	configured := 0
	if h.notifier() != nil {
		configured = 1
	}
	fmt.Fprintf(b, "# HELP clawctl_notify_configured 1 when a notify command is configured, otherwise 0.\n")
	fmt.Fprintf(b, "# TYPE clawctl_notify_configured gauge\n")
	fmt.Fprintf(b, "clawctl_notify_configured %d\n", configured)
	if h.store == nil {
		fmt.Fprintf(b, "# HELP clawctl_notify_consecutive_failures Undelivered attempts since the latest delivered row for this kind.\n")
		fmt.Fprintf(b, "# TYPE clawctl_notify_consecutive_failures gauge\n")
		fmt.Fprintf(b, "clawctl_notify_consecutive_failures{kind=%s} 0\n", quote("daily"))
		return nil
	}
	stats, err := h.store.NotifyKindStats("daily")
	if err != nil {
		return err
	}
	if stats.HasSuccess {
		fmt.Fprintf(b, "# HELP clawctl_notify_last_success_timestamp_seconds Unix time of the latest delivered notification for this kind.\n")
		fmt.Fprintf(b, "# TYPE clawctl_notify_last_success_timestamp_seconds gauge\n")
		fmt.Fprintf(b, "clawctl_notify_last_success_timestamp_seconds{kind=%s} %d\n", quote("daily"), stats.LastSuccess.Unix())
	}
	if stats.HasAttempt {
		fmt.Fprintf(b, "# HELP clawctl_notify_last_attempt_timestamp_seconds Unix time of the latest notification attempt for this kind.\n")
		fmt.Fprintf(b, "# TYPE clawctl_notify_last_attempt_timestamp_seconds gauge\n")
		fmt.Fprintf(b, "clawctl_notify_last_attempt_timestamp_seconds{kind=%s} %d\n", quote("daily"), stats.LastAttempt.Unix())
	}
	fmt.Fprintf(b, "# HELP clawctl_notify_consecutive_failures Undelivered attempts since the latest delivered row for this kind.\n")
	fmt.Fprintf(b, "# TYPE clawctl_notify_consecutive_failures gauge\n")
	fmt.Fprintf(b, "clawctl_notify_consecutive_failures{kind=%s} %d\n", quote("daily"), stats.ConsecutiveFailures)
	return nil
}

type machineMetric struct {
	id, name, st string
	ver          string
	age          time.Duration
	hasAge       bool
	skew         time.Duration
	hasSkew      bool
}

var allJobStates = []deploy.JobState{
	deploy.NotStarted,
	deploy.Claimed,
	deploy.Running,
	deploy.Verifying,
	deploy.Succeeded,
	deploy.Failed,
	deploy.Rejected,
	deploy.LeaseExpired,
	deploy.ManualIntervention,
}

// idLabels 是每一台機器都帶著的那兩個 label，一律一起出現。
// ⚠ 分開寫的話，總有一個指標會只帶 name —— 而那一個就是同名機器炸掉整頁的入口。
func (m machineMetric) idLabels() string {
	return "machine_id=" + quote(m.id) + ",machine=" + quote(m.name)
}

// machineMetrics 把名冊攤成每台一列，照名字排（順序穩定，diff 才看得懂）。
//
// ⚠ 用 ov.Machines（名冊），不是用「有回報的那些」。理由見上面呼叫處。
// ⚠ 同名時用 machine_id 決勝負，否則兩台同名機器的先後順序每次抓都可能不一樣。
func machineMetrics(ov store.Overview, now time.Time) []machineMetric {
	out := make([]machineMetric, 0, len(ov.Machines))
	for _, m := range ov.Machines {
		mm := machineMetric{
			id:   m.MachineID,
			name: m.DisplayName,
			st:   string(m.State),
			ver:  m.Facts.AgentVersion,
		}
		if m.Facts.EverCheckedIn {
			mm.age, mm.hasAge = now.Sub(m.Facts.LastCheckinReceived), true
			// ClockSkew 是 check-in 的 sent_at 減 received_at；沒有 check-in 就沒有東西
			// 可以相減，Facts.ClockSkew 的 0 是 Go 零值，不是量測結果。
			mm.skew, mm.hasSkew = m.Facts.ClockSkew, true
		}
		out = append(out, mm)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].name != out[j].name {
			return out[i].name < out[j].name
		}
		return out[i].id < out[j].id
	})
	return out
}

type cronMetric struct {
	labels                         string
	total, enabled, overdue        int
	jobsMeasured, scheduleMeasured bool
	lastRun                        *time.Time
}

type openClawInstallMetric struct {
	labels  string
	install *model.OpenClawInstall
}

// openClawInstallMetrics 走 cronMetrics 同一條路：store 一台一個吃索引的查詢。
// 這裡不叫 Detail()：Detail 會為每台做十幾個無關查詢，/metrics 不該為三條
// install 線把票帳、歷史與 journal 全部重算一次。
func openClawInstallMetrics(st *store.Store, machines []machineMetric) ([]openClawInstallMetric, error) {
	ids := make([]string, 0, len(machines))
	for _, m := range machines {
		ids = append(ids, m.id)
	}
	installs, err := st.LatestOpenClawInstalls(ids)
	if err != nil {
		return nil, err
	}
	out := make([]openClawInstallMetric, 0, len(machines))
	for _, m := range machines {
		install, ok := installs[m.id]
		if !ok {
			continue
		}
		out = append(out, openClawInstallMetric{labels: m.idLabels(), install: install})
	}
	return out, nil
}

// cronMetrics 取每一台最新的 OpenClaw 觀測，只攜帶結構化排程欄位。
//
// ⚠ 這裡**不叫 Detail()**。叫過，實測把 /metrics 從 57 ms 拖到 300 ms
// （每台一次、每 60 秒一輪）。理由寫在 store.LatestOpenClawDBs 上面。
func cronMetrics(st *store.Store, machines []machineMetric, now time.Time) ([]cronMetric, error) {
	ids := make([]string, 0, len(machines))
	for _, m := range machines {
		ids = append(ids, m.id)
	}
	dbs, err := st.LatestOpenClawDBs(ids)
	if err != nil {
		return nil, err
	}
	out := make([]cronMetric, 0, len(machines))
	for _, m := range machines {
		db, ok := dbs[m.id]
		if !ok {
			continue
		}
		out = append(out, cronMetric{
			labels:           m.idLabels(),
			total:            db.CronJobsTotal,
			enabled:          db.CronJobsEnabled,
			overdue:          db.CronJobsOverdue,
			jobsMeasured:     db.CronJobsTotalMeasured && db.CronJobsEnabledMeasured,
			scheduleMeasured: db.CronJobsScheduleMeasured,
			lastRun:          db.LastCronRunAt,
		})
	}
	return out, nil
}

type artifactRow struct {
	labels  string
	why     string
	checked bool
	exists  bool
	age     time.Duration
	hasAge  bool
	maxAge  time.Duration
	ev      *state.EventFact
}

type credRow struct {
	labels    string
	until     time.Duration
	lifetime  time.Duration // 0 = 算不出來，不吐
	refreshes int
	watched   time.Duration
}

// factRows 把名冊攤平成「每一台 × 每一條宣告」。
//
// ⚠⚠ 為什麼要先攤平、而不是在外面兩層迴圈直接吐：exposition format 要求
// 一個 metric 的所有 sample 必須是**連續的一整塊**。照機器迴圈的話會變成
// 「samplehub1 的 age、samplehub1 的 max_age、sampleagent2 的 age、sampleagent2 的 max_age」——
// 那會讓 Prometheus 把一個指標讀成好幾個 family。
// 這個檔案上面那段註解記著同一個 bug 已經發生過一次（11 個指標被讀成 25 個），
// 而且是官方 parser 抓到的，不是我們自己的測試。
//
// ⚠ 順序跟 machineMetrics 一樣要穩定，否則每次抓的 diff 都看不懂。
func factRows(ov store.Overview, now time.Time) ([]artifactRow, []credRow) {
	rows := machineMetrics(ov, now)
	byID := make(map[string]state.Facts, len(ov.Machines))
	for _, m := range ov.Machines {
		byID[m.MachineID] = m.Facts
	}

	var arts []artifactRow
	var creds []credRow
	for _, m := range rows {
		f := byID[m.id]

		as := append([]state.ArtifactFact(nil), f.Artifacts...)
		sort.Slice(as, func(i, j int) bool {
			if as[i].Unit != as[j].Unit {
				return as[i].Unit < as[j].Unit
			}
			return as[i].Artifact < as[j].Artifact
		})
		for _, a := range as {
			r := artifactRow{
				labels:  m.idLabels() + ",unit=" + quote(a.Unit) + ",artifact=" + quote(a.Artifact),
				why:     a.Why,
				checked: a.Checked,
				exists:  a.Exists,
				maxAge:  a.MaxAge,
				ev:      a.Events,
			}
			// ⚠ 只有真的量到 ModTime 才有年齡。宣告了但還沒量到 ≠ 通過，
			// 所以這裡寧可少一行，也不要吐一個 0 假裝它剛剛更新過。
			if a.Checked && a.Exists && a.ModTime != nil {
				r.age, r.hasAge = now.Sub(*a.ModTime), true
			}
			arts = append(arts, r)
		}

		cs := append([]state.CredFact(nil), f.Credentials...)
		sort.Slice(cs, func(i, j int) bool { return cs[i].Provider < cs[j].Provider })
		for _, c := range cs {
			if c.ExpiresAt == nil {
				continue
			}
			creds = append(creds, credRow{
				labels:    m.idLabels() + ",provider=" + quote(c.Provider),
				until:     c.ExpiresAt.Sub(now),
				lifetime:  c.Lifetime,
				refreshes: c.RefreshesSeen,
				watched:   c.WatchedFor,
			})
		}
	}
	return arts, creds
}

func boolGauge(b bool) int {
	if b {
		return 1
	}
	return 0
}

// quote 把值包成 Prometheus 的 label value。
//
// ⚠ 一定要跳脫。機器的顯示名稱是人打進名冊的自由文字，裡面出現一個
// 引號就會生出一行語法錯誤的 metrics —— 而 Prometheus 遇到解析錯誤是
// **整頁丟掉**，不是丟掉那一行。於是一台名字裡有引號的機器，
// 會讓整個機隊的監控安靜地消失。
func quote(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return `"` + r.Replace(s) + `"`
}
