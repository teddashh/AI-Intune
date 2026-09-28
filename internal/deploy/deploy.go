// Package deploy 是 Hub 的部署判決層 —— 拿 revision 與工作單事件算出下一個狀態。
//
// 這裡只處理純邏輯；持久化、租約時間與驗證證據的取得都由外層負責。
package deploy

import "fmt"

// Revision 是 Hub 對同一資源範圍發出的單調版本號。
type Revision int64

// Watermarks 分開記錄看過與實際套用過的最高 revision。
type Watermarks struct {
	MaxSeen    Revision
	MaxApplied Revision
}

// RejectionCode 是 Agent 合法拒絕工作單時回報的業務碼。
// 拒絕是協定中的結果，不是 error。
type RejectionCode string

const (
	StaleRevision          RejectionCode = "STALE_REVISION"
	DuplicateJobID         RejectionCode = "DUPLICATE_JOB_ID"
	ArtifactHashMismatch   RejectionCode = "ARTIFACT_HASH_MISMATCH"
	LeaseInvalid           RejectionCode = "LEASE_INVALID"
	IrreversibleMigration  RejectionCode = "IRREVERSIBLE_MIGRATION"
	PreconditionFailed     RejectionCode = "PRECONDITION_FAILED"
	UnspecifiedModelSwitch RejectionCode = "UNSPECIFIED_MODEL_SWITCH"
	// DependencyFailed is emitted by the Hub scheduler, never accepted from an agent.
	DependencyFailed RejectionCode = "DEPENDENCY_FAILED"
)

// Admission 是 revision 判斷的結果；允收時 Reason 為空字串。
type Admission struct {
	Accepted bool
	Reason   RejectionCode
}

// Admit 判斷 incoming 是否低於已看過的最高 revision。
//
// ⚠⚠ MaxSeen 在**看到**可允收的 revision 時就前進，不等套用結果 ——
// 這一行就是這個 package 存在的理由。機器離線期間 Hub 發了 41/42/43，
// 上線後亂序送達：先套 43 失敗，再收到 41。如果只比對 MaxApplied，
// 41 會被允收、套用成功，於是機器**成功地降版而且全綠**。
// 要回舊版只能由 Hub 發一個更高 revision 的 rollback，沒有「往回走」。
func Admit(w Watermarks, incoming Revision) (Admission, Watermarks) {
	if incoming < w.MaxSeen {
		return Admission{Accepted: false, Reason: StaleRevision}, w
	}
	w.MaxSeen = incoming
	return Admission{Accepted: true}, w
}

// Applied 在套用完成後推進 MaxApplied，但絕不讓它倒退。
func Applied(w Watermarks, r Revision) Watermarks {
	if r > w.MaxApplied {
		w.MaxApplied = r
	}
	return w
}

// JobState 是 Hub 判定的工作單狀態。
type JobState string

const (
	NotStarted         JobState = "not_started"
	Claimed            JobState = "claimed"
	Running            JobState = "running"
	Verifying          JobState = "verifying"
	Succeeded          JobState = "succeeded"
	Failed             JobState = "failed"
	Rejected           JobState = "rejected"
	LeaseExpired       JobState = "lease_expired"
	ManualIntervention JobState = "manual_intervention"
)

// AllJobStates is the canonical wire/display order for exhaustive job-state
// counts. Keep one list here rather than letting the metrics, operator API and
// clients silently grow different state universes.
var AllJobStates = []JobState{
	NotStarted,
	Claimed,
	Running,
	Verifying,
	Succeeded,
	Failed,
	Rejected,
	LeaseExpired,
	ManualIntervention,
}

// IsKnownJobState reports whether a persisted or incoming value belongs to the
// state machine above. Unknown ledger values must fail a structured read rather
// than being omitted from exhaustive counts.
func IsKnownJobState(candidate JobState) bool {
	for _, state := range AllJobStates {
		if candidate == state {
			return true
		}
	}
	return false
}

// Job 是狀態機需要知道的最小事實。
//
// ⚠ Irreversible 必須跟著狀態一起傳進來，不能讓呼叫端事後自己補。
// 失敗有兩種終態，而它們對人講的話是相反的：failed 說「已經回到舊版」，
// manual_intervention 說「沒有回退，機器現在停在中間」。
// 少傳這個旗標的預設值是 false，於是一個不可逆的失敗會被說成回退過了 ——
// 那是這裡最危險的一種錯，因為它會讓人不去看那台機器。
type Job struct {
	State        JobState
	Irreversible bool
}

// Event 是 Hub 可接受的工作單狀態事件。
type Event string

const (
	Claim              Event = "claim"
	Start              Event = "start"
	FinishWork         Event = "finish_work"
	VerificationPassed Event = "verification_passed"
	VerificationFailed Event = "verification_failed"
	Reject             Event = "reject"
	LeaseLost          Event = "lease_lost"
	Timeout            Event = "timeout"
)

// Advance 依 Hub 收到的事件推進工作單；不合法的事件保留原狀。
//
// ⚠ 所有會走向失敗的轉移一律經過 OnFailure，不直接寫 Failed ——
// 這樣「不可逆的失敗不准被說成已回退」就不是呼叫端要記得的紀律，
// 而是型別上做不到的事。
func Advance(j Job, ev Event) (JobState, error) {
	from := j.State
	if IsTerminal(from) {
		return from, fmt.Errorf("終態 %q 不能再接受事件 %q", from, ev)
	}

	switch {
	case from == NotStarted && ev == Claim:
		return Claimed, nil
	case from == Claimed && ev == Start:
		return Running, nil
	case from == Running && ev == FinishWork:
		return Verifying, nil
	case from == Verifying && ev == VerificationPassed:
		return Succeeded, nil
	case from == Verifying && ev == VerificationFailed:
		return OnFailure(j.Irreversible), nil
	case (from == NotStarted || from == Claimed || from == Running || from == Verifying) && ev == Reject:
		return Rejected, nil
	case (from == Claimed || from == Running || from == Verifying) && ev == LeaseLost:
		return LeaseExpired, nil
	case (from == Claimed || from == Running || from == Verifying) && ev == Timeout:
		return OnFailure(j.Irreversible), nil
	default:
		return from, fmt.Errorf("狀態 %q 不能接受事件 %q", from, ev)
	}
}

// IsTerminal 回報工作單是否已進入不再接受事件的終態。
func IsTerminal(s JobState) bool {
	switch s {
	case Succeeded, Failed, Rejected, LeaseExpired, ManualIntervention:
		return true
	default:
		return false
	}
}

// OnFailure 依變更是否不可逆，決定失敗後能否宣告已回退。
func OnFailure(irreversible bool) JobState {
	if irreversible {
		return ManualIntervention
	}
	return Failed
}
