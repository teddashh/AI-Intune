package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
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
	"runtime"
	"strings"

	"github.com/teddashh/AI-Intune/internal/agentadapter"
	"github.com/teddashh/AI-Intune/internal/deploy"
	"github.com/teddashh/AI-Intune/internal/model"
	"github.com/teddashh/AI-Intune/internal/processenv"
)

const (
	maxAntigravityBundleEntries = 64
	maxAntigravityArtifactBytes = int64(1 << 30)
	maxAntigravityManifestBytes = int64(64 << 10)
	maxAntigravityOfficialBytes = int64(512 << 20)
)

type antigravityExecutor struct {
	deps       execDeps
	targetOS   string
	targetArch string
}

func defaultAntigravityExecutor(hubURL, token string) antigravityExecutor {
	executor := antigravityExecutor{
		deps:     defaultOpenClawExecutor(hubURL, token).deps,
		targetOS: runtime.GOOS, targetArch: runtime.GOARCH,
	}
	executor.deps.antigravityVersion = runAntigravityVersion
	return executor
}

func (e antigravityExecutor) Run(ctx context.Context, job model.JobResponse) ([]model.JobVerificationRequest, error) {
	d := (openclawExecutor{deps: e.deps}).withDefaults()
	if d.antigravityVersion == nil {
		d.antigravityVersion = runAntigravityVersion
	}
	spec, err := e.gate(job)
	if err != nil {
		return nil, err
	}
	root := filepath.Join(d.home, ".local", "share", "clawctl", "antigravity")
	releases := filepath.Join(root, "releases")
	if err := d.requireWritableAncestor(root); err != nil {
		return nil, rejectPrecondition("Antigravity 目錄不可寫：" + err.Error())
	}
	if err := os.MkdirAll(d.fsPath(releases), 0o700); err != nil {
		return []model.JobVerificationRequest{*nodeRuntimeFailure(d, "stage", "建立 Antigravity releases", err)}, nil
	}
	release := filepath.Join(releases, spec.Version)
	stageVerification, err := e.ensureRelease(ctx, d, job, spec, release)
	if err != nil {
		return nil, err
	}
	if stageVerification != nil {
		return []model.JobVerificationRequest{*stageVerification}, nil
	}
	if err := sealAntigravityRelease(d, release, spec.TargetOS); err != nil {
		return []model.JobVerificationRequest{*nodeRuntimeFailure(d, "stage", "鎖定 Antigravity bin", err)}, nil
	}
	previous, currentRelease, err := readNodeRuntimeCurrent(d, root, releases)
	if err != nil {
		return nil, rejectPrecondition("Antigravity current 不合法：" + err.Error())
	}
	if samePath(currentRelease, release) {
		return verifyAntigravityRelease(ctx, d, release, spec.Version, spec.Artifact.SHA256,
			spec.TargetOS, "antigravity-current"), nil
	}
	if err := setNodeRuntimeCurrent(d, root, release, job.JobID); err != nil {
		return []model.JobVerificationRequest{*nodeRuntimeFailure(d, "activate", "切換 Antigravity current", err)}, nil
	}
	verifications := verifyAntigravityRelease(ctx, d, release, spec.Version, spec.Artifact.SHA256,
		spec.TargetOS, "antigravity-activate")
	if allPassed(verifications) {
		return verifications, nil
	}
	rollback := rollbackNodeRuntimeCurrent(d, root, previous, job.JobID)
	rollback.RuleID = "antigravity-rollback"
	rollback.Command = "restore Antigravity current"
	return append(verifications, rollback), nil
}

func (e antigravityExecutor) gate(job model.JobResponse) (model.AntigravitySpec, error) {
	var spec model.AntigravitySpec
	decoder := json.NewDecoder(strings.NewReader(string(job.Spec)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&spec); err != nil {
		return spec, rejectPrecondition("Antigravity spec 不是合法 JSON：" + err.Error())
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return spec, rejectPrecondition("Antigravity spec 含有尾隨資料")
	}
	if job.ResourceKind != agentadapter.ExecutorKindAntigravity || spec.Kind != agentadapter.ExecutorKindAntigravity {
		return spec, rejectPrecondition("工作單與 spec kind 必須是 antigravity")
	}
	if job.ResourceID != "antigravity" || !safePathComponent(spec.Version) || !validNodeRuntimeExactVersion(spec.Version) {
		return spec, rejectPrecondition("Antigravity identity 不合法")
	}
	if _, ok := antigravityOfficialFile(spec.TargetOS, spec.TargetArch); !ok ||
		spec.TargetOS != e.targetOS || spec.TargetArch != e.targetArch {
		return spec, rejectPrecondition("Antigravity target 與 agent 平台不一致")
	}
	if spec.BundleLayout != model.AntigravityBundleLayoutV1 || spec.Artifact == nil {
		return spec, rejectPrecondition("Antigravity bundle contract 不合法")
	}
	pinned := spec.Artifact
	if len(pinned.SHA256) != sha256.Size*2 {
		return spec, rejectPrecondition("artifact.sha256 不是 64 碼十六進位")
	}
	if _, err := hex.DecodeString(pinned.SHA256); err != nil || strings.ToLower(pinned.SHA256) != pinned.SHA256 {
		return spec, rejectPrecondition("artifact.sha256 必須是小寫十六進位")
	}
	if pinned.Size <= 0 || pinned.Size > maxAntigravityArtifactBytes {
		return spec, rejectPrecondition("artifact.size 超出 Antigravity bundle 上限")
	}
	if pinned.URL != "/v1/artifacts/"+pinned.SHA256 ||
		pinned.EnginesNode != "" || pinned.UpstreamTarball != "" || pinned.SHA512 != "" {
		return spec, rejectPrecondition("Antigravity artifact contract 不合法")
	}
	if job.ArtifactDigest != "sha256:"+pinned.SHA256 {
		return spec, &rejectError{Code: deploy.ArtifactHashMismatch, Detail: "工作單 artifact digest 與 spec 不一致"}
	}
	return spec, nil
}

func (e antigravityExecutor) ensureRelease(ctx context.Context, d execDeps, job model.JobResponse,
	spec model.AntigravitySpec, release string,
) (*model.JobVerificationRequest, error) {
	if info, err := os.Lstat(d.fsPath(release)); err == nil {
		if info.IsDir() && allPassed(verifyAntigravityRelease(ctx, d, release, spec.Version,
			spec.Artifact.SHA256, spec.TargetOS, "antigravity-stage")) {
			return nil, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if err := unsealAntigravityBin(d, release, spec.TargetOS); err != nil {
			return nodeRuntimeFailure(d, "stage", "解除 Antigravity bin", err), nil
		}
		broken := release + ".broken-" + safeJobID(job.JobID)
		if _, err := os.Lstat(d.fsPath(broken)); err == nil {
			return nodeRuntimeFailure(d, "stage", "保留既有 Antigravity release", errors.New("broken 證據路徑已存在")), nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return nodeRuntimeFailure(d, "stage", "檢查 Antigravity broken 證據", err), nil
		}
		if err := os.Rename(d.fsPath(release), d.fsPath(broken)); err != nil {
			return nodeRuntimeFailure(d, "stage", "保留既有 Antigravity release", err), nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nodeRuntimeFailure(d, "stage", "檢查 Antigravity release", err), nil
	}
	staging := filepath.Join(filepath.Dir(release), ".staging-"+safeJobID(job.JobID))
	if err := os.RemoveAll(d.fsPath(staging)); err != nil {
		return nodeRuntimeFailure(d, "stage", "清理 Antigravity staging", err), nil
	}
	defer os.RemoveAll(d.fsPath(staging))
	if err := os.MkdirAll(d.fsPath(staging), 0o700); err != nil {
		return nodeRuntimeFailure(d, "stage", "建立 Antigravity staging", err), nil
	}
	bundle := filepath.Join(staging, "antigravity.tgz")
	actualDigest, actualSize, err := d.downloadArtifactAtMost(ctx, d.hubURL+spec.Artifact.URL, bundle, spec.Artifact.Size)
	if err != nil {
		var sizeErr *artifactDownloadSizeError
		if errors.As(err, &sizeErr) {
			return nil, &rejectError{Code: deploy.ArtifactHashMismatch,
				Detail: fmt.Sprintf("Antigravity artifact 超過宣告 size=%d", spec.Artifact.Size)}
		}
		return nodeRuntimeFailure(d, "stage", "下載 Antigravity bundle", err), nil
	}
	if actualDigest != spec.Artifact.SHA256 || actualSize != spec.Artifact.Size {
		return nil, &rejectError{Code: deploy.ArtifactHashMismatch,
			Detail: fmt.Sprintf("Antigravity artifact 期望 sha256=%s size=%d，實得 sha256=%s size=%d",
				shortDigest(spec.Artifact.SHA256), spec.Artifact.Size, shortDigest(actualDigest), actualSize)}
	}
	payload := filepath.Join(staging, "payload")
	if err := stageAntigravityBundle(d.fsPath(bundle), d.fsPath(staging), d.fsPath(payload), spec.Version, spec.TargetOS, spec.TargetArch); err != nil {
		return nodeRuntimeFailure(d, "stage", "展開 Antigravity bundle", err), nil
	}
	marker := filepath.Join(payload, nodeRuntimeArtifactMarker)
	if err := writePrivateFile(d.fsPath(marker), []byte("sha256:"+spec.Artifact.SHA256+"\n")); err != nil {
		return nodeRuntimeFailure(d, "stage", "記錄 Antigravity artifact identity", err), nil
	}
	checks := verifyAntigravityRelease(ctx, d, payload, spec.Version, spec.Artifact.SHA256, spec.TargetOS, "antigravity-stage")
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
		return nodeRuntimeFailure(d, "stage", "發佈 Antigravity release", err), nil
	}
	if err := syncNodeRuntimeDirectory(d.fsPath(filepath.Dir(release))); err != nil {
		return nodeRuntimeFailure(d, "stage", "同步 Antigravity releases", err), nil
	}
	return nil, nil
}

func sealAntigravityRelease(d execDeps, release, targetOS string) error {
	if targetOS == "windows" || runtime.GOOS == "windows" {
		return nil
	}
	bin := d.fsPath(filepath.Join(release, "bin"))
	binary := d.fsPath(filepath.Join(release, "bin", "agy"))
	if err := os.Chmod(binary, 0o555); err != nil {
		return err
	}
	return os.Chmod(bin, 0o555)
}

func unsealAntigravityBin(d execDeps, release, targetOS string) error {
	if targetOS == "windows" || runtime.GOOS == "windows" {
		return nil
	}
	bin := d.fsPath(filepath.Join(release, "bin"))
	info, err := os.Lstat(bin)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return nil
	}
	return os.Chmod(bin, 0o755)
}

type antigravityStagedManifest struct {
	Version string `json:"version"`
	URL     string `json:"url"`
	SHA512  string `json:"sha512"`
}

func stageAntigravityBundle(bundle, staging, payload, version, targetOS, targetArch string) error {
	fileName, ok := antigravityOfficialFile(targetOS, targetArch)
	if !ok {
		return errors.New("Antigravity target 不合法")
	}
	wantFile := "antigravity/" + targetOS + "-" + targetArch + "/" + fileName
	wantManifest := "antigravity/" + targetOS + "-" + targetArch + "/manifest.json"
	input, err := os.Open(bundle)
	if err != nil {
		return err
	}
	defer input.Close()
	gz, err := gzip.NewReader(input)
	if err != nil {
		return errors.New("Antigravity bundle 解不開")
	}
	defer gz.Close()
	officialPath := filepath.Join(staging, fileName)
	manifestPath := filepath.Join(staging, "manifest.json")
	if !underDir(officialPath, staging) || !underDir(manifestPath, staging) {
		return errors.New("Antigravity 暫存路徑超出 staging")
	}
	reader := tar.NewReader(gz)
	var manifest antigravityStagedManifest
	var gotSHA string
	foundFile, foundManifest := false, false
	entries := 0
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return errors.New("Antigravity bundle 解不開")
		}
		entries++
		if entries > maxAntigravityBundleEntries || header.Size < 0 {
			return errors.New("Antigravity bundle 超出展開上限")
		}
		name, err := cleanAntigravityBundlePath(header.Name)
		if err != nil {
			return err
		}
		if header.Typeflag == tar.TypeDir {
			if header.Size != 0 {
				return errors.New("Antigravity bundle 成員不是 regular file")
			}
			continue
		}
		if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA {
			return errors.New("Antigravity bundle 成員不是 regular file")
		}
		switch name {
		case wantManifest:
			if foundManifest || header.Size <= 0 || header.Size > maxAntigravityManifestBytes {
				return errors.New("Antigravity manifest 不合法")
			}
			body, err := readBoundedMember(reader, header.Size)
			if err != nil {
				return err
			}
			if err := writePrivateFile(manifestPath, body); err != nil {
				return err
			}
			decoded, err := decodeAntigravityStagedManifest(body)
			if err != nil {
				return err
			}
			manifest = decoded
			foundManifest = true
		case wantFile:
			if foundFile || header.Size <= 0 || header.Size > maxAntigravityOfficialBytes {
				return errors.New("Antigravity 官方檔大小不合法")
			}
			sum, err := writeHashedMember(reader, header.Size, officialPath)
			if err != nil {
				return err
			}
			gotSHA = sum
			foundFile = true
		default:
			if header.Size > maxAntigravityOfficialBytes {
				return errors.New("Antigravity bundle 成員大小不合法")
			}
			if err := discardAntigravityMember(reader, header.Size); err != nil {
				return err
			}
		}
	}
	if !foundFile || !foundManifest {
		return errors.New("Antigravity bundle 沒有這個平台的執行檔")
	}
	file, ok := antigravityURLFile(manifest.URL)
	if manifest.Version != version || !ok || file != fileName || !validAntigravitySHA512Hex(manifest.SHA512) || gotSHA != manifest.SHA512 {
		return errors.New("Antigravity manifest 與官方檔不一致")
	}
	if targetOS == "windows" {
		return installAntigravityWindows(officialPath, payload)
	}
	return extractAntigravityUnix(officialPath, payload)
}

func decodeAntigravityStagedManifest(body []byte) (antigravityStagedManifest, error) {
	var document antigravityStagedManifest
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&document); err != nil {
		return document, errors.New("Antigravity manifest 不是合法 JSON")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return document, errors.New("Antigravity manifest 含有尾隨資料")
	}
	if document.Version == "" || document.URL == "" || document.SHA512 == "" {
		return document, errors.New("Antigravity manifest 不完整")
	}
	return document, nil
}

func readBoundedMember(reader io.Reader, size int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(reader, size))
	if err != nil || int64(len(body)) != size {
		return nil, errors.New("Antigravity bundle 成員不完整")
	}
	return body, nil
}

func writeHashedMember(reader io.Reader, size int64, destination string) (string, error) {
	file, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	success := false
	defer func() {
		_ = file.Close()
		if !success {
			_ = os.Remove(destination)
		}
	}()
	digest := sha512.New()
	n, err := io.Copy(io.MultiWriter(file, digest), io.LimitReader(reader, size))
	if err != nil || n != size {
		return "", errors.New("Antigravity bundle 成員不完整")
	}
	if err := file.Sync(); err != nil {
		return "", err
	}
	if err := file.Close(); err != nil {
		return "", err
	}
	success = true
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func discardAntigravityMember(reader io.Reader, size int64) error {
	n, err := io.Copy(io.Discard, io.LimitReader(reader, size+1))
	if err != nil || n != size {
		return errors.New("Antigravity bundle 成員不完整")
	}
	return nil
}

func cleanAntigravityBundlePath(name string) (string, error) {
	if name == "" || len(name) > 256 || strings.HasPrefix(name, "/") || strings.Contains(name, `\`) || strings.ContainsRune(name, 0) {
		return "", errors.New("Antigravity bundle 路徑不合法")
	}
	cleaned := path.Clean(strings.TrimSuffix(name, "/"))
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") || strings.Contains("/"+cleaned+"/", "/../") {
		return "", errors.New("Antigravity bundle 路徑超出範圍")
	}
	if cleaned != "antigravity" && !strings.HasPrefix(cleaned, "antigravity/") {
		return "", errors.New("Antigravity bundle 路徑超出範圍")
	}
	return cleaned, nil
}

func extractAntigravityUnix(archive, destination string) error {
	if err := os.Mkdir(destination, 0o700); err != nil {
		return err
	}
	input, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer input.Close()
	gz, err := gzip.NewReader(input)
	if err != nil {
		return errors.New("Antigravity 官方檔解不開")
	}
	defer gz.Close()
	binary := filepath.Join(destination, "bin", "agy")
	if !underDir(binary, destination) {
		return errors.New("Antigravity 執行檔路徑超出 release")
	}
	if err := os.MkdirAll(filepath.Dir(binary), 0o755); err != nil {
		return err
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
			return errors.New("Antigravity 官方檔解不開")
		}
		entries++
		if entries > 1 || header.Size < 0 {
			return errors.New("Antigravity 官方檔必須只有一個執行檔")
		}
		name := path.Clean(strings.TrimSuffix(header.Name, "/"))
		if header.Typeflag == tar.TypeDir || header.Typeflag == tar.TypeSymlink || header.Typeflag == tar.TypeLink ||
			(header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA) || name != "antigravity" {
			return errors.New("Antigravity 官方檔必須只有一個執行檔")
		}
		if header.Size <= 0 || header.Size > maxAntigravityOfficialBytes {
			return errors.New("Antigravity 執行檔大小不合法")
		}
		if err := writeAntigravityBinary(reader, header.Size, binary); err != nil {
			return err
		}
		found = true
	}
	if !found {
		return errors.New("Antigravity 官方檔必須只有一個執行檔")
	}
	return nil
}

func writeAntigravityBinary(reader io.Reader, size int64, destination string) error {
	file, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o755)
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
	n, err := io.Copy(file, io.LimitReader(reader, size+1))
	if err != nil || n != size || n > maxAntigravityOfficialBytes {
		return errors.New("Antigravity 執行檔大小不合法")
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

func installAntigravityWindows(staged, destination string) error {
	if err := os.Mkdir(destination, 0o700); err != nil {
		return err
	}
	binary := filepath.Join(destination, "bin", "agy.exe")
	if !underDir(binary, destination) {
		return errors.New("Antigravity 執行檔路徑超出 release")
	}
	if err := os.MkdirAll(filepath.Dir(binary), 0o755); err != nil {
		return err
	}
	if err := os.Rename(staged, binary); err != nil {
		return err
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(binary, 0o755); err != nil {
			return err
		}
	}
	return nil
}

func verifyAntigravityRelease(ctx context.Context, d execDeps, release, version, artifactSHA256, targetOS,
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
	relative, ok := antigravityCommandRelative(targetOS)
	if !ok {
		return append(results, *nodeRuntimeFailure(d, rulePrefix+"-version", "agy --version",
			errors.New("Antigravity target 不合法")))
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
	run := d.antigravityVersion
	if run == nil {
		run = runAntigravityVersion
	}
	stdout, stderr, err := run(ctx, binaryPath)
	passed := err == nil && antigravityVersionMatches(stdout, version)
	if err == nil && !passed {
		err = fmt.Errorf("Antigravity version=%q；要 %q", strings.TrimSpace(stdout), version)
	}
	results = append(results, d.verification(rulePrefix+"-version", binaryLogical+" --version",
		stdout, errorText(stderr, err), exitCode(err), passed))
	return results
}

func antigravityOfficialFile(targetOS, targetArch string) (string, bool) {
	switch targetOS + "/" + targetArch {
	case "linux/amd64":
		return "cli_linux_x64.tar.gz", true
	case "linux/arm64":
		return "cli_linux_arm64.tar.gz", true
	case "darwin/amd64":
		return "cli_mac_x64.tar.gz", true
	case "darwin/arm64":
		return "cli_mac_arm64.tar.gz", true
	case "windows/amd64":
		return "cli_windows_x64.exe", true
	case "windows/arm64":
		return "cli_windows_arm64.exe", true
	default:
		return "", false
	}
}

func antigravityCommandRelative(targetOS string) (string, bool) {
	switch targetOS {
	case "linux", "darwin":
		return "bin/agy", true
	case "windows":
		return "bin/agy.exe", true
	default:
		return "", false
	}
}

func antigravityVersionMatches(stdout, version string) bool {
	for _, line := range strings.Split(stdout, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		return line == version
	}
	return false
}

func validAntigravitySHA512Hex(value string) bool {
	if len(value) != sha512.Size*2 {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && hex.EncodeToString(decoded) == value
}

func antigravityURLFile(raw string) (string, bool) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Path == "" {
		return "", false
	}
	base := path.Base(parsed.Path)
	if base == "." || base == "/" || base == "" {
		return "", false
	}
	return base, true
}

func runAntigravityVersion(ctx context.Context, binary string) (string, string, error) {
	cmd := processenv.CommandContext(ctx, binary, "--version")
	cmd.Env = antigravityVersionEnv(cmd.Env)
	stdout := limitBuffer{remaining: maxExecOutput}
	stderr := limitBuffer{remaining: maxExecOutput}
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

func antigravityVersionEnv(inherited []string) []string {
	const key = "AGY_CLI_DISABLE_AUTO_UPDATE"
	result := make([]string, 0, len(inherited)+1)
	for _, variable := range inherited {
		name, _, ok := strings.Cut(variable, "=")
		if ok && antigravityEnvNameEquals(name, key) {
			continue
		}
		result = append(result, variable)
	}
	return append(result, key+"=true")
}

func antigravityEnvNameEquals(got, want string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(got, want)
	}
	return got == want
}
