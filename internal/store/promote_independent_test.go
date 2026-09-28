package store

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/model"
	"github.com/teddashh/AI-Intune/internal/rollout"
)

type promotionGateFixture struct {
	store      *Store
	clock      *time.Time
	jobID      string
	verifierID string
	now        time.Time
}

func newPromotionGateFixture(t *testing.T) promotionGateFixture {
	t.Helper()
	s := rolloutStore(t)
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	clock := now.Add(-time.Hour)
	s.nowFn = func() time.Time { return clock }
	registerDeployMachine(t, s, "cnode")
	registerDeployMachine(t, s, "onode")
	jobID := newJobForDeployTestOnMachine(t, s, "cnode")
	if _, err := s.DB().Exec(`UPDATE jobs SET state='succeeded',terminal_at=?,artifact_digest=? WHERE job_id=?`,
		fmtTime(now.Add(-30*time.Minute)), "sha256:"+strings.Repeat("a", 64), jobID); err != nil {
		t.Fatal(err)
	}
	verifier, _, err := s.RegisterVerifier(VerifierKindFleetPeerAgent, "onode peer", "onode", "")
	if err != nil {
		t.Fatal(err)
	}
	return promotionGateFixture{s, &clock, jobID, verifier.VerifierID, now}
}

func (f promotionGateFixture) assign(t *testing.T) {
	t.Helper()
	*f.clock = f.now.Add(-20 * time.Minute)
	assignJob(t, f.store, f.verifierID, f.jobID, "gate-assign-"+newID())
}

func (f promotionGateFixture) record(t *testing.T, verifierID, ruleID string, passed bool, digest string, at time.Time) {
	t.Helper()
	observedVersion := ""
	if ruleID == model.IndependentRuleOpenClawCurrentRelease && passed {
		observedVersion = "2026.9.2"
	}
	f.recordObserved(t, verifierID, ruleID, passed, digest, observedVersion, at)
}

func (f promotionGateFixture) recordObserved(t *testing.T, verifierID, ruleID string,
	passed bool, digest, observedVersion string, at time.Time,
) {
	t.Helper()
	*f.clock = at
	if err := f.store.RecordIndependentVerification(IndependentVerificationRequest{
		VerifierID: verifierID, JobID: f.jobID, RuleID: ruleID,
		Command: "gate-test " + ruleID, ObservedDigest: digest,
		ObservedVersion: observedVersion, Passed: passed, VerifiedAt: at,
	}); err != nil {
		t.Fatal(err)
	}
}

func (f promotionGateFixture) recordRequired(t *testing.T, passed bool, digest string, at time.Time) {
	t.Helper()
	for _, ruleID := range []string{
		model.IndependentRuleOpenClawCurrentRelease,
		model.IndependentRuleOpenClawGatewayHTTP,
		model.IndependentRuleOpenClawUnitState,
	} {
		f.record(t, f.verifierID, ruleID, passed, digest, at)
	}
}

func (f promotionGateFixture) state(t *testing.T) rollout.IndependentGateState {
	t.Helper()
	state, err := f.store.promotionIndependentGateState(f.jobID, "2026.9.2", f.now)
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func TestPromotionIndependentGateDistinguishesEveryOperatorState(t *testing.T) {
	t.Run("unassigned", func(t *testing.T) {
		f := newPromotionGateFixture(t)
		if got := f.state(t); got != rollout.IndependentGateUnassigned {
			t.Fatalf("state=%s", got)
		}
	})
	t.Run("awaiting report", func(t *testing.T) {
		f := newPromotionGateFixture(t)
		f.assign(t)
		if got := f.state(t); got != rollout.IndependentGateAwaitingReport {
			t.Fatalf("state=%s", got)
		}
	})
	t.Run("incomplete report", func(t *testing.T) {
		f := newPromotionGateFixture(t)
		f.assign(t)
		f.record(t, f.verifierID, model.IndependentRuleOpenClawGatewayHTTP, true, "", f.now.Add(-time.Minute))
		if got := f.state(t); got != rollout.IndependentGateIncompleteReport {
			t.Fatalf("state=%s", got)
		}
	})
	t.Run("passed", func(t *testing.T) {
		f := newPromotionGateFixture(t)
		f.assign(t)
		f.recordRequired(t, true, "", f.now.Add(-time.Minute))
		if got := f.state(t); got != rollout.IndependentGatePassed {
			t.Fatalf("state=%s", got)
		}
	})
	t.Run("failed", func(t *testing.T) {
		f := newPromotionGateFixture(t)
		f.assign(t)
		f.recordRequired(t, true, "", f.now.Add(-time.Minute))
		f.record(t, f.verifierID, "extra.failed", false, "", f.now.Add(-time.Minute))
		if got := f.state(t); got != rollout.IndependentGateFailed {
			t.Fatalf("state=%s", got)
		}
	})
	t.Run("digest mismatch", func(t *testing.T) {
		f := newPromotionGateFixture(t)
		f.assign(t)
		f.recordRequired(t, true, "sha256:"+strings.Repeat("b", 64), f.now.Add(-time.Minute))
		if got := f.state(t); got != rollout.IndependentGateDigestMismatch {
			t.Fatalf("state=%s", got)
		}
	})
	t.Run("release mismatch", func(t *testing.T) {
		f := newPromotionGateFixture(t)
		f.assign(t)
		at := f.now.Add(-time.Minute)
		f.recordObserved(t, f.verifierID, model.IndependentRuleOpenClawCurrentRelease,
			true, "", "2026.9.1", at)
		f.record(t, f.verifierID, model.IndependentRuleOpenClawGatewayHTTP, true, "", at)
		f.record(t, f.verifierID, model.IndependentRuleOpenClawUnitState, true, "", at)
		if got := f.state(t); got != rollout.IndependentGateReleaseMismatch {
			t.Fatalf("state=%s", got)
		}
	})
	t.Run("release unreported", func(t *testing.T) {
		f := newPromotionGateFixture(t)
		f.assign(t)
		f.recordRequired(t, true, "", f.now.Add(-time.Minute))
		if _, err := f.store.DB().Exec(`UPDATE verification_results SET observed_version=''
 WHERE job_id=? AND verifier_id=? AND rule_id=?`, f.jobID, f.verifierID,
			model.IndependentRuleOpenClawCurrentRelease); err != nil {
			t.Fatal(err)
		}
		if got := f.state(t); got != rollout.IndependentGateReleaseUnreported {
			t.Fatalf("state=%s", got)
		}
	})
	t.Run("stale", func(t *testing.T) {
		f := newPromotionGateFixture(t)
		*f.clock = f.now.Add(-50 * time.Minute)
		assignJob(t, f.store, f.verifierID, f.jobID, "gate-stale-"+newID())
		staleAt := f.now.Add(-40 * time.Minute)
		f.recordRequired(t, true, "", staleAt)
		if got := f.state(t); got != rollout.IndependentGateStale {
			t.Fatalf("state=%s", got)
		}
	})
	t.Run("producer revoked", func(t *testing.T) {
		f := newPromotionGateFixture(t)
		f.assign(t)
		f.recordRequired(t, true, "", f.now.Add(-time.Minute))
		if _, err := f.store.DB().Exec(`UPDATE verifiers SET revoked_at=? WHERE verifier_id=?`,
			fmtTime(f.now), f.verifierID); err != nil {
			t.Fatal(err)
		}
		if got := f.state(t); got != rollout.IndependentGateProducerRevoked {
			t.Fatalf("state=%s", got)
		}
	})
}

// Digest 與 release 衝突同時成立時，優先回報哪一個會決定操作員被叫去
// 「確認 artifact」還是「修復」；這個優先序不是風格問題。
func TestDigestMismatchOutranksReleaseMismatchWhenBothAreTrue(t *testing.T) {
	f := newPromotionGateFixture(t)
	f.assign(t)
	at := f.now.Add(-time.Minute)
	// ObservedDigest 送 sha256: 後接 64 個 b；工作單 artifact_digest 是 sha256: 後接 64 個 a，造成 digest 衝突。
	// ObservedVersion 送 2026.9.1；gate 期望版本是 2026.9.2，造成 release 衝突。
	f.recordObserved(t, f.verifierID, model.IndependentRuleOpenClawCurrentRelease,
		true, "sha256:"+strings.Repeat("b", 64), "2026.9.1", at)
	f.record(t, f.verifierID, model.IndependentRuleOpenClawGatewayHTTP, true, "", at)
	f.record(t, f.verifierID, model.IndependentRuleOpenClawUnitState, true, "", at)

	if got := f.state(t); got != rollout.IndependentGateDigestMismatch {
		t.Errorf("digest 與 release 同時衝突時 gate=%s，預期 %s：報成 release 衝突會把操作員叫去修復，而不是去確認 artifact",
			got, rollout.IndependentGateDigestMismatch)
	}
}

// assign 在前、report 分別於 terminal_at 之前與同秒到時，失敗列都還不能描述終態；
// 這兩列釘住操作員應被指引去重新指派 verifier 量終態，而不是去修復後重跑 canary。
func TestFailedReportAtOrBeforeTerminalRemainsStale(t *testing.T) {
	tests := []struct {
		name   string
		offset time.Duration
	}{
		{name: "before terminal", offset: -10 * time.Minute}, // 報告早於 terminal_at 十分鐘，仍不能描述終態。
		{name: "same second", offset: 0},                     // 報告與 terminal_at 同秒，邊界上仍不能描述終態。
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			f := newPromotionGateFixture(t)
			terminalAt := f.now.Add(-30 * time.Minute)   // fixture 將工作單 terminal_at 設在基準時間前三十分鐘。
			*f.clock = terminalAt.Add(-20 * time.Minute) // 派工比最早的報告再早十分鐘，確保兩列報告都在派工後。
			assignJob(t, f.store, f.verifierID, f.jobID, "gate-failed-stale-"+newID())
			f.recordRequired(t, false, "", terminalAt.Add(test.offset))

			if got := f.state(t); got != rollout.IndependentGateStale {
				t.Errorf("列 %q：實際 gate=%s，預期 gate=%s：報成 Failed 會把操作員叫去修復後重跑 canary，但這些失敗列的 received_at 都不晚於 terminal_at、還不能描述終態；操作員該做的是重新指派 verifier 去量終態",
					test.name, got, rollout.IndependentGateStale)
			}
		})
	}
}

// 終態前與終態同秒的失敗列都不能描述終態；操作員依 Stale 重新指派 verifier 後，完整的新鮮通過報告都應讓 gate 通過。
// 每列的三拍時鐘依序是終態前派工、終態前或同秒失敗、終態後重新派工與通過報告。
func TestReassignmentAfterFailureAtOrBeforeTerminalCanPassWithFreshReport(t *testing.T) {
	tests := []struct {
		name   string
		offset time.Duration
	}{
		{name: "before terminal", offset: -10 * time.Minute}, // 失敗報告早於 terminal_at 十分鐘，還不能描述終態。
		{name: "same second", offset: 0},                     // 失敗報告與 terminal_at 同秒，邊界上仍不能描述終態。
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			f := newPromotionGateFixture(t)
			terminalAt := f.now.Add(-30 * time.Minute)   // fixture 將工作單 terminal_at 設在基準時間前三十分鐘。
			*f.clock = terminalAt.Add(-20 * time.Minute) // 首次派工早於 terminal_at 二十分鐘，並早於兩列的失敗報告。
			assignJob(t, f.store, f.verifierID, f.jobID, "gate-stale-reassign-old-"+newID())
			f.recordRequired(t, false, "", terminalAt.Add(test.offset))
			*f.clock = terminalAt.Add(10 * time.Minute) // 重新派工晚於 terminal_at 十分鐘，模擬操作員依 Stale 行動。
			assignJob(t, f.store, f.verifierID, f.jobID, "gate-stale-reassign-new-"+newID())
			f.recordRequired(t, true, "", terminalAt.Add(29*time.Minute)) // 通過報告晚於 terminal_at 二十九分鐘，送齊重新指派後的三條規則。

			if got := f.state(t); got != rollout.IndependentGatePassed {
				t.Errorf("列 %q：實際 gate=%s，預期 gate=%s：終態前或同秒的失敗列不能描述終態；操作員照 Stale 的下一步重新指派 verifier、又送到終態後的通過報告，卻被舊失敗列鎖成 Failed，畫面會叫他修復後重跑 canary，而重新指派永遠洗不掉",
					test.name, got, rollout.IndependentGatePassed)
			}
		})
	}
}

func TestPromotionIndependentGateCannotBeUnlockedByIncompleteOrUnassignedRows(t *testing.T) {
	t.Run("unassigned producer", func(t *testing.T) {
		f := newPromotionGateFixture(t)
		f.recordRequired(t, true, "", f.now.Add(-time.Minute))
		if got := f.state(t); got != rollout.IndependentGateUnassigned {
			t.Fatalf("state=%s", got)
		}
	})
	t.Run("duplicate rule", func(t *testing.T) {
		f := newPromotionGateFixture(t)
		f.assign(t)
		for range model.IndependentFleetPeerRequiredRules {
			f.record(t, f.verifierID, model.IndependentRuleOpenClawGatewayHTTP, true, "", f.now.Add(-time.Minute))
		}
		if got := f.state(t); got != rollout.IndependentGateIncompleteReport {
			t.Fatalf("state=%s", got)
		}
	})
	t.Run("new assignment resets report", func(t *testing.T) {
		f := newPromotionGateFixture(t)
		f.assign(t)
		f.recordRequired(t, true, "", f.now.Add(-10*time.Minute))
		*f.clock = f.now.Add(-5 * time.Minute)
		assignJob(t, f.store, f.verifierID, f.jobID, "gate-recheck-"+newID())
		if got := f.state(t); got != rollout.IndependentGateAwaitingReport {
			t.Fatalf("state=%s", got)
		}
	})
	t.Run("pre-assignment report stays ineligible", func(t *testing.T) {
		f := newPromotionGateFixture(t)
		f.recordRequired(t, true, "sha256:"+strings.Repeat("b", 64), f.now.Add(-20*time.Minute))
		*f.clock = f.now.Add(-5 * time.Minute)
		assignJob(t, f.store, f.verifierID, f.jobID, "gate-after-evidence-"+newID())
		if got := f.state(t); got != rollout.IndependentGateAwaitingReport {
			t.Fatalf("state=%s", got)
		}
	})
	t.Run("old complete report plus one new rule is still incomplete", func(t *testing.T) {
		f := newPromotionGateFixture(t)
		f.assign(t)
		f.recordRequired(t, true, "", f.now.Add(-10*time.Minute))
		*f.clock = f.now.Add(-5 * time.Minute)
		assignJob(t, f.store, f.verifierID, f.jobID, "gate-partial-recheck-"+newID())
		f.record(t, f.verifierID, model.IndependentRuleOpenClawGatewayHTTP, true, "", f.now.Add(-time.Minute))
		if got := f.state(t); got != rollout.IndependentGateIncompleteReport {
			t.Fatalf("state=%s", got)
		}
	})
	t.Run("new assignment cannot erase failure", func(t *testing.T) {
		f := newPromotionGateFixture(t)
		f.assign(t)
		f.recordRequired(t, false, "", f.now.Add(-10*time.Minute))
		*f.clock = f.now.Add(-5 * time.Minute)
		assignJob(t, f.store, f.verifierID, f.jobID, "gate-failed-recheck-"+newID())
		if got := f.state(t); got != rollout.IndependentGateFailed {
			t.Fatalf("state=%s", got)
		}
	})
	t.Run("future report", func(t *testing.T) {
		f := newPromotionGateFixture(t)
		f.assign(t)
		f.recordRequired(t, true, "", f.now.Add(time.Minute))
		if got := f.state(t); got != rollout.IndependentGateAwaitingReport {
			t.Fatalf("state=%s", got)
		}
	})
	t.Run("non fleet peer producer", func(t *testing.T) {
		f := newPromotionGateFixture(t)
		verifier, _, err := f.store.RegisterVerifier(VerifierKindExternalJobRunner,
			"external verifier", "external-domain", "")
		if err != nil {
			t.Fatal(err)
		}
		*f.clock = f.now.Add(-20 * time.Minute)
		assignJob(t, f.store, verifier.VerifierID, f.jobID, "gate-external-"+newID())
		for _, ruleID := range []string{
			model.IndependentRuleOpenClawCurrentRelease,
			model.IndependentRuleOpenClawGatewayHTTP,
			model.IndependentRuleOpenClawUnitState,
		} {
			f.record(t, verifier.VerifierID, ruleID, true, "", f.now.Add(-time.Minute))
		}
		if got := f.state(t); got != rollout.IndependentGateUnassigned {
			t.Fatalf("state=%s", got)
		}
	})
	t.Run("partial reports from different producers do not combine", func(t *testing.T) {
		f := newPromotionGateFixture(t)
		registerDeployMachine(t, f.store, "pnode")
		second, _, err := f.store.RegisterVerifier(VerifierKindFleetPeerAgent, "pnode peer", "pnode", "")
		if err != nil {
			t.Fatal(err)
		}
		f.assign(t)
		*f.clock = f.now.Add(-20 * time.Minute)
		assignJob(t, f.store, second.VerifierID, f.jobID, "gate-second-"+newID())
		f.record(t, f.verifierID, model.IndependentRuleOpenClawCurrentRelease, true, "", f.now.Add(-time.Minute))
		f.record(t, f.verifierID, model.IndependentRuleOpenClawGatewayHTTP, true, "", f.now.Add(-time.Minute))
		f.record(t, second.VerifierID, model.IndependentRuleOpenClawUnitState, true, "", f.now.Add(-time.Minute))
		if got := f.state(t); got != rollout.IndependentGateIncompleteReport {
			t.Fatalf("state=%s", got)
		}
	})
}

func TestPromotionIndependentGateAllowsCanonicalFutureAssignment(t *testing.T) {
	f := newPromotionGateFixture(t)
	f.assign(t)
	f.recordRequired(t, true, "", f.now.Add(-time.Minute))
	*f.clock = f.now.Add(time.Hour)
	assignJob(t, f.store, f.verifierID, f.jobID, "gate-future-"+newID())

	state, err := f.store.promotionIndependentGateState(f.jobID, "2026.9.2", f.now)
	if err != nil || state != rollout.IndependentGatePassed {
		t.Fatalf("state=%s err=%v, want %s", state, err, rollout.IndependentGatePassed)
	}
}

func TestPromotionIndependentGateRejectsNoncanonicalAssignmentOrdering(t *testing.T) {
	f := newPromotionGateFixture(t)
	const (
		assignmentA = "2026-09-13T11:59:00Z"
		assignmentB = "2026-09-13T10:59:30-01:00"
	)
	if _, err := f.store.DB().Exec(`INSERT INTO verification_assignments
 (assignment_id,job_id,verifier_id,assigned_at,assigned_by) VALUES (?,?,?,?,?)`,
		newID(), f.jobID, f.verifierID, assignmentA, "gate-order-a"); err != nil {
		t.Fatal(err)
	}
	f.recordRequired(t, true, "", time.Date(2026, 9, 13, 11, 59, 15, 0, time.UTC))
	if _, err := f.store.DB().Exec(`INSERT INTO verification_assignments
 (assignment_id,job_id,verifier_id,assigned_at,assigned_by) VALUES (?,?,?,?,?)`,
		newID(), f.jobID, f.verifierID, assignmentB, "gate-order-b"); err != nil {
		t.Fatal(err)
	}

	state, err := f.store.promotionIndependentGateState(f.jobID, "2026.9.2", f.now)
	if err == nil {
		t.Fatalf("state=%s err=%v, want integrity error", state, err)
	}
	if !strings.Contains(err.Error(), "assignment assigned_at is not canonical") {
		t.Fatalf("err=%v, want assignment integrity error", err)
	}
}

func TestPromotionIndependentGateRejectsNoncanonicalAssignmentOutsideTextWindow(t *testing.T) {
	f := newPromotionGateFixture(t)
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	const (
		assignmentA = "2026-09-15T10:00:00Z"
		assignmentB = "2026-09-16T00:00:00+13:00"
	)
	if _, err := f.store.DB().Exec(`INSERT INTO verification_assignments
 (assignment_id,job_id,verifier_id,assigned_at,assigned_by) VALUES (?,?,?,?,?)`,
		newID(), f.jobID, f.verifierID, assignmentA, "gate-window-a"); err != nil {
		t.Fatal(err)
	}
	f.recordRequired(t, true, "", time.Date(2026, 9, 15, 10, 30, 0, 0, time.UTC))
	if _, err := f.store.DB().Exec(`INSERT INTO verification_assignments
 (assignment_id,job_id,verifier_id,assigned_at,assigned_by) VALUES (?,?,?,?,?)`,
		newID(), f.jobID, f.verifierID, assignmentB, "gate-window-b"); err != nil {
		t.Fatal(err)
	}

	state, err := f.store.promotionIndependentGateState(f.jobID, "2026.9.2", now)
	if err == nil {
		t.Fatalf("state=%s err=%v, want integrity error", state, err)
	}
	if !strings.Contains(err.Error(), "assignment assigned_at is not canonical") {
		t.Fatalf("err=%v, want assignment integrity error", err)
	}
}

func TestPromotionIndependentGateRejectsNoncanonicalComparisonTimes(t *testing.T) {
	tests := []struct {
		name    string
		corrupt func(*testing.T, promotionGateFixture)
		want    string
	}{
		{
			name: "independent result received_at",
			corrupt: func(t *testing.T, f promotionGateFixture) {
				if _, err := f.store.DB().Exec(`UPDATE verification_results SET received_at=?
 WHERE job_id=? AND evidence_role=? AND rule_id=?`, "2026-09-13T10:59:15-01:00",
					f.jobID, JobVerificationRoleIndependent, model.IndependentRuleOpenClawGatewayHTTP); err != nil {
					t.Fatal(err)
				}
			},
			want: "independent result received_at is not canonical",
		},
		{
			name: "job terminal_at",
			corrupt: func(t *testing.T, f promotionGateFixture) {
				if _, err := f.store.DB().Exec(`UPDATE jobs SET terminal_at=? WHERE job_id=?`,
					"2026-09-13T10:30:00-01:00", f.jobID); err != nil {
					t.Fatal(err)
				}
			},
			want: "job terminal_at is not canonical",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			f := newPromotionGateFixture(t)
			f.assign(t)
			f.recordRequired(t, true, "", f.now.Add(-time.Minute))
			test.corrupt(t, f)

			state, err := f.store.promotionIndependentGateState(f.jobID, "2026.9.2", f.now)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("state=%s err=%v, want %q integrity error", state, err, test.want)
			}
		})
	}
}

func TestPromotionIndependentGateCanonicalTimestampBoundaries(t *testing.T) {
	t.Run("received in assignment second is current", func(t *testing.T) {
		f := newPromotionGateFixture(t)
		at := f.now.Add(-time.Minute)
		*f.clock = at
		assignJob(t, f.store, f.verifierID, f.jobID, "gate-same-second-"+newID())
		f.recordRequired(t, true, "", at)
		if got := f.state(t); got != rollout.IndependentGatePassed {
			t.Fatalf("state=%s", got)
		}
	})

	t.Run("empty independent received_at stays ineligible", func(t *testing.T) {
		f := newPromotionGateFixture(t)
		f.assign(t)
		f.recordRequired(t, true, "", f.now.Add(-time.Minute))
		if _, err := f.store.DB().Exec(`UPDATE verification_results SET received_at=''
 WHERE job_id=? AND evidence_role=?`, f.jobID, JobVerificationRoleIndependent); err != nil {
			t.Fatal(err)
		}
		state, err := f.store.promotionIndependentGateState(f.jobID, "2026.9.2", f.now)
		if err != nil || state != rollout.IndependentGateAwaitingReport {
			t.Fatalf("state=%s err=%v", state, err)
		}
	})
}

func TestStablePromotionKeepsEligiblePreReassignmentClashesLocked(t *testing.T) {
	tests := []struct {
		name      string
		want      rollout.IndependentGateState
		makeClash func(*testing.T, promotionGateFixture)
	}{
		{
			name: "digest",
			want: rollout.IndependentGateDigestMismatch,
			makeClash: func(t *testing.T, f promotionGateFixture) {
				if _, err := f.store.DB().Exec(`UPDATE verification_results
 SET observed_digest=?
 WHERE job_id=? AND verifier_id=? AND rule_id=?`,
					"sha256:"+strings.Repeat("b", 64), f.jobID, f.verifierID,
					model.IndependentRuleOpenClawGatewayHTTP); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "release",
			want: rollout.IndependentGateReleaseMismatch,
			makeClash: func(t *testing.T, f promotionGateFixture) {
				if _, err := f.store.DB().Exec(`UPDATE verification_results
 SET observed_version='2026.9.1'
 WHERE job_id=? AND verifier_id=? AND rule_id=?`, f.jobID, f.verifierID,
					model.IndependentRuleOpenClawCurrentRelease); err != nil {
					t.Fatal(err)
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s := rolloutStore(t)
			n, _ := eligiblePromotionFixture(t, s)
			var jobID, verifierID string
			if err := s.DB().QueryRow(`SELECT job_id,verifier_id
 FROM verification_results WHERE evidence_role=? LIMIT 1`,
				JobVerificationRoleIndependent).Scan(&jobID, &verifierID); err != nil {
				t.Fatal(err)
			}
			f := promotionGateFixture{store: s, jobID: jobID, verifierID: verifierID, now: s.now()}
			test.makeClash(t, f)

			assignedAt := f.now.Add(-2 * time.Minute)
			s.nowFn = func() time.Time { return assignedAt }
			assignJob(t, s, verifierID, jobID, "gate-reassigned-"+newID())
			currentAt := f.now.Add(-time.Minute)
			f.clock = &currentAt
			f.recordRequired(t, true, "", currentAt)
			s.nowFn = func() time.Time { return f.now }

			decision, err := s.PreviewStableOpenClawPromotion("2026.9.2", n.Job.ArtifactDigest, s.now())
			if err != nil || decision.Allowed || len(decision.IndependentTargets) != 1 ||
				decision.IndependentTargets[0].State != test.want {
				t.Fatalf("preview decision=%+v err=%v, want denied with %s", decision, err, test.want)
			}
			before := deploymentWriteCounts(t, s)
			_, jobs, applied, err := s.CreateStableOpenClawDeployment(n)
			if !errors.Is(err, ErrPromoteLocked) || applied.Allowed || len(jobs) != 0 ||
				len(applied.IndependentTargets) != 1 || applied.IndependentTargets[0].State != test.want {
				t.Fatalf("create jobs=%+v decision=%+v err=%v, want denied with %s",
					jobs, applied, err, test.want)
			}
			assertDeploymentWriteCounts(t, s, before)
		})
	}
}

func TestPromotionIndependentGateReceivedAtTerminalSecondBoundary(t *testing.T) {
	tests := []struct {
		name   string
		offset time.Duration
		want   rollout.IndependentGateState
	}{
		{name: "same second", offset: 0, want: rollout.IndependentGateStale},
		{name: "one second later", offset: time.Second, want: rollout.IndependentGatePassed},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			f := newPromotionGateFixture(t)
			terminalAt := f.now.Add(-30 * time.Minute)
			*f.clock = terminalAt.Add(-time.Minute)
			assignJob(t, f.store, f.verifierID, f.jobID, "gate-terminal-boundary-"+newID())
			f.recordRequired(t, true, "", terminalAt.Add(test.offset))
			if got := f.state(t); got != test.want {
				t.Fatalf("state=%s, want %s", got, test.want)
			}
		})
	}
}

func TestStableCreateCannotCrossAnUnassignedIndependentGate(t *testing.T) {
	s := rolloutStore(t)
	n, _ := eligiblePromotionFixture(t, s)
	if _, err := s.DB().Exec(`DELETE FROM verification_results WHERE evidence_role=?`,
		JobVerificationRoleIndependent); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(`DELETE FROM verification_assignments`); err != nil {
		t.Fatal(err)
	}
	decision, err := s.PreviewStableOpenClawPromotion("2026.9.2", n.Job.ArtifactDigest, s.now())
	if err != nil || decision.Allowed || !strings.Contains(decision.Summary(), "尚未指派跨故障域 verifier") {
		t.Fatalf("preview decision=%+v err=%v", decision, err)
	}
	before := deploymentWriteCounts(t, s)
	_, jobs, applied, err := s.createStableOpenClawDeployment(n, time.UTC)
	if !errors.Is(err, ErrPromoteLocked) || applied.Allowed || len(jobs) != 0 {
		t.Fatalf("create jobs=%+v decision=%+v err=%v", jobs, applied, err)
	}
	assertDeploymentWriteCounts(t, s, before)
}

func TestStableCreateCannotCrossAnIndependentReleaseMismatch(t *testing.T) {
	s := rolloutStore(t)
	n, _ := eligiblePromotionFixture(t, s)
	if _, err := s.DB().Exec(`UPDATE verification_results SET observed_version='2026.9.1'
 WHERE rule_id=? AND evidence_role=?`, model.IndependentRuleOpenClawCurrentRelease,
		JobVerificationRoleIndependent); err != nil {
		t.Fatal(err)
	}
	decision, err := s.PreviewStableOpenClawPromotion("2026.9.2", n.Job.ArtifactDigest, s.now())
	if err != nil || decision.Allowed ||
		!strings.Contains(decision.Summary(), "verifier 看到另一個 OpenClaw 版本") {
		t.Fatalf("preview decision=%+v err=%v", decision, err)
	}
	before := deploymentWriteCounts(t, s)
	_, jobs, applied, err := s.createStableOpenClawDeployment(n, time.UTC)
	if !errors.Is(err, ErrPromoteLocked) || applied.Allowed || len(jobs) != 0 {
		t.Fatalf("create jobs=%+v decision=%+v err=%v", jobs, applied, err)
	}
	assertDeploymentWriteCounts(t, s, before)
}
