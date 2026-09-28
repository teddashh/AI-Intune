package compliance

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func freshness(seconds int) Rule {
	return Rule{Kind: RuleCheckinMaxAge, MaxAgeSeconds: seconds}
}

func withActions(actions ...Action) Policy {
	return Policy{SchemaVersion: SchemaVersion, Rules: []Rule{freshness(900)}, Actions: actions}
}

func TestAPolicyWithNoActionsOnlyReports(t *testing.T) {
	p := Policy{SchemaVersion: SchemaVersion, Rules: []Rule{freshness(900)}}
	if err := p.Validate(); err != nil {
		t.Fatalf("只回報的原則應該發佈得出去：%v", err)
	}
	if p.MaxGrace() != 0 {
		t.Fatalf("沒有動作就沒有要讀的證據窗：%v", p.MaxGrace())
	}
	out := DecideActions(p, VerdictNoncompliant, time.Time{}, true, time.Now())
	if len(out) != 0 {
		t.Fatalf("沒有動作的原則不該產生任何後果：%+v", out)
	}
}

func TestAnActionIsRefusedUnlessTheHubCanActuallyDoIt(t *testing.T) {
	for name, p := range map[string]Policy{
		"不認得的動作": withActions(Action{Kind: "retire_device"}),
		// 同一個後果兩個寬限期＝兩個互相矛盾的生效時刻，發佈時就得擋掉。
		"同一個後果兩個寬限期": {SchemaVersion: SchemaVersion, Rules: []Rule{freshness(900)},
			Actions: []Action{{Kind: ActionBlockJobs, GraceSeconds: 60},
				{Kind: ActionBlockJobs, GraceSeconds: 120}}},
		"寬限期是負的":  withActions(Action{Kind: ActionBlockJobs, GraceSeconds: -1}),
		"寬限期超過一天": withActions(Action{Kind: ActionBlockJobs, GraceSeconds: MaxGraceSeconds + 1}),
	} {
		t.Run(name, func(t *testing.T) {
			err := p.Validate()
			if err == nil {
				t.Fatalf("%+v 被接受了", p.Actions)
			}
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("不是一個看得懂的拒絕理由：%v", err)
			}
		})
	}
}

// 動作是原則的一部分，所以改寬限期就是改原則：digest 必須跟著變，否則「同一份
// 規則」會蓋掉一個後果完全不同的決定。
// 每一種後果只准出現一次，所以那句拒絕要指名是哪一個後果重複了，操作員才知道
// 要刪掉哪一行。
func TestARepeatedActionIsNamedInTheRefusal(t *testing.T) {
	p := Policy{SchemaVersion: SchemaVersion, Rules: []Rule{freshness(900)},
		Actions: []Action{{Kind: ActionBlockJobs, GraceSeconds: 60},
			{Kind: ActionBlockJobs, GraceSeconds: 120}}}
	err := p.Validate()
	if err == nil || !strings.Contains(err.Error(), string(ActionBlockJobs)) ||
		!strings.Contains(err.Error(), "兩次") {
		t.Fatalf("沒說清楚是哪個後果重複：%v", err)
	}
}

func TestChangingOnlyTheGracePeriodIsADifferentPolicy(t *testing.T) {
	hour, day := withActions(Action{Kind: ActionBlockJobs, GraceSeconds: 3600}),
		withActions(Action{Kind: ActionBlockJobs, GraceSeconds: 86400})
	a, err := Digest(hour)
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	b, err := Digest(day)
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	if a == b {
		t.Fatal("寬限期不同卻算出同一個 digest")
	}
	reported, err := Digest(Policy{SchemaVersion: SchemaVersion, Rules: []Rule{freshness(900)}})
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	if reported == a {
		t.Fatal("只回報與會停發工作單算出同一個 digest")
	}
}

func TestOnlyADefiniteFailureHasConsequences(t *testing.T) {
	p := withActions(Action{Kind: ActionBlockJobs})
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	// 從未報到、量不到、未指派都不是「確定沒過」，不可以被當成違規處理。
	for _, verdict := range []Verdict{VerdictCompliant, VerdictUnmeasured,
		VerdictNeverReported, VerdictNotEvaluated} {
		out := DecideActions(p, verdict, time.Time{}, false, now)
		if len(out) != 1 || out[0].State != ActionStateNotTriggered || Blocks(out) {
			t.Fatalf("%s 觸發了動作：%+v", verdict, out)
		}
	}
}

func TestTheGracePeriodIsCountedFromWhenNoncomplianceStarted(t *testing.T) {
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	p := withActions(Action{Kind: ActionBlockJobs, GraceSeconds: 3600})

	// 才壞了 59 分鐘：還在寬限裡，而且畫面要說得出什麼時候會生效。
	early := DecideActions(p, VerdictNoncompliant, now.Add(-59*time.Minute), false, now)
	if len(early) != 1 || early[0].State != ActionStateInGrace || Blocks(early) {
		t.Fatalf("寬限期內就停發了：%+v", early)
	}
	if early[0].DueAt == nil || !early[0].DueAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("生效時間算錯：%+v", early[0].DueAt)
	}
	if early[0].Since == nil || early[0].SinceIsFloor {
		t.Fatalf("寬限中必須說得出起算時刻：%+v", early[0])
	}

	// 剛好滿一小時：生效，而且不再宣稱一個未來的生效時間。
	at := DecideActions(p, VerdictNoncompliant, now.Add(-time.Hour), false, now)
	if at[0].State != ActionStateEnforced || !Blocks(at) || at[0].DueAt != nil {
		t.Fatalf("滿寬限期沒有生效：%+v", at)
	}

	// 證據窗只證得出「至少從那時起」：那也已經超過寬限期了。
	floor := DecideActions(p, VerdictNoncompliant, now.Add(-time.Hour), true, now)
	if floor[0].State != ActionStateEnforced || !floor[0].SinceIsFloor || !Blocks(floor) {
		t.Fatalf("窗外就開始壞的機器沒有生效：%+v", floor)
	}
}

func TestAZeroGracePeriodActsAsSoonAsTheVerdictSaysSo(t *testing.T) {
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	out := DecideActions(withActions(Action{Kind: ActionBlockJobs}), VerdictNoncompliant, now, false, now)
	if out[0].State != ActionStateEnforced || !Blocks(out) {
		t.Fatalf("寬限期 0 沒有立即生效：%+v", out)
	}
}

func TestEveryActionKindHasAnOrderALabelAndAnEffect(t *testing.T) {
	kinds := ActionKinds()
	if len(kinds) != len(actionOrder) {
		t.Fatalf("ActionKinds 漏了：%v", kinds)
	}
	for _, kind := range kinds {
		if ActionLabel(kind) == string(kind) || ActionEffect(kind) == "" {
			t.Fatalf("%s 沒有操作員讀得懂的說法", kind)
		}
		if err := withActions(Action{Kind: kind}).Validate(); err != nil {
			t.Fatalf("%s 發佈不出去：%v", kind, err)
		}
	}
	for _, state := range []ActionState{ActionStateNotTriggered, ActionStateInGrace, ActionStateEnforced} {
		if ActionStateLabel(state) == string(state) {
			t.Fatalf("%s 沒有中文說法", state)
		}
	}
}

// 一份原則兩個動作時，要讀的證據窗是最長的那個寬限期：讀得比它短就證明不了
// 那個最長的寬限期已經走完。
func TestTheEvidenceWindowIsTheLongestGracePeriod(t *testing.T) {
	p := withActions(Action{Kind: ActionBlockJobs, GraceSeconds: 7200})
	if p.MaxGrace() != 2*time.Hour {
		t.Fatalf("證據窗=%v", p.MaxGrace())
	}
}
