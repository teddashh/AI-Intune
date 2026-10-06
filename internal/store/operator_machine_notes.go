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
	OperatorMachineNotesMaxBytes = 1000

	OperatorCodeMachineNotesInvalid              = "MACHINE_NOTES_INVALID"
	OperatorCodeMachineNotesUnchanged            = "MACHINE_NOTES_UNCHANGED"
	OperatorCodeMachineNotesPreviewRequired      = "MACHINE_NOTES_PREVIEW_REQUIRED"
	OperatorCodeMachineNotesPreviewStale         = "MACHINE_NOTES_PREVIEW_STALE"
	OperatorCodeMachineNotesConfirmationMismatch = "MACHINE_NOTES_CONFIRMATION_MISMATCH"
)

var (
	ErrMachineNotesInvalid              = errors.New("store: machine notes are invalid")
	ErrMachineNotesUnchanged            = errors.New("store: machine notes are unchanged")
	ErrMachineNotesPreviewRequired      = errors.New("store: machine notes preview is required")
	ErrMachineNotesPreviewStale         = errors.New("store: machine notes preview is stale")
	ErrMachineNotesConfirmationMismatch = errors.New("store: machine notes confirmation does not match")
)

const (
	operatorMachineNotesVersion = "v1"
	machineNotesReplayDetail    = "沒有再次更改名冊備註"
)

type OperatorMachineNotesPreviewResult struct {
	MachineID                     string    `json:"machine_id"`
	DisplayName                   string    `json:"display_name"`
	CurrentNotes                  string    `json:"current_notes"`
	Notes                         string    `json:"notes"`
	PreviewedAt                   time.Time `json:"previewed_at"`
	RegistryNotesChanged          bool      `json:"registry_notes_changed"`
	MachineConfigurationUnchanged bool      `json:"machine_configuration_unchanged"`
	AgentUnaffected               bool      `json:"agent_unaffected"`
	PreviewDigest                 string    `json:"preview_digest"`
}

type OperatorMachineNotesRequest struct {
	MachineID          string
	Notes              string
	ConfirmDisplayName string
	PreviewDigest      string
	Reason             string
	IdempotencyKey     string
	RequestDigest      string
	Audit              AuditEntry
}

// The durable result intentionally records only presence, not note text. The
// request digest still binds replay to the exact request, while old free text
// is not copied into the permanent idempotency receipt or audit detail.
type OperatorMachineNotesResult struct {
	MachineID                     string    `json:"machine_id"`
	DisplayName                   string    `json:"display_name"`
	PreviousNotesPresent          bool      `json:"previous_notes_present"`
	NotesPresent                  bool      `json:"notes_present"`
	AppliedAt                     time.Time `json:"applied_at"`
	RegistryNotesChanged          bool      `json:"registry_notes_changed"`
	MachineConfigurationUnchanged bool      `json:"machine_configuration_unchanged"`
	AgentUnaffected               bool      `json:"agent_unaffected"`
	PreviewDigest                 string    `json:"preview_digest"`
	Replayed                      bool      `json:"replayed"`
	Audited                       bool      `json:"-"`
}

func (s *Store) PreviewOperatorMachineNotes(machineID, notes string) (OperatorMachineNotesPreviewResult, error) {
	displayName, current, err := machineNotesCurrent(s.rdb, machineID)
	if err != nil {
		return OperatorMachineNotesPreviewResult{}, err
	}
	if rejection := validateMachineNotesIntent(notes, current); rejection != nil {
		return OperatorMachineNotesPreviewResult{}, rejection
	}
	now := s.now().UTC()
	return OperatorMachineNotesPreviewResult{
		MachineID: machineID, DisplayName: displayName, CurrentNotes: current, Notes: notes,
		PreviewedAt: now, RegistryNotesChanged: true, MachineConfigurationUnchanged: true,
		AgentUnaffected: true, PreviewDigest: machineNotesPreviewDigest(machineID, displayName, current, notes),
	}, nil
}

func (s *Store) ApplyOperatorMachineNotes(req OperatorMachineNotesRequest) (OperatorMachineNotesResult, error) {
	if strings.TrimSpace(req.IdempotencyKey) == "" || len(req.IdempotencyKey) > 200 {
		return OperatorMachineNotesResult{}, operatorError(OperatorCodeIdempotencyKeyRequired,
			"Idempotency-Key 不可省略且最多 200 bytes")
	}
	if req.RequestDigest == "" {
		return OperatorMachineNotesResult{}, operatorError(OperatorCodeRequestDigestRequired,
			"request body digest 不可省略")
	}

	operation := "machine-notes:" + operatorMachineNotesVersion + ":" + req.MachineID
	audit := req.Audit
	audit.Action, audit.MachineID = AuditMachineNotes, req.MachineID
	audit.IdempotencyKey, audit.RequestDigest = req.IdempotencyKey, req.RequestDigest
	if audit.Subject == "" {
		audit.Subject = req.MachineID
	}
	tx, err := s.beginWrite(context.Background(), "apply_operator_machine_notes")
	if err != nil {
		return OperatorMachineNotesResult{}, fmt.Errorf("store: begin machine notes update: %w", err)
	}
	defer tx.Rollback()
	writerNow := s.now().UTC().Truncate(time.Second)
	audit.At = writerNow

	replayed, rejection, err := s.policyIdempotency(tx, req.IdempotencyKey, operation,
		req.RequestDigest, &audit, func(raw string) (any, error) {
			var result OperatorMachineNotesResult
			if err := json.Unmarshal([]byte(raw), &result); err != nil {
				return nil, err
			}
			if result.MachineID != req.MachineID || result.DisplayName != req.ConfirmDisplayName ||
				result.NotesPresent != (req.Notes != "") || result.AppliedAt.IsZero() ||
				!result.RegistryNotesChanged || !result.MachineConfigurationUnchanged ||
				!result.AgentUnaffected || result.PreviewDigest != req.PreviewDigest || result.Replayed {
				return nil, errors.New("cached machine notes receipt is invalid")
			}
			return result, nil
		}, machineNotesReplayDetail)
	if err != nil {
		return OperatorMachineNotesResult{}, err
	}
	if rejection != nil {
		return OperatorMachineNotesResult{}, rejection
	}
	if replayed != nil {
		result := replayed.(OperatorMachineNotesResult)
		result.Replayed, result.Audited = true, true
		return result, nil
	}

	displayName, current, err := machineNotesCurrent(tx, req.MachineID)
	if errors.Is(err, ErrNotFound) {
		return OperatorMachineNotesResult{}, s.rejectMachineNotes(tx, req, &audit,
			operation, OperatorCodeMachineNotFound, "找不到這台機器")
	}
	if err != nil {
		return OperatorMachineNotesResult{}, err
	}
	audit.Subject = displayName
	if rejection := validateMachineNotesIntent(req.Notes, current); rejection != nil {
		return OperatorMachineNotesResult{}, s.rejectMachineNotes(tx, req, &audit,
			operation, rejection.Code, rejection.Detail)
	}
	if strings.TrimSpace(req.Reason) == "" || req.Reason != strings.TrimSpace(req.Reason) || len(req.Reason) > auditMaxReason {
		return OperatorMachineNotesResult{}, s.rejectMachineNotes(tx, req, &audit,
			operation, OperatorCodeReasonRequired, "reason 不可省略、前後不可有空白，且最多 500 bytes")
	}
	if req.ConfirmDisplayName != displayName {
		return OperatorMachineNotesResult{}, s.rejectMachineNotes(tx, req, &audit,
			operation, OperatorCodeMachineNotesConfirmationMismatch, "確認名稱與目前的 display_name 不符；請重新預覽")
	}
	if req.PreviewDigest == "" {
		return OperatorMachineNotesResult{}, s.rejectMachineNotes(tx, req, &audit,
			operation, OperatorCodeMachineNotesPreviewRequired, "preview_digest 不可省略；請先重新預覽")
	}
	expectedDigest := machineNotesPreviewDigest(req.MachineID, displayName, current, req.Notes)
	if req.PreviewDigest != expectedDigest {
		return OperatorMachineNotesResult{}, s.rejectMachineNotes(tx, req, &audit,
			operation, OperatorCodeMachineNotesPreviewStale, "名冊名稱、目前備註或新備註已改變；請重新預覽")
	}
	update, err := tx.Exec(`UPDATE machine_registry SET notes=?
 WHERE machine_id=? AND display_name=? AND COALESCE(notes,'')=?`,
		nullStr(req.Notes), req.MachineID, displayName, current)
	if err != nil {
		return OperatorMachineNotesResult{}, fmt.Errorf("store: update machine notes: %w", err)
	}
	changed, err := update.RowsAffected()
	if err != nil {
		return OperatorMachineNotesResult{}, fmt.Errorf("store: count updated machine notes rows: %w", err)
	}
	if changed != 1 {
		return OperatorMachineNotesResult{}, fmt.Errorf("store: machine notes update affected %d rows; want 1", changed)
	}
	result := OperatorMachineNotesResult{
		MachineID: req.MachineID, DisplayName: displayName,
		PreviousNotesPresent: current != "", NotesPresent: req.Notes != "", AppliedAt: writerNow,
		RegistryNotesChanged: true, MachineConfigurationUnchanged: true, AgentUnaffected: true,
		PreviewDigest: req.PreviewDigest,
	}
	audit.Detail = machineNotesAuditDetail(req.Notes)
	if err := s.commitMachineNotes(tx, req, operation, &audit, result); err != nil {
		return OperatorMachineNotesResult{}, err
	}
	result.Audited = true
	return result, nil
}

type machineNotesQuery interface {
	QueryRow(query string, args ...any) *sql.Row
}

func machineNotesCurrent(q machineNotesQuery, machineID string) (string, string, error) {
	var displayName, notes string
	if err := q.QueryRow(`SELECT display_name,COALESCE(notes,'') FROM machine_registry WHERE machine_id=?`,
		machineID).Scan(&displayName, &notes); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", "", ErrNotFound
		}
		return "", "", fmt.Errorf("store: read machine notes: %w", err)
	}
	if displayName == "" || !validMachineNotesValue(notes) {
		return "", "", errors.New("store: machine notes projection is invalid")
	}
	return displayName, notes, nil
}

func validateMachineNotesIntent(notes, current string) *OperatorRequestError {
	if !validMachineNotesValue(notes) {
		return machineNotesError(OperatorCodeMachineNotesInvalid)
	}
	if notes == current {
		return machineNotesError(OperatorCodeMachineNotesUnchanged)
	}
	return nil
}

func validMachineNotesValue(notes string) bool {
	if len(notes) > OperatorMachineNotesMaxBytes || !utf8.ValidString(notes) || notes != strings.TrimSpace(notes) {
		return false
	}
	for _, char := range notes {
		if unicode.IsControl(char) || unicode.Is(unicode.Cf, char) {
			return false
		}
	}
	return true
}

func machineNotesError(code string) *OperatorRequestError {
	switch code {
	case OperatorCodeMachineNotesInvalid:
		return operatorError(code, "notes 前後不可有空白、不可含控制字元，且最多 1000 bytes；空字串表示清除")
	case OperatorCodeMachineNotesUnchanged:
		return operatorError(code, "新的 notes 與目前備註相同；名冊沒有需要變更的內容")
	default:
		return operatorError(code, "名冊備註更新被拒絕")
	}
}

func machineNotesPreviewDigest(machineID, displayName, current, desired string) string {
	body := struct {
		Version                       string `json:"version"`
		MachineID                     string `json:"machine_id"`
		DisplayName                   string `json:"display_name"`
		CurrentNotes                  string `json:"current_notes"`
		Notes                         string `json:"notes"`
		RegistryNotesChanged          bool   `json:"registry_notes_changed"`
		MachineConfigurationUnchanged bool   `json:"machine_configuration_unchanged"`
		AgentUnaffected               bool   `json:"agent_unaffected"`
	}{operatorMachineNotesVersion, machineID, displayName, current, desired, true, true, true}
	raw, _ := json.Marshal(body)
	return digestOf(string(raw))
}

func machineNotesAuditDetail(notes string) string {
	if notes == "" {
		return "名冊備註已清除；機器設定與 agent 不變"
	}
	return "名冊備註已更新；機器設定與 agent 不變"
}

func (s *Store) rejectMachineNotes(tx dbTx, req OperatorMachineNotesRequest, audit *AuditEntry,
	operation, code, detail string,
) error {
	return s.policyReject(tx, req.IdempotencyKey, operation, req.RequestDigest, audit, code, detail)
}

func (s *Store) commitMachineNotes(tx dbTx, req OperatorMachineNotesRequest, operation string,
	audit *AuditEntry, result OperatorMachineNotesResult,
) error {
	raw, err := json.Marshal(result)
	if err != nil {
		return fmt.Errorf("store: encode machine notes receipt: %w", err)
	}
	if _, err := tx.Exec(`INSERT INTO operator_idempotency
	 (idempotency_key,operation,request_digest,outcome,response_json,created_at)
	 VALUES (?,?,?,'ok',?,?)`, req.IdempotencyKey, operation, req.RequestDigest, string(raw),
		fmtTime(result.AppliedAt)); err != nil {
		return fmt.Errorf("store: persist machine notes receipt: %w", err)
	}
	audit.OK = true
	if err := s.recordAuditTx(tx, *audit); err != nil {
		return fmt.Errorf("store: record machine notes audit: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit machine notes: %w", err)
	}
	return nil
}
