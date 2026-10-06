package operator

import (
	"errors"
	"net/http"

	"github.com/teddashh/AI-Intune/internal/store"
)

// HTTPError is the shared presentation contract for operator adapters. HTML
// and JSON transports must not disagree about a domain rejection, and an
// unexpected storage/runtime error must never be rendered back to the caller.
func HTTPError(err error) (status int, code, detail string) {
	var rejection *store.OperatorRequestError
	if !errors.As(err, &rejection) {
		return http.StatusInternalServerError, "INTERNAL", "控制面操作失敗"
	}
	status = http.StatusBadRequest
	switch rejection.Code {
	case store.OperatorCodeMachineNotFound, store.OperatorCodeDeploymentNotFound,
		store.OperatorCodeMachineProfileNotFound,
		store.OperatorCodeTailnetPeerNotFound, store.OperatorCodeVerifierNotFound,
		store.OperatorCodeVerificationAssignmentJobNotFound,
		store.OperatorCodeSettingPolicyNotFound,
		store.OperatorCodeCompliancePolicyNotFound:
		status = http.StatusNotFound
	case store.OperatorCodeIdempotencyConflict, store.OperatorCodeNeverObserved,
		store.OperatorCodeMachinePlatformUnknown, store.OperatorCodeMachinePlatformUnsupported,
		store.OperatorCodeMachineActiveJob, store.OperatorCodeMachineRetired,
		store.OperatorCodeAgentExecutionUnknown, store.OperatorCodeAgentExecutionDisabled,
		store.OperatorCodeLifecycleRevisionLimit,
		store.OperatorCodeEnrollTokenNotPending, store.OperatorCodeDeploymentNotPaused,
		store.OperatorCodeDeploymentActiveJobs, store.OperatorCodeDeploymentNoRetryTargets,
		store.OperatorCodeDeploymentNoIncludedTargets, store.OperatorCodeDeploymentPromotionBlocked,
		store.OperatorCodeDeploymentContinueRefused,
		store.OperatorCodeArtifactFetchActive:
		status = http.StatusConflict
	case store.OperatorCodeMachineRenameUnchanged, store.OperatorCodeMachineRenameNameTaken,
		store.OperatorCodeMachineNotesUnchanged:
		status = http.StatusConflict
	case store.OperatorCodeRestoreDrillActive:
		status = http.StatusConflict
	case store.OperatorCodeVerifierAlreadyRevoked, store.OperatorCodeVerifierNameTaken:
		status = http.StatusConflict
	// The separation rule is a fact about the fleet, not a malformed request:
	// the same body becomes valid the moment that verifier moves domain.
	case store.OperatorCodeVerificationAssignmentDomainConflict:
		status = http.StatusConflict
	case store.OperatorCodeVerificationAssignmentPreviewStale:
		status = http.StatusPreconditionFailed
	case store.OperatorCodeVerifierPreviewStale:
		status = http.StatusPreconditionFailed
	case store.OperatorCodeSettingPreviewStale, store.OperatorCodeCompliancePreviewStale:
		status = http.StatusPreconditionFailed
	// A revision conflict is a fact about the ledger, not a malformed body:
	// the same request becomes valid once the operator re-reads the policy.
	case store.OperatorCodeSettingPolicyConflict, store.OperatorCodeCompliancePolicyConflict:
		status = http.StatusConflict
	case store.OperatorCodeRetentionNothingToPrune:
		status = http.StatusConflict
	// 到註冊上限是機隊現在的事實，不是送錯的 request：同一份 body 在退役一台、
	// 或把上限調高之後就會成立。回 400 會叫呼叫端去改一個沒有錯的 body。
	case store.OperatorCodeEnrollmentLimitReached:
		status = http.StatusConflict
	case store.OperatorCodePreconditionRequired:
		status = http.StatusPreconditionRequired
	case store.OperatorCodeMachineRenamePreviewRequired, store.OperatorCodeMachineRenameConfirmationMismatch,
		store.OperatorCodeMachineNotesPreviewRequired, store.OperatorCodeMachineNotesConfirmationMismatch:
		status = http.StatusPreconditionRequired
	case store.OperatorCodeCatalogConfirmationMismatch:
		status = http.StatusPreconditionRequired
	case store.OperatorCodeMachineProfileConfirmationMismatch:
		status = http.StatusPreconditionRequired
	case store.OperatorCodePreconditionFailed, store.OperatorCodeDeploymentPreconditionFailed,
		store.OperatorCodeArtifactFetchPreviewStale, store.OperatorCodeDiagnosticPreviewStale,
		store.OperatorCodeProfileAssignmentPreviewStale:
		status = http.StatusPreconditionFailed
	case store.OperatorCodeCatalogPreviewStale:
		status = http.StatusPreconditionFailed
	case store.OperatorCodeMachineProfilePreviewStale:
		status = http.StatusPreconditionFailed
	case store.OperatorCodeMachineRenamePreviewStale:
		status = http.StatusPreconditionFailed
	case store.OperatorCodeMachineNotesPreviewStale:
		status = http.StatusPreconditionFailed
	case store.OperatorCodeTailnetPeerPreviewStale:
		status = http.StatusPreconditionFailed
	case store.OperatorCodeRetentionPreviewStale:
		status = http.StatusPreconditionFailed
	case store.OperatorCodeRestoreDrillPreviewStale:
		status = http.StatusPreconditionFailed
	case store.OperatorCodeEnrollmentLimitPreviewStale:
		status = http.StatusPreconditionFailed
	case store.OperatorCodePreviewRequired:
		status = http.StatusPreconditionRequired
	case store.OperatorCodePreviewStale, store.OperatorCodeDeploymentPreviewStale:
		status = http.StatusPreconditionFailed
	case store.OperatorCodeTailnetSourceUnavailable:
		status = http.StatusServiceUnavailable
	}
	if rejection.Code == "PROFILE_ASSIGNMENT_REPLACEMENT_REQUIRED" {
		status = http.StatusConflict
	}
	return status, rejection.Code, rejection.Detail
}
