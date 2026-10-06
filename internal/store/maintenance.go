package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/teddashh/AI-Intune/internal/compliance"
	"github.com/teddashh/AI-Intune/internal/deploy"
	"github.com/teddashh/AI-Intune/internal/maintenance"
	"github.com/teddashh/AI-Intune/internal/model"
)

// Disk-clean is a maintenance profile, not an artifact deployment.
// CreateDeployment always mints a new desired revision, snapshots every
// non-retired member of a channel, and is shaped around an artifact digest.
// This ledger reuses desired_state, createJobTx, policy idempotency, and
// pause-after-canary: one canary job, then an explicit continue for the rest.

const diskCleanMaxMachines = 64

const (
	diskCleanStateCanary    = "canary"
	diskCleanStatePaused    = "paused"
	diskCleanStateBlocked   = "blocked"
	diskCleanStateExpanding = "expanding"
	diskCleanStateFinished  = "finished"
	diskCleanStateAbandoned = "abandoned"
)

// DiskCleanProfilePreview is the read-only confirmation of one publish.
// It does not promise the next revision number: the resource counter is
// shared by every scope, so another scope can move it before apply.
type DiskCleanProfilePreview struct {
	ScopeType       string `json:"scope_type"`
	ScopeID         string `json:"scope_id"`
	CurrentRevision int64  `json:"current_revision"`
	ConfigDigest    string `json:"config_digest"`
	PreviewDigest   string `json:"preview_digest"`
	Conf            string `json:"conf"`
}

// DiskCleanProfileRequest publishes one profile revision.
type DiskCleanProfileRequest struct {
	ScopeType        string
	ScopeID          string
	Profile          maintenance.Profile
	ExpectedRevision *int64
	PreviewDigest    string
	ConfirmScopeID   string
	Reason           string
	PublishedBy      string
	IdempotencyKey   string
	RequestDigest    string
	Audit            AuditEntry
}

// DiskCleanProfileResult is the published revision. Unchanged means the
// rendered conf matched the scope's current revision, so no new number was minted.
type DiskCleanProfileResult struct {
	DesiredID    string    `json:"desired_id"`
	ScopeType    string    `json:"scope_type"`
	ScopeID      string    `json:"scope_id"`
	Revision     int64     `json:"revision"`
	ConfigDigest string    `json:"config_digest"`
	CreatedAt    time.Time `json:"created_at"`
	Unchanged    bool      `json:"unchanged"`
	Replayed     bool      `json:"replayed"`
	Audited      bool      `json:"-"`
}

// DiskCleanTargetRequest is a dry-run or a canary. CanaryMachineID is set
// only for the canary, and it must be one of MachineIDs.
type DiskCleanTargetRequest struct {
	ScopeType       string
	ScopeID         string
	Revision        int64
	MachineIDs      []string
	CanaryMachineID string
	PreviewDigest   string
	ConfirmScopeID  string
	Reason          string
	IdempotencyKey  string
	RequestDigest   string
	Audit           AuditEntry
}

// DiskCleanBlocker is a fleet fact that is not part of the preview digest.
// A capability change between preview and apply is a conflict, not a stale preview.
type DiskCleanBlocker struct {
	Code   string `json:"code"`
	Detail string `json:"detail"`
}

// DiskCleanDryRunPreview is the read-only dry-run plan.
type DiskCleanDryRunPreview struct {
	ScopeType     string             `json:"scope_type"`
	ScopeID       string             `json:"scope_id"`
	Revision      int64              `json:"revision"`
	ConfigDigest  string             `json:"config_digest"`
	MachineIDs    []string           `json:"machine_ids"`
	PreviewDigest string             `json:"preview_digest"`
	Blockers      []DiskCleanBlocker `json:"blockers"`
}

// DiskCleanCanaryPreview is the read-only canary plan.
type DiskCleanCanaryPreview struct {
	DiskCleanDryRunPreview
	CanaryMachineID string `json:"canary_machine_id"`
}

// DiskCleanDryRunResult is one dry-run job per target machine.
type DiskCleanDryRunResult struct {
	DesiredID    string   `json:"desired_id"`
	Revision     int64    `json:"revision"`
	ConfigDigest string   `json:"config_digest"`
	MachineIDs   []string `json:"machine_ids"`
	JobIDs       []string `json:"job_ids"`
	Replayed     bool     `json:"replayed"`
	Audited      bool     `json:"-"`
}

// DiskCleanCanaryResult opens batch 1 only. The rest stay without a job
// until continue.
type DiskCleanCanaryResult struct {
	RolloutID       string   `json:"rollout_id"`
	DesiredID       string   `json:"desired_id"`
	Revision        int64    `json:"revision"`
	ConfigDigest    string   `json:"config_digest"`
	State           string   `json:"state"`
	ControlRevision int64    `json:"control_revision"`
	OpenedBatch     int      `json:"opened_batch"`
	CanaryMachineID string   `json:"canary_machine_id"`
	CanaryJobID     string   `json:"canary_job_id"`
	MachineIDs      []string `json:"machine_ids"`
	Replayed        bool     `json:"replayed"`
	Audited         bool     `json:"-"`
}

// DiskCleanControlRequest continues or abandons one rollout.
type DiskCleanControlRequest struct {
	RolloutID               string
	ExpectedControlRevision int64
	ExpectedOpenedBatch     int
	PreviewDigest           string
	ConfirmRolloutID        string
	Reason                  string
	IdempotencyKey          string
	RequestDigest           string
	Audit                   AuditEntry
}

// DiskCleanControlPreview is the read-only continue or abandon confirmation.
// State is part of the digest, so a reconcile between preview and apply is stale.
type DiskCleanControlPreview struct {
	RolloutID       string `json:"rollout_id"`
	State           string `json:"state"`
	ControlRevision int64  `json:"control_revision"`
	OpenedBatch     int    `json:"opened_batch"`
	PreviewDigest   string `json:"preview_digest"`
}

// DiskCleanRolloutResult is continue or abandon.
type DiskCleanRolloutResult struct {
	RolloutID       string   `json:"rollout_id"`
	State           string   `json:"state"`
	ControlRevision int64    `json:"control_revision"`
	OpenedBatch     int      `json:"opened_batch"`
	JobIDs          []string `json:"job_ids,omitempty"`
	Replayed        bool     `json:"replayed"`
	Audited         bool     `json:"-"`
}

// DiskCleanSummaryView is one machine's latest disk-clean evidence plus the
// Hub verdict. Summary.Root is the host timer's root object, read only.
type DiskCleanSummaryView struct {
	MachineID      string                     `json:"machine_id"`
	DisplayName    string                     `json:"display_name"`
	Assigned       bool                       `json:"assigned"`
	ExpectedDigest string                     `json:"expected_digest,omitempty"`
	Revision       int64                      `json:"revision,omitempty"`
	ReceivedAt     *time.Time                 `json:"received_at,omitempty"`
	ConfigDigest   string                     `json:"config_digest,omitempty"`
	DigestMatches  bool                       `json:"digest_matches"`
	Mode           string                     `json:"mode,omitempty"`
	Attention      string                     `json:"attention,omitempty"`
	Stale          bool                       `json:"stale"`
	Verdict        string                     `json:"verdict"`
	Reasons        []string                   `json:"reasons,omitempty"`
	Disk           maintenance.DiskEvaluation `json:"disk"`
	Summary        *maintenance.Summary       `json:"summary,omitempty"`
}

// DiskCleanAlertSend is one condition the notify path should deliver.
type DiskCleanAlertSend struct {
	Kind        string
	Body        string
	MachineID   string
	Condition   string
	Fingerprint string
}

type queryRower interface {
	QueryRow(query string, args ...any) *sql.Row
}

// DiskCleanProfilePreviewDigest binds the scope, the revision the operator
// saw, and the canonical profile. It does not include a promised next number.
func DiskCleanProfilePreviewDigest(scopeType, scopeID string, expected int64, canonical string) string {
	return digestOf(fmt.Sprintf("disk-clean-profile:v1:%s:%s:%d:%s", scopeType, scopeID, expected, canonical))
}

// DiskCleanDryRunPreviewDigest binds the scope, revision, and sorted machines.
func DiskCleanDryRunPreviewDigest(scopeType, scopeID string, revision int64, machineIDs []string) string {
	return digestOf(fmt.Sprintf("disk-clean-dry-run:v1:%s:%s:%d:%s", scopeType, scopeID, revision, joinSorted(machineIDs)))
}

// DiskCleanCanaryPreviewDigest binds the canary and the full target list.
func DiskCleanCanaryPreviewDigest(scopeType, scopeID string, revision int64, canary string, machineIDs []string) string {
	return digestOf(fmt.Sprintf("disk-clean-canary:v1:%s:%s:%d:%s:%s", scopeType, scopeID, revision, canary, joinSorted(machineIDs)))
}

// DiskCleanContinuePreviewDigest includes the rollout state so a reconcile
// between preview and apply cannot be confirmed.
func DiskCleanContinuePreviewDigest(rolloutID string, controlRevision int64, openedBatch int, state string) string {
	return digestOf(fmt.Sprintf("disk-clean-continue:v1:%s:%d:%d:%s", rolloutID, controlRevision, openedBatch, state))
}

// DiskCleanAbandonPreviewDigest includes the rollout state, same as continue.
func DiskCleanAbandonPreviewDigest(rolloutID string, controlRevision int64, openedBatch int, state string) string {
	return digestOf(fmt.Sprintf("disk-clean-abandon:v1:%s:%d:%d:%s", rolloutID, controlRevision, openedBatch, state))
}

// PreviewDiskCleanProfile validates and renders without writing.
func (s *Store) PreviewDiskCleanProfile(scopeType, scopeID string, profile maintenance.Profile) (DiskCleanProfilePreview, error) {
	if err := s.requireDiskCleanScope(scopeType, scopeID); err != nil {
		return DiskCleanProfilePreview{}, err
	}
	spec, conf, err := maintenance.BuildSpec(profile)
	if err != nil {
		return DiskCleanProfilePreview{}, diskCleanInvalid(err)
	}
	canonical, err := profile.Canonical()
	if err != nil {
		return DiskCleanProfilePreview{}, diskCleanInvalid(err)
	}
	rev, _, _, err := latestDiskCleanDesired(s.rdb, scopeType, scopeID)
	if err != nil {
		return DiskCleanProfilePreview{}, err
	}
	return DiskCleanProfilePreview{
		ScopeType: scopeType, ScopeID: scopeID, CurrentRevision: rev,
		ConfigDigest: spec.ConfigDigest, Conf: string(conf),
		PreviewDigest: DiskCleanProfilePreviewDigest(scopeType, scopeID, rev, string(canonical)),
	}, nil
}

// ApplyDiskCleanProfile publishes one desired_state revision for maintenance/disk-clean.
func (s *Store) ApplyDiskCleanProfile(req DiskCleanProfileRequest) (DiskCleanProfileResult, error) {
	operation := fmt.Sprintf("maintenance-profile:%s:%s", req.ScopeType, req.ScopeID)
	audit := diskCleanAudit(req.Audit, AuditMaintenanceProfile, req.IdempotencyKey, req.RequestDigest,
		req.ScopeType+":"+req.ScopeID, req.Reason)
	tx, replayed, err := s.beginDiskClean(req.IdempotencyKey, operation, req.RequestDigest, &audit,
		decodeDiskClean[DiskCleanProfileResult], "沒有再次發佈新的 revision")
	if tx != nil {
		defer tx.Rollback()
	}
	if err != nil {
		return DiskCleanProfileResult{}, err
	}
	if replayed != nil {
		return replayed.(DiskCleanProfileResult), nil
	}
	reject := func(code, detail string) (DiskCleanProfileResult, error) {
		return DiskCleanProfileResult{}, s.policyReject(tx, req.IdempotencyKey, operation, req.RequestDigest, &audit, code, detail)
	}
	if strings.TrimSpace(req.Reason) == "" {
		return reject(OperatorCodeReasonRequired, "reason is required")
	}
	if req.ConfirmScopeID != req.ScopeID {
		return reject(OperatorCodeConfirmationMismatch, fmt.Sprintf("confirm_scope_id is %q, not %q", req.ConfirmScopeID, req.ScopeID))
	}
	if err := requireDiskCleanScopeTx(tx, req.ScopeType, req.ScopeID); err != nil {
		return diskCleanReject[DiskCleanProfileResult](err, reject)
	}
	spec, _, err := maintenance.BuildSpec(req.Profile)
	if err != nil {
		if errors.Is(err, maintenance.ErrInvalid) {
			return reject(OperatorCodeMaintenanceProfileInvalid, err.Error())
		}
		return DiskCleanProfileResult{}, fmt.Errorf("store: render disk-clean profile: %w", err)
	}
	canonical, err := req.Profile.Canonical()
	if err != nil {
		return reject(OperatorCodeMaintenanceProfileInvalid, err.Error())
	}
	if req.ExpectedRevision == nil {
		return reject(OperatorCodePreconditionRequired, "expected_revision is required")
	}
	current, currentDigest, desiredID, err := latestDiskCleanDesired(tx, req.ScopeType, req.ScopeID)
	if err != nil {
		return DiskCleanProfileResult{}, err
	}
	if *req.ExpectedRevision != current {
		return reject(OperatorCodeMaintenanceRevisionConflict, fmt.Sprintf(
			"disk-clean scope %s/%s revision is %d, not %d", req.ScopeType, req.ScopeID, current, *req.ExpectedRevision))
	}
	want := DiskCleanProfilePreviewDigest(req.ScopeType, req.ScopeID, current, string(canonical))
	if req.PreviewDigest != want {
		return reject(OperatorCodeMaintenancePreviewStale, "preview is stale: the profile or its revision changed; preview again")
	}
	now := s.now().UTC()
	result := DiskCleanProfileResult{
		DesiredID: desiredID, ScopeType: req.ScopeType, ScopeID: req.ScopeID,
		Revision: current, ConfigDigest: spec.ConfigDigest, CreatedAt: now,
	}
	if current > 0 && currentDigest == spec.ConfigDigest {
		result.Unchanged = true
		var at string
		if err := tx.QueryRow(`SELECT created_at FROM desired_state WHERE desired_id=?`, desiredID).Scan(&at); err == nil {
			result.CreatedAt = parseTime(at)
		}
	} else {
		raw, err := maintenance.MarshalSpec(spec)
		if err != nil {
			return DiskCleanProfileResult{}, fmt.Errorf("store: encode disk-clean spec: %w", err)
		}
		by := strings.TrimSpace(req.PublishedBy)
		if by == "" {
			by = "operator"
		}
		id, rev, err := createDesiredStateTx(tx, req.ScopeType, req.ScopeID,
			maintenance.ResourceKind, maintenance.ResourceID, string(raw), by, now)
		if err != nil {
			return DiskCleanProfileResult{}, err
		}
		result.DesiredID = id
		result.Revision = int64(rev)
	}
	if err := s.policyCommit(tx, req.IdempotencyKey, operation, req.RequestDigest, &audit, result); err != nil {
		return DiskCleanProfileResult{}, err
	}
	result.Audited = true
	return result, nil
}

// PreviewDiskCleanDryRun checks the targets and returns a digest. Blockers
// are reported and are not part of the digest.
func (s *Store) PreviewDiskCleanDryRun(req DiskCleanTargetRequest) (DiskCleanDryRunPreview, error) {
	tx, err := s.rdb.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return DiskCleanDryRunPreview{}, fmt.Errorf("store: begin disk-clean dry-run preview: %w", err)
	}
	defer tx.Rollback()
	plan, err := s.prepareDiskCleanTargets(tx, req, false)
	if err != nil {
		return DiskCleanDryRunPreview{}, err
	}
	return plan.preview(), nil
}

// ApplyDiskCleanDryRun creates one reversible job per machine and assigns the profile.
func (s *Store) ApplyDiskCleanDryRun(req DiskCleanTargetRequest) (DiskCleanDryRunResult, error) {
	sorted := append([]string(nil), req.MachineIDs...)
	sort.Strings(sorted)
	operation := fmt.Sprintf("maintenance-dry-run:%d:%s", req.Revision, strings.Join(sorted, ","))
	audit := diskCleanAudit(req.Audit, AuditMaintenanceDryRun, req.IdempotencyKey, req.RequestDigest,
		fmt.Sprintf("%s:%s@%d", req.ScopeType, req.ScopeID, req.Revision), req.Reason)
	tx, replayed, err := s.beginDiskClean(req.IdempotencyKey, operation, req.RequestDigest, &audit,
		decodeDiskClean[DiskCleanDryRunResult], "沒有再次建立 dry-run 工作單")
	if tx != nil {
		defer tx.Rollback()
	}
	if err != nil {
		return DiskCleanDryRunResult{}, err
	}
	if replayed != nil {
		return replayed.(DiskCleanDryRunResult), nil
	}
	reject := func(code, detail string) (DiskCleanDryRunResult, error) {
		return DiskCleanDryRunResult{}, s.policyReject(tx, req.IdempotencyKey, operation, req.RequestDigest, &audit, code, detail)
	}
	if err := s.requireDiskCleanWrite(req.Reason, req.ConfirmScopeID, req.ScopeID); err != nil {
		return diskCleanReject[DiskCleanDryRunResult](err, reject)
	}
	plan, err := s.prepareDiskCleanTargets(tx, req, false)
	if err != nil {
		return diskCleanReject[DiskCleanDryRunResult](err, reject)
	}
	if req.PreviewDigest != plan.preview().PreviewDigest {
		return reject(OperatorCodeMaintenancePreviewStale, "preview is stale: the dry-run targets changed; preview again")
	}
	if len(plan.Blockers) > 0 {
		return reject(plan.Blockers[0].Code, plan.Blockers[0].Detail)
	}
	now := s.now().UTC()
	result := DiskCleanDryRunResult{
		DesiredID: plan.Desired.ID, Revision: plan.Desired.Revision,
		ConfigDigest: plan.Desired.Spec.ConfigDigest, MachineIDs: plan.Machines,
		JobIDs: make([]string, 0, len(plan.Machines)),
	}
	for _, machineID := range plan.Machines {
		jobID, err := createDiskCleanJobTx(tx, machineID, plan.Desired.ID, plan.Desired.Revision, plan.Desired.Spec.ConfigDigest, false, now)
		if err != nil {
			return diskCleanReject[DiskCleanDryRunResult](err, reject)
		}
		if err := assignDiskCleanTx(tx, machineID, plan.Desired.ID, plan.Desired.Revision, plan.Desired.Spec.ConfigDigest, now); err != nil {
			return DiskCleanDryRunResult{}, err
		}
		result.JobIDs = append(result.JobIDs, jobID)
	}
	if err := s.policyCommit(tx, req.IdempotencyKey, operation, req.RequestDigest, &audit, result); err != nil {
		return DiskCleanDryRunResult{}, err
	}
	result.Audited = true
	return result, nil
}

// PreviewDiskCleanCanary checks dry-run evidence and names exactly one canary.
func (s *Store) PreviewDiskCleanCanary(req DiskCleanTargetRequest) (DiskCleanCanaryPreview, error) {
	tx, err := s.rdb.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return DiskCleanCanaryPreview{}, fmt.Errorf("store: begin disk-clean canary preview: %w", err)
	}
	defer tx.Rollback()
	plan, err := s.prepareDiskCleanTargets(tx, req, true)
	if err != nil {
		return DiskCleanCanaryPreview{}, err
	}
	dry := plan.preview()
	return DiskCleanCanaryPreview{DiskCleanDryRunPreview: dry, CanaryMachineID: req.CanaryMachineID}, nil
}

// ApplyDiskCleanCanary opens a rollout whose first batch is exactly one machine.
func (s *Store) ApplyDiskCleanCanary(req DiskCleanTargetRequest) (DiskCleanCanaryResult, error) {
	sorted := append([]string(nil), req.MachineIDs...)
	sort.Strings(sorted)
	operation := fmt.Sprintf("maintenance-canary:%d:%s:%s", req.Revision, req.CanaryMachineID, strings.Join(sorted, ","))
	audit := diskCleanAudit(req.Audit, AuditMaintenanceCanary, req.IdempotencyKey, req.RequestDigest,
		fmt.Sprintf("%s:%s@%d", req.ScopeType, req.ScopeID, req.Revision), req.Reason)
	tx, replayed, err := s.beginDiskClean(req.IdempotencyKey, operation, req.RequestDigest, &audit,
		decodeDiskClean[DiskCleanCanaryResult], "沒有再次開啟 canary")
	if tx != nil {
		defer tx.Rollback()
	}
	if err != nil {
		return DiskCleanCanaryResult{}, err
	}
	if replayed != nil {
		return replayed.(DiskCleanCanaryResult), nil
	}
	reject := func(code, detail string) (DiskCleanCanaryResult, error) {
		return DiskCleanCanaryResult{}, s.policyReject(tx, req.IdempotencyKey, operation, req.RequestDigest, &audit, code, detail)
	}
	if err := s.requireDiskCleanWrite(req.Reason, req.ConfirmScopeID, req.ScopeID); err != nil {
		return diskCleanReject[DiskCleanCanaryResult](err, reject)
	}
	plan, err := s.prepareDiskCleanTargets(tx, req, true)
	if err != nil {
		return diskCleanReject[DiskCleanCanaryResult](err, reject)
	}
	want := DiskCleanCanaryPreviewDigest(req.ScopeType, req.ScopeID, plan.Desired.Revision, req.CanaryMachineID, plan.Machines)
	if req.PreviewDigest != want {
		return reject(OperatorCodeMaintenancePreviewStale, "preview is stale: the canary targets changed; preview again")
	}
	if len(plan.Blockers) > 0 {
		return reject(plan.Blockers[0].Code, plan.Blockers[0].Detail)
	}
	open, err := openDiskCleanRolloutTx(tx, plan.Desired.ID)
	if err != nil {
		return DiskCleanCanaryResult{}, err
	}
	if open != "" {
		return reject(OperatorCodeMaintenanceRolloutConflict, "a disk-clean rollout for this revision is already open")
	}
	now := s.now().UTC()
	rolloutID := newID()
	if _, err := tx.Exec(`INSERT INTO maintenance_rollouts
	 (rollout_id, desired_id, revision, state, control_revision, opened_batch, canary_machine_id,
	  scope_type, scope_id, config_digest, created_at, updated_at)
	 VALUES (?,?,?,?,1,1,?,?,?,?,?,?)`,
		rolloutID, plan.Desired.ID, plan.Desired.Revision, diskCleanStateCanary, req.CanaryMachineID,
		req.ScopeType, req.ScopeID, plan.Desired.Spec.ConfigDigest, fmtTime(now), fmtTime(now)); err != nil {
		return DiskCleanCanaryResult{}, fmt.Errorf("store: insert disk-clean rollout: %w", err)
	}
	var canaryJob string
	for _, machineID := range plan.Machines {
		batch := 2
		var jobID any
		if machineID == req.CanaryMachineID {
			batch = 1
			id, err := createDiskCleanJobTx(tx, machineID, plan.Desired.ID, plan.Desired.Revision, plan.Desired.Spec.ConfigDigest, true, now)
			if err != nil {
				return diskCleanReject[DiskCleanCanaryResult](err, reject)
			}
			canaryJob = id
			jobID = id
		}
		if _, err := tx.Exec(`INSERT INTO maintenance_rollout_targets
		 (rollout_id, machine_id, batch_no, job_id) VALUES (?,?,?,?)`, rolloutID, machineID, batch, jobID); err != nil {
			return DiskCleanCanaryResult{}, fmt.Errorf("store: insert disk-clean target: %w", err)
		}
		if err := assignDiskCleanTx(tx, machineID, plan.Desired.ID, plan.Desired.Revision, plan.Desired.Spec.ConfigDigest, now); err != nil {
			return DiskCleanCanaryResult{}, err
		}
	}
	result := DiskCleanCanaryResult{
		RolloutID: rolloutID, DesiredID: plan.Desired.ID, Revision: plan.Desired.Revision,
		ConfigDigest: plan.Desired.Spec.ConfigDigest, State: diskCleanStateCanary,
		ControlRevision: 1, OpenedBatch: 1, CanaryMachineID: req.CanaryMachineID,
		CanaryJobID: canaryJob, MachineIDs: plan.Machines,
	}
	if err := s.policyCommit(tx, req.IdempotencyKey, operation, req.RequestDigest, &audit, result); err != nil {
		return DiskCleanCanaryResult{}, err
	}
	result.Audited = true
	return result, nil
}

// PreviewDiskCleanContinue reconciles first, then digests the state the operator would confirm.
func (s *Store) PreviewDiskCleanContinue(rolloutID string) (DiskCleanControlPreview, error) {
	return s.previewDiskCleanControl(rolloutID, true)
}

// PreviewDiskCleanAbandon reconciles first, then digests the state the operator would confirm.
func (s *Store) PreviewDiskCleanAbandon(rolloutID string) (DiskCleanControlPreview, error) {
	return s.previewDiskCleanControl(rolloutID, false)
}

func (s *Store) previewDiskCleanControl(rolloutID string, continueRollout bool) (DiskCleanControlPreview, error) {
	if err := s.ReconcileDiskCleanRollouts(s.now()); err != nil {
		return DiskCleanControlPreview{}, err
	}
	row, err := loadDiskCleanRollout(s.rdb, rolloutID)
	if err != nil {
		return DiskCleanControlPreview{}, err
	}
	preview := DiskCleanControlPreview{
		RolloutID: row.ID, State: row.State, ControlRevision: row.ControlRevision, OpenedBatch: row.OpenedBatch,
	}
	if continueRollout {
		preview.PreviewDigest = DiskCleanContinuePreviewDigest(row.ID, row.ControlRevision, row.OpenedBatch, row.State)
	} else {
		preview.PreviewDigest = DiskCleanAbandonPreviewDigest(row.ID, row.ControlRevision, row.OpenedBatch, row.State)
	}
	return preview, nil
}

// ApplyDiskCleanContinue opens the rest of the rollout, or finishes when the canary was the only machine.
func (s *Store) ApplyDiskCleanContinue(req DiskCleanControlRequest) (DiskCleanRolloutResult, error) {
	return s.applyDiskCleanControl(req, true)
}

// ApplyDiskCleanAbandon stops a rollout without opening more jobs.
func (s *Store) ApplyDiskCleanAbandon(req DiskCleanControlRequest) (DiskCleanRolloutResult, error) {
	return s.applyDiskCleanControl(req, false)
}

func (s *Store) applyDiskCleanControl(req DiskCleanControlRequest, continueRollout bool) (DiskCleanRolloutResult, error) {
	action := AuditMaintenanceAbandon
	operation := "maintenance-abandon:" + req.RolloutID
	replayDetail := "沒有再次放棄 rollout"
	if continueRollout {
		action = AuditMaintenanceContinue
		operation = "maintenance-continue:" + req.RolloutID
		replayDetail = "沒有再次繼續 rollout"
	}
	audit := diskCleanAudit(req.Audit, action, req.IdempotencyKey, req.RequestDigest, req.RolloutID, req.Reason)
	tx, replayed, err := s.beginDiskClean(req.IdempotencyKey, operation, req.RequestDigest, &audit,
		decodeDiskClean[DiskCleanRolloutResult], replayDetail)
	if tx != nil {
		defer tx.Rollback()
	}
	if err != nil {
		return DiskCleanRolloutResult{}, err
	}
	if replayed != nil {
		return replayed.(DiskCleanRolloutResult), nil
	}
	reject := func(code, detail string) (DiskCleanRolloutResult, error) {
		return DiskCleanRolloutResult{}, s.policyReject(tx, req.IdempotencyKey, operation, req.RequestDigest, &audit, code, detail)
	}
	if strings.TrimSpace(req.Reason) == "" {
		return reject(OperatorCodeReasonRequired, "reason is required")
	}
	if req.ConfirmRolloutID != req.RolloutID {
		return reject(OperatorCodeConfirmationMismatch, fmt.Sprintf("confirm_rollout_id is %q, not %q", req.ConfirmRolloutID, req.RolloutID))
	}
	if req.ExpectedControlRevision <= 0 || req.ExpectedOpenedBatch <= 0 {
		return reject(OperatorCodePreconditionRequired, "expected_control_revision and expected_opened_batch are required")
	}
	if err := s.reconcileDiskCleanTx(tx, req.RolloutID, s.now()); err != nil {
		return DiskCleanRolloutResult{}, err
	}
	row, err := loadDiskCleanRollout(tx, req.RolloutID)
	if err != nil {
		return diskCleanReject[DiskCleanRolloutResult](err, reject)
	}
	if row.ControlRevision != req.ExpectedControlRevision || row.OpenedBatch != req.ExpectedOpenedBatch {
		return reject(OperatorCodeMaintenancePreviewStale, fmt.Sprintf(
			"preview is stale: control revision is %d and opened batch is %d", row.ControlRevision, row.OpenedBatch))
	}
	want := DiskCleanAbandonPreviewDigest(row.ID, row.ControlRevision, row.OpenedBatch, row.State)
	if continueRollout {
		want = DiskCleanContinuePreviewDigest(row.ID, row.ControlRevision, row.OpenedBatch, row.State)
	}
	if req.PreviewDigest != want {
		return reject(OperatorCodeMaintenancePreviewStale, "preview is stale: the rollout state changed; preview again")
	}
	if continueRollout {
		return s.continueDiskCleanTx(tx, req, row, reject, &audit, operation)
	}
	return s.abandonDiskCleanTx(tx, req, row, reject, &audit, operation)
}

func (s *Store) continueDiskCleanTx(tx dbTx, req DiskCleanControlRequest, row diskCleanRollout,
	reject func(string, string) (DiskCleanRolloutResult, error), audit *AuditEntry, operation string,
) (DiskCleanRolloutResult, error) {
	if row.State != diskCleanStatePaused || row.OpenedBatch != 1 {
		return reject(OperatorCodeMaintenanceRolloutConflict, "continue requires the disk-clean canary to be paused")
	}
	rest, err := diskCleanBatchMachines(tx, row.ID, 2)
	if err != nil {
		return DiskCleanRolloutResult{}, err
	}
	now := s.now().UTC()
	result := DiskCleanRolloutResult{
		RolloutID: row.ID, ControlRevision: row.ControlRevision + 1, OpenedBatch: row.OpenedBatch,
		JobIDs: []string{},
	}
	if len(rest) == 0 {
		result.State = diskCleanStateFinished
	} else {
		for _, machineID := range rest {
			blocker, err := s.machineBlockerTx(tx, machineID, row.ScopeType, row.ScopeID)
			if err != nil {
				return diskCleanReject[DiskCleanRolloutResult](err, reject)
			}
			if blocker != nil {
				return reject(blocker.Code, blocker.Detail)
			}
			jobID, err := createDiskCleanJobTx(tx, machineID, row.DesiredID, row.Revision, row.ConfigDigest, true, now)
			if err != nil {
				return diskCleanReject[DiskCleanRolloutResult](err, reject)
			}
			if _, err := tx.Exec(`UPDATE maintenance_rollout_targets SET job_id=?
			 WHERE rollout_id=? AND machine_id=? AND job_id IS NULL`, jobID, row.ID, machineID); err != nil {
				return DiskCleanRolloutResult{}, fmt.Errorf("store: attach disk-clean continue job: %w", err)
			}
			result.JobIDs = append(result.JobIDs, jobID)
		}
		result.State = diskCleanStateExpanding
		result.OpenedBatch = 2
	}
	res, err := tx.Exec(`UPDATE maintenance_rollouts
	 SET state=?, control_revision=control_revision+1, opened_batch=?, updated_at=?
	 WHERE rollout_id=? AND state=? AND control_revision=?`,
		result.State, result.OpenedBatch, fmtTime(now), row.ID, diskCleanStatePaused, row.ControlRevision)
	if err != nil {
		return DiskCleanRolloutResult{}, fmt.Errorf("store: continue disk-clean rollout: %w", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return DiskCleanRolloutResult{}, fmt.Errorf("store: disk-clean rollout %s changed during continue", row.ID)
	}
	if err := s.policyCommit(tx, req.IdempotencyKey, operation, req.RequestDigest, audit, result); err != nil {
		return DiskCleanRolloutResult{}, err
	}
	result.Audited = true
	return result, nil
}

func (s *Store) abandonDiskCleanTx(tx dbTx, req DiskCleanControlRequest, row diskCleanRollout,
	reject func(string, string) (DiskCleanRolloutResult, error), audit *AuditEntry, operation string,
) (DiskCleanRolloutResult, error) {
	switch row.State {
	case diskCleanStateFinished:
		return reject(OperatorCodeMaintenanceRolloutConflict, "disk-clean rollout is already finished")
	case diskCleanStateAbandoned:
		return reject(OperatorCodeMaintenanceRolloutConflict, "disk-clean rollout is already abandoned")
	}
	running, err := diskCleanNonterminalCount(tx, row.ID)
	if err != nil {
		return DiskCleanRolloutResult{}, err
	}
	if running > 0 {
		return reject(OperatorCodeMaintenanceRolloutConflict, "abandon refuses while a disk-clean job is still running")
	}
	now := s.now().UTC()
	res, err := tx.Exec(`UPDATE maintenance_rollouts
	 SET state=?, control_revision=control_revision+1, updated_at=?
	 WHERE rollout_id=? AND state=? AND control_revision=?`,
		diskCleanStateAbandoned, fmtTime(now), row.ID, row.State, row.ControlRevision)
	if err != nil {
		return DiskCleanRolloutResult{}, fmt.Errorf("store: abandon disk-clean rollout: %w", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return DiskCleanRolloutResult{}, fmt.Errorf("store: disk-clean rollout %s changed during abandon", row.ID)
	}
	result := DiskCleanRolloutResult{
		RolloutID: row.ID, State: diskCleanStateAbandoned,
		ControlRevision: row.ControlRevision + 1, OpenedBatch: row.OpenedBatch,
	}
	if err := s.policyCommit(tx, req.IdempotencyKey, operation, req.RequestDigest, audit, result); err != nil {
		return DiskCleanRolloutResult{}, err
	}
	result.Audited = true
	return result, nil
}

// ReconcileDiskCleanRollouts advances canary to paused or blocked, and
// expanding to finished or blocked. It never opens the next batch.
func (s *Store) ReconcileDiskCleanRollouts(now time.Time) error {
	rows, err := s.rdb.Query(`SELECT rollout_id FROM maintenance_rollouts WHERE state IN (?, ?)`,
		diskCleanStateCanary, diskCleanStateExpanding)
	if err != nil {
		return fmt.Errorf("store: list open disk-clean rollouts: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return fmt.Errorf("store: scan disk-clean rollout: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, id := range ids {
		tx, err := s.beginWrite(context.Background(), "disk_clean_reconcile")
		if err != nil {
			return fmt.Errorf("store: begin disk-clean reconcile: %w", err)
		}
		err = s.reconcileDiskCleanTx(tx, id, now)
		if err == nil {
			err = tx.Commit()
		}
		_ = tx.Rollback()
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) reconcileDiskCleanTx(tx dbTx, rolloutID string, now time.Time) error {
	row, err := loadDiskCleanRollout(tx, rolloutID)
	if err != nil {
		if op, ok := asOperator(err); ok && op.Code == OperatorCodeMaintenanceNotFound {
			return nil
		}
		return err
	}
	next := ""
	switch row.State {
	case diskCleanStateCanary:
		next, err = diskCleanBatchVerdict(tx, rolloutID, 1)
	case diskCleanStateExpanding:
		next, err = diskCleanExpandingVerdict(tx, rolloutID)
	default:
		return nil
	}
	if err != nil || next == "" || next == row.State {
		return err
	}
	if _, err := tx.Exec(`UPDATE maintenance_rollouts SET state=?, updated_at=? WHERE rollout_id=? AND state=?`,
		next, fmtTime(now), rolloutID, row.State); err != nil {
		return fmt.Errorf("store: reconcile disk-clean rollout: %w", err)
	}
	return nil
}

// ListDiskCleanSummaries returns assigned machines and machines that already
// have a summary. An empty fleet is an empty list.
func (s *Store) ListDiskCleanSummaries(now time.Time) ([]DiskCleanSummaryView, error) {
	rows, err := s.rdb.Query(`
SELECT m.machine_id, m.display_name, COALESCE(m.channel, '')
  FROM machine_registry m
 WHERE m.retired_at IS NULL
   AND (EXISTS (SELECT 1 FROM maintenance_assignments a WHERE a.machine_id=m.machine_id)
     OR EXISTS (SELECT 1 FROM maintenance_summaries s WHERE s.machine_id=m.machine_id))
 ORDER BY m.display_name, m.machine_id`)
	if err != nil {
		return nil, fmt.Errorf("store: list disk-clean summaries: %w", err)
	}
	defer rows.Close()
	assignments, err := s.complianceAssignments()
	if err != nil {
		return nil, err
	}
	out := []DiskCleanSummaryView{}
	for rows.Next() {
		var id, name, channel string
		if err := rows.Scan(&id, &name, &channel); err != nil {
			return nil, fmt.Errorf("store: scan disk-clean summary row: %w", err)
		}
		view, err := s.diskCleanSummaryView(id, name, channel, now, assignments)
		if err != nil {
			return nil, err
		}
		out = append(out, view)
	}
	return out, rows.Err()
}

// DiskCleanSummary returns one machine. A machine that is enrolled but not
// assigned is stale, not missing. A machine that is not in the registry is not found.
func (s *Store) DiskCleanSummary(machineID string, now time.Time) (DiskCleanSummaryView, error) {
	machine, err := s.GetMachine(machineID)
	if errors.Is(err, ErrNotFound) {
		return DiskCleanSummaryView{}, operatorError(OperatorCodeMachineNotFound, "machine was not found")
	}
	if err != nil {
		return DiskCleanSummaryView{}, err
	}
	assignments, err := s.complianceAssignments()
	if err != nil {
		return DiskCleanSummaryView{}, err
	}
	return s.diskCleanSummaryView(machine.MachineID, machine.DisplayName, machine.Channel, now, assignments)
}

// SweepDiskCleanAlerts returns conditions that should be notified. Delivery is
// recorded by MarkDiskCleanAlertDelivered only after notify succeeds.
func (s *Store) SweepDiskCleanAlerts(now time.Time) ([]DiskCleanAlertSend, error) {
	rows, err := s.rdb.Query(`
SELECT a.machine_id, m.display_name, COALESCE(m.channel, '')
  FROM maintenance_assignments a
  JOIN machine_registry m ON m.machine_id=a.machine_id
 WHERE m.retired_at IS NULL
 ORDER BY a.machine_id`)
	if err != nil {
		return nil, fmt.Errorf("store: list disk-clean alert machines: %w", err)
	}
	defer rows.Close()
	type machine struct{ id, name, channel string }
	var machines []machine
	for rows.Next() {
		var m machine
		if err := rows.Scan(&m.id, &m.name, &m.channel); err != nil {
			return nil, err
		}
		machines = append(machines, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	assignments, err := s.complianceAssignments()
	if err != nil {
		return nil, err
	}
	// Every verdict is computed on the reader pool before the writer is
	// taken. diskCleanSummaryView reads several tables per machine; doing that
	// while holding the single writer would stall check-ins for the whole sweep.
	views := make([]DiskCleanSummaryView, len(machines))
	for i, m := range machines {
		view, err := s.diskCleanSummaryView(m.id, m.name, m.channel, now, assignments)
		if err != nil {
			return nil, err
		}
		views[i] = view
	}
	tx, err := s.beginWrite(context.Background(), "disk_clean_alerts")
	if err != nil {
		return nil, fmt.Errorf("store: begin disk-clean alerts: %w", err)
	}
	defer tx.Rollback()
	var sends []DiskCleanAlertSend
	for i, m := range machines {
		view := views[i]
		verdict := maintenance.Verdict{
			Outcome: view.Verdict, Stale: view.Stale, Attention: view.Attention,
			Mode: view.Mode, Disk: view.Disk, Reasons: view.Reasons,
		}
		for _, alert := range maintenance.AlertViews(verdict) {
			var prev maintenance.StoredAlert
			var fingerprint string
			var active, delivered int
			err := tx.QueryRow(`SELECT fingerprint, active, delivered FROM maintenance_alert_state
			 WHERE machine_id=? AND condition=?`, m.id, alert.Condition).Scan(&fingerprint, &active, &delivered)
			found := err == nil
			if errors.Is(err, sql.ErrNoRows) {
				err = nil
			}
			if err != nil {
				return nil, fmt.Errorf("store: read disk-clean alert: %w", err)
			}
			if fingerprint != "" || active != 0 || delivered != 0 {
				prev = maintenance.StoredAlert{Fingerprint: fingerprint, Active: active == 1, Delivered: delivered == 1}
			}
			send := maintenance.ShouldNotify(prev, alert)
			activeBit := 0
			if alert.Active {
				activeBit = 1
			}
			deliveredBit := 0
			if !send && prev.Delivered && prev.Fingerprint == alert.Fingerprint && alert.Active {
				deliveredBit = 1
			}
			unchanged := found && fingerprint == alert.Fingerprint && active == activeBit && delivered == deliveredBit
			if unchanged {
				// Nothing to record. Skipping the upsert keeps the minute sweep
				// from rewriting every alert row on the single writer.
			} else if _, err := tx.Exec(`INSERT INTO maintenance_alert_state
			 (machine_id, condition, fingerprint, active, delivered, updated_at)
			 VALUES (?,?,?,?,?,?)
			 ON CONFLICT(machine_id, condition) DO UPDATE SET
			   fingerprint=excluded.fingerprint, active=excluded.active,
			   delivered=excluded.delivered, updated_at=excluded.updated_at`,
				m.id, alert.Condition, alert.Fingerprint, activeBit, deliveredBit, fmtTime(now)); err != nil {
				return nil, fmt.Errorf("store: upsert disk-clean alert: %w", err)
			}
			if !send {
				continue
			}
			detail := strings.Join(view.Reasons, "; ")
			if detail == "" {
				detail = alert.Fingerprint
			}
			body := fmt.Sprintf("disk-clean %s on %s: %s", alert.Condition, m.name, detail)
			if len(body) > 2000 {
				body = body[:2000] + "…"
			}
			sends = append(sends, DiskCleanAlertSend{
				Kind: fmt.Sprintf("disk-clean:%s:%s", m.id, alert.Condition), Body: body,
				MachineID: m.id, Condition: alert.Condition, Fingerprint: alert.Fingerprint,
			})
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("store: commit disk-clean alerts: %w", err)
	}
	return sends, nil
}

// MarkDiskCleanAlertDelivered records a successful notify for the fingerprint
// that was sent. A changed fingerprint is left for the next sweep.
func (s *Store) MarkDiskCleanAlertDelivered(machineID, condition, fingerprint string) error {
	if _, err := s.execWrite(context.Background(), "disk_clean_alert_mark", `UPDATE maintenance_alert_state SET delivered=1, updated_at=?
	 WHERE machine_id=? AND condition=? AND fingerprint=? AND active=1`,
		fmtTime(s.now()), machineID, condition, fingerprint); err != nil {
		return fmt.Errorf("store: mark disk-clean alert delivered: %w", err)
	}
	return nil
}

// projectMaintenanceSummary copies a parseable fleet-disk-clean/v1 line onto
// the board. A line that does not parse is left as executor evidence only.
func (s *Store) projectMaintenanceSummary(jobID, machineID, stdoutExcerpt string, receivedAt time.Time) error {
	var kind, resourceID string
	var revision int64
	err := s.rdb.QueryRow(`
SELECT d.resource_kind, d.resource_id, j.revision
  FROM jobs j
  JOIN desired_state d ON d.desired_id = j.desired_id
 WHERE j.job_id=? AND j.machine_id=?`, jobID, machineID).Scan(&kind, &resourceID, &revision)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("store: inspect disk-clean verification job: %w", err)
	}
	if kind != maintenance.ResourceKind || resourceID != maintenance.ResourceID {
		return nil
	}
	line := strings.TrimSpace(stdoutExcerpt)
	summary, err := maintenance.ParseSummary([]byte(line))
	if err != nil {
		return nil
	}
	if _, err := s.execWrite(context.Background(), "disk_clean_summary", `INSERT OR IGNORE INTO maintenance_summaries
	 (summary_id, machine_id, job_id, received_at, revision, config_digest, mode, summary_json)
	 VALUES (?,?,?,?,?,?,?,?)`,
		newID(), machineID, jobID, fmtTime(receivedAt), revision, summary.ConfigDigest, summary.Mode, line); err != nil {
		return fmt.Errorf("store: project disk-clean summary: %w", err)
	}
	return nil
}

type diskCleanDesired struct {
	ID       string
	Revision int64
	Spec     maintenance.Spec
}

type diskCleanPlan struct {
	ScopeType string
	ScopeID   string
	Desired   diskCleanDesired
	Machines  []string
	Canary    string
	Blockers  []DiskCleanBlocker
	CanaryOK  bool
}

func (p diskCleanPlan) preview() DiskCleanDryRunPreview {
	blockers := p.Blockers
	if blockers == nil {
		blockers = []DiskCleanBlocker{}
	}
	digest := DiskCleanDryRunPreviewDigest(p.ScopeType, p.ScopeID, p.Desired.Revision, p.Machines)
	if p.CanaryOK {
		digest = DiskCleanCanaryPreviewDigest(p.ScopeType, p.ScopeID, p.Desired.Revision, p.Canary, p.Machines)
	}
	return DiskCleanDryRunPreview{
		ScopeType: p.ScopeType, ScopeID: p.ScopeID, Revision: p.Desired.Revision,
		ConfigDigest: p.Desired.Spec.ConfigDigest, MachineIDs: append([]string(nil), p.Machines...),
		PreviewDigest: digest, Blockers: blockers,
	}
}

type diskCleanRollout struct {
	ID              string
	DesiredID       string
	Revision        int64
	State           string
	ControlRevision int64
	OpenedBatch     int
	CanaryMachineID string
	ScopeType       string
	ScopeID         string
	ConfigDigest    string
}

func (s *Store) prepareDiskCleanTargets(tx dbTx, req DiskCleanTargetRequest, canary bool) (diskCleanPlan, error) {
	var plan diskCleanPlan
	plan.ScopeType = req.ScopeType
	plan.ScopeID = req.ScopeID
	if req.Revision <= 0 {
		return plan, operatorError(OperatorCodePreconditionRequired, "revision is required")
	}
	machines, err := normalizeMachineIDs(req.MachineIDs)
	if err != nil {
		return plan, operatorError(OperatorCodeMaintenanceProfileInvalid, err.Error())
	}
	if err := requireDiskCleanScopeTx(tx, req.ScopeType, req.ScopeID); err != nil {
		return plan, err
	}
	if req.ScopeType == "machine" && (len(machines) != 1 || machines[0] != req.ScopeID) {
		return plan, operatorError(OperatorCodeMaintenanceProfileInvalid, "machine scope disk-clean runs only on the scope machine")
	}
	desired, err := lookupDiskCleanDesired(tx, req.ScopeType, req.ScopeID, req.Revision)
	if err != nil {
		return plan, err
	}
	plan.Desired = desired
	plan.Machines = machines
	if canary {
		if req.CanaryMachineID == "" || !diskCleanHasMachine(machines, req.CanaryMachineID) {
			return plan, operatorError(OperatorCodeMaintenanceProfileInvalid, "canary_machine_id must be one of machine_ids")
		}
		plan.Canary = req.CanaryMachineID
		plan.CanaryOK = true
		open, err := openDiskCleanRolloutTx(tx, desired.ID)
		if err != nil {
			return plan, err
		}
		if open != "" {
			return plan, operatorError(OperatorCodeMaintenanceRolloutConflict, "a disk-clean rollout for this revision is already open")
		}
	}
	for _, machineID := range machines {
		if blocker, err := s.machineBlockerTx(tx, machineID, req.ScopeType, req.ScopeID); err != nil {
			return plan, err
		} else if blocker != nil {
			plan.Blockers = append(plan.Blockers, *blocker)
		}
		if canary {
			if err := dryRunEvidenceTx(tx, machineID, desired.ID, desired.Spec.ConfigDigest); err != nil {
				return plan, err
			}
		}
	}
	if plan.Blockers == nil {
		plan.Blockers = []DiskCleanBlocker{}
	}
	return plan, nil
}

func (s *Store) machineBlockerTx(tx dbTx, machineID, scopeType, scopeID string) (*DiskCleanBlocker, error) {
	gate, err := diskCleanGateTx(tx, machineID)
	if err != nil {
		return nil, err
	}
	if !gate.Found {
		return nil, operatorError(OperatorCodeMachineNotFound, "machine was not found")
	}
	if gate.Retired {
		return nil, operatorError(OperatorCodeMachineRetired, "machine is retired")
	}
	if scopeType == "channel" && gate.Channel != scopeID {
		return nil, operatorError(OperatorCodeMaintenanceRolloutConflict,
			fmt.Sprintf("machine %s is not in channel %s", machineID, scopeID))
	}
	if gate.Capable == nil || !*gate.Capable {
		b := DiskCleanBlocker{Code: OperatorCodeMaintenanceCapabilityUnconfirmed, Detail: "machine has not confirmed maintenance_disk_clean_v1"}
		return &b, nil
	}
	if gate.JobsEnabled == nil {
		b := DiskCleanBlocker{Code: OperatorCodeAgentExecutionUnknown, Detail: "agent execution is unknown"}
		return &b, nil
	}
	if !*gate.JobsEnabled {
		b := DiskCleanBlocker{Code: OperatorCodeAgentExecutionDisabled, Detail: "agent execution is disabled"}
		return &b, nil
	}
	if gate.ActiveJobs > 0 {
		b := DiskCleanBlocker{Code: OperatorCodeMachineActiveJob, Detail: "machine has a job that is still running"}
		return &b, nil
	}
	return nil, nil
}

type diskCleanGate struct {
	Channel     string
	Retired     bool
	Found       bool
	JobsEnabled *bool
	Capable     *bool
	ActiveJobs  int
}

func diskCleanGateTx(tx dbTx, machineID string) (diskCleanGate, error) {
	var gate diskCleanGate
	var channel, retired sql.NullString
	var jobs, capable sql.NullBool
	err := tx.QueryRow(`
SELECT m.channel, m.retired_at, c.jobs_enabled, cap.supported,
       (SELECT COUNT(*) FROM jobs j WHERE j.machine_id=m.machine_id AND j.state NOT IN (?,?,?,?,?))
  FROM machine_registry m
  LEFT JOIN machine_checkins c ON c.rowid=(
    SELECT rowid FROM machine_checkins WHERE machine_id=m.machine_id
     ORDER BY received_at DESC, rowid DESC LIMIT 1)
  LEFT JOIN machine_job_capabilities cap
    ON cap.machine_id=c.machine_id AND cap.sent_at=c.sent_at AND cap.capability=?
 WHERE m.machine_id=?`,
		deploy.Succeeded, deploy.Failed, deploy.Rejected, deploy.LeaseExpired, deploy.ManualIntervention,
		model.MaintenanceDiskCleanCapability, machineID).Scan(&channel, &retired, &jobs, &capable, &gate.ActiveJobs)
	if errors.Is(err, sql.ErrNoRows) {
		return gate, nil
	}
	if err != nil {
		return gate, fmt.Errorf("store: inspect disk-clean machine: %w", err)
	}
	gate.Found = true
	if channel.Valid {
		gate.Channel = channel.String
	}
	gate.Retired = retired.Valid && retired.String != ""
	if jobs.Valid {
		value := jobs.Bool
		gate.JobsEnabled = &value
	}
	if capable.Valid {
		value := capable.Bool
		gate.Capable = &value
	}
	return gate, nil
}

func dryRunEvidenceTx(tx dbTx, machineID, desiredID, digest string) error {
	var stdout string
	err := tx.QueryRow(`
SELECT v.stdout_excerpt
  FROM jobs j
  JOIN verification_results v
    ON v.job_id=j.job_id AND v.rule_id=? AND v.passed=1 AND v.evidence_role=?
 WHERE j.machine_id=? AND j.desired_id=? AND j.state=? AND j.irreversible=0
 ORDER BY j.terminal_at DESC LIMIT 1`,
		maintenance.VerificationRuleID, JobVerificationRoleExecutor,
		machineID, desiredID, string(deploy.Succeeded)).Scan(&stdout)
	if errors.Is(err, sql.ErrNoRows) {
		return operatorError(OperatorCodeMaintenanceDryRunRequired,
			fmt.Sprintf("machine %s has no succeeded dry-run for this disk-clean revision", machineID))
	}
	if err != nil {
		return fmt.Errorf("store: read disk-clean dry-run evidence: %w", err)
	}
	summary, err := maintenance.ParseSummary([]byte(stdout))
	if err != nil || summary.Mode != "dry-run" || summary.ConfigDigest != digest {
		return operatorError(OperatorCodeMaintenanceDryRunRequired,
			fmt.Sprintf("machine %s dry-run evidence does not match this disk-clean revision", machineID))
	}
	return nil
}

func createDiskCleanJobTx(tx dbTx, machineID, desiredID string, revision int64, digest string, irreversible bool, now time.Time) (string, error) {
	jobID, err := createJobTx(tx, machineID, desiredID, deploy.Revision(revision), NewJob{
		ArtifactDigest:   digest,
		Irreversible:     irreversible,
		ExecutionTimeout: maintenance.ExecutionTimeoutSeconds,
	}, now)
	if err != nil {
		return "", err
	}
	return jobID, nil
}

func assignDiskCleanTx(tx dbTx, machineID, desiredID string, revision int64, digest string, now time.Time) error {
	if _, err := tx.Exec(`INSERT INTO maintenance_assignments
	 (machine_id, desired_id, revision, config_digest, assigned_at)
	 VALUES (?,?,?,?,?)
	 ON CONFLICT(machine_id) DO UPDATE SET
	   desired_id=excluded.desired_id, revision=excluded.revision,
	   config_digest=excluded.config_digest, assigned_at=excluded.assigned_at`,
		machineID, desiredID, revision, digest, fmtTime(now)); err != nil {
		return fmt.Errorf("store: assign disk-clean: %w", err)
	}
	return nil
}

func lookupDiskCleanDesired(q queryRower, scopeType, scopeID string, revision int64) (diskCleanDesired, error) {
	var desired diskCleanDesired
	var raw string
	err := q.QueryRow(`SELECT desired_id, revision, spec FROM desired_state
	 WHERE resource_kind=? AND resource_id=? AND scope_type=? AND scope_id=? AND revision=?`,
		maintenance.ResourceKind, maintenance.ResourceID, scopeType, scopeID, revision).Scan(&desired.ID, &desired.Revision, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return desired, operatorError(OperatorCodeMaintenanceNotFound, "disk-clean profile revision was not found for this scope")
	}
	if err != nil {
		return desired, fmt.Errorf("store: read disk-clean desired state: %w", err)
	}
	spec, err := maintenance.ParseSpec([]byte(raw))
	if err != nil {
		return desired, fmt.Errorf("store: stored disk-clean spec is unreadable: %w", err)
	}
	desired.Spec = spec
	return desired, nil
}

func latestDiskCleanDesired(q queryRower, scopeType, scopeID string) (int64, string, string, error) {
	var revision int64
	var desiredID, raw string
	err := q.QueryRow(`SELECT desired_id, revision, spec FROM desired_state
	 WHERE resource_kind=? AND resource_id=? AND scope_type=? AND scope_id=?
	 ORDER BY revision DESC LIMIT 1`,
		maintenance.ResourceKind, maintenance.ResourceID, scopeType, scopeID).Scan(&desiredID, &revision, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", "", nil
	}
	if err != nil {
		return 0, "", "", fmt.Errorf("store: read latest disk-clean desired state: %w", err)
	}
	spec, err := maintenance.ParseSpec([]byte(raw))
	if err != nil {
		return 0, "", "", fmt.Errorf("store: stored disk-clean spec is unreadable: %w", err)
	}
	return revision, spec.ConfigDigest, desiredID, nil
}

func loadDiskCleanRollout(q queryRower, rolloutID string) (diskCleanRollout, error) {
	var row diskCleanRollout
	err := q.QueryRow(`SELECT rollout_id, desired_id, revision, state, control_revision, opened_batch,
	 canary_machine_id, scope_type, scope_id, config_digest
	 FROM maintenance_rollouts WHERE rollout_id=?`, rolloutID).Scan(
		&row.ID, &row.DesiredID, &row.Revision, &row.State, &row.ControlRevision, &row.OpenedBatch,
		&row.CanaryMachineID, &row.ScopeType, &row.ScopeID, &row.ConfigDigest)
	if errors.Is(err, sql.ErrNoRows) {
		return row, operatorError(OperatorCodeMaintenanceNotFound, "disk-clean rollout was not found")
	}
	if err != nil {
		return row, fmt.Errorf("store: read disk-clean rollout: %w", err)
	}
	return row, nil
}

func openDiskCleanRolloutTx(tx dbTx, desiredID string) (string, error) {
	var id string
	err := tx.QueryRow(`SELECT rollout_id FROM maintenance_rollouts
	 WHERE desired_id=? AND state NOT IN (?, ?) LIMIT 1`,
		desiredID, diskCleanStateFinished, diskCleanStateAbandoned).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("store: inspect open disk-clean rollout: %w", err)
	}
	return id, nil
}

func diskCleanBatchMachines(tx dbTx, rolloutID string, batch int) ([]string, error) {
	rows, err := tx.Query(`SELECT machine_id FROM maintenance_rollout_targets
	 WHERE rollout_id=? AND batch_no=? ORDER BY machine_id`, rolloutID, batch)
	if err != nil {
		return nil, fmt.Errorf("store: list disk-clean batch: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func diskCleanBatchVerdict(tx dbTx, rolloutID string, batch int) (string, error) {
	var state sql.NullString
	err := tx.QueryRow(`
SELECT j.state FROM maintenance_rollout_targets t
 LEFT JOIN jobs j ON j.job_id=t.job_id
 WHERE t.rollout_id=? AND t.batch_no=?`, rolloutID, batch).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("store: read disk-clean batch job: %w", err)
	}
	if !state.Valid || !deploy.IsTerminal(deploy.JobState(state.String)) {
		return "", nil
	}
	if state.String == string(deploy.Succeeded) {
		return diskCleanStatePaused, nil
	}
	return diskCleanStateBlocked, nil
}

func diskCleanExpandingVerdict(tx dbTx, rolloutID string) (string, error) {
	rows, err := tx.Query(`
SELECT j.state FROM maintenance_rollout_targets t
 LEFT JOIN jobs j ON j.job_id=t.job_id
 WHERE t.rollout_id=?`, rolloutID)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	saw := false
	for rows.Next() {
		var state sql.NullString
		if err := rows.Scan(&state); err != nil {
			return "", err
		}
		saw = true
		if !state.Valid || !deploy.IsTerminal(deploy.JobState(state.String)) {
			return "", rows.Err()
		}
		if state.String != string(deploy.Succeeded) {
			return diskCleanStateBlocked, rows.Err()
		}
	}
	if err := rows.Err(); err != nil || !saw {
		return "", err
	}
	return diskCleanStateFinished, nil
}

func diskCleanNonterminalCount(tx dbTx, rolloutID string) (int, error) {
	var n int
	err := tx.QueryRow(`
SELECT COUNT(*) FROM maintenance_rollout_targets t
 JOIN jobs j ON j.job_id=t.job_id
 WHERE t.rollout_id=? AND j.state NOT IN (?,?,?,?,?)`,
		rolloutID, deploy.Succeeded, deploy.Failed, deploy.Rejected, deploy.LeaseExpired, deploy.ManualIntervention).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: count disk-clean running jobs: %w", err)
	}
	return n, nil
}

func (s *Store) diskCleanSummaryView(machineID, displayName, channel string, now time.Time, assignments []compliance.Assignment) (DiskCleanSummaryView, error) {
	view := DiskCleanSummaryView{MachineID: machineID, DisplayName: displayName, Disk: maintenance.DiskEvaluation{Outcome: maintenance.OutcomeUnmeasured}}
	var assignedRevision int64
	var expected string
	err := s.rdb.QueryRow(`SELECT revision, config_digest FROM maintenance_assignments WHERE machine_id=?`, machineID).Scan(&assignedRevision, &expected)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return view, fmt.Errorf("store: read disk-clean assignment: %w", err)
	}
	if err == nil {
		view.Assigned = true
		view.ExpectedDigest = expected
		view.Revision = assignedRevision
	}
	summary, received, summaryRevision, ok, err := s.latestDiskCleanSummary(machineID)
	if err != nil {
		return view, err
	}
	var receivedAt time.Time
	if ok {
		view.Summary = summary
		view.ConfigDigest = summary.ConfigDigest
		view.Mode = summary.Mode
		view.Attention = summary.Attention
		receivedAt = received
		view.ReceivedAt = &received
		if !view.Assigned {
			view.Revision = summaryRevision
		}
	}
	free, total, obsMeasured, obsReceived, measured, err := s.observationDisk(machineID)
	if err != nil {
		return view, err
	}
	if !measured {
		free, total = 0, 0
	}
	view.Disk = maintenance.EvaluateObservationDisk(free, total, diskRuleFor(assignments, machineID, channel))
	// "Still over threshold after cleaning" needs a reading taken after the
	// clean. An observation the Hub received before the apply summary, or one
	// the agent measured before the script's own timestamp, is a pre-clean
	// reading: report the disk as unmeasured until a fresh observation arrives.
	if ok && measured && (summary.Mode == "apply" || summary.Mode == "mixed") &&
		maintenance.ObservationPredatesSummary(obsMeasured, obsReceived, summary.TS, receivedAt) {
		view.Disk = maintenance.DiskEvaluation{
			Outcome:        maintenance.OutcomeUnmeasured,
			Detail:         "waiting for a resources observation taken after the latest disk-clean apply",
			RuleAssigned:   view.Disk.RuleAssigned,
			MinFreePercent: view.Disk.MinFreePercent,
		}
	}
	verdict := maintenance.Decide(summary, receivedAt, now, view.ExpectedDigest, view.Disk)
	view.Verdict = verdict.Outcome
	view.Stale = verdict.Stale
	view.Reasons = verdict.Reasons
	view.DigestMatches = ok && verdict.DigestOK
	return view, nil
}

func (s *Store) latestDiskCleanSummary(machineID string) (*maintenance.Summary, time.Time, int64, bool, error) {
	var raw, at string
	var revision int64
	err := s.rdb.QueryRow(`SELECT summary_json, received_at, revision FROM maintenance_summaries
	 WHERE machine_id=? ORDER BY received_at DESC, summary_id DESC LIMIT 1`, machineID).Scan(&raw, &at, &revision)
	if err == nil {
		summary, perr := maintenance.ParseSummary([]byte(raw))
		if perr != nil {
			return nil, time.Time{}, 0, false, fmt.Errorf("store: stored disk-clean summary is unreadable: %w", perr)
		}
		return &summary, parseTime(at), revision, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, time.Time{}, 0, false, fmt.Errorf("store: read disk-clean summary: %w", err)
	}
	err = s.rdb.QueryRow(`
SELECT v.stdout_excerpt, v.received_at, j.revision
  FROM verification_results v
  JOIN jobs j ON j.job_id=v.job_id
  JOIN desired_state d ON d.desired_id=j.desired_id
 WHERE v.machine_id=? AND v.rule_id=? AND v.evidence_role=?
   AND d.resource_kind=? AND d.resource_id=?
 ORDER BY v.received_at DESC LIMIT 1`,
		machineID, maintenance.VerificationRuleID, JobVerificationRoleExecutor,
		maintenance.ResourceKind, maintenance.ResourceID).Scan(&raw, &at, &revision)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, time.Time{}, 0, false, nil
	}
	if err != nil {
		return nil, time.Time{}, 0, false, fmt.Errorf("store: read disk-clean evidence: %w", err)
	}
	summary, perr := maintenance.ParseSummary([]byte(raw))
	if perr != nil {
		return nil, time.Time{}, 0, false, nil
	}
	return &summary, parseTime(at), revision, true, nil
}

func (s *Store) observationDisk(machineID string) (free, total int64, measuredAt, receivedAt time.Time, ok bool, err error) {
	obs, found, err := s.latestObservation(machineID, KindResources, KindResources)
	if err != nil || !found {
		return 0, 0, time.Time{}, time.Time{}, false, err
	}
	var resources model.Resources
	if err := json.Unmarshal([]byte(obs.Payload), &resources); err != nil {
		return 0, 0, time.Time{}, time.Time{}, false, nil
	}
	return resources.DiskFreeBytes, resources.DiskTotalBytes, obs.MeasuredAt, obs.ReceivedAt, true, nil
}

func diskRuleFor(assignments []compliance.Assignment, machineID, channel string) *compliance.Rule {
	effective := compliance.Resolve(machineID, channel, assignments)
	if !effective.Assigned() {
		return nil
	}
	for i := range effective.Policy.Rules {
		if effective.Policy.Rules[i].Kind == compliance.RuleDiskFreeMinPercent {
			rule := effective.Policy.Rules[i]
			return &rule
		}
	}
	return nil
}

func (s *Store) beginDiskClean(key, operation, requestDigest string, audit *AuditEntry, decode func(string) (any, error), replayDetail string) (*writeTx, any, error) {
	if strings.TrimSpace(key) == "" || len(key) > 200 {
		return nil, nil, operatorError(OperatorCodeIdempotencyKeyRequired, "Idempotency-Key is required and at most 200 bytes")
	}
	if !validArtifactFetchDigest(requestDigest) {
		return nil, nil, operatorError(OperatorCodeRequestDigestRequired, "request digest must be sha256 and 64 lowercase hex characters")
	}
	tx, err := s.beginWrite(context.Background(), "disk_clean_"+strings.SplitN(operation, ":", 2)[0])
	if err != nil {
		return nil, nil, fmt.Errorf("store: begin disk-clean: %w", err)
	}
	replayed, rejected, err := s.policyIdempotency(tx, key, operation, requestDigest, audit, decode, replayDetail)
	if err != nil {
		return tx, nil, err
	}
	if rejected != nil {
		return tx, nil, rejected
	}
	return tx, replayed, nil
}

func (s *Store) requireDiskCleanScope(scopeType, scopeID string) error {
	if scopeType != "machine" && scopeType != "channel" {
		return operatorError(OperatorCodeMaintenanceProfileInvalid, "scope_type must be machine or channel")
	}
	if scopeType == "channel" {
		if scopeID != "canary" && scopeID != "stable" {
			return operatorError(OperatorCodeMaintenanceProfileInvalid, "channel scope_id must be canary or stable")
		}
		return nil
	}
	machine, err := s.GetMachine(scopeID)
	if errors.Is(err, ErrNotFound) {
		return operatorError(OperatorCodeMachineNotFound, "machine was not found")
	}
	if err != nil {
		return err
	}
	if machine.RetiredAt != nil {
		return operatorError(OperatorCodeMachineRetired, "machine is retired")
	}
	return nil
}

func requireDiskCleanScopeTx(tx dbTx, scopeType, scopeID string) error {
	if scopeType != "machine" && scopeType != "channel" {
		return operatorError(OperatorCodeMaintenanceProfileInvalid, "scope_type must be machine or channel")
	}
	if scopeType == "channel" {
		if scopeID != "canary" && scopeID != "stable" {
			return operatorError(OperatorCodeMaintenanceProfileInvalid, "channel scope_id must be canary or stable")
		}
		return nil
	}
	var retired sql.NullString
	err := tx.QueryRow(`SELECT retired_at FROM machine_registry WHERE machine_id=?`, scopeID).Scan(&retired)
	if errors.Is(err, sql.ErrNoRows) {
		return operatorError(OperatorCodeMachineNotFound, "machine was not found")
	}
	if err != nil {
		return fmt.Errorf("store: inspect disk-clean scope: %w", err)
	}
	if retired.Valid && retired.String != "" {
		return operatorError(OperatorCodeMachineRetired, "machine is retired")
	}
	return nil
}

func (s *Store) requireDiskCleanWrite(reason, confirm, scopeID string) error {
	if strings.TrimSpace(reason) == "" {
		return operatorError(OperatorCodeReasonRequired, "reason is required")
	}
	if confirm != scopeID {
		return operatorError(OperatorCodeConfirmationMismatch, fmt.Sprintf("confirm_scope_id is %q, not %q", confirm, scopeID))
	}
	return nil
}

func diskCleanAudit(base AuditEntry, action AuditAction, key, digest, subject, reason string) AuditEntry {
	base.Action = action
	base.IdempotencyKey = key
	base.RequestDigest = digest
	base.Subject = subject
	base.Reason = reason
	return base
}

func decodeDiskClean[T any](raw string) (any, error) {
	var out T
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, err
	}
	switch value := any(&out).(type) {
	case *DiskCleanProfileResult:
		value.Replayed, value.Audited = true, true
	case *DiskCleanDryRunResult:
		value.Replayed, value.Audited = true, true
	case *DiskCleanCanaryResult:
		value.Replayed, value.Audited = true, true
	case *DiskCleanRolloutResult:
		value.Replayed, value.Audited = true, true
	}
	return out, nil
}

func diskCleanReject[T any](err error, reject func(string, string) (T, error)) (T, error) {
	if op, ok := asOperator(err); ok {
		return reject(op.Code, op.Detail)
	}
	var zero T
	if errors.Is(err, ErrMachineRetired) {
		return reject(OperatorCodeMachineRetired, "machine is retired")
	}
	if errors.Is(err, ErrNotFound) {
		return reject(OperatorCodeMachineNotFound, "machine was not found")
	}
	return zero, err
}

func diskCleanInvalid(err error) error {
	if errors.Is(err, maintenance.ErrInvalid) {
		return operatorError(OperatorCodeMaintenanceProfileInvalid, err.Error())
	}
	return err
}

func asOperator(err error) (*OperatorRequestError, bool) {
	var op *OperatorRequestError
	if errors.As(err, &op) {
		return op, true
	}
	return nil, false
}

func normalizeMachineIDs(ids []string) ([]string, error) {
	if len(ids) == 0 {
		return nil, errors.New("machine_ids is required")
	}
	if len(ids) > diskCleanMaxMachines {
		return nil, fmt.Errorf("machine_ids is limited to %d", diskCleanMaxMachines)
	}
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if strings.TrimSpace(id) == "" || len(id) > 128 {
			return nil, errors.New("machine_id is empty or too long")
		}
		if _, ok := seen[id]; ok {
			return nil, fmt.Errorf("machine_id %s is listed twice", id)
		}
		seen[id] = struct{}{}
	}
	sorted := append([]string(nil), ids...)
	sort.Strings(sorted)
	return sorted, nil
}

func joinSorted(ids []string) string {
	sorted := append([]string(nil), ids...)
	sort.Strings(sorted)
	return strings.Join(sorted, ",")
}

func diskCleanHasMachine(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
