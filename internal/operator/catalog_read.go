package operator

import (
	"bytes"
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

	appcatalog "github.com/teddashh/AI-Intune/internal/catalog"
)

const (
	CatalogReadSchemaVersion   = 1
	CatalogReadConsistencyLive = "live"
	DefaultCatalogReadLimit    = 50
	MaxCatalogReadLimit        = 100
)

var ErrInvalidCatalogRead = errors.New("operator: invalid catalog read request")

type CatalogManifestListRequest struct {
	PackageID string
	Kind      appcatalog.PackageKind
	Limit     int
	Cursor    string
}

type CatalogManifestListResult struct {
	SchemaVersion int                     `json:"schema_version"`
	Consistency   string                  `json:"consistency"`
	EvaluatedAt   time.Time               `json:"evaluated_at"`
	Total         int                     `json:"total"`
	Items         []CatalogManifestRecord `json:"items"`
	NextCursor    *string                 `json:"next_cursor"`
}

type MachineProfileListRequest struct {
	ProfileID string
	Limit     int
	Cursor    string
}

type MachineProfileListResult struct {
	SchemaVersion int                    `json:"schema_version"`
	Consistency   string                 `json:"consistency"`
	EvaluatedAt   time.Time              `json:"evaluated_at"`
	Total         int                    `json:"total"`
	Items         []MachineProfileRecord `json:"items"`
	NextCursor    *string                `json:"next_cursor"`
}

func (s *Service) ListCatalogManifests(request CatalogManifestListRequest,
	evaluatedAt time.Time,
) (CatalogManifestListResult, error) {
	request, err := normalizeCatalogManifestListRequest(request)
	if err != nil || s == nil || s.store == nil || evaluatedAt.IsZero() {
		return CatalogManifestListResult{}, fmt.Errorf("%w: store, filters, and evaluated_at are required", ErrInvalidCatalogRead)
	}
	filterDigest := catalogReadFilterDigest("manifest", request.PackageID, string(request.Kind))
	after := ""
	if request.Cursor != "" {
		cursor, decodeErr := decodeCatalogReadCursor(request.Cursor, "manifest", filterDigest)
		if decodeErr != nil {
			return CatalogManifestListResult{}, decodeErr
		}
		after = cursor.Key
	}
	stored, err := s.store.CatalogManifests()
	if err != nil {
		return CatalogManifestListResult{}, err
	}
	items := make([]CatalogManifestRecord, 0, len(stored))
	for _, record := range stored {
		if request.PackageID != "" && record.Manifest.ID != request.PackageID ||
			request.Kind != "" && record.Manifest.Kind != request.Kind {
			continue
		}
		items = append(items, CatalogManifestRecord{
			Manifest: record.Manifest, Digest: record.Digest, PublishedAt: record.PublishedAt,
		})
	}
	result := CatalogManifestListResult{
		SchemaVersion: CatalogReadSchemaVersion, Consistency: CatalogReadConsistencyLive,
		EvaluatedAt: evaluatedAt.UTC(), Total: len(items), Items: make([]CatalogManifestRecord, 0, request.Limit),
	}
	start := sort.Search(len(items), func(index int) bool { return manifestReadKey(items[index]) > after })
	end := start + request.Limit
	if end > len(items) {
		end = len(items)
	}
	result.Items = append(result.Items, items[start:end]...)
	if end < len(items) && len(result.Items) > 0 {
		encoded, encodeErr := encodeCatalogReadCursor(catalogReadCursor{
			Version: CatalogReadSchemaVersion, Kind: "manifest", FilterDigest: filterDigest,
			Key: manifestReadKey(result.Items[len(result.Items)-1]),
		})
		if encodeErr != nil {
			return CatalogManifestListResult{}, encodeErr
		}
		result.NextCursor = &encoded
	}
	return result, nil
}

func (s *Service) ListMachineProfiles(request MachineProfileListRequest,
	evaluatedAt time.Time,
) (MachineProfileListResult, error) {
	request, err := normalizeMachineProfileListRequest(request)
	if err != nil || s == nil || s.store == nil || evaluatedAt.IsZero() {
		return MachineProfileListResult{}, fmt.Errorf("%w: store, filters, and evaluated_at are required", ErrInvalidCatalogRead)
	}
	filterDigest := catalogReadFilterDigest("profile", request.ProfileID, "")
	after := ""
	if request.Cursor != "" {
		cursor, decodeErr := decodeCatalogReadCursor(request.Cursor, "profile", filterDigest)
		if decodeErr != nil {
			return MachineProfileListResult{}, decodeErr
		}
		after = cursor.Key
	}
	stored, err := s.store.MachineProfiles()
	if err != nil {
		return MachineProfileListResult{}, err
	}
	items := make([]MachineProfileRecord, 0, len(stored))
	for _, record := range stored {
		if request.ProfileID != "" && record.Profile.ID != request.ProfileID {
			continue
		}
		items = append(items, MachineProfileRecord{
			Profile: record.Profile, Digest: record.Digest, PublishedAt: record.PublishedAt,
		})
	}
	// Store orders revision descending. The cursor key mirrors that order with
	// a fixed-width inverted revision so ordinary lexical keyset search applies.
	sort.Slice(items, func(i, j int) bool { return profileReadKey(items[i]) < profileReadKey(items[j]) })
	result := MachineProfileListResult{
		SchemaVersion: CatalogReadSchemaVersion, Consistency: CatalogReadConsistencyLive,
		EvaluatedAt: evaluatedAt.UTC(), Total: len(items), Items: make([]MachineProfileRecord, 0, request.Limit),
	}
	start := sort.Search(len(items), func(index int) bool { return profileReadKey(items[index]) > after })
	end := start + request.Limit
	if end > len(items) {
		end = len(items)
	}
	result.Items = append(result.Items, items[start:end]...)
	if end < len(items) && len(result.Items) > 0 {
		encoded, encodeErr := encodeCatalogReadCursor(catalogReadCursor{
			Version: CatalogReadSchemaVersion, Kind: "profile", FilterDigest: filterDigest,
			Key: profileReadKey(result.Items[len(result.Items)-1]),
		})
		if encodeErr != nil {
			return MachineProfileListResult{}, encodeErr
		}
		result.NextCursor = &encoded
	}
	return result, nil
}

func normalizeCatalogManifestListRequest(request CatalogManifestListRequest) (CatalogManifestListRequest, error) {
	if request.PackageID != "" && !validDeploymentIdentifier(request.PackageID, 128) {
		return request, ErrInvalidCatalogRead
	}
	if request.Kind != "" && request.Kind != appcatalog.KindApp && request.Kind != appcatalog.KindRuntime {
		return request, ErrInvalidCatalogRead
	}
	if request.Limit == 0 {
		request.Limit = DefaultCatalogReadLimit
	}
	if request.Limit < 1 || request.Limit > MaxCatalogReadLimit {
		return request, ErrInvalidCatalogRead
	}
	return request, nil
}

func normalizeMachineProfileListRequest(request MachineProfileListRequest) (MachineProfileListRequest, error) {
	if request.ProfileID != "" && !validDeploymentIdentifier(request.ProfileID, 128) {
		return request, ErrInvalidCatalogRead
	}
	if request.Limit == 0 {
		request.Limit = DefaultCatalogReadLimit
	}
	if request.Limit < 1 || request.Limit > MaxCatalogReadLimit {
		return request, ErrInvalidCatalogRead
	}
	return request, nil
}

func manifestReadKey(record CatalogManifestRecord) string {
	return record.Manifest.ID + "\x00" + record.Manifest.Version
}

func profileReadKey(record MachineProfileRecord) string {
	return record.Profile.ID + "\x00" + fmt.Sprintf("%019d", int64(^uint64(0)>>1)-record.Profile.Revision)
}

type catalogReadCursor struct {
	Version      int    `json:"v"`
	Kind         string `json:"kind"`
	FilterDigest string `json:"filter_digest"`
	Key          string `json:"key"`
}

func catalogReadFilterDigest(kind, first, second string) string {
	raw, _ := json.Marshal(struct {
		Version int    `json:"v"`
		Kind    string `json:"kind"`
		First   string `json:"first"`
		Second  string `json:"second"`
	}{CatalogReadSchemaVersion, kind, first, second})
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func encodeCatalogReadCursor(cursor catalogReadCursor) (string, error) {
	raw, err := json.Marshal(cursor)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func decodeCatalogReadCursor(encoded, kind, filterDigest string) (catalogReadCursor, error) {
	if encoded == "" || len(encoded) > 2048 {
		return catalogReadCursor{}, ErrInvalidCatalogRead
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || base64.RawURLEncoding.EncodeToString(raw) != encoded {
		return catalogReadCursor{}, ErrInvalidCatalogRead
	}
	var cursor catalogReadCursor
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cursor); err != nil {
		return catalogReadCursor{}, ErrInvalidCatalogRead
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return catalogReadCursor{}, ErrInvalidCatalogRead
	}
	canonical, _ := encodeCatalogReadCursor(cursor)
	if canonical != encoded || cursor.Version != CatalogReadSchemaVersion || cursor.Kind != kind ||
		cursor.FilterDigest != filterDigest || cursor.Key == "" || len(cursor.Key) > 300 ||
		strings.ContainsAny(cursor.Key, "\r\n") {
		return catalogReadCursor{}, ErrInvalidCatalogRead
	}
	return cursor, nil
}
