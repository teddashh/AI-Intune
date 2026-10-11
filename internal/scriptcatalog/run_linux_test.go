//go:build linux

package scriptcatalog

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

func testEntry(body string) Entry {
	return Entry{ID: "test-only", Version: "1", Bytes: []byte(body), SHA256: Digest([]byte(body)), RunsAs: "agent-user", Mode: "read", AllowedOS: []string{"linux"}, MaxTimeout: 5, OutputCap: 64, ArgsSchema: Schema{Type: "object", Properties: map[string]Property{}}}
}
func TestRunTimeoutKillsProcessGroup(t *testing.T) {
	requireAgentUser(t)
	e := testEntry("sleep 30 &\nwait\n")
	start := time.Now()
	r, err := Run(context.Background(), e, []byte(`{}`), 1)
	if err != nil || !r.TimedOut || r.ExitCode == 0 || time.Since(start) > 3*time.Second {
		t.Fatalf("timeout result %+v %v", r, err)
	}
}
func TestRunCapsHashesAndRedacts(t *testing.T) {
	requireAgentUser(t)
	body := "printf 'token=sample-secret\\n'\ni=0; while [ \"$i\" -lt 100 ]; do printf 'abcdef\\n'; printf 'stderr\\n' >&2; i=$((i+1)); done\n"
	e := testEntry(body)
	r, err := Run(context.Background(), e, []byte(`{}`), 3)
	if err != nil {
		t.Fatal(err)
	}
	if !r.StdoutTruncated || !r.StderrTruncated || len(r.Stdout) > e.OutputCap || len(r.Stderr) > e.OutputCap || strings.Contains(r.Stdout, "sample-secret") || !strings.Contains(r.Stdout, "[REDACTED]") {
		t.Fatalf("cap/redaction failed %+v", r)
	}
	expected := "token=sample-secret\n" + strings.Repeat("abcdef\n", 100)
	if r.StdoutSHA256 != Digest([]byte(expected)) || r.StderrSHA256 != Digest([]byte(strings.Repeat("stderr\n", 100))) {
		t.Fatal("must hash full streams")
	}
}
func TestRunArgsAreLiteralAndEnvironmentClean(t *testing.T) {
	requireAgentUser(t)
	t.Setenv("SCRIPT_TEST_SECRET", "never inherit")
	e := testEntry("cat\nprintf '\\n%s' \"${SCRIPT_TEST_SECRET-unset}\"\n")
	e.OutputCap = 1024
	e.ArgsSchema.Properties["label"] = Property{Type: "string", MaxLength: 128}
	raw := []byte(`{"label":";$()$(printf interpreted)"}`)
	r, err := Run(context.Background(), e, raw, 3)
	if err != nil || r.ExitCode != 0 || r.Stdout != string(raw)+"\nunset" {
		t.Fatalf("literal args or clean env failed %+v %v", r, err)
	}
}
func TestProbeJSONContainsOnlyFacts(t *testing.T) {
	requireAgentUser(t)
	e, _ := Lookup("fleet-probe-v1")
	r, err := Run(context.Background(), e, []byte(`{}`), e.DefaultTimeout)
	if err != nil || r.ExitCode != 0 {
		t.Fatalf("probe failed %+v %v", r, err)
	}
	var facts map[string]any
	if json.Unmarshal([]byte(r.Stdout), &facts) != nil || len(facts) != 4 {
		t.Fatal("probe facts are not JSON")
	}
	for _, key := range []string{"uptime_seconds", "load", "root_disk_use_percent", "failed_systemd_unit_count"} {
		if _, ok := facts[key]; !ok {
			t.Fatalf("missing %s", key)
		}
	}
}
func TestRedaction(t *testing.T) {
	for _, s := range []string{"Bearer sample-value", "api_key=sample-value", "ghp_samplevalue", "sk-samplevalue", strings.Repeat("a", 40)} {
		if Redact(s) == s {
			t.Fatal("secret shape unredacted")
		}
	}
}

func requireAgentUser(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("phase-one script execution tests require a non-root agent user")
	}
}
