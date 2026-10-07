package store

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/totp"
)

func TestMFALifecycleReplayRecoveryLockout(t *testing.T) {
	fastAccountHashes(t)
	s := newTestStore(t)
	now := time.Unix(1800000000, 0)
	s.nowFn = func() time.Time { return now }
	a, err := s.CreateFirstAdmin("admin", testAdminPassword)
	if err != nil {
		t.Fatal(err)
	}
	secret, err := s.BeginTOTPEnrollment(a.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	code, _ := totp.Code(secret, now.Unix()/30)
	codes, err := s.ConfirmTOTP(a.AccountID, code)
	if err != nil || len(codes) != 10 {
		t.Fatal(codes, err)
	}
	if _, err = s.BeginTOTPEnrollment(a.AccountID); err == nil {
		t.Fatal("replaced enabled factor")
	}
	if err = s.VerifySecondFactor(a.AccountID, code); !errors.Is(err, ErrAccountAuth) {
		t.Fatal("enrollment code replay", err)
	}
	now = now.Add(30 * time.Second)
	code, _ = totp.Code(secret, now.Unix()/30)
	if err = s.VerifySecondFactor(a.AccountID, code); err != nil {
		t.Fatal(err)
	}
	if err = s.VerifySecondFactor(a.AccountID, code); err == nil {
		t.Fatal("replayed code")
	}
	if err = s.VerifySecondFactor(a.AccountID, codes[0]); err != nil {
		t.Fatal(err)
	}
	if err = s.VerifySecondFactor(a.AccountID, codes[0]); err == nil {
		t.Fatal("reused recovery")
	}
	for range 4 {
		if err = s.VerifySecondFactor(a.AccountID, "bad"); err == nil {
			t.Fatal("bad code")
		}
	}
	if _, err = s.VerifyPassword("admin", testAdminPassword); err == nil {
		t.Fatal("password bypassed MFA lockout")
	}
	if err = s.VerifySecondFactor(a.AccountID, codes[1]); err == nil {
		t.Fatal("recovery bypassed lockout")
	}
	now = now.Add(16 * time.Minute)
	if err = s.VerifySecondFactor(a.AccountID, codes[1]); err != nil {
		t.Fatal(err)
	}

	if err = s.DisableMFA(a.AccountID); err != nil {
		t.Fatal(err)
	}
	enabled, err := s.MFAEnabled(a.AccountID)
	if enabled || err != nil {
		t.Fatal(enabled, err)
	}
	rows, err := s.rdb.Query(`SELECT * FROM audit_log`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	actions := map[string]bool{}
	for rows.Next() {
		values := make([]any, len(columns))
		dest := make([]any, len(columns))
		for i := range values {
			dest[i] = &values[i]
		}
		if err = rows.Scan(dest...); err != nil {
			t.Fatal(err)
		}
		for i, value := range values {
			text := fmt.Sprint(value)
			if columns[i] == "action" {
				actions[text] = true
			}
			if strings.Contains(text, secret) {
				t.Fatal("audit secret")
			}
			for _, c := range codes {
				if strings.Contains(text, c) {
					t.Fatal("audit code")
				}
			}
		}
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	for _, action := range []AuditAction{AuditMFAEnabled, AuditMFADisabled, AuditMFAFailed, AuditRecoveryCodeUsed} {
		if !actions[string(action)] {
			t.Fatal("missing MFA audit", action)
		}
	}
}

func TestMFAConcurrentConsumption(t *testing.T) {
	fastAccountHashes(t)
	s := newTestStore(t)
	now := time.Unix(1800000000, 0)
	s.nowFn = func() time.Time { return now }
	a, err := s.CreateFirstAdmin("admin", testAdminPassword)
	if err != nil {
		t.Fatal(err)
	}
	secret, _ := s.BeginTOTPEnrollment(a.AccountID)
	code, _ := totp.Code(secret, now.Unix()/30-1)
	codes, err := s.ConfirmTOTP(a.AccountID, code)
	if err != nil {
		t.Fatal(err)
	}
	code, _ = totp.Code(secret, now.Unix()/30)
	for _, value := range []string{code, codes[0]} {
		results := make(chan error, 2)
		for range 2 {
			go func() { results <- s.VerifySecondFactor(a.AccountID, value) }()
		}
		wins := 0
		for range 2 {
			if err := <-results; err == nil {
				wins++
			} else if !errors.Is(err, ErrAccountAuth) {
				t.Fatal(err)
			}
		}
		if wins != 1 {
			t.Fatal("concurrent consumption", wins)
		}
	}
}
