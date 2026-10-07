package operatorclient

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/teddashh/AI-Intune/internal/store"
)

// VerificationAssignmentPreviewRequest and VerificationAssignmentRequest carry
// only the job. The verifier comes from the path, and no field on this wire
// names a command: an assignment tells a verifier which job to look at and
// nothing about how to look at it.
type VerificationAssignmentPreviewRequest struct {
	JobID string `json:"job_id"`
}

type VerificationAssignmentRequest struct {
	JobID               string `json:"job_id"`
	ConfirmVerifierName string `json:"confirm_verifier_name"`
	PreviewDigest       string `json:"preview_digest"`
	Reason              string `json:"reason"`
}

func (c *Client) PreviewVerificationAssignment(ctx context.Context, verifierID, jobID string) (
	store.OperatorVerificationAssignmentPreviewResult, error,
) {
	path, err := verifierPath(verifierID, "/assignment-preview")
	if err != nil {
		return store.OperatorVerificationAssignmentPreviewResult{}, err
	}
	var result store.OperatorVerificationAssignmentPreviewResult
	if err := c.postVerifierDocument(ctx, path, "verification assignment preview",
		VerificationAssignmentPreviewRequest{JobID: jobID}, &result); err != nil {
		return store.OperatorVerificationAssignmentPreviewResult{}, err
	}
	if result.VerifierID != verifierID || result.JobID != jobID {
		return store.OperatorVerificationAssignmentPreviewResult{}, errors.New(
			"operator client: verification assignment preview does not describe the requested pair")
	}
	if err := validateVerificationAssignmentPreview(result); err != nil {
		return store.OperatorVerificationAssignmentPreviewResult{}, err
	}
	return result, nil
}

func (c *Client) AssignVerification(ctx context.Context, verifierID, idempotencyKey string,
	body VerificationAssignmentRequest,
) (store.OperatorVerificationAssignmentResult, error) {
	if strings.TrimSpace(idempotencyKey) == "" {
		return store.OperatorVerificationAssignmentResult{}, errors.New(
			"operator client: Idempotency-Key cannot be omitted")
	}
	if !validSHA256Digest(body.PreviewDigest) {
		return store.OperatorVerificationAssignmentResult{}, errors.New(
			"operator client: verification assignment must include canonical preview_digest")
	}
	if strings.TrimSpace(body.ConfirmVerifierName) == "" {
		return store.OperatorVerificationAssignmentResult{}, errors.New(
			"operator client: verification assignment must include confirm_verifier_name")
	}
	path, err := verifierPath(verifierID, "/assignments")
	if err != nil {
		return store.OperatorVerificationAssignmentResult{}, err
	}
	response, err := c.doVerifierMutation(ctx, path, idempotencyKey, body)
	if err != nil {
		return store.OperatorVerificationAssignmentResult{}, err
	}
	var result store.OperatorVerificationAssignmentResult
	if err := decodeStrictJSONDocument(response.body, "verification assignment", &result); err != nil {
		return store.OperatorVerificationAssignmentResult{}, err
	}
	if err := validateVerifierMutationStatus("verification assignment", response, result.Replayed); err != nil {
		return store.OperatorVerificationAssignmentResult{}, err
	}
	if result.VerifierID != verifierID || result.JobID != body.JobID ||
		result.PreviewDigest != body.PreviewDigest ||
		result.VerifierName != body.ConfirmVerifierName {
		return store.OperatorVerificationAssignmentResult{}, errors.New(
			"operator client: verification assignment receipt does not match the request")
	}
	if result.AssignmentID == "" || result.MachineID == "" || result.AssignedBy == "" {
		return store.OperatorVerificationAssignmentResult{}, errors.New(
			"operator client: verification assignment receipt is missing its identities")
	}
	if result.AssignedAt.IsZero() || result.AssignedAt.Location() != time.UTC {
		return store.OperatorVerificationAssignmentResult{}, errors.New(
			"operator client: verification assignment assigned_at is not a UTC instant")
	}
	return result, nil
}

// validateVerificationAssignmentPreview recomputes the separation rule from the
// two identities the preview reports. The Hub enforces it, and a client that
// took the Hub's word for it could render a same-domain pairing as an
// independent one.
func validateVerificationAssignmentPreview(result store.OperatorVerificationAssignmentPreviewResult) error {
	if !validVerifierKind(result.VerifierKind) {
		return errors.New("operator client: verification assignment preview kind is unknown")
	}
	if result.MachineID == "" || result.FailureDomain == "" {
		return errors.New("operator client: verification assignment preview is missing an identity")
	}
	if result.FailureDomain == result.MachineID {
		return errors.New(
			"operator client: verification assignment preview pairs a verifier with its own failure domain")
	}
	if result.SeparationRule != store.OperatorVerifierSeparationRule ||
		result.SatisfiedBy != store.VerificationAssignmentSatisfiedBy ||
		!result.HandoutRequiresTerminalJob || result.CommandsSuppliedByHub ||
		result.GrantsDeploymentGate != store.VerifierKindGrantsDeploymentGate(result.VerifierKind) {
		return errors.New("operator client: verification assignment policy contract is invalid")
	}
	if !validSHA256Digest(result.PreviewDigest) {
		return errors.New("operator client: verification assignment preview_digest is not canonical SHA-256")
	}
	if result.PreviewedAt.IsZero() || result.PreviewedAt.Location() != time.UTC {
		return errors.New("operator client: verification assignment previewed_at is not a UTC instant")
	}
	if result.JobState == "" {
		return errors.New("operator client: verification assignment preview has no job state")
	}
	return nil
}
