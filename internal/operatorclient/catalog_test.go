package operatorclient

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	appcatalog "github.com/teddashh/AI-Intune/internal/catalog"
	"github.com/teddashh/AI-Intune/internal/deploy"
	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/store"
)

func catalogClientManifest() appcatalog.Manifest {
	return appcatalog.Manifest{
		SchemaVersion: appcatalog.SchemaVersion, ID: "node-runtime", Version: "24.21.0",
		Kind: appcatalog.KindRuntime, Title: "Node.js",
		Source: appcatalog.Source{
			Catalog: "nodejs.org", UpstreamURL: "https://nodejs.org/dist/v24.21.0/",
			Revision: "v24.21.0", License: "MIT",
		},
		Artifact: appcatalog.Artifact{SHA256: strings.Repeat("a", 64), Size: 1024},
		Adapter:  appcatalog.Adapter{Name: "node-runtime", Version: 1},
		Platforms: []appcatalog.Platform{
			{OS: "linux", Arch: "amd64"}, {OS: "linux", Arch: "arm64"},
		},
		Dependencies: []appcatalog.PackageRef{}, Provides: []string{"runtime.node"},
		Conflicts: []string{}, ExclusiveGroups: []string{},
	}
}

func catalogClientProfile() appcatalog.MachineProfile {
	return appcatalog.MachineProfile{
		SchemaVersion: appcatalog.SchemaVersion, ID: "standard", Revision: 1,
		Packages: []appcatalog.PackageRef{{PackageID: "node-runtime", Version: "24.21.0"}},
	}
}

func catalogClientHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "private, no-store")
}

func catalogClientImpact() store.OperatorMachineProfileAssignmentPackageImpact {
	return store.OperatorMachineProfileAssignmentPackageImpact{
		Position: 0, PackageID: "node-runtime", PackageVersion: "24.21.0",
		ManifestDigest: "sha256:" + strings.Repeat("b", 64), ResourceKind: "node-runtime", ResourceID: "node-runtime",
		SpecDigest: "sha256:" + strings.Repeat("c", 64), ArtifactDigest: "sha256:" + strings.Repeat("a", 64),
		ExecutionTimeoutSeconds: 600, Direct: true, PrerequisitePackages: []string{},
		CurrentRevision: 0, PlannedRevision: 1,
	}
}

func TestCatalogClientRoutesSchemasAndReplayEvidence(t *testing.T) {
	now := time.Date(2026, 9, 10, 16, 0, 0, 0, time.UTC)
	manifest := catalogClientManifest()
	manifestDigest := catalogManifestWireDigest(manifest)
	profile := catalogClientProfile()
	profileDigest := machineProfileWireDigest(profile)
	previewDigest := "sha256:" + strings.Repeat("d", 64)
	var manifestWrites, profileWrites, assignmentWrites atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		catalogClientHeaders(w)
		if r.Header.Get("Accept") != "application/json" || r.UserAgent() != UserAgent {
			t.Errorf("headers=%v", r.Header)
		}
		switch r.URL.Path {
		case "/v1/operator/catalog-manifests/standard-preview":
			if r.Method != http.MethodPost || r.Header.Get("Idempotency-Key") != "" {
				t.Errorf("standard preview method=%s headers=%v", r.Method, r.Header)
			}
			_ = json.NewEncoder(w).Encode(operator.StandardCatalogManifestPreviewResult{
				SchemaVersion: operator.StandardCatalogPreviewSchemaVersion, Manifest: manifest,
				ManifestDigest: manifestDigest, PreviewedAt: now, PreviewDigest: previewDigest,
			})
		case "/v1/operator/catalog-manifests":
			if r.Method == http.MethodGet {
				if r.URL.Query().Get("package_id") != "node-runtime" || r.URL.Query().Get("kind") != "runtime" ||
					r.URL.Query().Get("limit") != "1" || r.Header.Get("Idempotency-Key") != "" {
					t.Errorf("manifest list url=%s headers=%v", r.URL, r.Header)
				}
				_ = json.NewEncoder(w).Encode(operator.CatalogManifestListResult{
					SchemaVersion: operator.CatalogReadSchemaVersion, Consistency: operator.CatalogReadConsistencyLive,
					EvaluatedAt: now, Total: 1, Items: []operator.CatalogManifestRecord{{
						Manifest: manifest, Digest: manifestDigest, PublishedAt: now,
					}},
				})
				return
			}
			if r.Header.Get("Idempotency-Key") != "manifest-key" {
				t.Errorf("manifest mutation headers=%v", r.Header)
			}
			raw, _ := io.ReadAll(r.Body)
			var fields map[string]json.RawMessage
			if json.Unmarshal(raw, &fields) != nil || fields["actor"] != nil || fields["idempotency_key"] != nil ||
				len(fields) != 5 {
				t.Errorf("manifest body=%s", raw)
			}
			n := manifestWrites.Add(1)
			result := operator.CatalogManifestPublishResult{Record: operator.CatalogManifestRecord{
				Manifest: manifest, Digest: manifestDigest, PublishedAt: now,
			}}
			if n == 2 {
				result.Replayed = true
				w.Header().Set("Idempotency-Replayed", "true")
				w.WriteHeader(http.StatusOK)
			} else {
				w.WriteHeader(http.StatusCreated)
			}
			_ = json.NewEncoder(w).Encode(result)
		case "/v1/operator/machine-profiles":
			if r.Method == http.MethodGet {
				_ = json.NewEncoder(w).Encode(operator.MachineProfileListResult{
					SchemaVersion: operator.CatalogReadSchemaVersion, Consistency: operator.CatalogReadConsistencyLive,
					EvaluatedAt: now, Total: 1, Items: []operator.MachineProfileRecord{{
						Profile: profile, Digest: profileDigest, PublishedAt: now,
					}},
				})
				return
			}
			profileWrites.Add(1)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(operator.MachineProfilePublishResult{Record: operator.MachineProfileRecord{
				Profile: profile, Digest: profileDigest, PublishedAt: now,
			}})
		case "/v1/operator/machine-profiles/preview":
			_ = json.NewEncoder(w).Encode(operator.MachineProfilePreviewResult{
				SchemaVersion: operator.MachineProfilePreviewSchemaVersion, Profile: profile,
				ProfileDigest: profileDigest, PreviewedAt: now, PreviewDigest: previewDigest,
			})
		case "/v1/operator/machines/machine-1/profile-assignment-preview":
			impact := catalogClientImpact()
			_ = json.NewEncoder(w).Encode(operator.MachineProfileAssignmentPreviewResult{
				MachineID: "machine-1", DisplayName: "samplehub1", LifecycleRevision: 0,
				ProfileID: "standard", ProfileRevision: 1, ProfileDigest: profileDigest,
				Target: appcatalog.Platform{OS: "linux", Arch: "amd64"}, PreviewedAt: now,
				OperatorMachineProfileAssignmentImpact: store.OperatorMachineProfileAssignmentImpact{
					ChangesMachineConfiguration: true, CreatesAssignment: true, CreatesDesiredStates: 1, CreatesJobs: 1,
					DeliveryRequiresJobsEnabled: true, JobsEnabled: boolPtr(true), EverReported: true,
					Packages: []store.OperatorMachineProfileAssignmentPackageImpact{impact}, Blockers: []store.OperatorMachineProfileAssignmentBlocker{},
				},
				PreviewDigest: previewDigest,
			})
		case "/v1/operator/machines/machine-1/profile-assignments":
			n := assignmentWrites.Add(1)
			impact := catalogClientImpact()
			result := operator.MachineProfileAssignmentResult{
				AssignmentID: "assignment-1", AssignmentRevision: 1, MachineID: "machine-1", DisplayName: "samplehub1",
				ProfileID: "standard", ProfileRevision: 1, ProfileDigest: profileDigest,
				Target: appcatalog.Platform{OS: "linux", Arch: "amd64"}, AssignedAt: now,
				Packages: []store.OperatorMachineProfileAssignmentPackageResult{{
					OperatorMachineProfileAssignmentPackageImpact: impact,
					DesiredID: "desired-1", JobID: "job-1", Revision: deploy.Revision(1),
				}}, PreviewDigest: previewDigest,
			}
			if n == 2 {
				result.Replayed = true
				w.Header().Set("Idempotency-Replayed", "true")
				w.WriteHeader(http.StatusOK)
			} else {
				w.WriteHeader(http.StatusCreated)
			}
			_ = json.NewEncoder(w).Encode(result)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := operatorClientForServer(t, server)

	preview, err := client.PreviewStandardCatalogManifest(t.Context(), operator.StandardCatalogManifestPreviewRequest{
		ArtifactSHA256: manifest.Artifact.SHA256,
	})
	if err != nil || preview.ManifestDigest != manifestDigest {
		t.Fatalf("preview=%+v err=%v", preview, err)
	}
	manifestRequest := operator.CatalogManifestPublishRequest{
		Manifest: manifest, ConfirmPackageID: manifest.ID, ConfirmVersion: manifest.Version,
		PreviewDigest: previewDigest, Reason: "publish standard package", IdempotencyKey: "manifest-key",
	}
	for attempt := 0; attempt < 2; attempt++ {
		result, err := client.PublishStandardCatalogManifest(t.Context(), "manifest-key", manifestRequest)
		if err != nil || result.Replayed != (attempt == 1) {
			t.Fatalf("manifest attempt=%d result=%+v err=%v", attempt, result, err)
		}
	}
	listed, err := client.CatalogManifests(t.Context(), operator.CatalogManifestListRequest{
		PackageID: "node-runtime", Kind: appcatalog.KindRuntime, Limit: 1,
	})
	if err != nil || listed.Total != 1 || len(listed.Items) != 1 {
		t.Fatalf("manifest list=%+v err=%v", listed, err)
	}
	profilePreview, err := client.PreviewMachineProfile(t.Context(), operator.MachineProfilePreviewRequest{Profile: profile})
	if err != nil || profilePreview.ProfileDigest != profileDigest {
		t.Fatalf("profile preview=%+v err=%v", profilePreview, err)
	}
	profileRequest := operator.MachineProfilePublishRequest{
		Profile: profile, ConfirmProfileID: profile.ID, ConfirmRevision: profile.Revision,
		PreviewDigest: profilePreview.PreviewDigest, Reason: "publish profile", IdempotencyKey: "profile-key",
	}
	if _, err := client.PublishReviewedMachineProfile(t.Context(), "profile-key", profileRequest); err != nil {
		t.Fatal(err)
	}
	profiles, err := client.MachineProfiles(t.Context(), operator.MachineProfileListRequest{ProfileID: "standard"})
	if err != nil || profiles.Total != 1 || profileWrites.Load() != 1 {
		t.Fatalf("profiles=%+v writes=%d err=%v", profiles, profileWrites.Load(), err)
	}
	assignmentPreview, err := client.PreviewMachineProfileAssignment(t.Context(), operator.MachineProfileAssignmentPreviewRequest{
		MachineID: "machine-1", ProfileID: "standard", ProfileRevision: 1,
	})
	if err != nil || assignmentPreview.CreatesJobs != 1 {
		t.Fatalf("assignment preview=%+v err=%v", assignmentPreview, err)
	}
	assignmentRequest := operator.MachineProfileAssignmentRequest{
		MachineID: "machine-1", ProfileID: "standard", ProfileRevision: 1,
		ConfirmDisplayName: "samplehub1", PreviewDigest: previewDigest, Reason: "assign profile",
		IdempotencyKey: "assignment-key",
	}
	for attempt := 0; attempt < 2; attempt++ {
		result, err := client.AssignMachineProfile(t.Context(), "assignment-key", assignmentRequest)
		if err != nil || result.Replayed != (attempt == 1) || result.Packages[0].JobID != "job-1" {
			t.Fatalf("assignment attempt=%d result=%+v err=%v", attempt, result, err)
		}
	}
}

func boolPtr(value bool) *bool { return &value }

func TestProfileAssignmentClientAcceptsDarwinTargets(t *testing.T) {
	now := time.Date(2026, 9, 20, 5, 30, 0, 0, time.UTC)
	profileDigest := "sha256:" + strings.Repeat("e", 64)
	previewDigest := "sha256:" + strings.Repeat("d", 64)
	previewRequest := operator.MachineProfileAssignmentPreviewRequest{
		MachineID: "machine-mac", ProfileID: "standard", ProfileRevision: 1,
	}
	assignmentRequest := operator.MachineProfileAssignmentRequest{
		MachineID: "machine-mac", ProfileID: "standard", ProfileRevision: 1,
		ConfirmDisplayName: "macbook", PreviewDigest: previewDigest,
	}
	previewFor := func(target appcatalog.Platform) operator.MachineProfileAssignmentPreviewResult {
		return operator.MachineProfileAssignmentPreviewResult{
			MachineID: "machine-mac", DisplayName: "macbook", ProfileID: "standard", ProfileRevision: 1,
			ProfileDigest: profileDigest, Target: target, PreviewedAt: now, PreviewDigest: previewDigest,
			OperatorMachineProfileAssignmentImpact: store.OperatorMachineProfileAssignmentImpact{
				ChangesMachineConfiguration: true, CreatesAssignment: true,
				CreatesDesiredStates: 1, CreatesJobs: 1, DeliveryRequiresJobsEnabled: true,
				JobsEnabled: boolPtr(true), EverReported: true,
				Packages: []store.OperatorMachineProfileAssignmentPackageImpact{catalogClientImpact()},
				Blockers: []store.OperatorMachineProfileAssignmentBlocker{},
			},
		}
	}
	resultFor := func(target appcatalog.Platform) operator.MachineProfileAssignmentResult {
		return operator.MachineProfileAssignmentResult{
			AssignmentID: "assignment-mac", AssignmentRevision: 1,
			MachineID: "machine-mac", DisplayName: "macbook", ProfileID: "standard", ProfileRevision: 1,
			ProfileDigest: profileDigest, Target: target, AssignedAt: now, PreviewDigest: previewDigest,
			Packages: []store.OperatorMachineProfileAssignmentPackageResult{{
				OperatorMachineProfileAssignmentPackageImpact: catalogClientImpact(),
				DesiredID: "desired-mac", JobID: "job-mac", Revision: deploy.Revision(1),
			}},
		}
	}
	for _, arch := range []string{"amd64", "arm64"} {
		t.Run(arch, func(t *testing.T) {
			target := appcatalog.Platform{OS: "darwin", Arch: arch}
			if err := validateProfileAssignmentPreview(previewFor(target), previewRequest); err != nil {
				t.Fatalf("darwin preview rejected: %v", err)
			}
			if err := validateProfileAssignmentResult(resultFor(target), assignmentRequest, false, http.StatusCreated); err != nil {
				t.Fatalf("darwin assignment rejected: %v", err)
			}
		})
	}
	for _, target := range []appcatalog.Platform{{OS: "freebsd", Arch: "amd64"}, {OS: "darwin", Arch: "riscv64"}} {
		if err := validateProfileAssignmentPreview(previewFor(target), previewRequest); err == nil {
			t.Errorf("preview accepted unsupported target %+v", target)
		}
		if err := validateProfileAssignmentResult(resultFor(target), assignmentRequest, false, http.StatusCreated); err == nil {
			t.Errorf("assignment accepted unsupported target %+v", target)
		}
	}
}

// ⚠ 這支實際量到的是未知欄位那一關，不是 digest 那一關：
// CatalogManifestRecord.PublishedBy 的 tag 是 `json:"-"`，所以線上塞
// published_by 會先被 decodeStrictJSONDocument 當未知欄位擋掉，
// 走不到 validateCatalogManifestRecord 的 digest 重算。
// 兩個突變塞在同一個回應，只有先開火的那個被量到。
// digest 那一條由 TestCatalogListRejectsAManifestDigestThatDescribesSomethingElse 守。
func TestCatalogClientRejectsPublishedByAndDigestSubstitution(t *testing.T) {
	now := time.Date(2026, 9, 10, 16, 0, 0, 0, time.UTC)
	manifest := catalogClientManifest()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		catalogClientHeaders(w)
		raw, _ := json.Marshal(operator.CatalogManifestListResult{
			SchemaVersion: operator.CatalogReadSchemaVersion, Consistency: operator.CatalogReadConsistencyLive,
			EvaluatedAt: now, Total: 1, Items: []operator.CatalogManifestRecord{{
				Manifest: manifest, Digest: "sha256:" + strings.Repeat("f", 64), PublishedAt: now,
			}},
		})
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		items := body["items"].([]any)
		items[0].(map[string]any)["published_by"] = "private@example.com"
		_ = json.NewEncoder(w).Encode(body)
	}))
	defer server.Close()
	client := operatorClientForServer(t, server)
	if _, err := client.CatalogManifests(t.Context(), operator.CatalogManifestListRequest{}); err == nil {
		t.Fatal("client accepted actor disclosure and substituted digest")
	}
}

func TestCatalogListRejectsAManifestDigestThatDescribesSomethingElse(t *testing.T) {
	manifest := catalogClientManifest()
	honest := operator.CatalogManifestListResult{
		SchemaVersion: operator.CatalogReadSchemaVersion,
		Consistency:   operator.CatalogReadConsistencyLive,
		EvaluatedAt:   time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC),
		Total:         1,
		Items: []operator.CatalogManifestRecord{{
			Manifest:    manifest,
			Digest:      catalogManifestWireDigest(manifest),
			PublishedAt: time.Date(2026, 9, 18, 11, 0, 0, 0, time.UTC),
		}},
	}
	list := func(result operator.CatalogManifestListResult) error {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			catalogClientHeaders(w)
			_ = json.NewEncoder(w).Encode(result)
		}))
		defer server.Close()
		client := operatorClientForServer(t, server)
		_, err := client.CatalogManifests(t.Context(), operator.CatalogManifestListRequest{})
		return err
	}
	if err := list(honest); err != nil {
		t.Fatalf("honest catalog manifest list was rejected, so the case below would "+
			"not measure the manifest digest validation under test: %v", err)
	}

	substituted := honest
	substituted.Items[0].Digest = "sha256:" + strings.Repeat("f", 64)
	const expected = "operator client: catalog manifest record is invalid"
	if err := list(substituted); err == nil || err.Error() != expected {
		t.Errorf("got error %v, expected %q; an operator would read the manifest on screen and "+
			"pin a deployment to a digest that describes something else, so the fleet installs "+
			"an artifact they never reviewed", err, expected)
	}
}

func TestMachineProfileListRejectsAProfileDigestThatDescribesSomethingElse(t *testing.T) {
	profile := catalogClientProfile()
	honest := operator.MachineProfileListResult{
		SchemaVersion: operator.CatalogReadSchemaVersion,
		Consistency:   operator.CatalogReadConsistencyLive,
		EvaluatedAt:   time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC),
		Total:         1,
		Items: []operator.MachineProfileRecord{{
			Profile:     profile,
			Digest:      machineProfileWireDigest(profile),
			PublishedAt: time.Date(2026, 9, 18, 11, 0, 0, 0, time.UTC),
		}},
	}
	list := func(result operator.MachineProfileListResult) error {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			catalogClientHeaders(w)
			_ = json.NewEncoder(w).Encode(result)
		}))
		defer server.Close()
		client := operatorClientForServer(t, server)
		_, err := client.MachineProfiles(t.Context(), operator.MachineProfileListRequest{})
		return err
	}
	if err := list(honest); err != nil {
		t.Fatalf("honest machine profile list was rejected, so the case below would "+
			"not measure the profile digest validation under test: %v", err)
	}

	substituted := honest
	substituted.Items[0].Digest = "sha256:" + strings.Repeat("f", 64)
	const expected = "operator client: machine profile record is invalid"
	if err := list(substituted); err == nil || err.Error() != expected {
		t.Errorf("got error %v, expected %q; an operator would read the profile on screen and "+
			"pin machines to a digest that describes a different profile, so the fleet converges "+
			"on settings they never reviewed", err, expected)
	}
}

func TestCatalogListRejectsAFirstPageThatAdmitsMoreRowsWithNoCursor(t *testing.T) {
	manifest := catalogClientManifest()
	nextCursor := "cursor-page-2"
	request := operator.CatalogManifestListRequest{Limit: 1}
	honest := operator.CatalogManifestListResult{
		SchemaVersion: operator.CatalogReadSchemaVersion,
		Consistency:   operator.CatalogReadConsistencyLive,
		EvaluatedAt:   time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC),
		Total:         2,
		Items: []operator.CatalogManifestRecord{{
			Manifest:    manifest,
			Digest:      catalogManifestWireDigest(manifest),
			PublishedAt: time.Date(2026, 9, 18, 11, 0, 0, 0, time.UTC),
		}},
		NextCursor: &nextCursor,
	}
	list := func(result operator.CatalogManifestListResult) error {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			catalogClientHeaders(w)
			_ = json.NewEncoder(w).Encode(result)
		}))
		defer server.Close()
		client := operatorClientForServer(t, server)
		_, err := client.CatalogManifests(t.Context(), request)
		return err
	}
	if err := list(honest); err != nil {
		t.Fatalf("honest paginated page was rejected, so the case below would "+
			"not measure the missing-cursor clause under test: %v", err)
	}

	truncated := honest
	truncated.NextCursor = nil
	const expected = "operator client: catalog list pagination evidence is invalid"
	if err := list(truncated); err == nil || err.Error() != expected {
		t.Errorf("got error %v, expected %q; human output omits total and, with a nil "+
			"cursor, omits the cursor line too, so this truncated list looks like the "+
			"complete catalog and an operator may conclude a package is absent and "+
			"publish it again", err, expected)
	}
}
