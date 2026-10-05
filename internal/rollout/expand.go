package rollout

import (
	"fmt"
	"sort"

	"github.com/teddashh/AI-Intune/internal/deploy"
)

// Canary phases are the shared reading of one deployment. UI, CLI, and MCP
// switch on these strings. They do not open jobs.
const (
	CanaryNoTargets     = "no_included_targets"
	CanaryNotSingular   = "not_a_single_canary"
	CanaryPending       = "canary_pending"
	CanaryStopped       = "canary_stopped"
	CanaryReadyToExpand = "ready_to_expand"
	CanaryExpanding     = "expanding"
	CanaryExpandStopped = "expand_stopped"
	CanaryFinished      = "finished"
	CanaryPromotionGate = "stable_promotion_uses_existing_gate"
)

// RollbackSentences is the honest rollback contract for a canary rollout.
// Nothing in this package rolls a fleet back. Callers show this text as-is.
var RollbackSentences = []string{
	"There is no automatic fleet rollback to the previous artifact.",
	"Abandon stops opening further jobs. Machines whose jobs already succeeded keep that result.",
	"failed means Hub closed a reversible job as returned to the previous version. manual_intervention has no rollback evidence. rejected means the machine did not apply the job. lease_expired means the agent stopped reporting and the machine contents are unknown.",
	"Continue on a paused deployment can skip a failed batch and open the next one. rollout_expand does not send Continue while the canary job is not succeeded.",
	"Stable promotion uses the existing promotion gate: a finished canary, one full business day, and cross-failure-domain evidence. Opening the next canary batch is not that gate.",
	"succeeded is the terminal state Hub writes after stored verification evidence. An agent finish_work report only enters verifying.",
}

// CanaryMachine is one deployment target, included or excluded.
// Opened is false until Hub has a job id for it. JobState empty means no Hub verdict yet.
// IndependentVerdict is the second producer's verdict when Hub has one. It does
// not by itself move the phase.
type CanaryMachine struct {
	MachineID          string
	DisplayName        string
	BatchNo            int
	Excluded           bool
	Opened             bool
	JobState           deploy.JobState
	IndependentVerdict string
}

// CanaryInput is the deployment facts AssessCanary needs. State is the
// deployment state (running, paused, finished) or "preview" before create.
type CanaryInput struct {
	State        string
	OpenedBatch  int
	TotalBatches int
	BatchSize    int
	Targets      []CanaryMachine
}

// CanaryAssessment is the operator-visible canary position.
// ExpandAllowed is true only for a one-machine first batch whose Hub job
// state is succeeded, with a later included machine still unopened, and no
// failure terminal on an opened machine. It does not mean a write was sent.
type CanaryAssessment struct {
	Phase              string   `json:"phase"`
	DeploymentState    string   `json:"deployment_state"`
	OpenedBatch        int      `json:"opened_batch"`
	TotalBatches       int      `json:"total_batches"`
	BatchSize          int      `json:"batch_size"`
	CanaryMachineID    string   `json:"canary_machine_id,omitempty"`
	CanaryDisplayName  string   `json:"canary_display_name,omitempty"`
	CanaryBatch        int      `json:"canary_batch,omitempty"`
	CanaryJobState     string   `json:"canary_job_state,omitempty"`
	CanaryIndependent  string   `json:"canary_independent,omitempty"`
	FirstBatchMachines int      `json:"first_batch_machines"`
	SingularCanary     bool     `json:"singular_canary"`
	ExpandAllowed      bool     `json:"expand_allowed"`
	Stopped            bool     `json:"stopped"`
	PromotionGate      string   `json:"promotion_gate"`
	Reason             string   `json:"reason"`
	Rollback           []string `json:"rollback"`
}

// AssessCanary reads a frozen target list. It does not sort the caller's slice.
func AssessCanary(in CanaryInput) CanaryAssessment {
	out := CanaryAssessment{
		DeploymentState: in.State,
		OpenedBatch:     in.OpenedBatch,
		TotalBatches:    in.TotalBatches,
		BatchSize:       in.BatchSize,
		PromotionGate:   CanaryPromotionGate,
		Rollback:        append([]string(nil), RollbackSentences...),
	}
	included := make([]CanaryMachine, 0, len(in.Targets))
	for _, target := range in.Targets {
		if !target.Excluded {
			included = append(included, target)
		}
	}
	if len(included) == 0 {
		out.Phase = CanaryNoTargets
		out.Reason = "No included machine. Nothing to open."
		return out
	}
	sort.Slice(included, func(i, j int) bool {
		if included[i].BatchNo != included[j].BatchNo {
			return included[i].BatchNo < included[j].BatchNo
		}
		if included[i].DisplayName != included[j].DisplayName {
			return included[i].DisplayName < included[j].DisplayName
		}
		return included[i].MachineID < included[j].MachineID
	})
	firstBatch := included[0].BatchNo
	var first []CanaryMachine
	var later []CanaryMachine
	for _, target := range included {
		if target.BatchNo == firstBatch {
			first = append(first, target)
		} else {
			later = append(later, target)
		}
	}
	out.FirstBatchMachines = len(first)
	out.CanaryBatch = firstBatch
	out.SingularCanary = len(first) == 1
	if out.SingularCanary {
		out.CanaryMachineID = first[0].MachineID
		out.CanaryDisplayName = first[0].DisplayName
		if first[0].JobState != "" {
			out.CanaryJobState = string(first[0].JobState)
		}
		out.CanaryIndependent = first[0].IndependentVerdict
	}
	if !out.SingularCanary {
		out.Phase = CanaryNotSingular
		out.Stopped = anyFailure(first)
		out.Reason = fmt.Sprintf("First included batch has %d machines. The canary rollout accepts a first batch of exactly one machine.", len(first))
		return out
	}

	canary := first[0]
	switch {
	case jobFailed(canary.JobState):
		out.Phase = CanaryStopped
		out.Stopped = true
		out.Reason = stoppedReason(canary)
	case !jobSucceeded(canary.JobState):
		out.Phase = CanaryPending
		out.Reason = pendingReason(canary)
	case anyFailure(later):
		out.Phase = CanaryExpandStopped
		out.Stopped = true
		out.Reason = "A machine after the canary has a failure terminal. Hub pauses the deployment and does not open another batch."
	case anyUnopened(later):
		out.Phase = CanaryReadyToExpand
		out.ExpandAllowed = true
		out.Reason = readyReason(in.State)
	case anyOpenNonTerminal(later):
		out.Phase = CanaryExpanding
		out.Reason = "A later batch is open. Those jobs are waiting for a Hub terminal state."
	default:
		out.Phase = CanaryFinished
		out.Reason = "Every included machine is succeeded. No further batch is waiting."
	}
	return out
}

func jobSucceeded(state deploy.JobState) bool {
	return state == deploy.Succeeded
}

func jobFailed(state deploy.JobState) bool {
	switch state {
	case deploy.Failed, deploy.Rejected, deploy.LeaseExpired, deploy.ManualIntervention:
		return true
	default:
		return false
	}
}

func anyFailure(targets []CanaryMachine) bool {
	for _, target := range targets {
		if target.Opened && jobFailed(target.JobState) {
			return true
		}
	}
	return false
}

func anyUnopened(targets []CanaryMachine) bool {
	for _, target := range targets {
		if !target.Opened {
			return true
		}
	}
	return false
}

func anyOpenNonTerminal(targets []CanaryMachine) bool {
	for _, target := range targets {
		if target.Opened && !jobSucceeded(target.JobState) && !jobFailed(target.JobState) {
			return true
		}
	}
	return false
}

func pendingReason(canary CanaryMachine) string {
	if !canary.Opened || canary.JobState == "" {
		return "The canary job is not open yet. Hub has not written a verdict."
	}
	return fmt.Sprintf("The canary job is %s. Hub has not written succeeded.", canary.JobState)
}

func stoppedReason(canary CanaryMachine) string {
	switch canary.JobState {
	case deploy.Failed:
		return "The canary job is failed. Hub closed that reversible job as returned to the previous version. The next batch stays closed."
	case deploy.ManualIntervention:
		return "The canary job is manual_intervention. There is no rollback evidence for that machine. The next batch stays closed."
	case deploy.Rejected:
		return "The canary job is rejected. The machine did not apply the job. The next batch stays closed."
	case deploy.LeaseExpired:
		return "The canary job is lease_expired. The agent stopped reporting and the machine contents are unknown. The next batch stays closed."
	default:
		return fmt.Sprintf("The canary job is %s. The next batch stays closed.", canary.JobState)
	}
}

func readyReason(state string) string {
	switch state {
	case "running":
		return "The canary job is succeeded. A later machine is still unopened. While the deployment is running, the Hub deployment driver opens the next batch after that verdict."
	case "paused":
		return "The canary job is succeeded and the deployment is paused. A later machine is still unopened. The next write is the existing Continue preview, then rollout_expand with that preview digest."
	default:
		return "The canary job is succeeded. A later machine is still unopened."
	}
}
