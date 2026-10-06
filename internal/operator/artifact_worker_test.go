package operator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/artifact"
	"github.com/teddashh/AI-Intune/internal/store"
)

type artifactWorkerBackend struct {
	mu        sync.Mutex
	plans     map[string]artifact.PreviewPlan
	fetch     func(context.Context, artifact.PreviewPlan, string, artifact.ProgressFunc) (artifact.Sidecar, bool, error)
	reconcile func(time.Time) (int, error)
}

func (b *artifactWorkerBackend) PreviewPlan(_ context.Context, name, version string) (artifact.PreviewPlan, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	plan, ok := b.plans[version]
	if !ok || plan.Name != name {
		return artifact.PreviewPlan{}, errors.New("missing test plan")
	}
	return plan, nil
}

func (b *artifactWorkerBackend) FetchExact(ctx context.Context, plan artifact.PreviewPlan, fetchedBy string,
	progress artifact.ProgressFunc,
) (artifact.Sidecar, bool, error) {
	if b.fetch == nil {
		return artifact.Sidecar{}, false, errors.New("missing test fetch")
	}
	return b.fetch(ctx, plan, fetchedBy, progress)
}

func (b *artifactWorkerBackend) ReconcileStaleTemps(before time.Time) (int, error) {
	if b.reconcile == nil {
		return 0, nil
	}
	return b.reconcile(before)
}

func artifactWorkerPlan(version string) artifact.PreviewPlan {
	plan := artifactFetchTestPlan()
	plan.Version = version
	plan.TarballURL = artifact.ProductionRegistryOrigin + "/openclaw/-/openclaw-" + version + ".tgz"
	digest := sha256.Sum256([]byte("preview:" + version))
	plan.PreviewDigest = "sha256:" + hex.EncodeToString(digest[:])
	return plan
}

// enqueueStoredArtifactWorkerOperation stores a prepared fetch without the operator plan check.
func enqueueStoredArtifactWorkerOperation(t *testing.T, st *store.Store, plan artifact.PreviewPlan,
	key string,
) store.ArtifactFetchOperation {
	t.Helper()
	digest := sha256.Sum256([]byte("worker-request:" + key))
	created, err := st.ApplyOperatorArtifactFetch(store.OperatorArtifactFetchRequest{
		Name: plan.Name, Version: plan.Version, PreviewDigest: plan.PreviewDigest,
		IdempotencyKey: key, RequestDigest: "sha256:" + hex.EncodeToString(digest[:]),
		Reason: "artifact worker test",
		Audit:  store.AuditEntry{SourceAddr: "100.64.0.1:1234", AuthSubject: "operator-test"},
	}, func() (store.ArtifactFetchPrepared, error) {
		return store.ArtifactFetchPrepared{
			Name: plan.Name, Version: plan.Version, SourceKind: plan.SourceKind, SourcePlan: plan.SourcePlan,
			RegistryOrigin: plan.RegistryOrigin, TarballURL: plan.TarballURL,
			SHA512Integrity: plan.SHA512Integrity, EnginesNode: plan.EnginesNode,
			MaxBytes: plan.MaxBytes, CurrentPreviewDigest: plan.PreviewDigest,
		}, nil
	})
	if err != nil {
		t.Fatalf("enqueue artifact fetch: %v", err)
	}
	return created.Operation
}

func enqueueArtifactWorkerOperation(t *testing.T, service *Service, plan artifact.PreviewPlan,
	key string,
) store.ArtifactFetchOperation {
	t.Helper()
	result, err := service.ApplyArtifactFetch(t.Context(), ArtifactFetchApplyRequest{
		Name: plan.Name, Version: plan.Version, PreviewDigest: plan.PreviewDigest,
		ConfirmName: plan.Name, ConfirmVersion: plan.Version,
		Reason: "artifact worker test", IdempotencyKey: key,
	})
	if err != nil {
		t.Fatalf("enqueue artifact fetch: %v", err)
	}
	return result.Operation
}

func artifactWorkerRecord(plan artifact.PreviewPlan, size int64) artifact.Sidecar {
	digest := sha256.Sum256([]byte(plan.Version))
	return artifact.Sidecar{
		Name: plan.Name, Version: plan.Version, TarballURL: plan.TarballURL,
		SHA512Integrity: plan.SHA512Integrity, SHA256: hex.EncodeToString(digest[:]),
		Size: size, EnginesNode: plan.EnginesNode,
		FetchedAt: time.Date(2026, 9, 8, 20, 0, 0, 0, time.UTC), FetchedBy: "cached-or-new",
	}
}

func reportArtifactWorkerPhases(progress artifact.ProgressFunc, size int64) error {
	for _, update := range []artifact.FetchProgress{
		{Phase: artifact.FetchPhaseDownloading, DownloadedBytes: 0},
		{Phase: artifact.FetchPhaseDownloading, DownloadedBytes: size},
		{Phase: artifact.FetchPhaseVerifying, DownloadedBytes: size},
		{Phase: artifact.FetchPhasePublishing, DownloadedBytes: size},
	} {
		if err := progress(update); err != nil {
			return err
		}
	}
	return nil
}

func TestRunArtifactFetchOperationDownloadAndCacheReachComplete(t *testing.T) {
	for _, cached := range []bool{false, true} {
		cached := cached
		t.Run(fmt.Sprintf("cached=%t", cached), func(t *testing.T) {
			plan := artifactWorkerPlan("2026.9.8")
			backend := &artifactWorkerBackend{plans: map[string]artifact.PreviewPlan{plan.Version: plan}}
			service, st, _ := artifactFetchTestService(t, backend)
			queued := enqueueArtifactWorkerOperation(t, service, plan, fmt.Sprintf("worker-cache-%t", cached))
			var gotPlan artifact.PreviewPlan
			var gotFetchedBy string
			var observed []store.ArtifactFetchOperation
			backend.fetch = func(_ context.Context, workerPlan artifact.PreviewPlan, fetchedBy string,
				progress artifact.ProgressFunc,
			) (artifact.Sidecar, bool, error) {
				gotPlan, gotFetchedBy = workerPlan, fetchedBy
				for _, update := range []artifact.FetchProgress{
					{Phase: artifact.FetchPhaseDownloading, DownloadedBytes: 0},
					{Phase: artifact.FetchPhaseDownloading, DownloadedBytes: 42},
					{Phase: artifact.FetchPhaseVerifying, DownloadedBytes: 42},
					{Phase: artifact.FetchPhasePublishing, DownloadedBytes: 42},
				} {
					if err := progress(update); err != nil {
						return artifact.Sidecar{}, cached, err
					}
					snapshot, err := st.GetArtifactFetchOperation(queued.OperationID)
					if err != nil {
						return artifact.Sidecar{}, cached, err
					}
					observed = append(observed, snapshot)
				}
				return artifactWorkerRecord(workerPlan, 42), cached, nil
			}

			completed, err := service.RunArtifactFetchOperation(t.Context(), queued.OperationID, false)
			if err != nil {
				t.Fatalf("RunArtifactFetchOperation: %v", err)
			}
			if completed.State != store.ArtifactFetchSucceeded || completed.Phase != store.ArtifactFetchPhaseComplete ||
				completed.ProgressBytes != 42 || completed.ResultSHA256 == nil || completed.ResultSizeBytes == nil ||
				*completed.ResultSizeBytes != 42 {
				t.Fatalf("completed operation: %+v", completed)
			}
			if gotPlan.PolicyVersion != artifact.FetchPolicyVersion || gotPlan.Name != queued.Name ||
				gotPlan.Version != queued.Version || gotPlan.RegistryOrigin != queued.RegistryOrigin ||
				gotPlan.TarballURL != plan.TarballURL || gotPlan.SHA512Integrity != queued.SHA512Integrity ||
				gotPlan.EnginesNode != queued.EnginesNode || gotPlan.MaxBytes != queued.MaxBytes ||
				gotPlan.PreviewDigest != queued.PreviewDigest || !gotPlan.PreviewedAt.Equal(queued.CreatedAt) {
				t.Fatalf("reconstructed plan: %+v", gotPlan)
			}
			if gotFetchedBy != artifactFetchWorkerIdentityPrefix+queued.OperationID {
				t.Fatalf("fetchedBy = %q", gotFetchedBy)
			}
			wantPhases := []store.ArtifactFetchPhase{
				store.ArtifactFetchPhaseDownloading, store.ArtifactFetchPhaseDownloading,
				store.ArtifactFetchPhaseVerifying, store.ArtifactFetchPhasePublishing,
			}
			for index, want := range wantPhases {
				if observed[index].Phase != want {
					t.Fatalf("observed phases = %+v", observed)
				}
			}
		})
	}
}

func TestRunArtifactFetchOperationPreservesNodeRuntimeSourcePlan(t *testing.T) {
	previewDigest := sha256.Sum256([]byte("node-runtime-preview"))
	previewDigestText := "sha256:" + hex.EncodeToString(previewDigest[:])
	checksumURL := artifact.ProductionNodeDistributionOrigin + "/dist/v24.21.0/SHASUMS256.txt"
	sourcePlan, err := json.Marshal(artifact.NodeRuntimeFetchPlan{
		PolicyVersion: artifact.NodeRuntimeFetchPolicyVersion,
		Name:          "node-runtime",
		Version:       "24.21.0",
		SourceOrigin:  artifact.ProductionNodeDistributionOrigin,
		ChecksumURL:   checksumURL,
		Sources: []artifact.NodeRuntimeSource{
			{TargetOS: "linux", TargetArch: "amd64", Filename: "node-v24.21.0-linux-x64.tar.gz", SHA256: strings.Repeat("a", 64)},
			{TargetOS: "linux", TargetArch: "arm64", Filename: "node-v24.21.0-linux-arm64.tar.gz", SHA256: strings.Repeat("b", 64)},
		},
		SourceIdentity: artifactFetchTestPlan().SHA512Integrity,
		SourceMaxBytes: 1 << 20,
		BundleMaxBytes: artifact.DefaultArtifactMaxBytes,
		PreviewedAt:    time.Date(2026, 9, 8, 20, 0, 0, 0, time.UTC),
		PreviewDigest:  previewDigestText,
	})
	if err != nil {
		t.Fatal(err)
	}
	plan := artifact.PreviewPlan{
		PolicyVersion:   artifact.NodeRuntimeFetchPolicyVersion,
		SourceKind:      artifact.ArtifactSourceNode,
		Name:            "node-runtime",
		Version:         "24.21.0",
		RegistryOrigin:  artifact.ProductionNodeDistributionOrigin,
		TarballURL:      checksumURL,
		SHA512Integrity: artifactFetchTestPlan().SHA512Integrity,
		MaxBytes:        artifact.DefaultArtifactMaxBytes,
		PreviewedAt:     time.Date(2026, 9, 8, 20, 0, 0, 0, time.UTC),
		PreviewDigest:   previewDigestText,
		SourcePlan:      string(sourcePlan),
	}
	backend := &artifactWorkerBackend{plans: map[string]artifact.PreviewPlan{plan.Version: plan}}
	service, _, _ := artifactFetchTestService(t, backend)
	queued := enqueueArtifactWorkerOperation(t, service, plan, "worker-node-runtime")
	if queued.SourceKind != artifact.ArtifactSourceNode || queued.EnginesNode != "" {
		t.Fatalf("queued node operation=%+v", queued)
	}
	backend.fetch = func(_ context.Context, workerPlan artifact.PreviewPlan, _ string,
		progress artifact.ProgressFunc,
	) (artifact.Sidecar, bool, error) {
		if workerPlan.SourceKind != artifact.ArtifactSourceNode ||
			workerPlan.PolicyVersion != artifact.NodeRuntimeFetchPolicyVersion ||
			workerPlan.SourcePlan != plan.SourcePlan || workerPlan.TarballURL != plan.TarballURL {
			return artifact.Sidecar{}, false, fmt.Errorf("reconstructed node plan=%+v", workerPlan)
		}
		if err := reportArtifactWorkerPhases(progress, 84); err != nil {
			return artifact.Sidecar{}, false, err
		}
		return artifactWorkerRecord(workerPlan, 84), false, nil
	}
	completed, err := service.RunArtifactFetchOperation(t.Context(), queued.OperationID, false)
	if err != nil {
		t.Fatal(err)
	}
	if completed.State != store.ArtifactFetchSucceeded || completed.SourceKind != artifact.ArtifactSourceNode ||
		completed.ProgressBytes != 84 || completed.ResultSizeBytes == nil || *completed.ResultSizeBytes != 84 {
		t.Fatalf("completed node operation=%+v", completed)
	}
	raw, err := json.Marshal(completed)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "checksum_url") || strings.Contains(string(raw), "source_plan") {
		t.Fatalf("public node operation exposed source plan: %s", raw)
	}
}

func TestRunArtifactFetchOperationPreservesClaudeCodeSourcePlan(t *testing.T) {
	previewDigest := sha256.Sum256([]byte("claude-code-preview"))
	previewDigestText := "sha256:" + hex.EncodeToString(previewDigest[:])
	const version = "2.1.50"
	checksumURL := artifact.ProductionClaudeCodeOrigin + "/claude-code-releases/" + version + "/manifest.json"
	sourcePlan, err := json.Marshal(artifact.ClaudeCodeFetchPlan{
		PolicyVersion: artifact.ClaudeCodeFetchPolicyVersion,
		Name:          "claude-code",
		Version:       version,
		SourceOrigin:  artifact.ProductionClaudeCodeOrigin,
		ChecksumURL:   checksumURL,
		Sources: []artifact.ClaudeCodeSource{
			{TargetOS: "linux", TargetArch: "amd64", Platform: "linux-x64", Filename: "claude", SHA256: strings.Repeat("a", 64), Size: 1024},
			{TargetOS: "linux", TargetArch: "arm64", Platform: "linux-arm64", Filename: "claude", SHA256: strings.Repeat("b", 64), Size: 1024},
		},
		SourceIdentity: artifactFetchTestPlan().SHA512Integrity,
		SourceMaxBytes: 1 << 20,
		BundleMaxBytes: artifact.DefaultClaudeCodeBundleMaxBytes,
		PreviewedAt:    time.Date(2026, 9, 8, 20, 0, 0, 0, time.UTC),
		PreviewDigest:  previewDigestText,
	})
	if err != nil {
		t.Fatal(err)
	}
	plan := artifact.PreviewPlan{
		PolicyVersion:   artifact.ClaudeCodeFetchPolicyVersion,
		SourceKind:      artifact.ArtifactSourceClaudeCode,
		Name:            "claude-code",
		Version:         version,
		RegistryOrigin:  artifact.ProductionClaudeCodeOrigin,
		TarballURL:      checksumURL,
		SHA512Integrity: artifactFetchTestPlan().SHA512Integrity,
		MaxBytes:        1 << 30,
		PreviewedAt:     time.Date(2026, 9, 8, 20, 0, 0, 0, time.UTC),
		PreviewDigest:   previewDigestText,
		SourcePlan:      string(sourcePlan),
	}
	backend := &artifactWorkerBackend{plans: map[string]artifact.PreviewPlan{plan.Version: plan}}
	service, st, _ := artifactFetchTestService(t, backend)
	queued := enqueueStoredArtifactWorkerOperation(t, st, plan, "worker-claude-code")
	if queued.SourceKind != artifact.ArtifactSourceClaudeCode || queued.EnginesNode != "" {
		t.Fatalf("queued claude code operation=%+v", queued)
	}
	var gotPolicy string
	backend.fetch = func(_ context.Context, workerPlan artifact.PreviewPlan, _ string,
		progress artifact.ProgressFunc,
	) (artifact.Sidecar, bool, error) {
		gotPolicy = workerPlan.PolicyVersion
		if workerPlan.SourceKind != artifact.ArtifactSourceClaudeCode ||
			workerPlan.PolicyVersion != artifact.ClaudeCodeFetchPolicyVersion ||
			workerPlan.SourcePlan != plan.SourcePlan || workerPlan.TarballURL != plan.TarballURL {
			return artifact.Sidecar{}, false, fmt.Errorf("reconstructed claude code plan=%+v", workerPlan)
		}
		if err := reportArtifactWorkerPhases(progress, 84); err != nil {
			return artifact.Sidecar{}, false, err
		}
		return artifactWorkerRecord(workerPlan, 84), false, nil
	}
	completed, err := service.RunArtifactFetchOperation(t.Context(), queued.OperationID, false)
	if err != nil {
		t.Fatal(err)
	}
	if gotPolicy != artifact.ClaudeCodeFetchPolicyVersion {
		t.Fatalf("worker policy = %q", gotPolicy)
	}
	if completed.State != store.ArtifactFetchSucceeded || completed.SourceKind != artifact.ArtifactSourceClaudeCode ||
		completed.ProgressBytes != 84 || completed.ResultSizeBytes == nil || *completed.ResultSizeBytes != 84 {
		t.Fatalf("completed claude code operation=%+v", completed)
	}
	raw, err := json.Marshal(completed)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "checksum_url") || strings.Contains(string(raw), "source_plan") {
		t.Fatalf("public claude code operation exposed source plan: %s", raw)
	}
}

func TestRunArtifactFetchOperationPreservesCodexSourcePlan(t *testing.T) {
	previewDigest := sha256.Sum256([]byte("codex-preview"))
	previewDigestText := "sha256:" + hex.EncodeToString(previewDigest[:])
	const version = "0.98.0"
	checksumURL := artifact.ProductionCodexOrigin + "/codex/releases/" + version + "/release.json"
	sourcePlan, err := json.Marshal(artifact.CodexFetchPlan{
		PolicyVersion:  artifact.CodexFetchPolicyVersion,
		Name:           "codex",
		Version:        version,
		SourceOrigin:   artifact.ProductionCodexOrigin,
		ChecksumURL:    checksumURL,
		ChecksumSHA256: strings.Repeat("c", 64),
		Sources: []artifact.CodexSource{
			{TargetOS: "linux", TargetArch: "amd64", Filename: "codex-package-x86_64-unknown-linux-musl.tar.gz", SHA256: strings.Repeat("a", 64)},
			{TargetOS: "linux", TargetArch: "arm64", Filename: "codex-package-aarch64-unknown-linux-musl.tar.gz", SHA256: strings.Repeat("b", 64)},
		},
		SourceIdentity:   artifactFetchTestPlan().SHA512Integrity,
		SourceMaxBytes:   1 << 20,
		ChecksumMaxBytes: 1 << 20,
		BundleMaxBytes:   artifact.DefaultCodexBundleMaxBytes,
		PreviewedAt:      time.Date(2026, 9, 8, 20, 0, 0, 0, time.UTC),
		PreviewDigest:    previewDigestText,
	})
	if err != nil {
		t.Fatal(err)
	}
	plan := artifact.PreviewPlan{
		PolicyVersion:   artifact.CodexFetchPolicyVersion,
		SourceKind:      artifact.ArtifactSourceCodex,
		Name:            "codex",
		Version:         version,
		RegistryOrigin:  artifact.ProductionCodexOrigin,
		TarballURL:      checksumURL,
		SHA512Integrity: artifactFetchTestPlan().SHA512Integrity,
		MaxBytes:        1 << 30,
		PreviewedAt:     time.Date(2026, 9, 8, 20, 0, 0, 0, time.UTC),
		PreviewDigest:   previewDigestText,
		SourcePlan:      string(sourcePlan),
	}
	backend := &artifactWorkerBackend{plans: map[string]artifact.PreviewPlan{plan.Version: plan}}
	service, st, _ := artifactFetchTestService(t, backend)
	queued := enqueueStoredArtifactWorkerOperation(t, st, plan, "worker-codex")
	if queued.SourceKind != artifact.ArtifactSourceCodex || queued.EnginesNode != "" {
		t.Fatalf("queued codex operation=%+v", queued)
	}
	var gotPolicy string
	backend.fetch = func(_ context.Context, workerPlan artifact.PreviewPlan, _ string,
		progress artifact.ProgressFunc,
	) (artifact.Sidecar, bool, error) {
		gotPolicy = workerPlan.PolicyVersion
		if workerPlan.SourceKind != artifact.ArtifactSourceCodex ||
			workerPlan.PolicyVersion != artifact.CodexFetchPolicyVersion ||
			workerPlan.SourcePlan != plan.SourcePlan || workerPlan.TarballURL != plan.TarballURL {
			return artifact.Sidecar{}, false, fmt.Errorf("reconstructed codex plan=%+v", workerPlan)
		}
		if err := reportArtifactWorkerPhases(progress, 84); err != nil {
			return artifact.Sidecar{}, false, err
		}
		return artifactWorkerRecord(workerPlan, 84), false, nil
	}
	completed, err := service.RunArtifactFetchOperation(t.Context(), queued.OperationID, false)
	if err != nil {
		t.Fatal(err)
	}
	if gotPolicy != artifact.CodexFetchPolicyVersion {
		t.Fatalf("worker policy = %q", gotPolicy)
	}
	if completed.State != store.ArtifactFetchSucceeded || completed.SourceKind != artifact.ArtifactSourceCodex ||
		completed.ProgressBytes != 84 || completed.ResultSizeBytes == nil || *completed.ResultSizeBytes != 84 {
		t.Fatalf("completed codex operation=%+v", completed)
	}
	raw, err := json.Marshal(completed)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "checksum_url") || strings.Contains(string(raw), "source_plan") {
		t.Fatalf("public codex operation exposed source plan: %s", raw)
	}
}

func TestRunArtifactFetchOperationRestartClampsRegressiveProgress(t *testing.T) {
	plan := artifactWorkerPlan("2026.9.9")
	backend := &artifactWorkerBackend{plans: map[string]artifact.PreviewPlan{plan.Version: plan}}
	service, st, _ := artifactFetchTestService(t, backend)
	queued := enqueueArtifactWorkerOperation(t, service, plan, "worker-restart-clamp")
	oldClaim, err := st.ClaimArtifactFetchOperation(queued.OperationID, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AdvanceArtifactFetchOperation(queued.OperationID, oldClaim.RunToken,
		store.ArtifactFetchPhaseDownloading, 100); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AdvanceArtifactFetchOperation(queued.OperationID, oldClaim.RunToken,
		store.ArtifactFetchPhaseVerifying, 100); err != nil {
		t.Fatal(err)
	}

	backend.fetch = func(_ context.Context, workerPlan artifact.PreviewPlan, _ string,
		progress artifact.ProgressFunc,
	) (artifact.Sidecar, bool, error) {
		updates := []artifact.FetchProgress{
			{Phase: artifact.FetchPhaseDownloading, DownloadedBytes: 0},
			{Phase: artifact.FetchPhaseDownloading, DownloadedBytes: 110},
			{Phase: artifact.FetchPhaseVerifying, DownloadedBytes: 50},
			{Phase: artifact.FetchPhasePublishing, DownloadedBytes: 80},
		}
		for index, update := range updates {
			if err := progress(update); err != nil {
				return artifact.Sidecar{}, false, err
			}
			current, err := st.GetArtifactFetchOperation(queued.OperationID)
			if err != nil {
				return artifact.Sidecar{}, false, err
			}
			if index < 3 && (current.Phase != store.ArtifactFetchPhaseVerifying || current.ProgressBytes != 100) {
				return artifact.Sidecar{}, false, fmt.Errorf("regressed at %d: %+v", index, current)
			}
			if index == 3 && (current.Phase != store.ArtifactFetchPhasePublishing || current.ProgressBytes != 100) {
				return artifact.Sidecar{}, false, fmt.Errorf("higher phase did not retain max bytes: %+v", current)
			}
		}
		return artifactWorkerRecord(workerPlan, 120), false, nil
	}
	completed, err := service.RunArtifactFetchOperation(t.Context(), queued.OperationID, true)
	if err != nil {
		t.Fatalf("reclaim RunArtifactFetchOperation: %v", err)
	}
	if completed.State != store.ArtifactFetchSucceeded || completed.Attempt != 2 ||
		completed.ProgressBytes != 120 || completed.Phase != store.ArtifactFetchPhaseComplete {
		t.Fatalf("reclaimed result: %+v", completed)
	}
}

func TestRunArtifactFetchOperationPublishingFenceStopsStaleWorker(t *testing.T) {
	plan := artifactWorkerPlan("2026.9.10")
	backend := &artifactWorkerBackend{plans: map[string]artifact.PreviewPlan{plan.Version: plan}}
	service, st, _ := artifactFetchTestService(t, backend)
	queued := enqueueArtifactWorkerOperation(t, service, plan, "worker-publish-fence")
	published := false
	backend.fetch = func(_ context.Context, workerPlan artifact.PreviewPlan, _ string,
		progress artifact.ProgressFunc,
	) (artifact.Sidecar, bool, error) {
		if err := progress(artifact.FetchProgress{Phase: artifact.FetchPhaseDownloading, DownloadedBytes: 42}); err != nil {
			return artifact.Sidecar{}, false, err
		}
		if _, err := st.ClaimArtifactFetchOperation(queued.OperationID, true); err != nil {
			return artifact.Sidecar{}, false, err
		}
		if err := progress(artifact.FetchProgress{Phase: artifact.FetchPhasePublishing, DownloadedBytes: 42}); err != nil {
			return artifact.Sidecar{}, false, err
		}
		published = true
		return artifactWorkerRecord(workerPlan, 42), false, nil
	}
	_, err := service.RunArtifactFetchOperation(t.Context(), queued.OperationID, false)
	if !errors.Is(err, store.ErrArtifactFetchClaimLost) {
		t.Fatalf("stale worker error = %v", err)
	}
	if published {
		t.Fatal("stale worker passed the publishing fence")
	}
	current, err := st.GetArtifactFetchOperation(queued.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	if current.State != store.ArtifactFetchRunning || current.Attempt != 2 || current.ErrorCode != nil || current.FinishedAt != nil {
		t.Fatalf("claim-lost operation was made terminal: %+v", current)
	}
}

func TestRunArtifactFetchOperationCancellationLeavesRunning(t *testing.T) {
	plan := artifactWorkerPlan("2026.9.11")
	backend := &artifactWorkerBackend{plans: map[string]artifact.PreviewPlan{plan.Version: plan}}
	service, st, _ := artifactFetchTestService(t, backend)
	queued := enqueueArtifactWorkerOperation(t, service, plan, "worker-cancel")
	ctx, cancel := context.WithCancel(context.Background())
	backend.fetch = func(_ context.Context, _ artifact.PreviewPlan, _ string,
		progress artifact.ProgressFunc,
	) (artifact.Sidecar, bool, error) {
		if err := progress(artifact.FetchProgress{Phase: artifact.FetchPhaseDownloading, DownloadedBytes: 10}); err != nil {
			return artifact.Sidecar{}, false, err
		}
		cancel()
		return artifact.Sidecar{}, false, ctx.Err()
	}
	_, err := service.RunArtifactFetchOperation(ctx, queued.OperationID, false)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled run error = %v", err)
	}
	current, err := st.GetArtifactFetchOperation(queued.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	if current.State != store.ArtifactFetchRunning || current.ProgressBytes != 10 ||
		current.ErrorCode != nil || current.FinishedAt != nil {
		t.Fatalf("canceled operation was not left recoverable: %+v", current)
	}
}

func TestRunArtifactFetchOperationChildDeadlineBecomesTerminalFailure(t *testing.T) {
	plan := artifactWorkerPlan("2026.9.12")
	backend := &artifactWorkerBackend{plans: map[string]artifact.PreviewPlan{plan.Version: plan}}
	service, st, _ := artifactFetchTestService(t, backend)
	queued := enqueueArtifactWorkerOperation(t, service, plan, "worker-child-deadline")
	backend.fetch = func(ctx context.Context, _ artifact.PreviewPlan, _ string,
		progress artifact.ProgressFunc,
	) (artifact.Sidecar, bool, error) {
		if err := ctx.Err(); err != nil {
			return artifact.Sidecar{}, false, err
		}
		if err := progress(artifact.FetchProgress{
			Phase: artifact.FetchPhaseDownloading, DownloadedBytes: 10,
		}); err != nil {
			return artifact.Sidecar{}, false, err
		}
		return artifact.Sidecar{}, false, fmt.Errorf("bounded tarball download: %w", context.DeadlineExceeded)
	}

	failed, err := service.RunArtifactFetchOperation(context.Background(), queued.OperationID, false)
	if err != nil {
		t.Fatalf("child deadline run: %v", err)
	}
	if failed.State != store.ArtifactFetchFailed || failed.ErrorCode == nil ||
		*failed.ErrorCode != ArtifactFetchFailureUnknown || failed.ErrorDetail == nil ||
		failed.FinishedAt == nil || failed.ProgressBytes != 10 {
		t.Fatalf("child deadline did not become a durable failure: %+v", failed)
	}
	current, err := st.GetArtifactFetchOperation(queued.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	if current.State != store.ArtifactFetchFailed || current.FinishedAt == nil {
		t.Fatalf("persisted child deadline state: %+v", current)
	}
}

func TestRunArtifactFetchOperationPersistsOnlyCanonicalSafeFailure(t *testing.T) {
	tests := []struct {
		name     string
		fetch    func(artifact.PreviewPlan) (artifact.Sidecar, error)
		wantCode string
	}{
		{
			name: "classified integrity failure",
			fetch: func(artifact.PreviewPlan) (artifact.Sidecar, error) {
				return artifact.Sidecar{}, fmt.Errorf("GET https://registry.npmjs.org/private-token /srv/private: %w", artifact.ErrIntegrityMismatch)
			},
			wantCode: ArtifactFetchFailureIntegrityMismatch,
		},
		{
			name: "invalid result identity",
			fetch: func(plan artifact.PreviewPlan) (artifact.Sidecar, error) {
				record := artifactWorkerRecord(plan, 42)
				record.Version = "wrong-version"
				return record, nil
			},
			wantCode: ArtifactFetchFailureResultInvalid,
		},
		{
			name: "unknown failure",
			fetch: func(artifact.PreviewPlan) (artifact.Sidecar, error) {
				return artifact.Sidecar{}, errors.New("secret URL https://registry.npmjs.org/private-token and /srv/private")
			},
			wantCode: ArtifactFetchFailureUnknown,
		},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			plan := artifactWorkerPlan(fmt.Sprintf("2026.10.%d", index+1))
			backend := &artifactWorkerBackend{plans: map[string]artifact.PreviewPlan{plan.Version: plan}}
			service, _, dir := artifactFetchTestService(t, backend)
			queued := enqueueArtifactWorkerOperation(t, service, plan, fmt.Sprintf("worker-safe-failure-%d", index))
			backend.fetch = func(_ context.Context, workerPlan artifact.PreviewPlan, _ string,
				progress artifact.ProgressFunc,
			) (artifact.Sidecar, bool, error) {
				if err := reportArtifactWorkerPhases(progress, 42); err != nil {
					return artifact.Sidecar{}, false, err
				}
				record, err := test.fetch(workerPlan)
				return record, false, err
			}
			failed, err := service.RunArtifactFetchOperation(t.Context(), queued.OperationID, false)
			if err != nil {
				t.Fatalf("RunArtifactFetchOperation: %v", err)
			}
			if failed.State != store.ArtifactFetchFailed || failed.ErrorCode == nil ||
				*failed.ErrorCode != test.wantCode || failed.ErrorDetail == nil || failed.FinishedAt == nil {
				t.Fatalf("failed operation: %+v", failed)
			}
			raw, err := json.Marshal(failed)
			if err != nil {
				t.Fatal(err)
			}
			for _, secret := range []string{"private-token", "/srv/private", dir, plan.TarballURL} {
				if strings.Contains(string(raw), secret) {
					t.Fatalf("persisted failure leaked %q: %s", secret, raw)
				}
			}
		})
	}
}

func TestRecoverArtifactFetchOperationsDoesNoFetchAndREADYDrainRecoversWork(t *testing.T) {
	runningPlan := artifactWorkerPlan("2026.11.1")
	queuedPlan := artifactWorkerPlan("2026.11.2")
	var mu sync.Mutex
	events := []string{}
	backend := &artifactWorkerBackend{plans: map[string]artifact.PreviewPlan{
		runningPlan.Version: runningPlan, queuedPlan.Version: queuedPlan,
	}}
	service, st, _ := artifactFetchTestService(t, backend)
	running := enqueueArtifactWorkerOperation(t, service, runningPlan, "worker-recover-running")
	oldClaim, err := st.ClaimArtifactFetchOperation(running.OperationID, false)
	if err != nil {
		t.Fatal(err)
	}
	queued := enqueueArtifactWorkerOperation(t, service, queuedPlan, "worker-recover-queued")
	backend.reconcile = func(before time.Time) (int, error) {
		if before.IsZero() {
			return 0, errors.New("zero reconcile cutoff")
		}
		mu.Lock()
		events = append(events, "reconcile")
		mu.Unlock()
		return 3, nil
	}
	backend.fetch = func(_ context.Context, plan artifact.PreviewPlan, _ string,
		progress artifact.ProgressFunc,
	) (artifact.Sidecar, bool, error) {
		mu.Lock()
		events = append(events, "fetch:"+plan.Version)
		mu.Unlock()
		if err := reportArtifactWorkerPhases(progress, 42); err != nil {
			return artifact.Sidecar{}, false, err
		}
		return artifactWorkerRecord(plan, 42), false, nil
	}

	fenced, err := service.RecoverArtifactFetchOperations(t.Context())
	if err != nil || fenced != 1 {
		t.Fatalf("RecoverArtifactFetchOperations = %d, %v", fenced, err)
	}
	mu.Lock()
	gotEvents := append([]string(nil), events...)
	mu.Unlock()
	wantEvents := []string{"reconcile"}
	if strings.Join(gotEvents, ",") != strings.Join(wantEvents, ",") {
		t.Fatalf("startup recovery performed external work: events=%v, want %v", gotEvents, wantEvents)
	}
	runningBeforeDrain, err := st.GetArtifactFetchOperation(running.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	queuedBeforeDrain, err := st.GetArtifactFetchOperation(queued.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	if runningBeforeDrain.State != store.ArtifactFetchRunning || runningBeforeDrain.Attempt != 1 ||
		queuedBeforeDrain.State != store.ArtifactFetchQueued || queuedBeforeDrain.Attempt != 0 {
		t.Fatalf("startup recovery changed work state: running=%+v queued=%+v", runningBeforeDrain, queuedBeforeDrain)
	}
	if _, err := st.AdvanceArtifactFetchOperation(running.OperationID, oldClaim.RunToken,
		store.ArtifactFetchPhasePublishing, 0); !errors.Is(err, store.ErrArtifactFetchClaimLost) {
		t.Fatalf("pre-startup run token retained authority: %v", err)
	}

	processed, err := service.RunQueuedArtifactFetchOperations(t.Context())
	if err != nil || processed != 2 {
		t.Fatalf("READY-time drain = %d, %v", processed, err)
	}
	mu.Lock()
	gotEvents = append([]string(nil), events...)
	mu.Unlock()
	wantEvents = []string{"reconcile", "fetch:" + runningPlan.Version, "fetch:" + queuedPlan.Version}
	if strings.Join(gotEvents, ",") != strings.Join(wantEvents, ",") {
		t.Fatalf("READY-time worker order = %v, want %v", gotEvents, wantEvents)
	}
	runningResult, err := st.GetArtifactFetchOperation(running.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	queuedResult, err := st.GetArtifactFetchOperation(queued.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	if runningResult.State != store.ArtifactFetchSucceeded || runningResult.Attempt != 2 ||
		queuedResult.State != store.ArtifactFetchSucceeded || queuedResult.Attempt != 1 {
		t.Fatalf("recovery results: running=%+v queued=%+v", runningResult, queuedResult)
	}
}

func TestRunQueuedArtifactFetchOperationsRecoversRunningAfterTerminalPersistenceFailure(t *testing.T) {
	plan := artifactWorkerPlan("2026.11.3")
	backend := &artifactWorkerBackend{plans: map[string]artifact.PreviewPlan{plan.Version: plan}}
	service, st, _ := artifactFetchTestService(t, backend)
	queued := enqueueArtifactWorkerOperation(t, service, plan, "worker-terminal-persistence-retry")
	fetchCalls := 0
	backend.fetch = func(_ context.Context, workerPlan artifact.PreviewPlan, _ string,
		progress artifact.ProgressFunc,
	) (artifact.Sidecar, bool, error) {
		fetchCalls++
		if err := reportArtifactWorkerPhases(progress, 42); err != nil {
			return artifact.Sidecar{}, false, err
		}
		return artifactWorkerRecord(workerPlan, 42), false, nil
	}
	if _, err := st.DB().Exec(`CREATE TRIGGER reject_artifact_fetch_terminal
 BEFORE UPDATE OF state ON artifact_fetch_operations
 WHEN NEW.state IN ('succeeded','failed')
 BEGIN SELECT RAISE(ABORT, 'test terminal persistence failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := service.RunArtifactFetchOperation(t.Context(), queued.OperationID, false); !errors.Is(err, ErrArtifactFetchWorkerUnavailable) {
		t.Fatalf("terminal persistence failure=%v", err)
	}
	stranded, err := st.GetArtifactFetchOperation(queued.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	if stranded.State != store.ArtifactFetchRunning || stranded.Phase != store.ArtifactFetchPhasePublishing ||
		stranded.Attempt != 1 {
		t.Fatalf("failed terminal write did not leave recoverable running work: %+v", stranded)
	}
	if _, err := st.DB().Exec(`DROP TRIGGER reject_artifact_fetch_terminal`); err != nil {
		t.Fatal(err)
	}
	processed, err := service.RunQueuedArtifactFetchOperations(t.Context())
	if err != nil || processed != 1 {
		t.Fatalf("runtime recovery drain=%d, %v", processed, err)
	}
	completed, err := st.GetArtifactFetchOperation(queued.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	if completed.State != store.ArtifactFetchSucceeded || completed.Attempt != 2 || fetchCalls != 2 {
		t.Fatalf("runtime recovery result=%+v fetch_calls=%d", completed, fetchCalls)
	}
}

func TestPublishedArtifactSuccessPersistenceFailureNeverBecomesFalseFailed(t *testing.T) {
	plan := artifactWorkerPlan("2026.11.4")
	backend := &artifactWorkerBackend{plans: map[string]artifact.PreviewPlan{plan.Version: plan}}
	service, st, _ := artifactFetchTestService(t, backend)
	queued := enqueueArtifactWorkerOperation(t, service, plan, "worker-published-success-retry")
	fetchCalls := 0
	cacheHits := 0
	backend.fetch = func(_ context.Context, workerPlan artifact.PreviewPlan, _ string,
		progress artifact.ProgressFunc,
	) (artifact.Sidecar, bool, error) {
		fetchCalls++
		cached := fetchCalls > 1
		if cached {
			cacheHits++
		}
		if err := reportArtifactWorkerPhases(progress, 42); err != nil {
			return artifact.Sidecar{}, cached, err
		}
		return artifactWorkerRecord(workerPlan, 42), cached, nil
	}
	// This trigger rejects only the succeeded transition. A failed transition
	// would succeed, so the previous implementation deterministically recorded a
	// false failure after the backend had already reported durable publication.
	if _, err := st.DB().Exec(`CREATE TRIGGER reject_first_artifact_fetch_success
 BEFORE UPDATE OF state ON artifact_fetch_operations
 WHEN NEW.state='succeeded'
 BEGIN SELECT RAISE(ABORT, 'test success persistence failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := service.RunArtifactFetchOperation(t.Context(), queued.OperationID, false); !errors.Is(err, ErrArtifactFetchWorkerUnavailable) {
		t.Fatalf("success persistence failure=%v", err)
	}
	stillRunning, err := st.GetArtifactFetchOperation(queued.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	if stillRunning.State != store.ArtifactFetchRunning ||
		stillRunning.Phase != store.ArtifactFetchPhasePublishing || stillRunning.Attempt != 1 ||
		stillRunning.ErrorCode != nil || stillRunning.ErrorDetail != nil || stillRunning.FinishedAt != nil {
		t.Fatalf("published artifact was recorded as terminal failure: %+v", stillRunning)
	}
	if _, err := st.DB().Exec(`DROP TRIGGER reject_first_artifact_fetch_success`); err != nil {
		t.Fatal(err)
	}
	processed, err := service.RunQueuedArtifactFetchOperations(t.Context())
	if err != nil || processed != 1 {
		t.Fatalf("published artifact retry drain=%d, %v", processed, err)
	}
	completed, err := st.GetArtifactFetchOperation(queued.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	if completed.State != store.ArtifactFetchSucceeded || completed.Attempt != 2 ||
		fetchCalls != 2 || cacheHits != 1 {
		t.Fatalf("published artifact retry=%+v fetch_calls=%d cache_hits=%d",
			completed, fetchCalls, cacheHits)
	}
}

func TestRunQueuedArtifactFetchOperationsIsOldestFirstBeyondOnePage(t *testing.T) {
	const operationCount = store.MaxArtifactFetchReadLimit + 1
	plans := make(map[string]artifact.PreviewPlan, operationCount)
	orderedPlans := make([]artifact.PreviewPlan, 0, operationCount)
	for index := 0; index < operationCount; index++ {
		plan := artifactWorkerPlan(fmt.Sprintf("2026.20.%d", index))
		plans[plan.Version] = plan
		orderedPlans = append(orderedPlans, plan)
	}
	backend := &artifactWorkerBackend{plans: plans}
	service, st, _ := artifactFetchTestService(t, backend)
	base := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	for index, plan := range orderedPlans {
		operation := enqueueArtifactWorkerOperation(t, service, plan,
			fmt.Sprintf("worker-oldest-first-%03d", index))
		created := base.Add(time.Duration(index) * time.Second).Format(time.RFC3339)
		if _, err := st.DB().Exec(`UPDATE artifact_fetch_operations
 SET created_at=?,updated_at=? WHERE operation_id=?`, created, created, operation.OperationID); err != nil {
			t.Fatalf("set deterministic queue order %d: %v", index, err)
		}
	}
	var fetched []string
	backend.fetch = func(_ context.Context, plan artifact.PreviewPlan, _ string,
		progress artifact.ProgressFunc,
	) (artifact.Sidecar, bool, error) {
		fetched = append(fetched, plan.Version)
		if err := reportArtifactWorkerPhases(progress, 42); err != nil {
			return artifact.Sidecar{}, false, err
		}
		return artifactWorkerRecord(plan, 42), false, nil
	}

	processed, err := service.RunQueuedArtifactFetchOperations(t.Context())
	if err != nil || processed != operationCount {
		t.Fatalf("oldest-first drain=%d, %v", processed, err)
	}
	if len(fetched) != operationCount {
		t.Fatalf("fetched %d operations", len(fetched))
	}
	for index, version := range fetched {
		if version != orderedPlans[index].Version {
			t.Fatalf("fetch[%d]=%s, want oldest-first %s", index, version, orderedPlans[index].Version)
		}
	}
}

func TestRecoverArtifactFetchOperationsStopsWhenReconcileFails(t *testing.T) {
	plan := artifactWorkerPlan("2026.12.1")
	backend := &artifactWorkerBackend{plans: map[string]artifact.PreviewPlan{plan.Version: plan}}
	service, st, _ := artifactFetchTestService(t, backend)
	queued := enqueueArtifactWorkerOperation(t, service, plan, "worker-reconcile-failure")
	backend.reconcile = func(time.Time) (int, error) {
		return 0, errors.New("/private/path could not be reconciled")
	}
	processed, err := service.RecoverArtifactFetchOperations(t.Context())
	if processed != 0 || !errors.Is(err, ErrArtifactFetchWorkerUnavailable) || strings.Contains(err.Error(), "/private/path") {
		t.Fatalf("reconcile failure = processed %d, error %v", processed, err)
	}
	current, err := st.GetArtifactFetchOperation(queued.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	if current.State != store.ArtifactFetchQueued {
		t.Fatalf("reconcile failure started queued work: %+v", current)
	}
}

type recordingBlobPublisher struct {
	digest string
	size   int64
	dir    string
	err    error
	calls  int
}

func (p *recordingBlobPublisher) PublishArtifact(_ context.Context, artifactsDir, digest string, size int64) error {
	p.calls++
	p.dir = artifactsDir
	p.digest = digest
	p.size = size
	return p.err
}

func TestRunArtifactFetchOperationPublishesMeasuredBlobBeforeSuccess(t *testing.T) {
	plan := artifactWorkerPlan("2026.10.5")
	backend := &artifactWorkerBackend{plans: map[string]artifact.PreviewPlan{plan.Version: plan}}
	service, _, dir := artifactFetchTestService(t, backend)
	queued := enqueueArtifactWorkerOperation(t, service, plan, "worker-blob-ok")
	publisher := &recordingBlobPublisher{}
	service.SetArtifactBlobPublisher(publisher)
	record := artifactWorkerRecord(plan, 42)
	backend.fetch = func(_ context.Context, _ artifact.PreviewPlan, _ string, progress artifact.ProgressFunc) (artifact.Sidecar, bool, error) {
		if err := reportArtifactWorkerPhases(progress, 42); err != nil {
			return artifact.Sidecar{}, false, err
		}
		return record, true, nil
	}
	completed, err := service.RunArtifactFetchOperation(context.Background(), queued.OperationID, false)
	if err != nil {
		t.Fatal(err)
	}
	if completed.State != store.ArtifactFetchSucceeded || publisher.calls != 1 ||
		publisher.digest != record.SHA256 || publisher.size != 42 || publisher.dir != dir {
		t.Fatalf("completed=%+v publisher=%+v", completed, publisher)
	}
}

func TestRunArtifactFetchOperationStorageFailureDoesNotSucceed(t *testing.T) {
	plan := artifactWorkerPlan("2026.10.6")
	backend := &artifactWorkerBackend{plans: map[string]artifact.PreviewPlan{plan.Version: plan}}
	service, st, _ := artifactFetchTestService(t, backend)
	queued := enqueueArtifactWorkerOperation(t, service, plan, "worker-blob-fail")
	publisher := &recordingBlobPublisher{err: errors.New("put s3://secret-bucket/private failed")}
	service.SetArtifactBlobPublisher(publisher)
	record := artifactWorkerRecord(plan, 42)
	backend.fetch = func(_ context.Context, _ artifact.PreviewPlan, _ string, progress artifact.ProgressFunc) (artifact.Sidecar, bool, error) {
		if err := reportArtifactWorkerPhases(progress, 42); err != nil {
			return artifact.Sidecar{}, false, err
		}
		return record, false, nil
	}
	failed, err := service.RunArtifactFetchOperation(context.Background(), queued.OperationID, false)
	if err != nil {
		t.Fatalf("storage failure returned a worker error: %v", err)
	}
	if failed.State != store.ArtifactFetchFailed || failed.ErrorCode == nil ||
		*failed.ErrorCode != ArtifactFetchFailureStorage || failed.ErrorDetail == nil ||
		*failed.ErrorDetail != "remote object publish failed" || strings.Contains(*failed.ErrorDetail, "secret") {
		t.Fatalf("failure=%+v", failed)
	}
	if publisher.calls != 1 || publisher.digest != record.SHA256 {
		t.Fatalf("publisher=%+v", publisher)
	}
	current, err := st.GetArtifactFetchOperation(queued.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	if current.State != store.ArtifactFetchFailed || current.ResultSHA256 != nil {
		t.Fatalf("persisted=%+v", current)
	}
}

func TestRunArtifactFetchOperationBlobClaimLossStaysRunning(t *testing.T) {
	plan := artifactWorkerPlan("2026.10.7")
	backend := &artifactWorkerBackend{plans: map[string]artifact.PreviewPlan{plan.Version: plan}}
	service, st, _ := artifactFetchTestService(t, backend)
	queued := enqueueArtifactWorkerOperation(t, service, plan, "worker-blob-claim")
	service.SetArtifactBlobPublisher(&recordingBlobPublisher{err: store.ErrArtifactFetchClaimLost})
	backend.fetch = func(_ context.Context, workerPlan artifact.PreviewPlan, _ string, progress artifact.ProgressFunc) (artifact.Sidecar, bool, error) {
		if err := reportArtifactWorkerPhases(progress, 42); err != nil {
			return artifact.Sidecar{}, false, err
		}
		return artifactWorkerRecord(workerPlan, 42), false, nil
	}
	_, err := service.RunArtifactFetchOperation(context.Background(), queued.OperationID, false)
	if !errors.Is(err, store.ErrArtifactFetchClaimLost) {
		t.Fatalf("claim loss err=%v", err)
	}
	current, err := st.GetArtifactFetchOperation(queued.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	if current.State != store.ArtifactFetchRunning || current.ErrorCode != nil || current.FinishedAt != nil {
		t.Fatalf("claim loss became terminal: %+v", current)
	}
}
