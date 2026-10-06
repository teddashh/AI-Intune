package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/teddashh/AI-Intune/internal/deploy"
)

type MachineLifecycleState string

type MachineLifecycleBlocker string

const (
	MachineLifecycleActive                 MachineLifecycleState   = "active"
	MachineLifecycleRetired                MachineLifecycleState   = "retired"
	MachineLifecycleBlockerNonterminalJobs MachineLifecycleBlocker = "nonterminal_jobs"

	operatorMachineLifecycleVersion         = "v2"
	operatorMachineLifecycleLegacyVersion   = "v1"
	operatorMachineLifecycleOperationPrefix = "machine-lifecycle:v1:"
	operatorMachineLifecycleCacheInvalid    = "operator machine lifecycle idempotency cache invalid；未回放結果"
	operatorMachineLifecycleRejectPrefix    = "operator machine lifecycle rejection code="
)

// OperatorMachineLifecycleReadResult is a safe current projection. It says
// what retirement gates without exposing either credential hash.
type OperatorMachineLifecycleReadResult struct {
	MachineID                          string                `json:"machine_id"`
	DisplayName                        string                `json:"display_name"`
	State                              MachineLifecycleState `json:"state"`
	LifecycleRevision                  int64                 `json:"lifecycle_revision"`
	InDenominator                      bool                  `json:"in_denominator"`
	RetiredAt                          *time.Time            `json:"retired_at,omitempty"`
	RegistryRetained                   bool                  `json:"registry_retained"`
	HistoryPreserved                   bool                  `json:"history_preserved"`
	Channel                            string                `json:"channel,omitempty"`
	ChannelRevision                    int64                 `json:"channel_revision"`
	AgentCredentialPresent             bool                  `json:"agent_credential_present"`
	AgentAuthenticationAllowed         bool                  `json:"agent_authentication_allowed"`
	PendingEnrollmentTokenCount        int64                 `json:"pending_enrollment_token_count"`
	PendingEnrollmentTokenExpiredCount int64                 `json:"pending_enrollment_token_expired_count"`
	PendingEnrollmentRedemptionAllowed bool                  `json:"pending_enrollment_redemption_allowed"`
	ActiveJobCount                     int64                 `json:"active_job_count"`
}

// OperatorMachineLifecycleImpact is flattened into preview/apply responses so
// every adapter renders the same consequences and preservation guarantees.
type OperatorMachineLifecycleImpact struct {
	InDenominatorBefore                bool                      `json:"in_denominator_before"`
	InDenominatorAfter                 bool                      `json:"in_denominator_after"`
	DenominatorDelta                   int64                     `json:"denominator_delta"`
	RegistryRetained                   bool                      `json:"registry_retained"`
	HistoryPreserved                   bool                      `json:"history_preserved"`
	Channel                            string                    `json:"channel,omitempty"`
	ChannelRevision                    int64                     `json:"channel_revision"`
	ChannelPreserved                   bool                      `json:"channel_preserved"`
	AgentCredentialPresent             bool                      `json:"agent_credential_present"`
	AgentAuthenticationBefore          bool                      `json:"agent_authentication_before"`
	AgentAuthenticationAfter           bool                      `json:"agent_authentication_after"`
	PendingEnrollmentTokenCount        int64                     `json:"pending_enrollment_token_count"`
	PendingEnrollmentTokenExpiredCount int64                     `json:"pending_enrollment_token_expired_count"`
	PendingEnrollmentRedemptionBefore  bool                      `json:"pending_enrollment_redemption_before"`
	PendingEnrollmentRedemptionAfter   bool                      `json:"pending_enrollment_redemption_after"`
	ActiveJobCount                     int64                     `json:"active_job_count"`
	Blockers                           []MachineLifecycleBlocker `json:"blockers"`
}

type OperatorMachineLifecyclePreviewRequest struct {
	MachineID        string
	DesiredState     MachineLifecycleState
	ExpectedRevision *int64
}

type OperatorMachineLifecyclePreviewResult struct {
	MachineID             string                `json:"machine_id"`
	DisplayName           string                `json:"display_name"`
	CurrentState          MachineLifecycleState `json:"current_state"`
	DesiredState          MachineLifecycleState `json:"desired_state"`
	LifecycleRevision     int64                 `json:"lifecycle_revision"`
	PreviewedAt           time.Time             `json:"previewed_at"`
	OpenAgentSessionCount int64                 `json:"open_agent_session_count"`
	OperatorMachineLifecycleImpact
	PreviewDigest string `json:"preview_digest"`
}

type OperatorMachineLifecycleRequest struct {
	MachineID          string
	DesiredState       MachineLifecycleState
	ExpectedRevision   *int64
	ConfirmDisplayName string
	PreviewDigest      string
	Reason             string
	IdempotencyKey     string
	RequestDigest      string
	Audit              AuditEntry
}

type OperatorMachineLifecycleResult struct {
	MachineID         string                `json:"machine_id"`
	DisplayName       string                `json:"display_name"`
	PreviousState     MachineLifecycleState `json:"previous_state"`
	State             MachineLifecycleState `json:"state"`
	LifecycleRevision int64                 `json:"lifecycle_revision"`
	Changed           bool                  `json:"changed"`
	NoOp              bool                  `json:"no_op"`
	TransitionEventID *int64                `json:"transition_event_id,omitempty"`
	RetiredAt         *time.Time            `json:"retired_at,omitempty"`
	AppliedAt         time.Time             `json:"applied_at"`
	OperatorMachineLifecycleImpact
	PreviewDigest    string   `json:"preview_digest"`
	Replayed         bool     `json:"replayed"`
	Audited          bool     `json:"-"`
	ClosedSessionIDs []string `json:"-"`
}

type operatorMachineLifecycleSnapshot struct {
	MachineID             string
	DisplayName           string
	CreatedAt             time.Time
	State                 MachineLifecycleState
	Revision              int64
	RetiredAt             *time.Time
	Channel               string
	ChannelRevision       int64
	AgentCredential       bool
	agentCredentialHash   string
	PendingCount          int64
	PendingExpiredCount   int64
	pendingIdentityDigest string
	ActiveJobCount        int64
	OpenAgentSessionCount int64
}

type operatorMachineLifecycleReceipt struct {
	SchemaVersion     string                `json:"schema_version"`
	MachineID         string                `json:"machine_id"`
	DisplayName       string                `json:"display_name"`
	PreviousState     MachineLifecycleState `json:"previous_state"`
	State             MachineLifecycleState `json:"state"`
	LifecycleRevision int64                 `json:"lifecycle_revision"`
	Changed           bool                  `json:"changed"`
	NoOp              bool                  `json:"no_op"`
	TransitionEventID *int64                `json:"transition_event_id,omitempty"`
	RetiredAt         *time.Time            `json:"retired_at,omitempty"`
	AppliedAt         time.Time             `json:"applied_at"`
	// LegacyExpected is decoded only so v1 receipts remain replayable after the
	// public lifecycle DTO stopped exposing the dead expected axis. New v2
	// receipts omit it.
	LegacyExpected *bool `json:"expected,omitempty"`
	OperatorMachineLifecycleImpact
	PreviewDigest string `json:"preview_digest"`
}

func (s *Store) OperatorMachineLifecycle(machineID string) (OperatorMachineLifecycleReadResult, error) {
	now, err := canonicalMachineLifecycleTime(s.now())
	if err != nil {
		return OperatorMachineLifecycleReadResult{}, fmt.Errorf("store: read machine lifecycle clock: %w", err)
	}
	snapshot, err := loadOperatorMachineLifecycleSnapshot(s.rdb, machineID, now)
	if err != nil {
		return OperatorMachineLifecycleReadResult{}, err
	}
	active := snapshot.State == MachineLifecycleActive
	return OperatorMachineLifecycleReadResult{
		MachineID: snapshot.MachineID, DisplayName: snapshot.DisplayName,
		State: snapshot.State, LifecycleRevision: snapshot.Revision,
		InDenominator: active,
		RetiredAt:     snapshot.RetiredAt, RegistryRetained: true, HistoryPreserved: true,
		Channel: snapshot.Channel, ChannelRevision: snapshot.ChannelRevision,
		AgentCredentialPresent:             snapshot.AgentCredential,
		AgentAuthenticationAllowed:         snapshot.AgentCredential && active,
		PendingEnrollmentTokenCount:        snapshot.PendingCount,
		PendingEnrollmentTokenExpiredCount: snapshot.PendingExpiredCount,
		PendingEnrollmentRedemptionAllowed: snapshot.PendingCount > snapshot.PendingExpiredCount && active,
		ActiveJobCount:                     snapshot.ActiveJobCount,
	}, nil
}

func (s *Store) PreviewOperatorMachineLifecycle(req OperatorMachineLifecyclePreviewRequest) (OperatorMachineLifecyclePreviewResult, error) {
	if !validMachineLifecycleState(req.DesiredState) {
		return OperatorMachineLifecyclePreviewResult{}, operatorError(OperatorCodeBadLifecycle,
			"desired_state 只接受 active 或 retired")
	}
	if req.ExpectedRevision == nil {
		return OperatorMachineLifecyclePreviewResult{}, operatorError(OperatorCodePreconditionRequired,
			"expected_revision 不可省略")
	}
	now, err := canonicalMachineLifecycleTime(s.now())
	if err != nil {
		return OperatorMachineLifecyclePreviewResult{}, fmt.Errorf("store: preview machine lifecycle clock: %w", err)
	}
	snapshot, err := loadOperatorMachineLifecycleSnapshot(s.rdb, req.MachineID, now)
	if err != nil {
		return OperatorMachineLifecyclePreviewResult{}, err
	}
	if *req.ExpectedRevision != snapshot.Revision {
		return OperatorMachineLifecyclePreviewResult{}, operatorError(OperatorCodePreconditionFailed,
			"lifecycle revision 已改變；請重新讀取機器後再預覽")
	}
	if snapshot.State != req.DesiredState && snapshot.Revision >= MaxMachineLifecycleRevision-1 {
		return OperatorMachineLifecyclePreviewResult{}, operatorError(OperatorCodeLifecycleRevisionLimit,
			"lifecycle revision 已無法安全遞增；拒絕預覽變更")
	}
	return operatorMachineLifecyclePreview(snapshot, req.DesiredState, now), nil
}

func operatorMachineLifecyclePreview(snapshot operatorMachineLifecycleSnapshot, desired MachineLifecycleState, now time.Time) OperatorMachineLifecyclePreviewResult {
	impact := operatorMachineLifecycleImpact(snapshot, desired)
	result := OperatorMachineLifecyclePreviewResult{
		MachineID: snapshot.MachineID, DisplayName: snapshot.DisplayName,
		CurrentState: snapshot.State, DesiredState: desired,
		LifecycleRevision: snapshot.Revision, PreviewedAt: now,
		OpenAgentSessionCount:          snapshot.OpenAgentSessionCount,
		OperatorMachineLifecycleImpact: impact,
	}
	result.PreviewDigest = operatorMachineLifecyclePreviewDigest(snapshot, desired, impact)
	return result
}

func operatorMachineLifecycleImpact(snapshot operatorMachineLifecycleSnapshot, desired MachineLifecycleState) OperatorMachineLifecycleImpact {
	beforeActive := snapshot.State == MachineLifecycleActive
	afterActive := desired == MachineLifecycleActive
	impact := OperatorMachineLifecycleImpact{
		InDenominatorBefore: beforeActive, InDenominatorAfter: afterActive,
		DenominatorDelta: int64(boolToInt(afterActive) - boolToInt(beforeActive)),
		RegistryRetained: true, HistoryPreserved: true,
		Channel: snapshot.Channel, ChannelRevision: snapshot.ChannelRevision, ChannelPreserved: true,
		AgentCredentialPresent:             snapshot.AgentCredential,
		AgentAuthenticationBefore:          snapshot.AgentCredential && beforeActive,
		AgentAuthenticationAfter:           snapshot.AgentCredential && afterActive,
		PendingEnrollmentTokenCount:        snapshot.PendingCount,
		PendingEnrollmentTokenExpiredCount: snapshot.PendingExpiredCount,
		PendingEnrollmentRedemptionBefore:  snapshot.PendingCount > snapshot.PendingExpiredCount && beforeActive,
		PendingEnrollmentRedemptionAfter:   snapshot.PendingCount > snapshot.PendingExpiredCount && afterActive,
		ActiveJobCount:                     snapshot.ActiveJobCount,
		Blockers:                           []MachineLifecycleBlocker{},
	}
	if beforeActive && !afterActive && snapshot.ActiveJobCount > 0 {
		impact.Blockers = append(impact.Blockers, MachineLifecycleBlockerNonterminalJobs)
	}
	return impact
}

func loadOperatorMachineLifecycleSnapshot(q operatorRowQuerier, machineID string, now time.Time) (operatorMachineLifecycleSnapshot, error) {
	var snapshot operatorMachineLifecycleSnapshot
	var channel, retiredRaw, agentHash sql.NullString
	var createdRaw string
	var pendingIdentity string
	err := q.QueryRow(`SELECT m.display_name,m.created_at,COALESCE(m.channel,''),m.channel_revision,
	 m.lifecycle_revision,m.retired_at,COALESCE(m.agent_token_hash,''),
	 (SELECT COUNT(*) FROM enrollment_tokens e WHERE e.used_by=m.machine_id AND e.used_at IS NULL),
	 (SELECT COUNT(*) FROM enrollment_tokens e
	   WHERE e.used_by=m.machine_id AND e.used_at IS NULL
	     AND `+enrollmentTicketExpiredPredicate+`),
	 COALESCE((SELECT group_concat(e.token_hash || ':' || e.created_at || ':' || e.expires_at, '|' ORDER BY e.token_hash)
	   FROM enrollment_tokens e WHERE e.used_by=m.machine_id AND e.used_at IS NULL),''),
	 (SELECT COUNT(*) FROM jobs j WHERE j.machine_id=m.machine_id AND j.state NOT IN (?,?,?,?,?)),
	 (SELECT COUNT(*) FROM agent_sessions a WHERE a.machine_id=m.machine_id AND a.closed_at IS NULL)
	 FROM machine_registry m WHERE m.machine_id=?`, fmtTime(now),
		deploy.Succeeded, deploy.Failed, deploy.Rejected, deploy.LeaseExpired, deploy.ManualIntervention,
		machineID).Scan(&snapshot.DisplayName, &createdRaw, &channel, &snapshot.ChannelRevision,
		&snapshot.Revision, &retiredRaw, &agentHash, &snapshot.PendingCount,
		&snapshot.PendingExpiredCount, &pendingIdentity, &snapshot.ActiveJobCount,
		&snapshot.OpenAgentSessionCount)
	if errors.Is(err, sql.ErrNoRows) {
		return snapshot, operatorError(OperatorCodeMachineNotFound, "找不到這台機器")
	}
	if err != nil {
		return snapshot, fmt.Errorf("store: inspect operator machine lifecycle: %w", err)
	}
	if snapshot.DisplayName == "" || snapshot.Revision < 0 || snapshot.Revision >= MaxMachineLifecycleRevision ||
		snapshot.ChannelRevision < 0 || snapshot.PendingCount < 0 || snapshot.PendingExpiredCount < 0 ||
		snapshot.PendingExpiredCount > snapshot.PendingCount || snapshot.ActiveJobCount < 0 ||
		snapshot.OpenAgentSessionCount < 0 {
		if snapshot.Revision < 0 || snapshot.Revision >= MaxMachineLifecycleRevision {
			return snapshot, operatorError(OperatorCodeLifecycleRevisionLimit,
				"lifecycle revision 已耗盡或損毀；拒絕讀取或變更")
		}
		return snapshot, errors.New("store: machine lifecycle projection is invalid")
	}
	snapshot.MachineID = machineID
	createdAt, ok := parseChangeReadHubTime(createdRaw)
	if !ok || now.Before(createdAt) {
		return snapshot, errors.New("store: machine lifecycle created_at or trusted clock is invalid")
	}
	snapshot.CreatedAt = createdAt
	snapshot.Channel = channel.String
	snapshot.AgentCredential = agentHash.Valid && agentHash.String != ""
	snapshot.agentCredentialHash = agentHash.String
	snapshot.pendingIdentityDigest = sha256Hex(pendingIdentity)
	if retiredRaw.Valid {
		retiredAt, ok := parseChangeReadHubTime(retiredRaw.String)
		if !ok {
			return snapshot, errors.New("store: machine retired_at is invalid")
		}
		snapshot.RetiredAt = &retiredAt
		snapshot.State = MachineLifecycleRetired
	} else {
		snapshot.State = MachineLifecycleActive
	}
	return snapshot, nil
}

func operatorMachineLifecyclePreviewDigest(snapshot operatorMachineLifecycleSnapshot, desired MachineLifecycleState,
	impact OperatorMachineLifecycleImpact,
) string {
	body := struct {
		Version               string                         `json:"version"`
		MachineID             string                         `json:"machine_id"`
		DisplayName           string                         `json:"display_name"`
		CurrentState          MachineLifecycleState          `json:"current_state"`
		DesiredState          MachineLifecycleState          `json:"desired_state"`
		LifecycleRevision     int64                          `json:"lifecycle_revision"`
		RetiredAt             string                         `json:"retired_at"`
		AgentCredentialHash   string                         `json:"agent_credential_hash"`
		PendingIdentityDigest string                         `json:"pending_identity_digest"`
		Impact                OperatorMachineLifecycleImpact `json:"impact"`
	}{
		Version: operatorMachineLifecycleVersion, MachineID: snapshot.MachineID,
		DisplayName: snapshot.DisplayName, CurrentState: snapshot.State, DesiredState: desired,
		LifecycleRevision: snapshot.Revision, AgentCredentialHash: snapshot.agentCredentialHash,
		PendingIdentityDigest: snapshot.pendingIdentityDigest, Impact: impact,
	}
	if snapshot.RetiredAt != nil {
		body.RetiredAt = fmtTime(*snapshot.RetiredAt)
	}
	raw, _ := json.Marshal(body)
	return "sha256:" + sha256Hex(string(raw))
}

func sha256Hex(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func validMachineLifecycleState(state MachineLifecycleState) bool {
	return state == MachineLifecycleActive || state == MachineLifecycleRetired
}

func operatorMachineLifecycleOperation(machineID string) string {
	return operatorMachineLifecycleOperationPrefix + machineID
}

// ApplyOperatorMachineLifecycle is the sole canonical operator writer. The
// projection, append-only transition, immutable receipt, and original audit
// evidence either all commit or none do.
func (s *Store) ApplyOperatorMachineLifecycle(req OperatorMachineLifecycleRequest) (OperatorMachineLifecycleResult, error) {
	if strings.TrimSpace(req.IdempotencyKey) == "" {
		return OperatorMachineLifecycleResult{}, operatorError(OperatorCodeIdempotencyKeyRequired, "Idempotency-Key 不可省略")
	}
	if len(req.IdempotencyKey) > 200 {
		return OperatorMachineLifecycleResult{}, operatorError(OperatorCodeIdempotencyKeyRequired, "Idempotency-Key 最多 200 bytes")
	}
	if req.RequestDigest == "" {
		return OperatorMachineLifecycleResult{}, operatorError(OperatorCodeRequestDigestRequired, "request body digest 不可省略")
	}

	audit := req.Audit
	audit.Action = AuditMachineLifecycle
	audit.MachineID = req.MachineID
	audit.Subject = req.MachineID
	audit.Reason = req.Reason
	audit.IdempotencyKey = req.IdempotencyKey
	audit.RequestDigest = req.RequestDigest
	tx, err := s.beginWrite(context.Background(), "apply_operator_machine_lifecycle")
	if err != nil {
		return OperatorMachineLifecycleResult{}, fmt.Errorf("store: begin operator machine lifecycle: %w", err)
	}
	defer tx.Rollback()
	writerNow, err := canonicalMachineLifecycleTime(s.now())
	if err != nil {
		return OperatorMachineLifecycleResult{}, fmt.Errorf("store: operator machine lifecycle clock: %w", err)
	}
	audit.At = writerNow

	var cached operatorCachedRequest
	err = tx.QueryRow(`SELECT operation,request_digest,outcome,response_json,error_code,error_detail,created_at
	 FROM operator_idempotency WHERE idempotency_key=?`, req.IdempotencyKey).Scan(
		&cached.Operation, &cached.Digest, &cached.Outcome, &cached.ResponseJSON,
		&cached.ErrorCode, &cached.ErrorDetail, &cached.CreatedAt)
	switch {
	case err == nil:
		return s.replayOperatorMachineLifecycle(tx, req, audit, cached)
	case !errors.Is(err, sql.ErrNoRows):
		return OperatorMachineLifecycleResult{}, fmt.Errorf("store: inspect machine lifecycle idempotency key: %w", err)
	}

	reject := func(code string) (OperatorMachineLifecycleResult, error) {
		detail, ok := canonicalOperatorMachineLifecycleRejectionDetail(code)
		if !ok {
			return OperatorMachineLifecycleResult{}, errors.New("store: invalid machine lifecycle rejection code")
		}
		audit.OK = false
		audit.Detail = operatorMachineLifecycleRejectionAuditDetail(code, detail)
		if err := s.recordAuditTx(tx, audit); err != nil {
			return OperatorMachineLifecycleResult{}, fmt.Errorf("store: record machine lifecycle rejection audit: %w", err)
		}
		if _, err := tx.Exec(`INSERT INTO operator_idempotency
		 (idempotency_key,operation,request_digest,outcome,error_code,error_detail,created_at)
		 VALUES (?,?,?,'rejected',?,?,?)`, req.IdempotencyKey,
			operatorMachineLifecycleOperation(req.MachineID), req.RequestDigest,
			code, detail, fmtTime(writerNow)); err != nil {
			return OperatorMachineLifecycleResult{}, fmt.Errorf("store: persist machine lifecycle rejection: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return OperatorMachineLifecycleResult{}, fmt.Errorf("store: commit machine lifecycle rejection: %w", err)
		}
		rejection := operatorError(code, detail)
		rejection.Audited = true
		return OperatorMachineLifecycleResult{}, rejection
	}

	if !validMachineLifecycleState(req.DesiredState) {
		return reject(OperatorCodeBadLifecycle)
	}
	if strings.TrimSpace(req.Reason) == "" {
		return reject(OperatorCodeReasonRequired)
	}
	if req.ExpectedRevision == nil {
		return reject(OperatorCodePreconditionRequired)
	}
	if strings.TrimSpace(req.PreviewDigest) == "" {
		return reject(OperatorCodePreviewRequired)
	}
	snapshot, err := loadOperatorMachineLifecycleSnapshot(tx, req.MachineID, writerNow)
	if err != nil {
		var rejection *OperatorRequestError
		if errors.As(err, &rejection) && (rejection.Code == OperatorCodeMachineNotFound ||
			rejection.Code == OperatorCodeLifecycleRevisionLimit) {
			return reject(rejection.Code)
		}
		return OperatorMachineLifecycleResult{}, err
	}
	audit.Subject = snapshot.DisplayName
	if req.ConfirmDisplayName != snapshot.DisplayName {
		return reject(OperatorCodeConfirmationMismatch)
	}
	if *req.ExpectedRevision != snapshot.Revision {
		return reject(OperatorCodePreconditionFailed)
	}
	if snapshot.State != req.DesiredState && snapshot.Revision >= MaxMachineLifecycleRevision-1 {
		return reject(OperatorCodeLifecycleRevisionLimit)
	}
	preview := operatorMachineLifecyclePreview(snapshot, req.DesiredState, writerNow)
	if req.PreviewDigest != preview.PreviewDigest {
		return reject(OperatorCodePreviewStale)
	}
	changed := snapshot.State != req.DesiredState
	if changed && len(preview.Blockers) > 0 {
		for _, blocker := range preview.Blockers {
			if blocker == MachineLifecycleBlockerNonterminalJobs {
				return reject(OperatorCodeMachineActiveJob)
			}
		}
		return OperatorMachineLifecycleResult{}, errors.New("store: unrecognized machine lifecycle blocker")
	}
	var rawCreated string
	var currentRetired sql.NullString
	if err := tx.QueryRow(`SELECT created_at,retired_at FROM machine_registry WHERE machine_id=?`, req.MachineID).
		Scan(&rawCreated, &currentRetired); err != nil {
		return OperatorMachineLifecycleResult{}, fmt.Errorf("store: read lifecycle chronology: %w", err)
	}
	if err := validateMachineLifecycleTimeTx(tx, req.MachineID, rawCreated, currentRetired, writerNow); err != nil {
		return OperatorMachineLifecycleResult{}, fmt.Errorf("store: validate lifecycle chronology: %w", err)
	}

	receipt := operatorMachineLifecycleReceipt{
		SchemaVersion: operatorMachineLifecycleVersion,
		MachineID:     snapshot.MachineID, DisplayName: snapshot.DisplayName,
		PreviousState: snapshot.State, State: req.DesiredState,
		LifecycleRevision: snapshot.Revision, Changed: changed, NoOp: !changed,
		RetiredAt: snapshot.RetiredAt, AppliedAt: writerNow,
		OperatorMachineLifecycleImpact: preview.OperatorMachineLifecycleImpact,
		PreviewDigest:                  req.PreviewDigest,
	}
	var closedSessionIDs []string
	if changed {
		var retiredValue any
		if req.DesiredState == MachineLifecycleRetired {
			retiredValue = fmtTime(writerNow)
			retired := writerNow
			receipt.RetiredAt = &retired
		} else {
			receipt.RetiredAt = nil
		}
		statePredicate := "retired_at IS NULL"
		if snapshot.State == MachineLifecycleRetired {
			statePredicate = "retired_at IS NOT NULL"
		}
		res, err := tx.Exec(`UPDATE machine_registry SET retired_at=?,lifecycle_revision=lifecycle_revision+1
		 WHERE machine_id=? AND lifecycle_revision=? AND `+statePredicate,
			retiredValue, req.MachineID, snapshot.Revision)
		if err != nil {
			return OperatorMachineLifecycleResult{}, fmt.Errorf("store: apply operator machine lifecycle: %w", err)
		}
		if rows, err := res.RowsAffected(); err != nil || rows != 1 {
			return OperatorMachineLifecycleResult{}, errors.New("store: machine lifecycle changed during transaction")
		}
		if req.DesiredState == MachineLifecycleRetired {
			closedSessionIDs, err = closeOpenAgentSessionsForMachine(tx, req.MachineID, writerNow, AgentSessionCloseReasonMachineRetired)
			if err != nil {
				return OperatorMachineLifecycleResult{}, fmt.Errorf("store: close sessions after operator machine retirement: %w", err)
			}
		}
		receipt.LifecycleRevision++
		eventType := ChangeReadRegistered
		if req.DesiredState == MachineLifecycleRetired {
			eventType = ChangeReadRetired
		}
		res, err = tx.Exec(`INSERT INTO machine_registry_lifecycle_events(machine_id,event_type,occurred_at)
		 VALUES(?,?,?)`, req.MachineID, eventType, fmtTime(writerNow))
		if err != nil {
			return OperatorMachineLifecycleResult{}, fmt.Errorf("store: append operator machine lifecycle event: %w", err)
		}
		eventID, err := res.LastInsertId()
		if err != nil || eventID <= 0 {
			return OperatorMachineLifecycleResult{}, errors.New("store: read appended machine lifecycle event identity")
		}
		receipt.TransitionEventID = &eventID
	}

	raw, err := json.Marshal(receipt)
	if err != nil {
		return OperatorMachineLifecycleResult{}, fmt.Errorf("store: encode machine lifecycle receipt: %w", err)
	}
	if _, err := tx.Exec(`INSERT INTO operator_idempotency
	 (idempotency_key,operation,request_digest,outcome,response_json,created_at)
	 VALUES (?,?,?,'ok',?,?)`, req.IdempotencyKey,
		operatorMachineLifecycleOperation(req.MachineID), req.RequestDigest,
		string(raw), fmtTime(writerNow)); err != nil {
		return OperatorMachineLifecycleResult{}, fmt.Errorf("store: persist machine lifecycle receipt: %w", err)
	}
	audit.OK = true
	audit.Detail = operatorMachineLifecycleSuccessAuditDetail(receipt)
	if err := s.recordAuditTx(tx, audit); err != nil {
		return OperatorMachineLifecycleResult{}, fmt.Errorf("store: record machine lifecycle success audit: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return OperatorMachineLifecycleResult{}, fmt.Errorf("store: commit operator machine lifecycle: %w", err)
	}
	result := operatorMachineLifecycleResult(receipt)
	result.ClosedSessionIDs = closedSessionIDs
	result.Audited = true
	return result, nil
}

func canonicalOperatorMachineLifecycleRejectionDetail(code string) (string, bool) {
	switch code {
	case OperatorCodeBadLifecycle:
		return "desired_state 只接受 active 或 retired", true
	case OperatorCodeMachineNotFound:
		return "找不到這台機器", true
	case OperatorCodePreconditionRequired:
		return "expected_revision 不可省略", true
	case OperatorCodePreconditionFailed:
		return "lifecycle revision 已改變；請重新讀取機器後再預覽", true
	case OperatorCodeConfirmationMismatch:
		return "confirm_display_name 必須與目前顯示名稱逐字相同", true
	case OperatorCodePreviewRequired:
		return "preview_digest 不可省略；請先重新預覽", true
	case OperatorCodePreviewStale:
		return "preview_digest 與目前 lifecycle、credential、ticket、job 或 policy 不符；請重新預覽", true
	case OperatorCodeMachineActiveJob:
		return "機器仍有未終態 job；先讓工作單結束，再退役機器", true
	case OperatorCodeLifecycleRevisionLimit:
		return "lifecycle revision 已耗盡或損毀；拒絕讀取或變更", true
	case OperatorCodeReasonRequired:
		return "reason 不可省略或只含空白", true
	default:
		return "", false
	}
}

func operatorMachineLifecycleRejectionAuditDetail(code, detail string) string {
	return truncAudit(operatorMachineLifecycleRejectPrefix+code+"；"+detail, auditMaxReason)
}

func operatorMachineLifecycleSuccessAuditDetail(receipt operatorMachineLifecycleReceipt) string {
	raw, _ := json.Marshal(receipt)
	return fmt.Sprintf("machine lifecycle %s→%s；changed=%t no_op=%t revision=%d receipt_sha256=%s",
		receipt.PreviousState, receipt.State, receipt.Changed, receipt.NoOp,
		receipt.LifecycleRevision, sha256Hex(string(raw)))
}

func operatorMachineLifecycleResult(receipt operatorMachineLifecycleReceipt) OperatorMachineLifecycleResult {
	return OperatorMachineLifecycleResult{
		MachineID: receipt.MachineID, DisplayName: receipt.DisplayName,
		PreviousState: receipt.PreviousState, State: receipt.State,
		LifecycleRevision: receipt.LifecycleRevision, Changed: receipt.Changed, NoOp: receipt.NoOp,
		TransitionEventID: receipt.TransitionEventID, RetiredAt: receipt.RetiredAt, AppliedAt: receipt.AppliedAt,
		OperatorMachineLifecycleImpact: receipt.OperatorMachineLifecycleImpact,
		PreviewDigest:                  receipt.PreviewDigest,
	}
}

func (s *Store) replayOperatorMachineLifecycle(tx dbTx, req OperatorMachineLifecycleRequest,
	audit AuditEntry, cached operatorCachedRequest,
) (OperatorMachineLifecycleResult, error) {
	if cached.Operation != operatorMachineLifecycleOperation(req.MachineID) || cached.Digest != req.RequestDigest {
		audit.OK = false
		audit.Detail = "idempotency conflict：idempotency key 已被不同的 operation 或 canonical request body 使用"
		if err := s.recordAuditTx(tx, audit); err != nil {
			return OperatorMachineLifecycleResult{}, fmt.Errorf("store: record lifecycle idempotency conflict audit: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return OperatorMachineLifecycleResult{}, fmt.Errorf("store: commit lifecycle idempotency conflict audit: %w", err)
		}
		rejection := operatorError(OperatorCodeIdempotencyConflict,
			"idempotency key 已被不同的 operation 或 canonical request body 使用")
		rejection.Audited = true
		return OperatorMachineLifecycleResult{}, rejection
	}
	if !canonicalOperatorEnrollCacheTime(cached.CreatedAt) {
		return OperatorMachineLifecycleResult{}, errors.New(operatorMachineLifecycleCacheInvalid)
	}
	if cached.Outcome == "rejected" {
		if cached.ResponseJSON.Valid || !cached.ErrorCode.Valid || !cached.ErrorDetail.Valid {
			return OperatorMachineLifecycleResult{}, errors.New(operatorMachineLifecycleCacheInvalid)
		}
		canonical, ok := canonicalOperatorMachineLifecycleRejectionDetail(cached.ErrorCode.String)
		if !ok || cached.ErrorDetail.String != canonical {
			return OperatorMachineLifecycleResult{}, errors.New(operatorMachineLifecycleCacheInvalid)
		}
		valid, err := validateOperatorMachineLifecycleAuditEvidence(tx, req, cached.CreatedAt,
			false, operatorMachineLifecycleRejectionAuditDetail(cached.ErrorCode.String, canonical))
		if err != nil || !valid {
			return OperatorMachineLifecycleResult{}, errors.New(operatorMachineLifecycleCacheInvalid)
		}
		audit.OK = false
		audit.Detail = OperatorIdempotencyReplayPrefix + "原判決：" + canonical
		if err := s.recordAuditTx(tx, audit); err != nil {
			return OperatorMachineLifecycleResult{}, fmt.Errorf("store: record rejected lifecycle replay audit: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return OperatorMachineLifecycleResult{}, fmt.Errorf("store: commit rejected lifecycle replay audit: %w", err)
		}
		return OperatorMachineLifecycleResult{}, &OperatorRequestError{
			Code: cached.ErrorCode.String, Detail: canonical, Replayed: true, Audited: true,
		}
	}
	if cached.Outcome != "ok" || !cached.ResponseJSON.Valid || cached.ErrorCode.Valid || cached.ErrorDetail.Valid {
		return OperatorMachineLifecycleResult{}, errors.New(operatorMachineLifecycleCacheInvalid)
	}
	receipt, err := decodeOperatorMachineLifecycleReceipt(cached.ResponseJSON.String)
	if err != nil || validateOperatorMachineLifecycleReceipt(receipt, req, cached.CreatedAt) != nil {
		return OperatorMachineLifecycleResult{}, errors.New(operatorMachineLifecycleCacheInvalid)
	}
	valid, err := validateOperatorMachineLifecycleSuccessEvidence(tx, receipt, req)
	if err != nil || !valid {
		return OperatorMachineLifecycleResult{}, errors.New(operatorMachineLifecycleCacheInvalid)
	}
	audit.Subject = receipt.DisplayName
	audit.OK = true
	audit.Detail = OperatorIdempotencyReplayPrefix + "沒有再次改 lifecycle state、revision 或 transition ledger"
	if err := s.recordAuditTx(tx, audit); err != nil {
		return OperatorMachineLifecycleResult{}, fmt.Errorf("store: record successful lifecycle replay audit: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return OperatorMachineLifecycleResult{}, fmt.Errorf("store: commit successful lifecycle replay audit: %w", err)
	}
	result := operatorMachineLifecycleResult(receipt)
	result.Replayed, result.Audited = true, true
	return result, nil
}

func validateOperatorMachineLifecycleReceipt(receipt operatorMachineLifecycleReceipt,
	req OperatorMachineLifecycleRequest, cachedCreatedAt string,
) error {
	if receipt.MachineID != req.MachineID ||
		receipt.MachineID == "" || receipt.DisplayName == "" || !validMachineLifecycleState(receipt.PreviousState) ||
		!validMachineLifecycleState(receipt.State) || receipt.State != req.DesiredState ||
		receipt.PreviewDigest == "" || receipt.PreviewDigest != req.PreviewDigest || req.ExpectedRevision == nil ||
		receipt.LifecycleRevision < 0 || receipt.LifecycleRevision >= MaxMachineLifecycleRevision ||
		!canonicalOperatorTime(receipt.AppliedAt) ||
		fmtTime(receipt.AppliedAt) != cachedCreatedAt || receipt.Changed == receipt.NoOp ||
		receipt.Changed != (receipt.PreviousState != receipt.State) || receipt.NoOp != (receipt.PreviousState == receipt.State) ||
		!receipt.RegistryRetained || !receipt.HistoryPreserved || !receipt.ChannelPreserved ||
		receipt.PendingEnrollmentTokenCount < 0 || receipt.PendingEnrollmentTokenExpiredCount < 0 ||
		receipt.PendingEnrollmentTokenExpiredCount > receipt.PendingEnrollmentTokenCount || receipt.ActiveJobCount < 0 {
		return errors.New("cached lifecycle receipt fields are invalid")
	}
	beforeActive := receipt.PreviousState == MachineLifecycleActive
	afterActive := receipt.State == MachineLifecycleActive
	wantBefore, wantAfter := beforeActive, afterActive
	switch receipt.SchemaVersion {
	case operatorMachineLifecycleVersion:
		if receipt.LegacyExpected != nil {
			return errors.New("cached lifecycle v2 receipt contains legacy expected")
		}
	case operatorMachineLifecycleLegacyVersion:
		if receipt.LegacyExpected == nil {
			return errors.New("cached lifecycle v1 receipt lacks expected")
		}
		wantBefore = *receipt.LegacyExpected && beforeActive
		wantAfter = *receipt.LegacyExpected && afterActive
	default:
		return errors.New("cached lifecycle receipt schema is unsupported")
	}
	if receipt.InDenominatorBefore != wantBefore || receipt.InDenominatorAfter != wantAfter ||
		receipt.DenominatorDelta != int64(boolToInt(wantAfter)-boolToInt(wantBefore)) {
		return errors.New("cached lifecycle denominator impact is invalid")
	}
	wantRevision := *req.ExpectedRevision
	if receipt.Changed {
		if wantRevision >= MaxMachineLifecycleRevision || receipt.LifecycleRevision != wantRevision+1 ||
			receipt.TransitionEventID == nil || *receipt.TransitionEventID <= 0 {
			return errors.New("cached lifecycle transition receipt is invalid")
		}
	} else if receipt.LifecycleRevision != wantRevision || receipt.TransitionEventID != nil {
		return errors.New("cached lifecycle no-op receipt is invalid")
	}
	if receipt.State == MachineLifecycleRetired {
		if receipt.RetiredAt == nil || !canonicalOperatorTime(*receipt.RetiredAt) {
			return errors.New("cached retired lifecycle receipt is invalid")
		}
	} else if receipt.RetiredAt != nil {
		return errors.New("cached active lifecycle receipt is invalid")
	}
	if receipt.AgentAuthenticationBefore != (receipt.AgentCredentialPresent && receipt.PreviousState == MachineLifecycleActive) ||
		receipt.AgentAuthenticationAfter != (receipt.AgentCredentialPresent && receipt.State == MachineLifecycleActive) ||
		receipt.PendingEnrollmentRedemptionBefore != (receipt.PendingEnrollmentTokenCount > receipt.PendingEnrollmentTokenExpiredCount && receipt.PreviousState == MachineLifecycleActive) ||
		receipt.PendingEnrollmentRedemptionAfter != (receipt.PendingEnrollmentTokenCount > receipt.PendingEnrollmentTokenExpiredCount && receipt.State == MachineLifecycleActive) {
		return errors.New("cached lifecycle impact is invalid")
	}
	return nil
}

func validateOperatorMachineLifecycleSuccessEvidence(tx dbTx, receipt operatorMachineLifecycleReceipt,
	req OperatorMachineLifecycleRequest,
) (bool, error) {
	if receipt.Changed {
		wantEvent := ChangeReadRegistered
		if receipt.State == MachineLifecycleRetired {
			wantEvent = ChangeReadRetired
		}
		var eventCount int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM machine_registry_lifecycle_events
		 WHERE event_id=? AND machine_id=? AND event_type=? AND occurred_at=?`,
			*receipt.TransitionEventID, receipt.MachineID, wantEvent, fmtTime(receipt.AppliedAt)).Scan(&eventCount); err != nil || eventCount != 1 {
			return false, err
		}
	}
	return validateOperatorMachineLifecycleAuditEvidence(tx, req, fmtTime(receipt.AppliedAt), true,
		operatorMachineLifecycleSuccessAuditDetail(receipt))
}

func validateOperatorMachineLifecycleAuditEvidence(tx dbTx, req OperatorMachineLifecycleRequest,
	at string, ok bool, detail string,
) (bool, error) {
	var count int
	err := tx.QueryRow(`SELECT COUNT(*) FROM audit_log
	 WHERE action=? AND machine_id=? AND COALESCE(reason,'')=?
	   AND idempotency_key=? AND request_digest=? AND at=? AND outcome=? AND COALESCE(detail,'')=?`,
		string(AuditMachineLifecycle), req.MachineID, truncAudit(req.Reason, auditMaxReason),
		req.IdempotencyKey, req.RequestDigest, at, outcomeOf(ok), detail).Scan(&count)
	return count == 1, err
}

func decodeOperatorMachineLifecycleReceipt(raw string) (operatorMachineLifecycleReceipt, error) {
	var receipt operatorMachineLifecycleReceipt
	if err := rejectDuplicateLifecycleReceiptFields(raw); err != nil {
		return receipt, err
	}
	dec := json.NewDecoder(bytes.NewReader([]byte(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&receipt); err != nil {
		return receipt, err
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return receipt, errors.New("cached lifecycle receipt has trailing JSON")
	}
	return receipt, nil
}

func rejectDuplicateLifecycleReceiptFields(raw string) error {
	dec := json.NewDecoder(bytes.NewReader([]byte(raw)))
	opening, err := dec.Token()
	if err != nil {
		return err
	}
	if delimiter, ok := opening.(json.Delim); !ok || delimiter != '{' {
		return errors.New("cached lifecycle receipt is not an object")
	}
	seen := make(map[string]struct{}, 32)
	for dec.More() {
		nameToken, err := dec.Token()
		if err != nil {
			return err
		}
		name, ok := nameToken.(string)
		if !ok {
			return errors.New("cached lifecycle receipt has an invalid field name")
		}
		if _, duplicate := seen[name]; duplicate {
			return fmt.Errorf("cached lifecycle receipt has duplicate field %q", name)
		}
		seen[name] = struct{}{}
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return err
		}
	}
	if closing, err := dec.Token(); err != nil || closing != json.Delim('}') {
		return errors.New("cached lifecycle receipt has invalid closing token")
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("cached lifecycle receipt has trailing JSON")
	}
	return nil
}
