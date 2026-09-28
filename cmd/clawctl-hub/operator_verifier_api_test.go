package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/teddashh/AI-Intune/internal/store"
)

func TestTheVerifierRevocationReplayIsLabelledOnAllThreeChannels(t *testing.T) {
	f := observedOperatorFixture(t)
	const (
		name          = "api-revocation-replay"
		kind          = "external_job_runner"
		failureDomain = "api-replay-domain"
	)

	previewBody := fmt.Sprintf(`{"kind":%q,"display_name":%q,"failure_domain":%q}`,
		kind, name, failureDomain)
	previewRec := operatorRequest(t, f.mux, http.MethodPost, "/v1/operator/verifiers/preview", "", previewBody)
	if previewRec.Code != http.StatusOK {
		t.Fatalf("preview status=%d body=%s", previewRec.Code, previewRec.Body.String())
	}
	var preview store.OperatorVerifierPreviewResult
	if err := json.Unmarshal(previewRec.Body.Bytes(), &preview); err != nil {
		t.Fatalf("decode preview: %v; status=%d body=%s", err, previewRec.Code, previewRec.Body.String())
	}

	createBody := fmt.Sprintf(`{"kind":%q,"display_name":%q,"failure_domain":%q,"preview_digest":%q,"reason":%q}`,
		kind, name, failureDomain, preview.PreviewDigest, "建立撤銷重放測試 verifier")
	createRec := operatorRequest(t, f.mux, http.MethodPost, "/v1/operator/verifiers", "api-verifier-create", createBody)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", createRec.Code, createRec.Body.String())
	}
	var created verifierOperatorResponse
	if err := json.Unmarshal(createRec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create: %v; status=%d body=%s", err, createRec.Code, createRec.Body.String())
	}

	base := "/v1/operator/verifiers/" + created.VerifierID
	revocationPreviewRec := operatorRequest(t, f.mux, http.MethodPost, base+"/revocation-preview", "", `{}`)
	if revocationPreviewRec.Code != http.StatusOK {
		t.Fatalf("revocation preview status=%d body=%s", revocationPreviewRec.Code, revocationPreviewRec.Body.String())
	}
	var revocationPreview store.OperatorVerifierRevocationPreviewResult
	if err := json.Unmarshal(revocationPreviewRec.Body.Bytes(), &revocationPreview); err != nil {
		t.Fatalf("decode revocation preview: %v; status=%d body=%s", err, revocationPreviewRec.Code, revocationPreviewRec.Body.String())
	}

	revokeBody := fmt.Sprintf(`{"expected_revision":%d,"confirm_display_name":%q,"preview_digest":%q,"reason":%q}`,
		revocationPreview.Revision, name, revocationPreview.PreviewDigest, "驗證撤銷重放的三路證據")
	freshRec := operatorRequest(t, f.mux, http.MethodPost, base+"/revocations", "api-verifier-revoke", revokeBody)
	var fresh store.OperatorVerifierRevocationResult
	if err := json.Unmarshal(freshRec.Body.Bytes(), &fresh); err != nil {
		t.Fatalf("decode fresh revocation: %v; status=%d body=%s", err, freshRec.Code, freshRec.Body.String())
	}
	if freshRec.Code != http.StatusCreated {
		t.Errorf("got status %d, expected 201；全新撤銷回 200，client 會走到「200 卻沒標重放」那一條，回 \"operator client: HTTP 200 verifier revocation is not labelled replay\"；operator 第一次撤銷就拿不到收據，不知道那台 verifier 撤了沒。", freshRec.Code)
	}
	if fresh.Replayed {
		t.Errorf("got replayed true, expected false；第一次撤銷的收據說自己是重放，operator 會以為稍早已經撤過一次，去找一筆不存在的前次操作；client 也會因為 201 帶 replayed 回 \"operator client: HTTP 201 verifier revocation claims replay\"，收據根本印不出來。")
	}
	if got := freshRec.Header().Get("Idempotency-Replayed"); got != "" {
		t.Errorf("got Idempotency-Replayed %q, expected empty；全新撤銷帶了重放 header，client 的 body／header 對帳會回 \"operator client: verifier revocation replay evidence mismatch body=false header=true\"，第一次撤銷就印不出收據。", got)
	}

	replayRec := operatorRequest(t, f.mux, http.MethodPost, base+"/revocations", "api-verifier-revoke", revokeBody)
	var replay store.OperatorVerifierRevocationResult
	if err := json.Unmarshal(replayRec.Body.Bytes(), &replay); err != nil {
		t.Fatalf("decode replayed revocation: %v; status=%d body=%s", err, replayRec.Code, replayRec.Body.String())
	}
	if replayRec.Code != http.StatusOK {
		t.Errorf("got status %d, expected 200；重放回 201，client 回 \"operator client: HTTP 201 verifier revocation claims replay\"；照 --idempotency-key 說明走官方復原路徑的 operator 拿不到收據。", replayRec.Code)
	}
	if !replay.Replayed {
		t.Errorf("got replayed false, expected true；重放的收據說自己是全新撤銷，CLI 會把它印成 revoked 而不是 replayed，operator 會以為自己剛剛又撤了一次。")
	}
	if got := replayRec.Header().Get("Idempotency-Replayed"); got != "true" {
		t.Errorf("got Idempotency-Replayed %q, expected %q；這一發就是量到的缺口：header 掉了之後 client 的 body／header 對帳回 \"operator client: verifier revocation replay evidence mismatch body=true header=false\"；operator 在網路中斷後照旗標說明重送，換到一句 client 協定錯誤，這次呼叫拿不到那張寫著撤了沒、何時、revision 多少的收據。", got, "true")
	}
	if replay.Revision != fresh.Revision || !replay.RevokedAt.Equal(fresh.RevokedAt) {
		t.Errorf("got replay revision=%d revoked_at=%s, expected revision=%d revoked_at=%s；重放交回的 revision 或撤銷時間跟第一次不同，operator 會以為第二次真的又動了一次帳本，去追查誰在同一台 verifier 上撤了兩次。",
			replay.Revision, replay.RevokedAt, fresh.Revision, fresh.RevokedAt)
	}
}

func TestAVerifierRequestRejectedAtTheTransportLayerIsStillAttributedInTheLedger(t *testing.T) {
	const (
		body                      = `{"unknown":true}`
		wantAuthSubject           = "tailscale-user:42"
		missingRowConsequence     = "被擋下的請求沒有在帳本裡留下任何一列，事後稽核者問「有沒有人試過動這台 verifier」會得到「沒有」；operator 當下看得到錯誤，但那個畫面不會留下來。"
		classificationConsequence = "這一列沒有被歸類成 transport rejection，所有依這個分類去撈「還沒進 domain 就被擋下」的讀取面都撈不到它，稽核者會把它當成一次普通的領域失敗。"
		outcomeConsequence        = "被拒的請求在結果欄記成成功，任何依結果欄篩失敗或數成功次數的面，都會把一次被擋下的請求算成一次做成的操作。"
		actionConsequence         = "記在另一個 canonical action 底下，`audit list --action` 用 SQL `a.action IN (...)` 過濾時這一列會從該看到它的清單裡消失，並在另一份清單裡變成一筆幽靈紀錄。"
		keyConsequence            = "審計列沒有帶上呼叫端那把 key，operator 重試同一個動作時沒辦法把兩次企圖串起來，看起來像兩件不相干的事。"
		actorConsequence          = "審計列沒有操作者，問「誰在送壞掉的請求」就答不出來；transport rejection 正是最需要知道來源的那一種列。"
		idempotencyConsequence    = "transport 失敗佔掉了冪等槽，operator 把 body 修好之後用同一把 key 重送會被當成重放，他會收到一份根本沒發生過的結果。"
	)
	tests := []struct {
		name               string
		path               string
		key                string
		wantAction         store.AuditAction
		wantSubject        string
		subjectConsequence string
	}{
		{
			name: "註冊", path: "/v1/operator/verifiers", key: "transport-register-key",
			wantAction: store.AuditVerifierRegister, wantSubject: "",
			subjectConsequence: "註冊的 subject 不是空的，就表示有人又在 body 解析失敗時編了一個對象出來；console 的「對象無法判讀」會被蓋掉，稽核者會以為系統真的認出了某個 display_name。",
		},
		{
			name: "撤銷", path: "/v1/operator/verifiers/verifier-under-audit/revocations", key: "transport-revoke-key",
			wantAction: store.AuditVerifierRevoke, wantSubject: "verifier-under-audit",
			subjectConsequence: "撤銷的 subject 不是 path 上那個 verifier id，稽核者就不知道這次被擋下的企圖是要撤誰；那個 id 在 URL 裡，body 壞掉也還在，沒有理由丟掉它。",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := observedOperatorFixture(t)
			rec := operatorRequest(t, f.mux, http.MethodPost, tc.path, tc.key, body)
			assertAPIError(t, rec, http.StatusBadRequest, "BAD_REQUEST")

			entries, err := f.store.ListAuditReads(store.AuditReadFilter{
				Actions: []store.AuditAction{tc.wantAction}, Limit: 10,
			})
			if err != nil {
				t.Fatal(err)
			}
			if got := len(entries.Items); got != 1 {
				t.Errorf("got %d audit rows, expected 1；%s", got, missingRowConsequence)
				return
			}
			entry := entries.Items[0]
			if !entry.IsOperatorTransportRejection() {
				t.Errorf("audit detail %q 沒有被歸類成 transport rejection —— %s", entry.Detail, classificationConsequence)
			}
			gotOutcome := "(缺結果欄)"
			if entry.Outcome != nil {
				gotOutcome = string(*entry.Outcome)
			}
			if entry.Outcome == nil || *entry.Outcome != store.AuditOutcomeFailed {
				t.Errorf("got outcome %q, expected %q；%s", gotOutcome, store.AuditOutcomeFailed, outcomeConsequence)
			}
			if got := entry.Subject; got != tc.wantSubject {
				t.Errorf("got subject %q, expected %q；%s", got, tc.wantSubject, tc.subjectConsequence)
			}
			if got := entry.Action; got != string(tc.wantAction) {
				t.Errorf("got action %q, expected %q；%s", got, string(tc.wantAction), actionConsequence)
			}
			if got := entry.IdempotencyKey; got != tc.key {
				t.Errorf("got idempotency key %q, expected %q；%s", got, tc.key, keyConsequence)
			}
			if got := entry.AuthSubject; got != wantAuthSubject {
				t.Errorf("got auth subject %q, expected %q；%s", got, wantAuthSubject, actorConsequence)
			}

			var receipts int
			if err := f.store.DB().QueryRow("SELECT COUNT(*) FROM operator_idempotency WHERE idempotency_key=?", tc.key).Scan(&receipts); err != nil {
				t.Fatal(err)
			}
			if receipts != 0 {
				t.Errorf("got %d idempotency rows, expected 0；%s", receipts, idempotencyConsequence)
			}
		})
	}
}
