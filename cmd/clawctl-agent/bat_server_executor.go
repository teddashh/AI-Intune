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
			return nil, rejectPrecondition("port " + strconv.Itoa(model.BATServerPort) + " is already in use by another program, BAT Server cannot start. Stop the program using this port, then redeploy BAT Server.")
		}
		_ = listener.Close()
	}
	needsLink, err := batServerNeedsLink(d)
	if err != nil {
		return nil, err
	}

	root := model.BATServerRoot(d.home)
	if err := d.requireWritableAncestor(root); err != nil {
		return nil, rejectPrecondition("BAT Server directory is not writable: " + err.Error())
	}
	release := filepath.Join(root, "releases", spec.Version)
	if err := os.MkdirAll(d.fsPath(filepath.Dir(release)), 0o700); err != nil {
		return []model.JobVerificationRequest{*nodeRuntimeFailure(d, "stage", "create BAT Server releases", err)}, nil
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
		return []model.JobVerificationRequest{*nodeRuntimeFailure(d, "token", "install BAT Server token", err)}, nil
	}
	dataDir := model.BATServerDataDir(d.home)
	if err := os.MkdirAll(d.fsPath(dataDir), 0o700); err != nil {
		return []model.JobVerificationRequest{*nodeRuntimeFailure(d, "data", "create BAT Server data", err)}, nil
	}
	binaryRel, ok := model.BATServerInstalledBinary(spec.TargetArch)
	if !ok {
		return nil, rejectPrecondition("BAT Server can only be installed on linux/amd64 or linux/arm64")
	}
	binary := filepath.Join(release, filepath.FromSlash(binaryRel))

	unitPath := model.BATServerUnitFile(d.home)
	changed, err := writeBATServerUnit(d, unitPath, []byte(batServerUnitBody(release, binary, tokenFile, dataDir)))
	if err != nil {
		return []model.JobVerificationRequest{*nodeRuntimeFailure(d, "unit", "write "+model.BATServerUnit, err)}, nil
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
	foreign := rejectPrecondition(link + " is not a link created by AI-Intune, BAT Server cannot be installed. Remove this file, then redeploy BAT Server.")
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
		return spec, rejectPrecondition("BAT Server spec is not valid JSON: " + err.Error())
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return spec, rejectPrecondition("BAT Server spec contains trailing data")
	}
	if job.ResourceKind != agentadapter.ExecutorKindBATServer || spec.Kind != agentadapter.ExecutorKindBATServer {
		return spec, rejectPrecondition("job and spec kind must be bat-server")
	}
	if job.ResourceID != "bat-server" || !safePathComponent(spec.Version) || !validNodeRuntimeExactVersion(spec.Version) {
		return spec, rejectPrecondition("invalid BAT Server identity")
	}
	if spec.TargetOS != "linux" || (spec.TargetArch != "amd64" && spec.TargetArch != "arm64") {
		return spec, rejectPrecondition("BAT Server can only be installed on linux/amd64 or linux/arm64")
	}
	if spec.TargetOS != e.targetOS || spec.TargetArch != e.targetArch {
		return spec, rejectPrecondition("BAT Server target does not match agent platform")
	}
	if spec.BundleLayout != model.BATServerBundleLayoutV1 || spec.Artifact == nil {
		return spec, rejectPrecondition("invalid BAT Server bundle contract")
	}
	if len(spec.BinarySHA256) != sha256.Size*2 {
		return spec, rejectPrecondition("binary_sha256 is not 64 hex digits")
	}
	if decoded, err := hex.DecodeString(spec.BinarySHA256); err != nil || len(decoded) != sha256.Size ||
		strings.ToLower(spec.BinarySHA256) != spec.BinarySHA256 {
		return spec, rejectPrecondition("binary_sha256 must be lowercase hex")
	}
	pinned := spec.Artifact
	if len(pinned.SHA256) != sha256.Size*2 {
		return spec, rejectPrecondition("artifact.sha256 is not 64 hex digits")
	}
	if _, err := hex.DecodeString(pinned.SHA256); err != nil || strings.ToLower(pinned.SHA256) != pinned.SHA256 {
		return spec, rejectPrecondition("artifact.sha256 must be lowercase hex")
	}
	if pinned.Size <= 0 || pinned.Size > int64(1<<30) {
		return spec, rejectPrecondition("artifact.size exceeds BAT Server bundle limit")
	}
	if pinned.URL != "/v1/artifacts/"+pinned.SHA256 ||
		pinned.EnginesNode != "" || pinned.UpstreamTarball != "" || pinned.SHA512 != "" {
		return spec, rejectPrecondition("invalid BAT Server artifact contract")
	}
	if job.ArtifactDigest != "sha256:"+pinned.SHA256 {
		return spec, &rejectError{Code: deploy.ArtifactHashMismatch, Detail: "job artifact digest does not match spec"}
	}
	return spec, nil
}

func (e batServerExecutor) ensureRelease(ctx context.Context, d execDeps, job model.JobResponse,
	spec model.BATServerSpec, release string,
) (*model.JobVerificationRequest, error) {
	member, ok := batServerBundleMember(spec.TargetOS, spec.TargetArch)
	if !ok {
		return nodeRuntimeFailure(d, "stage", "extract BAT Server bundle", errors.New("BAT Server member does not exist")), nil
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
			return nodeRuntimeFailure(d, "stage", "preserve existing BAT Server release", errors.New("broken evidence path already exists")), nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return nodeRuntimeFailure(d, "stage", "check BAT Server broken evidence", err), nil
		}
		if err := os.Rename(d.fsPath(release), d.fsPath(broken)); err != nil {
			return nodeRuntimeFailure(d, "stage", "preserve existing BAT Server release", err), nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nodeRuntimeFailure(d, "stage", "check BAT Server release", err), nil
	}
	staging := filepath.Join(filepath.Dir(release), ".staging-"+safeJobID(job.JobID))
	if err := os.RemoveAll(d.fsPath(staging)); err != nil {
		return nodeRuntimeFailure(d, "stage", "clean BAT Server staging", err), nil
	}
	defer os.RemoveAll(d.fsPath(staging))
	if err := os.MkdirAll(d.fsPath(staging), 0o700); err != nil {
		return nodeRuntimeFailure(d, "stage", "create BAT Server staging", err), nil
	}
	bundle := filepath.Join(staging, "bat-server.tgz")
	actualDigest, actualSize, err := d.downloadArtifactAtMost(ctx, d.hubURL+spec.Artifact.URL, bundle, spec.Artifact.Size)
	if err != nil {
		var sizeErr *artifactDownloadSizeError
		if errors.As(err, &sizeErr) {
			return nil, &rejectError{Code: deploy.ArtifactHashMismatch,
				Detail: fmt.Sprintf("BAT Server artifact exceeds declared size=%d", spec.Artifact.Size)}
		}
		return nodeRuntimeFailure(d, "stage", "download BAT Server bundle", err), nil
	}
	if actualDigest != spec.Artifact.SHA256 || actualSize != spec.Artifact.Size {
		return nil, &rejectError{Code: deploy.ArtifactHashMismatch,
			Detail: fmt.Sprintf("BAT Server artifact expected sha256=%s size=%d; got sha256=%s size=%d",
				shortDigest(spec.Artifact.SHA256), spec.Artifact.Size, shortDigest(actualDigest), actualSize)}
	}
	payload := filepath.Join(staging, "payload")
	if err := extractBATServerBundle(d.fsPath(bundle), d.fsPath(payload), member); err != nil {
		return nodeRuntimeFailure(d, "stage", "extract BAT Server bundle", err), nil
	}
	if err := requireBATServerBinary(d, payload, spec); err != nil {
		relative, _ := model.BATServerInstalledBinary(spec.TargetArch)
		return nodeRuntimeFailure(d, "bat-server-binary", "sha256sum "+filepath.Join(payload, filepath.FromSlash(relative)), err), nil
	}
	marker := filepath.Join(payload, nodeRuntimeArtifactMarker)
	if err := writePrivateFile(d.fsPath(marker), []byte("sha256:"+spec.Artifact.SHA256+"\n")); err != nil {
		return nodeRuntimeFailure(d, "stage", "record BAT Server artifact identity", err), nil
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
		return nodeRuntimeFailure(d, "stage", "publish BAT Server release", err), nil
	}
	if err := syncNodeRuntimeDirectory(d.fsPath(filepath.Dir(release))); err != nil {
		return nodeRuntimeFailure(d, "stage", "sync BAT Server releases", err), nil
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
		return errors.New("cannot unpack BAT Server bundle")
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
			return errors.New("cannot unpack BAT Server bundle")
		}
		entries++
		if entries > maxBATServerExtractEntries || header.Size < 0 {
			return errors.New("BAT Server bundle exceeds extraction limit")
		}
		name, err := cleanBATServerBundleMember(header.Name)
		if err != nil {
			return err
		}
		if header.Typeflag == tar.TypeDir {
			continue
		}
		if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA {
			return fmt.Errorf("BAT Server bundle member %s is not a regular file", header.Name)
		}
		if name != member {
			if err := discardBATServerMember(reader, header.Size); err != nil {
				return err
			}
			continue
		}
		if found || header.Size <= 0 || header.Size > maxBATServerMemberBytes {
			return errors.New("BAT Server member exceeds size limit")
		}
		found = true
		if err := unpackBATServerTar(reader, header.Size, destination); err != nil {
			return err
		}
	}
	if !found {
		return errors.New("missing target member in BAT Server bundle")
	}
	return nil
}

func cleanBATServerBundleMember(name string) (string, error) {
	if name == "" || strings.ContainsRune(name, 0) || strings.Contains(name, `\`) || strings.HasPrefix(name, "/") {
		return "", errors.New("BAT Server bundle path out of bounds")
	}
	cleaned := path.Clean(strings.TrimSuffix(name, "/"))
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", errors.New("BAT Server bundle path out of bounds")
	}
	if cleaned != "bat-server" && !strings.HasPrefix(cleaned, "bat-server/") {
		return "", errors.New("BAT Server bundle path out of bounds")
	}
	return cleaned, nil
}

func discardBATServerMember(reader io.Reader, size int64) error {
	if size < 0 || size > maxBATServerMemberBytes {
		return errors.New("BAT Server member exceeds size limit")
	}
	n, err := io.Copy(io.Discard, io.LimitReader(reader, size+1))
	if err != nil || n != size {
		return errors.New("incomplete BAT Server member")
	}
	return nil
}

func unpackBATServerTar(reader io.Reader, size int64, destination string) error {
	if size <= 0 || size > maxBATServerMemberBytes {
		return errors.New("BAT Server member exceeds size limit")
	}
	limited := &io.LimitedReader{R: reader, N: size}
	gz, err := gzip.NewReader(limited)
	if err != nil {
		return errors.New("cannot unpack BAT Server member")
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
			return errors.New("cannot unpack BAT Server member")
		}
		entries++
		if entries > maxBATServerExtractEntries {
			return errors.New("BAT Server member exceeds extraction limit")
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
			return fmt.Errorf("BAT Server member %s is not a regular file", relative)
		}
		if header.Size < 0 || header.Size > maxBATServerMemberBytes || total > maxBATServerMemberBytes-header.Size {
			return errors.New("BAT Server member exceeds size limit")
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
			return errors.New("incomplete BAT Server member")
		}
	}
	return nil
}

func cleanBATServerExtractRel(name string) (string, error) {
	if name == "" || strings.ContainsRune(name, 0) {
		return "", errors.New("invalid BAT Server path")
	}
	slash := strings.ReplaceAll(name, `\`, "/")
	if strings.HasPrefix(slash, "/") || (len(slash) >= 2 && slash[1] == ':') {
		return "", errors.New("BAT Server path outside release")
	}
	cleaned := path.Clean(slash)
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", errors.New("BAT Server path outside release")
	}
	if path.IsAbs(cleaned) || strings.Contains(cleaned, ":") {
		return "", errors.New("BAT Server path outside release")
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
		return errors.New("incomplete BAT Server member")
	}
	return file.Close()
}

func ensureBATServerToken(path string) error {
	info, err := os.Lstat(path)
	if err == nil {
		if !info.Mode().IsRegular() {
			return errors.New("BAT Server token is not a regular file")
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
			return false, errors.New("BAT Server unit contains token value")
		}
		if bytes.Equal(line, []byte("StandardOutput=null")) {
			stdoutDiscarded = true
		}
	}
	if !stdoutDiscarded {
		return false, errors.New("BAT Server unit does not discard stdout")
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
		err = errors.New("BAT Server unit is not active")
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
			"", "no BAT Server responding with local token and certificate on "+address, 1, false))
	}
	return append(checks, d.verification("bat-server-endpoint", model.BATServerEndpointCommand, "authenticated\n", "", 0, true))
}

func verifyBATServerFiles(d execDeps, release string, spec model.BATServerSpec) []model.JobVerificationRequest {
	releaseCmd := "test -d " + release
	releaseInfo, releaseErr := os.Lstat(d.fsPath(release))
	releasePassed := releaseErr == nil && releaseInfo.IsDir()
	if releaseErr == nil && !releasePassed {
		releaseErr = errors.New("BAT Server release is not a directory")
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
		err = errors.New("artifact identity marker does not match job digest")
	}
	results = append(results, d.verification("bat-server-artifact", "cat "+markerLogical,
		string(marker), errorText("", err), exitCode(err), markerPassed))
	if !markerPassed {
		return results
	}
	relative, ok := model.BATServerInstalledBinary(spec.TargetArch)
	if !ok {
		results = append(results, *nodeRuntimeFailure(d, "bat-server-binary", "sha256sum",
			errors.New("invalid BAT Server target")))
		return results
	}
	binaryLogical := filepath.Join(release, filepath.FromSlash(relative))
	binaryPath := d.fsPath(binaryLogical)
	info, statErr := os.Lstat(binaryPath)
	if statErr != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o111 == 0 {
		if statErr == nil {
			statErr = errors.New(relative + " is not an executable regular file")
		}
		results = append(results, *nodeRuntimeFailure(d, "bat-server-binary", "sha256sum "+binaryLogical, statErr))
		return results
	}
	sum, hashErr := hashBATServerFile(binaryPath)
	passed := hashErr == nil && sum == spec.BinarySHA256
	if hashErr == nil && !passed {
		hashErr = errors.New("installed BAT Server binary sha256 does not match spec")
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
		return errors.New("invalid BAT Server target")
	}
	binaryPath := d.fsPath(filepath.Join(release, filepath.FromSlash(relative)))
	info, err := os.Lstat(binaryPath)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o111 == 0 {
		return errors.New(relative + " is not an executable regular file")
	}
	sum, err := hashBATServerFile(binaryPath)
	if err != nil {
		return err
	}
	if sum != spec.BinarySHA256 {
		return errors.New("installed BAT Server binary sha256 does not match spec")
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
		return "", errors.New("BAT Server binary exceeds size limit")
	}
	hash := sha256.New()
	n, err := io.Copy(hash, io.LimitReader(file, maxBATServerMemberBytes+1))
	if err != nil {
		return "", err
	}
	if n != info.Size() || n > maxBATServerMemberBytes {
		return "", errors.New("BAT Server binary exceeds size limit")
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
