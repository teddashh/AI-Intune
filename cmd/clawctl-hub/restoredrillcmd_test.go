package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/operatorclient"
	"github.com/teddashh/AI-Intune/internal/store"
)

func TestRestoreDrillCLIHTTPPreviewRunShowListAndReplay(t *testing.T) {
	f, service, _ := operatorRestoreDrillAPIFixture(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mux.ServeHTTP(w, r)
	}))
	defer server.Close()
	base, machineDeps := machineHTTPTestDeps(t, server)
	deps := restoreDrillCommandDeps{machineCommandDeps: machineDeps}
	var out, errOut bytes.Buffer
	if err := runRestoreDrillCommandWithDeps(t.Context(), []string{"preview", "--hub-url", base}, &out, &errOut, deps); err != nil {
		t.Fatalf("preview: %v stderr=%s", err, errOut.String())
	}
	for _, want := range []string{"HTTP operator API preview", "clawctl-20260911T120000Z-before-api.sqlite", "next-step", `--confirm "VERIFY clawctl-20260911T120000Z-before-api.sqlite"`} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("preview missing %q: %s", want, out.String())
		}
	}
	out.Reset()
	errOut.Reset()
	if err := runRestoreDrillCommandWithDeps(t.Context(), []string{
		"run", "--hub-url", base, "--reason", "quarterly recovery verification",
		"--confirm", "VERIFY clawctl-20260911T120000Z-before-api.sqlite",
	}, &out, &errOut, deps); err != nil {
		t.Fatalf("run: %v stderr=%s", err, errOut.String())
	}
	operationMatch := regexp.MustCompile(`operation=([a-z0-9]+) state=queued`).FindStringSubmatch(out.String())
	coordinates := regexp.MustCompile(`idempotency-key=([^ ]+) preview-digest=([^\n]+)`).FindStringSubmatch(errOut.String())
	if len(operationMatch) != 2 || len(coordinates) != 3 || !strings.Contains(out.String(), "next-step: show") {
		t.Fatalf("run output=%s stderr=%s", out.String(), errOut.String())
	}
	operationID := operationMatch[1]
	if processed, err := service.RunQueuedRestoreDrillOperations(t.Context()); err != nil || processed != 1 {
		t.Fatalf("processed=%d err=%v", processed, err)
	}
	out.Reset()
	errOut.Reset()
	if err := runRestoreDrillCommandWithDeps(t.Context(), []string{
		"show", operationID, "--hub-url", base,
	}, &out, &errOut, deps); err != nil || !strings.Contains(out.String(), "state=succeeded") ||
		!strings.Contains(out.String(), "result: machines=1") {
		t.Fatalf("show err=%v output=%s stderr=%s", err, out.String(), errOut.String())
	}
	out.Reset()
	if err := runRestoreDrillCommandWithDeps(t.Context(), []string{"list", "--hub-url", base}, &out, &errOut, deps); err != nil ||
		!strings.Contains(out.String(), operationID) || !strings.Contains(out.String(), "succeeded complete") {
		t.Fatalf("list err=%v output=%s", err, out.String())
	}
	out.Reset()
	errOut.Reset()
	if err := runRestoreDrillCommandWithDeps(t.Context(), []string{
		"run", "--hub-url", base, "--reason", "quarterly recovery verification",
		"--confirm", "VERIFY clawctl-20260911T120000Z-before-api.sqlite",
		"--idempotency-key", coordinates[1], "--preview-digest", coordinates[2],
	}, &out, &errOut, deps); err != nil || !strings.Contains(out.String(), "state=succeeded") {
		t.Fatalf("replay err=%v output=%s stderr=%s", err, out.String(), errOut.String())
	}
	audits, err := f.store.ListAuditReads(store.AuditReadFilter{Actions: []store.AuditAction{store.AuditRestoreDrill}, Limit: 10})
	if err != nil || len(audits.Items) != 2 || audits.Items[0].UserAgent != operatorclient.UserAgent ||
		!audits.Items[0].IsOperatorReplay() {
		t.Fatalf("audits=%+v err=%v", audits, err)
	}
}

func TestRestoreDrillCLIRejectsInvalidCoordinatesBeforeDiscovery(t *testing.T) {
	discoveries := 0
	deps := restoreDrillCommandDeps{machineCommandDeps: machineCommandDeps{
		discoverHubURL: func() (string, error) { discoveries++; return "", nil },
	}}
	for _, args := range [][]string{
		{"run", "--reason", "reason"},
		{"run", "--reason", "reason", "--confirm", "VERIFY x", "--idempotency-key", "key"},
		{"preview", "--reason", "not accepted"},
		{"show"},
		{"list", "--limit", "0"},
		{"preview", "--hub-url", "", "--db", "/tmp/not-used"},
		{"preview", "--backups", "/tmp/backups"},
	} {
		if err := runRestoreDrillCommandWithDeps(t.Context(), args, &bytes.Buffer{}, &bytes.Buffer{}, deps); err == nil {
			t.Fatalf("invalid args accepted: %q", args)
		}
	}
	if discoveries != 0 {
		t.Fatalf("invalid coordinates attempted discovery %d times", discoveries)
	}
}

func TestRestoreDrillCLIDirectDBUsesStoppedServiceBreakGlass(t *testing.T) {
	directory := t.TempDir()
	dbPath := filepath.Join(directory, "hub.sqlite")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	backupsDir := filepath.Join(directory, "backups")
	makeBackup(t, backupsDir, "clawctl-20260911T120000Z-before-direct.sqlite", time.Now().UTC(), 1)
	deps := restoreDrillCommandDeps{machineCommandDeps: machineCommandDeps{
		validateDirectPath:   func(string) error { return nil },
		validateDirectLedger: func(string) error { return nil },
		acquireDirect: func(string) (io.Closer, error) {
			return os.Open(os.DevNull)
		},
		checkMaintenance: func(string) error { return nil },
		verifyHubStopped: func(context.Context, string) error { return nil },
		openDirectDB:     openExisting,
	}, now: time.Now}
	var out, errOut bytes.Buffer
	if err := runRestoreDrillCommandWithDeps(t.Context(), []string{"preview", "--db", dbPath}, &out, &errOut, deps); err != nil ||
		!strings.Contains(out.String(), "direct DB operator service preview") {
		t.Fatalf("preview err=%v output=%s stderr=%s", err, out.String(), errOut.String())
	}
	out.Reset()
	errOut.Reset()
	if err := runRestoreDrillCommandWithDeps(t.Context(), []string{
		"run", "--db", dbPath, "--reason", "stopped service recovery verification",
		"--confirm", "VERIFY clawctl-20260911T120000Z-before-direct.sqlite",
	}, &out, &errOut, deps); err != nil || !strings.Contains(out.String(), "state=succeeded") {
		t.Fatalf("run err=%v output=%s stderr=%s", err, out.String(), errOut.String())
	}
	if _, ok := readDrillStamp(filepath.Join(directory, "restore-drill.stamp")); !ok {
		t.Fatal("direct success did not write stamp")
	}
	check, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	audits, err := check.ListAuditReads(store.AuditReadFilter{Actions: []store.AuditAction{store.AuditRestoreDrill}, Limit: 10})
	if err != nil || len(audits.Items) != 1 || audits.Items[0].SourceKind != "direct-db-cli" {
		t.Fatalf("audits=%+v err=%v", audits, err)
	}
}
