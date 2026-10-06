package web

import (
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/artifact"
	"github.com/teddashh/AI-Intune/internal/deploy"
	"github.com/teddashh/AI-Intune/internal/expect"
	"github.com/teddashh/AI-Intune/internal/model"
	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/rollout"
	"github.com/teddashh/AI-Intune/internal/store"
)

func webDeployment(t *testing.T, st *store.Store, channel string, names ...string) (store.Deployment, []store.Job, map[string]string) {
	return webDeploymentWithBatchSize(t, st, channel, 1, names...)
}

func webDeploymentWithBatchSize(t *testing.T, st *store.Store, channel string, batchSize int, names ...string) (store.Deployment, []store.Job, map[string]string) {
	t.Helper()
	st.SetExpectations(&expect.Set{})
	// Web rollout fixtures may backdate a finished canary by a few hours; make
	// their evidence recorder boundary explicitly older than those deployments.
	if err := st.PublishExpectationsPolicy(time.Now().UTC().Add(-24 * time.Hour)); err != nil {
		t.Fatal(err)
	}
	digest := strings.Repeat("a", 64)
	ids := make(map[string]string, len(names))
	targets := make([]store.NewDeploymentTarget, 0, len(names))
	for i, name := range names {
		id := enroll(t, st, name, time.Now().UTC().Add(-time.Hour))
		measuredAt := time.Now().UTC().Add(-time.Minute)
		if err := st.RecordCheckin(id, checkin(measuredAt), measuredAt); err != nil {
			t.Fatal(err)
		}
		b := batch(measuredAt)
		matchesUnit := true
		b.OpenClaw.Install = &model.OpenClawInstall{
			UnitFound:          true,
			MainPID:            4242,
			ProcessMatchesUnit: &matchesUnit,
			RunningDir:         "/home/test/.local/share/clawctl/openclaw/releases/2026.9.2",
			RunningDirExists:   true,
			RunningDirVersion:  "2026.9.2", NodeVersion: "24.15.0",
			ReleasesDir: "/home/test/.local/share/clawctl/openclaw/releases",
		}
		var err error
		b.WorkloadPolicyToken, err = st.CurrentWorkloadPolicyToken(name)
		if err != nil {
			t.Fatal(err)
		}
		if err := st.RecordObservation(id, b, b.MeasuredAt); err != nil {
			t.Fatal(err)
		}
		if err := st.SetMachineChannel(id, channel); err != nil {
			t.Fatal(err)
		}
		ids[name] = id
		targets = append(targets, store.NewDeploymentTarget{MachineID: id, BatchNo: i/batchSize + 1})
	}
	createChannel := channel
	if createChannel == "stable" {
		// 這些畫面測試需要能顯示舊 stable 帳本，不是要測開單。
		// 先用受支援的 canary 入口建 fixture，再直接標記成歷史列。
		createChannel = "canary"
	}
	for _, id := range ids {
		if err := st.SetMachineChannel(id, createChannel); err != nil {
			t.Fatal(err)
		}
	}
	d, jobs, err := st.CreateDeployment(store.NewDeployment{
		Channel: createChannel, ResourceKind: "openclaw", ResourceID: "openclaw",
		Spec:      `{"kind":"openclaw","version":"2026.9.2","artifact":{"sha256":"` + digest + `"},"unsafe":"<script>"}`,
		BatchSize: batchSize, CreatedBy: "web 測試", Targets: targets,
		Job: store.NewJob{ArtifactDigest: "sha256:" + digest, ExecutionTimeout: 600},
	})
	if err != nil {
		t.Fatal(err)
	}
	if channel == "stable" {
		tx, err := st.DB().Begin()
		if err != nil {
			t.Fatal(err)
		}
		if _, err = tx.Exec(`UPDATE deployments SET channel='stable' WHERE deployment_id=?`, d.DeploymentID); err == nil {
			_, err = tx.Exec(`UPDATE desired_state SET scope_id='stable' WHERE desired_id=?`, d.DesiredID)
		}
		// This helper manufactures a legacy stable ledger row for read-only page
		// tests. Production assignment intentionally cannot move these machines
		// while the just-created canary jobs are nonterminal.
		if err == nil {
			for _, id := range ids {
				if _, err = tx.Exec(`UPDATE machine_registry SET channel='stable' WHERE machine_id=?`, id); err != nil {
					break
				}
			}
		}
		if err == nil {
			err = tx.Commit()
		} else {
			_ = tx.Rollback()
		}
		if err != nil {
			t.Fatal(err)
		}
		d.Channel = "stable"
	}
	return d, jobs, ids
}

func succeedDeploymentJob(t *testing.T, st *store.Store, job store.Job, now time.Time) {
	t.Helper()
	at := now.UTC().Truncate(time.Second)
	if _, err := st.DB().Exec(`UPDATE jobs SET state=?,terminal_at=?,lease_token=NULL,lease_expires_at=NULL WHERE job_id=?`,
		deploy.Succeeded, at.Format(time.RFC3339Nano), job.JobID); err != nil {
		t.Fatal(err)
	}
}

func failDeploymentJob(t *testing.T, st *store.Store, job store.Job, now time.Time) {
	t.Helper()
	if _, err := st.ClaimJob(job.JobID, job.MachineID, now.Add(-time.Minute), time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AdvanceJobByHub(job.JobID, deploy.Timeout, now); err != nil {
		t.Fatal(err)
	}
}

func attachVerifiedDeploymentMaterial(t *testing.T, s *Server, st *store.Store, d store.Deployment) (string, artifact.Sidecar) {
	t.Helper()
	body := []byte("web deployment artifact bytes")
	sum := sha256.Sum256(body)
	sri := sha512.Sum512(body)
	digest := hex.EncodeToString(sum[:])
	dir := t.TempDir()
	record := artifact.Sidecar{
		Name: "openclaw", Version: "2026.9.2", TarballURL: "https://registry.example/openclaw.tgz",
		SHA512Integrity: "sha512-" + base64.StdEncoding.EncodeToString(sri[:]),
		SHA256:          digest, Size: int64(len(body)),
		EnginesNode: ">=24.15.0 <25", FetchedAt: time.Now().UTC().Add(-time.Hour), FetchedBy: "web-test",
	}
	raw, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, digest+".json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, digest+".tgz"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	material, err := artifact.ResolveOpenClawMaterial(dir, record.Version, record.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`UPDATE desired_state SET spec=? WHERE desired_id=?`, material.Spec, d.DesiredID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`UPDATE jobs SET artifact_digest=? WHERE desired_id=?`, material.Digest, d.DesiredID); err != nil {
		t.Fatal(err)
	}
	s.SetArtifactsDir(dir)
	return dir, record
}

func TestDeploymentPagePutsStuckFirstAndFinishedDoesNotWashItGreen(t *testing.T) {
	s, st := newServer(t)
	d, jobs, _ := webDeployment(t, st, "canary", "samplehub1")
	failDeploymentJob(t, st, jobs[0], time.Now().UTC())
	if changed, err := st.SetDeploymentState(d.DeploymentID, store.DeploymentRunning, store.DeploymentPaused, time.Now().UTC()); err != nil || !changed {
		t.Fatalf("pause deployment: changed=%v err=%v", changed, err)
	}
	if finished, opened, err := st.ContinueDeployment(d.DeploymentID, time.Now().UTC()); err != nil ||
		finished.State != store.DeploymentFinished || len(opened) != 0 {
		t.Fatalf("finish failed deployment through Continue: deployment=%+v opened=%+v err=%v", finished, opened, err)
	}

	body := get(t, s, "/deployments/"+d.DeploymentID)
	stuckAt, statusAt := strings.Index(body, `id="stuck"`), strings.Index(body, `id="deployment-actions"`)
	if stuckAt < 0 || statusAt < 0 || stuckAt >= statusAt {
		t.Fatalf("Stuck 沒排在狀態句之前：stuck=%d status=%d", stuckAt, statusAt)
	}
	for _, want := range []string{"終態未成功", "samplehub1", "failed", "finished，1 台 stuck"} {
		if !strings.Contains(body, want) {
			t.Errorf("deployment 詳細頁缺少 %q", want)
		}
	}
	if strings.Contains(body, "失敗終態") {
		t.Error("deployment 詳細頁不得把終態未成功的機器標成「失敗終態」")
	}
	if strings.Contains(body, `<script>`) {
		t.Fatal("desired spec 沒有被 html/template 逃逸")
	}
	for _, raw := range []string{`&#34;unsafe&#34;`, "web 測試"} {
		if strings.Contains(body, raw) {
			t.Fatalf("safe deployment detail disclosed raw Store field %q", raw)
		}
	}
	list := get(t, s, "/deployments")
	for _, raw := range []string{`&#34;unsafe&#34;`, "web 測試"} {
		if strings.Contains(list, raw) {
			t.Fatalf("safe deployment list disclosed raw Store field %q", raw)
		}
	}
	if !strings.Contains(body, "finished 之後 canary 集合的沉默失敗") || !strings.Contains(body, ">沒有<") {
		t.Fatal("finished canary 的真 0 沒講出來")
	}
}

func TestFinishedCanaryDeploymentPageShowsSilentFailuresAfterFinish(t *testing.T) {
	s, st := newServer(t)
	d, jobs, ids := webDeployment(t, st, "canary", "samplehub1")
	finished := time.Now().UTC().Add(-time.Hour)
	if _, err := st.DB().Exec(`UPDATE jobs SET state=?, terminal_at=? WHERE job_id=?`, deploy.Succeeded, finished.Format(time.RFC3339), jobs[0].JobID); err != nil {
		t.Fatal(err)
	}
	if changed, err := st.SetDeploymentState(d.DeploymentID, store.DeploymentRunning, store.DeploymentFinished, finished); err != nil || !changed {
		t.Fatalf("finish changed=%v err=%v", changed, err)
	}
	if recorded, err := st.RecordCanarySilentFailure(ids["samplehub1"], "第 10 小時沒有合格完成事件", finished.Add(30*time.Minute)); err != nil || !recorded {
		t.Fatalf("recorded=%v err=%v", recorded, err)
	}
	body := get(t, s, "/deployments/"+d.DeploymentID)
	for _, want := range []string{"finished 之後 canary 集合的沉默失敗", "samplehub1", "第 10 小時沒有合格完成事件"} {
		if !strings.Contains(body, want) {
			t.Errorf("canary deployment 頁缺少 %q", want)
		}
	}
}

func TestFinishedCanaryRetryPageShowsSilentFailureFromPriorSuccessfulTarget(t *testing.T) {
	s, st := newServer(t)
	root, firstBatch, ids := webDeployment(t, st, "canary", "samplehub1", "sampleagent2")
	now := time.Now().UTC()
	rootFinished := now.Add(-2 * time.Hour)
	if _, err := st.DB().Exec(`UPDATE jobs SET state=?, terminal_at=? WHERE job_id=?`, deploy.Succeeded, rootFinished.Format(time.RFC3339), firstBatch[0].JobID); err != nil {
		t.Fatal(err)
	}
	secondBatch, err := st.OpenDeploymentBatch(root.DeploymentID, 2, rootFinished.Add(-time.Minute))
	if err != nil || len(secondBatch) != 1 {
		t.Fatalf("open root batch 2 jobs=%+v err=%v", secondBatch, err)
	}
	if _, err := st.DB().Exec(`UPDATE jobs SET state=?, terminal_at=? WHERE job_id=?`, deploy.Failed, rootFinished.Format(time.RFC3339), secondBatch[0].JobID); err != nil {
		t.Fatal(err)
	}
	if changed, err := st.SetDeploymentState(root.DeploymentID, store.DeploymentRunning, store.DeploymentPaused, rootFinished); err != nil || !changed {
		t.Fatalf("pause root changed=%v err=%v", changed, err)
	}
	if finished, opened, err := st.ContinueDeployment(root.DeploymentID, rootFinished); err != nil ||
		finished.State != store.DeploymentFinished || len(opened) != 0 {
		t.Fatalf("finish root through Continue: deployment=%+v opened=%+v err=%v", finished, opened, err)
	}

	retry, retryJobs, err := st.CreateDeployment(store.NewDeployment{
		Channel: "canary", ResourceKind: "openclaw", ResourceID: "openclaw",
		Spec: root.Spec, BatchSize: 1, CreatedBy: "web retry 測試", RetryOf: root.DeploymentID,
		Targets: []store.NewDeploymentTarget{{MachineID: ids["sampleagent2"], BatchNo: 1}},
		Job:     store.NewJob{ArtifactDigest: "sha256:" + strings.Repeat("a", 64), ExecutionTimeout: 600},
	})
	if err != nil || len(retryJobs) != 1 {
		t.Fatalf("create retry jobs=%+v err=%v", retryJobs, err)
	}
	retryFinished := now.Add(-time.Hour)
	if _, err := st.DB().Exec(`UPDATE jobs SET state=?, terminal_at=? WHERE job_id=?`, deploy.Succeeded, retryFinished.Format(time.RFC3339), retryJobs[0].JobID); err != nil {
		t.Fatal(err)
	}
	if changed, err := st.SetDeploymentState(retry.DeploymentID, store.DeploymentRunning, store.DeploymentFinished, retryFinished); err != nil || !changed {
		t.Fatalf("finish retry changed=%v err=%v", changed, err)
	}
	reason := "samplehub1 在 retry 完成後沒有合格事件"
	if recorded, err := st.RecordCanarySilentFailure(ids["samplehub1"], reason, retryFinished.Add(30*time.Minute)); err != nil || !recorded {
		t.Fatalf("recorded=%v err=%v", recorded, err)
	}

	body := get(t, s, "/deployments/"+retry.DeploymentID)
	start := strings.Index(body, "finished 之後 canary 集合的沉默失敗")
	if start < 0 {
		t.Fatal("retry 詳細頁缺少 canary 沉默失敗區塊")
	}
	end := strings.Index(body[start:], "<h2>計數</h2>")
	if end < 0 {
		t.Fatal("找不到 canary 沉默失敗區塊結尾")
	}
	panel := body[start : start+end]
	if !strings.Contains(panel, "samplehub1") || !strings.Contains(panel, reason) {
		t.Fatalf("retry 詳細頁沒有顯示 ancestry 中先前成功 target 的失敗：%s", panel)
	}
	if strings.Contains(panel, `class="empty">沒有</p>`) {
		t.Fatalf("retry 詳細頁有失敗卻說真 0：%s", panel)
	}
}

func TestDeploymentContinueIsPOSTConfirmedAuditedAndDoesNotRetryStuck(t *testing.T) {
	s, st := newServer(t)
	d, jobs, _ := webDeployment(t, st, "canary", "bad", "next")
	artifactDir, record := attachVerifiedDeploymentMaterial(t, s, st, d)
	failDeploymentJob(t, st, jobs[0], time.Now().UTC())
	if changed, err := st.SetDeploymentState(d.DeploymentID, store.DeploymentRunning, store.DeploymentPaused, time.Now().UTC()); err != nil || !changed {
		t.Fatalf("pause deployment: changed=%v err=%v", changed, err)
	}

	if rec := doGet(t, s, "/deployments/"+d.DeploymentID+"/continue"); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET continue = %d，預期 405", rec.Code)
	}
	if rec := doGet(t, s, "/deployments/"+d.DeploymentID+"/continue-preview"); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET continue preview = %d，預期 405", rec.Code)
	}
	pausedPage := get(t, s, "/deployments/"+d.DeploymentID)
	for _, want := range []string{
		"paused", "批次 1 失敗即停；後續批次沒有開始（沒有 auto-continue）",
		"plain Continue 拒絕失敗批次", "skip failed batch",
		`action="/deployments/` + d.DeploymentID + `/skip-failed-batch-preview"`,
		`action="/deployments/` + d.DeploymentID + `/retry-preview"`,
	} {
		if !strings.Contains(pausedPage, want) {
			t.Errorf("paused deployment 頁缺少 %q", want)
		}
	}
	if strings.Contains(pausedPage, `action="/deployments/`+d.DeploymentID+`/continue-preview"`) {
		t.Fatal("failed batch still offered plain Continue")
	}
	if strings.Contains(pausedPage, "clawctl-hub deployment") || strings.Contains(pausedPage, `name="confirm_channel"`) {
		t.Fatal("deployment detail exposed CLI-only instructions or skipped the review step")
	}
	emptyReason := postForm(t, s, "/deployments/"+d.DeploymentID+"/skip-failed-batch-preview", url.Values{"reason": {""}})
	if emptyReason.Code != http.StatusBadRequest || !strings.Contains(emptyReason.Body.String(), "必須留下理由") {
		t.Fatalf("empty skip reason = %d %s", emptyReason.Code, emptyReason.Body.String())
	}

	continuePreview := postForm(t, s, "/deployments/"+d.DeploymentID+"/continue-preview", url.Values{"reason": {"review next batch"}})
	if continuePreview.Code != http.StatusOK || !strings.Contains(continuePreview.Body.String(), "plain Continue 拒絕失敗批次") ||
		strings.Contains(continuePreview.Body.String(), `name="confirm_channel"`) {
		t.Fatalf("continue preview did not refuse the failed batch: %d %s", continuePreview.Code, continuePreview.Body.String())
	}

	preview := postForm(t, s, "/deployments/"+d.DeploymentID+"/skip-failed-batch-preview", url.Values{"reason": {"review next batch"}})
	if preview.Code != http.StatusOK || preview.Header().Get("Cache-Control") != "no-store" ||
		!strings.Contains(preview.Body.String(), "skip failed batch") ||
		!strings.Contains(preview.Body.String(), `name="confirm_channel"`) {
		t.Fatalf("skip preview=%d cache=%q: %s", preview.Code, preview.Header().Get("Cache-Control"), preview.Body.String())
	}
	applyForm := url.Values{
		"preview_digest":            {hiddenFormValue(t, preview.Body.String(), "preview_digest")},
		"expected_control_revision": {hiddenFormValue(t, preview.Body.String(), "expected_control_revision")},
		"expected_opened_batch":     {hiddenFormValue(t, preview.Body.String(), "expected_opened_batch")},
		"reason":                    {hiddenFormValue(t, preview.Body.String(), "reason")},
		"idempotency_key":           {hiddenFormValue(t, preview.Body.String(), "idempotency_key")},
		"confirm_channel":           {"stable"},
	}
	wrong := postForm(t, s, "/deployments/"+d.DeploymentID+"/skip-failed-batch", applyForm)
	if wrong.Code != http.StatusBadRequest {
		t.Fatalf("確認字打錯 = %d，預期 400：%s", wrong.Code, wrong.Body.String())
	}
	wrongNav := renderedSubNavigation(t, wrong.Body.String(), "部署詳細資料子選單")
	if !strings.Contains(wrongNav, `aria-current="location">動作</a>`) ||
		!strings.Contains(wrongNav, `/deployments/`+d.DeploymentID+`?section=deployment-overview#deployment-overview`) {
		t.Fatalf("deployment confirmation error lost detail action navigation: %s", wrongNav)
	}
	if got, _ := st.Deployment(d.DeploymentID); got.State != store.DeploymentPaused {
		t.Fatalf("確認打錯仍改了 state：%s", got.State)
	}

	// A rejected typed confirmation owns its key. Reloading the preview mints a
	// new request identity for the corrected operator decision.
	preview = postForm(t, s, "/deployments/"+d.DeploymentID+"/skip-failed-batch-preview", url.Values{"reason": {"review next batch"}})
	if preview.Code != http.StatusOK {
		t.Fatalf("fresh skip preview=%d: %s", preview.Code, preview.Body.String())
	}
	applyForm = url.Values{
		"preview_digest":            {hiddenFormValue(t, preview.Body.String(), "preview_digest")},
		"expected_control_revision": {hiddenFormValue(t, preview.Body.String(), "expected_control_revision")},
		"expected_opened_batch":     {hiddenFormValue(t, preview.Body.String(), "expected_opened_batch")},
		"reason":                    {hiddenFormValue(t, preview.Body.String(), "reason")},
		"idempotency_key":           {hiddenFormValue(t, preview.Body.String(), "idempotency_key")},
		"confirm_channel":           {"canary"},
	}
	ok := postForm(t, s, "/deployments/"+d.DeploymentID+"/skip-failed-batch", applyForm)
	if ok.Code != http.StatusOK || ok.Header().Get("Cache-Control") != "no-store" ||
		!strings.Contains(ok.Body.String(), "已 skip failed batch") {
		t.Fatalf("skip = %d cache=%q body=%s", ok.Code, ok.Header().Get("Cache-Control"), ok.Body.String())
	}
	view, err := st.DeploymentView(d.DeploymentID, time.Now().UTC())
	if err != nil || view.State != store.DeploymentRunning || view.Stuck != 1 || len(view.Counts) != 2 {
		t.Fatalf("continue 後 view=%+v err=%v", view, err)
	}
	if view.Targets[0].JobID != jobs[0].JobID {
		t.Fatal("Continue next batch 重開了 stuck 的 job")
	}
	if err := os.Remove(filepath.Join(artifactDir, record.SHA256+".tgz")); err != nil {
		t.Fatal(err)
	}
	replayed := postForm(t, s, "/deployments/"+d.DeploymentID+"/skip-failed-batch", applyForm)
	if replayed.Code != http.StatusOK || !strings.Contains(replayed.Body.String(), "replay") {
		t.Fatalf("continue replay=%d: %s", replayed.Code, replayed.Body.String())
	}
	afterReplay, err := st.DeploymentView(d.DeploymentID, time.Now().UTC())
	if err != nil || len(afterReplay.Counts) != len(view.Counts) || afterReplay.OpenedBatch != view.OpenedBatch {
		t.Fatalf("continue replay reopened work: before=%+v after=%+v err=%v", view.Counts, afterReplay.Counts, err)
	}
	entries, err := st.Audit("", 20)
	foundAudit := false
	for _, entry := range entries {
		if entry.Action == store.AuditDeploymentSkipFailedBatch && entry.OK && entry.Subject == d.DeploymentID {
			foundAudit = true
		}
	}
	if err != nil || !foundAudit {
		t.Fatalf("成功 continue 的 audit 不對：%+v err=%v", entries, err)
	}
	events, _ := st.HubEventsBetween(d.CreatedAt, time.Now().UTC().Add(time.Second))
	found := false
	for _, event := range events {
		if event.Kind == store.HubDeploymentSkippedFailedBatch && strings.Contains(event.Detail, "review next batch") {
			found = true
		}
	}
	if !found {
		t.Fatalf("沒有帶理由的 deployment_skip_failed_batch Hub event：%+v", events)
	}
}

func TestDeploymentRestartHintOnlyCountsStartedKindInsideWindow(t *testing.T) {
	s, st := newServer(t)
	d, _, _ := webDeployment(t, st, "stable", "sampleagent3")
	_ = st.RecordHubEvent(store.HubStarted, "窗口之前", d.CreatedAt.Add(-time.Second))
	_ = st.RecordHubEvent(store.JobTimeout, "detail 寫 started 也不算", d.CreatedAt)
	_ = st.RecordHubEvent(store.HubStarted, "窗口裡", d.CreatedAt)
	body := get(t, s, "/deployments/"+d.DeploymentID)
	if !strings.Contains(body, "Hub 這段期間重啟過 1 次；見總覽的 Hub 日誌") || strings.Contains(body, "重啟過 2 次") {
		t.Fatalf("Hub 重啟提示沒有只數窗口內 kind=started：%s", body)
	}
}

func TestDeploymentsListHasTrueZeroAndUnfinishedFirst(t *testing.T) {
	s, st := newServer(t)
	if body := get(t, s, "/deployments"); !strings.Contains(body, "還沒有任何部署") {
		t.Fatal("0 deployments 沒有講出真 0")
	}
	finished, finishedJobs, ids := webDeployment(t, st, "canary", "old")
	if _, err := st.DB().Exec(`UPDATE jobs SET state=?, terminal_at=? WHERE job_id=?`,
		deploy.Succeeded, time.Now().UTC().Format(time.RFC3339), finishedJobs[0].JobID); err != nil {
		t.Fatal(err)
	}
	_, _ = st.SetDeploymentState(finished.DeploymentID, store.DeploymentRunning, store.DeploymentFinished, time.Now().UTC())
	if err := st.SetMachineChannel(ids["old"], ""); err != nil {
		t.Fatal(err)
	}
	running, _, _ := webDeployment(t, st, "stable", "new")
	body := get(t, s, "/deployments")
	if strings.Index(body, running.DeploymentID[:8]) > strings.Index(body, finished.DeploymentID[:8]) {
		t.Fatal("未 finished 的 deployment 沒排在 finished 前面")
	}
}

func TestDeploymentPageShowsNotOpenedAndExplainsEveryExclusion(t *testing.T) {
	s, st := newServer(t)
	var targets []store.NewDeploymentTarget
	for i, item := range []struct {
		name, excluded string
	}{
		{"opened", ""}, {"later", ""}, {"conflict", "conflict"},
		{"missing", "missing_package"}, {"unknown", "unknown_node"},
	} {
		id := enroll(t, st, item.name, time.Now().UTC())
		if _, err := st.DB().Exec(`UPDATE machine_registry SET channel='canary' WHERE machine_id=?`, id); err != nil {
			t.Fatal(err)
		}
		batchNo := i + 1
		if item.excluded != "" {
			batchNo = 0
		}
		targets = append(targets, store.NewDeploymentTarget{MachineID: id, BatchNo: batchNo, ExcludedReason: item.excluded})
	}
	d, _, err := st.CreateDeployment(store.NewDeployment{
		Channel: "canary", ResourceKind: "openclaw", ResourceID: "openclaw",
		Spec: `{"kind":"openclaw","version":"2026.9.2"}`, BatchSize: 1,
		CreatedBy: "test", Targets: targets, Job: store.NewJob{ExecutionTimeout: 600},
	})
	if err != nil {
		t.Fatal(err)
	}
	body := get(t, s, "/deployments/"+d.DeploymentID)
	for _, want := range []string{
		"not_opened", "conflict", "避免兩張單同時改它", "missing_package",
		"Node 版本不符合 artifact", "unknown_node", "Node 版本未知",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("deployment 逐台或排除清單缺少 %q", want)
		}
	}
}

func TestPhaseFourNavigationAndMachineChannelFieldAreVisible(t *testing.T) {
	s, st := newServer(t)
	id := onlineMachine(t, st, "samplehub1")
	if err := st.SetMachineChannel(id, "canary"); err != nil {
		t.Fatal(err)
	}
	body := get(t, s, "/machines/"+id)
	for _, want := range []string{`href="/apps"`, `href="/jobs"`, "channel：canary", `/machines/` + id + `/channel`, `name="expected_revision" value="1"`, "輸入 samplehub1"} {
		if !strings.Contains(body, want) {
			t.Errorf("Phase 4 導覽或 channel 欄缺少 %q", want)
		}
	}
	if strings.Index(body, `id="action-channel"`) > strings.Index(body, `id="action-connect"`) {
		t.Fatal("channel 表單沒有放在 Connect BAT 之上")
	}
}

func TestDeploymentHeadlineFourSituationsAndCapsMachineNames(t *testing.T) {
	base := store.DeploymentView{Deployment: store.Deployment{DeploymentID: "1234567890", Channel: "canary", State: store.DeploymentRunning}, Counts: map[deploy.JobState]int{deploy.Succeeded: 3, deploy.Running: 1}}
	cases := []struct {
		name string
		in   []store.DeploymentView
		want string
	}{
		{"none", nil, "沒有部署在跑。"},
		{"running", []store.DeploymentView{base}, "canary 部署 12345678 在跑：3 succeeded / 1 running。"},
		{"paused", []store.DeploymentView{{Deployment: store.Deployment{DeploymentID: "abcdefghi", Channel: "stable", State: store.DeploymentPaused}, Stuck: 1, Targets: []store.DeploymentTarget{{DisplayName: "samplehub1", JobState: deploy.Rejected, StuckKind: "terminal_failure"}}}}, "部署卡住 —— stable abcdefgh：samplehub1 被拒絕，機器沒有改動（rejected）。"},
		{"running-stuck", []store.DeploymentView{{Deployment: store.Deployment{DeploymentID: "fedcba987", Channel: "canary", State: store.DeploymentRunning}, Stuck: 1, Targets: []store.DeploymentTarget{{DisplayName: "sampleagent4", JobState: deploy.Running, StuckKind: "no_event"}}}}, "部署卡住 —— canary fedcba98：sampleagent4 15 分鐘沒事件（running）。"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := deploymentHeadline(tc.in); got != tc.want {
				t.Fatalf("headline=%q，預期 %q", got, tc.want)
			}
		})
	}
	many := base
	many.State, many.Stuck = store.DeploymentPaused, 4
	for _, name := range []string{"a", "b", "c", "d"} {
		many.Targets = append(many.Targets, store.DeploymentTarget{DisplayName: name, JobState: deploy.Failed, StuckKind: "terminal_failure"})
	}
	if got := deploymentHeadline([]store.DeploymentView{many}); !strings.Contains(got, "a 失敗") || !strings.Contains(got, "+1 台") || strings.Contains(got, "d 失敗") {
		t.Fatalf("stuck 名單沒有最多三台：%q", got)
	}
}

func TestDeploymentHeadlineDistinguishesTerminalOutcomes(t *testing.T) {
	cases := []struct {
		state deploy.JobState
		want  string
	}{
		{deploy.Failed, "部署卡住 —— canary abcdefgh：samplehub1 失敗（failed）。"},
		{deploy.Rejected, "部署卡住 —— canary abcdefgh：samplehub1 被拒絕，機器沒有改動（rejected）。"},
		{deploy.LeaseExpired, "部署卡住 —— canary abcdefgh：samplehub1 代理程式沒有回報，Hub 收了這張單（lease_expired）。"},
		{deploy.ManualIntervention, "部署卡住 —— canary abcdefgh：samplehub1 需要人介入，沒有回退證據（manual_intervention）。"},
	}
	seen := make(map[string]bool, len(cases))
	for _, tc := range cases {
		view := store.DeploymentView{
			Deployment: store.Deployment{DeploymentID: "abcdefghi", Channel: "canary", State: store.DeploymentPaused},
			Stuck:      1,
			Targets:    []store.DeploymentTarget{{DisplayName: "samplehub1", JobState: tc.state, StuckKind: "terminal_failure"}},
		}
		got := deploymentHeadline([]store.DeploymentView{view})
		if got != tc.want {
			t.Errorf("state %s headline=%q，預期 %q", tc.state, got, tc.want)
		}
		if tc.state != deploy.Failed && strings.Contains(got, "失敗") {
			t.Errorf("state %s headline 不得包含「失敗」：%q", tc.state, got)
		}
		if seen[got] {
			t.Errorf("state %s 與其他終態共用同一句話：%q", tc.state, got)
		}
		seen[got] = true
	}
}

func TestDeploymentPageDistinguishesTerminalOutcomesAndKeepsRawTokens(t *testing.T) {
	s, st := newServer(t)
	d, jobs, ids := webDeploymentWithBatchSize(t, st, "canary", 2, "manual-machine", "rejected-machine")
	if len(jobs) != 2 {
		t.Fatalf("第一批 jobs=%d，預期 2", len(jobs))
	}
	jobsByMachineID := make(map[string]store.Job, len(jobs))
	for _, job := range jobs {
		jobsByMachineID[job.MachineID] = job
	}
	manual, ok := jobsByMachineID[ids["manual-machine"]]
	if !ok {
		t.Fatalf("第一批 jobs 找不到 manual-machine（machine_id=%q）", ids["manual-machine"])
	}
	rejected, ok := jobsByMachineID[ids["rejected-machine"]]
	if !ok {
		t.Fatalf("第一批 jobs 找不到 rejected-machine（machine_id=%q）", ids["rejected-machine"])
	}
	if _, err := st.DB().Exec(`UPDATE jobs SET irreversible=1 WHERE job_id=?`, manual.JobID); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := st.ClaimJob(manual.JobID, manual.MachineID, now.Add(-time.Minute), time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AdvanceJobByHub(manual.JobID, deploy.Timeout, now); err != nil {
		t.Fatal(err)
	}
	token, err := st.ClaimJob(rejected.JobID, rejected.MachineID, now.Add(-time.Minute), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AdvanceJobByAgent(rejected.JobID, rejected.MachineID, token, deploy.Reject, now); err != nil {
		t.Fatal(err)
	}
	if changed, err := st.SetDeploymentState(d.DeploymentID, store.DeploymentRunning, store.DeploymentPaused, now); err != nil || !changed {
		t.Fatalf("pause deployment: changed=%v err=%v", changed, err)
	}

	body := get(t, s, "/deployments/"+d.DeploymentID)
	if !strings.Contains(body, "終態未成功（2 台）") {
		t.Error("部署頁缺少「終態未成功（2 台）」小標題")
	}
	// ⚠ rejected 代表機器沒有改動，不能把整個終態桶說成失敗。
	if strings.Contains(body, "失敗終態") {
		t.Error("部署頁不得把含 rejected 的終態桶標成「失敗終態」")
	}
	for _, want := range []string{
		"將建立新的 deployment",
		"為 2 台終態未成功的機器重新開單",
		"不會沿用原本那張單的進度，也不會判斷機器停在哪一步",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("retry 邀請句缺少 %q", want)
		}
	}
	if strings.Contains(body, "不會讀取機器現況") {
		t.Error("retry 邀請句不得聲稱不會讀取機器現況")
	}
	if strings.Contains(body, "重做") {
		t.Error("retry 邀請句不得使用「重做」")
	}
	for _, want := range []string{
		"manual-machine</a>：需要人介入，沒有回退證據（<span class=\"mono\">manual_intervention</span>",
		"rejected-machine</a>：被拒絕，機器沒有改動（<span class=\"mono\">rejected</span>",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("部署頁缺少終態文案或原始 token %q", want)
		}
	}
}

func TestBoundaryPausedDeploymentShowsItsRealReasonInsteadOfClaimingBatchFailure(t *testing.T) {
	s, st := newServer(t)
	d, _, _ := webDeployment(t, st, "canary", "first", "second")
	reason := "promote 鎖著：canary 尚未滿一個完整工作天"
	changed, err := st.PauseDeploymentAtBoundary(
		d.DeploymentID, 1, store.DeploymentPausePromoteLocked, reason, time.Now().UTC())
	if err != nil || !changed {
		t.Fatalf("boundary pause changed=%v err=%v", changed, err)
	}

	detail := get(t, s, "/deployments/"+d.DeploymentID)
	for _, want := range []string{"安全閘門", store.DeploymentPausePromoteLocked, "Continue next batch"} {
		if !strings.Contains(detail, want) {
			t.Errorf("boundary-paused deployment detail missing %q", want)
		}
	}
	if strings.Contains(detail, reason) {
		t.Fatal("safe deployment detail disclosed the raw boundary reason")
	}
	if strings.Contains(detail, "批次 1 失敗即停") {
		t.Fatal("boundary pause was mislabeled as a failed job batch")
	}

	dashboard := get(t, s, "/")
	if !strings.Contains(dashboard, "安全閘門") || !strings.Contains(dashboard, reason) ||
		strings.Contains(dashboard, "部署已暫停，但帳本沒有列出 stuck 機器") {
		t.Fatalf("dashboard did not expose boundary reason truthfully: %s", dashboard)
	}
	list := get(t, s, "/deployments")
	if !strings.Contains(list, store.DeploymentPausePromoteLocked) || strings.Contains(list, reason) {
		t.Fatalf("safe deployments list did not preserve bounded kind or disclosed raw reason: %s", list)
	}
}

func TestMachinePagesScrubPersistedBoundaryAndHubEventText(t *testing.T) {
	s, st := newServer(t)
	d, _, _ := webDeployment(t, st, "canary", "first", "second")
	reason := "promote 鎖著：bad\u202e\a 現在失聯"
	safeReason := "promote 鎖著：bad�� 現在失聯"
	changed, err := st.PauseDeploymentAtBoundary(
		d.DeploymentID, 1, store.DeploymentPausePromoteLocked, reason, time.Now().UTC())
	if err != nil || !changed {
		t.Fatalf("boundary pause changed=%v err=%v", changed, err)
	}

	for _, path := range []string{"/", "/machines"} {
		page := get(t, s, path)
		// The reason appears once in the deployment headline and once in the
		// persisted Hub event. Both paths must scrub presentation controls.
		if !strings.Contains(page, "安全閘門") || strings.Count(page, safeReason) < 2 ||
			strings.Contains(page, "部署已暫停，但帳本沒有列出 stuck 機器") ||
			strings.Contains(page, "\u202e") || strings.Contains(page, "\a") {
			t.Fatalf("GET %s did not safely expose headline and Hub-event boundary truth: %s", path, page)
		}
	}
}

func TestUpdatesShowsFetchedArtifactsAndMissingPackagePreview(t *testing.T) {
	s, st := newServer(t)
	dir := filepath.Join(t.TempDir(), "artifacts")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	tarball := []byte("updates catalog fixture")
	digest := sha256.Sum256(tarball)
	integrity := sha512.Sum512(tarball)
	record := artifact.Sidecar{
		Name: "openclaw", Version: "2026.9.2",
		TarballURL:      "https://registry.npmjs.org/@openclaw/openclaw/-/openclaw-2026.9.2.tgz",
		SHA512Integrity: "sha512-" + base64.StdEncoding.EncodeToString(integrity[:]),
		SHA256:          hex.EncodeToString(digest[:]), Size: int64(len(tarball)),
		EnginesNode: ">=22.22.3 <23", FetchedAt: time.Now().UTC(), FetchedBy: "web-test",
	}
	b, _ := json.Marshal(record)
	if err := os.WriteFile(filepath.Join(dir, record.SHA256+".json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, record.SHA256+".tgz"), tarball, 0o600); err != nil {
		t.Fatal(err)
	}
	s.SetArtifactsDir(dir)
	id := enroll(t, st, "sampleagent2", time.Now().UTC().Add(-time.Hour))
	obs := batch(time.Now().UTC().Add(-time.Minute))
	obs.OpenClaw.Install = &model.OpenClawInstall{RunningDirVersion: "2026.9.1", RunningDir: "/opt/openclaw", NodeVersion: "22.22.2"}
	if err := st.RecordObservation(id, obs, obs.MeasuredAt); err != nil {
		t.Fatal(err)
	}
	if err := st.SetMachineChannel(id, "stable"); err != nil {
		t.Fatal(err)
	}
	body := get(t, s, "/updates")
	for _, want := range []string{"Hub artifact catalog evidence", "2026.9.2", record.SHA256[:12], "sampleagent2", "2026.9.1", "最後觀測版本", "觀測收件時間", "顯示 Hub 保存的最後一筆觀測", "影響 0 台，衝突 0，缺套件 1，unreachable 0"} {
		if !strings.Contains(body, want) {
			t.Errorf("Updates 缺少 %q", want)
		}
	}
	if strings.Contains(body, "<button") {
		t.Fatal("Updates 不該有任何按鈕")
	}
	if strings.Count(body, "stable promote 鎖著（stable_promotion_locked）") != 1 {
		t.Fatalf("stable 應有且只有一行 gate、canary 不該加：%s", body)
	}
}

func TestUpdatesTrueZeroMessagesAndBothChannels(t *testing.T) {
	s, _ := newServer(t)
	s.SetArtifactsDir(t.TempDir())
	body := get(t, s, "/updates")
	for _, want := range []string{"沒有 artifact", "canary channel", "stable channel", "沒有成員", "還沒有部署過"} {
		if !strings.Contains(body, want) {
			t.Errorf("空 Updates 頁缺少 %q", want)
		}
	}
}

func TestUpdatesRendersMissingArtifactSizeAsUnknownEvidence(t *testing.T) {
	s, _ := newServer(t)
	dir := t.TempDir()
	malformedID := strings.Repeat("e", 64)
	if err := os.WriteFile(filepath.Join(dir, malformedID+".json"), []byte(`{"broken":`), 0o600); err != nil {
		t.Fatal(err)
	}
	s.SetArtifactsDir(dir)
	body := get(t, s, "/updates")
	if !strings.Contains(body, "sidecar_invalid_json") || !strings.Contains(body, ">未知</td>") {
		t.Fatalf("Updates did not preserve unknown size evidence: %s", body)
	}
	if strings.Contains(body, ">0 B</td>") {
		t.Fatalf("Updates invented zero-byte evidence for an unknown size: %s", body)
	}
}

func TestUpdatesStrictQueryAndDiscoverableArtifactPagination(t *testing.T) {
	s, _ := newServer(t)
	dir := t.TempDir()
	for i := 0; i < operator.MaxArtifactReadLimit+1; i++ {
		writeWebCatalogArtifact(t, dir, fmt.Sprintf("2026.9.%d", i+1),
			[]byte(fmt.Sprintf("updates pagination artifact %03d", i)), true)
	}
	s.SetArtifactsDir(dir)

	for _, path := range []string{
		"/updates?", "/updates?unknown=value", "/updates?section=artifacts&section=channel-canary",
		"/updates?section=unknown", "/updates?cursor=", "/updates?cursor=not-a-cursor",
	} {
		if rec := doGet(t, s, path); rec.Code != http.StatusBadRequest {
			t.Errorf("GET %s status=%d, want 400", path, rec.Code)
		}
	}

	first := doGet(t, s, "/updates?section=channel-canary")
	if first.Code != http.StatusOK {
		t.Fatalf("first updates page=%d: %s", first.Code, first.Body.String())
	}
	if !strings.Contains(first.Body.String(), "本頁 100 項；符合條件共 101 項") ||
		strings.Contains(first.Body.String(), "從第一頁重新開始") {
		t.Fatalf("first updates page lacks truthful pagination evidence: %s", first.Body.String())
	}
	nextMatch := regexp.MustCompile(`href="([^"]+)">下一頁</a>`).FindStringSubmatch(first.Body.String())
	if len(nextMatch) != 2 {
		t.Fatalf("first updates page lacks next link: %s", first.Body.String())
	}
	nextURL, err := url.Parse(html.UnescapeString(nextMatch[1]))
	if err != nil || nextURL.Query().Get("section") != "channel-canary" ||
		nextURL.Query().Get("cursor") == "" || nextURL.Fragment != "channel-canary" {
		t.Fatalf("next updates URL did not preserve section/cursor: url=%v err=%v", nextURL, err)
	}
	second := doGet(t, s, nextURL.RequestURI())
	if second.Code != http.StatusOK ||
		!strings.Contains(second.Body.String(), "本頁 1 項；符合條件共 101 項") ||
		!strings.Contains(second.Body.String(), "從第一頁重新開始") ||
		strings.Contains(second.Body.String(), ">下一頁</a>") {
		t.Fatalf("second updates page is not truthful: status=%d body=%s", second.Code, second.Body.String())
	}
	startMatch := regexp.MustCompile(`href="([^"]+)">從第一頁重新開始</a>`).FindStringSubmatch(second.Body.String())
	if len(startMatch) != 2 {
		t.Fatalf("second updates page lacks start-over link: %s", second.Body.String())
	}
	startURL, err := url.Parse(html.UnescapeString(startMatch[1]))
	if err != nil || startURL.Query().Get("section") != "channel-canary" ||
		startURL.Query().Has("cursor") || startURL.Fragment != "channel-canary" {
		t.Fatalf("start-over URL did not preserve section or clear cursor: url=%v err=%v", startURL, err)
	}
}

func TestNewDeploymentIsolatesMalformedCatalogEntriesAndRequiresPresentRegularTarball(t *testing.T) {
	s, _ := newServer(t)
	dir := t.TempDir()
	good := writeWebCatalogArtifact(t, dir, "2026.9.8", []byte("deployable artifact"), true)
	missing := writeWebCatalogArtifact(t, dir, "2026.9.9", []byte("missing tarball"), false)
	malformedID := strings.Repeat("f", 64)
	if malformedID == good.SHA256 || malformedID == missing.SHA256 {
		t.Fatal("fixture digest collision")
	}
	if err := os.WriteFile(filepath.Join(dir, malformedID+".json"), []byte(`{"private":"broken"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	s.SetArtifactsDir(dir)

	page := doGet(t, s, "/deployments?view=new")
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), good.Version) ||
		!strings.Contains(page.Body.String(), good.SHA256) {
		t.Fatalf("deployable neighbor disappeared from New Deployment: status=%d body=%s", page.Code, page.Body.String())
	}
	if strings.Contains(page.Body.String(), missing.Version) || strings.Contains(page.Body.String(), malformedID) {
		t.Fatalf("New Deployment offered unavailable/invalid catalog evidence: %s", page.Body.String())
	}
}

func writeWebCatalogArtifact(t *testing.T, dir, version string, body []byte, withTarball bool) artifact.Sidecar {
	t.Helper()
	sha256Sum, sha512Sum := sha256.Sum256(body), sha512.Sum512(body)
	record := artifact.Sidecar{
		Name: "openclaw", Version: version,
		TarballURL:      "https://registry.npmjs.org/openclaw/-/openclaw-" + version + ".tgz",
		SHA512Integrity: "sha512-" + base64.StdEncoding.EncodeToString(sha512Sum[:]),
		SHA256:          hex.EncodeToString(sha256Sum[:]), Size: int64(len(body)),
		EnginesNode: ">=24 <25", FetchedAt: time.Now().UTC().Truncate(time.Second), FetchedBy: "web-test",
	}
	raw, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, record.SHA256+".json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if withTarball {
		if err := os.WriteFile(filepath.Join(dir, record.SHA256+".tgz"), body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return record
}

func TestAppsShowsUnknownForMachineWithoutObservation(t *testing.T) {
	s, st := newServer(t)
	_ = enroll(t, st, "sampleagent1", time.Now().UTC())
	body := get(t, s, "/apps?view=openclaw")
	section := sectionOf(t, body, "OpenClaw", "偵測規則")
	if !strings.Contains(section, "sampleagent1") || !strings.Contains(section, "未知") {
		t.Fatalf("Apps 沒把未觀測機器顯示成未知：%s", section)
	}
	if strings.Contains(body, "/proc/") {
		t.Fatal("Apps 不應公開主機上的程序檔案位置")
	}
}

func TestAppsShowsRunningInstallAndLatestSucceededOpenClawJob(t *testing.T) {
	s, st := newServer(t)
	_, jobs, _ := webDeployment(t, st, "canary", "samplehub1")
	job := jobs[0]
	now := time.Now().UTC()
	token, err := st.ClaimJob(job.JobID, job.MachineID, now.Add(-time.Minute), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AdvanceJobByAgent(job.JobID, job.MachineID, token, deploy.Start, now); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AdvanceJobByAgent(job.JobID, job.MachineID, token, deploy.FinishWork, now); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordVerification(job.JobID, job.MachineID, token, "health", "curl /health", 0, "ok", "", true, now); err != nil {
		t.Fatal(err)
	}
	if _, err := st.MarkSucceededIfVerified(job.JobID, now); err != nil {
		t.Fatal(err)
	}
	body := get(t, s, "/apps?view=openclaw")
	for _, want := range []string{"samplehub1", "2026.9.2", "24.15.0", job.JobID[:8], "canary"} {
		if !strings.Contains(body, want) {
			t.Errorf("Apps 缺少 %q", want)
		}
	}
	if strings.Contains(body, "/home/test/.local/share/clawctl") {
		t.Fatal("Apps 不應公開主機上的 release 位置")
	}
}

func TestAppsTreatsUntrustedObservedTextAsUnknown(t *testing.T) {
	s, st := newServer(t)
	_, _, ids := webDeployment(t, st, "canary", "hostile-app-observation")
	machineID := ids["hostile-app-observation"]
	observedAt := time.Now().UTC()
	b := batch(observedAt)
	oversized := strings.Repeat("9", 129)
	b.OpenClaw.Install = &model.OpenClawInstall{
		RunningDirVersion: oversized,
		NodeVersion:       oversized,
		RunningDir:        "\u202eprivate-release",
		ReleasesDir:       "/safe/releases",
	}
	var err error
	b.WorkloadPolicyToken, err = st.CurrentWorkloadPolicyToken("hostile-app-observation")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.RecordObservation(machineID, b, observedAt); err != nil {
		t.Fatal(err)
	}

	body := get(t, s, "/apps?view=openclaw")
	if strings.Contains(body, oversized) || strings.Contains(body, "private-release") {
		t.Fatalf("Apps reflected untrusted observed text: %s", body)
	}
	if strings.Count(body, "未知") < 2 {
		t.Fatalf("Apps did not fail closed to unknown values: %s", body)
	}
}

// TestDeploymentPageStatesCrossDomainEvidencePerTarget checks the deployment
// page answers the same question the job page does, one row per target, and
// that the header line counts targets rather than declaring the deployment
// verified.
func TestDeploymentPageStatesCrossDomainEvidencePerTarget(t *testing.T) {
	s, st := newServer(t)
	d, jobs, ids := webDeployment(t, st, "canary", "alpha", "beta")
	peerID := enroll(t, st, "independent-peer", time.Now().UTC().Add(-time.Hour))
	digest := "sha256:" + strings.Repeat("a", 64)

	page := get(t, s, "/deployments/"+d.DeploymentID)
	for _, want := range []string{
		"跨故障域證據", "已開單的 1 台中，0 台有第二個 producer 回報通過", "可用 producer 0",
		"absent", "沒有第二個 producer 為這張工作單寫過證據",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("day-one deployment page missing %q", want)
		}
	}

	verifier, _, err := st.RegisterVerifier(store.VerifierKindFleetPeerAgent, "peer-verifier", peerID, "")
	if err != nil {
		t.Fatalf("register verifier: %v", err)
	}
	if err := st.RecordIndependentVerification(store.IndependentVerificationRequest{
		VerifierID: verifier.VerifierID, JobID: jobs[0].JobID, RuleID: "installed-version",
		Command: "clawctl version", ObservedDigest: digest, Passed: true,
		VerifiedAt: time.Now().UTC().Truncate(time.Second),
	}); err != nil {
		t.Fatalf("record independent evidence: %v", err)
	}

	page = get(t, s, "/deployments/"+d.DeploymentID)
	for _, want := range []string{
		"已開單的 1 台中，1 台有第二個 producer 回報通過", "可用 producer 1",
		"passed", "第二個 producer 回報的規則全部通過",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("verified deployment page missing %q", want)
		}
	}
	// Scope the conclusion check to the block this slice owns. The top bar's
	// "Tailscale 已驗證" chip is about the operator's own identity and is not
	// part of the deployment's account of its targets.
	section := page[strings.Index(page, "跨故障域證據"):]
	if end := strings.Index(section, "<h3>排除</h3>"); end > 0 {
		section = section[:end]
	}
	for _, banned := range []string{"已驗證", "驗證通過", "驗證成功", "已確認", "healthy", "健康"} {
		if strings.Contains(section, banned) {
			t.Errorf("deployment independent block promoted a producer report into a conclusion: %q", banned)
		}
	}
	// The unopened second batch has no job, so it must stay blank rather than be
	// given a verdict about evidence that could not exist yet.
	if _, opened := ids["beta"]; !opened {
		t.Fatal("fixture lost its second target")
	}
	if strings.Count(page, "absent") != 0 {
		t.Errorf("unopened target was given a verdict: %q", page)
	}
}

// 部署頁把排除原因翻成一句話。少翻一個，畫面上就會出現「未識別的排除原因」——
// 操作員讀到的是一個沒有人解釋得了的字串，而不是那台為什麼沒被開單。
func TestEveryExclusionReasonHasWordsOnTheDeploymentPage(t *testing.T) {
	for _, reason := range []string{
		rollout.ExcludedConflict, rollout.ExcludedMissingPackage,
		rollout.ExcludedUnknownNode, rollout.ExcludedNoncompliant,
	} {
		words := exclusionReason(reason)
		if words == "" || strings.Contains(words, "未識別的排除原因") {
			t.Errorf("%s 沒有說法：%q", reason, words)
		}
	}
	if !strings.Contains(exclusionReason(rollout.ExcludedNoncompliant), "領不到新工作單") {
		t.Errorf("停發工作單那句沒說清楚後果：%q", exclusionReason(rollout.ExcludedNoncompliant))
	}
}
