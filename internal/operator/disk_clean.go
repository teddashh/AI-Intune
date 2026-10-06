package operator

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/teddashh/AI-Intune/internal/maintenance"
	"github.com/teddashh/AI-Intune/internal/store"
)

// Disk-clean writes compute the request digest here and pass it to the store.
// The store does not rebuild it. A reused idempotency key with a different
// digest conflicts, the same way a setting-policy publish does.

// DiskCleanProfilePublishRequest publishes one maintenance/disk-clean revision.
type DiskCleanProfilePublishRequest struct {
	ScopeType        string
	ScopeID          string
	Profile          maintenance.Profile
	ExpectedRevision *int64
	PreviewDigest    string
	ConfirmScopeID   string
	Reason           string
	IdempotencyKey   string
	Actor            Actor
}

// DiskCleanTargetApplyRequest is a dry-run or a canary apply.
type DiskCleanTargetApplyRequest struct {
	ScopeType       string
	ScopeID         string
	Revision        int64
	MachineIDs      []string
	CanaryMachineID string
	PreviewDigest   string
	ConfirmScopeID  string
	Reason          string
	IdempotencyKey  string
	Actor           Actor
}

// DiskCleanControlApplyRequest continues or abandons one rollout.
type DiskCleanControlApplyRequest struct {
	RolloutID               string
	ExpectedControlRevision int64
	ExpectedOpenedBatch     int
	PreviewDigest           string
	ConfirmRolloutID        string
	Reason                  string
	IdempotencyKey          string
	Actor                   Actor
}

// DiskCleanSummaries is the read-only board. now is the Hub clock.
func (s *Service) DiskCleanSummaries(now time.Time) ([]store.DiskCleanSummaryView, error) {
	items, err := s.store.ListDiskCleanSummaries(now)
	if err != nil {
		return nil, err
	}
	if items == nil {
		items = []store.DiskCleanSummaryView{}
	}
	return items, nil
}

// DiskCleanSummary is one machine, including an enrolled machine with no assignment.
func (s *Service) DiskCleanSummary(machineID string, now time.Time) (store.DiskCleanSummaryView, error) {
	return s.store.DiskCleanSummary(machineID, now)
}

// PreviewDiskCleanProfile renders the conf and returns the confirmation digest.
func (s *Service) PreviewDiskCleanProfile(scopeType, scopeID string, profile maintenance.Profile) (store.DiskCleanProfilePreview, error) {
	return s.store.PreviewDiskCleanProfile(scopeType, scopeID, profile)
}

// PublishDiskCleanProfile applies one profile revision.
func (s *Service) PublishDiskCleanProfile(req DiskCleanProfilePublishRequest) (store.DiskCleanProfileResult, error) {
	digest := diskCleanProfileApplyDigest(req)
	result, err := s.store.ApplyDiskCleanProfile(store.DiskCleanProfileRequest{
		ScopeType: req.ScopeType, ScopeID: req.ScopeID, Profile: req.Profile,
		ExpectedRevision: req.ExpectedRevision, PreviewDigest: req.PreviewDigest,
		ConfirmScopeID: req.ConfirmScopeID, Reason: req.Reason,
		PublishedBy:    settingActorLabel(req.Actor),
		IdempotencyKey: req.IdempotencyKey, RequestDigest: digest,
		Audit: auditFromActor(req.Actor),
	})
	s.recordSettingFallback(req.Actor, store.AuditMaintenanceProfile, req.ScopeType+":"+req.ScopeID,
		req.Reason, req.IdempotencyKey, digest, result.Audited, result.Replayed,
		"沒有再次發佈新的 revision", err)
	return result, err
}

// PreviewDiskCleanDryRun returns the dry-run digest and any blockers.
func (s *Service) PreviewDiskCleanDryRun(scopeType, scopeID string, revision int64, machineIDs []string) (store.DiskCleanDryRunPreview, error) {
	return s.store.PreviewDiskCleanDryRun(store.DiskCleanTargetRequest{
		ScopeType: scopeType, ScopeID: scopeID, Revision: revision, MachineIDs: machineIDs,
	})
}

// ApplyDiskCleanDryRun creates one reversible job per machine.
func (s *Service) ApplyDiskCleanDryRun(req DiskCleanTargetApplyRequest) (store.DiskCleanDryRunResult, error) {
	digest := diskCleanTargetApplyDigest("disk-clean-dry-run-apply:v1", req)
	result, err := s.store.ApplyDiskCleanDryRun(diskCleanTargetStoreRequest(req, digest))
	s.recordSettingFallback(req.Actor, store.AuditMaintenanceDryRun,
		diskCleanTargetSubject(req), req.Reason, req.IdempotencyKey, digest,
		result.Audited, result.Replayed, "沒有再次建立 dry-run 工作單", err)
	return result, err
}

// PreviewDiskCleanCanary returns the canary digest. It does not open a job.
func (s *Service) PreviewDiskCleanCanary(scopeType, scopeID string, revision int64, canary string, machineIDs []string) (store.DiskCleanCanaryPreview, error) {
	return s.store.PreviewDiskCleanCanary(store.DiskCleanTargetRequest{
		ScopeType: scopeType, ScopeID: scopeID, Revision: revision,
		MachineIDs: machineIDs, CanaryMachineID: canary,
	})
}

// ApplyDiskCleanCanary opens batch 1 for exactly one machine.
func (s *Service) ApplyDiskCleanCanary(req DiskCleanTargetApplyRequest) (store.DiskCleanCanaryResult, error) {
	digest := diskCleanTargetApplyDigest("disk-clean-canary-apply:v1", req)
	result, err := s.store.ApplyDiskCleanCanary(diskCleanTargetStoreRequest(req, digest))
	s.recordSettingFallback(req.Actor, store.AuditMaintenanceCanary,
		diskCleanTargetSubject(req), req.Reason, req.IdempotencyKey, digest,
		result.Audited, result.Replayed, "沒有再次開啟 canary", err)
	return result, err
}

// PreviewDiskCleanContinue reconciles, then digests the state the operator would confirm.
func (s *Service) PreviewDiskCleanContinue(rolloutID string) (store.DiskCleanControlPreview, error) {
	return s.store.PreviewDiskCleanContinue(rolloutID)
}

// PreviewDiskCleanAbandon reconciles, then digests the state the operator would confirm.
func (s *Service) PreviewDiskCleanAbandon(rolloutID string) (store.DiskCleanControlPreview, error) {
	return s.store.PreviewDiskCleanAbandon(rolloutID)
}

// ApplyDiskCleanContinue opens the rest of a paused rollout, or finishes when there is no rest.
func (s *Service) ApplyDiskCleanContinue(req DiskCleanControlApplyRequest) (store.DiskCleanRolloutResult, error) {
	digest := diskCleanControlApplyDigest("disk-clean-continue-apply:v1", req)
	result, err := s.store.ApplyDiskCleanContinue(diskCleanControlStoreRequest(req, digest))
	s.recordSettingFallback(req.Actor, store.AuditMaintenanceContinue, req.RolloutID,
		req.Reason, req.IdempotencyKey, digest, result.Audited, result.Replayed,
		"沒有再次繼續 rollout", err)
	return result, err
}

// ApplyDiskCleanAbandon stops a rollout without opening more jobs.
func (s *Service) ApplyDiskCleanAbandon(req DiskCleanControlApplyRequest) (store.DiskCleanRolloutResult, error) {
	digest := diskCleanControlApplyDigest("disk-clean-abandon-apply:v1", req)
	result, err := s.store.ApplyDiskCleanAbandon(diskCleanControlStoreRequest(req, digest))
	s.recordSettingFallback(req.Actor, store.AuditMaintenanceAbandon, req.RolloutID,
		req.Reason, req.IdempotencyKey, digest, result.Audited, result.Replayed,
		"沒有再次放棄 rollout", err)
	return result, err
}

func diskCleanTargetStoreRequest(req DiskCleanTargetApplyRequest, digest string) store.DiskCleanTargetRequest {
	return store.DiskCleanTargetRequest{
		ScopeType: req.ScopeType, ScopeID: req.ScopeID, Revision: req.Revision,
		MachineIDs: req.MachineIDs, CanaryMachineID: req.CanaryMachineID,
		PreviewDigest: req.PreviewDigest, ConfirmScopeID: req.ConfirmScopeID,
		Reason: req.Reason, IdempotencyKey: req.IdempotencyKey, RequestDigest: digest,
		Audit: auditFromActor(req.Actor),
	}
}

func diskCleanControlStoreRequest(req DiskCleanControlApplyRequest, digest string) store.DiskCleanControlRequest {
	return store.DiskCleanControlRequest{
		RolloutID: req.RolloutID, ExpectedControlRevision: req.ExpectedControlRevision,
		ExpectedOpenedBatch: req.ExpectedOpenedBatch, PreviewDigest: req.PreviewDigest,
		ConfirmRolloutID: req.ConfirmRolloutID, Reason: req.Reason,
		IdempotencyKey: req.IdempotencyKey, RequestDigest: digest,
		Audit: auditFromActor(req.Actor),
	}
}

func diskCleanTargetSubject(req DiskCleanTargetApplyRequest) string {
	return req.ScopeType + ":" + req.ScopeID + "@" + strconv.FormatInt(req.Revision, 10)
}

func diskCleanProfileApplyDigest(req DiskCleanProfilePublishRequest) string {
	raw, err := req.Profile.Canonical()
	if err != nil {
		encoded, mErr := json.Marshal(req.Profile)
		if mErr != nil {
			raw = []byte("invalid")
		} else {
			raw = encoded
		}
	}
	expected := "absent"
	if req.ExpectedRevision != nil {
		expected = strconv.FormatInt(*req.ExpectedRevision, 10)
	}
	return sha256Digest("disk-clean-profile-apply:v1", req.ScopeType, req.ScopeID, expected,
		string(raw), req.ConfirmScopeID, req.Reason)
}

func diskCleanTargetApplyDigest(label string, req DiskCleanTargetApplyRequest) string {
	return sha256Digest(label, req.ScopeType, req.ScopeID, strconv.FormatInt(req.Revision, 10),
		req.CanaryMachineID, sortedJoin(req.MachineIDs), req.ConfirmScopeID, req.Reason)
}

func diskCleanControlApplyDigest(label string, req DiskCleanControlApplyRequest) string {
	return sha256Digest(label, req.RolloutID, strconv.FormatInt(req.ExpectedControlRevision, 10),
		strconv.Itoa(req.ExpectedOpenedBatch), req.ConfirmRolloutID, req.Reason)
}

func sortedJoin(ids []string) string {
	cp := append([]string(nil), ids...)
	sort.Strings(cp)
	return strings.Join(cp, ",")
}

func sha256Digest(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\n")))
	return "sha256:" + hex.EncodeToString(sum[:])
}
