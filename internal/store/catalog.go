package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	appcatalog "github.com/teddashh/AI-Intune/internal/catalog"
)

const maxStoredCatalogRecords = 4096

var (
	ErrCatalogManifestConflict = errors.New("store: catalog package identity already has different content")
	ErrMachineProfileConflict  = errors.New("store: machine profile identity already has different content")
	ErrCatalogCapacity         = errors.New("store: catalog capacity reached")
	ErrCatalogRecordCorrupt    = errors.New("store: catalog record is corrupt")
)

type CatalogManifestRecord struct {
	Manifest    appcatalog.Manifest
	Digest      string
	PublishedAt time.Time
	PublishedBy string
	Replayed    bool
}

type MachineProfileRecord struct {
	Profile     appcatalog.MachineProfile
	Digest      string
	PublishedAt time.Time
	PublishedBy string
	Replayed    bool
}

func (s *Store) PublishCatalogManifest(manifest appcatalog.Manifest, publishedBy string) (CatalogManifestRecord, error) {
	canonical, raw, digest, err := canonicalCatalogManifest(manifest)
	if err != nil {
		return CatalogManifestRecord{}, err
	}
	if !validCatalogPublisher(publishedBy) {
		return CatalogManifestRecord{}, errors.New("store: catalog publisher is invalid")
	}
	now := s.now().UTC().Truncate(time.Second)
	tx, err := s.beginWrite(context.Background(), "publish_catalog_manifest")
	if err != nil {
		return CatalogManifestRecord{}, fmt.Errorf("store: begin catalog manifest publication: %w", err)
	}
	defer tx.Rollback()
	record, err := scanCatalogManifest(tx.QueryRow(`SELECT manifest_json,manifest_digest,published_at,published_by
	 FROM catalog_manifests WHERE package_id=? AND package_version=?`, canonical.ID, canonical.Version), canonical.ID, canonical.Version)
	if err == nil {
		if record.Digest != digest {
			return CatalogManifestRecord{}, ErrCatalogManifestConflict
		}
		record.Replayed = true
		return record, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return CatalogManifestRecord{}, err
	}
	if err := ensureCatalogPublicationCapacity(tx, "catalog_manifests"); err != nil {
		return CatalogManifestRecord{}, err
	}
	if _, err := tx.Exec(`INSERT INTO catalog_manifests
	 (package_id,package_version,manifest_json,manifest_digest,published_at,published_by)
	 VALUES (?,?,?,?,?,?)`,
		canonical.ID, canonical.Version, raw, digest, fmtTime(now), publishedBy); err != nil {
		return CatalogManifestRecord{}, fmt.Errorf("store: publish catalog manifest: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return CatalogManifestRecord{}, fmt.Errorf("store: commit catalog manifest publication: %w", err)
	}
	return CatalogManifestRecord{Manifest: canonical, Digest: digest, PublishedAt: now, PublishedBy: publishedBy}, nil
}

func (s *Store) CatalogManifest(packageID, version string) (CatalogManifestRecord, error) {
	return scanCatalogManifest(s.rdb.QueryRow(`SELECT manifest_json,manifest_digest,published_at,published_by
	 FROM catalog_manifests WHERE package_id=? AND package_version=?`, packageID, version), packageID, version)
}

func (s *Store) CatalogManifests() ([]CatalogManifestRecord, error) {
	rows, err := s.rdb.Query(`SELECT package_id,package_version,manifest_json,manifest_digest,published_at,published_by
	 FROM catalog_manifests ORDER BY package_id,package_version LIMIT ?`, maxStoredCatalogRecords+1)
	if err != nil {
		return nil, fmt.Errorf("store: list catalog manifests: %w", err)
	}
	defer rows.Close()
	records := make([]CatalogManifestRecord, 0)
	for rows.Next() {
		var packageID, version, raw, digest, publishedAt, publishedBy string
		if err := rows.Scan(&packageID, &version, &raw, &digest, &publishedAt, &publishedBy); err != nil {
			return nil, fmt.Errorf("store: scan catalog manifest: %w", err)
		}
		record, err := decodeCatalogManifestRecord(packageID, version, raw, digest, publishedAt, publishedBy)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate catalog manifests: %w", err)
	}
	if len(records) > maxStoredCatalogRecords {
		return nil, fmt.Errorf("%w: catalog manifest count exceeds %d", ErrCatalogRecordCorrupt, maxStoredCatalogRecords)
	}
	return records, nil
}

func (s *Store) PublishMachineProfile(profile appcatalog.MachineProfile, publishedBy string) (MachineProfileRecord, error) {
	canonical, raw, digest, err := canonicalMachineProfile(profile)
	if err != nil {
		return MachineProfileRecord{}, err
	}
	if !validCatalogPublisher(publishedBy) {
		return MachineProfileRecord{}, errors.New("store: profile publisher is invalid")
	}
	existing, err := s.MachineProfile(canonical.ID, canonical.Revision)
	if err == nil {
		if existing.Digest != digest {
			return MachineProfileRecord{}, ErrMachineProfileConflict
		}
		existing.Replayed = true
		return existing, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return MachineProfileRecord{}, err
	}
	manifests, err := s.CatalogManifests()
	if err != nil {
		return MachineProfileRecord{}, err
	}
	if err := validateStoredProfile(canonical, manifests); err != nil {
		return MachineProfileRecord{}, err
	}
	now := s.now().UTC().Truncate(time.Second)
	tx, err := s.beginWrite(context.Background(), "publish_machine_profile")
	if err != nil {
		return MachineProfileRecord{}, fmt.Errorf("store: begin machine profile publication: %w", err)
	}
	defer tx.Rollback()
	existing, err = scanMachineProfile(tx.QueryRow(`SELECT profile_json,profile_digest,published_at,published_by
	 FROM machine_profiles WHERE profile_id=? AND profile_revision=?`, canonical.ID, canonical.Revision), canonical.ID, canonical.Revision)
	if err == nil {
		if existing.Digest != digest {
			return MachineProfileRecord{}, ErrMachineProfileConflict
		}
		existing.Replayed = true
		return existing, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return MachineProfileRecord{}, err
	}
	if err := ensureCatalogPublicationCapacity(tx, "machine_profiles"); err != nil {
		return MachineProfileRecord{}, err
	}
	if _, err := tx.Exec(`INSERT INTO machine_profiles
	 (profile_id,profile_revision,profile_json,profile_digest,published_at,published_by)
	 VALUES (?,?,?,?,?,?)`,
		canonical.ID, canonical.Revision, raw, digest, fmtTime(now), publishedBy); err != nil {
		return MachineProfileRecord{}, fmt.Errorf("store: publish machine profile: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return MachineProfileRecord{}, fmt.Errorf("store: commit machine profile publication: %w", err)
	}
	return MachineProfileRecord{Profile: canonical, Digest: digest, PublishedAt: now, PublishedBy: publishedBy}, nil
}

func (s *Store) MachineProfile(profileID string, revision int64) (MachineProfileRecord, error) {
	return scanMachineProfile(s.rdb.QueryRow(`SELECT profile_json,profile_digest,published_at,published_by
	 FROM machine_profiles WHERE profile_id=? AND profile_revision=?`, profileID, revision), profileID, revision)
}

func (s *Store) MachineProfiles() ([]MachineProfileRecord, error) {
	rows, err := s.rdb.Query(`SELECT profile_id,profile_revision,profile_json,profile_digest,published_at,published_by
	 FROM machine_profiles ORDER BY profile_id,profile_revision DESC LIMIT ?`, maxStoredCatalogRecords+1)
	if err != nil {
		return nil, fmt.Errorf("store: list machine profiles: %w", err)
	}
	defer rows.Close()
	records := make([]MachineProfileRecord, 0)
	for rows.Next() {
		var profileID, raw, digest, publishedAt, publishedBy string
		var revision int64
		if err := rows.Scan(&profileID, &revision, &raw, &digest, &publishedAt, &publishedBy); err != nil {
			return nil, fmt.Errorf("store: scan machine profile: %w", err)
		}
		record, err := decodeMachineProfileRecord(profileID, revision, raw, digest, publishedAt, publishedBy)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate machine profiles: %w", err)
	}
	if len(records) > maxStoredCatalogRecords {
		return nil, fmt.Errorf("%w: machine profile count exceeds %d", ErrCatalogRecordCorrupt, maxStoredCatalogRecords)
	}
	return records, nil
}

func (s *Store) ResolveMachineProfile(profileID string, revision int64, target appcatalog.Platform) (appcatalog.Plan, error) {
	profile, err := s.MachineProfile(profileID, revision)
	if err != nil {
		return appcatalog.Plan{}, err
	}
	records, err := s.CatalogManifests()
	if err != nil {
		return appcatalog.Plan{}, err
	}
	manifests := make([]appcatalog.Manifest, 0, len(records))
	for _, record := range records {
		manifests = append(manifests, record.Manifest)
	}
	index, err := appcatalog.New(manifests)
	if err != nil {
		return appcatalog.Plan{}, fmt.Errorf("%w: %v", ErrCatalogRecordCorrupt, err)
	}
	return index.Resolve(profile.Profile, target)
}

type catalogManifestScanner interface {
	Scan(dest ...any) error
}

type catalogCounter interface {
	QueryRow(query string, args ...any) *sql.Row
}

func ensureCatalogPublicationCapacity(q catalogCounter, table string) error {
	var count int
	if err := q.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&count); err != nil {
		return fmt.Errorf("store: count %s: %w", table, err)
	}
	if count >= maxStoredCatalogRecords {
		return fmt.Errorf("%w: %s has %d records", ErrCatalogCapacity, table, count)
	}
	return nil
}

func scanCatalogManifest(row catalogManifestScanner, packageID, version string) (CatalogManifestRecord, error) {
	var raw, digest, publishedAt, publishedBy string
	if err := row.Scan(&raw, &digest, &publishedAt, &publishedBy); errors.Is(err, sql.ErrNoRows) {
		return CatalogManifestRecord{}, ErrNotFound
	} else if err != nil {
		return CatalogManifestRecord{}, fmt.Errorf("store: read catalog manifest: %w", err)
	}
	return decodeCatalogManifestRecord(packageID, version, raw, digest, publishedAt, publishedBy)
}

func scanMachineProfile(row catalogManifestScanner, profileID string, revision int64) (MachineProfileRecord, error) {
	var raw, digest, publishedAt, publishedBy string
	if err := row.Scan(&raw, &digest, &publishedAt, &publishedBy); errors.Is(err, sql.ErrNoRows) {
		return MachineProfileRecord{}, ErrNotFound
	} else if err != nil {
		return MachineProfileRecord{}, fmt.Errorf("store: read machine profile: %w", err)
	}
	return decodeMachineProfileRecord(profileID, revision, raw, digest, publishedAt, publishedBy)
}

func decodeCatalogManifestRecord(packageID, version, raw, digest, publishedAt, publishedBy string) (CatalogManifestRecord, error) {
	manifest, canonicalRaw, canonicalDigest, err := canonicalCatalogManifestJSON([]byte(raw))
	if err != nil {
		return CatalogManifestRecord{}, fmt.Errorf("%w: manifest document: %v", ErrCatalogRecordCorrupt, err)
	}
	if manifest.ID != packageID || manifest.Version != version {
		return CatalogManifestRecord{}, fmt.Errorf("%w: manifest identity", ErrCatalogRecordCorrupt)
	}
	if raw != canonicalRaw || digest != canonicalDigest {
		return CatalogManifestRecord{}, fmt.Errorf("%w: manifest digest", ErrCatalogRecordCorrupt)
	}
	if !validCatalogPublisher(publishedBy) {
		return CatalogManifestRecord{}, fmt.Errorf("%w: manifest publisher", ErrCatalogRecordCorrupt)
	}
	at := parseTime(publishedAt)
	if at.IsZero() || fmtTime(at) != publishedAt {
		return CatalogManifestRecord{}, fmt.Errorf("%w: manifest publication time", ErrCatalogRecordCorrupt)
	}
	return CatalogManifestRecord{Manifest: manifest, Digest: digest, PublishedAt: at, PublishedBy: publishedBy}, nil
}

func decodeMachineProfileRecord(profileID string, revision int64, raw, digest, publishedAt, publishedBy string) (MachineProfileRecord, error) {
	profile, canonicalRaw, canonicalDigest, err := canonicalMachineProfileJSON([]byte(raw))
	if err != nil {
		return MachineProfileRecord{}, fmt.Errorf("%w: profile document: %v", ErrCatalogRecordCorrupt, err)
	}
	if profile.ID != profileID || profile.Revision != revision {
		return MachineProfileRecord{}, fmt.Errorf("%w: profile identity", ErrCatalogRecordCorrupt)
	}
	if raw != canonicalRaw || digest != canonicalDigest {
		return MachineProfileRecord{}, fmt.Errorf("%w: profile digest", ErrCatalogRecordCorrupt)
	}
	if !validCatalogPublisher(publishedBy) {
		return MachineProfileRecord{}, fmt.Errorf("%w: profile publisher", ErrCatalogRecordCorrupt)
	}
	at := parseTime(publishedAt)
	if at.IsZero() || fmtTime(at) != publishedAt {
		return MachineProfileRecord{}, fmt.Errorf("%w: profile publication time", ErrCatalogRecordCorrupt)
	}
	return MachineProfileRecord{Profile: profile, Digest: digest, PublishedAt: at, PublishedBy: publishedBy}, nil
}

func canonicalCatalogManifest(manifest appcatalog.Manifest) (appcatalog.Manifest, string, string, error) {
	raw, err := json.Marshal(manifest)
	if err != nil {
		return appcatalog.Manifest{}, "", "", fmt.Errorf("store: encode catalog manifest: %w", err)
	}
	return canonicalCatalogManifestJSON(raw)
}

func canonicalCatalogManifestJSON(raw []byte) (appcatalog.Manifest, string, string, error) {
	manifest, err := appcatalog.ParseManifest(raw)
	if err != nil {
		return appcatalog.Manifest{}, "", "", err
	}
	canonical, err := json.Marshal(manifest)
	if err != nil {
		return appcatalog.Manifest{}, "", "", fmt.Errorf("store: encode canonical catalog manifest: %w", err)
	}
	return manifest, string(canonical), sha256Digest(canonical), nil
}

func canonicalMachineProfile(profile appcatalog.MachineProfile) (appcatalog.MachineProfile, string, string, error) {
	raw, err := json.Marshal(profile)
	if err != nil {
		return appcatalog.MachineProfile{}, "", "", fmt.Errorf("store: encode machine profile: %w", err)
	}
	return canonicalMachineProfileJSON(raw)
}

func canonicalMachineProfileJSON(raw []byte) (appcatalog.MachineProfile, string, string, error) {
	profile, err := appcatalog.ParseProfile(raw)
	if err != nil {
		return appcatalog.MachineProfile{}, "", "", err
	}
	canonical, err := json.Marshal(profile)
	if err != nil {
		return appcatalog.MachineProfile{}, "", "", fmt.Errorf("store: encode canonical machine profile: %w", err)
	}
	return profile, string(canonical), sha256Digest(canonical), nil
}

func sha256Digest(raw []byte) string {
	digest := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func validateStoredProfile(profile appcatalog.MachineProfile, records []CatalogManifestRecord) error {
	manifests := make([]appcatalog.Manifest, 0, len(records))
	targets := make(map[appcatalog.Platform]struct{})
	for _, record := range records {
		manifests = append(manifests, record.Manifest)
		for _, platform := range record.Manifest.Platforms {
			targets[platform] = struct{}{}
		}
	}
	index, err := appcatalog.New(manifests)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrCatalogRecordCorrupt, err)
	}
	orderedTargets := make([]appcatalog.Platform, 0, len(targets))
	for target := range targets {
		orderedTargets = append(orderedTargets, target)
	}
	sort.Slice(orderedTargets, func(i, j int) bool {
		if orderedTargets[i].OS != orderedTargets[j].OS {
			return orderedTargets[i].OS < orderedTargets[j].OS
		}
		return orderedTargets[i].Arch < orderedTargets[j].Arch
	})
	var firstErr error
	for _, target := range orderedTargets {
		if _, err := index.Resolve(profile, target); err == nil {
			return nil
		} else if firstErr == nil {
			firstErr = err
		}
	}
	if firstErr != nil {
		return firstErr
	}
	return &appcatalog.ResolutionError{Code: appcatalog.CodeMissingPackage, Package: profile.Packages[0].PackageID + "@" + profile.Packages[0].Version}
}

func validCatalogPublisher(value string) bool {
	if value == "" || value != strings.TrimSpace(value) || len(value) > 512 || !utf8.ValidString(value) {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return false
		}
	}
	return true
}
