package rollout

import (
	"strings"
	"testing"
	"time"
)

func TestPromoteGateNeedsACompleteBusinessCalendarDay(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name     string
		finished time.Time
		earliest time.Time
	}{
		{"週五下午", time.Date(2026, 9, 4, 15, 0, 0, 0, loc), time.Date(2026, 9, 8, 0, 0, 0, 0, loc)},
		{"週一上午", time.Date(2026, 9, 7, 10, 0, 0, 0, loc), time.Date(2026, 9, 9, 0, 0, 0, 0, loc)},
		{"週一整點午夜", time.Date(2026, 9, 7, 0, 0, 0, 0, loc), time.Date(2026, 9, 8, 0, 0, 0, 0, loc)},
		{"週六", time.Date(2026, 9, 5, 8, 0, 0, 0, loc), time.Date(2026, 9, 8, 0, 0, 0, 0, loc)},
		// 2026-11-01 回撥一小時；這裡要數本地日曆日，不是 finished+24h。
		{"跨 DST 週末", time.Date(2026, 10, 31, 12, 0, 0, 0, loc), time.Date(2026, 11, 3, 0, 0, 0, 0, loc)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			facts := PromoteFacts{Version: "2026.9.2", Digest: strings.Repeat("a", 64), Canary: &CanaryRun{
				DeploymentID: "1234567890", FinishedAt: tc.finished,
				Targets: []CanaryTarget{{
					MachineID: "cnode", DisplayName: "samplehub1", JobID: "job-cnode", Ran: true, Succeeded: true, Judged: true,
					LastObservation: tc.earliest, WorkloadObserved: true, OpenClawPresent: true, WorkloadReady: true,
					RunningVersion: "2026.9.2", AppliedDigest: strings.Repeat("a", 64), IndependentGate: IndependentGatePassed,
				}},
			}}
			before := PromoteGate(facts, tc.earliest.Add(-time.Nanosecond), loc)
			if before.Allowed || !before.EarliestAt.Equal(tc.earliest) || len(before.Reasons) != 1 {
				t.Fatalf("到界線前 decision=%+v，預期 earliest=%v", before, tc.earliest)
			}
			at := PromoteGate(facts, tc.earliest, loc)
			if !at.Allowed || !at.EarliestAt.IsZero() || len(at.Reasons) != 0 {
				t.Fatalf("界線上仍被擋：%+v", at)
			}
			if got := at.Summary(); !strings.Contains(got, "promote 可以：canary 12345678") ||
				!strings.Contains(got, "滿一個完整工作天，期間沒有沉默失敗，跨故障域 verifier 已通過") {
				t.Fatalf("允許摘要=%q", got)
			}
		})
	}
}

func TestPromoteGateListsEveryReasonAndCapsSilentFailures(t *testing.T) {
	loc := time.FixedZone("EDT", -4*60*60)
	finished := time.Date(2026, 9, 7, 10, 0, 0, 0, loc)
	facts := PromoteFacts{
		Version: "2026.9.2", Digest: "abcdefghijklmnop", Canary: &CanaryRun{
			DeploymentID: "deployment-long", FinishedAt: finished, Stuck: 2,
			Targets: []CanaryTarget{
				{DisplayName: "retired", Retired: true, Ran: true, Succeeded: true},
				{DisplayName: "offline", Judged: true, Ran: true, Succeeded: true, Unreachable: true, WorkloadObserved: true, OpenClawPresent: true, WorkloadReady: true},
				{DisplayName: "silent-now", Judged: true, Ran: true, Succeeded: true, SilentNow: true, WorkloadObserved: true, OpenClawPresent: true, WorkloadReady: true},
			},
		},
		Silent: []SilentFailure{
			{FirstSeenAt: finished.Add(10 * time.Hour), LastSeenAt: finished.Add(10 * time.Hour), DisplayName: "samplehub1", Reason: "8 小時沒完成"},
			{FirstSeenAt: finished.Add(11 * time.Hour), LastSeenAt: finished.Add(11 * time.Hour), DisplayName: "samplehub1", Reason: "仍然沒有"},
			{FirstSeenAt: finished.Add(12 * time.Hour), LastSeenAt: finished.Add(12 * time.Hour), DisplayName: "sampleagent2", Reason: "失聯"},
			{FirstSeenAt: finished.Add(13 * time.Hour), LastSeenAt: finished.Add(13 * time.Hour), DisplayName: "sampleagent4", Reason: "失聯"},
		},
	}
	d := PromoteGate(facts, finished.Add(14*time.Hour), loc)
	joined := strings.Join(d.Reasons, "；")
	for _, want := range []string{
		"canary 部署 deployme 有 2 台 stuck", "最早 2026-09-09 00:00 EDT",
		"samplehub1 2026-09-07 20:00 EDT（8 小時沒完成）", "+1",
		"retired 已退場，canary 的結果現在沒有證人",
		"offline 現在失聯", "silent-now 現在就有沉默失敗",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("reasons 缺少 %q：%s", want, joined)
		}
	}
	if strings.Contains(joined, "sampleagent4") {
		t.Fatalf("沉默失敗超過三筆仍逐筆列出：%s", joined)
	}
	if strings.Contains(joined, "retired 現在沒有判決") {
		t.Fatalf("退場機器重複講沒有判決：%s", joined)
	}
	if got := d.Summary(); got != "promote 鎖著："+joined {
		t.Fatalf("Summary=%q", got)
	}
}

func TestPromoteGateRejectsExcludedAndUnrunCanaryTargets(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	digest := strings.Repeat("c", 64)
	facts := PromoteFacts{Version: "2026.9.2", Digest: digest, Canary: &CanaryRun{
		DeploymentID: "excluded-run", FinishedAt: now.Add(-48 * time.Hour),
		Targets: []CanaryTarget{
			{DisplayName: "node-old", ExcludedReason: "missing_package"},
			{DisplayName: "not-opened"},
		},
	}}
	d := PromoteGate(facts, now, time.UTC)
	joined := strings.Join(d.Reasons, "；")
	for _, want := range []string{"node-old 在 canary 被排除（missing_package）", "not-opened 沒有跑到 canary 工作單"} {
		if !strings.Contains(joined, want) {
			t.Errorf("判決缺少 %q：%s", want, joined)
		}
	}
	if d.Allowed {
		t.Fatalf("被排除／沒開單的 canary 竟可 promote：%+v", d)
	}
}

func TestPromoteGateRequiresFreshMatchingRunningAndAppliedEvidence(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	digest := strings.Repeat("d", 64)
	base := PromoteFacts{Version: "2026.9.2", Digest: digest, Canary: &CanaryRun{
		DeploymentID: "witnesses", FinishedAt: now.Add(-48 * time.Hour),
		Targets: []CanaryTarget{{
			DisplayName: "samplehub1", JobID: "job-cnode", Judged: true, Ran: true, Succeeded: true,
			LastObservation: now, WorkloadObserved: true, OpenClawPresent: true, WorkloadReady: true,
			RunningVersion: "2026.9.2", AppliedDigest: digest, IndependentGate: IndependentGatePassed,
		}},
	}}
	if d := PromoteGate(base, now, time.UTC); !d.Allowed {
		t.Fatalf("完整證據被擋：%+v", d)
	}

	cases := []struct {
		name   string
		mutate func(*CanaryTarget)
		want   string
	}{
		{"觀測過期", func(t *CanaryTarget) { t.LastObservation = now.Add(-21 * time.Minute) }, "沒有新鮮觀測"},
		{"OpenClaw 不存在", func(t *CanaryTarget) { t.OpenClawPresent = false }, "OpenClaw 不存在"},
		{"時鐘不可信", func(t *CanaryTarget) { t.ClockUntrusted = true }, "時鐘跟 Hub 偏差"},
		{"workload 證據不完整", func(t *CanaryTarget) {
			t.WorkloadReady = false
			t.WorkloadReason = "最新同一批 workload observation 證據不完整"
		}, "workload observation 證據不完整"},
		{"版本不同", func(t *CanaryTarget) { t.RunningVersion = "2026.9.1" }, "現在跑的是 2026.9.1，不是 2026.9.2"},
		{"版本未知", func(t *CanaryTarget) { t.RunningVersion = "" }, "現在跑的 OpenClaw 版本不知道"},
		{"applied 順序不可信", func(t *CanaryTarget) { t.AppliedReason = "工作單時間順序不可信" }, "工作單時間順序不可信"},
		{"digest 不同", func(t *CanaryTarget) { t.AppliedDigest = strings.Repeat("e", 64) }, "最近成功套用的 digest"},
		{"digest 未知", func(t *CanaryTarget) { t.AppliedDigest = "" }, "找不到成功套用這個 digest 的工作單"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			facts := base
			run := *base.Canary
			run.Targets = append([]CanaryTarget(nil), base.Canary.Targets...)
			facts.Canary = &run
			tc.mutate(&facts.Canary.Targets[0])
			d := PromoteGate(facts, now, time.UTC)
			if d.Allowed || !strings.Contains(d.Summary(), tc.want) {
				t.Fatalf("decision=%+v，預期 %q", d, tc.want)
			}
		})
	}
}

func TestPromoteGateReportsMissingWorkloadWitnessHonestly(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	digest := strings.Repeat("d", 64)
	facts := PromoteFacts{Version: "2026.9.2", Digest: digest, Canary: &CanaryRun{
		DeploymentID: "missing-witness", FinishedAt: now.Add(-48 * time.Hour),
		Targets: []CanaryTarget{{
			DisplayName: "samplehub1", JobID: "job-cnode", Judged: true, Ran: true, Succeeded: true,
			LastObservation: now, WorkloadObserved: false, OpenClawPresent: false, WorkloadReady: false,
			WorkloadReason: "還沒有同一批證據完整的 workload observation",
			RunningVersion: "2026.9.2", AppliedDigest: digest, IndependentGate: IndependentGatePassed,
		}},
	}}
	d := PromoteGate(facts, now, time.UTC)
	summary := d.Summary()
	if strings.Contains(summary, "現在回報 OpenClaw 不存在") {
		t.Fatalf("沒有 workload witness 卻宣稱機器回報不存在：%s", summary)
	}
	if !strings.Contains(summary, "還沒有同一批證據完整的 workload observation") {
		t.Fatalf("沒有說出 workload witness 缺口：%s", summary)
	}
	if d.Allowed {
		t.Fatalf("缺少 workload witness 竟可 promote：%+v", d)
	}
}

func TestPromoteGateRequiresOneCompleteFleetPeerReportPerSuccessfulTarget(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	digest := strings.Repeat("f", 64)
	base := CanaryTarget{
		MachineID: "cnode", DisplayName: "samplehub1", JobID: "1234567890-job",
		Ran: true, Succeeded: true, Judged: true, LastObservation: now,
		WorkloadObserved: true, OpenClawPresent: true, WorkloadReady: true, RunningVersion: "2026.9.2", AppliedDigest: digest,
	}
	for _, test := range []struct {
		state IndependentGateState
		want  string
	}{
		{IndependentGateUnassigned, "尚未指派跨故障域 verifier；先到工作單指派"},
		{IndependentGateAwaitingReport, "正在等完整回報"},
		{IndependentGateIncompleteReport, "只有部分 verifier 規則回報；重新執行"},
		{IndependentGateProducerRevoked, "只剩已撤銷 verifier 的證據；指派仍有效"},
		{IndependentGateDigestMismatch, "獨立證據 digest 衝突；確認 artifact 後重跑 canary"},
		{IndependentGateReleaseMismatch, "verifier 看到另一個 OpenClaw 版本；修復後重跑 canary"},
		{IndependentGateReleaseUnreported, "verifier 沒有回報結構化版本；升級後重新指派 verifier"},
		{IndependentGateStale, "獨立證據早於工作單完成；重新指派 verifier"},
		{IndependentGateFailed, "獨立證據沒有通過；修復後重跑 canary"},
	} {
		t.Run(string(test.state), func(t *testing.T) {
			target := base
			target.IndependentGate = test.state
			facts := PromoteFacts{Version: "2026.9.2", Digest: digest, Canary: &CanaryRun{
				DeploymentID: "canary-gate", FinishedAt: now.Add(-48 * time.Hour), Targets: []CanaryTarget{target},
			}}
			decision := PromoteGate(facts, now, time.UTC)
			if decision.Allowed || !strings.Contains(decision.Summary(), test.want) {
				t.Fatalf("state=%s decision=%+v", test.state, decision)
			}
			if len(decision.IndependentTargets) != 1 ||
				decision.IndependentTargets[0].State != test.state ||
				decision.IndependentTargets[0].JobID != base.JobID {
				t.Fatalf("state=%s target evidence=%+v", test.state, decision.IndependentTargets)
			}
		})
	}
	passed := base
	passed.IndependentGate = IndependentGatePassed
	decision := PromoteGate(PromoteFacts{Version: "2026.9.2", Digest: digest, Canary: &CanaryRun{
		DeploymentID: "canary-gate", FinishedAt: now.Add(-48 * time.Hour), Targets: []CanaryTarget{passed},
	}}, now, time.UTC)
	if !decision.Allowed || len(decision.IndependentTargets) != 1 ||
		!strings.Contains(decision.Summary(), "跨故障域 verifier 已通過") {
		t.Fatalf("完整獨立報告仍被擋：%+v", decision)
	}
}

func TestPromotionDoesNotAskForAVerifierOnAFailedCanaryJob(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	facts := PromoteFacts{Version: "2026.9.2", Digest: strings.Repeat("f", 64), Canary: &CanaryRun{
		DeploymentID: "canary-failed", FinishedAt: now.Add(-48 * time.Hour), Targets: []CanaryTarget{{
			MachineID: "cnode", DisplayName: "samplehub1", JobID: "failed-job",
			Ran: true, Succeeded: false,
		}},
	}}
	decision := PromoteGate(facts, now, time.UTC)
	if decision.Allowed || len(decision.IndependentTargets) != 0 ||
		!strings.Contains(decision.Summary(), "工作單沒有成功完成") {
		t.Fatalf("failed canary got a verifier action: %+v", decision)
	}
}

func TestPromoteGateSilentFailureAtHourTenPermanentlyLocksThatRun(t *testing.T) {
	loc := time.UTC
	finished := time.Date(2026, 9, 7, 0, 0, 0, 0, loc)
	facts := PromoteFacts{Version: "2026.9.2", Digest: strings.Repeat("b", 64), Canary: &CanaryRun{
		DeploymentID: "abcdefghi", FinishedAt: finished,
	}, Silent: []SilentFailure{{FirstSeenAt: finished.Add(10 * time.Hour), LastSeenAt: finished.Add(10 * time.Hour), DisplayName: "samplehub1", Reason: "8 小時無合格完成事件"}}}
	d := PromoteGate(facts, finished.Add(30*24*time.Hour), loc)
	if d.Allowed || !strings.Contains(d.Summary(), "canary 這段期間有沉默失敗") {
		t.Fatalf("第 10 小時的沉默失敗被時間洗掉：%+v", d)
	}
}

func TestPromoteGateWithoutCanary(t *testing.T) {
	d := PromoteGate(PromoteFacts{Version: "2026.9.2", Digest: "1234567890abcdef"}, time.Now(), time.UTC)
	if d.Allowed || len(d.Reasons) != 1 || d.Reasons[0] != "canary 沒部署過 2026.9.2（digest 1234567890ab）" {
		t.Fatalf("decision=%+v", d)
	}
}
