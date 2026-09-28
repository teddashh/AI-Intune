package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	moderncsqlite "modernc.org/sqlite"
)

var ErrRollbackOrphanedSidecar = errors.New("store: rollback database main file is missing but a SQLite sidecar remains")

// rollbackDatabasePresent distinguishes a genuinely empty first install from a
// damaged/mid-move SQLite set. A WAL without its main file is not an empty
// ledger: silently accepting it would discard the only remaining evidence.
func rollbackDatabasePresent(path string) (bool, error) {
	if strings.TrimSpace(path) == "" {
		return false, errors.New("store: rollback database path is empty")
	}
	if _, err := os.Stat(path); err == nil {
		return true, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("store: stat rollback database: %w", err)
	}

	// os.Stat follows symlinks. A dangling link is not the same thing as a path
	// that never existed and must not be blessed as an empty installation.
	if _, err := os.Lstat(path); err == nil {
		return false, fmt.Errorf("store: rollback database main file is a dangling link")
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("store: lstat rollback database: %w", err)
	}

	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		sidecar := path + suffix
		if _, err := os.Lstat(sidecar); err == nil {
			return false, fmt.Errorf("%w (%s)", ErrRollbackOrphanedSidecar, suffix)
		} else if !errors.Is(err, os.ErrNotExist) {
			return false, fmt.Errorf("store: lstat rollback database sidecar %s: %w", suffix, err)
		}
	}
	return false, nil
}

func rollbackReadOnlyDSN(path string) string {
	u := &url.URL{Scheme: "file", Path: path}
	q := u.Query()
	q.Set("mode", "ro")
	q.Add("_pragma", "query_only(1)")
	q.Add("_pragma", "busy_timeout(5000)")
	u.RawQuery = q.Encode()
	return u.String()
}

// CreateRollbackSnapshot creates one standalone, consistent SQLite file from
// source, including commits that currently live only in source's WAL. It then
// validates integrity and rollback quiescence on that completed snapshot—not
// on a separately timed view of the live database.
//
// The bool reports whether a source database existed. A clean first install
// returns (false, nil) and creates no destination. On every error, destination
// and any sidecars SQLite created for it are removed.
func CreateRollbackSnapshot(source, destination string) (present bool, err error) {
	if strings.TrimSpace(destination) == "" {
		return false, errors.New("store: rollback snapshot destination is empty")
	}
	sourceAbs, err := filepath.Abs(source)
	if err != nil {
		return false, fmt.Errorf("store: resolve rollback source: %w", err)
	}
	destinationAbs, err := filepath.Abs(destination)
	if err != nil {
		return false, fmt.Errorf("store: resolve rollback snapshot destination: %w", err)
	}
	if filepath.Clean(sourceAbs) == filepath.Clean(destinationAbs) {
		return false, errors.New("store: rollback snapshot destination is the source database")
	}

	present, err = rollbackDatabasePresent(source)
	if err != nil || !present {
		return present, err
	}
	if err := ValidateExistingLedger(source); err != nil {
		return false, fmt.Errorf("store: rollback source is not a clawctl ledger: %w", err)
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if _, err := os.Lstat(destination + suffix); err == nil {
			return false, fmt.Errorf("store: rollback snapshot destination has an orphan SQLite sidecar %s", suffix)
		} else if !errors.Is(err, os.ErrNotExist) {
			return false, fmt.Errorf("store: lstat rollback snapshot destination sidecar %s: %w", suffix, err)
		}
	}

	// Reserve the exact caller-selected name without overwriting an older
	// rollback coordinate. modernc SQLite can open an existing zero-byte file as
	// a new database and preserves this 0600 mode.
	reserved, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return false, fmt.Errorf("store: reserve rollback snapshot: %w", err)
	}
	if err := reserved.Close(); err != nil {
		_ = os.Remove(destination)
		return false, fmt.Errorf("store: close reserved rollback snapshot: %w", err)
	}
	if err := os.Chmod(destination, 0o600); err != nil {
		_ = os.Remove(destination)
		return false, fmt.Errorf("store: chmod rollback snapshot: %w", err)
	}
	keep := false
	defer func() {
		if keep {
			return
		}
		for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
			_ = os.Remove(destination + suffix)
		}
	}()

	sourceDB, err := sql.Open("sqlite", rollbackReadOnlyDSN(source))
	if err != nil {
		return false, fmt.Errorf("store: open rollback snapshot source: %w", err)
	}
	sourceDB.SetMaxOpenConns(1)
	defer sourceDB.Close()
	conn, err := sourceDB.Conn(context.Background())
	if err != nil {
		return false, fmt.Errorf("store: connect rollback snapshot source: %w", err)
	}
	defer conn.Close()

	type backuper interface {
		NewBackup(string) (*moderncsqlite.Backup, error)
	}
	if err := conn.Raw(func(driverConn any) error {
		creator, ok := driverConn.(backuper)
		if !ok {
			return errors.New("store: SQLite driver does not support online backup")
		}
		backup, err := creator.NewBackup(destination)
		if err != nil {
			return fmt.Errorf("start: %w", err)
		}
		for {
			more, stepErr := backup.Step(-1)
			if stepErr != nil {
				finishErr := backup.Finish()
				return errors.Join(fmt.Errorf("copy pages: %w", stepErr), finishErr)
			}
			if !more {
				break
			}
		}
		if err := backup.Finish(); err != nil {
			return fmt.Errorf("finish: %w", err)
		}
		return nil
	}); err != nil {
		return false, fmt.Errorf("store: create online rollback snapshot: %w", err)
	}

	if err := checkRollbackSnapshotIntegrity(destination); err != nil {
		return false, err
	}
	if err := CheckRollbackCompatible(destination); err != nil {
		return false, fmt.Errorf("store: rollback snapshot is not quiescent: %w", err)
	}
	if err := removeRollbackSnapshotSidecars(destination); err != nil {
		return false, err
	}
	if err := syncRollbackSnapshot(destination); err != nil {
		return false, err
	}
	keep = true
	return true, nil
}

func removeRollbackSnapshotSidecars(path string) error {
	wal := path + "-wal"
	if info, err := os.Stat(wal); err == nil {
		if info.Size() != 0 {
			return fmt.Errorf("store: rollback snapshot unexpectedly contains a nonempty WAL")
		}
		if err := os.Remove(wal); err != nil {
			return fmt.Errorf("store: remove empty rollback snapshot WAL: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("store: stat rollback snapshot WAL: %w", err)
	}
	if err := os.Remove(path + "-shm"); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("store: remove rollback snapshot SHM: %w", err)
	}
	return nil
}

func checkRollbackSnapshotIntegrity(path string) error {
	db, err := sql.Open("sqlite", rollbackReadOnlyDSN(path))
	if err != nil {
		return fmt.Errorf("store: open rollback snapshot for integrity check: %w", err)
	}
	defer db.Close()
	rows, err := db.Query(`PRAGMA quick_check`)
	if err != nil {
		return fmt.Errorf("store: quick_check rollback snapshot: %w", err)
	}
	defer rows.Close()
	results := make([]string, 0, 1)
	for rows.Next() {
		var result string
		if err := rows.Scan(&result); err != nil {
			return fmt.Errorf("store: scan rollback snapshot quick_check: %w", err)
		}
		results = append(results, result)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("store: rollback snapshot quick_check rows: %w", err)
	}
	if len(results) != 1 || results[0] != "ok" {
		return fmt.Errorf("store: rollback snapshot failed quick_check: %s", strings.Join(results, "; "))
	}
	return nil
}

func syncRollbackSnapshot(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("store: open rollback snapshot for sync: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("store: sync rollback snapshot: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("store: close rollback snapshot after sync: %w", err)
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("store: open rollback snapshot directory for sync: %w", err)
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return fmt.Errorf("store: sync rollback snapshot directory: %w", err)
	}
	return nil
}
