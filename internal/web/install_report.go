package web

// 每機安裝狀態那一頁 —— 「我叫它裝的那一個，跟我看到的一不一樣」。
//
// ⚠⚠ 這一頁跟軟體清查讀的是同一份名冊與同一份 FleetTools，但問的是不同的問題：
// 軟體清查問「機隊上有什麼」，這一頁問「我指派的東西怎麼樣了」。所以它不重算任何
// 一邊 —— 它把 operator.InstallReport 原樣攤開，view 只決定哪一段先出現。

import (
	"errors"
	"net/http"
	"time"

	"github.com/teddashh/AI-Intune/internal/operator"
)

type installReportView struct {
	Report     operator.InstallReport
	ExportHref string
	// Misattributed 是看到的版號量在一個沒在跑的檔案上的那幾格，攤成一列一格。
	//
	// ⚠⚠ 它排在 Differing 前面。那幾格的版號量的是沒在跑的那一份，所以下面「指派的
	// 比看到的舊」比的是一個沒有人在用的檔案——先看在比哪一個檔案，再談要不要回滾。
	Misattributed []installRuntimeCell
	// Differing 是指派跟看到明確不一樣的那幾個資源，單獨拿出來，因為這一頁存在的
	// 理由就是它們。
	Differing []operator.InstallResource
	// Unassigned 是還有機器從來沒被指派過的那幾個資源：那幾格不是「沒問題」，
	// 是「沒有人叫它裝」。
	Unassigned []operator.InstallResource
	// States 只留有格子的狀態：一個永遠是 0 的狀態在畫面上只是雜訊。
	States []operator.InstallStateCount
	// Runtimes 是「版號講的是哪一份」那七種各有幾格，七種一律列出來。
	//
	// ⚠ 這一軸跟 States 不一樣，0 的那幾種也要留著：「沒有一格是這樣」跟「這一頁不
	// 講這件事」是兩件事，而這一軸是這一頁最新的一句話。
	Runtimes []operator.ToolRuntimeCount
	// Matrix 是同一份報告的另一個形狀：一個資源一列、一台機器一欄。
	Matrix installMatrix
}

// installRuntimeCell 是攤平後的一格：哪一個資源在哪一台機器上的那一列。
//
// ⚠ 資源名字要跟著那一列走。一張只有機器名字的表答不出「是哪一個東西量錯了檔案」，
// 而一台機器上可以有好幾個資源。
type installRuntimeCell struct {
	Resource string
	Row      operator.InstallRow
}

// installMatrix 是資源 × 機器的安裝狀態表。
//
// ⚠ 欄位順序來自報告本身：每一個資源的 Rows 都是照同一份名冊、同一個順序展開的，
// 所以第 i 格永遠是同一台機器。自己再排一次等於多一個會跟報告分岔的地方。
type installMatrix struct {
	Labels   []string
	Rows     []installMatrixRow
	Machines int
	// Differing 是指派跟看到不一樣的資源數。0 的時候畫面要說「都對得起來」，而不是
	// 什麼都不說 —— 一張沒有結論的表看起來像壞掉。
	Differing int
	// Misattributed 是看到的版號量在一個沒在跑的檔案上的格數。
	//
	// ⚠⚠ 這張表是這一頁最多人只看一眼就走的地方。一格綠色的版號量在一個沒有人在跑的
	// 檔案上時，那一眼看到的是一個不成立的「對得起來」。
	Misattributed int
}

type installMatrixRow struct {
	Name            string
	AssignedOn      int
	MatchingOn      int
	DifferingOn     int
	MisattributedOn int
	Cells           []operator.InstallRow
}

// installReport 回答一句話：我叫它裝的那一個，跟我看到的一不一樣。
func (s *Server) installReport(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	report, ok := s.readInstallReport(w, r)
	if !ok {
		return
	}
	s.render(w, r, "install_report.html", page{
		Title: "每機安裝狀態", Nav: "install-report",
		Now:           time.Now().Local().Format("2006-01-02 15:04"),
		InstallReport: buildInstallReportView(report),
	})
}

// installReportCSV 匯出的就是畫面上那些列，一列不多一列不少。
func (s *Server) installReportCSV(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	report, ok := s.readInstallReport(w, r)
	if !ok {
		return
	}
	s.writeReportCSV(w, operator.InstallReportCSV(report), "產生每機安裝狀態 CSV 失敗")
}

func (s *Server) readInstallReport(w http.ResponseWriter, r *http.Request) (operator.InstallReport, bool) {
	if r.URL != nil && (r.URL.ForceQuery || r.URL.RawQuery != "") {
		http.Error(w, "每機安裝狀態不接受查詢參數", http.StatusBadRequest)
		return operator.InstallReport{}, false
	}
	report, err := s.operator.InstallReport(time.Now().UTC())
	if err != nil {
		if errors.Is(err, operator.ErrInvalidInstallReport) {
			http.Error(w, "機隊現在的狀態產不出每機安裝狀態。", http.StatusBadRequest)
			return operator.InstallReport{}, false
		}
		s.fail(w, "讀取每機安裝狀態失敗", err)
		return operator.InstallReport{}, false
	}
	return report, true
}

func buildInstallReportView(report operator.InstallReport) *installReportView {
	view := &installReportView{
		Report:     report,
		ExportHref: operator.InstallReportExportPath(),
		Matrix:     buildInstallMatrix(report),
	}
	for _, resource := range report.Resources {
		for _, row := range resource.Rows {
			if row.Runtime != nil && operator.ToolRuntimeMisattributed(row.Runtime.State) {
				view.Misattributed = append(view.Misattributed,
					installRuntimeCell{Resource: resource.Name, Row: row})
			}
		}
		if resource.DifferingOn > 0 {
			view.Differing = append(view.Differing, resource)
		}
		if resource.UnassignedOn > 0 {
			view.Unassigned = append(view.Unassigned, resource)
		}
	}
	for _, stateCount := range report.States {
		if stateCount.Count > 0 {
			view.States = append(view.States, stateCount)
		}
	}
	view.Runtimes = report.RuntimeStates
	return view
}

// buildInstallMatrix 把報告攤成資源 × 機器那張表。
//
// ⚠ 表頭取自第一個資源的那幾列，因為每一個資源都是照同一份名冊展開的：一台從來沒
// 被指派過任何東西的機器，在每一列上都有一格「沒有被指派過」。一個資源都沒有的時候
// 這張表是空的 —— 那時候畫面要講的是「還沒有任何一台被指派過任何東西」，不是一張
// 空表。
func buildInstallMatrix(report operator.InstallReport) installMatrix {
	matrix := installMatrix{Machines: report.Machines, Differing: report.Differing,
		Misattributed: report.Misattributed}
	if len(report.Resources) == 0 {
		return matrix
	}
	for _, row := range report.Resources[0].Rows {
		matrix.Labels = append(matrix.Labels, row.DisplayName)
	}
	for _, resource := range report.Resources {
		matrix.Rows = append(matrix.Rows, installMatrixRow{
			Name: resource.Name, AssignedOn: resource.AssignedOn,
			MatchingOn: resource.MatchingOn, DifferingOn: resource.DifferingOn,
			MisattributedOn: resource.MisattributedOn, Cells: resource.Rows,
		})
	}
	return matrix
}
