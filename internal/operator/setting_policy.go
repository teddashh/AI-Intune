package operator

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/teddashh/AI-Intune/internal/settingpolicy"
	"github.com/teddashh/AI-Intune/internal/store"
)

// ---------------------------------------------------------------- 發佈

type SettingPolicyPreviewRequest struct {
	PolicyID string
	Settings settingpolicy.Settings
}

// SettingPolicyPreviewResult is what the operator confirms. It states the
// values, the revision they would become, and whether anything would change.
type SettingPolicyPreviewResult struct {
	PolicyID      string                 `json:"policy_id"`
	CurrentRev    int64                  `json:"current_revision"`
	NextRev       int64                  `json:"next_revision"`
	Settings      settingpolicy.Settings `json:"settings"`
	Digest        string                 `json:"settings_digest"`
	PreviewDigest string                 `json:"preview_digest"`
	Unchanged     bool                   `json:"unchanged"`
	// AffectedMachines is how many machines resolve to this policy today. It is
	// the impact line: publishing a new revision re-paces exactly these.
	AffectedMachines int `json:"affected_machines"`
}

type SettingPolicyPublishRequest struct {
	PolicyID         string
	Settings         settingpolicy.Settings
	ExpectedRevision *int64
	PreviewDigest    string
	ConfirmPolicyID  string
	Reason           string
	IdempotencyKey   string
	Actor            Actor
}

type SettingPolicyPublishResult = store.OperatorSettingPolicyResult

func (s *Service) PreviewSettingPolicy(req SettingPolicyPreviewRequest) (SettingPolicyPreviewResult, error) {
	if err := req.Settings.Validate(); err != nil {
		return SettingPolicyPreviewResult{}, &store.OperatorRequestError{
			Code:   store.OperatorCodeSettingPolicyInvalid,
			Detail: trimSettingError(err),
		}
	}
	out := SettingPolicyPreviewResult{PolicyID: req.PolicyID, Settings: req.Settings}
	digest, err := settingpolicy.Digest(req.Settings)
	if err != nil {
		return SettingPolicyPreviewResult{}, err
	}
	out.Digest = digest

	revs, err := s.store.SettingPolicyRevisions(req.PolicyID)
	switch {
	case err == nil:
		out.CurrentRev = revs[0].Revision
		out.Unchanged = revs[0].Digest == digest
	case errors.Is(err, store.ErrSettingPolicyNotFound):
	default:
		return SettingPolicyPreviewResult{}, err
	}
	out.NextRev = out.CurrentRev
	if !out.Unchanged {
		out.NextRev = out.CurrentRev + 1
	}
	if out.PreviewDigest, err = store.SettingPolicyPreviewDigest(req.PolicyID, out.CurrentRev, req.Settings); err != nil {
		return SettingPolicyPreviewResult{}, err
	}
	if out.AffectedMachines, err = s.settingPolicyReach(req.PolicyID); err != nil {
		return SettingPolicyPreviewResult{}, err
	}
	return out, nil
}

func (s *Service) PublishSettingPolicy(req SettingPolicyPublishRequest) (SettingPolicyPublishResult, error) {
	digest := settingPolicySemanticDigest(req)
	auditBase := auditFromActor(req.Actor)
	auditBase.Reason = req.Reason
	auditBase.IdempotencyKey = req.IdempotencyKey
	auditBase.RequestDigest = digest
	result, err := s.store.ApplyOperatorSettingPolicy(store.OperatorSettingPolicyRequest{
		PolicyID: req.PolicyID, Settings: req.Settings, ExpectedRevision: req.ExpectedRevision,
		PreviewDigest: req.PreviewDigest, ConfirmPolicyID: req.ConfirmPolicyID,
		Reason: req.Reason, PublishedBy: settingActorLabel(req.Actor),
		IdempotencyKey: req.IdempotencyKey, RequestDigest: digest, Audit: auditBase,
	})
	s.recordSettingFallback(req.Actor, store.AuditSettingPolicy, req.PolicyID,
		req.Reason, req.IdempotencyKey, digest, result.Audited, result.Replayed,
		"沒有再次發佈新的 revision", err)
	return result, err
}

// ---------------------------------------------------------------- 指派

type SettingAssignmentPreviewRequest struct {
	Scope    settingpolicy.Scope
	ScopeID  string
	PolicyID string
	Revision int64
}

type SettingAssignmentPreviewResult struct {
	Scope         settingpolicy.Scope    `json:"scope"`
	ScopeID       string                 `json:"scope_id"`
	ScopeLabel    string                 `json:"scope_label"`
	PolicyID      string                 `json:"policy_id"`
	Revision      int64                  `json:"policy_revision"`
	Settings      settingpolicy.Settings `json:"settings"`
	Digest        string                 `json:"settings_digest"`
	PreviewDigest string                 `json:"preview_digest"`
	Unchanged     bool                   `json:"unchanged"`
	// Current is what the scope resolves to before this assignment.
	CurrentPolicyID string `json:"current_policy_id,omitempty"`
	CurrentRevision int64  `json:"current_policy_revision,omitempty"`
	// AffectedMachines is how many machines this scope covers today.
	AffectedMachines int `json:"affected_machines"`
}

type SettingAssignmentRequest struct {
	Scope          settingpolicy.Scope
	ScopeID        string
	PolicyID       string
	Revision       int64
	PreviewDigest  string
	ConfirmScopeID string
	Reason         string
	IdempotencyKey string
	Actor          Actor
}

type SettingAssignmentResult = store.OperatorSettingAssignmentResult

func (s *Service) PreviewSettingAssignment(req SettingAssignmentPreviewRequest) (SettingAssignmentPreviewResult, error) {
	revs, err := s.store.SettingPolicyRevisions(req.PolicyID)
	if err != nil {
		if errors.Is(err, store.ErrSettingPolicyNotFound) {
			return SettingAssignmentPreviewResult{}, &store.OperatorRequestError{
				Code:   store.OperatorCodeSettingPolicyNotFound,
				Detail: "找不到設定原則 " + req.PolicyID + "；先發佈它，再指派",
			}
		}
		return SettingAssignmentPreviewResult{}, err
	}
	var chosen *store.SettingPolicyRecord
	for i := range revs {
		if revs[i].Revision == req.Revision {
			chosen = &revs[i]
			break
		}
	}
	if chosen == nil {
		return SettingAssignmentPreviewResult{}, &store.OperatorRequestError{
			Code: store.OperatorCodeSettingPolicyNotFound,
			Detail: fmt.Sprintf("%s 沒有 revision %d；目前最新的是 %d",
				req.PolicyID, req.Revision, revs[0].Revision),
		}
	}

	out := SettingAssignmentPreviewResult{Scope: req.Scope, ScopeID: req.ScopeID,
		PolicyID: req.PolicyID, Revision: req.Revision, Settings: chosen.Settings,
		Digest: chosen.Digest,
		PreviewDigest: store.SettingAssignmentPreviewDigest(req.Scope, req.ScopeID,
			req.PolicyID, req.Revision),
	}
	out.ScopeLabel = settingScopeLabel(req.Scope)

	current, err := s.store.CurrentSettingAssignments()
	if err != nil {
		return SettingAssignmentPreviewResult{}, err
	}
	for _, a := range current {
		if a.Scope == req.Scope && a.ScopeID == req.ScopeID {
			out.CurrentPolicyID, out.CurrentRevision = a.PolicyID, a.PolicyRev
			out.Unchanged = a.Digest == chosen.Digest
			break
		}
	}
	if out.AffectedMachines, err = s.settingScopeReach(req.Scope, req.ScopeID); err != nil {
		return SettingAssignmentPreviewResult{}, err
	}
	return out, nil
}

func (s *Service) AssignSettingPolicy(req SettingAssignmentRequest) (SettingAssignmentResult, error) {
	digest := settingAssignmentSemanticDigest(req)
	auditBase := auditFromActor(req.Actor)
	auditBase.Reason = req.Reason
	auditBase.IdempotencyKey = req.IdempotencyKey
	auditBase.RequestDigest = digest
	result, err := s.store.ApplyOperatorSettingAssignment(store.OperatorSettingAssignmentRequest{
		Scope: req.Scope, ScopeID: req.ScopeID, PolicyID: req.PolicyID,
		PolicyRevision: req.Revision, PreviewDigest: req.PreviewDigest,
		ConfirmScopeID: req.ConfirmScopeID, Reason: req.Reason,
		AssignedBy: settingActorLabel(req.Actor), IdempotencyKey: req.IdempotencyKey,
		RequestDigest: digest, Audit: auditBase,
	})
	s.recordSettingFallback(req.Actor, store.AuditSettingAssign,
		string(req.Scope)+":"+req.ScopeID, req.Reason, req.IdempotencyKey, digest,
		result.Audited, result.Replayed, "沒有再次寫入指派", err)
	return result, err
}

// ---------------------------------------------------------------- 讀

// SettingBoardMachine is one row of the applied-state board.
type SettingBoardMachine struct {
	MachineID    string                 `json:"machine_id"`
	DisplayName  string                 `json:"display_name"`
	Channel      string                 `json:"channel,omitempty"`
	Source       settingpolicy.Source   `json:"source"`
	PolicyID     string                 `json:"policy_id,omitempty"`
	Revision     int64                  `json:"policy_revision,omitempty"`
	Settings     settingpolicy.Settings `json:"settings"`
	Verdict      settingpolicy.Verdict  `json:"verdict"`
	VerdictLabel string                 `json:"verdict_label"`
	ReportedAt   *time.Time             `json:"reported_at,omitempty"`
}

type SettingBoardResult struct {
	Policies    []store.SettingPolicySummary    `json:"policies"`
	Assignments []store.SettingAssignmentRecord `json:"assignments"`
	Machines    []SettingBoardMachine           `json:"machines"`
	Defaults    settingpolicy.Settings          `json:"defaults"`
	Counts      map[settingpolicy.Verdict]int   `json:"counts"`
}

func (s *Service) SettingBoard() (SettingBoardResult, error) {
	var out SettingBoardResult
	var err error
	if out.Policies, err = s.store.SettingPolicies(); err != nil {
		return SettingBoardResult{}, err
	}
	if out.Assignments, err = s.store.CurrentSettingAssignments(); err != nil {
		return SettingBoardResult{}, err
	}
	states, err := s.store.MachineSettingStates()
	if err != nil {
		return SettingBoardResult{}, err
	}
	out.Defaults = settingpolicy.Defaults()
	out.Counts = map[settingpolicy.Verdict]int{}
	// An empty fleet is an empty list, not an absent one: a null here would
	// read downstream as "the Hub could not answer".
	if out.Policies == nil {
		out.Policies = []store.SettingPolicySummary{}
	}
	if out.Assignments == nil {
		out.Assignments = []store.SettingAssignmentRecord{}
	}
	out.Machines = []SettingBoardMachine{}
	for _, st := range states {
		out.Machines = append(out.Machines, SettingBoardMachine{
			MachineID: st.MachineID, DisplayName: st.DisplayName, Channel: st.Channel,
			Source: st.Effective.Source, PolicyID: st.Effective.PolicyID,
			Revision: st.Effective.Revision, Settings: st.Effective.Settings,
			Verdict: st.Verdict, VerdictLabel: settingpolicy.Label(st.Verdict),
			ReportedAt: st.ReportedAt,
		})
		out.Counts[st.Verdict]++
	}
	return out, nil
}

// ---------------------------------------------------------------- helpers

// settingPolicyReach counts the machines that resolve to this policy today.
func (s *Service) settingPolicyReach(policyID string) (int, error) {
	states, err := s.store.MachineSettingStates()
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

// settingScopeReach counts the machines one scope covers, so the confirmation
// page states the blast radius before the operator presses.
func (s *Service) settingScopeReach(scope settingpolicy.Scope, scopeID string) (int, error) {
	if scope == settingpolicy.ScopeMachine {
		return 1, nil
	}
	states, err := s.store.MachineSettingStates()
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

func settingScopeLabel(scope settingpolicy.Scope) string {
	if scope == settingpolicy.ScopeChannel {
		return "channel"
	}
	return "裝置"
}

func trimSettingError(err error) string {
	const prefix = "settingpolicy: invalid settings: "
	msg := err.Error()
	if len(msg) > len(prefix) && msg[:len(prefix)] == prefix {
		return msg[len(prefix):]
	}
	return msg
}

func settingPolicySemanticDigest(req SettingPolicyPublishRequest) string {
	raw, err := req.Settings.Canonical()
	if err != nil {
		raw = []byte("invalid")
	}
	expected := int64(-1)
	if req.ExpectedRevision != nil {
		expected = *req.ExpectedRevision
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("setting-policy-publish:v1:%s:%d:%s:%s",
		req.PolicyID, expected, raw, req.Reason)))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func settingAssignmentSemanticDigest(req SettingAssignmentRequest) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("setting-assign-apply:v1:%s:%s:%s:%d:%s",
		req.Scope, req.ScopeID, req.PolicyID, req.Revision, req.Reason)))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// recordSettingFallback writes an audit row only when the writer transaction
// did not. The store records its own decision inside the transaction; writing
// a second row here would double-count every press on the audit page.
func (s *Service) recordSettingFallback(actor Actor, action store.AuditAction,
	subject, reason, key, digest string, audited, replayed bool, replayDetail string, err error) {
	entry := auditFromActor(actor)
	entry.Action = action
	entry.Subject = subject
	entry.Reason = reason
	entry.IdempotencyKey = key
	entry.RequestDigest = digest
	entry.OK = err == nil
	if err != nil {
		entry.Detail = err.Error()
		var rejection *store.OperatorRequestError
		if errors.As(err, &rejection) && rejection.Replayed {
			entry.Detail = store.OperatorIdempotencyReplayPrefix + "原判決：" + entry.Detail
		}
	} else if replayed {
		entry.Detail = store.OperatorIdempotencyReplayPrefix + replayDetail
	}
	recordOperatorFallbackAudit(s.store, entry, audited, err)
}

// settingActorLabel is the published_by/assigned_by value. It reuses the
// deployment rule so one operator reads the same way in every ledger.
func settingActorLabel(actor Actor) string {
	label, _ := deploymentCreatedBy(actor)
	return label
}
