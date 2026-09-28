package operator

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/artifact"
	"github.com/teddashh/AI-Intune/internal/deploy"
	"github.com/teddashh/AI-Intune/internal/operatorauth"
	"github.com/teddashh/AI-Intune/internal/store"
)

func TestDeploymentActionReasonUsesOneCanonicalBound(t *testing.T) {
	for _, value := range []string{"", "approved rollout", strings.Repeat("界", 166)} {
		if err := validateDeploymentActionReason(value); err != nil {
			t.Errorf("valid reason %q: %v", value, err)
		}
	}
	for _, value := range []string{" leading", "trailing ", "line\nbreak", strings.Repeat("x", 501), string([]byte{0xff})} {
		if err := validateDeploymentActionReason(value); !errors.Is(err, ErrInvalidDeploymentAction) {
			t.Errorf("invalid reason %q err=%v", value, err)
		}
	}
}

func TestDeploymentTransportRejectionOwnsItsAuditShape(t *testing.T) {
	tests := []struct {
		name        string
		action      DeploymentTransportRejectionAction
		deployment  string
		wantAction  store.AuditAction
		wantSubject string
	}{
		{"create", DeploymentTransportRejectionCreate, "ignored-create-id", store.AuditDeploymentCreate, "deployment create"},
		{"continue", DeploymentTransportRejectionContinue, "deployment-continue", store.AuditDeploymentContinue, "deployment-continue"},
		{"retry", DeploymentTransportRejectionRetry, "deployment-retry", store.AuditDeploymentRetry, "deployment-retry"},
		{"abandon", DeploymentTransportRejectionAbandon, "deployment-abandon", store.AuditDeploymentAbandon, "deployment-abandon"},
		{"invalid target", DeploymentTransportRejectionContinue, "../raw-target", store.AuditDeploymentContinue, "deployment action"},
	}
	wantDetail := store.OperatorTransportRejectionPrefix + "BAD_REQUEST: canonical request digest 無法取得"
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			st := newDeploymentOperatorStore(t)
			service := New(st)
			actor := Actor{
				SourceAddr: "100.64.0.10", WhoNode: "console-node", AuthSubject: "tailscale-user:1",
				AuthCapability: "admin", AuthDecision: "authorized", SourceKind: SourceKindWeb,
			}
			err := service.RecordDeploymentTransportRejection(DeploymentTransportRejectionRequest{
				Action: test.action, DeploymentID: test.deployment, Actor: actor,
			})
			if err != nil {
				t.Fatal(err)
			}
			entries, err := st.Audit("", 10)
			if err != nil || len(entries) != 1 {
				t.Fatalf("transport audit=%+v err=%v", entries, err)
			}
			entry := entries[0]
			if entry.Action != test.wantAction || entry.Subject != test.wantSubject || entry.OK ||
				entry.IdempotencyKey != "" || entry.RequestDigest != "" || entry.Reason != "" ||
				entry.Detail != wantDetail || entry.SourceAddr != actor.SourceAddr || entry.WhoNode != actor.WhoNode ||
				entry.AuthSubject != actor.AuthSubject || entry.AuthCapability != actor.AuthCapability ||
				entry.AuthDecision != actor.AuthDecision || entry.SourceKind != SourceKindWeb {
				t.Fatalf("transport audit shape=%+v", entry)
			}
		})
	}
}

func TestDeploymentTransportRejectionRejectsUnknownAction(t *testing.T) {
	st := newDeploymentOperatorStore(t)
	service := New(st)
	err := service.RecordDeploymentTransportRejection(DeploymentTransportRejectionRequest{
		Action: DeploymentTransportRejectionAction("free-form-action"), DeploymentID: "deployment-a",
	})
	if err == nil {
		t.Fatal("operator service accepted an unbounded deployment rejection action")
	}
	entries, auditErr := st.Audit("", 10)
	if auditErr != nil || len(entries) != 0 {
		t.Fatalf("invalid action wrote an audit row: %+v err=%v", entries, auditErr)
	}
}

func TestApplyDeploymentCreateReplayBypassesArtifactAndConflictsOnBody(t *testing.T) {
	st := newDeploymentOperatorStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	if err := prepareDeploymentObservationPolicy(st, now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	observeDeploymentMachine(t, st, "machine-a", "alpha", "canary", "24.15.0", deploymentTestVersion, now)
	artifactDir := t.TempDir()
	record := writeDeploymentArtifact(t, artifactDir, deploymentTestVersion, ">=24.15.0 <25", now.Add(-time.Hour))
	svc := NewWithArtifacts(st, artifactDir)
	plan := DeploymentCreatePreviewRequest{
		Channel: "canary", Version: deploymentTestVersion, ArtifactSHA256: record.SHA256,
		BatchSize: 1, ExecutionTimeoutSeconds: 600,
	}
	preview, err := svc.PreviewDeploymentCreate(plan, now)
	if err != nil {
		t.Fatal(err)
	}
	request := DeploymentCreateApplyRequest{
		DeploymentCreatePreviewRequest: plan, PreviewDigest: preview.PreviewDigest,
		ConfirmChannel: "canary", ConfirmVersion: deploymentTestVersion,
		Reason: "approved rollout", IdempotencyKey: "deployment-create-replay",
		Actor: verifiedDeploymentActor(),
	}
	first, err := svc.ApplyDeploymentCreate(request)
	if err != nil {
		t.Fatal(err)
	}
	if first.Replayed || first.ControlRevision != 0 || first.OpenedBatch != 1 || len(first.Jobs) != 1 ||
		first.Jobs[0].MachineID != "machine-a" {
		t.Fatalf("first=%+v", first)
	}
	var createdBy string
	if err := st.DB().QueryRow(`SELECT created_by FROM deployments WHERE deployment_id=?`, first.DeploymentID).Scan(&createdBy); err != nil {
		t.Fatal(err)
	}
	if createdBy != request.Actor.AuthSubject {
		t.Fatalf("created_by=%q want verified actor %q", createdBy, request.Actor.AuthSubject)
	}
	assertDeploymentJSONSafe(t, first)

	// A historical replay must run before both today's artifact validation and
	// today's deployment/target planning. The artifact is deliberately broken.
	if err := os.WriteFile(filepath.Join(artifactDir, record.SHA256+".tgz"), []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	replayed, err := svc.ApplyDeploymentCreate(request)
	if err != nil {
		t.Fatal(err)
	}
	if !replayed.Replayed || replayed.DeploymentID != first.DeploymentID || len(replayed.Jobs) != 1 ||
		replayed.Jobs[0].JobID != first.Jobs[0].JobID {
		t.Fatalf("replayed=%+v first=%+v", replayed, first)
	}

	conflict := request
	conflict.Reason = "different approval statement"
	_, err = svc.ApplyDeploymentCreate(conflict)
	assertDeploymentOperatorCode(t, err, store.OperatorCodeIdempotencyConflict, false)
}

func TestApplyDeploymentCreateStaleAndConfirmationRejectionsReplay(t *testing.T) {
	for _, test := range []struct {
		name     string
		mutate   func(*artifact.Sidecar)
		confirm  string
		wantCode string
	}{
		{
			name: "preview becomes stale", confirm: deploymentTestVersion,
			mutate:   func(record *artifact.Sidecar) { record.EnginesNode = ">=99 <100" },
			wantCode: store.OperatorCodeDeploymentPreviewStale,
		},
		{
			name: "typed version confirmation", confirm: "wrong-version",
			mutate:   func(*artifact.Sidecar) {},
			wantCode: store.OperatorCodeDeploymentConfirmationMismatch,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			st := newDeploymentOperatorStore(t)
			now := time.Now().UTC().Truncate(time.Second)
			if err := prepareDeploymentObservationPolicy(st, now.Add(-time.Hour)); err != nil {
				t.Fatal(err)
			}
			observeDeploymentMachine(t, st, "machine-a", "alpha", "canary", "24.15.0", deploymentTestVersion, now)
			artifactDir := t.TempDir()
			record := writeDeploymentArtifact(t, artifactDir, deploymentTestVersion, ">=24.15.0 <25", now.Add(-time.Hour))
			svc := NewWithArtifacts(st, artifactDir)
			plan := DeploymentCreatePreviewRequest{
				Channel: "canary", Version: deploymentTestVersion, ArtifactSHA256: record.SHA256,
				BatchSize: 1, ExecutionTimeoutSeconds: 600,
			}
			preview, err := svc.PreviewDeploymentCreate(plan, now)
			if err != nil {
				t.Fatal(err)
			}
			test.mutate(&record)
			writeDeploymentSidecar(t, artifactDir, record)
			request := DeploymentCreateApplyRequest{
				DeploymentCreatePreviewRequest: plan, PreviewDigest: preview.PreviewDigest,
				ConfirmChannel: "canary", ConfirmVersion: test.confirm,
				Reason: "rejection must be replayable", IdempotencyKey: "deployment-create-rejection",
				Actor: verifiedDeploymentActor(),
			}
			_, err = svc.ApplyDeploymentCreate(request)
			assertDeploymentOperatorCode(t, err, test.wantCode, false)
			if err := os.Remove(filepath.Join(artifactDir, record.SHA256+".tgz")); err != nil {
				t.Fatal(err)
			}
			_, err = svc.ApplyDeploymentCreate(request)
			assertDeploymentOperatorCode(t, err, test.wantCode, true)
		})
	}
}

func TestDeploymentContinuePreviewApplyAndReplayDoNotReopenBatch(t *testing.T) {
	svc, st, artifactDir, record, parent, jobs := deploymentActionFixture(t, 2, 1)
	markDeploymentJobTerminal(t, st, jobs[0].JobID, deploy.Failed)
	pauseDeploymentForAction(t, st, parent.DeploymentID)
	preview, err := svc.PreviewDeploymentContinue(DeploymentContinuePreviewRequest{DeploymentID: parent.DeploymentID}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if !preview.Eligibility.Eligible || preview.Deployment.ControlRevision != 1 || preview.Deployment.OpenedBatch != 1 ||
		len(preview.Targets) != 1 || preview.Targets[0].MachineID != "machine-b" || preview.Artifact == nil {
		t.Fatalf("preview=%+v", preview)
	}
	assertDeploymentJSONSafe(t, preview)
	revision, opened := preview.Deployment.ControlRevision, preview.Deployment.OpenedBatch
	request := DeploymentContinueApplyRequest{
		DeploymentID: parent.DeploymentID, PreviewDigest: preview.PreviewDigest,
		ExpectedControlRevision: &revision, ExpectedOpenedBatch: &opened,
		ConfirmChannel: "canary", Reason: "continue after boundary review",
		IdempotencyKey: "deployment-continue", Actor: verifiedDeploymentActor(),
	}
	first, err := svc.ApplyDeploymentContinue(request)
	if err != nil {
		t.Fatal(err)
	}
	if first.ControlRevision != 2 || first.OpenedBatch != 2 || len(first.Jobs) != 1 || first.Jobs[0].MachineID != "machine-b" {
		t.Fatalf("continued=%+v", first)
	}
	if err := os.Remove(filepath.Join(artifactDir, record.SHA256+".tgz")); err != nil {
		t.Fatal(err)
	}
	replayed, err := svc.ApplyDeploymentContinue(request)
	if err != nil {
		t.Fatal(err)
	}
	if !replayed.Replayed || replayed.ControlRevision != 2 || len(replayed.Jobs) != 1 ||
		replayed.Jobs[0].JobID != first.Jobs[0].JobID {
		t.Fatalf("replayed=%+v", replayed)
	}
}

func TestDeploymentContinuePreviewBlocksSplitAuthorityJobTemplate(t *testing.T) {
	svc, st, _, _, parent, jobs := deploymentActionFixture(t, 2, 1)
	markDeploymentJobTerminal(t, st, jobs[0].JobID, deploy.Failed)
	pauseDeploymentForAction(t, st, parent.DeploymentID)
	eligiblePreview, err := svc.PreviewDeploymentContinue(
		DeploymentContinuePreviewRequest{DeploymentID: parent.DeploymentID}, time.Now().UTC())
	if err != nil || !eligiblePreview.Eligibility.Eligible {
		t.Fatalf("eligible preview=%+v err=%v", eligiblePreview, err)
	}

	// Model a legacy ledger changing after the operator received an eligible
	// preview. control_revision and opened_batch do not change, so the material
	// identity must independently invalidate that preview.
	if _, err := st.DB().Exec(`UPDATE jobs SET artifact_digest=? WHERE job_id=?`,
		"sha256:"+strings.Repeat("6", 64), jobs[0].JobID); err != nil {
		t.Fatal(err)
	}
	blockedPreview, err := svc.PreviewDeploymentContinue(
		DeploymentContinuePreviewRequest{DeploymentID: parent.DeploymentID}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if blockedPreview.Eligibility.Eligible ||
		!containsActionBlocker(blockedPreview.Eligibility.Blockers, "material_identity_changed") ||
		blockedPreview.Artifact == nil || len(blockedPreview.Targets) != 1 ||
		blockedPreview.PreviewDigest == eligiblePreview.PreviewDigest {
		t.Fatalf("eligible=%+v split-authority=%+v", eligiblePreview, blockedPreview)
	}
	before, err := st.Deployment(parent.DeploymentID)
	if err != nil {
		t.Fatal(err)
	}
	var beforeJobs, beforeContinues int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM jobs`).Scan(&beforeJobs); err != nil {
		t.Fatal(err)
	}
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM hub_events WHERE kind=?`,
		store.HubDeploymentContinued).Scan(&beforeContinues); err != nil {
		t.Fatal(err)
	}
	revision, opened := eligiblePreview.Deployment.ControlRevision, eligiblePreview.Deployment.OpenedBatch
	_, err = svc.ApplyDeploymentContinue(DeploymentContinueApplyRequest{
		DeploymentID: parent.DeploymentID, PreviewDigest: eligiblePreview.PreviewDigest,
		ExpectedControlRevision: &revision, ExpectedOpenedBatch: &opened,
		ConfirmChannel: "canary", Reason: "must not copy mismatched job material",
		IdempotencyKey: "deployment-continue-split-material", Actor: verifiedDeploymentActor(),
	})
	assertDeploymentOperatorCode(t, err, store.OperatorCodeDeploymentPreviewStale, false)
	after, err := st.Deployment(parent.DeploymentID)
	if err != nil || after.State != before.State || after.ControlRevision != before.ControlRevision ||
		after.PausedAt == nil || before.PausedAt == nil || !after.PausedAt.Equal(*before.PausedAt) {
		t.Fatalf("rejected Continue changed deployment: before=%+v after=%+v err=%v", before, after, err)
	}
	targets, err := st.DeploymentTargets(parent.DeploymentID)
	if err != nil || len(targets) != 2 || targets[1].JobID != "" {
		t.Fatalf("rejected Continue opened next target: targets=%+v err=%v", targets, err)
	}
	var afterJobs, afterContinues int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM jobs`).Scan(&afterJobs); err != nil {
		t.Fatal(err)
	}
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM hub_events WHERE kind=?`,
		store.HubDeploymentContinued).Scan(&afterContinues); err != nil {
		t.Fatal(err)
	}
	if afterJobs != beforeJobs || afterContinues != beforeContinues {
		t.Fatalf("rejected Continue jobs/events changed: jobs=%d/%d continues=%d/%d",
			afterJobs, beforeJobs, afterContinues, beforeContinues)
	}
}

func TestDeploymentFinishOnlyContinueDoesNotRequireArtifactBytes(t *testing.T) {
	svc, st, artifactDir, record, parent, jobs := deploymentActionFixture(t, 1, 1)
	markDeploymentJobTerminal(t, st, jobs[0].JobID, deploy.Failed)
	pauseDeploymentForAction(t, st, parent.DeploymentID)
	if err := os.Remove(filepath.Join(artifactDir, record.SHA256+".tgz")); err != nil {
		t.Fatal(err)
	}

	preview, err := svc.PreviewDeploymentContinue(
		DeploymentContinuePreviewRequest{DeploymentID: parent.DeploymentID}, time.Now().UTC())
	if err != nil || !preview.Eligibility.Eligible || preview.Eligibility.Outcome != "finish" ||
		preview.Artifact != nil || len(preview.Targets) != 0 {
		t.Fatalf("finish-only preview=%+v err=%v", preview, err)
	}
	revision, opened := preview.Deployment.ControlRevision, preview.Deployment.OpenedBatch
	result, err := svc.ApplyDeploymentContinue(DeploymentContinueApplyRequest{
		DeploymentID: parent.DeploymentID, PreviewDigest: preview.PreviewDigest,
		ExpectedControlRevision: &revision, ExpectedOpenedBatch: &opened,
		ConfirmChannel: "canary", Reason: "finish terminal deployment without material lookup",
		IdempotencyKey: "deployment-continue-finish-only", Actor: verifiedDeploymentActor(),
	})
	if err != nil || result.State != store.DeploymentFinished || result.ControlRevision != revision+1 ||
		result.OpenedBatch != opened || len(result.Jobs) != 0 || result.FinishedAt == nil {
		t.Fatalf("finish-only result=%+v err=%v", result, err)
	}
}

func TestDeploymentRetryBindsExactTerminalFailuresAndRejectsStaleJobState(t *testing.T) {
	svc, st, _, _, parent, jobs := deploymentActionFixture(t, 2, 2)
	markDeploymentJobTerminal(t, st, jobs[0].JobID, deploy.Failed)
	markDeploymentJobTerminal(t, st, jobs[1].JobID, deploy.Succeeded)
	pauseDeploymentForAction(t, st, parent.DeploymentID)

	stalePreview, err := svc.PreviewDeploymentRetry(DeploymentRetryPreviewRequest{DeploymentID: parent.DeploymentID}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if !stalePreview.Eligibility.Eligible || len(stalePreview.TerminalFailureTargets) != 1 ||
		stalePreview.TerminalFailureTargets[0].MachineID != jobs[0].MachineID || len(stalePreview.Targets) != 1 {
		t.Fatalf("retry preview=%+v", stalePreview)
	}
	// A terminal state transition does not alter control_revision, so the
	// preview digest must independently bind the exact terminal evidence.
	markDeploymentJobTerminal(t, st, jobs[0].JobID, deploy.Rejected)
	revision, opened := stalePreview.Deployment.ControlRevision, stalePreview.Deployment.OpenedBatch
	staleRequest := DeploymentRetryApplyRequest{
		DeploymentID: parent.DeploymentID, PreviewDigest: stalePreview.PreviewDigest,
		ExpectedControlRevision: &revision, ExpectedOpenedBatch: &opened,
		ConfirmChannel: "canary", ConfirmVersion: deploymentTestVersion,
		Reason: "retry exact failed target", IdempotencyKey: "deployment-retry-stale",
		Actor: verifiedDeploymentActor(),
	}
	_, err = svc.ApplyDeploymentRetry(staleRequest)
	assertDeploymentOperatorCode(t, err, store.OperatorCodeDeploymentPreviewStale, false)

	preview, err := svc.PreviewDeploymentRetry(DeploymentRetryPreviewRequest{DeploymentID: parent.DeploymentID}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	revision, opened = preview.Deployment.ControlRevision, preview.Deployment.OpenedBatch
	request := staleRequest
	request.PreviewDigest = preview.PreviewDigest
	request.ExpectedControlRevision, request.ExpectedOpenedBatch = &revision, &opened
	request.IdempotencyKey = "deployment-retry-fresh"
	result, err := svc.ApplyDeploymentRetry(request)
	if err != nil {
		t.Fatal(err)
	}
	if result.RetryOf == nil || *result.RetryOf != parent.DeploymentID || result.ControlRevision != 0 ||
		result.OpenedBatch != 1 || len(result.Jobs) != 1 || result.Jobs[0].MachineID != jobs[0].MachineID {
		t.Fatalf("retry result=%+v", result)
	}
	parentView, err := st.DeploymentView(parent.DeploymentID, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if parentView.State != store.DeploymentFinished || parentView.ControlRevision != revision+1 {
		t.Fatalf("parent after retry=%+v", parentView.Deployment)
	}
}

func TestDeploymentAbandonUsesTypedIDConfirmationWithoutArtifactLookup(t *testing.T) {
	svc, st, artifactDir, record, parent, jobs := deploymentActionFixture(t, 2, 1)
	markDeploymentJobTerminal(t, st, jobs[0].JobID, deploy.Failed)
	pauseDeploymentForAction(t, st, parent.DeploymentID)
	preview, err := svc.PreviewDeploymentAbandon(DeploymentAbandonPreviewRequest{DeploymentID: parent.DeploymentID}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if !preview.Eligibility.Eligible || preview.Artifact != nil || len(preview.Targets) != 1 {
		t.Fatalf("abandon preview=%+v", preview)
	}
	revision, opened := preview.Deployment.ControlRevision, preview.Deployment.OpenedBatch
	bad := DeploymentAbandonApplyRequest{
		DeploymentID: parent.DeploymentID, PreviewDigest: preview.PreviewDigest,
		ExpectedControlRevision: &revision, ExpectedOpenedBatch: &opened,
		ConfirmDeploymentID: "wrong-deployment", Reason: "abandon unopened batch",
		IdempotencyKey: "deployment-abandon-bad-confirm", Actor: verifiedDeploymentActor(),
	}
	_, err = svc.ApplyDeploymentAbandon(bad)
	assertDeploymentOperatorCode(t, err, store.OperatorCodeDeploymentConfirmationMismatch, false)

	// Abandon changes only lifecycle authority and does not need artifact bytes.
	if err := os.Remove(filepath.Join(artifactDir, record.SHA256+".tgz")); err != nil {
		t.Fatal(err)
	}
	good := bad
	good.ConfirmDeploymentID = parent.DeploymentID
	good.IdempotencyKey = "deployment-abandon-good"
	result, err := svc.ApplyDeploymentAbandon(good)
	if err != nil {
		t.Fatal(err)
	}
	if result.State != store.DeploymentFinished || result.ControlRevision != revision+1 || result.OpenedBatch != opened || len(result.Jobs) != 0 {
		t.Fatalf("abandon result=%+v", result)
	}
	replayed, err := svc.ApplyDeploymentAbandon(good)
	if err != nil || !replayed.Replayed || replayed.ControlRevision != result.ControlRevision {
		t.Fatalf("abandon replay=%+v err=%v", replayed, err)
	}
}

func TestProjectDeploymentMutationRejectsZeroOpenedBatch(t *testing.T) {
	now := time.Date(2026, 9, 8, 19, 30, 0, 0, time.UTC)
	receipt := store.OperatorDeploymentResult{
		Deployment: store.Deployment{
			DeploymentID: "deployment-projection", Channel: "canary", DesiredID: "desired-projection",
			ResourceKind: "openclaw", ResourceID: "openclaw", Revision: 1, BatchSize: 1,
			State: store.DeploymentRunning, CreatedAt: now, CreatedBy: "operator-test",
		},
		Jobs: []store.Job{}, OpenedBatch: 1, PreviewDigest: "sha256:" + strings.Repeat("a", 64),
	}
	if _, err := projectDeploymentMutation("create", receipt); err != nil {
		t.Fatalf("canonical mutation projection failed: %v", err)
	}
	receipt.OpenedBatch = 0
	if _, err := projectDeploymentMutation("create", receipt); err == nil {
		t.Fatal("mutation projection accepted opened_batch=0")
	}
}

func deploymentActionFixture(t *testing.T, machineCount, batchSize int) (*Service, *store.Store, string, artifact.Sidecar, DeploymentMutationResult, []DeploymentMutationJob) {
	t.Helper()
	st := newDeploymentOperatorStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	if err := prepareDeploymentObservationPolicy(st, now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < machineCount; i++ {
		id := "machine-" + string(rune('a'+i))
		name := "node-" + string(rune('a'+i))
		observeDeploymentMachine(t, st, id, name, "canary", "24.15.0", deploymentTestVersion, now)
	}
	artifactDir := t.TempDir()
	record := writeDeploymentArtifact(t, artifactDir, deploymentTestVersion, ">=24.15.0 <25", now.Add(-time.Hour))
	svc := NewWithArtifacts(st, artifactDir)
	plan := DeploymentCreatePreviewRequest{
		Channel: "canary", Version: deploymentTestVersion, ArtifactSHA256: record.SHA256,
		BatchSize: batchSize, ExecutionTimeoutSeconds: 600,
	}
	preview, err := svc.PreviewDeploymentCreate(plan, now)
	if err != nil {
		t.Fatal(err)
	}
	created, err := svc.ApplyDeploymentCreate(DeploymentCreateApplyRequest{
		DeploymentCreatePreviewRequest: plan, PreviewDigest: preview.PreviewDigest,
		ConfirmChannel: "canary", ConfirmVersion: deploymentTestVersion,
		Reason: "fixture", IdempotencyKey: "fixture-create", Actor: verifiedDeploymentActor(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return svc, st, artifactDir, record, created, created.Jobs
}

func markDeploymentJobTerminal(t *testing.T, st *store.Store, jobID string, state deploy.JobState) {
	t.Helper()
	at := time.Now().UTC().Truncate(time.Second)
	if _, err := st.DB().Exec(`UPDATE jobs SET state=?,terminal_at=?,lease_token=NULL,lease_expires_at=NULL WHERE job_id=?`,
		state, at.Format(time.RFC3339Nano), jobID); err != nil {
		t.Fatal(err)
	}
}

func pauseDeploymentForAction(t *testing.T, st *store.Store, deploymentID string) {
	t.Helper()
	changed, err := st.SetDeploymentState(deploymentID, store.DeploymentRunning, store.DeploymentPaused, time.Now().UTC())
	if err != nil || !changed {
		t.Fatalf("pause deployment changed=%t err=%v", changed, err)
	}
}

func verifiedDeploymentActor() Actor {
	return Actor{
		AuthSubject: "user:test@example.com", AuthDecision: string(operatorauth.Authorized),
		AuthCapability: "example.com/cap/clawctl-operate", AuthMethod: operatorauth.AuthMethodLocalAPI,
		SourceKind: SourceKindOperatorAPI, WhoUser: "test@example.com", WhoNode: "test-node",
	}
}

func assertDeploymentOperatorCode(t *testing.T, err error, code string, replayed bool) {
	t.Helper()
	var rejection *store.OperatorRequestError
	if !errors.As(err, &rejection) || rejection.Code != code || rejection.Replayed != replayed {
		t.Fatalf("error=%v rejection=%+v want code=%s replayed=%t", err, rejection, code, replayed)
	}
}

func writeDeploymentSidecar(t *testing.T, dir string, record artifact.Sidecar) {
	t.Helper()
	raw, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, record.SHA256+".json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// Preview 說不行後，operator 若仍送出同一份未變更的 digest，Hub 必須用
// 409「仍有未終態 job」拒絕，而不是 412「請重新預覽」。STALE 的語意是
// snapshot 已經改變，而這裡的 snapshot 並沒有變。
func TestContinuingPastALiveJobIsRefusedAsActiveJobsNotAsAStalePreview(t *testing.T) {
	svc, st, _, _, parent, jobs := deploymentActionFixture(t, 3, 2)
	markDeploymentJobTerminal(t, st, jobs[0].JobID, deploy.Failed)
	pauseDeploymentForAction(t, st, parent.DeploymentID)

	preview, err := svc.PreviewDeploymentContinue(
		DeploymentContinuePreviewRequest{DeploymentID: parent.DeploymentID}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if preview.Eligibility.Eligible || len(preview.Eligibility.Blockers) != 1 ||
		preview.Eligibility.Blockers[0] != "nonterminal_jobs" {
		t.Fatalf("precondition does not isolate nonterminal_jobs, so apply would not measure that blocker mapping: eligibility=%+v", preview.Eligibility)
	}

	revision, opened := preview.Deployment.ControlRevision, preview.Deployment.OpenedBatch
	_, err = svc.ApplyDeploymentContinue(DeploymentContinueApplyRequest{
		DeploymentID: parent.DeploymentID, PreviewDigest: preview.PreviewDigest,
		ExpectedControlRevision: &revision, ExpectedOpenedBatch: &opened,
		ConfirmChannel: "canary", Reason: "refuse continue past active job",
		IdempotencyKey: "deployment-continue-active-job", Actor: verifiedDeploymentActor(),
	})
	var rejection *store.OperatorRequestError
	if !errors.As(err, &rejection) || rejection.Code != store.OperatorCodeDeploymentActiveJobs || rejection.Replayed {
		t.Errorf("continue rejection=%+v err=%v; expected code=%s replayed=false: 409 means wait for active jobs, while 412 sends the operator into an unchanged re-preview loop",
			rejection, err, store.OperatorCodeDeploymentActiveJobs)
	}

	view, err := st.DeploymentView(parent.DeploymentID, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if view.State != store.DeploymentPaused || view.ControlRevision != revision || view.OpenedBatch != opened {
		t.Errorf("deployment after rejected continue state=%s control_revision=%d opened_batch=%d; expected state=%s control_revision=%d opened_batch=%d: a 409 receipt cannot claim refusal after new machines were started",
			view.State, view.ControlRevision, view.OpenedBatch, store.DeploymentPaused, revision, opened)
	}
}

// 三個控制動作共用同一支錯誤映射；deployment 不在了必須回覆「找不到」，
// 而不是「你的預覽過期了」。因為 deployment 已經消失，重新預覽也不會把
// 它變回來，operator 必須回到名冊重新尋找。
func TestControlActionsOnAMissingDeploymentSayNotFoundNotStalePreview(t *testing.T) {
	svc, st, _, _, parent, jobs := deploymentActionFixture(t, 2, 1)
	markDeploymentJobTerminal(t, st, jobs[0].JobID, deploy.Failed)
	pauseDeploymentForAction(t, st, parent.DeploymentID)

	preview, err := svc.PreviewDeploymentContinue(
		DeploymentContinuePreviewRequest{DeploymentID: parent.DeploymentID}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if !preview.Eligibility.Eligible {
		t.Fatalf("precondition is not eligible, so applying to a missing deployment would not isolate the not-found mapping: eligibility=%+v", preview.Eligibility)
	}
	if _, err := st.DeploymentView(parent.DeploymentID, time.Now().UTC()); err != nil {
		t.Fatalf("precondition cannot read the existing deployment from the store, so the cases below would not isolate a missing deployment: %v", err)
	}

	revision, opened := preview.Deployment.ControlRevision, preview.Deployment.OpenedBatch
	missing := parent.DeploymentID + "-pruned"
	tests := []struct {
		name  string
		apply func() error
	}{
		{
			name: "continue",
			apply: func() error {
				_, err := svc.ApplyDeploymentContinue(DeploymentContinueApplyRequest{
					DeploymentID: missing, PreviewDigest: preview.PreviewDigest,
					ExpectedControlRevision: &revision, ExpectedOpenedBatch: &opened,
					ConfirmChannel: "canary", Reason: "continue missing deployment",
					IdempotencyKey: "deployment-continue-missing", Actor: verifiedDeploymentActor(),
				})
				return err
			},
		},
		{
			name: "retry",
			apply: func() error {
				_, err := svc.ApplyDeploymentRetry(DeploymentRetryApplyRequest{
					DeploymentID: missing, PreviewDigest: preview.PreviewDigest,
					ExpectedControlRevision: &revision, ExpectedOpenedBatch: &opened,
					ConfirmChannel: "canary", ConfirmVersion: deploymentTestVersion,
					Reason: "retry missing deployment", IdempotencyKey: "deployment-retry-missing",
					Actor: verifiedDeploymentActor(),
				})
				return err
			},
		},
		{
			name: "abandon",
			apply: func() error {
				_, err := svc.ApplyDeploymentAbandon(DeploymentAbandonApplyRequest{
					DeploymentID: missing, PreviewDigest: preview.PreviewDigest,
					ExpectedControlRevision: &revision, ExpectedOpenedBatch: &opened,
					ConfirmDeploymentID: missing, Reason: "abandon missing deployment",
					IdempotencyKey: "deployment-abandon-missing", Actor: verifiedDeploymentActor(),
				})
				return err
			},
		},
	}

	for _, tt := range tests {
		err := tt.apply()
		var rejection *store.OperatorRequestError
		if !errors.As(err, &rejection) || rejection.Code != store.OperatorCodeDeploymentNotFound ||
			rejection.Replayed {
			t.Errorf("%s rejection=%+v err=%v; expected code=%s replayed=false: 404 means the deployment is gone and the operator must return to the roster, while 412 means re-preview; automation that retries 412 would retry forever for a deployment that will never return",
				tt.name, rejection, err, store.OperatorCodeDeploymentNotFound)
		}
	}
}

// Preview 已經精確說明沒有可排進計畫的機器，同一份 digest apply
// 必須保留這個機隊事實。412 會誤導 operator 重新預覽，並讓自動化
// 在完全沒有變動的 snapshot 上無限重試。
func TestDeploymentApplyWithNoIncludedTargetsSaysSoInsteadOfStalePreview(t *testing.T) {
	st := newDeploymentOperatorStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	if err := prepareDeploymentObservationPolicy(st, now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	artifactDir := t.TempDir()
	record := writeDeploymentArtifact(t, artifactDir, deploymentTestVersion, ">=24.15.0 <25", now.Add(-time.Hour))
	svc := NewWithArtifacts(st, artifactDir)
	plan := DeploymentCreatePreviewRequest{
		Channel: "canary", Version: deploymentTestVersion, ArtifactSHA256: record.SHA256,
		BatchSize: 1, ExecutionTimeoutSeconds: 600,
	}
	preview, err := svc.PreviewDeploymentCreate(plan, now)
	if err != nil {
		t.Fatal(err)
	}
	if preview.CreateAllowed || len(preview.Blockers) != 1 || preview.Blockers[0] != "no_included_targets" {
		t.Fatalf("precondition does not isolate no_included_targets, so apply would not measure that blocker mapping: create_allowed=%t blockers=%v",
			preview.CreateAllowed, preview.Blockers)
	}

	_, err = svc.ApplyDeploymentCreate(DeploymentCreateApplyRequest{
		DeploymentCreatePreviewRequest: plan, PreviewDigest: preview.PreviewDigest,
		ConfirmChannel: "canary", ConfirmVersion: deploymentTestVersion,
		Reason: "apply plan with no included targets", IdempotencyKey: "deployment-create-no-included-targets",
		Actor: verifiedDeploymentActor(),
	})
	var rejection *store.OperatorRequestError
	if !errors.As(err, &rejection) || rejection.Code != store.OperatorCodeDeploymentNoIncludedTargets ||
		rejection.Replayed {
		t.Errorf("create rejection=%+v err=%v; expected code=%s replayed=false: 409 reports the fleet fact so the operator can address the previewed exclusion reasons; 412 claims a stale snapshot even though the digest matches, and automation that retries 412 would loop forever on the same blocker",
			rejection, err, store.OperatorCodeDeploymentNoIncludedTargets)
	}
	wantDetail := "目前沒有可排進計畫的機器；請處理排除原因後再預覽"
	if rejection != nil && rejection.Detail != wantDetail {
		t.Errorf("rejection.Detail=%q; expected %q: this is the line the operator actually reads in Web and JSON; it must point to actionable exclusion reasons instead of telling them to repeat a preview that cannot change the decision",
			rejection.Detail, wantDetail)
	}
	status, code, _ := HTTPError(err)
	if status != http.StatusConflict || code != store.OperatorCodeDeploymentNoIncludedTargets {
		t.Errorf("HTTPError status=%d code=%q; expected status=%d code=%q: 409 is a fleet fact, so automation must not retry; 412 would make automation retry forever against a completely unchanged snapshot",
			status, code, http.StatusConflict, store.OperatorCodeDeploymentNoIncludedTargets)
	}
}

// deploymentCreatePrepareError 的兩臂搶同一個 ErrInvalidDeploymentPreview；
// channel 不是 canary／stable 時，normalize 回的就是這顆 sentinel。因此
// 「channel 那一臂排在前面」本身就是契約；對調兩臂，壞 channel 會變成
// 412「請重新預覽」。400 表示「這份 body 寫錯了，改完再送」，412 表示
// 「snapshot 變了」；這裡 snapshot 根本沒變，重新預覽只會拿到同一個錯。
func TestCreateApplyWithAnUnsupportedChannelSaysBadChannelNotStalePreview(t *testing.T) {
	st := newDeploymentOperatorStore(t)
	svc := NewWithArtifacts(st, t.TempDir())
	now := time.Now().UTC().Truncate(time.Second)
	tests := []struct {
		channel        string
		idempotencyKey string
	}{
		{channel: "prod", idempotencyKey: "deployment-create-unsupported-channel-prod"},
		{channel: "Canary", idempotencyKey: "deployment-create-unsupported-channel-case"},
		{channel: " canary", idempotencyKey: "deployment-create-unsupported-channel-space"},
	}

	for _, tt := range tests {
		plan := DeploymentCreatePreviewRequest{
			Channel: tt.channel, Version: deploymentTestVersion,
			BatchSize: 1, ExecutionTimeoutSeconds: 600,
		}
		_, previewErr := svc.PreviewDeploymentCreate(plan, now)
		if !errors.Is(previewErr, ErrInvalidDeploymentPreview) {
			t.Fatalf("channel=%q preview error=%v; expected ErrInvalidDeploymentPreview: this test measures which rejection wins when two branches compete for that exact error; without it apply does not isolate BAD_CHANNEL (fix the body and resend) from a false 412 stale-preview instruction that would make automation re-preview the unchanged snapshot and retry the same bad body forever",
				tt.channel, previewErr)
		}

		_, err := svc.ApplyDeploymentCreate(DeploymentCreateApplyRequest{
			DeploymentCreatePreviewRequest: plan,
			PreviewDigest:                  "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			ConfirmChannel:                 tt.channel,
			ConfirmVersion:                 deploymentTestVersion,
			Reason:                         "refuse an unsupported deployment channel",
			IdempotencyKey:                 tt.idempotencyKey,
			Actor:                          verifiedDeploymentActor(),
		})
		var rejection *store.OperatorRequestError
		if !errors.As(err, &rejection) || rejection.Code != store.OperatorCodeBadChannel ||
			rejection.Replayed {
			t.Errorf("channel=%q rejection=%+v err=%v; expected code=%s replayed=false: 400 BAD_CHANNEL says this body is wrong and the channel must be changed before resending; 412 DEPLOYMENT_PREVIEW_STALE falsely says to re-preview, which returns the same error because the snapshot did not change and makes retry automation submit the same bad body forever",
				tt.channel, rejection, err, store.OperatorCodeBadChannel)
		}

		wantDetail := "deployment channel 只接受 canary 或 stable"
		if rejection != nil && rejection.Detail != wantDetail {
			t.Errorf("channel=%q rejection.Detail=%q; expected %q: this is the line the operator actually reads in Web and JSON, so it must name the valid channel values; otherwise the operator cannot know how to fix the bad body, while a stale-preview message would cause an unchanged re-preview and endless retries",
				tt.channel, rejection.Detail, wantDetail)
		}

		status, code, _ := HTTPError(err)
		if status != http.StatusBadRequest || code != store.OperatorCodeBadChannel {
			t.Errorf("channel=%q HTTPError status=%d code=%q; expected status=%d code=%q: the status is the only signal automation reads, so 400 must stop retries and require a corrected channel; 412 would tell it to re-preview an unchanged snapshot and retry the same bad body forever",
				tt.channel, status, code, http.StatusBadRequest, store.OperatorCodeBadChannel)
		}
	}
}
