package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base32"
	"errors"
	"strings"
	"time"

	"github.com/teddashh/AI-Intune/internal/clientip"
	"github.com/teddashh/AI-Intune/internal/totp"
)

func (s *Store) MFAEnabled(id string) (bool, error) {
	var enabled sql.NullString
	err := s.rdb.QueryRow(`SELECT enabled_at FROM hub_account_mfa WHERE account_id=?`, id).Scan(&enabled)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return enabled.Valid, err
}

// TOTP secrets must remain readable for verification: DB read access = MFA bypass.
// Protect DB backups as sensitive credentials, just like the live database.
func (s *Store) BeginTOTPEnrollment(id string) (string, error) {
	secret, err := totp.NewSecret()
	if err != nil {
		return "", err
	}
	// Refreshes and concurrent setup visits keep the same pending secret.
	tx, err := s.beginWrite(context.Background(), "begin_totp")
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`INSERT INTO hub_account_mfa(account_id,totp_secret,pending_secret) VALUES (?,'',?) ON CONFLICT(account_id) DO UPDATE SET pending_secret=COALESCE(hub_account_mfa.pending_secret,excluded.pending_secret) WHERE enabled_at IS NULL`, id, secret)
	if err != nil {
		return "", err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return "", err
	}
	if n != 1 {
		return "", ErrAccountAuth
	}
	if err = tx.QueryRow(`SELECT pending_secret FROM hub_account_mfa WHERE account_id=?`, id).Scan(&secret); err != nil {
		return "", err
	}
	return secret, tx.Commit()
}
func (s *Store) ConfirmTOTP(id, code string, metadata ...AuditEntry) ([]string, error) {
	return s.ConfirmTOTPForSession(id, code, "", metadata...)
}

// ConfirmTOTPForSession promotes only the session that proved possession of the factor.
// Other password-only sessions and in-flight password logins must not gain access.
func (s *Store) ConfirmTOTPForSession(id, code, keepToken string, metadata ...AuditEntry) ([]string, error) {
	tx, err := s.beginWrite(context.Background(), "confirm_totp")
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var secret, username string
	err = tx.QueryRow(`SELECT m.pending_secret,a.username FROM hub_account_mfa m JOIN hub_accounts a USING(account_id) WHERE m.account_id=? AND m.enabled_at IS NULL`, id).Scan(&secret, &username)
	step, ok := totp.Verify(secret, code, s.nowFn())
	if err != nil {
		return nil, ErrAccountAuth
	}
	if !ok {
		if err = s.recordAuditTx(tx, accountAudit(AuditMFAFailed, HubAccount{AccountID: id, Username: username}, false, metadata...)); err != nil {
			return nil, err
		}
		if err = tx.Commit(); err != nil {
			return nil, err
		}
		return nil, ErrAccountAuth
	}
	codes, err := generateRecoveryCodesTx(tx, id)
	if err != nil {
		return nil, err
	}
	if _, err = tx.Exec(`UPDATE hub_account_mfa SET totp_secret=pending_secret,pending_secret=NULL,enabled_at=?,last_used_step=? WHERE account_id=?`, fmtTime(s.nowFn()), step, id); err != nil {
		return nil, err
	}
	if _, err = tx.Exec(`UPDATE hub_sessions SET revoked_at=? WHERE account_id=? AND session_hash<>? AND revoked_at IS NULL`, fmtTime(s.nowFn()), id, sessionHash(keepToken)); err != nil {
		return nil, err
	}
	if err = s.recordAuditTx(tx, accountAudit(AuditMFAEnabled, HubAccount{AccountID: id, Username: username}, true, metadata...)); err != nil {
		return nil, err
	}
	return codes, tx.Commit()
}
func (s *Store) VerifySecondFactor(id, clientIP, code string, metadata ...AuditEntry) error {
	clientIP = clientip.Key(clientIP)
	tx, err := s.beginWrite(context.Background(), "verify_second_factor")
	if err != nil {
		return err
	}
	defer tx.Rollback()
	err = s.verifySecondFactorTx(tx, id, clientIP, code, true, metadata...)
	if err != nil && !errors.Is(err, ErrAccountAuth) {
		return err
	}
	if commitErr := tx.Commit(); commitErr != nil {
		return commitErr
	}
	return err
}

// Both factor operations share replay protection, failure counters, and audit handling.
func (s *Store) verifySecondFactorTx(tx *writeTx, id, clientIP, code string, allowRecovery bool, metadata ...AuditEntry) error {
	var err error
	var a HubAccount
	a.AccountID = id
	var failed int
	var locked, disabled sql.NullString
	if err = tx.QueryRow(`SELECT username,disabled_at FROM hub_accounts WHERE account_id=?`, id).Scan(&a.Username, &disabled); err != nil {
		return err
	}
	now := s.nowFn()
	if _, err = tx.Exec(`DELETE FROM hub_login_failures WHERE last_failed_at<?`, fmtTime(now.Add(-24*time.Hour))); err != nil {
		return err
	}
	err = tx.QueryRow(`SELECT failed_attempts,locked_until FROM hub_login_failures WHERE account_id=? AND client_ip=?`, id, clientIP).Scan(&failed, &locked)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	until, _ := time.Parse(time.RFC3339, locked.String)
	blocked := disabled.Valid || now.Before(until)
	var secret string
	var last int64
	err = tx.QueryRow(`SELECT totp_secret,last_used_step FROM hub_account_mfa WHERE account_id=? AND enabled_at IS NOT NULL`, id).Scan(&secret, &last)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	step, valid := totp.Verify(secret, code, now)
	valid = valid && step > last && err == nil && !blocked
	action := AuditMFAFailed
	if valid {
		var res sql.Result
		res, err = tx.Exec(`UPDATE hub_account_mfa SET last_used_step=? WHERE account_id=? AND last_used_step<?`, step, id, step)
		if err == nil {
			var n int64
			n, err = res.RowsAffected()
			valid = n == 1
		}
	} else if allowRecovery && !blocked && secret != "" {
		var res sql.Result
		res, err = tx.Exec(`UPDATE hub_account_recovery_codes SET used_at=? WHERE account_id=? AND code_hash=? AND used_at IS NULL`, fmtTime(now), id, sessionHash(strings.ToLower(strings.TrimSpace(code))))
		if err == nil {
			n, _ := res.RowsAffected()
			valid = n == 1
			if valid {
				action = AuditRecoveryCodeUsed
			}
		}
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if valid {
		_, err = tx.Exec(`DELETE FROM hub_login_failures WHERE account_id=? AND client_ip=?`, id, clientIP)
	} else if !blocked {
		if locked.Valid {
			failed = 0
		}
		failed++
		var lock any
		if failed >= 5 {
			lock = fmtTime(now.Add(15 * time.Minute))
			if err = s.recordAuditTx(tx, accountAudit(AuditHubLockout, a, false, metadata...)); err != nil {
				return err
			}
		}
		_, err = tx.Exec(`INSERT INTO hub_login_failures(account_id,client_ip,failed_attempts,locked_until,last_failed_at) VALUES(?,?,?,?,?) ON CONFLICT(account_id,client_ip) DO UPDATE SET failed_attempts=excluded.failed_attempts,locked_until=excluded.locked_until,last_failed_at=excluded.last_failed_at`, id, clientIP, failed, lock, fmtTime(now))
	}
	if err != nil {
		return err
	}
	if !valid || action == AuditRecoveryCodeUsed {
		if err = s.recordAuditTx(tx, accountAudit(action, a, valid, metadata...)); err != nil {
			return err
		}
	}
	if !valid {
		if err = s.auditHubGuessingTx(tx, a, metadata...); err != nil {
			return err
		}
	}
	if !valid {
		return ErrAccountAuth
	}
	return nil
}

func (s *Store) DisableMFA(id string, metadata ...AuditEntry) error {
	tx, err := s.beginWrite(context.Background(), "disable_mfa")
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var a HubAccount
	a.AccountID = id
	if err = tx.QueryRow(`SELECT username FROM hub_accounts WHERE account_id=?`, id).Scan(&a.Username); err != nil {
		return err
	}
	if _, err = tx.Exec(`DELETE FROM hub_account_recovery_codes WHERE account_id=?`, id); err != nil {
		return err
	}
	if _, err = tx.Exec(`DELETE FROM hub_account_mfa WHERE account_id=?`, id); err != nil {
		return err
	}
	if err = s.recordAuditTx(tx, accountAudit(AuditMFADisabled, a, true, metadata...)); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) DisableAdminMFA(username string) error {
	var id string
	if err := s.rdb.QueryRow(`SELECT account_id FROM hub_accounts WHERE username=?`, username).Scan(&id); err != nil {
		return err
	}
	return s.DisableMFA(id)
}

func generateRecoveryCodesTx(tx dbTx, id string) ([]string, error) {
	var err error
	codes := make([]string, 10)
	for i := range codes {
		b := make([]byte, 8)
		if _, err = rand.Read(b); err != nil {
			return nil, err
		}
		raw := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b)
		codes[i] = strings.ToLower(raw[:4] + "-" + raw[4:8] + "-" + raw[8:12])
		if _, err = tx.Exec(`INSERT INTO hub_account_recovery_codes(account_id,code_hash) VALUES (?,?)`, id, sessionHash(codes[i])); err != nil {
			return nil, err
		}
	}
	return codes, nil
}

// RegenerateRecoveryCodes accepts only an unused authenticator step.
func (s *Store) RegenerateRecoveryCodes(id, clientIP, code string, metadata ...AuditEntry) ([]string, error) {
	tx, err := s.beginWrite(context.Background(), "regenerate_recovery_codes")
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err = s.verifySecondFactorTx(tx, id, clientip.Key(clientIP), code, false, metadata...); err != nil {
		if errors.Is(err, ErrAccountAuth) {
			if e := tx.Commit(); e != nil {
				return nil, e
			}
		}
		return nil, err
	}
	codes, err := s.replaceRecoveryCodesTx(tx, id, metadata...)
	if err != nil {
		return nil, err
	}
	return codes, tx.Commit()
}

func (s *Store) replaceRecoveryCodesTx(tx dbTx, id string, metadata ...AuditEntry) ([]string, error) {
	var a HubAccount
	a.AccountID = id
	if err := tx.QueryRow(`SELECT a.username FROM hub_accounts a JOIN hub_account_mfa m USING(account_id) WHERE a.account_id=? AND a.disabled_at IS NULL AND m.enabled_at IS NOT NULL`, id).Scan(&a.Username); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrAccountAuth
		}
		return nil, err
	}
	if _, err := tx.Exec(`DELETE FROM hub_account_recovery_codes WHERE account_id=?`, id); err != nil {
		return nil, err
	}
	codes, err := generateRecoveryCodesTx(tx, id)
	if err != nil {
		return nil, err
	}
	if err = s.recordAuditTx(tx, accountAudit(AuditRecoveryCodesRegenerated, a, true, metadata...)); err != nil {
		return nil, err
	}
	return codes, nil
}

// RegenerateAdminRecoveryCodes is host recovery and does not require a factor.
func (s *Store) RegenerateAdminRecoveryCodes(username string) ([]string, error) {
	tx, err := s.beginWrite(context.Background(), "regenerate_admin_recovery_codes")
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var id string
	if err = tx.QueryRow(`SELECT account_id FROM hub_accounts WHERE username=?`, username).Scan(&id); err != nil {
		return nil, err
	}
	codes, err := s.replaceRecoveryCodesTx(tx, id)
	if err != nil {
		return nil, err
	}
	return codes, tx.Commit()
}
