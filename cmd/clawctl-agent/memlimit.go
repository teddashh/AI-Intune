package main

import (
	"log"
	"math"
	"os"
	"runtime/debug"
	"strings"
)

// softMemoryLimitBytes is the Go soft limit used when the operator did not
// set GOMEMLIMIT. It is below the unit's MemoryHigh so the runtime returns
// memory before the kernel starts reclaiming the whole cgroup.
const softMemoryLimitBytes int64 = 96 << 20

// applySoftMemoryLimit sets the process soft memory limit for the long-running
// agent. One-shot subcommands do not call it. A set GOMEMLIMIT is left to the
// runtime, which already applied it at startup; this function only logs the
// effective value.
func applySoftMemoryLimit() {
	if strings.TrimSpace(os.Getenv("GOMEMLIMIT")) == "" {
		debug.SetMemoryLimit(softMemoryLimitBytes)
		log.Printf("soft memory limit set to 96 MiB (%d bytes); GOMEMLIMIT is unset", softMemoryLimitBytes)
		return
	}
	effective := debug.SetMemoryLimit(-1)
	if effective == math.MaxInt64 {
		log.Printf("soft memory limit respects GOMEMLIMIT=%q (runtime limit is unlimited)", os.Getenv("GOMEMLIMIT"))
		return
	}
	log.Printf("soft memory limit respects GOMEMLIMIT=%q (effective %d bytes)", os.Getenv("GOMEMLIMIT"), effective)
}
