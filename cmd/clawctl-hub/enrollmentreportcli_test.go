package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/operator"
)

func enrollmentCLIFixture(t *testing.T) (string, machineCommandDeps) {
	t.Helper()
	f, _ := reportFixture(t)
	// 一台開了票、從來沒來的機器：正式環境現在就有這麼一列。
	if _, _, err := f.store.CreateEnrollTokenFor("never-came", 0); err != nil {
		t.Fatal(err)
	}
	return reportCLIServer(t, f)
}

func runEnrollmentCLI(t *testing.T, argv ...string) string {
	t.Helper()
	base, deps := enrollmentCLIFixture(t)
	var out, errOut bytes.Buffer
	if err := runEnrollmentReportCommandWithDeps(t.Context(),
		append([]string{"--hub-url", base}, argv...), &out, &errOut, deps); err != nil {
		t.Fatalf("err=%v errOut=%s", err, errOut.String())
	}
	return out.String()
}

// CLI 上的那一份要跟畫面回答同一句話：說好要納管的機器，來了沒有。少了下一步，
// 操作員看到一台「票過期沒用」只會再開一次瀏覽器才知道要重開票還是退役。
func TestEnrollmentCLIPrintsWhoNeverArrivedAndWhatToDoNext(t *testing.T) {
	text := runEnrollmentCLI(t)
	for _, want := range []string{
		"名冊上", "分母", "已退役", "有機器從來沒有報到過",
		"還沒到的", "名冊",
		"機器", "走到哪一步", "宣告納管", "票到期", "最後一次報到", "下一步",
		"從未報到", "never-came",
		operator.EnrollmentStageTitle(operator.EnrollmentWaiting),
		operator.EnrollmentStageNextStep(operator.EnrollmentWaiting),
		"離開分母只有一條路：退役。",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("輸出少了 %q：\n%s", want, text)
		}
	}
}

func TestEnrollmentCLIEmitsTheSameDocumentAsTheAPI(t *testing.T) {
	var report operator.EnrollmentReport
	body := runEnrollmentCLI(t, "--json")
	if err := json.Unmarshal([]byte(body), &report); err != nil {
		t.Fatalf("--json 不是合法 JSON：%v\n%s", err, body)
	}
	if report.SchemaVersion != operator.EnrollmentReportSchemaVersion ||
		report.Arrived+report.Owed != report.Denominator ||
		report.Denominator+report.Retired != report.Registered ||
		len(report.Rows) != report.Registered {
		t.Fatalf("report=%+v", report)
	}
}

func TestEnrollmentCLIExportsExactlyTheRowsItPrints(t *testing.T) {
	rows := reportCLIExportRows(t, runEnrollmentCLI(t, "--csv"))
	var report operator.EnrollmentReport
	if err := json.Unmarshal([]byte(runEnrollmentCLI(t, "--json")), &report); err != nil {
		t.Fatal(err)
	}
	if len(rows) != len(report.Rows)+1 {
		t.Fatalf("匯出 %d 列（含表頭），報告有 %d 列", len(rows), len(report.Rows))
	}
	columns := operator.EnrollmentReportCSV(report).Columns
	if len(rows[0]) != len(columns) {
		t.Fatalf("表頭 %d 欄，文件說 %d 欄", len(rows[0]), len(columns))
	}
	for index, column := range columns {
		if rows[0][index] != column.Header {
			t.Errorf("第 %d 欄是 %q，文件說 %q", index, rows[0][index], column.Header)
		}
	}
}

func TestEnrollmentCLIRefusesWhatItCannotRun(t *testing.T) {
	base, deps := enrollmentCLIFixture(t)
	for name, argv := range map[string][]string{
		"positional 參數":     {"--hub-url", base, "expired"},
		"認不得的 flag":         {"--hub-url", base, "--days", "7"},
		"--json 與 --csv 並用": {"--hub-url", base, "--json", "--csv"},
		"hub-url 裡有控制字元":    {"--hub-url", base + "\x00"},
	} {
		var out, errOut bytes.Buffer
		if err := runEnrollmentReportCommandWithDeps(t.Context(), argv,
			&out, &errOut, deps); err == nil {
			t.Errorf("%s：被接受了\n%s", name, out.String())
		}
	}
}

func enrollmentCLIOwedRow(name, nextStep string, declared time.Time) operator.EnrollmentRow {
	expires := declared.Add(2 * time.Hour)
	return operator.EnrollmentRow{
		MachineID: name, DisplayName: name,
		Stage:      operator.EnrollmentExpired,
		StageTitle: operator.EnrollmentStageTitle(operator.EnrollmentExpired),
		Meaning:    operator.EnrollmentStageMeaning(operator.EnrollmentExpired),
		NextStep:   nextStep, InDenominator: true,
		DeclaredAt: declared, TicketIssuedAt: &declared, TicketExpiresAt: &expires,
		PendingTickets: 1,
	}
}

// enrollmentCLILineFor 印一份兩列都還沒到的報告，回 short 那一列印出來的樣子。
func enrollmentCLILineFor(t *testing.T, otherNextStep string) string {
	t.Helper()
	declared := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	report := operator.EnrollmentReport{
		SchemaVersion: operator.EnrollmentReportSchemaVersion,
		EvaluatedAt:   declared.Add(72 * time.Hour),
		Registered:    2, Denominator: 2, Owed: 2,
		Rows: []operator.EnrollmentRow{
			enrollmentCLIOwedRow("short", "再開一張票。", declared),
			enrollmentCLIOwedRow("other", otherNextStep, declared),
		},
	}
	var out bytes.Buffer
	if err := writeEnrollmentReport(&out, report); err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(out.String(), "\n") {
		if strings.HasPrefix(line, "short") && strings.Contains(line, "再開一張票。") {
			return line
		}
	}
	t.Fatalf("輸出沒有 short 那一列：\n%s", out.String())
	return ""
}

// 「下一步」沒有長度上限。它一旦進了對齊的欄位，一台機器的長句子就會把每一列都
// 推寬——包含那些跟它無關的列。
func TestTheEnrollmentNextStepDoesNotWidenTheAlignedColumns(t *testing.T) {
	short := enrollmentCLILineFor(t, "退役就好。")
	long := enrollmentCLILineFor(t,
		strings.Repeat("還要納管就再開一張票；不打算納管了就退役，讓它離開分母。", 40))
	if short != long {
		t.Fatalf("另一列的長下一步把這一列推寬了：\n%q\n%q", short, long)
	}
}
