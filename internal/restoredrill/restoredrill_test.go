package restoredrill

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/model"
	"github.com/teddashh/AI-Intune/internal/store"
)

func testBackup(t *testing.T, directory, name string, machines int, modified time.Time) string {
	t.Helper()
	source := filepath.Join(t.TempDir(), "source.sqlite")
	st, err := store.Open(source)
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < machines; index++ {
		token, err := st.CreateEnrollToken("restore-drill-machine", time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		id, _, err := st.RedeemEnrollToken(token, model.EnrollRequest{
			SchemaVersion: model.SchemaVersion, EnrollToken: token, Hostname: "restore-drill-machine",
			OS: "linux", Arch: "amd64", UnixUser: "operator",
		}, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		at := modified.Add(-time.Duration(index+1) * time.Minute).UTC()
		if _, err := st.DB().Exec(`INSERT INTO machine_checkins(machine_id,sent_at,received_at) VALUES(?,?,?)`,
			id, at.Format(time.RFC3339Nano), at.Format(time.RFC3339Nano)); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(directory, name)
	if err := os.WriteFile(destination, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(destination, modified, modified); err != nil {
		t.Fatal(err)
	}
	return destination
}

func TestRunnerPinsNewestBackupAndVerifiesDisposableCopy(t *testing.T) {
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	directory := filepath.Join(t.TempDir(), "backups")
	_ = testBackup(t, directory, "clawctl-20260909T000000Z-before-old.sqlite", 1, now.Add(-48*time.Hour))
	newest := testBackup(t, directory, "clawctl-20260910T000000Z-before-new.sqlite", 2, now.Add(-24*time.Hour))
	before, err := os.ReadFile(newest)
	if err != nil {
		t.Fatal(err)
	}
	stamp := filepath.Join(t.TempDir(), "restore-drill.stamp")
	runner := Runner{BackupsDir: directory, StampPath: stamp, Now: func() time.Time { return now }}
	preview, err := runner.Preview(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if preview.Backup.Name != filepath.Base(newest) || preview.Backup.SizeBytes != int64(len(before)) ||
		preview.Backup.SHA256 == "" || preview.LiveExpected != -1 {
		t.Fatalf("preview=%+v", preview)
	}
	result, err := runner.Run(t.Context(), preview.Backup)
	if err != nil {
		t.Fatal(err)
	}
	if result.Machines != 2 || result.Expected != 2 || result.LiveExpected != -1 || result.NewestAt.IsZero() ||
		!result.CompletedAt.Equal(now) {
		t.Fatalf("result=%+v", result)
	}
	after, err := os.ReadFile(newest)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("restore drill modified the pinned backup")
	}
	if stamped, ok := ReadStamp(stamp); !ok || !stamped.Equal(now) {
		t.Fatalf("stamp=%s ok=%t", stamped, ok)
	}
}

func TestRunnerRejectsChangedOrUnsafeBackupWithoutStamp(t *testing.T) {
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	t.Run("changed after preview", func(t *testing.T) {
		directory := filepath.Join(t.TempDir(), "backups")
		path := testBackup(t, directory, "clawctl-current.sqlite", 1, now)
		stamp := filepath.Join(t.TempDir(), "stamp")
		runner := Runner{BackupsDir: directory, StampPath: stamp, Now: func() time.Time { return now }}
		preview, err := runner.Preview(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, now.Add(time.Second), now.Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		if _, err := runner.Run(t.Context(), preview.Backup); !errors.Is(err, ErrBackupChanged) {
			t.Fatalf("run err=%v", err)
		}
		if _, ok := ReadStamp(stamp); ok {
			t.Fatal("changed backup wrote completion stamp")
		}
	})
	for _, test := range []struct {
		name  string
		setup func(*testing.T, string)
	}{
		{name: "symlink", setup: func(t *testing.T, directory string) {
			target := testBackup(t, filepath.Join(t.TempDir(), "outside"), "clawctl-target.sqlite", 1, now)
			if err := os.MkdirAll(directory, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, filepath.Join(directory, "clawctl-link.sqlite")); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "sidecar", setup: func(t *testing.T, directory string) {
			path := testBackup(t, directory, "clawctl-sidecar.sqlite", 1, now)
			if err := os.WriteFile(path+"-wal", []byte("unexpected"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "hardlink", setup: func(t *testing.T, directory string) {
			path := testBackup(t, directory, "clawctl-linked.sqlite", 1, now)
			if err := os.Link(path, filepath.Join(t.TempDir(), "second-link")); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			directory := filepath.Join(t.TempDir(), "backups")
			test.setup(t, directory)
			stamp := filepath.Join(t.TempDir(), "stamp")
			if _, err := (Runner{BackupsDir: directory, StampPath: stamp}).Preview(t.Context()); !errors.Is(err, ErrUnsafeBackup) {
				t.Fatalf("preview err=%v", err)
			}
			if _, ok := ReadStamp(stamp); ok {
				t.Fatal("unsafe backup wrote completion stamp")
			}
		})
	}
}

func TestRunnerCancellationAndEmptyRegistryNeverStamp(t *testing.T) {
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	t.Run("cancelled", func(t *testing.T) {
		directory := filepath.Join(t.TempDir(), "backups")
		testBackup(t, directory, "clawctl-cancel.sqlite", 1, now)
		stamp := filepath.Join(t.TempDir(), "stamp")
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if _, err := (Runner{BackupsDir: directory, StampPath: stamp}).Preview(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("preview err=%v", err)
		}
		if _, ok := ReadStamp(stamp); ok {
			t.Fatal("cancelled preview wrote completion stamp")
		}
	})
	t.Run("empty registry", func(t *testing.T) {
		directory := filepath.Join(t.TempDir(), "backups")
		testBackup(t, directory, "clawctl-empty.sqlite", 0, now)
		stamp := filepath.Join(t.TempDir(), "stamp")
		runner := Runner{BackupsDir: directory, StampPath: stamp}
		preview, err := runner.Preview(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := runner.Run(t.Context(), preview.Backup); !errors.Is(err, ErrEmptyRegistry) {
			t.Fatalf("run err=%v", err)
		}
		if _, ok := ReadStamp(stamp); ok {
			t.Fatal("failed drill wrote completion stamp")
		}
	})
}
