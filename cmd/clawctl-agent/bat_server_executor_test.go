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
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/agentadapter"
	"github.com/teddashh/AI-Intune/internal/model"
)

func TestBATServerExecutorInstallsAndRestartsUserService(t *testing.T) {
	const version = "3.2.10"
	const token = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	binary := []byte("bat-server-linux-amd64-binary")
	notes := []byte("readme")
	sum := sha256.Sum256(binary)
	binaryHash := hex.EncodeToString(sum[:])
	bundle := batServerExecutorBundle(t, map[string][]byte{
		"bat-server/linux-amd64/bat-server.tar.gz": batServerInnerGzip(t, []batServerTarEntry{
			{name: "bat-server-linux-x86_64/", dir: true, mode: 0o755},
			{name: "bat-server-linux-x86_64/bat-server", mode: 0o755, body: binary},
			{name: "bat-server-linux-x86_64/README.md", mode: 0o644, body: notes},
		}),
		"bat-server/linux-arm64/bat-server.tar.gz": batServerInnerGzip(t, []batServerTarEntry{
			{name: "bat-server-linux-aarch64/", dir: true, mode: 0o755},
			{name: "bat-server-linux-aarch64/bat-server", mode: 0o755, body: []byte("other-arch")},
		}),
	})
	executor, job, downloads, actions := batServerTestRun(t, "linux", "amd64", bundle, binaryHash, "active")
	var listened []string
	executor.listen = func(network, address string) (net.Listener, error) {
		listened = append(listened, network+" "+address)
		return fakeBATPortListener{}, nil
	}
	tokenPath := executor.deps.fsPath("/home/bat-test/.local/share/clawctl/bat-server/credentials/token")
	if err := os.MkdirAll(filepath.Dir(tokenPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tokenPath, []byte(token), 0o600); err != nil {
		t.Fatal(err)
	}
	// The machine's own BAT server keeps its unit.
	ownerUnit := executor.deps.fsPath("/home/bat-test/.config/systemd/user/bat-server.service")
	ownerBody := []byte("[Service]\nExecStart=/opt/bat-server/bat-server --bind=tailscale --port=9876\n")
	ownerTime := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := os.MkdirAll(filepath.Dir(ownerUnit), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ownerUnit, ownerBody, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(ownerUnit, ownerTime, ownerTime); err != nil {
		t.Fatal(err)
	}

	rows, err := executor.Run(t.Context(), job)
	if err != nil || len(rows) != 5 || !allPassed(rows) || *downloads != 1 {
		t.Fatalf("rows=%+v downloads=%d err=%v", rows, *downloads, err)
	}
	if strings.Join(listened, "\n") != "tcp 127.0.0.1:19876" {
		t.Fatalf("port check listened on %v", listened)
	}
	if strings.Contains(batServerResultText(rows, err), token) {
		t.Fatalf("token leaked: %+v err=%v", rows, err)
	}
	release := "/home/bat-test/.local/share/clawctl/bat-server/releases/" + version
	installed := executor.deps.fsPath(release + "/bat-server-linux-x86_64/bat-server")
	info, err := os.Lstat(installed)
	body, readErr := os.ReadFile(installed)
	if err != nil || readErr != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o755 || !bytes.Equal(body, binary) {
		t.Fatalf("binary info=%v mode=%v err=%v read=%v body=%q", info, info.Mode().Perm(), err, readErr, body)
	}
	noteInfo, err := os.Lstat(executor.deps.fsPath(release + "/bat-server-linux-x86_64/README.md"))
	if err != nil || noteInfo.Mode().Perm() != 0o644 {
		t.Fatalf("readme mode=%v err=%v", noteInfo, err)
	}
	if _, err := os.Lstat(executor.deps.fsPath(release + "/bat-server-linux-aarch64/bat-server")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unpacked the other architecture: %v", err)
	}
	tokenBody, err := os.ReadFile(tokenPath)
	if err != nil || string(tokenBody) != token {
		t.Fatalf("token changed body=%q err=%v", tokenBody, err)
	}
	const unitFile = "/home/bat-test/.local/share/clawctl/bat-server/clawctl-bat-server.service"
	unit, err := os.ReadFile(executor.deps.fsPath(unitFile))
	if err != nil || strings.Contains(string(unit), token) || strings.Contains(string(unit), "--token=") {
		t.Fatalf("unit=%s err=%v", unit, err)
	}
	unitText := string(unit)
	for _, want := range []string{
		"Description=AI-Intune BAT Server",
		"Type=simple",
		"WorkingDirectory=" + release,
		"ExecStart=" + release + "/bat-server-linux-x86_64/bat-server --bind=localhost --port=19876 --token-file=/home/bat-test/.local/share/clawctl/bat-server/credentials/token --data-dir=/home/bat-test/.local/share/clawctl/bat-server/data",
		"StandardOutput=null",
		"StandardError=journal",
		"UMask=0077",
		"Restart=on-failure",
		"WantedBy=default.target",
	} {
		if !strings.Contains(unitText, want+"\n") {
			t.Fatalf("unit missing %q\n%s", want, unitText)
		}
	}
	for _, banned := range []string{"IS_SANDBOX", "network-online", "--port=9876", "Environment="} {
		if strings.Contains(unitText, banned) {
			t.Fatalf("unit contains %q\n%s", banned, unitText)
		}
	}
	link := executor.deps.fsPath("/home/bat-test/.config/systemd/user/clawctl-bat-server.service")
	if target, err := os.Readlink(link); err != nil || target != unitFile {
		t.Fatalf("link target=%q err=%v", target, err)
	}
	if strings.Join(*actions, "\n") != strings.Join([]string{
		"--user is-active clawctl-bat-server.service",
		"--user link " + unitFile,
		"--user daemon-reload",
		"--user enable clawctl-bat-server.service",
		"--user restart clawctl-bat-server.service",
		"--user is-active clawctl-bat-server.service",
	}, "\n") {
		t.Fatalf("actions=%v", *actions)
	}
	for _, row := range rows {
		switch row.RuleID {
		case "bat-server-release":
			if row.Command != "test -d "+release || strings.TrimSpace(row.StdoutExcerpt) != release {
				t.Fatalf("release evidence=%+v", row)
			}
		case "bat-server-artifact":
			if row.Command != "cat "+release+"/.clawctl-artifact-sha256" || !strings.Contains(row.StdoutExcerpt, job.ArtifactDigest) {
				t.Fatalf("artifact evidence=%+v", row)
			}
		case "bat-server-binary":
			if row.Command != "sha256sum "+release+"/bat-server-linux-x86_64/bat-server" || strings.TrimSpace(row.StdoutExcerpt) != binaryHash {
				t.Fatalf("binary evidence=%+v", row)
			}
		case "bat-server-unit":
			if row.Command != "systemctl --user is-active clawctl-bat-server.service" || strings.TrimSpace(row.StdoutExcerpt) != "active" {
				t.Fatalf("unit evidence=%+v", row)
			}
		case "bat-server-endpoint":
			if row.Command != "bat-remote auth 127.0.0.1:19876" || row.StdoutExcerpt != "authenticated\n" ||
				row.StderrExcerpt != "" || row.ExitCode != 0 {
				t.Fatalf("endpoint evidence=%+v", row)
			}
		default:
			t.Fatalf("unexpected rule %s", row.RuleID)
		}
	}

	*actions = nil
	again, err := executor.Run(t.Context(), job)
	if err != nil || len(again) != 5 || !allPassed(again) || *downloads != 1 || string(tokenBody) != string(mustRead(t, tokenPath)) {
		t.Fatalf("reuse rows=%+v downloads=%d err=%v", again, *downloads, err)
	}
	if strings.Contains(batServerResultText(again, err), token) {
		t.Fatalf("reuse leaked token: %+v", again)
	}
	// The unit is active, so the port is its own; the link exists, so there is
	// nothing to link or reload.
	if len(listened) != 1 || strings.Join(*actions, "\n") != strings.Join([]string{
		"--user is-active clawctl-bat-server.service",
		"--user enable clawctl-bat-server.service",
		"--user restart clawctl-bat-server.service",
		"--user is-active clawctl-bat-server.service",
	}, "\n") {
		t.Fatalf("second run listened=%v actions=%v", listened, *actions)
	}

	ownerInfo, err := os.Lstat(ownerUnit)
	if err != nil || !ownerInfo.Mode().IsRegular() || !ownerInfo.ModTime().Equal(ownerTime) || !bytes.Equal(mustRead(t, ownerUnit), ownerBody) {
		t.Fatalf("the owner's unit changed: info=%v err=%v", ownerInfo, err)
	}
}

func TestBATServerExecutorSourceNamesOnlyItsOwnUnitAndPort(t *testing.T) {
	source := string(mustRead(t, "bat_server_executor.go"))
	if strings.Contains(strings.ReplaceAll(source, "clawctl-bat-server.service", ""), "bat-server.service") {
		t.Fatal("the executor names the machine's own bat-server.service")
	}
	for _, banned := range []string{"9876", "IS_SANDBOX"} {
		if strings.Contains(source, banned) {
			t.Fatalf("the executor contains %q", banned)
		}
	}
}

func TestBATServerExecutorSilencesServiceStdout(t *testing.T) {
	body := batServerUnitBody(
		"/home/bat-test/.local/share/clawctl/bat-server/releases/3.2.10",
		"/home/bat-test/.local/share/clawctl/bat-server/releases/3.2.10/bat-server-linux-x86_64/bat-server",
		"/home/bat-test/.local/share/clawctl/bat-server/credentials/token",
		"/home/bat-test/.local/share/clawctl/bat-server/data",
	)
	if !strings.Contains(body, "\nStandardOutput=null\nStandardError=journal\nUMask=0077\n") {
		t.Fatalf("unit body did not discard stdout:\n%s", body)
	}

	deps := execDeps{fsRoot: t.TempDir()}
	logical := "/home/bat-test/.config/systemd/user/bat-server.service"
	unsilenced := []byte("[Service]\nExecStart=/opt/bat/bat-server --token-file=/opt/bat/token\nUMask=0077\n")
	for _, line := range bytes.Split(unsilenced, []byte{'\n'}) {
		if bytes.Equal(line, []byte("StandardOutput=null")) || bytes.Contains(line, []byte("--token=")) {
			t.Fatalf("fixture contains forbidden gate string: %s", unsilenced)
		}
	}
	changed, err := writeBATServerUnit(deps, logical, unsilenced)
	unitPath := deps.fsPath(logical)
	_, statErr := os.Lstat(unitPath)
	entries, dirErr := os.ReadDir(filepath.Dir(unitPath))
	if err == nil || err.Error() != "BAT Server unit does not discard stdout" || changed || !errors.Is(statErr, os.ErrNotExist) ||
		(dirErr == nil && len(entries) > 0) || (dirErr != nil && !errors.Is(dirErr, os.ErrNotExist)) {
		t.Fatalf("unit without discarded stdout was written changed=%v err=%v stat=%v entries=%v dirErr=%v", changed, err, statErr, entries, dirErr)
	}
}

func TestBATServerExecutorReusesExistingToken(t *testing.T) {
	const token = "bat-token-sentinel-do-not-leak-0123456789abcdef"
	binary := []byte("token-reuse-binary")
	sum := sha256.Sum256(binary)
	bundle := batServerExecutorBundle(t, map[string][]byte{
		"bat-server/linux-amd64/bat-server.tar.gz": batServerInnerGzip(t, []batServerTarEntry{
			{name: "bat-server-linux-x86_64/bat-server", mode: 0o755, body: binary},
		}),
	})
	executor, job, _, _ := batServerTestRun(t, "linux", "amd64", bundle, hex.EncodeToString(sum[:]), "active")
	tokenPath := executor.deps.fsPath("/home/bat-test/.local/share/clawctl/bat-server/credentials/token")
	if err := os.MkdirAll(filepath.Dir(tokenPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tokenPath, []byte(token), 0o600); err != nil {
		t.Fatal(err)
	}
	rows, err := executor.Run(t.Context(), job)
	body, readErr := os.ReadFile(tokenPath)
	if err != nil || readErr != nil || string(body) != token || !allPassed(rows) {
		t.Fatalf("rows=%+v body=%q err=%v read=%v", rows, body, err, readErr)
	}
	if strings.Contains(batServerResultText(rows, err), token) {
		t.Fatalf("token leaked: %+v", rows)
	}
}

func TestBATServerExecutorCreatesTokenWithoutReplacingIt(t *testing.T) {
	binary := []byte("fresh-token-binary")
	sum := sha256.Sum256(binary)
	bundle := batServerExecutorBundle(t, map[string][]byte{
		"bat-server/linux-amd64/bat-server.tar.gz": batServerInnerGzip(t, []batServerTarEntry{
			{name: "bat-server-linux-x86_64/bat-server", mode: 0o755, body: binary},
		}),
	})
	executor, job, _, _ := batServerTestRun(t, "linux", "amd64", bundle, hex.EncodeToString(sum[:]), "active")
	rows, err := executor.Run(t.Context(), job)
	tokenPath := executor.deps.fsPath("/home/bat-test/.local/share/clawctl/bat-server/credentials/token")
	body, readErr := os.ReadFile(tokenPath)
	info, statErr := os.Lstat(tokenPath)
	dirInfo, dirErr := os.Lstat(filepath.Dir(tokenPath))
	if err != nil || readErr != nil || statErr != nil || dirErr != nil || !allPassed(rows) ||
		len(body) != 64 || info.Mode().Perm() != 0o600 || dirInfo.Mode().Perm() != 0o700 {
		t.Fatalf("rows=%+v body=%q mode=%v dir=%v err=%v", rows, body, info, dirInfo, err)
	}
	if strings.Contains(batServerResultText(rows, err), string(body)) {
		t.Fatalf("generated token leaked: %+v", rows)
	}
	again, err := executor.Run(t.Context(), job)
	next, readErr := os.ReadFile(tokenPath)
	if err != nil || readErr != nil || !bytes.Equal(body, next) || !allPassed(again) {
		t.Fatalf("rotated token next=%q err=%v read=%v", next, err, readErr)
	}
}

func TestBATServerExecutorRejectsNonRegularToken(t *testing.T) {
	t.Run("symlink", func(t *testing.T) {
		root := t.TempDir()
		credentials := filepath.Join(root, "credentials")
		if err := os.MkdirAll(credentials, 0o700); err != nil {
			t.Fatal(err)
		}
		tokenPath := filepath.Join(credentials, "token")
		decoyPath := filepath.Join(root, "decoy")
		decoy := []byte("planted-bat-server-token-must-stay-unread")
		if len(bytes.TrimSpace(decoy)) == 0 {
			t.Fatal("decoy fixture is empty")
		}
		if err := os.WriteFile(decoyPath, decoy, 0o600); err != nil {
			t.Fatal(err)
		}
		before, err := os.Lstat(decoyPath)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(decoyPath, tokenPath); err != nil {
			t.Fatal(err)
		}
		err = ensureBATServerToken(tokenPath)
		after, statErr := os.Lstat(decoyPath)
		got, readErr := os.ReadFile(decoyPath)
		if statErr != nil || readErr != nil || !os.SameFile(before, after) || after.Mode() != before.Mode() || !bytes.Equal(got, decoy) {
			t.Fatal("decoy file was modified or deleted")
		}
		target, linkErr := os.Readlink(tokenPath)
		if linkErr != nil || target != decoyPath {
			t.Fatal("token symlink was replaced or retargeted")
		}
		refuseNonRegularBATServerToken(t, err, decoy)
	})
	t.Run("directory", func(t *testing.T) {
		credentials := filepath.Join(t.TempDir(), "credentials")
		if err := os.MkdirAll(credentials, 0o700); err != nil {
			t.Fatal(err)
		}
		tokenPath := filepath.Join(credentials, "token")
		if err := os.Mkdir(tokenPath, 0o700); err != nil {
			t.Fatal(err)
		}
		markerPath := filepath.Join(tokenPath, "marker")
		marker := []byte("token-directory-marker")
		if err := os.WriteFile(markerPath, marker, 0o600); err != nil {
			t.Fatal(err)
		}
		err := ensureBATServerToken(tokenPath)
		info, statErr := os.Lstat(tokenPath)
		got, readErr := os.ReadFile(markerPath)
		if statErr != nil || readErr != nil || info == nil || !info.IsDir() || !bytes.Equal(got, marker) {
			t.Fatal("token directory was modified or deleted")
		}
		refuseNonRegularBATServerToken(t, err, nil)
	})
}

func refuseNonRegularBATServerToken(t *testing.T, err error, secret []byte) {
	t.Helper()
	if err == nil {
		if len(secret) > 0 {
			t.Fatal("decoy contents were adopted as the token")
		}
		t.Fatal("non-regular token path was accepted")
	}
	msg := err.Error()
	if len(secret) > 0 && bytes.Contains([]byte(msg), secret) {
		t.Fatal("token path was not rejected as a non-regular file")
	}
	if msg != "BAT Server token is not a regular file" {
		t.Fatalf("token path rejected as %q", msg)
	}
}

func TestBATServerExecutorRejectsDarwinTarget(t *testing.T) {
	binary := []byte("darwin-should-not-install")
	sum := sha256.Sum256(binary)
	bundle := batServerExecutorBundle(t, map[string][]byte{
		"bat-server/linux-arm64/bat-server.tar.gz": batServerInnerGzip(t, []batServerTarEntry{
			{name: "bat-server-linux-aarch64/bat-server", mode: 0o755, body: binary},
		}),
	})
	executor, job, downloads, actions := batServerTestRun(t, "darwin", "arm64", bundle, hex.EncodeToString(sum[:]), "active")
	rows, err := executor.Run(t.Context(), job)
	var rejected *rejectError
	if err == nil || len(rows) != 0 || !errors.As(err, &rejected) || *downloads != 0 || len(*actions) != 0 {
		t.Fatalf("darwin was accepted rows=%+v downloads=%d actions=%v err=%v", rows, *downloads, *actions, err)
	}
	release := executor.deps.fsPath("/home/bat-test/.local/share/clawctl/bat-server/releases/3.2.10")
	if _, statErr := os.Lstat(release); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("darwin install left %s: %v", release, statErr)
	}
}

func TestBATServerExecutorRejectsSymlinkAndHardlinkByTypeflag(t *testing.T) {
	payload := []byte("typeflag-body-not-empty")
	if len(payload) == 0 {
		t.Fatal("typeflag fixture is empty")
	}
	sum := sha256.Sum256(payload)
	for _, tc := range []struct {
		name     string
		typeflag byte
	}{
		{name: "symlink", typeflag: tar.TypeSymlink},
		{name: "hardlink", typeflag: tar.TypeLink},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := batServerRawTar(t, "bat-server-linux-x86_64/bat-server", 0o755, payload)
			patched := patchTarMemberTypeflag(t, raw, "bat-server-linux-x86_64/bat-server", tc.typeflag, payload)
			header := readTarHeader(t, patched, "bat-server-linux-x86_64/bat-server")
			if header.Typeflag != tc.typeflag || header.Size == 0 {
				t.Fatalf("fixture type=%q size=%d", header.Typeflag, header.Size)
			}
			bundle := batServerExecutorBundle(t, map[string][]byte{
				"bat-server/linux-amd64/bat-server.tar.gz": gzipBytesForAgent(t, patched),
			})
			executor, job, _, actions := batServerTestRun(t, "linux", "amd64", bundle, hex.EncodeToString(sum[:]), "active")
			rows, err := executor.Run(t.Context(), job)
			if err != nil || len(rows) != 1 || rows[0].Passed || !strings.Contains(rows[0].StderrExcerpt, "is not a regular file") {
				t.Fatalf("typeflag %s rows=%+v err=%v", tc.name, rows, err)
			}
			if strings.Join(*actions, "\n") != "--user is-active clawctl-bat-server.service" {
				t.Fatalf("typeflag %s started the service: %v", tc.name, *actions)
			}
			binary := executor.deps.fsPath("/home/bat-test/.local/share/clawctl/bat-server/releases/3.2.10/bat-server-linux-x86_64/bat-server")
			if _, statErr := os.Lstat(binary); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("typeflag %s left %s: %v", tc.name, binary, statErr)
			}
		})
	}
}

func TestBATServerExecutorRejectsPathEscape(t *testing.T) {
	payload := []byte("escaped")
	bundle := batServerExecutorBundle(t, map[string][]byte{
		"bat-server/linux-amd64/bat-server.tar.gz": batServerInnerGzip(t, []batServerTarEntry{
			{name: "bat-server-linux-x86_64/bat-server", mode: 0o755, body: []byte("real-binary")},
			{name: "../../bat-server-escaped", mode: 0o644, body: payload},
		}),
	})
	sum := sha256.Sum256([]byte("real-binary"))
	executor, job, _, _ := batServerTestRun(t, "linux", "amd64", bundle, hex.EncodeToString(sum[:]), "active")
	rows, err := executor.Run(t.Context(), job)
	if err == nil && allPassed(rows) {
		t.Fatal("accepted a member that escapes the release")
	}
	if err != nil || len(rows) == 0 || !strings.Contains(rows[0].StderrExcerpt, "path outside release") {
		t.Fatalf("rows=%+v err=%v", rows, err)
	}
	found := false
	_ = filepath.WalkDir(executor.deps.fsRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr == nil && entry.Name() == "bat-server-escaped" {
			found = true
		}
		return nil
	})
	if found {
		t.Fatal("path escape wrote bat-server-escaped")
	}
}

func TestBATServerExecutorDoesNotActivateOnBinaryHashMismatch(t *testing.T) {
	binary := []byte("hash-mismatch-binary")
	bundle := batServerExecutorBundle(t, map[string][]byte{
		"bat-server/linux-amd64/bat-server.tar.gz": batServerInnerGzip(t, []batServerTarEntry{
			{name: "bat-server-linux-x86_64/bat-server", mode: 0o755, body: binary},
		}),
	})
	executor, job, _, actions := batServerTestRun(t, "linux", "amd64", bundle, strings.Repeat("ab", 32), "active")
	rows, err := executor.Run(t.Context(), job)
	if err != nil || len(rows) != 1 || rows[0].Passed || rows[0].RuleID != "bat-server-binary" ||
		strings.Join(*actions, "\n") != "--user is-active clawctl-bat-server.service" {
		t.Fatalf("rows=%+v actions=%v err=%v", rows, *actions, err)
	}
	release := executor.deps.fsPath("/home/bat-test/.local/share/clawctl/bat-server/releases/3.2.10")
	if _, statErr := os.Lstat(release); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("mismatch activated %s: %v", release, statErr)
	}
}

func TestBATServerExecutorRequiresActiveUnit(t *testing.T) {
	binary := []byte("inactive-unit-binary")
	sum := sha256.Sum256(binary)
	bundle := batServerExecutorBundle(t, map[string][]byte{
		"bat-server/linux-amd64/bat-server.tar.gz": batServerInnerGzip(t, []batServerTarEntry{
			{name: "bat-server-linux-x86_64/bat-server", mode: 0o755, body: binary},
		}),
	})
	executor, job, _, _ := batServerTestRun(t, "linux", "amd64", bundle, hex.EncodeToString(sum[:]), "inactive")
	handshakes := 0
	executor.handshake = func(context.Context, string, string) error {
		handshakes++
		return nil
	}
	rows, err := executor.Run(t.Context(), job)
	if err != nil || allPassed(rows) || handshakes != 0 {
		t.Fatalf("inactive unit was accepted rows=%+v handshakes=%d err=%v", rows, handshakes, err)
	}
	for _, row := range rows {
		if row.RuleID == "bat-server-endpoint" {
			t.Fatalf("an endpoint row followed a failed unit row: %+v", row)
		}
	}
	var unit *model.JobVerificationRequest
	for i := range rows {
		if rows[i].RuleID == "bat-server-unit" {
			unit = &rows[i]
		}
	}
	if unit == nil || unit.Passed || unit.Command != "systemctl --user is-active clawctl-bat-server.service" {
		t.Fatalf("unit evidence=%+v rows=%+v", unit, rows)
	}
}

func TestBATServerExecutorInstallsARM64Tree(t *testing.T) {
	binary := []byte("arm64-only-binary")
	sum := sha256.Sum256(binary)
	bundle := batServerExecutorBundle(t, map[string][]byte{
		"bat-server/linux-arm64/bat-server.tar.gz": batServerInnerGzip(t, []batServerTarEntry{
			{name: "bat-server-linux-aarch64/bat-server", mode: 0o755, body: binary},
		}),
	})
	executor, job, _, _ := batServerTestRun(t, "linux", "arm64", bundle, hex.EncodeToString(sum[:]), "active")
	rows, err := executor.Run(t.Context(), job)
	if err != nil || !allPassed(rows) {
		t.Fatalf("rows=%+v err=%v", rows, err)
	}
	var binaryRow *model.JobVerificationRequest
	for i := range rows {
		if rows[i].RuleID == "bat-server-binary" {
			binaryRow = &rows[i]
		}
	}
	want := "/home/bat-test/.local/share/clawctl/bat-server/releases/3.2.10/bat-server-linux-aarch64/bat-server"
	if binaryRow == nil || binaryRow.Command != "sha256sum "+want {
		t.Fatalf("binary evidence=%+v", binaryRow)
	}
}

type batServerTarEntry struct {
	name string
	mode int64
	body []byte
	dir  bool
}

func batServerTestRun(t *testing.T, targetOS, targetArch string, bundle []byte, binaryHash, active string) (batServerExecutor, model.JobResponse, *int, *[]string) {
	t.Helper()
	downloads := 0
	var actions []string
	restarted := false
	fsRoot := t.TempDir()

	executor := batServerExecutor{
		targetOS: targetOS, targetArch: targetArch,
		listen: func(string, string) (net.Listener, error) {
			return fakeBATPortListener{}, nil
		},
		handshake: func(context.Context, string, string) error {
			return nil
		},
		deps: execDeps{
			home: "/home/bat-test", fsRoot: fsRoot, hubURL: "https://hub.example", token: "machine-token",
			now:   func() time.Time { return time.Date(2026, 9, 22, 3, 0, 0, 0, time.UTC) },
			sleep: func(ctx context.Context, d time.Duration) error { return ctx.Err() },
			httpGet: func(_ context.Context, rawURL string) (*http.Response, error) {
				downloads++
				if !strings.HasPrefix(rawURL, "https://hub.example/v1/artifacts/") {
					return nil, errors.New("unexpected artifact URL")
				}
				return &http.Response{
					StatusCode: http.StatusOK, ContentLength: int64(len(bundle)),
					Body: io.NopCloser(bytes.NewReader(bundle)),
				}, nil
			},
			systemctl: func(_ context.Context, args ...string) (string, string, error) {
				actions = append(actions, strings.Join(args, " "))
				if len(args) >= 2 && args[0] == "--user" {
					if args[1] == "restart" {
						restarted = true
					}
					if args[1] == "is-active" {
						if !restarted {
							return "inactive\n", "", nil
						}
						if active == "active" {
							return "active\n", "", nil
						}
						return active + "\n", "", nil
					}
					if args[1] == "link" && len(args) == 3 {
						link := filepath.Join(fsRoot, "/home/bat-test/.config/systemd/user/clawctl-bat-server.service")
						if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
							return "", "", err
						}
						return "", "", os.Symlink(args[2], link)
					}
				}
				return "", "", nil
			},
		},
	}
	digest := sha256.Sum256(bundle)
	hexDigest := hex.EncodeToString(digest[:])
	spec, err := json.Marshal(model.BATServerSpec{
		Kind: agentadapter.ExecutorKindBATServer, Version: "3.2.10",
		TargetOS: targetOS, TargetArch: targetArch, BundleLayout: model.BATServerBundleLayoutV1,
		BinarySHA256: binaryHash,
		Artifact:     &model.ArtifactRef{SHA256: hexDigest, Size: int64(len(bundle)), URL: "/v1/artifacts/" + hexDigest},
	})
	if err != nil {
		t.Fatal(err)
	}
	job := model.JobResponse{
		JobID: "bat-job", ResourceKind: agentadapter.ExecutorKindBATServer, ResourceID: "bat-server",
		Revision: 1, Spec: spec, ArtifactDigest: "sha256:" + hexDigest, ExecutionTimeout: 30,
	}
	return executor, job, &downloads, &actions
}

// fakeBATPortListener stands in for a successful bind, so the tests never
// depend on the real port being free.
type fakeBATPortListener struct{}

func (fakeBATPortListener) Accept() (net.Conn, error) { return nil, net.ErrClosed }
func (fakeBATPortListener) Close() error              { return nil }
func (fakeBATPortListener) Addr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: model.BATServerPort}
}

func batServerExecutorBundle(t *testing.T, members map[string][]byte) []byte {
	t.Helper()
	var output bytes.Buffer
	gz := gzip.NewWriter(&output)
	tw := tar.NewWriter(gz)
	for _, name := range []string{
		"bat-server/linux-amd64/bat-server.tar.gz",
		"bat-server/linux-arm64/bat-server.tar.gz",
	} {
		body, ok := members[name]
		if !ok {
			continue
		}
		if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body))}); err != nil {
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

func batServerInnerGzip(t *testing.T, entries []batServerTarEntry) []byte {
	t.Helper()
	return gzipBytesForAgent(t, batServerRawEntries(t, entries))
}

func batServerRawEntries(t *testing.T, entries []batServerTarEntry) []byte {
	t.Helper()
	var raw bytes.Buffer
	tw := tar.NewWriter(&raw)
	for _, entry := range entries {
		flag := byte(tar.TypeReg)
		size := int64(len(entry.body))
		if entry.dir {
			flag = tar.TypeDir
			size = 0
		}
		if err := tw.WriteHeader(&tar.Header{Name: entry.name, Typeflag: flag, Mode: entry.mode, Size: size}); err != nil {
			t.Fatal(err)
		}
		if size > 0 {
			if _, err := tw.Write(entry.body); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return raw.Bytes()
}

func batServerRawTar(t *testing.T, name string, mode int64, body []byte) []byte {
	t.Helper()
	return batServerRawEntries(t, []batServerTarEntry{{name: name, mode: mode, body: body}})
}

func gzipBytesForAgent(t *testing.T, raw []byte) []byte {
	t.Helper()
	var output bytes.Buffer
	gz := gzip.NewWriter(&output)
	if _, err := gz.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func readTarHeader(t *testing.T, raw []byte, name string) *tar.Header {
	t.Helper()
	tr := tar.NewReader(bytes.NewReader(raw))
	for {
		header, err := tr.Next()
		if err != nil {
			t.Fatal(err)
		}
		if header.Name == name {
			return header
		}
	}
}

func batServerResultText(rows []model.JobVerificationRequest, err error) string {
	var b strings.Builder
	if err != nil {
		b.WriteString(err.Error())
	}
	for _, row := range rows {
		b.WriteString(row.RuleID)
		b.WriteString(row.Command)
		b.WriteString(row.StdoutExcerpt)
		b.WriteString(row.StderrExcerpt)
	}
	return b.String()
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func batServerSmallBundle(t *testing.T) ([]byte, string) {
	t.Helper()
	binary := []byte("binary")
	sum := sha256.Sum256(binary)
	return batServerExecutorBundle(t, map[string][]byte{
		"bat-server/linux-amd64/bat-server.tar.gz": batServerInnerGzip(t, []batServerTarEntry{
			{name: "bat-server-linux-x86_64/bat-server", mode: 0o755, body: binary},
		}),
	}), hex.EncodeToString(sum[:])
}

func TestBATServerExecutorRefusesPortHeldByAnotherProgram(t *testing.T) {
	bundle, binaryHash := batServerSmallBundle(t)
	executor, job, downloads, actions := batServerTestRun(t, "linux", "amd64", bundle, binaryHash, "active")
	executor.listen = func(string, string) (net.Listener, error) {
		return nil, errors.New("address in use")
	}

	rows, err := executor.Run(t.Context(), job)
	var rejected *rejectError
	if !errors.As(err, &rejected) || rejected.Detail != "port 19876 is already in use by another program, BAT Server cannot start. Stop the program using this port, then redeploy BAT Server." ||
		len(rows) != 0 || *downloads != 0 {
		t.Fatalf("rows=%v err=%v downloads=%d", rows, err, *downloads)
	}
	if strings.Join(*actions, "\n") != "--user is-active clawctl-bat-server.service" {
		t.Fatalf("actions=%v", *actions)
	}
	if entries, err := os.ReadDir(executor.deps.fsRoot); err != nil || len(entries) != 0 {
		t.Fatalf("a refused install wrote files: %v err=%v", entries, err)
	}
}

func TestBATServerExecutorRefusesForeignFileAtLinkPath(t *testing.T) {
	bundle, binaryHash := batServerSmallBundle(t)
	for _, tc := range []struct {
		name  string
		place func(path string) error
		check func(path string) bool
	}{
		{"regular file", func(path string) error { return os.WriteFile(path, []byte("foreign"), 0o644) },
			func(path string) bool { body, err := os.ReadFile(path); return err == nil && string(body) == "foreign" }},
		{"link elsewhere", func(path string) error { return os.Symlink("/etc/foreign.service", path) },
			func(path string) bool {
				target, err := os.Readlink(path)
				return err == nil && target == "/etc/foreign.service"
			}},
		{"directory", func(path string) error { return os.Mkdir(path, 0o755) },
			func(path string) bool { info, err := os.Lstat(path); return err == nil && info.IsDir() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			executor, job, downloads, actions := batServerTestRun(t, "linux", "amd64", bundle, binaryHash, "active")
			link := executor.deps.fsPath("/home/bat-test/.config/systemd/user/clawctl-bat-server.service")
			if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := tc.place(link); err != nil {
				t.Fatal(err)
			}

			rows, err := executor.Run(t.Context(), job)
			var rejected *rejectError
			if !errors.As(err, &rejected) ||
				rejected.Detail != "/home/bat-test/.config/systemd/user/clawctl-bat-server.service is not a link created by AI-Intune, BAT Server cannot be installed. Remove this file, then redeploy BAT Server." ||
				len(rows) != 0 || *downloads != 0 {
				t.Fatalf("rows=%v err=%v downloads=%d", rows, err, *downloads)
			}
			if strings.Join(*actions, "\n") != "--user is-active clawctl-bat-server.service" {
				t.Fatalf("actions=%v", *actions)
			}
			if !tc.check(link) {
				t.Fatal("the foreign file at the link path changed")
			}
		})
	}
}

func TestBATServerExecutorHandshakeRetriesAndNeverRecordsItsError(t *testing.T) {
	const sentinel = "handshake-token-sentinel"
	bundle, binaryHash := batServerSmallBundle(t)

	t.Run("succeeds on the third attempt", func(t *testing.T) {
		executor, job, _, _ := batServerTestRun(t, "linux", "amd64", bundle, binaryHash, "active")
		sleeps, attempts := 0, 0
		executor.deps.sleep = func(ctx context.Context, delay time.Duration) error {
			if delay != 500*time.Millisecond {
				t.Errorf("delay=%v", delay)
			}
			sleeps++
			return ctx.Err()
		}
		executor.handshake = func(_ context.Context, tokenFile, dataDir string) error {
			attempts++
			if tokenFile != "/home/bat-test/.local/share/clawctl/bat-server/credentials/token" ||
				dataDir != "/home/bat-test/.local/share/clawctl/bat-server/data" {
				t.Errorf("handshake paths %q %q", tokenFile, dataDir)
			}
			if attempts <= 2 {
				return errors.New("connection refused " + sentinel)
			}
			return nil
		}
		rows, err := executor.Run(t.Context(), job)
		if err != nil || !allPassed(rows) || attempts != 3 || sleeps != 2 {
			t.Fatalf("rows=%v attempts=%d sleeps=%d err=%v", rows, attempts, sleeps, err)
		}
		if strings.Contains(batServerResultText(rows, err), sentinel) {
			t.Fatal("handshake error reached the evidence")
		}
	})

	t.Run("never succeeds", func(t *testing.T) {
		executor, job, _, _ := batServerTestRun(t, "linux", "amd64", bundle, binaryHash, "active")
		sleeps, attempts := 0, 0
		executor.deps.sleep = func(ctx context.Context, _ time.Duration) error {
			sleeps++
			return ctx.Err()
		}
		executor.handshake = func(context.Context, string, string) error {
			attempts++
			return errors.New("connection refused " + sentinel)
		}
		rows, err := executor.Run(t.Context(), job)
		if err != nil || allPassed(rows) || attempts != 30 || sleeps != 29 {
			t.Fatalf("rows=%v attempts=%d sleeps=%d err=%v", rows, attempts, sleeps, err)
		}
		if strings.Contains(batServerResultText(rows, err), sentinel) {
			t.Fatal("handshake error reached the evidence")
		}
		last := rows[len(rows)-1]
		if last.RuleID != "bat-server-endpoint" || last.Passed || last.Command != "bat-remote auth 127.0.0.1:19876" ||
			last.StdoutExcerpt != "" || last.ExitCode != 1 ||
			last.StderrExcerpt != "no BAT Server responding with local token and certificate on 127.0.0.1:19876" {
			t.Fatalf("endpoint row=%+v", last)
		}
	})

	t.Run("stops when the wait is cancelled", func(t *testing.T) {
		executor, job, _, _ := batServerTestRun(t, "linux", "amd64", bundle, binaryHash, "active")
		attempts := 0
		executor.deps.sleep = func(context.Context, time.Duration) error { return context.Canceled }
		executor.handshake = func(context.Context, string, string) error {
			attempts++
			return errors.New(sentinel)
		}
		rows, err := executor.Run(t.Context(), job)
		if err != nil || allPassed(rows) || attempts != 1 {
			t.Fatalf("rows=%v attempts=%d err=%v", rows, attempts, err)
		}
	})
}
