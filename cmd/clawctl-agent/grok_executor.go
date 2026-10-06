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

	"github.com/andybalholm/brotli"
	"github.com/teddashh/AI-Intune/internal/agentadapter"
	"github.com/teddashh/AI-Intune/internal/deploy"
	"github.com/teddashh/AI-Intune/internal/model"
)

const (
	maxGrokBundleEntries = 64
	maxGrokArtifactBytes = int64(1 << 30)
)

var maxGrokBinaryBytes = int64(512 << 20)

type grokExecutor struct {
	deps       execDeps
	targetOS   string
	targetArch string
}

func defaultGrokExecutor(hubURL, token string) grokExecutor {
	return grokExecutor{
		deps:     defaultOpenClawExecutor(hubURL, token).deps,
		targetOS: runtime.GOOS, targetArch: runtime.GOARCH,
	}
}

func (e grokExecutor) Run(ctx context.Context, job model.JobResponse) ([]model.JobVerificationRequest, error) {
	d := (openclawExecutor{deps: e.deps}).withDefaults()
	spec, err := e.gate(job)
	if err != nil {
		return nil, err
	}
	root := filepath.Join(d.home, ".local", "share", "clawctl", "grok")
	releases := filepath.Join(root, "releases")
	if err := d.requireWritableAncestor(root); err != nil {
		return nil, rejectPrecondition("Grok 目錄不可寫：" + err.Error())
	}
	if err := os.MkdirAll(d.fsPath(releases), 0o700); err != nil {
		return []model.JobVerificationRequest{*nodeRuntimeFailure(d, "stage", "建立 Grok releases", err)}, nil
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
		return nil, rejectPrecondition("Grok current 不合法：" + err.Error())
	}
	if samePath(currentRelease, release) {
		return verifyGrokRelease(ctx, d, release, spec.Version, spec.Artifact.SHA256,
			spec.TargetOS, "grok-current"), nil
	}
	if err := setNodeRuntimeCurrent(d, root, release, job.JobID); err != nil {
		return []model.JobVerificationRequest{*nodeRuntimeFailure(d, "activate", "切換 Grok current", err)}, nil
	}
	verifications := verifyGrokRelease(ctx, d, release, spec.Version, spec.Artifact.SHA256,
		spec.TargetOS, "grok-activate")
	if allPassed(verifications) {
		return verifications, nil
	}
	rollback := rollbackNodeRuntimeCurrent(d, root, previous, job.JobID)
	rollback.RuleID = "grok-rollback"
	rollback.Command = "restore Grok current"
	return append(verifications, rollback), nil
}

func (e grokExecutor) gate(job model.JobResponse) (model.GrokSpec, error) {
	var spec model.GrokSpec
	decoder := json.NewDecoder(strings.NewReader(string(job.Spec)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&spec); err != nil {
		return spec, rejectPrecondition("Grok spec 不是合法 JSON：" + err.Error())
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return spec, rejectPrecondition("Grok spec 含有尾隨資料")
	}
	if job.ResourceKind != agentadapter.ExecutorKindGrok || spec.Kind != agentadapter.ExecutorKindGrok {
		return spec, rejectPrecondition("工作單與 spec kind 必須是 grok")
	}
	if job.ResourceID != "grok" || !safePathComponent(spec.Version) || !validNodeRuntimeExactVersion(spec.Version) {
		return spec, rejectPrecondition("Grok identity 不合法")
	}
	if _, ok := grokBundleMember(spec.TargetOS, spec.TargetArch); !ok ||
		spec.TargetOS != e.targetOS || spec.TargetArch != e.targetArch {
		return spec, rejectPrecondition("Grok target 與 agent 平台不一致")
	}
	if spec.BundleLayout != model.GrokBundleLayoutV1 || spec.Artifact == nil {
		return spec, rejectPrecondition("Grok bundle contract 不合法")
	}
	pinned := spec.Artifact
	if len(pinned.SHA256) != sha256.Size*2 {
		return spec, rejectPrecondition("artifact.sha256 不是 64 碼十六進位")
	}
	if _, err := hex.DecodeString(pinned.SHA256); err != nil || strings.ToLower(pinned.SHA256) != pinned.SHA256 {
		return spec, rejectPrecondition("artifact.sha256 必須是小寫十六進位")
	}
	if pinned.Size <= 0 || pinned.Size > maxGrokArtifactBytes {
		return spec, rejectPrecondition("artifact.size 超出 Grok bundle 上限")
	}
	if pinned.URL != "/v1/artifacts/"+pinned.SHA256 ||
		pinned.EnginesNode != "" || pinned.UpstreamTarball != "" || pinned.SHA512 != "" {
		return spec, rejectPrecondition("Grok artifact contract 不合法")
	}
	if job.ArtifactDigest != "sha256:"+pinned.SHA256 {
		return spec, &rejectError{Code: deploy.ArtifactHashMismatch, Detail: "工作單 artifact digest 與 spec 不一致"}
	}
	return spec, nil
}

func (e grokExecutor) ensureRelease(ctx context.Context, d execDeps, job model.JobResponse,
	spec model.GrokSpec, release string,
) (*model.JobVerificationRequest, error) {
	if info, err := os.Lstat(d.fsPath(release)); err == nil {
		if info.IsDir() && allPassed(verifyGrokRelease(ctx, d, release, spec.Version,
			spec.Artifact.SHA256, spec.TargetOS, "grok-stage")) {
			return nil, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		broken := release + ".broken-" + safeJobID(job.JobID)
		if _, err := os.Lstat(d.fsPath(broken)); err == nil {
			return nodeRuntimeFailure(d, "stage", "保留既有 Grok release", errors.New("broken 證據路徑已存在")), nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return nodeRuntimeFailure(d, "stage", "檢查 Grok broken 證據", err), nil
		}
		if err := os.Rename(d.fsPath(release), d.fsPath(broken)); err != nil {
			return nodeRuntimeFailure(d, "stage", "保留既有 Grok release", err), nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nodeRuntimeFailure(d, "stage", "檢查 Grok release", err), nil
	}
	staging := filepath.Join(filepath.Dir(release), ".staging-"+safeJobID(job.JobID))
	if err := os.RemoveAll(d.fsPath(staging)); err != nil {
		return nodeRuntimeFailure(d, "stage", "清理 Grok staging", err), nil
	}
	defer os.RemoveAll(d.fsPath(staging))
	if err := os.MkdirAll(d.fsPath(staging), 0o700); err != nil {
		return nodeRuntimeFailure(d, "stage", "建立 Grok staging", err), nil
	}
	bundle := filepath.Join(staging, "grok.tgz")
	actualDigest, actualSize, err := d.downloadArtifactAtMost(ctx, d.hubURL+spec.Artifact.URL, bundle, spec.Artifact.Size)
	if err != nil {
		var sizeErr *artifactDownloadSizeError
		if errors.As(err, &sizeErr) {
			return nil, &rejectError{Code: deploy.ArtifactHashMismatch,
				Detail: fmt.Sprintf("Grok artifact 超過宣告 size=%d", spec.Artifact.Size)}
		}
		return nodeRuntimeFailure(d, "stage", "下載 Grok bundle", err), nil
	}
	if actualDigest != spec.Artifact.SHA256 || actualSize != spec.Artifact.Size {
		return nil, &rejectError{Code: deploy.ArtifactHashMismatch,
			Detail: fmt.Sprintf("Grok artifact 期望 sha256=%s size=%d，實得 sha256=%s size=%d",
				shortDigest(spec.Artifact.SHA256), spec.Artifact.Size, shortDigest(actualDigest), actualSize)}
	}
	payload := filepath.Join(staging, "payload")
	if err := extractGrokBundle(d.fsPath(bundle), d.fsPath(payload), spec.TargetOS, spec.TargetArch); err != nil {
		return nodeRuntimeFailure(d, "stage", "展開 Grok bundle", err), nil
	}
	marker := filepath.Join(payload, nodeRuntimeArtifactMarker)
	if err := writePrivateFile(d.fsPath(marker), []byte("sha256:"+spec.Artifact.SHA256+"\n")); err != nil {
		return nodeRuntimeFailure(d, "stage", "記錄 Grok artifact identity", err), nil
	}
	checks := verifyGrokRelease(ctx, d, payload, spec.Version, spec.Artifact.SHA256, spec.TargetOS, "grok-stage")
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
		return nodeRuntimeFailure(d, "stage", "發佈 Grok release", err), nil
	}
	if err := syncNodeRuntimeDirectory(d.fsPath(filepath.Dir(release))); err != nil {
		return nodeRuntimeFailure(d, "stage", "同步 Grok releases", err), nil
	}
	return nil, nil
}

func extractGrokBundle(bundle, destination, targetOS, targetArch string) error {
	member, ok := grokBundleMember(targetOS, targetArch)
	command, commandOK := grokCommandRelative(targetOS)
	if !ok || !commandOK {
		return errors.New("Grok target 不合法")
	}
	input, err := os.Open(bundle)
	if err != nil {
		return err
	}
	defer input.Close()
	gz, err := gzip.NewReader(input)
	if err != nil {
		return errors.New("Grok bundle 解不開")
	}
	defer gz.Close()
	if err := os.Mkdir(destination, 0o700); err != nil {
		return err
	}
	binaryPath := filepath.Join(destination, filepath.FromSlash(command))
	if !underDir(binaryPath, destination) {
		return errors.New("Grok 執行檔路徑超出 release")
	}
	reader := tar.NewReader(gz)
	found := false
	entries := 0
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return errors.New("Grok bundle 解不開")
		}
		entries++
		if entries > maxGrokBundleEntries || header.Size < 0 {
			return errors.New("Grok bundle 超出展開上限")
		}
		name, err := cleanGrokBundlePath(header.Name)
		if err != nil {
			return err
		}
		if header.Typeflag == tar.TypeDir {
			if header.Size != 0 {
				return errors.New("Grok bundle 成員不是 regular file")
			}
			continue
		}
		if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA {
			return errors.New("Grok bundle 成員不是 regular file")
		}
		if header.Size <= 0 || header.Size > maxGrokBinaryBytes {
			return errors.New("Grok bundle 成員大小不合法")
		}
		if name != member {
			if err := discardGrokMember(reader, header.Size); err != nil {
				return err
			}
			continue
		}
		if found {
			return errors.New("Grok bundle 目標成員重複")
		}
		if err := decompressGrokMember(reader, header.Size, binaryPath); err != nil {
			return err
		}
		found = true
	}
	if !found {
		return errors.New("Grok bundle 沒有這個平台的執行檔")
	}
	return nil
}

type grokMemberFile interface {
	io.WriteCloser
	Sync() error
}

var openGrokMemberFile = func(destination string) (grokMemberFile, error) {
	return os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o755)
}

func decompressGrokMember(reader io.Reader, size int64, destination string) error {
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return err
	}
	file, err := openGrokMemberFile(destination)
	if err != nil {
		return err
	}
	success := false
	defer func() {
		_ = file.Close()
		if !success {
			_ = os.Remove(destination)
		}
	}()
	entry := &io.LimitedReader{R: reader, N: size}
	n, copyErr := io.Copy(file, io.LimitReader(brotli.NewReader(entry), maxGrokBinaryBytes+1))
	leftover := entry.N
	if leftover > 0 {
		if _, drainErr := io.CopyN(io.Discard, reader, leftover); drainErr != nil && copyErr == nil {
			copyErr = drainErr
		}
	}
	if n > maxGrokBinaryBytes {
		return errors.New("Grok 執行檔超出大小上限")
	}
	if copyErr != nil || n <= 0 || leftover != 0 {
		return errors.New("Grok 執行檔解不開")
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Chmod(destination, 0o755); err != nil {
		return err
	}
	success = true
	return nil
}

func discardGrokMember(reader io.Reader, size int64) error {
	n, err := io.Copy(io.Discard, io.LimitReader(reader, size+1))
	if err != nil || n != size {
		return errors.New("Grok bundle 成員不完整")
	}
	return nil
}

func cleanGrokBundlePath(name string) (string, error) {
	if name == "" || len(name) > 256 || strings.HasPrefix(name, "/") || strings.Contains(name, `\`) || strings.ContainsRune(name, 0) {
		return "", errors.New("Grok bundle 路徑不合法")
	}
	cleaned := path.Clean(strings.TrimSuffix(name, "/"))
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") || strings.Contains("/"+cleaned+"/", "/../") {
		return "", errors.New("Grok bundle 路徑超出範圍")
	}
	if cleaned != "grok" && !strings.HasPrefix(cleaned, "grok/") {
		return "", errors.New("Grok bundle 路徑超出範圍")
	}
	return cleaned, nil
}

func verifyGrokRelease(ctx context.Context, d execDeps, release, version, artifactSHA256, targetOS,
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
	relative, ok := grokCommandRelative(targetOS)
	if !ok {
		return append(results, *nodeRuntimeFailure(d, rulePrefix+"-version", "grok --version",
			errors.New("Grok target 不合法")))
	}
	binaryLogical := filepath.Join(release, filepath.FromSlash(relative))
	binaryPath := d.fsPath(binaryLogical)
	if info, err := os.Lstat(binaryPath); err != nil || !info.Mode().IsRegular() ||
		(runtime.GOOS != "windows" && info.Mode().Perm()&0o111 == 0) {
		if err == nil {
			err = errors.New(relative + " 不是可執行 regular file")
		}
		return append(results, *nodeRuntimeFailure(d, rulePrefix+"-version", binaryLogical+" --version", err))
	}
	stdout, stderr, err := d.run(ctx, binaryPath, "--version")
	passed := err == nil && grokVersionMatches(stdout, version)
	if err == nil && !passed {
		err = fmt.Errorf("Grok version=%q；要 %q", strings.TrimSpace(stdout), version)
	}
	results = append(results, d.verification(rulePrefix+"-version", binaryLogical+" --version",
		stdout, errorText(stderr, err), exitCode(err), passed))
	return results
}

func grokBundleMember(targetOS, targetArch string) (string, bool) {
	switch targetOS + "/" + targetArch {
	case "linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64":
		return "grok/" + targetOS + "-" + targetArch + "/bin/grok.br", true
	case "windows/amd64", "windows/arm64":
		return "grok/" + targetOS + "-" + targetArch + "/bin/grok.exe.br", true
	default:
		return "", false
	}
}

func grokCommandRelative(targetOS string) (string, bool) {
	switch targetOS {
	case "linux", "darwin":
		return "bin/grok", true
	case "windows":
		return "bin/grok.exe", true
	default:
		return "", false
	}
}

func grokVersionMatches(stdout, version string) bool {
	for _, line := range strings.Split(stdout, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		return len(fields) >= 2 && fields[0] == "grok" && fields[1] == version
	}
	return false
}
