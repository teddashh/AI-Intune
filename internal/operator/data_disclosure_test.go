package operator

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/state"
	"github.com/teddashh/AI-Intune/internal/store"
)

func disclosureAt(t *testing.T, policy store.RetentionPolicy) DataDisclosure {
	t.Helper()
	disclosure, err := DataDisclosureFor(policy, time.Date(2026, 9, 12, 9, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	return disclosure
}

func categoryOf(t *testing.T, disclosure DataDisclosure, key DataCategoryKey) DataCategory {
	t.Helper()
	for _, category := range disclosure.Categories {
		if category.Key == key {
			return category
		}
	}
	t.Fatalf("揭露面裡沒有 %s", key)
	return DataCategory{}
}

// 揭露面唯一真正危險的失效模式是「少講一張表」。store 那一層已經對著 SQLite 釘住
// 「哪些表存著機器的資料」；這一支釘的是下一步：那些表有沒有被分進某一類。一張
// 被量到卻沒有被歸類的表，在畫面上跟不存在一模一樣。
func TestEveryMeasuredTableLandsInExactlyOneCategory(t *testing.T) {
	disclosure := disclosureAt(t, store.DefaultRetention())
	placed := map[string]DataCategoryKey{}
	for _, category := range disclosure.Categories {
		for _, table := range category.Tables {
			if previous, seen := placed[table]; seen {
				t.Errorf("%s 同時被 %s 與 %s 講了", table, previous, category.Key)
			}
			placed[table] = category.Key
		}
	}
	for _, table := range store.MachineDataTables() {
		if _, known := placed[table.Table]; !known {
			t.Errorf("%s 被量了，但沒有任何一類講它留的是什麼", table.Table)
		}
		delete(placed, table.Table)
	}
	for table, key := range placed {
		t.Errorf("%s 被 %s 講了，但沒有人量它", table, key)
	}
	if disclosure.Tables != len(store.MachineDataTables()) {
		t.Errorf("揭露面說涵蓋 %d 張表，實際量了 %d 張",
			disclosure.Tables, len(store.MachineDataTables()))
	}
}

// 保留期是現行設定的函數，不是寫死的一句話。調短它，這一頁必須跟著改。
func TestTheDisclosedRetentionFollowsThePolicyInForce(t *testing.T) {
	policy := store.DefaultRetention()
	wide := categoryOf(t, disclosureAt(t, policy), DataCategoryObservations)
	if wide.Retention.Kind != DataRetentionTimed || wide.Retention.Days != 30 ||
		wide.Retention.Class != string(store.RetentionObservations) {
		t.Fatalf("預設保留期下的觀測=%+v", wide.Retention)
	}
	policy.Observations = 11 * 24 * time.Hour
	narrow := categoryOf(t, disclosureAt(t, policy), DataCategoryObservations)
	if narrow.Retention.Days != 11 {
		t.Fatalf("保留期縮短之後=%+v", narrow.Retention)
	}
	if !strings.Contains(narrow.RetentionSentence, "11") {
		t.Errorf("文案沒有跟著保留期走：%q", narrow.RetentionSentence)
	}
	if !strings.Contains(narrow.RetirementSentence, "自然到期") {
		t.Errorf("會被清掉的類別沒有講退役之後會怎樣：%q", narrow.RetirementSentence)
	}
}

// 名冊是分母，它不按時間清。一個把名冊講成「留 30 天」的揭露面，會讓人以為一台
// 一個月沒動的機器會自己從分母裡消失。
func TestWhatIsNeverPrunedSaysSo(t *testing.T) {
	disclosure := disclosureAt(t, store.DefaultRetention())
	for _, key := range []DataCategoryKey{DataCategoryRegistry, DataCategoryState, DataCategoryJobs} {
		category := categoryOf(t, disclosure, key)
		if category.Retention.Kind != DataRetentionKept || category.Retention.Days != 0 {
			t.Errorf("%s 說自己會被時間清：%+v", key, category.Retention)
		}
		if strings.Contains(category.RetentionSentence, "天") {
			t.Errorf("%s 的文案給了一個天數：%q", key, category.RetentionSentence)
		}
	}
	if disclosure.Timed != 4 {
		t.Errorf("會被時間清的類別有 %d 類，pruneJobs 管的是報到、觀測、票證與沉默失敗四張表", disclosure.Timed)
	}
}

// 被拒絕的請求只留固定筆數，那是真的會讓列消失的一條規則。不講出來，操作員會以為
// 稽核記錄是完整的。
func TestTheAuditCategoryDisclosesItsRing(t *testing.T) {
	audit := categoryOf(t, disclosureAt(t, store.DefaultRetention()), DataCategoryAudit)
	if audit.Retention.RingRows != store.OperatorDenialAuditLimit || audit.Retention.RingSubject == "" {
		t.Fatalf("稽核記錄沒有講被拒絕的請求那個上限：%+v", audit.Retention)
	}
	if !strings.Contains(audit.RetentionSentence, "1000") ||
		!strings.Contains(audit.RetentionSentence, audit.Retention.RingSubject) {
		t.Errorf("文案沒有講出那個上限：%q", audit.RetentionSentence)
	}
}

// 含自由文字的類別必須自己說出來。自由文字是這份揭露面唯一真正敏感的部分：其餘
// 每一格都是 Hub 自己的詞彙，長度與取值都受控。
func TestEveryCategorySaysWhetherItHoldsFreeText(t *testing.T) {
	disclosure := disclosureAt(t, store.DefaultRetention())
	withText := 0
	for _, category := range disclosure.Categories {
		if category.FreeTextSentence == "" {
			t.Errorf("%s 沒有交代有沒有自由文字", category.Key)
		}
		if category.FreeText == "" {
			if !strings.Contains(category.FreeTextSentence, "沒有自由文字") {
				t.Errorf("%s 沒有自由文字，文案卻是 %q", category.Key, category.FreeTextSentence)
			}
			continue
		}
		withText++
		if !strings.Contains(category.FreeTextSentence, category.FreeText) {
			t.Errorf("%s 的文案沒有講出那段自由文字是什麼：%q", category.Key, category.FreeTextSentence)
		}
	}
	if withText == 0 {
		t.Error("沒有任何一類講出自由文字，但 observed_state 的 payload 就是機器送上來的原文")
	}
	observations := categoryOf(t, disclosure, DataCategoryObservations)
	if !strings.Contains(observations.FreeText, "payload") {
		t.Errorf("觀測沒有講 payload 是機器的原文：%q", observations.FreeText)
	}
}

// 一個類別裡的表必須全部同一種待遇。半數被清、半數留著的類別只給得出一句保留期，
// 而那一句對其中一半是假的。
func TestACategoryCannotMixTwoRetentionRules(t *testing.T) {
	policy := store.DefaultRetention()
	for name, shape := range map[string]dataCategoryShape{
		"一半被清一半留著": {title: "混的", tables: []string{"observed_state", "machine_registry"}},
		"兩種保留期":    {title: "混的", tables: []string{"observed_state", "machine_checkins"}},
	} {
		if _, err := dataRetentionFor(shape, policy); !errors.Is(err, ErrInvalidDataDisclosure) {
			t.Errorf("%s：err=%v，這種類別不可以算得出一句保留期", name, err)
		}
	}
}

func TestTheDisclosureRefusesWhatItCannotAnswer(t *testing.T) {
	if _, err := DataDisclosureFor(store.DefaultRetention(), time.Time{}); !errors.Is(err, ErrInvalidDataDisclosure) {
		t.Errorf("沒有時刻也算得出來：%v", err)
	}
	broken := store.RetentionPolicy{Observations: time.Hour, Checkins: time.Hour, Occupancy: time.Hour}
	if _, err := DataDisclosureFor(broken, time.Now().UTC()); !errors.Is(err, ErrInvalidDataDisclosure) {
		t.Errorf("站不住的保留期也算得出來：%v", err)
	}
}

func TestMachineDataCountsWhatThisHubActuallyHolds(t *testing.T) {
	service, st, machineID, now := machineActionsFixture(t, "data-machine", enabledCheckin())
	if err := st.RecordStateTransition(machineID, state.Degraded, "磁碟快滿了", now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	result, err := service.MachineData(machineID, store.DefaultRetention(), now)
	if err != nil {
		t.Fatal(err)
	}
	if result.SchemaVersion != DataDisclosureSchemaVersion || result.MachineID != machineID ||
		result.DisplayName != "data-machine" || result.Retired || result.RetiredAt != nil {
		t.Fatalf("result=%+v", result)
	}
	if len(result.Categories) != len(DataCategoryKeys()) {
		t.Fatalf("categories=%d keys=%d", len(result.Categories), len(DataCategoryKeys()))
	}
	var summed int64
	for index, measured := range result.Categories {
		if measured.Category.Key != DataCategoryKeys()[index] {
			t.Fatalf("第 %d 類是 %s，宣告的順序是 %s",
				index, measured.Category.Key, DataCategoryKeys()[index])
		}
		summed += measured.Rows
		if measured.Category.Retention.Kind == DataRetentionTimed {
			if measured.CutoffAt == nil {
				t.Errorf("%s 會被時間清，卻沒有講現在的界線", measured.Category.Key)
				continue
			}
			want := now.UTC().Add(-time.Duration(measured.Category.Retention.Days) * 24 * time.Hour)
			if !measured.CutoffAt.Equal(want) {
				t.Errorf("%s 的界線=%v, want %v", measured.Category.Key, measured.CutoffAt, want)
			}
		} else if measured.CutoffAt != nil {
			t.Errorf("%s 不按時間清，卻給了一條界線 %v", measured.Category.Key, measured.CutoffAt)
		}
	}
	if summed != result.Rows {
		t.Fatalf("總列數 %d，逐類加起來是 %d", result.Rows, summed)
	}
	registry := measuredCategory(t, result, DataCategoryRegistry)
	if registry.Rows != 1 {
		t.Errorf("名冊 rows=%d, want 1", registry.Rows)
	}
	checkins := measuredCategory(t, result, DataCategoryCheckins)
	if checkins.Rows != 1 || checkins.Newest == nil {
		t.Errorf("報到=%+v, 這台剛剛報到過一次", checkins)
	}
	health := measuredCategory(t, result, DataCategoryState)
	if health.Rows < 2 {
		t.Errorf("狀態判定 rows=%d，歷史與轉移事件是兩張表", health.Rows)
	}
}

// 最舊 / 最新要跨得過同一類裡的每一張表。只讀其中一張，畫面就會說一段比實際短的
// 歷史，而哪一張表比較舊事先是不知道的。
func TestOneCategorySpansEveryTableItNames(t *testing.T) {
	service, st, machineID, now := machineActionsFixture(t, "data-span", enabledCheckin())
	retiredAt := now.Add(time.Minute).Truncate(time.Second)
	if err := st.RetireMachine(machineID, retiredAt); err != nil {
		t.Fatal(err)
	}
	// 憑證這一類的兩張表：狀態那張剛剛才寫，帳本那張刻意放在兩天前。宣告的順序是
	// 狀態在前，所以只讀第一張的話最舊會少算兩天。
	early := now.Add(-48 * time.Hour).Truncate(time.Second)
	if _, err := st.DB().Exec(
		`INSERT INTO credential_profile(profile_id, profile_name, provider, kind, created_at)
		 VALUES(?,?,?,?,?)`,
		"p1", "測試憑證", "openai", "api_key", fmtStoreTime(early)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(
		`INSERT INTO credential_on_machine(machine_id, provider, profile_id, status, updated_at)
		 VALUES(?,?,?,?,?)`,
		machineID, "openai", "p1", "present", fmtStoreTime(now)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(
		`INSERT INTO credential_ledger_event(event_id, profile_id, machine_id, provider,
		 event_type, occurred_at, received_at, evidence_ref, actor) VALUES(?,?,?,?,?,?,?,?,?)`,
		"e1", "p1", machineID, "openai", "assigned",
		fmtStoreTime(early), fmtStoreTime(early), "", "test"); err != nil {
		t.Fatal(err)
	}

	result, err := service.MachineData(machineID, store.DefaultRetention(), retiredAt.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if !result.Retired || result.RetiredAt == nil {
		t.Fatalf("已退役的機器沒有被講出來：%+v", result)
	}
	credentials := measuredCategory(t, result, DataCategoryCredentials)
	if credentials.Rows != 2 {
		t.Fatalf("憑證狀態 rows=%d, want 2 —— 狀態一列、帳本一列", credentials.Rows)
	}
	if credentials.Oldest == nil || !credentials.Oldest.Equal(early) {
		t.Fatalf("憑證狀態的最舊=%v, want %v —— 宣告在後面的那張表也算數",
			credentials.Oldest, early)
	}
	if credentials.Newest == nil || !credentials.Newest.Equal(now.Truncate(time.Second)) {
		t.Fatalf("憑證狀態的最新=%v, want %v", credentials.Newest, now.Truncate(time.Second))
	}
	registry := measuredCategory(t, result, DataCategoryRegistry)
	if registry.Newest == nil || !registry.Newest.Equal(retiredAt) {
		t.Fatalf("名冊的最新=%v, want %v —— 生命週期事件那張表也算數",
			registry.Newest, retiredAt)
	}
}

func fmtStoreTime(value time.Time) string { return value.UTC().Format(time.RFC3339) }

// 一列沒有 Hub 時刻的資料仍然被留著，逐類與總數都要算進去。不算的話，畫面上的
// 列數會比 Hub 手上真正有的少——而那正是揭露面不可以發生的方向。
func TestRowsWithoutAHubClockAreCountedAndCalledOut(t *testing.T) {
	service, st, machineID, now := machineActionsFixture(t, "data-undated", enabledCheckin())
	dated := now.Add(-time.Hour).Truncate(time.Second)
	if err := st.RecordStateTransition(machineID, state.Online, "心跳準時", dated); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(
		`INSERT INTO machine_state_history(machine_id, state, reason, entered_at) VALUES(?,?,?,?)`,
		machineID, "Degraded", "磁碟快滿了", ""); err != nil {
		t.Fatal(err)
	}
	result, err := service.MachineData(machineID, store.DefaultRetention(), now)
	if err != nil {
		t.Fatal(err)
	}
	// 狀態判定這一類有兩張表，而 append_state_transition_insert 會把歷史那一列
	// 原樣抄進轉移事件——包括它沒有時刻這件事。所以一列沒有時刻的歷史，在這一類
	// 裡是兩列。
	health := measuredCategory(t, result, DataCategoryState)
	if health.Undated != 2 {
		t.Fatalf("狀態判定 undated=%d, want 2", health.Undated)
	}
	if result.Undated != 2 {
		t.Fatalf("總計 undated=%d, want 2", result.Undated)
	}
	if health.Oldest == nil || !health.Oldest.Equal(dated) {
		t.Fatalf("狀態判定的最舊=%v, want %v —— 沒有時刻的那兩列不可以把時間範圍抹掉",
			health.Oldest, dated)
	}
}

// 一張被講了卻沒有人量的表，會讓那一類的列數少算而且沒有人知道。這個矛盾要停下來。
func TestACategoryCannotNameATableNobodyMeasures(t *testing.T) {
	category := DataCategory{
		Key: DataCategoryRegistry, Tables: []string{"machine_registry", "verifiers"},
	}
	_, err := machineDataCategory(category, map[string]store.MachineDataHolding{
		"machine_registry": {Table: "machine_registry", Rows: 1, Dated: 1},
	}, time.Now().UTC())
	if !errors.Is(err, ErrInvalidDataDisclosure) {
		t.Fatalf("err=%v，沒有人量的表不可以被靜靜跳過", err)
	}
}

func TestMachineDataRefusesWhatItCannotAnswer(t *testing.T) {
	service, _, machineID, now := machineActionsFixture(t, "data-refuse", enabledCheckin())
	for name, id := range map[string]string{"空的": "", "前後有空白": " " + machineID} {
		if _, err := service.MachineData(id, store.DefaultRetention(), now); !errors.Is(err, ErrInvalidDataDisclosure) {
			t.Errorf("%s 的 machine id 沒有被擋下：%v", name, err)
		}
	}
	if _, err := service.MachineData("不在名冊上", store.DefaultRetention(), now); err == nil {
		t.Error("不在名冊上的機器應該讀不出來")
	}
}

func TestMachineDataCSVExportsEveryCategory(t *testing.T) {
	service, _, machineID, now := machineActionsFixture(t, "data-csv", enabledCheckin())
	result, err := service.MachineData(machineID, store.DefaultRetention(), now)
	if err != nil {
		t.Fatal(err)
	}
	document := MachineDataCSV(result)
	if len(document.Rows) != len(result.Categories) {
		t.Fatalf("CSV 有 %d 列，畫面上有 %d 類", len(document.Rows), len(result.Categories))
	}
	text, err := ReportCSV(document)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"類別", "留多久", "退役之後", "自由文字", "名冊", "稽核記錄"} {
		if !strings.Contains(text, want) {
			t.Errorf("CSV 少了 %q：\n%s", want, text)
		}
	}
	if !strings.Contains(document.Filename, machineID) {
		t.Errorf("檔名沒有指出是哪一台：%q", document.Filename)
	}
}

func measuredCategory(t *testing.T, result MachineDataResult, key DataCategoryKey) MachineDataCategory {
	t.Helper()
	for _, measured := range result.Categories {
		if measured.Category.Key == key {
			return measured
		}
	}
	t.Fatalf("結果裡沒有 %s", key)
	return MachineDataCategory{}
}

func TestTheDisclosureSentencesSayTheseExactWords(t *testing.T) {
	t.Run("資料來源", func(t *testing.T) {
		for name, test := range map[string]struct {
			source DataSource
			want   string
		}{
			"機器":  {source: DataSourceMachine, want: "機器自己送上來的。"},
			"Hub": {source: DataSourceHub, want: "Hub 自己量到或判定的。"},
			"操作員": {source: DataSourceOperator, want: "操作員輸入或按下去留下的。"},
			"不認得": {source: DataSource("nope"), want: ""},
		} {
			if got := DataSourceSentence(test.source); got != test.want {
				t.Errorf("%s 的實際值=%q，期望值=%q", name, got, test.want)
			}
		}
	})

	t.Run("保留期", func(t *testing.T) {
		for name, test := range map[string]struct {
			retention DataRetention
			want      string
		}{
			"按時間清": {
				retention: DataRetention{Kind: DataRetentionTimed, Days: 90},
				want:      "留 90 天，每一組最新的那一列永遠留著。",
			},
			"不按時間清": {
				retention: DataRetention{Kind: DataRetentionKept},
				want:      "不按時間清。",
			},
			"按時間清且有固定筆數": {
				retention: DataRetention{Kind: DataRetentionTimed, Days: 90, RingRows: 200, RingSubject: "報到"},
				want:      "留 90 天，每一組最新的那一列永遠留著。報到只留最近 200 筆。",
			},
			"不按時間清且有固定筆數": {
				retention: DataRetention{Kind: DataRetentionKept, RingRows: 50, RingSubject: "事件"},
				want:      "不按時間清。事件只留最近 50 筆。",
			},
			"不認得": {
				retention: DataRetention{Kind: "nope", RingRows: 5, RingSubject: "x"},
				want:      "",
			},
		} {
			if got := DataRetentionSentence(test.retention); got != test.want {
				t.Errorf("%s 的實際值=%q，期望值=%q", name, got, test.want)
			}
		}
	})

	t.Run("退役", func(t *testing.T) {
		for name, test := range map[string]struct {
			retention DataRetention
			want      string
		}{
			"按時間清": {
				retention: DataRetention{Kind: DataRetentionTimed},
				want:      "退役不刪列，但不再有新的一列；現有的照上面那個天數自然到期。",
			},
			"不按時間清": {
				retention: DataRetention{Kind: DataRetentionKept},
				want:      "退役不刪列，也不再有新的一列；現有的留著。",
			},
			"不認得": {retention: DataRetention{Kind: "nope"}, want: ""},
		} {
			if got := DataRetirementSentence(test.retention); got != test.want {
				t.Errorf("%s 的實際值=%q，期望值=%q", name, got, test.want)
			}
		}
	})

	t.Run("自由文字", func(t *testing.T) {
		for name, test := range map[string]struct {
			freeText string
			want     string
		}{
			"沒有": {freeText: "", want: "沒有自由文字，每一格都是 Hub 自己的詞彙。"},
			"有":  {freeText: "名冊備註", want: "含自由文字：名冊備註"},
		} {
			if got := DataFreeTextSentence(test.freeText); got != test.want {
				t.Errorf("%s 的實際值=%q，期望值=%q", name, got, test.want)
			}
		}
	})
}

func TestEveryDisclosedCategoryCarriesThePinnedSentences(t *testing.T) {
	disclosure := disclosureAt(t, store.DefaultRetention())
	sourceSentences := map[DataSource]string{
		DataSourceMachine:  "機器自己送上來的。",
		DataSourceHub:      "Hub 自己量到或判定的。",
		DataSourceOperator: "操作員輸入或按下去留下的。",
	}
	retentionHeads := map[DataRetentionKind]string{
		DataRetentionTimed: "留 %d 天，每一組最新的那一列永遠留著。",
		DataRetentionKept:  "不按時間清。",
	}
	retirementSentences := map[DataRetentionKind]string{
		DataRetentionTimed: "退役不刪列，但不再有新的一列；現有的照上面那個天數自然到期。",
		DataRetentionKept:  "退役不刪列，也不再有新的一列；現有的留著。",
	}

	for _, category := range disclosure.Categories {
		category := category
		t.Run(string(category.Key), func(t *testing.T) {
			wantSource, knownSource := sourceSentences[category.Source]
			if !knownSource {
				t.Errorf("%s 這一類帶了一個沒有人逐字讀過的句子種類：資料來源的實際值=%q，期望值=測試表中已逐字讀過的種類", category.Key, category.Source)
			} else if category.SourceSentence != wantSource {
				t.Errorf("資料來源句子的實際值=%q，期望值=%q", category.SourceSentence, wantSource)
			}

			retentionHead, knownRetention := retentionHeads[category.Retention.Kind]
			wantRetirement, knownRetirement := retirementSentences[category.Retention.Kind]
			if !knownRetention || !knownRetirement {
				t.Errorf("%s 這一類帶了一個沒有人逐字讀過的句子種類：保留種類的實際值=%q，期望值=測試表中已逐字讀過的種類", category.Key, category.Retention.Kind)
			} else {
				wantRetention := retentionHead
				if category.Retention.Kind == DataRetentionTimed {
					wantRetention = fmt.Sprintf(retentionHead, category.Retention.Days)
				}
				if category.Retention.RingRows > 0 {
					wantRetention += fmt.Sprintf("%s只留最近 %d 筆。", category.Retention.RingSubject, category.Retention.RingRows)
				}
				if category.RetentionSentence != wantRetention {
					t.Errorf("保留期句子的實際值=%q，期望值=%q", category.RetentionSentence, wantRetention)
				}
				if category.RetirementSentence != wantRetirement {
					t.Errorf("退役句子的實際值=%q，期望值=%q", category.RetirementSentence, wantRetirement)
				}
			}

			wantFreeText := "沒有自由文字，每一格都是 Hub 自己的詞彙。"
			if category.FreeText != "" {
				wantFreeText = "含自由文字：" + category.FreeText
			}
			if category.FreeTextSentence != wantFreeText {
				t.Errorf("自由文字句子的實際值=%q，期望值=%q", category.FreeTextSentence, wantFreeText)
			}
		})
	}
}
