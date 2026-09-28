package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Hub 自己的日誌。理由寫在 schema.sql 的 hub_events 上面：
// 它回答「觀測者那幾分鐘為什麼沒在聽」，不回答「Hub 健不健康」。

const (
	HubStarted                  = "started"                    // 程序起來了；detail 帶「上一次活著是多久前」
	HubStopping                 = "stopping"                   // 收到停止訊號（kill -9 不會有這一筆，那是預期的）
	HubReconcileSlow            = "reconcile_slow"             // 一輪對帳花得比節奏還久
	HubLoopStall                = "loop_stall"                 // 對帳迴圈本身很久沒被排到（整個程序卡住）
	HubClockJump                = "clock_jump"                 // 主機時鐘比程序的單調時鐘多走：睡著、被暫停、或時鐘被調過
	JobLeaseExpired             = "job_lease_expired"          // 工作單因 agent 沒回來而由 Hub 收成 lease_expired
	JobTimeout                  = "job_timeout"                // 工作單持續續租但超過 executor 的最壞預算
	HubDeploymentCreated        = "deployment_created"         // 人核准一張新的 deployment；desired/snapshot/first batch 同 tx
	HubDeploymentRetried        = "deployment_retried"         // 人針對終態失敗 targets 建立新 attempt；parent/child 同 tx
	HubDeploymentPaused         = "deployment_paused"          // 某一批出現失敗終態，Hub 停止開後續批次（沒有 auto-continue）
	HubDeploymentBoundaryPaused = "deployment_boundary_paused" // 下一批被 deterministic safety guard 擋住，原因在 detail 與 boundary ledger
	HubDeploymentFinished       = "deployment_finished"        // 已規劃的批次全部開完且最後一批全部 succeeded，或人 continue 之後沒有下一批
	HubDeploymentContinued      = "deployment_continued"       // 人按了 Continue next batch；stuck 的機器不重開
	HubDeploymentAbandoned      = "deployment_abandoned"       // 人明確放棄 paused deployment；未開批次永遠不再開，釋放資源 owner
)

type HubEvent struct {
	At     time.Time `json:"at"`
	Kind   string    `json:"kind"`
	Detail string    `json:"detail"`
}

// hubAliveKey 是 schema_meta 裡的鍵：對帳迴圈每一輪蓋一次的「我還活著」。
//
// ⚠ 它的用途只有一個：下次起來的時候算出「上一次活著是多久前」。
// 不要拿它當健康判定 —— 那正是 handleHealthz 註解說的自證。
const hubAliveKey = "hub_last_alive"

func (s *Store) RecordHubEvent(kind, detail string, at time.Time) error {
	if kind == "" || detail == "" {
		return fmt.Errorf("store: hub event needs kind and detail")
	}
	_, err := s.db.Exec(`INSERT INTO hub_events (at, kind, detail) VALUES (?, ?, ?)`,
		at.UTC().Format(time.RFC3339), kind, detail)
	if err != nil {
		return fmt.Errorf("store: record hub event: %w", err)
	}
	return nil
}

// HubEventsBetween 回 [from, to] 內的事件，時間由舊到新。
func (s *Store) HubEventsBetween(from, to time.Time) ([]HubEvent, error) {
	rows, err := s.db.Query(`
SELECT at, kind, detail FROM hub_events
 WHERE at >= ? AND at <= ?
 ORDER BY at ASC, event_id ASC`,
		from.UTC().Format(time.RFC3339), to.UTC().Format(time.RFC3339))
	if err != nil {
		return nil, fmt.Errorf("store: hub events: %w", err)
	}
	defer rows.Close()
	var out []HubEvent
	for rows.Next() {
		var e HubEvent
		var at string
		if err := rows.Scan(&at, &e.Kind, &e.Detail); err != nil {
			return nil, fmt.Errorf("store: scan hub event: %w", err)
		}
		e.At = parseTime(at)
		out = append(out, e)
	}
	return out, rows.Err()
}

// TouchHubAlive 蓋一次「我還活著」的章（對帳迴圈每一輪叫一次）。
func (s *Store) TouchHubAlive(now time.Time) error {
	_, err := s.db.Exec(`
INSERT INTO schema_meta (key, value) VALUES (?, ?)
ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		hubAliveKey, now.UTC().Format(time.RFC3339))
	if err != nil {
		return fmt.Errorf("store: touch hub alive: %w", err)
	}
	return nil
}

// LastHubAlive 回上一次蓋章的時間。沒有蓋過（第一次跑、或舊版寫的資料庫）回 false。
func (s *Store) LastHubAlive() (time.Time, bool, error) {
	var v string
	err := s.db.QueryRow(`SELECT value FROM schema_meta WHERE key = ?`, hubAliveKey).Scan(&v)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return time.Time{}, false, nil
		}
		return time.Time{}, false, fmt.Errorf("store: last hub alive: %w", err)
	}
	t := parseTime(v)
	return t, !t.IsZero(), nil
}
