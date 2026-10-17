package probe

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOOMJournalAttribution(t *testing.T) {
	raw := `{"MESSAGE":"Killed /system.slice/example.service due to memory pressure for /system.slice/example.service","SYSLOG_IDENTIFIER":"systemd-oomd","_SYSTEMD_UNIT":"systemd-oomd.service","__REALTIME_TIMESTAMP":"1792234800000000"}
{"MESSAGE":"oom-kill: oom_memcg=/system.slice/worker.service, Killed process 123 (worker) total-vm:42kB token=private","SYSLOG_IDENTIFIER":"kernel","_TRANSPORT":"kernel","__REALTIME_TIMESTAMP":"1792234800000001"}
{"MESSAGE":"Killed process 9 (worker) token=private","SYSLOG_IDENTIFIER":"application","__REALTIME_TIMESTAMP":"1792234800000002"}
invalid
`
	events := parseOOMJournal(raw)
	if len(events) != 2 || events[0].Source != "oomd" || events[0].Unit != "example.service" || events[0].Process != "" || events[1].Process != "worker" || events[1].Unit != "worker.service" {
		t.Fatalf("wrong attribution: %+v", events)
	}
	if got := parseOOMJournal(strings.Repeat(raw, 100)); len(got) != 64 {
		t.Fatalf("bound: %d", len(got))
	}
}
func TestOOMOutputBound(t *testing.T) {
	var b cappedOOMOutput
	payload := []byte(strings.Repeat("x", 300<<10))
	n, err := b.Write(payload)
	if err != nil || n != len(payload) || len(b.data) != 256<<10 {
		t.Fatal("unbounded output")
	}
}

func TestOOMCounterBaselineDeltaResetAndReplacement(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "example.service")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "memory.events.local")
	write := func(raw string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UTC()
	write("oom 5\noom_kill 3\n")
	events, baseline := collectOOMCounters(context.Background(), root, now, nil)
	if len(events) != 0 {
		t.Fatal("baseline invented kills")
	}
	write("oom 8\noom_kill 5\n")
	events, next := collectOOMCounters(context.Background(), root, now, baseline)
	if len(events) != 1 || events[0].Count != 2 || events[0].Unit != "example.service" || events[0].Process != "" {
		t.Fatalf("delta %+v", events)
	}
	events, _ = collectOOMCounters(context.Background(), root, now, next)
	if len(events) != 0 {
		t.Fatal("duplicate delta")
	}
	write("oom_kill 1\n")
	events, _ = collectOOMCounters(context.Background(), root, now, next)
	if len(events) != 0 {
		t.Fatal("reset invented kills")
	}
	replacement := filepath.Join(dir, "replacement")
	if err := os.WriteFile(replacement, []byte("oom_kill 99\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	events, _ = collectOOMCounters(context.Background(), root, now, next)
	if len(events) != 0 {
		t.Fatal("recreated cgroup invented delta")
	}
	for _, raw := range []string{"oom 9", "oom_kill -1", "oom_kill invalid"} {
		if _, ok := parseOOMCounter(raw); ok {
			t.Fatal("accepted malformed counter")
		}
	}
}
func TestOOMSeparateKernelAttributionAndPressure(t *testing.T) {
	rawBytes, err := os.ReadFile("testdata/oom-journal.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	raw := string(rawBytes)
	events := parseOOMJournal(raw)
	if len(events) != 3 || events[0].Unit != "example.service" || events[0].Process != "worker" || events[1].PressureAvg10 == nil || *events[1].PressureAvg10 != 81.25 || events[2].Unit != "worker.service" {
		t.Fatalf("events %+v", events)
	}
}

func TestOOMDmesgAttribution(t *testing.T) {
	raw := `2026-10-17T11:00:00,000001-0400 kern  :warn  : oom-kill:constraint=CONSTRAINT_MEMCG,oom_memcg=/system.slice/example.service,task_memcg=/system.slice/example.service,task=worker,pid=123
2026-10-17T11:00:00,010001-0400 kern  :warn  : Memory cgroup out of memory: Killed process 123 (worker) total-vm:42kB token=private
not-a-timestamp kern :warn : Killed process 999 (ignored)
`
	events := parseOOMDmesg(raw)
	if len(events) != 1 || events[0].Source != "kernel" || events[0].Unit != "example.service" || events[0].Process != "worker" || !events[0].Time.Equal(time.Date(2026, 10, 17, 15, 0, 0, 10001000, time.UTC)) {
		t.Fatalf("events %+v", events)
	}
}
