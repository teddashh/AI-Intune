// Package compliance holds the closed set of conditions the Hub can evaluate
// about one machine, and the pure evaluation that turns a published policy
// plus measured facts into a verdict.
//
// 規則是**封閉的**，理由跟 settingpolicy 一樣：開一個任意 expression 語言會讓
// 操作員寫得出 Hub 根本量不到的條件，而畫面只能回「不知道」。這裡每一條規則
// 都對應 Hub 手上已經有的證據。
//
// ⚠ 沒有任何規則讀 agent 說的「我合規」。Agent 只回報事實，判決永遠是 Hub 算的
// （docs/SPEC.md §2.1 規則 1）。說明書 3060 頁對 Intune 自己的自訂合規性下了同一個
// 警告：設定值是裝置回報的，服務不驗證它，不要拿它當管理動作的唯一依據。
package compliance

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// SchemaVersion is the compliance document version. The Hub refuses a document
// it does not recognise rather than best-effort parsing it.
const SchemaVersion = 1

// MaxRules bounds one policy. A policy nobody can read in one screen is a
// policy nobody reviews.
const MaxRules = 8

// RuleKind is one condition the Hub can evaluate. Adding a kind means adding
// the evidence that answers it; there is no generic escape hatch.
type RuleKind string

const (
	// RuleCheckinMaxAge: the Hub received a check-in from this machine within
	// the given window. The clock is the Hub's received_at, never the agent's.
	RuleCheckinMaxAge RuleKind = "checkin_max_age"
	// RuleAgentVersion: the machine reports exactly this agent version.
	RuleAgentVersion RuleKind = "agent_version"
	// RuleDiskFreeMinPercent: free disk is at or above this share of total.
	RuleDiskFreeMinPercent RuleKind = "disk_free_min_percent"
	// RuleSettingsApplied: the machine echoed the settings digest the Hub
	// resolves for it. This is the settings board's verdict, not a new one.
	RuleSettingsApplied RuleKind = "settings_applied"
	// RuleJobsEnabled: the machine reports that it accepts work.
	RuleJobsEnabled RuleKind = "jobs_enabled"
)

// Bounds. A check-in window under a minute would mark a healthy fleet
// noncompliant between two heartbeats; a week is long enough that the answer
// stops being about today.
const (
	MinCheckinMaxAgeSeconds = 60
	MaxCheckinMaxAgeSeconds = 604800
	MinFreePercent          = 1
	MaxFreePercent          = 99
	MaxAgentVersionBytes    = 64
)

// Rule is one condition. Exactly the parameter its kind uses may be set: a
// rule carrying a parameter it never reads would show an operator a condition
// that is not the one being evaluated.
type Rule struct {
	Kind RuleKind `json:"kind"`
	// MaxAgeSeconds belongs to RuleCheckinMaxAge.
	MaxAgeSeconds int `json:"max_age_seconds,omitempty"`
	// AgentVersion belongs to RuleAgentVersion.
	AgentVersion string `json:"agent_version,omitempty"`
	// MinFreePercent belongs to RuleDiskFreeMinPercent.
	MinFreePercent int `json:"min_free_percent,omitempty"`
}

// Policy is one complete set of conditions and the consequences of failing
// them. A policy with no actions is a policy that only reports: the board
// turns red and nothing else happens, which is the right first step.
type Policy struct {
	SchemaVersion int      `json:"schema_version"`
	Rules         []Rule   `json:"rules"`
	Actions       []Action `json:"actions,omitempty"`
}

var ErrInvalid = errors.New("compliance: invalid policy")

// ruleOrder fixes the display and canonical order. It is the order an operator
// reads them in, worst-blast-radius first.
var ruleOrder = map[RuleKind]int{
	RuleCheckinMaxAge:      0,
	RuleAgentVersion:       1,
	RuleSettingsApplied:    2,
	RuleDiskFreeMinPercent: 3,
	RuleJobsEnabled:        4,
}

// RuleKinds returns every kind in canonical order.
func RuleKinds() []RuleKind {
	out := make([]RuleKind, 0, len(ruleOrder))
	for kind := range ruleOrder {
		out = append(out, kind)
	}
	sort.Slice(out, func(i, j int) bool { return ruleOrder[out[i]] < ruleOrder[out[j]] })
	return out
}

// Normalize puts the rules and actions in canonical order so two policies that
// say the same thing have the same bytes and therefore the same digest.
func (p *Policy) Normalize() {
	sort.SliceStable(p.Rules, func(i, j int) bool {
		return ruleOrder[p.Rules[i].Kind] < ruleOrder[p.Rules[j].Kind]
	})
	sort.SliceStable(p.Actions, func(i, j int) bool {
		return actionOrder[p.Actions[i].Kind] < actionOrder[p.Actions[j].Kind]
	})
}

// Validate reports the first reason this document cannot be published. The
// message is operator copy: it names the rule, the bound, and the value that
// broke it.
func (p Policy) Validate() error {
	if p.SchemaVersion != SchemaVersion {
		return fmt.Errorf("%w: schema_version is %d, this Hub only recognizes %d",
			ErrInvalid, p.SchemaVersion, SchemaVersion)
	}
	if len(p.Rules) == 0 {
		return fmt.Errorf("%w: a compliance policy must have at least one rule", ErrInvalid)
	}
	if len(p.Rules) > MaxRules {
		return fmt.Errorf("%w: has %d rules, limit is %d rules", ErrInvalid, len(p.Rules), MaxRules)
	}
	seen := make(map[RuleKind]struct{}, len(p.Rules))
	for _, rule := range p.Rules {
		if _, ok := ruleOrder[rule.Kind]; !ok {
			return fmt.Errorf("%w: unknown rule %q", ErrInvalid, string(rule.Kind))
		}
		if _, duplicate := seen[rule.Kind]; duplicate {
			return fmt.Errorf("%w: rule %s appears twice; only one rule allowed per condition",
				ErrInvalid, rule.Kind)
		}
		seen[rule.Kind] = struct{}{}
		if err := rule.validate(); err != nil {
			return err
		}
	}
	// 動作不另外設上限：每一種後果只准出現一次，所以認得的動作種類就是上限，
	// 而超出的那一個一定會先撞上「重複」或「不認得」，操作員拿到的是更精確的
	// 那句話。
	seenAction := make(map[ActionKind]struct{}, len(actionOrder))
	for _, action := range p.Actions {
		if _, duplicate := seenAction[action.Kind]; duplicate {
			return fmt.Errorf("%w: action %s appears twice; only one grace period allowed per consequence",
				ErrInvalid, action.Kind)
		}
		seenAction[action.Kind] = struct{}{}
		if err := action.validate(); err != nil {
			return err
		}
	}
	return nil
}

func (r Rule) validate() error {
	// 每一條規則只准帶它自己會讀的參數。多帶的參數會被靜靜忽略，而操作員看到的
	// 就不是他寫的那條規則。
	unused := func(fields ...string) error {
		if len(fields) == 0 {
			return nil
		}
		return fmt.Errorf("%w: rule %s does not use %s", ErrInvalid, r.Kind, strings.Join(fields, ", "))
	}
	var extra []string
	if r.Kind != RuleCheckinMaxAge && r.MaxAgeSeconds != 0 {
		extra = append(extra, "max_age_seconds")
	}
	if r.Kind != RuleAgentVersion && r.AgentVersion != "" {
		extra = append(extra, "agent_version")
	}
	if r.Kind != RuleDiskFreeMinPercent && r.MinFreePercent != 0 {
		extra = append(extra, "min_free_percent")
	}
	if err := unused(extra...); err != nil {
		return err
	}
	switch r.Kind {
	case RuleCheckinMaxAge:
		if r.MaxAgeSeconds < MinCheckinMaxAgeSeconds || r.MaxAgeSeconds > MaxCheckinMaxAgeSeconds {
			return fmt.Errorf("%w: max_age_seconds is %d, allowed range is %d-%d seconds",
				ErrInvalid, r.MaxAgeSeconds, MinCheckinMaxAgeSeconds, MaxCheckinMaxAgeSeconds)
		}
	case RuleAgentVersion:
		if _, ok := versionShaped(r.AgentVersion); !ok {
			return fmt.Errorf("%w: agent_version must be 1-%d version characters (alphanumeric, '.', '-', '_', '+')",
				ErrInvalid, MaxAgentVersionBytes)
		}
	case RuleDiskFreeMinPercent:
		if r.MinFreePercent < MinFreePercent || r.MinFreePercent > MaxFreePercent {
			return fmt.Errorf("%w: min_free_percent is %d, allowed range is %d-%d",
				ErrInvalid, r.MinFreePercent, MinFreePercent, MaxFreePercent)
		}
	}
	return nil
}

// Canonical returns the byte form used for digests and storage.
func (p Policy) Canonical() ([]byte, error) {
	p.Rules = append([]Rule(nil), p.Rules...)
	p.Actions = append([]Action(nil), p.Actions...)
	(&p).Normalize()
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(p)
}

// Parse decodes a stored or submitted document. Unknown fields are refused:
// silently dropping a field an operator typed would show them a policy that is
// not the one they wrote.
func Parse(raw []byte) (Policy, error) {
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	var p Policy
	if err := dec.Decode(&p); err != nil {
		return Policy{}, fmt.Errorf("%w: %s", ErrInvalid, firstLine(err.Error()))
	}
	if dec.More() {
		return Policy{}, fmt.Errorf("%w: multiple JSON documents in file", ErrInvalid)
	}
	(&p).Normalize()
	if err := p.Validate(); err != nil {
		return Policy{}, err
	}
	return p, nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// versionShaped answers whether a version string can be put in front of an
// operator as-is.
//
// ⚠ 回報的版本是**裝置報上來的**，Hub 沒有驗證過它。說明書 3060 頁對這件事寫得
// 很直白：裝置回報的值可能是自由文字、URL 或檔案路徑，少數情況下甚至會試圖指揮
// 看畫面的人。本產品的版本字串是 git describe 的輸出，字元集很窄；不是這個形狀的
// 一律不顯示原文 —— 而「它回報的東西根本不像版本」本身就是判決的一部分。
func versionShaped(v string) (string, bool) {
	if v == "" || len(v) > MaxAgentVersionBytes {
		return "", false
	}
	for _, char := range v {
		switch {
		case char >= '0' && char <= '9',
			char >= 'a' && char <= 'z',
			char >= 'A' && char <= 'Z',
			char == '.', char == '-', char == '_', char == '+':
		default:
			return "", false
		}
	}
	return v, true
}

// ---------------------------------------------------------------- evaluation

// Facts is everything about one machine that a rule may read. Absent evidence
// is absent, never zero: a rule whose fact is missing returns Unmeasured, and
// the machine is not called compliant on it.
type Facts struct {
	// EverCheckedIn is false for a machine the Hub has never heard from.
	EverCheckedIn bool
	// CheckinAge is how long ago the Hub received the newest check-in,
	// measured on the Hub's clock. Only meaningful when EverCheckedIn.
	CheckinAge time.Duration
	// AgentVersion is what the newest check-in reported. Device-reported.
	AgentVersion string
	// DiskFreeBytes/DiskTotalBytes come from the newest check-in.
	DiskFreeBytes  int64
	DiskTotalBytes int64
	// JobsEnabled is nil when the machine did not report it.
	JobsEnabled *bool
	// SettingsApplied is nil when the settings verdict itself is unmeasurable:
	// the machine never reported, or it runs an agent too old to echo a digest.
	SettingsApplied *bool
}

// Outcome is what one rule concluded.
type Outcome string

const (
	OutcomePass Outcome = "pass"
	OutcomeFail Outcome = "fail"
	// OutcomeUnmeasured: the Hub does not hold the evidence this rule needs.
	// It is not a pass and it is not a failure; it is the absence of an answer.
	OutcomeUnmeasured Outcome = "unmeasured"
)

// RuleResult is one rule's conclusion with the sentence an operator reads.
type RuleResult struct {
	Kind    RuleKind `json:"kind"`
	Outcome Outcome  `json:"outcome"`
	Detail  string   `json:"detail"`
}

// Evaluate runs every rule in the policy against one machine's facts. The
// results come back in canonical rule order, one per rule, always.
func Evaluate(p Policy, f Facts) []RuleResult {
	p.Rules = append([]Rule(nil), p.Rules...)
	(&p).Normalize()
	out := make([]RuleResult, 0, len(p.Rules))
	for _, rule := range p.Rules {
		out = append(out, rule.evaluate(f))
	}
	return out
}

func (r Rule) evaluate(f Facts) RuleResult {
	result := func(outcome Outcome, detail string) RuleResult {
		return RuleResult{Kind: r.Kind, Outcome: outcome, Detail: detail}
	}
	if !f.EverCheckedIn {
		return result(OutcomeUnmeasured, "機器從未報到，這條規則沒有可以評估的證據")
	}
	switch r.Kind {
	case RuleCheckinMaxAge:
		age := int64(f.CheckinAge / time.Second)
		if age <= int64(r.MaxAgeSeconds) {
			return result(OutcomePass, fmt.Sprintf("上次報到在 %d 秒前，上限 %d 秒",
				age, r.MaxAgeSeconds))
		}
		return result(OutcomeFail, fmt.Sprintf("上次報到在 %d 秒前，超過上限 %d 秒",
			age, r.MaxAgeSeconds))
	case RuleAgentVersion:
		if f.AgentVersion == r.AgentVersion {
			return result(OutcomePass, fmt.Sprintf("回報的 agent 版本是 %s", r.AgentVersion))
		}
		shown, ok := versionShaped(f.AgentVersion)
		if !ok {
			if f.AgentVersion == "" {
				return result(OutcomeUnmeasured, fmt.Sprintf(
					"機器沒有回報 agent 版本；要求 %s", r.AgentVersion))
			}
			return result(OutcomeFail, fmt.Sprintf(
				"機器回報的不是版本字串；要求 %s", r.AgentVersion))
		}
		return result(OutcomeFail, fmt.Sprintf("回報的是 %s，要求 %s", shown, r.AgentVersion))
	case RuleDiskFreeMinPercent:
		if f.DiskTotalBytes <= 0 || f.DiskFreeBytes < 0 || f.DiskFreeBytes > f.DiskTotalBytes {
			return result(OutcomeUnmeasured, "機器沒有回報可用的磁碟容量")
		}
		// 用整數算，避免浮點讓邊界值在不同機器上落在不同邊。
		percent := f.DiskFreeBytes * 100 / f.DiskTotalBytes
		if percent >= int64(r.MinFreePercent) {
			return result(OutcomePass, fmt.Sprintf("剩餘空間 %d%%，下限 %d%%",
				percent, r.MinFreePercent))
		}
		return result(OutcomeFail, fmt.Sprintf("剩餘空間 %d%%，低於下限 %d%%",
			percent, r.MinFreePercent))
	case RuleSettingsApplied:
		if f.SettingsApplied == nil {
			return result(OutcomeUnmeasured, "機器還沒回報它在跑哪一份設定")
		}
		if *f.SettingsApplied {
			return result(OutcomePass, "回報的設定就是 Hub 指派的那一份")
		}
		return result(OutcomeFail, "回報的設定不是 Hub 指派的那一份")
	case RuleJobsEnabled:
		if f.JobsEnabled == nil {
			return result(OutcomeUnmeasured, "機器沒有回報它收不收工作單")
		}
		if *f.JobsEnabled {
			return result(OutcomePass, "機器回報它收工作單")
		}
		return result(OutcomeFail, "機器回報它不收工作單")
	}
	return result(OutcomeUnmeasured, "這個 Hub 不認得這條規則")
}
