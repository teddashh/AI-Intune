package operatorclient

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/deploy"
	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/store"
)

func deploymentActionTestDigest() string {
	return "sha256:" + strings.Repeat("b", 64)
}

func deploymentActionTestCreateRequest() DeploymentCreateRequest {
	return DeploymentCreateRequest{
		Channel: "canary", Version: "2026.9.2", ArtifactSHA256: strings.Repeat("a", 64),
		BatchSize: 1, ExecutionTimeoutSeconds: 600,
		PreviewDigest: deploymentActionTestDigest(), ConfirmChannel: "canary",
		ConfirmVersion: "2026.9.2", Reason: "scheduled rollout",
	}
}

func deploymentActionTestContinueRequest() DeploymentContinueRequest {
	return DeploymentContinueRequest{
		PreviewDigest:           deploymentActionTestDigest(),
		ExpectedControlRevision: deploymentClientTestPtr(int64(2)),
		ExpectedOpenedBatch:     deploymentClientTestPtr(1),
		ConfirmChannel:          "canary",
		Reason:                  "reviewed next batch",
	}
}

func deploymentActionTestRetryRequest() DeploymentRetryRequest {
	return DeploymentRetryRequest{
		PreviewDigest:           deploymentActionTestDigest(),
		ExpectedControlRevision: deploymentClientTestPtr(int64(2)),
		ExpectedOpenedBatch:     deploymentClientTestPtr(1),
		ConfirmChannel:          "canary",
		ConfirmVersion:          "2026.9.2",
		Reason:                  "retry terminal failures",
	}
}

func deploymentActionTestAbandonRequest() DeploymentAbandonRequest {
	return DeploymentAbandonRequest{
		PreviewDigest:           deploymentActionTestDigest(),
		ExpectedControlRevision: deploymentClientTestPtr(int64(2)),
		ExpectedOpenedBatch:     deploymentClientTestPtr(1),
		ConfirmDeploymentID:     "deployment-1",
		Reason:                  "abandon unopened batches",
	}
}

func deploymentActionTestPreview(action string) operator.DeploymentActionPreviewResult {
	failed := deploy.Failed
	result := operator.DeploymentActionPreviewResult{
		SchemaVersion: operator.DeploymentActionSchemaVersion,
		PolicyVersion: operator.DeploymentActionPolicyVersion,
		Action:        action,
		PreviewedAt:   deploymentClientTestTime(13, 0),
		Deployment:    deploymentClientTestSummary(),
		TerminalFailureTargets: []operator.DeploymentTerminalFailurePreview{
			{
				MachineID: "machine-a", DisplayName: "alpha",
				JobID: "job-full-identifier-1", JobState: failed,
			},
		},
	}
	artifact := deploymentClientTestPreview().Artifact
	switch action {
	case "continue":
		result.Eligibility = operator.DeploymentActionEligibility{
			Eligible: true, Outcome: "open_next_batch", AffectedTargets: 1, Blockers: []string{},
		}
		result.Targets = []operator.DeploymentActionTargetPreview{
			{MachineID: "machine-b", DisplayName: "beta", BatchNo: 2},
		}
		result.Artifact = &artifact
	case "retry":
		result.Eligibility = operator.DeploymentActionEligibility{
			Eligible: true, Outcome: "create_retry_attempt", AffectedTargets: 1, Blockers: []string{},
		}
		result.Targets = []operator.DeploymentActionTargetPreview{
			{MachineID: "machine-a", DisplayName: "alpha", BatchNo: 1},
		}
		result.Artifact = &artifact
	case "abandon":
		result.Eligibility = operator.DeploymentActionEligibility{
			Eligible: true, Outcome: "finish_without_unopened_batches", AffectedTargets: 1, Blockers: []string{},
		}
		result.Targets = []operator.DeploymentActionTargetPreview{
			{MachineID: "machine-b", DisplayName: "beta", BatchNo: 2},
		}
	default:
		panic("unsupported deployment action test fixture")
	}
	result.PreviewDigest = deploymentActionClientPreviewDigest(result)
	return result
}

func deploymentActionTestMutation(action string, body any) operator.DeploymentMutationResult {
	result := operator.DeploymentMutationResult{
		SchemaVersion: operator.DeploymentActionSchemaVersion,
		Action:        action, Channel: "canary", ResourceKind: "openclaw", ResourceID: "openclaw",
		DesiredRevision: 7, BatchSize: 1, CreatedAt: deploymentClientTestTime(12, 0),
		State: store.DeploymentRunning, Jobs: []operator.DeploymentMutationJob{},
		PreviewDigest: deploymentActionTestDigest(),
	}
	newJob := operator.DeploymentMutationJob{
		JobID: "job-new-1", MachineID: "machine-b", DesiredRevision: 7,
		State: deploy.NotStarted, CreatedAt: deploymentClientTestTime(13, 5),
	}
	switch request := body.(type) {
	case DeploymentCreateRequest:
		result.DeploymentID = "deployment-created"
		result.Channel, result.BatchSize, result.PreviewDigest = request.Channel, request.BatchSize, request.PreviewDigest
		result.CreatedAt, newJob.CreatedAt = deploymentClientTestTime(13, 5), deploymentClientTestTime(13, 5)
		result.OpenedBatch, result.Jobs = 1, []operator.DeploymentMutationJob{newJob}
	case DeploymentContinueRequest:
		result.DeploymentID, result.Channel, result.PreviewDigest = "deployment-1", request.ConfirmChannel, request.PreviewDigest
		result.ControlRevision, result.OpenedBatch = *request.ExpectedControlRevision+1, *request.ExpectedOpenedBatch+1
		result.Jobs = []operator.DeploymentMutationJob{newJob}
	case DeploymentRetryRequest:
		parent := "deployment-1"
		result.DeploymentID, result.Channel, result.PreviewDigest = "deployment-retry", request.ConfirmChannel, request.PreviewDigest
		result.RetryOf, result.OpenedBatch = &parent, 1
		result.CreatedAt, newJob.CreatedAt = deploymentClientTestTime(13, 5), deploymentClientTestTime(13, 5)
		newJob.MachineID = "machine-a"
		result.Jobs = []operator.DeploymentMutationJob{newJob}
	case DeploymentAbandonRequest:
		pausedAt, finishedAt := deploymentClientTestTime(12, 4), deploymentClientTestTime(13, 5)
		result.DeploymentID, result.PreviewDigest = "deployment-1", request.PreviewDigest
		result.ControlRevision, result.OpenedBatch = *request.ExpectedControlRevision+1, *request.ExpectedOpenedBatch
		result.State, result.PausedAt, result.FinishedAt = store.DeploymentFinished, &pausedAt, &finishedAt
	default:
		panic("unsupported deployment mutation test fixture")
	}
	return result
}

func TestDeploymentActionClientAcceptsCanonicalPreviews(t *testing.T) {
	previews := map[string]operator.DeploymentActionPreviewResult{
		"/v1/operator/deployments/deployment-1/continuation-preview": deploymentActionTestPreview("continue"),
		"/v1/operator/deployments/deployment-1/retry-preview":        deploymentActionTestPreview("retry"),
		"/v1/operator/deployments/deployment-1/abandonment-preview":  deploymentActionTestPreview("abandon"),
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		preview, ok := previews[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		if r.Method != http.MethodPost || string(raw) != `{}` || r.Header.Get("Content-Type") != "application/json" ||
			r.Header.Get("Idempotency-Key") != "" || r.URL.RawQuery != "" {
			t.Errorf("preview request method=%s path=%s headers=%v body=%q", r.Method, r.URL.String(), r.Header, raw)
		}
		deploymentClientTestResponseHeaders(w)
		_, _ = w.Write([]byte(deploymentClientTestJSON(t, preview)))
	}))
	defer server.Close()
	client := operatorClientForServer(t, server)
	continuePreview, err := client.PreviewDeploymentContinue(t.Context(), "deployment-1")
	if err != nil || continuePreview.Action != "continue" || continuePreview.Artifact == nil {
		t.Fatalf("continue preview=%+v err=%v", continuePreview, err)
	}
	retryPreview, err := client.PreviewDeploymentRetry(t.Context(), "deployment-1")
	if err != nil || retryPreview.Action != "retry" || len(retryPreview.TerminalFailureTargets) != 1 {
		t.Fatalf("retry preview=%+v err=%v", retryPreview, err)
	}
	abandonPreview, err := client.PreviewDeploymentAbandon(t.Context(), "deployment-1")
	if err != nil || abandonPreview.Action != "abandon" || abandonPreview.Artifact != nil {
		t.Fatalf("abandon preview=%+v err=%v", abandonPreview, err)
	}
}

func TestDeploymentActionClientRequiresExhaustedRevisionBlocker(t *testing.T) {
	for _, action := range []string{"continue", "retry", "abandon"} {
		t.Run(action, func(t *testing.T) {
			result := deploymentActionTestPreview(action)
			result.Deployment.ControlRevision = store.MaxDeploymentControlRevision
			result.Eligibility.Eligible = false
			result.Eligibility.Blockers = append(result.Eligibility.Blockers, "control_revision_exhausted")
			result.PreviewDigest = deploymentActionClientPreviewDigest(result)
			if err := validateDeploymentActionPreviewResult(result, "deployment-1", action); err != nil {
				t.Fatalf("canonical exhausted preview rejected: %v", err)
			}
			result.Eligibility.Eligible = true
			result.Eligibility.Blockers = []string{}
			result.PreviewDigest = deploymentActionClientPreviewDigest(result)
			if err := validateDeploymentActionPreviewResult(result, "deployment-1", action); err == nil {
				t.Fatal("preview omitted exhausted revision blocker")
			}
		})
	}
}

func TestDeploymentActionClientFreshAndReplayApplyContracts(t *testing.T) {
	createRequest := deploymentActionTestCreateRequest()
	continueRequest := deploymentActionTestContinueRequest()
	retryRequest := deploymentActionTestRetryRequest()
	abandonRequest := deploymentActionTestAbandonRequest()
	type response struct {
		body        string
		freshStatus int
		key         string
	}
	responses := map[string]response{
		"/v1/operator/deployments": {
			body:        deploymentClientTestJSON(t, deploymentActionTestMutation("create", createRequest)),
			freshStatus: http.StatusCreated, key: "deployment-create-key",
		},
		"/v1/operator/deployments/deployment-1/continuations": {
			body:        deploymentClientTestJSON(t, deploymentActionTestMutation("continue", continueRequest)),
			freshStatus: http.StatusOK, key: "deployment-continue-key",
		},
		"/v1/operator/deployments/deployment-1/retries": {
			body:        deploymentClientTestJSON(t, deploymentActionTestMutation("retry", retryRequest)),
			freshStatus: http.StatusCreated, key: "deployment-retry-key",
		},
		"/v1/operator/deployments/deployment-1/abandonments": {
			body:        deploymentClientTestJSON(t, deploymentActionTestMutation("abandon", abandonRequest)),
			freshStatus: http.StatusOK, key: "deployment-abandon-key",
		},
	}
	wantBodies := map[string]string{
		"/v1/operator/deployments":                            deploymentClientTestJSON(t, createRequest),
		"/v1/operator/deployments/deployment-1/continuations": deploymentClientTestJSON(t, continueRequest),
		"/v1/operator/deployments/deployment-1/retries":       deploymentClientTestJSON(t, retryRequest),
		"/v1/operator/deployments/deployment-1/abandonments":  deploymentClientTestJSON(t, abandonRequest),
	}
	calls := make(map[string]int)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		response, ok := responses[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" ||
			r.Header.Get("Idempotency-Key") != response.key || string(raw) != wantBodies[r.URL.Path] {
			t.Errorf("apply request path=%s method=%s headers=%v body=%s", r.URL.Path, r.Method, r.Header, raw)
		}
		calls[r.URL.Path]++
		deploymentClientTestResponseHeaders(w)
		body := response.body
		status := response.freshStatus
		if calls[r.URL.Path] > 1 {
			var result operator.DeploymentMutationResult
			if err := jsonUnmarshalDeploymentTest(body, &result); err != nil {
				t.Error(err)
			}
			result.Replayed = true
			body = deploymentClientTestJSON(t, result)
			status = http.StatusOK
			w.Header().Set("Idempotency-Replayed", "true")
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()
	client := operatorClientForServer(t, server)
	tests := []struct {
		name string
		call func() (operator.DeploymentMutationResult, error)
	}{
		{"create", func() (operator.DeploymentMutationResult, error) {
			return client.CreateDeployment(t.Context(), "deployment-create-key", createRequest)
		}},
		{"continue", func() (operator.DeploymentMutationResult, error) {
			return client.ContinueDeployment(t.Context(), "deployment-1", "deployment-continue-key", continueRequest)
		}},
		{"retry", func() (operator.DeploymentMutationResult, error) {
			return client.RetryDeployment(t.Context(), "deployment-1", "deployment-retry-key", retryRequest)
		}},
		{"abandon", func() (operator.DeploymentMutationResult, error) {
			return client.AbandonDeployment(t.Context(), "deployment-1", "deployment-abandon-key", abandonRequest)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fresh, err := test.call()
			if err != nil || fresh.Replayed || fresh.Action != test.name {
				t.Fatalf("fresh=%+v err=%v", fresh, err)
			}
			replay, err := test.call()
			if err != nil || !replay.Replayed || replay.DeploymentID != fresh.DeploymentID {
				t.Fatalf("replay=%+v err=%v", replay, err)
			}
		})
	}
}

func jsonUnmarshalDeploymentTest(raw string, target any) error {
	return json.Unmarshal([]byte(raw), target)
}

func TestDeploymentActionClientRejectsInvalidRequestsBeforeNetwork(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		http.Error(w, "unexpected", http.StatusInternalServerError)
	}))
	defer server.Close()
	client := operatorClientForServer(t, server)
	create := deploymentActionTestCreateRequest()
	badCreateChannel := create
	badCreateChannel.Channel = "preview"
	badCreateVersion := create
	badCreateVersion.Version = "../escape"
	badCreateSHA := create
	badCreateSHA.ArtifactSHA256 = strings.Repeat("A", 64)
	zeroCreateBatch := create
	zeroCreateBatch.BatchSize = 0
	badCreateBatch := create
	badCreateBatch.BatchSize = store.MaxDeploymentBatchSize + 1
	zeroCreateTimeout := create
	zeroCreateTimeout.ExecutionTimeoutSeconds = 0
	badCreateTimeout := create
	badCreateTimeout.ExecutionTimeoutSeconds = 86401
	badCreateDigest := create
	badCreateDigest.PreviewDigest = "sha256:short"
	badCreateConfirm := create
	badCreateConfirm.ConfirmVersion = "different"
	badCreateReason := create
	badCreateReason.Reason = " "
	badCreateLongReason := create
	badCreateLongReason.Reason = strings.Repeat("a", 501)
	continueRequest := deploymentActionTestContinueRequest()
	missingRevision := continueRequest
	missingRevision.ExpectedControlRevision = nil
	missingBatch := continueRequest
	missingBatch.ExpectedOpenedBatch = nil
	zeroBatch := continueRequest
	zeroBatch.ExpectedOpenedBatch = deploymentClientTestPtr(0)
	negativeRevision := continueRequest
	negativeRevision.ExpectedControlRevision = deploymentClientTestPtr(int64(-1))
	badContinueChannel := continueRequest
	badContinueChannel.ConfirmChannel = "ring-0"
	retryRequest := deploymentActionTestRetryRequest()
	badRetryVersion := retryRequest
	badRetryVersion.ConfirmVersion = "."
	abandonRequest := deploymentActionTestAbandonRequest()
	badAbandonConfirm := abandonRequest
	badAbandonConfirm.ConfirmDeploymentID = "another-deployment"
	badReason := abandonRequest
	badReason.Reason = "unsafe\nreason"
	tests := []struct {
		name string
		call func() error
	}{
		{"preview route identity", func() error { _, err := client.PreviewDeploymentRetry(t.Context(), "../escape"); return err }},
		{"empty idempotency key", func() error { _, err := client.CreateDeployment(t.Context(), " ", create); return err }},
		{"unsafe idempotency key", func() error { _, err := client.CreateDeployment(t.Context(), " bad-key", create); return err }},
		{"create channel", func() error { _, err := client.CreateDeployment(t.Context(), "key", badCreateChannel); return err }},
		{"create version", func() error { _, err := client.CreateDeployment(t.Context(), "key", badCreateVersion); return err }},
		{"create artifact", func() error { _, err := client.CreateDeployment(t.Context(), "key", badCreateSHA); return err }},
		{"create zero batch", func() error { _, err := client.CreateDeployment(t.Context(), "key", zeroCreateBatch); return err }},
		{"create batch", func() error { _, err := client.CreateDeployment(t.Context(), "key", badCreateBatch); return err }},
		{"create zero timeout", func() error { _, err := client.CreateDeployment(t.Context(), "key", zeroCreateTimeout); return err }},
		{"create timeout", func() error { _, err := client.CreateDeployment(t.Context(), "key", badCreateTimeout); return err }},
		{"create digest", func() error { _, err := client.CreateDeployment(t.Context(), "key", badCreateDigest); return err }},
		{"create confirmation", func() error { _, err := client.CreateDeployment(t.Context(), "key", badCreateConfirm); return err }},
		{"create reason", func() error { _, err := client.CreateDeployment(t.Context(), "key", badCreateReason); return err }},
		{"create long reason", func() error { _, err := client.CreateDeployment(t.Context(), "key", badCreateLongReason); return err }},
		{"continue revision", func() error {
			_, err := client.ContinueDeployment(t.Context(), "deployment-1", "key", missingRevision)
			return err
		}},
		{"continue opened batch", func() error {
			_, err := client.ContinueDeployment(t.Context(), "deployment-1", "key", missingBatch)
			return err
		}},
		{"continue zero opened batch", func() error {
			_, err := client.ContinueDeployment(t.Context(), "deployment-1", "key", zeroBatch)
			return err
		}},
		{"continue negative revision", func() error {
			_, err := client.ContinueDeployment(t.Context(), "deployment-1", "key", negativeRevision)
			return err
		}},
		{"continue channel", func() error {
			_, err := client.ContinueDeployment(t.Context(), "deployment-1", "key", badContinueChannel)
			return err
		}},
		{"retry version", func() error {
			_, err := client.RetryDeployment(t.Context(), "deployment-1", "key", badRetryVersion)
			return err
		}},
		{"abandon confirmation", func() error {
			_, err := client.AbandonDeployment(t.Context(), "deployment-1", "key", badAbandonConfirm)
			return err
		}},
		{"action reason", func() error {
			_, err := client.AbandonDeployment(t.Context(), "deployment-1", "key", badReason)
			return err
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.call(); err == nil {
				t.Fatal("invalid deployment action request unexpectedly succeeded")
			}
		})
	}
	if got := hits.Load(); got != 0 {
		t.Fatalf("invalid requests reached network %d times", got)
	}
	emptyReason := deploymentActionTestCreateRequest()
	emptyReason.Reason = ""
	if err := validateDeploymentActionApplyRequest("", "create", emptyReason); err != nil {
		t.Fatalf("optional empty reason rejected: %v", err)
	}
}

func TestDeploymentActionClientAcceptsCanonicalBlockedMaterialPreview(t *testing.T) {
	result := deploymentActionTestPreview("retry")
	result.Artifact = nil
	result.Targets = []operator.DeploymentActionTargetPreview{}
	result.Eligibility.Eligible = false
	result.Eligibility.Blockers = []string{"material_unavailable"}
	result.PreviewDigest = deploymentActionClientPreviewDigest(result)
	if err := validateDeploymentActionPreviewResult(result, "deployment-1", "retry"); err != nil {
		t.Fatalf("canonical material-unavailable retry preview rejected: %v", err)
	}

	stable := deploymentActionTestPreview("retry")
	stable.Deployment.Channel = "stable"
	stable.Eligibility.Eligible = false
	stable.Eligibility.Blockers = []string{"stable_promotion_locked"}
	stable.Promotion = &operator.DeploymentPromotionPreview{
		Allowed: false, Blockers: []string{"stable_promotion_locked"},
		IndependentRequired: true, IndependentTargets: []operator.DeploymentPromotionIndependentTargetPreview{},
	}
	stable.PreviewDigest = deploymentActionClientPreviewDigest(stable)
	if err := validateDeploymentActionPreviewResult(stable, "deployment-1", "retry"); err != nil {
		t.Fatalf("canonical stable-blocked retry preview rejected: %v", err)
	}

	splitAuthority := deploymentActionTestPreview("continue")
	splitAuthority.Eligibility.Eligible = false
	splitAuthority.Eligibility.Blockers = []string{"material_identity_changed"}
	splitAuthority.PreviewDigest = deploymentActionClientPreviewDigest(splitAuthority)
	if err := validateDeploymentActionPreviewResult(splitAuthority, "deployment-1", "continue"); err != nil {
		t.Fatalf("canonical split-authority Continue preview rejected: %v", err)
	}
}

func TestDeploymentActionClientRejectsPoisonedPreviews(t *testing.T) {
	tests := []struct {
		name   string
		action string
		mutate func(*operator.DeploymentActionPreviewResult)
	}{
		{"stale digest", "continue", func(result *operator.DeploymentActionPreviewResult) {
			result.Artifact.EnginesNode = ">=99"
		}},
		{"wrong policy", "continue", func(result *operator.DeploymentActionPreviewResult) {
			result.PolicyVersion = "unsafe-policy"
		}},
		{"terminal evidence omitted", "retry", func(result *operator.DeploymentActionPreviewResult) {
			result.TerminalFailureTargets = []operator.DeploymentTerminalFailurePreview{}
			result.PreviewDigest = deploymentActionClientPreviewDigest(*result)
		}},
		{"retry target outside failure scope", "retry", func(result *operator.DeploymentActionPreviewResult) {
			result.Targets[0].MachineID = "machine-z"
			result.PreviewDigest = deploymentActionClientPreviewDigest(*result)
		}},
		{"excluded retry target has batch", "retry", func(result *operator.DeploymentActionPreviewResult) {
			result.Targets[0].ExcludedReason = deploymentClientTestPtr("conflict")
			result.PreviewDigest = deploymentActionClientPreviewDigest(*result)
		}},
		{"unknown blocker", "continue", func(result *operator.DeploymentActionPreviewResult) {
			result.Eligibility.Eligible = false
			result.Eligibility.Blockers = []string{"operator_message"}
			result.PreviewDigest = deploymentActionClientPreviewDigest(*result)
		}},
		{"affected count", "abandon", func(result *operator.DeploymentActionPreviewResult) {
			result.Eligibility.AffectedTargets = 2
			result.PreviewDigest = deploymentActionClientPreviewDigest(*result)
		}},
		{"canary promotion", "retry", func(result *operator.DeploymentActionPreviewResult) {
			result.Promotion = &operator.DeploymentPromotionPreview{Allowed: true, Blockers: []string{}}
			result.PreviewDigest = deploymentActionClientPreviewDigest(*result)
		}},
		{"summary failure undercount", "retry", func(result *operator.DeploymentActionPreviewResult) {
			result.Deployment.TerminalStuck, result.Deployment.Stuck = 0, 0
			result.PreviewDigest = deploymentActionClientPreviewDigest(*result)
		}},
		{"summary zero opened batch", "retry", func(result *operator.DeploymentActionPreviewResult) {
			result.Deployment.OpenedBatch = 0
			result.PreviewDigest = deploymentActionClientPreviewDigest(*result)
		}},
		{"non UTC artifact", "continue", func(result *operator.DeploymentActionPreviewResult) {
			result.Artifact.FetchedAt = result.Artifact.FetchedAt.In(time.FixedZone("offset", 3600))
			result.PreviewDigest = deploymentActionClientPreviewDigest(*result)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := deploymentActionTestPreview(test.action)
			test.mutate(&result)
			if err := validateDeploymentActionPreviewResult(result, "deployment-1", test.action); err == nil {
				t.Fatalf("poisoned preview accepted: %+v", result)
			}
		})
	}
}

func TestDeploymentActionClientRejectsPoisonedMutationResults(t *testing.T) {
	create := deploymentActionTestCreateRequest()
	continueRequest := deploymentActionTestContinueRequest()
	retryRequest := deploymentActionTestRetryRequest()
	abandonRequest := deploymentActionTestAbandonRequest()
	tests := []struct {
		name, action, deploymentID string
		body                       any
		result                     operator.DeploymentMutationResult
		mutate                     func(*operator.DeploymentMutationResult)
	}{
		{"create control revision", "create", "", create, deploymentActionTestMutation("create", create), func(result *operator.DeploymentMutationResult) { result.ControlRevision = 1 }},
		{"zero opened batch", "create", "", create, deploymentActionTestMutation("create", create), func(result *operator.DeploymentMutationResult) { result.OpenedBatch = 0 }},
		{"continue opened leap", "continue", "deployment-1", continueRequest, deploymentActionTestMutation("continue", continueRequest), func(result *operator.DeploymentMutationResult) { result.OpenedBatch++ }},
		{"retry lineage", "retry", "deployment-1", retryRequest, deploymentActionTestMutation("retry", retryRequest), func(result *operator.DeploymentMutationResult) { result.RetryOf = deploymentClientTestPtr("another") }},
		{"abandon jobs", "abandon", "deployment-1", abandonRequest, deploymentActionTestMutation("abandon", abandonRequest), func(result *operator.DeploymentMutationResult) {
			result.Jobs = []operator.DeploymentMutationJob{{JobID: "job-bad", MachineID: "machine-a", DesiredRevision: 7, State: deploy.NotStarted, CreatedAt: deploymentClientTestTime(13, 5)}}
		}},
		{"wrong digest", "create", "", create, deploymentActionTestMutation("create", create), func(result *operator.DeploymentMutationResult) {
			result.PreviewDigest = "sha256:" + strings.Repeat("c", 64)
		}},
		{"noncanonical job state", "retry", "deployment-1", retryRequest, deploymentActionTestMutation("retry", retryRequest), func(result *operator.DeploymentMutationResult) { result.Jobs[0].State = deploy.Running }},
		{"duplicate job machine", "continue", "deployment-1", continueRequest, deploymentActionTestMutation("continue", continueRequest), func(result *operator.DeploymentMutationResult) {
			result.BatchSize = 2
			second := result.Jobs[0]
			second.JobID = "job-new-2"
			result.Jobs = append(result.Jobs, second)
		}},
		{"non UTC creation", "create", "", create, deploymentActionTestMutation("create", create), func(result *operator.DeploymentMutationResult) {
			result.CreatedAt = result.CreatedAt.In(time.FixedZone("offset", -18000))
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := test.result
			test.mutate(&result)
			if err := validateDeploymentMutationResult(result, test.deploymentID, test.action, test.body); err == nil {
				t.Fatalf("poisoned mutation accepted: %+v", result)
			}
		})
	}
}

func TestDeploymentActionClientStrictResponseHeadersStatusAndSchema(t *testing.T) {
	preview := deploymentActionTestPreview("continue")
	validBody := deploymentClientTestJSON(t, preview)
	unknownBody := deploymentClientTestJSONMap(t, preview, func(document map[string]any) {
		document["operator_reason"] = "secret"
	})
	nullTargets := deploymentClientTestJSONMap(t, preview, func(document map[string]any) {
		document["targets"] = nil
	})
	tests := []struct {
		name    string
		status  int
		headers http.Header
		body    string
		want    string
	}{
		{"missing no-store", http.StatusOK, http.Header{"Content-Type": {"application/json"}}, validBody, "no-store"},
		{"wrong content type", http.StatusOK, http.Header{"Content-Type": {"text/plain"}, "Cache-Control": {"no-store"}}, validBody, "Content-Type"},
		{"etag", http.StatusOK, http.Header{"Content-Type": {"application/json"}, "Cache-Control": {"no-store"}, "ETag": {`"unsafe"`}}, validBody, "ETag"},
		{"preview replay", http.StatusOK, http.Header{"Content-Type": {"application/json"}, "Cache-Control": {"no-store"}, "Idempotency-Replayed": {"true"}}, validBody, "cannot be replayed"},
		{"bad replay header", http.StatusOK, http.Header{"Content-Type": {"application/json"}, "Cache-Control": {"no-store"}, "Idempotency-Replayed": {"false"}}, validBody, "invalid Idempotency-Replayed"},
		{"unexpected success status", http.StatusCreated, http.Header{"Content-Type": {"application/json"}, "Cache-Control": {"no-store"}}, validBody, "HTTP 201"},
		{"unknown field", http.StatusOK, http.Header{"Content-Type": {"application/json"}, "Cache-Control": {"no-store"}}, unknownBody, "unknown field"},
		{"null targets", http.StatusOK, http.Header{"Content-Type": {"application/json"}, "Cache-Control": {"no-store"}}, nullTargets, "null is not allowed"},
		{"duplicate field", http.StatusOK, http.Header{"Content-Type": {"application/json"}, "Cache-Control": {"no-store"}}, strings.Replace(validBody, `"action":"continue"`, `"action":"continue","action":"continue"`, 1), "duplicate object field"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := machineReadResponseServer(t, test.status, test.body, test.headers)
			defer server.Close()
			_, err := operatorClientForServer(t, server).PreviewDeploymentContinue(t.Context(), "deployment-1")
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error=%v want substring %q", err, test.want)
			}
		})
	}
}

func TestDeploymentActionClientRejectsFreshReplayContradictions(t *testing.T) {
	request := deploymentActionTestCreateRequest()
	fresh := deploymentActionTestMutation("create", request)
	replay := fresh
	replay.Replayed = true
	tests := []struct {
		name           string
		status         int
		replayedHeader bool
		body           operator.DeploymentMutationResult
		want           string
	}{
		{"fresh create uses replay status", http.StatusOK, false, fresh, "fresh"},
		{"replay uses created status", http.StatusCreated, true, replay, "must return HTTP 200"},
		{"body claims replay", http.StatusCreated, false, replay, "differs"},
		{"header claims replay", http.StatusOK, true, fresh, "differs"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				deploymentClientTestResponseHeaders(w)
				if test.replayedHeader {
					w.Header().Set("Idempotency-Replayed", "true")
				}
				w.WriteHeader(test.status)
				_, _ = w.Write([]byte(deploymentClientTestJSON(t, test.body)))
			}))
			defer server.Close()
			_, err := operatorClientForServer(t, server).CreateDeployment(t.Context(), "key", request)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error=%v want substring %q", err, test.want)
			}
		})
	}
}

func TestDeploymentActionClientPreservesReplayedAPIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Idempotency-Replayed", "true")
		w.WriteHeader(http.StatusPreconditionFailed)
		_, _ = io.WriteString(w, `{"code":"DEPLOYMENT_PREVIEW_STALE","message":"preview changed"}`)
	}))
	defer server.Close()
	_, err := operatorClientForServer(t, server).RetryDeployment(
		t.Context(), "deployment-1", "retry-key", deploymentActionTestRetryRequest())
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusPreconditionFailed ||
		apiErr.Code != "DEPLOYMENT_PREVIEW_STALE" || !apiErr.Replayed {
		t.Fatalf("error=%T %+v", err, apiErr)
	}
}

func TestEveryAcceptedBlockerTokenHasItsOwnSentence(t *testing.T) {
	blockerMaps := []struct {
		name     string
		blockers map[string]int
	}{
		{"deploymentActionContinueBlockers", deploymentActionContinueBlockers},
		{"deploymentActionRetryBlockers", deploymentActionRetryBlockers},
		{"deploymentActionAbandonBlockers", deploymentActionAbandonBlockers},
		{"deploymentContinueBlockers", deploymentContinueBlockers},
		{"deploymentRetryBlockers", deploymentRetryBlockers},
		{"deploymentAbandonBlockers", deploymentAbandonBlockers},
	}

	sources := make(map[string][]string)
	for _, blockerMap := range blockerMaps {
		for token := range blockerMap.blockers {
			sources[token] = append(sources[token], blockerMap.name)
		}
	}
	tokens := make([]string, 0, len(sources))
	for token := range sources {
		tokens = append(tokens, token)
	}
	sort.Strings(tokens)

	for _, token := range tokens {
		label := operator.DeploymentBlockerLabel(token)
		owners := strings.Join(sources[token], ", ")
		// ⚠ default 是唯一會把 token 原樣串進句子的分支；改成比對 default 的前綴，前綴一改就會靜默失效。
		if strings.Contains(label, token) {
			t.Errorf("map %s 的 token %q 拿到的句子是 %q，沒有具名文案", owners, token, label)
		}
	}
}
