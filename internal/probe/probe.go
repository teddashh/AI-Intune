// Package probe 是 agent 端的觀測收集器 —— 只收事實，不下判決。
//
// 三條規則（違反任何一條都會讓整個健康模型失去意義）：
//
//  1. 唯讀。不寫檔、不重啟服務、不呼叫遠端 API。
//  2. 不輸出任何 token / key / 憑證內容，只輸出過期時間、更新時間與 mtime。
//  3. 拿不到的東西回報零值 + Reason/Note，不猜、不填預設值。
//
// 邏輯移植自 tools/fleet-probe.py（已在 5 台實機上跑過，輸出經人工核對）。
package probe

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"
	_ "modernc.org/sqlite" // 純 Go，不需要 cgo；driver 名字是 "sqlite" 不是 "sqlite3"

	"github.com/teddashh/AI-Intune/internal/model"
	"github.com/teddashh/AI-Intune/internal/processenv"
)

// Version 由 build 時的 -ldflags 打進來。沒打就退回 VCS revision，再退回 "dev"。
var Version string

// ---------------------------------------------------------------- 常數
//
// 這些數字都有理由，改之前先讀理由。

const (
	// cmdTimeout：一般外部指令的上限。⚠ 每一個 subprocess 都必須有 timeout ——
	// 一支卡住的 CLI 不可以讓整支 probe 跟著卡住。
	cmdTimeout = 10 * time.Second

	// versionTimeout：`openclaw --version` 實測在部分機器上要數秒到十幾秒
	// （它會先做一輪版本一致性檢查並印出多行警告），所以給它更寬的額度。
	versionTimeout = 25 * time.Second

	// quickTimeout：tailscale 這種「不在也無所謂」的查詢，不值得等。
	quickTimeout = 5 * time.Second

	// loginShellTimeout：起一個 login shell 只為了問它的 PATH。
	// ⚠ 一輪觀測只問一次（見 cliTools），所以這裡可以給得比 quickTimeout 寬 ——
	// 但也不能太寬：rc 檔裡有人放 nvm、conda 這種要跑好幾秒的東西是常態。
	loginShellTimeout = 15 * time.Second

	// dbTimeout：SQLite 查詢的上限。DB 就在本機，慢就是有事，不要無限等。
	dbTimeout = 10 * time.Second

	// waitDelay：process 結束後仍有子孫抓著 stdout 時的收屍寬限。
	// ⚠ 沒有這個，一個 fork 出 daemon 的 CLI 會讓 cmd.Output() 永遠不返回，
	// 即使它本人早就退出了 —— context 逾時也救不了，因為 pipe 還開著。
	waitDelay = 2 * time.Second

	// ⚠ 這裡刻意「沒有」expires_soon 的門檻常數。原因見 classifyCred。

	maxSummaryRunes = 240
	maxVersionRaw   = 200
	// maxArgv：命令列太長就截斷。實測有 agent 帶著幾 KB 的 prompt 當參數。
	maxArgv = 64
	// minPlausibleProcs：低於這個數字就別說「沒有在跑」。
	// ⚠ 一台開著的 Linux 至少有幾十個 process。掃到個位數只有一種解釋：
	// 我的視野被擋住了。實測 samplehub1 有 553 個。
	// 0 也算 restricted，因為 ReadDir 成功就代表 /proc 讀得到。
	minPlausibleProcs = 20
	maxToolRaw        = 120
	recentSummaries   = 5
	grokKeyPrefix     = "https://auth.x.ai"
	cronCountsWindow  = 24 * time.Hour
)

// watchedUnits 是要觀測的 systemd --user unit。
// ⚠ 沒裝的 unit 也要回報（Present=false），因為「名冊是分母」：
// 一台以為裝了 bat-server 其實沒裝的機器不可以從畫面上消失。
var watchedUnits = []string{
	// ⚠ agent 觀測**自己的** unit，而這不是自證。
	//
	// 回報的內容不是 agent 對自己的評語，是 systemd 對它的計數 ——
	// NRestarts 是 systemd 因為它死掉而重新拉起來的次數，agent 碰不到那個數字。
	// agent 在這裡只是搬運工，而搬運工是我們所有觀測共有的角色。
	//
	// 收它的理由：`agent_started_at` 換了幾個值分不出「crash-loop」與
	// 「有人在部署」，兩者都是「process 重啟了很多次」。實測 systemd 分得出來：
	// 自動重啟會讓 NRestarts 往上加，而 `systemctl restart` 會把它歸零。
	"clawctl-agent.service",
	"clawctl-hermes.service",
	"openclaw-gateway.service",
	"openclaw-watcher.service",
	"bat-server.service",
	"hermes-gateway.service",
}

// provider 是「一家 AI 服務」在這台機器上的兩個面：一支 CLI、一份本機憑證。
//
// ⚠⚠ 這張表存在的理由是**兩張手寫的清單對不起來**，而沒有任何東西在比它們。
//
// 原本 watchedTools 是 {claude, codex, grok, agy, openclaw}，
// credentials() 讀的是 {claude, codex, grok, gemini, openclaw} ——
// 兩張清單各改各的，結果實測（samplehub1，2026-09-03）：
//
//   - **gemini**：`~/.gemini` 真的存在，憑證每兩分鐘被讀一次並回報，
//     但那支 CLI 從來沒有被檢查過。裝上 `@google/gemini-cli` 之後，
//     畫面會顯示它的登入狀態，卻永遠說不出它裝了沒有、版號多少、在不在跑。
//   - **agy**：CLI 被監看（全機隊 present=false，npm 上也沒有），
//     但它的憑證從來沒有被讀過。
//
// 兩個方向都漏，而且漏的方式完全看不出來 —— 一個沒有被檢查的東西
// 不會亮紅燈，它根本不會有那一列。這是 §5.0：bug 住在零件中間。
//
// ⚠ 修法不是「把兩張清單改成一樣」。CLI 與憑證本來就可以只有一邊 ——
// 重點是那個不對稱必須是**寫出來的決定**，不是兩張清單各自漂移的結果。
// 所以每一個不對稱都要在 Reason 裡講清楚為什麼。
type provider struct {
	Name string
	// HasCLI：有一支同名的指令要檢查（LookPath + --version + 在不在跑）。
	HasCLI bool
	// Cred 讀本機憑證，nil 代表這家沒有本機憑證可讀 —— 而那要有理由。
	Cred func(home string, now time.Time) model.Credential
	// Reason 只在 HasCLI 與 Cred 不對稱時才要填，說明為什麼只有一邊。
	Reason string
}

// providers 是唯一的一份名單。⚠ 加一家就在這裡加，兩邊會自動跟上。
var providers = []provider{
	{Name: "claude", HasCLI: true, Cred: claudeCred},
	{Name: "codex", HasCLI: true, Cred: codexCred},
	{Name: "grok", HasCLI: true, Cred: grokCred},
	{
		// ⚠ 2026-09-03 之前這一家只有憑證那一半。
		Name: "gemini", HasCLI: true, Cred: geminiCred,
	},
	{
		// openclaw 不是一家 AI 服務，是那個 agent 本身 —— 但它兩邊都有：
		// 一支 CLI，加上一份「它自己拿什麼票在跑」的設定。
		Name: "openclaw", HasCLI: true,
		Cred: func(home string, _ time.Time) model.Credential { return openclawCred(home) },
	},
	{
		// ⚠ agy 全機隊 present=false，npm 上也找不到它。留著它是刻意的：
		// 一個「我有在找、但它不在」的答案，跟「我從來沒找過」是兩件事，
		// 而前者才是這個產品要給的答案（名冊是分母，對工具也一樣）。
		Name: "agy", HasCLI: true, Cred: nil,
		Reason: "沒有已知的本機憑證檔位置。全機隊都沒有裝，" +
			"該位置尚未觀測。",
	},
}

// watchedTools 是要檢查的指令名。⚠ 從 providers 生出來，不要再手寫一份。
var watchedTools = func() []string {
	out := make([]string, 0, len(providers))
	for _, p := range providers {
		if p.HasCLI {
			out = append(out, p.Name)
		}
	}
	return out
}()

// versionRe 從 --version 的輸出裡撈版號。2026.6.1 這種年份版號也吃得下，
// 因為 \d+ 不限位數。
var versionRe = regexp.MustCompile(`\d+\.\d+\.\d+`)

// nodeModulesRe 從 realpath 反推 npm 套件目錄，用來讀第二來源的 package.json。
// ⚠ 故意不用「往上找到第一個 package.json」：那會在 standalone 安裝
// （codex 的 releases/<ver>/bin/codex）撈到不相干的套件，變成假的第二來源。
// ⚠ 第二段只在 scope（@foo/bar）時才吃。原本寫成 `[^/]+(?:/[^/]+)?` 會貪心地
// 把 `node_modules/openclaw/openclaw.mjs` 整串當成套件目錄，於是 package.json
// 永遠讀不到，而「版本多來源比對」就會靜靜地退化成單一來源 —— 那正是
// 我們用來抓 grok 版本謊報（1.0.3 vs 1.0.13）的那條線。
var nodeModulesRe = regexp.MustCompile(`(.*/node_modules/(?:@[^/]+/)?[^/]+)/`)

// lastObservationUnixNano 是本行程最後一次 Collect 成功的時間。
// 心跳靠它算 observation_age —— 心跳有到但這個一直變大 = agent 在線但沒在觀測。
var lastObservationUnixNano atomic.Int64

// ---------------------------------------------------------------- 對外

// Collect 做一次完整觀測（約 10 分鐘一次）。
//
// ⚠ 它幾乎不會回 error：拿不到的欄位就是零值 + Reason，部分觀測遠比沒有觀測有用。
// 只有 ctx 在進場時就已經死掉才回 error —— 那代表呼叫端已經放棄了。
func Collect(ctx context.Context) (model.ObservationBatch, error) {
	if err := ctx.Err(); err != nil {
		return model.ObservationBatch{}, err
	}
	now := time.Now().UTC()
	home := homeDir()

	// ⚠ /proc 只掃一次，CLI 工具與 bat-server 共用同一份切片。
	procs, processScan := scanProcesses()

	// CLI 工具先收，因為 openclaw 的 CLI 版號可以直接沿用，
	// 不必再花一次 25 秒的 timeout 額度跑第二遍 `openclaw --version`。
	tools, bat := processObservations(ctx, procs, processScan)

	// ⚠ units 先算出來給 journal 用：只有 Present 的 unit 才值得跑 journalctl。
	units := systemdUnits(ctx, watchedUnits)

	batch := model.ObservationBatch{
		SchemaVersion: model.SchemaVersion,
		MeasuredAt:    now,
		Identity:      identity(ctx),
		Resources:     resources(),
		Systemd:       units,
		Journals:      unitJournals(ctx, units),
		OpenClaw:      openClaw(ctx, home, tools),
		Credentials:   credentials(home, now),
		CLITools:      tools,
		BAT:           bat,
	}

	lastObservationUnixNano.Store(time.Now().UnixNano())
	return batch, nil
}

// processObservations 把同一輪 process 掃描交給 CLI 工具與 BAT。
// ⚠ 兩邊必須拿到同一個 processScan：任何一邊改回從 len(procs) 推論，
// 同一台機器就會在 CLI 工具與 BAT 兩個表面講出不同的掃描品質。
func processObservations(ctx context.Context, procs []procInfo, processScan string) ([]model.CLITool, model.BAT) {
	return cliTools(ctx, procs, processScan), batServer(procs, processScan)
}

// Heartbeat 是 2 分鐘一次的心跳，必須小而快。
//
// ⚠ 這裡只碰 /proc、statfs 與行程內的快取。不 spawn subprocess、不開 sqlite ——
// observation 連續失敗時心跳仍要送得出去，否則 Hub 分不出「機器死了」與
// 「只是狀態過期」。
func Heartbeat(ctx context.Context, seq int64) (model.Checkin, error) {
	if err := ctx.Err(); err != nil {
		return model.Checkin{}, err
	}
	c := model.Checkin{
		SchemaVersion: model.SchemaVersion,
		SentAt:        time.Now().UTC(),
		AgentVersion:  agentVersion(),
		BootID:        bootID(),
		// ⚠ processStarted 是 package 變數，在 process 起來時決定一次。
		// 不准每次心跳重算 —— 重算會讓每顆心跳都長得像新 process，
		// crash-loop 偵測就會反過來永遠都在告警。
		AgentStartedAt: processStarted,
		AgentSeq:       seq,
		UptimeSeconds:  uptimeSeconds(),
	}
	c.DiskFreeBytes, c.DiskTotalBytes = diskUsage("/")
	c.ObservationAgeSeconds = observationAge(time.Now())
	// MaxSeenRevision / MaxAppliedRevision 是 Phase 4 的東西，由 agent 的 journal
	// 填，probe 不知道也不該猜。
	return c, nil
}

// observationAge 回報本機最後一次完整觀測到現在幾秒。
// ⚠ 從沒觀測過就回 nil，不回 0 —— 0 的意思是「剛剛才觀測完」，正好相反。
func observationAge(now time.Time) *int64 {
	ns := lastObservationUnixNano.Load()
	if ns == 0 {
		return nil
	}
	age := int64(now.Sub(time.Unix(0, ns)).Seconds())
	if age < 0 {
		age = 0
	}
	return &age
}

func agentVersion() string {
	if Version != "" {
		return Version
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			if s.Key == "vcs.revision" && s.Value != "" {
				return s.Value
			}
		}
	}
	return "dev"
}

// ---------------------------------------------------------------- identity

// identity 收機器身分。
// ⚠ hostname 不能當 machine_id：實測 tailnet 名稱與 hostname 常常不一樣
// （tailnet 上的 sampleagent1 主機名是 sample-agent-node）。
func identity(ctx context.Context) model.Identity {
	id := model.Identity{
		OS:            prettyOSName(),
		UnixUser:      unixUser(),
		MachineIDHint: machineIDHint(),
		BootID:        bootID(),
	}
	if h, err := os.Hostname(); err == nil {
		id.Hostname = h
	}
	var uts unix.Utsname
	if err := unix.Uname(&uts); err == nil {
		id.Kernel = utsString(uts.Release[:])
		id.Arch = utsString(uts.Machine[:]) // x86_64 / aarch64，不是 GOARCH 的 amd64
	}
	// tailscale 不在也無所謂，拿不到就是空字串（欄位是 omitempty）。
	if out, _, err := run(ctx, quickTimeout, "tailscale", "ip", "-4"); err == nil {
		if lines := strings.Fields(out); len(lines) > 0 {
			id.TailscaleIP = lines[0]
		}
	}
	// linger：沒開的機器會在使用者登出時「假離線」。Hub 靠這個把假離線
	// 跟真離線分開講 —— 見 docs/OPEN-QUESTIONS.md Q4。
	id.LingerEnabled, id.LingerMeasured = lingerFacts(id.UnixUser)
	return id
}

// machineIDHint：比 hostname 穩定，但重灌會變，所以只是候選之一，不是身分本身。
func machineIDHint() string {
	for _, p := range []string{"/etc/machine-id", "/var/lib/dbus/machine-id"} {
		if s := readTrimmed(p); s != "" {
			return s
		}
	}
	return ""
}

// bootID 用來抓 crash-loop：一小時內換 10 次就是一直在崩，不是健康。
func bootID() string { return readTrimmed("/proc/sys/kernel/random/boot_id") }

// processStarted：這個 process 起來的時刻，agent 的身分證。
var processStarted = time.Now().UTC()

func unixUser() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	// os/user 在沒有 cgo 又不在 /etc/passwd 裡時會失敗，退回環境變數。
	if s := os.Getenv("USER"); s != "" {
		return s
	}
	return ""
}

func homeDir() string {
	if h, err := os.UserHomeDir(); err == nil && h != "" {
		return h
	}
	if u, err := user.Current(); err == nil {
		return u.HomeDir
	}
	return ""
}

func utsString(b []byte) string {
	if i := indexZero(b); i >= 0 {
		b = b[:i]
	}
	return string(b)
}

func indexZero(b []byte) int {
	for i, c := range b {
		if c == 0 {
			return i
		}
	}
	return -1
}

// ---------------------------------------------------------------- resources

func resources() model.Resources {
	r := model.Resources{CPUCount: runtime.NumCPU()}
	r.DiskFreeBytes, r.DiskTotalBytes = diskUsage("/")
	r.MemTotalBytes, r.MemAvailableBytes = memInfo()
	r.Load1m = load1m()
	return r
}

// diskUsage 回 (可用, 總量)。用 Bavail（非 root 可用）而不是 Bfree，
// 因為 agent 不是 root，保留區塊對它來說就是不存在。
func diskUsage(path string) (free, total int64) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, 0
	}
	unit := statfsBlockUnit(&st)
	return int64(st.Bavail) * unit, int64(st.Blocks) * unit
}

func memInfo() (total, available int64) {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		key, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			continue
		}
		kb, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil {
			continue
		}
		switch key {
		case "MemTotal":
			total = kb * 1024
		case "MemAvailable":
			available = kb * 1024
		}
	}
	return total, available
}

// ---------------------------------------------------------------- systemd

// systemdUnits 收 unit 觀測。
//
// ⚠ 不要用 `systemctl is-active` 判健康：Restart=always 讓它恆為 "active"，
// 一個每 10 秒崩一次的服務也是 "active"。有資訊量的是 ActiveEnterTimestamp
// 一直在變，以及 NRestarts。
//
// Unit 的 wire 契約有三態：Present=true 是 unit 存在；!Present && Measured
// 是 systemctl 明確回答沒有這個 unit；!Present && !Measured 是 systemctl
// 問不到，或 payload 來自還不會送 Measured 的舊 agent。舊 agent 一律落在
// 第三格，fail-closed 成「不知道」，不可以講成「沒有這個 unit」。
func systemdUnits(ctx context.Context, names []string) []model.Unit {
	units := make([]model.Unit, 0, len(names))
	unixTS := true // 先試 --timestamp=unix，舊版 systemd 不認得就整輪退回人類格式
	for _, name := range names {
		u, ok := showUnit(ctx, name, unixTS)
		if !ok && unixTS {
			unixTS = false
			u, _ = showUnit(ctx, name, false)
		}
		units = append(units, u)
	}
	return units
}

func showUnit(ctx context.Context, name string, unixTS bool) (model.Unit, bool) {
	u := model.Unit{Name: name}
	args := []string{"--user", "show", name}
	if unixTS {
		// @<epoch> 比 "Wed 2026-09-02 00:40:26 EDT" 好解析太多：時區縮寫在
		// DST 換季之後會解錯一小時。
		args = append(args, "--timestamp=unix")
	}
	args = append(args,
		"-p", "LoadState", "-p", "ActiveState", "-p", "SubState",
		"-p", "ActiveEnterTimestamp", "-p", "NRestarts", "-p", "ExecMainPID")

	out, stderr, err := run(ctx, cmdTimeout, "systemctl", args...)
	if err != nil || out == "" {
		u.Reason = commandFailure("systemctl --user show "+name, out, stderr, err)
		return u, false
	}
	props := parseProps(out)
	if props["LoadState"] == "not-found" {
		u.Measured = true
		return u, true // systemctl 明確回答 unit 不存在
	}
	if props["LoadState"] == "" {
		u.Reason = "systemctl 沒有回報 LoadState"
		return u, true // 呼叫成功，不觸發舊版 systemd 的 timestamp 退回重試
	}
	u.Present = true
	u.Measured = true
	u.ActiveState = props["ActiveState"]
	u.SubState = props["SubState"]
	u.ActiveEnterTimestamp = parseSystemdTime(props["ActiveEnterTimestamp"])
	u.NRestarts = atoiSafe(props["NRestarts"])
	u.MainPID = atoiSafe(props["ExecMainPID"])
	return u, true
}

func parseProps(out string) map[string]string {
	props := make(map[string]string)
	for _, line := range strings.Split(out, "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			props[k] = strings.TrimSpace(v)
		}
	}
	return props
}

// parseSystemdTime 吃兩種格式：--timestamp=unix 的 "@1788324026"，
// 以及預設的 "Wed 2026-09-02 00:40:26 EDT"。空字串代表 unit 從沒 active 過。
func parseSystemdTime(s string) *time.Time {
	s = strings.TrimSpace(s)
	if s == "" || s == "n/a" {
		return nil
	}
	if rest, ok := strings.CutPrefix(s, "@"); ok {
		// 有些版本印 "@1788324026" 有些印 "@1788324026.123456"
		sec, frac, _ := strings.Cut(rest, ".")
		epoch, err := strconv.ParseInt(sec, 10, 64)
		if err != nil {
			return nil
		}
		t := time.Unix(epoch, 0).UTC()
		if micro, err := strconv.ParseInt(frac, 10, 64); err == nil && frac != "" {
			t = t.Add(time.Duration(micro) * time.Microsecond)
		}
		return &t
	}
	// ⚠ 用 Local 解：字串裡的時區縮寫是本機的，Go 對不認得的縮寫會給 offset 0。
	if t, err := time.ParseInLocation("Mon 2006-01-02 15:04:05 MST", s, time.Local); err == nil {
		utc := t.UTC()
		return &utc
	}
	return nil
}

// ---------------------------------------------------------------- openclaw

// openClaw 收三個版本座標與 L1 存活訊號。
//
// ⚠ 核心警告：上游的 status='ok' 意思是「agent 的回合正常結束並產出文字」，
// 不是「任務成功」。實測 64% 的 ok 其 summary 在描述失敗。所以這裡只回報
// 「最後一次跑完是什麼時候」(L1)，外加把 summary 原文帶回去給人看 (L2)。
// 永遠不要在這裡加一個布林值說它好不好。
func openClaw(ctx context.Context, home string, tools []model.CLITool) model.OpenClaw {
	oc := model.OpenClaw{Install: discoverInstall(ctx, defaultInstallDeps(home))}
	if oc.Install != nil {
		oc.GatewayVersion = oc.Install.RunningDirVersion
	}
	root := filepath.Join(home, ".openclaw")
	if fi, err := os.Stat(root); err != nil || !fi.IsDir() {
		oc.Reason = "no ~/.openclaw"
		return oc
	}
	oc.Present = true

	// CLI 版號沿用 cliTools 的結果 —— `openclaw --version` 很慢，跑一次就好。
	// ⚠ 它只報 CLI 這一個座標，不是 gateway 也不是 upstream。
	if t := findTool(tools, "openclaw"); t != nil {
		oc.CLIVersion = t.VersionReported
		oc.CLIVersionRaw = truncRunes(t.VersionRaw, maxVersionRaw)
		if oc.CLIVersion == "" {
			// model.OpenClaw 沒有 cli_version_reason 欄位，只好借 Reason 講。
			// ⚠ 「有 ~/.openclaw 但 CLI 不在 PATH 上」是真的會發生的組合
			//（用 systemd unit 直接跑 dist/index.js 的機器），不能默默留白。
			oc.Reason = "cli_version unavailable: " +
				firstNonEmpty(t.VersionReason, "openclaw not on PATH")
		}
	}
	// UpstreamVersion 留空：唯一的來源是 npm registry，而 probe 不准打遠端 API。
	// 寧可空白也不假造 —— 見本檔開頭規則 3。

	oc.DB = openClawDB(ctx, root)
	oc.CrashBundles = crashBundleCount(root)
	return oc
}

// openClawDB 讀狀態資料庫。
//
// ⚠ 兩種 layout 同時活在機隊上，寫死一條路徑今天就會在 5 台裡錯 2 台：
//
//	consolidated (2026.6.x) → ~/.openclaw/state/openclaw.sqlite
//	split        (2026.5.x) → ~/.openclaw/tasks/runs.sqlite
//
// 欄位相同，所以 L1 兩種都拿得到。sampleagent3 上還留著 *.migrated 後綴的舊檔，
// 那是遷移的殘骸，不可以當成 split layout 的證據。
// strayOpenClawDBs 在 ~/.openclaw 底下找我們不認得的 sqlite 檔。
//
// ⚠ 只找兩層深，而且**排除 .migrated 殘骸**（sampleagent3 上就有一個）。
// 遷移剩下的屍體檔不是「新 layout」的證據 —— 拿它當證據，
// 每一台升級過的機器都會被誤標成 unsupported。
func strayOpenClawDBs(root string) []string {
	known := map[string]bool{
		filepath.Join(root, "state", "openclaw.sqlite"): true,
		filepath.Join(root, "tasks", "runs.sqlite"):     true,
	}
	var out []string
	for _, pat := range []string{"*.sqlite", "*/*.sqlite", "*.db", "*/*.db"} {
		hits, _ := filepath.Glob(filepath.Join(root, pat))
		for _, h := range hits {
			if known[h] || strings.Contains(h, ".migrated") {
				continue
			}
			if fi, err := os.Stat(h); err == nil && !fi.IsDir() && fi.Size() > 0 {
				out = append(out, h)
			}
		}
	}
	sort.Strings(out)
	return dedupe(out)
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	out := in[:0]
	for _, v := range in {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

func openClawDB(ctx context.Context, root string) *model.OpenClawDB {
	layouts := []struct{ name, path string }{
		{"consolidated", filepath.Join(root, "state", "openclaw.sqlite")},
		{"split", filepath.Join(root, "tasks", "runs.sqlite")},
	}
	d := &model.OpenClawDB{}
	for _, l := range layouts {
		if fi, err := os.Stat(l.path); err == nil && !fi.IsDir() {
			d.Present, d.Layout, d.Path = true, l.name, l.path
			break
		}
	}
	if !d.Present {
		d.Reason = "no state/openclaw.sqlite nor tasks/runs.sqlite"
		// ⚠ 在說「沒有資料庫」之前，先確認不是「有，但搬家了」。
		// 這兩句話在畫面上很像，要做的事完全相反。
		if found := strayOpenClawDBs(root); len(found) > 0 {
			d.Support, d.FoundAt = model.SupportUnsupported, found
			d.Reason = "認得的兩條路徑上都沒有資料庫，但底下找得到別的 sqlite —— " +
				"這個 OpenClaw 版本的 layout 我沒見過"
		}
		return d
	}

	// ⚠ mode=ro，絕對不可以用 immutable=1：實測 WAL 有數 MB 未 checkpoint，
	// immutable 會讓 SQLite 跳過 WAL 而讀到好幾小時前的舊資料 —— 那比沒有資料更糟，
	// 因為它看起來是對的。
	db, err := sql.Open("sqlite", "file:"+d.Path+"?mode=ro")
	if err != nil {
		d.Reason = "sqlite: " + err.Error()
		return d
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	qctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()

	tables, err := tableSet(qctx, db)
	if err != nil {
		d.Reason = "sqlite: " + err.Error()
		return d
	}
	if tables["task_runs"] {
		if err := readTaskRuns(qctx, db, d); err != nil {
			d.Reason = "sqlite: " + err.Error()
		}
	}
	if tables["cron_run_logs"] {
		if err := readCronLogs(qctx, db, d); err != nil && d.Reason == "" {
			d.Reason = "sqlite: " + err.Error()
		}
		if err := readOccupancyFromDB(qctx, db, d); err != nil && d.Reason == "" {
			d.Reason = "sqlite: " + err.Error()
		}
	} else {
		// ⚠ split layout（2026.5.x）的 runs.sqlite **沒有 cron_run_logs**。
		// 那台機器的 cron 證據在 ~/.openclaw/cron/runs/*.jsonl 裡，
		// 欄位形狀一樣但是 JSON。實測 sampleagent2：2942 行、2793 行有 provider。
		//
		// 這一段是照著真機挖出來的，不是照 SPEC 寫的 —— SPEC 假設有一張
		// task_event 表，而那張表在任何一台上都不存在。
		readOccupancyFromJSONL(filepath.Join(root, "cron", "runs"), d)
	}
	if d.Layout == "split" {
		readCronJobsFromJSON(filepath.Join(root, "cron"), d)
	}
	if tables["cron_jobs"] {
		if err := readCronJobs(qctx, db, d); err != nil && d.Reason == "" {
			d.Reason = "sqlite: " + err.Error()
		}
	}
	// ⚠ cron_jobs 有 last_run_at_ms，但那是「開始」時間不是完成時間，
	// model 刻意沒有收它的欄位。不要因為它看起來很方便就加回來。
	return d
}

// OpenClawDBDir 使用觀測器相同的 layout 優先序找資料庫，只交回應快照的目錄。
// ⚠ consolidated 與 split 同時活在機隊上；寫死 state/ 會漏掉 2026.5.x 的 tasks/。
func OpenClawDBDir(home string) (dir string, layout string, ok bool) {
	root := filepath.Join(home, ".openclaw")
	layouts := []struct{ name, path string }{
		{"consolidated", filepath.Join(root, "state", "openclaw.sqlite")},
		{"split", filepath.Join(root, "tasks", "runs.sqlite")},
	}
	for _, candidate := range layouts {
		if fi, err := os.Stat(candidate.path); err == nil && !fi.IsDir() {
			return filepath.Dir(candidate.path), candidate.name, true
		}
	}
	return "", "", false
}

// occupancyLimit 是每次上報最多帶幾筆占用證據。
//
// ⚠ 這是「每次觀測的增量上限」不是「總量上限」—— Hub 那邊是 append 帳本，
// 十分鐘一次、每次 200 筆，跟得上任何正常的 cron 頻率。設上限的理由是
// 一台積了七千筆歷史的機器不該在第一次觀測就送出七千筆。
const occupancyLimit = 200

// occupancyWindow：只看這段時間內的執行紀錄。
// 帳本要回答的是「現在哪張票在被用」，三十天前的事沒有幫助。
const occupancyWindow = 30 * 24 * time.Hour

// readOccupancyFromDB 讀 consolidated layout 的 cron_run_logs。
//
// ⚠ 先確認欄位存在再查。`cron_run_logs` 的 schema 會隨 OpenClaw 版本變 ——
// provider / model / total_tokens 都是後來才加的。少一個欄位就讓整份觀測
// 掛掉，等於「舊版本的機器在畫面上消失」，而那正是這個產品要修的那個 bug。
// 沒有 provider 欄位就是沒有占用證據，如實留白，不是錯誤。
func readOccupancyFromDB(ctx context.Context, db *sql.DB, d *model.OpenClawDB) error {
	cols, err := columnSet(ctx, db, "cron_run_logs")
	if err != nil {
		return err
	}
	if !cols["provider"] || !cols["ts"] {
		return nil
	}
	cutoff := time.Now().Add(-occupancyWindow).UnixMilli()

	// ⚠ 先數總筆數再撈，而且**兩個數字都要送**。
	// 只送撈回來的那些，Hub 會以為帳本是完整的。
	if err := db.QueryRowContext(ctx, `
SELECT COUNT(*), SUM(CASE WHEN provider IS NULL OR provider = '' THEN 1 ELSE 0 END)
  FROM cron_run_logs WHERE ts > ?`, cutoff).
		Scan(&d.OccupancyRowsSeen, &d.OccupancyRowsNoProvider); err != nil {
		return err
	}

	// 選欄位時挑掉這個版本沒有的，用 NULL 補位 —— 欄位順序固定，Scan 才不會錯位。
	col := func(name string) string {
		if cols[name] {
			return name
		}
		return "NULL"
	}
	rows, err := db.QueryContext(ctx, fmt.Sprintf(`
SELECT %s, ts, %s, provider, %s, %s, %s, %s
  FROM cron_run_logs
 WHERE ts > ? AND provider IS NOT NULL AND provider != ''
 ORDER BY ts DESC LIMIT ?`,
		col("job_id"), col("status"), col("model"),
		col("session_key"), col("error"), col("total_tokens")), cutoff, occupancyLimit)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var jobID, status, provider, mdl, sessionKey, errText sql.NullString
		var ts, tokens sql.NullInt64
		if err := rows.Scan(&jobID, &ts, &status, &provider, &mdl, &sessionKey, &errText, &tokens); err != nil {
			return err
		}
		e := model.OccupancyEvidence{
			Source: "cron_run_logs", JobID: jobID.String,
			Status: status.String, Provider: provider.String, Model: mdl.String,
			SessionKey: sessionKey.String, AgentID: agentFromSessionKey(sessionKey.String),
			ErrorText: truncRunes(errText.String, maxSummaryRunes), TotalTokens: int(tokens.Int64),
		}
		if ts.Valid {
			e.At = *msToTime(ts.Int64)
		}
		d.Occupancy = append(d.Occupancy, e)
	}
	return rows.Err()
}

// readOccupancyFromJSONL 讀 split layout 的 ~/.openclaw/cron/runs/*.jsonl。
//
// ⚠ 這裡刻意不回傳 error。一個讀不到的目錄、一行壞掉的 JSON，都不該讓
// 整份觀測失敗 —— 部分失敗仍然要送（見 Collect 的註解）。壞行就跳過，
// 但**跳過的都算進 OccupancyRowsSeen**，所以帳面上看得出落差。
func readOccupancyFromJSONL(dir string, d *model.OpenClawDB) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-occupancyWindow).UnixMilli()
	var all []model.OccupancyEvidence
	for _, ent := range ents {
		if ent.IsDir() || !strings.HasSuffix(ent.Name(), ".jsonl") {
			continue
		}
		f, err := os.Open(filepath.Join(dir, ent.Name()))
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" {
				continue
			}
			var r cronRunLine
			if err := json.Unmarshal([]byte(line), &r); err != nil {
				continue
			}
			if r.TS <= cutoff {
				continue
			}
			d.OccupancyRowsSeen++
			if r.Provider == "" {
				d.OccupancyRowsNoProvider++
				continue
			}
			e := model.OccupancyEvidence{
				Source: "cron_runs_jsonl", JobID: r.JobID, At: *msToTime(r.TS),
				Status: r.Status, Provider: r.Provider, Model: r.Model,
				SessionKey: r.SessionKey, AgentID: agentFromSessionKey(r.SessionKey),
				ErrorText: truncRunes(r.Error, maxSummaryRunes), TotalTokens: r.Usage.TotalTokens,
			}
			all = append(all, e)
		}
		f.Close()
	}
	sort.Slice(all, func(i, j int) bool { return all[i].At.After(all[j].At) })
	if len(all) > occupancyLimit {
		all = all[:occupancyLimit]
	}
	d.Occupancy = append(d.Occupancy, all...)
}

// cronRunLine 是 ~/.openclaw/cron/runs/*.jsonl 的一行。
// ⚠ 只宣告我們真的會用的欄位。上游還有 delivery / diagnostics / summary 等，
// 那些是 L2 素材，不屬於占用帳本。
type cronRunLine struct {
	TS         int64  `json:"ts"`
	JobID      string `json:"jobId"`
	Status     string `json:"status"`
	Provider   string `json:"provider"`
	Model      string `json:"model"`
	SessionKey string `json:"sessionKey"`
	Error      string `json:"error"`
	Usage      struct {
		TotalTokens int `json:"total_tokens"`
	} `json:"usage"`
}

// readCronJobsFromJSON 讀 split layout 的 jobs.json 與 jobs-state.json。
//
// ⚠ 兩個檔案要分開量：jobs.json 讀得到就能回答總數與啟用數，不必等狀態檔。
// 狀態檔讀不到時不能把排程時間寫成 0，也不能讓整份觀測失敗。
// 這裡只宣告 id、enabled、state 裡的排程時間；自由文字欄位不屬於這份觀測。
func readCronJobsFromJSON(dir string, d *model.OpenClawDB) {
	b, err := os.ReadFile(filepath.Join(dir, "jobs.json"))
	if err != nil {
		return
	}
	var jobsDoc struct {
		Jobs json.RawMessage `json:"jobs"`
	}
	if err := json.Unmarshal(b, &jobsDoc); err != nil || !jsonHasPrefix(jobsDoc.Jobs, '[') {
		return
	}
	var jobs []struct {
		ID      string `json:"id"`
		Enabled *bool  `json:"enabled"`
	}
	if err := json.Unmarshal(jobsDoc.Jobs, &jobs); err != nil {
		return
	}

	enabledIDs := make([]string, 0, len(jobs))
	d.CronJobsTotal = len(jobs)
	d.CronJobsTotalMeasured = true
	for _, job := range jobs {
		if job.Enabled == nil || !*job.Enabled {
			continue
		}
		d.CronJobsEnabled++
		enabledIDs = append(enabledIDs, job.ID)
	}
	d.CronJobsEnabledMeasured = true

	b, err = os.ReadFile(filepath.Join(dir, "jobs-state.json"))
	if err != nil {
		return
	}
	var stateDoc struct {
		Jobs json.RawMessage `json:"jobs"`
	}
	if err := json.Unmarshal(b, &stateDoc); err != nil || !jsonHasPrefix(stateDoc.Jobs, '{') {
		return
	}
	var states map[string]struct {
		State *struct {
			NextRunAtMs *int64 `json:"nextRunAtMs"`
			LastRunAtMs *int64 `json:"lastRunAtMs"`
		} `json:"state"`
	}
	if err := json.Unmarshal(stateDoc.Jobs, &states); err != nil {
		return
	}

	// 上次執行是過去的機器紀錄，停用後也不會失效，所以要看所有工作。
	for _, jobState := range states {
		if jobState.State == nil || jobState.State.LastRunAtMs == nil {
			continue
		}
		last := *jobState.State.LastRunAtMs
		if d.LastCronRunAt == nil || last > d.LastCronRunAt.UnixMilli() {
			d.LastCronRunAt = msToTime(last)
		}
	}

	now := time.Now().UnixMilli()
	for _, id := range enabledIDs {
		jobState, ok := states[id]
		if !ok || jobState.State == nil || jobState.State.NextRunAtMs == nil {
			continue
		}
		next := *jobState.State.NextRunAtMs
		if d.NextCronRunAt == nil || next < d.NextCronRunAt.UnixMilli() {
			d.NextCronRunAt = msToTime(next)
		}
		if next < now {
			d.CronJobsOverdue++
		}
	}
	d.CronJobsScheduleMeasured = true
}

func jsonHasPrefix(raw json.RawMessage, prefix byte) bool {
	s := strings.TrimSpace(string(raw))
	return len(s) > 0 && s[0] == prefix
}

// agentFromSessionKey 從 "agent:<名字>:cron:<job>:run:<id>" 切出 <名字>。
//
// ⚠ 切不出來就回空字串，不猜。這個值會變成帳本上「是誰在用這張票」，
// 而一個猜錯的占用者，比一個空的占用者難查得多。
func agentFromSessionKey(k string) string {
	parts := strings.Split(k, ":")
	if len(parts) >= 2 && parts[0] == "agent" && parts[1] != "" {
		return parts[1]
	}
	return ""
}

// columnSet 回報一張表有哪些欄位。
// ⚠ 表不存在時 PRAGMA 回空集合而不是錯誤，呼叫端要自己判斷。
func columnSet(ctx context.Context, db *sql.DB, table string) (map[string]bool, error) {
	rows, err := db.QueryContext(ctx, "PRAGMA table_info("+table+")")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notNull, &dflt, &pk); err != nil {
			return nil, err
		}
		out[name] = true
	}
	return out, rows.Err()
}

func tableSet(ctx context.Context, db *sql.DB) (map[string]bool, error) {
	rows, err := db.QueryContext(ctx, "SELECT name FROM sqlite_master WHERE type='table'")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	tables := make(map[string]bool)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		tables[name] = true
	}
	return tables, rows.Err()
}

func readTaskRuns(ctx context.Context, db *sql.DB, d *model.OpenClawDB) error {
	var maxEnd sql.NullInt64
	var n int
	if err := db.QueryRowContext(ctx,
		"SELECT MAX(ended_at), COUNT(*) FROM task_runs").Scan(&maxEnd, &n); err != nil {
		return err
	}
	d.TaskRunRows = n
	if maxEnd.Valid {
		d.LastTaskEndedAt = msToTime(maxEnd.Int64) // ended_at 是 epoch 毫秒
	}
	counts, err := statusCounts(ctx, db, "SELECT status, COUNT(*) FROM task_runs GROUP BY status")
	if err != nil {
		return err
	}
	d.TaskStatusCount = counts

	// ⚠ terminal_outcome 實測全機隊都是 NULL，上游沒在寫。收它是為了在上游
	// 開始寫的那天能發現 —— 那是 L2 自動化唯一的希望。
	if err := db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM task_runs WHERE terminal_outcome IS NOT NULL").
		Scan(&d.TerminalOutcomePopulated); err != nil {
		return err
	}
	return nil
}

func readCronLogs(ctx context.Context, db *sql.DB, d *model.OpenClawDB) error {
	var maxTS sql.NullInt64
	var n int
	if err := db.QueryRowContext(ctx,
		"SELECT MAX(ts), COUNT(*) FROM cron_run_logs").Scan(&maxTS, &n); err != nil {
		return err
	}
	d.CronRunLogRows = n
	if maxTS.Valid {
		d.LastCronRunAt = msToTime(maxTS.Int64)
	}

	cutoff := time.Now().Add(-cronCountsWindow).UnixMilli()
	counts, err := statusCounts(ctx, db,
		"SELECT status, COUNT(*) FROM cron_run_logs WHERE ts > ? GROUP BY status", cutoff)
	if err != nil {
		return err
	}
	d.CronStatusCount = counts

	// L2 的原始素材。⚠ 絕對不要在 Agent 或 Hub 解析這些文字來推斷成敗 ——
	// 那是「掃 log 關鍵字」的變形，docs/PRODUCT.md 的地基二否決過。
	rows, err := db.QueryContext(ctx,
		"SELECT job_id, ts, status, summary FROM cron_run_logs ORDER BY ts DESC LIMIT ?",
		recentSummaries)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var jobID, status sql.NullString
		var ts sql.NullInt64
		var summary sql.NullString
		if err := rows.Scan(&jobID, &ts, &status, &summary); err != nil {
			return err
		}
		rs := model.RunSummary{
			JobID:   jobID.String,
			Status:  status.String, // ⚠ "ok" 不代表成功
			Summary: truncRunes(summary.String, maxSummaryRunes),
		}
		if ts.Valid {
			rs.At = *msToTime(ts.Int64)
		}
		d.RecentSummaries = append(d.RecentSummaries, rs)
	}
	return rows.Err()
}

// readCronJobs 只讀 cron_jobs 的結構化欄位，不碰 summary、error、job_json 等自由文字。
//
// ⚠ 一定先查欄位。cron_jobs 的 schema 會隨 OpenClaw 版本改變，少一格只能
// 少收那一格，不能讓其他已經量到的事實一起消失。
func readCronJobs(ctx context.Context, db *sql.DB, d *model.OpenClawDB) error {
	cols, err := columnSet(ctx, db, "cron_jobs")
	if err != nil {
		return err
	}
	if len(cols) == 0 {
		return nil
	}

	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM cron_jobs").Scan(&d.CronJobsTotal); err != nil {
		return err
	}
	d.CronJobsTotalMeasured = true
	if !cols["enabled"] {
		return nil
	}
	if err := db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM cron_jobs WHERE enabled = 1").Scan(&d.CronJobsEnabled); err != nil {
		return err
	}
	d.CronJobsEnabledMeasured = true
	if !cols["next_run_at_ms"] {
		return nil
	}

	var next sql.NullInt64
	if err := db.QueryRowContext(ctx, `
SELECT MIN(CASE WHEN enabled = 1 THEN next_run_at_ms END),
       COUNT(CASE WHEN enabled = 1 AND next_run_at_ms < ? THEN 1 END)
  FROM cron_jobs`, time.Now().UnixMilli()).Scan(&next, &d.CronJobsOverdue); err != nil {
		return err
	}
	d.CronJobsScheduleMeasured = true
	if next.Valid {
		d.NextCronRunAt = msToTime(next.Int64)
	}
	return nil
}

func statusCounts(ctx context.Context, db *sql.DB, query string, args ...any) (map[string]int, error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	counts := make(map[string]int)
	for rows.Next() {
		var status sql.NullString
		var n int
		if err := rows.Scan(&status, &n); err != nil {
			return nil, err
		}
		key := status.String
		if !status.Valid {
			key = "null"
		}
		counts[key] = n
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(counts) == 0 {
		return nil, nil
	}
	return counts, nil
}

// crashBundleCount 數崩潰包。比 is-active 有意義得多：一個一直被 Restart=always
// 撿起來的 process 永遠是 active，但它每崩一次就在這裡留一個檔案。
func crashBundleCount(root string) int {
	entries, err := os.ReadDir(filepath.Join(root, "logs", "stability"))
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			n++
		}
	}
	return n
}

// ---------------------------------------------------------------- credentials

// credentials 離線算出每家 CLI 的登入過期時間。
//
// 只輸出 {過期時間, 最後更新時間, 檔案 mtime} 三元組。⚠ 絕不輸出 token 本身。
// ⚠ 「被伺服器端踢掉」在本機偵測不到（檔案裡沒有這個欄位），要靠 Hub 跨機器
// 比對 mtime。所以這裡的 configured 只代表「檔案裡的時間還沒到」。
func credentials(home string, now time.Time) []model.Credential {
	// ⚠ 走 providers 那張表，不要在這裡再列一次。兩份手寫的清單一定會漂開，
	// 而漂開的結果是某一家只被檢查了一半 —— 理由寫在 provider 的註解裡。
	out := make([]model.Credential, 0, len(providers))
	for _, p := range providers {
		if p.Cred == nil {
			continue
		}
		out = append(out, p.Cred(home, now))
	}
	// ⚠ 一次蓋在整批上，因為這一整批的來源只有一個：讀本機上的檔案。
	// 這一行是在畫面上寫下「我沒有真的拿這張票去打過任何請求」——
	// 沒有它，一個 configured 會被讀成「登入正常」，而那兩件事差很遠：
	// 被伺服器端踢掉的 session，它的檔案跟一張好票長得一模一樣。
	for i := range out {
		out[i].VerificationMethod = model.VerifyFileParse
	}
	return out
}

// classifyCred 把過期時間變成三態：expired / configured / unknown。
//
// ⚠ 它刻意「不會」回傳 expires_soon。這條規則是實測改出來的，不要改回去。
//
// 原本的寫法是「剩餘 < 7 天就 expires_soon」。在真機上跑一次就發現那會讓
// 每一台機器永遠都是黃燈：
//
//	claude  access token 名目壽命 8 小時   → 永遠在「7 天內到期」
//	grok    access token 名目壽命 6 小時   → 同上
//	codex   access token 名目壽命 10 天    → 大部分時間也在門檻內
//
// 因為這些是**會自動續期**的 access token。它的 expires_at 講的是
// 「客戶端下次會在什麼時候默默換一張」，不是「人要重新登入了」。
// 拿它當預警，等於每天對人喊十次狼來了 —— 然後人就把通知關掉，
// 而這個產品最想避免的就是那件事（docs/PHASES.md 失敗模式 D1）。
//
// 真正有意義的訊號只有一個，而且它是可靠的：**expires_at 已經過去**。
// 那代表續期迴圈停掉了，需要人。實測支持這點 —— 機隊上 4 台的 claude
// 分別過期了 137 / 68 / 13 天與剛過期，全都是真的要處理的。
//
// 那 expires_soon 呢？它需要「這個檔案已經好幾輪沒有被更新了」這種
// 跨時間的觀察，而 agent 只看得到當下這一張快照。所以它由 Hub 從
// {expires_at, last_refresh, file_mtime} 的歷史推導 —— 判斷放中控台，
// 不放 agent，跟「被踢掉」的偵測是同一個道理。
func classifyCred(now time.Time, expiresAt *time.Time) model.CredStatus {
	if expiresAt == nil {
		return model.CredUnknown
	}
	if !expiresAt.After(now) {
		return model.CredExpired
	}
	return model.CredConfigured
}

func newCred(provider, path string, expiresAt *time.Time, note string, now time.Time) model.Credential {
	return model.Credential{
		Provider:  provider,
		Status:    classifyCred(now, expiresAt),
		ExpiresAt: expiresAt,
		FileMTime: fileMTime(path),
		Note:      note,
	}
}

// credUnreadable：檔案不在 = absent（沒裝這家），讀不動 = unknown + 原因。
// ⚠ 兩者必須分開。把「讀不到」講成「沒裝」會讓一台權限壞掉的機器看起來很乾淨。
func credUnreadable(provider string, err error) model.Credential {
	if errors.Is(err, fs.ErrNotExist) {
		// ⚠ LastError 刻意留空：「檔案不在」不是一個錯誤，它就是答案本身。
		// 把它寫進 LastError，畫面上每一台沒裝 gemini 的機器都會多一行紅字。
		return model.Credential{Provider: provider, Status: model.CredAbsent}
	}
	return model.Credential{Provider: provider, Status: model.CredUnknown,
		Note: reasonFor(err), LastError: err.Error()}
}

// credUnparseable：檔案讀得到、但看不懂。
//
// ⚠ Note 帶的是歸類過的詞（permission_denied / bad_json），LastError 帶**原文**。
// 兩個都要：歸類的那個可以拿來比對與統計，原文那個才講得出是「哪一個檔案」
// 的「哪一個位元組」出問題。只留歸類的那個，畫面上就只剩一句
// 「permission_denied」，而人接下來要做的第一件事正好是「denied 在哪個路徑」。
func credUnparseable(provider, path string, err error) model.Credential {
	return model.Credential{
		Provider:  provider,
		Status:    model.CredUnknown,
		Note:      reasonFor(err),
		LastError: err.Error(),
		FileMTime: fileMTime(path),
	}
}

func claudeCred(home string, now time.Time) model.Credential {
	path := filepath.Join(home, ".claude", ".credentials.json")
	b, err := os.ReadFile(path)
	if err != nil {
		return credUnreadable("claude", err)
	}
	var doc struct {
		OAuth struct {
			ExpiresAt json.RawMessage `json:"expiresAt"` // epoch 毫秒
		} `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return credUnparseable("claude", path, err)
	}
	exp := flexTime(doc.OAuth.ExpiresAt)
	note := ""
	if exp == nil {
		note = "no claudeAiOauth.expiresAt in .credentials.json"
	}
	return newCred("claude", path, exp, note, now)
}

func codexCred(home string, now time.Time) model.Credential {
	path := filepath.Join(home, ".codex", "auth.json")
	b, err := os.ReadFile(path)
	if err != nil {
		return credUnreadable("codex", err)
	}
	var doc struct {
		Tokens struct {
			AccessToken string `json:"access_token"`
		} `json:"tokens"`
		LastRefresh string `json:"last_refresh"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return credUnparseable("codex", path, err)
	}

	// ⚠⚠ 這裡只能用 access_token 的 exp。
	// tokens 裡還有一個 id_token，它壽命只有 1 小時、實測平常就已經過期
	// （量測當下 id_token 已過期 10 天而 access_token 還有 2 小時）。
	// 拿 id_token 判斷會讓整個機隊每一台都亮紅燈。不要「順手清理」成 id_token。
	var exp *time.Time
	note := "exp from access_token (NOT id_token)"
	if doc.Tokens.AccessToken != "" {
		exp = jwtExp(doc.Tokens.AccessToken)
	}
	if exp == nil {
		note = "no usable exp in tokens.access_token (id_token is NOT a substitute)"
	}
	c := newCred("codex", path, exp, note, now)
	c.LastRefresh = flexTimeString(doc.LastRefresh)

	// ⚠ BAT 會熱抽換 ~/.codex/auth.json，只讀 auth.json 不知道現在是哪個帳號在用。
	if b, err := os.ReadFile(filepath.Join(home, ".codex", "codex-accounts.json")); err == nil {
		var acct struct {
			ActiveAccountID string                     `json:"activeAccountId"`
			Accounts        map[string]json.RawMessage `json:"accounts"`
		}
		if json.Unmarshal(b, &acct) == nil {
			c.ActiveAccountID = acct.ActiveAccountID
			c.AccountCount = len(acct.Accounts)
		} else {
			// accounts 也可能是陣列
			var alt struct {
				ActiveAccountID string            `json:"activeAccountId"`
				Accounts        []json.RawMessage `json:"accounts"`
			}
			if json.Unmarshal(b, &alt) == nil {
				c.ActiveAccountID = alt.ActiveAccountID
				c.AccountCount = len(alt.Accounts)
			}
		}
	}
	return c
}

func grokCred(home string, now time.Time) model.Credential {
	path := filepath.Join(home, ".grok", "auth.json")
	b, err := os.ReadFile(path)
	if err != nil {
		return credUnreadable("grok", err)
	}
	exp, err := grokExpiry(b)
	if err != nil {
		return credUnparseable("grok", path, err)
	}
	note := ""
	if exp == nil {
		note = "no expires_at under any " + grokKeyPrefix + "::* key"
	}
	return newCred("grok", path, exp, note, now)
}

// grokExpiry 掃出最晚的一個 expires_at。
//
// ⚠ 頂層 key 是動態的 "https://auth.x.ai::<uuid>"，uuid 每次登入都不一樣，
// 寫死 key 等於這個欄位永遠是 unknown。只能用前綴掃。
//
// ⚠ 值的型別實測是 RFC3339 字串（"2026-09-03T04:18:35.306566984Z"），
// 但別家（gemini）用 epoch 數字，所以兩種都吃。Python 原型只吃數字，
// 因此 samplehub1 的 grok 一直被誤報成 unknown。
func grokExpiry(raw []byte) (*time.Time, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return nil, err
	}
	// key 排序後再掃，讓多個 session 存在時的結果是可重現的。
	keys := make([]string, 0, len(top))
	for k := range top {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var best *time.Time
	for _, k := range keys {
		if !strings.HasPrefix(k, grokKeyPrefix) {
			continue
		}
		// 值可能是單一物件，也可能是物件陣列。
		var items []map[string]json.RawMessage
		if err := json.Unmarshal(top[k], &items); err != nil {
			var one map[string]json.RawMessage
			if err := json.Unmarshal(top[k], &one); err != nil {
				continue
			}
			items = []map[string]json.RawMessage{one}
		}
		for _, item := range items {
			if t := flexTime(item["expires_at"]); t != nil && (best == nil || t.After(*best)) {
				best = t
			}
		}
	}
	return best, nil
}

func geminiCred(home string, now time.Time) model.Credential {
	path := filepath.Join(home, ".gemini", "oauth_creds.json")
	b, err := os.ReadFile(path)
	if err != nil {
		return credUnreadable("gemini", err)
	}
	var doc struct {
		Token struct {
			Expiry json.RawMessage `json:"expiry"`
		} `json:"token"`
		ExpiryDate json.RawMessage `json:"expiry_date"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return credUnparseable("gemini", path, err)
	}
	exp := flexTime(doc.Token.Expiry)
	if exp == nil {
		exp = flexTime(doc.ExpiryDate) // 舊版 gemini CLI 用頂層 expiry_date（毫秒）
	}
	note := ""
	if exp == nil {
		note = "no token.expiry nor expiry_date in oauth_creds.json"
	}
	return newCred("gemini", path, exp, note, now)
}

// openclawCred：⚠ 實測 openclaw 的設定裡沒有任何過期欄位。誠實回報 unknown，
// 不要因為「有裝就當它是好的」而填 configured —— 把 unknown 當綠燈是這個專案
// 最想避免的事。
func openclawCred(home string) model.Credential {
	if fi, err := os.Stat(filepath.Join(home, ".openclaw")); err != nil || !fi.IsDir() {
		return model.Credential{Provider: "openclaw", Status: model.CredAbsent}
	}
	return model.Credential{
		Provider: "openclaw",
		Status:   model.CredUnknown,
		Note:     "no expiry field exists anywhere in openclaw config",
	}
}

// jwtExp 取 JWT 的 exp，不驗簽章（我們要的是過期時間，不是要信任這張票）。
func jwtExp(token string) *time.Time {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return nil
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return nil
	}
	var claims struct {
		Exp json.RawMessage `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil
	}
	return flexTime(claims.Exp)
}

// ---------------------------------------------------------------- cli tools

// cliTools 收版本座標。
//
// ⚠ --version 全部不可信：實測 grok 自報 1.0.3、它的 package.json 說 1.0.13。
// 所以多來源都收，矛盾時如實回報矛盾（SourcesDisagree），不挑一個當答案。
// ⚠ procs 由呼叫端掃好傳進來，這裡不自己掃。
// 掃兩次會拿到兩個時間點的機器狀態，然後「CLI 在跑但 BAT 不在」這種矛盾
// 會變成偶發的假訊號 —— 一份觀測必須是同一個瞬間的切片。
func cliTools(ctx context.Context, procs []procInfo, processScan string) []model.CLITool {
	// ⚠ 一輪觀測只問一次 PATH，然後每個工具都用**同一條**。
	// 理由跟 procs 只掃一次一樣：問兩次會拿到兩個環境，然後
	//「claude 在 A、openclaw 在 B」這種矛盾會變成偶發的假訊號。
	sp := cachedLoginSearchPath(ctx)
	tools := make([]model.CLITool, 0, len(watchedTools))
	for _, name := range watchedTools {
		tools = append(tools, cliTool(ctx, name, procs, processScan, sp))
	}
	return tools
}

// searchPath 是「人打這個指令的時候，shell 會去哪裡找」。
//
// ⚠⚠ 這個型別存在的唯一理由，是 2026-09-03 在 samplehub1 量到的一件事：
// clawctl-agent 是 systemd --user 起來的，它的 PATH 是
//
//	/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin:...
//
// 而 operator 的登入 shell 前面還有 ~/.cargo/bin 跟 ~/.local/bin。於是同一個
// 指令名字在兩邊解到不同的檔案，六個工具裡有三個中：
//
//	claude    人 ~/.local/share/claude/versions/2.1.258
//	          我 /usr/local/lib/node_modules/.../claude.exe  → 2.1.205
//	openclaw  人 2026.6.6 (8c802aa)   我 2026.6.1 (2e08f0f)
//	grok      兩邊不同檔案，版號剛好一樣 —— 巧合，不是正確
//	agy       人有，我完全找不到 → 畫面說「沒有安裝」
//
// 拿自己的環境去量，再把答案講成關於這台機器的事實，就是 §5.15
// 「看時間戳之前先問它是哪一個鐘的」同一個形狀，只是換成了 PATH。
type searchPath struct {
	Dirs   []string
	Source string // model.PathSourceLogin | model.PathSourceDaemon
	Reason string // 退回 daemon 時，說為什麼
}

// cachedLoginSearchPath 一個 process 只問一次登入 shell。
//
// ⚠⚠ 為什麼要快取：`bash -lc` 會把使用者**整份 login profile 跑一遍**，
// 那是有副作用的。實測 samplehub1 的 agent log 裡出現
//
//	im-config[2146555]: @ IM_CONFIG_ENTRY='profile' … PPID=2146475
//
// —— `PPID` 就是 agent 自己。Ubuntu 的 ~/.profile 會跑 im-config；
// 別人的 .profile 裡還會有 nvm、conda、pyenv 這種要跑好幾秒、
// 而且會寫檔案的東西。觀測平均每 9 分鐘一輪（實測 samplehub1 540 秒），
// 一天就是 160 次別人的 login profile。**觀測不應該改變被觀測的機器。**
//
// PATH 是一個變得很慢的事實，agent 重啟就會重新量 —— 拿一次夠了。
// ⚠ 代價要講清楚：operator 改了 ~/.profile 之後要重啟 agent 才看得到。
// 這個代價是知情的，寫在這裡就是為了讓下一個人不用重新發現它。
var loginPathOnce struct {
	sync.Once
	sp searchPath
}

func cachedLoginSearchPath(ctx context.Context) searchPath {
	loginPathOnce.Do(func() { loginPathOnce.sp = loginSearchPath(ctx) })
	return loginPathOnce.sp
}

// loginSearchPath 去問登入 shell 它的 PATH 是什麼。問不到就退回自己的，
// 並且把原因記下來 —— 一個沒有說明的降級，跟一個假裝沒發生的降級一樣糟。
func loginSearchPath(ctx context.Context) searchPath {
	fallback := func(reason string) searchPath {
		return searchPath{
			Dirs:   filepath.SplitList(os.Getenv("PATH")),
			Source: model.PathSourceDaemon,
			Reason: reason,
		}
	}
	shell := firstNonEmpty(os.Getenv("SHELL"), "/bin/bash")
	// ⚠ 用 printf 而不是 echo：echo 在某些 shell 會吃掉反斜線。
	// -l 是關鍵 —— ~/.profile 那段「把 ~/.local/bin 加進 PATH」只有
	// login shell 會跑到，而那正是整個 bug 的所在。
	out, stderr, err := run(ctx, loginShellTimeout, shell, "-lc", `printf %s "$PATH"`)
	if err != nil {
		return fallback(fmt.Sprintf("問不到登入 shell（%s）的 PATH：%s",
			shell, truncRunes(firstNonEmpty(stderr, errString(err)), maxToolRaw)))
	}
	// ⚠ login shell 會印 motd、rc 檔裡也常有人自己 echo 東西。
	// PATH 是最後一行，因為 printf 是最後一個指令。
	line := lastLine(out)
	dirs := filepath.SplitList(line)
	if !looksLikeSearchPath(dirs) {
		return fallback("登入 shell 回的東西不像一條 PATH，不敢用：" + truncRunes(line, maxToolRaw))
	}
	return searchPath{Dirs: dirs, Source: model.PathSourceLogin}
}

// looksLikeSearchPath 確認拿到的真的是一條 PATH，而不是 motd 的最後一行。
//
// ⚠ 這不違反地基二（不准 parse free text 推論成敗）：這裡不是在讀
// 「成功了嗎」，是在檢查一個**值的形狀**。形狀不對就不用它，
// 而且原文會被留在 PathReason 裡讓人自己看。
func looksLikeSearchPath(dirs []string) bool {
	real := 0
	for _, d := range dirs {
		if d == "" {
			continue // PATH 裡的空項合法（意思是「目前目錄」），跳過不判死
		}
		if !filepath.IsAbs(d) {
			return false
		}
		if st, err := os.Stat(d); err == nil && st.IsDir() {
			real++
		}
	}
	// 一條真的 PATH 至少會有兩個存在的目錄。實測 samplehub1 的登入 PATH 有 11 項。
	return real >= 2
}

// lookPathIn 是 exec.LookPath，但用給定的目錄清單，不是這個 process 的 PATH。
//
// ⚠⚠ 不可以改 os.Setenv("PATH") 再呼叫 exec.LookPath —— 那會動到整個
// process 的全域狀態，而 probe 的各段是併發跑的，等於在別人腳下換地板。
func lookPathIn(dirs []string, name string) (string, bool) {
	if strings.ContainsRune(name, filepath.Separator) {
		return "", false // 帶路徑的名字不是要查 PATH，不要假裝查了
	}
	for _, dir := range dirs {
		if dir == "" {
			continue
		}
		cand := filepath.Join(dir, name)
		// ⚠ os.Stat 會跟著 symlink 走，這是要的：指向執行檔的 symlink
		// 本身就是可執行的，而這些工具幾乎都是 symlink。
		st, err := os.Stat(cand)
		if err != nil || st.IsDir() || st.Mode().Perm()&0o111 == 0 {
			continue
		}
		return cand, true
	}
	return "", false
}

// daemonReach 回答「clawctl 自己伸手去拿這個名字，會拿到人打的那一個嗎」。
//
// ⚠ 只在人的 PATH 上找得到的時候才問 —— 沒有人那一邊，就沒有東西可以比，
// 而「沒有東西可以比」必須留白，不可以填一個看起來像答案的值。
func daemonReach(sp searchPath, name, humanReal string) (reach, path string) {
	if sp.Source != model.PathSourceLogin || humanReal == "" {
		return "", ""
	}
	found, ok := lookPathIn(filepath.SplitList(os.Getenv("PATH")), name)
	if !ok {
		return model.DaemonReachMissing, ""
	}
	real := found
	if r, err := filepath.EvalSymlinks(found); err == nil {
		real = r
	}
	if real == humanReal {
		return model.DaemonReachSame, found
	}
	return model.DaemonReachShadowed, real
}

// packageDirFor 從 realpath 反推 npm 套件目錄，找不到回空字串。
//
// ⚠ 不可以在 realpath 後面補 "/" 再比對：那會讓可選的第二段吃掉檔名本身，
// .../node_modules/openclaw/openclaw.mjs 就會被當成套件目錄，
// 於是第二來源永遠讀不到 package.json。原型沒有補那個斜線是對的。
func packageDirFor(realPath string) string {
	if m := nodeModulesRe.FindStringSubmatch(realPath); len(m) == 2 {
		return m[1]
	}
	return ""
}

func cliTool(ctx context.Context, name string, procs []procInfo, processScan string, sp searchPath) model.CLITool {
	t := model.CLITool{Name: name, ProcessScan: processScan}
	t.PathSource, t.PathReason = sp.Source, sp.Reason

	// ⚠⚠ 這裡**不可以**用 exec.LookPath —— 那查的是 clawctl-agent 自己的 PATH，
	// 而 agent 是 systemd --user 起來的，裡面沒有 ~/.local/bin。查錯了 PATH，
	// 就是量了一台「只有 daemon 看得到」的假機器（見 searchPath 的註解）。
	//
	// ⚠⚠ 也不可以在找不到的時候就 return。那是同一天早一點修掉的 bug：
	// procs 就在手上、裡面就有答案，而這個函式看都不看一眼就宣告「沒有安裝」。
	// 實測 samplehub1 上 `agy` 這個 process 從 10:59 跑到現在，
	// 而畫面對全機隊四台都說它沒裝。§5.10 的形狀：
	// 讀了唯一看不到那個東西的來源，然後把「我沒看到」寫成「它不存在」。
	if path, ok := lookPathIn(sp.Dirs, name); ok {
		t.OnPath = true
		t.Present = true
		t.PresentEvidence = "path"
		t.Path = path
		t.RealPath = path
		if real, err := filepath.EvalSymlinks(path); err == nil {
			t.RealPath = real
		}
	}

	// 人打到的那一個，跟 clawctl 自己叫得到的那一個，是不是同一個檔案。
	// 這不影響上面的答案，但它決定「以後 clawctl 真的去跑它」會跑到哪一個。
	t.DaemonReach, t.DaemonPath = daemonReach(sp, name, t.RealPath)

	// process 的比對放在版本檢查**之前**，因為它會決定 Present ——
	// 而後面那些檢查（--version、package.json）只有在 Present 時才有意義。
	//
	// ⚠ realPath 是空的時候 matchProcess 會退回用 comm/argv 比名字，
	// 那正是抓到 agy 的那條路。
	proc, script, running := matchProcess(procs, name, t.RealPath)
	if running {
		t.RunningPID = proc.pid
		t.RunningExe = proc.exe
		// ⚠ node CLI 的 exe 是 /usr/bin/node，它對「跑的是哪個版本」
		// 一點資訊都沒有。真正的答案在 script 這個路徑上。
		t.RunningScript = script
		if !t.Present {
			// ⚠ 它在跑，但 PATH 上找不到它。這不是「沒裝」——
			// 是「裝了，而且正在用，只是叫不動」。這兩句話的下一步差很遠。
			t.Present = true
			t.PresentEvidence = "process"
			t.RealPath = firstNonEmpty(proc.exe, script)
		}
	} else if processScan == model.ProcessScanUnavailable {
		t.RunningReason = "讀不到 /proc，這台的 process 偵測整個是關的"
	} else if processScan == model.ProcessScanRestricted {
		// ⚠ 一台活著的 Linux 不會只有幾個 process。掃得到的太少，
		// 代表看得見的東西被擋掉了 —— 那是關於我自己的事實，不是關於世界的。
		t.RunningReason = fmt.Sprintf("只掃得到 %d 個 process，這台的 /proc 視野被限制了", len(procs))
	} else if processScan == model.ProcessScanComplete {
		t.RunningReason = fmt.Sprintf("掃了 %d 個 process，沒有一個對得上", len(procs))
	}

	if !t.Present {
		// PATH 上沒有、也沒有在跑。就我們看得到的範圍，它真的不在。
		return t
	}

	// ⚠ 不在 PATH 上就不要跑 `name --version` —— 那一定會失敗，
	// 而那個失敗訊息會蓋掉「它其實在跑」這個更重要的事實。
	if !t.OnPath {
		t.VersionReason = "不在 PATH 上，叫不動它，所以問不到版本。" +
			"它是靠正在跑的 process 才被看到的（pid " +
			strconv.Itoa(t.RunningPID) + "）"
		return t
	}

	// openclaw --version 慢，其他家不慢，但統一給寬額度比較不會誤判成「壞掉」。
	//
	// ⚠⚠ 第一個參數是 t.Path，**不可以**是 name。
	// 用名字就是把剛剛解好的答案丟掉、交給 exec 拿 daemon 的 PATH 再解一次 ——
	// 於是「我報告的那個路徑」跟「我實際問到版號的那個檔案」是兩個不同的檔案。
	// 實測 2026-09-03：畫面寫 claude 2.1.205，operator 打 claude 跑的是 2.1.258。
	// 那一行沒有錯誤、沒有警告、沒有紅燈 —— 只有一個很像真的錯數字。
	//
	// ⚠⚠ 而且要用 runIn 把**人的 PATH** 交給子行程，不可以用 run。
	// 解對了檔案還不夠 —— 在錯的環境裡跑它，答案一樣是錯的。見 runIn。
	out, stderr, err := runIn(ctx, versionTimeout, sp.Dirs, t.Path, "--version")
	switch {
	case err == nil && out != "":
		t.VersionReported = versionRe.FindString(out)
		t.VersionRaw = truncRunes(lastLine(out), maxToolRaw)
		if t.VersionReported == "" {
			// ⚠ 這裡就是「碰到未知 CLI 版本」的樣子：它回答了，而我看不懂。
			// Path / RealPath / VersionRaw 三個一個都不能丟 ——
			// 那三個是人接下來唯一能用的東西，而我手上明明有。
			t.Support = model.SupportUnsupported
			t.VersionReason = "認不得 --version 的輸出格式，原文留在 version_raw"
		} else {
			t.Support = model.SupportOK
		}
	default:
		t.VersionReason = truncRunes(firstNonEmpty(stderr, errString(err), "empty output"), maxToolRaw)
	}

	// 第二來源：npm 裝的 package.json。
	if dir := packageDirFor(t.RealPath); dir != "" {
		var pkg struct {
			Version string `json:"version"`
		}
		if b, err := os.ReadFile(filepath.Join(dir, "package.json")); err == nil {
			if json.Unmarshal(b, &pkg) == nil {
				t.VersionPackageJSON = pkg.Version
			}
		}
	}
	t.SourcesDisagree = sourcesDisagree(t.VersionReported, t.VersionPackageJSON)

	// ⚠ 「裝的跟正在跑的可能不是同一個 binary」那件事（實測 codex 裝在
	// /usr/local/bin，跑的是 openclaw 自己 npm 進來的 vendor 版）現在在
	// 上面的 matchProcess 那一段就做完了 —— 它必須在版本檢查之前跑，
	// 因為它會決定 Present。
	return t
}

// sourcesDisagree：兩個非空來源給了不同答案就是矛盾。只有一個來源不算矛盾 ——
// 「只有一個人講話」跟「兩個人講不同的話」是不一樣的事。
func sourcesDisagree(versions ...string) bool {
	seen := make(map[string]bool)
	for _, v := range versions {
		if v != "" {
			seen[v] = true
		}
	}
	return len(seen) > 1
}

type procInfo struct {
	pid  int
	comm string
	exe  string

	// argv 是完整的命令列。
	//
	// ⚠ 少了它，這一整組欄位在真機上是空的。我們盯的每一支 CLI
	// （openclaw / claude / codex / gemini / grok）都是 node script，
	// 所以它們的 exe 一律是 /usr/bin/node、comm 一律是 "node"。
	// 用 exe 或 comm 去比對，永遠一個都對不上 —— 而「對不上」在畫面上
	// 長得跟「沒有在跑」一模一樣。實測 2026-09-03：全機隊 running_pid
	// 100% 是 NULL，而 samplehub1 上的 openclaw gateway 正在 pid 3191 跑著。
	argv []string
}

// scanProcesses 掃一次 /proc，不 spawn ps。
//
// ⚠⚠ 讀不到 exe 的 process **不可以略過**。這一行原本是 `continue`，
// 而它讓整個 process 偵測在真機上等於沒有裝。
//
// 原因是 /proc/<pid>/exe 這個 symlink 要 PTRACE_MODE_READ 才讀得到，
// 而 Ubuntu 預設 yama/ptrace_scope=1 —— 一個 process 只讀得到**自己的子孫**。
// systemd service 沒有子孫，所以：
//
//	從我的 shell 跑    → 550 個 pid，77 個讀得到 exe
//	從 agent 的 unit 跑 → 553 個 pid，**0 個**讀得到 exe
//
// 兩者跑的是同一份程式碼。單元測試全綠，因為測試餵的是已經填好 exe 的假資料。
//
// /proc/<pid>/cmdline 與 /proc/<pid>/comm 則**不需要 ptrace**，任何人都讀得到。
// 所以真正可靠的識別來源是 cmdline，exe 只是「拿得到就順便帶上」的附加證據。
//
// ⚠ comm 也靠不住：node 會把主執行緒改名，實測 openclaw gateway 的 comm
// 是 "MainThread"。cmdline 是這裡唯一能信的東西。
func scanProcesses() ([]procInfo, string) {
	return scanProcessesIn("/proc")
}

func scanProcessesIn(root string) ([]procInfo, string) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, model.ProcessScanUnavailable
	}
	self := os.Getpid()
	procs := make([]procInfo, 0, 64)
	restricted := false
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == self {
			continue
		}
		procDir := filepath.Join(root, e.Name())
		// ⚠ cmdline 讀成功但為空的是 kernel thread；列舉後消失的 pid 也只代表時間差。
		// 這兩種都略過。只有權限失敗代表視野真的擋住了，必須降級，
		// 不能宣稱掃過所有 process。
		argv, err := readArgv(filepath.Join(procDir, "cmdline"))
		if err != nil {
			if errors.Is(err, fs.ErrPermission) {
				restricted = true
			}
			continue
		}
		if len(argv) == 0 {
			continue
		}
		// exe 讀得到就帶上，讀不到就空著 —— 空著是常態，不是錯誤。
		exe, _ := os.Readlink(filepath.Join(procDir, "exe"))
		procs = append(procs, procInfo{
			pid:  pid,
			comm: readTrimmed(filepath.Join(procDir, "comm")),
			exe:  exe,
			argv: argv,
		})
	}
	sort.Slice(procs, func(i, j int) bool { return procs[i].pid < procs[j].pid })
	if restricted || len(procs) < minPlausibleProcs {
		return procs, model.ProcessScanRestricted
	}
	return procs, model.ProcessScanComplete
}

// matchProcess 找一個「疑似這個工具」的 process。盡力而為，找不到就空著。
//
// 優先順序：exe 正好等於安裝路徑 > comm 相符 > exe 檔名相符。
// 先挑完全相符的，是為了在「多數 process 跑的是裝的那個」時不要報一個
// 邊緣的 vendor binary 當代表；真的沒有相符的才回報不一致的那個。
func matchProcess(procs []procInfo, name, realPath string) (procInfo, string, bool) {
	var byComm, byBase, byScript *procInfo
	var script string
	for i := range procs {
		p := &procs[i]
		if realPath != "" && p.exe == realPath {
			return *p, "", true
		}
		// ⚠ exe 常常是空的（ptrace_scope=1，見 scanProcesses）。
		// argv[0] 拿得到，而對非 node 的工具它就是安裝路徑本身。
		if realPath != "" && len(p.argv) > 0 && p.argv[0] == realPath {
			return *p, "", true
		}
		if byComm == nil && p.comm == name {
			byComm = p
		}
		if byBase == nil && p.exe != "" && filepath.Base(p.exe) == name {
			byBase = p
		}
		if byBase == nil && len(p.argv) > 0 && strings.Contains(p.argv[0], "/") &&
			filepath.Base(p.argv[0]) == name {
			byBase = p
		}
		if byScript == nil {
			if sc := scriptArgFor(p.argv, name); sc != "" {
				byScript, script = p, sc
			}
		}
	}
	switch {
	case byComm != nil:
		return *byComm, "", true
	case byBase != nil:
		return *byBase, "", true
	case byScript != nil:
		return *byScript, script, true
	}
	return procInfo{}, "", false
}

// scriptArgFor 在命令列裡找「這支工具的進入點」，找不到回空字串。
//
// 認兩種形狀，兩種都是實機上量到的：
//
//	.../node_modules/<name>/dist/index.js   ← 套件目錄，最可靠
//	.../node_modules/.bin/<name>            ← npm 產生的 shim
//
// ⚠ 刻意**不做**「argv 裡含有 name 這個字」。samplehub1 上那樣會抓到三個錯的：
//
//	/bin/bash ~/.openclaw/workspace/baseline/watcher.sh      （目錄剛好叫 .openclaw）
//	python -m hermes_cli.main --profile openclaw-evolution   （profile 名字裡有）
//	node .../node_modules/@openclaw/codex/.../codex          （是 codex，不是 openclaw）
//
// 抓錯一個 process 比抓不到更糟：它會讓「跑的是哪個版本」這一格出現一個
// 看起來很具體、而且是錯的答案。
func scriptArgFor(argv []string, name string) string {
	for _, a := range argv {
		if !strings.Contains(a, "/") {
			continue // 子指令、旗標，不是路徑
		}
		if strings.Contains(a, "/node_modules/"+name+"/") {
			return a
		}
		if strings.Contains(a, "/node_modules/.bin/") &&
			filepath.Base(a) == name {
			return a
		}
	}
	return ""
}

// readArgv 讀 /proc/<pid>/cmdline。它是 NUL 分隔的，不是空白分隔的 ——
// 用空白切會把帶空格的路徑切成兩半。
func readArgv(path string) ([]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	// ⚠ 不能直接 Split。strings.Split("", sep) 回的是 [""] 而不是 []，
	// 於是每一個 kernel thread（cmdline 是空的）都會帶著一個空字串混進來，
	// 把 process 總數灌到四倍 —— 而那個總數正是「視野有沒有被擋住」的判斷依據。
	trimmed := strings.TrimRight(string(b), "\x00")
	if trimmed == "" {
		return nil, nil
	}
	parts := strings.Split(trimmed, "\x00")
	if len(parts) > maxArgv {
		parts = parts[:maxArgv]
	}
	return parts, nil
}

func findTool(tools []model.CLITool, name string) *model.CLITool {
	for i := range tools {
		if tools[i].Name == name {
			return &tools[i]
		}
	}
	return nil
}

// ---------------------------------------------------------------- helpers

// run 跑一個外部指令並回 (stdout, stderr, err)。永遠有 timeout。
func run(ctx context.Context, timeout time.Duration, name string, args ...string) (string, string, error) {
	return runIn(ctx, timeout, nil, name, args...)
}

// runIn 跟 run 一樣，但把子行程的 PATH 換成 dirs。dirs 是空的就用自己的 PATH。
// A single "--version" argument is served from the process-lifetime cache in
// version_cache.go when the executable identity is unchanged. A miss still
// follows the PATH rules below.
//
// ⚠⚠ 這是 2026-09-03 那個 bug 的**第三層**，也是最容易被漏掉的一層：
// 解對了檔案還不夠 —— 在錯的環境裡跑那個檔案，答案一樣是錯的。
//
// 實測 sampleagent3：/home/example-user-c/.local/bin/openclaw 這個 wrapper 的第一行是
// `#!/usr/bin/env node`，所以它跑哪一個 node **完全由呼叫者的 PATH 決定**：
//
//	登入的 PATH   → /home/example-user-c/.local/bin/node v24.15.0 → OpenClaw 2026.6.10（連問 8 次都成功）
//	daemon 的 PATH → /usr/bin/node v20.20.2          → "Node.js v22.19+ is required"（連問 6 次都失敗）
//
// 同一個檔案、同一台機器、同一個瞬間，兩個環境給出兩個相反的答案 ——
// 一個說「裝著全機隊最新的版本」，一個說「它根本起不來」。
// 而在資料庫裡，這件事長成一個會在兩個值之間跳動的欄位。
//
// ⚠ 除了 systemd notification/watchdog 控制變數，其他環境變數照傳。
// HOME 一定要留著 ——
// 這些工具的憑證跟設定都住在 $HOME，把它清掉會得到一台「什麼都沒裝」的假機器
// （ops/ansible/bootstrap.yml 那句 `become: false` 的註解講的是同一件事）。
func runIn(ctx context.Context, timeout time.Duration, dirs []string, name string, args ...string) (string, string, error) {
	cacheKey, cacheable := versionProbeKey(name, dirs, args)
	if cacheable {
		if cachedOut, cachedErr, ok := versionProbeLoad(cacheKey); ok {
			return cachedOut, cachedErr, nil
		}
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := processenv.CommandContext(cctx, name, args...)
	env := cmd.Env
	if len(dirs) > 0 {
		pathEnv := make([]string, 0, len(env)+1)
		for _, kv := range env {
			if !strings.HasPrefix(kv, "PATH=") {
				pathEnv = append(pathEnv, kv)
			}
		}
		env = append(pathEnv, "PATH="+strings.Join(dirs, string(filepath.ListSeparator)))
	}
	cmd.Env = env
	// ⚠ WaitDelay：process 被 kill 之後若還有子孫抓著 stdout，Wait 會永遠不返回。
	// 這是 Go 特有的坑，Python 的 subprocess.run(timeout=) 沒有。
	cmd.WaitDelay = waitDelay
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	outText := strings.TrimSpace(stdout.String())
	errText := strings.TrimSpace(stderr.String())
	if cacheable && err == nil && outText != "" {
		if again, ok := versionProbeKey(name, dirs, args); ok && again == cacheKey {
			versionProbeStore(cacheKey, outText, errText)
		}
	}
	return outText, errText, err
}

func readTrimmed(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func fileMTime(path string) *time.Time {
	fi, err := os.Stat(path)
	if err != nil {
		return nil
	}
	t := fi.ModTime().UTC()
	return &t
}

// reasonFor 把 error 變成一句人看得懂、而且不含檔案內容的原因。
func reasonFor(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, fs.ErrNotExist):
		return "not_found"
	case errors.Is(err, fs.ErrPermission):
		return "permission_denied"
	}
	var syn *json.SyntaxError
	var ute *json.UnmarshalTypeError
	if errors.As(err, &syn) || errors.As(err, &ute) {
		return "bad_json: " + err.Error()
	}
	return "io_error: " + err.Error()
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// flexTime 解析「可能是 RFC3339 字串、可能是 epoch 秒、也可能是 epoch 毫秒」的
// 時間欄位 —— 五家 CLI 五種寫法，這是它們的最小公倍數。
func flexTime(raw json.RawMessage) *time.Time {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return nil
	}
	if strings.HasPrefix(s, `"`) {
		var str string
		if err := json.Unmarshal(raw, &str); err != nil {
			return nil
		}
		return flexTimeString(str)
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return nil
	}
	return epochToTime(f)
}

func flexTimeString(s string) *time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	// time.RFC3339 的 Parse 也吃得下小數秒，所以 "…:35.306566984Z" 一併涵蓋。
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		utc := t.UTC()
		return &utc
	}
	// 少數工具寫成沒有時區的本地時間。
	if t, err := time.ParseInLocation("2006-01-02T15:04:05", s, time.Local); err == nil {
		utc := t.UTC()
		return &utc
	}
	// 也可能整個是一串數字的字串。
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return epochToTime(f)
	}
	return nil
}

// epochToTime：> 1e11 當毫秒，否則當秒。
// 1e11 秒是西元 5138 年，1e11 毫秒是 1973 年 —— 這個門檻在人類尺度上不會誤判。
func epochToTime(v float64) *time.Time {
	if v <= 0 {
		return nil
	}
	var t time.Time
	if v > 1e11 {
		t = time.UnixMilli(int64(v)).UTC()
	} else {
		sec, frac := int64(v), v-float64(int64(v))
		t = time.Unix(sec, int64(frac*float64(time.Second))).UTC()
	}
	return &t
}

func msToTime(ms int64) *time.Time {
	t := time.UnixMilli(ms).UTC()
	return &t
}

// truncRunes 按字元截斷，不按 byte —— 按 byte 會把中文/emoji 切成半個字，
// 而 summary 裡實測有中文。
func truncRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

func atoiSafe(s string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0
	}
	return n
}
