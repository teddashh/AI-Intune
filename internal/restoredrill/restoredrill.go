// Package restoredrill verifies an immutable Hub backup on a disposable copy.
package restoredrill

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/teddashh/AI-Intune/internal/store"
)

var (
	ErrNoBackup      = errors.New("restore drill: no backup is available")
	ErrUnsafeBackup  = errors.New("restore drill: backup file is unsafe")
	ErrBackupChanged = errors.New("restore drill: selected backup changed")
	ErrEmptyRegistry = errors.New("restore drill: restored registry is empty")
	ErrVerification  = errors.New("restore drill: restored backup verification failed")
	ErrStamp         = errors.New("restore drill: completion stamp could not be written")
)

type Backup struct {
	Name       string    `json:"name"`
	SizeBytes  int64     `json:"size_bytes"`
	ModifiedAt time.Time `json:"modified_at"`
	SHA256     string    `json:"sha256"`
}

type Preview struct {
	Backup       Backup
	LiveExpected int
}

type Result struct {
	Backup       Backup
	Machines     int
	Expected     int
	LiveExpected int
	NewestAt     time.Time
	CompletedAt  time.Time
	Took         time.Duration
}

type Runner struct {
	BackupsDir string
	StampPath  string
	Live       *store.Store
	Now        func() time.Time
}

func (r Runner) Preview(ctx context.Context) (Preview, error) {
	file, backup, err := openNewestBackup(ctx, r.BackupsDir)
	if err != nil {
		return Preview{}, err
	}
	if err := file.Close(); err != nil {
		return Preview{}, fmt.Errorf("%w: close selected backup", ErrVerification)
	}
	liveExpected, err := r.liveExpected()
	if err != nil {
		return Preview{}, err
	}
	return Preview{Backup: backup, LiveExpected: liveExpected}, nil
}

func (r Runner) Run(ctx context.Context, expected Backup) (Result, error) {
	if ctx == nil {
		return Result{}, context.Canceled
	}
	started := time.Now()
	result := Result{Backup: expected, LiveExpected: -1}
	file, current, err := openNewestBackup(ctx, r.BackupsDir)
	if err != nil {
		return result, err
	}
	defer file.Close()
	if current != expected {
		return result, ErrBackupChanged
	}
	tmp, err := os.MkdirTemp("", "clawctl-restore-drill-*")
	if err != nil {
		return result, fmt.Errorf("%w: create private workspace", ErrVerification)
	}
	defer os.RemoveAll(tmp)
	destination := filepath.Join(tmp, "restore.sqlite")
	if err := copyOpenFile(ctx, file, destination, expected); err != nil {
		return result, err
	}
	if err := verifyPinnedBackup(file, filepath.Join(r.BackupsDir, expected.Name), expected); err != nil {
		return result, err
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	restored, err := store.Open(destination)
	if err != nil {
		return result, fmt.Errorf("%w: open disposable copy", ErrVerification)
	}
	defer restored.Close()
	if err := verifySQLiteIntegrity(ctx, restored); err != nil {
		return result, err
	}
	machines, err := restored.ListMachines()
	if err != nil {
		return result, fmt.Errorf("%w: read restored registry", ErrVerification)
	}
	result.Machines = len(machines)
	for _, machine := range machines {
		if machine.InDenominator() {
			result.Expected++
		}
	}
	if result.Machines == 0 {
		return result, ErrEmptyRegistry
	}
	var newest *string
	if err := restored.DB().QueryRowContext(ctx, `SELECT MAX(received_at) FROM machine_checkins`).Scan(&newest); err != nil {
		return result, fmt.Errorf("%w: read restored check-in", ErrVerification)
	}
	if newest != nil {
		parsed, err := time.Parse(time.RFC3339Nano, *newest)
		if err != nil || parsed.IsZero() {
			return result, fmt.Errorf("%w: invalid restored check-in time", ErrVerification)
		}
		result.NewestAt = parsed.UTC()
	}
	result.LiveExpected, err = r.liveExpected()
	if err != nil {
		return result, err
	}
	result.CompletedAt = r.now().UTC().Truncate(time.Second)
	result.Took = time.Since(started)
	if r.StampPath == "" {
		return result, fmt.Errorf("%w: stamp path is unavailable", ErrStamp)
	}
	if err := WriteStamp(r.StampPath, result.CompletedAt, result); err != nil {
		return result, fmt.Errorf("%w: persist completion", ErrStamp)
	}
	return result, nil
}

func verifySQLiteIntegrity(ctx context.Context, restored *store.Store) error {
	rows, err := restored.DB().QueryContext(ctx, `PRAGMA quick_check`)
	if err != nil {
		return fmt.Errorf("%w: check disposable copy integrity", ErrVerification)
	}
	defer rows.Close()
	results := 0
	for rows.Next() {
		var result string
		if err := rows.Scan(&result); err != nil || result != "ok" {
			return fmt.Errorf("%w: disposable copy integrity failed", ErrVerification)
		}
		results++
	}
	if err := rows.Err(); err != nil || results != 1 {
		return fmt.Errorf("%w: disposable copy integrity incomplete", ErrVerification)
	}
	return nil
}

func (r Runner) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r Runner) liveExpected() (int, error) {
	if r.Live == nil {
		return -1, nil
	}
	machines, err := r.Live.ListMachines()
	if err != nil {
		return -1, fmt.Errorf("%w: read live registry comparison", ErrVerification)
	}
	expected := 0
	for _, machine := range machines {
		if machine.InDenominator() {
			expected++
		}
	}
	return expected, nil
}

type candidate struct {
	path string
	info os.FileInfo
}

func openNewestBackup(ctx context.Context, dir string) (*os.File, Backup, error) {
	if ctx == nil {
		return nil, Backup{}, context.Canceled
	}
	if dir == "" || !filepath.IsAbs(dir) || filepath.Clean(dir) != dir {
		return nil, Backup{}, ErrUnsafeBackup
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, Backup{}, ErrNoBackup
		}
		return nil, Backup{}, ErrUnsafeBackup
	}
	if resolved != dir {
		return nil, Backup{}, ErrUnsafeBackup
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, Backup{}, ErrNoBackup
		}
		return nil, Backup{}, fmt.Errorf("%w: read backup directory", ErrUnsafeBackup)
	}
	candidates := make([]candidate, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "clawctl-") || !strings.HasSuffix(name, ".sqlite") {
			continue
		}
		info, err := entry.Info()
		if err != nil || entry.Type()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || !singleLink(info) {
			return nil, Backup{}, ErrUnsafeBackup
		}
		candidates = append(candidates, candidate{path: filepath.Join(dir, name), info: info})
	}
	if len(candidates) == 0 {
		return nil, Backup{}, ErrNoBackup
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].info.ModTime().Equal(candidates[j].info.ModTime()) {
			return candidates[i].path > candidates[j].path
		}
		return candidates[i].info.ModTime().After(candidates[j].info.ModTime())
	})
	selected := candidates[0]
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if _, err := os.Lstat(selected.path + suffix); err == nil || !errors.Is(err, os.ErrNotExist) {
			return nil, Backup{}, ErrUnsafeBackup
		}
	}
	file, err := os.Open(selected.path)
	if err != nil {
		return nil, Backup{}, fmt.Errorf("%w: open selected backup", ErrUnsafeBackup)
	}
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !singleLink(opened) || !os.SameFile(selected.info, opened) {
		file.Close()
		return nil, Backup{}, ErrUnsafeBackup
	}
	digest, err := hashOpenFile(ctx, file)
	if err != nil {
		file.Close()
		return nil, Backup{}, err
	}
	after, err := file.Stat()
	if err != nil || !os.SameFile(opened, after) || after.Size() != opened.Size() || !after.ModTime().Equal(opened.ModTime()) {
		file.Close()
		return nil, Backup{}, ErrBackupChanged
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		file.Close()
		return nil, Backup{}, fmt.Errorf("%w: rewind selected backup", ErrVerification)
	}
	return file, Backup{
		Name: filepath.Base(selected.path), SizeBytes: opened.Size(),
		ModifiedAt: opened.ModTime().UTC(), SHA256: digest,
	}, nil
}

func hashOpenFile(ctx context.Context, file *os.File) (string, error) {
	hash := sha256.New()
	buffer := make([]byte, 128<<10)
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		n, err := file.Read(buffer)
		if n > 0 {
			_, _ = hash.Write(buffer[:n])
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", fmt.Errorf("%w: hash selected backup", ErrVerification)
		}
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil)), nil
}

func copyOpenFile(ctx context.Context, source *os.File, destination string, expected Backup) error {
	if _, err := source.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("%w: rewind selected backup", ErrVerification)
	}
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("%w: create disposable copy", ErrVerification)
	}
	buffer := make([]byte, 128<<10)
	hash := sha256.New()
	var copied int64
	for {
		if err := ctx.Err(); err != nil {
			_ = output.Close()
			return err
		}
		n, readErr := source.Read(buffer)
		if n > 0 {
			if _, err := output.Write(buffer[:n]); err != nil {
				_ = output.Close()
				return fmt.Errorf("%w: write disposable copy", ErrVerification)
			}
			_, _ = hash.Write(buffer[:n])
			copied += int64(n)
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			_ = output.Close()
			return fmt.Errorf("%w: read selected backup", ErrVerification)
		}
	}
	if err := output.Sync(); err != nil {
		_ = output.Close()
		return fmt.Errorf("%w: sync disposable copy", ErrVerification)
	}
	if err := output.Close(); err != nil {
		return fmt.Errorf("%w: close disposable copy", ErrVerification)
	}
	digest := "sha256:" + hex.EncodeToString(hash.Sum(nil))
	if copied != expected.SizeBytes || digest != expected.SHA256 {
		return ErrBackupChanged
	}
	return nil
}

func verifyPinnedBackup(file *os.File, path string, expected Backup) error {
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !singleLink(opened) || opened.Size() != expected.SizeBytes ||
		!opened.ModTime().UTC().Equal(expected.ModifiedAt) {
		return ErrBackupChanged
	}
	pathInfo, err := os.Lstat(path)
	if err != nil || pathInfo.Mode()&os.ModeSymlink != 0 || !pathInfo.Mode().IsRegular() ||
		!singleLink(pathInfo) || !os.SameFile(opened, pathInfo) {
		return ErrBackupChanged
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if _, err := os.Lstat(path + suffix); err == nil || !errors.Is(err, os.ErrNotExist) {
			return ErrBackupChanged
		}
	}
	return nil
}

func singleLink(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Nlink == 1
}

func WriteStamp(path string, at time.Time, result Result) error {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path || at.IsZero() {
		return ErrStamp
	}
	directory := filepath.Dir(path)
	tmp, err := os.CreateTemp(directory, ".restore-drill-stamp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	body := fmt.Sprintf("%d\n%s\nbackup=%s\nmachines=%d expected=%d\n",
		at.Unix(), at.UTC().Format(time.RFC3339), result.Backup.Name, result.Machines, result.Expected)
	if _, err := io.WriteString(tmp, body); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	dir, err := os.Open(directory)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func ReadStamp(path string) (time.Time, bool) {
	if path == "" {
		return time.Time{}, false
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return time.Time{}, false
	}
	first, _, _ := strings.Cut(strings.TrimSpace(string(raw)), "\n")
	seconds, err := strconv.ParseInt(strings.TrimSpace(first), 10, 64)
	if err != nil || seconds <= 0 {
		return time.Time{}, false
	}
	return time.Unix(seconds, 0).UTC(), true
}
