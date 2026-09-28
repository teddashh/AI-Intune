// Package tailnet 從 Tailscale 借一件事：**一份不是我們自己寫的機器清單。**
//
// 名冊是分母 —— 但名冊只知道「我宣告要納管的機器」。它是我們自己寫進去的，
// 所以它沒辦法回答「有沒有一台機器存在、而我根本忘了把它放進名冊」。
// 名冊替自己作證，而自證不算數。
//
// Tailscale 知道「網路上實際連著的機器」，那是一份**獨立產生**的清單。
// 兩份的差集就是你沒在看的東西。
//
// ⚠ 但 Tailscale 也不是真相。它只知道裝了 Tailscale 的機器。
// 一台既不在名冊、也不在 tailnet 上的機器，兩邊都看不到它 ——
// 這一層的文案永遠只能說「多一個來源說……」，不准說「全部的機器」。
//
// ⚠⚠ 讀不到 tailscale 的時候，回傳的是「沒有這個來源」，**不是「0 台在名冊外」**。
// 這兩件事在畫面上長得很像，意思差很多。2026-09-03 的 §5.9 就是這樣來的：
// 一個空的、乾淨的、沒有壞消息的答案，其實是「你問錯地方了」。
package tailnet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/teddashh/AI-Intune/internal/processenv"
)

// Peer 是 tailnet 上的一台機器。欄位刻意只留我們真的會用的。
type Peer struct {
	StableID string    `json:"stable_id"`
	Hostname string    `json:"hostname"`
	IP       string    `json:"ip"`
	OS       string    `json:"os"`
	Online   bool      `json:"online"`
	LastSeen time.Time `json:"last_seen,omitempty"` // ⚠ online 的 peer 這欄是零值，不是 1 年
}

// Status 是一次查詢的結果。
type Status struct {
	ObservedAt time.Time `json:"observed_at,omitempty"`
	// Available：這次到底有沒有問到。
	// ⚠ false 的時候 Peers 一定是空的，而空的 Peers **不代表 tailnet 上沒有機器**。
	Available bool
	// Unavailable 說明為什麼問不到，要能直接貼給人看。
	Unavailable string
	Self        Peer
	Peers       []Peer
}

// tailscale status --json 的欄位子集。
type rawStatus struct {
	Self  *rawPeer            `json:"Self"`
	Peer  map[string]*rawPeer `json:"Peer"`
	Error string              `json:"Error"`
}

type rawPeer struct {
	ID           string   `json:"ID"`
	HostName     string   `json:"HostName"`
	TailscaleIPs []string `json:"TailscaleIPs"`
	OS           string   `json:"OS"`
	Online       bool     `json:"Online"`
	LastSeen     string   `json:"LastSeen"`
}

func (r *rawPeer) toPeer() Peer {
	p := Peer{StableID: strings.TrimSpace(r.ID), Hostname: r.HostName, OS: r.OS, Online: r.Online}
	for _, ip := range r.TailscaleIPs {
		// ⚠ 只取 IPv4。fd7a:… 那條 v6 在畫面上沒人認得出是哪台，
		// 而且 samplehub1 的 LAN 根本沒有 v6，貼上去也連不到。
		if !strings.Contains(ip, ":") {
			p.IP = ip
			break
		}
	}
	if r.LastSeen != "" {
		if t, err := time.Parse(time.RFC3339, r.LastSeen); err == nil && t.Year() > 1 {
			p.LastSeen = t
		}
	}
	return p
}

// Query 跑 `tailscale status --json`。
//
// ⚠ 用 CLI 不用 API key 是刻意的：API key 要申請、會過期、要放進設定檔，
// 而這個功能的價值在於「不必多做任何事就多一個獨立來源」。
// 一個需要先去辦手續才會生效的交叉檢查，等於沒有做。
func Query(ctx context.Context) Status {
	return queryWith(ctx, runTailscale)
}

func queryWith(ctx context.Context, run func(context.Context) ([]byte, error)) Status {
	out, err := run(ctx)
	if err != nil {
		return Status{Unavailable: humanReason(err)}
	}
	var raw rawStatus
	if err := json.Unmarshal(out, &raw); err != nil {
		return Status{Unavailable: fmt.Sprintf("tailscale status 的輸出看不懂（%v）", err)}
	}
	if raw.Error != "" {
		return Status{Unavailable: "tailscale 回報：" + raw.Error}
	}
	if raw.Self == nil {
		return Status{Unavailable: "tailscale status 未回報本機"}
	}

	s := Status{Available: true, ObservedAt: time.Now().UTC(), Self: raw.Self.toPeer()}
	for _, p := range raw.Peer {
		if p == nil || p.HostName == "" {
			continue
		}
		s.Peers = append(s.Peers, p.toPeer())
	}
	return s
}

func runTailscale(ctx context.Context) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return processenv.CommandContext(ctx, "tailscale", "status", "--json").Output()
}

func humanReason(err error) string {
	var ee *exec.ExitError
	if errors.As(err, &ee) && len(ee.Stderr) > 0 {
		return "tailscale status 失敗：" + strings.TrimSpace(string(ee.Stderr))
	}
	if errors.Is(err, exec.ErrNotFound) {
		return "這台 Hub 上沒有 tailscale 指令，所以沒有第二份機器清單可以對照"
	}
	return "問不到 tailscale：" + err.Error()
}
