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
	"strings"
	"time"

	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/store"
)

type RetentionPrunePreviewRequest struct {
	EvaluatedAt *time.Time                     `json:"evaluated_at,omitempty"`
	Policy      *store.OperatorRetentionPolicy `json:"policy,omitempty"`
}

type RetentionPruneApplyRequest struct {
	EvaluatedAt      time.Time                      `json:"evaluated_at"`
	Policy           *store.OperatorRetentionPolicy `json:"policy"`
	ExpectedRevision int64                          `json:"expected_revision"`
	Confirm          string                         `json:"confirm"`
	PreviewDigest    string                         `json:"preview_digest"`
	Reason           string                         `json:"reason"`
}

func (c *Client) RetentionStatus(ctx context.Context) (operator.RetentionStatus, error) {
	req, err := c.newOperatorRequest(ctx, http.MethodGet, "/v1/operator/maintenance/retention", nil)
	if err != nil {
		return operator.RetentionStatus{}, err
	}
	response, err := c.doRaw(req)
	if err != nil {
		return operator.RetentionStatus{}, err
	}
	if response.status != http.StatusOK {
		return operator.RetentionStatus{}, fmt.Errorf("operator client: retention status returned HTTP %d", response.status)
	}
	if err := validateRetentionHeaders(response.header, false); err != nil {
		return operator.RetentionStatus{}, err
	}
	var result operator.RetentionStatus
	if err := decodeStrictJSONDocument(response.body, "retention status", &result); err != nil {
		return result, err
	}
	if err := validateRetentionStatus(result); err != nil {
		return result, err
	}
	return result, nil
}

func (c *Client) PreviewRetentionPrune(ctx context.Context, body RetentionPrunePreviewRequest) (operator.RetentionPrunePreview, error) {
	if body.EvaluatedAt != nil && (body.EvaluatedAt.IsZero() || !body.EvaluatedAt.Equal(body.EvaluatedAt.UTC().Truncate(time.Second))) {
		return operator.RetentionPrunePreview{}, errors.New("operator client: retention evaluated_at must be UTC second precision")
	}
	if body.Policy != nil {
		if _, err := body.Policy.RetentionPolicy(); err != nil {
			return operator.RetentionPrunePreview{}, fmt.Errorf("operator client: invalid retention policy: %w", err)
		}
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return operator.RetentionPrunePreview{}, err
	}
	req, err := c.newOperatorRequest(ctx, http.MethodPost, "/v1/operator/maintenance/retention/prune-preview", bytes.NewReader(raw))
	if err != nil {
		return operator.RetentionPrunePreview{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	response, err := c.doRaw(req)
	if err != nil {
		return operator.RetentionPrunePreview{}, err
	}
	if response.status != http.StatusOK {
		return operator.RetentionPrunePreview{}, fmt.Errorf("operator client: retention preview returned HTTP %d", response.status)
	}
	if err := validateRetentionHeaders(response.header, false); err != nil {
		return operator.RetentionPrunePreview{}, err
	}
	result, err := decodeRetentionPreview(response.body)
	if err != nil {
		return result, err
	}
	if err := validateRetentionPreview(result, body); err != nil {
		return result, err
	}
	return result, nil
}

func (c *Client) ApplyRetentionPrune(ctx context.Context, key string, body RetentionPruneApplyRequest) (store.OperatorPruneResult, error) {
	if strings.TrimSpace(key) != key || key == "" || len(key) > 200 || body.Policy == nil ||
		body.ExpectedRevision < 0 || body.EvaluatedAt.IsZero() ||
		!body.EvaluatedAt.Equal(body.EvaluatedAt.UTC().Truncate(time.Second)) ||
		!validSHA256Digest(body.PreviewDigest) || body.Confirm == "" ||
		!validTailnetReason(body.Reason) {
		return store.OperatorPruneResult{}, errors.New("operator client: retention apply coordinates are invalid")
	}
	if _, err := body.Policy.RetentionPolicy(); err != nil {
		return store.OperatorPruneResult{}, fmt.Errorf("operator client: invalid retention policy: %w", err)
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return store.OperatorPruneResult{}, err
	}
	req, err := c.newOperatorRequest(ctx, http.MethodPost, "/v1/operator/maintenance/retention/prunes", bytes.NewReader(raw))
	if err != nil {
		return store.OperatorPruneResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", key)
	response, err := c.doRaw(req)
	if err != nil {
		return store.OperatorPruneResult{}, err
	}
	if response.status != http.StatusOK {
		return store.OperatorPruneResult{}, fmt.Errorf("operator client: retention apply returned HTTP %d", response.status)
	}
	replayed, err := responseReplayEvidenceFromHeader(response.header)
	if err != nil {
		return store.OperatorPruneResult{}, err
	}
	if err := validateRetentionHeaders(response.header, replayed); err != nil {
		return store.OperatorPruneResult{}, err
	}
	result, err := decodeRetentionResult(response.body)
	if err != nil {
		return result, err
	}
	if result.Replayed != replayed {
		return result, errors.New("operator client: retention replay evidence mismatch")
	}
	if err := validateRetentionResult(result, body); err != nil {
		return result, err
	}
	return result, nil
}

func validateRetentionHeaders(header http.Header, replayAllowed bool) error {
	if err := validateJSONNoStoreResponse(header); err != nil {
		return err
	}
	replayed, err := responseReplayEvidenceFromHeader(header)
	if err != nil {
		return err
	}
	if replayed && !replayAllowed {
		return errors.New("operator client: retention read or preview cannot be replayed")
	}
	return nil
}

func validateRetentionStatus(result operator.RetentionStatus) error {
	if result.SchemaVersion != 1 || !canonicalRetentionResponseTime(result.EvaluatedAt) ||
		result.Revision < 0 || result.LastPruneRows < 0 {
		return errors.New("operator client: invalid retention status identity")
	}
	if _, err := result.Policy.RetentionPolicy(); err != nil {
		return errors.New("operator client: retention status has invalid policy")
	}
	if result.HasRun != (result.LastPruneAt != nil) || (!result.HasRun && result.LastPruneRows != 0) {
		return errors.New("operator client: retention status has contradictory run evidence")
	}
	if result.LastPruneAt != nil && !canonicalRetentionResponseTime(*result.LastPruneAt) {
		return errors.New("operator client: retention status has invalid last_prune_at")
	}
	return nil
}

func decodeRetentionPreview(raw []byte) (operator.RetentionPrunePreview, error) {
	var result operator.RetentionPrunePreview
	if err := decodeStrictJSONDocument(raw, "retention preview", &result); err != nil {
		return result, err
	}
	return result, nil
}

func validateRetentionPreview(result operator.RetentionPrunePreview, request RetentionPrunePreviewRequest) error {
	if result.SchemaVersion != 1 || !canonicalRetentionResponseTime(result.EvaluatedAt) || result.ExpectedRevision < 0 ||
		result.TotalDeleted < 0 || result.KeptNewest < 0 || !validSHA256Digest(result.PreviewDigest) ||
		result.Confirmation != fmt.Sprintf("DELETE %d ROWS", result.TotalDeleted) {
		return errors.New("operator client: invalid retention preview identity")
	}
	if request.EvaluatedAt != nil && !result.EvaluatedAt.Equal(request.EvaluatedAt.UTC()) {
		return errors.New("operator client: retention preview evaluated_at mismatch")
	}
	if request.Policy != nil && result.Policy != *request.Policy {
		return errors.New("operator client: retention preview policy mismatch")
	}
	if _, err := result.Policy.RetentionPolicy(); err != nil {
		return errors.New("operator client: retention preview has invalid policy")
	}
	if err := validateRetentionCounts(result.Counts, result.EvaluatedAt, result.Policy, result.TotalDeleted, result.KeptNewest); err != nil {
		return err
	}
	copy := result
	copy.PreviewDigest = ""
	raw, _ := json.Marshal(copy)
	if retentionDigest(raw) != result.PreviewDigest {
		return errors.New("operator client: retention preview digest mismatch")
	}
	return nil
}

func retentionDigest(raw []byte) string {
	digest := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func decodeRetentionResult(raw []byte) (store.OperatorPruneResult, error) {
	var result store.OperatorPruneResult
	if err := decodeStrictJSONDocument(raw, "retention apply", &result); err != nil {
		return result, err
	}
	return result, nil
}

func validateRetentionResult(result store.OperatorPruneResult, request RetentionPruneApplyRequest) error {
	if result.EvaluatedAt != request.EvaluatedAt.UTC() || result.Policy != *request.Policy ||
		!canonicalRetentionResponseTime(result.EvaluatedAt) || !canonicalRetentionResponseTime(result.AppliedAt) ||
		result.AppliedAt.Before(result.EvaluatedAt) || result.TotalDeleted <= 0 || result.Revision <= request.ExpectedRevision {
		return errors.New("operator client: retention apply response does not match request")
	}
	return validateRetentionCounts(result.Counts, result.EvaluatedAt, result.Policy,
		result.TotalDeleted, result.KeptNewest)
}

func canonicalRetentionResponseTime(value time.Time) bool {
	_, offset := value.Zone()
	return !value.IsZero() && value.Nanosecond() == 0 && offset == 0
}

func validateRetentionCounts(counts []store.PruneCount, evaluatedAt time.Time,
	policy store.OperatorRetentionPolicy, total, kept int64,
) error {
	if counts == nil || len(counts) != 5 || total < 0 || kept < 0 {
		return errors.New("operator client: retention counts are incomplete")
	}
	want := []struct {
		name    string
		seconds int64
	}{
		{"observed_state", policy.ObservationsSeconds},
		{"machine_checkins", policy.CheckinsSeconds},
		{"ticket_occupancy_observation", policy.OccupancySeconds},
		{"canary_silent_failures", policy.ObservationsSeconds},
		{"notifications", policy.ObservationsSeconds},
	}
	var gotTotal, gotKept int64
	for i, item := range counts {
		if item.Table != want[i].name || item.Deleted < 0 || item.Kept < 0 ||
			!item.Older.Equal(evaluatedAt.Add(-time.Duration(want[i].seconds)*time.Second)) {
			return errors.New("operator client: retention count boundary is invalid")
		}
		gotTotal += item.Deleted
		gotKept += item.Kept
	}
	if gotTotal != total || gotKept != kept {
		return errors.New("operator client: retention count totals are inconsistent")
	}
	return nil
}
