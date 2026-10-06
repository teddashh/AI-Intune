package store

import (
	"fmt"
	"time"
)

// enrollmentTicketExpiredPredicate 是「這張沒被用掉的票算不算過期」的唯一判準。
//
// ⚠ 它是一個共用常數而不是各查各的，因為單機那一頁跟機隊那一份報告必須對同一張票
// 給出同一個答案。兩份各自寫一次 SQL，第一次有人改了其中一份，畫面上就會出現
// 「這台的票還有效」跟「這台的票過期了」同時成立。
//
// 解不出來的 expires_at 一律算過期——跟 RedeemEnrollToken 同一條規則。一個壞掉的
// 時間戳不可以在畫面上變成「還有效」。欄位固定寫成 e.expires_at，時間走一個 bind
// 參數。
const enrollmentTicketExpiredPredicate = `(length(e.expires_at) != 20 OR substr(e.expires_at,20,1) != 'Z'
	 OR strftime('%Y-%m-%dT%H:%M:%SZ',e.expires_at) IS NULL
	 OR strftime('%Y-%m-%dT%H:%M:%SZ',e.expires_at) != e.expires_at
	 OR e.expires_at<=?)`

// MaxPendingEnrollmentTickets 是一次讀得了幾張還沒用掉的票。
//
// 開一張票就建一列名冊，所以未用票的數量被「有多少人宣告過要納管某台機器」綁住。
// 超過這個數字表示這個 Hub 已經不是這個產品在講的那種規模，這時候寧可拒絕，也不要
// 回一份看起來完整、其實少算的清單。
const MaxPendingEnrollmentTickets = 5000

// PendingEnrollmentTicket 是一台機器身上還沒被用掉的票。
type PendingEnrollmentTicket struct {
	// Pending 是這台機器身上還沒被用掉的票數。
	Pending int
	// Expired 是其中已經過期的票數。
	Expired int
	// CreatedAt / ExpiresAt 來自最新的那一張未用票，跟單機頁挑的是同一張。
	CreatedAt time.Time
	ExpiresAt time.Time
	// NewestExpired 說最新的那一張過期了沒有。
	NewestExpired bool
}

// PendingEnrollmentTickets 回名冊上每一台機器身上還沒被用掉的票。
//
// ⚠ 一次掃完整張 enrollment_tokens 再在 Go 裡分組，不是逐台機器各查一次：
// used_by 沒有索引，逐台查會變成「機器數 × 票數」次掃描。
func (s *Store) PendingEnrollmentTickets(now time.Time) (map[string]PendingEnrollmentTicket, error) {
	if now.IsZero() {
		return nil, fmt.Errorf("store: pending enrollment tickets need a trusted clock")
	}
	rows, err := s.rdb.Query(`
SELECT e.used_by, e.created_at, e.expires_at, `+enrollmentTicketExpiredPredicate+`
  FROM enrollment_tokens e
 WHERE e.used_at IS NULL AND e.used_by IS NOT NULL AND e.used_by <> ''
 ORDER BY e.used_by, e.created_at DESC, e.token_hash DESC
 LIMIT ?`, fmtTime(now.UTC()), MaxPendingEnrollmentTickets+1)
	if err != nil {
		return nil, fmt.Errorf("store: read pending enrollment tickets: %w", err)
	}
	defer rows.Close()

	tickets := map[string]PendingEnrollmentTicket{}
	read := 0
	for rows.Next() {
		var machineID, createdRaw, expiresRaw string
		var expired int
		if err := rows.Scan(&machineID, &createdRaw, &expiresRaw, &expired); err != nil {
			return nil, fmt.Errorf("store: scan pending enrollment ticket: %w", err)
		}
		read++
		if read > MaxPendingEnrollmentTickets {
			return nil, fmt.Errorf(
				"store: 還沒用掉的 enroll 票超過 %d 張，拒絕回一份少算的清單",
				MaxPendingEnrollmentTickets)
		}
		ticket, seen := tickets[machineID]
		ticket.Pending++
		if expired != 0 {
			ticket.Expired++
		}
		// ORDER BY 讓每一組的第一列就是最新的那一張；後面的只加總，不覆蓋。
		if !seen {
			ticket.CreatedAt = parseTime(createdRaw)
			ticket.ExpiresAt = parseTime(expiresRaw)
			ticket.NewestExpired = expired != 0
		}
		tickets[machineID] = ticket
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate pending enrollment tickets: %w", err)
	}
	return tickets, nil
}
