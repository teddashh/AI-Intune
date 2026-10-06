package artifact

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/teddashh/AI-Intune/internal/model"
)

const (
	MaxCodexOuterEntries     = 64
	MaxCodexPackageEntries   = 256
	maxCodexPackageFileBytes = DefaultCodexSourceMaxBytes
)

type CodexMaterial struct {
	Spec       string
	Digest     string
	Version    string
	TargetOS   string
	TargetArch string
	Artifact   Sidecar
}

func ValidateCodexBundleTargetsContext(ctx context.Context, dir string, record Sidecar,
	targets ...NodeRuntimeTarget,
) error {
	if ctx == nil || record.Name != "codex" || !ValidCodexVersion(record.Version) ||
		!ValidSHA256Hex(record.SHA256) || len(targets) == 0 {
		return errors.New("artifact: Codex target validation is incomplete")
	}
	wanted := make(map[string]string, len(targets))
	for _, target := range targets {
		filename, ok := CodexPackageFilename(target.OS, target.Arch)
		if !ok {
			return errors.New("artifact: Codex target is invalid")
		}
		wanted[nodeRuntimeSourceKey(target.OS, target.Arch)] = filename
	}
	input, err := os.Open(filepath.Join(dir, record.SHA256+".tgz"))
	if err != nil {
		return fmt.Errorf("artifact: open Codex bundle: %w", err)
	}
	defer input.Close()
	gz, err := gzip.NewReader(input)
	if err != nil {
		return fmt.Errorf("%w: Codex bundle gzip is invalid", ErrMetadataInvalid)
	}
	defer gz.Close()
	reader := tar.NewReader(gz)
	var sums []byte
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
			return fmt.Errorf("%w: read Codex bundle: %v", ErrMetadataInvalid, err)
		}
		entries++
		if entries > MaxCodexOuterEntries || header.Size < 0 {
			return fmt.Errorf("%w: Codex bundle exceeds extract limits", ErrMetadataInvalid)
		}
		name := path.Clean(strings.TrimSuffix(header.Name, "/"))
		if header.Typeflag == tar.TypeDir {
			continue
		}
		if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA {
			return fmt.Errorf("%w: Codex bundle entry %s is not a regular file", ErrMetadataInvalid, header.Name)
		}
		parts := strings.Split(name, "/")
		switch {
		case name == "codex/"+CodexChecksumAssetName():
			if sums != nil || header.Size <= 0 || header.Size > DefaultCodexChecksumMaxBytes {
				return fmt.Errorf("%w: Codex checksum manifest is invalid", ErrMetadataInvalid)
			}
			sums, err = readExact(reader, header.Size)
			if err != nil {
				return fmt.Errorf("%w: read Codex checksum manifest: %v", ErrMetadataInvalid, err)
			}
		case len(parts) == 3 && parts[0] == "codex":
			filename, ok := wanted[parts[1]]
			if !ok || parts[2] != filename {
				if err := discardExact(reader, header.Size, maxCodexPackageFileBytes); err != nil {
					return err
				}
				continue
			}
			if _, exists := digests[parts[1]]; exists || header.Size <= 0 || header.Size > maxCodexPackageFileBytes {
				return fmt.Errorf("%w: Codex package %s is invalid", ErrMetadataInvalid, filename)
			}
			digest, err := hashAndInspectCodexPackage(ctx, reader, header.Size, targetOSFromKey(parts[1]))
			if err != nil {
				return err
			}
			digests[parts[1]] = digest
		default:
			if err := discardExact(reader, header.Size, maxCodexPackageFileBytes); err != nil {
				return err
			}
		}
	}
	if sums == nil {
		return fmt.Errorf("%w: Codex bundle lacks %s", ErrMetadataInvalid, CodexChecksumAssetName())
	}
	for key, filename := range wanted {
		digest, err := CodexPackageDigest(sums, filename)
		if err != nil {
			return err
		}
		if digests[key] != digest {
			return fmt.Errorf("%w: Codex package %s does not match %s", ErrIntegrityMismatch, filename, CodexChecksumAssetName())
		}
	}
	return nil
}

func targetOSFromKey(key string) string {
	osName, _, _ := strings.Cut(key, "-")
	return osName
}

func hashAndInspectCodexPackage(ctx context.Context, reader io.Reader, size int64, targetOS string) (string, error) {
	hash := sha256.New()
	limited := &io.LimitedReader{R: io.TeeReader(reader, hash), N: size}
	err := inspectCodexPackageGzip(ctx, limited, targetOS)
	if _, drainErr := io.Copy(io.Discard, limited); err == nil && drainErr != nil {
		err = drainErr
	}
	if err != nil {
		return "", err
	}
	if limited.N != 0 {
		return "", fmt.Errorf("%w: Codex package size mismatch", ErrIntegrityMismatch)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func inspectCodexPackageGzip(ctx context.Context, reader io.Reader, targetOS string) error {
	required := CodexPackageRequiredPaths(targetOS)
	if len(required) == 0 {
		return errors.New("artifact: Codex target is invalid")
	}
	want := make(map[string]struct{}, len(required))
	for _, relative := range required {
		want[relative] = struct{}{}
	}
	gz, err := gzip.NewReader(reader)
	if err != nil {
		return fmt.Errorf("%w: Codex package gzip is invalid", ErrMetadataInvalid)
	}
	defer gz.Close()
	inner := tar.NewReader(gz)
	found := map[string]struct{}{}
	entries := 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		header, err := inner.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("%w: read Codex package: %v", ErrMetadataInvalid, err)
		}
		entries++
		if entries > MaxCodexPackageEntries || header.Size < 0 || header.Size > maxCodexPackageFileBytes {
			return fmt.Errorf("%w: Codex package exceeds extract limits", ErrMetadataInvalid)
		}
		relative, err := CleanCodexPackagePath(header.Name)
		if err != nil {
			return err
		}
		if header.Typeflag == tar.TypeDir || relative == "" {
			continue
		}
		if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA {
			return fmt.Errorf("%w: Codex package entry %s is not a regular file", ErrMetadataInvalid, relative)
		}
		if err := discardExact(inner, header.Size, header.Size); err != nil {
			return fmt.Errorf("%w: read Codex package entry %s: %v", ErrMetadataInvalid, relative, err)
		}
		if _, ok := want[relative]; !ok {
			continue
		}
		if header.Size <= 0 {
			return fmt.Errorf("%w: Codex package entry %s is empty", ErrMetadataInvalid, relative)
		}
		if CodexPackagePathExecutable(relative) && header.Mode&0o111 == 0 {
			return fmt.Errorf("%w: Codex package entry %s is not executable", ErrMetadataInvalid, relative)
		}
		found[relative] = struct{}{}
	}
	for _, relative := range required {
		if _, ok := found[relative]; !ok {
			return fmt.Errorf("%w: Codex package lacks %s", ErrMetadataInvalid, relative)
		}
	}
	return nil
}

func readExact(reader io.Reader, size int64) ([]byte, error) {
	body := make([]byte, size)
	if _, err := io.ReadFull(reader, body); err != nil {
		return nil, err
	}
	return body, nil
}

func discardExact(reader io.Reader, size, maximum int64) error {
	if size < 0 || size > maximum {
		return fmt.Errorf("%w: Codex bundle entry exceeds extract limits", ErrMetadataInvalid)
	}
	_, err := io.CopyN(io.Discard, reader, size)
	return err
}

func ResolveCodexMaterialContext(ctx context.Context, dir, version, requestedSHA256,
	targetOS, targetArch string,
) (CodexMaterial, error) {
	if ctx == nil {
		return CodexMaterial{}, errors.New("artifact: Codex material context is required")
	}
	if err := ctx.Err(); err != nil {
		return CodexMaterial{}, err
	}
	version = strings.TrimSpace(version)
	requestedSHA256 = strings.TrimSpace(requestedSHA256)
	if _, ok := CodexPackageFilename(targetOS, targetArch); version == "" || !ok {
		return CodexMaterial{}, errors.New("artifact: Codex version and target are required")
	}
	record, err := selectCatalogArtifact(dir, "codex", version, requestedSHA256)
	if err != nil {
		return CodexMaterial{}, err
	}
	if err := ValidateStoredArtifactContext(ctx, dir, record); err != nil {
		return CodexMaterial{}, err
	}
	if err := ValidateCodexBundleTargetsContext(ctx, dir, record,
		NodeRuntimeTarget{OS: targetOS, Arch: targetArch}); err != nil {
		return CodexMaterial{}, err
	}
	raw, err := marshalCompactNoEscape(model.CodexSpec{
		Kind: "codex", Version: record.Version,
		TargetOS: targetOS, TargetArch: targetArch,
		BundleLayout: model.CodexBundleLayoutV1,
		Artifact: &model.ArtifactRef{
			SHA256: record.SHA256, Size: record.Size,
			URL: "/v1/artifacts/" + record.SHA256,
		},
	})
	if err != nil {
		return CodexMaterial{}, fmt.Errorf("artifact: encode Codex spec: %w", err)
	}
	return CodexMaterial{
		Spec: string(raw), Digest: "sha256:" + record.SHA256,
		Version: record.Version, TargetOS: targetOS, TargetArch: targetArch,
		Artifact: record,
	}, nil
}
