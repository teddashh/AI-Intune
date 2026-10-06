package operatorclient

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/teddashh/AI-Intune/internal/deploy"
	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/store"
)

type DeploymentCreateRequest struct {
	Channel                 string `json:"channel"`
	Version                 string `json:"version"`
	ArtifactSHA256          string `json:"artifact_sha256"`
	BatchSize               int    `json:"batch_size"`
	ExecutionTimeoutSeconds int    `json:"execution_timeout_seconds"`
	Irreversible            bool   `json:"irreversible"`
	PreviewDigest           string `json:"preview_digest"`
	ConfirmChannel          string `json:"confirm_channel"`
	ConfirmVersion          string `json:"confirm_version"`
	Reason                  string `json:"reason"`
}

type DeploymentContinueRequest struct {
	PreviewDigest           string `json:"preview_digest"`
	ExpectedControlRevision *int64 `json:"expected_control_revision"`
	ExpectedOpenedBatch     *int   `json:"expected_opened_batch"`
	ConfirmChannel          string `json:"confirm_channel"`
	Reason                  string `json:"reason"`
}

type DeploymentRetryRequest struct {
	PreviewDigest           string `json:"preview_digest"`
	ExpectedControlRevision *int64 `json:"expected_control_revision"`
	ExpectedOpenedBatch     *int   `json:"expected_opened_batch"`
	ConfirmChannel          string `json:"confirm_channel"`
	ConfirmVersion          string `json:"confirm_version"`
	Reason                  string `json:"reason"`
}

type DeploymentAbandonRequest struct {
	PreviewDigest           string `json:"preview_digest"`
	ExpectedControlRevision *int64 `json:"expected_control_revision"`
	ExpectedOpenedBatch     *int   `json:"expected_opened_batch"`
	ConfirmDeploymentID     string `json:"confirm_deployment_id"`
	Reason                  string `json:"reason"`
}

func (c *Client) PreviewDeploymentContinue(ctx context.Context, deploymentID string) (operator.DeploymentActionPreviewResult, error) {
	return c.previewDeploymentAction(ctx, deploymentID, "continue", "continuation-preview")
}

func (c *Client) PreviewDeploymentRetry(ctx context.Context, deploymentID string) (operator.DeploymentActionPreviewResult, error) {
	return c.previewDeploymentAction(ctx, deploymentID, "retry", "retry-preview")
}

func (c *Client) PreviewDeploymentAbandon(ctx context.Context, deploymentID string) (operator.DeploymentActionPreviewResult, error) {
	return c.previewDeploymentAction(ctx, deploymentID, "abandon", "abandonment-preview")
}

func (c *Client) CreateDeployment(ctx context.Context, idempotencyKey string,
	body DeploymentCreateRequest,
) (operator.DeploymentMutationResult, error) {
	return c.applyDeploymentAction(ctx, "", "create", "/v1/operator/deployments",
		idempotencyKey, body, http.StatusCreated)
}

func (c *Client) ContinueDeployment(ctx context.Context, deploymentID, idempotencyKey string,
	body DeploymentContinueRequest,
) (operator.DeploymentMutationResult, error) {
	path, err := deploymentActionPath(deploymentID, "continuations")
	if err != nil {
		return operator.DeploymentMutationResult{}, err
	}
	return c.applyDeploymentAction(ctx, deploymentID, "continue", path, idempotencyKey, body, http.StatusOK)
}

func (c *Client) RetryDeployment(ctx context.Context, deploymentID, idempotencyKey string,
	body DeploymentRetryRequest,
) (operator.DeploymentMutationResult, error) {
	path, err := deploymentActionPath(deploymentID, "retries")
	if err != nil {
		return operator.DeploymentMutationResult{}, err
	}
	return c.applyDeploymentAction(ctx, deploymentID, "retry", path, idempotencyKey, body, http.StatusCreated)
}

func (c *Client) AbandonDeployment(ctx context.Context, deploymentID, idempotencyKey string,
	body DeploymentAbandonRequest,
) (operator.DeploymentMutationResult, error) {
	path, err := deploymentActionPath(deploymentID, "abandonments")
	if err != nil {
		return operator.DeploymentMutationResult{}, err
	}
	return c.applyDeploymentAction(ctx, deploymentID, "abandon", path, idempotencyKey, body, http.StatusOK)
}

func (c *Client) previewDeploymentAction(ctx context.Context, deploymentID, action, leaf string) (operator.DeploymentActionPreviewResult, error) {
	path, err := deploymentActionPath(deploymentID, leaf)
	if err != nil {
		return operator.DeploymentActionPreviewResult{}, err
	}
	req, err := c.newOperatorRequest(ctx, http.MethodPost, path, bytes.NewReader([]byte(`{}`)))
	if err != nil {
		return operator.DeploymentActionPreviewResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	response, err := c.doRaw(req)
	if err != nil {
		return operator.DeploymentActionPreviewResult{}, err
	}
	if response.status != http.StatusOK {
		return operator.DeploymentActionPreviewResult{}, fmt.Errorf("operator client: deployment %s preview returned HTTP %d", action, response.status)
	}
	if err := validateDeploymentActionHeaders(response.header, false); err != nil {
		return operator.DeploymentActionPreviewResult{}, err
	}
	var result operator.DeploymentActionPreviewResult
	if err := decodeStrictJSONDocument(response.body, "deployment action preview", &result); err != nil {
		return operator.DeploymentActionPreviewResult{}, err
	}
	if err := validateDeploymentActionPreviewResult(result, deploymentID, action); err != nil {
		return operator.DeploymentActionPreviewResult{}, err
	}
	return result, nil
}

func (c *Client) applyDeploymentAction(ctx context.Context, deploymentID, action, path,
	idempotencyKey string, body any, freshStatus int,
) (operator.DeploymentMutationResult, error) {
	if err := validateDeploymentActionIdempotencyKey(idempotencyKey); err != nil {
		return operator.DeploymentMutationResult{}, err
	}
	if err := validateDeploymentActionApplyRequest(deploymentID, action, body); err != nil {
		return operator.DeploymentMutationResult{}, err
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return operator.DeploymentMutationResult{}, fmt.Errorf("operator client: encode deployment %s request: %w", action, err)
	}
	req, err := c.newOperatorRequest(ctx, http.MethodPost, path, bytes.NewReader(raw))
	if err != nil {
		return operator.DeploymentMutationResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", idempotencyKey)
	response, err := c.doRaw(req)
	if err != nil {
		return operator.DeploymentMutationResult{}, err
	}
	if response.status != freshStatus && response.status != http.StatusOK {
		return operator.DeploymentMutationResult{}, fmt.Errorf("operator client: deployment %s returned HTTP %d", action, response.status)
	}
	replayed, err := responseReplayEvidenceFromHeader(response.header)
	if err != nil {
		return operator.DeploymentMutationResult{}, err
	}
	if err := validateDeploymentActionHeaders(response.header, replayed); err != nil {
		return operator.DeploymentMutationResult{}, err
	}
	if replayed && response.status != http.StatusOK {
		return operator.DeploymentMutationResult{}, errors.New("operator client: replayed deployment mutation must return HTTP 200")
	}
	if !replayed && response.status != freshStatus {
		return operator.DeploymentMutationResult{}, errors.New("operator client: fresh deployment mutation returned replay status")
	}
	var result operator.DeploymentMutationResult
	if err := decodeStrictJSONDocument(response.body, "deployment mutation", &result); err != nil {
		return operator.DeploymentMutationResult{}, err
	}
	if result.Replayed != replayed {
		return operator.DeploymentMutationResult{}, errors.New("operator client: deployment replay evidence differs between header and body")
	}
	if err := validateDeploymentMutationResult(result, deploymentID, action, body); err != nil {
		return operator.DeploymentMutationResult{}, err
	}
	return result, nil
}

func validateDeploymentActionIdempotencyKey(value string) error {
	if value != strings.TrimSpace(value) ||
		validateDeploymentClientText("Idempotency-Key", value, 200, false) != nil {
		return errors.New("operator client: Idempotency-Key 不可省略、含首尾空白/控制字元或超過 200 bytes")
	}
	return nil
}

func validateDeploymentActionApplyRequest(deploymentID, action string, body any) error {
	switch request := body.(type) {
	case DeploymentCreateRequest:
		if action != "create" || deploymentID != "" {
			return errors.New("operator client: deployment create request routed to the wrong action")
		}
		if err := validateDeploymentCreatePreviewRequest(operator.DeploymentCreatePreviewRequest{
			Channel: request.Channel, Version: request.Version, ArtifactSHA256: request.ArtifactSHA256,
			BatchSize: request.BatchSize, ExecutionTimeoutSeconds: request.ExecutionTimeoutSeconds,
			Irreversible: request.Irreversible,
		}); err != nil {
			return err
		}
		if request.ConfirmChannel != request.Channel || request.ConfirmVersion != request.Version {
			return errors.New("operator client: deployment create confirmation does not match planning inputs")
		}
		if err := validateDeploymentClientIdentifier("confirm_version", request.ConfirmVersion, 128); err != nil {
			return err
		}
		return validateDeploymentActionCommonApply(request.PreviewDigest, request.Reason)
	case DeploymentContinueRequest:
		if action != "continue" {
			return errors.New("operator client: deployment continue request routed to the wrong action")
		}
		if err := validateDeploymentExistingActionRequest(deploymentID, request.PreviewDigest,
			request.ExpectedControlRevision, request.ExpectedOpenedBatch, request.Reason); err != nil {
			return err
		}
		return validateDeploymentActionChannel(request.ConfirmChannel)
	case DeploymentRetryRequest:
		if action != "retry" {
			return errors.New("operator client: deployment retry request routed to the wrong action")
		}
		if err := validateDeploymentExistingActionRequest(deploymentID, request.PreviewDigest,
			request.ExpectedControlRevision, request.ExpectedOpenedBatch, request.Reason); err != nil {
			return err
		}
		if err := validateDeploymentActionChannel(request.ConfirmChannel); err != nil {
			return err
		}
		return validateDeploymentClientIdentifier("confirm_version", request.ConfirmVersion, 128)
	case DeploymentAbandonRequest:
		if action != "abandon" {
			return errors.New("operator client: deployment abandon request routed to the wrong action")
		}
		if err := validateDeploymentExistingActionRequest(deploymentID, request.PreviewDigest,
			request.ExpectedControlRevision, request.ExpectedOpenedBatch, request.Reason); err != nil {
			return err
		}
		if request.ConfirmDeploymentID != deploymentID {
			return errors.New("operator client: confirm_deployment_id does not match the request path")
		}
		return validateDeploymentClientIdentifier("confirm_deployment_id", request.ConfirmDeploymentID, 256)
	default:
		return errors.New("operator client: unsupported deployment action request body")
	}
}

func validateDeploymentExistingActionRequest(deploymentID, previewDigest string,
	expectedControlRevision *int64, expectedOpenedBatch *int, reason string,
) error {
	if err := validateDeploymentClientIdentifier("deployment_id", deploymentID, 256); err != nil {
		return err
	}
	if expectedControlRevision == nil || expectedOpenedBatch == nil {
		return errors.New("operator client: expected_control_revision and expected_opened_batch are required")
	}
	if *expectedControlRevision < 0 || *expectedControlRevision == store.MaxDeploymentControlRevision ||
		*expectedOpenedBatch < 1 || *expectedOpenedBatch == int(^uint(0)>>1) {
		return errors.New("operator client: deployment action preconditions are invalid")
	}
	return validateDeploymentActionCommonApply(previewDigest, reason)
}

func validateDeploymentActionCommonApply(previewDigest, reason string) error {
	if !validSHA256Digest(previewDigest) {
		return errors.New("operator client: preview_digest must be a canonical SHA-256 digest")
	}
	if reason != "" && (reason != strings.TrimSpace(reason) ||
		validateDeploymentClientText("reason", reason, 500, false) != nil) {
		return errors.New("operator client: reason must be canonical and limited to 500 bytes")
	}
	return nil
}

func validateDeploymentActionChannel(channel string) error {
	if channel != "canary" && channel != "stable" {
		return errors.New("operator client: confirm_channel must be canary or stable")
	}
	return nil
}

func deploymentActionPath(deploymentID, leaf string) (string, error) {
	if err := validateDeploymentClientIdentifier("deployment_id", deploymentID, 256); err != nil {
		return "", err
	}
	return "/v1/operator/deployments/" + url.PathEscape(deploymentID) + "/" + leaf, nil
}

func validateDeploymentActionHeaders(header http.Header, replayAllowed bool) error {
	if err := validateJSONNoStoreResponse(header); err != nil {
		return err
	}
	if len(header.Values("ETag")) != 0 {
		return errors.New("operator client: deployment action response must not carry an ETag")
	}
	replayed, err := responseReplayEvidenceFromHeader(header)
	if err != nil {
		return err
	}
	if replayed && !replayAllowed {
		return errors.New("operator client: deployment action preview cannot be replayed")
	}
	return nil
}

func validateDeploymentActionPreviewResult(result operator.DeploymentActionPreviewResult,
	deploymentID, action string,
) error {
	if action != "continue" && action != "retry" && action != "abandon" {
		return errors.New("operator client: invalid deployment action preview selector")
	}
	if result.SchemaVersion != operator.DeploymentActionSchemaVersion ||
		result.PolicyVersion != operator.DeploymentActionPolicyVersion || result.Action != action ||
		result.PreviewedAt.IsZero() || !validSHA256Digest(result.PreviewDigest) ||
		result.Targets == nil || result.TerminalFailureTargets == nil {
		return errors.New("operator client: invalid deployment action preview shape")
	}
	if err := requireUTCJobTime(result.PreviewedAt, "deployment action previewed_at"); err != nil {
		return err
	}
	if result.Deployment.DeploymentID != deploymentID {
		return errors.New("operator client: deployment action preview target mismatch")
	}
	if err := validateDeploymentSummary(result.Deployment); err != nil {
		return err
	}
	nonterminalJobs, terminalFailureJobs := 0, 0
	wantTerminalStates := make(map[deploy.JobState]int)
	for _, count := range result.Deployment.JobStateCounts {
		if deploy.IsTerminal(count.State) {
			if count.State != deploy.Succeeded {
				terminalFailureJobs += count.Count
				wantTerminalStates[count.State] = count.Count
			}
		} else {
			nonterminalJobs += count.Count
		}
	}
	if terminalFailureJobs != result.Deployment.TerminalStuck {
		return errors.New("operator client: deployment action summary undercounts terminal failures")
	}
	wantOutcome := map[string]string{
		"retry": "create_retry_attempt", "abandon": "finish_without_unopened_batches",
	}[action]
	if action == "continue" {
		wantOutcome = "open_next_batch"
		if result.Deployment.OpenedBatch >= result.Deployment.TotalBatches {
			wantOutcome = "finish"
		}
	}
	allowedBlockers := deploymentActionContinueBlockers
	if action == "retry" {
		allowedBlockers = deploymentActionRetryBlockers
	} else if action == "abandon" {
		allowedBlockers = deploymentActionAbandonBlockers
	}
	if err := validateDeploymentActionEligibility(result.Eligibility, wantOutcome, allowedBlockers); err != nil {
		return err
	}
	if err := validateDeploymentActionPreviewVisibleBlockers(result, nonterminalJobs); err != nil {
		return err
	}
	seenTargets := make(map[string]bool, len(result.Targets))
	previousID := ""
	for _, target := range result.Targets {
		if err := validateDeploymentClientIdentifier("action target machine_id", target.MachineID, 256); err != nil ||
			validateDeploymentClientText("action target display_name", target.DisplayName, 256, false) != nil ||
			target.BatchNo < 0 || seenTargets[target.MachineID] || (previousID != "" && target.MachineID <= previousID) {
			return errors.New("operator client: invalid, duplicate or unordered deployment action target")
		}
		seenTargets[target.MachineID], previousID = true, target.MachineID
		if target.ExcludedReason != nil && !validDeploymentExclusion(*target.ExcludedReason) {
			return errors.New("operator client: invalid deployment action target exclusion")
		}
		if target.JobID != nil {
			if err := validateDeploymentClientIdentifier("action target job_id", *target.JobID, 256); err != nil ||
				target.JobState == nil || !deploy.IsKnownJobState(*target.JobState) {
				return errors.New("operator client: invalid deployment action target job")
			}
		} else if target.JobState != nil {
			return errors.New("operator client: deployment action target state lacks job identity")
		}
	}
	seenFailures := make(map[string]bool, len(result.TerminalFailureTargets))
	seenFailureJobs := make(map[string]bool, len(result.TerminalFailureTargets))
	gotTerminalStates := make(map[deploy.JobState]int)
	previousID = ""
	for _, target := range result.TerminalFailureTargets {
		if err := validateDeploymentClientIdentifier("terminal failure machine_id", target.MachineID, 256); err != nil ||
			validateDeploymentClientText("terminal failure display_name", target.DisplayName, 256, false) != nil ||
			validateDeploymentClientIdentifier("terminal failure job_id", target.JobID, 256) != nil ||
			!deploy.IsTerminal(target.JobState) || target.JobState == deploy.Succeeded || seenFailures[target.MachineID] ||
			seenFailureJobs[target.JobID] ||
			(previousID != "" && target.MachineID <= previousID) {
			return errors.New("operator client: invalid, duplicate or unordered terminal failure target")
		}
		seenFailures[target.MachineID], seenFailureJobs[target.JobID], previousID = true, true, target.MachineID
		gotTerminalStates[target.JobState]++
	}
	if len(result.TerminalFailureTargets) != result.Deployment.TerminalStuck {
		return errors.New("operator client: terminal failure evidence contradicts deployment summary")
	}
	for state, count := range wantTerminalStates {
		if gotTerminalStates[state] != count {
			return errors.New("operator client: terminal failure state evidence contradicts deployment summary")
		}
	}
	if action == "continue" {
		for _, target := range result.Targets {
			if target.BatchNo != result.Deployment.OpenedBatch+1 || target.JobID != nil || target.JobState != nil ||
				target.ExcludedReason != nil || seenFailures[target.MachineID] {
				return errors.New("operator client: continue preview targets are not the exact next batch")
			}
		}
	} else if action == "retry" {
		if result.Artifact == nil && len(result.Targets) != 0 {
			return errors.New("operator client: retry without resolved material carries a target plan")
		}
		if result.Artifact != nil && len(result.Targets) != len(result.TerminalFailureTargets) {
			return errors.New("operator client: retry plan does not cover the exact terminal failure set")
		}
		batchCounts, maxBatch, included := make(map[int]int), 0, 0
		for _, target := range result.Targets {
			if !seenFailures[target.MachineID] || target.JobID != nil || target.JobState != nil {
				return errors.New("operator client: retry target is outside terminal failure scope")
			}
			if target.ExcludedReason != nil {
				if target.BatchNo != 0 {
					return errors.New("operator client: excluded retry target has a batch")
				}
				continue
			}
			if target.BatchNo < 1 {
				return errors.New("operator client: included retry target lacks a batch")
			}
			included++
			batchCounts[target.BatchNo]++
			if batchCounts[target.BatchNo] > result.Deployment.BatchSize {
				return errors.New("operator client: retry target batch exceeds batch_size")
			}
			if target.BatchNo > maxBatch {
				maxBatch = target.BatchNo
			}
		}
		if maxBatch > 0 && len(batchCounts) != maxBatch {
			return errors.New("operator client: retry target batches are not contiguous")
		}
		if deploymentHasBlocker(result.Eligibility, "no_included_targets") !=
			(result.Artifact != nil && len(result.TerminalFailureTargets) > 0 && included == 0) {
			return errors.New("operator client: retry included-target blocker is incoherent")
		}
	} else if action == "abandon" {
		if result.Artifact != nil || result.Promotion != nil {
			return errors.New("operator client: abandon preview unexpectedly contains material or promotion")
		}
		for _, target := range result.Targets {
			if target.JobID != nil || target.JobState != nil || target.ExcludedReason != nil ||
				target.BatchNo <= result.Deployment.OpenedBatch || seenFailures[target.MachineID] {
				return errors.New("operator client: abandon target is not an unopened included target")
			}
		}
	}
	if result.Eligibility.AffectedTargets != len(result.Targets) && action != "retry" {
		return errors.New("operator client: action affected target count contradicts targets")
	}
	if action == "retry" && result.Eligibility.AffectedTargets != len(result.TerminalFailureTargets) {
		return errors.New("operator client: retry affected count contradicts terminal failures")
	}
	if err := validateDeploymentActionPreviewMaterial(result); err != nil {
		return err
	}
	if deploymentActionClientPreviewDigest(result) != result.PreviewDigest {
		return errors.New("operator client: deployment action preview digest does not match canonical content")
	}
	return nil
}

var deploymentActionContinueBlockers = map[string]int{
	"deployment_not_paused": 0, "nonterminal_jobs": 1, "no_opened_batch": 2,
	"next_batch_empty": 3, "invalid_material": 4, "stable_promotion_locked": 5,
	"active_resource_deployment": 6, "control_revision_exhausted": 7,
	"failed_batch_requires_explicit_skip": 8,
	"material_unavailable":                9, "material_identity_changed": 9,
}

var deploymentActionRetryBlockers = map[string]int{
	"deployment_not_retryable": 0, "nonterminal_jobs": 1, "no_terminal_failure_targets": 2,
	"no_opened_batch": 3, "invalid_material": 4, "stable_promotion_locked": 5,
	"active_resource_deployment": 6, "control_revision_exhausted": 7,
	"material_unavailable": 8, "material_identity_changed": 8,
	"target_snapshot_changed": 9, "no_included_targets": 10,
}

var deploymentActionAbandonBlockers = map[string]int{
	"deployment_not_paused": 0, "nonterminal_jobs": 1, "active_resource_deployment": 2,
	"control_revision_exhausted": 3,
}

func validateDeploymentActionPreviewVisibleBlockers(result operator.DeploymentActionPreviewResult,
	nonterminalJobs int,
) error {
	item, eligibility := result.Deployment, result.Eligibility
	checks := []struct {
		blocker  string
		expected bool
	}{
		{"nonterminal_jobs", nonterminalJobs > 0},
		{"control_revision_exhausted", item.ControlRevision == store.MaxDeploymentControlRevision},
	}
	switch result.Action {
	case "continue":
		checks = append(checks,
			struct {
				blocker  string
				expected bool
			}{"deployment_not_paused", item.State != store.DeploymentPaused},
			struct {
				blocker  string
				expected bool
			}{"no_opened_batch", item.OpenedBatch == 0},
			struct {
				blocker  string
				expected bool
			}{"next_batch_empty", item.OpenedBatch < item.TotalBatches && eligibility.AffectedTargets == 0},
			struct {
				blocker  string
				expected bool
			}{"invalid_material", item.OpenedBatch < item.TotalBatches && item.Material.Status != operator.DeploymentMaterialRecorded},
		)
	case "retry":
		checks = append(checks,
			struct {
				blocker  string
				expected bool
			}{"deployment_not_retryable", item.State != store.DeploymentPaused && item.State != store.DeploymentFinished},
			struct {
				blocker  string
				expected bool
			}{"no_terminal_failure_targets", item.TerminalStuck == 0},
			struct {
				blocker  string
				expected bool
			}{"no_opened_batch", item.OpenedBatch == 0},
			struct {
				blocker  string
				expected bool
			}{"invalid_material", item.Material.Status != operator.DeploymentMaterialRecorded},
		)
	case "abandon":
		checks = append(checks, struct {
			blocker  string
			expected bool
		}{"deployment_not_paused", item.State != store.DeploymentPaused})
	}
	for _, check := range checks {
		if deploymentHasBlocker(eligibility, check.blocker) != check.expected {
			return fmt.Errorf("operator client: deployment action blocker %s contradicts visible state", check.blocker)
		}
	}
	return nil
}

func validateDeploymentActionPreviewMaterial(result operator.DeploymentActionPreviewResult) error {
	requiresMaterial := result.Action == "retry" ||
		(result.Action == "continue" && result.Deployment.OpenedBatch < result.Deployment.TotalBatches)
	invalid := deploymentHasBlocker(result.Eligibility, "invalid_material")
	unavailable := deploymentHasBlocker(result.Eligibility, "material_unavailable")
	identityChanged := deploymentHasBlocker(result.Eligibility, "material_identity_changed")
	if !requiresMaterial {
		if result.Artifact != nil || invalid || unavailable || identityChanged {
			return errors.New("operator client: material-free deployment action contains material evidence")
		}
	} else if result.Deployment.Material.Status != operator.DeploymentMaterialRecorded {
		if result.Artifact != nil || !invalid || unavailable || identityChanged {
			return errors.New("operator client: invalid deployment material evidence is incoherent")
		}
	} else if result.Artifact == nil {
		if invalid || unavailable == identityChanged {
			return errors.New("operator client: unresolved deployment material lacks one canonical blocker")
		}
	} else {
		// Continue can resolve the spec artifact successfully and still discover
		// that the stored first-job template names different bytes. That is a
		// canonical fail-closed preview: keep the verified artifact evidence for
		// the operator, but carry material_identity_changed and refuse Apply.
		if invalid || unavailable || (identityChanged && result.Action != "continue") {
			return errors.New("operator client: resolved deployment material still carries a material blocker")
		}
		if err := validateDeploymentActionArtifact(*result.Artifact, result.Deployment.Material); err != nil {
			return err
		}
	}

	wantPromotion := requiresMaterial && result.Deployment.Channel == "stable" &&
		result.Deployment.Material.Status == operator.DeploymentMaterialRecorded
	if (result.Promotion != nil) != wantPromotion {
		return errors.New("operator client: deployment action promotion evidence is missing or unexpected")
	}
	promotionBlocked := false
	if result.Promotion != nil {
		if err := validateDeploymentActionPromotion(*result.Promotion, result.PreviewedAt); err != nil {
			return err
		}
		promotionBlocked = !result.Promotion.Allowed
	}
	if deploymentHasBlocker(result.Eligibility, "stable_promotion_locked") != promotionBlocked {
		return errors.New("operator client: promotion decision contradicts action eligibility")
	}
	return nil
}

func validateDeploymentActionArtifact(value operator.DeploymentArtifactPreview,
	material operator.DeploymentMaterialSummary,
) error {
	engines := ""
	if material.EnginesNode != nil {
		engines = *material.EnginesNode
	}
	if material.Status != operator.DeploymentMaterialRecorded || material.Version == nil ||
		material.ArtifactDigest == nil || material.SizeBytes == nil || value.Name != "openclaw" ||
		value.Version != *material.Version || !validSHA256Digest(value.Digest) ||
		!strings.HasPrefix(value.Digest, "sha256:") || value.SHA256 != strings.TrimPrefix(value.Digest, "sha256:") ||
		value.Digest != *material.ArtifactDigest || value.SizeBytes != *material.SizeBytes ||
		value.EnginesNode != engines || !value.AvailableAndVerified || value.FetchedAt.IsZero() ||
		validateDeploymentClientText("action artifact engines_node", value.EnginesNode, 512, true) != nil ||
		validateDeploymentClientText("action artifact sha512_integrity", value.SHA512Integrity, 1024, false) != nil {
		return errors.New("operator client: deployment action artifact contradicts recorded material")
	}
	return requireUTCJobTime(value.FetchedAt, "deployment action artifact fetched_at")
}

func validateDeploymentActionPromotion(value operator.DeploymentPromotionPreview, previewedAt time.Time) error {
	return validateDeploymentPromotionPreview(value, previewedAt)
}

func deploymentActionClientPreviewDigest(result operator.DeploymentActionPreviewResult) string {
	type artifactIdentity struct {
		Name          string `json:"name"`
		Version       string `json:"version"`
		SHA256        string `json:"sha256"`
		Digest        string `json:"digest"`
		SizeBytes     int64  `json:"size_bytes"`
		EnginesNode   string `json:"engines_node"`
		SHA512        string `json:"sha512_integrity"`
		BytesVerified bool   `json:"bytes_verified"`
	}
	var artifactValue *artifactIdentity
	if result.Artifact != nil {
		artifactValue = &artifactIdentity{
			Name: result.Artifact.Name, Version: result.Artifact.Version,
			SHA256: result.Artifact.SHA256, Digest: result.Artifact.Digest,
			SizeBytes: result.Artifact.SizeBytes, EnginesNode: result.Artifact.EnginesNode,
			SHA512: result.Artifact.SHA512Integrity, BytesVerified: result.Artifact.AvailableAndVerified,
		}
	}
	body := struct {
		Version                int                                         `json:"version"`
		PolicyVersion          string                                      `json:"policy_version"`
		Action                 string                                      `json:"action"`
		Deployment             operator.DeploymentSummary                  `json:"deployment"`
		Eligibility            operator.DeploymentActionEligibility        `json:"eligibility"`
		Targets                []operator.DeploymentActionTargetPreview    `json:"targets"`
		TerminalFailureTargets []operator.DeploymentTerminalFailurePreview `json:"terminal_failure_targets"`
		Artifact               *artifactIdentity                           `json:"artifact"`
		Promotion              *operator.DeploymentPromotionPreview        `json:"promotion"`
	}{
		Version: operator.DeploymentActionSchemaVersion, PolicyVersion: operator.DeploymentActionPolicyVersion,
		Action: result.Action, Deployment: result.Deployment, Eligibility: result.Eligibility,
		Targets: result.Targets, TerminalFailureTargets: result.TerminalFailureTargets,
		Artifact: artifactValue, Promotion: result.Promotion,
	}
	raw, _ := json.Marshal(body)
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func validateDeploymentMutationResult(result operator.DeploymentMutationResult,
	requestDeploymentID, action string, body any,
) error {
	if action != "create" && action != "continue" && action != "retry" && action != "abandon" {
		return errors.New("operator client: invalid deployment mutation selector")
	}
	if result.SchemaVersion != operator.DeploymentActionSchemaVersion || result.Action != action ||
		!validSHA256Digest(result.PreviewDigest) || result.Jobs == nil || result.DesiredRevision <= 0 ||
		result.ControlRevision < 0 || result.BatchSize < 1 || result.BatchSize > store.MaxDeploymentBatchSize ||
		result.CreatedAt.IsZero() || result.OpenedBatch < 1 ||
		(result.Channel != "canary" && result.Channel != "stable") ||
		result.ResourceKind != "openclaw" || result.ResourceID != "openclaw" ||
		(result.State != store.DeploymentRunning && result.State != store.DeploymentPaused && result.State != store.DeploymentFinished) {
		return errors.New("operator client: invalid deployment mutation result")
	}
	if err := validateDeploymentClientIdentifier("deployment_id", result.DeploymentID, 256); err != nil {
		return err
	}
	if err := requireUTCJobTime(result.CreatedAt, "deployment mutation created_at"); err != nil {
		return err
	}
	for name, value := range map[string]*time.Time{
		"deployment mutation paused_at": result.PausedAt, "deployment mutation finished_at": result.FinishedAt,
	} {
		if value != nil {
			if value.IsZero() || value.Before(result.CreatedAt) {
				return fmt.Errorf("operator client: %s is zero or predates deployment", name)
			}
			if err := requireUTCJobTime(*value, name); err != nil {
				return err
			}
		}
	}
	if result.RetryOf != nil {
		if err := validateDeploymentClientIdentifier("retry_of", *result.RetryOf, 256); err != nil {
			return err
		}
		if *result.RetryOf == result.DeploymentID {
			return errors.New("operator client: deployment mutation contains self retry lineage")
		}
	}
	wantDigest, confirmChannel := "", ""
	var expectedControlRevision *int64
	var expectedOpenedBatch *int
	switch typed := body.(type) {
	case DeploymentCreateRequest:
		if action != "create" {
			return errors.New("operator client: deployment mutation body/action mismatch")
		}
		wantDigest = typed.PreviewDigest
		confirmChannel = typed.Channel
	case DeploymentContinueRequest:
		if action != "continue" {
			return errors.New("operator client: deployment mutation body/action mismatch")
		}
		wantDigest = typed.PreviewDigest
		confirmChannel, expectedControlRevision, expectedOpenedBatch = typed.ConfirmChannel,
			typed.ExpectedControlRevision, typed.ExpectedOpenedBatch
	case DeploymentRetryRequest:
		if action != "retry" {
			return errors.New("operator client: deployment mutation body/action mismatch")
		}
		wantDigest = typed.PreviewDigest
		confirmChannel, expectedControlRevision, expectedOpenedBatch = typed.ConfirmChannel,
			typed.ExpectedControlRevision, typed.ExpectedOpenedBatch
	case DeploymentAbandonRequest:
		if action != "abandon" {
			return errors.New("operator client: deployment mutation body/action mismatch")
		}
		wantDigest = typed.PreviewDigest
		expectedControlRevision, expectedOpenedBatch = typed.ExpectedControlRevision, typed.ExpectedOpenedBatch
	default:
		return errors.New("operator client: unknown deployment mutation request body")
	}
	if result.PreviewDigest != wantDigest {
		return errors.New("operator client: deployment mutation preview digest mismatch")
	}
	if confirmChannel != "" && result.Channel != confirmChannel {
		return errors.New("operator client: deployment mutation channel differs from confirmation")
	}
	if (action == "continue" || action == "abandon") && result.DeploymentID != requestDeploymentID {
		return errors.New("operator client: deployment mutation target mismatch")
	}

	switch action {
	case "create":
		if result.RetryOf != nil || result.ControlRevision != 0 || result.OpenedBatch != 1 ||
			result.State != store.DeploymentRunning || result.PausedAt != nil || result.FinishedAt != nil || len(result.Jobs) == 0 {
			return errors.New("operator client: create mutation lifecycle is incoherent")
		}
	case "retry":
		if expectedControlRevision == nil || expectedOpenedBatch == nil || result.RetryOf == nil ||
			*result.RetryOf != requestDeploymentID || result.DeploymentID == requestDeploymentID ||
			result.ControlRevision != 0 || result.OpenedBatch != 1 || result.State != store.DeploymentRunning ||
			result.PausedAt != nil || result.FinishedAt != nil || len(result.Jobs) == 0 {
			return errors.New("operator client: retry mutation lifecycle or lineage is incoherent")
		}
	case "continue":
		if expectedControlRevision == nil || expectedOpenedBatch == nil ||
			result.ControlRevision != *expectedControlRevision+1 || result.PausedAt != nil {
			return errors.New("operator client: continue mutation precondition result is incoherent")
		}
		switch result.OpenedBatch {
		case *expectedOpenedBatch:
			if result.State != store.DeploymentFinished || result.FinishedAt == nil || len(result.Jobs) != 0 {
				return errors.New("operator client: finishing continue mutation is incoherent")
			}
		case *expectedOpenedBatch + 1:
			if result.State != store.DeploymentRunning || result.FinishedAt != nil || len(result.Jobs) == 0 {
				return errors.New("operator client: next-batch continue mutation is incoherent")
			}
		default:
			return errors.New("operator client: continue mutation opened_batch is incoherent")
		}
	case "abandon":
		if expectedControlRevision == nil || expectedOpenedBatch == nil ||
			result.ControlRevision != *expectedControlRevision+1 || result.OpenedBatch != *expectedOpenedBatch ||
			result.State != store.DeploymentFinished || result.PausedAt == nil || result.FinishedAt == nil || len(result.Jobs) != 0 {
			return errors.New("operator client: abandon mutation lifecycle is incoherent")
		}
	}
	if len(result.Jobs) > result.BatchSize {
		return errors.New("operator client: deployment mutation returned more jobs than batch_size")
	}
	seenJobs := make(map[string]bool, len(result.Jobs))
	seenMachines := make(map[string]bool, len(result.Jobs))
	for _, job := range result.Jobs {
		if validateDeploymentClientIdentifier("mutation job_id", job.JobID, 256) != nil ||
			validateDeploymentClientIdentifier("mutation machine_id", job.MachineID, 256) != nil ||
			job.DesiredRevision != result.DesiredRevision || job.State != deploy.NotStarted ||
			job.CreatedAt.IsZero() || job.CreatedAt.Before(result.CreatedAt) || seenJobs[job.JobID] || seenMachines[job.MachineID] {
			return errors.New("operator client: invalid or duplicate deployment mutation job")
		}
		if err := requireUTCJobTime(job.CreatedAt, "deployment mutation job created_at"); err != nil {
			return err
		}
		seenJobs[job.JobID], seenMachines[job.MachineID] = true, true
	}
	return nil
}
