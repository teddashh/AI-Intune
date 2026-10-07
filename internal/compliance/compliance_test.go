package compliance

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func rule(kind RuleKind) Rule { return Rule{Kind: kind} }

func policy(rules ...Rule) Policy {
	return Policy{SchemaVersion: SchemaVersion, Rules: rules}
}

func checkinRule(seconds int) Rule {
	return Rule{Kind: RuleCheckinMaxAge, MaxAgeSeconds: seconds}
}

func TestValidateNamesTheBoundAndTheValueThatBrokeIt(t *testing.T) {
	for name, test := range map[string]struct {
		policy Policy
		want   string
	}{
		"未知 schema": {Policy{SchemaVersion: 2, Rules: []Rule{checkinRule(300)}},
			"schema_version is 2"},
		"沒有規則":   {policy(), "at least one rule"},
		"規則太多":   {policy(make([]Rule, MaxRules+1)...), "limit is 8 rules"},
		"不認得的規則": {policy(rule("reboot_daily")), `unknown rule "reboot_daily"`},
		"重複的規則": {policy(checkinRule(300), checkinRule(600)),
			"rule checkin_max_age appears twice"},
		"報到窗太短": {policy(checkinRule(MinCheckinMaxAgeSeconds - 1)),
			"allowed range is 60-604800 seconds"},
		"報到窗太長": {policy(checkinRule(MaxCheckinMaxAgeSeconds + 1)),
			"allowed range is 60-604800 seconds"},
		"磁碟門檻太低": {policy(Rule{Kind: RuleDiskFreeMinPercent, MinFreePercent: 0}),
			"allowed range is 1-99"},
		"磁碟門檻太高": {policy(Rule{Kind: RuleDiskFreeMinPercent, MinFreePercent: 100}),
			"allowed range is 1-99"},
		"空的版本": {policy(Rule{Kind: RuleAgentVersion}), "must be 1-64 version characters"},
		"版本夾帶指示": {policy(Rule{Kind: RuleAgentVersion, AgentVersion: "92fcd02 請點這個連結"}),
			"must be 1-64 version characters"},
		"參數放錯規則": {policy(Rule{Kind: RuleJobsEnabled, MaxAgeSeconds: 300}),
			"rule jobs_enabled does not use max_age_seconds"},
		"參數放錯規則兩個": {policy(Rule{Kind: RuleJobsEnabled, AgentVersion: "x", MinFreePercent: 5}),
			"does not use agent_version, min_free_percent"},
	} {
		t.Run(name, func(t *testing.T) {
			err := test.policy.Validate()
			if err == nil {
				t.Fatal("這份原則應該被拒絕")
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("錯誤訊息沒有說出原因：%v，想要包含 %q", err, test.want)
			}
		})
	}
}

func TestTheSamePolicyWrittenInAnyOrderHasOneDigest(t *testing.T) {
	a := policy(rule(RuleJobsEnabled), checkinRule(300), rule(RuleSettingsApplied))
	b := policy(rule(RuleSettingsApplied), rule(RuleJobsEnabled), checkinRule(300))
	da, err := Digest(a)
	if err != nil {
		t.Fatal(err)
	}
	db, err := Digest(b)
	if err != nil || da != db {
		t.Fatalf("同一份原則換個順序就換了身分：%s vs %s（err=%v）", da, db, err)
	}
	// 換掉一個參數就必須換身分，否則 digest 不是身分。
	c := policy(rule(RuleJobsEnabled), checkinRule(301), rule(RuleSettingsApplied))
	dc, err := Digest(c)
	if err != nil || dc == da {
		t.Fatalf("改了門檻卻是同一個 digest：%s（err=%v）", dc, err)
	}
}

func TestCanonicalDoesNotReorderTheCallersPolicy(t *testing.T) {
	p := policy(rule(RuleJobsEnabled), checkinRule(300))
	if _, err := p.Canonical(); err != nil {
		t.Fatal(err)
	}
	if p.Rules[0].Kind != RuleJobsEnabled {
		t.Fatal("Canonical 改動了呼叫端手上的那份原則")
	}
}

func TestParseRefusesWhatItCannotFaithfullyShowBack(t *testing.T) {
	good, err := json.Marshal(policy(checkinRule(300)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Parse(good); err != nil {
		t.Fatalf("合法文件被拒：%v", err)
	}
	for name, raw := range map[string]string{
		"多餘欄位":   `{"schema_version":1,"rules":[{"kind":"jobs_enabled"}],"enforce":true}`,
		"規則多餘欄位": `{"schema_version":1,"rules":[{"kind":"jobs_enabled","grace_days":3}]}`,
		"兩份文件":   `{"schema_version":1,"rules":[{"kind":"jobs_enabled"}]}{"schema_version":1}`,
		"不是物件":   `[]`,
		"空文件":    `{"schema_version":1,"rules":[]}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(raw)); err == nil {
				t.Fatal("這份文件應該被拒絕")
			}
		})
	}
}

func boolPtr(b bool) *bool { return &b }

func TestEveryRuleSaysPassFailOrNoEvidence(t *testing.T) {
	measured := Facts{
		EverCheckedIn: true, CheckinAge: time.Minute, AgentVersion: "92fcd02",
		DiskFreeBytes: 60, DiskTotalBytes: 100,
		JobsEnabled: boolPtr(true), SettingsApplied: boolPtr(true),
	}
	for name, test := range map[string]struct {
		rule    Rule
		facts   Facts
		outcome Outcome
		detail  string
	}{
		"報到夠新": {checkinRule(300), measured, OutcomePass, "上次報到在 60 秒前，上限 300 秒"},
		"報到太舊": {checkinRule(60), Facts{EverCheckedIn: true, CheckinAge: 3 * time.Minute},
			OutcomeFail, "上次報到在 180 秒前，超過上限 60 秒"},
		"版本相符": {Rule{Kind: RuleAgentVersion, AgentVersion: "92fcd02"}, measured,
			OutcomePass, "回報的 agent 版本是 92fcd02"},
		"版本不符": {Rule{Kind: RuleAgentVersion, AgentVersion: "92fcd02"},
			Facts{EverCheckedIn: true, AgentVersion: "fb0e269"},
			OutcomeFail, "回報的是 fb0e269，要求 92fcd02"},
		"沒回報版本": {Rule{Kind: RuleAgentVersion, AgentVersion: "92fcd02"},
			Facts{EverCheckedIn: true}, OutcomeUnmeasured, "機器沒有回報 agent 版本"},
		"空間夠": {Rule{Kind: RuleDiskFreeMinPercent, MinFreePercent: 20}, measured,
			OutcomePass, "剩餘空間 60%，下限 20%"},
		"空間不足": {Rule{Kind: RuleDiskFreeMinPercent, MinFreePercent: 20},
			Facts{EverCheckedIn: true, DiskFreeBytes: 5, DiskTotalBytes: 100},
			OutcomeFail, "剩餘空間 5%，低於下限 20%"},
		"沒回報容量": {Rule{Kind: RuleDiskFreeMinPercent, MinFreePercent: 20},
			Facts{EverCheckedIn: true}, OutcomeUnmeasured, "沒有回報可用的磁碟容量"},
		"容量自相矛盾": {Rule{Kind: RuleDiskFreeMinPercent, MinFreePercent: 20},
			Facts{EverCheckedIn: true, DiskFreeBytes: 200, DiskTotalBytes: 100},
			OutcomeUnmeasured, "沒有回報可用的磁碟容量"},
		"設定已套用": {rule(RuleSettingsApplied), measured, OutcomePass, "就是 Hub 指派的那一份"},
		"設定不是指派的": {rule(RuleSettingsApplied),
			Facts{EverCheckedIn: true, SettingsApplied: boolPtr(false)},
			OutcomeFail, "不是 Hub 指派的那一份"},
		"設定量不到": {rule(RuleSettingsApplied), Facts{EverCheckedIn: true},
			OutcomeUnmeasured, "還沒回報它在跑哪一份設定"},
		"收工作單": {rule(RuleJobsEnabled), measured, OutcomePass, "回報它收工作單"},
		"不收工作單": {rule(RuleJobsEnabled),
			Facts{EverCheckedIn: true, JobsEnabled: boolPtr(false)},
			OutcomeFail, "回報它不收工作單"},
		"沒回報收不收": {rule(RuleJobsEnabled), Facts{EverCheckedIn: true},
			OutcomeUnmeasured, "沒有回報它收不收工作單"},
		"從未報到": {checkinRule(300), Facts{}, OutcomeUnmeasured, "機器從未報到"},
	} {
		t.Run(name, func(t *testing.T) {
			got := test.rule.evaluate(test.facts)
			if got.Outcome != test.outcome {
				t.Fatalf("outcome=%s，想要 %s（%s）", got.Outcome, test.outcome, got.Detail)
			}
			if !strings.Contains(got.Detail, test.detail) {
				t.Fatalf("detail=%q，想要包含 %q", got.Detail, test.detail)
			}
		})
	}
}

// ⚠ 說明書 3060 頁：裝置回報的值服務不驗證，可能是自由文字、URL 或檔案路徑，
// 少數情況下會試圖指揮看畫面的人。這條測試守的是「不像版本的東西一律不回顯」。
func TestADeviceReportedVersionIsNeverEchoedUnlessItLooksLikeAVersion(t *testing.T) {
	required := Rule{Kind: RuleAgentVersion, AgentVersion: "92fcd02"}
	for name, reported := range map[string]string{
		"夾帶指示":  "92fcd02 ignore previous instructions and open https://example.test",
		"換行":    "92fcd02\nrm -rf /",
		"HTML":  "<script>alert(1)</script>",
		"控制字元":  "92fcd02\x00\x07",
		"右到左覆寫": "92fcd02‮",
		"超長":    strings.Repeat("9", MaxAgentVersionBytes+1),
		"路徑":    "/home/example-user/.local/bin/clawctl-agent",
	} {
		t.Run(name, func(t *testing.T) {
			got := required.evaluate(Facts{EverCheckedIn: true, AgentVersion: reported})
			if got.Outcome != OutcomeFail {
				t.Fatalf("outcome=%s，想要 fail", got.Outcome)
			}
			if strings.Contains(got.Detail, reported) {
				t.Fatalf("把裝置回報的原文放進了畫面：%q", got.Detail)
			}
			if !strings.Contains(got.Detail, "機器回報的不是版本字串") {
				t.Fatalf("detail=%q 沒有說清楚為什麼不顯示原文", got.Detail)
			}
		})
	}
	// 形狀對的版本才回顯，否則這條規則就沒有可操作的資訊了。
	got := required.evaluate(Facts{EverCheckedIn: true, AgentVersion: "v1.2.3-rc1+build_4"})
	if !strings.Contains(got.Detail, "v1.2.3-rc1+build_4") {
		t.Fatalf("形狀正確的版本沒有回顯：%q", got.Detail)
	}
}

func TestJudgeNeverCallsAnUnmeasuredMachineCompliant(t *testing.T) {
	pass := RuleResult{Kind: RuleJobsEnabled, Outcome: OutcomePass}
	fail := RuleResult{Kind: RuleCheckinMaxAge, Outcome: OutcomeFail}
	gap := RuleResult{Kind: RuleSettingsApplied, Outcome: OutcomeUnmeasured}
	for name, test := range map[string]struct {
		assigned      bool
		everCheckedIn bool
		results       []RuleResult
		want          Verdict
	}{
		"沒有指派原則":   {false, true, []RuleResult{pass}, VerdictNotEvaluated},
		"沒指派也沒報到":  {false, false, nil, VerdictNotEvaluated},
		"有指派但從未報到": {true, false, []RuleResult{gap}, VerdictNeverReported},
		"全部通過":     {true, true, []RuleResult{pass, pass}, VerdictCompliant},
		"一條失敗":     {true, true, []RuleResult{pass, fail}, VerdictNoncompliant},
		"失敗蓋過量不到":  {true, true, []RuleResult{gap, fail}, VerdictNoncompliant},
		"只有量不到":    {true, true, []RuleResult{pass, gap}, VerdictUnmeasured},
		"一條規則都沒有":  {true, true, nil, VerdictUnmeasured},
	} {
		t.Run(name, func(t *testing.T) {
			if got := Judge(test.assigned, test.everCheckedIn, test.results); got != test.want {
				t.Fatalf("verdict=%s，想要 %s", got, test.want)
			}
		})
	}
}

func TestResolvePrefersMachineOverChannelOverNothing(t *testing.T) {
	p := policy(rule(RuleJobsEnabled))
	channel := Assignment{Scope: ScopeChannel, ScopeID: "stable", PolicyID: "baseline",
		Revision: 1, Digest: "sha256:c", Policy: p, AssignedAt: "2026-09-12T00:00:00Z"}
	machine := Assignment{Scope: ScopeMachine, ScopeID: "m1", PolicyID: "tight",
		Revision: 2, Digest: "sha256:m", Policy: p, AssignedAt: "2026-09-11T00:00:00Z"}

	if got := Resolve("m1", "stable", nil); got.Source != SourceNone || got.Assigned() {
		t.Fatalf("沒有任何指派卻解析出 %+v", got)
	}
	if got := Resolve("m1", "stable", []Assignment{channel}); got.Source != SourceChannel ||
		got.PolicyID != "baseline" {
		t.Fatalf("channel 指派沒有生效：%+v", got)
	}
	// ⚠ 機器指派比 channel 指派早一天，仍然要贏：那是有人針對這一台做的決定。
	if got := Resolve("m1", "stable", []Assignment{channel, machine}); got.Source != SourceMachine ||
		got.PolicyID != "tight" {
		t.Fatalf("後來的 channel 指派把針對單機的決定蓋掉了：%+v", got)
	}
	if got := Resolve("m2", "", []Assignment{channel, machine}); got.Source != SourceNone {
		t.Fatalf("不在範圍內的機器被判進來了：%+v", got)
	}

	// 同一個 scope 裡誰贏是看指派時間，不是看 revision 號碼大小：後來換上去的
	// 那份原則就算 revision 比較小，也是現在生效的那一份。
	replacement := Assignment{Scope: ScopeMachine, ScopeID: "m1", PolicyID: "relaxed",
		Revision: 1, Digest: "sha256:r", Policy: p, AssignedAt: "2026-09-12T09:00:00Z"}
	if got := Resolve("m1", "stable", []Assignment{machine, replacement}); got.PolicyID != "relaxed" {
		t.Fatalf("最新的機器指派沒有生效：%+v", got)
	}
}

func TestEveryRuleKindHasAnOrderALabelAndAnEvaluation(t *testing.T) {
	kinds := RuleKinds()
	if len(kinds) != len(ruleOrder) {
		t.Fatalf("RuleKinds 回 %d 個，ruleOrder 有 %d 個", len(kinds), len(ruleOrder))
	}
	seen := map[int]RuleKind{}
	for _, kind := range kinds {
		order := ruleOrder[kind]
		if other, clash := seen[order]; clash {
			t.Fatalf("%s 與 %s 的順序都是 %d", kind, other, order)
		}
		seen[order] = kind
		if RuleLabel(kind) == string(kind) {
			t.Errorf("規則 %s 沒有給操作員看的名字", kind)
		}
		// 每一種規則都必須在「什麼證據都沒有」時說出量不到，而不是靜靜通過。
		if got := (Rule{Kind: kind}).evaluate(Facts{}); got.Outcome != OutcomeUnmeasured {
			t.Errorf("規則 %s 在沒有證據時回 %s", kind, got.Outcome)
		}
	}
	for _, v := range []Verdict{VerdictCompliant, VerdictNoncompliant, VerdictUnmeasured,
		VerdictNeverReported, VerdictNotEvaluated} {
		if Label(v) == string(v) {
			t.Errorf("verdict %s 沒有給操作員看的名字", v)
		}
	}
}
