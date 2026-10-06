package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/deploy"
	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/store"
)

func renderDeploymentActionReview(t *testing.T, s *Server, preview operator.DeploymentActionPreviewResult) string {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/deployments/"+preview.Deployment.DeploymentID+"/review", nil)
	s.render(rec, req, "deployment_action_review.html", page{
		Title: "確認部署動作", Nav: "deployments", DeploymentActionReview: &deploymentActionReview{
			Title: "確認部署動作", Preview: preview,
			ActionView: deploymentActionView(preview.Action, preview.Eligibility),
		},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

func TestDeploymentActionReviewLabelsBlockersWithoutWireTokens(t *testing.T) {
	s, _ := newServer(t)
	body := renderDeploymentActionReview(t, s, operator.DeploymentActionPreviewResult{
		Action: "continue", Deployment: operator.DeploymentSummary{DeploymentID: "deployment-blocked"},
		Eligibility: operator.DeploymentActionEligibility{
			Blockers: []string{"material_unavailable", "target_snapshot_changed"},
		},
	})
	for _, want := range []string{`class="caveat"`, "目前不可執行："} {
		if !strings.Contains(body, want) {
			t.Errorf("action review 缺少 %q: %s", want, body)
		}
	}
	for _, want := range []string{"Hub 無法重新驗證原 deployment material", "target snapshot 已變更"} {
		if !strings.Contains(body, want) {
			t.Errorf("action review 缺少 %q: %s", want, body)
		}
	}
	for _, forbidden := range []string{"material_unavailable", "target_snapshot_changed"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("action review 顯示 wire token %q: %s", forbidden, body)
		}
	}
}

func TestDeploymentActionReviewExplainsContinueFinish(t *testing.T) {
	s, _ := newServer(t)
	body := renderDeploymentActionReview(t, s, operator.DeploymentActionPreviewResult{
		Action: "continue", Deployment: operator.DeploymentSummary{DeploymentID: "deployment-finish"},
		Eligibility: operator.DeploymentActionEligibility{Eligible: true, Outcome: "finish"},
	})
	want := "確認後會發生：</b>所有批次都已開完且最後一批是 succeeded；確認後會把 deployment 收成 finished。"
	if !strings.Contains(body, want) || strings.Contains(body, "預覽允許：") {
		t.Fatalf("action review 未正確解釋 finish: %s", body)
	}
}

func TestDeploymentActionReviewExplainsEligibleImpacts(t *testing.T) {
	tests := []struct {
		name, action, outcome string
		affected              int
		want, forbidden       string
	}{
		{name: "continue next batch", action: "continue", outcome: "open_next_batch", affected: 3, want: "將開下一批 3 台"},
		{name: "retry", action: "retry", affected: 4, want: "將建立新的 deployment，為 4 台終態未成功的機器重新開單"},
		{name: "abandon", action: "abandon", affected: 5, want: "5 台尚未開單的機器不會再開單", forbidden: "未開 target"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, _ := newServer(t)
			body := renderDeploymentActionReview(t, s, operator.DeploymentActionPreviewResult{
				Action: tt.action, Deployment: operator.DeploymentSummary{DeploymentID: "deployment-impact"},
				Eligibility: operator.DeploymentActionEligibility{Eligible: true, Outcome: tt.outcome, AffectedTargets: tt.affected},
			})
			if !strings.Contains(body, tt.want) || tt.forbidden != "" && strings.Contains(body, tt.forbidden) {
				t.Fatalf("action review impact 不符: %s", body)
			}
		})
	}
}

func TestDeploymentCreateReviewLabelsBlockersWithoutWireTokens(t *testing.T) {
	s, _ := newServer(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/deployments?view=new", nil)
	s.render(rec, req, "deployment_create_review.html", page{
		Title: "確認建立 deployment", DeploymentCreateReview: &deploymentCreateReview{
			Preview: operator.DeploymentCreatePreviewResult{
				Blockers:  []string{"no_included_targets"},
				Promotion: &operator.DeploymentPromotionPreview{Allowed: false, Blockers: []string{"stable_promotion_locked"}},
			},
		},
	})
	body := rec.Body.String()
	for _, want := range []string{"沒有可開啟的 target", "stable promotion 安全閘門目前未通過"} {
		if !strings.Contains(body, want) {
			t.Errorf("create review 缺少 %q: %s", want, body)
		}
	}
	for _, forbidden := range []string{"no_included_targets", "stable_promotion_locked"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("create review 顯示 wire token %q: %s", forbidden, body)
		}
	}
}

func TestDeploymentDetailAndActionReviewUseIdenticalImpact(t *testing.T) {
	s, _ := newServer(t)
	eligibility := operator.DeploymentActionEligibility{Eligible: true, Outcome: "open_next_batch", AffectedTargets: 3}
	want := "將開下一批 3 台。目前批次的 Hub 判決是 succeeded；Continue 不會跳過失敗批次。"

	detailRec := httptest.NewRecorder()
	detailReq := httptest.NewRequest(http.MethodGet, "/deployments/deployment-shared", nil)
	s.render(detailRec, detailReq, "deployment.html", page{
		Title: "deployment", Deployment: &deploymentPage{
			Summary:  operator.DeploymentSummary{DeploymentID: "deployment-shared"},
			Continue: deploymentActionView("continue", eligibility),
		},
	})
	reviewBody := renderDeploymentActionReview(t, s, operator.DeploymentActionPreviewResult{
		Action: "continue", Deployment: operator.DeploymentSummary{DeploymentID: "deployment-shared"}, Eligibility: eligibility,
	})
	if strings.Count(detailRec.Body.String(), want) != 1 || strings.Count(reviewBody, want) != 1 {
		t.Fatalf("detail/action review 沒有一字不差地共用文案\ndetail=%s\nreview=%s", detailRec.Body.String(), reviewBody)
	}
}

func TestDeploymentReviewShowsIndependentGateStateAndExactNextStep(t *testing.T) {
	s, _ := newServer(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/deployments?view=new", nil)
	promotion := operator.DeploymentPromotionPreview{
		Allowed: false, Blockers: []string{"stable_promotion_locked"}, IndependentRequired: true,
		IndependentTargets: []operator.DeploymentPromotionIndependentTargetPreview{{
			MachineID: "machine-canary", DisplayName: "canary-one", JobID: "job-independent",
			State: "unassigned", NextStep: operator.PromotionNextStepAssignVerifier,
		}, {
			MachineID: "machine-canary-two", DisplayName: "canary-two", JobID: "job-release-unreported",
			State: "release_unreported", NextStep: operator.PromotionNextStepUpgradeAndReassignVerifier,
		}},
	}
	s.render(rec, req, "deployment_create_review.html", page{
		Title: "確認建立 deployment", Nav: "deployments", DeploymentCreateReview: &deploymentCreateReview{
			Preview: operator.DeploymentCreatePreviewResult{
				SchemaVersion: operator.DeploymentPreviewSchemaVersion, Channel: "stable",
				Targets: []operator.DeploymentPlanTargetPreview{}, Promotion: &promotion,
				Blockers: []string{"stable_promotion_locked"},
			},
		},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		"跨故障域 verifier 0 / 2 台通過", "canary-one", "unassigned",
		`href="/jobs/job-independent"`, "到工作單指派跨故障域 verifier",
		"canary-two", "release_unreported", "升級後重新指派 verifier",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("review 缺少 %q：%s", want, body)
		}
	}

	actionRec := httptest.NewRecorder()
	s.render(actionRec, req, "deployment_action_review.html", page{
		Title: "確認部署動作", Nav: "deployments", DeploymentActionReview: &deploymentActionReview{
			Title: "確認繼續", Preview: operator.DeploymentActionPreviewResult{
				Action: "continue", Promotion: &promotion,
				Eligibility: operator.DeploymentActionEligibility{Blockers: []string{"stable_promotion_locked"}},
				Deployment:  operator.DeploymentSummary{DeploymentID: "deployment-one", Channel: "stable"},
			},
		},
	})
	actionBody := actionRec.Body.String()
	if actionRec.Code != http.StatusOK ||
		!strings.Contains(actionBody, "到工作單指派跨故障域 verifier") ||
		!strings.Contains(actionBody, "release_unreported") ||
		!strings.Contains(actionBody, "升級後重新指派 verifier") {
		t.Fatalf("action review 沒有 exact 下一步：status=%d body=%s", actionRec.Code, actionRec.Body.String())
	}
}

func deploymentCreateApplyForm(t *testing.T, body, channel, version string) url.Values {
	t.Helper()
	return url.Values{
		"channel":                   {hiddenFormValue(t, body, "channel")},
		"version":                   {hiddenFormValue(t, body, "version")},
		"artifact_sha256":           {hiddenFormValue(t, body, "artifact_sha256")},
		"batch_size":                {hiddenFormValue(t, body, "batch_size")},
		"execution_timeout_seconds": {hiddenFormValue(t, body, "execution_timeout_seconds")},
		"irreversible":              {hiddenFormValue(t, body, "irreversible")},
		"preview_digest":            {hiddenFormValue(t, body, "preview_digest")},
		"reason":                    {hiddenFormValue(t, body, "reason")},
		"idempotency_key":           {hiddenFormValue(t, body, "idempotency_key")},
		"confirm_channel":           {channel},
		"confirm_version":           {version},
	}
}

func deploymentControlApplyForm(t *testing.T, body, action, confirmation, version string) url.Values {
	t.Helper()
	form := url.Values{
		"preview_digest":            {hiddenFormValue(t, body, "preview_digest")},
		"expected_control_revision": {hiddenFormValue(t, body, "expected_control_revision")},
		"expected_opened_batch":     {hiddenFormValue(t, body, "expected_opened_batch")},
		"reason":                    {hiddenFormValue(t, body, "reason")},
		"idempotency_key":           {hiddenFormValue(t, body, "idempotency_key")},
	}
	switch action {
	case "continue":
		form.Set("confirm_channel", confirmation)
	case "retry":
		form.Set("confirm_channel", confirmation)
		form.Set("confirm_version", version)
	case "abandon":
		form.Set("confirm_deployment_id", confirmation)
	default:
		t.Fatalf("unknown deployment action %q", action)
	}
	return form
}

func TestDeploymentCreateWebPreviewApplyReplayAndVerifiedActor(t *testing.T) {
	s, st := newServer(t)
	old, jobs, _ := webDeployment(t, st, "canary", "create-web-target")
	artifactDir, record := attachVerifiedDeploymentMaterial(t, s, st, old)
	finishedAt := time.Now().UTC().Truncate(time.Second)
	if _, err := st.DB().Exec(`UPDATE jobs SET state=?,terminal_at=? WHERE job_id=?`,
		deploy.Succeeded, finishedAt.Format(time.RFC3339Nano), jobs[0].JobID); err != nil {
		t.Fatal(err)
	}
	if changed, err := st.SetDeploymentState(old.DeploymentID, store.DeploymentRunning, store.DeploymentFinished, finishedAt); err != nil || !changed {
		t.Fatalf("finish old deployment changed=%t err=%v", changed, err)
	}

	settings := url.Values{
		"channel": {"canary"}, "version": {record.Version}, "artifact_sha256": {record.SHA256},
		"batch_size": {"1"}, "execution_timeout_seconds": {"600"}, "irreversible": {"false"},
		"reason": {"web create review"},
	}
	firstPreview := postForm(t, s, "/deployments/preview", settings)
	secondPreview := postForm(t, s, "/deployments/preview", settings)
	if firstPreview.Code != http.StatusOK || secondPreview.Code != http.StatusOK ||
		firstPreview.Header().Get("Cache-Control") != "no-store" || secondPreview.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("create previews=%d/%d cache=%q/%q", firstPreview.Code, secondPreview.Code,
			firstPreview.Header().Get("Cache-Control"), secondPreview.Header().Get("Cache-Control"))
	}
	firstKey := hiddenFormValue(t, firstPreview.Body.String(), "idempotency_key")
	secondKey := hiddenFormValue(t, secondPreview.Body.String(), "idempotency_key")
	if firstKey == secondKey {
		t.Fatalf("two create reviews reused idempotency key %q", firstKey)
	}
	for _, forbidden := range []string{record.TarballURL, record.FetchedBy, record.SHA512Integrity} {
		if strings.Contains(secondPreview.Body.String(), forbidden) {
			t.Fatalf("create review disclosed private artifact provenance %q", forbidden)
		}
	}

	badForm := deploymentCreateApplyForm(t, secondPreview.Body.String(), "stable", record.Version)
	bad := postForm(t, s, "/deployments", badForm)
	if bad.Code != http.StatusBadRequest || bad.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("bad create confirmation=%d cache=%q: %s", bad.Code, bad.Header().Get("Cache-Control"), bad.Body.String())
	}
	if got := tableCounts(t, st, "deployments")["deployments"]; got != 1 {
		t.Fatalf("bad confirmation created deployment; count=%d", got)
	}

	// A domain rejection owns its key. A new review creates a new operator
	// request identity for the corrected typed confirmation.
	preview := postForm(t, s, "/deployments/preview", settings)
	form := deploymentCreateApplyForm(t, preview.Body.String(), "canary", record.Version)
	created := postForm(t, s, "/deployments", form)
	if created.Code != http.StatusOK || created.Header().Get("Cache-Control") != "no-store" ||
		!strings.Contains(created.Body.String(), "已建立 deployment") {
		t.Fatalf("create apply=%d cache=%q: %s", created.Code, created.Header().Get("Cache-Control"), created.Body.String())
	}
	var deploymentID, createdBy string
	if err := st.DB().QueryRow(`SELECT deployment_id,created_by FROM deployments WHERE deployment_id<>?`, old.DeploymentID).
		Scan(&deploymentID, &createdBy); err != nil {
		t.Fatal(err)
	}
	if createdBy != "tailscale-user:42" {
		t.Fatalf("deployment created_by=%q, want verified request actor", createdBy)
	}
	var jobsBefore int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM jobs`).Scan(&jobsBefore); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(artifactDir, record.SHA256+".tgz")); err != nil {
		t.Fatal(err)
	}
	replayed := postForm(t, s, "/deployments", form)
	if replayed.Code != http.StatusOK || !strings.Contains(replayed.Body.String(), "replay") ||
		!strings.Contains(replayed.Body.String(), deploymentID) {
		t.Fatalf("create replay=%d: %s", replayed.Code, replayed.Body.String())
	}
	var jobsAfter int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM jobs`).Scan(&jobsAfter); err != nil {
		t.Fatal(err)
	}
	if jobsAfter != jobsBefore {
		t.Fatalf("create replay made jobs: before=%d after=%d", jobsBefore, jobsAfter)
	}
}

func TestDeploymentRetryWebUsesExactTerminalFailureSnapshot(t *testing.T) {
	s, st := newServer(t)
	parent, jobs, ids := webDeployment(t, st, "canary", "retry-only-this", "retry-not-opened")
	_, record := attachVerifiedDeploymentMaterial(t, s, st, parent)
	failDeploymentJob(t, st, jobs[0], time.Now().UTC())
	if changed, err := st.SetDeploymentState(parent.DeploymentID, store.DeploymentRunning, store.DeploymentPaused, time.Now().UTC()); err != nil || !changed {
		t.Fatalf("pause parent changed=%t err=%v", changed, err)
	}
	before, err := st.Deployment(parent.DeploymentID)
	if err != nil {
		t.Fatal(err)
	}
	preview := postForm(t, s, "/deployments/"+parent.DeploymentID+"/retry-preview", url.Values{"reason": {"retry exact terminal"}})
	if preview.Code != http.StatusOK || preview.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("retry preview=%d cache=%q: %s", preview.Code, preview.Header().Get("Cache-Control"), preview.Body.String())
	}
	if !strings.Contains(preview.Body.String(), "retry-only-this") || strings.Contains(preview.Body.String(), "retry-not-opened") ||
		!strings.Contains(preview.Body.String(), "Retry attempt 計畫") ||
		!strings.Contains(preview.Body.String(), "新 attempt 結果") ||
		!strings.Contains(preview.Body.String(), "範圍是下列機器") ||
		!strings.Contains(preview.Body.String(), "確認後會建立新的 deployment") ||
		!strings.Contains(preview.Body.String(), "只有下方「新 attempt 結果」標為「納入」的機器會開新工作單") ||
		!strings.Contains(preview.Body.String(), "新 attempt 的 batch 與排除結果") ||
		!strings.Contains(preview.Body.String(), "納入") {
		t.Fatalf("retry review did not show only exact terminal failure: %s", preview.Body.String())
	}
	form := deploymentControlApplyForm(t, preview.Body.String(), "retry", "canary", record.Version)
	result := postForm(t, s, "/deployments/"+parent.DeploymentID+"/retry", form)
	if result.Code != http.StatusOK || result.Header().Get("Cache-Control") != "no-store" ||
		!strings.Contains(result.Body.String(), "已 retry deployment") {
		t.Fatalf("retry apply=%d cache=%q: %s", result.Code, result.Header().Get("Cache-Control"), result.Body.String())
	}
	parentAfter, err := st.Deployment(parent.DeploymentID)
	if err != nil || parentAfter.State != store.DeploymentFinished || parentAfter.ControlRevision != before.ControlRevision+1 {
		t.Fatalf("retry parent not atomically finished: before=%+v after=%+v err=%v", before, parentAfter, err)
	}
	views, err := st.ListDeployments(time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, view := range views {
		if view.RetryOf != parent.DeploymentID {
			continue
		}
		found = true
		if len(view.Targets) != 1 || view.Targets[0].MachineID != ids["retry-only-this"] ||
			view.Targets[0].MachineID == ids["retry-not-opened"] {
			t.Fatalf("retry child targets=%+v", view.Targets)
		}
	}
	if !found {
		t.Fatal("retry apply did not create a child attempt")
	}
}

func TestDeploymentRetryReviewExplainsEveryTerminalFailureState(t *testing.T) {
	tests := []struct {
		state deploy.JobState
		want  string
	}{
		{deploy.Failed, "失敗"},
		{deploy.Rejected, "被拒絕，機器沒有改動"},
		{deploy.LeaseExpired, "代理程式沒有回報，Hub 收了這張單"},
		{deploy.ManualIntervention, "需要人介入，沒有回退證據"},
	}
	for _, tc := range tests {
		t.Run(string(tc.state), func(t *testing.T) {
			s, st := newServer(t)
			machineName := "terminal-" + string(tc.state)
			parent, jobs, ids := webDeployment(t, st, "canary", machineName)
			attachVerifiedDeploymentMaterial(t, s, st, parent)
			var job store.Job
			for _, candidate := range jobs {
				if candidate.MachineID == ids[machineName] {
					job = candidate
					break
				}
			}
			if job.JobID == "" {
				t.Fatalf("no job for machine %s", ids[machineName])
			}
			terminalAt := time.Now().UTC().Format(time.RFC3339)
			if _, err := st.DB().Exec(`UPDATE jobs SET state=?, terminal_at=? WHERE job_id=?`, tc.state, terminalAt, job.JobID); err != nil {
				t.Fatal(err)
			}
			if changed, err := st.SetDeploymentState(parent.DeploymentID, store.DeploymentRunning, store.DeploymentPaused, time.Now().UTC()); err != nil || !changed {
				t.Fatalf("pause parent changed=%t err=%v", changed, err)
			}
			preview := postForm(t, s, "/deployments/"+parent.DeploymentID+"/retry-preview", url.Values{"reason": {"review terminal outcome"}})
			if preview.Code != http.StatusOK {
				t.Fatalf("retry preview=%d: %s", preview.Code, preview.Body.String())
			}
			body := preview.Body.String()
			for _, want := range []string{
				tc.want + `（<span class="mono">` + string(tc.state) + `</span>）`,
				`<a class="mono" href="/jobs/` + job.JobID + `">` + job.JobID + `</a>`,
			} {
				if !strings.Contains(body, want) {
					t.Errorf("retry review missing %q: %s", want, body)
				}
			}
			if strings.Contains(body, "<td>"+string(tc.state)+"</td>") {
				t.Errorf("retry review showed raw state without outcome: %s", body)
			}
		})
	}
}

func TestDeploymentAbandonWebRequiresTypedIDAndDoesNotNeedArtifactBytes(t *testing.T) {
	s, st := newServer(t)
	parent, jobs, _ := webDeployment(t, st, "canary", "abandon-opened", "abandon-unopened")
	artifactDir, record := attachVerifiedDeploymentMaterial(t, s, st, parent)
	failDeploymentJob(t, st, jobs[0], time.Now().UTC())
	if changed, err := st.SetDeploymentState(parent.DeploymentID, store.DeploymentRunning, store.DeploymentPaused, time.Now().UTC()); err != nil || !changed {
		t.Fatalf("pause parent changed=%t err=%v", changed, err)
	}
	preview := postForm(t, s, "/deployments/"+parent.DeploymentID+"/abandon-preview", url.Values{"reason": {"stop unopened work"}})
	if preview.Code != http.StatusOK || !strings.Contains(preview.Body.String(), "輸入完整 deployment ID") {
		t.Fatalf("abandon preview=%d: %s", preview.Code, preview.Body.String())
	}
	badForm := deploymentControlApplyForm(t, preview.Body.String(), "abandon", "wrong-deployment", "")
	bad := postForm(t, s, "/deployments/"+parent.DeploymentID+"/abandon", badForm)
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("bad abandon confirmation=%d: %s", bad.Code, bad.Body.String())
	}
	if current, _ := st.Deployment(parent.DeploymentID); current.State != store.DeploymentPaused {
		t.Fatalf("bad abandon confirmation changed state=%s", current.State)
	}
	preview = postForm(t, s, "/deployments/"+parent.DeploymentID+"/abandon-preview", url.Values{"reason": {"stop unopened work"}})
	form := deploymentControlApplyForm(t, preview.Body.String(), "abandon", parent.DeploymentID, "")
	if err := os.Remove(filepath.Join(artifactDir, record.SHA256+".tgz")); err != nil {
		t.Fatal(err)
	}
	result := postForm(t, s, "/deployments/"+parent.DeploymentID+"/abandon", form)
	if result.Code != http.StatusOK || result.Header().Get("Cache-Control") != "no-store" ||
		!strings.Contains(result.Body.String(), "已 abandon deployment") {
		t.Fatalf("abandon apply=%d cache=%q: %s", result.Code, result.Header().Get("Cache-Control"), result.Body.String())
	}
	if current, err := st.Deployment(parent.DeploymentID); err != nil || current.State != store.DeploymentFinished {
		t.Fatalf("abandon did not finish parent=%+v err=%v", current, err)
	}
}

func TestDeploymentContinueWebRejectsStaleReview(t *testing.T) {
	s, st := newServer(t)
	parent, jobs, _ := webDeployment(t, st, "canary", "stale-opened", "stale-next")
	attachVerifiedDeploymentMaterial(t, s, st, parent)
	// The confirm form exists only when Continue is eligible. A failed opened
	// batch hides it; this review is the legitimate expand after success.
	// The concurrent opener below is the legacy store transition.
	succeedDeploymentJob(t, st, jobs[0], time.Now().UTC())
	if changed, err := st.SetDeploymentState(parent.DeploymentID, store.DeploymentRunning, store.DeploymentPaused, time.Now().UTC()); err != nil || !changed {
		t.Fatalf("pause parent changed=%t err=%v", changed, err)
	}
	preview := postForm(t, s, "/deployments/"+parent.DeploymentID+"/continue-preview", url.Values{"reason": {"stale review"}})
	if preview.Code != http.StatusOK {
		t.Fatalf("continue preview=%d: %s", preview.Code, preview.Body.String())
	}
	form := deploymentControlApplyForm(t, preview.Body.String(), "continue", "canary", "")
	if _, opened, err := st.ContinueDeployment(parent.DeploymentID, time.Now().UTC()); err != nil || len(opened) != 1 {
		t.Fatalf("concurrent continue opened=%+v err=%v", opened, err)
	}
	var before int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM jobs WHERE desired_id=?`, parent.DesiredID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	stale := postForm(t, s, "/deployments/"+parent.DeploymentID+"/continue", form)
	if stale.Code != http.StatusPreconditionFailed || stale.Header().Get("Cache-Control") != "no-store" ||
		!strings.Contains(stale.Body.String(), "preview") {
		t.Fatalf("stale continue=%d cache=%q: %s", stale.Code, stale.Header().Get("Cache-Control"), stale.Body.String())
	}
	var after int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM jobs WHERE desired_id=?`, parent.DesiredID).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("stale apply opened work: before=%d after=%d", before, after)
	}
}

func postRawDeploymentForm(t *testing.T, s *Server, target, mediaType, body string) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	s.Routes(mux)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(body))
	if mediaType != "" {
		req.Header.Set("Content-Type", mediaType)
	}
	req.RemoteAddr = "100.64.200.2:54321"
	req = verifiedWebRequest(req, "example.com/cap/clawctl-operate")
	mux.ServeHTTP(rec, req)
	return rec
}

func TestDeploymentMalformedFormsKeepTheirTypedActionIdentity(t *testing.T) {
	tests := []struct {
		name, target, subject string
		action                store.AuditAction
	}{
		{"create", "/deployments", "deployment create", store.AuditDeploymentCreate},
		{"continue", "/deployments/deployment-continue/continue", "deployment-continue", store.AuditDeploymentContinue},
		{"retry", "/deployments/deployment-retry/retry", "deployment-retry", store.AuditDeploymentRetry},
		{"abandon", "/deployments/deployment-abandon/abandon", "deployment-abandon", store.AuditDeploymentAbandon},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s, st := newServer(t)
			rec := postRawDeploymentForm(t, s, test.target, "application/x-www-form-urlencoded", "")
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("malformed %s status=%d: %s", test.name, rec.Code, rec.Body.String())
			}
			entries, err := st.Audit("", 10)
			if err != nil || len(entries) != 1 {
				t.Fatalf("transport audit=%+v err=%v", entries, err)
			}
			entry := entries[0]
			if entry.Action != test.action || entry.Subject != test.subject || entry.OK ||
				entry.SourceKind != "web" || entry.AuthSubject != "tailscale-user:42" {
				t.Fatalf("typed action audit=%+v", entry)
			}
		})
	}
}

func TestDeploymentApplyFormFailsClosedWithoutPersistingRawBytes(t *testing.T) {
	s, st := newServer(t)
	const deploymentID = "transport-form-deployment"
	validShape := "preview_digest=digest&expected_control_revision=1&expected_opened_batch=1&reason=ok&" +
		"idempotency_key=key&confirm_channel=canary"
	tests := []struct {
		name, target, mediaType, body, sentinel string
	}{
		{"oversized", "/deployments/" + deploymentID + "/continue", "application/x-www-form-urlencoded",
			validShape + "&reason=" + strings.Repeat("x", int(deploymentWebFormMaxBytes)) + "RAW_OVERSIZE", "RAW_OVERSIZE"},
		{"duplicate", "/deployments/" + deploymentID + "/continue", "application/x-www-form-urlencoded",
			validShape + "&idempotency_key=RAW_DUPLICATE", "RAW_DUPLICATE"},
		{"unknown trailing-like field", "/deployments/" + deploymentID + "/continue", "application/x-www-form-urlencoded",
			validShape + "&confirm_channel.trailing=RAW_UNKNOWN", "RAW_UNKNOWN"},
		{"query", "/deployments/" + deploymentID + "/continue?secret=RAW_QUERY", "application/x-www-form-urlencoded",
			validShape, "RAW_QUERY"},
		{"media type parameters", "/deployments/" + deploymentID + "/continue", "application/x-www-form-urlencoded; charset=utf-8",
			validShape + "&reason=RAW_MEDIA", "RAW_MEDIA"},
		{"malformed encoding", "/deployments/" + deploymentID + "/continue", "application/x-www-form-urlencoded",
			validShape + "&reason=%RAW_ENCODING", "RAW_ENCODING"},
	}
	wantDetail := store.OperatorTransportRejectionPrefix + "BAD_REQUEST: canonical request digest 無法取得"
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rec := postRawDeploymentForm(t, s, test.target, test.mediaType, test.body)
			if rec.Code != http.StatusBadRequest || rec.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("malformed form status=%d cache=%q: %s", rec.Code, rec.Header().Get("Cache-Control"), rec.Body.String())
			}
			entries, err := st.Audit("", 1)
			if err != nil || len(entries) != 1 {
				t.Fatalf("transport audit=%+v err=%v", entries, err)
			}
			entry := entries[0]
			if entry.Action != store.AuditDeploymentContinue || entry.Subject != deploymentID || entry.OK ||
				entry.Detail != wantDetail || entry.IdempotencyKey != "" || entry.RequestDigest != "" || entry.Reason != "" ||
				entry.SourceKind != "web" || entry.AuthSubject != "tailscale-user:42" {
				t.Fatalf("transport audit persisted noncanonical data: %+v", entry)
			}
			raw, err := json.Marshal(entry)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(raw), test.sentinel) {
				t.Fatalf("transport audit persisted raw sentinel %q: %s", test.sentinel, raw)
			}
		})
	}
}
