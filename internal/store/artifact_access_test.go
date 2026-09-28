package store

import (
	"fmt"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/deploy"
)

func TestMachineMayDownloadArtifactRequiresExactOwnedNonterminalJob(t *testing.T) {
	s := newTestStore(t)
	for _, id := range []string{"machine-a", "machine-b"} {
		if err := s.UpsertMachine(Machine{MachineID: id, DisplayName: id, Expected: true}); err != nil {
			t.Fatal(err)
		}
	}
	digest := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	spec := fmt.Sprintf(`{"kind":"openclaw","version":"2026.9.8","artifact":{"sha256":%q,"size":12,"url":%q}}`,
		digest, "/v1/artifacts/"+digest)
	if _, err := s.db.Exec(`UPDATE machine_registry SET channel='canary' WHERE machine_id='machine-a'`); err != nil {
		t.Fatal(err)
	}
	_, jobs, err := s.CreateDeployment(NewDeployment{
		Channel: "canary", ResourceKind: "openclaw", ResourceID: "openclaw", Spec: spec,
		BatchSize: 1, CreatedBy: "test", Targets: []NewDeploymentTarget{{MachineID: "machine-a", BatchNo: 1}},
		Job: NewJob{ArtifactDigest: "sha256:" + digest},
	})
	if err != nil {
		t.Fatal(err)
	}
	jobID := jobs[0].JobID

	if allowed, err := s.MachineMayDownloadArtifact("machine-a", digest); err != nil || !allowed {
		t.Fatalf("owner grant=%t err=%v", allowed, err)
	}
	for _, tc := range []struct{ machine, digest string }{
		{"machine-b", digest},
		{"machine-a", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
		{"machine-a", "../not-a-digest"},
	} {
		if allowed, err := s.MachineMayDownloadArtifact(tc.machine, tc.digest); err != nil || allowed {
			t.Fatalf("unexpected grant machine=%q digest=%q allowed=%t err=%v", tc.machine, tc.digest, allowed, err)
		}
	}

	now := time.Now().UTC().Truncate(time.Second)
	if _, err := s.db.Exec(`UPDATE jobs SET state=?,terminal_at=? WHERE job_id=?`, deploy.Succeeded, fmtTime(now), jobID); err != nil {
		t.Fatal(err)
	}
	if allowed, err := s.MachineMayDownloadArtifact("machine-a", digest); err != nil || allowed {
		t.Fatalf("terminal job still grants download: allowed=%t err=%v", allowed, err)
	}
}

func TestMachineMayDownloadArtifactFailsClosedOnContradictorySpec(t *testing.T) {
	s := newTestStore(t)
	if err := s.UpsertMachine(Machine{MachineID: "machine-a", DisplayName: "machine-a", Expected: true}); err != nil {
		t.Fatal(err)
	}
	digest := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if _, err := s.db.Exec(`UPDATE machine_registry SET channel='canary' WHERE machine_id='machine-a'`); err != nil {
		t.Fatal(err)
	}
	badSpec := `{"kind":"openclaw","version":"2026.9.8","artifact":{"sha256":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","size":12,"url":"/v1/artifacts/bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}}`
	desiredID := newID()
	now := fmtTime(time.Now().UTC().Truncate(time.Second))
	if _, err := s.db.Exec(`INSERT INTO desired_state
 (desired_id,scope_type,scope_id,resource_kind,resource_id,revision,spec,created_at,created_by)
 VALUES (?, 'channel', 'canary', 'openclaw', 'openclaw', 1, ?, ?, 'test')`, desiredID, badSpec, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO jobs
 (job_id,machine_id,desired_id,revision,state,created_at,artifact_digest,irreversible,execution_timeout)
 VALUES (?, 'machine-a', ?, 1, ?, ?, ?, 0, 600)`, newID(), desiredID, deploy.NotStarted, now, "sha256:"+digest); err != nil {
		t.Fatal(err)
	}
	if allowed, err := s.MachineMayDownloadArtifact("machine-a", digest); err == nil || allowed {
		t.Fatalf("contradictory spec did not fail closed: allowed=%t err=%v", allowed, err)
	}
}
