package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/teddashh/AI-Intune/internal/agentadapter"
	"github.com/teddashh/AI-Intune/internal/batremote"
	"github.com/teddashh/AI-Intune/internal/deploy"
	"github.com/teddashh/AI-Intune/internal/model"
)

const (
	maxBATServerExtractEntries = 256

	batServerHandshakeAttempts = 30
	batServerHandshakeDelay    = 500 * time.Millisecond
	batServerHandshakeTimeout  = 3 * time.Second
)

var maxBATServerMemberBytes = int64(512 << 20)

type batServerExecutor struct {
	deps       execDeps
	targetOS   string
	targetArch string
	listen     func(network, address string) (net.Listener, error)
	handshake  func(ctx context.Context, tokenFile, dataDir string) error
}

func defaultBATServerExecutor(hubURL, token string) batServerExecutor {
	return batServerExecutor{
		deps:       defaultOpenClawExecutor(hubURL, token).deps,
		targetOS:   runtime.GOOS,
		targetArch: runtime.GOARCH,
		listen:     net.Listen,
		handshake:  batServerHandshake,
	}
}

// batServerHandshake authenticates to the local server with the token and
// certificate this executor installed. A unit that is active but never bound
// the port, or that answers with other credentials, fails here.
func batServerHandshake(ctx context.Context, tokenFile, dataDir string) error {
	endpoint, err := batremote.LoadEndpoint(tokenFile, dataDir, model.BATServerPort)
	if err != nil {
		return err
	}
	dialCtx, cancel := context.WithTimeout(ctx, batServerHandshakeTimeout)
	defer cancel()
	session, err := batremote.Dial(dialCtx, endpoint, "clawctl-agent", "bat-server-verify", batremote.ClientInfo{
		AppName: "clawctl-agent", AppVersion: version, Label: "bat-server-verify",
		Platform: runtime.GOOS + "/" + runtime.GOARCH,
	})
	if err != nil {
		return err
	}
	_ = session.Close()
	return nil
}

func (e batServerExecutor) Run(ctx context.Context, job model.JobResponse) ([]model.JobVerificationRequest, error) {
	d := (openclawExecutor{deps: e.deps}).withDefaults()
	spec, err := e.gate(job)
	if err != nil {
		return nil, err
	}

	// Only AI-Intune's own unit may hold the port. When that unit is not
	// running, a listener there belongs to another program.
	if state, _, _ := d.systemctl(ctx, "--user", "is-active", model.BATServerUnit); strings.TrimSpace(state) != "active" {
		listener, err := e.listen("tcp", net.JoinHostPort(batremote.LoopbackHost, strconv.Itoa(model.BATServerPort)))
		if err != nil {
			return nil, rejectPrecondition("連接埠 " + strconv.Itoa(model.BATServerPort) + " 已被其他程式使用，BAT Server 無法啟動。停止使用該連接埠的程式後，重新部署 BAT Server。")
		}
		_ = listener.Close()
	}
	needsLink, err := batServerNeedsLink(d)
	if err != nil {
		return nil, err
	}

	root := model.BATServerRoot(d.home)
	if err := d.requireWritableAncestor(root); err != nil {
		return nil, rejectPrecondition("BAT Server 目錄不可寫：" + err.Error())
	}
	release := filepath.Join(root, "releases", spec.Version)
	if err := os.MkdirAll(d.fsPath(filepath.Dir(release)), 0o700); err != nil {
		return []model.JobVerificationRequest{*nodeRuntimeFailure(d, "stage", "建立 BAT Server releases", err)}, nil
	}
	failure, err := e.ensureRelease(ctx, d, job, spec, release)
	if err != nil {
		return nil, err
	}
	if failure != nil {
		return []model.JobVerificationRequest{*failure}, nil
	}
	tokenFile := model.BATServerTokenFile(d.home)
	if err := ensureBATServerToken(d.fsPath(tokenFile)); err != nil {
		return []model.JobVerificationRequest{*nodeRuntimeFailure(d, "token", "安裝 BAT Server token", err)}, nil
	}
	dataDir := model.BATServerDataDir(d.home)
	if err := os.MkdirAll(d.fsPath(dataDir), 0o700); err != nil {
		return []model.JobVerificationRequest{*nodeRuntimeFailure(d, "data", "建立 BAT Server data", err)}, nil
	}
	binaryRel, ok := model.BATServerInstalledBinary(spec.TargetArch)
	if !ok {
		return nil, rejectPrecondition("BAT Server 只能安裝在 linux/amd64 或 linux/arm64")
	}
	binary := filepath.Join(release, filepath.FromSlash(binaryRel))

	unitPath := model.BATServerUnitFile(d.home)
	changed, err := writeBATServerUnit(d, unitPath, []byte(batServerUnitBody(release, binary, tokenFile, dataDir)))
	if err != nil {
		return []model.JobVerificationRequest{*nodeRuntimeFailure(d, "unit", "寫入 "+model.BATServerUnit, err)}, nil
	}

	if needsLink {
		if _, stderr, err := d.systemctl(ctx, "--user", "link", unitPath); err != nil {
			checks := verifyBATServerFiles(d, release, spec)
			checks = append(checks, d.verification("bat-server-unit", "systemctl --user is-active "+model.BATServerUnit,
				"", "link BAT Server: "+commandErr(stderr, err), 1, false))
			return checks, nil
		}
		changed = true
	}
	if activateErr := activateBATServer(ctx, d, changed); activateErr != nil {
		checks := verifyBATServerFiles(d, release, spec)
		checks = append(checks, d.verification("bat-server-unit", "systemctl --user is-active "+model.BATServerUnit,
			"", activateErr.Error(), 1, false))
		return checks, nil
	}
	return e.verifyBATServerService(ctx, d, release, tokenFile, dataDir, spec), nil
}

// batServerNeedsLink reports whether systemd still needs the link to
// AI-Intune's unit file. The executor never writes under ~/.config/systemd/user
// itself: `systemctl --user link` creates the link, and anything else already
// at that path belongs to someone else and stops the install.
func batServerNeedsLink(d execDeps) (bool, error) {
	link := filepath.Join(d.home, ".config", "systemd", "user", model.BATServerUnit)
	foreign := rejectPrecondition(link + " 不是 AI-Intune 建立的連結，BAT Server 無法安裝。移除這個檔案後，重新部署 BAT Server。")
	info, err := os.Lstat(d.fsPath(link))
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		return false, foreign
	}
	if target, err := os.Readlink(d.fsPath(link)); err != nil || target != model.BATServerUnitFile(d.home) {
		return false, foreign
	}
	return false, nil
}

func (e batServerExecutor) gate(job model.JobResponse) (model.BATServerSpec, error) {
	var spec model.BATServerSpec
	decoder := json.NewDecoder(strings.NewReader(string(job.Spec)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&spec); err != nil {
		return spec, rejectPrecondition("BAT Server spec 不是合法 JSON：" + err.Error())
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return spec, rejectPrecondition("BAT Server spec 含有尾隨資料")
	}
	if job.ResourceKind != agentadapter.ExecutorKindBATServer || spec.Kind != agentadapter.ExecutorKindBATServer {
		return spec, rejectPrecondition("工作單與 spec kind 必須是 bat-server")
	}
	if job.ResourceID != "bat-server" || !safePathComponent(spec.Version) || !validNodeRuntimeExactVersion(spec.Version) {
		return spec, rejectPrecondition("BAT Server identity 不合法")
	}
	if spec.TargetOS != "linux" || (spec.TargetArch != "amd64" && spec.TargetArch != "arm64") {
		return spec, rejectPrecondition("BAT Server 只能安裝在 linux/amd64 或 linux/arm64")
	}
	if spec.TargetOS != e.targetOS || spec.TargetArch != e.targetArch {
		return spec, rejectPrecondition("BAT Server target 與 agent 平台不一致")
	}
	if spec.BundleLayout != model.BATServerBundleLayoutV1 || spec.Artifact == nil {
		return spec, rejectPrecondition("BAT Server bundle contract 不合法")
	}
	if len(spec.BinarySHA256) != sha256.Size*2 {
		return spec, rejectPrecondition("binary_sha256 不是 64 碼十六進位")
	}
	if decoded, err := hex.DecodeString(spec.BinarySHA256); err != nil || len(decoded) != sha256.Size ||
		strings.ToLower(spec.BinarySHA256) != spec.BinarySHA256 {
		return spec, rejectPrecondition("binary_sha256 必須是小寫十六進位")
	}
	pinned := spec.Artifact
	if len(pinned.SHA256) != sha256.Size*2 {
		return spec, rejectPrecondition("artifact.sha256 不是 64 碼十六進位")
	}
	if _, err := hex.DecodeString(pinned.SHA256); err != nil || strings.ToLower(pinned.SHA256) != pinned.SHA256 {
		return spec, rejectPrecondition("artifact.sha256 必須是小寫十六進位")
	}
	if pinned.Size <= 0 || pinned.Size > int64(1<<30) {
		return spec, rejectPrecondition("artifact.size 超出 BAT Server bundle 上限")
	}
	if pinned.URL != "/v1/artifacts/"+pinned.SHA256 ||
		pinned.EnginesNode != "" || pinned.UpstreamTarball != "" || pinned.SHA512 != "" {
		return spec, rejectPrecondition("BAT Server artifact contract 不合法")
	}
	if job.ArtifactDigest != "sha256:"+pinned.SHA256 {
		return spec, &rejectError{Code: deploy.ArtifactHashMismatch, Detail: "工作單 artifact digest 與 spec 不一致"}
	}
	return spec, nil
}

func (e batServerExecutor) ensureRelease(ctx context.Context, d execDeps, job model.JobResponse,
	spec model.BATServerSpec, release string,
) (*model.JobVerificationRequest, error) {
	member, ok := batServerBundleMember(spec.TargetOS, spec.TargetArch)
	if !ok {
		return nodeRuntimeFailure(d, "stage", "展開 BAT Server bundle", errors.New("BAT Server 成員不存在")), nil
	}
	if info, err := os.Lstat(d.fsPath(release)); err == nil {
		if info.IsDir() && allPassed(verifyBATServerFiles(d, release, spec)) {
			return nil, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		broken := release + ".broken-" + safeJobID(job.JobID)
		if _, err := os.Lstat(d.fsPath(broken)); err == nil {
			return nodeRuntimeFailure(d, "stage", "保留既有 BAT Server release", errors.New("broken 證據路徑已存在")), nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return nodeRuntimeFailure(d, "stage", "檢查 BAT Server broken 證據", err), nil
		}
		if err := os.Rename(d.fsPath(release), d.fsPath(broken)); err != nil {
			return nodeRuntimeFailure(d, "stage", "保留既有 BAT Server release", err), nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nodeRuntimeFailure(d, "stage", "檢查 BAT Server release", err), nil
	}
	staging := filepath.Join(filepath.Dir(release), ".staging-"+safeJobID(job.JobID))
	if err := os.RemoveAll(d.fsPath(staging)); err != nil {
		return nodeRuntimeFailure(d, "stage", "清理 BAT Server staging", err), nil
	}
	defer os.RemoveAll(d.fsPath(staging))
	if err := os.MkdirAll(d.fsPath(staging), 0o700); err != nil {
		return nodeRuntimeFailure(d, "stage", "建立 BAT Server staging", err), nil
	}
	bundle := filepath.Join(staging, "bat-server.tgz")
	actualDigest, actualSize, err := d.downloadArtifactAtMost(ctx, d.hubURL+spec.Artifact.URL, bundle, spec.Artifact.Size)
	if err != nil {
		var sizeErr *artifactDownloadSizeError
		if errors.As(err, &sizeErr) {
			return nil, &rejectError{Code: deploy.ArtifactHashMismatch,
				Detail: fmt.Sprintf("BAT Server artifact 超過宣告 size=%d", spec.Artifact.Size)}
		}
		return nodeRuntimeFailure(d, "stage", "下載 BAT Server bundle", err), nil
	}
	if actualDigest != spec.Artifact.SHA256 || actualSize != spec.Artifact.Size {
		return nil, &rejectError{Code: deploy.ArtifactHashMismatch,
			Detail: fmt.Sprintf("BAT Server artifact 期望 sha256=%s size=%d，實得 sha256=%s size=%d",
				shortDigest(spec.Artifact.SHA256), spec.Artifact.Size, shortDigest(actualDigest), actualSize)}
	}
	payload := filepath.Join(staging, "payload")
	if err := extractBATServerBundle(d.fsPath(bundle), d.fsPath(payload), member); err != nil {
		return nodeRuntimeFailure(d, "stage", "展開 BAT Server bundle", err), nil
	}
	if err := requireBATServerBinary(d, payload, spec); err != nil {
		relative, _ := model.BATServerInstalledBinary(spec.TargetArch)
		return nodeRuntimeFailure(d, "bat-server-binary", "sha256sum "+filepath.Join(payload, filepath.FromSlash(relative)), err), nil
	}
	marker := filepath.Join(payload, nodeRuntimeArtifactMarker)
	if err := writePrivateFile(d.fsPath(marker), []byte("sha256:"+spec.Artifact.SHA256+"\n")); err != nil {
		return nodeRuntimeFailure(d, "stage", "記錄 BAT Server artifact identity", err), nil
	}
	checks := verifyBATServerFiles(d, payload, spec)
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
		return nodeRuntimeFailure(d, "stage", "發佈 BAT Server release", err), nil
	}
	if err := syncNodeRuntimeDirectory(d.fsPath(filepath.Dir(release))); err != nil {
		return nodeRuntimeFailure(d, "stage", "同步 BAT Server releases", err), nil
	}
	return nil, nil
}

func batServerBundleMember(targetOS, targetArch string) (string, bool) {
	if targetOS != "linux" {
		return "", false
	}
	switch targetArch {
	case "amd64":
		return "bat-server/linux-amd64/bat-server.tar.gz", true
	case "arm64":
		return "bat-server/linux-arm64/bat-server.tar.gz", true
	default:
		return "", false
	}
}

func extractBATServerBundle(bundle, destination, member string) error {
	input, err := os.Open(bundle)
	if err != nil {
		return err
	}
	defer input.Close()
	gz, err := gzip.NewReader(input)
	if err != nil {
		return errors.New("BAT Server bundle 解不開")
	}
	defer gz.Close()
	if err := os.MkdirAll(destination, 0o755); err != nil {
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
			return errors.New("BAT Server bundle 解不開")
		}
		entries++
		if entries > maxBATServerExtractEntries || header.Size < 0 {
			return errors.New("BAT Server bundle 超出展開上限")
		}
		name, err := cleanBATServerBundleMember(header.Name)
		if err != nil {
			return err
		}
		if header.Typeflag == tar.TypeDir {
			continue
		}
		if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA {
			return fmt.Errorf("BAT Server bundle 成員 %s 不是 regular file", header.Name)
		}
		if name != member {
			if err := discardBATServerMember(reader, header.Size); err != nil {
				return err
			}
			continue
		}
		if found || header.Size <= 0 || header.Size > maxBATServerMemberBytes {
			return errors.New("BAT Server 成員超出大小上限")
		}
		found = true
		if err := unpackBATServerTar(reader, header.Size, destination); err != nil {
			return err
		}
	}
	if !found {
		return errors.New("BAT Server bundle 缺少目標成員")
	}
	return nil
}

func cleanBATServerBundleMember(name string) (string, error) {
	if name == "" || strings.ContainsRune(name, 0) || strings.Contains(name, `\`) || strings.HasPrefix(name, "/") {
		return "", errors.New("BAT Server bundle 路徑超出範圍")
	}
	cleaned := path.Clean(strings.TrimSuffix(name, "/"))
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", errors.New("BAT Server bundle 路徑超出範圍")
	}
	if cleaned != "bat-server" && !strings.HasPrefix(cleaned, "bat-server/") {
		return "", errors.New("BAT Server bundle 路徑超出範圍")
	}
	return cleaned, nil
}

func discardBATServerMember(reader io.Reader, size int64) error {
	if size < 0 || size > maxBATServerMemberBytes {
		return errors.New("BAT Server 成員超出大小上限")
	}
	n, err := io.Copy(io.Discard, io.LimitReader(reader, size+1))
	if err != nil || n != size {
		return errors.New("BAT Server 成員不完整")
	}
	return nil
}

func unpackBATServerTar(reader io.Reader, size int64, destination string) error {
	if size <= 0 || size > maxBATServerMemberBytes {
		return errors.New("BAT Server 成員超出大小上限")
	}
	limited := &io.LimitedReader{R: reader, N: size}
	gz, err := gzip.NewReader(limited)
	if err != nil {
		return errors.New("BAT Server 成員解不開")
	}
	defer gz.Close()
	inner := tar.NewReader(gz)
	var total int64
	entries := 0
	for {
		header, err := inner.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return errors.New("BAT Server 成員解不開")
		}
		entries++
		if entries > maxBATServerExtractEntries {
			return errors.New("BAT Server 成員超出展開上限")
		}
		relative, err := cleanBATServerExtractRel(header.Name)
		if err != nil {
			return err
		}
		if relative == "" || relative == "." {
			continue
		}
		target := filepath.Join(destination, filepath.FromSlash(relative))
		if header.Typeflag == tar.TypeDir {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA {
			return fmt.Errorf("BAT Server 成員 %s 不是 regular file", relative)
		}
		if header.Size < 0 || header.Size > maxBATServerMemberBytes || total > maxBATServerMemberBytes-header.Size {
			return errors.New("BAT Server 成員超出大小上限")
		}
		total += header.Size
		mode := os.FileMode(0o644)
		if header.Mode&0o111 != 0 {
			mode = 0o755
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err := writeBATServerMember(target, inner, header.Size, mode); err != nil {
			return err
		}
	}
	if limited.N > 0 {
		if _, err := io.Copy(io.Discard, limited); err != nil {
			return errors.New("BAT Server 成員不完整")
		}
	}
	return nil
}

func cleanBATServerExtractRel(name string) (string, error) {
	if name == "" || strings.ContainsRune(name, 0) {
		return "", errors.New("BAT Server 路徑不合法")
	}
	slash := strings.ReplaceAll(name, `\`, "/")
	if strings.HasPrefix(slash, "/") || (len(slash) >= 2 && slash[1] == ':') {
		return "", errors.New("BAT Server 路徑超出 release")
	}
	cleaned := path.Clean(slash)
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", errors.New("BAT Server 路徑超出 release")
	}
	if path.IsAbs(cleaned) || strings.Contains(cleaned, ":") {
		return "", errors.New("BAT Server 路徑超出 release")
	}
	return cleaned, nil
}

func writeBATServerMember(path string, reader io.Reader, size int64, mode os.FileMode) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	defer file.Close()
	if err := file.Chmod(mode); err != nil {
		return err
	}
	n, err := io.Copy(file, io.LimitReader(reader, size+1))
	if err != nil || n != size {
		return errors.New("BAT Server 成員不完整")
	}
	return file.Close()
}

func ensureBATServerToken(path string) error {
	info, err := os.Lstat(path)
	if err == nil {
		if !info.Mode().IsRegular() {
			return errors.New("BAT Server token 不是 regular file")
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		body, readErr := io.ReadAll(io.LimitReader(file, 4096))
		closeErr := file.Close()
		if readErr != nil {
			return readErr
		}
		if closeErr != nil {
			return closeErr
		}
		if kept := bytes.TrimSpace(body); len(kept) > 0 {
			return nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return writePrivateFile(path, []byte(hex.EncodeToString(raw)))
}

func batServerUnitBody(release, binary, tokenFile, dataDir string) string {
	return "[Unit]\n" +
		"Description=AI-Intune BAT Server\n" +
		"\n" +
		"[Service]\n" +
		"Type=simple\n" +
		"WorkingDirectory=" + release + "\n" +
		"ExecStart=" + binary + " --bind=localhost --port=" + strconv.Itoa(model.BATServerPort) + " --token-file=" + tokenFile + " --data-dir=" + dataDir + "\n" +
		"StandardOutput=null\n" +
		"StandardError=journal\n" +
		"UMask=0077\n" +
		"Restart=on-failure\n" +
		"\n" +
		"[Install]\n" +
		"WantedBy=default.target\n"
}

func writeBATServerUnit(d execDeps, logical string, body []byte) (bool, error) {
	stdoutDiscarded := false
	for _, line := range bytes.Split(body, []byte{'\n'}) {
		if bytes.Contains(line, []byte("--token=")) {
			return false, errors.New("BAT Server unit 含有 token 值")
		}
		if bytes.Equal(line, []byte("StandardOutput=null")) {
			stdoutDiscarded = true
		}
	}
	if !stdoutDiscarded {
		return false, errors.New("BAT Server unit 未丟棄 stdout")
	}
	fsPath := d.fsPath(logical)
	existing, err := os.ReadFile(fsPath)
	if err == nil && bytes.Equal(existing, body) {
		return false, nil
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	if err := os.MkdirAll(filepath.Dir(fsPath), 0o755); err != nil {
		return false, err
	}
	if err := atomicWriteFile(fsPath, body, 0o644); err != nil {
		return false, err
	}
	return true, nil
}

func activateBATServer(ctx context.Context, d execDeps, reload bool) error {
	if reload {
		if _, stderr, err := d.systemctl(ctx, "--user", "daemon-reload"); err != nil {
			return fmt.Errorf("daemon-reload: %s", commandErr(stderr, err))
		}
	}
	if _, stderr, err := d.systemctl(ctx, "--user", "enable", model.BATServerUnit); err != nil {
		return fmt.Errorf("enable BAT Server: %s", commandErr(stderr, err))
	}
	if _, stderr, err := d.systemctl(ctx, "--user", "restart", model.BATServerUnit); err != nil {
		return fmt.Errorf("restart BAT Server: %s", commandErr(stderr, err))
	}
	return nil
}

func (e batServerExecutor) verifyBATServerService(ctx context.Context, d execDeps, release, tokenFile, dataDir string, spec model.BATServerSpec) []model.JobVerificationRequest {
	checks := verifyBATServerFiles(d, release, spec)
	stdout, stderr, err := d.systemctl(ctx, "--user", "is-active", model.BATServerUnit)
	passed := err == nil && strings.TrimSpace(stdout) == "active"
	if err == nil && !passed {
		err = errors.New("BAT Server unit 不是 active")
	}
	checks = append(checks, d.verification("bat-server-unit", "systemctl --user is-active "+model.BATServerUnit,
		stdout, errorText(stderr, err), exitCode(err), passed))
	if !allPassed(checks) {
		return checks
	}

	// The unit reports active before the server has bound its port, so the
	// handshake retries. Its error is never recorded: it can carry peer text.
	var handshakeErr error
	for attempt := 1; ; attempt++ {
		handshakeErr = e.handshake(ctx, tokenFile, dataDir)
		if handshakeErr == nil || attempt == batServerHandshakeAttempts || ctx.Err() != nil {
			break
		}
		if d.sleep(ctx, batServerHandshakeDelay) != nil {
			break
		}
	}
	if handshakeErr != nil {
		address := net.JoinHostPort(batremote.LoopbackHost, strconv.Itoa(model.BATServerPort))
		return append(checks, d.verification("bat-server-endpoint", model.BATServerEndpointCommand,
			"", address+" 上沒有以本機 token 與憑證回應的 BAT Server。", 1, false))
	}
	return append(checks, d.verification("bat-server-endpoint", model.BATServerEndpointCommand, "authenticated\n", "", 0, true))
}

func verifyBATServerFiles(d execDeps, release string, spec model.BATServerSpec) []model.JobVerificationRequest {
	releaseCmd := "test -d " + release
	releaseInfo, releaseErr := os.Lstat(d.fsPath(release))
	releasePassed := releaseErr == nil && releaseInfo.IsDir()
	if releaseErr == nil && !releasePassed {
		releaseErr = errors.New("BAT Server release 不是目錄")
	}
	results := []model.JobVerificationRequest{d.verification("bat-server-release", releaseCmd,
		release+"\n", errorText("", releaseErr), exitCode(releaseErr), releasePassed)}
	if !releasePassed {
		return results
	}
	markerLogical := filepath.Join(release, nodeRuntimeArtifactMarker)
	marker, err := readPrivateRegularFile(d.fsPath(markerLogical))
	markerPassed := err == nil && string(marker) == "sha256:"+spec.Artifact.SHA256+"\n"
	if err == nil && !markerPassed {
		err = errors.New("artifact identity marker 與工作單 digest 不符")
	}
	results = append(results, d.verification("bat-server-artifact", "cat "+markerLogical,
		string(marker), errorText("", err), exitCode(err), markerPassed))
	if !markerPassed {
		return results
	}
	relative, ok := model.BATServerInstalledBinary(spec.TargetArch)
	if !ok {
		results = append(results, *nodeRuntimeFailure(d, "bat-server-binary", "sha256sum",
			errors.New("BAT Server target 不合法")))
		return results
	}
	binaryLogical := filepath.Join(release, filepath.FromSlash(relative))
	binaryPath := d.fsPath(binaryLogical)
	info, statErr := os.Lstat(binaryPath)
	if statErr != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o111 == 0 {
		if statErr == nil {
			statErr = errors.New(relative + " 不是可執行 regular file")
		}
		results = append(results, *nodeRuntimeFailure(d, "bat-server-binary", "sha256sum "+binaryLogical, statErr))
		return results
	}
	sum, hashErr := hashBATServerFile(binaryPath)
	passed := hashErr == nil && sum == spec.BinarySHA256
	if hashErr == nil && !passed {
		hashErr = errors.New("安裝的 BAT Server binary sha256 與 spec 不一致")
	}
	stdout := ""
	if hashErr == nil || sum != "" {
		stdout = sum + "\n"
	}
	results = append(results, d.verification("bat-server-binary", "sha256sum "+binaryLogical,
		stdout, errorText("", hashErr), exitCode(hashErr), passed))
	return results
}

func requireBATServerBinary(d execDeps, release string, spec model.BATServerSpec) error {
	relative, ok := model.BATServerInstalledBinary(spec.TargetArch)
	if !ok {
		return errors.New("BAT Server target 不合法")
	}
	binaryPath := d.fsPath(filepath.Join(release, filepath.FromSlash(relative)))
	info, err := os.Lstat(binaryPath)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o111 == 0 {
		return errors.New(relative + " 不是可執行 regular file")
	}
	sum, err := hashBATServerFile(binaryPath)
	if err != nil {
		return err
	}
	if sum != spec.BinarySHA256 {
		return errors.New("安裝的 BAT Server binary sha256 與 spec 不一致")
	}
	return nil
}

func hashBATServerFile(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > maxBATServerMemberBytes {
		return "", errors.New("BAT Server binary 超出大小上限")
	}
	hash := sha256.New()
	n, err := io.Copy(hash, io.LimitReader(file, maxBATServerMemberBytes+1))
	if err != nil {
		return "", err
	}
	if n != info.Size() || n > maxBATServerMemberBytes {
		return "", errors.New("BAT Server binary 超出大小上限")
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
