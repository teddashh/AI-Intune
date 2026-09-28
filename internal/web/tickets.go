package web

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/store"
)

type ticketPageView struct {
	Result         operator.TicketReadResult
	Rows           []ticketPageRow
	WindowOptions  []ticketWindowOption
	ExportHref     string
	ClearHref      string
	HasFilter      bool
	TotalRuns      int
	TotalErrorRuns int
}

type ticketPageRow struct {
	Item       operator.TicketProviderItem
	FilterHref string
}

type ticketWindowOption struct {
	Days     int
	Selected bool
}

func (s *Server) tickets(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	request, err := parseTicketPageRequest(r)
	if err != nil {
		http.Error(w, "票證使用量篩選不合法", http.StatusBadRequest)
		return
	}
	now := time.Now().UTC()
	result, err := s.operator.ListTicketsContext(r.Context(), request, now)
	if err != nil {
		if !writeTicketPageReadError(w, err) {
			s.fail(w, "讀取票證使用量失敗", err)
		}
		return
	}
	s.render(w, r, "tickets.html", page{
		Title: "票證使用量", Nav: "tickets", Now: now.Local().Format("2006-01-02 15:04"),
		Tickets: buildTicketPage(request, result),
	})
}

func (s *Server) ticketsCSV(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	request, err := parseTicketPageRequest(r)
	if err != nil {
		http.Error(w, "票證使用量篩選不合法", http.StatusBadRequest)
		return
	}
	result, err := s.operator.ListTicketsContext(r.Context(), request, time.Now().UTC())
	if err != nil {
		if !writeTicketPageReadError(w, err) {
			s.fail(w, "匯出票證使用量失敗", err)
		}
		return
	}
	s.writeReportCSV(w, operator.TicketCSV(result), "產生票證使用量 CSV 失敗")
}

func parseTicketPageRequest(r *http.Request) (operator.TicketReadRequest, error) {
	if r == nil || r.URL == nil || r.URL.ForceQuery && r.URL.RawQuery == "" {
		return operator.TicketReadRequest{}, errors.New("missing ticket URL")
	}
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return operator.TicketReadRequest{}, errors.New("invalid ticket query encoding")
	}
	for key := range values {
		if key != "days" && key != "provider_ref" {
			return operator.TicketReadRequest{}, fmt.Errorf("unknown ticket query %q", key)
		}
	}
	request := operator.TicketReadRequest{}
	if raw, present := values["days"]; present {
		if len(raw) != 1 || raw[0] == "" {
			return operator.TicketReadRequest{}, errors.New("days must appear once")
		}
		request.Days, err = strconv.Atoi(raw[0])
		if err != nil || strconv.Itoa(request.Days) != raw[0] || request.Days < 1 {
			return operator.TicketReadRequest{}, errors.New("days must be canonical")
		}
	}
	if raw, present := values["provider_ref"]; present {
		if len(raw) != 1 || raw[0] == "" {
			return operator.TicketReadRequest{}, errors.New("provider_ref must appear once")
		}
		request.ProviderRef = raw[0]
	}
	return operator.NormalizeTicketReadRequest(request)
}

func writeTicketPageReadError(w http.ResponseWriter, err error) bool {
	switch {
	case errors.Is(err, operator.ErrInvalidTicketRead), errors.Is(err, store.ErrInvalidTicketRead):
		http.Error(w, "票證使用量篩選不合法", http.StatusBadRequest)
	case errors.Is(err, store.ErrTicketReadTooBroad):
		http.Error(w, "票證使用量查詢超過安全上限；請縮短天數", http.StatusUnprocessableEntity)
	default:
		return false
	}
	return true
}

func buildTicketPage(request operator.TicketReadRequest, result operator.TicketReadResult) *ticketPageView {
	view := &ticketPageView{
		Result: result, HasFilter: request.ProviderRef != "", TotalRuns: result.TotalRuns(),
		TotalErrorRuns: result.TotalErrorRuns(),
		ExportHref:     ticketPageHref("/reports/tickets.csv", request),
		ClearHref:      ticketPageHref("/reports/tickets", operator.TicketReadRequest{Days: request.Days}),
	}
	for _, days := range []int{7, 14, 30} {
		view.WindowOptions = append(view.WindowOptions, ticketWindowOption{Days: days, Selected: days == request.Days})
	}
	for _, item := range result.Items {
		filtered := request
		filtered.ProviderRef = item.ProviderRef
		view.Rows = append(view.Rows, ticketPageRow{Item: item, FilterHref: ticketPageHref("/reports/tickets", filtered)})
	}
	return view
}

func ticketPageHref(path string, request operator.TicketReadRequest) string {
	query := make(url.Values)
	query.Set("days", strconv.Itoa(request.Days))
	if request.ProviderRef != "" {
		query.Set("provider_ref", request.ProviderRef)
	}
	return path + "?" + query.Encode()
}

// writeReportCSV 是這個 console 唯一一條把報告送出去的路徑。
//
// 檔名、BOM 與公式防護都由 operator.ReportCSV 決定，所以網頁下載到的檔案跟 CLI
// 匯出到的檔案是同一個檔案。
func (s *Server) writeReportCSV(w http.ResponseWriter, doc operator.ReportCSVDocument, failure string) {
	body, err := operator.ReportCSV(doc)
	if err != nil {
		s.fail(w, failure, err)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", operator.ReportCSVContentDisposition(doc))
	_, _ = w.Write([]byte(body))
}
