package operatorclient

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/teddashh/AI-Intune/internal/deploy"
	"github.com/teddashh/AI-Intune/internal/store"
)

type DiagnosticNoopPreviewRequest struct {
	ExecutionTimeoutSeconds int `json:"execution_timeout_seconds"`
}

type DiagnosticNoopPreviewResponse struct {
	MachineID                   string                                `json:"machine_id"`
	DisplayName                 string                                `json:"display_name"`
	LifecycleRevision           int64                                 `json:"lifecycle_revision"`
	PreviewedAt                 time.Time                             `json:"previewed_at"`
	Kind                        string                                `json:"kind"`
	ResourceKind                string                                `json:"resource_kind"`
	ResourceID                  string                                `json:"resource_id"`
	AgentWatermarkScope         string                                `json:"agent_watermark_scope"`
	SpecDigest                  string                                `json:"spec_digest"`
	ExecutionTimeoutSeconds     int                                   `json:"execution_timeout_seconds"`
	ChangesMachineConfiguration bool                                  `json:"changes_machine_configuration"`
	CreatesDesiredState         bool                                  `json:"creates_desired_state"`
	CreatesJob                  bool                                  `json:"creates_job"`
	DeliveryRequiresJobsEnabled bool                                  `json:"delivery_requires_jobs_enabled"`
	JobsEnabled                 *bool                                 `json:"jobs_enabled"`
	EverReported                bool                                  `json:"ever_reported"`
	ActiveJobCount              int64                                 `json:"active_job_count"`
	CurrentResourceRevision     deploy.Revision                       `json:"current_resource_revision"`
	PlannedRevision             deploy.Revision                       `json:"planned_revision"`
	Blockers                    []store.OperatorDiagnosticNoopBlocker `json:"blockers"`
	PreviewDigest               string                                `json:"preview_digest"`
}

type DiagnosticNoopRequest struct {
	ExecutionTimeoutSeconds int    `json:"execution_timeout_seconds"`
	ConfirmDisplayName      string `json:"confirm_display_name"`
	PreviewDigest           string `json:"preview_digest"`
	Reason                  string `json:"reason"`
}

type DiagnosticNoopResponse struct {
	MachineID                   string                                `json:"machine_id"`
	DisplayName                 string                                `json:"display_name"`
	DesiredID                   string                                `json:"desired_id"`
	JobID                       string                                `json:"job_id"`
	Revision                    deploy.Revision                       `json:"revision"`
	CreatedAt                   time.Time                             `json:"created_at"`
	LifecycleRevision           int64                                 `json:"lifecycle_revision"`
	Kind                        string                                `json:"kind"`
	ResourceKind                string                                `json:"resource_kind"`
	ResourceID                  string                                `json:"resource_id"`
	AgentWatermarkScope         string                                `json:"agent_watermark_scope"`
	SpecDigest                  string                                `json:"spec_digest"`
	ExecutionTimeoutSeconds     int                                   `json:"execution_timeout_seconds"`
	ChangesMachineConfiguration bool                                  `json:"changes_machine_configuration"`
	CreatesDesiredState         bool                                  `json:"creates_desired_state"`
	CreatesJob                  bool                                  `json:"creates_job"`
	DeliveryRequiresJobsEnabled bool                                  `json:"delivery_requires_jobs_enabled"`
	JobsEnabled                 *bool                                 `json:"jobs_enabled"`
	EverReported                bool                                  `json:"ever_reported"`
	ActiveJobCount              int64                                 `json:"active_job_count"`
	CurrentResourceRevision     deploy.Revision                       `json:"current_resource_revision"`
	PlannedRevision             deploy.Revision                       `json:"planned_revision"`
	Blockers                    []store.OperatorDiagnosticNoopBlocker `json:"blockers"`
	PreviewDigest               string                                `json:"preview_digest"`
	Replayed                    bool                                  `json:"replayed"`
	Meta                        ResponseMetadata                      `json:"-"`
}

func (c *Client) PreviewDiagnosticNoop(ctx context.Context, machineID string,
	body DiagnosticNoopPreviewRequest,
) (DiagnosticNoopPreviewResponse, error) {
	if body.ExecutionTimeoutSeconds < store.OperatorDiagnosticNoopMinTimeoutSeconds ||
		body.ExecutionTimeoutSeconds > store.OperatorDiagnosticNoopMaxTimeoutSeconds {
		return DiagnosticNoopPreviewResponse{}, errors.New("operator client: diagnostic noop timeout is outside policy")
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return DiagnosticNoopPreviewResponse{}, fmt.Errorf("operator client: encode diagnostic noop preview: %w", err)
	}
	req, err := c.newMachineOperatorRequest(ctx, http.MethodPost, machineID, "/diagnostic-noop-preview", bytes.NewReader(raw))
	if err != nil {
		return DiagnosticNoopPreviewResponse{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	response, err := c.doRaw(req)
	if err != nil {
		return DiagnosticNoopPreviewResponse{}, err
	}
	if response.status != http.StatusOK {
		return DiagnosticNoopPreviewResponse{}, fmt.Errorf("operator client: diagnostic noop preview returned HTTP %d", response.status)
	}
	if err := validateJSONNoStoreResponse(response.header); err != nil {
		return DiagnosticNoopPreviewResponse{}, err
	}
	if replayed, err := responseReplayEvidenceFromHeader(response.header); err != nil {
		return DiagnosticNoopPreviewResponse{}, err
	} else if replayed {
		return DiagnosticNoopPreviewResponse{}, errors.New("operator client: diagnostic noop preview cannot be an idempotency replay")
	}
	var result DiagnosticNoopPreviewResponse
	if err := decodeStrictJSONDocument(response.body, "diagnostic noop preview", &result); err != nil {
		return DiagnosticNoopPreviewResponse{}, err
	}
	if err := validateDiagnosticNoopPreview(result, machineID, body.ExecutionTimeoutSeconds); err != nil {
		return DiagnosticNoopPreviewResponse{}, err
	}
	return result, nil
}

func (c *Client) CreateDiagnosticNoop(ctx context.Context, machineID, idempotencyKey string,
	body DiagnosticNoopRequest,
) (DiagnosticNoopResponse, error) {
	if strings.TrimSpace(idempotencyKey) == "" || len(idempotencyKey) > 200 ||
		body.ExecutionTimeoutSeconds < store.OperatorDiagnosticNoopMinTimeoutSeconds ||
		body.ExecutionTimeoutSeconds > store.OperatorDiagnosticNoopMaxTimeoutSeconds ||
		strings.TrimSpace(body.ConfirmDisplayName) == "" || !validSHA256Digest(body.PreviewDigest) ||
		strings.TrimSpace(body.Reason) == "" {
		return DiagnosticNoopResponse{}, errors.New("operator client: diagnostic noop apply request is incomplete or invalid")
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return DiagnosticNoopResponse{}, fmt.Errorf("operator client: encode diagnostic noop apply: %w", err)
	}
	req, err := c.newMachineOperatorRequest(ctx, http.MethodPost, machineID, "/diagnostic-noop-jobs", bytes.NewReader(raw))
	if err != nil {
		return DiagnosticNoopResponse{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", idempotencyKey)
	response, err := c.doRaw(req)
	if err != nil {
		return DiagnosticNoopResponse{}, err
	}
	if response.status != http.StatusCreated && response.status != http.StatusOK {
		return DiagnosticNoopResponse{}, fmt.Errorf("operator client: diagnostic noop create returned HTTP %d", response.status)
	}
	if err := validateJSONNoStoreResponse(response.header); err != nil {
		return DiagnosticNoopResponse{}, err
	}
	var result DiagnosticNoopResponse
	if err := decodeStrictJSONDocument(response.body, "diagnostic noop create", &result); err != nil {
		return DiagnosticNoopResponse{}, err
	}
	replayed, err := responseReplayEvidenceFromHeader(response.header)
	if err != nil {
		return DiagnosticNoopResponse{}, err
	}
	if result.Replayed != replayed || (result.Replayed && response.status != http.StatusOK) ||
		(!result.Replayed && response.status != http.StatusCreated) {
		return DiagnosticNoopResponse{}, errors.New("operator client: diagnostic noop status and replay evidence disagree")
	}
	meta, err := diagnosticNoopResponseMetadata(response.header, result, machineID)
	if err != nil {
		return DiagnosticNoopResponse{}, err
	}
	result.Meta = meta
	if err := validateDiagnosticNoopResult(result, machineID, body); err != nil {
		return DiagnosticNoopResponse{}, err
	}
	return result, nil
}

func diagnosticNoopResponseMetadata(header http.Header, result DiagnosticNoopResponse, machineID string) (ResponseMetadata, error) {
	locations := header.Values("Location")
	wantLocation := "/v1/operator/jobs/" + result.JobID
	if len(locations) != 1 || locations[0] != wantLocation {
		return ResponseMetadata{}, errors.New("operator client: diagnostic noop Location does not identify returned job")
	}
	etags := header.Values("ETag")
	wantETag := `"diagnostic-noop-revision-` + strconv.FormatInt(int64(result.Revision), 10) + `"`
	if len(etags) != 1 || etags[0] != wantETag {
		return ResponseMetadata{}, errors.New("operator client: diagnostic noop ETag does not match returned revision")
	}
	if result.MachineID != machineID {
		return ResponseMetadata{}, errors.New("operator client: diagnostic noop response machine identity changed")
	}
	return ResponseMetadata{ETag: wantETag, ETagRevision: int64(result.Revision), IdempotencyReplayed: result.Replayed}, nil
}

func validateDiagnosticNoopPreview(result DiagnosticNoopPreviewResponse, machineID string, timeout int) error {
	if result.MachineID != machineID || strings.TrimSpace(result.DisplayName) == "" || result.LifecycleRevision < 0 ||
		result.PreviewedAt.IsZero() || !result.PreviewedAt.Equal(result.PreviewedAt.UTC().Truncate(time.Second)) ||
		result.ExecutionTimeoutSeconds != timeout || !validSHA256Digest(result.PreviewDigest) {
		return errors.New("operator client: invalid diagnostic noop preview identity, clock, timeout or digest")
	}
	return validateDiagnosticNoopImpact(result.Kind, result.ResourceKind, result.ResourceID,
		result.AgentWatermarkScope, result.SpecDigest, result.ChangesMachineConfiguration,
		result.CreatesDesiredState, result.CreatesJob, result.DeliveryRequiresJobsEnabled,
		result.EverReported, result.JobsEnabled,
		result.ActiveJobCount, result.CurrentResourceRevision,
		result.PlannedRevision, result.Blockers)
}

func validateDiagnosticNoopResult(result DiagnosticNoopResponse, machineID string, request DiagnosticNoopRequest) error {
	if result.MachineID != machineID || result.DisplayName != request.ConfirmDisplayName ||
		result.DesiredID == "" || result.JobID == "" || result.Revision <= 0 ||
		result.Revision != result.PlannedRevision || result.LifecycleRevision < 0 ||
		result.CreatedAt.IsZero() || !result.CreatedAt.Equal(result.CreatedAt.UTC().Truncate(time.Second)) ||
		result.ExecutionTimeoutSeconds != request.ExecutionTimeoutSeconds || result.PreviewDigest != request.PreviewDigest ||
		!result.EverReported || result.ActiveJobCount != 0 || len(result.Blockers) != 0 {
		return errors.New("operator client: invalid diagnostic noop creation receipt")
	}
	return validateDiagnosticNoopImpact(result.Kind, result.ResourceKind, result.ResourceID,
		result.AgentWatermarkScope, result.SpecDigest, result.ChangesMachineConfiguration,
		result.CreatesDesiredState, result.CreatesJob, result.DeliveryRequiresJobsEnabled,
		result.EverReported, result.JobsEnabled,
		result.ActiveJobCount, result.CurrentResourceRevision,
		result.PlannedRevision, result.Blockers)
}

func validateDiagnosticNoopImpact(kind, resourceKind, resourceID, watermarkScope, specDigest string,
	changesConfig, createsDesired, createsJob, requiresJobs bool,
	everReported bool, jobsEnabled *bool,
	activeJobs int64, currentRevision, plannedRevision deploy.Revision,
	blockers []store.OperatorDiagnosticNoopBlocker,
) error {
	wantSpecDigest := fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(store.OperatorDiagnosticNoopSpec)))
	if kind != store.OperatorDiagnosticNoopKind || resourceKind != store.OperatorDiagnosticNoopResourceKind ||
		resourceID != store.OperatorDiagnosticNoopResourceID || watermarkScope != store.OperatorDiagnosticNoopWatermarkScope ||
		specDigest != wantSpecDigest || changesConfig || !createsDesired || !createsJob || !requiresJobs ||
		activeJobs < 0 || currentRevision < 0 || plannedRevision != currentRevision+1 || blockers == nil {
		return errors.New("operator client: diagnostic noop impact violates the fixed protocol contract")
	}
	seen := map[store.OperatorDiagnosticNoopBlocker]bool{}
	for _, blocker := range blockers {
		if blocker != store.OperatorDiagnosticNoopBlockerRetired && blocker != store.OperatorDiagnosticNoopBlockerNeverReported &&
			blocker != store.OperatorDiagnosticNoopBlockerNonterminalJob &&
			blocker != store.OperatorDiagnosticNoopBlockerExecutionUnknown &&
			blocker != store.OperatorDiagnosticNoopBlockerExecutionDisabled || seen[blocker] {
			return errors.New("operator client: diagnostic noop preview contains unknown or duplicate blocker")
		}
		seen[blocker] = true
	}
	if seen[store.OperatorDiagnosticNoopBlockerNeverReported] != !everReported ||
		seen[store.OperatorDiagnosticNoopBlockerNonterminalJob] != (activeJobs > 0) {
		return errors.New("operator client: diagnostic noop blockers do not match machine state")
	}
	switch {
	case !everReported:
		if jobsEnabled != nil || seen[store.OperatorDiagnosticNoopBlockerExecutionUnknown] ||
			seen[store.OperatorDiagnosticNoopBlockerExecutionDisabled] {
			return errors.New("operator client: unreported machine has execution evidence")
		}
	case jobsEnabled == nil:
		if !seen[store.OperatorDiagnosticNoopBlockerExecutionUnknown] || seen[store.OperatorDiagnosticNoopBlockerExecutionDisabled] {
			return errors.New("operator client: unknown execution state is not blocked")
		}
	case !*jobsEnabled:
		if seen[store.OperatorDiagnosticNoopBlockerExecutionUnknown] || !seen[store.OperatorDiagnosticNoopBlockerExecutionDisabled] {
			return errors.New("operator client: disabled execution state is not blocked")
		}
	default:
		if seen[store.OperatorDiagnosticNoopBlockerExecutionUnknown] || seen[store.OperatorDiagnosticNoopBlockerExecutionDisabled] {
			return errors.New("operator client: enabled execution state is blocked")
		}
	}
	return nil
}
