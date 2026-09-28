package operatorclient

import (
	"bytes"
	"context"
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

type TailnetPeerIgnoreRequest struct {
	Action           string    `json:"action"`
	ExpiresAt        time.Time `json:"expires_at,omitempty"`
	ExpectedRevision int64     `json:"expected_revision"`
	ConfirmHostname  string    `json:"confirm_hostname"`
	PreviewDigest    string    `json:"preview_digest"`
	Reason           string    `json:"reason"`
}

func (c *Client) Tailnet(ctx context.Context) (operator.TailnetOverview, error) {
	req, err := c.newOperatorRequest(ctx, http.MethodGet, "/v1/operator/tailnet", nil)
	if err != nil {
		return operator.TailnetOverview{}, err
	}
	response, err := c.doRaw(req)
	if err != nil {
		return operator.TailnetOverview{}, err
	}
	if response.status != http.StatusOK {
		return operator.TailnetOverview{}, fmt.Errorf("operator client: tailnet read returned unexpected success status HTTP %d", response.status)
	}
	if err := validateTailnetResponseHeaders(response.header, false); err != nil {
		return operator.TailnetOverview{}, err
	}
	result, err := decodeTailnetOverview(response.body)
	if err != nil {
		return operator.TailnetOverview{}, err
	}
	if err := validateTailnetOverview(result); err != nil {
		return operator.TailnetOverview{}, err
	}
	return result, nil
}

func (c *Client) PreviewTailnetPeerIgnore(ctx context.Context,
	body operator.TailnetPeerIgnorePreviewRequest,
) (operator.TailnetPeerIgnorePreview, error) {
	if err := validateTailnetPreviewRequest(body); err != nil {
		return operator.TailnetPeerIgnorePreview{}, err
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return operator.TailnetPeerIgnorePreview{}, fmt.Errorf("operator client: encode tailnet preview request: %w", err)
	}
	req, err := c.newOperatorRequest(ctx, http.MethodPost, "/v1/operator/tailnet/peer-ignore-preview", bytes.NewReader(raw))
	if err != nil {
		return operator.TailnetPeerIgnorePreview{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	response, err := c.doRaw(req)
	if err != nil {
		return operator.TailnetPeerIgnorePreview{}, err
	}
	if response.status != http.StatusOK {
		return operator.TailnetPeerIgnorePreview{}, fmt.Errorf("operator client: tailnet preview returned unexpected success status HTTP %d", response.status)
	}
	if err := validateTailnetResponseHeaders(response.header, false); err != nil {
		return operator.TailnetPeerIgnorePreview{}, err
	}
	result, err := decodeTailnetPeerIgnorePreview(response.body)
	if err != nil {
		return operator.TailnetPeerIgnorePreview{}, err
	}
	if err := validateTailnetPeerIgnorePreview(result, body); err != nil {
		return operator.TailnetPeerIgnorePreview{}, err
	}
	return result, nil
}

func (c *Client) PutTailnetPeerIgnore(ctx context.Context, peerID, idempotencyKey string,
	body TailnetPeerIgnoreRequest,
) (store.OperatorTailnetPeerIgnoreResult, error) {
	if err := validateTailnetApplyRequest(peerID, idempotencyKey, body); err != nil {
		return store.OperatorTailnetPeerIgnoreResult{}, err
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return store.OperatorTailnetPeerIgnoreResult{}, fmt.Errorf("operator client: encode tailnet apply request: %w", err)
	}
	path := "/v1/operator/tailnet/peer-ignores/" + url.PathEscape(peerID)
	req, err := c.newOperatorRequest(ctx, http.MethodPut, path, bytes.NewReader(raw))
	if err != nil {
		return store.OperatorTailnetPeerIgnoreResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", idempotencyKey)
	response, err := c.doRaw(req)
	if err != nil {
		return store.OperatorTailnetPeerIgnoreResult{}, err
	}
	if response.status != http.StatusOK {
		return store.OperatorTailnetPeerIgnoreResult{}, fmt.Errorf("operator client: tailnet apply returned unexpected success status HTTP %d", response.status)
	}
	replayed, err := responseReplayEvidenceFromHeader(response.header)
	if err != nil {
		return store.OperatorTailnetPeerIgnoreResult{}, err
	}
	if err := validateTailnetResponseHeaders(response.header, replayed); err != nil {
		return store.OperatorTailnetPeerIgnoreResult{}, err
	}
	result, err := decodeTailnetPeerIgnoreResult(response.body)
	if err != nil {
		return store.OperatorTailnetPeerIgnoreResult{}, err
	}
	if result.Replayed != replayed {
		return store.OperatorTailnetPeerIgnoreResult{}, fmt.Errorf("operator client: tailnet replay evidence mismatch body=%t header=%t", result.Replayed, replayed)
	}
	if err := validateTailnetPeerIgnoreResult(result, peerID, body); err != nil {
		return store.OperatorTailnetPeerIgnoreResult{}, err
	}
	return result, nil
}

func validateTailnetResponseHeaders(header http.Header, replayAllowed bool) error {
	if err := validateJSONNoStoreResponse(header); err != nil {
		return err
	}
	replayed, err := responseReplayEvidenceFromHeader(header)
	if err != nil {
		return err
	}
	if replayed && !replayAllowed {
		return errors.New("operator client: tailnet read or preview cannot be an idempotency replay")
	}
	return nil
}

func validateTailnetPreviewRequest(body operator.TailnetPeerIgnorePreviewRequest) error {
	if !validTailnetPeerID(body.PeerID) || !validTailnetReason(body.Reason) ||
		(body.Action != "ignore" && body.Action != "unignore") {
		return errors.New("operator client: tailnet peer_id、action 與 reason 必須是 canonical values")
	}
	if body.Action == "ignore" && body.ExpiresAt.IsZero() {
		return errors.New("operator client: tailnet ignore expires_at 不可省略")
	}
	if body.Action == "unignore" && !body.ExpiresAt.IsZero() {
		return errors.New("operator client: tailnet unignore 不接受 expires_at")
	}
	return nil
}

func validateTailnetApplyRequest(peerID, key string, body TailnetPeerIgnoreRequest) error {
	if !validTailnetPeerID(peerID) || strings.TrimSpace(key) != key || key == "" || len(key) > 200 ||
		strings.TrimSpace(body.ConfirmHostname) != body.ConfirmHostname || body.ConfirmHostname == "" || len(body.ConfirmHostname) > 255 ||
		body.ExpectedRevision < 0 || !validSHA256Digest(body.PreviewDigest) {
		return errors.New("operator client: tailnet apply target、key、revision、confirmation 或 preview digest 不合法")
	}
	return validateTailnetPreviewRequest(operator.TailnetPeerIgnorePreviewRequest{
		PeerID: peerID, Action: body.Action, ExpiresAt: body.ExpiresAt, Reason: body.Reason,
	})
}

func validTailnetPeerID(value string) bool {
	return value != "" && strings.TrimSpace(value) == value && len(value) <= 128 &&
		value != "." && value != ".." && !strings.Contains(value, "/")
}

func validTailnetReason(value string) bool {
	return value != "" && strings.TrimSpace(value) == value && len(value) <= 500 &&
		!strings.ContainsAny(value, "\r\n")
}

func decodeTailnetOverview(raw []byte) (operator.TailnetOverview, error) {
	var result operator.TailnetOverview
	seen, err := decodeExactJSONObject(raw, "tailnet read", map[string]any{
		"available": &result.Available, "unavailable": &result.Unavailable,
		"observed_at": &result.ObservedAt, "peer_count": &result.PeerCount,
		"unenrolled": &result.Unenrolled, "online_but_silent": &result.OnlineButSilent,
		"retired_but_online": &result.RetiredButOnline, "ignored": &result.Ignored,
		"legacy_ignored_count": &result.LegacyIgnoredCount,
	})
	if err != nil {
		return result, err
	}
	if err := requireResponseFields("tailnet read", seen, "available", "peer_count", "unenrolled",
		"online_but_silent", "retired_but_online", "ignored", "legacy_ignored_count"); err != nil {
		return result, err
	}
	if result.Available {
		if err := requireResponseFields("tailnet read", seen, "observed_at"); err != nil {
			return result, err
		}
	} else if err := requireResponseFields("tailnet read", seen, "unavailable"); err != nil {
		return result, err
	}
	return result, nil
}

func validateTailnetOverview(result operator.TailnetOverview) error {
	if result.PeerCount < 0 || result.LegacyIgnoredCount < 0 || result.Unenrolled == nil ||
		result.OnlineButSilent == nil || result.RetiredButOnline == nil || result.Ignored == nil {
		return errors.New("operator client: tailnet overview contains invalid counts or null collections")
	}
	if !result.Available {
		if strings.TrimSpace(result.Unavailable) == "" || result.PeerCount != 0 || !result.ObservedAt.IsZero() ||
			len(result.Unenrolled)+len(result.OnlineButSilent)+len(result.RetiredButOnline) != 0 {
			return errors.New("operator client: unavailable tailnet overview contains contradictory evidence")
		}
		return nil
	}
	if result.Unavailable != "" || result.ObservedAt.IsZero() || result.PeerCount == 0 ||
		len(result.Unenrolled) > result.PeerCount {
		return errors.New("operator client: available tailnet overview contains contradictory evidence")
	}
	peerIDs := map[string]bool{}
	for _, peer := range result.Unenrolled {
		if !validTailnetPeerID(peer.StableID) || strings.TrimSpace(peer.Hostname) == "" || peerIDs[peer.StableID] {
			return errors.New("operator client: tailnet overview contains invalid or duplicate unenrolled peer")
		}
		peerIDs[peer.StableID] = true
	}
	ignoreIDs := map[string]bool{}
	for _, item := range result.Ignored {
		if !validTailnetPeerID(item.PeerID) || strings.TrimSpace(item.Hostname) == "" || !item.Active ||
			item.ExpiresAt.IsZero() || item.Revision <= 0 || ignoreIDs[item.PeerID] {
			return errors.New("operator client: tailnet overview contains invalid or duplicate ignore rule")
		}
		ignoreIDs[item.PeerID] = true
	}
	return nil
}

func decodeTailnetPeerIgnorePreview(raw []byte) (operator.TailnetPeerIgnorePreview, error) {
	var result operator.TailnetPeerIgnorePreview
	seen, err := decodeExactJSONObject(raw, "tailnet preview", map[string]any{
		"peer_id": &result.PeerID, "hostname": &result.Hostname, "action": &result.Action,
		"observed_at": &result.ObservedAt, "currently_ignored": &result.CurrentlyIgnored,
		"ignored_after": &result.IgnoredAfter, "expires_at": &result.ExpiresAt,
		"expected_revision": &result.ExpectedRevision, "reason": &result.Reason,
		"preview_digest": &result.PreviewDigest,
	})
	if err != nil {
		return result, err
	}
	if err := requireResponseFields("tailnet preview", seen, "peer_id", "hostname", "action", "observed_at",
		"currently_ignored", "ignored_after", "expires_at", "expected_revision", "reason", "preview_digest"); err != nil {
		return result, err
	}
	return result, nil
}

func validateTailnetPeerIgnorePreview(result operator.TailnetPeerIgnorePreview,
	request operator.TailnetPeerIgnorePreviewRequest,
) error {
	if result.PeerID != request.PeerID || result.Action != request.Action || result.Reason != request.Reason ||
		strings.TrimSpace(result.Hostname) == "" || result.ObservedAt.IsZero() || result.ExpectedRevision < 0 ||
		!validSHA256Digest(result.PreviewDigest) || result.IgnoredAfter != (result.Action == "ignore") {
		return errors.New("operator client: tailnet preview response does not match request or contract")
	}
	if result.Action == "ignore" && (!result.ExpiresAt.Equal(request.ExpiresAt.UTC()) || result.ExpiresAt.IsZero()) {
		return errors.New("operator client: tailnet preview expiry does not match request")
	}
	if result.Action == "unignore" && !result.ExpiresAt.IsZero() {
		return errors.New("operator client: tailnet unignore preview contains expiry")
	}
	return nil
}

func decodeTailnetPeerIgnoreResult(raw []byte) (store.OperatorTailnetPeerIgnoreResult, error) {
	var result store.OperatorTailnetPeerIgnoreResult
	seen, err := decodeExactJSONObject(raw, "tailnet apply", map[string]any{
		"peer_id": &result.PeerID, "hostname": &result.Hostname, "action": &result.Action,
		"previous_ignored": &result.PreviousIgnored, "ignored": &result.Ignored,
		"expires_at": &result.ExpiresAt, "revision": &result.Revision, "replayed": &result.Replayed,
	})
	if err != nil {
		return result, err
	}
	if err := requireResponseFields("tailnet apply", seen, "peer_id", "hostname", "action", "previous_ignored",
		"ignored", "expires_at", "revision", "replayed"); err != nil {
		return result, err
	}
	return result, nil
}

func validateTailnetPeerIgnoreResult(result store.OperatorTailnetPeerIgnoreResult, peerID string,
	request TailnetPeerIgnoreRequest,
) error {
	if result.PeerID != peerID || result.Hostname != request.ConfirmHostname || result.Action != request.Action ||
		result.Ignored != (request.Action == "ignore") || result.Revision <= 0 {
		return errors.New("operator client: tailnet apply response does not match request target or action")
	}
	if result.Ignored && !result.ExpiresAt.Equal(request.ExpiresAt.UTC()) {
		return errors.New("operator client: tailnet ignore response has invalid revision or expiry")
	}
	if !result.Ignored && !result.ExpiresAt.IsZero() {
		return errors.New("operator client: tailnet unignore response contains expiry")
	}
	return nil
}
