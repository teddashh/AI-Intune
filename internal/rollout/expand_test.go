package rollout

import (
	"strings"
	"testing"

	"github.com/teddashh/AI-Intune/internal/deploy"
)

func canaryPair() []CanaryMachine {
	return []CanaryMachine{
		{MachineID: "m-b", DisplayName: "beta", BatchNo: 2},
		{MachineID: "m-a", DisplayName: "alpha", BatchNo: 1, Opened: true, JobState: deploy.Succeeded, IndependentVerdict: "absent"},
		{MachineID: "m-x", DisplayName: "skip", BatchNo: 1, Excluded: true},
	}
}

func TestAssessCanaryPhases(t *testing.T) {
	base := CanaryInput{State: "running", OpenedBatch: 1, TotalBatches: 2, BatchSize: 1, Targets: canaryPair()}

	pending := base
	pending.Targets = canaryPair()
	pending.Targets[1].JobState = deploy.Verifying
	got := AssessCanary(pending)
	if got.Phase != CanaryPending || got.ExpandAllowed || got.Stopped || got.CanaryMachineID != "m-a" {
		t.Fatalf("pending: %+v", got)
	}
	if !strings.Contains(got.Reason, "verifying") {
		t.Fatalf("pending reason: %s", got.Reason)
	}

	stopped := base
	stopped.Targets = canaryPair()
	stopped.Targets[1].JobState = deploy.Failed
	stopped.State = "paused"
	got = AssessCanary(stopped)
	if got.Phase != CanaryStopped || !got.Stopped || got.ExpandAllowed {
		t.Fatalf("stopped: %+v", got)
	}

	for _, state := range []deploy.JobState{deploy.Rejected, deploy.LeaseExpired, deploy.ManualIntervention} {
		in := base
		in.Targets = canaryPair()
		in.Targets[1].JobState = state
		if got := AssessCanary(in); got.Phase != CanaryStopped || !got.Stopped {
			t.Fatalf("%s: %+v", state, got)
		}
	}

	ready := AssessCanary(base)
	if ready.Phase != CanaryReadyToExpand || !ready.ExpandAllowed || !ready.SingularCanary || ready.CanaryIndependent != "absent" {
		t.Fatalf("ready: %+v", ready)
	}
	if ready.PromotionGate != CanaryPromotionGate {
		t.Fatalf("promotion gate: %s", ready.PromotionGate)
	}

	// A second producer's failure does not replace the Hub job verdict, and it
	// does not authorize a different phase either. The job state decides.
	indep := base
	indep.Targets = canaryPair()
	indep.Targets[1].IndependentVerdict = "failed"
	if got := AssessCanary(indep); got.Phase != CanaryReadyToExpand || got.CanaryIndependent != "failed" {
		t.Fatalf("independent verdict must not retcon the job phase: %+v", got)
	}

	expanding := base
	expanding.OpenedBatch = 2
	expanding.Targets = canaryPair()
	expanding.Targets[0].Opened = true
	expanding.Targets[0].JobState = deploy.Running
	got = AssessCanary(expanding)
	if got.Phase != CanaryExpanding || got.ExpandAllowed {
		t.Fatalf("expanding: %+v", got)
	}

	expandStopped := base
	expandStopped.Targets = canaryPair()
	expandStopped.Targets[0].Opened = true
	expandStopped.Targets[0].JobState = deploy.Failed
	got = AssessCanary(expandStopped)
	if got.Phase != CanaryExpandStopped || !got.Stopped || got.ExpandAllowed {
		t.Fatalf("expand stopped: %+v", got)
	}

	finished := base
	finished.State = "finished"
	finished.OpenedBatch = 2
	finished.Targets = canaryPair()
	finished.Targets[0].Opened = true
	finished.Targets[0].JobState = deploy.Succeeded
	got = AssessCanary(finished)
	if got.Phase != CanaryFinished || got.ExpandAllowed || got.Stopped {
		t.Fatalf("finished: %+v", got)
	}
}

func TestAssessCanaryRefusesAMultiMachineFirstBatch(t *testing.T) {
	got := AssessCanary(CanaryInput{
		State: "running", BatchSize: 5, OpenedBatch: 1, TotalBatches: 1,
		Targets: []CanaryMachine{
			{MachineID: "a", DisplayName: "a", BatchNo: 1, Opened: true, JobState: deploy.Succeeded},
			{MachineID: "b", DisplayName: "b", BatchNo: 1, Opened: true, JobState: deploy.Succeeded},
		},
	})
	if got.Phase != CanaryNotSingular || got.SingularCanary || got.ExpandAllowed || got.FirstBatchMachines != 2 {
		t.Fatalf("multi: %+v", got)
	}
	failed := AssessCanary(CanaryInput{Targets: []CanaryMachine{
		{MachineID: "a", BatchNo: 1, Opened: true, JobState: deploy.Failed},
		{MachineID: "b", BatchNo: 1},
	}})
	if failed.Phase != CanaryNotSingular || !failed.Stopped {
		t.Fatalf("multi failure: %+v", failed)
	}
}

func TestAssessCanaryEmptyAndUnopened(t *testing.T) {
	empty := AssessCanary(CanaryInput{Targets: []CanaryMachine{{MachineID: "x", Excluded: true, BatchNo: 1}}})
	if empty.Phase != CanaryNoTargets || empty.ExpandAllowed {
		t.Fatalf("empty: %+v", empty)
	}
	unopened := AssessCanary(CanaryInput{State: "preview", BatchSize: 1, Targets: []CanaryMachine{
		{MachineID: "a", DisplayName: "alpha", BatchNo: 1},
		{MachineID: "b", DisplayName: "beta", BatchNo: 2},
	}})
	if unopened.Phase != CanaryPending || unopened.ExpandAllowed || !unopened.SingularCanary {
		t.Fatalf("unopened: %+v", unopened)
	}
}

func TestAssessCanaryRollbackTextIsStable(t *testing.T) {
	got := AssessCanary(CanaryInput{})
	if len(got.Rollback) != len(RollbackSentences) {
		t.Fatalf("rollback len %d", len(got.Rollback))
	}
	for i, sentence := range RollbackSentences {
		if got.Rollback[i] != sentence {
			t.Fatalf("rollback[%d]=%q", i, got.Rollback[i])
		}
	}
	joined := strings.Join(got.Rollback, "\n")
	for _, want := range []string{"no automatic fleet rollback", "manual_intervention", "lease_expired", "rollout_expand", "promotion gate", "finish_work"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("rollback missing %q: %s", want, joined)
		}
	}
}

func TestAssessCanaryDoesNotMutateInputOrder(t *testing.T) {
	targets := []CanaryMachine{
		{MachineID: "b", BatchNo: 2},
		{MachineID: "a", BatchNo: 1, Opened: true, JobState: deploy.Succeeded},
	}
	_ = AssessCanary(CanaryInput{State: "running", Targets: targets})
	if targets[0].MachineID != "b" || targets[1].MachineID != "a" {
		t.Fatalf("input reordered: %+v", targets)
	}
}
