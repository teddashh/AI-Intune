package probe

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/teddashh/AI-Intune/internal/model"
)

var kernelVictim = regexp.MustCompile(`Killed process ([0-9]+) \(([^)]+)\)`)
var oomdVictim = regexp.MustCompile(`^Killed /([^ ]+) due to memory (?:pressure|used)`)
var kernelPID = regexp.MustCompile(`(?:^|,)pid=([0-9]+)(?:,|$)`)
var oomdPressure = regexp.MustCompile(`being ([0-9]+(?:\.[0-9]+)?)% >`)
var kernelGroup = regexp.MustCompile(`oom_memcg=([^, ]+)`)
var kernelTaskGroup = regexp.MustCompile(`task_memcg=([^, ]+)`)
var dmesgLine = regexp.MustCompile(`^(\S+)\s+\S+\s*:\S+\s*:\s*(.*)$`)

func oomKernelGroup(message string) []string {
	if m := kernelTaskGroup.FindStringSubmatch(message); len(m) > 0 {
		return m
	}
	return kernelGroup.FindStringSubmatch(message)
}

func parseOOMJournal(raw string) []model.OOMKill {
	out := []model.OOMKill{}
	type attribution struct {
		unit string
		at   time.Time
	}
	groups := map[string]attribution{}
	scanner := bufio.NewScanner(strings.NewReader(raw))
	scanner.Buffer(make([]byte, 4096), 256<<10)
	for records := 0; records < 256 && scanner.Scan(); records++ {
		var row struct {
			Message    string `json:"MESSAGE"`
			Timestamp  string `json:"__REALTIME_TIMESTAMP"`
			Identifier string `json:"SYSLOG_IDENTIFIER"`
			Transport  string `json:"_TRANSPORT"`
			Unit       string `json:"_SYSTEMD_UNIT"`
		}
		if json.Unmarshal(scanner.Bytes(), &row) != nil {
			continue
		}
		us, err := strconv.ParseInt(row.Timestamp, 10, 64)
		if err != nil {
			continue
		}
		e := model.OOMKill{Time: time.UnixMicro(us).UTC()}
		if row.Transport == "kernel" {
			g := oomKernelGroup(row.Message)
			pid := kernelPID.FindStringSubmatch(row.Message)
			if len(g) > 0 && len(pid) > 0 {
				groups[pid[1]] = attribution{model.OOMName(filepath.Base(g[1])), e.Time}
			}
		}
		if m := kernelVictim.FindStringSubmatch(row.Message); len(m) > 0 && row.Transport == "kernel" {
			e.Source = "kernel"
			e.Process = model.OOMName(m[2])
			if g, ok := groups[m[1]]; ok && e.Time.Sub(g.at) >= 0 && e.Time.Sub(g.at) <= 5*time.Second {
				e.Unit = g.unit
				delete(groups, m[1])
			}
			if g := oomKernelGroup(row.Message); len(g) > 0 {
				e.Unit = model.OOMName(filepath.Base(g[1]))
			}
		} else if m := oomdVictim.FindStringSubmatch(row.Message); len(m) > 0 && row.Identifier == "systemd-oomd" && row.Unit == "systemd-oomd.service" {
			e.Source = "oomd"
			e.Unit = model.OOMName(filepath.Base(m[1]))
			if p := oomdPressure.FindStringSubmatch(row.Message); len(p) > 0 {
				v, err := strconv.ParseFloat(p[1], 64)
				if err == nil && v >= 0 && v <= 100 {
					e.PressureAvg10 = &v
				}
			}
		} else {
			continue
		}
		out = append(out, e)
	}
	if len(out) > 64 {
		out = out[len(out)-64:]
	}
	return out
}

func parseOOMDmesg(raw string) []model.OOMKill {
	out := []model.OOMKill{}
	type attribution struct {
		unit string
		at   time.Time
	}
	groups := map[string]attribution{}
	scanner := bufio.NewScanner(strings.NewReader(raw))
	scanner.Buffer(make([]byte, 4096), 256<<10)
	for records := 0; records < 256 && scanner.Scan(); records++ {
		parts := dmesgLine.FindStringSubmatch(scanner.Text())
		if len(parts) == 0 {
			continue
		}
		at, err := time.Parse("2006-01-02T15:04:05,999999-0700", parts[1])
		if err != nil {
			continue
		}
		message := parts[2]
		if g, pid := oomKernelGroup(message), kernelPID.FindStringSubmatch(message); len(g) > 0 && len(pid) > 0 {
			groups[pid[1]] = attribution{model.OOMName(filepath.Base(g[1])), at.UTC()}
		}
		victim := kernelVictim.FindStringSubmatch(message)
		if len(victim) == 0 {
			continue
		}
		e := model.OOMKill{Time: at.UTC(), Source: "kernel", Process: model.OOMName(victim[2])}
		if g, ok := groups[victim[1]]; ok && e.Time.Sub(g.at) >= 0 && e.Time.Sub(g.at) <= 5*time.Second {
			e.Unit = g.unit
			delete(groups, victim[1])
		}
		if g := oomKernelGroup(message); len(g) > 0 {
			e.Unit = model.OOMName(filepath.Base(g[1]))
		}
		out = append(out, e)
	}
	if len(out) > 64 {
		out = out[len(out)-64:]
	}
	return out
}

type oomCounter struct {
	count uint64
	file  os.FileInfo
}

var oomCounters = struct {
	sync.Mutex
	values map[string]oomCounter
}{values: map[string]oomCounter{}}

func collectOOM(ctx context.Context) []model.OOMKill {
	if runtime.GOOS != "linux" {
		return nil
	}
	events := parseOOMJournal(oomJournal(ctx))
	hasKernel := false
	for _, event := range events {
		if event.Source == "kernel" {
			hasKernel = true
			break
		}
	}
	// The journal is preferred because it supplies trusted real-time stamps and
	// structured provenance. dmesg is a bounded fallback when no kernel victim
	// was readable there; this avoids uploading the same kernel event twice.
	if !hasKernel {
		events = append(events, parseOOMDmesg(oomDmesg(ctx))...)
		if len(events) > 64 {
			events = events[len(events)-64:]
		}
	}
	oomCounters.Lock()
	defer oomCounters.Unlock()
	deltas, next := collectOOMCounters(ctx, "/sys/fs/cgroup", time.Now().UTC(), oomCounters.values)
	oomCounters.values = next
	for _, e := range deltas {
		if len(events) >= 64 {
			break
		}
		events = append(events, e)
	}
	return events
}

// Counter deltas establish neither individual victim identities nor exact times.
// Read directories in bounded chunks, rather than loading an entire cgroup tree.
func collectOOMCounters(ctx context.Context, root string, now time.Time, previous map[string]oomCounter) ([]model.OOMKill, map[string]oomCounter) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	events := []model.OOMKill{}
	next := map[string]oomCounter{}
	queue := []string{root}
	visited := 0
	for len(queue) > 0 && visited < 2048 && ctx.Err() == nil {
		dir := queue[0]
		queue = queue[1:]
		f, err := os.Open(dir)
		if err != nil {
			continue
		}
		for visited < 2048 && ctx.Err() == nil {
			entries, err := f.ReadDir(64)
			for _, d := range entries {
				visited++
				if visited > 2048 {
					break
				}
				path := filepath.Join(dir, d.Name())
				if d.IsDir() {
					if len(queue) < 2048 {
						queue = append(queue, path)
					}
					continue
				}
				if d.Name() != "memory.events.local" || d.Type()&os.ModeSymlink != 0 {
					continue
				}
				counter, err := os.Open(path)
				if err != nil {
					continue
				}
				info, statErr := counter.Stat()
				b, readErr := io.ReadAll(io.LimitReader(counter, 4097))
				counter.Close()
				if statErr != nil || readErr != nil || len(b) > 4096 {
					continue
				}
				n, ok := parseOOMCounter(string(b))
				if !ok {
					continue
				}
				next[path] = oomCounter{n, info}
				if old, ok := previous[path]; ok && os.SameFile(old.file, info) && n > old.count && len(events) < 64 {
					events = append(events, model.OOMKill{Time: now, Source: "memcg", Unit: model.OOMName(filepath.Base(dir)), Count: n - old.count})
				}
			}
			if err != nil {
				break
			}
		}
		f.Close()
	}
	return events, next
}
func parseOOMCounter(raw string) (uint64, bool) {
	fields := strings.Fields(raw)
	for i := 0; i+1 < len(fields); i += 2 {
		if fields[i] == "oom_kill" {
			n, err := strconv.ParseUint(fields[i+1], 10, 64)
			return n, err == nil
		}
	}
	return 0, false
}

// cappedOOMOutput drains excess bytes without retaining them.
type cappedOOMOutput struct{ data []byte }

func (b *cappedOOMOutput) Write(p []byte) (int, error) {
	n := len(p)
	remaining := (256 << 10) - len(b.data)
	if remaining > len(p) {
		remaining = len(p)
	}
	if remaining > 0 {
		b.data = append(b.data, p[:remaining]...)
	}
	return n, nil
}
func oomJournal(ctx context.Context) string {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "journalctl", "--since=-1 hour", "--no-pager", "-o", "json", "-n", "256", "--grep=Killed process|^Killed /|oom-kill:")
	cmd.WaitDelay = 100 * time.Millisecond
	var out cappedOOMOutput
	cmd.Stdout = &out
	cmd.Stderr = io.Discard
	_ = cmd.Run()
	return string(out.data)
}

func oomDmesg(ctx context.Context) string {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "dmesg", "--time-format=iso", "--nopager", "--level=emerg,alert,crit,err,warn")
	cmd.WaitDelay = 100 * time.Millisecond
	var out cappedOOMOutput
	cmd.Stdout = &out
	cmd.Stderr = io.Discard
	_ = cmd.Run()
	return string(out.data)
}
