package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"golang.org/x/crypto/argon2"

	"github.com/teddashh/AI-Intune/internal/clientip"
)

// Tests in this package lower these costs; production uses 64 MiB, three passes.
var argonMemory uint32 = 64 * 1024
var argonTime uint32 = 3
var argonThreads uint8 = 2
var usernamePattern = regexp.MustCompile(`^[a-z0-9._-]{3,64}$`)
var ErrAccountAuth = errors.New("invalid username or password")
var ErrSessionAuth = errors.New("invalid or expired session")
var ErrAdminExists = errors.New("admin already exists")

type HubAccount struct {
	AccountID    string
	Username     string
	passwordHash string // also guards session issuance against a concurrent password reset
	mfaSecret    string // snapshots the factor at password verification; enrollment invalidates it
}

func passwordHash(password string) (string, error) {
	if len(password) < 12 || len(password) > 256 {
		return "", errors.New("password must be 12–256 bytes")
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	hash := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, 32)
	return fmt.Sprintf("$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s", argonMemory, argonTime, argonThreads, base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(hash)), nil
}

func checkPassword(encoded, password string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" || parts[2] != "v=19" {
		return false
	}
	var m, t uint32
	var p uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil || m < 8*uint32(p) || m > 65536 || t == 0 || t > 3 || p == 0 || p > 2 {
		return false
	}
	salt, e1 := base64.RawStdEncoding.DecodeString(parts[4])
	want, e2 := base64.RawStdEncoding.DecodeString(parts[5])
	if e1 != nil || e2 != nil || len(salt) != 16 || len(want) != 32 {
		return false
	}
	got := argon2.IDKey([]byte(password), salt, t, m, p, 32)
	return subtle.ConstantTimeCompare(got, want) == 1
}

func (s *Store) CountAccounts() (int, error) {
	var n int
	err := s.rdb.QueryRow(`SELECT count(*) FROM hub_accounts`).Scan(&n)
	return n, err
}

func accountAudit(action AuditAction, a HubAccount, ok bool, metadata ...AuditEntry) AuditEntry {
	subject := ""
	if a.AccountID != "" {
		subject = "local-user:" + a.AccountID
	}
	entry := AuditEntry{Action: action, Subject: a.Username, AuthSubject: subject, AuthMethod: "local-account-session", OK: ok}
	if len(metadata) > 0 {
		entry.SourceAddr = metadata[0].SourceAddr
		entry.UserAgent = metadata[0].UserAgent
	}
	return entry
}

func (s *Store) CreateFirstAdmin(username, password string, metadata ...AuditEntry) (HubAccount, error) {
	a := HubAccount{Username: username}
	if !usernamePattern.MatchString(username) {
		return a, errors.New("username must be 3–64 lowercase letters, digits, dots, underscores or hyphens")
	}
	hash, err := passwordHash(password)
	if err != nil {
		return a, err
	}
	id := make([]byte, 16)
	if _, err = rand.Read(id); err != nil {
		return a, err
	}
	id[6] = (id[6] & 15) | 64
	id[8] = (id[8] & 63) | 128
	a.AccountID = fmt.Sprintf("%x-%x-%x-%x-%x", id[:4], id[4:6], id[6:8], id[8:10], id[10:])
	a.passwordHash = hash
	tx, err := s.beginWrite(context.Background(), "create_first_admin")
	if err != nil {
		return HubAccount{}, err
	}
	defer tx.Rollback()
	// The predicate and insert are a single SQLite write, including across processes.
	res, err := tx.Exec(`INSERT INTO hub_accounts(account_id,username,password_hash,created_at) SELECT ?,?,?,? WHERE NOT EXISTS (SELECT 1 FROM hub_accounts)`, a.AccountID, username, hash, fmtTime(s.nowFn()))
	if err != nil {
		return HubAccount{}, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return HubAccount{}, err
	}
	if n != 1 {
		return HubAccount{}, ErrAdminExists
	}
	if err = s.recordAuditTx(tx, accountAudit(AuditHubSetup, a, true, metadata...)); err != nil {
		return HubAccount{}, err
	}
	return a, tx.Commit()
}

func (s *Store) VerifyPassword(username, password, clientIP string, metadata ...AuditEntry) (HubAccount, error) {
	clientIP = clientip.Key(clientIP)
	var a HubAccount
	err := s.rdb.QueryRow(`SELECT account_id,username,password_hash FROM hub_accounts WHERE username=?`, username).Scan(&a.AccountID, &a.Username, &a.passwordHash)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return a, err
	}
	validLength := len(password) <= 256
	if !validLength {
		password = ""
	}
	valid := false
	if errors.Is(err, sql.ErrNoRows) {
		// Same KDF cost for unknown users; no persistent dummy secret is needed.
		dummy := argon2.IDKey([]byte(password), make([]byte, 16), argonTime, argonMemory, argonThreads, 32)
		_ = subtle.ConstantTimeCompare(dummy, make([]byte, 32))
	} else {
		valid = checkPassword(a.passwordHash, password) && validLength
	}
	tx, err := s.beginWrite(context.Background(), "verify_hub_password")
	if err != nil {
		return HubAccount{}, err
	}
	defer tx.Rollback()
	now := s.nowFn()
	// Legacy hub_accounts.failed_attempts/locked_until remain for compatibility only.
	if _, err = tx.Exec(`DELETE FROM hub_login_failures WHERE last_failed_at<?`, fmtTime(now.Add(-24*time.Hour))); err != nil {
		return HubAccount{}, err
	}
	var disabled sql.NullString
	var currentHash string
	err = tx.QueryRow(`SELECT password_hash,disabled_at,COALESCE((SELECT totp_secret FROM hub_account_mfa WHERE account_id=hub_accounts.account_id AND enabled_at IS NOT NULL),'') FROM hub_accounts WHERE account_id=?`, a.AccountID).Scan(&currentHash, &disabled, &a.mfaSecret)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return HubAccount{}, err
	}
	blocked := disabled.Valid || err != nil || currentHash != a.passwordHash
	failed := 0
	var locked sql.NullString
	if a.AccountID != "" {
		e := tx.QueryRow(`SELECT failed_attempts,locked_until FROM hub_login_failures WHERE account_id=? AND client_ip=?`, a.AccountID, clientIP).Scan(&failed, &locked)
		if e != nil && !errors.Is(e, sql.ErrNoRows) {
			return HubAccount{}, e
		}
	}
	until, _ := time.Parse(time.RFC3339, locked.String)
	blocked = blocked || now.Before(until)
	action := AuditAction(AuditHubLoginFailed)
	if valid && !blocked {
		// With MFA, completion of the second factor clears login failures.
		_, err = tx.Exec(`DELETE FROM hub_login_failures WHERE account_id=? AND client_ip=? AND NOT EXISTS (SELECT 1 FROM hub_account_mfa WHERE account_id=? AND enabled_at IS NOT NULL)`, a.AccountID, clientIP, a.AccountID)
		action = AuditHubLoginOK
	} else if !blocked {
		if locked.Valid {
			failed = 0
		}
		failed++
		var lock any
		if failed >= 5 {
			lock = fmtTime(now.Add(15 * time.Minute))
			action = AuditHubLockout
		}
		_, err = tx.Exec(`INSERT INTO hub_login_failures(account_id,client_ip,failed_attempts,locked_until,last_failed_at) VALUES(?,?,?,?,?) ON CONFLICT(account_id,client_ip) DO UPDATE SET failed_attempts=excluded.failed_attempts,locked_until=excluded.locked_until,last_failed_at=excluded.last_failed_at`, a.AccountID, clientIP, failed, lock, fmtTime(now))
	} else {
		err = nil
	}
	if err != nil {
		return HubAccount{}, err
	}
	if err = s.recordAuditTx(tx, accountAudit(action, a, valid && !blocked, metadata...)); err != nil {
		return HubAccount{}, err
	}
	if action == AuditHubLockout {
		if err = s.recordAuditTx(tx, accountAudit(AuditHubLoginFailed, a, false, metadata...)); err != nil {
			return HubAccount{}, err
		}
	}
	if a.AccountID != "" && (!valid || blocked) {
		if err = s.auditHubGuessingTx(tx, a, metadata...); err != nil {
			return HubAccount{}, err
		}
	}
	if err = tx.Commit(); err != nil {
		return HubAccount{}, err
	}
	if !valid || blocked {
		return HubAccount{}, ErrAccountAuth
	}
	return a, nil
}

func sessionHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return fmt.Sprintf("%x", sum)
}

func (s *Store) CreateSession(a HubAccount, sourceAddr, userAgent string) (string, error) {
	token, err := newToken()
	if err != nil {
		return "", err
	}
	now := s.nowFn()
	res, err := s.execWrite(context.Background(), "create_hub_session", `INSERT INTO hub_sessions(session_hash,account_id,created_at,last_seen_at,idle_expires_at,absolute_expires_at,source_addr,user_agent) SELECT ?,account_id,?,?,?,?,?,? FROM hub_accounts WHERE account_id=? AND password_hash=? AND disabled_at IS NULL AND COALESCE((SELECT totp_secret FROM hub_account_mfa WHERE account_id=hub_accounts.account_id AND enabled_at IS NOT NULL),'')=?`, sessionHash(token), fmtTime(now), fmtTime(now), fmtTime(now.Add(12*time.Hour)), fmtTime(now.Add(7*24*time.Hour)), sourceAddr, truncAudit(userAgent, 200), a.AccountID, a.passwordHash, a.mfaSecret)
	if err != nil {
		return "", err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return "", ErrSessionAuth
	}
	return token, nil
}

func (s *Store) LookupSession(token string) (HubAccount, error) {
	var a HubAccount
	if len(token) != 43 {
		return a, ErrSessionAuth
	}
	now := s.nowFn()
	var seen, idle, absolute string
	err := s.rdb.QueryRow(`SELECT a.account_id,a.username,s.last_seen_at,s.idle_expires_at,s.absolute_expires_at FROM hub_sessions s JOIN hub_accounts a ON a.account_id=s.account_id WHERE session_hash=? AND revoked_at IS NULL AND disabled_at IS NULL`, sessionHash(token)).Scan(&a.AccountID, &a.Username, &seen, &idle, &absolute)
	if errors.Is(err, sql.ErrNoRows) {
		return a, ErrSessionAuth
	}
	if err != nil {
		return a, err
	}
	last, e1 := time.Parse(time.RFC3339, seen)
	ie, e2 := time.Parse(time.RFC3339, idle)
	ae, e3 := time.Parse(time.RFC3339, absolute)
	if e1 != nil || e2 != nil || e3 != nil || !now.Before(ie) || !now.Before(ae) {
		return HubAccount{}, ErrSessionAuth
	}
	if now.Sub(last) >= time.Minute {
		expiry := now.Add(12 * time.Hour)
		if expiry.After(ae) {
			expiry = ae
		}
		_, err = s.execWrite(context.Background(), "slide_hub_session", `UPDATE hub_sessions SET last_seen_at=?,idle_expires_at=? WHERE session_hash=? AND last_seen_at=? AND revoked_at IS NULL`, fmtTime(now), fmtTime(expiry), sessionHash(token), seen)
	}
	return a, err
}

func (s *Store) RevokeSession(token string, metadata ...AuditEntry) error {
	tx, err := s.beginWrite(context.Background(), "revoke_hub_session")
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var a HubAccount
	err = tx.QueryRow(`SELECT a.account_id,a.username FROM hub_sessions s JOIN hub_accounts a ON a.account_id=s.account_id WHERE session_hash=? AND revoked_at IS NULL`, sessionHash(token)).Scan(&a.AccountID, &a.Username)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE hub_sessions SET revoked_at=? WHERE session_hash=?`, fmtTime(s.nowFn()), sessionHash(token)); err != nil {
		return err
	}
	if err = s.recordAuditTx(tx, accountAudit(AuditHubLogout, a, true, metadata...)); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) RevokeAllSessionsForAccount(accountID string) error {
	_, err := s.execWrite(context.Background(), "revoke_hub_sessions", `UPDATE hub_sessions SET revoked_at=? WHERE account_id=? AND revoked_at IS NULL`, fmtTime(s.nowFn()), accountID)
	return err
}

// ChangePassword is a trusted caller API. keepToken may preserve the caller's
// own session; an empty value (host recovery) revokes every session.
func (s *Store) ChangePassword(accountID, password, keepToken string) error {
	hash, err := passwordHash(password)
	if err != nil {
		return err
	}
	tx, err := s.beginWrite(context.Background(), "change_hub_password")
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`UPDATE hub_accounts SET password_hash=? WHERE account_id=?`, hash, accountID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return ErrAccountAuth
	}
	if _, err = tx.Exec(`DELETE FROM hub_login_failures WHERE account_id=?`, accountID); err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE hub_sessions SET revoked_at=? WHERE account_id=? AND session_hash<>? AND revoked_at IS NULL`, fmtTime(s.nowFn()), accountID, sessionHash(keepToken)); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ResetAdminPassword(username, password string) error {
	var id string
	if err := s.rdb.QueryRow(`SELECT account_id FROM hub_accounts WHERE username=?`, username).Scan(&id); err != nil {
		return err
	}
	return s.ChangePassword(id, password, "")
}

// LocalAccountLogin resolves only active local accounts for machine assignment.
func (s *Store) LocalAccountLogin(accountID string) (string, error) {
	var login string
	err := s.rdb.QueryRow(`SELECT username FROM hub_accounts WHERE account_id=? AND disabled_at IS NULL`, accountID).Scan(&login)
	return login, err
}

// Password and second-factor failures contribute to one rolling audit signal.
// This observes distributed guessing without locking the account globally.
func (s *Store) auditHubGuessingTx(tx *writeTx, a HubAccount, metadata ...AuditEntry) error {
	var failures, signals int
	since := fmtTime(s.nowFn().Add(-time.Hour))
	subject := "local-user:" + a.AccountID
	if err := tx.QueryRow(`SELECT count(*) FROM audit_log WHERE auth_subject=? AND action IN (?,?) AND at>?`, subject, AuditHubLoginFailed, AuditMFAFailed, since).Scan(&failures); err != nil {
		return err
	}
	if failures < 50 {
		return nil
	}
	if err := tx.QueryRow(`SELECT count(*) FROM audit_log WHERE auth_subject=? AND action=? AND at>?`, subject, AuditHubGuessing, since).Scan(&signals); err != nil {
		return err
	}
	if signals != 0 {
		return nil
	}
	entry := accountAudit(AuditHubGuessing, a, false, metadata...)
	entry.Detail = "hub login: distributed password or second-factor guessing suspected"
	return s.recordAuditTx(tx, entry)
}
