package operator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/teddashh/AI-Intune/internal/artifact"
	appcatalog "github.com/teddashh/AI-Intune/internal/catalog"
	"github.com/teddashh/AI-Intune/internal/rollout"
	"github.com/teddashh/AI-Intune/internal/store"
)

const StandardCatalogPreviewSchemaVersion = 1

var ErrInvalidStandardCatalogPreview = errors.New("operator: invalid standard catalog preview")

type StandardCatalogManifestPreviewRequest struct {
	ArtifactSHA256     string `json:"artifact_sha256"`
	NodeRuntimeVersion string `json:"node_runtime_version"`
}

type StandardCatalogManifestPreviewResult struct {
	SchemaVersion    int                 `json:"schema_version"`
	Manifest         appcatalog.Manifest `json:"manifest"`
	ManifestDigest   string              `json:"manifest_digest"`
	EnginesNode      *string             `json:"engines_node"`
	AlreadyPublished bool                `json:"already_published"`
	PreviewedAt      time.Time           `json:"previewed_at"`
	PreviewDigest    string              `json:"preview_digest"`
}

func (s *Service) PreviewStandardCatalogManifest(ctx context.Context,
	request StandardCatalogManifestPreviewRequest,
) (StandardCatalogManifestPreviewResult, error) {
	if s == nil || s.store == nil || ctx == nil || !artifact.ValidSHA256Hex(request.ArtifactSHA256) {
		return StandardCatalogManifestPreviewResult{}, ErrInvalidStandardCatalogPreview
	}
	entry, err := artifact.InspectCatalogEntry(ctx, s.artifactsDir, request.ArtifactSHA256)
	if errors.Is(err, artifact.ErrCatalogEntryNotFound) || err == nil && (entry.Status != artifact.CatalogReady || entry.Record == nil) {
		return StandardCatalogManifestPreviewResult{}, catalogManifestRejection(store.OperatorCodeCatalogArtifactUnavailable)
	}
	if err != nil {
		return StandardCatalogManifestPreviewResult{}, err
	}
	if entry.Record.Name == "bat-server" {
		err = artifact.ValidateBATServerBundleTargetsContext(ctx, s.artifactsDir, *entry.Record,
			artifact.NodeRuntimeTarget{OS: "linux", Arch: "amd64"},
			artifact.NodeRuntimeTarget{OS: "linux", Arch: "arm64"})
		if err != nil {
			return StandardCatalogManifestPreviewResult{}, catalogManifestRejection(store.OperatorCodeCatalogArtifactMismatch)
		}
	} else if entry.Record.Name == "node-runtime" || entry.Record.Name == "claude-code" || entry.Record.Name == "codex" || entry.Record.Name == "grok" || entry.Record.Name == "antigravity" {
		targets := []artifact.NodeRuntimeTarget{
			{OS: "linux", Arch: "amd64"}, {OS: "linux", Arch: "arm64"},
			{OS: "darwin", Arch: "amd64"}, {OS: "darwin", Arch: "arm64"},
			{OS: "windows", Arch: "amd64"}, {OS: "windows", Arch: "arm64"},
		}
		var err error
		switch entry.Record.Name {
		case "node-runtime":
			err = artifact.ValidateNodeRuntimeBundleTargetsContext(ctx, s.artifactsDir, *entry.Record, targets...)
		case "claude-code":
			err = artifact.ValidateClaudeCodeBundleTargetsContext(ctx, s.artifactsDir, *entry.Record, targets...)
		case "codex":
			err = artifact.ValidateCodexBundleTargetsContext(ctx, s.artifactsDir, *entry.Record, targets...)
		case "grok":
			err = artifact.ValidateGrokBundleTargetsContext(ctx, s.artifactsDir, *entry.Record, targets...)
		case "antigravity":
			err = artifact.ValidateAntigravityBundleTargetsContext(ctx, s.artifactsDir, *entry.Record, targets...)
		default:
			return StandardCatalogManifestPreviewResult{}, catalogManifestRejection(store.OperatorCodeCatalogArtifactMismatch)
		}
		if err != nil {
			return StandardCatalogManifestPreviewResult{}, catalogManifestRejection(store.OperatorCodeCatalogArtifactMismatch)
		}
	}
	manifest, err := s.standardManifestFromRecord(*entry.Record, request.NodeRuntimeVersion)
	if err != nil {
		return StandardCatalogManifestPreviewResult{}, err
	}
	digest := standardManifestDigest(manifest)
	result := StandardCatalogManifestPreviewResult{
		SchemaVersion: StandardCatalogPreviewSchemaVersion, Manifest: manifest, ManifestDigest: digest,
		PreviewedAt: time.Now().UTC().Truncate(time.Second),
	}
	if entry.Record.EnginesNode != "" {
		engines := entry.Record.EnginesNode
		result.EnginesNode = &engines
	}
	if existing, existingErr := s.store.CatalogManifest(manifest.ID, manifest.Version); existingErr == nil {
		if existing.Digest != digest {
			return StandardCatalogManifestPreviewResult{}, catalogManifestRejection(store.OperatorCodeCatalogManifestConflict)
		}
		result.AlreadyPublished = true
	} else if !errors.Is(existingErr, store.ErrNotFound) {
		return StandardCatalogManifestPreviewResult{}, existingErr
	}
	result.PreviewDigest = standardCatalogPreviewDigest(manifest)
	return result, nil
}

func (s *Service) PublishStandardCatalogManifest(ctx context.Context,
	request CatalogManifestPublishRequest,
) (CatalogManifestPublishResult, error) {
	return s.publishCatalogManifest(ctx, request, func(record artifact.Sidecar) error {
		if request.ConfirmPackageID != request.Manifest.ID || request.ConfirmVersion != request.Manifest.Version {
			return catalogManifestRejection(store.OperatorCodeCatalogConfirmationMismatch)
		}
		if request.PreviewDigest != standardCatalogPreviewDigest(request.Manifest) {
			return catalogManifestRejection(store.OperatorCodeCatalogPreviewStale)
		}
		nodeVersion := ""
		if record.Name == "openclaw" && len(request.Manifest.Dependencies) == 1 &&
			request.Manifest.Dependencies[0].PackageID == "node-runtime" {
			nodeVersion = request.Manifest.Dependencies[0].Version
		}
		expected, err := s.standardManifestFromRecord(record, nodeVersion)
		if err != nil {
			return err
		}
		if standardManifestDigest(expected) != standardManifestDigest(request.Manifest) {
			return catalogManifestRejection(store.OperatorCodeCatalogPreviewStale)
		}
		return nil
	})
}

func (s *Service) standardManifestFromRecord(record artifact.Sidecar,
	nodeRuntimeVersion string,
) (appcatalog.Manifest, error) {
	platforms := []appcatalog.Platform{{OS: "linux", Arch: "amd64"}, {OS: "linux", Arch: "arm64"}}
	manifest := appcatalog.Manifest{
		SchemaVersion: appcatalog.SchemaVersion, ID: record.Name, Version: record.Version,
		Artifact: appcatalog.Artifact{SHA256: record.SHA256, Size: record.Size},
		Adapter:  appcatalog.Adapter{Name: record.Name, Version: 1}, Platforms: platforms,
		Dependencies: []appcatalog.PackageRef{}, Conflicts: []string{}, ExclusiveGroups: []string{},
	}
	switch record.Name {
	case "node-runtime":
		if nodeRuntimeVersion != "" || !artifact.ValidNodeRuntimeVersion(record.Version) || record.EnginesNode != "" {
			return appcatalog.Manifest{}, ErrInvalidStandardCatalogPreview
		}
		manifest.Platforms = append(manifest.Platforms,
			appcatalog.Platform{OS: "darwin", Arch: "amd64"},
			appcatalog.Platform{OS: "darwin", Arch: "arm64"},
			appcatalog.Platform{OS: "windows", Arch: "amd64"},
			appcatalog.Platform{OS: "windows", Arch: "arm64"},
		)
		manifest.Kind, manifest.Title = appcatalog.KindRuntime, "Node.js"
		manifest.Source = appcatalog.Source{
			Catalog: "nodejs.org", UpstreamURL: "https://nodejs.org/dist/v" + record.Version + "/",
			Revision: "v" + record.Version, License: "MIT",
		}
		manifest.Provides = []string{"runtime.node"}
	case "claude-code":
		if nodeRuntimeVersion != "" || !artifact.ValidClaudeCodeVersion(record.Version) || record.EnginesNode != "" {
			return appcatalog.Manifest{}, ErrInvalidStandardCatalogPreview
		}
		manifest.Platforms = append(manifest.Platforms,
			appcatalog.Platform{OS: "darwin", Arch: "amd64"},
			appcatalog.Platform{OS: "darwin", Arch: "arm64"},
			appcatalog.Platform{OS: "windows", Arch: "amd64"},
			appcatalog.Platform{OS: "windows", Arch: "arm64"},
		)
		manifest.Kind, manifest.Title = appcatalog.KindApp, "Claude Code"
		manifest.Source = appcatalog.Source{
			Catalog:     "downloads.claude.ai",
			UpstreamURL: "https://downloads.claude.ai/claude-code-releases/" + record.Version + "/manifest.json",
			Revision:    "v" + record.Version, License: "proprietary",
		}
		manifest.Provides = []string{"cli.claude"}
	case "codex":
		if nodeRuntimeVersion != "" || !artifact.ValidCodexVersion(record.Version) || record.EnginesNode != "" {
			return appcatalog.Manifest{}, ErrInvalidStandardCatalogPreview
		}
		manifest.Platforms = append(manifest.Platforms,
			appcatalog.Platform{OS: "darwin", Arch: "amd64"},
			appcatalog.Platform{OS: "darwin", Arch: "arm64"},
			appcatalog.Platform{OS: "windows", Arch: "amd64"},
			appcatalog.Platform{OS: "windows", Arch: "arm64"},
		)
		manifest.Kind, manifest.Title = appcatalog.KindApp, "Codex"
		manifest.Source = appcatalog.Source{
			Catalog:     "releases.openai.com",
			UpstreamURL: "https://releases.openai.com/codex/releases/" + record.Version + "/release.json",
			Revision:    "rust-v" + record.Version, License: "proprietary",
		}
		manifest.Provides = []string{"cli.codex"}
	case "grok":
		if nodeRuntimeVersion != "" || !artifact.ValidGrokVersion(record.Version) || record.EnginesNode != "" {
			return appcatalog.Manifest{}, ErrInvalidStandardCatalogPreview
		}
		manifest.Platforms = append(manifest.Platforms,
			appcatalog.Platform{OS: "darwin", Arch: "amd64"},
			appcatalog.Platform{OS: "darwin", Arch: "arm64"},
			appcatalog.Platform{OS: "windows", Arch: "amd64"},
			appcatalog.Platform{OS: "windows", Arch: "arm64"},
		)
		manifest.Kind, manifest.Title = appcatalog.KindApp, "Grok"
		manifest.Source = appcatalog.Source{
			Catalog:     "registry.npmjs.org",
			UpstreamURL: "https://registry.npmjs.org/@xai-official/grok/" + record.Version,
			Revision:    record.Version, License: "Apache-2.0",
		}
		manifest.Provides = []string{"cli.grok"}
	case "antigravity":
		if nodeRuntimeVersion != "" || !artifact.ValidAntigravityVersion(record.Version) || record.EnginesNode != "" ||
			!artifact.ValidAntigravityReleaseDirectory(record.Version, record.TarballURL) {
			return appcatalog.Manifest{}, ErrInvalidStandardCatalogPreview
		}
		manifest.Platforms = append(manifest.Platforms,
			appcatalog.Platform{OS: "darwin", Arch: "amd64"},
			appcatalog.Platform{OS: "darwin", Arch: "arm64"},
			appcatalog.Platform{OS: "windows", Arch: "amd64"},
			appcatalog.Platform{OS: "windows", Arch: "arm64"},
		)
		manifest.Kind, manifest.Title = appcatalog.KindApp, "Antigravity"
		manifest.Source = appcatalog.Source{
			Catalog: "storage.googleapis.com", UpstreamURL: record.TarballURL,
			Revision: record.Version, License: "proprietary",
		}
		manifest.Provides = []string{"cli.agy"}
	case "bat-server":
		if nodeRuntimeVersion != "" || !artifact.ValidBATServerVersion(record.Version) || record.EnginesNode != "" {
			return appcatalog.Manifest{}, ErrInvalidStandardCatalogPreview
		}
		manifest.Kind, manifest.Title = appcatalog.KindApp, "BAT Server"
		manifest.Source = appcatalog.Source{
			Catalog:     "github.com/tony1223/better-agent-terminal",
			UpstreamURL: "https://github.com/tony1223/better-agent-terminal/releases/tag/v" + record.Version,
			Revision:    record.Version, License: "MIT",
		}
		manifest.Provides = []string{"service.bat-server"}
	case "openclaw":
		if !artifact.ValidOpenClawVersion(record.Version) || !artifact.ValidNodeRuntimeVersion(nodeRuntimeVersion) ||
			strings.TrimSpace(record.EnginesNode) == "" {
			return appcatalog.Manifest{}, ErrInvalidStandardCatalogPreview
		}
		node, err := s.store.CatalogManifest("node-runtime", nodeRuntimeVersion)
		if errors.Is(err, store.ErrNotFound) {
			return appcatalog.Manifest{}, catalogManifestRejection(store.OperatorCodeCatalogDependencyInvalid)
		}
		if err != nil {
			return appcatalog.Manifest{}, err
		}
		if node.Manifest.ID != "node-runtime" || node.Manifest.Version != nodeRuntimeVersion ||
			node.Manifest.Kind != appcatalog.KindRuntime || node.Manifest.Adapter != (appcatalog.Adapter{Name: "node-runtime", Version: 1}) {
			return appcatalog.Manifest{}, catalogManifestRejection(store.OperatorCodeCatalogDependencyInvalid)
		}
		matched, understood := rollout.NodeSatisfiesRange(nodeRuntimeVersion, record.EnginesNode)
		if !understood || !matched {
			return appcatalog.Manifest{}, catalogManifestRejection(store.OperatorCodeCatalogDependencyInvalid)
		}
		manifest.Kind, manifest.Title = appcatalog.KindApp, "OpenClaw"
		manifest.Source = appcatalog.Source{
			Catalog: "npmjs.com", UpstreamURL: "https://www.npmjs.com/package/openclaw/v/" + record.Version,
			Revision: record.Version, License: "MIT",
		}
		manifest.Dependencies = []appcatalog.PackageRef{{PackageID: "node-runtime", Version: nodeRuntimeVersion}}
		manifest.Provides = []string{appcatalog.CapabilityAgentRuntime}
		manifest.Conflicts = []string{"hermes-agent"}
		manifest.ExclusiveGroups = []string{appcatalog.ExclusiveGroupPrimaryAgentRuntime}
	case "hermes-agent":
		if nodeRuntimeVersion != "" || !artifact.ValidHermesVersion(record.Version) || record.EnginesNode != "" {
			return appcatalog.Manifest{}, ErrInvalidStandardCatalogPreview
		}
		manifest.Kind, manifest.Title = appcatalog.KindApp, "Hermes Agent"
		manifest.Source = appcatalog.Source{
			Catalog: "github.com", UpstreamURL: "https://github.com/NousResearch/hermes-agent/releases/tag/v" + record.Version,
			Revision: "v" + record.Version, License: "MIT",
		}
		manifest.Provides = []string{appcatalog.CapabilityAgentRuntime}
		manifest.Conflicts = []string{"openclaw"}
		manifest.ExclusiveGroups = []string{appcatalog.ExclusiveGroupPrimaryAgentRuntime}
	default:
		return appcatalog.Manifest{}, ErrInvalidStandardCatalogPreview
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		return appcatalog.Manifest{}, err
	}
	canonical, err := appcatalog.ParseManifest(raw)
	if err != nil || !validateStandardManifestURL(canonical.Source.UpstreamURL) {
		return appcatalog.Manifest{}, fmt.Errorf("%w: generated manifest is invalid", ErrInvalidStandardCatalogPreview)
	}
	return canonical, nil
}

func standardManifestDigest(manifest appcatalog.Manifest) string {
	raw, err := json.Marshal(manifest)
	if err != nil {
		return ""
	}
	canonical, err := appcatalog.ParseManifest(raw)
	if err != nil {
		return ""
	}
	raw, _ = json.Marshal(canonical)
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func standardCatalogPreviewDigest(manifest appcatalog.Manifest) string {
	raw, _ := json.Marshal(struct {
		SchemaVersion  int    `json:"schema_version"`
		ManifestDigest string `json:"manifest_digest"`
	}{StandardCatalogPreviewSchemaVersion, standardManifestDigest(manifest)})
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func validateStandardManifestURL(value string) bool {
	return strings.HasPrefix(value, "https://nodejs.org/dist/v") ||
		strings.HasPrefix(value, "https://www.npmjs.com/package/openclaw/v/") ||
		strings.HasPrefix(value, "https://github.com/NousResearch/hermes-agent/releases/tag/v") ||
		strings.HasPrefix(value, "https://downloads.claude.ai/claude-code-releases/") ||
		strings.HasPrefix(value, "https://releases.openai.com/codex/releases/") ||
		strings.HasPrefix(value, "https://registry.npmjs.org/@xai-official/grok/") ||
		strings.HasPrefix(value, "https://storage.googleapis.com/antigravity-public/antigravity-cli/") ||
		strings.HasPrefix(value, "https://github.com/tony1223/better-agent-terminal/releases/tag/v")
}
