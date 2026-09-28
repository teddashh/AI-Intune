package tailnet

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"
)

// RosterEntry 是名冊裡的一列，只帶對照需要的欄位。
// ⚠ 刻意不 import store —— 對照是純函式，要能不碰資料庫就測。
type RosterEntry struct {
	MachineID   string
	DisplayName string
	Hostname    string
	TailscaleIP string
	// Retired 的機器**照樣要傳進來**。它不算分母，但它在名冊裡 ——
	// 抽掉它只會讓對照把它講成「不在名冊裡」。理由見 store.RosterForTailnet。
	Retired bool
	// RetiredAt 是退役的時間。
	// ⚠ 帶著它是因為「五分鐘前退役的」跟「三週前退役的」要做的事不一樣：
	// 前者是退役程序還沒做完，後者是你很可能退役錯了一台。
	RetiredAt time.Time
}

// Result 是名冊與 tailnet 對照之後的結果。
type Result struct {
	Available   bool
	Unavailable string

	// Unenrolled：tailnet 上看得到、名冊裡沒有、也沒有被明確忽略的機器。
	//
	// ⚠ 這一格是這整個功能的重點。名冊是我們自己寫進去的，
	// 所以它永遠答不出「有沒有一台機器存在、而我忘了把它放進名冊」。
	Unenrolled []Peer

	// OnlineButSilent：名冊裡有、tailnet 說它 online、但它沒在跟 Hub 講話。
	//
	// ⚠ 這一格改變的是**下一步要做什麼**，不是狀態。
	// 「機器死了」跟「機器活著但 agent 沒在報」要做的事完全不同，
	// 而 Hub 自己永遠分不出這兩件事 —— 它只知道沒收到心跳。
	OnlineButSilent map[string]Peer // machine_id → peer

	// RetiredButOnline：名冊上退役了，但 tailnet 說那台機器還活著。
	//
	// ⚠ 這一格不是警報，是一個**矛盾**：一邊是人手動宣告的「這台不管了」，
	// 一邊是第三方觀測到的「它還在跑」。兩種可能都很常見 ——
	// 退役程序還沒做完（正常），或者退役按錯了機器（不正常）。
	// Hub 分不出是哪一種，所以它只負責把矛盾攤出來，不下判決。
	//
	// ⚠ 它必須跟 Unenrolled 分開。混在一起的時候畫面會說「不在名冊裡」，
	// 而那是假的 —— 它在名冊裡，只是退役了。
	RetiredButOnline []RetiredPeer

	Ignored int // 被明確忽略的有幾台，讓畫面可以說「另外 N 台你說過不管」
}

// RetiredPeer 是一台「退役了但還在 tailnet 上」的機器。
// ⚠ 帶著 MachineID 是為了讓畫面連得回那一頁 —— 而取消退役的按鈕在那一頁上。
type RetiredPeer struct {
	MachineID   string    `json:"machine_id"`
	DisplayName string    `json:"display_name"`
	RetiredAt   time.Time `json:"retired_at"`
	Peer        Peer      `json:"peer"`
}

// Reconcile 把 tailnet 的清單跟名冊對起來。
//
// ⚠ 對照的鍵依序是 tailscale IP → hostname → display name，全部忽略大小寫。
// 只用 hostname 會對不上：實機上名冊記的是 `open-claw-vnic`、`fuhqsiem0`，
// 而 tailnet 上叫 `sampleagent2`、`sampleagent4`。IP 才是可靠的那個。
// 但一台從未報到的機器 IP 與 hostname 都是空的，那時只剩 display name 能對 ——
// 而那正好是最需要對上的情況。
func Reconcile(s Status, roster []RosterEntry, ignored map[string]bool, silent map[string]bool) Result {
	if !s.Available {
		// ⚠ 不准回一個空的 Result 就算了。空的 Unenrolled 意思是
		// 「沒有名冊外的機器」，而這裡的真相是「我沒有問到」。
		return Result{Unavailable: s.Unavailable}
	}
	r := Result{Available: true, OnlineButSilent: map[string]Peer{}}

	byKey := map[string]RosterEntry{}
	for _, e := range roster {
		for _, k := range []string{e.TailscaleIP, e.Hostname, e.DisplayName} {
			if k = norm(k); k != "" {
				byKey[k] = e
			}
		}
	}

	// Hub 自己也在 tailnet 上，而且它通常也在名冊裡。把 Self 一起看。
	all := append([]Peer{s.Self}, s.Peers...)
	for _, p := range all {
		e, known := lookup(byKey, p)
		if !known {
			// Current rules use Tailscale's stable node identity so two devices
			// named localhost remain independently manageable. The normalized
			// hostname lookup is retained only for pre-stable-ID ledger rows.
			if ignored[PeerIgnoreKey(p.StableID)] || ignored[norm(p.Hostname)] {
				r.Ignored++
				continue
			}
			r.Unenrolled = append(r.Unenrolled, p)
			continue
		}
		if e.Retired {
			// ⚠ 退役的機器不進 OnlineButSilent —— 那一格的意思是
			// 「它活著，但 agent 沒在報」，而退役的機器**本來就不該報**。
			// 把它放進去會產生一個要人去修的警報，而那裡沒有東西壞掉。
			if p.Online {
				r.RetiredButOnline = append(r.RetiredButOnline, RetiredPeer{
					MachineID: e.MachineID, DisplayName: e.DisplayName,
					RetiredAt: e.RetiredAt, Peer: p,
				})
			}
			continue
		}
		if p.Online && silent[e.MachineID] {
			r.OnlineButSilent[e.MachineID] = p
		}
	}
	// ⚠ tailscale 的 Peer 是 map，迭代順序每次都不一樣。
	// 不排序的話畫面每次重整都在跳，而且早報會每天說「順序變了」。
	// online 的排前面 —— 那幾台是你現在真的可以連上去看的。
	sort.Slice(r.Unenrolled, func(i, j int) bool {
		a, b := r.Unenrolled[i], r.Unenrolled[j]
		if a.Online != b.Online {
			return a.Online
		}
		return strings.ToLower(a.Hostname) < strings.ToLower(b.Hostname)
	})
	// ⚠ 同一個理由：Peer 是 map，不排序畫面每次重整都在跳。
	sort.Slice(r.RetiredButOnline, func(i, j int) bool {
		return strings.ToLower(r.RetiredButOnline[i].DisplayName) <
			strings.ToLower(r.RetiredButOnline[j].DisplayName)
	})
	return r
}

func lookup(byKey map[string]RosterEntry, p Peer) (RosterEntry, bool) {
	for _, k := range []string{p.IP, p.Hostname} {
		if k = norm(k); k != "" {
			if e, ok := byKey[k]; ok {
				return e, true
			}
		}
	}
	return RosterEntry{}, false
}

func norm(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// PeerIgnoreKey names the stable-identity namespace used by current ignore rules.
// Hostname keys remain readable for ledgers created before Tailscale node IDs were stored.
func PeerIgnoreKey(stableID string) string {
	stableID = strings.TrimSpace(stableID)
	if stableID == "" {
		return ""
	}
	return "id:" + stableID
}

// ---------------------------------------------------------------- 快取
//
// ⚠ 每次開總覽都 exec 一次 tailscale 太貴（要 10 秒 timeout）。
// 但快取不准太久：名冊外多一台機器這件事，晚一分鐘知道沒關係，
// 晚一小時就變成「這個功能其實沒在跑」。

const cacheTTL = 60 * time.Second

type Cache struct {
	mu     sync.Mutex
	at     time.Time
	last   Status
	pinned bool
	now    func() time.Time // 測試用
}

func NewCache() *Cache { return &Cache{now: time.Now} }

// SetStatus 釘住一份 Status，讓這個 Cache 永遠不去 exec tailscale。**只給測試用。**
//
// ⚠ 一個會 exec tailscale 的測試，答案取決於**跑測試那台機器的 tailnet 狀態**：
// 在 samplehub1 上它拿到真的五台機器，在一台沒裝 tailscale 的機器上它拿到
// 「沒有 tailscale 指令」—— 兩邊走的是畫面上完全不同的分支。
//
// ⚠ 更糟的不是不穩定，是**假的覆蓋**：退役那支測試裡有一條斷言在檢查
// 「退役的機器不准被講成不在名冊裡」，而它的名冊只有 keeper/leaver 兩台。
// leaver 從來不是真的 tailnet peer，所以那條斷言永遠不可能失敗 ——
// 它看起來像覆蓋，實際上是一句空話。§5.9 的那句話又應驗一次：
// 一個乾淨的、沒有壞消息的答案，第一個要懷疑的是有沒有問對地方。
//
// ⚠ pinned 是必要的：只塞 last 會在 60 秒的 TTL 之後自己去 exec 一次。
func (c *Cache) SetStatus(s Status) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.last, c.pinned = s, true
}

func (c *Cache) Get(ctx context.Context) Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pinned {
		if c.last.Available && c.last.ObservedAt.IsZero() {
			c.last.ObservedAt = c.nowTime().UTC()
		}
		return c.last
	}
	now := c.nowTime()
	if !c.at.IsZero() && now.Sub(c.at) < cacheTTL {
		return c.last
	}
	c.last = Query(ctx)
	if c.last.Available {
		c.last.ObservedAt = now.UTC()
	}
	c.at = now
	return c.last
}

func (c *Cache) nowTime() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}
