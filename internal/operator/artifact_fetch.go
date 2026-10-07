package operator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/teddashh/AI-Intune/internal/artifact"
	"github.com/teddashh/AI-Intune/internal/store"
)

const ArtifactFetchPreviewSchemaVersion = 2

var ErrInvalidArtifactFetchPreview = errors.New("operator: invalid artifact fetch preview")

type artifactFetchBackend interface {
	PreviewPlan(context.Context, string, string) (artifact.PreviewPlan, error)
	FetchExact(context.Context, artifact.PreviewPlan, string, artifact.ProgressFunc) (artifact.Sidecar, bool, error)
	ReconcileStaleTemps(time.Time) (int, error)
}

type ArtifactFetchPreviewRequest struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// ArtifactFetchPreviewResult is an explicit safe projection. The exact
// tarball URL remains worker-only because registries may use signed URLs.
type ArtifactFetchPreviewResult struct {
	SchemaVersion               int       `json:"schema_version"`
	PolicyVersion               string    `json:"policy_version"`
	SourceKind                  string    `json:"source_kind"`
	PreviewedAt                 time.Time `json:"previewed_at"`
	Name                        string    `json:"name"`
	Version                     string    `json:"version"`
	RegistryOrigin              string    `json:"registry_origin"`
	SHA512Integrity             string    `json:"sha512_integrity"`
	EnginesNode                 *string   `json:"engines_node"`
	MaxBytes                    int64     `json:"max_bytes"`
	AlreadyAvailableAndVerified bool      `json:"already_available_and_verified"`
	ExistingArtifactSHA256      *string   `json:"existing_artifact_sha256"`
	PreviewDigest               string    `json:"preview_digest"`
	EnqueueAllowed              bool      `json:"enqueue_allowed"`
	Blockers                    []string  `json:"blockers"`
}

type ArtifactFetchApplyRequest struct {
	Name           string `json:"name"`
	Version        string `json:"version"`
	PreviewDigest  string `json:"preview_digest"`
	ConfirmName    string `json:"confirm_name"`
	ConfirmVersion string `json:"confirm_version"`
	Reason         string `json:"reason"`
	IdempotencyKey string `json:"-"`
	Actor          Actor  `json:"-"`
}

type ArtifactFetchApplyResult = store.OperatorArtifactFetchResult

func (s *Service) PreviewArtifactFetch(ctx context.Context, request ArtifactFetchPreviewRequest) (ArtifactFetchPreviewResult, error) {
	if s == nil || s.store == nil || s.artifactFetcher == nil || ctx == nil ||
		!validArtifactFetchTarget(request.Name, request.Version) {
		return ArtifactFetchPreviewResult{}, fmt.Errorf("%w: supported package, exact version, and configured artifact fetcher are required", ErrInvalidArtifactFetchPreview)
	}
	plan, err := s.artifactFetcher.PreviewPlan(ctx, request.Name, request.Version)
	if err != nil {
		return ArtifactFetchPreviewResult{}, err
	}
	if err := validateArtifactFetchPlan(plan, request); err != nil {
		return ArtifactFetchPreviewResult{}, err
	}
	result := ArtifactFetchPreviewResult{
		SchemaVersion: ArtifactFetchPreviewSchemaVersion, PolicyVersion: plan.PolicyVersion,
		SourceKind:  plan.SourceKind,
		PreviewedAt: plan.PreviewedAt.UTC(), Name: plan.Name, Version: plan.Version,
		RegistryOrigin: plan.RegistryOrigin, SHA512Integrity: plan.SHA512Integrity,
		MaxBytes: plan.MaxBytes, PreviewDigest: plan.PreviewDigest,
		EnqueueAllowed: true, Blockers: []string{},
	}
	if plan.EnginesNode != "" {
		engines := plan.EnginesNode
		result.EnginesNode = &engines
	}
	digest, available, err := s.existingVerifiedArtifact(ctx, plan)
	if err != nil {
		return ArtifactFetchPreviewResult{}, err
	}
	result.AlreadyAvailableAndVerified = available
	if available {
		result.ExistingArtifactSHA256 = &digest
	}
	return result, nil
}

func validateArtifactFetchPlan(plan artifact.PreviewPlan, request ArtifactFetchPreviewRequest) error {
	if plan.Name != request.Name || plan.Version != request.Version ||
		plan.PreviewedAt.IsZero() || plan.PreviewedAt.Location() != time.UTC ||
		!strings.HasPrefix(plan.SHA512Integrity, "sha512-") ||
		!artifactFetchDigest(plan.PreviewDigest) ||
		!validDeploymentText(plan.EnginesNode, 512, true) {
		return fmt.Errorf("%w: fetch backend returned an incoherent plan", ErrInvalidArtifactFetchPreview)
	}
	switch plan.SourceKind {
	case artifact.ArtifactSourceNPM:
		if plan.PolicyVersion != artifact.FetchPolicyVersion || plan.RegistryOrigin != artifact.ProductionRegistryOrigin ||
			plan.Name != "openclaw" || !artifact.ValidOpenClawVersion(plan.Version) || plan.SourcePlan != "" ||
			plan.MaxBytes != artifact.DefaultArtifactMaxBytes {
			return fmt.Errorf("%w: fetch backend returned an incoherent npm plan", ErrInvalidArtifactFetchPreview)
		}
	case artifact.ArtifactSourceNode:
		if plan.PolicyVersion != artifact.NodeRuntimeFetchPolicyVersion ||
			plan.RegistryOrigin != artifact.ProductionNodeDistributionOrigin || plan.Name != "node-runtime" ||
			!artifact.ValidNodeRuntimeVersion(plan.Version) || plan.EnginesNode != "" ||
			len(plan.SourcePlan) == 0 || len(plan.SourcePlan) > artifact.MaxArtifactSourcePlanBytes ||
			plan.MaxBytes != artifact.DefaultNodeRuntimeBundleMaxBytes {
			return fmt.Errorf("%w: fetch backend returned an incoherent Node runtime plan", ErrInvalidArtifactFetchPreview)
		}
	case artifact.ArtifactSourceHermesImage:
		if plan.PolicyVersion != artifact.HermesImageFetchPolicyVersion ||
			plan.RegistryOrigin != artifact.ProductionHermesRegistryOrigin || plan.Name != "hermes-agent" ||
			!artifact.ValidHermesVersion(plan.Version) || plan.EnginesNode != "" ||
			len(plan.SourcePlan) == 0 || len(plan.SourcePlan) > artifact.MaxArtifactSourcePlanBytes ||
			!artifact.ValidHermesImageSourcePlan(plan.SourcePlan) ||
			plan.MaxBytes != artifact.DefaultHermesImageBundleMaxBytes {
			return fmt.Errorf("%w: fetch backend returned an incoherent Hermes image plan", ErrInvalidArtifactFetchPreview)
		}
	case artifact.ArtifactSourceClaudeCode:
		if plan.PolicyVersion != artifact.ClaudeCodeFetchPolicyVersion ||
			plan.RegistryOrigin != artifact.ProductionClaudeCodeOrigin || plan.Name != "claude-code" ||
			!artifact.ValidClaudeCodeVersion(plan.Version) || plan.EnginesNode != "" ||
			len(plan.SourcePlan) == 0 || len(plan.SourcePlan) > artifact.MaxArtifactSourcePlanBytes ||
			!artifact.ValidClaudeCodeSourcePlan(plan.SourcePlan) ||
			plan.MaxBytes != artifact.DefaultClaudeCodeBundleMaxBytes {
			return fmt.Errorf("%w: fetch backend returned an incoherent Claude Code plan", ErrInvalidArtifactFetchPreview)
		}
	case artifact.ArtifactSourceCodex:
		if plan.PolicyVersion != artifact.CodexFetchPolicyVersion ||
			plan.RegistryOrigin != artifact.ProductionCodexOrigin || plan.Name != "codex" ||
			!artifact.ValidCodexVersion(plan.Version) || plan.EnginesNode != "" ||
			len(plan.SourcePlan) == 0 || len(plan.SourcePlan) > artifact.MaxArtifactSourcePlanBytes ||
			!artifact.ValidCodexSourcePlan(plan.SourcePlan) ||
			plan.MaxBytes != artifact.DefaultCodexBundleMaxBytes {
			return fmt.Errorf("%w: fetch backend returned an incoherent Codex plan", ErrInvalidArtifactFetchPreview)
		}
	case artifact.ArtifactSourceGrok:
		if plan.PolicyVersion != artifact.GrokFetchPolicyVersion ||
			plan.RegistryOrigin != artifact.ProductionRegistryOrigin || plan.Name != "grok" ||
			!artifact.ValidGrokVersion(plan.Version) || plan.EnginesNode != "" ||
			len(plan.SourcePlan) == 0 || len(plan.SourcePlan) > artifact.MaxArtifactSourcePlanBytes ||
			!artifact.ValidGrokSourcePlan(plan.SourcePlan) ||
			plan.MaxBytes != artifact.DefaultGrokBundleMaxBytes {
			return fmt.Errorf("%w: fetch backend returned an incoherent Grok plan", ErrInvalidArtifactFetchPreview)
		}
	case artifact.ArtifactSourceBATServer:
		if plan.PolicyVersion != artifact.BATServerFetchPolicyVersion ||
			plan.RegistryOrigin != artifact.ProductionBATServerOrigin || plan.Name != "bat-server" ||
			!artifact.ValidBATServerVersion(plan.Version) || plan.EnginesNode != "" ||
			len(plan.SourcePlan) == 0 || len(plan.SourcePlan) > artifact.MaxArtifactSourcePlanBytes ||
			!artifact.ValidBATServerSourcePlan(plan.SourcePlan) ||
			plan.MaxBytes != artifact.DefaultBATServerBundleMaxBytes {
			return fmt.Errorf("%w: fetch backend returned an incoherent bat-server plan", ErrInvalidArtifactFetchPreview)
		}
	case artifact.ArtifactSourceAntigravity:
		if plan.PolicyVersion != artifact.AntigravityFetchPolicyVersion ||
			plan.RegistryOrigin != artifact.ProductionAntigravityManifestOrigin || plan.Name != "antigravity" ||
			!artifact.ValidAntigravityVersion(plan.Version) || plan.EnginesNode != "" ||
			len(plan.SourcePlan) == 0 || len(plan.SourcePlan) > artifact.MaxArtifactSourcePlanBytes ||
			!artifact.ValidAntigravitySourcePlan(plan.SourcePlan) ||
			plan.MaxBytes != artifact.DefaultAntigravityBundleMaxBytes {
			return fmt.Errorf("%w: fetch backend returned an incoherent Antigravity plan", ErrInvalidArtifactFetchPreview)
		}
	default:
		return fmt.Errorf("%w: fetch backend returned an unknown source kind", ErrInvalidArtifactFetchPreview)
	}
	return nil
}

func validArtifactFetchTarget(name, version string) bool {
	switch name {
	case "openclaw":
		return artifact.ValidOpenClawVersion(version)
	case "node-runtime":
		return artifact.ValidNodeRuntimeVersion(version)
	case "hermes-agent":
		return artifact.ValidHermesVersion(version)
	case "claude-code":
		return artifact.ValidClaudeCodeVersion(version)
	case "codex":
		return artifact.ValidCodexVersion(version)
	case "grok":
		return artifact.ValidGrokVersion(version)
	case "bat-server":
		return artifact.ValidBATServerVersion(version)
	case "antigravity":
		return artifact.ValidAntigravityVersion(version)
	default:
		return false
	}
}

func (s *Service) existingVerifiedArtifact(ctx context.Context, plan artifact.PreviewPlan) (string, bool, error) {
	entries, err := artifact.ScanCatalog(s.artifactsDir)
	if err != nil {
		return "", false, err
	}
	for _, entry := range entries {
		if entry.Record == nil || entry.Record.Name != plan.Name || entry.Record.Version != plan.Version ||
			entry.Record.TarballURL != plan.TarballURL || entry.Record.SHA512Integrity != plan.SHA512Integrity ||
			entry.Record.EnginesNode != plan.EnginesNode || entry.Record.Size > plan.MaxBytes {
			continue
		}
		inspected, err := artifact.InspectCatalogEntry(ctx, s.artifactsDir, entry.ID)
		if err != nil {
			return "", false, err
		}
		if inspected.Status == artifact.CatalogReady {
			return inspected.SHA256, true, nil
		}
	}
	return "", false, nil
}

func (s *Service) ApplyArtifactFetch(ctx context.Context, request ArtifactFetchApplyRequest) (ArtifactFetchApplyResult, error) {
	if s == nil || s.store == nil || s.artifactFetcher == nil || ctx == nil {
		return ArtifactFetchApplyResult{}, fmt.Errorf("%w: store, fetcher, and context are required", ErrInvalidArtifactFetchPreview)
	}
	digest := ArtifactFetchApplySemanticDigest(request)
	audit := auditFromActor(request.Actor)
	audit.Reason, audit.IdempotencyKey, audit.RequestDigest = request.Reason, request.IdempotencyKey, digest
	result, err := s.store.ApplyOperatorArtifactFetch(store.OperatorArtifactFetchRequest{
		Name: request.Name, Version: request.Version, PreviewDigest: request.PreviewDigest,
		Reason: request.Reason, IdempotencyKey: request.IdempotencyKey, RequestDigest: digest, Audit: audit,
	}, func() (store.ArtifactFetchPrepared, error) {
		// Typed confirmation is request-local evidence. Reject it durably before
		// touching the registry when it already contradicts the requested target.
		if request.ConfirmName != request.Name || request.ConfirmVersion != request.Version {
			return store.ArtifactFetchPrepared{}, &store.OperatorRequestError{
				Code: store.OperatorCodeArtifactFetchInvalid, Detail: store.OperatorCodeArtifactFetchInvalid,
			}
		}
		plan, prepareErr := s.artifactFetcher.PreviewPlan(ctx, request.Name, request.Version)
		if prepareErr != nil {
			if errors.Is(prepareErr, artifact.ErrInvalidFetchRequest) {
				return store.ArtifactFetchPrepared{}, &store.OperatorRequestError{
					Code: store.OperatorCodeArtifactFetchInvalid, Detail: store.OperatorCodeArtifactFetchInvalid,
				}
			}
			if errors.Is(prepareErr, artifact.ErrUpstreamVersionUnavailable) {
				return store.ArtifactFetchPrepared{}, &store.OperatorRequestError{
					Code: store.OperatorCodeArtifactFetchPreviewStale, Detail: store.OperatorCodeArtifactFetchPreviewStale,
				}
			}
			return store.ArtifactFetchPrepared{}, prepareErr
		}
		if validateArtifactFetchPlan(plan, ArtifactFetchPreviewRequest{Name: request.Name, Version: request.Version}) != nil {
			return store.ArtifactFetchPrepared{}, &store.OperatorRequestError{
				Code: store.OperatorCodeArtifactFetchInvalid, Detail: store.OperatorCodeArtifactFetchInvalid,
			}
		}
		return store.ArtifactFetchPrepared{
			Name: plan.Name, Version: plan.Version, SourceKind: plan.SourceKind, SourcePlan: plan.SourcePlan,
			RegistryOrigin: plan.RegistryOrigin,
			TarballURL:     plan.TarballURL, SHA512Integrity: plan.SHA512Integrity,
			EnginesNode: plan.EnginesNode, MaxBytes: plan.MaxBytes,
			CurrentPreviewDigest: plan.PreviewDigest,
		}, nil
	})
	if err != nil {
		s.recordArtifactFetchFallback(request, digest, result.Audited, err)
	}
	return result, err
}

// ArtifactFetchApplySemanticDigest binds all operator-authored meaning while
// excluding transport identity and the idempotency key itself.
func ArtifactFetchApplySemanticDigest(request ArtifactFetchApplyRequest) string {
	body := struct {
		Name           string `json:"name"`
		Version        string `json:"version"`
		PreviewDigest  string `json:"preview_digest"`
		ConfirmName    string `json:"confirm_name"`
		ConfirmVersion string `json:"confirm_version"`
		Reason         string `json:"reason"`
	}{request.Name, request.Version, request.PreviewDigest, request.ConfirmName, request.ConfirmVersion, request.Reason}
	raw, _ := json.Marshal(body)
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func (s *Service) ArtifactFetchOperation(operationID string) (store.ArtifactFetchOperation, error) {
	if s == nil || s.store == nil {
		return store.ArtifactFetchOperation{}, store.ErrArtifactFetchNotFound
	}
	return s.store.GetArtifactFetchOperation(operationID)
}

func (s *Service) ArtifactFetchOperations(request store.ArtifactFetchListRequest) (store.ArtifactFetchListResult, error) {
	if s == nil || s.store == nil {
		return store.ArtifactFetchListResult{}, store.ErrArtifactFetchInvalid
	}
	return s.store.ListArtifactFetchOperations(request)
}

func (s *Service) recordArtifactFetchFallback(request ArtifactFetchApplyRequest, digest string, audited bool, err error) {
	var rejection *store.OperatorRequestError
	if audited || (errors.As(err, &rejection) && rejection.Audited) {
		return
	}
	entry := auditFromActor(request.Actor)
	subject := "invalid artifact fetch request"
	if validDeploymentIdentifier(request.Name, 128) && validDeploymentIdentifier(request.Version, 128) {
		subject = request.Name + "@" + request.Version
	}
	entry.Action, entry.Subject, entry.Reason = store.AuditArtifactFetch, subject, request.Reason
	entry.IdempotencyKey, entry.RequestDigest, entry.OK = request.IdempotencyKey, digest, false
	entry.Detail = "artifact fetch request failed before enqueue"
	if errors.As(err, &rejection) && rejection.Replayed {
		entry.Detail = store.OperatorIdempotencyReplayPrefix + "原判決：artifact fetch 未再次入列"
	}
	if auditErr := s.store.RecordAudit(entry); auditErr != nil {
		log.Printf("operator artifact fetch audit write failed subject=%s: %v", entry.Subject, auditErr)
	}
}

func artifactFetchDigest(value string) bool {
	return strings.HasPrefix(value, "sha256:") && artifact.ValidSHA256Hex(strings.TrimPrefix(value, "sha256:"))
}
