package store

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/model"
)

func newOperatorEnrollTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	st.nowFn = func() time.Time {
		return time.Date(2026, 9, 7, 20, 30, 0, 123456789, time.UTC)
	}
	return st
}

func operatorEnrollTestRequest(preview OperatorEnrollTokenPreviewResult) OperatorEnrollTokenCreateRequest {
	return OperatorEnrollTokenCreateRequest{
		DisplayName: preview.DisplayName, TTLSeconds: preview.TTLSeconds,
		PreviewDigest: preview.PreviewDigest, Reason: "acceptance enrollment",
		IdempotencyKey: "enroll-test-key", RequestDigest: "sha256:canonical-request",
		Audit: AuditEntry{
			Reason:     "caller supplied decoration must be overwritten",
			SourceAddr: "100.64.0.10", AuthSubject: "tailscale-user:1",
			AuthCapability: "example.com/cap/clawctl-admin", SourceKind: "operator-api",
		},
	}
}

func TestOperatorEnrollTokenPreviewIsDeterministicAndReadOnly(t *testing.T) {
	st := newOperatorEnrollTestStore(t)
	preview, err := st.PreviewOperatorEnrollToken("onode-preview", 2*60*60)
	if err != nil {
		t.Fatal(err)
	}
	if preview.DisplayName != "onode-preview" || preview.TTLSeconds != 7200 ||
		!preview.PreviewedAt.Equal(st.nowFn()) ||
		!preview.ExpiresAtIfCreatedNow.Equal(st.nowFn().Add(2*time.Hour)) ||
		!preview.CreatesExpectedMachine ||
		preview.InitialState != OperatorEnrollTokenInitialStateNeverReported ||
		!preview.RevocationKeepsRegistryRow ||
		preview.SecretDelivery != OperatorEnrollTokenSecretDeliveryFirstOnly ||
		!strings.HasPrefix(preview.PreviewDigest, "sha256:") {
		t.Fatalf("preview=%+v", preview)
	}
	// Time is presentation-only and therefore cannot make a valid preview
	// digest stale before the create transaction begins.
	st.nowFn = func() time.Time { return preview.PreviewedAt.Add(30 * time.Minute) }
	later, err := st.PreviewOperatorEnrollToken("onode-preview", 2*60*60)
	if err != nil || later.PreviewDigest != preview.PreviewDigest {
		t.Fatalf("later preview=%+v err=%v", later, err)
	}
	var machines, tokens, keys int
	for query, dst := range map[string]*int{
		`SELECT COUNT(*) FROM machine_registry`:     &machines,
		`SELECT COUNT(*) FROM enrollment_tokens`:    &tokens,
		`SELECT COUNT(*) FROM operator_idempotency`: &keys,
	} {
		if err := st.db.QueryRow(query).Scan(dst); err != nil {
			t.Fatal(err)
		}
	}
	if machines != 0 || tokens != 0 || keys != 0 {
		t.Fatalf("preview wrote state: machines=%d tokens=%d keys=%d", machines, tokens, keys)
	}
}

func TestOperatorEnrollTokenPreviewRejectsNonCanonicalIntent(t *testing.T) {
	st := newOperatorEnrollTestStore(t)
	tests := []struct {
		name        string
		displayName string
		ttl         int64
		want        error
	}{
		{name: "empty", displayName: "", ttl: 60, want: ErrBadDisplayName},
		{name: "only whitespace", displayName: " \t", ttl: 60, want: ErrBadDisplayName},
		{name: "leading whitespace", displayName: " onode", ttl: 60, want: ErrBadDisplayName},
		{name: "trailing whitespace", displayName: "onode ", ttl: 60, want: ErrBadDisplayName},
		{name: "too short", displayName: "onode", ttl: 59, want: ErrBadEnrollTTL},
		{name: "too long", displayName: "onode", ttl: 86401, want: ErrBadEnrollTTL},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := st.PreviewOperatorEnrollToken(tc.displayName, tc.ttl)
			if !errors.Is(err, tc.want) {
				t.Fatalf("error=%v, want %v", err, tc.want)
			}
		})
	}
	for _, ttl := range []int64{OperatorEnrollTokenMinTTLSeconds, OperatorEnrollTokenMaxTTLSeconds} {
		if _, err := st.PreviewOperatorEnrollToken("boundary", ttl); err != nil {
			t.Fatalf("boundary ttl %d: %v", ttl, err)
		}
	}
}

func TestOperatorEnrollTokenPreviewDigestBindsEveryImpactPolicyComponent(t *testing.T) {
	policy := currentOperatorEnrollTokenPolicy()
	base := operatorEnrollTokenPreviewDigestForIdentity(
		operatorEnrollTokenVersion, "policy-machine", 3600, policy)
	tests := []struct {
		name    string
		version string
		mutate  func(*operatorEnrollTokenPolicyIdentity)
	}{
		{name: "version", version: "v2"},
		{name: "minimum ttl", mutate: func(p *operatorEnrollTokenPolicyIdentity) { p.MinTTLSeconds++ }},
		{name: "maximum ttl", mutate: func(p *operatorEnrollTokenPolicyIdentity) { p.MaxTTLSeconds-- }},
		{name: "expected denominator effect", mutate: func(p *operatorEnrollTokenPolicyIdentity) { p.CreatesExpectedMachine = false }},
		{name: "initial state", mutate: func(p *operatorEnrollTokenPolicyIdentity) { p.InitialState = "pending" }},
		{name: "revocation effect", mutate: func(p *operatorEnrollTokenPolicyIdentity) { p.RevocationKeepsRegistryRow = false }},
		{name: "secret delivery", mutate: func(p *operatorEnrollTokenPolicyIdentity) { p.SecretDelivery = "replayable" }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			changed := policy
			version := operatorEnrollTokenVersion
			if tc.version != "" {
				version = tc.version
			}
			if tc.mutate != nil {
				tc.mutate(&changed)
			}
			if got := operatorEnrollTokenPreviewDigestForIdentity(
				version, "policy-machine", 3600, changed); got == base {
				t.Fatalf("policy change did not rotate preview digest: %+v", changed)
			}
		})
	}
}

func TestOperatorEnrollTokenFreshCommitStoresOnlyHashAndRedactedReceipt(t *testing.T) {
	st := newOperatorEnrollTestStore(t)
	preview, err := st.PreviewOperatorEnrollToken("onode-fresh", 7200)
	if err != nil {
		t.Fatal(err)
	}
	req := operatorEnrollTestRequest(preview)
	result, err := st.ApplyOperatorEnrollToken(req)
	if err != nil {
		t.Fatal(err)
	}
	if result.MachineID == "" || result.DisplayName != req.DisplayName ||
		result.TTLSeconds != req.TTLSeconds || result.PreviewDigest != req.PreviewDigest ||
		result.EnrollmentToken == "" || !result.SecretAvailable || result.Replayed ||
		result.RecoveryRequired || result.RecoveryAction != "" || !result.Audited {
		t.Fatalf("fresh result=%+v", result)
	}
	if len(result.EnrollmentToken) != 43 {
		t.Fatalf("token length=%d, want 43", len(result.EnrollmentToken))
	}
	serializedResult, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(serializedResult), result.EnrollmentToken) ||
		strings.Contains(string(serializedResult), "enrollment_token") {
		t.Fatalf("generic domain result serialization exposed token field: %s", serializedResult)
	}
	expectedCreatedAt := st.nowFn().UTC().Truncate(time.Second)
	if !result.CreatedAt.Equal(expectedCreatedAt) ||
		!result.ExpiresAt.Equal(result.CreatedAt.Add(2*time.Hour)) {
		t.Fatalf("fresh timestamps=%s..%s", result.CreatedAt, result.ExpiresAt)
	}

	var displayName, createdAt, enrolledAt string
	var expected int
	if err := st.db.QueryRow(`SELECT display_name,expected,created_at,COALESCE(enrolled_at,'')
	 FROM machine_registry WHERE machine_id=?`, result.MachineID).
		Scan(&displayName, &expected, &createdAt, &enrolledAt); err != nil {
		t.Fatal(err)
	}
	if displayName != req.DisplayName || expected != 1 || enrolledAt != "" || createdAt == "" {
		t.Fatalf("registry display=%q expected=%d created=%q enrolled=%q",
			displayName, expected, createdAt, enrolledAt)
	}
	var tokenHash, tokenDisplay, responseJSON string
	var tokenCreatedAt, tokenExpiresAt string
	if err := st.db.QueryRow(`SELECT token_hash,display_name,created_at,expires_at
	 FROM enrollment_tokens WHERE used_by=?`, result.MachineID).
		Scan(&tokenHash, &tokenDisplay, &tokenCreatedAt, &tokenExpiresAt); err != nil {
		t.Fatal(err)
	}
	if tokenHash != hashToken(result.EnrollmentToken) || tokenHash == result.EnrollmentToken ||
		tokenDisplay != req.DisplayName {
		t.Fatalf("stored token identity hash=%q display=%q", tokenHash, tokenDisplay)
	}
	if err := st.db.QueryRow(`SELECT response_json FROM operator_idempotency WHERE idempotency_key=?`,
		req.IdempotencyKey).Scan(&responseJSON); err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{result.EnrollmentToken, "enrollment_token", "secret_available", "recovery_action"} {
		if strings.Contains(responseJSON, forbidden) {
			t.Fatalf("cached receipt contains %q: %s", forbidden, responseJSON)
		}
	}
	var receipt operatorEnrollTokenReceipt
	if err := json.Unmarshal([]byte(responseJSON), &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.MachineID != result.MachineID || receipt.PreviewDigest != req.PreviewDigest {
		t.Fatalf("cached receipt=%+v", receipt)
	}
	var idempotencyCreatedAt, auditAt string
	if err := st.db.QueryRow(`SELECT created_at FROM operator_idempotency WHERE idempotency_key=?`,
		req.IdempotencyKey).Scan(&idempotencyCreatedAt); err != nil {
		t.Fatal(err)
	}
	if err := st.db.QueryRow(`SELECT at FROM audit_log WHERE machine_id=? AND action=?`,
		result.MachineID, string(AuditEnrollToken)).Scan(&auditAt); err != nil {
		t.Fatal(err)
	}
	wantCreatedAt, wantExpiresAt := fmtTime(result.CreatedAt), fmtTime(result.ExpiresAt)
	if createdAt != wantCreatedAt || tokenCreatedAt != wantCreatedAt ||
		idempotencyCreatedAt != wantCreatedAt || auditAt != wantCreatedAt ||
		tokenExpiresAt != wantExpiresAt {
		t.Fatalf("writer clock drift registry=%q token=%q idempotency=%q audit=%q expires=%q want=%q/%q",
			createdAt, tokenCreatedAt, idempotencyCreatedAt, auditAt, tokenExpiresAt,
			wantCreatedAt, wantExpiresAt)
	}
	entries, err := st.Audit(result.MachineID, 10)
	if err != nil || len(entries) != 1 {
		t.Fatalf("audit=%+v err=%v", entries, err)
	}
	audit := entries[0]
	if !audit.OK || audit.Action != AuditEnrollToken || audit.Subject != req.DisplayName ||
		audit.Reason != req.Reason || audit.IdempotencyKey != req.IdempotencyKey ||
		audit.RequestDigest != req.RequestDigest || strings.Contains(audit.Detail, result.EnrollmentToken) {
		t.Fatalf("audit=%+v", audit)
	}
	pending, err := st.PendingEnrollToken(result.MachineID, result.ExpiresAt)
	if err != nil || pending == nil || !pending.Expired {
		t.Fatalf("token at expires_at must be expired: pending=%+v err=%v", pending, err)
	}
	enrollReq := model.EnrollRequest{
		SchemaVersion: model.SchemaVersion, EnrollToken: result.EnrollmentToken,
		Hostname: "onode-fresh", OS: "linux", Arch: "amd64",
	}
	if _, _, err := st.RedeemEnrollToken(result.EnrollmentToken, enrollReq, result.ExpiresAt); !errors.Is(err, ErrTokenExpired) {
		t.Fatalf("redeem at expires_at error=%v, want ErrTokenExpired", err)
	}
	if machineID, agentToken, err := st.RedeemEnrollToken(result.EnrollmentToken, enrollReq,
		result.ExpiresAt.Add(-time.Nanosecond)); err != nil || machineID != result.MachineID || agentToken == "" {
		t.Fatalf("redeem immediately before expires_at machine=%q token_present=%t err=%v",
			machineID, agentToken != "", err)
	}
}

func TestOperatorEnrollTokenReplayNeverReturnsSecretOrCreatesSecondMachine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	st.nowFn = func() time.Time { return time.Date(2026, 9, 7, 21, 0, 0, 0, time.UTC) }
	preview, err := st.PreviewOperatorEnrollToken("onode-replay", 3600)
	if err != nil {
		t.Fatal(err)
	}
	req := operatorEnrollTestRequest(preview)
	first, err := st.ApplyOperatorEnrollToken(req)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	st.nowFn = func() time.Time { return time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC) }
	replay, err := st.ApplyOperatorEnrollToken(req)
	if err != nil {
		t.Fatal(err)
	}
	if replay.MachineID != first.MachineID || replay.CreatedAt != first.CreatedAt ||
		replay.ExpiresAt != first.ExpiresAt || replay.PreviewDigest != first.PreviewDigest ||
		replay.EnrollmentToken != "" || replay.SecretAvailable || !replay.Replayed ||
		!replay.RecoveryRequired || replay.RecoveryAction != OperatorEnrollTokenRecoveryRevokeAndReissue ||
		!replay.Audited {
		t.Fatalf("replay=%+v first=%+v", replay, first)
	}
	var machines, tokens int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM machine_registry`).Scan(&machines); err != nil {
		t.Fatal(err)
	}
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM enrollment_tokens`).Scan(&tokens); err != nil {
		t.Fatal(err)
	}
	if machines != 1 || tokens != 1 {
		t.Fatalf("replay duplicated state: machines=%d tokens=%d", machines, tokens)
	}
	entries, err := st.Audit(first.MachineID, 10)
	if err != nil || len(entries) != 2 || !entries[0].IsOperatorReplay() ||
		!strings.Contains(entries[0].Detail, "明文不會重顯") {
		t.Fatalf("replay audit=%+v err=%v", entries, err)
	}
}

func TestOperatorEnrollTokenConcurrentSameKeyIssuesExactlyOneSecret(t *testing.T) {
	st := newOperatorEnrollTestStore(t)
	preview, err := st.PreviewOperatorEnrollToken("onode-concurrent", 3600)
	if err != nil {
		t.Fatal(err)
	}
	req := operatorEnrollTestRequest(preview)
	type outcome struct {
		result OperatorEnrollTokenCreateResult
		err    error
	}
	start := make(chan struct{})
	outcomes := make(chan outcome, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			result, err := st.ApplyOperatorEnrollToken(req)
			outcomes <- outcome{result: result, err: err}
		}()
	}
	close(start)
	wg.Wait()
	close(outcomes)
	fresh, replay := 0, 0
	var machineID string
	for got := range outcomes {
		if got.err != nil {
			t.Fatalf("concurrent create: %v", got.err)
		}
		if machineID == "" {
			machineID = got.result.MachineID
		} else if got.result.MachineID != machineID {
			t.Fatalf("concurrent calls returned different machines: %q != %q",
				got.result.MachineID, machineID)
		}
		if got.result.SecretAvailable && got.result.EnrollmentToken != "" && !got.result.Replayed {
			fresh++
		} else if !got.result.SecretAvailable && got.result.EnrollmentToken == "" && got.result.Replayed {
			replay++
		} else {
			t.Fatalf("invalid concurrent delivery flags: %+v", got.result)
		}
	}
	if fresh != 1 || replay != 1 {
		t.Fatalf("concurrent delivery fresh=%d replay=%d", fresh, replay)
	}
	var machines, tokens, receipts int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM machine_registry`).Scan(&machines); err != nil {
		t.Fatal(err)
	}
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM enrollment_tokens`).Scan(&tokens); err != nil {
		t.Fatal(err)
	}
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM operator_idempotency`).Scan(&receipts); err != nil {
		t.Fatal(err)
	}
	if machines != 1 || tokens != 1 || receipts != 1 {
		t.Fatalf("concurrent rows machines=%d tokens=%d receipts=%d", machines, tokens, receipts)
	}
}

func TestOperatorEnrollTokenIdempotencyConflictDoesNotLeakOriginal(t *testing.T) {
	st := newOperatorEnrollTestStore(t)
	preview, err := st.PreviewOperatorEnrollToken("onode-original", 3600)
	if err != nil {
		t.Fatal(err)
	}
	req := operatorEnrollTestRequest(preview)
	first, err := st.ApplyOperatorEnrollToken(req)
	if err != nil {
		t.Fatal(err)
	}
	conflict := req
	conflict.DisplayName = "onode-different"
	conflict.RequestDigest = "sha256:different-request"
	_, err = st.ApplyOperatorEnrollToken(conflict)
	if !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("conflict error=%v", err)
	}
	if strings.Contains(err.Error(), first.EnrollmentToken) || strings.Contains(err.Error(), first.MachineID) {
		t.Fatalf("conflict leaked original receipt: %v", err)
	}
	var machines, tokens int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM machine_registry`).Scan(&machines); err != nil {
		t.Fatal(err)
	}
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM enrollment_tokens`).Scan(&tokens); err != nil {
		t.Fatal(err)
	}
	if machines != 1 || tokens != 1 {
		t.Fatalf("conflict changed state: machines=%d tokens=%d", machines, tokens)
	}
}

func TestOperatorEnrollTokenDomainRejectionIsPersistedAndReplayed(t *testing.T) {
	st := newOperatorEnrollTestStore(t)
	req := OperatorEnrollTokenCreateRequest{
		DisplayName: "onode-rejected", TTLSeconds: 3600,
		IdempotencyKey: "rejected-enroll-key", RequestDigest: "sha256:rejected",
		Audit: AuditEntry{SourceAddr: "100.64.0.11"},
	}
	_, firstErr := st.ApplyOperatorEnrollToken(req)
	if !errors.Is(firstErr, ErrPreviewRequired) {
		t.Fatalf("first error=%v", firstErr)
	}
	var firstRejection *OperatorRequestError
	if !errors.As(firstErr, &firstRejection) || firstRejection.Replayed ||
		firstRejection.Detail != mustCanonicalOperatorEnrollRejectionDetail(t, OperatorCodePreviewRequired) {
		t.Fatalf("first rejection=%+v", firstRejection)
	}
	var storedDetail string
	if err := st.db.QueryRow(`SELECT error_detail FROM operator_idempotency WHERE idempotency_key=?`,
		req.IdempotencyKey).Scan(&storedDetail); err != nil {
		t.Fatal(err)
	}
	if !operatorEnrollTokenStoredRejectionMatchesCode(storedDetail, OperatorCodePreviewRequired) {
		t.Fatalf("stored rejection lacks stable code evidence: %q", storedDetail)
	}
	// Even after a valid preview becomes available, the exact historical body
	// keeps its original rejection. The caller needs a new body and key.
	preview, err := st.PreviewOperatorEnrollToken(req.DisplayName, req.TTLSeconds)
	if err != nil || preview.PreviewDigest == "" {
		t.Fatalf("preview=%+v err=%v", preview, err)
	}
	_, replayErr := st.ApplyOperatorEnrollToken(req)
	if !errors.Is(replayErr, ErrPreviewRequired) {
		t.Fatalf("replay error=%v", replayErr)
	}
	var rejection *OperatorRequestError
	if !errors.As(replayErr, &rejection) || !rejection.Replayed || !rejection.Audited ||
		rejection.Detail != mustHistoricalOperatorEnrollRejectionDetail(t, OperatorCodePreviewRequired) {
		t.Fatalf("replayed rejection=%T %+v", replayErr, rejection)
	}
	entries, auditErr := st.Audit("", 10)
	if auditErr != nil || len(entries) != 2 || entries[1].Detail != storedDetail ||
		strings.Contains(entries[0].Detail, storedDetail) ||
		!strings.Contains(entries[0].Detail,
			mustHistoricalOperatorEnrollRejectionDetail(t, OperatorCodePreviewRequired)) {
		t.Fatalf("fresh/replay rejection audit=%+v err=%v", entries, auditErr)
	}
	var machines, tokens int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM machine_registry`).Scan(&machines); err != nil {
		t.Fatal(err)
	}
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM enrollment_tokens`).Scan(&tokens); err != nil {
		t.Fatal(err)
	}
	if machines != 0 || tokens != 0 {
		t.Fatalf("rejection changed state: machines=%d tokens=%d", machines, tokens)
	}
}

func TestOperatorEnrollTokenChecksCacheBeforeCurrentPolicy(t *testing.T) {
	t.Run("historical success", func(t *testing.T) {
		st := newOperatorEnrollTestStore(t)
		createdAt := st.nowFn().Add(-time.Hour).UTC().Truncate(time.Second)
		receipt := operatorEnrollTokenReceipt{
			MachineID: "historical-machine", DisplayName: "historical-success",
			CreatedAt: createdAt, ExpiresAt: createdAt.Add(30 * time.Second),
			TTLSeconds: 30, PreviewDigest: "sha256:old-policy-preview",
		}
		raw, err := json.Marshal(receipt)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.db.Exec(`INSERT INTO machine_registry
		 (machine_id,display_name,expected,created_at) VALUES (?,?,1,?)`,
			receipt.MachineID, receipt.DisplayName, fmtTime(createdAt)); err != nil {
			t.Fatal(err)
		}
		if _, err := st.db.Exec(`INSERT INTO operator_idempotency
		 (idempotency_key,operation,request_digest,outcome,response_json,created_at)
		 VALUES (?,?,?,'ok',?,?)`, "old-success-key", operatorEnrollTokenOperation,
			"sha256:old-success", string(raw), fmtTime(createdAt)); err != nil {
			t.Fatal(err)
		}
		if err := st.RecordAudit(AuditEntry{
			At: createdAt, Action: AuditEnrollToken, MachineID: receipt.MachineID,
			Subject: receipt.DisplayName, IdempotencyKey: "old-success-key",
			RequestDigest: "sha256:old-success", SourceAddr: "historical-test", OK: true,
			Detail: operatorEnrollTokenSuccessDetail(receipt),
		}); err != nil {
			t.Fatal(err)
		}
		result, err := st.ApplyOperatorEnrollToken(OperatorEnrollTokenCreateRequest{
			DisplayName: receipt.DisplayName, TTLSeconds: receipt.TTLSeconds,
			PreviewDigest: receipt.PreviewDigest, IdempotencyKey: "old-success-key",
			RequestDigest: "sha256:old-success", Audit: AuditEntry{SourceAddr: "local-test"},
		})
		if err != nil || !result.Replayed || result.SecretAvailable || result.EnrollmentToken != "" ||
			result.MachineID != receipt.MachineID {
			t.Fatalf("historical success replay=%+v err=%v", result, err)
		}
	})

	t.Run("historical rejection", func(t *testing.T) {
		st := newOperatorEnrollTestStore(t)
		const rawHistoricalDetail = "舊 policy 是 120..43200；corrupt-secret-must-not-replay"
		preview, err := st.PreviewOperatorEnrollToken("historical-rejection", 3600)
		if err != nil {
			t.Fatal(err)
		}
		createdAt := st.nowFn().UTC().Truncate(time.Second)
		storedHistoricalDetail := operatorEnrollTokenStoredRejectionDetail(
			OperatorCodeBadTTL, rawHistoricalDetail)
		if _, err := st.db.Exec(`INSERT INTO operator_idempotency
		 (idempotency_key,operation,request_digest,outcome,error_code,error_detail,created_at)
		 VALUES (?,?,?,'rejected',?,?,?)`, "old-rejection-key", operatorEnrollTokenOperation,
			"sha256:old-rejection", OperatorCodeBadTTL,
			storedHistoricalDetail, fmtTime(createdAt)); err != nil {
			t.Fatal(err)
		}
		if err := st.RecordAudit(AuditEntry{
			At: createdAt, Action: AuditEnrollToken, Subject: preview.DisplayName,
			IdempotencyKey: "old-rejection-key", RequestDigest: "sha256:old-rejection",
			SourceAddr: "historical-test", OK: false, Detail: storedHistoricalDetail,
		}); err != nil {
			t.Fatal(err)
		}
		_, err = st.ApplyOperatorEnrollToken(OperatorEnrollTokenCreateRequest{
			DisplayName: preview.DisplayName, TTLSeconds: preview.TTLSeconds,
			PreviewDigest: preview.PreviewDigest, IdempotencyKey: "old-rejection-key",
			RequestDigest: "sha256:old-rejection", Audit: AuditEntry{SourceAddr: "local-test"},
		})
		if !errors.Is(err, ErrBadEnrollTTL) {
			t.Fatalf("historical rejection changed meaning: %v", err)
		}
		var rejection *OperatorRequestError
		if !errors.As(err, &rejection) || !rejection.Replayed ||
			rejection.Detail != mustHistoricalOperatorEnrollRejectionDetail(t, OperatorCodeBadTTL) {
			t.Fatalf("historical rejection=%+v err=%v", rejection, err)
		}
		if strings.Contains(err.Error(), rawHistoricalDetail) {
			t.Fatalf("raw historical rejection escaped: %v", err)
		}
		entries, auditErr := st.Audit("", 10)
		if auditErr != nil || len(entries) != 2 || strings.Contains(entries[0].Detail, rawHistoricalDetail) ||
			!strings.Contains(entries[0].Detail, mustHistoricalOperatorEnrollRejectionDetail(t, OperatorCodeBadTTL)) ||
			entries[1].Detail != storedHistoricalDetail {
			t.Fatalf("historical rejection audit=%+v err=%v", entries, auditErr)
		}
	})
}

func mustHistoricalOperatorEnrollRejectionDetail(t *testing.T, code string) string {
	t.Helper()
	detail, ok := historicalOperatorEnrollTokenRejectionDetail(code)
	if !ok {
		t.Fatalf("no historical detail for %q", code)
	}
	return detail
}

func mustCanonicalOperatorEnrollRejectionDetail(t *testing.T, code string) string {
	t.Helper()
	detail, ok := canonicalOperatorEnrollTokenRejectionDetail(code)
	if !ok {
		t.Fatalf("no canonical detail for %q", code)
	}
	return detail
}

func TestOperatorEnrollTokenRollsBackWhenAtomicAuditFails(t *testing.T) {
	st := newOperatorEnrollTestStore(t)
	preview, err := st.PreviewOperatorEnrollToken("onode-audit", 3600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`DROP TABLE audit_log`); err != nil {
		t.Fatal(err)
	}
	result, err := st.ApplyOperatorEnrollToken(operatorEnrollTestRequest(preview))
	if err == nil || !strings.Contains(err.Error(), "audit") || result.EnrollmentToken != "" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	for _, query := range []string{
		`SELECT COUNT(*) FROM machine_registry`,
		`SELECT COUNT(*) FROM enrollment_tokens`,
		`SELECT COUNT(*) FROM operator_idempotency`,
	} {
		var count int
		if err := st.db.QueryRow(query).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("transaction survived failed audit for %q: %d", query, count)
		}
	}
}

func TestOperatorEnrollTokenCachedReceiptCannotContainSecretFields(t *testing.T) {
	st := newOperatorEnrollTestStore(t)
	preview, err := st.PreviewOperatorEnrollToken("onode-corrupt", 3600)
	if err != nil {
		t.Fatal(err)
	}
	req := operatorEnrollTestRequest(preview)
	createdAt := st.nowFn().UTC().Truncate(time.Second)
	receipt := operatorEnrollTokenReceipt{
		MachineID: "machine-from-cache", DisplayName: req.DisplayName,
		CreatedAt: createdAt, ExpiresAt: createdAt.Add(time.Hour),
		TTLSeconds: req.TTLSeconds, PreviewDigest: req.PreviewDigest,
	}
	raw, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a future regression or corrupt ledger that cached a plaintext
	// field. The replay decoder must fail closed rather than deserialize around
	// it and normalize secret persistence.
	corrupt := strings.TrimSuffix(string(raw), "}") + `,"enrollment_token":"must-not-escape"}`
	if _, err := st.db.Exec(`INSERT INTO operator_idempotency
	 (idempotency_key,operation,request_digest,outcome,response_json,created_at)
	 VALUES (?,?,?,'ok',?,?)`, req.IdempotencyKey, operatorEnrollTokenOperation,
		req.RequestDigest, corrupt, fmtTime(st.nowFn())); err != nil {
		t.Fatal(err)
	}
	result, err := st.ApplyOperatorEnrollToken(req)
	if err == nil || result.EnrollmentToken != "" || !result.Audited ||
		strings.Contains(err.Error(), "must-not-escape") {
		t.Fatalf("corrupt cache result=%+v err=%v", result, err)
	}
	entries, auditErr := st.Audit("", 10)
	if auditErr != nil || len(entries) != 1 {
		t.Fatalf("corrupt cache audit=%+v err=%v", entries, auditErr)
	}
	entry := entries[0]
	if entry.OK || entry.MachineID != "" || entry.IsOperatorReplay() ||
		entry.Detail != operatorEnrollTokenCacheInvalidDetail ||
		strings.Contains(entry.Detail, "must-not-escape") {
		t.Fatalf("corrupt cache wrote misleading audit=%+v", entry)
	}
}

func TestOperatorEnrollTokenRejectsUnknownCachedRejectionWithoutLeakingDetail(t *testing.T) {
	st := newOperatorEnrollTestStore(t)
	preview, err := st.PreviewOperatorEnrollToken("unknown-rejection", 3600)
	if err != nil {
		t.Fatal(err)
	}
	req := operatorEnrollTestRequest(preview)
	const corruptDetail = "credential-looking-secret-must-not-escape"
	if _, err := st.db.Exec(`INSERT INTO operator_idempotency
	 (idempotency_key,operation,request_digest,outcome,error_code,error_detail,created_at)
	 VALUES (?,?,?,'rejected',?,?,?)`, req.IdempotencyKey, operatorEnrollTokenOperation,
		req.RequestDigest, "FORGED_REJECTION_CODE", corruptDetail,
		fmtTime(st.nowFn().UTC().Truncate(time.Second))); err != nil {
		t.Fatal(err)
	}
	result, err := st.ApplyOperatorEnrollToken(req)
	if err == nil || !result.Audited || result.EnrollmentToken != "" ||
		strings.Contains(err.Error(), corruptDetail) || strings.Contains(err.Error(), "FORGED_REJECTION_CODE") {
		t.Fatalf("unknown cached rejection result=%+v err=%v", result, err)
	}
	var rejection *OperatorRequestError
	if errors.As(err, &rejection) {
		t.Fatalf("cache corruption became a domain rejection: %+v", rejection)
	}
	entries, auditErr := st.Audit("", 10)
	if auditErr != nil || len(entries) != 1 || entries[0].OK || entries[0].IsOperatorReplay() ||
		entries[0].Detail != operatorEnrollTokenCacheInvalidDetail ||
		strings.Contains(entries[0].Detail, corruptDetail) {
		t.Fatalf("unknown cached rejection audit=%+v err=%v", entries, auditErr)
	}
}

func TestOperatorEnrollTokenRejectsOrphanedAllowlistedRejectionCache(t *testing.T) {
	st := newOperatorEnrollTestStore(t)
	preview, err := st.PreviewOperatorEnrollToken("orphaned-rejection", 3600)
	if err != nil {
		t.Fatal(err)
	}
	req := operatorEnrollTestRequest(preview)
	createdAt := st.nowFn().UTC().Truncate(time.Second)
	storedDetail := operatorEnrollTokenStoredRejectionDetail(
		OperatorCodeBadTTL, "舊 policy 的原始拒絕文案")
	if _, err := st.db.Exec(`INSERT INTO operator_idempotency
	 (idempotency_key,operation,request_digest,outcome,error_code,error_detail,created_at)
	 VALUES (?,?,?,'rejected',?,?,?)`, req.IdempotencyKey, operatorEnrollTokenOperation,
		req.RequestDigest, OperatorCodeBadTTL, storedDetail, fmtTime(createdAt)); err != nil {
		t.Fatal(err)
	}
	result, err := st.ApplyOperatorEnrollToken(req)
	if err == nil || !result.Audited || result.EnrollmentToken != "" {
		t.Fatalf("orphaned rejection result=%+v err=%v", result, err)
	}
	var rejection *OperatorRequestError
	if errors.As(err, &rejection) {
		t.Fatalf("orphaned cache became replayed domain rejection=%+v", rejection)
	}
	entries, auditErr := st.Audit("", 10)
	if auditErr != nil || len(entries) != 1 || entries[0].Detail != operatorEnrollTokenCacheInvalidDetail ||
		entries[0].IsOperatorReplay() || strings.Contains(entries[0].Detail, storedDetail) {
		t.Fatalf("orphaned rejection audit=%+v err=%v", entries, auditErr)
	}
}

func TestOperatorEnrollTokenDetectsSingleFieldCachedRejectionCodeCorruption(t *testing.T) {
	st := newOperatorEnrollTestStore(t)
	req := OperatorEnrollTokenCreateRequest{
		DisplayName: "code-corruption", TTLSeconds: 3600,
		IdempotencyKey: "code-corruption-key", RequestDigest: "sha256:code-corruption",
		Audit: AuditEntry{SourceAddr: "local-test"},
	}
	if _, err := st.ApplyOperatorEnrollToken(req); !errors.Is(err, ErrPreviewRequired) {
		t.Fatalf("seed rejection=%v", err)
	}
	// Change only an allowlisted code. The stable code marker in the original
	// error_detail/audit must prevent this from becoming a valid BAD_TTL replay.
	if _, err := st.db.Exec(`UPDATE operator_idempotency SET error_code=? WHERE idempotency_key=?`,
		OperatorCodeBadTTL, req.IdempotencyKey); err != nil {
		t.Fatal(err)
	}
	result, err := st.ApplyOperatorEnrollToken(req)
	if err == nil || !result.Audited || result.EnrollmentToken != "" {
		t.Fatalf("code corruption result=%+v err=%v", result, err)
	}
	var rejection *OperatorRequestError
	if errors.As(err, &rejection) {
		t.Fatalf("code corruption replayed as domain rejection=%+v", rejection)
	}
	entries, auditErr := st.Audit("", 10)
	if auditErr != nil || len(entries) != 2 || entries[0].Detail != operatorEnrollTokenCacheInvalidDetail ||
		entries[0].IsOperatorReplay() ||
		!operatorEnrollTokenStoredRejectionMatchesCode(entries[1].Detail, OperatorCodePreviewRequired) {
		t.Fatalf("code corruption audit=%+v err=%v", entries, auditErr)
	}
}

func TestOperatorEnrollTokenReplayCorruptMachineIdentityHasNoRecoveryAudit(t *testing.T) {
	st := newOperatorEnrollTestStore(t)
	preview, err := st.PreviewOperatorEnrollToken("receipt-identity", 3600)
	if err != nil {
		t.Fatal(err)
	}
	req := operatorEnrollTestRequest(preview)
	first, err := st.ApplyOperatorEnrollToken(req)
	if err != nil {
		t.Fatal(err)
	}
	var raw string
	if err := st.db.QueryRow(`SELECT response_json FROM operator_idempotency WHERE idempotency_key=?`,
		req.IdempotencyKey).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	receipt, err := decodeOperatorEnrollTokenReceipt(raw)
	if err != nil {
		t.Fatal(err)
	}
	const forgedMachineID = "forged-machine-with-matching-created-at"
	if _, err := st.db.Exec(`INSERT INTO machine_registry
	 (machine_id,display_name,expected,created_at) VALUES (?,?,1,?)`,
		forgedMachineID, receipt.DisplayName, fmtTime(receipt.CreatedAt)); err != nil {
		t.Fatal(err)
	}
	receipt.MachineID = forgedMachineID
	corruptRaw, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`UPDATE operator_idempotency SET response_json=? WHERE idempotency_key=?`,
		string(corruptRaw), req.IdempotencyKey); err != nil {
		t.Fatal(err)
	}

	result, err := st.ApplyOperatorEnrollToken(req)
	if err == nil || !result.Audited || result.MachineID != "" || result.EnrollmentToken != "" ||
		result.Replayed || result.RecoveryRequired || result.RecoveryAction != "" {
		t.Fatalf("corrupt identity result=%+v err=%v", result, err)
	}
	entries, auditErr := st.Audit("", 10)
	if auditErr != nil || len(entries) != 2 {
		t.Fatalf("corrupt identity audit=%+v err=%v", entries, auditErr)
	}
	invalid := entries[0]
	if invalid.OK || invalid.MachineID != "" || invalid.IsOperatorReplay() ||
		invalid.Detail != operatorEnrollTokenCacheInvalidDetail ||
		strings.Contains(invalid.Detail, forgedMachineID) || strings.Contains(invalid.Detail, first.EnrollmentToken) {
		t.Fatalf("corrupt identity wrote misleading recovery audit=%+v", invalid)
	}
	if entries[1].MachineID != first.MachineID || !entries[1].OK {
		t.Fatalf("original success evidence changed=%+v", entries[1])
	}
}

func TestOperatorEnrollTokenReplayRejectsInconsistentReceipt(t *testing.T) {
	offset := time.FixedZone("receipt-offset", 8*60*60)
	tests := []struct {
		name        string
		consequence string
		mutate      func(*operatorEnrollTokenReceipt)
	}{
		{
			name: "ttl seconds",
			consequence: "回放告訴 operator 這張憑證的存活期是一個數字，" +
				"實際發出的那張卻是另一個",
			mutate: func(receipt *operatorEnrollTokenReceipt) {
				receipt.TTLSeconds = receipt.TTLSeconds + 60
				receipt.ExpiresAt = receipt.CreatedAt.Add(time.Duration(receipt.TTLSeconds) * time.Second)
			},
		},
		{
			name:        "preview digest",
			consequence: "operator 確認的是另一份 preview，回放卻把它當成同一次決定回給他",
			mutate: func(receipt *operatorEnrollTokenReceipt) {
				receipt.PreviewDigest = receipt.PreviewDigest + "-other"
			},
		},
		{
			name: "created_at offset",
			consequence: "created_at 的時刻對得上帳本，序列化形狀卻是 Hub 從來不會寫出的那一種；" +
				"帳本比對走 fmtTime 看不出差別，於是 Hub 替一份自己不認得的收據背書，當成 operator 當初那次決定回放出去",
			mutate: func(receipt *operatorEnrollTokenReceipt) {
				receipt.CreatedAt = receipt.CreatedAt.In(offset)
			},
		},
		{
			name: "expires_at offset",
			consequence: "expires_at 的時刻雖然對得上帳本，序列化形狀卻不是 Hub 會寫的那一種；" +
				"fmtTime 帳本比對看不出差別，Hub 會把不是自己寫的到期宣告，當成當初發出的那張憑證的到期時間",
			mutate: func(receipt *operatorEnrollTokenReceipt) {
				receipt.ExpiresAt = receipt.ExpiresAt.In(offset)
			},
		},
		{
			name:        "expires_at drift",
			consequence: "到期時間跟「建立時間＋ttl」對不起來，operator 無法從收據推算這張憑證何時失效",
			mutate: func(receipt *operatorEnrollTokenReceipt) {
				receipt.ExpiresAt = receipt.ExpiresAt.Add(time.Second)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			st := newOperatorEnrollTestStore(t)
			preview, err := st.PreviewOperatorEnrollToken("receipt-"+test.name, 3600)
			if err != nil {
				t.Fatal(err)
			}
			req := operatorEnrollTestRequest(preview)
			if _, err := st.ApplyOperatorEnrollToken(req); err != nil {
				t.Fatal(err)
			}
			var raw string
			if err := st.db.QueryRow(`SELECT response_json FROM operator_idempotency WHERE idempotency_key=?`,
				req.IdempotencyKey).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			receipt, err := decodeOperatorEnrollTokenReceipt(raw)
			if err != nil {
				t.Fatal(err)
			}
			test.mutate(&receipt)
			changed, err := json.Marshal(receipt)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := st.db.Exec(`UPDATE operator_idempotency SET response_json=? WHERE idempotency_key=?`,
				string(changed), req.IdempotencyKey); err != nil {
				t.Fatal(err)
			}
			// 刻意拆掉 evidence 層的 detail 這條臂，讓判決落在收據欄位驗證。
			if _, err := st.db.Exec(`UPDATE audit_log SET detail=? WHERE idempotency_key=? AND outcome='ok'`,
				operatorEnrollTokenSuccessDetail(receipt), req.IdempotencyKey); err != nil {
				t.Fatal(err)
			}
			before := countRows(t, st, `SELECT COUNT(*) FROM audit_log`)

			result, err := st.ApplyOperatorEnrollToken(req)
			if err == nil {
				t.Errorf("err=%v, expected a rejection of the cached receipt; %s", err, test.consequence)
			}
			if result.Replayed || result.EnrollmentToken != "" || result.MachineID != "" ||
				result.RecoveryRequired || result.RecoveryAction != "" {
				t.Errorf("Replayed=%t, EnrollmentToken=%q, MachineID=%q, RecoveryRequired=%t, RecoveryAction=%q; "+
					"expected false, empty, empty, false, empty; Hub 拒絕了這份快取，卻仍把憑證、機器代號或補救"+
					"指示交了出去——operator 會照著一個 Hub 自己不承認的決定去動作。",
					result.Replayed, result.EnrollmentToken, result.MachineID, result.RecoveryRequired, result.RecoveryAction)
			}
			after := countRows(t, st, `SELECT COUNT(*) FROM audit_log`)
			entries, auditErr := st.Audit("", 1)
			if auditErr != nil {
				t.Fatal(auditErr)
			}
			delta := after - before
			actualDetail := "<missing>"
			if len(entries) > 0 {
				actualDetail = entries[0].Detail
			}
			if delta != 1 || actualDetail != operatorEnrollTokenCacheInvalidDetail {
				t.Errorf("audit row delta=%d, Detail=%q; expected delta=1, Detail=%q; 被拒絕的重放要麼沒有留下紀錄、要麼留下的紀錄講的是別件事，operator 事後查不到這次重放為什麼沒有生效。",
					delta, actualDetail, operatorEnrollTokenCacheInvalidDetail)
			}
		})
	}
}

func TestOperatorEnrollTokenReplayRejectsCacheRowTimeThatLeftItsReceipt(t *testing.T) {
	const consequence = "operator_idempotency 那一列的 created_at 離開了它存的那份收據，" +
		"這一列不再能證明它存的是那一次決定"
	st := newOperatorEnrollTestStore(t)
	preview, err := st.PreviewOperatorEnrollToken("receipt-cache-row-time", 3600)
	if err != nil {
		t.Fatal(err)
	}
	req := operatorEnrollTestRequest(preview)
	if _, err := st.ApplyOperatorEnrollToken(req); err != nil {
		t.Fatal(err)
	}
	var cachedCreatedAt string
	if err := st.db.QueryRow(`SELECT created_at FROM operator_idempotency WHERE idempotency_key=?`,
		req.IdempotencyKey).Scan(&cachedCreatedAt); err != nil {
		t.Fatal(err)
	}
	wantCreatedAt := fmtTime(st.nowFn().UTC().Truncate(time.Second))
	if cachedCreatedAt != wantCreatedAt {
		t.Errorf("created_at=%q, expected %q; Hub 寫進 operator_idempotency 的 created_at 原本就不等於收據時間，"+
			"後面的加一秒沒有拆開兩者，這支測試會變成空的、守衛失效也看不出來。", cachedCreatedAt, wantCreatedAt)
	}
	changedCreatedAt := fmtTime(st.nowFn().UTC().Truncate(time.Second).Add(time.Second))
	if _, err := st.db.Exec(`UPDATE operator_idempotency SET created_at=? WHERE idempotency_key=?`,
		changedCreatedAt, req.IdempotencyKey); err != nil {
		t.Fatal(err)
	}
	before := countRows(t, st, `SELECT COUNT(*) FROM audit_log`)

	result, err := st.ApplyOperatorEnrollToken(req)
	if err == nil {
		t.Errorf("err=%v, expected a rejection of the cached receipt; %s", err, consequence)
	}
	if result.Replayed || result.EnrollmentToken != "" || result.MachineID != "" ||
		result.RecoveryRequired || result.RecoveryAction != "" {
		t.Errorf("Replayed=%t, EnrollmentToken=%q, MachineID=%q, RecoveryRequired=%t, RecoveryAction=%q; "+
			"expected false, empty, empty, false, empty; Hub 拒絕了這份快取，卻仍把憑證、機器代號或補救"+
			"指示交了出去——operator 會照著一個 Hub 自己不承認的決定去動作。",
			result.Replayed, result.EnrollmentToken, result.MachineID, result.RecoveryRequired, result.RecoveryAction)
	}
	after := countRows(t, st, `SELECT COUNT(*) FROM audit_log`)
	entries, auditErr := st.Audit("", 1)
	if auditErr != nil {
		t.Fatal(auditErr)
	}
	delta := after - before
	actualDetail := "<missing>"
	if len(entries) > 0 {
		actualDetail = entries[0].Detail
	}
	if delta != 1 || actualDetail != operatorEnrollTokenCacheInvalidDetail {
		t.Errorf("audit row delta=%d, Detail=%q; expected delta=1, Detail=%q; 被拒絕的重放要麼沒有留下紀錄、要麼留下的紀錄講的是別件事，operator 事後查不到這次重放為什麼沒有生效。",
			delta, actualDetail, operatorEnrollTokenCacheInvalidDetail)
	}
}

func TestEnrollmentAuditClassificationUsesCanonicalPrefixes(t *testing.T) {
	transport := AuditEntry{
		Action: AuditEnrollToken, Detail: OperatorTransportRejectionPrefix + "BAD_REQUEST",
	}
	replay := AuditEntry{
		Action: AuditEnrollToken, Detail: OperatorIdempotencyReplayPrefix + "redacted receipt",
	}
	if !transport.IsOperatorTransportRejection() || transport.IsOperatorReplay() {
		t.Fatalf("transport classification failed: %+v", transport)
	}
	if replay.IsOperatorTransportRejection() || !replay.IsOperatorReplay() {
		t.Fatalf("replay classification failed: %+v", replay)
	}
	// Revocation is canonical now; retire remains a legacy best-effort action
	// until its own vertical slice migrates.
	legacy := AuditEntry{Action: AuditRetire, Detail: replay.Detail}
	if legacy.IsOperatorTransportRejection() || legacy.IsOperatorReplay() {
		t.Fatalf("non-canonical action inherited prefix classification: %+v", legacy)
	}
}

func TestDecodeOperatorEnrollTokenReceiptRejectsAmbiguousShapes(t *testing.T) {
	receipt := operatorEnrollTokenReceipt{
		MachineID: "machine", DisplayName: "onode",
		CreatedAt:  time.Date(2026, 9, 7, 20, 0, 0, 0, time.UTC),
		ExpiresAt:  time.Date(2026, 9, 7, 21, 0, 0, 0, time.UTC),
		TTLSeconds: 3600, PreviewDigest: "sha256:preview",
	}
	raw, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := decodeOperatorEnrollTokenReceipt(string(raw)); err != nil || got != receipt {
		t.Fatalf("valid receipt=%+v err=%v", got, err)
	}
	tests := map[string]string{
		"unknown secret field": strings.TrimSuffix(string(raw), "}") + `,"enrollment_token":"secret"}`,
		"duplicate field":      strings.TrimSuffix(string(raw), "}") + `,"machine_id":"other"}`,
		"null field":           strings.Replace(string(raw), `"preview_digest":"sha256:preview"`, `"preview_digest":null`, 1),
		"missing field":        strings.Replace(string(raw), `,"preview_digest":"sha256:preview"`, "", 1),
		"trailing json":        string(raw) + `{}`,
	}
	for name, malformed := range tests {
		t.Run(name, func(t *testing.T) {
			if got, err := decodeOperatorEnrollTokenReceipt(malformed); err == nil {
				t.Fatalf("accepted malformed receipt=%+v raw=%s", got, malformed)
			}
		})
	}
}
