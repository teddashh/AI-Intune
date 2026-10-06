package operator

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/agentadapter"
	"github.com/teddashh/AI-Intune/internal/artifact"
	appcatalog "github.com/teddashh/AI-Intune/internal/catalog"
	"github.com/teddashh/AI-Intune/internal/deploy"
	"github.com/teddashh/AI-Intune/internal/model"
	"github.com/teddashh/AI-Intune/internal/store"
)

func writeNodeRuntimeArtifact(t *testing.T, dir, version string) artifact.Sidecar {
	return writeNodeRuntimeArtifactTargets(t, dir, version,
		"linux-amd64", "linux-arm64", "darwin-amd64", "darwin-arm64",
		"windows-amd64", "windows-arm64")
}

func writeNodeRuntimeArtifactTargets(t *testing.T, dir, version string, targets ...string) artifact.Sidecar {
	t.Helper()
	var bundle bytes.Buffer
	gz := gzip.NewWriter(&bundle)
	tw := tar.NewWriter(gz)
	for _, target := range targets {
		nodeName := "node-runtime/" + target + "/bin/node"
		if strings.HasPrefix(target, "windows-") {
			nodeName = "node-runtime/" + target + "/bin/node.exe"
		}
		for _, entry := range []struct {
			name string
			mode int64
			body string
		}{
			{name: nodeName, mode: 0o755, body: "node-" + target},
			{name: "node-runtime/" + target + "/lib/node_modules/npm/bin/npm-cli.js", mode: 0o644, body: "npm-" + target},
		} {
			if err := tw.WriteHeader(&tar.Header{Name: entry.name, Typeflag: tar.TypeReg,
				Mode: entry.mode, Size: int64(len(entry.body))}); err != nil {
				t.Fatal(err)
			}
			if _, err := tw.Write([]byte(entry.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	body := bundle.Bytes()
	sum := sha256.Sum256(body)
	sri := sha512.Sum512(body)
	digest := hex.EncodeToString(sum[:])
	record := artifact.Sidecar{
		Name: "node-runtime", Version: version,
		TarballURL:      "https://nodejs.org/dist/v" + version + "/",
		SHA512Integrity: "sha512-" + base64.StdEncoding.EncodeToString(sri[:]),
		SHA256:          digest, Size: int64(len(body)),
		FetchedAt: time.Now().UTC().Truncate(time.Second), FetchedBy: "operator:test",
	}
	raw, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, digest+".json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, digest+".tgz"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	return record
}

func nodeRuntimeManifest(record artifact.Sidecar) appcatalog.Manifest {
	return appcatalog.Manifest{
		SchemaVersion: appcatalog.SchemaVersion, ID: "node-runtime", Version: record.Version,
		Kind: appcatalog.KindRuntime, Title: "Node.js",
		Source: appcatalog.Source{
			Catalog: "ai-intune", UpstreamURL: "https://nodejs.org/dist/v" + record.Version + "/",
			Revision: "v" + record.Version, License: "MIT",
		},
		Artifact: appcatalog.Artifact{SHA256: record.SHA256, Size: record.Size},
		Adapter:  appcatalog.Adapter{Name: "node-runtime", Version: 1},
		Platforms: []appcatalog.Platform{
			{OS: "linux", Arch: "amd64"}, {OS: "linux", Arch: "arm64"},
		},
		Dependencies: []appcatalog.PackageRef{}, Provides: []string{"runtime.node"},
		Conflicts: []string{}, ExclusiveGroups: []string{},
	}
}

func admittedOpenClawProfileService(t *testing.T) (*Service, *store.Store, string, CatalogManifestPublishRequest) {
	t.Helper()
	service, st, dir, record := catalogManifestService(t)
	manifestRequest := catalogManifestRequest(record, "profile-fixture-manifest")
	if _, err := service.PublishCatalogManifest(t.Context(), manifestRequest); err != nil {
		t.Fatal(err)
	}
	return service, st, dir, manifestRequest
}

func machineProfileRequest(manifest appcatalog.Manifest, key string) MachineProfilePublishRequest {
	return MachineProfilePublishRequest{
		Profile: appcatalog.MachineProfile{
			SchemaVersion: appcatalog.SchemaVersion, ID: "openclaw-standard", Revision: 1,
			Packages: []appcatalog.PackageRef{{PackageID: manifest.ID, Version: manifest.Version}},
		},
		Reason: "approve managed profile", IdempotencyKey: key, Actor: verifiedDeploymentActor(),
	}
}

func TestPublishMachineProfileVerifiesCompleteGraphAndAudit(t *testing.T) {
	service, st, _, manifestRequest := admittedOpenClawProfileService(t)
	request := machineProfileRequest(manifestRequest.Manifest, "profile-service-publish")
	result, err := service.PublishMachineProfile(t.Context(), request)
	if err != nil || result.Replayed || result.AlreadyPublished || !result.Audited ||
		result.Record.Profile.ID != request.Profile.ID || result.Record.PublishedBy != request.Actor.AuthSubject {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	plan, err := st.ResolveMachineProfile(request.Profile.ID, request.Profile.Revision,
		appcatalog.Platform{OS: "linux", Arch: "amd64"})
	if err != nil || len(plan.Packages) != 1 || plan.Packages[0].Manifest.ID != "openclaw" || !plan.Packages[0].Direct {
		t.Fatalf("plan=%+v err=%v", plan, err)
	}
	var action, subject, outcome, authSubject, requestDigest string
	if err := st.DB().QueryRow(`SELECT action,subject,outcome,COALESCE(auth_subject,''),request_digest
	 FROM audit_log WHERE idempotency_key=?`, request.IdempotencyKey).Scan(
		&action, &subject, &outcome, &authSubject, &requestDigest); err != nil {
		t.Fatal(err)
	}
	if action != string(store.AuditMachineProfile) || subject != "openclaw-standard@1" ||
		outcome != "ok" || authSubject != request.Actor.AuthSubject ||
		requestDigest != MachineProfilePublishSemanticDigest(request) {
		t.Fatalf("audit action=%q subject=%q outcome=%q auth=%q digest=%q", action, subject, outcome, authSubject, requestDigest)
	}
}

func TestPublishMachineProfileReplayNeedsNoCurrentArtifactOrActor(t *testing.T) {
	service, _, dir, manifestRequest := admittedOpenClawProfileService(t)
	request := machineProfileRequest(manifestRequest.Manifest, "profile-service-replay")
	first, err := service.PublishMachineProfile(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, manifestRequest.Manifest.Artifact.SHA256+".tgz")); err != nil {
		t.Fatal(err)
	}
	replayRequest := request
	replayRequest.Actor = Actor{SourceAddr: "100.64.0.20", SourceKind: SourceKindOperatorAPI}
	replay, err := service.PublishMachineProfile(t.Context(), replayRequest)
	if err != nil || !replay.Replayed || replay.Record.Digest != first.Record.Digest ||
		replay.Record.PublishedBy != first.Record.PublishedBy {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}
}

func TestPublishMachineProfileRejectsMaterialRemovedAfterManifestAdmission(t *testing.T) {
	service, st, dir, manifestRequest := admittedOpenClawProfileService(t)
	if err := os.Remove(filepath.Join(dir, manifestRequest.Manifest.Artifact.SHA256+".tgz")); err != nil {
		t.Fatal(err)
	}
	request := machineProfileRequest(manifestRequest.Manifest, "profile-artifact-removed")
	_, err := service.PublishMachineProfile(t.Context(), request)
	assertDeploymentOperatorCode(t, err, store.OperatorCodeCatalogArtifactUnavailable, false)
	if count := countOperatorRows(t, st, "machine_profiles"); count != 0 {
		t.Fatalf("published profiles=%d", count)
	}
}

func TestPublishMachineProfileRejectsMissingGraphAndMetadataOnlyAdapter(t *testing.T) {
	service, st, _, manifestRequest := admittedOpenClawProfileService(t)
	missing := machineProfileRequest(manifestRequest.Manifest, "profile-missing-package")
	missing.Profile.Packages[0] = appcatalog.PackageRef{PackageID: "missing", Version: "1"}
	_, err := service.PublishMachineProfile(t.Context(), missing)
	assertDeploymentOperatorCode(t, err, store.OperatorCodeMachineProfileUnresolvable, false)

	node := appcatalog.Manifest{
		SchemaVersion: appcatalog.SchemaVersion, ID: "node-runtime", Version: "24.15.0",
		Kind: appcatalog.KindRuntime, Title: "Node.js",
		Source: appcatalog.Source{
			Catalog: "ai-intune", UpstreamURL: "https://nodejs.org/dist/v24.15.0/", Revision: "v24.15.0", License: "MIT",
		},
		Artifact:     appcatalog.Artifact{SHA256: "1111111111111111111111111111111111111111111111111111111111111111", Size: 1024},
		Adapter:      appcatalog.Adapter{Name: "unimplemented-runtime", Version: 1},
		Platforms:    []appcatalog.Platform{{OS: "linux", Arch: "amd64"}},
		Dependencies: []appcatalog.PackageRef{}, Provides: []string{"runtime.node"},
		Conflicts: []string{}, ExclusiveGroups: []string{},
	}
	if _, err := st.PublishCatalogManifest(node, "operator:test"); err != nil {
		t.Fatal(err)
	}
	unsupported := machineProfileRequest(node, "profile-node-metadata-only")
	unsupported.Profile.ID = "node-only"
	_, err = service.PublishMachineProfile(t.Context(), unsupported)
	assertDeploymentOperatorCode(t, err, store.OperatorCodeCatalogAdapterUnsupported, false)
	if count := countOperatorRows(t, st, "machine_profiles"); count != 0 {
		t.Fatalf("published profiles=%d", count)
	}
}

func TestMachineProfilePublishSemanticDigestUsesCanonicalPackageOrdering(t *testing.T) {
	manifest := appcatalog.Manifest{ID: "unused", Version: "1"}
	first := machineProfileRequest(manifest, "profile-digest")
	first.Profile.Packages = []appcatalog.PackageRef{
		{PackageID: "openclaw", Version: "1"}, {PackageID: "sidecar", Version: "2"},
	}
	second := first
	second.Profile.Packages = []appcatalog.PackageRef{
		{PackageID: "sidecar", Version: "2"}, {PackageID: "openclaw", Version: "1"},
	}
	if MachineProfilePublishSemanticDigest(first) != MachineProfilePublishSemanticDigest(second) {
		t.Fatal("canonical package ordering changed semantic profile digest")
	}
	second.Reason = "different reason"
	if MachineProfilePublishSemanticDigest(first) == MachineProfilePublishSemanticDigest(second) {
		t.Fatal("reason is absent from semantic profile digest")
	}
}

func enrollProfileAssignmentMachine(t *testing.T, st *store.Store, name string) string {
	t.Helper()
	machineID, token, err := st.CreateEnrollTokenFor(name, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.RedeemEnrollToken(token, model.EnrollRequest{
		SchemaVersion: model.SchemaVersion, EnrollToken: token, Hostname: name,
		UnixUser: "profile-test", OS: "linux", Arch: "amd64", AgentVersion: "test",
	}, time.Now().UTC().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	enabled := true
	now := time.Now().UTC().Truncate(time.Second)
	if err := st.RecordCheckin(machineID, model.Checkin{
		SchemaVersion: model.SchemaVersion, SentAt: now, AgentStartedAt: now.Add(-time.Hour),
		AgentSeq: 1, JobsEnabled: &enabled,
	}, now); err != nil {
		t.Fatal(err)
	}
	return machineID
}

func TestCatalogPlatformFromProbeIdentity(t *testing.T) {
	tests := []struct {
		name    string
		machine store.Machine
		want    appcatalog.Platform
		wantErr bool
	}{
		{name: "ubuntu x86", machine: store.Machine{OS: "Ubuntu 26.04 LTS", Arch: "x86_64"},
			want: appcatalog.Platform{OS: "linux", Arch: "amd64"}},
		{name: "onode arm", machine: store.Machine{OS: "Oracle Linux Server 9.7", Arch: "aarch64"},
			want: appcatalog.Platform{OS: "linux", Arch: "arm64"}},
		{name: "canonical", machine: store.Machine{OS: "linux", Arch: "amd64"},
			want: appcatalog.Platform{OS: "linux", Arch: "amd64"}},
		{name: "mac arm", machine: store.Machine{OS: "macOS 15.1", Arch: "arm64"},
			want: appcatalog.Platform{OS: "darwin", Arch: "arm64"}},
		{name: "mac x86", machine: store.Machine{OS: "macOS 15.1", Arch: "x86_64"},
			want: appcatalog.Platform{OS: "darwin", Arch: "amd64"}},
		{name: "windows 11 x86", machine: store.Machine{OS: "Windows 11 Pro", Arch: "x86_64"},
			want: appcatalog.Platform{OS: "windows", Arch: "amd64"}},
		{name: "windows tailscale", machine: store.Machine{OS: "windows", Arch: "amd64"},
			want: appcatalog.Platform{OS: "windows", Arch: "amd64"}},
		{name: "windows server arm", machine: store.Machine{OS: "Windows Server 2022", Arch: "aarch64"},
			want: appcatalog.Platform{OS: "windows", Arch: "arm64"}},
		{name: "microsoft windows", machine: store.Machine{OS: "Microsoft Windows 10", Arch: "x86_64"},
			want: appcatalog.Platform{OS: "windows", Arch: "amd64"}},
		{name: "missing os", machine: store.Machine{Arch: "x86_64"}, wantErr: true},
		{name: "unsupported arch", machine: store.Machine{OS: "Linux", Arch: "riscv64"}, wantErr: true},
		{name: "noncanonical arch", machine: store.Machine{OS: "Linux", Arch: " x86_64"}, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := catalogPlatformFromMachine(test.machine)
			if (err != nil) != test.wantErr || got != test.want {
				t.Fatalf("platform=%+v err=%v want=%+v wantErr=%t", got, err, test.want, test.wantErr)
			}
		})
	}
}

func checkWindowsIdentityCannotReuseOrExtendALinuxPinnedAssignment(t *testing.T) {
	service, st, _, manifestRequest := admittedOpenClawProfileService(t)
	profileRequest := machineProfileRequest(manifestRequest.Manifest, "windows-pin-hold-profile")
	if _, err := service.PublishMachineProfile(t.Context(), profileRequest); err != nil {
		t.Fatal(err)
	}
	machineID := enrollProfileAssignmentMachine(t, st, "windows-pin-hold-target")
	preview, err := service.PreviewMachineProfileAssignment(t.Context(), MachineProfileAssignmentPreviewRequest{
		MachineID: machineID, ProfileID: profileRequest.Profile.ID, ProfileRevision: profileRequest.Profile.Revision,
	})
	if err != nil {
		t.Fatal(err)
	}
	if preview.Target != (appcatalog.Platform{OS: "linux", Arch: "amd64"}) || preview.CreatesJobs != 1 {
		t.Fatalf("linux preview=%+v", preview)
	}
	first, err := service.AssignMachineProfile(t.Context(),
		profileAssignmentRequest(machineID, profileRequest, preview, "windows-pin-hold-linux"))
	if err != nil || first.AlreadyAssigned || len(first.Packages) != 1 {
		t.Fatalf("linux assign=%+v err=%v", first, err)
	}
	if _, err := st.DB().Exec(`UPDATE machine_registry SET os=?,arch=? WHERE machine_id=?`,
		"Windows 11 Pro", "x86_64", machineID); err != nil {
		t.Fatal(err)
	}
	_, err = service.PreviewMachineProfileAssignment(t.Context(), MachineProfileAssignmentPreviewRequest{
		MachineID: machineID, ProfileID: profileRequest.Profile.ID, ProfileRevision: profileRequest.Profile.Revision,
	})
	var rejection *store.OperatorRequestError
	if !errors.As(err, &rejection) || rejection.Code != store.OperatorCodeMachineProfileUnresolvable ||
		!errors.Is(err, store.ErrMachineProfileUnresolvable) {
		t.Fatalf("windows preview after linux pin rejection=%+v err=%v", rejection, err)
	}
	if strings.Contains(rejection.Detail, "agent 回報") {
		t.Fatalf("recognized Windows identity asked for another agent report: %q", rejection.Detail)
	}
	stale := profileAssignmentRequest(machineID, profileRequest, preview, "windows-pin-hold-stale-linux")
	_, err = service.AssignMachineProfile(t.Context(), stale)
	if !errors.As(err, &rejection) || rejection.Code != store.OperatorCodeMachineProfileUnresolvable {
		t.Fatalf("stale linux apply after Windows identity err=%v", err)
	}
	if countOperatorRows(t, st, "machine_profile_assignments") != 1 ||
		countOperatorRows(t, st, "jobs") != 1 {
		t.Fatalf("Windows identity created extra pin rows assignments=%d jobs=%d",
			countOperatorRows(t, st, "machine_profile_assignments"), countOperatorRows(t, st, "jobs"))
	}
	var targetOS, targetArch string
	if err := st.DB().QueryRow(`SELECT target_os,target_arch FROM machine_profile_assignments WHERE assignment_id=?`,
		first.AssignmentID).Scan(&targetOS, &targetArch); err != nil || targetOS != "linux" || targetArch != "amd64" {
		t.Fatalf("historical linux pin mutated to %s/%s err=%v", targetOS, targetArch, err)
	}
}

func TestMachineProfileAssignmentUsesLinuxProbePlatform(t *testing.T) {
	service, st, _, manifestRequest := admittedOpenClawProfileService(t)
	profileRequest := machineProfileRequest(manifestRequest.Manifest, "profile-native-platform-profile")
	if _, err := service.PublishMachineProfile(t.Context(), profileRequest); err != nil {
		t.Fatal(err)
	}
	machineID := enrollProfileAssignmentMachine(t, st, "profile-native-platform-target")
	if _, err := st.DB().Exec(`UPDATE machine_registry SET os=?,arch=? WHERE machine_id=?`,
		"Ubuntu 26.04 LTS", "x86_64", machineID); err != nil {
		t.Fatal(err)
	}
	preview, err := service.PreviewMachineProfileAssignment(t.Context(), MachineProfileAssignmentPreviewRequest{
		MachineID: machineID, ProfileID: profileRequest.Profile.ID, ProfileRevision: profileRequest.Profile.Revision,
	})
	if err != nil {
		t.Fatal(err)
	}
	if preview.Target != (appcatalog.Platform{OS: "linux", Arch: "amd64"}) || len(preview.Packages) != 1 {
		t.Fatalf("preview=%+v", preview)
	}
}

func TestDarwinNodeProfilePreviewApplyAndMissingArtifactFailure(t *testing.T) {
	type fixture struct {
		service     *Service
		store       *store.Store
		artifactDir string
		manifest    appcatalog.Manifest
		profile     MachineProfilePublishRequest
		machineID   string
	}
	newFixture := func(t *testing.T, suffix string) fixture {
		t.Helper()
		service, st, dir, _ := catalogManifestService(t)
		record := writeNodeRuntimeArtifact(t, dir, "24.15.0")
		manifest := nodeRuntimeManifest(record)
		manifest.Platforms = []appcatalog.Platform{{OS: "darwin", Arch: "arm64"}}
		if _, err := service.PublishCatalogManifest(t.Context(), CatalogManifestPublishRequest{
			Manifest: manifest, Reason: "approve Darwin Node runtime",
			IdempotencyKey: "darwin-node-manifest-" + suffix, Actor: verifiedDeploymentActor(),
		}); err != nil {
			t.Fatal(err)
		}
		profile := machineProfileRequest(manifest, "darwin-node-profile-"+suffix)
		profile.Profile.ID = "darwin-node-" + suffix
		if _, err := service.PublishMachineProfile(t.Context(), profile); err != nil {
			t.Fatal(err)
		}
		machineID := enrollProfileAssignmentMachine(t, st, "darwin-node-"+suffix)
		if _, err := st.DB().Exec(`UPDATE machine_registry SET os='macOS 15.7',arch='arm64' WHERE machine_id=?`, machineID); err != nil {
			t.Fatal(err)
		}
		return fixture{service: service, store: st, artifactDir: dir, manifest: manifest, profile: profile, machineID: machineID}
	}

	t.Run("preview and apply produce an exact Darwin job", func(t *testing.T) {
		f := newFixture(t, "apply")
		preview, err := f.service.PreviewMachineProfileAssignment(t.Context(), MachineProfileAssignmentPreviewRequest{
			MachineID: f.machineID, ProfileID: f.profile.Profile.ID, ProfileRevision: f.profile.Profile.Revision,
		})
		if err != nil || preview.Target != (appcatalog.Platform{OS: "darwin", Arch: "arm64"}) ||
			preview.CreatesJobs != 1 || len(preview.Packages) != 1 ||
			preview.Packages[0].ResourceKind != agentadapter.ExecutorKindNodeRuntime {
			t.Fatalf("preview=%+v err=%v", preview, err)
		}
		result, err := f.service.AssignMachineProfile(t.Context(),
			profileAssignmentRequest(f.machineID, f.profile, preview, "darwin-node-apply"))
		if err != nil || len(result.Packages) != 1 || result.Packages[0].JobID == "" {
			t.Fatalf("result=%+v err=%v", result, err)
		}
		desired, err := f.store.DesiredState(result.Packages[0].DesiredID)
		if err != nil {
			t.Fatal(err)
		}
		var spec model.NodeRuntimeSpec
		if err := json.Unmarshal([]byte(desired.Spec), &spec); err != nil ||
			spec.Kind != agentadapter.ExecutorKindNodeRuntime || spec.TargetOS != "darwin" ||
			spec.TargetArch != "arm64" || spec.Artifact == nil ||
			spec.Artifact.SHA256 != f.manifest.Artifact.SHA256 {
			t.Fatalf("desired=%+v spec=%+v err=%v", desired, spec, err)
		}
	})

	t.Run("artifact removed after preview rejects apply", func(t *testing.T) {
		f := newFixture(t, "missing")
		preview, err := f.service.PreviewMachineProfileAssignment(t.Context(), MachineProfileAssignmentPreviewRequest{
			MachineID: f.machineID, ProfileID: f.profile.Profile.ID, ProfileRevision: f.profile.Profile.Revision,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(filepath.Join(f.artifactDir, f.manifest.Artifact.SHA256+".tgz")); err != nil {
			t.Fatal(err)
		}
		_, err = f.service.AssignMachineProfile(t.Context(),
			profileAssignmentRequest(f.machineID, f.profile, preview, "darwin-node-missing-apply"))
		assertDeploymentOperatorCode(t, err, store.OperatorCodeCatalogArtifactUnavailable, false)
		for _, table := range []string{"machine_profile_assignments", "desired_state", "jobs"} {
			if count := countOperatorRows(t, f.store, table); count != 0 {
				t.Fatalf("missing Darwin artifact wrote %d rows to %s", count, table)
			}
		}
	})
}

func TestWindowsNodeProfilePreviewApplyAndMissingArtifactFailure(t *testing.T) {
	type fixture struct {
		service     *Service
		store       *store.Store
		artifactDir string
		manifest    appcatalog.Manifest
		profile     MachineProfilePublishRequest
		machineID   string
	}
	newFixture := func(t *testing.T, suffix string) fixture {
		t.Helper()
		service, st, dir, _ := catalogManifestService(t)
		record := writeNodeRuntimeArtifact(t, dir, "24.15.0")
		manifest := nodeRuntimeManifest(record)
		manifest.Platforms = []appcatalog.Platform{{OS: "windows", Arch: "amd64"}}
		if _, err := service.PublishCatalogManifest(t.Context(), CatalogManifestPublishRequest{
			Manifest: manifest, Reason: "approve Windows Node runtime",
			IdempotencyKey: "windows-node-manifest-" + suffix, Actor: verifiedDeploymentActor(),
		}); err != nil {
			t.Fatal(err)
		}
		profile := machineProfileRequest(manifest, "windows-node-profile-"+suffix)
		profile.Profile.ID = "windows-node-" + suffix
		if _, err := service.PublishMachineProfile(t.Context(), profile); err != nil {
			t.Fatal(err)
		}
		machineID := enrollProfileAssignmentMachine(t, st, "windows-node-"+suffix)
		if _, err := st.DB().Exec(`UPDATE machine_registry SET os='Windows 11 Pro',arch='amd64' WHERE machine_id=?`, machineID); err != nil {
			t.Fatal(err)
		}
		return fixture{service: service, store: st, artifactDir: dir, manifest: manifest, profile: profile, machineID: machineID}
	}

	t.Run("preview and apply produce an exact Windows job", func(t *testing.T) {
		f := newFixture(t, "apply")
		preview, err := f.service.PreviewMachineProfileAssignment(t.Context(), MachineProfileAssignmentPreviewRequest{
			MachineID: f.machineID, ProfileID: f.profile.Profile.ID, ProfileRevision: f.profile.Profile.Revision,
		})
		if err != nil || preview.Target != (appcatalog.Platform{OS: "windows", Arch: "amd64"}) ||
			preview.CreatesJobs != 1 || len(preview.Packages) != 1 ||
			preview.Packages[0].ResourceKind != agentadapter.ExecutorKindNodeRuntime {
			t.Fatalf("preview=%+v err=%v", preview, err)
		}
		result, err := f.service.AssignMachineProfile(t.Context(),
			profileAssignmentRequest(f.machineID, f.profile, preview, "windows-node-apply"))
		if err != nil || len(result.Packages) != 1 || result.Packages[0].JobID == "" {
			t.Fatalf("result=%+v err=%v", result, err)
		}
		desired, err := f.store.DesiredState(result.Packages[0].DesiredID)
		if err != nil {
			t.Fatal(err)
		}
		var spec model.NodeRuntimeSpec
		if err := json.Unmarshal([]byte(desired.Spec), &spec); err != nil ||
			spec.Kind != agentadapter.ExecutorKindNodeRuntime || spec.TargetOS != "windows" ||
			spec.TargetArch != "amd64" || spec.Artifact == nil ||
			spec.Artifact.SHA256 != f.manifest.Artifact.SHA256 {
			t.Fatalf("desired=%+v spec=%+v err=%v", desired, spec, err)
		}
	})

	t.Run("artifact removed after preview rejects apply", func(t *testing.T) {
		f := newFixture(t, "missing")
		preview, err := f.service.PreviewMachineProfileAssignment(t.Context(), MachineProfileAssignmentPreviewRequest{
			MachineID: f.machineID, ProfileID: f.profile.Profile.ID, ProfileRevision: f.profile.Profile.Revision,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(filepath.Join(f.artifactDir, f.manifest.Artifact.SHA256+".tgz")); err != nil {
			t.Fatal(err)
		}
		_, err = f.service.AssignMachineProfile(t.Context(),
			profileAssignmentRequest(f.machineID, f.profile, preview, "windows-node-missing-apply"))
		assertDeploymentOperatorCode(t, err, store.OperatorCodeCatalogArtifactUnavailable, false)
		for _, table := range []string{"machine_profile_assignments", "desired_state", "jobs"} {
			if count := countOperatorRows(t, f.store, table); count != 0 {
				t.Fatalf("missing Windows artifact wrote %d rows to %s", count, table)
			}
		}
	})
}

func profileAssignmentRequest(machineID string, profile MachineProfilePublishRequest,
	preview MachineProfileAssignmentPreviewResult, key string,
) MachineProfileAssignmentRequest {
	return MachineProfileAssignmentRequest{
		MachineID: machineID, ProfileID: profile.Profile.ID, ProfileRevision: profile.Profile.Revision,
		ConfirmDisplayName: preview.DisplayName, PreviewDigest: preview.PreviewDigest,
		Reason: "install managed profile", IdempotencyKey: key, Actor: verifiedDeploymentActor(),
	}
}

func TestAssignMachineProfileCreatesAtomicExecutableGraphAndReplaysWithoutMaterial(t *testing.T) {
	service, st, artifactDir, manifestRequest := admittedOpenClawProfileService(t)
	profileRequest := machineProfileRequest(manifestRequest.Manifest, "profile-assignment-profile")
	if _, err := service.PublishMachineProfile(t.Context(), profileRequest); err != nil {
		t.Fatal(err)
	}
	machineID := enrollProfileAssignmentMachine(t, st, "profile-target")
	preview, err := service.PreviewMachineProfileAssignment(t.Context(), MachineProfileAssignmentPreviewRequest{
		MachineID: machineID, ProfileID: profileRequest.Profile.ID, ProfileRevision: profileRequest.Profile.Revision,
	})
	if err != nil {
		t.Fatal(err)
	}
	if preview.MachineID != machineID || preview.DisplayName != "profile-target" ||
		preview.Target != (appcatalog.Platform{OS: "linux", Arch: "amd64"}) ||
		preview.AlreadyAssigned || !preview.ChangesMachineConfiguration || !preview.CreatesAssignment ||
		preview.CreatesDesiredStates != 1 || preview.CreatesJobs != 1 ||
		preview.JobsEnabled == nil || !*preview.JobsEnabled || !preview.EverReported ||
		preview.ActiveJobCount != 0 || len(preview.Blockers) != 0 || len(preview.Packages) != 1 ||
		preview.Packages[0].PackageID != "openclaw" || !preview.Packages[0].Direct ||
		preview.Packages[0].CurrentRevision != 0 || preview.Packages[0].PlannedRevision != 1 {
		t.Fatalf("preview=%+v", preview)
	}
	request := profileAssignmentRequest(machineID, profileRequest, preview, "profile-assignment-apply")
	result, err := service.AssignMachineProfile(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if result.Replayed || !result.Audited || result.AlreadyAssigned || result.AssignmentID == "" ||
		result.AssignmentRevision != 1 || len(result.Packages) != 1 ||
		result.Packages[0].JobID == "" || result.Packages[0].DesiredID == "" ||
		result.Packages[0].Revision != 1 {
		t.Fatalf("result=%+v", result)
	}
	desired, err := st.DesiredState(result.Packages[0].DesiredID)
	if err != nil {
		t.Fatal(err)
	}
	job, err := st.Job(result.Packages[0].JobID)
	if err != nil {
		t.Fatal(err)
	}
	if desired.ScopeType != "machine" || desired.ScopeID != machineID ||
		desired.ResourceKind != "openclaw" || desired.ResourceID != "openclaw" ||
		job.State != deploy.NotStarted || job.ArtifactDigest != "sha256:"+manifestRequest.Manifest.Artifact.SHA256 ||
		job.ExecutionTimeout != store.OperatorMachineProfileAssignmentDefaultTimeout {
		t.Fatalf("desired=%+v job=%+v", desired, job)
	}
	if _, err := st.DB().Exec(`UPDATE machine_profile_assignments SET profile_digest=profile_digest WHERE assignment_id=?`,
		result.AssignmentID); err == nil {
		t.Fatal("assignment row was mutable")
	}
	if _, err := st.DB().Exec(`DELETE FROM machine_profile_assignments WHERE assignment_id=?`,
		result.AssignmentID); err == nil {
		t.Fatal("assignment row was deletable")
	}
	if _, err := st.DB().Exec(`UPDATE machine_profile_assignment_packages SET direct=direct WHERE assignment_id=?`,
		result.AssignmentID); err == nil {
		t.Fatal("assignment package row was mutable")
	}
	if _, err := st.DB().Exec(`DELETE FROM machine_profile_assignment_packages WHERE assignment_id=?`,
		result.AssignmentID); err == nil {
		t.Fatal("assignment package row was deletable")
	}
	var foreignKeyViolations int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM pragma_foreign_key_check`).Scan(&foreignKeyViolations); err != nil || foreignKeyViolations != 0 {
		t.Fatalf("foreign key violations=%d err=%v", foreignKeyViolations, err)
	}
	for table, want := range map[string]int{
		"machine_profile_assignments": 1, "machine_profile_assignment_packages": 1,
		"desired_state": 1, "jobs": 1, "operator_idempotency": 3,
	} {
		if got := countOperatorRows(t, st, table); got != want {
			t.Fatalf("%s rows=%d want=%d", table, got, want)
		}
	}
	if err := os.Remove(filepath.Join(artifactDir, manifestRequest.Manifest.Artifact.SHA256+".tgz")); err != nil {
		t.Fatal(err)
	}
	replay, err := service.AssignMachineProfile(t.Context(), request)
	if err != nil || !replay.Replayed || !replay.Audited || replay.AssignmentID != result.AssignmentID ||
		replay.Packages[0].JobID != result.Packages[0].JobID {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}
	if countOperatorRows(t, st, "jobs") != 1 || countOperatorRows(t, st, "desired_state") != 1 {
		t.Fatal("idempotency replay created another graph")
	}
}

func TestAssignMachineProfileCreatesNodeBeforeOpenClawExecutionGraph(t *testing.T) {
	st := newDeploymentOperatorStore(t)
	artifactDir := t.TempDir()
	service := NewWithArtifacts(st, artifactDir)
	nodeRecord := writeNodeRuntimeArtifact(t, artifactDir, "24.15.0")
	nodeManifest := nodeRuntimeManifest(nodeRecord)
	if _, err := service.PublishCatalogManifest(t.Context(), CatalogManifestPublishRequest{
		Manifest: nodeManifest, Reason: "approve Node runtime", IdempotencyKey: "profile-graph-node-manifest",
		Actor: verifiedDeploymentActor(),
	}); err != nil {
		t.Fatal(err)
	}
	openRecord := writeDeploymentArtifact(t, artifactDir, "2026.9.10", ">=24.15.0 <25",
		time.Now().UTC().Truncate(time.Second))
	openManifest := catalogManifestForArtifact(openRecord)
	openManifest.Dependencies = []appcatalog.PackageRef{{PackageID: nodeManifest.ID, Version: nodeManifest.Version}}
	if _, err := service.PublishCatalogManifest(t.Context(), CatalogManifestPublishRequest{
		Manifest: openManifest, Reason: "approve OpenClaw with exact runtime",
		IdempotencyKey: "profile-graph-openclaw-manifest", Actor: verifiedDeploymentActor(),
	}); err != nil {
		t.Fatal(err)
	}
	profileRequest := machineProfileRequest(openManifest, "profile-graph-profile")
	if _, err := service.PublishMachineProfile(t.Context(), profileRequest); err != nil {
		t.Fatal(err)
	}
	machineID := enrollProfileAssignmentMachine(t, st, "profile-graph-target")
	if _, err := st.DB().Exec(`UPDATE machine_registry SET os=?,arch=? WHERE machine_id=?`,
		"Ubuntu 26.04 LTS", "x86_64", machineID); err != nil {
		t.Fatal(err)
	}
	preview, err := service.PreviewMachineProfileAssignment(t.Context(), MachineProfileAssignmentPreviewRequest{
		MachineID: machineID, ProfileID: profileRequest.Profile.ID, ProfileRevision: profileRequest.Profile.Revision,
	})
	if err != nil {
		t.Fatal(err)
	}
	if preview.CreatesDesiredStates != 2 || preview.CreatesJobs != 2 || len(preview.Packages) != 2 ||
		preview.Packages[0].PackageID != "node-runtime" || preview.Packages[0].Direct ||
		len(preview.Packages[0].PrerequisitePackages) != 0 ||
		preview.Packages[1].PackageID != "openclaw" || !preview.Packages[1].Direct ||
		len(preview.Packages[1].PrerequisitePackages) != 1 ||
		preview.Packages[1].PrerequisitePackages[0] != "node-runtime@24.15.0" {
		t.Fatalf("preview=%+v", preview)
	}
	result, err := service.AssignMachineProfile(t.Context(), profileAssignmentRequest(machineID,
		profileRequest, preview, "profile-graph-apply"))
	if err != nil || len(result.Packages) != 2 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if result.Packages[0].ResourceKind != "node-runtime" || result.Packages[1].ResourceKind != "openclaw" {
		t.Fatalf("packages=%+v", result.Packages)
	}
	var prerequisiteJobID string
	var position int
	if err := st.DB().QueryRow(`SELECT prerequisite_job_id,position FROM job_dependencies WHERE job_id=?`,
		result.Packages[1].JobID).Scan(&prerequisiteJobID, &position); err != nil {
		t.Fatal(err)
	}
	if prerequisiteJobID != result.Packages[0].JobID || position != 0 {
		t.Fatalf("prerequisite job=%q position=%d", prerequisiteJobID, position)
	}
	firstJob, found, err := st.NextJobForMachine(machineID)
	if err != nil || !found || firstJob.JobID != result.Packages[0].JobID {
		t.Fatalf("first job=%+v found=%t err=%v", firstJob, found, err)
	}
	terminalAt := time.Now().UTC().Truncate(time.Second)
	if _, err := st.DB().Exec(`UPDATE jobs SET state=?,terminal_at=? WHERE job_id=?`, deploy.Succeeded,
		terminalAt.Format(time.RFC3339), result.Packages[0].JobID); err != nil {
		t.Fatal(err)
	}
	secondJob, found, err := st.NextJobForMachine(machineID)
	if err != nil || !found || secondJob.JobID != result.Packages[1].JobID {
		t.Fatalf("second job=%+v found=%t err=%v", secondJob, found, err)
	}
	nodeDesired, err := st.DesiredState(result.Packages[0].DesiredID)
	if err != nil {
		t.Fatal(err)
	}
	var nodeSpec model.NodeRuntimeSpec
	if err := json.Unmarshal([]byte(nodeDesired.Spec), &nodeSpec); err != nil ||
		nodeSpec.TargetOS != "linux" || nodeSpec.TargetArch != "amd64" ||
		nodeSpec.BundleLayout != model.NodeRuntimeBundleLayoutV1 {
		t.Fatalf("node spec=%+v err=%v", nodeSpec, err)
	}
}

func TestAssignMachineProfileRejectsStaleRevisionWithoutPartialGraph(t *testing.T) {
	service, st, _, manifestRequest := admittedOpenClawProfileService(t)
	profileRequest := machineProfileRequest(manifestRequest.Manifest, "profile-assignment-stale-profile")
	if _, err := service.PublishMachineProfile(t.Context(), profileRequest); err != nil {
		t.Fatal(err)
	}
	machineID := enrollProfileAssignmentMachine(t, st, "profile-stale")
	preview, err := service.PreviewMachineProfileAssignment(t.Context(), MachineProfileAssignmentPreviewRequest{
		MachineID: machineID, ProfileID: profileRequest.Profile.ID, ProfileRevision: profileRequest.Profile.Revision,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AllocateRevision("openclaw:openclaw"); err != nil {
		t.Fatal(err)
	}
	_, err = service.AssignMachineProfile(t.Context(), profileAssignmentRequest(machineID, profileRequest,
		preview, "profile-assignment-stale"))
	if !errors.Is(err, store.ErrProfileAssignmentPreviewStale) {
		t.Fatalf("error=%v", err)
	}
	if countOperatorRows(t, st, "machine_profile_assignments") != 0 ||
		countOperatorRows(t, st, "machine_profile_assignment_packages") != 0 ||
		countOperatorRows(t, st, "desired_state") != 0 || countOperatorRows(t, st, "jobs") != 0 {
		t.Fatal("stale assignment left a partial graph")
	}
}

func TestAssignMachineProfileCompletedProfileIsNoopAndFailedProfileCreatesTrackedRetry(t *testing.T) {
	for _, terminal := range []struct {
		name            string
		state           deploy.JobState
		already         bool
		wantAssignments int
		wantJobs        int
		wantRevision    int64
	}{
		{name: "succeeded", state: deploy.Succeeded, already: true, wantAssignments: 1, wantJobs: 1, wantRevision: 1},
		{name: "failed", state: deploy.Failed, already: false, wantAssignments: 2, wantJobs: 2, wantRevision: 2},
	} {
		t.Run(terminal.name, func(t *testing.T) {
			service, st, _, manifestRequest := admittedOpenClawProfileService(t)
			profileRequest := machineProfileRequest(manifestRequest.Manifest, "profile-terminal-"+terminal.name)
			if _, err := service.PublishMachineProfile(t.Context(), profileRequest); err != nil {
				t.Fatal(err)
			}
			machineID := enrollProfileAssignmentMachine(t, st, "profile-terminal-target-"+terminal.name)
			preview, err := service.PreviewMachineProfileAssignment(t.Context(), MachineProfileAssignmentPreviewRequest{
				MachineID: machineID, ProfileID: profileRequest.Profile.ID, ProfileRevision: profileRequest.Profile.Revision,
			})
			if err != nil {
				t.Fatal(err)
			}
			first, err := service.AssignMachineProfile(t.Context(), profileAssignmentRequest(machineID,
				profileRequest, preview, "profile-terminal-first-"+terminal.name))
			if err != nil {
				t.Fatal(err)
			}
			terminalAt := time.Now().UTC().Truncate(time.Second)
			if _, err := st.DB().Exec(`UPDATE jobs SET state=?,terminal_at=? WHERE job_id=?`,
				terminal.state, terminalAt.Format(time.RFC3339), first.Packages[0].JobID); err != nil {
				t.Fatal(err)
			}
			nextPreview, err := service.PreviewMachineProfileAssignment(t.Context(), MachineProfileAssignmentPreviewRequest{
				MachineID: machineID, ProfileID: profileRequest.Profile.ID, ProfileRevision: profileRequest.Profile.Revision,
			})
			if err != nil {
				t.Fatal(err)
			}
			if nextPreview.AlreadyAssigned != terminal.already || len(nextPreview.Blockers) != 0 ||
				nextPreview.CreatesAssignment == terminal.already || nextPreview.CreatesJobs != map[bool]int{true: 0, false: 1}[terminal.already] {
				t.Fatalf("next preview=%+v", nextPreview)
			}
			if terminal.already && (nextPreview.Packages[0].CurrentRevision != first.Packages[0].Revision ||
				nextPreview.Packages[0].PlannedRevision != first.Packages[0].Revision) {
				t.Fatalf("noop preview revisions=%+v first=%+v", nextPreview.Packages[0], first.Packages[0])
			}
			second, err := service.AssignMachineProfile(t.Context(), profileAssignmentRequest(machineID,
				profileRequest, nextPreview, "profile-terminal-second-"+terminal.name))
			if err != nil {
				t.Fatal(err)
			}
			if second.AlreadyAssigned != terminal.already || second.AssignmentRevision != terminal.wantRevision ||
				countOperatorRows(t, st, "machine_profile_assignments") != terminal.wantAssignments ||
				countOperatorRows(t, st, "jobs") != terminal.wantJobs ||
				countOperatorRows(t, st, "desired_state") != terminal.wantJobs {
				t.Fatalf("second=%+v assignments=%d jobs=%d desired=%d", second,
					countOperatorRows(t, st, "machine_profile_assignments"), countOperatorRows(t, st, "jobs"),
					countOperatorRows(t, st, "desired_state"))
			}
			if terminal.already && second.AssignmentID != first.AssignmentID {
				t.Fatalf("noop assignment changed identity: first=%s second=%s", first.AssignmentID, second.AssignmentID)
			}
			if terminal.already && (second.Packages[0].CurrentRevision != first.Packages[0].Revision ||
				second.Packages[0].PlannedRevision != first.Packages[0].Revision ||
				second.Packages[0].Revision != first.Packages[0].Revision) {
				t.Fatalf("noop result revisions=%+v first=%+v", second.Packages[0], first.Packages[0])
			}
			if !terminal.already {
				var supersedes string
				if err := st.DB().QueryRow(`SELECT COALESCE(supersedes_assignment_id,'')
				 FROM machine_profile_assignments WHERE assignment_id=?`, second.AssignmentID).Scan(&supersedes); err != nil {
					t.Fatal(err)
				}
				if supersedes != first.AssignmentID || second.Packages[0].Revision != 2 {
					t.Fatalf("retry=%+v supersedes=%q", second, supersedes)
				}
			}
		})
	}
}

func TestAssignMachineProfileSucceededProfileWithNewerResourceRevisionCreatesTrackedApply(t *testing.T) {
	service, st, _, manifestRequest := admittedOpenClawProfileService(t)
	profileRequest := machineProfileRequest(manifestRequest.Manifest, "profile-newer-resource-profile")
	if _, err := service.PublishMachineProfile(t.Context(), profileRequest); err != nil {
		t.Fatal(err)
	}
	machineID := enrollProfileAssignmentMachine(t, st, "profile-newer-resource-target")
	preview, err := service.PreviewMachineProfileAssignment(t.Context(), MachineProfileAssignmentPreviewRequest{
		MachineID: machineID, ProfileID: profileRequest.Profile.ID, ProfileRevision: profileRequest.Profile.Revision,
	})
	if err != nil {
		t.Fatal(err)
	}
	first, err := service.AssignMachineProfile(t.Context(), profileAssignmentRequest(machineID,
		profileRequest, preview, "profile-newer-resource-first"))
	if err != nil {
		t.Fatal(err)
	}
	terminalAt := time.Now().UTC().Truncate(time.Second)
	if _, err := st.DB().Exec(`UPDATE jobs SET state=?,terminal_at=? WHERE job_id=?`,
		deploy.Succeeded, terminalAt.Format(time.RFC3339), first.Packages[0].JobID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AllocateRevision("openclaw:openclaw"); err != nil {
		t.Fatal(err)
	}
	nextPreview, err := service.PreviewMachineProfileAssignment(t.Context(), MachineProfileAssignmentPreviewRequest{
		MachineID: machineID, ProfileID: profileRequest.Profile.ID, ProfileRevision: profileRequest.Profile.Revision,
	})
	if err != nil {
		t.Fatal(err)
	}
	if nextPreview.AlreadyAssigned || !nextPreview.CreatesAssignment || nextPreview.CreatesJobs != 1 ||
		nextPreview.Packages[0].CurrentRevision != 2 || nextPreview.Packages[0].PlannedRevision != 3 ||
		len(nextPreview.Blockers) != 0 {
		t.Fatalf("preview=%+v", nextPreview)
	}
	second, err := service.AssignMachineProfile(t.Context(), profileAssignmentRequest(machineID,
		profileRequest, nextPreview, "profile-newer-resource-second"))
	if err != nil {
		t.Fatal(err)
	}
	if second.AssignmentRevision != 2 || second.Packages[0].Revision != 3 ||
		countOperatorRows(t, st, "machine_profile_assignments") != 2 || countOperatorRows(t, st, "jobs") != 2 {
		t.Fatalf("second=%+v assignments=%d jobs=%d", second,
			countOperatorRows(t, st, "machine_profile_assignments"), countOperatorRows(t, st, "jobs"))
	}
}

func TestAssignMachineProfileAtomicallyReplacesDifferentProfile(t *testing.T) {
	service, st, _, manifestRequest := admittedOpenClawProfileService(t)
	firstProfile := machineProfileRequest(manifestRequest.Manifest, "profile-replacement-first-profile")
	firstProfile.Profile.ID = "openclaw-primary"
	if _, err := service.PublishMachineProfile(t.Context(), firstProfile); err != nil {
		t.Fatal(err)
	}
	secondProfile := machineProfileRequest(manifestRequest.Manifest, "profile-replacement-second-profile")
	secondProfile.Profile.ID = "openclaw-secondary"
	if _, err := service.PublishMachineProfile(t.Context(), secondProfile); err != nil {
		t.Fatal(err)
	}
	machineID := enrollProfileAssignmentMachine(t, st, "profile-replacement-target")
	firstPreview, err := service.PreviewMachineProfileAssignment(t.Context(), MachineProfileAssignmentPreviewRequest{
		MachineID: machineID, ProfileID: firstProfile.Profile.ID, ProfileRevision: firstProfile.Profile.Revision,
	})
	if err != nil {
		t.Fatal(err)
	}
	first, err := service.AssignMachineProfile(t.Context(), profileAssignmentRequest(machineID,
		firstProfile, firstPreview, "profile-replacement-first-apply"))
	if err != nil {
		t.Fatal(err)
	}
	terminalAt := time.Now().UTC().Truncate(time.Second)
	if _, err := st.DB().Exec(`UPDATE jobs SET state=?,terminal_at=? WHERE job_id=?`,
		deploy.Succeeded, terminalAt.Format(time.RFC3339), first.Packages[0].JobID); err != nil {
		t.Fatal(err)
	}
	secondPreview, err := service.PreviewMachineProfileAssignment(t.Context(), MachineProfileAssignmentPreviewRequest{
		MachineID: machineID, ProfileID: secondProfile.Profile.ID, ProfileRevision: secondProfile.Profile.Revision,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(secondPreview.Blockers) != 0 || !secondPreview.ChangesMachineConfiguration ||
		!secondPreview.CreatesAssignment || secondPreview.CurrentAssignmentID != first.AssignmentID ||
		secondPreview.CurrentAssignmentRevision != 1 {
		t.Fatalf("preview=%+v", secondPreview)
	}
	second, err := service.AssignMachineProfile(t.Context(), profileAssignmentRequest(machineID,
		secondProfile, secondPreview, "profile-replacement-second-apply"))
	if err != nil {
		t.Fatal(err)
	}
	if second.AssignmentRevision != 2 || second.AlreadyAssigned || len(second.Packages) != 1 ||
		second.Packages[0].Revision != 2 || countOperatorRows(t, st, "machine_profile_assignments") != 2 ||
		countOperatorRows(t, st, "machine_profile_assignment_packages") != 2 ||
		countOperatorRows(t, st, "jobs") != 2 || countOperatorRows(t, st, "desired_state") != 2 {
		t.Fatalf("replacement=%+v", second)
	}
	var supersedes string
	if err := st.DB().QueryRow(`SELECT COALESCE(supersedes_assignment_id,'') FROM machine_profile_assignments
		WHERE assignment_id=?`, second.AssignmentID).Scan(&supersedes); err != nil || supersedes != first.AssignmentID {
		t.Fatalf("supersedes=%q err=%v", supersedes, err)
	}
}

func TestMachineProfileAssignmentSemanticDigestBindsMeaningNotTransport(t *testing.T) {
	request := MachineProfileAssignmentRequest{
		MachineID: "machine-a", ProfileID: "openclaw-standard", ProfileRevision: 7,
		ConfirmDisplayName: "cnode", PreviewDigest: "sha256:" + strings.Repeat("a", 64),
		Reason: "install selected profile", IdempotencyKey: "first",
		Actor: Actor{AuthSubject: "operator-a", SourceAddr: "100.64.0.1"},
	}
	digest := MachineProfileAssignmentSemanticDigest(request)
	transport := request
	transport.IdempotencyKey = "second"
	transport.Actor = Actor{AuthSubject: "operator-b", SourceAddr: "100.64.0.2"}
	if MachineProfileAssignmentSemanticDigest(transport) != digest {
		t.Fatal("transport metadata changed assignment semantic digest")
	}
	changed := request
	changed.ProfileRevision++
	if MachineProfileAssignmentSemanticDigest(changed) == digest {
		t.Fatal("profile revision was absent from assignment semantic digest")
	}
	changed = request
	changed.PreviewDigest = "sha256:" + strings.Repeat("b", 64)
	if MachineProfileAssignmentSemanticDigest(changed) == digest {
		t.Fatal("preview digest was absent from assignment semantic digest")
	}
}

// TestProfileAssignmentPreviewAndApplyGiveTheOperatorTheSameSentence 確認兩條路徑
// 給操作員相同的文案。preview 路徑的 PreviewMachineProfileAssignment 會把
// prepareMachineProfileAssignment 回傳的 *store.OperatorRequestError 原樣交出去；
// 操作員在 JSON message 與 Web 上讀到的，就是 operator 這一側寫死的那一句。
// apply 路徑的 AssignMachineProfile 進入 store 後只取 code，Detail 被丟掉，再由
// canonicalOperatorProfileAssignmentRejectionDetail 的表重建，因此同一個失敗有
// 兩個文案來源。全樹沒有任何測試提到那些句子，兩邊任一邊被改寫都不會有人紅。
// 這支測試刻意不抄任何一句文案，只比對兩邊是否相同；抄進來就等於再開一份清單。
// 這支測試守不到 verifyMachineProfilePlan 那一族：
// prepareMachineProfileAssignment 會先呼叫它，而那一族使用
// machineProfileRejection，Detail 就是 code 字串本身。實際量過，把 artifactDir
// 裡的 artifact 移除後預覽，preview 交給操作員的是
// "CATALOG_ARTIFACT_UNAVAILABLE" 這個代碼字串，apply 交出的是表裡的中文句子。
// 要讓那一列也綠，得先在 production 把兩份文案收斂成同一個來源，所以這裡沒有
// 把它放進表內。
func TestProfileAssignmentPreviewAndApplyGiveTheOperatorTheSameSentence(t *testing.T) {
	service, st, _, manifestRequest := admittedOpenClawProfileService(t)
	profileRequest := machineProfileRequest(manifestRequest.Manifest, "profile-seam-profile")
	if _, err := service.PublishMachineProfile(t.Context(), profileRequest); err != nil {
		t.Fatal(err)
	}

	assertSameSentence := func(t *testing.T, machineID, profileID string, profileRevision int64,
		idempotencyKey string,
	) {
		t.Helper()
		_, previewErr := service.PreviewMachineProfileAssignment(t.Context(),
			MachineProfileAssignmentPreviewRequest{
				MachineID: machineID, ProfileID: profileID, ProfileRevision: profileRevision,
			})
		_, applyErr := service.AssignMachineProfile(t.Context(), MachineProfileAssignmentRequest{
			MachineID: machineID, ProfileID: profileID, ProfileRevision: profileRevision,
			Reason: "seam check", IdempotencyKey: idempotencyKey, Actor: verifiedDeploymentActor(),
		})

		if previewErr == nil {
			t.Fatal("preview error is nil")
		}
		var previewRequestErr *store.OperatorRequestError
		if !errors.As(previewErr, &previewRequestErr) {
			t.Fatalf("preview error is not *store.OperatorRequestError: %v", previewErr)
		}
		if applyErr == nil {
			t.Fatal("apply error is nil")
		}
		var applyRequestErr *store.OperatorRequestError
		if !errors.As(applyErr, &applyRequestErr) {
			t.Fatalf("apply error is not *store.OperatorRequestError: %v", applyErr)
		}
		if previewRequestErr.Code != applyRequestErr.Code {
			t.Fatalf("preview code=%q apply code=%q", previewRequestErr.Code, applyRequestErr.Code)
		}
		if previewRequestErr.Detail != applyRequestErr.Detail {
			t.Fatalf("同一個失敗在預覽與套用講了兩句不同的話，操作員會以為那是兩件事：code=%q preview detail=%q apply detail=%q",
				previewRequestErr.Code, previewRequestErr.Detail, applyRequestErr.Detail)
		}
		if applyRequestErr.Detail == "" {
			t.Fatal("apply detail is empty")
		}
		if previewRequestErr.Detail == previewRequestErr.Code {
			t.Fatalf("這裡把錯誤代碼當成給人看的句子交出去了：code=%q", previewRequestErr.Code)
		}
	}

	t.Run("機器不在名冊上", func(t *testing.T) {
		assertSameSentence(t, "machine-not-enrolled", profileRequest.Profile.ID,
			profileRequest.Profile.Revision, "profile-seam-not-enrolled")
	})

	t.Run("profile 不存在", func(t *testing.T) {
		machineID := enrollProfileAssignmentMachine(t, st, "profile-seam-missing")
		assertSameSentence(t, machineID, "no-such-profile", 1, "profile-seam-missing-profile")
	})

	t.Run("機器平台解析不出來", func(t *testing.T) {
		machineID := enrollProfileAssignmentMachine(t, st, "profile-seam-platform")
		if _, err := st.DB().Exec(`UPDATE machine_registry SET arch=? WHERE machine_id=?`,
			"riscv64", machineID); err != nil {
			t.Fatal(err)
		}
		assertSameSentence(t, machineID, profileRequest.Profile.ID,
			profileRequest.Profile.Revision, "profile-seam-unresolved-platform")
	})
}
