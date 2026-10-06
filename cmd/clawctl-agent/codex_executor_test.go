package main

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/agentadapter"
	"github.com/teddashh/AI-Intune/internal/artifact"
	"github.com/teddashh/AI-Intune/internal/model"
)

func TestCodexExecutorActivatesAndReusesExactRelease(t *testing.T) {
	const version = "0.155.1"
	bundle := codexExecutorFixtureBundle(t, version)
	downloads := 0
	executor := codexExecutor{
		targetOS: "linux", targetArch: "amd64",
		deps: execDeps{
			home: "/home/codex-test", fsRoot: t.TempDir(), hubURL: "https://hub.example", token: "machine-token",
			now: func() time.Time { return time.Date(2026, 9, 21, 16, 0, 0, 0, time.UTC) },
			httpGet: func(_ context.Context, rawURL string) (*http.Response, error) {
				downloads++
				if !strings.HasPrefix(rawURL, "https://hub.example/v1/artifacts/") {
					return nil, errors.New("unexpected artifact URL")
				}
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(bundle))}, nil
			},
		},
	}
	digest := sha256.Sum256(bundle)
	hexDigest := hex.EncodeToString(digest[:])
	spec, err := json.Marshal(model.CodexSpec{
		Kind: agentadapter.ExecutorKindCodex, Version: version,
		TargetOS: "linux", TargetArch: "amd64", BundleLayout: model.CodexBundleLayoutV1,
		Artifact: &model.ArtifactRef{SHA256: hexDigest, Size: int64(len(bundle)), URL: "/v1/artifacts/" + hexDigest},
	})
	if err != nil {
		t.Fatal(err)
	}
	job := model.JobResponse{
		JobID: "codex-job", ResourceKind: agentadapter.ExecutorKindCodex, ResourceID: "codex",
		Revision: 1, Spec: spec, ArtifactDigest: "sha256:" + hexDigest, ExecutionTimeout: 30,
	}
	rows, err := executor.Run(t.Context(), job)
	if err != nil || len(rows) != 2 || !allPassed(rows) || downloads != 1 {
		t.Fatalf("activate rows=%+v downloads=%d err=%v", rows, downloads, err)
	}
	if !strings.Contains(rows[1].Command, "/bin/codex --version") || !strings.Contains(rows[1].StdoutExcerpt, version) {
		t.Fatalf("version evidence=%+v", rows[1])
	}
	again, err := executor.Run(t.Context(), job)
	if err != nil || len(again) != 2 || !allPassed(again) || downloads != 1 {
		t.Fatalf("reuse rows=%+v downloads=%d err=%v", again, downloads, err)
	}
	if !strings.HasPrefix(again[0].RuleID, "codex-current") {
		t.Fatalf("reuse rule=%s", again[0].RuleID)
	}
}

func TestCodexExecutorLayoutMatchesArtifactContract(t *testing.T) {
	if codexChecksumAssetName != artifact.CodexChecksumAssetName() {
		t.Fatalf("checksum asset %s", codexChecksumAssetName)
	}
	for _, platform := range [][2]string{
		{"linux", "amd64"}, {"linux", "arm64"},
		{"darwin", "amd64"}, {"darwin", "arm64"},
		{"windows", "amd64"}, {"windows", "arm64"},
	} {
		got, ok := codexPackageFilename(platform[0], platform[1])
		want, wantOK := artifact.CodexPackageFilename(platform[0], platform[1])
		if !ok || !wantOK || got != want || codexCommandRelative(platform[0]) != artifact.CodexCommandRelative(platform[0]) ||
			!reflect.DeepEqual(codexPackageRequiredPaths(platform[0]), artifact.CodexPackageRequiredPaths(platform[0])) {
			t.Fatalf("platform %s/%s executor=%s artifact=%s", platform[0], platform[1], got, want)
		}
	}
}

func TestCodexVersionUsesTheLastToken(t *testing.T) {
	if !codexVersionMatches("codex-cli 0.155.1\n", "0.155.1") {
		t.Fatal("last token was not accepted")
	}
	if codexVersionMatches("0.155.1 trailing\n", "0.155.1") {
		t.Fatal("accepted a version that is not the last token")
	}
}

func codexExecutorFixtureBundle(t *testing.T, version string) []byte {
	t.Helper()
	script := []byte("#!/bin/sh\nif [ \"$1\" = --version ]; then echo codex-cli " + version + "; exit 0; fi\nexit 1\n")
	var packageEntries []nodeBundleEntry
	for _, relative := range artifact.CodexPackageRequiredPaths("linux") {
		body := script
		mode := int64(0o755)
		if relative == "codex-package.json" {
			body = []byte("{\"layout\":\"codex-package\"}\n")
			mode = 0o644
		}
		packageEntries = append(packageEntries, nodeBundleEntry{name: relative, typeflag: tar.TypeReg, mode: mode, body: body})
	}
	pkg := writeNodeRuntimeBundle(t, packageEntries)
	filename, ok := artifact.CodexPackageFilename("linux", "amd64")
	if !ok {
		t.Fatal("missing linux package name")
	}
	sum := sha256.Sum256(pkg)
	sums := hex.EncodeToString(sum[:]) + "  " + filename + "\n"
	return writeNodeRuntimeBundle(t, []nodeBundleEntry{
		{name: "codex/", typeflag: tar.TypeDir, mode: 0o755},
		{name: "codex/linux-amd64/", typeflag: tar.TypeDir, mode: 0o755},
		{name: "codex/" + artifact.CodexChecksumAssetName(), typeflag: tar.TypeReg, mode: 0o644, body: []byte(sums)},
		{name: "codex/linux-amd64/" + filename, typeflag: tar.TypeReg, mode: 0o644, body: pkg},
	})
}
