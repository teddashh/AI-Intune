package store

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 拿**真的那個資料庫的複本**跑一次 prune。
//
// ⚠⚠ 這支測試存在的理由，是 `retention_test.go` 裡每一條都有的一個共同盲點：
// 它們的資料是自己造的。造資料的人跟寫 prune 的人是同一個，所以兩邊會共用
// 同一套誤解 —— 欄位長什麼樣、時間字串怎麼寫、哪些表有哪些索引。
// 一個只在自己造的資料上綠的刪除路徑，證明的是「它跟我的 fixture 一致」，
// 不是「它跟那台機器上那 13 MB 一致」。
//
// 而實機上這條路徑從來沒有真的刪過任何一列：資料庫比最短的保留期（14 天）
// 還年輕，所以每天都正確地回報 0 列。**一個沒有東西該清的清理程序，跟一個
// 壞掉的清理程序，在輸出上長得一模一樣。** 要等到 9/17 之後才會第一次動手，
// 而那時候沒有人會在旁邊看。
//
// 所以這裡把時鐘往前撥，用**真的資料**跑真的 DELETE。
//
// ⚠ 一律在複本上做。CLAWCTL_REAL_DB 指到誰都一樣 —— 下面第一件事就是複製，
// 而且之後只碰複本。真的那個檔案在這支測試裡是唯讀的。
//
// 沒有設 CLAWCTL_REAL_DB 就跳過：這是一支只有在有真資料的機器上才有意義的
// 測試，在 CI 或別人的機器上它沒有東西可驗。
//
//	CLAWCTL_REAL_DB=~/.local/share/clawctl/clawctl.sqlite go test ./internal/store -run RealDatabase -v
func TestPruneReallyDeletesFromARealDatabase(t *testing.T) {
	src := os.Getenv("CLAWCTL_REAL_DB")
	if src == "" {
		t.Skip("沒有設 CLAWCTL_REAL_DB —— 這支要有真的資料庫才有意義")
	}

	// ⚠ 連 -wal 一起複製。少了它，複本會少掉還沒 checkpoint 的那一段，
	// 而那正好是最新的資料 —— 於是「最新的一筆」會是錯的一筆，
	// 這支測試最想驗的那條「不刪最後一筆」規則就會驗到一個假的情境。
	dir := t.TempDir()
	dst := filepath.Join(dir, "copy.sqlite")
	for _, suffix := range []string{"", "-wal", "-shm"} {
		b, err := os.ReadFile(src + suffix)
		if err != nil {
			if suffix == "" {
				t.Fatalf("讀不到真的資料庫 %s：%v", src, err)
			}
			continue // -wal / -shm 不一定存在
		}
		if err := os.WriteFile(dst+suffix, b, 0o600); err != nil {
			t.Fatalf("複製 %s：%v", suffix, err)
		}
	}

	st, err := Open(dst)
	if err != nil {
		t.Fatalf("開複本：%v", err)
	}
	defer st.Close()

	count := func(table string) int64 {
		var n int64
		if err := st.db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
			t.Fatalf("數 %s：%v", table, err)
		}
		return n
	}
	// 每一台機器最新的那一筆心跳 —— prune 之後這些都必須還在。
	newestPerMachine := func() map[string]string {
		rows, err := st.db.Query(`
SELECT machine_id, MAX(received_at) FROM machine_checkins GROUP BY machine_id`)
		if err != nil {
			t.Fatalf("查每台最新心跳：%v", err)
		}
		defer rows.Close()
		out := map[string]string{}
		for rows.Next() {
			var id string
			var at sql.NullString
			if err := rows.Scan(&id, &at); err != nil {
				t.Fatalf("掃描：%v", err)
			}
			out[id] = at.String
		}
		return out
	}

	before := map[string]int64{}
	for _, j := range pruneJobs {
		before[j.table] = count(j.table)
	}
	newestBefore := newestPerMachine()
	machinesBefore := count("machine_registry")

	if before["machine_checkins"] == 0 {
		t.Skip("這個資料庫沒有心跳資料，驗不到什麼")
	}

	// ⚠ 用**真的保留政策**，把時鐘往前撥。改政策去逼出刪除，驗到的是一組
	// 沒有人會用的參數；改時間，驗到的才是 9/17 那天真的會發生的事。
	//
	// ⚠⚠ 要撥過**最長**的那個保留期，不是隨便一個。
	// 第一版撥的是 Observations(30d)+60d，跑起來很漂亮：兩張表刪了快兩萬列。
	// 但占用帳留 400 天，所以它一列都沒刪 —— 三條刪除路徑我只走了兩條，
	// 而測試照樣是綠的，因為當時只問了「有沒有刪到東西」。
	// 那正好是這個專案在講的那件事：一個綠燈只代表它問到的那部分沒事。
	p := DefaultRetention()
	longest := p.Observations
	for _, d := range []time.Duration{p.Checkins, p.Occupancy} {
		if d > longest {
			longest = d
		}
	}
	future := time.Now().UTC().Add(longest + 60*24*time.Hour)

	dry, err := st.Prune(future, p, true)
	if err != nil {
		t.Fatalf("預演：%v", err)
	}
	if dry.Total() == 0 {
		t.Fatalf("把時鐘撥到 %s，預演還是說 0 列 —— "+
			"要嘛這個資料庫是空的，要嘛刪除條件根本圈不到真實資料。"+
			"不管哪一種，實機上那個每天出現的 0 都不能再被當成好消息。",
			future.Format("2006-01-02"))
	}
	for _, j := range pruneJobs {
		if got := count(j.table); got != before[j.table] {
			t.Fatalf("預演動了 %s：%d → %d。預演不准改任何東西。",
				j.table, before[j.table], got)
		}
	}

	rep, err := st.Prune(future, p, false)
	if err != nil {
		t.Fatalf("真的刪：%v", err)
	}
	if rep.Total() != dry.Total() {
		t.Errorf("預演說 %d 列，真的刪掉 %d 列 —— "+
			"預演如果不準，它就不是預演", dry.Total(), rep.Total())
	}

	for _, c := range rep.Counts {
		got := count(c.Table)
		if want := before[c.Table] - c.Deleted; got != want {
			t.Errorf("%s：報告說刪了 %d 列，實際從 %d 變成 %d（應該是 %d）",
				c.Table, c.Deleted, before[c.Table], got, want)
		}
		// ⚠ 逐張表要求「真的刪到東西」，而不是全部加起來有就好。
		// 只問總數的話，一張表的刪除條件整個寫錯也會被另外兩張蓋過去。
		// 有資料卻一列都沒刪，代表那條路徑這次根本沒被走到。
		if before[c.Table] > 0 && c.Deleted == 0 {
			t.Errorf("%s 有 %d 列，把時鐘撥到 %s 之後卻一列都沒刪 —— "+
				"這條刪除路徑這次沒有被走到，它的綠燈是別張表給的",
				c.Table, before[c.Table], future.Format("2006-01-02"))
		}
		t.Logf("%-30s %6d → %6d（刪 %d，因為是最新一筆而留下 %d）",
			c.Table, before[c.Table], got, c.Deleted, c.Kept)
	}

	// ⚠⚠ 最重要的一條：清理不准讓任何一台機器從畫面上消失。
	// 「被清掉了」跟「從來沒有回報過」在 UI 上是同一句話，而後者會讓人
	// 去查一台其實好好的機器 —— 或者更糟，去查一台早就死了但看起來
	// 只是「還沒報到」的機器。
	if got := count("machine_registry"); got != machinesBefore {
		t.Errorf("machine_registry 從 %d 變成 %d —— 清理不該動到名冊", machinesBefore, got)
	}
	newestAfter := newestPerMachine()
	for id, at := range newestBefore {
		got, ok := newestAfter[id]
		if !ok {
			t.Errorf("機器 %s 的心跳被清光了 —— 它會變成「從未回報」", id)
			continue
		}
		if got != at {
			t.Errorf("機器 %s 最新的心跳從 %s 變成 %s —— "+
				"最新的那一筆不准被刪", id, at, got)
		}
	}

	// 留痕跡：沒有這一筆的話，「被清掉了」跟「從來沒有」長得一樣。
	at, rows, ok, err := st.LastPrune()
	if err != nil {
		t.Fatalf("LastPrune：%v", err)
	}
	if !ok {
		t.Error("刪完了卻查不到 retention_log —— 那筆痕跡就是這件事發生過的唯一證據")
	}
	if rows != rep.Total() {
		t.Errorf("retention_log 記 %d 列，報告說 %d 列", rows, rep.Total())
	}
	t.Logf("retention_log：%s 清了 %d 列", at.Format(time.RFC3339), rows)
}
