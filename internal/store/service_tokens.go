package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/netip"
	"regexp"
	"strings"
	"time"
)

var ErrServiceToken = errors.New("service token rejected")
var serviceName = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

type ServiceToken struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	Scope       string     `json:"scope"`
	Allowlist   []string   `json:"allowlist"`
	SourceCIDRs []string   `json:"source_cidrs"`
	CreatedBy   string     `json:"created_by"`
	CreatedAt   time.Time  `json:"created_at"`
	ExpiresAt   time.Time  `json:"expires_at"`
	LastUsedAt  *time.Time `json:"last_used_at,omitempty"`
	RevokedAt   *time.Time `json:"revoked_at,omitempty"`
}

func ValidServiceSecret(secret string) bool {
	if len(secret) != 68 || !strings.HasPrefix(secret, "cst_") {
		return false
	}
	_, err := hex.DecodeString(secret[4:])
	return err == nil
}

func (s *Store) CreateServiceToken(t ServiceToken, actor string) (ServiceToken, string, error) {
	now := s.now().UTC()
	if !serviceName.MatchString(t.Name) || (t.Scope != "view" && t.Scope != "operate") || !t.ExpiresAt.After(now) || t.ExpiresAt.After(now.Add(90*24*time.Hour)) || len(t.Allowlist) == 0 || len(t.Allowlist) > 128 || len(t.SourceCIDRs) > 32 {
		return ServiceToken{}, "", ErrServiceToken
	}
	for _, v := range t.Allowlist {
		if !validServiceRoute(v) {
			return ServiceToken{}, "", ErrServiceToken
		}
	}
	for _, v := range t.SourceCIDRs {
		if _, err := netip.ParsePrefix(v); err != nil {
			return ServiceToken{}, "", ErrServiceToken
		}
	}
	tx, err := s.beginWrite(context.Background(), "create_service_token")
	if err != nil {
		return ServiceToken{}, "", err
	}
	defer tx.Rollback()
	var n int
	if err = tx.QueryRow(`SELECT count(*) FROM hub_accounts WHERE account_id=? AND role='admin' AND disabled_at IS NULL`, actor).Scan(&n); err != nil || n != 1 {
		return ServiceToken{}, "", ErrServiceToken
	}
	var b [32]byte
	if _, err = rand.Read(b[:]); err != nil {
		return ServiceToken{}, "", err
	}
	secret := "cst_" + hex.EncodeToString(b[:])
	hash := sha256.Sum256([]byte(secret))
	t.ID, err = newHubAccountID()
	if err != nil {
		return ServiceToken{}, "", err
	}
	t.CreatedBy = actor
	t.CreatedAt = now
	t.LastUsedAt = nil
	t.RevokedAt = nil
	a, _ := json.Marshal(t.Allowlist)
	c, _ := json.Marshal(t.SourceCIDRs)
	_, err = tx.Exec(`INSERT INTO service_tokens(id,name,scope,token_hash,allowlist,source_cidrs,created_by,created_at,expires_at) VALUES(?,?,?,?,?,?,?,?,?)`, t.ID, t.Name, t.Scope, hash[:], string(a), string(c), actor, now.Format(time.RFC3339Nano), t.ExpiresAt.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return ServiceToken{}, "", ErrServiceToken
	}
	err = s.recordAuditTx(tx, AuditEntry{Action: "service-token-create", AuthSubject: "local-user:" + actor, AuthMethod: "local-account-session", Subject: t.ID, OK: true})
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		return ServiceToken{}, "", err
	}
	return t, secret, nil
}

const serviceTokenColumns = "id,name,scope,allowlist,source_cidrs,created_by,created_at,expires_at,last_used_at,revoked_at"

// ListServiceTokens returns at most 201 entries; the UI shows 200 and reports truncation.
func (s *Store) ListServiceTokens() ([]ServiceToken, error) {
	rows, err := s.rdb.Query("SELECT " + serviceTokenColumns + " FROM service_tokens ORDER BY created_at DESC,id LIMIT 201")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ServiceToken{}
	for rows.Next() {
		t, err := scanServiceToken(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func scanServiceToken(row rowScanner) (ServiceToken, error) {
	var t ServiceToken
	var a, c, created, expires string
	var used, revoked *string
	if err := row.Scan(&t.ID, &t.Name, &t.Scope, &a, &c, &t.CreatedBy, &created, &expires, &used, &revoked); err != nil {
		return t, err
	}
	if json.Unmarshal([]byte(a), &t.Allowlist) != nil || json.Unmarshal([]byte(c), &t.SourceCIDRs) != nil {
		return t, ErrServiceToken
	}
	var err error
	t.CreatedAt, err = time.Parse(time.RFC3339Nano, created)
	if err != nil {
		return t, ErrServiceToken
	}
	t.ExpiresAt, err = time.Parse(time.RFC3339Nano, expires)
	if err != nil {
		return t, ErrServiceToken
	}
	if used != nil {
		v, e := time.Parse(time.RFC3339Nano, *used)
		if e != nil {
			return t, ErrServiceToken
		}
		t.LastUsedAt = &v
	}
	if revoked != nil {
		v, e := time.Parse(time.RFC3339Nano, *revoked)
		if e != nil {
			return t, ErrServiceToken
		}
		t.RevokedAt = &v
	}
	return t, nil
}

func (s *Store) RevokeServiceToken(id, actor string) error {
	tx, err := s.beginWrite(context.Background(), "revoke_service_token")
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var n int
	if err = tx.QueryRow(`SELECT count(*) FROM hub_accounts WHERE account_id=? AND role='admin' AND disabled_at IS NULL`, actor).Scan(&n); err != nil || n != 1 {
		return ErrServiceToken
	}
	res, err := tx.Exec(`UPDATE service_tokens SET revoked_at=COALESCE(revoked_at,?) WHERE id=?`, s.now().UTC().Format(time.RFC3339Nano), id)
	if err != nil {
		return err
	}
	nrows, _ := res.RowsAffected()
	if nrows != 1 {
		return ErrServiceToken
	}
	err = s.recordAuditTx(tx, AuditEntry{Action: "service-token-revoke", AuthSubject: "local-user:" + actor, AuthMethod: "local-account-session", Subject: id, OK: true})
	if err != nil {
		return err
	}
	return tx.Commit()
}

// AuthenticateServiceToken checks current database state on every request; no cache delays revocation.
func (s *Store) AuthenticateServiceToken(secret, route, peer string, required string) (ServiceToken, error) {
	if !ValidServiceSecret(secret) {
		return ServiceToken{}, ErrServiceToken
	}
	hash := sha256.Sum256([]byte(secret))
	var stored []byte
	var id string
	if err := s.rdb.QueryRow(`SELECT id,token_hash FROM service_tokens WHERE token_hash=?`, hash[:]).Scan(&id, &stored); err != nil || subtle.ConstantTimeCompare(hash[:], stored) != 1 {
		return ServiceToken{}, ErrServiceToken
	}
	t, err := scanServiceToken(s.rdb.QueryRow("SELECT "+serviceTokenColumns+" FROM service_tokens WHERE id=?", id))
	if err != nil {
		return ServiceToken{}, ErrServiceToken
	}
	if !serviceName.MatchString(t.Name) || len(t.Allowlist) == 0 || len(t.Allowlist) > 128 {
		return ServiceToken{}, ErrServiceToken
	}
	if (required != "view" && required != "operate") || (t.Scope != "view" && t.Scope != "operate") || (required == "operate" && t.Scope != "operate") || t.RevokedAt != nil || !t.ExpiresAt.After(s.now()) || t.ExpiresAt.After(t.CreatedAt.Add(90*24*time.Hour)) {
		return t, ErrServiceToken
	}
	for _, route := range t.Allowlist {
		if !validServiceRoute(route) {
			return t, ErrServiceToken
		}
	}
	if len(t.Allowlist) > 0 {
		found := false
		for _, v := range t.Allowlist {
			if v == route {
				found = true
			}
		}
		if !found {
			return t, ErrServiceToken
		}
	}
	if len(t.SourceCIDRs) > 0 {
		ip, err := netip.ParseAddr(peer)
		if err != nil {
			return t, ErrServiceToken
		}
		found := false
		for _, v := range t.SourceCIDRs {
			p, e := netip.ParsePrefix(v)
			if e == nil && p.Contains(ip.Unmap()) {
				found = true
			}
		}
		if !found {
			return t, ErrServiceToken
		}
	}
	res, err := (boundExec{s: s, name: "use_service_token"}).Exec(`UPDATE service_tokens SET last_used_at=? WHERE id=? AND revoked_at IS NULL AND scope=? AND julianday(expires_at)>julianday(?)`, s.now().UTC().Format(time.RFC3339Nano), id, t.Scope, s.now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return ServiceToken{}, err
	}
	n, err := res.RowsAffected()
	if err != nil || n != 1 {
		return t, ErrServiceToken
	}
	return t, nil
}

var serviceRoute = regexp.MustCompile(`^(GET|HEAD|POST|PUT|PATCH|DELETE) /v1/operator/[A-Za-z0-9_{}./$-]+$`)

// Validate using the same ServeMux pattern parser as the Hub, without accepting
// a malformed method, whitespace, query, fragment, or wildcard pattern.
func validServiceRoute(route string) (valid bool) {
	if len(route) > 256 || !serviceRoute.MatchString(route) {
		return false
	}
	defer func() {
		if recover() != nil {
			valid = false
		}
	}()
	http.NewServeMux().Handle(route, http.NotFoundHandler())
	return true
}
