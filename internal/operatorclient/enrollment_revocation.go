package operatorclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// PendingEnrollmentTokenResponse is deliberately limited to an unused
// enrollment ticket. It is not evidence about (and cannot revoke) an active
// agent bearer credential.
type PendingEnrollmentTokenResponse struct {
	MachineID      string    `json:"machine_id"`
	DisplayName    string    `json:"display_name"`
	TokenCreatedAt time.Time `json:"token_created_at"`
	TokenExpiresAt time.Time `json:"token_expires_at"`
	TokenExpired   bool      `json:"token_expired"`
}

type EnrollmentTokenRevocationPreviewResponse struct {
	MachineID                     string    `json:"machine_id"`
	DisplayName                   string    `json:"display_name"`
	TokenCreatedAt                time.Time `json:"token_created_at"`
	TokenExpiresAt                time.Time `json:"token_expires_at"`
	TokenExpired                  bool      `json:"token_expired"`
	PreviewedAt                   time.Time `json:"previewed_at"`
	RegistryRetained              bool      `json:"registry_retained"`
	DenominatorDelta              int       `json:"denominator_delta"`
	ActiveAgentCredentialAffected bool      `json:"active_agent_credential_affected"`
	PreviewDigest                 string    `json:"preview_digest"`
}

type EnrollmentTokenRevocationRequest struct {
	PreviewDigest string `json:"preview_digest"`
	Reason        string `json:"reason"`
}

type EnrollmentTokenRevocationResponse struct {
	MachineID                     string    `json:"machine_id"`
	DisplayName                   string    `json:"display_name"`
	TokenCreatedAt                time.Time `json:"token_created_at"`
	TokenExpiresAt                time.Time `json:"token_expires_at"`
	TokenWasExpired               bool      `json:"token_was_expired"`
	RevokedAt                     time.Time `json:"revoked_at"`
	RegistryRetained              bool      `json:"registry_retained"`
	DenominatorDelta              int       `json:"denominator_delta"`
	ActiveAgentCredentialAffected bool      `json:"active_agent_credential_affected"`
	PreviewDigest                 string    `json:"preview_digest"`
	Replayed                      bool      `json:"replayed"`
}

func (c *Client) PendingEnrollmentToken(ctx context.Context, machineID string) (PendingEnrollmentTokenResponse, error) {
	req, err := c.newMachineOperatorRequest(ctx, http.MethodGet, machineID, "/enrollment-token", nil)
	if err != nil {
		return PendingEnrollmentTokenResponse{}, err
	}
	response, err := c.doRaw(req)
	if err != nil {
		return PendingEnrollmentTokenResponse{}, err
	}
	if response.status != http.StatusOK {
		return PendingEnrollmentTokenResponse{}, fmt.Errorf(
			"operator client: pending enrollment token returned unexpected success status HTTP %d", response.status)
	}
	if err := validateJSONNoStoreResponse(response.header); err != nil {
		return PendingEnrollmentTokenResponse{}, err
	}
	if replayed, err := responseReplayEvidenceFromHeader(response.header); err != nil {
		return PendingEnrollmentTokenResponse{}, err
	} else if replayed {
		return PendingEnrollmentTokenResponse{}, errors.New("operator client: pending enrollment token read cannot be an idempotency replay")
	}
	result, err := decodePendingEnrollmentTokenResponse(response.body)
	if err != nil {
		return PendingEnrollmentTokenResponse{}, err
	}
	if err := validatePendingEnrollmentToken(result, machineID); err != nil {
		return PendingEnrollmentTokenResponse{}, err
	}
	return result, nil
}

func (c *Client) PreviewEnrollmentTokenRevocation(ctx context.Context, machineID string) (EnrollmentTokenRevocationPreviewResponse, error) {
	req, err := c.newMachineOperatorRequest(ctx, http.MethodPost, machineID, "/enrollment-token/revocation-preview", bytes.NewReader([]byte("{}")))
	if err != nil {
		return EnrollmentTokenRevocationPreviewResponse{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	response, err := c.doRaw(req)
	if err != nil {
		return EnrollmentTokenRevocationPreviewResponse{}, err
	}
	if response.status != http.StatusOK {
		return EnrollmentTokenRevocationPreviewResponse{}, fmt.Errorf(
			"operator client: enrollment token revocation preview returned unexpected success status HTTP %d", response.status)
	}
	if err := validateJSONNoStoreResponse(response.header); err != nil {
		return EnrollmentTokenRevocationPreviewResponse{}, err
	}
	if replayed, err := responseReplayEvidenceFromHeader(response.header); err != nil {
		return EnrollmentTokenRevocationPreviewResponse{}, err
	} else if replayed {
		return EnrollmentTokenRevocationPreviewResponse{}, errors.New("operator client: enrollment token revocation preview cannot be an idempotency replay")
	}
	result, err := decodeEnrollmentTokenRevocationPreviewResponse(response.body)
	if err != nil {
		return EnrollmentTokenRevocationPreviewResponse{}, err
	}
	if err := validateEnrollmentTokenRevocationPreview(result, machineID); err != nil {
		return EnrollmentTokenRevocationPreviewResponse{}, err
	}
	return result, nil
}

func (c *Client) RevokeEnrollmentToken(ctx context.Context, machineID, idempotencyKey string,
	body EnrollmentTokenRevocationRequest,
) (EnrollmentTokenRevocationResponse, error) {
	if strings.TrimSpace(idempotencyKey) == "" {
		return EnrollmentTokenRevocationResponse{}, errors.New("operator client: Idempotency-Key 不可省略")
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return EnrollmentTokenRevocationResponse{}, fmt.Errorf("operator client: encode enrollment token revocation request: %w", err)
	}
	req, err := c.newMachineOperatorRequest(ctx, http.MethodPost, machineID, "/enrollment-token/revocations", bytes.NewReader(raw))
	if err != nil {
		return EnrollmentTokenRevocationResponse{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", idempotencyKey)
	response, err := c.doRaw(req)
	if err != nil {
		return EnrollmentTokenRevocationResponse{}, err
	}
	if response.status != http.StatusCreated && response.status != http.StatusOK {
		return EnrollmentTokenRevocationResponse{}, fmt.Errorf(
			"operator client: enrollment token revocation returned unexpected success status HTTP %d", response.status)
	}
	if err := validateJSONNoStoreResponse(response.header); err != nil {
		return EnrollmentTokenRevocationResponse{}, err
	}
	replayedHeader, err := responseReplayEvidenceFromHeader(response.header)
	if err != nil {
		return EnrollmentTokenRevocationResponse{}, err
	}
	result, err := decodeEnrollmentTokenRevocationResponse(response.body)
	if err != nil {
		return EnrollmentTokenRevocationResponse{}, err
	}
	if result.Replayed != replayedHeader {
		return EnrollmentTokenRevocationResponse{}, fmt.Errorf(
			"operator client: enrollment token revocation replay evidence mismatch body=%t header=%t",
			result.Replayed, replayedHeader)
	}
	if response.status == http.StatusCreated && result.Replayed {
		return EnrollmentTokenRevocationResponse{}, errors.New("operator client: HTTP 201 enrollment token revocation claims replay")
	}
	if response.status == http.StatusOK && !result.Replayed {
		return EnrollmentTokenRevocationResponse{}, errors.New("operator client: HTTP 200 enrollment token revocation is not labelled replay")
	}
	if err := validateEnrollmentTokenRevocation(result, machineID, body); err != nil {
		return EnrollmentTokenRevocationResponse{}, err
	}
	return result, nil
}

func (c *Client) newMachineOperatorRequest(ctx context.Context, method, machineID, suffix string, body io.Reader) (*http.Request, error) {
	if strings.TrimSpace(machineID) == "" || strings.Contains(machineID, "/") || machineID == "." || machineID == ".." {
		return nil, errors.New("operator client: machine_id 不可為空、dot segment 或包含斜線")
	}
	path := "/v1/operator/machines/" + url.PathEscape(machineID) + suffix
	return c.newOperatorRequest(ctx, method, path, body)
}

func decodePendingEnrollmentTokenResponse(raw []byte) (PendingEnrollmentTokenResponse, error) {
	var result PendingEnrollmentTokenResponse
	seen, err := decodeExactJSONObject(raw, "pending enrollment token", map[string]any{
		"machine_id":       &result.MachineID,
		"display_name":     &result.DisplayName,
		"token_created_at": &result.TokenCreatedAt,
		"token_expires_at": &result.TokenExpiresAt,
		"token_expired":    &result.TokenExpired,
	})
	if err != nil {
		return PendingEnrollmentTokenResponse{}, err
	}
	if err := requireResponseFields("pending enrollment token", seen,
		"machine_id", "display_name", "token_created_at", "token_expires_at", "token_expired"); err != nil {
		return PendingEnrollmentTokenResponse{}, err
	}
	return result, nil
}

func decodeEnrollmentTokenRevocationPreviewResponse(raw []byte) (EnrollmentTokenRevocationPreviewResponse, error) {
	var result EnrollmentTokenRevocationPreviewResponse
	seen, err := decodeExactJSONObject(raw, "enrollment token revocation preview", map[string]any{
		"machine_id":                       &result.MachineID,
		"display_name":                     &result.DisplayName,
		"token_created_at":                 &result.TokenCreatedAt,
		"token_expires_at":                 &result.TokenExpiresAt,
		"token_expired":                    &result.TokenExpired,
		"previewed_at":                     &result.PreviewedAt,
		"registry_retained":                &result.RegistryRetained,
		"denominator_delta":                &result.DenominatorDelta,
		"active_agent_credential_affected": &result.ActiveAgentCredentialAffected,
		"preview_digest":                   &result.PreviewDigest,
	})
	if err != nil {
		return EnrollmentTokenRevocationPreviewResponse{}, err
	}
	if err := requireResponseFields("enrollment token revocation preview", seen,
		"machine_id", "display_name", "token_created_at", "token_expires_at", "token_expired",
		"previewed_at", "registry_retained", "denominator_delta",
		"active_agent_credential_affected", "preview_digest"); err != nil {
		return EnrollmentTokenRevocationPreviewResponse{}, err
	}
	return result, nil
}

func decodeEnrollmentTokenRevocationResponse(raw []byte) (EnrollmentTokenRevocationResponse, error) {
	var result EnrollmentTokenRevocationResponse
	seen, err := decodeExactJSONObject(raw, "enrollment token revocation", map[string]any{
		"machine_id":                       &result.MachineID,
		"display_name":                     &result.DisplayName,
		"token_created_at":                 &result.TokenCreatedAt,
		"token_expires_at":                 &result.TokenExpiresAt,
		"token_was_expired":                &result.TokenWasExpired,
		"revoked_at":                       &result.RevokedAt,
		"registry_retained":                &result.RegistryRetained,
		"denominator_delta":                &result.DenominatorDelta,
		"active_agent_credential_affected": &result.ActiveAgentCredentialAffected,
		"preview_digest":                   &result.PreviewDigest,
		"replayed":                         &result.Replayed,
	})
	if err != nil {
		return EnrollmentTokenRevocationResponse{}, err
	}
	if err := requireResponseFields("enrollment token revocation", seen,
		"machine_id", "display_name", "token_created_at", "token_expires_at", "token_was_expired",
		"revoked_at", "registry_retained", "denominator_delta",
		"active_agent_credential_affected", "preview_digest", "replayed"); err != nil {
		return EnrollmentTokenRevocationResponse{}, err
	}
	return result, nil
}

func validatePendingEnrollmentToken(result PendingEnrollmentTokenResponse, machineID string) error {
	if result.MachineID != machineID {
		return errors.New("operator client: pending enrollment token machine_id does not match request target")
	}
	if strings.TrimSpace(result.DisplayName) == "" {
		return errors.New("operator client: pending enrollment token display_name is empty")
	}
	if result.TokenCreatedAt.IsZero() || result.TokenExpiresAt.IsZero() || !result.TokenCreatedAt.Before(result.TokenExpiresAt) {
		return errors.New("operator client: pending enrollment token timestamps are incoherent")
	}
	return nil
}

func validateEnrollmentTokenRevocationPreview(result EnrollmentTokenRevocationPreviewResponse, machineID string) error {
	status := PendingEnrollmentTokenResponse{
		MachineID: result.MachineID, DisplayName: result.DisplayName,
		TokenCreatedAt: result.TokenCreatedAt, TokenExpiresAt: result.TokenExpiresAt,
		TokenExpired: result.TokenExpired,
	}
	if err := validatePendingEnrollmentToken(status, machineID); err != nil {
		return err
	}
	if result.PreviewedAt.IsZero() || result.PreviewedAt.Before(result.TokenCreatedAt) ||
		result.TokenExpired != !result.PreviewedAt.Before(result.TokenExpiresAt) {
		return errors.New("operator client: enrollment token revocation preview expiry evidence is incoherent")
	}
	if !result.RegistryRetained || result.DenominatorDelta != 0 || result.ActiveAgentCredentialAffected {
		return errors.New("operator client: enrollment token revocation preview impact contract is invalid")
	}
	if !validSHA256Digest(result.PreviewDigest) {
		return errors.New("operator client: enrollment token revocation preview_digest is not canonical SHA-256")
	}
	return nil
}

func validateEnrollmentTokenRevocation(result EnrollmentTokenRevocationResponse, machineID string,
	request EnrollmentTokenRevocationRequest,
) error {
	status := PendingEnrollmentTokenResponse{
		MachineID: result.MachineID, DisplayName: result.DisplayName,
		TokenCreatedAt: result.TokenCreatedAt, TokenExpiresAt: result.TokenExpiresAt,
	}
	if err := validatePendingEnrollmentToken(status, machineID); err != nil {
		return err
	}
	if result.RevokedAt.IsZero() || result.RevokedAt.Before(result.TokenCreatedAt) ||
		result.TokenWasExpired != !result.RevokedAt.Before(result.TokenExpiresAt) {
		return errors.New("operator client: enrollment token revocation expiry evidence is incoherent")
	}
	if !result.RegistryRetained || result.DenominatorDelta != 0 || result.ActiveAgentCredentialAffected {
		return errors.New("operator client: enrollment token revocation impact contract is invalid")
	}
	if result.PreviewDigest != request.PreviewDigest || !validSHA256Digest(result.PreviewDigest) {
		return errors.New("operator client: enrollment token revocation preview_digest does not match request")
	}
	return nil
}
