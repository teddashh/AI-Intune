package store

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/deploy"
)

func TestCheckRollbackCompatibleRejectsRunningDeployment(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	now := time.Date(2026, 9, 6, 16, 0, 0, 0, time.UTC)
	desiredID, revision, err := s.CreateDesiredState(
		"channel", "canary", "maintenance", "proof", `{"kind":"noop"}`, "test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(`INSERT INTO deployments
 (deployment_id,channel,desired_id,resource_kind,resource_id,revision,batch_size,state,created_at,created_by)
 VALUES('running-deployment','canary',?,'maintenance','proof',?,1,'running',?,'test')`,
		desiredID, revision, fmtTime(now)); err != nil {
		t.Fatal(err)
	}

	err = CheckRollbackCompatible(path)
	if !errors.Is(err, ErrRollbackNotQuiescent) || !strings.Contains(err.Error(), "1 active deployment") {
		t.Fatalf("running deployment compatibility err=%v", err)
	}
}

func TestCheckRollbackCompatibleRejectsPausedDeployment(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	now := time.Date(2026, 9, 6, 16, 0, 0, 0, time.UTC)
	desiredID, revision, err := s.CreateDesiredState(
		"channel", "stable", "maintenance", "paused-proof", `{"kind":"noop"}`, "test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(`INSERT INTO deployments
 (deployment_id,channel,desired_id,resource_kind,resource_id,revision,batch_size,state,created_at,created_by,paused_at)
 VALUES('paused-deployment','stable',?,'maintenance','paused-proof',?,1,'paused',?,'test',?)`,
		desiredID, revision, fmtTime(now), fmtTime(now)); err != nil {
		t.Fatal(err)
	}

	err = CheckRollbackCompatible(path)
	if !errors.Is(err, ErrRollbackNotQuiescent) || !strings.Contains(err.Error(), "1 active deployment") {
		t.Fatalf("paused deployment compatibility err=%v", err)
	}
}

func TestCheckRollbackCompatibleRejectsNonTerminalJob(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	now := time.Date(2026, 9, 6, 16, 0, 0, 0, time.UTC)
	machineID := mustEnroll(t, s, "rollback-check", now)
	desiredID, revision, err := s.CreateDesiredState(
		"machine", machineID, "maintenance", "proof", `{"kind":"noop"}`, "test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateJob(machineID, desiredID, revision, NewJob{}); err != nil {
		t.Fatal(err)
	}

	err = CheckRollbackCompatible(path)
	if !errors.Is(err, ErrRollbackNotQuiescent) || !strings.Contains(err.Error(), "1 non-terminal job") {
		t.Fatalf("nonterminal job compatibility err=%v", err)
	}
}

func TestCheckRollbackCompatibleRejectsActiveArtifactFetchAndAllowsTerminal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	req := artifactFetchTestRequest("rollback-artifact-fetch", "d")
	queued, err := s.ApplyOperatorArtifactFetch(req, func() (ArtifactFetchPrepared, error) {
		return artifactFetchTestPrepared(req), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckRollbackCompatible(path); !errors.Is(err, ErrRollbackNotQuiescent) ||
		!strings.Contains(err.Error(), "1 active artifact fetch") {
		t.Fatalf("queued artifact fetch compatibility err=%v", err)
	}
	claim, err := s.ClaimArtifactFetchOperation(queued.Operation.OperationID, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckRollbackCompatible(path); !errors.Is(err, ErrRollbackNotQuiescent) ||
		!strings.Contains(err.Error(), "1 active artifact fetch") {
		t.Fatalf("running artifact fetch compatibility err=%v", err)
	}
	if _, err := s.FailArtifactFetchOperation(claim.Operation.OperationID, claim.RunToken,
		"ARTIFACT_FETCH_TEST_FAILURE", "terminal test failure"); err != nil {
		t.Fatal(err)
	}
	if err := CheckRollbackCompatible(path); err != nil {
		t.Fatalf("terminal artifact fetch should be rollback-compatible: %v", err)
	}
}

func TestCheckRollbackCompatibleIsReadOnlyAndAllowsTerminalLedger(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	now := time.Date(2026, 9, 6, 16, 0, 0, 0, time.UTC)
	machineID := mustEnroll(t, s, "rollback-check", now)
	desiredID, revision, err := s.CreateDesiredState(
		"machine", machineID, "maintenance", "proof", `{"kind":"noop"}`, "test")
	if err != nil {
		t.Fatal(err)
	}
	jobID, err := s.CreateJob(machineID, desiredID, revision, NewJob{})
	if err != nil {
		t.Fatal(err)
	}
	// 舊 Hub 可能留下「終態仍有 lease」的列。一般 Open() 會修掉它；
	// rollback capability 必須只讀，連這種 migration/backfill 都不能偷做。
	if _, err := s.DB().Exec(`UPDATE jobs
 SET state=?,terminal_at=?,lease_token='legacy-lease',lease_expires_at=? WHERE job_id=?`,
		deploy.Succeeded, fmtTime(now), fmtTime(now.Add(time.Hour)), jobID); err != nil {
		t.Fatal(err)
	}

	if err := CheckRollbackCompatible(path); err != nil {
		t.Fatalf("terminal ledger should be rollback-compatible: %v", err)
	}
	var lease string
	if err := s.DB().QueryRow(`SELECT lease_token FROM jobs WHERE job_id=?`, jobID).Scan(&lease); err != nil {
		t.Fatal(err)
	}
	if lease != "legacy-lease" {
		t.Fatalf("read-only compatibility check mutated terminal lease: %q", lease)
	}
}

func TestCheckRollbackCompatibleMissingDatabaseDoesNotCreateIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "hub.db")
	if err := CheckRollbackCompatible(path); err != nil {
		t.Fatalf("missing DB is an empty ledger, got %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read-only compatibility check created DB: stat err=%v", err)
	}
	if _, err := os.Stat(filepath.Dir(path)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read-only compatibility check even created the DB directory: stat err=%v", err)
	}
}

func TestCheckRollbackCompatibleTreatsPreDeploymentClawctlLedgerAsNoActiveDeployments(t *testing.T) {
	path := filepath.Join(t.TempDir(), "phase1.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, dropErr := db.Exec(`DROP TABLE deployments`)
	closeErr := db.Close()
	if dropErr != nil || closeErr != nil {
		t.Fatalf("make pre-deployment fixture: drop=%v close=%v", dropErr, closeErr)
	}

	if err := CheckRollbackCompatible(path); err != nil {
		t.Fatalf("complete pre-deployment clawctl ledger rejected: %v", err)
	}
}

func TestCheckRollbackCompatibleRejectsJobsOnlySQLiteDecoy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "decoy.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, createErr := db.Exec(`CREATE TABLE jobs(state TEXT)`)
	closeErr := db.Close()
	if createErr != nil || closeErr != nil {
		t.Fatalf("make jobs-only decoy: create=%v close=%v", createErr, closeErr)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	if err := CheckRollbackCompatible(path); err == nil || !strings.Contains(err.Error(), "not a clawctl ledger") {
		t.Fatalf("jobs-only decoy accepted: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("rollback identity check modified jobs-only decoy")
	}
}

func TestCheckRollbackCompatibleRejectsOrphanSQLiteSidecars(t *testing.T) {
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		t.Run(suffix, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "missing.sqlite")
			if err := os.WriteFile(path+suffix, []byte("orphan"), 0o600); err != nil {
				t.Fatal(err)
			}

			err := CheckRollbackCompatible(path)
			if !errors.Is(err, ErrRollbackOrphanedSidecar) {
				t.Fatalf("orphan %s was treated as an empty ledger: %v", suffix, err)
			}
			if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("orphan check created the missing main DB: %v", statErr)
			}
		})
	}
}

func TestCreateRollbackSnapshotIncludesWALAndIsStandalone(t *testing.T) {
	source := filepath.Join(t.TempDir(), "source.sqlite")
	destination := filepath.Join(t.TempDir(), "backup.sqlite")
	s, err := Open(source)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.DB().Exec(`PRAGMA wal_autocheckpoint=0`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.CreateDesiredState("channel", "canary", "maintenance", "wal-proof", `{"kind":"noop"}`, "test"); err != nil {
		t.Fatal(err)
	}
	walInfo, err := os.Stat(source + "-wal")
	if err != nil || walInfo.Size() == 0 {
		t.Fatalf("test setup did not leave committed data in WAL: info=%v err=%v", walInfo, err)
	}

	present, err := CreateRollbackSnapshot(source, destination)
	if err != nil {
		t.Fatalf("online snapshot: %v", err)
	}
	if !present {
		t.Fatal("existing source reported absent")
	}
	if info, err := os.Stat(destination + "-wal"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("snapshot depends on a WAL sidecar: info=%v err=%v", info, err)
	}

	db, err := sql.Open("sqlite", destination)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM desired_state WHERE resource_id='wal-proof'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("standalone snapshot lost WAL commit: count=%d", count)
	}
	var quick string
	if err := db.QueryRow(`PRAGMA quick_check`).Scan(&quick); err != nil || quick != "ok" {
		t.Fatalf("snapshot quick_check=%q err=%v", quick, err)
	}
}

func TestCreateRollbackSnapshotChecksQuiescenceOnSnapshotAndCleansRefusal(t *testing.T) {
	source := filepath.Join(t.TempDir(), "source.sqlite")
	destination := filepath.Join(t.TempDir(), "backup.sqlite")
	s, err := Open(source)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Date(2026, 9, 6, 16, 0, 0, 0, time.UTC)
	machineID := mustEnroll(t, s, "rollback-snapshot", now)
	desiredID, revision, err := s.CreateDesiredState("machine", machineID, "maintenance", "proof", `{"kind":"noop"}`, "test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateJob(machineID, desiredID, revision, NewJob{}); err != nil {
		t.Fatal(err)
	}

	present, err := CreateRollbackSnapshot(source, destination)
	if present || !errors.Is(err, ErrRollbackNotQuiescent) {
		t.Fatalf("nonquiescent snapshot: present=%v err=%v", present, err)
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if _, statErr := os.Stat(destination + suffix); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("refused snapshot left %q behind: %v", destination+suffix, statErr)
		}
	}
}

func TestCreateRollbackSnapshotRejectsPausedDeploymentAndCleansRefusal(t *testing.T) {
	source := filepath.Join(t.TempDir(), "source.sqlite")
	destination := filepath.Join(t.TempDir(), "backup.sqlite")
	s, err := Open(source)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Date(2026, 9, 6, 16, 0, 0, 0, time.UTC)
	desiredID, revision, err := s.CreateDesiredState(
		"channel", "stable", "maintenance", "paused-snapshot", `{"kind":"noop"}`, "test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(`INSERT INTO deployments
 (deployment_id,channel,desired_id,resource_kind,resource_id,revision,batch_size,state,created_at,created_by,paused_at)
 VALUES('paused-snapshot','stable',?,'maintenance','paused-snapshot',?,1,'paused',?,'test',?)`,
		desiredID, revision, fmtTime(now), fmtTime(now)); err != nil {
		t.Fatal(err)
	}

	present, err := CreateRollbackSnapshot(source, destination)
	if present || !errors.Is(err, ErrRollbackNotQuiescent) || !strings.Contains(err.Error(), "active deployment") {
		t.Fatalf("paused automatic rollback snapshot: present=%v err=%v", present, err)
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if _, statErr := os.Stat(destination + suffix); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("paused refusal left %q behind: %v", destination+suffix, statErr)
		}
	}
}

func TestCreateRollbackSnapshotMissingDatabaseAndOrphanSemantics(t *testing.T) {
	t.Run("clean first install", func(t *testing.T) {
		source := filepath.Join(t.TempDir(), "missing.sqlite")
		destination := filepath.Join(t.TempDir(), "backup.sqlite")
		present, err := CreateRollbackSnapshot(source, destination)
		if err != nil || present {
			t.Fatalf("clean missing DB: present=%v err=%v", present, err)
		}
		if _, err := os.Stat(destination); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("empty install created backup: %v", err)
		}
	})

	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		t.Run("orphan sidecar "+suffix, func(t *testing.T) {
			source := filepath.Join(t.TempDir(), "missing.sqlite")
			destination := filepath.Join(t.TempDir(), "backup.sqlite")
			if err := os.WriteFile(source+suffix, []byte("orphan"), 0o600); err != nil {
				t.Fatal(err)
			}
			present, err := CreateRollbackSnapshot(source, destination)
			if present || !errors.Is(err, ErrRollbackOrphanedSidecar) {
				t.Fatalf("orphan %s snapshot: present=%v err=%v", suffix, present, err)
			}
		})
	}
}

func TestCreateRollbackSnapshotRefusesToOverwriteDestination(t *testing.T) {
	source := filepath.Join(t.TempDir(), "source.sqlite")
	destination := filepath.Join(t.TempDir(), "backup.sqlite")
	s, err := Open(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	const sentinel = "do not overwrite"
	if err := os.WriteFile(destination, []byte(sentinel), 0o600); err != nil {
		t.Fatal(err)
	}

	if present, err := CreateRollbackSnapshot(source, destination); err == nil || present {
		t.Fatalf("existing destination accepted: present=%v err=%v", present, err)
	}
	got, err := os.ReadFile(destination)
	if err != nil || string(got) != sentinel {
		t.Fatalf("existing destination changed: got=%q err=%v", got, err)
	}
}

func TestCreateRollbackSnapshotRejectsOrphanDestinationSidecar(t *testing.T) {
	source := filepath.Join(t.TempDir(), "source.sqlite")
	destination := filepath.Join(t.TempDir(), "backup.sqlite")
	s, err := Open(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(destination+"-wal", []byte("old partial backup"), 0o600); err != nil {
		t.Fatal(err)
	}

	if present, err := CreateRollbackSnapshot(source, destination); err == nil || present {
		t.Fatalf("orphan destination WAL accepted: present=%v err=%v", present, err)
	}
	if _, err := os.Stat(destination); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("refusal created destination main: %v", err)
	}
}
