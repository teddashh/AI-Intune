package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/teddashh/AI-Intune/internal/deploy"
	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/state"
	"github.com/teddashh/AI-Intune/internal/store"
)

type machineChannelOperatorRequest struct {
	Channel            string `json:"channel"`
	ExpectedRevision   *int64 `json:"expected_revision"`
	ConfirmDisplayName string `json:"confirm_display_name"`
}

type enrollTokenPreviewOperatorRequest struct {
	DisplayName string `json:"display_name"`
	TTLSeconds  int64  `json:"ttl_seconds"`
}

type enrollTokenCreateOperatorRequest struct {
	DisplayName   string `json:"display_name"`
	TTLSeconds    int64  `json:"ttl_seconds"`
	PreviewDigest string `json:"preview_digest"`
	Reason        string `json:"reason"`
}

type enrollTokenRevocationPreviewOperatorRequest struct{}

type enrollTokenRevocationOperatorRequest struct {
	PreviewDigest string `json:"preview_digest"`
	Reason        string `json:"reason"`
}

// enrollTokenOperatorResponse deliberately spells out the two wire shapes.
// RecoveryAction is always present so strict clients can distinguish an
// intentionally empty fresh value from a response produced by older code.
// EnrollmentToken is the only optional field and must be absent on replay.
type enrollTokenOperatorResponse struct {
	MachineID        string    `json:"machine_id"`
	DisplayName      string    `json:"display_name"`
	CreatedAt        time.Time `json:"created_at"`
	ExpiresAt        time.Time `json:"expires_at"`
	TTLSeconds       int64     `json:"ttl_seconds"`
	PreviewDigest    string    `json:"preview_digest"`
	EnrollmentToken  string    `json:"enrollment_token,omitempty"`
	SecretAvailable  bool      `json:"secret_available"`
	Replayed         bool      `json:"replayed"`
	RecoveryRequired bool      `json:"recovery_required"`
	RecoveryAction   string    `json:"recovery_action"`
}

// operatorRoutes is deliberately separate from agentRoutes. Production mounts
// this mux behind Tailscale LocalAPI app-cap authorization and CSRF protection.
// It never accepts or interprets machine bearer tokens.
func (h *hub) operatorRoutes(mux *http.ServeMux) []string {
	patterns := []string{
		"GET /v1/operator/machines",
		"GET /v1/operator/machines/{id}",
		"GET /v1/operator/machines/{id}/channel",
		"PUT /v1/operator/machines/{id}/channel",
		"POST /v1/operator/enrollment-tokens/preview",
		"POST /v1/operator/enrollment-tokens",
		"GET /v1/operator/machines/{id}/enrollment-token",
		"POST /v1/operator/machines/{id}/enrollment-token/revocation-preview",
		"POST /v1/operator/machines/{id}/enrollment-token/revocations",
		"GET /v1/operator/jobs",
		"GET /v1/operator/jobs/{id}",
		"GET /v1/operator/deployments",
		"GET /v1/operator/deployments/{id}",
		"POST /v1/operator/deployments/preview",
		"POST /v1/operator/deployments",
		"POST /v1/operator/deployments/{id}/continuation-preview",
		"POST /v1/operator/deployments/{id}/continuations",
		"POST /v1/operator/deployments/{id}/retry-preview",
		"POST /v1/operator/deployments/{id}/retries",
		"POST /v1/operator/deployments/{id}/abandonment-preview",
		"POST /v1/operator/deployments/{id}/abandonments",
		"GET /v1/operator/artifacts",
		"GET /v1/operator/artifacts/{sha256}",
		"POST /v1/operator/artifact-fetches/preview",
		"POST /v1/operator/artifact-fetches",
		"GET /v1/operator/artifact-fetches",
		"GET /v1/operator/artifact-fetches/{id}",
		"GET /v1/operator/updates",
		"GET /v1/operator/audit-events",
		"GET /v1/operator/changes",
		"GET /v1/operator/machines/{id}/lifecycle",
		"POST /v1/operator/machines/{id}/lifecycle-preview",
		"PUT /v1/operator/machines/{id}/lifecycle",
		"GET /v1/operator/jobs/{id}/evidence",
		"GET /v1/operator/machines/{id}/evidence",
		"POST /v1/operator/machines/{id}/diagnostic-noop-preview",
		"POST /v1/operator/machines/{id}/diagnostic-noop-jobs",
		"GET /v1/operator/catalog-manifests",
		"POST /v1/operator/catalog-manifests",
		"GET /v1/operator/machine-profiles",
		"POST /v1/operator/machine-profiles",
		"POST /v1/operator/machines/{id}/profile-assignment-preview",
		"POST /v1/operator/machines/{id}/profile-assignments",
		"POST /v1/operator/catalog-manifests/standard-preview",
		"POST /v1/operator/machine-profiles/preview",
		"GET /v1/operator/tailnet",
		"POST /v1/operator/tailnet/peer-ignore-preview",
		"PUT /v1/operator/tailnet/peer-ignores/{id}",
		"GET /v1/operator/maintenance/retention",
		"POST /v1/operator/maintenance/retention/prune-preview",
		"POST /v1/operator/maintenance/retention/prunes",
		"POST /v1/operator/maintenance/restore-drill-preview",
		"POST /v1/operator/maintenance/restore-drills",
		"GET /v1/operator/maintenance/restore-drills",
		"GET /v1/operator/maintenance/restore-drills/{id}",
		"GET /v1/operator/tickets",
		"POST /v1/operator/verifiers/preview",
		"POST /v1/operator/verifiers",
		"GET /v1/operator/verifiers",
		"GET /v1/operator/verifiers/{id}",
		"POST /v1/operator/verifiers/{id}/revocation-preview",
		"POST /v1/operator/verifiers/{id}/revocations",
		"POST /v1/operator/verifiers/{id}/assignment-preview",
		"POST /v1/operator/verifiers/{id}/assignments",
		"GET /v1/operator/settings",
		"POST /v1/operator/setting-policies/preview",
		"POST /v1/operator/setting-policies",
		"POST /v1/operator/setting-assignments/preview",
		"POST /v1/operator/setting-assignments",
		"GET /v1/operator/compliance",
		"POST /v1/operator/compliance-policies/preview",
		"POST /v1/operator/compliance-policies",
		"POST /v1/operator/compliance-assignments/preview",
		"POST /v1/operator/compliance-assignments",
		"GET /v1/operator/machines/{id}/actions",
		"GET /v1/operator/reports",
		"GET /v1/operator/machines/{id}/timeline",
		"GET /v1/operator/data-disclosure",
		"GET /v1/operator/machines/{id}/data",
		"GET /v1/operator/enrollment-report",
		"GET /v1/operator/enrollment-limit",
		"POST /v1/operator/enrollment-limit/preview",
		"POST /v1/operator/enrollment-limit",
		"GET /v1/operator/software-report",
		"GET /v1/operator/install-report",
		"GET /v1/operator/profile-report",
		"POST /v1/operator/machines/{id}/display-name-preview",
		"PUT /v1/operator/machines/{id}/display-name",
		"POST /v1/operator/machines/{id}/notes-preview",
		"PUT /v1/operator/machines/{id}/notes",
		"GET /v1/operator/daily-report",
		"GET /v1/operator/machines/{id}/assigned-user",
		"PUT /v1/operator/machines/{id}/assigned-user",
		"POST /v1/operator/deployments/{id}/skip-failed-batch-preview",
		"POST /v1/operator/deployments/{id}/skip-failed-batches",
	}
	mux.HandleFunc(patterns[0], h.handleListOperatorMachines)
	mux.HandleFunc(patterns[1], h.handleGetOperatorMachine)
	mux.HandleFunc(patterns[2], h.handleGetOperatorMachineChannel)
	mux.HandleFunc(patterns[3], h.handlePutOperatorMachineChannel)
	mux.HandleFunc(patterns[4], h.handlePreviewOperatorEnrollToken)
	mux.HandleFunc(patterns[5], h.handleCreateOperatorEnrollToken)
	mux.HandleFunc(patterns[6], h.handleGetOperatorPendingEnrollToken)
	mux.HandleFunc(patterns[7], h.handlePreviewOperatorEnrollTokenRevocation)
	mux.HandleFunc(patterns[8], h.handleCreateOperatorEnrollTokenRevocation)
	mux.HandleFunc(patterns[9], h.handleListOperatorJobs)
	mux.HandleFunc(patterns[10], h.handleGetOperatorJob)
	mux.HandleFunc(patterns[11], h.handleListOperatorDeployments)
	mux.HandleFunc(patterns[12], h.handleGetOperatorDeployment)
	mux.HandleFunc(patterns[13], h.handlePreviewOperatorDeploymentCreate)
	mux.HandleFunc(patterns[14], h.handleCreateOperatorDeployment)
	mux.HandleFunc(patterns[15], h.handlePreviewOperatorDeploymentContinue)
	mux.HandleFunc(patterns[16], h.handleContinueOperatorDeployment)
	mux.HandleFunc(patterns[17], h.handlePreviewOperatorDeploymentRetry)
	mux.HandleFunc(patterns[18], h.handleRetryOperatorDeployment)
	mux.HandleFunc(patterns[19], h.handlePreviewOperatorDeploymentAbandon)
	mux.HandleFunc(patterns[20], h.handleAbandonOperatorDeployment)
	mux.HandleFunc(patterns[21], h.handleListOperatorArtifacts)
	mux.HandleFunc(patterns[22], h.handleGetOperatorArtifact)
	mux.HandleFunc(patterns[23], h.handlePreviewOperatorArtifactFetch)
	mux.HandleFunc(patterns[24], h.handleCreateOperatorArtifactFetch)
	mux.HandleFunc(patterns[25], h.handleListOperatorArtifactFetches)
	mux.HandleFunc(patterns[26], h.handleGetOperatorArtifactFetch)
	mux.HandleFunc(patterns[27], h.handleGetOperatorUpdates)
	mux.HandleFunc(patterns[28], h.handleListOperatorAuditEvents)
	mux.HandleFunc(patterns[29], h.handleListOperatorChanges)
	mux.HandleFunc(patterns[30], h.handleGetOperatorMachineLifecycle)
	mux.HandleFunc(patterns[31], h.handlePreviewOperatorMachineLifecycle)
	mux.HandleFunc(patterns[32], h.handlePutOperatorMachineLifecycle)
	mux.HandleFunc(patterns[33], h.handleGetOperatorJobEvidence)
	mux.HandleFunc(patterns[34], h.handleGetOperatorMachineEvidence)
	mux.HandleFunc(patterns[35], h.handlePreviewOperatorDiagnosticNoop)
	mux.HandleFunc(patterns[36], h.handleCreateOperatorDiagnosticNoop)
	mux.HandleFunc(patterns[37], h.handleListOperatorCatalogManifests)
	mux.HandleFunc(patterns[38], h.handlePublishOperatorCatalogManifest)
	mux.HandleFunc(patterns[39], h.handleListOperatorMachineProfiles)
	mux.HandleFunc(patterns[40], h.handlePublishOperatorMachineProfile)
	mux.HandleFunc(patterns[41], h.handlePreviewOperatorMachineProfileAssignment)
	mux.HandleFunc(patterns[42], h.handleCreateOperatorMachineProfileAssignment)
	mux.HandleFunc(patterns[43], h.handlePreviewOperatorStandardCatalogManifest)
	mux.HandleFunc(patterns[44], h.handlePreviewOperatorMachineProfile)
	mux.HandleFunc(patterns[45], h.handleGetOperatorTailnet)
	mux.HandleFunc(patterns[46], h.handlePreviewOperatorTailnetPeerIgnore)
	mux.HandleFunc(patterns[47], h.handlePutOperatorTailnetPeerIgnore)
	mux.HandleFunc(patterns[48], h.handleGetOperatorRetention)
	mux.HandleFunc(patterns[49], h.handlePreviewOperatorRetentionPrune)
	mux.HandleFunc(patterns[50], h.handleCreateOperatorRetentionPrune)
	mux.HandleFunc(patterns[51], h.handlePreviewOperatorRestoreDrill)
	mux.HandleFunc(patterns[52], h.handleCreateOperatorRestoreDrill)
	mux.HandleFunc(patterns[53], h.handleListOperatorRestoreDrills)
	mux.HandleFunc(patterns[54], h.handleGetOperatorRestoreDrill)
	mux.HandleFunc(patterns[55], h.handleListOperatorTickets)
	mux.HandleFunc(patterns[56], h.handlePreviewOperatorVerifier)
	mux.HandleFunc(patterns[57], h.handleCreateOperatorVerifier)
	mux.HandleFunc(patterns[58], h.handleListOperatorVerifiers)
	mux.HandleFunc(patterns[59], h.handleGetOperatorVerifier)
	mux.HandleFunc(patterns[60], h.handlePreviewOperatorVerifierRevocation)
	mux.HandleFunc(patterns[61], h.handleCreateOperatorVerifierRevocation)
	mux.HandleFunc(patterns[62], h.handlePreviewOperatorVerificationAssignment)
	mux.HandleFunc(patterns[63], h.handleCreateOperatorVerificationAssignment)
	mux.HandleFunc(patterns[64], h.handleGetOperatorSettings)
	mux.HandleFunc(patterns[65], h.handlePreviewOperatorSettingPolicy)
	mux.HandleFunc(patterns[66], h.handlePublishOperatorSettingPolicy)
	mux.HandleFunc(patterns[67], h.handlePreviewOperatorSettingAssignment)
	mux.HandleFunc(patterns[68], h.handleCreateOperatorSettingAssignment)
	mux.HandleFunc(patterns[69], h.handleGetOperatorCompliance)
	mux.HandleFunc(patterns[70], h.handlePreviewOperatorCompliancePolicy)
	mux.HandleFunc(patterns[71], h.handlePublishOperatorCompliancePolicy)
	mux.HandleFunc(patterns[72], h.handlePreviewOperatorComplianceAssignment)
	mux.HandleFunc(patterns[73], h.handleCreateOperatorComplianceAssignment)
	mux.HandleFunc(patterns[74], h.handleGetOperatorMachineActions)
	mux.HandleFunc(patterns[75], h.handleGetOperatorReports)
	mux.HandleFunc(patterns[76], h.handleGetOperatorMachineTimeline)
	mux.HandleFunc(patterns[77], h.handleGetOperatorDataDisclosure)
	mux.HandleFunc(patterns[78], h.handleGetOperatorMachineData)
	mux.HandleFunc(patterns[79], h.handleGetOperatorEnrollmentReport)
	mux.HandleFunc(patterns[80], h.handleGetOperatorEnrollmentLimit)
	mux.HandleFunc(patterns[81], h.handlePreviewOperatorEnrollmentLimit)
	mux.HandleFunc(patterns[82], h.handleSetOperatorEnrollmentLimit)
	mux.HandleFunc(patterns[83], h.handleGetOperatorSoftwareReport)
	mux.HandleFunc(patterns[84], h.handleGetOperatorInstallReport)
	mux.HandleFunc(patterns[85], h.handleGetOperatorProfileReport)
	mux.HandleFunc(patterns[86], h.handlePreviewOperatorMachineRename)
	mux.HandleFunc(patterns[87], h.handlePutOperatorMachineRename)
	mux.HandleFunc(patterns[88], h.handlePreviewOperatorMachineNotes)
	mux.HandleFunc(patterns[89], h.handlePutOperatorMachineNotes)
	mux.HandleFunc(patterns[90], h.handleGetOperatorDailyReport)
	mux.HandleFunc(patterns[91], h.handleGetOperatorMachineAssignedUser)
	mux.HandleFunc(patterns[92], h.handlePutOperatorMachineAssignedUser)
	mux.HandleFunc(patterns[93], h.handlePreviewOperatorDeploymentSkipFailedBatch)
	mux.HandleFunc(patterns[94], h.handleSkipFailedBatchOperatorDeployment)
	patterns = append(patterns, "GET /metrics")
	mux.HandleFunc("GET /metrics", h.handleMetrics)
	diskClean := len(patterns)
	patterns = append(patterns,
		"GET /v1/operator/disk-clean/summaries",
		"GET /v1/operator/disk-clean/summaries/{id}",
		"POST /v1/operator/disk-clean/profile-preview",
		"POST /v1/operator/disk-clean/profiles",
		"POST /v1/operator/disk-clean/dry-run-preview",
		"POST /v1/operator/disk-clean/dry-runs",
		"POST /v1/operator/disk-clean/canary-preview",
		"POST /v1/operator/disk-clean/canaries",
		"POST /v1/operator/disk-clean/continuation-preview",
		"POST /v1/operator/disk-clean/continuations",
		"POST /v1/operator/disk-clean/abandonment-preview",
		"POST /v1/operator/disk-clean/abandonments",
	)
	for i, handler := range []http.HandlerFunc{
		h.handleListOperatorDiskCleanSummaries,
		h.handleGetOperatorDiskCleanSummary,
		h.handlePreviewOperatorDiskCleanProfile,
		h.handlePublishOperatorDiskCleanProfile,
		h.handlePreviewOperatorDiskCleanDryRun,
		h.handleApplyOperatorDiskCleanDryRun,
		h.handlePreviewOperatorDiskCleanCanary,
		h.handleApplyOperatorDiskCleanCanary,
		h.handlePreviewOperatorDiskCleanContinue,
		h.handleContinueOperatorDiskClean,
		h.handlePreviewOperatorDiskCleanAbandon,
		h.handleAbandonOperatorDiskClean,
	} {
		mux.HandleFunc(patterns[diskClean+i], handler)
	}
	return patterns
}

func (h *hub) handleListOperatorJobs(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	request, err := parseOperatorJobListRequest(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "BAD_REQUEST", err.Error())
		return
	}
	result, err := operator.New(h.store).ListJobs(request, time.Now().UTC())
	if err != nil {
		if errors.Is(err, operator.ErrInvalidJobRead) {
			writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "工作單 filter 或 cursor 不合法")
			return
		}
		log.Printf("讀取 operator job list 失敗: %v", err)
		writeErr(w, http.StatusInternalServerError, "INTERNAL", "讀取 job list 失敗")
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *hub) handleGetOperatorJob(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.ForceQuery || r.URL.RawQuery != "" {
		writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "job detail 不接受 query parameters")
		return
	}
	result, err := operator.New(h.store).JobDetail(r.PathValue("id"), time.Now().UTC())
	if err != nil {
		if errors.Is(err, store.ErrJobNotFound) {
			writeErr(w, http.StatusNotFound, "JOB_NOT_FOUND", "找不到這張工作單")
			return
		}
		if errors.Is(err, operator.ErrInvalidJobRead) {
			writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "job_id 不合法")
			return
		}
		log.Printf("讀取 operator job detail 失敗 job=%q: %v", r.PathValue("id"), err)
		writeErr(w, http.StatusInternalServerError, "INTERNAL", "讀取 job detail 失敗")
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *hub) handleGetOperatorJobEvidence(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	limit, err := parseOperatorJobEvidenceLimit(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "BAD_REQUEST", err.Error())
		return
	}
	result, err := operator.New(h.store).JobEvidence(operator.JobEvidenceRequest{
		JobID: r.PathValue("id"), Limit: limit,
	}, time.Now().UTC())
	if err != nil {
		switch {
		case errors.Is(err, store.ErrJobNotFound):
			writeErr(w, http.StatusNotFound, "JOB_NOT_FOUND", "找不到這張工作單")
		case errors.Is(err, operator.ErrInvalidJobRead):
			writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "job_id 或 limit 不合法")
		default:
			log.Printf("讀取 operator job evidence 失敗 job=%q: %v", r.PathValue("id"), err)
			writeErr(w, http.StatusInternalServerError, "INTERNAL", "讀取 job evidence 失敗")
		}
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func parseOperatorJobEvidenceLimit(r *http.Request) (int, error) {
	if r == nil || r.URL == nil {
		return 0, errors.New("job evidence request 不完整")
	}
	if r.URL.ForceQuery && r.URL.RawQuery == "" {
		return 0, errors.New("job evidence 不接受空的 query marker")
	}
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return 0, errors.New("job evidence query 編碼不合法")
	}
	for key := range values {
		if key != "limit" {
			return 0, fmt.Errorf("job evidence 不接受 query parameter %q", key)
		}
	}
	raw, present := values["limit"]
	if !present {
		return 0, nil
	}
	if len(raw) != 1 || raw[0] == "" || raw[0] != strings.TrimSpace(raw[0]) {
		return 0, errors.New("job evidence limit 必須只出現一次且不可為空")
	}
	limit, err := strconv.Atoi(raw[0])
	if err != nil || limit < 1 || limit > operator.JobEvidenceDefaultLimit {
		return 0, fmt.Errorf("job evidence limit 必須介於 1 與 %d", operator.JobEvidenceDefaultLimit)
	}
	return limit, nil
}

func (h *hub) handleGetOperatorMachineEvidence(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	limit, err := parseOperatorMachineEvidenceLimit(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "BAD_REQUEST", err.Error())
		return
	}
	result, err := operator.New(h.store).MachineEvidence(operator.MachineEvidenceRequest{
		MachineID: r.PathValue("id"), Limit: limit,
	}, time.Now().UTC())
	if err != nil {
		switch {
		case errors.Is(err, store.ErrNotFound):
			writeErr(w, http.StatusNotFound, store.OperatorCodeMachineNotFound, "找不到這台機器")
		case errors.Is(err, operator.ErrInvalidMachineRead):
			writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "machine_id 或 limit 不合法")
		default:
			log.Printf("讀取 operator machine evidence 失敗 machine=%q: %v", r.PathValue("id"), err)
			writeErr(w, http.StatusInternalServerError, "INTERNAL", "讀取 machine evidence 失敗")
		}
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func parseOperatorMachineEvidenceLimit(r *http.Request) (int, error) {
	if r == nil || r.URL == nil {
		return 0, errors.New("machine evidence request 不完整")
	}
	if r.URL.ForceQuery && r.URL.RawQuery == "" {
		return 0, errors.New("machine evidence 不接受空的 query marker")
	}
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return 0, errors.New("machine evidence query 編碼不合法")
	}
	for key := range values {
		if key != "limit" {
			return 0, fmt.Errorf("machine evidence 不接受 query parameter %q", key)
		}
	}
	raw, present := values["limit"]
	if !present {
		return 0, nil
	}
	if len(raw) != 1 || raw[0] == "" || raw[0] != strings.TrimSpace(raw[0]) {
		return 0, errors.New("machine evidence limit 必須只出現一次且不可為空")
	}
	limit, err := strconv.Atoi(raw[0])
	if err != nil || limit < 1 || limit > store.MaxMachineEvidencePageSize {
		return 0, fmt.Errorf("machine evidence limit 必須介於 1 與 %d", store.MaxMachineEvidencePageSize)
	}
	return limit, nil
}

func parseOperatorJobListRequest(r *http.Request) (operator.JobListRequest, error) {
	if r == nil || r.URL == nil {
		return operator.JobListRequest{}, errors.New("job list request 不完整")
	}
	if r.URL.ForceQuery && r.URL.RawQuery == "" {
		return operator.JobListRequest{}, errors.New("job list 不接受空的 query marker")
	}
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return operator.JobListRequest{}, errors.New("job list query 編碼不合法")
	}
	allowed := map[string]bool{
		"machine_id": true, "state": true, "deployment_id": true,
		"resource_kind": true, "resource_id": true, "limit": true, "cursor": true,
	}
	for key := range values {
		if !allowed[key] {
			return operator.JobListRequest{}, fmt.Errorf("job list 不接受 query parameter %q", key)
		}
	}
	request := operator.JobListRequest{}
	if request.MachineID, err = oneOperatorJobQueryValue(values, "machine_id", 256); err != nil {
		return operator.JobListRequest{}, err
	}
	if request.DeploymentID, err = oneOperatorJobQueryValue(values, "deployment_id", 256); err != nil {
		return operator.JobListRequest{}, err
	}
	if request.ResourceKind, err = oneOperatorJobQueryValue(values, "resource_kind", 128); err != nil {
		return operator.JobListRequest{}, err
	}
	if request.ResourceID, err = oneOperatorJobQueryValue(values, "resource_id", 256); err != nil {
		return operator.JobListRequest{}, err
	}
	if request.Cursor, err = oneOperatorJobQueryValue(values, "cursor", 2048); err != nil {
		return operator.JobListRequest{}, err
	}
	if raw, present := values["limit"]; present {
		if len(raw) != 1 || raw[0] == "" || raw[0] != strings.TrimSpace(raw[0]) {
			return operator.JobListRequest{}, errors.New("job list limit 必須只出現一次且不可為空")
		}
		request.Limit, err = strconv.Atoi(raw[0])
		if err != nil {
			return operator.JobListRequest{}, errors.New("job list limit 必須是整數")
		}
		if request.Limit < 1 || request.Limit > store.MaxJobReadPageSize {
			return operator.JobListRequest{}, fmt.Errorf("job list limit 必須介於 1 與 %d", store.MaxJobReadPageSize)
		}
	}
	if raw, present := values["state"]; present {
		if len(raw) == 0 || len(raw) > len(deploy.AllJobStates) {
			return operator.JobListRequest{}, errors.New("job list state 數量不合法")
		}
		seen := make(map[deploy.JobState]bool, len(raw))
		for _, value := range raw {
			state := deploy.JobState(value)
			if value == "" || value != strings.TrimSpace(value) || !deploy.IsKnownJobState(state) {
				return operator.JobListRequest{}, errors.New("job list state 必須是 canonical job state")
			}
			if seen[state] {
				return operator.JobListRequest{}, errors.New("job list state 不可重複")
			}
			seen[state] = true
			request.States = append(request.States, state)
		}
	}
	if request.ResourceID != "" && request.ResourceKind == "" {
		return operator.JobListRequest{}, errors.New("job list resource_id 必須搭配 resource_kind")
	}
	return request, nil
}

func oneOperatorJobQueryValue(values url.Values, name string, maxBytes int) (string, error) {
	raw, present := values[name]
	if !present {
		return "", nil
	}
	if len(raw) != 1 || raw[0] == "" || raw[0] != strings.TrimSpace(raw[0]) || len(raw[0]) > maxBytes {
		return "", fmt.Errorf("job list %s 必須只出現一次、不可為空或含首尾空白", name)
	}
	for _, r := range raw[0] {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return "", fmt.Errorf("job list %s 不可含控制或隱形格式字元", name)
		}
	}
	return raw[0], nil
}

func (h *hub) handleListOperatorMachines(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	request, err := parseOperatorMachineListRequest(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "BAD_REQUEST", err.Error())
		return
	}
	result, err := operator.New(h.store).ListMachinesPage(request, time.Now().UTC())
	if err != nil {
		if errors.Is(err, operator.ErrInvalidMachineRead) {
			writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "machine filter 或 cursor 不合法")
			return
		}
		log.Printf("讀取 operator machine list 失敗: %v", err)
		writeErr(w, http.StatusInternalServerError, "INTERNAL", "讀取 machine list 失敗")
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func parseOperatorMachineListRequest(r *http.Request) (operator.MachineListRequest, error) {
	if r == nil || r.URL == nil {
		return operator.MachineListRequest{}, errors.New("machine list request 不完整")
	}
	if r.URL.ForceQuery && r.URL.RawQuery == "" {
		return operator.MachineListRequest{}, errors.New("machine list 不接受空的 query marker")
	}
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return operator.MachineListRequest{}, errors.New("machine list query 編碼不合法")
	}
	allowed := map[string]bool{
		"machine_id": true, "display_name": true, "state": true, "lifecycle": true,
		"reporting": true, "channel": true, "limit": true, "cursor": true,
	}
	for key := range values {
		if !allowed[key] {
			return operator.MachineListRequest{}, fmt.Errorf("machine list 不接受 query parameter %q", key)
		}
	}
	request := operator.MachineListRequest{}
	for _, field := range []struct {
		name   string
		max    int
		target *string
	}{
		{"machine_id", 256, &request.MachineID}, {"display_name", 256, &request.DisplayName},
		{"cursor", 2048, &request.Cursor},
	} {
		value, err := oneOperatorMachineQueryValue(values, field.name, field.max)
		if err != nil {
			return operator.MachineListRequest{}, err
		}
		*field.target = value
	}
	if raw, present := values["state"]; present {
		if len(raw) == 0 || len(raw) > len(state.AllStates) {
			return operator.MachineListRequest{}, errors.New("machine list state 數量不合法")
		}
		seen := make(map[state.State]bool, len(raw))
		for _, value := range raw {
			candidate := state.State(value)
			known := false
			for _, allowedState := range state.AllStates {
				known = known || candidate == allowedState
			}
			if !known || seen[candidate] {
				return operator.MachineListRequest{}, errors.New("machine list state 必須 canonical 且不可重複")
			}
			seen[candidate] = true
			request.States = append(request.States, candidate)
		}
	}
	if value, err := oneOperatorMachineQueryValue(values, "lifecycle", 16); err != nil {
		return operator.MachineListRequest{}, err
	} else {
		request.Lifecycle = operator.MachineLifecycleFilter(value)
	}
	if value, err := oneOperatorMachineQueryValue(values, "reporting", 8); err != nil {
		return operator.MachineListRequest{}, err
	} else {
		request.Reporting = operator.MachineReportingFilter(value)
	}
	if value, err := oneOperatorMachineQueryValue(values, "channel", 16); err != nil {
		return operator.MachineListRequest{}, err
	} else {
		request.Channel = operator.MachineChannelFilter(value)
	}
	if raw, present := values["limit"]; present {
		if len(raw) != 1 || raw[0] == "" || raw[0] != strings.TrimSpace(raw[0]) {
			return operator.MachineListRequest{}, errors.New("machine list limit 必須只出現一次且不可為空")
		}
		request.Limit, err = strconv.Atoi(raw[0])
		if err != nil || strconv.Itoa(request.Limit) != raw[0] ||
			request.Limit < 1 || request.Limit > operator.MaxMachineReadLimit {
			return operator.MachineListRequest{}, fmt.Errorf("machine list limit 必須介於 1 與 %d", operator.MaxMachineReadLimit)
		}
	}
	// Run the shared normalization once at the service boundary as well. These
	// explicit enum checks keep malformed requests from reaching Store work.
	if request.Lifecycle != "" && request.Lifecycle != operator.MachineLifecycleAny &&
		request.Lifecycle != operator.MachineLifecycleActive && request.Lifecycle != operator.MachineLifecycleRetired {
		return operator.MachineListRequest{}, errors.New("machine list lifecycle 必須是 any、active 或 retired")
	}
	if request.Reporting != "" && request.Reporting != operator.MachineReportingAny &&
		request.Reporting != operator.MachineReportingTrue && request.Reporting != operator.MachineReportingFalse &&
		request.Reporting != operator.MachineReportingUnknown {
		return operator.MachineListRequest{}, errors.New("machine list reporting 必須是 any、true、false 或 unknown")
	}
	if request.Channel != "" && request.Channel != operator.MachineChannelAny &&
		request.Channel != operator.MachineChannelNone && request.Channel != operator.MachineChannelCanary &&
		request.Channel != operator.MachineChannelStable {
		return operator.MachineListRequest{}, errors.New("machine list channel 必須是 any、none、canary 或 stable")
	}
	return request, nil
}

func oneOperatorMachineQueryValue(values url.Values, name string, maxBytes int) (string, error) {
	raw, present := values[name]
	if !present {
		return "", nil
	}
	if len(raw) != 1 || raw[0] == "" || raw[0] != strings.TrimSpace(raw[0]) ||
		len(raw[0]) > maxBytes || !utf8.ValidString(raw[0]) {
		return "", fmt.Errorf("machine list %s 必須只出現一次、不可為空或含首尾空白", name)
	}
	for _, char := range raw[0] {
		if unicode.IsControl(char) || unicode.Is(unicode.Cf, char) {
			return "", fmt.Errorf("machine list %s 不可含控制或隱形格式字元", name)
		}
	}
	return raw[0], nil
}

func (h *hub) handleGetOperatorMachine(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.ForceQuery || r.URL.RawQuery != "" {
		writeErr(w, http.StatusBadRequest, "BAD_REQUEST",
			"machine detail 目前不接受 query parameters")
		return
	}
	machineID := r.PathValue("id")
	result, err := operator.New(h.store).MachineDetail(machineID, time.Now().UTC())
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, store.OperatorCodeMachineNotFound, "找不到這台機器")
			return
		}
		log.Printf("讀取 operator machine detail 失敗 machine=%q: %v", machineID, err)
		writeErr(w, http.StatusInternalServerError, "INTERNAL", "讀取 machine detail 失敗")
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *hub) handleGetOperatorPendingEnrollToken(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	result, err := operator.New(h.store).PendingEnrollToken(r.PathValue("id"))
	if err != nil {
		status, code, detail := operator.HTTPError(err)
		if status == http.StatusInternalServerError {
			log.Printf("operator pending enrollment token read 失敗 machine=%s: %v", r.PathValue("id"), err)
		}
		writeErr(w, status, code, detail)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *hub) handlePreviewOperatorEnrollTokenRevocation(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeErr(w, http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE",
			"Content-Type 必須是 application/json")
		return
	}
	var body enrollTokenRevocationPreviewOperatorRequest
	if rejection := decodeOperatorBody(w, r, &body); rejection != nil {
		writeErr(w, rejection.Status, rejection.Code, rejection.Detail)
		return
	}
	result, err := operator.New(h.store).PreviewEnrollTokenRevocation(
		operator.EnrollTokenRevocationPreviewRequest{MachineID: r.PathValue("id")})
	if err != nil {
		status, code, detail := operator.HTTPError(err)
		if status == http.StatusInternalServerError {
			log.Printf("operator enrollment token revocation preview 失敗 machine=%s: %v", r.PathValue("id"), err)
		}
		writeErr(w, status, code, detail)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *hub) handleCreateOperatorEnrollTokenRevocation(w http.ResponseWriter, r *http.Request) {
	actor := operatorActor(r)
	w.Header().Set("Cache-Control", "no-store")
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		h.rejectOperatorEnrollTokenRevocationTransport(w, r, actor,
			http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE",
			"Content-Type 必須是 application/json")
		return
	}
	var body enrollTokenRevocationOperatorRequest
	if rejection := decodeOperatorBody(w, r, &body); rejection != nil {
		h.rejectOperatorEnrollTokenRevocationTransport(w, r, actor,
			rejection.Status, rejection.Code, rejection.Detail)
		return
	}
	result, err := operator.New(h.store).RevokeEnrollToken(operator.EnrollTokenRevocationRequest{
		MachineID: r.PathValue("id"), PreviewDigest: body.PreviewDigest,
		Reason: body.Reason, IdempotencyKey: r.Header.Get("Idempotency-Key"), Actor: actor,
	})
	if err != nil {
		status, code, detail := operator.HTTPError(err)
		if status == http.StatusInternalServerError {
			log.Printf("operator enrollment token revocation 失敗 machine=%s: %v", r.PathValue("id"), err)
		}
		var rejection *store.OperatorRequestError
		if errors.As(err, &rejection) && rejection.Replayed {
			w.Header().Set("Idempotency-Replayed", "true")
		}
		writeErr(w, status, code, detail)
		return
	}
	if result.Replayed {
		w.Header().Set("Idempotency-Replayed", "true")
		writeJSON(w, http.StatusOK, result)
		return
	}
	writeJSON(w, http.StatusCreated, result)
}

func (h *hub) handlePreviewOperatorEnrollToken(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeErr(w, http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE",
			"Content-Type 必須是 application/json")
		return
	}
	var body enrollTokenPreviewOperatorRequest
	if rejection := decodeOperatorBody(w, r, &body); rejection != nil {
		writeErr(w, rejection.Status, rejection.Code, rejection.Detail)
		return
	}
	result, err := operator.New(h.store).PreviewEnrollToken(operator.EnrollTokenPreviewRequest{
		DisplayName: body.DisplayName,
		TTLSeconds:  body.TTLSeconds,
	})
	if err != nil {
		status, code, detail := operator.HTTPError(err)
		if status == http.StatusInternalServerError {
			log.Printf("operator enrollment token preview 失敗: %v", err)
		}
		writeErr(w, status, code, detail)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *hub) handleCreateOperatorEnrollToken(w http.ResponseWriter, r *http.Request) {
	// Capture the boundary-authenticated actor once. Transport failures and the
	// canonical service path must retain the same authority evidence.
	actor := operatorActor(r)
	w.Header().Set("Cache-Control", "no-store")
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		h.rejectOperatorEnrollTokenTransport(w, r, actor, http.StatusUnsupportedMediaType,
			"UNSUPPORTED_MEDIA_TYPE", "Content-Type 必須是 application/json")
		return
	}
	var body enrollTokenCreateOperatorRequest
	if rejection := decodeOperatorBody(w, r, &body); rejection != nil {
		h.rejectOperatorEnrollTokenTransport(w, r, actor,
			rejection.Status, rejection.Code, rejection.Detail)
		return
	}
	result, err := operator.New(h.store).CreateEnrollToken(operator.EnrollTokenCreateRequest{
		DisplayName: body.DisplayName, TTLSeconds: body.TTLSeconds,
		PreviewDigest: body.PreviewDigest, Reason: body.Reason,
		IdempotencyKey: r.Header.Get("Idempotency-Key"), Actor: actor,
	})
	if err != nil {
		status, code, detail := operator.HTTPError(err)
		if status == http.StatusInternalServerError {
			log.Printf("operator enrollment token create 失敗: %v", err)
		}
		var rejection *store.OperatorRequestError
		if errors.As(err, &rejection) && rejection.Replayed {
			w.Header().Set("Idempotency-Replayed", "true")
		}
		writeErr(w, status, code, detail)
		return
	}

	response := enrollTokenOperatorResponse{
		MachineID: result.MachineID, DisplayName: result.DisplayName,
		CreatedAt: result.CreatedAt, ExpiresAt: result.ExpiresAt,
		TTLSeconds: result.TTLSeconds, PreviewDigest: result.PreviewDigest,
		EnrollmentToken: result.EnrollmentToken, SecretAvailable: result.SecretAvailable,
		Replayed: result.Replayed, RecoveryRequired: result.RecoveryRequired,
		RecoveryAction: result.RecoveryAction,
	}
	if result.Replayed {
		// Fail closed if a future domain regression tries to combine replay and
		// credential delivery. Never serialize the secret even on this error path.
		response.EnrollmentToken = ""
		if result.EnrollmentToken != "" || result.SecretAvailable ||
			!result.RecoveryRequired || result.RecoveryAction != store.OperatorEnrollTokenRecoveryRevokeAndReissue {
			log.Printf("operator enrollment token replay result violated redaction contract")
			writeErr(w, http.StatusInternalServerError, "INTERNAL", "控制面操作失敗")
			return
		}
		w.Header().Set("Idempotency-Replayed", "true")
		writeJSON(w, http.StatusOK, response)
		return
	}
	if result.EnrollmentToken == "" || !result.SecretAvailable ||
		result.RecoveryRequired || result.RecoveryAction != "" {
		log.Printf("operator enrollment token fresh result violated one-time secret contract")
		writeErr(w, http.StatusInternalServerError, "INTERNAL", "控制面操作失敗")
		return
	}
	writeJSON(w, http.StatusCreated, response)
}

func (h *hub) handleGetOperatorMachineChannel(w http.ResponseWriter, r *http.Request) {
	result, err := operator.New(h.store).MachineChannel(r.PathValue("id"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, store.OperatorCodeMachineNotFound, "找不到這台機器")
			return
		}
		log.Printf("讀取 operator machine channel 失敗 machine=%s: %v", r.PathValue("id"), err)
		writeErr(w, http.StatusInternalServerError, "INTERNAL", "讀取 machine channel 失敗")
		return
	}
	w.Header().Set("ETag", fmt.Sprintf(`"channel-revision-%d"`, result.Revision))
	writeJSON(w, http.StatusOK, result)
}

func (h *hub) handlePutOperatorMachineChannel(w http.ResponseWriter, r *http.Request) {
	// Consume the boundary's verified principal exactly once. Transport
	// rejections and domain outcomes must describe the same authority decision;
	// the handler never performs a second whois lookup.
	actor := operatorActor(r)
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		h.rejectOperatorMachineChannelTransport(w, r, actor, http.StatusUnsupportedMediaType,
			"UNSUPPORTED_MEDIA_TYPE", "Content-Type 必須是 application/json")
		return
	}
	var body machineChannelOperatorRequest
	if rejection := decodeOperatorBody(w, r, &body); rejection != nil {
		h.rejectOperatorMachineChannelTransport(w, r, actor,
			rejection.Status, rejection.Code, rejection.Detail)
		return
	}
	result, err := operator.New(h.store).ChangeMachineChannel(operator.MachineChannelRequest{
		MachineID:          r.PathValue("id"),
		Channel:            body.Channel,
		ExpectedRevision:   body.ExpectedRevision,
		ConfirmDisplayName: body.ConfirmDisplayName,
		IdempotencyKey:     r.Header.Get("Idempotency-Key"),
		Actor:              actor,
	})
	if err != nil {
		status, code, detail := operator.HTTPError(err)
		if status == http.StatusInternalServerError {
			log.Printf("operator machine channel 失敗 machine=%s: %v", r.PathValue("id"), err)
		}
		var rejection *store.OperatorRequestError
		if errors.As(err, &rejection) && rejection.Replayed {
			w.Header().Set("Idempotency-Replayed", "true")
		}
		writeErr(w, status, code, detail)
		return
	}
	if result.Replayed {
		w.Header().Set("Idempotency-Replayed", "true")
	}
	w.Header().Set("ETag", fmt.Sprintf(`"channel-revision-%d"`, result.Revision))
	writeJSON(w, http.StatusOK, result)
}

type operatorTransportRejection struct {
	Status int
	Code   string
	Detail string
}

func decodeOperatorBody(w http.ResponseWriter, r *http.Request, dst any) *operatorTransportRejection {
	defer r.Body.Close()
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return &operatorTransportRejection{
				Status: http.StatusRequestEntityTooLarge, Code: "PAYLOAD_TOO_LARGE",
				Detail: "JSON body 不可超過 64 KiB",
			}
		}
		return &operatorTransportRejection{
			Status: http.StatusBadRequest, Code: "BAD_REQUEST", Detail: "讀取 JSON 失敗：" + err.Error(),
		}
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return &operatorTransportRejection{
			Status: http.StatusBadRequest, Code: "BAD_REQUEST", Detail: "JSON 最外層必須是 object",
		}
	}
	switch exactOperatorBodyFieldIssue(raw, dst) {
	case operatorBodyFieldUnknown:
		return &operatorTransportRejection{
			Status: http.StatusBadRequest, Code: "BAD_REQUEST",
			Detail: "JSON 含有未允許或大小寫不正確的 field",
		}
	case operatorBodyFieldDuplicate:
		return &operatorTransportRejection{
			Status: http.StatusBadRequest, Code: "BAD_REQUEST",
			Detail: "JSON field 不可重複",
		}
	case operatorBodyFieldNull:
		return &operatorTransportRejection{
			Status: http.StatusBadRequest, Code: "BAD_REQUEST",
			Detail: "JSON field 不可為 null",
		}
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return &operatorTransportRejection{
			Status: http.StatusBadRequest, Code: "BAD_REQUEST", Detail: "JSON 解析失敗：" + err.Error(),
		}
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return &operatorTransportRejection{
			Status: http.StatusBadRequest, Code: "BAD_REQUEST", Detail: "JSON body 後面還有多餘內容",
		}
	}
	return nil
}

// recordOperatorTransportRejection is the sole path for transport rejections
// into the ledger. Call sites still supply Action, Subject, and MachineID
// because each endpoint rejects under a different authority. Composing Detail
// here prevents call sites from giving operators different amounts of
// information for the same class of rejection. Audit details are truncated
// from the end to 500 characters before storage, so the classification prefix
// must come first to survive an oversized decoder-specific detail.
func (h *hub) recordOperatorTransportRejection(r *http.Request, actor operator.Actor,
	entry store.AuditEntry, code, detail string,
) {
	entry.IdempotencyKey = r.Header.Get("Idempotency-Key")
	entry.OK = false
	entry.Detail = store.OperatorTransportRejectionPrefix + code + ": " + detail +
		"；canonical request digest 無法取得"
	operator.ApplyActor(&entry, actor)
	if err := h.store.RecordAudit(entry); err != nil {
		log.Printf("⚠ operator transport rejection audit 寫不進去 action=%s code=%s: %v",
			entry.Action, code, err)
	}
}

type operatorBodyFieldIssue uint8

const (
	operatorBodyFieldsValid operatorBodyFieldIssue = iota
	operatorBodyFieldUnknown
	operatorBodyFieldDuplicate
	operatorBodyFieldNull
)

// exactOperatorBodyFieldIssue derives the only legal spellings from the
// concrete request struct's json tags. encoding/json deliberately accepts
// case-insensitive aliases and silently keeps the last duplicate value; both
// behaviours are too ambiguous for an idempotency digest and audit boundary.
//
// This scanner never returns a supplied field name. Callers persist transport
// rejection details, so reflecting attacker-controlled names into Detail
// would turn the permanent audit ledger into attacker-controlled storage.
func exactOperatorBodyFieldIssue(raw []byte, dst any) operatorBodyFieldIssue {
	allowed := exactOperatorJSONFieldNames(dst)
	dec := json.NewDecoder(bytes.NewReader(raw))
	first, err := dec.Token()
	if err != nil {
		return operatorBodyFieldsValid
	}
	if delim, ok := first.(json.Delim); !ok || delim != '{' {
		return operatorBodyFieldsValid
	}
	seen := make(map[string]bool)
	for dec.More() {
		nameToken, err := dec.Token()
		if err != nil {
			return operatorBodyFieldsValid
		}
		name, ok := nameToken.(string)
		if !ok {
			return operatorBodyFieldsValid
		}
		if !allowed[name] {
			return operatorBodyFieldUnknown
		}
		if seen[name] {
			return operatorBodyFieldDuplicate
		}
		seen[name] = true
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return operatorBodyFieldsValid
		}
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return operatorBodyFieldNull
		}
	}
	return operatorBodyFieldsValid
}

func exactOperatorJSONFieldNames(dst any) map[string]bool {
	allowed := make(map[string]bool)
	typ := reflect.TypeOf(dst)
	for typ != nil && typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	if typ == nil || typ.Kind() != reflect.Struct {
		return allowed
	}
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		if field.PkgPath != "" { // unexported
			continue
		}
		name := field.Name
		if tag, ok := field.Tag.Lookup("json"); ok {
			name, _, _ = strings.Cut(tag, ",")
			if name == "-" {
				continue
			}
			if name == "" {
				name = field.Name
			}
		}
		allowed[name] = true
	}
	return allowed
}

// rejectOperatorMachineChannelTransport audits bytes that cannot acquire a
// canonical operator meaning. It deliberately does not occupy the idempotency
// ledger: correcting JSON or Content-Type and retrying the same key is legal.
func (h *hub) rejectOperatorMachineChannelTransport(w http.ResponseWriter, r *http.Request,
	actor operator.Actor, status int, code, detail string,
) {
	machineID := r.PathValue("id")
	subject := machineID
	if machine, err := h.store.GetMachine(machineID); err == nil {
		subject = machine.DisplayName
	}
	h.recordOperatorTransportRejection(r, actor, store.AuditEntry{
		Action: store.AuditMachineChannel, MachineID: machineID, Subject: subject,
	}, code, detail)
	writeErr(w, status, code, detail)
}

// rejectOperatorEnrollTokenTransport records bytes that could not be assigned
// canonical create semantics. It intentionally leaves the idempotency ledger
// untouched, allowing the caller to correct the transport and reuse its key.
func (h *hub) rejectOperatorEnrollTokenTransport(w http.ResponseWriter, r *http.Request,
	actor operator.Actor, status int, code, detail string,
) {
	// Create display_name exists only in the request body, so a transport
	// rejection has no canonical identity. The Content-Type guard can return
	// before reading even valid JSON, making a name here an assertion rather
	// than an observation. Leave Subject empty for the CLI and console's
	// existing typed state; revocation deliberately differs because its
	// machine ID comes from the path.
	h.recordOperatorTransportRejection(r, actor, store.AuditEntry{
		Action: store.AuditEnrollToken, Subject: "",
	}, code, detail)
	writeErr(w, status, code, detail)
}

// rejectOperatorEnrollTokenRevocationTransport records only the fixed
// transport classification. Malformed caller bytes never occupy an
// idempotency key and are never reflected into the durable audit detail.
func (h *hub) rejectOperatorEnrollTokenRevocationTransport(w http.ResponseWriter, r *http.Request,
	actor operator.Actor, status int, code, detail string,
) {
	machineID := r.PathValue("id")
	subject := machineID
	if machine, err := h.store.GetMachine(machineID); err == nil {
		subject = machine.DisplayName
	}
	h.recordOperatorTransportRejection(r, actor, store.AuditEntry{
		Action: store.AuditRevokeToken, MachineID: machineID, Subject: subject,
	}, code, detail)
	writeErr(w, status, code, detail)
}

func operatorActor(r *http.Request) operator.Actor {
	return operator.ActorFromRequest(r, operator.SourceKindOperatorAPI)
}
