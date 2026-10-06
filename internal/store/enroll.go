package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// 發出去的票，以及唯一一條收回它的路。
//
// ⚠⚠ 這個檔案存在的理由：**發票原本沒有反向操作。**
//
// `internal/web/actions.go` 開頭替這個 console 立了三條規則，第三條是
// 「每一個寫入都有反向操作，而且畫面上看得到」。retire 有 unretire，
// connect 只是轉址不改狀態 —— 只有發票沒有。發出去的票在 TTL 到期前
// 一直有效，而人唯一能做的事是等。
//
// ⚠ 但要講清楚 revoke **收不回什麼**：token 的明文一旦被畫在畫面上，
// 它就已經進了瀏覽器歷史、可能進了截圖、進了任何一個看得到那個畫面的人
// 眼裡。revoke 讓那串字**不能再兌換**，它不會讓那串字消失。
// 這兩件事的差別要寫在畫面上，不然「我撤銷了」會被讀成「沒事了」。

// PendingToken 是一張還沒被用掉的票。
type PendingToken struct {
	MachineID   string    `json:"machine_id"`
	DisplayName string    `json:"display_name"`
	CreatedAt   time.Time `json:"created_at"`
	ExpiresAt   time.Time `json:"expires_at"`
	// Expired 是相對於問的那個當下。
	// ⚠ 過期的票**照樣要顯示**，只是標成過期 —— 因為它綁的名冊列還在分母裡，
	// 而「開了票、機器沒來」正是最需要看到的那個狀態。
	Expired bool `json:"expired"`
}

// PendingEnrollToken 回這台機器身上還沒被用掉的票，沒有就回 nil。
//
// ⚠ 一台機器同時只會有一張未用的票（開新票會建新的名冊列），
// 所以這裡只回一張。真的有多張時取最新的那一張。
func (s *Store) PendingEnrollToken(machineID string, now time.Time) (*PendingToken, error) {
	var displayName, createdAt, expiresAt string
	err := s.rdb.QueryRow(`
SELECT display_name, created_at, expires_at
  FROM enrollment_tokens
 WHERE used_by = ? AND used_at IS NULL
 ORDER BY created_at DESC LIMIT 1`, machineID).Scan(&displayName, &createdAt, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: pending token: %w", err)
	}
	exp := parseTime(expiresAt)
	return &PendingToken{
		MachineID: machineID, DisplayName: displayName,
		CreatedAt: parseTime(createdAt), ExpiresAt: exp,
		// ⚠ 解不出來的 expires_at 算過期 —— 跟 RedeemEnrollToken 同一條規則。
		// 一個壞掉的時間戳不可以在畫面上變成「還有效」。
		Expired: exp.IsZero() || !now.Before(exp),
	}, nil
}

// RevokeEnrollToken 讓一張還沒用掉的票不能再兌換。
//
// ⚠ 只刪 enrollment_tokens 那一列，**名冊列不動**。
//
// 那是刻意的：開票的那一刻已經有人明確說出「我打算納管這台」，那個宣告
// 是事實，撤票不會讓它變成沒發生過。要讓它離開分母只有一條路 ——
// 明確 retire，而那是一個看得見、有理由、進 audit 的動作。
//
// ⚠ 撤銷本身不需要反向操作：想再要一張就再開一張。
// 這跟 retire 不一樣 —— 收回一張憑證是安全的方向。
func (s *Store) RevokeEnrollToken(machineID string) (string, error) {
	var displayName string
	err := s.rdb.QueryRow(`
SELECT display_name FROM enrollment_tokens
 WHERE used_by = ? AND used_at IS NULL
 ORDER BY created_at DESC LIMIT 1`, machineID).Scan(&displayName)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("store: revoke lookup: %w", err)
	}
	if _, err := s.execWrite(context.Background(), "revoke_enroll_token", `DELETE FROM enrollment_tokens WHERE used_by = ? AND used_at IS NULL`,
		machineID); err != nil {
		return "", fmt.Errorf("store: revoke: %w", err)
	}
	return displayName, nil
}
