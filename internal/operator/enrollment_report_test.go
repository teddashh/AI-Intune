package operator

import (
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/model"
	"github.com/teddashh/AI-Intune/internal/store"
)

type enrollmentFleet struct {
	store *store.Store
	now   time.Time
	ids   map[string]string
}

// enrolmentFixture 造出這個產品真正會遇到的每一種註冊狀態，包括正式環境現在就有的
// 那一種：開了票、票過期了、機器從來沒出現。
func enrollmentFixture(t *testing.T) enrollmentFleet {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	now := time.Now().UTC().Truncate(time.Second)
	fleet := enrollmentFleet{store: st, now: now, ids: map[string]string{}}

	issue := func(key, name string, ttl time.Duration) string {
		t.Helper()
		id, token, err := st.CreateEnrollTokenFor(name, ttl)
		if err != nil {
			t.Fatalf("開票 %s: %v", name, err)
		}
		fleet.ids[key] = id
		fleet.ids[key+"-token"] = token
		return token
	}

	// 已納管：票用掉了，也報到過。
	token := issue("arrived", "samplehub1", time.Hour)
	if _, _, err := st.RedeemEnrollToken(token, model.EnrollRequest{
		SchemaVersion: model.SchemaVersion, EnrollToken: token,
		Hostname: "samplehub1", UnixUser: "example-user", OS: "linux", Arch: "amd64",
	}, now.Add(-2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordCheckin(fleet.ids["arrived"], model.Checkin{
		SchemaVersion: model.SchemaVersion, SentAt: now.Add(-time.Minute),
		AgentVersion: "v-test", AgentStartedAt: now.Add(-time.Hour), AgentSeq: 3,
	}, now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}

	// 拿了憑證沒回來：票用掉了，從來沒報到。
	token = issue("credentialed", "sampleagent7", time.Hour)
	if _, _, err := st.RedeemEnrollToken(token, model.EnrollRequest{
		SchemaVersion: model.SchemaVersion, EnrollToken: token,
		Hostname: "sampleagent7", UnixUser: "example-user-b", OS: "linux", Arch: "arm64",
	}, now.Add(-90*time.Second)); err != nil {
		t.Fatal(err)
	}

	// 等它來：票還有效。
	issue("waiting", "sampleagent5", time.Hour)

	// 票過期沒用：正式環境的 sampleagent1 就是這一種。
	issue("expired", "sampleagent1", time.Hour)
	if _, err := st.DB().Exec(`UPDATE enrollment_tokens SET expires_at=? WHERE used_by=?`,
		now.Add(-time.Hour).Format("2006-01-02T15:04:05Z"), fleet.ids["expired"]); err != nil {
		t.Fatal(err)
	}

	// 沒有票可以用：票被撤銷了，名冊列照樣留著。
	issue("no-ticket", "sampleagent6", time.Hour)
	if _, err := st.RevokeEnrollToken(fleet.ids["no-ticket"]); err != nil {
		t.Fatal(err)
	}

	// 已退役。
	issue("retired", "old-box", time.Hour)
	if err := st.RetireMachine(fleet.ids["retired"], time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	// 宣告要納管永遠早於「它來了」。開票與兌換在測試裡是同一秒發生的，那個順序在
	// 正式環境走不到，也會讓「等了多久」變成 0。
	declared := now.Add(-3 * time.Hour).Format("2006-01-02T15:04:05Z")
	for _, statement := range []string{
		`UPDATE machine_registry SET created_at=?`,
		`UPDATE enrollment_tokens SET created_at=?`,
	} {
		if _, err := st.DB().Exec(statement, declared); err != nil {
			t.Fatal(err)
		}
	}
	return fleet
}

func (f enrollmentFleet) report(t *testing.T) EnrollmentReport {
	t.Helper()
	report, err := New(f.store).EnrollmentReport(f.now)
	if err != nil {
		t.Fatal(err)
	}
	return report
}

func (f enrollmentFleet) row(t *testing.T, report EnrollmentReport, key string) EnrollmentRow {
	t.Helper()
	for _, row := range report.Rows {
		if row.MachineID == f.ids[key] {
			return row
		}
	}
	t.Fatalf("報告裡沒有 %s", key)
	return EnrollmentRow{}
}

// 名冊上的每一列剛好落在一個階段。一列落不進任何階段，等於它在這份報告上不存在；
// 落進兩個階段，等於分母被算了兩次。
func TestEveryRegistryRowLandsInExactlyOneStage(t *testing.T) {
	fleet := enrollmentFixture(t)
	report := fleet.report(t)
	known := map[EnrollmentStage]bool{}
	for _, stage := range EnrollmentStages() {
		known[stage] = true
	}
	seen := map[string]int{}
	for _, row := range report.Rows {
		if !known[row.Stage] {
			t.Errorf("%s 的階段 %q 不在階段表裡", row.DisplayName, row.Stage)
		}
		seen[row.MachineID]++
	}
	for id, count := range seen {
		if count != 1 {
			t.Errorf("%s 出現 %d 次", id, count)
		}
	}
	if len(report.Rows) != report.Registered {
		t.Errorf("報告有 %d 列，卻說名冊上有 %d 列", len(report.Rows), report.Registered)
	}
	total := 0
	for _, stage := range report.Stages {
		total += stage.Count
	}
	if total != report.Registered {
		t.Errorf("逐階段加起來 %d 列，名冊上有 %d 列", total, report.Registered)
	}
}

// 每一種註冊狀態都要被講成它自己那一種。
func TestEachEnrollmentStateIsNamedForWhatItIs(t *testing.T) {
	fleet := enrollmentFixture(t)
	report := fleet.report(t)
	for key, want := range map[string]EnrollmentStage{
		"arrived":      EnrollmentArrived,
		"credentialed": EnrollmentCredentialed,
		"waiting":      EnrollmentWaiting,
		"expired":      EnrollmentExpired,
		"no-ticket":    EnrollmentNoTicket,
		"retired":      EnrollmentRetired,
	} {
		if got := fleet.row(t, report, key).Stage; got != want {
			t.Errorf("%s 被講成 %q，應該是 %q", key, got, want)
		}
	}
}

// 分母就是分母：退役的不算，其餘的每一台不是已到就是還沒到。
func TestTheDenominatorIsTheRegisterMinusRetirements(t *testing.T) {
	fleet := enrollmentFixture(t)
	report := fleet.report(t)
	if report.Registered != 6 || report.Retired != 1 || report.Denominator != 5 {
		t.Fatalf("名冊 %d 列、退役 %d、分母 %d", report.Registered, report.Retired, report.Denominator)
	}
	if report.Arrived != 1 || report.Owed != 4 {
		t.Errorf("已到 %d、還沒到 %d", report.Arrived, report.Owed)
	}
	if report.Arrived+report.Owed != report.Denominator {
		t.Errorf("已到加還沒到 %d，分母 %d", report.Arrived+report.Owed, report.Denominator)
	}
	if report.Denominator+report.Retired != report.Registered {
		t.Errorf("分母加退役 %d，名冊 %d", report.Denominator+report.Retired, report.Registered)
	}
}

// 一張票過期了沒有，讀的必須是單機頁在用的那條判準。這兩邊各寫一次 SQL 的話，
// 同一張票會在兩頁上一邊「還有效」一邊「已過期」。
func TestAnExpiredTicketReadsTheSameAsOnTheMachinePage(t *testing.T) {
	fleet := enrollmentFixture(t)
	report := fleet.report(t)
	for _, key := range []string{"expired", "waiting"} {
		row := fleet.row(t, report, key)
		pending, err := fleet.store.PendingEnrollToken(row.MachineID, fleet.now)
		if err != nil {
			t.Fatal(err)
		}
		if pending == nil {
			t.Fatalf("%s 沒有未用票", key)
		}
		wantExpired := row.Stage == EnrollmentExpired
		if pending.Expired != wantExpired {
			t.Errorf("%s：報告說過期=%v，單機頁說過期=%v", key, wantExpired, pending.Expired)
		}
		if row.TicketExpiresAt == nil || !row.TicketExpiresAt.Equal(pending.ExpiresAt.UTC()) {
			t.Errorf("%s 的到期時刻兩邊不一樣：%v vs %s", key, row.TicketExpiresAt, pending.ExpiresAt)
		}
	}
}

// 還沒來的排在前面。這份報告存在的理由就是「誰還沒來」，把已納管的排在最上面
// 等於把答案藏起來。
func TestTheMachinesThatNeverArrivedComeFirst(t *testing.T) {
	fleet := enrollmentFixture(t)
	report := fleet.report(t)
	arrivedIndex, owedIndex := -1, -1
	for index, row := range report.Rows {
		if row.Stage == EnrollmentArrived && arrivedIndex < 0 {
			arrivedIndex = index
		}
		if row.Stage == EnrollmentExpired && owedIndex < 0 {
			owedIndex = index
		}
	}
	if owedIndex < 0 || arrivedIndex < 0 {
		t.Fatalf("排序測不到：expired=%d arrived=%d", owedIndex, arrivedIndex)
	}
	if owedIndex > arrivedIndex {
		t.Errorf("已納管的 (%d) 排在票過期的 (%d) 前面", arrivedIndex, owedIndex)
	}
	if last := report.Rows[len(report.Rows)-1]; last.Stage != EnrollmentRetired {
		t.Errorf("退役的沒有排在最後：%q", last.Stage)
	}
}

// 每一個階段都要說得出它是什麼意思、下一步做什麼，而且分母那一格不可以自相矛盾。
func TestEveryStageSaysWhatItMeansAndWhatToDoNext(t *testing.T) {
	fleet := enrollmentFixture(t)
	report := fleet.report(t)
	for _, stage := range report.Stages {
		if stage.Title == "" || stage.Meaning == "" || stage.NextStep == "" {
			t.Errorf("%s 少了標題、說明或下一步：%+v", stage.Stage, stage)
		}
	}
	for _, row := range report.Rows {
		if row.StageTitle != EnrollmentStageTitle(row.Stage) ||
			row.Meaning != EnrollmentStageMeaning(row.Stage) ||
			row.NextStep != EnrollmentStageNextStep(row.Stage) {
			t.Errorf("%s 的句子跟階段表對不上：%+v", row.DisplayName, row)
		}
		if row.InDenominator != EnrollmentStageInDenominator(row.Stage) {
			t.Errorf("%s 的分母旗標跟階段表對不上", row.DisplayName)
		}
		if (row.RetiredAt != nil) == row.InDenominator {
			t.Errorf("%s 退役=%v 卻說算分母=%v", row.DisplayName, row.RetiredAt != nil, row.InDenominator)
		}
	}
	// 只有退役離開分母。這是這個產品的核心承諾：開票就進名冊，離開分母只有一條路。
	for _, stage := range EnrollmentStages() {
		if EnrollmentStageInDenominator(stage) == (stage == EnrollmentRetired) {
			t.Errorf("%s 的分母規則不對：只有退役才離開分母", stage)
		}
	}
}

// 報到過卻沒有兌換時刻，在協定上走不到。與其挑一個階段蓋過去，不如停下來——
// 一份把壞資料講成正常狀態的報告，比沒有報告更糟。
func TestAMachineThatReportedWithoutEnrollingIsRefused(t *testing.T) {
	fleet := enrollmentFixture(t)
	if _, err := fleet.store.DB().Exec(
		`UPDATE machine_registry SET enrolled_at=NULL WHERE machine_id=?`,
		fleet.ids["arrived"]); err != nil {
		t.Fatal(err)
	}
	_, err := New(fleet.store).EnrollmentReport(fleet.now)
	if !errors.Is(err, ErrInvalidEnrollmentReport) {
		t.Errorf("報到過但沒有兌換時刻，卻給了一份報告：%v", err)
	}
}

// 沒有可信的時鐘就答不出「票過期了沒有」。
func TestTheEnrollmentReportNeedsATrustedClock(t *testing.T) {
	fleet := enrollmentFixture(t)
	if _, err := New(fleet.store).EnrollmentReport(time.Time{}); !errors.Is(err, ErrInvalidEnrollmentReport) {
		t.Errorf("沒有評估時間也給了報告：%v", err)
	}
}

// 匯出的就是畫面上那些列，一列不多一列不少。
func TestTheEnrollmentCSVExportsExactlyTheRowsOnThePage(t *testing.T) {
	fleet := enrollmentFixture(t)
	report := fleet.report(t)
	document := EnrollmentReportCSV(report)
	if len(document.Rows) != len(report.Rows) {
		t.Fatalf("CSV %d 列，報告 %d 列", len(document.Rows), len(report.Rows))
	}
	raw, err := ReportCSV(document)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range report.Rows {
		if !strings.Contains(raw, row.MachineID) {
			t.Errorf("CSV 少了 %s", row.DisplayName)
		}
	}
	for index, row := range report.Rows {
		if document.Rows[index][0] != row.DisplayName ||
			document.Rows[index][2] != row.StageTitle {
			t.Errorf("第 %d 列對不上：%v", index, document.Rows[index])
		}
		if len(document.Rows[index]) != len(document.Columns) {
			t.Fatalf("第 %d 列有 %d 格，表頭有 %d 欄",
				index, len(document.Rows[index]), len(document.Columns))
		}
	}
}

// 等多久的起點是「有人說出要納管這台」，終點是「它真的來了」。把已納管的機器也算
// 到現在，那個數字只會一直長，而它要回答的是「那一次等了多久」。
func TestTheWaitIsMeasuredToTheMomentItArrived(t *testing.T) {
	fleet := enrollmentFixture(t)
	report, err := New(fleet.store).EnrollmentReport(fleet.now)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range report.Rows {
		switch row.Stage {
		case EnrollmentArrived:
			if row.EnrolledAt == nil {
				t.Fatalf("%s 已納管卻沒有兌換時刻", row.DisplayName)
			}
			if want := row.EnrolledAt.Sub(row.DeclaredAt); row.WaitedFor != want {
				t.Errorf("%s 等了 %s，宣告到兌換是 %s", row.DisplayName, row.WaitedFor, want)
			}
		case EnrollmentExpired, EnrollmentNoTicket, EnrollmentWaiting, EnrollmentCredentialed:
			if want := fleet.now.Sub(row.DeclaredAt); row.WaitedFor != want {
				t.Errorf("%s 還沒到，等的時間 %s 不是宣告到現在的 %s",
					row.DisplayName, row.WaitedFor, want)
			}
		}
	}
}

// 名冊就是分母，而一份少算的分母比沒有分母更糟：它看起來像是全部。讀不完就拒絕，
// 不截斷。
func TestARegisterTooLargeToReadIsRefusedNotTruncated(t *testing.T) {
	fleet := enrollmentFixture(t)
	for index := 0; index <= MaxEnrollmentReportMachines; index++ {
		if _, err := fleet.store.DB().Exec(`
INSERT INTO machine_registry (machine_id, display_name, expected, created_at)
VALUES (?,?,1,?)`, "bulk-"+strconv.Itoa(index), "bulk-"+strconv.Itoa(index),
			fleet.now.Format("2006-01-02T15:04:05Z")); err != nil {
			t.Fatal(err)
		}
	}
	_, err := New(fleet.store).EnrollmentReport(fleet.now)
	if !errors.Is(err, ErrInvalidEnrollmentReport) {
		t.Fatalf("名冊讀不完卻回了一份報告：%v", err)
	}
}
