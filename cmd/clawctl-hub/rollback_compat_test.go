package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/teddashh/AI-Intune/internal/store"
)

func TestRunRollbackCompatibilityUsesReadOnlyCapabilityMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := runRollbackCompatibility(
		[]string{"--rollback-compatible", "--db", path}, &out); err != nil {
		t.Fatalf("rollback capability command: %v", err)
	}
	if !strings.Contains(out.String(), "rollback-compatible") || !strings.Contains(out.String(), "ledger 已靜止") ||
		!strings.Contains(out.String(), "0 active deployments") {
		t.Fatalf("capability output=%q", out.String())
	}
}

func TestRunRollbackCompatibilityRejectsUnexpectedArguments(t *testing.T) {
	var out bytes.Buffer
	err := runRollbackCompatibility([]string{"--rollback-compatible", "extra"}, &out)
	if err == nil || !strings.Contains(err.Error(), "多餘參數") {
		t.Fatalf("unexpected argument err=%v", err)
	}
}
