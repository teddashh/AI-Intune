package web

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var agentBundleFilenames = map[string]string{
	"amd64": linuxAgentBundleSpecs[0].Filename,
	"arm64": linuxAgentBundleSpecs[1].Filename,
}

func linuxAgentBinary(arch string) []byte {
	machine := uint16(62)
	if arch == "arm64" {
		machine = 183
	}
	elf := make([]byte, 64)
	copy(elf, "\x7fELF")
	elf[4], elf[5] = 2, 1
	binary.LittleEndian.PutUint16(elf[18:20], machine)
	return elf
}

func darwinAgentBinary(arch string) []byte {
	macho := make([]byte, 64)
	binary.LittleEndian.PutUint32(macho[0:4], machoMagic64)
	cpu := uint32(machoCPUTypeX86_64)
	if arch == "arm64" {
		cpu = machoCPUTypeARM64
	}
	binary.LittleEndian.PutUint32(macho[4:8], cpu)
	binary.LittleEndian.PutUint32(macho[12:16], machoFileExecute)
	return macho
}

func darwinInstallerFixture() []byte {
	return []byte("#!/usr/bin/env bash\nset -euo pipefail\n[[ \"$(uname -s)\" == Darwin ]] || exit 1\nlaunchctl bootstrap gui/$UID clawctl-agent.plist\n# com.clawctl.agent --hub\n")
}

func darwinPlistFixture() []byte {
	return []byte(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>com.clawctl.agent</string>
  <key>ProgramArguments</key>
  <array><string>@@CLAWCTL_AGENT_BIN@@</string></array>
  <key>KeepAlive</key>
  <true/>
  <key>StandardOutPath</key>
  <string>@@CLAWCTL_AGENT_LOG@@</string>
</dict>
</plist>
`)
}

func windowsAgentBinary(arch string) []byte {
	pe := make([]byte, 256)
	pe[0], pe[1] = 'M', 'Z'
	binary.LittleEndian.PutUint32(pe[0x3C:], 0x80)
	copy(pe[0x80:], "PE\x00\x00")
	machine := uint16(peMachineAMD64)
	if arch == "arm64" {
		machine = peMachineARM64
	}
	binary.LittleEndian.PutUint16(pe[0x84:], machine)
	binary.LittleEndian.PutUint16(pe[0x80+22:], peFileExecutableImage)
	return pe
}

func windowsInstallerFixture() []byte {
	return []byte("This installer requires Windows.\nRegister-ScheduledTask -Xml clawctl-agent.task.xml\n# clawctl-agent --hub --require-platform-evidence\n")
}

func windowsTaskFixture() []byte {
	return []byte(`<?xml version="1.0" encoding="UTF-8"?>
<Task version="1.4" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <RegistrationInfo><URI>\clawctl\clawctl-agent</URI></RegistrationInfo>
  <Triggers><LogonTrigger><Enabled>true</Enabled></LogonTrigger></Triggers>
  <Principals><Principal id="Author"><LogonType>InteractiveToken</LogonType><RunLevel>LeastPrivilege</RunLevel></Principal></Principals>
  <Actions><Exec><Command>@@CLAWCTL_AGENT_BIN@@</Command></Exec></Actions>
</Task>
`)
}

func writeAgentBundleArchive(t *testing.T, dir, filename string, entries []struct {
	name string
	mode int64
	body []byte
}) []byte {
	t.Helper()
	var compressed bytes.Buffer
	gz := gzip.NewWriter(&compressed)
	gz.Header.ModTime = time.Unix(0, 0)
	gz.Header.OS = 255
	tw := tar.NewWriter(gz)
	for _, entry := range entries {
		if err := tw.WriteHeader(&tar.Header{
			Name: entry.name, Mode: entry.mode, Size: int64(len(entry.body)),
			ModTime: time.Unix(0, 0), Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(entry.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, filename), compressed.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return append([]byte(nil), compressed.Bytes()...)
}

func writeAgentBundleFixture(t *testing.T, dir, arch string) []byte {
	t.Helper()
	return writeAgentBundleArchive(t, dir, agentBundleFilenames[arch], []struct {
		name string
		mode int64
		body []byte
	}{
		{name: "VERSION", mode: 0o644, body: []byte("release-123\n")},
		{name: "clawctl-agent", mode: 0o755, body: linuxAgentBinary(arch)},
		{name: "clawctl-agent.service", mode: 0o644, body: []byte("[Unit]\nDescription=clawctl agent\n")},
		{name: "clawctl-hermes.service", mode: 0o644, body: []byte("[Unit]\nDescription=Hermes Agent\n")},
		{name: "install-agent.sh", mode: 0o755, body: []byte("#!/usr/bin/env bash\nset -euo pipefail\n")},
		{name: "openclaw-gateway.service", mode: 0o644, body: []byte("[Unit]\nDescription=OpenClaw Gateway\n")},
	})
}

func writeDarwinAgentBundleFixture(t *testing.T, dir, arch string) []byte {
	t.Helper()
	filename := "clawctl-agent-bootstrap-darwin-" + arch + ".tar.gz"
	return writeAgentBundleArchive(t, dir, filename, []struct {
		name string
		mode int64
		body []byte
	}{
		{name: "VERSION", mode: 0o644, body: []byte("release-123\n")},
		{name: "clawctl-agent", mode: 0o755, body: darwinAgentBinary(arch)},
		{name: "clawctl-agent.plist", mode: 0o644, body: darwinPlistFixture()},
		{name: "install-agent-macos.sh", mode: 0o755, body: darwinInstallerFixture()},
	})
}

func writeWindowsAgentBundleFixture(t *testing.T, dir, arch string) []byte {
	t.Helper()
	filename := "clawctl-agent-bootstrap-windows-" + arch + ".tar.gz"
	return writeAgentBundleArchive(t, dir, filename, []struct {
		name string
		mode int64
		body []byte
	}{
		{name: "VERSION", mode: 0o644, body: []byte("release-123\n")},
		{name: "clawctl-agent.exe", mode: 0o755, body: windowsAgentBinary(arch)},
		{name: "clawctl-agent.task.xml", mode: 0o644, body: windowsTaskFixture()},
		{name: "install-agent-windows.ps1", mode: 0o755, body: windowsInstallerFixture()},
	})
}

func configuredAgentBundleServer(t *testing.T) (*Server, string, map[string][]byte) {
	t.Helper()
	s, _ := newServer(t)
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	contents := map[string][]byte{
		"amd64": writeAgentBundleFixture(t, dir, "amd64"),
		"arm64": writeAgentBundleFixture(t, dir, "arm64"),
	}
	if err := s.SetAgentBundles(dir, "release-123"); err != nil {
		t.Fatal(err)
	}
	return s, dir, contents
}

func TestAgentBundlesAppearOnEnrollmentPageAndDownloadExactBytes(t *testing.T) {
	s, _, contents := configuredAgentBundleServer(t)
	body := get(t, s, "/machines/enrollment")
	for _, arch := range []string{"amd64", "arm64"} {
		sum := sha256.Sum256(contents[arch])
		for _, want := range []string{
			"/downloads/agent/" + arch,
			"linux/" + arch,
			hex.EncodeToString(sum[:]),
			"release-123",
		} {
			if !strings.Contains(body, want) {
				t.Errorf("enrollment page missing %q", want)
			}
		}
	}

	mux := http.NewServeMux()
	s.Routes(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/downloads/agent/amd64", nil))
	if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), contents["amd64"]) {
		t.Fatalf("download status=%d bytes_match=%t body=%q", rec.Code,
			bytes.Equal(rec.Body.Bytes(), contents["amd64"]), rec.Body.String())
	}
	sum := sha256.Sum256(contents["amd64"])
	if got := rec.Header().Get("Content-Type"); got != "application/gzip" {
		t.Errorf("Content-Type=%q", got)
	}
	if got := rec.Header().Get("Content-Disposition"); got != `attachment; filename="clawctl-agent-bootstrap-linux-amd64.tar.gz"` {
		t.Errorf("Content-Disposition=%q", got)
	}
	if got := rec.Header().Get("ETag"); got != `"sha256-`+hex.EncodeToString(sum[:])+`"` {
		t.Errorf("ETag=%q", got)
	}
	if got := rec.Header().Get("Cache-Control"); got != "private, no-store" {
		t.Errorf("Cache-Control=%q", got)
	}

	notFound := httptest.NewRecorder()
	mux.ServeHTTP(notFound, httptest.NewRequest(http.MethodGet, "/downloads/agent/ppc64", nil))
	if notFound.Code != http.StatusNotFound {
		t.Fatalf("unsupported architecture status=%d", notFound.Code)
	}
}

func TestAgentBundleDownloadRejectsChangedRelease(t *testing.T) {
	s, dir, _ := configuredAgentBundleServer(t)
	path := filepath.Join(dir, agentBundleFilenames["amd64"])
	if err := os.WriteFile(path, []byte("replaced"), 0o644); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	s.Routes(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/downloads/agent/amd64", nil))
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "Agent bootstrap 下載失敗") {
		t.Fatalf("changed release status=%d body=%q", rec.Code, rec.Body.String())
	}
}

func TestSetAgentBundlesRejectsSymlinkWrongArchitectureAndPartialRelease(t *testing.T) {
	tests := []struct {
		name  string
		alter func(t *testing.T, dir string)
	}{
		{name: "symlink", alter: func(t *testing.T, dir string) {
			t.Helper()
			target := filepath.Join(dir, agentBundleFilenames["amd64"])
			link := filepath.Join(dir, agentBundleFilenames["arm64"])
			if err := os.Remove(link); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, link); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "hardlink", alter: func(t *testing.T, dir string) {
			t.Helper()
			target := filepath.Join(dir, agentBundleFilenames["amd64"])
			link := filepath.Join(dir, agentBundleFilenames["arm64"])
			if err := os.Remove(link); err != nil {
				t.Fatal(err)
			}
			if err := os.Link(target, link); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "wrong architecture", alter: func(t *testing.T, dir string) {
			t.Helper()
			writeAgentBundleFixture(t, dir, "amd64")
			amd64 := filepath.Join(dir, agentBundleFilenames["amd64"])
			arm64 := filepath.Join(dir, agentBundleFilenames["arm64"])
			body, err := os.ReadFile(amd64)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(arm64, body, 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "missing architecture", alter: func(t *testing.T, dir string) {
			t.Helper()
			if err := os.Remove(filepath.Join(dir, agentBundleFilenames["arm64"])); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s, _ := newServer(t)
			dir := t.TempDir()
			if err := os.Chmod(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			writeAgentBundleFixture(t, dir, "amd64")
			writeAgentBundleFixture(t, dir, "arm64")
			test.alter(t, dir)
			if err := s.SetAgentBundles(dir, "release-123"); err == nil {
				t.Fatal("invalid bundle release accepted")
			}
		})
	}

	s, _ := newServer(t)
	if err := s.SetAgentBundles(t.TempDir(), "../release"); err == nil {
		t.Fatal("unsafe version accepted")
	}
}

func configuredDarwinAgentBundleServer(t *testing.T) (*Server, string, map[string][]byte) {
	t.Helper()
	s, dir, contents := configuredAgentBundleServer(t)
	contents["darwin-amd64"] = writeDarwinAgentBundleFixture(t, dir, "amd64")
	contents["darwin-arm64"] = writeDarwinAgentBundleFixture(t, dir, "arm64")
	if err := s.SetAgentBundles(dir, "release-123"); err != nil {
		t.Fatal(err)
	}
	return s, dir, contents
}

func checkLinuxOnlyAgentBundlesDoNotAdvertiseDarwinDownloads(t *testing.T) {
	s, _, _ := configuredAgentBundleServer(t)
	body := get(t, s, "/machines/enrollment")
	for _, leak := range []string{
		"darwin/", "/downloads/agent/darwin-", "install-agent-macos.sh",
		"windows/", "/downloads/agent/windows-", "install-agent-windows.ps1",
	} {
		if strings.Contains(body, leak) {
			t.Errorf("linux-only enrollment advertised %q", leak)
		}
	}
	mux := http.NewServeMux()
	s.Routes(mux)
	missing := httptest.NewRecorder()
	mux.ServeHTTP(missing, httptest.NewRequest(http.MethodGet, "/downloads/agent/darwin-arm64", nil))
	if missing.Code != http.StatusNotFound {
		t.Fatalf("unpublished darwin download status=%d", missing.Code)
	}
	rec, _ := createEnrollment(t, s, "linux-host", "linux bundle install")
	if rec.Code != http.StatusOK {
		t.Fatalf("enrollment status=%d body=%s", rec.Code, rec.Body.String())
	}
	token := rec.Body.String()
	if !strings.Contains(token, "./install-agent.sh --hub") {
		t.Fatal("linux token page missing linux installer command")
	}
	if strings.Contains(token, "install-agent-macos.sh") || strings.Contains(token, "darwin/") ||
		strings.Contains(token, "install-agent-windows.ps1") || strings.Contains(token, "windows/") {
		t.Fatal("linux-only token page advertised another OS installer")
	}
}

func checkDarwinAgentBundlesAreSelectableThroughExistingDownloadRoute(t *testing.T) {
	s, _, contents := configuredDarwinAgentBundleServer(t)
	body := get(t, s, "/machines/enrollment")
	for _, arch := range []string{"amd64", "arm64"} {
		linuxSum := sha256.Sum256(contents[arch])
		darwinSum := sha256.Sum256(contents["darwin-"+arch])
		for _, want := range []string{
			"/downloads/agent/" + arch,
			"/downloads/agent/darwin-" + arch,
			"linux/" + arch,
			"darwin/" + arch,
			hex.EncodeToString(linuxSum[:]),
			hex.EncodeToString(darwinSum[:]),
			"release-123",
		} {
			if !strings.Contains(body, want) {
				t.Errorf("enrollment page missing %q", want)
			}
		}
	}

	mux := http.NewServeMux()
	s.Routes(mux)
	assertBundleDownload(t, mux, "/downloads/agent/amd64", contents["amd64"],
		`attachment; filename="clawctl-agent-bootstrap-linux-amd64.tar.gz"`)
	assertBundleDownload(t, mux, "/downloads/agent/linux-amd64", contents["amd64"],
		`attachment; filename="clawctl-agent-bootstrap-linux-amd64.tar.gz"`)
	assertBundleDownload(t, mux, "/downloads/agent/darwin-arm64", contents["darwin-arm64"],
		`attachment; filename="clawctl-agent-bootstrap-darwin-arm64.tar.gz"`)
	assertBundleDownload(t, mux, "/downloads/agent/darwin-amd64", contents["darwin-amd64"],
		`attachment; filename="clawctl-agent-bootstrap-darwin-amd64.tar.gz"`)

	notFound := httptest.NewRecorder()
	mux.ServeHTTP(notFound, httptest.NewRequest(http.MethodGet, "/downloads/agent/windows-amd64", nil))
	if notFound.Code != http.StatusNotFound {
		t.Fatalf("unsupported platform status=%d", notFound.Code)
	}
}

func assertBundleDownload(t *testing.T, mux *http.ServeMux, path string, want []byte, disposition string) {
	t.Helper()
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), want) {
		t.Fatalf("%s status=%d bytes_match=%t body=%q", path, rec.Code,
			bytes.Equal(rec.Body.Bytes(), want), rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "application/gzip" {
		t.Errorf("%s Content-Type=%q", path, got)
	}
	if got := rec.Header().Get("Content-Disposition"); got != disposition {
		t.Errorf("%s Content-Disposition=%q", path, got)
	}
	sum := sha256.Sum256(want)
	if got := rec.Header().Get("ETag"); got != `"sha256-`+hex.EncodeToString(sum[:])+`"` {
		t.Errorf("%s ETag=%q", path, got)
	}
}

func checkTokenPageShowsDarwinInstallerWhenDarwinBundlesArePublished(t *testing.T) {
	s, _, _ := configuredDarwinAgentBundleServer(t)
	s.SetHubBase("http://100.64.0.1:8787")
	rec, _ := createEnrollment(t, s, "mac-host", "darwin bundle install")
	if rec.Code != http.StatusOK {
		t.Fatalf("enrollment status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		"./install-agent.sh --hub http://100.64.0.1:8787",
		"./install-agent-macos.sh --hub http://100.64.0.1:8787",
		"darwin/arm64",
		"/downloads/agent/darwin-arm64",
		"/downloads/agent/amd64",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("token page missing %q", want)
		}
	}
	if strings.Contains(body, "--token '") {
		t.Fatal("token page embedded the secret in the installer command")
	}
}

func checkSetAgentBundlesRejectsInvalidDarwinRelease(t *testing.T) {
	tests := []struct {
		name  string
		alter func(t *testing.T, dir string)
	}{
		{name: "partial darwin", alter: func(t *testing.T, dir string) {
			t.Helper()
			writeDarwinAgentBundleFixture(t, dir, "arm64")
		}},
		{name: "darwin symlink", alter: func(t *testing.T, dir string) {
			t.Helper()
			writeDarwinAgentBundleFixture(t, dir, "amd64")
			writeDarwinAgentBundleFixture(t, dir, "arm64")
			link := filepath.Join(dir, "clawctl-agent-bootstrap-darwin-arm64.tar.gz")
			if err := os.Remove(link); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(dir, "clawctl-agent-bootstrap-darwin-amd64.tar.gz"), link); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "darwin wrong architecture", alter: func(t *testing.T, dir string) {
			t.Helper()
			writeDarwinAgentBundleFixture(t, dir, "amd64")
			amd64 := filepath.Join(dir, "clawctl-agent-bootstrap-darwin-amd64.tar.gz")
			arm64 := filepath.Join(dir, "clawctl-agent-bootstrap-darwin-arm64.tar.gz")
			body, err := os.ReadFile(amd64)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(arm64, body, 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "darwin linux layout", alter: func(t *testing.T, dir string) {
			t.Helper()
			writeDarwinAgentBundleFixture(t, dir, "amd64")
			linux := writeAgentBundleFixture(t, dir, "arm64")
			if err := os.WriteFile(filepath.Join(dir, "clawctl-agent-bootstrap-darwin-arm64.tar.gz"), linux, 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "darwin elf binary", alter: func(t *testing.T, dir string) {
			t.Helper()
			writeDarwinAgentBundleFixture(t, dir, "amd64")
			writeAgentBundleArchive(t, dir, "clawctl-agent-bootstrap-darwin-arm64.tar.gz", []struct {
				name string
				mode int64
				body []byte
			}{
				{name: "VERSION", mode: 0o644, body: []byte("release-123\n")},
				{name: "clawctl-agent", mode: 0o755, body: linuxAgentBinary("arm64")},
				{name: "clawctl-agent.plist", mode: 0o644, body: darwinPlistFixture()},
				{name: "install-agent-macos.sh", mode: 0o755, body: darwinInstallerFixture()},
			})
		}},
		{name: "darwin shared library is not an executable", alter: func(t *testing.T, dir string) {
			t.Helper()
			writeDarwinAgentBundleFixture(t, dir, "amd64")
			library := darwinAgentBinary("arm64")
			binary.LittleEndian.PutUint32(library[12:16], 6) // MH_DYLIB
			writeAgentBundleArchive(t, dir, "clawctl-agent-bootstrap-darwin-arm64.tar.gz", []struct {
				name string
				mode int64
				body []byte
			}{
				{name: "VERSION", mode: 0o644, body: []byte("release-123\n")},
				{name: "clawctl-agent", mode: 0o755, body: library},
				{name: "clawctl-agent.plist", mode: 0o644, body: darwinPlistFixture()},
				{name: "install-agent-macos.sh", mode: 0o755, body: darwinInstallerFixture()},
			})
		}},
		{name: "darwin missing plist", alter: func(t *testing.T, dir string) {
			t.Helper()
			writeDarwinAgentBundleFixture(t, dir, "amd64")
			writeAgentBundleArchive(t, dir, "clawctl-agent-bootstrap-darwin-arm64.tar.gz", []struct {
				name string
				mode int64
				body []byte
			}{
				{name: "VERSION", mode: 0o644, body: []byte("release-123\n")},
				{name: "clawctl-agent", mode: 0o755, body: darwinAgentBinary("arm64")},
				{name: "install-agent-macos.sh", mode: 0o755, body: darwinInstallerFixture()},
			})
		}},
		{name: "darwin wrong installer", alter: func(t *testing.T, dir string) {
			t.Helper()
			writeDarwinAgentBundleFixture(t, dir, "amd64")
			writeAgentBundleArchive(t, dir, "clawctl-agent-bootstrap-darwin-arm64.tar.gz", []struct {
				name string
				mode int64
				body []byte
			}{
				{name: "VERSION", mode: 0o644, body: []byte("release-123\n")},
				{name: "clawctl-agent", mode: 0o755, body: darwinAgentBinary("arm64")},
				{name: "clawctl-agent.plist", mode: 0o644, body: darwinPlistFixture()},
				{name: "install-agent-macos.sh", mode: 0o755, body: []byte("#!/usr/bin/env bash\necho linux only\n")},
			})
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s, _ := newServer(t)
			dir := t.TempDir()
			if err := os.Chmod(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			writeAgentBundleFixture(t, dir, "amd64")
			writeAgentBundleFixture(t, dir, "arm64")
			test.alter(t, dir)
			if err := s.SetAgentBundles(dir, "release-123"); err == nil {
				t.Fatal("invalid darwin bundle release accepted")
			}
		})
	}
}

func TestDarwinAgentBundleDelivery(t *testing.T) {
	t.Run("DarwinAgentBundlesAreSelectableThroughExistingDownloadRoute", checkDarwinAgentBundlesAreSelectableThroughExistingDownloadRoute)
	t.Run("LinuxOnlyAgentBundlesDoNotAdvertiseDarwinDownloads", checkLinuxOnlyAgentBundlesDoNotAdvertiseDarwinDownloads)
	t.Run("SetAgentBundlesRejectsInvalidDarwinRelease", checkSetAgentBundlesRejectsInvalidDarwinRelease)
	t.Run("TokenPageShowsDarwinInstallerWhenDarwinBundlesArePublished", checkTokenPageShowsDarwinInstallerWhenDarwinBundlesArePublished)
}

func configuredWindowsAgentBundleServer(t *testing.T) (*Server, string, map[string][]byte) {
	t.Helper()
	s, dir, contents := configuredAgentBundleServer(t)
	contents["windows-amd64"] = writeWindowsAgentBundleFixture(t, dir, "amd64")
	contents["windows-arm64"] = writeWindowsAgentBundleFixture(t, dir, "arm64")
	if err := s.SetAgentBundles(dir, "release-123"); err != nil {
		t.Fatal(err)
	}
	return s, dir, contents
}

func checkWindowsAgentBundlesAreSelectableThroughExistingDownloadRoute(t *testing.T) {
	s, _, contents := configuredWindowsAgentBundleServer(t)
	body := get(t, s, "/machines/enrollment")
	for _, arch := range []string{"amd64", "arm64"} {
		linuxSum := sha256.Sum256(contents[arch])
		windowsSum := sha256.Sum256(contents["windows-"+arch])
		for _, want := range []string{
			"/downloads/agent/" + arch,
			"/downloads/agent/windows-" + arch,
			"linux/" + arch,
			"windows/" + arch,
			hex.EncodeToString(linuxSum[:]),
			hex.EncodeToString(windowsSum[:]),
			"release-123",
		} {
			if !strings.Contains(body, want) {
				t.Errorf("enrollment page missing %q", want)
			}
		}
	}

	mux := http.NewServeMux()
	s.Routes(mux)
	assertBundleDownload(t, mux, "/downloads/agent/amd64", contents["amd64"],
		`attachment; filename="clawctl-agent-bootstrap-linux-amd64.tar.gz"`)
	assertBundleDownload(t, mux, "/downloads/agent/windows-arm64", contents["windows-arm64"],
		`attachment; filename="clawctl-agent-bootstrap-windows-arm64.tar.gz"`)
	assertBundleDownload(t, mux, "/downloads/agent/windows-amd64", contents["windows-amd64"],
		`attachment; filename="clawctl-agent-bootstrap-windows-amd64.tar.gz"`)
}

func checkTokenPageShowsWindowsInstallerWhenWindowsBundlesArePublished(t *testing.T) {
	s, _, _ := configuredWindowsAgentBundleServer(t)
	s.SetHubBase("http://100.64.0.1:8787")
	rec, _ := createEnrollment(t, s, "windows-host", "windows bundle install")
	if rec.Code != http.StatusOK {
		t.Fatalf("enrollment status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		"./install-agent.sh --hub http://100.64.0.1:8787",
		`.\install-agent-windows.ps1 --hub http://100.64.0.1:8787`,
		"windows/arm64",
		"/downloads/agent/windows-arm64",
		"/downloads/agent/amd64",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("token page missing %q", want)
		}
	}
	if strings.Contains(body, "--token '") {
		t.Fatal("token page embedded the secret in the installer command")
	}
	if strings.Contains(body, "install-agent-macos.sh") {
		t.Fatal("windows-only token page advertised a macOS installer")
	}
}

func checkSetAgentBundlesRejectsInvalidWindowsRelease(t *testing.T) {
	tests := []struct {
		name  string
		alter func(t *testing.T, dir string)
	}{
		{name: "partial windows", alter: func(t *testing.T, dir string) {
			t.Helper()
			writeWindowsAgentBundleFixture(t, dir, "arm64")
		}},
		{name: "windows symlink", alter: func(t *testing.T, dir string) {
			t.Helper()
			writeWindowsAgentBundleFixture(t, dir, "amd64")
			writeWindowsAgentBundleFixture(t, dir, "arm64")
			link := filepath.Join(dir, "clawctl-agent-bootstrap-windows-arm64.tar.gz")
			if err := os.Remove(link); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(dir, "clawctl-agent-bootstrap-windows-amd64.tar.gz"), link); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "windows wrong architecture", alter: func(t *testing.T, dir string) {
			t.Helper()
			writeWindowsAgentBundleFixture(t, dir, "amd64")
			amd64 := filepath.Join(dir, "clawctl-agent-bootstrap-windows-amd64.tar.gz")
			arm64 := filepath.Join(dir, "clawctl-agent-bootstrap-windows-arm64.tar.gz")
			body, err := os.ReadFile(amd64)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(arm64, body, 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "windows linux layout", alter: func(t *testing.T, dir string) {
			t.Helper()
			writeWindowsAgentBundleFixture(t, dir, "amd64")
			linux := writeAgentBundleFixture(t, dir, "arm64")
			if err := os.WriteFile(filepath.Join(dir, "clawctl-agent-bootstrap-windows-arm64.tar.gz"), linux, 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "windows elf binary", alter: func(t *testing.T, dir string) {
			t.Helper()
			writeWindowsAgentBundleFixture(t, dir, "amd64")
			writeAgentBundleArchive(t, dir, "clawctl-agent-bootstrap-windows-arm64.tar.gz", []struct {
				name string
				mode int64
				body []byte
			}{
				{name: "VERSION", mode: 0o644, body: []byte("release-123\n")},
				{name: "clawctl-agent.exe", mode: 0o755, body: linuxAgentBinary("arm64")},
				{name: "clawctl-agent.task.xml", mode: 0o644, body: windowsTaskFixture()},
				{name: "install-agent-windows.ps1", mode: 0o755, body: windowsInstallerFixture()},
			})
		}},
		{name: "windows dll is not an executable", alter: func(t *testing.T, dir string) {
			t.Helper()
			writeWindowsAgentBundleFixture(t, dir, "amd64")
			library := windowsAgentBinary("arm64")
			binary.LittleEndian.PutUint16(library[0x80+22:], peFileExecutableImage|peFileDLL)
			writeAgentBundleArchive(t, dir, "clawctl-agent-bootstrap-windows-arm64.tar.gz", []struct {
				name string
				mode int64
				body []byte
			}{
				{name: "VERSION", mode: 0o644, body: []byte("release-123\n")},
				{name: "clawctl-agent.exe", mode: 0o755, body: library},
				{name: "clawctl-agent.task.xml", mode: 0o644, body: windowsTaskFixture()},
				{name: "install-agent-windows.ps1", mode: 0o755, body: windowsInstallerFixture()},
			})
		}},
		{name: "windows missing task xml", alter: func(t *testing.T, dir string) {
			t.Helper()
			writeWindowsAgentBundleFixture(t, dir, "amd64")
			writeAgentBundleArchive(t, dir, "clawctl-agent-bootstrap-windows-arm64.tar.gz", []struct {
				name string
				mode int64
				body []byte
			}{
				{name: "VERSION", mode: 0o644, body: []byte("release-123\n")},
				{name: "clawctl-agent.exe", mode: 0o755, body: windowsAgentBinary("arm64")},
				{name: "install-agent-windows.ps1", mode: 0o755, body: windowsInstallerFixture()},
			})
		}},
		{name: "windows wrong installer", alter: func(t *testing.T, dir string) {
			t.Helper()
			writeWindowsAgentBundleFixture(t, dir, "amd64")
			writeAgentBundleArchive(t, dir, "clawctl-agent-bootstrap-windows-arm64.tar.gz", []struct {
				name string
				mode int64
				body []byte
			}{
				{name: "VERSION", mode: 0o644, body: []byte("release-123\n")},
				{name: "clawctl-agent.exe", mode: 0o755, body: windowsAgentBinary("arm64")},
				{name: "clawctl-agent.task.xml", mode: 0o644, body: windowsTaskFixture()},
				{name: "install-agent-windows.ps1", mode: 0o755, body: []byte("#!/usr/bin/env bash\necho linux only\n")},
			})
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s, _ := newServer(t)
			dir := t.TempDir()
			if err := os.Chmod(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			writeAgentBundleFixture(t, dir, "amd64")
			writeAgentBundleFixture(t, dir, "arm64")
			test.alter(t, dir)
			if err := s.SetAgentBundles(dir, "release-123"); err == nil {
				t.Fatal("invalid windows bundle release accepted")
			}
		})
	}
}

func TestWindowsAgentBundleDelivery(t *testing.T) {
	t.Run("WindowsAgentBundlesAreSelectableThroughExistingDownloadRoute", checkWindowsAgentBundlesAreSelectableThroughExistingDownloadRoute)
	t.Run("LinuxOnlyAgentBundlesDoNotAdvertiseWindowsDownloads", checkLinuxOnlyAgentBundlesDoNotAdvertiseDarwinDownloads)
	t.Run("SetAgentBundlesRejectsInvalidWindowsRelease", checkSetAgentBundlesRejectsInvalidWindowsRelease)
	t.Run("TokenPageShowsWindowsInstallerWhenWindowsBundlesArePublished", checkTokenPageShowsWindowsInstallerWhenWindowsBundlesArePublished)
}
