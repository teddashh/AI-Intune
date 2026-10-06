package store

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/teddashh/AI-Intune/internal/tailnet"
)

// tailnet 忽略清單 —— 「這台我知道，我不打算納管」。
//
// ⚠ 沒有這張清單，手機、Windows 桌機那些永遠不會裝 agent 的東西會一直掛在
// 「名冊外的機器」裡，三天之後整段被當成背景雜訊（§5.1 那個永遠亮的黃燈）。
// ⚠ 忽略不是刪除。被忽略的台數仍然要顯示，否則就變成偷偷藏起來。

func (s *Store) IgnorePeer(hostname, note string) error {
	h := strings.ToLower(strings.TrimSpace(hostname))
	if h == "" {
		return errors.New("store: 要忽略哪一台？hostname 是空的")
	}
	_, err := s.execWrite(context.Background(), "ignore_peer", `INSERT INTO tailnet_ignored (hostname, note, created_at) VALUES (?,?,?)
		 ON CONFLICT(hostname) DO UPDATE SET note = excluded.note`,
		h, nullStr(note), fmtTime(time.Now()))
	return err
}

func (s *Store) UnignorePeer(hostname string) error {
	_, err := s.execWrite(context.Background(), "unignore_peer", `DELETE FROM tailnet_ignored WHERE hostname = ?`,
		strings.ToLower(strings.TrimSpace(hostname)))
	return err
}

// IgnoredPeers 回傳忽略清單，key 已正規化成小寫。
func (s *Store) IgnoredPeers() (map[string]bool, error) {
	return s.IgnoredPeersAt(s.now())
}

// IgnoredPeersAt combines the legacy hostname ledger with current stable-ID
// rules using one caller-supplied clock. Reconcile and the visible rule list
// must agree at an expiry boundary instead of consulting two nearby instants.
func (s *Store) IgnoredPeersAt(now time.Time) (map[string]bool, error) {
	rows, err := s.rdb.Query(`SELECT hostname FROM tailnet_ignored`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			return nil, err
		}
		out[h] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	current, err := s.TailnetPeerIgnores(now)
	if err != nil {
		return nil, err
	}
	for _, item := range current {
		out[tailnet.PeerIgnoreKey(item.PeerID)] = true
	}
	return out, nil
}

// TailnetLegacyIgnoredCount reports durable hostname-only rules created by the
// retired ignore-peer workflow. They remain visible until explicitly migrated
// or removed, even when the current Tailnet observation cannot be collected.
func (s *Store) TailnetLegacyIgnoredCount() (int, error) {
	var count int
	if err := s.rdb.QueryRow(`SELECT COUNT(*) FROM tailnet_ignored`).Scan(&count); err != nil {
		return 0, err
	}
	return count, nil
}

// RosterForTailnet 把名冊攤成對照用的最小欄位。
//
// ⚠⚠ retire 過的**也要列**，只是標記起來。這裡原本是
// `if m.RetiredAt != nil { continue }`，理由寫的是「它已經離開分母，
// 再出現在 tailnet 上是正常的」。那個理由沒錯，那個做法達不到它。
//
// 從對照的名冊裡把一台機器抽掉，效果不是「安靜」，是**換一句錯的話**：
// 對照不到的機器會被歸進 Unenrolled，而畫面上那一段的標題是
// 「Tailscale 上看得到 N 台不在名冊裡的機器」。它在名冊裡。它是退役的。
//
// 實測（samplehub1，2026-09-03，退役 sampleagent1 兩秒）：首頁把 sampleagent1 列成
// 「不在名冊裡」，綠燈 online，底下還附一句「不打算納管就跑
// ignore-peer」—— 對一台納管過、而且剛剛才被人手動退役的機器。
// 唯一還提到它的那一列，說的每一件事都是錯的。
//
// **沉默要靠說對的話達成，不是靠把資料抽掉。** 少一列資料，
// 下游不會安靜，只會拿剩下的資料去推出一個錯的結論。
func (s *Store) RosterForTailnet() ([]tailnet.RosterEntry, error) {
	machines, err := s.ListMachines()
	if err != nil {
		return nil, err
	}
	out := make([]tailnet.RosterEntry, 0, len(machines))
	for _, m := range machines {
		out = append(out, tailnet.RosterEntry{
			MachineID: m.MachineID, DisplayName: m.DisplayName,
			Hostname: m.Hostname, TailscaleIP: m.TailscaleIP,
			Retired: m.RetiredAt != nil, RetiredAt: derefTime(m.RetiredAt),
		})
	}
	return out, nil
}

func derefTime(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}
