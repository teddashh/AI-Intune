package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"unicode"

	"github.com/teddashh/AI-Intune/internal/clientip"
)

var ErrUserUpdate = errors.New("unable to update user")

func NormalizeEmail(email string) (string, error) {
	email = strings.TrimSpace(email)
	for i := range email {
		if email[i] >= 0x80 {
			return "", errors.New("email must be ASCII; use the punycode A-label form for IDN domains")
		}
	}
	email = strings.ToLower(email)
	if email == "" {
		return "", nil
	}
	if len(email) > 254 || strings.Count(email, "@") != 1 {
		return "", ErrUserUpdate
	}
	// ASCII only: no Unicode case-folding, normalization, or confusable look-alikes.
	for _, r := range email {
		if r > unicode.MaxASCII || unicode.IsSpace(r) || unicode.IsControl(r) {
			return "", ErrUserUpdate
		}
	}
	parts := strings.Split(email, "@")
	a, err := mail.ParseAddress(email)
	if err != nil || a.Name != "" || a.Address != email || parts[0] == "" || parts[1] == "" || !strings.Contains(parts[1], ".") {
		return "", ErrUserUpdate
	}
	return email, nil
}

func (s *Store) ListHubUsers() ([]HubAccount, error) {
	rows, err := s.rdb.Query(`SELECT account_id,username,COALESCE(email,''),role,created_at,disabled_at IS NOT NULL,EXISTS(SELECT 1 FROM hub_account_mfa m WHERE m.account_id=a.account_id AND enabled_at IS NOT NULL) FROM hub_accounts a ORDER BY username`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var users []HubAccount
	for rows.Next() {
		var a HubAccount
		if err = rows.Scan(&a.AccountID, &a.Username, &a.Email, &a.Role, &a.CreatedAt, &a.Disabled, &a.MFAEnrolled); err != nil {
			return nil, err
		}
		users = append(users, a)
	}
	return users, rows.Err()
}

// MutateHubUser is a trusted host/admin API. Authorization and reauthentication
// belong to the caller; all state changes and their success audit are atomic.
func (s *Store) MutateHubUser(action, username, value, password, actorID string, metadata ...AuditEntry) error {
	username = strings.ToLower(strings.TrimSpace(username))
	entry := AuditEntry{Action: AuditAction("hub-user-" + action), Subject: username, AuthSubject: "local-user:" + actorID, AuthMethod: "local-account-session"}
	if actorID == "" {
		entry.AuthSubject = "host-cli"
		entry.AuthMethod = "host-cli"
	}
	if len(metadata) > 0 {
		entry.SourceAddr = metadata[0].SourceAddr
		entry.UserAgent = metadata[0].UserAgent
	}
	fail := func(err error) error { entry.OK = false; _ = s.RecordAudit(entry); return err }
	var email string
	var err error
	if action == "created" || action == "email-changed" {
		email, err = NormalizeEmail(value)
		if err != nil {
			return fail(err)
		}
	}
	var hash string
	if action == "created" {
		if !usernamePattern.MatchString(username) {
			return fail(ErrUserUpdate)
		}
		hash, err = passwordHash(password)
		if err != nil {
			return fail(err)
		}
	}
	if action == "renamed" {
		value = strings.ToLower(strings.TrimSpace(value))
		if !usernamePattern.MatchString(value) {
			return fail(ErrUserUpdate)
		}
	}
	tx, err := s.beginWrite(context.Background(), "mutate_hub_user")
	if err != nil {
		return fail(err)
	}
	defer tx.Rollback()
	// Roll back before recording failures using the writer connection.
	abort := func(e error) error { _ = tx.Rollback(); return fail(e) }
	if actorID != "" {
		var active int
		if err = tx.QueryRow(`SELECT count(*) FROM hub_accounts WHERE account_id=? AND disabled_at IS NULL AND role='admin'`, actorID).Scan(&active); err != nil {
			return abort(err)
		}
		if active != 1 {
			return abort(ErrUserUpdate)
		}
	}
	var target string
	if action != "created" {
		if err = tx.QueryRow(`SELECT account_id FROM hub_accounts WHERE username=?`, username).Scan(&target); err != nil {
			return abort(fmt.Errorf("account not found: %w", err))
		}
	}
	var res sql.Result
	switch action {
	case "created":
		token, e := newHubAccountID()
		if e != nil {
			return abort(e)
		}
		target = token
		res, err = tx.Exec(`INSERT INTO hub_accounts(account_id,username,email,password_hash,created_at) VALUES(?,?,?,?,?)`, target, username, nullableEmail(email), hash, fmtTime(s.nowFn()))
	case "disabled":
		if target == actorID {
			return abort(ErrUserUpdate)
		}
		var n int
		if err = tx.QueryRow(`SELECT count(*) FROM hub_accounts WHERE disabled_at IS NULL AND role='admin' AND account_id<>?`, target).Scan(&n); err != nil {
			return abort(err)
		}
		if n == 0 {
			return abort(ErrUserUpdate)
		}
		res, err = tx.Exec(`UPDATE hub_accounts SET disabled_at=?,auth_generation=auth_generation+1 WHERE account_id=?`, fmtTime(s.nowFn()), target)
		if err == nil {
			_, err = tx.Exec(`UPDATE hub_sessions SET revoked_at=? WHERE account_id=? AND revoked_at IS NULL`, fmtTime(s.nowFn()), target)
		}
	case "enabled":
		res, err = tx.Exec(`UPDATE hub_accounts SET disabled_at=NULL,auth_generation=auth_generation+1 WHERE account_id=?`, target)
	case "email-changed":
		res, err = tx.Exec(`UPDATE hub_accounts SET email=? WHERE account_id=?`, nullableEmail(email), target)
	case "renamed":
		res, err = tx.Exec(`UPDATE hub_accounts SET username=? WHERE account_id=?`, value, target)
		entry.Detail = "username: " + username + " -> " + value
	default:
		return abort(ErrUserUpdate)
	}
	if err != nil {
		return abort(ErrUserUpdate)
	}
	n, err := res.RowsAffected()
	if err != nil || n != 1 {
		return abort(ErrUserUpdate)
	}
	entry.OK = true
	if err = s.recordAuditTx(tx, entry); err != nil {
		return abort(err)
	}
	return tx.Commit()
}
func nullableEmail(email string) any {
	if email == "" {
		return nil
	}
	return email
}

func (s *Store) VerifyAdminTOTP(id, ip, code string, metadata ...AuditEntry) error {
	tx, err := s.beginWrite(context.Background(), "verify_admin_totp")
	if err != nil {
		return err
	}
	defer tx.Rollback()
	err = s.verifySecondFactorTx(tx, id, clientip.Key(ip), code, false, metadata...)
	if err != nil && !errors.Is(err, ErrAccountAuth) {
		return err
	}
	if e := tx.Commit(); e != nil {
		return e
	}
	return err
}
