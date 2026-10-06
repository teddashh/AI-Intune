package maintenance

import "fmt"

const (
	AlertAttention         = "attention"
	AlertStale             = "stale"
	AlertDiskOverThreshold = "disk_over_threshold"
)

// AlertView is one condition the Hub may notify. Fingerprint is stable for
// as long as the condition is unchanged, so a loop does not re-alert.
type AlertView struct {
	Condition   string
	Active      bool
	Fingerprint string
}

// StoredAlert is the last decision persisted for one machine and condition.
type StoredAlert struct {
	Fingerprint string
	Active      bool
	Delivered   bool
}

// AlertViews lists the three disk-clean conditions. Disk-over-threshold is
// active only after an apply or mixed run whose Hub disk evaluation failed.
// A dry-run does not raise that condition. Attention and staleness are
// independent of the job's success.
func AlertViews(v Verdict) []AlertView {
	attentionFP := v.Attention
	if attentionFP == "" {
		attentionFP = "clear"
	}
	staleFP := "stale"
	if v.Outcome == OutcomeStale && v.Mode == "" {
		staleFP = "missing"
	}
	diskFP := "clear"
	diskActive := v.Disk.RuleAssigned && v.Disk.Outcome == OutcomeFail && (v.Mode == "apply" || v.Mode == "mixed")
	if diskActive {
		diskFP = fmt.Sprintf("below:%d", v.Disk.MinFreePercent)
	}
	return []AlertView{
		{Condition: AlertAttention, Active: v.Attention != "", Fingerprint: attentionFP},
		{Condition: AlertStale, Active: v.Stale, Fingerprint: staleFP},
		{Condition: AlertDiskOverThreshold, Active: diskActive, Fingerprint: diskFP},
	}
}

// ShouldNotify is true when an active condition has not yet been delivered
// for this fingerprint. A cleared condition does not notify. The same
// fingerprint stays quiet until it clears or the delivery did not land.
func ShouldNotify(prev StoredAlert, next AlertView) bool {
	if !next.Active {
		return false
	}
	if !prev.Active || !prev.Delivered || prev.Fingerprint != next.Fingerprint {
		return true
	}
	return false
}
