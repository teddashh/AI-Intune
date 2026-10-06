package artifact

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/andybalholm/brotli"
	"github.com/teddashh/AI-Intune/internal/model"
)

type GrokMaterial struct {
	Spec       string
	Digest     string
	Version    string
	TargetOS   string
	TargetArch string
	Artifact   Sidecar
}

func ValidateGrokBundleTargetsContext(ctx context.Context, dir string, record Sidecar,
	targets ...NodeRuntimeTarget,
) error {
	if ctx == nil || record.Name != "grok" || !ValidGrokVersion(record.Version) ||
		!ValidSHA256Hex(record.SHA256) || len(targets) == 0 {
		return errors.New("artifact: Grok target validation is incomplete")
	}
	wanted := make(map[string]struct{}, len(targets))
	for _, target := range targets {
		member, ok := GrokBundleMember(target.OS, target.Arch)
		if !ok {
			return errors.New("artifact: Grok target is invalid")
		}
		if _, exists := wanted[member]; exists {
			return errors.New("artifact: Grok target is invalid")
		}
		wanted[member] = struct{}{}
	}
	input, err := os.Open(filepath.Join(dir, record.SHA256+".tgz"))
	if err != nil {
		return fmt.Errorf("artifact: open Grok bundle: %w", err)
	}
	defer input.Close()
	gz, err := gzip.NewReader(input)
	if err != nil {
		return fmt.Errorf("%w: Grok bundle gzip is invalid", ErrMetadataInvalid)
	}
	defer gz.Close()
	reader := tar.NewReader(gz)
	found := map[string]struct{}{}
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
			return fmt.Errorf("%w: read Grok bundle: %v", ErrMetadataInvalid, err)
		}
		entries++
		if entries > MaxGrokBundleEntries || header.Size < 0 {
			return fmt.Errorf("%w: Grok bundle exceeds extract limits", ErrMetadataInvalid)
		}
		name, err := cleanGrokBundlePath(header.Name)
		if err != nil {
			return err
		}
		if header.Typeflag == tar.TypeDir {
			if header.Size != 0 {
				return fmt.Errorf("%w: Grok bundle entry %s is not a regular file", ErrMetadataInvalid, header.Name)
			}
			continue
		}
		if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA {
			return fmt.Errorf("%w: Grok bundle entry %s is not a regular file", ErrMetadataInvalid, header.Name)
		}
		if _, ok := wanted[name]; !ok {
			if err := discardGrokBundleEntry(reader, header.Size); err != nil {
				return err
			}
			continue
		}
		if _, exists := found[name]; exists || header.Size <= 0 || header.Size > MaxGrokBinaryBytes {
			return fmt.Errorf("%w: Grok bundle entry %s is invalid", ErrMetadataInvalid, header.Name)
		}
		if err := measureGrokBinary(reader, header.Size); err != nil {
			return err
		}
		found[name] = struct{}{}
	}
	for name := range wanted {
		if _, ok := found[name]; !ok {
			return fmt.Errorf("%w: Grok bundle lacks %s", ErrMetadataInvalid, name)
		}
	}
	return nil
}

func cleanGrokBundlePath(name string) (string, error) {
	if name == "" || len(name) > 256 || strings.HasPrefix(name, "/") || strings.Contains(name, `\`) || strings.ContainsRune(name, 0) {
		return "", fmt.Errorf("%w: Grok bundle path is invalid", ErrMetadataInvalid)
	}
	cleaned := path.Clean(strings.TrimSuffix(name, "/"))
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") || strings.Contains("/"+cleaned+"/", "/../") {
		return "", fmt.Errorf("%w: Grok bundle path is invalid", ErrMetadataInvalid)
	}
	if cleaned != "grok" && !strings.HasPrefix(cleaned, "grok/") {
		return "", fmt.Errorf("%w: Grok bundle path is invalid", ErrMetadataInvalid)
	}
	return cleaned, nil
}

func discardGrokBundleEntry(reader io.Reader, size int64) error {
	if size < 0 || size > MaxGrokBinaryBytes {
		return fmt.Errorf("%w: Grok bundle entry exceeds extract limits", ErrMetadataInvalid)
	}
	n, err := io.Copy(io.Discard, io.LimitReader(reader, size+1))
	if err != nil || n != size {
		return fmt.Errorf("%w: Grok bundle entry is incomplete", ErrMetadataInvalid)
	}
	return nil
}

func measureGrokBinary(reader io.Reader, size int64) error {
	entry := &io.LimitedReader{R: reader, N: size}
	n, err := io.Copy(io.Discard, io.LimitReader(brotli.NewReader(entry), MaxGrokBinaryBytes+1))
	leftover := entry.N
	if leftover > 0 {
		if _, drainErr := io.CopyN(io.Discard, reader, leftover); drainErr != nil && err == nil {
			err = drainErr
		}
	}
	if n > MaxGrokBinaryBytes {
		return fmt.Errorf("%w: Grok binary exceeds extract limits", ErrArtifactTooLarge)
	}
	if err != nil || n <= 0 || leftover != 0 {
		return fmt.Errorf("%w: Grok binary is not a compressed executable", ErrMetadataInvalid)
	}
	return nil
}

func ResolveGrokMaterialContext(ctx context.Context, dir, version, requestedSHA256,
	targetOS, targetArch string,
) (GrokMaterial, error) {
	if ctx == nil {
		return GrokMaterial{}, errors.New("artifact: Grok material context is required")
	}
	if err := ctx.Err(); err != nil {
		return GrokMaterial{}, err
	}
	version = strings.TrimSpace(version)
	requestedSHA256 = strings.TrimSpace(requestedSHA256)
	if _, ok := GrokBundleMember(targetOS, targetArch); version == "" || !ok {
		return GrokMaterial{}, errors.New("artifact: Grok version and target are required")
	}
	record, err := selectCatalogArtifact(dir, "grok", version, requestedSHA256)
	if err != nil {
		return GrokMaterial{}, err
	}
	if err := ValidateStoredArtifactContext(ctx, dir, record); err != nil {
		return GrokMaterial{}, err
	}
	if err := ValidateGrokBundleTargetsContext(ctx, dir, record,
		NodeRuntimeTarget{OS: targetOS, Arch: targetArch}); err != nil {
		return GrokMaterial{}, err
	}
	raw, err := marshalCompactNoEscape(model.GrokSpec{
		Kind: "grok", Version: record.Version,
		TargetOS: targetOS, TargetArch: targetArch,
		BundleLayout: model.GrokBundleLayoutV1,
		Artifact: &model.ArtifactRef{
			SHA256: record.SHA256, Size: record.Size,
			URL: "/v1/artifacts/" + record.SHA256,
		},
	})
	if err != nil {
		return GrokMaterial{}, fmt.Errorf("artifact: encode Grok spec: %w", err)
	}
	return GrokMaterial{
		Spec: string(raw), Digest: "sha256:" + record.SHA256,
		Version: record.Version, TargetOS: targetOS, TargetArch: targetArch,
		Artifact: record,
	}, nil
}
