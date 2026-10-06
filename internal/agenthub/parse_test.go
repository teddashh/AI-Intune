package agenthub

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestParseTailscaleAndTunnel(t *testing.T) {
	cases := []struct {
		raw  string
		want string
	}{
		{"http://100.64.0.1:8787", "http://100.64.0.1:8787"},
		{"http://100.64.0.1:8787/", "http://100.64.0.1:8787"},
		{"http://100.127.255.254:1", "http://100.127.255.254:1"},
		{"http://[fd7a:115c:a1e0::1]:8787", "http://[fd7a:115c:a1e0::1]:8787"},
		{"https://agents.example.com", "https://agents.example.com"},
		{"https://agents.example.com/", "https://agents.example.com"},
		{"https://Agents.Example.com", "https://agents.example.com"},
		{"https://agents.example.com:8443", "https://agents.example.com:8443"},
		{"https://agents.example.com:443", "https://agents.example.com:443"},
	}
	for _, tc := range cases {
		got, err := Parse(tc.raw)
		if err != nil || got != tc.want {
			t.Errorf("Parse(%q) = %q %v, want %q", tc.raw, got, err, tc.want)
		}
	}
	rejected := []string{
		"",
		" http://100.64.0.1:8787",
		"http://100.64.0.1",
		"http://127.0.0.1:8787",
		"http://10.0.0.1:8787",
		"http://100.100.100.100:8787",
		"http://100.115.92.1:8787",
		"http://100.115.93.10:8787",
		"http://100.64.0.1:8787/v1",
		"http://100.64.0.1:8787//",
		"https://localhost",
		"https://localhost:8443",
		"https://example",
		"https://192.0.2.1",
		"https://192.0.2.1:443",
		"https://user:pass@agents.example.com",
		"https://agents.example.com/enroll",
		"https://agents.example.com?x=1",
		"https://agents.example.com#frag",
		"https://agents.example.com//",
		"HTTPS://agents.example.com",
	}
	for _, raw := range rejected {
		if _, err := Parse(raw); err == nil {
			t.Errorf("Parse(%q) accepted", raw)
		}
	}
}

func TestInstallerHubURLMatchesParse(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("caller")
	}
	root := filepath.Join(filepath.Dir(file), "..", "..")
	scripts := []string{
		filepath.Join(root, "ops", "install-agent.sh"),
		filepath.Join(root, "ops", "install-agent-macos.sh"),
	}
	accepted := []string{
		"http://100.64.0.1:8787",
		"http://100.64.0.1:8787/",
		"http://100.127.255.254:1",
		"http://100.115.91.1:8787",
		"http://100.115.94.1:8787",
		"https://agents.example.com",
		"https://agents.example.com/",
		"https://Agents.Example.com",
		"https://agents.example.com:8443",
		"https://agents.example.com:443",
	}
	rejected := []string{
		"http://127.0.0.1:8787",
		"http://10.0.0.1:8787",
		"http://100.100.100.100:8787",
		"http://100.115.92.1:8787",
		"http://100.115.93.255:8787",
		"http://100.64.0.1",
		"http://100.064.0.1:8787",
		"http://100.64.0.1:08787",
		"https://localhost",
		"https://example",
		"https://192.0.2.1:443",
		"https://user:pass@agents.example.com",
		"https://agents.example.com/enroll",
		"https://agents.example.com?x=1",
		"http://[fd7a:115c:a1e0::1]:8787",
	}
	for _, script := range scripts {
		for _, raw := range accepted {
			want, err := Parse(raw)
			if err != nil {
				t.Fatal(err)
			}
			got, err := runHubCheck(script, raw)
			if err != nil {
				t.Errorf("%s accept %q: %v", script, raw, err)
				continue
			}
			if got != want {
				t.Errorf("%s %q canonical %q, Parse %q", filepath.Base(script), raw, got, want)
			}
		}
		for _, raw := range rejected {
			if _, err := runHubCheck(script, raw); err == nil {
				t.Errorf("%s accepted %q", filepath.Base(script), raw)
			}
		}
	}
}

func runHubCheck(script, hub string) (string, error) {
	cmd := exec.Command("bash", script, "--hub", hub)
	cmd.Env = append(os.Environ(), "CLAWCTL_INSTALL_AGENT_CHECK_HUB=1")
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
