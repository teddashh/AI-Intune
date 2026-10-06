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

	"github.com/teddashh/AI-Intune/internal/model"
)

type ClaudeCodeMaterial struct {
	Spec       string
	Digest     string
	Version    string
	TargetOS   string
	TargetArch string
	Artifact   Sidecar
}

func ValidateClaudeCodeBundleTargetsContext(ctx context.Context, dir string, record Sidecar,
	targets ...NodeRuntimeTarget,
) error {
	if ctx == nil || record.Name != "claude-code" || !ValidClaudeCodeVersion(record.Version) ||
		!ValidSHA256Hex(record.SHA256) || len(targets) == 0 {
		return errors.New("artifact: Claude Code target validation is incomplete")
	}
	wanted := make(map[string]struct{}, len(targets))
	for _, target := range targets {
		if (target.OS != "linux" && target.OS != "darwin" && target.OS != "windows") ||
			(target.Arch != "amd64" && target.Arch != "arm64") {
			return errors.New("artifact: Claude Code target is invalid")
		}
		wanted[nodeRuntimeSourceKey(target.OS, target.Arch)] = struct{}{}
	}
	input, err := os.Open(filepath.Join(dir, record.SHA256+".tgz"))
	if err != nil {
		return fmt.Errorf("artifact: open Claude Code bundle: %w", err)
	}
	defer input.Close()
	gz, err := gzip.NewReader(input)
	if err != nil {
		return fmt.Errorf("%w: Claude Code bundle gzip is invalid", ErrMetadataInvalid)
	}
	defer gz.Close()
	reader := tar.NewReader(gz)
	found := map[string]bool{}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("%w: read Claude Code bundle: %v", ErrMetadataInvalid, err)
		}
		name := path.Clean(strings.TrimSuffix(header.Name, "/"))
		parts := strings.Split(name, "/")
		if len(parts) < 4 || parts[0] != "claude-code" || parts[2] != "bin" {
			continue
		}
		key := parts[1]
		if _, ok := wanted[key]; !ok {
			continue
		}
		want := "claude"
		if strings.HasPrefix(key, "windows-") {
			want = "claude.exe"
		}
		if parts[3] != want || (header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA) ||
			header.Mode&0o111 == 0 {
			return fmt.Errorf("%w: Claude Code target has no executable binary", ErrMetadataInvalid)
		}
		found[key] = true
	}
	for key := range wanted {
		if !found[key] {
			return fmt.Errorf("%w: Claude Code bundle lacks target %s", ErrMetadataInvalid, key)
		}
	}
	return nil
}

func ResolveClaudeCodeMaterialContext(ctx context.Context, dir, version, requestedSHA256,
	targetOS, targetArch string,
) (ClaudeCodeMaterial, error) {
	if ctx == nil {
		return ClaudeCodeMaterial{}, errors.New("artifact: Claude Code material context is required")
	}
	if err := ctx.Err(); err != nil {
		return ClaudeCodeMaterial{}, err
	}
	version = strings.TrimSpace(version)
	requestedSHA256 = strings.TrimSpace(requestedSHA256)
	if version == "" || (targetOS != "linux" && targetOS != "darwin" && targetOS != "windows") ||
		(targetArch != "amd64" && targetArch != "arm64") {
		return ClaudeCodeMaterial{}, errors.New("artifact: Claude Code version and target are required")
	}
	record, err := selectCatalogArtifact(dir, "claude-code", version, requestedSHA256)
	if err != nil {
		return ClaudeCodeMaterial{}, err
	}
	if err := ValidateStoredArtifactContext(ctx, dir, record); err != nil {
		return ClaudeCodeMaterial{}, err
	}
	if err := ValidateClaudeCodeBundleTargetsContext(ctx, dir, record,
		NodeRuntimeTarget{OS: targetOS, Arch: targetArch}); err != nil {
		return ClaudeCodeMaterial{}, err
	}
	raw, err := marshalCompactNoEscape(model.ClaudeCodeSpec{
		Kind: "claude-code", Version: record.Version,
		TargetOS: targetOS, TargetArch: targetArch,
		BundleLayout: model.ClaudeCodeBundleLayoutV1,
		Artifact: &model.ArtifactRef{
			SHA256: record.SHA256, Size: record.Size,
			URL: "/v1/artifacts/" + record.SHA256,
		},
	})
	if err != nil {
		return ClaudeCodeMaterial{}, fmt.Errorf("artifact: encode Claude Code spec: %w", err)
	}
	return ClaudeCodeMaterial{
		Spec: string(raw), Digest: "sha256:" + record.SHA256,
		Version: record.Version, TargetOS: targetOS, TargetArch: targetArch,
		Artifact: record,
	}, nil
}
