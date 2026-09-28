package web

// 發佈的 vs 指派的那一頁 —— 「這份 profile 發佈了，然後呢」。
//
// ⚠⚠ 每機安裝狀態那一頁的分母是機器，所以一份一台都沒指派的 profile 在那一頁上
// 完全不存在。這一頁的分母是**已發佈的 revision**，它存在的唯一理由就是把那幾列
// 畫出來。⚠ 它不重算任何一個數字 —— 它把 operator.ProfileReport 原樣攤開，view 只
// 決定哪一段先出現。

import (
	"errors"
	"net/http"
	"time"

	"github.com/teddashh/AI-Intune/internal/operator"
)

type profileReportView struct {
	Report     operator.ProfileReport
	ExportHref string
	// Unassigned 是發佈了卻一台都沒指派的那幾版，單獨拿出來，因為這一頁存在的
	// 理由就是它們。
	Unassigned []operator.ProfileRow
	// Unknown 是點名了一個這個 Hub 沒有指派過也沒有看到過的版本的那幾版。
	Unknown []profileUnknownPackage
	// Misattributed 是那幾格「看得到」，而看到的那個版號量在一份沒有人在跑的安裝上。
	//
	// ⚠⚠ 它排在 Unassigned 前面，跟報告那一句下一步同一個順序。「發佈了一台都沒指派」
	// 講的是還沒開始；這幾格講的是上面每一個「看得到」有多硬——一份看起來已經上去了的
	// profile 可能只是在機器的硬碟上放著。反過來排的話，操作員會先去指派新的。
	Misattributed []profileUnknownPackage
	// States / PackageStates 只留有列的狀態：一個永遠是 0 的狀態在畫面上只是雜訊。
	States        []operator.ProfileStateCount
	PackageStates []operator.ProfilePackageStateCount
}

// profileUnknownPackage 是一份 profile 點名的一格，連同它是哪一份 profile 點名的。
//
// ⚠ 它要帶著 profile 的身分，因為這一格要人做的事是去看那一份 profile —— 一個沒有
// 出處的版號，讀的人沒有辦法知道該去改哪裡。
//
// ⚠ 上面兩段各自挑出來的格子共用這一個形狀，不各自寫一份：兩段都是「一格 + 它的出處」，
// 而兩份形狀會讓其中一份哪天漏掉 revision，然後那一段就指不出該去改哪一份 profile。
type profileUnknownPackage struct {
	ProfileID string
	Revision  int64
	Package   operator.ProfilePackage
}

// profileReport 回答一句話：這份 profile 發佈了，然後呢。
func (s *Server) profileReport(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	report, ok := s.readProfileReport(w, r)
	if !ok {
		return
	}
	s.render(w, r, "profile_report.html", page{
		Title: "發佈與指派", Nav: "profile-report",
		Now:           time.Now().Local().Format("2006-01-02 15:04"),
		ProfileReport: buildProfileReportView(report),
	})
}

// profileReportCSV 匯出的就是畫面上那些列，一列不多一列不少。
func (s *Server) profileReportCSV(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	report, ok := s.readProfileReport(w, r)
	if !ok {
		return
	}
	s.writeReportCSV(w, operator.ProfileReportCSV(report), "產生發佈與指派對照 CSV 失敗")
}

func (s *Server) readProfileReport(w http.ResponseWriter, r *http.Request) (operator.ProfileReport, bool) {
	if r.URL != nil && (r.URL.ForceQuery || r.URL.RawQuery != "") {
		http.Error(w, "發佈與指派對照不接受查詢參數", http.StatusBadRequest)
		return operator.ProfileReport{}, false
	}
	report, err := s.operator.ProfileReport(time.Now().UTC())
	if err != nil {
		if errors.Is(err, operator.ErrInvalidProfileReport) {
			http.Error(w, "機隊現在的狀態產不出發佈與指派對照。", http.StatusBadRequest)
			return operator.ProfileReport{}, false
		}
		s.fail(w, "讀取發佈與指派對照失敗", err)
		return operator.ProfileReport{}, false
	}
	return report, true
}

func buildProfileReportView(report operator.ProfileReport) *profileReportView {
	view := &profileReportView{
		Report:     report,
		ExportHref: operator.ProfileReportExportPath(),
	}
	for _, row := range report.Profiles {
		if row.State == operator.ProfileUnassigned {
			view.Unassigned = append(view.Unassigned, row)
		}
		for _, pkg := range row.Packages {
			cell := profileUnknownPackage{
				ProfileID: row.ProfileID, Revision: row.Revision, Package: pkg,
			}
			if pkg.State == operator.ProfilePackageNeither {
				view.Unknown = append(view.Unknown, cell)
			}
			// ⚠ 這一個 if 跟上面那一個並列，不是它的 else。一格可以同時是「這個 Hub
			// 沒有指派過也沒有看到過」以外的任何一種狀態而量錯了檔案——最要緊的是
			// 「指派過這一版，機隊上也看得到」那一格，它是這一頁上最讓人放下心的一格。
			if pkg.SeenMisattributedOn > 0 {
				view.Misattributed = append(view.Misattributed, cell)
			}
		}
	}
	for _, count := range report.States {
		if count.Count > 0 {
			view.States = append(view.States, count)
		}
	}
	for _, count := range report.PackageStates {
		if count.Count > 0 {
			view.PackageStates = append(view.PackageStates, count)
		}
	}
	return view
}
