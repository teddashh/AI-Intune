package main

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/teddashh/AI-Intune/internal/ledgerlock"
)

// The listener still comes first so an unowned Tailscale address cannot touch
// state.  Once the address is owned, the process must hold the writer flock
// before Store.Open can create/migrate SQLite, and retain it with a defer.
func TestServeOwnsWriterLockBeforeOpeningStore(t *testing.T) {
	mainSource := readRepoFile(t, "main.go")
	serveAt := strings.Index(mainSource, "func serve(argv []string)")
	if serveAt < 0 {
		t.Fatal("main.go has no serve function")
	}
	serveBody := mainSource[serveAt:]
	listenAt := strings.Index(serveBody, `net.Listen("tcp", *addr)`)
	writerAt := strings.Index(serveBody, "ledgerlock.AcquireWriter(*dbPath)")
	deferAt := strings.Index(serveBody, "writerGuard.Close()")
	storeAt := strings.Index(serveBody, "openServeStore(*dbPath)")
	if listenAt < 0 || writerAt < 0 || deferAt < 0 || storeAt < 0 {
		t.Fatalf("missing serve lock-order anchors: listen=%d writer=%d defer=%d store=%d",
			listenAt, writerAt, deferAt, storeAt)
	}
	if !(listenAt < writerAt && writerAt < deferAt && deferAt < storeAt) {
		t.Fatalf("serve order must be listener -> writer lock/defer -> validated Store.Open: listen=%d writer=%d defer=%d store=%d",
			listenAt, writerAt, deferAt, storeAt)
	}
	preStore := serveBody[:storeAt]
	if strings.Contains(preStore, "ledgerlock.AcquireUpgrade(") || strings.Contains(preStore, "ledgerlock.AcquireDirect(") {
		t.Fatal("Hub must not acquire lifecycle lock; upgrader owns it while starting the candidate")
	}
}

func TestOpenServeStoreRejectsUnrelatedSQLiteBeforeMutation(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "unrelated.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, createErr := db.Exec(`CREATE TABLE unrelated(value TEXT)`)
	closeErr := db.Close()
	if createErr != nil || closeErr != nil {
		t.Fatalf("create unrelated DB: create=%v close=%v", createErr, closeErr)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	if st, err := openServeStore(path); err == nil {
		_ = st.Close()
		t.Fatal("unrelated SQLite reached writable Store.Open")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("serve identity preflight modified unrelated SQLite")
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if _, err := os.Lstat(path + suffix); !os.IsNotExist(err) {
			t.Fatalf("serve identity preflight left sidecar %s: %v", suffix, err)
		}
	}
}

func TestOpenServeStoreAllowsExplicitFirstInstallUnderWriterLock(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "new.sqlite")
	guard, err := ledgerlock.AcquireWriter(path)
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Close()
	st, err := openServeStore(path)
	if err != nil {
		t.Fatalf("first install: %v", err)
	}
	defer st.Close()
}
