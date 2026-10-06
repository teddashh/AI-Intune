package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/teddashh/AI-Intune/internal/deploy"
)

// Operator mutations use a separate error vocabulary from the machine-facing
// API. These are application-level rejections, not SQLite failures.
var (
	ErrIdempotencyKeyRequired          = errors.New("store: operator idempotency key is required")
	ErrRequestDigestRequired           = errors.New("store: operator request digest is required")
	ErrIdempotencyConflict             = errors.New("store: idempotency key was already used for another request")
	ErrPreconditionRequired            = errors.New("store: expected revision is required")
	ErrPreconditionFailed              = errors.New("store: machine channel revision does not match")
	ErrConfirmationMismatch            = errors.New("store: machine display-name confirmation does not match")
	ErrBadLifecycle                    = errors.New("store: machine lifecycle state is invalid")
	ErrLifecycleRevisionLimit          = errors.New("store: machine lifecycle revision cannot be incremented")
	ErrReasonRequired                  = errors.New("store: operator reason is required")
	ErrBadDiagnosticTimeout            = errors.New("store: diagnostic execution timeout is invalid")
	ErrDiagnosticPreviewStale          = errors.New("store: diagnostic preview is stale")
	ErrAgentExecutionUnknown           = errors.New("store: agent execution state is unknown")
	ErrAgentExecutionDisabled          = errors.New("store: agent execution is disabled")
	ErrBadDeviceSyncTimeout            = errors.New("store: device sync execution timeout is invalid")
	ErrDeviceSyncCapabilityUnconfirmed = errors.New("store: device sync capability is unconfirmed")
	ErrDeviceSyncPreviewStale          = errors.New("store: device sync preview is stale")
	ErrCatalogManifestInvalid          = errors.New("store: catalog manifest request is invalid")
	ErrCatalogAdapterUnsupported       = errors.New("store: catalog adapter is unsupported")
	ErrCatalogArtifactUnavailable      = errors.New("store: catalog artifact is unavailable")
	ErrCatalogArtifactMismatch         = errors.New("store: catalog artifact metadata does not match")
	ErrCatalogDependencyInvalid        = errors.New("store: catalog dependency is invalid")
	ErrMachineProfileInvalid           = errors.New("store: machine profile request is invalid")
	ErrMachineProfileUnresolvable      = errors.New("store: machine profile cannot be resolved")
	ErrMachinePlatformUnknown          = errors.New("store: machine platform identity is unknown")
	ErrMachinePlatformUnsupported      = errors.New("store: machine platform is unsupported")
	ErrMachineProfileNotFound          = errors.New("store: machine profile revision was not found")
	ErrMachineProfilePreviewStale      = errors.New("store: machine profile preview is stale")
	ErrTailnetPeerInvalid              = errors.New("store: tailnet peer request is invalid")
	ErrTailnetPeerNotFound             = errors.New("store: tailnet peer was not found")
	ErrTailnetPeerPreviewStale         = errors.New("store: tailnet peer preview is stale")
	ErrTailnetSourceUnavailable        = errors.New("store: tailnet source is unavailable")
	ErrRetentionPolicyInvalid          = errors.New("store: retention policy is invalid")
	ErrRetentionPreviewStale           = errors.New("store: retention preview is stale")
	ErrRetentionNothingToPrune         = errors.New("store: retention preview has no rows to prune")
	ErrRestoreDrillInvalid             = errors.New("store: restore drill request is invalid")
	ErrRestoreDrillPreviewStale        = errors.New("store: restore drill preview is stale")
	ErrRestoreDrillActive              = errors.New("store: restore drill is already active")
)

const (
	OperatorCodeIdempotencyKeyRequired             = "IDEMPOTENCY_KEY_REQUIRED"
	OperatorCodeRequestDigestRequired              = "REQUEST_DIGEST_REQUIRED"
	OperatorCodeIdempotencyConflict                = "IDEMPOTENCY_CONFLICT"
	OperatorCodePreconditionRequired               = "PRECONDITION_REQUIRED"
	OperatorCodePreconditionFailed                 = "PRECONDITION_FAILED"
	OperatorCodeConfirmationMismatch               = "CONFIRMATION_MISMATCH"
	OperatorCodeBadChannel                         = "BAD_CHANNEL"
	OperatorCodeMachineNotFound                    = "MACHINE_NOT_FOUND"
	OperatorCodeMachineRetired                     = "MACHINE_RETIRED"
	OperatorCodeNeverObserved                      = "MACHINE_NEVER_OBSERVED"
	OperatorCodeMachineActiveJob                   = "MACHINE_HAS_ACTIVE_JOB"
	OperatorCodeBadLifecycle                       = "BAD_LIFECYCLE"
	OperatorCodeLifecycleRevisionLimit             = "LIFECYCLE_REVISION_EXHAUSTED"
	OperatorCodeReasonRequired                     = "REASON_REQUIRED"
	OperatorCodeBadDiagnosticTimeout               = "BAD_DIAGNOSTIC_TIMEOUT"
	OperatorCodeDiagnosticPreviewStale             = "DIAGNOSTIC_PREVIEW_STALE"
	OperatorCodeAgentExecutionUnknown              = "AGENT_EXECUTION_UNKNOWN"
	OperatorCodeAgentExecutionDisabled             = "AGENT_EXECUTION_DISABLED"
	OperatorCodeBadDeviceSyncTimeout               = "BAD_DEVICE_SYNC_TIMEOUT"
	OperatorCodeDeviceSyncCapabilityUnconfirmed    = "DEVICE_SYNC_CAPABILITY_UNCONFIRMED"
	OperatorCodeDeviceSyncPreviewStale             = "DEVICE_SYNC_PREVIEW_STALE"
	OperatorCodeCatalogManifestInvalid             = "CATALOG_MANIFEST_INVALID"
	OperatorCodeCatalogAdapterUnsupported          = "CATALOG_ADAPTER_UNSUPPORTED"
	OperatorCodeCatalogArtifactUnavailable         = "CATALOG_ARTIFACT_UNAVAILABLE"
	OperatorCodeCatalogArtifactMismatch            = "CATALOG_ARTIFACT_MISMATCH"
	OperatorCodeCatalogManifestConflict            = "CATALOG_MANIFEST_CONFLICT"
	OperatorCodeCatalogCapacity                    = "CATALOG_CAPACITY"
	OperatorCodeCatalogPreviewStale                = "CATALOG_PREVIEW_STALE"
	OperatorCodeCatalogConfirmationMismatch        = "CATALOG_CONFIRMATION_MISMATCH"
	OperatorCodeCatalogDependencyInvalid           = "CATALOG_DEPENDENCY_INVALID"
	OperatorCodeMachineProfileInvalid              = "MACHINE_PROFILE_INVALID"
	OperatorCodeMachineProfileUnresolvable         = "MACHINE_PROFILE_UNRESOLVABLE"
	OperatorCodeMachinePlatformUnknown             = "MACHINE_PLATFORM_UNKNOWN"
	OperatorCodeMachinePlatformUnsupported         = "MACHINE_PLATFORM_UNSUPPORTED"
	OperatorCodeMachineProfileNotFound             = "MACHINE_PROFILE_NOT_FOUND"
	OperatorCodeMachineProfileConflict             = "MACHINE_PROFILE_CONFLICT"
	OperatorCodeMachineProfileCapacity             = "MACHINE_PROFILE_CAPACITY"
	OperatorCodeMachineProfilePreviewStale         = "MACHINE_PROFILE_PREVIEW_STALE"
	OperatorCodeMachineProfileConfirmationMismatch = "MACHINE_PROFILE_CONFIRMATION_MISMATCH"
	OperatorCodeTailnetPeerInvalid                 = "TAILNET_PEER_INVALID"
	OperatorCodeTailnetPeerNotFound                = "TAILNET_PEER_NOT_FOUND"
	OperatorCodeTailnetPeerPreviewStale            = "TAILNET_PEER_PREVIEW_STALE"
	OperatorCodeTailnetSourceUnavailable           = "TAILNET_SOURCE_UNAVAILABLE"
	OperatorCodeRetentionPolicyInvalid             = "RETENTION_POLICY_INVALID"
	OperatorCodeRetentionPreviewStale              = "RETENTION_PREVIEW_STALE"
	OperatorCodeRetentionNothingToPrune            = "RETENTION_NOTHING_TO_PRUNE"
	OperatorCodeRestoreDrillInvalid                = "RESTORE_DRILL_INVALID"
	OperatorCodeRestoreDrillPreviewStale           = "RESTORE_DRILL_PREVIEW_STALE"
	OperatorCodeRestoreDrillActive                 = "RESTORE_DRILL_ACTIVE"
	OperatorCodeSettingPolicyInvalid               = "SETTING_POLICY_INVALID"
	OperatorCodeSettingPolicyConflict              = "SETTING_POLICY_CONFLICT"
	OperatorCodeSettingPolicyNotFound              = "SETTING_POLICY_NOT_FOUND"
	OperatorCodeSettingPreviewStale                = "SETTING_PREVIEW_STALE"
	OperatorCodeSettingScopeInvalid                = "SETTING_SCOPE_INVALID"
	OperatorCodeCompliancePolicyInvalid            = "COMPLIANCE_POLICY_INVALID"
	OperatorCodeCompliancePolicyConflict           = "COMPLIANCE_POLICY_CONFLICT"
	OperatorCodeCompliancePolicyNotFound           = "COMPLIANCE_POLICY_NOT_FOUND"
	OperatorCodeCompliancePreviewStale             = "COMPLIANCE_PREVIEW_STALE"
	OperatorCodeComplianceScopeInvalid             = "COMPLIANCE_SCOPE_INVALID"
)

// OperatorRequestError is safe to translate into a stable JSON API error.
// Replayed says the same key and body already received this exact rejection.
type OperatorRequestError struct {
	Code     string
	Detail   string
	Replayed bool
	Audited  bool
}

func (e *OperatorRequestError) Error() string { return e.Detail }

func (e *OperatorRequestError) Unwrap() error {
	switch e.Code {
	case OperatorCodeIdempotencyKeyRequired:
		return ErrIdempotencyKeyRequired
	case OperatorCodeRequestDigestRequired:
		return ErrRequestDigestRequired
	case OperatorCodeIdempotencyConflict:
		return ErrIdempotencyConflict
	case OperatorCodePreconditionRequired:
		return ErrPreconditionRequired
	case OperatorCodePreconditionFailed:
		return ErrPreconditionFailed
	case OperatorCodeConfirmationMismatch:
		return ErrConfirmationMismatch
	case OperatorCodeBadChannel:
		return ErrBadChannel
	case OperatorCodeMachineNotFound:
		return ErrNotFound
	case OperatorCodeMachineRetired:
		return ErrMachineRetired
	case OperatorCodeNeverObserved:
		return ErrNeverObserved
	case OperatorCodeMachineActiveJob:
		return ErrMachineActiveJob
	case OperatorCodeBadLifecycle:
		return ErrBadLifecycle
	case OperatorCodeLifecycleRevisionLimit:
		return ErrLifecycleRevisionLimit
	case OperatorCodeReasonRequired:
		return ErrReasonRequired
	case OperatorCodeBadDiagnosticTimeout:
		return ErrBadDiagnosticTimeout
	case OperatorCodeDiagnosticPreviewStale:
		return ErrDiagnosticPreviewStale
	case OperatorCodeAgentExecutionUnknown:
		return ErrAgentExecutionUnknown
	case OperatorCodeAgentExecutionDisabled:
		return ErrAgentExecutionDisabled
	case OperatorCodeBadDeviceSyncTimeout:
		return ErrBadDeviceSyncTimeout
	case OperatorCodeDeviceSyncCapabilityUnconfirmed:
		return ErrDeviceSyncCapabilityUnconfirmed
	case OperatorCodeDeviceSyncPreviewStale:
		return ErrDeviceSyncPreviewStale
	case OperatorCodeEnrollTokenNotPending:
		return ErrEnrollTokenNotPending
	case OperatorCodeBadDisplayName:
		return ErrBadDisplayName
	case OperatorCodeBadTTL:
		return ErrBadEnrollTTL
	case OperatorCodePreviewRequired:
		return ErrPreviewRequired
	case OperatorCodePreviewStale:
		return ErrPreviewStale
	case OperatorCodeDeploymentPreviewStale:
		return ErrDeploymentPreviewStale
	case OperatorCodeDeploymentPreconditionFailed:
		return ErrDeploymentPreconditionFailed
	case OperatorCodeDeploymentContinueRefused:
		return ErrDeploymentContinueRefused
	case OperatorCodeDeploymentNotFound:
		return ErrDeploymentNotFound
	case OperatorCodeDeploymentNotPaused:
		return ErrDeploymentNotPaused
	case OperatorCodeDeploymentActiveJobs:
		return ErrDeploymentActiveJobs
	case OperatorCodeDeploymentNoRetryTargets:
		return ErrDeploymentNoRetryTargets
	case OperatorCodeDeploymentNoIncludedTargets:
		return ErrDeploymentNoIncludedTargets
	case OperatorCodeDeploymentPromotionBlocked:
		return ErrPromoteLocked
	case OperatorCodeDeploymentConfirmationMismatch:
		return ErrDeploymentConfirmationMismatch
	case OperatorCodeArtifactFetchInvalid:
		return ErrArtifactFetchInvalid
	case OperatorCodeArtifactFetchPrepareFailed:
		return ErrArtifactFetchPrepareFailed
	case OperatorCodeArtifactFetchPreviewStale:
		return ErrArtifactFetchPreviewStale
	case OperatorCodeArtifactFetchActive:
		return ErrArtifactFetchActive
	case OperatorCodeCatalogManifestInvalid:
		return ErrCatalogManifestInvalid
	case OperatorCodeCatalogAdapterUnsupported:
		return ErrCatalogAdapterUnsupported
	case OperatorCodeCatalogArtifactUnavailable:
		return ErrCatalogArtifactUnavailable
	case OperatorCodeCatalogArtifactMismatch:
		return ErrCatalogArtifactMismatch
	case OperatorCodeCatalogManifestConflict:
		return ErrCatalogManifestConflict
	case OperatorCodeCatalogCapacity:
		return ErrCatalogCapacity
	case OperatorCodeCatalogPreviewStale:
		return ErrPreconditionFailed
	case OperatorCodeCatalogConfirmationMismatch:
		return ErrPreconditionRequired
	case OperatorCodeCatalogDependencyInvalid:
		return ErrCatalogDependencyInvalid
	case OperatorCodeMachineProfileInvalid:
		return ErrMachineProfileInvalid
	case OperatorCodeMachineProfileUnresolvable:
		return ErrMachineProfileUnresolvable
	case OperatorCodeMachinePlatformUnknown:
		return ErrMachinePlatformUnknown
	case OperatorCodeMachinePlatformUnsupported:
		return ErrMachinePlatformUnsupported
	case OperatorCodeMachineProfileNotFound:
		return ErrMachineProfileNotFound
	case OperatorCodeMachineProfileConflict:
		return ErrMachineProfileConflict
	case OperatorCodeMachineProfileCapacity:
		return ErrCatalogCapacity
	case OperatorCodeMachineProfilePreviewStale:
		return ErrMachineProfilePreviewStale
	case OperatorCodeMachineProfileConfirmationMismatch:
		return ErrPreconditionRequired
	case OperatorCodeProfileAssignmentInvalid:
		return ErrProfileAssignmentInvalid
	case OperatorCodeProfileAssignmentPreviewStale:
		return ErrProfileAssignmentPreviewStale
	case OperatorCodeTailnetPeerInvalid:
		return ErrTailnetPeerInvalid
	case OperatorCodeTailnetPeerNotFound:
		return ErrTailnetPeerNotFound
	case OperatorCodeTailnetPeerPreviewStale:
		return ErrTailnetPeerPreviewStale
	case OperatorCodeTailnetSourceUnavailable:
		return ErrTailnetSourceUnavailable
	case OperatorCodeRetentionPolicyInvalid:
		return ErrRetentionPolicyInvalid
	case OperatorCodeRetentionPreviewStale:
		return ErrRetentionPreviewStale
	case OperatorCodeRetentionNothingToPrune:
		return ErrRetentionNothingToPrune
	case OperatorCodeRestoreDrillInvalid:
		return ErrRestoreDrillInvalid
	case OperatorCodeRestoreDrillPreviewStale:
		return ErrRestoreDrillPreviewStale
	case OperatorCodeRestoreDrillActive:
		return ErrRestoreDrillActive
	case OperatorCodeVerifierInvalid, OperatorCodeVerifierDomainInvalid:
		return ErrInvalidVerifier
	case OperatorCodeVerifierNameTaken:
		return ErrVerifierNameTaken
	case OperatorCodeVerifierNotFound:
		return ErrVerifierNotFound
	case OperatorCodeVerifierAlreadyRevoked:
		return ErrVerifierRevoked
	case OperatorCodeVerifierPreviewStale:
		return ErrPreviewStale
	case OperatorCodeMachineRenameInvalid:
		return ErrMachineRenameInvalid
	case OperatorCodeMachineRenameUnchanged:
		return ErrMachineRenameUnchanged
	case OperatorCodeMachineRenameNameTaken:
		return ErrMachineRenameNameTaken
	case OperatorCodeMachineRenamePreviewRequired:
		return ErrMachineRenamePreviewRequired
	case OperatorCodeMachineRenamePreviewStale:
		return ErrMachineRenamePreviewStale
	case OperatorCodeMachineRenameConfirmationMismatch:
		return ErrMachineRenameConfirmationMismatch
	case OperatorCodeMachineNotesInvalid:
		return ErrMachineNotesInvalid
	case OperatorCodeMachineNotesUnchanged:
		return ErrMachineNotesUnchanged
	case OperatorCodeMachineNotesPreviewRequired:
		return ErrMachineNotesPreviewRequired
	case OperatorCodeMachineNotesPreviewStale:
		return ErrMachineNotesPreviewStale
	case OperatorCodeMachineNotesConfirmationMismatch:
		return ErrMachineNotesConfirmationMismatch
	default:
		return nil
	}
}

type OperatorMachineChannelRequest struct {
	MachineID          string
	Channel            string
	ExpectedRevision   *int64
	ConfirmDisplayName string
	IdempotencyKey     string
	RequestDigest      string
	Audit              AuditEntry
}

type OperatorMachineChannelResult struct {
	MachineID       string `json:"machine_id"`
	DisplayName     string `json:"display_name"`
	PreviousChannel string `json:"previous_channel"`
	Channel         string `json:"channel"`
	Revision        int64  `json:"revision"`
	Replayed        bool   `json:"replayed"`
	Audited         bool   `json:"-"`
}

type operatorCachedRequest struct {
	Operation    string
	Digest       string
	Outcome      string
	ResponseJSON sql.NullString
	ErrorCode    sql.NullString
	ErrorDetail  sql.NullString
	CreatedAt    string
}

// ApplyOperatorMachineChannel is the one transactional authority for operator
// channel changes. It persists both successful results and domain rejections,
// so retrying a request cannot acquire new meaning after fleet state changes.
func (s *Store) ApplyOperatorMachineChannel(req OperatorMachineChannelRequest) (OperatorMachineChannelResult, error) {
	if strings.TrimSpace(req.IdempotencyKey) == "" {
		return OperatorMachineChannelResult{}, operatorError(
			OperatorCodeIdempotencyKeyRequired, "Idempotency-Key 不可省略")
	}
	if len(req.IdempotencyKey) > 200 {
		return OperatorMachineChannelResult{}, operatorError(
			OperatorCodeIdempotencyKeyRequired, "Idempotency-Key 最多 200 bytes")
	}
	if req.RequestDigest == "" {
		return OperatorMachineChannelResult{}, operatorError(
			OperatorCodeRequestDigestRequired, "request body digest 不可省略")
	}

	operation := "machine-channel:" + req.MachineID
	audit := req.Audit
	audit.Action = AuditMachineChannel
	audit.MachineID = req.MachineID
	// Correlation identity is authoritative request data, never caller-supplied
	// audit decoration. Overwrite it so the two ledgers always join exactly.
	audit.IdempotencyKey = req.IdempotencyKey
	audit.RequestDigest = req.RequestDigest
	if audit.Subject == "" {
		audit.Subject = req.MachineID
	}
	tx, err := s.db.Begin()
	if err != nil {
		return OperatorMachineChannelResult{}, fmt.Errorf("store: begin operator machine channel: %w", err)
	}
	defer tx.Rollback()

	var cached operatorCachedRequest
	err = tx.QueryRow(`SELECT operation,request_digest,outcome,response_json,error_code,error_detail
	 FROM operator_idempotency WHERE idempotency_key=?`, req.IdempotencyKey).Scan(
		&cached.Operation, &cached.Digest, &cached.Outcome, &cached.ResponseJSON,
		&cached.ErrorCode, &cached.ErrorDetail)
	switch {
	case err == nil:
		if cached.Operation != operation || cached.Digest != req.RequestDigest {
			detail := "idempotency key 已被不同的 operation 或 canonical request body 使用"
			auditTarget := req.Channel
			if auditTarget == "none" {
				auditTarget = ""
			}
			populateMachineChannelAudit(tx, &audit, req.MachineID, auditTarget)
			audit.OK, audit.Detail = false, "idempotency conflict："+detail
			if err := s.recordAuditTx(tx, audit); err != nil {
				return OperatorMachineChannelResult{}, fmt.Errorf("store: record operator idempotency conflict audit: %w", err)
			}
			if err := tx.Commit(); err != nil {
				return OperatorMachineChannelResult{}, fmt.Errorf("store: commit operator idempotency conflict audit: %w", err)
			}
			rejection := operatorError(OperatorCodeIdempotencyConflict, detail)
			rejection.Audited = true
			return OperatorMachineChannelResult{}, rejection
		}
		if cached.Outcome == "rejected" {
			if !cached.ErrorCode.Valid || cached.ErrorCode.String == "" || !cached.ErrorDetail.Valid {
				return OperatorMachineChannelResult{}, errors.New("store: cached operator rejection is incomplete")
			}
			auditTarget := req.Channel
			if auditTarget == "none" {
				auditTarget = ""
			}
			populateMachineChannelAudit(tx, &audit, req.MachineID, auditTarget)
			audit.OK = false
			audit.Detail = OperatorIdempotencyReplayPrefix + "原判決：" + cached.ErrorDetail.String
			if err := s.recordAuditTx(tx, audit); err != nil {
				return OperatorMachineChannelResult{}, fmt.Errorf("store: record rejected operator replay audit: %w", err)
			}
			if err := tx.Commit(); err != nil {
				return OperatorMachineChannelResult{}, fmt.Errorf("store: commit rejected operator replay audit: %w", err)
			}
			return OperatorMachineChannelResult{}, &OperatorRequestError{
				Code: cached.ErrorCode.String, Detail: cached.ErrorDetail.String, Replayed: true, Audited: true,
			}
		}
		if cached.Outcome != "ok" || !cached.ResponseJSON.Valid {
			return OperatorMachineChannelResult{}, fmt.Errorf("store: invalid cached operator outcome %q", cached.Outcome)
		}
		var result OperatorMachineChannelResult
		if err := json.Unmarshal([]byte(cached.ResponseJSON.String), &result); err != nil {
			return OperatorMachineChannelResult{}, fmt.Errorf("store: decode cached machine channel response: %w", err)
		}
		result.Replayed = true
		audit.Subject = result.DisplayName
		audit.Reason = auditChannelLabel(result.PreviousChannel) + " → " + auditChannelLabel(result.Channel)
		audit.OK = true
		audit.Detail = OperatorIdempotencyReplayPrefix + "沒有再次改 state 或 revision"
		if err := s.recordAuditTx(tx, audit); err != nil {
			return OperatorMachineChannelResult{}, fmt.Errorf("store: record successful operator replay audit: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return OperatorMachineChannelResult{}, fmt.Errorf("store: commit successful operator replay audit: %w", err)
		}
		result.Audited = true
		return result, nil
	case !errors.Is(err, sql.ErrNoRows):
		return OperatorMachineChannelResult{}, fmt.Errorf("store: inspect operator idempotency key: %w", err)
	}

	reject := func(code, detail string) (OperatorMachineChannelResult, error) {
		audit.OK, audit.Detail = false, detail
		if err := s.recordAuditTx(tx, audit); err != nil {
			return OperatorMachineChannelResult{}, fmt.Errorf("store: record operator rejection audit: %w", err)
		}
		if _, err := tx.Exec(`INSERT INTO operator_idempotency
		 (idempotency_key,operation,request_digest,outcome,error_code,error_detail,created_at)
		 VALUES (?,?,?,'rejected',?,?,?)`, req.IdempotencyKey, operation, req.RequestDigest,
			code, detail, fmtTime(s.now())); err != nil {
			return OperatorMachineChannelResult{}, fmt.Errorf("store: persist operator rejection: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return OperatorMachineChannelResult{}, fmt.Errorf("store: commit operator rejection: %w", err)
		}
		rejection := operatorError(code, detail)
		rejection.Audited = true
		return OperatorMachineChannelResult{}, rejection
	}

	var targetChannel string
	switch req.Channel {
	case "canary", "stable":
		targetChannel = req.Channel
	case "none":
		targetChannel = ""
	default:
		return reject(OperatorCodeBadChannel, "channel 只接受 canary、stable 或 none")
	}
	if req.ExpectedRevision == nil {
		return reject(OperatorCodePreconditionRequired, "expected_revision 不可省略")
	}

	var displayName string
	var current, retiredAt sql.NullString
	var revision int64
	if err := tx.QueryRow(`SELECT display_name,channel,channel_revision,retired_at
	 FROM machine_registry WHERE machine_id=?`, req.MachineID).Scan(&displayName, &current, &revision, &retiredAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return reject(OperatorCodeMachineNotFound, "找不到 machine "+req.MachineID)
		}
		return OperatorMachineChannelResult{}, fmt.Errorf("store: inspect operator machine channel: %w", err)
	}
	currentChannel := current.String
	audit.Subject = displayName
	audit.Reason = auditChannelLabel(currentChannel) + " → " + auditChannelLabel(targetChannel)
	if retiredAt.Valid {
		return reject(OperatorCodeMachineRetired,
			fmt.Sprintf("%s 已在 %s 退役；先放回分母，才能變更 channel", displayName, retiredAt.String))
	}
	if req.ConfirmDisplayName != displayName {
		return reject(OperatorCodeConfirmationMismatch,
			fmt.Sprintf("確認欄位打的是 %q，不是 %q", req.ConfirmDisplayName, displayName))
	}
	if *req.ExpectedRevision != revision {
		return reject(OperatorCodePreconditionFailed,
			fmt.Sprintf("channel revision 是 %d，不是 request 預期的 %d；請重新讀取機器後再試", revision, *req.ExpectedRevision))
	}

	var observed bool
	if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM observed_state WHERE machine_id=?)`, req.MachineID).Scan(&observed); err != nil {
		return OperatorMachineChannelResult{}, fmt.Errorf("store: inspect operator machine observations: %w", err)
	}
	if !observed {
		return reject(OperatorCodeNeverObserved, ErrNeverObserved.Error())
	}

	result := OperatorMachineChannelResult{
		MachineID: req.MachineID, DisplayName: displayName,
		PreviousChannel: currentChannel, Channel: targetChannel, Revision: revision,
	}
	// Same-value requests create no new authority and therefore no new revision.
	if currentChannel != targetChannel {
		var active bool
		if err := tx.QueryRow(`SELECT EXISTS(
		 SELECT 1 FROM jobs WHERE machine_id=? AND state NOT IN (?,?,?,?,?))`, req.MachineID,
			deploy.Succeeded, deploy.Failed, deploy.Rejected, deploy.LeaseExpired, deploy.ManualIntervention).Scan(&active); err != nil {
			return OperatorMachineChannelResult{}, fmt.Errorf("store: inspect operator machine active jobs: %w", err)
		}
		if active {
			return reject(OperatorCodeMachineActiveJob,
				fmt.Sprintf("%s 仍有未終態 job；請先讓工作單結束，再把 channel 從 %q 改成 %q",
					req.MachineID, currentChannel, targetChannel))
		}
		res, err := tx.Exec(`UPDATE machine_registry
		 SET channel=?,channel_revision=channel_revision+1
			 WHERE machine_id=? AND channel_revision=?`, nullIfEmpty(targetChannel), req.MachineID, revision)
		if err != nil {
			return OperatorMachineChannelResult{}, fmt.Errorf("store: apply operator machine channel: %w", err)
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return OperatorMachineChannelResult{}, fmt.Errorf("store: operator machine channel revision changed during transaction")
		}
		result.Revision++
	}

	raw, err := json.Marshal(result)
	if err != nil {
		return OperatorMachineChannelResult{}, fmt.Errorf("store: encode operator machine channel response: %w", err)
	}
	if _, err := tx.Exec(`INSERT INTO operator_idempotency
	 (idempotency_key,operation,request_digest,outcome,response_json,created_at)
	 VALUES (?,?,?,'ok',?,?)`, req.IdempotencyKey, operation, req.RequestDigest, string(raw), fmtTime(s.now())); err != nil {
		return OperatorMachineChannelResult{}, fmt.Errorf("store: persist operator result: %w", err)
	}
	audit.OK, audit.Detail = true, ""
	if err := s.recordAuditTx(tx, audit); err != nil {
		return OperatorMachineChannelResult{}, fmt.Errorf("store: record operator success audit: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return OperatorMachineChannelResult{}, fmt.Errorf("store: commit operator machine channel: %w", err)
	}
	result.Audited = true
	return result, nil
}

func operatorError(code, detail string) *OperatorRequestError {
	return &OperatorRequestError{Code: code, Detail: detail}
}

func populateMachineChannelAudit(tx *sql.Tx, audit *AuditEntry, machineID, target string) {
	var display string
	var current sql.NullString
	if err := tx.QueryRow(`SELECT display_name,channel FROM machine_registry WHERE machine_id=?`, machineID).
		Scan(&display, &current); err != nil {
		return
	}
	audit.Subject = display
	audit.Reason = auditChannelLabel(current.String) + " → " + auditChannelLabel(target)
}

func auditChannelLabel(channel string) string {
	if channel == "" {
		return "未指派"
	}
	return channel
}
