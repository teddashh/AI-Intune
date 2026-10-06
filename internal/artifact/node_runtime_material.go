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
)

// NodeRuntimeTarget identifies one subtree in a multi-platform Node runtime
// artifact.
type NodeRuntimeTarget struct {
	OS   string
	Arch string
}

// ValidateNodeRuntimeBundleTargetsContext checks that already-hashed artifact
// bytes contain executable Node and npm material for every advertised target.
// Callers must first validate the Sidecar and tarball digest with
// ValidateStoredArtifactContext or InspectCatalogEntry.
func ValidateNodeRuntimeBundleTargetsContext(ctx context.Context, dir string, record Sidecar,
	targets ...NodeRuntimeTarget,
) error {
	if ctx == nil || record.Name != "node-runtime" || !ValidNodeRuntimeVersion(record.Version) ||
		!ValidSHA256Hex(record.SHA256) || len(targets) == 0 {
		return errors.New("artifact: Node runtime target validation is incomplete")
	}
	wanted := make(map[string]NodeRuntimeTarget, len(targets))
	validPlatforms := map[string]struct{}{
		"linux-amd64": {}, "linux-arm64": {}, "darwin-amd64": {}, "darwin-arm64": {},
		"windows-amd64": {}, "windows-arm64": {},
	}
	for _, target := range targets {
		if (target.OS != "linux" && target.OS != "darwin" && target.OS != "windows") ||
			(target.Arch != "amd64" && target.Arch != "arm64") {
			return errors.New("artifact: Node runtime target is invalid")
		}
		wanted[nodeRuntimeSourceKey(target.OS, target.Arch)] = target
	}

	input, err := os.Open(filepath.Join(dir, record.SHA256+".tgz"))
	if err != nil {
		return fmt.Errorf("artifact: open Node runtime bundle: %w", err)
	}
	defer input.Close()
	gz, err := gzip.NewReader(input)
	if err != nil {
		return fmt.Errorf("artifact: open Node runtime bundle gzip: %w", err)
	}
	defer gz.Close()

	type material struct{ node, npm bool }
	found := make(map[string]*material, len(wanted))
	for key := range wanted {
		found[key] = &material{}
	}
	seen := make(map[string]struct{})
	reader := tar.NewReader(gz)
	var entries int
	var uncompressed int64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("artifact: read Node runtime bundle: %w", err)
		}
		entries++
		if entries > nodeRuntimeMaxEntries || header.Size < 0 || header.Size > nodeRuntimeMaxFileBytes ||
			uncompressed > nodeRuntimeMaxUncompressedBytes-header.Size {
			return fmt.Errorf("%w: Node runtime bundle exceeds validation policy", ErrArtifactTooLarge)
		}
		uncompressed += header.Size
		name := path.Clean(strings.TrimSuffix(header.Name, "/"))
		if header.Name == "" || path.IsAbs(header.Name) || strings.ContainsAny(header.Name, "\\\x00") ||
			name == "." || name == ".." || strings.HasPrefix(name, "../") ||
			(header.Name != name && header.Name != name+"/") {
			return fmt.Errorf("%w: Node runtime bundle path is invalid", ErrMetadataInvalid)
		}
		if _, duplicate := seen[name]; duplicate {
			return fmt.Errorf("%w: Node runtime bundle path is duplicated", ErrMetadataInvalid)
		}
		seen[name] = struct{}{}
		parts := strings.Split(name, "/")
		if parts[0] != "node-runtime" {
			return fmt.Errorf("%w: Node runtime bundle path is outside its layout", ErrMetadataInvalid)
		}
		if len(parts) == 1 {
			if header.Typeflag != tar.TypeDir {
				return fmt.Errorf("%w: Node runtime bundle root is not a directory", ErrMetadataInvalid)
			}
			continue
		}
		key := parts[1]
		if _, valid := validPlatforms[key]; !valid {
			return fmt.Errorf("%w: Node runtime bundle platform is invalid", ErrMetadataInvalid)
		}
		if len(parts) == 2 {
			if header.Typeflag != tar.TypeDir {
				return fmt.Errorf("%w: Node runtime platform root is not a directory", ErrMetadataInvalid)
			}
			continue
		}
		relative := strings.Join(parts[2:], "/")
		switch header.Typeflag {
		case tar.TypeDir, tar.TypeReg, tar.TypeRegA:
		case tar.TypeSymlink:
			if header.Size != 0 || !validNodeRuntimeSourceLink(relative, header.Linkname) {
				return fmt.Errorf("%w: Node runtime bundle symlink is invalid", ErrMetadataInvalid)
			}
		default:
			return fmt.Errorf("%w: Node runtime bundle entry type is unsupported", ErrMetadataInvalid)
		}
		material, selected := found[key]
		if !selected {
			continue
		}
		switch relative {
		case "bin/node", "bin/node.exe":
			if (header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA) || header.Mode&0o111 == 0 {
				return fmt.Errorf("%w: Node runtime target has no executable node", ErrMetadataInvalid)
			}
			material.node = true
		case "lib/node_modules/npm/bin/npm-cli.js":
			if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA {
				return fmt.Errorf("%w: Node runtime target has no npm CLI", ErrMetadataInvalid)
			}
			material.npm = true
		}
	}
	for key, material := range found {
		if !material.node || !material.npm {
			return fmt.Errorf("%w: Node runtime bundle lacks target %s", ErrMetadataInvalid, key)
		}
	}
	return nil
}
