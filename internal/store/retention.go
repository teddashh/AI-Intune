package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// 清舊資料。
//
// ⚠⚠ 這個檔案在做一件跟這整個產品的主張**相反**的事：它刪證據。
//
// 所以它的每一條規則都是為了讓「刪掉」不要變成「從來不存在」：
//
//  1. **每一組的最新那一筆永遠不刪。**
//     這是唯一真正危險的失效模式。一台 40 天前死掉的機器，如果它最後
//     那筆觀測被清掉了，畫面上會從「40 天前最後一次回報」變成
//     「從來沒有回報過」—— 而那正好是 sampleagent3 隱形七週的那個 bug，
//     只是這次是我們自己動手做出來的。
//
//     ⚠ 「留最新一筆」不是「留最近 N 天」。一台停在 40 天前的機器，
//     在任何以時間為界的規則下都會被清光。分組要用
//     (machine_id, kind, subject)，跟 latestObservation 讀的那個 key 一樣。
//
//  2. **界線一定要比任何一個畫面讀的窗口長。**
//     讀窗口目前最長是 TicketWindow（7 天）。一個切進讀窗口的界線，
//     會讓畫面安靜地少講一段時間 —— 而它會說「這段時間沒事」。
//
//  3. **刪了要留痕跡。** retention_log 記下每一次清了什麼。
//     沒有這張表的話，「這台三十天前的資料被清掉了」跟
//     「這台三十天前沒有資料」在畫面上長得一模一樣（§5.7 / §5.9）。
//
//  4. **有些證據不准被時間 retention 動。**
//     machine_registry（名冊是分母）、一般 audit_log（人做過什麼）、
//     machine_state_history（「still broken since」的來源，而且它很小）。外部可製造的
//     operator-denied noise 另在寫入 transaction 內維持固定 ring；不由 Prune 按時間猜。
//
// ⚠ 為什麼需要它：實測（samplehub1，2026-09-03，10 小時的資料）
// 修好 withoutRawOccupancy 之後的成長率是 **4.5 MB/天 → 1.6 GB/年**，
// 而當下資料庫裡有 **7 MB（54%）是那個已經修掉的 bug 留下的**。
// **修好不等於清掉** —— 一個止血的修正不會癒合已經造成的傷口。

// RetentionPolicy 是每一類資料留多久。
//
// ⚠ 每一個數字後面都要有一個「它比誰長」的理由，不是取整數好看。
type RetentionPolicy struct {
	// Observations：observed_state。判決與單機頁只讀最新一筆，但 tickets.go:123
	// 的每小時 token 圖讀 TicketWindow（7 天）整段。30 天給了 4 倍餘裕，
	// 而多出來的那些是為了讓人回頭查「這台是什麼時候開始壞的」。
	Observations time.Duration

	// Checkins：machine_checkins。量最大的一張表（每 2 分鐘一筆）。
	// 讀窗口是 DetailCheckinWindow（24h）與 CrashLoopWindow（1h），
	// 14 天給了 14 倍的餘裕。
	Checkins time.Duration

	// Occupancy：ticket_occupancy_observation。這是**帳本**，不是觀測。
	// 讀窗口是 TicketWindow（7 天），但留 400 天是為了「去年這個月花了多少」——
	// 一本只記得七天的帳本答不出任何一個關於趨勢的問題。
	Occupancy time.Duration
}

// DefaultRetention 是預設值。
func DefaultRetention() RetentionPolicy {
	return RetentionPolicy{
		Observations: 30 * 24 * time.Hour,
		Checkins:     14 * 24 * time.Hour,
		Occupancy:    400 * 24 * time.Hour,
	}
}

// shortestReadWindow 是所有畫面裡最長的那個讀取窗口。
//
// ⚠ 這個常數存在的唯一理由是讓 Validate 有東西可以比。它必須跟著
// DeadSignalWindow / TicketWindow / DetailCheckinWindow 一起改 ——
// 而那正是為什麼有一支測試在掃這件事，不是靠人記得。
const longestReadWindow = TicketWindow

// Validate 檢查這個政策會不會切進某個畫面正在讀的範圍。
//
// ⚠ 回傳 error 而不是自己修正。一個會偷偷把你的設定改掉的函式，
// 會讓人以為他設的值生效了。
func (p RetentionPolicy) Validate() error {
	for _, c := range []struct {
		name string
		d    time.Duration
	}{
		{"observations", p.Observations},
		{"checkins", p.Checkins},
		{"occupancy", p.Occupancy},
	} {
		if c.d <= 0 {
			return fmt.Errorf("retention: %s retention period is %v; zero or negative would purge everything", c.name, c.d)
		}
		if c.d <= longestReadWindow {
			return fmt.Errorf(
				"retention: %s retains only %v, but a UI window reads data up to %v ago; "+
					"that view would silently drop part of the range and look as if nothing happened during that time",
				c.name, c.d, longestReadWindow)
		}
	}
	return nil
}

// pruneJob 是「一張表怎麼清」。
//
// ⚠ 做成 package 層的一張表，而不是散在 Prune 裡面，是為了讓測試可以走過
// 每一筆去問資料庫「這些欄位真的存在嗎」。一個欄名打錯的 prune，
// 在 SQLite 上是執行期才會炸的 —— 而它炸的時候，人正在等它刪東西。
type pruneJob struct {
	table string
	// cutCol 是圈時間用的欄位。**一律是 Hub 自己的鐘。**
	//
	// ⚠ 不可以用 measured_at。一台時鐘慢兩年的機器，它剛剛送上來的觀測
	// 在 measured_at 上一出生就過界，會在下一次 prune 被整批清掉，
	// 然後畫面上它變成「從來沒有回報過」。同樣的理由寫在
	// deadsignal.go:111 與 schema.sql 的 machine_checkins 註解上。
	cutCol string
	// newestCols 是所有「會被拿來排序出最新一筆」的時間欄。全部都要保護。
	newestCols []string
	// groupBy 是「一組」的定義，要跟讀取那一側的 GROUP BY / 索引一致。
	groupBy string
	// protectExpr, when set, replaces the generated "older than the newest row
	// in the group" predicate. It must be true for rows that are eligible to
	// delete once they are past the horizon. notifications uses it so only the
	// newest delivered=1 row per kind is kept; undelivered rows are not.
	protectExpr string
	// class 說這張表看哪一個保留期。
	//
	// ⚠ 寫成資料而不是一個 closure，是因為揭露面要回答「這張表留多久」，
	// 而它必須讀到**真的在刪東西的那一張表**。一個 closure 只能被呼叫，
	// 不能被問「你看的是哪一欄」——那會逼揭露面自己再抄一份對照表，
	// 然後兩份會漂。漂掉的樣子是產品承諾了一個它不會遵守的保留期。
	class RetentionClass
}

// RetentionClass 是保留期的三個類別之一。
type RetentionClass string

const (
	RetentionObservations RetentionClass = "observations"
	RetentionCheckins     RetentionClass = "checkins"
	RetentionOccupancy    RetentionClass = "occupancy"
)

func (c RetentionClass) horizon(p RetentionPolicy) time.Duration {
	switch c {
	case RetentionObservations:
		return p.Observations
	case RetentionCheckins:
		return p.Checkins
	case RetentionOccupancy:
		return p.Occupancy
	default:
		return 0
	}
}

// RetentionClassOf 說一張表會不會被時間清，會的話看哪一個保留期。
//
// 直接刪除的表讀 pruneJobs；以已驗證的 ON DELETE CASCADE 綁在父列上的表，讀下面
// 這張很短的從屬關係。後者沒有自己的保留期，也不能被講成永久保留。
func RetentionClassOf(table string) (RetentionClass, bool) {
	for _, job := range pruneJobs {
		if job.table == table {
			return job.class, true
		}
	}
	if table == "machine_job_capabilities" {
		return RetentionCheckins, true
	}
	return "", false
}

// RetentionHorizon 是一個類別在這份政策下的保留長度。
func RetentionHorizon(class RetentionClass, policy RetentionPolicy) (time.Duration, bool) {
	switch class {
	case RetentionObservations, RetentionCheckins, RetentionOccupancy:
		return class.horizon(policy), true
	default:
		return 0, false
	}
}

// ⚠ 這張表決定了「什麼會被刪」。加一張表進來之前，先回答兩個問題：
// 讀它的地方是不是只讀最新一筆？它的「一組」是什麼？答不出來就不要加。
//
// ⚠ 不在這張表裡的東西一列都不會動 —— 那是預設值，而且是對的預設值：
// machine_registry（名冊是分母）、audit_log（一般 control audit 永久保留；
// operator-denied 的固定 ring 在 insert 時處理，不是 retention job）、
// machine_state_history（「壞了多久」的來源）、credential_ledger_event（帳本）、
// workload_observation_evidence（canary soak 的完整 coverage；刪一格就可能洗綠）、
// enrollment_tokens（自己會過期）。
var pruneJobs = []pruneJob{{
	table:      "observed_state",
	cutCol:     "received_at",
	newestCols: []string{"measured_at", "received_at"},
	// 跟 ix_observed_recent 與 latestObservation 讀的 key 一樣。
	groupBy: "o2.machine_id = observed_state.machine_id AND o2.kind = observed_state.kind AND o2.subject = observed_state.subject",
	class:   RetentionObservations,
}, {
	table:      "machine_checkins",
	cutCol:     "received_at",
	newestCols: []string{"received_at"},
	// 一台機器一組。「最後一次回報」是每一台都必須答得出來的那一格。
	groupBy: "o2.machine_id = machine_checkins.machine_id",
	class:   RetentionCheckins,
}, {
	table:      "ticket_occupancy_observation",
	cutCol:     "received_at",
	newestCols: []string{"measured_at", "received_at"},
	// (機器, provider)：跟 ix_occupancy_window 一致。
	// ⚠ provider 是上游原文、刻意不正規化（見 schema.sql），所以
	// "openai" 與 "openai-codex" 在這裡是兩組 —— 那是對的，
	// 併起來會讓其中一邊的最後一筆失去保護。
	groupBy: "o2.machine_id = ticket_occupancy_observation.machine_id AND o2.provider = ticket_occupancy_observation.provider",
	class:   RetentionOccupancy,
}, {
	table:      "canary_silent_failures",
	cutCol:     "last_seen_at",
	newestCols: []string{"last_seen_at"},
	// 每台留最新一段：不能因清理把仍是最後證據的失敗洗成全綠。
	groupBy: "o2.machine_id = canary_silent_failures.machine_id",
	class:   RetentionObservations,
}, {
	table:      "notifications",
	cutCol:     "sent_at",
	newestCols: []string{"sent_at"},
	// 分組是 kind。真正留下的是該 kind 最新一筆 delivered=1，不是最新一筆失敗。
	// protectExpr 覆寫下面 where() 的預設「留同組最新一列」。
	groupBy: "o2.kind = notifications.kind",
	class:   RetentionObservations,
	protectExpr: `NOT (
  notifications.delivered = 1 AND notifications.sent_at = (
    SELECT MAX(n2.sent_at) FROM notifications n2
    WHERE n2.kind = notifications.kind AND n2.delivered = 1
  )
)`,
}}

// where 生出「該刪的」與「過界但留下來的」兩條 WHERE。
//
// ⚠ 抽成一個方法，是為了讓測試能拿到跟 Prune 真的跑的**同一個字串**去
// EXPLAIN。一支自己重打一遍 SQL 的效能測試，測的是它自己抄對了沒有。
func (j pruneJob) where() (del, kept string) {
	// protect：這一列在**每一個**會被拿來排序的時間欄上，都比同組的最大值嚴格小。
	//
	// ⚠ 「每一個」不是求全，是必要的。cutoff 圈的是 received_at（Hub 的鐘），
	// 畫面上「最新一筆」是 ORDER BY received_at DESC（Hub 的鐘）排出來的；
	// 一台時鐘歪掉的機器身上，這兩欄的最大值可能是**不同的列**，雙欄保護會
	// 保守地留下兩個鐘各自選中的列，避免清掉仍有調查價值的 agent 時鐘證據。
	//
	// ⚠ 用相關子查詢而不是 window function：「不刪最新那一筆」這件事要能
	// 直接從 SQL 讀出來。這句話一年跑三百次、每次都在刪東西，
	// 它的可讀性比它的執行速度重要 —— 而速度靠索引解決，不是靠改寫這句話
	// （ix_observed_prune / ix_occupancy_prune，理由寫在 schema.sql）。
	//
	// ⚠ 用 `<` 而不是 `<>`：跟最大值同時間的列會一起留下來。刻意的 ——
	// 寧可多留一筆，也不要在「同一秒有兩筆」這種邊角上把最後一筆刪掉。
	var keep string
	if j.protectExpr != "" {
		keep = j.protectExpr
	} else {
		protect := make([]string, 0, len(j.newestCols))
		for _, col := range j.newestCols {
			protect = append(protect, fmt.Sprintf(
				`%[1]s.%[2]s < (SELECT MAX(o2.%[2]s) FROM %[1]s o2 WHERE %[3]s)`,
				j.table, col, j.groupBy))
		}
		keep = strings.Join(protect, " AND ")
	}

	// ⚠ 兩條從同一個字串生出來，一個是另一個的 NOT。
	// 各自手寫的兩句 SQL 會慢慢漂開，然後「留了幾筆」就開始說謊。
	return fmt.Sprintf(`%s.%s < ? AND (%s)`, j.table, j.cutCol, keep),
		fmt.Sprintf(`%s.%s < ? AND NOT (%s)`, j.table, j.cutCol, keep)
}

// PruneCount 是一張表清掉了幾列、留了幾列。
type PruneCount struct {
	Table   string    `json:"table"`
	Deleted int64     `json:"deleted"`
	Kept    int64     `json:"kept_newest"`
	Older   time.Time `json:"older_than"`
}

// PruneReport 是一次清理的結果。
type PruneReport struct {
	At     time.Time    `json:"at"`
	DryRun bool         `json:"dry_run"`
	Counts []PruneCount `json:"counts"`
	// KeptNewest 是「因為是那一組最新的一筆而留下來」的列數。
	//
	// ⚠ 這個數字要印出來給人看，因為它是規則 1 到底有沒有在做事的證據。
	// 它是 0 的時候有兩種可能：沒有任何一組的最新筆落在界線之外（正常），
	// 或者那段邏輯壞了（不正常）—— 而印出來至少讓人有機會發現後者。
	KeptNewest int64 `json:"kept_newest"`
}

// Total 是總共刪了幾列。
func (r PruneReport) Total() int64 {
	var n int64
	for _, c := range r.Counts {
		n += c.Deleted
	}
	return n
}

// Prune 清掉超過保留期的資料。
//
// dryRun 為 true 時**只數不刪** —— 而且數的是同一條 WHERE，
// 不是另外寫一句近似的 SQL。兩句不一樣的 SQL 會讓預演跟真的做不一樣，
// 而預演的全部價值就在於它跟真的做一樣。
// pruneBatchSize is how many rows one writer transaction deletes from one table.
// Scheduled prune must not hold the single writer for one unbounded DELETE.
// The operator apply path still deletes inside its own idempotent transaction
// via pruneReportTx; that path confirms an exact row count and keeps the log
// in the same transaction.
const pruneBatchSize = 5000

func (s *Store) Prune(now time.Time, p RetentionPolicy, dryRun bool) (PruneReport, error) {
	if err := p.Validate(); err != nil {
		return PruneReport{}, err
	}
	now = now.UTC()
	if dryRun {
		return s.pruneCount(now, p)
	}
	return s.pruneBatched(now, p)
}

// pruneCount is the dry-run. It uses the reader pool and does not write
// retention_log. The WHERE is pruneJob.where, the same text the deletes use.
func (s *Store) pruneCount(now time.Time, p RetentionPolicy) (PruneReport, error) {
	tx, err := s.rdb.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return PruneReport{At: now, DryRun: true}, fmt.Errorf("store: begin prune: %w", err)
	}
	defer tx.Rollback()
	return pruneReportTx(tx, now, p, true)
}

func (s *Store) pruneBatched(now time.Time, p RetentionPolicy) (PruneReport, error) {
	counted, err := s.pruneCount(now, p)
	if err != nil {
		return counted, err
	}
	rep := PruneReport{At: now, DryRun: false}
	for i, j := range pruneJobs {
		kept := int64(0)
		if i < len(counted.Counts) {
			kept = counted.Counts[i].Kept
		}
		rep.KeptNewest += kept
		older := now.Add(-j.class.horizon(p))
		cut := fmtTime(older)
		del, _ := j.where()
		var deleted int64
		for batch := 0; ; batch++ {
			// Each batch deletes and writes its own retention_log row in one
			// writer transaction, so a reader never sees evidence gone without
			// the matching log row, and a log failure rolls the batch back.
			// The first batch also records kept_newest (and a zero-row
			// result), so every table still gets at least one row per prune.
			tx, err := s.beginWrite(context.Background(), "prune")
			if err != nil {
				return rep, fmt.Errorf("store: begin prune: %w", err)
			}
			res, err := tx.Exec(fmt.Sprintf(
				`DELETE FROM %s WHERE rowid IN (SELECT rowid FROM %s WHERE %s LIMIT %d)`,
				j.table, j.table, del, pruneBatchSize), cut)
			if err != nil {
				tx.Rollback()
				return rep, fmt.Errorf("store: prune %s: %w", j.table, err)
			}
			n, _ := res.RowsAffected()
			if n > 0 || batch == 0 {
				batchKept := int64(0)
				if batch == 0 {
					batchKept = kept
				}
				if err := writeRetentionLogTx(tx, PruneReport{At: now, Counts: []PruneCount{{
					Table: j.table, Deleted: n, Kept: batchKept, Older: older,
				}}}); err != nil {
					tx.Rollback()
					return rep, err
				}
			}
			if err := tx.Commit(); err != nil {
				return rep, fmt.Errorf("store: commit prune: %w", err)
			}
			deleted += n
			if n < int64(pruneBatchSize) {
				break
			}
		}
		rep.Counts = append(rep.Counts, PruneCount{
			Table: j.table, Deleted: deleted, Kept: kept, Older: older,
		})
	}
	return rep, nil
}

// pruneReportTx is the single count/delete implementation used by the
// scheduled worker and the canonical operator preview/apply transaction.  A
// preview and its eventual delete therefore cannot drift through duplicated
// WHERE clauses.
func pruneReportTx(tx dbTx, now time.Time, p RetentionPolicy, dryRun bool) (PruneReport, error) {
	rep := PruneReport{At: now.UTC(), DryRun: dryRun}
	for _, j := range pruneJobs {
		cut := fmtTime(rep.At.Add(-j.class.horizon(p)))
		del, kpt := j.where()

		// 先數「過界、但因為是那一組最新的一筆而留下來」的有幾列 —— 規則 1 的證據。
		var kept int64
		if err := tx.QueryRow(
			fmt.Sprintf(`SELECT COUNT(*) FROM %s WHERE %s`, j.table, kpt), cut).
			Scan(&kept); err != nil {
			return rep, fmt.Errorf("store: prune count retained %s: %w", j.table, err)
		}
		rep.KeptNewest += kept

		var n int64
		if dryRun {
			if err := tx.QueryRow(
				fmt.Sprintf(`SELECT COUNT(*) FROM %s WHERE %s`, j.table, del), cut).
				Scan(&n); err != nil {
				return rep, fmt.Errorf("store: prune dry run %s: %w", j.table, err)
			}
		} else {
			res, err := tx.Exec(
				fmt.Sprintf(`DELETE FROM %s WHERE %s`, j.table, del), cut)
			if err != nil {
				return rep, fmt.Errorf("store: prune %s: %w", j.table, err)
			}
			n, _ = res.RowsAffected()
		}
		rep.Counts = append(rep.Counts, PruneCount{
			Table: j.table, Deleted: n, Kept: kept, Older: rep.At.Add(-j.class.horizon(p)),
		})
	}
	return rep, nil
}

func writeRetentionLogTx(tx dbTx, rep PruneReport) error {
	// ⚠ 留痕跡。沒有這一筆的話，「被清掉了」跟「從來沒有」長得一樣。
	// DELETE、它的計數與每一筆 log 必須同一個 transaction。否則讀者能在
	// 「證據已消失、retention ceiling 尚未增加」的縫裡看見一份假完整歷史；
	// log 寫失敗更會永久失去「曾經刪過」的唯一證據。
	for _, c := range rep.Counts {
		if _, err := tx.Exec(`
INSERT INTO retention_log (at, table_name, rows_deleted, older_than, kept_newest)
VALUES (?,?,?,?,?)`,
			fmtTime(rep.At), c.Table, c.Deleted, fmtTime(c.Older), c.Kept); err != nil {
			return fmt.Errorf("store: retention log (purge rolled back): %w", err)
		}
	}
	return nil
}

// LastPrune 是最後一次清理的時間與清掉的總列數，沒清過就回 ok=false。
//
// ⚠ 畫面上要講出來。一個從來沒跑過的清理程序，跟一個「沒有東西該清」的
// 清理程序，在資料庫大小上長得一模一樣 —— 直到磁碟滿。
// ⚠⚠ 這個函式的第一版把「沒有紀錄」跟「查不動」寫成同一個回傳值
// （ok=false, err=nil），而它上面那行註解正好在講不可以這樣。
// 註解是對的，下面的程式把它講的那個區別自己抹掉了。
//
// 抓到它的是 web 那支「讀不到的時候不准說沒清過」的測試 ——
// 把 retention_log 這張表 DROP 掉之後，畫面很有自信地說
// 「這裡看到的就是這台機器的全部歷史」。這是同一類錯誤的第九次：
// 一個空的、乾淨的、沒有壞消息的答案，第一個要懷疑的是有沒有問對地方。
func (s *Store) LastPrune() (at time.Time, rows int64, ok bool, err error) {
	var atStr string
	e := s.rdb.QueryRow(`
SELECT at, SUM(rows_deleted) FROM retention_log
 WHERE at = (SELECT MAX(at) FROM retention_log) GROUP BY at`).Scan(&atStr, &rows)
	switch {
	case errors.Is(e, sql.ErrNoRows):
		// 真的沒清過。這不是錯誤，是一個要講出來的事實。
		return time.Time{}, 0, false, nil
	case e != nil:
		// ⚠ 表不見了、資料庫壞了、查詢寫錯了 —— 全部走這裡，而且要往上吼。
		// 這一條跟上面那一條在畫面上必須講不一樣的話。
		return time.Time{}, 0, false, fmt.Errorf("store: read retention log: %w", e)
	}
	return parseTime(atStr), rows, true, nil
}

// OldestObservation 是這台機器身上最舊的一筆觀測。
//
// ⚠ 單機頁要拿它跟保留期比：如果最舊的那筆剛好貼在界線上，
// 那一頁必須說「更早的已經清掉了」，否則人會把它讀成「這台是那天才出生的」。
// ⚠ 三種情況要分開，理由跟 LastPrune 一樣：
// 這台沒有任何觀測（NULL）、查不動（error）、真的有一筆。
// 前兩種在畫面上不是同一句話。
func (s *Store) OldestObservation(machineID string) (time.Time, bool, error) {
	var at sql.NullString
	if err := s.rdb.QueryRow(
		`SELECT MIN(measured_at) FROM observed_state WHERE machine_id = ?`,
		machineID).Scan(&at); err != nil {
		return time.Time{}, false, fmt.Errorf("store: read oldest observation: %w", err)
	}
	if !at.Valid || at.String == "" {
		// 這台身上一筆觀測都沒有。不是錯誤 —— 而且單機頁本來就會
		// 用「從來沒有送過心跳」那一句講這件事。
		return time.Time{}, false, nil
	}
	return parseTime(at.String), true, nil
}
