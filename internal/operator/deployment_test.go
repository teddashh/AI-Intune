package operator

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/artifact"
	"github.com/teddashh/AI-Intune/internal/deploy"
	"github.com/teddashh/AI-Intune/internal/expect"
	"github.com/teddashh/AI-Intune/internal/model"
	"github.com/teddashh/AI-Intune/internal/rollout"
	"github.com/teddashh/AI-Intune/internal/store"
)

const deploymentTestVersion = "2026.9.8"

func newDeploymentOperatorStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func deploymentTestSpec(digest string) string {
	raw, _ := json.Marshal(model.OpenClawSpec{
		Kind: "openclaw", Version: deploymentTestVersion,
		Artifact: &model.ArtifactRef{
			SHA256: digest, Size: 42, URL: "/private/RAW_SPEC_SENTINEL",
			EnginesNode: ">=24.15.0 <25", UpstreamTarball: "https://registry.example/signed-secret",
			SHA512: "sha512-upstream",
		},
	})
	return string(raw)
}

func TestProjectDeploymentPromotionCarriesEveryIndependentNextStep(t *testing.T) {
	states := []rollout.IndependentGateState{
		rollout.IndependentGateUnassigned,
		rollout.IndependentGateAwaitingReport,
		rollout.IndependentGateIncompleteReport,
		rollout.IndependentGateProducerRevoked,
		rollout.IndependentGateDigestMismatch,
		rollout.IndependentGateReleaseMismatch,
		rollout.IndependentGateReleaseUnreported,
		rollout.IndependentGateStale,
		rollout.IndependentGateFailed,
		rollout.IndependentGatePassed,
	}
	wantSteps := []string{
		PromotionNextStepAssignVerifier,
		PromotionNextStepWaitForVerifier,
		PromotionNextStepRerunVerifier,
		PromotionNextStepAssignActiveVerifier,
		PromotionNextStepRerunCanary,
		PromotionNextStepRepairAndRerunCanary,
		PromotionNextStepUpgradeAndReassignVerifier,
		PromotionNextStepReassignVerifier,
		PromotionNextStepRepairAndRerunCanary,
		PromotionNextStepNone,
	}
	decision := rollout.PromoteDecision{IndependentTargets: []rollout.IndependentGateTarget{}}
	for i, state := range states {
		decision.IndependentTargets = append(decision.IndependentTargets, rollout.IndependentGateTarget{
			MachineID: "machine-" + string(rune('a'+i)), DisplayName: "machine", JobID: "job-" + string(rune('a'+i)), State: state,
		})
	}
	projected := projectDeploymentPromotion(decision)
	if !projected.IndependentRequired || projected.IndependentPassedTargets != 1 ||
		len(projected.IndependentTargets) != len(states) || projected.Blockers == nil {
		t.Fatalf("projection=%+v", projected)
	}
	for i, target := range projected.IndependentTargets {
		if target.State != string(states[i]) || target.NextStep != wantSteps[i] {
			t.Errorf("target %d=%+v want state=%s next=%s", i, target, states[i], wantSteps[i])
		}
	}
}

func TestProjectDeploymentPromotionKeepsCanaryNotSucceededOutOfThePassedCount(t *testing.T) {
	decision := rollout.PromoteDecision{Allowed: false, IndependentTargets: []rollout.IndependentGateTarget{
		{MachineID: "machine-pass", DisplayName: "passed", JobID: "job-pass", State: rollout.IndependentGatePassed},
		{MachineID: "machine-miss", DisplayName: "missing", JobID: "", State: rollout.IndependentGateCanaryNotSucceeded},
	}}
	projected := projectDeploymentPromotion(decision)
	if projected.IndependentPassedTargets != 1 || len(projected.IndependentTargets) != 2 {
		t.Fatalf("projection=%+v", projected)
	}
	missing := projected.IndependentTargets[1]
	if missing.State != string(rollout.IndependentGateCanaryNotSucceeded) || missing.JobID != "" ||
		missing.NextStep != PromotionNextStepRepairAndRerunCanary {
		t.Fatalf("missing target=%+v", missing)
	}
}

// A promotion field crosses three public documents: the create preview, the
// action preview that may finish a stable deployment, and Updates. Old strict
// clients reject unknown fields, so all three schema versions move together.
func TestIndependentPromotionFieldsComeWithNewSchemaVersions(t *testing.T) {
	if DeploymentReadSchemaVersion != 3 || DeploymentPreviewSchemaVersion != 4 ||
		DeploymentActionSchemaVersion != 4 || UpdateReadSchemaVersion != 5 {
		t.Fatalf("schema versions read=%d preview=%d action=%d updates=%d",
			DeploymentReadSchemaVersion, DeploymentPreviewSchemaVersion,
			DeploymentActionSchemaVersion, UpdateReadSchemaVersion)
	}
	want := []string{
		"allowed", "blockers", "earliest_at", "independent_required",
		"independent_passed_targets", "independent_targets",
	}
	if got := softwareJSONFields(DeploymentPromotionPreview{}); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("promotion fields=%v want=%v", got, want)
	}
}

func addDeploymentTestMachine(t *testing.T, st *store.Store, id, displayName, channel string) {
	t.Helper()
	if err := st.UpsertMachine(store.Machine{MachineID: id, DisplayName: displayName, Expected: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`UPDATE machine_registry SET channel=? WHERE machine_id=?`, channel, id); err != nil {
		t.Fatal(err)
	}
}

func TestDeploymentListDetailFiltersCursorActionsAndSafeJSON(t *testing.T) {
	st := newDeploymentOperatorStore(t)
	for _, machine := range []struct{ id, name string }{{"machine-a", "alpha"}, {"machine-b", "beta"}} {
		addDeploymentTestMachine(t, st, machine.id, machine.name, "canary")
	}
	digest := strings.Repeat("a", 64)
	input := store.NewDeployment{
		Channel: "canary", ResourceKind: "openclaw", ResourceID: "openclaw",
		Spec: deploymentTestSpec(digest), BatchSize: 1, CreatedBy: "RAW_CREATED_BY_SENTINEL",
		Targets: []store.NewDeploymentTarget{{MachineID: "machine-a", BatchNo: 1}, {MachineID: "machine-b", BatchNo: 2}},
		Job:     store.NewJob{ArtifactDigest: "sha256:" + digest, ExecutionTimeout: 600},
	}
	first, jobs, err := st.CreateDeployment(input)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("create first deployment=%+v jobs=%d err=%v", first, len(jobs), err)
	}
	failedAt := time.Now().UTC().Truncate(time.Second)
	if _, err := st.DB().Exec(`UPDATE jobs SET state=?,terminal_at=? WHERE job_id=?`,
		deploy.Failed, failedAt.Format(time.RFC3339Nano), jobs[0].JobID); err != nil {
		t.Fatal(err)
	}
	if changed, err := st.SetDeploymentState(first.DeploymentID, store.DeploymentRunning, store.DeploymentPaused, failedAt); err != nil || !changed {
		t.Fatalf("pause changed=%t err=%v", changed, err)
	}

	svc := New(st)
	evaluatedAt := time.Now().UTC()
	detail, err := svc.DeploymentDetail(first.DeploymentID, evaluatedAt)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Consistency != DeploymentReadConsistencyLive || detail.Item.DesiredRevision != first.Revision ||
		detail.Item.ControlRevision != 1 || detail.Item.OpenedBatch != 1 || detail.Item.TotalBatches != 2 ||
		detail.Item.Attempt != 1 || detail.Item.TerminalStuck != 1 || detail.Item.SilentStuck != 0 {
		t.Fatalf("detail item=%+v", detail.Item)
	}
	if len(detail.Targets) != 2 || detail.Targets[0].DisplayName != "alpha" || detail.Targets[1].DisplayName != "beta" {
		t.Fatalf("targets=%+v", detail.Targets)
	}
	if detail.Actions.Continue.Eligible || detail.Actions.Continue.AffectedTargets != 1 ||
		!detail.Actions.SkipFailedBatch.Eligible || detail.Actions.SkipFailedBatch.AffectedTargets != 1 ||
		!detail.Actions.Retry.Eligible || detail.Actions.Retry.AffectedTargets != 1 ||
		!detail.Actions.Abandon.Eligible || detail.Actions.Abandon.AffectedTargets != 1 {
		t.Fatalf("actions=%+v", detail.Actions)
	}
	assertDeploymentJSONSafe(t, detail)

	if _, err := st.AbandonDeployment(first.DeploymentID, evaluatedAt); err != nil {
		t.Fatal(err)
	}
	second, secondJobs, err := st.CreateDeployment(input)
	if err != nil || len(secondJobs) != 1 {
		t.Fatalf("create second deployment=%+v jobs=%d err=%v", second, len(secondJobs), err)
	}
	states := []string{store.DeploymentFinished, store.DeploymentRunning}
	page1, err := svc.ListDeployments(DeploymentListRequest{Channel: "canary", States: states, Limit: 1}, evaluatedAt)
	if err != nil {
		t.Fatal(err)
	}
	if states[0] != store.DeploymentFinished || page1.Consistency != DeploymentReadConsistencyLive ||
		page1.Total != 2 || len(page1.Items) != 1 || page1.NextCursor == nil {
		t.Fatalf("page1=%+v states=%v", page1, states)
	}
	page2, err := svc.ListDeployments(DeploymentListRequest{
		Channel: "canary", States: states, Limit: 1, Cursor: *page1.NextCursor,
	}, evaluatedAt)
	if err != nil || len(page2.Items) != 1 || page2.Items[0].DeploymentID == page1.Items[0].DeploymentID || page2.NextCursor != nil {
		t.Fatalf("page2=%+v err=%v", page2, err)
	}
	if !containsDeploymentIDs(append(page1.Items, page2.Items...), first.DeploymentID, second.DeploymentID) {
		t.Fatalf("pagination omitted deployments: page1=%+v page2=%+v", page1.Items, page2.Items)
	}
	wantStuck := true
	stuck, err := svc.ListDeployments(DeploymentListRequest{Stuck: &wantStuck}, evaluatedAt)
	if err != nil || stuck.Total != 1 || stuck.Items[0].DeploymentID != first.DeploymentID {
		t.Fatalf("stuck filter=%+v err=%v", stuck, err)
	}
	if _, err := svc.ListDeployments(DeploymentListRequest{Channel: "stable", States: states, Limit: 1, Cursor: *page1.NextCursor}, evaluatedAt); !errors.Is(err, ErrInvalidDeploymentRead) {
		t.Fatalf("cursor reused across filter: %v", err)
	}
	assertDeploymentJSONSafe(t, page1)
}

func TestDeploymentProjectionRejectsPoisonedViews(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*store.DeploymentView)
	}{
		{name: "unknown count", mutate: func(v *store.DeploymentView) { v.Counts[deploy.JobState("poison")] = 1 }},
		{name: "negative count", mutate: func(v *store.DeploymentView) { v.Counts[deploy.Failed] = -1 }},
		{name: "zero opened batch", mutate: func(v *store.DeploymentView) { v.OpenedBatch = 0 }},
		{name: "batch gap", mutate: func(v *store.DeploymentView) {
			v.Targets = append(v.Targets, store.DeploymentTarget{MachineID: "machine-b", DisplayName: "beta", BatchNo: 3})
			v.TotalBatches = 3
		}},
		{name: "partially opened batch", mutate: func(v *store.DeploymentView) {
			v.Targets = append(v.Targets, store.DeploymentTarget{MachineID: "machine-b", DisplayName: "beta", BatchNo: 1})
		}},
		{name: "terminal without terminal time", mutate: func(v *store.DeploymentView) { v.Targets[0].TerminalAt = nil }},
		{name: "nonterminal with terminal time", mutate: func(v *store.DeploymentView) {
			v.Targets[0].JobState = deploy.Running
			v.Targets[0].StuckKind = "no_event"
			v.Counts = map[deploy.JobState]int{deploy.Running: 1}
			v.TerminalStuck, v.SilentStuck = 0, 1
		}},
		{name: "mixed-up stuck kind", mutate: func(v *store.DeploymentView) {
			v.Targets[0].StuckKind = "no_event"
			v.TerminalStuck, v.SilentStuck = 0, 1
		}},
		{name: "terminal failure unclassified", mutate: func(v *store.DeploymentView) {
			v.Targets[0].StuckKind = ""
			v.Stuck, v.TerminalStuck = 0, 0
		}},
		{name: "linked job machine mismatch", mutate: func(v *store.DeploymentView) { v.Targets[0].JobMachineID = "machine-b" }},
		{name: "linked job desired mismatch", mutate: func(v *store.DeploymentView) { v.Targets[0].JobDesiredID = "other-desired" }},
		{name: "linked job revision mismatch", mutate: func(v *store.DeploymentView) { v.Targets[0].JobRevision++ }},
		{name: "globally shared linked job", mutate: func(v *store.DeploymentView) { v.Targets[0].JobReferences = 2 }},
		{name: "orphaned deployment desired-state job", mutate: func(v *store.DeploymentView) { v.DesiredJobCount++ }},
		{name: "cleared only opened job link", mutate: func(v *store.DeploymentView) {
			v.Targets[0] = store.DeploymentTarget{MachineID: "machine-a", DisplayName: "alpha", BatchNo: 1}
			v.Counts = map[deploy.JobState]int{}
			v.OpenedBatch, v.Stuck, v.TerminalStuck = 0, 0, 0
		}},
		{name: "duplicate linked job in projection", mutate: func(v *store.DeploymentView) {
			duplicate := v.Targets[0]
			duplicate.MachineID, duplicate.DisplayName, duplicate.JobMachineID = "machine-b", "beta", "machine-b"
			v.Targets = append(v.Targets, duplicate)
			v.Counts[deploy.Failed] = 2
			v.Stuck, v.TerminalStuck = 2, 2
		}},
		{name: "poisoned display name", mutate: func(v *store.DeploymentView) { v.Targets[0].DisplayName = "alpha\nsecret" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			view := coherentFailedDeploymentView()
			test.mutate(&view)
			if _, err := projectDeploymentSummary(view); !errors.Is(err, ErrInvalidDeploymentRead) {
				t.Fatalf("error=%v want ErrInvalidDeploymentRead", err)
			}
		})
	}
	if got := projectDeploymentMaterial("openclaw", "wrong-resource", deploymentTestSpec(strings.Repeat("a", 64))); got.Status != DeploymentMaterialInvalid {
		t.Fatalf("mislabeled OpenClaw material=%+v", got)
	}
}

func TestDeploymentActionEligibilityCountsOnlyTerminalFailuresForRetry(t *testing.T) {
	st := newDeploymentOperatorStore(t)
	version, artifactDigest := deploymentTestVersion, "sha256:"+strings.Repeat("a", 64)
	now := time.Now().UTC()
	terminal := now.Add(-time.Minute)
	view := store.DeploymentView{Deployment: store.Deployment{
		DeploymentID: "deployment", Channel: "canary", ResourceKind: "openclaw", ResourceID: "openclaw", State: store.DeploymentPaused,
	}, OpenedBatch: 1, TotalBatches: 1, Targets: []store.DeploymentTarget{
		{MachineID: "failed", JobID: "failed-job", JobState: deploy.Failed, TerminalAt: &terminal, StuckKind: "terminal_failure"},
		{MachineID: "silent", JobID: "silent-job", JobState: deploy.Running, StuckKind: "no_event"},
	}}
	actions, err := New(st).deploymentActionEligibility(view, DeploymentMaterialSummary{
		Status: DeploymentMaterialRecorded, Version: &version, ArtifactDigest: &artifactDigest,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if actions.Retry.AffectedTargets != 1 || actions.Retry.Eligible || !containsString(actions.Retry.Blockers, "nonterminal_jobs") {
		t.Fatalf("retry eligibility=%+v", actions.Retry)
	}
}

func TestDeploymentActionEligibilityBlocksExhaustedControlRevision(t *testing.T) {
	st := newDeploymentOperatorStore(t)
	// Continue is otherwise eligible only when the opened batch succeeded.
	// Retry is otherwise eligible only when some target is a terminal failure.
	// An earlier failed batch plus a succeeded opened batch satisfies both,
	// so the exhausted control revision is the only blocker on each action.
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	terminal := now.Add(-time.Minute)
	paused := now
	view := store.DeploymentView{
		Deployment: store.Deployment{
			DeploymentID: "deployment", Channel: "canary", DesiredID: "desired",
			ResourceKind: "openclaw", ResourceID: "openclaw", Revision: 1, BatchSize: 1,
			State: store.DeploymentPaused, CreatedAt: now.Add(-time.Hour), PausedAt: &paused,
			ControlRevision: store.MaxDeploymentControlRevision,
			Spec:            deploymentTestSpec(strings.Repeat("a", 64)),
		},
		Targets: []store.DeploymentTarget{
			{MachineID: "machine-a", DisplayName: "alpha", BatchNo: 1, JobID: "job-a",
				JobState: deploy.Failed, StuckKind: "terminal_failure", TerminalAt: &terminal},
			{MachineID: "machine-b", DisplayName: "beta", BatchNo: 2, JobID: "job-b",
				JobState: deploy.Succeeded, TerminalAt: &terminal},
			{MachineID: "machine-c", DisplayName: "gamma", BatchNo: 3},
		},
		OpenedBatch: 2, TotalBatches: 3,
	}
	version, digest := "2026.9.2", "sha256:"+strings.Repeat("a", 64)
	actions, err := New(st).deploymentActionEligibility(view, DeploymentMaterialSummary{
		Status: DeploymentMaterialRecorded, Version: &version, ArtifactDigest: &digest,
	}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	for name, action := range map[string]DeploymentActionEligibility{
		"continue": actions.Continue, "retry": actions.Retry, "abandon": actions.Abandon,
	} {
		if action.Eligible || len(action.Blockers) != 1 || action.Blockers[0] != "control_revision_exhausted" {
			t.Errorf("%s exhausted eligibility=%+v", name, action)
		}
	}
}

func TestDeploymentCreatePreviewVerifiesBytesOrdersPlanAndBindsSemanticInputs(t *testing.T) {
	st := newDeploymentOperatorStore(t)
	now := time.Date(2026, 9, 8, 18, 0, 0, 0, time.UTC)
	if err := prepareDeploymentObservationPolicy(st, now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	observeDeploymentMachine(t, st, "machine-z", "zeta", "canary", "24.15.0", deploymentTestVersion, now)
	observeDeploymentMachine(t, st, "machine-a", "alpha", "canary", "22.22.2", deploymentTestVersion, now)
	observeDeploymentMachine(t, st, "machine-m", "mu", "canary", "24.15.0", deploymentTestVersion, now.Add(-30*time.Minute))

	artifactDir := t.TempDir()
	record := writeDeploymentArtifact(t, artifactDir, deploymentTestVersion, ">=24.15.0 <25", now.Add(-time.Hour))
	svc := NewWithArtifacts(st, artifactDir)
	req := DeploymentCreatePreviewRequest{Channel: "canary", Version: deploymentTestVersion, ArtifactSHA256: record.SHA256, BatchSize: 1}
	preview, err := svc.PreviewDeploymentCreate(req, now)
	if err != nil {
		t.Fatal(err)
	}
	if !preview.CreateAllowed || preview.ExecutionTimeoutSeconds != DefaultDeploymentTimeout ||
		preview.Impact != 2 || preview.MissingPackages != 1 || preview.Unreachable != 1 || preview.TotalBatches != 2 ||
		len(preview.Targets) != 3 || preview.Targets[0].DisplayName != "alpha" || preview.Targets[1].DisplayName != "mu" ||
		preview.Targets[2].DisplayName != "zeta" || !strings.HasPrefix(preview.PreviewDigest, "sha256:") {
		t.Fatalf("preview=%+v", preview)
	}
	assertDeploymentJSONSafe(t, preview)

	later := preview
	later, err = svc.PreviewDeploymentCreate(req, now.Add(time.Minute))
	if err != nil || later.PreviewDigest != preview.PreviewDigest {
		t.Fatalf("preview time changed semantic digest: before=%s after=%s err=%v", preview.PreviewDigest, later.PreviewDigest, err)
	}
	changed := req
	changed.BatchSize = 2
	changedPreview, err := svc.PreviewDeploymentCreate(changed, now)
	if err != nil || changedPreview.PreviewDigest == preview.PreviewDigest {
		t.Fatalf("batch size did not change digest: %+v err=%v", changedPreview, err)
	}
	whitespace := req
	whitespace.Version += " "
	if _, err := svc.PreviewDeploymentCreate(whitespace, now); !errors.Is(err, ErrInvalidDeploymentPreview) {
		t.Fatalf("surrounding whitespace accepted: %v", err)
	}
	if _, err := New(st).PreviewDeploymentCreate(req, now); !errors.Is(err, ErrInvalidDeploymentPreview) {
		t.Fatalf("unconfigured artifact catalog accepted: %v", err)
	}
	bad := bytes.Repeat([]byte{'x'}, int(record.Size))
	if err := os.WriteFile(filepath.Join(artifactDir, record.SHA256+".tgz"), bad, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.PreviewDeploymentCreate(req, now); !errors.Is(err, ErrInvalidDeploymentPreview) {
		t.Fatalf("corrupt artifact accepted: %v", err)
	}
}

func TestDeploymentContextVariantsRejectCanceledRequest(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	svc := &Service{}
	now := time.Date(2026, 9, 8, 18, 0, 0, 0, time.UTC)
	if _, err := svc.PreviewDeploymentCreateContext(ctx, DeploymentCreatePreviewRequest{}, now); !errors.Is(err, context.Canceled) {
		t.Fatalf("create preview cancellation error = %v", err)
	}
	if _, err := svc.ApplyDeploymentCreateContext(ctx, DeploymentCreateApplyRequest{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("create apply cancellation error = %v", err)
	}
	if _, err := svc.PreviewDeploymentContinueContext(ctx, DeploymentContinuePreviewRequest{}, now); !errors.Is(err, context.Canceled) {
		t.Fatalf("continue preview cancellation error = %v", err)
	}
	if _, err := svc.PreviewDeploymentRetryContext(ctx, DeploymentRetryPreviewRequest{}, now); !errors.Is(err, context.Canceled) {
		t.Fatalf("retry preview cancellation error = %v", err)
	}
	if _, err := svc.ApplyDeploymentContinueContext(ctx, DeploymentContinueApplyRequest{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("continue apply cancellation error = %v", err)
	}
	if _, err := svc.ApplyDeploymentRetryContext(ctx, DeploymentRetryApplyRequest{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("retry apply cancellation error = %v", err)
	}
}

func TestStableDeploymentCreatePreviewAcceptsExactMatchingCanaryArtifact(t *testing.T) {
	st := newDeploymentOperatorStore(t)
	finishedAt := time.Date(2026, 9, 4, 0, 0, 0, 0, time.UTC)
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	if err := prepareDeploymentObservationPolicy(st, finishedAt.Add(-24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	artifactDir := t.TempDir()
	record := writeDeploymentArtifact(t, artifactDir, deploymentTestVersion, ">=24.15.0 <25", finishedAt.Add(-time.Hour))
	observeDeploymentMachine(t, st, "canary-machine", "canary witness", "canary", "24.15.0", deploymentTestVersion, finishedAt.Add(-time.Hour))
	material, err := artifact.ResolveOpenClawMaterial(artifactDir, deploymentTestVersion, record.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	canary, jobs, err := st.CreateDeployment(store.NewDeployment{
		Channel: "canary", ResourceKind: "openclaw", ResourceID: "openclaw", Spec: material.Spec,
		BatchSize: 1, CreatedBy: "canary fixture", Targets: []store.NewDeploymentTarget{{MachineID: "canary-machine", BatchNo: 1}},
		Job: store.NewJob{ArtifactDigest: material.Digest, ExecutionTimeout: 600},
	})
	if err != nil || len(jobs) != 1 {
		t.Fatalf("canary=%+v jobs=%d err=%v", canary, len(jobs), err)
	}
	var boundary int64
	if err := st.DB().QueryRow(`SELECT COALESCE(MAX(evidence_id),0) FROM workload_observation_evidence`).Scan(&boundary); err != nil {
		t.Fatal(err)
	}
	stamp := finishedAt.Format(time.RFC3339Nano)
	if _, err := st.DB().Exec(`UPDATE jobs SET state=?,terminal_at=? WHERE job_id=?`, deploy.Succeeded, stamp, jobs[0].JobID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`UPDATE deployments SET state=?,finished_at=?,control_revision=control_revision+1 WHERE deployment_id=?`,
		store.DeploymentFinished, stamp, canary.DeploymentID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`INSERT INTO deployment_soak_boundaries(deployment_id,evidence_id) VALUES(?,?)`, canary.DeploymentID, boundary); err != nil {
		t.Fatal(err)
	}
	verifierID := "promotion-gate-peer"
	if _, err := st.DB().Exec(`INSERT INTO verifiers
 (verifier_id,kind,display_name,failure_domain,credential_hash,created_at,revision)
 VALUES (?,?,?,?,?,?,1)`, verifierID, store.VerifierKindFleetPeerAgent, "canary peer", "another-machine",
		"fixture-hash", finishedAt.Add(time.Second).Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`INSERT INTO verification_assignments
 (assignment_id,job_id,verifier_id,assigned_at,assigned_by) VALUES (?,?,?,?,?)`,
		"promotion-gate-assignment", jobs[0].JobID, verifierID,
		finishedAt.Add(time.Second).Format(time.RFC3339Nano), "fixture"); err != nil {
		t.Fatal(err)
	}
	for i, ruleID := range []string{
		model.IndependentRuleOpenClawCurrentRelease,
		model.IndependentRuleOpenClawGatewayHTTP,
		model.IndependentRuleOpenClawUnitState,
	} {
		observedVersion := ""
		if ruleID == model.IndependentRuleOpenClawCurrentRelease {
			observedVersion = "2026.9.8"
		}
		if _, err := st.DB().Exec(`INSERT INTO verification_results
 (verification_id,job_id,machine_id,rule_id,command,exit_code,stdout_excerpt,stderr_excerpt,
  passed,verified_at,producer_kind,producer_id,evidence_role,authority,provenance_recorded,
  received_at,observed_digest,observed_version,verifier_id)
 VALUES (?,?,?,?,?,0,'','','1',?,?,?,?,?,1,?,'',?,?)`,
			[]string{"promotion-gate-rule-1", "promotion-gate-rule-2", "promotion-gate-rule-3"}[i],
			jobs[0].JobID, "canary-machine", ruleID, "fixture "+ruleID,
			finishedAt.Add(2*time.Second).Format(time.RFC3339Nano), store.VerifierKindFleetPeerAgent,
			verifierID, store.JobVerificationRoleIndependent, store.JobVerificationAuthorityVerifierBearer,
			finishedAt.Add(2*time.Second).Format(time.RFC3339Nano), observedVersion, verifierID); err != nil {
			t.Fatal(err)
		}
	}
	token, err := st.CurrentWorkloadPolicyToken("canary witness")
	if err != nil {
		t.Fatal(err)
	}
	tx, err := st.DB().Begin()
	if err != nil {
		t.Fatal(err)
	}
	statement, err := tx.Prepare(`INSERT INTO workload_observation_evidence
 (machine_id,received_at,evidence_at,verdict,openclaw_present,running_version,workload_policy_token,policy_valid)
 VALUES (?,?,?,?,?,?,?,?)`)
	if err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	for at := finishedAt.Add(time.Minute); at.Before(now); at = at.Add(10 * time.Minute) {
		stamp := at.Format(time.RFC3339Nano)
		if _, err := statement.Exec("canary-machine", stamp, stamp, "healthy", 1, deploymentTestVersion, token, 1); err != nil {
			_ = statement.Close()
			_ = tx.Rollback()
			t.Fatal(err)
		}
	}
	if err := statement.Close(); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	observeDeploymentMachine(t, st, "canary-machine", "canary witness", "canary", "24.15.0", deploymentTestVersion, now)
	observeDeploymentMachine(t, st, "stable-machine", "stable target", "stable", "24.15.0", deploymentTestVersion, now)

	preview, err := NewWithArtifacts(st, artifactDir).PreviewDeploymentCreate(DeploymentCreatePreviewRequest{
		Channel: "stable", Version: deploymentTestVersion, ArtifactSHA256: record.SHA256, BatchSize: 1,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if preview.Promotion == nil || !preview.Promotion.Allowed || len(preview.Promotion.Blockers) != 0 || !preview.CreateAllowed {
		t.Fatalf("matching canary did not unlock stable preview: %+v", preview)
	}
}

func coherentFailedDeploymentView() store.DeploymentView {
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	terminal := now.Add(-time.Minute)
	paused := now
	return store.DeploymentView{
		Deployment: store.Deployment{
			DeploymentID: "deployment", Channel: "canary", DesiredID: "desired", ResourceKind: "openclaw", ResourceID: "openclaw",
			Revision: 1, BatchSize: 2, State: store.DeploymentPaused, CreatedAt: now.Add(-time.Hour), PausedAt: &paused,
			Spec: deploymentTestSpec(strings.Repeat("a", 64)),
		},
		Targets: []store.DeploymentTarget{{
			MachineID: "machine-a", DisplayName: "alpha", BatchNo: 1, JobID: "job-a", JobState: deploy.Failed,
			JobMachineID: "machine-a", JobDesiredID: "desired", JobRevision: 1, JobReferences: 1,
			CreatedAt: now.Add(-30 * time.Minute), LastActivity: terminal, TerminalAt: &terminal, StuckKind: "terminal_failure",
		}},
		DesiredJobCount: 1, Counts: map[deploy.JobState]int{deploy.Failed: 1}, OpenedBatch: 1, TotalBatches: 1,
		Stuck: 1, TerminalStuck: 1, Attempt: 1,
	}
}

func containsDeploymentIDs(items []DeploymentSummary, ids ...string) bool {
	want := make(map[string]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}
	for _, item := range items {
		delete(want, item.DeploymentID)
	}
	return len(want) == 0
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func assertDeploymentJSONSafe(t *testing.T, value any) {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	for _, sentinel := range []string{"RAW_SPEC_SENTINEL", "RAW_CREATED_BY_SENTINEL", "/private/", "signed-secret"} {
		if bytes.Contains(raw, []byte(sentinel)) {
			t.Errorf("deployment DTO exposed forbidden value %q: %s", sentinel, raw)
		}
	}
	var document any
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	forbidden := map[string]bool{
		"spec": true, "created_by": true, "path": true, "upstream_tarball": true,
		"upstream_tarball_url": true, "fetched_by": true, "reason": true,
	}
	var walk func(any)
	walk = func(node any) {
		switch typed := node.(type) {
		case map[string]any:
			for key, child := range typed {
				if forbidden[key] {
					t.Errorf("deployment DTO exposed forbidden key %q: %s", key, raw)
				}
				walk(child)
			}
		case []any:
			for _, child := range typed {
				walk(child)
			}
		}
	}
	walk(document)
}

func prepareDeploymentObservationPolicy(st *store.Store, epoch time.Time) error {
	st.SetExpectations(&expect.Set{})
	return st.PublishExpectationsPolicy(epoch)
}

func observeDeploymentMachine(t *testing.T, st *store.Store, id, name, channel, nodeVersion, runningVersion string, at time.Time) {
	t.Helper()
	if err := st.UpsertMachine(store.Machine{MachineID: id, DisplayName: name, Expected: true}); err != nil {
		t.Fatal(err)
	}
	token, err := st.CurrentWorkloadPolicyToken(name)
	if err != nil {
		t.Fatal(err)
	}
	lastTask := at.Add(-time.Minute)
	matched := true
	if err := st.RecordCheckin(id, model.Checkin{SentAt: at, AgentStartedAt: at.Add(-time.Hour)}, at); err != nil {
		t.Fatal(err)
	}
	batch := model.ObservationBatch{
		MeasuredAt: at, WorkloadPolicyToken: token,
		Identity: model.Identity{Hostname: name, OS: "linux", Arch: "amd64", UnixUser: "operator-test", MachineIDHint: id},
		OpenClaw: model.OpenClaw{Present: true, Install: &model.OpenClawInstall{
			UnitFound: true, MainPID: 42, ProcessMatchesUnit: &matched,
			RunningDirVersion: runningVersion, NodeVersion: nodeVersion,
		}, DB: &model.OpenClawDB{Present: true, TaskRunRows: 1, LastTaskEndedAt: &lastTask}},
		CLITools: []model.CLITool{{Name: "openclaw", Present: true, PresentEvidence: "process", RunningPID: 42}},
	}
	if err := st.RecordObservation(id, batch, at); err != nil {
		t.Fatal(err)
	}
	if err := st.SetMachineChannel(id, channel); err != nil {
		t.Fatal(err)
	}
}

func writeDeploymentArtifact(t *testing.T, dir, version, engines string, fetchedAt time.Time) artifact.Sidecar {
	t.Helper()
	body := []byte("operator deployment artifact bytes")
	sum := sha256.Sum256(body)
	sri := sha512.Sum512(body)
	digest := hex.EncodeToString(sum[:])
	record := artifact.Sidecar{
		Name: "openclaw", Version: version, TarballURL: "https://registry.example/openclaw.tgz",
		SHA512Integrity: "sha512-" + base64.StdEncoding.EncodeToString(sri[:]),
		SHA256:          digest, Size: int64(len(body)),
		EnginesNode: engines, FetchedAt: fetchedAt, FetchedBy: "RAW_FETCHED_BY_SENTINEL@hub",
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
	return record
}

// TestDeploymentDetailReportsIndependentEvidencePerTarget checks the three
// states a target can be in — no job, a job nobody verified, and a job a second
// producer reported on — and checks the rollup is derived from the targets
// rather than asserted next to them.
func TestDeploymentDetailReportsIndependentEvidencePerTarget(t *testing.T) {
	st := newDeploymentOperatorStore(t)
	for _, machine := range []struct{ id, name string }{
		{"machine-a", "alpha"}, {"machine-b", "beta"}, {"machine-peer", "peer"},
	} {
		addDeploymentTestMachine(t, st, machine.id, machine.name, "canary")
	}
	digest := strings.Repeat("a", 64)
	created, jobs, err := st.CreateDeployment(store.NewDeployment{
		Channel: "canary", ResourceKind: "openclaw", ResourceID: "openclaw",
		Spec: deploymentTestSpec(digest), BatchSize: 2, CreatedBy: "independent-detail-test",
		Targets: []store.NewDeploymentTarget{
			{MachineID: "machine-a", BatchNo: 1}, {MachineID: "machine-b", BatchNo: 1},
			{MachineID: "machine-peer", BatchNo: 2},
		},
		Job: store.NewJob{ArtifactDigest: "sha256:" + digest, ExecutionTimeout: 600},
	})
	if err != nil || len(jobs) != 2 {
		t.Fatalf("create deployment=%+v jobs=%d err=%v", created, len(jobs), err)
	}
	opened := make(map[string]string, len(jobs))
	for _, job := range jobs {
		opened[job.MachineID] = job.JobID
	}

	verifier, _, err := st.RegisterVerifier(store.VerifierKindFleetPeerAgent, "peer-verifier", "machine-peer", "")
	if err != nil {
		t.Fatalf("register verifier: %v", err)
	}
	if err := st.RecordIndependentVerification(store.IndependentVerificationRequest{
		VerifierID: verifier.VerifierID, JobID: opened["machine-a"], RuleID: "installed-version",
		Command: "clawctl version", ObservedDigest: "sha256:" + digest, Passed: true,
		VerifiedAt: time.Now().UTC().Truncate(time.Second),
	}); err != nil {
		t.Fatalf("record independent evidence: %v", err)
	}

	detail, err := New(st).DeploymentDetail(created.DeploymentID, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	byMachine := make(map[string]DeploymentTargetSummary, len(detail.Targets))
	for _, target := range detail.Targets {
		byMachine[target.MachineID] = target
	}
	verified := byMachine["machine-a"].Independent
	if verified == nil || verified.Verdict != string(store.IndependentPassed) ||
		verified.Rows != 1 || verified.LiveProducers != 1 {
		t.Fatalf("verified target independent=%+v", verified)
	}
	unverified := byMachine["machine-b"].Independent
	if unverified == nil || unverified.Verdict != string(store.IndependentAbsent) ||
		unverified.Rows != 0 || unverified.LiveProducers != 0 {
		t.Fatalf("unverified target independent=%+v", unverified)
	}
	if byMachine["machine-peer"].JobID != nil || byMachine["machine-peer"].Independent != nil {
		t.Fatalf("unopened target was given a verdict: %+v", byMachine["machine-peer"])
	}

	if detail.Independent.OpenedTargets != 2 || detail.Independent.PassedTargets != 1 ||
		detail.Independent.LiveProducers != 1 {
		t.Fatalf("rollup=%+v", detail.Independent)
	}
	want := map[string]int{"absent": 1, "producer_revoked": 0, "digest_mismatch": 0,
		"release_mismatch": 0, "stale": 0, "failed": 0, "release_unreported": 0, "passed": 1}
	if len(detail.Independent.Verdicts) != len(want) {
		t.Fatalf("rollup does not list every verdict: %+v", detail.Independent.Verdicts)
	}
	order := []string{"absent", "producer_revoked", "digest_mismatch", "release_mismatch",
		"stale", "failed", "release_unreported", "passed"}
	for i, count := range detail.Independent.Verdicts {
		if count.Verdict != order[i] || count.Targets != want[count.Verdict] {
			t.Fatalf("rollup row %d=%+v", i, count)
		}
	}
	assertDeploymentJSONSafe(t, detail)
}

// TestDeploymentDetailIndependentSurvivesProducerRevocation pins the behaviour
// an operator will hit first: a verifier is decommissioned and its rows stay in
// the ledger. The target must stop reading as passed without the evidence
// disappearing.
func TestDeploymentDetailIndependentSurvivesProducerRevocation(t *testing.T) {
	st := newDeploymentOperatorStore(t)
	addDeploymentTestMachine(t, st, "machine-a", "alpha", "canary")
	// The verifier's own machine is deliberately outside the canary channel: a
	// second producer does not have to be a deployment target to speak about one.
	addDeploymentTestMachine(t, st, "machine-peer", "peer", "stable")
	digest := strings.Repeat("a", 64)
	created, jobs, err := st.CreateDeployment(store.NewDeployment{
		Channel: "canary", ResourceKind: "openclaw", ResourceID: "openclaw",
		Spec: deploymentTestSpec(digest), BatchSize: 1, CreatedBy: "independent-revoke-test",
		Targets: []store.NewDeploymentTarget{{MachineID: "machine-a", BatchNo: 1}},
		Job:     store.NewJob{ArtifactDigest: "sha256:" + digest, ExecutionTimeout: 600},
	})
	if err != nil || len(jobs) != 1 {
		t.Fatalf("create deployment=%+v jobs=%d err=%v", created, len(jobs), err)
	}
	verifier, _, err := st.RegisterVerifier(store.VerifierKindFleetPeerAgent, "peer-verifier", "machine-peer", "")
	if err != nil {
		t.Fatalf("register verifier: %v", err)
	}
	if err := st.RecordIndependentVerification(store.IndependentVerificationRequest{
		VerifierID: verifier.VerifierID, JobID: jobs[0].JobID, RuleID: "installed-version",
		Command: "clawctl version", ObservedDigest: "sha256:" + digest, Passed: true,
		VerifiedAt: time.Now().UTC().Truncate(time.Second),
	}); err != nil {
		t.Fatalf("record independent evidence: %v", err)
	}
	current, err := st.GetVerifier(verifier.VerifierID)
	if err != nil {
		t.Fatalf("read verifier: %v", err)
	}
	if err := st.RevokeVerifier(verifier.VerifierID, current.Revision, time.Now().UTC()); err != nil {
		t.Fatalf("revoke verifier: %v", err)
	}

	detail, err := New(st).DeploymentDetail(created.DeploymentID, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	target := detail.Targets[0].Independent
	if target == nil || target.Verdict != string(store.IndependentProducerRevoked) ||
		target.Rows != 1 || target.LiveProducers != 0 {
		t.Fatalf("revoked producer target=%+v", target)
	}
	if detail.Independent.PassedTargets != 0 || detail.Independent.LiveProducers != 0 ||
		detail.Independent.OpenedTargets != 1 {
		t.Fatalf("rollup after revocation=%+v", detail.Independent)
	}
}

func TestTheBlockerLabelSaysTheseExactWords(t *testing.T) {
	tests := []struct {
		token string
		want  string
	}{
		{"deployment_not_paused", "deployment 目前狀態不允許這個動作"},
		{"deployment_not_retryable", "deployment 目前狀態不允許這個動作"},
		{"nonterminal_jobs", "仍有未終態工作單"},
		{"no_terminal_failure_targets", "沒有終態未成功且可 retry 的機器"},
		{"stable_promotion_locked", "stable promotion 安全閘門目前未通過"},
		{"invalid_material", "Hub 無法重新驗證原 deployment material"},
		{"material_unavailable", "Hub 無法重新驗證原 deployment material"},
		{"material_identity_changed", "Hub 無法重新驗證原 deployment material"},
		{"active_resource_deployment", "同一資源已有 active deployment"},
		{"no_opened_batch", "帳本沒有已開批次"},
		{"next_batch_empty", "沒有可開啟的 target"},
		{"no_included_targets", "沒有可開啟的 target"},
		{"target_snapshot_changed", "target snapshot 已變更"},
		{"control_revision_exhausted", "deployment 控制版本已達上限，不能再執行動作"},
		{"failed_batch_requires_explicit_skip", "plain Continue 拒絕失敗批次。請改用單獨標示的 skip failed batch，並留下理由。"},
		{"opened_batch_not_failed", "目前已開批次沒有失敗終態，不能 skip failed batch"},
		{"some_unknown_blocker", "目前安全條件不允許這個動作（some_unknown_blocker）"},
	}

	for _, test := range tests {
		t.Run(test.token, func(t *testing.T) {
			got := DeploymentBlockerLabel(test.token)
			if got != test.want {
				t.Errorf("token %q 拿到的句子是 %q，期望的句子是 %q", test.token, got, test.want)
			}
		})
	}
}
