package probe

import (
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/teddashh/AI-Intune/internal/model"
)

// bat-server 的偵測。
//
// ⚠⚠ 這整個檔案是一個 bug 的墓碑。
//
// 詳細頁最底下的「連到這台的 BAT」原本寫的是 `https://{{TailscaleIP}}:8080`，
// 那個 8080 是**寫死在樣板裡的猜測**，而且從來沒有人點過。真的去量之後：
//
//	samplehub1  --bind=tailscale --port=9876  →  100.64.200.2:9876
//	sampleagent4   --bind=tailscale --port=9876  →  100.64.200.5:9876
//	sampleagent2  --bind=localhost --port=9876  →  127.0.0.1:9876   ← 從別台連不到
//	sampleagent3  --bind=localhost --port=9876  →  127.0.0.1:9876   ← 從別台連不到
//
// 四台沒有一台在 8080。而且有兩台**不管給什麼位址都連不上**，因為
// bat-server 根本沒有綁在對外的介面上 —— 那兩台正確的答案不是換個 port，
// 是「你從這裡連不過去，要先改 --bind」。
//
// 這是「自證不算數」的一個很乾淨的例子：那一行 HTML 在每一次 code review、
// 每一次單元測試裡都看起來完全正確，因為沒有任何東西去問過機器。
//
// ---------------------------------------------------------------- 兩層證據
//
// 這裡刻意收兩種**不同層級**的東西，而且不合併：
//
//	argv        它被要求要做什麼（--port / --bind）
//	socket 表   核心那邊真的發生了什麼（有沒有東西在那個 port 上 LISTEN）
//
// 一個 --bind=tailscale 但 tailscale 還沒起來的 bat-server，argv 完全正常，
// 而 socket 表是空的。把兩者混成一個「BAT 好不好」的布林值，就會把這種
// 情況洗成綠燈 —— 那正是這個專案存在要防的那件事。

// batProcName 是要找的 process 名字。⚠ 不含路徑：實測四台的安裝路徑
// 四個都不一樣（~/.local/share、/opt、~/bat-upgrades、~/Applications）。
const batProcName = "bat-server"

// batMaxArgv 是留在證據裡的 argv 原文長度上限。
const batMaxArgv = 300

var procNetTCPPaths = []string{"/proc/net/tcp", "/proc/net/tcp6"}

// batServer 從已經掃好的 process 表裡認出 bat-server，並回報怎麼連上去。
//
// ⚠ 吃 procs 而不是自己再掃一次 /proc：掃兩次會拿到兩個時間點的機隊狀態，
// 然後「process 在但 BAT 不在」這種矛盾會變成偶發的假訊號。
func batServer(procs []procInfo, processScan string) model.BAT {
	return batServerWithSocketPaths(procs, processScan, procNetTCPPaths)
}

func batServerWithSocketPaths(procs []procInfo, processScan string, socketPaths []string) model.BAT {
	p, ok := findBAT(procs)
	if !ok {
		// ⚠ 這裡不能回一個空的 BAT{} 就算了 —— 空的意思是「這台沒跑 BAT」，
		// 而真相可能是「我沒有看到 /proc」。那兩件事要做的事完全不同。
		// 掃描品質一律看 processScan，不准用 len(procs) 推論：len(procs) == 0
		// 同時可能是 unavailable 與 restricted（ReadDir 成功但過濾後 0 個也是 restricted）。
		switch processScan {
		case model.ProcessScanUnavailable:
			return model.BAT{Reason: "讀不到 /proc，這台的 process 偵測整個是關的"}
		case model.ProcessScanComplete:
			return model.BAT{Reason: fmt.Sprintf(
				"掃了 %d 個 process，沒有一個是 %s", len(procs), batProcName)}
		case model.ProcessScanRestricted:
			return model.BAT{Reason: fmt.Sprintf(
				"只掃得到 %d 個 process，這台的 /proc 視野被限制了，查不出 %s 是否在跑", len(procs), batProcName)}
		default:
			return model.BAT{Reason: fmt.Sprintf(
				"process 掃描沒有完成，查不出 %s 是否在跑", batProcName)}
		}
	}

	b := model.BAT{Running: true, Argv: truncRunes(strings.Join(p.argv, " "), batMaxArgv)}
	b.Port = batFlagInt(p.argv, "--port")
	b.Bind = batFlagStr(p.argv, "--bind")

	switch {
	case b.Port == 0:
		// ⚠ 認不得就說認不得，不要填一個預設值。上一個版本填的預設值
		// 就是 8080，而它在四台機器上全錯。
		b.Reason = "認不得 argv 裡的 --port，原文留在 argv 欄位"
		return b
	case b.Bind == "":
		b.Reason = "認不得 argv 裡的 --bind —— 不知道它有沒有綁在對外的介面上"
	}

	var measured bool
	b.ListenAddrs, measured = listeningOnPaths(socketPaths, b.Port)
	if len(b.ListenAddrs) == 0 && b.Reason == "" {
		if measured {
			// argv 說了要綁哪裡，但核心的表上沒有。這是「啟動了但沒綁成功」。
			b.Reason = fmt.Sprintf(
				"process 在跑（--port=%d），但核心的 socket 表上沒有任何東西在聽這個 port", b.Port)
		} else {
			b.Reason = fmt.Sprintf(
				"這台的核心 socket 表量測沒有跑到底（--port=%d），查不出這個 port 上有沒有東西在聽", b.Port)
		}
	}
	return b
}

// findBAT 只認 argv[0] 的檔名。
//
// ⚠ 不用 comm：node 之類的 runtime 會改主執行緒的名字（見 scanProcesses 的註解）。
// ⚠ 不用 exe：那個要 ptrace，在 agent 的 unit 裡永遠是空的（§5.10）。
// argv[0] 是這裡唯一在四台機器上都讀得到的東西 —— 包括 sampleagent3 上那個
// 以 root 身分跑、而 agent 是 opc 的那一個。
func findBAT(procs []procInfo) (procInfo, bool) {
	for _, p := range procs {
		if len(p.argv) > 0 && baseName(p.argv[0]) == batProcName {
			return p, true
		}
	}
	return procInfo{}, false
}

func baseName(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}

// batFlagStr 讀 `--flag=value` 或 `--flag value` 兩種寫法。
// ⚠ 兩種都要支援：bat-server 自己的 unit 用第一種，但那是它的選擇，不是保證。
func batFlagStr(argv []string, flag string) string {
	for i, a := range argv {
		if v, ok := strings.CutPrefix(a, flag+"="); ok {
			return v
		}
		if a == flag && i+1 < len(argv) {
			return argv[i+1]
		}
	}
	return ""
}

func batFlagInt(argv []string, flag string) int {
	n, err := strconv.Atoi(strings.TrimSpace(batFlagStr(argv, flag)))
	if err != nil || n <= 0 || n > 65535 {
		return 0
	}
	return n
}

// listeningOnPaths 回報核心的 socket 表裡，處於 LISTEN 而且 port 相符的本機位址。
//
// ⚠⚠ 這一格**不能**宣稱那個 socket 就是 BAT 的。
//
// 把 socket 歸屬到 pid 要讀 /proc/<pid>/fd/，而那對別的使用者的 process
// 需要特權 —— 機隊裡有兩台的 bat-server 是 root 起的，agent 不是。
// 所以這裡誠實的說法只有「這個 port 上有東西在聽」。
// 畫面上的措辭必須守住這個分寸，否則就是在填一個沒有證據的欄位。
//
// 讀 /proc/net/tcp{,6} 而不是跑 ss：ss 不是每台都有（實測 sampleagent3 上
// 一般使用者跑 ss 拿不到任何東西），而且解析它的輸出就是在解析自由文字。
// /proc/net/tcp 是核心的格式，欄位是固定的。
func listeningOnPaths(paths []string, port int) ([]string, bool) {
	seen := map[string]bool{}
	read := 0
	measured := true
	for _, path := range paths {
		addrs, err := parseProcNetTCP(path, port)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			measured = false
			continue
		}
		read++
		for _, a := range addrs {
			seen[a] = true
		}
	}
	out := make([]string, 0, len(seen))
	for a := range seen {
		out = append(out, a)
	}
	sort.Strings(out)
	return out, measured && read > 0
}

// parseProcNetTCP 解析核心的連線表。格式（欄位以空白分隔）：
//
//	sl  local_address rem_address st tx_queue rx_queue tr tm->when retrnsmt uid timeout inode
//	 0: 0100007F:26AC 00000000:0000 0A ...
//
// local_address 是 `位址:PORT`，兩邊都是十六進位大寫。st == 0A 是 TCP_LISTEN。
//
// ⚠ 位址那串是**主機位元組序**印出來的，所以 x86/arm 上要把每 4 bytes 反轉
// 才是網路位元組序：0100007F → 7F 00 00 01 → 127.0.0.1。
// 少了這一步不會噴錯，只會安靜地印出一個看起來很像 IP 的錯位址。
func parseProcNetTCP(path string, port int) ([]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	const tcpListen = "0A"
	var out []string
	lines := strings.Split(string(b), "\n")
	for _, ln := range lines[1:] { // 第一行是表頭
		f := strings.Fields(ln)
		if len(f) < 4 || f[3] != tcpListen {
			continue
		}
		host, p, ok := strings.Cut(f[1], ":")
		if !ok {
			continue
		}
		n, err := strconv.ParseUint(p, 16, 32)
		if err != nil || int(n) != port {
			continue
		}
		if ip := hexHostIP(host); ip != nil {
			out = append(out, ip.String())
		}
	}
	return out, nil
}

// hexHostIP 把 /proc/net/tcp 的十六進位位址轉回 IP。
// 8 個字元是 IPv4，32 個是 IPv6；兩者都是每 4 bytes 一組、主機位元組序。
func hexHostIP(s string) net.IP {
	raw, err := hex.DecodeString(s)
	if err != nil || (len(raw) != 4 && len(raw) != 16) {
		return nil
	}
	for i := 0; i < len(raw); i += 4 {
		raw[i], raw[i+1], raw[i+2], raw[i+3] = raw[i+3], raw[i+2], raw[i+1], raw[i]
	}
	return net.IP(raw)
}
