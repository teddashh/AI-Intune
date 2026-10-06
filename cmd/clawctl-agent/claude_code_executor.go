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
		return nil, rejectPrecondition("Claude Code 目錄不可寫：" + err.Error())
	}
	if err := os.MkdirAll(d.fsPath(releases), 0o700); err != nil {
		return []model.JobVerificationRequest{*nodeRuntimeFailure(d, "stage", "建立 Claude Code releases", err)}, nil
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
		return nil, rejectPrecondition("Claude Code current 不合法：" + err.Error())
	}
	if samePath(currentRelease, release) {
		return verifyClaudeCodeRelease(ctx, d, release, spec.Version, spec.Artifact.SHA256,
			spec.TargetOS, "claude-code-current"), nil
	}
	if err := setNodeRuntimeCurrent(d, root, release, job.JobID); err != nil {
		return []model.JobVerificationRequest{*nodeRuntimeFailure(d, "activate", "切換 Claude Code current", err)}, nil
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
		return spec, rejectPrecondition("Claude Code spec 不是合法 JSON：" + err.Error())
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return spec, rejectPrecondition("Claude Code spec 含有尾隨資料")
	}
	if job.ResourceKind != agentadapter.ExecutorKindClaudeCode || spec.Kind != agentadapter.ExecutorKindClaudeCode {
		return spec, rejectPrecondition("工作單與 spec kind 必須是 claude-code")
	}
	if job.ResourceID != "claude-code" || !safePathComponent(spec.Version) || !validNodeRuntimeExactVersion(spec.Version) {
		return spec, rejectPrecondition("Claude Code identity 不合法")
	}
	if spec.TargetOS != e.targetOS || spec.TargetArch != e.targetArch ||
		(spec.TargetOS != "linux" && spec.TargetOS != "darwin" && spec.TargetOS != "windows") ||
		(spec.TargetArch != "amd64" && spec.TargetArch != "arm64") {
		return spec, rejectPrecondition("Claude Code target 與 agent 平台不一致")
	}
	if spec.BundleLayout != model.ClaudeCodeBundleLayoutV1 || spec.Artifact == nil {
		return spec, rejectPrecondition("Claude Code bundle contract 不合法")
	}
	artifact := spec.Artifact
	if len(artifact.SHA256) != sha256.Size*2 {
		return spec, rejectPrecondition("artifact.sha256 不是 64 碼十六進位")
	}
	if _, err := hex.DecodeString(artifact.SHA256); err != nil || strings.ToLower(artifact.SHA256) != artifact.SHA256 {
		return spec, rejectPrecondition("artifact.sha256 必須是小寫十六進位")
	}
	if artifact.Size <= 0 || artifact.Size > maxClaudeCodeArtifactBytes {
		return spec, rejectPrecondition("artifact.size 超出 Claude Code bundle 上限")
	}
	if artifact.URL != "/v1/artifacts/"+artifact.SHA256 ||
		artifact.EnginesNode != "" || artifact.UpstreamTarball != "" || artifact.SHA512 != "" {
		return spec, rejectPrecondition("Claude Code artifact contract 不合法")
	}
	if job.ArtifactDigest != "sha256:"+artifact.SHA256 {
		return spec, &rejectError{Code: deploy.ArtifactHashMismatch, Detail: "工作單 artifact digest 與 spec 不一致"}
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
			return nodeRuntimeFailure(d, "stage", "保留既有 Claude Code release", errors.New("broken 證據路徑已存在")), nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return nodeRuntimeFailure(d, "stage", "檢查 Claude Code broken 證據", err), nil
		}
		if err := os.Rename(d.fsPath(release), d.fsPath(broken)); err != nil {
			return nodeRuntimeFailure(d, "stage", "保留既有 Claude Code release", err), nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nodeRuntimeFailure(d, "stage", "檢查 Claude Code release", err), nil
	}
	staging := filepath.Join(filepath.Dir(release), ".staging-"+safeJobID(job.JobID))
	if err := os.RemoveAll(d.fsPath(staging)); err != nil {
		return nodeRuntimeFailure(d, "stage", "清理 Claude Code staging", err), nil
	}
	defer os.RemoveAll(d.fsPath(staging))
	if err := os.MkdirAll(d.fsPath(staging), 0o700); err != nil {
		return nodeRuntimeFailure(d, "stage", "建立 Claude Code staging", err), nil
	}
	bundle := filepath.Join(staging, "claude-code.tgz")
	actualDigest, actualSize, err := d.downloadArtifactAtMost(ctx, d.hubURL+spec.Artifact.URL, bundle, spec.Artifact.Size)
	if err != nil {
		var sizeErr *artifactDownloadSizeError
		if errors.As(err, &sizeErr) {
			return nil, &rejectError{Code: deploy.ArtifactHashMismatch,
				Detail: fmt.Sprintf("Claude Code artifact 超過宣告 size=%d", spec.Artifact.Size)}
		}
		return nodeRuntimeFailure(d, "stage", "下載 Claude Code bundle", err), nil
	}
	if actualDigest != spec.Artifact.SHA256 || actualSize != spec.Artifact.Size {
		return nil, &rejectError{Code: deploy.ArtifactHashMismatch,
			Detail: fmt.Sprintf("Claude Code artifact 期望 sha256=%s size=%d，實得 sha256=%s size=%d",
				shortDigest(spec.Artifact.SHA256), spec.Artifact.Size, shortDigest(actualDigest), actualSize)}
	}
	payload := filepath.Join(staging, "payload")
	if err := extractClaudeCodeBundle(d.fsPath(bundle), d.fsPath(payload), spec.TargetOS, spec.TargetArch); err != nil {
		return nodeRuntimeFailure(d, "stage", "展開 Claude Code bundle", err), nil
	}
	marker := filepath.Join(payload, nodeRuntimeArtifactMarker)
	if err := writePrivateFile(d.fsPath(marker), []byte("sha256:"+spec.Artifact.SHA256+"\n")); err != nil {
		return nodeRuntimeFailure(d, "stage", "記錄 Claude Code artifact identity", err), nil
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
		return nodeRuntimeFailure(d, "stage", "發佈 Claude Code release", err), nil
	}
	if err := syncNodeRuntimeDirectory(d.fsPath(filepath.Dir(release))); err != nil {
		return nodeRuntimeFailure(d, "stage", "同步 Claude Code releases", err), nil
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
			return errors.New("Claude Code bundle 超出展開上限")
		}
		name := path.Clean(strings.TrimSuffix(header.Name, "/"))
		parts := strings.Split(name, "/")
		if len(parts) == 0 || parts[0] != "claude-code" {
			return fmt.Errorf("Claude Code bundle 路徑超出 layout：%s", header.Name)
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
			return fmt.Errorf("Claude Code bundle 含非目標檔案：%s", relative)
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
			return fmt.Errorf("Claude Code bundle 檔案不完整：%s", relative)
		}
		found = true
	}
	if !found {
		return errors.New("Claude Code bundle 沒有目標平台 binary")
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
		err = errors.New("artifact identity marker 與工作單 digest 不符")
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
			err = errors.New(claudeCodeBinaryRelative(targetOS) + " 不是可執行 regular file")
		}
		return append(results, *nodeRuntimeFailure(d, rulePrefix+"-version", binaryLogical+" --version", err))
	}
	stdout, stderr, err := d.run(ctx, binaryPath, "--version")
	passed := err == nil && claudeCodeVersionMatches(stdout, version)
	if err == nil && !passed {
		err = fmt.Errorf("Claude Code version=%q；要 %q", strings.TrimSpace(stdout), version)
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
