package store

import (
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	appcatalog "github.com/teddashh/AI-Intune/internal/catalog"
)

func storedCatalogManifest(id, version, digest string, kind appcatalog.PackageKind) appcatalog.Manifest {
	return appcatalog.Manifest{
		SchemaVersion: appcatalog.SchemaVersion,
		ID:            id,
		Version:       version,
		Kind:          kind,
		Title:         id,
		Source: appcatalog.Source{
			Catalog: "ai-intune", UpstreamURL: "https://github.com/example/" + id,
			Revision: version, License: "MIT",
		},
		Artifact:     appcatalog.Artifact{SHA256: digest, Size: 1024},
		Adapter:      appcatalog.Adapter{Name: id, Version: 1},
		Platforms:    []appcatalog.Platform{{OS: "linux", Arch: "arm64"}, {OS: "linux", Arch: "amd64"}},
		Dependencies: []appcatalog.PackageRef{}, Provides: []string{"package." + id},
		Conflicts: []string{}, ExclusiveGroups: []string{},
	}
}

func storedCatalogFixture() (appcatalog.Manifest, appcatalog.Manifest) {
	node := storedCatalogManifest("node-runtime", "24.15.0",
		"1111111111111111111111111111111111111111111111111111111111111111", appcatalog.KindRuntime)
	node.Provides = []string{"runtime.node"}
	openclaw := storedCatalogManifest("openclaw", "2026.9.2",
		"2222222222222222222222222222222222222222222222222222222222222222", appcatalog.KindApp)
	openclaw.Dependencies = []appcatalog.PackageRef{{PackageID: node.ID, Version: node.Version}}
	openclaw.Provides = []string{appcatalog.CapabilityAgentRuntime}
	openclaw.Conflicts = []string{"hermes-agent"}
	openclaw.ExclusiveGroups = []string{appcatalog.ExclusiveGroupPrimaryAgentRuntime}
	return node, openclaw
}

func TestCatalogManifestPublicationIsCanonicalImmutableAndReplayable(t *testing.T) {
	st := newTestStore(t)
	now := time.Date(2026, 9, 10, 8, 0, 0, 0, time.UTC)
	st.nowFn = func() time.Time { return now }
	node, openclaw := storedCatalogFixture()

	first, err := st.PublishCatalogManifest(openclaw, "operator:test")
	if err != nil {
		t.Fatal(err)
	}
	if first.Replayed || first.Manifest.Platforms[0].Arch != "amd64" ||
		len(first.Digest) != len("sha256:")+64 || !first.PublishedAt.Equal(now) {
		t.Fatalf("first=%+v", first)
	}
	reordered := openclaw
	reordered.Platforms[0], reordered.Platforms[1] = reordered.Platforms[1], reordered.Platforms[0]
	replay, err := st.PublishCatalogManifest(reordered, "operator:retry")
	if err != nil || !replay.Replayed || replay.Digest != first.Digest || replay.PublishedBy != "operator:test" {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}
	changed := openclaw
	changed.Title = "Different title"
	if _, err := st.PublishCatalogManifest(changed, "operator:test"); !errors.Is(err, ErrCatalogManifestConflict) {
		t.Fatalf("conflict err=%v", err)
	}
	if _, err := st.PublishCatalogManifest(node, "operator:test"); err != nil {
		t.Fatal(err)
	}
	records, err := st.CatalogManifests()
	if err != nil || len(records) != 2 || records[0].Manifest.ID != "node-runtime" || records[1].Manifest.ID != "openclaw" {
		t.Fatalf("records=%+v err=%v", records, err)
	}
}

func TestMachineProfilePublicationRequiresAResolvableCatalog(t *testing.T) {
	st := newTestStore(t)
	node, openclaw := storedCatalogFixture()
	profile := appcatalog.MachineProfile{
		SchemaVersion: appcatalog.SchemaVersion, ID: "openclaw-standard", Revision: 1,
		Packages: []appcatalog.PackageRef{{PackageID: openclaw.ID, Version: openclaw.Version}},
	}
	if _, err := st.PublishMachineProfile(profile, "operator:test"); err == nil {
		t.Fatal("profile without catalog material was published")
	}
	for _, manifest := range []appcatalog.Manifest{openclaw, node} {
		if _, err := st.PublishCatalogManifest(manifest, "operator:test"); err != nil {
			t.Fatal(err)
		}
	}
	published, err := st.PublishMachineProfile(profile, "operator:test")
	if err != nil || published.Replayed || published.Profile.ID != profile.ID {
		t.Fatalf("published=%+v err=%v", published, err)
	}
	replay, err := st.PublishMachineProfile(profile, "operator:retry")
	if err != nil || !replay.Replayed || replay.Digest != published.Digest || replay.PublishedBy != "operator:test" {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}
	plan, err := st.ResolveMachineProfile(profile.ID, profile.Revision, appcatalog.Platform{OS: "linux", Arch: "amd64"})
	if err != nil || len(plan.Packages) != 2 || plan.Packages[0].Manifest.ID != node.ID ||
		plan.Packages[1].Manifest.ID != openclaw.ID || !plan.Packages[1].Direct {
		t.Fatalf("plan=%+v err=%v", plan, err)
	}
	changed := profile
	changed.Packages = []appcatalog.PackageRef{{PackageID: node.ID, Version: node.Version}}
	if _, err := st.PublishMachineProfile(changed, "operator:test"); !errors.Is(err, ErrMachineProfileConflict) {
		t.Fatalf("profile conflict err=%v", err)
	}
}

func TestMachineProfilePublicationRejectsExclusiveRuntimeConflict(t *testing.T) {
	st := newTestStore(t)
	node, openclaw := storedCatalogFixture()
	hermes := storedCatalogManifest("hermes-agent", "1.0.0",
		"3333333333333333333333333333333333333333333333333333333333333333", appcatalog.KindApp)
	hermes.Provides = []string{appcatalog.CapabilityAgentRuntime}
	hermes.Conflicts = []string{"openclaw"}
	hermes.ExclusiveGroups = []string{appcatalog.ExclusiveGroupPrimaryAgentRuntime}
	for _, manifest := range []appcatalog.Manifest{node, openclaw, hermes} {
		if _, err := st.PublishCatalogManifest(manifest, "operator:test"); err != nil {
			t.Fatal(err)
		}
	}
	profile := appcatalog.MachineProfile{
		SchemaVersion: appcatalog.SchemaVersion, ID: "two-agent-runtimes", Revision: 1,
		Packages: []appcatalog.PackageRef{
			{PackageID: openclaw.ID, Version: openclaw.Version},
			{PackageID: hermes.ID, Version: hermes.Version},
		},
	}
	_, err := st.PublishMachineProfile(profile, "operator:test")
	var resolution *appcatalog.ResolutionError
	if !errors.As(err, &resolution) || resolution.Code != appcatalog.CodeExclusiveGroupConflict {
		t.Fatalf("resolution=%+v err=%v", resolution, err)
	}
	if countRows(t, st, `SELECT COUNT(*) FROM machine_profiles`) != 0 {
		t.Fatal("conflicting profile was persisted")
	}
}

func TestCatalogReadsDetectLedgerTampering(t *testing.T) {
	st := newTestStore(t)
	node, _ := storedCatalogFixture()
	if _, err := st.PublishCatalogManifest(node, "operator:test"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`UPDATE catalog_manifests SET manifest_digest='sha256:tampered'`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CatalogManifest(node.ID, node.Version); !errors.Is(err, ErrCatalogRecordCorrupt) {
		t.Fatalf("tampered record err=%v", err)
	}
}

func TestCatalogReadsReturnTypedNotFoundAndDetectProfileTampering(t *testing.T) {
	st := newTestStore(t)
	if _, err := st.CatalogManifest("missing", "1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing manifest err=%v", err)
	}
	if _, err := st.MachineProfile("missing", 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing profile err=%v", err)
	}
	node, openclaw := storedCatalogFixture()
	for _, manifest := range []appcatalog.Manifest{node, openclaw} {
		if _, err := st.PublishCatalogManifest(manifest, "operator:test"); err != nil {
			t.Fatal(err)
		}
	}
	profile := appcatalog.MachineProfile{
		SchemaVersion: appcatalog.SchemaVersion, ID: "tamper-target", Revision: 1,
		Packages: []appcatalog.PackageRef{{PackageID: openclaw.ID, Version: openclaw.Version}},
	}
	if _, err := st.PublishMachineProfile(profile, "operator:test"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`UPDATE machine_profiles SET profile_digest='sha256:tampered'`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.MachineProfile(profile.ID, profile.Revision); !errors.Is(err, ErrCatalogRecordCorrupt) {
		t.Fatalf("tampered profile err=%v", err)
	}
}

func TestMachineProfileIdentityConflictPrecedesNewCatalogResolution(t *testing.T) {
	st := newTestStore(t)
	node, openclaw := storedCatalogFixture()
	for _, manifest := range []appcatalog.Manifest{node, openclaw} {
		if _, err := st.PublishCatalogManifest(manifest, "operator:test"); err != nil {
			t.Fatal(err)
		}
	}
	profile := appcatalog.MachineProfile{
		SchemaVersion: appcatalog.SchemaVersion, ID: "immutable", Revision: 1,
		Packages: []appcatalog.PackageRef{{PackageID: openclaw.ID, Version: openclaw.Version}},
	}
	if _, err := st.PublishMachineProfile(profile, "operator:test"); err != nil {
		t.Fatal(err)
	}
	profile.Packages = []appcatalog.PackageRef{{PackageID: "unpublished", Version: "1"}}
	if _, err := st.PublishMachineProfile(profile, "operator:test"); !errors.Is(err, ErrMachineProfileConflict) {
		t.Fatalf("identity conflict err=%v", err)
	}
}

func TestConcurrentCatalogPublicationCreatesOneImmutableRecord(t *testing.T) {
	st := newTestStore(t)
	node, _ := storedCatalogFixture()
	var fresh, replayed, failed atomic.Int64
	var wait sync.WaitGroup
	for i := 0; i < 16; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			record, err := st.PublishCatalogManifest(node, "operator:test")
			if err != nil {
				failed.Add(1)
			} else if record.Replayed {
				replayed.Add(1)
			} else {
				fresh.Add(1)
			}
		}()
	}
	wait.Wait()
	if fresh.Load() != 1 || replayed.Load() != 15 || failed.Load() != 0 ||
		countRows(t, st, `SELECT COUNT(*) FROM catalog_manifests`) != 1 {
		t.Fatalf("fresh=%d replayed=%d failed=%d", fresh.Load(), replayed.Load(), failed.Load())
	}
}

func TestCatalogPublicationCapacityIsAtomicAndStillAllowsReplay(t *testing.T) {
	st := newTestStore(t)
	node, _ := storedCatalogFixture()
	first, err := st.PublishCatalogManifest(node, "operator:test")
	if err != nil {
		t.Fatal(err)
	}
	tx, err := st.DB().Begin()
	if err != nil {
		t.Fatal(err)
	}
	stmt, err := tx.Prepare(`INSERT INTO catalog_manifests
	 (package_id,package_version,manifest_json,manifest_digest,published_at,published_by)
	 VALUES (?,?,?,?,?,?)`)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i < maxStoredCatalogRecords; i++ {
		if _, err := stmt.Exec("capacity-"+strconv.Itoa(i), "1", `{}`, "sha256:x", "2026-09-10T12:00:00Z", "test"); err != nil {
			t.Fatal(err)
		}
	}
	if err := stmt.Close(); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	replay, err := st.PublishCatalogManifest(node, "operator:retry")
	if err != nil || !replay.Replayed || replay.Digest != first.Digest {
		t.Fatalf("capacity replay=%+v err=%v", replay, err)
	}
	overflow := storedCatalogManifest("overflow", "1",
		"4444444444444444444444444444444444444444444444444444444444444444", appcatalog.KindApp)
	if _, err := st.PublishCatalogManifest(overflow, "operator:test"); !errors.Is(err, ErrCatalogCapacity) {
		t.Fatalf("overflow err=%v", err)
	}
	if countRows(t, st, `SELECT COUNT(*) FROM catalog_manifests`) != maxStoredCatalogRecords {
		t.Fatal("overflow publication changed the catalog")
	}
}
