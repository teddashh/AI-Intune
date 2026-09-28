package settingpolicy

import (
	"crypto/sha256"
	"encoding/hex"
)

// Digest is the stable identity of one settings document. The Hub sends it
// with the settings; the agent echoes back the digest it is actually running
// under. Comparing the two is the whole applied-state evidence.
//
// ⚠ 這是唯一合法的「套用了沒有」證據來源。不准去讀 agent 的 log 或數心跳
// 間隔反推 —— 那是 docs/PRODUCT.md 地基二禁止的自證。
func Digest(s Settings) (string, error) {
	raw, err := s.Canonical()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// MustDigest is Digest for values already known to be valid, such as Defaults.
func MustDigest(s Settings) string {
	d, err := Digest(s)
	if err != nil {
		panic("settingpolicy: digest of invalid settings: " + err.Error())
	}
	return d
}

// Verdict is what the Hub can say about one machine's settings.
type Verdict string

const (
	// VerdictNeverReported: the machine has never checked in. The Hub knows
	// what it would send and nothing about what is running.
	VerdictNeverReported Verdict = "never_reported"
	// VerdictUnknown: the machine checks in but has not echoed a digest yet,
	// which is what an agent too old to report one looks like.
	VerdictUnknown Verdict = "unknown"
	// VerdictMismatch: the machine echoed a digest that is not the one the Hub
	// resolves for it, and it is not the digest of any published revision of
	// the assigned policy either.
	VerdictMismatch Verdict = "mismatch"
	// VerdictPending: the machine is still running an earlier revision of the
	// policy it is assigned. It will pick the new one up on its next check-in.
	VerdictPending Verdict = "pending"
	// VerdictApplied: the machine echoed exactly what the Hub resolves for it.
	VerdictApplied Verdict = "applied"
)

// Report is what one machine last told the Hub about its settings.
type Report struct {
	EverCheckedIn bool
	// ReportedDigest is empty when the machine has never echoed one.
	ReportedDigest string
}

// Judge decides the verdict for one machine. Precedence is strict and runs
// worst-evidence first, so a machine that cannot be measured is never reported
// as applied.
//
// knownDigests are the digests of every published revision of the policy this
// machine is assigned. A machine echoing one of those is behind, not wrong;
// a machine echoing anything else is running settings this Hub did not send.
func Judge(effectiveDigest string, knownDigests []string, r Report) Verdict {
	if !r.EverCheckedIn {
		return VerdictNeverReported
	}
	if r.ReportedDigest == "" {
		return VerdictUnknown
	}
	if r.ReportedDigest == effectiveDigest {
		return VerdictApplied
	}
	for _, d := range knownDigests {
		if d == r.ReportedDigest {
			return VerdictPending
		}
	}
	return VerdictMismatch
}

// Label is the operator-facing wording for a verdict. It states the state and,
// where there is one, the next thing that happens.
func Label(v Verdict) string {
	switch v {
	case VerdictApplied:
		return "已套用"
	case VerdictPending:
		return "下次報到時套用"
	case VerdictMismatch:
		return "機器回報的設定不是 Hub 指派的"
	case VerdictUnknown:
		return "機器還沒回報設定"
	case VerdictNeverReported:
		return "機器從未報到"
	default:
		return string(v)
	}
}
