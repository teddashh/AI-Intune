package compliance

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
)

// Digest is the stable identity of one compliance document.
func Digest(p Policy) (string, error) {
	raw, err := p.Canonical()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// Verdict is what the Hub can say about one machine's compliance.
type Verdict string

const (
	// VerdictNotEvaluated: no compliance policy resolves for this machine, so
	// nothing was asked of it. This is not a pass.
	VerdictNotEvaluated Verdict = "not_evaluated"
	// VerdictNeverReported: a policy applies but the machine has never checked
	// in, so not one rule has evidence.
	VerdictNeverReported Verdict = "never_reported"
	// VerdictNoncompliant: at least one rule definitively failed.
	VerdictNoncompliant Verdict = "noncompliant"
	// VerdictUnmeasured: nothing failed, but at least one rule had no evidence
	// to evaluate. Not compliant, because compliant means measured.
	VerdictUnmeasured Verdict = "unmeasured"
	// VerdictCompliant: every rule was evaluated and every rule passed.
	VerdictCompliant Verdict = "compliant"
)

// Judge decides one machine's verdict from its rule results.
//
// Precedence runs worst-evidence first with one deliberate exception: a rule
// that definitively failed outranks a rule that could not be measured. A known
// failure is information; a missing measurement is the absence of it, and
// hiding the failure behind the gap would make the fleet look calmer than it
// is. Everything after that is the house rule — green means measured.
//
// ⚠ 判決只吃 Hub 自己算出來的 RuleResult。沒有任何一條路徑讓機器自己說它合規。
func Judge(assigned bool, everCheckedIn bool, results []RuleResult) Verdict {
	if !assigned {
		return VerdictNotEvaluated
	}
	if !everCheckedIn {
		return VerdictNeverReported
	}
	unmeasured := false
	for _, r := range results {
		switch r.Outcome {
		case OutcomeFail:
			return VerdictNoncompliant
		case OutcomeUnmeasured:
			unmeasured = true
		}
	}
	if unmeasured {
		return VerdictUnmeasured
	}
	if len(results) == 0 {
		// 一份沒有規則的原則發佈不出來，所以走到這裡表示上游拿到的是空的，
		// 而空的東西沒有通過任何檢查。
		return VerdictUnmeasured
	}
	return VerdictCompliant
}

// Label is the operator-facing wording for a verdict. It states the state and,
// where there is one, what to do about it.
func Label(v Verdict) string {
	switch v {
	case VerdictCompliant:
		return "符合"
	case VerdictNoncompliant:
		return "不符合"
	case VerdictUnmeasured:
		return "有規則量不到"
	case VerdictNeverReported:
		return "機器從未報到"
	case VerdictNotEvaluated:
		return "未指派合規性原則"
	default:
		return string(v)
	}
}

// RuleLabel is the operator-facing name of one condition.
// RuleBound is the threshold in the words the console puts next to the rule
// name. A rule that takes no parameter has no bound and returns "".
//
// ⚠ AgentVersion 是操作員自己打進來的要求值，不是裝置回報的值，所以原樣顯示
// 是安全的 —— 裝置回報的那一側走 versionShaped。
func RuleBound(r Rule) string {
	switch r.Kind {
	case RuleCheckinMaxAge:
		return fmt.Sprintf("最久 %d 秒", r.MaxAgeSeconds)
	case RuleAgentVersion:
		return r.AgentVersion
	case RuleDiskFreeMinPercent:
		return fmt.Sprintf("至少 %d%%", r.MinFreePercent)
	default:
		return ""
	}
}

func RuleLabel(k RuleKind) string {
	switch k {
	case RuleCheckinMaxAge:
		return "報到新鮮度"
	case RuleAgentVersion:
		return "Agent 版本"
	case RuleSettingsApplied:
		return "設定已套用"
	case RuleDiskFreeMinPercent:
		return "磁碟剩餘空間"
	case RuleJobsEnabled:
		return "接受工作單"
	default:
		return string(k)
	}
}

// ---------------------------------------------------------------- resolution

// Scope is where an assignment was made: a machine or a channel, the same two
// the settings and desired-state planes allow.
type Scope string

const (
	ScopeMachine Scope = "machine"
	ScopeChannel Scope = "channel"
)

// Assignment is one published policy pinned to one scope.
type Assignment struct {
	Scope      Scope  `json:"scope"`
	ScopeID    string `json:"scope_id"`
	PolicyID   string `json:"policy_id"`
	Revision   int64  `json:"policy_revision"`
	Digest     string `json:"policy_digest"`
	Policy     Policy `json:"policy"`
	AssignedAt string `json:"assigned_at"`
}

// Source says which assignment produced the policy a machine is judged by.
type Source string

const (
	// SourceNone: nothing is assigned. Unlike settings, there is no product
	// default here -- a machine nobody set conditions for has no conditions,
	// and claiming otherwise would invent an operator decision.
	SourceNone    Source = "none"
	SourceChannel Source = "channel"
	SourceMachine Source = "machine"
)

// Effective is the answer to "which conditions is this machine judged by, and
// why".
type Effective struct {
	Source Source `json:"source"`
	// PolicyID/Revision/Digest/Policy are empty for SourceNone.
	PolicyID string `json:"policy_id,omitempty"`
	Revision int64  `json:"policy_revision,omitempty"`
	Digest   string `json:"policy_digest,omitempty"`
	Policy   Policy `json:"policy,omitzero"`
}

// Assigned reports whether any policy applies to this machine.
func (e Effective) Assigned() bool { return e.Source != SourceNone }

// Resolve picks the policy one machine is judged by. Machine beats channel,
// strictly: a machine assignment is a decision somebody made about this one
// machine, so a later channel-wide assignment must not silently take it back.
func Resolve(machineID, channel string, assignments []Assignment) Effective {
	var machineHit, channelHit *Assignment
	for i := range assignments {
		a := &assignments[i]
		switch a.Scope {
		case ScopeMachine:
			if a.ScopeID == machineID && (machineHit == nil || newer(*a, *machineHit)) {
				machineHit = a
			}
		case ScopeChannel:
			if channel != "" && a.ScopeID == channel && (channelHit == nil || newer(*a, *channelHit)) {
				channelHit = a
			}
		}
	}
	switch {
	case machineHit != nil:
		return Effective{Source: SourceMachine, PolicyID: machineHit.PolicyID,
			Revision: machineHit.Revision, Digest: machineHit.Digest, Policy: machineHit.Policy}
	case channelHit != nil:
		return Effective{Source: SourceChannel, PolicyID: channelHit.PolicyID,
			Revision: channelHit.Revision, Digest: channelHit.Digest, Policy: channelHit.Policy}
	default:
		return Effective{Source: SourceNone}
	}
}

// newer breaks ties inside one scope by assignment time, then by revision.
func newer(a, b Assignment) bool {
	if a.AssignedAt != b.AssignedAt {
		return a.AssignedAt > b.AssignedAt
	}
	return a.Revision > b.Revision
}

// SortAssignments puts assignments in a stable display order.
func SortAssignments(rows []Assignment) {
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Scope != rows[j].Scope {
			return rows[i].Scope == ScopeMachine
		}
		if rows[i].ScopeID != rows[j].ScopeID {
			return rows[i].ScopeID < rows[j].ScopeID
		}
		return rows[i].AssignedAt > rows[j].AssignedAt
	})
}
