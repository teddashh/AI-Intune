//go:build linux

package scriptcatalog

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"hash"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

type output struct {
	buf    bytes.Buffer
	digest hash.Hash
	cap    int
	count  int64
}

func (w *output) Write(p []byte) (int, error) {
	n := len(p)
	_, _ = w.digest.Write(p)
	w.count += int64(n)
	remaining := w.cap - w.buf.Len()
	if remaining > 0 {
		if remaining > n {
			remaining = n
		}
		_, _ = w.buf.Write(p[:remaining])
	}
	return n, nil
}
func (w *output) text() string {
	s := w.buf.String()
	if w.count > int64(w.cap) {
		// A token crossing the cap must never escape as an unrecognized fragment.
		if i := strings.LastIndexAny(s, "\n\r\t "); i >= 0 {
			s = s[:i+1]
		} else {
			s = ""
		}
	}
	s = Redact(s)
	if len(s) > w.cap {
		s = s[:w.cap]
	}
	return strings.ToValidUTF8(s, "")
}

// Run never accepts a command or path from a job. The interpreter and script
// path are fixed, and JSON args travel exclusively on stdin.
func Run(ctx context.Context, e Entry, args []byte, timeout int) (Result, error) {
	var result Result
	if os.Geteuid() == 0 || e.RunsAs != "agent-user" || !e.AllowsOS("linux") || timeout < 1 || timeout > e.MaxTimeout || e.OutputCap < 1 || e.OutputCap > 64<<10 || Digest(e.Bytes) != e.SHA256 {
		return result, fmt.Errorf("invalid catalog execution constraints")
	}
	canonical, err := e.Validate(args)
	if err != nil {
		return result, err
	}
	dir, err := os.MkdirTemp("", "clawctl-script-*")
	if err != nil {
		return result, fmt.Errorf("create private script directory")
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "script.sh")
	if os.WriteFile(path, e.Bytes, 0600) != nil {
		return result, fmt.Errorf("write embedded script")
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sh", path)
	cmd.Dir = dir
	cmd.Env = []string{"PATH=/usr/bin:/bin", "LANG=C", "LC_ALL=C", "HOME=" + dir, "TMPDIR=" + dir}
	cmd.Stdin = bytes.NewReader(canonical)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = time.Second
	stdout := output{digest: sha256.New(), cap: e.OutputCap}
	stderr := output{digest: sha256.New(), cap: e.OutputCap}
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	start := time.Now()
	err = cmd.Run()
	// Also dispose of descendants after a parent exits normally.
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	result.DurationMS = time.Since(start).Milliseconds()
	result.ExitCode = 0
	if err != nil {
		result.ExitCode = -1
		if cmd.ProcessState != nil {
			result.ExitCode = cmd.ProcessState.ExitCode()
		}
	}
	result.TimedOut = ctx.Err() == context.DeadlineExceeded
	result.Stdout, result.Stderr = stdout.text(), stderr.text()
	result.StdoutTruncated, result.StderrTruncated = stdout.count > int64(e.OutputCap), stderr.count > int64(e.OutputCap)
	result.StdoutSHA256, result.StderrSHA256 = fmt.Sprintf("%x", stdout.digest.Sum(nil)), fmt.Sprintf("%x", stderr.digest.Sum(nil))
	return result, nil
}
