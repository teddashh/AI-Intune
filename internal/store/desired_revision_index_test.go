package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestDesiredStateResourceRevisionUniqueIndexIsDatabaseBackstop(t *testing.T) {
	st, err := Open(t.TempDir() + "/ledger.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	const insert = `INSERT INTO desired_state
 (desired_id,scope_type,scope_id,resource_kind,resource_id,revision,spec,created_at,created_by)
 VALUES (?,?,?,?,?,?,?,?,?)`
	if _, err := st.DB().Exec(insert, "desired-one", "machine", "machine-one", "app", "shared", 7, `{}`, "2026-09-15T00:00:00Z", "test"); err != nil {
		t.Fatal(err)
	}
	_, err = st.DB().Exec(insert, "desired-two", "channel", "stable", "app", "shared", 7, `{}`, "2026-09-15T00:00:01Z", "test")
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "unique") {
		t.Fatalf("raw SQL inserted a duplicate resource revision: %v", err)
	}
}

func TestOpenRejectsDuplicateDesiredStateResourceRevisionWithoutMutation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`DROP INDEX ` + desiredStateResourceRevisionIndex); err != nil {
		t.Fatal(err)
	}
	const insert = `INSERT INTO desired_state
 (desired_id,scope_type,scope_id,resource_kind,resource_id,revision,spec,created_at,created_by)
 VALUES (?,?,?,?,?,?,?,?,?)`
	for _, id := range []string{"desired-one", "desired-two"} {
		if _, err := st.DB().Exec(insert, id, "machine", "machine-one", "app", "shared", 7, `{}`, "2026-09-15T00:00:00Z", "test"); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(path)
	if reopened != nil {
		_ = reopened.Close()
	}
	if !errors.Is(err, ErrDesiredResourceRevisionConflict) || !strings.Contains(err.Error(), "app:shared") ||
		!strings.Contains(err.Error(), "revision 7") || !strings.Contains(err.Error(), "desired-one") || !strings.Contains(err.Error(), "desired-two") {
		t.Fatalf("duplicate migration error=%v", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var rows, indexes int
	if err := db.QueryRow(`SELECT COUNT(*) FROM desired_state`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name=?`, desiredStateResourceRevisionIndex).Scan(&indexes); err != nil {
		t.Fatal(err)
	}
	if rows != 2 || indexes != 0 {
		t.Fatalf("rejected Open changed ledger: rows=%d indexes=%d", rows, indexes)
	}
}

func TestOpenCreatesDesiredStateResourceRevisionIndexOnHistoricalLedger(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.db")
	createManifestLedger(t, path, ledgerFixtureOptions{occupancyColumns: legacyOccupancyFixtureColumns})
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var count int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name=?`, desiredStateResourceRevisionIndex).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("index count=%d, want 1", count)
	}
	if err := ValidateExistingLedger(path); err != nil {
		t.Fatalf("upgraded ledger identity: %v", err)
	}
}

func TestOpenFreshDirectoryCreatesDesiredStateResourceRevisionIndex(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "ledger.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	assertSQLiteSchemaObject(t, path, "index", desiredStateResourceRevisionIndex, true)
}
