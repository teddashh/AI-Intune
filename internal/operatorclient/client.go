// Package operatorclient calls the Hub operator JSON API using tailnet identity
// or an explicitly configured HTTPS service token.
package operatorclient

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/teddashh/AI-Intune/internal/operatorendpoint"
)

const (
	maxResponseBytes = 1 << 20
	requestTimeout   = 30 * time.Second
	// UserAgent identifies the official HTTP CLI/client in the audit trail. It
	// is provenance only, never authentication or authorization.
	UserAgent = "clawctl-hub-operator/1"
)

type Client struct {
	token string
	base  *url.URL
	http  *http.Client
}

// New creates a client with a bounded timeout and refuses redirects. A control
// mutation must not silently resend its body to a different authority.
func New(baseURL string) (*Client, error) {
	httpClient := &http.Client{
		Timeout: requestTimeout,
	}
	return NewWithHTTPClient(baseURL, httpClient)
}

// NewWithHTTPClient lets tests and explicitly routed clients provide dialing
// policy without changing the operator origin contract. The supplied client is
// not mutated; its value is cloned, then proxy, timeout and redirect safety
// invariants are enforced by this package.
func NewWithHTTPClient(baseURL string, httpClient *http.Client) (*Client, error) {
	endpoint, err := operatorendpoint.ParseBaseURL(baseURL)
	if err != nil {
		return nil, fmt.Errorf("operator client: invalid base URL: %w", err)
	}
	if httpClient == nil {
		return nil, errors.New("operator client: HTTP client cannot be nil")
	}
	u, err := url.Parse(endpoint.BaseURL())
	if err != nil {
		return nil, fmt.Errorf("operator client: internal endpoint URL: %w", err)
	}
	client := *httpClient
	client.Transport, err = transportWithoutAmbientProxy(client.Transport)
	if err != nil {
		return nil, err
	}
	if client.Timeout <= 0 || client.Timeout > requestTimeout {
		client.Timeout = requestTimeout
	}
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &Client{base: u, http: &client}, nil
}

// transportWithoutAmbientProxy severs the standard library's implicit
// HTTP_PROXY/HTTPS_PROXY/ALL_PROXY input. Operator discovery selects one
// pinned tailnet origin; silently handing its requests (and mutation bodies)
// to an environment-selected intermediary would create a second authority.
//
// Only *http.Transport can be inspected and cloned safely here. An opaque
// RoundTripper can wrap http.DefaultTransport (and therefore retain
// ProxyFromEnvironment), so accepting one would make the no-ambient-proxy
// invariant unverifiable.
func transportWithoutAmbientProxy(roundTripper http.RoundTripper) (http.RoundTripper, error) {
	if roundTripper == nil {
		transport, ok := http.DefaultTransport.(*http.Transport)
		if !ok || transport == nil {
			return nil, errors.New("operator client: default HTTP transport is not usable")
		}
		clone := transport.Clone()
		clone.Proxy = nil
		return clone, nil
	}
	if transport, ok := roundTripper.(*http.Transport); ok {
		if transport == nil {
			return nil, errors.New("operator client: HTTP transport cannot be nil")
		}
		clone := transport.Clone()
		clone.Proxy = nil
		return clone, nil
	}
	return nil, fmt.Errorf("operator client: unsupported HTTP RoundTripper %T; only *http.Transport can prove ambient proxy is disabled", roundTripper)
}

type MachineChannelRequest struct {
	Channel            string `json:"channel"`
	ExpectedRevision   int64  `json:"expected_revision"`
	ConfirmDisplayName string `json:"confirm_display_name"`
}

type ResponseMetadata struct {
	ETag                string
	ETagRevision        int64
	IdempotencyReplayed bool
}

type MachineChannelResponse struct {
	MachineID       string           `json:"machine_id"`
	DisplayName     string           `json:"display_name"`
	PreviousChannel string           `json:"previous_channel"`
	Channel         string           `json:"channel"`
	Revision        int64            `json:"revision"`
	Replayed        bool             `json:"replayed"`
	Meta            ResponseMetadata `json:"-"`
}

// EnrollmentTokenPreviewRequest describes the exact material whose preview
// digest must be carried into CreateEnrollToken. TTLSeconds is deliberately an
// integer on the wire: clients must not disagree about duration spelling.
type EnrollmentTokenPreviewRequest struct {
	DisplayName string `json:"display_name"`
	TTLSeconds  int64  `json:"ttl_seconds"`
}

type EnrollmentTokenPreviewResponse struct {
	DisplayName                string    `json:"display_name"`
	TTLSeconds                 int64     `json:"ttl_seconds"`
	PreviewedAt                time.Time `json:"previewed_at"`
	ExpiresAtIfCreatedNow      time.Time `json:"expires_at_if_created_now"`
	CreatesExpectedMachine     bool      `json:"creates_expected_machine"`
	InitialState               string    `json:"initial_state"`
	RevocationKeepsRegistryRow bool      `json:"revocation_keeps_registry_row"`
	SecretDelivery             string    `json:"secret_delivery"`
	PreviewDigest              string    `json:"preview_digest"`

	// 註冊上限現在的樣子。⚠ 它們不進 preview digest：一份把分母釘進去的預覽會在
	// 按下送出之前就過期，而把上限「調高」也會作廢它——那是完全相反的方向。真正
	// 擋人的那一次數，發生在開票的同一筆交易裡。
	LimitSet         bool `json:"limit_set"`
	LimitMaxMachines int  `json:"limit_max_machines"`
	InDenominator    int  `json:"in_denominator"`
	Headroom         int  `json:"headroom"`
	AtLimit          bool `json:"at_limit"`
}

type EnrollmentTokenCreateRequest struct {
	DisplayName   string `json:"display_name"`
	TTLSeconds    int64  `json:"ttl_seconds"`
	PreviewDigest string `json:"preview_digest"`
	Reason        string `json:"reason"`
}

type EnrollmentTokenResponse struct {
	DisplayName      string    `json:"display_name"`
	MachineID        string    `json:"machine_id"`
	TTLSeconds       int64     `json:"ttl_seconds"`
	CreatedAt        time.Time `json:"created_at"`
	ExpiresAt        time.Time `json:"expires_at"`
	EnrollmentToken  string    `json:"enrollment_token,omitempty"`
	SecretAvailable  bool      `json:"secret_available"`
	Replayed         bool      `json:"replayed"`
	RecoveryRequired bool      `json:"recovery_required"`
	RecoveryAction   string    `json:"recovery_action"`
	PreviewDigest    string    `json:"preview_digest"`
}

// APIError preserves the server's stable code/message pair for automation.
type APIError struct {
	StatusCode int    `json:"-"`
	Code       string `json:"code"`
	Message    string `json:"message"`
	Replayed   bool   `json:"-"`
}

func (e *APIError) Error() string {
	return fmt.Sprintf("operator API HTTP %d %s: %s", e.StatusCode, e.Code, e.Message)
}

func (c *Client) GetMachineChannel(ctx context.Context, machineID string) (MachineChannelResponse, error) {
	req, err := c.newRequest(ctx, http.MethodGet, machineID, nil)
	if err != nil {
		return MachineChannelResponse{}, err
	}
	result, err := c.do(req)
	if err != nil {
		return MachineChannelResponse{}, err
	}
	if err := validateMachineChannelResponse(result, machineID); err != nil {
		return MachineChannelResponse{}, err
	}
	if result.PreviousChannel != result.Channel || result.Replayed || result.Meta.IdempotencyReplayed {
		return MachineChannelResponse{}, errors.New("operator client: GET response contains mutation/replay semantics")
	}
	return result, nil
}

func (c *Client) PutMachineChannel(ctx context.Context, machineID, idempotencyKey string, body MachineChannelRequest) (MachineChannelResponse, error) {
	if strings.TrimSpace(idempotencyKey) == "" {
		return MachineChannelResponse{}, errors.New("operator client: Idempotency-Key is required")
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return MachineChannelResponse{}, fmt.Errorf("operator client: encode machine channel request: %w", err)
	}
	req, err := c.newRequest(ctx, http.MethodPut, machineID, bytes.NewReader(raw))
	if err != nil {
		return MachineChannelResponse{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", idempotencyKey)
	result, err := c.do(req)
	if err != nil {
		return MachineChannelResponse{}, err
	}
	if err := validateMachineChannelResponse(result, machineID); err != nil {
		return MachineChannelResponse{}, err
	}
	wantedChannel, validRequestChannel := canonicalRequestedChannel(body.Channel)
	if !validRequestChannel || result.Channel != wantedChannel {
		return MachineChannelResponse{}, fmt.Errorf(
			"operator client: response channel %q does not match requested channel %q", result.Channel, body.Channel)
	}
	if result.DisplayName != body.ConfirmDisplayName {
		return MachineChannelResponse{}, errors.New("operator client: response display_name does not match typed confirmation")
	}
	if body.ExpectedRevision < 0 ||
		(result.Revision != body.ExpectedRevision && result.Revision != body.ExpectedRevision+1) {
		return MachineChannelResponse{}, fmt.Errorf(
			"operator client: response revision %d is not expected unchanged-or-next revision from %d",
			result.Revision, body.ExpectedRevision)
	}
	if result.Revision == body.ExpectedRevision && result.PreviousChannel != result.Channel {
		return MachineChannelResponse{}, errors.New("operator client: unchanged revision response claims a channel transition")
	}
	if result.Revision == body.ExpectedRevision+1 && result.PreviousChannel == result.Channel {
		return MachineChannelResponse{}, errors.New("operator client: incremented revision response has no channel transition")
	}
	return result, nil
}

// PreviewEnrollToken gets the server-authored impact statement and digest
// that CreateEnrollToken requires. It is a POST because its input is a strict
// JSON object, but it does not carry an idempotency key or create state.
func (c *Client) PreviewEnrollToken(ctx context.Context, body EnrollmentTokenPreviewRequest) (EnrollmentTokenPreviewResponse, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return EnrollmentTokenPreviewResponse{}, fmt.Errorf("operator client: encode enrollment token preview request: %w", err)
	}
	req, err := c.newOperatorRequest(ctx, http.MethodPost, "/v1/operator/enrollment-tokens/preview", bytes.NewReader(raw))
	if err != nil {
		return EnrollmentTokenPreviewResponse{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	response, err := c.doRaw(req)
	if err != nil {
		return EnrollmentTokenPreviewResponse{}, err
	}
	if response.status != http.StatusOK {
		return EnrollmentTokenPreviewResponse{}, fmt.Errorf(
			"operator client: enrollment token preview returned unexpected success status HTTP %d", response.status)
	}
	if err := validateJSONNoStoreResponse(response.header); err != nil {
		return EnrollmentTokenPreviewResponse{}, err
	}
	if replayed, err := responseReplayEvidenceFromHeader(response.header); err != nil {
		return EnrollmentTokenPreviewResponse{}, err
	} else if replayed {
		return EnrollmentTokenPreviewResponse{}, errors.New("operator client: enrollment token preview cannot be an idempotency replay")
	}
	result, err := decodeEnrollmentTokenPreviewResponse(response.body)
	if err != nil {
		return EnrollmentTokenPreviewResponse{}, err
	}
	if err := validateEnrollmentTokenPreviewResponse(result, body); err != nil {
		return EnrollmentTokenPreviewResponse{}, err
	}
	return result, nil
}

// CreateEnrollToken accepts exactly two success shapes. A fresh creation is
// HTTP 201 and carries the secret once. An idempotent replay is HTTP 200,
// carries no secret, and tells the operator to revoke and reissue when the
// first response was lost. Treating those as interchangeable would turn a
// network retry into a false claim that the original secret can be recovered.
func (c *Client) CreateEnrollToken(ctx context.Context, idempotencyKey string, body EnrollmentTokenCreateRequest) (EnrollmentTokenResponse, error) {
	if strings.TrimSpace(idempotencyKey) == "" {
		return EnrollmentTokenResponse{}, errors.New("operator client: Idempotency-Key is required")
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return EnrollmentTokenResponse{}, fmt.Errorf("operator client: encode enrollment token create request: %w", err)
	}
	req, err := c.newOperatorRequest(ctx, http.MethodPost, "/v1/operator/enrollment-tokens", bytes.NewReader(raw))
	if err != nil {
		return EnrollmentTokenResponse{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", idempotencyKey)
	response, err := c.doRaw(req)
	if err != nil {
		return EnrollmentTokenResponse{}, err
	}
	if response.status != http.StatusCreated && response.status != http.StatusOK {
		return EnrollmentTokenResponse{}, fmt.Errorf(
			"operator client: enrollment token create returned unexpected success status HTTP %d", response.status)
	}
	if err := validateJSONNoStoreResponse(response.header); err != nil {
		return EnrollmentTokenResponse{}, err
	}
	replayedHeader, err := responseReplayEvidenceFromHeader(response.header)
	if err != nil {
		return EnrollmentTokenResponse{}, err
	}
	result, tokenFieldPresent, err := decodeEnrollmentTokenResponse(response.body)
	if err != nil {
		return EnrollmentTokenResponse{}, err
	}
	if result.Replayed != replayedHeader {
		return EnrollmentTokenResponse{}, fmt.Errorf(
			"operator client: enrollment token replay evidence mismatch body=%t header=%t",
			result.Replayed, replayedHeader)
	}
	if err := validateEnrollmentTokenResponse(result, tokenFieldPresent, response.status, body); err != nil {
		return EnrollmentTokenResponse{}, err
	}
	return result, nil
}

func validateMachineChannelResponse(result MachineChannelResponse, requestedMachineID string) error {
	if result.MachineID != requestedMachineID {
		return errors.New("operator client: response machine_id does not match request target")
	}
	if strings.TrimSpace(result.DisplayName) == "" {
		return errors.New("operator client: response display_name is empty")
	}
	if !validMachineChannel(result.Channel) || !validMachineChannel(result.PreviousChannel) {
		return errors.New("operator client: response contains invalid channel")
	}
	return nil
}

func validMachineChannel(channel string) bool {
	return channel == "" || channel == "canary" || channel == "stable"
}

func canonicalRequestedChannel(channel string) (string, bool) {
	switch channel {
	case "canary", "stable":
		return channel, true
	case "none":
		return "", true
	default:
		return "", false
	}
}

func (c *Client) newRequest(ctx context.Context, method, machineID string, body io.Reader) (*http.Request, error) {
	if strings.TrimSpace(machineID) == "" || strings.Contains(machineID, "/") || machineID == "." || machineID == ".." {
		return nil, errors.New("operator client: machine_id cannot be empty, a dot segment, or contain slashes")
	}
	// JoinPath treats percent escapes in its elements as already encoded. A
	// machine ID such as "%61bc" could therefore be sent as "abc", mutate that
	// real target, and only fail the response-identity check afterwards. Escape
	// the opaque ID exactly once and concatenate beneath the already-validated
	// origin, which has no base path/query/fragment.
	endpoint := "/v1/operator/machines/" + url.PathEscape(machineID) + "/channel"
	return c.newOperatorRequest(ctx, method, endpoint, body)
}

func (c *Client) newOperatorRequest(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	endpoint := c.base.String() + path
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return nil, fmt.Errorf("operator client: build request: %w", err)
	}
	if c.token != "" {
		if !strings.HasPrefix(path, "/v1/operator/") {
			return nil, errors.New("service token cannot access this route")
		}
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", UserAgent)
	return req, nil
}

type operatorRawResponse struct {
	status int
	header http.Header
	body   []byte
}

func (c *Client) doRaw(req *http.Request) (operatorRawResponse, error) {
	return c.doRawWithLimit(req, maxResponseBytes, "1 MiB")
}

func (c *Client) doRawWithLimit(req *http.Request, limit int64, limitLabel string) (operatorRawResponse, error) {
	resp, err := c.http.Do(req)
	if err != nil {
		return operatorRawResponse{}, fmt.Errorf("operator client: %s %s: %w", req.Method, req.URL.Path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return operatorRawResponse{}, fmt.Errorf("operator client: read response: %w", err)
	}
	if int64(len(raw)) > limit {
		return operatorRawResponse{}, fmt.Errorf("operator client: response exceeds %s", limitLabel)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		replayed, replayErr := responseReplayEvidence(resp)
		if replayErr != nil {
			return operatorRawResponse{}, replayErr
		}
		apiErr := &APIError{StatusCode: resp.StatusCode, Replayed: replayed}
		if err := json.Unmarshal(raw, apiErr); err != nil || apiErr.Code == "" || apiErr.Message == "" {
			apiErr.Code = "HTTP_ERROR"
			apiErr.Message = http.StatusText(resp.StatusCode)
			if apiErr.Message == "" {
				apiErr.Message = "non-2xx response"
			}
		}
		return operatorRawResponse{}, apiErr
	}
	return operatorRawResponse{status: resp.StatusCode, header: resp.Header.Clone(), body: raw}, nil
}

func (c *Client) do(req *http.Request) (MachineChannelResponse, error) {
	response, err := c.doRaw(req)
	if err != nil {
		return MachineChannelResponse{}, err
	}
	if response.status != http.StatusOK {
		return MachineChannelResponse{}, fmt.Errorf("operator client: unexpected success status HTTP %d", response.status)
	}
	mediaType, _, err := mime.ParseMediaType(response.header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return MachineChannelResponse{}, errors.New("operator client: success response Content-Type must be application/json")
	}
	result, err := decodeMachineChannelResponse(response.body)
	if err != nil {
		return MachineChannelResponse{}, err
	}
	meta, err := responseMetadataFromHeader(response.header, result.Revision)
	if err != nil {
		return MachineChannelResponse{}, err
	}
	result.Meta = meta
	if result.Replayed != meta.IdempotencyReplayed {
		return MachineChannelResponse{}, fmt.Errorf(
			"operator client: replay evidence mismatch body=%t header=%t", result.Replayed, meta.IdempotencyReplayed)
	}
	return result, nil
}

func responseMetadata(resp *http.Response, bodyRevision int64) (ResponseMetadata, error) {
	return responseMetadataFromHeader(resp.Header, bodyRevision)
}

func responseMetadataFromHeader(header http.Header, bodyRevision int64) (ResponseMetadata, error) {
	values := header.Values("ETag")
	if len(values) != 1 {
		return ResponseMetadata{}, fmt.Errorf("operator client: machine channel response has %d ETag values", len(values))
	}
	raw := values[0]
	const prefix = `"channel-revision-`
	if !strings.HasPrefix(raw, prefix) || !strings.HasSuffix(raw, `"`) {
		return ResponseMetadata{}, fmt.Errorf("operator client: missing or invalid machine channel ETag %q", raw)
	}
	n := strings.TrimSuffix(strings.TrimPrefix(raw, prefix), `"`)
	revision, err := strconv.ParseInt(n, 10, 64)
	if err != nil || revision < 0 || n != strconv.FormatInt(revision, 10) {
		return ResponseMetadata{}, fmt.Errorf("operator client: invalid machine channel ETag %q", raw)
	}
	if revision != bodyRevision {
		return ResponseMetadata{}, fmt.Errorf(
			"operator client: ETag revision %d does not match response revision %d", revision, bodyRevision)
	}
	replayed, err := responseReplayEvidenceFromHeader(header)
	if err != nil {
		return ResponseMetadata{}, err
	}
	return ResponseMetadata{ETag: raw, ETagRevision: revision, IdempotencyReplayed: replayed}, nil
}

func responseReplayEvidence(resp *http.Response) (bool, error) {
	return responseReplayEvidenceFromHeader(resp.Header)
}

func responseReplayEvidenceFromHeader(header http.Header) (bool, error) {
	values := header.Values("Idempotency-Replayed")
	switch len(values) {
	case 0:
		return false, nil
	case 1:
		if values[0] == "true" {
			return true, nil
		}
		return false, fmt.Errorf("operator client: invalid Idempotency-Replayed value %q", values[0])
	default:
		return false, fmt.Errorf("operator client: response has %d Idempotency-Replayed values", len(values))
	}
}

func decodeMachineChannelResponse(raw []byte) (MachineChannelResponse, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	first, err := dec.Token()
	if err != nil {
		return MachineChannelResponse{}, fmt.Errorf("operator client: decode success response: %w", err)
	}
	if delim, ok := first.(json.Delim); !ok || delim != '{' {
		return MachineChannelResponse{}, errors.New("operator client: success response must be a JSON object")
	}
	var result MachineChannelResponse
	seen := make(map[string]bool, 6)
	for dec.More() {
		token, err := dec.Token()
		if err != nil {
			return MachineChannelResponse{}, fmt.Errorf("operator client: decode success response field: %w", err)
		}
		name, ok := token.(string)
		if !ok {
			return MachineChannelResponse{}, errors.New("operator client: success response field name is not a string")
		}
		if seen[name] {
			return MachineChannelResponse{}, fmt.Errorf("operator client: duplicate success response field %q", name)
		}
		seen[name] = true
		var target any
		switch name {
		case "machine_id":
			target = &result.MachineID
		case "display_name":
			target = &result.DisplayName
		case "previous_channel":
			target = &result.PreviousChannel
		case "channel":
			target = &result.Channel
		case "revision":
			target = &result.Revision
		case "replayed":
			target = &result.Replayed
		default:
			return MachineChannelResponse{}, fmt.Errorf("operator client: unknown success response field %q", name)
		}
		var field json.RawMessage
		if err := dec.Decode(&field); err != nil {
			return MachineChannelResponse{}, fmt.Errorf("operator client: decode success response field %q: %w", name, err)
		}
		if bytes.Equal(bytes.TrimSpace(field), []byte("null")) {
			return MachineChannelResponse{}, fmt.Errorf("operator client: success response field %q cannot be null", name)
		}
		if err := json.Unmarshal(field, target); err != nil {
			return MachineChannelResponse{}, fmt.Errorf("operator client: invalid success response field %q: %w", name, err)
		}
	}
	last, err := dec.Token()
	if err != nil {
		return MachineChannelResponse{}, fmt.Errorf("operator client: close success response object: %w", err)
	}
	if delim, ok := last.(json.Delim); !ok || delim != '}' {
		return MachineChannelResponse{}, errors.New("operator client: success response object did not close")
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return MachineChannelResponse{}, errors.New("operator client: success response has trailing JSON")
	}
	for _, required := range []string{"machine_id", "display_name", "previous_channel", "channel", "revision", "replayed"} {
		if !seen[required] {
			return MachineChannelResponse{}, fmt.Errorf("operator client: success response is missing field %q", required)
		}
	}
	return result, nil
}

func validateJSONNoStoreResponse(header http.Header) error {
	mediaType, _, err := mime.ParseMediaType(header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return errors.New("operator client: operator JSON success response Content-Type must be application/json")
	}
	for _, value := range header.Values("Cache-Control") {
		for _, directive := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(directive), "no-store") {
				return nil
			}
		}
	}
	return errors.New("operator client: operator JSON response must be Cache-Control: no-store")
}

func decodeEnrollmentTokenPreviewResponse(raw []byte) (EnrollmentTokenPreviewResponse, error) {
	var result EnrollmentTokenPreviewResponse
	seen, err := decodeExactJSONObject(raw, "enrollment token preview", map[string]any{
		"display_name":                  &result.DisplayName,
		"ttl_seconds":                   &result.TTLSeconds,
		"previewed_at":                  &result.PreviewedAt,
		"expires_at_if_created_now":     &result.ExpiresAtIfCreatedNow,
		"creates_expected_machine":      &result.CreatesExpectedMachine,
		"initial_state":                 &result.InitialState,
		"revocation_keeps_registry_row": &result.RevocationKeepsRegistryRow,
		"secret_delivery":               &result.SecretDelivery,
		"preview_digest":                &result.PreviewDigest,
		"limit_set":                     &result.LimitSet,
		"limit_max_machines":            &result.LimitMaxMachines,
		"in_denominator":                &result.InDenominator,
		"headroom":                      &result.Headroom,
		"at_limit":                      &result.AtLimit,
	})
	if err != nil {
		return EnrollmentTokenPreviewResponse{}, err
	}
	if err := requireResponseFields("enrollment token preview", seen,
		"display_name", "ttl_seconds", "previewed_at", "expires_at_if_created_now",
		"creates_expected_machine", "initial_state", "revocation_keeps_registry_row",
		"secret_delivery", "preview_digest",
		"limit_set", "limit_max_machines", "in_denominator", "headroom", "at_limit"); err != nil {
		return EnrollmentTokenPreviewResponse{}, err
	}
	return result, nil
}

func validateEnrollmentTokenPreviewResponse(result EnrollmentTokenPreviewResponse, request EnrollmentTokenPreviewRequest) error {
	if result.DisplayName != request.DisplayName {
		return errors.New("operator client: enrollment token preview display_name does not match request")
	}
	if result.TTLSeconds != request.TTLSeconds || result.TTLSeconds <= 0 {
		return errors.New("operator client: enrollment token preview ttl_seconds does not match request")
	}
	if result.PreviewedAt.IsZero() || result.ExpiresAtIfCreatedNow.IsZero() ||
		result.ExpiresAtIfCreatedNow.Sub(result.PreviewedAt) != time.Duration(result.TTLSeconds)*time.Second {
		return errors.New("operator client: enrollment token preview expiry does not match ttl_seconds")
	}
	if !result.CreatesExpectedMachine || result.InitialState != "never_reported" ||
		!result.RevocationKeepsRegistryRow || result.SecretDelivery != "first-response-only" {
		return errors.New("operator client: enrollment token preview impact contract is invalid")
	}
	if !validSHA256Digest(result.PreviewDigest) {
		return errors.New("operator client: enrollment token preview_digest is not canonical SHA-256")
	}
	// ⚠ 一份說「還收得下」但其實已經到上限的預覽，會讓人按下去才發現開不了票。
	// at_limit 是這份預覽上唯一一個會讓人改變下一步的欄位，所以自己再判一次。
	if result.InDenominator < 0 || result.Headroom < 0 || result.LimitMaxMachines < 0 {
		return errors.New("operator client: enrollment token preview enrollment limit has negative values")
	}
	if !result.LimitSet {
		if result.LimitMaxMachines != 0 || result.Headroom != 0 || result.AtLimit {
			return errors.New("operator client: enrollment token preview indicates no limit, but carries limit details")
		}
		return nil
	}
	headroom := result.LimitMaxMachines - result.InDenominator
	if headroom < 0 {
		headroom = 0
	}
	if result.Headroom != headroom || result.AtLimit != (result.InDenominator >= result.LimitMaxMachines) {
		return fmt.Errorf("operator client: limit is %d machines, enrolled %d machines, preview says %d machines can still be enrolled, at_limit=%t",
			result.LimitMaxMachines, result.InDenominator, result.Headroom, result.AtLimit)
	}
	return nil
}

func decodeEnrollmentTokenResponse(raw []byte) (EnrollmentTokenResponse, bool, error) {
	var result EnrollmentTokenResponse
	seen, err := decodeExactJSONObject(raw, "enrollment token create", map[string]any{
		"machine_id":        &result.MachineID,
		"display_name":      &result.DisplayName,
		"created_at":        &result.CreatedAt,
		"expires_at":        &result.ExpiresAt,
		"ttl_seconds":       &result.TTLSeconds,
		"enrollment_token":  &result.EnrollmentToken,
		"secret_available":  &result.SecretAvailable,
		"replayed":          &result.Replayed,
		"recovery_required": &result.RecoveryRequired,
		"recovery_action":   &result.RecoveryAction,
		"preview_digest":    &result.PreviewDigest,
	})
	if err != nil {
		return EnrollmentTokenResponse{}, false, err
	}
	if err := requireResponseFields("enrollment token create", seen,
		"machine_id", "display_name", "created_at", "expires_at", "ttl_seconds",
		"secret_available", "replayed", "recovery_required", "recovery_action",
		"preview_digest"); err != nil {
		return EnrollmentTokenResponse{}, false, err
	}
	return result, seen["enrollment_token"], nil
}

func validateEnrollmentTokenResponse(result EnrollmentTokenResponse, tokenFieldPresent bool,
	status int, request EnrollmentTokenCreateRequest,
) error {
	if result.DisplayName != request.DisplayName {
		return errors.New("operator client: enrollment token response display_name does not match request")
	}
	if result.TTLSeconds != request.TTLSeconds || result.TTLSeconds <= 0 {
		return errors.New("operator client: enrollment token response ttl_seconds does not match request")
	}
	if result.PreviewDigest != request.PreviewDigest || !validSHA256Digest(result.PreviewDigest) {
		return errors.New("operator client: enrollment token response preview_digest does not match request")
	}
	if result.MachineID == "" || strings.TrimSpace(result.MachineID) != result.MachineID {
		return errors.New("operator client: enrollment token response machine_id is empty or noncanonical")
	}
	if result.CreatedAt.IsZero() || result.ExpiresAt.IsZero() ||
		result.ExpiresAt.Sub(result.CreatedAt) != time.Duration(result.TTLSeconds)*time.Second {
		return errors.New("operator client: enrollment token response expiry does not match ttl_seconds")
	}

	if status == http.StatusCreated {
		if result.Replayed {
			return errors.New("operator client: HTTP 201 enrollment token response claims replay")
		}
		if !tokenFieldPresent || !validEnrollmentToken(result.EnrollmentToken) || !result.SecretAvailable {
			return errors.New("operator client: fresh enrollment token response does not contain one valid available secret")
		}
		if result.RecoveryRequired || result.RecoveryAction != "" {
			return errors.New("operator client: fresh enrollment token response incorrectly requires recovery")
		}
		return nil
	}

	if !result.Replayed {
		return errors.New("operator client: HTTP 200 enrollment token response is not labelled replay")
	}
	if tokenFieldPresent || result.EnrollmentToken != "" || result.SecretAvailable {
		return errors.New("operator client: replayed enrollment token response exposed or claimed a secret")
	}
	if !result.RecoveryRequired || result.RecoveryAction != "revoke_and_reissue" {
		return errors.New("operator client: replayed enrollment token response lacks exact recovery instructions")
	}
	return nil
}

func decodeExactJSONObject(raw []byte, label string, fields map[string]any) (map[string]bool, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	first, err := dec.Token()
	if err != nil {
		return nil, fmt.Errorf("operator client: decode %s response: %w", label, err)
	}
	if delim, ok := first.(json.Delim); !ok || delim != '{' {
		return nil, fmt.Errorf("operator client: %s response must be a JSON object", label)
	}
	seen := make(map[string]bool, len(fields))
	for dec.More() {
		token, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("operator client: decode %s response field: %w", label, err)
		}
		name, ok := token.(string)
		if !ok {
			return nil, fmt.Errorf("operator client: %s response field name is not a string", label)
		}
		if seen[name] {
			return nil, fmt.Errorf("operator client: duplicate %s response field %q", label, name)
		}
		target, ok := fields[name]
		if !ok {
			return nil, fmt.Errorf("operator client: unknown %s response field %q", label, name)
		}
		seen[name] = true
		var field json.RawMessage
		if err := dec.Decode(&field); err != nil {
			return nil, fmt.Errorf("operator client: decode %s response field %q: %w", label, name, err)
		}
		if bytes.Equal(bytes.TrimSpace(field), []byte("null")) {
			return nil, fmt.Errorf("operator client: %s response field %q cannot be null", label, name)
		}
		if err := json.Unmarshal(field, target); err != nil {
			return nil, fmt.Errorf("operator client: invalid %s response field %q: %w", label, name, err)
		}
	}
	last, err := dec.Token()
	if err != nil {
		return nil, fmt.Errorf("operator client: close %s response object: %w", label, err)
	}
	if delim, ok := last.(json.Delim); !ok || delim != '}' {
		return nil, fmt.Errorf("operator client: %s response object did not close", label)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("operator client: %s response has trailing JSON", label)
	}
	return seen, nil
}

func requireResponseFields(label string, seen map[string]bool, fields ...string) error {
	for _, field := range fields {
		if !seen[field] {
			return fmt.Errorf("operator client: %s response is missing field %q", label, field)
		}
	}
	return nil
}

func validSHA256Digest(value string) bool {
	if len(value) != len("sha256:")+64 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	for _, c := range value[len("sha256:"):] {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func validEnrollmentToken(value string) bool {
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	return err == nil && len(decoded) == 32 && base64.RawURLEncoding.EncodeToString(decoded) == value
}
