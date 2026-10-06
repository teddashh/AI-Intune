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
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/agentadapter"
	"github.com/teddashh/AI-Intune/internal/model"
)

func TestClaudeCodeExecutorActivatesAndReusesExactRelease(t *testing.T) {
	const version = "2.1.278"
	body := []byte("#!/bin/sh\nif [ \"$1\" = --version ]; then echo " + version + " '(Claude Code)'; exit 0; fi\nexit 1\n")
	bundle := writeNodeRuntimeBundle(t, []nodeBundleEntry{
		{name: "claude-code/linux-amd64/bin/claude", typeflag: tar.TypeReg, mode: 0o755, body: body},
	})
	downloads := 0
	executor := claudeCodeExecutor{
		targetOS: "linux", targetArch: "amd64",
		deps: execDeps{
			home: "/home/claude-test", fsRoot: t.TempDir(), hubURL: "https://hub.example", token: "machine-token",
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
	spec, err := json.Marshal(model.ClaudeCodeSpec{
		Kind: agentadapter.ExecutorKindClaudeCode, Version: version,
		TargetOS: "linux", TargetArch: "amd64", BundleLayout: model.ClaudeCodeBundleLayoutV1,
		Artifact: &model.ArtifactRef{SHA256: hexDigest, Size: int64(len(bundle)), URL: "/v1/artifacts/" + hexDigest},
	})
	if err != nil {
		t.Fatal(err)
	}
	job := model.JobResponse{
		JobID: "claude-job", ResourceKind: agentadapter.ExecutorKindClaudeCode, ResourceID: "claude-code",
		Revision: 1, Spec: spec, ArtifactDigest: "sha256:" + hexDigest, ExecutionTimeout: 30,
	}
	rows, err := executor.Run(t.Context(), job)
	if err != nil || len(rows) != 2 || !allPassed(rows) || downloads != 1 {
		t.Fatalf("activate rows=%+v downloads=%d err=%v", rows, downloads, err)
	}
	if !strings.Contains(rows[1].Command, "/bin/claude --version") || !strings.Contains(rows[1].StdoutExcerpt, version) {
		t.Fatalf("version evidence=%+v", rows[1])
	}
	again, err := executor.Run(t.Context(), job)
	if err != nil || len(again) != 2 || !allPassed(again) || downloads != 1 {
		t.Fatalf("reuse rows=%+v downloads=%d err=%v", again, downloads, err)
	}
	if !strings.HasPrefix(again[0].RuleID, "claude-code-current") {
		t.Fatalf("reuse rule=%s", again[0].RuleID)
	}
}
