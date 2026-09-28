package operator

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/artifact"
	appcatalog "github.com/teddashh/AI-Intune/internal/catalog"
	"github.com/teddashh/AI-Intune/internal/store"
)

func catalogManifestForArtifact(record artifact.Sidecar) appcatalog.Manifest {
	return appcatalog.Manifest{
		SchemaVersion: appcatalog.SchemaVersion,
		ID:            "openclaw",
		Version:       record.Version,
		Kind:          appcatalog.KindApp,
		Title:         "OpenClaw",
		Source: appcatalog.Source{
			Catalog: "ai-intune", UpstreamURL: "https://github.com/openclaw/openclaw",
			Revision: record.Version, License: "MIT",
		},
		Artifact:     appcatalog.Artifact{SHA256: record.SHA256, Size: record.Size},
		Adapter:      appcatalog.Adapter{Name: "openclaw", Version: 1},
		Platforms:    []appcatalog.Platform{{OS: "linux", Arch: "arm64"}, {OS: "linux", Arch: "amd64"}},
		Dependencies: []appcatalog.PackageRef{},
		Provides:     []string{appcatalog.CapabilityAgentRuntime},
		Conflicts:    []string{"hermes-agent"},
		ExclusiveGroups: []string{
			appcatalog.ExclusiveGroupPrimaryAgentRuntime,
		},
	}
}

func catalogManifestService(t *testing.T) (*Service, *store.Store, string, artifact.Sidecar) {
	t.Helper()
	st := newDeploymentOperatorStore(t)
	dir := t.TempDir()
	record := writeDeploymentArtifact(t, dir, "2026.9.10", ">=24.15.0 <25", time.Now().UTC().Truncate(time.Second))
	return NewWithArtifacts(st, dir), st, dir, record
}

func catalogManifestRequest(record artifact.Sidecar, key string) CatalogManifestPublishRequest {
	return CatalogManifestPublishRequest{
		Manifest: catalogManifestForArtifact(record), Reason: "approve exact package",
		IdempotencyKey: key, Actor: verifiedDeploymentActor(),
	}
}

func TestPublishCatalogManifestBindsAdapterArtifactLedgerAndAudit(t *testing.T) {
	service, st, _, record := catalogManifestService(t)
	request := catalogManifestRequest(record, "catalog-service-publish")
	result, err := service.PublishCatalogManifest(t.Context(), request)
	if err != nil || result.Replayed || result.AlreadyPublished || !result.Audited {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if result.Record.Manifest.ID != "openclaw" || result.Record.Manifest.Version != record.Version ||
		result.Record.Manifest.Artifact.SHA256 != record.SHA256 || result.Record.Manifest.Artifact.Size != record.Size ||
		result.Record.PublishedBy != request.Actor.AuthSubject {
		t.Fatalf("record=%+v", result.Record)
	}
	var action, subject, outcome, authSubject, requestDigest string
	if err := st.DB().QueryRow(`SELECT action,subject,outcome,COALESCE(auth_subject,''),request_digest
	 FROM audit_log WHERE idempotency_key=?`, request.IdempotencyKey).Scan(
		&action, &subject, &outcome, &authSubject, &requestDigest); err != nil {
		t.Fatal(err)
	}
	if action != string(store.AuditCatalogManifest) || subject != "openclaw@"+record.Version ||
		outcome != "ok" || authSubject != request.Actor.AuthSubject ||
		requestDigest != CatalogManifestPublishSemanticDigest(request) {
		t.Fatalf("audit action=%q subject=%q outcome=%q auth=%q digest=%q", action, subject, outcome, authSubject, requestDigest)
	}
}

func TestPublishCatalogManifestReplayNeedsNoCurrentArtifactOrActor(t *testing.T) {
	service, _, dir, record := catalogManifestService(t)
	request := catalogManifestRequest(record, "catalog-service-replay")
	first, err := service.PublishCatalogManifest(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, record.SHA256+".tgz")); err != nil {
		t.Fatal(err)
	}
	replayRequest := request
	replayRequest.Actor = Actor{SourceAddr: "100.64.0.20", SourceKind: SourceKindOperatorAPI}
	replay, err := service.PublishCatalogManifest(t.Context(), replayRequest)
	if err != nil || !replay.Replayed || replay.Record.Digest != first.Record.Digest ||
		replay.Record.PublishedBy != first.Record.PublishedBy {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}
}

func TestPublishCatalogManifestRejectsUnsupportedTypedContractsBeforeArtifactRead(t *testing.T) {
	_, _, _, record := catalogManifestService(t)
	for _, test := range []struct {
		name   string
		mutate func(*appcatalog.Manifest)
	}{
		{name: "adapter", mutate: func(m *appcatalog.Manifest) { m.Adapter.Name = "shell" }},
		{name: "adapter version", mutate: func(m *appcatalog.Manifest) { m.Adapter.Version = 2 }},
		{name: "package", mutate: func(m *appcatalog.Manifest) { m.ID = "another-app" }},
		{name: "kind", mutate: func(m *appcatalog.Manifest) { m.Kind = appcatalog.KindRuntime }},
		{name: "platform", mutate: func(m *appcatalog.Manifest) { m.Platforms = []appcatalog.Platform{{OS: "darwin", Arch: "arm64"}} }},
	} {
		t.Run(test.name, func(t *testing.T) {
			st := newDeploymentOperatorStore(t)
			service := NewWithArtifacts(st, filepath.Join(t.TempDir(), "absent"))
			request := catalogManifestRequest(record, "unsupported-"+test.name)
			test.mutate(&request.Manifest)
			_, err := service.PublishCatalogManifest(t.Context(), request)
			assertDeploymentOperatorCode(t, err, store.OperatorCodeCatalogAdapterUnsupported, false)
			if count := countOperatorRows(t, st, "catalog_manifests"); count != 0 {
				t.Fatalf("published manifests=%d", count)
			}
		})
	}
}

func TestPublishCatalogManifestRejectsUnavailableAndMismatchedArtifactEvidence(t *testing.T) {
	for _, test := range []struct {
		name     string
		prepare  func(*testing.T, string, artifact.Sidecar) appcatalog.Manifest
		wantCode string
	}{
		{
			name: "missing", wantCode: store.OperatorCodeCatalogArtifactUnavailable,
			prepare: func(t *testing.T, _ string, record artifact.Sidecar) appcatalog.Manifest {
				return catalogManifestForArtifact(record)
			},
		},
		{
			name: "corrupt bytes", wantCode: store.OperatorCodeCatalogArtifactUnavailable,
			prepare: func(t *testing.T, dir string, record artifact.Sidecar) appcatalog.Manifest {
				if err := os.WriteFile(filepath.Join(dir, record.SHA256+".tgz"), []byte("changed"), 0o600); err != nil {
					t.Fatal(err)
				}
				return catalogManifestForArtifact(record)
			},
		},
		{
			name: "size", wantCode: store.OperatorCodeCatalogArtifactMismatch,
			prepare: func(t *testing.T, _ string, record artifact.Sidecar) appcatalog.Manifest {
				manifest := catalogManifestForArtifact(record)
				manifest.Artifact.Size++
				return manifest
			},
		},
		{
			name: "version", wantCode: store.OperatorCodeCatalogArtifactMismatch,
			prepare: func(t *testing.T, _ string, record artifact.Sidecar) appcatalog.Manifest {
				manifest := catalogManifestForArtifact(record)
				manifest.Version = "2026.9.11"
				return manifest
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, st, dir, record := catalogManifestService(t)
			if test.name == "missing" {
				if err := os.Remove(filepath.Join(dir, record.SHA256+".tgz")); err != nil {
					t.Fatal(err)
				}
			}
			request := catalogManifestRequest(record, "artifact-evidence-"+test.name)
			request.Manifest = test.prepare(t, dir, record)
			_, err := service.PublishCatalogManifest(t.Context(), request)
			assertDeploymentOperatorCode(t, err, test.wantCode, false)
			if count := countOperatorRows(t, st, "catalog_manifests"); count != 0 {
				t.Fatalf("published manifests=%d", count)
			}
		})
	}
}

func TestPublishCatalogManifestCanonicalDigestIgnoresManifestSetOrdering(t *testing.T) {
	_, _, _, record := catalogManifestService(t)
	first := catalogManifestRequest(record, "catalog-digest")
	second := first
	second.Manifest.Platforms = []appcatalog.Platform{
		{OS: "linux", Arch: "amd64"}, {OS: "linux", Arch: "arm64"},
	}
	if CatalogManifestPublishSemanticDigest(first) != CatalogManifestPublishSemanticDigest(second) {
		t.Fatal("canonical manifest ordering changed semantic request digest")
	}
	second.Reason = "different reason"
	if CatalogManifestPublishSemanticDigest(first) == CatalogManifestPublishSemanticDigest(second) {
		t.Fatal("reason is absent from semantic request digest")
	}
}

func TestPublishCatalogManifestCatalogIOFailureLeavesKeyRetryable(t *testing.T) {
	st := newDeploymentOperatorStore(t)
	path := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(path, []byte("file"), 0o600); err != nil {
		t.Fatal(err)
	}
	record := artifact.Sidecar{
		Name: "openclaw", Version: "2026.9.10",
		SHA256: "1111111111111111111111111111111111111111111111111111111111111111", Size: 1,
	}
	request := catalogManifestRequest(record, "catalog-io-retry")
	service := NewWithArtifacts(st, path)
	if _, err := service.PublishCatalogManifest(t.Context(), request); err == nil {
		t.Fatal("catalog I/O failure was accepted")
	}
	if count := countOperatorRows(t, st, "operator_idempotency"); count != 0 {
		t.Fatalf("idempotency rows=%d", count)
	}
	if count := countOperatorRows(t, st, "audit_log"); count != 1 {
		t.Fatalf("fallback audit rows=%d", count)
	}
}

func countOperatorRows(t *testing.T, st *store.Store, table string) int {
	t.Helper()
	var count int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestPublishCatalogManifestRejectsNilContext(t *testing.T) {
	service, _, _, record := catalogManifestService(t)
	if _, err := service.PublishCatalogManifest(nil, catalogManifestRequest(record, "nil-context")); !errors.Is(err, ErrInvalidCatalogManifestPublication) {
		t.Fatalf("err=%v", err)
	}
}
