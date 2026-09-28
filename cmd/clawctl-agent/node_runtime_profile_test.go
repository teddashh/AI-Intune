package main

import (
	"archive/tar"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/artifact"
	appcatalog "github.com/teddashh/AI-Intune/internal/catalog"
	"github.com/teddashh/AI-Intune/internal/deploy"
	"github.com/teddashh/AI-Intune/internal/model"
	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/operatorauth"
	"github.com/teddashh/AI-Intune/internal/operatorclient"
	"github.com/teddashh/AI-Intune/internal/store"
)

// This exercises real assignment, executor and Hub store code with a shell
// runtime fixture. It does not execute a Darwin binary or test HTTP transport.
func TestDarwinNodeProfileExecutorEvidenceCompletesAssignedJob(t *testing.T) {
	for _, arch := range []string{"arm64", "amd64"} {
		for _, mode := range []string{"activate", "current"} {
			t.Run(arch+"/"+mode, func(t *testing.T) {
				fixture := assignedDarwinNodeProfile(t, arch)
				st, job, bundle := fixture.store, fixture.job, fixture.bundle
				now := time.Now().UTC().Truncate(time.Second)
				downloads := 0
				node := nodeRuntimeFixture(t, &bundle, &downloads)
				node.targetOS, node.targetArch = "darwin", arch
				node.deps.home = "/Users/profile-test"
				node.deps.now = func() time.Time { return now }
				executor := newKindExecutor(nil, nil, nil, node, nil)
				if mode == "current" {
					// Model an endpoint that already has this exact artifact active.
					rows, err := executor.Run(t.Context(), job)
					if err != nil || len(rows) != 3 || !allPassed(rows) {
						t.Fatalf("prepare current release: rows=%+v err=%v", rows, err)
					}
				}
				lease, err := st.ClaimJob(job.JobID, job.MachineID, now, time.Minute)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := st.AdvanceJobByAgent(job.JobID, job.MachineID, lease, deploy.Start, now); err != nil {
					t.Fatal(err)
				}
				rows, err := executor.Run(t.Context(), job)
				if err != nil || len(rows) != 3 || !allPassed(rows) {
					t.Fatalf("assigned job execution: rows=%+v err=%v", rows, err)
				}
				if downloads != 1 {
					t.Fatalf("exact artifact downloads=%d want=1", downloads)
				}
				if _, err := st.AdvanceJobByAgent(job.JobID, job.MachineID, lease, deploy.FinishWork, now); err != nil {
					t.Fatal(err)
				}
				for _, row := range rows {
					if _, err := st.MarkSucceededIfVerified(job.JobID, now); !errors.Is(err, store.ErrNoVerification) {
						t.Fatalf("incomplete executor evidence: err=%v want ErrNoVerification", err)
					}
					// Forward the executor's evidence without rebuilding rule IDs,
					// commands or stdout to match the Hub's expectations.
					if err := st.RecordVerification(job.JobID, job.MachineID, lease, row.RuleID,
						row.Command, row.ExitCode, row.StdoutExcerpt, row.StderrExcerpt,
						row.Passed, row.VerifiedAt); err != nil {
						t.Fatal(err)
					}
				}
				if state, err := st.MarkSucceededIfVerified(job.JobID, now); err != nil || state != deploy.Succeeded {
					t.Fatalf("complete executor evidence: state=%s err=%v", state, err)
				}
				stored, err := st.JobForMachine(job.JobID, job.MachineID)
				if err != nil || stored.State != deploy.Succeeded || stored.ArtifactDigest != job.ArtifactDigest {
					t.Fatalf("persisted job: state=%s digest=%s err=%v", stored.State, stored.ArtifactDigest, err)
				}
				evidence, err := st.JobVerifications(job.JobID)
				if err != nil || len(evidence) != len(rows) {
					t.Fatalf("persisted evidence count=%d err=%v", len(evidence), err)
				}
				byRule := make(map[string]model.JobVerificationRequest, len(rows))
				for _, row := range rows {
					byRule[row.RuleID] = row
				}
				for _, got := range evidence {
					want, ok := byRule[got.RuleID]
					if !ok || got.Command != want.Command ||
						got.StdoutExcerpt != want.StdoutExcerpt || got.StderrExcerpt != want.StderrExcerpt ||
						got.ExitCode == nil || *got.ExitCode != want.ExitCode || got.Passed != want.Passed ||
						!got.VerifiedAt.Equal(want.VerifiedAt) || got.ProducerID != job.MachineID ||
						got.EvidenceRole != store.JobVerificationRoleExecutor {
						t.Fatalf("executor evidence changed in storage: got=%+v want=%+v", got, want)
					}
				}
			})
		}
	}
}

type darwinNodeProfileFixture struct {
	assignment   store.OperatorMachineProfileAssignmentResult
	store        *store.Store
	job          model.JobResponse
	bundle       []byte
	artifactsDir string
	agentToken   string
}

func assignedDarwinNodeProfile(t *testing.T, arch string) darwinNodeProfileFixture {
	t.Helper()
	const version = "24.15.0"
	dir := t.TempDir()
	bundle := writeNodeRuntimeBundle(t, []nodeBundleEntry{
		{name: "node-runtime/darwin-" + arch + "/bin/node", typeflag: tar.TypeReg, mode: 0o755,
			body: []byte("#!/bin/sh\nif [ \"$#\" -eq 1 ] && [ \"$1\" = --version ]; then echo v" + version + "; exit 0; fi\n" +
				"[ \"$#\" -eq 2 ] && [ \"$2\" = --version ] && [ -f \"$1\" ] || exit 1\necho 11.7.0\n")},
		{name: "node-runtime/darwin-" + arch + "/lib/node_modules/npm/bin/npm-cli.js",
			typeflag: tar.TypeReg, mode: 0o644, body: []byte("fixture npm")},
	})
	sum, sri := sha256.Sum256(bundle), sha512.Sum512(bundle)
	digest := hex.EncodeToString(sum[:])
	now := time.Now().UTC().Truncate(time.Second)
	record := artifact.Sidecar{
		Name: "node-runtime", Version: version, TarballURL: "https://nodejs.org/dist/v" + version + "/",
		SHA512Integrity: "sha512-" + base64.StdEncoding.EncodeToString(sri[:]),
		SHA256:          digest, Size: int64(len(bundle)), FetchedAt: now, FetchedBy: "operator:test",
	}
	raw, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string][]byte{digest + ".json": raw, digest + ".tgz": bundle} {
		if err := os.WriteFile(filepath.Join(dir, name), content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	st, err := store.Open(filepath.Join(dir, "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	service := operator.NewWithArtifacts(st, dir)
	actor := operator.Actor{
		AuthSubject: "user:test@example.com", AuthDecision: string(operatorauth.Authorized),
		AuthCapability: "example.com/cap/clawctl-operate", AuthMethod: operatorauth.AuthMethodLocalAPI,
		SourceKind: operator.SourceKindOperatorAPI, WhoUser: "test@example.com", WhoNode: "test-node",
	}
	target := appcatalog.Platform{OS: "darwin", Arch: arch}
	manifest := appcatalog.Manifest{
		SchemaVersion: appcatalog.SchemaVersion, ID: "node-runtime", Version: version,
		Kind: appcatalog.KindRuntime, Title: "Node.js",
		Source:   appcatalog.Source{Catalog: "ai-intune", UpstreamURL: record.TarballURL, Revision: "v" + version, License: "MIT"},
		Artifact: appcatalog.Artifact{SHA256: digest, Size: record.Size},
		Adapter:  appcatalog.Adapter{Name: "node-runtime", Version: 1}, Platforms: []appcatalog.Platform{target},
		Dependencies: []appcatalog.PackageRef{}, Provides: []string{"runtime.node"},
		Conflicts: []string{}, ExclusiveGroups: []string{},
	}
	if _, err := service.PublishCatalogManifest(t.Context(), operator.CatalogManifestPublishRequest{
		Manifest: manifest, Reason: "approve pinned Node", IdempotencyKey: "node-manifest", Actor: actor,
	}); err != nil {
		t.Fatal(err)
	}
	profile := appcatalog.MachineProfile{
		SchemaVersion: appcatalog.SchemaVersion, ID: "darwin-node", Revision: 1,
		Packages: []appcatalog.PackageRef{{PackageID: manifest.ID, Version: version}},
	}
	if _, err := service.PublishMachineProfile(t.Context(), operator.MachineProfilePublishRequest{
		Profile: profile, Reason: "approve Node profile", IdempotencyKey: "node-profile", Actor: actor,
	}); err != nil {
		t.Fatal(err)
	}
	machineID, token, err := st.CreateEnrollTokenFor("darwin-profile", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	_, agentToken, err := st.RedeemEnrollToken(token, model.EnrollRequest{
		SchemaVersion: model.SchemaVersion, EnrollToken: token, Hostname: "darwin-profile",
		UnixUser: "profile-test", AgentVersion: "test",
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	enabled := true
	if err := st.RecordCheckin(machineID, model.Checkin{
		SchemaVersion: model.SchemaVersion, SentAt: now, AgentStartedAt: now.Add(-time.Hour),
		AgentSeq: 1, JobsEnabled: &enabled,
	}, now); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordObservation(machineID, model.ObservationBatch{
		SchemaVersion: model.SchemaVersion, MeasuredAt: now,
		Identity: model.Identity{Hostname: "darwin-profile", OS: "macOS 15.7", Arch: arch, UnixUser: "profile-test"},
	}, now); err != nil {
		t.Fatal(err)
	}
	preview, err := service.PreviewMachineProfileAssignment(t.Context(), operator.MachineProfileAssignmentPreviewRequest{
		MachineID: machineID, ProfileID: profile.ID, ProfileRevision: profile.Revision,
	})
	if err != nil || preview.Target != target || preview.CreatesJobs != 1 || len(preview.Blockers) != 0 {
		t.Fatalf("assignment preview=%+v err=%v", preview, err)
	}
	assignment, err := service.AssignMachineProfile(t.Context(), operator.MachineProfileAssignmentRequest{
		MachineID: machineID, ProfileID: profile.ID, ProfileRevision: profile.Revision,
		ConfirmDisplayName: preview.DisplayName, PreviewDigest: preview.PreviewDigest,
		Reason: "install pinned Node", IdempotencyKey: "node-assignment", Actor: actor,
	})
	if err != nil || len(assignment.Packages) != 1 {
		t.Fatalf("assignment=%+v err=%v", assignment, err)
	}
	job, ok, err := st.NextJobForMachine(machineID)
	if err != nil || !ok || job.JobID != assignment.Packages[0].JobID || job.ArtifactDigest != "sha256:"+digest {
		t.Fatalf("assigned job: id=%s digest=%s ok=%t err=%v", job.JobID, job.ArtifactDigest, ok, err)
	}
	desired, err := st.DesiredState(job.DesiredID)
	if err != nil {
		t.Fatal(err)
	}
	var spec model.NodeRuntimeSpec
	if err := json.Unmarshal([]byte(desired.Spec), &spec); err != nil || spec.Version != version ||
		spec.TargetOS != target.OS || spec.TargetArch != target.Arch ||
		!reflect.DeepEqual(spec.Artifact, &model.ArtifactRef{SHA256: digest, Size: record.Size, URL: "/v1/artifacts/" + digest}) {
		t.Fatalf("assigned spec=%+v err=%v", spec, err)
	}
	return darwinNodeProfileFixture{assignment: assignment, store: st, artifactsDir: dir, agentToken: agentToken, bundle: bundle, job: model.JobResponse{
		JobID: job.JobID, MachineID: job.MachineID, DesiredID: job.DesiredID,
		Revision: int64(job.Revision), State: string(job.State), ExecutionTimeout: job.ExecutionTimeout,
		ArtifactDigest: job.ArtifactDigest, ResourceKind: desired.ResourceKind, ResourceID: desired.ResourceID,
		Spec: json.RawMessage(desired.Spec), Irreversible: job.Irreversible,
	}}
}

func reassignDarwinNodeProfile(t *testing.T, f darwinNodeProfileFixture, client *operatorclient.Client,
	priorPreview operator.MachineProfileAssignmentPreviewResult, dropReply bool) darwinNodeProfileFixture {
	t.Helper()
	assertNodeProfileStaleHTTPReplay(t, f, client, priorPreview)
	preview, err := client.PreviewMachineProfileAssignment(t.Context(), operator.MachineProfileAssignmentPreviewRequest{
		MachineID:       f.job.MachineID,
		ProfileID:       f.assignment.ProfileID,
		ProfileRevision: f.assignment.ProfileRevision,
	})
	if err != nil {
		t.Fatalf("preview machine profile assignment: %v", err)
	}
	if preview.AlreadyAssigned {
		t.Fatal("preview already assigned")
	}
	if preview.CreatesJobs != 1 {
		t.Fatalf("preview creates jobs: got %d, want 1", preview.CreatesJobs)
	}
	if preview.CreatesDesiredStates != 1 {
		t.Fatalf("preview creates desired states: got %d, want 1", preview.CreatesDesiredStates)
	}
	if !preview.CreatesAssignment {
		t.Fatal("preview does not create assignment")
	}
	if len(preview.Blockers) != 0 {
		t.Fatalf("preview blockers: got %d, want 0", len(preview.Blockers))
	}
	if len(preview.Packages) != 1 {
		t.Fatalf("preview packages: got %d, want 1", len(preview.Packages))
	}
	if preview.CurrentAssignmentID != f.assignment.AssignmentID ||
		preview.CurrentAssignmentRevision != f.assignment.AssignmentRevision ||
		preview.Target != f.assignment.Target || preview.ProfileDigest != f.assignment.ProfileDigest {
		t.Fatal("retry preview changed the pinned profile or previous assignment identity")
	}
	wantRevision := deploy.Revision(f.job.Revision + 1)
	if preview.Packages[0].PlannedRevision != wantRevision {
		t.Fatalf("preview planned revision: got %v, want %v", preview.Packages[0].PlannedRevision, wantRevision)
	}

	result := assignNodeProfileHTTPWithReplay(t, f, client, "node-reassignment", operator.MachineProfileAssignmentRequest{
		MachineID:          f.job.MachineID,
		ProfileID:          f.assignment.ProfileID,
		ProfileRevision:    f.assignment.ProfileRevision,
		ConfirmDisplayName: preview.DisplayName,
		PreviewDigest:      preview.PreviewDigest,
		Reason:             "retry pinned Node",
	}, dropReply)
	if result.AssignmentID == "" || result.AssignmentID == f.assignment.AssignmentID {
		t.Fatal("assignment id unchanged")
	}
	if result.AssignmentRevision != f.assignment.AssignmentRevision+1 {
		t.Fatalf("assignment revision: got %v, want %v", result.AssignmentRevision, f.assignment.AssignmentRevision+1)
	}
	if !result.Replayed {
		t.Fatal("assignment was not replayed")
	}
	if result.AlreadyAssigned {
		t.Fatal("assignment already assigned")
	}
	if len(result.Packages) != 1 {
		t.Fatalf("assignment packages: got %d, want 1", len(result.Packages))
	}

	if result.ProfileID != f.assignment.ProfileID || result.ProfileRevision != f.assignment.ProfileRevision ||
		result.ProfileDigest != f.assignment.ProfileDigest || result.Target != f.assignment.Target {
		t.Fatal("reassignment changed the pinned profile identity")
	}

	job, ok, err := f.store.NextJobForMachine(f.job.MachineID)
	if err != nil {
		t.Fatalf("next job for machine: %v", err)
	}
	if !ok {
		t.Fatal("next job for machine not found")
	}
	if job.JobID == f.job.JobID {
		t.Fatal("job id unchanged")
	}
	if job.DesiredID == f.job.DesiredID {
		t.Fatal("desired id unchanged")
	}
	if job.Revision != wantRevision {
		t.Fatalf("job revision: got %v, want %v", job.Revision, wantRevision)
	}
	if job.ArtifactDigest != f.job.ArtifactDigest {
		t.Fatalf("job artifact digest: got %v, want %v", job.ArtifactDigest, f.job.ArtifactDigest)
	}
	if job.JobID != result.Packages[0].JobID {
		t.Fatalf("job id: got %v, want %v", job.JobID, result.Packages[0].JobID)
	}

	desired, err := f.store.DesiredState(job.DesiredID)
	if err != nil {
		t.Fatalf("desired state: %v", err)
	}
	if desired.Spec != string(f.job.Spec) {
		t.Fatalf("desired spec: got %q, want %q", desired.Spec, string(f.job.Spec))
	}
	if desired.ResourceKind != f.job.ResourceKind {
		t.Fatalf("desired resource kind: got %q, want %q", desired.ResourceKind, f.job.ResourceKind)
	}
	if desired.ResourceID != f.job.ResourceID {
		t.Fatalf("desired resource id: got %q, want %q", desired.ResourceID, f.job.ResourceID)
	}

	updated := f.job
	updated.JobID = job.JobID
	updated.DesiredID = job.DesiredID
	updated.Revision = int64(job.Revision)
	updated.State = string(job.State)
	updated.ExecutionTimeout = job.ExecutionTimeout
	updated.Irreversible = job.Irreversible
	f.job = updated
	f.assignment = result
	return f
}
