package store

import (
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/model"
)

func newOperatorRevocationFixture(t *testing.T, name string) (*Store, string, string) {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	issuedAt := time.Date(2026, 9, 7, 18, 0, 0, 0, time.UTC)
	st.nowFn = func() time.Time { return issuedAt }
	machineID, token, err := st.CreateEnrollTokenFor(name, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	st.nowFn = func() time.Time { return issuedAt.Add(10 * time.Minute) }
	return st, machineID, token
}

func operatorRevocationRequest(machineID string, preview OperatorEnrollTokenRevocationPreviewResult) OperatorEnrollTokenRevocationRequest {
	return OperatorEnrollTokenRevocationRequest{
		MachineID: machineID, PreviewDigest: preview.PreviewDigest,
		Reason: "ticket exposed", IdempotencyKey: "revoke-test-key",
		RequestDigest: "sha256:canonical-revocation",
		Audit: AuditEntry{
			SourceAddr: "100.64.0.20", AuthSubject: "tailscale-user:20",
			AuthCapability: "example.com/cap/clawctl-admin", SourceKind: "operator-api",
		},
	}
}

func TestOperatorEnrollTokenRevocationReadPreviewAndAtomicCommit(t *testing.T) {
	st, machineID, token := newOperatorRevocationFixture(t, "revoke-fresh")
	status, err := st.OperatorPendingEnrollToken(machineID)
	if err != nil || status.MachineID != machineID || status.DisplayName != "revoke-fresh" ||
		status.TokenExpired || status.TokenCreatedAt.IsZero() || status.TokenExpiresAt.IsZero() {
		t.Fatalf("status=%+v err=%v", status, err)
	}
	preview, err := st.PreviewOperatorEnrollTokenRevocation(machineID)
	if err != nil || preview.MachineID != machineID || preview.PreviewDigest == "" ||
		!preview.RegistryRetained || preview.DenominatorDelta != 0 ||
		preview.ActiveAgentCredentialAffected || preview.TokenExpired {
		t.Fatalf("preview=%+v err=%v", preview, err)
	}
	var beforeKeys, beforeAudits int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM operator_idempotency`).Scan(&beforeKeys); err != nil {
		t.Fatal(err)
	}
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM audit_log`).Scan(&beforeAudits); err != nil {
		t.Fatal(err)
	}
	if beforeKeys != 0 || beforeAudits != 0 {
		t.Fatalf("read/preview wrote keys=%d audits=%d", beforeKeys, beforeAudits)
	}

	req := operatorRevocationRequest(machineID, preview)
	result, err := st.ApplyOperatorEnrollTokenRevocation(req)
	if err != nil || result.MachineID != machineID || result.DisplayName != "revoke-fresh" ||
		result.Replayed || !result.Audited || !result.RegistryRetained ||
		result.DenominatorDelta != 0 || result.ActiveAgentCredentialAffected ||
		result.TokenWasExpired || !result.RevokedAt.Equal(st.nowFn()) {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	for query, want := range map[string]int{
		`SELECT COUNT(*) FROM machine_registry WHERE machine_id='` + machineID + `'`: 1,
		`SELECT COUNT(*) FROM enrollment_tokens WHERE used_by='` + machineID + `'`:   0,
		`SELECT COUNT(*) FROM operator_idempotency`:                                  1,
		`SELECT COUNT(*) FROM audit_log WHERE action='revoke-token'`:                 1,
	} {
		var count int
		if err := st.db.QueryRow(query).Scan(&count); err != nil || count != want {
			t.Fatalf("query=%q count=%d want=%d err=%v", query, count, want, err)
		}
	}
	var receiptJSON, ledgerAt, auditAt, auditDetail string
	if err := st.db.QueryRow(`SELECT response_json,created_at FROM operator_idempotency
	 WHERE idempotency_key=?`, req.IdempotencyKey).Scan(&receiptJSON, &ledgerAt); err != nil {
		t.Fatal(err)
	}
	if err := st.db.QueryRow(`SELECT at,detail FROM audit_log WHERE idempotency_key=?`,
		req.IdempotencyKey).Scan(&auditAt, &auditDetail); err != nil {
		t.Fatal(err)
	}
	if ledgerAt != fmtTime(result.RevokedAt) || auditAt != ledgerAt ||
		strings.Contains(receiptJSON, token) || strings.Contains(auditDetail, token) ||
		strings.Contains(receiptJSON, hashToken(token)) || strings.Contains(auditDetail, hashToken(token)) {
		t.Fatalf("receipt/audit leaked or clock drift receipt=%q detail=%q ledger=%q audit=%q",
			receiptJSON, auditDetail, ledgerAt, auditAt)
	}
	if _, _, err := st.RedeemEnrollToken(token, model.EnrollRequest{
		SchemaVersion: model.SchemaVersion, EnrollToken: token,
		Hostname: "revoke-fresh", OS: "linux", Arch: "amd64",
	}, st.nowFn()); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("revoked token redeem=%v", err)
	}
}

func TestOperatorEnrollTokenRevocationFailsClosedOnMultiplePendingCredentials(t *testing.T) {
	st, machineID, _ := newOperatorRevocationFixture(t, "revoke-ambiguous")
	createdAt := st.nowFn().UTC().Truncate(time.Second)
	if _, err := st.db.Exec(`INSERT INTO enrollment_tokens
	 (token_hash,display_name,created_at,expires_at,used_by) VALUES (?,?,?,?,?)`,
		hashToken("second-pending-secret"), "revoke-ambiguous", fmtTime(createdAt),
		fmtTime(createdAt.Add(time.Hour)), machineID); err != nil {
		t.Fatal(err)
	}

	if _, err := st.OperatorPendingEnrollToken(machineID); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("pending read accepted ambiguous credentials: %v", err)
	}
	if _, err := st.PreviewOperatorEnrollTokenRevocation(machineID); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("preview accepted ambiguous credentials: %v", err)
	}
	req := OperatorEnrollTokenRevocationRequest{
		MachineID: machineID, PreviewDigest: "sha256:" + strings.Repeat("a", 64),
		Reason: "ambiguous ledger", IdempotencyKey: "ambiguous-revoke-key",
		RequestDigest: "sha256:ambiguous-revoke", Audit: AuditEntry{SourceAddr: "test"},
	}
	if _, err := st.ApplyOperatorEnrollTokenRevocation(req); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("apply accepted ambiguous credentials: %v", err)
	}
	var pending, keys, audits int
	_ = st.db.QueryRow(`SELECT COUNT(*) FROM enrollment_tokens WHERE used_by=? AND used_at IS NULL`, machineID).Scan(&pending)
	_ = st.db.QueryRow(`SELECT COUNT(*) FROM operator_idempotency`).Scan(&keys)
	_ = st.db.QueryRow(`SELECT COUNT(*) FROM audit_log`).Scan(&audits)
	if pending != 2 || keys != 0 || audits != 0 {
		t.Fatalf("fail-closed path mutated rows: pending=%d keys=%d audits=%d", pending, keys, audits)
	}
}

func TestOperatorEnrollTokenRevocationReplayAndConflictDoNotMutate(t *testing.T) {
	st, machineID, _ := newOperatorRevocationFixture(t, "revoke-replay")
	preview, err := st.PreviewOperatorEnrollTokenRevocation(machineID)
	if err != nil {
		t.Fatal(err)
	}
	req := operatorRevocationRequest(machineID, preview)
	first, err := st.ApplyOperatorEnrollTokenRevocation(req)
	if err != nil {
		t.Fatal(err)
	}
	st.nowFn = func() time.Time { return first.RevokedAt.Add(24 * time.Hour) }
	replay, err := st.ApplyOperatorEnrollTokenRevocation(req)
	if err != nil || !replay.Replayed || replay.MachineID != first.MachineID ||
		!replay.RevokedAt.Equal(first.RevokedAt) || !replay.Audited {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}
	changed := req
	changed.Reason = "different"
	changed.RequestDigest = "sha256:different"
	if _, err := st.ApplyOperatorEnrollTokenRevocation(changed); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("changed body=%v", err)
	}
	var tokens, keys, audits int
	_ = st.db.QueryRow(`SELECT COUNT(*) FROM enrollment_tokens WHERE used_by=?`, machineID).Scan(&tokens)
	_ = st.db.QueryRow(`SELECT COUNT(*) FROM operator_idempotency`).Scan(&keys)
	_ = st.db.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE action='revoke-token'`).Scan(&audits)
	if tokens != 0 || keys != 1 || audits != 3 {
		t.Fatalf("rows tokens=%d keys=%d audits=%d", tokens, keys, audits)
	}
}

func TestOperatorEnrollTokenRevocationRejectsStalePreviewAndReplaysRejection(t *testing.T) {
	st, machineID, _ := newOperatorRevocationFixture(t, "revoke-stale")
	preview, err := st.PreviewOperatorEnrollTokenRevocation(machineID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`UPDATE enrollment_tokens SET token_hash=? WHERE used_by=?`,
		hashToken("replacement-secret"), machineID); err != nil {
		t.Fatal(err)
	}
	req := operatorRevocationRequest(machineID, preview)
	_, firstErr := st.ApplyOperatorEnrollTokenRevocation(req)
	if !errors.Is(firstErr, ErrPreviewStale) {
		t.Fatalf("first stale=%v", firstErr)
	}
	_, replayErr := st.ApplyOperatorEnrollTokenRevocation(req)
	var rejection *OperatorRequestError
	if !errors.Is(replayErr, ErrPreviewStale) || !errors.As(replayErr, &rejection) ||
		!rejection.Replayed || !rejection.Audited {
		t.Fatalf("replayed stale=%T %+v", replayErr, rejection)
	}
	var tokens int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM enrollment_tokens WHERE used_by=?`, machineID).Scan(&tokens); err != nil || tokens != 1 {
		t.Fatalf("stale deleted token count=%d err=%v", tokens, err)
	}
}

func TestOperatorEnrollTokenRevocationRollsBackDeleteWhenAuditFails(t *testing.T) {
	st, machineID, _ := newOperatorRevocationFixture(t, "revoke-atomic")
	preview, err := st.PreviewOperatorEnrollTokenRevocation(machineID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`DROP TABLE audit_log`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ApplyOperatorEnrollTokenRevocation(operatorRevocationRequest(machineID, preview)); err == nil || !strings.Contains(err.Error(), "audit") {
		t.Fatalf("missing audit error=%v", err)
	}
	var tokens, keys int
	_ = st.db.QueryRow(`SELECT COUNT(*) FROM enrollment_tokens WHERE used_by=?`, machineID).Scan(&tokens)
	_ = st.db.QueryRow(`SELECT COUNT(*) FROM operator_idempotency`).Scan(&keys)
	if tokens != 1 || keys != 0 {
		t.Fatalf("failed transaction tokens=%d keys=%d", tokens, keys)
	}
}

func TestOperatorEnrollTokenRevocationConcurrentSameKeyDeletesOnce(t *testing.T) {
	st, machineID, _ := newOperatorRevocationFixture(t, "revoke-concurrent")
	preview, err := st.PreviewOperatorEnrollTokenRevocation(machineID)
	if err != nil {
		t.Fatal(err)
	}
	req := operatorRevocationRequest(machineID, preview)
	type outcome struct {
		result OperatorEnrollTokenRevocationResult
		err    error
	}
	start := make(chan struct{})
	out := make(chan outcome, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			result, err := st.ApplyOperatorEnrollTokenRevocation(req)
			out <- outcome{result, err}
		}()
	}
	close(start)
	wg.Wait()
	close(out)
	fresh, replay := 0, 0
	for got := range out {
		if got.err != nil {
			t.Fatal(got.err)
		}
		if got.result.Replayed {
			replay++
		} else {
			fresh++
		}
	}
	if fresh != 1 || replay != 1 {
		t.Fatalf("fresh=%d replay=%d", fresh, replay)
	}
}

func TestOperatorEnrollTokenRevocationConcurrentDifferentKeysCacheBothDecisions(t *testing.T) {
	st, machineID, _ := newOperatorRevocationFixture(t, "revoke-concurrent-decisions")
	preview, err := st.PreviewOperatorEnrollTokenRevocation(machineID)
	if err != nil {
		t.Fatal(err)
	}
	reqs := []OperatorEnrollTokenRevocationRequest{
		operatorRevocationRequest(machineID, preview),
		operatorRevocationRequest(machineID, preview),
	}
	reqs[0].IdempotencyKey = "revoke-concurrent-key-a"
	reqs[0].RequestDigest = "sha256:concurrent-revocation-a"
	reqs[0].Reason = "concurrent decision a"
	reqs[1].IdempotencyKey = "revoke-concurrent-key-b"
	reqs[1].RequestDigest = "sha256:concurrent-revocation-b"
	reqs[1].Reason = "concurrent decision b"

	type outcome struct {
		index  int
		result OperatorEnrollTokenRevocationResult
		err    error
	}
	start := make(chan struct{})
	out := make(chan outcome, len(reqs))
	var wg sync.WaitGroup
	for i := range reqs {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			<-start
			result, err := st.ApplyOperatorEnrollTokenRevocation(reqs[index])
			out <- outcome{index: index, result: result, err: err}
		}(i)
	}
	close(start)
	wg.Wait()
	close(out)

	winner, loser := -1, -1
	for got := range out {
		if got.err == nil {
			if got.result.Replayed || !got.result.Audited || winner != -1 {
				t.Fatalf("unexpected fresh outcome=%+v winner=%d", got, winner)
			}
			winner = got.index
			continue
		}
		var rejection *OperatorRequestError
		if !errors.Is(got.err, ErrEnrollTokenNotPending) ||
			!errors.As(got.err, &rejection) || rejection.Code != OperatorCodeEnrollTokenNotPending ||
			rejection.Replayed || !rejection.Audited || loser != -1 {
			t.Fatalf("unexpected rejected outcome=%+v rejection=%+v loser=%d", got, rejection, loser)
		}
		loser = got.index
	}
	if winner == -1 || loser == -1 || winner == loser {
		t.Fatalf("winner=%d loser=%d", winner, loser)
	}

	var tokens, decisions, initialAudits, successfulAudits, rejectedAudits int
	queries := []struct {
		query string
		args  []any
		dst   *int
	}{
		{`SELECT COUNT(*) FROM enrollment_tokens WHERE used_by=? AND used_at IS NULL`, []any{machineID}, &tokens},
		{`SELECT COUNT(*) FROM operator_idempotency WHERE operation=?`, []any{operatorEnrollTokenRevocationOperation(machineID)}, &decisions},
		{`SELECT COUNT(*) FROM audit_log WHERE action=?`, []any{string(AuditRevokeToken)}, &initialAudits},
		{`SELECT COUNT(*) FROM audit_log WHERE action=? AND outcome='ok'`, []any{string(AuditRevokeToken)}, &successfulAudits},
		{`SELECT COUNT(*) FROM audit_log WHERE action=? AND outcome='failed'`, []any{string(AuditRevokeToken)}, &rejectedAudits},
	}
	for _, check := range queries {
		if err := st.db.QueryRow(check.query, check.args...).Scan(check.dst); err != nil {
			t.Fatalf("query=%q: %v", check.query, err)
		}
	}
	if tokens != 0 || decisions != 2 || initialAudits != 2 || successfulAudits != 1 || rejectedAudits != 1 {
		t.Fatalf("tokens=%d decisions=%d audits=%d success=%d rejected=%d",
			tokens, decisions, initialAudits, successfulAudits, rejectedAudits)
	}
	for _, check := range []struct {
		index        int
		ledgerResult string
		auditResult  string
		errorCode    string
	}{
		{index: winner, ledgerResult: "ok", auditResult: "ok"},
		{index: loser, ledgerResult: "rejected", auditResult: "failed", errorCode: OperatorCodeEnrollTokenNotPending},
	} {
		var joined int
		if err := st.db.QueryRow(`SELECT COUNT(*) FROM operator_idempotency i
		 JOIN audit_log a ON a.idempotency_key=i.idempotency_key
		  AND a.request_digest=i.request_digest AND a.at=i.created_at
		 WHERE i.idempotency_key=? AND i.request_digest=? AND i.outcome=?
		  AND COALESCE(i.error_code,'')=? AND a.action=? AND a.outcome=?`,
			reqs[check.index].IdempotencyKey, reqs[check.index].RequestDigest,
			check.ledgerResult, check.errorCode, string(AuditRevokeToken), check.auditResult).Scan(&joined); err != nil {
			t.Fatal(err)
		}
		if joined != 1 {
			t.Fatalf("decision/audit are not atomic for index=%d joined=%d", check.index, joined)
		}
	}

	winnerReplay, err := st.ApplyOperatorEnrollTokenRevocation(reqs[winner])
	if err != nil || !winnerReplay.Replayed || !winnerReplay.Audited {
		t.Fatalf("winner replay=%+v err=%v", winnerReplay, err)
	}
	_, err = st.ApplyOperatorEnrollTokenRevocation(reqs[loser])
	var rejection *OperatorRequestError
	if !errors.Is(err, ErrEnrollTokenNotPending) || !errors.As(err, &rejection) ||
		rejection.Code != OperatorCodeEnrollTokenNotPending || !rejection.Replayed || !rejection.Audited {
		t.Fatalf("loser replay err=%v rejection=%+v", err, rejection)
	}
	var finalAudits int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE action=?`,
		string(AuditRevokeToken)).Scan(&finalAudits); err != nil {
		t.Fatal(err)
	}
	if finalAudits != 4 {
		t.Fatalf("audits after both replays=%d want=4", finalAudits)
	}
}

func TestOperatorEnrollTokenRevocationExpiryBoundaryIsExclusive(t *testing.T) {
	st, machineID, _ := newOperatorRevocationFixture(t, "revoke-expiry-boundary")
	status, err := st.OperatorPendingEnrollToken(machineID)
	if err != nil {
		t.Fatal(err)
	}
	expiresAt := status.TokenExpiresAt
	st.nowFn = func() time.Time { return expiresAt.Add(-time.Nanosecond) }
	before, err := st.PreviewOperatorEnrollTokenRevocation(machineID)
	if err != nil || before.TokenExpired {
		t.Fatalf("preview immediately before expiry=%+v err=%v", before, err)
	}

	st.nowFn = func() time.Time { return expiresAt }
	statusAtBoundary, err := st.OperatorPendingEnrollToken(machineID)
	if err != nil || !statusAtBoundary.TokenExpired || !statusAtBoundary.TokenExpiresAt.Equal(expiresAt) {
		t.Fatalf("status at exclusive expiry boundary=%+v err=%v", statusAtBoundary, err)
	}
	atBoundary, err := st.PreviewOperatorEnrollTokenRevocation(machineID)
	if err != nil || !atBoundary.TokenExpired || !atBoundary.PreviewedAt.Equal(expiresAt) ||
		atBoundary.PreviewDigest != before.PreviewDigest {
		t.Fatalf("preview at exclusive expiry boundary=%+v before=%+v err=%v", atBoundary, before, err)
	}
	result, err := st.ApplyOperatorEnrollTokenRevocation(operatorRevocationRequest(machineID, atBoundary))
	if err != nil || !result.TokenWasExpired || !result.RevokedAt.Equal(expiresAt) ||
		!result.TokenExpiresAt.Equal(expiresAt) || result.Replayed || !result.Audited {
		t.Fatalf("expired revocation=%+v err=%v", result, err)
	}
}

func TestOperatorEnrollTokenRevocationReplayRejectsCorruptOrOrphanedReceipt(t *testing.T) {
	for _, tc := range []struct {
		name    string
		corrupt func(*testing.T, *Store, OperatorEnrollTokenRevocationRequest)
	}{
		{
			name: "unknown cached field",
			corrupt: func(t *testing.T, st *Store, req OperatorEnrollTokenRevocationRequest) {
				var raw string
				if err := st.db.QueryRow(`SELECT response_json FROM operator_idempotency
				 WHERE idempotency_key=?`, req.IdempotencyKey).Scan(&raw); err != nil {
					t.Fatal(err)
				}
				raw = strings.TrimSuffix(raw, "}") + `,"credential":"do-not-leak"}`
				if _, err := st.db.Exec(`UPDATE operator_idempotency SET response_json=?
				 WHERE idempotency_key=?`, raw, req.IdempotencyKey); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "missing original audit",
			corrupt: func(t *testing.T, st *Store, req OperatorEnrollTokenRevocationRequest) {
				if _, err := st.db.Exec(`DELETE FROM audit_log WHERE idempotency_key=?`,
					req.IdempotencyKey); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "new unexplained pending credential",
			corrupt: func(t *testing.T, st *Store, req OperatorEnrollTokenRevocationRequest) {
				now := st.now().UTC().Truncate(time.Second)
				if _, err := st.db.Exec(`INSERT INTO enrollment_tokens
				 (token_hash,display_name,created_at,expires_at,used_by) VALUES (?,?,?,?,?)`,
					hashToken("unexplained-new-token"), "revoke-corrupt", fmtTime(now),
					fmtTime(now.Add(time.Hour)), req.MachineID); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, machineID, _ := newOperatorRevocationFixture(t, "revoke-corrupt")
			preview, err := st.PreviewOperatorEnrollTokenRevocation(machineID)
			if err != nil {
				t.Fatal(err)
			}
			req := operatorRevocationRequest(machineID, preview)
			if _, err := st.ApplyOperatorEnrollTokenRevocation(req); err != nil {
				t.Fatal(err)
			}
			tc.corrupt(t, st, req)
			result, err := st.ApplyOperatorEnrollTokenRevocation(req)
			if err == nil || !result.Audited || result.Replayed ||
				strings.Contains(err.Error(), "do-not-leak") {
				t.Fatalf("corrupt replay result=%+v err=%v", result, err)
			}
			entries, auditErr := st.Audit("", 20)
			if auditErr != nil || len(entries) == 0 ||
				entries[0].Detail != operatorEnrollTokenRevocationCacheInvalid ||
				strings.Contains(entries[0].Detail, "do-not-leak") {
				t.Fatalf("corrupt audit=%+v err=%v", entries, auditErr)
			}
		})
	}
}

func TestOperatorEnrollTokenRevocationAndRedeemAreSerialized(t *testing.T) {
	st, machineID, token := newOperatorRevocationFixture(t, "revoke-race")
	preview, err := st.PreviewOperatorEnrollTokenRevocation(machineID)
	if err != nil {
		t.Fatal(err)
	}
	req := operatorRevocationRequest(machineID, preview)
	start := make(chan struct{})
	var revokeErr, redeemErr error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		_, revokeErr = st.ApplyOperatorEnrollTokenRevocation(req)
	}()
	go func() {
		defer wg.Done()
		<-start
		_, _, redeemErr = st.RedeemEnrollToken(token, model.EnrollRequest{
			SchemaVersion: model.SchemaVersion, EnrollToken: token,
			Hostname: "revoke-race", OS: "linux", Arch: "amd64",
		}, st.nowFn())
	}()
	close(start)
	wg.Wait()
	switch {
	case revokeErr == nil:
		if !errors.Is(redeemErr, ErrTokenInvalid) {
			t.Fatalf("revoke won but redeem=%v", redeemErr)
		}
	case redeemErr == nil:
		if !errors.Is(revokeErr, ErrEnrollTokenNotPending) {
			t.Fatalf("redeem won but revoke=%v", revokeErr)
		}
	default:
		t.Fatalf("neither operation won: revoke=%v redeem=%v", revokeErr, redeemErr)
	}
}
