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
	"sort"
	"strings"

	"github.com/teddashh/AI-Intune/internal/agentadapter"
	"github.com/teddashh/AI-Intune/internal/deploy"
	"github.com/teddashh/AI-Intune/internal/model"
)

const (
	maxNodeBundleEntries           = 250_000
	maxNodeBundleUncompressedBytes = int64(2 << 30)
	maxNodeBundleFileBytes         = int64(512 << 20)
	maxNodeBundleArtifactBytes     = int64(1 << 30)
	nodeRuntimeArtifactMarker      = ".clawctl-artifact-sha256"
)

type nodeRuntimeExecutor struct {
	deps       execDeps
	targetOS   string
	targetArch string
}

type nodeRuntimeActivation struct {
	existed bool
	target  string
}

func defaultNodeRuntimeExecutor(hubURL, token string) nodeRuntimeExecutor {
	return nodeRuntimeExecutor{
		deps:     defaultOpenClawExecutor(hubURL, token).deps,
		targetOS: runtime.GOOS, targetArch: runtime.GOARCH,
	}
}

func (e nodeRuntimeExecutor) Run(ctx context.Context, job model.JobResponse) ([]model.JobVerificationRequest, error) {
	d := (openclawExecutor{deps: e.deps}).withDefaults()
	spec, err := e.gate(job)
	if err != nil {
		return nil, err
	}
	root := filepath.Join(d.home, ".local", "share", "clawctl", "node-runtime")
	releases := filepath.Join(root, "releases")
	if err := d.requireWritableAncestor(root); err != nil {
		return nil, rejectPrecondition("Node runtime 目錄不可寫：" + err.Error())
	}
	if err := os.MkdirAll(d.fsPath(releases), 0o700); err != nil {
		return []model.JobVerificationRequest{*nodeRuntimeFailure(d, "stage", "建立 Node runtime releases", err)}, nil
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
		return nil, rejectPrecondition("Node runtime current 不合法：" + err.Error())
	}
	if samePath(currentRelease, release) {
		return verifyNodeRuntimeRelease(ctx, d, release, spec.Version, spec.Artifact.SHA256,
			"node-runtime-current"), nil
	}
	if err := setNodeRuntimeCurrent(d, root, release, job.JobID); err != nil {
		return []model.JobVerificationRequest{*nodeRuntimeFailure(d, "activate", "切換 Node runtime current", err)}, nil
	}
	verifications := verifyNodeRuntimeRelease(ctx, d, release, spec.Version, spec.Artifact.SHA256,
		"node-runtime-activate")
	if allPassed(verifications) {
		return verifications, nil
	}
	verifications = append(verifications, rollbackNodeRuntimeCurrent(d, root, previous, job.JobID))
	return verifications, nil
}

func (e nodeRuntimeExecutor) gate(job model.JobResponse) (model.NodeRuntimeSpec, error) {
	var spec model.NodeRuntimeSpec
	decoder := json.NewDecoder(strings.NewReader(string(job.Spec)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&spec); err != nil {
		return spec, rejectPrecondition("Node runtime spec 不是合法 JSON：" + err.Error())
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return spec, rejectPrecondition("Node runtime spec 含有尾隨資料")
	}
	if job.ResourceKind != agentadapter.ExecutorKindNodeRuntime ||
		spec.Kind != agentadapter.ExecutorKindNodeRuntime {
		return spec, rejectPrecondition("工作單與 spec kind 必須是 node-runtime")
	}
	if job.ResourceID != "node-runtime" || !safePathComponent(spec.Version) ||
		!validNodeRuntimeExactVersion(spec.Version) {
		return spec, rejectPrecondition("Node runtime identity 不合法")
	}
	if spec.TargetOS != e.targetOS || spec.TargetArch != e.targetArch ||
		(spec.TargetOS != "linux" && spec.TargetOS != "darwin") ||
		(spec.TargetArch != "amd64" && spec.TargetArch != "arm64") {
		return spec, rejectPrecondition("Node runtime target 與 agent 平台不一致")
	}
	if spec.BundleLayout != model.NodeRuntimeBundleLayoutV1 || spec.Artifact == nil {
		return spec, rejectPrecondition("Node runtime bundle contract 不合法")
	}
	artifact := spec.Artifact
	if len(artifact.SHA256) != sha256.Size*2 {
		return spec, rejectPrecondition("artifact.sha256 不是 64 碼十六進位")
	}
	if _, err := hex.DecodeString(artifact.SHA256); err != nil || strings.ToLower(artifact.SHA256) != artifact.SHA256 {
		return spec, rejectPrecondition("artifact.sha256 必須是小寫十六進位")
	}
	if artifact.Size <= 0 || artifact.Size > maxNodeBundleArtifactBytes {
		return spec, rejectPrecondition("artifact.size 超出 Node runtime bundle 上限")
	}
	if artifact.URL != "/v1/artifacts/"+artifact.SHA256 ||
		artifact.EnginesNode != "" || artifact.UpstreamTarball != "" || artifact.SHA512 != "" {
		return spec, rejectPrecondition("Node runtime artifact contract 不合法")
	}
	if job.ArtifactDigest != "sha256:"+artifact.SHA256 {
		return spec, &rejectError{Code: deploy.ArtifactHashMismatch, Detail: "工作單 artifact digest 與 spec 不一致"}
	}
	return spec, nil
}

func (e nodeRuntimeExecutor) ensureRelease(ctx context.Context, d execDeps, job model.JobResponse,
	spec model.NodeRuntimeSpec, release string,
) (*model.JobVerificationRequest, error) {
	if info, err := os.Lstat(d.fsPath(release)); err == nil {
		if info.IsDir() && allPassed(verifyNodeRuntimeRelease(ctx, d, release, spec.Version,
			spec.Artifact.SHA256, "node-runtime-stage")) {
			return nil, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		broken := release + ".broken-" + safeJobID(job.JobID)
		if _, err := os.Lstat(d.fsPath(broken)); err == nil {
			return nodeRuntimeFailure(d, "stage", "保留既有 Node runtime release", errors.New("broken 證據路徑已存在")), nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return nodeRuntimeFailure(d, "stage", "檢查 Node runtime broken 證據", err), nil
		}
		if err := os.Rename(d.fsPath(release), d.fsPath(broken)); err != nil {
			return nodeRuntimeFailure(d, "stage", "保留既有 Node runtime release", err), nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nodeRuntimeFailure(d, "stage", "檢查 Node runtime release", err), nil
	}

	staging := filepath.Join(filepath.Dir(release), ".staging-"+safeJobID(job.JobID))
	if err := os.RemoveAll(d.fsPath(staging)); err != nil {
		return nodeRuntimeFailure(d, "stage", "清理 Node runtime staging", err), nil
	}
	defer os.RemoveAll(d.fsPath(staging))
	if err := os.MkdirAll(d.fsPath(staging), 0o700); err != nil {
		return nodeRuntimeFailure(d, "stage", "建立 Node runtime staging", err), nil
	}
	bundle := filepath.Join(staging, "node-runtime.tgz")
	actualDigest, actualSize, err := d.downloadArtifactAtMost(ctx, d.hubURL+spec.Artifact.URL, bundle, spec.Artifact.Size)
	if err != nil {
		var sizeErr *artifactDownloadSizeError
		if errors.As(err, &sizeErr) {
			return nil, &rejectError{Code: deploy.ArtifactHashMismatch,
				Detail: fmt.Sprintf("Node runtime artifact 超過宣告 size=%d", spec.Artifact.Size)}
		}
		return nodeRuntimeFailure(d, "stage", "下載 Node runtime bundle", err), nil
	}
	if actualDigest != spec.Artifact.SHA256 || actualSize != spec.Artifact.Size {
		return nil, &rejectError{Code: deploy.ArtifactHashMismatch,
			Detail: fmt.Sprintf("Node runtime artifact 期望 sha256=%s size=%d，實得 sha256=%s size=%d",
				shortDigest(spec.Artifact.SHA256), spec.Artifact.Size, shortDigest(actualDigest), actualSize)}
	}
	payload := filepath.Join(staging, "payload")
	if err := extractNodeRuntimeBundle(d.fsPath(bundle), d.fsPath(payload), spec.TargetOS, spec.TargetArch); err != nil {
		return nodeRuntimeFailure(d, "stage", "展開 Node runtime bundle", err), nil
	}
	marker := filepath.Join(payload, nodeRuntimeArtifactMarker)
	if err := atomicWriteFile(d.fsPath(marker), []byte("sha256:"+spec.Artifact.SHA256+"\n"), 0o600); err != nil {
		return nodeRuntimeFailure(d, "stage", "記錄 Node runtime artifact identity", err), nil
	}
	checks := verifyNodeRuntimeRelease(ctx, d, payload, spec.Version, spec.Artifact.SHA256,
		"node-runtime-stage")
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
		return nodeRuntimeFailure(d, "stage", "發佈 Node runtime release", err), nil
	}
	if err := syncNodeRuntimeDirectory(d.fsPath(filepath.Dir(release))); err != nil {
		return nodeRuntimeFailure(d, "stage", "同步 Node runtime releases", err), nil
	}
	return nil, nil
}

func extractNodeRuntimeBundle(bundle, destination, targetOS, targetArch string) error {
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
	reader := tar.NewReader(gz)
	seen := make(map[string]struct{})
	symlinks := make(map[string]struct{})
	var entries int
	var uncompressed int64
	var targetEntries int
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		entries++
		if entries > maxNodeBundleEntries || header.Size < 0 || header.Size > maxNodeBundleFileBytes ||
			uncompressed > maxNodeBundleUncompressedBytes-header.Size {
			return errors.New("Node runtime bundle 超出展開上限")
		}
		uncompressed += header.Size
		name, platform, relative, err := nodeRuntimeArchivePath(header.Name)
		if err != nil {
			return err
		}
		if _, duplicate := seen[name]; duplicate {
			return fmt.Errorf("Node runtime bundle 路徑重複：%s", name)
		}
		seen[name] = struct{}{}
		if relative == "" {
			if header.Typeflag != tar.TypeDir {
				return fmt.Errorf("Node runtime bundle root 必須是目錄：%s", name)
			}
			continue
		}
		switch header.Typeflag {
		case tar.TypeDir, tar.TypeReg, tar.TypeRegA:
		case tar.TypeSymlink:
			if header.Size != 0 || !validNodeRuntimeLink(relative, header.Linkname) {
				return fmt.Errorf("Node runtime bundle symlink 不合法：%s", relative)
			}
		default:
			return fmt.Errorf("Node runtime bundle 含不支援的 entry type：%s", relative)
		}
		if platform != targetPlatform {
			continue
		}
		targetEntries++
		if nodeRuntimePathHasSymlinkAncestor(relative, symlinks) {
			return fmt.Errorf("Node runtime bundle 不能穿過 symlink：%s", relative)
		}
		dest := filepath.Join(destination, filepath.FromSlash(relative))
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(dest, 0o755); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
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
			if copyErr != nil || written != header.Size {
				return fmt.Errorf("Node runtime bundle 檔案不完整：%s", relative)
			}
			if syncErr != nil {
				return syncErr
			}
			if closeErr != nil {
				return closeErr
			}
		case tar.TypeSymlink:
			if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
				return err
			}
			if err := os.Symlink(header.Linkname, dest); err != nil {
				return err
			}
			symlinks[relative] = struct{}{}
		}
	}
	if targetEntries == 0 {
		return errors.New("Node runtime bundle 沒有目標平台")
	}
	return syncNodeRuntimeDirectory(destination)
}

func nodeRuntimeArchivePath(raw string) (name, platform, relative string, err error) {
	if raw == "" || strings.Contains(raw, "\\") || strings.ContainsRune(raw, '\x00') || path.IsAbs(raw) {
		return "", "", "", errors.New("Node runtime bundle 路徑不合法")
	}
	name = path.Clean(strings.TrimSuffix(raw, "/"))
	if name == "." || name == ".." || strings.HasPrefix(name, "../") ||
		(raw != name && raw != name+"/") {
		return "", "", "", fmt.Errorf("Node runtime bundle 路徑不是 canonical：%s", raw)
	}
	parts := strings.Split(name, "/")
	if parts[0] != "node-runtime" || (len(parts) > 1 &&
		parts[1] != "linux-amd64" && parts[1] != "linux-arm64" &&
		parts[1] != "darwin-amd64" && parts[1] != "darwin-arm64") {
		return "", "", "", fmt.Errorf("Node runtime bundle 路徑超出 layout：%s", name)
	}
	if len(parts) >= 2 {
		platform = parts[1]
	}
	if len(parts) >= 3 {
		relative = strings.Join(parts[2:], "/")
	}
	return name, platform, relative, nil
}

func validNodeRuntimeLink(relative, target string) bool {
	if target == "" || strings.Contains(target, "\\") || strings.ContainsRune(target, '\x00') || path.IsAbs(target) {
		return false
	}
	resolved := path.Clean(path.Join(path.Dir(relative), target))
	return resolved != "." && resolved != ".." && !strings.HasPrefix(resolved, "../")
}

func nodeRuntimePathHasSymlinkAncestor(relative string, symlinks map[string]struct{}) bool {
	for parent := path.Dir(relative); parent != "." && parent != "/"; parent = path.Dir(parent) {
		if _, exists := symlinks[parent]; exists {
			return true
		}
	}
	return false
}

func verifyNodeRuntimeRelease(ctx context.Context, d execDeps, release, version, artifactSHA256,
	rulePrefix string,
) []model.JobVerificationRequest {
	markerLogical := filepath.Join(release, nodeRuntimeArtifactMarker)
	markerPath := d.fsPath(markerLogical)
	markerInfo, err := os.Lstat(markerPath)
	if err != nil || !markerInfo.Mode().IsRegular() || markerInfo.Mode().Perm() != 0o600 || markerInfo.Size() != 72 {
		if err == nil {
			err = errors.New("artifact identity marker 不是 0600 regular file")
		}
		return []model.JobVerificationRequest{*nodeRuntimeFailure(d, rulePrefix+"-artifact",
			"cat "+markerLogical, err)}
	}
	marker, err := os.ReadFile(markerPath)
	wantMarker := "sha256:" + artifactSHA256 + "\n"
	markerPassed := err == nil && string(marker) == wantMarker
	if err == nil && !markerPassed {
		err = errors.New("artifact identity marker 與工作單 digest 不符")
	}
	results := []model.JobVerificationRequest{d.verification(rulePrefix+"-artifact", "cat "+markerLogical,
		string(marker), errorText("", err), exitCode(err), markerPassed)}
	if !markerPassed {
		return results
	}
	nodeLogical := filepath.Join(release, "bin", "node")
	npmLogical := filepath.Join(release, "lib", "node_modules", "npm", "bin", "npm-cli.js")
	nodePath, npmPath := d.fsPath(nodeLogical), d.fsPath(npmLogical)
	if info, err := os.Lstat(nodePath); err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		if err == nil {
			err = errors.New("bin/node 不是可執行 regular file")
		}
		return append(results, *nodeRuntimeFailure(d, rulePrefix+"-node", nodeLogical+" --version", err))
	}
	if info, err := os.Lstat(npmPath); err != nil || !info.Mode().IsRegular() {
		if err == nil {
			err = errors.New("npm-cli.js 不是 regular file")
		}
		return append(results, *nodeRuntimeFailure(d, rulePrefix+"-npm", nodeLogical+" "+npmLogical+" --version", err))
	}
	stdout, stderr, err := d.run(ctx, nodePath, "--version")
	nodePassed := err == nil && strings.TrimSpace(stdout) == "v"+version
	if err == nil && !nodePassed {
		err = fmt.Errorf("Node version=%q；要 %q", strings.TrimSpace(stdout), "v"+version)
	}
	results = append(results, d.verification(rulePrefix+"-node", nodeLogical+" --version",
		stdout, errorText(stderr, err), exitCode(err), nodePassed))
	if !nodePassed {
		return results
	}
	stdout, stderr, err = d.run(ctx, nodePath, npmPath, "--version")
	npmPassed := err == nil && validNodeRuntimeVersionOutput(strings.TrimSpace(stdout))
	if err == nil && !npmPassed {
		err = fmt.Errorf("npm version 輸出不合法：%q", strings.TrimSpace(stdout))
	}
	results = append(results, d.verification(rulePrefix+"-npm", nodeLogical+" "+npmLogical+" --version",
		stdout, errorText(stderr, err), exitCode(err), npmPassed))
	return results
}

func validNodeRuntimeVersionOutput(value string) bool {
	parts := strings.Split(value, ".")
	if len(parts) != 3 {
		return false
	}
	for _, part := range parts {
		if part == "" {
			return false
		}
		for _, char := range part {
			if char < '0' || char > '9' {
				return false
			}
		}
	}
	return true
}

func validNodeRuntimeExactVersion(value string) bool {
	if !validNodeRuntimeVersionOutput(value) {
		return false
	}
	for _, part := range strings.Split(value, ".") {
		if len(part) > 1 && part[0] == '0' {
			return false
		}
	}
	return true
}

func errorText(stderr string, err error) string {
	if err == nil {
		return stderr
	}
	if stderr == "" {
		return err.Error()
	}
	return err.Error() + "：" + stderr
}

func nodeRuntimeFailure(d execDeps, rule, command string, err error) *model.JobVerificationRequest {
	detail := ""
	if err != nil {
		detail = err.Error()
	}
	result := d.verification(rule, command, "", detail, 1, false)
	return &result
}

func readNodeRuntimeCurrent(d execDeps, root, releases string) (nodeRuntimeActivation, string, error) {
	current := filepath.Join(root, "current")
	info, err := os.Lstat(d.fsPath(current))
	if errors.Is(err, os.ErrNotExist) {
		return nodeRuntimeActivation{}, "", nil
	}
	if err != nil {
		return nodeRuntimeActivation{}, "", err
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return nodeRuntimeActivation{}, "", errors.New("current 不是 symlink")
	}
	target, err := os.Readlink(d.fsPath(current))
	if err != nil {
		return nodeRuntimeActivation{}, "", err
	}
	logicalTarget := target
	if !filepath.IsAbs(logicalTarget) {
		logicalTarget = filepath.Join(root, logicalTarget)
	}
	logicalTarget = filepath.Clean(logicalTarget)
	if !underDir(logicalTarget, releases) || filepath.Dir(logicalTarget) != filepath.Clean(releases) {
		return nodeRuntimeActivation{}, "", errors.New("current 指到 releases 外")
	}
	if target != filepath.Join("releases", filepath.Base(logicalTarget)) {
		return nodeRuntimeActivation{}, "", errors.New("current target 不是 canonical relative release")
	}
	if targetInfo, err := os.Stat(d.fsPath(current)); err != nil || !targetInfo.IsDir() {
		if err == nil {
			err = errors.New("current target 不是目錄")
		}
		return nodeRuntimeActivation{}, "", err
	}
	return nodeRuntimeActivation{existed: true, target: target}, logicalTarget, nil
}

func setNodeRuntimeCurrent(d execDeps, root, release, jobID string) error {
	current := filepath.Join(root, "current")
	if info, err := os.Lstat(d.fsPath(current)); err == nil && info.Mode()&os.ModeSymlink == 0 {
		return errors.New("current 不是 symlink")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	target := filepath.Join("releases", filepath.Base(release))
	tmp := current + ".tmp-" + safeJobID(jobID)
	_ = os.Remove(d.fsPath(tmp))
	if err := os.Symlink(target, d.fsPath(tmp)); err != nil {
		return err
	}
	if err := os.Rename(d.fsPath(tmp), d.fsPath(current)); err != nil {
		_ = os.Remove(d.fsPath(tmp))
		return err
	}
	return syncNodeRuntimeDirectory(d.fsPath(root))
}

func rollbackNodeRuntimeCurrent(d execDeps, root string, previous nodeRuntimeActivation, jobID string) model.JobVerificationRequest {
	current := filepath.Join(root, "current")
	var err error
	if previous.existed {
		tmp := current + ".rollback-" + safeJobID(jobID)
		_ = os.Remove(d.fsPath(tmp))
		if err = os.Symlink(previous.target, d.fsPath(tmp)); err == nil {
			err = os.Rename(d.fsPath(tmp), d.fsPath(current))
		}
		if err != nil {
			_ = os.Remove(d.fsPath(tmp))
		}
	} else {
		err = os.Remove(d.fsPath(current))
		if errors.Is(err, os.ErrNotExist) {
			err = nil
		}
	}
	if err == nil {
		err = syncNodeRuntimeDirectory(d.fsPath(root))
	}
	return d.verification("node-runtime-rollback", "restore Node runtime current", "", errorText("", err), exitCode(err), err == nil)
}

func syncNodeRuntimeDirectory(directory string) error {
	fd, err := os.Open(directory)
	if err != nil {
		return err
	}
	defer fd.Close()
	return fd.Sync()
}

func (e nodeRuntimeExecutor) AfterSucceeded(_ context.Context, job model.JobResponse) []string {
	d := (openclawExecutor{deps: e.deps}).withDefaults()
	var spec model.NodeRuntimeSpec
	if json.Unmarshal(job.Spec, &spec) != nil || !safePathComponent(spec.Version) ||
		!validNodeRuntimeExactVersion(spec.Version) {
		return nil
	}
	root := filepath.Join(d.home, ".local", "share", "clawctl", "node-runtime")
	releases := filepath.Join(root, "releases")
	_, current, err := readNodeRuntimeCurrent(d, root, releases)
	if err != nil || current == "" {
		return []string{"Node runtime release retention 失敗：" + errorText("current 缺少", err)}
	}
	entries, err := os.ReadDir(d.fsPath(releases))
	if err != nil {
		return []string{"Node runtime release retention 失敗：" + err.Error()}
	}
	type candidate struct {
		name string
		mod  int64
	}
	var older []candidate
	for _, entry := range entries {
		logical := filepath.Join(releases, entry.Name())
		if samePath(logical, current) || !validNodeRuntimeExactVersion(entry.Name()) || !entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return []string{"Node runtime release retention 失敗：" + err.Error()}
		}
		older = append(older, candidate{name: entry.Name(), mod: info.ModTime().UnixNano()})
	}
	sort.Slice(older, func(i, j int) bool {
		if older[i].mod != older[j].mod {
			return older[i].mod > older[j].mod
		}
		return older[i].name > older[j].name
	})
	var reports []string
	for index, remove := range older {
		if index == 0 {
			continue
		}
		if err := os.RemoveAll(d.fsPath(filepath.Join(releases, remove.name))); err != nil {
			reports = append(reports, "Node runtime release cleanup 失敗："+remove.name+"："+err.Error())
		}
	}
	return reports
}
