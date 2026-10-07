package store

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func fastAccountHashes(t *testing.T) {
	t.Helper()
	m, tm, p := argonMemory, argonTime, argonThreads
	argonMemory, argonTime, argonThreads = 64, 1, 1
	t.Cleanup(func() { argonMemory, argonTime, argonThreads = m, tm, p })
}

const testAdminPassword = "a long test password"

func TestFirstHubAdminConcurrent(t *testing.T) {
	fastAccountHashes(t)
	s := newTestStore(t)
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := s.CreateFirstAdmin("admin", testAdminPassword); results <- err }()
	}
	wg.Wait()
	close(results)
	wins := 0
	for err := range results {
		if err == nil {
			wins++
		} else if !errors.Is(err, ErrAdminExists) {
			t.Fatal(err)
		}
	}
	if wins != 1 {
		t.Fatalf("wins=%d", wins)
	}
	n, err := s.CountAccounts()
	if err != nil || n != 1 {
		t.Fatalf("count=%d %v", n, err)
	}
	for _, tt := range []struct{ user, pass string }{{"UPPER", testAdminPassword}, {"ab", testAdminPassword}, {"valid", "short"}, {"valid", strings.Repeat("x", 257)}} {
		if _, err := s.CreateFirstAdmin(tt.user, tt.pass); err == nil {
			t.Fatalf("accepted %+v", tt)
		}
	}
}

func TestHubPasswordLockout(t *testing.T) {
	fastAccountHashes(t)
	s := newTestStore(t)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s.nowFn = func() time.Time { return now }
	a, err := s.CreateFirstAdmin("admin", testAdminPassword)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if _, err = s.VerifyPassword("admin", "wrong", "192.0.2.1"); !errors.Is(err, ErrAccountAuth) {
			t.Fatal(err)
		}
	}
	for _, user := range []string{"admin", "unknown"} {
		if _, err = s.VerifyPassword(user, testAdminPassword, "192.0.2.1"); !errors.Is(err, ErrAccountAuth) {
			t.Fatalf("%s: %v", user, err)
		}
	}
	now = now.Add(15*time.Minute - time.Second)
	if _, err = s.VerifyPassword("admin", testAdminPassword, "192.0.2.1"); err == nil {
		t.Fatal("unlocked early")
	}
	now = now.Add(time.Second)
	if _, err = s.VerifyPassword("admin", testAdminPassword, "192.0.2.1"); err != nil {
		t.Fatal(err)
	}
	var failed int
	s.rdb.QueryRow(`SELECT count(*) FROM hub_login_failures WHERE account_id=?`, a.AccountID).Scan(&failed)
	if failed != 0 {
		t.Fatal(failed)
	}
	entries, err := s.Audit("", 100)
	if err != nil {
		t.Fatal(err)
	}
	lockouts := 0
	for _, e := range entries {
		if e.Action == AuditHubLockout {
			lockouts++
		}
		if strings.Contains(e.Detail, testAdminPassword) {
			t.Fatal("password in audit")
		}
	}
	if lockouts != 1 {
		t.Fatal(lockouts)
	}
}

func TestHubSessions(t *testing.T) {
	fastAccountHashes(t)
	for _, kind := range []string{"idle", "absolute", "revoke", "revoke-all", "disabled", "change", "reset"} {
		t.Run(kind, func(t *testing.T) {
			s := newTestStore(t)
			now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			s.nowFn = func() time.Time { return now }
			a, err := s.CreateFirstAdmin("admin", testAdminPassword)
			if err != nil {
				t.Fatal(err)
			}
			token, err := s.CreateSession(a, "127.0.0.1:1234", "test")
			if err != nil {
				t.Fatal(err)
			}
			other, err := s.CreateSession(a, "127.0.0.1:1234", "test")
			if err != nil {
				t.Fatal(err)
			}
			var hash string
			if err = s.rdb.QueryRow(`SELECT session_hash FROM hub_sessions WHERE session_hash=?`, sessionHash(token)).Scan(&hash); err != nil {
				t.Fatal(err)
			}
			if hash == token || len(hash) != 64 {
				t.Fatal("plaintext session")
			}
			if _, err = s.LookupSession(token); err != nil {
				t.Fatal(err)
			}
			now = now.Add(30 * time.Second)
			s.LookupSession(token)
			var seen string
			s.rdb.QueryRow(`SELECT last_seen_at FROM hub_sessions WHERE session_hash=?`, hash).Scan(&seen)
			if seen != fmtTime(now.Add(-30*time.Second)) {
				t.Fatal("slid too soon")
			}
			now = now.Add(30 * time.Second)
			s.LookupSession(token)
			s.rdb.QueryRow(`SELECT last_seen_at FROM hub_sessions WHERE session_hash=?`, hash).Scan(&seen)
			if seen != fmtTime(now) {
				t.Fatal("did not slide")
			}
			switch kind {
			case "idle":
				now = now.Add(12 * time.Hour)
			case "absolute":
				for range 16 {
					now = now.Add(10 * time.Hour)
					if _, err = s.LookupSession(token); err != nil {
						t.Fatal(err)
					}
				}
				now = now.Add(8 * time.Hour)
			case "revoke":
				err = s.RevokeSession(token)
			case "revoke-all":
				err = s.RevokeAllSessionsForAccount(a.AccountID)
			case "disabled":
				_, err = s.db.Exec(`UPDATE hub_accounts SET disabled_at=?`, fmtTime(now))
			case "change":
				err = s.ChangePassword(a.AccountID, "new long password", other)
			case "reset":
				err = s.ResetAdminPassword("admin", "new long password")
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.LookupSession(token); !errors.Is(err, ErrSessionAuth) {
				t.Fatalf("session survived: %v", err)
			}
			if kind == "change" {
				if _, err = s.LookupSession(other); err != nil {
					t.Fatal("own session revoked", err)
				}
				if _, err = s.CreateSession(a, "", ""); err == nil {
					t.Fatal("stale password verification minted a session")
				}
			}
		})
	}
}

func TestHubPerClientFailures(t *testing.T) {
	fastAccountHashes(t)
	s := newTestStore(t)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s.nowFn = func() time.Time { return now }
	a, err := s.CreateFirstAdmin("admin", testAdminPassword)
	if err != nil {
		t.Fatal(err)
	}
	for range 5 {
		s.VerifyPassword("admin", "wrong", "192.0.2.1")
	}
	if _, err = s.VerifyPassword("admin", testAdminPassword, "192.0.2.1"); !errors.Is(err, ErrAccountAuth) {
		t.Fatal("locked IP accepted", err)
	}
	if _, err = s.VerifyPassword("admin", testAdminPassword, "192.0.2.2"); err != nil {
		t.Fatal("other IP blocked", err)
	}
	s.VerifyPassword("unknown", "wrong", "192.0.2.3")
	var n int
	if err = s.rdb.QueryRow(`SELECT count(*) FROM hub_login_failures`).Scan(&n); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	s.VerifyPassword("admin", "wrong", "192.0.2.2")
	if _, err = s.VerifyPassword("admin", testAdminPassword, "192.0.2.2"); err != nil {
		t.Fatal(err)
	}
	if err = s.rdb.QueryRow(`SELECT count(*) FROM hub_login_failures`).Scan(&n); err != nil || n != 1 {
		t.Fatal("success must reset only its pair", n, err)
	}
	for i := 0; i < 50; i++ {
		s.VerifyPassword("admin", "wrong", fmt.Sprintf("198.51.100.%d", i))
	}
	countSignals := func() int {
		t.Helper()
		var n int
		if err := s.rdb.QueryRow(`SELECT count(*) FROM audit_log WHERE action=?`, AuditHubGuessing).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := countSignals(); n != 1 {
		t.Fatal("signals", n)
	}
	now = now.Add(time.Hour)
	for i := 0; i < 50; i++ {
		s.VerifyPassword("admin", "wrong", fmt.Sprintf("203.0.113.%d", i))
	}
	if n := countSignals(); n != 2 {
		t.Fatal("next hour signals", n)
	}
	if err = s.ResetAdminPassword("admin", "replacement password"); err != nil {
		t.Fatal(err)
	}
	if err = s.rdb.QueryRow(`SELECT count(*) FROM hub_login_failures WHERE account_id=?`, a.AccountID).Scan(&n); err != nil || n != 0 {
		t.Fatal("reset", n, err)
	}
	s.VerifyPassword("admin", "wrong", "192.0.2.1")
	now = now.Add(25 * time.Hour)
	s.VerifyPassword("unknown", "wrong", "192.0.2.3")
	if err = s.rdb.QueryRow(`SELECT count(*) FROM hub_login_failures`).Scan(&n); err != nil || n != 0 {
		t.Fatal("prune", n, err)
	}
}
