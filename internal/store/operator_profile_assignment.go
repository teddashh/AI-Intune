package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"time"

	"github.com/teddashh/AI-Intune/internal/agentadapter"
	appcatalog "github.com/teddashh/AI-Intune/internal/catalog"
	"github.com/teddashh/AI-Intune/internal/deploy"
	"github.com/teddashh/AI-Intune/internal/model"
)

const (
	OperatorMachineProfileAssignmentDefaultTimeout = 600

	OperatorCodeProfileAssignmentInvalid      = "PROFILE_ASSIGNMENT_INVALID"
	OperatorCodeProfileAssignmentPreviewStale = "PROFILE_ASSIGNMENT_PREVIEW_STALE"

	operatorProfileAssignmentVersion            = "v1"
	operatorProfileAssignmentOperationPrefix    = "machine-profile-assign:v1:"
	operatorProfileAssignmentRejectPrefix       = "operator machine profile assignment rejection code="
	operatorProfileAssignmentCacheInvalid       = "machine profile assignment idempotency receipt is invalid"
	operatorProfileAssignmentCacheInvalidDetail = "machine profile assignment idempotency cache invalid；未回放 assignment、desired state 或 job"
	historicalProfileReplacementCode            = "PROFILE_ASSIGNMENT_REPLACEMENT_REQUIRED"
)

var (
	ErrProfileAssignmentInvalid      = errors.New("store: machine profile assignment request is invalid")
	ErrProfileAssignmentPreviewStale = errors.New("store: machine profile assignment preview is stale")
)

// OperatorMachineProfileAssignmentPrepared is the material verified by the
// operator service immediately before preview or apply. Spec remains internal;
// every public projection carries only its digest and immutable artifact ID.
type OperatorMachineProfileAssignmentPrepared struct {
	ProfileDigest string
	Target        appcatalog.Platform
	Packages      []OperatorMachineProfileAssignmentPreparedPackage
}

type OperatorMachineProfileAssignmentPreparedPackage struct {
	PackageID        string
	PackageVersion   string
	ManifestDigest   string
	ResourceKind     string
	ResourceID       string
	Spec             string
	ArtifactDigest   string
	ExecutionTimeout int
	Direct           bool
}

type OperatorMachineProfileAssignmentBlocker string

const (
	OperatorMachineProfileAssignmentBlockerRetired           OperatorMachineProfileAssignmentBlocker = "machine_retired"
	OperatorMachineProfileAssignmentBlockerNeverReported     OperatorMachineProfileAssignmentBlocker = "machine_never_reported"
	OperatorMachineProfileAssignmentBlockerExecutionUnknown  OperatorMachineProfileAssignmentBlocker = "agent_execution_unknown"
	OperatorMachineProfileAssignmentBlockerExecutionDisabled OperatorMachineProfileAssignmentBlocker = "agent_execution_disabled"
	OperatorMachineProfileAssignmentBlockerActiveJob         OperatorMachineProfileAssignmentBlocker = "nonterminal_jobs"
)

type OperatorMachineProfileAssignmentPackageImpact struct {
	Position                int             `json:"position"`
	PackageID               string          `json:"package_id"`
	PackageVersion          string          `json:"package_version"`
	ManifestDigest          string          `json:"manifest_digest"`
	ResourceKind            string          `json:"resource_kind"`
	ResourceID              string          `json:"resource_id"`
	SpecDigest              string          `json:"spec_digest"`
	ArtifactDigest          string          `json:"artifact_digest"`
	ExecutionTimeoutSeconds int             `json:"execution_timeout_seconds"`
	Direct                  bool            `json:"direct"`
	PrerequisitePackages    []string        `json:"prerequisite_packages"`
	CurrentRevision         deploy.Revision `json:"current_revision"`
	PlannedRevision         deploy.Revision `json:"planned_revision"`
}

type OperatorMachineProfileAssignmentImpact struct {
	ChangesMachineConfiguration bool                                            `json:"changes_machine_configuration"`
	CreatesAssignment           bool                                            `json:"creates_assignment"`
	CreatesDesiredStates        int                                             `json:"creates_desired_states"`
	CreatesJobs                 int                                             `json:"creates_jobs"`
	AlreadyAssigned             bool                                            `json:"already_assigned"`
	DeliveryRequiresJobsEnabled bool                                            `json:"delivery_requires_jobs_enabled"`
	JobsEnabled                 *bool                                           `json:"jobs_enabled"`
	EverReported                bool                                            `json:"ever_reported"`
	ActiveJobCount              int64                                           `json:"active_job_count"`
	CurrentAssignmentID         string                                          `json:"current_assignment_id,omitempty"`
	CurrentAssignmentRevision   int64                                           `json:"current_assignment_revision"`
	Packages                    []OperatorMachineProfileAssignmentPackageImpact `json:"packages"`
	Blockers                    []OperatorMachineProfileAssignmentBlocker       `json:"blockers"`
}

type OperatorMachineProfileAssignmentPreviewResult struct {
	MachineID         string              `json:"machine_id"`
	DisplayName       string              `json:"display_name"`
	LifecycleRevision int64               `json:"lifecycle_revision"`
	ProfileID         string              `json:"profile_id"`
	ProfileRevision   int64               `json:"profile_revision"`
	ProfileDigest     string              `json:"profile_digest"`
	Target            appcatalog.Platform `json:"target"`
	PreviewedAt       time.Time           `json:"previewed_at"`
	OperatorMachineProfileAssignmentImpact
	PreviewDigest string `json:"preview_digest"`
}

type OperatorMachineProfileAssignmentRequest struct {
	MachineID          string
	ProfileID          string
	ProfileRevision    int64
	ConfirmDisplayName string
	PreviewDigest      string
	Reason             string
	IdempotencyKey     string
	RequestDigest      string
	AssignedBy         string
	Audit              AuditEntry
}

type OperatorMachineProfileAssignmentPackageResult struct {
	OperatorMachineProfileAssignmentPackageImpact
	DesiredID string          `json:"desired_id"`
	JobID     string          `json:"job_id"`
	Revision  deploy.Revision `json:"revision"`
}

type OperatorMachineProfileAssignmentResult struct {
	AssignmentID       string                                          `json:"assignment_id"`
	AssignmentRevision int64                                           `json:"assignment_revision"`
	MachineID          string                                          `json:"machine_id"`
	DisplayName        string                                          `json:"display_name"`
	LifecycleRevision  int64                                           `json:"lifecycle_revision"`
	ProfileID          string                                          `json:"profile_id"`
	ProfileRevision    int64                                           `json:"profile_revision"`
	ProfileDigest      string                                          `json:"profile_digest"`
	Target             appcatalog.Platform                             `json:"target"`
	AssignedAt         time.Time                                       `json:"assigned_at"`
	Packages           []OperatorMachineProfileAssignmentPackageResult `json:"packages"`
	AlreadyAssigned    bool                                            `json:"already_assigned"`
	PreviewDigest      string                                          `json:"preview_digest"`
	Replayed           bool                                            `json:"replayed"`
	Audited            bool                                            `json:"-"`
}

type operatorProfileAssignmentSnapshot struct {
	MachineID         string
	DisplayName       string
	OS                string
	Arch              string
	LifecycleRevision int64
	Retired           bool
	EverReported      bool
	JobsEnabled       *bool
	ActiveJobCount    int64
	Current           *operatorProfileAssignmentCurrent
}

type operatorProfileAssignmentCurrent struct {
	AssignmentID       string
	AssignmentRevision int64
	ProfileID          string
	ProfileRevision    int64
	ProfileDigest      string
	Target             appcatalog.Platform
	PackageCount       int
	SucceededCount     int
	NonterminalCount   int
}

type operatorProfileAssignmentReceipt struct {
	SchemaVersion          string                                          `json:"schema_version"`
	AssignmentID           string                                          `json:"assignment_id"`
	AssignmentRevision     int64                                           `json:"assignment_revision"`
	MachineID              string                                          `json:"machine_id"`
	DisplayName            string                                          `json:"display_name"`
	LifecycleRevision      int64                                           `json:"lifecycle_revision"`
	ProfileID              string                                          `json:"profile_id"`
	ProfileRevision        int64                                           `json:"profile_revision"`
	ProfileDigest          string                                          `json:"profile_digest"`
	Target                 appcatalog.Platform                             `json:"target"`
	AssignedAt             time.Time                                       `json:"assigned_at"`
	AssignedBy             string                                          `json:"assigned_by"`
	SupersedesAssignmentID string                                          `json:"supersedes_assignment_id,omitempty"`
	Packages               []OperatorMachineProfileAssignmentPackageResult `json:"packages"`
	AlreadyAssigned        bool                                            `json:"already_assigned"`
	PreviewDigest          string                                          `json:"preview_digest"`
}

type machineJobsEnabledQueryer interface {
	Query(query string, args ...any) (*sql.Rows, error)
}

type operatorProfileAssignmentQueryer interface {
	machineJobsEnabledQueryer
	QueryRow(query string, args ...any) *sql.Row
}

var errMachineJobsEnabledProjectionInvalid = errors.New("store: machine jobs enabled projection is invalid")

// MachinePlatformIdentity returns the newest platform identity the Hub has
// accepted from this machine. Enrollment values are only the fallback: an
// agent observation is newer evidence and must not leave an initially blank
// registry identity looking unknown forever.
func (s *Store) MachinePlatformIdentity(machineID string) (string, string, error) {
	var registryOS, registryArch string
	err := s.rdb.QueryRow(`SELECT COALESCE(os,''),COALESCE(arch,'') FROM machine_registry WHERE machine_id=?`,
		machineID).Scan(&registryOS, &registryArch)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", ErrNotFound
	}
	if err != nil {
		return "", "", fmt.Errorf("store: read machine platform identity: %w", err)
	}
	return latestProfileAssignmentPlatformIdentity(s.rdb, machineID, registryOS, registryArch)
}

func latestProfileAssignmentPlatformIdentity(q operatorProfileAssignmentQueryer, machineID, registryOS,
	registryArch string,
) (string, string, error) {
	var payload string
	err := q.QueryRow(`SELECT payload FROM observed_state
 WHERE machine_id=? AND kind=? AND subject=? AND source=?
 ORDER BY received_at DESC,measured_at DESC,rowid DESC LIMIT 1`,
		machineID, KindIdentity, KindIdentity, SourceAgentMeasurement).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return registryOS, registryArch, nil
	}
	if err != nil {
		return "", "", fmt.Errorf("store: read machine platform observation: %w", err)
	}
	identity, ok := unmarshalInto[model.Identity](payload)
	if !ok {
		return "", "", errors.New("store: machine platform observation is invalid")
	}
	return identity.OS, identity.Arch, nil
}

func latestMachineJobsEnabled(q machineJobsEnabledQueryer, machineID string) (*bool, error) {
	rows, err := q.Query(`SELECT jobs_enabled,received_at,rowid FROM machine_checkins WHERE machine_id=?`, machineID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var latestJobsEnabled sql.NullBool
	var latestReceivedAt time.Time
	var latestRowID int64
	found := false
	for rows.Next() {
		var jobsEnabled sql.NullBool
		var receivedAtRaw string
		var rowID int64
		if err := rows.Scan(&jobsEnabled, &receivedAtRaw, &rowID); err != nil {
			return nil, err
		}
		receivedAt, err := time.Parse(time.RFC3339Nano, receivedAtRaw)
		if err != nil {
			return nil, errMachineJobsEnabledProjectionInvalid
		}
		if !found || receivedAt.After(latestReceivedAt) ||
			(receivedAt.Equal(latestReceivedAt) && rowID > latestRowID) {
			found = true
			latestJobsEnabled = jobsEnabled
			latestReceivedAt = receivedAt
			latestRowID = rowID
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if !found || !latestJobsEnabled.Valid {
		return nil, nil
	}
	value := latestJobsEnabled.Bool
	return &value, nil
}

func profileAssignmentOperation(machineID string) string {
	return operatorProfileAssignmentOperationPrefix + machineID
}

// PreviewOperatorMachineProfileAssignment binds the exact machine platform,
// immutable catalog graph, verified package material, revisions and current
// machine execution state into one review digest.
func (s *Store) PreviewOperatorMachineProfileAssignment(machineID, profileID string, profileRevision int64,
	prepared OperatorMachineProfileAssignmentPrepared,
) (OperatorMachineProfileAssignmentPreviewResult, error) {
	now := s.now().UTC().Truncate(time.Second)
	return s.previewOperatorMachineProfileAssignment(s.rdb, machineID, profileID, profileRevision, prepared, now)
}

func (s *Store) previewOperatorMachineProfileAssignment(q operatorProfileAssignmentQueryer,
	machineID, profileID string, profileRevision int64,
	prepared OperatorMachineProfileAssignmentPrepared, now time.Time,
) (OperatorMachineProfileAssignmentPreviewResult, error) {
	snapshot, err := loadOperatorProfileAssignmentSnapshot(q, machineID)
	if err != nil {
		return OperatorMachineProfileAssignmentPreviewResult{}, err
	}
	packages, profileDigest, err := validateOperatorProfileAssignmentPrepared(q, profileID, profileRevision, snapshot, prepared)
	if err != nil {
		return OperatorMachineProfileAssignmentPreviewResult{}, err
	}
	impact := OperatorMachineProfileAssignmentImpact{
		ChangesMachineConfiguration: true,
		CreatesAssignment:           true,
		CreatesDesiredStates:        len(packages),
		CreatesJobs:                 len(packages),
		DeliveryRequiresJobsEnabled: true,
		JobsEnabled:                 snapshot.JobsEnabled,
		EverReported:                snapshot.EverReported,
		ActiveJobCount:              snapshot.ActiveJobCount,
		Packages:                    packages,
		Blockers:                    make([]OperatorMachineProfileAssignmentBlocker, 0, 6),
	}
	if snapshot.Current != nil {
		impact.CurrentAssignmentID = snapshot.Current.AssignmentID
		impact.CurrentAssignmentRevision = snapshot.Current.AssignmentRevision
		same := snapshot.Current.ProfileID == profileID && snapshot.Current.ProfileRevision == profileRevision &&
			snapshot.Current.ProfileDigest == profileDigest && snapshot.Current.Target == prepared.Target &&
			snapshot.Current.PackageCount == len(packages)
		if same {
			matches, revisions, err := currentProfileAssignmentMatchesPlan(q, snapshot.Current.AssignmentID, packages)
			if err != nil {
				return OperatorMachineProfileAssignmentPreviewResult{}, err
			}
			if !matches {
				return OperatorMachineProfileAssignmentPreviewResult{}, errors.New("store: current profile assignment graph is invalid")
			}
			if snapshot.Current.SucceededCount == snapshot.Current.PackageCount && snapshot.Current.NonterminalCount == 0 {
				current := true
				for position, revision := range revisions {
					if packages[position].CurrentRevision != revision {
						current = false
						break
					}
				}
				if current {
					for position, revision := range revisions {
						packages[position].CurrentRevision = revision
						packages[position].PlannedRevision = revision
					}
					impact.ChangesMachineConfiguration = false
					impact.CreatesAssignment = false
					impact.CreatesDesiredStates = 0
					impact.CreatesJobs = 0
					impact.AlreadyAssigned = true
				}
			}
		}
	}
	if snapshot.Retired {
		impact.Blockers = append(impact.Blockers, OperatorMachineProfileAssignmentBlockerRetired)
	}
	if !snapshot.EverReported {
		impact.Blockers = append(impact.Blockers, OperatorMachineProfileAssignmentBlockerNeverReported)
	} else if snapshot.JobsEnabled == nil {
		impact.Blockers = append(impact.Blockers, OperatorMachineProfileAssignmentBlockerExecutionUnknown)
	} else if !*snapshot.JobsEnabled {
		impact.Blockers = append(impact.Blockers, OperatorMachineProfileAssignmentBlockerExecutionDisabled)
	}
	if snapshot.ActiveJobCount > 0 {
		impact.Blockers = append(impact.Blockers, OperatorMachineProfileAssignmentBlockerActiveJob)
	}
	result := OperatorMachineProfileAssignmentPreviewResult{
		MachineID: snapshot.MachineID, DisplayName: snapshot.DisplayName,
		LifecycleRevision: snapshot.LifecycleRevision,
		ProfileID:         profileID, ProfileRevision: profileRevision, ProfileDigest: profileDigest,
		Target: prepared.Target, PreviewedAt: now,
		OperatorMachineProfileAssignmentImpact: impact,
	}
	result.PreviewDigest = operatorProfileAssignmentPreviewDigest(result, snapshot.Retired)
	return result, nil
}

func currentProfileAssignmentMatchesPlan(q operatorProfileAssignmentQueryer, assignmentID string,
	packages []OperatorMachineProfileAssignmentPackageImpact,
) (bool, []deploy.Revision, error) {
	rows, err := q.Query(`SELECT p.position,p.package_id,p.package_version,p.manifest_digest,p.direct,
	 d.resource_kind,d.resource_id,d.revision,d.spec,j.artifact_digest,j.execution_timeout,p.job_id
	 FROM machine_profile_assignment_packages p
	 JOIN desired_state d ON d.desired_id=p.desired_id
	 JOIN jobs j ON j.job_id=p.job_id
	 WHERE p.assignment_id=? ORDER BY p.position`, assignmentID)
	if err != nil {
		return false, nil, err
	}
	type storedPackage struct {
		jobID    string
		revision deploy.Revision
	}
	stored := make([]storedPackage, 0, len(packages))
	position := 0
	for rows.Next() {
		if position >= len(packages) {
			_ = rows.Close()
			return false, nil, nil
		}
		var storedPosition int
		var packageID, version, manifestDigest, resourceKind, resourceID, spec, artifactDigest, jobID string
		var direct bool
		var timeout int
		var revision deploy.Revision
		if err := rows.Scan(&storedPosition, &packageID, &version, &manifestDigest, &direct,
			&resourceKind, &resourceID, &revision, &spec, &artifactDigest, &timeout, &jobID); err != nil {
			_ = rows.Close()
			return false, nil, err
		}
		planned := packages[position]
		if revision <= 0 || storedPosition != planned.Position || packageID != planned.PackageID || version != planned.PackageVersion ||
			manifestDigest != planned.ManifestDigest || direct != planned.Direct || resourceKind != planned.ResourceKind ||
			resourceID != planned.ResourceID || profileAssignmentSpecDigest(spec) != planned.SpecDigest ||
			artifactDigest != planned.ArtifactDigest || timeout != planned.ExecutionTimeoutSeconds {
			_ = rows.Close()
			return false, nil, nil
		}
		stored = append(stored, storedPackage{jobID: jobID, revision: revision})
		position++
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return false, nil, err
	}
	if err := rows.Close(); err != nil {
		return false, nil, err
	}
	if position != len(packages) {
		return false, nil, nil
	}
	revisions := make([]deploy.Revision, len(stored))
	for position, item := range stored {
		prerequisites, err := profileAssignmentPrerequisites(q, item.jobID)
		if err != nil {
			return false, nil, err
		}
		if !sameStringList(prerequisites, packages[position].PrerequisitePackages) {
			return false, nil, nil
		}
		revisions[position] = item.revision
	}
	return true, revisions, nil
}

func loadOperatorProfileAssignmentSnapshot(q operatorProfileAssignmentQueryer, machineID string) (operatorProfileAssignmentSnapshot, error) {
	var snapshot operatorProfileAssignmentSnapshot
	var retired sql.NullString
	var everReported int
	err := q.QueryRow(`SELECT display_name,COALESCE(os,''),COALESCE(arch,''),lifecycle_revision,retired_at,
 EXISTS(SELECT 1 FROM machine_checkins c WHERE c.machine_id=m.machine_id),
 (SELECT COUNT(*) FROM jobs j WHERE j.machine_id=m.machine_id AND j.state NOT IN (?,?,?,?,?))
 FROM machine_registry m WHERE machine_id=?`, deploy.Succeeded, deploy.Failed, deploy.Rejected,
		deploy.LeaseExpired, deploy.ManualIntervention, machineID).Scan(
		&snapshot.DisplayName, &snapshot.OS, &snapshot.Arch, &snapshot.LifecycleRevision,
		&retired, &everReported, &snapshot.ActiveJobCount)
	if errors.Is(err, sql.ErrNoRows) {
		return snapshot, operatorError(OperatorCodeMachineNotFound, "找不到這台機器")
	}
	if err != nil {
		return snapshot, fmt.Errorf("store: inspect profile assignment target: %w", err)
	}
	if snapshot.DisplayName == "" || snapshot.LifecycleRevision < 0 || snapshot.ActiveJobCount < 0 {
		return snapshot, errors.New("store: profile assignment target projection is invalid")
	}
	snapshot.OS, snapshot.Arch, err = latestProfileAssignmentPlatformIdentity(q, machineID, snapshot.OS, snapshot.Arch)
	if err != nil {
		return snapshot, err
	}
	snapshot.MachineID = machineID
	snapshot.Retired = retired.Valid && retired.String != ""
	snapshot.EverReported = everReported != 0
	snapshot.JobsEnabled, err = latestMachineJobsEnabled(q, machineID)
	if errors.Is(err, errMachineJobsEnabledProjectionInvalid) {
		return snapshot, errors.New("store: profile assignment target projection is invalid")
	}
	if err != nil {
		return snapshot, fmt.Errorf("store: inspect profile assignment check-in jobs state: %w", err)
	}
	var current operatorProfileAssignmentCurrent
	err = q.QueryRow(`SELECT a.assignment_id,a.assignment_revision,a.profile_id,a.profile_revision,
 a.profile_digest,a.target_os,a.target_arch,
 COUNT(p.job_id),COALESCE(SUM(CASE WHEN j.state=? THEN 1 ELSE 0 END),0),
 COALESCE(SUM(CASE WHEN j.state NOT IN (?,?,?,?,?) THEN 1 ELSE 0 END),0)
 FROM machine_profile_assignments a
 LEFT JOIN machine_profile_assignment_packages p ON p.assignment_id=a.assignment_id
 LEFT JOIN jobs j ON j.job_id=p.job_id
 WHERE a.machine_id=? AND a.assignment_revision=(
   SELECT MAX(newest.assignment_revision) FROM machine_profile_assignments newest WHERE newest.machine_id=a.machine_id)
 GROUP BY a.assignment_id,a.assignment_revision,a.profile_id,a.profile_revision,a.profile_digest,a.target_os,a.target_arch`,
		deploy.Succeeded, deploy.Succeeded, deploy.Failed, deploy.Rejected, deploy.LeaseExpired,
		deploy.ManualIntervention, machineID).Scan(
		&current.AssignmentID, &current.AssignmentRevision, &current.ProfileID, &current.ProfileRevision,
		&current.ProfileDigest, &current.Target.OS, &current.Target.Arch,
		&current.PackageCount, &current.SucceededCount, &current.NonterminalCount)
	if err == nil {
		if current.AssignmentID == "" || current.AssignmentRevision <= 0 || current.AssignmentRevision >= math.MaxInt64 ||
			current.ProfileID == "" ||
			current.ProfileRevision <= 0 || !validArtifactFetchDigest(current.ProfileDigest) ||
			current.Target.OS == "" || current.Target.Arch == "" || current.PackageCount <= 0 ||
			current.SucceededCount < 0 || current.SucceededCount > current.PackageCount ||
			current.NonterminalCount < 0 || current.NonterminalCount > current.PackageCount {
			return snapshot, errors.New("store: current profile assignment projection is invalid")
		}
		snapshot.Current = &current
	} else if !errors.Is(err, sql.ErrNoRows) {
		return snapshot, fmt.Errorf("store: inspect current profile assignment: %w", err)
	}
	return snapshot, nil
}

func validateOperatorProfileAssignmentPrepared(q operatorProfileAssignmentQueryer,
	profileID string, profileRevision int64, snapshot operatorProfileAssignmentSnapshot,
	prepared OperatorMachineProfileAssignmentPrepared,
) ([]OperatorMachineProfileAssignmentPackageImpact, string, error) {
	snapshotTarget, platformErr := appcatalog.PlatformFromProbeIdentity(snapshot.OS, snapshot.Arch)
	if profileID == "" || profileRevision <= 0 || platformErr != nil || prepared.Target != snapshotTarget ||
		!validArtifactFetchDigest(prepared.ProfileDigest) {
		return nil, "", profileAssignmentRejection(OperatorCodeProfileAssignmentInvalid)
	}
	profile, err := scanMachineProfile(q.QueryRow(`SELECT profile_json,profile_digest,published_at,published_by
	 FROM machine_profiles WHERE profile_id=? AND profile_revision=?`, profileID, profileRevision), profileID, profileRevision)
	if errors.Is(err, ErrNotFound) {
		return nil, "", profileAssignmentRejection(OperatorCodeMachineProfileUnresolvable)
	}
	if err != nil {
		return nil, "", err
	}
	if profile.Digest != prepared.ProfileDigest {
		return nil, "", profileAssignmentRejection(OperatorCodeProfileAssignmentPreviewStale)
	}
	manifestRecords, err := catalogManifestRecordsFrom(q)
	if err != nil {
		return nil, "", err
	}
	manifests := make([]appcatalog.Manifest, 0, len(manifestRecords))
	digests := make(map[string]string, len(manifestRecords))
	for _, record := range manifestRecords {
		manifests = append(manifests, record.Manifest)
		digests[profilePackageIdentity(record.Manifest.ID, record.Manifest.Version)] = record.Digest
	}
	index, err := appcatalog.New(manifests)
	if err != nil {
		return nil, "", fmt.Errorf("%w: %v", ErrCatalogRecordCorrupt, err)
	}
	plan, err := index.Resolve(profile.Profile, prepared.Target)
	if err != nil {
		return nil, "", profileAssignmentRejection(OperatorCodeMachineProfileUnresolvable)
	}
	if len(plan.Packages) == 0 || len(plan.Packages) != len(prepared.Packages) {
		return nil, "", profileAssignmentRejection(OperatorCodeProfileAssignmentPreviewStale)
	}
	resources := make(map[string]struct{}, len(plan.Packages))
	impacts := make([]OperatorMachineProfileAssignmentPackageImpact, 0, len(plan.Packages))
	for position, planned := range plan.Packages {
		manifest, candidate := planned.Manifest, prepared.Packages[position]
		identity := profilePackageIdentity(manifest.ID, manifest.Version)
		contract, supported := agentadapter.Lookup(manifest.Adapter)
		if !supported || !agentadapter.SupportsManifest(manifest) {
			return nil, "", profileAssignmentRejection(OperatorCodeCatalogAdapterUnsupported)
		}
		if candidate.PackageID != manifest.ID || candidate.PackageVersion != manifest.Version ||
			candidate.ManifestDigest != digests[identity] || candidate.ResourceKind != contract.ExecutorKind ||
			candidate.ResourceID != manifest.ID || candidate.ArtifactDigest != "sha256:"+manifest.Artifact.SHA256 ||
			candidate.ExecutionTimeout != OperatorMachineProfileAssignmentDefaultTimeout || candidate.Direct != planned.Direct ||
			!validPreparedAssignmentSpec(contract, manifest, prepared.Target, candidate.Spec) {
			return nil, "", profileAssignmentRejection(OperatorCodeProfileAssignmentPreviewStale)
		}
		resource := candidate.ResourceKind + ":" + candidate.ResourceID
		if _, duplicate := resources[resource]; duplicate {
			return nil, "", profileAssignmentRejection(OperatorCodeProfileAssignmentInvalid)
		}
		resources[resource] = struct{}{}
		var current deploy.Revision
		if err := q.QueryRow(`SELECT COALESCE((SELECT current_revision FROM revision_counters WHERE resource_scope=?),0)`, resource).Scan(&current); err != nil {
			return nil, "", fmt.Errorf("store: inspect profile package revision: %w", err)
		}
		if current < 0 || current >= deploy.Revision(math.MaxInt64) {
			return nil, "", errors.New("store: profile package revision projection is invalid")
		}
		prerequisites := make([]string, 0, len(manifest.Dependencies))
		for _, dependency := range manifest.Dependencies {
			prerequisites = append(prerequisites, profilePackageIdentity(dependency.PackageID, dependency.Version))
		}
		impacts = append(impacts, OperatorMachineProfileAssignmentPackageImpact{
			Position: position, PackageID: manifest.ID, PackageVersion: manifest.Version,
			ManifestDigest: candidate.ManifestDigest, ResourceKind: candidate.ResourceKind,
			ResourceID: candidate.ResourceID, SpecDigest: profileAssignmentSpecDigest(candidate.Spec),
			ArtifactDigest: candidate.ArtifactDigest, ExecutionTimeoutSeconds: candidate.ExecutionTimeout,
			Direct: candidate.Direct, PrerequisitePackages: prerequisites,
			CurrentRevision: current, PlannedRevision: current + 1,
		})
	}
	return impacts, profile.Digest, nil
}

func catalogManifestRecordsFrom(q operatorProfileAssignmentQueryer) ([]CatalogManifestRecord, error) {
	rows, err := q.Query(`SELECT package_id,package_version,manifest_json,manifest_digest,published_at,published_by
	 FROM catalog_manifests ORDER BY package_id,package_version LIMIT ?`, maxStoredCatalogRecords+1)
	if err != nil {
		return nil, fmt.Errorf("store: list profile assignment catalog: %w", err)
	}
	defer rows.Close()
	records := make([]CatalogManifestRecord, 0)
	for rows.Next() {
		var packageID, version, raw, digest, publishedAt, publishedBy string
		if err := rows.Scan(&packageID, &version, &raw, &digest, &publishedAt, &publishedBy); err != nil {
			return nil, err
		}
		record, err := decodeCatalogManifestRecord(packageID, version, raw, digest, publishedAt, publishedBy)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(records) > maxStoredCatalogRecords {
		return nil, fmt.Errorf("%w: catalog manifest count exceeds %d", ErrCatalogRecordCorrupt, maxStoredCatalogRecords)
	}
	return records, nil
}

func validPreparedAssignmentSpec(contract agentadapter.Contract, manifest appcatalog.Manifest,
	target appcatalog.Platform, raw string,
) bool {
	if raw == "" {
		return false
	}
	switch contract.ExecutorKind {
	case agentadapter.ExecutorKindOpenClaw:
		var spec model.OpenClawSpec
		if !decodeCanonicalProfileAssignmentSpec(raw, &spec) {
			return false
		}
		return spec.Kind == contract.ExecutorKind && spec.Version == manifest.Version && spec.Artifact != nil &&
			spec.Artifact.SHA256 == manifest.Artifact.SHA256 && spec.Artifact.Size == manifest.Artifact.Size &&
			spec.Artifact.URL == "/v1/artifacts/"+manifest.Artifact.SHA256 && spec.Artifact.EnginesNode != "" &&
			spec.Artifact.UpstreamTarball == "" && spec.Artifact.SHA512 == ""
	case agentadapter.ExecutorKindNodeRuntime:
		var spec model.NodeRuntimeSpec
		if !decodeCanonicalProfileAssignmentSpec(raw, &spec) {
			return false
		}
		return spec.Kind == contract.ExecutorKind && spec.Version == manifest.Version &&
			spec.TargetOS == target.OS && spec.TargetArch == target.Arch &&
			spec.BundleLayout == model.NodeRuntimeBundleLayoutV1 && spec.Artifact != nil &&
			spec.Artifact.SHA256 == manifest.Artifact.SHA256 && spec.Artifact.Size == manifest.Artifact.Size &&
			spec.Artifact.URL == "/v1/artifacts/"+manifest.Artifact.SHA256 && spec.Artifact.EnginesNode == "" &&
			spec.Artifact.UpstreamTarball == "" && spec.Artifact.SHA512 == ""
	case agentadapter.ExecutorKindHermes:
		var spec model.HermesSpec
		if !decodeCanonicalProfileAssignmentSpec(raw, &spec) {
			return false
		}
		return spec.Kind == contract.ExecutorKind && spec.Version == manifest.Version &&
			spec.TargetOS == target.OS && spec.TargetArch == target.Arch &&
			spec.BundleLayout == model.HermesOCIBundleLayoutV1 &&
			spec.ImageReference == "docker.io/nousresearch/hermes-agent:v"+manifest.Version &&
			validArtifactFetchDigest(spec.ImageIndexDigest) &&
			spec.Artifact != nil && spec.Artifact.SHA256 == manifest.Artifact.SHA256 &&
			spec.Artifact.Size == manifest.Artifact.Size &&
			spec.Artifact.URL == "/v1/artifacts/"+manifest.Artifact.SHA256 &&
			spec.Artifact.EnginesNode == "" && spec.Artifact.UpstreamTarball == "" && spec.Artifact.SHA512 == ""
	default:
		return false
	}
}

func decodeCanonicalProfileAssignmentSpec(raw string, target any) bool {
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return false
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return false
	}
	var canonical bytes.Buffer
	encoder := json.NewEncoder(&canonical)
	encoder.SetEscapeHTML(false)
	return encoder.Encode(target) == nil && strings.TrimSuffix(canonical.String(), "\n") == raw
}

func profilePackageIdentity(packageID, version string) string { return packageID + "@" + version }

func profileAssignmentSpecDigest(spec string) string {
	sum := sha256.Sum256([]byte(spec))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func operatorProfileAssignmentPreviewDigest(preview OperatorMachineProfileAssignmentPreviewResult, retired bool) string {
	body := struct {
		Version           string                                 `json:"version"`
		MachineID         string                                 `json:"machine_id"`
		DisplayName       string                                 `json:"display_name"`
		LifecycleRevision int64                                  `json:"lifecycle_revision"`
		Retired           bool                                   `json:"retired"`
		ProfileID         string                                 `json:"profile_id"`
		ProfileRevision   int64                                  `json:"profile_revision"`
		ProfileDigest     string                                 `json:"profile_digest"`
		Target            appcatalog.Platform                    `json:"target"`
		Impact            OperatorMachineProfileAssignmentImpact `json:"impact"`
	}{operatorProfileAssignmentVersion, preview.MachineID, preview.DisplayName,
		preview.LifecycleRevision, retired, preview.ProfileID, preview.ProfileRevision,
		preview.ProfileDigest, preview.Target, preview.OperatorMachineProfileAssignmentImpact}
	raw, _ := json.Marshal(body)
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func profileAssignmentRejection(code string) *OperatorRequestError {
	detail, ok := canonicalOperatorProfileAssignmentRejectionDetail(code)
	if !ok {
		detail = "profile assignment request rejected"
	}
	return operatorError(code, detail)
}

func canonicalOperatorProfileAssignmentRejectionDetail(code string) (string, bool) {
	switch code {
	case OperatorCodeProfileAssignmentInvalid:
		return "machine、profile 或 execution plan 不合法", true
	case OperatorCodeProfileAssignmentPreviewStale:
		return "machine、profile、material、job occupancy 或 revision 已變更；請重新預覽", true
	case OperatorCodeMachineNotFound:
		return "找不到這台機器", true
	case OperatorCodeMachineProfileUnresolvable:
		return "profile 無法在這台機器的平台解析", true
	case OperatorCodeMachinePlatformUnknown:
		return "機器的作業系統或架構資料無法辨識；請確認 agent 回報", true
	case OperatorCodeMachinePlatformUnsupported:
		return "agent 沒有支援這台機器平台的 adapter 或可執行套件；不可建立 profile job", true
	case OperatorCodeMachineProfileNotFound:
		return "找不到指定的 profile 版本；請重新選擇 profile", true
	case OperatorCodeCatalogAdapterUnsupported:
		return "profile 包含 agent 不支援的 adapter", true
	case OperatorCodeCatalogArtifactUnavailable:
		return "profile artifact bytes 不可用", true
	case OperatorCodeCatalogArtifactMismatch:
		return "profile artifact 與 manifest identity 不一致", true
	case OperatorCodeConfirmationMismatch:
		return "confirm_display_name 必須與目前顯示名稱逐字相同", true
	case OperatorCodePreviewRequired:
		return "preview_digest 不可省略；請先重新預覽", true
	case OperatorCodeMachineRetired:
		return "機器已退役", true
	case OperatorCodeNeverObserved:
		return "機器從未回報", true
	case OperatorCodeMachineActiveJob:
		return "機器已有未終態 job", true
	case OperatorCodeAgentExecutionUnknown:
		return "agent 尚未回報工作單執行狀態", true
	case OperatorCodeAgentExecutionDisabled:
		return "agent 工作單執行未啟用", true
	case OperatorCodeReasonRequired:
		return "reason 不可省略或只含空白", true
	default:
		return "", false
	}
}

func historicalOperatorProfileAssignmentRejectionDetail(code string) (string, bool) {
	if _, ok := storedOperatorProfileAssignmentRejectionDetail(code); !ok {
		return "", false
	}
	return "原 profile assignment request 當時以 " + code + " 拒絕", true
}

func storedOperatorProfileAssignmentRejectionDetail(code string) (string, bool) {
	if detail, ok := canonicalOperatorProfileAssignmentRejectionDetail(code); ok {
		return detail, true
	}
	if code == historicalProfileReplacementCode {
		return "目前已有不同 profile；請使用 replacement workflow", true
	}
	return "", false
}

type OperatorMachineProfileAssignmentPrepare func() (OperatorMachineProfileAssignmentPrepared, error)

// ApplyOperatorMachineProfileAssignment atomically creates the assignment,
// all desired states, all jobs, ordered dependency edges, replay receipt and
// audit. No package job can become visible without the complete graph.
func (s *Store) ApplyOperatorMachineProfileAssignment(req OperatorMachineProfileAssignmentRequest,
	prepare OperatorMachineProfileAssignmentPrepare,
) (OperatorMachineProfileAssignmentResult, error) {
	if strings.TrimSpace(req.IdempotencyKey) == "" || len(req.IdempotencyKey) > 200 {
		return OperatorMachineProfileAssignmentResult{}, operatorError(OperatorCodeIdempotencyKeyRequired, "Idempotency-Key 不可省略且最多 200 bytes")
	}
	if !validArtifactFetchDigest(req.RequestDigest) {
		return OperatorMachineProfileAssignmentResult{}, operatorError(OperatorCodeRequestDigestRequired, "request body digest 必須是 canonical sha256")
	}
	audit := req.Audit
	audit.Action, audit.MachineID, audit.Subject = AuditMachineProfileAssign, req.MachineID, req.MachineID
	audit.Reason, audit.IdempotencyKey, audit.RequestDigest = req.Reason, req.IdempotencyKey, req.RequestDigest

	lookupTx, err := s.beginWrite(context.Background(), "apply_operator_machine_profile_assignment")
	if err != nil {
		return OperatorMachineProfileAssignmentResult{}, fmt.Errorf("store: begin profile assignment idempotency lookup: %w", err)
	}
	cached, found, err := loadOperatorProfileAssignmentCached(lookupTx, req.IdempotencyKey)
	if err != nil {
		_ = lookupTx.Rollback()
		return OperatorMachineProfileAssignmentResult{}, err
	}
	if found {
		defer lookupTx.Rollback()
		return s.replayOperatorProfileAssignment(lookupTx, req, audit, cached)
	}
	if err := lookupTx.Rollback(); err != nil {
		return OperatorMachineProfileAssignmentResult{}, err
	}

	var prepared OperatorMachineProfileAssignmentPrepared
	var prepareErr error
	if prepare == nil {
		return OperatorMachineProfileAssignmentResult{}, errors.New("store: profile assignment prepare callback is nil")
	}
	prepared, prepareErr = prepare()

	tx, err := s.beginWrite(context.Background(), "apply_operator_machine_profile_assignment_2")
	if err != nil {
		return OperatorMachineProfileAssignmentResult{}, fmt.Errorf("store: begin profile assignment apply: %w", err)
	}
	defer tx.Rollback()
	now := s.now().UTC().Truncate(time.Second)
	audit.At = now
	cached, found, err = loadOperatorProfileAssignmentCached(tx, req.IdempotencyKey)
	if err != nil {
		return OperatorMachineProfileAssignmentResult{}, err
	}
	if found {
		return s.replayOperatorProfileAssignment(tx, req, audit, cached)
	}
	reject := func(code string) (OperatorMachineProfileAssignmentResult, error) {
		return s.rejectOperatorProfileAssignmentTx(tx, req, audit, code, now)
	}
	if prepareErr != nil {
		var rejection *OperatorRequestError
		if errors.As(prepareErr, &rejection) {
			if _, ok := canonicalOperatorProfileAssignmentRejectionDetail(rejection.Code); ok {
				return reject(rejection.Code)
			}
		}
		return OperatorMachineProfileAssignmentResult{}, fmt.Errorf("store: prepare profile assignment: %w", prepareErr)
	}
	if strings.TrimSpace(req.Reason) == "" || len(req.Reason) > auditMaxReason {
		return reject(OperatorCodeReasonRequired)
	}
	if !validArtifactFetchDigest(req.PreviewDigest) {
		return reject(OperatorCodePreviewRequired)
	}
	if !validCatalogPublisher(req.AssignedBy) {
		return reject(OperatorCodeProfileAssignmentInvalid)
	}
	preview, err := s.previewOperatorMachineProfileAssignment(tx, req.MachineID, req.ProfileID,
		req.ProfileRevision, prepared, now)
	if err != nil {
		var rejection *OperatorRequestError
		if errors.As(err, &rejection) {
			if _, ok := canonicalOperatorProfileAssignmentRejectionDetail(rejection.Code); ok {
				return reject(rejection.Code)
			}
		}
		return OperatorMachineProfileAssignmentResult{}, err
	}
	audit.Subject = preview.DisplayName
	if req.ConfirmDisplayName != preview.DisplayName {
		return reject(OperatorCodeConfirmationMismatch)
	}
	if req.PreviewDigest != preview.PreviewDigest {
		return reject(OperatorCodeProfileAssignmentPreviewStale)
	}
	for _, blocker := range preview.Blockers {
		switch blocker {
		case OperatorMachineProfileAssignmentBlockerRetired:
			return reject(OperatorCodeMachineRetired)
		case OperatorMachineProfileAssignmentBlockerNeverReported:
			return reject(OperatorCodeNeverObserved)
		case OperatorMachineProfileAssignmentBlockerExecutionUnknown:
			return reject(OperatorCodeAgentExecutionUnknown)
		case OperatorMachineProfileAssignmentBlockerExecutionDisabled:
			return reject(OperatorCodeAgentExecutionDisabled)
		case OperatorMachineProfileAssignmentBlockerActiveJob:
			return reject(OperatorCodeMachineActiveJob)
		default:
			return OperatorMachineProfileAssignmentResult{}, errors.New("store: unknown profile assignment blocker")
		}
	}

	receipt, err := s.mutateOperatorProfileAssignmentTx(tx, req, prepared, preview, now)
	if err != nil {
		return OperatorMachineProfileAssignmentResult{}, err
	}
	raw, err := json.Marshal(receipt)
	if err != nil {
		return OperatorMachineProfileAssignmentResult{}, err
	}
	if _, err := tx.Exec(`INSERT INTO operator_idempotency
	 (idempotency_key,operation,request_digest,outcome,response_json,created_at)
	 VALUES (?,?,?,'ok',?,?)`, req.IdempotencyKey, profileAssignmentOperation(req.MachineID),
		req.RequestDigest, string(raw), fmtTime(now)); err != nil {
		return OperatorMachineProfileAssignmentResult{}, fmt.Errorf("store: persist profile assignment receipt: %w", err)
	}
	audit.OK, audit.Detail = true, operatorProfileAssignmentSuccessDetail(receipt)
	if err := s.recordAuditTx(tx, audit); err != nil {
		return OperatorMachineProfileAssignmentResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return OperatorMachineProfileAssignmentResult{}, fmt.Errorf("store: commit profile assignment: %w", err)
	}
	result := operatorProfileAssignmentResult(receipt)
	result.Audited = true
	return result, nil
}

func (s *Store) mutateOperatorProfileAssignmentTx(tx dbTx, req OperatorMachineProfileAssignmentRequest,
	prepared OperatorMachineProfileAssignmentPrepared, preview OperatorMachineProfileAssignmentPreviewResult,
	now time.Time,
) (operatorProfileAssignmentReceipt, error) {
	if preview.AlreadyAssigned {
		current, err := operatorProfileAssignmentReceiptForCurrent(tx, preview)
		if err != nil {
			return operatorProfileAssignmentReceipt{}, err
		}
		current.AlreadyAssigned = true
		current.PreviewDigest = preview.PreviewDigest
		return current, nil
	}
	assignmentRevision := preview.CurrentAssignmentRevision + 1
	assignmentID := newID()
	if _, err := tx.Exec(`INSERT INTO machine_profile_assignments
	 (assignment_id,machine_id,assignment_revision,profile_id,profile_revision,profile_digest,
	  target_os,target_arch,assigned_at,assigned_by,supersedes_assignment_id)
	 VALUES (?,?,?,?,?,?,?,?,?,?,?)`, assignmentID, req.MachineID, assignmentRevision,
		req.ProfileID, req.ProfileRevision, preview.ProfileDigest, preview.Target.OS, preview.Target.Arch,
		fmtTime(now), req.AssignedBy, nullIfEmpty(preview.CurrentAssignmentID)); err != nil {
		return operatorProfileAssignmentReceipt{}, fmt.Errorf("store: create profile assignment: %w", err)
	}
	jobsByPackage := make(map[string]string, len(prepared.Packages))
	results := make([]OperatorMachineProfileAssignmentPackageResult, 0, len(prepared.Packages))
	for position, candidate := range prepared.Packages {
		impact := preview.Packages[position]
		desiredID, revision, err := createDesiredStateTx(tx, "machine", req.MachineID,
			candidate.ResourceKind, candidate.ResourceID, candidate.Spec, req.AssignedBy, now)
		if err != nil {
			return operatorProfileAssignmentReceipt{}, err
		}
		if revision != impact.PlannedRevision {
			return operatorProfileAssignmentReceipt{}, errors.New("store: profile package revision changed during transaction")
		}
		prerequisiteJobs := make([]string, 0, len(impact.PrerequisitePackages))
		for _, identity := range impact.PrerequisitePackages {
			jobID := jobsByPackage[identity]
			if jobID == "" {
				return operatorProfileAssignmentReceipt{}, errors.New("store: profile plan prerequisite order is invalid")
			}
			prerequisiteJobs = append(prerequisiteJobs, jobID)
		}
		jobID, err := createManagedJobTx(tx, req.MachineID, desiredID, revision, NewJob{
			ArtifactDigest: candidate.ArtifactDigest, ExecutionTimeout: candidate.ExecutionTimeout,
			PrerequisiteJobIDs: prerequisiteJobs,
		}, now)
		if err != nil {
			return operatorProfileAssignmentReceipt{}, err
		}
		if _, err := tx.Exec(`INSERT INTO machine_profile_assignment_packages
		 (assignment_id,position,package_id,package_version,manifest_digest,desired_id,job_id,direct)
		 VALUES (?,?,?,?,?,?,?,?)`, assignmentID, position, candidate.PackageID,
			candidate.PackageVersion, candidate.ManifestDigest, desiredID, jobID, candidate.Direct); err != nil {
			return operatorProfileAssignmentReceipt{}, fmt.Errorf("store: link profile assignment package: %w", err)
		}
		jobsByPackage[profilePackageIdentity(candidate.PackageID, candidate.PackageVersion)] = jobID
		results = append(results, OperatorMachineProfileAssignmentPackageResult{
			OperatorMachineProfileAssignmentPackageImpact: impact,
			DesiredID: desiredID, JobID: jobID, Revision: revision,
		})
	}
	return operatorProfileAssignmentReceipt{
		SchemaVersion: operatorProfileAssignmentVersion,
		AssignmentID:  assignmentID, AssignmentRevision: assignmentRevision,
		MachineID: req.MachineID, DisplayName: preview.DisplayName,
		LifecycleRevision: preview.LifecycleRevision,
		ProfileID:         req.ProfileID, ProfileRevision: req.ProfileRevision, ProfileDigest: preview.ProfileDigest,
		Target: preview.Target, AssignedAt: now, Packages: results,
		AssignedBy: req.AssignedBy, SupersedesAssignmentID: preview.CurrentAssignmentID,
		PreviewDigest: preview.PreviewDigest,
	}, nil
}

func operatorProfileAssignmentReceiptForCurrent(tx dbTx,
	preview OperatorMachineProfileAssignmentPreviewResult,
) (operatorProfileAssignmentReceipt, error) {
	var receipt operatorProfileAssignmentReceipt
	var assignedAt string
	err := tx.QueryRow(`SELECT assignment_id,assignment_revision,machine_id,profile_id,profile_revision,
	 profile_digest,target_os,target_arch,assigned_at,assigned_by,COALESCE(supersedes_assignment_id,'')
	 FROM machine_profile_assignments WHERE assignment_id=?`, preview.CurrentAssignmentID).Scan(
		&receipt.AssignmentID, &receipt.AssignmentRevision, &receipt.MachineID,
		&receipt.ProfileID, &receipt.ProfileRevision, &receipt.ProfileDigest,
		&receipt.Target.OS, &receipt.Target.Arch, &assignedAt, &receipt.AssignedBy,
		&receipt.SupersedesAssignmentID)
	if err != nil {
		return receipt, fmt.Errorf("store: read current profile assignment: %w", err)
	}
	receipt.SchemaVersion = operatorProfileAssignmentVersion
	receipt.DisplayName = preview.DisplayName
	receipt.LifecycleRevision = preview.LifecycleRevision
	receipt.AssignedAt = parseTime(assignedAt)
	rows, err := tx.Query(`SELECT p.position,p.package_id,p.package_version,p.manifest_digest,
	 p.desired_id,p.job_id,p.direct,d.resource_kind,d.resource_id,d.revision,d.spec,
	 j.artifact_digest,j.execution_timeout
	 FROM machine_profile_assignment_packages p
	 JOIN desired_state d ON d.desired_id=p.desired_id
	 JOIN jobs j ON j.job_id=p.job_id
	 WHERE p.assignment_id=? ORDER BY p.position`, receipt.AssignmentID)
	if err != nil {
		return receipt, err
	}
	for rows.Next() {
		var item OperatorMachineProfileAssignmentPackageResult
		var direct bool
		var spec, artifactDigest string
		if err := rows.Scan(&item.Position, &item.PackageID, &item.PackageVersion, &item.ManifestDigest,
			&item.DesiredID, &item.JobID, &direct, &item.ResourceKind, &item.ResourceID,
			&item.Revision, &spec, &artifactDigest, &item.ExecutionTimeoutSeconds); err != nil {
			return receipt, err
		}
		item.Direct = direct
		item.SpecDigest = profileAssignmentSpecDigest(spec)
		item.ArtifactDigest = artifactDigest
		item.CurrentRevision = item.Revision
		item.PlannedRevision = item.Revision
		receipt.Packages = append(receipt.Packages, item)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return receipt, err
	}
	if err := rows.Close(); err != nil {
		return receipt, err
	}
	for position := range receipt.Packages {
		edges, err := profileAssignmentPrerequisites(tx, receipt.Packages[position].JobID)
		if err != nil {
			return receipt, err
		}
		receipt.Packages[position].PrerequisitePackages = edges
	}
	return receipt, nil
}

func profileAssignmentPrerequisites(q operatorProfileAssignmentQueryer, jobID string) ([]string, error) {
	rows, err := q.Query(`SELECT parent.package_id,parent.package_version
	 FROM job_dependencies edge
	 JOIN machine_profile_assignment_packages child ON child.job_id=edge.job_id
	 JOIN machine_profile_assignment_packages parent ON parent.job_id=edge.prerequisite_job_id
	 WHERE edge.job_id=? AND parent.assignment_id=child.assignment_id ORDER BY edge.position`, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []string
	for rows.Next() {
		var id, version string
		if err := rows.Scan(&id, &version); err != nil {
			return nil, err
		}
		result = append(result, profilePackageIdentity(id, version))
	}
	return result, rows.Err()
}

func loadOperatorProfileAssignmentCached(q operatorRowQuerier, key string) (operatorCachedRequest, bool, error) {
	var cached operatorCachedRequest
	err := q.QueryRow(`SELECT operation,request_digest,outcome,response_json,error_code,error_detail,created_at
	 FROM operator_idempotency WHERE idempotency_key=?`, key).Scan(
		&cached.Operation, &cached.Digest, &cached.Outcome, &cached.ResponseJSON,
		&cached.ErrorCode, &cached.ErrorDetail, &cached.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return cached, false, nil
	}
	if err != nil {
		return cached, false, err
	}
	return cached, true, nil
}

func (s *Store) rejectOperatorProfileAssignmentTx(tx dbTx, req OperatorMachineProfileAssignmentRequest,
	audit AuditEntry, code string, now time.Time,
) (OperatorMachineProfileAssignmentResult, error) {
	detail, ok := canonicalOperatorProfileAssignmentRejectionDetail(code)
	if !ok {
		return OperatorMachineProfileAssignmentResult{}, errors.New("store: invalid profile assignment rejection code")
	}
	storedDetail := truncAudit(operatorProfileAssignmentRejectPrefix+code+"；"+detail, auditMaxReason)
	audit.OK, audit.Detail = false, storedDetail
	if err := s.recordAuditTx(tx, audit); err != nil {
		return OperatorMachineProfileAssignmentResult{}, err
	}
	if _, err := tx.Exec(`INSERT INTO operator_idempotency
	 (idempotency_key,operation,request_digest,outcome,error_code,error_detail,created_at)
	 VALUES (?,?,?,'rejected',?,?,?)`, req.IdempotencyKey, profileAssignmentOperation(req.MachineID),
		req.RequestDigest, code, storedDetail, fmtTime(now)); err != nil {
		return OperatorMachineProfileAssignmentResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return OperatorMachineProfileAssignmentResult{}, err
	}
	rejection := operatorError(code, detail)
	rejection.Audited = true
	return OperatorMachineProfileAssignmentResult{}, rejection
}

func (s *Store) replayOperatorProfileAssignment(tx dbTx, req OperatorMachineProfileAssignmentRequest,
	audit AuditEntry, cached operatorCachedRequest,
) (OperatorMachineProfileAssignmentResult, error) {
	audit.At = s.now().UTC().Truncate(time.Second)
	if cached.Operation != profileAssignmentOperation(req.MachineID) || cached.Digest != req.RequestDigest {
		audit.OK = false
		audit.Detail = "idempotency conflict：idempotency key 已被不同的 operation 或 canonical request body 使用"
		if err := s.recordAuditTx(tx, audit); err != nil {
			return OperatorMachineProfileAssignmentResult{}, err
		}
		if err := tx.Commit(); err != nil {
			return OperatorMachineProfileAssignmentResult{}, err
		}
		rejection := operatorError(OperatorCodeIdempotencyConflict, "idempotency key 已被不同的 operation 或 canonical request body 使用")
		rejection.Audited = true
		return OperatorMachineProfileAssignmentResult{}, rejection
	}
	if cached.Outcome == "rejected" {
		if cached.ResponseJSON.Valid || !cached.ErrorCode.Valid || !cached.ErrorDetail.Valid {
			return s.rejectInvalidOperatorProfileAssignmentCache(tx, audit)
		}
		canonical, ok := storedOperatorProfileAssignmentRejectionDetail(cached.ErrorCode.String)
		historical, historicalOK := historicalOperatorProfileAssignmentRejectionDetail(cached.ErrorCode.String)
		want := truncAudit(operatorProfileAssignmentRejectPrefix+cached.ErrorCode.String+"；"+canonical, auditMaxReason)
		if !ok || !historicalOK || cached.ErrorDetail.String != want {
			return s.rejectInvalidOperatorProfileAssignmentCache(tx, audit)
		}
		valid, err := validateOperatorProfileAssignmentAudit(tx, req, cached.CreatedAt, false, want)
		if err != nil || !valid {
			return s.rejectInvalidOperatorProfileAssignmentCache(tx, audit)
		}
		audit.OK, audit.Detail = false, OperatorIdempotencyReplayPrefix+"原判決："+historical
		if err := s.recordAuditTx(tx, audit); err != nil {
			return OperatorMachineProfileAssignmentResult{}, err
		}
		if err := tx.Commit(); err != nil {
			return OperatorMachineProfileAssignmentResult{}, err
		}
		return OperatorMachineProfileAssignmentResult{}, &OperatorRequestError{
			Code: cached.ErrorCode.String, Detail: historical, Replayed: true, Audited: true,
		}
	}
	if cached.Outcome != "ok" || !cached.ResponseJSON.Valid || cached.ErrorCode.Valid || cached.ErrorDetail.Valid {
		return s.rejectInvalidOperatorProfileAssignmentCache(tx, audit)
	}
	receipt, err := decodeOperatorProfileAssignmentReceipt(cached.ResponseJSON.String)
	if err != nil || !validOperatorProfileAssignmentReceipt(receipt, req, cached.CreatedAt) {
		return s.rejectInvalidOperatorProfileAssignmentCache(tx, audit)
	}
	valid, err := validateOperatorProfileAssignmentSuccessEvidence(tx, receipt, req)
	if err != nil || !valid {
		return s.rejectInvalidOperatorProfileAssignmentCache(tx, audit)
	}
	audit.Subject, audit.OK = receipt.DisplayName, true
	audit.Detail = OperatorIdempotencyReplayPrefix + "沒有再次建立 assignment、desired state、job 或 revision"
	if err := s.recordAuditTx(tx, audit); err != nil {
		return OperatorMachineProfileAssignmentResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return OperatorMachineProfileAssignmentResult{}, err
	}
	result := operatorProfileAssignmentResult(receipt)
	result.Replayed, result.Audited = true, true
	return result, nil
}

func (s *Store) rejectInvalidOperatorProfileAssignmentCache(tx dbTx,
	audit AuditEntry,
) (OperatorMachineProfileAssignmentResult, error) {
	// Never copy a corrupt cached value into the returned error or the audit:
	// a forged error_detail or JSON field could itself contain a credential.
	audit.OK, audit.Detail = false, operatorProfileAssignmentCacheInvalidDetail
	if err := s.recordAuditTx(tx, audit); err != nil {
		return OperatorMachineProfileAssignmentResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return OperatorMachineProfileAssignmentResult{}, err
	}
	return OperatorMachineProfileAssignmentResult{Audited: true},
		errors.New(operatorProfileAssignmentCacheInvalid)
}

func decodeOperatorProfileAssignmentReceipt(raw string) (operatorProfileAssignmentReceipt, error) {
	var receipt operatorProfileAssignmentReceipt
	decoder := json.NewDecoder(bytes.NewBufferString(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&receipt); err != nil {
		return receipt, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return receipt, errors.New("profile assignment receipt has trailing JSON")
	}
	canonical, err := json.Marshal(receipt)
	if err != nil || string(canonical) != raw {
		return receipt, errors.New("profile assignment receipt is not canonical JSON")
	}
	return receipt, nil
}

func validOperatorProfileAssignmentReceipt(receipt operatorProfileAssignmentReceipt,
	req OperatorMachineProfileAssignmentRequest, cachedAt string,
) bool {
	if receipt.SchemaVersion != operatorProfileAssignmentVersion || receipt.AssignmentID == "" ||
		receipt.AssignmentRevision <= 0 || receipt.LifecycleRevision < 0 ||
		receipt.MachineID != req.MachineID || receipt.DisplayName == "" ||
		receipt.ProfileID != req.ProfileID || receipt.ProfileRevision != req.ProfileRevision ||
		!validArtifactFetchDigest(receipt.ProfileDigest) || receipt.Target.OS == "" || receipt.Target.Arch == "" ||
		!validCatalogPublisher(receipt.AssignedBy) ||
		receipt.PreviewDigest != req.PreviewDigest ||
		!canonicalOperatorTime(receipt.AssignedAt) ||
		!canonicalOperatorDeploymentCacheTime(cachedAt) ||
		(!receipt.AlreadyAssigned && fmtTime(receipt.AssignedAt) != cachedAt) ||
		(receipt.AssignmentRevision == 1 && receipt.SupersedesAssignmentID != "") ||
		(receipt.AssignmentRevision > 1 && receipt.SupersedesAssignmentID == "") ||
		len(receipt.Packages) == 0 || len(receipt.Packages) > maxStoredCatalogRecords {
		return false
	}
	seen := make(map[string]struct{}, len(receipt.Packages))
	for position, item := range receipt.Packages {
		identity := profilePackageIdentity(item.PackageID, item.PackageVersion)
		if item.Position != position || identity == "@" || !validArtifactFetchDigest(item.ManifestDigest) ||
			item.ResourceKind == "" || item.ResourceID == "" || item.DesiredID == "" ||
			item.JobID == "" || item.Revision <= 0 || item.Revision != item.PlannedRevision ||
			item.CurrentRevision < 0 ||
			!validArtifactFetchDigest(item.SpecDigest) ||
			!validArtifactFetchDigest(item.ArtifactDigest) || item.ExecutionTimeoutSeconds <= 0 {
			return false
		}
		if receipt.AlreadyAssigned {
			if item.CurrentRevision != item.PlannedRevision {
				return false
			}
		} else if item.CurrentRevision+1 != item.PlannedRevision {
			return false
		}
		if _, duplicate := seen[identity]; duplicate {
			return false
		}
		seen[identity] = struct{}{}
		for _, prerequisite := range item.PrerequisitePackages {
			if _, exists := seen[prerequisite]; !exists {
				return false
			}
		}
	}
	return true
}

func validateOperatorProfileAssignmentSuccessEvidence(tx dbTx, receipt operatorProfileAssignmentReceipt,
	req OperatorMachineProfileAssignmentRequest,
) (bool, error) {
	var assignmentRows int
	err := tx.QueryRow(`SELECT COUNT(*) FROM machine_profile_assignments
	 WHERE assignment_id=? AND assignment_revision=? AND machine_id=? AND profile_id=? AND profile_revision=?
	 AND profile_digest=? AND target_os=? AND target_arch=? AND assigned_at=? AND assigned_by=?
	 AND COALESCE(supersedes_assignment_id,'')=?`, receipt.AssignmentID,
		receipt.AssignmentRevision, receipt.MachineID, receipt.ProfileID, receipt.ProfileRevision,
		receipt.ProfileDigest, receipt.Target.OS, receipt.Target.Arch, fmtTime(receipt.AssignedAt),
		receipt.AssignedBy, receipt.SupersedesAssignmentID).Scan(&assignmentRows)
	if err != nil || assignmentRows != 1 {
		return false, err
	}
	var packageRows int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM machine_profile_assignment_packages WHERE assignment_id=?`,
		receipt.AssignmentID).Scan(&packageRows); err != nil || packageRows != len(receipt.Packages) {
		return false, err
	}
	for _, item := range receipt.Packages {
		var rows int
		err := tx.QueryRow(`SELECT COUNT(*) FROM machine_profile_assignment_packages p
		 JOIN desired_state d ON d.desired_id=p.desired_id
		 JOIN jobs j ON j.job_id=p.job_id
		 WHERE p.assignment_id=? AND p.position=? AND p.package_id=? AND p.package_version=?
		 AND p.manifest_digest=? AND p.desired_id=? AND p.job_id=? AND p.direct=?
		 AND d.scope_type='machine' AND d.scope_id=? AND d.resource_kind=? AND d.resource_id=?
		 AND d.revision=? AND d.created_at=? AND d.created_by=?
		 AND j.machine_id=? AND j.desired_id=d.desired_id AND j.created_at=?
		 AND j.revision=d.revision AND j.artifact_digest=? AND j.execution_timeout=? AND j.irreversible=0`,
			receipt.AssignmentID, item.Position, item.PackageID, item.PackageVersion, item.ManifestDigest,
			item.DesiredID, item.JobID, item.Direct, receipt.MachineID, item.ResourceKind, item.ResourceID,
			item.Revision, fmtTime(receipt.AssignedAt), receipt.AssignedBy, receipt.MachineID,
			fmtTime(receipt.AssignedAt), item.ArtifactDigest,
			item.ExecutionTimeoutSeconds).Scan(&rows)
		if err != nil || rows != 1 {
			return false, err
		}
		var spec string
		if err := tx.QueryRow(`SELECT spec FROM desired_state WHERE desired_id=?`, item.DesiredID).Scan(&spec); err != nil ||
			profileAssignmentSpecDigest(spec) != item.SpecDigest {
			return false, err
		}
		prerequisites, err := profileAssignmentPrerequisites(tx, item.JobID)
		if err != nil || !sameStringList(prerequisites, item.PrerequisitePackages) {
			return false, err
		}
	}
	return validateOperatorProfileAssignmentAudit(tx, req, cachedTimestampForReceipt(receipt), true,
		operatorProfileAssignmentSuccessDetail(receipt))
}

func cachedTimestampForReceipt(receipt operatorProfileAssignmentReceipt) string {
	// A no-op receipt retains the original assignment time, not this call's audit_log.at;
	// the no-op audit is timestamped when it is written. Do not constrain audit.at with
	// receipt.AssignedAt; locate it by idempotency_key, request_digest, and detail.
	// Do not substitute the cache row's created_at, which describes the cache write instead.
	if receipt.AlreadyAssigned {
		return ""
	}
	return fmtTime(receipt.AssignedAt)
}

func validateOperatorProfileAssignmentAudit(tx dbTx, req OperatorMachineProfileAssignmentRequest,
	at string, ok bool, detail string,
) (bool, error) {
	query := `SELECT COUNT(*) FROM audit_log WHERE action=? AND machine_id=? AND COALESCE(reason,'')=?
	 AND idempotency_key=? AND request_digest=? AND outcome=? AND COALESCE(detail,'')=?`
	args := []any{string(AuditMachineProfileAssign), req.MachineID, truncAudit(req.Reason, auditMaxReason),
		req.IdempotencyKey, req.RequestDigest, outcomeOf(ok), truncAudit(detail, auditMaxReason)}
	if at != "" {
		query += ` AND at=?`
		args = append(args, at)
	}
	var count int
	err := tx.QueryRow(query, args...).Scan(&count)
	return count == 1, err
}

func sameStringList(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func operatorProfileAssignmentSuccessDetail(receipt operatorProfileAssignmentReceipt) string {
	return fmt.Sprintf("profile=%s@%d；assignment_id=%s；assignment_revision=%d；jobs=%d；already_assigned=%t",
		receipt.ProfileID, receipt.ProfileRevision, receipt.AssignmentID, receipt.AssignmentRevision,
		len(receipt.Packages), receipt.AlreadyAssigned)
}

func operatorProfileAssignmentResult(receipt operatorProfileAssignmentReceipt) OperatorMachineProfileAssignmentResult {
	// Empty prerequisites are an empty list on the HTTP contract, including
	// receipts reconstructed from current assignments or replayed from storage.
	packages := append([]OperatorMachineProfileAssignmentPackageResult{}, receipt.Packages...)
	for index := range packages {
		packages[index].PrerequisitePackages = append([]string{}, packages[index].PrerequisitePackages...)
	}
	return OperatorMachineProfileAssignmentResult{
		AssignmentID: receipt.AssignmentID, AssignmentRevision: receipt.AssignmentRevision,
		MachineID: receipt.MachineID, DisplayName: receipt.DisplayName,
		LifecycleRevision: receipt.LifecycleRevision,
		ProfileID:         receipt.ProfileID, ProfileRevision: receipt.ProfileRevision, ProfileDigest: receipt.ProfileDigest,
		Target: receipt.Target, AssignedAt: receipt.AssignedAt, Packages: packages,
		AlreadyAssigned: receipt.AlreadyAssigned, PreviewDigest: receipt.PreviewDigest,
	}
}
