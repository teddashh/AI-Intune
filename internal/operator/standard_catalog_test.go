package operator

import (
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/artifact"
	appcatalog "github.com/teddashh/AI-Intune/internal/catalog"
	"github.com/teddashh/AI-Intune/internal/store"
)

func writeHermesArtifact(t *testing.T, dir, version string) artifact.Sidecar {
	t.Helper()
	body := []byte("Hermes OCI bundle " + version)
	sum := sha256.Sum256(body)
	source := sha512.Sum512([]byte("Hermes OCI source " + version))
	digest := hex.EncodeToString(sum[:])
	record := artifact.Sidecar{
		Name: "hermes-agent", Version: version,
		TarballURL: artifact.ProductionHermesRegistryOrigin + "/v2/" + artifact.HermesImageRepository +
			"/manifests/sha256:" + strings.Repeat("b", 64),
		SHA512Integrity: "sha512-" + base64.StdEncoding.EncodeToString(source[:]),
		SHA256:          digest, Size: int64(len(body)), FetchedAt: time.Now().UTC(), FetchedBy: "operator:test",
	}
	raw, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, digest+".json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, digest+".tgz"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	return record
}

func TestStandardCatalogPublishesHermesWithoutNodeDependency(t *testing.T) {
	service, _, dir, _ := catalogManifestService(t)
	record := writeHermesArtifact(t, dir, "2026.9.7")
	preview, err := service.PreviewStandardCatalogManifest(t.Context(), StandardCatalogManifestPreviewRequest{
		ArtifactSHA256: record.SHA256,
	})
	if err != nil || preview.Manifest.ID != "hermes-agent" || preview.Manifest.Title != "Hermes Agent" ||
		preview.Manifest.Kind != appcatalog.KindApp || preview.Manifest.Source.Catalog != "github.com" ||
		preview.Manifest.Source.Revision != "v2026.9.7" || len(preview.Manifest.Dependencies) != 0 ||
		preview.EnginesNode != nil || !slices.Equal(preview.Manifest.Provides, []string{appcatalog.CapabilityAgentRuntime}) ||
		!slices.Equal(preview.Manifest.Conflicts, []string{"openclaw"}) ||
		!slices.Equal(preview.Manifest.ExclusiveGroups, []string{appcatalog.ExclusiveGroupPrimaryAgentRuntime}) {
		t.Fatalf("preview=%+v err=%v", preview, err)
	}
	result, err := service.PublishStandardCatalogManifest(t.Context(), standardCatalogPublishRequest(preview, "standard-hermes"))
	if err != nil || result.Record.Manifest.ID != "hermes-agent" || result.Record.Digest != preview.ManifestDigest {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if _, err := service.PreviewStandardCatalogManifest(t.Context(), StandardCatalogManifestPreviewRequest{
		ArtifactSHA256: record.SHA256, NodeRuntimeVersion: "24.21.0",
	}); !errors.Is(err, ErrInvalidStandardCatalogPreview) {
		t.Fatalf("Hermes accepted Node dependency: %v", err)
	}
}

func TestStandardCatalogPublishesNodeThenCompatibleOpenClaw(t *testing.T) {
	service, st, dir, openClawRecord := catalogManifestService(t)
	nodeRecord := writeNodeRuntimeArtifact(t, dir, "24.21.0")

	nodePreview, err := service.PreviewStandardCatalogManifest(t.Context(), StandardCatalogManifestPreviewRequest{
		ArtifactSHA256: nodeRecord.SHA256, NodeRuntimeVersion: "",
	})
	if err != nil || nodePreview.SchemaVersion != StandardCatalogPreviewSchemaVersion ||
		nodePreview.Manifest.ID != "node-runtime" || nodePreview.Manifest.Kind != appcatalog.KindRuntime ||
		nodePreview.Manifest.Source.Catalog != "nodejs.org" || nodePreview.Manifest.Source.Revision != "v24.21.0" ||
		!slices.Equal(nodePreview.Manifest.Platforms, []appcatalog.Platform{
			{OS: "darwin", Arch: "amd64"}, {OS: "darwin", Arch: "arm64"},
			{OS: "linux", Arch: "amd64"}, {OS: "linux", Arch: "arm64"},
			{OS: "windows", Arch: "amd64"}, {OS: "windows", Arch: "arm64"},
		}) ||
		nodePreview.ManifestDigest == "" || nodePreview.PreviewDigest == "" || nodePreview.EnginesNode != nil ||
		nodePreview.AlreadyPublished {
		t.Fatalf("node preview=%+v err=%v", nodePreview, err)
	}
	nodeRequest := standardCatalogPublishRequest(nodePreview, "standard-node")
	nodePublished, err := service.PublishStandardCatalogManifest(t.Context(), nodeRequest)
	if err != nil || nodePublished.Record.Digest != nodePreview.ManifestDigest || nodePublished.Replayed {
		t.Fatalf("node publish=%+v err=%v", nodePublished, err)
	}

	openClawPreview, err := service.PreviewStandardCatalogManifest(t.Context(), StandardCatalogManifestPreviewRequest{
		ArtifactSHA256: openClawRecord.SHA256, NodeRuntimeVersion: nodeRecord.Version,
	})
	if err != nil || openClawPreview.Manifest.ID != "openclaw" || openClawPreview.Manifest.Kind != appcatalog.KindApp ||
		openClawPreview.Manifest.Source.Catalog != "npmjs.com" || openClawPreview.EnginesNode == nil ||
		len(openClawPreview.Manifest.Dependencies) != 1 || openClawPreview.Manifest.Dependencies[0] != (appcatalog.PackageRef{
		PackageID: "node-runtime", Version: nodeRecord.Version,
	}) {
		t.Fatalf("openclaw preview=%+v err=%v", openClawPreview, err)
	}
	openClawRequest := standardCatalogPublishRequest(openClawPreview, "standard-openclaw")
	openClawPublished, err := service.PublishStandardCatalogManifest(t.Context(), openClawRequest)
	if err != nil || openClawPublished.Record.Digest != openClawPreview.ManifestDigest {
		t.Fatalf("openclaw publish=%+v err=%v", openClawPublished, err)
	}

	again, err := service.PreviewStandardCatalogManifest(t.Context(), StandardCatalogManifestPreviewRequest{
		ArtifactSHA256: openClawRecord.SHA256, NodeRuntimeVersion: nodeRecord.Version,
	})
	if err != nil || !again.AlreadyPublished || again.ManifestDigest != openClawPublished.Record.Digest {
		t.Fatalf("published preview=%+v err=%v", again, err)
	}
	if _, err := st.ResolveMachineProfile("missing", 1, appcatalog.Platform{OS: "linux", Arch: "amd64"}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unexpected profile side effect: %v", err)
	}
}

func TestStandardCatalogRejectsLegacyNodeBundleWithoutDarwinTargets(t *testing.T) {
	service, _, dir, _ := catalogManifestService(t)
	legacy := writeNodeRuntimeArtifactTargets(t, dir, "24.20.0", "linux-amd64", "linux-arm64")

	_, err := service.PreviewStandardCatalogManifest(t.Context(), StandardCatalogManifestPreviewRequest{
		ArtifactSHA256: legacy.SHA256,
	})
	var rejection *store.OperatorRequestError
	if !errors.As(err, &rejection) || rejection.Code != store.OperatorCodeCatalogArtifactMismatch {
		t.Fatalf("legacy Linux-only bundle preview error=%v; want %s", err, store.OperatorCodeCatalogArtifactMismatch)
	}
}

func standardCatalogPublishRequest(preview StandardCatalogManifestPreviewResult,
	key string,
) CatalogManifestPublishRequest {
	return CatalogManifestPublishRequest{
		Manifest: preview.Manifest, ConfirmPackageID: preview.Manifest.ID,
		ConfirmVersion: preview.Manifest.Version, PreviewDigest: preview.PreviewDigest,
		Reason: "add package to standard store", IdempotencyKey: key, Actor: verifiedDeploymentActor(),
	}
}

func TestStandardCatalogRejectsMissingOrIncompatibleNodeDependency(t *testing.T) {
	service, _, dir, openClawRecord := catalogManifestService(t)
	for _, version := range []string{"24.21.0", "23.1.0"} {
		node := writeNodeRuntimeArtifact(t, dir, version)
		if version == "23.1.0" {
			request := catalogManifestRequest(node, "not-used")
			request.Manifest = nodeRuntimeManifest(node)
			if _, err := service.PublishCatalogManifest(t.Context(), request); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, version := range []string{"24.21.0", "23.1.0"} {
		_, err := service.PreviewStandardCatalogManifest(t.Context(), StandardCatalogManifestPreviewRequest{
			ArtifactSHA256: openClawRecord.SHA256, NodeRuntimeVersion: version,
		})
		var rejection *store.OperatorRequestError
		if !errors.As(err, &rejection) || rejection.Code != store.OperatorCodeCatalogDependencyInvalid {
			t.Fatalf("node=%s err=%v", version, err)
		}
	}
}

func TestStandardCatalogDurablyRejectsConfirmationAndPreviewMismatch(t *testing.T) {
	service, st, dir, _ := catalogManifestService(t)
	node := writeNodeRuntimeArtifact(t, dir, "24.21.0")
	preview, err := service.PreviewStandardCatalogManifest(t.Context(), StandardCatalogManifestPreviewRequest{
		ArtifactSHA256: node.SHA256,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name string
		code string
		edit func(*CatalogManifestPublishRequest)
	}{
		{"confirmation", store.OperatorCodeCatalogConfirmationMismatch, func(r *CatalogManifestPublishRequest) { r.ConfirmVersion = "24.20.0" }},
		{"preview", store.OperatorCodeCatalogPreviewStale, func(r *CatalogManifestPublishRequest) { r.PreviewDigest = "sha256:" + string(make([]byte, 64)) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := standardCatalogPublishRequest(preview, "standard-reject-"+test.name)
			test.edit(&request)
			for attempt := 0; attempt < 2; attempt++ {
				_, err := service.PublishStandardCatalogManifest(t.Context(), request)
				var rejection *store.OperatorRequestError
				if !errors.As(err, &rejection) || rejection.Code != test.code || rejection.Replayed != (attempt == 1) {
					t.Fatalf("attempt=%d err=%+v", attempt, err)
				}
			}
			var count int
			if err := st.DB().QueryRow(`SELECT COUNT(*) FROM operator_idempotency WHERE idempotency_key=?`, request.IdempotencyKey).Scan(&count); err != nil || count != 1 {
				t.Fatalf("receipt count=%d err=%v", count, err)
			}
		})
	}
}

func TestStandardCatalogReplayNeedsNoArtifactBytes(t *testing.T) {
	service, _, dir, _ := catalogManifestService(t)
	node := writeNodeRuntimeArtifact(t, dir, "24.21.0")
	preview, err := service.PreviewStandardCatalogManifest(t.Context(), StandardCatalogManifestPreviewRequest{
		ArtifactSHA256: node.SHA256,
	})
	if err != nil {
		t.Fatal(err)
	}
	request := standardCatalogPublishRequest(preview, "standard-replay")
	first, err := service.PublishStandardCatalogManifest(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, node.SHA256+".tgz")); err != nil {
		t.Fatal(err)
	}
	replay, err := service.PublishStandardCatalogManifest(t.Context(), request)
	if err != nil || !replay.Replayed || replay.Record.Digest != first.Record.Digest ||
		replay.Record.PublishedAt != first.Record.PublishedAt {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}
}

func TestStandardCatalogPreviewRejectsInvalidIdentity(t *testing.T) {
	service, _, _, _ := catalogManifestService(t)
	_, err := service.PreviewStandardCatalogManifest(t.Context(), StandardCatalogManifestPreviewRequest{
		ArtifactSHA256: "bad", NodeRuntimeVersion: "24.21.0",
	})
	if !errors.Is(err, ErrInvalidStandardCatalogPreview) {
		t.Fatalf("err=%v", err)
	}
	_, err = service.PreviewStandardCatalogManifest(nil, StandardCatalogManifestPreviewRequest{
		ArtifactSHA256: string(make([]byte, 64)),
	})
	if !errors.Is(err, ErrInvalidStandardCatalogPreview) {
		t.Fatalf("nil context err=%v", err)
	}
}
