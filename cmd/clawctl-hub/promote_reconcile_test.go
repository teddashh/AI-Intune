package main

import (
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/model"
)

func TestReconcileRecordsEligibleFailureFactsForEveryNonRetiredMachine(t *testing.T) {
	f := newJobsFixture(t, "workload")
	now := time.Now().UTC()
	if err := f.store.RecordCheckin(f.machine.id, model.Checkin{SentAt: now, AgentStartedAt: now}, now); err != nil {
		t.Fatal(err)
	}
	if err := f.store.RecordObservation(f.machine.id, model.ObservationBatch{
		MeasuredAt: now, OpenClaw: model.OpenClaw{Present: true},
	}, now); err != nil {
		t.Fatal(err)
	}
	if err := f.store.SetMachineChannel(f.machine.id, "canary"); err != nil {
		t.Fatal(err)
	}

	offline := enrollViaHTTP(t, f.mux, f.store, "offline")
	old := now.Add(-10 * time.Minute)
	if err := f.store.RecordCheckin(offline.id, model.Checkin{SentAt: old, AgentStartedAt: old}, old); err != nil {
		t.Fatal(err)
	}
	// 只要一列 raw observation 讓它能分 channel；零 measured_at 不能在 ingest
	// 階段冒充 current failure，這樣本測試才能單獨驗 Reconcile 的失聯理由。
	if err := f.store.RecordObservation(offline.id, model.ObservationBatch{
		OpenClaw: model.OpenClaw{Present: true},
	}, old); err != nil {
		t.Fatal(err)
	}
	if err := f.store.SetMachineChannel(offline.id, "canary"); err != nil {
		t.Fatal(err)
	}

	stable := enrollViaHTTP(t, f.mux, f.store, "stable-bad")
	if err := f.store.RecordCheckin(stable.id, model.Checkin{SentAt: now, AgentStartedAt: now}, now); err != nil {
		t.Fatal(err)
	}
	if err := f.store.RecordObservation(stable.id, model.ObservationBatch{MeasuredAt: now, OpenClaw: model.OpenClaw{Present: true}}, now); err != nil {
		t.Fatal(err)
	}
	if err := f.store.SetMachineChannel(stable.id, "stable"); err != nil {
		t.Fatal(err)
	}

	(&hub{store: f.store}).reconcile()
	rows, err := f.store.CanarySilentFailuresSince([]string{f.machine.id, offline.id, stable.id}, now.Add(-time.Hour), time.Now().UTC().Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("記錄=%+v，預期兩台 canary 與一台 stable 都留下可供快照查詢的事實", rows)
	}
	var reasons string
	for _, row := range rows {
		reasons += row.Reason + "\n"
	}
	if !strings.Contains(reasons, "OpenClaw") || !strings.Contains(reasons, "失聯") {
		t.Fatalf("沒有沿用 Overview finding 與失聯理由：%s", reasons)
	}
	var stableRows int
	if err := f.store.DB().QueryRow(`SELECT COUNT(*) FROM canary_silent_failures WHERE machine_id=?`, stable.id).Scan(&stableRows); err != nil || stableRows != 1 {
		t.Fatalf("stable 的 failure fact 沒留下來：rows=%d err=%v", stableRows, err)
	}
}

func TestReconcileSilentFailureWriteErrorRollsBackStateHistory(t *testing.T) {
	f := newJobsFixture(t, "samplehub1")
	now := time.Now().UTC()
	if err := f.store.RecordCheckin(f.machine.id, model.Checkin{SentAt: now, AgentStartedAt: now}, now); err != nil {
		t.Fatal(err)
	}
	if err := f.store.RecordObservation(f.machine.id, model.ObservationBatch{MeasuredAt: now, OpenClaw: model.OpenClaw{Present: true}}, now); err != nil {
		t.Fatal(err)
	}
	if err := f.store.SetMachineChannel(f.machine.id, "canary"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.DB().Exec(`DROP TABLE canary_silent_failures`); err != nil {
		t.Fatal(err)
	}
	(&hub{store: f.store}).reconcile()
	var histories int
	if err := f.store.DB().QueryRow(`SELECT COUNT(*) FROM machine_state_history WHERE machine_id=?`, f.machine.id).Scan(&histories); err != nil || histories != 0 {
		t.Fatalf("promote 證據寫失敗時 state history 不可單獨 commit：history=%d err=%v", histories, err)
	}
}
