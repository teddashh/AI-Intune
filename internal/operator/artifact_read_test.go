package operator

import (
	"bytes"
	"context"
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

	"github.com/teddashh/AI-Intune/internal/artifact"
	"github.com/teddashh/AI-Intune/internal/deploy"
	"github.com/teddashh/AI-Intune/internal/store"
)

func writeArtifactReadFixture(t *testing.T, dir, version string, body []byte, tarball bool) artifact.Sidecar {
	t.Helper()
	digestSum := sha256.Sum256(body)
	integritySum := sha512.Sum512(body)
	record := artifact.Sidecar{
		Name: "openclaw", Version: version,
		TarballURL:      "https://registry.example/private/openclaw-" + version + ".tgz?token=RAW_URL_SECRET",
		SHA512Integrity: "sha512-" + base64.StdEncoding.EncodeToString(integritySum[:]),
		SHA256:          hex.EncodeToString(digestSum[:]), Size: int64(len(body)), EnginesNode: ">=24.15.0 <25",
		FetchedAt: time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC), FetchedBy: "RAW_FETCHED_BY_SECRET",
	}
	raw, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, record.SHA256+".json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if tarball {
		if err := os.WriteFile(filepath.Join(dir, record.SHA256+".tgz"), body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return record
}

func TestArtifactListSafeProjectionFiltersAndFilterBoundLiveKeyset(t *testing.T) {
	st := newDeploymentOperatorStore(t)
	dir := t.TempDir()
	first := writeArtifactReadFixture(t, dir, "2026.9.1", []byte("first artifact"), true)
	missing := writeArtifactReadFixture(t, dir, "2026.9.2", []byte("missing artifact"), false)
	malformedDigest := strings.Repeat("f", 64)
	if malformedDigest == first.SHA256 || malformedDigest == missing.SHA256 {
		t.Fatal("fixture digest collision")
	}
	if err := os.WriteFile(filepath.Join(dir, malformedDigest+".json"), []byte(`{"tarball_url":"RAW_MALFORMED_SECRET"`), 0o600); err != nil {
		t.Fatal(err)
	}
	orphanDigest := strings.Repeat("e", 64)
	if err := os.WriteFile(filepath.Join(dir, orphanDigest+".tgz"), []byte("orphan"), 0o600); err != nil {
		t.Fatal(err)
	}
	svc := NewWithArtifacts(st, dir)
	evaluatedAt := time.Date(2026, 9, 8, 12, 30, 0, 0, time.UTC)

	seen := make(map[string]bool)
	cursor := ""
	for {
		page, err := svc.ListArtifacts(ArtifactListRequest{Limit: 1, Cursor: cursor}, evaluatedAt)
		if err != nil {
			t.Fatal(err)
		}
		if page.SchemaVersion != ArtifactReadSchemaVersion || page.Consistency != ArtifactReadConsistencyLive ||
			!page.EvaluatedAt.Equal(evaluatedAt) || page.Total != 4 || len(page.StatusCounts) != 3 || len(page.Items) != 1 {
			t.Fatalf("page=%+v", page)
		}
		item := page.Items[0]
		if seen[item.ArtifactID] {
			t.Fatalf("cursor duplicated artifact %q", item.ArtifactID)
		}
		seen[item.ArtifactID] = true
		if item.Status == ArtifactReady || item.VerifiedAt != nil {
			t.Fatalf("list claimed byte verification: %+v", item)
		}
		raw, err := json.Marshal(item)
		if err != nil {
			t.Fatal(err)
		}
		for _, forbidden := range [][]byte{
			[]byte("tarball_url"), []byte("fetched_by"), []byte("RAW_URL_SECRET"),
			[]byte("RAW_FETCHED_BY_SECRET"), []byte(dir), []byte("RAW_MALFORMED_SECRET"),
		} {
			if bytes.Contains(raw, forbidden) {
				t.Fatalf("safe artifact DTO disclosed %q: %s", forbidden, raw)
			}
		}
		if page.NextCursor == nil {
			break
		}
		cursor = *page.NextCursor
	}
	if len(seen) != 4 {
		t.Fatalf("seen=%v", seen)
	}

	available, err := svc.ListArtifacts(ArtifactListRequest{Status: ArtifactAvailableUnverified}, evaluatedAt)
	if err != nil || available.Total != 1 || len(available.Items) != 1 || available.Items[0].ArtifactID != first.SHA256 {
		t.Fatalf("available=%+v err=%v", available, err)
	}
	unavailable, err := svc.ListArtifacts(ArtifactListRequest{Status: ArtifactUnavailable}, evaluatedAt)
	if err != nil || unavailable.Total != 2 {
		t.Fatalf("unavailable=%+v err=%v", unavailable, err)
	}
	invalid, err := svc.ListArtifacts(ArtifactListRequest{Status: ArtifactInvalid}, evaluatedAt)
	if err != nil || invalid.Total != 1 || invalid.Items[0].ArtifactID != malformedDigest {
		t.Fatalf("invalid=%+v err=%v", invalid, err)
	}
	version, err := svc.ListArtifacts(ArtifactListRequest{Version: missing.Version}, evaluatedAt)
	if err != nil || version.Total != 1 || version.Items[0].ArtifactID != missing.SHA256 {
		t.Fatalf("version=%+v err=%v", version, err)
	}

	firstPage, err := svc.ListArtifacts(ArtifactListRequest{Limit: 1}, evaluatedAt)
	if err != nil || firstPage.NextCursor == nil {
		t.Fatalf("first page=%+v err=%v", firstPage, err)
	}
	for _, request := range []ArtifactListRequest{
		{Limit: 1, Cursor: *firstPage.NextCursor, Status: ArtifactInvalid},
		{Limit: 1, Cursor: *firstPage.NextCursor, Version: first.Version},
	} {
		if _, err := svc.ListArtifacts(request, evaluatedAt); !errors.Is(err, ErrInvalidArtifactRead) {
			t.Fatalf("filter-mismatched cursor request=%+v err=%v", request, err)
		}
	}
}

func TestArtifactDetailVerifiesBytesAndNeverCarriesStaleVerifiedAt(t *testing.T) {
	st := newDeploymentOperatorStore(t)
	dir := t.TempDir()
	body := []byte("detail artifact bytes")
	record := writeArtifactReadFixture(t, dir, "2026.9.8", body, true)
	svc := NewWithArtifacts(st, dir)
	evaluatedAt := time.Now().UTC().Add(-time.Minute)

	detail, err := svc.ArtifactDetail(context.Background(), record.SHA256, evaluatedAt)
	if err != nil || detail.Item.Status != ArtifactReady || detail.Item.VerifiedAt == nil ||
		detail.Item.Issue != nil || detail.Item.SHA256 == nil || *detail.Item.SHA256 != record.SHA256 {
		t.Fatalf("ready detail=%+v err=%v", detail, err)
	}
	if _, offset := detail.Item.VerifiedAt.Zone(); offset != 0 {
		t.Fatalf("verified_at is not UTC: %s", detail.Item.VerifiedAt)
	}
	if !detail.Item.VerifiedAt.Equal(evaluatedAt) {
		t.Fatalf("verified_at=%s evaluated_at=%s", detail.Item.VerifiedAt, evaluatedAt)
	}
	raw, _ := json.Marshal(detail)
	for _, forbidden := range []string{"tarball_url", "fetched_by", "RAW_URL_SECRET", "RAW_FETCHED_BY_SECRET", dir} {
		if bytes.Contains(raw, []byte(forbidden)) {
			t.Fatalf("detail disclosed %q: %s", forbidden, raw)
		}
	}

	if err := os.WriteFile(filepath.Join(dir, record.SHA256+".tgz"), bytes.Repeat([]byte{'z'}, len(body)), 0o600); err != nil {
		t.Fatal(err)
	}
	detail, err = svc.ArtifactDetail(context.Background(), record.SHA256, evaluatedAt)
	if err != nil || detail.Item.Status != ArtifactInvalid || detail.Item.VerifiedAt != nil ||
		detail.Item.Issue == nil || *detail.Item.Issue != ArtifactReadIssue(artifact.CatalogIssueTarballDigestMismatch) {
		t.Fatalf("corrupt detail=%+v err=%v", detail, err)
	}

	if _, err := svc.ArtifactDetail(context.Background(), strings.Repeat("0", 64), evaluatedAt); !errors.Is(err, ErrArtifactNotFound) {
		t.Fatalf("missing detail err=%v", err)
	}
	if _, err := svc.ArtifactDetail(context.Background(), "../private", evaluatedAt); !errors.Is(err, ErrInvalidArtifactRead) {
		t.Fatalf("unsafe detail ID err=%v", err)
	}
}

func TestArtifactDetailPropagatesRequestCancellation(t *testing.T) {
	st := newDeploymentOperatorStore(t)
	dir := t.TempDir()
	record := writeArtifactReadFixture(t, dir, "2026.9.10", []byte("cancelable detail bytes"), true)
	svc := NewWithArtifacts(st, dir)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := svc.ArtifactDetail(ctx, record.SHA256, time.Now().UTC()); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled artifact detail err=%v", err)
	}
	if _, err := svc.ArtifactDetail(nil, record.SHA256, time.Now().UTC()); !errors.Is(err, ErrInvalidArtifactRead) {
		t.Fatalf("nil-context artifact detail err=%v", err)
	}
}

func TestArtifactDeploymentReferenceCountsAreSafeAndExact(t *testing.T) {
	st := newDeploymentOperatorStore(t)
	dir := t.TempDir()
	record := writeArtifactReadFixture(t, dir, deploymentTestVersion, []byte("referenced artifact"), true)
	addDeploymentTestMachine(t, st, "artifact-ref-a", "ref-a", "canary")
	newDeployment := func(machineID string) (store.Deployment, []store.Job) {
		t.Helper()
		spec := deploymentTestSpec(record.SHA256)
		d, jobs, err := st.CreateDeployment(store.NewDeployment{
			Channel: "canary", ResourceKind: "openclaw", ResourceID: "openclaw", Spec: spec,
			BatchSize: 1, CreatedBy: "RAW_DEPLOYMENT_ACTOR", Targets: []store.NewDeploymentTarget{{MachineID: machineID, BatchNo: 1}},
			Job: store.NewJob{ArtifactDigest: "sha256:" + record.SHA256, ExecutionTimeout: 600},
		})
		if err != nil {
			t.Fatal(err)
		}
		return d, jobs
	}
	finished, jobs := newDeployment("artifact-ref-a")
	finishedAt := time.Now().UTC().Truncate(time.Second)
	if _, err := st.DB().Exec(`UPDATE jobs SET state=?,terminal_at=? WHERE job_id=?`,
		deploy.Succeeded, finishedAt.Format(time.RFC3339Nano), jobs[0].JobID); err != nil {
		t.Fatal(err)
	}
	if changed, err := st.SetDeploymentState(finished.DeploymentID, store.DeploymentRunning, store.DeploymentFinished, finishedAt); err != nil || !changed {
		t.Fatalf("finish deployment changed=%t err=%v", changed, err)
	}
	if _, err := st.DB().Exec(`UPDATE machine_registry SET channel=NULL WHERE machine_id=?`, "artifact-ref-a"); err != nil {
		t.Fatal(err)
	}
	addDeploymentTestMachine(t, st, "artifact-ref-b", "ref-b", "canary")
	_, _ = newDeployment("artifact-ref-b")

	svc := NewWithArtifacts(st, dir)
	list, err := svc.ListArtifacts(ArtifactListRequest{}, time.Now().UTC())
	if err != nil || len(list.Items) != 1 {
		t.Fatalf("list=%+v err=%v", list, err)
	}
	item := list.Items[0]
	if item.DeploymentReferences != 2 || item.ActiveDeploymentReferences != 1 {
		t.Fatalf("reference counts=%+v", item)
	}
	raw, _ := json.Marshal(item)
	if bytes.Contains(raw, []byte("RAW_DEPLOYMENT_ACTOR")) {
		t.Fatalf("reference projection disclosed deployment actor: %s", raw)
	}
}

func TestArtifactReadRejectsInvalidRequestsAndUnconfiguredService(t *testing.T) {
	st := newDeploymentOperatorStore(t)
	dir := t.TempDir()
	svc := NewWithArtifacts(st, dir)
	now := time.Now().UTC()
	for _, request := range []ArtifactListRequest{
		{Status: ArtifactReady}, {Status: "unknown"}, {Version: " padded"}, {Limit: -1},
		{Limit: MaxArtifactReadLimit + 1}, {Cursor: "not-base64"},
	} {
		if _, err := svc.ListArtifacts(request, now); !errors.Is(err, ErrInvalidArtifactRead) {
			t.Errorf("request=%+v err=%v", request, err)
		}
	}
	if _, err := svc.ListArtifacts(ArtifactListRequest{}, time.Time{}); !errors.Is(err, ErrInvalidArtifactRead) {
		t.Fatalf("zero evaluated_at err=%v", err)
	}
	if _, err := New(st).ListArtifacts(ArtifactListRequest{}, now); !errors.Is(err, ErrInvalidArtifactRead) {
		t.Fatalf("unconfigured catalog err=%v", err)
	}
	var nilService *Service
	if _, err := nilService.ListArtifacts(ArtifactListRequest{}, now); !errors.Is(err, ErrInvalidArtifactRead) {
		t.Fatalf("nil service err=%v", err)
	}
}
