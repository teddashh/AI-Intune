package rollout

import (
	"fmt"
	"strings"
	"time"

	"github.com/teddashh/AI-Intune/internal/state"
)

type IndependentGateState string

const (
	IndependentGateUnassigned        IndependentGateState = "unassigned"
	IndependentGateAwaitingReport    IndependentGateState = "awaiting_report"
	IndependentGateIncompleteReport  IndependentGateState = "incomplete_report"
	IndependentGateProducerRevoked   IndependentGateState = "producer_revoked"
	IndependentGateDigestMismatch    IndependentGateState = "digest_mismatch"
	IndependentGateReleaseMismatch   IndependentGateState = "release_mismatch"
	IndependentGateReleaseUnreported IndependentGateState = "release_unreported"
	IndependentGateStale             IndependentGateState = "stale"
	IndependentGateFailed            IndependentGateState = "failed"
	IndependentGatePassed            IndependentGateState = "passed"
)

type CanaryTarget struct {
	MachineID        string
	DisplayName      string
	JobID            string
	ExcludedReason   string
	Ran              bool
	Succeeded        bool
	Judged           bool
	Retired          bool
	IdentityConflict bool
	ClockUntrusted   bool
	Unreachable      bool
	SilentNow        bool
	LastObservation  time.Time
	// WorkloadObserved：這台在帳本上有沒有 workload observation witness 列。
	// ⚠ 沒有列的時候 OpenClawPresent 會是 false，而那個 false 的意思是
	// 「這台從來沒回報過」，不是「它回報了，而且說沒裝」。
	WorkloadObserved bool
	OpenClawPresent  bool
	RunningVersion   string
	AppliedDigest    string
	// AppliedReason 是 current explicit OpenClaw attempt 無法作為 applied
	// witness 的具體原因。非空永遠 fail closed，即使 digest 字串剛好相同。
	AppliedReason   string
	WorkloadReady   bool
	WorkloadReason  string
	IndependentGate IndependentGateState
}

type IndependentGateTarget struct {
	MachineID   string
	DisplayName string
	JobID       string
	State       IndependentGateState
}

type SilentFailure struct {
	FirstSeenAt time.Time
	LastSeenAt  time.Time
	DisplayName string
	Reason      string
}

type CanaryRun struct {
	DeploymentID string
	FinishedAt   time.Time
	// WaitFrom 是完整工作天的起點。FinishedAt 永遠保留 canary lineage 的
	// factual completion，因為 silent-failure history 必須從那一刻開始查。
	// lineage 外的套用必須重跑 canary，因此目前 WaitFrom 與 FinishedAt 相同。
	WaitFrom time.Time
	Stuck    int
	Targets  []CanaryTarget
}

type PromoteFacts struct {
	Version     string
	Digest      string
	Canary      *CanaryRun
	CanaryBlock string
	Silent      []SilentFailure
}

type PromoteDecision struct {
	Allowed            bool
	Reasons            []string
	EarliestAt         time.Time
	IndependentTargets []IndependentGateTarget
	facts              PromoteFacts
	loc                *time.Location
}

const promoteTimeFormat = "2006-01-02 15:04 MST"

// PromoteGate 是 stable 開單前唯一的純判決；國定假日刻意不認，因為 Hub 沒有假日表。
func PromoteGate(f PromoteFacts, now time.Time, loc *time.Location) PromoteDecision {
	if loc == nil {
		loc = time.Local
	}
	d := PromoteDecision{facts: f, loc: loc}
	if f.Canary == nil {
		if f.CanaryBlock != "" {
			d.Reasons = append(d.Reasons, f.CanaryBlock)
		} else {
			d.Reasons = append(d.Reasons, fmt.Sprintf("canary 沒部署過 %s（digest %s）", f.Version, shortPromote(f.Digest, 12)))
		}
		d.Allowed = false
		return d
	}
	run := f.Canary
	if len(run.Targets) == 0 {
		d.Reasons = append(d.Reasons, fmt.Sprintf("canary 部署 %s 沒有任何成功證人", shortPromote(run.DeploymentID, 8)))
	}
	if run.Stuck > 0 {
		d.Reasons = append(d.Reasons, fmt.Sprintf("canary 部署 %s 有 %d 台 stuck", shortPromote(run.DeploymentID, 8), run.Stuck))
	}

	waitFrom := run.WaitFrom
	if waitFrom.IsZero() {
		waitFrom = run.FinishedAt
	}
	earliest := earliestCompleteBusinessDay(waitFrom, loc)
	if now.Before(earliest) {
		d.EarliestAt = earliest
		anchor := fmt.Sprintf("canary %s 完成", run.FinishedAt.In(loc).Format(promoteTimeFormat))
		if waitFrom.After(run.FinishedAt) {
			anchor = fmt.Sprintf("同一 artifact 最近在 %s 再次套用", waitFrom.In(loc).Format(promoteTimeFormat))
		}
		d.Reasons = append(d.Reasons, fmt.Sprintf("%s，最早 %s 才滿一個完整工作天",
			anchor, earliest.Format(promoteTimeFormat)))
	}

	// ⚠ 擋 PHASES 死法 C：「4 小時全綠，第 10 小時沉默失敗」不能被後來的綠燈洗掉。
	// 這個窗口裡只要有一筆，這次 canary 永遠鎖著；要 promote 必須重跑出新的 FinishedAt。
	limit := len(f.Silent)
	if limit > 3 {
		limit = 3
	}
	for _, failure := range f.Silent[:limit] {
		when := failure.FirstSeenAt.In(loc).Format(promoteTimeFormat)
		if !failure.LastSeenAt.Equal(failure.FirstSeenAt) {
			when += "～" + failure.LastSeenAt.In(loc).Format(promoteTimeFormat)
		}
		d.Reasons = append(d.Reasons, fmt.Sprintf("canary 這段期間有沉默失敗：%s %s（%s）",
			failure.DisplayName, when, failure.Reason))
	}
	if more := len(f.Silent) - limit; more > 0 {
		d.Reasons = append(d.Reasons, fmt.Sprintf("+%d", more))
	}
	for _, target := range run.Targets {
		name := target.DisplayName
		if name == "" {
			name = target.MachineID
		}
		if target.Ran && target.Succeeded && target.JobID != "" {
			state := target.IndependentGate
			if state == "" {
				state = IndependentGateUnassigned
			}
			d.IndependentTargets = append(d.IndependentTargets, IndependentGateTarget{
				MachineID: target.MachineID, DisplayName: name, JobID: target.JobID, State: state,
			})
		}
		if target.Retired {
			d.Reasons = append(d.Reasons, fmt.Sprintf("%s 已退場，canary 的結果現在沒有證人", name))
			// 退場已經足以擋 promote；Overview 刻意不替退場機器下現在的判決，
			// 再補一句「沒有判決」只是把同一個缺口講兩次。
			continue
		}
		if target.ExcludedReason != "" {
			d.Reasons = append(d.Reasons, fmt.Sprintf("%s 在 canary 被排除（%s）", name, target.ExcludedReason))
			continue
		}
		if !target.Ran {
			d.Reasons = append(d.Reasons, fmt.Sprintf("%s 沒有跑到 canary 工作單", name))
			continue
		}
		if !target.Succeeded {
			d.Reasons = append(d.Reasons, fmt.Sprintf("%s 的 canary 工作單沒有成功完成", name))
			continue
		}
		if !target.Judged {
			d.Reasons = append(d.Reasons, fmt.Sprintf("%s 現在沒有判決", name))
			continue
		}
		if target.IdentityConflict {
			d.Reasons = append(d.Reasons, fmt.Sprintf("%s 現在有身分衝突，不能當 canary 證人", name))
			continue
		}
		if target.ClockUntrusted {
			d.Reasons = append(d.Reasons, fmt.Sprintf("%s 的時鐘跟 Hub 偏差超過 %s，不能當 canary 證人", name, state.ClockSkewTolerance))
			continue
		}
		if target.Unreachable {
			d.Reasons = append(d.Reasons, fmt.Sprintf("%s 現在失聯", name))
		}
		if target.SilentNow {
			d.Reasons = append(d.Reasons, fmt.Sprintf("%s 現在就有沉默失敗", name))
		}
		if target.LastObservation.IsZero() || now.Sub(target.LastObservation) > state.ObservationStale {
			d.Reasons = append(d.Reasons, fmt.Sprintf("%s 現在沒有新鮮觀測（門檻 %s）", name, state.ObservationStale))
		}
		// 沒有 witness 的 target 往下交給 !WorkloadReady 說出誠實的原因。
		if target.WorkloadObserved && !target.OpenClawPresent {
			d.Reasons = append(d.Reasons, fmt.Sprintf("%s 現在回報 OpenClaw 不存在", name))
			continue
		}
		if !target.WorkloadReady {
			reason := target.WorkloadReason
			if reason == "" {
				reason = "沒有合格的同批 workload 證據"
			}
			d.Reasons = append(d.Reasons, fmt.Sprintf("%s 不能當 canary 證人：%s", name, reason))
			continue
		}
		switch {
		case target.RunningVersion == "":
			d.Reasons = append(d.Reasons, fmt.Sprintf("%s 現在跑的 OpenClaw 版本不知道", name))
		case target.RunningVersion != f.Version:
			d.Reasons = append(d.Reasons, fmt.Sprintf("%s 現在跑的是 %s，不是 %s", name, target.RunningVersion, f.Version))
		}
		applied := strings.TrimPrefix(target.AppliedDigest, "sha256:")
		want := strings.TrimPrefix(f.Digest, "sha256:")
		switch {
		case target.AppliedReason != "":
			d.Reasons = append(d.Reasons, fmt.Sprintf("%s 的 applied witness 不可信：%s", name, target.AppliedReason))
		case applied == "":
			d.Reasons = append(d.Reasons, fmt.Sprintf("%s 找不到成功套用這個 digest 的工作單", name))
		case !strings.EqualFold(applied, want):
			d.Reasons = append(d.Reasons, fmt.Sprintf("%s 最近成功套用的 digest %s，不是 canary 的 %s",
				name, shortPromote(applied, 12), shortPromote(want, 12)))
		}
		state := target.IndependentGate
		if state == "" {
			state = IndependentGateUnassigned
		}
		if state != IndependentGatePassed {
			d.Reasons = append(d.Reasons, independentGateReason(name, target.JobID, state))
		}
	}
	d.Allowed = len(d.Reasons) == 0
	return d
}

func earliestCompleteBusinessDay(finished time.Time, loc *time.Location) time.Time {
	local := finished.In(loc)
	day := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)
	if local.After(day) {
		day = day.AddDate(0, 0, 1)
	}
	for day.Weekday() == time.Saturday || day.Weekday() == time.Sunday {
		day = day.AddDate(0, 0, 1)
	}
	return day.AddDate(0, 0, 1)
}

func (d PromoteDecision) Summary() string {
	if !d.Allowed {
		return "promote 鎖著：" + strings.Join(d.Reasons, "；")
	}
	loc := d.loc
	if loc == nil {
		loc = time.Local
	}
	run := d.facts.Canary
	return fmt.Sprintf("promote 可以：canary %s %s 完成，滿一個完整工作天，期間沒有沉默失敗，跨故障域 verifier 已通過",
		shortPromote(run.DeploymentID, 8), run.FinishedAt.In(loc).Format(promoteTimeFormat))
}

func independentGateReason(name, jobID string, state IndependentGateState) string {
	job := ""
	if jobID != "" {
		job = "（工作單 " + shortPromote(jobID, 8) + "）"
	}
	switch state {
	case IndependentGateAwaitingReport:
		return fmt.Sprintf("%s 的 canary%s 已指派跨故障域 verifier，正在等完整回報", name, job)
	case IndependentGateIncompleteReport:
		return fmt.Sprintf("%s 的 canary%s 只有部分 verifier 規則回報；重新執行已指派的 verifier", name, job)
	case IndependentGateProducerRevoked:
		return fmt.Sprintf("%s 的 canary%s 只剩已撤銷 verifier 的證據；指派仍有效的跨故障域 verifier", name, job)
	case IndependentGateDigestMismatch:
		return fmt.Sprintf("%s 的 canary%s 有獨立證據 digest 衝突；確認 artifact 後重跑 canary", name, job)
	case IndependentGateReleaseMismatch:
		return fmt.Sprintf("%s 的 canary%s 跨故障域 verifier 看到另一個 OpenClaw 版本；修復後重跑 canary", name, job)
	case IndependentGateReleaseUnreported:
		return fmt.Sprintf("%s 的 canary%s 跨故障域 verifier 沒有回報結構化版本；升級後重新指派 verifier", name, job)
	case IndependentGateStale:
		return fmt.Sprintf("%s 的 canary%s 獨立證據早於工作單完成；重新指派 verifier", name, job)
	case IndependentGateFailed:
		return fmt.Sprintf("%s 的 canary%s 獨立證據沒有通過；修復後重跑 canary", name, job)
	default:
		return fmt.Sprintf("%s 的 canary%s 尚未指派跨故障域 verifier；先到工作單指派", name, job)
	}
}

func shortPromote(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
