package probe

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/model"
)

func writeCountingCommand(t *testing.T, path, counter, stdout string, exitCode int) {
	t.Helper()
	body := "#!/bin/sh\nprintf 'x\\n' >> '" + counter + "'\n"
	if stdout != "" {
		body += "printf '%s\\n' '" + stdout + "'\n"
	}
	body += fmt.Sprintf("exit %d\n", exitCode)
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

func commandRuns(t *testing.T, counter string) int {
	t.Helper()
	b, err := os.ReadFile(counter)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return bytes.Count(b, []byte("x\n"))
}

func TestVersionProbeCachesUntilTheExecutableChanges(t *testing.T) {
	dir := t.TempDir()
	counter := filepath.Join(dir, "runs")
	tool := filepath.Join(dir, "tool")
	writeCountingCommand(t, tool, counter, "1.2.3", 0)
	dirs := []string{dir}

	out, _, err := runIn(context.Background(), quickTimeout, dirs, tool, "--version")
	if err != nil || out != "1.2.3" {
		t.Fatalf("first probe = %q, %v", out, err)
	}
	out, _, err = runIn(context.Background(), quickTimeout, dirs, tool, "--version")
	if err != nil || out != "1.2.3" {
		t.Fatalf("second probe = %q, %v", out, err)
	}
	if got := commandRuns(t, counter); got != 1 {
		t.Fatalf("two version probes exec'd %d times; a cache hit must not re-exec", got)
	}

	// cliTool is the observation path Collect uses for --version.
	name := "zzz-version-cache"
	linked := filepath.Join(dir, name)
	if err := os.Symlink(tool, linked); err != nil {
		t.Fatal(err)
	}
	sp := searchPath{Dirs: dirs, Source: model.PathSourceDaemon}
	first := cliTool(context.Background(), name, nil, model.ProcessScanUnavailable, sp)
	second := cliTool(context.Background(), name, nil, model.ProcessScanUnavailable, sp)
	if first.VersionReported != "1.2.3" || second.VersionReported != first.VersionReported || second.VersionRaw != first.VersionRaw {
		t.Fatalf("observation version changed across a cache hit: %+v then %+v", first, second)
	}
	if got := commandRuns(t, counter); got != 1 {
		t.Fatalf("cliTool re-exec'd the same resolved file; runs=%d", got)
	}

	later := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(tool, later, later); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runIn(context.Background(), quickTimeout, dirs, tool, "--version"); err != nil {
		t.Fatal(err)
	}
	if got := commandRuns(t, counter); got != 2 {
		t.Fatalf("touching the binary did not re-exec; runs=%d", got)
	}

	if err := os.Remove(tool); err != nil {
		t.Fatal(err)
	}
	// A different length changes size. A same-size rewrite can reuse the inode
	// and, when the clock does not advance, the original mtime, which collides
	// with the pre-touch cache entry.
	writeCountingCommand(t, tool, counter, "9.9.9-replaced", 0)
	replacedAt := time.Now().Add(4 * time.Second)
	if err := os.Chtimes(tool, replacedAt, replacedAt); err != nil {
		t.Fatal(err)
	}
	out, _, err = runIn(context.Background(), quickTimeout, dirs, tool, "--version")
	if err != nil || out != "9.9.9-replaced" {
		t.Fatalf("replaced binary = %q, %v", out, err)
	}
	if got := commandRuns(t, counter); got != 3 {
		t.Fatalf("replacing the binary did not re-exec; runs=%d", got)
	}
}

func TestVersionProbeRetriesAFailureAndThenCachesSuccess(t *testing.T) {
	dir := t.TempDir()
	counter := filepath.Join(dir, "runs")
	tool := filepath.Join(dir, "tool")
	writeCountingCommand(t, tool, counter, "", 1)
	dirs := []string{dir}

	if _, _, err := runIn(context.Background(), quickTimeout, dirs, tool, "--version"); err == nil {
		t.Fatal("failing tool returned success")
	}
	if _, _, err := runIn(context.Background(), quickTimeout, dirs, tool, "--version"); err == nil {
		t.Fatal("second failure was cached as success")
	}
	if got := commandRuns(t, counter); got != 2 {
		t.Fatalf("failure was not retried; runs=%d", got)
	}

	writeCountingCommand(t, tool, counter, "3.0.0", 0)
	out, _, err := runIn(context.Background(), quickTimeout, dirs, tool, "--version")
	if err != nil || out != "3.0.0" {
		t.Fatalf("recovered probe = %q, %v", out, err)
	}
	out, _, err = runIn(context.Background(), quickTimeout, dirs, tool, "--version")
	if err != nil || out != "3.0.0" {
		t.Fatalf("cached recovery = %q, %v", out, err)
	}
	if got := commandRuns(t, counter); got != 3 {
		t.Fatalf("success after a failure exec'd %d times; want 3", got)
	}
}

func TestVersionProbeReexecsWhenPATHChanges(t *testing.T) {
	dir := t.TempDir()
	other := t.TempDir()
	counter := filepath.Join(dir, "runs")
	tool := filepath.Join(dir, "tool")
	writeCountingCommand(t, tool, counter, "4.0.0", 0)

	out, _, err := runIn(context.Background(), quickTimeout, []string{dir}, tool, "--version")
	if err != nil || out != "4.0.0" {
		t.Fatalf("first = %q, %v", out, err)
	}
	out, _, err = runIn(context.Background(), quickTimeout, []string{dir, other}, tool, "--version")
	if err != nil || out != "4.0.0" {
		t.Fatalf("other PATH = %q, %v", out, err)
	}
	if got := commandRuns(t, counter); got != 2 {
		t.Fatalf("PATH change did not re-exec; runs=%d", got)
	}
	if _, _, err := runIn(context.Background(), quickTimeout, []string{dir, other}, tool, "--version"); err != nil {
		t.Fatal(err)
	}
	if got := commandRuns(t, counter); got != 2 {
		t.Fatalf("same PATH exec'd again; runs=%d", got)
	}
}

func TestVersionProbeExpiresAfter24h(t *testing.T) {
	dir := t.TempDir()
	counter := filepath.Join(dir, "runs")
	tool := filepath.Join(dir, "tool")
	writeCountingCommand(t, tool, counter, "5.0.0", 0)
	dirs := []string{dir}
	if _, _, err := runIn(context.Background(), quickTimeout, dirs, tool, "--version"); err != nil {
		t.Fatal(err)
	}
	ageVersionCache(t, tool, dirs, 23*time.Hour)
	if _, _, err := runIn(context.Background(), quickTimeout, dirs, tool, "--version"); err != nil {
		t.Fatal(err)
	}
	if got := commandRuns(t, counter); got != 1 {
		t.Fatalf("23h old success was re-exec'd; runs=%d", got)
	}
	ageVersionCache(t, tool, dirs, versionCacheMaxAge+time.Second)
	if _, _, err := runIn(context.Background(), quickTimeout, dirs, tool, "--version"); err != nil {
		t.Fatal(err)
	}
	if got := commandRuns(t, counter); got != 2 {
		t.Fatalf("success older than 24h was not re-exec'd; runs=%d", got)
	}
}

func ageVersionCache(t *testing.T, path string, dirs []string, age time.Duration) {
	t.Helper()
	key, ok := versionProbeKey(path, dirs, []string{"--version"})
	if !ok {
		t.Fatal("version probe key")
	}
	versionProbeCache.Lock()
	defer versionProbeCache.Unlock()
	entry, ok := versionProbeCache.entries[key]
	if !ok {
		t.Fatal("expected a cached success to age")
	}
	entry.stored = time.Now().Add(-age)
	versionProbeCache.entries[key] = entry
}

func TestVersionProbeReexecsWhenShebangInterpreterChanges(t *testing.T) {
	if _, err := os.Stat("/usr/bin/env"); err != nil {
		t.Skip("/usr/bin/env is not available")
	}
	dir := t.TempDir()
	counter := filepath.Join(dir, "runs")
	node := filepath.Join(dir, "node")
	wrapper := filepath.Join(dir, "wrapper")
	writeCountingCommand(t, node, counter, "v1.0.0", 0)
	if err := os.WriteFile(wrapper, []byte("#!/usr/bin/env node\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	dirs := []string{dir}

	out, _, err := runIn(context.Background(), quickTimeout, dirs, wrapper, "--version")
	if err != nil || out != "v1.0.0" {
		t.Fatalf("wrapper = %q, %v", out, err)
	}
	out, _, err = runIn(context.Background(), quickTimeout, dirs, wrapper, "--version")
	if err != nil || out != "v1.0.0" {
		t.Fatalf("cached wrapper = %q, %v", out, err)
	}
	if got := commandRuns(t, counter); got != 1 {
		t.Fatalf("wrapper re-exec'd; runs=%d", got)
	}

	if err := os.Remove(node); err != nil {
		t.Fatal(err)
	}
	// Different length so size changes even if the filesystem reuses the inode
	// and only stores mtime at 1s resolution.
	writeCountingCommand(t, node, counter, "v2.0.0-new", 0)
	later := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(node, later, later); err != nil {
		t.Fatal(err)
	}
	out, _, err = runIn(context.Background(), quickTimeout, dirs, wrapper, "--version")
	if err != nil || out != "v2.0.0-new" {
		t.Fatalf("replaced interpreter = %q, %v; the cache key must include the env interpreter", out, err)
	}
	if got := commandRuns(t, counter); got != 2 {
		t.Fatalf("replaced interpreter runs=%d", got)
	}
}

func TestVersionProbeDoesNotCacheOtherCommands(t *testing.T) {
	dir := t.TempDir()
	counter := filepath.Join(dir, "runs")
	tool := filepath.Join(dir, "tool")
	writeCountingCommand(t, tool, counter, "6.0.0", 0)
	for i := 0; i < 2; i++ {
		out, _, err := runIn(context.Background(), quickTimeout, []string{dir}, tool, "run")
		if err != nil || out != "6.0.0" {
			t.Fatalf("call %d = %q, %v", i, out, err)
		}
	}
	if got := commandRuns(t, counter); got != 2 {
		t.Fatalf("non-version command was cached; runs=%d", got)
	}
}

func TestOpenClawNodeVersionUsesTheVersionCache(t *testing.T) {
	dir := t.TempDir()
	counter := filepath.Join(dir, "runs")
	node := filepath.Join(dir, "node")
	writeCountingCommand(t, node, counter, "v24.1.0", 0)
	inst := &model.OpenClawInstall{NodePath: node}
	deps := installDeps{run: run}
	discoverNode(context.Background(), inst, deps)
	discoverNode(context.Background(), inst, deps)
	if inst.NodeVersion != "v24.1.0" || inst.NodeVersionReason != "" {
		t.Fatalf("node probe = version %q reason %q", inst.NodeVersion, inst.NodeVersionReason)
	}
	if got := commandRuns(t, counter); got != 1 {
		t.Fatalf("openclaw node --version exec'd %d times", got)
	}

	bad := &model.OpenClawInstall{NodePath: node}
	writeCountingCommand(t, node, counter, "", 1)
	discoverNode(context.Background(), bad, deps)
	discoverNode(context.Background(), bad, deps)
	if bad.NodeVersion != "" || bad.NodeVersionReason == "" {
		t.Fatalf("failed node probe = version %q reason %q", bad.NodeVersion, bad.NodeVersionReason)
	}
	if got := commandRuns(t, counter); got != 3 {
		t.Fatalf("failed node --version was not retried; runs=%d", got)
	}
}

func TestVersionProbeCacheIsSafeForConcurrentUse(t *testing.T) {
	dir := t.TempDir()
	counter := filepath.Join(dir, "runs")
	tool := filepath.Join(dir, "tool")
	writeCountingCommand(t, tool, counter, "7.7.7", 0)
	dirs := []string{dir}

	const n = 32
	start := make(chan struct{})
	var wg sync.WaitGroup
	outs := make([]string, n)
	errs := make([]error, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			<-start
			outs[i], _, errs[i] = runIn(context.Background(), quickTimeout, dirs, tool, "--version")
		}(i)
	}
	close(start)
	wg.Wait()
	for i := 0; i < n; i++ {
		if errs[i] != nil || outs[i] != "7.7.7" {
			t.Fatalf("goroutine %d = %q, %v", i, outs[i], errs[i])
		}
	}
	got := commandRuns(t, counter)
	if got < 1 || got > n {
		t.Fatalf("concurrent cold probes ran %d times", got)
	}

	before := got
	wg.Add(n)
	start = make(chan struct{})
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			<-start
			outs[i], _, errs[i] = runIn(context.Background(), quickTimeout, dirs, tool, "--version")
		}(i)
	}
	close(start)
	wg.Wait()
	for i := 0; i < n; i++ {
		if errs[i] != nil || outs[i] != "7.7.7" {
			t.Fatalf("warm goroutine %d = %q, %v", i, outs[i], errs[i])
		}
	}
	if commandRuns(t, counter) != before {
		t.Fatalf("warm concurrent probes re-exec'd; runs %d -> %d", before, commandRuns(t, counter))
	}
}
