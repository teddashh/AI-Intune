package operator

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/teddashh/AI-Intune/internal/compliance"
	"github.com/teddashh/AI-Intune/internal/store"
)

// ---------------------------------------------------------------- 發佈

type CompliancePolicyPreviewRequest struct {
	PolicyID string
	Policy   compliance.Policy
}

// CompliancePolicyPreviewResult is what the operator confirms. It states the
// rules, the revision they would become, and how many machines they would
// start judging.
type CompliancePolicyPreviewResult struct {
	PolicyID      string             `json:"policy_id"`
	CurrentRev    int64              `json:"current_revision"`
	NextRev       int64              `json:"next_revision"`
	Policy        compliance.Policy  `json:"policy"`
	Rules         []ComplianceRule   `json:"rules"`
	Actions       []ComplianceAction `json:"actions"`
	Digest        string             `json:"rules_digest"`
	PreviewDigest string             `json:"preview_digest"`
	Unchanged     bool               `json:"unchanged"`
	// AffectedMachines is how many machines this policy judges today.
	AffectedMachines int `json:"affected_machines"`
}

// ComplianceRule is one rule with the words the console shows for it.
type ComplianceRule struct {
	Kind  compliance.RuleKind `json:"kind"`
	Label string              `json:"label"`
	Bound string              `json:"bound"`
}

// ComplianceAction is one consequence with the words the console shows for it:
// what it does, and how long a machine may stay noncompliant before it does.
type ComplianceAction struct {
	Kind   compliance.ActionKind `json:"kind"`
	Label  string                `json:"label"`
	Effect string                `json:"effect"`
	Grace  string                `json:"grace"`
}

type CompliancePolicyPublishRequest struct {
	PolicyID         string
	Policy           compliance.Policy
	ExpectedRevision *int64
	PreviewDigest    string
	ConfirmPolicyID  string
	Reason           string
	IdempotencyKey   string
	Actor            Actor
}

type CompliancePolicyPublishResult = store.OperatorCompliancePolicyResult

func (s *Service) PreviewCompliancePolicy(req CompliancePolicyPreviewRequest) (CompliancePolicyPreviewResult, error) {
	if err := req.Policy.Validate(); err != nil {
		return CompliancePolicyPreviewResult{}, &store.OperatorRequestError{
			Code:   store.OperatorCodeCompliancePolicyInvalid,
			Detail: trimComplianceError(err),
		}
	}
	out := CompliancePolicyPreviewResult{PolicyID: req.PolicyID, Policy: req.Policy}
	digest, err := compliance.Digest(req.Policy)
	if err != nil {
		return CompliancePolicyPreviewResult{}, err
	}
	out.Digest = digest
	out.Rules = describeRules(req.Policy)
	out.Actions = describeActions(req.Policy)

	revs, err := s.store.CompliancePolicyRevisions(req.PolicyID)
	switch {
	case err == nil:
		out.CurrentRev = revs[0].Revision
		out.Unchanged = revs[0].Digest == digest
	case errors.Is(err, store.ErrCompliancePolicyNotFound):
	default:
		return CompliancePolicyPreviewResult{}, err
	}
	out.NextRev = out.CurrentRev
	if !out.Unchanged {
		out.NextRev = out.CurrentRev + 1
	}
	if out.PreviewDigest, err = store.CompliancePolicyPreviewDigest(req.PolicyID, out.CurrentRev, req.Policy); err != nil {
		return CompliancePolicyPreviewResult{}, err
	}
	if out.AffectedMachines, err = s.compliancePolicyReach(req.PolicyID); err != nil {
		return CompliancePolicyPreviewResult{}, err
	}
	return out, nil
}

func (s *Service) PublishCompliancePolicy(req CompliancePolicyPublishRequest) (CompliancePolicyPublishResult, error) {
	digest := compliancePolicySemanticDigest(req)
	auditBase := auditFromActor(req.Actor)
	auditBase.Reason = req.Reason
	auditBase.IdempotencyKey = req.IdempotencyKey
	auditBase.RequestDigest = digest
	result, err := s.store.ApplyOperatorCompliancePolicy(store.OperatorCompliancePolicyRequest{
		PolicyID: req.PolicyID, Policy: req.Policy, ExpectedRevision: req.ExpectedRevision,
		PreviewDigest: req.PreviewDigest, ConfirmPolicyID: req.ConfirmPolicyID,
		Reason: req.Reason, PublishedBy: settingActorLabel(req.Actor),
		IdempotencyKey: req.IdempotencyKey, RequestDigest: digest, Audit: auditBase,
	})
	s.recordSettingFallback(req.Actor, store.AuditCompliancePolicy, req.PolicyID,
		req.Reason, req.IdempotencyKey, digest, result.Audited, result.Replayed,
		"沒有再次發佈新的 revision", err)
	return result, err
}

// ---------------------------------------------------------------- 指派

type ComplianceAssignmentPreviewRequest struct {
	Scope    compliance.Scope
	ScopeID  string
	PolicyID string
	Revision int64
}

type ComplianceAssignmentPreviewResult struct {
	Scope         compliance.Scope   `json:"scope"`
	ScopeID       string             `json:"scope_id"`
	ScopeLabel    string             `json:"scope_label"`
	PolicyID      string             `json:"policy_id"`
	Revision      int64              `json:"policy_revision"`
	Policy        compliance.Policy  `json:"policy"`
	Rules         []ComplianceRule   `json:"rules"`
	Actions       []ComplianceAction `json:"actions"`
	Digest        string             `json:"rules_digest"`
	PreviewDigest string             `json:"preview_digest"`
	Unchanged     bool               `json:"unchanged"`
	// Current is what judges the scope before this assignment.
	CurrentPolicyID string `json:"current_policy_id,omitempty"`
	CurrentRevision int64  `json:"current_policy_revision,omitempty"`
	// AffectedMachines is how many machines this scope covers today.
	AffectedMachines int `json:"affected_machines"`
}

type ComplianceAssignmentRequest struct {
	Scope          compliance.Scope
	ScopeID        string
	PolicyID       string
	Revision       int64
	PreviewDigest  string
	ConfirmScopeID string
	Reason         string
	IdempotencyKey string
	Actor          Actor
}

type ComplianceAssignmentResult = store.OperatorComplianceAssignmentResult

func (s *Service) PreviewComplianceAssignment(req ComplianceAssignmentPreviewRequest) (ComplianceAssignmentPreviewResult, error) {
	revs, err := s.store.CompliancePolicyRevisions(req.PolicyID)
	if err != nil {
		if errors.Is(err, store.ErrCompliancePolicyNotFound) {
			return ComplianceAssignmentPreviewResult{}, &store.OperatorRequestError{
				Code:   store.OperatorCodeCompliancePolicyNotFound,
				Detail: "找不到合規性原則 " + req.PolicyID + "；先發佈它，再指派",
			}
		}
		return ComplianceAssignmentPreviewResult{}, err
	}
	var chosen *store.CompliancePolicyRecord
	for i := range revs {
		if revs[i].Revision == req.Revision {
			chosen = &revs[i]
			break
		}
	}
	if chosen == nil {
		return ComplianceAssignmentPreviewResult{}, &store.OperatorRequestError{
			Code: store.OperatorCodeCompliancePolicyNotFound,
			Detail: fmt.Sprintf("%s 沒有 revision %d；目前最新的是 %d",
				req.PolicyID, req.Revision, revs[0].Revision),
		}
	}

	out := ComplianceAssignmentPreviewResult{Scope: req.Scope, ScopeID: req.ScopeID,
		PolicyID: req.PolicyID, Revision: req.Revision, Policy: chosen.Policy,
		Rules: describeRules(chosen.Policy), Actions: describeActions(chosen.Policy),
		Digest: chosen.Digest,
		PreviewDigest: store.ComplianceAssignmentPreviewDigest(req.Scope, req.ScopeID,
			req.PolicyID, req.Revision),
	}
	out.ScopeLabel = complianceScopeLabel(req.Scope)

	current, err := s.store.CurrentComplianceAssignments()
	if err != nil {
		return ComplianceAssignmentPreviewResult{}, err
	}
	for _, a := range current {
		if a.Scope == req.Scope && a.ScopeID == req.ScopeID {
			out.CurrentPolicyID, out.CurrentRevision = a.PolicyID, a.PolicyRev
			out.Unchanged = a.Digest == chosen.Digest
			break
		}
	}
	if out.AffectedMachines, err = s.complianceScopeReach(req.Scope, req.ScopeID); err != nil {
		return ComplianceAssignmentPreviewResult{}, err
	}
	return out, nil
}

func (s *Service) AssignCompliancePolicy(req ComplianceAssignmentRequest) (ComplianceAssignmentResult, error) {
	digest := complianceAssignmentSemanticDigest(req)
	auditBase := auditFromActor(req.Actor)
	auditBase.Reason = req.Reason
	auditBase.IdempotencyKey = req.IdempotencyKey
	auditBase.RequestDigest = digest
	result, err := s.store.ApplyOperatorComplianceAssignment(store.OperatorComplianceAssignmentRequest{
		Scope: req.Scope, ScopeID: req.ScopeID, PolicyID: req.PolicyID,
		PolicyRevision: req.Revision, PreviewDigest: req.PreviewDigest,
		ConfirmScopeID: req.ConfirmScopeID, Reason: req.Reason,
		AssignedBy: settingActorLabel(req.Actor), IdempotencyKey: req.IdempotencyKey,
		RequestDigest: digest, Audit: auditBase,
	})
	s.recordSettingFallback(req.Actor, store.AuditComplianceAssign,
		string(req.Scope)+":"+req.ScopeID, req.Reason, req.IdempotencyKey, digest,
		result.Audited, result.Replayed, "沒有再次寫入指派", err)
	return result, err
}

// ---------------------------------------------------------------- 讀

// ComplianceRuleResult is one rule's verdict with the words for it.
type ComplianceRuleResult struct {
	Kind    compliance.RuleKind `json:"kind"`
	Label   string              `json:"label"`
	Outcome compliance.Outcome  `json:"outcome"`
	Detail  string              `json:"detail"`
}

// ComplianceBoardMachine is one row of the compliance board.
type ComplianceBoardMachine struct {
	MachineID    string                 `json:"machine_id"`
	DisplayName  string                 `json:"display_name"`
	Channel      string                 `json:"channel,omitempty"`
	Source       compliance.Source      `json:"source"`
	PolicyID     string                 `json:"policy_id,omitempty"`
	Revision     int64                  `json:"policy_revision,omitempty"`
	Verdict      compliance.Verdict     `json:"verdict"`
	VerdictLabel string                 `json:"verdict_label"`
	Results      []ComplianceRuleResult `json:"results"`
	// Actions is what the policy's consequences are doing to this machine
	// right now. Empty when the policy that judges it carries none.
	Actions    []ComplianceActionOutcome `json:"actions,omitempty"`
	ReportedAt *time.Time                `json:"reported_at,omitempty"`
}

// ComplianceActionOutcome is one consequence's current effect on one machine.
type ComplianceActionOutcome struct {
	Kind         compliance.ActionKind  `json:"kind"`
	Label        string                 `json:"label"`
	State        compliance.ActionState `json:"state"`
	StateLabel   string                 `json:"state_label"`
	DueAt        *time.Time             `json:"due_at,omitempty"`
	Since        *time.Time             `json:"since,omitempty"`
	SinceIsFloor bool                   `json:"since_is_floor,omitempty"`
}

type ComplianceBoardResult struct {
	Policies    []store.CompliancePolicySummary    `json:"policies"`
	Assignments []store.ComplianceAssignmentRecord `json:"assignments"`
	Machines    []ComplianceBoardMachine           `json:"machines"`
	Counts      map[compliance.Verdict]int         `json:"counts"`
	// Blocked is how many machines a compliance action is withholding work
	// from right now.
	Blocked     int       `json:"blocked"`
	EvaluatedAt time.Time `json:"evaluated_at"`
}

func (s *Service) ComplianceBoard() (ComplianceBoardResult, error) {
	var out ComplianceBoardResult
	var err error
	if out.Policies, err = s.store.CompliancePolicies(); err != nil {
		return ComplianceBoardResult{}, err
	}
	if out.Assignments, err = s.store.CurrentComplianceAssignments(); err != nil {
		return ComplianceBoardResult{}, err
	}
	states, err := s.store.MachineComplianceStates()
	if err != nil {
		return ComplianceBoardResult{}, err
	}
	out.Counts = map[compliance.Verdict]int{}
	// An empty fleet is an empty list, not an absent one: a null here would
	// read downstream as "the Hub could not answer".
	if out.Policies == nil {
		out.Policies = []store.CompliancePolicySummary{}
	}
	if out.Assignments == nil {
		out.Assignments = []store.ComplianceAssignmentRecord{}
	}
	out.Machines = []ComplianceBoardMachine{}
	for _, st := range states {
		out.Machines = append(out.Machines, complianceRow(st))
		out.Counts[st.Verdict]++
		if compliance.Blocks(st.Actions) {
			out.Blocked++
		}
		out.EvaluatedAt = st.EvaluatedAt
	}
	if out.EvaluatedAt.IsZero() {
		out.EvaluatedAt = s.store.Now().UTC()
	}
	return out, nil
}

// MachineCompliance answers one machine's verdict and the evidence for it.
func (s *Service) MachineCompliance(machineID string) (ComplianceBoardMachine, error) {
	state, err := s.store.ResolveMachineCompliance(machineID)
	if err != nil {
		return ComplianceBoardMachine{}, err
	}
	return complianceRow(state), nil
}

func complianceRow(st store.MachineComplianceState) ComplianceBoardMachine {
	row := ComplianceBoardMachine{
		MachineID: st.MachineID, DisplayName: st.DisplayName, Channel: st.Channel,
		Source: st.Effective.Source, PolicyID: st.Effective.PolicyID,
		Revision: st.Effective.Revision, Verdict: st.Verdict,
		VerdictLabel: compliance.Label(st.Verdict), ReportedAt: st.ReportedAt,
		Results: []ComplianceRuleResult{},
	}
	for _, r := range st.Results {
		row.Results = append(row.Results, ComplianceRuleResult{
			Kind: r.Kind, Label: compliance.RuleLabel(r.Kind),
			Outcome: r.Outcome, Detail: r.Detail,
		})
	}
	for _, a := range st.Actions {
		row.Actions = append(row.Actions, ComplianceActionOutcome{
			Kind: a.Kind, Label: compliance.ActionLabel(a.Kind),
			State: a.State, StateLabel: compliance.ActionStateLabel(a.State),
			DueAt: a.DueAt, Since: a.Since, SinceIsFloor: a.SinceIsFloor,
		})
	}
	return row
}

// ---------------------------------------------------------------- helpers

func describeRules(p compliance.Policy) []ComplianceRule {
	out := make([]ComplianceRule, 0, len(p.Rules))
	for _, r := range p.Rules {
		out = append(out, ComplianceRule{Kind: r.Kind,
			Label: compliance.RuleLabel(r.Kind), Bound: compliance.RuleBound(r)})
	}
	return out
}

func describeActions(p compliance.Policy) []ComplianceAction {
	out := make([]ComplianceAction, 0, len(p.Actions))
	for _, a := range p.Actions {
		out = append(out, ComplianceAction{Kind: a.Kind,
			Label:  compliance.ActionLabel(a.Kind),
			Effect: compliance.ActionEffect(a.Kind),
			Grace:  complianceGraceLabel(a.GraceSeconds)})
	}
	return out
}

// complianceGraceLabel puts a grace period in the words the console uses next
// to the action: when it starts, not how many seconds it is.
func complianceGraceLabel(seconds int) string {
	if seconds <= 0 {
		return "判定不符合就立即生效"
	}
	d := time.Duration(seconds) * time.Second
	switch {
	case d%(24*time.Hour) == 0:
		return fmt.Sprintf("連續不符合 %d 天後生效", int(d/(24*time.Hour)))
	case d%time.Hour == 0:
		return fmt.Sprintf("連續不符合 %d 小時後生效", int(d/time.Hour))
	case d%time.Minute == 0:
		return fmt.Sprintf("連續不符合 %d 分鐘後生效", int(d/time.Minute))
	default:
		return fmt.Sprintf("連續不符合 %d 秒後生效", seconds)
	}
}

// compliancePolicyReach counts the machines this policy judges today.
func (s *Service) compliancePolicyReach(policyID string) (int, error) {
	states, err := s.store.MachineComplianceStates()
	if err != nil {
		return 0, err
	}
	n := 0
	for _, st := range states {
		if st.Effective.PolicyID == policyID {
			n++
		}
	}
	return n, nil
}

// complianceScopeReach counts the machines one scope covers, so the
// confirmation page states the blast radius before the operator presses.
func (s *Service) complianceScopeReach(scope compliance.Scope, scopeID string) (int, error) {
	if scope == compliance.ScopeMachine {
		return 1, nil
	}
	states, err := s.store.MachineComplianceStates()
	if err != nil {
		return 0, err
	}
	n := 0
	for _, st := range states {
		if st.Channel == scopeID {
			n++
		}
	}
	return n, nil
}

func complianceScopeLabel(scope compliance.Scope) string {
	if scope == compliance.ScopeChannel {
		return "channel"
	}
	return "裝置"
}

func trimComplianceError(err error) string {
	const prefix = "compliance: invalid policy: "
	msg := err.Error()
	if len(msg) > len(prefix) && msg[:len(prefix)] == prefix {
		return msg[len(prefix):]
	}
	return msg
}

func compliancePolicySemanticDigest(req CompliancePolicyPublishRequest) string {
	raw, err := req.Policy.Canonical()
	if err != nil {
		raw = []byte("invalid")
	}
	expected := int64(-1)
	if req.ExpectedRevision != nil {
		expected = *req.ExpectedRevision
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("compliance-policy-publish:v1:%s:%d:%s:%s",
		req.PolicyID, expected, raw, req.Reason)))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func complianceAssignmentSemanticDigest(req ComplianceAssignmentRequest) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("compliance-assign-apply:v1:%s:%s:%s:%d:%s",
		req.Scope, req.ScopeID, req.PolicyID, req.Revision, req.Reason)))
	return "sha256:" + hex.EncodeToString(sum[:])
}
