package compliance

import "testing"

// Verdict 與 ActionState 這兩組是手寫的，新增常數時要自己回來加一列。
func TestTheComplianceVocabularySaysTheseExactWords(t *testing.T) {
	labels := map[Verdict]string{
		VerdictCompliant:     "符合",
		VerdictNoncompliant:  "不符合",
		VerdictUnmeasured:    "有規則量不到",
		VerdictNeverReported: "機器從未報到",
		VerdictNotEvaluated:  "未指派合規性原則",
		// 不認得的判決 token 刻意原樣回傳，讓操作員看見實際收到的值。
		Verdict("nope"): "nope",
	}
	ruleLabels := map[RuleKind]string{
		RuleCheckinMaxAge:      "報到新鮮度",
		RuleAgentVersion:       "Agent 版本",
		RuleSettingsApplied:    "設定已套用",
		RuleDiskFreeMinPercent: "磁碟剩餘空間",
		RuleJobsEnabled:        "接受工作單",
		RuleKind("nope"):       "nope",
	}
	ruleBounds := []struct {
		name string
		rule Rule
		want string
	}{
		{name: "checkin max age", rule: Rule{Kind: RuleCheckinMaxAge, MaxAgeSeconds: 900}, want: "最久 900 秒"},
		// Agent 版本是操作員輸入的要求值，不是裝置回報值，因此安全地原樣顯示。
		{name: "agent version", rule: Rule{Kind: RuleAgentVersion, AgentVersion: "2026.9.1"}, want: "2026.9.1"},
		{name: "disk free minimum", rule: Rule{Kind: RuleDiskFreeMinPercent, MinFreePercent: 20}, want: "至少 20%"},
		{name: "settings applied", rule: Rule{Kind: RuleSettingsApplied}, want: ""},
		{name: "jobs enabled", rule: Rule{Kind: RuleJobsEnabled}, want: ""},
	}
	actionLabels := map[ActionKind]string{
		ActionBlockJobs:    "停發工作單",
		ActionKind("nope"): "nope",
	}
	actionEffects := map[ActionKind]string{
		ActionBlockJobs:    "不再領到新的工作單，也不會被算進新的部署",
		ActionKind("nope"): "",
	}
	actionStateLabels := map[ActionState]string{
		ActionStateNotTriggered: "未觸發",
		ActionStateInGrace:      "寬限中",
		ActionStateEnforced:     "生效中",
		ActionState("nope"):     "nope",
	}

	t.Run("Label", func(t *testing.T) {
		for verdict, want := range labels {
			if got := Label(verdict); got != want {
				t.Errorf("Label(%q) 實際得到 %q，期望 %q", verdict, got, want)
			}
		}
	})

	t.Run("RuleLabel", func(t *testing.T) {
		for kind, want := range ruleLabels {
			if got := RuleLabel(kind); got != want {
				t.Errorf("RuleLabel(%q) 實際得到 %q，期望 %q", kind, got, want)
			}
		}
	})

	t.Run("RuleBound", func(t *testing.T) {
		for _, tt := range ruleBounds {
			if got := RuleBound(tt.rule); got != tt.want {
				t.Errorf("RuleBound 的 %s 案例（%+v）實際得到 %q，期望 %q", tt.name, tt.rule, got, tt.want)
			}
		}
	})

	t.Run("ActionLabel", func(t *testing.T) {
		for kind, want := range actionLabels {
			if got := ActionLabel(kind); got != want {
				t.Errorf("ActionLabel(%q) 實際得到 %q，期望 %q", kind, got, want)
			}
		}
	})

	t.Run("ActionEffect", func(t *testing.T) {
		for kind, want := range actionEffects {
			if got := ActionEffect(kind); got != want {
				t.Errorf("ActionEffect(%q) 實際得到 %q，期望 %q", kind, got, want)
			}
		}
	})

	t.Run("ActionStateLabel", func(t *testing.T) {
		for state, want := range actionStateLabels {
			if got := ActionStateLabel(state); got != want {
				t.Errorf("ActionStateLabel(%q) 實際得到 %q，期望 %q", state, got, want)
			}
		}
	})

	for _, kind := range RuleKinds() {
		if _, ok := ruleLabels[kind]; !ok {
			t.Errorf("合規性多了一個規則 %q，但沒有人逐字讀過它對操作員說的話", kind)
		}
	}
	for _, kind := range ActionKinds() {
		if _, ok := actionLabels[kind]; !ok {
			t.Errorf("合規性多了一個動作 %q，但沒有人逐字讀過它對操作員說的話", kind)
		}
		if _, ok := actionEffects[kind]; !ok {
			t.Errorf("合規性多了一個動作效果 %q，但沒有人逐字讀過它對操作員說的話", kind)
		}
	}
}
