package operator

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/teddashh/AI-Intune/internal/artifact"
	"github.com/teddashh/AI-Intune/internal/store"
)

const (
	ArtifactFetchFailurePlanInvalid       = "ARTIFACT_FETCH_PLAN_INVALID"
	ArtifactFetchFailureRegistryPolicy    = "ARTIFACT_FETCH_REGISTRY_POLICY"
	ArtifactFetchFailureMetadataInvalid   = "ARTIFACT_FETCH_METADATA_INVALID"
	ArtifactFetchFailureMetadataTooLarge  = "ARTIFACT_FETCH_METADATA_TOO_LARGE"
	ArtifactFetchFailureArtifactTooLarge  = "ARTIFACT_FETCH_ARTIFACT_TOO_LARGE"
	ArtifactFetchFailureIntegrityMismatch = "ARTIFACT_FETCH_INTEGRITY_MISMATCH"
	ArtifactFetchFailureStorage           = "ARTIFACT_FETCH_STORAGE_FAILED"
	ArtifactFetchFailureProgress          = "ARTIFACT_FETCH_PROGRESS_FAILED"
	ArtifactFetchFailureResultInvalid     = "ARTIFACT_FETCH_RESULT_INVALID"
	ArtifactFetchFailureUnknown           = "ARTIFACT_FETCH_FAILED"

	artifactFetchWorkerIdentityPrefix = "artifact-fetch-operation:"
)

var ErrArtifactFetchWorkerUnavailable = errors.New("operator: artifact fetch worker is unavailable")

type artifactFetchFailure struct {
	code   string
	detail string
}

var artifactFetchPhaseRank = map[store.ArtifactFetchPhase]int{
	store.ArtifactFetchPhaseDownloading: 1,
	store.ArtifactFetchPhaseVerifying:   2,
	store.ArtifactFetchPhasePublishing:  3,
}

// RunArtifactFetchOperation drives one queued operation, or reclaims one
// running operation during startup recovery. A nil error means the operation
// reached a durable terminal state, including a safely classified failure.
// Cancellation and a lost worker claim intentionally leave it running so a
// later startup recovery can reclaim it.
func (s *Service) RunArtifactFetchOperation(ctx context.Context, operationID string,
	reclaimRunning bool,
) (store.ArtifactFetchOperation, error) {
	if s == nil || s.store == nil || s.artifactFetcher == nil || ctx == nil {
		return store.ArtifactFetchOperation{}, ErrArtifactFetchWorkerUnavailable
	}
	if err := ctx.Err(); err != nil {
		return store.ArtifactFetchOperation{}, err
	}
	claim, err := s.store.ClaimArtifactFetchOperation(operationID, reclaimRunning)
	if err != nil {
		return store.ArtifactFetchOperation{}, err
	}
	plan := artifact.PreviewPlan{
		PolicyVersion: artifact.FetchPolicyVersion, SourceKind: claim.Operation.SourceKind,
		Name: claim.Operation.Name, Version: claim.Operation.Version,
		RegistryOrigin: claim.Operation.RegistryOrigin, TarballURL: claim.TarballURL,
		SHA512Integrity: claim.Operation.SHA512Integrity, EnginesNode: claim.Operation.EnginesNode,
		MaxBytes: claim.Operation.MaxBytes, PreviewedAt: claim.Operation.CreatedAt,
		PreviewDigest: claim.Operation.PreviewDigest, SourcePlan: claim.SourcePlan,
	}
	if plan.SourceKind == "" {
		plan.SourceKind = artifact.ArtifactSourceNPM
	}
	if plan.SourceKind == artifact.ArtifactSourceNode {
		plan.PolicyVersion = artifact.NodeRuntimeFetchPolicyVersion
	}
	if plan.SourceKind == artifact.ArtifactSourceHermesImage {
		plan.PolicyVersion = artifact.HermesImageFetchPolicyVersion
	}
	progress := artifactFetchProgressWriter{
		ctx: ctx, store: s.store, operationID: claim.Operation.OperationID,
		runToken: claim.RunToken, phase: claim.Operation.Phase,
		bytes: claim.Operation.ProgressBytes, maxBytes: claim.Operation.MaxBytes,
	}
	record, _, fetchErr := s.artifactFetcher.FetchExact(ctx, plan,
		artifactFetchWorkerIdentityPrefix+claim.Operation.OperationID, progress.write)
	if fetchErr != nil {
		return s.finishArtifactFetchFailure(ctx, claim, fetchErr)
	}
	if failure := validateArtifactFetchWorkerResult(claim, record); failure != nil {
		return s.finishArtifactFetchFailure(ctx, claim, failure)
	}
	// A conforming fetcher reports publishing before it renames. Repeat the
	// fenced advancement here so even a cache hit or backend implementation that
	// coalesces progress cannot call Succeed from an earlier Store phase.
	if err := progress.write(artifact.FetchProgress{
		Phase: artifact.FetchPhasePublishing, DownloadedBytes: record.Size,
	}); err != nil {
		return s.finishArtifactFetchFailure(ctx, claim, err)
	}
	if err := ctx.Err(); err != nil {
		return claim.Operation, err
	}
	// Mirror after the local tarball exists and before the ledger says success.
	// A failed upload leaves the operation running or terminally failed so the
	// next drain can retry from the cache hit. A nil publisher is local-only.
	if s.blobPublisher != nil {
		if err := s.blobPublisher.PublishArtifact(ctx, s.artifactsDir, record.SHA256, record.Size); err != nil {
			if artifactFetchWorkerMustRemainRunning(ctx, err) {
				return claim.Operation, err
			}
			return s.finishArtifactFetchFailure(ctx, claim, artifactFetchFailure{
				code:   ArtifactFetchFailureStorage,
				detail: "remote object publish failed",
			})
		}
	}
	completed, err := s.store.SucceedArtifactFetchOperation(
		claim.Operation.OperationID, claim.RunToken, record.SHA256, record.Size)
	if err != nil {
		// FetchExact has already durably published both the tarball and its
		// authoritative sidecar. A transient ledger failure here must not turn that
		// successful external effect into a false terminal failure. Leave the row
		// running; the serial background drain will rotate the token, observe the
		// cache hit, and retry the succeeded transition.
		if artifactFetchWorkerMustRemainRunning(ctx, err) {
			return claim.Operation, err
		}
		return claim.Operation, fmt.Errorf("%w: persist terminal artifact fetch success", ErrArtifactFetchWorkerUnavailable)
	}
	return completed, nil
}

type artifactFetchProgressWriter struct {
	ctx         context.Context
	store       *store.Store
	operationID string
	runToken    string
	phase       store.ArtifactFetchPhase
	bytes       int64
	maxBytes    int64
}

func (w *artifactFetchProgressWriter) write(update artifact.FetchProgress) error {
	if err := w.ctx.Err(); err != nil {
		return err
	}
	phase, ok := artifactFetchStorePhase(update.Phase)
	if !ok || update.DownloadedBytes < 0 || update.DownloadedBytes > w.maxBytes {
		return store.ErrArtifactFetchProgress
	}
	currentRank, currentOK := artifactFetchPhaseRank[w.phase]
	nextRank := artifactFetchPhaseRank[phase]
	if !currentOK || currentRank == 0 || nextRank == 0 {
		return store.ErrArtifactFetchProgress
	}
	// On restart, the fresh byte fetch begins again at downloading/zero while
	// the durable operation may already be verifying or publishing. Never
	// regress the Store. A genuinely higher phase advances while retaining the
	// largest already-durable byte count.
	if nextRank < currentRank || (nextRank == currentRank && update.DownloadedBytes < w.bytes) {
		return nil
	}
	progressBytes := update.DownloadedBytes
	if progressBytes < w.bytes {
		progressBytes = w.bytes
	}
	advanced, err := w.store.AdvanceArtifactFetchOperation(
		w.operationID, w.runToken, phase, progressBytes)
	if err != nil {
		return err
	}
	w.phase, w.bytes = advanced.Phase, advanced.ProgressBytes
	return nil
}

func artifactFetchStorePhase(phase artifact.FetchPhase) (store.ArtifactFetchPhase, bool) {
	switch phase {
	case artifact.FetchPhaseDownloading:
		return store.ArtifactFetchPhaseDownloading, true
	case artifact.FetchPhaseVerifying:
		return store.ArtifactFetchPhaseVerifying, true
	case artifact.FetchPhasePublishing:
		return store.ArtifactFetchPhasePublishing, true
	default:
		return "", false
	}
}

func validateArtifactFetchWorkerResult(claim store.ArtifactFetchClaim,
	record artifact.Sidecar,
) error {
	if record.Name != claim.Operation.Name || record.Version != claim.Operation.Version ||
		record.TarballURL != claim.TarballURL ||
		record.SHA512Integrity != claim.Operation.SHA512Integrity ||
		record.EnginesNode != claim.Operation.EnginesNode ||
		!artifact.ValidSHA256Hex(record.SHA256) || record.Size <= 0 ||
		record.Size > claim.Operation.MaxBytes || record.FetchedAt.IsZero() {
		return artifactFetchFailure{
			code:   ArtifactFetchFailureResultInvalid,
			detail: "fetch backend returned an inconsistent artifact identity",
		}
	}
	return nil
}

func (e artifactFetchFailure) Error() string { return e.code }

func (s *Service) finishArtifactFetchFailure(ctx context.Context, claim store.ArtifactFetchClaim,
	cause error,
) (store.ArtifactFetchOperation, error) {
	if artifactFetchWorkerMustRemainRunning(ctx, cause) {
		return claim.Operation, cause
	}
	failure := classifyArtifactFetchFailure(cause)
	failed, err := s.store.FailArtifactFetchOperation(
		claim.Operation.OperationID, claim.RunToken, failure.code, failure.detail)
	if err != nil {
		if artifactFetchWorkerMustRemainRunning(ctx, err) {
			return claim.Operation, err
		}
		return claim.Operation, fmt.Errorf("%w: persist terminal failure: %v", ErrArtifactFetchWorkerUnavailable, err)
	}
	return failed, nil
}

func artifactFetchWorkerMustRemainRunning(ctx context.Context, err error) bool {
	// FetchExact owns bounded child contexts for metadata and tarball I/O. A
	// child timeout/cancel while the worker lifecycle is still live is an
	// operation failure, not a reason to strand the row in running. Only the
	// parent lifecycle ending, or losing the durable claim, is recoverable by a
	// later worker startup.
	return ctx.Err() != nil || errors.Is(err, store.ErrArtifactFetchClaimLost)
}

func classifyArtifactFetchFailure(err error) artifactFetchFailure {
	var classified artifactFetchFailure
	if errors.As(err, &classified) {
		return classified
	}
	switch {
	case errors.Is(err, artifact.ErrInvalidFetchRequest):
		return artifactFetchFailure{ArtifactFetchFailurePlanInvalid, "persisted artifact fetch plan did not satisfy the active policy"}
	case errors.Is(err, artifact.ErrRegistryPolicy):
		return artifactFetchFailure{ArtifactFetchFailureRegistryPolicy, "artifact source did not satisfy the active registry policy"}
	case errors.Is(err, artifact.ErrMetadataTooLarge):
		return artifactFetchFailure{ArtifactFetchFailureMetadataTooLarge, "registry metadata exceeded the accepted size limit"}
	case errors.Is(err, artifact.ErrMetadataInvalid):
		return artifactFetchFailure{ArtifactFetchFailureMetadataInvalid, "registry metadata or response representation was invalid"}
	case errors.Is(err, artifact.ErrArtifactTooLarge):
		return artifactFetchFailure{ArtifactFetchFailureArtifactTooLarge, "artifact exceeded the accepted size limit"}
	case errors.Is(err, artifact.ErrIntegrityMismatch):
		return artifactFetchFailure{ArtifactFetchFailureIntegrityMismatch, "artifact bytes did not match the pinned integrity"}
	case errors.Is(err, artifact.ErrArtifactStorage):
		return artifactFetchFailure{ArtifactFetchFailureStorage, "artifact storage did not complete safely"}
	case errors.Is(err, store.ErrArtifactFetchProgress), errors.Is(err, store.ErrArtifactFetchInvalidState):
		return artifactFetchFailure{ArtifactFetchFailureProgress, "artifact worker could not advance durable progress"}
	default:
		return artifactFetchFailure{ArtifactFetchFailureUnknown, "artifact fetch did not complete"}
	}
}

// RunQueuedArtifactFetchOperations first reclaims running work left by a
// completed/canceled worker invocation, then drains queued work. The Hub calls
// this method from one serial background loop, so reclaim never steals from a
// concurrently active lifecycle worker. Operations that durably fail still
// count as run.
func (s *Service) RunQueuedArtifactFetchOperations(ctx context.Context) (int, error) {
	running, err := s.drainArtifactFetchOperations(ctx, store.ArtifactFetchRunning, true)
	if err != nil {
		return running, err
	}
	queued, err := s.drainArtifactFetchOperations(ctx, store.ArtifactFetchQueued, false)
	return running + queued, err
}

// RecoverArtifactFetchOperations is the bounded, local-only startup phase. Once
// the previous process is known dead it removes stale private temps and rotates
// every running capability. It deliberately performs no registry/download I/O;
// the READY-time background loop reclaims fenced running work and then queued
// work.
func (s *Service) RecoverArtifactFetchOperations(ctx context.Context) (int, error) {
	if s == nil || s.store == nil || s.artifactFetcher == nil || ctx == nil {
		return 0, ErrArtifactFetchWorkerUnavailable
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if _, err := s.artifactFetcher.ReconcileStaleTemps(time.Now().UTC()); err != nil {
		return 0, fmt.Errorf("%w: reconcile artifact fetch temporary files", ErrArtifactFetchWorkerUnavailable)
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	fenced, err := s.store.FenceRunningArtifactFetchOperations()
	if err != nil {
		return 0, fmt.Errorf("%w: fence running artifact fetch operations", ErrArtifactFetchWorkerUnavailable)
	}
	return fenced, nil
}

func (s *Service) drainArtifactFetchOperations(ctx context.Context, state store.ArtifactFetchState,
	reclaimRunning bool,
) (int, error) {
	if s == nil || s.store == nil || s.artifactFetcher == nil || ctx == nil {
		return 0, ErrArtifactFetchWorkerUnavailable
	}
	processed := 0
	for {
		if err := ctx.Err(); err != nil {
			return processed, err
		}
		operationIDs, err := s.store.ListArtifactFetchOperationIDsForWorker(
			state, store.MaxArtifactFetchReadLimit)
		if err != nil {
			return processed, err
		}
		if len(operationIDs) == 0 {
			return processed, nil
		}
		for _, operationID := range operationIDs {
			if _, err := s.RunArtifactFetchOperation(ctx, operationID, reclaimRunning); err != nil {
				return processed, err
			}
			processed++
		}
	}
}
