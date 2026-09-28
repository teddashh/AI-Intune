package store

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/deploy"
	"github.com/teddashh/AI-Intune/internal/expect"
	"github.com/teddashh/AI-Intune/internal/model"
	"github.com/teddashh/AI-Intune/internal/rollout"
	"github.com/teddashh/AI-Intune/internal/state"
)

type promoteCoverageFixture struct {
	store    *Store
	stable   NewDeployment
	finished time.Time
	now      time.Time
}

func newPromoteCoverageFixture(t *testing.T) promoteCoverageFixture {
	t.Helper()
	s := rolloutStore(t)
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	finished := now.Add(-48 * time.Hour)
	s.nowFn = func() time.Time { return now }
	digest := strings.Repeat("a", 64)
	addRolloutMachine(t, s, "cnode", "samplehub1", false)
	canary, jobs := createPromoteDeployment(t, s, "canary", "2026.9.2", digest, "",
		[]NewDeploymentTarget{{MachineID: "cnode", BatchNo: 1}})
	setPromoteJobState(t, s, jobs["cnode"], deploy.Succeeded, finished)
	finishPromoteDeployment(t, s, canary.DeploymentID, DeploymentRunning, finished)
	stable := NewDeployment{
		Channel: "stable", ResourceKind: "openclaw", ResourceID: "openclaw",
		Spec:      `{"kind":"openclaw","version":"2026.9.2","artifact":{"sha256":"` + digest + `"}}`,
		BatchSize: 1, CreatedBy: "coverage-test",
		Targets: []NewDeploymentTarget{{MachineID: "cnode", BatchNo: 1}},
		Job:     NewJob{ArtifactDigest: "sha256:" + digest, ExecutionTimeout: 600},
	}
	return promoteCoverageFixture{store: s, stable: stable, finished: finished, now: now}
}

func recordPromoteCoverageObservation(t *testing.T, s *Store, at time.Time, version string, mutate func(*model.ObservationBatch)) {
	t.Helper()
	if err := s.RecordCheckin("cnode", model.Checkin{SentAt: at, AgentStartedAt: at}, at); err != nil {
		t.Fatal(err)
	}
	b := promoteObservation(version, at)
	stampCurrentWorkloadPolicy(t, s, "samplehub1", &b)
	if mutate != nil {
		mutate(&b)
	}
	if err := s.RecordObservation("cnode", b, at); err != nil {
		t.Fatal(err)
	}
}

func recordPromoteCoverageRange(t *testing.T, f promoteCoverageFixture, mutateAt map[time.Time]func(*model.ObservationBatch)) {
	t.Helper()
	for at := f.finished; !at.After(f.now); at = at.Add(state.ObservationStale) {
		mutate := mutateAt[at]
		recordPromoteCoverageObservation(t, f.store, at, "2026.9.2", mutate)
	}
}

func coverageDecision(t *testing.T, f promoteCoverageFixture) rollout.PromoteDecision {
	t.Helper()
	facts, err := f.store.PromoteFacts("2026.9.2", f.stable.Job.ArtifactDigest, f.now)
	if err != nil {
		t.Fatal(err)
	}
	return rollout.PromoteGate(facts, f.now, time.UTC)
}

func TestPromoteRequiresContinuousHealthyWorkloadEvidence(t *testing.T) {
	t.Run("normal cadence", func(t *testing.T) {
		f := newPromoteCoverageFixture(t)
		recordPromoteCoverageRange(t, f, nil)
		if decision := coverageDecision(t, f); !decision.Allowed {
			t.Fatalf("continuous healthy coverage stayed locked: %s", decision.Summary())
		}
	})

	t.Run("Hub outage or forward clock jump", func(t *testing.T) {
		f := newPromoteCoverageFixture(t)
		gapAt := f.finished.Add(12 * time.Hour)
		for at := f.finished; !at.After(f.now); at = at.Add(state.ObservationStale) {
			if at.Equal(gapAt) {
				continue
			}
			recordPromoteCoverageObservation(t, f.store, at, "2026.9.2", nil)
		}
		if decision := coverageDecision(t, f); decision.Allowed || !strings.Contains(decision.Summary(), "觀測") {
			t.Fatalf("coverage gap unlocked stable: %s", decision.Summary())
		}
	})

	t.Run("unknown middle batch", func(t *testing.T) {
		f := newPromoteCoverageFixture(t)
		unknownAt := f.finished.Add(12 * time.Hour)
		recordPromoteCoverageRange(t, f, map[time.Time]func(*model.ObservationBatch){
			unknownAt: func(b *model.ObservationBatch) { b.CLITools = nil },
		})
		if decision := coverageDecision(t, f); decision.Allowed || !strings.Contains(decision.Summary(), "觀測") {
			t.Fatalf("unknown evidence was washed out by later healthy batch: %s", decision.Summary())
		}
	})

	// 這裡釘住的操作員後果是：soak 期間 gateway 明確沒有在跑的那一批若被後面的綠燈洗掉，
	// 這台機器就會被當成合格的 canary 證人，把這一版開到 stable 全機隊；不同於 unknown middle batch 的沒量到，這裡是明確量到壞事實。
	// 這個情境會同時產出沉默失敗的時間、機器與故障細節，以及該機器不能當 canary 證人的結論；兩句各自指引不同的操作員下一步。
	t.Run("explicitly failed middle batch", func(t *testing.T) {
		f := newPromoteCoverageFixture(t)
		failedAt := f.finished.Add(12 * time.Hour)
		recordPromoteCoverageRange(t, f, map[time.Time]func(*model.ObservationBatch){
			failedAt: func(b *model.ObservationBatch) { b.OpenClaw.Install.MainPID = 0 },
		})
		decision := coverageDecision(t, f)
		want := []string{
			"canary 這段期間有沉默失敗：samplehub1 2026-09-08 00:00 UTC～2026-09-08 00:20 UTC（OpenClaw gateway 沒有正在執行的 MainPID）",
			"samplehub1 不能當 canary 證人：canary 期間有一批 workload 觀測明確失敗",
		}
		if decision.Allowed {
			t.Errorf("decision.Allowed=%t，預期 false：soak 期間 gateway 明確沒有在跑的那一批被後面的綠燈洗掉，這台機器就會被當成合格的 canary 證人，把這一版開到 stable 全機隊。", decision.Allowed)
		}
		equal := len(decision.Reasons) == len(want)
		for i := 0; equal && i < len(want); i++ {
			equal = decision.Reasons[i] == want[i]
		}
		if !equal {
			t.Errorf("decision.Reasons=%q，預期兩句 %q：少掉「不能當 canary 證人」那句，這台機器就會被當成合格的 canary 證人，把這一版開到 stable 全機隊；少掉沉默失敗那句，操作員看不到是什麼時候壞的。", decision.Reasons, want)
		}
	})

	// soak cursor 保證其後的列都是 canary 完成後才 ingest；若 Hub 牆鐘回撥，received_at 卻會落在完成時刻之前，破壞 append 順序與牆鐘時間的一致性。
	// 下毒列必須排在正常 cadence 前面，才能確定 cursor 後第一筆證據就已矛盾，而不是讓其他拒絕理由先遮住這個不可修復的帳本問題。
	t.Run("first row after cursor has wall clock before canary completion", func(t *testing.T) {
		f := newPromoteCoverageFixture(t)
		recordPromoteCoverageObservation(t, f.store, f.finished.Add(-time.Second), "2026.9.2", nil)
		recordPromoteCoverageRange(t, f, nil)
		decision := coverageDecision(t, f)
		want := []string{
			"samplehub1 不能當 canary 證人：canary 完成後 append 的 workload 觀測時間早於完成時間，Hub 時鐘順序不可信",
		}
		if decision.Allowed {
			t.Errorf("decision.Allowed=%t，預期 false：這份 soak 會被當成完整覆蓋，這一版會被開到 stable 全機隊。", decision.Allowed)
		}
		equal := len(decision.Reasons) == len(want)
		for i := 0; equal && i < len(want); i++ {
			equal = decision.Reasons[i] == want[i]
		}
		if !equal {
			t.Errorf("decision.Reasons=%q，預期 %q：報成別的理由（例如「Hub 時間倒退」「最近 N 內沒有觀測覆蓋」）會叫操作員再等一輪觀測，但帳本的收件時間跟完成游標互相矛盾，再收新觀測也無法讓這份 soak 證據變可信。", decision.Reasons, want)
		}
	})

	// Hub 牆鐘在 soak 中途倒退但仍晚於 canary 完成時刻，所以上一條「早於完成時間」的守衛接不到；
	// T 那筆刻意讓量測時間落後 19 分鐘，讓倒退 ingest 的量測時間仍嚴格向前，只留下 Hub 收件時間倒退的矛盾。
	t.Run("Hub received time moves backward during soak", func(t *testing.T) {
		f := newPromoteCoverageFixture(t)
		stale := state.ObservationStale          // S 是 workload 觀測允許的 20 分鐘 stale 窗。
		turnAt := f.finished.Add(12 * time.Hour) // T 固定在 canary 完成後 12 小時，確保倒退後仍晚於完成時刻。
		for at := f.finished; at.Before(turnAt); at = at.Add(stale) {
			recordPromoteCoverageObservation(t, f.store, at, "2026.9.2", nil)
		}
		recordPromoteCoverageObservation(t, f.store, turnAt, "2026.9.2", func(b *model.ObservationBatch) {
			b.MeasuredAt = turnAt.Add(-19 * time.Minute) // 落後 19 分鐘仍在 S 內，並讓下一筆量測時間嚴格向前。
		})
		recordPromoteCoverageObservation(t, f.store, turnAt.Add(-10*time.Minute), "2026.9.2", nil) // Hub 倒退 10 分鐘，但量測時間比 T-19m 晚。
		for at := turnAt.Add(10 * time.Minute); at.Before(f.now); at = at.Add(stale) {             // 從 T+10m 接回 S cadence，避開倒退點後的空窗。
			recordPromoteCoverageObservation(t, f.store, at, "2026.9.2", nil)
		}
		// 倒退讓 cadence 相位偏移十分鐘；補在判斷時刻的 checkin 與 latest witness，避免機器被判成失聯而把失聯理由混進 Reasons。
		recordPromoteCoverageObservation(t, f.store, f.now, "2026.9.2", nil)
		decision := coverageDecision(t, f)
		want := []string{
			"samplehub1 不能當 canary 證人：workload 觀測 ledger 的 Hub 時間倒退，連續覆蓋不可信",
		}
		if decision.Allowed {
			t.Errorf("decision.Allowed=%t，預期 false：牆鐘倒退那段時間其實沒有可信的連續覆蓋，這份 soak 會被當成完整，這一版會被開到 stable 全機隊。", decision.Allowed)
		}
		equal := len(decision.Reasons) == len(want)
		for i := 0; equal && i < len(want); i++ {
			equal = decision.Reasons[i] == want[i]
		}
		if !equal {
			t.Errorf("decision.Reasons=%q，預期 %q：報成「evidence_at 沒有嚴格向前」或「量測證據中間有空窗」會把操作員送去查 agent 的量測，但壞的是 Hub 自己的收件時間。", decision.Reasons, want)
		}
	})

	// 這一臂的方向跟前兩支相反：這筆收件在判斷時刻之後，不是在 canary 完成時刻之前，也不是比前一筆收件更早；
	// 量測時間必須留在判斷時刻之前，才不會先被 evidence_at 晚於判斷時刻的守衛攔下，確實釘住收件時間與這次判斷時刻的矛盾。
	t.Run("received time is later than decision time", func(t *testing.T) {
		f := newPromoteCoverageFixture(t)
		stale := state.ObservationStale                              // S 是 workload 觀測允許的 20 分鐘 stale 窗，用作正常 cadence。
		for at := f.finished; at.Before(f.now); at = at.Add(stale) { // 正常 cadence 只記到判斷時刻之前，不包含 f.now。
			recordPromoteCoverageObservation(t, f.store, at, "2026.9.2", nil)
		}
		recordPromoteCoverageObservation(t, f.store, f.now, "2026.9.2", func(b *model.ObservationBatch) { // 收件正好落在判斷時刻，補齊健康覆蓋。
			b.MeasuredAt = f.now.Add(-2 * time.Minute) // 落後 2 分鐘仍在 S 內，並讓下一筆量測時間能嚴格向前。
		})
		recordPromoteCoverageObservation(t, f.store, f.now.Add(time.Second), "2026.9.2", func(b *model.ObservationBatch) { // 晚 1 秒的毒列落在判斷時刻之後。
			b.MeasuredAt = f.now.Add(-time.Minute) // 落後 1 分鐘使量測早於判斷時刻，且比上一筆 f.now-2m 嚴格向前。
		})
		decision := coverageDecision(t, f)
		want := []string{
			"samplehub1 不能當 canary 證人：workload 觀測 ledger 的 received_at 晚於目前判斷時間",
		}
		if decision.Allowed {
			t.Errorf("decision.Allowed=%t，預期 false：判決時刻之後才進帳本的收件被當成這次判決的 soak 證據，沒監看到的空窗被補綠，這一版會被開到 stable 全機隊。", decision.Allowed)
		}
		equal := len(decision.Reasons) == len(want)
		for i := 0; equal && i < len(want); i++ {
			equal = decision.Reasons[i] == want[i]
		}
		if !equal {
			t.Errorf("decision.Reasons=%q，預期 %q：報成「evidence_at 晚於目前判斷時間」或「Hub 時間倒退」會把操作員送去查 agent 的量測或前一筆收件，但壞的是這一筆收件時間與這次判決時刻的關係。", decision.Reasons, want)
		}
	})

	t.Run("measurement blackout behind on-time delivery", func(t *testing.T) {
		f := newPromoteCoverageFixture(t)
		laggedAt := f.finished.Add(12 * time.Hour)
		recordPromoteCoverageRange(t, f, map[time.Time]func(*model.ObservationBatch){
			laggedAt: func(b *model.ObservationBatch) {
				b.MeasuredAt = laggedAt.Add(-state.ObservationStale / 2)
			},
		})
		decision := coverageDecision(t, f)
		want := fmt.Sprintf("samplehub1 不能當 canary 證人：workload 量測證據中間有超過 %s 的空窗", state.ObservationStale)
		if decision.Allowed {
			t.Fatalf("measurement blackout unlocked stable: %s", decision.Summary())
		}
		if len(decision.Reasons) != 1 {
			t.Fatalf("measurement blackout produced %d reasons, want 1: %s", len(decision.Reasons), decision.Summary())
		}
		if decision.Reasons[0] != want {
			t.Fatalf("measurement blackout reason = %q, want %q: %s", decision.Reasons[0], want, decision.Summary())
		}
	})

	t.Run("version drift", func(t *testing.T) {
		f := newPromoteCoverageFixture(t)
		driftAt := f.finished.Add(12 * time.Hour)
		for at := f.finished; !at.After(f.now); at = at.Add(state.ObservationStale) {
			version := "2026.9.2"
			if at.Equal(driftAt) {
				version = "2026.9.1"
			}
			recordPromoteCoverageObservation(t, f.store, at, version, nil)
		}
		if decision := coverageDecision(t, f); decision.Allowed || !strings.Contains(decision.Summary(), "版本") {
			t.Fatalf("temporary version drift was washed out: %s", decision.Summary())
		}
	})

	t.Run("policy generation change", func(t *testing.T) {
		f := newPromoteCoverageFixture(t)
		changeAt := f.finished.Add(12 * time.Hour)
		for at := f.finished; !at.After(f.now); at = at.Add(state.ObservationStale) {
			if at.Equal(changeAt) {
				f.store.SetExpectations(&expect.Set{Configured: true})
				if err := f.store.PublishExpectationsPolicy(at); err != nil {
					t.Fatal(err)
				}
			}
			recordPromoteCoverageObservation(t, f.store, at, "2026.9.2", nil)
		}
		if decision := coverageDecision(t, f); decision.Allowed || !strings.Contains(decision.Summary(), "policy") {
			t.Fatalf("policy change reused earlier soak: %s", decision.Summary())
		}
	})

	t.Run("future Hub receipt", func(t *testing.T) {
		f := newPromoteCoverageFixture(t)
		recordPromoteCoverageRange(t, f, nil)
		future := f.now.Add(time.Minute)
		recordPromoteCoverageObservation(t, f.store, future, "2026.9.2", nil)
		if decision := coverageDecision(t, f); decision.Allowed || !strings.Contains(decision.Summary(), "判斷時間") {
			t.Fatalf("future evidence unlocked stable: %s", decision.Summary())
		}
	})
}

func TestCoverageLedgerMigrationDoesNotBackfillFromLatestWitness(t *testing.T) {
	f := newPromoteCoverageFixture(t)
	// 模擬 upgrade 後只收到一批 fresh healthy：latest projection 是綠的，
	// 但新 ledger 在先前 48 小時是空的，不能反推那段歷史也健康。
	recordPromoteCoverageObservation(t, f.store, f.now, "2026.9.2", nil)
	decision := coverageDecision(t, f)
	if decision.Allowed || !strings.Contains(decision.Summary(), "canary 完成後") {
		t.Fatalf("empty migrated ledger was backfilled from latest witness: %s", decision.Summary())
	}
}

func TestMigratedFinishedCanaryWithoutSequenceBoundaryRequiresRerun(t *testing.T) {
	f := newPromoteCoverageFixture(t)
	recordPromoteCoverageRange(t, f, nil)
	if _, err := f.store.DB().Exec(`DELETE FROM deployment_soak_boundaries`); err != nil {
		t.Fatal(err)
	}
	facts, err := f.store.PromoteFacts("2026.9.2", f.stable.Job.ArtifactDigest, f.now)
	if err != nil {
		t.Fatal(err)
	}
	decision := rollout.PromoteGate(facts, f.now, time.UTC)
	if decision.Allowed || !strings.Contains(decision.Summary(), "sequence boundary") ||
		!strings.Contains(decision.Summary(), "重跑 canary") {
		t.Fatalf("migrated finished canary without append boundary unlocked stable: %s", decision.Summary())
	}
}

func TestPromoteCoverageCannotReuseEvidenceAppendedBeforeCanaryFinished(t *testing.T) {
	s := rolloutStore(t)
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	forgedFinished := now.Add(-48 * time.Hour)
	s.nowFn = func() time.Time { return now }
	digest := strings.Repeat("a", 64)
	addRolloutMachine(t, s, "cnode", "samplehub1", false)

	// Every row has a healthy timestamp inside the forged soak window, but its
	// append order proves it existed before this canary deployment was created.
	// A Hub clock rollback (or an internal caller passing an old now) must not let
	// those old rows pay for a canary that actually completed afterwards.
	for at := forgedFinished; !at.After(now); at = at.Add(state.ObservationStale) {
		recordPromoteCoverageObservation(t, s, at, "2026.9.2", nil)
	}
	canary, jobs := createPromoteDeployment(t, s, "canary", "2026.9.2", digest, "",
		[]NewDeploymentTarget{{MachineID: "cnode", BatchNo: 1}})
	setPromoteJobState(t, s, jobs["cnode"], deploy.Succeeded, forgedFinished)
	finishPromoteDeployment(t, s, canary.DeploymentID, DeploymentRunning, forgedFinished)

	facts, err := s.PromoteFacts("2026.9.2", digest, now)
	if err != nil {
		t.Fatal(err)
	}
	decision := rollout.PromoteGate(facts, now, time.UTC)
	if decision.Allowed {
		t.Fatalf("pre-canary evidence paid a forged old soak window: %s", decision.Summary())
	}
}

func TestCanaryFinishBusinessAnchorUsesStoreClockAcrossMidnight(t *testing.T) {
	s := rolloutStore(t)
	// The transition really commits just after Friday starts, but an internal
	// caller supplies a time ten seconds earlier on Thursday.  Both instants are
	// within the 20-minute coverage tolerance, so a sequence cursor alone cannot
	// stop the forged Thursday timestamp from buying all of Friday as a complete
	// business day.
	trustedFinished := time.Date(2026, 9, 11, 0, 0, 5, 0, time.UTC)
	callerFinished := trustedFinished.Add(-10 * time.Second)
	decisionAt := time.Date(2026, 9, 12, 0, 0, 5, 0, time.UTC)
	s.nowFn = func() time.Time { return trustedFinished }
	digest := strings.Repeat("a", 64)
	addRolloutMachine(t, s, "cnode", "samplehub1", false)
	canary, jobs := createPromoteDeployment(t, s, "canary", "2026.9.2", digest, "",
		[]NewDeploymentTarget{{MachineID: "cnode", BatchNo: 1}})
	setPromoteJobState(t, s, jobs["cnode"], deploy.Succeeded, callerFinished)
	changed, err := s.SetDeploymentState(canary.DeploymentID, DeploymentRunning, DeploymentFinished, callerFinished)
	if err != nil || !changed {
		t.Fatalf("finish canary changed=%v err=%v", changed, err)
	}
	stored, err := s.Deployment(canary.DeploymentID)
	if err != nil || stored.FinishedAt == nil || !stored.FinishedAt.Equal(trustedFinished) {
		t.Fatalf("finished business anchor trusted caller input: finished=%v err=%v", stored.FinishedAt, err)
	}

	for at := trustedFinished; !at.After(decisionAt); at = at.Add(state.ObservationStale) {
		recordPromoteCoverageObservation(t, s, at, "2026.9.2", nil)
	}
	s.nowFn = func() time.Time { return decisionAt }
	facts, err := s.PromoteFacts("2026.9.2", digest, decisionAt)
	if err != nil {
		t.Fatal(err)
	}
	decision := rollout.PromoteGate(facts, decisionAt, time.UTC)
	if decision.Allowed || !strings.Contains(decision.Summary(), "完整工作天") {
		t.Fatalf("caller-controlled pre-midnight finish bought Friday: %s", decision.Summary())
	}
}

func TestRecordObservationCoverageLedgerIsAppendOnlyAndAtomic(t *testing.T) {
	t.Run("same-second batches both remain", func(t *testing.T) {
		s := rolloutStore(t)
		addRolloutMachine(t, s, "cnode", "samplehub1", false)
		at := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
		if err := s.RecordCheckin("cnode", model.Checkin{SentAt: at, AgentStartedAt: at}, at); err != nil {
			t.Fatal(err)
		}
		first := promoteObservation("2026.9.2", at)
		stampCurrentWorkloadPolicy(t, s, "samplehub1", &first)
		if err := s.RecordObservation("cnode", first, at); err != nil {
			t.Fatal(err)
		}
		second := first
		second.CLITools = nil
		if err := s.RecordObservation("cnode", second, at); err != nil {
			t.Fatal(err)
		}
		rows, err := s.DB().Query(`SELECT verdict FROM workload_observation_evidence
		 WHERE machine_id='cnode' ORDER BY evidence_id`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var verdicts []string
		for rows.Next() {
			var verdict string
			if err := rows.Scan(&verdict); err != nil {
				t.Fatal(err)
			}
			verdicts = append(verdicts, verdict)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		if len(verdicts) != 2 || verdicts[0] != workloadWitnessHealthy || verdicts[1] != workloadWitnessUnknown {
			t.Fatalf("coverage ledger deduped or reordered same-second batches: %v", verdicts)
		}
	})

	t.Run("ledger write failure rolls back raw batch", func(t *testing.T) {
		s := rolloutStore(t)
		addRolloutMachine(t, s, "cnode", "samplehub1", false)
		at := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
		if err := s.RecordCheckin("cnode", model.Checkin{SentAt: at, AgentStartedAt: at}, at); err != nil {
			t.Fatal(err)
		}
		if _, err := s.DB().Exec(`DROP TABLE workload_observation_evidence`); err != nil {
			t.Fatal(err)
		}
		b := promoteObservation("2026.9.2", at)
		stampCurrentWorkloadPolicy(t, s, "samplehub1", &b)
		if err := s.RecordObservation("cnode", b, at); err == nil || !strings.Contains(err.Error(), "workload_observation_evidence") {
			t.Fatalf("missing coverage ledger error=%v", err)
		}
		for _, table := range []string{"observed_state", "workload_observation_witness", "canary_silent_failures"} {
			var count int
			if err := s.DB().QueryRow(`SELECT COUNT(*) FROM ` + table + ` WHERE machine_id='cnode'`).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count != 0 {
				t.Fatalf("%s retained %d partial rows after coverage write failure", table, count)
			}
		}
	})
}
