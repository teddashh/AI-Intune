package operator

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/teddashh/AI-Intune/internal/model"
	"github.com/teddashh/AI-Intune/internal/store"
)

func TestHTTPErrorKeepsAdaptersOnOneDomainMapping(t *testing.T) {
	tests := []struct {
		code   string
		status int
	}{
		{store.OperatorCodeMachineNotFound, http.StatusNotFound},
		{store.OperatorCodeMachineRetired, http.StatusConflict},
		{store.OperatorCodeMachineActiveJob, http.StatusConflict},
		{store.OperatorCodeLifecycleRevisionLimit, http.StatusConflict},
		{store.OperatorCodeNeverObserved, http.StatusConflict},
		{store.OperatorCodeAgentExecutionUnknown, http.StatusConflict},
		{store.OperatorCodeAgentExecutionDisabled, http.StatusConflict},
		{store.OperatorCodeIdempotencyConflict, http.StatusConflict},
		{store.OperatorCodeAgentSessionLimitReached, http.StatusConflict},
		{store.OperatorCodePreconditionRequired, http.StatusPreconditionRequired},
		{store.OperatorCodePreconditionFailed, http.StatusPreconditionFailed},
		{store.OperatorCodePreviewRequired, http.StatusPreconditionRequired},
		{store.OperatorCodePreviewStale, http.StatusPreconditionFailed},
		{store.OperatorCodeBadDisplayName, http.StatusBadRequest},
		{store.OperatorCodeBadTTL, http.StatusBadRequest},
		{store.OperatorCodeConfirmationMismatch, http.StatusBadRequest},
		{store.OperatorCodeDeploymentConfirmationMismatch, http.StatusBadRequest},
		{store.OperatorCodeDeploymentNotFound, http.StatusNotFound},
		{store.OperatorCodeDeploymentNotPaused, http.StatusConflict},
		{store.OperatorCodeDeploymentActiveJobs, http.StatusConflict},
		{store.OperatorCodeDeploymentNoRetryTargets, http.StatusConflict},
		{store.OperatorCodeDeploymentNoIncludedTargets, http.StatusConflict},
		{store.OperatorCodeDeploymentPromotionBlocked, http.StatusConflict},
		{store.OperatorCodeDeploymentPreconditionFailed, http.StatusPreconditionFailed},
		{store.OperatorCodeDeploymentPreviewStale, http.StatusPreconditionFailed},
		{store.OperatorCodeProfileAssignmentPreviewStale, http.StatusPreconditionFailed},
		{store.OperatorCodeProfileAssignmentInvalid, http.StatusBadRequest},
		{store.OperatorCodeTailnetPeerInvalid, http.StatusBadRequest},
		{store.OperatorCodeTailnetPeerNotFound, http.StatusNotFound},
		{store.OperatorCodeTailnetPeerPreviewStale, http.StatusPreconditionFailed},
		{store.OperatorCodeTailnetSourceUnavailable, http.StatusServiceUnavailable},
		{store.OperatorCodeVerifierNotFound, http.StatusNotFound},
		{store.OperatorCodeVerifierAlreadyRevoked, http.StatusConflict},
		{store.OperatorCodeVerifierNameTaken, http.StatusConflict},
		{store.OperatorCodeVerifierPreviewStale, http.StatusPreconditionFailed},
		{store.OperatorCodeVerificationAssignmentJobNotFound, http.StatusNotFound},
		{store.OperatorCodeVerificationAssignmentDomainConflict, http.StatusConflict},
		{store.OperatorCodeVerificationAssignmentPreviewStale, http.StatusPreconditionFailed},
		{"PROFILE_ASSIGNMENT_REPLACEMENT_REQUIRED", http.StatusConflict},
	}
	for _, test := range tests {
		t.Run(test.code, func(t *testing.T) {
			status, code, detail := HTTPError(&store.OperatorRequestError{Code: test.code, Detail: "safe detail"})
			if status != test.status || code != test.code || detail != "safe detail" {
				t.Fatalf("HTTPError = (%d,%q,%q), want (%d,%q,%q)",
					status, code, detail, test.status, test.code, "safe detail")
			}
		})
	}
}

func TestHTTPErrorHidesUnexpectedInternalDetail(t *testing.T) {
	status, code, detail := HTTPError(errors.New("sqlite secret path /private/hub.db"))
	if status != http.StatusInternalServerError || code != "INTERNAL" || detail != "控制面操作失敗" {
		t.Fatalf("unexpected error mapping = (%d,%q,%q)", status, code, detail)
	}
}

func TestHTTPErrorMapsWriterBusy(t *testing.T) {
	err := errors.Join(store.ErrWriterBusy, errors.New("hidden /private/hub.db"))
	status, code, detail := HTTPError(err)
	if status != http.StatusServiceUnavailable || code != model.ErrHubBusy || detail != "the hub is busy; retry shortly" {
		t.Fatalf("busy mapping = (%d,%q,%q)", status, code, detail)
	}
	if strings.Contains(detail, "hub.db") || strings.Contains(detail, "/private") {
		t.Fatalf("busy detail leaked storage text: %q", detail)
	}
}
