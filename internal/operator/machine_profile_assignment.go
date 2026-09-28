package operator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"

	"github.com/teddashh/AI-Intune/internal/agentadapter"
	"github.com/teddashh/AI-Intune/internal/artifact"
	appcatalog "github.com/teddashh/AI-Intune/internal/catalog"
	"github.com/teddashh/AI-Intune/internal/store"
)

var ErrInvalidMachineProfileAssignment = errors.New("operator: invalid machine profile assignment")

type MachineProfileAssignmentPreviewRequest struct {
	MachineID       string `json:"machine_id"`
	ProfileID       string `json:"profile_id"`
	ProfileRevision int64  `json:"profile_revision"`
}

type MachineProfileAssignmentPreviewResult = store.OperatorMachineProfileAssignmentPreviewResult

type MachineProfileAssignmentRequest struct {
	MachineID          string `json:"machine_id"`
	ProfileID          string `json:"profile_id"`
	ProfileRevision    int64  `json:"profile_revision"`
	ConfirmDisplayName string `json:"confirm_display_name"`
	PreviewDigest      string `json:"preview_digest"`
	Reason             string `json:"reason"`
	IdempotencyKey     string `json:"-"`
	Actor              Actor  `json:"-"`
}

type MachineProfileAssignmentResult = store.OperatorMachineProfileAssignmentResult

func (s *Service) PreviewMachineProfileAssignment(ctx context.Context,
	request MachineProfileAssignmentPreviewRequest,
) (MachineProfileAssignmentPreviewResult, error) {
	if s == nil || s.store == nil || ctx == nil {
		return MachineProfileAssignmentPreviewResult{}, ErrInvalidMachineProfileAssignment
	}
	prepared, err := s.prepareMachineProfileAssignment(ctx, request.MachineID,
		request.ProfileID, request.ProfileRevision)
	if err != nil {
		return MachineProfileAssignmentPreviewResult{}, err
	}
	return s.store.PreviewOperatorMachineProfileAssignment(request.MachineID,
		request.ProfileID, request.ProfileRevision, prepared)
}

func (s *Service) AssignMachineProfile(ctx context.Context,
	request MachineProfileAssignmentRequest,
) (MachineProfileAssignmentResult, error) {
	if s == nil || s.store == nil || ctx == nil {
		return MachineProfileAssignmentResult{}, ErrInvalidMachineProfileAssignment
	}
	digest := MachineProfileAssignmentSemanticDigest(request)
	assignedBy, _ := deploymentCreatedBy(request.Actor)
	audit := auditFromActor(request.Actor)
	audit.Reason, audit.IdempotencyKey, audit.RequestDigest = request.Reason, request.IdempotencyKey, digest
	result, err := s.store.ApplyOperatorMachineProfileAssignment(store.OperatorMachineProfileAssignmentRequest{
		MachineID: request.MachineID, ProfileID: request.ProfileID, ProfileRevision: request.ProfileRevision,
		ConfirmDisplayName: request.ConfirmDisplayName, PreviewDigest: request.PreviewDigest,
		Reason: request.Reason, IdempotencyKey: request.IdempotencyKey, RequestDigest: digest,
		AssignedBy: assignedBy, Audit: audit,
	}, func() (store.OperatorMachineProfileAssignmentPrepared, error) {
		return s.prepareMachineProfileAssignment(ctx, request.MachineID, request.ProfileID, request.ProfileRevision)
	})
	if err != nil {
		s.recordMachineProfileAssignmentFallback(request, digest, result.Audited, err)
	}
	return result, err
}

func (s *Service) prepareMachineProfileAssignment(ctx context.Context, machineID, profileID string,
	profileRevision int64,
) (store.OperatorMachineProfileAssignmentPrepared, error) {
	osDisplay, unameArch, err := s.store.MachinePlatformIdentity(machineID)
	if errors.Is(err, store.ErrNotFound) {
		return store.OperatorMachineProfileAssignmentPrepared{}, &store.OperatorRequestError{
			Code: store.OperatorCodeMachineNotFound, Detail: "找不到這台機器",
		}
	}
	if err != nil {
		return store.OperatorMachineProfileAssignmentPrepared{}, err
	}
	target, err := catalogPlatformFromMachine(store.Machine{OS: osDisplay, Arch: unameArch})
	if err != nil {
		var resolutionErr *appcatalog.ResolutionError
		if errors.As(err, &resolutionErr) && resolutionErr.Code == appcatalog.CodeUnsupportedPlatform {
			return store.OperatorMachineProfileAssignmentPrepared{}, &store.OperatorRequestError{
				Code: store.OperatorCodeMachinePlatformUnsupported, Detail: "agent 沒有支援這台機器平台的 adapter 或可執行套件；不可建立 profile job",
			}
		}
		return store.OperatorMachineProfileAssignmentPrepared{}, &store.OperatorRequestError{
			Code: store.OperatorCodeMachinePlatformUnknown, Detail: "機器的作業系統或架構資料無法辨識；請確認 agent 回報",
		}
	}
	if !agentadapter.HasExecutorForPlatform(target) {
		return store.OperatorMachineProfileAssignmentPrepared{}, &store.OperatorRequestError{
			Code: store.OperatorCodeMachinePlatformUnsupported, Detail: "agent 沒有支援這台機器平台的 adapter 或可執行套件；不可建立 profile job",
		}
	}
	profile, err := s.store.MachineProfile(profileID, profileRevision)
	if errors.Is(err, store.ErrNotFound) {
		return store.OperatorMachineProfileAssignmentPrepared{}, &store.OperatorRequestError{
			Code: store.OperatorCodeMachineProfileNotFound, Detail: "找不到指定的 profile 版本；請重新選擇 profile",
		}
	}
	if err != nil {
		return store.OperatorMachineProfileAssignmentPrepared{}, err
	}
	plan, err := s.store.ResolveMachineProfile(profileID, profileRevision, target)
	if err != nil {
		return store.OperatorMachineProfileAssignmentPrepared{}, &store.OperatorRequestError{
			Code: store.OperatorCodeMachineProfileUnresolvable, Detail: "profile 無法在這台機器的平台解析",
		}
	}
	if err := s.verifyMachineProfilePlan(ctx, plan); err != nil {
		return store.OperatorMachineProfileAssignmentPrepared{}, err
	}
	records, err := s.store.CatalogManifests()
	if err != nil {
		return store.OperatorMachineProfileAssignmentPrepared{}, err
	}
	manifestDigests := make(map[string]string, len(records))
	for _, record := range records {
		manifestDigests[assignmentPackageIdentity(record.Manifest.ID, record.Manifest.Version)] = record.Digest
	}
	prepared := store.OperatorMachineProfileAssignmentPrepared{
		ProfileDigest: profile.Digest,
		Target:        target,
		Packages:      make([]store.OperatorMachineProfileAssignmentPreparedPackage, 0, len(plan.Packages)),
	}
	for _, planned := range plan.Packages {
		manifest := planned.Manifest
		contract, ok := agentadapter.Lookup(manifest.Adapter)
		if !ok || !agentadapter.SupportsManifest(manifest) {
			return store.OperatorMachineProfileAssignmentPrepared{}, &store.OperatorRequestError{
				Code: store.OperatorCodeCatalogAdapterUnsupported, Detail: "profile 包含 agent 不支援的 adapter",
			}
		}
		candidate := store.OperatorMachineProfileAssignmentPreparedPackage{
			PackageID: manifest.ID, PackageVersion: manifest.Version,
			ManifestDigest: manifestDigests[assignmentPackageIdentity(manifest.ID, manifest.Version)],
			ResourceKind:   contract.ExecutorKind, ResourceID: manifest.ID,
			ArtifactDigest:   "sha256:" + manifest.Artifact.SHA256,
			ExecutionTimeout: store.OperatorMachineProfileAssignmentDefaultTimeout,
			Direct:           planned.Direct,
		}
		switch contract.ExecutorKind {
		case agentadapter.ExecutorKindOpenClaw:
			material, resolveErr := artifact.ResolveOpenClawMaterialContext(ctx, s.artifactsDir,
				manifest.Version, manifest.Artifact.SHA256)
			if resolveErr != nil {
				return store.OperatorMachineProfileAssignmentPrepared{}, &store.OperatorRequestError{
					Code: store.OperatorCodeCatalogArtifactUnavailable, Detail: "profile artifact bytes 不可用",
				}
			}
			if material.Artifact.Name != manifest.ID || material.Version != manifest.Version ||
				material.Artifact.Size != manifest.Artifact.Size || material.Digest != candidate.ArtifactDigest {
				return store.OperatorMachineProfileAssignmentPrepared{}, &store.OperatorRequestError{
					Code: store.OperatorCodeCatalogArtifactMismatch, Detail: "profile artifact 與 manifest identity 不一致",
				}
			}
			candidate.Spec = material.Spec
		case agentadapter.ExecutorKindNodeRuntime:
			material, resolveErr := artifact.ResolveNodeRuntimeMaterialContext(ctx, s.artifactsDir,
				manifest.Version, manifest.Artifact.SHA256, target.OS, target.Arch)
			if resolveErr != nil {
				return store.OperatorMachineProfileAssignmentPrepared{}, &store.OperatorRequestError{
					Code: store.OperatorCodeCatalogArtifactUnavailable, Detail: "profile artifact bytes 不可用",
				}
			}
			if material.Artifact.Name != manifest.ID || material.Version != manifest.Version ||
				material.Artifact.Size != manifest.Artifact.Size || material.Digest != candidate.ArtifactDigest ||
				material.TargetOS != target.OS || material.TargetArch != target.Arch {
				return store.OperatorMachineProfileAssignmentPrepared{}, &store.OperatorRequestError{
					Code: store.OperatorCodeCatalogArtifactMismatch, Detail: "profile artifact 與 manifest identity 不一致",
				}
			}
			candidate.Spec = material.Spec
		case agentadapter.ExecutorKindHermes:
			material, resolveErr := artifact.ResolveHermesMaterialContext(ctx, s.artifactsDir,
				manifest.Version, manifest.Artifact.SHA256, target.OS, target.Arch)
			if resolveErr != nil {
				return store.OperatorMachineProfileAssignmentPrepared{}, &store.OperatorRequestError{
					Code: store.OperatorCodeCatalogArtifactUnavailable, Detail: "profile artifact bytes 不可用",
				}
			}
			if material.Artifact.Name != manifest.ID || material.Version != manifest.Version ||
				material.Artifact.Size != manifest.Artifact.Size || material.Digest != candidate.ArtifactDigest ||
				material.TargetOS != target.OS || material.TargetArch != target.Arch {
				return store.OperatorMachineProfileAssignmentPrepared{}, &store.OperatorRequestError{
					Code: store.OperatorCodeCatalogArtifactMismatch, Detail: "profile artifact 與 manifest identity 不一致",
				}
			}
			candidate.Spec = material.Spec
		default:
			return store.OperatorMachineProfileAssignmentPrepared{}, &store.OperatorRequestError{
				Code: store.OperatorCodeCatalogAdapterUnsupported, Detail: "profile 包含 agent 不支援的 adapter",
			}
		}
		prepared.Packages = append(prepared.Packages, candidate)
	}
	return prepared, nil
}

// catalogPlatformFromMachine converts the probe's OS display name and uname -m
// into the catalog target vocabulary. Recognized macOS identities remain
// darwin targets even when a selected profile has no matching packages.
func catalogPlatformFromMachine(machine store.Machine) (appcatalog.Platform, error) {
	return appcatalog.PlatformFromProbeIdentity(machine.OS, machine.Arch)
}

func assignmentPackageIdentity(packageID, version string) string { return packageID + "@" + version }

func MachineProfileAssignmentSemanticDigest(request MachineProfileAssignmentRequest) string {
	body := struct {
		MachineID          string `json:"machine_id"`
		ProfileID          string `json:"profile_id"`
		ProfileRevision    int64  `json:"profile_revision"`
		ConfirmDisplayName string `json:"confirm_display_name"`
		PreviewDigest      string `json:"preview_digest"`
		Reason             string `json:"reason"`
	}{request.MachineID, request.ProfileID, request.ProfileRevision, request.ConfirmDisplayName,
		request.PreviewDigest, request.Reason}
	raw, _ := json.Marshal(body)
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func (s *Service) recordMachineProfileAssignmentFallback(request MachineProfileAssignmentRequest,
	digest string, audited bool, err error,
) {
	var rejection *store.OperatorRequestError
	if audited || (errors.As(err, &rejection) && rejection.Audited) {
		return
	}
	entry := auditFromActor(request.Actor)
	entry.Action = store.AuditMachineProfileAssign
	entry.MachineID, entry.Subject = request.MachineID, request.MachineID
	if validDeploymentText(request.Reason, 500, false) {
		entry.Reason = request.Reason
	}
	entry.IdempotencyKey, entry.RequestDigest, entry.OK = request.IdempotencyKey, digest, false
	entry.Detail = "machine profile assignment verification failed"
	if auditErr := s.store.RecordAudit(entry); auditErr != nil {
		log.Printf("operator machine profile assignment audit 寫入失敗 subject=%s: %v", entry.Subject, auditErr)
	}
}
