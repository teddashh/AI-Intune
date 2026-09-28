package web

import (
	"encoding/csv"
	"html"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/store"
)

// enrollmentPageFleet 造出一台已納管、一台票過期沒用、一台已退役。
func enrollmentPageFleet(t *testing.T, st *store.Store) (arrived, expired, retired string) {
	t.Helper()
	arrived = onlineMachine(t, st, "samplehub1")
	id, _, err := st.CreateEnrollTokenFor("sampleagent1", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	expired = id
	if _, err := st.DB().Exec(`UPDATE enrollment_tokens SET expires_at=? WHERE used_by=?`,
		time.Now().UTC().Add(-time.Hour).Format("2006-01-02T15:04:05Z"), expired); err != nil {
		t.Fatal(err)
	}
	id, _, err = st.CreateEnrollTokenFor("old-box", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	retired = id
	if err := st.RetireMachine(retired, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	return arrived, expired, retired
}

func TestTheEnrollmentPageLeadsWithTheMachinesThatNeverArrived(t *testing.T) {
	s, st := newServer(t)
	_, expired, _ := enrollmentPageFleet(t, st)
	rec := webRequest(t, s, "/reports/enrollment")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := html.UnescapeString(rec.Body.String())
	owed := strings.Index(body, `id="owed"`)
	register := strings.Index(body, `id="register"`)
	if owed < 0 || register < 0 || owed > register {
		t.Fatalf("還沒到的那一節不在名冊全表前面：owed=%d register=%d", owed, register)
	}
	section := body[owed:register]
	if !strings.Contains(section, "sampleagent1") {
		t.Errorf("票過期沒用的機器不在「還沒到」那一節：\n%s", section)
	}
	if !strings.Contains(section, operator.EnrollmentStageNextStep(operator.EnrollmentExpired)) {
		t.Error("票過期那一列沒有講下一步")
	}
	if !strings.Contains(body, "/machines/"+expired) {
		t.Error("還沒到的機器沒有連到它自己那一頁")
	}
	// 已納管的機器不該混進「還沒到」那一節。
	if strings.Contains(section, "samplehub1") {
		t.Error("已納管的機器被列進「還沒到」")
	}
	if !strings.Contains(body, "離開分母只有一條路：退役") {
		t.Error("這一頁沒有講出名冊唯一的出口")
	}
}

// 摘要卡上的數字必須跟下面的表數得出來的一樣。
func TestTheEnrollmentSummaryMatchesTheRowsBelowIt(t *testing.T) {
	s, st := newServer(t)
	enrollmentPageFleet(t, st)
	body := html.UnescapeString(webRequest(t, s, "/reports/enrollment").Body.String())
	report, err := s.operator.EnrollmentReport(time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if report.Denominator == 0 || report.Owed == 0 || report.Retired == 0 {
		t.Fatalf("fixture 沒有造出該有的狀態：%+v", report)
	}
	for want, label := range map[string]string{
		metricValue(report.Denominator): "分母",
		metricValue(report.Owed):        "還沒到",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("摘要卡上沒有 %s 的 %s：\n%s", label, want, body)
		}
	}
	if want := "已退役 " + strconv.Itoa(report.Retired) + " 台，不算分母"; !strings.Contains(body, want) {
		t.Errorf("摘要卡沒有 %q", want)
	}
	for _, row := range report.Rows {
		if !strings.Contains(body, row.DisplayName) {
			t.Errorf("名冊全表少了 %s", row.DisplayName)
		}
	}
}

// 一個永遠是 0 的階段在畫面上只是雜訊。
func TestStagesWithNoMachinesAreNotListed(t *testing.T) {
	s, st := newServer(t)
	enrollmentPageFleet(t, st)
	report, err := s.operator.EnrollmentReport(time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	view := buildEnrollmentReportView(report)
	empty := 0
	for _, stage := range report.Stages {
		if stage.Count == 0 {
			empty++
		}
	}
	if empty == 0 {
		t.Fatal("每一個階段都有機器，這支測試沒有在測東西")
	}
	if len(view.Stages)+empty != len(report.Stages) {
		t.Errorf("版面列了 %d 個階段，實際有機器的是 %d 個", len(view.Stages), len(report.Stages)-empty)
	}
	for _, stage := range view.Stages {
		if stage.Count == 0 {
			t.Errorf("%s 一台都沒有，卻被列出來", stage.Stage)
		}
	}
}

func TestTheEnrollmentCSVExportsExactlyWhatIsOnThePage(t *testing.T) {
	s, st := newServer(t)
	enrollmentPageFleet(t, st)
	rec := webRequest(t, s, "/reports/enrollment.csv")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "text/csv; charset=utf-8" {
		t.Errorf("content-type=%q", got)
	}
	if got := rec.Header().Get("Content-Disposition"); !strings.Contains(got, "enrollment") {
		t.Errorf("content-disposition=%q", got)
	}
	rows, err := csv.NewReader(strings.NewReader(
		strings.TrimPrefix(rec.Body.String(), "\ufeff"))).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	report, err := s.operator.EnrollmentReport(time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != report.Registered+1 {
		t.Fatalf("CSV %d 列（含表頭），名冊上 %d 列", len(rows), report.Registered)
	}
	if rows[0][0] != "機器" || rows[0][len(rows[0])-1] != "下一步" {
		t.Fatalf("表頭=%v", rows[0])
	}
}

// 這份報告只講「現在」，沒有範圍好挑。
func TestTheEnrollmentPageRejectsAQueryItCannotHonour(t *testing.T) {
	s, st := newServer(t)
	enrollmentPageFleet(t, st)
	for _, target := range []string{
		"/reports/enrollment?days=7", "/reports/enrollment.csv?stage=expired",
	} {
		if rec := webRequest(t, s, target); rec.Code != http.StatusBadRequest {
			t.Errorf("%s status=%d，應該拒絕", target, rec.Code)
		}
	}
}
