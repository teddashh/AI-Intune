package operatorclient

import (
	"bytes"
	"context"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/teddashh/AI-Intune/internal/artifact"
	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/store"
)

// PreviewArtifactFetch asks the Hub to pin one exact package version to the
// registry metadata that a later create request must confirm.
func (c *Client) PreviewArtifactFetch(ctx context.Context,
	body operator.ArtifactFetchPreviewRequest,
) (operator.ArtifactFetchPreviewResult, error) {
	if err := validateArtifactFetchTarget(body.Name, body.Version); err != nil {
		return operator.ArtifactFetchPreviewResult{}, err
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return operator.ArtifactFetchPreviewResult{}, fmt.Errorf("operator client: encode artifact fetch preview: %w", err)
	}
	req, err := c.newOperatorRequest(ctx, http.MethodPost,
		"/v1/operator/artifact-fetches/preview", bytes.NewReader(raw))
	if err != nil {
		return operator.ArtifactFetchPreviewResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	response, err := c.doRaw(req)
	if err != nil {
		return operator.ArtifactFetchPreviewResult{}, err
	}
	if response.status != http.StatusOK {
		return operator.ArtifactFetchPreviewResult{}, fmt.Errorf(
			"operator client: artifact fetch preview returned HTTP %d", response.status)
	}
	if _, err := validateArtifactFetchResponseHeaders(response.header, false); err != nil {
		return operator.ArtifactFetchPreviewResult{}, err
	}
	var result operator.ArtifactFetchPreviewResult
	if err := decodeStrictJSONDocument(response.body, "artifact fetch preview", &result); err != nil {
		return operator.ArtifactFetchPreviewResult{}, err
	}
	if err := validateArtifactFetchPreviewResult(result, body); err != nil {
		return operator.ArtifactFetchPreviewResult{}, err
	}
	return result, nil
}

// CreateArtifactFetch durably enqueues a fetch. Fresh and replayed success are
// both HTTP 202; Idempotency-Replayed and the response body must agree.
func (c *Client) CreateArtifactFetch(ctx context.Context, idempotencyKey string,
	body operator.ArtifactFetchApplyRequest,
) (operator.ArtifactFetchApplyResult, error) {
	if err := validateDeploymentActionIdempotencyKey(idempotencyKey); err != nil {
		return operator.ArtifactFetchApplyResult{}, err
	}
	if body.IdempotencyKey != "" && body.IdempotencyKey != idempotencyKey {
		return operator.ArtifactFetchApplyResult{}, errors.New(
			"operator client: artifact fetch body and header idempotency authorities differ")
	}
	if body.Actor != (operator.Actor{}) {
		return operator.ArtifactFetchApplyResult{}, errors.New(
			"operator client: artifact fetch HTTP request must not supply an in-process actor")
	}
	if err := validateArtifactFetchApplyRequest(body); err != nil {
		return operator.ArtifactFetchApplyResult{}, err
	}
	// Marshal an explicit wire allowlist. Actor and the convenience copy of the
	// idempotency key must never become body authority even if their tags change.
	wireBody := struct {
		Name           string `json:"name"`
		Version        string `json:"version"`
		PreviewDigest  string `json:"preview_digest"`
		ConfirmName    string `json:"confirm_name"`
		ConfirmVersion string `json:"confirm_version"`
		Reason         string `json:"reason"`
	}{
		Name: body.Name, Version: body.Version, PreviewDigest: body.PreviewDigest,
		ConfirmName: body.ConfirmName, ConfirmVersion: body.ConfirmVersion, Reason: body.Reason,
	}
	raw, err := json.Marshal(wireBody)
	if err != nil {
		return operator.ArtifactFetchApplyResult{}, fmt.Errorf("operator client: encode artifact fetch create: %w", err)
	}
	req, err := c.newOperatorRequest(ctx, http.MethodPost,
		"/v1/operator/artifact-fetches", bytes.NewReader(raw))
	if err != nil {
		return operator.ArtifactFetchApplyResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", idempotencyKey)
	response, err := c.doRaw(req)
	if err != nil {
		return operator.ArtifactFetchApplyResult{}, err
	}
	if response.status != http.StatusAccepted {
		return operator.ArtifactFetchApplyResult{}, fmt.Errorf(
			"operator client: artifact fetch create returned HTTP %d", response.status)
	}
	replayed, err := validateArtifactFetchResponseHeaders(response.header, true)
	if err != nil {
		return operator.ArtifactFetchApplyResult{}, err
	}
	var result operator.ArtifactFetchApplyResult
	if err := decodeStrictJSONDocument(response.body, "artifact fetch create", &result); err != nil {
		return operator.ArtifactFetchApplyResult{}, err
	}
	if result.Replayed != replayed {
		return operator.ArtifactFetchApplyResult{}, errors.New(
			"operator client: artifact fetch replay evidence differs between header and body")
	}
	if err := validateArtifactFetchOperation(result.Operation); err != nil {
		return operator.ArtifactFetchApplyResult{}, err
	}
	if result.Operation.Name != body.Name || result.Operation.Version != body.Version ||
		result.Operation.PreviewDigest != body.PreviewDigest {
		return operator.ArtifactFetchApplyResult{}, errors.New(
			"operator client: artifact fetch operation does not match the confirmed request")
	}
	if !replayed && (result.Operation.State != store.ArtifactFetchQueued ||
		result.Operation.Phase != store.ArtifactFetchPhaseQueued) {
		return operator.ArtifactFetchApplyResult{}, errors.New(
			"operator client: fresh artifact fetch did not return a queued operation")
	}
	return result, nil
}

func (c *Client) ArtifactFetches(ctx context.Context,
	request store.ArtifactFetchListRequest,
) (store.ArtifactFetchListResult, error) {
	query, effectiveLimit, err := encodeArtifactFetchListQuery(request)
	if err != nil {
		return store.ArtifactFetchListResult{}, err
	}
	path := "/v1/operator/artifact-fetches"
	if encoded := query.Encode(); encoded != "" {
		path += "?" + encoded
	}
	req, err := c.newOperatorRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return store.ArtifactFetchListResult{}, err
	}
	response, err := c.doRaw(req)
	if err != nil {
		return store.ArtifactFetchListResult{}, err
	}
	if response.status != http.StatusOK {
		return store.ArtifactFetchListResult{}, fmt.Errorf(
			"operator client: artifact fetch list returned HTTP %d", response.status)
	}
	if _, err := validateArtifactFetchResponseHeaders(response.header, false); err != nil {
		return store.ArtifactFetchListResult{}, err
	}
	var result store.ArtifactFetchListResult
	if err := decodeStrictJSONDocument(response.body, "artifact fetch list", &result); err != nil {
		return store.ArtifactFetchListResult{}, err
	}
	if err := validateArtifactFetchListResult(result, request, effectiveLimit); err != nil {
		return store.ArtifactFetchListResult{}, err
	}
	return result, nil
}

func (c *Client) ArtifactFetch(ctx context.Context, operationID string) (store.ArtifactFetchOperation, error) {
	if err := validateArtifactFetchIdentifier("operation_id", operationID, 128); err != nil {
		return store.ArtifactFetchOperation{}, err
	}
	req, err := c.newOperatorRequest(ctx, http.MethodGet,
		"/v1/operator/artifact-fetches/"+url.PathEscape(operationID), nil)
	if err != nil {
		return store.ArtifactFetchOperation{}, err
	}
	response, err := c.doRaw(req)
	if err != nil {
		return store.ArtifactFetchOperation{}, err
	}
	if response.status != http.StatusOK {
		return store.ArtifactFetchOperation{}, fmt.Errorf(
			"operator client: artifact fetch detail returned HTTP %d", response.status)
	}
	if _, err := validateArtifactFetchResponseHeaders(response.header, false); err != nil {
		return store.ArtifactFetchOperation{}, err
	}
	var result store.ArtifactFetchOperation
	if err := decodeStrictJSONDocument(response.body, "artifact fetch detail", &result); err != nil {
		return store.ArtifactFetchOperation{}, err
	}
	if err := validateArtifactFetchOperation(result); err != nil {
		return store.ArtifactFetchOperation{}, err
	}
	if result.OperationID != operationID {
		return store.ArtifactFetchOperation{}, errors.New(
			"operator client: artifact fetch detail operation_id does not match request path")
	}
	return result, nil
}

func validateArtifactFetchTarget(name, version string) error {
	valid := name == "openclaw" && artifact.ValidOpenClawVersion(version)
	if name == "node-runtime" {
		valid = artifact.ValidNodeRuntimeVersion(version)
	}
	if name == "hermes-agent" {
		valid = artifact.ValidHermesVersion(version)
	}
	if !valid {
		return errors.New("operator client: artifact fetch requires a supported package and exact version")
	}
	return nil
}

func validateArtifactFetchApplyRequest(request operator.ArtifactFetchApplyRequest) error {
	if err := validateArtifactFetchTarget(request.Name, request.Version); err != nil {
		return err
	}
	if !validSHA256Digest(request.PreviewDigest) {
		return errors.New("operator client: artifact fetch preview_digest must be canonical SHA-256")
	}
	if request.ConfirmName != request.Name || request.ConfirmVersion != request.Version {
		return errors.New("operator client: artifact fetch confirmation does not match target")
	}
	if !validArtifactFetchText(request.Reason, 500, false) {
		return errors.New("operator client: artifact fetch reason is required, must be canonical, and cannot exceed 500 bytes")
	}
	return nil
}

func encodeArtifactFetchListQuery(request store.ArtifactFetchListRequest) (url.Values, int, error) {
	query := make(url.Values)
	if request.State != "" {
		if !validArtifactFetchState(request.State) {
			return nil, 0, fmt.Errorf("operator client: invalid artifact fetch state %q", request.State)
		}
		query.Set("state", string(request.State))
	}
	if request.Name != "" {
		if err := validateArtifactFetchIdentifier("name", request.Name, 128); err != nil {
			return nil, 0, err
		}
		query.Set("name", request.Name)
	}
	if request.Version != "" {
		if err := validateArtifactFetchIdentifier("version", request.Version, 128); err != nil {
			return nil, 0, err
		}
		query.Set("version", request.Version)
	}
	limit := request.Limit
	if limit == 0 {
		limit = store.DefaultArtifactFetchReadLimit
	} else if limit < 1 || limit > store.MaxArtifactFetchReadLimit {
		return nil, 0, fmt.Errorf("operator client: artifact fetch limit must be between 1 and %d",
			store.MaxArtifactFetchReadLimit)
	} else {
		query.Set("limit", strconv.Itoa(limit))
	}
	return query, limit, nil
}

func validateArtifactFetchResponseHeaders(header http.Header, replayAllowed bool) (bool, error) {
	if err := validateJSONNoStoreResponse(header); err != nil {
		return false, err
	}
	if len(header.Values("ETag")) != 0 {
		return false, errors.New("operator client: artifact fetch response must not carry an ETag")
	}
	replayed, err := responseReplayEvidenceFromHeader(header)
	if err != nil {
		return false, err
	}
	if replayed && !replayAllowed {
		return false, errors.New("operator client: artifact fetch read or preview cannot be replayed")
	}
	return replayed, nil
}

func validateArtifactFetchPreviewResult(result operator.ArtifactFetchPreviewResult,
	request operator.ArtifactFetchPreviewRequest,
) error {
	if result.SchemaVersion != operator.ArtifactFetchPreviewSchemaVersion ||
		result.Name != request.Name || result.Version != request.Version ||
		!validArtifactFetchSHA512(result.SHA512Integrity) || !validSHA256Digest(result.PreviewDigest) ||
		!validArtifactFetchPreviewMaxBytes(result.SourceKind, result.MaxBytes) || !result.EnqueueAllowed ||
		result.Blockers == nil || len(result.Blockers) != 0 || result.PreviewedAt.IsZero() ||
		result.PreviewedAt.Location() != time.UTC {
		return errors.New("operator client: invalid artifact fetch preview result")
	}
	if !validArtifactFetchSourceResult(result.Name, result.Version, result.SourceKind,
		result.RegistryOrigin, result.PolicyVersion, result.EnginesNode) {
		return errors.New("operator client: invalid artifact fetch preview source")
	}
	if result.EnginesNode != nil && !validArtifactFetchText(*result.EnginesNode, 512, false) {
		return errors.New("operator client: artifact fetch preview engines_node is invalid")
	}
	if result.AlreadyAvailableAndVerified != (result.ExistingArtifactSHA256 != nil) {
		return errors.New("operator client: artifact fetch preview existing artifact evidence is inconsistent")
	}
	if result.ExistingArtifactSHA256 != nil && !validArtifactFetchSHA256(*result.ExistingArtifactSHA256) {
		return errors.New("operator client: artifact fetch preview existing artifact digest is invalid")
	}
	return nil
}

func validateArtifactFetchListResult(result store.ArtifactFetchListResult,
	request store.ArtifactFetchListRequest, limit int,
) error {
	if result.SchemaVersion != store.ArtifactFetchReadSchemaVersion ||
		result.Consistency != store.ArtifactFetchReadConsistency ||
		result.Items == nil || result.Total < 0 || result.Total < len(result.Items) ||
		len(result.Items) > limit {
		return errors.New("operator client: invalid artifact fetch list result")
	}
	if err := requireArtifactFetchUTCSecond(result.EvaluatedAt, "evaluated_at"); err != nil {
		return err
	}
	seen := make(map[string]bool, len(result.Items))
	for i, operation := range result.Items {
		if err := validateArtifactFetchOperation(operation); err != nil {
			return fmt.Errorf("operator client: artifact fetch list item %d: %w", i, err)
		}
		if seen[operation.OperationID] {
			return errors.New("operator client: duplicate artifact fetch operation_id")
		}
		seen[operation.OperationID] = true
		if request.State != "" && operation.State != request.State ||
			request.Name != "" && operation.Name != request.Name ||
			request.Version != "" && operation.Version != request.Version {
			return errors.New("operator client: artifact fetch item violates list filter")
		}
		if i > 0 && !artifactFetchOperationBefore(result.Items[i-1], operation) {
			return errors.New("operator client: artifact fetch operations are not in canonical order")
		}
	}
	if result.Total == 0 && len(result.Items) != 0 {
		return errors.New("operator client: empty artifact fetch list carries items")
	}
	return nil
}

func validateArtifactFetchOperation(operation store.ArtifactFetchOperation) error {
	if err := validateArtifactFetchIdentifier("operation_id", operation.OperationID, 128); err != nil {
		return err
	}
	if !validArtifactFetchOperationSource(operation) ||
		!validArtifactFetchSHA512(operation.SHA512Integrity) ||
		!validArtifactFetchText(operation.EnginesNode, 512, true) ||
		!validSHA256Digest(operation.IdentityDigest) || !validSHA256Digest(operation.PreviewDigest) ||
		operation.MaxBytes <= 0 || operation.MaxBytes > store.MaxArtifactFetchBytes ||
		operation.ProgressBytes < 0 || operation.ProgressBytes > operation.MaxBytes ||
		operation.Attempt < 0 || !validArtifactFetchState(operation.State) {
		return errors.New("operator client: invalid artifact fetch operation identity or bounds")
	}
	if err := requireArtifactFetchUTCSecond(operation.CreatedAt, "created_at"); err != nil {
		return err
	}
	if err := requireArtifactFetchUTCSecond(operation.UpdatedAt, "updated_at"); err != nil {
		return err
	}
	if operation.UpdatedAt.Before(operation.CreatedAt) {
		return errors.New("operator client: artifact fetch updated_at predates created_at")
	}
	if operation.StartedAt != nil {
		if err := requireArtifactFetchUTCSecond(*operation.StartedAt, "started_at"); err != nil {
			return err
		}
		if operation.StartedAt.Before(operation.CreatedAt) || operation.StartedAt.After(operation.UpdatedAt) {
			return errors.New("operator client: artifact fetch started_at is outside operation lifetime")
		}
	}
	if operation.FinishedAt != nil {
		if err := requireArtifactFetchUTCSecond(*operation.FinishedAt, "finished_at"); err != nil {
			return err
		}
		if !operation.FinishedAt.Equal(operation.UpdatedAt) {
			return errors.New("operator client: artifact fetch finished_at must equal updated_at")
		}
	}

	rank := artifactFetchPhaseRank(operation.Phase)
	switch operation.State {
	case store.ArtifactFetchQueued:
		if operation.Phase != store.ArtifactFetchPhaseQueued || rank != 0 ||
			operation.ProgressBytes != 0 || operation.Attempt != 0 ||
			!operation.UpdatedAt.Equal(operation.CreatedAt) || operation.StartedAt != nil ||
			operation.FinishedAt != nil || operation.ResultSHA256 != nil ||
			operation.ResultSizeBytes != nil || operation.ErrorCode != nil || operation.ErrorDetail != nil {
			return errors.New("operator client: queued artifact fetch lifecycle is incoherent")
		}
	case store.ArtifactFetchRunning:
		if rank < 1 || rank > 3 || operation.Attempt < 1 || operation.StartedAt == nil ||
			operation.FinishedAt != nil || operation.ResultSHA256 != nil ||
			operation.ResultSizeBytes != nil || operation.ErrorCode != nil || operation.ErrorDetail != nil {
			return errors.New("operator client: running artifact fetch lifecycle is incoherent")
		}
	case store.ArtifactFetchSucceeded:
		if operation.Phase != store.ArtifactFetchPhaseComplete || rank != 4 || operation.Attempt < 1 ||
			operation.StartedAt == nil || operation.FinishedAt == nil ||
			operation.ResultSHA256 == nil || !validArtifactFetchSHA256(*operation.ResultSHA256) ||
			operation.ResultSizeBytes == nil || *operation.ResultSizeBytes != operation.ProgressBytes ||
			operation.ErrorCode != nil || operation.ErrorDetail != nil {
			return errors.New("operator client: succeeded artifact fetch lifecycle is incoherent")
		}
	case store.ArtifactFetchFailed:
		if rank < 1 || rank > 3 || operation.Attempt < 1 || operation.StartedAt == nil ||
			operation.FinishedAt == nil || operation.ResultSHA256 != nil ||
			operation.ResultSizeBytes != nil || operation.ErrorCode == nil ||
			!validArtifactFetchErrorCode(*operation.ErrorCode) || operation.ErrorDetail == nil ||
			!validArtifactFetchText(*operation.ErrorDetail, store.ArtifactFetchMaxErrorBytes, false) {
			return errors.New("operator client: failed artifact fetch lifecycle is incoherent")
		}
	}
	return nil
}

func validArtifactFetchSourceResult(name, version, sourceKind, origin, policy string, enginesNode *string) bool {
	switch sourceKind {
	case artifact.ArtifactSourceNPM:
		return name == "openclaw" && artifact.ValidOpenClawVersion(version) &&
			origin == artifact.ProductionRegistryOrigin && policy == artifact.FetchPolicyVersion
	case artifact.ArtifactSourceNode:
		return name == "node-runtime" && artifact.ValidNodeRuntimeVersion(version) &&
			origin == artifact.ProductionNodeDistributionOrigin && policy == artifact.NodeRuntimeFetchPolicyVersion &&
			enginesNode == nil
	case artifact.ArtifactSourceHermesImage:
		return name == "hermes-agent" && artifact.ValidHermesVersion(version) &&
			origin == artifact.ProductionHermesRegistryOrigin && policy == artifact.HermesImageFetchPolicyVersion &&
			enginesNode == nil
	default:
		return false
	}
}

func validArtifactFetchOperationSource(operation store.ArtifactFetchOperation) bool {
	switch operation.SourceKind {
	case "", artifact.ArtifactSourceNPM:
		return operation.Name == "openclaw" && artifact.ValidOpenClawVersion(operation.Version) &&
			operation.RegistryOrigin == artifact.ProductionRegistryOrigin && operation.MaxBytes == artifact.DefaultArtifactMaxBytes
	case artifact.ArtifactSourceNode:
		return operation.Name == "node-runtime" && artifact.ValidNodeRuntimeVersion(operation.Version) &&
			operation.RegistryOrigin == artifact.ProductionNodeDistributionOrigin && operation.EnginesNode == "" &&
			operation.MaxBytes == artifact.DefaultNodeRuntimeBundleMaxBytes
	case artifact.ArtifactSourceHermesImage:
		return operation.Name == "hermes-agent" && artifact.ValidHermesVersion(operation.Version) &&
			operation.RegistryOrigin == artifact.ProductionHermesRegistryOrigin && operation.EnginesNode == "" &&
			operation.MaxBytes == artifact.DefaultHermesImageBundleMaxBytes
	default:
		return false
	}
}

func validArtifactFetchPreviewMaxBytes(sourceKind string, maxBytes int64) bool {
	switch sourceKind {
	case artifact.ArtifactSourceNPM:
		return maxBytes == artifact.DefaultArtifactMaxBytes
	case artifact.ArtifactSourceNode:
		return maxBytes == artifact.DefaultNodeRuntimeBundleMaxBytes
	case artifact.ArtifactSourceHermesImage:
		return maxBytes == artifact.DefaultHermesImageBundleMaxBytes
	default:
		return false
	}
}

func artifactFetchOperationBefore(left, right store.ArtifactFetchOperation) bool {
	if !left.CreatedAt.Equal(right.CreatedAt) {
		return left.CreatedAt.After(right.CreatedAt)
	}
	return left.OperationID > right.OperationID
}

func artifactFetchPhaseRank(phase store.ArtifactFetchPhase) int {
	switch phase {
	case store.ArtifactFetchPhaseQueued:
		return 0
	case store.ArtifactFetchPhaseDownloading:
		return 1
	case store.ArtifactFetchPhaseVerifying:
		return 2
	case store.ArtifactFetchPhasePublishing:
		return 3
	case store.ArtifactFetchPhaseComplete:
		return 4
	default:
		return -1
	}
}

func validArtifactFetchState(state store.ArtifactFetchState) bool {
	return state == store.ArtifactFetchQueued || state == store.ArtifactFetchRunning ||
		state == store.ArtifactFetchSucceeded || state == store.ArtifactFetchFailed
}

func validateArtifactFetchIdentifier(label, value string, maxBytes int) error {
	if value == "" || len(value) > maxBytes || !utf8.ValidString(value) ||
		value != strings.TrimSpace(value) || value == "." || value == ".." ||
		strings.ContainsAny(value, "/\\") {
		return fmt.Errorf("operator client: artifact fetch %s is not a canonical route-safe identifier", label)
	}
	for _, char := range value {
		if unicode.IsControl(char) || unicode.Is(unicode.Cf, char) {
			return fmt.Errorf("operator client: artifact fetch %s contains control or format characters", label)
		}
	}
	return nil
}

func validArtifactFetchText(value string, maxBytes int, allowEmpty bool) bool {
	if len(value) > maxBytes || !utf8.ValidString(value) || value != strings.TrimSpace(value) ||
		(!allowEmpty && value == "") {
		return false
	}
	for _, char := range value {
		if unicode.IsControl(char) || unicode.Is(unicode.Cf, char) {
			return false
		}
	}
	return true
}

func validArtifactFetchSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

func validArtifactFetchSHA512(value string) bool {
	encoded, ok := strings.CutPrefix(value, "sha512-")
	if !ok || encoded == "" {
		return false
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	return err == nil && len(raw) == sha512.Size && base64.StdEncoding.EncodeToString(raw) == encoded
}

func validArtifactFetchErrorCode(value string) bool {
	if len(value) < 1 || len(value) > 64 || value[0] < 'A' || value[0] > 'Z' {
		return false
	}
	for _, char := range value {
		if (char < 'A' || char > 'Z') && (char < '0' || char > '9') && char != '_' {
			return false
		}
	}
	return true
}

func requireArtifactFetchUTCSecond(value time.Time, label string) error {
	if value.IsZero() || value.Location() != time.UTC || value.Nanosecond() != 0 {
		return fmt.Errorf("operator client: artifact fetch %s must be a canonical UTC whole-second timestamp", label)
	}
	return nil
}
