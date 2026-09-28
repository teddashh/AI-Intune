package store

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/deploy"
	"github.com/teddashh/AI-Intune/internal/expect"
	"github.com/teddashh/AI-Intune/internal/rollout"
)

func readCanaryEvidenceEpoch(t *testing.T, s *Store) time.Time {
	t.Helper()
	var raw string
	if err := s.DB().QueryRow(`SELECT started_at FROM canary_evidence_epoch WHERE singleton=1`).Scan(&raw); err != nil {
		t.Fatalf("read canary evidence epoch: %v", err)
	}
	return parseTime(raw)
}

func TestStableCreateRequiresCanaryStrictlyAfterEvidenceEpoch(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "legacy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.SetExpectations(&expect.Set{})

	epoch := time.Date(2026, 9, 7, 9, 0, 0, 0, time.UTC)
	oldFinished := epoch.Add(-72 * time.Hour)
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	digest := strings.Repeat("a", 64)
	s.nowFn = func() time.Time { return oldFinished }
	addRolloutMachine(t, s, "cnode", "samplehub1", false)
	old, oldJobs := createPromoteDeployment(t, s, "canary", "2026.9.2", digest, "",
		[]NewDeploymentTarget{{MachineID: "cnode", BatchNo: 1}})
	setPromoteJobState(t, s, oldJobs["cnode"], deploy.Succeeded, oldFinished)
	finishPromoteDeployment(t, s, old.DeploymentID, DeploymentRunning, oldFinished)

	// Open() has migrated the table, but migration itself is not proof that the
	// final evidence recorder ever ran.
	facts, err := s.PromoteFacts("2026.9.2", digest, now)
	if err != nil || facts.Canary != nil || !strings.Contains(facts.CanaryBlock, "evidence history") {
		t.Fatalf("missing epoch did not fail closed: facts=%+v err=%v", facts, err)
	}
	if err := s.PublishExpectationsPolicy(epoch); err != nil {
		t.Fatal(err)
	}
	if got := readCanaryEvidenceEpoch(t, s); !got.Equal(epoch) {
		t.Fatalf("evidence epoch=%v, want %v", got, epoch)
	}

	// This is the tempting false green: the legacy canary is old enough and a
	// single post-upgrade batch is fresh/coherent, but there is no failure history
	// for the interval before the epoch.
	s.nowFn = func() time.Time { return now }
	observePromoteMachine(t, s, "cnode", "2026.9.2", now)
	if err := s.SetMachineChannel("cnode", "stable"); err != nil {
		t.Fatal(err)
	}
	stable := NewDeployment{
		Channel: "stable", ResourceKind: "openclaw", ResourceID: "openclaw",
		Spec:      `{"kind":"openclaw","version":"2026.9.2","artifact":{"sha256":"` + digest + `"}}`,
		BatchSize: 1, CreatedBy: "epoch-test",
		Targets: []NewDeploymentTarget{{MachineID: "cnode", BatchNo: 1}},
		Job:     NewJob{ArtifactDigest: "sha256:" + digest, ExecutionTimeout: 600},
	}
	before := deploymentWriteCounts(t, s)
	if _, _, decision, err := s.createStableOpenClawDeployment(stable, time.UTC); !errors.Is(err, ErrPromoteLocked) || decision.Allowed {
		t.Fatalf("legacy canary crossed evidence epoch: err=%v decision=%s", err, decision.Summary())
	}
	assertDeploymentWriteCounts(t, s, before)

	// With second-precision ledger timestamps, equality has no trustworthy order.
	// A canary completed in the exact epoch second must also stay locked.
	s.nowFn = func() time.Time { return epoch }
	equal, equalJobs := createPromoteDeployment(t, s, "canary", "2026.9.2", digest, "",
		[]NewDeploymentTarget{{MachineID: "cnode", BatchNo: 1}})
	setPromoteJobState(t, s, equalJobs["cnode"], deploy.Succeeded, epoch)
	finishPromoteDeployment(t, s, equal.DeploymentID, DeploymentRunning, epoch)
	if err := s.SetMachineChannel("cnode", "stable"); err != nil {
		t.Fatal(err)
	}
	facts, err = s.PromoteFacts("2026.9.2", digest, now)
	if err != nil || facts.Canary != nil || !strings.Contains(facts.CanaryBlock, "evidence history") {
		t.Fatalf("same-second canary did not fail closed: facts=%+v err=%v", facts, err)
	}

	// A new canary that completes after recorder activation has a fully covered
	// post-completion window and may promote once a full business day passes.
	newFinished := epoch.Add(time.Hour)
	s.nowFn = func() time.Time { return newFinished }
	newer, newerJobs := createPromoteDeployment(t, s, "canary", "2026.9.2", digest, "",
		[]NewDeploymentTarget{{MachineID: "cnode", BatchNo: 1}})
	setPromoteJobState(t, s, newerJobs["cnode"], deploy.Succeeded, newFinished)
	finishPromoteDeployment(t, s, newer.DeploymentID, DeploymentRunning, newFinished)
	s.nowFn = func() time.Time { return now }
	establishPromoteCoverage(t, s, "cnode", "samplehub1", "2026.9.2", newFinished, now)
	if err := s.SetMachineChannel("cnode", "stable"); err != nil {
		t.Fatal(err)
	}
	created, jobs, decision, err := s.createStableOpenClawDeployment(stable, time.UTC)
	if err != nil || !decision.Allowed || created.Channel != "stable" || len(jobs) != 1 {
		t.Fatalf("post-epoch canary stayed locked: deployment=%+v jobs=%+v err=%v decision=%s",
			created, jobs, err, decision.Summary())
	}
}

func TestCanaryEvidenceEpochDoesNotMoveOnPolicyRepublishOrRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.db")
	first, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 9, 7, 9, 0, 0, 0, time.UTC)
	first.SetExpectations(&expect.Set{})
	if err := first.PublishExpectationsPolicy(start); err != nil {
		t.Fatal(err)
	}
	if err := first.PublishExpectationsPolicy(start.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	first.SetExpectations(testWorkloadPolicy("changed"))
	if err := first.PublishExpectationsPolicy(start.Add(2 * time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got := readCanaryEvidenceEpoch(t, first); !got.Equal(start) {
		t.Fatalf("republish moved evidence epoch: got %v want %v", got, start)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	restarted, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	restarted.SetExpectations(testWorkloadPolicy("changed"))
	if err := restarted.PublishExpectationsPolicy(start.Add(24 * time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got := readCanaryEvidenceEpoch(t, restarted); !got.Equal(start) {
		t.Fatalf("restart moved evidence epoch: got %v want %v", got, start)
	}
}

func TestInvalidCanaryEvidenceEpochFailsClosed(t *testing.T) {
	s := rolloutStore(t)
	n, _ := eligiblePromotionFixture(t, s)
	if _, err := s.DB().Exec(`UPDATE canary_evidence_epoch SET started_at='not-a-time' WHERE singleton=1`); err != nil {
		t.Fatal(err)
	}
	facts, err := s.PromoteFacts("2026.9.2", n.Job.ArtifactDigest, s.now())
	if err != nil || facts.Canary != nil || !strings.Contains(facts.CanaryBlock, "evidence history") {
		t.Fatalf("invalid epoch did not fail closed: facts=%+v err=%v", facts, err)
	}
	if decision := rollout.PromoteGate(facts, s.now(), time.UTC); decision.Allowed {
		t.Fatalf("invalid epoch allowed promote: %s", decision.Summary())
	}
}
