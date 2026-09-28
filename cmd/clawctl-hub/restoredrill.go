package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/teddashh/AI-Intune/internal/restoredrill"
)

const restoreDrillEvery = 90 * 24 * time.Hour

// drillResult is the presentation adapter used by the dashboard/metrics stamp
// contract. Backup verification itself lives in the restoredrill package and
// product invocations enter through the operator service.
type drillResult struct {
	Backup   string
	Machines int
	Expected int
}

func drillStampPath(dbPath string) string {
	if value := os.Getenv("CLAWCTL_RESTORE_DRILL_STAMP"); value != "" {
		return value
	}
	return filepath.Join(filepath.Dir(dbPath), "restore-drill.stamp")
}

func writeDrillStamp(path string, now time.Time, result drillResult) error {
	return restoredrill.WriteStamp(path, now, restoredrill.Result{
		Backup:   restoredrill.Backup{Name: filepath.Base(result.Backup)},
		Machines: result.Machines, Expected: result.Expected,
	})
}

func readDrillStamp(path string) (time.Time, bool) { return restoredrill.ReadStamp(path) }

func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.0f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}
