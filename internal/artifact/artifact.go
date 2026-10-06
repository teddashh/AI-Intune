// Package artifact 讀取並驗證 Hub 已收下的 artifact sidecar。
package artifact

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/teddashh/AI-Intune/internal/model"
)

// Sidecar 是 Hub 下載 artifact 時留下的量測紀錄。
type Sidecar struct {
	Name            string    `json:"name"`
	Version         string    `json:"version"`
	TarballURL      string    `json:"tarball_url"`
	SHA512Integrity string    `json:"sha512_integrity"`
	SHA256          string    `json:"sha256"`
	Size            int64     `json:"size"`
	EnginesNode     string    `json:"engines_node"`
	FetchedAt       time.Time `json:"fetched_at"`
	FetchedBy       string    `json:"fetched_by"`
}

// OpenClawMaterial is the exact, Hub-measured deployment material shared by
// preview and apply. Path names stay private to the Hub; callers receive only
// the artifact identity and the canonical spec delivered to an agent.
type OpenClawMaterial struct {
	Spec        string
	Digest      string
	Version     string
	EnginesNode string
	Artifact    Sidecar
}

// NodeRuntimeMaterial is one exact platform selection from a verified
// multi-platform runtime bundle.
type NodeRuntimeMaterial struct {
	Spec       string
	Digest     string
	Version    string
	TargetOS   string
	TargetArch string
	Artifact   Sidecar
}

// HermesMaterial is one exact platform selection from a verified official OCI
// image bundle.
type HermesMaterial struct {
	Spec             string
	Digest           string
	Version          string
	TargetOS         string
	TargetArch       string
	ImageReference   string
	ImageIndexDigest string
	Artifact         Sidecar
}

func ResolveHermesMaterialContext(ctx context.Context, dir, version, requestedSHA256,
	targetOS, targetArch string,
) (HermesMaterial, error) {
	if ctx == nil {
		return HermesMaterial{}, errors.New("artifact: Hermes material context is required")
	}
	if err := ctx.Err(); err != nil {
		return HermesMaterial{}, err
	}
	version = strings.TrimSpace(version)
	requestedSHA256 = strings.TrimSpace(requestedSHA256)
	if !ValidHermesVersion(version) || targetOS != "linux" || (targetArch != "amd64" && targetArch != "arm64") {
		return HermesMaterial{}, errors.New("artifact: Hermes version and target are invalid")
	}
	record, err := selectCatalogArtifact(dir, "hermes-agent", version, requestedSHA256)
	if err != nil {
		return HermesMaterial{}, err
	}
	if err := ValidateStoredArtifactContext(ctx, dir, record); err != nil {
		return HermesMaterial{}, err
	}
	prefix := ProductionHermesRegistryOrigin + "/v2/" + HermesImageRepository + "/manifests/"
	indexDigest, ok := strings.CutPrefix(record.TarballURL, prefix)
	if !ok || !validOCIDigest(indexDigest) || record.TarballURL != prefix+indexDigest ||
		record.EnginesNode != "" {
		return HermesMaterial{}, errors.New("artifact: Hermes sidecar source identity is invalid")
	}
	imageReference := hermesImageRefName + ":v" + record.Version
	raw, err := marshalCompactNoEscape(model.HermesSpec{
		Kind: "hermes-agent", Version: record.Version,
		TargetOS: targetOS, TargetArch: targetArch, BundleLayout: model.HermesOCIBundleLayoutV1,
		ImageReference: imageReference, ImageIndexDigest: indexDigest,
		Artifact: &model.ArtifactRef{
			SHA256: record.SHA256, Size: record.Size, URL: "/v1/artifacts/" + record.SHA256,
		},
	})
	if err != nil {
		return HermesMaterial{}, fmt.Errorf("artifact: encode Hermes spec: %w", err)
	}
	return HermesMaterial{
		Spec: string(raw), Digest: "sha256:" + record.SHA256, Version: record.Version,
		TargetOS: targetOS, TargetArch: targetArch, ImageReference: imageReference,
		ImageIndexDigest: indexDigest, Artifact: record,
	}, nil
}

func ResolveNodeRuntimeMaterialContext(ctx context.Context, dir, version, requestedSHA256,
	targetOS, targetArch string,
) (NodeRuntimeMaterial, error) {
	if ctx == nil {
		return NodeRuntimeMaterial{}, errors.New("artifact: Node runtime material context is required")
	}
	if err := ctx.Err(); err != nil {
		return NodeRuntimeMaterial{}, err
	}
	version = strings.TrimSpace(version)
	requestedSHA256 = strings.TrimSpace(requestedSHA256)
	if version == "" || (targetOS != "linux" && targetOS != "darwin" && targetOS != "windows") ||
		(targetArch != "amd64" && targetArch != "arm64") {
		return NodeRuntimeMaterial{}, errors.New("artifact: Node runtime version and target are required")
	}
	record, err := selectCatalogArtifact(dir, "node-runtime", version, requestedSHA256)
	if err != nil {
		return NodeRuntimeMaterial{}, err
	}
	if err := ValidateStoredArtifactContext(ctx, dir, record); err != nil {
		return NodeRuntimeMaterial{}, err
	}
	if err := ValidateNodeRuntimeBundleTargetsContext(ctx, dir, record,
		NodeRuntimeTarget{OS: targetOS, Arch: targetArch}); err != nil {
		return NodeRuntimeMaterial{}, err
	}
	raw, err := marshalCompactNoEscape(model.NodeRuntimeSpec{
		Kind: "node-runtime", Version: record.Version,
		TargetOS: targetOS, TargetArch: targetArch,
		BundleLayout: model.NodeRuntimeBundleLayoutV1,
		Artifact: &model.ArtifactRef{
			SHA256: record.SHA256, Size: record.Size,
			URL: "/v1/artifacts/" + record.SHA256,
		},
	})
	if err != nil {
		return NodeRuntimeMaterial{}, fmt.Errorf("artifact: encode Node runtime spec: %w", err)
	}
	return NodeRuntimeMaterial{
		Spec: string(raw), Digest: "sha256:" + record.SHA256,
		Version: record.Version, TargetOS: targetOS, TargetArch: targetArch,
		Artifact: record,
	}, nil
}

// ResolveOpenClawMaterial selects one already-fetched OpenClaw artifact and
// proves that its sidecar still has a corresponding regular tarball with the
// recorded size and SHA-256. Preview and apply must both call this function:
// a sidecar by itself is not evidence that bytes can safely be deployed.
func ResolveOpenClawMaterial(dir, version, requestedSHA256 string) (OpenClawMaterial, error) {
	return ResolveOpenClawMaterialContext(context.Background(), dir, version, requestedSHA256)
}

// ResolveOpenClawMaterialContext is ResolveOpenClawMaterial with cancellation
// propagated through the potentially large stored-artifact hash. Deployment
// request paths must use this form so a disconnected or timed-out operator
// does not leave a full artifact read running in the background.
func ResolveOpenClawMaterialContext(ctx context.Context, dir, version, requestedSHA256 string) (OpenClawMaterial, error) {
	if ctx == nil {
		return OpenClawMaterial{}, errors.New("artifact: OpenClaw material context is required")
	}
	if err := ctx.Err(); err != nil {
		return OpenClawMaterial{}, err
	}
	version = strings.TrimSpace(version)
	requestedSHA256 = strings.TrimSpace(requestedSHA256)
	if version == "" {
		return OpenClawMaterial{}, errors.New("artifact: OpenClaw version is required")
	}
	record, err := SelectOpenClawArtifact(dir, version, requestedSHA256)
	if err != nil {
		return OpenClawMaterial{}, err
	}
	if err := ValidateStoredArtifactContext(ctx, dir, record); err != nil {
		return OpenClawMaterial{}, err
	}
	raw, err := marshalCompactNoEscape(model.OpenClawSpec{
		Kind: "openclaw", Version: record.Version,
		Artifact: &model.ArtifactRef{
			SHA256: record.SHA256, Size: record.Size,
			URL: "/v1/artifacts/" + record.SHA256, EnginesNode: record.EnginesNode,
			// Registry provenance stays in the Hub-owned sidecar and preview
			// evidence. Agents need only the content-addressed Hub URL and the
			// digest/size they must verify; copying a signed upstream URL into a
			// job spec would disclose its query credentials to every target.
		},
	})
	if err != nil {
		return OpenClawMaterial{}, fmt.Errorf("artifact: encode OpenClaw spec: %w", err)
	}
	return OpenClawMaterial{
		Spec: string(raw), Digest: "sha256:" + record.SHA256,
		Version: record.Version, EnginesNode: record.EnginesNode, Artifact: record,
	}, nil
}

// SelectOpenClawArtifact resolves an exact version/digest identity from the
// sidecar catalog. It deliberately does not validate the tarball; callers that
// intend to preview or deploy must use ResolveOpenClawMaterial instead.
func SelectOpenClawArtifact(dir, version, requestedSHA256 string) (Sidecar, error) {
	return selectCatalogArtifact(dir, "openclaw", version, requestedSHA256)
}

func selectCatalogArtifact(dir, name, version, requestedSHA256 string) (Sidecar, error) {
	entries, err := ScanCatalog(dir)
	if err != nil {
		return Sidecar{}, fmt.Errorf("artifact: scan catalog: %w", err)
	}
	if requestedSHA256 != "" {
		if !ValidSHA256Hex(requestedSHA256) {
			return Sidecar{}, fmt.Errorf("artifact: sha256 must be 64 lowercase hex characters, got %q", requestedSHA256)
		}
		for _, entry := range entries {
			if entry.SHA256 != requestedSHA256 || entry.Record == nil {
				continue
			}
			record := *entry.Record
			if record.Name != name || record.Version != version {
				return Sidecar{}, fmt.Errorf("artifact: %s is %s@%s, not %s@%s",
					record.SHA256, record.Name, record.Version, name, version)
			}
			return record, nil
		}
		if name == "openclaw" {
			return Sidecar{}, fmt.Errorf("artifact: %s is not available; run `artifact fetch openclaw@%s` first", requestedSHA256, version)
		}
		return Sidecar{}, fmt.Errorf("artifact: %s for %s@%s is not available", requestedSHA256, name, version)
	}

	var matches []Sidecar
	for _, entry := range entries {
		if entry.Record != nil && entry.Record.Name == name && entry.Record.Version == version {
			matches = append(matches, *entry.Record)
		}
	}
	switch len(matches) {
	case 0:
		if name == "openclaw" {
			return Sidecar{}, fmt.Errorf("artifact: openclaw@%s is not available; run `artifact fetch openclaw@%s` first", version, version)
		}
		return Sidecar{}, fmt.Errorf("artifact: %s@%s is not available", name, version)
	case 1:
		return matches[0], nil
	default:
		digests := make([]string, 0, len(matches))
		for _, record := range matches {
			digests = append(digests, record.SHA256)
		}
		return Sidecar{}, fmt.Errorf("artifact: %s@%s has multiple artifacts (%s); select one sha256",
			name, version, strings.Join(digests, ", "))
	}
}

// ValidateStoredArtifact verifies the selected tarball through one opened file
// descriptor, then proves that the path still names that same regular file.
// This rejects symlinks, size drift, digest drift, and path swaps during the
// check. It does not return the filesystem path to higher layers.
func ValidateStoredArtifact(dir string, record Sidecar) error {
	return ValidateStoredArtifactContext(context.Background(), dir, record)
}

// ValidateStoredArtifactContext proves the stored bytes under the same
// canonical-directory concurrency bound used by InspectCatalogEntry. The
// Fetcher may call the background wrapper while holding its directory serial
// lock: permit holders never acquire that lock, so the lock order remains
// Fetcher serial -> hash permit and cannot form a cycle.
func ValidateStoredArtifactContext(ctx context.Context, dir string, record Sidecar) error {
	if ctx == nil {
		return errors.New("artifact: stored artifact validation context is required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if dir == "" || !ValidSHA256Hex(record.SHA256) || record.Size < 0 {
		return errors.New("artifact: stored artifact identity is invalid")
	}
	issue, err := verifyCatalogArtifactWithPermit(ctx, dir, record)
	if err != nil {
		return err
	}
	switch issue {
	case "":
		return nil
	case CatalogIssueTarballMissing:
		return fmt.Errorf("artifact: tarball %s is unavailable", record.SHA256)
	case CatalogIssueTarballNotRegular:
		return fmt.Errorf("artifact: tarball %s is not a regular file", record.SHA256)
	case CatalogIssueTarballSizeMismatch:
		return fmt.Errorf("artifact: tarball %s size does not match sidecar record %d", record.SHA256, record.Size)
	case CatalogIssueTarballChanged:
		return fmt.Errorf("artifact: tarball %s changed while being validated", record.SHA256)
	case CatalogIssueTarballDigestMismatch:
		return fmt.Errorf("artifact: tarball %s does not match its SHA-256", record.SHA256)
	case CatalogIssueTarballUnreadable:
		return fmt.Errorf("artifact: tarball %s cannot be opened or hashed", record.SHA256)
	default:
		return fmt.Errorf("artifact: tarball %s could not be validated", record.SHA256)
	}
}

func marshalCompactNoEscape(value any) ([]byte, error) {
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(out.Bytes(), []byte{'\n'}), nil
}

// ReadSidecars 讀出目錄裡全部合法 sidecar，依版本與 digest 排序。
func ReadSidecars(dir string) ([]Sidecar, error) {
	if dir == "" {
		return nil, nil
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	records := make([]Sidecar, 0)
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("讀 sidecar %s：%w", entry.Name(), err)
		}
		var record Sidecar
		if err := json.Unmarshal(b, &record); err != nil {
			return nil, fmt.Errorf("sidecar %s 不是合法 JSON：%w", entry.Name(), err)
		}
		if !ValidSHA256Hex(record.SHA256) || entry.Name() != record.SHA256+".json" {
			return nil, fmt.Errorf("sidecar %s 的 sha256 或檔名不合法", entry.Name())
		}
		records = append(records, record)
	}
	sort.Slice(records, func(i, j int) bool {
		if records[i].Version != records[j].Version {
			return records[i].Version < records[j].Version
		}
		return records[i].SHA256 < records[j].SHA256
	})
	return records, nil
}

// FileHasSHA256 重新量檔案內容，而不是相信檔名宣告的 digest。
func FileHasSHA256(path, want string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return false
	}
	return hex.EncodeToString(h.Sum(nil)) == want
}

// ValidSHA256Hex 只接受恰好 64 個小寫 hex。
func ValidSHA256Hex(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	for _, c := range value {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
