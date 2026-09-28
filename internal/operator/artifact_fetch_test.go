package operator

import (
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
	"sync/atomic"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/artifact"
	"github.com/teddashh/AI-Intune/internal/store"
)

type fakeArtifactFetchBackend struct {
	plan  artifact.PreviewPlan
	err   error
	calls atomic.Int32
}

func (f *fakeArtifactFetchBackend) PreviewPlan(context.Context, string, string) (artifact.PreviewPlan, error) {
	f.calls.Add(1)
	return f.plan, f.err
}

func (*fakeArtifactFetchBackend) FetchExact(context.Context, artifact.PreviewPlan, string,
	artifact.ProgressFunc,
) (artifact.Sidecar, bool, error) {
	return artifact.Sidecar{}, false, errors.New("unexpected fetch")
}

func (*fakeArtifactFetchBackend) ReconcileStaleTemps(time.Time) (int, error) { return 0, nil }

func artifactFetchTestPlan() artifact.PreviewPlan {
	return artifact.PreviewPlan{
		PolicyVersion: artifact.FetchPolicyVersion, SourceKind: artifact.ArtifactSourceNPM,
		Name: "openclaw", Version: "2026.9.8",
		RegistryOrigin:  artifact.ProductionRegistryOrigin,
		TarballURL:      artifact.ProductionRegistryOrigin + "/openclaw/-/openclaw-2026.9.8.tgz",
		SHA512Integrity: "sha512-" + base64.StdEncoding.EncodeToString(make([]byte, sha512.Size)),
		EnginesNode:     ">=24.15.0 <25", MaxBytes: artifact.DefaultArtifactMaxBytes,
		PreviewedAt:   time.Date(2026, 9, 8, 14, 0, 0, 0, time.UTC),
		PreviewDigest: "sha256:" + strings.Repeat("a", 64),
	}
}

func artifactFetchTestService(t *testing.T, backend artifactFetchBackend) (*Service, *store.Store, string) {
	t.Helper()
	st := newDeploymentOperatorStore(t)
	dir := filepath.Join(t.TempDir(), "artifacts")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return &Service{store: st, artifactsDir: dir, artifactFetcher: backend}, st, dir
}

func TestArtifactFetchPreviewIsSafeAndRecognizesVerifiedCache(t *testing.T) {
	plan := artifactFetchTestPlan()
	body := []byte("already cached artifact")
	sha256Sum, sha512Sum := sha256.Sum256(body), sha512.Sum512(body)
	plan.SHA512Integrity = "sha512-" + base64.StdEncoding.EncodeToString(sha512Sum[:])
	backend := &fakeArtifactFetchBackend{plan: plan}
	service, _, dir := artifactFetchTestService(t, backend)
	digest := hex.EncodeToString(sha256Sum[:])
	record := artifact.Sidecar{
		Name: plan.Name, Version: plan.Version, TarballURL: plan.TarballURL,
		SHA512Integrity: plan.SHA512Integrity, SHA256: digest, Size: int64(len(body)),
		EnginesNode: plan.EnginesNode, FetchedAt: plan.PreviewedAt.Add(-time.Hour), FetchedBy: "private-actor",
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

	result, err := service.PreviewArtifactFetch(t.Context(), ArtifactFetchPreviewRequest{Name: plan.Name, Version: plan.Version})
	if err != nil || !result.EnqueueAllowed || result.Blockers == nil ||
		!result.AlreadyAvailableAndVerified || result.ExistingArtifactSHA256 == nil ||
		*result.ExistingArtifactSHA256 != digest || result.EnginesNode == nil || *result.EnginesNode != plan.EnginesNode {
		t.Fatalf("preview=%+v err=%v", result, err)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{plan.TarballURL, record.FetchedBy, dir, "tarball_url", "fetched_by"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("safe preview disclosed %q: %s", forbidden, encoded)
		}
	}
}

func TestArtifactFetchApplyQueuesAndReplaysWithoutRegistryRecall(t *testing.T) {
	backend := &fakeArtifactFetchBackend{plan: artifactFetchTestPlan()}
	service, _, _ := artifactFetchTestService(t, backend)
	request := ArtifactFetchApplyRequest{
		Name: "openclaw", Version: "2026.9.8", PreviewDigest: backend.plan.PreviewDigest,
		ConfirmName: "openclaw", ConfirmVersion: "2026.9.8", Reason: "scheduled artifact intake",
		IdempotencyKey: "artifact-fetch-apply-test", Actor: Actor{SourceAddr: "100.64.0.1", SourceKind: SourceKindOperatorAPI},
	}
	result, err := service.ApplyArtifactFetch(t.Context(), request)
	if err != nil || result.Replayed || result.Operation.State != store.ArtifactFetchQueued ||
		result.Operation.OperationID == "" || result.Operation.PreviewDigest != request.PreviewDigest {
		t.Fatalf("fresh=%+v err=%v", result, err)
	}
	backend.err = errors.New("registry must not be called for replay")
	replay, err := service.ApplyArtifactFetch(t.Context(), request)
	if err != nil || !replay.Replayed || replay.Operation.OperationID != result.Operation.OperationID || backend.calls.Load() != 1 {
		t.Fatalf("replay=%+v calls=%d err=%v", replay, backend.calls.Load(), err)
	}
}

func TestArtifactFetchApplyTransientPrepareFailureDoesNotConsumeKey(t *testing.T) {
	backend := &fakeArtifactFetchBackend{plan: artifactFetchTestPlan(), err: errors.New("temporary registry outage")}
	service, st, _ := artifactFetchTestService(t, backend)
	request := ArtifactFetchApplyRequest{
		Name: "openclaw", Version: "2026.9.8", PreviewDigest: backend.plan.PreviewDigest,
		ConfirmName: "openclaw", ConfirmVersion: "2026.9.8", Reason: "retryable intake",
		IdempotencyKey: "artifact-fetch-transient",
	}
	if _, err := service.ApplyArtifactFetch(t.Context(), request); !errors.Is(err, store.ErrArtifactFetchPrepareFailed) {
		t.Fatalf("transient err=%v", err)
	}
	var receipts int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM operator_idempotency WHERE idempotency_key=?`, request.IdempotencyKey).Scan(&receipts); err != nil || receipts != 0 {
		t.Fatalf("transient receipts=%d err=%v", receipts, err)
	}
	backend.err = nil
	result, err := service.ApplyArtifactFetch(t.Context(), request)
	if err != nil || result.Operation.State != store.ArtifactFetchQueued {
		t.Fatalf("retry=%+v err=%v", result, err)
	}
}

func TestArtifactFetchApplyRejectsMismatchedConfirmationBeforeRegistryIO(t *testing.T) {
	backend := &fakeArtifactFetchBackend{plan: artifactFetchTestPlan()}
	service, st, _ := artifactFetchTestService(t, backend)
	request := ArtifactFetchApplyRequest{
		Name: "openclaw", Version: "2026.9.8", PreviewDigest: backend.plan.PreviewDigest,
		ConfirmName: "openclaw", ConfirmVersion: "2026.9.9", Reason: "typed confirmation mismatch",
		IdempotencyKey: "artifact-fetch-confirm-mismatch",
	}
	for attempt := 0; attempt < 2; attempt++ {
		_, err := service.ApplyArtifactFetch(t.Context(), request)
		var rejection *store.OperatorRequestError
		if !errors.As(err, &rejection) || !errors.Is(err, store.ErrArtifactFetchInvalid) ||
			rejection.Replayed != (attempt == 1) {
			t.Fatalf("attempt=%d rejection=%+v err=%v", attempt, rejection, err)
		}
	}
	if backend.calls.Load() != 0 {
		t.Fatalf("mismatched confirmation reached registry %d times", backend.calls.Load())
	}
	var receipts, operations, audits int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM operator_idempotency WHERE idempotency_key=?`,
		request.IdempotencyKey).Scan(&receipts); err != nil {
		t.Fatal(err)
	}
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM artifact_fetch_operations`).Scan(&operations); err != nil {
		t.Fatal(err)
	}
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM audit_log WHERE idempotency_key=?`,
		request.IdempotencyKey).Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if receipts != 1 || operations != 0 || audits != 2 {
		t.Fatalf("receipts=%d operations=%d audits=%d", receipts, operations, audits)
	}
}

func TestArtifactFetchApplyPersistsStalePreviewDecision(t *testing.T) {
	backend := &fakeArtifactFetchBackend{plan: artifactFetchTestPlan()}
	service, _, _ := artifactFetchTestService(t, backend)
	request := ArtifactFetchApplyRequest{
		Name: "openclaw", Version: "2026.9.8", PreviewDigest: "sha256:" + strings.Repeat("b", 64),
		ConfirmName: "openclaw", ConfirmVersion: "2026.9.8", Reason: "stale preview",
		IdempotencyKey: "artifact-fetch-stale",
	}
	if _, err := service.ApplyArtifactFetch(t.Context(), request); !errors.Is(err, store.ErrArtifactFetchPreviewStale) {
		t.Fatalf("stale err=%v", err)
	}
	backend.err = errors.New("replay must bypass registry")
	_, err := service.ApplyArtifactFetch(t.Context(), request)
	var rejection *store.OperatorRequestError
	if !errors.As(err, &rejection) || !rejection.Replayed || !errors.Is(err, store.ErrArtifactFetchPreviewStale) || backend.calls.Load() != 1 {
		t.Fatalf("replayed stale err=%T %v rejection=%+v calls=%d", err, err, rejection, backend.calls.Load())
	}
}
