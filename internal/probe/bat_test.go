package probe

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/teddashh/AI-Intune/internal/model"
)

// argv 全部照實機抄的。⚠ 四台的安裝路徑四個都不一樣，
// 所以只能認檔名 —— 這組資料存在就是為了讓「認路徑」的寫法測不過。
var fleetArgv = map[string][]string{
	"samplehub1": {"/home/example-user/.local/share/bat-server-v3.2.5/bat-server",
		"--bind=tailscale", "--port=9876",
		"--data-dir=/home/example-user/.local/share/org.tonyq.better-agent-terminal"},
	"sampleagent2": {"/home/ubuntu/bat-upgrades/v3.2.5/extract/bat-server-linux-aarch64/bat-server",
		"--port=9876", "--bind=localhost",
		"--data-dir=/home/ubuntu/.local/share/org.tonyq.better-agent-terminal"},
	"sampleagent3": {"/opt/bat-server/bat-server", "--bind=localhost", "--port=9876",
		"--data-dir=/root/.bat-server"},
	"sampleagent4": {"/home/example-user-b/Applications/bat-server-current/bat-server",
		"--port=9876", "--bind=tailscale",
		"--data-dir=/home/example-user-b/.local/share/org.tonyq.better-agent-terminal"},
}

// TestReadsRealFleetArgv：四台實機的命令列，四個安裝路徑，兩種參數順序。
//
// ⚠ 沒有一台在 8080。這個測試的存在就是為了讓那個寫死的猜測再也回不來。
func TestReadsRealFleetArgv(t *testing.T) {
	for name, argv := range fleetArgv {
		t.Run(name, func(t *testing.T) {
			// ⚠ exe 刻意留空：在 agent 的 unit 裡它永遠是空的（§5.10 的 ptrace）。
			// 一份填好 exe 的假資料會讓錯的實作測得過。
			procs := []procInfo{
				{pid: 1, comm: "systemd", argv: []string{"/sbin/init"}},
				{pid: 42, comm: "MainThread", argv: argv},
			}
			b := batServer(procs, model.ProcessScanRestricted, "/home/bat-test")
			if !b.Running {
				t.Fatalf("沒認出 bat-server，argv=%v", argv)
			}
			if b.Port != 9876 {
				t.Errorf("port = %d，實機四台都是 9876", b.Port)
			}
			wantBind := "tailscale"
			if name == "sampleagent2" || name == "sampleagent3" {
				wantBind = "localhost"
			}
			if b.Bind != wantBind {
				t.Errorf("bind = %q，想要 %q", b.Bind, wantBind)
			}
			if !strings.Contains(b.Argv, "--port=9876") {
				t.Errorf("argv 原文沒留下來：%q", b.Argv)
			}
		})
	}
}

func TestBATMatchIsEvidenceRegardlessOfProcessScan(t *testing.T) {
	procs := []procInfo{{pid: 42, argv: []string{"/opt/bat-server/bat-server", "--bind=tailscale", "--port=9876"}}}
	for _, processScan := range []string{
		model.ProcessScanUnavailable,
		model.ProcessScanRestricted,
		model.ProcessScanComplete,
		"",
	} {
		t.Run(firstNonEmpty(processScan, "empty"), func(t *testing.T) {
			b := batServer(procs, processScan, "/home/bat-test")
			if !b.Running || b.Port != 9876 || b.Bind != "tailscale" {
				t.Fatalf("processScan=%q 改變了已命中的 BAT 證據：%+v", processScan, b)
			}
			if !strings.Contains(b.Argv, "--port=9876") {
				t.Errorf("processScan=%q 沒有保留 argv：%q", processScan, b.Argv)
			}
		})
	}
}

// TestBATAbsenceIsNotSilence 守的是這個專案的核心：
// **「我沒看到」跟「它不存在」是兩件事，而空白兩者都像。**
func TestBATAbsenceIsNotSilence(t *testing.T) {
	t.Run("讀不到 /proc", func(t *testing.T) {
		b := batServer(nil, model.ProcessScanUnavailable, "/home/bat-test")
		if b.Running || !strings.Contains(b.Reason, "/proc") {
			t.Errorf("掃不到 process 時要說是偵測關掉了，實際：%+v", b)
		}
	})
	t.Run("讀得到但 0 個 process 是 restricted", func(t *testing.T) {
		b := batServer([]procInfo{}, model.ProcessScanRestricted, "/home/bat-test")
		if b.Running {
			t.Fatal("空的 process 表不該認成 bat-server")
		}
		want := "只掃得到 0 個 process，這台的 /proc 視野被限制了，查不出 bat-server 是否在跑"
		if b.Reason != want {
			t.Errorf("restricted Reason = %q，想要 %q", b.Reason, want)
		}
		if strings.Contains(b.Reason, "讀不到 /proc") || strings.Contains(b.Reason, "沒有一個是") {
			t.Errorf("restricted 不能說 /proc 讀不到或宣告缺席，實際：%q", b.Reason)
		}
	})
	t.Run("restricted 掃描不宣告 bat-server 缺席", func(t *testing.T) {
		b := batServer([]procInfo{{pid: 1, argv: []string{"/sbin/init"}}}, model.ProcessScanRestricted, "/home/bat-test")
		if b.Running {
			t.Fatal("認錯了 process")
		}
		want := "只掃得到 1 個 process，這台的 /proc 視野被限制了，查不出 bat-server 是否在跑"
		if b.Reason != want {
			t.Errorf("restricted Reason = %q，想要 %q", b.Reason, want)
		}
		if strings.Contains(b.Reason, "沒有一個是") {
			t.Errorf("restricted 不能宣告 bat-server 缺席，實際：%q", b.Reason)
		}
	})
	t.Run("complete 掃描才可以宣告沒有 bat-server", func(t *testing.T) {
		b := batServer([]procInfo{{pid: 1, argv: []string{"/sbin/init"}}}, model.ProcessScanComplete, "/home/bat-test")
		if b.Running {
			t.Fatal("認錯了 process")
		}
		if !strings.Contains(b.Reason, "沒有一個是") {
			t.Errorf("要說清楚是掃過了才沒有，實際：%q", b.Reason)
		}
	})
	t.Run("未知掃描狀態 fail closed", func(t *testing.T) {
		for _, processScan := range []string{"", "future-value"} {
			b := batServer([]procInfo{{pid: 1, argv: []string{"/sbin/init"}}}, processScan, "/home/bat-test")
			if b.Running || !strings.Contains(b.Reason, "查不出 bat-server 是否在跑") {
				t.Errorf("processScan=%q 沒有 fail closed：%+v", processScan, b)
			}
			if strings.Contains(b.Reason, "沒有一個是") {
				t.Errorf("processScan=%q 不能掉進 complete：%q", processScan, b.Reason)
			}
		}
	})
	t.Run("認不得 port 就不要猜一個", func(t *testing.T) {
		b := batServer([]procInfo{{pid: 9, argv: []string{"/opt/bat-server/bat-server", "--config=/etc/bat.toml"}}}, model.ProcessScanRestricted, "/home/bat-test")
		if b.Port != 0 {
			t.Errorf("認不得卻填了 port %d —— 上一個填出來的預設值就是四台全錯的 8080", b.Port)
		}
		if !strings.Contains(b.Reason, "--port") {
			t.Errorf("沒說出是哪裡認不得：%q", b.Reason)
		}
	})
}

// TestFlagFormsBothWork：`--port=9876` 與 `--port 9876` 都要讀得懂。
func TestFlagFormsBothWork(t *testing.T) {
	for _, argv := range [][]string{
		{"bat-server", "--port=9876", "--bind=tailscale"},
		{"bat-server", "--port", "9876", "--bind", "tailscale"},
	} {
		b := batServer([]procInfo{{pid: 3, argv: argv}}, model.ProcessScanRestricted, "/home/bat-test")
		if b.Port != 9876 || b.Bind != "tailscale" {
			t.Errorf("argv=%v → port=%d bind=%q", argv, b.Port, b.Bind)
		}
	}
}

// TestBATRestrictedReasonCrossPackageBinding 把探針真正產生的 BAT 喂給 model，
// 防止 Connect 畫面的 why 測試只驗到手填 fixture。
func TestBATRestrictedReasonCrossPackageBinding(t *testing.T) {
	b := batServer([]procInfo{{pid: 1, argv: []string{"/sbin/init"}}}, model.ProcessScanRestricted, "/home/bat-test")
	url, why := model.ConnectURL(b, "100.64.0.1")
	if url != "" {
		t.Fatalf("restricted 掃描不該給連線位址：%q", url)
	}
	if why != b.Reason {
		t.Fatalf("Connect why = %q，想要 batServer Reason %q", why, b.Reason)
	}
	if strings.Contains(why, "沒有一個是") {
		t.Fatalf("restricted Connect why 不能宣告 bat-server 缺席：%q", why)
	}
}

// TestParseProcNetTCP 用真的 /proc/net/tcp 格式。
//
// ⚠ 位址是**主機位元組序**印出來的，每 4 bytes 要反轉。
// 少了那一步不會噴錯，只會安靜地產出一個看起來很像 IP 的錯位址 ——
// 而那個錯位址會被貼到畫面上叫人連過去。
func TestParseProcNetTCP(t *testing.T) {
	// Synthetic /proc/net/tcp rows with distinct listener and connection states.
	// 9876 = 0x2694（第一版這裡寫 0x26AC，那是 9900 —— 一個自己編的假 port
	// 讓測試紅了，而那正是假資料應該做的事）。
	// 0100007F = 127.0.0.1，02C84064 = 100.64.200.2。
	// st: 0A=LISTEN、01=ESTABLISHED、06=TIME_WAIT ——
	// 實機上同一個 port 三種都有，所以篩 LISTEN 這件事不是防禦性程式碼。
	const body = `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
  23: 02C84064:2694 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 3127506 1 0000000000000000 100 0 0 10 0
  24: 0100007F:2694 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 3127507 1 0000000000000000 100 0 0 10 0
  30: 00000000:225F 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 3127508 1 0000000000000000 100 0 0 10 0
  40: 02C84064:2694 02C84064:B7E4 01 00000000:00000000 00:00000000 00000000  1000        0 10315016 1 0000000000000000 20 4 2 10 -1
  44: 02C84064:85B0 02C84064:2694 06 00000000:00000000 03:00000BD5 00000000     0        0 0 3 0000000000000000
`
	dir := t.TempDir()
	path := filepath.Join(dir, "tcp")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := parseProcNetTCP(path, 9876)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"127.0.0.1": true, "100.64.200.2": true}
	if len(got) != 2 {
		t.Fatalf("拿到 %v，想要兩筆（127.0.0.1 與 100.64.200.2）", got)
	}
	for _, a := range got {
		if !want[a] {
			t.Errorf("解出了 %q —— 位元組序沒有反轉的話就會長這樣", a)
		}
	}
	// 0xB7E4 / 0x85B0 是那兩列 ESTABLISHED / TIME_WAIT 的 port。
	// ⚠ 它們也在同一份檔案裡，而且 local_address 長得一模一樣 ——
	// 不篩 st 的話，一個「有人連過去過」的痕跡會被讀成「有人在聽」。
	for _, p := range []int{0xB7E4, 0x85B0} {
		got, err := parseProcNetTCP(path, p)
		if err != nil {
			t.Fatal(err)
		}
		if n := len(got); n != 0 {
			t.Errorf("port %d：把非 LISTEN 的 socket 也算進來了（%d 筆）", p, n)
		}
	}
}

func TestListeningOnMeasurement(t *testing.T) {
	const (
		noListen = `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
  0: 0100007F:2694 00000000:0000 01 00000000:00000000 00:00000000 00000000  1000        0 1
`
		withListen = `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
  0: 0100007F:2694 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 1
`
		oldReason = "process 在跑（--port=9876），但核心的 socket 表上沒有任何東西在聽這個 port"
		newReason = "這台的核心 socket 表量測沒有跑到底（--port=9876），查不出這個 port 上有沒有東西在聽"
	)

	writeTable := func(t *testing.T, path, body string, mode os.FileMode) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
	}
	batAt := func(paths []string) model.BAT {
		return batServerWithSocketPaths([]procInfo{{
			pid:  42,
			argv: []string{"/opt/bat-server/bat-server", "--port=9876", "--bind=tailscale"},
		}}, model.ProcessScanComplete, paths, "/home/bat-test")
	}

	t.Run("兩張表讀成功且沒有 LISTEN", func(t *testing.T) {
		dir := t.TempDir()
		tcp, tcp6 := filepath.Join(dir, "tcp"), filepath.Join(dir, "tcp6")
		writeTable(t, tcp, noListen, 0o644)
		writeTable(t, tcp6, noListen, 0o644)

		addrs, measured := listeningOnPaths([]string{tcp, tcp6}, 9876)
		if !measured || len(addrs) != 0 {
			t.Fatalf("addrs=%v measured=%t，想要空位址且已量到", addrs, measured)
		}
		if got := batAt([]string{tcp, tcp6}).Reason; got != oldReason {
			t.Errorf("Reason=%q，想要 %q", got, oldReason)
		}
	})

	t.Run("主表 EACCES 而 tcp6 讀成功", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root 會忽略 mode 0000")
		}
		dir := t.TempDir()
		tcp, tcp6 := filepath.Join(dir, "tcp"), filepath.Join(dir, "tcp6")
		writeTable(t, tcp, noListen, 0o000)
		writeTable(t, tcp6, noListen, 0o644)

		addrs, measured := listeningOnPaths([]string{tcp, tcp6}, 9876)
		if measured || len(addrs) != 0 {
			t.Fatalf("addrs=%v measured=%t，想要空位址且沒量到", addrs, measured)
		}
		got := batAt([]string{tcp, tcp6}).Reason
		if got != newReason {
			t.Errorf("Reason=%q，想要 %q", got, newReason)
		}
		if strings.Contains(got, "沒有任何東西在聽") {
			t.Errorf("沒量到卻宣稱沒人在聽：%q", got)
		}
	})

	t.Run("tcp6 ENOENT 而主表讀成功", func(t *testing.T) {
		dir := t.TempDir()
		tcp, tcp6 := filepath.Join(dir, "tcp"), filepath.Join(dir, "tcp6")
		writeTable(t, tcp, noListen, 0o644)
		addrs, measured := listeningOnPaths([]string{tcp, tcp6}, 9876)
		if !measured || len(addrs) != 0 {
			t.Fatalf("addrs=%v measured=%t，想要空位址且已量到", addrs, measured)
		}
	})

	t.Run("兩張表都 ENOENT", func(t *testing.T) {
		dir := t.TempDir()
		addrs, measured := listeningOnPaths([]string{filepath.Join(dir, "tcp"), filepath.Join(dir, "tcp6")}, 9876)
		if measured || len(addrs) != 0 {
			t.Fatalf("addrs=%v measured=%t，想要空位址且沒量到", addrs, measured)
		}
	})

	t.Run("主表讀成功而 tcp6 EACCES", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root 會忽略 mode 0000")
		}
		dir := t.TempDir()
		tcp, tcp6 := filepath.Join(dir, "tcp"), filepath.Join(dir, "tcp6")
		writeTable(t, tcp, noListen, 0o644)
		writeTable(t, tcp6, noListen, 0o000)

		addrs, measured := listeningOnPaths([]string{tcp, tcp6}, 9876)
		if measured || len(addrs) != 0 {
			t.Fatalf("addrs=%v measured=%t，想要空位址且沒量到", addrs, measured)
		}
		got := batAt([]string{tcp, tcp6}).Reason
		if got != newReason {
			t.Errorf("Reason=%q，想要 %q", got, newReason)
		}
		if strings.Contains(got, "沒有任何東西在聽") {
			t.Errorf("沒量到卻宣稱沒人在聽：%q", got)
		}
	})

	t.Run("找到 LISTEN 時 measured 不影響位址", func(t *testing.T) {
		dir := t.TempDir()
		tcp, missing := filepath.Join(dir, "tcp"), filepath.Join(dir, "tcp6")
		writeTable(t, tcp, withListen, 0o644)

		addrs, measured := listeningOnPaths([]string{tcp, missing}, 9876)
		if !measured || len(addrs) != 1 || addrs[0] != "127.0.0.1" {
			t.Fatalf("addrs=%v measured=%t，想要 127.0.0.1 且已量到", addrs, measured)
		}

		blocked := filepath.Join(dir, "blocked")
		if err := os.Mkdir(blocked, 0o755); err != nil {
			t.Fatal(err)
		}
		addrs, measured = listeningOnPaths([]string{tcp, blocked}, 9876)
		if len(addrs) != 1 || addrs[0] != "127.0.0.1" {
			t.Fatalf("addrs=%v measured=%t，量測狀態不該移除已找到的 127.0.0.1", addrs, measured)
		}
	})
}

// TestConnectURLMatchesTheFleet 把四台實機的形狀走一遍。
//
// ⚠ 這裡最重要的兩列是 sampleagent2 / sampleagent3：它們必須回傳**空字串**加一個理由。
// 一個永遠給得出位址的 Connect 面板，是在對機隊五分之二說謊。
func TestConnectURLMatchesTheFleet(t *testing.T) {
	cases := []struct {
		name, tsIP, wantURL, wantWhy string
		bat                          model.BAT
	}{
		{name: "samplehub1：綁 tailscale，量得到位址",
			tsIP:    "100.64.200.2",
			bat:     model.BAT{Running: true, Port: 9876, Bind: "tailscale", ListenAddrs: []string{"100.64.200.2"}},
			wantURL: "https://100.64.200.2:9876"},

		{name: "sampleagent2：綁 localhost，不准給位址",
			tsIP:    "100.64.200.6",
			bat:     model.BAT{Running: true, Port: 9876, Bind: "localhost", ListenAddrs: []string{"127.0.0.1"}},
			wantWhy: "連不進來"},

		{name: "綁在所有介面，用它自己報的 tailnet 位址",
			tsIP:    "100.64.200.5",
			bat:     model.BAT{Running: true, Port: 9876, Bind: "all", ListenAddrs: []string{"0.0.0.0"}},
			wantURL: "https://100.64.200.5:9876"},

		{name: "process 在但沒綁成功",
			tsIP: "100.64.0.1",
			bat: model.BAT{Running: true, Port: 9876, Bind: "tailscale",
				Reason: "process 在跑（--port=9876），但核心的 socket 表上沒有任何東西在聽這個 port"},
			wantWhy: "沒有任何東西在聽"},

		{name: "沒跑 BAT",
			tsIP:    "100.64.0.1",
			bat:     model.BAT{Reason: "掃了 312 個 process，沒有一個是 bat-server"},
			wantWhy: "沒有一個是 bat-server"},

		{name: "IPv6 也要包中括號",
			tsIP:    "100.64.0.1",
			bat:     model.BAT{Running: true, Port: 9876, Bind: "all", ListenAddrs: []string{"fd7a:115c:a1e0::1"}},
			wantURL: "https://[fd7a:115c:a1e0::1]:9876"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			url, why := model.ConnectURL(c.bat, c.tsIP)
			if url != c.wantURL {
				t.Errorf("url = %q，想要 %q", url, c.wantURL)
			}
			if c.wantWhy != "" && !strings.Contains(why, c.wantWhy) {
				t.Errorf("理由 %q 裡沒有 %q", why, c.wantWhy)
			}
			// ⚠ 這一條比上面兩條都重要：沒有位址就一定要有理由。
			if url == "" && why == "" {
				t.Error("沒有位址、也沒有理由 —— 那是一格看起來像「這裡不重要」的留白")
			}
		})
	}
}

func TestFindBATSkipsAIIntuneProcess(t *testing.T) {
	home := "/home/bat-test"
	mydata := model.BATServerDataDir(home)

	ownerEq := procInfo{pid: 2, argv: []string{"/opt/bat-server/bat-server", "--bind=tailscale", "--port=9876", "--data-dir=/owner/data"}}
	ownerSpace := procInfo{pid: 3, argv: []string{"/opt/bat-server/bat-server", "--bind", "tailscale", "--port", "9876", "--data-dir", "/owner/data"}}

	aiEq := procInfo{pid: 4, argv: []string{"bat-server", "--bind=localhost", "--port=19876", "--data-dir=" + mydata}}
	aiSpace := procInfo{pid: 5, argv: []string{"bat-server", "--bind", "localhost", "--port", "19876", "--data-dir", mydata}}

	for _, ai := range []procInfo{aiEq, aiSpace} {
		for _, owner := range []procInfo{ownerEq, ownerSpace} {
			got, ok := findBAT([]procInfo{ai, owner}, home)
			if !ok || got.pid != owner.pid {
				t.Fatalf("expected owner pid %d, got ok=%v, p=%v", owner.pid, ok, got)
			}
		}

		res := batServer([]procInfo{ai}, model.ProcessScanComplete, home)
		if res.Reason == "" || res.Reason != "掃了 1 個 process，沒有一個是 bat-server" {
			t.Fatalf("expected no bat-server for AI process, got %+v", res)
		}
	}
}
