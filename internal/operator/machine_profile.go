package operator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"sort"
	"strconv"
	"time"

	"github.com/teddashh/AI-Intune/internal/artifact"
	appcatalog "github.com/teddashh/AI-Intune/internal/catalog"
	"github.com/teddashh/AI-Intune/internal/store"
)

var ErrInvalidMachineProfilePublication = errors.New("operator: invalid machine profile publication")

type MachineProfilePublishRequest struct {
	Profile          appcatalog.MachineProfile `json:"profile"`
	ConfirmProfileID string                    `json:"-"`
	ConfirmRevision  int64                     `json:"-"`
	PreviewDigest    string                    `json:"-"`
	Reason           string                    `json:"reason"`
	IdempotencyKey   string                    `json:"-"`
	Actor            Actor                     `json:"-"`
}

type MachineProfileRecord struct {
	Profile     appcatalog.MachineProfile `json:"profile"`
	Digest      string                    `json:"profile_digest"`
	PublishedAt time.Time                 `json:"published_at"`
	PublishedBy string                    `json:"-"`
}

type MachineProfilePublishResult struct {
	Record           MachineProfileRecord `json:"record"`
	AlreadyPublished bool                 `json:"already_published"`
	Replayed         bool                 `json:"replayed"`
	Audited          bool                 `json:"-"`
}

// PublishMachineProfile verifies one complete executable graph before Store
// commits the immutable profile, receipt, and audit as a single decision.
func (s *Service) PublishMachineProfile(ctx context.Context,
	request MachineProfilePublishRequest,
) (MachineProfilePublishResult, error) {
	return s.publishMachineProfile(ctx, request, false)
}

func (s *Service) PublishReviewedMachineProfile(ctx context.Context,
	request MachineProfilePublishRequest,
) (MachineProfilePublishResult, error) {
	return s.publishMachineProfile(ctx, request, true)
}

func (s *Service) publishMachineProfile(ctx context.Context, request MachineProfilePublishRequest,
	reviewed bool,
) (MachineProfilePublishResult, error) {
	if s == nil || s.store == nil || ctx == nil {
		return MachineProfilePublishResult{}, ErrInvalidMachineProfilePublication
	}
	digest := MachineProfilePublishSemanticDigest(request)
	publishedBy, _ := deploymentCreatedBy(request.Actor)
	audit := auditFromActor(request.Actor)
	audit.Reason, audit.IdempotencyKey, audit.RequestDigest = request.Reason, request.IdempotencyKey, digest
	stored, err := s.store.ApplyOperatorMachineProfile(store.OperatorMachineProfileRequest{
		Profile: request.Profile, PublishedBy: publishedBy, Reason: request.Reason,
		IdempotencyKey: request.IdempotencyKey, RequestDigest: digest, Audit: audit,
	}, func(profile appcatalog.MachineProfile) error {
		if reviewed {
			if request.ConfirmProfileID != profile.ID || request.ConfirmRevision != profile.Revision {
				return machineProfileRejection(store.OperatorCodeMachineProfileConfirmationMismatch)
			}
			if request.PreviewDigest != machineProfilePreviewDigest(profile) {
				return machineProfileRejection(store.OperatorCodeMachineProfilePreviewStale)
			}
		}
		return s.verifyMachineProfileMaterial(ctx, profile)
	})
	if err != nil {
		s.recordMachineProfileFallback(request, digest, stored.Audited, err)
		return MachineProfilePublishResult{}, err
	}
	return MachineProfilePublishResult{
		Record: MachineProfileRecord{
			Profile: stored.Record.Profile, Digest: stored.Record.Digest,
			PublishedAt: stored.Record.PublishedAt, PublishedBy: stored.Record.PublishedBy,
		},
		AlreadyPublished: stored.AlreadyPublished, Replayed: stored.Replayed, Audited: stored.Audited,
	}, nil
}

func (s *Service) verifyMachineProfileMaterial(ctx context.Context, profile appcatalog.MachineProfile) error {
	records, err := s.store.CatalogManifests()
	if err != nil {
		return err
	}
	manifests := make([]appcatalog.Manifest, 0, len(records))
	targetSet := make(map[appcatalog.Platform]struct{})
	for _, record := range records {
		manifests = append(manifests, record.Manifest)
		for _, target := range record.Manifest.Platforms {
			targetSet[target] = struct{}{}
		}
	}
	index, err := appcatalog.New(manifests)
	if err != nil {
		return err
	}
	targets := make([]appcatalog.Platform, 0, len(targetSet))
	for target := range targetSet {
		targets = append(targets, target)
	}
	sort.Slice(targets, func(i, j int) bool {
		if targets[i].OS != targets[j].OS {
			return targets[i].OS < targets[j].OS
		}
		return targets[i].Arch < targets[j].Arch
	})
	for _, target := range targets {
		plan, resolveErr := index.Resolve(profile, target)
		if resolveErr != nil {
			continue
		}
		if err := s.verifyMachineProfilePlan(ctx, plan); err != nil {
			return err
		}
		return nil
	}
	return machineProfileRejection(store.OperatorCodeMachineProfileUnresolvable)
}

func (s *Service) verifyMachineProfilePlan(ctx context.Context, plan appcatalog.Plan) error {
	verified := make(map[string]struct{}, len(plan.Packages))
	for _, planned := range plan.Packages {
		manifest := planned.Manifest
		if err := validateCatalogAdapterContract(manifest); err != nil {
			return err
		}
		if _, ok := verified[manifest.Artifact.SHA256]; ok {
			continue
		}
		entry, err := artifact.InspectCatalogEntry(ctx, s.artifactsDir, manifest.Artifact.SHA256)
		if errors.Is(err, artifact.ErrCatalogEntryNotFound) {
			return machineProfileRejection(store.OperatorCodeCatalogArtifactUnavailable)
		}
		if err != nil {
			return err
		}
		if entry.Status != artifact.CatalogReady || entry.Record == nil {
			return machineProfileRejection(store.OperatorCodeCatalogArtifactUnavailable)
		}
		record := entry.Record
		if entry.SHA256 != manifest.Artifact.SHA256 || record.SHA256 != manifest.Artifact.SHA256 ||
			record.Name != manifest.ID || record.Version != manifest.Version || record.Size != manifest.Artifact.Size {
			return machineProfileRejection(store.OperatorCodeCatalogArtifactMismatch)
		}
		verified[manifest.Artifact.SHA256] = struct{}{}
	}
	return nil
}

func machineProfileRejection(code string) *store.OperatorRequestError {
	return &store.OperatorRequestError{Code: code, Detail: code}
}

func MachineProfilePublishSemanticDigest(request MachineProfilePublishRequest) string {
	profile := request.Profile
	if raw, err := json.Marshal(profile); err == nil {
		if canonical, err := appcatalog.ParseProfile(raw); err == nil {
			profile = canonical
		}
	}
	body := struct {
		Profile          appcatalog.MachineProfile `json:"profile"`
		ConfirmProfileID string                    `json:"confirm_profile_id"`
		ConfirmRevision  int64                     `json:"confirm_revision"`
		PreviewDigest    string                    `json:"preview_digest"`
		Reason           string                    `json:"reason"`
	}{Profile: profile, ConfirmProfileID: request.ConfirmProfileID,
		ConfirmRevision: request.ConfirmRevision, PreviewDigest: request.PreviewDigest, Reason: request.Reason}
	raw, _ := json.Marshal(body)
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func (s *Service) recordMachineProfileFallback(request MachineProfilePublishRequest,
	digest string, audited bool, err error,
) {
	var rejection *store.OperatorRequestError
	if audited || (errors.As(err, &rejection) && rejection.Audited) {
		return
	}
	entry := auditFromActor(request.Actor)
	entry.Action = store.AuditMachineProfile
	entry.Subject = "machine profile request"
	if appcatalog.ValidateProfile(request.Profile) == nil {
		entry.Subject = machineProfileSubject(request.Profile)
	}
	if validDeploymentText(request.Reason, 500, false) {
		entry.Reason = request.Reason
	}
	entry.IdempotencyKey, entry.RequestDigest, entry.OK = request.IdempotencyKey, digest, false
	entry.Detail = "machine profile verification failed"
	if auditErr := s.store.RecordAudit(entry); auditErr != nil {
		log.Printf("operator machine profile audit write failed subject=%s: %v", entry.Subject, auditErr)
	}
}

func machineProfileSubject(profile appcatalog.MachineProfile) string {
	return profile.ID + "@" + strconv.FormatInt(profile.Revision, 10)
}
