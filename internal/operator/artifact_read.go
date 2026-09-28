package operator

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/teddashh/AI-Intune/internal/artifact"
	"github.com/teddashh/AI-Intune/internal/model"
	"github.com/teddashh/AI-Intune/internal/store"
)

const (
	ArtifactReadSchemaVersion   = 1
	ArtifactReadConsistencyLive = "live"
	DefaultArtifactReadLimit    = 50
	MaxArtifactReadLimit        = 100
)

type ArtifactReadStatus string

const (
	ArtifactAvailableUnverified ArtifactReadStatus = "available_unverified"
	ArtifactReady               ArtifactReadStatus = "ready"
	ArtifactUnavailable         ArtifactReadStatus = "unavailable"
	ArtifactInvalid             ArtifactReadStatus = "invalid"
)

type ArtifactReadIssue string

const (
	ArtifactIssueSidecarMissing         ArtifactReadIssue = "sidecar_missing"
	ArtifactIssueSidecarFilenameInvalid ArtifactReadIssue = "sidecar_filename_invalid"
	ArtifactIssueSidecarNotRegular      ArtifactReadIssue = "sidecar_not_regular"
	ArtifactIssueSidecarUnreadable      ArtifactReadIssue = "sidecar_unreadable"
	ArtifactIssueSidecarTooLarge        ArtifactReadIssue = "sidecar_too_large"
	ArtifactIssueSidecarInvalidJSON     ArtifactReadIssue = "sidecar_invalid_json"
	ArtifactIssueSidecarInvalidSchema   ArtifactReadIssue = "sidecar_invalid_schema"
	ArtifactIssueSidecarInvalidMetadata ArtifactReadIssue = "sidecar_invalid_metadata"
	ArtifactIssueTarballMissing         ArtifactReadIssue = "tarball_missing"
	ArtifactIssueTarballNotRegular      ArtifactReadIssue = "tarball_not_regular"
	ArtifactIssueTarballSizeMismatch    ArtifactReadIssue = "tarball_size_mismatch"
	ArtifactIssueTarballDigestMismatch  ArtifactReadIssue = "tarball_digest_mismatch"
	ArtifactIssueTarballChanged         ArtifactReadIssue = "tarball_changed"
	ArtifactIssueTarballUnreadable      ArtifactReadIssue = "tarball_unreadable"
)

var (
	ErrInvalidArtifactRead = errors.New("operator: invalid artifact read request")
	ErrArtifactNotFound    = errors.New("operator: artifact not found")
)

type ArtifactListRequest struct {
	Status  ArtifactReadStatus
	Version string
	Limit   int
	Cursor  string
}

type ArtifactStatusCount struct {
	Status ArtifactReadStatus `json:"status"`
	Count  int                `json:"count"`
}

// ArtifactSummary is an explicit safe projection. It intentionally has no
// tarball URL, fetched-by identity, or filesystem path. Pointer fields preserve
// unknown metadata for malformed sidecars and orphan tarballs.
type ArtifactSummary struct {
	ArtifactID                 string             `json:"artifact_id"`
	Name                       *string            `json:"name"`
	Version                    *string            `json:"version"`
	SHA256                     *string            `json:"sha256"`
	Digest                     *string            `json:"digest"`
	SizeBytes                  *int64             `json:"size_bytes"`
	EnginesNode                *string            `json:"engines_node"`
	FetchedAt                  *time.Time         `json:"fetched_at"`
	Status                     ArtifactReadStatus `json:"status"`
	Issue                      *ArtifactReadIssue `json:"issue"`
	DeploymentReferences       int                `json:"deployment_references"`
	ActiveDeploymentReferences int                `json:"active_deployment_references"`
	VerifiedAt                 *time.Time         `json:"verified_at"`
}

type ArtifactListResult struct {
	SchemaVersion int                   `json:"schema_version"`
	Consistency   string                `json:"consistency"`
	EvaluatedAt   time.Time             `json:"evaluated_at"`
	Total         int                   `json:"total"`
	StatusCounts  []ArtifactStatusCount `json:"status_counts"`
	Items         []ArtifactSummary     `json:"items"`
	NextCursor    *string               `json:"next_cursor"`
}

type ArtifactDetailResult struct {
	SchemaVersion int             `json:"schema_version"`
	Consistency   string          `json:"consistency"`
	EvaluatedAt   time.Time       `json:"evaluated_at"`
	Item          ArtifactSummary `json:"item"`
}

type artifactReferenceCount struct {
	all    int
	active int
}

// ListArtifacts performs metadata-only inspection. Matching type and size is
// exposed as available_unverified, never ready; callers that need byte proof
// must request ArtifactDetail or run deployment preview/apply.
func (s *Service) ListArtifacts(request ArtifactListRequest, evaluatedAt time.Time) (ArtifactListResult, error) {
	normalized, err := normalizeArtifactListRequest(request)
	if err != nil {
		return ArtifactListResult{}, err
	}
	if s == nil || s.store == nil || strings.TrimSpace(s.artifactsDir) == "" || evaluatedAt.IsZero() {
		return ArtifactListResult{}, fmt.Errorf("%w: store, artifact catalog, and evaluated_at are required", ErrInvalidArtifactRead)
	}
	evaluatedAt = evaluatedAt.UTC()
	filterDigest := artifactFilterDigest(normalized)
	var afterID string
	if normalized.Cursor != "" {
		cursor, err := decodeArtifactListCursor(normalized.Cursor, filterDigest)
		if err != nil {
			return ArtifactListResult{}, err
		}
		afterID = cursor.ArtifactID
	}
	entries, err := artifact.ScanCatalog(s.artifactsDir)
	if err != nil {
		return ArtifactListResult{}, err
	}
	references, err := s.artifactDeploymentReferenceCounts(evaluatedAt)
	if err != nil {
		return ArtifactListResult{}, err
	}
	matching := make([]artifact.CatalogEntry, 0, len(entries))
	for i, entry := range entries {
		if i > 0 && entries[i-1].ID >= entry.ID {
			return ArtifactListResult{}, fmt.Errorf("%w: catalog order or identity is invalid", ErrInvalidArtifactRead)
		}
		if artifactEntryMatches(entry, normalized) {
			matching = append(matching, entry)
		}
	}
	result := ArtifactListResult{
		SchemaVersion: ArtifactReadSchemaVersion, Consistency: ArtifactReadConsistencyLive,
		EvaluatedAt: evaluatedAt, Total: len(matching),
		StatusCounts: make([]ArtifactStatusCount, 0, 3),
		Items:        make([]ArtifactSummary, 0, normalized.Limit),
	}
	for _, status := range []ArtifactReadStatus{ArtifactAvailableUnverified, ArtifactUnavailable, ArtifactInvalid} {
		count := 0
		for _, entry := range matching {
			if artifactReadStatus(entry.Status) == status {
				count++
			}
		}
		result.StatusCounts = append(result.StatusCounts, ArtifactStatusCount{Status: status, Count: count})
	}
	start := 0
	if afterID != "" {
		start = sort.Search(len(matching), func(i int) bool { return matching[i].ID > afterID })
	}
	end := start + normalized.Limit
	if end > len(matching) {
		end = len(matching)
	}
	for _, entry := range matching[start:end] {
		summary, err := projectArtifactSummary(entry, references[entry.SHA256], nil)
		if err != nil {
			return ArtifactListResult{}, err
		}
		result.Items = append(result.Items, summary)
	}
	if end < len(matching) && len(result.Items) > 0 {
		cursor, err := encodeArtifactListCursor(artifactListCursor{
			Version: ArtifactReadSchemaVersion, FilterDigest: filterDigest,
			ArtifactID: result.Items[len(result.Items)-1].ArtifactID,
		})
		if err != nil {
			return ArtifactListResult{}, err
		}
		result.NextCursor = &cursor
	}
	return result, nil
}

// ArtifactDetail performs the expensive full SHA-256 verification. VerifiedAt
// is present only when the current bytes reached ready; a failed or unavailable
// artifact must never carry a successful verification timestamp.
func (s *Service) ArtifactDetail(ctx context.Context, artifactID string, evaluatedAt time.Time) (ArtifactDetailResult, error) {
	if s == nil || s.store == nil || ctx == nil || strings.TrimSpace(s.artifactsDir) == "" || evaluatedAt.IsZero() ||
		!validArtifactReadID(artifactID) {
		return ArtifactDetailResult{}, fmt.Errorf("%w: context, artifact_id, store, catalog, and evaluated_at are required", ErrInvalidArtifactRead)
	}
	if err := ctx.Err(); err != nil {
		return ArtifactDetailResult{}, err
	}
	evaluatedAt = evaluatedAt.UTC()
	entry, err := artifact.InspectCatalogEntry(ctx, s.artifactsDir, artifactID)
	if err != nil {
		if errors.Is(err, artifact.ErrCatalogEntryNotFound) {
			return ArtifactDetailResult{}, ErrArtifactNotFound
		}
		return ArtifactDetailResult{}, err
	}
	references, err := s.artifactDeploymentReferenceCounts(evaluatedAt)
	if err != nil {
		return ArtifactDetailResult{}, err
	}
	var verifiedAt *time.Time
	if entry.Status == artifact.CatalogReady {
		// evaluatedAt is the single request clock used by every read DTO. The
		// full hash completed within this request; introducing time.Now here
		// would make one response carry two unrelated clocks.
		value := evaluatedAt
		verifiedAt = &value
	}
	summary, err := projectArtifactSummary(entry, references[entry.SHA256], verifiedAt)
	if err != nil {
		return ArtifactDetailResult{}, err
	}
	return ArtifactDetailResult{
		SchemaVersion: ArtifactReadSchemaVersion, Consistency: ArtifactReadConsistencyLive,
		EvaluatedAt: evaluatedAt, Item: summary,
	}, nil
}

func normalizeArtifactListRequest(request ArtifactListRequest) (ArtifactListRequest, error) {
	if request.Status != "" && request.Status != ArtifactAvailableUnverified &&
		request.Status != ArtifactUnavailable && request.Status != ArtifactInvalid {
		return ArtifactListRequest{}, fmt.Errorf("%w: status must be available_unverified, unavailable, or invalid", ErrInvalidArtifactRead)
	}
	if request.Version != "" && !validDeploymentIdentifier(request.Version, 128) {
		return ArtifactListRequest{}, fmt.Errorf("%w: version is invalid", ErrInvalidArtifactRead)
	}
	if request.Limit == 0 {
		request.Limit = DefaultArtifactReadLimit
	}
	if request.Limit < 1 || request.Limit > MaxArtifactReadLimit {
		return ArtifactListRequest{}, fmt.Errorf("%w: limit must be between 1 and %d", ErrInvalidArtifactRead, MaxArtifactReadLimit)
	}
	return request, nil
}

func artifactEntryMatches(entry artifact.CatalogEntry, request ArtifactListRequest) bool {
	if request.Status != "" && artifactReadStatus(entry.Status) != request.Status {
		return false
	}
	return request.Version == "" || (entry.Record != nil && entry.Record.Version == request.Version)
}

func projectArtifactSummary(entry artifact.CatalogEntry, references artifactReferenceCount,
	verifiedAt *time.Time,
) (ArtifactSummary, error) {
	status := artifactReadStatus(entry.Status)
	issue, issueOK := artifactReadIssue(entry.Issue)
	if status == "" || entry.ID == "" || !issueOK || references.all < 0 || references.active < 0 || references.active > references.all {
		return ArtifactSummary{}, fmt.Errorf("%w: catalog entry is incoherent", ErrInvalidArtifactRead)
	}
	if (entry.SHA256 == "") != strings.HasPrefix(entry.ID, "invalid-") ||
		entry.SHA256 != "" && entry.ID != entry.SHA256 ||
		entry.Record != nil && entry.Record.SHA256 != entry.SHA256 {
		return ArtifactSummary{}, fmt.Errorf("%w: catalog identity is incoherent", ErrInvalidArtifactRead)
	}
	switch status {
	case ArtifactAvailableUnverified:
		if entry.Record == nil || issue != "" || verifiedAt != nil {
			return ArtifactSummary{}, fmt.Errorf("%w: unverified artifact evidence is incoherent", ErrInvalidArtifactRead)
		}
	case ArtifactReady:
		if entry.Record == nil || issue != "" || verifiedAt == nil {
			return ArtifactSummary{}, fmt.Errorf("%w: ready artifact evidence is incoherent", ErrInvalidArtifactRead)
		}
	case ArtifactUnavailable:
		if (issue != ArtifactIssueSidecarMissing && issue != ArtifactIssueTarballMissing) || verifiedAt != nil {
			return ArtifactSummary{}, fmt.Errorf("%w: unavailable artifact evidence is incoherent", ErrInvalidArtifactRead)
		}
	case ArtifactInvalid:
		if issue == "" || issue == ArtifactIssueSidecarMissing || issue == ArtifactIssueTarballMissing || verifiedAt != nil {
			return ArtifactSummary{}, fmt.Errorf("%w: invalid artifact evidence is incoherent", ErrInvalidArtifactRead)
		}
	}
	result := ArtifactSummary{
		ArtifactID: entry.ID, Status: status,
		DeploymentReferences: references.all, ActiveDeploymentReferences: references.active,
	}
	if entry.SHA256 != "" {
		if !artifact.ValidSHA256Hex(entry.SHA256) {
			return ArtifactSummary{}, fmt.Errorf("%w: catalog digest is invalid", ErrInvalidArtifactRead)
		}
		sha, digest := entry.SHA256, "sha256:"+entry.SHA256
		result.SHA256, result.Digest = &sha, &digest
	}
	if entry.Record != nil {
		name, version, size := entry.Record.Name, entry.Record.Version, entry.Record.Size
		fetchedAt := entry.Record.FetchedAt.UTC()
		result.Name, result.Version, result.SizeBytes, result.FetchedAt = &name, &version, &size, &fetchedAt
		if entry.Record.EnginesNode != "" {
			engines := entry.Record.EnginesNode
			result.EnginesNode = &engines
		}
	}
	if issue != "" {
		result.Issue = &issue
	}
	if verifiedAt != nil {
		if status != ArtifactReady || verifiedAt.IsZero() {
			return ArtifactSummary{}, fmt.Errorf("%w: verification timestamp contradicts status", ErrInvalidArtifactRead)
		}
		value := verifiedAt.UTC()
		result.VerifiedAt = &value
	} else if status == ArtifactReady {
		return ArtifactSummary{}, fmt.Errorf("%w: ready artifact lacks verification timestamp", ErrInvalidArtifactRead)
	}
	return result, nil
}

func artifactReadStatus(status artifact.CatalogStatus) ArtifactReadStatus {
	switch status {
	case artifact.CatalogAvailableUnverified:
		return ArtifactAvailableUnverified
	case artifact.CatalogReady:
		return ArtifactReady
	case artifact.CatalogUnavailable:
		return ArtifactUnavailable
	case artifact.CatalogInvalid:
		return ArtifactInvalid
	default:
		return ""
	}
}

func artifactReadIssue(issue artifact.CatalogIssue) (ArtifactReadIssue, bool) {
	switch issue {
	case "":
		return "", true
	case artifact.CatalogIssueSidecarMissing:
		return ArtifactIssueSidecarMissing, true
	case artifact.CatalogIssueSidecarFilenameInvalid:
		return ArtifactIssueSidecarFilenameInvalid, true
	case artifact.CatalogIssueSidecarNotRegular:
		return ArtifactIssueSidecarNotRegular, true
	case artifact.CatalogIssueSidecarUnreadable:
		return ArtifactIssueSidecarUnreadable, true
	case artifact.CatalogIssueSidecarTooLarge:
		return ArtifactIssueSidecarTooLarge, true
	case artifact.CatalogIssueSidecarInvalidJSON:
		return ArtifactIssueSidecarInvalidJSON, true
	case artifact.CatalogIssueSidecarInvalidSchema:
		return ArtifactIssueSidecarInvalidSchema, true
	case artifact.CatalogIssueSidecarInvalidMetadata:
		return ArtifactIssueSidecarInvalidMetadata, true
	case artifact.CatalogIssueTarballMissing:
		return ArtifactIssueTarballMissing, true
	case artifact.CatalogIssueTarballNotRegular:
		return ArtifactIssueTarballNotRegular, true
	case artifact.CatalogIssueTarballSizeMismatch:
		return ArtifactIssueTarballSizeMismatch, true
	case artifact.CatalogIssueTarballDigestMismatch:
		return ArtifactIssueTarballDigestMismatch, true
	case artifact.CatalogIssueTarballChanged:
		return ArtifactIssueTarballChanged, true
	case artifact.CatalogIssueTarballUnreadable:
		return ArtifactIssueTarballUnreadable, true
	default:
		return "", false
	}
}

func (s *Service) artifactDeploymentReferenceCounts(evaluatedAt time.Time) (map[string]artifactReferenceCount, error) {
	views, err := s.store.ListDeployments(evaluatedAt)
	if err != nil {
		return nil, err
	}
	result := make(map[string]artifactReferenceCount)
	for _, view := range views {
		if view.ResourceKind != "openclaw" || view.ResourceID != "openclaw" {
			continue
		}
		var spec model.OpenClawSpec
		if json.Unmarshal([]byte(view.Spec), &spec) != nil || spec.Kind != "openclaw" || spec.Artifact == nil ||
			!artifact.ValidSHA256Hex(spec.Artifact.SHA256) {
			continue
		}
		count := result[spec.Artifact.SHA256]
		count.all++
		if view.State != store.DeploymentFinished {
			count.active++
		}
		result[spec.Artifact.SHA256] = count
	}
	return result, nil
}

type artifactListCursor struct {
	Version      int    `json:"v"`
	FilterDigest string `json:"filter_digest"`
	ArtifactID   string `json:"artifact_id"`
}

func artifactFilterDigest(request ArtifactListRequest) string {
	body := struct {
		Version         int                `json:"v"`
		Status          ArtifactReadStatus `json:"status"`
		ArtifactVersion string             `json:"artifact_version"`
	}{ArtifactReadSchemaVersion, request.Status, request.Version}
	raw, _ := json.Marshal(body)
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func encodeArtifactListCursor(cursor artifactListCursor) (string, error) {
	raw, err := json.Marshal(cursor)
	if err != nil {
		return "", fmt.Errorf("operator: encode artifact cursor: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func decodeArtifactListCursor(encoded, filterDigest string) (artifactListCursor, error) {
	if encoded == "" || len(encoded) > 2048 {
		return artifactListCursor{}, fmt.Errorf("%w: cursor length is invalid", ErrInvalidArtifactRead)
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || base64.RawURLEncoding.EncodeToString(raw) != encoded {
		return artifactListCursor{}, fmt.Errorf("%w: cursor encoding is invalid", ErrInvalidArtifactRead)
	}
	var cursor artifactListCursor
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cursor); err != nil {
		return artifactListCursor{}, fmt.Errorf("%w: cursor document is invalid", ErrInvalidArtifactRead)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return artifactListCursor{}, fmt.Errorf("%w: cursor has trailing JSON", ErrInvalidArtifactRead)
	}
	canonical, err := encodeArtifactListCursor(cursor)
	if err != nil || canonical != encoded || cursor.Version != ArtifactReadSchemaVersion ||
		cursor.FilterDigest != filterDigest || !validArtifactReadID(cursor.ArtifactID) {
		return artifactListCursor{}, fmt.Errorf("%w: cursor does not match this query", ErrInvalidArtifactRead)
	}
	return cursor, nil
}

func validArtifactReadID(value string) bool {
	if artifact.ValidSHA256Hex(value) {
		return true
	}
	digest, ok := strings.CutPrefix(value, "invalid-")
	return ok && artifact.ValidSHA256Hex(digest)
}
