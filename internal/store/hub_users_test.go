package store

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/totp"
)

func TestHubUsersLifecycle(t *testing.T) {
	fastAccountHashes(t)
	s := newTestStore(t)
	alice, err := s.CreateFirstAdmin("alice", testAdminPassword)
	if err != nil {
		t.Fatal(err)
	}
	mutate := func(action, user, value, actor string) error {
		return s.MutateHubUser(action, user, value, testAdminPassword, actor)
	}
	if err = mutate("created", "  BoB  ", " BOB@Example.com ", alice.AccountID); err != nil {
		t.Fatal(err)
	}
	users, err := s.ListHubUsers()
	if err != nil || len(users) != 2 || users[1].Email != "bob@example.com" || users[1].Role != "admin" || users[1].MFAEnrolled {
		t.Fatal(users, err)
	}
	for _, id := range []string{"bob", " BoB@EXAMPLE.COM "} {
		if _, err = s.VerifyPassword(id, testAdminPassword, "192.0.2.1"); err != nil {
			t.Fatal(id, err)
		}
	}
	if err = mutate("created", "carol", "bob@example.com", alice.AccountID); err == nil {
		t.Fatal("duplicate email accepted")
	}
	if err = mutate("email-changed", "alice", "bob@example.com", alice.AccountID); err == nil {
		t.Fatal("duplicate email update accepted")
	}
	bob, err := s.VerifyPassword("bob", testAdminPassword, "192.0.2.1")
	if err != nil {
		t.Fatal(err)
	}
	token, err := s.CreateSession(bob, "192.0.2.1", "")
	if err != nil {
		t.Fatal(err)
	}
	if err = mutate("renamed", "bob", "carol", alice.AccountID); err != nil {
		t.Fatal(err)
	}
	if a, err := s.LookupSession(token); err != nil || a.Username != "carol" {
		t.Fatal(a, err)
	}
	if _, err = s.VerifyPassword("bob", testAdminPassword, "192.0.2.1"); !errors.Is(err, ErrAccountAuth) {
		t.Fatal(err)
	}
	if _, err = s.VerifyPassword("carol", testAdminPassword, "192.0.2.1"); err != nil {
		t.Fatal(err)
	}
	if err = mutate("disabled", "alice", "", alice.AccountID); err == nil {
		t.Fatal("disabled self")
	}
	if err = mutate("disabled", "carol", "", alice.AccountID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.LookupSession(token); !errors.Is(err, ErrSessionAuth) {
		t.Fatal(err)
	}
	if _, err = s.VerifyPassword("bob@example.com", testAdminPassword, "192.0.2.1"); !errors.Is(err, ErrAccountAuth) {
		t.Fatal(err)
	}
	if _, err = s.CreateSession(bob, "192.0.2.1", ""); !errors.Is(err, ErrSessionAuth) {
		t.Fatal(err)
	}
	if err = mutate("disabled", "alice", "", ""); err == nil {
		t.Fatal("disabled last admin")
	}
	if err = mutate("enabled", "carol", "", alice.AccountID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.VerifyPassword("carol", testAdminPassword, "192.0.2.1"); err != nil {
		t.Fatal(err)
	}
	if err = mutate("email-changed", "carol", "", alice.AccountID); err != nil {
		t.Fatal(err)
	}
	if err = mutate("email-changed", "alice", "bob@example.com", alice.AccountID); err != nil {
		t.Fatal(err)
	}
}
func TestEmailValidation(t *testing.T) {
	for _, email := range []string{"alice", "alice@localhost", "@example.com", "alice@@example.com", "Alice <alice@example.com>", "alice @example.com", "alice\n@example.com", "al\u0131ce@example.com", "\u212Alice@example.com", "alice@exam\xffple.com", "alice@ex\u00e4mple.com", "alice@exam\u0440le.com", strings.Repeat("a", 243) + "@example.com"} {
		if _, err := NormalizeEmail(email); err == nil {
			t.Fatalf("accepted %q", email)
		}
	}
	for _, email := range []string{"", " alice@example.com ", "ALICE@EXAMPLE.COM", "alice@xn--exmple-cua.com"} {
		if _, err := NormalizeEmail(email); err != nil {
			t.Fatal(email, err)
		}
	}
}
func TestHubUsersLegacyMigration(t *testing.T) {
	fastAccountHashes(t)
	path := filepath.Join(t.TempDir(), "hub.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	hash, err := passwordHash(testAdminPassword)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE hub_accounts(account_id TEXT PRIMARY KEY,username TEXT UNIQUE NOT NULL CHECK(length(username) BETWEEN 3 AND 64 AND username NOT GLOB '*[^a-z0-9._-]*'),password_hash TEXT NOT NULL,created_at TEXT NOT NULL,failed_attempts INTEGER NOT NULL DEFAULT 0,locked_until TEXT,disabled_at TEXT); INSERT INTO hub_accounts(account_id,username,password_hash,created_at) VALUES('alice-id','alice',?,'2026-01-01T00:00:00Z')`, hash)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	for range 2 {
		s, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		var email sql.NullString
		var role, currentHash string
		var generation int64
		if err = s.rdb.QueryRow(`SELECT email,role,password_hash,auth_generation FROM hub_accounts WHERE username='alice'`).Scan(&email, &role, &currentHash, &generation); err != nil || email.Valid || role != "admin" || currentHash != hash || generation != 0 {
			t.Fatal(email, role, err)
		}
		var n int
		if err = s.rdb.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='index' AND name='hub_accounts_email' AND sql LIKE '%WHERE email IS NOT NULL%'`).Scan(&n); err != nil || n != 1 {
			t.Fatal(n, err)
		}
		if _, err = s.VerifyPassword("alice", testAdminPassword, "192.0.2.1"); err != nil {
			t.Fatal(err)
		}
		s.Close()
	}
}

func TestHubUsersAuditAndConcurrentLastAdmin(t *testing.T) {
	fastAccountHashes(t)
	s := newTestStore(t)
	alice, err := s.CreateFirstAdmin("alice", testAdminPassword)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.MutateHubUser("created", "bob", "bob@example.com", testAdminPassword, alice.AccountID); err != nil {
		t.Fatal(err)
	}
	if err = s.MutateHubUser("renamed", "bob", "carol", "", alice.AccountID); err != nil {
		t.Fatal(err)
	}
	var actor, subject, detail string
	if err = s.rdb.QueryRow(`SELECT auth_subject,subject,detail FROM audit_log WHERE action=?`, AuditHubUserRenamed).Scan(&actor, &subject, &detail); err != nil || actor != "local-user:"+alice.AccountID || subject != "bob" || !strings.Contains(detail, "bob -> carol") {
		t.Fatal(actor, subject, detail, err)
	}
	done := make(chan error, 2)
	for _, name := range []string{"alice", "carol"} {
		go func(name string) { done <- s.MutateHubUser("disabled", name, "", "", "") }(name)
	}
	successes := 0
	for range 2 {
		if <-done == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatal("concurrent last-admin guard", successes)
	}
	var active, failures int
	if err = s.rdb.QueryRow(`SELECT count(*) FROM hub_accounts WHERE disabled_at IS NULL AND role='admin'`).Scan(&active); err != nil || active != 1 {
		t.Fatal(active, err)
	}
	if err = s.rdb.QueryRow(`SELECT count(*) FROM audit_log WHERE action=? AND outcome='failed'`, AuditHubUserDisabled).Scan(&failures); err != nil || failures != 1 {
		t.Fatal(failures, err)
	}
}

func TestDisabledHubUserCannotCompleteMFA(t *testing.T) {
	fastAccountHashes(t)
	s := newTestStore(t)
	alice, err := s.CreateFirstAdmin("alice", testAdminPassword)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.MutateHubUser("created", "bob", "", testAdminPassword, alice.AccountID); err != nil {
		t.Fatal(err)
	}
	bob, err := s.VerifyPassword("bob", testAdminPassword, "192.0.2.1")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s.nowFn = func() time.Time { return now }
	secret, err := s.BeginTOTPEnrollment(bob.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	code, _ := totp.Code(secret, now.Unix()/30)
	codes, err := s.ConfirmTOTP(bob.AccountID, code)
	if err != nil {
		t.Fatal(err)
	}
	bob, err = s.VerifyPassword("bob", testAdminPassword, "192.0.2.1")
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(30 * time.Second)
	code, _ = totp.Code(secret, now.Unix()/30)
	if err = s.MutateHubUser("disabled", "bob", "", "", alice.AccountID); err != nil {
		t.Fatal(err)
	}
	for _, factor := range []string{code, codes[0]} {
		if err = s.VerifySecondFactor(bob.AccountID, "192.0.2.1", factor); !errors.Is(err, ErrAccountAuth) {
			t.Fatal("disabled factor accepted", err)
		}
	}
	if _, err = s.CreateSession(bob, "192.0.2.1", ""); !errors.Is(err, ErrSessionAuth) {
		t.Fatal(err)
	}
}

func TestAuthenticationGenerationInvalidatesPendingLogin(t *testing.T) {
	for _, enrolled := range []bool{false, true} {
		for _, change := range []string{"disable-enable", "password-reset", "mfa-disable"} {
			t.Run(fmt.Sprintf("mfa=%t/%s", enrolled, change), func(t *testing.T) {
				fastAccountHashes(t)
				s := newTestStore(t)
				alice, err := s.CreateFirstAdmin("alice", testAdminPassword)
				if err != nil {
					t.Fatal(err)
				}
				if err = s.MutateHubUser("created", "bob", "bob@example.com", testAdminPassword, alice.AccountID); err != nil {
					t.Fatal(err)
				}
				bob, err := s.VerifyPassword("bob", testAdminPassword, "192.0.2.1")
				if err != nil {
					t.Fatal(err)
				}
				var codes []string
				if enrolled {
					secret, err := s.BeginTOTPEnrollment(bob.AccountID)
					if err != nil {
						t.Fatal(err)
					}
					code, _ := totp.Code(secret, s.nowFn().Unix()/30)
					codes, err = s.ConfirmTOTP(bob.AccountID, code)
					if err != nil {
						t.Fatal(err)
					}
					bob, err = s.VerifyPassword("bob", testAdminPassword, "192.0.2.1")
					if err != nil {
						t.Fatal(err)
					}
				}
				switch change {
				case "disable-enable":
					for _, action := range []string{"disabled", "enabled"} {
						if err = s.MutateHubUser(action, "bob", "", "", alice.AccountID); err != nil {
							t.Fatal(err)
						}
					}
				case "password-reset":
					if err = s.ResetAdminPassword("bob", testAdminPassword); err != nil {
						t.Fatal(err)
					}
				case "mfa-disable":
					if err = s.DisableMFA(bob.AccountID); err != nil {
						t.Fatal(err)
					}
				}
				if enrolled && change != "mfa-disable" {
					if err = s.VerifyLoginSecondFactor(bob, "192.0.2.1", codes[0]); !errors.Is(err, ErrSessionAuth) {
						t.Fatal("stale factor step accepted", err)
					}
				}
				if _, err = s.CreateSession(bob, "192.0.2.1", ""); !errors.Is(err, ErrSessionAuth) {
					t.Fatal("stale password step issued a session", err)
				}
				fresh, err := s.VerifyPassword("bob", testAdminPassword, "192.0.2.1")
				if err != nil {
					t.Fatal(err)
				}
				if enrolled && change != "mfa-disable" {
					if err = s.VerifySecondFactor(fresh.AccountID, "192.0.2.1", codes[1]); err != nil {
						t.Fatal(err)
					}
				}
				if _, err = s.CreateSession(fresh, "192.0.2.1", ""); err != nil {
					t.Fatal("fresh login rejected", err)
				}
			})
		}
	}
}

func TestUnknownLoginUsesGuessingAuditWithoutAccounts(t *testing.T) {
	fastAccountHashes(t)
	s := newTestStore(t)
	for i := range 50 {
		identifier := "unknown"
		if i%2 == 0 {
			identifier = "unknown@example.com"
		}
		if _, err := s.VerifyPassword(identifier, "wrong password", "192.0.2.1"); !errors.Is(err, ErrAccountAuth) {
			t.Fatal(err)
		}
	}
	var failures, signals int
	if err := s.rdb.QueryRow(`SELECT count(*) FROM audit_log WHERE auth_subject='local-user:unknown' AND action=?`, AuditHubLoginFailed).Scan(&failures); err != nil || failures != 50 {
		t.Fatal(failures, err)
	}
	if err := s.rdb.QueryRow(`SELECT count(*) FROM audit_log WHERE auth_subject='local-user:unknown' AND action=?`, AuditHubGuessing).Scan(&signals); err != nil || signals != 1 {
		t.Fatal(signals, err)
	}
	if n, err := s.CountAccounts(); err != nil || n != 0 {
		t.Fatal("unknown login created an account", n, err)
	}
}
