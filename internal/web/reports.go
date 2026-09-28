package web

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/teddashh/AI-Intune/internal/operator"
)

type reportIndexView struct {
	Index operator.ReportIndex
}

type machineTimelineView struct {
	Result        operator.MachineTimelineResult
	MachineHref   string
	ExportHref    string
	WindowOptions []timelineWindowOption
	Incomplete    []operator.MachineTimelineSourceRead
}

type timelineWindowOption struct {
	Days     int
	Href     string
	Selected bool
}

// reports 是報告的落地頁：每一份報告算的是什麼、看得到多遠、能不能把列帶走。
func (s *Server) reports(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.URL != nil && (r.URL.ForceQuery || r.URL.RawQuery != "") {
		http.Error(w, "報告清單不接受查詢參數", http.StatusBadRequest)
		return
	}
	now := time.Now().UTC()
	index, err := s.operator.ReportIndex(s.retentionPolicy, now)
	if err != nil {
		s.fail(w, "讀取報告清單失敗", err)
		return
	}
	s.render(w, r, "reports.html", page{
		Title: "報告", Nav: "reports", Now: now.Local().Format("2006-01-02 15:04"),
		Reports: &reportIndexView{Index: index},
	})
}

// machineTimeline 是單機事件時間軸：名冊、健康判定、工作單與操作員動作排成一條。
func (s *Server) machineTimeline(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	result, ok := s.readMachineTimeline(w, r)
	if !ok {
		return
	}
	s.render(w, r, "machine_timeline.html", page{
		Title: result.DisplayName + " 事件時間軸", Nav: "machines-detail",
		Now:             time.Now().Local().Format("2006-01-02 15:04"),
		MachineTimeline: buildMachineTimelineView(result),
	})
}

// machineTimelineCSV 匯出的就是畫面上那些列，一列不多一列不少。
func (s *Server) machineTimelineCSV(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	result, ok := s.readMachineTimeline(w, r)
	if !ok {
		return
	}
	s.writeReportCSV(w, operator.MachineTimelineCSV(result), "產生單機事件時間軸 CSV 失敗")
}

func (s *Server) readMachineTimeline(w http.ResponseWriter, r *http.Request) (operator.MachineTimelineResult, bool) {
	days, err := parseTimelineDays(r)
	if err != nil {
		http.Error(w, "事件時間軸範圍不合法", http.StatusBadRequest)
		return operator.MachineTimelineResult{}, false
	}
	result, err := s.operator.MachineTimeline(operator.MachineTimelineRequest{
		MachineID: r.PathValue("id"), Days: days,
	}, time.Now().UTC())
	if err != nil {
		if errors.Is(err, operator.ErrInvalidMachineTimeline) {
			http.Error(w, "事件時間軸範圍不合法", http.StatusBadRequest)
			return operator.MachineTimelineResult{}, false
		}
		http.Error(w, "名冊上沒有這台機器。", http.StatusNotFound)
		return operator.MachineTimelineResult{}, false
	}
	return result, true
}

func parseTimelineDays(r *http.Request) (int, error) {
	if r == nil || r.URL == nil {
		return 0, errors.New("missing timeline URL")
	}
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return 0, errors.New("invalid timeline query encoding")
	}
	for key := range values {
		if key != "days" && key != "section" {
			return 0, fmt.Errorf("unknown timeline query %q", key)
		}
	}
	raw, present := values["days"]
	if !present {
		return 0, nil
	}
	if len(raw) != 1 || raw[0] == "" {
		return 0, errors.New("days must appear once")
	}
	days, err := strconv.Atoi(raw[0])
	if err != nil || strconv.Itoa(days) != raw[0] || days < 1 {
		return 0, errors.New("days must be canonical")
	}
	return days, nil
}

func buildMachineTimelineView(result operator.MachineTimelineResult) *machineTimelineView {
	view := &machineTimelineView{
		Result:      result,
		MachineHref: "/machines/" + result.MachineID,
		ExportHref:  timelineHref(operator.MachineTimelineExportPath(result.MachineID), result.Window.Days),
	}
	for _, days := range []int{1, 7, operator.MaxMachineTimelineDays} {
		view.WindowOptions = append(view.WindowOptions, timelineWindowOption{
			Days:     days,
			Href:     timelineHref(operator.MachineTimelinePath(result.MachineID), days),
			Selected: days == result.Window.Days,
		})
	}
	for _, read := range result.Sources {
		if !read.Complete {
			view.Incomplete = append(view.Incomplete, read)
		}
	}
	return view
}

func timelineHref(path string, days int) string {
	return path + "?days=" + strconv.Itoa(days)
}
