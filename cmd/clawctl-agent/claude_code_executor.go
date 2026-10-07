package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/teddashh/AI-Intune/internal/agentadapter"
	"github.com/teddashh/AI-Intune/internal/deploy"
	"github.com/teddashh/AI-Intune/internal/model"
)

const maxClaudeCodeArtifactBytes = int64(3 << 30)

type claudeCodeExecutor struct {
	deps       execDeps
	targetOS   string
	targetArch string
}

func defaultClaudeCodeExecutor(hubURL, token string) claudeCodeExecutor {
	return claudeCodeExecutor{
		deps:     defaultOpenClawExecutor(hubURL, token).deps,
		targetOS: runtime.GOOS, targetArch: runtime.GOARCH,
	}
}

func (e claudeCodeExecutor) Run(ctx context.Context, job model.JobResponse) ([]model.JobVerificationRequest, error) {
	d := (openclawExecutor{deps: e.deps}).withDefaults()
	spec, err := e.gate(job)
	if err != nil {
		return nil, err
	}
	root := filepath.Join(d.home, ".local", "share", "clawctl", "claude-code")
	releases := filepath.Join(root, "releases")
	if err := d.requireWritableAncestor(root); err != nil {
		return nil, rejectPrecondition("Claude Code directory is not writable: " + err.Error())
	}
	if err := os.MkdirAll(d.fsPath(releases), 0o700); err != nil {
		return []model.JobVerificationRequest{*nodeRuntimeFailure(d, "stage", "create Claude Code releases", err)}, nil
	}
	release := filepath.Join(releases, spec.Version)
	stageVerification, err := e.ensureRelease(ctx, d, job, spec, release)
	if err != nil {
		return nil, err
	}
	if stageVerification != nil {
		return []model.JobVerificationRequest{*stageVerification}, nil
	}
	previous, currentRelease, err := readNodeRuntimeCurrent(d, root, releases)
	if err != nil {
		return nil, rejectPrecondition("invalid Claude Code current: " + err.Error())
	}
	if samePath(currentRelease, release) {
		return verifyClaudeCodeRelease(ctx, d, release, spec.Version, spec.Artifact.SHA256,
			spec.TargetOS, "claude-code-current"), nil
	}
	if err := setNodeRuntimeCurrent(d, root, release, job.JobID); err != nil {
		return []model.JobVerificationRequest{*nodeRuntimeFailure(d, "activate", "switch Claude Code current", err)}, nil
	}
	verifications := verifyClaudeCodeRelease(ctx, d, release, spec.Version, spec.Artifact.SHA256,
		spec.TargetOS, "claude-code-activate")
	if allPassed(verifications) {
		return verifications, nil
	}
	rollback := rollbackNodeRuntimeCurrent(d, root, previous, job.JobID)
	rollback.RuleID = "claude-code-rollback"
	rollback.Command = "restore Claude Code current"
	return append(verifications, rollback), nil
}

func (e claudeCodeExecutor) gate(job model.JobResponse) (model.ClaudeCodeSpec, error) {
	var spec model.ClaudeCodeSpec
	decoder := json.NewDecoder(strings.NewReader(string(job.Spec)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&spec); err != nil {
		return spec, rejectPrecondition("Claude Code spec is not valid JSON: " + err.Error())
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return spec, rejectPrecondition("Claude Code spec contains trailing data")
	}
	if job.ResourceKind != agentadapter.ExecutorKindClaudeCode || spec.Kind != agentadapter.ExecutorKindClaudeCode {
		return spec, rejectPrecondition("job and spec kind must be claude-code")
	}
	if job.ResourceID != "claude-code" || !safePathComponent(spec.Version) || !validNodeRuntimeExactVersion(spec.Version) {
		return spec, rejectPrecondition("invalid Claude Code identity")
	}
	if spec.TargetOS != e.targetOS || spec.TargetArch != e.targetArch ||
		(spec.TargetOS != "linux" && spec.TargetOS != "darwin" && spec.TargetOS != "windows") ||
		(spec.TargetArch != "amd64" && spec.TargetArch != "arm64") {
		return spec, rejectPrecondition("Claude Code target does not match agent platform")
	}
	if spec.BundleLayout != model.ClaudeCodeBundleLayoutV1 || spec.Artifact == nil {
		return spec, rejectPrecondition("invalid Claude Code bundle contract")
	}
	artifact := spec.Artifact
	if len(artifact.SHA256) != sha256.Size*2 {
		return spec, rejectPrecondition("artifact.sha256 is not 64 hex digits")
	}
	if _, err := hex.DecodeString(artifact.SHA256); err != nil || strings.ToLower(artifact.SHA256) != artifact.SHA256 {
		return spec, rejectPrecondition("artifact.sha256 must be lowercase hex")
	}
	if artifact.Size <= 0 || artifact.Size > maxClaudeCodeArtifactBytes {
		return spec, rejectPrecondition("artifact.size exceeds Claude Code bundle limit")
	}
	if artifact.URL != "/v1/artifacts/"+artifact.SHA256 ||
		artifact.EnginesNode != "" || artifact.UpstreamTarball != "" || artifact.SHA512 != "" {
		return spec, rejectPrecondition("invalid Claude Code artifact contract")
	}
	if job.ArtifactDigest != "sha256:"+artifact.SHA256 {
		return spec, &rejectError{Code: deploy.ArtifactHashMismatch, Detail: "job artifact digest does not match spec"}
	}
	return spec, nil
}

func (e claudeCodeExecutor) ensureRelease(ctx context.Context, d execDeps, job model.JobResponse,
	spec model.ClaudeCodeSpec, release string,
) (*model.JobVerificationRequest, error) {
	if info, err := os.Lstat(d.fsPath(release)); err == nil {
		if info.IsDir() && allPassed(verifyClaudeCodeRelease(ctx, d, release, spec.Version,
			spec.Artifact.SHA256, spec.TargetOS, "claude-code-stage")) {
			return nil, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		broken := release + ".broken-" + safeJobID(job.JobID)
		if _, err := os.Lstat(d.fsPath(broken)); err == nil {
			return nodeRuntimeFailure(d, "stage", "preserve existing Claude Code release", errors.New("broken evidence path already exists")), nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return nodeRuntimeFailure(d, "stage", "check Claude Code broken evidence", err), nil
		}
		if err := os.Rename(d.fsPath(release), d.fsPath(broken)); err != nil {
			return nodeRuntimeFailure(d, "stage", "preserve existing Claude Code release", err), nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nodeRuntimeFailure(d, "stage", "check Claude Code release", err), nil
	}
	staging := filepath.Join(filepath.Dir(release), ".staging-"+safeJobID(job.JobID))
	if err := os.RemoveAll(d.fsPath(staging)); err != nil {
		return nodeRuntimeFailure(d, "stage", "clean Claude Code staging", err), nil
	}
	defer os.RemoveAll(d.fsPath(staging))
	if err := os.MkdirAll(d.fsPath(staging), 0o700); err != nil {
		return nodeRuntimeFailure(d, "stage", "create Claude Code staging", err), nil
	}
	bundle := filepath.Join(staging, "claude-code.tgz")
	actualDigest, actualSize, err := d.downloadArtifactAtMost(ctx, d.hubURL+spec.Artifact.URL, bundle, spec.Artifact.Size)
	if err != nil {
		var sizeErr *artifactDownloadSizeError
		if errors.As(err, &sizeErr) {
			return nil, &rejectError{Code: deploy.ArtifactHashMismatch,
				Detail: fmt.Sprintf("Claude Code artifact exceeds declared size=%d", spec.Artifact.Size)}
		}
		return nodeRuntimeFailure(d, "stage", "download Claude Code bundle", err), nil
	}
	if actualDigest != spec.Artifact.SHA256 || actualSize != spec.Artifact.Size {
		return nil, &rejectError{Code: deploy.ArtifactHashMismatch,
			Detail: fmt.Sprintf("Claude Code artifact want sha256=%s size=%d, got sha256=%s size=%d",
				shortDigest(spec.Artifact.SHA256), spec.Artifact.Size, shortDigest(actualDigest), actualSize)}
	}
	payload := filepath.Join(staging, "payload")
	if err := extractClaudeCodeBundle(d.fsPath(bundle), d.fsPath(payload), spec.TargetOS, spec.TargetArch); err != nil {
		return nodeRuntimeFailure(d, "stage", "extract Claude Code bundle", err), nil
	}
	marker := filepath.Join(payload, nodeRuntimeArtifactMarker)
	if err := writePrivateFile(d.fsPath(marker), []byte("sha256:"+spec.Artifact.SHA256+"\n")); err != nil {
		return nodeRuntimeFailure(d, "stage", "record Claude Code artifact identity", err), nil
	}
	checks := verifyClaudeCodeRelease(ctx, d, payload, spec.Version, spec.Artifact.SHA256, spec.TargetOS, "claude-code-stage")
	if !allPassed(checks) {
		failure := checks[0]
		for _, check := range checks {
			if !check.Passed {
				failure = check
				break
			}
		}
		return &failure, nil
	}
	if err := os.Rename(d.fsPath(payload), d.fsPath(release)); err != nil {
		return nodeRuntimeFailure(d, "stage", "publish Claude Code release", err), nil
	}
	if err := syncNodeRuntimeDirectory(d.fsPath(filepath.Dir(release))); err != nil {
		return nodeRuntimeFailure(d, "stage", "sync Claude Code releases", err), nil
	}
	return nil, nil
}

func extractClaudeCodeBundle(bundle, destination, targetOS, targetArch string) error {
	input, err := os.Open(bundle)
	if err != nil {
		return err
	}
	defer input.Close()
	gz, err := gzip.NewReader(input)
	if err != nil {
		return err
	}
	defer gz.Close()
	if err := os.Mkdir(destination, 0o700); err != nil {
		return err
	}
	targetPlatform := targetOS + "-" + targetArch
	wantBinary := "bin/claude"
	if targetOS == "windows" {
		wantBinary = "bin/claude.exe"
	}
	reader := tar.NewReader(gz)
	found := false
	var entries int
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		entries++
		if entries > 64 || header.Size < 0 || header.Size > maxClaudeCodeArtifactBytes {
			return errors.New("Claude Code bundle exceeds extraction limit")
		}
		name := path.Clean(strings.TrimSuffix(header.Name, "/"))
		parts := strings.Split(name, "/")
		if len(parts) == 0 || parts[0] != "claude-code" {
			return fmt.Errorf("Claude Code bundle path outside layout: %s", header.Name)
		}
		if len(parts) == 1 {
			continue
		}
		if parts[1] != targetPlatform {
			continue
		}
		relative := strings.Join(parts[2:], "/")
		if relative == "" || strings.HasSuffix(relative, "/") || header.Typeflag == tar.TypeDir {
			continue
		}
		if relative != wantBinary || (header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA) {
			return fmt.Errorf("Claude Code bundle contains non-target file: %s", relative)
		}
		dest := filepath.Join(destination, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return err
		}
		file, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o755)
		if err != nil {
			return err
		}
		written, copyErr := io.CopyN(file, reader, header.Size)
		syncErr := file.Sync()
		closeErr := file.Close()
		if copyErr != nil || written != header.Size || syncErr != nil || closeErr != nil {
			return fmt.Errorf("incomplete Claude Code bundle file: %s", relative)
		}
		found = true
	}
	if !found {
		return errors.New("Claude Code bundle has no target platform binary")
	}
	return nil
}

func verifyClaudeCodeRelease(ctx context.Context, d execDeps, release, version, artifactSHA256, targetOS,
	rulePrefix string,
) []model.JobVerificationRequest {
	markerLogical := filepath.Join(release, nodeRuntimeArtifactMarker)
	marker, err := readPrivateRegularFile(d.fsPath(markerLogical))
	markerPassed := err == nil && string(marker) == "sha256:"+artifactSHA256+"\n"
	if err == nil && !markerPassed {
		err = errors.New("artifact identity marker does not match job digest")
	}
	results := []model.JobVerificationRequest{d.verification(rulePrefix+"-artifact", "cat "+markerLogical,
		string(marker), errorText("", err), exitCode(err), markerPassed)}
	if !markerPassed {
		return results
	}
	binaryLogical := filepath.Join(release, claudeCodeBinaryRelative(targetOS))
	binaryPath := d.fsPath(binaryLogical)
	if info, err := os.Lstat(binaryPath); err != nil || !info.Mode().IsRegular() ||
		(runtime.GOOS != "windows" && info.Mode().Perm()&0o111 == 0) {
		if err == nil {
			err = errors.New(claudeCodeBinaryRelative(targetOS) + " is not an executable regular file")
		}
		return append(results, *nodeRuntimeFailure(d, rulePrefix+"-version", binaryLogical+" --version", err))
	}
	stdout, stderr, err := d.run(ctx, binaryPath, "--version")
	passed := err == nil && claudeCodeVersionMatches(stdout, version)
	if err == nil && !passed {
		err = fmt.Errorf("Claude Code version=%q; want %q", strings.TrimSpace(stdout), version)
	}
	results = append(results, d.verification(rulePrefix+"-version", binaryLogical+" --version",
		stdout, errorText(stderr, err), exitCode(err), passed))
	return results
}

func claudeCodeBinaryRelative(targetOS string) string {
	if targetOS == "windows" {
		return filepath.Join("bin", "claude.exe")
	}
	return filepath.Join("bin", "claude")
}

func claudeCodeVersionMatches(stdout, version string) bool {
	fields := strings.Fields(strings.TrimSpace(stdout))
	return len(fields) > 0 && fields[0] == version
}
