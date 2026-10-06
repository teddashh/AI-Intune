package artifact

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/teddashh/AI-Intune/internal/model"
)

type AntigravityMaterial struct {
	Spec       string
	Digest     string
	Version    string
	TargetOS   string
	TargetArch string
	Artifact   Sidecar
}

func ValidateAntigravityBundleTargetsContext(ctx context.Context, dir string, record Sidecar,
	targets ...NodeRuntimeTarget,
) error {
	if ctx == nil || record.Name != "antigravity" || !ValidAntigravityVersion(record.Version) ||
		!ValidSHA256Hex(record.SHA256) || len(targets) == 0 {
		return errors.New("artifact: Antigravity target validation is incomplete")
	}
	wanted := make(map[string]struct{}, len(targets))
	for _, target := range targets {
		if _, ok := AntigravityOfficialFile(target.OS, target.Arch); !ok {
			return errors.New("artifact: Antigravity target is invalid")
		}
		key := target.OS + "-" + target.Arch
		if _, exists := wanted[key]; exists {
			return errors.New("artifact: Antigravity target is invalid")
		}
		wanted[key] = struct{}{}
	}
	input, err := os.Open(filepath.Join(dir, record.SHA256+".tgz"))
	if err != nil {
		return fmt.Errorf("artifact: open Antigravity bundle: %w", err)
	}
	defer input.Close()
	gz, err := gzip.NewReader(input)
	if err != nil {
		return fmt.Errorf("%w: Antigravity bundle gzip is invalid", ErrMetadataInvalid)
	}
	defer gz.Close()
	dirs, files := antigravityBundleLayout()
	reader := tar.NewReader(gz)
	seen := map[string]struct{}{}
	manifests := map[string]antigravityManifestDocument{}
	digests := map[string]string{}
	entries := 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("%w: read Antigravity bundle: %v", ErrMetadataInvalid, err)
		}
		entries++
		if entries > MaxAntigravityBundleEntries || header.Size < 0 {
			return fmt.Errorf("%w: Antigravity bundle exceeds extract limits", ErrMetadataInvalid)
		}
		name, err := cleanAntigravityBundlePath(header.Name)
		if err != nil {
			return err
		}
		if _, exists := seen[name]; exists {
			return fmt.Errorf("%w: Antigravity bundle entry is duplicated", ErrMetadataInvalid)
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if header.Size != 0 {
				return fmt.Errorf("%w: Antigravity bundle entry is not a directory", ErrMetadataInvalid)
			}
			if _, ok := dirs[name]; !ok {
				return fmt.Errorf("%w: Antigravity bundle entry is unexpected", ErrMetadataInvalid)
			}
			seen[name] = struct{}{}
		case tar.TypeReg, tar.TypeRegA:
			platform, ok := files[name]
			if !ok {
				return fmt.Errorf("%w: Antigravity bundle entry is unexpected", ErrMetadataInvalid)
			}
			key := platform.TargetOS + "-" + platform.TargetArch
			if strings.HasSuffix(name, "/manifest.json") {
				document, err := readAntigravityBundleManifest(reader, header.Size)
				if err != nil {
					return err
				}
				if document.Version != record.Version || !antigravityManifestURLNamesFile(document.URL, record.Version, platform) {
					return fmt.Errorf("%w: Antigravity manifest does not match the bundle", ErrMetadataInvalid)
				}
				manifests[key] = document
			} else {
				sum, err := hashAntigravityBundleFile(reader, header.Size)
				if err != nil {
					return err
				}
				digests[key] = sum
			}
			seen[name] = struct{}{}
		default:
			return fmt.Errorf("%w: Antigravity bundle entry is not a regular file", ErrMetadataInvalid)
		}
	}
	for _, platform := range antigravityPlatforms() {
		key := platform.TargetOS + "-" + platform.TargetArch
		document, hasManifest := manifests[key]
		sum, hasFile := digests[key]
		if !hasManifest || !hasFile || !validAntigravitySHA512(document.SHA512) || sum != document.SHA512 {
			return fmt.Errorf("%w: Antigravity bundle lacks target %s", ErrMetadataInvalid, key)
		}
		base := "antigravity/" + key
		if _, ok := seen[base]; !ok {
			return fmt.Errorf("%w: Antigravity bundle lacks target %s", ErrMetadataInvalid, key)
		}
	}
	if _, ok := seen["antigravity"]; !ok {
		return fmt.Errorf("%w: Antigravity bundle lacks its root", ErrMetadataInvalid)
	}
	for key := range wanted {
		if _, ok := digests[key]; !ok {
			return fmt.Errorf("%w: Antigravity bundle lacks target %s", ErrMetadataInvalid, key)
		}
	}
	return nil
}

func antigravityBundleLayout() (map[string]struct{}, map[string]antigravityPlatform) {
	dirs := map[string]struct{}{"antigravity": {}}
	files := map[string]antigravityPlatform{}
	for _, platform := range antigravityPlatforms() {
		base := "antigravity/" + platform.TargetOS + "-" + platform.TargetArch
		dirs[base] = struct{}{}
		files[base+"/"+platform.File] = platform
		files[base+"/manifest.json"] = platform
	}
	return dirs, files
}

func cleanAntigravityBundlePath(name string) (string, error) {
	if name == "" || len(name) > 256 || strings.HasPrefix(name, "/") || strings.Contains(name, `\`) || strings.ContainsRune(name, 0) {
		return "", fmt.Errorf("%w: Antigravity bundle path is invalid", ErrMetadataInvalid)
	}
	cleaned := path.Clean(strings.TrimSuffix(name, "/"))
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") || strings.Contains("/"+cleaned+"/", "/../") {
		return "", fmt.Errorf("%w: Antigravity bundle path is invalid", ErrMetadataInvalid)
	}
	if cleaned != "antigravity" && !strings.HasPrefix(cleaned, "antigravity/") {
		return "", fmt.Errorf("%w: Antigravity bundle path is invalid", ErrMetadataInvalid)
	}
	return cleaned, nil
}

func readAntigravityBundleManifest(reader io.Reader, size int64) (antigravityManifestDocument, error) {
	var document antigravityManifestDocument
	if size <= 0 || size > AntigravityManifestMaxBytes {
		return document, fmt.Errorf("%w: Antigravity manifest exceeds extract limits", ErrMetadataInvalid)
	}
	body, err := io.ReadAll(io.LimitReader(reader, size))
	if err != nil || int64(len(body)) != size {
		return document, fmt.Errorf("%w: Antigravity manifest is incomplete", ErrMetadataInvalid)
	}
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&document); err != nil {
		return document, fmt.Errorf("%w: Antigravity manifest is not JSON", ErrMetadataInvalid)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return document, fmt.Errorf("%w: Antigravity manifest contains trailing JSON", ErrMetadataInvalid)
	}
	if document.Version == "" || document.URL == "" || !validAntigravitySHA512(document.SHA512) {
		return document, fmt.Errorf("%w: Antigravity manifest is incomplete", ErrMetadataInvalid)
	}
	return document, nil
}

func hashAntigravityBundleFile(reader io.Reader, size int64) (string, error) {
	if size <= 0 || size > DefaultAntigravitySourceMaxBytes {
		return "", fmt.Errorf("%w: Antigravity bundle entry exceeds extract limits", ErrMetadataInvalid)
	}
	digest := sha512.New()
	n, err := io.Copy(digest, io.LimitReader(reader, size))
	if err != nil || n != size {
		return "", fmt.Errorf("%w: Antigravity bundle entry is incomplete", ErrMetadataInvalid)
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func antigravityManifestURLNamesFile(raw, version string, platform antigravityPlatform) bool {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Path == "" {
		return false
	}
	if path.Base(parsed.Path) != platform.File {
		return false
	}
	_, ok := antigravityBuildFromFileURL(version, platform.Dir, platform.File, raw)
	return ok
}

func ResolveAntigravityMaterialContext(ctx context.Context, dir, version, requestedSHA256,
	targetOS, targetArch string,
) (AntigravityMaterial, error) {
	if ctx == nil {
		return AntigravityMaterial{}, errors.New("artifact: Antigravity material context is required")
	}
	if err := ctx.Err(); err != nil {
		return AntigravityMaterial{}, err
	}
	version = strings.TrimSpace(version)
	requestedSHA256 = strings.TrimSpace(requestedSHA256)
	if _, ok := AntigravityOfficialFile(targetOS, targetArch); version == "" || !ok {
		return AntigravityMaterial{}, errors.New("artifact: Antigravity version and target are required")
	}
	record, err := selectCatalogArtifact(dir, "antigravity", version, requestedSHA256)
	if err != nil {
		return AntigravityMaterial{}, err
	}
	if err := ValidateStoredArtifactContext(ctx, dir, record); err != nil {
		return AntigravityMaterial{}, err
	}
	if err := ValidateAntigravityBundleTargetsContext(ctx, dir, record,
		NodeRuntimeTarget{OS: targetOS, Arch: targetArch}); err != nil {
		return AntigravityMaterial{}, err
	}
	raw, err := marshalCompactNoEscape(model.AntigravitySpec{
		Kind: "antigravity", Version: record.Version,
		TargetOS: targetOS, TargetArch: targetArch,
		BundleLayout: model.AntigravityBundleLayoutV1,
		Artifact: &model.ArtifactRef{
			SHA256: record.SHA256, Size: record.Size,
			URL: "/v1/artifacts/" + record.SHA256,
		},
	})
	if err != nil {
		return AntigravityMaterial{}, fmt.Errorf("artifact: encode Antigravity spec: %w", err)
	}
	return AntigravityMaterial{
		Spec: string(raw), Digest: "sha256:" + record.SHA256,
		Version: record.Version, TargetOS: targetOS, TargetArch: targetArch,
		Artifact: record,
	}, nil
}
