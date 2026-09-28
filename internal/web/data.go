package web

import (
	"errors"
	"net/http"
	"time"

	"github.com/teddashh/AI-Intune/internal/operator"
)

type dataDisclosureView struct {
	Disclosure operator.DataDisclosure
}

type machineDataView struct {
	Result      operator.MachineDataResult
	MachineHref string
	ExportHref  string
	// Timed 是會被保留期清掉的類別，單獨列出來，因為只有這些類別的列會消失。
	Timed []operator.MachineDataCategory
	// Kept 是剩下那些不按時間清的類別。
	//
	// ⚠ 這一份是分出來的，不是在版面上寫「其餘 N 類」再填總數。摘要卡講的是
	// 「其餘」，而總數不是其餘——那句話會在畫面上印出一個比事實大的數字。
	Kept []operator.MachineDataCategory
	// FreeText 是含自由文字的類別，那是這一頁唯一真正敏感的部分。
	FreeText []operator.MachineDataCategory
}

// dataDisclosure 是租用戶層的揭露面：這個 Hub 對每一台機器留了什麼。
func (s *Server) dataDisclosure(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.URL != nil && (r.URL.ForceQuery || r.URL.RawQuery != "") {
		http.Error(w, "資料揭露面不接受查詢參數", http.StatusBadRequest)
		return
	}
	now := time.Now().UTC()
	disclosure, err := operator.DataDisclosureFor(s.retentionPolicy, now)
	if err != nil {
		s.fail(w, "讀取資料揭露面失敗", err)
		return
	}
	s.render(w, r, "data_disclosure.html", page{
		Title: "資料揭露", Nav: "tenant-data", Now: now.Local().Format("2006-01-02 15:04"),
		DataDisclosure: &dataDisclosureView{Disclosure: disclosure},
	})
}

// machineData 是單機的揭露面：Hub 現在替這一台留著多少列、最舊的一列有多舊。
func (s *Server) machineData(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	result, ok := s.readMachineData(w, r)
	if !ok {
		return
	}
	s.render(w, r, "machine_data.html", page{
		Title: result.DisplayName + " 的資料", Nav: "machines-detail",
		Now:         time.Now().Local().Format("2006-01-02 15:04"),
		MachineData: buildMachineDataView(result),
	})
}

// machineDataCSV 匯出的就是畫面上那些列，一列不多一列不少。
func (s *Server) machineDataCSV(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	result, ok := s.readMachineData(w, r)
	if !ok {
		return
	}
	s.writeReportCSV(w, operator.MachineDataCSV(result), "產生單機資料揭露 CSV 失敗")
}

func (s *Server) readMachineData(w http.ResponseWriter, r *http.Request) (operator.MachineDataResult, bool) {
	if r.URL != nil && (r.URL.ForceQuery || r.URL.RawQuery != "") {
		http.Error(w, "單機資料揭露面不接受查詢參數", http.StatusBadRequest)
		return operator.MachineDataResult{}, false
	}
	result, err := s.operator.MachineData(r.PathValue("id"), s.retentionPolicy, time.Now().UTC())
	if err != nil {
		if errors.Is(err, operator.ErrInvalidDataDisclosure) {
			http.Error(w, "單機資料揭露面請求不合法", http.StatusBadRequest)
			return operator.MachineDataResult{}, false
		}
		http.Error(w, "名冊上沒有這台機器。", http.StatusNotFound)
		return operator.MachineDataResult{}, false
	}
	return result, true
}

func buildMachineDataView(result operator.MachineDataResult) *machineDataView {
	view := &machineDataView{
		Result:      result,
		MachineHref: "/machines/" + result.MachineID,
		ExportHref:  operator.MachineDataExportPath(result.MachineID),
	}
	for _, measured := range result.Categories {
		if measured.Category.Retention.Kind == operator.DataRetentionTimed {
			view.Timed = append(view.Timed, measured)
		} else {
			view.Kept = append(view.Kept, measured)
		}
		if measured.Category.FreeText != "" {
			view.FreeText = append(view.FreeText, measured)
		}
	}
	return view
}
