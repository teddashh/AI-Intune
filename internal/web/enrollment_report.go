package web

import (
	"errors"
	"net/http"
	"time"

	"github.com/teddashh/AI-Intune/internal/operator"
)

type enrollmentReportView struct {
	Report     operator.EnrollmentReport
	ExportHref string
	// Owed 是分母裡還沒到的那幾列，單獨拿出來，因為這份報告存在的理由就是它們。
	Owed []operator.EnrollmentRow
	// Stages 只留有機器的階段：一個永遠是 0 的階段在畫面上只是雜訊。
	Stages []operator.EnrollmentStageCount
}

// enrollmentReport 回答一句話：說好要納管的機器，來了沒有。
func (s *Server) enrollmentReport(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	report, ok := s.readEnrollmentReport(w, r)
	if !ok {
		return
	}
	s.render(w, r, "enrollment_report.html", page{
		Title: "註冊報告", Nav: "enrollment-report",
		Now:              time.Now().Local().Format("2006-01-02 15:04"),
		EnrollmentReport: buildEnrollmentReportView(report),
	})
}

// enrollmentReportCSV 匯出的就是畫面上那些列，一列不多一列不少。
func (s *Server) enrollmentReportCSV(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	report, ok := s.readEnrollmentReport(w, r)
	if !ok {
		return
	}
	s.writeReportCSV(w, operator.EnrollmentReportCSV(report), "產生註冊報告 CSV 失敗")
}

func (s *Server) readEnrollmentReport(w http.ResponseWriter, r *http.Request) (operator.EnrollmentReport, bool) {
	if r.URL != nil && (r.URL.ForceQuery || r.URL.RawQuery != "") {
		http.Error(w, "註冊報告不接受查詢參數", http.StatusBadRequest)
		return operator.EnrollmentReport{}, false
	}
	report, err := s.operator.EnrollmentReport(time.Now().UTC())
	if err != nil {
		if errors.Is(err, operator.ErrInvalidEnrollmentReport) {
			http.Error(w, "名冊現在的狀態產不出註冊報告。", http.StatusBadRequest)
			return operator.EnrollmentReport{}, false
		}
		s.fail(w, "讀取註冊報告失敗", err)
		return operator.EnrollmentReport{}, false
	}
	return report, true
}

func buildEnrollmentReportView(report operator.EnrollmentReport) *enrollmentReportView {
	view := &enrollmentReportView{
		Report:     report,
		ExportHref: operator.EnrollmentReportExportPath(),
	}
	for _, row := range report.Rows {
		if row.InDenominator && !operator.EnrollmentStageArrived(row.Stage) {
			view.Owed = append(view.Owed, row)
		}
	}
	for _, stage := range report.Stages {
		if stage.Count > 0 {
			view.Stages = append(view.Stages, stage)
		}
	}
	return view
}
