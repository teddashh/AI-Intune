package store

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func tailnetIgnoreStoreRequest(now time.Time) OperatorTailnetPeerIgnoreRequest {
	return OperatorTailnetPeerIgnoreRequest{
		PeerID: "node-stable-1", Hostname: "workstation", Action: "ignore",
		ExpiresAt: now.Add(30 * 24 * time.Hour), ExpectedRevision: 0,
		ConfirmHostname: "workstation", PreviewDigest: "sha256:" + strings.Repeat("a", 64),
		CurrentPreviewDigest: "sha256:" + strings.Repeat("a", 64), Reason: "personal device",
		IdempotencyKey: "tailnet-ignore-1", RequestDigest: "sha256:" + strings.Repeat("b", 64),
		CreatedBy: "tailscale-user:42",
		Audit: AuditEntry{
			SourceAddr: "100.64.0.7", AuthSubject: "tailscale-user:42",
			AuthCapability: "example.com/cap/clawctl-admin", SourceKind: "operator-api",
		},
	}
}

func TestOperatorTailnetPeerIgnoreApplyReplayAndUnignoreAreAtomic(t *testing.T) {
	st := newTestStore(t)
	now := time.Date(2026, 9, 10, 18, 0, 0, 0, time.UTC)
	st.nowFn = func() time.Time { return now }
	req := tailnetIgnoreStoreRequest(now)

	first, err := st.ApplyOperatorTailnetPeerIgnore(req)
	if err != nil || !first.Ignored || first.PreviousIgnored || first.Revision != 1 || first.Replayed || !first.Audited {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	stored, found, err := st.TailnetPeerIgnore(req.PeerID, now)
	if err != nil || !found || !stored.Active || stored.Hostname != req.Hostname ||
		stored.Reason != req.Reason || stored.Revision != 1 || stored.CreatedBy != req.CreatedBy {
		t.Fatalf("stored=%+v found=%v err=%v", stored, found, err)
	}

	replay, err := st.ApplyOperatorTailnetPeerIgnore(req)
	if err != nil || !replay.Replayed || replay.Revision != first.Revision || !replay.Ignored || !replay.Audited {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}
	var rules, receipts, audits int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM tailnet_peer_ignores`).Scan(&rules); err != nil {
		t.Fatal(err)
	}
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM operator_idempotency WHERE idempotency_key=?`, req.IdempotencyKey).Scan(&receipts); err != nil {
		t.Fatal(err)
	}
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE action=? AND idempotency_key=?`, AuditTailnetPeerIgnore, req.IdempotencyKey).Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if rules != 1 || receipts != 1 || audits != 2 {
		t.Fatalf("rules=%d receipts=%d audits=%d", rules, receipts, audits)
	}

	unignore := req
	unignore.Action = "unignore"
	unignore.ExpiresAt = time.Time{}
	unignore.ExpectedRevision = first.Revision
	unignore.IdempotencyKey = "tailnet-unignore-1"
	unignore.RequestDigest = "sha256:" + strings.Repeat("c", 64)
	removed, err := st.ApplyOperatorTailnetPeerIgnore(unignore)
	if err != nil || removed.Ignored || !removed.PreviousIgnored || !removed.Audited {
		t.Fatalf("removed=%+v err=%v", removed, err)
	}
	if _, found, err := st.TailnetPeerIgnore(req.PeerID, now); err != nil || found {
		t.Fatalf("rule after unignore found=%v err=%v", found, err)
	}
}

func TestOperatorTailnetPeerIgnoreRejectionsAreDurableAndIdempotent(t *testing.T) {
	st := newTestStore(t)
	now := time.Date(2026, 9, 10, 18, 0, 0, 0, time.UTC)
	st.nowFn = func() time.Time { return now }
	req := tailnetIgnoreStoreRequest(now)
	req.ExpectedRevision = 7
	req.IdempotencyKey = "tailnet-stale-rule"
	req.RequestDigest = "sha256:" + strings.Repeat("d", 64)

	_, err := st.ApplyOperatorTailnetPeerIgnore(req)
	var rejection *OperatorRequestError
	if !errors.As(err, &rejection) || rejection.Code != OperatorCodePreconditionFailed || !rejection.Audited || rejection.Replayed {
		t.Fatalf("first rejection=%#v err=%v", rejection, err)
	}
	_, err = st.ApplyOperatorTailnetPeerIgnore(req)
	if !errors.As(err, &rejection) || rejection.Code != OperatorCodePreconditionFailed || !rejection.Audited || !rejection.Replayed {
		t.Fatalf("replayed rejection=%#v err=%v", rejection, err)
	}
	var rules, receipts, failedAudits int
	_ = st.db.QueryRow(`SELECT COUNT(*) FROM tailnet_peer_ignores`).Scan(&rules)
	_ = st.db.QueryRow(`SELECT COUNT(*) FROM operator_idempotency WHERE idempotency_key=?`, req.IdempotencyKey).Scan(&receipts)
	_ = st.db.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE idempotency_key=? AND outcome='failed'`, req.IdempotencyKey).Scan(&failedAudits)
	if rules != 0 || receipts != 1 || failedAudits != 2 {
		t.Fatalf("rules=%d receipts=%d failed audits=%d", rules, receipts, failedAudits)
	}
}

func TestOperatorTailnetPeerIgnoreRejectsCorruptSuccessCache(t *testing.T) {
	st := newTestStore(t)
	now := time.Date(2026, 9, 10, 18, 0, 0, 0, time.UTC)
	st.nowFn = func() time.Time { return now }
	req := tailnetIgnoreStoreRequest(now)
	if _, err := st.ApplyOperatorTailnetPeerIgnore(req); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`UPDATE operator_idempotency SET response_json='{}' WHERE idempotency_key=?`, req.IdempotencyKey); err != nil {
		t.Fatal(err)
	}
	result, err := st.ApplyOperatorTailnetPeerIgnore(req)
	if err == nil || !result.Audited || !strings.Contains(err.Error(), "idempotency cache is invalid") {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	var rules, invalidAudits int
	_ = st.db.QueryRow(`SELECT COUNT(*) FROM tailnet_peer_ignores`).Scan(&rules)
	_ = st.db.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE idempotency_key=? AND outcome='failed' AND detail=?`,
		req.IdempotencyKey, operatorTailnetPeerIgnoreCacheInvalid).Scan(&invalidAudits)
	if rules != 1 || invalidAudits != 1 {
		t.Fatalf("rules=%d invalid audits=%d", rules, invalidAudits)
	}
}

func TestTailnetIgnoredPeersUsesStableIdentityAndOneExpiryClock(t *testing.T) {
	st := newTestStore(t)
	now := time.Date(2026, 9, 10, 18, 0, 0, 0, time.UTC)
	if _, err := st.db.Exec(`INSERT INTO tailnet_ignored(hostname,note,created_at) VALUES(?,?,?)`,
		"legacy-phone", "legacy", fmtTime(now)); err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct {
		id, host string
		expires  time.Time
	}{
		{"Node-Active", "same-name", now.Add(time.Second)},
		{"Node-Expired", "same-name", now},
	} {
		if _, err := st.db.Exec(`INSERT INTO tailnet_peer_ignores
		 (peer_id,hostname,reason,expires_at,revision,created_at,updated_at,created_by)
		 VALUES(?,?,?,?,1,?,?,?)`, item.id, item.host, "test", fmtTime(item.expires), fmtTime(now), fmtTime(now), "test"); err != nil {
			t.Fatal(err)
		}
	}
	ignored, err := st.IgnoredPeersAt(now)
	if err != nil {
		t.Fatal(err)
	}
	if !ignored["legacy-phone"] || !ignored["id:Node-Active"] || ignored["id:Node-Expired"] {
		t.Fatalf("ignored=%v", ignored)
	}
	// The current ledger is stable-ID and case-preserving. It must not revive
	// the legacy hostname collision that hid every peer with the same name.
	if ignored["same-name"] || ignored["id:node-active"] {
		t.Fatal("stable-ID rules leaked into the legacy hostname namespace")
	}
	if count, err := st.TailnetLegacyIgnoredCount(); err != nil || count != 1 {
		t.Fatalf("legacy count=%d err=%v", count, err)
	}
}

func TestOperatorTailnetPeerIgnoreReplayRefusesAReceiptThatClaimsThePeerWasAlreadyIgnored(t *testing.T) {
	st := newTestStore(t)
	now := time.Date(2026, 9, 10, 18, 0, 0, 0, time.UTC)
	st.nowFn = func() time.Time { return now }
	req := tailnetIgnoreStoreRequest(now)

	first, err := st.ApplyOperatorTailnetPeerIgnore(req)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Ignored || first.PreviousIgnored {
		t.Fatalf("first=%+v；不改任何東西時重放本來就會說 false→true，下面的拒絕才歸因得了偽造", first)
	}
	replay, err := st.ApplyOperatorTailnetPeerIgnore(req)
	if err != nil || !replay.Replayed || replay.PreviousIgnored || !replay.Ignored {
		t.Fatalf("replay=%+v err=%v；不改任何東西時重放本來就會說 false→true，下面的拒絕才歸因得了偽造", replay, err)
	}

	// tailnet_operator.go:351-361：共同段未比 PreviousIgnored；只有 unignore 的 else 要求它為 true。
	// tailnet_operator.go:367-372：結果把 receipt.PreviousIgnored 原封不動投影出去。
	// tailnet_operator.go:374-378：人讀段只有 action、ignored、revision；整包 receipt 由 receipt_sha256 綁定。
	// tailnetcmd.go:413-421：CLI 會把這份偽造回條印成 ignored=true→true，也就是「什麼都沒變」，但這次動作才把這台改成忽略。
	// ⚠ 把 validateTailnetPeerIgnoreReceipt 整支焊成 return nil：本測試仍綠、others=[]；這支欄位驗證器全樹沒有任何人守。
	// 既有的 TestOperatorTailnetPeerIgnoreRejectsCorruptSuccessCache 把整包換成 '{}'，會先死在嚴格解碼那一層，碰不到欄位驗證器。
	// 把 tailnetPeerIgnoreSuccessAuditDetail 裡的 json.Marshal(receipt) 換成 json.Marshal(struct{}{})：雜湊變常數、不再綁 receipt，
	// 但寫入端與驗證端仍然一致，比對照樣通過：本測試轉紅、others=[]。
	// 對照組：拿掉 unignore 分支裡 !receipt.PreviousIgnored || 這一條：本測試仍綠、others=[]；也就是說唯一會讀
	// PreviousIgnored 的那一條（unignore 分支）本身也沒人守。
	var raw string
	if err := st.db.QueryRow(`SELECT response_json FROM operator_idempotency WHERE idempotency_key=?`, req.IdempotencyKey).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	const honest = `"previous_ignored":false`
	if count := strings.Count(raw, honest); count != 1 {
		t.Fatalf("previous_ignored 錨點不唯一（count=%d），量到的會是別的東西；response_json=%q", count, raw)
	}
	tampered := strings.Replace(raw, honest, `"previous_ignored":true`, 1)
	update, err := st.db.Exec(`UPDATE operator_idempotency SET response_json=? WHERE idempotency_key=?`, tampered, req.IdempotencyKey)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := update.RowsAffected()
	if err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("updated rows=%d, want 1", rows)
	}

	result, err := st.ApplyOperatorTailnetPeerIgnore(req)
	if err == nil || !strings.Contains(err.Error(), "idempotency cache is invalid") || !result.Audited ||
		result.PreviousIgnored || result.Ignored || result.Revision != 0 {
		t.Fatalf("result=%+v err=%v；operator 會看到 ignored=true→true，也就是「什麼都沒變」，但這次動作才把這台改成忽略", result, err)
	}
	var rules, invalidAudits int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM tailnet_peer_ignores`).Scan(&rules); err != nil {
		t.Fatal(err)
	}
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE idempotency_key=? AND outcome='failed' AND detail=?`,
		req.IdempotencyKey, operatorTailnetPeerIgnoreCacheInvalid).Scan(&invalidAudits); err != nil {
		t.Fatal(err)
	}
	if rules != 1 || invalidAudits != 1 {
		t.Fatalf("rules=%d invalid audits=%d", rules, invalidAudits)
	}
}
