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
	"time"

	"github.com/teddashh/AI-Intune/internal/agentadapter"
	"github.com/teddashh/AI-Intune/internal/deploy"
	"github.com/teddashh/AI-Intune/internal/model"
)

const (
	hermesUnit                       = "clawctl-hermes.service"
	hermesContainer                  = "clawctl-hermes"
	hermesPodmanPath                 = "/usr/bin/podman"
	maxHermesBundleArtifactBytes     = int64(3 << 30)
	maxHermesBundleUncompressedBytes = int64(6 << 30)
	maxHermesBundleBlobBytes         = int64(2 << 30)
	maxHermesBundleMetadataBytes     = int64(4 << 20)
	maxHermesBundleEntries           = 2048
	maxHermesBundleLayers            = 512
	hermesOCIIndexMedia              = "application/vnd.oci.image.index.v1+json"
	hermesOCIManifestMedia           = "application/vnd.oci.image.manifest.v1+json"
	hermesDockerManifestMedia        = "application/vnd.docker.distribution.manifest.v2+json"
)

type hermesExecutor struct {
	deps       execDeps
	targetOS   string
	targetArch string
	podmanPath string
}

type hermesSwitchSnapshot struct {
	envExisted      bool
	env             []byte
	hermesActive    bool
	hermesEnabled   bool
	openClawActive  bool
	openClawEnabled bool
	openClawPort    int
}

type hermesOCIDescriptor struct {
	MediaType   string             `json:"mediaType"`
	Digest      string             `json:"digest"`
	Size        int64              `json:"size"`
	Platform    *hermesOCIPlatform `json:"platform,omitempty"`
	Annotations map[string]string  `json:"annotations,omitempty"`
}

type hermesOCIPlatform struct {
	Architecture string `json:"architecture"`
	OS           string `json:"os"`
	Variant      string `json:"variant,omitempty"`
}

type hermesOCIIndex struct {
	SchemaVersion int                   `json:"schemaVersion"`
	MediaType     string                `json:"mediaType"`
	Manifests     []hermesOCIDescriptor `json:"manifests"`
}

type hermesOCIManifest struct {
	SchemaVersion int                   `json:"schemaVersion"`
	MediaType     string                `json:"mediaType"`
	Config        hermesOCIDescriptor   `json:"config"`
	Layers        []hermesOCIDescriptor `json:"layers"`
}

func defaultHermesExecutor(hubURL, token string) hermesExecutor {
	return hermesExecutor{
		deps:     defaultOpenClawExecutor(hubURL, token).deps,
		targetOS: runtime.GOOS, targetArch: runtime.GOARCH, podmanPath: hermesPodmanPath,
	}
}

func (e hermesExecutor) Run(ctx context.Context, job model.JobResponse) ([]model.JobVerificationRequest, error) {
	d := (openclawExecutor{deps: e.deps}).withDefaults()
	spec, err := e.gate(job, d)
	if err != nil {
		return nil, err
	}
	if current, readErr := readHermesEnvironment(d, e.hermesEnvPath(d)); readErr == nil &&
		current == hermesEnvironment(spec) {
		checks := e.verify(ctx, d, spec)
		if allPassed(checks) {
			return checks, nil
		}
	}

	root := filepath.Join(d.home, ".local", "share", "clawctl", "hermes")
	if err := d.requireWritableAncestor(root); err != nil {
		return nil, rejectPrecondition("Hermes directory is not writable: " + err.Error())
	}
	if err := os.MkdirAll(d.fsPath(filepath.Join(root, "data")), 0o700); err != nil {
		return []model.JobVerificationRequest{e.failure(d, "hermes-stage", "create Hermes data", err)}, nil
	}
	staging := filepath.Join(root, ".staging-"+safeJobID(job.JobID))
	if err := os.RemoveAll(d.fsPath(staging)); err != nil {
		return []model.JobVerificationRequest{e.failure(d, "hermes-stage", "clean Hermes staging", err)}, nil
	}
	defer os.RemoveAll(d.fsPath(staging))
	if err := os.Mkdir(d.fsPath(staging), 0o700); err != nil {
		return []model.JobVerificationRequest{e.failure(d, "hermes-stage", "create Hermes staging", err)}, nil
	}
	bundle := filepath.Join(staging, "hermes-oci.tgz")
	actualDigest, actualSize, err := d.downloadArtifactAtMost(ctx, d.hubURL+spec.Artifact.URL,
		bundle, spec.Artifact.Size)
	if err != nil {
		var sizeErr *artifactDownloadSizeError
		if errors.As(err, &sizeErr) {
			return nil, &rejectError{Code: deploy.ArtifactHashMismatch, Detail: "Hermes artifact exceeds declared size"}
		}
		return []model.JobVerificationRequest{e.failure(d, "hermes-stage", "download Hermes OCI bundle", err)}, nil
	}
	if actualDigest != spec.Artifact.SHA256 || actualSize != spec.Artifact.Size {
		return nil, &rejectError{Code: deploy.ArtifactHashMismatch,
			Detail: fmt.Sprintf("Hermes artifact expected sha256=%s size=%d; got sha256=%s size=%d",
				shortDigest(spec.Artifact.SHA256), spec.Artifact.Size, shortDigest(actualDigest), actualSize)}
	}
	manifestDigest, err := validateHermesOCIBundle(d.fsPath(bundle), spec.TargetOS, spec.TargetArch, spec.ImageReference)
	if err != nil {
		return []model.JobVerificationRequest{e.failure(d, "hermes-stage", "verify Hermes OCI bundle", err)}, nil
	}
	loadUnit := "clawctl-hermes-load-" + safeJobID(job.JobID)
	stdout, stderr, err := e.runAsUserService(ctx, d, loadUnit, e.podman(), "load", "--input", d.fsPath(bundle))
	if err != nil {
		failure := e.failure(d, "hermes-load", "load Hermes image", err)
		failure.StdoutExcerpt = excerpt(stdout, maxExecOutput)
		failure.StderrExcerpt = excerpt(errorText(stderr, err), maxExecOutput)
		return []model.JobVerificationRequest{failure}, nil
	}
	if _, stderr, err := e.runAsUserService(ctx, d, "clawctl-hermes-image-"+safeJobID(job.JobID),
		e.podman(), "image", "exists", spec.ImageReference); err != nil {
		return []model.JobVerificationRequest{e.failure(d, "hermes-load", "verify Hermes image", errors.New(errorText(stderr, err)))}, nil
	}

	snapshot, err := e.snapshot(ctx, d)
	if err != nil {
		return nil, rejectPrecondition("invalid Hermes service state: " + err.Error())
	}
	if err := e.activate(ctx, d, spec, snapshot); err != nil {
		failed := e.failure(d, "hermes-activate", "switch Hermes service", err)
		rollback := e.rollbackWithOwnBudget(ctx, d, job, snapshot)
		return []model.JobVerificationRequest{failed, rollback}, nil
	}
	checks := e.verify(ctx, d, spec)
	checks = append(checks, d.verification("hermes-manifest", "OCI platform manifest",
		manifestDigest, "", 0, true))
	if allPassed(checks) {
		return checks, nil
	}
	return append(checks, e.rollbackWithOwnBudget(ctx, d, job, snapshot)), nil
}

func (e hermesExecutor) gate(job model.JobResponse, d execDeps) (model.HermesSpec, error) {
	var spec model.HermesSpec
	decoder := json.NewDecoder(bytes.NewReader(job.Spec))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&spec); err != nil {
		return spec, rejectPrecondition("Hermes spec is not valid JSON: " + err.Error())
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return spec, rejectPrecondition("Hermes spec contains trailing data")
	}
	if job.ResourceKind != agentadapter.ExecutorKindHermes || spec.Kind != agentadapter.ExecutorKindHermes ||
		job.ResourceID != "hermes-agent" || !validHermesExactVersion(spec.Version) {
		return spec, rejectPrecondition("invalid Hermes identity")
	}
	if spec.TargetOS != e.targetOS || spec.TargetArch != e.targetArch || spec.TargetOS != "linux" ||
		(spec.TargetArch != "amd64" && spec.TargetArch != "arm64") {
		return spec, rejectPrecondition("Hermes target does not match agent platform")
	}
	if spec.BundleLayout != model.HermesOCIBundleLayoutV1 || spec.Artifact == nil ||
		spec.ImageReference != "docker.io/nousresearch/hermes-agent:v"+spec.Version ||
		!validHermesDigest(spec.ImageIndexDigest) {
		return spec, rejectPrecondition("invalid Hermes image contract")
	}
	artifact := spec.Artifact
	if !validHermesSHA256(artifact.SHA256) || artifact.Size <= 0 || artifact.Size > maxHermesBundleArtifactBytes ||
		artifact.URL != "/v1/artifacts/"+artifact.SHA256 || artifact.EnginesNode != "" ||
		artifact.UpstreamTarball != "" || artifact.SHA512 != "" {
		return spec, rejectPrecondition("invalid Hermes artifact contract")
	}
	if job.ArtifactDigest != "sha256:"+artifact.SHA256 {
		return spec, &rejectError{Code: deploy.ArtifactHashMismatch, Detail: "job artifact digest does not match Hermes spec"}
	}
	for _, logical := range []string{e.podman(), filepath.Join(d.home, ".config", "systemd", "user", hermesUnit)} {
		info, err := os.Lstat(d.fsPath(logical))
		if err != nil || !info.Mode().IsRegular() || (logical == e.podman() && info.Mode().Perm()&0o111 == 0) {
			return spec, rejectPrecondition("missing Hermes runtime: " + logical)
		}
	}
	return spec, nil
}

func (e hermesExecutor) podman() string {
	if e.podmanPath == "" {
		return hermesPodmanPath
	}
	return e.podmanPath
}

func validHermesExactVersion(value string) bool {
	return validNodeRuntimeExactVersion(value)
}

func validHermesSHA256(value string) bool {
	if len(value) != sha256.Size*2 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func validHermesDigest(value string) bool {
	raw, ok := strings.CutPrefix(value, "sha256:")
	return ok && validHermesSHA256(raw)
}

func (e hermesExecutor) hermesEnvPath(d execDeps) string {
	return filepath.Join(d.home, ".config", "clawctl", "hermes.env")
}

func hermesEnvironment(spec model.HermesSpec) string {
	return "HERMES_IMAGE=" + spec.ImageReference + "\n" +
		"HERMES_IMAGE_INDEX=" + spec.ImageIndexDigest + "\n"
}

func readHermesEnvironment(d execDeps, logical string) (string, error) {
	info, err := os.Lstat(d.fsPath(logical))
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > 1024 {
		return "", errors.New("Hermes environment is not a regular file")
	}
	body, err := os.ReadFile(d.fsPath(logical))
	if err != nil {
		return "", err
	}
	return string(body), nil
}

func (e hermesExecutor) snapshot(ctx context.Context, d execDeps) (hermesSwitchSnapshot, error) {
	snapshot := hermesSwitchSnapshot{}
	envPath := e.hermesEnvPath(d)
	if body, err := readHermesEnvironment(d, envPath); err == nil {
		snapshot.envExisted, snapshot.env = true, []byte(body)
	} else if !errors.Is(err, os.ErrNotExist) {
		return snapshot, err
	}
	snapshot.hermesActive = unitIsActive(ctx, d, hermesUnit)
	snapshot.hermesEnabled = unitIsEnabled(ctx, d, hermesUnit)
	snapshot.openClawActive = unitIsActive(ctx, d, openClawUnit)
	snapshot.openClawEnabled = unitIsEnabled(ctx, d, openClawUnit)
	if snapshot.openClawActive {
		install := d.discover(ctx)
		if install == nil {
			return snapshot, errors.New("active OpenClaw missing verifiable gateway port")
		}
		port, ok := gatewayPort(install.GatewayArgs)
		if !ok {
			return snapshot, errors.New("active OpenClaw missing verifiable gateway port")
		}
		snapshot.openClawPort = port
	}
	return snapshot, nil
}

func unitIsActive(ctx context.Context, d execDeps, unit string) bool {
	_, _, err := d.systemctl(ctx, "--user", "is-active", "--quiet", unit)
	return err == nil
}

func unitIsEnabled(ctx context.Context, d execDeps, unit string) bool {
	_, _, err := d.systemctl(ctx, "--user", "is-enabled", "--quiet", unit)
	return err == nil
}

func (e hermesExecutor) activate(ctx context.Context, d execDeps, spec model.HermesSpec,
	snapshot hermesSwitchSnapshot,
) error {
	if snapshot.openClawActive {
		if _, stderr, err := d.systemctl(ctx, "--user", "stop", openClawUnit); err != nil {
			return fmt.Errorf("stop OpenClaw: %s", commandErr(stderr, err))
		}
	}
	if snapshot.openClawEnabled {
		if _, stderr, err := d.systemctl(ctx, "--user", "disable", openClawUnit); err != nil {
			return fmt.Errorf("disable OpenClaw: %s", commandErr(stderr, err))
		}
	}
	if err := atomicWriteFile(d.fsPath(e.hermesEnvPath(d)), []byte(hermesEnvironment(spec)), 0o600); err != nil {
		return err
	}
	if _, stderr, err := d.systemctl(ctx, "--user", "daemon-reload"); err != nil {
		return fmt.Errorf("daemon-reload: %s", commandErr(stderr, err))
	}
	if _, stderr, err := d.systemctl(ctx, "--user", "enable", hermesUnit); err != nil {
		return fmt.Errorf("enable Hermes: %s", commandErr(stderr, err))
	}
	if _, stderr, err := d.systemctl(ctx, "--user", "restart", hermesUnit); err != nil {
		return fmt.Errorf("restart Hermes: %s", commandErr(stderr, err))
	}
	return nil
}

func (e hermesExecutor) verify(ctx context.Context, d execDeps, spec model.HermesSpec) []model.JobVerificationRequest {
	if stdout, stderr, err := d.systemctl(ctx, "--user", "is-active", hermesUnit); err != nil || strings.TrimSpace(stdout) != "active" {
		if err == nil {
			err = errors.New("Hermes unit is not active")
		}
		return []model.JobVerificationRequest{d.verification("hermes-unit", "systemctl --user is-active "+hermesUnit,
			stdout, errorText(stderr, err), exitCode(err), false)}
	}
	checks := []model.JobVerificationRequest{d.verification("hermes-unit", "systemctl --user is-active "+hermesUnit,
		"active", "", 0, true)}
	stdout, stderr, err := e.runAsUserService(ctx, d, "clawctl-hermes-container-check", e.podman(),
		"container", "inspect", "--format={{.State.Running}}|{{.ImageName}}", hermesContainer)
	want := "true|" + spec.ImageReference
	passed := err == nil && strings.TrimSpace(stdout) == want
	if err == nil && !passed {
		err = fmt.Errorf("Hermes container identity=%q; want %q", strings.TrimSpace(stdout), want)
	}
	checks = append(checks, d.verification("hermes-container", e.podman()+" container inspect "+hermesContainer,
		stdout, errorText(stderr, err), exitCode(err), passed))
	if !passed {
		return checks
	}
	stdout, stderr, err = e.runAsUserService(ctx, d, "clawctl-hermes-image-check", e.podman(),
		"image", "exists", spec.ImageReference)
	passed = err == nil
	checks = append(checks, d.verification("hermes-image", e.podman()+" image exists "+spec.ImageReference,
		stdout, errorText(stderr, err), exitCode(err), passed))
	return checks
}

func (e hermesExecutor) runAsUserService(ctx context.Context, d execDeps, unit, name string,
	args ...string,
) (string, string, error) {
	wrapped := append([]string{"--user", "--wait", "--pipe", "--quiet", "--collect", "--service-type=exec",
		"--unit=" + unit, "--", name}, args...)
	return d.run(ctx, "systemd-run", wrapped...)
}

func (e hermesExecutor) rollbackWithOwnBudget(parent context.Context, d execDeps, job model.JobResponse,
	snapshot hermesSwitchSnapshot,
) model.JobVerificationRequest {
	ctx := context.WithoutCancel(parent)
	cancel := context.CancelFunc(func() {})
	if job.ExecutionTimeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, durationSeconds(job.ExecutionTimeout))
	}
	defer cancel()
	var problems []string
	if _, stderr, err := d.systemctl(ctx, "--user", "stop", hermesUnit); err != nil {
		problems = append(problems, "stop Hermes: "+commandErr(stderr, err))
	}
	envPath := d.fsPath(e.hermesEnvPath(d))
	if snapshot.envExisted {
		if err := atomicWriteFile(envPath, snapshot.env, 0o600); err != nil {
			problems = append(problems, "restore Hermes environment: "+err.Error())
		}
	} else if err := os.Remove(envPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		problems = append(problems, "remove Hermes environment: "+err.Error())
	}
	if !snapshot.hermesEnabled {
		if _, stderr, err := d.systemctl(ctx, "--user", "disable", hermesUnit); err != nil {
			problems = append(problems, "disable Hermes: "+commandErr(stderr, err))
		}
	} else if _, stderr, err := d.systemctl(ctx, "--user", "enable", hermesUnit); err != nil {
		problems = append(problems, "enable Hermes: "+commandErr(stderr, err))
	}
	if snapshot.openClawEnabled {
		if _, stderr, err := d.systemctl(ctx, "--user", "enable", openClawUnit); err != nil {
			problems = append(problems, "enable OpenClaw: "+commandErr(stderr, err))
		}
	} else if _, stderr, err := d.systemctl(ctx, "--user", "disable", openClawUnit); err != nil {
		problems = append(problems, "disable OpenClaw: "+commandErr(stderr, err))
	}
	if snapshot.hermesActive {
		if _, stderr, err := d.systemctl(ctx, "--user", "restart", hermesUnit); err != nil {
			problems = append(problems, "restart Hermes: "+commandErr(stderr, err))
		}
	}
	if snapshot.openClawActive {
		if _, stderr, err := d.systemctl(ctx, "--user", "start", openClawUnit); err != nil {
			problems = append(problems, "start OpenClaw: "+commandErr(stderr, err))
		}
	}
	result := d.verification("hermes-rollback", "restore previous agent runtime", "", "", 0, true)
	if snapshot.openClawActive {
		result = d.health(ctx, snapshot.openClawPort, "hermes-rollback")
	}
	if len(problems) > 0 {
		result.Passed = false
		result.ExitCode = 1
		result.StderrExcerpt = excerpt(strings.Join(append(problems, result.StderrExcerpt), "; "), maxExecOutput)
	}
	return result
}

func durationSeconds(seconds int) time.Duration { return time.Duration(seconds) * time.Second }

func (e hermesExecutor) failure(d execDeps, rule, command string, err error) model.JobVerificationRequest {
	detail := ""
	if err != nil {
		detail = err.Error()
	}
	return d.verification(rule, command, "", detail, 1, false)
}

func validateHermesOCIBundle(filename, targetOS, targetArch, imageReference string) (string, error) {
	input, err := os.Open(filename)
	if err != nil {
		return "", err
	}
	defer input.Close()
	gz, err := gzip.NewReader(input)
	if err != nil {
		return "", err
	}
	defer gz.Close()
	reader := tar.NewReader(gz)
	metadata := make(map[string][]byte)
	sizes := make(map[string]int64)
	seen := make(map[string]struct{})
	var total int64
	for entries := 0; ; entries++ {
		header, nextErr := reader.Next()
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			return "", nextErr
		}
		if entries >= maxHermesBundleEntries || header.Typeflag != tar.TypeReg || header.Size <= 0 ||
			header.Size > maxHermesBundleBlobBytes || total > maxHermesBundleUncompressedBytes-header.Size {
			return "", errors.New("Hermes OCI bundle exceeds layout limit")
		}
		total += header.Size
		name := path.Clean(header.Name)
		if name != header.Name || path.IsAbs(name) || strings.Contains(name, "\\") || strings.ContainsRune(name, '\x00') {
			return "", errors.New("invalid Hermes OCI bundle path")
		}
		if _, duplicate := seen[name]; duplicate {
			return "", errors.New("duplicate path in Hermes OCI bundle")
		}
		seen[name] = struct{}{}
		isMetadata := name == "oci-layout" || name == "index.json"
		blobHex, isBlob := strings.CutPrefix(name, "blobs/sha256/")
		if !isMetadata && (!isBlob || !validHermesSHA256(blobHex)) {
			return "", errors.New("Hermes OCI bundle path outside layout")
		}
		hash := sha256.New()
		var body bytes.Buffer
		writer := io.Writer(hash)
		if header.Size <= maxHermesBundleMetadataBytes {
			writer = io.MultiWriter(hash, &body)
		}
		written, copyErr := io.CopyN(writer, reader, header.Size)
		if copyErr != nil || written != header.Size {
			return "", errors.New("incomplete Hermes OCI bundle entry")
		}
		if isBlob && hex.EncodeToString(hash.Sum(nil)) != blobHex {
			return "", errors.New("Hermes OCI blob digest mismatch")
		}
		sizes[name] = header.Size
		if header.Size <= maxHermesBundleMetadataBytes {
			metadata[name] = body.Bytes()
		}
	}
	var layout struct {
		ImageLayoutVersion string `json:"imageLayoutVersion"`
	}
	if err := decodeHermesOCIJSON(metadata["oci-layout"], &layout); err != nil || layout.ImageLayoutVersion != "1.0.0" {
		return "", errors.New("invalid Hermes OCI layout")
	}
	var root hermesOCIIndex
	if err := decodeHermesOCIJSON(metadata["index.json"], &root); err != nil || root.SchemaVersion != 2 ||
		root.MediaType != hermesOCIIndexMedia || len(root.Manifests) != 1 {
		return "", errors.New("invalid Hermes OCI root index")
	}
	rootDescriptor := root.Manifests[0]
	if rootDescriptor.MediaType != hermesOCIIndexMedia || rootDescriptor.Platform != nil ||
		!validHermesDigest(rootDescriptor.Digest) || len(rootDescriptor.Annotations) != 1 ||
		rootDescriptor.Annotations["org.opencontainers.image.ref.name"] != imageReference {
		return "", errors.New("Hermes OCI image reference mismatch")
	}
	referenced := map[string]struct{}{}
	rootBlob := hermesOCIBlobPath(rootDescriptor.Digest)
	if sizes[rootBlob] != rootDescriptor.Size {
		return "", errors.New("Hermes OCI platform index descriptor mismatch")
	}
	referenced[rootBlob] = struct{}{}
	var index hermesOCIIndex
	if err := decodeHermesOCIJSON(metadata[rootBlob], &index); err != nil || index.SchemaVersion != 2 ||
		index.MediaType != hermesOCIIndexMedia || len(index.Manifests) != 2 {
		return "", errors.New("invalid Hermes OCI platform index")
	}
	selected := ""
	platforms := make(map[string]struct{}, 2)
	for _, descriptor := range index.Manifests {
		if descriptor.Platform == nil || descriptor.Platform.OS != "linux" || descriptor.Platform.Variant != "" ||
			(descriptor.Platform.Architecture != "amd64" && descriptor.Platform.Architecture != "arm64") ||
			(descriptor.MediaType != hermesOCIManifestMedia && descriptor.MediaType != hermesDockerManifestMedia) ||
			!validHermesDigest(descriptor.Digest) {
			return "", errors.New("invalid Hermes OCI platform descriptor")
		}
		platformKey := descriptor.Platform.OS + "/" + descriptor.Platform.Architecture
		if _, duplicate := platforms[platformKey]; duplicate {
			return "", errors.New("duplicate Hermes OCI platform descriptor")
		}
		platforms[platformKey] = struct{}{}
		manifestPath := hermesOCIBlobPath(descriptor.Digest)
		if sizes[manifestPath] != descriptor.Size {
			return "", errors.New("Hermes OCI manifest descriptor mismatch")
		}
		referenced[manifestPath] = struct{}{}
		var manifest hermesOCIManifest
		if err := decodeHermesOCIJSON(metadata[manifestPath], &manifest); err != nil || manifest.SchemaVersion != 2 ||
			manifest.MediaType != descriptor.MediaType || len(manifest.Layers) == 0 || len(manifest.Layers) > maxHermesBundleLayers {
			return "", errors.New("invalid Hermes OCI image manifest")
		}
		content := append([]hermesOCIDescriptor{manifest.Config}, manifest.Layers...)
		for _, item := range content {
			if item.Platform != nil || !validHermesDigest(item.Digest) || item.Size <= 0 ||
				sizes[hermesOCIBlobPath(item.Digest)] != item.Size {
				return "", errors.New("Hermes OCI content descriptor mismatch")
			}
			referenced[hermesOCIBlobPath(item.Digest)] = struct{}{}
		}
		if platformKey == targetOS+"/"+targetArch {
			selected = descriptor.Digest
		}
	}
	if selected == "" {
		return "", errors.New("Hermes OCI bundle has no target platform")
	}
	for name := range sizes {
		if name == "oci-layout" || name == "index.json" {
			continue
		}
		if _, ok := referenced[name]; !ok {
			return "", errors.New("Hermes OCI bundle contains unreferenced blob")
		}
	}
	return selected, nil
}

func hermesOCIBlobPath(digest string) string {
	return "blobs/sha256/" + strings.TrimPrefix(digest, "sha256:")
}

func decodeHermesOCIJSON(body []byte, target any) error {
	if len(body) == 0 || len(body) > int(maxHermesBundleMetadataBytes) {
		return errors.New("missing or oversized OCI JSON")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("OCI JSON contains trailing data")
	}
	return nil
}
