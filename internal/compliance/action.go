package compliance

import (
	"fmt"
	"sort"
	"time"
)

// ActionKind is one consequence the Hub can attach to a failing policy.
//
// 動作是**封閉的**，理由跟規則一樣：Hub 只能做它自己控制得了的事。這裡每一個
// 動作都對應 Hub 手上真的握著的那個開關，不需要 agent 配合，也不需要相信 agent
// 說了什麼。
type ActionKind string

const (
	// ActionBlockJobs: the Hub stops handing this machine new work, and leaves
	// it out of new deployments so nobody opens a job that can never be taken.
	ActionBlockJobs ActionKind = "block_jobs"
)

// MaxGraceSeconds bounds a grace period at one day.
//
// 上限不是隨便訂的：Hub 要證明「這段期間一直不符合」，唯一的證據是這段期間裡的
// 每一次 check-in 都不符合，所以寬限期有多長，要掃的 check-in 就有多長。機隊的
// 報到間隔是分鐘級，一天的窗已經是幾千列；再長下去就得改成存判決，而判決不准存。
const MaxGraceSeconds = 86400

// Action is one consequence plus how long a machine may stay noncompliant
// before it takes effect.
type Action struct {
	Kind ActionKind `json:"kind"`
	// GraceSeconds is how long uninterrupted noncompliance is tolerated before
	// the action applies. Zero means it applies as soon as the verdict says
	// noncompliant.
	GraceSeconds int `json:"grace_seconds,omitempty"`
}

var actionOrder = map[ActionKind]int{
	ActionBlockJobs: 0,
}

// ActionKinds returns every kind in canonical order.
func ActionKinds() []ActionKind {
	out := make([]ActionKind, 0, len(actionOrder))
	for kind := range actionOrder {
		out = append(out, kind)
	}
	sort.Slice(out, func(i, j int) bool { return actionOrder[out[i]] < actionOrder[out[j]] })
	return out
}

// ActionLabel is the operator-facing name of one consequence.
func ActionLabel(kind ActionKind) string {
	switch kind {
	case ActionBlockJobs:
		return "停發工作單"
	default:
		return string(kind)
	}
}

// ActionEffect is the sentence that says what the machine loses.
func ActionEffect(kind ActionKind) string {
	switch kind {
	case ActionBlockJobs:
		return "不再領到新的工作單，也不會被算進新的部署"
	default:
		return ""
	}
}

func (a Action) validate() error {
	if _, ok := actionOrder[a.Kind]; !ok {
		return fmt.Errorf("%w: unknown action %q", ErrInvalid, string(a.Kind))
	}
	if a.GraceSeconds < 0 || a.GraceSeconds > MaxGraceSeconds {
		return fmt.Errorf("%w: grace_seconds is %d, allowed range is 0-%d seconds",
			ErrInvalid, a.GraceSeconds, MaxGraceSeconds)
	}
	return nil
}

// MaxGrace is the longest grace period in the policy. It is also the width of
// the evidence window the Hub has to read to prove uninterrupted noncompliance.
func (p Policy) MaxGrace() time.Duration {
	longest := 0
	for _, a := range p.Actions {
		if a.GraceSeconds > longest {
			longest = a.GraceSeconds
		}
	}
	return time.Duration(longest) * time.Second
}

// ---------------------------------------------------------------- decision

// ActionState is what one action is currently doing to one machine.
type ActionState string

const (
	// ActionStateNotTriggered: the machine is not noncompliant, so nothing is
	// being withheld from it.
	ActionStateNotTriggered ActionState = "not_triggered"
	// ActionStateInGrace: the machine is noncompliant and the grace period has
	// not run out yet. Nothing is being withheld yet.
	ActionStateInGrace ActionState = "in_grace"
	// ActionStateEnforced: the action is in force right now.
	ActionStateEnforced ActionState = "enforced"
)

// ActionStateLabel is the operator-facing wording.
func ActionStateLabel(s ActionState) string {
	switch s {
	case ActionStateNotTriggered:
		return "未觸發"
	case ActionStateInGrace:
		return "寬限中"
	case ActionStateEnforced:
		return "生效中"
	default:
		return string(s)
	}
}

// ActionOutcome is one action's current effect on one machine.
type ActionOutcome struct {
	Kind  ActionKind  `json:"kind"`
	State ActionState `json:"state"`
	// DueAt is the moment the action starts, for a machine still inside its
	// grace period. Absent when the action is already in force or untriggered.
	DueAt *time.Time `json:"due_at,omitempty"`
	// Since is when this machine's uninterrupted noncompliance began. Absent
	// when the machine is not noncompliant, or when it began before the
	// evidence window the Hub read, in which case SinceIsFloor says so.
	Since *time.Time `json:"since,omitempty"`
	// SinceIsFloor reports that noncompliance began at or before Since; the
	// Hub read a bounded window and the run did not start inside it.
	SinceIsFloor bool `json:"since_is_floor,omitempty"`
}

// DecideActions says what every action in the policy is doing to one machine
// right now.
//
// ⚠ 這是純函式，也是唯一一處決定「動作生不生效」的地方。它不寫任何東西：動作的
// 效果是這個判斷的函數，不是資料庫裡的一個欄位。存下「已封鎖」會讓某台機器在它
// 早就恢復之後還被鎖著——跟存判決是同一個錯。
//
// since 是這台機器「連續不符合」的起點；sinceIsFloor 表示 Hub 只讀了一段有限的
// 證據窗，這段連續不符合在窗開始之前就已經在跑了。窗的寬度就是最長的寬限期，
// 所以 floor 成立時每一個動作都必然已經到期。
func DecideActions(p Policy, verdict Verdict, since time.Time, sinceIsFloor bool,
	now time.Time,
) []ActionOutcome {
	actions := append([]Action(nil), p.Actions...)
	sort.SliceStable(actions, func(i, j int) bool {
		return actionOrder[actions[i].Kind] < actionOrder[actions[j].Kind]
	})
	out := make([]ActionOutcome, 0, len(actions))
	for _, a := range actions {
		outcome := ActionOutcome{Kind: a.Kind, State: ActionStateNotTriggered}
		switch {
		case verdict != VerdictNoncompliant:
			// 只有「確定有一條規則沒過」才會有後果。量不到不是不符合，從未報到
			// 也不是——把沒有證據當成違規，等於讓一台剛裝好的機器先被懲罰。
		case sinceIsFloor:
			at := since
			outcome.State, outcome.Since, outcome.SinceIsFloor = ActionStateEnforced, &at, true
		default:
			at := since
			due := since.Add(time.Duration(a.GraceSeconds) * time.Second)
			outcome.Since = &at
			if now.Before(due) {
				outcome.State, outcome.DueAt = ActionStateInGrace, &due
			} else {
				outcome.State = ActionStateEnforced
			}
		}
		out = append(out, outcome)
	}
	return out
}

// Blocks reports whether these outcomes currently withhold work from a machine.
func Blocks(outcomes []ActionOutcome) bool {
	for _, o := range outcomes {
		if o.Kind == ActionBlockJobs && o.State == ActionStateEnforced {
			return true
		}
	}
	return false
}
