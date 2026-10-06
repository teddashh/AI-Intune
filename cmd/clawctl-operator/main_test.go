package main

import (
	"bytes"
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
