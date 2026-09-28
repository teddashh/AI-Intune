package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

const upgradeMaintenanceSuffix = ".upgrade-maintenance"
const upgradeMaintenanceContents = "clawctl-hub upgrade maintenance\n"

func upgradeMaintenanceMarker(dbPath string) string { return dbPath + upgradeMaintenanceSuffix }

// createUpgradeMaintenanceMarker is deliberately O_EXCL. A marker left by an
// interrupted upgrade is state we do not understand, not litter to tidy up.
func createUpgradeMaintenanceMarker(dbPath string) error {
	marker := upgradeMaintenanceMarker(dbPath)
	f, err := os.OpenFile(marker, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("Hub 升級維護中；marker=%s", marker)
		}
		return fmt.Errorf("建立 Hub 升級維護 marker：%w", err)
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return fmt.Errorf("設定 Hub 升級維護 marker 權限失敗；marker=%s：%w", marker, err)
	}
	if _, err := f.WriteString(upgradeMaintenanceContents); err != nil {
		_ = f.Close()
		return fmt.Errorf("寫入 Hub 升級維護 marker 失敗；marker=%s：%w", marker, err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("sync Hub 升級維護 marker 失敗；marker=%s：%w", marker, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("關閉 Hub 升級維護 marker 失敗；marker=%s：%w", marker, err)
	}
	dir, err := os.Open(filepath.Dir(marker))
	if err != nil {
		return fmt.Errorf("開啟 Hub 升級維護 marker 目錄失敗；marker=%s：%w", marker, err)
	}
	if err := dir.Sync(); err != nil {
		_ = dir.Close()
		return fmt.Errorf("sync Hub 升級維護 marker 目錄失敗；marker=%s：%w", marker, err)
	}
	if err := dir.Close(); err != nil {
		return fmt.Errorf("關閉 Hub 升級維護 marker 目錄失敗；marker=%s：%w", marker, err)
	}
	return nil
}

// validateUpgradeMaintenanceMarker proves that a marker established by the
// controlling upgrade still names one private, owned inode.  Snapshot mode
// uses this after the script has made every dormant binary non-executable and
// drained processes; accepting an arbitrary pre-existing pathname would turn
// a stale or swapped marker into permission to take a rollback coordinate.
func validateUpgradeMaintenanceMarker(dbPath string) error {
	marker := upgradeMaintenanceMarker(dbPath)
	before, err := os.Lstat(marker)
	if err != nil {
		return fmt.Errorf("讀取既有 Hub 升級維護 marker：%w", err)
	}
	if err := validateUpgradeMaintenanceMarkerInfo(before); err != nil {
		return fmt.Errorf("既有 Hub 升級維護 marker 不安全：%w", err)
	}
	fd, err := unix.Open(marker, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return fmt.Errorf("開啟既有 Hub 升級維護 marker：%w", err)
	}
	f := os.NewFile(uintptr(fd), marker)
	if f == nil {
		_ = unix.Close(fd)
		return errors.New("採用既有 Hub 升級維護 marker fd 失敗")
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil {
		return fmt.Errorf("確認既有 Hub 升級維護 marker fd：%w", err)
	}
	if err := validateUpgradeMaintenanceMarkerInfo(opened); err != nil {
		return fmt.Errorf("既有 Hub 升級維護 marker fd 不安全：%w", err)
	}
	if !os.SameFile(before, opened) {
		return errors.New("既有 Hub 升級維護 marker path/fd identity 已改變")
	}
	contents, err := io.ReadAll(io.LimitReader(f, int64(len(upgradeMaintenanceContents)+1)))
	if err != nil {
		return fmt.Errorf("讀取既有 Hub 升級維護 marker 內容：%w", err)
	}
	if string(contents) != upgradeMaintenanceContents {
		return errors.New("既有 Hub 升級維護 marker 內容不符")
	}
	after, err := os.Lstat(marker)
	if err != nil {
		return fmt.Errorf("重查既有 Hub 升級維護 marker：%w", err)
	}
	if err := validateUpgradeMaintenanceMarkerInfo(after); err != nil {
		return fmt.Errorf("重查既有 Hub 升級維護 marker 不安全：%w", err)
	}
	if !os.SameFile(opened, after) {
		return errors.New("既有 Hub 升級維護 marker 在讀取後被替換")
	}
	return nil
}

func validateUpgradeMaintenanceMarkerInfo(info fs.FileInfo) error {
	if !info.Mode().IsRegular() {
		return fmt.Errorf("必須是 regular file（實際 %s）", info.Mode().Type())
	}
	if info.Mode().Perm() != 0o600 {
		return fmt.Errorf("權限必須是 0600（實際 %#o）", info.Mode().Perm())
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("無法取得 inode owner/link count")
	}
	if stat.Uid != uint32(os.Geteuid()) || stat.Nlink != 1 {
		return fmt.Errorf("必須由目前使用者持有且 link count=1（uid=%d links=%d）", stat.Uid, stat.Nlink)
	}
	return nil
}

// rejectCLIWhileUpgradeMaintenance is called before normal CLI dispatch.
// Serving HTTP is intentionally allowed: the per-request middleware keeps it
// read-only until the marker is removed after verification.
func rejectCLIWhileUpgradeMaintenance(argv []string) error {
	if len(argv) == 0 || strings.HasPrefix(argv[0], "-") || argv[0] == "version" {
		return nil
	}
	defaultPath := defaultDB()
	dbPath := defaultPath
	for i := 1; i < len(argv); i++ {
		switch {
		case argv[i] == "--db" && i+1 < len(argv) && argv[i+1] != "":
			dbPath = argv[i+1]
			i++
		case strings.HasPrefix(argv[i], "--db=") && strings.TrimPrefix(argv[i], "--db=") != "":
			dbPath = strings.TrimPrefix(argv[i], "--db=")
		}
	}
	paths := []string{defaultPath}
	if dbPath != defaultPath {
		paths = append(paths, dbPath)
	}
	for _, path := range paths {
		marker := upgradeMaintenanceMarker(path)
		if _, err := os.Lstat(marker); err == nil {
			return fmt.Errorf("Hub 升級維護中；一般 CLI 停用；marker=%s", marker)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("Hub 升級維護 marker 狀態未知：%w", err)
		}
	}
	return nil
}

// machine channel, machines/audit/report-changes reads, job/artifact list/show,
// and deployment list/show/preview resolve transport before consulting local state. Their
// normal HTTP mode can target a different Hub and must not be blocked by an
// unrelated marker beside this workstation's default DB. The explicit --db
// branch acquires lifecycle+writer locks and calls
// rejectDBWhileUpgradeMaintenance for its exact target instead. Keep job
// create and deployment mutations outside this exception.
func rejectTopLevelCLIWhileUpgradeMaintenance(command string, argv []string) error {
	if command == "machine" || command == "machines" || command == "audit" {
		return nil
	}
	if command == "report" && len(argv) >= 2 && argv[1] == "changes" {
		return nil
	}
	if command == "job" && len(argv) >= 2 && (argv[1] == "list" || argv[1] == "show") {
		return nil
	}
	if command == "artifact" && len(argv) >= 2 && (argv[1] == "list" || argv[1] == "show") {
		return nil
	}
	if command == "artifact" && len(argv) >= 3 && argv[1] == "fetch" &&
		(argv[2] == "list" || argv[2] == "show") {
		return nil
	}
	if command == "deployment" && len(argv) >= 2 &&
		(argv[1] == "list" || argv[1] == "show" || argv[1] == "preview") {
		return nil
	}
	// Catalog and settings commands use the authenticated HTTP operator API and
	// never open the workstation's local Hub ledger.
	if command == "catalog" || command == "settings" {
		return nil
	}
	return rejectCLIWhileUpgradeMaintenance(argv)
}

func rejectDBWhileUpgradeMaintenance(dbPath string) error {
	marker := upgradeMaintenanceMarker(dbPath)
	if _, err := os.Lstat(marker); err == nil {
		return fmt.Errorf("Hub 升級維護中；direct DB 停用；marker=%s", marker)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("Hub 升級維護 marker 狀態未知：%w", err)
	}
	return nil
}

// maintenanceMiddleware keeps the Hub readable while an upgrade owns the
// database.  The marker is checked for every mutating request so that the same
// process starts accepting writes again as soon as the upgrader removes it.
func maintenanceMiddleware(markerPath string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			next.ServeHTTP(w, r)
			return
		}

		_, err := os.Lstat(markerPath)
		if err == nil || !errors.Is(err, fs.ErrNotExist) {
			w.Header().Set("Retry-After", "5")
			http.Error(w, "Hub 正在維護，請稍後再試", http.StatusServiceUnavailable)
			return
		}

		next.ServeHTTP(w, r)
	})
}
