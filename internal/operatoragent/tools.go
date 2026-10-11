package operatoragent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/teddashh/AI-Intune/internal/deploy"
	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/operatorclient"
	"github.com/teddashh/AI-Intune/internal/rollout"
	"github.com/teddashh/AI-Intune/internal/state"
)

// Tool is one MCP/CLI operation. InputSchema is a JSON Schema object.
// Annotations is set on the disk-clean tools. Existing tools leave it empty
// so a later merge that adds the same field stays small.
type Tool struct {
	Name        string           `json:"name"`
	Description string           `json:"description"`
	InputSchema map[string]any   `json:"inputSchema"`
	Annotations *ToolAnnotations `json:"annotations,omitempty"`
}

// ToolAnnotations is the MCP tools/list annotations object. Bool fields are
// always encoded so a false hint is visible to the client.
type ToolAnnotations struct {
	Title           string `json:"title,omitempty"`
	ReadOnlyHint    bool   `json:"readOnlyHint"`
	DestructiveHint bool   `json:"destructiveHint"`
	IdempotentHint  bool   `json:"idempotentHint"`
	OpenWorldHint   bool   `json:"openWorldHint"`
}

// CallError is a tool failure. Code is stable. The Hub was not called when
// Code is preview_digest_required, expected_revision_required, canary_blocked,
// or invalid_arguments.
type CallError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Detail  any    `json:"detail,omitempty"`
}

func (e *CallError) Error() string {
	if e == nil {
		return "operator agent: nil error"
	}
	return e.Code + ": " + e.Message
}

// Service calls Hub. A nil Hub fails closed on every call.
// Version is the build version reported by MCP initialize. Empty means dev.
type Service struct {
	Hub     Hub
	Version string
}

func (s *Service) Call(ctx context.Context, name string, raw json.RawMessage) (any, error) {
	if s == nil || s.Hub == nil {
		return nil, &CallError{Code: "not_configured", Message: "operator Hub client is not configured"}
	}
	if ctx == nil {
		return nil, &CallError{Code: "invalid_arguments", Message: "context is required"}
	}
	switch name {
	case "jobs_summary":
		var args operator.SupervisionFilter
		if err := decodeSupervisionArgs(raw, &args); err != nil {
			return nil, err
		}
		if err := operator.ValidateSupervisionFilter(args); err != nil {
			return nil, &CallError{Code: "invalid_arguments", Message: "invalid supervision filter"}
		}
		result, err := s.Hub.JobsSummary(ctx, args)
		return result, hubErr(err)
	case "approvals_summary":
		var args struct {
			Window string `json:"window"`
		}
		if err := decodeSupervisionArgs(raw, &args); err != nil {
			return nil, err
		}
		if err := operator.ValidateSupervisionFilter(operator.SupervisionFilter{Window: args.Window}); err != nil {
			return nil, &CallError{Code: "invalid_arguments", Message: "invalid supervision window"}
		}
		result, err := s.Hub.ApprovalsSummary(ctx, args.Window)
		return result, hubErr(err)
	case "hub_status":
		if err := decodeSupervisionArgs(raw, &struct{}{}); err != nil {
			return nil, err
		}
		result, err := s.Hub.HubStatus(ctx)
		return result, hubErr(err)
	case "fleet_overview":
		return s.fleetOverview(ctx, raw)
	case "machines_list":
		return s.machinesList(ctx, raw)
	case "machine_get":
		return s.machineGet(ctx, raw)
	case "machine_evidence":
		return s.machineEvidence(ctx, raw)
	case "jobs_list":
		return s.jobsList(ctx, raw)
	case "job_get":
		return s.jobGet(ctx, raw)
	case "job_evidence":
		return s.jobEvidence(ctx, raw)
	case "deployments_list":
		return s.deploymentsList(ctx, raw)
	case "deployment_get":
		return s.deploymentGet(ctx, raw)
	case "software_report":
		return s.softwareReport(ctx, raw)
	case "compliance":
		return s.compliance(ctx, raw)
	case "rollout_status":
		return s.rolloutStatus(ctx, raw)
	case "rollout_preview":
		return s.rolloutPreview(ctx, raw)
	case "rollout_apply":
		return s.rolloutApply(ctx, raw)
	case "rollout_expand":
		return s.rolloutExpand(ctx, raw)
	case "enroll_ticket_preview":
		return s.enrollPreview(ctx, raw)
	case "enroll_ticket_create":
		return s.enrollCreate(ctx, raw)
	case "deployment_create_preview":
		return s.deploymentCreatePreview(ctx, raw)
	case "deployment_create":
		return s.deploymentCreate(ctx, raw)
	case "deployment_continue_preview":
		return s.deploymentContinuePreview(ctx, raw)
	case "deployment_continue":
		return s.deploymentContinue(ctx, raw)
	case "deployment_abandon_preview":
		return s.deploymentAbandonPreview(ctx, raw)
	case "deployment_abandon":
		return s.deploymentAbandon(ctx, raw)
	case "profile_assignment_preview":
		return s.profilePreview(ctx, raw)
	case "profile_assignment_apply":
		return s.profileApply(ctx, raw)
	case "disk_clean_summaries":
		return s.diskCleanSummaries(ctx, raw)
	case "disk_clean_summary":
		return s.diskCleanSummary(ctx, raw)
	case "disk_clean_profile_preview":
		return s.diskCleanProfilePreview(ctx, raw)
	case "disk_clean_profile_publish":
		return s.diskCleanProfilePublish(ctx, raw)
	case "disk_clean_dry_run_preview":
		return s.diskCleanDryRunPreview(ctx, raw)
	case "disk_clean_dry_run_apply":
		return s.diskCleanDryRunApply(ctx, raw)
	case "disk_clean_canary_preview":
		return s.diskCleanCanaryPreview(ctx, raw)
	case "disk_clean_canary_apply":
		return s.diskCleanCanaryApply(ctx, raw)
	case "disk_clean_continue_preview":
		return s.diskCleanContinuePreview(ctx, raw)
	case "disk_clean_continue_apply":
		return s.diskCleanContinueApply(ctx, raw)
	case "disk_clean_abandon_preview":
		return s.diskCleanAbandonPreview(ctx, raw)
	case "disk_clean_abandon_apply":
		return s.diskCleanAbandonApply(ctx, raw)
	default:
		return nil, &CallError{Code: "invalid_arguments", Message: "unknown tool " + name}
	}
}

func (s *Service) fleetOverview(ctx context.Context, raw json.RawMessage) (any, error) {
	if err := decodeEmpty(raw); err != nil {
		return nil, err
	}
	result, err := s.Hub.ListMachines(ctx, operator.MachineListRequest{})
	if err != nil {
		return nil, hubErr(err)
	}
	return map[string]any{
		"source":         "GET /v1/operator/machines",
		"page_truncated": result.NextCursor != nil,
		"machines":       result,
	}, nil
}

func (s *Service) machinesList(ctx context.Context, raw json.RawMessage) (any, error) {
	var args struct {
		MachineID   string   `json:"machine_id"`
		DisplayName string   `json:"display_name"`
		States      []string `json:"states"`
		Lifecycle   string   `json:"lifecycle"`
		Reporting   string   `json:"reporting"`
		Channel     string   `json:"channel"`
		Limit       int      `json:"limit"`
		Cursor      string   `json:"cursor"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	states := make([]state.State, len(args.States))
	for i, value := range args.States {
		states[i] = state.State(value)
	}
	result, err := s.Hub.ListMachines(ctx, operator.MachineListRequest{
		MachineID: args.MachineID, DisplayName: args.DisplayName, States: states,
		Lifecycle: operator.MachineLifecycleFilter(args.Lifecycle),
		Reporting: operator.MachineReportingFilter(args.Reporting),
		Channel:   operator.MachineChannelFilter(args.Channel),
		Limit:     args.Limit, Cursor: args.Cursor,
	})
	if err != nil {
		return nil, hubErr(err)
	}
	return result, nil
}

func (s *Service) machineGet(ctx context.Context, raw json.RawMessage) (any, error) {
	var args struct {
		MachineID string `json:"machine_id"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	if err := requireID("machine_id", args.MachineID); err != nil {
		return nil, err
	}
	result, err := s.Hub.Machine(ctx, args.MachineID)
	if err != nil {
		return nil, hubErr(err)
	}
	return result, nil
}

func (s *Service) machineEvidence(ctx context.Context, raw json.RawMessage) (any, error) {
	var args struct {
		MachineID string `json:"machine_id"`
		Limit     int    `json:"limit"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	if err := requireID("machine_id", args.MachineID); err != nil {
		return nil, err
	}
	result, err := s.Hub.MachineEvidence(ctx, args.MachineID, args.Limit)
	if err != nil {
		return nil, hubErr(err)
	}
	return result, nil
}

func (s *Service) jobsList(ctx context.Context, raw json.RawMessage) (any, error) {
	var args struct {
		MachineID    string   `json:"machine_id"`
		States       []string `json:"states"`
		DeploymentID string   `json:"deployment_id"`
		ResourceKind string   `json:"resource_kind"`
		ResourceID   string   `json:"resource_id"`
		Limit        int      `json:"limit"`
		Cursor       string   `json:"cursor"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	states := make([]deploy.JobState, len(args.States))
	for i, value := range args.States {
		states[i] = deploy.JobState(value)
	}
	result, err := s.Hub.Jobs(ctx, operator.JobListRequest{
		MachineID: args.MachineID, States: states, DeploymentID: args.DeploymentID,
		ResourceKind: args.ResourceKind, ResourceID: args.ResourceID,
		Limit: args.Limit, Cursor: args.Cursor,
	})
	if err != nil {
		return nil, hubErr(err)
	}
	return result, nil
}

func (s *Service) jobGet(ctx context.Context, raw json.RawMessage) (any, error) {
	var args struct {
		JobID string `json:"job_id"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	if err := requireID("job_id", args.JobID); err != nil {
		return nil, err
	}
	result, err := s.Hub.Job(ctx, args.JobID)
	if err != nil {
		return nil, hubErr(err)
	}
	return result, nil
}

func (s *Service) jobEvidence(ctx context.Context, raw json.RawMessage) (any, error) {
	var args struct {
		JobID string `json:"job_id"`
		Limit int    `json:"limit"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	if err := requireID("job_id", args.JobID); err != nil {
		return nil, err
	}
	result, err := s.Hub.JobEvidence(ctx, args.JobID, args.Limit)
	if err != nil {
		return nil, hubErr(err)
	}
	return result, nil
}

func (s *Service) deploymentsList(ctx context.Context, raw json.RawMessage) (any, error) {
	var args struct {
		Channel string   `json:"channel"`
		States  []string `json:"states"`
		Stuck   *bool    `json:"stuck"`
		Limit   int      `json:"limit"`
		Cursor  string   `json:"cursor"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	result, err := s.Hub.Deployments(ctx, operator.DeploymentListRequest{
		Channel: args.Channel, States: args.States, Stuck: args.Stuck, Limit: args.Limit, Cursor: args.Cursor,
	})
	if err != nil {
		return nil, hubErr(err)
	}
	return result, nil
}

func (s *Service) deploymentGet(ctx context.Context, raw json.RawMessage) (any, error) {
	id, err := deploymentIDArg(raw)
	if err != nil {
		return nil, err
	}
	result, err := s.Hub.Deployment(ctx, id)
	if err != nil {
		return nil, hubErr(err)
	}
	return result, nil
}

func (s *Service) softwareReport(ctx context.Context, raw json.RawMessage) (any, error) {
	if err := decodeEmpty(raw); err != nil {
		return nil, err
	}
	result, err := s.Hub.SoftwareReport(ctx)
	if err != nil {
		return nil, hubErr(err)
	}
	return result, nil
}

func (s *Service) compliance(ctx context.Context, raw json.RawMessage) (any, error) {
	if err := decodeEmpty(raw); err != nil {
		return nil, err
	}
	result, err := s.Hub.ComplianceBoard(ctx)
	if err != nil {
		return nil, hubErr(err)
	}
	return result, nil
}

func (s *Service) rolloutStatus(ctx context.Context, raw json.RawMessage) (any, error) {
	id, err := deploymentIDArg(raw)
	if err != nil {
		return nil, err
	}
	detail, err := s.Hub.Deployment(ctx, id)
	if err != nil {
		return nil, hubErr(err)
	}
	return rolloutView(detail), nil
}

func (s *Service) rolloutPreview(ctx context.Context, raw json.RawMessage) (any, error) {
	args, err := decodeCreatePreview(raw)
	if err != nil {
		return nil, err
	}
	if args.BatchSize == 0 {
		args.BatchSize = 1
	}
	if args.BatchSize != 1 {
		return nil, &CallError{Code: "canary_batch_size", Message: "rollout_preview accepts batch_size 1 or omitted"}
	}
	preview, err := s.Hub.PreviewDeploymentCreate(ctx, args)
	if err != nil {
		return nil, hubErr(err)
	}
	return map[string]any{
		"preview": preview,
		"canary":  rollout.AssessCanary(canaryFromPreview(preview)),
		"apply_with": []string{
			"preview_digest from this preview", "idempotency_key", "confirm_channel", "confirm_version", "reason", "batch_size 1",
		},
	}, nil
}

func (s *Service) rolloutApply(ctx context.Context, raw json.RawMessage) (any, error) {
	args, err := decodeCreateApply(raw)
	if err != nil {
		return nil, err
	}
	if args.BatchSize == 0 {
		args.BatchSize = 1
	}
	if args.BatchSize != 1 {
		return nil, &CallError{Code: "canary_batch_size", Message: "rollout_apply accepts batch_size 1 or omitted"}
	}
	if err := requireDigest(args.PreviewDigest); err != nil {
		return nil, err
	}
	if err := requireIdempotency(args.IdempotencyKey); err != nil {
		return nil, err
	}
	body := operatorclient.DeploymentCreateRequest{
		Channel: args.Channel, Version: args.Version, ArtifactSHA256: args.ArtifactSHA256,
		BatchSize: args.BatchSize, ExecutionTimeoutSeconds: args.ExecutionTimeoutSeconds, Irreversible: args.Irreversible,
		PreviewDigest: args.PreviewDigest, ConfirmChannel: args.ConfirmChannel, ConfirmVersion: args.ConfirmVersion, Reason: args.Reason,
	}
	result, err := s.Hub.CreateDeployment(ctx, args.IdempotencyKey, body)
	if err != nil {
		return nil, hubErr(err)
	}
	return map[string]any{"wrote": true, "result": result}, nil
}

func (s *Service) rolloutExpand(ctx context.Context, raw json.RawMessage) (any, error) {
	var args struct {
		DeploymentID            string `json:"deployment_id"`
		PreviewDigest           string `json:"preview_digest"`
		ExpectedControlRevision *int64 `json:"expected_control_revision"`
		ExpectedOpenedBatch     *int   `json:"expected_opened_batch"`
		ConfirmChannel          string `json:"confirm_channel"`
		Reason                  string `json:"reason"`
		IdempotencyKey          string `json:"idempotency_key"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	if err := requireID("deployment_id", args.DeploymentID); err != nil {
		return nil, err
	}
	detail, err := s.Hub.Deployment(ctx, args.DeploymentID)
	if err != nil {
		return nil, hubErr(err)
	}
	view := rolloutView(detail)
	assessment := view["canary"].(rollout.CanaryAssessment)
	if assessment.Stopped || !assessment.ExpandAllowed {
		return nil, &CallError{
			Code: "canary_blocked", Message: "rollout_expand did not send Continue. " + assessment.Reason,
			Detail: view,
		}
	}
	if detail.Item.State == "running" {
		next := "The deployment is running. This deployment was created before the canary hold, so the Hub driver still opens the next batch. Poll rollout_status. rollout_expand does not open it."
		if detail.Item.PauseAfterCanary {
			next = "The deployment is running. Poll rollout_status until the Hub pauses after the canary verdict. rollout_expand does not open the next batch."
		}
		return map[string]any{
			"wrote": false, "canary": assessment,
			"next": next,
		}, nil
	}
	if detail.Item.State != "paused" {
		return nil, &CallError{Code: "canary_blocked", Message: "rollout_expand did not send Continue for deployment state " + detail.Item.State, Detail: view}
	}
	if err := requireDigest(args.PreviewDigest); err != nil {
		return nil, err
	}
	if args.ExpectedControlRevision == nil || args.ExpectedOpenedBatch == nil {
		return nil, &CallError{Code: "expected_revision_required", Message: "expected_control_revision and expected_opened_batch are required"}
	}
	if *args.ExpectedControlRevision != detail.Item.ControlRevision || *args.ExpectedOpenedBatch != detail.Item.OpenedBatch {
		return nil, &CallError{Code: "expected_revision_mismatch", Message: "expected control revision or opened batch does not match the deployment just read"}
	}
	if err := requireIdempotency(args.IdempotencyKey); err != nil {
		return nil, err
	}
	if strings.TrimSpace(args.ConfirmChannel) == "" || strings.TrimSpace(args.Reason) == "" {
		return nil, &CallError{Code: "invalid_arguments", Message: "confirm_channel and reason are required"}
	}
	result, err := s.Hub.ContinueDeployment(ctx, args.DeploymentID, args.IdempotencyKey, operatorclient.DeploymentContinueRequest{
		PreviewDigest: args.PreviewDigest, ExpectedControlRevision: args.ExpectedControlRevision,
		ExpectedOpenedBatch: args.ExpectedOpenedBatch, ConfirmChannel: args.ConfirmChannel, Reason: args.Reason,
	})
	if err != nil {
		return nil, hubErr(err)
	}
	return map[string]any{"wrote": true, "result": result, "canary": assessment}, nil
}

func (s *Service) enrollPreview(ctx context.Context, raw json.RawMessage) (any, error) {
	var args struct {
		DisplayName string `json:"display_name"`
		TTLSeconds  int64  `json:"ttl_seconds"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	result, err := s.Hub.PreviewEnrollToken(ctx, operatorclient.EnrollmentTokenPreviewRequest{
		DisplayName: args.DisplayName, TTLSeconds: args.TTLSeconds,
	})
	if err != nil {
		return nil, hubErr(err)
	}
	return result, nil
}

func (s *Service) enrollCreate(ctx context.Context, raw json.RawMessage) (any, error) {
	var args struct {
		DisplayName    string `json:"display_name"`
		TTLSeconds     int64  `json:"ttl_seconds"`
		PreviewDigest  string `json:"preview_digest"`
		Reason         string `json:"reason"`
		IdempotencyKey string `json:"idempotency_key"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	if err := requireDigest(args.PreviewDigest); err != nil {
		return nil, err
	}
	if err := requireIdempotency(args.IdempotencyKey); err != nil {
		return nil, err
	}
	result, err := s.Hub.CreateEnrollToken(ctx, args.IdempotencyKey, operatorclient.EnrollmentTokenCreateRequest{
		DisplayName: args.DisplayName, TTLSeconds: args.TTLSeconds, PreviewDigest: args.PreviewDigest, Reason: args.Reason,
	})
	if err != nil {
		return nil, hubErr(err)
	}
	return result, nil
}

func (s *Service) deploymentCreatePreview(ctx context.Context, raw json.RawMessage) (any, error) {
	args, err := decodeCreatePreview(raw)
	if err != nil {
		return nil, err
	}
	result, err := s.Hub.PreviewDeploymentCreate(ctx, args)
	if err != nil {
		return nil, hubErr(err)
	}
	return result, nil
}

func (s *Service) deploymentCreate(ctx context.Context, raw json.RawMessage) (any, error) {
	args, err := decodeCreateApply(raw)
	if err != nil {
		return nil, err
	}
	if err := requireDigest(args.PreviewDigest); err != nil {
		return nil, err
	}
	if err := requireIdempotency(args.IdempotencyKey); err != nil {
		return nil, err
	}
	result, err := s.Hub.CreateDeployment(ctx, args.IdempotencyKey, operatorclient.DeploymentCreateRequest{
		Channel: args.Channel, Version: args.Version, ArtifactSHA256: args.ArtifactSHA256,
		BatchSize: args.BatchSize, ExecutionTimeoutSeconds: args.ExecutionTimeoutSeconds, Irreversible: args.Irreversible,
		PreviewDigest: args.PreviewDigest, ConfirmChannel: args.ConfirmChannel, ConfirmVersion: args.ConfirmVersion, Reason: args.Reason,
	})
	if err != nil {
		return nil, hubErr(err)
	}
	return result, nil
}

func (s *Service) deploymentContinuePreview(ctx context.Context, raw json.RawMessage) (any, error) {
	id, err := deploymentIDArg(raw)
	if err != nil {
		return nil, err
	}
	result, err := s.Hub.PreviewDeploymentContinue(ctx, id)
	if err != nil {
		return nil, hubErr(err)
	}
	return result, nil
}

func (s *Service) deploymentContinue(ctx context.Context, raw json.RawMessage) (any, error) {
	args, err := decodeRevisionWrite(raw)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(args.ConfirmChannel) == "" {
		return nil, &CallError{Code: "invalid_arguments", Message: "confirm_channel is required"}
	}
	detail, err := s.Hub.Deployment(ctx, args.DeploymentID)
	if err != nil {
		return nil, hubErr(err)
	}
	for _, blocker := range detail.Actions.Continue.Blockers {
		if blocker == "failed_batch_requires_explicit_skip" {
			return nil, &CallError{
				Code:    "failed_batch_skip_refused",
				Message: "plain Continue refuses a failed batch. Use the separately labelled skip failed batch action and record a reason. MCP tools do not send that skip.",
			}
		}
	}
	result, err := s.Hub.ContinueDeployment(ctx, args.DeploymentID, args.IdempotencyKey, operatorclient.DeploymentContinueRequest{
		PreviewDigest: args.PreviewDigest, ExpectedControlRevision: args.ExpectedControlRevision,
		ExpectedOpenedBatch: args.ExpectedOpenedBatch, ConfirmChannel: args.ConfirmChannel, Reason: args.Reason,
	})
	if err != nil {
		return nil, hubErr(err)
	}
	return result, nil
}

func (s *Service) deploymentAbandonPreview(ctx context.Context, raw json.RawMessage) (any, error) {
	id, err := deploymentIDArg(raw)
	if err != nil {
		return nil, err
	}
	result, err := s.Hub.PreviewDeploymentAbandon(ctx, id)
	if err != nil {
		return nil, hubErr(err)
	}
	return result, nil
}

func (s *Service) deploymentAbandon(ctx context.Context, raw json.RawMessage) (any, error) {
	var args struct {
		DeploymentID            string `json:"deployment_id"`
		PreviewDigest           string `json:"preview_digest"`
		ExpectedControlRevision *int64 `json:"expected_control_revision"`
		ExpectedOpenedBatch     *int   `json:"expected_opened_batch"`
		ConfirmDeploymentID     string `json:"confirm_deployment_id"`
		Reason                  string `json:"reason"`
		IdempotencyKey          string `json:"idempotency_key"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	if err := requireDigest(args.PreviewDigest); err != nil {
		return nil, err
	}
	if args.ExpectedControlRevision == nil || args.ExpectedOpenedBatch == nil {
		return nil, &CallError{Code: "expected_revision_required", Message: "expected_control_revision and expected_opened_batch are required"}
	}
	if err := requireIdempotency(args.IdempotencyKey); err != nil {
		return nil, err
	}
	if err := requireID("deployment_id", args.DeploymentID); err != nil {
		return nil, err
	}
	if args.ConfirmDeploymentID == "" || args.Reason == "" {
		return nil, &CallError{Code: "invalid_arguments", Message: "confirm_deployment_id and reason are required"}
	}
	result, err := s.Hub.AbandonDeployment(ctx, args.DeploymentID, args.IdempotencyKey, operatorclient.DeploymentAbandonRequest{
		PreviewDigest: args.PreviewDigest, ExpectedControlRevision: args.ExpectedControlRevision,
		ExpectedOpenedBatch: args.ExpectedOpenedBatch, ConfirmDeploymentID: args.ConfirmDeploymentID, Reason: args.Reason,
	})
	if err != nil {
		return nil, hubErr(err)
	}
	return result, nil
}

func (s *Service) profilePreview(ctx context.Context, raw json.RawMessage) (any, error) {
	var args struct {
		MachineID       string `json:"machine_id"`
		ProfileID       string `json:"profile_id"`
		ProfileRevision int64  `json:"profile_revision"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	result, err := s.Hub.PreviewMachineProfileAssignment(ctx, operator.MachineProfileAssignmentPreviewRequest{
		MachineID: args.MachineID, ProfileID: args.ProfileID, ProfileRevision: args.ProfileRevision,
	})
	if err != nil {
		return nil, hubErr(err)
	}
	return result, nil
}

func (s *Service) profileApply(ctx context.Context, raw json.RawMessage) (any, error) {
	var args struct {
		MachineID          string `json:"machine_id"`
		ProfileID          string `json:"profile_id"`
		ProfileRevision    int64  `json:"profile_revision"`
		ConfirmDisplayName string `json:"confirm_display_name"`
		PreviewDigest      string `json:"preview_digest"`
		Reason             string `json:"reason"`
		IdempotencyKey     string `json:"idempotency_key"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	if err := requireDigest(args.PreviewDigest); err != nil {
		return nil, err
	}
	if err := requireIdempotency(args.IdempotencyKey); err != nil {
		return nil, err
	}
	result, err := s.Hub.AssignMachineProfile(ctx, args.IdempotencyKey, operator.MachineProfileAssignmentRequest{
		MachineID: args.MachineID, ProfileID: args.ProfileID, ProfileRevision: args.ProfileRevision,
		ConfirmDisplayName: args.ConfirmDisplayName, PreviewDigest: args.PreviewDigest, Reason: args.Reason,
	})
	if err != nil {
		return nil, hubErr(err)
	}
	return result, nil
}

type createPreviewArgs struct {
	Channel                 string `json:"channel"`
	Version                 string `json:"version"`
	ArtifactSHA256          string `json:"artifact_sha256"`
	BatchSize               int    `json:"batch_size"`
	ExecutionTimeoutSeconds int    `json:"execution_timeout_seconds"`
	Irreversible            bool   `json:"irreversible"`
}

type createApplyArgs struct {
	createPreviewArgs
	PreviewDigest  string `json:"preview_digest"`
	ConfirmChannel string `json:"confirm_channel"`
	ConfirmVersion string `json:"confirm_version"`
	Reason         string `json:"reason"`
	IdempotencyKey string `json:"idempotency_key"`
}

func decodeCreatePreview(raw json.RawMessage) (operator.DeploymentCreatePreviewRequest, error) {
	var args createPreviewArgs
	if err := decodeArgs(raw, &args); err != nil {
		return operator.DeploymentCreatePreviewRequest{}, err
	}
	return operator.DeploymentCreatePreviewRequest{
		Channel: args.Channel, Version: args.Version, ArtifactSHA256: args.ArtifactSHA256,
		BatchSize: args.BatchSize, ExecutionTimeoutSeconds: args.ExecutionTimeoutSeconds, Irreversible: args.Irreversible,
	}, nil
}

func decodeCreateApply(raw json.RawMessage) (createApplyArgs, error) {
	var args createApplyArgs
	if err := decodeArgs(raw, &args); err != nil {
		return createApplyArgs{}, err
	}
	return args, nil
}

type revisionWriteArgs struct {
	DeploymentID            string `json:"deployment_id"`
	PreviewDigest           string `json:"preview_digest"`
	ExpectedControlRevision *int64 `json:"expected_control_revision"`
	ExpectedOpenedBatch     *int   `json:"expected_opened_batch"`
	ConfirmChannel          string `json:"confirm_channel"`
	Reason                  string `json:"reason"`
	IdempotencyKey          string `json:"idempotency_key"`
}

func decodeRevisionWrite(raw json.RawMessage) (revisionWriteArgs, error) {
	var args revisionWriteArgs
	if err := decodeArgs(raw, &args); err != nil {
		return revisionWriteArgs{}, err
	}
	if err := requireDigest(args.PreviewDigest); err != nil {
		return revisionWriteArgs{}, err
	}
	if args.ExpectedControlRevision == nil || args.ExpectedOpenedBatch == nil {
		return revisionWriteArgs{}, &CallError{Code: "expected_revision_required", Message: "expected_control_revision and expected_opened_batch are required"}
	}
	if err := requireIdempotency(args.IdempotencyKey); err != nil {
		return revisionWriteArgs{}, err
	}
	if err := requireID("deployment_id", args.DeploymentID); err != nil {
		return revisionWriteArgs{}, err
	}
	if strings.TrimSpace(args.Reason) == "" {
		return revisionWriteArgs{}, &CallError{Code: "invalid_arguments", Message: "reason is required"}
	}
	return args, nil
}

func rolloutView(detail operator.DeploymentDetailResult) map[string]any {
	return map[string]any{
		"deployment_id":    detail.Item.DeploymentID,
		"deployment_state": detail.Item.State,
		"channel":          detail.Item.Channel,
		"control_revision": detail.Item.ControlRevision,
		"opened_batch":     detail.Item.OpenedBatch,
		"total_batches":    detail.Item.TotalBatches,
		"canary":           rollout.AssessCanary(canaryFromDetail(detail)),
	}
}

func canaryFromDetail(detail operator.DeploymentDetailResult) rollout.CanaryInput {
	in := rollout.CanaryInput{
		State: detail.Item.State, OpenedBatch: detail.Item.OpenedBatch,
		TotalBatches: detail.Item.TotalBatches, BatchSize: detail.Item.BatchSize,
		PauseAfterCanary: detail.Item.PauseAfterCanary,
	}
	for _, target := range detail.Targets {
		machine := rollout.CanaryMachine{
			MachineID: target.MachineID, DisplayName: target.DisplayName, BatchNo: target.BatchNo,
			Excluded: target.ExcludedReason != nil, Opened: target.JobID != nil && *target.JobID != "",
		}
		if target.JobState != nil {
			machine.JobState = *target.JobState
		}
		if target.Independent != nil {
			machine.IndependentVerdict = target.Independent.Verdict
		}
		in.Targets = append(in.Targets, machine)
	}
	return in
}

func canaryFromPreview(preview operator.DeploymentCreatePreviewResult) rollout.CanaryInput {
	in := rollout.CanaryInput{
		State: "preview", BatchSize: preview.BatchSize, TotalBatches: preview.TotalBatches,
		PauseAfterCanary: true,
	}
	for _, target := range preview.Targets {
		machine := rollout.CanaryMachine{
			MachineID: target.MachineID, DisplayName: target.DisplayName, BatchNo: target.BatchNo,
			Excluded: target.ExcludedReason != nil,
		}
		in.Targets = append(in.Targets, machine)
	}
	return in
}

func deploymentIDArg(raw json.RawMessage) (string, error) {
	var args struct {
		DeploymentID string `json:"deployment_id"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return "", err
	}
	if err := requireID("deployment_id", args.DeploymentID); err != nil {
		return "", err
	}
	return args.DeploymentID, nil
}

func decodeEmpty(raw json.RawMessage) error {
	var args struct{}
	return decodeArgs(raw, &args)
}

var unknownFieldPattern = regexp.MustCompile(`unknown field "([^"]+)"`)

func decodeArgs(raw json.RawMessage, dst any) error {
	if len(bytes.TrimSpace(raw)) == 0 {
		raw = []byte("{}")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		message := "arguments are not valid JSON for this tool"
		if match := unknownFieldPattern.FindStringSubmatch(err.Error()); match != nil {
			message = fmt.Sprintf("unknown field %q; allowed fields: %s", match[1], allowedJSONFields(dst))
		}
		return &CallError{Code: "invalid_arguments", Message: message}
	}
	if dec.More() {
		return &CallError{Code: "invalid_arguments", Message: "arguments contain a trailing JSON value"}
	}
	return nil
}

func allowedJSONFields(dst any) string {
	valueType := reflect.TypeOf(dst)
	for valueType != nil && valueType.Kind() == reflect.Pointer {
		valueType = valueType.Elem()
	}
	if valueType == nil || valueType.Kind() != reflect.Struct {
		return "(none)"
	}
	names := jsonFieldNames(valueType)
	if len(names) == 0 {
		return "(none)"
	}
	return strings.Join(names, ", ")
}

func jsonFieldNames(valueType reflect.Type) []string {
	var names []string
	for i := 0; i < valueType.NumField(); i++ {
		field := valueType.Field(i)
		if field.Anonymous {
			embedded := field.Type
			for embedded.Kind() == reflect.Pointer {
				embedded = embedded.Elem()
			}
			if embedded.Kind() == reflect.Struct {
				names = append(names, jsonFieldNames(embedded)...)
				continue
			}
		}
		tag := field.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name := tag
		if comma := strings.IndexByte(name, ','); comma >= 0 {
			name = name[:comma]
		}
		if name == "" {
			if field.PkgPath != "" {
				continue
			}
			name = field.Name
		}
		names = append(names, name)
	}
	return names
}

func requireID(name, value string) error {
	if value == "" || strings.TrimSpace(value) != value || strings.Contains(value, "/") || value == "." || value == ".." {
		return &CallError{Code: "invalid_arguments", Message: name + " is empty or not a single path segment"}
	}
	return nil
}

func requireDigest(value string) error {
	const prefix = "sha256:"
	if len(value) != len(prefix)+64 || !strings.HasPrefix(value, prefix) {
		return &CallError{Code: "preview_digest_required", Message: "preview_digest must be sha256: and 64 lowercase hex from a prior preview call"}
	}
	for _, r := range value[len(prefix):] {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return &CallError{Code: "preview_digest_required", Message: "preview_digest must be sha256: and 64 lowercase hex from a prior preview call"}
		}
	}
	return nil
}

func requireIdempotency(value string) error {
	if value == "" || strings.TrimSpace(value) != value || len(value) > 200 || !utf8.ValidString(value) {
		return &CallError{Code: "invalid_arguments", Message: "idempotency_key must be 1 to 200 bytes with no surrounding space"}
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return &CallError{Code: "invalid_arguments", Message: "idempotency_key must not contain control characters"}
		}
	}
	return nil
}

func hubErr(err error) error {
	if err == nil {
		return nil
	}
	var call *CallError
	if errors.As(err, &call) {
		return err
	}
	var api *operatorclient.APIError
	if errors.As(err, &api) && api.Code != "" {
		return &CallError{Code: api.Code, Message: api.Message}
	}
	return &CallError{Code: "hub_error", Message: err.Error()}
}

// Supervision's closed schemas reject null objects/fields and explicit empty
// optional strings. Return a fixed error without echoing caller-provided keys.
func decodeSupervisionArgs(raw json.RawMessage, dst any) error {
	if len(bytes.TrimSpace(raw)) == 0 {
		raw = json.RawMessage(`{}`)
	}
	invalid := &CallError{Code: "invalid_arguments", Message: "invalid supervision arguments"}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return invalid
	}
	for _, v := range fields {
		if bytes.Equal(bytes.TrimSpace(v), []byte("null")) || bytes.Equal(bytes.TrimSpace(v), []byte(`""`)) {
			return invalid
		}
	}
	if err := decodeArgs(raw, dst); err != nil {
		return invalid
	}
	return nil
}
