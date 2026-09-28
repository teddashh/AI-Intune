package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestClassifyTopLevelRejectsUnknownPositionalInsteadOfServing(t *testing.T) {
	for _, argv := range [][]string{
		{"rollback-snapshot", "--help"},
		{"serve"},
		{"machins"},
	} {
		if command, err := classifyTopLevel(argv); err == nil || command != "" {
			t.Errorf("argv=%q classified as command=%q err=%v; typo must not become serve", argv, command, err)
		}
	}
}

func TestClassifyTopLevelPreservesCommandsAndServeFlags(t *testing.T) {
	for _, name := range []string{
		"enroll-token", "machines", "audit", "retire", "report", "tickets", "tailnet",
		"prune", "restore-drill", "job", "machine", "deployment",
		"artifact", "catalog", "verifier", "settings", "compliance", "version",
	} {
		command, err := classifyTopLevel([]string{name, "--help"})
		if err != nil || command != name {
			t.Errorf("known command %q classified as command=%q err=%v", name, command, err)
		}
	}
	for _, argv := range [][]string{nil, {"--listen", "127.0.0.1:0"}, {"-h"}} {
		command, err := classifyTopLevel(argv)
		if err != nil || command != "" {
			t.Errorf("serve argv=%q classified as command=%q err=%v", argv, command, err)
		}
	}
}

func TestServeRejectsPositionalsLeftAfterValidFlags(t *testing.T) {
	if err := rejectUnexpectedServePositionals([]string{"rollback-snapshot", "--help"}); err == nil {
		t.Fatal("serve accepted positional arguments")
	}
	if err := rejectUnexpectedServePositionals(nil); err != nil {
		t.Fatalf("serve rejected an empty positional list: %v", err)
	}
}

// TestUnknownCommandProcessHelper runs the real package main in an isolated
// child. It must never be selected by an ordinary test invocation.
func TestUnknownCommandProcessHelper(t *testing.T) {
	if os.Getenv("CLAWCTL_DISPATCH_TEST_HELPER") != "1" {
		return
	}
	var argv []string
	if err := json.Unmarshal([]byte(os.Getenv("CLAWCTL_DISPATCH_TEST_ARGS")), &argv); err != nil {
		t.Fatalf("decode helper argv: %v", err)
	}
	os.Args = append([]string{"clawctl-hub"}, argv...)
	main()
}

func TestUnknownPositionalExitsBeforeDatabaseOrListener(t *testing.T) {
	for _, tc := range []struct {
		name string
		argv []string
		want string
	}{
		{name: "first argument typo", argv: []string{"rollback-snapshot", "--help"}, want: "不認得命令"},
		{name: "positional after serve flags", argv: []string{"--listen", "127.0.0.1:0", "rollback-snapshot", "--help"}, want: "不接受 positional"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			dbPath := filepath.Join(dir, "must-not-exist.sqlite")
			rawArgs, err := json.Marshal(tc.argv)
			if err != nil {
				t.Fatal(err)
			}

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestUnknownCommandProcessHelper$")
			cmd.Env = dispatchHelperEnv(dbPath, string(rawArgs))
			output, runErr := cmd.CombinedOutput()
			if ctx.Err() != nil {
				t.Fatalf("typo entered serve and stayed alive instead of exiting: %v\n%s", ctx.Err(), output)
			}
			var exitErr *exec.ExitError
			if !errors.As(runErr, &exitErr) || exitErr.ExitCode() == 0 {
				t.Fatalf("typo exit err=%v, want a non-zero exit\n%s", runErr, output)
			}
			if !strings.Contains(string(output), tc.want) {
				t.Fatalf("error does not explain dispatch rejection; want %q in:\n%s", tc.want, output)
			}
			for _, path := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
				if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("unknown command touched database path %s: %v", path, err)
				}
			}
		})
	}
}

func TestBareDeploymentCommandExitsNonzeroWithUsage(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "must-not-exist.sqlite")
	rawArgs, err := json.Marshal([]string{"deployment"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestUnknownCommandProcessHelper$")
	cmd.Env = dispatchHelperEnv(dbPath, string(rawArgs))
	output, runErr := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("bare deployment command did not exit: %v\n%s", ctx.Err(), output)
	}
	var exitErr *exec.ExitError
	if !errors.As(runErr, &exitErr) || exitErr.ExitCode() == 0 {
		t.Fatalf("bare deployment exit err=%v, want non-zero\n%s", runErr, output)
	}
	for _, want := range []string{"用法：clawctl-hub deployment", "必須指定 subcommand"} {
		if !strings.Contains(string(output), want) {
			t.Fatalf("bare deployment output missing %q:\n%s", want, output)
		}
	}
	for _, path := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("bare deployment command touched database path %s: %v", path, err)
		}
	}
}

func TestUnsafeOperatorAuthExitsBeforeDatabaseOpen(t *testing.T) {
	for _, tc := range []struct {
		name string
		argv []string
	}{
		{name: "no args uses fail-closed default", argv: nil},
		{
			name: "explicit loopback listener",
			argv: []string{
				"--listen", "127.0.0.1:8787",
				"--operator-capability-prefix", "example.com/cap/clawctl",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			dbPath := filepath.Join(dir, "must-not-exist.sqlite")
			rawArgs, err := json.Marshal(tc.argv)
			if err != nil {
				t.Fatal(err)
			}

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestUnknownCommandProcessHelper$")
			cmd.Env = dispatchHelperEnv(dbPath, string(rawArgs))
			output, runErr := cmd.CombinedOutput()
			if ctx.Err() != nil {
				t.Fatalf("unsafe operator auth entered serve instead of exiting: %v\n%s", ctx.Err(), output)
			}
			var exitErr *exec.ExitError
			if !errors.As(runErr, &exitErr) || exitErr.ExitCode() == 0 {
				t.Fatalf("unsafe operator auth exit err=%v, want a non-zero exit\n%s", runErr, output)
			}
			for _, want := range []string{"operator auth 設定不合法", "Tailscale listener IP"} {
				if !strings.Contains(string(output), want) {
					t.Fatalf("error does not explain fail-closed operator auth; want %q in:\n%s", want, output)
				}
			}
			for _, path := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
				if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("unsafe operator auth touched database path %s: %v", path, err)
				}
			}
		})
	}
}

func TestOperatorAuthCheckProcessExitsBeforeDatabaseOpen(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "must-not-exist.sqlite")
	argv := []string{
		"--operator-auth-check",
		"--listen", "127.0.0.1:8787",
		"--operator-capability-prefix", "example.com/cap/clawctl",
	}
	rawArgs, err := json.Marshal(argv)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestUnknownCommandProcessHelper$")
	cmd.Env = dispatchHelperEnv(dbPath, string(rawArgs))
	output, runErr := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("operator auth preflight entered serve: %v\n%s", ctx.Err(), output)
	}
	var exitErr *exec.ExitError
	if !errors.As(runErr, &exitErr) || exitErr.ExitCode() == 0 {
		t.Fatalf("preflight exit err=%v, want non-zero\n%s", runErr, output)
	}
	for _, want := range []string{"operator auth preflight 失敗", "operator console"} {
		if !strings.Contains(string(output), want) {
			t.Fatalf("preflight error missing %q:\n%s", want, output)
		}
	}
	for _, path := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("operator auth preflight touched database path %s: %v", path, err)
		}
	}
}

func TestConflictingPublicURLExitsBeforeDatabaseOpen(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "must-not-exist.sqlite")
	t.Setenv("CLAWCTL_PUBLIC_URL", "https://host-name-that-boundary-would-reject.example")
	argv := []string{
		"--listen", "100.64.200.2:8787",
		"--operator-capability-prefix", "example.com/cap/clawctl",
	}
	rawArgs, err := json.Marshal(argv)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestUnknownCommandProcessHelper$")
	cmd.Env = dispatchHelperEnv(dbPath, string(rawArgs))
	output, runErr := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("conflicting public URL entered serve instead of exiting: %v\n%s", ctx.Err(), output)
	}
	var exitErr *exec.ExitError
	if !errors.As(runErr, &exitErr) || exitErr.ExitCode() == 0 {
		t.Fatalf("conflicting public URL exit err=%v, want a non-zero exit\n%s", runErr, output)
	}
	for _, want := range []string{"operator console 位址設定不合法", "CLAWCTL_PUBLIC_URL", "不一致"} {
		if !strings.Contains(string(output), want) {
			t.Fatalf("error does not explain authority conflict; want %q in:\n%s", want, output)
		}
	}
	for _, path := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("conflicting public URL touched database path %s: %v", path, err)
		}
	}
}

func dispatchHelperEnv(dbPath, argvJSON string) []string {
	env := make([]string, 0, len(os.Environ())+3)
	for _, entry := range os.Environ() {
		if strings.HasPrefix(entry, "CLAWCTL_DB=") ||
			strings.HasPrefix(entry, "CLAWCTL_DISPATCH_TEST_HELPER=") ||
			strings.HasPrefix(entry, "CLAWCTL_DISPATCH_TEST_ARGS=") {
			continue
		}
		env = append(env, entry)
	}
	return append(env,
		"CLAWCTL_DB="+dbPath,
		"CLAWCTL_DISPATCH_TEST_HELPER=1",
		"CLAWCTL_DISPATCH_TEST_ARGS="+argvJSON,
	)
}
