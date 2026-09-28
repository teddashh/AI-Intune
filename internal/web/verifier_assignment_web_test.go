package web

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/deploy"
	"github.com/teddashh/AI-Intune/internal/operatorauth"
	"github.com/teddashh/AI-Intune/internal/store"
)

// assignmentFixture returns a finished job on one machine plus a verifier that
// lives somewhere else. Both halves matter: an unfinished job is never handed
// out, and a verifier in the same failure domain is not a second opinion.
func assignmentFixture(t *testing.T, st *store.Store) (jobID string, verifier store.Verifier, peer store.Verifier) {
	t.Helper()
	machineID := onlineMachine(t, st, "samplehub1")
	jobID, token, now := createWebJob(t, st, machineID, false)
	startWebJob(t, st, machineID, jobID, token, now)
	// The producer reports its own success. That self-report is exactly what a
	// second, independent look is for.
	if err := st.RecordVerification(jobID, machineID, token, "health", "curl /health",
		0, "ok", "", true, now); err != nil {
		t.Fatalf("record verification: %v", err)
	}
	if _, err := st.DB().Exec(`UPDATE jobs SET state=?,terminal_at=?,
  lease_token=NULL,lease_expires_at=NULL WHERE job_id=?`,
		deploy.Succeeded, now.Add(time.Minute).UTC().Format(time.RFC3339), jobID); err != nil {
		t.Fatalf("finish job: %v", err)
	}
	peerID := onlineMachine(t, st, "sampleagent3")
	verifier, _, err := st.RegisterVerifier(store.VerifierKindFleetPeerAgent, "sampleagent3-peer", peerID, "")
	if err != nil {
		t.Fatalf("register verifier: %v", err)
	}
	peer, _, err = st.RegisterVerifier(store.VerifierKindFleetPeerAgent, "samplehub1-self", machineID, "")
	if err != nil {
		t.Fatalf("register same-domain verifier: %v", err)
	}
	return jobID, verifier, peer
}

// assignmentForm reads the confirmation page's own hidden fields, so the test
// submits what the operator's browser would submit and cannot pass against a
// page that stopped rendering one of them.
func assignmentForm(t *testing.T, body string) url.Values {
	t.Helper()
	form := url.Values{}
	for _, name := range []string{"verifier_id", "preview_digest", "reason", "idempotency_key"} {
		form.Set(name, hiddenFormValue(t, body, name))
	}
	return form
}

func assignmentRows(t *testing.T, st *store.Store, jobID string) []store.VerificationAssignment {
	t.Helper()
	rows, err := st.JobVerificationAssignments(jobID)
	if err != nil {
		t.Fatalf("read assignments: %v", err)
	}
	return rows
}

// The whole point of the 派工 板 is that an operator can ask a *different*
// machine to look at a job. So the form must not offer the producer's own
// domain, and posting that pairing by hand must be refused by the Hub rather
// than by the dropdown that omitted it.
func TestJobPageOffersOnlyVerifiersOutsideTheProducersDomain(t *testing.T) {
	s, st := newServer(t)
	jobID, verifier, peer := assignmentFixture(t, st)

	page := doGet(t, s, "/jobs/"+jobID).Body.String()
	if !strings.Contains(page, `action="/jobs/`+jobID+`/verifier-assignment-preview"`) {
		t.Fatalf("job page 沒有派工表單：\n%s", page)
	}
	if !strings.Contains(page, `value="`+verifier.VerifierID+`"`) {
		t.Error("派工表單沒有列出不同 failure domain 的 verifier")
	}
	if strings.Contains(page, `value="`+peer.VerifierID+`"`) {
		t.Error("派工表單列出了跟工作單同一台機器的 verifier —— 那不是第二個判斷")
	}

	refused := postForm(t, s, "/jobs/"+jobID+"/verifier-assignment-preview", url.Values{
		"verifier_id": {peer.VerifierID}, "reason": {"手動貼上同域 verifier"},
	})
	if refused.Code != http.StatusConflict {
		t.Fatalf("同域派工預覽 = %d，應該是 409：%s", refused.Code, refused.Body.String())
	}
	if !strings.Contains(refused.Body.String(), "換一個 failure domain 不同的 verifier") {
		t.Errorf("拒絕訊息沒有講下一步：%s", refused.Body.String())
	}
	if rows := assignmentRows(t, st, jobID); len(rows) != 0 {
		t.Fatalf("被拒絕的預覽竟然寫了 %d 列派工", len(rows))
	}
}

func TestJobPageHidesAssignmentFormWithoutOperate(t *testing.T) {
	s, st := newServer(t)
	jobID, verifier, _ := assignmentFixture(t, st)

	names, err := operatorauth.NamesForPrefix("example.com/cap/clawctl")
	if err != nil {
		t.Fatal(err)
	}
	page := renderWithCapabilities(t, s, "/jobs/"+jobID,
		operatorauth.CapabilityNames{View: names.View})
	if strings.Contains(page, "/verifier-assignment-preview") {
		t.Error("只有 view 權限的人看到了派工表單")
	}
	if strings.Contains(page, verifier.VerifierID) {
		t.Error("只有 view 權限的人看到了 verifier 名冊")
	}
}

// preview → confirm → applied, and the confirmation is checked against the
// registry. A confirmation field compared with a hidden field in the same form
// would confirm nothing.
func TestAssignVerifierFromTheJobPageWritesOneAssignment(t *testing.T) {
	s, st := newServer(t)
	jobID, verifier, _ := assignmentFixture(t, st)

	preview := postForm(t, s, "/jobs/"+jobID+"/verifier-assignment-preview", url.Values{
		"verifier_id": {verifier.VerifierID}, "reason": {"samplehub1 自己說成功，找 sampleagent3 看一次"},
	})
	if preview.Code != http.StatusOK {
		t.Fatalf("派工預覽 = %d：%s", preview.Code, preview.Body.String())
	}
	body := preview.Body.String()
	for _, want := range []string{"sampleagent3-peer", "sampleagent3", "samplehub1 自己說成功，找 sampleagent3 看一次",
		"這個 verifier 送齊三條必要規則的完整報告", "完整報告會參與 stable promotion",
		"派工本身不決定這張工作單成不成功"} {
		if !strings.Contains(body, want) {
			t.Errorf("預覽頁少了 %q：\n%s", want, body)
		}
	}
	if strings.Contains(body, "派工不改變部署閘") {
		t.Errorf("預覽頁仍在說舊契約：\n%s", body)
	}
	// The Hub does not decide what the verifier measures, so the confirmation
	// page has nothing to say about commands, rules or expected digests.
	for _, forbidden := range []string{"指令", "rule_id", "artifact_digest", "<code>"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("預覽頁出現了 %q —— Hub 不決定 verifier 量什麼", forbidden)
		}
	}

	// A mistyped name spends that request key, exactly like every other typed
	// confirmation in this console: recovering means previewing again, so the
	// operator re-reads the pairing rather than retrying blind.
	wrong := assignmentForm(t, body)
	wrong.Set("confirm_verifier_name", "sampleagent3")
	if rec := postForm(t, s, "/jobs/"+jobID+"/verifier-assignments", wrong); rec.Code != http.StatusBadRequest {
		t.Fatalf("打錯 verifier 名字 = %d，應該是 400：%s", rec.Code, rec.Body.String())
	}
	if rows := assignmentRows(t, st, jobID); len(rows) != 0 {
		t.Fatalf("打錯名字卻寫了 %d 列派工", len(rows))
	}

	second := postForm(t, s, "/jobs/"+jobID+"/verifier-assignment-preview", url.Values{
		"verifier_id": {verifier.VerifierID}, "reason": {"samplehub1 自己說成功，找 sampleagent3 看一次"},
	})
	if second.Code != http.StatusOK {
		t.Fatalf("重新預覽 = %d：%s", second.Code, second.Body.String())
	}
	form := assignmentForm(t, second.Body.String())
	form.Set("confirm_verifier_name", "sampleagent3-peer")
	applied := postForm(t, s, "/jobs/"+jobID+"/verifier-assignments", form)
	if applied.Code != http.StatusSeeOther {
		t.Fatalf("指派 = %d：%s", applied.Code, applied.Body.String())
	}
	if back := applied.Header().Get("Location"); back != "/jobs/"+jobID {
		t.Errorf("指派後沒有回到工作單頁：%q", back)
	}
	rows := assignmentRows(t, st, jobID)
	if len(rows) != 1 {
		t.Fatalf("派工列數 = %d，want 1", len(rows))
	}
	if rows[0].VerifierID != verifier.VerifierID || !rows[0].Pending() {
		t.Fatalf("派工列不對：%+v", rows[0])
	}
	// One press, one audit row. The writer records the decision inside its own
	// transaction, so the service-level fallback must stay out of the way; when
	// it did not, the live ledger carried two identical rows for every
	// successful assignment and the audit page counted each press twice.
	if got := assignmentAuditRows(t, st, jobID); got != 1 {
		t.Fatalf("一次成功的派工寫了 %d 筆 audit，want 1", got)
	}

	// Same form again is a retry of one intent, not a second assignment.
	replayed := postForm(t, s, "/jobs/"+jobID+"/verifier-assignments", form)
	if replayed.Code != http.StatusSeeOther {
		t.Fatalf("重送 = %d：%s", replayed.Code, replayed.Body.String())
	}
	if rows := assignmentRows(t, st, jobID); len(rows) != 1 {
		t.Fatalf("重送同一張表單之後派工列數 = %d，want 1", len(rows))
	}
	// The replay adds its own row because pressing again is a thing that
	// happened; what it must not do is add two.
	if got := assignmentAuditRows(t, st, jobID); got != 2 {
		t.Fatalf("apply 加 replay 寫了 %d 筆 audit，want 2", got)
	}

	page := doGet(t, s, "/jobs/"+jobID).Body.String()
	if !strings.Contains(page, "已發出，等它回報") {
		t.Errorf("job 頁沒有顯示待回報的派工：\n%s", page)
	}

	// And the verifier plane now has exactly this one job to look at.
	pending, err := st.PendingVerificationAssignments(verifier.VerifierID)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].JobID != jobID {
		t.Fatalf("verifier 平面拿到 %d 張單：%+v", len(pending), pending)
	}
}

func TestAssignVerifierRefusesAnIncompleteConfirmation(t *testing.T) {
	s, st := newServer(t)
	jobID, verifier, _ := assignmentFixture(t, st)

	for name, form := range map[string]url.Values{
		"沒有理由":        {"verifier_id": {verifier.VerifierID}, "reason": {""}},
		"沒有 verifier": {"verifier_id": {""}, "reason": {"試試看"}},
	} {
		rec := postForm(t, s, "/jobs/"+jobID+"/verifier-assignment-preview", form)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s 的預覽 = %d，應該是 400", name, rec.Code)
		}
	}
	for name, form := range map[string]url.Values{
		"沒有 preview digest": {"verifier_id": {verifier.VerifierID}, "reason": {"x"},
			"idempotency_key": {"key-1"}, "confirm_verifier_name": {"sampleagent3-peer"}},
		"沒有 request key": {"verifier_id": {verifier.VerifierID}, "reason": {"x"},
			"preview_digest": {"sha256:whatever"}, "confirm_verifier_name": {"sampleagent3-peer"}},
	} {
		rec := postForm(t, s, "/jobs/"+jobID+"/verifier-assignments", form)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s 的指派 = %d，應該是 400", name, rec.Code)
		}
	}
	if rows := assignmentRows(t, st, jobID); len(rows) != 0 {
		t.Fatalf("不完整的表單寫了 %d 列派工", len(rows))
	}
}

// assignmentAuditRows counts the successful verification-assign rows this job
// collected. It reads the table rather than the read model so a duplicate can
// never hide behind a projection that de-duplicates.
func assignmentAuditRows(t *testing.T, st *store.Store, jobID string) int {
	t.Helper()
	var count int
	if err := st.DB().QueryRow(
		`SELECT COUNT(*) FROM audit_log WHERE action=? AND subject=? AND outcome='ok'`,
		store.AuditVerificationAssign, jobID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}
