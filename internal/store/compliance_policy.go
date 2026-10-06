package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/teddashh/AI-Intune/internal/compliance"
)

var (
	ErrCompliancePolicyNotFound = errors.New("store: compliance policy not found")
)

// CompliancePolicyRecord is one published, immutable revision of a rule set.
type CompliancePolicyRecord struct {
	PolicyID    string            `json:"policy_id"`
	Revision    int64             `json:"policy_revision"`
	Policy      compliance.Policy `json:"policy"`
	Digest      string            `json:"rules_digest"`
	PublishedAt time.Time         `json:"published_at"`
	PublishedBy string            `json:"-"`
}

// ComplianceAssignmentRecord is one published revision pinned to one scope.
type ComplianceAssignmentRecord struct {
	AssignmentID string            `json:"assignment_id"`
	Scope        compliance.Scope  `json:"scope"`
	ScopeID      string            `json:"scope_id"`
	Revision     int64             `json:"assignment_revision"`
	PolicyID     string            `json:"policy_id"`
	PolicyRev    int64             `json:"policy_revision"`
	Digest       string            `json:"rules_digest"`
	Policy       compliance.Policy `json:"policy"`
	AssignedAt   time.Time         `json:"assigned_at"`
	AssignedBy   string            `json:"-"`
}

// ------------------------------------------------------------ 發佈合規性原則

type OperatorCompliancePolicyRequest struct {
	PolicyID         string
	Policy           compliance.Policy
	ExpectedRevision *int64
	PreviewDigest    string
	ConfirmPolicyID  string
	Reason           string
	PublishedBy      string
	IdempotencyKey   string
	RequestDigest    string
	Audit            AuditEntry
}

type OperatorCompliancePolicyResult struct {
	PolicyID    string            `json:"policy_id"`
	Revision    int64             `json:"policy_revision"`
	Digest      string            `json:"rules_digest"`
	Policy      compliance.Policy `json:"policy"`
	PublishedAt time.Time         `json:"published_at"`
	Unchanged   bool              `json:"unchanged"`
	Replayed    bool              `json:"replayed"`
	Audited     bool              `json:"-"`
}

// CompliancePolicyPreviewDigest is what an operator confirms before publishing.
// It covers the id, the expected revision and the rules, so a preview taken
// against one revision cannot be confirmed after somebody else published.
func CompliancePolicyPreviewDigest(policyID string, expected int64, p compliance.Policy) (string, error) {
	raw, err := p.Canonical()
	if err != nil {
		return "", err
	}
	return digestOf(fmt.Sprintf("compliance-policy:v1:%s:%d:%s", policyID, expected, raw)), nil
}

// ApplyOperatorCompliancePolicy publishes one immutable revision of a rule set.
//
// 重新發佈同一組規則不會產生新的 revision：判決不會因此改變，而每一台被指派
// 的機器都得重新解釋自己為什麼還是那個判決。
func (s *Store) ApplyOperatorCompliancePolicy(req OperatorCompliancePolicyRequest) (OperatorCompliancePolicyResult, error) {
	if strings.TrimSpace(req.IdempotencyKey) == "" || len(req.IdempotencyKey) > 200 {
		return OperatorCompliancePolicyResult{}, operatorError(
			OperatorCodeIdempotencyKeyRequired, "Idempotency-Key 不可省略且最多 200 bytes")
	}
	if req.RequestDigest == "" {
		return OperatorCompliancePolicyResult{}, operatorError(
			OperatorCodeRequestDigestRequired, "request body digest 不可省略")
	}

	operation := "compliance-policy:" + req.PolicyID
	audit := req.Audit
	audit.Action = AuditCompliancePolicy
	audit.IdempotencyKey = req.IdempotencyKey
	audit.RequestDigest = req.RequestDigest
	audit.Subject = req.PolicyID
	audit.Reason = req.Reason

	tx, err := s.beginWrite(context.Background(), "apply_operator_compliance_policy")
	if err != nil {
		return OperatorCompliancePolicyResult{}, fmt.Errorf("store: begin compliance policy: %w", err)
	}
	defer tx.Rollback()

	replayed, rejected, err := s.policyIdempotency(tx, req.IdempotencyKey, operation, req.RequestDigest, &audit,
		func(raw string) (any, error) {
			var out OperatorCompliancePolicyResult
			if err := json.Unmarshal([]byte(raw), &out); err != nil {
				return nil, err
			}
			out.Replayed, out.Audited = true, true
			return out, nil
		}, "沒有再次發佈新的 revision")
	if err != nil {
		return OperatorCompliancePolicyResult{}, err
	}
	if rejected != nil {
		return OperatorCompliancePolicyResult{}, rejected
	}
	if replayed != nil {
		return replayed.(OperatorCompliancePolicyResult), nil
	}

	reject := func(code, detail string) (OperatorCompliancePolicyResult, error) {
		err := s.policyReject(tx, req.IdempotencyKey, operation, req.RequestDigest, &audit, code, detail)
		return OperatorCompliancePolicyResult{}, err
	}

	if err := validPolicyID(req.PolicyID); err != nil {
		return reject(OperatorCodeCompliancePolicyInvalid, err.Error())
	}
	if strings.TrimSpace(req.Reason) == "" {
		return reject(OperatorCodeReasonRequired, "reason 不可省略：這一份規則會改變機隊的判決")
	}
	if req.ConfirmPolicyID != req.PolicyID {
		return reject(OperatorCodeConfirmationMismatch,
			fmt.Sprintf("確認欄位打的是 %q，不是 %q", req.ConfirmPolicyID, req.PolicyID))
	}
	if err := req.Policy.Validate(); err != nil {
		return reject(OperatorCodeCompliancePolicyInvalid,
			strings.TrimPrefix(err.Error(), "compliance: invalid policy: "))
	}
	if req.ExpectedRevision == nil {
		return reject(OperatorCodePreconditionRequired, "expected_revision 不可省略")
	}

	var current int64
	var currentDigest sql.NullString
	if err := tx.QueryRow(`SELECT policy_revision,rules_digest FROM compliance_policies
	 WHERE policy_id=? ORDER BY policy_revision DESC LIMIT 1`, req.PolicyID).Scan(&current, &currentDigest); err != nil &&
		!errors.Is(err, sql.ErrNoRows) {
		return OperatorCompliancePolicyResult{}, fmt.Errorf("store: inspect compliance policy: %w", err)
	}
	if *req.ExpectedRevision != current {
		return reject(OperatorCodeCompliancePolicyConflict, fmt.Sprintf(
			"%s 目前的 revision 是 %d，不是 request 預期的 %d；請重新讀取後再發佈",
			req.PolicyID, current, *req.ExpectedRevision))
	}

	want, err := CompliancePolicyPreviewDigest(req.PolicyID, current, req.Policy)
	if err != nil {
		return OperatorCompliancePolicyResult{}, fmt.Errorf("store: compliance policy preview digest: %w", err)
	}
	if req.PreviewDigest != want {
		return reject(OperatorCodeCompliancePreviewStale,
			"預覽已過期：這一份規則或它的 revision 在你按下確認之前變了；請重新預覽")
	}

	digest, err := compliance.Digest(req.Policy)
	if err != nil {
		return OperatorCompliancePolicyResult{}, fmt.Errorf("store: compliance digest: %w", err)
	}
	raw, err := req.Policy.Canonical()
	if err != nil {
		return OperatorCompliancePolicyResult{}, fmt.Errorf("store: canonical compliance policy: %w", err)
	}
	// 存進去與回給操作員的都是正規化之後那一份，因為那才是 digest 算的東西。
	normalized, err := compliance.Parse(raw)
	if err != nil {
		return OperatorCompliancePolicyResult{}, fmt.Errorf("store: canonical compliance policy: %w", err)
	}
	now := s.now().UTC()
	result := OperatorCompliancePolicyResult{PolicyID: req.PolicyID, Revision: current,
		Digest: digest, Policy: normalized, PublishedAt: now}

	if currentDigest.Valid && currentDigest.String == digest {
		result.Unchanged = true
		var at string
		if err := tx.QueryRow(`SELECT published_at FROM compliance_policies
		 WHERE policy_id=? AND policy_revision=?`, req.PolicyID, current).Scan(&at); err == nil {
			result.PublishedAt = parseTime(at)
		}
	} else {
		result.Revision = current + 1
		if _, err := tx.Exec(`INSERT INTO compliance_policies
		 (policy_id,policy_revision,rules_json,rules_digest,published_at,published_by)
		 VALUES (?,?,?,?,?,?)`, req.PolicyID, result.Revision, string(raw), digest,
			fmtTime(now), req.PublishedBy); err != nil {
			return OperatorCompliancePolicyResult{}, fmt.Errorf("store: insert compliance policy: %w", err)
		}
	}

	if err := s.policyCommit(tx, req.IdempotencyKey, operation, req.RequestDigest, &audit, result); err != nil {
		return OperatorCompliancePolicyResult{}, err
	}
	result.Audited = true
	return result, nil
}

// ------------------------------------------------------------ 指派合規性原則

type OperatorComplianceAssignmentRequest struct {
	Scope          compliance.Scope
	ScopeID        string
	PolicyID       string
	PolicyRevision int64
	PreviewDigest  string
	ConfirmScopeID string
	Reason         string
	AssignedBy     string
	IdempotencyKey string
	RequestDigest  string
	Audit          AuditEntry
}

type OperatorComplianceAssignmentResult struct {
	AssignmentID string            `json:"assignment_id"`
	Scope        compliance.Scope  `json:"scope"`
	ScopeID      string            `json:"scope_id"`
	Revision     int64             `json:"assignment_revision"`
	PolicyID     string            `json:"policy_id"`
	PolicyRev    int64             `json:"policy_revision"`
	Digest       string            `json:"rules_digest"`
	Policy       compliance.Policy `json:"policy"`
	AssignedAt   time.Time         `json:"assigned_at"`
	Unchanged    bool              `json:"unchanged"`
	Replayed     bool              `json:"replayed"`
	Audited      bool              `json:"-"`
}

// ComplianceAssignmentPreviewDigest covers scope, target policy and revision.
func ComplianceAssignmentPreviewDigest(scope compliance.Scope, scopeID, policyID string, rev int64) string {
	return digestOf(fmt.Sprintf("compliance-assign:v1:%s:%s:%s:%d", scope, scopeID, policyID, rev))
}

// ApplyOperatorComplianceAssignment pins one published rule set to one scope.
func (s *Store) ApplyOperatorComplianceAssignment(req OperatorComplianceAssignmentRequest) (OperatorComplianceAssignmentResult, error) {
	if strings.TrimSpace(req.IdempotencyKey) == "" || len(req.IdempotencyKey) > 200 {
		return OperatorComplianceAssignmentResult{}, operatorError(
			OperatorCodeIdempotencyKeyRequired, "Idempotency-Key 不可省略且最多 200 bytes")
	}
	if req.RequestDigest == "" {
		return OperatorComplianceAssignmentResult{}, operatorError(
			OperatorCodeRequestDigestRequired, "request body digest 不可省略")
	}

	operation := fmt.Sprintf("compliance-assign:%s:%s", req.Scope, req.ScopeID)
	audit := req.Audit
	audit.Action = AuditComplianceAssign
	audit.IdempotencyKey = req.IdempotencyKey
	audit.RequestDigest = req.RequestDigest
	audit.Subject = string(req.Scope) + ":" + req.ScopeID
	audit.Reason = req.Reason
	if req.Scope == compliance.ScopeMachine {
		audit.MachineID = req.ScopeID
	}

	tx, err := s.beginWrite(context.Background(), "apply_operator_compliance_assignment")
	if err != nil {
		return OperatorComplianceAssignmentResult{}, fmt.Errorf("store: begin compliance assignment: %w", err)
	}
	defer tx.Rollback()

	replayed, rejected, err := s.policyIdempotency(tx, req.IdempotencyKey, operation, req.RequestDigest, &audit,
		func(raw string) (any, error) {
			var out OperatorComplianceAssignmentResult
			if err := json.Unmarshal([]byte(raw), &out); err != nil {
				return nil, err
			}
			out.Replayed, out.Audited = true, true
			return out, nil
		}, "沒有再次寫入指派")
	if err != nil {
		return OperatorComplianceAssignmentResult{}, err
	}
	if rejected != nil {
		return OperatorComplianceAssignmentResult{}, rejected
	}
	if replayed != nil {
		return replayed.(OperatorComplianceAssignmentResult), nil
	}

	reject := func(code, detail string) (OperatorComplianceAssignmentResult, error) {
		err := s.policyReject(tx, req.IdempotencyKey, operation, req.RequestDigest, &audit, code, detail)
		return OperatorComplianceAssignmentResult{}, err
	}

	if strings.TrimSpace(req.Reason) == "" {
		return reject(OperatorCodeReasonRequired, "reason 不可省略：這一份指派會改變這些機器的判決")
	}
	if req.ConfirmScopeID != req.ScopeID {
		return reject(OperatorCodeConfirmationMismatch,
			fmt.Sprintf("確認欄位打的是 %q，不是 %q", req.ConfirmScopeID, req.ScopeID))
	}
	switch req.Scope {
	case compliance.ScopeMachine:
		var retiredAt sql.NullString
		if err := tx.QueryRow(`SELECT retired_at FROM machine_registry WHERE machine_id=?`,
			req.ScopeID).Scan(&retiredAt); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return reject(OperatorCodeMachineNotFound, "找不到 machine "+req.ScopeID)
			}
			return OperatorComplianceAssignmentResult{}, fmt.Errorf("store: inspect compliance scope machine: %w", err)
		}
		if retiredAt.Valid {
			return reject(OperatorCodeMachineRetired,
				fmt.Sprintf("%s 已在 %s 退役；先放回分母，才能判它合不合規", req.ScopeID, retiredAt.String))
		}
	case compliance.ScopeChannel:
		if req.ScopeID != "canary" && req.ScopeID != "stable" {
			return reject(OperatorCodeBadChannel, "channel 只接受 canary 或 stable")
		}
	default:
		return reject(OperatorCodeComplianceScopeInvalid, "scope 只接受 machine 或 channel")
	}

	var raw, digest string
	if err := tx.QueryRow(`SELECT rules_json,rules_digest FROM compliance_policies
	 WHERE policy_id=? AND policy_revision=?`, req.PolicyID, req.PolicyRevision).Scan(&raw, &digest); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return reject(OperatorCodeCompliancePolicyNotFound,
				fmt.Sprintf("找不到 %s 的 revision %d；先發佈它，再指派", req.PolicyID, req.PolicyRevision))
		}
		return OperatorComplianceAssignmentResult{}, fmt.Errorf("store: inspect compliance policy revision: %w", err)
	}
	policy, err := compliance.Parse([]byte(raw))
	if err != nil {
		return OperatorComplianceAssignmentResult{}, fmt.Errorf("store: stored compliance policy is unreadable: %w", err)
	}
	if want := ComplianceAssignmentPreviewDigest(req.Scope, req.ScopeID, req.PolicyID, req.PolicyRevision); req.PreviewDigest != want {
		return reject(OperatorCodeCompliancePreviewStale,
			"預覽已過期：指派的目標在你按下確認之前變了；請重新預覽")
	}

	var currentRev int64
	var currentDigest, currentAssignment sql.NullString
	if err := tx.QueryRow(`SELECT assignment_revision,rules_digest,assignment_id FROM compliance_assignments
	 WHERE scope_type=? AND scope_id=? ORDER BY assignment_revision DESC LIMIT 1`,
		req.Scope, req.ScopeID).Scan(&currentRev, &currentDigest, &currentAssignment); err != nil &&
		!errors.Is(err, sql.ErrNoRows) {
		return OperatorComplianceAssignmentResult{}, fmt.Errorf("store: inspect compliance assignment: %w", err)
	}

	now := s.now().UTC()
	result := OperatorComplianceAssignmentResult{Scope: req.Scope, ScopeID: req.ScopeID,
		Revision: currentRev, PolicyID: req.PolicyID, PolicyRev: req.PolicyRevision,
		Digest: digest, Policy: policy, AssignedAt: now}

	// Assigning the rules already in force changes no verdict, so it must not
	// mint a revision that makes the board look like something just moved.
	if currentDigest.Valid && currentDigest.String == digest {
		result.Unchanged = true
		result.AssignmentID = currentAssignment.String
		var at string
		if err := tx.QueryRow(`SELECT assigned_at FROM compliance_assignments WHERE assignment_id=?`,
			currentAssignment.String).Scan(&at); err == nil {
			result.AssignedAt = parseTime(at)
		}
	} else {
		result.Revision = currentRev + 1
		result.AssignmentID = newID()
		if _, err := tx.Exec(`INSERT INTO compliance_assignments
		 (assignment_id,scope_type,scope_id,assignment_revision,policy_id,policy_revision,
		  rules_digest,assigned_at,assigned_by,supersedes_assignment_id)
		 VALUES (?,?,?,?,?,?,?,?,?,?)`, result.AssignmentID, req.Scope, req.ScopeID, result.Revision,
			req.PolicyID, req.PolicyRevision, digest, fmtTime(now), req.AssignedBy,
			nullStr(currentAssignment.String)); err != nil {
			return OperatorComplianceAssignmentResult{}, fmt.Errorf("store: insert compliance assignment: %w", err)
		}
	}

	if err := s.policyCommit(tx, req.IdempotencyKey, operation, req.RequestDigest, &audit, result); err != nil {
		return OperatorComplianceAssignmentResult{}, err
	}
	result.Audited = true
	return result, nil
}
