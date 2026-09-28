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
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/agentadapter"
	"github.com/teddashh/AI-Intune/internal/deploy"
	"github.com/teddashh/AI-Intune/internal/model"
)

type nodeBundleEntry struct {
	name     string
	typeflag byte
	mode     int64
	linkname string
	body     []byte
}

func nodeRuntimeBundle(t *testing.T, version string, extra ...nodeBundleEntry) []byte {
	t.Helper()
	node := []byte("#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then echo v" + version + "; else echo 11.7.0; fi\n")
	entries := []nodeBundleEntry{
		{name: "node-runtime/", typeflag: tar.TypeDir, mode: 0o755},
		{name: "node-runtime/linux-amd64/", typeflag: tar.TypeDir, mode: 0o755},
		{name: "node-runtime/linux-amd64/bin/node", typeflag: tar.TypeReg, mode: 0o755, body: node},
		{name: "node-runtime/linux-amd64/bin/npm", typeflag: tar.TypeSymlink, mode: 0o777,
			linkname: "../lib/node_modules/npm/bin/npm-cli.js"},
		{name: "node-runtime/linux-amd64/lib/node_modules/npm/bin/npm-cli.js", typeflag: tar.TypeReg, mode: 0o644, body: []byte("npm")},
		{name: "node-runtime/linux-arm64/", typeflag: tar.TypeDir, mode: 0o755},
		{name: "node-runtime/linux-arm64/not-selected", typeflag: tar.TypeReg, mode: 0o644, body: []byte("arm64")},
	}
	entries = append(entries, extra...)
	return writeNodeRuntimeBundle(t, entries)
}

func writeNodeRuntimeBundle(t *testing.T, entries []nodeBundleEntry) []byte {
	t.Helper()
	var output bytes.Buffer
	gz := gzip.NewWriter(&output)
	tw := tar.NewWriter(gz)
	for _, entry := range entries {
		header := &tar.Header{
			Name: entry.name, Typeflag: entry.typeflag, Mode: entry.mode,
			Linkname: entry.linkname, Size: int64(len(entry.body)),
		}
		if entry.typeflag != tar.TypeReg && entry.typeflag != tar.TypeRegA {
			header.Size = 0
		}
		if err := tw.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if len(entry.body) > 0 {
			if _, err := tw.Write(entry.body); err != nil {
				t.Fatal(err)
			}
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

func nodeRuntimeJob(t *testing.T, id, version string, bundle []byte) model.JobResponse {
	t.Helper()
	digest := sha256.Sum256(bundle)
	hexDigest := hex.EncodeToString(digest[:])
	spec, err := json.Marshal(model.NodeRuntimeSpec{
		Kind: agentadapter.ExecutorKindNodeRuntime, Version: version,
		TargetOS: "linux", TargetArch: "amd64", BundleLayout: model.NodeRuntimeBundleLayoutV1,
		Artifact: &model.ArtifactRef{
			SHA256: hexDigest, Size: int64(len(bundle)), URL: "/v1/artifacts/" + hexDigest,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return model.JobResponse{
		JobID: id, ResourceKind: agentadapter.ExecutorKindNodeRuntime, ResourceID: "node-runtime",
		Revision: 1, Spec: spec, ArtifactDigest: "sha256:" + hexDigest, ExecutionTimeout: 30,
	}
}

func nodeRuntimeFixture(t *testing.T, bundle *[]byte, downloads *int) nodeRuntimeExecutor {
	t.Helper()
	root := t.TempDir()
	return nodeRuntimeExecutor{
		targetOS: "linux", targetArch: "amd64",
		deps: execDeps{
			home: "/home/node-test", fsRoot: root, hubURL: "https://hub.example", token: "machine-token",
			now: func() time.Time { return time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC) },
			httpGet: func(_ context.Context, rawURL string) (*http.Response, error) {
				*downloads++
				if !strings.HasPrefix(rawURL, "https://hub.example/v1/artifacts/") {
					return nil, errors.New("unexpected artifact URL")
				}
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(*bundle))}, nil
			},
		},
	}
}

func nodeRuntimeReleasePath(e nodeRuntimeExecutor, version string) string {
	logical := filepath.Join(e.deps.home, ".local", "share", "clawctl", "node-runtime", "releases", version)
	return e.deps.fsPath(logical)
}

func TestNodeRuntimeExecutorStagesActivatesVerifiesAndReusesExactRelease(t *testing.T) {
	bundle := nodeRuntimeBundle(t, "24.15.0")
	downloads := 0
	executor := nodeRuntimeFixture(t, &bundle, &downloads)
	job := nodeRuntimeJob(t, "node-job-1", "24.15.0", bundle)
	for attempt := 1; attempt <= 2; attempt++ {
		verifications, err := executor.Run(t.Context(), job)
		if err != nil || len(verifications) != 3 || !allPassed(verifications) {
			t.Fatalf("attempt=%d verifications=%+v err=%v", attempt, verifications, err)
		}
	}
	if downloads != 1 {
		t.Fatalf("downloads=%d want=1", downloads)
	}
	release := nodeRuntimeReleasePath(executor, "24.15.0")
	if info, err := os.Stat(filepath.Join(release, "bin", "node")); err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("node info=%v err=%v", info, err)
	}
	if _, err := os.Stat(filepath.Join(release, "not-selected")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("non-target platform was extracted: %v", err)
	}
	current := executor.deps.fsPath(filepath.Join(executor.deps.home, ".local", "share", "clawctl", "node-runtime", "current"))
	if target, err := os.Readlink(current); err != nil || target != filepath.Join("releases", "24.15.0") {
		t.Fatalf("current=%q err=%v", target, err)
	}
}

func TestNodeRuntimeExecutorDoesNotReuseSameVersionFromAnotherArtifact(t *testing.T) {
	bundle := nodeRuntimeBundle(t, "24.15.0")
	downloads := 0
	executor := nodeRuntimeFixture(t, &bundle, &downloads)
	first := nodeRuntimeJob(t, "node-job-first-artifact", "24.15.0", bundle)
	if verifications, err := executor.Run(t.Context(), first); err != nil || !allPassed(verifications) {
		t.Fatalf("first artifact verifications=%+v err=%v", verifications, err)
	}

	bundle = nodeRuntimeBundle(t, "24.15.0", nodeBundleEntry{
		name: "node-runtime/linux-amd64/second-artifact", typeflag: tar.TypeReg,
		mode: 0o644, body: []byte("different immutable bytes"),
	})
	second := nodeRuntimeJob(t, "node-job-second-artifact", "24.15.0", bundle)
	verifications, err := executor.Run(t.Context(), second)
	if err != nil || !allPassed(verifications) {
		t.Fatalf("second artifact verifications=%+v err=%v", verifications, err)
	}
	if downloads != 2 {
		t.Fatalf("downloads=%d want=2; same version must not bypass the second artifact", downloads)
	}
	if body, readErr := os.ReadFile(filepath.Join(nodeRuntimeReleasePath(executor, "24.15.0"), "second-artifact")); readErr != nil || string(body) != "different immutable bytes" {
		t.Fatalf("second artifact body=%q err=%v", body, readErr)
	}
}

func TestNodeRuntimeExecutorStopsDownloadAtDeclaredSize(t *testing.T) {
	bundle := nodeRuntimeBundle(t, "24.15.0")
	job := nodeRuntimeJob(t, "node-job-size", "24.15.0", bundle)
	bundle = append(bundle, bytes.Repeat([]byte("x"), 4096)...)
	downloads := 0
	executor := nodeRuntimeFixture(t, &bundle, &downloads)
	verifications, err := executor.Run(t.Context(), job)
	var rejection *rejectError
	if len(verifications) != 0 || !errors.As(err, &rejection) || rejection.Code != deploy.ArtifactHashMismatch {
		t.Fatalf("verifications=%+v error=%v", verifications, err)
	}
	if downloads != 1 {
		t.Fatalf("downloads=%d want=1", downloads)
	}
	if _, statErr := os.Stat(nodeRuntimeReleasePath(executor, "24.15.0")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("oversized release was published: %v", statErr)
	}
}

func TestNodeRuntimeExecutorRestoresPreviousActivationAfterPostSwitchFailure(t *testing.T) {
	bundle := nodeRuntimeBundle(t, "24.14.0")
	downloads := 0
	executor := nodeRuntimeFixture(t, &bundle, &downloads)
	first := nodeRuntimeJob(t, "node-job-old", "24.14.0", bundle)
	if verifications, err := executor.Run(t.Context(), first); err != nil || !allPassed(verifications) {
		t.Fatalf("first=%+v err=%v", verifications, err)
	}

	bundle = nodeRuntimeBundle(t, "24.16.0")
	second := nodeRuntimeJob(t, "node-job-new", "24.16.0", bundle)
	executor.deps.run = func(_ context.Context, name string, args ...string) (string, string, error) {
		if len(args) == 1 && args[0] == "--version" {
			if strings.Contains(name, filepath.Join("releases", "24.16.0")) {
				return "v0.0.0\n", "", nil
			}
			return "v24.16.0\n", "", nil
		}
		return "11.7.0\n", "", nil
	}
	verifications, err := executor.Run(t.Context(), second)
	if err != nil || len(verifications) != 3 || !verifications[0].Passed || verifications[1].Passed ||
		!verifications[2].Passed || verifications[2].RuleID != "node-runtime-rollback" {
		t.Fatalf("verifications=%+v err=%v", verifications, err)
	}
	current := executor.deps.fsPath(filepath.Join(executor.deps.home, ".local", "share", "clawctl", "node-runtime", "current"))
	if target, err := os.Readlink(current); err != nil || target != filepath.Join("releases", "24.14.0") {
		t.Fatalf("current=%q err=%v", target, err)
	}
}

func TestNodeRuntimeBundleExtractionRejectsUnsafeOrIncompleteArchives(t *testing.T) {
	base := []nodeBundleEntry{
		{name: "node-runtime/", typeflag: tar.TypeDir, mode: 0o755},
		{name: "node-runtime/linux-amd64/", typeflag: tar.TypeDir, mode: 0o755},
	}
	for _, test := range []struct {
		name    string
		entries []nodeBundleEntry
	}{
		{name: "traversal", entries: append(base, nodeBundleEntry{name: "node-runtime/linux-amd64/../../escape", typeflag: tar.TypeReg, mode: 0o644, body: []byte("x")})},
		{name: "absolute", entries: append(base, nodeBundleEntry{name: "/node-runtime/linux-amd64/file", typeflag: tar.TypeReg, mode: 0o644, body: []byte("x")})},
		{name: "symlink escape", entries: append(base, nodeBundleEntry{name: "node-runtime/linux-amd64/bin/link", typeflag: tar.TypeSymlink, mode: 0o777, linkname: "../../../escape"})},
		{name: "symlink ancestor", entries: append(base,
			nodeBundleEntry{name: "node-runtime/linux-amd64/lib", typeflag: tar.TypeSymlink, mode: 0o777, linkname: "real"},
			nodeBundleEntry{name: "node-runtime/linux-amd64/lib/file", typeflag: tar.TypeReg, mode: 0o644, body: []byte("x")})},
		{name: "device", entries: append(base, nodeBundleEntry{name: "node-runtime/linux-amd64/device", typeflag: tar.TypeChar, mode: 0o600})},
		{name: "hardlink", entries: append(base,
			nodeBundleEntry{name: "node-runtime/linux-amd64/file", typeflag: tar.TypeReg, mode: 0o644, body: []byte("x")},
			nodeBundleEntry{name: "node-runtime/linux-amd64/hard", typeflag: tar.TypeLink, mode: 0o644,
				linkname: "node-runtime/linux-amd64/file"})},
		{name: "duplicate", entries: append(base,
			nodeBundleEntry{name: "node-runtime/linux-amd64/file", typeflag: tar.TypeReg, mode: 0o644, body: []byte("a")},
			nodeBundleEntry{name: "node-runtime/linux-amd64/file", typeflag: tar.TypeReg, mode: 0o644, body: []byte("b")})},
		{name: "unknown platform", entries: append(base, nodeBundleEntry{name: "node-runtime/linux-riscv64/file", typeflag: tar.TypeReg, mode: 0o644, body: []byte("x")})},
		{name: "target absent", entries: []nodeBundleEntry{
			{name: "node-runtime/", typeflag: tar.TypeDir, mode: 0o755},
			{name: "node-runtime/linux-arm64/", typeflag: tar.TypeDir, mode: 0o755},
			{name: "node-runtime/linux-arm64/file", typeflag: tar.TypeReg, mode: 0o644, body: []byte("x")},
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			bundle := filepath.Join(root, "bundle.tgz")
			if err := os.WriteFile(bundle, writeNodeRuntimeBundle(t, test.entries), 0o600); err != nil {
				t.Fatal(err)
			}
			destination := filepath.Join(root, "out")
			if err := extractNodeRuntimeBundle(bundle, destination, "linux", "amd64"); err == nil {
				t.Fatal("unsafe bundle was accepted")
			}
			if _, err := os.Stat(filepath.Join(root, "escape")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("archive escaped destination: %v", err)
			}
		})
	}
}

func TestNodeRuntimeExecutorGateBindsPlatformLayoutArtifactAndJobIdentity(t *testing.T) {
	bundle := nodeRuntimeBundle(t, "24.15.0")
	job := nodeRuntimeJob(t, "node-gate", "24.15.0", bundle)
	executor := nodeRuntimeExecutor{targetOS: "linux", targetArch: "amd64"}
	if _, err := executor.gate(job); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		mutate func(*model.JobResponse, *model.NodeRuntimeSpec)
		code   deploy.RejectionCode
	}{
		{name: "resource kind", mutate: func(job *model.JobResponse, _ *model.NodeRuntimeSpec) { job.ResourceKind = "openclaw" }, code: deploy.PreconditionFailed},
		{name: "resource id", mutate: func(job *model.JobResponse, _ *model.NodeRuntimeSpec) { job.ResourceID = "node" }, code: deploy.PreconditionFailed},
		{name: "version", mutate: func(_ *model.JobResponse, spec *model.NodeRuntimeSpec) { spec.Version = "024.15.0" }, code: deploy.PreconditionFailed},
		{name: "target", mutate: func(_ *model.JobResponse, spec *model.NodeRuntimeSpec) { spec.TargetArch = "arm64" }, code: deploy.PreconditionFailed},
		{name: "layout", mutate: func(_ *model.JobResponse, spec *model.NodeRuntimeSpec) { spec.BundleLayout = "v2" }, code: deploy.PreconditionFailed},
		{name: "url", mutate: func(_ *model.JobResponse, spec *model.NodeRuntimeSpec) {
			spec.Artifact.URL = "https://elsewhere/artifact"
		}, code: deploy.PreconditionFailed},
		{name: "job digest", mutate: func(job *model.JobResponse, _ *model.NodeRuntimeSpec) {
			job.ArtifactDigest = "sha256:" + strings.Repeat("0", 64)
		}, code: deploy.ArtifactHashMismatch},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate := job
			var spec model.NodeRuntimeSpec
			if err := json.Unmarshal(job.Spec, &spec); err != nil {
				t.Fatal(err)
			}
			artifact := *spec.Artifact
			spec.Artifact = &artifact
			test.mutate(&candidate, &spec)
			candidate.Spec, _ = json.Marshal(spec)
			_, err := executor.gate(candidate)
			var rejection *rejectError
			if !errors.As(err, &rejection) || rejection.Code != test.code {
				t.Fatalf("error=%v", err)
			}
		})
	}
	unknown := job
	unknown.Spec = append([]byte(strings.TrimSuffix(string(job.Spec), "}")), []byte(`,"unknown":true}`)...)
	if _, err := executor.gate(unknown); err == nil {
		t.Fatal("unknown spec field was accepted")
	}
}

func TestNodeRuntimeExecutorAcceptsAndExtractsDarwinContract(t *testing.T) {
	entries := []nodeBundleEntry{
		{name: "node-runtime/", typeflag: tar.TypeDir, mode: 0o755},
		{name: "node-runtime/darwin-arm64/", typeflag: tar.TypeDir, mode: 0o755},
		{name: "node-runtime/darwin-arm64/bin/node", typeflag: tar.TypeReg, mode: 0o755, body: []byte("darwin node")},
	}
	bundle := writeNodeRuntimeBundle(t, entries)
	job := nodeRuntimeJob(t, "node-darwin", "24.15.0", bundle)
	var spec model.NodeRuntimeSpec
	if err := json.Unmarshal(job.Spec, &spec); err != nil {
		t.Fatal(err)
	}
	spec.TargetOS, spec.TargetArch = "darwin", "arm64"
	job.Spec, _ = json.Marshal(spec)
	executor := nodeRuntimeExecutor{targetOS: "darwin", targetArch: "arm64"}
	if _, err := executor.gate(job); err != nil {
		t.Fatalf("Darwin executor contract rejected: %v", err)
	}
	destination := filepath.Join(t.TempDir(), "payload")
	bundlePath := filepath.Join(t.TempDir(), "bundle.tgz")
	if err := os.WriteFile(bundlePath, bundle, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := extractNodeRuntimeBundle(bundlePath, destination, "darwin", "arm64"); err != nil {
		t.Fatalf("Darwin bundle rejected: %v", err)
	}
	if info, err := os.Stat(filepath.Join(destination, "bin", "node")); err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("Darwin node info=%v err=%v", info, err)
	}
	linuxOnlyPath := filepath.Join(t.TempDir(), "linux-only.tgz")
	if err := os.WriteFile(linuxOnlyPath, nodeRuntimeBundle(t, "24.15.0"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := extractNodeRuntimeBundle(linuxOnlyPath, filepath.Join(t.TempDir(), "linux-only"), "darwin", "arm64"); err == nil {
		t.Fatal("Linux-only bundle was accepted as a Darwin artifact")
	}
}

func TestNodeRuntimeRetentionKeepsCurrentAndOnePreviousRelease(t *testing.T) {
	bundle := nodeRuntimeBundle(t, "24.13.0")
	downloads := 0
	executor := nodeRuntimeFixture(t, &bundle, &downloads)
	var currentJob model.JobResponse
	for index, version := range []string{"24.13.0", "24.14.0", "24.15.0"} {
		bundle = nodeRuntimeBundle(t, version)
		currentJob = nodeRuntimeJob(t, "node-retain-"+version, version, bundle)
		currentJob.Revision = int64(index + 1)
		verifications, err := executor.Run(t.Context(), currentJob)
		if err != nil || !allPassed(verifications) {
			t.Fatalf("version=%s verifications=%+v err=%v", version, verifications, err)
		}
	}
	if reports := executor.AfterSucceeded(t.Context(), currentJob); len(reports) != 0 {
		t.Fatalf("retention reports=%v", reports)
	}
	if _, err := os.Stat(nodeRuntimeReleasePath(executor, "24.13.0")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("oldest release still exists: %v", err)
	}
	for _, version := range []string{"24.14.0", "24.15.0"} {
		if info, err := os.Stat(nodeRuntimeReleasePath(executor, version)); err != nil || !info.IsDir() {
			t.Fatalf("retained %s info=%v err=%v", version, info, err)
		}
	}
}
