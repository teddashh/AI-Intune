package store

import (
	"crypto/sha256"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestServiceTokenLifecycle(t *testing.T) {
	fastAccountHashes(t)
	s := newTestStore(t)
	actor, err := s.CreateFirstAdmin("admin", testAdminPassword)
	if err != nil {
		t.Fatal(err)
	}
	now := s.now()
	req := ServiceToken{Name: "agent", Scope: "view", ExpiresAt: now.Add(time.Hour), Allowlist: []string{"GET /v1/operator/machines"}, SourceCIDRs: []string{"192.0.2.0/24"}}
	token, secret, err := s.CreateServiceToken(req, actor.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	var hash []byte
	if err = s.rdb.QueryRow(`SELECT token_hash FROM service_tokens WHERE id=?`, token.ID).Scan(&hash); err != nil {
		t.Fatal(err)
	}
	expected := sha256.Sum256([]byte(secret))
	if string(hash) != string(expected[:]) {
		t.Fatal("hash mismatch")
	}
	list, err := s.ListServiceTokens()
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(list)
	if strings.Contains(string(raw), secret) || strings.Contains(string(raw), "token_hash") {
		t.Fatal("secret disclosure")
	}
	checks := []struct {
		route, peer, scope string
		ok                 bool
	}{{"GET /v1/operator/machines", "192.0.2.1", "view", true}, {"GET /v1/operator/jobs", "192.0.2.1", "view", false}, {"GET /v1/operator/machines", "198.51.100.1", "view", false}, {"GET /v1/operator/machines", "192.0.2.1", "operate", false}, {"GET /v1/operator/machines", "192.0.2.1", "admin", false}}
	for _, c := range checks {
		_, err = s.AuthenticateServiceToken(secret, c.route, c.peer, c.scope)
		if (err == nil) != c.ok {
			t.Fatalf("%+v: %v", c, err)
		}
	}
	if err = s.RevokeServiceToken(token.ID, actor.AccountID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AuthenticateServiceToken(secret, checks[0].route, checks[0].peer, "view"); err == nil {
		t.Fatal("revoked accepted")
	}
	for _, scope := range []string{"admin", "", "VIEW"} {
		req.Scope = scope
		if _, _, err = s.CreateServiceToken(req, actor.AccountID); err == nil {
			t.Fatal("invalid scope accepted")
		}
	}
	req.Scope = "operate"
	req.Name = "other"
	req.ExpiresAt = now.Add(91 * 24 * time.Hour)
	if _, _, err = s.CreateServiceToken(req, actor.AccountID); err == nil {
		t.Fatal("long expiry accepted")
	}
	req.ExpiresAt = time.Time{}
	if _, _, err = s.CreateServiceToken(req, actor.AccountID); err == nil {
		t.Fatal("missing expiry accepted")
	}
	req.ExpiresAt = now.Add(time.Hour)
	token, secret, err = s.CreateServiceToken(req, actor.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	s.nowFn = func() time.Time { return now.Add(2 * time.Hour) }
	if _, err = s.AuthenticateServiceToken(secret, checks[0].route, checks[0].peer, "view"); err == nil {
		t.Fatal("expired accepted")
	}
	if _, err = s.db.Exec(`UPDATE service_tokens SET scope='admin' WHERE id=?`, token.ID); err == nil {
		t.Fatal("database accepted admin")
	}
	// Even a legacy/corrupt row bypassing schema validation cannot grant admin.
	s.nowFn = func() time.Time { return now }
	if _, err = s.db.Exec(`PRAGMA ignore_check_constraints=ON`); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`UPDATE service_tokens SET scope='admin' WHERE id=?`, token.ID); err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"view", "operate", "admin"} {
		if _, err = s.AuthenticateServiceToken(secret, checks[0].route, checks[0].peer, required); err == nil {
			t.Fatal("corrupt admin scope accepted")
		}
	}
	if _, err = s.db.Exec(`PRAGMA ignore_check_constraints=OFF`); err != nil {
		t.Fatal(err)
	}
}
