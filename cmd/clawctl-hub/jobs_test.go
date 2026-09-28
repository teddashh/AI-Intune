package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/agentadapter"
	"github.com/teddashh/AI-Intune/internal/compliance"
	"github.com/teddashh/AI-Intune/internal/deploy"
	"github.com/teddashh/AI-Intune/internal/model"
	"github.com/teddashh/AI-Intune/internal/store"
)

var jobsTestNow = time.Date(2026, 9, 6, 16, 0, 0, 0, time.UTC)

type jobsFixture struct {
	store        *store.Store
	mux          *http.ServeMux
	machine      enrolled
	artifactsDir string
}

func newJobsFixture(t *testing.T, name string) jobsFixture {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "hub.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("開啟測試資料庫失敗：%v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	oldNow := hubNow
	hubNow = func() time.Time { return jobsTestNow }
	t.Cleanup(func() { hubNow = oldNow })

	mux := http.NewServeMux()
	artifactsDir := artifactsDirFor(dbPath)
	(&hub{store: st, artifactsDir: artifactsDir}).machineAndPublicRoutes(mux)
	return jobsFixture{store: st, mux: mux, machine: enrollViaHTTP(t, mux, st, name), artifactsDir: artifactsDir}
}

func (f jobsFixture) newJob(t *testing.T, digest string) string {
	t.Helper()
	return f.newJobWithSpec(t, digest, `{}`)
}

func (f jobsFixture) newJobWithSpec(t *testing.T, digest, spec string) string {
	t.Helper()
	desiredID, rev, err := f.store.CreateDesiredState(
		"machine", f.machine.id, "openclaw", "openclaw", spec, "HTTP 測試",
	)
	if err != nil {
		t.Fatalf("建立測試期望狀態失敗：%v", err)
	}
	jobID, err := f.store.CreateJob(f.machine.id, desiredID, rev, store.NewJob{
		ArtifactDigest: digest,
	})
	if err != nil {
		t.Fatalf("建立測試工作單失敗：%v", err)
	}
	return jobID
}

func requestJobAPI(t *testing.T, mux *http.ServeMux, method, path, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var raw []byte
	if body != nil {
		var err error
		raw, err = json.Marshal(body)
		if err != nil {
			t.Fatalf("編碼測試 request 失敗：%v", err)
		}
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, bytes.NewReader(raw))
	req.Header.Set("Authorization", "Bearer "+token)
	mux.ServeHTTP(rec, req)
	return rec
}

func claimJobViaHTTP(t *testing.T, f jobsFixture, jobID string) model.JobLeaseResponse {
	t.Helper()
	rec := requestJobAPI(t, f.mux, http.MethodPost, "/v1/jobs/"+jobID+"/claims", f.machine.token, struct{}{})
	if rec.Code != http.StatusOK {
		t.Fatalf("領取工作單回應 %d，預期 200：%s", rec.Code, rec.Body.String())
	}
	var resp model.JobLeaseResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解碼領取回應失敗：%v", err)
	}
	if resp.LeaseToken == "" {
		t.Fatal("領取工作單後沒有拿到 lease token")
	}
	return resp
}

func assertAPIError(t *testing.T, rec *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("HTTP 狀態是 %d，預期 %d：%s", rec.Code, status, rec.Body.String())
	}
	var got model.APIError
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("解碼錯誤回應失敗：%v", err)
	}
	if got.Code != code {
		t.Fatalf("錯誤碼是 %q，預期 %q：%s", got.Code, code, rec.Body.String())
	}
}

// ⚠⚠ 守住只驗 job_id、不把 authed 交出的 machine_id 放進儲存層查詢，
// 讓 A 能領 B 的工作單；也守住用錯誤差異列舉 job_id 的錯。
func TestClaimJobHidesOtherMachinesJobLikeMissingJob(t *testing.T) {
	f := newJobsFixture(t, "machine-a")
	other := enrollViaHTTP(t, f.mux, f.store, "machine-b")
	desiredID, rev, err := f.store.CreateDesiredState("machine", other.id, "openclaw", "openclaw", `{}`, "HTTP 測試")
	if err != nil {
		t.Fatalf("替 B 建立期望狀態失敗：%v", err)
	}
	jobID, err := f.store.CreateJob(other.id, desiredID, rev, store.NewJob{ArtifactDigest: "sha256:b"})
	if err != nil {
		t.Fatalf("替 B 建立工作單失敗：%v", err)
	}

	wrongOwner := requestJobAPI(t, f.mux, http.MethodPost, "/v1/jobs/"+jobID+"/claims", f.machine.token, struct{}{})
	missing := requestJobAPI(t, f.mux, http.MethodPost, "/v1/jobs/不存在/claims", f.machine.token, struct{}{})
	assertAPIError(t, wrongOwner, http.StatusNotFound, model.ErrJobNotFound)
	assertAPIError(t, missing, http.StatusNotFound, model.ErrJobNotFound)
	if wrongOwner.Body.String() != missing.Body.String() {
		t.Fatalf("歸屬不符與不存在的回應不同：\n歸屬不符：%s不存在：%s", wrongOwner.Body.String(), missing.Body.String())
	}
}

// ⚠⚠ 守住安靜忽略 body 的 machine_id，讓送出者誤信身分宣告生效的錯；
// 拒絕必須發生在任何工作單變更之前。
func TestJobRequestRejectsMachineIDWithoutChangingJob(t *testing.T) {
	f := newJobsFixture(t, "machine-a")
	jobID := f.newJob(t, "sha256:a")
	rec := requestJobAPI(t, f.mux, http.MethodPost, "/v1/jobs/"+jobID+"/claims", f.machine.token,
		map[string]any{"machine_id": nil})
	assertAPIError(t, rec, http.StatusBadRequest, "BAD_REQUEST")
	job, err := f.store.JobForMachine(jobID, f.machine.id)
	if err != nil {
		t.Fatalf("讀回工作單失敗：%v", err)
	}
	if job.State != deploy.NotStarted || job.LeaseToken != "" || job.LeaseExpiresAt != nil {
		t.Fatalf("帶 machine_id 的 request 改動了工作單：%+v", job)
	}
}

// ⚠ 守住把「目前沒有工作單」包成 200 空物件，讓 Agent 誤以為有單可做的錯。
func TestNextJobWithoutWorkReturnsEmpty204(t *testing.T) {
	f := newJobsFixture(t, "machine-a")
	rec := requestJobAPI(t, f.mux, http.MethodGet, "/v1/jobs/next", f.machine.token, nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("沒有工作單時回應 %d，預期 204：%s", rec.Code, rec.Body.String())
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("204 回應帶了 %d bytes，預期空 body：%q", rec.Body.Len(), rec.Body.String())
	}
}

// ⚠ 守住領單回應漏掉 artifact_digest，讓 Agent 在 activation 前無從核對產物的錯。
func TestNextJobIncludesArtifactDigest(t *testing.T) {
	f := newJobsFixture(t, "machine-a")
	jobID := f.newJob(t, "sha256:abc123")
	rec := requestJobAPI(t, f.mux, http.MethodGet, "/v1/jobs/next", f.machine.token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("讀取下一張工作單回應 %d，預期 200：%s", rec.Code, rec.Body.String())
	}
	var got model.JobResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("解碼工作單回應失敗：%v", err)
	}
	if got.JobID != jobID || got.ArtifactDigest != "sha256:abc123" {
		t.Fatalf("工作單或 digest 不符：%+v", got)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &fields); err != nil {
		t.Fatalf("檢查工作單欄位失敗：%v", err)
	}
	if _, ok := fields["artifact_digest"]; !ok {
		t.Fatal("工作單 JSON 沒有 artifact_digest 欄位")
	}
}

// ⚠ 守住續租接受別人的或已過期的 token，讓舊執行者跨過 fencing 邊界的錯。
func TestRenewLeaseRejectsWrongAndExpiredTokens(t *testing.T) {
	f := newJobsFixture(t, "machine-a")
	jobID := f.newJob(t, "sha256:a")
	lease := claimJobViaHTTP(t, f, jobID)

	wrong := requestJobAPI(t, f.mux, http.MethodPost, "/v1/jobs/"+jobID+"/lease:renew", f.machine.token,
		model.JobLeaseRequest{LeaseToken: "別人的-token"})
	assertAPIError(t, wrong, http.StatusConflict, model.ErrLeaseInvalid)

	hubNow = func() time.Time { return jobsTestNow.Add(jobLeaseDuration + time.Second) }
	expired := requestJobAPI(t, f.mux, http.MethodPost, "/v1/jobs/"+jobID+"/lease:renew", f.machine.token,
		model.JobLeaseRequest{LeaseToken: lease.LeaseToken})
	assertAPIError(t, expired, http.StatusConflict, model.ErrLeaseInvalid)
}

// ⚠ 守住把 Agent 的 occurred_at 同時寫成 received_at，抹掉機器時鐘偏移訊號的錯。
func TestJobEventReceivedAtUsesHubClock(t *testing.T) {
	f := newJobsFixture(t, "machine-a")
	jobID := f.newJob(t, "sha256:a")
	lease := claimJobViaHTTP(t, f, jobID)
	occurredAt := jobsTestNow.Add(-365 * 24 * time.Hour)
	rec := requestJobAPI(t, f.mux, http.MethodPost, "/v1/jobs/"+jobID+"/events", f.machine.token,
		model.JobEventRequest{
			LeaseToken: lease.LeaseToken, Seq: 7, Phase: "installing",
			Payload: json.RawMessage(`{"進度":"下載完成"}`), OccurredAt: occurredAt,
		})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("寫入事件回應 %d，預期 202：%s", rec.Code, rec.Body.String())
	}
	var occurred, received string
	if err := f.store.DB().QueryRow(
		`SELECT occurred_at, received_at FROM job_events WHERE job_id = ? AND seq = 7`, jobID,
	).Scan(&occurred, &received); err != nil {
		t.Fatalf("讀回事件時間失敗：%v", err)
	}
	if occurred != occurredAt.Format(time.RFC3339) {
		t.Fatalf("occurred_at 是 %q，預期 %q", occurred, occurredAt.Format(time.RFC3339))
	}
	if received != jobsTestNow.Format(time.RFC3339) || received == occurred {
		t.Fatalf("received_at 沒有使用 Hub 時鐘：occurred_at=%q received_at=%q", occurred, received)
	}
}

func TestJobEvidenceRejectsInvalidMetadataAtHTTPBoundary(t *testing.T) {
	f := newJobsFixture(t, "machine-a")
	jobID := f.newJob(t, "sha256:a")
	lease := claimJobViaHTTP(t, f, jobID)
	tests := []struct {
		name, path string
		body       any
		want       string
	}{
		{
			name: "negative event sequence", path: "/v1/jobs/" + jobID + "/events",
			body: model.JobEventRequest{LeaseToken: lease.LeaseToken, Seq: -1,
				Phase: "installing", OccurredAt: jobsTestNow},
			want: "seq",
		},
		{
			name: "zero event time", path: "/v1/jobs/" + jobID + "/events",
			body: model.JobEventRequest{LeaseToken: lease.LeaseToken, Seq: 1,
				Phase: "installing"},
			want: "occurred_at",
		},
		{
			name: "oversized event payload", path: "/v1/jobs/" + jobID + "/events",
			body: model.JobEventRequest{LeaseToken: lease.LeaseToken, Seq: 1,
				Phase: "installing", Payload: json.RawMessage(`"` + strings.Repeat("x", 65<<10) + `"`),
				OccurredAt: jobsTestNow},
			want: "payload",
		},
		{
			name: "zero verification time", path: "/v1/jobs/" + jobID + "/verifications",
			body: model.JobVerificationRequest{LeaseToken: lease.LeaseToken,
				RuleID: "health", Command: "true", Passed: true},
			want: "verified_at",
		},
		{
			name: "oversized verification command", path: "/v1/jobs/" + jobID + "/verifications",
			body: model.JobVerificationRequest{LeaseToken: lease.LeaseToken,
				RuleID: "health", Command: strings.Repeat("x", 17<<10), Passed: true,
				VerifiedAt: jobsTestNow},
			want: "command",
		},
		{
			name: "negative reject sequence", path: "/v1/jobs/" + jobID + "/reject",
			body: model.JobRejectRequest{LeaseToken: lease.LeaseToken, Seq: -1,
				RejectionCode: string(deploy.PreconditionFailed)},
			want: "seq",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := requestJobAPI(t, f.mux, http.MethodPost, test.path, f.machine.token, test.body)
			assertAPIError(t, response, http.StatusBadRequest, "BAD_REQUEST")
			if !strings.Contains(response.Body.String(), test.want) {
				t.Fatalf("response did not explain %q: %s", test.want, response.Body.String())
			}
		})
	}
	var eventCount, verificationCount int
	if err := f.store.DB().QueryRow(`SELECT COUNT(*) FROM job_events WHERE job_id=?`, jobID).Scan(&eventCount); err != nil {
		t.Fatal(err)
	}
	if err := f.store.DB().QueryRow(`SELECT COUNT(*) FROM verification_results WHERE job_id=?`, jobID).Scan(&verificationCount); err != nil {
		t.Fatal(err)
	}
	if eventCount != 0 || verificationCount != 0 {
		t.Fatalf("invalid HTTP evidence wrote events=%d verifications=%d", eventCount, verificationCount)
	}
}

// ⚠ 守住 Agent 網路重送被當成第二筆事件，而不是依同一個 seq 去重的錯。
func TestJobEventReplayIsIdempotent(t *testing.T) {
	f := newJobsFixture(t, "machine-a")
	jobID := f.newJob(t, "sha256:a")
	lease := claimJobViaHTTP(t, f, jobID)
	body := model.JobEventRequest{
		LeaseToken: lease.LeaseToken, Seq: 8, Phase: "installing",
		Payload: json.RawMessage(`{"進度":"套用"}`), OccurredAt: jobsTestNow.Add(-time.Minute),
	}
	for attempt := 1; attempt <= 2; attempt++ {
		rec := requestJobAPI(t, f.mux, http.MethodPost, "/v1/jobs/"+jobID+"/events", f.machine.token, body)
		if rec.Code < 200 || rec.Code >= 300 {
			t.Fatalf("第 %d 次送事件回應 %d，預期 2xx：%s", attempt, rec.Code, rec.Body.String())
		}
	}
	var count int
	if err := f.store.DB().QueryRow(
		`SELECT COUNT(*) FROM job_events WHERE job_id = ? AND seq = 8`, jobID,
	).Scan(&count); err != nil {
		t.Fatalf("計算事件列數失敗：%v", err)
	}
	if count != 1 {
		t.Fatalf("同一個 seq 重送後有 %d 列，預期 1 列", count)
	}
}

// ⚠⚠ seq 是事件的 idempotency key，不是「丟掉所有同 seq 內容」的開關。
func TestJobEventReplayWithDifferentBodyReturnsConflict(t *testing.T) {
	f := newJobsFixture(t, "machine-a")
	jobID := f.newJob(t, "sha256:a")
	lease := claimJobViaHTTP(t, f, jobID)
	first := model.JobEventRequest{
		LeaseToken: lease.LeaseToken, Seq: 8, Phase: "installing",
		Payload: json.RawMessage(`{"step":"download"}`), OccurredAt: jobsTestNow.Add(-time.Minute),
	}
	accepted := requestJobAPI(t, f.mux, http.MethodPost, "/v1/jobs/"+jobID+"/events", f.machine.token, first)
	if accepted.Code != http.StatusAccepted {
		t.Fatalf("寫入第一次事件回應 %d，預期 202：%s", accepted.Code, accepted.Body.String())
	}

	conflicting := first
	conflicting.Payload = json.RawMessage(`{"step":"activate"}`)
	rec := requestJobAPI(t, f.mux, http.MethodPost, "/v1/jobs/"+jobID+"/events", f.machine.token, conflicting)
	assertAPIError(t, rec, http.StatusConflict, model.ErrJobEventConflict)

	var phase, payload string
	if err := f.store.DB().QueryRow(
		`SELECT phase, payload FROM job_events WHERE job_id = ? AND seq = 8`, jobID,
	).Scan(&phase, &payload); err != nil {
		t.Fatalf("讀回第一次事件失敗：%v", err)
	}
	if phase != first.Phase || payload != string(first.Payload) {
		t.Fatalf("衝突回放覆蓋了第一次事件：phase=%q payload=%q", phase, payload)
	}
}

func TestTerminalStartEventExactReplayIsAcceptedButChangedBodyConflicts(t *testing.T) {
	f := newJobsFixture(t, "machine-a")
	jobID := f.newJob(t, "sha256:a")
	lease := claimJobViaHTTP(t, f, jobID)
	body := model.JobEventRequest{
		LeaseToken: lease.LeaseToken, Seq: 1, Phase: "start",
		Payload: json.RawMessage(`{"step":"activate"}`), OccurredAt: jobsTestNow.Add(-time.Minute),
	}
	first := requestJobAPI(t, f.mux, http.MethodPost, "/v1/jobs/"+jobID+"/events", f.machine.token, body)
	if first.Code != http.StatusAccepted {
		t.Fatalf("first start = %d: %s", first.Code, first.Body.String())
	}
	if _, err := f.store.AdvanceJobByHub(jobID, deploy.Timeout, jobsTestNow.Add(time.Minute)); err != nil {
		t.Fatalf("make job terminal: %v", err)
	}

	replay := requestJobAPI(t, f.mux, http.MethodPost, "/v1/jobs/"+jobID+"/events", f.machine.token, body)
	if replay.Code != http.StatusAccepted {
		t.Fatalf("exact terminal start replay = %d: %s", replay.Code, replay.Body.String())
	}
	body.Payload = json.RawMessage(`{"step":"different"}`)
	conflict := requestJobAPI(t, f.mux, http.MethodPost, "/v1/jobs/"+jobID+"/events", f.machine.token, body)
	assertAPIError(t, conflict, http.StatusConflict, model.ErrJobEventConflict)

	var n int
	if err := f.store.DB().QueryRow(`SELECT COUNT(*) FROM job_events WHERE job_id=? AND seq=1`, jobID).Scan(&n); err != nil || n != 1 {
		t.Fatalf("terminal replays changed event count=%d err=%v", n, err)
	}
}

func TestReplayedStartEventHealsCommittedEventBeforeStateTransition(t *testing.T) {
	f := newJobsFixture(t, "machine-a")
	jobID := f.newJob(t, "sha256:a")
	lease := claimJobViaHTTP(t, f, jobID)
	occurredAt := jobsTestNow.Add(-time.Minute)
	if err := f.store.AppendJobEvent(jobID, f.machine.id, lease.LeaseToken, 1, "start",
		`{"step":"activate"}`, occurredAt, jobsTestNow); err != nil {
		t.Fatal(err)
	}
	before, err := f.store.JobForMachine(jobID, f.machine.id)
	if err != nil || before.State != deploy.Claimed {
		t.Fatalf("crash-window fixture state=%+v err=%v", before, err)
	}

	retry := requestJobAPI(t, f.mux, http.MethodPost, "/v1/jobs/"+jobID+"/events", f.machine.token,
		model.JobEventRequest{LeaseToken: lease.LeaseToken, Seq: 1, Phase: "start",
			Payload: json.RawMessage(`{"step":"activate"}`), OccurredAt: occurredAt})
	if retry.Code != http.StatusAccepted {
		t.Fatalf("start retry in crash window = %d: %s", retry.Code, retry.Body.String())
	}
	after, err := f.store.JobForMachine(jobID, f.machine.id)
	if err != nil || after.State != deploy.Running {
		t.Fatalf("start retry did not heal claimed->running: state=%+v err=%v", after, err)
	}
}

func TestReplayedOversizedLegacyStartEventStillHealsCrashWindow(t *testing.T) {
	f := newJobsFixture(t, "machine-a")
	jobID := f.newJob(t, "sha256:a")
	lease := claimJobViaHTTP(t, f, jobID)
	occurredAt := jobsTestNow.Add(-time.Minute)
	payload := `{"detail":"` + strings.Repeat("x", 65<<10) + `"}`
	if _, err := f.store.DB().Exec(`
INSERT INTO job_events (event_id, job_id, seq, phase, occurred_at, received_at, payload)
VALUES (?, ?, ?, ?, ?, ?, ?)`, "legacy-oversized-start", jobID, 1, "start",
		occurredAt.UTC().Format(time.RFC3339), jobsTestNow.Format(time.RFC3339), payload); err != nil {
		t.Fatalf("insert legacy oversized event: %v", err)
	}

	retry := requestJobAPI(t, f.mux, http.MethodPost, "/v1/jobs/"+jobID+"/events", f.machine.token,
		model.JobEventRequest{LeaseToken: lease.LeaseToken, Seq: 1, Phase: "start",
			Payload: json.RawMessage(payload), OccurredAt: occurredAt})
	if retry.Code != http.StatusAccepted {
		t.Fatalf("legacy start retry in crash window = %d: %s", retry.Code, retry.Body.String())
	}
	after, err := f.store.JobForMachine(jobID, f.machine.id)
	if err != nil || after.State != deploy.Running {
		t.Fatalf("legacy start retry did not heal claimed->running: state=%+v err=%v", after, err)
	}
	var eventCount int
	if err := f.store.DB().QueryRow(
		`SELECT COUNT(*) FROM job_events WHERE job_id = ?`, jobID,
	).Scan(&eventCount); err != nil || eventCount != 1 {
		t.Fatalf("legacy start replay changed event count to %d: %v", eventCount, err)
	}
}

func TestReplayedInvalidMetadataLegacyStartStillHealsCrashWindow(t *testing.T) {
	f := newJobsFixture(t, "machine-a")
	jobID := f.newJob(t, "sha256:a")
	lease := claimJobViaHTTP(t, f, jobID)
	occurredAt := time.Time{}
	if _, err := f.store.DB().Exec(`
INSERT INTO job_events (event_id, job_id, seq, phase, occurred_at, received_at, payload)
VALUES (?, ?, ?, ?, ?, ?, ?)`, "legacy-invalid-start", jobID, -1, "start",
		occurredAt.UTC().Format(time.RFC3339), jobsTestNow.Format(time.RFC3339), `{}`); err != nil {
		t.Fatalf("insert legacy invalid event: %v", err)
	}

	retry := requestJobAPI(t, f.mux, http.MethodPost, "/v1/jobs/"+jobID+"/events", f.machine.token,
		model.JobEventRequest{LeaseToken: lease.LeaseToken, Seq: -1, Phase: "start",
			Payload: json.RawMessage(`{}`), OccurredAt: occurredAt})
	if retry.Code != http.StatusAccepted {
		t.Fatalf("legacy invalid start retry in crash window = %d: %s", retry.Code, retry.Body.String())
	}
	after, err := f.store.JobForMachine(jobID, f.machine.id)
	if err != nil || after.State != deploy.Running {
		t.Fatalf("legacy invalid start retry did not heal claimed->running: state=%+v err=%v", after, err)
	}
}

// The event INSERT and job transition are separate commits. Another exact
// request can advance the job between them even when this request inserted the
// event row (and therefore has replayed=false). The trigger makes that overlap
// deterministic instead of relying on goroutine scheduling.
func TestStartEventAcceptsOverlappingProgressAfterItsInsert(t *testing.T) {
	tests := []struct {
		name          string
		triggerUpdate string
		wantState     deploy.JobState
	}{
		{
			name:          "running",
			triggerUpdate: `UPDATE jobs SET state = 'running' WHERE job_id = NEW.job_id`,
			wantState:     deploy.Running,
		},
		{
			name:          "verifying",
			triggerUpdate: `UPDATE jobs SET state = 'verifying' WHERE job_id = NEW.job_id`,
			wantState:     deploy.Verifying,
		},
		{
			name: "terminal",
			triggerUpdate: `UPDATE jobs
SET state = 'failed', terminal_at = '2026-09-06T16:00:00Z',
    lease_token = NULL, lease_expires_at = NULL
WHERE job_id = NEW.job_id`,
			wantState: deploy.Failed,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newJobsFixture(t, "machine-a")
			jobID := f.newJob(t, "sha256:a")
			lease := claimJobViaHTTP(t, f, jobID)
			if _, err := f.store.DB().Exec(`CREATE TRIGGER overlap_start_after_insert
AFTER INSERT ON job_events
WHEN NEW.phase = 'start'
BEGIN
  ` + tt.triggerUpdate + `;
END`); err != nil {
				t.Fatalf("create overlap trigger: %v", err)
			}

			rec := requestJobAPI(t, f.mux, http.MethodPost, "/v1/jobs/"+jobID+"/events", f.machine.token,
				model.JobEventRequest{LeaseToken: lease.LeaseToken, Seq: 1, Phase: "start",
					Payload: json.RawMessage(`{"step":"activate"}`), OccurredAt: jobsTestNow})
			if rec.Code != http.StatusAccepted {
				t.Fatalf("overlapping start at %s = %d: %s", tt.wantState, rec.Code, rec.Body.String())
			}
			job, err := f.store.JobForMachine(jobID, f.machine.id)
			if err != nil || job.State != tt.wantState {
				t.Fatalf("overlapping state=%+v err=%v; want %s", job, err, tt.wantState)
			}
		})
	}
}

// ⚠⚠ 守住把 Agent 的完成宣告直接當成成功；沒有 verification 時只能停在 verifying。
func TestCompleteWithoutVerificationStopsAtVerifying(t *testing.T) {
	f := newJobsFixture(t, "machine-a")
	jobID := f.newJob(t, "sha256:a")
	lease := claimJobViaHTTP(t, f, jobID)
	if _, err := f.store.AdvanceJobByAgent(jobID, f.machine.id, lease.LeaseToken, deploy.Start, jobsTestNow); err != nil {
		t.Fatalf("把測試工作單推進 running 失敗：%v", err)
	}
	rec := requestJobAPI(t, f.mux, http.MethodPost, "/v1/jobs/"+jobID+"/complete", f.machine.token,
		model.JobCompleteRequest{LeaseToken: lease.LeaseToken})
	assertAPIError(t, rec, http.StatusConflict, model.ErrNoVerification)
	var stateErr model.JobStateError
	if err := json.Unmarshal(rec.Body.Bytes(), &stateErr); err != nil {
		t.Fatalf("解碼完成衝突回應失敗：%v", err)
	}
	if stateErr.State != string(deploy.Verifying) {
		t.Fatalf("完成衝突回應狀態是 %q，預期 verifying", stateErr.State)
	}
	job, err := f.store.JobForMachine(jobID, f.machine.id)
	if err != nil {
		t.Fatalf("讀回工作單失敗：%v", err)
	}
	if job.State != deploy.Verifying {
		t.Fatalf("沒有驗證證據的工作單停在 %q，預期 verifying", job.State)
	}
}

// ⚠ 守住已有通過證據卻沒有由 Hub 完成最後判決，或回應沒有說出最終狀態的錯。
func TestCompleteWithPassedVerificationSucceeds(t *testing.T) {
	f := newJobsFixture(t, "machine-a")
	jobID := f.newJob(t, "sha256:a")
	lease := claimJobViaHTTP(t, f, jobID)
	if _, err := f.store.AdvanceJobByAgent(jobID, f.machine.id, lease.LeaseToken, deploy.Start, jobsTestNow); err != nil {
		t.Fatalf("把測試工作單推進 running 失敗：%v", err)
	}
	verification := model.JobVerificationRequest{
		LeaseToken: lease.LeaseToken, RuleID: "版本", Command: "openclaw --version",
		ExitCode: 0, StdoutExcerpt: "openclaw 2026.9.6", Passed: true, VerifiedAt: jobsTestNow,
	}
	verified := requestJobAPI(t, f.mux, http.MethodPost, "/v1/jobs/"+jobID+"/verifications", f.machine.token, verification)
	if verified.Code != http.StatusCreated {
		t.Fatalf("寫入驗證回應 %d，預期 201：%s", verified.Code, verified.Body.String())
	}
	completed := requestJobAPI(t, f.mux, http.MethodPost, "/v1/jobs/"+jobID+"/complete", f.machine.token,
		model.JobCompleteRequest{LeaseToken: lease.LeaseToken})
	if completed.Code != http.StatusOK {
		t.Fatalf("完成工作單回應 %d，預期 200：%s", completed.Code, completed.Body.String())
	}
	var resp model.JobStateResponse
	if err := json.Unmarshal(completed.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解碼完成回應失敗：%v", err)
	}
	if resp.State != string(deploy.Succeeded) {
		t.Fatalf("完成回應狀態是 %q，預期 succeeded", resp.State)
	}
	job, err := f.store.JobForMachine(jobID, f.machine.id)
	if err != nil {
		t.Fatalf("讀回工作單失敗：%v", err)
	}
	if job.State != deploy.Succeeded {
		t.Fatalf("工作單最後停在 %q，預期 succeeded", job.State)
	}
}

// ⚠⚠ 守的是別人先把單收成終態時，complete 不准回死字面值 succeeded；
// 否則 agent 會把沒裝成的 revision 寫進 MaxApplied 與 journal。
func TestCompleteReportsTheLedgerStateWhenAnotherWriterEndsTheJob(t *testing.T) {
	f := newJobsFixture(t, "machine-a")
	jobID := f.newJob(t, "sha256:a")
	lease := claimJobViaHTTP(t, f, jobID)
	if _, err := f.store.AdvanceJobByAgent(jobID, f.machine.id, lease.LeaseToken, deploy.Start, jobsTestNow); err != nil {
		t.Fatalf("把測試工作單推進 running 失敗：%v", err)
	}
	verified := requestJobAPI(t, f.mux, http.MethodPost, "/v1/jobs/"+jobID+"/verifications", f.machine.token,
		model.JobVerificationRequest{
			LeaseToken: lease.LeaseToken, RuleID: "版本", Command: "openclaw --version",
			ExitCode: 0, StdoutExcerpt: "openclaw 2026.9.6", Passed: true, VerifiedAt: jobsTestNow,
		})
	if verified.Code != http.StatusCreated {
		t.Fatalf("寫入驗證回應 %d，預期 201：%s", verified.Code, verified.Body.String())
	}
	if _, err := f.store.DB().Exec(`CREATE TRIGGER complete_terminal_overlap
AFTER UPDATE OF state ON jobs
WHEN NEW.state = 'verifying'
BEGIN
  UPDATE jobs
     SET state = 'failed', terminal_at = '2026-09-06T16:00:00Z',
         lease_token = NULL, lease_expires_at = NULL
   WHERE job_id = NEW.job_id;
END`); err != nil {
		t.Fatalf("建立 complete 終態交錯 trigger 失敗：%v", err)
	}
	t.Cleanup(func() {
		if _, err := f.store.DB().Exec(`DROP TRIGGER IF EXISTS complete_terminal_overlap`); err != nil {
			t.Errorf("移除 complete 終態交錯 trigger 失敗：%v", err)
		}
	})

	completed := requestJobAPI(t, f.mux, http.MethodPost, "/v1/jobs/"+jobID+"/complete", f.machine.token,
		model.JobCompleteRequest{LeaseToken: lease.LeaseToken})
	if completed.Code != http.StatusOK {
		t.Fatalf("完成工作單回應 %d，預期 200：%s", completed.Code, completed.Body.String())
	}
	var resp model.JobStateResponse
	if err := json.Unmarshal(completed.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解碼完成回應失敗：%v", err)
	}
	if resp.State != string(deploy.Failed) || !resp.Replayed {
		t.Fatalf("完成回應是 %+v，預期 failed 且 replayed=true", resp)
	}
	job, err := f.store.JobForMachine(jobID, f.machine.id)
	if err != nil {
		t.Fatalf("讀回工作單失敗：%v", err)
	}
	if job.State != deploy.Failed {
		t.Fatalf("帳本狀態是 %q，預期仍是 failed", job.State)
	}
}

// ⚠⚠ 守的是 reaper 先判 lease_expired 時，失敗證據的 complete 不准回 failed；
// failed 會讓人以為機器已回到舊版，但 lease_expired 代表機器現況仍未知。
func TestFailedVerificationReportsLeaseExpiredWhenReaperEndsTheJob(t *testing.T) {
	f := newJobsFixture(t, "machine-a")
	desiredID, rev, err := f.store.CreateDesiredState("machine", f.machine.id, "openclaw", "openclaw", `{}`, "測試")
	if err != nil {
		t.Fatalf("建立期望狀態失敗：%v", err)
	}
	jobID, err := f.store.CreateJob(f.machine.id, desiredID, rev, store.NewJob{ArtifactDigest: "sha256:a", Irreversible: false})
	if err != nil {
		t.Fatalf("建立工作單失敗：%v", err)
	}
	lease := claimJobViaHTTP(t, f, jobID)
	requestJobAPI(t, f.mux, http.MethodPost, "/v1/jobs/"+jobID+"/events", f.machine.token,
		model.JobEventRequest{LeaseToken: lease.LeaseToken, Seq: 1, Phase: "start", OccurredAt: jobsTestNow})
	verified := requestJobAPI(t, f.mux, http.MethodPost, "/v1/jobs/"+jobID+"/verifications", f.machine.token,
		model.JobVerificationRequest{LeaseToken: lease.LeaseToken, RuleID: "版本", Command: "openclaw --version",
			ExitCode: 1, StderrExcerpt: "command not found", Passed: false, VerifiedAt: jobsTestNow})
	if verified.Code != http.StatusCreated {
		t.Fatalf("寫入驗證回應 %d，預期 201：%s", verified.Code, verified.Body.String())
	}
	if _, err := f.store.DB().Exec(`CREATE TRIGGER fail_terminal_overlap
AFTER UPDATE OF state ON jobs
WHEN NEW.state = 'failed'
BEGIN
  UPDATE jobs
     SET state = 'lease_expired', terminal_at = '2026-09-06T16:00:00Z',
         lease_token = NULL, lease_expires_at = NULL
   WHERE job_id = NEW.job_id;
END`); err != nil {
		t.Fatalf("建立失敗終態交錯 trigger 失敗：%v", err)
	}
	t.Cleanup(func() {
		if _, err := f.store.DB().Exec(`DROP TRIGGER IF EXISTS fail_terminal_overlap`); err != nil {
			t.Errorf("移除失敗終態交錯 trigger 失敗：%v", err)
		}
	})

	completed := requestJobAPI(t, f.mux, http.MethodPost, "/v1/jobs/"+jobID+"/complete", f.machine.token,
		model.JobCompleteRequest{LeaseToken: lease.LeaseToken})
	if completed.Code != http.StatusOK {
		t.Fatalf("完成工作單回應 %d，預期 200：%s", completed.Code, completed.Body.String())
	}
	var resp model.JobStateResponse
	if err := json.Unmarshal(completed.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解碼完成回應失敗：%v", err)
	}
	job, err := f.store.JobForMachine(jobID, f.machine.id)
	if err != nil {
		t.Fatalf("讀回工作單失敗：%v", err)
	}
	if job.State != deploy.LeaseExpired {
		t.Fatalf("帳本狀態是 %q，預期 %q", job.State, deploy.LeaseExpired)
	}
	if resp.State != string(deploy.LeaseExpired) || !resp.Replayed {
		t.Fatalf("完成回應是 state=%q、replayed=%v，帳本狀態是 %q；預期回應 lease_expired 且 replayed=true，否則回應與帳本不一致時，agent 會把 Hub 自己的判決記成「已回到舊版」",
			resp.State, resp.Replayed, job.State)
	}
}

// ⚠ 守住把自由文字當成拒絕碼，讓拒絕結果失去可判讀語意的錯。
func TestRejectRefusesUnknownCode(t *testing.T) {
	f := newJobsFixture(t, "machine-a")
	jobID := f.newJob(t, "sha256:a")
	lease := claimJobViaHTTP(t, f, jobID)
	for _, code := range []string{"因為我不想", string(deploy.DependencyFailed)} {
		rec := requestJobAPI(t, f.mux, http.MethodPost, "/v1/jobs/"+jobID+"/reject", f.machine.token,
			model.JobRejectRequest{LeaseToken: lease.LeaseToken, RejectionCode: code, Detail: "自由文字"})
		assertAPIError(t, rec, http.StatusBadRequest, "BAD_REQUEST")
	}
	job, err := f.store.JobForMachine(jobID, f.machine.id)
	if err != nil {
		t.Fatalf("讀回工作單失敗：%v", err)
	}
	if job.State != deploy.Claimed {
		t.Fatalf("不合法拒絕碼把工作單改成 %q，預期仍是 claimed", job.State)
	}
}

// ⚠ 守住合法拒絕只改狀態卻沒留下碼與 detail，讓事後無法辨認拒絕依據的錯。
func TestRejectRecordsCodeAndDetail(t *testing.T) {
	f := newJobsFixture(t, "machine-a")
	jobID := f.newJob(t, "sha256:a")
	lease := claimJobViaHTTP(t, f, jobID)
	rec := requestJobAPI(t, f.mux, http.MethodPost, "/v1/jobs/"+jobID+"/reject", f.machine.token,
		model.JobRejectRequest{
			LeaseToken: lease.LeaseToken, RejectionCode: string(deploy.PreconditionFailed),
			Detail: "可用空間不足", Seq: 21, OccurredAt: jobsTestNow.Add(-time.Minute),
		})
	if rec.Code != http.StatusOK {
		t.Fatalf("合法拒絕回應 %d，預期 200：%s", rec.Code, rec.Body.String())
	}
	job, err := f.store.JobForMachine(jobID, f.machine.id)
	if err != nil {
		t.Fatalf("讀回工作單失敗：%v", err)
	}
	if job.State != deploy.Rejected {
		t.Fatalf("合法拒絕後狀態是 %q，預期 rejected", job.State)
	}
	var rawPayload, producerKind, producerID, evidenceRole, authority string
	var provenanceRecorded bool
	if err := f.store.DB().QueryRow(
		`SELECT payload,producer_kind,producer_id,evidence_role,authority,provenance_recorded
 FROM job_events WHERE job_id = ? AND seq = 21`, jobID,
	).Scan(&rawPayload, &producerKind, &producerID, &evidenceRole, &authority,
		&provenanceRecorded); err != nil {
		t.Fatalf("讀回拒絕事件失敗：%v", err)
	}
	var payload model.JobRejectEventPayload
	if err := json.Unmarshal([]byte(rawPayload), &payload); err != nil {
		t.Fatalf("解碼拒絕事件 payload 失敗：%v", err)
	}
	if payload.RejectionCode != string(deploy.PreconditionFailed) || payload.Detail != "可用空間不足" {
		t.Fatalf("拒絕事件內容不符：%+v", payload)
	}
	if producerKind != store.JobEventProducerExecutorAgent || producerID != f.machine.id ||
		evidenceRole != store.JobEventRoleExecutor || authority != store.JobEventAuthorityMachineLease ||
		!provenanceRecorded {
		t.Fatalf("拒絕事件來源 kind=%q id=%q role=%q authority=%q recorded=%t",
			producerKind, producerID, evidenceRole, authority, provenanceRecorded)
	}
}

func TestCapabilitiesAdvertiseTheSharedAgentAdapterRegistry(t *testing.T) {
	f := newJobsFixture(t, "machine-a")
	rec := requestJobAPI(t, f.mux, http.MethodGet, "/v1/capabilities", f.machine.token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("讀取 capabilities 回應 %d，預期 200：%s", rec.Code, rec.Body.String())
	}
	var got model.CapabilitiesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("解碼 capabilities 失敗：%v", err)
	}
	if got.SchemaVersion != model.SchemaVersion || !reflect.DeepEqual(got.ResourceKinds, agentadapter.ExecutorKinds()) {
		t.Fatalf("capabilities 宣告不符：%+v", got)
	}
}

func TestAgentReadinessReturnsTheAuthenticatedMachinesLatestCheckin(t *testing.T) {
	f := newJobsFixture(t, "bootstrap-machine")

	empty := requestJobAPI(t, f.mux, http.MethodGet, "/v1/agent/readiness", f.machine.token, nil)
	if empty.Code != http.StatusOK {
		t.Fatalf("empty readiness status=%d body=%s", empty.Code, empty.Body.String())
	}
	var receipt model.AgentReadinessResponse
	if err := json.Unmarshal(empty.Body.Bytes(), &receipt); err != nil || receipt.MachineID != f.machine.id ||
		receipt.LastCheckinReceivedAt != nil || receipt.AgentStartedAt != nil || receipt.AgentVersion != "" ||
		receipt.JobsEnabled != nil || receipt.DeviceSyncV1 != nil || receipt.IdentityReceivedAt != nil ||
		receipt.IdentityMeasuredAt != nil || receipt.IdentityOS != "" || receipt.IdentityArch != "" {
		t.Fatalf("empty readiness=%+v err=%v", receipt, err)
	}

	enabled := true
	sentAt := time.Now().UTC().Truncate(time.Second)
	if err := f.store.RecordCheckin(f.machine.id, model.Checkin{
		SchemaVersion: model.SchemaVersion, SentAt: sentAt, AgentVersion: "bootstrap-v1",
		BootID: "boot", AgentSeq: 1, AgentStartedAt: sentAt, JobsEnabled: &enabled, DeviceSyncV1: true,
	}, sentAt); err != nil {
		t.Fatalf("record readiness checkin: %v", err)
	}
	if err := f.store.RecordObservation(f.machine.id, model.ObservationBatch{
		MeasuredAt: sentAt, Identity: model.Identity{OS: "macOS 15.7", Arch: "arm64"},
	}, sentAt.Add(time.Second)); err != nil {
		t.Fatalf("record readiness identity evidence: %v", err)
	}
	other := enrollViaHTTP(t, f.mux, f.store, "other-bootstrap-machine")
	if err := f.store.RecordCheckin(other.id, model.Checkin{
		SchemaVersion: model.SchemaVersion, SentAt: sentAt.Add(time.Minute), AgentVersion: "other-version",
		BootID: "other-boot", AgentSeq: 1, AgentStartedAt: sentAt.Add(time.Minute), JobsEnabled: &enabled,
	}, sentAt.Add(time.Minute)); err != nil {
		t.Fatalf("record other machine checkin: %v", err)
	}
	ready := requestJobAPI(t, f.mux, http.MethodGet, "/v1/agent/readiness", f.machine.token, nil)
	if ready.Code != http.StatusOK {
		t.Fatalf("readiness status=%d body=%s", ready.Code, ready.Body.String())
	}
	receipt = model.AgentReadinessResponse{}
	if err := json.Unmarshal(ready.Body.Bytes(), &receipt); err != nil || receipt.MachineID != f.machine.id ||
		receipt.LastCheckinReceivedAt == nil || receipt.AgentStartedAt == nil || !receipt.AgentStartedAt.Equal(sentAt) ||
		receipt.AgentVersion != "bootstrap-v1" ||
		receipt.JobsEnabled == nil || !*receipt.JobsEnabled || receipt.DeviceSyncV1 == nil || !*receipt.DeviceSyncV1 ||
		receipt.IdentityReceivedAt == nil || !receipt.IdentityReceivedAt.Equal(sentAt.Add(time.Second)) ||
		receipt.IdentityMeasuredAt == nil || !receipt.IdentityMeasuredAt.Equal(sentAt) ||
		receipt.IdentityOS != "macOS 15.7" || receipt.IdentityArch != "arm64" {
		t.Fatalf("readiness=%+v err=%v", receipt, err)
	}
}

// ⚠⚠ 守的是這條**正常**路徑被 409 擋死：agent 先 complete（Hub 說證據不足）、
// 補送驗證、再 complete 一次。第二次 complete 時工作單已經在 verifying，
// 而 verifying 收 FinishWork 是不合法轉移 —— 若 handler 把那個錯誤直接回成
// 409，agent 就永遠沒辦法讓一張已經驗證通過的單變成 succeeded。
func TestCompleteAfterLateVerificationSucceeds(t *testing.T) {
	f := newJobsFixture(t, "machine-a")
	jobID := f.newJob(t, "sha256:a")
	lease := claimJobViaHTTP(t, f, jobID)
	if _, err := f.store.AdvanceJobByAgent(jobID, f.machine.id, lease.LeaseToken, deploy.Start, jobsTestNow); err != nil {
		t.Fatalf("把測試工作單推進 running 失敗：%v", err)
	}
	first := requestJobAPI(t, f.mux, http.MethodPost, "/v1/jobs/"+jobID+"/complete", f.machine.token,
		model.JobCompleteRequest{LeaseToken: lease.LeaseToken})
	assertAPIError(t, first, http.StatusConflict, model.ErrNoVerification)

	verified := requestJobAPI(t, f.mux, http.MethodPost, "/v1/jobs/"+jobID+"/verifications", f.machine.token,
		model.JobVerificationRequest{
			LeaseToken: lease.LeaseToken, RuleID: "版本", Command: "openclaw --version",
			ExitCode: 0, StdoutExcerpt: "openclaw 2026.9.6", Passed: true, VerifiedAt: jobsTestNow,
		})
	if verified.Code != http.StatusCreated {
		t.Fatalf("寫入驗證回應 %d，預期 201：%s", verified.Code, verified.Body.String())
	}
	second := requestJobAPI(t, f.mux, http.MethodPost, "/v1/jobs/"+jobID+"/complete", f.machine.token,
		model.JobCompleteRequest{LeaseToken: lease.LeaseToken})
	if second.Code != http.StatusOK {
		t.Fatalf("補完驗證後第二次 complete 回應 %d，預期 200：%s", second.Code, second.Body.String())
	}
	var resp model.JobStateResponse
	if err := json.Unmarshal(second.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解碼完成回應失敗：%v", err)
	}
	if resp.State != string(deploy.Succeeded) || resp.Replayed {
		t.Fatalf("第二次 complete 應該是這次判成 succeeded（不是回放），拿到 %+v", resp)
	}
}

// ⚠⚠ agent 是 at-least-once；201 回應弄丟後的重送不得在帳本上多出一列執行敘事，
// device-sync 的稽核回放也要求 executor 證據剛好只有一列。
func TestJobVerificationRetryDoesNotDuplicateEvidence(t *testing.T) {
	f := newJobsFixture(t, "machine-a")
	jobID := f.newJob(t, "sha256:a")
	lease := claimJobViaHTTP(t, f, jobID)
	if _, err := f.store.AdvanceJobByAgent(jobID, f.machine.id, lease.LeaseToken, deploy.Start, jobsTestNow); err != nil {
		t.Fatal(err)
	}
	body := model.JobVerificationRequest{
		LeaseToken: lease.LeaseToken, RuleID: "版本", Command: "openclaw --version",
		ExitCode: 0, StdoutExcerpt: "openclaw 2026.9.6", Passed: true, VerifiedAt: jobsTestNow,
	}
	for attempt := 1; attempt <= 2; attempt++ {
		rec := requestJobAPI(t, f.mux, http.MethodPost, "/v1/jobs/"+jobID+"/verifications", f.machine.token, body)
		if rec.Code != http.StatusCreated {
			t.Fatalf("第 %d 次寫入驗證回應 %d，預期 201：%s", attempt, rec.Code, rec.Body.String())
		}
	}
	var got int
	if err := f.store.DB().QueryRow(`SELECT COUNT(*) FROM verification_results WHERE job_id=?`, jobID).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != 1 {
		t.Fatalf("完全相同的重送留下 %d 列驗證證據，預期 1", got)
	}
	body.ExitCode = 1
	rec := requestJobAPI(t, f.mux, http.MethodPost, "/v1/jobs/"+jobID+"/verifications", f.machine.token, body)
	assertAPIError(t, rec, http.StatusConflict, model.ErrJobVerificationConflict)
	if err := f.store.DB().QueryRow(`SELECT COUNT(*) FROM verification_results WHERE job_id=?`, jobID).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != 1 {
		t.Fatalf("衝突重送後有 %d 列驗證證據，預期 1", got)
	}
}

// ⚠⚠ 守的是終態重送被當成錯誤。回應在網路上弄丟之後 agent 一定會重送，
// SPEC §5.2 DUPLICATE_JOB_ID 說終態要回放原結果 —— 而且**租約過期了也一樣**，
// 因為終態是事實，不管誰還拿著租約。
func TestCompleteRetryOnTerminalJobReplaysOutcome(t *testing.T) {
	f := newJobsFixture(t, "machine-a")
	jobID := f.newJob(t, "sha256:a")
	lease := claimJobViaHTTP(t, f, jobID)
	if _, err := f.store.AdvanceJobByAgent(jobID, f.machine.id, lease.LeaseToken, deploy.Start, jobsTestNow); err != nil {
		t.Fatalf("把測試工作單推進 running 失敗：%v", err)
	}
	requestJobAPI(t, f.mux, http.MethodPost, "/v1/jobs/"+jobID+"/verifications", f.machine.token,
		model.JobVerificationRequest{LeaseToken: lease.LeaseToken, RuleID: "版本", Command: "openclaw --version",
			Passed: true, VerifiedAt: jobsTestNow})
	if rec := requestJobAPI(t, f.mux, http.MethodPost, "/v1/jobs/"+jobID+"/complete", f.machine.token,
		model.JobCompleteRequest{LeaseToken: lease.LeaseToken}); rec.Code != http.StatusOK {
		t.Fatalf("第一次 complete 回應 %d，預期 200：%s", rec.Code, rec.Body.String())
	}

	check := func(label string) {
		t.Helper()
		rec := requestJobAPI(t, f.mux, http.MethodPost, "/v1/jobs/"+jobID+"/complete", f.machine.token,
			model.JobCompleteRequest{LeaseToken: lease.LeaseToken})
		if rec.Code != http.StatusOK {
			t.Fatalf("%s：重送 complete 回應 %d，預期 200 回放：%s", label, rec.Code, rec.Body.String())
		}
		var resp model.JobStateResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("%s：解碼回放回應失敗：%v", label, err)
		}
		if resp.State != string(deploy.Succeeded) || !resp.Replayed {
			t.Fatalf("%s：回放應該是 succeeded 且標 replayed，拿到 %+v", label, resp)
		}
	}
	check("租約還在")
	hubNow = func() time.Time { return jobsTestNow.Add(10 * time.Minute) } // fixture 的 cleanup 會還原
	check("租約已經過期")
}

// ⚠ 守的是 agent 領到單、回應弄丟、再領一次時被告知「找不到」。
// 那張單明明是它的、而且正是它自己拿著租約 —— 404 會讓它以為單消失了。
// 正確的答案是 409 配租約到期時間：等到那時候再領一次就好。
func TestClaimWhileAlreadyHoldingLeaseIsNotNotFound(t *testing.T) {
	f := newJobsFixture(t, "machine-a")
	jobID := f.newJob(t, "sha256:a")
	claimJobViaHTTP(t, f, jobID)
	rec := requestJobAPI(t, f.mux, http.MethodPost, "/v1/jobs/"+jobID+"/claims", f.machine.token, struct{}{})
	assertAPIError(t, rec, http.StatusConflict, model.ErrJobStateConflict)
	var stateErr model.JobStateError
	if err := json.Unmarshal(rec.Body.Bytes(), &stateErr); err != nil {
		t.Fatalf("解碼衝突回應失敗：%v", err)
	}
	if stateErr.State != string(deploy.Claimed) || stateErr.LeaseExpiresAt == nil {
		t.Fatalf("重複領單應該回 claimed 與租約到期時間，拿到 %+v", stateErr)
	}
	// ⚠ 而別台機器問同一張單，仍然是 404 —— 這條路不能變成列舉工具。
	secondMachine := enrollViaHTTP(t, f.mux, f.store, "machine-b")
	foreign := requestJobAPI(t, f.mux, http.MethodPost, "/v1/jobs/"+jobID+"/claims", secondMachine.token, struct{}{})
	assertAPIError(t, foreign, http.StatusNotFound, model.ErrJobNotFound)
}

// ⚠ 過期但 reaper 還沒掃到的空窗仍不准重跑同一個 job_id；否則第二次
// seq=1 會被 job_events 的 ON CONFLICT DO NOTHING 吃掉，證據安靜消失。
func TestExpiredClaimConflictsUntilReaperThenReplaysLeaseExpired(t *testing.T) {
	f := newJobsFixture(t, "machine-a")
	jobID := f.newJob(t, "sha256:a")
	lease := claimJobViaHTTP(t, f, jobID)
	hubNow = func() time.Time { return lease.LeaseExpiresAt.Add(time.Second) }

	before := requestJobAPI(t, f.mux, http.MethodPost, "/v1/jobs/"+jobID+"/claims", f.machine.token, struct{}{})
	assertAPIError(t, before, http.StatusConflict, model.ErrJobStateConflict)
	var conflict model.JobStateError
	if err := json.Unmarshal(before.Body.Bytes(), &conflict); err != nil {
		t.Fatalf("解碼過期衝突失敗：%v", err)
	}
	if conflict.LeaseExpiresAt == nil || !conflict.LeaseExpiresAt.Equal(lease.LeaseExpiresAt) {
		t.Fatalf("過期但尚未收尾的衝突沒有原租約時間：%+v", conflict)
	}

	(&hub{store: f.store}).reapJobs(jobNow())
	after := requestJobAPI(t, f.mux, http.MethodPost, "/v1/jobs/"+jobID+"/claims", f.machine.token, struct{}{})
	if after.Code != http.StatusOK {
		t.Fatalf("reaper 收尾後 claim 回應 %d，預期 200 回放：%s", after.Code, after.Body.String())
	}
	var replay model.JobStateResponse
	if err := json.Unmarshal(after.Body.Bytes(), &replay); err != nil {
		t.Fatalf("解碼 lease_expired 回放失敗：%v", err)
	}
	if replay.State != string(deploy.LeaseExpired) || !replay.Replayed {
		t.Fatalf("reaper 後應回放 lease_expired，拿到 %+v", replay)
	}
}

// ⚠ 守的是拒絕的重送被當成錯誤。同一個 seq 的事件是回放（store 已經這樣做），
// 但狀態推進那一步在終態上會回錯 —— handler 要把它翻成回放，不是 409。
func TestRejectRetryReplaysRejected(t *testing.T) {
	f := newJobsFixture(t, "machine-a")
	jobID := f.newJob(t, "sha256:a")
	lease := claimJobViaHTTP(t, f, jobID)
	body := model.JobRejectRequest{LeaseToken: lease.LeaseToken, RejectionCode: string(deploy.PreconditionFailed),
		Detail: "磁碟剩 200MB", Seq: 1, OccurredAt: jobsTestNow}
	first := requestJobAPI(t, f.mux, http.MethodPost, "/v1/jobs/"+jobID+"/reject", f.machine.token, body)
	if first.Code != http.StatusOK {
		t.Fatalf("第一次拒絕回應 %d，預期 200：%s", first.Code, first.Body.String())
	}
	second := requestJobAPI(t, f.mux, http.MethodPost, "/v1/jobs/"+jobID+"/reject", f.machine.token, body)
	if second.Code != http.StatusOK {
		t.Fatalf("重送拒絕回應 %d，預期 200 回放：%s", second.Code, second.Body.String())
	}
	var resp model.JobStateResponse
	if err := json.Unmarshal(second.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解碼回放回應失敗：%v", err)
	}
	if resp.State != string(deploy.Rejected) || !resp.Replayed {
		t.Fatalf("重送拒絕應該回放 rejected，拿到 %+v", resp)
	}
	var n int
	if err := f.store.DB().QueryRow(`SELECT COUNT(*) FROM job_events WHERE job_id = ?`, jobID).Scan(&n); err != nil {
		t.Fatalf("數事件失敗：%v", err)
	}
	if n != 1 {
		t.Fatalf("重送拒絕不該多出事件，job_events 有 %d 列", n)
	}
}

// A reject event uses the same (job_id, seq) identity as the generic event
// endpoint. It must therefore inherit the same canonical replay conflict, not
// turn a protocol error into a retryable 500.
func TestRejectWithConflictingActiveEventReturnsEventConflict(t *testing.T) {
	f := newJobsFixture(t, "machine-a")
	jobID := f.newJob(t, "sha256:a")
	lease := claimJobViaHTTP(t, f, jobID)
	occurredAt := jobsTestNow.Add(-time.Minute)
	accepted := requestJobAPI(t, f.mux, http.MethodPost, "/v1/jobs/"+jobID+"/events", f.machine.token,
		model.JobEventRequest{LeaseToken: lease.LeaseToken, Seq: 7, Phase: "installing",
			Payload: json.RawMessage(`{"step":"stage"}`), OccurredAt: occurredAt})
	if accepted.Code != http.StatusAccepted {
		t.Fatalf("seed event = %d: %s", accepted.Code, accepted.Body.String())
	}

	rejected := requestJobAPI(t, f.mux, http.MethodPost, "/v1/jobs/"+jobID+"/reject", f.machine.token,
		model.JobRejectRequest{LeaseToken: lease.LeaseToken, Seq: 7, OccurredAt: occurredAt,
			RejectionCode: string(deploy.PreconditionFailed), Detail: "不同內容"})
	assertAPIError(t, rejected, http.StatusConflict, model.ErrJobEventConflict)
}

// Once a reject reached terminal, only the exact same event may replay the
// result. Reusing its seq with changed detail must still expose the conflict.
func TestRejectTerminalReplayWithChangedBodyReturnsEventConflict(t *testing.T) {
	f := newJobsFixture(t, "machine-a")
	jobID := f.newJob(t, "sha256:a")
	lease := claimJobViaHTTP(t, f, jobID)
	body := model.JobRejectRequest{LeaseToken: lease.LeaseToken, Seq: 9, OccurredAt: jobsTestNow,
		RejectionCode: string(deploy.PreconditionFailed), Detail: "磁碟不足"}
	first := requestJobAPI(t, f.mux, http.MethodPost, "/v1/jobs/"+jobID+"/reject", f.machine.token, body)
	if first.Code != http.StatusOK {
		t.Fatalf("first reject = %d: %s", first.Code, first.Body.String())
	}

	body.Detail = "同一 seq 的另一個理由"
	conflict := requestJobAPI(t, f.mux, http.MethodPost, "/v1/jobs/"+jobID+"/reject", f.machine.token, body)
	assertAPIError(t, conflict, http.StatusConflict, model.ErrJobEventConflict)

	var payload string
	if err := f.store.DB().QueryRow(`SELECT payload FROM job_events WHERE job_id=? AND seq=9`, jobID).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(payload, "另一個理由") {
		t.Fatalf("terminal conflict overwrote original event: %s", payload)
	}
}

func TestRejectTerminalRequestWithNewSeqIsNotCalledAReplay(t *testing.T) {
	f := newJobsFixture(t, "machine-a")
	jobID := f.newJob(t, "sha256:a")
	lease := claimJobViaHTTP(t, f, jobID)
	body := model.JobRejectRequest{LeaseToken: lease.LeaseToken, Seq: 9, OccurredAt: jobsTestNow,
		RejectionCode: string(deploy.PreconditionFailed), Detail: "磁碟不足"}
	first := requestJobAPI(t, f.mux, http.MethodPost, "/v1/jobs/"+jobID+"/reject", f.machine.token, body)
	if first.Code != http.StatusOK {
		t.Fatalf("first reject = %d: %s", first.Code, first.Body.String())
	}

	body.Seq = 10
	body.Detail = "這不是原 request"
	notReplay := requestJobAPI(t, f.mux, http.MethodPost, "/v1/jobs/"+jobID+"/reject", f.machine.token, body)
	assertAPIError(t, notReplay, http.StatusConflict, model.ErrLeaseInvalid)
	if strings.Contains(notReplay.Body.String(), `"replayed":true`) {
		t.Fatalf("new terminal event identity was called replay: %s", notReplay.Body.String())
	}
}

// ⚠⚠ 守的是「沒有任何 HTTP 路徑能把單從 claimed 推到 running」。
// /complete 要求 running；如果 start 事件只落地不推狀態，真的 agent
// 從第一張單開始就 complete 不了。這支**只走 HTTP**，不直接碰 store 的狀態。
func TestStartEventMovesJobToRunningSoCompleteWorks(t *testing.T) {
	f := newJobsFixture(t, "machine-a")
	jobID := f.newJob(t, "sha256:a")
	lease := claimJobViaHTTP(t, f, jobID)
	start := model.JobEventRequest{LeaseToken: lease.LeaseToken, Seq: 1, Phase: "start", OccurredAt: jobsTestNow}
	for i, label := range []string{"第一次", "重送"} {
		rec := requestJobAPI(t, f.mux, http.MethodPost, "/v1/jobs/"+jobID+"/events", f.machine.token, start)
		if rec.Code != http.StatusAccepted {
			t.Fatalf("%s start 事件回應 %d，預期 202：%s", label, rec.Code, rec.Body.String())
		}
		job, err := f.store.JobForMachine(jobID, f.machine.id)
		if err != nil {
			t.Fatalf("讀回工作單失敗：%v", err)
		}
		if job.State != deploy.Running {
			t.Fatalf("%s start 之後狀態是 %q，預期 running（第 %d 次）", label, job.State, i+1)
		}
	}
	requestJobAPI(t, f.mux, http.MethodPost, "/v1/jobs/"+jobID+"/verifications", f.machine.token,
		model.JobVerificationRequest{LeaseToken: lease.LeaseToken, RuleID: "版本", Command: "openclaw --version",
			Passed: true, VerifiedAt: jobsTestNow})
	rec := requestJobAPI(t, f.mux, http.MethodPost, "/v1/jobs/"+jobID+"/complete", f.machine.token,
		model.JobCompleteRequest{LeaseToken: lease.LeaseToken})
	if rec.Code != http.StatusOK {
		t.Fatalf("純 HTTP 走完一圈之後 complete 回應 %d，預期 200：%s", rec.Code, rec.Body.String())
	}
}

// ⚠ 守的是工作單回應少了 spec：agent 拿到 desired_id 卻不知道要做什麼。
func TestNextJobCarriesDesiredSpec(t *testing.T) {
	f := newJobsFixture(t, "machine-a")
	f.newJobWithSpec(t, "sha256:a", `{"kind":"noop","note":"第五刀的 executor 什麼都不做"}`)
	rec := requestJobAPI(t, f.mux, http.MethodGet, "/v1/jobs/next", f.machine.token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("拉工作單回應 %d，預期 200：%s", rec.Code, rec.Body.String())
	}
	var job model.JobResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &job); err != nil {
		t.Fatalf("解碼工作單失敗：%v", err)
	}
	var spec struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(job.Spec, &spec); err != nil || spec.Kind != "noop" {
		t.Fatalf("工作單裡的 spec 不對：%s（err=%v）", string(job.Spec), err)
	}
	if job.ResourceKind != "openclaw" {
		t.Fatalf("resource_kind 是 %q，預期 openclaw", job.ResourceKind)
	}
	if job.ResourceID != "openclaw" {
		t.Fatalf("resource_id 是 %q，預期 openclaw", job.ResourceID)
	}
}

// ⚠⚠ 守的是「證據說失敗」被跟「沒有證據」一樣處理、把單停在 verifying。
// 有一筆 passed=0 這張單就永遠成功不了，停著沒意義；Hub 要下判決 ——
// 而且 failed 與 manual_intervention 不准互換（PHASES 完成判準第 6 條）。
func TestCompleteWithFailedVerificationIsJudgedByIrreversibility(t *testing.T) {
	for _, tc := range []struct {
		irreversible bool
		want         deploy.JobState
	}{
		{false, deploy.Failed},
		{true, deploy.ManualIntervention},
	} {
		f := newJobsFixture(t, "machine-a")
		desiredID, rev, err := f.store.CreateDesiredState("machine", f.machine.id, "openclaw", "openclaw", `{}`, "測試")
		if err != nil {
			t.Fatalf("建立期望狀態失敗：%v", err)
		}
		jobID, err := f.store.CreateJob(f.machine.id, desiredID, rev, store.NewJob{ArtifactDigest: "sha256:a", Irreversible: tc.irreversible})
		if err != nil {
			t.Fatalf("建立工作單失敗：%v", err)
		}
		lease := claimJobViaHTTP(t, f, jobID)
		requestJobAPI(t, f.mux, http.MethodPost, "/v1/jobs/"+jobID+"/events", f.machine.token,
			model.JobEventRequest{LeaseToken: lease.LeaseToken, Seq: 1, Phase: "start", OccurredAt: jobsTestNow})
		requestJobAPI(t, f.mux, http.MethodPost, "/v1/jobs/"+jobID+"/verifications", f.machine.token,
			model.JobVerificationRequest{LeaseToken: lease.LeaseToken, RuleID: "版本", Command: "openclaw --version",
				ExitCode: 1, StderrExcerpt: "command not found", Passed: false, VerifiedAt: jobsTestNow})
		rec := requestJobAPI(t, f.mux, http.MethodPost, "/v1/jobs/"+jobID+"/complete", f.machine.token,
			model.JobCompleteRequest{LeaseToken: lease.LeaseToken})
		if rec.Code != http.StatusOK {
			t.Fatalf("irreversible=%v：帶失敗證據 complete 回應 %d，預期 200 配終態：%s", tc.irreversible, rec.Code, rec.Body.String())
		}
		var resp model.JobStateResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("解碼回應失敗：%v", err)
		}
		job, _ := f.store.JobForMachine(jobID, f.machine.id)
		if resp.State != string(tc.want) || job.State != tc.want || job.TerminalAt == nil {
			t.Fatalf("irreversible=%v：回應 %q、資料庫 %q、terminal_at=%v，預期 %s",
				tc.irreversible, resp.State, job.State, job.TerminalAt, tc.want)
		}
	}
}

// governWithBlockingPolicy publishes one compliance policy that stops handing
// work to a failing machine, and assigns it to this fixture's machine.
func (f jobsFixture) governWithBlockingPolicy(t *testing.T, maxAgeSeconds int) {
	t.Helper()
	policy := compliance.Policy{SchemaVersion: compliance.SchemaVersion,
		Rules:   []compliance.Rule{{Kind: compliance.RuleCheckinMaxAge, MaxAgeSeconds: maxAgeSeconds}},
		Actions: []compliance.Action{{Kind: compliance.ActionBlockJobs}}}
	expected := int64(0)
	preview, err := store.CompliancePolicyPreviewDigest("floor", expected, policy)
	if err != nil {
		t.Fatalf("預覽合規性原則失敗：%v", err)
	}
	if _, err := f.store.ApplyOperatorCompliancePolicy(store.OperatorCompliancePolicyRequest{
		PolicyID: "floor", Policy: policy, ExpectedRevision: &expected, PreviewDigest: preview,
		ConfirmPolicyID: "floor", Reason: "測試", PublishedBy: "tester",
		IdempotencyKey: "p1", RequestDigest: "sha256:" + strings.Repeat("a", 64),
		Audit: store.AuditEntry{SourceAddr: "test"},
	}); err != nil {
		t.Fatalf("發佈合規性原則失敗：%v", err)
	}
	if _, err := f.store.ApplyOperatorComplianceAssignment(store.OperatorComplianceAssignmentRequest{
		Scope: compliance.ScopeMachine, ScopeID: f.machine.id, PolicyID: "floor", PolicyRevision: 1,
		PreviewDigest: store.ComplianceAssignmentPreviewDigest(
			compliance.ScopeMachine, f.machine.id, "floor", 1),
		ConfirmScopeID: f.machine.id, Reason: "測試", AssignedBy: "tester",
		IdempotencyKey: "a1", RequestDigest: "sha256:" + strings.Repeat("b", 64),
		Audit: store.AuditEntry{SourceAddr: "test"},
	}); err != nil {
		t.Fatalf("指派合規性原則失敗：%v", err)
	}
}

// ⚠ 守住合規性動作只停在看板上，機器照樣領得到工作單的錯。判決要真的改變機隊的
// 行為，不然它只是一個顏色。
func TestNextJobIsWithheldWhileAComplianceActionIsInForce(t *testing.T) {
	f := newJobsFixture(t, "machine-a")
	jobID := f.newJob(t, "sha256:a")
	f.governWithBlockingPolicy(t, 600)

	// 這台機器的最後一次回報已經超過新鮮度上限，寬限期是 0：現在就生效。
	stale := time.Now().UTC().Add(-2 * time.Hour)
	if err := f.store.RecordCheckin(f.machine.id, model.Checkin{
		SchemaVersion: model.SchemaVersion, AgentVersion: "test", SentAt: stale,
	}, stale); err != nil {
		t.Fatalf("寫入測試 check-in 失敗：%v", err)
	}
	blocked := requestJobAPI(t, f.mux, http.MethodGet, "/v1/jobs/next", f.machine.token, nil)
	if blocked.Code != http.StatusNoContent {
		t.Fatalf("停發工作單生效中卻回應 %d：%s", blocked.Code, blocked.Body.String())
	}
	if blocked.Body.Len() != 0 {
		t.Fatalf("204 回應帶了 body：%q", blocked.Body.String())
	}

	// 機器回報回來就自己領得到單，不需要任何人解鎖 —— 動作跟判決一樣是現算的。
	fresh := time.Now().UTC()
	if err := f.store.RecordCheckin(f.machine.id, model.Checkin{
		SchemaVersion: model.SchemaVersion, AgentVersion: "test", SentAt: fresh,
	}, fresh); err != nil {
		t.Fatalf("寫入測試 check-in 失敗：%v", err)
	}
	recovered := requestJobAPI(t, f.mux, http.MethodGet, "/v1/jobs/next", f.machine.token, nil)
	if recovered.Code != http.StatusOK {
		t.Fatalf("恢復回報後仍領不到工作單，回應 %d：%s", recovered.Code, recovered.Body.String())
	}
	var got model.JobResponse
	if err := json.Unmarshal(recovered.Body.Bytes(), &got); err != nil {
		t.Fatalf("解碼工作單回應失敗：%v", err)
	}
	if got.JobID != jobID {
		t.Fatalf("領到的不是那張工作單：%+v", got)
	}
}
