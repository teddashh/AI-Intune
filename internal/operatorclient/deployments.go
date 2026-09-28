package operatorclient

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/teddashh/AI-Intune/internal/artifact"
	"github.com/teddashh/AI-Intune/internal/deploy"
	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/store"
)

func (c *Client) Deployments(ctx context.Context, request operator.DeploymentListRequest) (operator.DeploymentListResult, error) {
	query, effectiveLimit, err := encodeDeploymentListQuery(request)
	if err != nil {
		return operator.DeploymentListResult{}, err
	}
	path := "/v1/operator/deployments"
	if encoded := query.Encode(); encoded != "" {
		path += "?" + encoded
	}
	req, err := c.newOperatorRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return operator.DeploymentListResult{}, err
	}
	response, err := c.doRaw(req)
	if err != nil {
		return operator.DeploymentListResult{}, err
	}
	if response.status != http.StatusOK {
		return operator.DeploymentListResult{}, fmt.Errorf("operator client: deployment list returned HTTP %d", response.status)
	}
	if err := validateDeploymentReadHeaders(response.header); err != nil {
		return operator.DeploymentListResult{}, err
	}
	var result operator.DeploymentListResult
	if err := decodeStrictJSONDocument(response.body, "deployment list", &result); err != nil {
		return operator.DeploymentListResult{}, err
	}
	if err := validateDeploymentListResult(result, request, effectiveLimit); err != nil {
		return operator.DeploymentListResult{}, err
	}
	return result, nil
}

func (c *Client) Deployment(ctx context.Context, deploymentID string) (operator.DeploymentDetailResult, error) {
	if err := validateDeploymentClientIdentifier("deployment_id", deploymentID, 256); err != nil {
		return operator.DeploymentDetailResult{}, errors.New("operator client: deployment_id 不可為空、含控制字元、dot segment 或斜線")
	}
	req, err := c.newOperatorRequest(ctx, http.MethodGet,
		"/v1/operator/deployments/"+url.PathEscape(deploymentID), nil)
	if err != nil {
		return operator.DeploymentDetailResult{}, err
	}
	response, err := c.doRaw(req)
	if err != nil {
		return operator.DeploymentDetailResult{}, err
	}
	if response.status != http.StatusOK {
		return operator.DeploymentDetailResult{}, fmt.Errorf("operator client: deployment detail returned HTTP %d", response.status)
	}
	if err := validateDeploymentReadHeaders(response.header); err != nil {
		return operator.DeploymentDetailResult{}, err
	}
	var result operator.DeploymentDetailResult
	if err := decodeStrictJSONDocument(response.body, "deployment detail", &result); err != nil {
		return operator.DeploymentDetailResult{}, err
	}
	if err := validateDeploymentDetailResult(result, deploymentID); err != nil {
		return operator.DeploymentDetailResult{}, err
	}
	return result, nil
}

func (c *Client) PreviewDeploymentCreate(ctx context.Context,
	body operator.DeploymentCreatePreviewRequest,
) (operator.DeploymentCreatePreviewResult, error) {
	if err := validateDeploymentCreatePreviewRequest(body); err != nil {
		return operator.DeploymentCreatePreviewResult{}, err
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return operator.DeploymentCreatePreviewResult{}, fmt.Errorf("operator client: encode deployment preview: %w", err)
	}
	req, err := c.newOperatorRequest(ctx, http.MethodPost, "/v1/operator/deployments/preview", bytes.NewReader(raw))
	if err != nil {
		return operator.DeploymentCreatePreviewResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	response, err := c.doRaw(req)
	if err != nil {
		return operator.DeploymentCreatePreviewResult{}, err
	}
	if response.status != http.StatusOK {
		return operator.DeploymentCreatePreviewResult{}, fmt.Errorf("operator client: deployment preview returned HTTP %d", response.status)
	}
	if err := validateDeploymentReadHeaders(response.header); err != nil {
		return operator.DeploymentCreatePreviewResult{}, err
	}
	var result operator.DeploymentCreatePreviewResult
	if err := decodeStrictJSONDocument(response.body, "deployment preview", &result); err != nil {
		return operator.DeploymentCreatePreviewResult{}, err
	}
	if err := validateDeploymentCreatePreviewResult(result, body); err != nil {
		return operator.DeploymentCreatePreviewResult{}, err
	}
	return result, nil
}

func encodeDeploymentListQuery(request operator.DeploymentListRequest) (url.Values, int, error) {
	query := make(url.Values)
	if request.Channel != "" {
		if request.Channel != "canary" && request.Channel != "stable" {
			return nil, 0, errors.New("operator client: deployment channel must be canary or stable")
		}
		query.Set("channel", request.Channel)
	}
	seenStates := make(map[string]bool, len(request.States))
	for _, state := range request.States {
		if (state != store.DeploymentRunning && state != store.DeploymentPaused && state != store.DeploymentFinished) || seenStates[state] {
			return nil, 0, fmt.Errorf("operator client: invalid or duplicate deployment state %q", state)
		}
		seenStates[state] = true
		query.Add("state", state)
	}
	if request.Stuck != nil {
		query.Set("stuck", strconv.FormatBool(*request.Stuck))
	}
	limit := request.Limit
	if limit == 0 {
		limit = operator.DefaultDeploymentReadLimit
	} else if limit < 1 || limit > operator.MaxDeploymentReadLimit {
		return nil, 0, fmt.Errorf("operator client: deployment limit must be between 1 and %d", operator.MaxDeploymentReadLimit)
	} else {
		query.Set("limit", strconv.Itoa(limit))
	}
	if request.Cursor != "" {
		if _, err := decodeDeploymentClientListCursor(request.Cursor, request); err != nil {
			return nil, 0, err
		}
		query.Set("cursor", request.Cursor)
	}
	return query, limit, nil
}

func validateDeploymentReadHeaders(header http.Header) error {
	if err := validateJSONNoStoreResponse(header); err != nil {
		return err
	}
	if replayed, err := responseReplayEvidenceFromHeader(header); err != nil {
		return err
	} else if replayed {
		return errors.New("operator client: deployment read/preview cannot be an idempotency replay")
	}
	if len(header.Values("ETag")) != 0 {
		return errors.New("operator client: composite deployment response must not carry an ETag")
	}
	return nil
}

func validateDeploymentListResult(result operator.DeploymentListResult, request operator.DeploymentListRequest, limit int) error {
	if result.SchemaVersion != operator.DeploymentReadSchemaVersion ||
		result.Consistency != operator.DeploymentReadConsistencyLive || result.EvaluatedAt.IsZero() {
		return errors.New("operator client: invalid deployment list schema or evaluation time")
	}
	if err := requireUTCJobTime(result.EvaluatedAt, "deployment evaluated_at"); err != nil {
		return err
	}
	if result.Items == nil || result.StateCounts == nil || len(result.StateCounts) != 3 ||
		result.Total < 0 || result.Total < len(result.Items) || len(result.Items) > limit {
		return errors.New("operator client: inconsistent deployment list shape")
	}
	wantStates := []string{store.DeploymentRunning, store.DeploymentPaused, store.DeploymentFinished}
	total := 0
	for i, count := range result.StateCounts {
		if count.State != wantStates[i] || count.Count < 0 {
			return errors.New("operator client: deployment state counts are not canonical")
		}
		total += count.Count
	}
	if total != result.Total {
		return errors.New("operator client: deployment state counts do not equal total")
	}
	seen := make(map[string]bool, len(result.Items))
	pageStateCounts := make(map[string]int, len(wantStates))
	for i, item := range result.Items {
		if err := validateDeploymentSummary(item); err != nil {
			return fmt.Errorf("operator client: deployment list item %d: %w", i, err)
		}
		if seen[item.DeploymentID] {
			return errors.New("operator client: duplicate deployment_id")
		}
		seen[item.DeploymentID] = true
		pageStateCounts[item.State]++
		if request.Channel != "" && item.Channel != request.Channel {
			return errors.New("operator client: deployment item violates channel filter")
		}
		if len(request.States) > 0 && !stringSelected(item.State, request.States) {
			return errors.New("operator client: deployment item violates state filter")
		}
		if request.Stuck != nil && (item.Stuck > 0) != *request.Stuck {
			return errors.New("operator client: deployment item violates stuck filter")
		}
		if i > 0 {
			previous := result.Items[i-1]
			if item.CreatedAt.After(previous.CreatedAt) ||
				(item.CreatedAt.Equal(previous.CreatedAt) && item.DeploymentID >= previous.DeploymentID) {
				return errors.New("operator client: deployment items are not in canonical keyset order")
			}
		}
	}
	for _, count := range result.StateCounts {
		if pageStateCounts[count.State] > count.Count {
			return errors.New("operator client: deployment state counts contradict page items")
		}
	}
	if result.NextCursor != nil {
		cursor, err := decodeDeploymentClientListCursor(*result.NextCursor, request)
		if err != nil || len(result.Items) != limit || result.Total <= len(result.Items) ||
			*result.NextCursor == request.Cursor || !cursor.CreatedAt.Equal(result.Items[len(result.Items)-1].CreatedAt) ||
			cursor.DeploymentID != result.Items[len(result.Items)-1].DeploymentID {
			return errors.New("operator client: deployment next_cursor is inconsistent")
		}
	}
	if request.Cursor == "" && (result.Total > len(result.Items)) != (result.NextCursor != nil) {
		return errors.New("operator client: first deployment page has inconsistent continuation evidence")
	}
	return nil
}

func validateDeploymentDetailResult(result operator.DeploymentDetailResult, deploymentID string) error {
	if result.SchemaVersion != operator.DeploymentReadSchemaVersion ||
		result.Consistency != operator.DeploymentReadConsistencyLive || result.EvaluatedAt.IsZero() ||
		result.Targets == nil {
		return errors.New("operator client: invalid deployment detail shape")
	}
	if err := requireUTCJobTime(result.EvaluatedAt, "deployment evaluated_at"); err != nil {
		return err
	}
	if err := validateDeploymentSummary(result.Item); err != nil {
		return err
	}
	if result.Item.DeploymentID != deploymentID {
		return errors.New("operator client: deployment detail target mismatch")
	}
	counts := make(map[deploy.JobState]int)
	batchTargets := make(map[int]int)
	batchJobs := make(map[int]int)
	opened, total, terminalStuck, silentStuck := 0, 0, 0, 0
	seenMachines := make(map[string]bool, len(result.Targets))
	seenJobs := make(map[string]bool, len(result.Targets))
	nonterminal, retryTargets, unopened := 0, 0, 0
	independentCounts := make(map[string]int, len(knownIndependentVerdicts))
	independentOpened, independentLive := 0, 0
	previousName, previousID := "", ""
	for i, target := range result.Targets {
		if err := validateDeploymentClientIdentifier("machine_id", target.MachineID, 256); err != nil ||
			validateDeploymentClientText("display_name", target.DisplayName, 256, false) != nil ||
			seenMachines[target.MachineID] || target.BatchNo < 0 {
			return errors.New("operator client: invalid or duplicate deployment target")
		}
		if i > 0 && (target.DisplayName < previousName ||
			(target.DisplayName == previousName && target.MachineID <= previousID)) {
			return errors.New("operator client: deployment targets are not canonically ordered")
		}
		previousName, previousID = target.DisplayName, target.MachineID
		seenMachines[target.MachineID] = true
		if target.ExcludedReason != nil {
			if target.BatchNo != 0 || target.JobID != nil || target.JobState != nil ||
				target.JobCreatedAt != nil || target.TerminalAt != nil || target.LastActivityAt != nil ||
				target.StuckKind != nil || target.Independent != nil ||
				!validDeploymentExclusion(*target.ExcludedReason) {
				return errors.New("operator client: excluded deployment target has a job")
			}
			continue
		}
		if target.BatchNo < 1 {
			return errors.New("operator client: included deployment target has no batch")
		}
		batchTargets[target.BatchNo]++
		if batchTargets[target.BatchNo] > result.Item.BatchSize {
			return errors.New("operator client: deployment target batch exceeds batch_size")
		}
		if target.BatchNo > total {
			total = target.BatchNo
		}
		if target.JobID == nil {
			unopened++
			if target.JobState != nil || target.JobCreatedAt != nil || target.TerminalAt != nil ||
				target.LastActivityAt != nil || target.StuckKind != nil || target.Independent != nil {
				return errors.New("operator client: unopened target contains job evidence")
			}
			continue
		}
		if err := validateDeploymentClientIdentifier("job_id", *target.JobID, 256); err != nil || seenJobs[*target.JobID] ||
			target.JobState == nil || !deploy.IsKnownJobState(*target.JobState) ||
			target.JobCreatedAt == nil || target.JobCreatedAt.IsZero() ||
			target.LastActivityAt == nil || target.LastActivityAt.IsZero() ||
			deploy.IsTerminal(*target.JobState) != (target.TerminalAt != nil) {
			return errors.New("operator client: opened target job is invalid")
		}
		seenJobs[*target.JobID] = true
		if err := requireUTCJobTime(*target.JobCreatedAt, "deployment target job_created_at"); err != nil {
			return err
		}
		if err := requireUTCJobTime(*target.LastActivityAt, "deployment target last_activity_at"); err != nil {
			return err
		}
		if target.TerminalAt != nil {
			if target.TerminalAt.IsZero() {
				return errors.New("operator client: deployment target terminal_at is zero")
			}
			if err := requireUTCJobTime(*target.TerminalAt, "deployment target terminal_at"); err != nil {
				return err
			}
		}
		counts[*target.JobState]++
		batchJobs[target.BatchNo]++
		if target.BatchNo > opened {
			opened = target.BatchNo
		}
		isSilentStuck := !deploy.IsTerminal(*target.JobState) &&
			result.EvaluatedAt.Sub(*target.LastActivityAt) > store.StuckThreshold
		if target.StuckKind != nil {
			switch *target.StuckKind {
			case "terminal_failure":
				if !deploy.IsTerminal(*target.JobState) || *target.JobState == deploy.Succeeded {
					return errors.New("operator client: terminal_failure target is not a failed terminal job")
				}
				terminalStuck++
				retryTargets++
			case "no_event":
				if !isSilentStuck {
					return errors.New("operator client: no_event target has fresh activity or is terminal")
				}
				silentStuck++
			default:
				return errors.New("operator client: invalid target stuck classification")
			}
		}
		if deploy.IsTerminal(*target.JobState) && *target.JobState != deploy.Succeeded && target.StuckKind == nil {
			return errors.New("operator client: terminal failure lacks stuck classification")
		}
		if isSilentStuck && target.StuckKind == nil {
			return errors.New("operator client: silent stuck target lacks stuck classification")
		}
		if !deploy.IsTerminal(*target.JobState) {
			nonterminal++
		}
		if err := validateDeploymentTargetIndependent(target.Independent); err != nil {
			return err
		}
		independentCounts[target.Independent.Verdict]++
		independentOpened++
		independentLive += target.Independent.LiveProducers
	}
	if err := validateDeploymentIndependentSummary(result.Independent,
		independentCounts, independentOpened, independentLive); err != nil {
		return err
	}
	if total == 0 || len(batchTargets) != total {
		return errors.New("operator client: deployment target batches are not contiguous")
	}
	for batchNo := 1; batchNo <= opened; batchNo++ {
		if batchJobs[batchNo] != batchTargets[batchNo] {
			return errors.New("operator client: opened deployment prefix contains an unopened target")
		}
	}
	if opened != result.Item.OpenedBatch || total != result.Item.TotalBatches ||
		terminalStuck != result.Item.TerminalStuck || silentStuck != result.Item.SilentStuck {
		return errors.New("operator client: target aggregates contradict deployment summary")
	}
	for _, stateCount := range result.Item.JobStateCounts {
		if counts[stateCount.State] != stateCount.Count {
			return errors.New("operator client: target job states contradict summary counts")
		}
	}
	continueAffected := 0
	if opened < total {
		continueAffected = batchTargets[opened+1] - batchJobs[opened+1]
	}
	continueOutcome := "open_next_batch"
	if opened >= total {
		continueOutcome = "finish"
	}
	if err := validateDeploymentDetailActionEligibility("continue", result.Actions.Continue, continueOutcome,
		continueAffected, deploymentContinueBlockers); err != nil {
		return err
	}
	if err := validateDeploymentDetailActionEligibility("retry", result.Actions.Retry, "create_retry_attempt",
		retryTargets, deploymentRetryBlockers); err != nil {
		return err
	}
	if err := validateDeploymentDetailActionEligibility("abandon", result.Actions.Abandon, "finish_without_unopened_batches",
		unopened, deploymentAbandonBlockers); err != nil {
		return err
	}
	if err := validateDeploymentVisibleActionBlockers(result.Item, result.Actions, nonterminal,
		retryTargets, continueAffected); err != nil {
		return err
	}
	return nil
}

func validateDeploymentSummary(item operator.DeploymentSummary) error {
	if err := validateDeploymentClientIdentifier("deployment_id", item.DeploymentID, 256); err != nil {
		return err
	}
	if err := validateDeploymentClientIdentifier("desired_id", item.DesiredID, 256); err != nil {
		return err
	}
	if err := validateDeploymentClientIdentifier("resource_kind", item.ResourceKind, 128); err != nil {
		return err
	}
	if err := validateDeploymentClientIdentifier("resource_id", item.ResourceID, 256); err != nil {
		return err
	}
	if item.Channel != "canary" && item.Channel != "stable" {
		return errors.New("operator client: invalid deployment channel")
	}
	if item.State != store.DeploymentRunning && item.State != store.DeploymentPaused && item.State != store.DeploymentFinished {
		return errors.New("operator client: invalid deployment state")
	}
	if item.DesiredRevision <= 0 || item.ControlRevision < 0 || item.BatchSize < 1 ||
		item.BatchSize > store.MaxDeploymentBatchSize || item.CreatedAt.IsZero() ||
		item.OpenedBatch < 1 || item.TotalBatches < 1 || item.OpenedBatch > item.TotalBatches ||
		item.Attempt < 1 || item.Stuck < 0 || item.TerminalStuck < 0 || item.SilentStuck < 0 ||
		item.Stuck != item.TerminalStuck+item.SilentStuck || item.JobStateCounts == nil ||
		len(item.JobStateCounts) != len(deploy.AllJobStates) {
		return errors.New("operator client: incoherent deployment summary")
	}
	if err := requireUTCJobTime(item.CreatedAt, "deployment created_at"); err != nil {
		return err
	}
	jobTotal, terminalFailureJobs, nonterminalJobs := 0, 0, 0
	for i, count := range item.JobStateCounts {
		if count.State != deploy.AllJobStates[i] || count.Count < 0 {
			return errors.New("operator client: deployment job state counts are not canonical")
		}
		jobTotal += count.Count
		if deploy.IsTerminal(count.State) {
			if count.State != deploy.Succeeded {
				terminalFailureJobs += count.Count
			}
		} else {
			nonterminalJobs += count.Count
		}
	}
	if (item.OpenedBatch > 0 && jobTotal == 0) || jobTotal > item.OpenedBatch*item.BatchSize ||
		item.TerminalStuck > terminalFailureJobs || item.SilentStuck > nonterminalJobs {
		return errors.New("operator client: deployment job counts contradict opened batches or stuck counts")
	}
	if item.State == store.DeploymentPaused && item.PausedAt == nil {
		return errors.New("operator client: paused deployment lacks paused_at")
	}
	if item.State == store.DeploymentFinished && item.FinishedAt == nil {
		return errors.New("operator client: finished deployment lacks finished_at")
	}
	if item.State != store.DeploymentFinished && item.FinishedAt != nil {
		return errors.New("operator client: non-finished deployment has finished_at")
	}
	for name, value := range map[string]*time.Time{
		"deployment paused_at": item.PausedAt, "deployment finished_at": item.FinishedAt,
	} {
		if value != nil {
			if value.IsZero() {
				return fmt.Errorf("operator client: %s is zero", name)
			}
			if err := requireUTCJobTime(*value, name); err != nil {
				return err
			}
		}
	}
	if item.RetryOf != nil {
		if err := validateDeploymentClientIdentifier("retry_of", *item.RetryOf, 256); err != nil {
			return err
		}
	}
	if item.Material.Kind != item.ResourceKind || (item.Material.Status != operator.DeploymentMaterialRecorded &&
		item.Material.Status != operator.DeploymentMaterialInvalid && item.Material.Status != operator.DeploymentMaterialUnsupported) {
		return errors.New("operator client: invalid deployment material classification")
	}
	switch item.Material.Status {
	case operator.DeploymentMaterialRecorded:
		if item.ResourceKind != "openclaw" || item.ResourceID != "openclaw" || item.Material.Version == nil ||
			validateDeploymentClientIdentifier("material.version", valueOrEmpty(item.Material.Version), 128) != nil ||
			item.Material.ArtifactDigest == nil || !validSHA256Digest(*item.Material.ArtifactDigest) ||
			item.Material.SizeBytes == nil || *item.Material.SizeBytes < 0 {
			return errors.New("operator client: incomplete recorded deployment material")
		}
		if item.Material.EnginesNode != nil && (*item.Material.EnginesNode == "" ||
			validateDeploymentClientText("material.engines_node", *item.Material.EnginesNode, 512, true) != nil) {
			return errors.New("operator client: invalid deployment material engines")
		}
	case operator.DeploymentMaterialInvalid:
		if item.ResourceKind != "openclaw" || deploymentMaterialHasRecordedFields(item.Material) {
			return errors.New("operator client: invalid material classification carries recorded fields")
		}
	case operator.DeploymentMaterialUnsupported:
		if item.ResourceKind == "openclaw" || deploymentMaterialHasRecordedFields(item.Material) {
			return errors.New("operator client: unsupported material classification is incoherent")
		}
	}
	if item.BoundaryPause != nil {
		if item.BoundaryPause.OpenedBatch < 1 || item.BoundaryPause.OpenedBatch > item.OpenedBatch ||
			!validDeploymentPauseKind(item.BoundaryPause.Kind) || item.BoundaryPause.PausedAt.IsZero() {
			return errors.New("operator client: invalid deployment boundary pause")
		}
		if err := requireUTCJobTime(item.BoundaryPause.PausedAt, "deployment boundary paused_at"); err != nil {
			return err
		}
	}
	return nil
}

func validateDeploymentCreatePreviewResult(result operator.DeploymentCreatePreviewResult,
	request operator.DeploymentCreatePreviewRequest,
) error {
	if result.SchemaVersion != operator.DeploymentPreviewSchemaVersion || result.PreviewedAt.IsZero() ||
		result.Targets == nil || result.Blockers == nil || !validSHA256Digest(result.PreviewDigest) {
		return errors.New("operator client: invalid deployment preview shape")
	}
	if err := requireUTCJobTime(result.PreviewedAt, "deployment previewed_at"); err != nil {
		return err
	}
	wantBatch := request.BatchSize
	if wantBatch == 0 {
		wantBatch = operator.DefaultDeploymentBatchSize
	}
	wantTimeout := request.ExecutionTimeoutSeconds
	if wantTimeout == 0 {
		wantTimeout = operator.DefaultDeploymentTimeout
	}
	if result.Channel != request.Channel || result.BatchSize != wantBatch ||
		result.ExecutionTimeoutSeconds != wantTimeout || result.Irreversible != request.Irreversible ||
		result.Artifact.Name != "openclaw" || result.Artifact.Version != request.Version || !result.Artifact.AvailableAndVerified ||
		!artifact.ValidSHA256Hex(result.Artifact.SHA256) || result.Artifact.Digest != "sha256:"+result.Artifact.SHA256 ||
		result.Artifact.SizeBytes < 0 || result.Artifact.FetchedAt.IsZero() ||
		validateDeploymentClientIdentifier("artifact.version", result.Artifact.Version, 128) != nil ||
		validateDeploymentClientText("artifact.engines_node", result.Artifact.EnginesNode, 512, true) != nil ||
		validateDeploymentClientText("artifact.sha512_integrity", result.Artifact.SHA512Integrity, 1024, false) != nil {
		return errors.New("operator client: deployment preview does not match request/material contract")
	}
	if err := requireUTCJobTime(result.Artifact.FetchedAt, "deployment artifact fetched_at"); err != nil {
		return err
	}
	if request.ArtifactSHA256 != "" && result.Artifact.SHA256 != request.ArtifactSHA256 {
		return errors.New("operator client: deployment preview selected a different artifact")
	}
	impact, conflicts, missing, unknown, noncompliant, unreachable, totalBatches := 0, 0, 0, 0, 0, 0, 0
	seen := make(map[string]bool, len(result.Targets))
	previousName, previousID := "", ""
	for i, target := range result.Targets {
		if validateDeploymentClientIdentifier("preview machine_id", target.MachineID, 256) != nil ||
			validateDeploymentClientText("preview display_name", target.DisplayName, 256, false) != nil ||
			seen[target.MachineID] {
			return errors.New("operator client: invalid or duplicate preview target")
		}
		if i > 0 && (target.DisplayName < previousName ||
			(target.DisplayName == previousName && target.MachineID <= previousID)) {
			return errors.New("operator client: preview targets are not canonically ordered")
		}
		previousName, previousID = target.DisplayName, target.MachineID
		seen[target.MachineID] = true
		if target.NodeVersion != nil {
			if *target.NodeVersion == "" ||
				validateDeploymentClientText("preview node_version", *target.NodeVersion, 128, true) != nil {
				return errors.New("operator client: invalid preview node version")
			}
		}
		if target.ExcludedReason != nil {
			if target.BatchNo != 0 {
				return errors.New("operator client: excluded preview target has a batch")
			}
			switch *target.ExcludedReason {
			case "conflict":
				conflicts++
			case "missing_package":
				missing++
			case "unknown_node":
				unknown++
			case "noncompliant":
				noncompliant++
			default:
				return errors.New("operator client: invalid preview exclusion")
			}
			continue
		}
		impact++
		if target.BatchNo != (impact-1)/result.BatchSize+1 {
			return errors.New("operator client: preview batches are not canonical")
		}
		if target.BatchNo > totalBatches {
			totalBatches = target.BatchNo
		}
		if !target.Reachable {
			unreachable++
		}
	}
	if impact != result.Impact || conflicts != result.Conflicts || missing != result.MissingPackages ||
		unknown != result.UnknownNodes || noncompliant != result.Noncompliant ||
		unreachable != result.Unreachable || totalBatches != result.TotalBatches {
		return errors.New("operator client: deployment preview aggregates contradict targets")
	}
	if result.Channel == "canary" && result.Promotion != nil {
		return errors.New("operator client: canary preview must not contain promotion decision")
	}
	if result.Channel == "stable" && (result.Promotion == nil || result.Promotion.Blockers == nil) {
		return errors.New("operator client: stable preview lacks promotion decision")
	}
	if result.Promotion != nil {
		if err := validateDeploymentPromotionPreview(*result.Promotion, result.PreviewedAt); err != nil {
			return err
		}
	}
	expectedBlockers := make([]string, 0, 2)
	if result.Impact == 0 {
		expectedBlockers = append(expectedBlockers, "no_included_targets")
	}
	if result.Promotion != nil && !result.Promotion.Allowed {
		expectedBlockers = append(expectedBlockers, "stable_promotion_locked")
	}
	allowed := len(expectedBlockers) == 0
	if !equalStrings(result.Blockers, expectedBlockers) || result.CreateAllowed != allowed {
		return errors.New("operator client: deployment preview allowed/blockers mismatch")
	}
	if deploymentClientPreviewDigest(result) != result.PreviewDigest {
		return errors.New("operator client: deployment preview digest does not match canonical content")
	}
	return nil
}

func validateDeploymentPromotionPreview(value operator.DeploymentPromotionPreview, evaluatedAt time.Time) error {
	if value.Blockers == nil || value.IndependentTargets == nil || !value.IndependentRequired ||
		value.Allowed != (len(value.Blockers) == 0) ||
		(!value.Allowed && !equalStrings(value.Blockers, []string{"stable_promotion_locked"})) {
		return errors.New("operator client: deployment promotion contract is incoherent")
	}
	seenMachines := make(map[string]bool, len(value.IndependentTargets))
	seenJobs := make(map[string]bool, len(value.IndependentTargets))
	passed := 0
	blockedByIndependent := false
	for _, target := range value.IndependentTargets {
		if validateDeploymentClientIdentifier("promotion machine_id", target.MachineID, 256) != nil ||
			validateDeploymentClientText("promotion display_name", target.DisplayName, 256, false) != nil ||
			validateDeploymentClientIdentifier("promotion job_id", target.JobID, 256) != nil ||
			seenMachines[target.MachineID] || seenJobs[target.JobID] {
			return errors.New("operator client: deployment promotion target identity is invalid or duplicated")
		}
		seenMachines[target.MachineID], seenJobs[target.JobID] = true, true
		wantStep, ok := promotionIndependentNextStep(target.State)
		if !ok || target.NextStep != wantStep {
			return errors.New("operator client: deployment promotion independent state/next_step is invalid")
		}
		if target.State == "passed" {
			passed++
		} else {
			blockedByIndependent = true
		}
	}
	if passed != value.IndependentPassedTargets || passed < 0 || passed > len(value.IndependentTargets) ||
		(value.Allowed && (blockedByIndependent || len(value.IndependentTargets) == 0)) {
		return errors.New("operator client: deployment promotion independent tally is incoherent")
	}
	if value.EarliestAt != nil {
		if value.Allowed || value.EarliestAt.IsZero() || !value.EarliestAt.After(evaluatedAt) {
			return errors.New("operator client: invalid deployment promotion earliest_at")
		}
		if err := requireUTCJobTime(*value.EarliestAt, "deployment promotion earliest_at"); err != nil {
			return err
		}
	}
	return nil
}

func promotionIndependentNextStep(state string) (string, bool) {
	switch state {
	case "unassigned":
		return operator.PromotionNextStepAssignVerifier, true
	case "awaiting_report":
		return operator.PromotionNextStepWaitForVerifier, true
	case "incomplete_report":
		return operator.PromotionNextStepRerunVerifier, true
	case "producer_revoked":
		return operator.PromotionNextStepAssignActiveVerifier, true
	case "digest_mismatch":
		return operator.PromotionNextStepRerunCanary, true
	case "release_mismatch":
		return operator.PromotionNextStepRepairAndRerunCanary, true
	case "release_unreported":
		return operator.PromotionNextStepUpgradeAndReassignVerifier, true
	case "stale":
		return operator.PromotionNextStepReassignVerifier, true
	case "failed":
		return operator.PromotionNextStepRepairAndRerunCanary, true
	case "passed":
		return operator.PromotionNextStepNone, true
	default:
		return "", false
	}
}

func validateDeploymentCreatePreviewRequest(request operator.DeploymentCreatePreviewRequest) error {
	if request.Channel != strings.TrimSpace(request.Channel) || request.Version != strings.TrimSpace(request.Version) ||
		request.ArtifactSHA256 != strings.TrimSpace(request.ArtifactSHA256) {
		return errors.New("operator client: deployment preview inputs must not have surrounding whitespace")
	}
	if request.Channel != "canary" && request.Channel != "stable" {
		return errors.New("operator client: deployment channel must be canary or stable")
	}
	if err := validateDeploymentClientIdentifier("version", request.Version, 128); err != nil {
		return err
	}
	if request.ArtifactSHA256 != "" && !artifact.ValidSHA256Hex(request.ArtifactSHA256) {
		return errors.New("operator client: artifact_sha256 must be canonical lowercase SHA-256")
	}
	if request.BatchSize < 1 || request.BatchSize > store.MaxDeploymentBatchSize {
		return fmt.Errorf("operator client: deployment batch_size must be between 1 and %d", store.MaxDeploymentBatchSize)
	}
	if request.ExecutionTimeoutSeconds < 1 || request.ExecutionTimeoutSeconds > 86400 {
		return errors.New("operator client: deployment execution_timeout_seconds must be between 1 and 86400")
	}
	return nil
}

type deploymentClientListCursor struct {
	Version      int       `json:"v"`
	FilterDigest string    `json:"filter_digest"`
	CreatedAt    time.Time `json:"created_at"`
	DeploymentID string    `json:"deployment_id"`
}

func decodeDeploymentClientListCursor(encoded string, request operator.DeploymentListRequest) (deploymentClientListCursor, error) {
	if err := validateJobReadIdentifier("deployment cursor", encoded, 2048); err != nil {
		return deploymentClientListCursor{}, err
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || base64.RawURLEncoding.EncodeToString(raw) != encoded {
		return deploymentClientListCursor{}, errors.New("operator client: deployment cursor encoding is invalid")
	}
	var cursor deploymentClientListCursor
	if err := decodeStrictJSONDocument(raw, "deployment cursor", &cursor); err != nil {
		return deploymentClientListCursor{}, err
	}
	canonical, err := json.Marshal(cursor)
	if err != nil || base64.RawURLEncoding.EncodeToString(canonical) != encoded ||
		cursor.Version != operator.DeploymentReadSchemaVersion ||
		cursor.FilterDigest != deploymentClientFilterDigest(request) || cursor.CreatedAt.IsZero() ||
		validateDeploymentClientIdentifier("cursor deployment_id", cursor.DeploymentID, 256) != nil {
		return deploymentClientListCursor{}, errors.New("operator client: deployment cursor does not match this query")
	}
	if err := requireUTCJobTime(cursor.CreatedAt, "deployment cursor created_at"); err != nil {
		return deploymentClientListCursor{}, err
	}
	return cursor, nil
}

func deploymentClientFilterDigest(request operator.DeploymentListRequest) string {
	states := append([]string(nil), request.States...)
	sort.Slice(states, func(i, j int) bool {
		return deploymentClientStateOrder(states[i]) < deploymentClientStateOrder(states[j])
	})
	stuck := "any"
	if request.Stuck != nil {
		stuck = strconv.FormatBool(*request.Stuck)
	}
	body := struct {
		Version int      `json:"v"`
		Channel string   `json:"channel"`
		States  []string `json:"states"`
		Stuck   string   `json:"stuck"`
	}{operator.DeploymentReadSchemaVersion, request.Channel, states, stuck}
	raw, _ := json.Marshal(body)
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func deploymentClientStateOrder(state string) int {
	switch state {
	case store.DeploymentRunning:
		return 0
	case store.DeploymentPaused:
		return 1
	case store.DeploymentFinished:
		return 2
	default:
		return 3
	}
}

func deploymentClientPreviewDigest(result operator.DeploymentCreatePreviewResult) string {
	type artifactIdentity struct {
		Name          string `json:"name"`
		Version       string `json:"version"`
		SHA256        string `json:"sha256"`
		SizeBytes     int64  `json:"size_bytes"`
		EnginesNode   string `json:"engines_node"`
		SHA512        string `json:"sha512_integrity"`
		BytesVerified bool   `json:"bytes_verified"`
	}
	body := struct {
		Version                 int                                    `json:"version"`
		Channel                 string                                 `json:"channel"`
		BatchSize               int                                    `json:"batch_size"`
		ExecutionTimeoutSeconds int                                    `json:"execution_timeout_seconds"`
		Irreversible            bool                                   `json:"irreversible"`
		Artifact                artifactIdentity                       `json:"artifact"`
		Targets                 []operator.DeploymentPlanTargetPreview `json:"targets"`
		Impact                  int                                    `json:"impact"`
		Conflicts               int                                    `json:"conflicts"`
		MissingPackages         int                                    `json:"missing_packages"`
		UnknownNodes            int                                    `json:"unknown_nodes"`
		Noncompliant            int                                    `json:"noncompliant"`
		Unreachable             int                                    `json:"unreachable"`
		TotalBatches            int                                    `json:"total_batches"`
		Promotion               *operator.DeploymentPromotionPreview   `json:"promotion"`
		CreateAllowed           bool                                   `json:"create_allowed"`
		Blockers                []string                               `json:"blockers"`
	}{
		Version: operator.DeploymentPreviewSchemaVersion, Channel: result.Channel, BatchSize: result.BatchSize,
		ExecutionTimeoutSeconds: result.ExecutionTimeoutSeconds, Irreversible: result.Irreversible,
		Artifact: artifactIdentity{
			Name: result.Artifact.Name, Version: result.Artifact.Version,
			SHA256: result.Artifact.SHA256, SizeBytes: result.Artifact.SizeBytes,
			EnginesNode: result.Artifact.EnginesNode, SHA512: result.Artifact.SHA512Integrity,
			BytesVerified: result.Artifact.AvailableAndVerified,
		}, Targets: result.Targets,
		Impact: result.Impact, Conflicts: result.Conflicts, MissingPackages: result.MissingPackages,
		UnknownNodes: result.UnknownNodes, Noncompliant: result.Noncompliant,
		Unreachable: result.Unreachable, TotalBatches: result.TotalBatches,
		Promotion: result.Promotion, CreateAllowed: result.CreateAllowed, Blockers: result.Blockers,
	}
	raw, _ := json.Marshal(body)
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

var deploymentContinueBlockers = map[string]int{
	"deployment_not_paused": 0, "nonterminal_jobs": 1, "no_opened_batch": 2,
	"next_batch_empty": 3, "invalid_material": 4, "stable_promotion_locked": 5,
	"active_resource_deployment": 6, "control_revision_exhausted": 7,
}

var deploymentRetryBlockers = map[string]int{
	"deployment_not_retryable": 0, "nonterminal_jobs": 1, "no_terminal_failure_targets": 2,
	"no_opened_batch": 3, "invalid_material": 4, "stable_promotion_locked": 5,
	"active_resource_deployment": 6, "control_revision_exhausted": 7,
}

var deploymentAbandonBlockers = map[string]int{
	"deployment_not_paused": 0, "nonterminal_jobs": 1, "active_resource_deployment": 2,
	"control_revision_exhausted": 3,
}

func validateDeploymentDetailActionEligibility(name string, action operator.DeploymentActionEligibility, outcome string,
	affected int, allowed map[string]int,
) error {
	if action.AffectedTargets != affected {
		return fmt.Errorf("operator client: deployment %s eligibility is incoherent", name)
	}
	return validateDeploymentActionEligibility(action, outcome, allowed)
}

func validateDeploymentActionEligibility(action operator.DeploymentActionEligibility, outcome string,
	allowed map[string]int,
) error {
	if action.Blockers == nil || action.Outcome != outcome || action.AffectedTargets < 0 ||
		action.Eligible != (len(action.Blockers) == 0) {
		return errors.New("operator client: deployment action eligibility is incoherent")
	}
	previousRank := -1
	for _, blocker := range action.Blockers {
		rank, ok := allowed[blocker]
		if !ok || rank <= previousRank {
			return errors.New("operator client: deployment action blockers are invalid or noncanonical")
		}
		previousRank = rank
	}
	return nil
}

func validateDeploymentVisibleActionBlockers(item operator.DeploymentSummary,
	actions operator.DeploymentActionEligibilitySet, nonterminal, retryTargets, continueAffected int,
) error {
	checks := []struct {
		name     string
		action   operator.DeploymentActionEligibility
		blocker  string
		expected bool
	}{
		{"continue", actions.Continue, "deployment_not_paused", item.State != store.DeploymentPaused},
		{"continue", actions.Continue, "nonterminal_jobs", nonterminal > 0},
		{"continue", actions.Continue, "no_opened_batch", item.OpenedBatch == 0},
		{"continue", actions.Continue, "next_batch_empty", item.OpenedBatch < item.TotalBatches && continueAffected == 0},
		{"continue", actions.Continue, "invalid_material", item.OpenedBatch < item.TotalBatches && item.Material.Status != operator.DeploymentMaterialRecorded},
		{"retry", actions.Retry, "deployment_not_retryable", item.State != store.DeploymentPaused && item.State != store.DeploymentFinished},
		{"retry", actions.Retry, "nonterminal_jobs", nonterminal > 0},
		{"retry", actions.Retry, "no_terminal_failure_targets", retryTargets == 0},
		{"retry", actions.Retry, "no_opened_batch", item.OpenedBatch == 0},
		{"retry", actions.Retry, "invalid_material", item.Material.Status != operator.DeploymentMaterialRecorded},
		{"abandon", actions.Abandon, "deployment_not_paused", item.State != store.DeploymentPaused},
		{"abandon", actions.Abandon, "nonterminal_jobs", nonterminal > 0},
		{"continue", actions.Continue, "control_revision_exhausted", item.ControlRevision == store.MaxDeploymentControlRevision},
		{"retry", actions.Retry, "control_revision_exhausted", item.ControlRevision == store.MaxDeploymentControlRevision},
		{"abandon", actions.Abandon, "control_revision_exhausted", item.ControlRevision == store.MaxDeploymentControlRevision},
	}
	for _, check := range checks {
		if deploymentHasBlocker(check.action, check.blocker) != check.expected {
			return fmt.Errorf("operator client: deployment %s blocker %s contradicts visible state", check.name, check.blocker)
		}
	}
	stableRetry := deploymentHasBlocker(actions.Retry, "stable_promotion_locked")
	stableContinue := deploymentHasBlocker(actions.Continue, "stable_promotion_locked")
	if (stableRetry && (item.Channel != "stable" || item.Material.Status != operator.DeploymentMaterialRecorded)) ||
		stableContinue != (stableRetry && item.OpenedBatch < item.TotalBatches) {
		return errors.New("operator client: stable promotion blockers are incoherent")
	}
	activeContinue := deploymentHasBlocker(actions.Continue, "active_resource_deployment")
	activeRetry := deploymentHasBlocker(actions.Retry, "active_resource_deployment")
	activeAbandon := deploymentHasBlocker(actions.Abandon, "active_resource_deployment")
	if activeContinue != activeRetry || activeRetry != activeAbandon {
		return errors.New("operator client: active resource blockers are incoherent")
	}
	return nil
}

func deploymentHasBlocker(action operator.DeploymentActionEligibility, blocker string) bool {
	for _, candidate := range action.Blockers {
		if candidate == blocker {
			return true
		}
	}
	return false
}

func validateDeploymentClientIdentifier(name, value string, maxBytes int) error {
	if value == "" || len(value) > maxBytes || !utf8.ValidString(value) || value != strings.TrimSpace(value) ||
		value == "." || value == ".." || strings.Contains(value, "/") {
		return fmt.Errorf("operator client: %s is not a canonical identifier", name)
	}
	for _, char := range value {
		if unicode.IsControl(char) || unicode.Is(unicode.Cf, char) {
			return fmt.Errorf("operator client: %s contains control or format characters", name)
		}
	}
	return nil
}

func validateDeploymentClientText(name, value string, maxBytes int, allowEmpty bool) error {
	if len(value) > maxBytes || !utf8.ValidString(value) || (!allowEmpty && strings.TrimSpace(value) == "") {
		return fmt.Errorf("operator client: %s is not valid text", name)
	}
	for _, char := range value {
		if unicode.IsControl(char) || unicode.Is(unicode.Cf, char) {
			return fmt.Errorf("operator client: %s contains control or format characters", name)
		}
	}
	return nil
}

func validDeploymentExclusion(value string) bool {
	return value == "conflict" || value == "missing_package" || value == "unknown_node"
}

func validDeploymentPauseKind(value string) bool {
	switch value {
	case store.DeploymentPausePromoteLocked, store.DeploymentPauseConflict,
		store.DeploymentPauseStaleRevision, store.DeploymentPauseBatchNotReady,
		store.DeploymentPauseMaterial, store.DeploymentPauseInvalidPlan,
		store.DeploymentPauseTargetChanged, store.DeploymentPauseMachineRetired:
		return true
	default:
		return false
	}
}

func deploymentMaterialHasRecordedFields(material operator.DeploymentMaterialSummary) bool {
	return material.Version != nil || material.ArtifactDigest != nil || material.SizeBytes != nil || material.EnginesNode != nil
}

func valueOrEmpty(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func stringSelected(value string, options []string) bool {
	for _, option := range options {
		if option == value {
			return true
		}
	}
	return false
}

// validateDeploymentTargetIndependent requires every opened target to carry a
// verdict and requires that verdict to agree with the weight behind it. A
// server that reports passed with no live producer, or absent with rows, is
// contradicting itself and the client must not repeat it.
func validateDeploymentTargetIndependent(verdict *operator.DeploymentTargetIndependent) error {
	if verdict == nil {
		return errors.New("operator client: opened deployment target has no independent verdict")
	}
	if !knownIndependentVerdicts[verdict.Verdict] {
		return errors.New("operator client: deployment target independent verdict is unknown")
	}
	if verdict.Rows < 0 || verdict.LiveProducers < 0 || verdict.LiveProducers > verdict.Rows {
		return errors.New("operator client: deployment target independent counts are inconsistent")
	}
	switch {
	case (verdict.Rows == 0) != (verdict.Verdict == string(store.IndependentAbsent)):
		return errors.New("operator client: deployment target independent evidence cannot be absent and present")
	case verdict.Rows > 0 && verdict.LiveProducers == 0 &&
		verdict.Verdict != string(store.IndependentProducerRevoked):
		return errors.New("operator client: deployment target with no live producer must be producer_revoked")
	}
	return nil
}

// validateDeploymentIndependentSummary recomputes the rollup from the targets.
// The summary is a convenience, never a second source of truth: if it does not
// match what the targets say, the client refuses the whole response rather than
// pick one of the two answers.
func validateDeploymentIndependentSummary(summary operator.DeploymentIndependentSummary,
	counts map[string]int, opened, live int,
) error {
	if summary.Verdicts == nil {
		return errors.New("operator client: deployment independent arrays must not be null")
	}
	if summary.OpenedTargets != opened || summary.LiveProducers != live ||
		summary.PassedTargets != counts[string(store.IndependentPassed)] {
		return errors.New("operator client: deployment independent summary contradicts its targets")
	}
	if len(summary.Verdicts) != len(knownIndependentVerdicts) {
		return errors.New("operator client: deployment independent summary does not list every verdict")
	}
	seen := make(map[string]bool, len(summary.Verdicts))
	listed := 0
	for _, verdict := range summary.Verdicts {
		if !knownIndependentVerdicts[verdict.Verdict] || seen[verdict.Verdict] ||
			verdict.Targets != counts[verdict.Verdict] {
			return errors.New("operator client: deployment independent verdict counts contradict its targets")
		}
		seen[verdict.Verdict] = true
		listed += verdict.Targets
	}
	if listed != opened {
		return errors.New("operator client: deployment independent verdict counts do not cover every opened target")
	}
	return nil
}
