package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/teddashh/AI-Intune/internal/agentadapter"
	"github.com/teddashh/AI-Intune/internal/deploy"
	"github.com/teddashh/AI-Intune/internal/model"
)

func TestAntigravityExecutorGate(t *testing.T) {
	const version = "1.2.14"
	valid := agyJobSpec(t, version, "linux", "amd64", model.AntigravityBundleLayoutV1, agyArtifactRef(strings.Repeat("a", 64), 32))
	digest := "sha256:" + strings.Repeat("a", 64)
	executor := agyExecutor(t, "linux", "amd64", nil)
	cases := []struct {
		name   string
		spec   []byte
		kind   string
		id     string
		digest string
		code   deploy.RejectionCode
		detail string
	}{
		{name: "unknown field", spec: []byte(strings.TrimSuffix(string(valid), "}") + `,"extra":true}`), digest: digest, detail: "不是合法 JSON"},
		{name: "trailing", spec: append(append([]byte{}, valid...), []byte(`{"nope":1}`)...), digest: digest, detail: "尾隨資料"},
		{name: "kind", spec: agyRawSpec(t, "grok", version, "linux", "amd64"), digest: digest, detail: "必須是 antigravity"},
		{name: "id", spec: valid, id: "grok", digest: digest, detail: "identity 不合法"},
		{name: "target", spec: agyJobSpec(t, version, "darwin", "arm64", model.AntigravityBundleLayoutV1, agyArtifactRef(strings.Repeat("a", 64), 32)), digest: digest, detail: "平台不一致"},
		{name: "layout", spec: agyJobSpec(t, version, "linux", "amd64", "grok-bundle:v1", agyArtifactRef(strings.Repeat("a", 64), 32)), digest: digest, detail: "bundle contract 不合法"},
		{name: "sha", spec: agyJobSpec(t, version, "linux", "amd64", model.AntigravityBundleLayoutV1, agyArtifactRef(strings.Repeat("A", 64), 32)), digest: "sha256:" + strings.Repeat("A", 64), detail: "小寫十六進位"},
		{name: "size", spec: agyJobSpec(t, version, "linux", "amd64", model.AntigravityBundleLayoutV1, agyArtifactRef(strings.Repeat("a", 64), 0)), digest: digest, detail: "artifact.size"},
		{name: "url", spec: agyJobSpec(t, version, "linux", "amd64", model.AntigravityBundleLayoutV1, &model.ArtifactRef{SHA256: strings.Repeat("a", 64), Size: 32, URL: "/v1/artifacts/nope"}), digest: digest, detail: "artifact contract 不合法"},
		{name: "digest", spec: valid, digest: "sha256:" + strings.Repeat("b", 64), code: deploy.ArtifactHashMismatch, detail: "digest 與 spec 不一致"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kind, id := tc.kind, tc.id
			if kind == "" {
				kind = agentadapter.ExecutorKindAntigravity
			}
			if id == "" {
				id = "antigravity"
			}
			_, err := executor.Run(t.Context(), model.JobResponse{
				JobID: "agy-gate", ResourceKind: kind, ResourceID: id, Spec: tc.spec, ArtifactDigest: tc.digest,
			})
			var rejected *rejectError
			if !errors.As(err, &rejected) || !strings.Contains(rejected.Detail, tc.detail) {
				t.Fatalf("err=%v", err)
			}
			if tc.code != "" && rejected.Code != tc.code {
				t.Fatalf("code=%s", rejected.Code)
			}
		})
	}
}

func TestAntigravityExecutorInstallsReadOnlyReleaseAndReusesIt(t *testing.T) {
	const version = "1.2.14"
	t.Setenv("AGY_CLI_DISABLE_AUTO_UPDATE", "1")
	script := agyVersionScript(version, true)
	bundle := agyExecutorBundle(t, "linux", "amd64", "cli_linux_x64.tar.gz", "linux-x64", version, agyInnerTar(t, []tarEntry{{name: "antigravity", body: script}}))
	executor, job := agyRun(t, "agy-job", "linux", "amd64", version, bundle)
	probe := defaultAntigravityExecutor("https://hub.example", "machine-token")
	if !agySameRunner(executor.deps.antigravityVersion, probe.deps.antigravityVersion) || !agySameRunner(executor.deps.antigravityVersion, runAntigravityVersion) {
		t.Fatal("test did not keep the production version runner")
	}
	rows, err := executor.Run(t.Context(), job)
	if err != nil || len(rows) != 2 || !allPassed(rows) {
		t.Fatalf("activate rows=%+v err=%v", rows, err)
	}
	if !strings.HasPrefix(rows[0].RuleID, "antigravity-activate-artifact") || !strings.HasPrefix(rows[1].RuleID, "antigravity-activate-version") {
		t.Fatalf("rules=%s %s", rows[0].RuleID, rows[1].RuleID)
	}
	if strings.Contains(rows[1].Command, "AGY_CLI") || !strings.HasSuffix(rows[1].Command, "/bin/agy --version") {
		t.Fatalf("command=%s", rows[1].Command)
	}
	root := filepath.Join(executor.deps.home, ".local", "share", "clawctl", "antigravity")
	release := executor.deps.fsPath(filepath.Join(root, "releases", version))
	binary := filepath.Join(release, "bin", "agy")
	info, err := os.Lstat(binary)
	binInfo, binErr := os.Lstat(filepath.Dir(binary))
	relInfo, relErr := os.Lstat(release)
	if err != nil || binErr != nil || relErr != nil || info.Mode().Perm() != 0o555 || binInfo.Mode().Perm() != 0o555 || relInfo.Mode().Perm()&0o200 == 0 {
		t.Fatalf("modes file=%v bin=%v release=%v err=%v %v %v", info, binInfo, relInfo, err, binErr, relErr)
	}
	marker, err := os.ReadFile(filepath.Join(release, ".clawctl-artifact-sha256"))
	if err != nil || string(marker) != "sha256:"+jobSHA(bundle)+"\n" {
		t.Fatalf("marker=%q err=%v", marker, err)
	}
	target, err := os.Readlink(executor.deps.fsPath(filepath.Join(root, "current")))
	if err != nil || target != filepath.Join("releases", version) {
		t.Fatalf("current=%q err=%v", target, err)
	}
	again, err := executor.Run(t.Context(), job)
	if err != nil || len(again) != 2 || !allPassed(again) || !strings.HasPrefix(again[0].RuleID, "antigravity-current") {
		t.Fatalf("reuse rows=%+v err=%v", again, err)
	}
	nextScript := agyVersionScript("1.2.15", true)
	nextBundle := agyExecutorBundle(t, "linux", "amd64", "cli_linux_x64.tar.gz", "linux-x64", "1.2.15", agyInnerTar(t, []tarEntry{{name: "antigravity", body: nextScript}}))
	executor, nextJob := agyRunOn(t, executor, "agy-next", "1.2.15", nextBundle)
	nextRows, err := executor.Run(t.Context(), nextJob)
	if err != nil || len(nextRows) != 2 || !allPassed(nextRows) || !strings.HasPrefix(nextRows[0].RuleID, "antigravity-activate") {
		t.Fatalf("next rows=%+v err=%v", nextRows, err)
	}
	nextTarget, err := os.Readlink(executor.deps.fsPath(filepath.Join(root, "current")))
	nextInfo, statErr := os.Lstat(executor.deps.fsPath(filepath.Join(root, "releases", "1.2.15", "bin", "agy")))
	if err != nil || statErr != nil || nextTarget != filepath.Join("releases", "1.2.15") || nextInfo.Mode().Perm() != 0o555 {
		t.Fatalf("next current=%q mode=%v err=%v %v", nextTarget, nextInfo, err, statErr)
	}
}

func TestAntigravityExecutorRollsBackWrongVersion(t *testing.T) {
	const current = "1.2.14"
	const next = "1.2.15"
	first := agyExecutorBundle(t, "linux", "amd64", "cli_linux_x64.tar.gz", "linux-x64", current, agyInnerTar(t, []tarEntry{{name: "antigravity", body: agyVersionScript(current, false)}}))
	executor, job := agyRun(t, "agy-first", "linux", "amd64", current, first)
	if rows, err := executor.Run(t.Context(), job); err != nil || !allPassed(rows) {
		t.Fatalf("first rows=%+v err=%v", rows, err)
	}
	script := []byte("#!/bin/sh\ncase \"$0\" in\n*releases/" + next + "*) echo 0.0.0 ;;\n*) echo " + next + " ;;\nesac\n")
	second := agyExecutorBundle(t, "linux", "amd64", "cli_linux_x64.tar.gz", "linux-x64", next, agyInnerTar(t, []tarEntry{{name: "antigravity", body: script}}))
	executor, nextJob := agyRunOn(t, executor, "agy-rollback", next, second)
	rows, err := executor.Run(t.Context(), nextJob)
	if err != nil || len(rows) < 2 || allPassed(rows) {
		t.Fatalf("rollback rows=%+v err=%v", rows, err)
	}
	var saw bool
	for _, row := range rows {
		if row.RuleID == "antigravity-rollback" && row.Command == "restore Antigravity current" {
			saw = true
		}
	}
	target, readErr := os.Readlink(executor.deps.fsPath(filepath.Join(executor.deps.home, ".local", "share", "clawctl", "antigravity", "current")))
	if !saw || readErr != nil || target != filepath.Join("releases", current) {
		t.Fatalf("rollback=%t current=%q err=%v rows=%+v", saw, target, readErr, rows)
	}
}

func TestAntigravityExecutorReplacesBrokenRelease(t *testing.T) {
	const version = "1.2.14"
	bundle := agyExecutorBundle(t, "linux", "amd64", "cli_linux_x64.tar.gz", "linux-x64", version,
		agyInnerTar(t, []tarEntry{{name: "antigravity", body: agyVersionScript(version, false)}}))
	executor, job := agyRun(t, "agy-first", "linux", "amd64", version, bundle)
	if rows, err := executor.Run(t.Context(), job); err != nil || !allPassed(rows) {
		t.Fatalf("first rows=%+v err=%v", rows, err)
	}
	release := executor.deps.fsPath(filepath.Join(executor.deps.home, ".local", "share", "clawctl", "antigravity", "releases", version))
	stale := "sha256:" + strings.Repeat("0", 64) + "\n"
	if err := os.WriteFile(filepath.Join(release, ".clawctl-artifact-sha256"), []byte(stale), 0o600); err != nil {
		t.Fatal(err)
	}
	executor, repair := agyRunOn(t, executor, "agy-repair", version, bundle)
	rows, err := executor.Run(t.Context(), repair)
	if err != nil || len(rows) != 2 || !allPassed(rows) || !strings.HasPrefix(rows[0].RuleID, "antigravity-current") {
		t.Fatalf("repair rows=%+v err=%v", rows, err)
	}
	broken := release + ".broken-" + safeJobID("agy-repair")
	kept, err := os.ReadFile(filepath.Join(broken, ".clawctl-artifact-sha256"))
	binInfo, binErr := os.Lstat(filepath.Join(broken, "bin"))
	if err != nil || string(kept) != stale || binErr != nil || binInfo.Mode().Perm() != 0o755 {
		t.Fatalf("broken marker=%q err=%v bin=%v binErr=%v", kept, err, binInfo, binErr)
	}
	marker, err := os.ReadFile(filepath.Join(release, ".clawctl-artifact-sha256"))
	info, statErr := os.Lstat(filepath.Join(release, "bin", "agy"))
	if err != nil || string(marker) != "sha256:"+jobSHA(bundle)+"\n" || statErr != nil || info.Mode().Perm() != 0o555 {
		t.Fatalf("release marker=%q err=%v mode=%v statErr=%v", marker, err, info, statErr)
	}
	if err := os.RemoveAll(broken); err != nil {
		t.Fatalf("broken release cannot be removed: %v", err)
	}
}

func TestAntigravityExecutorDiscardsStagedReleaseReportingWrongVersion(t *testing.T) {
	const version = "1.2.14"
	bundle := agyExecutorBundle(t, "linux", "amd64", "cli_linux_x64.tar.gz", "linux-x64", version,
		agyInnerTar(t, []tarEntry{{name: "antigravity", body: agyVersionScript("0.0.0", false)}}))
	executor, job := agyRun(t, "agy-stage-version", "linux", "amd64", version, bundle)
	rows, err := executor.Run(t.Context(), job)
	if err != nil || len(rows) != 1 || rows[0].Passed || !strings.HasPrefix(rows[0].RuleID, "antigravity-stage-version") {
		t.Fatalf("rows=%+v err=%v", rows, err)
	}
	root := executor.deps.fsPath(filepath.Join(executor.deps.home, ".local", "share", "clawctl", "antigravity"))
	entries, err := os.ReadDir(filepath.Join(root, "releases"))
	if _, currentErr := os.Lstat(filepath.Join(root, "current")); err != nil || len(entries) != 0 || !errors.Is(currentErr, os.ErrNotExist) {
		t.Fatalf("releases=%v err=%v current=%v", entries, err, currentErr)
	}
}

func TestAntigravityExecutorRefusesBundleThatDoesNotMatchPinnedDigest(t *testing.T) {
	const version = "1.2.14"
	bundle := agyExecutorBundle(t, "linux", "amd64", "cli_linux_x64.tar.gz", "linux-x64", version,
		agyInnerTar(t, []tarEntry{{name: "antigravity", body: agyVersionScript(version, false)}}))
	pinned := strings.Repeat("c", 64)
	executor := agyExecutor(t, "linux", "amd64", bundle)
	rows, err := executor.Run(t.Context(), model.JobResponse{
		JobID: "agy-digest", ResourceKind: agentadapter.ExecutorKindAntigravity, ResourceID: "antigravity",
		Spec:           agyJobSpec(t, version, "linux", "amd64", model.AntigravityBundleLayoutV1, agyArtifactRef(pinned, int64(len(bundle)))),
		ArtifactDigest: "sha256:" + pinned,
	})
	var rejected *rejectError
	if !errors.As(err, &rejected) || rejected.Code != deploy.ArtifactHashMismatch || len(rows) != 0 {
		t.Fatalf("rows=%+v err=%v", rows, err)
	}
	releases := executor.deps.fsPath(filepath.Join(executor.deps.home, ".local", "share", "clawctl", "antigravity", "releases"))
	if entries, err := os.ReadDir(releases); err != nil || len(entries) != 0 {
		t.Fatalf("releases=%v err=%v", entries, err)
	}
}

func TestAntigravityExecutorRejectsInnerArchive(t *testing.T) {
	const version = "1.2.14"
	cases := []struct {
		name    string
		entries []tarEntry
		excerpt string
	}{
		{name: "extra", entries: []tarEntry{{name: "antigravity", body: []byte("one")}, {name: "other", body: []byte("two")}}, excerpt: "必須只有一個執行檔"},
		{name: "directory", entries: []tarEntry{{name: "antigravity/", dir: true}}, excerpt: "必須只有一個執行檔"},
		{name: "symlink", entries: []tarEntry{{name: "antigravity", link: "other"}}, excerpt: "必須只有一個執行檔"},
		{name: "name", entries: []tarEntry{{name: "agy", body: []byte("nope")}}, excerpt: "必須只有一個執行檔"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bundle := agyExecutorBundle(t, "linux", "amd64", "cli_linux_x64.tar.gz", "linux-x64", version, agyInnerTar(t, tc.entries))
			executor, job := agyRun(t, "agy-inner", "linux", "amd64", version, bundle)
			rows, err := executor.Run(t.Context(), job)
			agyExpectStage(t, rows, err, tc.excerpt)
			binary := executor.deps.fsPath(filepath.Join(executor.deps.home, ".local", "share", "clawctl", "antigravity", "releases", version, "bin", "agy"))
			if _, statErr := os.Lstat(binary); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("left binary: %v", statErr)
			}
		})
	}
	t.Run("oversize", func(t *testing.T) {
		bundle := agyExecutorBundle(t, "linux", "amd64", "cli_linux_x64.tar.gz", "linux-x64", version, agyOversizeInner(t))
		executor, job := agyRun(t, "agy-oversize", "linux", "amd64", version, bundle)
		rows, err := executor.Run(t.Context(), job)
		agyExpectStage(t, rows, err, "執行檔大小不合法")
	})
	t.Run("manifest sha", func(t *testing.T) {
		inner := agyInnerTar(t, []tarEntry{{name: "antigravity", body: agyVersionScript(version, false)}})
		bundle := agyExecutorBundleSHA(t, "linux", "amd64", "cli_linux_x64.tar.gz", "linux-x64", version, inner, strings.Repeat("ab", 64))
		executor, job := agyRun(t, "agy-sha", "linux", "amd64", version, bundle)
		rows, err := executor.Run(t.Context(), job)
		agyExpectStage(t, rows, err, "manifest 與官方檔不一致")
	})
	t.Run("manifest version", func(t *testing.T) {
		inner := agyInnerTar(t, []tarEntry{{name: "antigravity", body: agyVersionScript(version, false)}})
		bundle := agyExecutorBundleAt(t, "linux", "amd64", "cli_linux_x64.tar.gz", "linux-x64", version, "1.2.15", inner, "")
		executor, job := agyRun(t, "agy-manifest-version", "linux", "amd64", version, bundle)
		rows, err := executor.Run(t.Context(), job)
		agyExpectStage(t, rows, err, "manifest 與官方檔不一致")
	})
}

func TestAntigravityExecutorInstallsWindowsBinary(t *testing.T) {
	const version = "1.2.14"
	script := agyVersionScript(version, false)
	bundle := agyExecutorBundle(t, "windows", "amd64", "cli_windows_x64.exe", "windows-x64", version, script)
	executor, job := agyRun(t, "agy-windows", "windows", "amd64", version, bundle)
	rows, err := executor.Run(t.Context(), job)
	if err != nil || len(rows) != 2 || !allPassed(rows) || !strings.HasSuffix(rows[1].Command, "bin/agy.exe --version") {
		t.Fatalf("rows=%+v err=%v", rows, err)
	}
	binary := executor.deps.fsPath(filepath.Join(executor.deps.home, ".local", "share", "clawctl", "antigravity", "releases", version, "bin", "agy.exe"))
	info, err := os.Lstat(binary)
	body, readErr := os.ReadFile(binary)
	if err != nil || readErr != nil || !info.Mode().IsRegular() || !bytes.Equal(body, script) || info.Mode().Perm() == 0o555 {
		t.Fatalf("windows info=%v err=%v read=%v body=%q", info, err, readErr, body)
	}
}

func agyExpectStage(t *testing.T, rows []model.JobVerificationRequest, err error, excerpt string) {
	t.Helper()
	if err != nil || len(rows) != 1 || rows[0].Passed || rows[0].RuleID != "stage" || !strings.Contains(rows[0].StderrExcerpt, excerpt) {
		t.Fatalf("rows=%+v err=%v", rows, err)
	}
}

func agySameRunner(got, want func(context.Context, string) (string, string, error)) bool {
	return got != nil && want != nil && reflect.ValueOf(got).Pointer() == reflect.ValueOf(want).Pointer()
}

func agyExecutor(t *testing.T, targetOS, targetArch string, bundle []byte) antigravityExecutor {
	t.Helper()
	root := t.TempDir()
	t.Cleanup(func() { _ = agyChmodTree(root) })
	return antigravityExecutor{
		targetOS: targetOS, targetArch: targetArch,
		deps: execDeps{
			home: "/home/agy-test", fsRoot: root, hubURL: "https://hub.example", token: "machine-token",
			antigravityVersion: runAntigravityVersion,
			httpGet: func(context.Context, string) (*http.Response, error) {
				if bundle == nil {
					t.Errorf("gate downloaded an artifact")
					return nil, errors.New("unexpected download")
				}
				return &http.Response{StatusCode: http.StatusOK, ContentLength: int64(len(bundle)), Body: io.NopCloser(bytes.NewReader(bundle))}, nil
			},
		},
	}
}

func agyRun(t *testing.T, jobID, targetOS, targetArch, version string, bundle []byte) (antigravityExecutor, model.JobResponse) {
	t.Helper()
	executor := agyExecutor(t, targetOS, targetArch, bundle)
	return executor, agyJob(jobID, targetOS, targetArch, version, bundle)
}

func agyRunOn(t *testing.T, executor antigravityExecutor, jobID, version string, bundle []byte) (antigravityExecutor, model.JobResponse) {
	t.Helper()
	executor.deps.httpGet = func(context.Context, string) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, ContentLength: int64(len(bundle)), Body: io.NopCloser(bytes.NewReader(bundle))}, nil
	}
	return executor, agyJob(jobID, executor.targetOS, executor.targetArch, version, bundle)
}

func agyJob(jobID, targetOS, targetArch, version string, bundle []byte) model.JobResponse {
	sha := jobSHA(bundle)
	spec := agyJobSpec(nil, version, targetOS, targetArch, model.AntigravityBundleLayoutV1, agyArtifactRef(sha, int64(len(bundle))))
	return model.JobResponse{
		JobID: jobID, ResourceKind: agentadapter.ExecutorKindAntigravity, ResourceID: "antigravity",
		Spec: spec, ArtifactDigest: "sha256:" + sha,
	}
}

func jobSHA(bundle []byte) string {
	sum := sha256.Sum256(bundle)
	return hex.EncodeToString(sum[:])
}

func agyArtifactRef(sha string, size int64) *model.ArtifactRef {
	return &model.ArtifactRef{SHA256: sha, Size: size, URL: "/v1/artifacts/" + sha}
}

func agyJobSpec(t *testing.T, version, targetOS, targetArch, layout string, artifact *model.ArtifactRef) []byte {
	raw, err := json.Marshal(model.AntigravitySpec{
		Kind: "antigravity", Version: version, TargetOS: targetOS, TargetArch: targetArch,
		BundleLayout: layout, Artifact: artifact,
	})
	if err != nil && t != nil {
		t.Fatal(err)
	}
	return raw
}

func agyRawSpec(t *testing.T, kind, version, targetOS, targetArch string) []byte {
	t.Helper()
	raw, err := json.Marshal(model.AntigravitySpec{
		Kind: kind, Version: version, TargetOS: targetOS, TargetArch: targetArch,
		BundleLayout: model.AntigravityBundleLayoutV1, Artifact: agyArtifactRef(strings.Repeat("a", 64), 32),
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func agyVersionScript(version string, requireUpdateOff bool) []byte {
	if !requireUpdateOff {
		return []byte("#!/bin/sh\necho " + version + "\n")
	}
	return []byte("#!/bin/sh\nif [ \"$AGY_CLI_DISABLE_AUTO_UPDATE\" = \"true\" ]; then echo " + version + "; exit 0; fi\nexit 1\n")
}

type tarEntry struct {
	name string
	body []byte
	link string
	dir  bool
}

func agyInnerTar(t *testing.T, entries []tarEntry) []byte {
	t.Helper()
	var output bytes.Buffer
	gz := gzip.NewWriter(&output)
	tw := tar.NewWriter(gz)
	for _, entry := range entries {
		header := &tar.Header{Name: entry.name, Mode: 0o755}
		switch {
		case entry.dir:
			header.Typeflag = tar.TypeDir
			header.Name = strings.TrimSuffix(entry.name, "/") + "/"
		case entry.link != "":
			header.Typeflag = tar.TypeSymlink
			header.Linkname = entry.link
		default:
			header.Typeflag = tar.TypeReg
			header.Size = int64(len(entry.body))
		}
		if err := tw.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if header.Typeflag == tar.TypeReg {
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

func agyOversizeInner(t *testing.T) []byte {
	t.Helper()
	var output bytes.Buffer
	gz := gzip.NewWriter(&output)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{
		Name: "antigravity", Mode: 0o755, Typeflag: tar.TypeReg, Size: maxAntigravityOfficialBytes + 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func agyExecutorBundle(t *testing.T, targetOS, targetArch, file, dir, version string, official []byte) []byte {
	t.Helper()
	return agyExecutorBundleAt(t, targetOS, targetArch, file, dir, version, version, official, "")
}

func agyExecutorBundleSHA(t *testing.T, targetOS, targetArch, file, dir, version string, official []byte, sha string) []byte {
	t.Helper()
	return agyExecutorBundleAt(t, targetOS, targetArch, file, dir, version, version, official, sha)
}

func agyExecutorBundleAt(t *testing.T, targetOS, targetArch, file, dir, urlVersion, manifestVersion string, official []byte, sha string) []byte {
	t.Helper()
	if sha == "" {
		sum := sha512.Sum512(official)
		sha = hex.EncodeToString(sum[:])
	}
	manifest, err := json.Marshal(map[string]string{
		"version": manifestVersion,
		"url":     "https://storage.googleapis.com/antigravity-public/antigravity-cli/" + urlVersion + "-4571742832820224/" + dir + "/" + file,
		"sha512":  sha,
	})
	if err != nil {
		t.Fatal(err)
	}
	base := "antigravity/" + targetOS + "-" + targetArch
	var output bytes.Buffer
	gz := gzip.NewWriter(&output)
	tw := tar.NewWriter(gz)
	for _, name := range []string{"antigravity", base} {
		if err := tw.WriteHeader(&tar.Header{Name: name + "/", Typeflag: tar.TypeDir, Mode: 0o755}); err != nil {
			t.Fatal(err)
		}
	}
	for _, member := range []struct {
		name string
		body []byte
	}{
		{base + "/" + file, official},
		{base + "/manifest.json", manifest},
	} {
		if err := tw.WriteHeader(&tar.Header{Name: member.name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(member.body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(member.body); err != nil {
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

func agyChmodTree(root string) error {
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		_ = os.Chmod(path, 0o755)
		return nil
	})
}
