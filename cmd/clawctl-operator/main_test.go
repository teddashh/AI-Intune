package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunRequiresATailscaleHubURL(t *testing.T) {
	t.Setenv("CLAWCTL_HUB_URL", "")
	var out, errOut bytes.Buffer
	err := run([]string{"call", "fleet_overview"}, strings.NewReader(""), &out, &errOut)
	if err == nil || !strings.Contains(errOut.String(), "CLAWCTL_HUB_URL") {
		t.Fatalf("err=%v stderr=%s", err, errOut.String())
	}
	if out.Len() != 0 {
		t.Fatalf("stdout = %s", out.String())
	}
}

func TestRunRejectsANonTailscaleOrigin(t *testing.T) {
	var out, errOut bytes.Buffer
	err := run([]string{"--hub-url", "http://127.0.0.1:8787", "tools"}, strings.NewReader(""), &out, &errOut)
	if err == nil || out.Len() != 0 {
		t.Fatalf("err=%v stdout=%s stderr=%s", err, out.String(), errOut.String())
	}
}

func TestCallLocalArgumentErrorsAreInvalidArguments(t *testing.T) {
	var out, errOut bytes.Buffer
	err := run([]string{"--hub-url", "http://100.64.0.1:8787", "call", "sudo"}, strings.NewReader(""), &out, &errOut)
	if err == nil || !strings.Contains(errOut.String(), `"code":"invalid_arguments"`) || !strings.Contains(errOut.String(), "sudo") {
		t.Fatalf("err=%v stdout=%s stderr=%s", err, out.String(), errOut.String())
	}
	out.Reset()
	errOut.Reset()
	err = run([]string{"--hub-url", "http://100.64.0.1:8787", "call", "fleet_overview", `{"nope":1}`}, strings.NewReader(""), &out, &errOut)
	if err == nil || !strings.Contains(errOut.String(), `"code":"invalid_arguments"`) || !strings.Contains(errOut.String(), "nope") || !strings.Contains(errOut.String(), "(none)") {
		t.Fatalf("err=%v stdout=%s stderr=%s", err, out.String(), errOut.String())
	}
	if out.Len() != 0 {
		t.Fatalf("stdout = %s", out.String())
	}
}

func TestRunVersionDoesNotNeedAHub(t *testing.T) {
	var out, errOut bytes.Buffer
	if err := run([]string{"version"}, strings.NewReader(""), &out, &errOut); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out.String()) == "" {
		t.Fatal("empty version")
	}
}

func TestTokenFileArgs(t *testing.T) {
	for _, args := range [][]string{{"--token-file", "/tmp/token", "--hub-url", "https://hub.example.com", "mcp"}, {"--hub-url", "https://hub.example.com", "--token-file=/tmp/token", "call", "fleet_overview"}} {
		path, rest, err := tokenFileFromArgs(args)
		if err != nil || path != "/tmp/token" {
			t.Fatal("token option rejected")
		}
		origin, rest, err := hubURLFromArgsMode(rest, true)
		if err != nil || origin != "https://hub.example.com" || len(rest) == 0 {
			t.Fatal("service URL rejected")
		}
	}
	if _, _, err := hubURLFromArgs([]string{"--hub-url", "https://hub.example.com", "mcp"}); err == nil {
		t.Fatal("tailnet mode changed")
	}
}

func TestServiceTokenMCPAndCallNeverPrintSecret(t *testing.T) {
	secret := "cst_" + strings.Repeat("a", 64)
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte(secret), 0600); err != nil {
		t.Fatal(err)
	}
	for _, command := range [][]string{{"mcp"}, {"call", "unknown_tool"}} {
		var out, errOut bytes.Buffer
		args := append([]string{"--hub-url", "https://hub.example.com", "--token-file", path}, command...)
		err := run(args, strings.NewReader(""), &out, &errOut)
		if command[0] == "mcp" && err != nil {
			t.Fatal("MCP startup failed")
		}
		if command[0] == "call" && err == nil {
			t.Fatal("unknown tool accepted")
		}
		if strings.Contains(out.String(), secret) || strings.Contains(errOut.String(), secret) {
			t.Fatal("CLI printed token")
		}
	}
}
