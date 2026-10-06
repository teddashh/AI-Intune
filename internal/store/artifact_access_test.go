package store

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/agentadapter"
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

func TestMachineMayDownloadArtifactAllowsEveryRegisteredExecutorKind(t *testing.T) {
	s := newTestStore(t)
	if err := s.UpsertMachine(Machine{MachineID: "machine-a", DisplayName: "machine-a", Expected: true}); err != nil {
		t.Fatal(err)
	}
	for _, kind := range registeredArtifactExecutorKinds(t) {
		digest := artifactGrantDigest(kind)
		insertMachineArtifactGrant(t, s, "machine-a", kind, digest)
		allowed, err := s.MachineMayDownloadArtifact("machine-a", digest)
		if err != nil || !allowed {
			t.Fatalf("kind %s grant=%t err=%v", kind, allowed, err)
		}
	}
}

func TestMachineMayDownloadArtifactFailsClosedOnUnregisteredKind(t *testing.T) {
	const kind = "not-an-adapter"
	for _, registered := range agentadapter.ExecutorKinds() {
		if registered == kind {
			t.Fatalf("%s is registered", kind)
		}
	}
	s := newTestStore(t)
	if err := s.UpsertMachine(Machine{MachineID: "machine-a", DisplayName: "machine-a", Expected: true}); err != nil {
		t.Fatal(err)
	}
	digest := artifactGrantDigest(kind)
	insertMachineArtifactGrant(t, s, "machine-a", kind, digest)
	allowed, err := s.MachineMayDownloadArtifact("machine-a", digest)
	if err == nil || allowed || !strings.Contains(err.Error(), "store: machine artifact grant contradicts desired state") {
		t.Fatalf("unregistered kind grant=%t err=%v", allowed, err)
	}
}

func registeredArtifactExecutorKinds(t *testing.T) []string {
	t.Helper()
	fromContracts := make([]string, 0, len(agentadapter.Contracts()))
	seen := make(map[string]struct{}, len(agentadapter.Contracts()))
	for _, contract := range agentadapter.Contracts() {
		if contract.ExecutorKind == "" {
			t.Fatal("registered contract has an empty executor kind")
		}
		if _, ok := seen[contract.ExecutorKind]; ok {
			t.Fatalf("duplicate executor kind %s", contract.ExecutorKind)
		}
		seen[contract.ExecutorKind] = struct{}{}
		fromContracts = append(fromContracts, contract.ExecutorKind)
	}
	sort.Strings(fromContracts)
	kinds := agentadapter.ExecutorKinds()
	if len(kinds) == 0 || !slices.Equal(kinds, fromContracts) {
		t.Fatalf("registry kinds=%v contracts=%v", kinds, fromContracts)
	}
	return kinds
}

func artifactGrantDigest(label string) string {
	sum := sha256.Sum256([]byte("artifact-grant:" + label))
	return hex.EncodeToString(sum[:])
}

func insertMachineArtifactGrant(t *testing.T, s *Store, machineID, kind, digest string) {
	t.Helper()
	spec := fmt.Sprintf(`{"kind":%q,"version":"1","artifact":{"sha256":%q,"size":12,"url":%q}}`,
		kind, digest, "/v1/artifacts/"+digest)
	desiredID := newID()
	now := fmtTime(time.Now().UTC().Truncate(time.Second))
	if _, err := s.db.Exec(`INSERT INTO desired_state
 (desired_id,scope_type,scope_id,resource_kind,resource_id,revision,spec,created_at,created_by)
 VALUES (?, 'machine', ?, ?, ?, 1, ?, ?, 'test')`, desiredID, machineID, kind, kind, spec, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO jobs
 (job_id,machine_id,desired_id,revision,state,created_at,artifact_digest,irreversible,execution_timeout)
 VALUES (?, ?, ?, 1, ?, ?, ?, 0, 600)`, newID(), machineID, desiredID, deploy.NotStarted, now, "sha256:"+digest); err != nil {
		t.Fatal(err)
	}
}
