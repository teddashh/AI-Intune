// Package operatoragent is the stdio MCP server and thin CLI in front of the
// existing operator JSON API. It does not open the ledger and it does not
// invent an operator credential. The process must run on a tailnet node whose
// source address Hub can WhoIs.
package operatoragent

import (
	"context"

	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/operatorclient"
	"github.com/teddashh/AI-Intune/internal/store"
)

// Hub is the subset of operatorclient.Client these tools call.
// *operatorclient.Client implements it. A test double must too.
type Hub interface {
	ListMachines(context.Context, operator.MachineListRequest) (operator.MachineListResult, error)
	Machine(context.Context, string) (operator.MachineDetailResult, error)
	MachineEvidence(context.Context, string, int) (operator.MachineEvidenceResult, error)
	Jobs(context.Context, operator.JobListRequest) (operator.JobListResult, error)
	Job(context.Context, string) (operator.JobDetailResult, error)
	JobEvidence(context.Context, string, int) (operator.JobEvidenceResult, error)
	Deployments(context.Context, operator.DeploymentListRequest) (operator.DeploymentListResult, error)
	Deployment(context.Context, string) (operator.DeploymentDetailResult, error)
	SoftwareReport(context.Context) (operator.SoftwareReport, error)
	ComplianceBoard(context.Context) (operator.ComplianceBoardResult, error)
	PreviewEnrollToken(context.Context, operatorclient.EnrollmentTokenPreviewRequest) (operatorclient.EnrollmentTokenPreviewResponse, error)
	CreateEnrollToken(context.Context, string, operatorclient.EnrollmentTokenCreateRequest) (operatorclient.EnrollmentTokenResponse, error)
	PreviewDeploymentCreate(context.Context, operator.DeploymentCreatePreviewRequest) (operator.DeploymentCreatePreviewResult, error)
	CreateDeployment(context.Context, string, operatorclient.DeploymentCreateRequest) (operator.DeploymentMutationResult, error)
	PreviewDeploymentContinue(context.Context, string) (operator.DeploymentActionPreviewResult, error)
	ContinueDeployment(context.Context, string, string, operatorclient.DeploymentContinueRequest) (operator.DeploymentMutationResult, error)
	PreviewDeploymentAbandon(context.Context, string) (operator.DeploymentActionPreviewResult, error)
	AbandonDeployment(context.Context, string, string, operatorclient.DeploymentAbandonRequest) (operator.DeploymentMutationResult, error)
	PreviewMachineProfileAssignment(context.Context, operator.MachineProfileAssignmentPreviewRequest) (operator.MachineProfileAssignmentPreviewResult, error)
	AssignMachineProfile(context.Context, string, operator.MachineProfileAssignmentRequest) (operator.MachineProfileAssignmentResult, error)
	DiskCleanSummaries(context.Context) ([]store.DiskCleanSummaryView, error)
	DiskCleanSummary(context.Context, string) (store.DiskCleanSummaryView, error)
	PreviewDiskCleanProfile(context.Context, operatorclient.DiskCleanProfilePreviewRequest) (store.DiskCleanProfilePreview, error)
	PublishDiskCleanProfile(context.Context, string, operatorclient.DiskCleanProfilePublishRequest) (store.DiskCleanProfileResult, error)
	PreviewDiskCleanDryRun(context.Context, operatorclient.DiskCleanTargetPreviewRequest) (store.DiskCleanDryRunPreview, error)
	ApplyDiskCleanDryRun(context.Context, string, operatorclient.DiskCleanTargetApplyRequest) (store.DiskCleanDryRunResult, error)
	PreviewDiskCleanCanary(context.Context, operatorclient.DiskCleanCanaryPreviewRequest) (store.DiskCleanCanaryPreview, error)
	ApplyDiskCleanCanary(context.Context, string, operatorclient.DiskCleanCanaryApplyRequest) (store.DiskCleanCanaryResult, error)
	PreviewDiskCleanContinue(context.Context, string) (store.DiskCleanControlPreview, error)
	ContinueDiskClean(context.Context, string, operatorclient.DiskCleanControlApplyRequest) (store.DiskCleanRolloutResult, error)
	PreviewDiskCleanAbandon(context.Context, string) (store.DiskCleanControlPreview, error)
	AbandonDiskClean(context.Context, string, operatorclient.DiskCleanControlApplyRequest) (store.DiskCleanRolloutResult, error)
}

// Static check: the real client remains the Hub implementation.
var _ Hub = (*operatorclient.Client)(nil)
