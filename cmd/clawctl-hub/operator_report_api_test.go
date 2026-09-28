package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/model"
	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/operatorauth"
	"github.com/teddashh/AI-Intune/internal/state"
	"github.com/teddashh/AI-Intune/internal/store"
)

// reportFixture 造出一台四個來源都有列的機器，而且每一列都落在現在這一刻附近——
// 時間軸讀的是真的時鐘，把事件釘在一個寫死的日期上，這些測試會在某一天自己壞掉。
func reportFixture(t *testing.T) (jobsFixture, time.Time) {
	t.Helper()
	f := newJobsFixture(t, "cnode-operator")
	now := time.Now().UTC().Truncate(time.Second)
	hubNow = func() time.Time { return now }
	(&hub{store: f.store, artifactsDir: f.artifactsDir, retention: store.DefaultRetention(),
		publicURL: "http://100.64.200.2:8787"}).
		operatorRoutes(f.mux)
	if err := f.store.RecordObservation(f.machine.id, model.ObservationBatch{
		SchemaVersion: model.SchemaVersion, MeasuredAt: now,
		OpenClaw: model.OpenClaw{Present: true},
	}, now); err != nil {
		t.Fatal(err)
	}
	jobsEnabled := true
	if err := f.store.RecordCheckin(f.machine.id, model.Checkin{
		SchemaVersion: model.SchemaVersion, SentAt: now, AgentVersion: "report-api-test",
		BootID: "boot-report", AgentSeq: 1, JobsEnabled: &jobsEnabled,
	}, now); err != nil {
		t.Fatal(err)
	}
	for _, transition := range []struct {
		at      time.Time
		verdict state.State
		reason  string
	}{
		{now.Add(-3 * time.Hour), state.Online, "心跳準時"},
		{now.Add(-2 * time.Hour), state.Degraded, "磁碟快滿了"},
	} {
		if err := f.store.RecordStateTransition(f.machine.id, transition.verdict,
			transition.reason, transition.at); err != nil {
			t.Fatal(err)
		}
	}
	startReportFixtureJob(t, f)
	return f, now
}

// startReportFixtureJob 透過 operator API 開一張診斷工作單，所以工作單與稽核那兩
// 個來源上的列，跟操作員自己按下去會留下的是同一種列。
func startReportFixtureJob(t *testing.T, f jobsFixture) {
	t.Helper()
	base := "/v1/operator/machines/" + f.machine.id
	previewRec := diagnosticOperatorRequest(t, f.mux, base+"/diagnostic-noop-preview", "",
		`{"execution_timeout_seconds":90}`)
	if previewRec.Code != http.StatusOK {
		t.Fatalf("preview=%d body=%s", previewRec.Code, previewRec.Body.String())
	}
	var preview store.OperatorDiagnosticNoopPreviewResult
	if err := json.Unmarshal(previewRec.Body.Bytes(), &preview); err != nil {
		t.Fatal(err)
	}
	created := diagnosticOperatorRequest(t, f.mux, base+"/diagnostic-noop-jobs", "report-fixture-key",
		fmt.Sprintf(`{"execution_timeout_seconds":90,"confirm_display_name":"cnode-operator","preview_digest":%q,"reason":"report fixture"}`,
			preview.PreviewDigest))
	if created.Code != http.StatusCreated {
		t.Fatalf("create=%d body=%s", created.Code, created.Body.String())
	}
}

func reportGet(t *testing.T, f jobsFixture, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.RemoteAddr = "100.64.0.7:41234"
	f.mux.ServeHTTP(rec, verifiedOperatorRequest(req, operatorauth.View))
	return rec
}

// 清單答的是「這個 Hub 做得出哪些報告、看得回去多遠」，而「多遠」讀的是這台 Hub
// 現行的保留期。印預設值的清單會在有人把保留期調短之後繼續承諾它給不出來的資料。
func TestOperatorReportsAPIAnswersFromThePolicyInForce(t *testing.T) {
	f, _ := reportFixture(t)
	rec := reportGet(t, f, "/v1/operator/reports")
	if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("status=%d headers=%v body=%s", rec.Code, rec.Header(), rec.Body.String())
	}
	var index operator.ReportIndex
	if err := json.Unmarshal(rec.Body.Bytes(), &index); err != nil {
		t.Fatal(err)
	}
	if index.SchemaVersion != operator.ReportIndexSchemaVersion ||
		index.Total != len(operator.ReportKinds()) || len(index.Entries) != index.Total {
		t.Fatalf("index=%+v", index)
	}
	changes := operator.ReportEntry{}
	for _, entry := range index.Entries {
		if entry.Kind == operator.ReportChanges {
			changes = entry
		}
	}
	if changes.Horizon.Kind != operator.ReportHorizonRetention || changes.Horizon.Days != 30 {
		t.Fatalf("預設保留期下的變更報告=%+v", changes.Horizon)
	}
}

// 保留期調短，清單上的「看得到多遠」必須跟著短。印預設值的清單會繼續承諾一份
// 這個 Hub 已經清掉的資料。
func TestOperatorReportsAPIFollowsAShortenedRetention(t *testing.T) {
	f := newJobsFixture(t, "cnode-retention")
	policy := store.DefaultRetention()
	policy.Observations = 10 * 24 * time.Hour
	(&hub{store: f.store, artifactsDir: f.artifactsDir, retention: policy}).operatorRoutes(f.mux)
	rec := reportGet(t, f, "/v1/operator/reports")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var index operator.ReportIndex
	if err := json.Unmarshal(rec.Body.Bytes(), &index); err != nil {
		t.Fatal(err)
	}
	for _, entry := range index.Entries {
		if entry.Kind != operator.ReportChanges {
			continue
		}
		if entry.Horizon.Kind != operator.ReportHorizonRetention || entry.Horizon.Days != 10 ||
			!strings.Contains(entry.HorizonSentence, "10") {
			t.Fatalf("保留期縮短之後的變更報告=%+v %q", entry.Horizon, entry.HorizonSentence)
		}
		return
	}
	t.Fatalf("清單上沒有變更報告：%+v", index.Entries)
}

func TestOperatorReportsAPIRefusesAQueryItDoesNotRead(t *testing.T) {
	f, _ := reportFixture(t)
	if rec := reportGet(t, f, "/v1/operator/reports?days=7"); rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

// 手動預覽讀的是正在跑的 Hub：它因此拿到跟排程送信同一個 public URL、expectations
// 與還原演練章。這個 GET 只產生本文，不可以吃掉今天尚未送出的那一則。
func TestOperatorDailyReportAPIRendersWithoutRecordingADelivery(t *testing.T) {
	f, now := reportFixture(t)
	evaluatedAt := now.Add(12 * time.Hour)
	previousNow := hubNow
	hubNow = func() time.Time { return evaluatedAt }
	t.Cleanup(func() { hubNow = previousNow })
	rec := reportGet(t, f, "/v1/operator/daily-report?since_seconds=3600")
	if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("status=%d headers=%v body=%s", rec.Code, rec.Header(), rec.Body.String())
	}
	var result operator.DailyReportResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.SchemaVersion != operator.DailyReportSchemaVersion ||
		!result.EvaluatedAt.Equal(evaluatedAt) || result.WindowSeconds != 3600 ||
		!result.Since.Equal(evaluatedAt.Add(-time.Hour)) ||
		!strings.Contains(result.Body, "http://100.64.200.2:8787/") ||
		!strings.HasSuffix(result.Body, "\n") {
		t.Fatalf("result=%+v", result)
	}
	var notificationRows int
	if err := f.store.DB().QueryRow("SELECT COUNT(*) FROM notifications").Scan(&notificationRows); err != nil || notificationRows != 0 {
		t.Fatalf("預覽寫了 notification：rows=%d err=%v", notificationRows, err)
	}
}

func TestOperatorDailyReportAPIDefaultsToOneDay(t *testing.T) {
	f, now := reportFixture(t)
	rec := reportGet(t, f, "/v1/operator/daily-report")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var result operator.DailyReportResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.WindowSeconds != int64(operator.DefaultDailyReportWindow/time.Second) ||
		!result.Since.Equal(now.Add(-operator.DefaultDailyReportWindow)) {
		t.Fatalf("result=%+v", result)
	}
}

func TestOperatorDailyReportAPIRefusesAnAmbiguousOrUnboundedWindow(t *testing.T) {
	f, _ := reportFixture(t)
	for name, path := range map[string]string{
		"空 query":        "/v1/operator/daily-report?",
		"unknown":        "/v1/operator/daily-report?days=1",
		"重複":             "/v1/operator/daily-report?since_seconds=1&since_seconds=2",
		"空白":             "/v1/operator/daily-report?since_seconds=%20",
		"零":              "/v1/operator/daily-report?since_seconds=0",
		"正號":             "/v1/operator/daily-report?since_seconds=%2B1",
		"前導零":            "/v1/operator/daily-report?since_seconds=01",
		"超過上限":           "/v1/operator/daily-report?since_seconds=2592001",
		"分號不是 query 分隔符": "/v1/operator/daily-report?since_seconds=1;days=1",
	} {
		t.Run(name, func(t *testing.T) {
			if rec := reportGet(t, f, path); rec.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestOperatorMachineTimelineAPIReturnsOneOrderedStory(t *testing.T) {
	f, now := reportFixture(t)
	rec := reportGet(t, f, "/v1/operator/machines/"+f.machine.id+"/timeline")
	if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("status=%d headers=%v body=%s", rec.Code, rec.Header(), rec.Body.String())
	}
	var result operator.MachineTimelineResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.MachineID != f.machine.id || result.DisplayName != "cnode-operator" ||
		result.Window.Days != operator.DefaultMachineTimelineDays ||
		len(result.Sources) != len(operator.MachineTimelineSources()) {
		t.Fatalf("result=%+v", result)
	}
	counted := 0
	for _, read := range result.Sources {
		counted += read.Count
	}
	if counted != result.Total || result.Total != len(result.Entries) || result.Total == 0 {
		t.Fatalf("各來源加起來 %d，總數 %d，清單 %d", counted, result.Total, len(result.Entries))
	}
	for index := 1; index < len(result.Entries); index++ {
		if result.Entries[index].At.After(result.Entries[index-1].At) {
			t.Fatalf("第 %d 列比它前面那一列新", index+1)
		}
	}
	if result.Window.To.Before(now) {
		t.Fatalf("窗在評估之前就結束：%v < %v", result.Window.To, now)
	}
}

func TestOperatorMachineTimelineAPIRefusesWhatItCannotStandBehind(t *testing.T) {
	f, _ := reportFixture(t)
	base := "/v1/operator/machines/" + f.machine.id + "/timeline"
	for name, test := range map[string]struct {
		path string
		code int
	}{
		"零天":     {base + "?days=0", http.StatusBadRequest},
		"超過上限":   {base + "?days=31", http.StatusBadRequest},
		"前導零":    {base + "?days=07", http.StatusBadRequest},
		"不是數字":   {base + "?days=week", http.StatusBadRequest},
		"重複出現":   {base + "?days=1&days=7", http.StatusBadRequest},
		"認不得的參數": {base + "?from=2026-09-01T00:00:00Z", http.StatusBadRequest},
		"不在名冊上":  {"/v1/operator/machines/" + strings.Repeat("0", 32) + "/timeline", http.StatusNotFound},
	} {
		t.Run(name, func(t *testing.T) {
			rec := reportGet(t, f, test.path)
			if rec.Code != test.code {
				t.Fatalf("status=%d，要 %d：%s", rec.Code, test.code, rec.Body.String())
			}
			if test.code == http.StatusNotFound &&
				!strings.Contains(rec.Body.String(), store.OperatorCodeMachineNotFound) {
				t.Fatalf("404 沒有帶 %s：%s", store.OperatorCodeMachineNotFound, rec.Body.String())
			}
		})
	}
}

// 每機安裝狀態走的是同一個 operator API：一份 no-store 的 JSON，而且不讀任何
// query——一個被默默忽略的 ?resource=openclaw，會讓人以為自己看的是過濾後的結果。
// ⚠ 這一頁不接受任何 query parameter。一個被默默忽略的 `?profile=` 會讓呼叫端以為
// 它拿到的是篩過的那一份——而它拿到的是全部，只是欄位少看了幾列。
func TestOperatorProfileReportAPIAnswersAndRefusesAQueryItDoesNotRead(t *testing.T) {
	f, _ := reportFixture(t)
	rec := reportGet(t, f, "/v1/operator/profile-report")
	if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("status=%d headers=%v body=%s", rec.Code, rec.Header(), rec.Body.String())
	}
	var report operator.ProfileReport
	if err := json.Unmarshal(rec.Body.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.SchemaVersion != operator.ProfileReportSchemaVersion ||
		report.Caveat != operator.ProfileReportCaveat ||
		len(report.States) != len(operator.ProfileStates()) ||
		len(report.PackageStates) != len(operator.ProfilePackageStates()) {
		t.Fatalf("report=%+v", report)
	}
	// 分母是名冊，跟同一個 Hub 上的每機安裝狀態必須是同一個數。
	var install operator.InstallReport
	if err := json.Unmarshal(reportGet(t, f, "/v1/operator/install-report").Body.Bytes(), &install); err != nil {
		t.Fatal(err)
	}
	if report.Machines != install.Machines {
		t.Errorf("同一個 Hub 上兩份報告的分母不一樣：發佈對照 %d、安裝狀態 %d",
			report.Machines, install.Machines)
	}
	if report.Wearing+report.Bare != report.Machines {
		t.Errorf("%d ＋ %d ≠ 分母 %d", report.Wearing, report.Bare, report.Machines)
	}
	if rec := reportGet(t, f, "/v1/operator/profile-report?profile=openclaw-standard"); rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestOperatorInstallReportAPIAnswersAndRefusesAQueryItDoesNotRead(t *testing.T) {
	f, _ := reportFixture(t)
	rec := reportGet(t, f, "/v1/operator/install-report")
	if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("status=%d headers=%v body=%s", rec.Code, rec.Header(), rec.Body.String())
	}
	var report operator.InstallReport
	if err := json.Unmarshal(rec.Body.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.SchemaVersion != operator.InstallReportSchemaVersion ||
		report.Caveat != operator.InstallReportCaveat ||
		len(report.States) != len(operator.InstallStates()) {
		t.Fatalf("report=%+v", report)
	}
	if rec := reportGet(t, f, "/v1/operator/install-report?resource=openclaw"); rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}
