package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	OperatorMachineDisplayNameMaxBytes = 256

	OperatorCodeMachineRenameInvalid              = "MACHINE_RENAME_INVALID"
	OperatorCodeMachineRenameUnchanged            = "MACHINE_RENAME_UNCHANGED"
	OperatorCodeMachineRenameNameTaken            = "MACHINE_RENAME_NAME_TAKEN"
	OperatorCodeMachineRenamePreviewRequired      = "MACHINE_RENAME_PREVIEW_REQUIRED"
	OperatorCodeMachineRenamePreviewStale         = "MACHINE_RENAME_PREVIEW_STALE"
	OperatorCodeMachineRenameConfirmationMismatch = "MACHINE_RENAME_CONFIRMATION_MISMATCH"
)

var (
	ErrMachineRenameInvalid              = errors.New("store: machine display name is invalid")
	ErrMachineRenameUnchanged            = errors.New("store: machine display name is unchanged")
	ErrMachineRenameNameTaken            = errors.New("store: machine display name is already in use")
	ErrMachineRenamePreviewRequired      = errors.New("store: machine rename preview is required")
	ErrMachineRenamePreviewStale         = errors.New("store: machine rename preview is stale")
	ErrMachineRenameConfirmationMismatch = errors.New("store: machine rename confirmation does not match")
)

const (
	operatorMachineRenameVersion = "v1"
	machineRenameReplayDetail    = "沒有再次更改名冊顯示名稱"
)

type OperatorMachineRenamePreviewResult struct {
	MachineID                string    `json:"machine_id"`
	CurrentDisplayName       string    `json:"current_display_name"`
	DisplayName              string    `json:"display_name"`
	PreviewedAt              time.Time `json:"previewed_at"`
	MachineIDPreserved       bool      `json:"machine_id_preserved"`
	AgentUnaffected          bool      `json:"agent_unaffected"`
	ExpectationKeyChanges    bool      `json:"expectation_key_changes"`
	PendingTokenLabelChanges bool      `json:"pending_token_label_changes"`
	PreviewDigest            string    `json:"preview_digest"`
}

type OperatorMachineRenameRequest struct {
	MachineID          string
	DisplayName        string
	ConfirmDisplayName string
	PreviewDigest      string
	Reason             string
	IdempotencyKey     string
	RequestDigest      string
	Audit              AuditEntry
}

type OperatorMachineRenameResult struct {
	MachineID                string    `json:"machine_id"`
	PreviousDisplayName      string    `json:"previous_display_name"`
	DisplayName              string    `json:"display_name"`
	AppliedAt                time.Time `json:"applied_at"`
	MachineIDPreserved       bool      `json:"machine_id_preserved"`
	AgentUnaffected          bool      `json:"agent_unaffected"`
	ExpectationKeyChanged    bool      `json:"expectation_key_changed"`
	PendingTokenLabelChanged bool      `json:"pending_token_label_changed"`
	PreviewDigest            string    `json:"preview_digest"`
	Replayed                 bool      `json:"replayed"`
	Audited                  bool      `json:"-"`
}

func (s *Store) PreviewOperatorMachineRename(machineID, displayName string) (OperatorMachineRenamePreviewResult, error) {
	current, err := machineRenameCurrentName(s.rdb, machineID)
	if err != nil {
		return OperatorMachineRenamePreviewResult{}, err
	}
	if rejection := validateMachineRenameIntent(displayName, current); rejection != nil {
		return OperatorMachineRenamePreviewResult{}, rejection
	}
	if taken, err := machineDisplayNameTaken(s.rdb, machineID, displayName); err != nil {
		return OperatorMachineRenamePreviewResult{}, err
	} else if taken {
		return OperatorMachineRenamePreviewResult{}, machineRenameError(OperatorCodeMachineRenameNameTaken)
	}
	var pending int
	if err := s.rdb.QueryRow(`SELECT COUNT(*) FROM enrollment_tokens WHERE used_by=? AND used_at IS NULL`,
		machineID).Scan(&pending); err != nil {
		return OperatorMachineRenamePreviewResult{}, fmt.Errorf("store: count pending token for rename: %w", err)
	}
	now := s.now().UTC()
	return OperatorMachineRenamePreviewResult{
		MachineID: machineID, CurrentDisplayName: current, DisplayName: displayName,
		PreviewedAt: now, MachineIDPreserved: true, AgentUnaffected: true,
		ExpectationKeyChanges: true, PendingTokenLabelChanges: pending > 0,
		PreviewDigest: machineRenamePreviewDigest(machineID, current, displayName),
	}, nil
}

func (s *Store) ApplyOperatorMachineRename(req OperatorMachineRenameRequest) (OperatorMachineRenameResult, error) {
	if strings.TrimSpace(req.IdempotencyKey) == "" || len(req.IdempotencyKey) > 200 {
		return OperatorMachineRenameResult{}, operatorError(OperatorCodeIdempotencyKeyRequired,
			"Idempotency-Key 不可省略且最多 200 bytes")
	}
	if req.RequestDigest == "" {
		return OperatorMachineRenameResult{}, operatorError(OperatorCodeRequestDigestRequired,
			"request body digest 不可省略")
	}

	operation := "machine-rename:" + operatorMachineRenameVersion + ":" + req.MachineID
	audit := req.Audit
	audit.Action, audit.MachineID = AuditMachineRename, req.MachineID
	audit.IdempotencyKey, audit.RequestDigest = req.IdempotencyKey, req.RequestDigest
	if audit.Subject == "" {
		audit.Subject = req.MachineID
	}
	tx, err := s.beginWrite(context.Background(), "apply_operator_machine_rename")
	if err != nil {
		return OperatorMachineRenameResult{}, fmt.Errorf("store: begin machine rename: %w", err)
	}
	defer tx.Rollback()
	writerNow := s.now().UTC().Truncate(time.Second)
	audit.At = writerNow

	replayed, rejection, err := s.policyIdempotency(tx, req.IdempotencyKey, operation,
		req.RequestDigest, &audit, func(raw string) (any, error) {
			var result OperatorMachineRenameResult
			if err := json.Unmarshal([]byte(raw), &result); err != nil {
				return nil, err
			}
			if result.MachineID != req.MachineID || result.PreviousDisplayName != req.ConfirmDisplayName ||
				result.DisplayName != req.DisplayName ||
				result.AppliedAt.IsZero() || !result.MachineIDPreserved || !result.AgentUnaffected ||
				!result.ExpectationKeyChanged || result.PreviewDigest != req.PreviewDigest || result.Replayed {
				return nil, errors.New("cached machine rename receipt is invalid")
			}
			return result, nil
		}, machineRenameReplayDetail)
	if err != nil {
		return OperatorMachineRenameResult{}, err
	}
	if rejection != nil {
		return OperatorMachineRenameResult{}, rejection
	}
	if replayed != nil {
		result := replayed.(OperatorMachineRenameResult)
		result.Replayed, result.Audited = true, true
		return result, nil
	}

	current, err := machineRenameCurrentName(tx, req.MachineID)
	if errors.Is(err, ErrNotFound) {
		return OperatorMachineRenameResult{}, s.rejectMachineRename(tx, req, &audit,
			operation, OperatorCodeMachineNotFound, "找不到這台機器")
	}
	if err != nil {
		return OperatorMachineRenameResult{}, err
	}
	audit.Subject = current
	if rejection := validateMachineRenameIntent(req.DisplayName, current); rejection != nil {
		return OperatorMachineRenameResult{}, s.rejectMachineRename(tx, req, &audit,
			operation, rejection.Code, rejection.Detail)
	}
	if strings.TrimSpace(req.Reason) == "" || req.Reason != strings.TrimSpace(req.Reason) || len(req.Reason) > auditMaxReason {
		return OperatorMachineRenameResult{}, s.rejectMachineRename(tx, req, &audit,
			operation, OperatorCodeReasonRequired, "reason 不可省略、前後不可有空白，且最多 500 bytes")
	}
	if req.ConfirmDisplayName != current {
		return OperatorMachineRenameResult{}, s.rejectMachineRename(tx, req, &audit,
			operation, OperatorCodeMachineRenameConfirmationMismatch, "確認名稱與目前的 display_name 不符；請重新預覽")
	}
	if req.PreviewDigest == "" {
		return OperatorMachineRenameResult{}, s.rejectMachineRename(tx, req, &audit,
			operation, OperatorCodeMachineRenamePreviewRequired, "preview_digest 不可省略；請先重新預覽")
	}
	expectedDigest := machineRenamePreviewDigest(req.MachineID, current, req.DisplayName)
	if req.PreviewDigest != expectedDigest {
		return OperatorMachineRenameResult{}, s.rejectMachineRename(tx, req, &audit,
			operation, OperatorCodeMachineRenamePreviewStale, "名冊名稱或重新命名內容已改變；請重新預覽")
	}
	if taken, err := machineDisplayNameTaken(tx, req.MachineID, req.DisplayName); err != nil {
		return OperatorMachineRenameResult{}, err
	} else if taken {
		return OperatorMachineRenameResult{}, s.rejectMachineRename(tx, req, &audit,
			operation, OperatorCodeMachineRenameNameTaken, "另一台機器已使用這個 display_name；請換一個名稱")
	}
	resultSQL, err := tx.Exec(`UPDATE machine_registry SET display_name=? WHERE machine_id=? AND display_name=?`,
		req.DisplayName, req.MachineID, current)
	if err != nil {
		return OperatorMachineRenameResult{}, fmt.Errorf("store: update machine display name: %w", err)
	}
	changed, err := resultSQL.RowsAffected()
	if err != nil {
		return OperatorMachineRenameResult{}, fmt.Errorf("store: count renamed registry rows: %w", err)
	}
	if changed != 1 {
		return OperatorMachineRenameResult{}, fmt.Errorf("store: machine rename affected %d rows; want 1", changed)
	}
	tokenResult, err := tx.Exec(`UPDATE enrollment_tokens SET display_name=? WHERE used_by=? AND used_at IS NULL`,
		req.DisplayName, req.MachineID)
	if err != nil {
		return OperatorMachineRenameResult{}, fmt.Errorf("store: update pending enrollment label: %w", err)
	}
	pendingChanged, err := tokenResult.RowsAffected()
	if err != nil {
		return OperatorMachineRenameResult{}, fmt.Errorf("store: count renamed pending enrollment labels: %w", err)
	}
	result := OperatorMachineRenameResult{
		MachineID: req.MachineID, PreviousDisplayName: current, DisplayName: req.DisplayName,
		AppliedAt: writerNow, MachineIDPreserved: true, AgentUnaffected: true,
		ExpectationKeyChanged: true, PendingTokenLabelChanged: pendingChanged > 0,
		PreviewDigest: req.PreviewDigest,
	}
	audit.Subject = req.DisplayName
	audit.Detail = current + " → " + req.DisplayName
	if err := s.commitMachineRename(tx, req, operation, &audit, result); err != nil {
		return OperatorMachineRenameResult{}, err
	}
	result.Audited = true
	return result, nil
}

type machineRenameQuery interface {
	QueryRow(query string, args ...any) *sql.Row
}

func machineRenameCurrentName(q machineRenameQuery, machineID string) (string, error) {
	var name string
	if err := q.QueryRow(`SELECT display_name FROM machine_registry WHERE machine_id=?`, machineID).Scan(&name); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrNotFound
		}
		return "", fmt.Errorf("store: read machine rename identity: %w", err)
	}
	return name, nil
}

func machineDisplayNameTaken(q machineRenameQuery, machineID, displayName string) (bool, error) {
	var found int
	err := q.QueryRow(`SELECT 1 FROM machine_registry WHERE display_name=? AND machine_id<>? LIMIT 1`,
		displayName, machineID).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: inspect machine display name: %w", err)
	}
	return true, nil
}

func validateMachineRenameIntent(displayName, current string) *OperatorRequestError {
	if strings.TrimSpace(displayName) == "" || displayName != strings.TrimSpace(displayName) ||
		len(displayName) > OperatorMachineDisplayNameMaxBytes || !utf8.ValidString(displayName) {
		return machineRenameError(OperatorCodeMachineRenameInvalid)
	}
	for _, char := range displayName {
		if unicode.IsControl(char) || unicode.Is(unicode.Cf, char) {
			return machineRenameError(OperatorCodeMachineRenameInvalid)
		}
	}
	if displayName == current {
		return machineRenameError(OperatorCodeMachineRenameUnchanged)
	}
	return nil
}

func machineRenameError(code string) *OperatorRequestError {
	switch code {
	case OperatorCodeMachineRenameInvalid:
		return operatorError(code, "display_name 不可為空、前後不可有空白、不可含控制字元，且最多 256 bytes")
	case OperatorCodeMachineRenameUnchanged:
		return operatorError(code, "新的 display_name 與目前名稱相同；名冊沒有需要變更的內容")
	case OperatorCodeMachineRenameNameTaken:
		return operatorError(code, "另一台機器已使用這個 display_name；請換一個名稱")
	default:
		return operatorError(code, "重新命名被拒絕")
	}
}

func machineRenamePreviewDigest(machineID, current, desired string) string {
	body := struct {
		Version               string `json:"version"`
		MachineID             string `json:"machine_id"`
		CurrentDisplayName    string `json:"current_display_name"`
		DisplayName           string `json:"display_name"`
		MachineIDPreserved    bool   `json:"machine_id_preserved"`
		AgentUnaffected       bool   `json:"agent_unaffected"`
		ExpectationKeyChanges bool   `json:"expectation_key_changes"`
	}{operatorMachineRenameVersion, machineID, current, desired, true, true, true}
	raw, _ := json.Marshal(body)
	return digestOf(string(raw))
}

func (s *Store) rejectMachineRename(tx dbTx, req OperatorMachineRenameRequest, audit *AuditEntry,
	operation, code, detail string,
) error {
	return s.policyReject(tx, req.IdempotencyKey, operation, req.RequestDigest, audit, code, detail)
}

func (s *Store) commitMachineRename(tx dbTx, req OperatorMachineRenameRequest, operation string,
	audit *AuditEntry, result OperatorMachineRenameResult,
) error {
	raw, err := json.Marshal(result)
	if err != nil {
		return fmt.Errorf("store: encode machine rename receipt: %w", err)
	}
	if _, err := tx.Exec(`INSERT INTO operator_idempotency
	 (idempotency_key,operation,request_digest,outcome,response_json,created_at)
	 VALUES (?,?,?,'ok',?,?)`, req.IdempotencyKey, operation, req.RequestDigest, string(raw),
		fmtTime(result.AppliedAt)); err != nil {
		return fmt.Errorf("store: persist machine rename receipt: %w", err)
	}
	audit.OK = true
	if err := s.recordAuditTx(tx, *audit); err != nil {
		return fmt.Errorf("store: record machine rename audit: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit machine rename: %w", err)
	}
	return nil
}
