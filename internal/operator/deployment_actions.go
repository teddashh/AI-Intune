package operator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/teddashh/AI-Intune/internal/artifact"
	"github.com/teddashh/AI-Intune/internal/deploy"
	"github.com/teddashh/AI-Intune/internal/operatorauth"
	"github.com/teddashh/AI-Intune/internal/rollout"
	"github.com/teddashh/AI-Intune/internal/store"
)

const (
	DeploymentActionSchemaVersion = 3
	DeploymentActionPolicyVersion = "deployment-control-v1"
)

var ErrInvalidDeploymentAction = errors.New("operator: invalid deployment action request")

type DeploymentTransportRejectionAction string

const (
	DeploymentTransportRejectionCreate          DeploymentTransportRejectionAction = "create"
	DeploymentTransportRejectionContinue        DeploymentTransportRejectionAction = "continue"
	DeploymentTransportRejectionSkipFailedBatch DeploymentTransportRejectionAction = "skip_failed_batch"
	DeploymentTransportRejectionRetry           DeploymentTransportRejectionAction = "retry"
	DeploymentTransportRejectionAbandon         DeploymentTransportRejectionAction = "abandon"
)

type DeploymentTransportRejectionRequest struct {
	Action       DeploymentTransportRejectionAction
	DeploymentID string
	Actor        Actor
}

type DeploymentCreateApplyRequest struct {
	DeploymentCreatePreviewRequest
	PreviewDigest  string `json:"preview_digest"`
	ConfirmChannel string `json:"confirm_channel"`
	ConfirmVersion string `json:"confirm_version"`
	Reason         string `json:"reason"`
	IdempotencyKey string `json:"-"`
	Actor          Actor  `json:"-"`
}

type DeploymentContinuePreviewRequest struct {
	DeploymentID string `json:"deployment_id"`
}

type DeploymentRetryPreviewRequest struct {
	DeploymentID string `json:"deployment_id"`
}

type DeploymentAbandonPreviewRequest struct {
	DeploymentID string `json:"deployment_id"`
}

type DeploymentContinueApplyRequest struct {
	DeploymentID            string `json:"deployment_id"`
	PreviewDigest           string `json:"preview_digest"`
	ExpectedControlRevision *int64 `json:"expected_control_revision"`
	ExpectedOpenedBatch     *int   `json:"expected_opened_batch"`
	ConfirmChannel          string `json:"confirm_channel"`
	Reason                  string `json:"reason"`
	IdempotencyKey          string `json:"-"`
	Actor                   Actor  `json:"-"`
}

type DeploymentRetryApplyRequest struct {
	DeploymentID            string `json:"deployment_id"`
	PreviewDigest           string `json:"preview_digest"`
	ExpectedControlRevision *int64 `json:"expected_control_revision"`
	ExpectedOpenedBatch     *int   `json:"expected_opened_batch"`
	ConfirmChannel          string `json:"confirm_channel"`
	ConfirmVersion          string `json:"confirm_version"`
	Reason                  string `json:"reason"`
	IdempotencyKey          string `json:"-"`
	Actor                   Actor  `json:"-"`
}

type DeploymentAbandonApplyRequest struct {
	DeploymentID            string `json:"deployment_id"`
	PreviewDigest           string `json:"preview_digest"`
	ExpectedControlRevision *int64 `json:"expected_control_revision"`
	ExpectedOpenedBatch     *int   `json:"expected_opened_batch"`
	ConfirmDeploymentID     string `json:"confirm_deployment_id"`
	Reason                  string `json:"reason"`
	IdempotencyKey          string `json:"-"`
	Actor                   Actor  `json:"-"`
}

type DeploymentActionTargetPreview struct {
	MachineID      string           `json:"machine_id"`
	DisplayName    string           `json:"display_name"`
	BatchNo        int              `json:"batch_no"`
	JobID          *string          `json:"job_id"`
	JobState       *deploy.JobState `json:"job_state"`
	ExcludedReason *string          `json:"excluded_reason"`
}

type DeploymentTerminalFailurePreview struct {
	MachineID   string          `json:"machine_id"`
	DisplayName string          `json:"display_name"`
	JobID       string          `json:"job_id"`
	JobState    deploy.JobState `json:"job_state"`
}

type DeploymentActionPreviewResult struct {
	SchemaVersion          int                                `json:"schema_version"`
	PolicyVersion          string                             `json:"policy_version"`
	Action                 string                             `json:"action"`
	PreviewedAt            time.Time                          `json:"previewed_at"`
	Deployment             DeploymentSummary                  `json:"deployment"`
	Eligibility            DeploymentActionEligibility        `json:"eligibility"`
	Targets                []DeploymentActionTargetPreview    `json:"targets"`
	TerminalFailureTargets []DeploymentTerminalFailurePreview `json:"terminal_failure_targets"`
	Artifact               *DeploymentArtifactPreview         `json:"artifact"`
	Promotion              *DeploymentPromotionPreview        `json:"promotion"`
	PreviewDigest          string                             `json:"preview_digest"`
}

type DeploymentMutationJob struct {
	JobID           string          `json:"job_id"`
	MachineID       string          `json:"machine_id"`
	DesiredRevision deploy.Revision `json:"desired_revision"`
	State           deploy.JobState `json:"state"`
	CreatedAt       time.Time       `json:"created_at"`
}

type DeploymentMutationResult struct {
	SchemaVersion   int                     `json:"schema_version"`
	Action          string                  `json:"action"`
	DeploymentID    string                  `json:"deployment_id"`
	Channel         string                  `json:"channel"`
	ResourceKind    string                  `json:"resource_kind"`
	ResourceID      string                  `json:"resource_id"`
	DesiredRevision deploy.Revision         `json:"desired_revision"`
	ControlRevision int64                   `json:"control_revision"`
	BatchSize       int                     `json:"batch_size"`
	State           string                  `json:"state"`
	CreatedAt       time.Time               `json:"created_at"`
	PausedAt        *time.Time              `json:"paused_at"`
	FinishedAt      *time.Time              `json:"finished_at"`
	RetryOf         *string                 `json:"retry_of"`
	OpenedBatch     int                     `json:"opened_batch"`
	Jobs            []DeploymentMutationJob `json:"jobs"`
	PreviewDigest   string                  `json:"preview_digest"`
	Replayed        bool                    `json:"replayed"`
}

func (s *Service) PreviewDeploymentContinue(request DeploymentContinuePreviewRequest, evaluatedAt time.Time) (DeploymentActionPreviewResult, error) {
	return s.PreviewDeploymentContinueContext(context.Background(), request, evaluatedAt)
}

// RecordDeploymentTransportRejection owns the audit shape for a Web form that
// could not become a canonical deployment request. It does not create an
// idempotency receipt or persist unparsed form fields.
func (s *Service) RecordDeploymentTransportRejection(request DeploymentTransportRejectionRequest) error {
	var action store.AuditAction
	subject := request.DeploymentID
	switch request.Action {
	case DeploymentTransportRejectionCreate:
		action, subject = store.AuditDeploymentCreate, "deployment create"
	case DeploymentTransportRejectionContinue:
		action = store.AuditDeploymentContinue
	case DeploymentTransportRejectionSkipFailedBatch:
		action = store.AuditDeploymentSkipFailedBatch
	case DeploymentTransportRejectionRetry:
		action = store.AuditDeploymentRetry
	case DeploymentTransportRejectionAbandon:
		action = store.AuditDeploymentAbandon
	default:
		return fmt.Errorf("operator: invalid deployment transport rejection action %q", request.Action)
	}
	if request.Action != DeploymentTransportRejectionCreate && !validDeploymentIdentifier(subject, 256) {
		subject = "deployment action"
	}
	entry := auditFromActor(request.Actor)
	entry.Action = action
	entry.Subject = subject
	entry.Detail = store.OperatorTransportRejectionPrefix + "BAD_REQUEST: canonical request digest 無法取得"
	return s.store.RecordAudit(entry)
}

func (s *Service) PreviewDeploymentContinueContext(ctx context.Context, request DeploymentContinuePreviewRequest,
	evaluatedAt time.Time,
) (DeploymentActionPreviewResult, error) {
	return s.previewDeploymentAction(ctx, "continue", request.DeploymentID, evaluatedAt)
}

func (s *Service) PreviewDeploymentSkipFailedBatchContext(ctx context.Context, request DeploymentContinuePreviewRequest,
	evaluatedAt time.Time,
) (DeploymentActionPreviewResult, error) {
	return s.previewDeploymentAction(ctx, "skip_failed_batch", request.DeploymentID, evaluatedAt)
}

func (s *Service) PreviewDeploymentRetry(request DeploymentRetryPreviewRequest, evaluatedAt time.Time) (DeploymentActionPreviewResult, error) {
	return s.PreviewDeploymentRetryContext(context.Background(), request, evaluatedAt)
}

func (s *Service) PreviewDeploymentRetryContext(ctx context.Context, request DeploymentRetryPreviewRequest,
	evaluatedAt time.Time,
) (DeploymentActionPreviewResult, error) {
	return s.previewDeploymentAction(ctx, "retry", request.DeploymentID, evaluatedAt)
}

func (s *Service) PreviewDeploymentAbandon(request DeploymentAbandonPreviewRequest, evaluatedAt time.Time) (DeploymentActionPreviewResult, error) {
	return s.PreviewDeploymentAbandonContext(context.Background(), request, evaluatedAt)
}

func (s *Service) PreviewDeploymentAbandonContext(ctx context.Context, request DeploymentAbandonPreviewRequest,
	evaluatedAt time.Time,
) (DeploymentActionPreviewResult, error) {
	return s.previewDeploymentAction(ctx, "abandon", request.DeploymentID, evaluatedAt)
}

func (s *Service) ApplyDeploymentCreate(request DeploymentCreateApplyRequest) (DeploymentMutationResult, error) {
	return s.ApplyDeploymentCreateContext(context.Background(), request)
}

func (s *Service) ApplyDeploymentCreateContext(ctx context.Context, request DeploymentCreateApplyRequest) (DeploymentMutationResult, error) {
	if ctx == nil {
		return DeploymentMutationResult{}, fmt.Errorf("%w: context is required", ErrInvalidDeploymentAction)
	}
	if err := ctx.Err(); err != nil {
		return DeploymentMutationResult{}, err
	}
	if s == nil || s.store == nil {
		return DeploymentMutationResult{}, fmt.Errorf("%w: store is required", ErrInvalidDeploymentAction)
	}
	digest := DeploymentCreateApplySemanticDigest(request)
	audit := deploymentActionAudit(request.Actor, request.Reason, request.IdempotencyKey, digest)
	result, err := s.store.ApplyOperatorDeploymentCreate(store.OperatorDeploymentRequest{
		PreviewDigest: request.PreviewDigest, IdempotencyKey: request.IdempotencyKey,
		ConfirmChannel: request.ConfirmChannel, ConfirmVersion: request.ConfirmVersion,
		RequestDigest: digest, Audit: audit,
	}, func() (store.OperatorDeploymentPrepared, error) {
		if err := validateDeploymentActionReason(request.Reason); err != nil {
			return store.OperatorDeploymentPrepared{}, err
		}
		preview, err := s.PreviewDeploymentCreateContext(ctx, request.DeploymentCreatePreviewRequest, time.Now().UTC())
		if err != nil {
			return store.OperatorDeploymentPrepared{}, deploymentCreatePrepareError(request.Channel, err)
		}
		prepared := store.OperatorDeploymentPrepared{CurrentPreviewDigest: preview.PreviewDigest}
		if preview.PreviewDigest != request.PreviewDigest {
			return prepared, nil
		}
		if request.ConfirmChannel != preview.Channel || request.ConfirmVersion != preview.Artifact.Version {
			return store.OperatorDeploymentPrepared{}, deploymentRejection(store.OperatorCodeDeploymentConfirmationMismatch)
		}
		if !preview.CreateAllowed {
			return store.OperatorDeploymentPrepared{}, deploymentEligibilityRejection("create", preview.Blockers)
		}
		createdBy, err := deploymentCreatedBy(request.Actor)
		if err != nil {
			return store.OperatorDeploymentPrepared{}, err
		}
		material, err := artifact.ResolveOpenClawMaterialContext(ctx, s.artifactsDir, preview.Artifact.Version, preview.Artifact.SHA256)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return store.OperatorDeploymentPrepared{}, ctxErr
			}
			return store.OperatorDeploymentPrepared{}, deploymentRejection(store.OperatorCodeDeploymentPreviewStale)
		}
		if err := validateDeploymentArtifactPreviewMaterial(material); err != nil ||
			!sameDeploymentArtifactIdentity(preview.Artifact, material) {
			return store.OperatorDeploymentPrepared{}, deploymentRejection(store.OperatorCodeDeploymentPreviewStale)
		}
		prepared.Deployment = store.NewDeployment{
			Channel: preview.Channel, ResourceKind: "openclaw", ResourceID: "openclaw",
			Spec: material.Spec, BatchSize: preview.BatchSize, PauseAfterCanary: true, CreatedBy: createdBy,
			Targets: deploymentTargetsFromPlan(preview.Targets),
			Job: store.NewJob{
				ArtifactDigest: material.Digest, Irreversible: preview.Irreversible,
				ExecutionTimeout: preview.ExecutionTimeoutSeconds,
			},
		}
		return prepared, nil
	})
	if err != nil {
		s.recordDeploymentActionFallback(store.AuditDeploymentCreate, "deployment create", request.Reason,
			request.IdempotencyKey, digest, request.Actor, result.Audited, err)
		return DeploymentMutationResult{}, err
	}
	projected, projectErr := projectDeploymentMutation("create", result)
	if projectErr != nil {
		return DeploymentMutationResult{}, projectErr
	}
	return projected, nil
}

func (s *Service) ApplyDeploymentContinue(request DeploymentContinueApplyRequest) (DeploymentMutationResult, error) {
	return s.ApplyDeploymentContinueContext(context.Background(), request)
}

func (s *Service) ApplyDeploymentContinueContext(ctx context.Context, request DeploymentContinueApplyRequest) (DeploymentMutationResult, error) {
	if ctx == nil {
		return DeploymentMutationResult{}, fmt.Errorf("%w: context is required", ErrInvalidDeploymentAction)
	}
	if err := ctx.Err(); err != nil {
		return DeploymentMutationResult{}, err
	}
	if s == nil || s.store == nil {
		return DeploymentMutationResult{}, fmt.Errorf("%w: store is required", ErrInvalidDeploymentAction)
	}
	digest := DeploymentContinueSemanticDigest(request)
	audit := deploymentActionAudit(request.Actor, request.Reason, request.IdempotencyKey, digest)
	result, err := s.store.ApplyOperatorDeploymentContinue(store.OperatorDeploymentRequest{
		DeploymentID: request.DeploymentID, PreviewDigest: request.PreviewDigest,
		ExpectedControlRevision: request.ExpectedControlRevision, ExpectedOpenedBatch: request.ExpectedOpenedBatch,
		ConfirmChannel: request.ConfirmChannel,
		IdempotencyKey: request.IdempotencyKey, RequestDigest: digest, Audit: audit,
	}, func() (store.OperatorDeploymentPrepared, error) {
		if err := validateDeploymentActionReason(request.Reason); err != nil {
			return store.OperatorDeploymentPrepared{}, err
		}
		snapshot, err := s.deploymentActionSnapshot(ctx, "continue", request.DeploymentID, time.Now().UTC())
		if err != nil {
			return store.OperatorDeploymentPrepared{}, deploymentControlPrepareError(err)
		}
		prepared := store.OperatorDeploymentPrepared{CurrentPreviewDigest: snapshot.Result.PreviewDigest}
		if snapshot.Result.PreviewDigest != request.PreviewDigest {
			return prepared, nil
		}
		if request.ConfirmChannel != snapshot.Result.Deployment.Channel {
			return store.OperatorDeploymentPrepared{}, deploymentRejection(store.OperatorCodeDeploymentConfirmationMismatch)
		}
		if !snapshot.Result.Eligibility.Eligible {
			return store.OperatorDeploymentPrepared{}, deploymentEligibilityRejection("continue", snapshot.Result.Eligibility.Blockers)
		}
		return prepared, nil
	})
	if err != nil {
		s.recordDeploymentActionFallback(store.AuditDeploymentContinue, request.DeploymentID, request.Reason,
			request.IdempotencyKey, digest, request.Actor, result.Audited, err)
		return DeploymentMutationResult{}, err
	}
	projected, projectErr := projectDeploymentMutation("continue", result)
	if projectErr != nil {
		return DeploymentMutationResult{}, projectErr
	}
	return projected, nil
}

func (s *Service) ApplyDeploymentSkipFailedBatch(request DeploymentContinueApplyRequest) (DeploymentMutationResult, error) {
	return s.ApplyDeploymentSkipFailedBatchContext(context.Background(), request)
}

func (s *Service) ApplyDeploymentSkipFailedBatchContext(ctx context.Context, request DeploymentContinueApplyRequest) (DeploymentMutationResult, error) {
	if ctx == nil {
		return DeploymentMutationResult{}, fmt.Errorf("%w: context is required", ErrInvalidDeploymentAction)
	}
	if err := ctx.Err(); err != nil {
		return DeploymentMutationResult{}, err
	}
	if s == nil || s.store == nil {
		return DeploymentMutationResult{}, fmt.Errorf("%w: store is required", ErrInvalidDeploymentAction)
	}
	digest := DeploymentSkipFailedBatchSemanticDigest(request)
	audit := deploymentActionAudit(request.Actor, request.Reason, request.IdempotencyKey, digest)
	result, err := s.store.ApplyOperatorDeploymentSkipFailedBatch(store.OperatorDeploymentRequest{
		DeploymentID: request.DeploymentID, PreviewDigest: request.PreviewDigest,
		ExpectedControlRevision: request.ExpectedControlRevision, ExpectedOpenedBatch: request.ExpectedOpenedBatch,
		ConfirmChannel: request.ConfirmChannel,
		IdempotencyKey: request.IdempotencyKey, RequestDigest: digest, Audit: audit,
	}, func() (store.OperatorDeploymentPrepared, error) {
		if err := validateDeploymentActionReason(request.Reason); err != nil {
			return store.OperatorDeploymentPrepared{}, err
		}
		if strings.TrimSpace(request.Reason) == "" {
			return store.OperatorDeploymentPrepared{}, fmt.Errorf("%w: skip failed batch requires a reason", ErrInvalidDeploymentAction)
		}
		snapshot, err := s.deploymentActionSnapshot(ctx, "skip_failed_batch", request.DeploymentID, time.Now().UTC())
		if err != nil {
			return store.OperatorDeploymentPrepared{}, deploymentControlPrepareError(err)
		}
		prepared := store.OperatorDeploymentPrepared{CurrentPreviewDigest: snapshot.Result.PreviewDigest}
		if snapshot.Result.PreviewDigest != request.PreviewDigest {
			return prepared, nil
		}
		if request.ConfirmChannel != snapshot.Result.Deployment.Channel {
			return store.OperatorDeploymentPrepared{}, deploymentRejection(store.OperatorCodeDeploymentConfirmationMismatch)
		}
		if !snapshot.Result.Eligibility.Eligible {
			return store.OperatorDeploymentPrepared{}, deploymentEligibilityRejection("skip_failed_batch", snapshot.Result.Eligibility.Blockers)
		}
		return prepared, nil
	})
	if err != nil {
		s.recordDeploymentActionFallback(store.AuditDeploymentSkipFailedBatch, request.DeploymentID, request.Reason,
			request.IdempotencyKey, digest, request.Actor, result.Audited, err)
		return DeploymentMutationResult{}, err
	}
	projected, projectErr := projectDeploymentMutation("skip_failed_batch", result)
	if projectErr != nil {
		return DeploymentMutationResult{}, projectErr
	}
	return projected, nil
}

func (s *Service) ApplyDeploymentRetry(request DeploymentRetryApplyRequest) (DeploymentMutationResult, error) {
	return s.ApplyDeploymentRetryContext(context.Background(), request)
}

func (s *Service) ApplyDeploymentRetryContext(ctx context.Context, request DeploymentRetryApplyRequest) (DeploymentMutationResult, error) {
	if ctx == nil {
		return DeploymentMutationResult{}, fmt.Errorf("%w: context is required", ErrInvalidDeploymentAction)
	}
	if err := ctx.Err(); err != nil {
		return DeploymentMutationResult{}, err
	}
	if s == nil || s.store == nil {
		return DeploymentMutationResult{}, fmt.Errorf("%w: store is required", ErrInvalidDeploymentAction)
	}
	digest := DeploymentRetrySemanticDigest(request)
	audit := deploymentActionAudit(request.Actor, request.Reason, request.IdempotencyKey, digest)
	result, err := s.store.ApplyOperatorDeploymentRetry(store.OperatorDeploymentRequest{
		DeploymentID: request.DeploymentID, PreviewDigest: request.PreviewDigest,
		ExpectedControlRevision: request.ExpectedControlRevision, ExpectedOpenedBatch: request.ExpectedOpenedBatch,
		ConfirmChannel: request.ConfirmChannel, ConfirmVersion: request.ConfirmVersion,
		IdempotencyKey: request.IdempotencyKey, RequestDigest: digest, Audit: audit,
	}, func() (store.OperatorDeploymentPrepared, error) {
		if err := validateDeploymentActionReason(request.Reason); err != nil {
			return store.OperatorDeploymentPrepared{}, err
		}
		snapshot, err := s.deploymentActionSnapshot(ctx, "retry", request.DeploymentID, time.Now().UTC())
		if err != nil {
			return store.OperatorDeploymentPrepared{}, deploymentControlPrepareError(err)
		}
		prepared := store.OperatorDeploymentPrepared{CurrentPreviewDigest: snapshot.Result.PreviewDigest}
		if snapshot.Result.PreviewDigest != request.PreviewDigest {
			return prepared, nil
		}
		materialVersion := ""
		if snapshot.Result.Deployment.Material.Version != nil {
			materialVersion = *snapshot.Result.Deployment.Material.Version
		}
		if request.ConfirmChannel != snapshot.Result.Deployment.Channel || request.ConfirmVersion != materialVersion {
			return store.OperatorDeploymentPrepared{}, deploymentRejection(store.OperatorCodeDeploymentConfirmationMismatch)
		}
		if !snapshot.Result.Eligibility.Eligible {
			return store.OperatorDeploymentPrepared{}, deploymentEligibilityRejection("retry", snapshot.Result.Eligibility.Blockers)
		}
		if !snapshot.HasMaterial || snapshot.Material.Spec != snapshot.View.Spec {
			return store.OperatorDeploymentPrepared{}, deploymentRejection(store.OperatorCodeDeploymentPreviewStale)
		}
		createdBy, err := deploymentCreatedBy(request.Actor)
		if err != nil {
			return store.OperatorDeploymentPrepared{}, err
		}
		template, err := s.store.DeploymentJobTemplate(request.DeploymentID)
		if err != nil {
			return store.OperatorDeploymentPrepared{}, err
		}
		if template.ArtifactDigest != snapshot.Material.Digest {
			return store.OperatorDeploymentPrepared{}, deploymentRejection(store.OperatorCodeDeploymentPreviewStale)
		}
		prepared.TerminalFailureMachineIDs = append([]string(nil), snapshot.TerminalFailureMachineIDs...)
		prepared.Deployment = store.NewDeployment{
			Channel: snapshot.View.Channel, ResourceKind: snapshot.View.ResourceKind,
			ResourceID: snapshot.View.ResourceID, Spec: snapshot.View.Spec,
			BatchSize: snapshot.View.BatchSize, PauseAfterCanary: true, CreatedBy: createdBy, RetryOf: request.DeploymentID,
			Targets: deploymentTargetsFromActionPlan(snapshot.Result.Targets), Job: template,
		}
		return prepared, nil
	})
	if err != nil {
		s.recordDeploymentActionFallback(store.AuditDeploymentRetry, request.DeploymentID, request.Reason,
			request.IdempotencyKey, digest, request.Actor, result.Audited, err)
		return DeploymentMutationResult{}, err
	}
	projected, projectErr := projectDeploymentMutation("retry", result)
	if projectErr != nil {
		return DeploymentMutationResult{}, projectErr
	}
	return projected, nil
}

func (s *Service) ApplyDeploymentAbandon(request DeploymentAbandonApplyRequest) (DeploymentMutationResult, error) {
	return s.ApplyDeploymentAbandonContext(context.Background(), request)
}

func (s *Service) ApplyDeploymentAbandonContext(ctx context.Context, request DeploymentAbandonApplyRequest) (DeploymentMutationResult, error) {
	if ctx == nil {
		return DeploymentMutationResult{}, fmt.Errorf("%w: context is required", ErrInvalidDeploymentAction)
	}
	if err := ctx.Err(); err != nil {
		return DeploymentMutationResult{}, err
	}
	if s == nil || s.store == nil {
		return DeploymentMutationResult{}, fmt.Errorf("%w: store is required", ErrInvalidDeploymentAction)
	}
	digest := DeploymentAbandonSemanticDigest(request)
	audit := deploymentActionAudit(request.Actor, request.Reason, request.IdempotencyKey, digest)
	result, err := s.store.ApplyOperatorDeploymentAbandon(store.OperatorDeploymentRequest{
		DeploymentID: request.DeploymentID, PreviewDigest: request.PreviewDigest,
		ExpectedControlRevision: request.ExpectedControlRevision, ExpectedOpenedBatch: request.ExpectedOpenedBatch,
		ConfirmDeploymentID: request.ConfirmDeploymentID, IdempotencyKey: request.IdempotencyKey,
		RequestDigest: digest, Audit: audit,
	}, func() (store.OperatorDeploymentPrepared, error) {
		if err := validateDeploymentActionReason(request.Reason); err != nil {
			return store.OperatorDeploymentPrepared{}, err
		}
		snapshot, err := s.deploymentActionSnapshot(ctx, "abandon", request.DeploymentID, time.Now().UTC())
		if err != nil {
			return store.OperatorDeploymentPrepared{}, deploymentControlPrepareError(err)
		}
		prepared := store.OperatorDeploymentPrepared{CurrentPreviewDigest: snapshot.Result.PreviewDigest}
		if snapshot.Result.PreviewDigest != request.PreviewDigest {
			return prepared, nil
		}
		if !snapshot.Result.Eligibility.Eligible {
			return store.OperatorDeploymentPrepared{}, deploymentEligibilityRejection("abandon", snapshot.Result.Eligibility.Blockers)
		}
		return prepared, nil
	})
	if err != nil {
		s.recordDeploymentActionFallback(store.AuditDeploymentAbandon, request.DeploymentID, request.Reason,
			request.IdempotencyKey, digest, request.Actor, result.Audited, err)
		return DeploymentMutationResult{}, err
	}
	projected, projectErr := projectDeploymentMutation("abandon", result)
	if projectErr != nil {
		return DeploymentMutationResult{}, projectErr
	}
	return projected, nil
}

type deploymentActionSnapshot struct {
	Result                    DeploymentActionPreviewResult
	View                      store.DeploymentView
	Material                  artifact.OpenClawMaterial
	HasMaterial               bool
	TerminalFailureMachineIDs []string
}

func (s *Service) previewDeploymentAction(ctx context.Context, action, deploymentID string,
	evaluatedAt time.Time,
) (DeploymentActionPreviewResult, error) {
	snapshot, err := s.deploymentActionSnapshot(ctx, action, deploymentID, evaluatedAt)
	if err != nil {
		return DeploymentActionPreviewResult{}, err
	}
	return snapshot.Result, nil
}

func (s *Service) deploymentActionSnapshot(ctx context.Context, action, deploymentID string,
	evaluatedAt time.Time,
) (deploymentActionSnapshot, error) {
	if ctx == nil {
		return deploymentActionSnapshot{}, fmt.Errorf("%w: context is required", ErrInvalidDeploymentAction)
	}
	if err := ctx.Err(); err != nil {
		return deploymentActionSnapshot{}, err
	}
	if s == nil || s.store == nil || evaluatedAt.IsZero() || !validDeploymentIdentifier(deploymentID, 256) {
		return deploymentActionSnapshot{}, fmt.Errorf("%w: deployment_id and evaluated_at are required", ErrInvalidDeploymentAction)
	}
	switch action {
	case "continue", "skip_failed_batch", "retry", "abandon":
	default:
		return deploymentActionSnapshot{}, fmt.Errorf("%w: action is invalid", ErrInvalidDeploymentAction)
	}
	evaluatedAt = evaluatedAt.UTC()
	view, err := s.store.DeploymentView(deploymentID, evaluatedAt)
	if err != nil {
		return deploymentActionSnapshot{}, err
	}
	summary, err := projectDeploymentSummary(view)
	if err != nil {
		return deploymentActionSnapshot{}, err
	}
	actions, err := s.deploymentActionEligibility(view, summary.Material, evaluatedAt)
	if err != nil {
		return deploymentActionSnapshot{}, err
	}
	eligibility := actions.Continue
	switch action {
	case "skip_failed_batch":
		eligibility = actions.SkipFailedBatch
	case "retry":
		eligibility = actions.Retry
	case "abandon":
		eligibility = actions.Abandon
	}
	result := DeploymentActionPreviewResult{
		SchemaVersion: DeploymentActionSchemaVersion, PolicyVersion: DeploymentActionPolicyVersion,
		Action: action, PreviewedAt: evaluatedAt, Deployment: summary, Eligibility: eligibility,
		Targets: []DeploymentActionTargetPreview{}, TerminalFailureTargets: []DeploymentTerminalFailurePreview{},
	}
	snapshot := deploymentActionSnapshot{Result: result, View: view}

	for _, target := range view.Targets {
		if target.StuckKind != "terminal_failure" {
			continue
		}
		snapshot.Result.TerminalFailureTargets = append(snapshot.Result.TerminalFailureTargets,
			DeploymentTerminalFailurePreview{
				MachineID: target.MachineID, DisplayName: target.DisplayName,
				JobID: target.JobID, JobState: target.JobState,
			})
		snapshot.TerminalFailureMachineIDs = append(snapshot.TerminalFailureMachineIDs, target.MachineID)
	}
	sort.Slice(snapshot.Result.TerminalFailureTargets, func(i, j int) bool {
		return snapshot.Result.TerminalFailureTargets[i].MachineID < snapshot.Result.TerminalFailureTargets[j].MachineID
	})
	sort.Strings(snapshot.TerminalFailureMachineIDs)

	switch action {
	case "continue", "skip_failed_batch":
		if view.OpenedBatch < view.TotalBatches {
			for _, target := range view.Targets {
				if target.ExcludedReason == "" && target.JobID == "" && target.BatchNo == view.OpenedBatch+1 {
					snapshot.Result.Targets = append(snapshot.Result.Targets, actionTargetFromStored(target))
				}
			}
			if err := s.resolveActionMaterial(ctx, &snapshot, false); err != nil {
				return deploymentActionSnapshot{}, err
			}
			if snapshot.HasMaterial {
				template, templateErr := s.store.DeploymentJobTemplate(deploymentID)
				if templateErr != nil {
					return deploymentActionSnapshot{}, templateErr
				}
				if template.ArtifactDigest != snapshot.Material.Digest {
					// Store repeats this binding in the delayed-batch writer
					// transaction. Surface it in preview too, so an operator never
					// receives an eligible plan for a legacy split-authority ledger.
					addDeploymentActionBlocker(&snapshot.Result.Eligibility, "material_identity_changed")
				}
			}
		}
	case "retry":
		if err := s.resolveActionMaterial(ctx, &snapshot, true); err != nil {
			return deploymentActionSnapshot{}, err
		}
		if snapshot.HasMaterial && len(snapshot.TerminalFailureMachineIDs) > 0 {
			members := make([]store.Machine, 0, len(snapshot.TerminalFailureMachineIDs))
			for _, machineID := range snapshot.TerminalFailureMachineIDs {
				machine, getErr := s.store.GetMachine(machineID)
				if getErr != nil {
					addDeploymentActionBlocker(&snapshot.Result.Eligibility, "target_snapshot_changed")
					continue
				}
				if machine.RetiredAt != nil || machine.Channel != view.Channel {
					addDeploymentActionBlocker(&snapshot.Result.Eligibility, "target_snapshot_changed")
				}
				members = append(members, machine)
			}
			if len(members) == len(snapshot.TerminalFailureMachineIDs) {
				plan, planErr := s.store.PlanMachinesDeployment(members, snapshot.Material.EnginesNode, view.BatchSize, evaluatedAt)
				if planErr != nil {
					return deploymentActionSnapshot{}, planErr
				}
				plan = rollout.ApplyCanaryFirst(plan, view.BatchSize)
				planned, projectErr := projectDeploymentPlan(plan, view.BatchSize)
				if projectErr != nil {
					return deploymentActionSnapshot{}, projectErr
				}
				for _, target := range planned {
					snapshot.Result.Targets = append(snapshot.Result.Targets, actionTargetFromPlan(target))
				}
				if plan.Impact == 0 {
					addDeploymentActionBlocker(&snapshot.Result.Eligibility, "no_included_targets")
				}
			}
		}
	case "abandon":
		for _, target := range view.Targets {
			if target.ExcludedReason == "" && target.JobID == "" {
				snapshot.Result.Targets = append(snapshot.Result.Targets, actionTargetFromStored(target))
			}
		}
	}
	sort.Slice(snapshot.Result.Targets, func(i, j int) bool {
		return snapshot.Result.Targets[i].MachineID < snapshot.Result.Targets[j].MachineID
	})

	if ((action == "continue" || action == "skip_failed_batch") && view.OpenedBatch < view.TotalBatches) || action == "retry" {
		if view.Channel == "stable" && summary.Material.Status == DeploymentMaterialRecorded &&
			summary.Material.Version != nil && summary.Material.ArtifactDigest != nil {
			decision, promoteErr := s.store.PreviewStableOpenClawPromotion(
				*summary.Material.Version, strings.TrimPrefix(*summary.Material.ArtifactDigest, "sha256:"), evaluatedAt)
			if promoteErr != nil {
				return deploymentActionSnapshot{}, promoteErr
			}
			promotion := projectDeploymentPromotion(decision)
			snapshot.Result.Promotion = &promotion
		}
	}
	snapshot.Result.Eligibility.Eligible = len(snapshot.Result.Eligibility.Blockers) == 0
	snapshot.Result.PreviewDigest = deploymentActionPreviewDigest(snapshot.Result)
	return snapshot, nil
}

func (s *Service) resolveActionMaterial(ctx context.Context, snapshot *deploymentActionSnapshot,
	requireExactSpec bool,
) error {
	materialSummary := snapshot.Result.Deployment.Material
	if materialSummary.Status != DeploymentMaterialRecorded || materialSummary.Version == nil ||
		materialSummary.ArtifactDigest == nil || materialSummary.SizeBytes == nil {
		addDeploymentActionBlocker(&snapshot.Result.Eligibility, "invalid_material")
		return nil
	}
	if strings.TrimSpace(s.artifactsDir) == "" {
		addDeploymentActionBlocker(&snapshot.Result.Eligibility, "material_unavailable")
		return nil
	}
	material, err := artifact.ResolveOpenClawMaterialContext(ctx, s.artifactsDir, *materialSummary.Version,
		strings.TrimPrefix(*materialSummary.ArtifactDigest, "sha256:"))
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		addDeploymentActionBlocker(&snapshot.Result.Eligibility, "material_unavailable")
		return nil
	}
	if validateDeploymentArtifactPreviewMaterial(material) != nil {
		addDeploymentActionBlocker(&snapshot.Result.Eligibility, "material_unavailable")
		return nil
	}
	engines := ""
	if materialSummary.EnginesNode != nil {
		engines = *materialSummary.EnginesNode
	}
	if material.Version != *materialSummary.Version || material.Digest != *materialSummary.ArtifactDigest ||
		material.Artifact.Size != *materialSummary.SizeBytes || material.EnginesNode != engines ||
		(requireExactSpec && material.Spec != snapshot.View.Spec) {
		addDeploymentActionBlocker(&snapshot.Result.Eligibility, "material_identity_changed")
		return nil
	}
	snapshot.Material = material
	snapshot.HasMaterial = true
	artifactPreview := deploymentArtifactPreview(material)
	snapshot.Result.Artifact = &artifactPreview
	return nil
}

func actionTargetFromStored(target store.DeploymentTarget) DeploymentActionTargetPreview {
	result := DeploymentActionTargetPreview{
		MachineID: target.MachineID, DisplayName: target.DisplayName, BatchNo: target.BatchNo,
	}
	if target.JobID != "" {
		jobID, state := target.JobID, target.JobState
		result.JobID, result.JobState = &jobID, &state
	}
	if target.ExcludedReason != "" {
		reason := target.ExcludedReason
		result.ExcludedReason = &reason
	}
	return result
}

func actionTargetFromPlan(target DeploymentPlanTargetPreview) DeploymentActionTargetPreview {
	return DeploymentActionTargetPreview{
		MachineID: target.MachineID, DisplayName: target.DisplayName, BatchNo: target.BatchNo,
		ExcludedReason: target.ExcludedReason,
	}
}

func addDeploymentActionBlocker(eligibility *DeploymentActionEligibility, blocker string) {
	for _, existing := range eligibility.Blockers {
		if existing == blocker {
			return
		}
	}
	eligibility.Blockers = append(eligibility.Blockers, blocker)
}

func deploymentActionPreviewDigest(result DeploymentActionPreviewResult) string {
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
		Version                int                                `json:"version"`
		PolicyVersion          string                             `json:"policy_version"`
		Action                 string                             `json:"action"`
		Deployment             DeploymentSummary                  `json:"deployment"`
		Eligibility            DeploymentActionEligibility        `json:"eligibility"`
		Targets                []DeploymentActionTargetPreview    `json:"targets"`
		TerminalFailureTargets []DeploymentTerminalFailurePreview `json:"terminal_failure_targets"`
		Artifact               *artifactIdentity                  `json:"artifact"`
		Promotion              *DeploymentPromotionPreview        `json:"promotion"`
	}{
		Version: DeploymentActionSchemaVersion, PolicyVersion: DeploymentActionPolicyVersion,
		Action: result.Action, Deployment: result.Deployment, Eligibility: result.Eligibility,
		Targets: result.Targets, TerminalFailureTargets: result.TerminalFailureTargets,
		Artifact: artifactValue, Promotion: result.Promotion,
	}
	return sha256JSON(body)
}

func DeploymentCreateApplySemanticDigest(request DeploymentCreateApplyRequest) string {
	body := struct {
		Version                 int    `json:"version"`
		Channel                 string `json:"channel"`
		ArtifactVersion         string `json:"artifact_version"`
		ArtifactSHA256          string `json:"artifact_sha256"`
		BatchSize               int    `json:"batch_size"`
		ExecutionTimeoutSeconds int    `json:"execution_timeout_seconds"`
		Irreversible            bool   `json:"irreversible"`
		PreviewDigest           string `json:"preview_digest"`
		ConfirmChannel          string `json:"confirm_channel"`
		ConfirmVersion          string `json:"confirm_version"`
		Reason                  string `json:"reason"`
	}{
		DeploymentActionSchemaVersion, request.Channel, request.Version, request.ArtifactSHA256,
		request.BatchSize, request.ExecutionTimeoutSeconds, request.Irreversible,
		request.PreviewDigest, request.ConfirmChannel, request.ConfirmVersion, request.Reason,
	}
	return sha256JSON(body)
}

func DeploymentSkipFailedBatchSemanticDigest(request DeploymentContinueApplyRequest) string {
	body := struct {
		Version                 int    `json:"version"`
		Action                  string `json:"action"`
		DeploymentID            string `json:"deployment_id"`
		PreviewDigest           string `json:"preview_digest"`
		ExpectedControlRevision *int64 `json:"expected_control_revision"`
		ExpectedOpenedBatch     *int   `json:"expected_opened_batch"`
		ConfirmChannel          string `json:"confirm_channel"`
		Reason                  string `json:"reason"`
	}{DeploymentActionSchemaVersion, "skip_failed_batch", request.DeploymentID, request.PreviewDigest,
		request.ExpectedControlRevision, request.ExpectedOpenedBatch, request.ConfirmChannel, request.Reason}
	return sha256JSON(body)
}

func DeploymentContinueSemanticDigest(request DeploymentContinueApplyRequest) string {
	body := struct {
		Version                 int    `json:"version"`
		DeploymentID            string `json:"deployment_id"`
		PreviewDigest           string `json:"preview_digest"`
		ExpectedControlRevision *int64 `json:"expected_control_revision"`
		ExpectedOpenedBatch     *int   `json:"expected_opened_batch"`
		ConfirmChannel          string `json:"confirm_channel"`
		Reason                  string `json:"reason"`
	}{DeploymentActionSchemaVersion, request.DeploymentID, request.PreviewDigest,
		request.ExpectedControlRevision, request.ExpectedOpenedBatch, request.ConfirmChannel, request.Reason}
	return sha256JSON(body)
}

func DeploymentRetrySemanticDigest(request DeploymentRetryApplyRequest) string {
	body := struct {
		Version                 int    `json:"version"`
		DeploymentID            string `json:"deployment_id"`
		PreviewDigest           string `json:"preview_digest"`
		ExpectedControlRevision *int64 `json:"expected_control_revision"`
		ExpectedOpenedBatch     *int   `json:"expected_opened_batch"`
		ConfirmChannel          string `json:"confirm_channel"`
		ConfirmVersion          string `json:"confirm_version"`
		Reason                  string `json:"reason"`
	}{DeploymentActionSchemaVersion, request.DeploymentID, request.PreviewDigest,
		request.ExpectedControlRevision, request.ExpectedOpenedBatch,
		request.ConfirmChannel, request.ConfirmVersion, request.Reason}
	return sha256JSON(body)
}

func DeploymentAbandonSemanticDigest(request DeploymentAbandonApplyRequest) string {
	body := struct {
		Version                 int    `json:"version"`
		DeploymentID            string `json:"deployment_id"`
		PreviewDigest           string `json:"preview_digest"`
		ExpectedControlRevision *int64 `json:"expected_control_revision"`
		ExpectedOpenedBatch     *int   `json:"expected_opened_batch"`
		ConfirmDeploymentID     string `json:"confirm_deployment_id"`
		Reason                  string `json:"reason"`
	}{DeploymentActionSchemaVersion, request.DeploymentID, request.PreviewDigest,
		request.ExpectedControlRevision, request.ExpectedOpenedBatch,
		request.ConfirmDeploymentID, request.Reason}
	return sha256JSON(body)
}

func sha256JSON(value any) string {
	raw, _ := json.Marshal(value)
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func deploymentTargetsFromPlan(targets []DeploymentPlanTargetPreview) []store.NewDeploymentTarget {
	result := make([]store.NewDeploymentTarget, 0, len(targets))
	for _, target := range targets {
		excluded := ""
		if target.ExcludedReason != nil {
			excluded = *target.ExcludedReason
		}
		result = append(result, store.NewDeploymentTarget{
			MachineID: target.MachineID, BatchNo: target.BatchNo, ExcludedReason: excluded,
		})
	}
	return result
}

func deploymentTargetsFromActionPlan(targets []DeploymentActionTargetPreview) []store.NewDeploymentTarget {
	result := make([]store.NewDeploymentTarget, 0, len(targets))
	for _, target := range targets {
		excluded := ""
		if target.ExcludedReason != nil {
			excluded = *target.ExcludedReason
		}
		result = append(result, store.NewDeploymentTarget{
			MachineID: target.MachineID, BatchNo: target.BatchNo, ExcludedReason: excluded,
		})
	}
	return result
}

func sameDeploymentArtifactIdentity(preview DeploymentArtifactPreview, material artifact.OpenClawMaterial) bool {
	record := material.Artifact
	return preview.Name == record.Name && preview.Version == record.Version && preview.SHA256 == record.SHA256 &&
		preview.Digest == material.Digest && preview.SizeBytes == record.Size && preview.EnginesNode == record.EnginesNode &&
		preview.SHA512Integrity == record.SHA512Integrity && preview.FetchedAt.Equal(record.FetchedAt.UTC()) &&
		preview.AvailableAndVerified
}

func deploymentCreatedBy(actor Actor) (string, error) {
	if actor.AuthDecision == string(operatorauth.Authorized) && validDeploymentText(actor.AuthSubject, 256, false) {
		return actor.AuthSubject, nil
	}
	if actor.SourceKind == SourceKindDirectDBCLI {
		identity := "direct-db-cli"
		if validDeploymentText(actor.WhoUser, 200, true) && actor.WhoUser != "" {
			identity += ":" + actor.WhoUser
		}
		return identity, nil
	}
	return "", fmt.Errorf("%w: create/retry requires a verified actor", ErrInvalidDeploymentAction)
}

func deploymentCreatePrepareError(channel string, err error) error {
	if channel != "canary" && channel != "stable" {
		return deploymentRejection(store.OperatorCodeBadChannel)
	}
	if errors.Is(err, ErrInvalidDeploymentPreview) {
		return deploymentRejection(store.OperatorCodeDeploymentPreviewStale)
	}
	return err
}

func deploymentControlPrepareError(err error) error {
	if errors.Is(err, store.ErrDeploymentNotFound) || errors.Is(err, store.ErrNotFound) {
		return deploymentRejection(store.OperatorCodeDeploymentNotFound)
	}
	if errors.Is(err, ErrInvalidDeploymentAction) || errors.Is(err, ErrInvalidDeploymentRead) ||
		errors.Is(err, ErrInvalidDeploymentPreview) {
		return deploymentRejection(store.OperatorCodeDeploymentPreviewStale)
	}
	return err
}

func deploymentEligibilityRejection(action string, blockers []string) error {
	if containsActionBlocker(blockers, "failed_batch_requires_explicit_skip") {
		return deploymentRejection(store.OperatorCodeDeploymentContinueRefused)
	}
	if containsActionBlocker(blockers, "stable_promotion_locked") {
		return deploymentRejection(store.OperatorCodeDeploymentPromotionBlocked)
	}
	if containsActionBlocker(blockers, "nonterminal_jobs") {
		return deploymentRejection(store.OperatorCodeDeploymentActiveJobs)
	}
	if action == "retry" && containsActionBlocker(blockers, "no_terminal_failure_targets") {
		return deploymentRejection(store.OperatorCodeDeploymentNoRetryTargets)
	}
	if containsActionBlocker(blockers, "deployment_not_paused") ||
		containsActionBlocker(blockers, "deployment_not_retryable") {
		return deploymentRejection(store.OperatorCodeDeploymentNotPaused)
	}
	if containsActionBlocker(blockers, "no_included_targets") {
		return deploymentRejection(store.OperatorCodeDeploymentNoIncludedTargets)
	}
	return deploymentRejection(store.OperatorCodeDeploymentPreviewStale)
}

func containsActionBlocker(blockers []string, want string) bool {
	for _, blocker := range blockers {
		if blocker == want {
			return true
		}
	}
	return false
}

func validateDeploymentActionReason(reason string) error {
	if reason != strings.TrimSpace(reason) || !validDeploymentText(reason, 500, true) {
		return fmt.Errorf("%w: reason must be canonical text up to 500 bytes", ErrInvalidDeploymentAction)
	}
	return nil
}

func deploymentRejection(code string) error {
	return &store.OperatorRequestError{Code: code, Detail: code}
}

func deploymentActionAudit(actor Actor, reason, key, digest string) store.AuditEntry {
	audit := auditFromActor(actor)
	audit.Reason = reason
	audit.IdempotencyKey = key
	audit.RequestDigest = digest
	return audit
}

func projectDeploymentMutation(action string, result store.OperatorDeploymentResult) (DeploymentMutationResult, error) {
	d := result.Deployment
	if !validDeploymentIdentifier(d.DeploymentID, 256) ||
		(d.Channel != "canary" && d.Channel != "stable") ||
		!validDeploymentIdentifier(d.ResourceKind, 128) || !validDeploymentIdentifier(d.ResourceID, 256) ||
		d.Revision <= 0 || d.ControlRevision < 0 || d.BatchSize < 1 || d.BatchSize > store.MaxDeploymentBatchSize ||
		!isDeploymentState(d.State) || d.CreatedAt.IsZero() || result.OpenedBatch < 1 {
		return DeploymentMutationResult{}, errors.New("operator: Store returned an invalid deployment mutation receipt")
	}
	projected := DeploymentMutationResult{
		SchemaVersion: DeploymentActionSchemaVersion, Action: action,
		DeploymentID: d.DeploymentID, Channel: d.Channel, ResourceKind: d.ResourceKind, ResourceID: d.ResourceID,
		DesiredRevision: d.Revision, ControlRevision: d.ControlRevision, BatchSize: d.BatchSize,
		State: d.State, CreatedAt: d.CreatedAt.UTC(), PausedAt: utcTimePtr(d.PausedAt), FinishedAt: utcTimePtr(d.FinishedAt),
		OpenedBatch: result.OpenedBatch, Jobs: make([]DeploymentMutationJob, 0, len(result.Jobs)),
		PreviewDigest: result.PreviewDigest, Replayed: result.Replayed,
	}
	if d.RetryOf != "" {
		if !validDeploymentIdentifier(d.RetryOf, 256) {
			return DeploymentMutationResult{}, errors.New("operator: Store returned an invalid retry identity")
		}
		retryOf := d.RetryOf
		projected.RetryOf = &retryOf
	}
	for _, job := range result.Jobs {
		if !validDeploymentIdentifier(job.JobID, 256) || !validDeploymentIdentifier(job.MachineID, 256) ||
			job.Revision != d.Revision || !deploy.IsKnownJobState(job.State) || job.CreatedAt.IsZero() {
			return DeploymentMutationResult{}, errors.New("operator: Store returned an invalid deployment job receipt")
		}
		projected.Jobs = append(projected.Jobs, DeploymentMutationJob{
			JobID: job.JobID, MachineID: job.MachineID, DesiredRevision: job.Revision,
			State: job.State, CreatedAt: job.CreatedAt.UTC(),
		})
	}
	return projected, nil
}

func (s *Service) recordDeploymentActionFallback(action store.AuditAction, subject, reason, key, digest string,
	actor Actor, resultAudited bool, err error,
) {
	var rejection *store.OperatorRequestError
	if resultAudited || (errors.As(err, &rejection) && rejection.Audited) {
		return
	}
	entry := deploymentActionAudit(actor, reason, key, digest)
	entry.Action, entry.Subject, entry.OK = action, subject, false
	entry.Detail = err.Error()
	if errors.As(err, &rejection) && rejection.Replayed {
		entry.Detail = store.OperatorIdempotencyReplayPrefix + "原判決：" + entry.Detail
	}
	if auditErr := s.store.RecordAudit(entry); auditErr != nil {
		log.Printf("operator deployment audit 寫入失敗 action=%s subject=%s: %v", action, subject, auditErr)
	}
}
