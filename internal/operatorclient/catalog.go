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
	"net/url"
	"strconv"
	"time"

	appcatalog "github.com/teddashh/AI-Intune/internal/catalog"
	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/store"
)

func (c *Client) CatalogManifests(ctx context.Context,
	request operator.CatalogManifestListRequest,
) (operator.CatalogManifestListResult, error) {
	query, limit, err := encodeCatalogManifestListQuery(request)
	if err != nil {
		return operator.CatalogManifestListResult{}, err
	}
	path := "/v1/operator/catalog-manifests"
	if encoded := query.Encode(); encoded != "" {
		path += "?" + encoded
	}
	response, err := c.catalogRequest(ctx, http.MethodGet, path, "", nil)
	if err != nil {
		return operator.CatalogManifestListResult{}, err
	}
	if response.status != http.StatusOK {
		return operator.CatalogManifestListResult{}, fmt.Errorf("operator client: catalog manifest list returned HTTP %d", response.status)
	}
	if _, err := validateCatalogResponseHeaders(response.header, false); err != nil {
		return operator.CatalogManifestListResult{}, err
	}
	var result operator.CatalogManifestListResult
	if err := decodeStrictJSONDocument(response.body, "catalog manifest list", &result); err != nil {
		return operator.CatalogManifestListResult{}, err
	}
	if err := validateCatalogManifestListResult(result, request, limit); err != nil {
		return operator.CatalogManifestListResult{}, err
	}
	return result, nil
}

func (c *Client) PreviewStandardCatalogManifest(ctx context.Context,
	request operator.StandardCatalogManifestPreviewRequest,
) (operator.StandardCatalogManifestPreviewResult, error) {
	if !validArtifactFetchSHA256(request.ArtifactSHA256) ||
		request.NodeRuntimeVersion != "" && !validArtifactFetchText(request.NodeRuntimeVersion, 128, false) {
		return operator.StandardCatalogManifestPreviewResult{}, errors.New("operator client: standard catalog preview request is invalid")
	}
	raw, _ := json.Marshal(request)
	response, err := c.catalogRequest(ctx, http.MethodPost,
		"/v1/operator/catalog-manifests/standard-preview", "", raw)
	if err != nil {
		return operator.StandardCatalogManifestPreviewResult{}, err
	}
	if response.status != http.StatusOK {
		return operator.StandardCatalogManifestPreviewResult{}, fmt.Errorf("operator client: standard catalog preview returned HTTP %d", response.status)
	}
	if _, err := validateCatalogResponseHeaders(response.header, false); err != nil {
		return operator.StandardCatalogManifestPreviewResult{}, err
	}
	var result operator.StandardCatalogManifestPreviewResult
	if err := decodeStrictJSONDocument(response.body, "standard catalog preview", &result); err != nil {
		return operator.StandardCatalogManifestPreviewResult{}, err
	}
	if result.SchemaVersion != operator.StandardCatalogPreviewSchemaVersion ||
		result.Manifest.Artifact.SHA256 != request.ArtifactSHA256 ||
		result.ManifestDigest != catalogManifestWireDigest(result.Manifest) ||
		!validSHA256Digest(result.PreviewDigest) || result.PreviewedAt.IsZero() || result.PreviewedAt.Location() != time.UTC ||
		validateCatalogManifestRecord(operator.CatalogManifestRecord{
			Manifest: result.Manifest, Digest: result.ManifestDigest, PublishedAt: result.PreviewedAt,
		}) != nil {
		return operator.StandardCatalogManifestPreviewResult{}, errors.New("operator client: standard catalog preview response is invalid")
	}
	validDependencyShape := false
	switch result.Manifest.ID {
	case "node-runtime", "hermes-agent":
		validDependencyShape = request.NodeRuntimeVersion == "" && result.EnginesNode == nil &&
			len(result.Manifest.Dependencies) == 0
	case "openclaw":
		validDependencyShape = len(result.Manifest.Dependencies) == 1 &&
			result.Manifest.Dependencies[0].Version == request.NodeRuntimeVersion && result.EnginesNode != nil
	}
	if !validDependencyShape {
		return operator.StandardCatalogManifestPreviewResult{}, errors.New("operator client: standard catalog dependency response is inconsistent")
	}
	return result, nil
}

func (c *Client) PublishStandardCatalogManifest(ctx context.Context, idempotencyKey string,
	request operator.CatalogManifestPublishRequest,
) (operator.CatalogManifestPublishResult, error) {
	if err := validateCatalogMutationAuthority(idempotencyKey, request.IdempotencyKey, request.Actor); err != nil {
		return operator.CatalogManifestPublishResult{}, err
	}
	if err := appcatalog.ValidateManifest(request.Manifest); err != nil ||
		request.ConfirmPackageID != request.Manifest.ID || request.ConfirmVersion != request.Manifest.Version ||
		!validSHA256Digest(request.PreviewDigest) || !validArtifactFetchText(request.Reason, 500, false) {
		return operator.CatalogManifestPublishResult{}, errors.New("operator client: standard catalog publication is invalid")
	}
	raw, err := json.Marshal(struct {
		Manifest         appcatalog.Manifest `json:"manifest"`
		ConfirmPackageID string              `json:"confirm_package_id"`
		ConfirmVersion   string              `json:"confirm_version"`
		PreviewDigest    string              `json:"preview_digest"`
		Reason           string              `json:"reason"`
	}{request.Manifest, request.ConfirmPackageID, request.ConfirmVersion, request.PreviewDigest, request.Reason})
	if err != nil {
		return operator.CatalogManifestPublishResult{}, err
	}
	return c.publishCatalogManifestRaw(ctx, idempotencyKey, request, raw)
}

func (c *Client) PublishCatalogManifest(ctx context.Context, idempotencyKey string,
	request operator.CatalogManifestPublishRequest,
) (operator.CatalogManifestPublishResult, error) {
	if err := validateCatalogMutationAuthority(idempotencyKey, request.IdempotencyKey, request.Actor); err != nil {
		return operator.CatalogManifestPublishResult{}, err
	}
	if err := appcatalog.ValidateManifest(request.Manifest); err != nil || !validArtifactFetchText(request.Reason, 500, false) {
		return operator.CatalogManifestPublishResult{}, errors.New("operator client: catalog manifest publication is invalid")
	}
	raw, err := json.Marshal(struct {
		Manifest appcatalog.Manifest `json:"manifest"`
		Reason   string              `json:"reason"`
	}{request.Manifest, request.Reason})
	if err != nil {
		return operator.CatalogManifestPublishResult{}, err
	}
	return c.publishCatalogManifestRaw(ctx, idempotencyKey, request, raw)
}

func (c *Client) publishCatalogManifestRaw(ctx context.Context, idempotencyKey string,
	request operator.CatalogManifestPublishRequest, raw []byte,
) (operator.CatalogManifestPublishResult, error) {
	response, err := c.catalogRequest(ctx, http.MethodPost, "/v1/operator/catalog-manifests", idempotencyKey, raw)
	if err != nil {
		return operator.CatalogManifestPublishResult{}, err
	}
	if response.status != http.StatusCreated && response.status != http.StatusOK {
		return operator.CatalogManifestPublishResult{}, fmt.Errorf("operator client: catalog manifest publication returned HTTP %d", response.status)
	}
	replayed, err := validateCatalogResponseHeaders(response.header, true)
	if err != nil {
		return operator.CatalogManifestPublishResult{}, err
	}
	var result operator.CatalogManifestPublishResult
	if err := decodeStrictJSONDocument(response.body, "catalog manifest publication", &result); err != nil {
		return operator.CatalogManifestPublishResult{}, err
	}
	if err := validateCatalogManifestRecord(result.Record); err != nil ||
		result.Record.Manifest.ID != request.Manifest.ID || result.Record.Manifest.Version != request.Manifest.Version ||
		result.Record.Digest != catalogManifestWireDigest(request.Manifest) ||
		result.Replayed != replayed || response.status == http.StatusCreated && (result.Replayed || result.AlreadyPublished) ||
		response.status == http.StatusOK && !result.Replayed && !result.AlreadyPublished {
		return operator.CatalogManifestPublishResult{}, errors.New("operator client: catalog manifest publication response is inconsistent")
	}
	return result, nil
}

func (c *Client) MachineProfiles(ctx context.Context,
	request operator.MachineProfileListRequest,
) (operator.MachineProfileListResult, error) {
	query, limit, err := encodeMachineProfileListQuery(request)
	if err != nil {
		return operator.MachineProfileListResult{}, err
	}
	path := "/v1/operator/machine-profiles"
	if encoded := query.Encode(); encoded != "" {
		path += "?" + encoded
	}
	response, err := c.catalogRequest(ctx, http.MethodGet, path, "", nil)
	if err != nil {
		return operator.MachineProfileListResult{}, err
	}
	if response.status != http.StatusOK {
		return operator.MachineProfileListResult{}, fmt.Errorf("operator client: machine profile list returned HTTP %d", response.status)
	}
	if _, err := validateCatalogResponseHeaders(response.header, false); err != nil {
		return operator.MachineProfileListResult{}, err
	}
	var result operator.MachineProfileListResult
	if err := decodeStrictJSONDocument(response.body, "machine profile list", &result); err != nil {
		return operator.MachineProfileListResult{}, err
	}
	if err := validateMachineProfileListResult(result, request, limit); err != nil {
		return operator.MachineProfileListResult{}, err
	}
	return result, nil
}

func (c *Client) PreviewMachineProfile(ctx context.Context,
	request operator.MachineProfilePreviewRequest,
) (operator.MachineProfilePreviewResult, error) {
	if err := appcatalog.ValidateProfile(request.Profile); err != nil {
		return operator.MachineProfilePreviewResult{}, errors.New("operator client: machine profile preview request is invalid")
	}
	raw, _ := json.Marshal(request)
	response, err := c.catalogRequest(ctx, http.MethodPost, "/v1/operator/machine-profiles/preview", "", raw)
	if err != nil {
		return operator.MachineProfilePreviewResult{}, err
	}
	if response.status != http.StatusOK {
		return operator.MachineProfilePreviewResult{}, fmt.Errorf("operator client: machine profile preview returned HTTP %d", response.status)
	}
	if _, err := validateCatalogResponseHeaders(response.header, false); err != nil {
		return operator.MachineProfilePreviewResult{}, err
	}
	var result operator.MachineProfilePreviewResult
	if err := decodeStrictJSONDocument(response.body, "machine profile preview", &result); err != nil {
		return operator.MachineProfilePreviewResult{}, err
	}
	if result.SchemaVersion != operator.MachineProfilePreviewSchemaVersion ||
		result.ProfileDigest != machineProfileWireDigest(result.Profile) || result.ProfileDigest != machineProfileWireDigest(request.Profile) ||
		!validSHA256Digest(result.PreviewDigest) || result.PreviewedAt.IsZero() || result.PreviewedAt.Location() != time.UTC {
		return operator.MachineProfilePreviewResult{}, errors.New("operator client: machine profile preview response is invalid")
	}
	return result, nil
}

func (c *Client) PublishReviewedMachineProfile(ctx context.Context, idempotencyKey string,
	request operator.MachineProfilePublishRequest,
) (operator.MachineProfilePublishResult, error) {
	if err := validateCatalogMutationAuthority(idempotencyKey, request.IdempotencyKey, request.Actor); err != nil {
		return operator.MachineProfilePublishResult{}, err
	}
	if err := appcatalog.ValidateProfile(request.Profile); err != nil || request.ConfirmProfileID != request.Profile.ID ||
		request.ConfirmRevision != request.Profile.Revision || !validSHA256Digest(request.PreviewDigest) ||
		!validArtifactFetchText(request.Reason, 500, false) {
		return operator.MachineProfilePublishResult{}, errors.New("operator client: reviewed machine profile publication is invalid")
	}
	raw, err := json.Marshal(struct {
		Profile          appcatalog.MachineProfile `json:"profile"`
		ConfirmProfileID string                    `json:"confirm_profile_id"`
		ConfirmRevision  int64                     `json:"confirm_revision"`
		PreviewDigest    string                    `json:"preview_digest"`
		Reason           string                    `json:"reason"`
	}{request.Profile, request.ConfirmProfileID, request.ConfirmRevision, request.PreviewDigest, request.Reason})
	if err != nil {
		return operator.MachineProfilePublishResult{}, err
	}
	return c.publishMachineProfileRaw(ctx, idempotencyKey, request, raw)
}

func (c *Client) PublishMachineProfile(ctx context.Context, idempotencyKey string,
	request operator.MachineProfilePublishRequest,
) (operator.MachineProfilePublishResult, error) {
	if err := validateCatalogMutationAuthority(idempotencyKey, request.IdempotencyKey, request.Actor); err != nil {
		return operator.MachineProfilePublishResult{}, err
	}
	if err := appcatalog.ValidateProfile(request.Profile); err != nil || !validArtifactFetchText(request.Reason, 500, false) {
		return operator.MachineProfilePublishResult{}, errors.New("operator client: machine profile publication is invalid")
	}
	raw, err := json.Marshal(struct {
		Profile appcatalog.MachineProfile `json:"profile"`
		Reason  string                    `json:"reason"`
	}{request.Profile, request.Reason})
	if err != nil {
		return operator.MachineProfilePublishResult{}, err
	}
	return c.publishMachineProfileRaw(ctx, idempotencyKey, request, raw)
}

func (c *Client) publishMachineProfileRaw(ctx context.Context, idempotencyKey string,
	request operator.MachineProfilePublishRequest, raw []byte,
) (operator.MachineProfilePublishResult, error) {
	response, err := c.catalogRequest(ctx, http.MethodPost, "/v1/operator/machine-profiles", idempotencyKey, raw)
	if err != nil {
		return operator.MachineProfilePublishResult{}, err
	}
	if response.status != http.StatusCreated && response.status != http.StatusOK {
		return operator.MachineProfilePublishResult{}, fmt.Errorf("operator client: machine profile publication returned HTTP %d", response.status)
	}
	replayed, err := validateCatalogResponseHeaders(response.header, true)
	if err != nil {
		return operator.MachineProfilePublishResult{}, err
	}
	var result operator.MachineProfilePublishResult
	if err := decodeStrictJSONDocument(response.body, "machine profile publication", &result); err != nil {
		return operator.MachineProfilePublishResult{}, err
	}
	if err := validateMachineProfileRecord(result.Record); err != nil ||
		result.Record.Profile.ID != request.Profile.ID || result.Record.Profile.Revision != request.Profile.Revision ||
		result.Record.Digest != machineProfileWireDigest(request.Profile) ||
		result.Replayed != replayed || response.status == http.StatusCreated && (result.Replayed || result.AlreadyPublished) ||
		response.status == http.StatusOK && !result.Replayed && !result.AlreadyPublished {
		return operator.MachineProfilePublishResult{}, errors.New("operator client: machine profile publication response is inconsistent")
	}
	return result, nil
}

func (c *Client) PreviewMachineProfileAssignment(ctx context.Context,
	request operator.MachineProfileAssignmentPreviewRequest,
) (operator.MachineProfileAssignmentPreviewResult, error) {
	if err := validateProfileAssignmentIdentity(request.MachineID, request.ProfileID, request.ProfileRevision); err != nil {
		return operator.MachineProfileAssignmentPreviewResult{}, err
	}
	raw, _ := json.Marshal(struct {
		ProfileID       string `json:"profile_id"`
		ProfileRevision int64  `json:"profile_revision"`
	}{request.ProfileID, request.ProfileRevision})
	path := "/v1/operator/machines/" + url.PathEscape(request.MachineID) + "/profile-assignment-preview"
	response, err := c.catalogRequest(ctx, http.MethodPost, path, "", raw)
	if err != nil {
		return operator.MachineProfileAssignmentPreviewResult{}, err
	}
	if response.status != http.StatusOK {
		return operator.MachineProfileAssignmentPreviewResult{}, fmt.Errorf("operator client: machine profile assignment preview returned HTTP %d", response.status)
	}
	if _, err := validateCatalogResponseHeaders(response.header, false); err != nil {
		return operator.MachineProfileAssignmentPreviewResult{}, err
	}
	var result operator.MachineProfileAssignmentPreviewResult
	if err := decodeStrictJSONDocument(response.body, "machine profile assignment preview", &result); err != nil {
		return operator.MachineProfileAssignmentPreviewResult{}, err
	}
	if err := validateProfileAssignmentPreview(result, request); err != nil {
		return operator.MachineProfileAssignmentPreviewResult{}, err
	}
	return result, nil
}

func (c *Client) AssignMachineProfile(ctx context.Context, idempotencyKey string,
	request operator.MachineProfileAssignmentRequest,
) (operator.MachineProfileAssignmentResult, error) {
	if err := validateCatalogMutationAuthority(idempotencyKey, request.IdempotencyKey, request.Actor); err != nil {
		return operator.MachineProfileAssignmentResult{}, err
	}
	if err := validateProfileAssignmentIdentity(request.MachineID, request.ProfileID, request.ProfileRevision); err != nil ||
		!validArtifactFetchText(request.ConfirmDisplayName, 200, false) || !validSHA256Digest(request.PreviewDigest) ||
		!validArtifactFetchText(request.Reason, 500, false) {
		return operator.MachineProfileAssignmentResult{}, errors.New("operator client: machine profile assignment request is invalid")
	}
	raw, _ := json.Marshal(struct {
		ProfileID          string `json:"profile_id"`
		ProfileRevision    int64  `json:"profile_revision"`
		ConfirmDisplayName string `json:"confirm_display_name"`
		PreviewDigest      string `json:"preview_digest"`
		Reason             string `json:"reason"`
	}{request.ProfileID, request.ProfileRevision, request.ConfirmDisplayName, request.PreviewDigest, request.Reason})
	path := "/v1/operator/machines/" + url.PathEscape(request.MachineID) + "/profile-assignments"
	response, err := c.catalogRequest(ctx, http.MethodPost, path, idempotencyKey, raw)
	if err != nil {
		return operator.MachineProfileAssignmentResult{}, err
	}
	if response.status != http.StatusCreated && response.status != http.StatusOK {
		return operator.MachineProfileAssignmentResult{}, fmt.Errorf("operator client: machine profile assignment returned HTTP %d", response.status)
	}
	replayed, err := validateCatalogResponseHeaders(response.header, true)
	if err != nil {
		return operator.MachineProfileAssignmentResult{}, err
	}
	var result operator.MachineProfileAssignmentResult
	if err := decodeStrictJSONDocument(response.body, "machine profile assignment", &result); err != nil {
		return operator.MachineProfileAssignmentResult{}, err
	}
	if err := validateProfileAssignmentResult(result, request, replayed, response.status); err != nil {
		return operator.MachineProfileAssignmentResult{}, err
	}
	return result, nil
}

func (c *Client) catalogRequest(ctx context.Context, method, path, idempotencyKey string,
	body []byte,
) (operatorRawResponse, error) {
	var reader *bytes.Reader
	if body == nil {
		reader = bytes.NewReader(nil)
	} else {
		reader = bytes.NewReader(body)
	}
	req, err := c.newOperatorRequest(ctx, method, path, reader)
	if err != nil {
		return operatorRawResponse{}, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
	return c.doRaw(req)
}

func validateCatalogMutationAuthority(headerKey, bodyKey string, actor operator.Actor) error {
	if err := validateDeploymentActionIdempotencyKey(headerKey); err != nil {
		return err
	}
	if bodyKey != "" && bodyKey != headerKey {
		return errors.New("operator client: body and header idempotency authorities differ")
	}
	if actor != (operator.Actor{}) {
		return errors.New("operator client: HTTP request must not supply an in-process actor")
	}
	return nil
}

func validateProfileAssignmentIdentity(machineID, profileID string, revision int64) error {
	if err := validateArtifactFetchIdentifier("machine_id", machineID, 128); err != nil {
		return err
	}
	if err := validateArtifactFetchIdentifier("profile_id", profileID, 128); err != nil {
		return err
	}
	if revision <= 0 {
		return errors.New("operator client: profile_revision must be positive")
	}
	return nil
}

func encodeCatalogManifestListQuery(request operator.CatalogManifestListRequest) (url.Values, int, error) {
	query := make(url.Values)
	if request.PackageID != "" {
		if err := validateArtifactFetchIdentifier("package_id", request.PackageID, 128); err != nil {
			return nil, 0, err
		}
		query.Set("package_id", request.PackageID)
	}
	if request.Kind != "" {
		if request.Kind != appcatalog.KindApp && request.Kind != appcatalog.KindRuntime {
			return nil, 0, errors.New("operator client: catalog manifest kind is invalid")
		}
		query.Set("kind", string(request.Kind))
	}
	return encodeCatalogListPage(query, request.Limit, request.Cursor)
}

func encodeMachineProfileListQuery(request operator.MachineProfileListRequest) (url.Values, int, error) {
	query := make(url.Values)
	if request.ProfileID != "" {
		if err := validateArtifactFetchIdentifier("profile_id", request.ProfileID, 128); err != nil {
			return nil, 0, err
		}
		query.Set("profile_id", request.ProfileID)
	}
	return encodeCatalogListPage(query, request.Limit, request.Cursor)
}

func encodeCatalogListPage(query url.Values, requested int, cursor string) (url.Values, int, error) {
	limit := requested
	if limit == 0 {
		limit = operator.DefaultCatalogReadLimit
	} else if limit < 1 || limit > operator.MaxCatalogReadLimit {
		return nil, 0, fmt.Errorf("operator client: catalog list limit must be between 1 and %d", operator.MaxCatalogReadLimit)
	} else {
		query.Set("limit", strconv.Itoa(limit))
	}
	if cursor != "" {
		if !validArtifactFetchText(cursor, 2048, false) {
			return nil, 0, errors.New("operator client: catalog list cursor is invalid")
		}
		query.Set("cursor", cursor)
	}
	return query, limit, nil
}

func validateCatalogResponseHeaders(header http.Header, replayAllowed bool) (bool, error) {
	if err := validateJSONNoStoreResponse(header); err != nil {
		return false, err
	}
	if len(header.Values("ETag")) != 0 || len(header.Values("Location")) != 0 {
		return false, errors.New("operator client: catalog response carries unsupported representation headers")
	}
	replayed, err := responseReplayEvidenceFromHeader(header)
	if err != nil {
		return false, err
	}
	if replayed && !replayAllowed {
		return false, errors.New("operator client: catalog read or preview cannot be replayed")
	}
	return replayed, nil
}

func validateCatalogManifestListResult(result operator.CatalogManifestListResult,
	request operator.CatalogManifestListRequest, limit int,
) error {
	if result.SchemaVersion != operator.CatalogReadSchemaVersion || result.Consistency != operator.CatalogReadConsistencyLive ||
		result.EvaluatedAt.IsZero() || result.EvaluatedAt.Location() != time.UTC || result.Items == nil ||
		result.Total < len(result.Items) || len(result.Items) > limit {
		return errors.New("operator client: catalog manifest list response is invalid")
	}
	previous := ""
	for _, item := range result.Items {
		if err := validateCatalogManifestRecord(item); err != nil {
			return err
		}
		if request.PackageID != "" && item.Manifest.ID != request.PackageID || request.Kind != "" && item.Manifest.Kind != request.Kind {
			return errors.New("operator client: catalog manifest violates list filters")
		}
		key := item.Manifest.ID + "\x00" + item.Manifest.Version
		if previous != "" && previous >= key {
			return errors.New("operator client: catalog manifest list order is invalid")
		}
		previous = key
	}
	return validateCatalogNextCursor(result.NextCursor, len(result.Items), result.Total, request.Cursor != "")
}

func validateMachineProfileListResult(result operator.MachineProfileListResult,
	request operator.MachineProfileListRequest, limit int,
) error {
	if result.SchemaVersion != operator.CatalogReadSchemaVersion || result.Consistency != operator.CatalogReadConsistencyLive ||
		result.EvaluatedAt.IsZero() || result.EvaluatedAt.Location() != time.UTC || result.Items == nil ||
		result.Total < len(result.Items) || len(result.Items) > limit {
		return errors.New("operator client: machine profile list response is invalid")
	}
	for index, item := range result.Items {
		if err := validateMachineProfileRecord(item); err != nil {
			return err
		}
		if request.ProfileID != "" && item.Profile.ID != request.ProfileID {
			return errors.New("operator client: machine profile violates list filter")
		}
		if index > 0 {
			previous := result.Items[index-1].Profile
			if previous.ID > item.Profile.ID || previous.ID == item.Profile.ID && previous.Revision <= item.Profile.Revision {
				return errors.New("operator client: machine profile list order is invalid")
			}
		}
	}
	return validateCatalogNextCursor(result.NextCursor, len(result.Items), result.Total, request.Cursor != "")
}

func validateCatalogNextCursor(cursor *string, count, total int, continued bool) error {
	if cursor != nil && !validArtifactFetchText(*cursor, 2048, false) || cursor != nil && count == 0 ||
		cursor == nil && count < total && !continued {
		return errors.New("operator client: catalog list pagination evidence is invalid")
	}
	return nil
}

func validateCatalogManifestRecord(record operator.CatalogManifestRecord) error {
	if err := appcatalog.ValidateManifest(record.Manifest); err != nil || !validSHA256Digest(record.Digest) ||
		record.Digest != catalogManifestWireDigest(record.Manifest) ||
		record.PublishedAt.IsZero() || record.PublishedAt.Location() != time.UTC || record.PublishedAt.Nanosecond() != 0 ||
		record.PublishedBy != "" {
		return errors.New("operator client: catalog manifest record is invalid")
	}
	return nil
}

func validateMachineProfileRecord(record operator.MachineProfileRecord) error {
	if err := appcatalog.ValidateProfile(record.Profile); err != nil || !validSHA256Digest(record.Digest) ||
		record.Digest != machineProfileWireDigest(record.Profile) ||
		record.PublishedAt.IsZero() || record.PublishedAt.Location() != time.UTC || record.PublishedAt.Nanosecond() != 0 ||
		record.PublishedBy != "" {
		return errors.New("operator client: machine profile record is invalid")
	}
	return nil
}

func catalogManifestWireDigest(manifest appcatalog.Manifest) string {
	raw, err := json.Marshal(manifest)
	if err != nil {
		return ""
	}
	canonical, err := appcatalog.ParseManifest(raw)
	if err != nil {
		return ""
	}
	raw, _ = json.Marshal(canonical)
	return catalogWireDigest(raw)
}

func machineProfileWireDigest(profile appcatalog.MachineProfile) string {
	raw, err := json.Marshal(profile)
	if err != nil {
		return ""
	}
	canonical, err := appcatalog.ParseProfile(raw)
	if err != nil {
		return ""
	}
	raw, _ = json.Marshal(canonical)
	return catalogWireDigest(raw)
}

func catalogWireDigest(raw []byte) string {
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func validateProfileAssignmentPreview(result operator.MachineProfileAssignmentPreviewResult,
	request operator.MachineProfileAssignmentPreviewRequest,
) error {
	if result.MachineID != request.MachineID || result.ProfileID != request.ProfileID || result.ProfileRevision != request.ProfileRevision ||
		!validArtifactFetchText(result.DisplayName, 200, false) || result.LifecycleRevision < 0 ||
		!validSHA256Digest(result.ProfileDigest) || !validSHA256Digest(result.PreviewDigest) ||
		!validProfileAssignmentTarget(result.Target) ||
		result.PreviewedAt.IsZero() || result.PreviewedAt.Location() != time.UTC || result.Packages == nil ||
		result.CreatesDesiredStates < 0 || result.CreatesJobs < 0 || result.ActiveJobCount < 0 || result.Blockers == nil {
		return errors.New("operator client: machine profile assignment preview is invalid")
	}
	if err := validateAssignmentImpacts(result.Packages); err != nil {
		return err
	}
	if result.ChangesMachineConfiguration {
		if result.CreatesDesiredStates != len(result.Packages) || result.CreatesJobs != len(result.Packages) || !result.CreatesAssignment {
			return errors.New("operator client: machine profile assignment preview impact is inconsistent")
		}
	} else if !result.AlreadyAssigned || result.CreatesDesiredStates != 0 || result.CreatesJobs != 0 || result.CreatesAssignment {
		return errors.New("operator client: machine profile assignment no-op impact is inconsistent")
	}
	for _, blocker := range result.Blockers {
		switch blocker {
		case store.OperatorMachineProfileAssignmentBlockerRetired,
			store.OperatorMachineProfileAssignmentBlockerNeverReported,
			store.OperatorMachineProfileAssignmentBlockerExecutionUnknown,
			store.OperatorMachineProfileAssignmentBlockerExecutionDisabled,
			store.OperatorMachineProfileAssignmentBlockerActiveJob:
		default:
			return errors.New("operator client: machine profile assignment preview blocker is invalid")
		}
	}
	return nil
}

func validateAssignmentImpacts(items []store.OperatorMachineProfileAssignmentPackageImpact) error {
	for index, item := range items {
		if item.Position != index || !validArtifactFetchText(item.PackageID, 128, false) ||
			!validArtifactFetchText(item.PackageVersion, 128, false) || !validSHA256Digest(item.ManifestDigest) ||
			!validArtifactFetchText(item.ResourceKind, 128, false) || !validArtifactFetchText(item.ResourceID, 128, false) ||
			!validSHA256Digest(item.SpecDigest) || !validSHA256Digest(item.ArtifactDigest) ||
			item.ExecutionTimeoutSeconds <= 0 || item.CurrentRevision < 0 || item.PlannedRevision <= 0 ||
			item.PrerequisitePackages == nil {
			return errors.New("operator client: machine profile assignment package impact is invalid")
		}
	}
	return nil
}

func validateProfileAssignmentResult(result operator.MachineProfileAssignmentResult,
	request operator.MachineProfileAssignmentRequest, replayed bool, status int,
) error {
	if result.Replayed != replayed || result.MachineID != request.MachineID || result.ProfileID != request.ProfileID ||
		result.ProfileRevision != request.ProfileRevision || result.DisplayName != request.ConfirmDisplayName ||
		result.PreviewDigest != request.PreviewDigest || result.AssignmentID == "" || result.AssignmentRevision <= 0 ||
		result.LifecycleRevision < 0 || !validSHA256Digest(result.ProfileDigest) || !validProfileAssignmentTarget(result.Target) ||
		result.AssignedAt.IsZero() ||
		result.AssignedAt.Location() != time.UTC || result.Packages == nil || status == http.StatusCreated && (replayed || result.AlreadyAssigned) ||
		status == http.StatusOK && !replayed && !result.AlreadyAssigned {
		return errors.New("operator client: machine profile assignment response is inconsistent")
	}
	impacts := make([]store.OperatorMachineProfileAssignmentPackageImpact, len(result.Packages))
	for index, item := range result.Packages {
		impacts[index] = item.OperatorMachineProfileAssignmentPackageImpact
		if !validArtifactFetchText(item.DesiredID, 128, false) || !validArtifactFetchText(item.JobID, 128, false) || item.Revision <= 0 {
			return errors.New("operator client: machine profile assignment result package is invalid")
		}
	}
	return validateAssignmentImpacts(impacts)
}

func validProfileAssignmentTarget(target appcatalog.Platform) bool {
	return (target.OS == "linux" || target.OS == "darwin") &&
		(target.Arch == "amd64" || target.Arch == "arm64")
}
