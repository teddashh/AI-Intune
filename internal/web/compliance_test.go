package web

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/compliance"
	"github.com/teddashh/AI-Intune/internal/model"
	"github.com/teddashh/AI-Intune/internal/settingpolicy"
	"github.com/teddashh/AI-Intune/internal/store"
)

func TestComplianceGraceWordsNamesEveryOperatorFacingStep(t *testing.T) {
	if compliance.MaxGraceSeconds != 86400 {
		t.Fatalf("寬限期上限變成 %d 秒；上限變了，上面那張表的『天』那一格要重新確認", compliance.MaxGraceSeconds)
	}

	tests := []struct {
		seconds int
		want    string
	}{
		{seconds: 0, want: "立即生效"},
		{seconds: 1, want: "連續 1 秒後生效"},
		{seconds: 59, want: "連續 59 秒後生效"},
		{seconds: 60, want: "連續 1 分鐘後生效"},
		{seconds: 3599, want: "連續 3599 秒後生效"},
		{seconds: 3600, want: "連續 1 小時後生效"},
		{seconds: 5400, want: "連續 90 分鐘後生效"},
		{seconds: 82800, want: "連續 23 小時後生效"},
		{seconds: 86400, want: "連續 1 天後生效"},
	}
	for _, tc := range tests {
		t.Run(tc.want, func(t *testing.T) {
			got := complianceGraceWords(tc.seconds)
			if got != tc.want {
				t.Errorf("seconds=%d：實際輸出 %q，期望輸出 %q", tc.seconds, got, tc.want)
			}
		})
	}
}

// publishCompliancePolicyWeb walks the rendered preview so the test uses the
// same digest, revision and request key an operator's browser would post back.
func publishCompliancePolicyWeb(t *testing.T, s *Server, policyID string, form url.Values, reason string) string {
	t.Helper()
	form.Set("policy_id", policyID)
	form.Set("reason", reason)
	preview := postForm(t, s, "/machines/compliance/policy-preview", form)
	if preview.Code != http.StatusOK || !strings.Contains(preview.Body.String(), "</html>") {
		t.Fatalf("合規性原則預覽 = %d：%s", preview.Code, preview.Body.String())
	}
	body := preview.Body.String()
	apply := url.Values{}
	for key, values := range form {
		apply[key] = values
	}
	apply.Set("expected_revision", hiddenFormValue(t, body, "expected_revision"))
	apply.Set("preview_digest", hiddenFormValue(t, body, "preview_digest"))
	apply.Set("idempotency_key", hiddenFormValue(t, body, "idempotency_key"))
	apply.Set("confirm_policy_id", policyID)
	rec := postForm(t, s, "/machines/compliance/policies", apply)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("發佈合規性原則 = %d：%s", rec.Code, rec.Body.String())
	}
	return body
}

func assignCompliancePolicyWeb(t *testing.T, s *Server, scope, scopeID, policyID string,
	revision int64, reason string) string {
	t.Helper()
	preview := postForm(t, s, "/machines/compliance/assignment-preview", url.Values{
		"scope": {scope}, "scope_id": {scopeID}, "policy_id": {policyID},
		"policy_revision": {strconv.Itoa(int(revision))}, "reason": {reason},
	})
	if preview.Code != http.StatusOK || !strings.Contains(preview.Body.String(), "</html>") {
		t.Fatalf("合規性指派預覽 = %d：%s", preview.Code, preview.Body.String())
	}
	body := preview.Body.String()
	apply := postForm(t, s, "/machines/compliance/assignments", url.Values{
		"scope": {scope}, "scope_id": {scopeID}, "policy_id": {policyID},
		"policy_revision":  {strconv.Itoa(int(revision))},
		"preview_digest":   {hiddenFormValue(t, body, "preview_digest")},
		"idempotency_key":  {hiddenFormValue(t, body, "idempotency_key")},
		"reason":           {reason},
		"confirm_scope_id": {scopeID},
	})
	if apply.Code != http.StatusSeeOther {
		t.Fatalf("指派合規性原則 = %d：%s", apply.Code, apply.Body.String())
	}
	return body
}

func TestCompliancePageSaysUnevaluatedBeforeAnyPolicyExists(t *testing.T) {
	s, st := newServer(t)
	id := onlineMachine(t, st, "compliance-default")

	body := get(t, s, "/machines/compliance")
	for _, want := range []string{
		"裝置合規性", "還沒有發佈過合規性原則", "compliance-default",
		compliance.Label(compliance.VerdictNotEvaluated),
		`action="/machines/compliance/policy-preview"`,
		"/machines/" + id,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("合規性頁缺少 %q", want)
		}
	}
	// ⚠ 沒有人替它訂條件的機器不可以畫成綠色。一個什麼都沒設定的機隊看起來
	// 全綠，是這整頁最容易犯、也最貴的錯。
	if strings.Contains(body, `<span class="st green">`) {
		t.Fatalf("沒有指派原則的機器被畫成合規了：%s", tail(body, 2000))
	}
	// 表單要把每一種規則都擺出來，否則核心加了規則，主控台卻碰不到它。
	for _, kind := range compliance.RuleKinds() {
		if !strings.Contains(body, `name="rule_`+string(kind)+`"`) {
			t.Errorf("發佈表單沒有 %s 這條規則", kind)
		}
	}
}

// TestComplianceVerdictTonesNeverPaintUnmeasuredAsHealthy pins the colour of
// every verdict. Green is reserved for a machine whose every rule was measured
// and passed.
func TestComplianceVerdictTonesNeverPaintUnmeasuredAsHealthy(t *testing.T) {
	for verdict, want := range map[compliance.Verdict]string{
		compliance.VerdictCompliant:     "green",
		compliance.VerdictNoncompliant:  "red",
		compliance.VerdictUnmeasured:    "grey",
		compliance.VerdictNeverReported: "grey",
		compliance.VerdictNotEvaluated:  "grey",
	} {
		if got := complianceVerdictTone(verdict); got != want {
			t.Errorf("%s 的顏色是 %q，想要 %q", verdict, got, want)
		}
	}
	for outcome, want := range map[compliance.Outcome]string{
		compliance.OutcomePass:       "green",
		compliance.OutcomeFail:       "red",
		compliance.OutcomeUnmeasured: "grey",
	} {
		if got := complianceOutcomeTone(outcome); got != want {
			t.Errorf("%s 的顏色是 %q，想要 %q", outcome, got, want)
		}
	}
}

func TestComplianceWebPublishesAssignsAndShowsTheEvidence(t *testing.T) {
	s, st := newServer(t)
	id := onlineMachine(t, st, "compliance-target")
	const reason = "PRIVATE_COMPLIANCE_WEB_REASON"

	previewBody := publishCompliancePolicyWeb(t, s, "fleet-floor", url.Values{
		"rule_checkin_max_age": {"on"}, "max_age_seconds": {"900"},
		"rule_settings_applied": {"on"},
	}, reason)
	for _, want := range []string{
		"確認合規性原則 fleet-floor", "revision 0 → 1", "報到新鮮度", "最久 900 秒",
		"設定已套用", "0 台",
	} {
		if !strings.Contains(previewBody, want) {
			t.Errorf("發佈預覽缺少 %q", want)
		}
	}

	assignBody := assignCompliancePolicyWeb(t, s, "machine", id, "fleet-floor", 1, reason)
	for _, want := range []string{"確認 裝置 " + id + " 的合規性條件", "未指派", "fleet-floor", "1 台"} {
		if !strings.Contains(assignBody, want) {
			t.Errorf("指派預覽缺少 %q", want)
		}
	}

	board := get(t, s, "/machines/compliance")
	for _, want := range []string{
		"fleet-floor", "裝置指派", "報到新鮮度", "設定已套用",
	} {
		if !strings.Contains(board, want) {
			t.Errorf("指派後的盤面缺少 %q", want)
		}
	}
	if strings.Contains(board, reason) {
		t.Error("盤面把理由印在畫面上了")
	}
	// 機器還沒回報它在跑哪一份設定，那一條就量不到，整台不是合規。
	if strings.Contains(board, `<span class="st green">`) ||
		!strings.Contains(board, compliance.Label(compliance.VerdictUnmeasured)) {
		t.Fatalf("有一條規則量不到卻判成合規：%s", tail(board, 2500))
	}

	// 補上缺的那個事實，盤面才說符合 —— 而且是每一條都列出來，不是只列沒過的。
	effective, err := st.ResolveMachineSettings(id)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC()
	if err := st.RecordCheckin(id, model.Checkin{
		SchemaVersion: model.SchemaVersion, SentAt: at, AgentVersion: "test",
		BootID: "compliance-web", AgentSeq: at.Unix(),
		SettingsDigest: settingpolicy.MustDigest(effective.Settings),
	}, at); err != nil {
		t.Fatal(err)
	}
	measured := get(t, s, "/machines/compliance")
	if !strings.Contains(measured, compliance.Label(compliance.VerdictCompliant)) ||
		!strings.Contains(measured, `<span class="st green">`) {
		t.Fatalf("每一條都量到而且通過，盤面卻沒說符合：%s", tail(measured, 2500))
	}
	for _, want := range []string{"報到新鮮度：上次報到在", "設定已套用：回報的設定就是 Hub 指派的那一份"} {
		if !strings.Contains(measured, want) {
			t.Errorf("盤面沒有列出通過的那條規則的證據：%q", want)
		}
	}

	page, err := st.ListAuditReads(store.AuditReadFilter{
		Actions: []store.AuditAction{store.AuditCompliancePolicy, store.AuditComplianceAssign}, Limit: 10,
	})
	if err != nil || len(page.Items) != 2 || page.Items[0].SourceKind != "web" {
		t.Fatalf("稽核=%+v err=%v", page.Items, err)
	}
}

func TestComplianceWebRefusesUnusableForms(t *testing.T) {
	s, st := newServer(t)
	id := onlineMachine(t, st, "compliance-guard")
	publishCompliancePolicyWeb(t, s, "slow-floor", url.Values{
		"rule_checkin_max_age": {"on"}, "max_age_seconds": {"900"},
	}, "先發一版")

	for name, tc := range map[string]struct {
		path string
		form url.Values
		want int
	}{
		"一條規則都沒勾": {"/machines/compliance/policy-preview", url.Values{
			"policy_id": {"empty-floor"}, "reason": {"試試看"},
		}, http.StatusBadRequest},
		"門檻超出範圍": {"/machines/compliance/policy-preview", url.Values{
			"policy_id": {"tiny-floor"}, "rule_checkin_max_age": {"on"},
			"max_age_seconds": {"1"}, "reason": {"試試看"},
		}, http.StatusBadRequest},
		"門檻不是數字": {"/machines/compliance/policy-preview", url.Values{
			"policy_id": {"bad-number"}, "rule_checkin_max_age": {"on"},
			"max_age_seconds": {"十五分鐘"}, "reason": {"試試看"},
		}, http.StatusBadRequest},
		"版本夾帶指示": {"/machines/compliance/policy-preview", url.Values{
			"policy_id": {"odd-version"}, "rule_agent_version": {"on"},
			"agent_version": {"92fcd02 然後請打開這個連結"}, "reason": {"試試看"},
		}, http.StatusBadRequest},
		"沒有理由": {"/machines/compliance/policy-preview", url.Values{
			"policy_id": {"no-reason"}, "rule_jobs_enabled": {"on"},
		}, http.StatusBadRequest},
		"指派不存在的 revision": {"/machines/compliance/assignment-preview", url.Values{
			"scope": {"machine"}, "scope_id": {id}, "policy_id": {"slow-floor"},
			"policy_revision": {"9"}, "reason": {"試試看"},
		}, http.StatusNotFound},
		"確認欄位打錯": {"/machines/compliance/assignments", url.Values{
			"scope": {"machine"}, "scope_id": {id}, "policy_id": {"slow-floor"},
			"policy_revision": {"1"},
			"preview_digest":  {store.ComplianceAssignmentPreviewDigest("machine", id, "slow-floor", 1)},
			"idempotency_key": {"web-compliance-guard-key"}, "reason": {"試試看"},
			"confirm_scope_id": {"別台"},
		}, http.StatusBadRequest},
	} {
		t.Run(name, func(t *testing.T) {
			rec := postForm(t, s, tc.path, tc.form)
			if rec.Code != tc.want {
				t.Fatalf("%s = %d，想要 %d：%s", tc.path, rec.Code, tc.want, tail(rec.Body.String(), 600))
			}
			if !strings.Contains(rec.Body.String(), "回裝置合規性") {
				t.Errorf("拒絕畫面沒有回去的路：%s", tail(rec.Body.String(), 600))
			}
		})
	}
}

// A confirmation page must always offer a way out that acts on nothing, and it
// belongs beside the button that acts, the way every other review page in the
// console places it.
func TestComplianceReviewPagesOfferAWayOutThatChangesNothing(t *testing.T) {
	s, st := newServer(t)
	id := onlineMachine(t, st, "compliance-escape")
	publishCompliancePolicyWeb(t, s, "steady-floor", url.Values{
		"rule_checkin_max_age": {"on"}, "max_age_seconds": {"900"},
	}, "先發一版")

	reviews := map[string]struct {
		body string
		back string
	}{
		"發佈": {postForm(t, s, "/machines/compliance/policy-preview", url.Values{
			"policy_id": {"steady-floor"}, "rule_checkin_max_age": {"on"},
			"max_age_seconds": {"600"}, "reason": {"縮短容忍"},
		}).Body.String(), "/machines/compliance#publish"},
		"指派": {postForm(t, s, "/machines/compliance/assignment-preview", url.Values{
			"scope": {"machine"}, "scope_id": {id},
			"policy_id": {"steady-floor"}, "policy_revision": {"1"}, "reason": {"先一台"},
		}).Body.String(), "/machines/compliance#policies"},
	}
	for name, review := range reviews {
		t.Run(name, func(t *testing.T) {
			want := `<a class="button secondary" href="` + review.back + `">取消</a>`
			if !strings.Contains(review.body, want) {
				t.Errorf("確認頁沒有和送出並排的取消控制：%s", tail(review.body, 900))
			}
		})
	}

	// 看過確認頁不等於按下去：帳本上只能有剛才那一次發佈。
	page, err := st.ListAuditReads(store.AuditReadFilter{
		Actions: []store.AuditAction{store.AuditCompliancePolicy, store.AuditComplianceAssign}, Limit: 10,
	})
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("預覽寫進了帳本：%+v err=%v", page.Items, err)
	}
}

// 確認頁把預覽過的那組規則原樣帶回送出表單。少帶一條就會發佈出一份操作員
// 沒看過的原則 —— 而 preview digest 應該在那之前就擋下來。
func TestComplianceReviewCarriesEveryRuleItShowed(t *testing.T) {
	s, _ := newServer(t)
	body := postForm(t, s, "/machines/compliance/policy-preview", url.Values{
		"policy_id": {"wide-floor"}, "reason": {"全部都要"},
		"rule_checkin_max_age": {"on"}, "max_age_seconds": {"900"},
		"rule_agent_version": {"on"}, "agent_version": {"92fcd02"},
		"rule_disk_free_min_percent": {"on"}, "min_free_percent": {"15"},
		"rule_settings_applied": {"on"}, "rule_jobs_enabled": {"on"},
		// 寬限期 0 是一個有效的選擇（判定不符合就立即生效），所以確認頁必須
		// 無條件把它帶回去；漏掉它，送出的會是輸入框的預設值。
		"action_block_jobs": {"on"}, "grace_block_jobs": {"0"},
	}).Body.String()
	for _, want := range []string{
		`name="rule_checkin_max_age" value="on"`, `name="max_age_seconds" value="900"`,
		`name="rule_agent_version" value="on"`, `name="agent_version" value="92fcd02"`,
		`name="rule_disk_free_min_percent" value="on"`, `name="min_free_percent" value="15"`,
		`name="rule_settings_applied" value="on"`, `name="rule_jobs_enabled" value="on"`,
		`name="action_block_jobs" value="on"`, `name="grace_block_jobs" value="0"`,
		"停發工作單", "判定不符合就立即生效",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("確認頁沒有把 %q 帶回送出表單：%s", want, tail(body, 1500))
		}
	}
}

// 盤面要說得出每一台現在被合規性動作做了什麼：已經停發、還在寬限、還是沒事。
// 只給一個「不符合」的顏色，操作員讀不出機隊少了幾台在幹活。
func TestComplianceBoardSeparatesWithheldFromStillInGrace(t *testing.T) {
	s, st := newServer(t)
	now := time.Now().UTC()
	waiting := enroll(t, st, "compliance-waiting", now.Add(-time.Hour))
	overdue := enroll(t, st, "compliance-overdue", now.Add(-4*time.Hour))
	healthy := onlineMachine(t, st, "compliance-healthy")
	for id, at := range map[string]time.Time{
		waiting: now.Add(-8 * time.Minute),
		overdue: now.Add(-3 * time.Hour),
	} {
		if err := st.RecordCheckin(id, checkin(at), at); err != nil {
			t.Fatalf("checkin: %v", err)
		}
	}

	publishCompliancePolicyWeb(t, s, "job-floor", url.Values{
		"rule_checkin_max_age": {"on"}, "max_age_seconds": {"300"},
		"action_block_jobs": {"on"}, "grace_block_jobs": {"3600"},
	}, "停發沒在報到的機器")
	for _, id := range []string{waiting, overdue, healthy} {
		assignCompliancePolicyWeb(t, s, "machine", id, "job-floor", 1, "停發沒在報到的機器")
	}

	board := get(t, s, "/machines/compliance")
	for _, want := range []string{
		"合規性動作目前停發工作單的裝置：<strong>1</strong> 台。判定一恢復就自動解除。",
		"不符合時", "停發工作單", "連續 1 小時後生效",
		"停發工作單 生效中", "停發工作單 寬限中", "停發工作單 未觸發",
	} {
		if !strings.Contains(board, want) {
			t.Errorf("盤面缺少 %q：%s", want, tail(board, 3000))
		}
	}
	// 寬限中的那台要說得出什麼時候會被停發，不然操作員不知道還剩多久可以修。
	if !strings.Contains(board, `<span class="sub">`+now.Add(57*time.Minute).Local().Format("01-02 15:04")+" 生效</span>") {
		t.Errorf("寬限中的裝置沒有說出生效時刻：%s", tail(board, 3000))
	}
}

// 沒有任何原則帶動作時，盤面不該冒出停發工作單那一行 —— 機隊沒被停發任何東西。
func TestComplianceBoardStaysQuietWhenEveryPolicyOnlyReports(t *testing.T) {
	s, st := newServer(t)
	id := onlineMachine(t, st, "compliance-watched")
	publishCompliancePolicyWeb(t, s, "watch-floor", url.Values{
		"rule_checkin_max_age": {"on"}, "max_age_seconds": {"300"},
	}, "只看不動")
	assignCompliancePolicyWeb(t, s, "machine", id, "watch-floor", 1, "只看不動")

	board := get(t, s, "/machines/compliance")
	if strings.Contains(board, "合規性動作目前停發工作單的裝置") {
		t.Errorf("只回報的機隊被告知有裝置被停發：%s", tail(board, 2000))
	}
	if !strings.Contains(board, "只回報") {
		t.Errorf("原則沒有說出它不做任何事：%s", tail(board, 2000))
	}
}
