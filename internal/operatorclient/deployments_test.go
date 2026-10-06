package operatorclient

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/deploy"
	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/store"
)

func deploymentClientTestTime(hour, minute int) time.Time {
	return time.Date(2026, 9, 8, hour, minute, 0, 0, time.UTC)
}

func deploymentClientTestPtr[T any](value T) *T {
	return &value
}

func deploymentClientTestCounts(state deploy.JobState) []operator.JobStateCount {
	counts := make([]operator.JobStateCount, 0, len(deploy.AllJobStates))
	for _, candidate := range deploy.AllJobStates {
		count := 0
		if candidate == state {
			count = 1
		}
		counts = append(counts, operator.JobStateCount{State: candidate, Count: count})
	}
	return counts
}

func deploymentClientTestSummary() operator.DeploymentSummary {
	version := "2026.9.2"
	digest := "sha256:" + strings.Repeat("a", 64)
	size := int64(4096)
	pausedAt := deploymentClientTestTime(12, 4)
	return operator.DeploymentSummary{
		DeploymentID:    "deployment-1",
		Channel:         "canary",
		DesiredID:       "desired-1",
		ResourceKind:    "openclaw",
		ResourceID:      "openclaw",
		DesiredRevision: 7,
		ControlRevision: 2,
		BatchSize:       1,
		State:           store.DeploymentPaused,
		CreatedAt:       deploymentClientTestTime(12, 0),
		PausedAt:        &pausedAt,
		Attempt:         1,
		OpenedBatch:     1,
		TotalBatches:    2,
		Stuck:           1,
		TerminalStuck:   1,
		SilentStuck:     0,
		JobStateCounts:  deploymentClientTestCounts(deploy.Failed),
		Material: operator.DeploymentMaterialSummary{
			Status: operator.DeploymentMaterialRecorded, Kind: "openclaw",
			Version: &version, ArtifactDigest: &digest, SizeBytes: &size,
			// The server deliberately uses null, not an empty string pointer, when
			// package metadata has no engines.node requirement.
			EnginesNode: nil,
		},
	}
}

func deploymentClientTestList() operator.DeploymentListResult {
	return operator.DeploymentListResult{
		SchemaVersion: operator.DeploymentReadSchemaVersion,
		Consistency:   operator.DeploymentReadConsistencyLive,
		EvaluatedAt:   deploymentClientTestTime(13, 0),
		Total:         1,
		StateCounts: []operator.DeploymentStateCount{
			{State: store.DeploymentRunning, Count: 0},
			{State: store.DeploymentPaused, Count: 1},
			{State: store.DeploymentFinished, Count: 0},
		},
		Items: []operator.DeploymentSummary{deploymentClientTestSummary()},
	}
}

func deploymentClientTestDetail() operator.DeploymentDetailResult {
	failed := deploy.Failed
	jobID := "job-full-identifier-1"
	createdAt := deploymentClientTestTime(12, 1)
	lastActivity := deploymentClientTestTime(12, 3)
	terminalAt := deploymentClientTestTime(12, 4)
	stuckKind := "terminal_failure"
	return operator.DeploymentDetailResult{
		SchemaVersion: operator.DeploymentReadSchemaVersion,
		Consistency:   operator.DeploymentReadConsistencyLive,
		EvaluatedAt:   deploymentClientTestTime(13, 0),
		Item:          deploymentClientTestSummary(),
		Targets: []operator.DeploymentTargetSummary{
			{
				MachineID: "machine-a", DisplayName: "alpha", BatchNo: 1,
				JobID: &jobID, JobState: &failed, JobCreatedAt: &createdAt,
				TerminalAt: &terminalAt, LastActivityAt: &lastActivity, StuckKind: &stuckKind,
				Independent: &operator.DeploymentTargetIndependent{Verdict: "absent"},
			},
			{MachineID: "machine-b", DisplayName: "beta", BatchNo: 2},
		},
		Independent: operator.DeploymentIndependentSummary{
			OpenedTargets: 1, PassedTargets: 0, LiveProducers: 0,
			Verdicts: []operator.DeploymentIndependentVerdictCount{
				{Verdict: "absent", Targets: 1}, {Verdict: "producer_revoked", Targets: 0},
				{Verdict: "digest_mismatch", Targets: 0}, {Verdict: "release_mismatch", Targets: 0},
				{Verdict: "stale", Targets: 0}, {Verdict: "failed", Targets: 0},
				{Verdict: "release_unreported", Targets: 0}, {Verdict: "passed", Targets: 0},
			},
		},
		Actions: operator.DeploymentActionEligibilitySet{
			Continue: operator.DeploymentActionEligibility{
				Outcome: "open_next_batch", AffectedTargets: 1,
				Blockers: []string{"failed_batch_requires_explicit_skip"},
			},
			SkipFailedBatch: operator.DeploymentActionEligibility{
				Eligible: true, Outcome: "open_next_batch", AffectedTargets: 1, Blockers: []string{},
			},
			Retry: operator.DeploymentActionEligibility{
				Eligible: true, Outcome: "create_retry_attempt", AffectedTargets: 1, Blockers: []string{},
			},
			Abandon: operator.DeploymentActionEligibility{
				Eligible: true, Outcome: "finish_without_unopened_batches", AffectedTargets: 1, Blockers: []string{},
			},
		},
	}
}

func deploymentClientTestPreviewRequest() operator.DeploymentCreatePreviewRequest {
	return operator.DeploymentCreatePreviewRequest{
		Channel: "canary", Version: "2026.9.2", ArtifactSHA256: strings.Repeat("a", 64),
		BatchSize: 1, ExecutionTimeoutSeconds: 600,
	}
}

func deploymentClientTestPreview() operator.DeploymentCreatePreviewResult {
	nodeVersion := "v20.0.0"
	conflict := "conflict"
	result := operator.DeploymentCreatePreviewResult{
		SchemaVersion:           operator.DeploymentPreviewSchemaVersion,
		PreviewedAt:             deploymentClientTestTime(13, 0),
		Channel:                 "canary",
		BatchSize:               1,
		ExecutionTimeoutSeconds: 600,
		Artifact: operator.DeploymentArtifactPreview{
			Name: "openclaw", Version: "2026.9.2", SHA256: strings.Repeat("a", 64),
			Digest: "sha256:" + strings.Repeat("a", 64), SizeBytes: 4096,
			EnginesNode: "", SHA512Integrity: "sha512-integrity-proof",
			FetchedAt: deploymentClientTestTime(11, 0), AvailableAndVerified: true,
		},
		Targets: []operator.DeploymentPlanTargetPreview{
			{MachineID: "machine-a", DisplayName: "alpha", Reachable: true, NodeVersion: &nodeVersion, BatchNo: 1},
			{MachineID: "machine-b", DisplayName: "beta", ExcludedReason: &conflict},
		},
		Impact: 1, Conflicts: 1, TotalBatches: 1,
		CreateAllowed: true, Blockers: []string{},
	}
	result.PreviewDigest = deploymentClientPreviewDigest(result)
	return result
}

func deploymentClientTestPromotion(state, next string, allowed bool) operator.DeploymentPromotionPreview {
	passed := 0
	blockers := []string{"stable_promotion_locked"}
	if allowed {
		passed = 1
		blockers = []string{}
	}
	return operator.DeploymentPromotionPreview{
		Allowed: allowed, Blockers: blockers, IndependentRequired: true,
		IndependentPassedTargets: passed,
		IndependentTargets: []operator.DeploymentPromotionIndependentTargetPreview{{
			MachineID: "machine-canary", DisplayName: "canary", JobID: "job-canary",
			State: state, NextStep: next,
		}},
	}
}

func TestDeploymentClientValidatesIndependentPromotionEvidence(t *testing.T) {
	request := deploymentClientTestPreviewRequest()
	request.Channel = "stable"
	canonical := deploymentClientTestPreview()
	canonical.Channel = "stable"
	promotion := deploymentClientTestPromotion("passed", operator.PromotionNextStepNone, true)
	canonical.Promotion = &promotion
	canonical.PreviewDigest = deploymentClientPreviewDigest(canonical)
	if err := validateDeploymentCreatePreviewResult(canonical, request); err != nil {
		t.Fatalf("canonical independent promotion rejected: %v", err)
	}
	for state, next := range map[string]string{
		"release_mismatch":   operator.PromotionNextStepRepairAndRerunCanary,
		"release_unreported": operator.PromotionNextStepUpgradeAndReassignVerifier,
	} {
		blocked := deploymentClientTestPromotion(state, next, false)
		result := canonical
		result.Promotion, result.CreateAllowed = &blocked, false
		result.Blockers = []string{"stable_promotion_locked"}
		result.PreviewDigest = deploymentClientPreviewDigest(result)
		if err := validateDeploymentCreatePreviewResult(result, request); err != nil {
			t.Fatalf("canonical %s promotion rejected: %v", state, err)
		}
	}

	for _, test := range []struct {
		name   string
		mutate func(*operator.DeploymentPromotionPreview)
	}{
		{"required flag", func(p *operator.DeploymentPromotionPreview) { p.IndependentRequired = false }},
		{"null targets", func(p *operator.DeploymentPromotionPreview) { p.IndependentTargets = nil }},
		{"passed tally", func(p *operator.DeploymentPromotionPreview) { p.IndependentPassedTargets = 0 }},
		{"unknown state", func(p *operator.DeploymentPromotionPreview) { p.IndependentTargets[0].State = "healthy" }},
		{"wrong next step", func(p *operator.DeploymentPromotionPreview) {
			p.IndependentTargets[0].NextStep = operator.PromotionNextStepRerunCanary
		}},
		{"missing job", func(p *operator.DeploymentPromotionPreview) { p.IndependentTargets[0].JobID = "" }},
		{"non-pass while allowed", func(p *operator.DeploymentPromotionPreview) {
			p.IndependentTargets[0].State = "unassigned"
			p.IndependentTargets[0].NextStep = operator.PromotionNextStepAssignVerifier
			p.IndependentPassedTargets = 0
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			result := canonical
			copyPromotion := *canonical.Promotion
			copyPromotion.IndependentTargets = append([]operator.DeploymentPromotionIndependentTargetPreview(nil),
				canonical.Promotion.IndependentTargets...)
			result.Promotion = &copyPromotion
			test.mutate(result.Promotion)
			result.PreviewDigest = deploymentClientPreviewDigest(result)
			if err := validateDeploymentCreatePreviewResult(result, request); err == nil {
				t.Fatalf("poisoned promotion accepted: %+v", result.Promotion)
			}
		})
	}
}

func deploymentClientTestJSON(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func deploymentClientTestResponseHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "private, no-store")
}

func TestDeploymentClientAcceptsCanonicalListDetailAndPreview(t *testing.T) {
	request := deploymentClientTestPreviewRequest()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") != "application/json" || r.UserAgent() != UserAgent {
			t.Errorf("request headers=%v", r.Header)
		}
		deploymentClientTestResponseHeaders(w)
		switch r.URL.Path {
		case "/v1/operator/deployments":
			query := r.URL.Query()
			if r.Method != http.MethodGet || query.Get("channel") != "canary" || query.Get("stuck") != "true" ||
				query.Get("limit") != "7" || !reflect.DeepEqual(query["state"], []string{store.DeploymentPaused}) {
				t.Errorf("deployment list request method=%s query=%v", r.Method, query)
			}
			_, _ = w.Write([]byte(deploymentClientTestJSON(t, deploymentClientTestList())))
		case "/v1/operator/deployments/deployment-1":
			if r.Method != http.MethodGet {
				t.Errorf("deployment detail method=%s", r.Method)
			}
			_, _ = w.Write([]byte(deploymentClientTestJSON(t, deploymentClientTestDetail())))
		case "/v1/operator/deployments/preview":
			if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" ||
				r.Header.Get("Idempotency-Key") != "" {
				t.Errorf("deployment preview method=%s headers=%v", r.Method, r.Header)
			}
			var got operator.DeploymentCreatePreviewRequest
			if err := json.NewDecoder(r.Body).Decode(&got); err != nil || !reflect.DeepEqual(got, request) {
				t.Errorf("deployment preview body=%+v err=%v", got, err)
			}
			_, _ = w.Write([]byte(deploymentClientTestJSON(t, deploymentClientTestPreview())))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := operatorClientForServer(t, server)
	stuck := true
	list, err := client.Deployments(t.Context(), operator.DeploymentListRequest{
		Channel: "canary", States: []string{store.DeploymentPaused}, Stuck: &stuck, Limit: 7,
	})
	if err != nil || len(list.Items) != 1 || list.Items[0].Material.EnginesNode != nil ||
		list.Items[0].ControlRevision != 2 {
		t.Fatalf("list=%+v err=%v", list, err)
	}
	detail, err := client.Deployment(t.Context(), "deployment-1")
	if err != nil || len(detail.Targets) != 2 || detail.Targets[0].JobID == nil ||
		*detail.Targets[0].JobID != "job-full-identifier-1" {
		t.Fatalf("detail=%+v err=%v", detail, err)
	}
	preview, err := client.PreviewDeploymentCreate(t.Context(), request)
	if err != nil || !preview.CreateAllowed || preview.PreviewDigest == "" {
		t.Fatalf("preview=%+v err=%v", preview, err)
	}
}

func TestDeploymentClientRejectsInvalidInputsBeforeNetwork(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		http.Error(w, "unexpected", http.StatusInternalServerError)
	}))
	defer server.Close()
	client := operatorClientForServer(t, server)
	zeroBatch := deploymentClientTestPreviewRequest()
	zeroBatch.BatchSize = 0
	zeroTimeout := deploymentClientTestPreviewRequest()
	zeroTimeout.ExecutionTimeoutSeconds = 0
	requests := []operator.DeploymentCreatePreviewRequest{
		{Channel: " canary", Version: "2026.9.2"},
		{Channel: "canary", Version: ".."},
		{Channel: "canary", Version: "2026.9.2", ArtifactSHA256: strings.Repeat("A", 64)},
		zeroBatch,
		zeroTimeout,
		{Channel: "canary", Version: "2026.9.2", BatchSize: store.MaxDeploymentBatchSize + 1},
		{Channel: "canary", Version: "2026.9.2", ExecutionTimeoutSeconds: 86401},
	}
	for _, request := range requests {
		if _, err := client.PreviewDeploymentCreate(t.Context(), request); err == nil {
			t.Errorf("PreviewDeploymentCreate(%+v) unexpectedly succeeded", request)
		}
	}
	if _, err := client.Deployment(t.Context(), "deployment/escape"); err == nil {
		t.Error("Deployment accepted a slash-bearing identifier")
	}
	if _, err := client.Deployments(t.Context(), operator.DeploymentListRequest{Cursor: "not-a-canonical-cursor"}); err == nil {
		t.Error("Deployments accepted an invalid cursor")
	}
	if got := hits.Load(); got != 0 {
		t.Fatalf("invalid requests reached network %d times", got)
	}
}

func TestDeploymentClientCursorIsCanonicalBoundAndPositioned(t *testing.T) {
	request := operator.DeploymentListRequest{
		States: []string{store.DeploymentFinished, store.DeploymentPaused}, Limit: 1,
	}
	item := deploymentClientTestSummary()
	cursor := deploymentClientTestCursor(t, request, item.CreatedAt, item.DeploymentID)
	page := deploymentClientTestList()
	page.Total = 2
	page.StateCounts[1].Count = 2
	page.NextCursor = &cursor
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		deploymentClientTestResponseHeaders(w)
		_, _ = w.Write([]byte(deploymentClientTestJSON(t, page)))
	}))
	defer server.Close()
	client := operatorClientForServer(t, server)
	result, err := client.Deployments(t.Context(), request)
	if err != nil || result.NextCursor == nil || *result.NextCursor != cursor {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	before := hits.Load()
	poisonedRequest := request
	poisonedRequest.Channel = "stable"
	poisonedRequest.Cursor = cursor
	if _, err := client.Deployments(t.Context(), poisonedRequest); err == nil {
		t.Fatal("cursor was reusable across a different filter")
	}
	if hits.Load() != before {
		t.Fatal("filter-mismatched cursor reached network")
	}

	wrongCursor := deploymentClientTestCursor(t, request, item.CreatedAt, "different-deployment")
	page.NextCursor = &wrongCursor
	if _, err := client.Deployments(t.Context(), request); err == nil || !strings.Contains(err.Error(), "next_cursor") {
		t.Fatalf("mispositioned response cursor error=%v", err)
	}
}

func deploymentClientTestCursor(t *testing.T, request operator.DeploymentListRequest, createdAt time.Time, deploymentID string) string {
	t.Helper()
	cursor := deploymentClientListCursor{
		Version: operator.DeploymentReadSchemaVersion, FilterDigest: deploymentClientFilterDigest(request),
		CreatedAt: createdAt, DeploymentID: deploymentID,
	}
	raw, err := json.Marshal(cursor)
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

func TestDeploymentClientRejectsPoisonedSummaryAndListCoherency(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*operator.DeploymentListResult)
	}{
		{"non UTC evaluated at", func(result *operator.DeploymentListResult) {
			result.EvaluatedAt = result.EvaluatedAt.In(time.FixedZone("offset", 3600))
		}},
		{"page state count contradiction", func(result *operator.DeploymentListResult) {
			result.StateCounts[1].Count = 0
			result.StateCounts[0].Count = 1
		}},
		{"negative control revision", func(result *operator.DeploymentListResult) {
			result.Items[0].ControlRevision = -1
		}},
		{"zero opened batch", func(result *operator.DeploymentListResult) {
			result.Items[0].OpenedBatch = 0
		}},
		{"route unsafe resource", func(result *operator.DeploymentListResult) {
			result.Items[0].ResourceID = "openclaw/escape"
		}},
		{"empty engines pointer", func(result *operator.DeploymentListResult) {
			result.Items[0].Material.EnginesNode = deploymentClientTestPtr("")
		}},
		{"recorded field on invalid material", func(result *operator.DeploymentListResult) {
			result.Items[0].Material.Status = operator.DeploymentMaterialInvalid
		}},
		{"job count exceeds opened capacity", func(result *operator.DeploymentListResult) {
			result.Items[0].BatchSize = 1
			result.Items[0].JobStateCounts[0].Count = 1
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := deploymentClientTestList()
			test.mutate(&result)
			if err := validateDeploymentListResult(result, operator.DeploymentListRequest{}, 50); err == nil {
				t.Fatalf("poisoned result accepted: %+v", result)
			}
		})
	}
}

func TestDeploymentClientRejectsPoisonedDetailTargetsAndActions(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*operator.DeploymentDetailResult)
	}{
		{"target order", func(result *operator.DeploymentDetailResult) {
			result.Targets[0], result.Targets[1] = result.Targets[1], result.Targets[0]
		}},
		{"opened job missing activity", func(result *operator.DeploymentDetailResult) {
			result.Targets[0].LastActivityAt = nil
		}},
		{"terminal failure missing stuck kind", func(result *operator.DeploymentDetailResult) {
			result.Targets[0].StuckKind = nil
		}},
		{"fresh job labeled no event", func(result *operator.DeploymentDetailResult) {
			deploymentClientMakeRunning(result, result.EvaluatedAt.Add(-time.Minute), true)
		}},
		{"silent stuck omitted", func(result *operator.DeploymentDetailResult) {
			deploymentClientMakeRunning(result, result.EvaluatedAt.Add(-store.StuckThreshold-time.Second), false)
		}},
		{"opened prefix gap", func(result *operator.DeploymentDetailResult) {
			result.Item.BatchSize = 2
			result.Item.TotalBatches = 1
			result.Targets[1].BatchNo = 1
		}},
		{"retry scope widened", func(result *operator.DeploymentDetailResult) {
			result.Actions.Retry.AffectedTargets = 2
		}},
		{"unknown action blocker", func(result *operator.DeploymentDetailResult) {
			result.Actions.Continue.Eligible = false
			result.Actions.Continue.Blockers = []string{"secret_reason"}
		}},
		{"noncanonical blocker order", func(result *operator.DeploymentDetailResult) {
			deploymentClientMakeRunning(result, result.EvaluatedAt.Add(-time.Minute), false)
			result.Actions.Continue.Blockers = []string{"nonterminal_jobs", "deployment_not_paused"}
		}},
		{"null action blockers", func(result *operator.DeploymentDetailResult) {
			result.Actions.Retry.Blockers = nil
		}},
		{"missing exhausted revision blockers", func(result *operator.DeploymentDetailResult) {
			result.Item.ControlRevision = store.MaxDeploymentControlRevision
		}},
		{"spurious exhausted revision blockers", func(result *operator.DeploymentDetailResult) {
			deploymentClientMarkExhausted(result)
			result.Item.ControlRevision--
		}},
		{"opened target without a verdict", func(result *operator.DeploymentDetailResult) {
			result.Targets[0].Independent = nil
		}},
		{"unopened target given a verdict", func(result *operator.DeploymentDetailResult) {
			result.Targets[1].Independent = &operator.DeploymentTargetIndependent{Verdict: "absent"}
		}},
		{"unknown verdict", func(result *operator.DeploymentDetailResult) {
			result.Targets[0].Independent.Verdict = "verified"
		}},
		{"passed with no live producer", func(result *operator.DeploymentDetailResult) {
			result.Targets[0].Independent = &operator.DeploymentTargetIndependent{Verdict: "passed", Rows: 1}
			deploymentClientSetIndependentCount(result, "absent", 0)
			deploymentClientSetIndependentCount(result, "passed", 1)
			result.Independent.PassedTargets = 1
		}},
		{"absent with rows", func(result *operator.DeploymentDetailResult) {
			result.Targets[0].Independent.Rows = 1
			result.Targets[0].Independent.LiveProducers = 1
			result.Independent.LiveProducers = 1
		}},
		{"more live producers than rows", func(result *operator.DeploymentDetailResult) {
			result.Targets[0].Independent = &operator.DeploymentTargetIndependent{
				Verdict: "passed", Rows: 1, LiveProducers: 2,
			}
			deploymentClientSetIndependentCount(result, "absent", 0)
			deploymentClientSetIndependentCount(result, "passed", 1)
			result.Independent.PassedTargets = 1
			result.Independent.LiveProducers = 2
		}},
		{"summary contradicts targets", func(result *operator.DeploymentDetailResult) {
			result.Independent.PassedTargets = 1
		}},
		{"summary drops a verdict row", func(result *operator.DeploymentDetailResult) {
			result.Independent.Verdicts = result.Independent.Verdicts[1:]
		}},
		{"summary miscounts opened targets", func(result *operator.DeploymentDetailResult) {
			result.Independent.OpenedTargets = 2
		}},
		{"null verdict rollup", func(result *operator.DeploymentDetailResult) {
			result.Independent.Verdicts = nil
		}},
		{"noncanonical exhausted blocker order", func(result *operator.DeploymentDetailResult) {
			deploymentClientMakeRunning(result, result.EvaluatedAt.Add(-time.Minute), false)
			deploymentClientMarkExhausted(result)
			blockers := result.Actions.Continue.Blockers
			blockers[0], blockers[len(blockers)-1] = blockers[len(blockers)-1], blockers[0]
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := deploymentClientTestDetail()
			test.mutate(&result)
			if err := validateDeploymentDetailResult(result, "deployment-1"); err == nil {
				t.Fatalf("poisoned result accepted: %+v", result)
			}
		})
	}
}

func TestDeploymentClientAcceptsCanonicalExhaustedRevisionBlockers(t *testing.T) {
	result := deploymentClientTestDetail()
	deploymentClientMarkExhausted(&result)
	if err := validateDeploymentDetailResult(result, result.Item.DeploymentID); err != nil {
		t.Fatalf("canonical exhausted detail rejected: %v", err)
	}
}

func deploymentClientMarkExhausted(result *operator.DeploymentDetailResult) {
	result.Item.ControlRevision = store.MaxDeploymentControlRevision
	actions := []*operator.DeploymentActionEligibility{
		&result.Actions.Continue, &result.Actions.SkipFailedBatch, &result.Actions.Retry, &result.Actions.Abandon,
	}
	for _, action := range actions {
		action.Eligible = false
		insertBefore := -1
		for i, blocker := range action.Blockers {
			if blocker == "failed_batch_requires_explicit_skip" {
				insertBefore = i
				break
			}
		}
		if insertBefore >= 0 {
			action.Blockers = append(action.Blockers[:insertBefore], append([]string{"control_revision_exhausted"}, action.Blockers[insertBefore:]...)...)
			continue
		}
		action.Blockers = append(action.Blockers, "control_revision_exhausted")
	}
}

func deploymentClientMakeRunning(result *operator.DeploymentDetailResult, lastActivity time.Time, includeStuck bool) {
	running := deploy.Running
	result.Targets[0].JobState = &running
	result.Targets[0].TerminalAt = nil
	result.Targets[0].LastActivityAt = &lastActivity
	result.Targets[0].StuckKind = nil
	result.Item.State = store.DeploymentRunning
	result.Item.PausedAt = nil
	result.Item.Stuck, result.Item.TerminalStuck, result.Item.SilentStuck = 0, 0, 0
	result.Item.JobStateCounts = deploymentClientTestCounts(deploy.Running)
	result.Actions.Continue = operator.DeploymentActionEligibility{
		Outcome: "open_next_batch", AffectedTargets: 1,
		Blockers: []string{"deployment_not_paused", "nonterminal_jobs"},
	}
	result.Actions.Retry = operator.DeploymentActionEligibility{
		Outcome: "create_retry_attempt", Blockers: []string{
			"deployment_not_retryable", "nonterminal_jobs", "no_terminal_failure_targets",
		},
	}
	result.Actions.SkipFailedBatch = operator.DeploymentActionEligibility{
		Outcome: "open_next_batch", AffectedTargets: 1,
		Blockers: []string{"deployment_not_paused", "nonterminal_jobs", "opened_batch_not_failed"},
	}
	result.Actions.Abandon = operator.DeploymentActionEligibility{
		Outcome: "finish_without_unopened_batches", AffectedTargets: 1,
		Blockers: []string{"deployment_not_paused", "nonterminal_jobs"},
	}
	if includeStuck {
		kind := "no_event"
		result.Targets[0].StuckKind = &kind
		result.Item.Stuck, result.Item.SilentStuck = 1, 1
	}
}

func TestDeploymentClientPreviewDigestAndPoisonValidation(t *testing.T) {
	base := deploymentClientTestPreview()
	timestampsChanged := base
	timestampsChanged.PreviewedAt = timestampsChanged.PreviewedAt.Add(time.Hour)
	timestampsChanged.Artifact.FetchedAt = timestampsChanged.Artifact.FetchedAt.Add(-time.Hour)
	if got := deploymentClientPreviewDigest(timestampsChanged); got != base.PreviewDigest {
		t.Fatalf("operational timestamps destabilized preview digest: got=%s want=%s", got, base.PreviewDigest)
	}
	contentChanged := base
	contentChanged.Artifact.EnginesNode = ">=22"
	if got := deploymentClientPreviewDigest(contentChanged); got == base.PreviewDigest {
		t.Fatal("material content change did not alter preview digest")
	}

	tests := []struct {
		name   string
		mutate func(*operator.DeploymentCreatePreviewResult, *operator.DeploymentCreatePreviewRequest)
	}{
		{"stale digest", func(result *operator.DeploymentCreatePreviewResult, _ *operator.DeploymentCreatePreviewRequest) {
			result.Artifact.EnginesNode = ">=22"
		}},
		{"non UTC preview time", func(result *operator.DeploymentCreatePreviewResult, _ *operator.DeploymentCreatePreviewRequest) {
			result.PreviewedAt = result.PreviewedAt.In(time.FixedZone("offset", -18000))
		}},
		{"target order", func(result *operator.DeploymentCreatePreviewResult, _ *operator.DeploymentCreatePreviewRequest) {
			result.Targets[0], result.Targets[1] = result.Targets[1], result.Targets[0]
			result.PreviewDigest = deploymentClientPreviewDigest(*result)
		}},
		{"empty node version pointer", func(result *operator.DeploymentCreatePreviewResult, _ *operator.DeploymentCreatePreviewRequest) {
			result.Targets[0].NodeVersion = deploymentClientTestPtr("")
			result.PreviewDigest = deploymentClientPreviewDigest(*result)
		}},
		{"invented blocker", func(result *operator.DeploymentCreatePreviewResult, _ *operator.DeploymentCreatePreviewRequest) {
			result.CreateAllowed = false
			result.Blockers = []string{"operator_message"}
			result.PreviewDigest = deploymentClientPreviewDigest(*result)
		}},
		{"allowed promotion has earliest time", func(result *operator.DeploymentCreatePreviewResult, request *operator.DeploymentCreatePreviewRequest) {
			result.Channel, request.Channel = "stable", "stable"
			earliest := result.PreviewedAt.Add(time.Hour)
			result.Promotion = &operator.DeploymentPromotionPreview{Allowed: true, Blockers: []string{}, EarliestAt: &earliest}
			result.PreviewDigest = deploymentClientPreviewDigest(*result)
		}},
		{"null top blockers", func(result *operator.DeploymentCreatePreviewResult, _ *operator.DeploymentCreatePreviewRequest) {
			result.Blockers = nil
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := deploymentClientTestPreview()
			request := deploymentClientTestPreviewRequest()
			test.mutate(&result, &request)
			if err := validateDeploymentCreatePreviewResult(result, request); err == nil {
				t.Fatalf("poisoned preview accepted: %+v", result)
			}
		})
	}
}

func TestDeploymentClientStrictlyRejectsLegacySensitiveAndNullFields(t *testing.T) {
	listWithReason := deploymentClientTestJSONMap(t, deploymentClientTestList(), func(document map[string]any) {
		item := document["items"].([]any)[0].(map[string]any)
		item["boundary_pause"] = map[string]any{
			"opened_batch": 1, "kind": store.DeploymentPauseConflict,
			"paused_at": deploymentClientTestTime(12, 4), "reason": "sensitive store reason",
		}
	})
	listMissingConsistency := deploymentClientTestJSONMap(t, deploymentClientTestList(), func(document map[string]any) {
		delete(document, "consistency")
	})
	previewWithUpstream := deploymentClientTestJSONMap(t, deploymentClientTestPreview(), func(document map[string]any) {
		artifact := document["artifact"].(map[string]any)
		artifact["tarball_url"] = "https://registry.example/private-token"
		artifact["fetched_by"] = "operator@example.test"
	})
	detailNullBlockers := deploymentClientTestJSONMap(t, deploymentClientTestDetail(), func(document map[string]any) {
		actions := document["actions"].(map[string]any)
		retry := actions["retry"].(map[string]any)
		retry["blockers"] = nil
	})
	tests := []struct {
		name, body, want string
		call             func(*Client) error
	}{
		{"legacy boundary reason", listWithReason, "unknown field", func(client *Client) error {
			_, err := client.Deployments(t.Context(), operator.DeploymentListRequest{})
			return err
		}},
		{"missing consistency", listMissingConsistency, "missing field", func(client *Client) error {
			_, err := client.Deployments(t.Context(), operator.DeploymentListRequest{})
			return err
		}},
		{"sensitive artifact provenance", previewWithUpstream, "unknown field", func(client *Client) error {
			_, err := client.PreviewDeploymentCreate(t.Context(), deploymentClientTestPreviewRequest())
			return err
		}},
		{"null action blockers", detailNullBlockers, "null is not allowed", func(client *Client) error {
			_, err := client.Deployment(t.Context(), "deployment-1")
			return err
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := machineReadResponseServer(t, http.StatusOK, test.body, nil)
			defer server.Close()
			err := test.call(operatorClientForServer(t, server))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error=%v want substring %q", err, test.want)
			}
		})
	}
}

func deploymentClientTestJSONMap(t *testing.T, value any, mutate func(map[string]any)) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	mutate(document)
	raw, err = json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// deploymentClientSetIndependentCount rewrites one verdict's target count in
// the rollup so a poisoned target can be paired with a rollup that agrees with
// it. Tests that leave the rollup stale would be rejected for the rollup rather
// than for the fact they meant to poison.
func deploymentClientSetIndependentCount(result *operator.DeploymentDetailResult, verdict string, targets int) {
	for i := range result.Independent.Verdicts {
		if result.Independent.Verdicts[i].Verdict == verdict {
			result.Independent.Verdicts[i].Targets = targets
		}
	}
}
