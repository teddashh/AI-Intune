package web

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/deploy"
	"github.com/teddashh/AI-Intune/internal/operatorauth"
	"github.com/teddashh/AI-Intune/internal/store"
)

func finishSucceededDeploymentForIA(t *testing.T, st *store.Store, d store.Deployment, jobs []store.Job) {
	t.Helper()
	finished := time.Now().UTC()
	for _, job := range jobs {
		if _, err := st.DB().Exec(`UPDATE jobs SET state=?,terminal_at=? WHERE job_id=?`,
			deploy.Succeeded, finished.Format(time.RFC3339), job.JobID); err != nil {
			t.Fatal(err)
		}
	}
	if changed, err := st.SetDeploymentState(d.DeploymentID, store.DeploymentRunning, store.DeploymentFinished, finished); err != nil || !changed {
		t.Fatalf("finish deployment changed=%v err=%v", changed, err)
	}
	for _, job := range jobs {
		if err := st.SetMachineChannel(job.MachineID, ""); err != nil {
			t.Fatalf("clear finished target channel: %v", err)
		}
	}
}

func finishStuckDeploymentForIA(t *testing.T, st *store.Store, d store.Deployment, jobs []store.Job) {
	t.Helper()
	now := time.Now().UTC()
	failDeploymentJob(t, st, jobs[0], now)
	if changed, err := st.SetDeploymentState(d.DeploymentID, store.DeploymentRunning, store.DeploymentPaused, now); err != nil || !changed {
		t.Fatalf("pause deployment changed=%v err=%v", changed, err)
	}
	if finished, opened, err := st.ContinueDeployment(d.DeploymentID, now); err != nil ||
		finished.State != store.DeploymentFinished || len(opened) != 0 {
		t.Fatalf("finish stuck deployment=%+v opened=%+v err=%v", finished, opened, err)
	}
	for _, job := range jobs {
		if err := st.SetMachineChannel(job.MachineID, ""); err != nil {
			t.Fatalf("clear stuck target channel: %v", err)
		}
	}
}

func TestDeploymentListViewsAreStrictAndShowDifferentLedgerSlices(t *testing.T) {
	s, st := newServer(t)
	finished, finishedJobs, _ := webDeployment(t, st, "canary", "finished-target")
	finishSucceededDeploymentForIA(t, st, finished, finishedJobs)

	stuck, stuckJobs, _ := webDeployment(t, st, "canary", "stuck-target")
	finishStuckDeploymentForIA(t, st, stuck, stuckJobs)

	active, _, _ := webDeployment(t, st, "canary", "active-target")

	assertRows := func(path string, included, excluded []string) string {
		t.Helper()
		body := get(t, s, path)
		for _, id := range included {
			if !strings.Contains(body, `title="`+id+`"`) {
				t.Errorf("GET %s missing deployment %s", path, id)
			}
		}
		for _, id := range excluded {
			if strings.Contains(body, `title="`+id+`"`) {
				t.Errorf("GET %s unexpectedly includes deployment %s", path, id)
			}
		}
		return body
	}
	assertRows("/deployments", []string{active.DeploymentID, stuck.DeploymentID, finished.DeploymentID}, nil)
	assertRows("/deployments?view=active", []string{active.DeploymentID}, []string{stuck.DeploymentID, finished.DeploymentID})
	assertRows("/deployments?view=stuck", []string{stuck.DeploymentID}, []string{active.DeploymentID, finished.DeploymentID})
	assertRows("/deployments?view=finished", []string{stuck.DeploymentID, finished.DeploymentID}, []string{active.DeploymentID})

	for _, path := range []string{
		"/deployments?", "/deployments?view=", "/deployments?view=all&view=active",
		"/deployments?view=unknown", "/deployments?unknown=value", "/deployments?section=unknown",
	} {
		if rec := doGet(t, s, path); rec.Code != http.StatusBadRequest {
			t.Errorf("GET %s status=%d, want 400; body=%s", path, rec.Code, rec.Body.String())
		}
	}
}

func TestNewDeploymentLeafExposesSettingsWithoutMutatingOnGET(t *testing.T) {
	s, st := newServer(t)
	tables := []string{"deployments", "desired_state", "jobs", "operator_idempotency", "audit_log"}
	before := tableCounts(t, st, tables...)
	body := get(t, s, "/deployments?view=new")
	after := tableCounts(t, st, tables...)

	for _, want := range []string{
		"新增部署", "Deployment settings", `method="post" action="/deployments/preview"`,
		"Preview deployment", "OpenClaw artifact 候選",
		"驗證層級：清單 metadata", "preview SHA-256 與部署資格",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("new deployment leaf missing %q", want)
		}
	}
	for _, forbidden := range []string{`method="get" action="/deployments"`, `name="preview_digest"`} {
		if strings.Contains(body, forbidden) {
			t.Errorf("new deployment leaf advertises undelivered mutation %q", forbidden)
		}
	}
	for _, table := range tables {
		if before[table] != after[table] {
			t.Errorf("GET new deployment mutated %s: before=%d after=%d", table, before[table], after[table])
		}
	}
	names, err := operatorauth.NamesForPrefix("example.com/cap/clawctl")
	if err != nil {
		t.Fatal(err)
	}
	viewOnly := renderWithCapabilities(t, s, "/deployments?view=new", operatorauth.CapabilityNames{View: names.View})
	if strings.Contains(viewOnly, `action="/deployments/preview"`) {
		t.Fatal("view-only operator saw create form")
	}
}

func TestDeploymentPagesExposeAccessibleIDsAndJobDrilldowns(t *testing.T) {
	s, st := newServer(t)
	d, jobs, _ := webDeployment(t, st, "canary", "drilldown-target")
	now := time.Now().UTC()
	failDeploymentJob(t, st, jobs[0], now)
	if changed, err := st.SetDeploymentState(d.DeploymentID, store.DeploymentRunning, store.DeploymentPaused, now); err != nil || !changed {
		t.Fatalf("pause changed=%v err=%v", changed, err)
	}

	list := get(t, s, "/deployments?view=stuck")
	for _, want := range []string{
		`<caption class="sr-only">Paused / Stuck deployment</caption>`, `scope="col"`, `scope="row"`,
		`aria-label="部署 ` + d.DeploymentID + `"`, d.DeploymentID,
		`/jobs?deployment_id=` + d.DeploymentID + `&amp;state=failed`,
	} {
		if !strings.Contains(list, want) {
			t.Errorf("deployment list missing accessible/drilldown evidence %q", want)
		}
	}

	detail := get(t, s, "/deployments/"+d.DeploymentID)
	for _, want := range []string{
		`href="/jobs?deployment_id=` + d.DeploymentID + `"`,
		`aria-label="工作單 ` + jobs[0].JobID + `"`,
		`/jobs?deployment_id=` + d.DeploymentID + `&amp;state=failed`,
		`<caption class="sr-only">此 deployment 的 included targets、工作單與跨故障域證據</caption>`,
		`id="canary-rollout"`, `canary_stopped`,
	} {
		if !strings.Contains(detail, want) {
			t.Errorf("deployment detail missing accessible/drilldown evidence %q", want)
		}
	}
}

func TestDeploymentActionEligibilityHidesDoomedContinueForm(t *testing.T) {
	names, err := operatorauth.NamesForPrefix("example.com/cap/clawctl")
	if err != nil {
		t.Fatal(err)
	}

	t.Run("terminal failure is structurally eligible", func(t *testing.T) {
		s, st := newServer(t)
		d, jobs, _ := webDeployment(t, st, "canary", "eligible-target")
		now := time.Now().UTC()
		failDeploymentJob(t, st, jobs[0], now)
		if changed, err := st.SetDeploymentState(d.DeploymentID, store.DeploymentRunning, store.DeploymentPaused, now); err != nil || !changed {
			t.Fatalf("pause changed=%v err=%v", changed, err)
		}

		body := renderWithCapabilities(t, s, "/deployments/"+d.DeploymentID, names)
		for _, want := range []string{
			"plain Continue 拒絕失敗批次", "Retry terminal failures",
			`action="/deployments/` + d.DeploymentID + `/abandon-preview"`,
			`action="/deployments/` + d.DeploymentID + `/retry-preview"`,
		} {
			if !strings.Contains(body, want) {
				t.Errorf("failed-batch deployment detail missing %q", want)
			}
		}
		if strings.Contains(body, `action="/deployments/`+d.DeploymentID+`/continue-preview"`) ||
			strings.Contains(body, `action="/deployments/`+d.DeploymentID+`/skip-failed-batch-preview"`) {
			t.Fatal("single-batch failure exposed Continue or skip failed batch")
		}
		if strings.Contains(body, "clawctl-hub deployment") || strings.Contains(body, `name="confirm_channel"`) {
			t.Fatal("detail exposed a CLI placeholder or skipped the preview step")
		}

		viewOnly := renderWithCapabilities(t, s, "/deployments/"+d.DeploymentID,
			operatorauth.CapabilityNames{View: names.View})
		if strings.Contains(viewOnly, `action="/deployments/`+d.DeploymentID+`/continue-preview"`) ||
			strings.Contains(viewOnly, `action="/deployments/`+d.DeploymentID+`/skip-failed-batch-preview"`) ||
			strings.Contains(viewOnly, `action="/deployments/`+d.DeploymentID+`/retry-preview"`) ||
			strings.Contains(viewOnly, `action="/deployments/`+d.DeploymentID+`/abandon-preview"`) {
			t.Fatal("view-only operator saw a mutation form")
		}
	})

	t.Run("paused with active job is blocked", func(t *testing.T) {
		s, st := newServer(t)
		d, _, _ := webDeployment(t, st, "canary", "active-paused-target")
		if changed, err := st.SetDeploymentState(d.DeploymentID, store.DeploymentRunning, store.DeploymentPaused, time.Now().UTC()); err != nil || !changed {
			t.Fatalf("pause changed=%v err=%v", changed, err)
		}
		body := renderWithCapabilities(t, s, "/deployments/"+d.DeploymentID, names)
		if strings.Contains(body, `action="/deployments/`+d.DeploymentID+`/continue-preview"`) {
			t.Fatal("paused deployment with active job exposed doomed Continue form")
		}
		for _, want := range []string{"仍有未終態工作單", "沒有終態未成功且可 retry 的機器"} {
			if !strings.Contains(body, want) {
				t.Errorf("blocked deployment detail missing %q", want)
			}
		}
	})

	t.Run("exhausted control revision is visible and blocks every action", func(t *testing.T) {
		s, st := newServer(t)
		d, jobs, _ := webDeployment(t, st, "canary", "exhausted-target")
		now := time.Now().UTC()
		failDeploymentJob(t, st, jobs[0], now)
		if changed, err := st.SetDeploymentState(d.DeploymentID, store.DeploymentRunning, store.DeploymentPaused, now); err != nil || !changed {
			t.Fatalf("pause changed=%v err=%v", changed, err)
		}
		if _, err := st.DB().Exec(`UPDATE deployments SET control_revision=? WHERE deployment_id=?`,
			store.MaxDeploymentControlRevision, d.DeploymentID); err != nil {
			t.Fatal(err)
		}
		body := renderWithCapabilities(t, s, "/deployments/"+d.DeploymentID, names)
		for _, action := range []string{"continue", "skip-failed-batch", "retry", "abandon"} {
			if strings.Contains(body, `action="/deployments/`+d.DeploymentID+`/`+action+`-preview"`) {
				t.Errorf("exhausted deployment exposed %s form", action)
			}
		}
		if !strings.Contains(body, "deployment 控制版本已達上限，不能再執行動作") {
			t.Fatal("exhausted deployment did not explain the blocker")
		}
	})
}
