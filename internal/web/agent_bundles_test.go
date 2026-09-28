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

func writeAgentBundleFixture(t *testing.T, dir, arch string) []byte {
	t.Helper()
	var compressed bytes.Buffer
	gz := gzip.NewWriter(&compressed)
	gz.Header.ModTime = time.Unix(0, 0)
	gz.Header.OS = 255
	tw := tar.NewWriter(gz)
	machine := uint16(62)
	if arch == "arm64" {
		machine = 183
	}
	elf := make([]byte, 64)
	copy(elf, "\x7fELF")
	elf[4], elf[5] = 2, 1
	binary.LittleEndian.PutUint16(elf[18:20], machine)
	entries := []struct {
		name string
		mode int64
		body []byte
	}{
		{name: "VERSION", mode: 0o644, body: []byte("release-123\n")},
		{name: "clawctl-agent", mode: 0o755, body: elf},
		{name: "clawctl-agent.service", mode: 0o644, body: []byte("[Unit]\nDescription=clawctl agent\n")},
		{name: "clawctl-hermes.service", mode: 0o644, body: []byte("[Unit]\nDescription=Hermes Agent\n")},
		{name: "install-agent.sh", mode: 0o755, body: []byte("#!/usr/bin/env bash\nset -euo pipefail\n")},
		{name: "openclaw-gateway.service", mode: 0o644, body: []byte("[Unit]\nDescription=OpenClaw Gateway\n")},
	}
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
	filename := agentBundleFilenames[arch]
	if err := os.WriteFile(filepath.Join(dir, filename), compressed.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return append([]byte(nil), compressed.Bytes()...)
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
