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

const MaxBATServerArchiveEntries = 256

type BATServerMaterial struct {
	Spec         string
	Digest       string
	Version      string
	TargetOS     string
	TargetArch   string
	BinarySHA256 string
	Artifact     Sidecar
}

func hashBATServerBinary(ctx context.Context, reader io.Reader, targetArch string, maxBytes int64) (string, error) {
	if ctx == nil {
		return "", errors.New("artifact: bat-server hash context is required")
	}
	relative, ok := model.BATServerInstalledBinary(targetArch)
	if !ok || maxBytes <= 0 {
		return "", fmt.Errorf("%w: bat-server target is invalid", ErrInvalidFetchRequest)
	}
	gz, err := gzip.NewReader(reader)
	if err != nil {
		return "", fmt.Errorf("%w: bat-server asset gzip is invalid", ErrMetadataInvalid)
	}
	defer gz.Close()
	inner := tar.NewReader(gz)
	found := ""
	var total int64
	entries := 0
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		header, err := inner.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", fmt.Errorf("%w: read bat-server asset: %v", ErrMetadataInvalid, err)
		}
		entries++
		if entries > MaxBATServerArchiveEntries {
			return "", fmt.Errorf("%w: bat-server asset exceeds extract limits", ErrMetadataInvalid)
		}
		name, err := cleanBATServerArchivePath(header.Name)
		if err != nil {
			return "", err
		}
		if header.Typeflag == tar.TypeDir {
			continue
		}
		if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA {
			return "", fmt.Errorf("%w: bat-server asset entry %s is not a regular file", ErrMetadataInvalid, header.Name)
		}
		if header.Size < 0 || header.Size > maxBytes || total > maxBytes-header.Size {
			return "", fmt.Errorf("%w: bat-server asset entry exceeds extract limits", ErrMetadataInvalid)
		}
		total += header.Size
		if name != relative {
			if err := discardBATServerEntry(inner, header.Size); err != nil {
				return "", err
			}
			continue
		}
		if found != "" {
			return "", fmt.Errorf("%w: bat-server binary is repeated", ErrMetadataInvalid)
		}
		if header.Mode&0o111 == 0 {
			return "", fmt.Errorf("%w: bat-server binary is not executable", ErrMetadataInvalid)
		}
		hash := sha256.New()
		n, err := io.Copy(hash, io.LimitReader(inner, header.Size))
		if err != nil || n != header.Size {
			return "", fmt.Errorf("%w: read bat-server binary: %v", ErrMetadataInvalid, err)
		}
		found = hex.EncodeToString(hash.Sum(nil))
	}
	if found == "" {
		return "", fmt.Errorf("%w: bat-server asset lacks %s", ErrMetadataInvalid, relative)
	}
	return found, nil
}

func cleanBATServerArchivePath(name string) (string, error) {
	if name == "" || len(name) > 512 || strings.ContainsRune(name, 0) || strings.Contains(name, `\`) {
		return "", fmt.Errorf("%w: bat-server asset path is invalid", ErrMetadataInvalid)
	}
	if strings.HasPrefix(name, "/") || (len(name) >= 2 && name[1] == ':') {
		return "", fmt.Errorf("%w: bat-server asset path is invalid", ErrMetadataInvalid)
	}
	cleaned := path.Clean(strings.TrimSuffix(name, "/"))
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") || strings.Contains(cleaned, ":") {
		return "", fmt.Errorf("%w: bat-server asset path is invalid", ErrMetadataInvalid)
	}
	return cleaned, nil
}

func discardBATServerEntry(reader io.Reader, size int64) error {
	n, err := io.Copy(io.Discard, io.LimitReader(reader, size+1))
	if err != nil || n != size {
		return fmt.Errorf("%w: bat-server asset entry is incomplete", ErrMetadataInvalid)
	}
	return nil
}

func validBATServerBinarySHA256(value string) bool {
	if len(value) != hex.EncodedLen(sha256.Size) || value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func ValidateBATServerBundleTargetsContext(ctx context.Context, dir string, record Sidecar,
	targets ...NodeRuntimeTarget,
) error {
	_, err := batServerBundleHashes(ctx, dir, record, targets...)
	return err
}

func batServerBundleHashes(ctx context.Context, dir string, record Sidecar,
	targets ...NodeRuntimeTarget,
) (map[string]string, error) {
	if ctx == nil || record.Name != batServerArtifactName || !ValidBATServerVersion(record.Version) ||
		!ValidSHA256Hex(record.SHA256) || len(targets) == 0 {
		return nil, errors.New("artifact: bat-server target validation is incomplete")
	}
	wanted := make(map[string]string, len(targets))
	for _, target := range targets {
		member, ok := BATServerBundleMember(target.OS, target.Arch)
		if !ok {
			return nil, errors.New("artifact: bat-server target is invalid")
		}
		key := target.OS + "/" + target.Arch
		if _, exists := wanted[key]; exists {
			return nil, errors.New("artifact: bat-server target is invalid")
		}
		wanted[key] = member
	}
	input, err := os.Open(filepath.Join(dir, record.SHA256+".tgz"))
	if err != nil {
		return nil, fmt.Errorf("artifact: open bat-server bundle: %w", err)
	}
	defer input.Close()
	gz, err := gzip.NewReader(input)
	if err != nil {
		return nil, fmt.Errorf("%w: bat-server bundle gzip is invalid", ErrMetadataInvalid)
	}
	defer gz.Close()
	reader := tar.NewReader(gz)
	found := map[string]string{}
	entries := 0
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("%w: read bat-server bundle: %v", ErrMetadataInvalid, err)
		}
		entries++
		if entries > MaxBATServerArchiveEntries || header.Size < 0 {
			return nil, fmt.Errorf("%w: bat-server bundle exceeds extract limits", ErrMetadataInvalid)
		}
		name, err := cleanBATServerBundlePath(header.Name)
		if err != nil {
			return nil, err
		}
		if header.Typeflag == tar.TypeDir {
			continue
		}
		if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA {
			return nil, fmt.Errorf("%w: bat-server bundle entry %s is not a regular file", ErrMetadataInvalid, header.Name)
		}
		key := ""
		for candidate, member := range wanted {
			if member == name {
				key = candidate
				break
			}
		}
		if key == "" {
			if err := discardBATServerEntry(reader, header.Size); err != nil {
				return nil, err
			}
			continue
		}
		if _, exists := found[key]; exists || header.Size <= 0 || header.Size > DefaultBATServerSourceMaxBytes {
			return nil, fmt.Errorf("%w: bat-server bundle entry %s is invalid", ErrMetadataInvalid, name)
		}
		limited := &io.LimitedReader{R: reader, N: header.Size}
		hash, hashErr := hashBATServerBinary(ctx, limited, targetArchFromKey(key), DefaultBATServerSourceMaxBytes)
		if limited.N > 0 {
			if _, drainErr := io.Copy(io.Discard, limited); drainErr != nil && hashErr == nil {
				hashErr = fmt.Errorf("%w: bat-server bundle entry %s is incomplete", ErrMetadataInvalid, name)
			}
		}
		if hashErr != nil {
			return nil, hashErr
		}
		found[key] = hash
	}
	for key, member := range wanted {
		if found[key] == "" {
			return nil, fmt.Errorf("%w: bat-server bundle lacks %s", ErrMetadataInvalid, member)
		}
	}
	return found, nil
}

func targetArchFromKey(key string) string {
	_, arch, _ := strings.Cut(key, "/")
	return arch
}

func cleanBATServerBundlePath(name string) (string, error) {
	if name == "" || len(name) > 256 || strings.HasPrefix(name, "/") || strings.Contains(name, `\`) || strings.ContainsRune(name, 0) {
		return "", fmt.Errorf("%w: bat-server bundle path is invalid", ErrMetadataInvalid)
	}
	cleaned := path.Clean(strings.TrimSuffix(name, "/"))
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") || strings.Contains("/"+cleaned+"/", "/../") {
		return "", fmt.Errorf("%w: bat-server bundle path is invalid", ErrMetadataInvalid)
	}
	if cleaned != "bat-server" && !strings.HasPrefix(cleaned, "bat-server/") {
		return "", fmt.Errorf("%w: bat-server bundle path is invalid", ErrMetadataInvalid)
	}
	return cleaned, nil
}

func ResolveBATServerMaterialContext(ctx context.Context, dir, version, requestedSHA256,
	targetOS, targetArch string,
) (BATServerMaterial, error) {
	if ctx == nil {
		return BATServerMaterial{}, errors.New("artifact: bat-server material context is required")
	}
	if err := ctx.Err(); err != nil {
		return BATServerMaterial{}, err
	}
	version = strings.TrimSpace(version)
	requestedSHA256 = strings.TrimSpace(requestedSHA256)
	if _, ok := BATServerBundleMember(targetOS, targetArch); version == "" || !ok {
		return BATServerMaterial{}, errors.New("artifact: bat-server version and target are required")
	}
	record, err := selectCatalogArtifact(dir, batServerArtifactName, version, requestedSHA256)
	if err != nil {
		return BATServerMaterial{}, err
	}
	if err := ValidateStoredArtifactContext(ctx, dir, record); err != nil {
		return BATServerMaterial{}, err
	}
	hashes, err := batServerBundleHashes(ctx, dir, record, NodeRuntimeTarget{OS: targetOS, Arch: targetArch})
	if err != nil {
		return BATServerMaterial{}, err
	}
	binarySHA256 := hashes[targetOS+"/"+targetArch]
	if !validBATServerBinarySHA256(binarySHA256) {
		return BATServerMaterial{}, fmt.Errorf("%w: bat-server binary hash is invalid", ErrMetadataInvalid)
	}
	raw, err := marshalCompactNoEscape(model.BATServerSpec{
		Kind: "bat-server", Version: record.Version,
		TargetOS: targetOS, TargetArch: targetArch,
		BundleLayout: model.BATServerBundleLayoutV1,
		BinarySHA256: binarySHA256,
		Artifact: &model.ArtifactRef{
			SHA256: record.SHA256, Size: record.Size,
			URL: "/v1/artifacts/" + record.SHA256,
		},
	})
	if err != nil {
		return BATServerMaterial{}, fmt.Errorf("artifact: encode bat-server spec: %w", err)
	}
	return BATServerMaterial{
		Spec: string(raw), Digest: "sha256:" + record.SHA256,
		Version: record.Version, TargetOS: targetOS, TargetArch: targetArch,
		BinarySHA256: binarySHA256, Artifact: record,
	}, nil
}
