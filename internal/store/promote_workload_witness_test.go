package store

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/deploy"
	"github.com/teddashh/AI-Intune/internal/expect"
	"github.com/teddashh/AI-Intune/internal/model"
	"github.com/teddashh/AI-Intune/internal/rollout"
	"github.com/teddashh/AI-Intune/internal/state"
)

func promoteObservation(version string, at time.Time) model.ObservationBatch {
	b := healthyBatch(at)
	matched := true
	b.OpenClaw.Install = &model.OpenClawInstall{UnitFound: true, MainPID: 42, ProcessMatchesUnit: &matched,
		RunningDirVersion: version, NodeVersion: "24.15.0"}
	fingerprint, _, _ := expectationsPolicyFingerprint(&expect.Set{})
	b.WorkloadPolicyToken = workloadPolicyToken(fingerprint, 1, "samplehub1")
	return b
}

func promoteDecision(t *testing.T, s *Store, n NewDeployment, now time.Time) rollout.PromoteDecision {
	t.Helper()
	facts, err := s.PromoteFacts("2026.9.2", n.Job.ArtifactDigest, now)
	if err != nil {
		t.Fatal(err)
	}
	return rollout.PromoteGate(facts, now, time.UTC)
}

func activePolicyIdentity(t *testing.T, s *Store) (string, int64, bool) {
	t.Helper()
	var fingerprint string
	var generation int64
	var valid bool
	if err := s.DB().QueryRow(`SELECT expectations_fingerprint,generation,valid
	 FROM active_workload_policy WHERE singleton=1`).Scan(&fingerprint, &generation, &valid); err != nil {
		t.Fatalf("active policy identity: %v", err)
	}
	return fingerprint, generation, valid
}

func testWorkloadPolicy(tag string) *expect.Set {
	return &expect.Set{Configured: true, Rules: []model.Expectation{{
		Machine: "samplehub1", Unit: tag + ".service", Artifact: "/tmp/" + tag,
		MaxAgeSeconds: 3600, Why: "policy " + tag,
	}}}
}

func TestLatestPartialObservationMasksOlderCoherentWorkloadWitness(t *testing.T) {
	s := rolloutStore(t)
	n, _ := eligiblePromotionFixture(t, s)
	now := s.now()
	if decision := promoteDecision(t, s, n, now); !decision.Allowed {
		t.Fatalf("baseline locked: %s", decision.Summary())
	}

	// OpenClaw/DB 這列是新的，但 CLI 整列缺席。latest-by-subject 會
	// 保留上一批的 healthy CLI；promote 不能把兩批拼在一起。
	partial := promoteObservation("2026.9.2", now)
	partial.CLITools = nil
	if err := s.RecordObservation("cnode", partial, now); err != nil {
		t.Fatal(err)
	}
	decision := promoteDecision(t, s, n, now)
	if decision.Allowed || !strings.Contains(decision.Summary(), "workload") {
		t.Fatalf("partial latest batch reused old CLI witness: %s", decision.Summary())
	}
}

func TestMissingPolicyTokenMasksOlderCoherentWorkloadWitness(t *testing.T) {
	s := rolloutStore(t)
	n, _ := eligiblePromotionFixture(t, s)
	now := s.now()
	if decision := promoteDecision(t, s, n, now); !decision.Allowed {
		t.Fatalf("baseline locked: %s", decision.Summary())
	}

	oldAgent := promoteObservation("2026.9.2", now)
	oldAgent.WorkloadPolicyToken = ""
	if err := s.RecordObservation("cnode", oldAgent, now); err != nil {
		t.Fatal(err)
	}
	if decision := promoteDecision(t, s, n, now); decision.Allowed || !strings.Contains(decision.Summary(), "expect") {
		t.Fatalf("missing token reused older healthy witness: %s", decision.Summary())
	}
}

func TestWorkloadWitnessProjectionUsesReceivedAtMonotonicLastWriteWins(t *testing.T) {
	s := rolloutStore(t)
	addRolloutMachine(t, s, "cnode", "samplehub1", false)
	base := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	newer := base.Add(time.Minute)
	if err := s.RecordCheckin("cnode", model.Checkin{SentAt: newer, AgentStartedAt: base}, newer); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordObservation("cnode", promoteObservation("2026.9.2", newer), newer); err != nil {
		t.Fatal(err)
	}
	olderUnknown := promoteObservation("2026.9.2", base)
	olderUnknown.CLITools = nil
	if err := s.RecordObservation("cnode", olderUnknown, base); err != nil {
		t.Fatal(err)
	}
	var verdict, received string
	if err := s.DB().QueryRow(`SELECT verdict,received_at FROM workload_observation_witness WHERE machine_id='cnode'`).Scan(&verdict, &received); err != nil || verdict != workloadWitnessHealthy || received != fmtTime(newer) {
		t.Fatalf("older received batch replaced witness: verdict=%q received=%q err=%v", verdict, received, err)
	}

	sameTimeUnknown := promoteObservation("2026.9.2", newer)
	sameTimeUnknown.CLITools = nil
	if err := s.RecordObservation("cnode", sameTimeUnknown, newer); err != nil {
		t.Fatal(err)
	}
	if err := s.DB().QueryRow(`SELECT verdict,received_at FROM workload_observation_witness WHERE machine_id='cnode'`).Scan(&verdict, &received); err != nil || verdict != workloadWitnessUnknown || received != fmtTime(newer) {
		t.Fatalf("same received-at later call did not mask witness: verdict=%q received=%q err=%v", verdict, received, err)
	}
}

func TestWorkloadWitnessWriteFailureRollsBackRawObservation(t *testing.T) {
	s := rolloutStore(t)
	addRolloutMachine(t, s, "cnode", "samplehub1", false)
	if _, err := s.DB().Exec(`DROP TABLE workload_observation_witness`); err != nil {
		t.Fatal(err)
	}
	var before int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM observed_state WHERE machine_id='cnode'`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	partial := promoteObservation("2026.9.2", now)
	partial.CLITools = nil
	err := s.RecordObservation("cnode", partial, now)
	if err == nil || !strings.Contains(err.Error(), "workload_observation_witness") {
		t.Fatalf("missing witness table did not fail observation: %v", err)
	}
	var after int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM observed_state WHERE machine_id='cnode'`).Scan(&after); err != nil || after != before {
		t.Fatalf("witness write failure left raw rows: before=%d after=%d err=%v", before, after, err)
	}
}

func TestOpenDoesNotBackfillWorkloadWitnessFromLegacyObservationRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	addRolloutMachine(t, s, "cnode", "samplehub1", false)
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	if err := s.RecordCheckin("cnode", model.Checkin{SentAt: now, AgentStartedAt: now}, now); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordObservation("cnode", promoteObservation("2026.9.2", now), now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(`DROP TABLE workload_observation_witness`); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var count int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM workload_observation_witness`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("migration stitched legacy rows into coherent witness: count=%d err=%v", count, err)
	}
}

func TestExpectationChangeAndMissingMeasurementsMaskOldWorkloadWitness(t *testing.T) {
	s := rolloutStore(t)
	n, _ := eligiblePromotionFixture(t, s)
	now := s.now()
	if decision := promoteDecision(t, s, n, now); !decision.Allowed {
		t.Fatalf("baseline locked: %s", decision.Summary())
	}
	rule := model.Expectation{
		Machine: "samplehub1", Unit: "proof.service", Artifact: "/tmp/proof.jsonl",
		MaxAgeSeconds: 3600, Why: "external proof",
		Events: &model.EventSpec{TsField: "at", TypeField: "kind", NotOK: []string{"broken"}, WindowSeconds: 3600},
	}
	s.SetExpectations(&expect.Set{Configured: true, Rules: []model.Expectation{rule}})
	if err := s.PublishExpectationsPolicy(now); err != nil {
		t.Fatal(err)
	}
	if decision := promoteDecision(t, s, n, now); decision.Allowed || !strings.Contains(decision.Summary(), "expect") {
		t.Fatalf("changed expectations reused pre-change witness: %s", decision.Summary())
	}

	missingArtifactAt := now.Add(time.Minute)
	if err := s.RecordCheckin("cnode", model.Checkin{SentAt: missingArtifactAt, AgentStartedAt: now}, missingArtifactAt); err != nil {
		t.Fatal(err)
	}
	missingArtifact := promoteObservation("2026.9.2", missingArtifactAt)
	stampCurrentWorkloadPolicy(t, s, "samplehub1", &missingArtifact)
	if err := s.RecordObservation("cnode", missingArtifact, missingArtifactAt); err != nil {
		t.Fatal(err)
	}
	if decision := promoteDecision(t, s, n, now); decision.Allowed {
		t.Fatalf("missing artifact measurement passed: %s", decision.Summary())
	}

	missingEventAt := missingArtifactAt.Add(time.Minute)
	modTime := missingEventAt
	missingEvent := promoteObservation("2026.9.2", missingEventAt)
	stampCurrentWorkloadPolicy(t, s, "samplehub1", &missingEvent)
	missingEvent.Artifacts = []model.ArtifactCheck{{
		Unit: rule.Unit, Artifact: rule.Artifact, Exists: true, ModTime: &modTime,
	}}
	if err := s.RecordObservation("cnode", missingEvent, missingEventAt); err != nil {
		t.Fatal(err)
	}
	if decision := promoteDecision(t, s, n, now); decision.Allowed {
		t.Fatalf("missing event measurement passed: %s", decision.Summary())
	}

	fullAt := missingEventAt.Add(time.Minute)
	full := promoteObservation("2026.9.2", fullAt)
	stampCurrentWorkloadPolicy(t, s, "samplehub1", &full)
	modTime = fullAt
	full.Artifacts = []model.ArtifactCheck{{
		Unit: rule.Unit, Artifact: rule.Artifact, Exists: true, ModTime: &modTime,
	}}
	full.Events = []model.EventStream{{
		Unit: rule.Unit, Path: rule.Artifact,
		Declared: []model.EventSummary{{Type: "broken", Count: 0}},
	}}
	if err := s.RecordObservation("cnode", full, fullAt); err != nil {
		t.Fatal(err)
	}
	if decision := promoteDecision(t, s, n, fullAt); decision.Allowed || !strings.Contains(decision.Summary(), "policy") {
		t.Fatalf("policy change reused the old canary soak after one fresh batch: %s", decision.Summary())
	}
}

func TestExpectationPolicyABARequiresFreshObservationForNewGeneration(t *testing.T) {
	s := rolloutStore(t)
	n, _ := eligiblePromotionFixture(t, s)
	now := s.now()
	if decision := promoteDecision(t, s, n, now); !decision.Allowed {
		t.Fatalf("baseline locked: %s", decision.Summary())
	}
	oldAToken, err := s.CurrentWorkloadPolicyToken("samplehub1")
	if err != nil {
		t.Fatal(err)
	}

	rule := model.Expectation{
		Machine: "samplehub1", Unit: "proof.service", Artifact: "/tmp/proof.jsonl",
		MaxAgeSeconds: 3600, Why: "generation B",
		Events: &model.EventSpec{TsField: "at", TypeField: "kind", NotOK: []string{"broken"}, WindowSeconds: 3600},
	}
	s.SetExpectations(&expect.Set{Configured: true, Rules: []model.Expectation{rule}})
	if err := s.PublishExpectationsPolicy(now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if decision := promoteDecision(t, s, n, now); decision.Allowed {
		t.Fatalf("policy B reused policy A witness: %s", decision.Summary())
	}

	// 回到相同語意的 A 仍是一次新的發布世代；不能讓先前 A 的 token 復活。
	s.SetExpectations(&expect.Set{})
	if err := s.PublishExpectationsPolicy(now.Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	newAToken, err := s.CurrentWorkloadPolicyToken("samplehub1")
	if err != nil {
		t.Fatal(err)
	}
	if newAToken == oldAToken {
		t.Fatal("policy A→B→A reused the old A wire token")
	}
	if decision := promoteDecision(t, s, n, now); decision.Allowed {
		t.Fatalf("policy A→B→A revived the old A witness: %s", decision.Summary())
	}

	freshAt := now.Add(time.Minute)
	if err := s.RecordCheckin("cnode", model.Checkin{SentAt: freshAt, AgentStartedAt: now}, freshAt); err != nil {
		t.Fatal(err)
	}
	b := promoteObservation("2026.9.2", freshAt)
	stampCurrentWorkloadPolicy(t, s, "samplehub1", &b)
	if err := s.RecordObservation("cnode", b, freshAt); err != nil {
		t.Fatal(err)
	}
	if decision := promoteDecision(t, s, n, freshAt); decision.Allowed || !strings.Contains(decision.Summary(), "policy") {
		t.Fatalf("fresh new-A observation reused the pre-change canary soak: %s", decision.Summary())
	}
}

func TestPolicyGenerationChangesOnlyWhenPublishedIdentityChanges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.db")
	first, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	policyA := &expect.Set{}
	first.SetExpectations(policyA)
	base := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	if err := first.PublishExpectationsPolicy(base); err != nil {
		t.Fatal(err)
	}
	fingerprintA, generationA, validA := activePolicyIdentity(t, first)
	tokenA, err := first.CurrentWorkloadPolicyToken("samplehub1")
	if err != nil {
		t.Fatal(err)
	}
	if generationA != 1 || !validA || tokenA == "" {
		t.Fatalf("first identity fingerprint=%q generation=%d valid=%v token=%q", fingerprintA, generationA, validA, tokenA)
	}

	if err := first.PublishExpectationsPolicy(base.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	_, generationRepeat, _ := activePolicyIdentity(t, first)
	if generationRepeat != generationA {
		t.Fatalf("same-policy republish advanced generation: %d -> %d", generationA, generationRepeat)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	restarted, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	restarted.SetExpectations(policyA)
	if err := restarted.PublishExpectationsPolicy(base.Add(2 * time.Minute)); err != nil {
		t.Fatal(err)
	}
	_, generationRestart, _ := activePolicyIdentity(t, restarted)
	tokenRestart, err := restarted.CurrentWorkloadPolicyToken("samplehub1")
	if err != nil {
		t.Fatal(err)
	}
	if generationRestart != generationA || tokenRestart != tokenA {
		t.Fatalf("same-policy restart changed identity: generation=%d token=%q, want %d/%q",
			generationRestart, tokenRestart, generationA, tokenA)
	}

	restarted.SetExpectations(testWorkloadPolicy("B"))
	if err := restarted.PublishExpectationsPolicy(base.Add(3 * time.Minute)); err != nil {
		t.Fatal(err)
	}
	fingerprintB, generationB, validB := activePolicyIdentity(t, restarted)
	tokenB, err := restarted.CurrentWorkloadPolicyToken("samplehub1")
	if err != nil {
		t.Fatal(err)
	}
	if fingerprintB == fingerprintA || generationB != generationA+1 || !validB || tokenB == tokenA {
		t.Fatalf("semantic change did not rotate identity: fp=%q generation=%d valid=%v token=%q",
			fingerprintB, generationB, validB, tokenB)
	}

	// valid 是 persisted identity 的一部分，即使 fingerprint 沒變也要換代。
	if _, err := restarted.DB().Exec(`UPDATE active_workload_policy SET valid=0 WHERE singleton=1`); err != nil {
		t.Fatal(err)
	}
	if err := restarted.PublishExpectationsPolicy(base.Add(4 * time.Minute)); err != nil {
		t.Fatal(err)
	}
	_, generationValidRepair, validRepair := activePolicyIdentity(t, restarted)
	if generationValidRepair != generationB+1 || !validRepair {
		t.Fatalf("valid-only change did not advance generation: generation=%d valid=%v", generationValidRepair, validRepair)
	}
}

func TestOpenMigratesPersistedPolicyGeneration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	fingerprint, valid, err := expectationsPolicyFingerprint(&expect.Set{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(`DROP TABLE active_workload_policy`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(`CREATE TABLE active_workload_policy (
	 singleton INTEGER PRIMARY KEY CHECK(singleton=1),
	 expectations_fingerprint TEXT NOT NULL,
	 valid INTEGER NOT NULL CHECK(valid IN (0,1)),
	 updated_at TEXT NOT NULL
	)`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(`INSERT INTO active_workload_policy
	 (singleton,expectations_fingerprint,valid,updated_at) VALUES(1,?,?,?)`,
		fingerprint, valid, fmtTime(time.Now().UTC())); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	gotFingerprint, generation, gotValid := activePolicyIdentity(t, s)
	if gotFingerprint != fingerprint || generation != 1 || gotValid != valid {
		t.Fatalf("migrated identity fingerprint=%q generation=%d valid=%v", gotFingerprint, generation, gotValid)
	}
	s.SetExpectations(&expect.Set{})
	if token, err := s.CurrentWorkloadPolicyToken("samplehub1"); err != nil || token == "" {
		t.Fatalf("migrated active identity could not issue token: token=%q err=%v", token, err)
	}
}

func TestConcurrentPolicyPublishSerializesGenerationAcrossStores(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.db")
	seed, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer seed.Close()
	seed.SetExpectations(&expect.Set{})
	if err := seed.PublishExpectationsPolicy(time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	_, startGeneration, _ := activePolicyIdentity(t, seed)

	second, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	third, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer third.Close()
	policyB, policyC := testWorkloadPolicy("B"), testWorkloadPolicy("C")
	second.SetExpectations(policyB)
	third.SetExpectations(policyC)
	fingerprintB, _, _ := expectationsPolicyFingerprint(policyB)
	fingerprintC, _, _ := expectationsPolicyFingerprint(policyC)
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, publisher := range []*Store{second, third} {
		publisher := publisher
		go func() {
			<-start
			results <- publisher.PublishExpectationsPolicy(time.Now().UTC())
		}()
	}
	close(start)
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	fingerprint, generation, valid := activePolicyIdentity(t, seed)
	if generation != startGeneration+2 || !valid || (fingerprint != fingerprintB && fingerprint != fingerprintC) {
		t.Fatalf("concurrent publishes lost a transition: fingerprint=%q generation=%d valid=%v", fingerprint, generation, valid)
	}
	if _, err := seed.CurrentWorkloadPolicyToken("samplehub1"); !errors.Is(err, ErrWorkloadPolicyIdentityMismatch) {
		t.Fatalf("stale process memory still issued active token: %v", err)
	}
}

func TestProcessPolicyMismatchFailsClosedForIngestAndPromote(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.db")
	service, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	service.SetExpectations(&expect.Set{})
	if err := service.PublishExpectationsPolicy(rolloutTestEvidenceEpoch); err != nil {
		t.Fatal(err)
	}
	n, _ := eligiblePromotionFixture(t, service)
	now := service.now()

	publisher, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer publisher.Close()
	publisher.SetExpectations(testWorkloadPolicy("B"))
	if err := publisher.PublishExpectationsPolicy(now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := service.CurrentWorkloadPolicyToken("samplehub1"); !errors.Is(err, ErrWorkloadPolicyIdentityMismatch) {
		t.Fatalf("stale service issued token: %v", err)
	}
	if _, err := service.PromoteFacts("2026.9.2", n.Job.ArtifactDigest, now); !errors.Is(err, ErrWorkloadPolicyIdentityMismatch) {
		t.Fatalf("stale service produced promote facts: %v", err)
	}

	freshAt := now.Add(time.Minute)
	if err := service.RecordCheckin("cnode", model.Checkin{SentAt: freshAt, AgentStartedAt: now}, freshAt); err != nil {
		t.Fatal(err)
	}
	b := promoteObservation("2026.9.2", freshAt) // generation A token
	if err := service.RecordObservation("cnode", b, freshAt); err != nil {
		t.Fatal(err)
	}
	var verdict string
	var policyValid bool
	if err := service.DB().QueryRow(`SELECT verdict,policy_valid FROM workload_observation_witness WHERE machine_id='cnode'`).
		Scan(&verdict, &policyValid); err != nil {
		t.Fatal(err)
	}
	if verdict != workloadWitnessUnknown || policyValid {
		t.Fatalf("mismatched process made a health witness: verdict=%q policy_valid=%v", verdict, policyValid)
	}
}

func TestPersistedActivePolicyIsUsedBySeparateCLIStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.db")
	service, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	service.nowFn = func() time.Time { return now }
	rule := model.Expectation{
		Machine: "samplehub1", Unit: "proof.service", Artifact: "/tmp/proof",
		MaxAgeSeconds: 3600, Why: "policy window witness",
		Events: &model.EventSpec{TsField: "at", TypeField: "kind", NotOK: []string{"broken"}, WindowSeconds: 3600},
	}
	service.SetExpectations(&expect.Set{Configured: true, Rules: []model.Expectation{rule}})
	// This test isolates cross-process policy identity. Its canary finishes 48h
	// before now, so start the evidence recorder before that canary.
	if err := service.PublishExpectationsPolicy(now.Add(-72 * time.Hour)); err != nil {
		t.Fatal(err)
	}
	addRolloutMachine(t, service, "cnode", "samplehub1", false)
	if err := service.RecordCheckin("cnode", model.Checkin{SentAt: now, AgentStartedAt: now}, now); err != nil {
		t.Fatal(err)
	}
	b := promoteObservation("2026.9.2", now)
	stampCurrentWorkloadPolicy(t, service, "samplehub1", &b)
	modTime := now
	b.Artifacts = []model.ArtifactCheck{{Unit: rule.Unit, Artifact: rule.Artifact, Exists: true, ModTime: &modTime}}
	b.Events = []model.EventStream{{
		Unit: rule.Unit, Path: rule.Artifact,
		Declared: []model.EventSummary{{Type: "broken", Count: 0}},
	}}
	if err := service.RecordObservation("cnode", b, now); err != nil {
		t.Fatal(err)
	}
	digest := strings.Repeat("a", 64)
	canary, jobs := createPromoteDeployment(t, service, "canary", "2026.9.2", digest, "",
		[]NewDeploymentTarget{{MachineID: "cnode", BatchNo: 1}})
	finished := now.Add(-48 * time.Hour)
	setPromoteJobState(t, service, jobs["cnode"], deploy.Succeeded, finished)
	finishPromoteDeployment(t, service, canary.DeploymentID, DeploymentRunning, finished)
	// Keep the real current witness above (it includes this test's artifact/event
	// rule), but replace the pre-canary ledger row with an explicit covered soak.
	if _, err := service.DB().Exec(`DELETE FROM workload_observation_evidence WHERE machine_id='cnode'`); err != nil {
		t.Fatal(err)
	}
	seedPromoteCoverage(t, service, "cnode", "samplehub1", "2026.9.2", finished, now.Add(time.Second))

	// 這個 Store 模擬手動 CLI：它沒有 SetExpectations，也不會繼承
	// systemd EnvironmentFile，但必須跟 service 已發布到 DB 的 policy 一致。
	cli, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	cli.nowFn = func() time.Time { return now }
	facts, err := cli.PromoteFacts("2026.9.2", digest, now)
	if err != nil || !rollout.PromoteGate(facts, now, time.UTC).Allowed {
		t.Fatalf("separate CLI did not use DB active policy: facts=%+v err=%v", facts, err)
	}

	rule.Events.WindowSeconds = 24 * 60 * 60
	service.SetExpectations(&expect.Set{Configured: true, Rules: []model.Expectation{rule}})
	if err := service.PublishExpectationsPolicy(now); err != nil {
		t.Fatal(err)
	}
	facts, err = cli.PromoteFacts("2026.9.2", digest, now)
	if err != nil {
		t.Fatal(err)
	}
	if decision := rollout.PromoteGate(facts, now, time.UTC); decision.Allowed || !strings.Contains(decision.Summary(), "expectations") {
		t.Fatalf("CLI reused witness after service policy change: %s", decision.Summary())
	}

	// Agent 還沒收到 v2，重送的仍是 v1 rules + v1 token。Hub 不可以把
	// 自己記憶體裡的 v2 fingerprint 貼到這批舊量測上。
	if err := service.RecordObservation("cnode", b, now); err != nil {
		t.Fatal(err)
	}
	facts, err = cli.PromoteFacts("2026.9.2", digest, now)
	if err != nil {
		t.Fatal(err)
	}
	if decision := rollout.PromoteGate(facts, now, time.UTC); decision.Allowed {
		t.Fatalf("舊 policy token 的 batch 被當成 v2 witness: %s", decision.Summary())
	}

	nextAt := now.Add(time.Minute)
	if err := service.RecordCheckin("cnode", model.Checkin{SentAt: nextAt, AgentStartedAt: now}, nextAt); err != nil {
		t.Fatal(err)
	}
	b.MeasuredAt = nextAt
	stampCurrentWorkloadPolicy(t, service, "samplehub1", &b)
	if err := service.RecordObservation("cnode", b, nextAt); err != nil {
		t.Fatal(err)
	}
	facts, err = cli.PromoteFacts("2026.9.2", digest, nextAt)
	if err != nil {
		t.Fatal(err)
	}
	if decision := rollout.PromoteGate(facts, nextAt, time.UTC); decision.Allowed || !strings.Contains(decision.Summary(), "policy") {
		t.Fatalf("CLI reused the old canary soak after policy change: %s facts=%+v", decision.Summary(), facts)
	}
}

func TestMachineRenameInvalidatesWorkloadPolicyWitness(t *testing.T) {
	s := rolloutStore(t)
	n, _ := eligiblePromotionFixture(t, s)
	now := s.now()
	if decision := promoteDecision(t, s, n, now); !decision.Allowed {
		t.Fatalf("baseline locked: %s", decision.Summary())
	}
	oldToken, err := s.CurrentWorkloadPolicyToken("samplehub1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(`UPDATE machine_registry SET display_name='cnode-renamed' WHERE machine_id='cnode'`); err != nil {
		t.Fatal(err)
	}
	if decision := promoteDecision(t, s, n, now); decision.Allowed || !strings.Contains(decision.Summary(), "expect") {
		t.Fatalf("rename reused old-name policy witness: %s", decision.Summary())
	}

	stale := promoteObservation("2026.9.2", now)
	stale.WorkloadPolicyToken = oldToken
	if err := s.RecordObservation("cnode", stale, now); err != nil {
		t.Fatal(err)
	}
	if decision := promoteDecision(t, s, n, now); decision.Allowed {
		t.Fatalf("old-name token submitted after rename unlocked gate: %s", decision.Summary())
	}

	freshAt := now.Add(time.Minute)
	if err := s.RecordCheckin("cnode", model.Checkin{SentAt: freshAt, AgentStartedAt: now}, freshAt); err != nil {
		t.Fatal(err)
	}
	stale.MeasuredAt = freshAt
	stampCurrentWorkloadPolicy(t, s, "cnode-renamed", &stale)
	if err := s.RecordObservation("cnode", stale, freshAt); err != nil {
		t.Fatal(err)
	}
	if decision := promoteDecision(t, s, n, freshAt); decision.Allowed || !strings.Contains(decision.Summary(), "policy") {
		t.Fatalf("rename reused evidence gathered under the old policy identity: %s", decision.Summary())
	}
}

func TestInstallProcessEvidenceMustBeCoherentBeforeFailureCloses(t *testing.T) {
	s := rolloutStore(t)
	addRolloutMachine(t, s, "cnode", "samplehub1", false)
	base := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	if err := s.RecordCheckin("cnode", model.Checkin{SentAt: base, AgentStartedAt: base}, base); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordCheckin("cnode", model.Checkin{SentAt: base, AgentStartedAt: base}, base); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordCanarySilentFailure("cnode", "existing failure", base); err != nil {
		t.Fatal(err)
	}

	for i, mutate := range []func(*model.ObservationBatch){
		func(b *model.ObservationBatch) { b.OpenClaw.Install = nil },
		func(b *model.ObservationBatch) { b.OpenClaw.Install.ProcessMatchesUnit = nil },
		func(b *model.ObservationBatch) { b.OpenClaw.Install.RunningDirVersion = "" },
	} {
		at := base.Add(time.Duration(i+1) * time.Minute)
		b := promoteObservation("2026.9.2", at)
		mutate(&b)
		if err := s.RecordObservation("cnode", b, at); err != nil {
			t.Fatal(err)
		}
		assertOpenFailureAt(t, s, base, "incomplete install/process witness")
	}

	healthyAt := base.Add(4 * time.Minute)
	if err := s.RecordCheckin("cnode", model.Checkin{SentAt: healthyAt, AgentStartedAt: base}, healthyAt); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordObservation("cnode", promoteObservation("2026.9.2", healthyAt), healthyAt); err != nil {
		t.Fatal(err)
	}
	var open int
	var last string
	if err := s.DB().QueryRow(`SELECT last_seen_at,open FROM canary_silent_failures WHERE machine_id='cnode'`).Scan(&last, &open); err != nil || open != 0 || last != fmtTime(healthyAt) {
		t.Fatalf("complete install/process witness did not close: open=%d last=%q err=%v", open, last, err)
	}
}

func TestExplicitProcessMismatchOpensHistoricalFailureAndRecoveryCannotEraseIt(t *testing.T) {
	s := rolloutStore(t)
	n, canaryFinished := eligiblePromotionFixture(t, s)
	now := s.now()
	failureAt := now.Add(time.Minute)
	bad := promoteObservation("2026.9.2", failureAt)
	no := false
	bad.OpenClaw.Install.ProcessMatchesUnit = &no
	bad.OpenClaw.Install.ProcessReason = "unit 改成新 release，但舊 process 還活著"
	if err := s.RecordObservation("cnode", bad, failureAt); err != nil {
		t.Fatal(err)
	}
	var first, last, reason string
	var open int
	if err := s.DB().QueryRow(`SELECT first_seen_at,last_seen_at,reason,open FROM canary_silent_failures WHERE machine_id='cnode'`).Scan(&first, &last, &reason, &open); err != nil ||
		first != fmtTime(failureAt) || last != fmtTime(failureAt) || open != 1 || !strings.Contains(reason, "process") {
		t.Fatalf("explicit process mismatch did not open span: first=%q last=%q reason=%q open=%d err=%v", first, last, reason, open, err)
	}

	recoveryAt := failureAt.Add(time.Minute)
	if err := s.RecordCheckin("cnode", model.Checkin{SentAt: recoveryAt, AgentStartedAt: canaryFinished}, recoveryAt); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordObservation("cnode", promoteObservation("2026.9.2", recoveryAt), recoveryAt); err != nil {
		t.Fatal(err)
	}
	if err := s.DB().QueryRow(`SELECT last_seen_at,open FROM canary_silent_failures WHERE machine_id='cnode'`).Scan(&last, &open); err != nil || last != fmtTime(recoveryAt) || open != 0 {
		t.Fatalf("coherent recovery did not close mismatch interval: last=%q open=%d err=%v", last, open, err)
	}

	digest := strings.TrimPrefix(n.Job.ArtifactDigest, "sha256:")
	decisionAt := recoveryAt.Add(time.Minute)
	facts, err := s.PromoteFacts("2026.9.2", digest, decisionAt)
	if err != nil || len(facts.Silent) == 0 {
		t.Fatalf("closed process mismatch disappeared from canary window: silent=%+v err=%v", facts.Silent, err)
	}
	if decision := rollout.PromoteGate(facts, decisionAt, time.UTC); decision.Allowed || !strings.Contains(decision.Summary(), "沉默失敗") {
		t.Fatalf("recovered process mismatch erased canary history: %s", decision.Summary())
	}
}

func TestStalePolicyCannotJudgeArtifactsButStillRecordsCoreFailure(t *testing.T) {
	s := rolloutStore(t)
	addRolloutMachine(t, s, "cnode", "samplehub1", false)
	base := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	if err := s.RecordCheckin("cnode", model.Checkin{SentAt: base, AgentStartedAt: base}, base); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordCanarySilentFailure("cnode", "existing failure", base); err != nil {
		t.Fatal(err)
	}
	oldToken, err := s.CurrentWorkloadPolicyToken("samplehub1")
	if err != nil {
		t.Fatal(err)
	}
	rule := model.Expectation{
		Machine: "samplehub1", Unit: "new.service", Artifact: "/tmp/new.jsonl", MaxAgeSeconds: 3600, Why: "policy B",
		Events: &model.EventSpec{TsField: "at", TypeField: "kind", NotOK: []string{"broken"}, WindowSeconds: 3600},
	}
	s.SetExpectations(&expect.Set{Configured: true, Rules: []model.Expectation{rule}})

	// 舊 token 的 artifact/event 明確 hit 是用舊規則量的，不能按 current B
	// 開或延長 span；projection 必須以 unknown 遮掉舊 healthy。
	artifactBadAt := base.Add(time.Minute)
	artifactBad := promoteObservation("2026.9.2", artifactBadAt)
	artifactBad.WorkloadPolicyToken = oldToken
	modTime := artifactBadAt
	lastAt := artifactBadAt
	artifactBad.Artifacts = []model.ArtifactCheck{{Unit: rule.Unit, Artifact: rule.Artifact, Exists: true, ModTime: &modTime}}
	artifactBad.Events = []model.EventStream{{
		Unit: rule.Unit, Path: rule.Artifact,
		Declared: []model.EventSummary{{Type: "broken", Count: 1, LastAt: &lastAt}},
	}}
	if err := s.RecordObservation("cnode", artifactBad, artifactBadAt); err != nil {
		t.Fatal(err)
	}
	assertOpenFailureAt(t, s, base, "stale policy artifact failure")
	var verdict string
	var open int
	if err := s.DB().QueryRow(`SELECT verdict FROM workload_observation_witness WHERE machine_id='cnode'`).Scan(&verdict); err != nil || verdict != workloadWitnessUnknown {
		t.Fatalf("stale artifact verdict=%q err=%v, want unknown", verdict, err)
	}

	// 同一個舊 token batch 的 PID=0 是 policy-independent L1，仍必須延長。
	coreBadAt := base.Add(2 * time.Minute)
	coreBad := promoteObservation("2026.9.2", coreBadAt)
	coreBad.WorkloadPolicyToken = oldToken
	coreBad.CLITools[len(coreBad.CLITools)-1].RunningPID = 0
	coreBad.CLITools[len(coreBad.CLITools)-1].RunningReason = "explicit core process failure"
	if err := s.RecordObservation("cnode", coreBad, coreBadAt); err != nil {
		t.Fatal(err)
	}
	var last string
	if err := s.DB().QueryRow(`SELECT last_seen_at,open FROM canary_silent_failures WHERE machine_id='cnode'`).Scan(&last, &open); err != nil || last != fmtTime(coreBadAt) || open != 1 {
		t.Fatalf("stale token hid core failure: last=%q open=%d err=%v", last, open, err)
	}
	if err := s.DB().QueryRow(`SELECT verdict FROM workload_observation_witness WHERE machine_id='cnode'`).Scan(&verdict); err != nil || verdict != workloadWitnessFailure {
		t.Fatalf("core failure verdict=%q err=%v, want failure", verdict, err)
	}
}

func TestDuplicateBatchMeasurementsNeverBecomeHealthyWitness(t *testing.T) {
	rule := model.Expectation{
		Machine: "samplehub1", Unit: "proof.service", Artifact: "/tmp/proof.jsonl", MaxAgeSeconds: 3600, Why: "proof",
		Events: &model.EventSpec{TsField: "at", TypeField: "kind", NotOK: []string{"broken"}, WindowSeconds: 3600},
	}
	base := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	modTime := base
	lastAt := base
	goodArtifact := model.ArtifactCheck{Unit: rule.Unit, Artifact: rule.Artifact, Exists: true, ModTime: &modTime}
	badArtifact := model.ArtifactCheck{Unit: rule.Unit, Artifact: rule.Artifact, Exists: false}
	goodEvent := model.EventStream{Unit: rule.Unit, Path: rule.Artifact, Declared: []model.EventSummary{{Type: "broken", Count: 0}}}
	badEvent := model.EventStream{Unit: rule.Unit, Path: rule.Artifact, Declared: []model.EventSummary{{Type: "broken", Count: 1, LastAt: &lastAt}}}

	tests := []struct {
		name        string
		artifacts   []model.ArtifactCheck
		events      []model.EventStream
		cli         []model.CLITool
		wantFailure bool
	}{
		{"artifact bad then good", []model.ArtifactCheck{badArtifact, goodArtifact}, []model.EventStream{goodEvent}, nil, false},
		{"artifact good then bad", []model.ArtifactCheck{goodArtifact, badArtifact}, []model.EventStream{goodEvent}, nil, false},
		{"event bad then good", []model.ArtifactCheck{goodArtifact}, []model.EventStream{badEvent, goodEvent}, nil, false},
		{"event good then bad", []model.ArtifactCheck{goodArtifact}, []model.EventStream{goodEvent, badEvent}, nil, false},
		{"cli bad then good", []model.ArtifactCheck{goodArtifact}, []model.EventStream{goodEvent}, []model.CLITool{
			{Name: "openclaw", Present: true, RunningReason: "not running"}, {Name: "openclaw", Present: true, RunningPID: 42},
		}, true},
		{"cli good then bad", []model.ArtifactCheck{goodArtifact}, []model.EventStream{goodEvent}, []model.CLITool{
			{Name: "openclaw", Present: true, RunningPID: 42}, {Name: "openclaw", Present: true, RunningReason: "not running"},
		}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := rolloutStore(t)
			addRolloutMachine(t, s, "cnode", "samplehub1", false)
			s.SetExpectations(&expect.Set{Configured: true, Rules: []model.Expectation{rule}})
			if err := s.PublishExpectationsPolicy(base); err != nil {
				t.Fatal(err)
			}
			if err := s.RecordCheckin("cnode", model.Checkin{SentAt: base, AgentStartedAt: base}, base); err != nil {
				t.Fatal(err)
			}
			b := promoteObservation("2026.9.2", base)
			stampCurrentWorkloadPolicy(t, s, "samplehub1", &b)
			b.Artifacts, b.Events = tc.artifacts, tc.events
			if tc.cli != nil {
				b.CLITools = tc.cli
			}
			if err := s.RecordObservation("cnode", b, base); err != nil {
				t.Fatal(err)
			}
			var verdict string
			if err := s.DB().QueryRow(`SELECT verdict FROM workload_observation_witness WHERE machine_id='cnode'`).Scan(&verdict); err != nil {
				t.Fatal(err)
			}
			if verdict == workloadWitnessHealthy {
				t.Fatal("duplicate batch became healthy witness")
			}
			if tc.wantFailure && verdict != workloadWitnessFailure {
				t.Fatalf("duplicate CLI hid explicit PID=0: verdict=%q", verdict)
			}
		})
	}
}

func TestDuplicateRunningCLIMeasurementsCannotCloseOpenFailure(t *testing.T) {
	base := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	at := base.Add(time.Minute)
	tests := []struct {
		name               string
		cli                []model.CLITool
		wantVerdict        string
		wantOpen           int
		wantLast           time.Time
		verdictConsequence string
		spanConsequence    string
	}{
		{
			name:        "single running CLI is a coherent recovery witness",
			cli:         []model.CLITool{{Name: "openclaw", Present: true, RunningPID: 42}},
			wantVerdict: workloadWitnessHealthy,
			wantOpen:    0,
			wantLast:    at,
			verdictConsequence: "operator would keep treating a fully coherent single-process observation as uncertain " +
				"instead of recognizing recovery",
			spanConsequence: "operator would keep a recovered machine under failure monitoring and out of service despite " +
				"a coherent single-process witness",
		},
		{
			name: "duplicate running CLIs are not a coherent recovery witness",
			cli: []model.CLITool{
				{Name: "openclaw", Present: true, RunningPID: 42},
				{Name: "openclaw", Present: true, RunningPID: 84},
			},
			wantVerdict: workloadWitnessUnknown,
			wantOpen:    1,
			wantLast:    base,
			verdictConsequence: "operator would treat this machine as recovered, stop watching it, and return it to service, " +
				"even though two openclaw rows in one batch cannot identify which process is authoritative",
			spanConsequence: "operator would close the failure span, stop watching this machine, and return it to service, " +
				"even though two openclaw rows in one batch cannot identify which process is authoritative",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := rolloutStore(t)
			addRolloutMachine(t, s, "cnode", "samplehub1", false)
			if err := s.RecordCheckin("cnode", model.Checkin{SentAt: at, AgentStartedAt: base}, at); err != nil {
				t.Fatal(err)
			}
			if _, err := s.RecordCanarySilentFailure("cnode", "existing failure", base); err != nil {
				t.Fatal(err)
			}
			b := promoteObservation("2026.9.2", at)
			b.CLITools = tc.cli
			if err := s.RecordObservation("cnode", b, at); err != nil {
				t.Fatal(err)
			}

			var verdict string
			if err := s.DB().QueryRow(`SELECT verdict FROM workload_observation_witness WHERE machine_id='cnode'`).Scan(&verdict); err != nil {
				t.Fatal(err)
			}
			if verdict != tc.wantVerdict {
				t.Errorf("verdict got %q, expected %q: %s", verdict, tc.wantVerdict, tc.verdictConsequence)
			}

			var last string
			var open int
			if err := s.DB().QueryRow(`SELECT last_seen_at,open FROM canary_silent_failures WHERE machine_id='cnode'`).Scan(&last, &open); err != nil {
				t.Fatal(err)
			}
			if open != tc.wantOpen || last != fmtTime(tc.wantLast) {
				t.Errorf("failure span got open=%d last=%q, expected open=%d last=%q: %s",
					open, last, tc.wantOpen, fmtTime(tc.wantLast), tc.spanConsequence)
			}
		})
	}
}

func TestExplicitCoreFailureUsesHubReceiptDespiteEvidenceOrdering(t *testing.T) {
	base := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name       string
		measuredAt time.Time
		untrusted  bool
	}{
		{name: "older than watermark", measuredAt: base.Add(-30 * time.Second)},
		{name: "same second as watermark", measuredAt: base},
		{name: "untrusted clock", measuredAt: base.Add(2 * time.Minute), untrusted: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := rolloutStore(t)
			addRolloutMachine(t, s, "cnode", "samplehub1", false)
			if err := s.RecordCheckin("cnode", model.Checkin{SentAt: base, AgentStartedAt: base}, base); err != nil {
				t.Fatal(err)
			}
			if err := s.RecordObservation("cnode", promoteObservation("2026.9.2", base), base); err != nil {
				t.Fatal(err)
			}

			receivedAt := base.Add(2 * time.Minute)
			checkinSentAt := receivedAt
			if tc.untrusted {
				checkinSentAt = receivedAt.Add(state.ClockSkewTolerance + time.Second)
			}
			if err := s.RecordCheckin("cnode", model.Checkin{SentAt: checkinSentAt, AgentStartedAt: base}, receivedAt); err != nil {
				t.Fatal(err)
			}
			bad := promoteObservation("2026.9.2", tc.measuredAt)
			bad.CLITools[0].RunningPID = 0
			bad.CLITools[0].RunningReason = "明確掃過 process，沒有找到 OpenClaw"
			if err := s.RecordObservation("cnode", bad, receivedAt); err != nil {
				t.Fatal(err)
			}
			assertOpenFailureAt(t, s, receivedAt, tc.name)
			var verdict, evidence string
			if err := s.DB().QueryRow(`SELECT verdict,evidence_at FROM workload_observation_witness WHERE machine_id='cnode'`).
				Scan(&verdict, &evidence); err != nil || verdict != workloadWitnessFailure || evidence != fmtTime(base) {
				t.Fatalf("core failure verdict=%q evidence=%q err=%v", verdict, evidence, err)
			}
		})
	}
}

func TestPolicyArtifactFailureRequiresMatchingTokenAndCoherentMeasurement(t *testing.T) {
	base := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	rule := model.Expectation{
		Machine: "samplehub1", Unit: "proof.service", Artifact: "/tmp/proof",
		MaxAgeSeconds: 3600, Why: "policy evidence",
	}
	tests := []struct {
		name      string
		badToken  bool
		untrusted bool
		wantFail  bool
	}{
		{name: "matching token and coherent delayed evidence", wantFail: true},
		{name: "wrong token", badToken: true},
		{name: "untrusted clock", untrusted: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := rolloutStore(t)
			addRolloutMachine(t, s, "cnode", "samplehub1", false)
			s.SetExpectations(&expect.Set{Configured: true, Rules: []model.Expectation{rule}})
			if err := s.PublishExpectationsPolicy(base); err != nil {
				t.Fatal(err)
			}
			if err := s.RecordCheckin("cnode", model.Checkin{SentAt: base, AgentStartedAt: base}, base); err != nil {
				t.Fatal(err)
			}
			good := promoteObservation("2026.9.2", base)
			stampCurrentWorkloadPolicy(t, s, "samplehub1", &good)
			modTime := base
			good.Artifacts = []model.ArtifactCheck{{Unit: rule.Unit, Artifact: rule.Artifact, Exists: true, ModTime: &modTime}}
			if err := s.RecordObservation("cnode", good, base); err != nil {
				t.Fatal(err)
			}

			receivedAt := base.Add(2 * time.Minute)
			checkinSentAt := receivedAt
			if tc.untrusted {
				checkinSentAt = receivedAt.Add(state.ClockSkewTolerance + time.Second)
			}
			if err := s.RecordCheckin("cnode", model.Checkin{SentAt: checkinSentAt, AgentStartedAt: base}, receivedAt); err != nil {
				t.Fatal(err)
			}
			bad := promoteObservation("2026.9.2", base.Add(-30*time.Second))
			stampCurrentWorkloadPolicy(t, s, "samplehub1", &bad)
			if tc.badToken {
				bad.WorkloadPolicyToken = "wrong-generation"
			}
			bad.Artifacts = []model.ArtifactCheck{{Unit: rule.Unit, Artifact: rule.Artifact, Exists: false}}
			if err := s.RecordObservation("cnode", bad, receivedAt); err != nil {
				t.Fatal(err)
			}
			var verdict string
			if err := s.DB().QueryRow(`SELECT verdict FROM workload_observation_witness WHERE machine_id='cnode'`).Scan(&verdict); err != nil {
				t.Fatal(err)
			}
			failures := countRows(t, s, `SELECT COUNT(*) FROM canary_silent_failures WHERE machine_id='cnode'`)
			if tc.wantFail {
				if verdict != workloadWitnessFailure || failures != 1 {
					t.Fatalf("coherent matching artifact failure lost: verdict=%q failures=%d", verdict, failures)
				}
				assertOpenFailureAt(t, s, receivedAt, tc.name)
			} else if verdict != workloadWitnessUnknown || failures != 0 {
				t.Fatalf("unqualified artifact evidence became failure: verdict=%q failures=%d", verdict, failures)
			}
		})
	}
}

func TestDatabaseObservationDistinguishesAbsentNeverRanAndUnknown(t *testing.T) {
	at := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	freshTask := at.Add(-time.Minute)
	zeroTask := time.Time{}
	futureTask := at.Add(state.ClockSkewTolerance + time.Second)
	tests := []struct {
		name        string
		db          *model.OpenClawDB
		wantVerdict string
		wantFailure bool
	}{
		{name: "missing measurement", db: nil, wantVerdict: workloadWitnessUnknown},
		{name: "read error stays unknown despite contradictory fields", db: &model.OpenClawDB{Present: true, Reason: "permission denied while reading", TaskRunRows: 0, LastTaskEndedAt: &freshTask}, wantVerdict: workloadWitnessUnknown},
		{name: "database absent", db: &model.OpenClawDB{Present: false, Reason: "no database at known paths"}, wantVerdict: workloadWitnessFailure, wantFailure: true},
		{name: "database present but never ran", db: &model.OpenClawDB{Present: true, TaskRunRows: 0}, wantVerdict: workloadWitnessFailure, wantFailure: true},
		{name: "zero rows cannot borrow fresh last task", db: &model.OpenClawDB{Present: true, TaskRunRows: 0, LastTaskEndedAt: &freshTask}, wantVerdict: workloadWitnessFailure, wantFailure: true},
		{name: "negative rows are invalid", db: &model.OpenClawDB{Present: true, TaskRunRows: -1, LastTaskEndedAt: &freshTask}, wantVerdict: workloadWitnessUnknown},
		{name: "positive rows without last task", db: &model.OpenClawDB{Present: true, TaskRunRows: 1}, wantVerdict: workloadWitnessUnknown},
		{name: "positive rows with zero last task", db: &model.OpenClawDB{Present: true, TaskRunRows: 1, LastTaskEndedAt: &zeroTask}, wantVerdict: workloadWitnessUnknown},
		{name: "positive rows with implausibly future task", db: &model.OpenClawDB{Present: true, TaskRunRows: 1, LastTaskEndedAt: &futureTask}, wantVerdict: workloadWitnessUnknown},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := rolloutStore(t)
			addRolloutMachine(t, s, "cnode", "samplehub1", false)
			if err := s.RecordCheckin("cnode", model.Checkin{SentAt: at, AgentStartedAt: at}, at); err != nil {
				t.Fatal(err)
			}
			b := promoteObservation("2026.9.2", at)
			b.OpenClaw.DB = tc.db
			if err := s.RecordObservation("cnode", b, at); err != nil {
				t.Fatal(err)
			}
			var verdict string
			if err := s.DB().QueryRow(`SELECT verdict FROM workload_observation_witness WHERE machine_id='cnode'`).Scan(&verdict); err != nil {
				t.Fatal(err)
			}
			failures := countRows(t, s, `SELECT COUNT(*) FROM canary_silent_failures WHERE machine_id='cnode'`)
			if verdict != tc.wantVerdict || (failures > 0) != tc.wantFailure {
				t.Fatalf("verdict=%q failures=%d, want %q failure=%v", verdict, failures, tc.wantVerdict, tc.wantFailure)
			}
		})
	}
}

func TestContradictoryDatabaseEvidenceCannotCloseOpenFailure(t *testing.T) {
	base := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	at := base.Add(time.Minute)
	freshTask := at.Add(-time.Minute)
	zeroTask := time.Time{}
	futureTask := at.Add(state.ClockSkewTolerance + time.Second)
	tests := []struct {
		name        string
		db          *model.OpenClawDB
		wantVerdict string
		wantLast    time.Time
	}{
		{name: "zero rows with fresh last is explicit failure", db: &model.OpenClawDB{Present: true, TaskRunRows: 0, LastTaskEndedAt: &freshTask}, wantVerdict: workloadWitnessFailure, wantLast: at},
		{name: "read error does not close despite contradictory fields", db: &model.OpenClawDB{Present: true, Reason: "sqlite read error", TaskRunRows: 0, LastTaskEndedAt: &freshTask}, wantVerdict: workloadWitnessUnknown, wantLast: base},
		{name: "read error does not close even when rows look usable", db: &model.OpenClawDB{Present: true, Reason: "sqlite read error", TaskRunRows: 1, LastTaskEndedAt: &freshTask}, wantVerdict: workloadWitnessUnknown, wantLast: base},
		{name: "negative rows with fresh last is unknown", db: &model.OpenClawDB{Present: true, TaskRunRows: -1, LastTaskEndedAt: &freshTask}, wantVerdict: workloadWitnessUnknown, wantLast: base},
		{name: "positive rows without last is unknown", db: &model.OpenClawDB{Present: true, TaskRunRows: 1}, wantVerdict: workloadWitnessUnknown, wantLast: base},
		{name: "positive rows with zero last is unknown", db: &model.OpenClawDB{Present: true, TaskRunRows: 1, LastTaskEndedAt: &zeroTask}, wantVerdict: workloadWitnessUnknown, wantLast: base},
		{name: "positive rows with future last is unknown", db: &model.OpenClawDB{Present: true, TaskRunRows: 1, LastTaskEndedAt: &futureTask}, wantVerdict: workloadWitnessUnknown, wantLast: base},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := rolloutStore(t)
			addRolloutMachine(t, s, "cnode", "samplehub1", false)
			if err := s.RecordCheckin("cnode", model.Checkin{SentAt: at, AgentStartedAt: base}, at); err != nil {
				t.Fatal(err)
			}
			if _, err := s.RecordCanarySilentFailure("cnode", "existing failure", base); err != nil {
				t.Fatal(err)
			}
			b := promoteObservation("2026.9.2", at)
			b.OpenClaw.DB = tc.db
			if err := s.RecordObservation("cnode", b, at); err != nil {
				t.Fatal(err)
			}
			var verdict string
			if err := s.DB().QueryRow(`SELECT verdict FROM workload_observation_witness WHERE machine_id='cnode'`).Scan(&verdict); err != nil {
				t.Fatal(err)
			}
			if verdict != tc.wantVerdict {
				t.Fatalf("verdict=%q, want %q", verdict, tc.wantVerdict)
			}
			assertOpenFailureAt(t, s, tc.wantLast, tc.name)
		})
	}
}

func TestDelayedOrFutureMeasuredBatchCannotRefreshWorkloadWitness(t *testing.T) {
	s := rolloutStore(t)
	addRolloutMachine(t, s, "cnode", "samplehub1", false)
	base := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	if _, err := s.RecordCanarySilentFailure("cnode", "existing failure", base); err != nil {
		t.Fatal(err)
	}
	receivedAt := base.Add(10 * time.Minute)
	if err := s.RecordCheckin("cnode", model.Checkin{SentAt: receivedAt, AgentStartedAt: base}, receivedAt); err != nil {
		t.Fatal(err)
	}
	for _, measuredAt := range []time.Time{base, receivedAt.Add(state.ClockSkewTolerance + time.Second), time.Time{}} {
		b := promoteObservation("2026.9.2", measuredAt)
		if measuredAt.IsZero() {
			// promoteObservation derives other timestamps from its argument; retain
			// complete-looking payload while making the batch timestamp absent.
			task := receivedAt.Add(-time.Minute)
			b.OpenClaw.DB.LastTaskEndedAt = &task
		}
		if err := s.RecordObservation("cnode", b, receivedAt); err != nil {
			t.Fatal(err)
		}
		assertOpenFailureAt(t, s, base, "non-coherent measured_at")
		var verdict string
		if err := s.DB().QueryRow(`SELECT verdict FROM workload_observation_witness WHERE machine_id='cnode'`).Scan(&verdict); err != nil || verdict != workloadWitnessUnknown {
			t.Fatalf("measured=%v verdict=%q err=%v, want unknown", measuredAt, verdict, err)
		}
	}
}

func TestWorkloadEvidenceAtUsesTrustedCheckinSkew(t *testing.T) {
	s := rolloutStore(t)
	addRolloutMachine(t, s, "cnode", "samplehub1", false)
	hubCheckinAt := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	agentSkew := 80 * time.Second
	if err := s.RecordCheckin("cnode", model.Checkin{
		SentAt: hubCheckinAt.Add(agentSkew), AgentStartedAt: hubCheckinAt.Add(agentSkew),
	}, hubCheckinAt); err != nil {
		t.Fatal(err)
	}
	hubEvidenceAt := hubCheckinAt.Add(time.Minute)
	b := promoteObservation("2026.9.2", hubEvidenceAt.Add(agentSkew))
	if err := s.RecordObservation("cnode", b, hubEvidenceAt); err != nil {
		t.Fatal(err)
	}
	var verdict, evidence string
	if err := s.DB().QueryRow(`SELECT verdict,evidence_at FROM workload_observation_witness WHERE machine_id='cnode'`).Scan(&verdict, &evidence); err != nil ||
		verdict != workloadWitnessHealthy || evidence != fmtTime(hubEvidenceAt) {
		t.Fatalf("+80s clock was tolerated instead of normalized: verdict=%q evidence=%q want=%q err=%v", verdict, evidence, fmtTime(hubEvidenceAt), err)
	}
}

func TestReplayOrOutOfOrderHealthyEvidenceCannotCloseNewerFailure(t *testing.T) {
	s := rolloutStore(t)
	addRolloutMachine(t, s, "cnode", "samplehub1", false)
	base := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	if err := s.RecordCheckin("cnode", model.Checkin{SentAt: base, AgentStartedAt: base}, base); err != nil {
		t.Fatal(err)
	}
	first := promoteObservation("2026.9.2", base)
	if err := s.RecordObservation("cnode", first, base); err != nil {
		t.Fatal(err)
	}
	failureAt := base.Add(30 * time.Second)
	if _, err := s.RecordCanarySilentFailure("cnode", "failure after measured T0", failureAt); err != nil {
		t.Fatal(err)
	}

	// T0 的 exact replay 在 T0+60 才送達；它既不比 witness watermark 新，
	// evidence time 也早於 T0+30 的 failure，兩條規則任一條都必須擋住 recovery。
	replayReceivedAt := base.Add(time.Minute)
	if err := s.RecordObservation("cnode", first, replayReceivedAt); err != nil {
		t.Fatal(err)
	}
	assertOpenFailureAt(t, s, failureAt, "exact replay before failure")
	var verdict, evidence string
	if err := s.DB().QueryRow(`SELECT verdict,evidence_at FROM workload_observation_witness WHERE machine_id='cnode'`).Scan(&verdict, &evidence); err != nil ||
		verdict != workloadWitnessUnknown || evidence != fmtTime(base) {
		t.Fatalf("replay refreshed witness: verdict=%q evidence=%q err=%v", verdict, evidence, err)
	}

	freshAt := base.Add(90 * time.Second)
	if err := s.RecordCheckin("cnode", model.Checkin{SentAt: freshAt, AgentStartedAt: base}, freshAt); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordObservation("cnode", promoteObservation("2026.9.2", freshAt), freshAt); err != nil {
		t.Fatal(err)
	}
	var open int
	var last string
	if err := s.DB().QueryRow(`SELECT last_seen_at,open FROM canary_silent_failures WHERE machine_id='cnode'`).Scan(&last, &open); err != nil || open != 0 || last != fmtTime(freshAt) {
		t.Fatalf("genuinely newer measurement did not recover: last=%q open=%d err=%v", last, open, err)
	}
}

func TestTransientExplicitOpenClawAbsenceIsPreservedInCanaryHistory(t *testing.T) {
	s := rolloutStore(t)
	n, _ := eligiblePromotionFixture(t, s)
	now := s.now()
	absentAt := now.Add(time.Minute)
	absent := promoteObservation("2026.9.2", absentAt)
	absent.OpenClaw.Present = false
	absent.OpenClaw.Reason = "~/.openclaw 不存在"
	if err := s.RecordObservation("cnode", absent, absentAt); err != nil {
		t.Fatal(err)
	}
	var reason string
	var open int
	if err := s.DB().QueryRow(`SELECT reason,open FROM canary_silent_failures WHERE machine_id='cnode'`).Scan(&reason, &open); err != nil || open != 1 || !strings.Contains(reason, "不存在") {
		t.Fatalf("explicit OpenClaw absence did not open span: reason=%q open=%d err=%v", reason, open, err)
	}

	recoveryAt := absentAt.Add(time.Minute)
	if err := s.RecordCheckin("cnode", model.Checkin{SentAt: recoveryAt, AgentStartedAt: now}, recoveryAt); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordObservation("cnode", promoteObservation("2026.9.2", recoveryAt), recoveryAt); err != nil {
		t.Fatal(err)
	}
	digest := strings.TrimPrefix(n.Job.ArtifactDigest, "sha256:")
	decisionAt := recoveryAt.Add(time.Minute)
	facts, err := s.PromoteFacts("2026.9.2", digest, decisionAt)
	if err != nil || len(facts.Silent) == 0 {
		t.Fatalf("transient absence disappeared after recovery: silent=%+v err=%v", facts.Silent, err)
	}
	if decision := rollout.PromoteGate(facts, decisionAt, time.UTC); decision.Allowed || !strings.Contains(decision.Summary(), "沉默失敗") {
		t.Fatalf("transient absence did not keep canary locked: %s", decision.Summary())
	}
}

func TestOpenMigratesEarlierWorkloadWitnessFailClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	addRolloutMachine(t, s, "cnode", "samplehub1", false)
	if _, err := s.DB().Exec(`DROP TABLE workload_observation_witness`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(`CREATE TABLE workload_observation_witness (
	 machine_id TEXT PRIMARY KEY, verdict TEXT NOT NULL, received_at TEXT NOT NULL,
	 running_version TEXT NOT NULL, policy_valid INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(`INSERT INTO workload_observation_witness
	 (machine_id,verdict,received_at,running_version,policy_valid) VALUES('cnode','healthy',?,'2026.9.2',1)`,
		fmtTime(time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC))); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	columns, err := columnSet(s.DB(), "workload_observation_witness")
	if err != nil || !columns["evidence_at"] || !columns["openclaw_present"] || !columns["workload_policy_token"] {
		t.Fatalf("witness migration columns=%v err=%v", columns, err)
	}
	var evidence, token string
	var present int
	if err := s.DB().QueryRow(`SELECT evidence_at,openclaw_present,workload_policy_token
	 FROM workload_observation_witness WHERE machine_id='cnode'`).Scan(&evidence, &present, &token); err != nil || evidence != "" || present != 0 || token != "" {
		t.Fatalf("legacy healthy witness was not made fail-closed: evidence=%q present=%d token=%q err=%v", evidence, present, token, err)
	}
}

func TestLineageExternalSameDigestCannotUseForgedTerminalTime(t *testing.T) {
	s := rolloutStore(t)
	n, canaryFinished := eligiblePromotionFixture(t, s)
	now := s.now()
	digest := strings.TrimPrefix(n.Job.ArtifactDigest, "sha256:")
	spec := `{"kind":"openclaw","version":"2026.9.2","artifact":{"sha256":"` + digest + `"}}`
	// The row is appended after the selected canary, but its caller-supplied
	// terminal clock claims it ran near the old canary finish. Timestamp-only
	// WaitFrom logic would immediately inherit the already elapsed workday.
	createSucceededPromoteWitness(t, s, "cnode", spec, "sha256:"+digest, canaryFinished.Add(time.Minute))

	facts, err := s.PromoteFacts("2026.9.2", digest, now)
	if err != nil {
		t.Fatal(err)
	}
	decision := rollout.PromoteGate(facts, now, time.UTC)
	if decision.Allowed || !strings.Contains(decision.Summary(), "lineage 外重套") ||
		!strings.Contains(decision.Summary(), "重跑 canary") {
		t.Fatalf("external reapply reused forged terminal time: %s", decision.Summary())
	}
}

func TestReappliedExactDigestRequiresFreshCanary(t *testing.T) {
	s := rolloutStore(t)
	n, oldFinished := eligiblePromotionFixture(t, s)
	now := s.now()
	if decision := promoteDecision(t, s, n, now); !decision.Allowed {
		t.Fatalf("baseline locked: %s", decision.Summary())
	}
	a := strings.Repeat("a", 64)
	b := strings.Repeat("b", 64)
	spec := func(digest string) string {
		return `{"kind":"openclaw","version":"2026.9.2","artifact":{"sha256":"` + digest + `"}}`
	}
	createSucceededPromoteWitness(t, s, "cnode", spec(b), "sha256:"+b, now.Add(-2*time.Hour))
	reappliedAt := now.Add(-time.Hour)
	createSucceededPromoteWitness(t, s, "cnode", spec(a), "sha256:"+a, reappliedAt)

	facts, err := s.PromoteFacts("2026.9.2", a, now)
	if err != nil {
		t.Fatal(err)
	}
	if facts.Canary == nil || !facts.Canary.FinishedAt.Equal(oldFinished) ||
		!facts.Canary.WaitFrom.Equal(oldFinished) ||
		!strings.Contains(facts.Canary.Targets[0].AppliedReason, "lineage 外重套") {
		t.Fatalf("A→B→A did not require a fresh canary: canary=%+v", facts.Canary)
	}
	decision := rollout.PromoteGate(facts, now, time.UTC)
	if decision.Allowed || !strings.Contains(decision.Summary(), "重跑 canary") {
		t.Fatalf("reapplied A reused old canary lineage: %s", decision.Summary())
	}
}

func TestLaterExactApplyDoesNotEraseEarlierCanarySilentFailure(t *testing.T) {
	s := rolloutStore(t)
	n, canaryFinished := eligiblePromotionFixture(t, s)
	now := s.now()
	failureAt := canaryFinished.Add(time.Hour)
	if _, err := s.RecordCanarySilentFailure("cnode", "failure between canary and reapply", failureAt); err != nil {
		t.Fatal(err)
	}
	recoveredAt := failureAt.Add(time.Minute)
	tx, err := s.DB().Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := closeCanarySilentFailureWithEvidenceTx(tx, "cnode", recoveredAt, recoveredAt); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	digest := strings.TrimPrefix(n.Job.ArtifactDigest, "sha256:")
	spec := `{"kind":"openclaw","version":"2026.9.2","artifact":{"sha256":"` + digest + `"}}`
	reappliedAt := recoveredAt.Add(time.Hour)
	createSucceededPromoteWitness(t, s, "cnode", spec, "sha256:"+digest, reappliedAt)

	facts, err := s.PromoteFacts("2026.9.2", digest, now)
	if err != nil {
		t.Fatal(err)
	}
	if facts.Canary == nil || !facts.Canary.FinishedAt.Equal(canaryFinished) ||
		!facts.Canary.WaitFrom.Equal(canaryFinished) ||
		!strings.Contains(facts.Canary.Targets[0].AppliedReason, "lineage 外重套") {
		t.Fatalf("canary history/wait anchors collapsed: %+v", facts.Canary)
	}
	if len(facts.Silent) == 0 || !facts.Silent[0].FirstSeenAt.Equal(failureAt) {
		t.Fatalf("later exact apply erased T0..T2 failure history: %+v", facts.Silent)
	}
	if decision := rollout.PromoteGate(facts, now, time.UTC); decision.Allowed || !strings.Contains(decision.Summary(), "沉默失敗") {
		t.Fatalf("failure before reapply no longer locks promote: %s", decision.Summary())
	}
}

func TestInvalidEventEvidenceCannotCloseOpenFailure(t *testing.T) {
	s := rolloutStore(t)
	addRolloutMachine(t, s, "cnode", "samplehub1", false)
	rule := model.Expectation{
		Machine: "samplehub1", Unit: "proof.service", Artifact: "/tmp/proof.jsonl",
		MaxAgeSeconds: 3600, Why: "external proof",
		Events: &model.EventSpec{TsField: "at", TypeField: "kind", NotOK: []string{"broken", "corrupt"}, WindowSeconds: 3600},
	}
	s.SetExpectations(&expect.Set{Configured: true, Rules: []model.Expectation{rule}})
	base := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	if err := s.PublishExpectationsPolicy(base); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordCheckin("cnode", model.Checkin{SentAt: base, AgentStartedAt: base}, base); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordCanarySilentFailure("cnode", "existing failure", base); err != nil {
		t.Fatal(err)
	}

	batch := func(at time.Time, event model.EventStream) model.ObservationBatch {
		b := promoteObservation("2026.9.2", at)
		stampCurrentWorkloadPolicy(t, s, "samplehub1", &b)
		modTime := at
		b.Artifacts = []model.ArtifactCheck{{Unit: rule.Unit, Artifact: rule.Artifact, Exists: true, ModTime: &modTime}}
		event.Unit, event.Path = rule.Unit, rule.Artifact
		b.Events = []model.EventStream{event}
		return b
	}
	invalid := []struct {
		name  string
		event model.EventStream
	}{
		{"missing declared name", model.EventStream{Declared: []model.EventSummary{{Type: "broken", Count: 0}}}},
		{"truncated without coverage", model.EventStream{Truncated: true, Declared: []model.EventSummary{{Type: "broken"}, {Type: "corrupt"}}}},
		{"negative required count", model.EventStream{Declared: []model.EventSummary{{Type: "broken", Count: -1}, {Type: "corrupt"}}}},
		{"negative undeclared count", model.EventStream{Declared: []model.EventSummary{{Type: "broken"}, {Type: "corrupt"}}, Undeclared: []model.EventSummary{{Type: "new", Count: -1}}, UndeclaredTotal: 1}},
		{"negative malformed", model.EventStream{Declared: []model.EventSummary{{Type: "broken"}, {Type: "corrupt"}}, Malformed: -1}},
		{"negative undeclared total", model.EventStream{Declared: []model.EventSummary{{Type: "broken"}, {Type: "corrupt"}}, UndeclaredTotal: -1}},
		{"undeclared total below list", model.EventStream{Declared: []model.EventSummary{{Type: "broken"}, {Type: "corrupt"}}, Undeclared: []model.EventSummary{{Type: "new", Count: 0}}}},
		{"zero count with last-at", model.EventStream{Declared: []model.EventSummary{{Type: "broken", Count: 0, LastAt: &base}, {Type: "corrupt", Count: 0}}}},
		{"duplicate declared type", model.EventStream{Declared: []model.EventSummary{{Type: "broken", Count: 0}, {Type: "broken", Count: 0}, {Type: "corrupt", Count: 0}}}},
	}
	for i, tc := range invalid {
		at := base.Add(time.Duration(i+1) * time.Minute)
		if err := s.RecordObservation("cnode", batch(at, tc.event), at); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		var last string
		var open int
		if err := s.DB().QueryRow(`SELECT last_seen_at,open FROM canary_silent_failures WHERE machine_id='cnode'`).Scan(&last, &open); err != nil || open != 1 || last != fmtTime(base) {
			t.Fatalf("%s closed/changed span: last=%q open=%d err=%v", tc.name, last, open, err)
		}
	}

	cleanAt := base.Add(time.Duration(len(invalid)+1) * time.Minute)
	clean := model.EventStream{Declared: []model.EventSummary{{Type: "broken", Count: 0}, {Type: "corrupt", Count: 0}}}
	if err := s.RecordCheckin("cnode", model.Checkin{SentAt: cleanAt, AgentStartedAt: base}, cleanAt); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordObservation("cnode", batch(cleanAt, clean), cleanAt); err != nil {
		t.Fatal(err)
	}
	var last string
	var open int
	if err := s.DB().QueryRow(`SELECT last_seen_at,open FROM canary_silent_failures WHERE machine_id='cnode'`).Scan(&last, &open); err != nil || open != 0 || last != fmtTime(cleanAt) {
		t.Fatalf("clean explicit zero-count event witness did not close: last=%q open=%d err=%v", last, open, err)
	}
}

func TestDuplicateEffectiveExpectationKeysCannotCloseOpenFailure(t *testing.T) {
	s := rolloutStore(t)
	addRolloutMachine(t, s, "cnode", "samplehub1", false)
	base := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	if err := s.RecordCheckin("cnode", model.Checkin{SentAt: base, AgentStartedAt: base}, base); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordCanarySilentFailure("cnode", "existing failure", base); err != nil {
		t.Fatal(err)
	}
	rules := []model.Expectation{
		{Machine: "*", Unit: "proof.service", Artifact: "/tmp/proof", MaxAgeSeconds: 3600, Why: "global",
			Events: &model.EventSpec{TsField: "at", TypeField: "kind", NotOK: []string{"broken"}, WindowSeconds: 3600}},
		{Machine: "samplehub1", Unit: "proof.service", Artifact: "/tmp/proof", MaxAgeSeconds: 60, Why: "specific",
			Events: &model.EventSpec{TsField: "at", TypeField: "kind", NotOK: []string{"corrupt"}, WindowSeconds: 60}},
	}
	s.SetExpectations(&expect.Set{Configured: true, Rules: rules})
	if err := s.PublishExpectationsPolicy(base); err != nil {
		t.Fatal(err)
	}
	at := base.Add(time.Minute)
	b := promoteObservation("2026.9.2", at)
	stampCurrentWorkloadPolicy(t, s, "samplehub1", &b)
	modTime := at
	b.Artifacts = []model.ArtifactCheck{{Unit: "proof.service", Artifact: "/tmp/proof", Exists: true, ModTime: &modTime}}
	b.Events = []model.EventStream{{
		Unit: "proof.service", Path: "/tmp/proof",
		Declared: []model.EventSummary{{Type: "broken", Count: 0}, {Type: "corrupt", Count: 0}},
	}}
	if err := s.RecordObservation("cnode", b, at); err != nil {
		t.Fatal(err)
	}
	assertOpenFailureAt(t, s, base, "duplicate effective expectation key")
}

func TestFutureWorkloadTimesAndSkewedCheckinCannotCloseOpenFailure(t *testing.T) {
	s := rolloutStore(t)
	addRolloutMachine(t, s, "cnode", "samplehub1", false)
	base := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	if err := s.RecordCheckin("cnode", model.Checkin{SentAt: base, AgentStartedAt: base}, base); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordCanarySilentFailure("cnode", "existing failure", base); err != nil {
		t.Fatal(err)
	}

	futureTaskAt := base.Add(time.Minute)
	futureTask := promoteObservation("2026.9.2", futureTaskAt)
	task := futureTaskAt.Add(state.ClockSkewTolerance + time.Second)
	futureTask.OpenClaw.DB.LastTaskEndedAt = &task
	if err := s.RecordObservation("cnode", futureTask, futureTaskAt); err != nil {
		t.Fatal(err)
	}
	assertOpenFailureAt(t, s, base, "future task")

	rule := model.Expectation{Machine: "samplehub1", Unit: "proof.service", Artifact: "/tmp/proof", MaxAgeSeconds: 3600, Why: "proof"}
	s.SetExpectations(&expect.Set{Configured: true, Rules: []model.Expectation{rule}})
	futureArtifactAt := base.Add(2 * time.Minute)
	if err := s.PublishExpectationsPolicy(futureArtifactAt); err != nil {
		t.Fatal(err)
	}
	futureArtifact := promoteObservation("2026.9.2", futureArtifactAt)
	stampCurrentWorkloadPolicy(t, s, "samplehub1", &futureArtifact)
	modTime := futureArtifactAt.Add(state.ClockSkewTolerance + time.Second)
	futureArtifact.Artifacts = []model.ArtifactCheck{{Unit: rule.Unit, Artifact: rule.Artifact, Exists: true, ModTime: &modTime}}
	if err := s.RecordObservation("cnode", futureArtifact, futureArtifactAt); err != nil {
		t.Fatal(err)
	}
	assertOpenFailureAt(t, s, base, "future artifact")

	skewedAt := base.Add(3 * time.Minute)
	if err := s.RecordCheckin("cnode", model.Checkin{
		SentAt: skewedAt.Add(state.ClockSkewTolerance + time.Second), AgentStartedAt: base,
	}, skewedAt); err != nil {
		t.Fatal(err)
	}
	coherent := promoteObservation("2026.9.2", skewedAt)
	stampCurrentWorkloadPolicy(t, s, "samplehub1", &coherent)
	modTime = skewedAt
	coherent.Artifacts = []model.ArtifactCheck{{Unit: rule.Unit, Artifact: rule.Artifact, Exists: true, ModTime: &modTime}}
	if err := s.RecordObservation("cnode", coherent, skewedAt); err != nil {
		t.Fatal(err)
	}
	assertOpenFailureAt(t, s, base, "skewed checkin")
}

func assertOpenFailureAt(t *testing.T, s *Store, want time.Time, context string) {
	t.Helper()
	var last string
	var open int
	if err := s.DB().QueryRow(`SELECT last_seen_at,open FROM canary_silent_failures WHERE machine_id='cnode'`).Scan(&last, &open); err != nil || open != 1 || last != fmtTime(want) {
		t.Fatalf("%s closed/changed span: last=%q open=%d err=%v", context, last, open, err)
	}
}
