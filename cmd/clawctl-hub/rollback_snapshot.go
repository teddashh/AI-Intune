package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/teddashh/AI-Intune/internal/ledgerlock"
	"github.com/teddashh/AI-Intune/internal/store"
	"golang.org/x/sys/unix"
)

func runRollbackSnapshot(argv []string, out io.Writer) error {
	fs := flag.NewFlagSet("rollback-snapshot", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	capability := fs.Bool("rollback-snapshot", false, "建立一致且已驗證靜止的 rollback snapshot")
	dbPath := fs.String("db", defaultDB(), "SQLite 檔位置")
	destination := fs.String("out", "", "snapshot 目的檔；不得已存在")
	maintenanceHeld := fs.Bool("maintenance-already-held", false, "upgrade script 已建立並驗證 maintenance marker")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	if !*capability {
		return errors.New("缺少 --rollback-snapshot")
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("多餘參數：%s", strings.Join(fs.Args(), " "))
	}
	if strings.TrimSpace(*destination) == "" {
		return errors.New("rollback snapshot 缺少 --out")
	}
	// Marker must precede every read of the live database. On any later error it
	// remains intentionally: only the controlling upgrade script, after putting
	// the old binary and untouched DB back into service, may remove it.
	if *maintenanceHeld {
		if err := validateUpgradeMaintenanceMarker(*dbPath); err != nil {
			return err
		}
	} else {
		if err := createUpgradeMaintenanceMarker(*dbPath); err != nil {
			return err
		}
	}
	if err := ledgerlock.ValidateUpgradeTarget(*dbPath); err != nil {
		return fmt.Errorf("rollback snapshot ledger path 拒絕：%w", err)
	}
	present, err := store.CreateRollbackSnapshot(*dbPath, *destination)
	if err != nil {
		return err
	}
	if !present {
		fmt.Fprintln(out, "rollback snapshot：換版前沒有 DB；已確認沒有孤兒 WAL/SHM/journal，ledger 視為空且已靜止")
		return nil
	}
	fmt.Fprintln(out, "rollback snapshot：一致單檔 snapshot 已完成 quick_check，ledger 已靜止")
	return nil
}

func runUpgradeMaintenanceBegin(argv []string, out io.Writer, disableCurrentExecutable func() error) error {
	fs := flag.NewFlagSet("upgrade-maintenance-begin", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	capability := fs.Bool("upgrade-maintenance-begin", false, "在任何最終 process drain 前原子建立 upgrade maintenance marker")
	dbPath := fs.String("db", defaultDB(), "SQLite 檔位置")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	if !*capability {
		return errors.New("缺少 --upgrade-maintenance-begin")
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("多餘參數：%s", strings.Join(fs.Args(), " "))
	}
	if disableCurrentExecutable == nil {
		return errors.New("upgrade maintenance executable disabler 未初始化")
	}
	// Make the invoking inode non-executable before publishing the marker. If
	// power is lost at either boundary, no .new/installed path can later be used
	// as an unfenced serve process against an uncertain rollback coordinate.
	if err := disableCurrentExecutable(); err != nil {
		return fmt.Errorf("停用 maintenance helper executable：%w", err)
	}
	if err := createUpgradeMaintenanceMarker(*dbPath); err != nil {
		return fmt.Errorf("executable 已停用；建立 marker：%w", err)
	}
	fmt.Fprintln(out, "upgrade maintenance：marker 已建立；必須先停用舊 binary 並重查 process=0，才能建立 snapshot")
	return nil
}

func disableCurrentExecutableForMaintenance() error {
	// /proc/self/exe is a kernel-owned reference to the inode that is actually
	// running.  Opening that reference works both for the installed/.new path
	// (link count 1) and for the upgrade script's already-unlinked private probe
	// (link count 0), without trusting a pathname returned by os.Executable.
	fd, err := unix.Open("/proc/self/exe", unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open running executable：%w", err)
	}
	defer unix.Close(fd)
	var opened unix.Stat_t
	if err := unix.Fstat(fd, &opened); err != nil {
		return fmt.Errorf("fstat running executable：%w", err)
	}
	if opened.Mode&unix.S_IFMT != unix.S_IFREG || opened.Uid != uint32(os.Geteuid()) || opened.Nlink > 1 {
		return fmt.Errorf("running executable 必須是目前使用者持有、link count<=1 的 regular file")
	}
	if err := unix.Fchmod(fd, 0o600); err != nil {
		return fmt.Errorf("chmod running executable 0600：%w", err)
	}
	return nil
}
