package probe

import (
	"fmt"
	"math"
	"strings"
)

// IMAGE_FILE_MACHINE values returned by IsWow64Process2's nativeMachine.
const (
	windowsMachineAMD64 = 0x8664
	windowsMachineARM64 = 0xaa64
)

type windowsVersionFacts struct {
	Major          uint32
	Minor          uint32
	Build          uint32
	ProductName    string
	DisplayVersion string
}

func windowsDisplayName(facts windowsVersionFacts) string {
	kernel := windowsKernelRelease(facts.Major, facts.Minor, facts.Build)
	if kernel == "" {
		return ""
	}
	// NT builds alone do not distinguish Windows clients from Server.
	// Keep the measured version and only enrich with a Windows product name.
	name := "Windows NT " + kernel
	product := strings.TrimSpace(facts.ProductName)
	if !strings.HasPrefix(product, "Windows ") && !strings.HasPrefix(product, "Microsoft Windows ") {
		return name
	}
	display := strings.TrimSpace(facts.DisplayVersion)
	if strings.ContainsAny(product+display, "\x00\r\n\t") {
		return name
	}
	if display != "" && !strings.Contains(product, display) {
		product += " " + display
	}
	return name + " (" + product + ")"
}

func windowsKernelRelease(major, minor, build uint32) string {
	if major == 0 && minor == 0 && build == 0 {
		return ""
	}
	return fmt.Sprintf("%d.%d.%d", major, minor, build)
}

func windowsNativeArch(query func() (uint16, error)) string {
	machine, err := query()
	if err != nil {
		return ""
	}
	switch machine {
	case windowsMachineAMD64:
		return "x86_64"
	case windowsMachineARM64:
		return "aarch64"
	default:
		return ""
	}
}

func tickCountUptimeSeconds(ms uint64) *int64 {
	sec := int64(ms / 1000)
	return &sec
}

func windowsDiskBytes(free, total uint64) (int64, int64) {
	measuredTotal, measuredFree := windowsMemoryBytes(total, free)
	return measuredFree, measuredTotal
}

func windowsMemoryBytes(total, available uint64) (int64, int64) {
	if total == 0 || total > math.MaxInt64 || available > total {
		return 0, 0
	}
	return int64(total), int64(available)
}
