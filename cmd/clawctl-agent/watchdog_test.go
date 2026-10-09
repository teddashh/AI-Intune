package main

import (
	"bytes"
	"context"
	"os"
	"regexp"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

func TestAgentUnitMakesManagedRuntimeStateWritable(t *testing.T) {
	raw, err := os.ReadFile("../../ops/clawctl-agent.service")
	if err != nil {
		t.Fatal(err)
	}
	want := []byte("ReadWritePaths=@@CLAWCTL_AGENT_HOME@@/.config/clawctl @@CLAWCTL_AGENT_HOME@@/.config/systemd/user/openclaw-gateway.service.d @@CLAWCTL_AGENT_HOME@@/.cache/clawctl @@CLAWCTL_AGENT_HOME@@/.local/share/clawctl @@CLAWCTL_AGENT_HOME@@/.openclaw")
	if !bytes.Contains(raw, want) {
		t.Fatalf("agent unit missing exact managed runtime write path: %s", want)
	}
}

func TestAgentUnitIsRenderedAsAnUnprivilegedSystemService(t *testing.T) {
	raw, err := os.ReadFile("../../ops/clawctl-agent.service")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range [][]byte{
		[]byte("User=@@CLAWCTL_AGENT_USER@@"),
		[]byte("ExecStart=@@CLAWCTL_AGENT_BIN@@"),
		[]byte("Environment=HOME=@@CLAWCTL_AGENT_HOME@@"),
		[]byte("Environment=XDG_RUNTIME_DIR=/run/user/@@CLAWCTL_AGENT_UID@@"),
		[]byte("ProtectSystem=strict"),
		[]byte("ProtectHome=read-only"),
		[]byte("WantedBy=multi-user.target"),
	} {
		if !bytes.Contains(raw, want) {
			t.Fatalf("agent system unit template missing contract: %s", want)
		}
	}
	if bytes.Contains(raw, []byte("WantedBy=default.target")) {
		t.Fatal("agent must not be installed as a user-manager service")
	}
}

func TestManagedPrimaryRuntimeUnitsAreMutuallyExclusiveAndPinnedToManagedCoordinates(t *testing.T) {
	openClaw, err := os.ReadFile("../../ops/openclaw-gateway.service")
	if err != nil {
		t.Fatal(err)
	}
	hermes, err := os.ReadFile("../../ops/clawctl-hermes.service")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range [][]byte{
		[]byte("Conflicts=clawctl-hermes.service"),
		[]byte("ExecStart=%h/.local/share/clawctl/node-runtime/current/bin/node %h/.local/share/clawctl/openclaw/current/lib/node_modules/openclaw/dist/index.js gateway --port 18789"),
		[]byte("ReadWritePaths=%h/.openclaw %h/.local/share/clawctl/openclaw"),
	} {
		if !bytes.Contains(openClaw, want) {
			t.Fatalf("OpenClaw unit missing contract: %s", want)
		}
	}
	if bytes.Contains(openClaw, []byte("--allow-unconfigured")) {
		t.Fatal("managed OpenClaw unit must persist a complete local gateway configuration")
	}
	for _, want := range [][]byte{
		[]byte("Conflicts=openclaw-gateway.service"),
		[]byte("--pull=never"),
		[]byte("--publish=127.0.0.1:8642:8642"),
		[]byte("--volume=%h/.local/share/clawctl/hermes/data:/opt/data:Z"),
	} {
		if !bytes.Contains(hermes, want) {
			t.Fatalf("Hermes unit missing contract: %s", want)
		}
	}
}

// 2026-09-03 的事故。全機隊每 90 秒被 systemd SIGABRT 一次，從部署第一天開始，
// 而 Hub 一路顯示綠燈 —— 因為每次重啟都會立刻送一次 check-in，
// 也就是說「心跳」正是「它死了」的產物。
//
// unit 檔寫 WatchdogSec=90；程式碼把餵食掛在 check-in 上，週期 120s ±20%
// = 96–144 秒。兩邊分開看都是合理的數字。
//
// 這一組測試存在的理由就是：這個 bug 活在兩個檔案之間，
// 任何只看 Go 這一邊的測試都抓不到它。

// unitWatchdogSec 從真正要部署的那個 unit 檔裡讀 WatchdogSec。
// ⚠ 不准在測試裡自己寫一個 90 —— 那樣就變成測試複製了 bug，
// 兩邊一起錯還會一起綠。
func unitWatchdogSec(t *testing.T) time.Duration {
	t.Helper()
	b, err := os.ReadFile("../../ops/clawctl-agent.service")
	if err != nil {
		t.Fatalf("讀不到 unit 檔（agent 的看門狗設定就住在那裡）：%v", err)
	}
	m := regexp.MustCompile(`(?m)^WatchdogSec=(\d+)`).FindSubmatch(b)
	if m == nil {
		t.Fatal("unit 檔裡找不到 WatchdogSec=，看門狗是不是被拿掉了？")
	}
	n, _ := strconv.Atoi(string(m[1]))
	return time.Duration(n) * time.Second
}

func TestWatchdogIsFedFasterThanSystemdKills(t *testing.T) {
	kill := unitWatchdogSec(t)
	feed := watchdogFeedInterval(kill)

	if feed <= 0 {
		t.Fatalf("餵食週期算出來是 %s，等於根本不餵", feed)
	}
	// 一次掉包就被打死太脆弱了：要求至少能連掉一次還活著。
	if feed*2 >= kill {
		t.Errorf("餵食週期 %s 對 WatchdogSec=%s 來說太慢 ——"+
			" 掉一次封包就會被 SIGABRT。要求 feed*2 < kill。", feed, kill)
	}
}

// TestWatchdogSurvivesTheLongestPossibleCheckin 是真正會變紅的那一個。
// 舊的實作只在 check-in 成功之後才餵一次；把最壞情況的 check-in 週期
// 拿來當餵食週期，它就會超過 WatchdogSec。
func TestWatchdogSurvivesTheLongestPossibleCheckin(t *testing.T) {
	kill := unitWatchdogSec(t)
	// check-in 的最壞情況：預設 2 分鐘、抖動 +20%。
	worstCheckin := time.Duration(float64(2*time.Minute) * 1.2)

	if worstCheckin < kill {
		t.Skipf("check-in 週期 %s 本來就短於 WatchdogSec %s，這個測試沒意義了", worstCheckin, kill)
	}
	feed := watchdogFeedInterval(kill)
	if feed >= kill {
		t.Fatalf("餵食週期 %s ≥ WatchdogSec %s：agent 一定會被打死", feed, kill)
	}
	// 明確記下這個關係，免得有人「順手」把餵食改回接在 check-in 上。
	if feed >= worstCheckin {
		t.Errorf("餵食週期 %s ≥ 最壞的 check-in 週期 %s ——"+
			" 這代表餵食又被綁回 check-in 的節奏上了。看門狗必須有自己的時鐘。", feed, worstCheckin)
	}
}

// TestHubBeingDownDoesNotGetTheAgentKilled ——
// 舊實作只有 postJSON 成功才 notifyWatchdog()。那等於把「Hub 活著」
// 接到「這台機器該不該被 kill」上面：Hub 停 90 秒，全機隊一起被 SIGABRT。
// unit 檔第一行就寫著「Hub 掛掉不是這台機器的錯」。
func TestHubBeingDownDoesNotGetTheAgentKilled(t *testing.T) {
	var pings atomic.Int64
	turn := &atomic.Int64{}
	turn.Store(time.Now().UnixNano()) // 心跳迴圈有在轉

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	// Hub 從頭到尾都不通：這裡完全沒有任何「成功」發生。
	go feedWatchdog(ctx, 20*time.Millisecond, time.Minute, turn,
		func() { pings.Add(1) })
	<-ctx.Done()

	if pings.Load() == 0 {
		t.Error("Hub 不通的整段時間裡一次都沒餵看門狗 —— systemd 會把這台打死，" +
			"但這台其實好好的，只是連不上 Hub")
	}
}

// TestWatchdogStopsWhenTheHeartbeatLoopWedges ——
// 反過來也要成立，否則這條看門狗就只是在自證自己活著。
// 「自證不算數」：餵食的憑據必須是心跳迴圈確實轉過，不是餵食那條 goroutine 還在。
func TestWatchdogStopsWhenTheHeartbeatLoopWedges(t *testing.T) {
	var pings atomic.Int64
	turn := &atomic.Int64{}
	turn.Store(time.Now().Add(-10 * time.Minute).UnixNano()) // 十分鐘沒轉了

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	go feedWatchdog(ctx, 20*time.Millisecond, time.Minute, turn,
		func() { pings.Add(1) })
	<-ctx.Done()

	if pings.Load() != 0 {
		t.Errorf("心跳迴圈卡死了還餵了 %d 次看門狗 —— 那這條看門狗什麼都守不住", pings.Load())
	}
}

// 沒有 systemd 的時候（手跑、測試、容器裡）不該有任何看門狗行為。
func TestNoSystemdMeansNoWatchdog(t *testing.T) {
	t.Setenv("WATCHDOG_USEC", "")
	if d := watchdogFromEnv(); d != 0 {
		t.Errorf("systemd 沒開看門狗時算出了 %s，應該是 0", d)
	}
	t.Setenv("WATCHDOG_USEC", "90000000")
	if d := watchdogFromEnv(); d != 90*time.Second {
		t.Errorf("WATCHDOG_USEC=90000000 應該是 90s，得到 %s", d)
	}
}

// Verifier unit 與 agent unit 是兩個 plane，測試放在一起只是因為它們都是
// 「檔案裡的一個字改掉、程式全綠、真機安靜地壞掉」這一類。
//
// ⚠ 這裡釘的每一條都是真機上撞到或量到的，不是想像出來的加固清單。
func TestVerifierUnitKeepsTheSecondPairOfEyesSeparateAndScheduled(t *testing.T) {
	service := unitDirectives(t, "../../ops/clawctl-verifier.service")
	timer := unitDirectives(t, "../../ops/clawctl-verifier.timer")

	// 憑證面：verifier 走自己的 bearer 檔，永遠不碰 agent.json。
	// 兩個 plane 共用一份憑證，第二個判斷就不再獨立。
	for _, want := range [][]byte{
		[]byte("%h/.local/bin/clawctl-verifier verifier"),
		[]byte("--token-file %h/.config/clawctl/verifier.token"),
		[]byte("--targets %h/.config/clawctl/verifier-targets.json"),
	} {
		if !bytes.Contains(service, want) {
			t.Errorf("verifier unit 少了：%s", want)
		}
	}
	if bytes.Contains(service, []byte("agent.json")) {
		t.Error("verifier unit 讀到了 agent 的設定 —— 那不是第二個判斷")
	}

	// 一趟一次。Type=oneshot 以外的任何值都表示它開始自己管生命週期了，
	// 而排程是 timer 的事。
	if !bytes.Contains(service, []byte("Type=oneshot")) {
		t.Error("verifier unit 不是 Type=oneshot")
	}

	// 2026-09-03 量過的坑：非特權的 systemd --user manager 動不了
	// capability bounding set，這三個指令隱含 capability 操作，
	// 加下去 unit 會 status=218/CAPABILITIES 起不來 —— 而單元測試全綠。
	for _, forbidden := range [][]byte{
		[]byte("PrivateDevices="), []byte("ProtectKernelTunables="),
		[]byte("ProtectKernelModules="), []byte("ProtectClock="),
	} {
		if bytes.Contains(service, forbidden) {
			t.Errorf("verifier unit 有 %s —— user manager 會 218/CAPABILITIES", forbidden)
		}
	}

	// 2026-09-12 裝上去當場撞到的：Persistent= 只對 calendar timer 有效。
	// 配 monotonic trigger 會被安靜地忽略，list-timers 的 NEXT 是空的，
	// 關機期間錯過的那一次不補跑，而派工會停在「等它回報」沒有人知道為什麼。
	if bytes.Contains(timer, []byte("Persistent=true")) && !bytes.Contains(timer, []byte("OnCalendar=")) {
		t.Error("timer 有 Persistent=true 卻沒有 OnCalendar —— Persistent 會被忽略，錯過的那次不補跑")
	}
	if !bytes.Contains(timer, []byte("Unit=clawctl-verifier.service")) {
		t.Error("timer 沒有指到 verifier service")
	}
}

// unitDirectives 只留下 systemd 真的會讀的那些行。這兩個 unit 的註解本身
// 就在講「不讀 agent.json」「不要加 PrivateDevices」，所以直接對整份檔案
// 做字串比對會把註解讀成設定 —— 一個說明自己為什麼安全的檔案，
// 會被那樣的測試judged成不安全。
func unitDirectives(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var kept [][]byte
	for _, line := range bytes.Split(raw, []byte("\n")) {
		if bytes.HasPrefix(bytes.TrimLeft(line, " \t"), []byte("#")) {
			continue
		}
		kept = append(kept, line)
	}
	return bytes.Join(kept, []byte("\n"))
}
