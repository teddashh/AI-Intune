// Package processenv builds subprocess environments without leaking service-manager control channels.
package processenv

import (
	"context"
	"os"
	"os/exec"
	"strings"
)

// CommandContext creates a subprocess that cannot report readiness or feed the
// watchdog for the service process that launched it.
func CommandContext(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = WithoutSystemdNotifications(os.Environ())
	return cmd
}

// WithoutSystemdNotifications removes the variables that let a child process
// send readiness or watchdog messages on behalf of its parent service.
func WithoutSystemdNotifications(environment []string) []string {
	result := make([]string, 0, len(environment))
	for _, variable := range environment {
		name, _, hasValue := strings.Cut(variable, "=")
		if hasValue {
			switch name {
			case "NOTIFY_SOCKET", "WATCHDOG_PID", "WATCHDOG_USEC":
				continue
			}
		}
		result = append(result, variable)
	}
	return result
}
