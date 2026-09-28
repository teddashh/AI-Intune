// Package ledgerlock serializes processes that can write the Hub ledger.
//
// There are two deliberately separate locks next to the SQLite database:
//
//   - <db>.upgrade.lock owns the upgrade/rollback lifecycle.
//   - <db>.writer.lock proves that only one Hub or stopped-service direct
//     operator is allowed to open the ledger for writing.
//
// A direct operator must acquire them in that order.  The Hub takes only the
// writer lock: upgrade-hub starts the candidate while it still owns the
// lifecycle lock, so making the Hub take both would deadlock every rollout.
package ledgerlock

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"golang.org/x/sys/unix"
)

// ErrContended means another process already owns the requested lock.  Lock
// acquisition is always nonblocking; callers decide whether contention is a
// normal refusal or a fatal startup error.
var ErrContended = errors.New("ledger lock is already held")

type lockKind string

const (
	upgradeLock lockKind = "upgrade"
	writerLock  lockKind = "writer"
)

// Handle owns one flock until Close.  Close is idempotent so partially-built
// multi-lock acquisitions can unwind without obscuring the original error.
type Handle struct {
	mu   sync.Mutex
	file *os.File
	path string
}

// Path returns the pathname whose inode was verified and locked.
func (h *Handle) Path() string {
	if h == nil {
		return ""
	}
	return h.path
}

// Close releases the flock and closes its descriptor.
func (h *Handle) Close() error {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.file == nil {
		return nil
	}
	f := h.file
	h.file = nil
	unlockErr := unix.Flock(int(f.Fd()), unix.LOCK_UN)
	closeErr := f.Close()
	return errors.Join(unlockErr, closeErr)
}

// DirectHandle owns the stopped-service break-glass pair.  The order is part
// of the contract: acquire lifecycle then writer; release in reverse order.
type DirectHandle struct {
	upgrade *Handle
	writer  *Handle
}

// Close releases writer then lifecycle.  It is safe to call more than once.
func (h *DirectHandle) Close() error {
	if h == nil {
		return nil
	}
	var writerErr, upgradeErr error
	if h.writer != nil {
		writerErr = h.writer.Close()
	}
	if h.upgrade != nil {
		upgradeErr = h.upgrade.Close()
	}
	return errors.Join(writerErr, upgradeErr)
}

// UpgradePath is the lifecycle lock belonging to dbPath.
func UpgradePath(dbPath string) string { return dbPath + ".upgrade.lock" }

// WriterPath is the single-writer lock belonging to dbPath.
func WriterPath(dbPath string) string { return dbPath + ".writer.lock" }

// AcquireUpgrade takes the lifecycle lock without waiting.
func AcquireUpgrade(dbPath string) (*Handle, error) {
	return acquire(dbPath, upgradeLock, nil)
}

// AcquireWriter takes the ledger writer lock without waiting.
func AcquireWriter(dbPath string) (*Handle, error) {
	return acquire(dbPath, writerLock, nil)
}

// AcquireDirect takes the lifecycle lock and then the writer lock, without
// waiting for either.  A writer failure releases the lifecycle lock before the
// error is returned.
func AcquireDirect(dbPath string) (*DirectHandle, error) {
	upgrade, err := AcquireUpgrade(dbPath)
	if err != nil {
		return nil, err
	}
	writer, err := AcquireWriter(dbPath)
	if err != nil {
		closeErr := upgrade.Close()
		if closeErr != nil {
			return nil, errors.Join(err, fmt.Errorf("release lifecycle lock after writer refusal: %w", closeErr))
		}
		return nil, err
	}
	return &DirectHandle{upgrade: upgrade, writer: writer}, nil
}

// ValidateExistingDB performs the side-effect-free pathname checks needed
// before a direct operator creates either sidecar lock.  Callers should repeat
// it after AcquireDirect and before Store.Open so a missing, symlinked, or
// hardlinked target can never cause lock/WAL/SHM files to be created.
func ValidateExistingDB(dbPath string) error {
	return validateDBPath(dbPath, false)
}

// ValidateUpgradeTarget verifies the canonical ledger pathname and every
// existing SQLite sidecar while allowing a genuinely absent first-install DB.
// Upgrade scripts call it only after their lifecycle+writer locks are held and
// repeat it at each snapshot/restore boundary.
func ValidateUpgradeTarget(dbPath string) error {
	return validateDBPath(dbPath, true)
}

func validateDBPath(dbPath string, allowMissing bool) error {
	parentPath, base, err := splitDBPath(dbPath)
	if err != nil {
		return err
	}
	parentFD, err := openTrustedParent(parentPath)
	if err != nil {
		return err
	}
	defer unix.Close(parentFD)

	if err := verifyTrustedParent(parentPath, parentFD); err != nil {
		return err
	}
	if err := validateDBTarget(parentFD, base, dbPath, allowMissing); err != nil {
		return err
	}
	return verifyTrustedParent(parentPath, parentFD)
}

type acquireHooks struct {
	afterOpen  func(path string)
	afterFlock func(path string)
}

func acquire(dbPath string, kind lockKind, hooks *acquireHooks) (*Handle, error) {
	parentPath, dbBase, err := splitDBPath(dbPath)
	if err != nil {
		return nil, err
	}
	parentFD, err := openTrustedParent(parentPath)
	if err != nil {
		return nil, err
	}
	defer unix.Close(parentFD)
	// Two alias spellings of one SQLite inode must not acquire two different
	// sidecar locks. A first-start Hub is allowed to name a DB that does not yet
	// exist; an existing target must already be an owned, unaliased regular file.
	if kind == writerLock {
		if err := validateDBTarget(parentFD, dbBase, dbPath, true); err != nil {
			return nil, err
		}
	}

	var lockPath string
	switch kind {
	case upgradeLock:
		lockPath = UpgradePath(dbPath)
	case writerLock:
		lockPath = WriterPath(dbPath)
	default:
		return nil, fmt.Errorf("unknown ledger lock kind %q", kind)
	}
	lockBase := dbBase + "." + string(kind) + ".lock"
	fd, err := openLockFile(parentFD, lockBase, lockPath)
	if err != nil {
		return nil, err
	}
	defer func() {
		if fd >= 0 {
			_ = unix.Close(fd)
		}
	}()

	if hooks != nil && hooks.afterOpen != nil {
		hooks.afterOpen(lockPath)
	}
	if err := verifyLockIdentity(parentPath, parentFD, lockPath, lockBase, fd); err != nil {
		return nil, err
	}
	// Change mode through the already-proven descriptor.  Never chmod the path:
	// a same-UID process replacing it must not redirect this mutation.
	if err := unix.Fchmod(fd, 0o600); err != nil {
		return nil, fmt.Errorf("chmod verified lock %s: %w", lockPath, err)
	}
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return nil, fmt.Errorf("%w: %s", ErrContended, lockPath)
		}
		return nil, fmt.Errorf("flock %s: %w", lockPath, err)
	}
	if hooks != nil && hooks.afterFlock != nil {
		hooks.afterFlock(lockPath)
	}
	// This second check closes the pathname-swap window around flock.  Without
	// it, another process could lock a replacement inode at the advertised path
	// while we confidently held an unlinked inode.
	if err := verifyLockIdentity(parentPath, parentFD, lockPath, lockBase, fd); err != nil {
		_ = unix.Flock(fd, unix.LOCK_UN)
		return nil, err
	}
	if kind == writerLock {
		if err := validateDBTarget(parentFD, dbBase, dbPath, true); err != nil {
			_ = unix.Flock(fd, unix.LOCK_UN)
			return nil, err
		}
	}

	f := os.NewFile(uintptr(fd), lockPath)
	if f == nil {
		_ = unix.Flock(fd, unix.LOCK_UN)
		return nil, fmt.Errorf("adopt lock descriptor %s", lockPath)
	}
	fd = -1
	return &Handle{file: f, path: lockPath}, nil
}

func validateDBTarget(parentFD int, base, path string, allowMissing bool) error {
	var relative unix.Stat_t
	if err := unix.Fstatat(parentFD, base, &relative, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if allowMissing && errors.Is(err, unix.ENOENT) {
			for _, suffix := range []string{"-wal", "-shm", "-journal"} {
				sidecarBase := base + suffix
				var sidecar unix.Stat_t
				if sidecarErr := unix.Fstatat(parentFD, sidecarBase, &sidecar, unix.AT_SYMLINK_NOFOLLOW); sidecarErr == nil {
					return fmt.Errorf("ledger %s is missing but SQLite sidecar %s remains", path, suffix)
				} else if !errors.Is(sidecarErr, unix.ENOENT) {
					return fmt.Errorf("lstat missing ledger sidecar %s%s: %w", path, suffix, sidecarErr)
				}
			}
			return nil
		}
		return fmt.Errorf("ledger %s must already exist: %w", path, err)
	}
	var absolute unix.Stat_t
	if err := unix.Lstat(path, &absolute); err != nil {
		return fmt.Errorf("lstat ledger %s: %w", path, err)
	}
	if err := validateOwnedRegular("ledger", path, &relative); err != nil {
		return err
	}
	if err := validateOwnedRegular("ledger", path, &absolute); err != nil {
		return err
	}
	if !sameObject(&relative, &absolute) {
		return fmt.Errorf("ledger %s changed while it was being verified", path)
	}
	return validateExistingSQLiteSidecars(parentFD, base, path)
}

func validateExistingSQLiteSidecars(parentFD int, base, path string) error {
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		sidecarBase := base + suffix
		sidecarPath := path + suffix
		var relative, absolute unix.Stat_t
		relativeErr := unix.Fstatat(parentFD, sidecarBase, &relative, unix.AT_SYMLINK_NOFOLLOW)
		absoluteErr := unix.Lstat(sidecarPath, &absolute)

		if errors.Is(relativeErr, unix.ENOENT) && errors.Is(absoluteErr, unix.ENOENT) {
			continue
		}
		if relativeErr != nil {
			if errors.Is(relativeErr, unix.ENOENT) && absoluteErr == nil {
				return fmt.Errorf("SQLite sidecar %s appeared while it was being verified", sidecarPath)
			}
			return fmt.Errorf("lstat SQLite sidecar %s relative to ledger parent: %w", sidecarPath, relativeErr)
		}
		if absoluteErr != nil {
			if errors.Is(absoluteErr, unix.ENOENT) {
				return fmt.Errorf("SQLite sidecar %s disappeared while it was being verified", sidecarPath)
			}
			return fmt.Errorf("lstat SQLite sidecar %s by absolute path: %w", sidecarPath, absoluteErr)
		}
		if err := validateOwnedRegular("SQLite sidecar", sidecarPath, &relative); err != nil {
			return err
		}
		if err := validateOwnedRegular("SQLite sidecar", sidecarPath, &absolute); err != nil {
			return err
		}
		if !sameObject(&relative, &absolute) {
			return fmt.Errorf("SQLite sidecar %s changed while it was being verified", sidecarPath)
		}
	}
	return nil
}

func splitDBPath(dbPath string) (parent, base string, err error) {
	if dbPath == "" || !filepath.IsAbs(dbPath) {
		return "", "", fmt.Errorf("ledger path must be absolute: %q", dbPath)
	}
	if filepath.Clean(dbPath) != dbPath {
		return "", "", fmt.Errorf("ledger path must be canonical: %q", dbPath)
	}
	parent, base = filepath.Dir(dbPath), filepath.Base(dbPath)
	if base == "." || base == string(filepath.Separator) || base == "" {
		return "", "", fmt.Errorf("ledger path has no filename: %q", dbPath)
	}
	return parent, base, nil
}

func openTrustedParent(parentPath string) (int, error) {
	resolved, err := filepath.EvalSymlinks(parentPath)
	if err != nil {
		return -1, fmt.Errorf("resolve ledger parent %s: %w", parentPath, err)
	}
	if resolved != parentPath {
		return -1, fmt.Errorf("ledger parent must be canonical and contain no symlink: %s", parentPath)
	}
	var pathStat unix.Stat_t
	if err := unix.Lstat(parentPath, &pathStat); err != nil {
		return -1, fmt.Errorf("lstat ledger parent %s: %w", parentPath, err)
	}
	if err := validatePrivateParent(parentPath, &pathStat); err != nil {
		return -1, err
	}
	fd, err := unix.Open(parentPath, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return -1, fmt.Errorf("open ledger parent %s: %w", parentPath, err)
	}
	if err := verifyTrustedParent(parentPath, fd); err != nil {
		_ = unix.Close(fd)
		return -1, err
	}
	return fd, nil
}

func verifyTrustedParent(parentPath string, fd int) error {
	resolved, err := filepath.EvalSymlinks(parentPath)
	if err != nil {
		return fmt.Errorf("re-resolve ledger parent %s: %w", parentPath, err)
	}
	if resolved != parentPath {
		return fmt.Errorf("ledger parent stopped being canonical: %s", parentPath)
	}
	var pathStat, fdStat unix.Stat_t
	if err := unix.Lstat(parentPath, &pathStat); err != nil {
		return fmt.Errorf("re-lstat ledger parent %s: %w", parentPath, err)
	}
	if err := unix.Fstat(fd, &fdStat); err != nil {
		return fmt.Errorf("fstat ledger parent %s: %w", parentPath, err)
	}
	if err := validatePrivateParent(parentPath, &pathStat); err != nil {
		return err
	}
	if err := validatePrivateParent(parentPath, &fdStat); err != nil {
		return err
	}
	if !sameObject(&pathStat, &fdStat) {
		return fmt.Errorf("ledger parent %s changed while it was being verified", parentPath)
	}
	return nil
}

func validatePrivateParent(path string, stat *unix.Stat_t) error {
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
		return fmt.Errorf("ledger parent is not a directory: %s", path)
	}
	if stat.Uid != uint32(os.Geteuid()) {
		return fmt.Errorf("ledger parent is not owned by euid %d: %s", os.Geteuid(), path)
	}
	if stat.Mode&0o077 != 0 {
		return fmt.Errorf("ledger parent must be private (no group/other permissions): %s", path)
	}
	return nil
}

func openLockFile(parentFD int, base, path string) (int, error) {
	// First classify an existing path without following it, so a FIFO/device or
	// hardlink is rejected before a read-write open can have side effects.
	for attempts := 0; attempts < 3; attempts++ {
		var stat unix.Stat_t
		err := unix.Fstatat(parentFD, base, &stat, unix.AT_SYMLINK_NOFOLLOW)
		switch {
		case err == nil:
			if err := validateOwnedRegular("lock", path, &stat); err != nil {
				return -1, err
			}
			fd, err := unix.Openat(parentFD, base,
				unix.O_RDWR|unix.O_NONBLOCK|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
			if err != nil {
				return -1, fmt.Errorf("open lock %s without truncation: %w", path, err)
			}
			return fd, nil
		case errors.Is(err, unix.ENOENT):
			fd, createErr := unix.Openat(parentFD, base,
				unix.O_RDWR|unix.O_NONBLOCK|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
			if createErr == nil {
				return fd, nil
			}
			if errors.Is(createErr, unix.EEXIST) {
				continue
			}
			return -1, fmt.Errorf("create lock %s safely: %w", path, createErr)
		default:
			return -1, fmt.Errorf("lstat lock %s: %w", path, err)
		}
	}
	return -1, fmt.Errorf("lock %s changed repeatedly while opening", path)
}

func verifyLockIdentity(parentPath string, parentFD int, path, base string, fd int) error {
	if err := verifyTrustedParent(parentPath, parentFD); err != nil {
		return err
	}
	var relative, absolute, opened unix.Stat_t
	if err := unix.Fstatat(parentFD, base, &relative, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return fmt.Errorf("re-lstat lock %s: %w", path, err)
	}
	if err := unix.Lstat(path, &absolute); err != nil {
		return fmt.Errorf("re-lstat lock %s by absolute path: %w", path, err)
	}
	if err := unix.Fstat(fd, &opened); err != nil {
		return fmt.Errorf("fstat lock %s: %w", path, err)
	}
	for _, stat := range []*unix.Stat_t{&relative, &absolute, &opened} {
		if err := validateOwnedRegular("lock", path, stat); err != nil {
			return err
		}
	}
	if !sameObject(&relative, &absolute) || !sameObject(&relative, &opened) {
		return fmt.Errorf("lock %s path/fd identity changed", path)
	}
	return nil
}

func validateOwnedRegular(kind, path string, stat *unix.Stat_t) error {
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return fmt.Errorf("%s must be a regular file (symlinks are refused): %s", kind, path)
	}
	if stat.Uid != uint32(os.Geteuid()) {
		return fmt.Errorf("%s is not owned by euid %d: %s", kind, os.Geteuid(), path)
	}
	if stat.Nlink != 1 {
		return fmt.Errorf("%s must have link count 1 (hardlinks are refused): %s", kind, path)
	}
	return nil
}

func sameObject(a, b *unix.Stat_t) bool {
	return a.Dev == b.Dev && a.Ino == b.Ino
}
