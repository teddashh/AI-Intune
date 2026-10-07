package web

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/model"
	"github.com/teddashh/AI-Intune/internal/store"
)

type keyedTestEntry struct {
	Header tar.Header
	Body   []byte
}

func keyedTestEntries(t *testing.T, body []byte) []keyedTestEntry {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	var entries []keyedTestEntry
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		entries = append(entries, keyedTestEntry{*h, b})
	}
	return entries
}

func TestKeyedInstallerDownload(t *testing.T) {
	for _, arch := range []string{"amd64", "arm64"} {
		t.Run(arch, func(t *testing.T) {
			s, _, generic := configuredAgentBundleServer(t)
			s.hubBase = "https://hub.example.com:8443"
			id, token, err := s.store.CreateEnrollTokenFor("Lab / Node", time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			rec := postForm(t, s, "/machines/"+id+"/keyed-installer", url.Values{"token": {token}, "arch": {arch}})
			if rec.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			if rec.Header().Get("Cache-Control") != "no-store" || rec.Header().Get("X-Content-Type-Options") != "nosniff" || rec.Header().Get("Content-Disposition") != fmt.Sprintf(`attachment; filename="clawctl-agent-Lab---Node-linux-%s.tar.gz"`, arch) {
				t.Fatalf("headers=%v", rec.Header())
			}
			entries, original := keyedTestEntries(t, rec.Body.Bytes()), keyedTestEntries(t, generic[arch])
			if len(entries) != len(original)+2 || !reflect.DeepEqual(entries[:len(original)], original) {
				t.Fatal("generic entries changed")
			}
			for i, want := range []struct {
				name, body string
				mode       int64
			}{{"hub-url", s.hubBase + "\n", 0644}, {"enroll-token", token + "\n", 0600}} {
				got := entries[len(original)+i]
				if got.Header.Name != want.name || got.Header.Mode != want.mode || string(got.Body) != want.body {
					t.Fatalf("embedded entry %s incorrect", want.name)
				}
			}
			audits, err := s.store.Audit(id, 10)
			if err != nil {
				t.Fatal(err)
			}
			if len(audits) != 1 || audits[0].Action != store.AuditKeyedInstaller || audits[0].Detail != "keyed installer downloaded; arch="+arch || strings.Contains(fmt.Sprint(audits), token) {
				t.Fatalf("audit=%+v", audits)
			}
			if _, _, err := s.store.RedeemEnrollToken(token, model.EnrollRequest{Hostname: "lab", OS: "linux", Arch: arch}, time.Now()); err != nil {
				t.Fatalf("download redeemed ticket: %v", err)
			}
		})
	}
}

func TestKeyedInstallerRefusals(t *testing.T) {
	for _, scenario := range []string{"wrong", "other machine", "used", "expired", "revoked", "retired", "missing bundles", "missing file", "tampered", "unknown arch", "no public URL"} {
		t.Run(scenario, func(t *testing.T) {
			s, dir, _ := configuredAgentBundleServer(t)
			s.hubBase = "https://hub.example.com"
			ttl := time.Hour
			if scenario == "expired" {
				ttl = time.Nanosecond
			}
			id, token, err := s.store.CreateEnrollTokenFor("machine", ttl)
			if err != nil {
				t.Fatal(err)
			}
			arch, status := "amd64", http.StatusNotFound
			switch scenario {
			case "wrong":
				token = "wrong-secret"
			case "other machine":
				_, token, err = s.store.CreateEnrollTokenFor("other", time.Hour)
			case "used":
				_, _, err = s.store.RedeemEnrollToken(token, model.EnrollRequest{Hostname: "machine", OS: "linux", Arch: arch}, time.Now())
			case "revoked":
				_, err = s.store.RevokeEnrollToken(id)
			case "retired":
				err = s.store.RetireMachine(id, time.Now())
			case "missing bundles":
				s.agentBundles = nil
			case "missing file":
				err = os.Remove(filepath.Join(dir, agentBundleFilenames[arch]))
			case "tampered":
				err = os.WriteFile(filepath.Join(dir, agentBundleFilenames[arch]), []byte("tampered"), 0600)
				status = http.StatusServiceUnavailable
			case "unknown arch":
				arch = "darwin-amd64"
			case "no public URL":
				s.hubBase = ""
				status = http.StatusConflict
			}
			if err != nil {
				t.Fatal(err)
			}
			rec := postForm(t, s, "/machines/"+id+"/keyed-installer", url.Values{"token": {token}, "arch": {arch}})
			if rec.Code != status {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			if status == http.StatusNotFound && rec.Body.String() != "Keyed installer unavailable\n" {
				t.Fatalf("oracle detail: %q", rec.Body.String())
			}
			if strings.Contains(rec.Body.String(), token) {
				t.Fatal("secret reflected")
			}
		})
	}
}

func TestKeyedInstallerTokenPageHTTPS(t *testing.T) {
	s, _, _ := configuredAgentBundleServer(t)
	s.hubBase = "https://hub.example.com"
	rec, form := createEnrollment(t, s, "keyed-machine", "new machine")
	if rec.Code != http.StatusOK {
		t.Fatal(rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"Download keyed installer (linux/amd64)", "Download keyed installer (linux/arm64)", `name="token"`, `name="arch"`, "Treat it like a password", "valid until", "once used or revoked", "<pre class=\"copy\">./install-agent.sh</pre>", "./install-agent.sh --hub https://hub.example.com --token-file FILE", "Tailscale is skipped automatically"} {
		if !strings.Contains(body, want) {
			t.Errorf("page missing %q", want)
		}
	}
	if strings.Contains(body, "http://100.x") {
		t.Fatal("HTTPS page contains Tailscale hub instructions")
	}
	token := hiddenFormValue(t, body, "token")
	for _, command := range regexp.MustCompile(`(?s)<pre class="copy">(.*?)</pre>`).FindAllStringSubmatch(body, -1) {
		if strings.Contains(command[1], "install-agent") && strings.Contains(command[1], token) {
			t.Fatal("secret embedded in installer command")
		}
	}
	replay := postForm(t, s, "/enrollments", form)
	if replay.Code != http.StatusOK || strings.Contains(replay.Body.String(), token) || strings.Contains(replay.Body.String(), "keyed-installer") {
		t.Fatal("replayed receipt offers a secret download")
	}
	commands := agentInstallerCommands(s.agentBundles, s.hubBase)
	if commands[0].Command != "./install-agent.sh" || commands[0].Alternative != "./install-agent.sh --hub https://hub.example.com --token-file FILE" {
		t.Fatalf("commands=%v", commands)
	}
}
