package maintenance

import (
	"fmt"
	"time"

	"github.com/teddashh/AI-Intune/internal/compliance"
)

const (
	OutcomePass       = "pass"
	OutcomeFail       = "fail"
	OutcomeAttention  = "attention"
	OutcomeStale      = "stale"
	OutcomeUnmeasured = "unmeasured"

	// StaleAfter matches RootMaxAgeHours. A missing summary is stale immediately.
	StaleAfter = RootMaxAgeHours * time.Hour
)

// DiskEvaluation is the Hub's own disk_free_min_percent reading. It is not
// the script's attention hint. Unmeasured means no rule is assigned, or the
// latest observation has no usable disk bytes.
type DiskEvaluation struct {
	Outcome        string `json:"outcome"`
	Detail         string `json:"detail,omitempty"`
	Percent        int    `json:"percent,omitempty"`
	MinFreePercent int    `json:"min_free_percent,omitempty"`
	RuleAssigned   bool   `json:"rule_assigned"`
}

// EvaluateObservationDisk runs the existing compliance rule against disk
// bytes the Hub read from the latest resources observation.
func EvaluateObservationDisk(free, total int64, rule *compliance.Rule) DiskEvaluation {
	if rule == nil || rule.Kind != compliance.RuleDiskFreeMinPercent {
		return DiskEvaluation{Outcome: OutcomeUnmeasured, Detail: "no disk_free_min_percent rule is assigned"}
	}
	policy := compliance.Policy{SchemaVersion: compliance.SchemaVersion, Rules: []compliance.Rule{*rule}}
	results := compliance.Evaluate(policy, compliance.Facts{
		EverCheckedIn:  true,
		DiskFreeBytes:  free,
		DiskTotalBytes: total,
	})
	for _, result := range results {
		if result.Kind != compliance.RuleDiskFreeMinPercent {
			continue
		}
		percent := 0
		if total > 0 && free >= 0 && free <= total {
			percent = int(free * 100 / total)
		}
		return DiskEvaluation{
			Outcome:        string(result.Outcome),
			Detail:         result.Detail,
			Percent:        percent,
			MinFreePercent: rule.MinFreePercent,
			RuleAssigned:   true,
		}
	}
	return DiskEvaluation{Outcome: OutcomeUnmeasured, Detail: "disk rule was not evaluated", RuleAssigned: true, MinFreePercent: rule.MinFreePercent}
}

// Verdict is the Hub's decision. The script's attention string is a hint.
// A digest mismatch or a failed disk rule after an apply is a fail even when
// the executor job itself succeeded.
type Verdict struct {
	Outcome   string         `json:"outcome"`
	DigestOK  bool           `json:"digest_ok"`
	Stale     bool           `json:"stale"`
	Attention string         `json:"attention,omitempty"`
	Mode      string         `json:"mode,omitempty"`
	Disk      DiskEvaluation `json:"disk"`
	Reasons   []string       `json:"reasons,omitempty"`
}

// Decide compares one summary with the digest the Hub rendered for the
// revision, the Hub clock of when that summary was received, and the Hub's
// disk evaluation. summary may be nil when the machine has never reported.
// receivedAt is the Hub clock. A nil summary is stale.
func Decide(summary *Summary, receivedAt, now time.Time, expectedDigest string, disk DiskEvaluation) Verdict {
	now = now.UTC()
	v := Verdict{Disk: disk}
	if summary == nil {
		v.Outcome = OutcomeStale
		v.Stale = true
		v.Reasons = []string{"no disk-clean summary has been received"}
		return v
	}
	v.Mode = summary.Mode
	v.Attention = summary.Attention
	v.DigestOK = expectedDigest != "" && expectedDigest != "none(defaults)" && summary.ConfigDigest == expectedDigest
	if !v.DigestOK {
		v.Reasons = append(v.Reasons, fmt.Sprintf("config_digest %s does not match the revision digest %s", summary.ConfigDigest, expectedDigest))
	}
	age := now.Sub(receivedAt.UTC())
	if receivedAt.IsZero() || age > StaleAfter {
		v.Stale = true
		v.Reasons = append(v.Reasons, fmt.Sprintf("summary is stale: Hub received it %s ago, limit %s", age.Truncate(time.Second), StaleAfter))
	}
	if summary.Attention != "" {
		v.Reasons = append(v.Reasons, "script attention: "+summary.Attention)
	}
	diskAfterApply := (summary.Mode == "apply" || summary.Mode == "mixed") && disk.Outcome == OutcomeFail
	if diskAfterApply {
		v.Reasons = append(v.Reasons, "disk is still over the Hub threshold after apply: "+disk.Detail)
	}
	switch {
	case !v.DigestOK || diskAfterApply:
		v.Outcome = OutcomeFail
	case summary.Attention != "":
		v.Outcome = OutcomeAttention
	case v.Stale:
		v.Outcome = OutcomeStale
	default:
		v.Outcome = OutcomePass
	}
	return v
}

// ObservationPredatesSummary reports whether a resources observation is older
// than a disk-clean summary. Hub clocks are compared with Hub clocks
// (received_at) and agent clocks with agent clocks (measured_at vs the
// script's ts). Either comparison showing the observation is older is enough.
func ObservationPredatesSummary(obsMeasured, obsReceived time.Time, summaryTS string, summaryReceived time.Time) bool {
	if !summaryReceived.IsZero() && !obsReceived.IsZero() && obsReceived.Before(summaryReceived) {
		return true
	}
	if ts, err := time.Parse(time.RFC3339, summaryTS); err == nil && !obsMeasured.IsZero() && obsMeasured.Before(ts) {
		return true
	}
	return false
}
