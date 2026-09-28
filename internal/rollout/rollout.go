package rollout

import (
	"fmt"
	"sort"

	"github.com/teddashh/AI-Intune/internal/deploy"
)

const (
	ExcludedConflict       = "conflict"
	ExcludedMissingPackage = "missing_package"
	ExcludedUnknownNode    = "unknown_node"
	// ExcludedNoncompliant: a compliance action is withholding work from this
	// machine, so opening a job for it would create one nobody can take.
	ExcludedNoncompliant = "noncompliant"
)

type MachineFacts struct {
	MachineID   string
	DisplayName string
	Reachable   bool
	NodeVersion string
	Conflict    bool
	// Noncompliant reports that a compliance action is currently withholding
	// work from this machine.
	Noncompliant bool
}

type Target struct {
	MachineID      string
	DisplayName    string
	Reachable      bool
	NodeVersion    string
	BatchNo        int
	ExcludedReason string
}

type DeploymentPlan struct {
	Targets         []Target
	Impact          int
	Conflicts       int
	MissingPackages int
	UnknownNodes    int
	Noncompliant    int
	Unreachable     int
	TotalBatches    int
}

// Plan 建出建立 deployment 時要凍結的成員快照；不讀 DB、不看真實時鐘。
func Plan(members []MachineFacts, engines string, batchSize int) DeploymentPlan {
	if batchSize <= 0 {
		batchSize = 5 // docs/SPEC.md §9 風險 2：預設 batch_size=5。
	}
	sorted := append([]MachineFacts(nil), members...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].DisplayName != sorted[j].DisplayName {
			return sorted[i].DisplayName < sorted[j].DisplayName
		}
		return sorted[i].MachineID < sorted[j].MachineID
	})
	var p DeploymentPlan
	for _, m := range sorted {
		t := Target{MachineID: m.MachineID, DisplayName: m.DisplayName, Reachable: m.Reachable, NodeVersion: m.NodeVersion}
		switch {
		case m.Noncompliant:
			// 先問這一條：一台領不到工作單的機器，講成「跟別的單衝突」或
			// 「node 版本不符」都是在講一個不是真正原因的原因。
			t.ExcludedReason = ExcludedNoncompliant
			p.Noncompliant++
		case m.Conflict:
			t.ExcludedReason = ExcludedConflict
			p.Conflicts++
		case m.NodeVersion == "":
			t.ExcludedReason = ExcludedUnknownNode
			p.UnknownNodes++
		default:
			matched, understood := NodeSatisfiesRange(m.NodeVersion, engines)
			if !understood {
				t.ExcludedReason = ExcludedUnknownNode
				p.UnknownNodes++
			} else if !matched {
				t.ExcludedReason = ExcludedMissingPackage
				p.MissingPackages++
			}
		}
		if t.ExcludedReason == "" {
			p.Impact++
			t.BatchNo = (p.Impact-1)/batchSize + 1
			if !m.Reachable {
				p.Unreachable++
			}
		}
		p.Targets = append(p.Targets, t)
	}
	if p.Impact > 0 {
		p.TotalBatches = (p.Impact-1)/batchSize + 1
	}
	return p
}

func (p DeploymentPlan) Summary() string {
	s := fmt.Sprintf("影響 %d 台，衝突 %d，缺套件 %d，unreachable %d", p.Impact, p.Conflicts, p.MissingPackages, p.Unreachable)
	if p.UnknownNodes > 0 {
		s += fmt.Sprintf("，node 版本未知 %d", p.UnknownNodes)
	}
	if p.Noncompliant > 0 {
		s += fmt.Sprintf("，合規性停發工作單 %d", p.Noncompliant)
	}
	return s
}

type Action string

const (
	Wait     Action = "wait"
	OpenNext Action = "open_next"
	Pause    Action = "pause"
	Finish   Action = "finish"
)

type JobState struct {
	MachineID string
	State     deploy.JobState
}

type Decision struct {
	Action          Action
	StuckMachineIDs []string
}

// Decide 只看目前最高已開批次。任一失敗終態優先於其他仍在執行的單。
func Decide(states []JobState, hasNextBatch bool) Decision {
	var stuck []string
	allSucceeded := len(states) > 0
	for _, job := range states {
		switch job.State {
		case deploy.Failed, deploy.ManualIntervention, deploy.LeaseExpired, deploy.Rejected:
			stuck = append(stuck, job.MachineID)
		case deploy.Succeeded:
		default:
			allSucceeded = false
		}
	}
	if len(stuck) > 0 {
		sort.Strings(stuck)
		return Decision{Action: Pause, StuckMachineIDs: stuck}
	}
	if !allSucceeded {
		return Decision{Action: Wait}
	}
	if hasNextBatch {
		return Decision{Action: OpenNext}
	}
	return Decision{Action: Finish}
}
