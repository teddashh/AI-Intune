package main

import (
	"archive/tar"
	"bytes"
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

const (
	codexChecksumAssetName       = "codex-package_SHA256SUMS"
	maxCodexOuterEntries         = 64
	maxCodexPackageEntries       = 256
	maxCodexPackageFileBytes     = int64(512 << 20)
	maxCodexChecksumManifestSize = int64(1 << 20)
)

const maxCodexArtifactBytes = int64(3 << 30)

type codexExecutor struct {
	deps       execDeps
	targetOS   string
	targetArch string
}

func defaultCodexExecutor(hubURL, token string) codexExecutor {
	return codexExecutor{
		deps:     defaultOpenClawExecutor(hubURL, token).deps,
		targetOS: runtime.GOOS, targetArch: runtime.GOARCH,
	}
}

func (e codexExecutor) Run(ctx context.Context, job model.JobResponse) ([]model.JobVerificationRequest, error) {
	d := (openclawExecutor{deps: e.deps}).withDefaults()
	spec, err := e.gate(job)
	if err != nil {
		return nil, err
	}
	root := filepath.Join(d.home, ".local", "share", "clawctl", "codex")
	releases := filepath.Join(root, "releases")
	if err := d.requireWritableAncestor(root); err != nil {
		return nil, rejectPrecondition("Codex 目錄不可寫：" + err.Error())
	}
	if err := os.MkdirAll(d.fsPath(releases), 0o700); err != nil {
		return []model.JobVerificationRequest{*nodeRuntimeFailure(d, "stage", "建立 Codex releases", err)}, nil
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
		return nil, rejectPrecondition("Codex current 不合法：" + err.Error())
	}
	if samePath(currentRelease, release) {
		return verifyCodexRelease(ctx, d, release, spec.Version, spec.Artifact.SHA256,
			spec.TargetOS, "codex-current"), nil
	}
	if err := setNodeRuntimeCurrent(d, root, release, job.JobID); err != nil {
		return []model.JobVerificationRequest{*nodeRuntimeFailure(d, "activate", "切換 Codex current", err)}, nil
	}
	verifications := verifyCodexRelease(ctx, d, release, spec.Version, spec.Artifact.SHA256,
		spec.TargetOS, "codex-activate")
	if allPassed(verifications) {
		return verifications, nil
	}
	rollback := rollbackNodeRuntimeCurrent(d, root, previous, job.JobID)
	rollback.RuleID = "codex-rollback"
	rollback.Command = "restore Codex current"
	return append(verifications, rollback), nil
}

func (e codexExecutor) gate(job model.JobResponse) (model.CodexSpec, error) {
	var spec model.CodexSpec
	decoder := json.NewDecoder(strings.NewReader(string(job.Spec)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&spec); err != nil {
		return spec, rejectPrecondition("Codex spec 不是合法 JSON：" + err.Error())
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return spec, rejectPrecondition("Codex spec 含有尾隨資料")
	}
	if job.ResourceKind != agentadapter.ExecutorKindCodex || spec.Kind != agentadapter.ExecutorKindCodex {
		return spec, rejectPrecondition("工作單與 spec kind 必須是 codex")
	}
	if job.ResourceID != "codex" || !safePathComponent(spec.Version) || !validNodeRuntimeExactVersion(spec.Version) {
		return spec, rejectPrecondition("Codex identity 不合法")
	}
	if _, ok := codexPackageFilename(spec.TargetOS, spec.TargetArch); !ok ||
		spec.TargetOS != e.targetOS || spec.TargetArch != e.targetArch {
		return spec, rejectPrecondition("Codex target 與 agent 平台不一致")
	}
	if spec.BundleLayout != model.CodexBundleLayoutV1 || spec.Artifact == nil {
		return spec, rejectPrecondition("Codex bundle contract 不合法")
	}
	pinned := spec.Artifact
	if len(pinned.SHA256) != sha256.Size*2 {
		return spec, rejectPrecondition("artifact.sha256 不是 64 碼十六進位")
	}
	if _, err := hex.DecodeString(pinned.SHA256); err != nil || strings.ToLower(pinned.SHA256) != pinned.SHA256 {
		return spec, rejectPrecondition("artifact.sha256 必須是小寫十六進位")
	}
	if pinned.Size <= 0 || pinned.Size > maxCodexArtifactBytes {
		return spec, rejectPrecondition("artifact.size 超出 Codex bundle 上限")
	}
	if pinned.URL != "/v1/artifacts/"+pinned.SHA256 ||
		pinned.EnginesNode != "" || pinned.UpstreamTarball != "" || pinned.SHA512 != "" {
		return spec, rejectPrecondition("Codex artifact contract 不合法")
	}
	if job.ArtifactDigest != "sha256:"+pinned.SHA256 {
		return spec, &rejectError{Code: deploy.ArtifactHashMismatch, Detail: "工作單 artifact digest 與 spec 不一致"}
	}
	return spec, nil
}

func (e codexExecutor) ensureRelease(ctx context.Context, d execDeps, job model.JobResponse,
	spec model.CodexSpec, release string,
) (*model.JobVerificationRequest, error) {
	if info, err := os.Lstat(d.fsPath(release)); err == nil {
		if info.IsDir() && allPassed(verifyCodexRelease(ctx, d, release, spec.Version,
			spec.Artifact.SHA256, spec.TargetOS, "codex-stage")) {
			return nil, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		broken := release + ".broken-" + safeJobID(job.JobID)
		if _, err := os.Lstat(d.fsPath(broken)); err == nil {
			return nodeRuntimeFailure(d, "stage", "保留既有 Codex release", errors.New("broken 證據路徑已存在")), nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return nodeRuntimeFailure(d, "stage", "檢查 Codex broken 證據", err), nil
		}
		if err := os.Rename(d.fsPath(release), d.fsPath(broken)); err != nil {
			return nodeRuntimeFailure(d, "stage", "保留既有 Codex release", err), nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nodeRuntimeFailure(d, "stage", "檢查 Codex release", err), nil
	}
	staging := filepath.Join(filepath.Dir(release), ".staging-"+safeJobID(job.JobID))
	if err := os.RemoveAll(d.fsPath(staging)); err != nil {
		return nodeRuntimeFailure(d, "stage", "清理 Codex staging", err), nil
	}
	defer os.RemoveAll(d.fsPath(staging))
	if err := os.MkdirAll(d.fsPath(staging), 0o700); err != nil {
		return nodeRuntimeFailure(d, "stage", "建立 Codex staging", err), nil
	}
	bundle := filepath.Join(staging, "codex.tgz")
	actualDigest, actualSize, err := d.downloadArtifactAtMost(ctx, d.hubURL+spec.Artifact.URL, bundle, spec.Artifact.Size)
	if err != nil {
		var sizeErr *artifactDownloadSizeError
		if errors.As(err, &sizeErr) {
			return nil, &rejectError{Code: deploy.ArtifactHashMismatch,
				Detail: fmt.Sprintf("Codex artifact 超過宣告 size=%d", spec.Artifact.Size)}
		}
		return nodeRuntimeFailure(d, "stage", "下載 Codex bundle", err), nil
	}
	if actualDigest != spec.Artifact.SHA256 || actualSize != spec.Artifact.Size {
		return nil, &rejectError{Code: deploy.ArtifactHashMismatch,
			Detail: fmt.Sprintf("Codex artifact 期望 sha256=%s size=%d，實得 sha256=%s size=%d",
				shortDigest(spec.Artifact.SHA256), spec.Artifact.Size, shortDigest(actualDigest), actualSize)}
	}
	payload := filepath.Join(staging, "payload")
	if err := extractCodexBundle(d.fsPath(bundle), d.fsPath(payload), spec.TargetOS, spec.TargetArch); err != nil {
		return nodeRuntimeFailure(d, "stage", "展開 Codex bundle", err), nil
	}
	marker := filepath.Join(payload, nodeRuntimeArtifactMarker)
	if err := writePrivateFile(d.fsPath(marker), []byte("sha256:"+spec.Artifact.SHA256+"\n")); err != nil {
		return nodeRuntimeFailure(d, "stage", "記錄 Codex artifact identity", err), nil
	}
	checks := verifyCodexRelease(ctx, d, payload, spec.Version, spec.Artifact.SHA256, spec.TargetOS, "codex-stage")
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
		return nodeRuntimeFailure(d, "stage", "發佈 Codex release", err), nil
	}
	if err := syncNodeRuntimeDirectory(d.fsPath(filepath.Dir(release))); err != nil {
		return nodeRuntimeFailure(d, "stage", "同步 Codex releases", err), nil
	}
	return nil, nil
}

func extractCodexBundle(bundle, destination, targetOS, targetArch string) error {
	filename, ok := codexPackageFilename(targetOS, targetArch)
	if !ok {
		return errors.New("Codex target 不合法")
	}
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
	reader := tar.NewReader(gz)
	want := "codex/" + targetOS + "-" + targetArch + "/" + filename
	var sums []byte
	var packagePath, packageHash string
	defer func() {
		if packagePath != "" {
			_ = os.Remove(packagePath)
		}
	}()
	entries := 0
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		entries++
		if entries > maxCodexOuterEntries || header.Size < 0 || header.Size > maxCodexArtifactBytes {
			return errors.New("Codex bundle 超出展開上限")
		}
		name := path.Clean(strings.TrimSuffix(header.Name, "/"))
		if header.Typeflag == tar.TypeDir {
			continue
		}
		if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA {
			return fmt.Errorf("Codex bundle 路徑不是 regular file：%s", header.Name)
		}
		switch name {
		case "codex/" + codexChecksumAssetName:
			if sums != nil || header.Size <= 0 || header.Size > maxCodexChecksumManifestSize {
				return errors.New("Codex checksum manifest 不合法")
			}
			sums, err = io.ReadAll(io.LimitReader(reader, header.Size+1))
			if err != nil || int64(len(sums)) != header.Size {
				return errors.New("Codex checksum manifest 不完整")
			}
		case want:
			if packagePath != "" {
				return errors.New("Codex bundle 重複目標 package")
			}
			packagePath, packageHash, err = writeHashedCodexTemp(filepath.Dir(bundle), reader, header.Size)
			if err != nil {
				return err
			}
		default:
			if _, err := io.CopyN(io.Discard, reader, header.Size); err != nil {
				return err
			}
		}
	}
	if sums == nil || packagePath == "" {
		return errors.New("Codex bundle 沒有目標平台 package")
	}
	digest, err := codexPackageDigest(sums, filename)
	if err != nil || digest != packageHash {
		return errors.New("Codex package 與 checksum manifest 不一致")
	}
	return extractCodexPackage(packagePath, destination, targetOS)
}

func writeHashedCodexTemp(dir string, reader io.Reader, size int64) (string, string, error) {
	temp, err := os.CreateTemp(dir, ".codex-package-*.tmp")
	if err != nil {
		return "", "", err
	}
	pathName := temp.Name()
	keep := false
	defer func() {
		_ = temp.Close()
		if !keep {
			_ = os.Remove(pathName)
		}
	}()
	if err := temp.Chmod(0o600); err != nil {
		return "", "", err
	}
	hash := sha256.New()
	written, err := io.Copy(io.MultiWriter(temp, hash), io.LimitReader(reader, size+1))
	if err != nil || written != size {
		return "", "", errors.New("Codex package 不完整")
	}
	if err := temp.Sync(); err != nil {
		return "", "", err
	}
	keep = true
	return pathName, hex.EncodeToString(hash.Sum(nil)), nil
}

func extractCodexPackage(packagePath, destination, targetOS string) error {
	input, err := os.Open(packagePath)
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
	required := map[string]struct{}{}
	for _, relative := range codexPackageRequiredPaths(targetOS) {
		required[relative] = struct{}{}
	}
	reader := tar.NewReader(gz)
	found := map[string]struct{}{}
	entries := 0
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		entries++
		if entries > maxCodexPackageEntries || header.Size < 0 || header.Size > maxCodexPackageFileBytes {
			return errors.New("Codex package 超出展開上限")
		}
		relative, err := cleanCodexPackagePath(header.Name)
		if err != nil {
			return err
		}
		if header.Typeflag == tar.TypeDir || relative == "" {
			continue
		}
		if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA {
			return fmt.Errorf("Codex package 含非 regular file：%s", relative)
		}
		if _, ok := found[relative]; ok {
			return fmt.Errorf("Codex package 重複路徑：%s", relative)
		}
		_, needed := required[relative]
		if needed && header.Size <= 0 {
			return fmt.Errorf("Codex package 檔案是空的：%s", relative)
		}
		if needed && codexPackagePathExecutable(relative) && header.Mode&0o111 == 0 {
			return fmt.Errorf("Codex package 檔案不可執行：%s", relative)
		}
		dest := filepath.Join(destination, filepath.FromSlash(relative))
		if !strings.HasPrefix(dest, destination+string(os.PathSeparator)) {
			return fmt.Errorf("Codex package 路徑超出 release：%s", relative)
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return err
		}
		mode := os.FileMode(0o644)
		if header.Mode&0o111 != 0 {
			mode = 0o755
		}
		file, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
		if err != nil {
			return err
		}
		written, copyErr := io.CopyN(file, reader, header.Size)
		syncErr := file.Sync()
		closeErr := file.Close()
		if copyErr != nil || written != header.Size || syncErr != nil || closeErr != nil {
			return fmt.Errorf("Codex package 檔案不完整：%s", relative)
		}
		found[relative] = struct{}{}
	}
	for relative := range required {
		if _, ok := found[relative]; !ok {
			return fmt.Errorf("Codex package 缺少 %s", relative)
		}
	}
	return nil
}

func verifyCodexRelease(ctx context.Context, d execDeps, release, version, artifactSHA256, targetOS,
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
	binaryLogical := filepath.Join(release, filepath.FromSlash(codexCommandRelative(targetOS)))
	binaryPath := d.fsPath(binaryLogical)
	if info, err := os.Lstat(binaryPath); err != nil || !info.Mode().IsRegular() ||
		(runtime.GOOS != "windows" && info.Mode().Perm()&0o111 == 0) {
		if err == nil {
			err = errors.New(codexCommandRelative(targetOS) + " 不是可執行 regular file")
		}
		return append(results, *nodeRuntimeFailure(d, rulePrefix+"-version", binaryLogical+" --version", err))
	}
	stdout, stderr, err := d.run(ctx, binaryPath, "--version")
	passed := err == nil && codexVersionMatches(stdout, version)
	if err == nil && !passed {
		err = fmt.Errorf("Codex version=%q；要 %q", strings.TrimSpace(stdout), version)
	}
	results = append(results, d.verification(rulePrefix+"-version", binaryLogical+" --version",
		stdout, errorText(stderr, err), exitCode(err), passed))
	return results
}

func codexPackageFilename(targetOS, targetArch string) (string, bool) {
	switch targetOS + "/" + targetArch {
	case "linux/amd64":
		return "codex-package-x86_64-unknown-linux-musl.tar.gz", true
	case "linux/arm64":
		return "codex-package-aarch64-unknown-linux-musl.tar.gz", true
	case "darwin/amd64":
		return "codex-package-x86_64-apple-darwin.tar.gz", true
	case "darwin/arm64":
		return "codex-package-aarch64-apple-darwin.tar.gz", true
	case "windows/amd64":
		return "codex-package-x86_64-pc-windows-msvc.tar.gz", true
	case "windows/arm64":
		return "codex-package-aarch64-pc-windows-msvc.tar.gz", true
	default:
		return "", false
	}
}

func codexCommandRelative(targetOS string) string {
	if targetOS == "windows" {
		return "bin/codex.exe"
	}
	return "bin/codex"
}

func codexPackageRequiredPaths(targetOS string) []string {
	switch targetOS {
	case "windows":
		return []string{
			"codex-package.json",
			"bin/codex.exe",
			"bin/codex-code-mode-host.exe",
			"codex-path/rg.exe",
			"codex-resources/codex-command-runner.exe",
			"codex-resources/codex-windows-sandbox-setup.exe",
		}
	case "linux":
		return []string{
			"codex-package.json",
			"bin/codex",
			"bin/codex-code-mode-host",
			"codex-path/rg",
			"codex-resources/bwrap",
		}
	case "darwin":
		return []string{
			"codex-package.json",
			"bin/codex",
			"bin/codex-code-mode-host",
			"codex-path/rg",
		}
	default:
		return nil
	}
}

func codexPackagePathExecutable(relative string) bool {
	return relative != "" && relative != "codex-package.json"
}

func cleanCodexPackagePath(name string) (string, error) {
	if name == "" || strings.HasPrefix(name, "/") || strings.Contains(name, `\`) || strings.ContainsRune(name, 0) {
		return "", errors.New("Codex package 路徑不合法")
	}
	cleaned := path.Clean(strings.TrimSuffix(name, "/"))
	if cleaned == "." {
		return "", nil
	}
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", errors.New("Codex package 路徑不合法")
	}
	return cleaned, nil
}

func codexPackageDigest(body []byte, filename string) (string, error) {
	if len(body) == 0 || bytes.Contains(body, []byte{'\r'}) || bytes.Contains(body, []byte{0}) {
		return "", errors.New("Codex checksum manifest 不合法")
	}
	text := strings.TrimSuffix(string(body), "\n")
	if text == "" || strings.Contains(text, "\n\n") {
		return "", errors.New("Codex checksum manifest 不合法")
	}
	digest := ""
	for _, line := range strings.Split(text, "\n") {
		sum, name, ok := strings.Cut(line, "  ")
		if !ok || strings.Contains(name, " ") || len(sum) != sha256.Size*2 || strings.ToLower(sum) != sum {
			return "", errors.New("Codex checksum manifest 行不合法")
		}
		if _, err := hex.DecodeString(sum); err != nil {
			return "", errors.New("Codex checksum manifest 行不合法")
		}
		if name == filename {
			if digest != "" {
				return "", errors.New("Codex checksum manifest 重複 package")
			}
			digest = sum
		}
	}
	if digest == "" {
		return "", errors.New("Codex checksum manifest 沒有目標 package")
	}
	return digest, nil
}

func codexVersionMatches(stdout, version string) bool {
	for _, line := range strings.Split(stdout, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		return len(fields) > 0 && fields[len(fields)-1] == version
	}
	return false
}
