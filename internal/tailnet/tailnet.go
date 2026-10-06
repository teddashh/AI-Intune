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
	"sort"
	"strconv"
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
	// UserID 是這台 peer 擁有者的 tailnet 使用者 ID，十進位字串，跟 operator auth 同一個鍵。
	// UserLogin 是顯示用的 login。兩者都不進現有的 tailnet JSON。
	UserID    string `json:"-"`
	UserLogin string `json:"-"`
}

// TailnetUser 是 tailscale status 的 User map 裡的一列。
type TailnetUser struct {
	UserID      string
	Login       string
	DisplayName string
}

// User 是至少擁有一台 peer（含 Self）的 tailnet 使用者。
type User struct {
	UserID      string
	Login       string
	DisplayName string
	PeerCount   int
}

// UserDirectory 是一次名冊。Available 為 false 時沒有這份來源。
type UserDirectory struct {
	Available   bool
	Unavailable string
	Users       []User
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
	// TailnetUsers 是這次讀到的 User map。問不到的時候是 nil。
	TailnetUsers []TailnetUser `json:"-"`
	// userPeerIDs 是每一台 peer（含 Self、含沒有 hostname 的）的使用者 ID。
	// nil 表示這份 Status 是呼叫端組的，Users 改從 Self 與 Peers 數。
	userPeerIDs []string
}

// tailscale status --json 的欄位子集。
type rawStatus struct {
	Self  *rawPeer            `json:"Self"`
	Peer  map[string]*rawPeer `json:"Peer"`
	User  map[string]*rawUser `json:"User"`
	Error string              `json:"Error"`
}

type rawUser struct {
	ID          int64  `json:"ID"`
	LoginName   string `json:"LoginName"`
	DisplayName string `json:"DisplayName"`
}

type rawPeer struct {
	ID           string   `json:"ID"`
	HostName     string   `json:"HostName"`
	TailscaleIPs []string `json:"TailscaleIPs"`
	OS           string   `json:"OS"`
	Online       bool     `json:"Online"`
	LastSeen     string   `json:"LastSeen"`
	UserID       int64    `json:"UserID"`
}

func (r *rawPeer) toPeer(logins map[string]string) Peer {
	p := Peer{StableID: strings.TrimSpace(r.ID), Hostname: r.HostName, OS: r.OS, Online: r.Online}
	if r.UserID > 0 {
		p.UserID = strconv.FormatInt(r.UserID, 10)
		p.UserLogin = logins[p.UserID]
	}
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

	logins := map[string]string{}
	var profiles []TailnetUser
	for _, user := range raw.User {
		if user == nil || user.ID <= 0 {
			continue
		}
		id := strconv.FormatInt(user.ID, 10)
		login := strings.TrimSpace(user.LoginName)
		logins[id] = login
		profiles = append(profiles, TailnetUser{
			UserID: id, Login: login, DisplayName: strings.TrimSpace(user.DisplayName),
		})
	}
	ownerIDs := make([]string, 0, 1+len(raw.Peer))
	if raw.Self.UserID > 0 {
		ownerIDs = append(ownerIDs, strconv.FormatInt(raw.Self.UserID, 10))
	}
	s := Status{
		Available: true, ObservedAt: time.Now().UTC(), Self: raw.Self.toPeer(logins),
		TailnetUsers: profiles, userPeerIDs: ownerIDs,
	}
	for _, p := range raw.Peer {
		if p == nil {
			continue
		}
		if p.UserID > 0 {
			s.userPeerIDs = append(s.userPeerIDs, strconv.FormatInt(p.UserID, 10))
		}
		if p.HostName == "" {
			continue
		}
		s.Peers = append(s.Peers, p.toPeer(logins))
	}
	return s
}

// Users 列出這次讀取裡至少擁有一台 peer（含 Self）的 tailnet 使用者。
//
// ⚠ Available == false 時，這是「沒有這個來源」，不是「0 個使用者」。
// 空的 Users 只有在真的問到、而且沒有任何 peer 帶使用者時才算數。
func Users(s Status) UserDirectory {
	if !s.Available {
		return UserDirectory{Unavailable: s.Unavailable}
	}
	ids := s.userPeerIDs
	if ids == nil {
		if s.Self.UserID != "" {
			ids = append(ids, s.Self.UserID)
		}
		for _, peer := range s.Peers {
			if peer.UserID != "" {
				ids = append(ids, peer.UserID)
			}
		}
	}
	counts := map[string]int{}
	for _, id := range ids {
		if id != "" {
			counts[id]++
		}
	}
	profiles := map[string]TailnetUser{}
	for _, user := range s.TailnetUsers {
		if user.UserID != "" {
			profiles[user.UserID] = user
		}
	}
	peerLogin := map[string]string{}
	if s.Self.UserID != "" && s.Self.UserLogin != "" {
		peerLogin[s.Self.UserID] = s.Self.UserLogin
	}
	for _, peer := range s.Peers {
		if peer.UserID != "" && peer.UserLogin != "" {
			peerLogin[peer.UserID] = peer.UserLogin
		}
	}
	users := make([]User, 0, len(counts))
	for id, count := range counts {
		user := User{UserID: id, PeerCount: count}
		if profile, ok := profiles[id]; ok {
			user.Login = profile.Login
			user.DisplayName = profile.DisplayName
		}
		if user.Login == "" {
			user.Login = peerLogin[id]
		}
		users = append(users, user)
	}
	sort.Slice(users, func(i, j int) bool {
		if users[i].Login != users[j].Login {
			return users[i].Login < users[j].Login
		}
		return users[i].UserID < users[j].UserID
	})
	return UserDirectory{Available: true, Users: users}
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
