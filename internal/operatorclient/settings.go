package operatorclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/settingpolicy"
	"github.com/teddashh/AI-Intune/internal/store"
)

// The wire bodies are declared here rather than shared with the Hub package so
// that a field renamed on one side becomes a compile-or-decode failure instead
// of a silently dropped value.

type SettingPolicyPreviewRequest struct {
	PolicyID                   string `json:"policy_id"`
	CheckinIntervalSeconds     int    `json:"checkin_interval_seconds"`
	ObservationIntervalSeconds int    `json:"observation_interval_seconds"`
}

type SettingPolicyPublishRequest struct {
	PolicyID                   string `json:"policy_id"`
	CheckinIntervalSeconds     int    `json:"checkin_interval_seconds"`
	ObservationIntervalSeconds int    `json:"observation_interval_seconds"`
	ExpectedRevision           *int64 `json:"expected_revision"`
	PreviewDigest              string `json:"preview_digest"`
	ConfirmPolicyID            string `json:"confirm_policy_id"`
	Reason                     string `json:"reason"`
}

type SettingAssignmentPreviewRequest struct {
	Scope    string `json:"scope"`
	ScopeID  string `json:"scope_id"`
	PolicyID string `json:"policy_id"`
	Revision int64  `json:"policy_revision"`
}

type SettingAssignmentRequest struct {
	Scope          string `json:"scope"`
	ScopeID        string `json:"scope_id"`
	PolicyID       string `json:"policy_id"`
	Revision       int64  `json:"policy_revision"`
	PreviewDigest  string `json:"preview_digest"`
	ConfirmScopeID string `json:"confirm_scope_id"`
	Reason         string `json:"reason"`
}

func (c *Client) SettingBoard(ctx context.Context) (operator.SettingBoardResult, error) {
	var out operator.SettingBoardResult
	req, err := c.newOperatorRequest(ctx, http.MethodGet, "/v1/operator/settings", nil)
	if err != nil {
		return out, err
	}
	response, err := c.doRaw(req)
	if err != nil {
		return out, err
	}
	if response.status != http.StatusOK {
		return out, fmt.Errorf("operator client: setting board returned HTTP %d", response.status)
	}
	if err := validateSettingReadHeaders(response.header); err != nil {
		return out, err
	}
	if err := decodeStrictJSONDocument(response.body, "setting board", &out); err != nil {
		return out, err
	}
	if err := validateSettingBoard(out); err != nil {
		return out, err
	}
	return out, nil
}

func (c *Client) PreviewSettingPolicy(ctx context.Context, body SettingPolicyPreviewRequest) (operator.SettingPolicyPreviewResult, error) {
	var out operator.SettingPolicyPreviewResult
	if strings.TrimSpace(body.PolicyID) != body.PolicyID || body.PolicyID == "" {
		return out, errors.New("operator client: setting policy id cannot be blank or have leading or trailing whitespace")
	}
	response, err := c.postSettingJSON(ctx, "/v1/operator/setting-policies/preview", "", body)
	if err != nil {
		return out, err
	}
	if response.status != http.StatusOK {
		return out, fmt.Errorf("operator client: setting policy preview returned HTTP %d", response.status)
	}
	if err := validateSettingReadHeaders(response.header); err != nil {
		return out, err
	}
	if err := decodeStrictJSONDocument(response.body, "setting policy preview", &out); err != nil {
		return out, err
	}
	if out.PolicyID != body.PolicyID || !validSHA256Digest(out.Digest) ||
		!validSHA256Digest(out.PreviewDigest) || out.CurrentRev < 0 ||
		out.Unchanged != (out.NextRev == out.CurrentRev) {
		return out, errors.New("operator client: setting policy preview identity is inconsistent")
	}
	return out, nil
}

func (c *Client) PublishSettingPolicy(ctx context.Context, key string, body SettingPolicyPublishRequest) (store.OperatorSettingPolicyResult, error) {
	var out store.OperatorSettingPolicyResult
	if !validSettingIdempotencyKey(key) || body.ExpectedRevision == nil || *body.ExpectedRevision < 0 ||
		!validSHA256Digest(body.PreviewDigest) || body.ConfirmPolicyID == "" ||
		strings.TrimSpace(body.Reason) != body.Reason || body.Reason == "" {
		return out, errors.New("operator client: setting policy publish coordinates are invalid")
	}
	response, err := c.postSettingJSON(ctx, "/v1/operator/setting-policies", key, body)
	if err != nil {
		return out, err
	}
	replayed, err := validateSettingWriteHeaders(response)
	if err != nil {
		return out, err
	}
	if err := decodeStrictJSONDocument(response.body, "setting policy publish", &out); err != nil {
		return out, err
	}
	if out.Replayed != replayed {
		return out, errors.New("operator client: setting policy replay evidence mismatch")
	}
	// A fresh decision is 201; a replay or an unchanged republish is 200. Any
	// other pairing means the two sides disagree about what just happened.
	if fresh := response.status == http.StatusCreated; fresh == (out.Replayed || out.Unchanged) {
		return out, fmt.Errorf("operator client: setting policy publish HTTP %d does not match result", response.status)
	}
	if out.PolicyID != body.PolicyID || out.Revision <= 0 || !validSHA256Digest(out.Digest) {
		return out, errors.New("operator client: setting policy publish identity is inconsistent")
	}
	return out, nil
}

func (c *Client) PreviewSettingAssignment(ctx context.Context, body SettingAssignmentPreviewRequest) (operator.SettingAssignmentPreviewResult, error) {
	var out operator.SettingAssignmentPreviewResult
	if body.Revision <= 0 || body.ScopeID == "" || body.PolicyID == "" {
		return out, errors.New("operator client: setting assignment preview coordinates are invalid")
	}
	response, err := c.postSettingJSON(ctx, "/v1/operator/setting-assignments/preview", "", body)
	if err != nil {
		return out, err
	}
	if response.status != http.StatusOK {
		return out, fmt.Errorf("operator client: setting assignment preview returned HTTP %d", response.status)
	}
	if err := validateSettingReadHeaders(response.header); err != nil {
		return out, err
	}
	if err := decodeStrictJSONDocument(response.body, "setting assignment preview", &out); err != nil {
		return out, err
	}
	if string(out.Scope) != body.Scope || out.ScopeID != body.ScopeID || out.Revision != body.Revision ||
		!validSHA256Digest(out.Digest) || !validSHA256Digest(out.PreviewDigest) {
		return out, errors.New("operator client: setting assignment preview identity is inconsistent")
	}
	return out, nil
}

func (c *Client) AssignSettingPolicy(ctx context.Context, key string, body SettingAssignmentRequest) (store.OperatorSettingAssignmentResult, error) {
	var out store.OperatorSettingAssignmentResult
	if !validSettingIdempotencyKey(key) || !validSHA256Digest(body.PreviewDigest) ||
		body.ConfirmScopeID == "" || body.Revision <= 0 ||
		strings.TrimSpace(body.Reason) != body.Reason || body.Reason == "" {
		return out, errors.New("operator client: setting assignment coordinates are invalid")
	}
	response, err := c.postSettingJSON(ctx, "/v1/operator/setting-assignments", key, body)
	if err != nil {
		return out, err
	}
	replayed, err := validateSettingWriteHeaders(response)
	if err != nil {
		return out, err
	}
	if err := decodeStrictJSONDocument(response.body, "setting assignment", &out); err != nil {
		return out, err
	}
	if out.Replayed != replayed {
		return out, errors.New("operator client: setting assignment replay evidence mismatch")
	}
	if fresh := response.status == http.StatusCreated; fresh == (out.Replayed || out.Unchanged) {
		return out, fmt.Errorf("operator client: setting assignment HTTP %d does not match result", response.status)
	}
	if string(out.Scope) != body.Scope || out.ScopeID != body.ScopeID ||
		out.PolicyID != body.PolicyID || out.PolicyRev != body.Revision ||
		out.AssignmentID == "" || !validSHA256Digest(out.Digest) {
		return out, errors.New("operator client: setting assignment identity is inconsistent")
	}
	return out, nil
}

func (c *Client) postSettingJSON(ctx context.Context, path, key string, body any) (operatorRawResponse, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return operatorRawResponse{}, err
	}
	req, err := c.newOperatorRequest(ctx, http.MethodPost, path, bytes.NewReader(raw))
	if err != nil {
		return operatorRawResponse{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	return c.doRaw(req)
}

func validSettingIdempotencyKey(key string) bool {
	return key != "" && strings.TrimSpace(key) == key && len(key) <= 200
}

func validateSettingReadHeaders(header http.Header) error {
	if err := validateJSONNoStoreResponse(header); err != nil {
		return err
	}
	replayed, err := responseReplayEvidenceFromHeader(header)
	if err != nil {
		return err
	}
	if replayed {
		return errors.New("operator client: setting read or preview cannot be replayed")
	}
	return nil
}

func validateSettingWriteHeaders(response operatorRawResponse) (bool, error) {
	if response.status != http.StatusOK && response.status != http.StatusCreated {
		return false, fmt.Errorf("operator client: setting write returned HTTP %d", response.status)
	}
	if err := validateJSONNoStoreResponse(response.header); err != nil {
		return false, err
	}
	return responseReplayEvidenceFromHeader(response.header)
}

// validateSettingBoard keeps the verdict counts and the rows one fact. A board
// whose totals disagree with its rows would be read as a fleet fact.
func validateSettingBoard(board operator.SettingBoardResult) error {
	if err := board.Defaults.Validate(); err != nil {
		return fmt.Errorf("operator client: setting board defaults are invalid: %w", err)
	}
	counted := map[settingpolicy.Verdict]int{}
	for _, machine := range board.Machines {
		if machine.MachineID == "" || machine.VerdictLabel == "" {
			return errors.New("operator client: setting board row is missing identity")
		}
		if (machine.Source == settingpolicy.SourceDefault) != (machine.PolicyID == "") {
			return errors.New("operator client: setting board row source is inconsistent with policy")
		}
		counted[machine.Verdict]++
	}
	for verdict, n := range board.Counts {
		if counted[verdict] != n {
			return fmt.Errorf("operator client: setting board %s count does not match rows", verdict)
		}
	}
	for verdict := range counted {
		if _, ok := board.Counts[verdict]; !ok {
			return fmt.Errorf("operator client: setting board did not report count for %s", verdict)
		}
	}
	return nil
}
