package operatorclient

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/teddashh/AI-Intune/internal/maintenance"
	"github.com/teddashh/AI-Intune/internal/store"
)

// Wire bodies live here, not in the Hub package. A renamed field fails at
// decode instead of being dropped.

// DiskCleanProfilePreviewRequest publishes nothing. Profile is the closed document.
type DiskCleanProfilePreviewRequest struct {
	ScopeType string              `json:"scope_type"`
	ScopeID   string              `json:"scope_id"`
	Profile   maintenance.Profile `json:"profile"`
}

// DiskCleanProfilePublishRequest is the apply body. The idempotency key is a header.
type DiskCleanProfilePublishRequest struct {
	ScopeType        string              `json:"scope_type"`
	ScopeID          string              `json:"scope_id"`
	Profile          maintenance.Profile `json:"profile"`
	ExpectedRevision *int64              `json:"expected_revision"`
	PreviewDigest    string              `json:"preview_digest"`
	ConfirmScopeID   string              `json:"confirm_scope_id"`
	Reason           string              `json:"reason"`
}

// DiskCleanTargetPreviewRequest names the machines for a dry-run preview.
type DiskCleanTargetPreviewRequest struct {
	ScopeType  string   `json:"scope_type"`
	ScopeID    string   `json:"scope_id"`
	Revision   int64    `json:"revision"`
	MachineIDs []string `json:"machine_ids"`
}

// DiskCleanTargetApplyRequest is the dry-run apply body.
type DiskCleanTargetApplyRequest struct {
	ScopeType      string   `json:"scope_type"`
	ScopeID        string   `json:"scope_id"`
	Revision       int64    `json:"revision"`
	MachineIDs     []string `json:"machine_ids"`
	PreviewDigest  string   `json:"preview_digest"`
	ConfirmScopeID string   `json:"confirm_scope_id"`
	Reason         string   `json:"reason"`
}

// DiskCleanCanaryPreviewRequest names the one canary and the full target list.
type DiskCleanCanaryPreviewRequest struct {
	ScopeType       string   `json:"scope_type"`
	ScopeID         string   `json:"scope_id"`
	Revision        int64    `json:"revision"`
	MachineIDs      []string `json:"machine_ids"`
	CanaryMachineID string   `json:"canary_machine_id"`
}

// DiskCleanCanaryApplyRequest is the canary apply body.
type DiskCleanCanaryApplyRequest struct {
	ScopeType       string   `json:"scope_type"`
	ScopeID         string   `json:"scope_id"`
	Revision        int64    `json:"revision"`
	MachineIDs      []string `json:"machine_ids"`
	CanaryMachineID string   `json:"canary_machine_id"`
	PreviewDigest   string   `json:"preview_digest"`
	ConfirmScopeID  string   `json:"confirm_scope_id"`
	Reason          string   `json:"reason"`
}

// DiskCleanControlPreviewRequest names the rollout to continue or abandon.
type DiskCleanControlPreviewRequest struct {
	RolloutID string `json:"rollout_id"`
}

// DiskCleanControlApplyRequest continues or abandons one rollout.
type DiskCleanControlApplyRequest struct {
	RolloutID               string `json:"rollout_id"`
	ExpectedControlRevision int64  `json:"expected_control_revision"`
	ExpectedOpenedBatch     int    `json:"expected_opened_batch"`
	PreviewDigest           string `json:"preview_digest"`
	ConfirmRolloutID        string `json:"confirm_rollout_id"`
	Reason                  string `json:"reason"`
}

type diskCleanSummaryList struct {
	Items []store.DiskCleanSummaryView `json:"items"`
}

// DiskCleanSummaries reads the latest disk-clean board.
func (c *Client) DiskCleanSummaries(ctx context.Context) ([]store.DiskCleanSummaryView, error) {
	var out diskCleanSummaryList
	req, err := c.newOperatorRequest(ctx, http.MethodGet, "/v1/operator/disk-clean/summaries", nil)
	if err != nil {
		return nil, err
	}
	response, err := c.doRaw(req)
	if err != nil {
		return nil, err
	}
	if response.status != http.StatusOK {
		return nil, fmt.Errorf("operator client: disk-clean summaries returned HTTP %d", response.status)
	}
	if err := validateSettingReadHeaders(response.header); err != nil {
		return nil, err
	}
	if err := decodeStrictJSONDocument(response.body, "disk-clean summaries", &out); err != nil {
		return nil, err
	}
	if out.Items == nil {
		return nil, errors.New("operator client: disk-clean summaries items is null")
	}
	return out.Items, nil
}

// DiskCleanSummary reads one machine.
func (c *Client) DiskCleanSummary(ctx context.Context, machineID string) (store.DiskCleanSummaryView, error) {
	var out store.DiskCleanSummaryView
	if err := requireDiskCleanID("machine_id", machineID); err != nil {
		return out, err
	}
	req, err := c.newOperatorRequest(ctx, http.MethodGet, "/v1/operator/disk-clean/summaries/"+url.PathEscape(machineID), nil)
	if err != nil {
		return out, err
	}
	response, err := c.doRaw(req)
	if err != nil {
		return out, err
	}
	if response.status != http.StatusOK {
		return out, fmt.Errorf("operator client: disk-clean summary returned HTTP %d", response.status)
	}
	if err := validateSettingReadHeaders(response.header); err != nil {
		return out, err
	}
	if err := decodeStrictJSONDocument(response.body, "disk-clean summary", &out); err != nil {
		return out, err
	}
	if out.MachineID != machineID || out.Verdict == "" {
		return out, errors.New("operator client: disk-clean summary identity mismatch")
	}
	return out, nil
}

// PreviewDiskCleanProfile renders a revision without writing it.
func (c *Client) PreviewDiskCleanProfile(ctx context.Context, body DiskCleanProfilePreviewRequest) (store.DiskCleanProfilePreview, error) {
	var out store.DiskCleanProfilePreview
	if err := requireDiskCleanScope(body.ScopeType, body.ScopeID); err != nil || body.Profile.SchemaVersion != maintenance.SchemaVersion {
		return out, errors.New("operator client: disk-clean profile preview coordinates are invalid")
	}
	response, err := c.postSettingJSON(ctx, "/v1/operator/disk-clean/profile-preview", "", body)
	if err != nil {
		return out, err
	}
	if response.status != http.StatusOK {
		return out, fmt.Errorf("operator client: disk-clean profile preview returned HTTP %d", response.status)
	}
	if err := validateSettingReadHeaders(response.header); err != nil {
		return out, err
	}
	if err := decodeStrictJSONDocument(response.body, "disk-clean profile preview", &out); err != nil {
		return out, err
	}
	if out.ScopeType != body.ScopeType || out.ScopeID != body.ScopeID ||
		!validSHA256Digest(out.ConfigDigest) || !validSHA256Digest(out.PreviewDigest) || out.Conf == "" {
		return out, errors.New("operator client: disk-clean profile preview identity mismatch")
	}
	return out, nil
}

// PublishDiskCleanProfile applies one profile revision.
func (c *Client) PublishDiskCleanProfile(ctx context.Context, key string, body DiskCleanProfilePublishRequest) (store.DiskCleanProfileResult, error) {
	var out store.DiskCleanProfileResult
	if !validSettingIdempotencyKey(key) || body.ExpectedRevision == nil || *body.ExpectedRevision < 0 ||
		!validSHA256Digest(body.PreviewDigest) || body.ConfirmScopeID != body.ScopeID ||
		!diskCleanReasonOK(body.Reason) || body.Profile.SchemaVersion != maintenance.SchemaVersion {
		return out, errors.New("operator client: disk-clean profile publish coordinates are invalid")
	}
	if err := requireDiskCleanScope(body.ScopeType, body.ScopeID); err != nil {
		return out, err
	}
	response, err := c.postSettingJSON(ctx, "/v1/operator/disk-clean/profiles", key, body)
	if err != nil {
		return out, err
	}
	replayed, err := validateSettingWriteHeaders(response)
	if err != nil {
		return out, err
	}
	if err := decodeStrictJSONDocument(response.body, "disk-clean profile publish", &out); err != nil {
		return out, err
	}
	if out.Replayed != replayed {
		return out, errors.New("operator client: disk-clean profile replay evidence mismatch")
	}
	if fresh := response.status == http.StatusCreated; fresh == (out.Replayed || out.Unchanged) {
		return out, fmt.Errorf("operator client: disk-clean profile publish HTTP %d does not match the result", response.status)
	}
	if out.ScopeType != body.ScopeType || out.ScopeID != body.ScopeID || out.Revision <= 0 || !validSHA256Digest(out.ConfigDigest) {
		return out, errors.New("operator client: disk-clean profile publish identity mismatch")
	}
	return out, nil
}

// PreviewDiskCleanDryRun plans reversible jobs.
func (c *Client) PreviewDiskCleanDryRun(ctx context.Context, body DiskCleanTargetPreviewRequest) (store.DiskCleanDryRunPreview, error) {
	var out store.DiskCleanDryRunPreview
	if err := requireDiskCleanTargets(body.ScopeType, body.ScopeID, body.Revision, body.MachineIDs); err != nil {
		return out, err
	}
	response, err := c.postSettingJSON(ctx, "/v1/operator/disk-clean/dry-run-preview", "", body)
	if err != nil {
		return out, err
	}
	if err := decodeDiskCleanPreview(response, "disk-clean dry-run preview", &out); err != nil {
		return out, err
	}
	if out.ScopeType != body.ScopeType || out.ScopeID != body.ScopeID || out.Revision != body.Revision ||
		!validSHA256Digest(out.PreviewDigest) || !validSHA256Digest(out.ConfigDigest) {
		return out, errors.New("operator client: disk-clean dry-run preview identity mismatch")
	}
	return out, nil
}

// ApplyDiskCleanDryRun creates the reversible jobs.
func (c *Client) ApplyDiskCleanDryRun(ctx context.Context, key string, body DiskCleanTargetApplyRequest) (store.DiskCleanDryRunResult, error) {
	var out store.DiskCleanDryRunResult
	if err := requireDiskCleanTargetApply(key, body.ScopeType, body.ScopeID, body.Revision, body.MachineIDs, body.PreviewDigest, body.ConfirmScopeID, body.Reason); err != nil {
		return out, err
	}
	response, err := c.postSettingJSON(ctx, "/v1/operator/disk-clean/dry-runs", key, body)
	if err != nil {
		return out, err
	}
	if err := decodeDiskCleanWrite(response, "disk-clean dry-run", &out, &out.Replayed); err != nil {
		return out, err
	}
	if out.Revision != body.Revision || !validSHA256Digest(out.ConfigDigest) || len(out.JobIDs) == 0 {
		return out, errors.New("operator client: disk-clean dry-run identity mismatch")
	}
	return out, nil
}

// PreviewDiskCleanCanary plans a one-machine first batch.
func (c *Client) PreviewDiskCleanCanary(ctx context.Context, body DiskCleanCanaryPreviewRequest) (store.DiskCleanCanaryPreview, error) {
	var out store.DiskCleanCanaryPreview
	if err := requireDiskCleanTargets(body.ScopeType, body.ScopeID, body.Revision, body.MachineIDs); err != nil ||
		!diskCleanContains(body.MachineIDs, body.CanaryMachineID) {
		return out, errors.New("operator client: disk-clean canary preview coordinates are invalid")
	}
	response, err := c.postSettingJSON(ctx, "/v1/operator/disk-clean/canary-preview", "", body)
	if err != nil {
		return out, err
	}
	if err := decodeDiskCleanPreview(response, "disk-clean canary preview", &out); err != nil {
		return out, err
	}
	if out.CanaryMachineID != body.CanaryMachineID || out.Revision != body.Revision || !validSHA256Digest(out.PreviewDigest) {
		return out, errors.New("operator client: disk-clean canary preview identity mismatch")
	}
	return out, nil
}

// ApplyDiskCleanCanary opens batch 1 only.
func (c *Client) ApplyDiskCleanCanary(ctx context.Context, key string, body DiskCleanCanaryApplyRequest) (store.DiskCleanCanaryResult, error) {
	var out store.DiskCleanCanaryResult
	if err := requireDiskCleanTargetApply(key, body.ScopeType, body.ScopeID, body.Revision, body.MachineIDs, body.PreviewDigest, body.ConfirmScopeID, body.Reason); err != nil ||
		!diskCleanContains(body.MachineIDs, body.CanaryMachineID) {
		return out, errors.New("operator client: disk-clean canary apply coordinates are invalid")
	}
	response, err := c.postSettingJSON(ctx, "/v1/operator/disk-clean/canaries", key, body)
	if err != nil {
		return out, err
	}
	if err := decodeDiskCleanWrite(response, "disk-clean canary", &out, &out.Replayed); err != nil {
		return out, err
	}
	if out.CanaryMachineID != body.CanaryMachineID || out.OpenedBatch != 1 || out.RolloutID == "" || out.CanaryJobID == "" {
		return out, errors.New("operator client: disk-clean canary identity mismatch")
	}
	return out, nil
}

// PreviewDiskCleanContinue reads the rollout the operator would continue.
func (c *Client) PreviewDiskCleanContinue(ctx context.Context, rolloutID string) (store.DiskCleanControlPreview, error) {
	return c.previewDiskCleanControl(ctx, "/v1/operator/disk-clean/continuation-preview", rolloutID)
}

// PreviewDiskCleanAbandon reads the rollout the operator would abandon.
func (c *Client) PreviewDiskCleanAbandon(ctx context.Context, rolloutID string) (store.DiskCleanControlPreview, error) {
	return c.previewDiskCleanControl(ctx, "/v1/operator/disk-clean/abandonment-preview", rolloutID)
}

func (c *Client) previewDiskCleanControl(ctx context.Context, path, rolloutID string) (store.DiskCleanControlPreview, error) {
	var out store.DiskCleanControlPreview
	if err := requireDiskCleanID("rollout_id", rolloutID); err != nil {
		return out, err
	}
	response, err := c.postSettingJSON(ctx, path, "", DiskCleanControlPreviewRequest{RolloutID: rolloutID})
	if err != nil {
		return out, err
	}
	if err := decodeDiskCleanPreview(response, "disk-clean control preview", &out); err != nil {
		return out, err
	}
	if out.RolloutID != rolloutID || out.State == "" || !validSHA256Digest(out.PreviewDigest) || out.ControlRevision <= 0 {
		return out, errors.New("operator client: disk-clean control preview identity mismatch")
	}
	return out, nil
}

// ContinueDiskClean opens the rest of a paused rollout.
func (c *Client) ContinueDiskClean(ctx context.Context, key string, body DiskCleanControlApplyRequest) (store.DiskCleanRolloutResult, error) {
	return c.applyDiskCleanControl(ctx, "/v1/operator/disk-clean/continuations", key, body)
}

// AbandonDiskClean stops a rollout without opening more jobs.
func (c *Client) AbandonDiskClean(ctx context.Context, key string, body DiskCleanControlApplyRequest) (store.DiskCleanRolloutResult, error) {
	return c.applyDiskCleanControl(ctx, "/v1/operator/disk-clean/abandonments", key, body)
}

func (c *Client) applyDiskCleanControl(ctx context.Context, path, key string, body DiskCleanControlApplyRequest) (store.DiskCleanRolloutResult, error) {
	var out store.DiskCleanRolloutResult
	if !validSettingIdempotencyKey(key) || !validSHA256Digest(body.PreviewDigest) ||
		body.ExpectedControlRevision <= 0 || body.ExpectedOpenedBatch <= 0 ||
		body.ConfirmRolloutID != body.RolloutID || !diskCleanReasonOK(body.Reason) {
		return out, errors.New("operator client: disk-clean control apply coordinates are invalid")
	}
	if err := requireDiskCleanID("rollout_id", body.RolloutID); err != nil {
		return out, err
	}
	response, err := c.postSettingJSON(ctx, path, key, body)
	if err != nil {
		return out, err
	}
	if err := decodeDiskCleanWrite(response, "disk-clean control", &out, &out.Replayed); err != nil {
		return out, err
	}
	if out.RolloutID != body.RolloutID || out.State == "" {
		return out, errors.New("operator client: disk-clean control identity mismatch")
	}
	return out, nil
}

func decodeDiskCleanPreview(response operatorRawResponse, label string, dst any) error {
	if response.status != http.StatusOK {
		return fmt.Errorf("operator client: %s returned HTTP %d", label, response.status)
	}
	if err := validateSettingReadHeaders(response.header); err != nil {
		return err
	}
	return decodeStrictJSONDocument(response.body, label, dst)
}

func decodeDiskCleanWrite(response operatorRawResponse, label string, dst any, replayed *bool) error {
	headerReplayed, err := validateSettingWriteHeaders(response)
	if err != nil {
		return err
	}
	if err := decodeStrictJSONDocument(response.body, label, dst); err != nil {
		return err
	}
	if *replayed != headerReplayed {
		return fmt.Errorf("operator client: %s replay evidence mismatch", label)
	}
	if fresh := response.status == http.StatusCreated; fresh == *replayed {
		return fmt.Errorf("operator client: %s HTTP %d does not match the result", label, response.status)
	}
	return nil
}

func requireDiskCleanScope(scopeType, scopeID string) error {
	if scopeType != "machine" && scopeType != "channel" {
		return errors.New("operator client: disk-clean scope_type must be machine or channel")
	}
	return requireDiskCleanID("scope_id", scopeID)
}

func requireDiskCleanTargets(scopeType, scopeID string, revision int64, machineIDs []string) error {
	if err := requireDiskCleanScope(scopeType, scopeID); err != nil || revision <= 0 || len(machineIDs) == 0 || len(machineIDs) > 64 {
		return errors.New("operator client: disk-clean target coordinates are invalid")
	}
	seen := map[string]bool{}
	for _, id := range machineIDs {
		if err := requireDiskCleanID("machine_id", id); err != nil || seen[id] {
			return errors.New("operator client: disk-clean machine id is invalid")
		}
		seen[id] = true
	}
	return nil
}

func requireDiskCleanTargetApply(key, scopeType, scopeID string, revision int64, machineIDs []string, digest, confirm, reason string) error {
	if !validSettingIdempotencyKey(key) || !validSHA256Digest(digest) || confirm != scopeID || !diskCleanReasonOK(reason) {
		return errors.New("operator client: disk-clean target apply coordinates are invalid")
	}
	return requireDiskCleanTargets(scopeType, scopeID, revision, machineIDs)
}

func requireDiskCleanID(name, value string) error {
	if value == "" || strings.TrimSpace(value) != value || strings.Contains(value, "/") || value == "." || value == ".." {
		return fmt.Errorf("operator client: disk-clean %s is invalid", name)
	}
	return nil
}

func diskCleanReasonOK(reason string) bool {
	return reason != "" && strings.TrimSpace(reason) == reason
}

func diskCleanContains(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}
