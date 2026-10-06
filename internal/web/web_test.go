package web

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/deploy"
	"github.com/teddashh/AI-Intune/internal/expect"
	"github.com/teddashh/AI-Intune/internal/model"
	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/state"
	"github.com/teddashh/AI-Intune/internal/store"
	"github.com/teddashh/AI-Intune/internal/tailnet"
)

// 這些測試打真的 store、渲染真的樣板。
//
// ⚠ 不 mock 是刻意的。這一層唯一會出錯的東西就是「樣板欄位打錯字」跟
// 「文案說了不該說的話」，兩個都只有真的把 HTML 生出來才看得到。
// html/template 在欄位打錯時是執行期錯誤 —— 畫面會從那裡截斷，
// 而一個截斷的畫面看起來就像「那台沒事」。所以每個測試都檢查 </html>。

func newServer(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	s, err := New(st, "")
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	// Production injects the Hub-owned catalog before serving. Keep the shared
	// page fixture equally configured while leaving the catalog genuinely empty.
	s.SetArtifactsDir(t.TempDir())
	return s, st
}

func get(t *testing.T, s *Server, path string) string {
	t.Helper()
	mux := http.NewServeMux()
	s.Routes(mux)
	rec := httptest.NewRecorder()
	req := verifiedWebRequest(httptest.NewRequest("GET", path, nil), "example.com/cap/clawctl-view")
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d，內容：%s", path, rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	// ⚠ 樣板執行錯誤不會變成 500，它會讓輸出從出錯的地方斷掉。
	// 這一行是所有測試共用的斷點偵測。
	if !strings.Contains(body, "</html>") {
		t.Fatalf("GET %s 的輸出被截斷了 —— 樣板執行到一半失敗。尾巴：\n%s",
			path, tail(body, 400))
	}
	return body
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}

func enroll(t *testing.T, st *store.Store, name string, now time.Time) string {
	t.Helper()
	tok, err := st.CreateEnrollToken(name, time.Hour)
	if err != nil {
		t.Fatalf("create token: %v", err)
	}
	id, _, err := st.RedeemEnrollToken(tok, model.EnrollRequest{
		SchemaVersion: model.SchemaVersion, EnrollToken: tok,
		Hostname: name, OS: "linux", Arch: "amd64", UnixUser: "example-user",
	}, now)
	if err != nil {
		t.Fatalf("redeem: %v", err)
	}
	return id
}

func batch(measuredAt time.Time) model.ObservationBatch {
	lastTask := measuredAt.Add(-time.Hour)
	return model.ObservationBatch{
		SchemaVersion: model.SchemaVersion,
		MeasuredAt:    measuredAt,
		Identity: model.Identity{
			Hostname: "samplehub1", OS: "linux", Arch: "amd64", UnixUser: "example-user",
			MachineIDHint: "aaaaaaaaaaaa", BootID: "boot-1", LingerEnabled: true, LingerMeasured: true,
			TailscaleIP: "100.64.0.1",
		},
		Resources: model.Resources{
			DiskFreeBytes: 50 << 30, DiskTotalBytes: 100 << 30,
			MemTotalBytes: 16 << 30, MemAvailableBytes: 8 << 30, CPUCount: 8, Load1m: func() *float64 { v := 0.4; return &v }(),
		},
		Systemd: []model.Unit{{
			Name: "openclaw.service", Present: true,
			ActiveState: "active", SubState: "running",
		}},
		OpenClaw: model.OpenClaw{
			Present: true, CLIVersion: "2026.6.1",
			GatewayVersion: "2026.6.1", UpstreamVersion: "2026.5.9",
			DB: &model.OpenClawDB{
				Present: true, Layout: "consolidated",
				Path:            "~/.openclaw/state/openclaw.sqlite",
				LastTaskEndedAt: &lastTask, TaskRunRows: 120,
				RecentSummaries: []model.RunSummary{{
					JobID: "job-7", At: measuredAt.Add(-time.Hour), Status: "ok",
					Summary: "我沒辦法連上 API，所以這次沒有做任何事。",
				}},
			},
		},
		Credentials: []model.Credential{{Provider: "claude", Status: model.CredConfigured}},
		CLITools: []model.CLITool{{
			Name: "openclaw", Present: true, Path: "/usr/bin/openclaw",
			VersionReported: "2026.6.1", RunningPID: 4242,
		}},
		// ⚠ 這組值照 samplehub1 的實機抄的：--bind=tailscale --port=9876，
		// 而且核心的 socket 表上真的有東西在聽那個 port。
		// 假資料太乾淨就測不到真的問題 —— port 刻意不是 8080，因為
		// 8080 正好是原本寫死在樣板裡、而且四台全錯的那個猜測。
		BAT: model.BAT{
			Running: true, Port: 9876, Bind: "tailscale",
			Argv:        "/opt/bat-server/bat-server --bind=tailscale --port=9876",
			ListenAddrs: []string{"100.64.0.1"},
		},
	}
}

// conflictObservation 讓同一個名冊列再看到一個不同的 machine-id ——
// 那就是身分衝突的定義（通常是複製了資料目錄，或是從同一個 image 開出來的機器）。
func conflictObservation(t *testing.T, st *store.Store, id string) {
	t.Helper()
	now := time.Now().UTC()
	b := batch(now.Add(-time.Minute))
	b.Identity.MachineIDHint = "bbbbbbbbbbbb"
	if err := st.RecordObservation(id, b, now.Add(-time.Minute)); err != nil {
		t.Fatalf("conflict observation: %v", err)
	}
}

func checkin(sentAt time.Time) model.Checkin {
	jobsEnabled := true
	return model.Checkin{
		SchemaVersion: model.SchemaVersion, SentAt: sentAt, AgentVersion: "0.1.0",
		BootID: "boot-1", AgentSeq: 1, UptimeSeconds: func() *int64 { v := int64(86400); return &v }(),
		DiskFreeBytes: 50 << 30, DiskTotalBytes: 100 << 30,
		JobsEnabled: &jobsEnabled,
	}
}

// 上線一台什麼都好的機器，回傳它的 id。
func onlineMachine(t *testing.T, st *store.Store, name string) string {
	t.Helper()
	now := time.Now().UTC()
	id := enroll(t, st, name, now.Add(-time.Hour))
	if err := st.RecordObservation(id, batch(now.Add(-2*time.Minute)), now.Add(-2*time.Minute)); err != nil {
		t.Fatalf("observation: %v", err)
	}
	for i := 5; i >= 0; i-- {
		at := now.Add(-time.Duration(i) * 2 * time.Minute)
		if err := st.RecordCheckin(id, checkin(at), at); err != nil {
			t.Fatalf("checkin: %v", err)
		}
	}
	return id
}

func createWebJob(t *testing.T, st *store.Store, machineID string, irreversible bool) (string, string, time.Time) {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Second)
	digest := strings.Repeat("1", 64)
	if _, err := st.DB().Exec(`UPDATE machine_registry SET channel='canary' WHERE machine_id=?`, machineID); err != nil {
		t.Fatalf("設定 canary fixture：%v", err)
	}
	_, jobs, err := st.CreateDeployment(store.NewDeployment{
		Channel: "canary", ResourceKind: "openclaw", ResourceID: "openclaw",
		Spec:      `{"kind":"openclaw","version":"2026.6.10","artifact":{"sha256":"` + digest + `"}}`,
		BatchSize: 1, CreatedBy: "web 測試",
		Targets: []store.NewDeploymentTarget{{MachineID: machineID, BatchNo: 1}},
		Job: store.NewJob{
			ArtifactDigest: "sha256:" + digest, Irreversible: irreversible, ExecutionTimeout: 30,
		},
	})
	if err != nil || len(jobs) != 1 {
		t.Fatalf("建立工作單失敗：%v", err)
	}
	jobID := jobs[0].JobID
	token, err := st.ClaimJob(jobID, machineID, now, time.Hour)
	if err != nil {
		t.Fatalf("領取工作單失敗：%v", err)
	}
	return jobID, token, now
}

func startWebJob(t *testing.T, st *store.Store, machineID, jobID, token string, now time.Time) {
	t.Helper()
	if err := st.AppendJobEvent(jobID, machineID, token, 1, "start", `{}`, now.Add(80*time.Second), now); err != nil {
		t.Fatalf("寫 start 事件失敗：%v", err)
	}
	if _, err := st.AdvanceJobByAgent(jobID, machineID, token, deploy.Start, now); err != nil {
		t.Fatalf("推進 running 失敗：%v", err)
	}
}

func failWebJob(t *testing.T, st *store.Store, machineID, jobID, token string, now time.Time) {
	t.Helper()
	startWebJob(t, st, machineID, jobID, token, now)
	if _, err := st.AdvanceJobByHub(jobID, deploy.Timeout, now.Add(time.Minute)); err != nil {
		t.Fatalf("判工作單失敗：%v", err)
	}
}

func TestMachinePageListsJobsAndExplainsRealZero(t *testing.T) {
	s, st := newServer(t)
	withJob := enroll(t, st, "with-job", time.Now().UTC())
	jobID, _, _ := createWebJob(t, st, withJob, false)
	body := get(t, s, "/machines/"+withJob)
	jobs := sectionOf(t, body, `<h2 id="jobs" class="section-anchor" tabindex="-1">工作單</h2>`, "<h2>systemd unit</h2>")
	if !strings.Contains(jobs, jobID[:8]) || !strings.Contains(jobs, "sha256:11111") {
		t.Fatalf("機器頁沒有列出 job_id 與 digest 前綴：%s", jobs)
	}

	withoutJobs := enroll(t, st, "without-job", time.Now().UTC())
	body = get(t, s, "/machines/"+withoutJobs)
	jobs = sectionOf(t, body, `<h2 id="jobs" class="section-anchor" tabindex="-1">工作單</h2>`, "<h2>systemd unit</h2>")
	if !strings.Contains(jobs, "這台還沒有任何工作單") {
		t.Fatalf("零張工作單沒有說出真 0：%s", jobs)
	}
}

func TestFailedJobPageUsesOnlyRollbackEvidenceItActuallyHas(t *testing.T) {
	for _, test := range []struct {
		name     string
		rollback *bool
		want     string
		notWant  []string
	}{
		{"最後一筆 rollback 通過", boolPtr(true), "最後 rollback 驗證通過", []string{"驗證失敗", "沒有退回的證據"}},
		{"最後一筆 rollback 失敗", boolPtr(false), "最後 rollback 驗證失敗", []string{"驗證通過", "沒有退回的證據"}},
		{"沒有 rollback", nil, "沒有退回的證據", []string{"rollback 結果為通過", "rollback 結果為失敗"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			s, st := newServer(t)
			machineID := enroll(t, st, "failed-machine", time.Now().UTC())
			jobID, token, now := createWebJob(t, st, machineID, false)
			startWebJob(t, st, machineID, jobID, token, now)
			if test.rollback != nil {
				if err := st.RecordVerification(jobID, machineID, token, "rollback", "rollback && health",
					1, "rollback output", "", *test.rollback, now); err != nil {
					t.Fatalf("寫 rollback 證據失敗：%v", err)
				}
			}
			if _, err := st.AdvanceJobByHub(jobID, deploy.Timeout, now.Add(time.Minute)); err != nil {
				t.Fatalf("判工作單失敗：%v", err)
			}
			body := get(t, s, "/jobs/"+jobID)
			if !strings.Contains(body, test.want) {
				t.Fatalf("job 頁沒有正確 rollback 句子 %q", test.want)
			}
			for _, bad := range test.notWant {
				if strings.Contains(body, bad) {
					t.Errorf("job 頁混入不該出現的 rollback 句子 %q", bad)
				}
			}
		})
	}
}

// 這一頁的 $v.Rollback 是從被截斷的視窗算的，不是帳本欄位。
// 所以視窗被切掉時，「沒有退回的證據」是一句假話。
// fixture 刻意把 rollback 放在最舊一筆，讓帳本與畫面真的不一致。
// 今天出貨的 agent 一輪只送個位數個 rule_id（stage／switch／health／
// rollback 之類），現實機隊不會自己撞到 101 筆；這一支守的是 Hub 對
// 「協定收得下的輸入」的推論，不是今天的機隊。
func TestFailedJobPageExplainsRollbackOutsideVerificationWindow(t *testing.T) {
	s, st := newServer(t)
	machineID := enroll(t, st, "truncated-rollback-machine", time.Now().UTC())
	jobID, token, now := createWebJob(t, st, machineID, false)
	startWebJob(t, st, machineID, jobID, token, now)
	if err := st.RecordVerification(jobID, machineID, token, "rollback", "rollback && health",
		0, "rollback passed", "", true, now.Add(-200*time.Second)); err != nil {
		t.Fatalf("寫最舊的 rollback 證據失敗：%v", err)
	}
	for i := 0; i < store.MaxJobReadPageSize; i++ {
		ruleID := fmt.Sprintf("rule-%03d", i)
		verifiedAt := now.Add(-150*time.Second + time.Duration(i)*time.Second)
		if err := st.RecordVerification(jobID, machineID, token, ruleID, "verify",
			0, "passed", "", true, verifiedAt); err != nil {
			t.Fatalf("寫第 %d 筆較新的驗證證據失敗：%v", i, err)
		}
	}
	if _, err := st.AdvanceJobByHub(jobID, deploy.Timeout, now.Add(time.Minute)); err != nil {
		t.Fatalf("判工作單失敗：%v", err)
	}

	body := get(t, s, "/jobs/"+jobID)
	if !strings.Contains(body, "最新 100 筆驗證結果裡沒有看到退回證據；更早的結果未顯示") {
		t.Fatalf("頁面沒說這份驗證結果被切掉了。")
	}
	if strings.Contains(body, "沒有退回的證據") {
		t.Fatalf("帳本裡有一筆 passed 的 rollback 驗證，只是比最新 100 筆更早；頁面卻對 operator 說謊，告訴他沒有退回的證據，他會去重新部署一台其實已經退回的機器。")
	}
	if !strings.Contains(body, "依 agent 回報時間顯示最近 100 筆。") {
		t.Fatalf("頁面沒有提示 operator 驗證清單只顯示最近 100 筆。")
	}
}

func boolPtr(v bool) *bool { return &v }

func TestJobPageManualInterventionAndEscapesVerificationOutput(t *testing.T) {
	s, st := newServer(t)
	machineID := enroll(t, st, "manual-machine", time.Now().UTC())
	jobID, token, now := createWebJob(t, st, machineID, true)
	startWebJob(t, st, machineID, jobID, token, now)
	malicious := `<script>alert(1)</script>`
	if err := st.RecordVerification(jobID, machineID, token, "health", "curl /health",
		1, "", malicious, false, now); err != nil {
		t.Fatalf("寫驗證證據失敗：%v", err)
	}
	if _, err := st.AdvanceJobByHub(jobID, deploy.Timeout, now.Add(time.Minute)); err != nil {
		t.Fatalf("判不可逆工作單逾時失敗：%v", err)
	}
	body := get(t, s, "/jobs/"+jobID)
	if !strings.Contains(body, `<p class="st red">沒有回退證據</p>`) {
		t.Error("manual_intervention 沒有顯示回退證據狀態")
	}
	if strings.Contains(body, malicious) || !strings.Contains(body, "&lt;script&gt;alert(1)&lt;/script&gt;") {
		t.Error("stderr excerpt 沒有交給 html/template 自動轉義")
	}
}

// 2026-09-06 D5（job f159f75f）：不可逆的單在 stage 就被逾時砍掉，機器一個檔案都沒動，
// 頁面卻寫「機器停在中間」。停在哪一步要看證據講，不看狀態名。
func TestJobPageManualInterventionSaysWhereItStoppedFromEvidenceNotFromState(t *testing.T) {
	s, st := newServer(t)
	machineID := enroll(t, st, "stage-only-machine", time.Now().UTC())
	midway := `<p class="st red">沒有回退證據</p>`

	// 只有 agent 回報的 stage 證據：不能推論實際上是否切換過。
	jobID, token, now := createWebJob(t, st, machineID, true)
	startWebJob(t, st, machineID, jobID, token, now)
	if err := st.RecordVerification(jobID, machineID, token, "stage", "npm install 並驗證 staging release",
		1, "", "staged OpenClaw --version 失敗：signal: killed", false, now); err != nil {
		t.Fatalf("寫驗證證據失敗：%v", err)
	}
	if _, err := st.AdvanceJobByHub(jobID, deploy.Timeout, now.Add(time.Minute)); err != nil {
		t.Fatalf("判不可逆工作單逾時失敗：%v", err)
	}
	body := get(t, s, "/jobs/"+jobID)
	if !strings.Contains(body, "目前只有 stage 驗證") || strings.Contains(body, midway) {
		t.Errorf("只有 stage 證據的不可逆單被寫成已知機器狀態：%s", sectionOf(t, body, "狀態", "desired spec"))
	}
	// In production the deployment driver closes this one-target attempt before
	// an operator can open a new attempt. Keep the page fixture faithful to the
	// resource-owner invariant instead of leaving a terminal job's deployment
	// spuriously running.
	var deploymentID string
	if err := st.DB().QueryRow(`SELECT deployment_id FROM deployment_targets WHERE job_id=?`, jobID).Scan(&deploymentID); err != nil {
		t.Fatal(err)
	}
	if changed, err := st.SetDeploymentState(deploymentID, store.DeploymentRunning, store.DeploymentPaused, now.Add(2*time.Minute)); err != nil || !changed {
		t.Fatalf("pause first page fixture changed=%v err=%v", changed, err)
	}
	if finished, opened, err := st.ContinueDeployment(deploymentID, now.Add(2*time.Minute)); err != nil ||
		finished.State != store.DeploymentFinished || len(opened) != 0 {
		t.Fatalf("finish first page fixture through Continue: deployment=%+v opened=%+v err=%v", finished, opened, err)
	}

	// 一筆證據都沒有：不知道停在哪一步，兩種都不准講。
	jobID2, token2, now2 := createWebJob(t, st, machineID, true)
	startWebJob(t, st, machineID, jobID2, token2, now2)
	if _, err := st.AdvanceJobByHub(jobID2, deploy.Timeout, now2.Add(time.Minute)); err != nil {
		t.Fatalf("判不可逆工作單逾時失敗：%v", err)
	}
	body = get(t, s, "/jobs/"+jobID2)
	if !strings.Contains(body, "尚無終態驗證證據") || strings.Contains(body, midway) || strings.Contains(body, "沒有切換過") {
		t.Errorf("沒有證據的不可逆單講了它不知道的事：%s", sectionOf(t, body, "狀態", "desired spec"))
	}
}

func TestSucceededJobPageDoesNotOverclaimIndependentEvidenceAndKeepsBothClocks(t *testing.T) {
	s, st := newServer(t)
	machineID := enroll(t, st, "succeeded-machine", time.Now().UTC())
	jobID, token, now := createWebJob(t, st, machineID, false)
	startWebJob(t, st, machineID, jobID, token, now)
	if _, err := st.AdvanceJobByAgent(jobID, machineID, token, deploy.FinishWork, now); err != nil {
		t.Fatalf("推進 verifying 失敗：%v", err)
	}
	if err := st.RecordVerification(jobID, machineID, token, "health", "curl /health", 0, "ok", "", true, now); err != nil {
		t.Fatalf("寫驗證失敗：%v", err)
	}
	if _, err := st.MarkSucceededIfVerified(jobID, now); err != nil {
		t.Fatalf("標成功失敗：%v", err)
	}
	body := get(t, s, "/jobs/"+jobID)
	for _, want := range []string{
		"已保存的驗證均通過",
		"producer", "role", "provenance recorded",
		"來源", "occurred_at", "received_at（Hub）",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("成功證據頁缺少 %q", want)
		}
	}
	if strings.Contains(body, "Hub 看過獨立的驗證證據才判的") {
		t.Error("成功證據頁仍宣稱目前不存在的獨立 verifier")
	}
}

func TestRejectedJobPageShowsStoredCodeAndHonestMissingDetail(t *testing.T) {
	s, st := newServer(t)
	machineID := enroll(t, st, "rejected-machine", time.Now().UTC())
	jobID, token, now := createWebJob(t, st, machineID, false)
	payload := `{"rejection_code":"PRECONDITION_FAILED"}`
	if err := st.AppendJobEvent(jobID, machineID, token, 7, "rejected", payload, now, now); err != nil {
		t.Fatalf("寫拒絕事件失敗：%v", err)
	}
	if _, err := st.AdvanceJobByAgent(jobID, machineID, token, deploy.Reject, now); err != nil {
		t.Fatalf("推進 rejected 失敗：%v", err)
	}
	body := get(t, s, "/jobs/"+jobID)
	if !strings.Contains(body, "拒絕碼 PRECONDITION_FAILED；來源 rejected-machine；detail —") {
		t.Fatalf("拒絕頁沒有如實顯示有 code、沒 detail：%s", around(body, "拒絕碼"))
	}
}

func TestRejectedJobPageLabelsAndEscapesAgentReportedDetail(t *testing.T) {
	s, st := newServer(t)
	machineID := enroll(t, st, "rejected-detail-machine", time.Now().UTC())
	jobID, token, now := createWebJob(t, st, machineID, false)
	malicious := `<script>alert("RAW_REJECTION_DETAIL_SENTINEL")</script>`
	payload, err := json.Marshal(model.JobRejectEventPayload{
		RejectionCode: string(deploy.PreconditionFailed), Detail: malicious,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AppendJobEvent(jobID, machineID, token, 7, "rejected", string(payload), now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AdvanceJobByAgent(jobID, machineID, token, deploy.Reject, now); err != nil {
		t.Fatal(err)
	}
	body := get(t, s, "/jobs/"+jobID)
	if !strings.Contains(body, "來源 rejected-detail-machine；detail（原文）：") || strings.Contains(body, malicious) ||
		!strings.Contains(body, `<pre class="copy">&lt;script&gt;alert(&#34;RAW_REJECTION_DETAIL_SENTINEL&#34;)&lt;/script&gt;</pre>`) {
		t.Fatalf("agent rejection detail was not explicitly labelled and escaped: %s", around(body, "拒絕碼"))
	}
}

func TestRejectedJobPageDoesNotCallScrubbedDetailVerbatim(t *testing.T) {
	s, st := newServer(t)
	machineID := enroll(t, st, "rejected-scrubbed-detail-machine", time.Now().UTC())
	jobID, token, now := createWebJob(t, st, machineID, false)
	payload, err := json.Marshal(model.JobRejectEventPayload{
		RejectionCode: string(deploy.PreconditionFailed), Detail: "unsafe\x01detail",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AppendJobEvent(jobID, machineID, token, 7, "rejected", string(payload), now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AdvanceJobByAgent(jobID, machineID, token, deploy.Reject, now); err != nil {
		t.Fatal(err)
	}
	body := get(t, s, "/jobs/"+jobID)
	if strings.Contains(body, "；detail（原文）：") ||
		!strings.Contains(body, "；detail：") ||
		!strings.Contains(body, "（含已替換的控制／非法字元）") {
		t.Fatalf("scrubbed rejection detail has dishonest verbatim label or lacks replacement label: %s",
			around(body, "拒絕碼"))
	}
}

func TestJobPageShowsRuleIDTruncationAt256Bytes(t *testing.T) {
	s, st := newServer(t)
	machineID := enroll(t, st, "rule-limit-machine", time.Now().UTC())
	jobID, token, now := createWebJob(t, st, machineID, false)
	startWebJob(t, st, machineID, jobID, token, now)
	ruleID := strings.Repeat("r", 300)
	if _, err := st.DB().Exec(`INSERT INTO verification_results
 (verification_id,job_id,machine_id,rule_id,command,exit_code,stdout_excerpt,stderr_excerpt,passed,verified_at)
 VALUES (?,?,?,?,?,?,?,?,?,?)`, "long-rule-verification", jobID, machineID, ruleID,
		"true", 0, "ok", "", true, now.Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	body := get(t, s, "/jobs/"+jobID)
	if !strings.Contains(body, "（已截斷至 256 bytes；帳本原文 300 bytes）") ||
		strings.Contains(body, "（已截斷至 16 KiB；帳本原文 300 bytes）") {
		t.Fatalf("rule_id truncation label did not report its actual limit: %s",
			around(body, "帳本原文 300 bytes"))
	}
}

func TestJobPagePreservesMultilineStdoutAndScrubsUnsafeControls(t *testing.T) {
	s, st := newServer(t)
	machineID := enroll(t, st, "multiline-stdout-machine", time.Now().UTC())
	jobID, token, now := createWebJob(t, st, machineID, false)
	startWebJob(t, st, machineID, jobID, token, now)
	rawStdout := "ActiveState=active\n\tMainPID=2424956\x1b[31m\rExecStart=node"
	if err := st.RecordVerification(jobID, machineID, token, "unit_execstart", "systemctl show",
		0, rawStdout, "", true, now); err != nil {
		t.Fatal(err)
	}

	body := get(t, s, "/jobs/"+jobID)
	want := "ActiveState=active\n\tMainPID=2424956�[31m�ExecStart=node"
	if !strings.Contains(body, `<pre class="copy">`+want+`</pre>`) || strings.ContainsRune(body, '\x1b') {
		t.Fatalf("multiline stdout was not preserved and safely scrubbed: %s", around(body, "ActiveState"))
	}
}

func TestJobPageRendersTypedEvidenceWithTruncationAndProvenance(t *testing.T) {
	s, st := newServer(t)
	machineID := enroll(t, st, "typed-evidence-machine", time.Now().UTC())
	jobID, token, now := createWebJob(t, st, machineID, false)
	startWebJob(t, st, machineID, jobID, token, now)
	command := "printf '\x1b[31mred'"
	rawStderr := strings.Repeat("TYPED_STDERR_EVIDENCE_", 1000)
	if err := st.RecordVerification(jobID, machineID, token, "health", command,
		1, "", rawStderr, false, now); err != nil {
		t.Fatal(err)
	}
	payload := `{"rejection_code":"FUTURE_REJECTION","detail":"future detail"}`
	if err := st.AppendJobEvent(jobID, machineID, token, 8, "rejected", payload, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AdvanceJobByAgent(jobID, machineID, token, deploy.Reject, now); err != nil {
		t.Fatal(err)
	}
	body := get(t, s, "/jobs/"+jobID)
	for _, want := range []string{
		"（已截斷至 16 KiB；帳本原文 ",
		"（含已替換的控制／非法字元）",
		"FUTURE_REJECTION（未知拒絕碼）",
		"received_at（Hub）",
		"provenance recorded",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("typed evidence page missing %q", want)
		}
	}
	if strings.ContainsRune(body, '\x1b') || strings.Contains(body, rawStderr) {
		t.Fatal("typed evidence page exposed raw control byte or unbounded stderr")
	}
}

func TestLeaseExpiredJobPageExplainsMachineStateIsStillUnknown(t *testing.T) {
	s, st := newServer(t)
	machineID := enroll(t, st, "expired-machine", time.Now().UTC())
	jobID, _, now := createWebJob(t, st, machineID, false)
	if _, err := st.AdvanceJobByHub(jobID, deploy.LeaseLost, now.Add(time.Hour)); err != nil {
		t.Fatalf("推進 lease_expired 失敗：%v", err)
	}
	body := get(t, s, "/jobs/"+jobID)
	if !strings.Contains(body, "租約已過期") {
		t.Error("lease_expired 頁沒有顯示租約狀態")
	}
}

func TestJobPageNotFoundIs404(t *testing.T) {
	s, _ := newServer(t)
	mux := http.NewServeMux()
	s.Routes(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/jobs/no-such-job", nil))
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "找不到這張工作單") {
		t.Fatalf("找不到 job 回應 %d：%s", rec.Code, rec.Body.String())
	}
}

// ---------------------------------------------------------------- 分母

// TestDashboardKeepsTheSilentMachineOnScreen 是這個檔案裡最重要的測試。
//
// 名冊上 5 台、只有 4 台報到 —— 那 1 台必須還在畫面上，而且第一句話要
// 講出這個落差。實測案例：sampleagent3 在某台 client 上隱形了七週，因為那個
// 畫面只畫「有回報的機器」。這個產品存在的理由就是不要再發生那件事。
func TestDashboardKeepsTheSilentMachineOnScreen(t *testing.T) {
	s, st := newServer(t)
	for _, n := range []string{"samplehub1", "sampleagent1", "sampleagent2", "sampleagent4"} {
		onlineMachine(t, st, n)
	}
	// 第 5 台只登記在名冊上，從來沒講過話。
	enroll(t, st, "sampleagent3", time.Now().UTC().Add(-72*time.Hour))

	body := get(t, s, "/")

	if !strings.Contains(body, "sampleagent3") {
		t.Fatal("從未報到的機器從畫面上消失了 —— 這正是這個產品要修的那個 bug")
	}
	if !strings.Contains(body, "名冊上 5 台，4 台正在回報。") {
		t.Errorf("第一句話沒有講出分母落差，實際內容：\n%s", firstHeadline(body))
	}
	if !strings.Contains(body, "從未報到") {
		t.Error("沒有把「從未報到」講成一個狀態")
	}
}

// TestAdminCenterShellAndOverviewSummary 守的是控制面的資訊架構，不只是配色。
// 首頁要先給可掃讀的摘要，再讓人往機器與例外下鑽；所有主要頁則共用同一組
// 產品導覽。這些 semantic hooks 也讓鍵盤、螢幕閱讀器與之後的 browser test
// 不必靠畫面座標猜按鈕在哪裡。
func TestAdminCenterShellAndOverviewSummary(t *testing.T) {
	s, st := newServer(t)
	id := onlineMachine(t, st, "samplehub1")

	pages := []string{
		"/",
		"/machines",
		"/machines/" + id,
		"/apps",
		"/deployments",
		"/updates",
		"/reports",
		"/reports/tickets",
		"/machines/" + id + "/timeline",
		"/audit",
		"/tenant/maintenance",
	}
	for _, path := range pages {
		body := get(t, s, path)
		for _, want := range []string{
			`class="topbar"`,
			`aria-label="主要導覽"`,
			`id="main-content"`,
			`href="/apps"`,
			`href="/jobs"`,
			`href="/reports"`,
			`href="/tenant/maintenance"`,
			"端點安全性",
			"租用戶管理",
			"疑難排解 &#43; 支援",
		} {
			if !strings.Contains(body, want) {
				t.Errorf("GET %s 缺少 admin-center shell %q", path, want)
			}
		}
		if got := strings.Count(body, "<h1"); got != 1 {
			t.Errorf("GET %s 有 %d 個 h1，主要頁必須恰好一個", path, got)
		}
	}

	dashboard := get(t, s, "/")
	for _, want := range []string{
		"<h1>儀表板</h1>",
		`class="metric-grid"`,
		"受管理裝置",
		"正在回報",
		"需要查看",
		"目前發現",
		`id="kpi-managed" href="/machines?lifecycle=active#machines"`,
		`id="kpi-reporting" href="/machines?lifecycle=active&amp;reporting=true#machines"`,
		`id="kpi-attention" href="/machines?lifecycle=active&amp;state=Degraded&amp;state=Unreachable&amp;state=NeverReported&amp;state=IdentityConflict#machines"`,
		`id="kpi-findings" href="#`,
	} {
		if !strings.Contains(dashboard, want) {
			t.Errorf("總覽缺少可掃讀的摘要 %q", want)
		}
	}
	if !strings.Contains(dashboard, `aria-current="page"`) {
		t.Error("總覽的目前導覽項目沒有 aria-current")
	}
}

func TestAuditPageExposesOperatorRequestCorrelation(t *testing.T) {
	s, st := newServer(t)
	if err := st.RecordAudit(store.AuditEntry{
		Action: store.AuditMachineChannel, Subject: "samplehub1", SourceAddr: "100.64.0.7",
		IdempotencyKey: "operator-key-1", RequestDigest: "sha256:canonical-body",
		UserAgent: "clawctl-hub-operator/1", OK: true,
	}); err != nil {
		t.Fatal(err)
	}
	body := get(t, s, "/audit")
	for _, want := range []string{"Request 關聯", "operator-key-1", "sha256:canonical-body", "UA clawctl-hub-operator/1"} {
		if !strings.Contains(body, want) {
			t.Errorf("audit page 沒有顯示 operator correlation %q", want)
		}
	}
}

func TestMachinesNavigationHasItsOwnTruthfulPageIdentity(t *testing.T) {
	s, _ := newServer(t)
	body := get(t, s, "/machines")
	for _, want := range []string{
		"<title>裝置 · AI-Intune</title>", "<h1>裝置</h1>",
		`href="/machines#machines" class="nav-item on" aria-current="page"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("machines index missing %q", want)
		}
	}
	if strings.Contains(body, "<h1>機隊總覽</h1>") {
		t.Error("machines current nav still identifies the page as fleet overview")
	}
}

func TestCanonicalChannelFormHasAccessibleRequiredNames(t *testing.T) {
	s, st := newServer(t)
	id := onlineMachine(t, st, "cnode-accessible")
	body := get(t, s, "/machines/"+id)
	for _, want := range []string{
		`<span class="field-label">部署通道</span>`,
		`<select name="channel" style="padding:6px" required aria-required="true">`,
		`<span class="field-label">確認機器名稱</span>`,
		`autocomplete="off" required aria-required="true">`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("channel form missing accessible contract %q", want)
		}
	}
}

func TestRetiredMachineDetailDoesNotPresentLiveAlarmOrSchedule(t *testing.T) {
	s, st := newServer(t)
	id := onlineMachine(t, st, "cnode-retired")
	old := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano)
	if _, err := st.DB().Exec(`UPDATE machine_checkins SET received_at=? WHERE machine_id=?`, old, id); err != nil {
		t.Fatal(err)
	}
	if err := st.RetireMachine(id, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	body := get(t, s, "/machines/"+id)
	if !strings.Contains(body, "<h1>cnode-retired —— 已退役</h1>") ||
		!strings.Contains(body, "退役前觀測已保留") {
		t.Fatalf("retired detail lacks neutral heading: %s", body)
	}
	for _, bad := range []string{"幾點會判失聯", "下一次應該到", "上面第一屏的理由才是即時重算的"} {
		if strings.Contains(body, bad) {
			t.Errorf("retired detail still presents live schedule copy %q", bad)
		}
	}
}

func TestActionBackLabelMatchesDestination(t *testing.T) {
	tests := map[string]string{
		"/machines/id":        "回那台機器",
		"/deployments/id":     "回該部署",
		"/":                   "回總覽",
		"/unexpected-section": "回上一個控制面頁面",
	}
	for back, want := range tests {
		if got := actionBackLabel(back); got != want {
			t.Errorf("actionBackLabel(%q)=%q, want %q", back, got, want)
		}
	}
}

func TestAuditPageDoesNotMislabelOperatorTransportRejectionAsLegacy(t *testing.T) {
	s, st := newServer(t)
	if err := st.RecordAudit(store.AuditEntry{
		Action: store.AuditMachineChannel, MachineID: "missing-machine", Subject: "missing-machine",
		SourceAddr: "100.64.0.7", IdempotencyKey: "transport-key", OK: false,
		Detail: store.OperatorTransportRejectionPrefix + "BAD_REQUEST: JSON 解析失敗：" + strings.Repeat("x", 700),
	}); err != nil {
		t.Fatal(err)
	}
	body := get(t, s, "/audit")
	for _, want := range []string{"transport-key", "digest —", "transport rejection"} {
		if !strings.Contains(body, want) {
			t.Errorf("transport rejection audit 沒有顯示 %q", want)
		}
	}
	if strings.Contains(body, "legacy / 非 canonical operator API") {
		t.Error("operator transport rejection 被誤標成 legacy / 非 canonical operator API")
	}
}

func TestAuditPageLabelsAuthorizationByEvaluationStage(t *testing.T) {
	tests := []struct {
		name      string
		entry     store.AuditEntry
		want      string
		forbidden []string
	}{
		{
			name: "actual authorization decision wins",
			entry: store.AuditEntry{
				Action: store.AuditMachineChannel, AuthDecision: "AUTHORIZED",
				BoundaryDecision: "PASSED", SourceKind: "operator-api",
			},
			want: `<b>授權判決</b> <span class="mono">AUTHORIZED</span>`,
			forbidden: []string{
				"多筆 denial 已聚合；授權逐筆結果未保留",
				"授權判決：未評估（先被 request guard 擋下）",
				"授權證據：不適用（本機 direct-DB process）",
				"授權證據：legacy／未接入",
			},
		},
		{
			name: "rate-limited denials are aggregated",
			entry: store.AuditEntry{
				Action: store.AuditOperatorDenied, BoundaryDecision: "OPERATOR_DENIALS_RATE_LIMITED",
				SourceKind: "operator-boundary",
			},
			want: "多筆 denial 已聚合；授權逐筆結果未保留",
			forbidden: []string{
				"授權判決：未評估（先被 request guard 擋下）",
				"授權證據：不適用（本機 direct-DB process）",
				"授權證據：legacy／未接入",
			},
		},
		{
			name: "request guard rejection was not authorized",
			entry: store.AuditEntry{
				Action: store.AuditOperatorDenied, BoundaryDecision: "HOST_REJECTED",
				SourceKind: "operator-boundary",
			},
			want: "授權判決：未評估；請求在邊界拒絕",
			forbidden: []string{
				"授權證據：不適用（本機 direct-DB process）",
				"授權證據：legacy／未接入",
			},
		},
		{
			name: "direct DB CLI has no Tailscale evidence",
			entry: store.AuditEntry{
				Action: store.AuditMachineChannel, SourceKind: "direct-db-cli",
			},
			want: "來源：direct-db-cli",
			forbidden: []string{
				"授權判決：未評估（先被 request guard 擋下）",
				"授權證據：legacy／未接入",
			},
		},
		{
			name: "pre-boundary row remains legacy",
			entry: store.AuditEntry{
				Action: store.AuditMachineChannel, Detail: "舊版 Web form channel action",
			},
			want: "授權證據：未記錄",
			forbidden: []string{
				"授權判決：未評估（先被 request guard 擋下）",
				"授權證據：不適用（本機 direct-DB process）",
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s, st := newServer(t)
			test.entry.Subject = test.name
			test.entry.SourceAddr = "100.64.0.7"
			if err := st.RecordAudit(test.entry); err != nil {
				t.Fatal(err)
			}
			body := get(t, s, "/audit")
			if !strings.Contains(body, test.want) {
				t.Fatalf("audit authorization label missing %q: %s", test.want, body)
			}
			for _, forbidden := range test.forbidden {
				if strings.Contains(body, forbidden) {
					t.Errorf("audit authorization label unexpectedly contains %q", forbidden)
				}
			}
		})
	}
}

func TestAuditPageSamplesDenialsWithoutHidingOlderDomainAudit(t *testing.T) {
	s, st := newServer(t)
	if err := st.RecordAudit(store.AuditEntry{
		Action: store.AuditRetire, Subject: "domain-row-must-remain-visible",
		SourceAddr: "local-test", OK: true,
	}); err != nil {
		t.Fatal(err)
	}
	for i := range 210 {
		if err := st.RecordAudit(store.AuditEntry{
			Action: store.AuditOperatorDenied, Subject: fmt.Sprintf("audit-ui-denial-%03d", i),
			SourceAddr: "100.64.0.7", OK: false,
		}); err != nil {
			t.Fatal(err)
		}
	}

	body := get(t, s, "/audit")
	for _, want := range []string{
		"domain-row-must-remain-visible",
		"audit-ui-denial-209",
		"audit-ui-denial-160",
		"Operator denials：符合 210、納入 50、省略 160",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("sampled audit page missing %q", want)
		}
	}
	if strings.Contains(body, "audit-ui-denial-159") {
		t.Error("sampled audit page showed an operator denial beyond the fixed cap")
	}
}

func TestAuditPageLabelsSuccessfulReplayAsNotRepeated(t *testing.T) {
	s, st := newServer(t)
	const subject = "successful-replay-row-only"
	if err := st.RecordAudit(store.AuditEntry{
		Action: store.AuditMachineChannel, Subject: subject, SourceAddr: "local-test", OK: true,
		IdempotencyKey: "replay-key", RequestDigest: "sha256:replay",
		Detail: store.OperatorIdempotencyReplayPrefix + "沒有再次改 state 或 revision",
	}); err != nil {
		t.Fatal(err)
	}
	body := get(t, s, "/audit")
	row := auditRowContaining(t, body, subject)
	if !strings.Contains(row, "回放（未重做）") {
		t.Fatalf("successful replay looks like a second mutation: %s", row)
	}
	if strings.Contains(row, "做了") {
		t.Fatalf("successful replay includes did-it copy: %s", row)
	}
}

func TestAuditPageLabelsEnrollmentLimitReplayAsNotRepeated(t *testing.T) {
	s, st := newServer(t)
	const subject = "enrollment-limit-replay-row-only"
	if err := st.RecordAudit(store.AuditEntry{
		Action: store.AuditEnrollmentLimit, Subject: subject, SourceAddr: "local-test", OK: true,
		Detail: store.OperatorIdempotencyReplayPrefix + "沒有再次套用上限",
	}); err != nil {
		t.Fatal(err)
	}
	body := get(t, s, "/audit")
	row := auditRowContaining(t, body, subject)
	if !strings.Contains(row, "回放（未重做）") {
		t.Fatalf("enrollment-limit replay looks like a second mutation: %s", row)
	}
	if strings.Contains(row, "做了") {
		t.Fatalf("enrollment-limit replay includes did-it copy: %s", row)
	}
}

func TestAuditPageShowsMissingDigestForEnrollmentLimitTransportRejection(t *testing.T) {
	s, st := newServer(t)
	const subject = "enrollment-limit-transport-rejection-row-only"
	if err := st.RecordAudit(store.AuditEntry{
		Action: store.AuditEnrollmentLimit, Subject: subject, SourceAddr: "local-test", OK: false,
		IdempotencyKey: "enrollment-limit-transport-key",
		Detail:         store.OperatorTransportRejectionPrefix + "FORM_INVALID: canonical request digest 無法取得",
	}); err != nil {
		t.Fatal(err)
	}
	body := get(t, s, "/audit")
	row := auditRowContaining(t, body, subject)
	for _, want := range []string{"enrollment-limit-transport-key", "digest —"} {
		if !strings.Contains(row, want) {
			t.Errorf("enrollment-limit transport rejection row missing %q: %s", want, row)
		}
	}
}

func auditRowContaining(t *testing.T, body, marker string) string {
	t.Helper()
	markerAt := strings.Index(body, marker)
	if markerAt < 0 {
		t.Fatalf("audit page missing row marker %q", marker)
	}
	rowStart := strings.LastIndex(body[:markerAt], "<tr>")
	rowEndOffset := strings.Index(body[markerAt:], "</tr>")
	if rowStart < 0 || rowEndOffset < 0 {
		t.Fatalf("audit row containing %q is malformed", marker)
	}
	return body[rowStart : markerAt+rowEndOffset+len("</tr>")]
}

func TestAuditMachineFilterShowsScopeAndTruthfulEmptyState(t *testing.T) {
	s, st := newServer(t)
	id := enroll(t, st, "cnode-filtered", time.Now().UTC())
	if err := st.RecordAudit(store.AuditEntry{
		Action: store.AuditConnect, MachineID: "someone-else", Subject: "other-machine",
		SourceAddr: "local-test", OK: true,
	}); err != nil {
		t.Fatal(err)
	}
	body := get(t, s, "/audit?machine="+id)
	for _, want := range []string{"稽核記錄：" + id, "目前只看這台機器", "清除篩選", "這台機器還沒有任何一筆"} {
		if !strings.Contains(body, want) {
			t.Errorf("filtered audit page missing %q", want)
		}
	}
	if strings.Contains(body, "other-machine") {
		t.Error("machine-scoped audit leaked a different machine's row")
	}
}

func TestAuditMachineFilterDoesNotBypassSafeProjectionForDisplayName(t *testing.T) {
	s, st := newServer(t)
	id := enroll(t, st, "legacy-display", time.Now().UTC())
	if _, err := st.DB().Exec(`UPDATE machine_registry SET display_name = ? WHERE machine_id = ?`,
		"safe\u202etxt", id); err != nil {
		t.Fatal(err)
	}
	body := get(t, s, "/audit?machine_id="+id)
	if !strings.Contains(body, "稽核記錄："+id) || strings.ContainsRune(body, '\u202e') {
		t.Fatalf("audit scope heading bypassed safe projection: %s", body)
	}
}

func TestAuditPageKeepsPreCanonicalMachineChannelRowLabelledLegacy(t *testing.T) {
	s, st := newServer(t)
	if err := st.RecordAudit(store.AuditEntry{
		Action: store.AuditMachineChannel, Subject: "samplehub1", SourceAddr: "local-test",
		Detail: "舊版 Web form channel action", OK: true,
	}); err != nil {
		t.Fatal(err)
	}
	body := get(t, s, "/audit")
	if !strings.Contains(body, "授權證據：未記錄") ||
		strings.Contains(body, "canonical digest 無法取得（transport rejection）") {
		t.Fatalf("legacy machine-channel audit 被誤標：%s", body)
	}
}

func TestAuditPageUsesCanonicalServiceFiltersWithoutMutatingLedger(t *testing.T) {
	s, st := newServer(t)
	machineID := enroll(t, st, "cnode-audit-filter", time.Now().UTC())
	at := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	if err := st.RecordAudit(store.AuditEntry{
		At: at, Action: store.AuditMachineChannel, MachineID: machineID,
		Subject: "target-audit-row", Reason: "planned", IdempotencyKey: "web-audit-key",
		RequestDigest: "sha256:web-audit", SourceAddr: "100.64.0.7", WhoUser: "legacy@example.com",
		AuthSubject: "tailscale-user:42", AuthCapability: "example.com/cap/clawctl-view",
		SourceKind: "operator-api", OK: false,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordAudit(store.AuditEntry{
		At: at, Action: store.AuditConnect, MachineID: "other-machine",
		Subject: "decoy-audit-row", SourceAddr: "local-test", OK: true,
	}); err != nil {
		t.Fatal(err)
	}
	before := auditWebRowCount(t, st)
	path := "/audit?machine_id=" + machineID + "&action=machine-channel&outcome=failed" +
		"&principal=tailscale-user%3A42&capability=example.com%2Fcap%2Fclawctl-view" +
		"&source_kind=operator-api&correlation=web-audit-key" +
		"&from=2026-09-08T08%3A00%3A00-04%3A00&to=2026-09-08T12%3A00%3A00Z&denials=all&limit=1"
	mux := http.NewServeMux()
	s.Routes(mux)
	rec := httptest.NewRecorder()
	req := verifiedWebRequest(httptest.NewRequest(http.MethodGet, path, nil), "example.com/cap/clawctl-view")
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "no-store" ||
		!strings.Contains(rec.Body.String(), "</html>") {
		t.Fatalf("filtered audit status=%d headers=%v body=%s", rec.Code, rec.Header(), rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		"稽核記錄：" + machineID, "target-audit-row", "web-audit-key", "sha256:web-audit",
		`value="machine-channel" checked`, `value="failed" selected`,
		`value="tailscale-user:42"`, `value="example.com/cap/clawctl-view"`,
		`value="2026-09-08T12:00:00Z"`, "採樣前符合 <b>1</b>",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("filtered audit page missing %q", want)
		}
	}
	if strings.Contains(body, "decoy-audit-row") {
		t.Fatal("canonical audit filters leaked a nonmatching row")
	}
	if after := auditWebRowCount(t, st); after != before {
		t.Fatalf("audit HTML GET mutated ledger: %d -> %d", before, after)
	}
}

func TestAuditPageCursorPinsCreationCeilingAndPreservesFilters(t *testing.T) {
	s, st := newServer(t)
	for i := range 3 {
		if err := st.RecordAudit(store.AuditEntry{
			Action: store.AuditConnect, Subject: fmt.Sprintf("cursor-row-%d", i),
			SourceAddr: "local-test", OK: true,
		}); err != nil {
			t.Fatal(err)
		}
	}
	first := get(t, s, "/audit?action=connect&denials=all&limit=1")
	if !strings.Contains(first, "cursor-row-2") || strings.Contains(first, "cursor-row-1") {
		t.Fatalf("first audit page did not contain only newest row: %s", first)
	}
	match := regexp.MustCompile(`href="([^"]+)"[^>]*>下一頁（較舊的 writer sequence）`).FindStringSubmatch(first)
	if len(match) != 2 {
		t.Fatalf("first audit page has no next cursor link: %s", first)
	}
	nextHref := html.UnescapeString(match[1])
	if !strings.Contains(nextHref, "action=connect") || !strings.Contains(nextHref, "denials=all") ||
		!strings.Contains(nextHref, "limit=1") || !strings.Contains(nextHref, "cursor=") {
		t.Fatalf("next audit link lost filters: %s", nextHref)
	}
	if err := st.RecordAudit(store.AuditEntry{
		Action: store.AuditConnect, Subject: "new-row-after-ceiling", SourceAddr: "local-test", OK: true,
	}); err != nil {
		t.Fatal(err)
	}
	second := get(t, s, nextHref)
	if !strings.Contains(second, "cursor-row-1") || strings.Contains(second, "cursor-row-2") ||
		strings.Contains(second, "new-row-after-ceiling") ||
		!strings.Contains(second, "回到這次查詢的第一頁") {
		t.Fatalf("second audit page crossed ceiling or lost navigation: %s", second)
	}
}

func TestAuditPageMakesMalformedLedgerEvidenceExplicitAndSafe(t *testing.T) {
	s, st := newServer(t)
	if _, err := st.DB().Exec(`INSERT INTO audit_log
 (at,action,subject,reason,source_addr,outcome,detail) VALUES (?,?,?,?,?,?,?)`,
		"not-a-time", "future-action", "subject\u202e", "line1\nline2", "", "maybe", "detail\x1b[31m"); err != nil {
		t.Fatal(err)
	}
	body := get(t, s, "/audit?denials=all")
	for _, want := range []string{
		"時間無法判讀", "結果無法判讀", "future-action", "invalid_at", "unknown_action",
		"unknown_outcome", "empty_source_addr", "安全輸出已替換／截短",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("malformed audit page missing %q", want)
		}
	}
	if strings.ContainsRune(body, '\u202e') || strings.Contains(body, "line1\nline2") || strings.ContainsRune(body, '\x1b') {
		t.Fatalf("audit HTML retained spoofing controls: %q", body)
	}
}

func TestAuditPageRejectsAmbiguousFiltersWithoutReflection(t *testing.T) {
	s, _ := newServer(t)
	mux := http.NewServeMux()
	s.Routes(mux)
	secret := "do-not-reflect-audit-secret"
	for _, suffix := range []string{
		"?unknown=value", "?machine=a&machine_id=b", "?outcome=maybe", "?action=connect&action=connect",
		"?limit=0", "?limit=101", "?from=2026-09-08", "?cursor=" + secret,
	} {
		rec := httptest.NewRecorder()
		req := verifiedWebRequest(httptest.NewRequest(http.MethodGet, "/audit"+suffix, nil), "example.com/cap/clawctl-view")
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest || strings.Contains(rec.Body.String(), secret) {
			t.Errorf("invalid audit query %q status=%d body=%s", suffix, rec.Code, rec.Body.String())
		}
	}
}

func auditWebRowCount(t *testing.T, st *store.Store) int {
	t.Helper()
	var count int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM audit_log`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestDashboardMachineTableFiltersUseDenominatorSemantics(t *testing.T) {
	now := time.Now().UTC()
	fresh := state.Facts{EverCheckedIn: true, LastCheckinReceived: now.Add(-time.Minute), CheckinInterval: state.CheckinInterval}
	ov := store.Overview{Now: now, Machines: []store.MachineRow{
		{Machine: store.Machine{DisplayName: "online", Expected: true}, State: state.Online, Facts: fresh},
		{Machine: store.Machine{DisplayName: "degraded", Expected: true}, State: state.Degraded, Facts: fresh},
		{Machine: store.Machine{DisplayName: "conflict", Expected: true}, State: state.IdentityConflict, Facts: fresh},
		{Machine: store.Machine{DisplayName: "unreachable", Expected: true}, State: state.Unreachable},
		{Machine: store.Machine{DisplayName: "never", Expected: true}, State: state.NeverReported},
		{Machine: store.Machine{DisplayName: "observer", Expected: false}, State: state.IdentityConflict, Facts: fresh},
	}}
	names := func(rows []store.MachineRow) string {
		out := make([]string, 0, len(rows))
		for _, row := range rows {
			out = append(out, row.DisplayName)
		}
		return strings.Join(out, ",")
	}

	tests := []struct {
		filter string
		want   string
	}{
		{"", "online,degraded,conflict,unreachable,never,observer"},
		{"managed", "online,degraded,conflict,unreachable,never,observer"},
		{"reporting", "online,degraded,conflict,observer"},
		{"attention", "degraded,conflict,unreachable,never,observer"},
		{"not-a-filter", "online,degraded,conflict,unreachable,never,observer"},
	}
	for _, test := range tests {
		t.Run(test.filter, func(t *testing.T) {
			table := buildDashboardMachineTable(ov, test.filter)
			if got := names(table.Rows); got != test.want {
				t.Fatalf("filter %q rows = %q，want %q", test.filter, got, test.want)
			}
			if test.filter == "not-a-filter" && table.Filter != "" {
				t.Fatalf("未知 filter 被顯示成有效篩選：%+v", table)
			}
		})
	}
}

func TestDashboardMachineFilterIsVisibleAndClearable(t *testing.T) {
	s, st := newServer(t)
	onlineMachine(t, st, "samplehub1")

	body := get(t, s, "/machines?machines=attention")
	for _, want := range []string{
		`<option value="active" selected>Active</option>`,
		`name="state" value="Degraded" checked`,
		`name="state" value="Unreachable" checked`,
		`符合 filter 0 / registry ceiling 內 1 台`,
		`href="/machines#machines">清除</a>`,
		`目前 filter 沒有符合的機器`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("attention 篩選頁缺少 %q", want)
		}
	}
	if strings.Contains(body, `name="expected"`) {
		t.Fatal("attention filter still renders the removed expected filter")
	}
}

func TestDashboardDoesNotRenderLegacyExpectedDenominatorSuffix(t *testing.T) {
	s, st := newServer(t)
	id := onlineMachine(t, st, "legacy-false")
	if _, err := st.DB().Exec(`UPDATE machine_registry SET expected=0 WHERE machine_id=?`, id); err != nil {
		t.Fatal(err)
	}
	body := get(t, s, "/")
	if strings.Contains(body, "不算分母") {
		t.Fatalf("dashboard rendered a denominator claim from the legacy expected field: %s", around(body, "不算分母"))
	}
}

// TestDashboardStatsPreservesDenominatorAndUnknowns 防止 KPI 為了看起來乾淨，
// 把 identity conflict 算成沒回報，或把已退役的機器算進來。
func TestDashboardStatsPreservesDenominatorAndUnknowns(t *testing.T) {
	now := time.Now().UTC()
	retiredAt := now
	fresh := state.Facts{EverCheckedIn: true, LastCheckinReceived: now.Add(-time.Minute), CheckinInterval: state.CheckinInterval}
	ov := store.Overview{
		Now:      now,
		Expected: 5,
		Findings: []store.FleetFinding{{}, {}},
		Machines: []store.MachineRow{
			{Machine: store.Machine{Expected: true}, State: state.Online, Facts: fresh},
			{Machine: store.Machine{Expected: true}, State: state.Degraded, Facts: fresh},
			{Machine: store.Machine{Expected: true}, State: state.IdentityConflict, Facts: fresh},
			{Machine: store.Machine{Expected: true}, State: state.Unreachable},
			{Machine: store.Machine{Expected: false}, State: state.Online, Facts: fresh},
			{Machine: store.Machine{Expected: true, RetiredAt: &retiredAt}, State: state.Unreachable},
		},
	}
	deployments := []store.DeploymentView{
		{Deployment: store.Deployment{State: store.DeploymentRunning}},
		{Deployment: store.Deployment{State: store.DeploymentPaused}},
		{Deployment: store.Deployment{State: store.DeploymentFinished}},
	}
	got := buildDashboardStats(ov, deployments, true)
	if got.Expected != 5 || got.Reporting != 4 || got.Attention != 3 || got.Findings != 2 {
		t.Fatalf("machine KPI = %+v，want expected=5 reporting=4 attention=3 findings=2", got)
	}
	if got.ActiveDeployments != 2 || got.StuckDeployments != 1 || !got.DeploymentsKnown {
		t.Fatalf("deployment KPI = %+v，want active=2 stuck=1 known=true", got)
	}
	first := headline(ov)[0]
	if !strings.Contains(first, "名冊上 5 台，4 台正在回報。") {
		t.Fatalf("headline 跟 KPI 使用不同分母：%q", first)
	}
}

func TestStaleIdentityConflictDoesNotClaimCurrentReporting(t *testing.T) {
	now := time.Now().UTC()
	ov := store.Overview{
		Now: now, Expected: 1,
		Machines: []store.MachineRow{{
			Machine: store.Machine{DisplayName: "stale-conflict", Expected: true},
			State:   state.IdentityConflict,
			Facts: state.Facts{EverCheckedIn: true, LastCheckinReceived: now.Add(-time.Hour),
				CheckinInterval: state.CheckinInterval},
		}},
	}
	if got := buildDashboardStats(ov, nil, true).Reporting; got != 0 {
		t.Fatalf("stale durable conflict counted as current reporting: %d", got)
	}
	if rows := buildDashboardMachineTable(ov, "reporting").Rows; len(rows) != 0 {
		t.Fatalf("reporting filter contains stale conflict: %+v", rows)
	}
	if first := headline(ov)[0]; !strings.Contains(first, "名冊上 1 台，0 台正在回報。") {
		t.Fatalf("headline revived stale conflict heartbeat: %q", first)
	}
}

func TestHeartbeatStripUsesRealPredecessorAtDisplayBoundary(t *testing.T) {
	base := time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC)
	pts := make([]operator.MachineCheckin, 91)
	pts[0].ReceivedAt = base
	pts[1].ReceivedAt = base.Add(10 * time.Minute)
	for i := 2; i < len(pts); i++ {
		pts[i].ReceivedAt = pts[i-1].ReceivedAt.Add(state.CheckinInterval)
	}
	blocks := machineDetailStrip(pts, state.CheckinInterval)
	if len(blocks) != 90 {
		t.Fatalf("strip len=%d, want 90", len(blocks))
	}
	if blocks[0].Class != "red" || blocks[0].Status != "嚴重遲到" || !strings.Contains(blocks[0].Title, "10 分鐘") {
		t.Fatalf("first displayed block forgot hidden predecessor: %+v", blocks[0])
	}
}

func TestHeartbeatStripDoesNotInventPredecessor(t *testing.T) {
	blocks := machineDetailStrip([]operator.MachineCheckin{{ReceivedAt: time.Now().UTC()}}, state.CheckinInterval)
	if len(blocks) != 1 || blocks[0].Class != "grey" || blocks[0].Status != "顯示範圍起點，前一筆未知" {
		t.Fatalf("first-ever heartbeat was invented as on-time: %+v", blocks)
	}
}

func TestTheHeartbeatStripLegendCoversEveryTierItCanRender(t *testing.T) {
	interval := state.CheckinInterval
	base := time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC)
	actual := make(map[string]string)
	for _, scenario := range []struct {
		points []operator.MachineCheckin
		index  int
	}{
		{points: []operator.MachineCheckin{{ReceivedAt: base}}, index: 0},
		{points: []operator.MachineCheckin{{ReceivedAt: base}, {ReceivedAt: base.Add(interval)}}, index: 1},
		{points: []operator.MachineCheckin{{ReceivedAt: base}, {ReceivedAt: base.Add(interval*3/2 + time.Second)}}, index: 1},
		{points: []operator.MachineCheckin{{ReceivedAt: base}, {ReceivedAt: base.Add(3*interval + time.Second)}}, index: 1},
	} {
		blocks := machineDetailStrip(scenario.points, interval)
		actual[blocks[scenario.index].Class] = blocks[scenario.index].Status
	}

	legend := make(map[string]string)
	for _, tier := range stripTiers() {
		legend[tier.Class] = tier.Meaning
	}
	for class, status := range actual {
		meaning, ok := legend[class]
		if !ok {
			t.Errorf("got rendered tier (%q, %q), expected its class in stripTiers(); 狀態條會畫出一個圖例沒有解釋的顏色；操作員在交通燈的脈絡裡會把它讀成「這台沒有心跳」，去追一段根本不存在的中斷", class, status)
			continue
		}
		if meaning != status {
			t.Errorf("got rendered meaning %q for class %q, expected %q; 同一個顏色在圖例與逐筆清單上講不同的話，操作員不知道該信哪一個", status, class, meaning)
		}
	}
	for class, meaning := range legend {
		_, ok := actual[class]
		if !ok {
			t.Errorf("got legend tier (%q, %q), expected the class in rendered tiers; 圖例解釋了一個狀態條產不出來的顏色，操作員會在條上找一個永遠不會出現的格子", class, meaning)
		}
	}
}

func TestTheHeartbeatStripLegendSaysTheseExactWordsAndNoColourNames(t *testing.T) {
	s, st := newServer(t)
	id := onlineMachine(t, st, "samplehub1")
	body := get(t, s, "/machines/"+id)

	for _, tier := range stripTiers() {
		if !strings.Contains(body, tier.Meaning) {
			t.Errorf("got contains(%q) = false, expected true; 圖例少了一階，操作員看到那個顏色時沒有任何地方告訴他那是什麼意思", tier.Meaning)
		}
	}
	for _, colourName := range []string{"綠色", "黃色", "橘色", "紅色", "灰色"} {
		if strings.Contains(body, colourName) {
			t.Errorf("got contains(%q) = true, expected false; 圖例又用文字講顏色名，而顏色名會跟 CSS 漂掉——「黃色」對上 #ca5010 這個橘色就是這樣來的", colourName)
		}
	}
}

// TestHeadlineIncludesEveryActiveIdentityConflict 守住點名和計數使用同一個
// lifecycle scope；legacy expected 欄位不能再把 active row 排除在外。
func TestHeadlineIncludesEveryActiveIdentityConflict(t *testing.T) {
	ov := store.Overview{
		Expected: 2,
		Machines: []store.MachineRow{
			{Machine: store.Machine{DisplayName: "alpha", Expected: false}, State: state.IdentityConflict},
			{Machine: store.Machine{DisplayName: "zulu", Expected: true}, State: state.IdentityConflict},
		},
	}

	got := strings.Join(headline(ov), "\n")
	if !strings.Contains(got, "其中 2 台的身分對不上") {
		t.Fatalf("active identity conflicts 沒有使用 lifecycle 分母：%q", got)
	}
}

// ---------------------------------------------------------------- 用字

// TestNeverClaimsHealthyOrSuccessful 守的是地基二。
//
// 一台「有在回報、最近有跑完」的機器，畫面上不准出現任何一個字宣稱它
// 健康、正常、或成功。我們知道的只有「它有在講話」跟「它的任務有結束」，
// 而實測 400 筆 status='ok' 裡有 257 筆的摘要在描述失敗。
// 同樣的規則在 internal/state 有一份 —— 那裡守判決，這裡守文案，
// 因為最後被人讀到的是文案。
func TestNeverClaimsHealthyOrSuccessful(t *testing.T) {
	s, st := newServer(t)
	id := onlineMachine(t, st, "samplehub1")

	banned := []string{"健康", "一切正常", "正常運作", "運作正常", "成功", "沒問題", "healthy", "Healthy", "success"}
	for _, path := range []string{"/", "/machines/" + id} {
		body := get(t, s, path)
		for _, w := range banned {
			if strings.Contains(body, w) {
				t.Errorf("%s 出現了不該出現的字「%s」—— L1 只證明它有在動，不證明它做對了\n上下文：%s",
					path, w, around(body, w))
			}
		}
	}
}

// TestGreenLightSaysItRan 是上面那條的正面版：不只是「不准說健康」，
// 而是「必須說出它到底知道什麼」。
func TestGreenLightSaysItRan(t *testing.T) {
	s, st := newServer(t)
	onlineMachine(t, st, "samplehub1")
	body := get(t, s, "/")
	if !strings.Contains(body, "跑完") {
		t.Error("綠燈那一欄沒有說出「跑完」—— 它是這個綠燈唯一的意思")
	}
}

// TestFirstScreenDoesNotContradictItself 守的是「同一個畫面不准自打嘴巴」。
//
// 這條是看著真機的畫面補的：第一屏寫「沒有需要處理的事」，同一頁下面卻列著
// 兩條 advisory 發現。人會相信上面那句，因為它比較大 —— 於是 unknown 就被
// 洗成綠燈了。advisory 的意思是「不影響狀態」，不是「不存在」。
func TestFirstScreenDoesNotContradictItself(t *testing.T) {
	s, st := newServer(t)
	now := time.Now().UTC()
	id := onlineMachine(t, st, "samplehub1")

	// 一張讀不到過期時間的票 —— 判決層會給一條 advisory，狀態仍然是 Online。
	b := batch(now)
	b.Credentials = []model.Credential{{
		Provider: "openclaw", Status: model.CredUnknown,
		Note: "這個設定檔裡沒有任何過期欄位",
	}}
	if err := st.RecordObservation(id, b, now); err != nil {
		t.Fatalf("observation: %v", err)
	}

	body := get(t, s, "/")
	head := firstHeadline(body)

	if strings.Contains(head, "沒有需要處理的事") {
		t.Error("第一屏宣稱沒事，但下面列著發現 —— 這是在洗掉 unknown")
	}
	if !strings.Contains(head, "講不清楚") {
		t.Errorf("第一屏沒有把待確認的事情算進來，實際內容：%s", head)
	}
	if !strings.Contains(body, "這個設定檔裡沒有任何過期欄位") {
		t.Error("unknown 的原因沒有出現在畫面上 —— 沒有原因的 unknown 沒有人會去查")
	}
}

func TestAdvisoryFindingUsesSameSuffixOnDashboardAndMachinePage(t *testing.T) {
	s, st := newServer(t)
	now := time.Now().UTC()
	id := onlineMachine(t, st, "samplehub1")
	b := batch(now)
	b.Credentials = []model.Credential{{
		Provider: "openclaw", Status: model.CredUnknown, Note: "讀不到過期時間",
	}}
	if err := st.RecordObservation(id, b, now); err != nil {
		t.Fatalf("observation: %v", err)
	}

	machineFindings := sectionOf(t, get(t, s, "/machines/"+id), "<h2>發現</h2>", `<h2 id="monitor"`)
	if !strings.Contains(machineFindings, "（不影響狀態）") {
		t.Errorf("單機頁的 advisory finding 沒有標示不影響狀態：%s", machineFindings)
	}

	dashboardFindings := sectionOf(t, get(t, s, "/"), `<h2 id="findings"`, "<h2>機器註冊與名冊之外</h2>")
	if !strings.Contains(dashboardFindings, "（不影響狀態）") {
		t.Errorf("總覽的 advisory finding 沒有標示不影響狀態：%s", dashboardFindings)
	}
	if strings.Contains(dashboardFindings, "（資訊）") {
		t.Errorf("總覽的 advisory finding 不該標成資訊：%s", dashboardFindings)
	}
}

// TestMachinePageAlwaysCarriesTheL2Caveat：成果判定沒接通這件事要常駐，
// ⚠ 就算整台機器全綠也要在。全綠的時候正是人最容易誤讀的時候。
func TestMachinePageDoesNotExposeIncompleteFeatureCommentary(t *testing.T) {
	s, st := newServer(t)
	id := onlineMachine(t, st, "samplehub1")
	body := get(t, s, "/machines/"+id)

	for _, forbidden := range []string{"成果判定未接通", "不代表「做對了」", "請自己讀"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("單機頁顯示開發中說明 %q", forbidden)
		}
	}
}

// TestConflictedMachineCountsAsReporting
//
// ⚠ 這是實機演練改出來的。
//
// 原本身分衝突跟「失聯」「從未報到」歸在同一堆，於是第一屏寫
// 「名冊上 3 台，0 台正在回報」，而同一頁下面的表格裡那台衝突的機器
// 寫著「36 秒前」。它明明在講話。
//
// 衝突的意思不是「它沒回報」，而是「它有回報，但你不知道那些回報來自
// 哪一台實體機器」。第一屏跟它下面的表格自相矛盾，人就會停止相信第一屏 ——
// 而第一屏是這個產品唯一的賣點。
func TestConflictedMachineCountsAsReporting(t *testing.T) {
	s, st := newServer(t)
	id := onlineMachine(t, st, "samplehub1")
	conflictObservation(t, st, id)

	body := stripTags(get(t, s, "/"))

	if strings.Contains(body, "0 台正在回報") {
		t.Error("有回報但身分衝突的機器被算成「沒回報」—— 表格裡明明寫著它幾秒前才講過話")
	}
	if !strings.Contains(body, "1 台正在回報") {
		t.Errorf("第一句的台數不對，實際輸出：\n%s", firstLines(body, 6))
	}
	if !strings.Contains(body, "身分對不上") {
		t.Error("第一屏沒有講出身分衝突 —— 那是最嚴重的狀態，不能只出現在表格裡")
	}
	if !strings.Contains(body, "不知道那些回報來自哪一台") {
		t.Error("只說「衝突」不夠。要講出它的後果：你分不出是哪一台在回報")
	}
	// 只有一台衝突時要點名，不要逼人自己去表格裡找。
	if !strings.Contains(body, "samplehub1") {
		t.Error("只有一台衝突卻沒點名")
	}
}

func firstLines(s string, n int) string {
	ls := strings.Split(s, "\n")
	var out []string
	for _, l := range ls {
		if strings.TrimSpace(l) == "" {
			continue
		}
		out = append(out, l)
		if len(out) >= n {
			break
		}
	}
	return strings.Join(out, "\n")
}

// TestNoMarkdownLeaksIntoHTML
//
// ⚠ 樣板是 HTML，不是 markdown。寫 `**粗體**` 不會變粗，會原封不動地
// 印出兩個星號 —— 這是我在真機畫面上看到才發現的。
//
// 這種錯特別容易犯，因為這個專案的文案本來就寫得像散文，而作者的手指
// 記得 markdown。它不會讓任何測試變紅，只會讓畫面看起來很業餘 ——
// 而畫面的可信度是這個產品唯一的賣點。
func TestNoMarkdownLeaksIntoHTML(t *testing.T) {
	s, st := newServer(t)
	id := onlineMachine(t, st, "samplehub1")

	for _, path := range []string{"/", "/machines/" + id} {
		text := stripTags(get(t, s, path))
		for _, bad := range []string{"**", "`", "](", "<b>"} {
			if strings.Contains(text, bad) {
				i := strings.Index(text, bad)
				lo := i - 40
				if lo < 0 {
					lo = 0
				}
				hi := i + 40
				if hi > len(text) {
					hi = len(text)
				}
				t.Errorf("%s 的文字裡有沒被渲染的 markdown %q：…%s…", path, bad, text[lo:hi])
			}
		}
	}
}

// TestNoStraySpaceBetweenChineseChars 掃整頁輸出找「中文 空格 中文」。
//
// ⚠ 這個 bug 已經出現兩次了：一次在 state.go（「已經 21.1 小時 沒有任何任務」），
// 一次在 web.go（「再 3 分鐘 沒消息就判失聯」）。兩次都是同一個原因 ——
// humanDur 回傳的是「數字 空格 中文單位」，而外面的格式字串又在 %s 後面
// 補了一個空格。
//
// 所以這裡不逐條比對字串，而是掃渲染後的整頁：任何人以後在任何樣板或
// 任何格式字串裡犯同一個錯，都會被這一個測試接住。文案的可信度是這個
// 產品唯一的賣點，而排版上的小破綻會直接扣掉它。
func TestNoStraySpaceBetweenChineseChars(t *testing.T) {
	s, st := newServer(t)
	id := onlineMachine(t, st, "samplehub1")
	enroll(t, st, "sampleagent1", time.Now().UTC().Add(-72*time.Hour))

	for _, path := range []string{"/", "/machines/" + id} {
		body := get(t, s, path)
		// 先把標籤拔掉，否則 <td>中文</td> 之類的換行會被誤判。
		text := stripTags(body)
		for _, line := range strings.Split(text, "\n") {
			if bad := findCJKSpaceCJK(line); bad != "" {
				t.Errorf("%s 有中文中間夾半形空格：%q", path, bad)
			}
		}
	}
}

func stripTags(s string) string {
	var b strings.Builder
	depth := 0
	for _, r := range s {
		switch {
		case r == '<':
			depth++
		case r == '>':
			if depth > 0 {
				depth--
			}
			b.WriteRune('\n') // 標籤邊界當成換行，免得相鄰欄位被接在一起
		case depth == 0:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// findCJKSpaceCJK 回傳第一個「中文 空格 中文」的片段，沒有就回空字串。
// ⚠ 只認單一半形空格。「中文 123 中文」是對的（數字兩邊本來就該空），
// 連續空白多半是縮排造成的，不是文案問題。
func findCJKSpaceCJK(line string) string {
	rs := []rune(line)
	isHan := func(r rune) bool {
		return (r >= 0x4E00 && r <= 0x9FFF) || // 中日韓統一表意文字
			(r >= 0x3000 && r <= 0x303F) // 中文標點
	}
	for i := 1; i+1 < len(rs); i++ {
		if rs[i] == ' ' && isHan(rs[i-1]) && isHan(rs[i+1]) {
			lo, hi := i-6, i+7
			if lo < 0 {
				lo = 0
			}
			if hi > len(rs) {
				hi = len(rs)
			}
			return string(rs[lo:hi])
		}
	}
	return ""
}

// TestMachinePageSaysWhenItWouldBeJudgedUnreachable
//
// ⚠ 這是一次失聯演練直接換來的測試。
//
// 演練跑到一半我去看畫面：Hub 說 Degraded，而我知道那台的 agent 已經停了。
// 我把「還沒到門檻」讀成了「Hub 沒發現」，開始查資料庫、懷疑時區 ——
// 查了幾分鐘才發現只是還沒到 210 秒。
//
// 一個有寬限期的判定，在寬限期內看起來跟壞掉一模一樣。畫面必須把門檻的
// 時間點寫出來，否則每一個在寬限期內看它的人都會犯同一個錯 ——
// 而在這個產品裡，「以為它壞了」跟「以為它沒壞」一樣糟。
func TestMachinePageSaysWhenItWouldBeJudgedUnreachable(t *testing.T) {
	s, st := newServer(t)
	id := onlineMachine(t, st, "samplehub1")
	body := get(t, s, "/machines/"+id)

	if !strings.Contains(body, "下一次應該到") {
		t.Error("單機頁沒有寫下一次心跳該什麼時候到")
	}
	if !strings.Contains(body, "幾點會判失聯") {
		t.Error("單機頁沒有寫幾點會被判定失聯 —— 讀畫面的人分不出「還沒到」跟「不會到」")
	}
	if !strings.Contains(body, "沒消息就判失聯") {
		t.Error("少了倒數的說明；只給一個時間點還是要人自己心算")
	}
}

func TestMachinePageRendersTypedMonitorIdentityAndResources(t *testing.T) {
	s, st := newServer(t)
	id := onlineMachine(t, st, "samplehub1")
	body := get(t, s, "/machines/"+id)

	for _, marker := range []string{
		"最新 agent 版本", "0.1.0", "最新 agent sequence / uptime", "seq=1 · uptime=86400s",
		"最新 boot identity", "boot-1", "不同 agent 啟動身分", "未知",
		"samplehub1", "100.64.0.1", "剩 50.0 GiB / 共 100.0 GiB",
	} {
		if !strings.Contains(body, marker) {
			t.Errorf("typed machine detail missing %q", marker)
		}
	}
	if strings.Contains(body, "一小時內 agent 重啟") {
		t.Fatal("typed page still mislabels distinct agent starts as restart count")
	}
}

func TestMachinePageDoesNotClaimLingerIsOffWhenItWasNeverMeasured(t *testing.T) {
	s, st := newServer(t)
	now := time.Now().UTC()
	id := enroll(t, st, "mac-without-linger", now.Add(-time.Hour))
	b := batch(now.Add(-time.Minute))
	b.Identity.OS = "darwin"
	b.Identity.LingerEnabled = false
	b.Identity.LingerMeasured = false
	if err := st.RecordObservation(id, b, now.Add(-time.Minute)); err != nil {
		t.Fatalf("寫入未量到 linger 的 identity 觀測失敗：%v", err)
	}
	body := get(t, s, "/machines/"+id)
	if !strings.Contains(body, "<tr><th>linger</th><td>\n      <span class=\"dim\">未知</span>") {
		t.Errorf("未量到 linger 的裝置頁沒有在 linger 欄顯示『未知』；畫面片段對不上：%s", tail(body, 1200))
	}
	for _, falseClaim := range []string{"未開啟", "登出時 agent 會停止"} {
		if strings.Contains(body, falseClaim) {
			t.Errorf("未量到 linger 的裝置頁不該出現 %q；這會把 macOS 的未知說成可修的 Linux 設定問題", falseClaim)
		}
	}
}

func TestTheCheckinWindowTextSaysTheseExactWords(t *testing.T) {
	tests := []struct {
		name     string
		input    time.Duration
		expected string
	}{
		{name: "24 hours", input: 24 * time.Hour, expected: "24 小時"},
		{name: "one hour", input: time.Hour, expected: "1 小時"},
		{name: "fractional hours", input: 90 * time.Minute, expected: "1.5 小時"},
		{name: "two days", input: 48 * time.Hour, expected: "2 天"},
	}
	const consequence = "讀取窗口在頁面上被講成一個跟常數對不起來的長度，操作員會以為自己看的是別的時間範圍"
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := disclosureWindowText(tc.input)
			if got != tc.expected {
				t.Errorf("disclosureWindowText(%s) = %q，預期 %q —— %s", tc.input, got, tc.expected, consequence)
			}
		})
	}
}

func TestTheTruncatedCheckinNoticeDoesNotOverstateWhatThePageDraws(t *testing.T) {
	s, st := newServer(t)
	now := time.Now().UTC().Truncate(time.Second)
	id := enroll(t, st, "bounded-checkins", now.Add(-time.Hour))
	oldest := now.Add(-time.Duration(store.DetailCheckinLimit) * time.Second)
	for i := 0; i <= store.DetailCheckinLimit; i++ {
		at := oldest.Add(time.Duration(i) * time.Second)
		if err := st.RecordCheckin(id, model.Checkin{
			SchemaVersion: model.SchemaVersion, SentAt: at, AgentVersion: "v3",
			BootID: "boot-one", AgentSeq: int64(i + 1), AgentStartedAt: now.Add(-time.Hour),
			UptimeSeconds: func() *int64 { v := int64(100 + i); return &v }(), DiskFreeBytes: 50, DiskTotalBytes: 100,
		}, at); err != nil {
			t.Fatal(err)
		}
	}

	body := get(t, s, "/machines/"+id)
	wantWindow := disclosureWindowText(store.DetailCheckinWindow)
	if !strings.Contains(body, wantWindow) {
		t.Errorf("機器詳情頁沒有出現 %q —— 頁面沒有講出讀取窗口，操作員不知道這些心跳是多長一段時間裡的", wantWindow)
	}
	oldClaim := "顯示 24 小時內最近"
	if strings.Contains(body, oldClaim) {
		t.Errorf("機器詳情頁仍然出現 %q —— 頁面仍然用「顯示」宣稱那 1000 筆，但它畫得出來的只有 90 筆，操作員會把狀態條左端當成 24 小時前、把過密的心跳算成偏稀", oldClaim)
	}
	wantStrip := "狀態條畫出其中最近"
	if !strings.Contains(body, wantStrip) {
		t.Errorf("機器詳情頁沒有出現 %q —— 截斷提示沒有說清楚狀態條只畫得出其中一部分，操作員會以為那 1000 筆都在眼前", wantStrip)
	}
}

func TestTheInvalidCheckinNoticeSaysWhichSetItCounted(t *testing.T) {
	s, st := newServer(t)
	now := time.Now().UTC().Truncate(time.Second)
	id := enroll(t, st, "invalid-checkin", now.Add(-time.Hour))
	if err := st.RecordCheckin(id, model.Checkin{
		SchemaVersion: model.SchemaVersion, AgentVersion: "v3",
		BootID: "boot-one", AgentSeq: 1, AgentStartedAt: now.Add(-time.Hour),
		UptimeSeconds: func() *int64 { v := int64(100); return &v }(), DiskFreeBytes: 50, DiskTotalBytes: 100,
	}, now); err != nil {
		t.Fatal(err)
	}

	body := get(t, s, "/machines/"+id)
	want := "已取回的心跳裡有 1 筆無效，沒有畫進狀態條。"
	if !strings.Contains(body, want) {
		t.Errorf("機器詳情頁沒有出現 %q —— 無效心跳的提示沒有講清楚範圍，那幾筆可能一筆都不在下面的逐筆清單裡，操作員會去找一個找不到的東西", want)
	}
}

// 從來沒報到過的機器沒有「下一次」。硬算會得到一個從零值時間推出來的日期，
// 而畫面上出現 1970 會讓人以為是系統壞了 —— 那台機器真正的問題是
// 「從未報到」，不該被一個假時間蓋掉。
func TestNeverReportedMachineHasNoNextCheckin(t *testing.T) {
	s, st := newServer(t)
	id := enroll(t, st, "sampleagent1", time.Now().UTC().Add(-72*time.Hour))
	body := get(t, s, "/machines/"+id)

	if strings.Contains(body, "幾點會判失聯") {
		t.Error("從未報到的機器不該有「幾點會判失聯」—— 它沒有下一次可言")
	}
	if strings.Contains(body, "1970-") {
		t.Error("畫面上出現 1970 —— 對一台沒有心跳的機器算了「下一次」")
	}
}

// TestRunSummariesAppearVerbatim：摘要原文必須原封不動地出現。
// ⚠ 這裡故意放一段 status='ok' 但內容在講失敗的摘要 —— 那是實測的常態，
// 也是這一區存在的唯一理由：讓人自己讀出程式讀不出來的東西。
func TestRunSummariesAppearVerbatim(t *testing.T) {
	s, st := newServer(t)
	id := onlineMachine(t, st, "samplehub1")
	body := get(t, s, "/machines/"+id)

	if !strings.Contains(body, "我沒辦法連上 API，所以這次沒有做任何事。") {
		t.Error("摘要原文沒有出現在畫面上")
	}
	if !strings.Contains(body, "measured_at（agent）") || !strings.Contains(body, "received_at（Hub）") {
		t.Error("摘要沒有顯示 agent/Hub 時鐘")
	}
}

// Machine evidence 必須只從 typed disclosure 進頁面：block text 保留換行，
// 控制字元被替換，截斷後不能再標成「原文」，journal 的三種狀態仍分得開。
func TestMachinePageRendersTypedFreeTextEvidenceWithoutOverclaiming(t *testing.T) {
	s, st := newServer(t)
	now := time.Now().UTC()
	id := enroll(t, st, "typed-evidence", now.Add(-time.Hour))
	b := batch(now.Add(-time.Minute))
	b.Systemd = []model.Unit{
		{Name: "failed.service", Present: true},
		{Name: "heard.service", Present: true, ActiveState: "active", SubState: "running",
			NRestarts: 7, MainPID: 987654321},
		{Name: "missing.service", Present: true},
		{Name: "quiet.service", Present: true},
	}
	b.Journals = []model.UnitJournal{
		{Unit: "failed.service", WindowSec: 3600, Err: "permission denied\nretry\x1b[31m"},
		{Unit: "heard.service", WindowSec: 3600, Lines: 2, Shapes: 1,
			Top: []model.JournalShape{{Count: 2, Example: "first line\nsecond line\x1b[31m"}}},
		{Unit: "quiet.service", WindowSec: 3600},
	}
	rawSummary := "summary line one\nsummary line two\x1b[31m" +
		strings.Repeat("x", operator.MachineEvidenceMaxFieldBytes)
	b.OpenClaw.DB.RecentSummaries = []model.RunSummary{{
		JobID: "typed-job", At: now.Add(-time.Hour), Status: "ok", Summary: rawSummary,
	}}
	if err := st.RecordObservation(id, b, now.Add(-30*time.Second)); err != nil {
		t.Fatal(err)
	}

	body := get(t, s, "/machines/"+id)
	for _, want := range []string{
		"Journal 讀取失敗", "最近一小時 0 行", "最近一小時說了", "本輪未收集 journal", "missing.service",
		"已截斷至 16 KiB", "含已替換的控制／非法字元", "active/running", "7",
		"measured_at（agent）", "received_at（Hub）",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("typed machine evidence page missing %q", want)
		}
	}
	if !strings.Contains(body, `<pre class="copy">summary line one
summary line two�[31m`) || !strings.Contains(body, `<pre class="copy">first line
second line�[31m</pre>`) {
		t.Error("summary/example block text did not preserve LF inside copy blocks after scrubbing")
	}
	if strings.ContainsRune(body, '\x1b') {
		t.Fatal("machine evidence page contains a raw terminal escape byte")
	}
	if strings.Contains(body, "987654321") {
		t.Fatal("machine evidence page exposed volatile MainPID")
	}
	if strings.Contains(body, "摘要（原文）") {
		t.Fatal("a truncated or scrubbed summary was labelled 原文")
	}
}

// TestMachinePageSaysWhichEvidenceListsWereCutShort 守住裝置頁三份清單各自的截斷訊息。
// 三份清單共用同一個 evidence 結構，接錯線或漏講都不會有任何人紅；實測把
// 條件改看 Journals.Truncated、整行刪掉、條件反向、總數改印頁長，四種改法全綠。
// 此 fixture 刻意讓 systemd 與「未收集 journal 的 unit」都被截斷且總數不同，
// journal 自己則沒被截斷，因為只有三者狀態不同才看得出是否各自講自己的狀態。
// 未收集 journal 的 unit 本身就是 Hub 沒在觀測的東西；若把截斷的漏網名單當成
// 完整名單交出去，操作員會以為自己已經看完了。
// 「沒收到 journal 的 unit」是 systemd unit 的子集，所以那一份被截斷時，
// systemd 那一份必然也被截斷；只有一個場景的話，把 systemd 那一行的條件接到
// 另一份清單的旗標上是量不出來的。第二個場景讓每個 unit 都收到 journal，
// 使兩個旗標第一次分開。
// 第三個場景讓三份清單的總數互不相同（150／101／49），這是唯一能量出
// 「某一行印了別份清單的總數」的狀態；而且它的「未收集 journal 的 unit」
// 清單非空卻沒被截斷，補上了「非空但完整的清單不准說自己被截斷」這一半。
func TestMachinePageSaysWhichEvidenceListsWereCutShort(t *testing.T) {
	s, st := newServer(t)
	now := time.Now().UTC()
	id := enroll(t, st, "truncated-evidence-lists", now.Add(-time.Hour))
	b := batch(now.Add(-time.Minute))
	b.Systemd = make([]model.Unit, 0, 102)
	for i := 0; i < 102; i++ {
		b.Systemd = append(b.Systemd, model.Unit{
			Name: fmt.Sprintf("u%03d.service", i), Present: true,
		})
	}
	b.Journals = []model.UnitJournal{{Unit: "u000.service", WindowSec: 3600}}
	if err := st.RecordObservation(id, b, now.Add(-30*time.Second)); err != nil {
		t.Fatal(err)
	}

	body := get(t, s, "/machines/"+id)
	systemdMessage := "systemd unit 超過顯示上限；共 102 個可解析 unit。"
	missingJournalMessage := "未收集 journal 的 unit 超過顯示上限；共 101 個。"
	journalMessage := "journal evidence 超過顯示上限"
	if !strings.Contains(body, systemdMessage) {
		t.Errorf("systemd unit 清單被截斷了，畫面卻沒說 %q；body 長度為 %d。",
			systemdMessage, len(body))
	}
	if !strings.Contains(body, missingJournalMessage) {
		t.Errorf("沒在觀測的 unit 清單被截掉了卻沒說 %q，操作員會以為畫面上列出來的就是全部；body 長度為 %d。",
			missingJournalMessage, len(body))
	}
	if strings.Contains(body, journalMessage) {
		t.Errorf("journal 那一份根本沒被截斷，畫面卻說它被截了（找到 %q）；body 長度為 %d。",
			journalMessage, len(body))
	}

	allHeardID := enroll(t, st, "cut-short-all-heard", now.Add(-time.Hour))
	allHeardBatch := batch(now.Add(-time.Minute))
	allHeardBatch.Systemd = make([]model.Unit, 0, 102)
	allHeardBatch.Journals = make([]model.UnitJournal, 0, 102)
	for i := 0; i < 102; i++ {
		unitName := fmt.Sprintf("all-heard-%03d.service", i)
		allHeardBatch.Systemd = append(allHeardBatch.Systemd, model.Unit{
			Name: unitName, Present: true,
		})
		allHeardBatch.Journals = append(allHeardBatch.Journals, model.UnitJournal{
			Unit: unitName, WindowSec: 3600,
		})
	}
	if err := st.RecordObservation(allHeardID, allHeardBatch, now.Add(-30*time.Second)); err != nil {
		t.Fatal(err)
	}

	allHeardBody := get(t, s, "/machines/"+allHeardID)
	allHeardSystemdMessage := "systemd unit 超過顯示上限；共 102 個可解析 unit。"
	allHeardJournalMessage := "journal evidence 超過顯示上限；共 102 個可解析 unit。"
	allHeardMissingJournalMessage := "未收集 journal 的 unit 超過顯示上限"
	if !strings.Contains(allHeardBody, allHeardSystemdMessage) {
		t.Errorf("每個 unit 都收到 journal、另一份清單為空時，systemd 清單被截斷了，畫面卻沒說 %q；body 長度為 %d。",
			allHeardSystemdMessage, len(allHeardBody))
	}
	if !strings.Contains(allHeardBody, allHeardJournalMessage) {
		t.Errorf("journal evidence 清單被截斷了，畫面卻沒說 %q；body 長度為 %d。",
			allHeardJournalMessage, len(allHeardBody))
	}
	if strings.Contains(allHeardBody, allHeardMissingJournalMessage) {
		t.Errorf("一個 unit 都沒漏，畫面卻說那份清單被截斷了（找到 %q）；body 長度為 %d。",
			allHeardMissingJournalMessage, len(allHeardBody))
	}

	mixedID := enroll(t, st, "cut-short-mixed", now.Add(-time.Hour))
	mixedBatch := batch(now.Add(-time.Minute))
	mixedBatch.Systemd = make([]model.Unit, 0, 150)
	mixedBatch.Journals = make([]model.UnitJournal, 0, 101)
	for i := 0; i < 150; i++ {
		unitName := fmt.Sprintf("mixed-%03d.service", i)
		mixedBatch.Systemd = append(mixedBatch.Systemd, model.Unit{
			Name: unitName, Present: true,
		})
		if i < 101 {
			mixedBatch.Journals = append(mixedBatch.Journals, model.UnitJournal{
				Unit: unitName, WindowSec: 3600,
			})
		}
	}
	if err := st.RecordObservation(mixedID, mixedBatch, now.Add(-30*time.Second)); err != nil {
		t.Fatal(err)
	}

	mixedBody := get(t, s, "/machines/"+mixedID)
	mixedSystemdMessage := "systemd unit 超過顯示上限；共 150 個可解析 unit。"
	mixedJournalMessage := "journal evidence 超過顯示上限；共 101 個可解析 unit。"
	mixedMissingJournalMessage := "未收集 journal 的 unit 超過顯示上限"
	if !strings.Contains(mixedBody, mixedSystemdMessage) {
		t.Errorf("systemd unit 清單被截斷了，畫面卻沒說 %q；body 長度為 %d。",
			mixedSystemdMessage, len(mixedBody))
	}
	if !strings.Contains(mixedBody, mixedJournalMessage) {
		t.Errorf("journal evidence 的總數印成了別份清單的數字，操作員會以為漏掉的是另一批；畫面沒說 %q；body 長度為 %d。",
			mixedJournalMessage, len(mixedBody))
	}
	if strings.Contains(mixedBody, mixedMissingJournalMessage) {
		t.Errorf("那 49 個 unit 一個都沒被截掉，畫面卻說清單不完整（找到 %q）；body 長度為 %d。",
			mixedMissingJournalMessage, len(mixedBody))
	}
}

// TestMachinePageAdmitsEveryOversizeEvidenceList 守住截斷提示。
// 裝置頁整組「這份清單不完整」的提示，實測整行刪掉也不會有人紅；
// 一份被截斷卻宣稱完整的清單，
// 比沒有這份清單更糟。這支用同一次觀測同時撐爆四份共用同一上限的清單，
// 一次量完比四支各自建立 fixture 便宜，也逼出四則提示各自對應自己那一份。
// :289／:294 的 task／cron 狀態提示文案都只是「（已截斷）」，這幾個字在整頁
// 出現很多次，字串比對分不出是哪一則，所以不在這支的範圍內。
// 第二個場景只撐爆 CLI 工具那一份，其餘三份都完整；
// 這是唯一能量出「某一則提示讀了別份清單的旗標」的狀態，
// 同時補上「完整的清單不准說自己被截斷」這一半。
func TestMachinePageAdmitsEveryOversizeEvidenceList(t *testing.T) {
	s, st := newServer(t)
	now := time.Now().UTC()
	id := enroll(t, st, "all-oversize-evidence-lists", now.Add(-time.Hour))
	b := batch(now.Add(-time.Minute))
	b.CLITools = make([]model.CLITool, 0, 101)
	b.Credentials = make([]model.Credential, 0, 101)
	b.OpenClaw.DB.Occupancy = make([]model.OccupancyEvidence, 0, 101)
	b.OpenClaw.DB.RecentSummaries = make([]model.RunSummary, 0, 101)
	b.OpenClaw.DB.OccupancyRowsSeen = 101
	for i := 0; i < 101; i++ {
		b.CLITools = append(b.CLITools, model.CLITool{
			Name: fmt.Sprintf("tool-%03d", i),
		})
		b.Credentials = append(b.Credentials, model.Credential{
			Provider: fmt.Sprintf("provider-%03d", i), Status: model.CredConfigured,
		})
		b.OpenClaw.DB.Occupancy = append(b.OpenClaw.DB.Occupancy, model.OccupancyEvidence{
			Source: "cron_run_logs", JobID: fmt.Sprintf("occupancy-job-%03d", i),
			At: now.Add(-time.Duration(i) * time.Second), Provider: fmt.Sprintf("occupancy-provider-%03d", i),
		})
		b.OpenClaw.DB.RecentSummaries = append(b.OpenClaw.DB.RecentSummaries, model.RunSummary{
			JobID: fmt.Sprintf("summary-job-%03d", i), At: now.Add(-time.Duration(i) * time.Second),
			Status: "ok", Summary: fmt.Sprintf("summary-%03d", i),
		})
	}
	if err := st.RecordObservation(id, b, now.Add(-30*time.Second)); err != nil {
		t.Fatal(err)
	}

	body := get(t, s, "/machines/"+id)
	runSummariesMessage := "摘要超過顯示上限；這裡保留 OpenClaw 的原始順序，只顯示前 100 筆。"
	occupancyMessage := "顯示前 100 組 provider/source。"
	credentialsMessage := "憑證超過顯示上限，只顯示前 100 家。"
	cliToolsMessage := "工具超過顯示上限，只顯示前 100 個。"
	if !strings.Contains(body, runSummariesMessage) {
		t.Errorf("OpenClaw 摘要清單被截斷了卻沒承認，操作員會以為自己看到的是全部；body 長度為 %d，沒找到 %q。",
			len(body), runSummariesMessage)
	}
	if !strings.Contains(body, occupancyMessage) {
		t.Errorf("provider/source 占用清單被截斷了卻沒承認，操作員會以為自己看到的是全部；body 長度為 %d，沒找到 %q。",
			len(body), occupancyMessage)
	}
	if !strings.Contains(body, credentialsMessage) {
		t.Errorf("憑證清單被截斷了卻沒承認，操作員會以為自己看到的是全部；body 長度為 %d，沒找到 %q。",
			len(body), credentialsMessage)
	}
	if !strings.Contains(body, cliToolsMessage) {
		t.Errorf("CLI 工具清單被截斷了卻沒承認，操作員會以為自己看到的是全部；body 長度為 %d，沒找到 %q。",
			len(body), cliToolsMessage)
	}

	oneID := enroll(t, st, "one-oversize-evidence-list", now.Add(-time.Hour))
	oneBatch := batch(now.Add(-time.Minute))
	oneBatch.CLITools = make([]model.CLITool, 0, 101)
	for i := 0; i < 101; i++ {
		oneBatch.CLITools = append(oneBatch.CLITools, model.CLITool{
			Name: fmt.Sprintf("one-oversize-tool-%03d", i),
		})
	}
	oneBatch.Credentials = []model.Credential{{
		Provider: "one-provider", Status: model.CredConfigured,
	}}
	oneBatch.OpenClaw.DB.Occupancy = []model.OccupancyEvidence{{
		Source: "cron_run_logs", JobID: "one-occupancy-job", At: now,
		Provider: "one-occupancy-provider",
	}}
	oneBatch.OpenClaw.DB.OccupancyRowsSeen = 1
	oneBatch.OpenClaw.DB.RecentSummaries = []model.RunSummary{{
		JobID: "one-summary-job", At: now, Status: "ok", Summary: "one-summary",
	}}
	if err := st.RecordObservation(oneID, oneBatch, now.Add(-30*time.Second)); err != nil {
		t.Fatal(err)
	}

	oneBody := get(t, s, "/machines/"+oneID)
	if !strings.Contains(oneBody, cliToolsMessage) {
		t.Errorf("只有 CLI 工具清單被截斷，畫面卻沒說 %q；body 長度為 %d。",
			cliToolsMessage, len(oneBody))
	}
	if strings.Contains(oneBody, credentialsMessage) {
		t.Errorf("憑證清單只有一筆、根本沒被截斷，畫面卻說它不完整，操作員會去找不存在的東西；body 長度為 %d，找到 %q。",
			len(oneBody), credentialsMessage)
	}
	if strings.Contains(oneBody, occupancyMessage) {
		t.Errorf("provider/source 占用清單只有一筆、根本沒被截斷，畫面卻說 %q；body 長度為 %d。",
			occupancyMessage, len(oneBody))
	}
	if strings.Contains(oneBody, runSummariesMessage) {
		t.Errorf("OpenClaw 摘要清單只有一筆、根本沒被截斷，畫面卻說 %q；body 長度為 %d。",
			runSummariesMessage, len(oneBody))
	}
}

// ---------------------------------------------------------------- 矛盾

// TestVersionConflictShowsEveryNumber：來源互相矛盾時，畫面要把矛盾攤開，
// ⚠ 不准挑一個當答案。實測 grok 自報 1.0.3、npm 說 1.0.13、
// version.json 說 0.2.118 —— 挑任何一個都是在騙人。
func TestVersionConflictShowsEveryNumber(t *testing.T) {
	s, st := newServer(t)
	now := time.Now().UTC()
	id := onlineMachine(t, st, "samplehub1")

	b := batch(now)
	b.CLITools = []model.CLITool{{
		Name: "grok", Present: true, Path: "/usr/bin/grok",
		VersionReported: "1.0.3", VersionPackageJSON: "1.0.13",
		SourcesDisagree: true,
	}}
	if err := st.RecordObservation(id, b, now); err != nil {
		t.Fatalf("observation: %v", err)
	}

	body := get(t, s, "/machines/"+id)
	for _, v := range []string{"1.0.3", "1.0.13"} {
		if !strings.Contains(body, v) {
			t.Errorf("矛盾的版本 %s 沒有顯示出來 —— 畫面挑了一個當答案", v)
		}
	}
	if !strings.Contains(body, "來源互相矛盾") {
		t.Error("沒有明講這是矛盾")
	}
}

// TestInstalledVersusRunningIsSpelledOut：裝的跟跑的不一樣要用人話講出來，
// 但不把兩條主機路徑或 PID 帶過 typed disclosure。
func TestInstalledVersusRunningIsSpelledOut(t *testing.T) {
	s, st := newServer(t)
	now := time.Now().UTC()
	id := onlineMachine(t, st, "samplehub1")

	b := batch(now)
	b.CLITools = []model.CLITool{{
		Name: "openclaw", Present: true, Path: "/usr/bin/openclaw",
		RealPath:   "/opt/openclaw-2.2.0/bin/openclaw",
		RunningExe: "/opt/openclaw-2.1.0/bin/openclaw", RunningPID: 4242,
		VersionReported: "2.2.0",
	}}
	if err := st.RecordObservation(id, b, now); err != nil {
		t.Fatalf("observation: %v", err)
	}

	body := get(t, s, "/machines/"+id)
	if !strings.Contains(body, "PATH 與執行中的安裝不同") {
		t.Error("沒有把安裝與執行中的檔案不一致講出來")
	}
	for _, private := range []string{"/opt/openclaw-2.2.0", "/opt/openclaw-2.1.0", "4242"} {
		if strings.Contains(body, private) {
			t.Errorf("typed machine page leaked host coordinate %q", private)
		}
	}
}

func TestMachinePageQualifiesCLIRunningReasonByProcessScan(t *testing.T) {
	s, st := newServer(t)
	now := time.Now().UTC()
	id := onlineMachine(t, st, "process-scan-reasons")
	b := batch(now)
	b.CLITools = []model.CLITool{
		{Name: "absent-tool", RunningReason: "工具沒有安裝，所以沒有可比對的 process"},
		{Name: "complete-tool", Present: true, ProcessScan: model.ProcessScanComplete,
			RunningReason: "掃描完成，沒有符合的 process"},
		{Name: "legacy-tool", Present: true, RunningReason: "舊 agent 沒有 process_scan"},
		{Name: "restricted-tool", Present: true, ProcessScan: model.ProcessScanRestricted,
			RunningReason: "只掃得到 3 個 process，這台的 /proc 視野被限制了"},
	}
	if err := st.RecordObservation(id, b, now); err != nil {
		t.Fatalf("observation: %v", err)
	}

	cli := sectionOf(t, get(t, s, "/machines/"+id), "<h2>CLI 工具</h2>", "<h2>它該做到什麼</h2>")
	row := func(name string) string {
		t.Helper()
		anchor := strings.Index(cli, "<td>"+name+"</td>")
		if anchor < 0 {
			t.Fatalf("CLI 工具表找不到 %q 那一列：%s", name, cli)
		}
		end := strings.Index(cli[anchor:], "</tr>")
		if end < 0 {
			t.Fatalf("CLI 工具 %q 那一列沒有結尾：%s", name, cli[anchor:])
		}
		return cli[anchor : anchor+end]
	}

	restricted := row("restricted-tool")
	if !strings.Contains(restricted, "這台的 process 偵測沒有跑到底：只掃得到 3 個 process，這台的 /proc 視野被限制了") ||
		strings.Contains(restricted, "沒有偵測到在跑的 process") {
		t.Fatalf("受限掃描列沒有使用 unscanned 文案：%s", restricted)
	}
	if absent := row("absent-tool"); !strings.Contains(absent, "沒有偵測到在跑的 process：工具沒有安裝，所以沒有可比對的 process") {
		t.Fatalf("未安裝工具列藏掉 running reason：%s", absent)
	}
	for _, name := range []string{"complete-tool", "legacy-tool"} {
		if got := row(name); !strings.Contains(got, "沒有偵測到在跑的 process：") ||
			strings.Contains(got, "這台的 process 偵測沒有跑到底") {
			t.Fatalf("%s 沒有維持 idle 文案：%s", name, got)
		}
	}
}

// ---------------------------------------------------------------- BAT

// TestConnectBATGivesAnAddressNotAnIframe：BAT 沒有 HTTP 端點、沒有交握認證，
// 憑證有效期到西元 4096 年 —— 嵌進來在技術上做不到。畫面只能給位址。
//
// ⚠⚠ 這個測試原本寫的是 `strings.Contains(body, "100.64.0.1")`，
// 而它在 Connect 那一段顯示「還不知道這台的 Tailscale 位址」的情況下**是綠的**。
//
// 原因：那個 IP 在頁面上出現兩次，一次在上面的身分表格、一次在 Connect。
// 測試抓到的是前者。也就是說這個叫做「Connect 有給位址」的測試，
// 從來沒有真的看過 Connect 那一段 —— 而那正好讓 `:8080` 這個
// 在四台實機上全錯的寫死 port 活了下來（見 internal/probe/bat.go）。
//
// 這是這個專案第七次遇到同一件事：**一個乾淨的綠燈，第一個要懷疑的是
// 有沒有問對地方。**（前六次見 docs/PHASE1.md §5.6–§5.11。）
// 現在斷言的是那個**可複製區塊本身**，而且連 port 都要對。
func TestConnectBATGivesAnAddressNotAnIframe(t *testing.T) {
	s, st := newServer(t)
	id := onlineMachine(t, st, "samplehub1")
	body := get(t, s, "/machines/"+id)

	if !strings.Contains(body, `<pre class="copy">https://100.64.0.1:9876</pre>`) {
		t.Errorf("Connect 那一段沒有給出量出來的位址，實際內容：\n%s", batPanel(body))
	}
	if strings.Contains(body, ":8080") {
		t.Error("畫面上又出現 8080 了 —— 那是寫死的猜測，四台實機沒有一台在那裡")
	}
	if strings.Contains(body, "<iframe") {
		t.Error("畫面上出現了 iframe —— BAT 嵌不進來，硬做出來的只會是個假的殼")
	}
}

// TestConnectRefusesToGuessWhenBATIsBoundToLocalhost
//
// ⚠⚠ 這是整組測試裡最重要的一個。
//
// 機隊五台裡有**兩台**（sampleagent2、sampleagent3）的 bat-server 是 --bind=localhost。
// 那兩台不管填哪個 IP 都連不上，因為它根本沒有綁在對外的介面上。
//
// 舊版畫面對它們給出 `https://<tailnet-ip>:8080`，看起來完全正常。
// 人要試了才知道連不上，而且連不上之後第一個懷疑的會是 Tailscale、
// 是防火牆、是那台機器掛了 —— 全都不是。
//
// 給不出位址的時候要說「給不出來，因為 X」，不是給一個可能會通的。
func TestConnectRefusesToGuessWhenBATIsBoundToLocalhost(t *testing.T) {
	s, st := newServer(t)
	now := time.Now().UTC()
	id := onlineMachine(t, st, "sampleagent2")

	b := batch(now)
	b.BAT = model.BAT{
		Running: true, Port: 9876, Bind: "localhost",
		Argv:        "/opt/bat-server/bat-server --bind=localhost --port=9876",
		ListenAddrs: []string{"127.0.0.1"},
	}
	if err := st.RecordObservation(id, b, now); err != nil {
		t.Fatalf("observation: %v", err)
	}

	panel := batPanel(get(t, s, "/machines/"+id))
	if strings.Contains(panel, `<pre class="copy">`) {
		t.Errorf("綁在 localhost 卻還是給了一個位址 —— 那個位址一定連不上：\n%s", panel)
	}
	for _, want := range []string{"127.0.0.1", "localhost", "連不進來"} {
		if !strings.Contains(panel, want) {
			t.Errorf("沒有講出「為什麼連不上」裡的 %q：\n%s", want, panel)
		}
	}
}

// TestConnectSaysSoWhenBATIsNotRunningAtAll
// ⚠ 「沒跑 BAT」跟「連不上」是兩件事，畫面要分得出來。
func TestConnectSaysSoWhenBATIsNotRunningAtAll(t *testing.T) {
	s, st := newServer(t)
	now := time.Now().UTC()
	id := onlineMachine(t, st, "sampleagent1")

	b := batch(now)
	b.BAT = model.BAT{Reason: "掃了 312 個 process，沒有一個是 bat-server"}
	if err := st.RecordObservation(id, b, now); err != nil {
		t.Fatalf("observation: %v", err)
	}

	panel := batPanel(get(t, s, "/machines/"+id))
	if strings.Contains(panel, `<pre class="copy">`) {
		t.Errorf("沒有 bat-server 卻給了位址：\n%s", panel)
	}
	if !strings.Contains(panel, "沒有一個是 bat-server") {
		t.Errorf("沒有把 agent 給的原因原文顯示出來：\n%s", panel)
	}
}

// TestConnectDistinguishesUnknownFromAbsent
//
// ⚠ 舊版 agent 不會送 BAT 這個欄位。那時候畫面必須說「還沒回報過」，
// 不可以說「沒跑 BAT」—— 把「不知道」寫成「沒有」是這個專案要防的核心失效。
func TestConnectDistinguishesUnknownFromAbsent(t *testing.T) {
	s, st := newServer(t)
	id := enroll(t, st, "oldagent", time.Now().UTC().Add(-time.Hour))

	panel := batPanel(get(t, s, "/machines/"+id))
	if !strings.Contains(panel, "尚無 bat-server 觀測") {
		t.Errorf("沒收過 BAT 觀測時說錯了話：\n%s", panel)
	}
}

// TestConnectUsesTheAddressTheMachineLastReported
//
// ⚠ 名冊上的 tailscale_ip 只在報到那一刻寫進去，之後的 check-in 不更新它。
// 一台換過 tailnet 位址的機器，用名冊那個舊值去連，最壞的結果不是連不上，
// 是**連到別台機器**然後在那台上動手 —— tailnet 位址會被回收。
//
// 這裡讓 BAT 綁在 0.0.0.0（所有介面），那時候「要用哪個 IP」才真的有選擇，
// 而正確答案是它自己最近一次報的那個，不是名冊裡的。
func TestConnectUsesTheAddressTheMachineLastReported(t *testing.T) {
	s, st := newServer(t)
	now := time.Now().UTC()
	id := onlineMachine(t, st, "samplehub1")

	b := batch(now)
	b.BAT = model.BAT{
		Running: true, Port: 9876, Bind: "all",
		Argv:        "/opt/bat-server/bat-server --bind=all --port=9876",
		ListenAddrs: []string{"0.0.0.0"},
	}
	if err := st.RecordObservation(id, b, now); err != nil {
		t.Fatalf("observation: %v", err)
	}
	// 名冊帶一個報到當天的舊位址。
	m, err := st.GetMachine(id)
	if err != nil {
		t.Fatalf("machine: %v", err)
	}
	m.TailscaleIP = "100.64.0.99"
	if err := st.UpsertMachine(m); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	panel := batPanel(get(t, s, "/machines/"+id))
	if !strings.Contains(panel, "https://100.64.0.1:9876") {
		t.Errorf("Connect 用的不是它最近一次自己報的位址：\n%s", panel)
	}
	if strings.Contains(panel, "https://100.64.0.99") {
		t.Errorf("Connect 給的是名冊裡報到當天的舊位址：\n%s", panel)
	}
	if !strings.Contains(panel, "100.64.0.99") {
		t.Errorf("兩個位址不一樣，但畫面沒有講出來 —— 安靜地選一個等於沒得選：\n%s", panel)
	}
}

// batPanel 只把 Connect 那一段切出來。
// ⚠ 測 Connect 就要看 Connect，不要看「頁面上某處」。
func batPanel(body string) string {
	i := strings.Index(body, `id="action-connect"`)
	if i < 0 {
		return "（頁面上根本沒有 Connect 這一段）"
	}
	rest := body[i:]
	if j := strings.Index(rest, "</div>"); j > 0 {
		return rest[:j]
	}
	return rest
}

// ---------------------------------------------------------------- 空狀態

// TestEmptyFleetStillRenders：名冊是空的時候不能是一片空白，
// 要告訴人下一步該打什麼指令。
func TestEmptyFleetStillRenders(t *testing.T) {
	s, _ := newServer(t)
	body := get(t, s, "/")
	if !strings.Contains(body, "名冊是空的") {
		t.Error("空名冊沒有講出「這裡是空的」")
	}
	if !strings.Contains(body, `href="/machines/enrollment"`) {
		t.Error("空名冊沒有註冊入口")
	}
}

// TestUnknownMachineIs404
func TestUnknownMachineIs404(t *testing.T) {
	s, _ := newServer(t)
	mux := http.NewServeMux()
	s.Routes(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/machines/nope", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("未知機器應該回 404，得到 %d", rec.Code)
	}
}

// ---------------------------------------------------------------- 小工具

func firstHeadline(body string) string {
	i := strings.Index(body, `<div class="headline">`)
	if i < 0 {
		return "(找不到第一屏)"
	}
	j := strings.Index(body[i:], "</div>")
	if j < 0 {
		return body[i:min(i+300, len(body))]
	}
	return body[i : i+j]
}

func around(body, word string) string {
	i := strings.Index(body, word)
	if i < 0 {
		return ""
	}
	return strings.TrimSpace(body[max(0, i-90):min(len(body), i+90)])
}

// TestDoesNotSingleOutOneOfManyIdenticalMachines：四台都是「從未報到」的時候，
// 不准寫「最該看的是 sampleagent1」。
//
// ⚠ 那個名字是照字母排序挑的，不是因為它比較嚴重。第一句已經說了
// 「4 台從未報到」，再點一個名字只會把讀者的注意力從四台縮到一台。
func TestDoesNotSingleOutOneOfManyIdenticalMachines(t *testing.T) {
	s, st := newServer(t)
	onlineMachine(t, st, "samplehub1")
	for _, n := range []string{"sampleagent1", "sampleagent2", "sampleagent3", "sampleagent4"} {
		if _, err := st.CreateEnrollToken(n, time.Hour); err != nil {
			t.Fatalf("token: %v", err)
		}
	}

	body := get(t, s, "/")
	head := firstHeadline(body)

	if !strings.Contains(head, "名冊上 5 台，1 台正在回報。4 台從未報到。") {
		t.Errorf("第一句沒有正確講出分母：%s", head)
	}
	if strings.Contains(head, "最該看的是") {
		t.Errorf("四台情況一樣卻點名了其中一台：%s", head)
	}
	// 但那四台都必須在畫面上點得到。
	for _, n := range []string{"sampleagent1", "sampleagent2", "sampleagent3", "sampleagent4"} {
		if !strings.Contains(body, n) {
			t.Errorf("%s 不在畫面上", n)
		}
	}
}

// TestNamesTheSingleWorstMachine：只有一台最嚴重的時候要點名，而且不能疊破折號。
func TestNamesTheSingleWorstMachine(t *testing.T) {
	s, st := newServer(t)
	onlineMachine(t, st, "samplehub1")
	if _, err := st.CreateEnrollToken("sampleagent3", time.Hour); err != nil {
		t.Fatalf("token: %v", err)
	}

	head := firstHeadline(get(t, s, "/"))
	if !strings.Contains(head, "最該看的是 sampleagent3：") {
		t.Errorf("沒有點名唯一那台，或接的字用錯：%s", head)
	}
	if strings.Contains(head, "—— 從未報到 ——") {
		t.Errorf("破折號疊在一起了：%s", head)
	}
}

// TestSilentMachineThatIsOnlineOnTailnetSaysSo ——
// 「機器死了」跟「機器活著但 agent 沒在報」，下一步要做的事完全不同，
// 而 Hub 自己永遠分不出這兩件事 —— 它只知道沒收到心跳。
//
// 實機上 sampleagent1 就是這樣：Hub 說「從未報到」（真的），
// 而它在 tailnet 上是 online 的。少了這一句，人會去查機器是不是掛了。
func TestSilentMachineThatIsOnlineOnTailnetSaysSo(t *testing.T) {
	body := renderDashboardWithTailnet(t, tailnet.Result{
		Available: true,
		OnlineButSilent: map[string]tailnet.Peer{
			"m-sampleagent1": {Hostname: "sampleagent1", IP: "100.64.200.8", Online: true},
		},
	})
	if !strings.Contains(body, "100.64.200.8") {
		t.Error("沒有帶出可以連過去的位址 —— 那句話就只是一個形容詞")
	}
	txt := stripTags(body)
	if !strings.Contains(txt, "online") || !strings.Contains(txt, "agent 未回報 Hub") {
		t.Errorf("沒有把「機器活著但 agent 沒在報」講出來：\n%s", firstLines(txt, 40))
	}
}

// ⚠ 問不到 tailscale 的時候，畫面不准長得像「檢查過了，沒有名冊外的機器」。
func TestNoTailscaleIsShownAsUnknownNotAsClean(t *testing.T) {
	txt := stripTags(renderDashboardWithTailnet(t, tailnet.Result{
		Unavailable: "這台 Hub 上沒有 tailscale 指令",
	}))
	if !strings.Contains(txt, "Tailnet 清單不可用") {
		t.Errorf("沒說「問不到」：\n%s", firstLines(txt, 40))
	}
	if strings.Contains(txt, "名冊裡都有") {
		t.Error("問不到卻說「Tailscale 上看得到的機器，名冊裡都有」—— 這是憑空生出來的結論")
	}
}

func TestUnreadableDeploymentLedgerIsUnknownNotInformational(t *testing.T) {
	body := renderDashboardWithTailnet(t, tailnet.Result{}, deploymentHeadlineRow{
		Text: "讀不到部署帳本，不能判斷有沒有部署卡住。", Unknown: true,
	})
	if !strings.Contains(body, `data-state="unknown"`) ||
		!strings.Contains(around(body, `data-state="unknown"`), `dot amber`) {
		t.Fatalf("讀不到部署帳本沒有畫成 unknown/amber：%s", around(body, "讀不到部署帳本"))
	}
}

// renderDashboardWithTailnet 直接算樣板，繞過真的去 exec tailscale。
// ⚠ 測試不准依賴這台機器上有沒有裝 tailscale、有沒有登入 ——
// 那會讓測試在 CI 上變成一個安靜的空綠燈。
func renderDashboardWithTailnet(t *testing.T, res tailnet.Result, deployments ...deploymentHeadlineRow) string {
	t.Helper()
	s, st := newServer(t)
	id := onlineMachine(t, st, "sampleagent1")
	ov, err := st.Overview(time.Now().UTC())
	if err != nil {
		t.Fatalf("overview: %v", err)
	}
	// 讓測資裡的 machine_id 對得上呼叫端寫的那個。
	if p, ok := res.OnlineButSilent["m-sampleagent1"]; ok {
		res.OnlineButSilent = map[string]tailnet.Peer{id: p}
	}
	var buf bytes.Buffer
	if err := s.tmpl.ExecuteTemplate(&buf, "dashboard.html", page{
		Title: "總覽", Nav: "dashboard", Now: "2026-09-03 07:00",
		Overview: ov, Dashboard: buildDashboardStats(ov, nil, true),
		MachineTable: buildDashboardMachineTable(ov, ""),
		Headline:     headline(ov), Tailnet: res, DeploymentHeadlines: deployments,
	}); err != nil {
		t.Fatalf("render: %v", err)
	}
	return buf.String()
}

// ---------------------------------------------------------------- Phase 2：憑證

// TestCredentialNeverClaimsItWorks 是 Phase 2 那條判準的牙齒：
// 「畫面上找不到任何『Logged in』的布林值」。
//
// ⚠ 這裡真正要擋的不是那三個英文字，是那個**念頭**：
// 把「檔案裡的到期時間還沒到」講成「這張票能用」。
// 那兩件事差很遠 —— 本機檔案裡沒有任何欄位能表達
// 「伺服器端把 session 踢掉了」。
func TestCredentialNeverClaimsItWorks(t *testing.T) {
	body := stripTags(machinePageWithCreds(t))
	for _, banned := range []string{
		"Logged in", "logged in", "已登入", "登入正常", "憑證正常", "可以使用", "驗證通過",
	} {
		if strings.Contains(body, banned) {
			t.Errorf("畫面上出現「%s」—— 那是在替一張沒有被驗證過的票背書\n%s",
				banned, around(body, banned))
		}
	}
}

// 三個欄位都要在畫面上，而且「沒有」要寫成字，不是留白。
//
// ⚠ 留白跟「從未驗證」在畫面上長得一樣，意思差很多 ——
// 前者看起來像「這一格不重要」，後者是「這件事我們根本沒做過」。
// 這是 2026-09-03 那四個 bug 的同一個形狀（PHASE1 §5.9）。
func TestCredentialShowsHowItKnows(t *testing.T) {
	body := stripTags(machinePageWithCreds(t))
	for _, want := range []string{"怎麼知道的", "從未驗證"} {
		if !strings.Contains(body, want) {
			t.Errorf("憑證那一段少了「%s」：\n%s", want, firstLines(body, 60))
		}
	}
	// 讀檔失敗的原文要看得見，而且不准被分類成一個形容詞。
	if !strings.Contains(body, "permission denied") {
		t.Errorf("讀憑證的錯誤原文沒有出現在畫面上：\n%s", firstLines(body, 60))
	}
}

func TestCredentialExpiredGraceControlsStatusColor(t *testing.T) {
	for _, test := range []struct {
		name, provider, color string
		overdue, lifetime     time.Duration
		wantOnline            bool
	}{
		{name: "in grace", provider: "claude-grace", color: "amber", overdue: 2 * time.Hour, lifetime: 8 * time.Hour, wantOnline: true},
		{name: "out of grace", provider: "claude-stuck", color: "red", overdue: 8 * time.Hour, lifetime: 8 * time.Hour},
	} {
		t.Run(test.name, func(t *testing.T) {
			s, st := newServer(t)
			now := time.Now().UTC()
			id := onlineMachine(t, st, "credential-"+test.provider)
			expires := now.Add(-test.overdue)
			mtime := expires.Add(-test.lifetime)
			b := model.ObservationBatch{SchemaVersion: model.SchemaVersion, MeasuredAt: now}
			b.Credentials = []model.Credential{{
				Provider: test.provider, Status: model.CredExpired, ExpiresAt: &expires, FileMTime: &mtime,
			}}
			if err := st.RecordObservation(id, b, now); err != nil {
				t.Fatal(err)
			}
			body := get(t, s, "/machines/"+id)
			credentials := sectionOf(t, body, "<h2>登入憑證</h2>", "<h2>CLI 工具</h2>")
			anchor := strings.Index(credentials, "<td>"+test.provider+"</td>")
			if anchor < 0 {
				t.Fatalf("憑證表找不到 %q：%s", test.provider, credentials)
			}
			end := strings.Index(credentials[anchor:], "</tr>")
			if end < 0 {
				t.Fatalf("憑證列沒有結尾：%s", credentials[anchor:])
			}
			row := credentials[anchor : anchor+end]
			if !strings.Contains(row, `class="st `+test.color+`">已過期</td>`) {
				t.Fatalf("憑證列顏色不符：%s", row)
			}
			if test.wantOnline {
				heading := sectionOf(t, body, `<div class="page-heading`, "</div></div>")
				if !strings.Contains(heading, "—— 在線</h1>") || strings.Contains(heading, "—— 降級</h1>") {
					t.Fatalf("寬限內憑證改變頁首狀態：%s", heading)
				}
			}
		})
	}
}

func TestCredentialPageExplainsVerificationMethod(t *testing.T) {
	t.Run("file parse", func(t *testing.T) {
		s, st := newServer(t)
		id := onlineMachine(t, st, "credential-file-parse")
		now := time.Now().UTC()
		b := model.ObservationBatch{SchemaVersion: model.SchemaVersion, MeasuredAt: now}
		b.Credentials = []model.Credential{{
			Provider: "codex", Status: model.CredConfigured, VerificationMethod: model.VerifyFileParse,
		}}
		if err := st.RecordObservation(id, b, now); err != nil {
			t.Fatalf("record file parse credential: %v", err)
		}
		body := stripTags(get(t, s, "/machines/"+id))
		for _, want := range []string{"有憑證，還沒到期", "從未驗證", "只讀了本機檔案"} {
			if !strings.Contains(body, want) {
				t.Errorf("file parse credential page missing %q: %s", want, firstLines(body, 60))
			}
		}
	})

	t.Run("live request", func(t *testing.T) {
		s, st := newServer(t)
		id := onlineMachine(t, st, "credential-live-request")
		now := time.Now().UTC()
		verifiedAt := now.Add(-time.Minute)
		b := model.ObservationBatch{SchemaVersion: model.SchemaVersion, MeasuredAt: now}
		b.Credentials = []model.Credential{{
			Provider: "codex", Status: model.CredConfigured, VerificationMethod: model.VerifyLiveRequest,
			VerifiedAt: &verifiedAt,
		}}
		if err := st.RecordObservation(id, b, now); err != nil {
			t.Fatalf("record live request credential: %v", err)
		}
		body := stripTags(get(t, s, "/machines/"+id))
		if !strings.Contains(body, "打了一個真實請求") {
			t.Errorf("live request credential page missing verification label: %s", firstLines(body, 60))
		}
	})

	t.Run("legacy observation", func(t *testing.T) {
		s, st := newServer(t)
		id := onlineMachine(t, st, "credential-legacy")
		now := time.Now().UTC()
		b := model.ObservationBatch{SchemaVersion: model.SchemaVersion, MeasuredAt: now}
		b.Credentials = []model.Credential{{Provider: "codex", Status: model.CredConfigured}}
		if err := st.RecordObservation(id, b, now); err != nil {
			t.Fatalf("record legacy credential: %v", err)
		}
		body := stripTags(get(t, s, "/machines/"+id))
		if !strings.Contains(body, "來源不明（舊觀測）") {
			t.Errorf("legacy credential page missing verification label: %s", firstLines(body, 60))
		}
	})
}

func TestMachinePageRendersTypedCredentialAndOccupancyWithoutPrivateIdentifiers(t *testing.T) {
	s, st := newServer(t)
	now := time.Now().UTC()
	id := onlineMachine(t, st, "typed-credential-occupancy")
	mtime := now.Add(-2 * time.Hour)
	expires := mtime.Add(8 * time.Hour)
	b := batch(now.Add(-time.Minute))
	b.Credentials = []model.Credential{{
		Provider: "codex", Status: model.CredConfigured, FileMTime: &mtime, ExpiresAt: &expires,
		VerificationMethod: model.VerifyFileParse, ActiveAccountID: "PRIVATE_ACCOUNT_ID", AccountCount: 2,
		Note: "note\x1b[31m", LastError: "permission denied\nretry\x1b[31m",
	}}
	b.OpenClaw.DB.OccupancyRowsSeen = 2
	b.OpenClaw.DB.OccupancyRowsNoProvider = 1
	b.OpenClaw.DB.Occupancy = []model.OccupancyEvidence{{
		Source: "cron_run_logs", JobID: "PRIVATE_JOB", At: now.Add(-2 * time.Minute),
		Provider: "openai", AgentID: "main", SessionKey: "PRIVATE_SESSION", ErrorText: "PRIVATE_RUN_ERROR",
	}}
	if err := st.RecordObservation(id, b, now.Add(-30*time.Second)); err != nil {
		t.Fatal(err)
	}

	body := get(t, s, "/machines/"+id)
	for _, want := range []string{
		"openai", "cron_run_logs", "main", "2 個回合中，1 個未記錄供應商",
		"目前有選中的帳號（識別碼不揭露）", "共 2 個，會被熱抽換", "codex",
		"note�[31m", "permission denied\nretry�[31m", "measured_at（agent）", "received_at（Hub）",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("typed credential/occupancy page missing %q", want)
		}
	}
	for _, forbidden := range []string{"PRIVATE_ACCOUNT_ID", "PRIVATE_JOB", "PRIVATE_SESSION", "PRIVATE_RUN_ERROR"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("typed credential/occupancy page exposed %q", forbidden)
		}
	}
	if strings.ContainsRune(body, '\x1b') {
		t.Fatal("typed credential/occupancy page contains a raw terminal escape byte")
	}
}

// machinePageWithCreds 造一台帶各種憑證狀態的機器，然後把單機頁算出來。
func machinePageWithCreds(t *testing.T) string {
	t.Helper()
	s, st := newServer(t)
	id := onlineMachine(t, st, "samplehub1")
	now := time.Now().UTC()
	exp := now.Add(-3 * time.Hour)

	b := model.ObservationBatch{SchemaVersion: model.SchemaVersion, MeasuredAt: now}
	b.Credentials = []model.Credential{
		{Provider: "claude", Status: model.CredExpired, ExpiresAt: &exp,
			VerificationMethod: model.VerifyFileParse},
		{Provider: "codex", Status: model.CredConfigured,
			VerificationMethod: model.VerifyFileParse},
		{Provider: "grok", Status: model.CredUnknown,
			VerificationMethod: model.VerifyFileParse,
			LastError:          "open /home/x/.grok/auth.json: permission denied"},
		{Provider: "gemini", Status: model.CredAbsent},
	}
	if err := st.RecordObservation(id, b, now); err != nil {
		t.Fatalf("record: %v", err)
	}
	return get(t, s, "/machines/"+id)
}

// TestCredentialTableShowsPeerRefreshEvidence 守的是跨機器證據真的走到詳細頁，
// 而不是只存在判決資料裡。這裡只顯示 provider 層級的事實，不把它寫成同一張票。
func TestCredentialTableShowsPeerRefreshEvidence(t *testing.T) {
	s, st := newServer(t)
	targetID := onlineMachine(t, st, "sampleagent2")
	peerID := onlineMachine(t, st, "samplehub1")
	now := time.Now().UTC()

	record := func(id string, at, mt time.Time) {
		t.Helper()
		exp := mt.Add(8 * time.Hour)
		b := batch(at)
		b.Credentials = []model.Credential{{
			Provider: "claude", Status: model.CredConfigured, ExpiresAt: &exp, FileMTime: &mt,
		}}
		if err := st.RecordObservation(id, b, at); err != nil {
			t.Fatalf("寫入 %s 在 %s 的憑證觀測：%v", id, at, err)
		}
	}

	targetMTime := now.Add(-9 * time.Hour)
	record(targetID, now.Add(-time.Minute), targetMTime)
	record(peerID, now.Add(-10*time.Minute), now.Add(-9*time.Hour))
	record(peerID, now.Add(-30*time.Second), now.Add(-time.Hour))

	body := get(t, s, "/machines/"+targetID)
	if !strings.Contains(body, "同一家在 samplehub1 續了 <b>1</b> 次") {
		t.Errorf("詳細頁少了同儕續期證據：\n%s", firstLines(body, 80))
	}
}

// 判準：「在同一台上製造五種憑證狀態 → UI 顯示五個詞」。
//
// ⚠ 重點在「不同的詞」。一個把 unknown 跟 configured 都寫成「正常」的畫面，
// 技術上也「顯示了五種狀態」—— 而它剛好毀掉這個欄位存在的理由。
// 所以這裡不只檢查每個詞在不在，還檢查它們**互不相同**。
func TestAllCredentialStatesGetTheirOwnWord(t *testing.T) {
	all := []model.CredStatus{
		model.CredAbsent, model.CredConfigured, model.CredExpiresSoon,
		model.CredExpired, model.CredUnknown, model.CredFailed,
	}
	s, st := newServer(t)
	id := onlineMachine(t, st, "samplehub1")
	now := time.Now().UTC()
	b := model.ObservationBatch{SchemaVersion: model.SchemaVersion, MeasuredAt: now}
	for i, cs := range all {
		b.Credentials = append(b.Credentials, model.Credential{
			Provider:           fmt.Sprintf("p%d", i),
			Status:             cs,
			VerificationMethod: model.VerifyFileParse,
		})
	}
	if err := st.RecordObservation(id, b, now); err != nil {
		t.Fatalf("record: %v", err)
	}
	body := stripTags(get(t, s, "/machines/"+id))

	seen := map[string]model.CredStatus{}
	for _, cs := range all {
		w := credWord(t, cs)
		if !strings.Contains(body, w) {
			t.Errorf("%s 的詞「%s」沒有出現在畫面上", cs, w)
		}
		if other, dup := seen[w]; dup {
			t.Errorf("%s 跟 %s 共用同一個詞「%s」—— 那等於畫面上少了一種狀態",
				cs, other, w)
		}
		seen[w] = cs
	}
	// ⚠ 六個狀態六個詞，但每一個都必須是**人話**，不能是原始的 enum 字串洩出來。
	for _, cs := range all {
		if strings.Contains(body, string(cs)) {
			t.Errorf("畫面上出現了原始代號 %q —— 那代表 credLabel 沒認得它，"+
				"人看到的會是 expires_soon 而不是「即將過期」", cs)
		}
	}
}

func credWord(t *testing.T, cs model.CredStatus) string {
	t.Helper()
	fn, ok := funcs["credLabel"].(func(model.CredStatus) string)
	if !ok {
		t.Fatal("credLabel 不見了或簽章變了")
	}
	return fn(cs)
}

// 判準：adapter 碰到未知 CLI 版本 → 畫面上仍看得到 raw version 字串與
// unsupported，但專用 binary/database path 欄位不跨過 disclosure。
//
// ⚠ 這一條放在 web 層而不是 probe 層，因為 probe 早就存對了 ——
// 「存了但畫面上沒有」跟「沒存」對使用者是同一件事（PHASE1 §5.0：
// bug 住在零件之間）。
func TestUnsupportedToolStillShowsPathAndRawString(t *testing.T) {
	s, st := newServer(t)
	id := onlineMachine(t, st, "samplehub1")
	now := time.Now().UTC()
	b := model.ObservationBatch{SchemaVersion: model.SchemaVersion, MeasuredAt: now}
	b.CLITools = []model.CLITool{{
		Name:          "weirdcli",
		Present:       true,
		Path:          "/usr/local/bin/weirdcli",
		RealPath:      "/usr/local/bin/weirdcli",
		VersionRaw:    "weirdcli (nightly channel, build cosmic-otter)",
		VersionReason: "認不得 --version 的輸出格式，原文留在 version_raw",
		Support:       model.SupportUnsupported,
	}}
	b.OpenClaw = model.OpenClaw{Present: true, DB: &model.OpenClawDB{
		Support: model.SupportUnsupported,
		Reason:  "認得的兩條路徑上都沒有資料庫",
		FoundAt: []string{"/home/x/.openclaw/data/openclaw.sqlite"},
	}}
	if err := st.RecordObservation(id, b, now); err != nil {
		t.Fatalf("record: %v", err)
	}
	body := stripTags(get(t, s, "/machines/"+id))

	for _, want := range []string{"unsupported", "cosmic-otter", "找到 1 個 SQLite 檔"} {
		if !strings.Contains(body, want) {
			t.Errorf("畫面上找不到 %q —— unsupported 的可操作證據不完整", want)
		}
	}
	for _, private := range []string{"/usr/local/bin/weirdcli", "/home/x/.openclaw/data/openclaw.sqlite"} {
		if strings.Contains(body, private) {
			t.Errorf("unsupported branch leaked dedicated path field %q", private)
		}
	}
}

// TestMachinePageSaysWhyZeroCronJobsAreQuiet 守的是「0 個排程工作被當成沒有狀況」這個錯。
// 頁面必須直說這台安靜是因為沒有人交代它做事。
func TestMachinePageSaysWhyZeroCronJobsAreQuiet(t *testing.T) {
	s, st := newServer(t)
	id := onlineMachine(t, st, "sampleagent3")
	now := time.Now().UTC()
	b := batch(now)
	b.OpenClaw.DB.CronJobsTotalMeasured = true
	b.OpenClaw.DB.CronJobsEnabledMeasured = true
	if err := st.RecordObservation(id, b, now); err != nil {
		t.Fatalf("寫入排程觀測失敗：%v", err)
	}

	body := stripTags(get(t, s, "/machines/"+id))
	want := "排程工作 0"
	if !strings.Contains(body, want) {
		t.Errorf("詳細頁沒有說明 0 個排程工作為什麼安靜：\n%s", body)
	}
}

// 詳細頁要把資料庫內容未知說成讀不到，不能呈現成確知沒有任何跑完紀錄。
func TestMachinePageSaysOpenClawDatabaseCouldNotBeRead(t *testing.T) {
	s, st := newServer(t)
	id := onlineMachine(t, st, "sampleagent3")
	now := time.Now().UTC()
	b := batch(now)
	b.OpenClaw.DB = &model.OpenClawDB{
		Present: true,
		Reason:  "sqlite: unable to open database file",
	}
	if err := st.RecordObservation(id, b, now); err != nil {
		t.Fatalf("寫入 OpenClaw 資料庫觀測失敗：%v", err)
	}

	body := stripTags(get(t, s, "/machines/"+id))
	evidenceStart := strings.Index(body, "安裝與執行")
	if evidenceStart < 0 {
		t.Fatal("詳細頁找不到 OpenClaw 證據區")
	}
	rowStart := strings.Index(body[evidenceStart:], "狀態資料庫")
	if rowStart < 0 {
		t.Fatal("詳細頁找不到狀態資料庫證據列")
	}
	rowStart += evidenceStart
	rowEnd := strings.Index(body[rowStart:], "最近一次跑完")
	if rowEnd < 0 {
		t.Fatal("詳細頁找不到狀態資料庫證據列的結尾")
	}
	databaseRow := body[rowStart : rowStart+rowEnd]
	for _, want := range []string{
		"已找到",
		"讀不到內容",
		"sqlite: unable to open database file",
	} {
		if !strings.Contains(databaseRow, want) {
			t.Errorf("狀態資料庫證據列找不到 %q：\n%s", want, databaseRow)
		}
	}
	for _, notWant := range []string{
		"這個資料庫裡沒有任何跑完的紀錄",
		"這個資料庫裡沒有排程工作跑完的紀錄",
	} {
		if strings.Contains(body, notWant) {
			t.Errorf("資料庫讀不到時頁面不可以顯示 %q：\n%s", notWant, body)
		}
	}
	for _, want := range []string{
		"沒有量到最近一次跑完的時間",
		"沒有量到排程工作跑完的時間",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("資料庫讀不到時頁面找不到 %q：\n%s", want, body)
		}
	}
}

func TestMachinePageDistinguishesSystemdPresentAbsentAndUnmeasured(t *testing.T) {
	s, st := newServer(t)
	id := onlineMachine(t, st, "systemd-three-states")
	now := time.Now().UTC()
	b := batch(now)
	b.Systemd = []model.Unit{
		{Name: "a-present.service", Present: true, Measured: true, ActiveState: "active", SubState: "running"},
		{Name: "b-absent.service", Measured: true},
		{Name: "c-unknown.service", Reason: "Failed to connect to bus"},
	}
	if err := st.RecordObservation(id, b, now); err != nil {
		t.Fatalf("寫入 systemd 三態觀測失敗：%v", err)
	}

	body := get(t, s, "/machines/"+id)
	absentRow := machineSystemdRowText(t, body, "b-absent.service")
	if !strings.Contains(absentRow, "沒有這個 unit") {
		t.Fatalf("systemctl 明確回報不存在的列沒有 absent 文案：%s", absentRow)
	}
	unknownRow := machineSystemdRowText(t, body, "c-unknown.service")
	for _, want := range []string{"沒有量到這個 unit", "Failed to connect to bus"} {
		if !strings.Contains(unknownRow, want) {
			t.Errorf("問不到 systemctl 的列找不到 %q：%s", want, unknownRow)
		}
	}
	if strings.Contains(unknownRow, "沒有這個 unit") {
		t.Errorf("問不到 systemctl 的列不可以說 unit 不存在：%s", unknownRow)
	}
}

func TestMachinePageTreatsLegacySystemdPayloadAsUnmeasured(t *testing.T) {
	s, st := newServer(t)
	id := onlineMachine(t, st, "legacy-systemd")
	now := time.Now().UTC()
	b := batch(now)
	b.Systemd = []model.Unit{{Name: "legacy-agent.service", Present: false}}
	if err := st.RecordObservation(id, b, now); err != nil {
		t.Fatalf("寫入舊 agent systemd 觀測失敗：%v", err)
	}

	body := get(t, s, "/machines/"+id)
	row := machineSystemdRowText(t, body, "legacy-agent.service")
	if !strings.Contains(row, "沒有量到這個 unit") || strings.Contains(row, "沒有這個 unit") {
		t.Fatalf("舊 agent 必須落在 typed unknown：%s", row)
	}
}

func machineSystemdRowText(t *testing.T, body, unit string) string {
	t.Helper()
	start := strings.Index(body, unit)
	if start < 0 {
		t.Fatalf("詳細頁找不到 systemd unit %q", unit)
	}
	end := strings.Index(body[start+len(unit):], "</tr>")
	if end < 0 {
		t.Fatalf("詳細頁找不到 systemd unit %q 的列尾", unit)
	}
	return stripTags(body[start : start+len(unit)+end])
}

// TestMachinePageShowsOpenClawInstallFacts 守的是三種很容易被折成同一個空值的狀態：
// 舊 agent 沒回報、真的量到不可寫、以及 unit 與 process 指向不同檔案。
func TestMachinePageShowsOpenClawInstallFacts(t *testing.T) {
	boolp := func(v bool) *bool { return &v }
	baseInstall := func() *model.OpenClawInstall {
		n := 2
		return &model.OpenClawInstall{
			UnitFound: true, UnitPath: "/home/example-user/.config/systemd/user/openclaw-gateway.service",
			DropInPaths: []string{"/home/example-user/.config/systemd/user/openclaw-gateway.service.d/override.conf"},
			ExecStart:   "/usr/bin/node /home/example-user/.local/node_modules/openclaw/dist/index.js gateway --port 18789",
			KillMode:    "control-group", NRestarts: &n, MainPID: 4242,
			NodePath: "/usr/bin/node", NodeVersion: "v24.15.0",
			NpmPath: "/usr/bin/npm", NpmVersion: "11.12.1",
			RunningDir: "/home/example-user/.local/node_modules/openclaw", RunningDirExists: true,
			RunningDirOwner: "example-user", RunningDirWritable: boolp(true), RunningDirVersion: "2026.6.6",
			ProcessIndexJS: "/home/example-user/.local/node_modules/openclaw/dist/index.js", ProcessMatchesUnit: boolp(true),
			GatewayArgs: []string{"gateway", "--port", "18789"},
			ReleasesDir: "/home/example-user/.local/share/clawctl/openclaw/releases", ReleasesPresent: true,
			CurrentLink: "releases/2026.6.6", DiskFreeBytes: 8 << 30, DiskFreeMeasured: true,
		}
	}

	tests := []struct {
		name    string
		mutate  func(*model.ObservationBatch)
		want    []string
		notWant []string
	}{
		{
			name: "完整",
			mutate: func(b *model.ObservationBatch) {
				b.OpenClaw.Install = baseInstall()
				b.OpenClaw.GatewayVersion = "2026.6.6"
				b.OpenClaw.CLIVersion = "2026.6.6"
			},
			want:    []string{"安裝與執行", "v24.15.0", "11.12.1", "current link：有", "8.0 GiB"},
			notWant: []string{"/usr/bin/node", "/usr/bin/npm", "releases/2026.6.6", "4242", "agent 版本太舊", "agent 不能寫 gateway 目前使用的目錄", "unit 與跑著的 process 指向不同份", "PATH 上的 openclaw 是"},
		},
		{
			name:    "沒有 Install",
			mutate:  func(*model.ObservationBatch) {},
			want:    []string{"尚無安裝資訊"},
			notWant: []string{"agent 不能寫這個目錄", "unit 改過但沒重啟"},
		},
		{
			// samplehub1 實測：/usr/bin/openclaw 指向 root 那份 2026.6.1，gateway 跑的是
			// ~/.local 那份 2026.6.6；人手打 openclaw --version 看到的不是 gateway 那份。
			name: "不可寫且 PATH 上的 openclaw 是另一份",
			mutate: func(b *model.ObservationBatch) {
				b.OpenClaw.Install = baseInstall()
				b.OpenClaw.Install.RunningDirWritable = boolp(false)
				b.OpenClaw.Install.RunningDirOwner = "root"
				b.OpenClaw.GatewayVersion = "2026.6.6"
				b.OpenClaw.CLIVersion = "2026.6.1"
			},
			want: []string{
				"Gateway 目錄不可寫",
				"CLI 2026.6.1；gateway 2026.6.6",
			},
			notWant: []string{"擁有者 root", "unit 與跑著的 process 指向不同份"},
		},
		{
			name: "process 不一致",
			mutate: func(b *model.ObservationBatch) {
				b.OpenClaw.Install = baseInstall()
				b.OpenClaw.Install.ProcessIndexJS = "/old/openclaw/dist/index.js"
				b.OpenClaw.Install.ProcessMatchesUnit = boolp(false)
				b.OpenClaw.GatewayVersion = "2026.6.6"
			},
			want: []string{
				"Unit 與 process 指向不同安裝",
			},
			notWant: []string{"/old/openclaw", "/home/example-user/.local/node_modules/openclaw", "agent 不能寫 gateway 目前使用的目錄", "agent 版本太舊"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, st := newServer(t)
			id := onlineMachine(t, st, "samplehub1")
			now := time.Now().UTC()
			b := batch(now)
			tt.mutate(&b)
			if err := st.RecordObservation(id, b, now); err != nil {
				t.Fatalf("寫入 OpenClaw 安裝觀測：%v", err)
			}
			body := stripTags(get(t, s, "/machines/"+id))
			for _, want := range tt.want {
				if !strings.Contains(body, want) {
					t.Errorf("頁面找不到 %q：\n%s", want, body)
				}
			}
			for _, notWant := range tt.notWant {
				if strings.Contains(body, notWant) {
					t.Errorf("頁面不該出現 %q：\n%s", notWant, body)
				}
			}
		})
	}
}

// Reports → Tickets：這一頁一半在講「我們知道的」，一半在講「我們不知道的」。
//
// ⚠ 第二半不是免責聲明。PHASES.md 設計的表有九欄，我們誠實地只填得出五欄，
// 而沒有名字的空白會被讀成「那一欄不重要」，不是「我們還不知道」。
func TestTicketPageKeepsReportFocusedOnRecordedFields(t *testing.T) {
	s, _ := newServer(t)
	body := stripTags(get(t, s, "/reports/tickets"))

	for _, want := range []string{"報到率", "provider", "距上次跑完", "有錯誤的回合"} {
		if !strings.Contains(body, want) {
			t.Errorf("週報上找不到 %q", want)
		}
	}
	for _, forbidden := range []string{"這一頁答不出來", "結論欄請自己填", "空著是刻意的"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("週報仍顯示防禦性文案 %q", forbidden)
		}
	}
}

// 報到率不到 80% → 畫面上要擋，不是照常顯示一張看起來很正常的表。
func TestLowReportingRateBlocksSchedulingTalk(t *testing.T) {
	s, st := newServer(t)
	// 名冊 5 台，只有 1 台講過話 → 20%
	onlineMachine(t, st, "samplehub1")
	for _, n := range []string{"sampleagent1", "sampleagent2", "sampleagent3", "sampleagent4"} {
		enroll(t, st, n, time.Now().UTC().Add(-time.Hour))
	}
	body := stripTags(get(t, s, "/reports/tickets"))
	if !strings.Contains(body, "調度分析停用") {
		t.Errorf("報到率只有五分之一還照常顯示 —— 那張表的每個結論都是抽樣偏誤\n%s",
			around(body, "報到率"))
	}
}

// CSV 要下載得動，而且欄名本身要誠實 ——
// ⚠ 匯出檔是這份資料唯一會被複製到別處的東西，在那裡它沒有旁邊的說明文字保護它。
func TestTicketCSVHeaderIsHonestOnItsOwn(t *testing.T) {
	s, _ := newServer(t)
	mux := http.NewServeMux()
	s.Routes(mux)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/reports/tickets.csv", nil))
	if w.Code != 200 {
		t.Fatalf("狀態 %d", w.Code)
	}
	body := w.Body.String()
	if !strings.HasPrefix(body, "\ufeff") {
		t.Error("沒有 BOM —— Excel 會把中文欄名讀成亂碼，而那是它唯一會被打開的地方")
	}
	if !strings.Contains(body, "距上次跑完") {
		t.Errorf("CSV 欄名沒寫「跑完」：%q", firstLine(body))
	}
	if strings.Contains(body, "距上次成功") {
		t.Errorf("CSV 欄名寫了「成功」，而我們沒有成功這個資料：%q", firstLine(body))
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// ---------------------------------------------------------------- BAT 與前端

// PHASES.md Phase 3 的第二條判準：「瀏覽器與前端 log 裡找不到 BAT token」。
//
// ⚠⚠ 這條判準在這個設計裡是**架構上成立的**，不是靠小心翼翼維持的。
// 那讓它很容易變成一個沒有人守的空條件，所以要把成立的理由寫成測試：
//
//  1. 沒有前端。整個 UI 是伺服器端算好的 HTML，一行 JavaScript 都沒有。
//     沒有 JS 就沒有 console、沒有 XHR、沒有 sourcemap —— 「前端 log」
//     這個東西根本不存在，所以裡面找不到任何東西。
//  2. Hub 從來沒有拿到過 BAT 的憑證。它對 BAT 的全部知識是
//     「bat-server.service 這個 unit 現在是什麼狀態」加上一個位址。
//     拿不到的東西漏不出去。
//  3. 那個位址是乾淨的 —— 沒有 query string、沒有 fragment。
//     一旦有人為了「方便」在網址後面掛上任何參數，這裡就會紅。
//
// 會讓這個測試紅的那次修改，正好就是這條判準真正要擋的那次修改：
// 把 BAT 嵌進來、或是幫使用者代打認證。
func TestNoFrontEndMeansNoFrontEndLog(t *testing.T) {
	s, st := newServer(t)
	id := onlineMachine(t, st, "samplehub1")

	for _, path := range []string{"/", "/machines/" + id, "/reports/tickets"} {
		body := get(t, s, path)
		low := strings.ToLower(body)
		// ⚠ 連 <script> 開頭都不准出現。有一個就有第二個，
		// 而第二個會是「只是加個複製按鈕」。
		for _, bad := range []string{"<script", "javascript:", "onclick=", "onload=", "<iframe"} {
			if strings.Contains(low, bad) {
				t.Errorf("%s 出現了 %q —— 沒有前端才是「前端 log 裡找不到 token」的理由", path, bad)
			}
		}
	}
}

// TestBATAddressCarriesNoCredential 盯著那個可複製的位址本身。
//
// ⚠ 只斷言「不含目前想得到的那幾個字」是沒有用的 —— 明天多一種寫法就漏了。
// 所以這裡斷言的是**形狀**：可複製區塊必須是純粹的 scheme://host:port，
// 任何 ? 或 # 或 = 都代表有人開始往裡面塞東西了。
func TestBATAddressCarriesNoCredential(t *testing.T) {
	s, st := newServer(t)
	id := onlineMachine(t, st, "samplehub1")
	body := get(t, s, "/machines/"+id)

	re := regexp.MustCompile(`(?s)<pre class="copy">(.*?)</pre>`)
	blocks := re.FindAllStringSubmatch(body, -1)
	if len(blocks) == 0 {
		t.Fatal("頁面上沒有可複製的位址 —— Connect 這一段整個不見了")
	}
	found := false
	for _, b := range blocks {
		addr := strings.TrimSpace(b[1])
		if !strings.HasPrefix(addr, "https://") {
			continue
		}
		found = true
		if !regexp.MustCompile(`^https://[0-9a-zA-Z.:\[\]-]+:\d+$`).MatchString(addr) {
			t.Errorf("BAT 位址 %q 不是乾淨的 scheme://host:port —— 有人往裡面加了東西", addr)
		}
	}
	if !found {
		t.Errorf("找不到 BAT 的位址，實際的可複製區塊：%v", blocks)
	}
}

// TestEveryPageTitleIsSaneOnce 逐頁看 <title>。
//
// ⚠ 這個測試是一個真的 bug 抓出來之後補的：有三頁的 Title 自己帶了
// 「· AI-Intune」，而 base.html 又接了一次，所以分頁標籤上會重複產品名。
//
// 它活下來的原因是**沒有任何測試看過 <title>**。所有測試都在斷言
// <body> 裡的內容，而 </html> 那個截斷偵測只證明樣板跑完了。
// 這一頁一頁去對的形式是刻意的：一個只檢查首頁的版本會漏掉那三頁，
// 而那三頁正好就是壞掉的那三頁 —— 首頁是對的。
func TestEveryPageTitleIsSaneOnce(t *testing.T) {
	s, st := newServer(t)
	id := onlineMachine(t, st, "samplehub1")

	for _, path := range []string{"/", "/machines/" + id, "/reports/tickets", "/audit"} {
		body := get(t, s, path)
		m := regexp.MustCompile(`<title>(.*?)</title>`).FindStringSubmatch(body)
		if m == nil {
			t.Errorf("%s 沒有 <title>", path)
			continue
		}
		title := m[1]
		if n := strings.Count(title, "AI-Intune"); n != 1 {
			t.Errorf("%s 的標題是 %q —— 「AI-Intune」出現 %d 次，應該剛好 1 次", path, title, n)
		}
		if strings.TrimSpace(strings.ReplaceAll(title, "· AI-Intune", "")) == "" {
			t.Errorf("%s 的標題只有產品名 %q —— 分頁標籤上分不出是哪一頁", path, title)
		}
	}
}

// ---------------------------------------------------------------- 清舊資料

// 清理過之後，畫面不准說「這裡看到的就是全部」。
//
// ⚠ 這支測試跨三層：store 的 Prune 刪資料、web 的 retentionNote 判斷三種狀態、
// 樣板挑一句話講出來。三層各自的測試都可以是綠的而這件事還是壞的 ——
// 例如 retentionNote 算對了但樣板寫成 `{{if .Retention.EverRun}}` 的反面，
// 於是清過的機器會被說成「就是全部歷史」。bug 住在零件中間（§5.0）。
//
// ⚠⚠ 這句話錯掉的代價不是「少講一句」：一頁只剩 14 天資料的機器，
// 看起來跟一台 14 天前才出生的機器一模一樣，而畫面會替我們保證它是後者。
func TestAPrunedPageDoesNotClaimToShowEverything(t *testing.T) {
	s, st := newServer(t)
	now := time.Now().UTC()
	id := enroll(t, st, "samplehub1", now.Add(-60*24*time.Hour))

	// 60 天的觀測與心跳，然後清一次。
	for d := 60; d >= 0; d -= 3 {
		at := now.Add(-time.Duration(d) * 24 * time.Hour)
		if err := st.RecordObservation(id, batch(at), at); err != nil {
			t.Fatalf("observe: %v", err)
		}
		if err := st.RecordCheckin(id, model.Checkin{
			SchemaVersion: model.SchemaVersion, SentAt: at, AgentVersion: "test",
			BootID: "boot-1", AgentSeq: 1,
		}, at); err != nil {
			t.Fatalf("checkin: %v", err)
		}
	}

	// 清之前：畫面要顯示尚未清理的 typed 狀態。
	page := get(t, s, "/machines/"+id)
	if !strings.Contains(page, "尚未清理觀測資料") {
		t.Error("還沒清過的時候，畫面沒有顯示尚未清理狀態")
	}

	rep, err := st.Prune(now, store.DefaultRetention(), false)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if rep.Total() == 0 {
		t.Fatal("設定就錯了：這一輪什麼都沒清掉，後面的斷言測不到東西")
	}

	page = get(t, s, "/machines/"+id)
	if strings.Contains(page, "尚未清理觀測資料") {
		t.Errorf("已經清掉 %d 列了，畫面還顯示尚未清理", rep.Total())
	}
	if !strings.Contains(page, "最舊保留觀測") {
		t.Error("畫面沒有顯示最舊保留觀測")
	}
	// 總覽也要跟著改口。
	dash := get(t, s, "/")
	if strings.Contains(dash, "還沒清過任何舊資料") {
		t.Error("清過了，總覽還在說「還沒清過」")
	}
	if !strings.Contains(dash, "舊資料上一次清理") {
		t.Error("總覽沒有講出上一次清理的時間 ——" +
			"一個從來不出聲的清理程序，跟一個壞掉三個月的清理程序長得一樣")
	}
}

// 讀不到清理紀錄的時候，畫面不准說「沒清過」。
//
// ⚠ 這是三種狀態裡最容易被寫成 zero value 的那一種：
// 「讀不到」跟「沒清過」都是 EverRun=false，合在一起的話，
// 畫面會說「這裡看到的就是全部」—— 一句我們根本沒問到答案的保證。
func TestUnknownRetentionIsNotReportedAsNothingPruned(t *testing.T) {
	s, st := newServer(t)
	now := time.Now().UTC()
	id := enroll(t, st, "samplehub1", now.Add(-time.Hour))
	if err := st.RecordObservation(id, batch(now), now); err != nil {
		t.Fatalf("observe: %v", err)
	}

	// 把清理紀錄那張表拿掉，模擬「讀不到」。
	if _, err := st.DB().Exec(`DROP TABLE retention_log`); err != nil {
		t.Fatalf("drop: %v", err)
	}

	for _, tc := range []struct {
		path string
		want string
	}{
		{path: "/", want: "觀測清理狀態未知"},
		{path: "/machines/" + id, want: "觀測保留狀態未知"},
	} {
		body := get(t, s, tc.path)
		if !strings.Contains(body, tc.want) {
			t.Errorf("%s 沒有顯示 %q", tc.path, tc.want)
		}
		if strings.Contains(body, "尚未清理觀測資料") ||
			strings.Contains(body, "還沒清過任何舊資料") {
			t.Errorf("%s 把未知狀態顯示成尚未清理", tc.path)
		}
	}
}

// 釘的是 web.go:962「任何一步失敗都必須變成『不知道』而不是『沒事』」。
//
// ⚠ 實測（全樹 go test ./... -count=1，下刀前）：把這一條的回傳改成
// `tailnet.Result{}`（一張乾淨的空表），全樹全綠。同一支函式上一條錯誤路徑
// （讀不到名冊）也是全綠。
//
// ⚠ 上面那一條（`RosterForTailnet`）下不了刀：它的錯誤源是 `ListMachines`，
// 而 web.go:303 與 `store.Overview`（store.go:2957）在 `tailnetResult` 被呼叫
// 之前就已經讀過同一張 `machines` 表。要讓它失敗就得先讓那兩個失敗，頁面
// 會在更早的地方變成「讀取機器名冊失敗」，根本走不到這一行。這一條記成
// 不可達，不硬湊。
//
// 同一種規矩在隔壁已經有看守者：`TestUnknownRetentionIsNotReportedAsNothingPruned`
// 用的就是 `DROP TABLE` 這條產品路徑。這一刀是同一個模子的另一格。
//
// ⚠ 這一支的第一版是假見證，兩層原因都記在這裡免得有人改回去：
//  1. newServer 沒設 tailnet 狀態，Reconcile 在 reconcile.go:76 就因為
//     !s.Available 回不可用。「清單不可用」在基底就已經出現，DROP TABLE
//     做不做都一樣。所以這一支一定要先 SetTailnetStatus 把基底墊成好消息，
//     而且要有一格空轉守衛去確認它真的是好消息。
//  2. tailnet.Result{} 的 Available 也是 false，dashboard.html:213 那句
//     amber 標題照畫，只有**理由**會變成空字串。所以驗標題永遠分不出
//     「問不到」跟「問到了但讀不到忽略清單」——要驗的是理由那一句。
//
// 下刀後四臂重量：
//   - 回一張乾淨的空表 → 只有這一支紅（others=[]）。
//   - 理由改成光禿禿的 err.Error()（不講是哪一個讀失敗）→ 也只有這一支紅。
//     那一句要講出「讀不到忽略清單」，operator 才知道下一步去看哪裡。
//   - 讀不到名冊那一條 → **仍然全空**，因為不可達（見上）。
//   - 第一屏 advisory 那一條 → 這一支維持綠，紅的是
//     TestFirstScreenDoesNotContradictItself。沒有白搭隔壁規矩的覆蓋。
func TestAnUnreadableTailnetListIsNotShownAsACleanFleet(t *testing.T) {
	s, st := newServer(t)
	now := time.Now().UTC()
	id := enroll(t, st, "samplehub1", now.Add(-time.Hour))
	if err := st.RecordObservation(id, batch(now), now); err != nil {
		t.Fatalf("observe: %v", err)
	}
	s.SetTailnetStatus(tailnet.Status{
		Available: true,
		Self:      tailnet.Peer{Hostname: "samplehub1", IP: "100.64.0.1", Online: true},
	})

	const cleanFleet = "Tailscale 上看得到的機器，名冊裡都有。"
	if body := get(t, s, "/"); !strings.Contains(body, cleanFleet) {
		t.Fatal("基底就不是好消息，這支測試證明不了任何事")
	}

	if _, err := st.DB().Exec(`DROP TABLE tailnet_ignored`); err != nil {
		t.Fatalf("drop: %v", err)
	}

	for _, path := range []string{"/", "/machines/enrollment"} {
		body := get(t, s, path)
		if !strings.Contains(body, "讀不到忽略清單") {
			t.Errorf("%s 沒有顯示讀不到忽略清單的理由", path)
		}
		if path == "/" && strings.Contains(body, cleanFleet) {
			t.Errorf("%s 把未知狀態顯示成 %q", path, cleanFleet)
		}
	}
}

// 一筆比 on_path 這個欄位更早的觀測，不准被標成「在跑但不在 PATH 上」。
//
// ⚠⚠ 這支測試守的是一個**遷移**的陷阱，而它差一點就上線了。
//
// `on_path` 是 2026-09-03 才加進 CLITool 的。畫面上那個新的警告本來寫成
// `{{if not .OnPath}}` —— 看起來完全正確。但資料庫裡每一筆更早的觀測
// 都沒有這個欄位，JSON 解出來一律是 `false`，於是**整個機隊的歷史**
// 都會被標成「在跑，但不在 PATH 上」。
//
// 一個缺席的欄位，意思是「這筆觀測比這個區別更早」，不是「否」。
// 所以那個判斷要看正面證據（present_evidence == "process"），
// 而不是一個布林的反面。
//
// ⚠ 這支測試刻意用**手寫的舊 JSON**，不用 model 結構去生 ——
// 用結構生的話，欄位永遠都在，這支測試就測不到它要測的那件事。
func TestObservationsOlderThanOnPathAreNotMislabelled(t *testing.T) {
	s, st := newServer(t)
	now := time.Now().UTC()
	id := enroll(t, st, "samplehub1", now.Add(-time.Hour))

	// 這就是舊觀測在資料庫裡的樣子：有 present、有 path，沒有 on_path。
	old := `{"name":"claude","present":true,"path":"/usr/local/bin/claude",` +
		`"realpath":"/usr/local/bin/claude","version_reported":"2.1.205",` +
		`"version_sources_disagree":false}`
	if _, err := st.DB().Exec(`
INSERT INTO observed_state
  (observation_id, machine_id, measured_at, received_at, kind, subject, payload, source)
VALUES ('old-1', ?, ?, ?, 'cli_tool', 'claude', ?, 'agent_measurement')`,
		id, now.Format(time.RFC3339), now.Format(time.RFC3339), old); err != nil {
		t.Fatalf("insert old observation: %v", err)
	}

	body := get(t, s, "/machines/"+id)
	if strings.Contains(body, "在跑，但不在 PATH 上") {
		t.Error("一筆沒有 on_path 欄位的舊觀測被標成「在跑但不在 PATH 上」。\n" +
			"那個欄位是後來才加的，舊資料裡它一律解成 false ——\n" +
			"缺席的欄位意思是「這筆觀測比這個區別更早」，不是「否」。\n" +
			"判斷要看正面證據（present_evidence），不是一個布林的反面。")
	}
	// 而且那一列該正常顯示版本，不是掉進某個警告分支。
	if !strings.Contains(body, "2.1.205") {
		t.Error("舊觀測的版號沒有顯示出來 —— 它掉進了別的分支")
	}
}

// 一個「在跑但不在 PATH 上」的工具，畫面上要講出它叫不動。
//
// ⚠ 這是 agy 在實機上的樣子（samplehub1，2026-09-03）：process 跑了三小時，
// OAuth token 剛更新過，而 PATH 上找不到它。
// 少了這一段，probe 那邊的修正在畫面上完全看不出來 —— 而看不出來的修正
// 等於沒修：人看畫面，不看 payload。
func TestAToolRunningWithoutBeingOnPathSaysSoOnScreen(t *testing.T) {
	s, st := newServer(t)
	now := time.Now().UTC()
	id := enroll(t, st, "samplehub1", now.Add(-time.Hour))

	payload := `{"name":"agy","present":true,"on_path":false,` +
		`"present_evidence":"process","running_pid":1725073,` +
		`"realpath":"/home/example-user/.gemini/antigravity-cli/bin/agy",` +
		`"version_reason":"不在 PATH 上，叫不動它，所以問不到版本",` +
		`"version_sources_disagree":false}`
	if _, err := st.DB().Exec(`
INSERT INTO observed_state
  (observation_id, machine_id, measured_at, received_at, kind, subject, payload, source)
VALUES ('agy-1', ?, ?, ?, 'cli_tool', 'agy', ?, 'agent_measurement')`,
		id, now.Format(time.RFC3339), now.Format(time.RFC3339), payload); err != nil {
		t.Fatalf("insert: %v", err)
	}

	body := get(t, s, "/machines/"+id)
	if strings.Contains(body, "沒有安裝") {
		t.Error("一個正在跑的工具被講成「沒有安裝」")
	}
	if !strings.Contains(body, "在跑，但不在 PATH 上") {
		t.Error("畫面沒有講出「它在跑，但 PATH 上找不到」")
	}
	if strings.Contains(body, "1725073") || strings.Contains(body, "/home/example-user/.gemini/antigravity-cli/bin/agy") {
		t.Error("typed machine page exposed the process PID or path")
	}
	if !strings.Contains(body, "process 存在；PATH 無此工具") {
		t.Error("畫面沒有顯示 process 與 PATH 狀態")
	}
}

// 兩個 PATH 解到兩個不同檔案時，畫面要保留差異關係，但不送出兩條路徑。
//
// ⚠ 這是 samplehub1 2026-09-03 的實測值：operator 打 claude 跑到
// ~/.local/share/claude/versions/2.1.258，clawctl 自己的 PATH 解到
// /usr/local/.../claude.exe（2.1.205）—— 而畫面只顯示過後者。
// 那不是一個紅燈、不是一格空白，是一個很像真的錯數字，
// 而人會照著它做決定（「已經是新版了，不用升級」）。
func TestAShadowedBinaryShowsBothFilesOnScreen(t *testing.T) {
	s, st := newServer(t)
	now := time.Now().UTC()
	id := enroll(t, st, "samplehub1", now.Add(-time.Hour))

	payload := `{"name":"claude","present":true,"on_path":true,` +
		`"present_evidence":"path","path_source":"login",` +
		`"path":"/home/example-user/.local/bin/claude",` +
		`"realpath":"/home/example-user/.local/share/claude/versions/2.1.258",` +
		`"daemon_reach":"shadowed",` +
		`"daemon_path":"/usr/local/lib/node_modules/@anthropic-ai/claude-code/bin/claude.exe",` +
		`"version_reported":"2.1.258","version_sources_disagree":false}`
	if _, err := st.DB().Exec(`
INSERT INTO observed_state
  (observation_id, machine_id, measured_at, received_at, kind, subject, payload, source)
VALUES ('shadow-1', ?, ?, ?, 'cli_tool', 'claude', ?, 'agent_measurement')`,
		id, now.Format(time.RFC3339), now.Format(time.RFC3339), payload); err != nil {
		t.Fatalf("insert: %v", err)
	}

	body := get(t, s, "/machines/"+id)
	if !strings.Contains(body, "2.1.258") {
		t.Error("畫面沒有顯示人真的在跑的那個版號")
	}
	if !strings.Contains(body, "登入 PATH 與 clawctl agent 指向不同檔案") {
		t.Error("畫面沒有講出登入 PATH 與 daemon PATH 指向不同檔案")
	}
	for _, private := range []string{"/home/example-user/.local/bin/claude", "/home/example-user/.local/share/claude", "claude.exe"} {
		if strings.Contains(body, private) {
			t.Errorf("typed machine page leaked dedicated path field %q", private)
		}
	}
}

// 早於 daemon_reach / path_source 這兩個欄位的觀測，不准被貼上任何警告。
//
// ⚠ 這支跟 TestObservationsOlderThanOnPathAreNotMislabelled 是同一個形狀，
// 而我在寫這一版 UI 的時候又差一點犯同一個錯：條件很自然會寫成
// {{if ne .DaemonReach "same"}} 或 {{if not (eq .PathSource "login")}} ——
// 兩者在舊資料上（空字串）都會成立，於是整個機隊的歷史一起變黃。
// 一個缺席的欄位意思是「這筆觀測比這個區別更早」，不是「有問題」。
func TestObservationsOlderThanTheTwoPathFieldsAreNotWarnedAbout(t *testing.T) {
	s, st := newServer(t)
	now := time.Now().UTC()
	id := enroll(t, st, "samplehub1", now.Add(-time.Hour))

	// 舊觀測在資料庫裡的樣子：有 on_path、有 present_evidence（那一輪加的），
	// 但沒有 path_source、沒有 daemon_reach。
	old := `{"name":"codex","present":true,"on_path":true,` +
		`"present_evidence":"path","path":"/usr/local/bin/codex",` +
		`"realpath":"/usr/local/bin/codex","version_reported":"0.5.9",` +
		`"version_sources_disagree":false}`
	if _, err := st.DB().Exec(`
INSERT INTO observed_state
  (observation_id, machine_id, measured_at, received_at, kind, subject, payload, source)
VALUES ('old-path-1', ?, ?, ?, 'cli_tool', 'codex', ?, 'agent_measurement')`,
		id, now.Format(time.RFC3339), now.Format(time.RFC3339), old); err != nil {
		t.Fatalf("insert: %v", err)
	}

	body := get(t, s, "/machines/"+id)
	for _, bad := range []string{"兩個不同的檔案", "拿 clawctl 自己的環境量的", "clawctl 自己的 PATH 上沒有它"} {
		if strings.Contains(body, bad) {
			t.Errorf("一筆早於這些欄位的舊觀測被貼上了警告：%q。\n"+
				"那些欄位是後來才加的，舊資料解出來一律是空字串 ——\n"+
				"條件要用正面證據（eq ... \"shadowed\"），不是否定式。", bad)
		}
	}
	if !strings.Contains(body, "0.5.9") {
		t.Error("舊觀測的版號沒有顯示出來 —— 它掉進了別的分支")
	}
}

// 量不到人的 PATH 而退回 daemon 環境的時候，畫面上必須說出來。
//
// ⚠ 沒有這一句，一整列在錯的環境下量出來的路徑跟版號，看起來跟正確的
// 一模一樣 —— 而這正是 2026-09-03 之前每一列的狀態。
func TestAToolMeasuredInTheWrongEnvironmentSaysSo(t *testing.T) {
	s, st := newServer(t)
	now := time.Now().UTC()
	id := enroll(t, st, "samplehub1", now.Add(-time.Hour))

	payload := `{"name":"grok","present":true,"on_path":true,` +
		`"present_evidence":"path","path_source":"daemon",` +
		`"path_reason":"問不到登入 shell（/bin/zsh）的 PATH：no such file or directory",` +
		`"path":"/usr/bin/grok","realpath":"/usr/lib/node_modules/@xai-official/grok/bin/grok",` +
		`"version_reported":"1.0.3","version_sources_disagree":false}`
	if _, err := st.DB().Exec(`
INSERT INTO observed_state
  (observation_id, machine_id, measured_at, received_at, kind, subject, payload, source)
VALUES ('wrongenv-1', ?, ?, ?, 'cli_tool', 'grok', ?, 'agent_measurement')`,
		id, now.Format(time.RFC3339), now.Format(time.RFC3339), payload); err != nil {
		t.Fatalf("insert: %v", err)
	}

	body := get(t, s, "/machines/"+id)
	if !strings.Contains(body, "量測環境：clawctl agent") {
		t.Error("畫面沒有顯示工具量測環境")
	}
	if !strings.Contains(body, "/bin/zsh") {
		t.Error("沒有把降級的原因寫出來 —— 一個沒有說明的降級沒辦法被修")
	}
}

// TestNoMarkdownLeaksIntoHTML 只看得到它自己那一台 fixture 走到的分支。
// 一個 fixture 到不了的分支，那支測試永遠是綠的 —— 而它就這樣漏掉了兩處：
//
//	token.html   「撤銷可以讓它**不能再兌換**」（那一頁只在發 token 時出現一次）
//	machine.html 「上面那個版號是**你的**那一份的」（新加的 shadowed 分支）
//
// 後面那一處是把條件突變成否定式之後才浮出來的 —— 也就是說，
// 那支守衛不是抓到它，是**剛好被別的錯誤帶到那一行**。
//
// ⚠ 這支改成直接讀模板原始碼，因為「有沒有漏 markdown」是模板的靜態性質，
// 不是某一次 render 的性質。這樣它的覆蓋率不再等於 fixture 的覆蓋率 ——
// §5.15 那句「一支只塞一列資料的測試，測不出跟『選哪一列』有關的規則」
// 換了一件衣服：一支只 render 一台機器的測試，測不出別的分支寫了什麼。
func TestNoTemplateSourceContainsUnrenderedMarkdown(t *testing.T) {
	// {{/* ... */}} 註解不會 render，而這個 codebase 的註解裡到處都是 **強調**。
	comments := regexp.MustCompile(`(?s)\{\{/\*.*?\*/\}\}`)

	entries, err := templateFS.ReadDir("templates")
	if err != nil {
		t.Fatalf("讀模板目錄：%v", err)
	}
	if len(entries) == 0 {
		t.Fatal("一個模板都沒讀到 —— 這支測試會永遠綠著，什麼都沒檢查")
	}
	for _, e := range entries {
		b, err := templateFS.ReadFile("templates/" + e.Name())
		if err != nil {
			t.Fatalf("讀 %s：%v", e.Name(), err)
		}
		src := comments.ReplaceAllString(string(b), "")
		// ⚠ 只查這兩個記號。`<b>` 在模板原始碼裡是合法 HTML，
		// 反引號在 Go 模板裡也可能是字串字面值 —— 那兩個在這裡查會全是假警報。
		for _, bad := range []string{"**", "]("} {
			if i := strings.Index(src, bad); i >= 0 {
				lo, hi := i-50, i+50
				if lo < 0 {
					lo = 0
				}
				if hi > len(src) {
					hi = len(src)
				}
				t.Errorf("%s 的 render 內容裡有 markdown %q，畫面上會直接看到那幾個符號：\n…%s…",
					e.Name(), bad, src[lo:hi])
			}
		}
	}
}

// 「問它本人拿到的版號」跟「從它旁邊的檔案讀到的版號」不准折成一格。
//
// ⚠ 實測 sampleagent3（instance-20260514-0131，2026-09-03）：openclaw 的
// package.json 說 2026.6.10 —— 全機隊四台裡最新的一份 —— 而它根本起不來：
//
//	openclaw: Node.js v22.19+ is required (current: v20.20.2)
//
// 舊的樣板寫 {{or .VersionReported .VersionPackageJSON}}，於是那一格顯示
// 2026.6.10，看起來比其他三台都新都好。而真相是這台上的 openclaw
// 一次都沒有成功執行過。掃版號那一欄的人會直接跳過它。
//
// 這是 §5.16 的同一個形狀，只是錯在來源而不是路徑：
// 一個內容飽滿、格式正確、數字合理的錯答案。
func TestAVersionReadOffDiskIsNotPresentedAsTheToolAnswering(t *testing.T) {
	s, st := newServer(t)
	now := time.Now().UTC()
	id := enroll(t, st, "sampleagent3", now.Add(-time.Hour))

	payload := `{"name":"openclaw","present":true,"on_path":true,` +
		`"present_evidence":"path","path_source":"login","daemon_reach":"missing",` +
		`"path":"/home/example-user-c/.local/bin/openclaw",` +
		`"realpath":"/home/example-user-c/.local/node24/lib/node_modules/openclaw/openclaw.mjs",` +
		`"version_package_json":"2026.6.10",` +
		`"version_reason":"openclaw: Node.js v22.19+ is required (current: v20.20.2)",` +
		`"version_sources_disagree":false}`
	if _, err := st.DB().Exec(`
INSERT INTO observed_state
  (observation_id, machine_id, measured_at, received_at, kind, subject, payload, source)
VALUES ('diskver-1', ?, ?, ?, 'cli_tool', 'openclaw', ?, 'agent_measurement')`,
		id, now.Format(time.RFC3339), now.Format(time.RFC3339), payload); err != nil {
		t.Fatalf("insert: %v", err)
	}

	body := get(t, s, "/machines/"+id)
	if !strings.Contains(body, "問它本人沒答") {
		t.Error("那一格把「檔案上的版號」講成了「它的版號」。\n" +
			"實測那台上的 openclaw 起不來（Node 太舊），而 package.json 是全機隊最新的 ——\n" +
			"折成一格之後它看起來比其他三台都好。")
	}
	// 數字本身還是要留著 —— 它是真的，只是來源不同。
	if !strings.Contains(body, "2026.6.10") {
		t.Error("連數字都不見了 —— 那個版號是真的，只是來源不是它本人")
	}
	// 而失敗的原文一定要在，那是人接下來唯一能用的東西。
	if !strings.Contains(body, "v22.19") {
		t.Error("沒有顯示它為什麼答不出來 —— 少了那一行，人不知道要去裝 Node")
	}
}

// 版本分佈表要真的出現在總覽上，而且要帶著它自己的限制一起出現。
//
// ⚠⚠ 那句限制不是客套話。這張表只知道「這個機隊裡誰最新」——
// clawctl 沒有任何上游的版本來源。四台一起落後兩個月的時候它會全白，
// 而一個看過這張表、以為自己確認過「都是最新的」的人，
// 會因此**不去**升級。那比不顯示這張表嚴重得多。
func TestTheFleetVersionTableCarriesItsOwnLimit(t *testing.T) {
	s, st := newServer(t)
	now := time.Now().UTC()
	a := enroll(t, st, "samplehub1", now.Add(-time.Hour))
	b := enroll(t, st, "sampleagent4", now.Add(-time.Hour))

	for _, c := range []struct{ id, ver string }{{a, "2.1.258"}, {b, "2.1.178"}} {
		payload := `{"name":"claude","present":true,"on_path":true,` +
			`"present_evidence":"path","path_source":"login",` +
			`"path":"/home/x/.local/bin/claude","version_reported":"` + c.ver + `",` +
			`"version_sources_disagree":false}`
		if _, err := st.DB().Exec(`
INSERT INTO observed_state
  (observation_id, machine_id, measured_at, received_at, kind, subject, payload, source)
VALUES (?, ?, ?, ?, 'cli_tool', 'claude', ?, 'agent_measurement')`,
			"fleetver-"+c.id, c.id, now.Format(time.RFC3339), now.Format(time.RFC3339),
			payload); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	body := get(t, s, "/")
	for _, want := range []string{"2.1.258", "2.1.178", "工具版本"} {
		if !strings.Contains(body, want) {
			t.Errorf("總覽上找不到 %q —— 四台的版號要在同一個畫面上比得出來，"+
				"不然人得開四個單機頁自己拼", want)
		}
	}
	if !strings.Contains(body, "1 個工具有版本差異") {
		t.Error("工具版本表缺少版本差異摘要")
	}
	// ⚠⚠ 這支測試的名字承諾的就是這一句。沒有它，一張全綠的表會被讀成「都是最新的」——
	// 而這個 Hub 沒有任何上游的版本來源，它只知道這個機隊裡誰最新。
	if !strings.Contains(body, operator.SoftwareReportCaveat) {
		t.Errorf("工具版本表沒有帶著它自己的極限：\n%s", body)
	}
}

// 名冊上有、但從來沒回報過工具的機器，在總覽的表上要有一欄。
//
// ⚠ 名冊是分母。欄位若從觀測長出來，那台機器連一欄都不會有 ——
// 而它正是最該被看到的那一台。
func TestAMachineWithNoToolReportsStillHasAColumnOnTheDashboard(t *testing.T) {
	s, st := newServer(t)
	now := time.Now().UTC()
	a := enroll(t, st, "samplehub1", now.Add(-time.Hour))
	enroll(t, st, "sampleagent3", now.Add(-time.Hour)) // 一筆工具觀測都沒有

	payload := `{"name":"claude","present":true,"on_path":true,` +
		`"present_evidence":"path","version_reported":"2.1.258",` +
		`"version_sources_disagree":false}`
	if _, err := st.DB().Exec(`
INSERT INTO observed_state
  (observation_id, machine_id, measured_at, received_at, kind, subject, payload, source)
VALUES ('onlyone', ?, ?, ?, 'cli_tool', 'claude', ?, 'agent_measurement')`,
		a, now.Format(time.RFC3339), now.Format(time.RFC3339), payload); err != nil {
		t.Fatalf("insert: %v", err)
	}

	// ⚠⚠ 一定要切出**那張表**再斷言。
	// 第一版寫的是 strings.Contains(body, "sampleagent3")，而它是假綠的：
	// sampleagent3 在下面的機隊名冊表格上也出現，「沒回報過」在表格下面那段
	// 說明文字裡也出現 —— 於是把欄位改成「從觀測長出來」（那台機器整欄消失）
	// 之後，那兩個斷言還是綠的。§5.12 一模一樣的形狀：
	// 抓到的是頁面上另一處的同一個字串。
	table := sectionOf(t, get(t, s, "/"), "工具版本", "</table>")
	if !strings.Contains(table, "sampleagent3") {
		t.Error("一台沒回報過任何工具的機器，在版本表上沒有欄位 —— " +
			"它從畫面上消失了，而它正是最該被看到的那一台")
	}
	if !strings.Contains(table, "沒回報過") {
		t.Error("那一格沒有講出「我沒問到」——「我沒問到」跟「那台上沒有」" +
			"的下一步不一樣：前者去看 agent，後者去裝東西")
	}
}

// sectionOf 切出畫面上的一段，讓斷言只看那一段。
//
// ⚠⚠ 這個函式存在是因為一支假綠的測試：在整頁 HTML 上做
// strings.Contains，等於讓頁面上任何一處的同一個字串替你的斷言背書。
// 這個 repo 已經被同一件事咬過一次（§5.12：Connect 的位址測試抓的是
// 頁面上另一處的同一個字串，而那一段其實是空的）。
func sectionOf(t *testing.T, body, from, to string) string {
	t.Helper()
	i := strings.Index(body, from)
	if i < 0 {
		t.Fatalf("畫面上找不到 %q，切不出那一段 —— 斷言會變成在整頁上亂抓", from)
	}
	rest := body[i+len(from):]
	j := strings.Index(rest, to)
	if j < 0 {
		t.Fatalf("%q 之後找不到 %q", from, to)
	}
	return rest[:j]
}

// ---------------------------------------------------------------- 兩個 agent

// twoAgents 讓一台機器上有兩個 agent 同時回報，回傳它的 id。
//
// ⚠ 時間全部相對於 now。寫死 2026-09-03 的話這幾支測試明天就會過期 ——
// 而一個因為日期而失效的測試，失效的樣子是「綠燈」。
func twoAgents(t *testing.T, st *store.Store, name string, endedAgo time.Duration) string {
	t.Helper()
	now := time.Now().UTC()
	id := enroll(t, st, name, now.Add(-7*time.Hour))
	// run A 從 6 小時前一直到現在都還在講話。
	// run B 晚兩分鐘起來，在 endedAgo 之前講最後一句。
	runs := []struct {
		started, first, last time.Time
		sentOffset           time.Duration
	}{
		{now.Add(-6 * time.Hour), now.Add(-6 * time.Hour), now.Add(-2 * time.Minute), 0},
		{now.Add(-6*time.Hour + 2*time.Minute), now.Add(-6*time.Hour + 2*time.Minute),
			now.Add(-endedAgo), 7 * time.Second},
	}
	for _, r := range runs {
		step := r.last.Sub(r.first) / 9
		for i := 0; i < 10; i++ {
			recv := r.first.Add(step * time.Duration(i))
			c := model.Checkin{
				SentAt:         recv.Add(r.sentOffset),
				BootID:         "boot-1", // 同一次開機
				AgentStartedAt: r.started,
			}
			if err := st.RecordCheckin(id, c, recv); err != nil {
				t.Fatalf("checkin: %v", err)
			}
		}
	}
	// ⚠⚠ 灌完要確認灌進去的是你以為的東西。
	//
	// machine_checkins 的唯一鍵是 (machine_id, sent_at)，而 sent_at 只存到秒
	// （RFC3339 沒有小數秒）。這裡原本 run B 的偏移寫 500ms，被格式化直接抹掉，
	// 於是 run B 最後一顆心跳 UPSERT 掉 run A 的，run A 看起來 40 分鐘前就閉嘴了。
	// 實測那個 fixture 20 跑 12 敗 —— 而它敗不敗取決於 time.Now() 的小數部分，
	// 也就是說**它有 40% 的機率是綠的**。一個抓不到 bug 的綠燈就是這樣長出來的。
	seeded, err := st.AgentRuns(now)
	if err != nil {
		t.Fatalf("AgentRuns: %v", err)
	}
	if len(seeded) != 2 {
		t.Fatalf("要灌出 2 次啟動，實際 %d 次", len(seeded))
	}
	for _, r := range seeded {
		if r.Checkins != 10 {
			t.Fatalf("run %s 要有 10 顆心跳，實際 %d 顆 —— sent_at 撞掉了，"+
				"fixture 已經不是你以為的情境", r.StartedAt.Format("15:04:05"), r.Checkins)
		}
	}
	return id
}

// 兩個 agent 的時候，畫面要指名道姓，而且要把兩次啟動的時刻都寫出來 ——
// 「有重複 agent」這種話等於要人自己去翻資料庫。
func TestADoubleAgentIsNamedOnTheDashboard(t *testing.T) {
	s, st := newServer(t)
	s.SetTailnetStatus(tailnet.Status{})
	twoAgents(t, st, "sampleagent3", 2*time.Minute)

	sec := sectionOf(t, get(t, s, "/"), `id="double-agents"`, "</div>")
	for _, want := range []string{"sampleagent3", "最多同時 2 個 agent", "現在還在發生", "顆心跳"} {
		if !strings.Contains(sec, want) {
			t.Errorf("這一段要提到 %q，實際：\n%s", want, sec)
		}
	}
	// 兩次啟動都要在畫面上 —— 只講一個，人不知道要殺哪一個。
	// ⚠ 數的字串要**只可能**來自那個 range 迴圈。這一段自己的說明文字裡也有
	// 「它自己說」三個字，拿它去 Count 會多算一個 —— 同一段的解說文案
	// 也是稻草堆的一部分，這是這輪第三次踩到（另兩次見
	// TestAFinishedOverlapIsShownButNotAsOngoing 與 §5.18）。
	if n := strings.Count(sec, "顆心跳（它自己說"); n != 2 {
		t.Errorf("要列出 2 次啟動，實際 %d 個：\n%s", n, sec)
	}
	// ⚠ 機器自報的啟動時刻跟 Hub 收到的時間是兩個時鐘，畫面要標清楚哪個是誰。
	// 實測 sampleagent4 快 79 秒，不標的話那一行看起來像資料壞了。
	for _, want := range []string{"收到 ", "它自己說", "時間範圍採 Hub received_at"} {
		if !strings.Contains(sec, want) {
			t.Errorf("要標明時間是哪個時鐘的（缺 %q）：\n%s", want, sec)
		}
	}
	// 兩邊都還在送心跳 = 紅點。理由見 TestAFinishedOverlapIsShownButNotAsOngoing。
	if !strings.Contains(sec, "dot red") {
		t.Errorf("兩邊都還在講話要是紅點，實際：\n%s", sec)
	}
}

// 重疊已經結束了還是要留在畫面上，但不可以說成「現在還在發生」。
// ⚠ 也不可以說成「已經處理好」—— 兩個 agent 一起死掉也長這樣。
func TestAFinishedOverlapIsShownButNotAsOngoing(t *testing.T) {
	s, st := newServer(t)
	s.SetTailnetStatus(tailnet.Status{})
	twoAgents(t, st, "sampleagent3", time.Hour) // run B 一小時前就閉嘴了

	sec := sectionOf(t, get(t, s, "/"), `id="double-agents"`, "</div>")
	// ⚠⚠ 這裡刻意釘燈的顏色，不是釘那句話。
	// 底下的說明文字自己就寫著「黃點＝重疊已經結束了」，所以
	// strings.Contains(sec, "已經結束") 在偵測器講反話的時候**照樣會過** ——
	// 那段文案自己滿足了斷言。要釘就釘畫面上真的分岔的那個東西（燈的 class）。
	// 這跟「不要從自由文字推論成敗」是同一條規矩，只是換到測試這一側。
	if !strings.Contains(sec, "dot amber") {
		t.Errorf("重疊結束了要是黃點，實際：\n%s", sec)
	}
	if strings.Contains(sec, "dot red") {
		t.Errorf("只剩一個 agent 在講話了，不該是紅點：\n%s", sec)
	}
	if strings.Contains(sec, "現在還在發生") {
		t.Errorf("只剩一個 agent 在講話了，不該說還在發生：\n%s", sec)
	}
}

// ⚠⚠ 沒抓到的時候，畫面不准說「每台都只有一個 agent」。
// 舊版 agent 不送 agent_started_at，那些機器上有幾個是**看不出來**的。
func TestACleanFleetIsNeverToldEveryMachineHasExactlyOneAgent(t *testing.T) {
	s, st := newServer(t)
	s.SetTailnetStatus(tailnet.Status{})
	onlineMachine(t, st, "samplehub1")

	body := get(t, s, "/")
	if strings.Contains(body, `id="double-agents"`) {
		t.Fatalf("只有一個 agent 的機隊不該出現這一段")
	}
	for _, forbidden := range []string{"每台都只有一個", "沒有重複的 agent", "agent 數量正常"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("不准說 %q —— 這個偵測答不出這句話", forbidden)
		}
	}
}

// ---------------------------------------------------------------- unit journal

// journalMachine 造一台有 journal 觀測的機器。
func journalMachine(t *testing.T, st *store.Store, name string, js []model.UnitJournal) string {
	t.Helper()
	now := time.Now().UTC()
	id := enroll(t, st, name, now)
	b := batch(now)
	b.Systemd = []model.Unit{{
		Name: "openclaw-watcher.service", Present: true,
		ActiveState: "active", SubState: "running",
	}}
	b.Journals = js
	if err := st.RecordObservation(id, b, now); err != nil {
		t.Fatalf("record: %v", err)
	}
	return id
}

// 這是 §5.25 那個實例：sampleagent2 的 openclaw-watcher 在 unit 表上整片綠燈
// （active/running、重啟 0 次），而它每分鐘印一次 FAILED。
// 綠燈那幾格還是會在，但現在同一列底下看得到它究竟在說什麼。
func TestAUnitThatIsGreenButScreamingShowsWhatItIsSaying(t *testing.T) {
	s, st := newServer(t)
	id := journalMachine(t, st, "sampleagent2", []model.UnitJournal{{
		Unit: "openclaw-watcher.service", WindowSec: 3600, Lines: 41, Shapes: 1,
		Top: []model.JournalShape{{Count: 41, Example: "machine-mission.md: FAILED"}},
	}})
	body := get(t, s, "/machines/"+id)

	if !strings.Contains(body, "machine-mission.md: FAILED") {
		t.Fatal("unit 說的話沒有出現在畫面上")
	}
	if !strings.Contains(body, "41") {
		t.Fatal("沒有講它說了幾次")
	}
	// ⚠ 綠燈那幾格**必須還在**。這裡不是要把壞消息蓋掉綠燈，
	// 而是要讓「systemd 說它活著」跟「它自己說它失敗」同時被看到。
	if !strings.Contains(body, "active/running") {
		t.Fatal("systemd 的說法被蓋掉了 —— 兩邊都要看得到才對得起「觀測先於控制」")
	}
}

// ⚠⚠ 這一支守的是產品線：畫面不准替使用者下判斷。
// 見 docs/PRODUCT.md 地基二 —— 掃 log 關鍵字判成敗會漏掉最痛的那種錯。
func TestTheJournalIsShownWithoutAVerdict(t *testing.T) {
	s, st := newServer(t)
	id := journalMachine(t, st, "sampleagent2", []model.UnitJournal{{
		Unit: "openclaw-watcher.service", WindowSec: 3600, Lines: 41, Shapes: 1,
		Top: []model.JournalShape{{Count: 41, Example: "machine-mission.md: FAILED"}},
	}})
	body := get(t, s, "/machines/"+id)

	// 畫面看到 FAILED，但不准因此宣布這個 unit 壞了 / 不健康 / 有幾個錯誤。
	for _, verdict := range []string{"unit 異常", "unit 不健康", "服務異常", "個錯誤", "錯誤數"} {
		if strings.Contains(body, verdict) {
			t.Errorf("畫面替使用者下了判斷「%s」—— journal 只提供材料，不提供結論", verdict)
		}
	}
	if strings.Contains(body, "不掃關鍵字") || strings.Contains(body, "由你判斷") {
		t.Error("journal 顯示了實作辯解")
	}
}

// ⚠ 「讀不到」跟「很安靜」在 0 行上長得一模一樣，畫面必須分得出來。
func TestAJournalWeCouldNotReadIsNotShownAsSilence(t *testing.T) {
	s, st := newServer(t)
	id := journalMachine(t, st, "sampleagent3", []model.UnitJournal{{
		Unit: "openclaw-watcher.service", WindowSec: 3600,
		Err: "Failed to add match: Invalid argument",
	}})
	body := get(t, s, "/machines/"+id)

	if !strings.Contains(body, "Journal 讀取失敗") {
		t.Fatal("讀取失敗沒有說出來")
	}
	if strings.Contains(body, "最近一小時 0 行") {
		t.Fatal("把「我們沒在聽」顯示成了「它沒說話」")
	}
}

// 一個安靜的 unit 要說出「安靜不等於健康」——
// 否則一片空白會被讀成好消息，那正是這個專案最想避免的假綠燈。
func TestSilenceIsNotPresentedAsGoodNews(t *testing.T) {
	s, st := newServer(t)
	id := journalMachine(t, st, "sampleagent3", []model.UnitJournal{{
		Unit: "openclaw-watcher.service", WindowSec: 3600, Lines: 0, Shapes: 0,
	}})
	body := get(t, s, "/machines/"+id)
	if !strings.Contains(body, "最近一小時 0 行") {
		t.Fatal("安靜被當成好消息呈現了")
	}
}

// 撞到讀取上限時要講「至少」，不可以讓一個下界看起來像總數。
func TestATruncatedLineCountSaysItIsALowerBound(t *testing.T) {
	s, st := newServer(t)
	id := journalMachine(t, st, "sampleagent4", []model.UnitJournal{{
		Unit: "openclaw-watcher.service", WindowSec: 3600, Lines: 2000, Shapes: 11,
		Truncated: true,
		Top:       []model.JournalShape{{Count: 996, Example: "sendMessage failed"}},
	}})
	body := get(t, s, "/machines/"+id)
	if !strings.Contains(body, "至少") {
		t.Fatal("被截斷的行數沒有標成下界")
	}
	if !strings.Contains(body, "實際更多") {
		t.Fatal("沒有講清楚實際行數比顯示的多")
	}
}

// 完全沒有 journal 觀測（例如那台的 agent 還是舊版）時，
// 不准憑空長出一段假的「沒說話」。
func TestNoJournalObservationRendersNothingRatherThanSilence(t *testing.T) {
	s, st := newServer(t)
	id := journalMachine(t, st, "samplehub1", nil)
	body := get(t, s, "/machines/"+id)
	if strings.Contains(body, "最近一小時 0 行") {
		t.Fatal("沒收到觀測卻顯示成「它沒說話」—— 那是兩件不同的事")
	}
}

// HTML 逃逸：journal 原文是不受控的外部字串。
func TestJournalTextIsEscapedNotInjected(t *testing.T) {
	s, st := newServer(t)
	id := journalMachine(t, st, "samplehub1", []model.UnitJournal{{
		Unit: "openclaw-watcher.service", WindowSec: 3600, Lines: 1, Shapes: 1,
		Top: []model.JournalShape{{Count: 1, Example: `<script>alert(1)</script>`}},
	}})
	body := get(t, s, "/machines/"+id)
	if strings.Contains(body, "<script>alert(1)</script>") {
		t.Fatal("journal 原文沒有被逃逸，可以注入 HTML")
	}
	if !strings.Contains(body, "alert(1)") {
		t.Fatal("逃逸過頭，原文整段不見了")
	}
}

// ---------------------------------------------------------------- 期望

// ⚠⚠ 沒有人宣告過期望時，一片空白會被讀成「都檢查過了」。
// 這一支守的是那句必須說出口的話。
func TestNoExpectationsSaysNothingIsBeingChecked(t *testing.T) {
	s, st := newServer(t)
	id := onlineMachine(t, st, "samplehub1")
	body := get(t, s, "/machines/"+id)

	if !strings.Contains(body, "沒有產出規則") {
		t.Fatal("沒有期望設定，畫面卻沒講 —— 空白會被讀成「都通過了」")
	}
}

// ---------------------------------------------------------------- 事件流

// eventMachine 造一台宣告了事件流期望、而且已經量到的機器。
func eventMachine(t *testing.T, st *store.Store, name string, spec *model.EventSpec, ev model.EventStream) string {
	t.Helper()
	now := time.Now().UTC()
	id := enroll(t, st, name, now)

	path := "/home/ubuntu/.openclaw/workspace/evolution-journal.jsonl"
	st.SetExpectations(&expect.Set{
		Configured: true, Path: "test.json",
		Rules: []model.Expectation{{
			Machine: name, Unit: "openclaw-watcher.service", Artifact: path,
			MaxAgeSeconds: 900,
			Why:           "哨兵自己還活著嗎",
			Events:        spec,
		}},
	})

	b := batch(now)
	b.Systemd = []model.Unit{{
		Name: "openclaw-watcher.service", Present: true,
		ActiveState: "active", SubState: "running",
	}}
	mt := now.Add(-time.Minute)
	b.Artifacts = []model.ArtifactCheck{{
		Unit: "openclaw-watcher.service", Artifact: path,
		Exists: true, ModTime: &mt, Size: 24508023,
	}}
	ev.Unit, ev.Path = "openclaw-watcher.service", path
	b.Events = []model.EventStream{ev}
	if err := st.RecordObservation(id, b, now); err != nil {
		t.Fatalf("record: %v", err)
	}
	return id
}

func evSpec(notOK ...string) *model.EventSpec {
	return &model.EventSpec{TsField: "ts", TypeField: "event", NotOK: notOK, WindowSeconds: 3600}
}

// ⚠⚠ 這是 Ted 要的那格：sampleagent2 的檔案是新的（哨兵活著），
// 但它在喊 baseline_hash_mismatch —— 那件事必須留在畫面上，
// 而且是「現在的狀態」，不是「今天的變化」。
func TestEventStreamShowsTheActiveCondition(t *testing.T) {
	s, st := newServer(t)
	last := time.Now().UTC().Add(-90 * time.Second)
	id := eventMachine(t, st, "sampleagent2", evSpec("baseline_hash_mismatch"),
		model.EventStream{
			Declared: []model.EventSummary{
				{Type: "baseline_hash_mismatch", Count: 59, LastAt: &last},
			},
			Undeclared:      []model.EventSummary{{Type: "watcher_heartbeat", Count: 32, LastAt: &last}},
			UndeclaredTotal: 1,
		})
	body := get(t, s, "/machines/"+id)

	if !strings.Contains(body, "baseline_hash_mismatch") {
		t.Fatal("事件名沒有出現在畫面上")
	}
	if !strings.Contains(body, "59") {
		t.Fatal("次數沒有出現")
	}
	// ⚠ 沒宣告過的事件也要看得見 —— 一種新長出來的失敗事件，
	// 在只看宣告名單的世界裡是完全隱形的。
	if !strings.Contains(body, "watcher_heartbeat") {
		t.Fatal("沒宣告過的事件必須看得見")
	}
	if !strings.Contains(body, "未宣告事件") {
		t.Fatal("要講清楚那些事件沒有人說過算不算壞")
	}
}

// ⚠ 撞到讀取上限時次數是下界。畫面上不准把下界畫成總數。
func TestPartialEventWindowIsDisclosedOnThePage(t *testing.T) {
	s, st := newServer(t)
	now := time.Now().UTC()
	last, from := now.Add(-time.Minute), now.Add(-20*time.Minute)
	id := eventMachine(t, st, "sampleagent2", evSpec("baseline_hash_mismatch"),
		model.EventStream{
			Declared:    []model.EventSummary{{Type: "baseline_hash_mismatch", Count: 40, LastAt: &last}},
			Truncated:   true,
			CoveredFrom: &from, // 比窗口起點（1 小時前）新 → 沒讀滿
		})
	body := get(t, s, "/machines/"+id)
	if !strings.Contains(body, "事件次數為下界") {
		t.Fatal("被截斷的事件次數沒有標成下界")
	}
	if !strings.Contains(body, "下界") {
		t.Fatal("要明講那個數字是下界")
	}
}

// ⚠⚠ 欄位名寫錯 → 每一行都讀不懂 → 這條規則永遠是安靜的。
// 那是最惡毒的假綠燈，畫面必須指出最可能的原因。
func TestMalformedEventLinesAreShownOnThePage(t *testing.T) {
	s, st := newServer(t)
	id := eventMachine(t, st, "sampleagent2", evSpec("baseline_hash_mismatch"),
		model.EventStream{
			Declared:  []model.EventSummary{{Type: "baseline_hash_mismatch"}},
			Malformed: 1440,
		})
	body := get(t, s, "/machines/"+id)
	if !strings.Contains(body, "1440") || !strings.Contains(body, "無法解析") {
		t.Fatal("讀不懂的行數要出現在畫面上")
	}
}

// 宣告了事件流但 agent 還沒回報 → 不准長得像「讀了，很安靜」。
func TestDeclaredEventStreamNotYetMeasuredSaysSo(t *testing.T) {
	s, st := newServer(t)
	now := time.Now().UTC()
	id := enroll(t, st, "sampleagent2", now)
	path := "/home/ubuntu/.openclaw/workspace/evolution-journal.jsonl"
	st.SetExpectations(&expect.Set{
		Configured: true, Path: "test.json",
		Rules: []model.Expectation{{
			Machine: "sampleagent2", Unit: "openclaw-watcher.service", Artifact: path,
			MaxAgeSeconds: 900, Why: "哨兵自己還活著嗎",
			Events: evSpec("baseline_hash_mismatch"),
		}},
	})
	b := batch(now) // 舊版 agent：沒有 Artifacts、也沒有 Events
	if err := st.RecordObservation(id, b, now); err != nil {
		t.Fatalf("record: %v", err)
	}
	body := get(t, s, "/machines/"+id)
	if !strings.Contains(body, "還沒量到") {
		t.Fatal("宣告了但沒量到，必須跟「量了很安靜」分開講")
	}
}

// 事件名來自別人的檔案 —— 畫到頁面上一定要逃逸。
func TestEventNamesAreEscaped(t *testing.T) {
	s, st := newServer(t)
	last := time.Now().UTC()
	id := eventMachine(t, st, "sampleagent2", evSpec("<script>alert(1)</script>"),
		model.EventStream{
			Declared: []model.EventSummary{{Type: "<script>alert(1)</script>", Count: 3, LastAt: &last}},
		})
	body := get(t, s, "/machines/"+id)
	if strings.Contains(body, "<script>alert(1)</script>") {
		t.Fatal("事件名沒有被逃逸，可以注入 HTML")
	}
	if !strings.Contains(body, "alert(1)") {
		t.Fatal("逃逸過頭，事件名整個不見了")
	}
}

// TestTemplatesHaveNoDeadLiveNumbers 掃模板原始碼，不是掃輸出。
//
// ⚠⚠ 這個教訓已經咬過兩次：
//   - 版面上寫死「×41」，而那一列真正的量測是 40
//   - 版面上寫死「到現在喊了 81 天、117,370 次」，那是寫下那一刻的快照，
//     而「到現在」讓它每一天都變得更錯
//
// 一個長得像即時數字的死字，比沒有數字更糟：讀的人會拿它去對帳，
// 然後懷疑的是量測而不是文案。即時數字只能來自量測。
//
// 歷史事實（日期、當時的秒數）寫死沒關係 —— 它們不會變。
func TestTemplatesHaveNoDeadLiveNumbers(t *testing.T) {
	files, err := templateFS.ReadDir("templates")
	if err != nil {
		t.Fatal(err)
	}
	// 這些是實際咬過的字串。發現新的就往下加。
	banned := []string{"117,370", "×41", "81 天、"}
	// ⚠ 只掃**會被畫出來**的部分。{{/* */}} 註解裡正好寫著這些數字
	//（那是在解釋為什麼不准寫死），掃進去會讓這個測試咬自己的說明文。
	comment := regexp.MustCompile(`(?s)\{\{/\*.*?\*/\}\}`)
	for _, f := range files {
		b, err := templateFS.ReadFile("templates/" + f.Name())
		if err != nil {
			t.Fatal(err)
		}
		rendered := comment.ReplaceAllString(string(b), "")
		for _, s := range banned {
			if strings.Contains(rendered, s) {
				t.Errorf("templates/%s 裡有寫死的即時數字 %q —— 即時數字只能來自量測",
					f.Name(), s)
			}
		}
	}
}

func TestTheAgoVocabularySaysTheseExactWordsAndDirections(t *testing.T) {
	ago := funcs["ago"].(func(time.Time) string)
	base := time.Now()
	// 偏移量刻意放在半個單位，避免截斷時因測試執行的幾微秒而跳格。
	tests := []struct {
		name string
		at   time.Time
		want string
	}{
		{name: "零值講從未", at: time.Time{}, want: "從未"},
		{name: "剛剛", at: base.Add(-30500 * time.Millisecond), want: "30 秒前"},
		{name: "一分半前", at: base.Add(-90 * time.Second), want: "1 分鐘前"},
		{name: "十一個半小時前", at: base.Add(-(11*time.Hour + 30*time.Minute + 30*time.Second)), want: "11.5 小時前"},
		{name: "三天前", at: base.Add(-73 * time.Hour), want: "3 天前"},
		{name: "時間戳在未來，講後不講前", at: base.Add(91 * time.Second), want: "1 分鐘後"},
		{name: "未來的秒數", at: base.Add(30500 * time.Millisecond), want: "30 秒後"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ago(tt.at); got != tt.want {
				t.Errorf("ago(%s) = %q，要 %q", tt.at, got, tt.want)
			}
		})
	}
}

// ago 自己會在結尾補「前」或「後」（方向由時間戳決定），所以 template 再補一個方向詞
// 就會印出「前前」／「後後」，而且會把 Hub 對時間方向的判斷蓋掉。
func TestNoTemplateRepeatsTheDirectionWordAfterAgo(t *testing.T) {
	files, err := templateFS.ReadDir("templates")
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		if file.IsDir() || !strings.HasSuffix(file.Name(), ".html") {
			continue
		}
		name := "templates/" + file.Name()
		body, err := templateFS.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(body), "\n") {
			for rest := line; ; {
				ago := strings.Index(rest, "{{ago ")
				if ago < 0 {
					break
				}
				rest = rest[ago+len("{{ago "):]
				end := strings.Index(rest, "}}")
				if end < 0 {
					break
				}
				after := rest[end+len("}}"):]
				if strings.HasPrefix(after, "前") || strings.HasPrefix(after, "後") {
					t.Errorf("%s:%d 的 ago 結束後緊接方向詞：%s", name, i+1, line)
				}
				rest = after
			}
		}
	}
}

func TestTheHeartbeatStripSaysTheseExactWordsAtEachTier(t *testing.T) {
	interval := state.CheckinInterval
	base := time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name       string
		gap        time.Duration
		wantClass  string
		wantStatus string
	}{
		{name: "準時", gap: interval, wantClass: "green", wantStatus: "準時"},
		// 相等的那一筆算輕的那一階，釘的是 > 不是 >=；改成 >= 必須紅。
		{name: "剛好踩在稍晚門檻上還算準時", gap: interval * 3 / 2, wantClass: "green", wantStatus: "準時"},
		{name: "過了門檻一秒就是稍晚", gap: interval*3/2 + time.Second, wantClass: "amber", wantStatus: "稍晚"},
		{name: "稍晚帶的中間", gap: 2 * interval, wantClass: "amber", wantStatus: "稍晚"},
		// 相等的那一筆算輕的那一階，釘的是 > 不是 >=；改成 >= 必須紅。
		{name: "剛好踩在嚴重遲到門檻上還算稍晚", gap: 3 * interval, wantClass: "amber", wantStatus: "稍晚"},
		{name: "過了門檻一秒就是嚴重遲到", gap: 3*interval + time.Second, wantClass: "red", wantStatus: "嚴重遲到"},
		{name: "遠遠遲到", gap: 10 * interval, wantClass: "red", wantStatus: "嚴重遲到"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			blocks := machineDetailStrip([]operator.MachineCheckin{
				{ReceivedAt: base},
				{ReceivedAt: base.Add(tt.gap)},
			}, interval)
			if blocks[0].Class != "grey" {
				t.Errorf("first block class = %q, want %q", blocks[0].Class, "grey")
			}
			if blocks[1].Class != tt.wantClass {
				t.Errorf("second block class = %q, want %q", blocks[1].Class, tt.wantClass)
			}
			if blocks[1].Status != tt.wantStatus {
				t.Errorf("second block status = %q, want %q", blocks[1].Status, tt.wantStatus)
			}
		})
	}
}

func TestMachinePageDoesNotClaimAMacHasNoMemory(t *testing.T) {
	s, st := newServer(t)
	now := time.Now().UTC().Truncate(time.Second)
	id := enroll(t, st, "mac-without-meminfo", now)
	if err := st.RecordObservation(id, model.ObservationBatch{
		SchemaVersion: model.SchemaVersion,
		MeasuredAt:    now,
		Resources: model.Resources{
			DiskFreeBytes: 50 << 30, DiskTotalBytes: 100 << 30, CPUCount: 8,
		},
	}, now); err != nil {
		t.Fatal(err)
	}

	body := get(t, s, "/machines/"+id)
	start := strings.Index(body, "<tr><th>記憶體</th><td>")
	if start < 0 {
		t.Fatal("機器頁找不到記憶體列；畫面若沒有清楚標示未知，operator 可能會把沒量到當成記憶體耗盡")
	}
	end := strings.Index(body[start:], "</tr>")
	if end < 0 {
		t.Fatal("機器頁的記憶體列不完整；畫面若沒有清楚標示未知，operator 可能會把沒量到當成記憶體耗盡")
	}
	memoryRow := body[start : start+end]
	if !strings.Contains(memoryRow, `<span class="dim">未知</span>`) || strings.Contains(memoryRow, "可用 0 B") {
		t.Fatalf("Mac 的記憶體列應顯示未知，實際為 %q；把沒量到畫成 0 B，operator 會誤以為那台機器記憶體耗盡", memoryRow)
	}
}

func TestMachinePageDoesNotClaimAnUnmeasuredMacIsIdle(t *testing.T) {
	s, st := newServer(t)
	now := time.Now().UTC().Truncate(time.Second)
	id := enroll(t, st, "mac-without-loadavg", now)
	if err := st.RecordObservation(id, model.ObservationBatch{
		SchemaVersion: model.SchemaVersion, MeasuredAt: now,
		Resources: model.Resources{
			DiskFreeBytes: 50 << 30, DiskTotalBytes: 100 << 30, CPUCount: 8, Load1m: nil,
		},
	}, now); err != nil {
		t.Fatal(err)
	}

	body := get(t, s, "/machines/"+id)
	start := strings.Index(body, "<tr><th>負載</th><td>")
	if start < 0 {
		t.Fatal("機器頁找不到負載列；把沒量到畫成 0.00，operator 會以為那台 Mac 很閒")
	}
	end := strings.Index(body[start:], "</tr>")
	if end < 0 {
		t.Fatal("機器頁的負載列不完整；把沒量到畫成 0.00，operator 會以為那台 Mac 很閒")
	}
	loadRow := body[start : start+end]
	if !strings.Contains(loadRow, `<span class="dim">未知</span>`) || strings.Contains(loadRow, "0.00") || !strings.Contains(loadRow, "/ 8 核") {
		t.Fatalf("Mac 的負載列應顯示未知並保留核心數，實際為 %q；把沒量到畫成 0.00，operator 會以為那台 Mac 很閒", loadRow)
	}
}

func TestMachinePageStillShowsMemoryAndLoadWhenTheyWereMeasured(t *testing.T) {
	s, st := newServer(t)
	now := time.Now().UTC().Truncate(time.Second)
	id := enroll(t, st, "linux-with-full-resources", now)
	load1m := 0.42
	if err := st.RecordObservation(id, model.ObservationBatch{
		SchemaVersion: model.SchemaVersion,
		MeasuredAt:    now,
		Resources: model.Resources{
			DiskFreeBytes: 50 << 30, DiskTotalBytes: 100 << 30,
			MemTotalBytes: 16 << 30, MemAvailableBytes: 8 << 30,
			CPUCount: 8, Load1m: &load1m,
		},
	}, now); err != nil {
		t.Fatal(err)
	}

	body := get(t, s, "/machines/"+id)
	memoryStart := strings.Index(body, "<tr><th>記憶體</th><td>")
	if memoryStart < 0 {
		t.Fatal("機器頁找不到記憶體列；量到的數字若被畫成「未知」，operator 會以為 Hub 什麼都沒收到，於是跑去查一台其實回報正常的機器")
	}
	memoryEnd := strings.Index(body[memoryStart:], "</tr>")
	if memoryEnd < 0 {
		t.Fatal("機器頁的記憶體列不完整；量到的數字若被畫成「未知」，operator 會以為 Hub 什麼都沒收到，於是跑去查一台其實回報正常的機器")
	}
	memoryRow := body[memoryStart : memoryStart+memoryEnd]
	if !strings.Contains(memoryRow, "可用 8.0 GiB") || !strings.Contains(memoryRow, "共 16.0 GiB") || strings.Contains(memoryRow, "未知") {
		t.Fatalf("已量到的記憶體列應顯示可用與總量，實際為 %q；量到的數字被畫成「未知」，operator 會以為 Hub 什麼都沒收到，於是跑去查一台其實回報正常的機器", memoryRow)
	}

	loadStart := strings.Index(body, "<tr><th>負載</th><td>")
	if loadStart < 0 {
		t.Fatal("機器頁找不到負載列；量到的數字若被畫成「未知」，operator 會以為 Hub 什麼都沒收到，於是跑去查一台其實回報正常的機器")
	}
	loadEnd := strings.Index(body[loadStart:], "</tr>")
	if loadEnd < 0 {
		t.Fatal("機器頁的負載列不完整；量到的數字若被畫成「未知」，operator 會以為 Hub 什麼都沒收到，於是跑去查一台其實回報正常的機器")
	}
	loadRow := body[loadStart : loadStart+loadEnd]
	if !strings.Contains(loadRow, "0.42") || strings.Contains(loadRow, "未知") || !strings.Contains(loadRow, "/ 8 核") {
		t.Fatalf("已量到的負載列應顯示 0.42 並保留 8 核，實際為 %q；量到的數字被畫成「未知」，operator 會以為 Hub 什麼都沒收到，於是跑去查一台其實回報正常的機器", loadRow)
	}
}

func TestMachinePageSeparatesAnUnmeasuredUptimeFromAFreshBoot(t *testing.T) {
	s, st := newServer(t)
	now := time.Now().UTC().Truncate(time.Second)

	unmeasuredID := enroll(t, st, "uptime-unmeasured", now.Add(-time.Hour))
	if err := st.RecordCheckin(unmeasuredID, model.Checkin{
		SchemaVersion: model.SchemaVersion, SentAt: now, UptimeSeconds: nil,
	}, now); err != nil {
		t.Fatalf("寫入沒量到 uptime 的心跳失敗：%v；無法驗證頁面不會讓 operator 以為 Mac 一直在重開", err)
	}
	unmeasuredBody := get(t, s, "/machines/"+unmeasuredID)
	if !strings.Contains(unmeasuredBody, "uptime=unknown") {
		t.Fatalf("沒量到 uptime 的機器頁沒有 uptime=unknown；operator 會失去 Mac 沒量到的事實")
	}
	if strings.Contains(unmeasuredBody, "uptime=0s") {
		t.Fatalf("沒量到 uptime 的機器頁出現 uptime=0s；operator 會以為那台 Mac 每顆心跳都剛開機／一直在重開")
	}

	zero := int64(0)
	freshBootID := enroll(t, st, "uptime-fresh-boot", now.Add(-time.Hour))
	if err := st.RecordCheckin(freshBootID, model.Checkin{
		SchemaVersion: model.SchemaVersion, SentAt: now, UptimeSeconds: &zero,
	}, now); err != nil {
		t.Fatalf("寫入量到 0 uptime 的心跳失敗：%v；無法驗證真正剛開機不會被印成 unknown", err)
	}
	freshBootBody := get(t, s, "/machines/"+freshBootID)
	if !strings.Contains(freshBootBody, "uptime=0s") {
		t.Fatalf("量到 0 uptime 的機器頁沒有 uptime=0s；真正剛開機被印成 unknown，operator 無法分辨量到的 0")
	}
}
