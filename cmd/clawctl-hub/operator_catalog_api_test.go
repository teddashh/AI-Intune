package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	appcatalog "github.com/teddashh/AI-Intune/internal/catalog"
	"github.com/teddashh/AI-Intune/internal/model"
	"github.com/teddashh/AI-Intune/internal/operator"
)

func writeCatalogAPINodeArtifact(t *testing.T, dir, version string) artifactSidecar {
	t.Helper()
	var bundle bytes.Buffer
	gz := gzip.NewWriter(&bundle)
	tw := tar.NewWriter(gz)
	for _, target := range []string{"linux-amd64", "linux-arm64", "darwin-amd64", "darwin-arm64",
		"windows-amd64", "windows-arm64"} {
		nodeName := "node-runtime/" + target + "/bin/node"
		if strings.HasPrefix(target, "windows-") {
			nodeName += ".exe"
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
	sha := sha256.Sum256(body)
	integrity := sha512.Sum512(body)
	record := artifactSidecar{
		Name: "node-runtime", Version: version,
		TarballURL: "https://nodejs.org/dist/v" + version + "/", SHA256: hex.EncodeToString(sha[:]),
		SHA512Integrity: "sha512-" + base64.StdEncoding.EncodeToString(integrity[:]),
		Size:            int64(len(body)), FetchedAt: jobsTestNow, FetchedBy: "RAW_FETCHED_BY_SENTINEL",
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, record.SHA256+".tgz"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeArtifactSidecar(dir, record); err != nil {
		t.Fatal(err)
	}
	return record
}

func operatorCatalogTestManifest(record artifactSidecar) appcatalog.Manifest {
	return appcatalog.Manifest{
		SchemaVersion: appcatalog.SchemaVersion, ID: "openclaw", Version: record.Version,
		Kind: appcatalog.KindApp, Title: "OpenClaw",
		Source: appcatalog.Source{
			Catalog: "ai-intune", UpstreamURL: "https://www.npmjs.com/package/openclaw/v/" + record.Version,
			Revision: record.Version, License: "MIT",
		},
		Artifact: recordArtifact(record), Adapter: appcatalog.Adapter{Name: "openclaw", Version: 1},
		Platforms:    []appcatalog.Platform{{OS: "linux", Arch: "amd64"}, {OS: "linux", Arch: "arm64"}},
		Dependencies: []appcatalog.PackageRef{}, Provides: []string{appcatalog.CapabilityAgentRuntime},
		Conflicts: []string{"hermes-agent"}, ExclusiveGroups: []string{appcatalog.ExclusiveGroupPrimaryAgentRuntime},
	}
}

func recordArtifact(record artifactSidecar) appcatalog.Artifact {
	return appcatalog.Artifact{SHA256: record.SHA256, Size: record.Size}
}

func TestOperatorCatalogProfileAssignmentVerticalSlice(t *testing.T) {
	f := newJobsFixture(t, "catalog-target")
	(&hub{store: f.store, artifactsDir: f.artifactsDir}).operatorRoutes(f.mux)
	record := writeJobTestArtifact(t, f.artifactsDir, "2026.9.2", "catalog-api")
	nodeRecord := writeCatalogAPINodeArtifact(t, f.artifactsDir, "24.21.0")
	nodePreview := operatorRequest(t, f.mux, http.MethodPost,
		"/v1/operator/catalog-manifests/standard-preview", "",
		`{"artifact_sha256":"`+nodeRecord.SHA256+`","node_runtime_version":""}`)
	var nodePreviewResult operator.StandardCatalogManifestPreviewResult
	if nodePreview.Code != http.StatusOK || json.Unmarshal(nodePreview.Body.Bytes(), &nodePreviewResult) != nil ||
		nodePreviewResult.Manifest.ID != "node-runtime" || nodePreviewResult.PreviewDigest == "" {
		t.Fatalf("node preview status=%d result=%+v body=%s", nodePreview.Code, nodePreviewResult, nodePreview.Body.String())
	}
	nodePublishBody, _ := json.Marshal(map[string]any{
		"manifest": nodePreviewResult.Manifest, "confirm_package_id": "node-runtime",
		"confirm_version": nodeRecord.Version, "preview_digest": nodePreviewResult.PreviewDigest,
		"reason": "add official runtime",
	})
	nodePublished := operatorRequest(t, f.mux, http.MethodPost, "/v1/operator/catalog-manifests",
		"catalog-api-node", string(nodePublishBody))
	if nodePublished.Code != http.StatusCreated || strings.Contains(nodePublished.Body.String(), "RAW_FETCHED_BY_SENTINEL") {
		t.Fatalf("node publish status=%d body=%s", nodePublished.Code, nodePublished.Body.String())
	}
	manifest := operatorCatalogTestManifest(record)
	manifestBody, _ := json.Marshal(map[string]any{"manifest": manifest, "reason": "add exact package"})

	published := operatorRequest(t, f.mux, http.MethodPost, "/v1/operator/catalog-manifests",
		"catalog-api-manifest", string(manifestBody))
	if published.Code != http.StatusCreated || published.Header().Get("Cache-Control") != "no-store" ||
		strings.Contains(published.Body.String(), "fixture-fetch-principal") ||
		strings.Contains(published.Body.String(), "ted@example.com") || strings.Contains(published.Body.String(), "published_by") {
		t.Fatalf("manifest status=%d headers=%v body=%s", published.Code, published.Header(), published.Body.String())
	}
	var publishedManifest operator.CatalogManifestPublishResult
	if err := json.Unmarshal(published.Body.Bytes(), &publishedManifest); err != nil ||
		publishedManifest.Record.Manifest.ID != "openclaw" || publishedManifest.Record.Digest == "" {
		t.Fatalf("published manifest=%+v err=%v", publishedManifest, err)
	}

	listed := operatorRequest(t, f.mux, http.MethodGet,
		"/v1/operator/catalog-manifests?package_id=openclaw&kind=app&limit=1", "", "")
	var manifestList operator.CatalogManifestListResult
	if listed.Code != http.StatusOK || json.Unmarshal(listed.Body.Bytes(), &manifestList) != nil ||
		manifestList.Total != 1 || len(manifestList.Items) != 1 || manifestList.Items[0].Manifest.Version != record.Version {
		t.Fatalf("manifest list status=%d result=%+v body=%s", listed.Code, manifestList, listed.Body.String())
	}

	profile := appcatalog.MachineProfile{
		SchemaVersion: appcatalog.SchemaVersion, ID: "openclaw-standard", Revision: 1,
		Packages: []appcatalog.PackageRef{{PackageID: "openclaw", Version: record.Version}},
	}
	profilePreviewBody, _ := json.Marshal(map[string]any{"profile": profile})
	profilePreview := operatorRequest(t, f.mux, http.MethodPost, "/v1/operator/machine-profiles/preview", "", string(profilePreviewBody))
	var profilePreviewResult operator.MachineProfilePreviewResult
	if profilePreview.Code != http.StatusOK || json.Unmarshal(profilePreview.Body.Bytes(), &profilePreviewResult) != nil ||
		profilePreviewResult.ProfileDigest == "" || profilePreviewResult.PreviewDigest == "" {
		t.Fatalf("profile preview status=%d result=%+v body=%s", profilePreview.Code, profilePreviewResult, profilePreview.Body.String())
	}
	profileBody, _ := json.Marshal(map[string]any{
		"profile": profile, "confirm_profile_id": profile.ID, "confirm_revision": profile.Revision,
		"preview_digest": profilePreviewResult.PreviewDigest, "reason": "publish managed profile",
	})
	publishedProfile := operatorRequest(t, f.mux, http.MethodPost, "/v1/operator/machine-profiles",
		"catalog-api-profile", string(profileBody))
	if publishedProfile.Code != http.StatusCreated || strings.Contains(publishedProfile.Body.String(), "published_by") {
		t.Fatalf("profile status=%d body=%s", publishedProfile.Code, publishedProfile.Body.String())
	}
	profiles := operatorRequest(t, f.mux, http.MethodGet,
		"/v1/operator/machine-profiles?profile_id=openclaw-standard&limit=1", "", "")
	var profileList operator.MachineProfileListResult
	if profiles.Code != http.StatusOK || json.Unmarshal(profiles.Body.Bytes(), &profileList) != nil ||
		profileList.Total != 1 || len(profileList.Items) != 1 || profileList.Items[0].Profile.Revision != 1 {
		t.Fatalf("profile list status=%d result=%+v body=%s", profiles.Code, profileList, profiles.Body.String())
	}

	jobsEnabled := true
	if err := f.store.RecordCheckin(f.machine.id, model.Checkin{
		SchemaVersion: model.SchemaVersion, SentAt: jobsTestNow,
		AgentVersion: "test", BootID: "boot-1", AgentSeq: 1,
		AgentStartedAt: jobsTestNow.Add(-time.Hour), JobsEnabled: &jobsEnabled,
	}, jobsTestNow); err != nil {
		t.Fatal(err)
	}
	previewBody := `{"profile_id":"openclaw-standard","profile_revision":1}`
	preview := operatorRequest(t, f.mux, http.MethodPost,
		"/v1/operator/machines/"+f.machine.id+"/profile-assignment-preview", "", previewBody)
	var previewResult operator.MachineProfileAssignmentPreviewResult
	if preview.Code != http.StatusOK || json.Unmarshal(preview.Body.Bytes(), &previewResult) != nil ||
		previewResult.MachineID != f.machine.id || len(previewResult.Packages) != 1 ||
		previewResult.CreatesJobs != 1 || len(previewResult.Blockers) != 0 {
		t.Fatalf("preview status=%d result=%+v body=%s", preview.Code, previewResult, preview.Body.String())
	}
	applyBody, _ := json.Marshal(map[string]any{
		"profile_id": "openclaw-standard", "profile_revision": int64(1),
		"confirm_display_name": "catalog-target", "preview_digest": previewResult.PreviewDigest,
		"reason": "assign managed profile",
	})
	assigned := operatorRequest(t, f.mux, http.MethodPost,
		"/v1/operator/machines/"+f.machine.id+"/profile-assignments", "catalog-api-assignment", string(applyBody))
	var assignment operator.MachineProfileAssignmentResult
	if assigned.Code != http.StatusCreated || json.Unmarshal(assigned.Body.Bytes(), &assignment) != nil ||
		assignment.MachineID != f.machine.id || len(assignment.Packages) != 1 || assignment.Packages[0].JobID == "" ||
		strings.Contains(assigned.Body.String(), "assigned_by") || strings.Contains(assigned.Body.String(), "spec\"") {
		t.Fatalf("assignment status=%d result=%+v body=%s", assigned.Code, assignment, assigned.Body.String())
	}

	replay := operatorRequest(t, f.mux, http.MethodPost,
		"/v1/operator/machines/"+f.machine.id+"/profile-assignments", "catalog-api-assignment", string(applyBody))
	if replay.Code != http.StatusOK || replay.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatalf("replay status=%d headers=%v body=%s", replay.Code, replay.Header(), replay.Body.String())
	}
}

func TestOperatorCatalogMutationTransportIsStrictAndDoesNotConsumeKeys(t *testing.T) {
	f := newJobsFixture(t, "catalog-transport")
	(&hub{store: f.store, artifactsDir: f.artifactsDir}).operatorRoutes(f.mux)
	for _, test := range []struct {
		path string
		body string
	}{
		{"/v1/operator/catalog-manifests", `{"manifest":null,"reason":"x"}`},
		{"/v1/operator/machine-profiles", `{"profile":null,"reason":"x"}`},
		{"/v1/operator/machines/" + f.machine.id + "/profile-assignments", `{"profile_id":"x","profile_revision":1}`},
	} {
		rec := operatorRequest(t, f.mux, http.MethodPost, test.path, "catalog-transport", test.body)
		assertAPIError(t, rec, http.StatusBadRequest, "BAD_REQUEST")
	}
	var count int
	if err := f.store.DB().QueryRow(`SELECT COUNT(*) FROM operator_idempotency WHERE idempotency_key='catalog-transport'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("transport receipts=%d err=%v", count, err)
	}
}

func TestOperatorCatalogListRejectsAmbiguousQueries(t *testing.T) {
	for _, path := range []string{
		"/v1/operator/catalog-manifests?", "/v1/operator/catalog-manifests?kind=app&kind=runtime",
		"/v1/operator/catalog-manifests?unknown=x", "/v1/operator/catalog-manifests?limit=101",
		"/v1/operator/machine-profiles?profile_id=", "/v1/operator/machine-profiles?cursor=%20x",
	} {
		req := httptestRequest(http.MethodGet, path)
		if strings.Contains(path, "catalog-manifests") {
			if _, err := parseOperatorCatalogManifestListRequest(req); err == nil {
				t.Fatalf("query %q accepted", path)
			}
		} else if _, err := parseOperatorMachineProfileListRequest(req); err == nil {
			t.Fatalf("query %q accepted", path)
		}
	}
}
