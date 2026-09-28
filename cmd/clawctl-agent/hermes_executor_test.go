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
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/agentadapter"
	"github.com/teddashh/AI-Intune/internal/deploy"
	"github.com/teddashh/AI-Intune/internal/model"
)

type hermesExecutorState struct {
	active         map[string]bool
	enabled        map[string]bool
	image          string
	loaded         bool
	badContainer   bool
	commands       [][]string
	systemdActions [][]string
}

func hermesTestBundle(t *testing.T, reference string) []byte {
	t.Helper()
	blobs := make(map[string][]byte)
	platformDescriptors := make([]hermesOCIDescriptor, 0, 2)
	for _, arch := range []string{"amd64", "arm64"} {
		config := []byte(`{"architecture":"` + arch + `","os":"linux"}`)
		layer := []byte("Hermes layer " + arch)
		configDescriptor := hermesTestBlob(blobs, "application/vnd.oci.image.config.v1+json", config)
		layerDescriptor := hermesTestBlob(blobs, "application/vnd.oci.image.layer.v1.tar+gzip", layer)
		manifestBody, err := json.Marshal(hermesOCIManifest{
			SchemaVersion: 2, MediaType: hermesOCIManifestMedia,
			Config: configDescriptor, Layers: []hermesOCIDescriptor{layerDescriptor},
		})
		if err != nil {
			t.Fatal(err)
		}
		manifestBody = append(manifestBody, '\n')
		descriptor := hermesTestBlob(blobs, hermesOCIManifestMedia, manifestBody)
		descriptor.Platform = &hermesOCIPlatform{Architecture: arch, OS: "linux"}
		platformDescriptors = append(platformDescriptors, descriptor)
	}
	nestedBody, err := json.Marshal(hermesOCIIndex{
		SchemaVersion: 2, MediaType: hermesOCIIndexMedia, Manifests: platformDescriptors,
	})
	if err != nil {
		t.Fatal(err)
	}
	nestedBody = append(nestedBody, '\n')
	nested := hermesTestBlob(blobs, hermesOCIIndexMedia, nestedBody)
	rootBody, err := json.Marshal(hermesOCIIndex{
		SchemaVersion: 2, MediaType: hermesOCIIndexMedia,
		Manifests: []hermesOCIDescriptor{{
			MediaType: hermesOCIIndexMedia, Digest: nested.Digest, Size: nested.Size,
			Annotations: map[string]string{"org.opencontainers.image.ref.name": reference},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{
		"oci-layout": []byte("{\"imageLayoutVersion\":\"1.0.0\"}\n"),
		"index.json": append(rootBody, '\n'),
	}
	for digest, body := range blobs {
		files[hermesOCIBlobPath(digest)] = body
	}
	return hermesTestTar(t, files)
}

func hermesTestBlob(blobs map[string][]byte, mediaType string, body []byte) hermesOCIDescriptor {
	sum := sha256.Sum256(body)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	blobs[digest] = body
	return hermesOCIDescriptor{MediaType: mediaType, Digest: digest, Size: int64(len(body))}
}

func hermesTestTar(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var output bytes.Buffer
	gz := gzip.NewWriter(&output)
	tw := tar.NewWriter(gz)
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		body := files[name]
		if err := tw.WriteHeader(&tar.Header{
			Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body)),
			ModTime: time.Unix(0, 0).UTC(), Format: tar.FormatPAX,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func hermesTestJob(t *testing.T, bundle []byte) model.JobResponse {
	t.Helper()
	sum := sha256.Sum256(bundle)
	digest := hex.EncodeToString(sum[:])
	spec, err := json.Marshal(model.HermesSpec{
		Kind: agentadapter.ExecutorKindHermes, Version: "2026.9.7",
		TargetOS: "linux", TargetArch: "amd64", BundleLayout: model.HermesOCIBundleLayoutV1,
		ImageReference:   "docker.io/nousresearch/hermes-agent:v2026.9.7",
		ImageIndexDigest: "sha256:" + strings.Repeat("a", 64),
		Artifact:         &model.ArtifactRef{SHA256: digest, Size: int64(len(bundle)), URL: "/v1/artifacts/" + digest},
	})
	if err != nil {
		t.Fatal(err)
	}
	return model.JobResponse{
		JobID: "hermes-job", ResourceKind: agentadapter.ExecutorKindHermes, ResourceID: "hermes-agent",
		Revision: 1, Spec: spec, ArtifactDigest: "sha256:" + digest, ExecutionTimeout: 30,
	}
}

func hermesExecutorFixture(t *testing.T, bundle *[]byte, downloads *int,
	state *hermesExecutorState,
) hermesExecutor {
	t.Helper()
	root := t.TempDir()
	home := "/home/hermes-test"
	e := hermesExecutor{
		targetOS: "linux", targetArch: "amd64", podmanPath: hermesPodmanPath,
		deps: execDeps{
			home: home, fsRoot: root, hubURL: "https://hub.example", token: "machine-token",
			now:   func() time.Time { return time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC) },
			sleep: sleepWithContext,
			httpGet: func(_ context.Context, rawURL string) (*http.Response, error) {
				if rawURL == "http://127.0.0.1:18789/health" {
					return &http.Response{StatusCode: http.StatusOK,
						Body: io.NopCloser(strings.NewReader(`{"ok":true}`))}, nil
				}
				(*downloads)++
				if !strings.HasPrefix(rawURL, "https://hub.example/v1/artifacts/") {
					return nil, errors.New("unexpected artifact URL")
				}
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(*bundle))}, nil
			},
			discover: func(context.Context) *model.OpenClawInstall {
				return &model.OpenClawInstall{GatewayArgs: []string{"gateway", "--port", "18789"}}
			},
		},
	}
	for _, logical := range []string{hermesPodmanPath, filepath.Join(home, ".config", "systemd", "user", hermesUnit)} {
		physical := e.deps.fsPath(logical)
		if err := os.MkdirAll(filepath.Dir(physical), 0o755); err != nil {
			t.Fatal(err)
		}
		mode := os.FileMode(0o644)
		if logical == hermesPodmanPath {
			mode = 0o755
		}
		if err := os.WriteFile(physical, []byte("fixture"), mode); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(e.deps.fsPath(filepath.Join(home, ".config", "clawctl")), 0o700); err != nil {
		t.Fatal(err)
	}
	state.systemdActions = nil
	if state.enabled == nil {
		state.enabled = make(map[string]bool)
	}
	e.deps.systemctl = func(_ context.Context, args ...string) (string, string, error) {
		state.systemdActions = append(state.systemdActions, append([]string{}, args...))
		if len(args) >= 4 && args[0] == "--user" && args[1] == "is-active" && args[2] == "--quiet" {
			if state.active[args[3]] {
				return "", "", nil
			}
			return "", "", errors.New("inactive")
		}
		if len(args) >= 4 && args[0] == "--user" && args[1] == "is-enabled" && args[2] == "--quiet" {
			if state.enabled[args[3]] {
				return "", "", nil
			}
			return "", "", errors.New("disabled")
		}
		if len(args) == 3 && args[0] == "--user" && args[1] == "is-active" {
			if state.active[args[2]] {
				return "active\n", "", nil
			}
			return "inactive\n", "", errors.New("inactive")
		}
		if len(args) == 3 && args[0] == "--user" && (args[1] == "stop" || args[1] == "start" || args[1] == "restart") {
			state.active[args[2]] = args[1] != "stop"
			if args[2] == hermesUnit && args[1] != "stop" {
				env, err := readHermesEnvironment(e.deps, e.hermesEnvPath(e.deps))
				if err != nil {
					return "", "", err
				}
				state.image = strings.TrimPrefix(strings.Split(env, "\n")[0], "HERMES_IMAGE=")
			}
			return "", "", nil
		}
		if len(args) == 3 && args[0] == "--user" && (args[1] == "enable" || args[1] == "disable") {
			state.enabled[args[2]] = args[1] == "enable"
			return "", "", nil
		}
		if len(args) == 2 && args[0] == "--user" && args[1] == "daemon-reload" {
			return "", "", nil
		}
		return "", "", errors.New("unexpected systemctl")
	}
	e.deps.run = func(_ context.Context, name string, args ...string) (string, string, error) {
		command := append([]string{name}, args...)
		state.commands = append(state.commands, command)
		if name == "systemd-run" {
			separator := slices.Index(args, "--")
			if separator < 0 || len(args) <= separator+2 || args[separator+1] != hermesPodmanPath {
				return "", "", errors.New("unexpected transient service")
			}
			podmanArgs := args[separator+2:]
			switch {
			case len(podmanArgs) >= 1 && podmanArgs[0] == "load":
				state.loaded = true
				return "Loaded image\n", "", nil
			case len(podmanArgs) >= 2 && podmanArgs[0] == "image" && podmanArgs[1] == "exists":
				if state.loaded {
					return "", "", nil
				}
				return "", "", errors.New("image missing")
			case len(podmanArgs) >= 2 && podmanArgs[0] == "container" && podmanArgs[1] == "inspect":
				if state.active[hermesUnit] {
					if state.badContainer {
						return "true|docker.io/nousresearch/hermes-agent:v0.0.1\n", "", nil
					}
					return "true|" + state.image + "\n", "", nil
				}
				return "", "", errors.New("container missing")
			}
		}
		return "", "", errors.New("unexpected command")
	}
	return e
}

func TestHermesExecutorLoadsActivatesVerifiesAndReusesImage(t *testing.T) {
	reference := "docker.io/nousresearch/hermes-agent:v2026.9.7"
	bundle := hermesTestBundle(t, reference)
	downloads := 0
	state := &hermesExecutorState{
		active: map[string]bool{openClawUnit: true}, enabled: map[string]bool{openClawUnit: true},
	}
	executor := hermesExecutorFixture(t, &bundle, &downloads, state)
	job := hermesTestJob(t, bundle)
	for attempt := 0; attempt < 2; attempt++ {
		checks, err := executor.Run(t.Context(), job)
		if err != nil || !allPassed(checks) || len(checks) < 3 {
			t.Fatalf("attempt=%d checks=%+v err=%v", attempt, checks, err)
		}
	}
	if downloads != 1 || !state.loaded || !state.active[hermesUnit] || !state.enabled[hermesUnit] ||
		state.active[openClawUnit] || state.enabled[openClawUnit] ||
		state.image != reference {
		t.Fatalf("downloads=%d state=%+v", downloads, state)
	}
	loadFound := false
	for _, command := range state.commands {
		if len(command) > 8 && command[0] == "systemd-run" &&
			slices.Contains(command, "--service-type=exec") && !slices.Contains(command, "--scope") {
			loadFound = true
		}
	}
	if !loadFound {
		t.Fatalf("Hermes image load did not run in a user service: %+v", state.commands)
	}
}

func TestHermesExecutorRestoresOpenClawAndEnvironmentAfterVerificationFailure(t *testing.T) {
	reference := "docker.io/nousresearch/hermes-agent:v2026.9.7"
	bundle := hermesTestBundle(t, reference)
	downloads := 0
	state := &hermesExecutorState{
		active: map[string]bool{openClawUnit: true}, enabled: map[string]bool{openClawUnit: true}, badContainer: true,
	}
	executor := hermesExecutorFixture(t, &bundle, &downloads, state)
	checks, err := executor.Run(t.Context(), hermesTestJob(t, bundle))
	if err != nil || allPassed(checks) || len(checks) < 3 || checks[len(checks)-1].RuleID != "hermes-rollback" ||
		!checks[len(checks)-1].Passed {
		t.Fatalf("checks=%+v err=%v", checks, err)
	}
	if state.active[hermesUnit] || state.enabled[hermesUnit] || !state.active[openClawUnit] || !state.enabled[openClawUnit] {
		t.Fatalf("runtime state not restored: %+v", state.active)
	}
	if _, statErr := os.Stat(executor.deps.fsPath(executor.hermesEnvPath(executor.deps))); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("Hermes environment was not removed: %v", statErr)
	}
}

func TestHermesExecutorGateRejectsNoncanonicalContracts(t *testing.T) {
	reference := "docker.io/nousresearch/hermes-agent:v2026.9.7"
	bundle := hermesTestBundle(t, reference)
	downloads := 0
	state := &hermesExecutorState{active: make(map[string]bool)}
	executor := hermesExecutorFixture(t, &bundle, &downloads, state)
	base := hermesTestJob(t, bundle)
	for _, test := range []struct {
		name   string
		mutate func(*model.JobResponse, *model.HermesSpec)
		code   deploy.RejectionCode
	}{
		{name: "resource kind", mutate: func(job *model.JobResponse, _ *model.HermesSpec) { job.ResourceKind = "openclaw" }, code: deploy.PreconditionFailed},
		{name: "resource id", mutate: func(job *model.JobResponse, _ *model.HermesSpec) { job.ResourceID = "hermes" }, code: deploy.PreconditionFailed},
		{name: "platform", mutate: func(_ *model.JobResponse, spec *model.HermesSpec) { spec.TargetArch = "arm64" }, code: deploy.PreconditionFailed},
		{name: "reference", mutate: func(_ *model.JobResponse, spec *model.HermesSpec) { spec.ImageReference += "-other" }, code: deploy.PreconditionFailed},
		{name: "index digest", mutate: func(_ *model.JobResponse, spec *model.HermesSpec) { spec.ImageIndexDigest = "sha256:BAD" }, code: deploy.PreconditionFailed},
		{name: "artifact URL", mutate: func(_ *model.JobResponse, spec *model.HermesSpec) { spec.Artifact.URL = "https://example.com/image" }, code: deploy.PreconditionFailed},
		{name: "job digest", mutate: func(job *model.JobResponse, _ *model.HermesSpec) {
			job.ArtifactDigest = "sha256:" + strings.Repeat("b", 64)
		}, code: deploy.ArtifactHashMismatch},
	} {
		t.Run(test.name, func(t *testing.T) {
			job := base
			var spec model.HermesSpec
			if err := json.Unmarshal(job.Spec, &spec); err != nil {
				t.Fatal(err)
			}
			test.mutate(&job, &spec)
			job.Spec, _ = json.Marshal(spec)
			checks, err := executor.Run(t.Context(), job)
			var rejection *rejectError
			if len(checks) != 0 || !errors.As(err, &rejection) || rejection.Code != test.code {
				t.Fatalf("checks=%+v err=%v", checks, err)
			}
		})
	}
	if downloads != 0 {
		t.Fatalf("invalid contracts downloaded %d artifacts", downloads)
	}
}

func TestValidateHermesOCIBundleRejectsWrongReferenceAndUnreferencedBlob(t *testing.T) {
	reference := "docker.io/nousresearch/hermes-agent:v2026.9.7"
	bundle := hermesTestBundle(t, reference)
	path := filepath.Join(t.TempDir(), "bundle.tgz")
	if err := os.WriteFile(path, bundle, 0o600); err != nil {
		t.Fatal(err)
	}
	if digest, err := validateHermesOCIBundle(path, "linux", "amd64", reference); err != nil || !validHermesDigest(digest) {
		t.Fatalf("digest=%q err=%v", digest, err)
	}
	if _, err := validateHermesOCIBundle(path, "linux", "amd64", reference+"-other"); err == nil {
		t.Fatal("wrong image reference was accepted")
	}

	extra := []byte("unreferenced")
	sum := sha256.Sum256(extra)
	files := map[string][]byte{"oci-layout": []byte("{\"imageLayoutVersion\":\"1.0.0\"}\n"),
		"index.json": []byte("{}\n"), "blobs/sha256/" + hex.EncodeToString(sum[:]): extra}
	if err := os.WriteFile(path, hermesTestTar(t, files), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := validateHermesOCIBundle(path, "linux", "amd64", reference); err == nil {
		t.Fatal("unreferenced OCI blob was accepted")
	}
}
