package main

import (
	"errors"
	"log"
	"mime"
	"net/http"

	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/settingpolicy"
	"github.com/teddashh/AI-Intune/internal/store"
)

type settingPolicyPreviewOperatorRequest struct {
	PolicyID                   string `json:"policy_id"`
	CheckinIntervalSeconds     int    `json:"checkin_interval_seconds"`
	ObservationIntervalSeconds int    `json:"observation_interval_seconds"`
}

type settingPolicyPublishOperatorRequest struct {
	PolicyID                   string `json:"policy_id"`
	CheckinIntervalSeconds     int    `json:"checkin_interval_seconds"`
	ObservationIntervalSeconds int    `json:"observation_interval_seconds"`
	ExpectedRevision           *int64 `json:"expected_revision"`
	PreviewDigest              string `json:"preview_digest"`
	ConfirmPolicyID            string `json:"confirm_policy_id"`
	Reason                     string `json:"reason"`
}

type settingAssignmentPreviewOperatorRequest struct {
	Scope    string `json:"scope"`
	ScopeID  string `json:"scope_id"`
	PolicyID string `json:"policy_id"`
	Revision int64  `json:"policy_revision"`
}

type settingAssignmentOperatorRequest struct {
	Scope          string `json:"scope"`
	ScopeID        string `json:"scope_id"`
	PolicyID       string `json:"policy_id"`
	Revision       int64  `json:"policy_revision"`
	PreviewDigest  string `json:"preview_digest"`
	ConfirmScopeID string `json:"confirm_scope_id"`
	Reason         string `json:"reason"`
}

// settingsFrom builds the domain value from wire fields. The schema version is
// supplied here rather than accepted from the caller: a client that sends the
// wrong one is describing a document this Hub cannot run, and the bounds check
// would report that as a range error instead of a version mismatch.
func settingsFrom(checkin, observation int) settingpolicy.Settings {
	return settingpolicy.Settings{SchemaVersion: settingpolicy.SchemaVersion,
		CheckinIntervalSeconds: checkin, ObservationIntervalSeconds: observation}
}

func requireJSON(w http.ResponseWriter, r *http.Request) bool {
	w.Header().Set("Cache-Control", "no-store")
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeErr(w, http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE",
			"Content-Type 必須是 application/json")
		return false
	}
	return true
}

// rejectOperatorPolicyTransport records the four compliance and setting
// policy/assignment attempts that never acquired canonical meaning. It
// deliberately leaves the idempotency ledger untouched: correcting the
// transport and reusing the same key is legal.
func (h *hub) rejectOperatorPolicyTransport(w http.ResponseWriter, r *http.Request,
	actor operator.Actor, action store.AuditAction, status int, code, detail string,
) {
	// policy_id and scope_id exist only in the request body, so a transport
	// rejection has no canonical subject; leave it empty for the CLI and
	// console's existing typed unreadable-subject state.
	h.recordOperatorTransportRejection(r, actor, store.AuditEntry{
		Action: action, Subject: "",
	}, code, detail)
	writeErr(w, status, code, detail)
}

// requireAuditedJSON is requireJSON for those same four endpoints. The
// Content-Type arm is the one automation trips most often, so leaving it
// silent while auditing the decoder arm would hide the more common refusal.
func (h *hub) requireAuditedJSON(w http.ResponseWriter, r *http.Request,
	actor operator.Actor, action store.AuditAction,
) bool {
	w.Header().Set("Cache-Control", "no-store")
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		h.rejectOperatorPolicyTransport(w, r, actor, action,
			http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE",
			"Content-Type 必須是 application/json")
		return false
	}
	return true
}

func writeSettingErr(w http.ResponseWriter, what string, err error) {
	status, code, detail := operator.HTTPError(err)
	if status == http.StatusInternalServerError {
		log.Printf("operator %s failed: %v", what, err)
	}
	// A cached rejection is still a replay: the caller retried a key that was
	// already decided, and the client must be able to tell that apart from a
	// second decision reaching the ledger.
	var rejection *store.OperatorRequestError
	if errors.As(err, &rejection) && rejection.Replayed {
		w.Header().Set("Idempotency-Replayed", "true")
	}
	writeErr(w, status, code, detail)
}

func (h *hub) handleGetOperatorSettings(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.ForceQuery || r.URL.RawQuery != "" {
		writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "settings 目前不接受 query parameters")
		return
	}
	result, err := operator.New(h.store).SettingBoard()
	if err != nil {
		writeSettingErr(w, "setting board", err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *hub) handlePreviewOperatorSettingPolicy(w http.ResponseWriter, r *http.Request) {
	if !requireJSON(w, r) {
		return
	}
	var body settingPolicyPreviewOperatorRequest
	if rejection := decodeOperatorBody(w, r, &body); rejection != nil {
		writeErr(w, rejection.Status, rejection.Code, rejection.Detail)
		return
	}
	result, err := operator.New(h.store).PreviewSettingPolicy(operator.SettingPolicyPreviewRequest{
		PolicyID: body.PolicyID,
		Settings: settingsFrom(body.CheckinIntervalSeconds, body.ObservationIntervalSeconds),
	})
	if err != nil {
		writeSettingErr(w, "setting policy preview", err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *hub) handlePublishOperatorSettingPolicy(w http.ResponseWriter, r *http.Request) {
	actor := operatorActor(r)
	if !h.requireAuditedJSON(w, r, actor, store.AuditSettingPolicy) {
		return
	}
	var body settingPolicyPublishOperatorRequest
	if rejection := decodeOperatorBody(w, r, &body); rejection != nil {
		h.rejectOperatorPolicyTransport(w, r, actor, store.AuditSettingPolicy,
			rejection.Status, rejection.Code, rejection.Detail)
		return
	}
	result, err := operator.New(h.store).PublishSettingPolicy(operator.SettingPolicyPublishRequest{
		PolicyID:         body.PolicyID,
		Settings:         settingsFrom(body.CheckinIntervalSeconds, body.ObservationIntervalSeconds),
		ExpectedRevision: body.ExpectedRevision,
		PreviewDigest:    body.PreviewDigest, ConfirmPolicyID: body.ConfirmPolicyID,
		Reason: body.Reason, IdempotencyKey: r.Header.Get("Idempotency-Key"), Actor: actor,
	})
	if err != nil {
		writeSettingErr(w, "setting policy publish", err)
		return
	}
	status := http.StatusCreated
	if result.Replayed || result.Unchanged {
		status = http.StatusOK
	}
	if result.Replayed {
		w.Header().Set("Idempotency-Replayed", "true")
	}
	writeJSON(w, status, result)
}

func (h *hub) handlePreviewOperatorSettingAssignment(w http.ResponseWriter, r *http.Request) {
	if !requireJSON(w, r) {
		return
	}
	var body settingAssignmentPreviewOperatorRequest
	if rejection := decodeOperatorBody(w, r, &body); rejection != nil {
		writeErr(w, rejection.Status, rejection.Code, rejection.Detail)
		return
	}
	result, err := operator.New(h.store).PreviewSettingAssignment(operator.SettingAssignmentPreviewRequest{
		Scope: settingpolicy.Scope(body.Scope), ScopeID: body.ScopeID,
		PolicyID: body.PolicyID, Revision: body.Revision,
	})
	if err != nil {
		writeSettingErr(w, "setting assignment preview", err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *hub) handleCreateOperatorSettingAssignment(w http.ResponseWriter, r *http.Request) {
	actor := operatorActor(r)
	if !h.requireAuditedJSON(w, r, actor, store.AuditSettingAssign) {
		return
	}
	var body settingAssignmentOperatorRequest
	if rejection := decodeOperatorBody(w, r, &body); rejection != nil {
		h.rejectOperatorPolicyTransport(w, r, actor, store.AuditSettingAssign,
			rejection.Status, rejection.Code, rejection.Detail)
		return
	}
	result, err := operator.New(h.store).AssignSettingPolicy(operator.SettingAssignmentRequest{
		Scope: settingpolicy.Scope(body.Scope), ScopeID: body.ScopeID,
		PolicyID: body.PolicyID, Revision: body.Revision,
		PreviewDigest: body.PreviewDigest, ConfirmScopeID: body.ConfirmScopeID,
		Reason: body.Reason, IdempotencyKey: r.Header.Get("Idempotency-Key"), Actor: actor,
	})
	if err != nil {
		writeSettingErr(w, "setting assignment", err)
		return
	}
	status := http.StatusCreated
	if result.Replayed || result.Unchanged {
		status = http.StatusOK
	}
	if result.Replayed {
		w.Header().Set("Idempotency-Replayed", "true")
	}
	writeJSON(w, status, result)
}
