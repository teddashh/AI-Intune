package store

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

var operatorVerifierTestNow = time.Date(2026, 9, 7, 20, 30, 0, 0, time.UTC)

func newOperatorVerifierTestStore(t *testing.T) *Store {
	t.Helper()
	st := newTestStore(t)
	st.nowFn = func() time.Time { return operatorVerifierTestNow }
	registerDeployMachine(t, st, "peer-machine")
	registerDeployMachine(t, st, "hub-machine")
	registerDeployMachine(t, st, "machine-a")
	return st
}

func operatorVerifierPreviewFor(t *testing.T, st *Store) OperatorVerifierPreviewResult {
	t.Helper()
	preview, err := st.PreviewOperatorVerifier(VerifierKindFleetPeerAgent, "peer", "peer-machine", "")
	if err != nil {
		t.Fatalf("preview verifier registration: %v", err)
	}
	return preview
}

func operatorVerifierCreateRequest(preview OperatorVerifierPreviewResult, key string) OperatorVerifierCreateRequest {
	return OperatorVerifierCreateRequest{
		Kind: preview.Kind, DisplayName: preview.DisplayName,
		FailureDomain: preview.FailureDomain, PreviewDigest: preview.PreviewDigest,
		Reason: "acceptance verifier", IdempotencyKey: key,
		RequestDigest: "sha256:canonical-verifier-request",
		Audit: AuditEntry{
			Reason:     "caller supplied decoration must be overwritten",
			SourceAddr: "100.64.0.10", AuthSubject: "tailscale-user:1",
			AuthCapability: "example.com/cap/clawctl-admin", SourceKind: "operator-api",
		},
	}
}

func TestResolveHubMachineIDUsesActiveLifecycleOnly(t *testing.T) {
	st := newOperatorVerifierTestStore(t)
	if _, err := st.DB().Exec(`UPDATE machine_registry SET expected=0 WHERE machine_id='hub-machine'`); err != nil {
		t.Fatal(err)
	}
	if got, err := st.ResolveHubMachineID("hub-machine"); err != nil || got != "hub-machine" {
		t.Fatalf("active legacy expected=false hub resolution=%q err=%v", got, err)
	}
	if err := st.RetireMachine("hub-machine", operatorVerifierTestNow.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if got, err := st.ResolveHubMachineID("hub-machine"); err != nil || got != "" {
		t.Fatalf("retired hub resolution=%q err=%v", got, err)
	}
}

func TestOperatorVerifierPreviewStatesItsPolicyAndBindsEveryPart(t *testing.T) {
	st := newOperatorVerifierTestStore(t)
	preview := operatorVerifierPreviewFor(t, st)
	if preview.SeparationRule != OperatorVerifierSeparationRule ||
		preview.CredentialDelivery != OperatorVerifierCredentialOneTime ||
		preview.EvidenceRole != JobVerificationRoleIndependent ||
		!preview.RevocationKeepsRow || !preview.GrantsDeploymentGate ||
		!strings.HasPrefix(preview.PreviewDigest, "sha256:") {
		t.Fatalf("preview=%+v", preview)
	}

	// Presentation time must not make a valid preview stale.
	st.nowFn = func() time.Time { return operatorVerifierTestNow.Add(time.Hour) }
	later := operatorVerifierPreviewFor(t, st)
	if later.PreviewDigest != preview.PreviewDigest {
		t.Fatalf("preview digest moved with the clock: %q vs %q",
			later.PreviewDigest, preview.PreviewDigest)
	}
	if !later.PreviewedAt.After(preview.PreviewedAt) {
		t.Fatalf("previewed_at did not advance: %s", later.PreviewedAt)
	}

	for _, other := range []struct{ kind, name, domain string }{
		{VerifierKindExternalJobRunner, "peer", "peer-machine"},
		{VerifierKindFleetPeerAgent, "peer-2", "peer-machine"},
		{VerifierKindFleetPeerAgent, "peer", "hub-machine"},
	} {
		got, err := st.PreviewOperatorVerifier(other.kind, other.name, other.domain, "")
		if err != nil {
			t.Fatalf("preview %+v: %v", other, err)
		}
		if got.PreviewDigest == preview.PreviewDigest {
			t.Fatalf("%+v shares a preview digest with the original", other)
		}
		if got.GrantsDeploymentGate != VerifierKindGrantsDeploymentGate(other.kind) {
			t.Fatalf("%s gate=%t", other.kind, got.GrantsDeploymentGate)
		}
	}
	if rows := countRows(t, st, `SELECT COUNT(*) FROM verifiers`); rows != 0 {
		t.Fatalf("preview wrote %d verifier rows", rows)
	}
	if rows := countRows(t, st, `SELECT COUNT(*) FROM operator_idempotency`); rows != 0 {
		t.Fatalf("preview consumed %d idempotency keys", rows)
	}
	if rows := countRows(t, st, `SELECT COUNT(*) FROM audit_log`); rows != 0 {
		t.Fatalf("preview wrote %d audit rows", rows)
	}
}

func TestOperatorVerifierRejectsEveryIneligibleIntentAndRecordsIt(t *testing.T) {
	for _, test := range []struct {
		name    string
		prepare func(*testing.T, *Store)
		mutate  func(*OperatorVerifierCreateRequest)
		want    string
	}{
		{"unknown kind", nil,
			func(r *OperatorVerifierCreateRequest) { r.Kind = "same_host_unit" },
			OperatorCodeVerifierInvalid},
		{"blank display name", nil,
			func(r *OperatorVerifierCreateRequest) { r.DisplayName = " " },
			OperatorCodeVerifierInvalid},
		{"failure domain not on the registry", nil,
			func(r *OperatorVerifierCreateRequest) { r.FailureDomain = "ghost" },
			OperatorCodeVerifierDomainInvalid},
		{"hub prober claiming the literal hub while the hub host is enrolled", nil,
			func(r *OperatorVerifierCreateRequest) {
				r.Kind, r.FailureDomain = VerifierKindHubProber, VerifierUnenrolledHubDomain
				r.HubMachineID = "hub-machine"
			}, OperatorCodeVerifierDomainInvalid},
		{"missing preview", nil,
			func(r *OperatorVerifierCreateRequest) { r.PreviewDigest = "" },
			OperatorCodePreviewRequired},
		{"stale preview", nil,
			func(r *OperatorVerifierCreateRequest) { r.PreviewDigest = "sha256:stale" },
			OperatorCodeVerifierPreviewStale},
		{"display name already taken",
			func(t *testing.T, st *Store) {
				if _, _, err := st.RegisterVerifier(VerifierKindExternalJobRunner, "peer", "awx", ""); err != nil {
					t.Fatalf("seed the colliding verifier: %v", err)
				}
			}, nil, OperatorCodeVerifierNameTaken},
	} {
		t.Run(test.name, func(t *testing.T) {
			st := newOperatorVerifierTestStore(t)
			request := operatorVerifierCreateRequest(operatorVerifierPreviewFor(t, st), "verifier-reject-key")
			if test.prepare != nil {
				test.prepare(t, st)
			}
			if test.mutate != nil {
				test.mutate(&request)
			}
			result, err := st.ApplyOperatorVerifier(request)
			var rejection *OperatorRequestError
			if !errors.As(err, &rejection) || rejection.Code != test.want {
				t.Fatalf("err=%v want code %s", err, test.want)
			}
			if !rejection.Audited || rejection.Replayed {
				t.Fatalf("rejection=%+v", rejection)
			}
			if result.Credential != "" || result.SecretAvailable {
				t.Fatal("a rejected registration returned a credential")
			}
			if rows := countRows(t, st, `SELECT COUNT(*) FROM verifiers WHERE credential_hash<>'' AND kind<>?`,
				VerifierKindExternalJobRunner); rows != 0 {
				t.Fatalf("rejected registration left %d verifier rows", rows)
			}

			// The same key must replay the committed decision, not re-run it.
			_, replayErr := st.ApplyOperatorVerifier(request)
			var replay *OperatorRequestError
			if !errors.As(replayErr, &replay) || replay.Code != test.want || !replay.Replayed {
				t.Fatalf("replay err=%v want replayed %s", replayErr, test.want)
			}
		})
	}
}

func TestOperatorVerifierDeliversItsCredentialExactlyOnce(t *testing.T) {
	st := newOperatorVerifierTestStore(t)
	preview := operatorVerifierPreviewFor(t, st)
	request := operatorVerifierCreateRequest(preview, "verifier-once-key")

	fresh, err := st.ApplyOperatorVerifier(request)
	if err != nil {
		t.Fatalf("register verifier: %v", err)
	}
	if fresh.Credential == "" || !fresh.SecretAvailable || fresh.Replayed ||
		fresh.RecoveryRequired || fresh.RecoveryAction != "" || fresh.Revision != 1 ||
		fresh.PreviewDigest != preview.PreviewDigest || !fresh.Audited {
		t.Fatalf("fresh=%+v", fresh)
	}
	authenticated, err := st.AuthenticateVerifier(fresh.Credential)
	if err != nil || authenticated.VerifierID != fresh.VerifierID {
		t.Fatalf("minted credential does not authenticate: %+v err=%v", authenticated, err)
	}

	replayed, err := st.ApplyOperatorVerifier(request)
	if err != nil {
		t.Fatalf("replay register: %v", err)
	}
	if replayed.Credential != "" || replayed.SecretAvailable || !replayed.Replayed ||
		!replayed.RecoveryRequired ||
		replayed.RecoveryAction != OperatorVerifierRecoveryRevokeAndRegister ||
		replayed.VerifierID != fresh.VerifierID || replayed.Revision != fresh.Revision {
		t.Fatalf("replay=%+v", replayed)
	}
	if rows := countRows(t, st, `SELECT COUNT(*) FROM verifiers`); rows != 1 {
		t.Fatalf("replay produced %d verifier rows", rows)
	}

	// Everything an operator can read back must describe the verifier without
	// reproducing its credential.
	listed, err := st.ListVerifiers()
	if err != nil || len(listed) != 1 || listed[0].VerifierID != fresh.VerifierID {
		t.Fatalf("list=%+v err=%v", listed, err)
	}
	detail, err := st.GetVerifier(fresh.VerifierID)
	if err != nil || detail.DisplayName != "peer" || detail.State() != VerifierStateActive {
		t.Fatalf("detail=%+v err=%v", detail, err)
	}
	assertCredentialAbsent(t, st, fresh.Credential)
}

func TestOperatorVerifierReplayRejectsReceiptThatLeftItsRegistration(t *testing.T) {
	tests := []struct {
		name        string
		consequence string
		mutate      func(*operatorVerifierReceipt)
	}{
		{
			name:        "preview digest",
			consequence: "重放交回的收據宣稱它答的是另一份確認畫面，operator 拿它去對帳時會以為自己確認過的是別的內容",
			mutate: func(receipt *operatorVerifierReceipt) {
				receipt.PreviewDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
			},
		},
		{
			name: "revision",
			consequence: "註冊收據上的 revision 被改成 7，但庫裡的 verifier 還是註冊當下那一版；operator 下一手會拿 7 當版本條件，" +
				"跟真 revision 對不上而被拒，真正的寫入做不成",
			mutate: func(receipt *operatorVerifierReceipt) { receipt.Revision = 7 },
		},
		{
			name: "created_at left utc",
			consequence: "重放交回的時間不是 Hub 寫進 verifiers、audit_log.at 與 operator_idempotency.created_at 的 canonical 時間；" +
				"operator 拿它對帳會對不上，原樣送進要求 canonical 時間的入口也會被拒",
			mutate: func(receipt *operatorVerifierReceipt) {
				receipt.CreatedAt = receipt.CreatedAt.In(time.FixedZone("UTC+8", 8*60*60))
			},
		},
		{
			name: "created_at carries a fraction",
			consequence: "重放交回的時間不是 Hub 寫進 verifiers、audit_log.at 與 operator_idempotency.created_at 的 canonical 時間；" +
				"operator 拿它對帳會對不上，原樣送進要求 canonical 時間的入口也會被拒",
			mutate: func(receipt *operatorVerifierReceipt) {
				receipt.CreatedAt = receipt.CreatedAt.Add(500 * time.Millisecond)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			st := newOperatorVerifierTestStore(t)
			request := operatorVerifierCreateRequest(operatorVerifierPreviewFor(t, st), "verifier-receipt-"+test.name)
			if _, err := st.ApplyOperatorVerifier(request); err != nil {
				t.Fatal(err)
			}
			var raw string
			if err := st.DB().QueryRow(`SELECT response_json FROM operator_idempotency WHERE idempotency_key=?`,
				request.IdempotencyKey).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			receipt, err := decodeOperatorVerifierReceipt(raw)
			if err != nil {
				t.Fatal(err)
			}
			test.mutate(&receipt)
			changed, err := json.Marshal(receipt)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := st.DB().Exec(`UPDATE operator_idempotency SET response_json=? WHERE idempotency_key=?`,
				string(changed), request.IdempotencyKey); err != nil {
				t.Fatal(err)
			}
			before := countRows(t, st, `SELECT COUNT(*) FROM audit_log`)

			result, err := st.ApplyOperatorVerifier(request)
			if err == nil {
				t.Errorf("err=%v, expected cached receipt rejection; %s", err, test.consequence)
			}
			if result.VerifierID != "" || result.Kind != "" || result.DisplayName != "" || result.FailureDomain != "" || !result.CreatedAt.IsZero() || result.Revision != 0 ||
				result.PreviewDigest != "" || result.Credential != "" || result.SecretAvailable || result.Replayed || result.RecoveryRequired || result.RecoveryAction != "" || !result.Audited {
				t.Errorf("result={VerifierID:%q Kind:%q DisplayName:%q FailureDomain:%q CreatedAt:%s Revision:%d PreviewDigest:%q Credential:%q "+
					"SecretAvailable:%t Replayed:%t RecoveryRequired:%t RecoveryAction:%q Audited:%t}; expected empty operator fields and Audited=true; %s",
					result.VerifierID, result.Kind, result.DisplayName, result.FailureDomain, result.CreatedAt, result.Revision, result.PreviewDigest, result.Credential,
					result.SecretAvailable, result.Replayed, result.RecoveryRequired, result.RecoveryAction, result.Audited, test.consequence)
			}
			after := countRows(t, st, `SELECT COUNT(*) FROM audit_log`)
			entries, auditErr := st.Audit("", 1)
			if auditErr != nil {
				t.Fatal(auditErr)
			}
			actualDetail := "<missing>"
			if len(entries) > 0 {
				actualDetail = entries[0].Detail
			}
			if after-before != 1 || actualDetail != operatorVerifierCacheInvalidDetail {
				t.Errorf("audit row delta=%d, Detail=%q; expected delta=1, Detail=%q; 被拒絕的重放若沒有精確稽核，operator 事後無法確認這份收據為何沒有生效",
					after-before, actualDetail, operatorVerifierCacheInvalidDetail)
			}
		})
	}
}

// assertCredentialAbsent scans every column of every table for the plaintext.
// A leak into a receipt, an audit detail or a stored error string is the one
// failure mode a one-time credential cannot recover from.
func assertCredentialAbsent(t *testing.T, st *Store, credential string) {
	t.Helper()
	if credential == "" {
		t.Fatal("refusing to scan for an empty credential")
	}
	tables, err := st.DB().Query(
		`SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		t.Fatalf("list tables: %v", err)
	}
	var names []string
	for tables.Next() {
		var name string
		if err := tables.Scan(&name); err != nil {
			t.Fatalf("scan table name: %v", err)
		}
		names = append(names, name)
	}
	if err := tables.Err(); err != nil {
		t.Fatalf("finish table list: %v", err)
	}
	tables.Close()
	if len(names) == 0 {
		t.Fatal("schema reported no tables to scan")
	}

	for _, name := range names {
		rows, err := st.DB().Query(`SELECT * FROM ` + name)
		if err != nil {
			t.Fatalf("scan %s: %v", name, err)
		}
		columns, err := rows.Columns()
		if err != nil {
			t.Fatalf("columns of %s: %v", name, err)
		}
		for rows.Next() {
			cells := make([]any, len(columns))
			for i := range cells {
				cells[i] = new(any)
			}
			if err := rows.Scan(cells...); err != nil {
				t.Fatalf("scan row of %s: %v", name, err)
			}
			for i, cell := range cells {
				var text string
				switch value := (*(cell.(*any))).(type) {
				case string:
					text = value
				case []byte:
					text = string(value)
				default:
					continue
				}
				if strings.Contains(text, credential) {
					t.Fatalf("credential plaintext is stored in %s.%s", name, columns[i])
				}
			}
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("finish scan of %s: %v", name, err)
		}
		rows.Close()
	}
}

func TestOperatorVerifierRevocationKeepsEvidenceAndNeedsEveryPrecondition(t *testing.T) {
	st := newOperatorVerifierTestStore(t)
	created, err := st.ApplyOperatorVerifier(
		operatorVerifierCreateRequest(operatorVerifierPreviewFor(t, st), "verifier-revoke-setup"))
	if err != nil {
		t.Fatalf("register verifier: %v", err)
	}
	jobID := newJobForDeployTestOnMachine(t, st, "machine-a")
	setJobDigest(t, st, jobID, verifierTestDigest)
	if err := st.RecordIndependentVerification(IndependentVerificationRequest{
		VerifierID: created.VerifierID, JobID: jobID, RuleID: "health",
		Command: "GET /health", ObservedDigest: verifierTestDigest,
		Passed: true, VerifiedAt: operatorVerifierTestNow,
	}); err != nil {
		t.Fatalf("record independent evidence: %v", err)
	}

	preview, err := st.PreviewOperatorVerifierRevocation(created.VerifierID)
	if err != nil {
		t.Fatalf("preview revocation: %v", err)
	}
	if preview.EvidenceRows != 1 || preview.JobsLosingOnlyProducer != 1 ||
		!preview.RegistryRowRetained || !preview.EvidenceRetained ||
		preview.DisplayNameReusable || !preview.GrantsDeploymentGate || preview.Revision != 1 {
		t.Fatalf("revocation preview=%+v", preview)
	}

	base := func(key string) OperatorVerifierRevocationRequest {
		revision := preview.Revision
		return OperatorVerifierRevocationRequest{
			VerifierID: created.VerifierID, ExpectedRevision: &revision,
			ConfirmDisplayName: preview.DisplayName, PreviewDigest: preview.PreviewDigest,
			Reason: "rotating the peer credential", IdempotencyKey: key,
			RequestDigest: "sha256:canonical-revocation",
			Audit: AuditEntry{
				SourceAddr: "100.64.0.10", AuthSubject: "tailscale-user:1",
				AuthCapability: "example.com/cap/clawctl-admin", SourceKind: "operator-api",
			},
		}
	}
	otherRevision := int64(99)
	for _, test := range []struct {
		name   string
		mutate func(*OperatorVerifierRevocationRequest)
		want   string
	}{
		{"missing revision", func(r *OperatorVerifierRevocationRequest) { r.ExpectedRevision = nil },
			OperatorCodePreconditionRequired},
		{"missing preview", func(r *OperatorVerifierRevocationRequest) { r.PreviewDigest = "" },
			OperatorCodePreviewRequired},
		{"unknown verifier", func(r *OperatorVerifierRevocationRequest) { r.VerifierID = "ghost" },
			OperatorCodeVerifierNotFound},
		{"stale preview", func(r *OperatorVerifierRevocationRequest) { r.PreviewDigest = "sha256:stale" },
			OperatorCodeVerifierPreviewStale},
		{"wrong revision", func(r *OperatorVerifierRevocationRequest) { r.ExpectedRevision = &otherRevision },
			OperatorCodePreconditionFailed},
		{"wrong confirmation", func(r *OperatorVerifierRevocationRequest) { r.ConfirmDisplayName = "Peer" },
			OperatorCodeConfirmationMismatch},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := base("verifier-revoke-" + test.name)
			test.mutate(&request)
			_, err := st.ApplyOperatorVerifierRevocation(request)
			var rejection *OperatorRequestError
			if !errors.As(err, &rejection) || rejection.Code != test.want {
				t.Fatalf("err=%v want %s", err, test.want)
			}
			if !rejection.Audited {
				t.Fatalf("rejection was not recorded: %+v", rejection)
			}
			verifier, err := st.GetVerifier(created.VerifierID)
			if err != nil || verifier.State() != VerifierStateActive || verifier.Revision != 1 {
				t.Fatalf("a rejected revocation changed the row: %+v err=%v", verifier, err)
			}
		})
	}

	request := base("verifier-revoke-apply")
	result, err := st.ApplyOperatorVerifierRevocation(request)
	if err != nil {
		t.Fatalf("revoke verifier: %v", err)
	}
	if result.Replayed || result.PreviousRevision != 1 || result.Revision != 2 ||
		result.EvidenceRows != 1 || result.JobsLosingOnlyProducer != 1 ||
		!result.RegistryRowRetained || !result.EvidenceRetained {
		t.Fatalf("revocation=%+v", result)
	}
	// Audited is how this writer tells the operator service "the decision is
	// already in audit_log". A success that returns false does not lose a row —
	// it makes the service write a second, identical one.
	if !result.Audited {
		t.Fatal("成功的 revoke 回報 Audited=false，service 會再寫一筆一模一樣的 audit")
	}
	replayed, err := st.ApplyOperatorVerifierRevocation(request)
	if err != nil || !replayed.Replayed || replayed.Revision != result.Revision ||
		!replayed.RevokedAt.Equal(result.RevokedAt) ||
		replayed.PreviousRevision != result.PreviousRevision || !replayed.Audited {
		t.Fatalf("replay=%+v err=%v", replayed, err)
	}

	// The row and its evidence both stay: a job page must still be able to name
	// the producer that wrote what it is showing.
	if rows := countRows(t, st, `SELECT COUNT(*) FROM verifiers`); rows != 1 {
		t.Fatalf("revocation left %d registry rows", rows)
	}
	if rows := countRows(t, st, `SELECT COUNT(*) FROM verification_results WHERE verifier_id=?`,
		created.VerifierID); rows != 1 {
		t.Fatalf("revocation left %d evidence rows", rows)
	}
	evidence, err := st.JobReadEvidence(jobID, 10)
	if err != nil {
		t.Fatalf("read job evidence: %v", err)
	}
	if evidence.IndependentVerdict != IndependentProducerRevoked ||
		evidence.IndependentLiveProducers != 0 || len(evidence.Independent) != 1 {
		t.Fatalf("verdict=%q live=%d rows=%d", evidence.IndependentVerdict,
			evidence.IndependentLiveProducers, len(evidence.Independent))
	}

	// A revoked identity cannot be silently taken over by a new registration.
	_, err = st.PreviewOperatorVerifier(VerifierKindFleetPeerAgent, "peer", "peer-machine", "")
	var taken *OperatorRequestError
	if !errors.As(err, &taken) || taken.Code != OperatorCodeVerifierNameTaken {
		t.Fatalf("preview after revocation: %v", err)
	}
}

func TestOperatorVerifierRevocationReplayRejectsReceiptThatLeftItsDecision(t *testing.T) {
	tests := []struct {
		name        string
		consequence string
		mutate      func(*operatorVerifierRevocationReceipt)
	}{
		{
			name:        "preview digest",
			consequence: "重放交回的收據宣稱它答的是另一份確認畫面，operator 拿它去對帳時會以為自己確認過的是別的內容",
			mutate: func(receipt *operatorVerifierRevocationReceipt) {
				receipt.PreviewDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
			},
		},
		{
			name: "revoked_at left utc",
			consequence: "重放交回的時間不是 Hub 寫進 verifiers、audit_log.at 與 operator_idempotency.created_at 的 canonical 時間；" +
				"operator 拿它對帳會對不上，原樣送進要求 canonical 時間的入口也會被拒",
			mutate: func(receipt *operatorVerifierRevocationReceipt) {
				receipt.RevokedAt = receipt.RevokedAt.In(time.FixedZone("UTC+8", 8*60*60))
			},
		},
		{
			name: "revoked_at carries a fraction",
			consequence: "重放交回的時間不是 Hub 寫進 verifiers、audit_log.at 與 operator_idempotency.created_at 的 canonical 時間；" +
				"operator 拿它對帳會對不上，原樣送進要求 canonical 時間的入口也會被拒",
			mutate: func(receipt *operatorVerifierRevocationReceipt) {
				receipt.RevokedAt = receipt.RevokedAt.Add(500 * time.Millisecond)
			},
		},
		// 這是這張表唯一一個能活過 validateOperatorVerifierRevocationReceipt 的
		// 偽造。OperatorVerifierRevocationRequest 沒有這個欄位，所以沒得跟 req 比；
		// preview digest 也刻意不含它（撤銷一個正在寫入的憑證必須永遠做得完），
		// 所以結構檢查只剩 `< 0`。verifiers 沒有這一欄可以對。撤銷之後 live 重算
		// 會算出 0，跟謊言一致，救不了它。現況唯一對得上的耐久副本是原判決那列
		// audit_log.detail，靠 operatorVerifierRevocationSuccessDetail 把
		// jobs_losing_only_producer 印進字串才擋得住；把那個 %d 拿掉，上面三個
		// case 仍然全綠。
		{
			name: "jobs losing only producer",
			consequence: "重放交回的回條把「會失去唯一獨立產出者的 job 數」從 1 說成 0；" +
				"重放正是為了第一次回應沒送到的情形，這份回條可能是 operator 手上唯一一份撤銷文件，" +
				"他會據此判定不必替那個 job 另外安排獨立驗證",
			mutate: func(receipt *operatorVerifierRevocationReceipt) {
				receipt.JobsLosingOnlyProducer = 0
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			st := newOperatorVerifierTestStore(t)
			created, err := st.ApplyOperatorVerifier(
				operatorVerifierCreateRequest(operatorVerifierPreviewFor(t, st), "verifier-revoke-receipt-setup-"+test.name))
			if err != nil {
				t.Fatal(err)
			}
			jobID := newJobForDeployTestOnMachine(t, st, "machine-a")
			setJobDigest(t, st, jobID, verifierTestDigest)
			if err := st.RecordIndependentVerification(IndependentVerificationRequest{
				VerifierID: created.VerifierID, JobID: jobID, RuleID: "health", Command: "GET /health",
				ObservedDigest: verifierTestDigest, Passed: true, VerifiedAt: operatorVerifierTestNow,
			}); err != nil {
				t.Fatal(err)
			}
			preview, err := st.PreviewOperatorVerifierRevocation(created.VerifierID)
			if err != nil {
				t.Fatal(err)
			}
			revision := preview.Revision
			request := OperatorVerifierRevocationRequest{
				VerifierID: created.VerifierID, ExpectedRevision: &revision, ConfirmDisplayName: preview.DisplayName,
				PreviewDigest: preview.PreviewDigest, Reason: "rotating the peer credential", IdempotencyKey: "verifier-revoke-receipt-" + test.name,
				RequestDigest: "sha256:canonical-revocation",
				Audit: AuditEntry{SourceAddr: "100.64.0.10", AuthSubject: "tailscale-user:1",
					AuthCapability: "example.com/cap/clawctl-admin", SourceKind: "operator-api"},
			}
			// 前置：fixture 必須真的造出 1，否則把它改成 0 等於沒改。
			applied, err := st.ApplyOperatorVerifierRevocation(request)
			if err != nil {
				t.Fatal(err)
			}
			if applied.JobsLosingOnlyProducer != 1 {
				t.Fatalf("fixture recorded jobs_losing_only_producer=%d, want 1; without it a later refusal "+
					"cannot be attributed to the forged number", applied.JobsLosingOnlyProducer)
			}
			var raw string
			if err := st.DB().QueryRow(`SELECT response_json FROM operator_idempotency WHERE idempotency_key=?`,
				request.IdempotencyKey).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			receipt, err := decodeOperatorVerifierRevocationReceipt(raw)
			if err != nil {
				t.Fatal(err)
			}
			test.mutate(&receipt)
			changed, err := json.Marshal(receipt)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := st.DB().Exec(`UPDATE operator_idempotency SET response_json=? WHERE idempotency_key=?`,
				string(changed), request.IdempotencyKey); err != nil {
				t.Fatal(err)
			}
			before := countRows(t, st, `SELECT COUNT(*) FROM audit_log`)

			result, err := st.ApplyOperatorVerifierRevocation(request)
			if err == nil {
				t.Errorf("err=%v, expected cached receipt rejection; %s", err, test.consequence)
			}
			if result.VerifierID != "" || result.Kind != "" || result.DisplayName != "" || result.FailureDomain != "" || !result.RevokedAt.IsZero() ||
				result.PreviousRevision != 0 || result.Revision != 0 || result.EvidenceRows != 0 || result.JobsLosingOnlyProducer != 0 || result.RegistryRowRetained ||
				result.EvidenceRetained || result.PreviewDigest != "" || result.Replayed || !result.Audited {
				t.Errorf("result={VerifierID:%q Kind:%q DisplayName:%q FailureDomain:%q RevokedAt:%s PreviousRevision:%d Revision:%d EvidenceRows:%d "+
					"JobsLosingOnlyProducer:%d RegistryRowRetained:%t EvidenceRetained:%t PreviewDigest:%q Replayed:%t Audited:%t}; expected empty operator fields and Audited=true; %s",
					result.VerifierID, result.Kind, result.DisplayName, result.FailureDomain, result.RevokedAt, result.PreviousRevision, result.Revision, result.EvidenceRows,
					result.JobsLosingOnlyProducer, result.RegistryRowRetained, result.EvidenceRetained, result.PreviewDigest, result.Replayed, result.Audited, test.consequence)
			}
			after := countRows(t, st, `SELECT COUNT(*) FROM audit_log`)
			entries, auditErr := st.Audit("", 1)
			if auditErr != nil {
				t.Fatal(auditErr)
			}
			actualDetail := "<missing>"
			if len(entries) > 0 {
				actualDetail = entries[0].Detail
			}
			if after-before != 1 || actualDetail != operatorVerifierRevokeCacheInvalid {
				t.Errorf("audit row delta=%d, Detail=%q; expected delta=1, Detail=%q; 被拒絕的重放若沒有精確稽核，operator 事後無法確認這份撤銷收據為何沒有生效",
					after-before, actualDetail, operatorVerifierRevokeCacheInvalid)
			}
		})
	}
}

func TestOperatorVerifierReplayRejectsCacheRowTimeThatLeftItsReceipt(t *testing.T) {
	const consequence = "operator_idempotency 那一列的 created_at 離開了它存的那份收據，這一列不再能證明它存的是那一次決定"
	st := newOperatorVerifierTestStore(t)
	request := operatorVerifierCreateRequest(operatorVerifierPreviewFor(t, st), "verifier-cache-row-time")
	if _, err := st.ApplyOperatorVerifier(request); err != nil {
		t.Fatal(err)
	}
	var cachedCreatedAt string
	if err := st.DB().QueryRow(`SELECT created_at FROM operator_idempotency WHERE idempotency_key=?`,
		request.IdempotencyKey).Scan(&cachedCreatedAt); err != nil {
		t.Fatal(err)
	}
	if cachedCreatedAt != fmtTime(operatorVerifierTestNow) {
		t.Errorf("cache created_at=%q, expected %q; operator 的原決定已被歸到錯的時間，這一列無法證明它存的是那一次決定",
			cachedCreatedAt, fmtTime(operatorVerifierTestNow))
	}
	if _, err := st.DB().Exec(`UPDATE operator_idempotency SET created_at=? WHERE idempotency_key=?`,
		fmtTime(operatorVerifierTestNow.Add(time.Second)), request.IdempotencyKey); err != nil {
		t.Fatal(err)
	}
	before := countRows(t, st, `SELECT COUNT(*) FROM audit_log`)

	result, err := st.ApplyOperatorVerifier(request)
	if err == nil {
		t.Errorf("err=%v, expected cached receipt rejection; %s", err, consequence)
	}
	if result.VerifierID != "" || result.Kind != "" || result.DisplayName != "" || result.FailureDomain != "" || !result.CreatedAt.IsZero() || result.Revision != 0 ||
		result.PreviewDigest != "" || result.Credential != "" || result.SecretAvailable || result.Replayed || result.RecoveryRequired || result.RecoveryAction != "" || !result.Audited {
		t.Errorf("result={VerifierID:%q Kind:%q DisplayName:%q FailureDomain:%q CreatedAt:%s Revision:%d PreviewDigest:%q Credential:%q "+
			"SecretAvailable:%t Replayed:%t RecoveryRequired:%t RecoveryAction:%q Audited:%t}; expected empty operator fields and Audited=true; %s",
			result.VerifierID, result.Kind, result.DisplayName, result.FailureDomain, result.CreatedAt, result.Revision, result.PreviewDigest, result.Credential,
			result.SecretAvailable, result.Replayed, result.RecoveryRequired, result.RecoveryAction, result.Audited, consequence)
	}
	after := countRows(t, st, `SELECT COUNT(*) FROM audit_log`)
	entries, auditErr := st.Audit("", 1)
	if auditErr != nil {
		t.Fatal(auditErr)
	}
	actualDetail := "<missing>"
	if len(entries) > 0 {
		actualDetail = entries[0].Detail
	}
	if after-before != 1 || actualDetail != operatorVerifierCacheInvalidDetail {
		t.Errorf("audit row delta=%d, Detail=%q; expected delta=1, Detail=%q; 被拒絕的重放若沒有精確稽核，operator 事後無法確認這列為何不能再證明原決定",
			after-before, actualDetail, operatorVerifierCacheInvalidDetail)
	}
}

func TestOperatorVerifierRevocationReplayRejectsCacheRowTimeThatLeftItsReceipt(t *testing.T) {
	const consequence = "operator_idempotency 那一列的 created_at 離開了它存的那份撤銷收據，這一列不再能證明它存的是那一次決定"
	st := newOperatorVerifierTestStore(t)
	created, err := st.ApplyOperatorVerifier(
		operatorVerifierCreateRequest(operatorVerifierPreviewFor(t, st), "verifier-revoke-cache-row-time-setup"))
	if err != nil {
		t.Fatal(err)
	}
	jobID := newJobForDeployTestOnMachine(t, st, "machine-a")
	setJobDigest(t, st, jobID, verifierTestDigest)
	if err := st.RecordIndependentVerification(IndependentVerificationRequest{
		VerifierID: created.VerifierID, JobID: jobID, RuleID: "health", Command: "GET /health",
		ObservedDigest: verifierTestDigest, Passed: true, VerifiedAt: operatorVerifierTestNow,
	}); err != nil {
		t.Fatal(err)
	}
	preview, err := st.PreviewOperatorVerifierRevocation(created.VerifierID)
	if err != nil {
		t.Fatal(err)
	}
	revision := preview.Revision
	request := OperatorVerifierRevocationRequest{
		VerifierID: created.VerifierID, ExpectedRevision: &revision, ConfirmDisplayName: preview.DisplayName,
		PreviewDigest: preview.PreviewDigest, Reason: "rotating the peer credential", IdempotencyKey: "verifier-revoke-cache-row-time",
		RequestDigest: "sha256:canonical-revocation",
		Audit: AuditEntry{SourceAddr: "100.64.0.10", AuthSubject: "tailscale-user:1",
			AuthCapability: "example.com/cap/clawctl-admin", SourceKind: "operator-api"},
	}
	if _, err := st.ApplyOperatorVerifierRevocation(request); err != nil {
		t.Fatal(err)
	}
	var cachedCreatedAt string
	if err := st.DB().QueryRow(`SELECT created_at FROM operator_idempotency WHERE idempotency_key=?`,
		request.IdempotencyKey).Scan(&cachedCreatedAt); err != nil {
		t.Fatal(err)
	}
	if cachedCreatedAt != fmtTime(operatorVerifierTestNow) {
		t.Errorf("cache created_at=%q, expected %q; operator 的原撤銷決定已被歸到錯的時間，這一列無法證明它存的是那一次決定",
			cachedCreatedAt, fmtTime(operatorVerifierTestNow))
	}
	if _, err := st.DB().Exec(`UPDATE operator_idempotency SET created_at=? WHERE idempotency_key=?`,
		fmtTime(operatorVerifierTestNow.Add(time.Second)), request.IdempotencyKey); err != nil {
		t.Fatal(err)
	}
	before := countRows(t, st, `SELECT COUNT(*) FROM audit_log`)

	result, err := st.ApplyOperatorVerifierRevocation(request)
	if err == nil {
		t.Errorf("err=%v, expected cached receipt rejection; %s", err, consequence)
	}
	if result.VerifierID != "" || result.Kind != "" || result.DisplayName != "" || result.FailureDomain != "" || !result.RevokedAt.IsZero() ||
		result.PreviousRevision != 0 || result.Revision != 0 || result.EvidenceRows != 0 || result.JobsLosingOnlyProducer != 0 || result.RegistryRowRetained ||
		result.EvidenceRetained || result.PreviewDigest != "" || result.Replayed || !result.Audited {
		t.Errorf("result={VerifierID:%q Kind:%q DisplayName:%q FailureDomain:%q RevokedAt:%s PreviousRevision:%d Revision:%d EvidenceRows:%d "+
			"JobsLosingOnlyProducer:%d RegistryRowRetained:%t EvidenceRetained:%t PreviewDigest:%q Replayed:%t Audited:%t}; expected empty operator fields and Audited=true; %s",
			result.VerifierID, result.Kind, result.DisplayName, result.FailureDomain, result.RevokedAt, result.PreviousRevision, result.Revision, result.EvidenceRows,
			result.JobsLosingOnlyProducer, result.RegistryRowRetained, result.EvidenceRetained, result.PreviewDigest, result.Replayed, result.Audited, consequence)
	}
	after := countRows(t, st, `SELECT COUNT(*) FROM audit_log`)
	entries, auditErr := st.Audit("", 1)
	if auditErr != nil {
		t.Fatal(auditErr)
	}
	actualDetail := "<missing>"
	if len(entries) > 0 {
		actualDetail = entries[0].Detail
	}
	if after-before != 1 || actualDetail != operatorVerifierRevokeCacheInvalid {
		t.Errorf("audit row delta=%d, Detail=%q; expected delta=1, Detail=%q; 被拒絕的重放若沒有精確稽核，operator 事後無法確認這列為何不能再證明原撤銷決定",
			after-before, actualDetail, operatorVerifierRevokeCacheInvalid)
	}
}

func TestRevokingAnAlreadyRevokedVerifierIsRefusedAndRecorded(t *testing.T) {
	st := newOperatorVerifierTestStore(t)
	created, err := st.ApplyOperatorVerifier(
		operatorVerifierCreateRequest(operatorVerifierPreviewFor(t, st), "verifier-revoke-setup"))
	if err != nil {
		t.Fatalf("register verifier: %v", err)
	}

	preview, err := st.PreviewOperatorVerifierRevocation(created.VerifierID)
	if err != nil {
		t.Fatalf("preview revocation: %v", err)
	}
	previewDigest := preview.PreviewDigest
	previewRevision := preview.Revision
	request := func(key string) OperatorVerifierRevocationRequest {
		return OperatorVerifierRevocationRequest{
			VerifierID: created.VerifierID, ExpectedRevision: &previewRevision,
			ConfirmDisplayName: preview.DisplayName, PreviewDigest: previewDigest,
			Reason: "rotating the peer credential", IdempotencyKey: key,
			RequestDigest: "sha256:canonical-revocation",
			Audit: AuditEntry{
				SourceAddr: "100.64.0.10", AuthSubject: "tailscale-user:1",
				AuthCapability: "example.com/cap/clawctl-admin", SourceKind: "operator-api",
			},
		}
	}

	first, err := st.ApplyOperatorVerifierRevocation(request("verifier-revoke-first"))
	if err != nil {
		t.Fatalf("first revocation: %v", err)
	}

	_, err = st.PreviewOperatorVerifierRevocation(created.VerifierID)
	var previewRejection *OperatorRequestError
	if !errors.As(err, &previewRejection) {
		t.Fatalf("second preview error=%v", err)
	}
	if previewRejection.Code != OperatorCodeVerifierAlreadyRevoked {
		t.Fatalf("second preview code=%q want %q", previewRejection.Code, OperatorCodeVerifierAlreadyRevoked)
	}

	secondRequest := request("verifier-revoke-second")
	_, err = st.ApplyOperatorVerifierRevocation(secondRequest)
	var rejection *OperatorRequestError
	if !errors.As(err, &rejection) {
		t.Fatalf("second revocation error=%v", err)
	}
	if rejection.Code != OperatorCodeVerifierAlreadyRevoked {
		t.Fatalf("second revocation code=%q want %q", rejection.Code, OperatorCodeVerifierAlreadyRevoked)
	}
	if !rejection.Audited || rejection.Replayed {
		t.Fatalf("second revocation audited=%t replayed=%t", rejection.Audited, rejection.Replayed)
	}

	verifier, err := st.GetVerifier(created.VerifierID)
	if err != nil {
		t.Fatalf("get verifier after rejected revocation: %v", err)
	}
	if verifier.RevokedAt == nil {
		t.Fatalf("verifier revoked_at=%v", verifier.RevokedAt)
	}
	if verifier.Revision != first.Revision || !verifier.RevokedAt.Equal(first.RevokedAt) {
		t.Fatalf("verifier revision=%d revoked_at=%s want revision=%d revoked_at=%s",
			verifier.Revision, verifier.RevokedAt, first.Revision, first.RevokedAt)
	}

	if rows := countRows(t, st, `SELECT COUNT(*) FROM operator_idempotency
 WHERE idempotency_key=? AND outcome='rejected' AND error_code=?`,
		"verifier-revoke-second", OperatorCodeVerifierAlreadyRevoked); rows != 1 {
		t.Fatalf("rejected idempotency rows=%d want 1", rows)
	}
	if rows := countRows(t, st, `SELECT COUNT(*) FROM audit_log
 WHERE idempotency_key=? AND outcome='failed'`, "verifier-revoke-second"); rows != 1 {
		t.Fatalf("failed audit rows=%d want 1", rows)
	}

	_, err = st.ApplyOperatorVerifierRevocation(secondRequest)
	var replayed *OperatorRequestError
	if !errors.As(err, &replayed) {
		t.Fatalf("replayed rejection error=%v", err)
	}
	if replayed.Code != OperatorCodeVerifierAlreadyRevoked {
		t.Fatalf("replayed rejection code=%q want %q", replayed.Code, OperatorCodeVerifierAlreadyRevoked)
	}
	if !replayed.Replayed || !replayed.Audited {
		t.Fatalf("replayed rejection replayed=%t audited=%t", replayed.Replayed, replayed.Audited)
	}
	if replayed.Detail != "原 request 當時這個 verifier 已經撤銷" {
		t.Fatalf("replayed rejection detail=%q want %q", replayed.Detail, "原 request 當時這個 verifier 已經撤銷")
	}
}

var verifierRevocationRefusalCases = []struct {
	name         string
	code         string
	want         string
	consequence  string
	breakRequest func(*testing.T, *Store, *OperatorVerifierRevocationRequest)
}{
	{
		name:        "找不到 verifier",
		code:        OperatorCodeVerifierNotFound,
		want:        "找不到這個 verifier",
		consequence: "漂成過去式之後，operator 會以為「那時沒有、現在也許有了」，換一把 key 再送一次撤銷；這個 id 現在仍然不存在，他會在一個不存在的目標上反覆重試，而不是回頭查自己是不是打錯 id。",
		breakRequest: func(_ *testing.T, _ *Store, request *OperatorVerifierRevocationRequest) {
			request.VerifierID = "verifier-does-not-exist"
		},
	},
	{
		name:        "verifier 已經撤銷",
		code:        OperatorCodeVerifierAlreadyRevoked,
		want:        "這個 verifier 已經撤銷過了",
		consequence: "漂成過去式之後，operator 會去翻一筆他從來沒送出過的前次 request，或把這次拒絕當成重放快取；這其實是這把 key 第一次出現。",
		breakRequest: func(t *testing.T, st *Store, request *OperatorVerifierRevocationRequest) {
			first := *request
			first.IdempotencyKey = "verifier-revocation-refusal-already-revoked-setup"
			if _, err := st.ApplyOperatorVerifierRevocation(first); err != nil {
				t.Fatal(err)
			}
		},
	},
	{
		name:        "缺少 preview digest",
		code:        OperatorCodePreviewRequired,
		want:        "preview_digest 不可省略；請先重新預覽",
		consequence: "「請先重新預覽」這個現在該做的下一步消失之後，operator 不知道要去按預覽，他會去查當時那筆 request 的內容。",
		breakRequest: func(_ *testing.T, _ *Store, request *OperatorVerifierRevocationRequest) {
			request.PreviewDigest = ""
		},
	},
	{
		name:        "preview digest 已過期",
		code:        OperatorCodeVerifierPreviewStale,
		want:        "preview_digest 與這個 verifier 目前的狀態不符；請重新預覽",
		consequence: "「與目前的狀態不符」變成「不符合當時的狀態」之後，operator 會以為手上這份 digest 對現在也許還有效，換一把 key 用同一份過期 digest 再送一次。",
		breakRequest: func(_ *testing.T, _ *Store, request *OperatorVerifierRevocationRequest) {
			request.PreviewDigest = "sha256:stale"
		},
	},
	{
		name:        "缺少 expected revision",
		code:        OperatorCodePreconditionRequired,
		want:        "expected_revision 不可省略",
		consequence: "「expected_revision 不可省略」這句講的是現在這筆 request 缺欄位；講成過去式，operator 會去補一筆舊 request 的欄位，而不是把欄位加進手上這一筆。",
		breakRequest: func(_ *testing.T, _ *Store, request *OperatorVerifierRevocationRequest) {
			request.ExpectedRevision = nil
		},
	},
	{
		name:        "expected revision 不符",
		code:        OperatorCodePreconditionFailed,
		want:        "expected_revision 與這個 verifier 目前的 revision 不符；請重新讀取",
		consequence: "「與目前的 revision 不符；請重新讀取」變成「不符合當時的 revision」之後，operator 會用同一個舊 revision 換一把 key 再送，而不是重讀現在的 revision。",
		breakRequest: func(_ *testing.T, _ *Store, request *OperatorVerifierRevocationRequest) {
			wrongRevision := *request.ExpectedRevision + 1
			request.ExpectedRevision = &wrongRevision
		},
	},
	{
		name:        "確認名稱不符",
		code:        OperatorCodeConfirmationMismatch,
		want:        "confirm_display_name 必須與這個 verifier 的 display_name 完全一致",
		consequence: "「必須與這個 verifier 的 display_name 完全一致」變成「與當時的 display_name 不符」之後，operator 會以為名字後來被改過，去查改名紀錄，而不是把現在這台的 display_name 抄對。",
		breakRequest: func(_ *testing.T, _ *Store, request *OperatorVerifierRevocationRequest) {
			request.ConfirmDisplayName = "不是這個名字"
		},
	},
}

func TestTheVerifierRevocationRefusalSaysTheseExactWordsRightNow(t *testing.T) {
	const missingDetailConsequence = "這個 code 在 reject() 裡查不到句子，reject() 會回 \"store: invalid verifier revocation rejection code\"，operator 拿到的是 500 INTERNAL「控制面操作失敗」，他不知道自己哪個欄位送錯，只會重送同一份 request。"

	for _, tc := range verifierRevocationRefusalCases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := canonicalOperatorVerifierRevocationRejectionDetail(tc.code)
			if !ok {
				t.Errorf("%s 的 canonical 文案查找失敗，預期能找到 %q —— %s", tc.code, tc.want, missingDetailConsequence)
				return
			}
			if got != tc.want {
				t.Errorf("%s 的 canonical 文案是 %q，預期 %q —— %s", tc.code, got, tc.want, tc.consequence)
			}
		})
	}
}

func TestAFirstVerifierRevocationRefusalNeverSpeaksOfAnEarlierRequest(t *testing.T) {
	const consequence = "第一次送出的 request 被拒時卻說「原 request」，operator 會去翻一筆他從來沒送出過的前次 request；這把 idempotency key 是第一次出現，那筆 request 不存在。"

	// 不直接要求 canonical != historical，因為兩者未來合理地合併成時間中性文案時，
	// 不應造成測試失敗；真正的不變量是首次拒絕不得使用重放的時間座標。
	for _, tc := range verifierRevocationRefusalCases {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := canonicalOperatorVerifierRevocationRejectionDetail(tc.code)
			if strings.Contains(got, "原 request") {
				t.Errorf("%s 的 canonical 文案是 %q，預期不含 %q —— %s", tc.code, got, "原 request", consequence)
			}
		})
	}
}

func TestEachVerifierRevocationRefusalReachesTheOperatorInThePresentTense(t *testing.T) {
	const (
		key                   = "verifier-revocation-refusal-under-test"
		notRefusedConsequence = "這筆 request 根本沒有被拒絕，operator 收不到任何要修正的欄位，他會當成撤銷成功；缺欄位、拿過期 digest、或對已撤銷的 verifier 再撤一次就這樣落庫，之後沒有人會回頭檢查它。"
		wrongCodeConsequence  = "回的 code 不是擋住他的那一條，operator 會照另一種失敗去修——例如把 revision 對不上當成 digest 過期而跑去重新預覽——他動的不是真正擋住他的那一項，下一次送出的 request 會被同一條閘再擋一次。"
		auditConsequence      = "審計列裡留下的不是 operator 當下讀到的那句話，事後查這筆被拒的撤銷時，紀錄與當時回給操作員的說法對不起來，追查的人會以為系統回過另一種理由。"
	)

	for _, tc := range verifierRevocationRefusalCases {
		t.Run(tc.name, func(t *testing.T) {
			st := newOperatorVerifierTestStore(t)
			created, err := st.ApplyOperatorVerifier(operatorVerifierCreateRequest(
				operatorVerifierPreviewFor(t, st), "verifier-revocation-refusal-create"))
			if err != nil {
				t.Fatal(err)
			}
			preview, err := st.PreviewOperatorVerifierRevocation(created.VerifierID)
			if err != nil {
				t.Fatal(err)
			}
			revision := preview.Revision
			request := OperatorVerifierRevocationRequest{
				VerifierID: created.VerifierID, ExpectedRevision: &revision,
				ConfirmDisplayName: preview.DisplayName, PreviewDigest: preview.PreviewDigest,
				Reason: "rotating the peer credential", IdempotencyKey: key,
				RequestDigest: "sha256:verifier-revocation-refusal",
				Audit: AuditEntry{
					SourceAddr: "100.64.0.10", AuthSubject: "tailscale-user:1",
					AuthCapability: "example.com/cap/clawctl-admin", SourceKind: "operator-api",
				},
			}
			tc.breakRequest(t, st, &request)

			_, err = st.ApplyOperatorVerifierRevocation(request)
			var rejection *OperatorRequestError
			if !errors.As(err, &rejection) {
				t.Errorf("撤銷結果是 %v，預期 *OperatorRequestError 且 code=%q —— %s", err, tc.code, notRefusedConsequence)
				return
			}
			if rejection.Code != tc.code {
				t.Errorf("撤銷拒絕 code 是 %q，預期 %q —— %s", rejection.Code, tc.code, wrongCodeConsequence)
			}
			if rejection.Detail != tc.want {
				t.Errorf("%s 到達 operator 的文案是 %q，預期 %q —— %s", tc.code, rejection.Detail, tc.want, tc.consequence)
			}

			var detail string
			if err := st.db.QueryRow(`SELECT detail FROM audit_log WHERE idempotency_key=? AND outcome='failed'`, key).Scan(&detail); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(detail, tc.want) {
				t.Errorf("%s 的審計 detail 是 %q，預期包含 %q —— %s", tc.code, detail, tc.want, auditConsequence)
			}
		})
	}
}
