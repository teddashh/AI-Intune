package operatorclient

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/store"
)

// VerifierPreviewRequest carries only what the operator chose. The Hub resolves
// a hub_prober's failure domain from its own --hub-host; a client that guessed
// it could name a domain the Hub never validated.
type VerifierPreviewRequest struct {
	Kind          string `json:"kind"`
	DisplayName   string `json:"display_name"`
	FailureDomain string `json:"failure_domain"`
}

type VerifierCreateRequest struct {
	Kind          string `json:"kind"`
	DisplayName   string `json:"display_name"`
	FailureDomain string `json:"failure_domain"`
	PreviewDigest string `json:"preview_digest"`
	Reason        string `json:"reason"`
}

// VerifierResponse has two legal shapes. A fresh registration carries the
// credential exactly once; a replay carries no credential field at all and
// names the only recovery there is.
type VerifierResponse struct {
	VerifierID       string    `json:"verifier_id"`
	Kind             string    `json:"kind"`
	DisplayName      string    `json:"display_name"`
	FailureDomain    string    `json:"failure_domain"`
	CreatedAt        time.Time `json:"created_at"`
	Revision         int64     `json:"revision"`
	PreviewDigest    string    `json:"preview_digest"`
	Credential       string    `json:"credential,omitempty"`
	SecretAvailable  bool      `json:"secret_available"`
	Replayed         bool      `json:"replayed"`
	RecoveryRequired bool      `json:"recovery_required"`
	RecoveryAction   string    `json:"recovery_action"`
}

type VerifierRevocationRequest struct {
	ExpectedRevision   *int64 `json:"expected_revision"`
	ConfirmDisplayName string `json:"confirm_display_name"`
	PreviewDigest      string `json:"preview_digest"`
	Reason             string `json:"reason"`
}

func (c *Client) Verifiers(ctx context.Context) (operator.VerifierListResult, error) {
	var result operator.VerifierListResult
	if err := c.readVerifierDocument(ctx, "/v1/operator/verifiers", "verifier list", &result); err != nil {
		return operator.VerifierListResult{}, err
	}
	if err := validateVerifierList(result); err != nil {
		return operator.VerifierListResult{}, err
	}
	return result, nil
}

func (c *Client) Verifier(ctx context.Context, verifierID string) (operator.VerifierDetailResult, error) {
	path, err := verifierPath(verifierID, "")
	if err != nil {
		return operator.VerifierDetailResult{}, err
	}
	var result operator.VerifierDetailResult
	if err := c.readVerifierDocument(ctx, path, "verifier detail", &result); err != nil {
		return operator.VerifierDetailResult{}, err
	}
	if err := validateVerifierItem(result.Item); err != nil {
		return operator.VerifierDetailResult{}, err
	}
	if result.Item.VerifierID != verifierID {
		return operator.VerifierDetailResult{}, errors.New(
			"operator client: verifier detail identity does not match the request")
	}
	if result.SeparationRule != store.OperatorVerifierSeparationRule ||
		result.GrantsDeploymentGate != store.VerifierKindGrantsDeploymentGate(result.Item.Kind) {
		return operator.VerifierDetailResult{}, errors.New(
			"operator client: verifier detail policy contract is invalid")
	}
	if result.JobsWithEvidence < 0 || result.JobsWithEvidence > result.Item.EvidenceRows {
		return operator.VerifierDetailResult{}, errors.New(
			"operator client: verifier detail job count exceeds its own evidence rows")
	}
	return result, nil
}

func (c *Client) PreviewVerifier(ctx context.Context, body VerifierPreviewRequest) (
	store.OperatorVerifierPreviewResult, error,
) {
	var result store.OperatorVerifierPreviewResult
	if err := c.postVerifierDocument(ctx, "/v1/operator/verifiers/preview",
		"verifier preview", body, &result); err != nil {
		return store.OperatorVerifierPreviewResult{}, err
	}
	if result.Kind != body.Kind || result.DisplayName != body.DisplayName ||
		result.FailureDomain != body.FailureDomain {
		return store.OperatorVerifierPreviewResult{}, errors.New(
			"operator client: verifier preview does not describe the requested verifier")
	}
	if err := validateVerifierPreviewPolicy(result); err != nil {
		return store.OperatorVerifierPreviewResult{}, err
	}
	return result, nil
}

func (c *Client) RegisterVerifier(ctx context.Context, idempotencyKey string,
	body VerifierCreateRequest,
) (VerifierResponse, error) {
	if strings.TrimSpace(idempotencyKey) == "" {
		return VerifierResponse{}, errors.New("operator client: Idempotency-Key cannot be omitted")
	}
	response, err := c.doVerifierMutation(ctx, "/v1/operator/verifiers", idempotencyKey, body)
	if err != nil {
		return VerifierResponse{}, err
	}
	result, credentialPresent, err := decodeVerifierResponse(response.body)
	if err != nil {
		return VerifierResponse{}, err
	}
	if err := validateVerifierMutationStatus("verifier register", response, result.Replayed); err != nil {
		return VerifierResponse{}, err
	}
	if err := validateVerifierResponse(result, credentialPresent, response.status, body); err != nil {
		return VerifierResponse{}, err
	}
	return result, nil
}

func (c *Client) PreviewVerifierRevocation(ctx context.Context, verifierID string) (
	store.OperatorVerifierRevocationPreviewResult, error,
) {
	path, err := verifierPath(verifierID, "/revocation-preview")
	if err != nil {
		return store.OperatorVerifierRevocationPreviewResult{}, err
	}
	var result store.OperatorVerifierRevocationPreviewResult
	if err := c.postVerifierDocument(ctx, path, "verifier revocation preview",
		struct{}{}, &result); err != nil {
		return store.OperatorVerifierRevocationPreviewResult{}, err
	}
	if result.VerifierID != verifierID {
		return store.OperatorVerifierRevocationPreviewResult{}, errors.New(
			"operator client: verifier revocation preview names a different verifier")
	}
	if err := validateVerifierRevocationImpact(result.Revision, result.EvidenceRows,
		result.JobsLosingOnlyProducer, result.RegistryRowRetained, result.EvidenceRetained,
		result.PreviewDigest); err != nil {
		return store.OperatorVerifierRevocationPreviewResult{}, err
	}
	if result.DisplayNameReusable ||
		result.GrantsDeploymentGate != store.VerifierKindGrantsDeploymentGate(result.Kind) {
		return store.OperatorVerifierRevocationPreviewResult{}, errors.New(
			"operator client: verifier revocation preview policy contract is invalid")
	}
	if !validVerifierKind(result.Kind) {
		return store.OperatorVerifierRevocationPreviewResult{}, errors.New(
			"operator client: verifier revocation preview kind is unknown")
	}
	return result, nil
}

func (c *Client) RevokeVerifier(ctx context.Context, verifierID, idempotencyKey string,
	body VerifierRevocationRequest,
) (store.OperatorVerifierRevocationResult, error) {
	if strings.TrimSpace(idempotencyKey) == "" {
		return store.OperatorVerifierRevocationResult{}, errors.New("operator client: Idempotency-Key cannot be omitted")
	}
	if body.ExpectedRevision == nil {
		return store.OperatorVerifierRevocationResult{}, errors.New(
			"operator client: verifier revocation must include expected_revision")
	}
	path, err := verifierPath(verifierID, "/revocations")
	if err != nil {
		return store.OperatorVerifierRevocationResult{}, err
	}
	response, err := c.doVerifierMutation(ctx, path, idempotencyKey, body)
	if err != nil {
		return store.OperatorVerifierRevocationResult{}, err
	}
	var result store.OperatorVerifierRevocationResult
	if err := decodeStrictJSONDocument(response.body, "verifier revocation", &result); err != nil {
		return store.OperatorVerifierRevocationResult{}, err
	}
	if err := validateVerifierMutationStatus("verifier revocation", response, result.Replayed); err != nil {
		return store.OperatorVerifierRevocationResult{}, err
	}
	if result.VerifierID != verifierID || result.PreviewDigest != body.PreviewDigest ||
		result.DisplayName != body.ConfirmDisplayName {
		return store.OperatorVerifierRevocationResult{}, errors.New(
			"operator client: verifier revocation result does not match the request")
	}
	if result.PreviousRevision != *body.ExpectedRevision || result.Revision != result.PreviousRevision+1 {
		return store.OperatorVerifierRevocationResult{}, errors.New(
			"operator client: verifier revocation revision does not advance from the asserted one")
	}
	if err := validateVerifierRevocationImpact(result.PreviousRevision, result.EvidenceRows,
		result.JobsLosingOnlyProducer, result.RegistryRowRetained, result.EvidenceRetained,
		result.PreviewDigest); err != nil {
		return store.OperatorVerifierRevocationResult{}, err
	}
	if result.RevokedAt.IsZero() || result.RevokedAt.Location() != time.UTC ||
		!validVerifierKind(result.Kind) {
		return store.OperatorVerifierRevocationResult{}, errors.New(
			"operator client: verifier revocation receipt metadata is invalid")
	}
	return result, nil
}

func (c *Client) readVerifierDocument(ctx context.Context, path, label string, dst any) error {
	req, err := c.newOperatorRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	response, err := c.doRaw(req)
	if err != nil {
		return err
	}
	if response.status != http.StatusOK {
		return fmt.Errorf("operator client: %s returned unexpected success status HTTP %d", label, response.status)
	}
	if err := validateJSONNoStoreResponse(response.header); err != nil {
		return err
	}
	if replayed, err := responseReplayEvidenceFromHeader(response.header); err != nil {
		return err
	} else if replayed {
		return fmt.Errorf("operator client: %s read cannot be an idempotency replay", label)
	}
	return decodeStrictJSONDocument(response.body, label, dst)
}

func (c *Client) postVerifierDocument(ctx context.Context, path, label string, body, dst any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("operator client: encode %s request: %w", label, err)
	}
	req, err := c.newOperatorRequest(ctx, http.MethodPost, path, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	response, err := c.doRaw(req)
	if err != nil {
		return err
	}
	if response.status != http.StatusOK {
		return fmt.Errorf("operator client: %s returned unexpected success status HTTP %d", label, response.status)
	}
	if err := validateJSONNoStoreResponse(response.header); err != nil {
		return err
	}
	if replayed, err := responseReplayEvidenceFromHeader(response.header); err != nil {
		return err
	} else if replayed {
		return fmt.Errorf("operator client: %s cannot be an idempotency replay", label)
	}
	return decodeStrictJSONDocument(response.body, label, dst)
}

func (c *Client) doVerifierMutation(ctx context.Context, path, idempotencyKey string, body any) (
	operatorRawResponse, error,
) {
	raw, err := json.Marshal(body)
	if err != nil {
		return operatorRawResponse{}, fmt.Errorf("operator client: encode verifier request: %w", err)
	}
	req, err := c.newOperatorRequest(ctx, http.MethodPost, path, bytes.NewReader(raw))
	if err != nil {
		return operatorRawResponse{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", idempotencyKey)
	return c.doRaw(req)
}

func validateVerifierMutationStatus(label string, response operatorRawResponse, replayed bool) error {
	if response.status != http.StatusCreated && response.status != http.StatusOK {
		return fmt.Errorf("operator client: %s returned unexpected success status HTTP %d", label, response.status)
	}
	if err := validateJSONNoStoreResponse(response.header); err != nil {
		return err
	}
	replayedHeader, err := responseReplayEvidenceFromHeader(response.header)
	if err != nil {
		return err
	}
	if replayed != replayedHeader {
		return fmt.Errorf("operator client: %s replay evidence mismatch body=%t header=%t",
			label, replayed, replayedHeader)
	}
	if response.status == http.StatusCreated && replayed {
		return fmt.Errorf("operator client: HTTP 201 %s claims replay", label)
	}
	if response.status == http.StatusOK && !replayed {
		return fmt.Errorf("operator client: HTTP 200 %s is not labelled replay", label)
	}
	return nil
}

func decodeVerifierResponse(raw []byte) (VerifierResponse, bool, error) {
	var result VerifierResponse
	seen, err := decodeExactJSONObject(raw, "verifier register", map[string]any{
		"verifier_id":       &result.VerifierID,
		"kind":              &result.Kind,
		"display_name":      &result.DisplayName,
		"failure_domain":    &result.FailureDomain,
		"created_at":        &result.CreatedAt,
		"revision":          &result.Revision,
		"preview_digest":    &result.PreviewDigest,
		"credential":        &result.Credential,
		"secret_available":  &result.SecretAvailable,
		"replayed":          &result.Replayed,
		"recovery_required": &result.RecoveryRequired,
		"recovery_action":   &result.RecoveryAction,
	})
	if err != nil {
		return VerifierResponse{}, false, err
	}
	if err := requireResponseFields("verifier register", seen,
		"verifier_id", "kind", "display_name", "failure_domain", "created_at", "revision",
		"preview_digest", "secret_available", "replayed", "recovery_required",
		"recovery_action"); err != nil {
		return VerifierResponse{}, false, err
	}
	return result, seen["credential"], nil
}

func validateVerifierResponse(result VerifierResponse, credentialPresent bool, status int,
	request VerifierCreateRequest,
) error {
	if result.Kind != request.Kind || result.DisplayName != request.DisplayName ||
		result.FailureDomain != request.FailureDomain ||
		result.PreviewDigest != request.PreviewDigest {
		return errors.New("operator client: verifier register response does not match the request")
	}
	if !validSHA256Digest(result.PreviewDigest) {
		return errors.New("operator client: verifier register preview_digest is not canonical SHA-256")
	}
	if result.VerifierID == "" || strings.TrimSpace(result.VerifierID) != result.VerifierID ||
		result.Revision != 1 || result.CreatedAt.IsZero() || result.CreatedAt.Location() != time.UTC {
		return errors.New("operator client: verifier register response identity is invalid")
	}
	if status == http.StatusCreated {
		if !credentialPresent || !validVerifierCredential(result.Credential) || !result.SecretAvailable {
			return errors.New("operator client: fresh verifier response does not contain one valid available credential")
		}
		if result.RecoveryRequired || result.RecoveryAction != "" {
			return errors.New("operator client: fresh verifier response incorrectly requires recovery")
		}
		return nil
	}
	if credentialPresent || result.Credential != "" || result.SecretAvailable {
		return errors.New("operator client: replayed verifier response exposed or claimed a credential")
	}
	if !result.RecoveryRequired ||
		result.RecoveryAction != store.OperatorVerifierRecoveryRevokeAndRegister {
		return errors.New("operator client: replayed verifier response lacks exact recovery instructions")
	}
	return nil
}

func validateVerifierPreviewPolicy(result store.OperatorVerifierPreviewResult) error {
	if !validVerifierKind(result.Kind) {
		return errors.New("operator client: verifier preview kind is unknown")
	}
	if result.SeparationRule != store.OperatorVerifierSeparationRule ||
		result.CredentialDelivery != store.OperatorVerifierCredentialOneTime ||
		result.EvidenceRole != store.JobVerificationRoleIndependent ||
		!result.RevocationKeepsRow ||
		result.GrantsDeploymentGate != store.VerifierKindGrantsDeploymentGate(result.Kind) {
		return errors.New("operator client: verifier preview policy contract is invalid")
	}
	if !validSHA256Digest(result.PreviewDigest) {
		return errors.New("operator client: verifier preview_digest is not canonical SHA-256")
	}
	if result.PreviewedAt.IsZero() || result.PreviewedAt.Location() != time.UTC {
		return errors.New("operator client: verifier previewed_at is not a UTC instant")
	}
	return nil
}

func validateVerifierList(result operator.VerifierListResult) error {
	if result.Verifiers == nil {
		return errors.New("operator client: verifier list array must not be null")
	}
	if result.SeparationRule != store.OperatorVerifierSeparationRule {
		return errors.New("operator client: verifier list separation rule is not the registered one")
	}
	if result.Active < 0 || result.Revoked < 0 || result.Active+result.Revoked != len(result.Verifiers) {
		return errors.New("operator client: verifier list state counts do not cover its rows")
	}
	active, seenIDs := 0, make(map[string]bool, len(result.Verifiers))
	seenNames := make(map[string]bool, len(result.Verifiers))
	for _, item := range result.Verifiers {
		if err := validateVerifierItem(item); err != nil {
			return err
		}
		if seenIDs[item.VerifierID] || seenNames[item.DisplayName] {
			return errors.New("operator client: verifier list repeats an identity")
		}
		seenIDs[item.VerifierID], seenNames[item.DisplayName] = true, true
		if item.State == store.VerifierStateActive {
			active++
		}
	}
	if active != result.Active {
		return errors.New("operator client: verifier list active count contradicts its rows")
	}
	return nil
}

// validateVerifierItem checks the one thing a revoked row must never do: read
// as a producer that can still speak. State and revoked_at have to agree.
func validateVerifierItem(item operator.VerifierItem) error {
	if err := validateJobReadIdentifier("verifier.verifier_id", item.VerifierID, 256); err != nil {
		return err
	}
	if !validVerifierKind(item.Kind) {
		return errors.New("operator client: verifier kind is unknown")
	}
	for _, field := range []struct{ name, value string }{
		{"verifier.display_name", item.DisplayName},
		{"verifier.failure_domain", item.FailureDomain},
	} {
		if err := validateJobReadIdentifier(field.name, field.value, 256); err != nil {
			return err
		}
	}
	if item.Revision < 1 || item.EvidenceRows < 0 || item.CreatedAt.IsZero() {
		return errors.New("operator client: verifier row metadata is invalid")
	}
	for _, field := range []struct {
		name string
		at   *time.Time
	}{
		{"verifier.created_at", &item.CreatedAt},
		{"verifier.revoked_at", item.RevokedAt},
		{"verifier.last_seen_at", item.LastSeenAt},
	} {
		if field.at == nil {
			continue
		}
		if field.at.IsZero() || field.at.Location() != time.UTC {
			return fmt.Errorf("operator client: %s is not a UTC instant", field.name)
		}
	}
	switch item.State {
	case store.VerifierStateActive:
		if item.RevokedAt != nil {
			return errors.New("operator client: active verifier carries a revocation time")
		}
	case store.VerifierStateRevoked:
		if item.RevokedAt == nil {
			return errors.New("operator client: revoked verifier carries no revocation time")
		}
	default:
		return errors.New("operator client: verifier state is unknown")
	}
	return nil
}

func validateVerifierRevocationImpact(revision, evidenceRows, jobsLosingOnlyProducer int64,
	registryRowRetained, evidenceRetained bool, previewDigest string,
) error {
	if revision < 1 || evidenceRows < 0 || jobsLosingOnlyProducer < 0 ||
		jobsLosingOnlyProducer > evidenceRows {
		return errors.New("operator client: verifier revocation impact counts are inconsistent")
	}
	if !registryRowRetained || !evidenceRetained {
		return errors.New("operator client: verifier revocation would drop the row or its evidence")
	}
	if !validSHA256Digest(previewDigest) {
		return errors.New("operator client: verifier revocation preview_digest is not canonical SHA-256")
	}
	return nil
}

func validVerifierKind(kind string) bool {
	switch kind {
	case store.VerifierKindFleetPeerAgent, store.VerifierKindHubProber, store.VerifierKindExternalJobRunner:
		return true
	}
	return false
}

func validVerifierCredential(value string) bool {
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	return err == nil && len(decoded) == 32 && base64.RawURLEncoding.EncodeToString(decoded) == value
}

func verifierPath(verifierID, suffix string) (string, error) {
	if strings.TrimSpace(verifierID) != verifierID || verifierID == "" ||
		strings.Contains(verifierID, "/") || verifierID == "." || verifierID == ".." {
		return "", errors.New("operator client: verifier_id cannot be empty, have leading or trailing whitespace, dot segment, or contain slash")
	}
	return "/v1/operator/verifiers/" + url.PathEscape(verifierID) + suffix, nil
}
