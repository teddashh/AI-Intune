package main

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/store"
)

func (h *hub) handleGetOperatorDailyReport(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	window, err := parseOperatorDailyReportWindow(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "daily-report 的 since_seconds 不合法")
		return
	}
	result, err := h.dailyReportAt(jobNow(), window)
	if err != nil {
		log.Printf("failed to read operator daily report: %v", err)
		writeErr(w, http.StatusInternalServerError, "INTERNAL", "產生每日早報失敗")
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *hub) dailyReportAt(now time.Time, window time.Duration) (operator.DailyReportResult, error) {
	var result operator.DailyReportResult
	if window < time.Second || window > operator.MaxDailyReportWindow || window%time.Second != 0 {
		return result, errors.New("invalid daily report window")
	}
	now = now.UTC().Truncate(time.Second)
	since := now.Add(-window)
	body, err := h.buildReport(now, since)
	if err != nil {
		return result, err
	}
	if body == "" || len(body) > operator.MaxDailyReportBodyBytes || !utf8.ValidString(body) ||
		!strings.HasSuffix(body, "\n") {
		return result, errors.New("daily report body is empty, exceeds limit, has invalid encoding, or is missing trailing newline")
	}
	for _, char := range body {
		if char != '\n' && (unicode.IsControl(char) || unicode.Is(unicode.Cf, char)) {
			return result, errors.New("daily report body contains control or formatting characters")
		}
	}
	return operator.DailyReportResult{
		SchemaVersion: operator.DailyReportSchemaVersion,
		EvaluatedAt:   now,
		Since:         since,
		WindowSeconds: int64(window / time.Second),
		Body:          body,
	}, nil
}

func parseOperatorDailyReportWindow(r *http.Request) (time.Duration, error) {
	if r == nil || r.URL == nil || r.URL.ForceQuery && r.URL.RawQuery == "" {
		return 0, errors.New("daily-report request 不完整")
	}
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return 0, errors.New("daily-report query 編碼不合法")
	}
	for key := range values {
		if key != "since_seconds" {
			return 0, fmt.Errorf("daily-report 不接受 query parameter %q", key)
		}
	}
	raw, present := values["since_seconds"]
	if !present {
		return operator.DefaultDailyReportWindow, nil
	}
	if len(raw) != 1 || !validOperatorChangeQueryText(raw[0], 7) {
		return 0, errors.New("since_seconds 必須只出現一次")
	}
	seconds, err := strconv.ParseInt(raw[0], 10, 64)
	if err != nil || strconv.FormatInt(seconds, 10) != raw[0] || seconds < 1 ||
		seconds > int64(operator.MaxDailyReportWindow/time.Second) {
		return 0, errors.New("since_seconds 超出範圍")
	}
	return time.Duration(seconds) * time.Second, nil
}

// handleGetOperatorReports lists every report this Hub can produce, how far
// back each one can still see, and which of them can be taken away whole.
//
// 它讀的是這個 Hub 現行的保留期，不是預設值：一個印著預設保留期的落地頁，會在
// 有人把保留期調短之後繼續承諾它給不出來的資料。
func (h *hub) handleGetOperatorReports(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.ForceQuery || r.URL.RawQuery != "" {
		writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "reports 不接受 query parameters")
		return
	}
	index, err := h.operatorReportService().ReportIndex(h.retention, time.Now().UTC())
	if err != nil {
		log.Printf("failed to read operator reports: %v", err)
		writeErr(w, http.StatusInternalServerError, "INTERNAL", "讀取報告清單失敗")
		return
	}
	writeJSON(w, http.StatusOK, index)
}

// handleGetOperatorEnrollmentReport says which of the machines this Hub was told
// to manage have actually turned up.
//
// 名冊就是分母，而分母是這個產品的第一個承諾：開一張票就是有人說出「我打算納管
// 這台」，從那一刻起它就算數。這一份把那些宣告攤開來，逐台說出它走到哪一步。
func (h *hub) handleGetOperatorEnrollmentReport(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.ForceQuery || r.URL.RawQuery != "" {
		writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "enrollment-report 不接受 query parameters")
		return
	}
	report, err := h.operatorReportService().EnrollmentReport(time.Now().UTC())
	if err != nil {
		if errors.Is(err, operator.ErrInvalidEnrollmentReport) {
			writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "名冊現在的狀態產不出註冊報告")
			return
		}
		log.Printf("failed to read operator enrollment report: %v", err)
		writeErr(w, http.StatusInternalServerError, "INTERNAL", "讀取註冊報告失敗")
		return
	}
	writeJSON(w, http.StatusOK, report)
}

// handleGetOperatorSoftwareReport says what is installed across the fleet.
//
// 它讀的是每一台最新一筆工具觀測，加上名冊上一台都還沒回報過的那幾台。⚠ 它講得出
// 口的只有「這個機隊裡最新的是哪一版」——這個 Hub 沒有上游的版本來源，所以那句限制
// 跟報告一起回，不是只寫在原始碼裡。
func (h *hub) handleGetOperatorSoftwareReport(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.ForceQuery || r.URL.RawQuery != "" {
		writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "software-report 不接受 query parameters")
		return
	}
	report, err := h.operatorReportService().SoftwareReport(time.Now().UTC())
	if err != nil {
		if errors.Is(err, operator.ErrInvalidSoftwareReport) {
			writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "機隊現在的狀態產不出軟體清查")
			return
		}
		log.Printf("failed to read operator software report: %v", err)
		writeErr(w, http.StatusInternalServerError, "INTERNAL", "讀取軟體清查失敗")
		return
	}
	writeJSON(w, http.StatusOK, report)
}

// handleGetOperatorInstallReport says how what this Hub assigned compares with
// what it sees on each machine.
//
// ⚠ 它只比兩件 Hub 自己持有的事實：最後一筆安裝意圖，與最新一筆觀測。它不知道機器
// 上的東西是從哪條路徑裝上去的，所以「指派的比看到的舊」是一個講得出口的狀態，
// 而那句限制跟報告一起回，不是只寫在原始碼裡。
func (h *hub) handleGetOperatorInstallReport(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.ForceQuery || r.URL.RawQuery != "" {
		writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "install-report 不接受 query parameters")
		return
	}
	report, err := h.operatorReportService().InstallReport(time.Now().UTC())
	if err != nil {
		if errors.Is(err, operator.ErrInvalidInstallReport) {
			writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "機隊現在的狀態產不出每機安裝狀態")
			return
		}
		log.Printf("failed to read operator install report: %v", err)
		writeErr(w, http.StatusInternalServerError, "INTERNAL", "讀取每機安裝狀態失敗")
		return
	}
	writeJSON(w, http.StatusOK, report)
}

// handleGetOperatorProfileReport says what happened to each published profile.
//
// ⚠ 它只講三件 Hub 自己持有的事：這份 profile 現在穿在幾台身上、它點名的版本這個
// Hub 有沒有指派過、有沒有在任何一台上看到過。它不講「那一版是不是最新」——那句話
// 需要一個上游版本來源，而這個 Hub 沒有，所以那句限制跟報告一起回。
func (h *hub) handleGetOperatorProfileReport(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.ForceQuery || r.URL.RawQuery != "" {
		writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "profile-report 不接受 query parameters")
		return
	}
	report, err := h.operatorReportService().ProfileReport(time.Now().UTC())
	if err != nil {
		if errors.Is(err, operator.ErrInvalidProfileReport) {
			writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "機隊現在的狀態產不出發佈與指派對照")
			return
		}
		log.Printf("failed to read operator profile report: %v", err)
		writeErr(w, http.StatusInternalServerError, "INTERNAL", "讀取發佈與指派對照失敗")
		return
	}
	writeJSON(w, http.StatusOK, report)
}

// handleGetOperatorMachineTimeline merges what the Hub itself recorded about
// one machine into one ordered story.
func (h *hub) handleGetOperatorMachineTimeline(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	machineID := r.PathValue("id")
	days, err := parseOperatorTimelineDays(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "時間軸 query 不合法")
		return
	}
	result, err := h.operatorReportService().MachineTimeline(
		operator.MachineTimelineRequest{MachineID: machineID, Days: days}, time.Now().UTC())
	if err != nil {
		writeOperatorMachineTimelineError(w, err, machineID)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *hub) operatorReportService() *operator.Service {
	if h.operatorService != nil {
		return h.operatorService
	}
	return operator.New(h.store)
}

func parseOperatorTimelineDays(r *http.Request) (int, error) {
	if r == nil || r.URL == nil || r.URL.ForceQuery && r.URL.RawQuery == "" {
		return 0, errors.New("時間軸 request 不完整")
	}
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return 0, errors.New("時間軸 query 編碼不合法")
	}
	for key := range values {
		if key != "days" {
			return 0, fmt.Errorf("時間軸不接受 query parameter %q", key)
		}
	}
	raw, present := values["days"]
	if !present {
		return 0, nil
	}
	if len(raw) != 1 || !validOperatorChangeQueryText(raw[0], 2) {
		return 0, errors.New("days 必須只出現一次")
	}
	days, err := strconv.Atoi(raw[0])
	if err != nil || strconv.Itoa(days) != raw[0] || days < 1 {
		return 0, errors.New("days 必須是 canonical 整數")
	}
	return days, nil
}

func writeOperatorMachineTimelineError(w http.ResponseWriter, err error, machineID string) {
	var rejection *store.OperatorRequestError
	switch {
	case errors.Is(err, operator.ErrInvalidMachineTimeline):
		writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "時間軸範圍不合法")
	case errors.Is(err, store.ErrNotFound):
		writeErr(w, http.StatusNotFound, store.OperatorCodeMachineNotFound, "找不到這台機器")
	case errors.As(err, &rejection) && rejection.Code == store.OperatorCodeMachineNotFound:
		writeErr(w, http.StatusNotFound, store.OperatorCodeMachineNotFound, "找不到這台機器")
	default:
		log.Printf("failed to read operator machine timeline machine=%q: %v", machineID, err)
		writeErr(w, http.StatusInternalServerError, "INTERNAL", "讀取單機事件時間軸失敗")
	}
}
