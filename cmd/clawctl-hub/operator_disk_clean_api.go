package main

import (
	"net/http"
	"time"

	"github.com/teddashh/AI-Intune/internal/maintenance"
	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/store"
)

// Disk-clean operator JSON. Reads are view. Writes are admin and follow the
// setting-policy preview/apply shape: Idempotency-Key header, preview_digest
// in the body, fresh 201, replay or unchanged 200.

type diskCleanProfilePreviewBody struct {
	ScopeType string              `json:"scope_type"`
	ScopeID   string              `json:"scope_id"`
	Profile   maintenance.Profile `json:"profile"`
}

type diskCleanProfilePublishBody struct {
	ScopeType        string              `json:"scope_type"`
	ScopeID          string              `json:"scope_id"`
	Profile          maintenance.Profile `json:"profile"`
	ExpectedRevision *int64              `json:"expected_revision"`
	PreviewDigest    string              `json:"preview_digest"`
	ConfirmScopeID   string              `json:"confirm_scope_id"`
	Reason           string              `json:"reason"`
}

type diskCleanTargetPreviewBody struct {
	ScopeType  string   `json:"scope_type"`
	ScopeID    string   `json:"scope_id"`
	Revision   int64    `json:"revision"`
	MachineIDs []string `json:"machine_ids"`
}

type diskCleanTargetApplyBody struct {
	ScopeType      string   `json:"scope_type"`
	ScopeID        string   `json:"scope_id"`
	Revision       int64    `json:"revision"`
	MachineIDs     []string `json:"machine_ids"`
	PreviewDigest  string   `json:"preview_digest"`
	ConfirmScopeID string   `json:"confirm_scope_id"`
	Reason         string   `json:"reason"`
}

type diskCleanCanaryPreviewBody struct {
	ScopeType       string   `json:"scope_type"`
	ScopeID         string   `json:"scope_id"`
	Revision        int64    `json:"revision"`
	MachineIDs      []string `json:"machine_ids"`
	CanaryMachineID string   `json:"canary_machine_id"`
}

type diskCleanCanaryApplyBody struct {
	ScopeType       string   `json:"scope_type"`
	ScopeID         string   `json:"scope_id"`
	Revision        int64    `json:"revision"`
	MachineIDs      []string `json:"machine_ids"`
	CanaryMachineID string   `json:"canary_machine_id"`
	PreviewDigest   string   `json:"preview_digest"`
	ConfirmScopeID  string   `json:"confirm_scope_id"`
	Reason          string   `json:"reason"`
}

type diskCleanControlPreviewBody struct {
	RolloutID string `json:"rollout_id"`
}

type diskCleanControlApplyBody struct {
	RolloutID               string `json:"rollout_id"`
	ExpectedControlRevision int64  `json:"expected_control_revision"`
	ExpectedOpenedBatch     int    `json:"expected_opened_batch"`
	PreviewDigest           string `json:"preview_digest"`
	ConfirmRolloutID        string `json:"confirm_rollout_id"`
	Reason                  string `json:"reason"`
}

type diskCleanSummaryList struct {
	Items []store.DiskCleanSummaryView `json:"items"`
}

func (h *hub) handleListOperatorDiskCleanSummaries(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.ForceQuery || r.URL.RawQuery != "" {
		writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "disk-clean summaries do not accept query parameters")
		return
	}
	items, err := operator.New(h.store).DiskCleanSummaries(time.Now().UTC())
	if err != nil {
		writeSettingErr(w, "disk-clean summaries", err)
		return
	}
	writeJSON(w, http.StatusOK, diskCleanSummaryList{Items: items})
}

func (h *hub) handleGetOperatorDiskCleanSummary(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.ForceQuery || r.URL.RawQuery != "" {
		writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "disk-clean summaries do not accept query parameters")
		return
	}
	view, err := operator.New(h.store).DiskCleanSummary(r.PathValue("id"), time.Now().UTC())
	if err != nil {
		writeSettingErr(w, "disk-clean summary", err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (h *hub) handlePreviewOperatorDiskCleanProfile(w http.ResponseWriter, r *http.Request) {
	if !requireJSON(w, r) {
		return
	}
	var body diskCleanProfilePreviewBody
	if rejection := decodeOperatorBody(w, r, &body); rejection != nil {
		writeErr(w, rejection.Status, rejection.Code, rejection.Detail)
		return
	}
	result, err := operator.New(h.store).PreviewDiskCleanProfile(body.ScopeType, body.ScopeID, body.Profile)
	if err != nil {
		writeSettingErr(w, "disk-clean profile preview", err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *hub) handlePublishOperatorDiskCleanProfile(w http.ResponseWriter, r *http.Request) {
	actor := operatorActor(r)
	if !h.requireAuditedJSON(w, r, actor, store.AuditMaintenanceProfile) {
		return
	}
	var body diskCleanProfilePublishBody
	if rejection := decodeOperatorBody(w, r, &body); rejection != nil {
		h.rejectOperatorPolicyTransport(w, r, actor, store.AuditMaintenanceProfile,
			rejection.Status, rejection.Code, rejection.Detail)
		return
	}
	result, err := operator.New(h.store).PublishDiskCleanProfile(operator.DiskCleanProfilePublishRequest{
		ScopeType: body.ScopeType, ScopeID: body.ScopeID, Profile: body.Profile,
		ExpectedRevision: body.ExpectedRevision, PreviewDigest: body.PreviewDigest,
		ConfirmScopeID: body.ConfirmScopeID, Reason: body.Reason,
		IdempotencyKey: r.Header.Get("Idempotency-Key"), Actor: actor,
	})
	if err != nil {
		writeSettingErr(w, "disk-clean profile publish", err)
		return
	}
	writeDiskCleanResult(w, result.Replayed, result.Unchanged, result)
}

func (h *hub) handlePreviewOperatorDiskCleanDryRun(w http.ResponseWriter, r *http.Request) {
	if !requireJSON(w, r) {
		return
	}
	var body diskCleanTargetPreviewBody
	if rejection := decodeOperatorBody(w, r, &body); rejection != nil {
		writeErr(w, rejection.Status, rejection.Code, rejection.Detail)
		return
	}
	result, err := operator.New(h.store).PreviewDiskCleanDryRun(body.ScopeType, body.ScopeID, body.Revision, body.MachineIDs)
	if err != nil {
		writeSettingErr(w, "disk-clean dry-run preview", err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *hub) handleApplyOperatorDiskCleanDryRun(w http.ResponseWriter, r *http.Request) {
	actor := operatorActor(r)
	if !h.requireAuditedJSON(w, r, actor, store.AuditMaintenanceDryRun) {
		return
	}
	var body diskCleanTargetApplyBody
	if rejection := decodeOperatorBody(w, r, &body); rejection != nil {
		h.rejectOperatorPolicyTransport(w, r, actor, store.AuditMaintenanceDryRun,
			rejection.Status, rejection.Code, rejection.Detail)
		return
	}
	result, err := operator.New(h.store).ApplyDiskCleanDryRun(operator.DiskCleanTargetApplyRequest{
		ScopeType: body.ScopeType, ScopeID: body.ScopeID, Revision: body.Revision,
		MachineIDs: body.MachineIDs, PreviewDigest: body.PreviewDigest,
		ConfirmScopeID: body.ConfirmScopeID, Reason: body.Reason,
		IdempotencyKey: r.Header.Get("Idempotency-Key"), Actor: actor,
	})
	if err != nil {
		writeSettingErr(w, "disk-clean dry-run", err)
		return
	}
	writeDiskCleanResult(w, result.Replayed, false, result)
}

func (h *hub) handlePreviewOperatorDiskCleanCanary(w http.ResponseWriter, r *http.Request) {
	if !requireJSON(w, r) {
		return
	}
	var body diskCleanCanaryPreviewBody
	if rejection := decodeOperatorBody(w, r, &body); rejection != nil {
		writeErr(w, rejection.Status, rejection.Code, rejection.Detail)
		return
	}
	result, err := operator.New(h.store).PreviewDiskCleanCanary(
		body.ScopeType, body.ScopeID, body.Revision, body.CanaryMachineID, body.MachineIDs)
	if err != nil {
		writeSettingErr(w, "disk-clean canary preview", err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *hub) handleApplyOperatorDiskCleanCanary(w http.ResponseWriter, r *http.Request) {
	actor := operatorActor(r)
	if !h.requireAuditedJSON(w, r, actor, store.AuditMaintenanceCanary) {
		return
	}
	var body diskCleanCanaryApplyBody
	if rejection := decodeOperatorBody(w, r, &body); rejection != nil {
		h.rejectOperatorPolicyTransport(w, r, actor, store.AuditMaintenanceCanary,
			rejection.Status, rejection.Code, rejection.Detail)
		return
	}
	result, err := operator.New(h.store).ApplyDiskCleanCanary(operator.DiskCleanTargetApplyRequest{
		ScopeType: body.ScopeType, ScopeID: body.ScopeID, Revision: body.Revision,
		MachineIDs: body.MachineIDs, CanaryMachineID: body.CanaryMachineID,
		PreviewDigest: body.PreviewDigest, ConfirmScopeID: body.ConfirmScopeID,
		Reason: body.Reason, IdempotencyKey: r.Header.Get("Idempotency-Key"), Actor: actor,
	})
	if err != nil {
		writeSettingErr(w, "disk-clean canary", err)
		return
	}
	writeDiskCleanResult(w, result.Replayed, false, result)
}

func (h *hub) handlePreviewOperatorDiskCleanContinue(w http.ResponseWriter, r *http.Request) {
	h.previewDiskCleanControl(w, r, true)
}

func (h *hub) handlePreviewOperatorDiskCleanAbandon(w http.ResponseWriter, r *http.Request) {
	h.previewDiskCleanControl(w, r, false)
}

func (h *hub) previewDiskCleanControl(w http.ResponseWriter, r *http.Request, continueRollout bool) {
	if !requireJSON(w, r) {
		return
	}
	var body diskCleanControlPreviewBody
	if rejection := decodeOperatorBody(w, r, &body); rejection != nil {
		writeErr(w, rejection.Status, rejection.Code, rejection.Detail)
		return
	}
	svc := operator.New(h.store)
	var (
		result store.DiskCleanControlPreview
		err    error
	)
	if continueRollout {
		result, err = svc.PreviewDiskCleanContinue(body.RolloutID)
	} else {
		result, err = svc.PreviewDiskCleanAbandon(body.RolloutID)
	}
	if err != nil {
		writeSettingErr(w, "disk-clean control preview", err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *hub) handleContinueOperatorDiskClean(w http.ResponseWriter, r *http.Request) {
	h.applyDiskCleanControl(w, r, true)
}

func (h *hub) handleAbandonOperatorDiskClean(w http.ResponseWriter, r *http.Request) {
	h.applyDiskCleanControl(w, r, false)
}

func (h *hub) applyDiskCleanControl(w http.ResponseWriter, r *http.Request, continueRollout bool) {
	actor := operatorActor(r)
	action := store.AuditMaintenanceAbandon
	what := "disk-clean abandon"
	if continueRollout {
		action = store.AuditMaintenanceContinue
		what = "disk-clean continue"
	}
	if !h.requireAuditedJSON(w, r, actor, action) {
		return
	}
	var body diskCleanControlApplyBody
	if rejection := decodeOperatorBody(w, r, &body); rejection != nil {
		h.rejectOperatorPolicyTransport(w, r, actor, action, rejection.Status, rejection.Code, rejection.Detail)
		return
	}
	req := operator.DiskCleanControlApplyRequest{
		RolloutID: body.RolloutID, ExpectedControlRevision: body.ExpectedControlRevision,
		ExpectedOpenedBatch: body.ExpectedOpenedBatch, PreviewDigest: body.PreviewDigest,
		ConfirmRolloutID: body.ConfirmRolloutID, Reason: body.Reason,
		IdempotencyKey: r.Header.Get("Idempotency-Key"), Actor: actor,
	}
	svc := operator.New(h.store)
	var (
		result store.DiskCleanRolloutResult
		err    error
	)
	if continueRollout {
		result, err = svc.ApplyDiskCleanContinue(req)
	} else {
		result, err = svc.ApplyDiskCleanAbandon(req)
	}
	if err != nil {
		writeSettingErr(w, what, err)
		return
	}
	writeDiskCleanResult(w, result.Replayed, false, result)
}

func writeDiskCleanResult(w http.ResponseWriter, replayed, unchanged bool, body any) {
	status := http.StatusCreated
	if replayed || unchanged {
		status = http.StatusOK
	}
	if replayed {
		w.Header().Set("Idempotency-Replayed", "true")
	}
	writeJSON(w, status, body)
}
