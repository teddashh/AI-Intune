package operator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"time"

	"github.com/teddashh/AI-Intune/internal/agentadapter"
	"github.com/teddashh/AI-Intune/internal/artifact"
	appcatalog "github.com/teddashh/AI-Intune/internal/catalog"
	"github.com/teddashh/AI-Intune/internal/store"
)

var ErrInvalidCatalogManifestPublication = errors.New("operator: invalid catalog manifest publication")

type CatalogManifestPublishRequest struct {
	Manifest         appcatalog.Manifest `json:"manifest"`
	ConfirmPackageID string              `json:"-"`
	ConfirmVersion   string              `json:"-"`
	PreviewDigest    string              `json:"-"`
	Reason           string              `json:"reason"`
	IdempotencyKey   string              `json:"-"`
	Actor            Actor               `json:"-"`
}

// CatalogManifestRecord is the public immutable catalog projection. The
// Store's publisher identity remains audit evidence and never enters API,
// CLI, or browser response bodies.
type CatalogManifestRecord struct {
	Manifest    appcatalog.Manifest `json:"manifest"`
	Digest      string              `json:"manifest_digest"`
	PublishedAt time.Time           `json:"published_at"`
	PublishedBy string              `json:"-"`
}

type CatalogManifestPublishResult struct {
	Record           CatalogManifestRecord `json:"record"`
	AlreadyPublished bool                  `json:"already_published"`
	Replayed         bool                  `json:"replayed"`
	Audited          bool                  `json:"-"`
}

// PublishCatalogManifest verifies the exact Hub-owned artifact and the agent
// adapter contract before asking Store to atomically admit the manifest.
func (s *Service) PublishCatalogManifest(ctx context.Context,
	request CatalogManifestPublishRequest,
) (CatalogManifestPublishResult, error) {
	return s.publishCatalogManifest(ctx, request, nil)
}

func (s *Service) publishCatalogManifest(ctx context.Context, request CatalogManifestPublishRequest,
	extraVerify func(artifact.Sidecar) error,
) (CatalogManifestPublishResult, error) {
	if s == nil || s.store == nil || ctx == nil {
		return CatalogManifestPublishResult{}, ErrInvalidCatalogManifestPublication
	}
	digest := CatalogManifestPublishSemanticDigest(request)
	publishedBy, _ := deploymentCreatedBy(request.Actor)
	audit := auditFromActor(request.Actor)
	audit.Reason, audit.IdempotencyKey, audit.RequestDigest = request.Reason, request.IdempotencyKey, digest
	stored, err := s.store.ApplyOperatorCatalogManifest(store.OperatorCatalogManifestRequest{
		Manifest: request.Manifest, PublishedBy: publishedBy, Reason: request.Reason,
		IdempotencyKey: request.IdempotencyKey, RequestDigest: digest, Audit: audit,
	}, func() error {
		if err := validateCatalogAdapterContract(request.Manifest); err != nil {
			return err
		}
		entry, inspectErr := artifact.InspectCatalogEntry(ctx, s.artifactsDir, request.Manifest.Artifact.SHA256)
		if errors.Is(inspectErr, artifact.ErrCatalogEntryNotFound) {
			return catalogManifestRejection(store.OperatorCodeCatalogArtifactUnavailable)
		}
		if inspectErr != nil {
			return inspectErr
		}
		if entry.Status != artifact.CatalogReady || entry.Record == nil {
			return catalogManifestRejection(store.OperatorCodeCatalogArtifactUnavailable)
		}
		record := entry.Record
		if entry.SHA256 != request.Manifest.Artifact.SHA256 ||
			record.SHA256 != request.Manifest.Artifact.SHA256 ||
			record.Name != request.Manifest.ID || record.Version != request.Manifest.Version ||
			record.Size != request.Manifest.Artifact.Size {
			return catalogManifestRejection(store.OperatorCodeCatalogArtifactMismatch)
		}
		if record.Name == "bat-server" {
			targets := make([]artifact.NodeRuntimeTarget, 0, len(request.Manifest.Platforms))
			for _, platform := range request.Manifest.Platforms {
				targets = append(targets, artifact.NodeRuntimeTarget{OS: platform.OS, Arch: platform.Arch})
			}
			if err := artifact.ValidateBATServerBundleTargetsContext(ctx, s.artifactsDir, *record, targets...); err != nil {
				return catalogManifestRejection(store.OperatorCodeCatalogArtifactMismatch)
			}
		} else if record.Name == "node-runtime" || record.Name == "claude-code" || record.Name == "codex" || record.Name == "grok" || record.Name == "antigravity" {
			targets := make([]artifact.NodeRuntimeTarget, 0, len(request.Manifest.Platforms))
			for _, platform := range request.Manifest.Platforms {
				targets = append(targets, artifact.NodeRuntimeTarget{OS: platform.OS, Arch: platform.Arch})
			}
			var validateErr error
			switch record.Name {
			case "node-runtime":
				validateErr = artifact.ValidateNodeRuntimeBundleTargetsContext(ctx, s.artifactsDir, *record, targets...)
			case "claude-code":
				validateErr = artifact.ValidateClaudeCodeBundleTargetsContext(ctx, s.artifactsDir, *record, targets...)
			case "codex":
				validateErr = artifact.ValidateCodexBundleTargetsContext(ctx, s.artifactsDir, *record, targets...)
			case "grok":
				validateErr = artifact.ValidateGrokBundleTargetsContext(ctx, s.artifactsDir, *record, targets...)
			case "antigravity":
				validateErr = artifact.ValidateAntigravityBundleTargetsContext(ctx, s.artifactsDir, *record, targets...)
			default:
				return catalogManifestRejection(store.OperatorCodeCatalogArtifactMismatch)
			}
			if validateErr != nil {
				return catalogManifestRejection(store.OperatorCodeCatalogArtifactMismatch)
			}
		}
		if extraVerify != nil {
			return extraVerify(*record)
		}
		return nil
	})
	if err != nil {
		s.recordCatalogManifestFallback(request, digest, stored.Audited, err)
		return CatalogManifestPublishResult{}, err
	}
	return CatalogManifestPublishResult{
		Record: CatalogManifestRecord{
			Manifest: stored.Record.Manifest, Digest: stored.Record.Digest,
			PublishedAt: stored.Record.PublishedAt, PublishedBy: stored.Record.PublishedBy,
		},
		AlreadyPublished: stored.AlreadyPublished, Replayed: stored.Replayed, Audited: stored.Audited,
	}, nil
}

func validateCatalogAdapterContract(manifest appcatalog.Manifest) error {
	if !agentadapter.SupportsManifest(manifest) {
		return catalogManifestRejection(store.OperatorCodeCatalogAdapterUnsupported)
	}
	return nil
}

func catalogManifestRejection(code string) *store.OperatorRequestError {
	return &store.OperatorRequestError{Code: code, Detail: code}
}

// CatalogManifestPublishSemanticDigest binds operator-authored manifest and
// reason while excluding transport identity and the idempotency key.
func CatalogManifestPublishSemanticDigest(request CatalogManifestPublishRequest) string {
	manifest := request.Manifest
	if raw, err := json.Marshal(manifest); err == nil {
		if canonical, err := appcatalog.ParseManifest(raw); err == nil {
			manifest = canonical
		}
	}
	body := struct {
		Manifest         appcatalog.Manifest `json:"manifest"`
		ConfirmPackageID string              `json:"confirm_package_id"`
		ConfirmVersion   string              `json:"confirm_version"`
		PreviewDigest    string              `json:"preview_digest"`
		Reason           string              `json:"reason"`
	}{Manifest: manifest, ConfirmPackageID: request.ConfirmPackageID,
		ConfirmVersion: request.ConfirmVersion, PreviewDigest: request.PreviewDigest, Reason: request.Reason}
	raw, _ := json.Marshal(body)
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func (s *Service) recordCatalogManifestFallback(request CatalogManifestPublishRequest,
	digest string, audited bool, err error,
) {
	var rejection *store.OperatorRequestError
	if audited || (errors.As(err, &rejection) && rejection.Audited) {
		return
	}
	entry := auditFromActor(request.Actor)
	entry.Action = store.AuditCatalogManifest
	entry.Subject = "catalog manifest request"
	if appcatalog.ValidateManifest(request.Manifest) == nil {
		entry.Subject = request.Manifest.ID + "@" + request.Manifest.Version
	}
	if validDeploymentText(request.Reason, 500, false) {
		entry.Reason = request.Reason
	}
	entry.IdempotencyKey, entry.RequestDigest, entry.OK = request.IdempotencyKey, digest, false
	entry.Detail = "catalog manifest verification failed"
	if auditErr := s.store.RecordAudit(entry); auditErr != nil {
		log.Printf("operator catalog manifest audit 寫入失敗 subject=%s: %v", entry.Subject, auditErr)
	}
}
