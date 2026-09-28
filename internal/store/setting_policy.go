package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/teddashh/AI-Intune/internal/settingpolicy"
)

var (
	ErrSettingPolicyInvalid  = errors.New("store: setting policy is invalid")
	ErrSettingPolicyConflict = errors.New("store: setting policy revision conflict")
	ErrSettingPolicyNotFound = errors.New("store: setting policy not found")
	ErrSettingScopeInvalid   = errors.New("store: setting assignment scope is invalid")
)

// policyIDLimit keeps an id short enough to read in a table cell and in an
// audit row. The value is an operator's own name for a policy.
const policyIDLimit = 64

// SettingPolicyRecord is one published, immutable revision.
type SettingPolicyRecord struct {
	PolicyID    string                 `json:"policy_id"`
	Revision    int64                  `json:"policy_revision"`
	Settings    settingpolicy.Settings `json:"settings"`
	Digest      string                 `json:"settings_digest"`
	PublishedAt time.Time              `json:"published_at"`
	PublishedBy string                 `json:"-"`
}

// SettingAssignmentRecord is one published revision pinned to one scope.
type SettingAssignmentRecord struct {
	AssignmentID string                 `json:"assignment_id"`
	Scope        settingpolicy.Scope    `json:"scope"`
	ScopeID      string                 `json:"scope_id"`
	Revision     int64                  `json:"assignment_revision"`
	PolicyID     string                 `json:"policy_id"`
	PolicyRev    int64                  `json:"policy_revision"`
	Digest       string                 `json:"settings_digest"`
	Settings     settingpolicy.Settings `json:"settings"`
	AssignedAt   time.Time              `json:"assigned_at"`
	AssignedBy   string                 `json:"-"`
}

// ---------------------------------------------------------------- 發佈設定原則

type OperatorSettingPolicyRequest struct {
	PolicyID         string
	Settings         settingpolicy.Settings
	ExpectedRevision *int64
	PreviewDigest    string
	ConfirmPolicyID  string
	Reason           string
	PublishedBy      string
	IdempotencyKey   string
	RequestDigest    string
	Audit            AuditEntry
}

type OperatorSettingPolicyResult struct {
	PolicyID    string                 `json:"policy_id"`
	Revision    int64                  `json:"policy_revision"`
	Digest      string                 `json:"settings_digest"`
	Settings    settingpolicy.Settings `json:"settings"`
	PublishedAt time.Time              `json:"published_at"`
	Unchanged   bool                   `json:"unchanged"`
	Replayed    bool                   `json:"replayed"`
	Audited     bool                   `json:"-"`
}

// SettingPolicyPreviewDigest is what an operator confirms before publishing.
// It covers the id, the expected revision and the settings, so a preview taken
// against one revision cannot be confirmed after somebody else published.
func SettingPolicyPreviewDigest(policyID string, expected int64, s settingpolicy.Settings) (string, error) {
	raw, err := s.Canonical()
	if err != nil {
		return "", err
	}
	return digestOf(fmt.Sprintf("setting-policy:v1:%s:%d:%s", policyID, expected, raw)), nil
}

// ApplyOperatorSettingPolicy publishes one immutable revision.
//
// 重新發佈同一組值不會產生新的 revision：那不是一個新的決定，而機器正在跑的
// digest 也不會因此改變。撞到別人剛發佈的 revision 則明確衝突，不靜默覆蓋。
func (s *Store) ApplyOperatorSettingPolicy(req OperatorSettingPolicyRequest) (OperatorSettingPolicyResult, error) {
	if strings.TrimSpace(req.IdempotencyKey) == "" || len(req.IdempotencyKey) > 200 {
		return OperatorSettingPolicyResult{}, operatorError(
			OperatorCodeIdempotencyKeyRequired, "Idempotency-Key 不可省略且最多 200 bytes")
	}
	if req.RequestDigest == "" {
		return OperatorSettingPolicyResult{}, operatorError(
			OperatorCodeRequestDigestRequired, "request body digest 不可省略")
	}

	operation := "setting-policy:" + req.PolicyID
	audit := req.Audit
	audit.Action = AuditSettingPolicy
	audit.IdempotencyKey = req.IdempotencyKey
	audit.RequestDigest = req.RequestDigest
	audit.Subject = req.PolicyID
	audit.Reason = req.Reason

	tx, err := s.db.Begin()
	if err != nil {
		return OperatorSettingPolicyResult{}, fmt.Errorf("store: begin setting policy: %w", err)
	}
	defer tx.Rollback()

	replayed, rejected, err := s.policyIdempotency(tx, req.IdempotencyKey, operation, req.RequestDigest, &audit,
		func(raw string) (any, error) {
			var out OperatorSettingPolicyResult
			if err := json.Unmarshal([]byte(raw), &out); err != nil {
				return nil, err
			}
			out.Replayed, out.Audited = true, true
			return out, nil
		}, "沒有再次發佈新的 revision")
	if err != nil {
		return OperatorSettingPolicyResult{}, err
	}
	if rejected != nil {
		return OperatorSettingPolicyResult{}, rejected
	}
	if replayed != nil {
		return replayed.(OperatorSettingPolicyResult), nil
	}

	reject := func(code, detail string) (OperatorSettingPolicyResult, error) {
		err := s.policyReject(tx, req.IdempotencyKey, operation, req.RequestDigest, &audit, code, detail)
		return OperatorSettingPolicyResult{}, err
	}

	if err := validPolicyID(req.PolicyID); err != nil {
		return reject(OperatorCodeSettingPolicyInvalid, err.Error())
	}
	if strings.TrimSpace(req.Reason) == "" {
		return reject(OperatorCodeReasonRequired, "reason 不可省略：這一份設定會改變機器的節奏")
	}
	if req.ConfirmPolicyID != req.PolicyID {
		return reject(OperatorCodeConfirmationMismatch,
			fmt.Sprintf("確認欄位打的是 %q，不是 %q", req.ConfirmPolicyID, req.PolicyID))
	}
	if err := req.Settings.Validate(); err != nil {
		return reject(OperatorCodeSettingPolicyInvalid, strings.TrimPrefix(err.Error(), "settingpolicy: invalid settings: "))
	}
	if req.ExpectedRevision == nil {
		return reject(OperatorCodePreconditionRequired, "expected_revision 不可省略")
	}

	var current int64
	var currentDigest sql.NullString
	if err := tx.QueryRow(`SELECT policy_revision,settings_digest FROM setting_policies
	 WHERE policy_id=? ORDER BY policy_revision DESC LIMIT 1`, req.PolicyID).Scan(&current, &currentDigest); err != nil &&
		!errors.Is(err, sql.ErrNoRows) {
		return OperatorSettingPolicyResult{}, fmt.Errorf("store: inspect setting policy: %w", err)
	}
	if *req.ExpectedRevision != current {
		return reject(OperatorCodeSettingPolicyConflict, fmt.Sprintf(
			"%s 目前的 revision 是 %d，不是 request 預期的 %d；請重新讀取後再發佈",
			req.PolicyID, current, *req.ExpectedRevision))
	}

	want, err := SettingPolicyPreviewDigest(req.PolicyID, current, req.Settings)
	if err != nil {
		return OperatorSettingPolicyResult{}, fmt.Errorf("store: setting policy preview digest: %w", err)
	}
	if req.PreviewDigest != want {
		return reject(OperatorCodeSettingPreviewStale,
			"預覽已過期：這一份設定或它的 revision 在你按下確認之前變了；請重新預覽")
	}

	digest, err := settingpolicy.Digest(req.Settings)
	if err != nil {
		return OperatorSettingPolicyResult{}, fmt.Errorf("store: setting digest: %w", err)
	}
	now := s.now().UTC()
	result := OperatorSettingPolicyResult{PolicyID: req.PolicyID, Revision: current,
		Digest: digest, Settings: req.Settings, PublishedAt: now}

	// Republishing identical values is not a new decision, so it does not mint a
	// revision that every assigned machine would then have to re-report.
	if currentDigest.Valid && currentDigest.String == digest {
		result.Unchanged = true
		var at string
		if err := tx.QueryRow(`SELECT published_at FROM setting_policies
		 WHERE policy_id=? AND policy_revision=?`, req.PolicyID, current).Scan(&at); err == nil {
			result.PublishedAt = parseTime(at)
		}
	} else {
		raw, err := req.Settings.Canonical()
		if err != nil {
			return OperatorSettingPolicyResult{}, fmt.Errorf("store: canonical settings: %w", err)
		}
		result.Revision = current + 1
		if _, err := tx.Exec(`INSERT INTO setting_policies
		 (policy_id,policy_revision,settings_json,settings_digest,published_at,published_by)
		 VALUES (?,?,?,?,?,?)`, req.PolicyID, result.Revision, string(raw), digest,
			fmtTime(now), req.PublishedBy); err != nil {
			return OperatorSettingPolicyResult{}, fmt.Errorf("store: insert setting policy: %w", err)
		}
	}

	if err := s.policyCommit(tx, req.IdempotencyKey, operation, req.RequestDigest, &audit, result); err != nil {
		return OperatorSettingPolicyResult{}, err
	}
	result.Audited = true
	return result, nil
}

// ---------------------------------------------------------------- 指派設定原則

type OperatorSettingAssignmentRequest struct {
	Scope          settingpolicy.Scope
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

type OperatorSettingAssignmentResult struct {
	AssignmentID string                 `json:"assignment_id"`
	Scope        settingpolicy.Scope    `json:"scope"`
	ScopeID      string                 `json:"scope_id"`
	Revision     int64                  `json:"assignment_revision"`
	PolicyID     string                 `json:"policy_id"`
	PolicyRev    int64                  `json:"policy_revision"`
	Digest       string                 `json:"settings_digest"`
	Settings     settingpolicy.Settings `json:"settings"`
	AssignedAt   time.Time              `json:"assigned_at"`
	Unchanged    bool                   `json:"unchanged"`
	Replayed     bool                   `json:"replayed"`
	Audited      bool                   `json:"-"`
}

// SettingAssignmentPreviewDigest covers scope, target policy and revision.
func SettingAssignmentPreviewDigest(scope settingpolicy.Scope, scopeID, policyID string, rev int64) string {
	return digestOf(fmt.Sprintf("setting-assign:v1:%s:%s:%s:%d", scope, scopeID, policyID, rev))
}

// ApplyOperatorSettingAssignment pins one published revision to one scope.
func (s *Store) ApplyOperatorSettingAssignment(req OperatorSettingAssignmentRequest) (OperatorSettingAssignmentResult, error) {
	if strings.TrimSpace(req.IdempotencyKey) == "" || len(req.IdempotencyKey) > 200 {
		return OperatorSettingAssignmentResult{}, operatorError(
			OperatorCodeIdempotencyKeyRequired, "Idempotency-Key 不可省略且最多 200 bytes")
	}
	if req.RequestDigest == "" {
		return OperatorSettingAssignmentResult{}, operatorError(
			OperatorCodeRequestDigestRequired, "request body digest 不可省略")
	}

	operation := fmt.Sprintf("setting-assign:%s:%s", req.Scope, req.ScopeID)
	audit := req.Audit
	audit.Action = AuditSettingAssign
	audit.IdempotencyKey = req.IdempotencyKey
	audit.RequestDigest = req.RequestDigest
	audit.Subject = string(req.Scope) + ":" + req.ScopeID
	audit.Reason = req.Reason
	if req.Scope == settingpolicy.ScopeMachine {
		audit.MachineID = req.ScopeID
	}

	tx, err := s.db.Begin()
	if err != nil {
		return OperatorSettingAssignmentResult{}, fmt.Errorf("store: begin setting assignment: %w", err)
	}
	defer tx.Rollback()

	replayed, rejected, err := s.policyIdempotency(tx, req.IdempotencyKey, operation, req.RequestDigest, &audit,
		func(raw string) (any, error) {
			var out OperatorSettingAssignmentResult
			if err := json.Unmarshal([]byte(raw), &out); err != nil {
				return nil, err
			}
			out.Replayed, out.Audited = true, true
			return out, nil
		}, "沒有再次寫入指派")
	if err != nil {
		return OperatorSettingAssignmentResult{}, err
	}
	if rejected != nil {
		return OperatorSettingAssignmentResult{}, rejected
	}
	if replayed != nil {
		return replayed.(OperatorSettingAssignmentResult), nil
	}

	reject := func(code, detail string) (OperatorSettingAssignmentResult, error) {
		err := s.policyReject(tx, req.IdempotencyKey, operation, req.RequestDigest, &audit, code, detail)
		return OperatorSettingAssignmentResult{}, err
	}

	if strings.TrimSpace(req.Reason) == "" {
		return reject(OperatorCodeReasonRequired, "reason 不可省略：這一份指派會改變機器的節奏")
	}
	if req.ConfirmScopeID != req.ScopeID {
		return reject(OperatorCodeConfirmationMismatch,
			fmt.Sprintf("確認欄位打的是 %q，不是 %q", req.ConfirmScopeID, req.ScopeID))
	}
	switch req.Scope {
	case settingpolicy.ScopeMachine:
		var retiredAt sql.NullString
		if err := tx.QueryRow(`SELECT retired_at FROM machine_registry WHERE machine_id=?`,
			req.ScopeID).Scan(&retiredAt); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return reject(OperatorCodeMachineNotFound, "找不到 machine "+req.ScopeID)
			}
			return OperatorSettingAssignmentResult{}, fmt.Errorf("store: inspect setting scope machine: %w", err)
		}
		if retiredAt.Valid {
			return reject(OperatorCodeMachineRetired,
				fmt.Sprintf("%s 已在 %s 退役；先放回分母，才能指派設定", req.ScopeID, retiredAt.String))
		}
	case settingpolicy.ScopeChannel:
		if req.ScopeID != "canary" && req.ScopeID != "stable" {
			return reject(OperatorCodeBadChannel, "channel 只接受 canary 或 stable")
		}
	default:
		return reject(OperatorCodeSettingScopeInvalid, "scope 只接受 machine 或 channel")
	}

	var raw, digest string
	if err := tx.QueryRow(`SELECT settings_json,settings_digest FROM setting_policies
	 WHERE policy_id=? AND policy_revision=?`, req.PolicyID, req.PolicyRevision).Scan(&raw, &digest); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return reject(OperatorCodeSettingPolicyNotFound,
				fmt.Sprintf("找不到 %s 的 revision %d；先發佈它，再指派", req.PolicyID, req.PolicyRevision))
		}
		return OperatorSettingAssignmentResult{}, fmt.Errorf("store: inspect setting policy revision: %w", err)
	}
	settings, err := settingpolicy.Parse([]byte(raw))
	if err != nil {
		return OperatorSettingAssignmentResult{}, fmt.Errorf("store: stored setting policy is unreadable: %w", err)
	}
	if want := SettingAssignmentPreviewDigest(req.Scope, req.ScopeID, req.PolicyID, req.PolicyRevision); req.PreviewDigest != want {
		return reject(OperatorCodeSettingPreviewStale,
			"預覽已過期：指派的目標在你按下確認之前變了；請重新預覽")
	}

	var currentRev int64
	var currentDigest, currentAssignment sql.NullString
	if err := tx.QueryRow(`SELECT assignment_revision,settings_digest,assignment_id FROM setting_assignments
	 WHERE scope_type=? AND scope_id=? ORDER BY assignment_revision DESC LIMIT 1`,
		req.Scope, req.ScopeID).Scan(&currentRev, &currentDigest, &currentAssignment); err != nil &&
		!errors.Is(err, sql.ErrNoRows) {
		return OperatorSettingAssignmentResult{}, fmt.Errorf("store: inspect setting assignment: %w", err)
	}

	now := s.now().UTC()
	result := OperatorSettingAssignmentResult{Scope: req.Scope, ScopeID: req.ScopeID,
		Revision: currentRev, PolicyID: req.PolicyID, PolicyRev: req.PolicyRevision,
		Digest: digest, Settings: settings, AssignedAt: now}

	// Assigning the digest already in force changes nothing on the wire, so it
	// must not mint a revision that makes every machine look pending again.
	if currentDigest.Valid && currentDigest.String == digest {
		result.Unchanged = true
		result.AssignmentID = currentAssignment.String
		var at string
		if err := tx.QueryRow(`SELECT assigned_at FROM setting_assignments WHERE assignment_id=?`,
			currentAssignment.String).Scan(&at); err == nil {
			result.AssignedAt = parseTime(at)
		}
	} else {
		result.Revision = currentRev + 1
		result.AssignmentID = newID()
		if _, err := tx.Exec(`INSERT INTO setting_assignments
		 (assignment_id,scope_type,scope_id,assignment_revision,policy_id,policy_revision,
		  settings_digest,assigned_at,assigned_by,supersedes_assignment_id)
		 VALUES (?,?,?,?,?,?,?,?,?,?)`, result.AssignmentID, req.Scope, req.ScopeID, result.Revision,
			req.PolicyID, req.PolicyRevision, digest, fmtTime(now), req.AssignedBy,
			nullStr(currentAssignment.String)); err != nil {
			return OperatorSettingAssignmentResult{}, fmt.Errorf("store: insert setting assignment: %w", err)
		}
	}

	if err := s.policyCommit(tx, req.IdempotencyKey, operation, req.RequestDigest, &audit, result); err != nil {
		return OperatorSettingAssignmentResult{}, err
	}
	result.Audited = true
	return result, nil
}

func validPolicyID(id string) error {
	if id == "" || len(id) > policyIDLimit {
		return fmt.Errorf("policy_id 不可空白，最多 %d 個字元", policyIDLimit)
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
		default:
			return errors.New("policy_id 只接受小寫英文、數字與連字號")
		}
	}
	if strings.HasPrefix(id, "-") || strings.HasSuffix(id, "-") {
		return errors.New("policy_id 不可以連字號開頭或結尾")
	}
	return nil
}
