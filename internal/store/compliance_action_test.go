package store

import (
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/compliance"
	"github.com/teddashh/AI-Intune/internal/rollout"
)

func blockJobs(grace int) compliance.Action {
	return compliance.Action{Kind: compliance.ActionBlockJobs, GraceSeconds: grace}
}

func withAction(p compliance.Policy, actions ...compliance.Action) compliance.Policy {
	p.Actions = actions
	return p
}

func diskRule(percent int) compliance.Rule {
	return compliance.Rule{Kind: compliance.RuleDiskFreeMinPercent, MinFreePercent: percent}
}

// governed publishes one policy, assigns it to one machine, and hands back the
// machine so a test reads only the plane it is about.
func governed(t *testing.T, s *Store, name string, p compliance.Policy, now time.Time) string {
	t.Helper()
	id := mustEnroll(t, s, name, now)
	publishCompliance(t, s, "floor-"+name, 0, p, "p-"+name)
	assignCompliance(t, s, compliance.ScopeMachine, id, "floor-"+name, 1, "a-"+name)
	return id
}

func actionOf(t *testing.T, s *Store, machineID string) compliance.ActionOutcome {
	t.Helper()
	state, err := s.ResolveMachineCompliance(machineID)
	if err != nil {
		t.Fatalf("resolve %s: %v", machineID, err)
	}
	if len(state.Actions) != 1 {
		t.Fatalf("%s 的動作有 %d 個：%+v", machineID, len(state.Actions), state.Actions)
	}
	return state.Actions[0]
}

// 一台停止回報的機器，是在「上次回報 + 新鮮度上限」那一刻才開始不符合，不是在
// 上次回報那一刻。差的就是那個上限；用錯的那個起點會讓寬限期提早走完，機器在
// 還被容許的時間裡就領不到工作單。
func TestNoncomplianceStartsWhenTheFreshnessBoundWasCrossed(t *testing.T) {
	s := newTestStore(t)
	now := time.Date(2026, 9, 12, 8, 0, 0, 0, time.UTC)
	s.nowFn = func() time.Time { return now }

	id := governed(t, s, "quiet", withAction(rules(freshnessRule(600)), blockJobs(3600)), now)
	complianceCheckin(t, s, id, checkinFacts{version: "test", free: 90, total: 100, jobs: true},
		now.Add(-30*time.Minute))

	out := actionOf(t, s, id)
	if out.State != compliance.ActionStateInGrace {
		t.Fatalf("狀態 = %q，這台才剛過新鮮度上限：%+v", out.State, out)
	}
	if out.Since == nil || !out.Since.Equal(now.Add(-20*time.Minute)) {
		t.Fatalf("起算時刻 = %v，應該是上次回報再加 600 秒", out.Since)
	}
	if out.DueAt == nil || !out.DueAt.Equal(now.Add(40*time.Minute)) {
		t.Fatalf("生效時刻 = %v", out.DueAt)
	}
	if out.SinceIsFloor {
		t.Fatal("窗裡問得到起點，不該只說「至少從」")
	}
}

// 寬限期算的是「連續」不符合：窗裡只要有一次回報是好的，前面壞掉的那段就不
// 算數，時鐘要從那次之後重新起跑。
func TestOneGoodCheckinInsideTheWindowRestartsTheGraceClock(t *testing.T) {
	s := newTestStore(t)
	now := time.Date(2026, 9, 12, 8, 0, 0, 0, time.UTC)
	s.nowFn = func() time.Time { return now }

	id := governed(t, s, "flapping",
		withAction(rules(freshnessRule(3600), diskRule(20)), blockJobs(3600)), now)
	full := checkinFacts{version: "test", free: 2, total: 100, jobs: true}
	roomy := checkinFacts{version: "test", free: 90, total: 100, jobs: true}
	complianceCheckin(t, s, id, full, now.Add(-90*time.Minute))
	complianceCheckin(t, s, id, roomy, now.Add(-40*time.Minute))
	complianceCheckin(t, s, id, full, now.Add(-10*time.Minute))

	out := actionOf(t, s, id)
	if out.State != compliance.ActionStateInGrace {
		t.Fatalf("狀態 = %q：中間那次好的回報沒有讓時鐘重跑：%+v", out.State, out)
	}
	if out.Since == nil || !out.Since.Equal(now.Add(-10*time.Minute)) {
		t.Fatalf("起算時刻 = %v，應該是那次好回報之後的下一列", out.Since)
	}
}

// 整個證據窗都在壞，連窗外最後那一次回報也是：Hub 只證得出「至少從窗開始就在
// 壞」，而那已經比任何一個寬限期都久了。
func TestAWindowThatNeverRecoveredEnforcesAndSaysSoAsAFloor(t *testing.T) {
	s := newTestStore(t)
	now := time.Date(2026, 9, 12, 8, 0, 0, 0, time.UTC)
	s.nowFn = func() time.Time { return now }

	id := governed(t, s, "broken",
		withAction(rules(freshnessRule(3600), diskRule(20)), blockJobs(3600)), now)
	full := checkinFacts{version: "test", free: 2, total: 100, jobs: true}
	for _, ago := range []time.Duration{90, 30, 5} {
		complianceCheckin(t, s, id, full, now.Add(-ago*time.Minute))
	}

	out := actionOf(t, s, id)
	if out.State != compliance.ActionStateEnforced || !out.SinceIsFloor {
		t.Fatalf("一直壞到現在卻沒有生效：%+v", out)
	}
	if out.DueAt != nil {
		t.Fatalf("已經生效了還在講未來的生效時刻：%v", out.DueAt)
	}
	blocked, err := s.ComplianceBlocksJobs(id)
	if err != nil {
		t.Fatal(err)
	}
	if !blocked {
		t.Fatal("生效中的停發工作單沒有真的擋住領單")
	}
}

// 窗開頭那一刻管事的是窗外最後那一次回報。少讀它，一個「窗裡每一列都壞」的
// 機器就會被當成「壞了比窗還久」，寬限期被判成早就走完 —— 一台其實才壞半小時
// 的機器會立刻被停發工作單。
func TestTheReportJustBeforeTheWindowDecidesWhetherTheRunPredatesIt(t *testing.T) {
	s := newTestStore(t)
	now := time.Date(2026, 9, 12, 8, 0, 0, 0, time.UTC)
	s.nowFn = func() time.Time { return now }

	id := governed(t, s, "recent-break",
		withAction(rules(freshnessRule(3600), diskRule(20)), blockJobs(3600)), now)
	complianceCheckin(t, s, id, checkinFacts{version: "test", free: 90, total: 100, jobs: true},
		now.Add(-90*time.Minute))
	full := checkinFacts{version: "test", free: 2, total: 100, jobs: true}
	complianceCheckin(t, s, id, full, now.Add(-30*time.Minute))
	complianceCheckin(t, s, id, full, now.Add(-5*time.Minute))

	out := actionOf(t, s, id)
	if out.State != compliance.ActionStateInGrace || out.SinceIsFloor {
		t.Fatalf("窗外那次好的回報沒有被讀到：%+v", out)
	}
	if out.Since == nil || !out.Since.Equal(now.Add(-30*time.Minute)) {
		t.Fatalf("起算時刻 = %v，應該是窗裡第一次壞掉的那列", out.Since)
	}
}

// ⚠ 從未報到不是「不符合」。一台剛註冊還沒裝好的機器如果一開機就被停發工作單，
// 它永遠裝不完。量不到也一樣：沒有證據不是壞掉的證據。
func TestAMachineWithNoEvidenceIsNeverBlocked(t *testing.T) {
	s := newTestStore(t)
	now := time.Date(2026, 9, 12, 8, 0, 0, 0, time.UTC)
	s.nowFn = func() time.Time { return now }

	policy := withAction(rules(freshnessRule(3600), diskRule(20)), blockJobs(0))
	silent := governed(t, s, "silent", policy, now)
	partial := governed(t, s, "partial", policy, now)
	complianceCheckin(t, s, partial, checkinFacts{version: "test", jobs: true}, now.Add(-time.Minute))

	for id, want := range map[string]compliance.Verdict{
		silent:  compliance.VerdictNeverReported,
		partial: compliance.VerdictUnmeasured,
	} {
		state, err := s.ResolveMachineCompliance(id)
		if err != nil {
			t.Fatal(err)
		}
		if state.Verdict != want {
			t.Fatalf("%s 判成 %q，want %q", id, state.Verdict, want)
		}
		if out := state.Actions[0]; out.State != compliance.ActionStateNotTriggered {
			t.Fatalf("%s（%s）被動作碰到了：%+v", id, want, out)
		}
		blocked, err := s.ComplianceBlocksJobs(id)
		if err != nil {
			t.Fatal(err)
		}
		if blocked {
			t.Fatalf("%s（%s）被停發工作單", id, want)
		}
	}
	blockedSet, err := s.ComplianceBlockedMachines()
	if err != nil {
		t.Fatal(err)
	}
	if len(blockedSet) != 0 {
		t.Fatalf("沒有證據的機器進了停發名單：%v", blockedSet)
	}
}

// 停發工作單不是一個存下來的旗標：它跟判決一樣是現算的，所以機器一恢復就自己
// 領得到單，不需要任何人去解鎖。
func TestARecoveredMachineIsHandedWorkAgainWithoutAnyoneClearingAnything(t *testing.T) {
	s := newTestStore(t)
	now := time.Date(2026, 9, 12, 8, 0, 0, 0, time.UTC)
	s.nowFn = func() time.Time { return now }

	id := governed(t, s, "recovered", withAction(rules(freshnessRule(600)), blockJobs(0)), now)
	complianceCheckin(t, s, id, checkinFacts{version: "test", free: 90, total: 100, jobs: true},
		now.Add(-2*time.Hour))
	blocked, err := s.ComplianceBlocksJobs(id)
	if err != nil {
		t.Fatal(err)
	}
	if !blocked {
		t.Fatal("失聯兩小時還領得到工作單")
	}

	complianceCheckin(t, s, id, checkinFacts{version: "test", free: 90, total: 100, jobs: true}, now)
	blocked, err = s.ComplianceBlocksJobs(id)
	if err != nil {
		t.Fatal(err)
	}
	if blocked {
		t.Fatal("報到回來了還被擋著")
	}
	if out := actionOf(t, s, id); out.State != compliance.ActionStateNotTriggered || out.Since != nil {
		t.Fatalf("恢復後還留著違規起點：%+v", out)
	}
}

// 只回報的原則就是只回報：判決會說不符合，機隊照常運作。
func TestAReportOnlyPolicyChangesNothingAboutDispatch(t *testing.T) {
	s := newTestStore(t)
	now := time.Date(2026, 9, 12, 8, 0, 0, 0, time.UTC)
	s.nowFn = func() time.Time { return now }

	id := governed(t, s, "watched", rules(freshnessRule(600)), now)
	complianceCheckin(t, s, id, checkinFacts{version: "test", free: 90, total: 100, jobs: true},
		now.Add(-2*time.Hour))

	state, err := s.ResolveMachineCompliance(id)
	if err != nil {
		t.Fatal(err)
	}
	if state.Verdict != compliance.VerdictNoncompliant {
		t.Fatalf("判決 = %q", state.Verdict)
	}
	if len(state.Actions) != 0 {
		t.Fatalf("沒有動作的原則長出了動作：%+v", state.Actions)
	}
	blocked, err := s.ComplianceBlocksJobs(id)
	if err != nil {
		t.Fatal(err)
	}
	if blocked {
		t.Fatal("只回報的原則擋住了領單")
	}
	set, err := s.ComplianceBlockedMachines()
	if err != nil {
		t.Fatal(err)
	}
	if len(set) != 0 {
		t.Fatalf("只回報的原則產生了停發名單：%v", set)
	}
}

// 停發名單只放真的被擋住的那些，寬限中的不算 —— 寬限中的機器還在正常工作。
func TestTheBlockedListHoldsOnlyMachinesWhoseGraceHasRunOut(t *testing.T) {
	s := newTestStore(t)
	now := time.Date(2026, 9, 12, 8, 0, 0, 0, time.UTC)
	s.nowFn = func() time.Time { return now }

	policy := withAction(rules(freshnessRule(600)), blockJobs(3600))
	waiting := governed(t, s, "waiting", policy, now)
	overdue := governed(t, s, "overdue", policy, now)
	healthy := governed(t, s, "healthy", policy, now)
	ok := checkinFacts{version: "test", free: 90, total: 100, jobs: true}
	complianceCheckin(t, s, waiting, ok, now.Add(-30*time.Minute))
	complianceCheckin(t, s, overdue, ok, now.Add(-3*time.Hour))
	complianceCheckin(t, s, healthy, ok, now.Add(-time.Minute))

	set, err := s.ComplianceBlockedMachines()
	if err != nil {
		t.Fatal(err)
	}
	if len(set) != 1 || !set[overdue] {
		t.Fatalf("停發名單 = %v，只該有寬限期走完的那一台", set)
	}
	// 寬限中的機器照常領工作單 —— 寬限期的意思就是「還沒有後果」。
	for id, want := range map[string]compliance.ActionState{
		waiting: compliance.ActionStateInGrace,
		overdue: compliance.ActionStateEnforced,
		healthy: compliance.ActionStateNotTriggered,
	} {
		if got := actionOf(t, s, id).State; got != want {
			t.Errorf("%s 的動作狀態 = %q，want %q", id, got, want)
		}
		blocked, err := s.ComplianceBlocksJobs(id)
		if err != nil {
			t.Fatal(err)
		}
		if blocked != (want == compliance.ActionStateEnforced) {
			t.Errorf("%s（%s）領單被擋 = %v", id, want, blocked)
		}
	}
}

// 部署預覽要在開單之前就把被停發工作單的機器排掉。放它進批次，那張單不會有人
// 領走，操作員最後讀到的是「lease 過期」——一句不是真正原因的話。
func TestDeploymentPreviewExcludesMachinesBeingWithheldWork(t *testing.T) {
	s := newTestStore(t)
	now := time.Date(2026, 9, 12, 8, 0, 0, 0, time.UTC)
	s.nowFn = func() time.Time { return now }

	policy := withAction(rules(freshnessRule(600)), blockJobs(0))
	blocked := governed(t, s, "blocked", policy, now)
	healthy := governed(t, s, "healthy", policy, now)
	ok := checkinFacts{version: "test", free: 90, total: 100, jobs: true}
	complianceCheckin(t, s, blocked, ok, now.Add(-3*time.Hour))
	complianceCheckin(t, s, healthy, ok, now.Add(-time.Minute))

	members := []Machine{{MachineID: blocked, DisplayName: "blocked"},
		{MachineID: healthy, DisplayName: "healthy"}}
	facts, err := s.DeploymentFacts(members, now)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range facts {
		if f.Noncompliant != (f.MachineID == blocked) {
			t.Fatalf("%s 的停發旗標 = %v", f.DisplayName, f.Noncompliant)
		}
	}
	plan, err := s.PlanMachinesDeployment(members, ">=0.0.0", 5, now)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Noncompliant != 1 {
		t.Fatalf("預覽沒有把被停發的那台排掉：%s", plan.Summary())
	}
	for _, target := range plan.Targets {
		if target.MachineID == blocked && target.ExcludedReason != rollout.ExcludedNoncompliant {
			t.Fatalf("排除理由 = %q", target.ExcludedReason)
		}
	}
}
