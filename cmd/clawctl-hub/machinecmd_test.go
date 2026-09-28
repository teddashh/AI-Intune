package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/model"
	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/store"
)

func TestMachineChannelCLIReportsNonterminalJobWithoutMovingMachine(t *testing.T) {
	f := newJobsFixture(t, "cnode-cli-busy")
	now := jobsTestNow
	if err := f.store.RecordObservation(f.machine.id, model.ObservationBatch{
		MeasuredAt: now, OpenClaw: model.OpenClaw{Present: true},
	}, now); err != nil {
		t.Fatal(err)
	}
	if err := f.store.SetMachineChannel(f.machine.id, "canary"); err != nil {
		t.Fatal(err)
	}
	_, jobs, err := f.store.CreateDeployment(store.NewDeployment{
		Channel: "canary", ResourceKind: "openclaw", ResourceID: "openclaw",
		Spec:      `{"kind":"openclaw","version":"2026.9.2"}`,
		BatchSize: 1, CreatedBy: "CLI test",
		Targets: []store.NewDeploymentTarget{{MachineID: f.machine.id, BatchNo: 1}},
	})
	if err != nil || len(jobs) != 1 {
		t.Fatalf("create active job: jobs=%+v err=%v", jobs, err)
	}

	var out bytes.Buffer
	err = runMachineChannel(f.store, machineChannelInputs{
		Machine: "cnode-cli-busy", Set: "stable", ConfirmName: "cnode-cli-busy",
	}, &out)
	if !errors.Is(err, store.ErrMachineActiveJob) || !strings.Contains(err.Error(), "設定 channel 失敗") ||
		!strings.Contains(err.Error(), "未終態 job") {
		t.Fatalf("CLI active-job error=%v", err)
	}
	if out.Len() != 0 {
		t.Fatalf("rejected CLI action printed success: %q", out.String())
	}
	m, err := f.store.GetMachine(f.machine.id)
	if err != nil || m.Channel != "canary" {
		t.Fatalf("rejected CLI action changed channel: machine=%+v err=%v", m, err)
	}
	entries, err := f.store.Audit(f.machine.id, 10)
	if err != nil || len(entries) != 1 || entries[0].OK || entries[0].SourceAddr != "local-cli" ||
		entries[0].SourceKind != operator.SourceKindDirectDBCLI ||
		!strings.Contains(entries[0].Detail, "未終態 job") {
		t.Fatalf("rejected CLI audit=%+v err=%v", entries, err)
	}
}

func TestMachineChannelCLISuccessUsesOperatorServiceAndAudit(t *testing.T) {
	f := newJobsFixture(t, "cnode-cli-ok")
	if err := f.store.RecordObservation(f.machine.id, model.ObservationBatch{
		SchemaVersion: model.SchemaVersion, MeasuredAt: jobsTestNow,
		OpenClaw: model.OpenClaw{Present: true},
	}, jobsTestNow); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runMachineChannel(f.store, machineChannelInputs{
		Machine: "cnode-cli-ok", Set: "canary", ConfirmName: "cnode-cli-ok",
	}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "none → canary") {
		t.Fatalf("CLI output=%q", out.String())
	}
	m, err := f.store.GetMachine(f.machine.id)
	if err != nil || m.Channel != "canary" || m.ChannelRevision != 1 {
		t.Fatalf("CLI machine=%+v err=%v", m, err)
	}
	entries, err := f.store.Audit(f.machine.id, 10)
	if err != nil || len(entries) != 1 || !entries[0].OK || entries[0].SourceAddr != "local-cli" ||
		entries[0].SourceKind != operator.SourceKindDirectDBCLI {
		t.Fatalf("CLI audit=%+v err=%v", entries, err)
	}
}

func TestMachineChannelDirectCLIShowsSuccessAndRejectionReplays(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		f := newJobsFixture(t, "cnode-cli-success-replay")
		if err := f.store.RecordObservation(f.machine.id, model.ObservationBatch{
			SchemaVersion: model.SchemaVersion, MeasuredAt: jobsTestNow,
			OpenClaw: model.OpenClaw{Present: true},
		}, jobsTestNow); err != nil {
			t.Fatal(err)
		}
		revision := int64(0)
		inputs := machineChannelInputs{
			Machine: "cnode-cli-success-replay", Set: "canary", ConfirmName: "cnode-cli-success-replay",
			IdempotencyKey: "direct-success-replay", ExpectedRevision: &revision,
		}
		for attempt := 1; attempt <= 2; attempt++ {
			var out bytes.Buffer
			if err := runMachineChannel(f.store, inputs, &out); err != nil {
				t.Fatalf("attempt %d: %v", attempt, err)
			}
			if attempt == 2 && !strings.Contains(out.String(), "idempotency replay") {
				t.Fatalf("successful replay not identified: %q", out.String())
			}
		}
		m, err := f.store.GetMachine(f.machine.id)
		if err != nil || m.ChannelRevision != 1 {
			t.Fatalf("successful replay changed revision twice: machine=%+v err=%v", m, err)
		}
	})

	t.Run("rejection", func(t *testing.T) {
		f := newJobsFixture(t, "cnode-cli-rejection-replay")
		if err := f.store.RecordObservation(f.machine.id, model.ObservationBatch{
			SchemaVersion: model.SchemaVersion, MeasuredAt: jobsTestNow,
			OpenClaw: model.OpenClaw{Present: true},
		}, jobsTestNow); err != nil {
			t.Fatal(err)
		}
		revision := int64(0)
		inputs := machineChannelInputs{
			Machine: "cnode-cli-rejection-replay", Set: "canary", ConfirmName: "wrong-name",
			IdempotencyKey: "direct-rejection-replay", ExpectedRevision: &revision,
		}
		for attempt := 1; attempt <= 2; attempt++ {
			var out bytes.Buffer
			err := runMachineChannel(f.store, inputs, &out)
			var requestErr *store.OperatorRequestError
			if !errors.As(err, &requestErr) || requestErr.Code != store.OperatorCodeConfirmationMismatch {
				t.Fatalf("attempt %d error=%T %v request=%+v", attempt, err, err, requestErr)
			}
			if attempt == 1 && strings.Contains(err.Error(), "這是原判決") {
				t.Fatalf("first rejection mislabeled as replay: %v", err)
			}
			if attempt == 2 && (!requestErr.Replayed || !strings.Contains(err.Error(), "這是原判決")) {
				t.Fatalf("rejected replay not identified as old verdict: %v request=%+v", err, requestErr)
			}
		}
	})
}

func TestMachineChannelCLIMissingSetCannotMeanNone(t *testing.T) {
	f := newJobsFixture(t, "cnode-cli-missing-set")
	if err := f.store.RecordObservation(f.machine.id, model.ObservationBatch{
		SchemaVersion: model.SchemaVersion, MeasuredAt: jobsTestNow,
		OpenClaw: model.OpenClaw{Present: true},
	}, jobsTestNow); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err := runMachineChannel(f.store, machineChannelInputs{
		Machine: "cnode-cli-missing-set", ConfirmName: "cnode-cli-missing-set",
	}, &out)
	if !errors.Is(err, store.ErrBadChannel) || out.Len() != 0 {
		t.Fatalf("missing --set err=%v output=%q", err, out.String())
	}
	m, getErr := f.store.GetMachine(f.machine.id)
	if getErr != nil || m.Channel != "" || m.ChannelRevision != 0 {
		t.Fatalf("missing --set mutated machine=%+v err=%v", m, getErr)
	}
}

func TestMachineChannelCLIRetiredMachineUsesSharedAuditedRejection(t *testing.T) {
	f := newJobsFixture(t, "cnode-cli-retired")
	if err := f.store.RecordObservation(f.machine.id, model.ObservationBatch{
		SchemaVersion: model.SchemaVersion, MeasuredAt: jobsTestNow,
		OpenClaw: model.OpenClaw{Present: true},
	}, jobsTestNow); err != nil {
		t.Fatal(err)
	}
	// The fixture freezes the Hub clock but Store registration uses its own
	// clock; retirement must not predate the immutable registry row.
	if err := f.store.RetireMachine(f.machine.id, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err := runMachineChannel(f.store, machineChannelInputs{
		Machine: "cnode-cli-retired", Set: "canary", ConfirmName: "cnode-cli-retired",
	}, &out)
	if !errors.Is(err, store.ErrMachineRetired) || !strings.Contains(err.Error(), "退役") || out.Len() != 0 {
		t.Fatalf("retired CLI err=%v output=%q", err, out.String())
	}
	entries, auditErr := f.store.Audit(f.machine.id, 10)
	if auditErr != nil || len(entries) != 1 || entries[0].OK || entries[0].SourceAddr != "local-cli" ||
		entries[0].SourceKind != operator.SourceKindDirectDBCLI ||
		!strings.Contains(entries[0].Detail, "退役") {
		t.Fatalf("retired CLI audit=%+v err=%v", entries, auditErr)
	}
}
