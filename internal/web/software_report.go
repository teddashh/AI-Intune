package web

// 軟體清查那一頁 —— 「機隊上裝了什麼、各是哪一版、哪幾台沒有」。
//
// ⚠⚠ 這一頁跟總覽上那張版本分佈表讀的是同一份 operator.SoftwareReport。
// 兩邊各算一次的話，漂掉的樣子是同一台機器在兩頁上有兩個版本答案 —— 而那種
// 分岔沒有人會發現，因為兩頁不會同時開著。

import (
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/teddashh/AI-Intune/internal/operator"
)

type softwareReportView struct {
	Report     operator.SoftwareReport
	ExportHref string
	// Drifted 是機隊上版號不一致的那幾個工具，單獨拿出來，因為這一頁存在的理由
	// 就是它們。
	Drifted []operator.SoftwareTool
	// Missing 是「還沒回報過」不是 0 的那幾個工具：那幾格是沉默，不是答案。
	Missing []operator.SoftwareTool
	// Misattributed 是那幾格「版號講的不是正在跑的那一份」。
	//
	// ⚠⚠ 它排在版號不一致前面。這一頁其他每一個版號比較都假設那個版號講的是機隊上
	// 真的在跑的東西；這幾格的版號量的是沒在跑的那一份，所以先看它們，再談誰落後。
	Misattributed []softwareRuntimeCell
	// States 只留有格子的狀態：一個永遠是 0 的狀態在畫面上只是雜訊。
	States []operator.SoftwareStateCount
	// Runtimes 同樣只留有格子的那幾種「版號講的是哪一份」。
	Runtimes []operator.ToolRuntimeCount
	// Matrix 是同一份報告的另一個形狀：一個工具一列、一台機器一欄。
	Matrix softwareMatrix
}

// softwareRuntimeCell 是一格「版號講的不是正在跑的那一份」，連著它屬於哪一個工具。
//
// ⚠ 工具名要跟著一起出去。一張只有機器名與兩個路徑的表，讀的人沒有辦法知道該去
// 收掉的是哪一個工具的第二份安裝。
type softwareRuntimeCell struct {
	Tool string
	Row  operator.SoftwareRow
}

// softwareMatrix 是工具 × 機器的版本分佈表。
//
// ⚠ 欄位順序來自報告本身：每一個工具的 Rows 都是照同一份名冊、同一個順序展開的，
// 所以第 i 格永遠是同一台機器。自己再排一次等於多一個會跟報告分岔的地方。
type softwareMatrix struct {
	Labels   []string
	Rows     []softwareMatrixRow
	Machines int
	// Drifted 是有超過一種版號的工具數。0 的時候畫面要說「版號一致」，而不是
	// 什麼都不說 —— 一張沒有結論的表看起來像壞掉。
	Drifted int
	// Misattributed 是版號量的不是正在跑的那一份的格數。
	//
	// ⚠⚠ 它要跟著這張表一起到總覽上。總覽是最多人看的那一頁，一張「工具版本一致」
	// 的表配上一句「版本一致」，講的卻是一批沒有人在跑的檔案時，那一頁就變成了一個
	// 讓人放心的謊。
	Misattributed int
}

type softwareMatrixRow struct {
	Name string
	// Newest 是**這個機隊裡**最新的版號。比不出來時是空字串。
	Newest string
	Spread int
	Cells  []operator.SoftwareRow
}

// softwareReport 回答一句話：機隊上裝了什麼、各是哪一版、哪幾台沒有。
func (s *Server) softwareReport(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	report, ok := s.readSoftwareReport(w, r)
	if !ok {
		return
	}
	s.render(w, r, "software_report.html", page{
		Title: "軟體清查", Nav: "software-report",
		Now:            time.Now().Local().Format("2006-01-02 15:04"),
		SoftwareReport: buildSoftwareReportView(report),
	})
}

// softwareReportCSV 匯出的就是畫面上那些列，一列不多一列不少。
func (s *Server) softwareReportCSV(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	report, ok := s.readSoftwareReport(w, r)
	if !ok {
		return
	}
	s.writeReportCSV(w, operator.SoftwareReportCSV(report), "產生軟體清查 CSV 失敗")
}

func (s *Server) readSoftwareReport(w http.ResponseWriter, r *http.Request) (operator.SoftwareReport, bool) {
	if r.URL != nil && (r.URL.ForceQuery || r.URL.RawQuery != "") {
		http.Error(w, "軟體清查不接受查詢參數", http.StatusBadRequest)
		return operator.SoftwareReport{}, false
	}
	report, err := s.operator.SoftwareReport(time.Now().UTC())
	if err != nil {
		if errors.Is(err, operator.ErrInvalidSoftwareReport) {
			http.Error(w, "機隊現在的狀態產不出軟體清查。", http.StatusBadRequest)
			return operator.SoftwareReport{}, false
		}
		s.fail(w, "讀取軟體清查失敗", err)
		return operator.SoftwareReport{}, false
	}
	return report, true
}

func buildSoftwareReportView(report operator.SoftwareReport) *softwareReportView {
	view := &softwareReportView{
		Report:     report,
		ExportHref: operator.SoftwareReportExportPath(),
		Matrix:     buildSoftwareMatrix(report),
	}
	for _, tool := range report.Tools {
		if tool.Spread > 1 {
			view.Drifted = append(view.Drifted, tool)
		}
		if tool.UnreportedOn > 0 {
			view.Missing = append(view.Missing, tool)
		}
		for _, row := range tool.Rows {
			if row.Runtime != nil && operator.ToolRuntimeMisattributed(row.Runtime.State) {
				view.Misattributed = append(view.Misattributed,
					softwareRuntimeCell{Tool: tool.Name, Row: row})
			}
		}
	}
	for _, stateCount := range report.States {
		if stateCount.Count > 0 {
			view.States = append(view.States, stateCount)
		}
	}
	for _, stateCount := range report.RuntimeStates {
		if stateCount.Count > 0 {
			view.Runtimes = append(view.Runtimes, stateCount)
		}
	}
	return view
}

// buildSoftwareMatrix 把報告攤成總覽上那張表。
//
// ⚠ 表頭取自第一個工具的那幾列，因為每一個工具都是照同一份名冊展開的：一台從來
// 沒回報過任何東西的機器，在每一列上都有一格「沒回報過」。一個工具都沒有的時候
// 這張表是空的 —— 那時候畫面要講的是「還沒有任何一台回報過工具」，不是一張空表。
func buildSoftwareMatrix(report operator.SoftwareReport) softwareMatrix {
	matrix := softwareMatrix{
		Machines: report.Machines, Drifted: report.Drifted,
		Misattributed: report.Misattributed,
	}
	if len(report.Tools) == 0 {
		return matrix
	}
	for _, row := range report.Tools[0].Rows {
		matrix.Labels = append(matrix.Labels, row.DisplayName)
	}
	for _, tool := range report.Tools {
		matrix.Rows = append(matrix.Rows, softwareMatrixRow{
			Name: tool.Name, Newest: tool.Newest, Spread: tool.Spread, Cells: tool.Rows,
		})
	}
	return matrix
}

// softwareMatrix 組出總覽上那張版本分佈表。
//
// ⚠ 讀不到就回一張空表，讓樣板把那一段收起來 —— 不可以讓版本分佈把整個總覽打不開。
// 看不到機隊比看不到版本分佈嚴重得多。
func (s *Server) softwareMatrix(now time.Time) softwareMatrix {
	report, err := s.operator.SoftwareReport(now)
	if err != nil {
		log.Printf("failed to read full fleet software inventory (overview displays normally): %v", err)
		return softwareMatrix{}
	}
	return buildSoftwareMatrix(report)
}
