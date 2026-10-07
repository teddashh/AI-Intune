package operatorclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/teddashh/AI-Intune/internal/store"
)

type MachineLifecycleReadResponse struct {
	MachineID                          string                      `json:"machine_id"`
	DisplayName                        string                      `json:"display_name"`
	State                              store.MachineLifecycleState `json:"state"`
	LifecycleRevision                  int64                       `json:"lifecycle_revision"`
	InDenominator                      bool                        `json:"in_denominator"`
	RetiredAt                          *time.Time                  `json:"retired_at,omitempty"`
	RegistryRetained                   bool                        `json:"registry_retained"`
	HistoryPreserved                   bool                        `json:"history_preserved"`
	Channel                            string                      `json:"channel,omitempty"`
	ChannelRevision                    int64                       `json:"channel_revision"`
	AgentCredentialPresent             bool                        `json:"agent_credential_present"`
	AgentAuthenticationAllowed         bool                        `json:"agent_authentication_allowed"`
	PendingEnrollmentTokenCount        int64                       `json:"pending_enrollment_token_count"`
	PendingEnrollmentTokenExpiredCount int64                       `json:"pending_enrollment_token_expired_count"`
	PendingEnrollmentRedemptionAllowed bool                        `json:"pending_enrollment_redemption_allowed"`
	ActiveJobCount                     int64                       `json:"active_job_count"`
	Meta                               ResponseMetadata            `json:"-"`
}

type MachineLifecyclePreviewRequest struct {
	DesiredState     store.MachineLifecycleState `json:"desired_state"`
	ExpectedRevision int64                       `json:"expected_revision"`
}

type MachineLifecycleImpact struct {
	InDenominatorBefore                bool     `json:"in_denominator_before"`
	InDenominatorAfter                 bool     `json:"in_denominator_after"`
	DenominatorDelta                   int64    `json:"denominator_delta"`
	RegistryRetained                   bool     `json:"registry_retained"`
	HistoryPreserved                   bool     `json:"history_preserved"`
	Channel                            string   `json:"channel,omitempty"`
	ChannelRevision                    int64    `json:"channel_revision"`
	ChannelPreserved                   bool     `json:"channel_preserved"`
	AgentCredentialPresent             bool     `json:"agent_credential_present"`
	AgentAuthenticationBefore          bool     `json:"agent_authentication_before"`
	AgentAuthenticationAfter           bool     `json:"agent_authentication_after"`
	PendingEnrollmentTokenCount        int64    `json:"pending_enrollment_token_count"`
	PendingEnrollmentTokenExpiredCount int64    `json:"pending_enrollment_token_expired_count"`
	PendingEnrollmentRedemptionBefore  bool     `json:"pending_enrollment_redemption_before"`
	PendingEnrollmentRedemptionAfter   bool     `json:"pending_enrollment_redemption_after"`
	ActiveJobCount                     int64    `json:"active_job_count"`
	Blockers                           []string `json:"blockers"`
}

type MachineLifecyclePreviewResponse struct {
	MachineID             string                      `json:"machine_id"`
	DisplayName           string                      `json:"display_name"`
	CurrentState          store.MachineLifecycleState `json:"current_state"`
	DesiredState          store.MachineLifecycleState `json:"desired_state"`
	LifecycleRevision     int64                       `json:"lifecycle_revision"`
	PreviewedAt           time.Time                   `json:"previewed_at"`
	OpenAgentSessionCount int64                       `json:"open_agent_session_count"`
	MachineLifecycleImpact
	PreviewDigest string `json:"preview_digest"`
}

type MachineLifecycleRequest struct {
	DesiredState       store.MachineLifecycleState `json:"desired_state"`
	ExpectedRevision   int64                       `json:"expected_revision"`
	ConfirmDisplayName string                      `json:"confirm_display_name"`
	PreviewDigest      string                      `json:"preview_digest"`
	Reason             string                      `json:"reason"`
}

type MachineLifecycleResponse struct {
	MachineID         string                      `json:"machine_id"`
	DisplayName       string                      `json:"display_name"`
	PreviousState     store.MachineLifecycleState `json:"previous_state"`
	State             store.MachineLifecycleState `json:"state"`
	LifecycleRevision int64                       `json:"lifecycle_revision"`
	Changed           bool                        `json:"changed"`
	NoOp              bool                        `json:"no_op"`
	TransitionEventID *int64                      `json:"transition_event_id,omitempty"`
	RetiredAt         *time.Time                  `json:"retired_at,omitempty"`
	AppliedAt         time.Time                   `json:"applied_at"`
	MachineLifecycleImpact
	PreviewDigest string           `json:"preview_digest"`
	Replayed      bool             `json:"replayed"`
	Meta          ResponseMetadata `json:"-"`
}

func (c *Client) MachineLifecycle(ctx context.Context, machineID string) (MachineLifecycleReadResponse, error) {
	req, err := c.newMachineOperatorRequest(ctx, http.MethodGet, machineID, "/lifecycle", nil)
	if err != nil {
		return MachineLifecycleReadResponse{}, err
	}
	response, err := c.doRaw(req)
	if err != nil {
		return MachineLifecycleReadResponse{}, err
	}
	if response.status != http.StatusOK {
		return MachineLifecycleReadResponse{}, fmt.Errorf("operator client: machine lifecycle read returned HTTP %d", response.status)
	}
	if err := validateJSONNoStoreResponse(response.header); err != nil {
		return MachineLifecycleReadResponse{}, err
	}
	result, err := decodeMachineLifecycleRead(response.body)
	if err != nil {
		return MachineLifecycleReadResponse{}, err
	}
	meta, err := lifecycleResponseMetadata(response.header, result.LifecycleRevision)
	if err != nil {
		return MachineLifecycleReadResponse{}, err
	}
	if meta.IdempotencyReplayed {
		return MachineLifecycleReadResponse{}, errors.New("operator client: lifecycle read cannot be an idempotency replay")
	}
	result.Meta = meta
	if err := validateMachineLifecycleRead(result, machineID); err != nil {
		return MachineLifecycleReadResponse{}, err
	}
	return result, nil
}

func (c *Client) PreviewMachineLifecycle(ctx context.Context, machineID string,
	body MachineLifecyclePreviewRequest,
) (MachineLifecyclePreviewResponse, error) {
	if !validLifecycleState(body.DesiredState) || body.ExpectedRevision < 0 {
		return MachineLifecyclePreviewResponse{}, errors.New("operator client: lifecycle preview requires active|retired desired_state and non-negative expected_revision")
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return MachineLifecyclePreviewResponse{}, fmt.Errorf("operator client: encode lifecycle preview: %w", err)
	}
	req, err := c.newMachineOperatorRequest(ctx, http.MethodPost, machineID, "/lifecycle-preview", bytes.NewReader(raw))
	if err != nil {
		return MachineLifecyclePreviewResponse{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	response, err := c.doRaw(req)
	if err != nil {
		return MachineLifecyclePreviewResponse{}, err
	}
	if response.status != http.StatusOK {
		return MachineLifecyclePreviewResponse{}, fmt.Errorf("operator client: lifecycle preview returned HTTP %d", response.status)
	}
	if err := validateJSONNoStoreResponse(response.header); err != nil {
		return MachineLifecyclePreviewResponse{}, err
	}
	if replayed, err := responseReplayEvidenceFromHeader(response.header); err != nil {
		return MachineLifecyclePreviewResponse{}, err
	} else if replayed {
		return MachineLifecyclePreviewResponse{}, errors.New("operator client: lifecycle preview cannot be an idempotency replay")
	}
	result, err := decodeMachineLifecyclePreview(response.body)
	if err != nil {
		return MachineLifecyclePreviewResponse{}, err
	}
	if err := validateMachineLifecyclePreview(result, machineID, body); err != nil {
		return MachineLifecyclePreviewResponse{}, err
	}
	return result, nil
}

func (c *Client) PutMachineLifecycle(ctx context.Context, machineID, idempotencyKey string,
	body MachineLifecycleRequest,
) (MachineLifecycleResponse, error) {
	if strings.TrimSpace(idempotencyKey) == "" || len(idempotencyKey) > 200 {
		return MachineLifecycleResponse{}, errors.New("operator client: lifecycle Idempotency-Key is required and cannot exceed 200 bytes")
	}
	if !validLifecycleState(body.DesiredState) || body.ExpectedRevision < 0 ||
		strings.TrimSpace(body.ConfirmDisplayName) == "" || !validSHA256Digest(body.PreviewDigest) ||
		strings.TrimSpace(body.Reason) == "" {
		return MachineLifecycleResponse{}, errors.New("operator client: lifecycle apply request is incomplete or invalid")
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return MachineLifecycleResponse{}, fmt.Errorf("operator client: encode lifecycle apply: %w", err)
	}
	req, err := c.newMachineOperatorRequest(ctx, http.MethodPut, machineID, "/lifecycle", bytes.NewReader(raw))
	if err != nil {
		return MachineLifecycleResponse{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", idempotencyKey)
	response, err := c.doRaw(req)
	if err != nil {
		return MachineLifecycleResponse{}, err
	}
	if response.status != http.StatusOK {
		return MachineLifecycleResponse{}, fmt.Errorf("operator client: lifecycle apply returned HTTP %d", response.status)
	}
	if err := validateJSONNoStoreResponse(response.header); err != nil {
		return MachineLifecycleResponse{}, err
	}
	result, err := decodeMachineLifecycleResponse(response.body)
	if err != nil {
		return MachineLifecycleResponse{}, err
	}
	meta, err := lifecycleResponseMetadata(response.header, result.LifecycleRevision)
	if err != nil {
		return MachineLifecycleResponse{}, err
	}
	if result.Replayed != meta.IdempotencyReplayed {
		return MachineLifecycleResponse{}, errors.New("operator client: lifecycle replay evidence differs between body and header")
	}
	result.Meta = meta
	if err := validateMachineLifecycleResponse(result, machineID, body); err != nil {
		return MachineLifecycleResponse{}, err
	}
	return result, nil
}

func lifecycleResponseMetadata(header http.Header, bodyRevision int64) (ResponseMetadata, error) {
	values := header.Values("ETag")
	if len(values) != 1 {
		return ResponseMetadata{}, fmt.Errorf("operator client: machine lifecycle response has %d ETag values", len(values))
	}
	raw := values[0]
	const prefix = `"lifecycle-revision-`
	if !strings.HasPrefix(raw, prefix) || !strings.HasSuffix(raw, `"`) {
		return ResponseMetadata{}, fmt.Errorf("operator client: invalid machine lifecycle ETag %q", raw)
	}
	n := strings.TrimSuffix(strings.TrimPrefix(raw, prefix), `"`)
	revision, err := strconv.ParseInt(n, 10, 64)
	if err != nil || revision < 0 || n != strconv.FormatInt(revision, 10) || revision != bodyRevision {
		return ResponseMetadata{}, fmt.Errorf("operator client: lifecycle ETag does not match canonical revision")
	}
	replayed, err := responseReplayEvidenceFromHeader(header)
	if err != nil {
		return ResponseMetadata{}, err
	}
	return ResponseMetadata{ETag: raw, ETagRevision: revision, IdempotencyReplayed: replayed}, nil
}

func lifecycleReadFields(result *MachineLifecycleReadResponse) map[string]any {
	return map[string]any{
		"machine_id": &result.MachineID, "display_name": &result.DisplayName, "state": &result.State,
		"lifecycle_revision": &result.LifecycleRevision,
		"in_denominator":     &result.InDenominator, "retired_at": &result.RetiredAt,
		"registry_retained": &result.RegistryRetained, "history_preserved": &result.HistoryPreserved,
		"channel": &result.Channel, "channel_revision": &result.ChannelRevision,
		"agent_credential_present":               &result.AgentCredentialPresent,
		"agent_authentication_allowed":           &result.AgentAuthenticationAllowed,
		"pending_enrollment_token_count":         &result.PendingEnrollmentTokenCount,
		"pending_enrollment_token_expired_count": &result.PendingEnrollmentTokenExpiredCount,
		"pending_enrollment_redemption_allowed":  &result.PendingEnrollmentRedemptionAllowed,
		"active_job_count":                       &result.ActiveJobCount,
	}
}

func decodeMachineLifecycleRead(raw []byte) (MachineLifecycleReadResponse, error) {
	var result MachineLifecycleReadResponse
	seen, err := decodeExactJSONObject(raw, "machine lifecycle read", lifecycleReadFields(&result))
	if err != nil {
		return result, err
	}
	if err := requireResponseFields("machine lifecycle read", seen,
		"machine_id", "display_name", "state", "lifecycle_revision", "in_denominator",
		"registry_retained", "history_preserved", "channel_revision", "agent_credential_present",
		"agent_authentication_allowed", "pending_enrollment_token_count",
		"pending_enrollment_token_expired_count", "pending_enrollment_redemption_allowed", "active_job_count"); err != nil {
		return result, err
	}
	return result, nil
}

func lifecycleImpactFields(impact *MachineLifecycleImpact) map[string]any {
	return map[string]any{
		"in_denominator_before": &impact.InDenominatorBefore,
		"in_denominator_after":  &impact.InDenominatorAfter, "denominator_delta": &impact.DenominatorDelta,
		"registry_retained": &impact.RegistryRetained, "history_preserved": &impact.HistoryPreserved,
		"channel": &impact.Channel, "channel_revision": &impact.ChannelRevision,
		"channel_preserved": &impact.ChannelPreserved, "agent_credential_present": &impact.AgentCredentialPresent,
		"agent_authentication_before":            &impact.AgentAuthenticationBefore,
		"agent_authentication_after":             &impact.AgentAuthenticationAfter,
		"pending_enrollment_token_count":         &impact.PendingEnrollmentTokenCount,
		"pending_enrollment_token_expired_count": &impact.PendingEnrollmentTokenExpiredCount,
		"pending_enrollment_redemption_before":   &impact.PendingEnrollmentRedemptionBefore,
		"pending_enrollment_redemption_after":    &impact.PendingEnrollmentRedemptionAfter,
		"active_job_count":                       &impact.ActiveJobCount, "blockers": &impact.Blockers,
	}
}

var lifecycleImpactRequired = []string{
	"in_denominator_before", "in_denominator_after", "denominator_delta",
	"registry_retained", "history_preserved", "channel_revision", "channel_preserved",
	"agent_credential_present", "agent_authentication_before", "agent_authentication_after",
	"pending_enrollment_token_count", "pending_enrollment_token_expired_count",
	"pending_enrollment_redemption_before", "pending_enrollment_redemption_after", "active_job_count", "blockers",
}

func decodeMachineLifecyclePreview(raw []byte) (MachineLifecyclePreviewResponse, error) {
	var result MachineLifecyclePreviewResponse
	fields := lifecycleImpactFields(&result.MachineLifecycleImpact)
	fields["machine_id"], fields["display_name"] = &result.MachineID, &result.DisplayName
	fields["current_state"], fields["desired_state"] = &result.CurrentState, &result.DesiredState
	fields["lifecycle_revision"], fields["previewed_at"] = &result.LifecycleRevision, &result.PreviewedAt
	fields["open_agent_session_count"] = &result.OpenAgentSessionCount
	fields["preview_digest"] = &result.PreviewDigest
	seen, err := decodeExactJSONObject(raw, "machine lifecycle preview", fields)
	if err != nil {
		return result, err
	}
	required := append([]string{"machine_id", "display_name", "current_state", "desired_state", "lifecycle_revision", "previewed_at", "open_agent_session_count", "preview_digest"}, lifecycleImpactRequired...)
	if err := requireResponseFields("machine lifecycle preview", seen, required...); err != nil {
		return result, err
	}
	return result, nil
}

func decodeMachineLifecycleResponse(raw []byte) (MachineLifecycleResponse, error) {
	var result MachineLifecycleResponse
	fields := lifecycleImpactFields(&result.MachineLifecycleImpact)
	fields["machine_id"], fields["display_name"] = &result.MachineID, &result.DisplayName
	fields["previous_state"], fields["state"] = &result.PreviousState, &result.State
	fields["lifecycle_revision"] = &result.LifecycleRevision
	fields["changed"], fields["no_op"] = &result.Changed, &result.NoOp
	fields["transition_event_id"], fields["retired_at"] = &result.TransitionEventID, &result.RetiredAt
	fields["applied_at"], fields["preview_digest"], fields["replayed"] = &result.AppliedAt, &result.PreviewDigest, &result.Replayed
	seen, err := decodeExactJSONObject(raw, "machine lifecycle apply", fields)
	if err != nil {
		return result, err
	}
	required := append([]string{"machine_id", "display_name", "previous_state", "state", "lifecycle_revision", "changed", "no_op", "applied_at", "preview_digest", "replayed"}, lifecycleImpactRequired...)
	if err := requireResponseFields("machine lifecycle apply", seen, required...); err != nil {
		return result, err
	}
	return result, nil
}

func validLifecycleState(value store.MachineLifecycleState) bool {
	return value == store.MachineLifecycleActive || value == store.MachineLifecycleRetired
}

func validLifecycleChannel(value string) bool {
	return value == "" || value == "canary" || value == "stable"
}

func validateLifecycleIdentity(machineID, displayName, wantedID string, revision, channelRevision int64, channel string) error {
	if machineID != wantedID || strings.TrimSpace(displayName) == "" || revision < 0 || channelRevision < 0 || !validLifecycleChannel(channel) {
		return errors.New("operator client: machine lifecycle identity or revision is invalid")
	}
	if err := validateMachineClientText("lifecycle machine_id", machineID, 256); err != nil {
		return err
	}
	if err := validateMachineClientText("lifecycle display_name", displayName, 256); err != nil {
		return err
	}
	return nil
}

func validateMachineLifecycleRead(result MachineLifecycleReadResponse, machineID string) error {
	if err := validateLifecycleIdentity(result.MachineID, result.DisplayName, machineID,
		result.LifecycleRevision, result.ChannelRevision, result.Channel); err != nil {
		return err
	}
	if !validLifecycleState(result.State) || !result.RegistryRetained || !result.HistoryPreserved ||
		result.InDenominator != (result.State == store.MachineLifecycleActive) ||
		result.AgentAuthenticationAllowed != (result.AgentCredentialPresent && result.State == store.MachineLifecycleActive) ||
		result.PendingEnrollmentTokenCount < 0 || result.PendingEnrollmentTokenExpiredCount < 0 ||
		result.PendingEnrollmentTokenExpiredCount > result.PendingEnrollmentTokenCount || result.ActiveJobCount < 0 ||
		result.PendingEnrollmentRedemptionAllowed != (result.State == store.MachineLifecycleActive && result.PendingEnrollmentTokenCount > result.PendingEnrollmentTokenExpiredCount) {
		return errors.New("operator client: machine lifecycle read facts are incoherent")
	}
	if result.State == store.MachineLifecycleRetired {
		if result.RetiredAt == nil || !canonicalLifecycleTime(*result.RetiredAt) {
			return errors.New("operator client: retired lifecycle read lacks canonical retired_at")
		}
	} else if result.RetiredAt != nil {
		return errors.New("operator client: active lifecycle read contains retired_at")
	}
	return nil
}

func validateMachineLifecycleImpact(impact MachineLifecycleImpact, before, after store.MachineLifecycleState) error {
	if impact.Blockers == nil || !impact.RegistryRetained || !impact.HistoryPreserved || !impact.ChannelPreserved ||
		impact.ChannelRevision < 0 || !validLifecycleChannel(impact.Channel) || impact.ActiveJobCount < 0 ||
		impact.PendingEnrollmentTokenCount < 0 || impact.PendingEnrollmentTokenExpiredCount < 0 ||
		impact.PendingEnrollmentTokenExpiredCount > impact.PendingEnrollmentTokenCount {
		return errors.New("operator client: lifecycle impact fields are invalid")
	}
	wantBefore := before == store.MachineLifecycleActive
	wantAfter := after == store.MachineLifecycleActive
	if impact.InDenominatorBefore != wantBefore || impact.InDenominatorAfter != wantAfter ||
		impact.DenominatorDelta != boolDelta(wantBefore, wantAfter) ||
		impact.AgentAuthenticationBefore != (impact.AgentCredentialPresent && before == store.MachineLifecycleActive) ||
		impact.AgentAuthenticationAfter != (impact.AgentCredentialPresent && after == store.MachineLifecycleActive) ||
		impact.PendingEnrollmentRedemptionBefore != (before == store.MachineLifecycleActive && impact.PendingEnrollmentTokenCount > impact.PendingEnrollmentTokenExpiredCount) ||
		impact.PendingEnrollmentRedemptionAfter != (after == store.MachineLifecycleActive && impact.PendingEnrollmentTokenCount > impact.PendingEnrollmentTokenExpiredCount) {
		return errors.New("operator client: lifecycle impact transition is incoherent")
	}
	wantBlocker := before == store.MachineLifecycleActive && after == store.MachineLifecycleRetired && impact.ActiveJobCount > 0
	if wantBlocker {
		if len(impact.Blockers) != 1 || impact.Blockers[0] != "nonterminal_jobs" {
			return errors.New("operator client: lifecycle preview omitted its nonterminal job blocker")
		}
	} else if len(impact.Blockers) != 0 {
		return errors.New("operator client: lifecycle response has an unexpected blocker")
	}
	return nil
}

func validateMachineLifecyclePreview(result MachineLifecyclePreviewResponse, machineID string,
	request MachineLifecyclePreviewRequest,
) error {
	if err := validateLifecycleIdentity(result.MachineID, result.DisplayName, machineID,
		result.LifecycleRevision, result.ChannelRevision, result.Channel); err != nil {
		return err
	}
	if !validLifecycleState(result.CurrentState) || result.DesiredState != request.DesiredState ||
		result.LifecycleRevision != request.ExpectedRevision || !canonicalLifecycleTime(result.PreviewedAt) ||
		result.OpenAgentSessionCount < 0 || !validSHA256Digest(result.PreviewDigest) {
		return errors.New("operator client: lifecycle preview identity, time, or digest is invalid")
	}
	return validateMachineLifecycleImpact(result.MachineLifecycleImpact, result.CurrentState, result.DesiredState)
}

func validateMachineLifecycleResponse(result MachineLifecycleResponse, machineID string,
	request MachineLifecycleRequest,
) error {
	if err := validateLifecycleIdentity(result.MachineID, result.DisplayName, machineID,
		result.LifecycleRevision, result.ChannelRevision, result.Channel); err != nil {
		return err
	}
	if result.DisplayName != request.ConfirmDisplayName || !validLifecycleState(result.PreviousState) ||
		result.State != request.DesiredState || result.PreviewDigest != request.PreviewDigest ||
		!canonicalLifecycleTime(result.AppliedAt) || result.Changed == result.NoOp ||
		result.Changed != (result.PreviousState != result.State) || len(result.Blockers) != 0 {
		return errors.New("operator client: lifecycle receipt identity or transition is incoherent")
	}
	if result.Changed {
		if request.ExpectedRevision == int64(^uint64(0)>>1) || result.LifecycleRevision != request.ExpectedRevision+1 ||
			result.TransitionEventID == nil || *result.TransitionEventID <= 0 {
			return errors.New("operator client: lifecycle transition receipt revision/event is invalid")
		}
	} else if result.LifecycleRevision != request.ExpectedRevision || result.TransitionEventID != nil {
		return errors.New("operator client: lifecycle no-op receipt changed authority")
	}
	if result.State == store.MachineLifecycleRetired {
		if result.RetiredAt == nil || !canonicalLifecycleTime(*result.RetiredAt) {
			return errors.New("operator client: retired lifecycle receipt lacks canonical retired_at")
		}
		if result.Changed && !result.RetiredAt.Equal(result.AppliedAt) {
			return errors.New("operator client: fresh retirement time differs from its applied_at")
		}
	} else if result.RetiredAt != nil {
		return errors.New("operator client: active lifecycle receipt contains retired_at")
	}
	return validateMachineLifecycleImpact(result.MachineLifecycleImpact, result.PreviousState, result.State)
}

func canonicalLifecycleTime(value time.Time) bool {
	return !value.IsZero() && value.Location() == time.UTC && value.Nanosecond() == 0
}

func boolDelta(before, after bool) int64 {
	var left, right int64
	if before {
		left = 1
	}
	if after {
		right = 1
	}
	return right - left
}
