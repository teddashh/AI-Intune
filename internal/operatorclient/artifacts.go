package operatorclient

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/teddashh/AI-Intune/internal/artifact"
	"github.com/teddashh/AI-Intune/internal/operator"
)

// Artifacts returns the safe operator projection of Hub artifact inventory.
// It never calls the machine download endpoint and therefore never needs or
// accepts a machine bearer token.
func (c *Client) Artifacts(ctx context.Context, request operator.ArtifactListRequest) (operator.ArtifactListResult, error) {
	query, effectiveLimit, err := encodeArtifactListQuery(request)
	if err != nil {
		return operator.ArtifactListResult{}, err
	}
	path := "/v1/operator/artifacts"
	if encoded := query.Encode(); encoded != "" {
		path += "?" + encoded
	}
	req, err := c.newOperatorRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return operator.ArtifactListResult{}, err
	}
	response, err := c.doRaw(req)
	if err != nil {
		return operator.ArtifactListResult{}, err
	}
	if response.status != http.StatusOK {
		return operator.ArtifactListResult{}, fmt.Errorf("operator client: artifact list returned HTTP %d", response.status)
	}
	if err := validateArtifactReadHeaders(response.header); err != nil {
		return operator.ArtifactListResult{}, err
	}
	var result operator.ArtifactListResult
	if err := decodeStrictJSONDocument(response.body, "artifact list", &result); err != nil {
		return operator.ArtifactListResult{}, err
	}
	if err := validateArtifactListResult(result, request, effectiveLimit); err != nil {
		return operator.ArtifactListResult{}, err
	}
	return result, nil
}

// Artifact performs the server's full byte verification for one catalog item.
func (c *Client) Artifact(ctx context.Context, artifactID string) (operator.ArtifactDetailResult, error) {
	if !validArtifactClientID(artifactID) {
		return operator.ArtifactDetailResult{}, errors.New("operator client: artifact_id 必須是 canonical catalog identity")
	}
	req, err := c.newOperatorRequest(ctx, http.MethodGet,
		"/v1/operator/artifacts/"+url.PathEscape(artifactID), nil)
	if err != nil {
		return operator.ArtifactDetailResult{}, err
	}
	response, err := c.doRaw(req)
	if err != nil {
		return operator.ArtifactDetailResult{}, err
	}
	if response.status != http.StatusOK {
		return operator.ArtifactDetailResult{}, fmt.Errorf("operator client: artifact detail returned HTTP %d", response.status)
	}
	if err := validateArtifactReadHeaders(response.header); err != nil {
		return operator.ArtifactDetailResult{}, err
	}
	var result operator.ArtifactDetailResult
	if err := decodeStrictJSONDocument(response.body, "artifact detail", &result); err != nil {
		return operator.ArtifactDetailResult{}, err
	}
	if err := validateArtifactDetailResult(result, artifactID); err != nil {
		return operator.ArtifactDetailResult{}, err
	}
	return result, nil
}

func encodeArtifactListQuery(request operator.ArtifactListRequest) (url.Values, int, error) {
	query := make(url.Values)
	if request.Status != "" {
		switch request.Status {
		case operator.ArtifactAvailableUnverified, operator.ArtifactUnavailable, operator.ArtifactInvalid:
			query.Set("status", string(request.Status))
		default:
			return nil, 0, fmt.Errorf("operator client: invalid artifact status %q", request.Status)
		}
	}
	if request.Version != "" {
		if !validArtifactClientText(request.Version, 128) {
			return nil, 0, errors.New("operator client: artifact version 不合法")
		}
		query.Set("version", request.Version)
	}
	limit := request.Limit
	if limit == 0 {
		limit = operator.DefaultArtifactReadLimit
	} else if limit < 1 || limit > operator.MaxArtifactReadLimit {
		return nil, 0, fmt.Errorf("operator client: artifact limit must be between 1 and %d", operator.MaxArtifactReadLimit)
	} else {
		query.Set("limit", strconv.Itoa(limit))
	}
	if request.Cursor != "" {
		if !validArtifactClientText(request.Cursor, 2048) {
			return nil, 0, errors.New("operator client: artifact cursor 不合法")
		}
		query.Set("cursor", request.Cursor)
	}
	return query, limit, nil
}

func validateArtifactReadHeaders(header http.Header) error {
	if err := validateJSONNoStoreResponse(header); err != nil {
		return err
	}
	if replayed, err := responseReplayEvidenceFromHeader(header); err != nil {
		return err
	} else if replayed {
		return errors.New("operator client: artifact read cannot be an idempotency replay")
	}
	if len(header.Values("ETag")) != 0 {
		return errors.New("operator client: live artifact read must not carry an ETag")
	}
	return nil
}

func validateArtifactListResult(result operator.ArtifactListResult, request operator.ArtifactListRequest, limit int) error {
	if result.SchemaVersion != operator.ArtifactReadSchemaVersion ||
		result.Consistency != operator.ArtifactReadConsistencyLive || result.EvaluatedAt.IsZero() {
		return errors.New("operator client: invalid artifact list schema or evaluation time")
	}
	if err := requireUTCArtifactTime(result.EvaluatedAt, "evaluated_at"); err != nil {
		return err
	}
	if result.Items == nil || result.StatusCounts == nil || len(result.StatusCounts) != 3 ||
		result.Total < 0 || result.Total < len(result.Items) || len(result.Items) > limit {
		return errors.New("operator client: inconsistent artifact list shape")
	}
	wantStatuses := []operator.ArtifactReadStatus{
		operator.ArtifactAvailableUnverified, operator.ArtifactUnavailable, operator.ArtifactInvalid,
	}
	total := 0
	pageCounts := make(map[operator.ArtifactReadStatus]int, len(wantStatuses))
	for i, count := range result.StatusCounts {
		if count.Status != wantStatuses[i] || count.Count < 0 {
			return errors.New("operator client: artifact status counts are not canonical")
		}
		total += count.Count
	}
	if total != result.Total {
		return errors.New("operator client: artifact status counts do not equal total")
	}
	seen := make(map[string]bool, len(result.Items))
	for i, item := range result.Items {
		if err := validateArtifactSummary(item, result.EvaluatedAt, false); err != nil {
			return fmt.Errorf("operator client: artifact list item %d: %w", i, err)
		}
		if seen[item.ArtifactID] {
			return errors.New("operator client: duplicate artifact_id")
		}
		seen[item.ArtifactID] = true
		pageCounts[item.Status]++
		if request.Status != "" && item.Status != request.Status {
			return errors.New("operator client: artifact item violates status filter")
		}
		if request.Version != "" && (item.Version == nil || *item.Version != request.Version) {
			return errors.New("operator client: artifact item violates version filter")
		}
		if i > 0 && result.Items[i-1].ArtifactID >= item.ArtifactID {
			return errors.New("operator client: artifact items are not in canonical order")
		}
	}
	for _, count := range result.StatusCounts {
		if pageCounts[count.Status] > count.Count {
			return errors.New("operator client: artifact status counts contradict page items")
		}
	}
	if result.NextCursor != nil {
		if !validArtifactClientText(*result.NextCursor, 2048) || len(result.Items) != limit ||
			result.Total <= len(result.Items) || *result.NextCursor == request.Cursor {
			return errors.New("operator client: artifact next_cursor is inconsistent")
		}
	}
	if request.Cursor == "" && (result.Total > len(result.Items)) != (result.NextCursor != nil) {
		return errors.New("operator client: first artifact page has inconsistent continuation evidence")
	}
	return nil
}

func validateArtifactDetailResult(result operator.ArtifactDetailResult, artifactID string) error {
	if result.SchemaVersion != operator.ArtifactReadSchemaVersion ||
		result.Consistency != operator.ArtifactReadConsistencyLive || result.EvaluatedAt.IsZero() {
		return errors.New("operator client: invalid artifact detail schema or evaluation time")
	}
	if err := requireUTCArtifactTime(result.EvaluatedAt, "evaluated_at"); err != nil {
		return err
	}
	if result.Item.ArtifactID != artifactID {
		return errors.New("operator client: artifact detail identity does not match request")
	}
	return validateArtifactSummary(result.Item, result.EvaluatedAt, true)
}

func validateArtifactSummary(item operator.ArtifactSummary, evaluatedAt time.Time, detail bool) error {
	if !validArtifactClientID(item.ArtifactID) || item.DeploymentReferences < 0 ||
		item.ActiveDeploymentReferences < 0 || item.ActiveDeploymentReferences > item.DeploymentReferences {
		return errors.New("operator client: invalid artifact identity or reference counts")
	}
	if (item.SHA256 == nil) != (item.Digest == nil) {
		return errors.New("operator client: artifact sha256 and digest must be present together")
	}
	if item.SHA256 != nil && (!artifact.ValidSHA256Hex(*item.SHA256) ||
		*item.Digest != "sha256:"+*item.SHA256 || item.ArtifactID != *item.SHA256) {
		return errors.New("operator client: artifact digest identity is inconsistent")
	}
	hasRecord := item.Name != nil || item.Version != nil || item.SizeBytes != nil || item.FetchedAt != nil || item.EnginesNode != nil
	if hasRecord {
		if item.Name == nil || item.Version == nil || item.SizeBytes == nil || item.FetchedAt == nil || item.SHA256 == nil ||
			!validArtifactClientText(*item.Name, 128) || !validArtifactClientText(*item.Version, 128) || *item.SizeBytes < 0 {
			return errors.New("operator client: artifact metadata is incomplete or invalid")
		}
		if err := requireUTCArtifactTime(*item.FetchedAt, "fetched_at"); err != nil {
			return err
		}
		if item.EnginesNode != nil && (*item.EnginesNode == "" || !validArtifactClientText(*item.EnginesNode, 512)) {
			return errors.New("operator client: artifact engines_node is invalid")
		}
	}
	if item.VerifiedAt != nil {
		if err := requireUTCArtifactTime(*item.VerifiedAt, "verified_at"); err != nil {
			return err
		}
		if !item.VerifiedAt.Equal(evaluatedAt) {
			return errors.New("operator client: artifact verification time is not bound to evaluation time")
		}
	}
	switch item.Status {
	case operator.ArtifactAvailableUnverified:
		if !hasRecord || item.Issue != nil || item.VerifiedAt != nil || detail && item.Status == operator.ArtifactAvailableUnverified {
			return errors.New("operator client: available_unverified artifact has contradictory evidence")
		}
	case operator.ArtifactReady:
		if !detail || !hasRecord || item.Issue != nil || item.VerifiedAt == nil {
			return errors.New("operator client: ready artifact lacks detail verification evidence")
		}
	case operator.ArtifactUnavailable:
		if item.Issue == nil || item.VerifiedAt != nil ||
			(*item.Issue != operator.ArtifactReadIssue(artifact.CatalogIssueSidecarMissing) &&
				*item.Issue != operator.ArtifactReadIssue(artifact.CatalogIssueTarballMissing)) {
			return errors.New("operator client: unavailable artifact has invalid evidence")
		}
	case operator.ArtifactInvalid:
		if item.Issue == nil || item.VerifiedAt != nil || !validArtifactInvalidIssue(*item.Issue) {
			return errors.New("operator client: invalid artifact lacks a canonical issue")
		}
	default:
		return errors.New("operator client: artifact status is not canonical")
	}
	return nil
}

func validArtifactInvalidIssue(issue operator.ArtifactReadIssue) bool {
	switch artifact.CatalogIssue(issue) {
	case artifact.CatalogIssueSidecarFilenameInvalid, artifact.CatalogIssueSidecarNotRegular,
		artifact.CatalogIssueSidecarUnreadable, artifact.CatalogIssueSidecarTooLarge,
		artifact.CatalogIssueSidecarInvalidJSON, artifact.CatalogIssueSidecarInvalidSchema,
		artifact.CatalogIssueSidecarInvalidMetadata, artifact.CatalogIssueTarballNotRegular,
		artifact.CatalogIssueTarballSizeMismatch, artifact.CatalogIssueTarballDigestMismatch,
		artifact.CatalogIssueTarballChanged, artifact.CatalogIssueTarballUnreadable:
		return true
	default:
		return false
	}
}

func validArtifactClientID(value string) bool {
	if artifact.ValidSHA256Hex(value) {
		return true
	}
	digest, ok := strings.CutPrefix(value, "invalid-")
	return ok && artifact.ValidSHA256Hex(digest)
}

func validArtifactClientText(value string, maxBytes int) bool {
	if value == "" || value != strings.TrimSpace(value) || len(value) > maxBytes {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return false
		}
	}
	return true
}

func requireUTCArtifactTime(value time.Time, field string) error {
	if value.IsZero() {
		return fmt.Errorf("operator client: artifact %s is missing", field)
	}
	if _, offset := value.Zone(); offset != 0 {
		return fmt.Errorf("operator client: artifact %s must be UTC", field)
	}
	return nil
}
