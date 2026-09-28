package main

import (
	"net/http"

	"github.com/teddashh/AI-Intune/internal/compliance"
	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/store"
)

type complianceRuleWire struct {
	Kind           string `json:"kind"`
	MaxAgeSeconds  int    `json:"max_age_seconds,omitempty"`
	AgentVersion   string `json:"agent_version,omitempty"`
	MinFreePercent int    `json:"min_free_percent,omitempty"`
}

type complianceActionWire struct {
	Kind         string `json:"kind"`
	GraceSeconds int    `json:"grace_seconds,omitempty"`
}

type compliancePolicyPreviewOperatorRequest struct {
	PolicyID string                 `json:"policy_id"`
	Rules    []complianceRuleWire   `json:"rules"`
	Actions  []complianceActionWire `json:"actions,omitempty"`
}

type compliancePolicyPublishOperatorRequest struct {
	PolicyID         string                 `json:"policy_id"`
	Rules            []complianceRuleWire   `json:"rules"`
	Actions          []complianceActionWire `json:"actions,omitempty"`
	ExpectedRevision *int64                 `json:"expected_revision"`
	PreviewDigest    string                 `json:"preview_digest"`
	ConfirmPolicyID  string                 `json:"confirm_policy_id"`
	Reason           string                 `json:"reason"`
}

type complianceAssignmentPreviewOperatorRequest struct {
	Scope    string `json:"scope"`
	ScopeID  string `json:"scope_id"`
	PolicyID string `json:"policy_id"`
	Revision int64  `json:"policy_revision"`
}

type complianceAssignmentOperatorRequest struct {
	Scope          string `json:"scope"`
	ScopeID        string `json:"scope_id"`
	PolicyID       string `json:"policy_id"`
	Revision       int64  `json:"policy_revision"`
	PreviewDigest  string `json:"preview_digest"`
	ConfirmScopeID string `json:"confirm_scope_id"`
	Reason         string `json:"reason"`
}

// policyFrom builds the domain value from wire fields. The schema version is
// supplied here rather than accepted from the caller, for the same reason the
// settings plane does it: a client that sends the wrong one is describing a
// document this Hub cannot evaluate, and the bounds check would report that as
// a range error instead of a version mismatch.
func policyFrom(rules []complianceRuleWire, actions []complianceActionWire) compliance.Policy {
	p := compliance.Policy{SchemaVersion: compliance.SchemaVersion}
	for _, r := range rules {
		p.Rules = append(p.Rules, compliance.Rule{
			Kind: compliance.RuleKind(r.Kind), MaxAgeSeconds: r.MaxAgeSeconds,
			AgentVersion: r.AgentVersion, MinFreePercent: r.MinFreePercent,
		})
	}
	for _, a := range actions {
		p.Actions = append(p.Actions, compliance.Action{
			Kind: compliance.ActionKind(a.Kind), GraceSeconds: a.GraceSeconds,
		})
	}
	return p
}

func (h *hub) handleGetOperatorCompliance(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.ForceQuery || r.URL.RawQuery != "" {
		writeErr(w, http.StatusBadRequest, "BAD_REQUEST", "compliance 目前不接受 query parameters")
		return
	}
	result, err := operator.New(h.store).ComplianceBoard()
	if err != nil {
		writeSettingErr(w, "compliance board", err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *hub) handlePreviewOperatorCompliancePolicy(w http.ResponseWriter, r *http.Request) {
	if !requireJSON(w, r) {
		return
	}
	var body compliancePolicyPreviewOperatorRequest
	if rejection := decodeOperatorBody(w, r, &body); rejection != nil {
		writeErr(w, rejection.Status, rejection.Code, rejection.Detail)
		return
	}
	result, err := operator.New(h.store).PreviewCompliancePolicy(operator.CompliancePolicyPreviewRequest{
		PolicyID: body.PolicyID, Policy: policyFrom(body.Rules, body.Actions),
	})
	if err != nil {
		writeSettingErr(w, "compliance policy preview", err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *hub) handlePublishOperatorCompliancePolicy(w http.ResponseWriter, r *http.Request) {
	actor := operatorActor(r)
	if !h.requireAuditedJSON(w, r, actor, store.AuditCompliancePolicy) {
		return
	}
	var body compliancePolicyPublishOperatorRequest
	if rejection := decodeOperatorBody(w, r, &body); rejection != nil {
		h.rejectOperatorPolicyTransport(w, r, actor, store.AuditCompliancePolicy,
			rejection.Status, rejection.Code, rejection.Detail)
		return
	}
	result, err := operator.New(h.store).PublishCompliancePolicy(operator.CompliancePolicyPublishRequest{
		PolicyID: body.PolicyID, Policy: policyFrom(body.Rules, body.Actions),
		ExpectedRevision: body.ExpectedRevision,
		PreviewDigest:    body.PreviewDigest, ConfirmPolicyID: body.ConfirmPolicyID,
		Reason: body.Reason, IdempotencyKey: r.Header.Get("Idempotency-Key"), Actor: actor,
	})
	if err != nil {
		writeSettingErr(w, "compliance policy publish", err)
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

func (h *hub) handlePreviewOperatorComplianceAssignment(w http.ResponseWriter, r *http.Request) {
	if !requireJSON(w, r) {
		return
	}
	var body complianceAssignmentPreviewOperatorRequest
	if rejection := decodeOperatorBody(w, r, &body); rejection != nil {
		writeErr(w, rejection.Status, rejection.Code, rejection.Detail)
		return
	}
	result, err := operator.New(h.store).PreviewComplianceAssignment(operator.ComplianceAssignmentPreviewRequest{
		Scope: compliance.Scope(body.Scope), ScopeID: body.ScopeID,
		PolicyID: body.PolicyID, Revision: body.Revision,
	})
	if err != nil {
		writeSettingErr(w, "compliance assignment preview", err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *hub) handleCreateOperatorComplianceAssignment(w http.ResponseWriter, r *http.Request) {
	actor := operatorActor(r)
	if !h.requireAuditedJSON(w, r, actor, store.AuditComplianceAssign) {
		return
	}
	var body complianceAssignmentOperatorRequest
	if rejection := decodeOperatorBody(w, r, &body); rejection != nil {
		h.rejectOperatorPolicyTransport(w, r, actor, store.AuditComplianceAssign,
			rejection.Status, rejection.Code, rejection.Detail)
		return
	}
	result, err := operator.New(h.store).AssignCompliancePolicy(operator.ComplianceAssignmentRequest{
		Scope: compliance.Scope(body.Scope), ScopeID: body.ScopeID,
		PolicyID: body.PolicyID, Revision: body.Revision,
		PreviewDigest: body.PreviewDigest, ConfirmScopeID: body.ConfirmScopeID,
		Reason: body.Reason, IdempotencyKey: r.Header.Get("Idempotency-Key"), Actor: actor,
	})
	if err != nil {
		writeSettingErr(w, "compliance assignment", err)
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
