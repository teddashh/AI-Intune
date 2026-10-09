package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// 連線與寫入的 audit。
//
// ⚠ 這一層唯一的規則：**每一次按下去都要留一筆，成功失敗都留。**
//
// 一張只記成功的 audit 答不出最需要知道的那些事 ——
// 「有人試著 retire 一台不存在的機器」、「有人按了 Connect 但那台其實
// 綁在 localhost」。那些才是事後會想查的。
//
// ⚠ RecordAudit 這條 legacy/best-effort 路徑寫失敗**不可以**讓已完成的
// connect/retire 類動作假裝失敗；呼叫端要大聲 log。canonical operator
// mutation 是明確例外：它透過 recordAuditTx 把 authority change、idempotency
// 判決與 audit evidence 放在同一個 transaction，缺任何一項就全部不 commit。

// AuditAction 是動作的種類。⚠ 固定字串，不是自由文字 ——
// 這一欄是要被 GROUP BY 的，而 reason 才是給人寫自由文字的地方。
type AuditAction string

const (
	AuditHubGuessing               AuditAction = "hub-login-guessing"
	AuditHubSetup                  AuditAction = "hub-setup"
	AuditHubUserCreated            AuditAction = "hub-user-created"
	AuditHubUserDisabled           AuditAction = "hub-user-disabled"
	AuditHubUserEnabled            AuditAction = "hub-user-enabled"
	AuditHubUserRenamed            AuditAction = "hub-user-renamed"
	AuditHubUserEmailChanged       AuditAction = "hub-user-email-changed"
	AuditHubLoginOK                AuditAction = "hub-login-ok"
	AuditHubLoginFailed            AuditAction = "hub-login-failed"
	AuditHubLockout                AuditAction = "hub-lockout"
	AuditHubLogout                 AuditAction = "hub-logout"
	AuditMFAEnabled                AuditAction = "mfa-enabled"
	AuditMFADisabled               AuditAction = "mfa-disabled"
	AuditMFAFailed                 AuditAction = "mfa-failed"
	AuditRecoveryCodeUsed          AuditAction = "recovery-code-used"
	AuditRecoveryCodesRegenerated  AuditAction = "recovery-codes-regenerated"
	AuditConnect                   AuditAction = "connect"
	AuditRetire                    AuditAction = "retire"
	AuditUnretire                  AuditAction = "unretire"
	AuditKeyedInstaller            AuditAction = "keyed-installer-download"
	AuditEnrollToken               AuditAction = "enroll-token"
	AuditRevokeToken               AuditAction = "revoke-token"
	AuditEnrollmentLimit           AuditAction = "enrollment-limit"
	AuditDeploymentCreate          AuditAction = "deployment-create"
	AuditDeploymentRetry           AuditAction = "deployment-retry"
	AuditDeploymentContinue        AuditAction = "deployment-continue"
	AuditDeploymentSkipFailedBatch AuditAction = "deployment-skip-failed-batch"
	AuditDeploymentAbandon         AuditAction = "deployment-abandon"
	AuditArtifactFetch             AuditAction = "artifact-fetch"
	AuditCatalogManifest           AuditAction = "catalog-manifest"
	AuditMachineProfile            AuditAction = "machine-profile"
	AuditMachineProfileAssign      AuditAction = "machine-profile-assign"
	AuditMachineChannel            AuditAction = "machine-channel"
	AuditMachineAssignedUser       AuditAction = "machine-assigned-user"
	AuditAgentSessionOpen          AuditAction = "agent-session-open"
	AuditAgentSessionClose         AuditAction = "agent-session-close"
	AuditMachineLifecycle          AuditAction = "machine-lifecycle"
	AuditMachineRename             AuditAction = "machine-rename"
	AuditMachineNotes              AuditAction = "machine-notes"
	AuditDiagnosticNoop            AuditAction = "diagnostic-noop"
	AuditDeviceSync                AuditAction = "device-sync"
	AuditTailnetPeerIgnore         AuditAction = "tailnet-peer-ignore"
	AuditRetentionPrune            AuditAction = "retention-prune"
	AuditRestoreDrill              AuditAction = "restore-drill"
	AuditVerifierRegister          AuditAction = "verifier-register"
	AuditVerifierRevoke            AuditAction = "verifier-revoke"
	AuditVerificationAssign        AuditAction = "verification-assign"
	AuditSettingPolicy             AuditAction = "setting-policy"
	AuditSettingAssign             AuditAction = "setting-assign"
	AuditCompliancePolicy          AuditAction = "compliance-policy"
	AuditComplianceAssign          AuditAction = "compliance-assign"
	AuditMaintenanceProfile        AuditAction = "maintenance-profile"
	AuditMaintenanceDryRun         AuditAction = "maintenance-dry-run"
	AuditMaintenanceCanary         AuditAction = "maintenance-canary"
	AuditMaintenanceContinue       AuditAction = "maintenance-continue"
	AuditMaintenanceAbandon        AuditAction = "maintenance-abandon"
	AuditOperatorDenied            AuditAction = "operator-denied"
)

// These prefixes are persisted classification markers for the legacy audit
// schema. They must stay at the beginning of Detail because Detail is bounded.
// A future structured outcome column can replace them without changing the UI.
const (
	OperatorTransportRejectionPrefix = "operator transport rejection；"
	OperatorIdempotencyReplayPrefix  = "idempotency replay；"
	OperatorBoundaryDenialPrefix     = "operator boundary denial；"
)

// AuditEntry 是一筆紀錄。
type AuditEntry struct {
	ID int64     `json:"id"`
	At time.Time `json:"at"`

	Action    AuditAction `json:"action"`
	MachineID string      `json:"machine_id,omitempty"`
	Subject   string      `json:"subject"` // 機器名字，或要連過去的位址
	Reason    string      `json:"reason,omitempty"`
	// Operator request correlation. Legacy/best-effort actions leave both empty.
	IdempotencyKey string `json:"idempotency_key,omitempty"`
	RequestDigest  string `json:"request_digest,omitempty"`

	// 「從哪裡按的」。⚠ 沒有 Actor 欄位，理由寫在 schema.sql 的註解裡。
	SourceAddr     string `json:"source_addr"`
	WhoNode        string `json:"who_node,omitempty"`
	WhoUser        string `json:"who_user,omitempty"`
	WhoUnavailable string `json:"who_unavailable,omitempty"`
	UserAgent      string `json:"user_agent,omitempty"`

	// Auth* records the verified operator principal when an authenticated
	// adapter supplies one. Empty means legacy/unavailable, never "anonymous".
	// Keep these separate from Who*: tailnet provenance and operator authority
	// answer different questions and must not overwrite each other.
	AuthSubject      string `json:"auth_subject,omitempty"`
	AuthNodeID       string `json:"auth_node_id,omitempty"`
	AuthCapability   string `json:"auth_capability,omitempty"`
	AuthMethod       string `json:"auth_method,omitempty"`
	AuthDecision     string `json:"auth_decision,omitempty"`
	BoundaryDecision string `json:"boundary_decision,omitempty"`
	SourceKind       string `json:"source_kind,omitempty"`

	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

// IsOperatorTransportRejection distinguishes a request whose bytes never had
// canonical domain meaning from old pre-operator-API machine-channel audit.
// The marker is written by the transport adapter and persisted in Detail, so
// old rows with empty correlation fields remain truthfully labelled legacy.
func (e AuditEntry) IsOperatorTransportRejection() bool {
	return IsCanonicalOperatorAction(e.Action) && strings.HasPrefix(e.Detail, OperatorTransportRejectionPrefix)
}

// IsOperatorReplay distinguishes a returned cached decision from a mutation
// that was actually attempted again. Both successful and rejected replays are
// valuable audit rows, but neither means the underlying action ran twice.
func (e AuditEntry) IsOperatorReplay() bool {
	return IsCanonicalOperatorAction(e.Action) && strings.HasPrefix(e.Detail, OperatorIdempotencyReplayPrefix)
}

// IsCanonicalOperatorAction is the single source of truth shared by store and
// operatorclient for actions whose replay and transport markers are canonical.
//
// ⚠ Keep this as an allowlist, never a denylist derived from allAuditActions.
// That catalog controls filtering and unknown_action; newly added observational
// actions must not silently become canonical mutations. Do not copy this list.
func IsCanonicalOperatorAction(action AuditAction) bool {
	return action == AuditMachineChannel || action == AuditMachineAssignedUser || action == AuditAgentSessionOpen || action == AuditAgentSessionClose || action == AuditMachineLifecycle || action == AuditMachineRename || action == AuditMachineNotes || action == AuditEnrollToken || action == AuditRevokeToken ||
		action == AuditEnrollmentLimit ||
		action == AuditDeploymentCreate || action == AuditDeploymentRetry ||
		action == AuditDeploymentContinue || action == AuditDeploymentSkipFailedBatch || action == AuditDeploymentAbandon ||
		action == AuditArtifactFetch || action == AuditCatalogManifest || action == AuditMachineProfile ||
		action == AuditMachineProfileAssign || action == AuditDiagnosticNoop || action == AuditDeviceSync || action == AuditTailnetPeerIgnore ||
		action == AuditRetentionPrune || action == AuditRestoreDrill ||
		action == AuditVerifierRegister || action == AuditVerifierRevoke ||
		action == AuditVerificationAssign ||
		action == AuditSettingPolicy || action == AuditSettingAssign ||
		action == AuditCompliancePolicy || action == AuditComplianceAssign ||
		action == AuditMaintenanceProfile || action == AuditMaintenanceDryRun ||
		action == AuditMaintenanceCanary || action == AuditMaintenanceContinue ||
		action == AuditMaintenanceAbandon
}

// auditMaxReason 是理由的長度上限。
// ⚠ 截斷要留下痕跡（見 truncAudit）—— 悄悄消失的內容比截斷更糟。
const auditMaxReason = 500

// OperatorDenialAuditLimit bounds only unauthenticated/request-boundary noise.
// Successful and domain-level control actions remain permanent. A network
// caller must not be able to turn fail-closed authorization into unbounded
// persistent writes simply by sending rejected requests forever.
const OperatorDenialAuditLimit = 1000

// RecordAudit 寫一筆。回傳 error 只是為了讓呼叫端能 log，**不准拿它中止動作**。
func (s *Store) RecordAudit(e AuditEntry) error {
	return s.recordAudit(boundExec{s: s, name: "record_audit"}, e)
}

// RecordOperatorDenial appends one sampled/aggregated boundary denial and
// keeps only the newest fixed-size denial ring. It never deletes ordinary
// control audit rows. The HTTP boundary also rate-limits calls into this
// method, so the ring bounds storage while the limiter bounds write I/O.
func (s *Store) RecordOperatorDenial(e AuditEntry) error {
	return s.recordOperatorDenialBounded(e, OperatorDenialAuditLimit)
}

func (s *Store) recordOperatorDenialBounded(e AuditEntry, limit int) error {
	if e.Action != AuditOperatorDenied {
		return fmt.Errorf("store: bounded operator denial requires action %q", AuditOperatorDenied)
	}
	if limit <= 0 {
		return errors.New("store: operator denial audit limit must be positive")
	}
	tx, err := s.beginWrite(context.Background(), "record_operator_denial_bounded")
	if err != nil {
		return fmt.Errorf("store: begin bounded operator denial audit: %w", err)
	}
	defer tx.Rollback()
	if err := s.recordAudit(tx, e); err != nil {
		return err
	}
	if _, err := tx.Exec(`
DELETE FROM audit_log
 WHERE action = ?
   AND audit_id NOT IN (
     SELECT audit_id FROM audit_log
      WHERE action = ?
      ORDER BY audit_id DESC
      LIMIT ?
   )`, string(AuditOperatorDenied), string(AuditOperatorDenied), limit); err != nil {
		return fmt.Errorf("store: bound operator denial audit: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit bounded operator denial audit: %w", err)
	}
	return nil
}

type auditExecer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

// recordAuditTx is used when the evidence and the authority-changing operator
// mutation must commit together. Ordinary observational audit still uses
// RecordAudit and remains non-gating.
func (s *Store) recordAuditTx(tx dbTx, e AuditEntry) error {
	return s.recordAudit(tx, e)
}

func (s *Store) recordAudit(exec auditExecer, e AuditEntry) error {
	if e.At.IsZero() {
		e.At = s.nowFn()
	}
	if e.SourceAddr == "" {
		// ⚠ 不准留空。空的 source_addr 跟「這個請求沒有來源」長得一樣，
		// 而後者在 HTTP 上不存在 —— 留空只可能是呼叫端漏傳。
		e.SourceAddr = "(呼叫端沒有傳來源位址)"
	}
	_, err := exec.Exec(`
INSERT INTO audit_log
  (at, action, machine_id, subject, reason, idempotency_key, request_digest,
	   source_addr, who_node, who_user, who_unavailable, user_agent,
	   auth_subject, auth_node_id, auth_capability, auth_method, auth_decision,
	   boundary_decision, source_kind, outcome, detail)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		fmtTime(e.At.UTC()), string(e.Action), nullStr(e.MachineID), truncAudit(e.Subject, auditMaxReason),
		nullStr(truncAudit(e.Reason, auditMaxReason)),
		nullStr(auditIdempotencyKey(e.IdempotencyKey)), nullStr(e.RequestDigest),
		e.SourceAddr, nullStr(e.WhoNode), nullStr(e.WhoUser), nullStr(e.WhoUnavailable),
		nullStr(truncAudit(e.UserAgent, 200)),
		nullStr(e.AuthSubject), nullStr(e.AuthNodeID), nullStr(e.AuthCapability),
		nullStr(e.AuthMethod), nullStr(e.AuthDecision), nullStr(e.BoundaryDecision), nullStr(e.SourceKind),
		outcomeOf(e.OK), nullStr(truncAudit(e.Detail, auditMaxReason)))
	if err != nil {
		return fmt.Errorf("store: audit: %w", err)
	}
	return nil
}

// auditIdempotencyKey keeps valid operator keys verbatim so they can join the
// idempotency ledger. An invalid oversized key is not an operator identity and
// must not turn one rejected HTTP header into an arbitrarily large audit row;
// retain a bounded, explicitly labelled hash instead of a misleading prefix.
func auditIdempotencyKey(key string) string {
	if len(key) <= 200 {
		return key
	}
	sum := sha256.Sum256([]byte(key))
	return fmt.Sprintf("invalid-key-sha256:%x（原值 %d bytes，未保存）", sum, len(key))
}

func outcomeOf(ok bool) string {
	if ok {
		return "ok"
	}
	return "failed"
}

func truncAudit(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…（已截斷）"
}

// AuditLimit 是畫面上最多列幾筆。
const AuditLimit = 200

// consoleOperatorDenialLimit reserves the console for useful domain history
// even when the network boundary is receiving sustained rejected traffic.
// This is a read-side presentation cap; it does not delete ledger rows.
const consoleOperatorDenialLimit = 50

// Audit 讀最近的紀錄。machineID 是空字串就讀全部。
//
// ⚠ 由新到舊。這一頁的第一個問題永遠是「剛剛誰動了什麼」。
func (s *Store) Audit(machineID string, limit int) ([]AuditEntry, error) {
	if limit <= 0 || limit > AuditLimit {
		limit = AuditLimit
	}
	q := `
SELECT audit_id, at, action, COALESCE(machine_id,''), subject, COALESCE(reason,''),
       COALESCE(idempotency_key,''), COALESCE(request_digest,''),
       source_addr, COALESCE(who_node,''), COALESCE(who_user,''),
       COALESCE(who_unavailable,''), COALESCE(user_agent,''),
	       COALESCE(auth_subject,''), COALESCE(auth_node_id,''), COALESCE(auth_capability,''),
	       COALESCE(auth_method,''), COALESCE(auth_decision,''), COALESCE(boundary_decision,''),
	       COALESCE(source_kind,''),
	       outcome, COALESCE(detail,'')
  FROM audit_log`
	args := []any{}
	if machineID != "" {
		q += ` WHERE machine_id = ?`
		args = append(args, machineID)
	}
	q += ` ORDER BY at DESC, audit_id DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.rdb.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: audit list: %w", err)
	}
	return scanAuditRows(rows)
}

// ConsoleAudit returns the rows used by the human-facing audit page. It keeps
// the same newest-first total cap as Audit, but admits at most the newest fixed
// number of operator-boundary denials. This prevents sampled network noise from
// hiding older domain/control rows without changing Audit or deleting evidence.
// machineID, when present, is applied independently to both candidate sets
// before either limit so another machine cannot consume the filtered budget.
func (s *Store) ConsoleAudit(machineID string) ([]AuditEntry, error) {
	regularWhere := "action <> ?"
	denialWhere := "action = ?"
	regularArgs := []any{string(AuditOperatorDenied)}
	denialArgs := []any{string(AuditOperatorDenied)}
	if machineID != "" {
		regularWhere += " AND machine_id = ?"
		denialWhere += " AND machine_id = ?"
		regularArgs = append(regularArgs, machineID)
		denialArgs = append(denialArgs, machineID)
	}
	regularArgs = append(regularArgs, AuditLimit)
	denialArgs = append(denialArgs, consoleOperatorDenialLimit)
	args := append(regularArgs, denialArgs...)
	args = append(args, AuditLimit)

	q := `
WITH regular_rows AS (
  SELECT * FROM audit_log
   WHERE ` + regularWhere + `
   ORDER BY at DESC, audit_id DESC
   LIMIT ?
), denial_rows AS (
  SELECT * FROM audit_log
   WHERE ` + denialWhere + `
   ORDER BY at DESC, audit_id DESC
   LIMIT ?
), console_rows AS (
  SELECT * FROM regular_rows
  UNION ALL
  SELECT * FROM denial_rows
)
SELECT audit_id, at, action, COALESCE(machine_id,''), subject, COALESCE(reason,''),
       COALESCE(idempotency_key,''), COALESCE(request_digest,''),
       source_addr, COALESCE(who_node,''), COALESCE(who_user,''),
       COALESCE(who_unavailable,''), COALESCE(user_agent,''),
       COALESCE(auth_subject,''), COALESCE(auth_node_id,''), COALESCE(auth_capability,''),
       COALESCE(auth_method,''), COALESCE(auth_decision,''), COALESCE(boundary_decision,''),
       COALESCE(source_kind,''), outcome, COALESCE(detail,'')
  FROM console_rows
 ORDER BY at DESC, audit_id DESC
 LIMIT ?`
	rows, err := s.rdb.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: console audit list: %w", err)
	}
	return scanAuditRows(rows)
}

func scanAuditRows(rows *sql.Rows) ([]AuditEntry, error) {
	defer rows.Close()

	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		var at, action, outcome string
		if err := rows.Scan(&e.ID, &at, &action, &e.MachineID, &e.Subject, &e.Reason,
			&e.IdempotencyKey, &e.RequestDigest,
			&e.SourceAddr, &e.WhoNode, &e.WhoUser, &e.WhoUnavailable, &e.UserAgent,
			&e.AuthSubject, &e.AuthNodeID, &e.AuthCapability, &e.AuthMethod,
			&e.AuthDecision, &e.BoundaryDecision, &e.SourceKind,
			&outcome, &e.Detail); err != nil {
			return nil, err
		}
		e.At, _ = time.Parse(time.RFC3339, at)
		e.Action, e.OK = AuditAction(action), outcome == "ok"
		out = append(out, e)
	}
	return out, rows.Err()
}

// UnretireMachine 讓一台機器回到分母。
//
// ⚠⚠ 這個函式存在的唯一理由是：**retire 必須可以反悔。**
//
// 一個按下去就回不來的按鈕，即使在有 operator auth 的 console 上也不能放 ——
// 授權不會消除人按錯的可能，而按錯之後那台機器就
// 從分母上消失了。而「機器不准從名冊上消失」正好是這整個產品存在的理由。
//
// ⚠ 它只清 retired_at。expected、created_at、以及退役期間的所有觀測
// 全部不動 —— 退役再回來不是重新出生。
func (s *Store) UnretireMachine(id string, now time.Time) error {
	return s.setMachineLifecycle(id, false, now)
}
