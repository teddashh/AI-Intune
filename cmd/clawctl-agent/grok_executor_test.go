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
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/teddashh/AI-Intune/internal/agentadapter"
	"github.com/teddashh/AI-Intune/internal/artifact"
	"github.com/teddashh/AI-Intune/internal/deploy"
	"github.com/teddashh/AI-Intune/internal/model"
)

func TestGrokExecutorActivatesAndReusesExactRelease(t *testing.T) {
	const version = "1.0.40"
	bundle := grokExecutorBundle(t, map[string][]byte{
		"darwin/arm64": brotliPayload(t, grokVersionScript("9.9.9")),
		"linux/amd64":  brotliPayload(t, grokVersionScript(version)),
	})
	executor, job, downloads := grokTestRun(t, "linux", "amd64", bundle, bundle, int64(len(bundle)))
	rows, err := executor.Run(t.Context(), job)
	if err != nil || len(rows) != 2 || !allPassed(rows) || *downloads != 1 {
		t.Fatalf("activate rows=%+v downloads=%d err=%v", rows, *downloads, err)
	}
	if !strings.Contains(rows[1].Command, "/bin/grok --version") || !strings.Contains(rows[1].StdoutExcerpt, "grok "+version) {
		t.Fatalf("version evidence=%+v", rows[1])
	}
	binary := executor.deps.fsPath(filepath.Join(executor.deps.home, ".local", "share", "clawctl", "grok", "releases", version, "bin", "grok"))
	info, err := os.Lstat(binary)
	if err != nil || !info.Mode().IsRegular() || (runtime.GOOS != "windows" && info.Mode().Perm()&0o111 == 0) {
		t.Fatalf("installed grok mode=%v err=%v", info, err)
	}
	body, err := os.ReadFile(binary)
	if err != nil || !bytes.Contains(body, []byte("grok "+version)) || bytes.Contains(body, []byte("9.9.9")) {
		t.Fatalf("installed bytes err=%v body=%q", err, body)
	}
	current := executor.deps.fsPath(filepath.Join(executor.deps.home, ".local", "share", "clawctl", "grok", "current"))
	currentInfo, err := os.Lstat(current)
	if err != nil || currentInfo.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("current info=%v err=%v", currentInfo, err)
	}
	target, err := os.Readlink(current)
	if err != nil || target != filepath.Join("releases", version) {
		t.Fatalf("current target=%q err=%v", target, err)
	}
	again, err := executor.Run(t.Context(), job)
	if err != nil || len(again) != 2 || !allPassed(again) || *downloads != 1 {
		t.Fatalf("reuse rows=%+v downloads=%d err=%v", again, *downloads, err)
	}
	if !strings.HasPrefix(again[0].RuleID, "grok-current") {
		t.Fatalf("reuse rule=%s", again[0].RuleID)
	}
}

func TestGrokExecutorInstallsWindowsScriptWithoutPEHeader(t *testing.T) {
	const version = "1.0.40"
	script := grokVersionScript(version)
	bundle := grokExecutorBundle(t, map[string][]byte{
		"windows/amd64": brotliPayload(t, script),
	})
	executor, job, downloads := grokTestRun(t, "windows", "amd64", bundle, bundle, int64(len(bundle)))
	rows, err := executor.Run(t.Context(), job)
	if err != nil || len(rows) != 2 || !allPassed(rows) || *downloads != 1 {
		t.Fatalf("rows=%+v downloads=%d err=%v", rows, *downloads, err)
	}
	if !strings.Contains(rows[1].Command, "grok.exe --version") {
		t.Fatalf("version command=%s", rows[1].Command)
	}
	binary := executor.deps.fsPath(filepath.Join(executor.deps.home, ".local", "share", "clawctl", "grok", "releases", version, "bin", "grok.exe"))
	info, err := os.Lstat(binary)
	body, readErr := os.ReadFile(binary)
	if err != nil || readErr != nil || !info.Mode().IsRegular() || !bytes.Equal(body, script) || bytes.HasPrefix(body, []byte("MZ")) {
		t.Fatalf("windows fixture info=%v err=%v read=%v body=%q", info, err, readErr, body)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("windows fixture mode=%o", info.Mode().Perm())
	}
}

func TestGrokExecutorRejectsBundleWithoutLocalMember(t *testing.T) {
	const version = "1.0.40"
	bundle := grokExecutorBundle(t, map[string][]byte{
		"darwin/arm64": brotliPayload(t, grokVersionScript(version)),
	})
	executor, job, _ := grokTestRun(t, "linux", "amd64", bundle, bundle, int64(len(bundle)))
	rows, err := executor.Run(t.Context(), job)
	if err == nil && allPassed(rows) {
		t.Fatal("installed a bundle that has no linux-amd64 member")
	}
}

func TestGrokExecutorRejectsSymlinkAndHardlinkMembers(t *testing.T) {
	body := brotliPayload(t, grokVersionScript("1.0.40"))
	if len(body) == 0 {
		t.Fatal("member body is empty")
	}
	t.Run("regular", func(t *testing.T) {
		bundle := grokSpecialEntryBundle(t, &tar.Header{
			Name:     "grok/linux-amd64/bin/grok.br",
			Typeflag: tar.TypeReg,
			Mode:     0o644,
			Size:     int64(len(body)),
		}, body)
		executor, job, _ := grokTestRun(t, "linux", "amd64", bundle, bundle, int64(len(bundle)))
		rows, err := executor.Run(t.Context(), job)
		if err != nil || len(rows) != 2 || !allPassed(rows) {
			t.Fatalf("regular member rows=%+v err=%v", rows, err)
		}
	})
	for _, tc := range []struct {
		name     string
		typeflag byte
	}{
		{name: "symlink", typeflag: tar.TypeSymlink},
		{name: "hardlink", typeflag: tar.TypeLink},
		{name: "char", typeflag: tar.TypeChar},
		{name: "block", typeflag: tar.TypeBlock},
		{name: "fifo", typeflag: tar.TypeFifo},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bundle := grokNonRegularMemberBundle(t, tc.typeflag, body)
			executor, job, _ := grokTestRun(t, "linux", "amd64", bundle, bundle, int64(len(bundle)))
			rows, err := executor.Run(t.Context(), job)
			binary := executor.deps.fsPath(filepath.Join(executor.deps.home, ".local", "share", "clawctl", "grok", "releases", "1.0.40", "bin", "grok"))
			if _, statErr := os.Lstat(binary); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("typeflag %q left %s: %v", tc.typeflag, binary, statErr)
			}
			if err != nil || len(rows) != 1 || rows[0].Passed || rows[0].RuleID != "stage" ||
				rows[0].StderrExcerpt != "Grok bundle 成員不是 regular file" {
				t.Fatalf("typeflag %q rows=%+v err=%v", tc.typeflag, rows, err)
			}
		})
	}
}

func TestGrokExecutorBoundsDecompressedBinaryWrite(t *testing.T) {
	const inflated = int64(4 << 20)
	previousLimit := maxGrokBinaryBytes
	maxGrokBinaryBytes = 32 << 10
	t.Cleanup(func() { maxGrokBinaryBytes = previousLimit })

	payload := brotliZeros(t, inflated)
	if int64(len(payload)) == 0 || int64(len(payload)) > maxGrokBinaryBytes {
		t.Fatalf("compressed fixture size=%d limit=%d", len(payload), maxGrokBinaryBytes)
	}
	var written int64
	previousOpen := openGrokMemberFile
	openGrokMemberFile = func(destination string) (grokMemberFile, error) {
		file, err := previousOpen(destination)
		if err != nil {
			return nil, err
		}
		return &countingGrokFile{grokMemberFile: file, written: &written}, nil
	}
	t.Cleanup(func() { openGrokMemberFile = previousOpen })

	dest := filepath.Join(t.TempDir(), "bin", "grok")
	err := decompressGrokMember(bytes.NewReader(payload), int64(len(payload)), dest)
	info, statErr := os.Lstat(dest)
	if statErr == nil {
		t.Fatalf("oversize binary remained size=%d written=%d", info.Size(), written)
	}
	if !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("oversize binary stat: %v", statErr)
	}
	if err == nil || err.Error() != "Grok 執行檔超出大小上限" {
		t.Fatalf("err=%v written=%d", err, written)
	}
	if written != maxGrokBinaryBytes+1 {
		t.Fatalf("wrote %d bytes; stream bound is %d", written, maxGrokBinaryBytes+1)
	}
}

func TestGrokExecutorRejectsPathEscape(t *testing.T) {
	body := brotliPayload(t, grokVersionScript("1.0.40"))
	bundle := grokSpecialEntryBundle(t, &tar.Header{
		Name:     "grok/linux-amd64/bin/../../../../tmp/grok.br",
		Typeflag: tar.TypeReg,
		Mode:     0o644,
		Size:     int64(len(body)),
	}, body)
	executor, job, _ := grokTestRun(t, "linux", "amd64", bundle, bundle, int64(len(bundle)))
	rows, err := executor.Run(t.Context(), job)
	if err == nil && allPassed(rows) {
		t.Fatal("accepted a bundle path that leaves the layout")
	}
}

func TestGrokExecutorRejectsBundleDigestMismatch(t *testing.T) {
	const version = "1.0.40"
	bundle := grokExecutorBundle(t, map[string][]byte{
		"linux/amd64": brotliPayload(t, grokVersionScript(version)),
	})
	served := append([]byte(nil), bundle...)
	served[len(served)-1] ^= 0xff
	executor, job, _ := grokTestRun(t, "linux", "amd64", bundle, served, int64(len(bundle)))
	_, err := executor.Run(t.Context(), job)
	var rejection *rejectError
	if !errors.As(err, &rejection) || rejection.Code != deploy.ArtifactHashMismatch {
		t.Fatalf("err=%v", err)
	}
}

func TestGrokExecutorRejectsDownloadOverDeclaredSize(t *testing.T) {
	const version = "1.0.40"
	bundle := grokExecutorBundle(t, map[string][]byte{
		"linux/amd64": brotliPayload(t, grokVersionScript(version)),
	})
	executor, job, _ := grokTestRun(t, "linux", "amd64", bundle, bundle, int64(len(bundle))-1)
	_, err := executor.Run(t.Context(), job)
	var rejection *rejectError
	if !errors.As(err, &rejection) || rejection.Code != deploy.ArtifactHashMismatch ||
		!strings.Contains(rejection.Detail, "超過宣告 size") {
		t.Fatalf("err=%v", err)
	}
}

func TestGrokExecutorLayoutMatchesArtifactContract(t *testing.T) {
	if maxGrokBundleEntries != artifact.MaxGrokBundleEntries || maxGrokBinaryBytes != artifact.MaxGrokBinaryBytes ||
		maxGrokArtifactBytes != artifact.DefaultGrokBundleMaxBytes {
		t.Fatalf("limits entries=%d binary=%d artifact=%d", maxGrokBundleEntries, maxGrokBinaryBytes, maxGrokArtifactBytes)
	}
	for _, platform := range [][2]string{
		{"linux", "amd64"}, {"linux", "arm64"},
		{"darwin", "amd64"}, {"darwin", "arm64"},
		{"windows", "amd64"}, {"windows", "arm64"},
	} {
		got, ok := grokBundleMember(platform[0], platform[1])
		want, wantOK := artifact.GrokBundleMember(platform[0], platform[1])
		command, commandOK := grokCommandRelative(platform[0])
		wantCommand, wantCommandOK := artifact.GrokCommandRelative(platform[0])
		if !ok || !wantOK || got != want || !commandOK || !wantCommandOK || command != wantCommand {
			t.Fatalf("platform %s/%s member %s/%s command %s/%s", platform[0], platform[1], got, want, command, wantCommand)
		}
	}
	if _, ok := grokBundleMember("linux", "386"); ok {
		t.Fatal("executor accepted an unsupported target")
	}
}

func TestGrokVersionUsesCommandThenExactVersion(t *testing.T) {
	const version = "1.0.40"
	if !grokVersionMatches("\n\ngrok 1.0.40 (eb1a2256660d) [stable]\n", version) {
		t.Fatal("official version line was not accepted")
	}
	if grokVersionMatches("1.0.40 grok\n", version) {
		t.Fatal("accepted a line that does not start with grok")
	}
	if grokVersionMatches("grokx 1.0.40\n", version) {
		t.Fatal("accepted a command that is not grok")
	}
	if grokVersionMatches("grok 1.0.41 (eb1a2256660d) [stable]\n", version) {
		t.Fatal("accepted a different version")
	}
}

func grokTestRun(t *testing.T, targetOS, targetArch string, identity, served []byte, declaredSize int64) (grokExecutor, model.JobResponse, *int) {
	t.Helper()
	downloads := 0
	executor := grokExecutor{
		targetOS: targetOS, targetArch: targetArch,
		deps: execDeps{
			home: "/home/grok-test", fsRoot: t.TempDir(), hubURL: "https://hub.example", token: "machine-token",
			now: func() time.Time { return time.Date(2026, 9, 21, 16, 0, 0, 0, time.UTC) },
			httpGet: func(_ context.Context, rawURL string) (*http.Response, error) {
				downloads++
				if !strings.HasPrefix(rawURL, "https://hub.example/v1/artifacts/") {
					return nil, errors.New("unexpected artifact URL")
				}
				return &http.Response{
					StatusCode: http.StatusOK, ContentLength: int64(len(served)),
					Body: io.NopCloser(bytes.NewReader(served)),
				}, nil
			},
		},
	}
	digest := sha256.Sum256(identity)
	hexDigest := hex.EncodeToString(digest[:])
	spec, err := json.Marshal(model.GrokSpec{
		Kind: agentadapter.ExecutorKindGrok, Version: "1.0.40",
		TargetOS: targetOS, TargetArch: targetArch, BundleLayout: model.GrokBundleLayoutV1,
		Artifact: &model.ArtifactRef{SHA256: hexDigest, Size: declaredSize, URL: "/v1/artifacts/" + hexDigest},
	})
	if err != nil {
		t.Fatal(err)
	}
	job := model.JobResponse{
		JobID: "grok-job", ResourceKind: agentadapter.ExecutorKindGrok, ResourceID: "grok",
		Revision: 1, Spec: spec, ArtifactDigest: "sha256:" + hexDigest, ExecutionTimeout: 30,
	}
	return executor, job, &downloads
}

func grokVersionScript(version string) []byte {
	return []byte("#!/bin/sh\nif [ \"$1\" = --version ]; then echo 'grok " + version + " (eb1a2256660d) [stable]'; exit 0; fi\nexit 1\n")
}

func grokExecutorBundle(t *testing.T, members map[string][]byte) []byte {
	t.Helper()
	var entries []nodeBundleEntry
	entries = append(entries, nodeBundleEntry{name: "grok/", typeflag: tar.TypeDir, mode: 0o755})
	order := []string{"darwin/arm64", "darwin/amd64", "linux/amd64", "linux/arm64", "windows/amd64", "windows/arm64"}
	for _, platform := range order {
		body, ok := members[platform]
		if !ok {
			continue
		}
		osName, arch, _ := strings.Cut(platform, "/")
		name, ok := grokBundleMember(osName, arch)
		if !ok {
			t.Fatalf("missing member for %s", platform)
		}
		entries = append(entries, nodeBundleEntry{name: name, typeflag: tar.TypeReg, mode: 0o644, body: body})
	}
	return writeNodeRuntimeBundle(t, entries)
}

func grokSpecialEntryBundle(t *testing.T, header *tar.Header, body ...[]byte) []byte {
	t.Helper()
	var output bytes.Buffer
	gz := gzip.NewWriter(&output)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "grok/", Typeflag: tar.TypeDir, Mode: 0o755}); err != nil {
		t.Fatal(err)
	}
	if err := tw.WriteHeader(header); err != nil {
		t.Fatal(err)
	}
	if len(body) > 0 {
		if _, err := tw.Write(body[0]); err != nil {
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

func brotliPayload(t *testing.T, payload []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	writer := brotli.NewWriterLevel(&buf, brotli.BestSpeed)
	if _, err := writer.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func brotliZeros(t *testing.T, n int64) []byte {
	t.Helper()
	var buf bytes.Buffer
	writer := brotli.NewWriterLevel(&buf, brotli.BestSpeed)
	if _, err := io.Copy(writer, &zeroReader{n: n}); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

type zeroReader struct{ n int64 }

func (z *zeroReader) Read(p []byte) (int, error) {
	if z.n <= 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > z.n {
		p = p[:z.n]
	}
	for i := range p {
		p[i] = 0
	}
	z.n -= int64(len(p))
	return len(p), nil
}

type countingGrokFile struct {
	grokMemberFile
	written *int64
}

func (f *countingGrokFile) Write(p []byte) (int, error) {
	n, err := f.grokMemberFile.Write(p)
	*f.written += int64(n)
	return n, err
}

func grokNonRegularMemberBundle(t *testing.T, typeflag byte, body []byte) []byte {
	t.Helper()
	const member = "grok/linux-amd64/bin/grok.br"
	var raw bytes.Buffer
	tw := tar.NewWriter(&raw)
	if err := tw.WriteHeader(&tar.Header{Name: "grok/", Typeflag: tar.TypeDir, Mode: 0o755}); err != nil {
		t.Fatal(err)
	}
	header := &tar.Header{
		Name: member, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body)),
	}
	switch typeflag {
	case tar.TypeSymlink, tar.TypeLink:
		header.Linkname = "elsewhere"
	case tar.TypeChar, tar.TypeBlock:
		header.Devmajor = 1
		header.Devminor = 3
	}
	if err := tw.WriteHeader(header); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	patched := patchTarMemberTypeflag(t, raw.Bytes(), member, typeflag, body)
	var output bytes.Buffer
	gz := gzip.NewWriter(&output)
	if _, err := gz.Write(patched); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	bundle := output.Bytes()
	gzReader, err := gzip.NewReader(bytes.NewReader(bundle))
	if err != nil {
		t.Fatal(err)
	}
	defer gzReader.Close()
	tr := tar.NewReader(gzReader)
	for {
		hdr, err := tr.Next()
		if err != nil {
			t.Fatal(err)
		}
		if hdr.Name == "grok/" {
			continue
		}
		if hdr.Name != member || hdr.Typeflag != typeflag || hdr.Size != int64(len(body)) {
			t.Fatalf("fixture header name=%q type=%q size=%d", hdr.Name, hdr.Typeflag, hdr.Size)
		}
		return bundle
	}
}

func patchTarMemberTypeflag(t *testing.T, raw []byte, name string, typeflag byte, body []byte) []byte {
	t.Helper()
	out := append([]byte(nil), raw...)
	for off := 0; off+512 <= len(out); {
		hdr := out[off : off+512]
		if bytes.Equal(hdr, make([]byte, 512)) {
			break
		}
		size := tarOctal(t, hdr[124:136])
		entryName := cString(hdr[:100])
		dataAt := off + 512
		padded := int((size + 511) &^ 511)
		if dataAt+padded > len(out) {
			t.Fatalf("tar member %q overruns the archive", entryName)
		}
		if entryName == name {
			if int(size) != len(body) || !bytes.Equal(out[dataAt:dataAt+len(body)], body) {
				t.Fatalf("tar member %q body missing before typeflag patch", name)
			}
			hdr[156] = typeflag
			putTarChecksum(hdr)
			return out
		}
		off = dataAt + padded
	}
	t.Fatalf("tar member %s not found", name)
	return nil
}

func cString(field []byte) string {
	if i := bytes.IndexByte(field, 0); i >= 0 {
		field = field[:i]
	}
	return string(field)
}

func tarOctal(t *testing.T, field []byte) int64 {
	t.Helper()
	s := strings.Trim(string(field), " \x00")
	if s == "" {
		return 0
	}
	n, err := strconv.ParseInt(s, 8, 64)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func putTarChecksum(hdr []byte) {
	for i := 148; i < 156; i++ {
		hdr[i] = ' '
	}
	sum := 0
	for _, c := range hdr {
		sum += int(c)
	}
	octal := strconv.FormatInt(int64(sum), 8)
	for len(octal) < 6 {
		octal = "0" + octal
	}
	copy(hdr[148:154], octal)
	hdr[154] = 0
	hdr[155] = ' '
}
