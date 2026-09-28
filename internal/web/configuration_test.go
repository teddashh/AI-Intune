package web

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/model"
	"github.com/teddashh/AI-Intune/internal/settingpolicy"
	"github.com/teddashh/AI-Intune/internal/store"
)

// publishConfigurationPolicy walks the rendered preview so the test uses the
// same digest, revision and request key an operator's browser would post back.
func publishConfigurationPolicy(t *testing.T, s *Server, policyID string, checkin, observation int, reason string) string {
	t.Helper()
	preview := postForm(t, s, "/machines/configuration/policy-preview", url.Values{
		"policy_id":                    {policyID},
		"checkin_interval_seconds":     {strconv.Itoa(checkin)},
		"observation_interval_seconds": {strconv.Itoa(observation)},
		"reason":                       {reason},
	})
	if preview.Code != http.StatusOK || !strings.Contains(preview.Body.String(), "</html>") {
		t.Fatalf("設定原則預覽 = %d：%s", preview.Code, preview.Body.String())
	}
	body := preview.Body.String()
	apply := postForm(t, s, "/machines/configuration/policies", url.Values{
		"policy_id":                    {policyID},
		"checkin_interval_seconds":     {strconv.Itoa(checkin)},
		"observation_interval_seconds": {strconv.Itoa(observation)},
		"expected_revision":            {hiddenFormValue(t, body, "expected_revision")},
		"preview_digest":               {hiddenFormValue(t, body, "preview_digest")},
		"idempotency_key":              {hiddenFormValue(t, body, "idempotency_key")},
		"reason":                       {reason},
		"confirm_policy_id":            {policyID},
	})
	if apply.Code != http.StatusSeeOther {
		t.Fatalf("發佈設定原則 = %d：%s", apply.Code, apply.Body.String())
	}
	return body
}

func assignConfigurationPolicy(t *testing.T, s *Server, scope, scopeID, policyID string, revision int64, reason string) string {
	t.Helper()
	preview := postForm(t, s, "/machines/configuration/assignment-preview", url.Values{
		"scope": {scope}, "scope_id": {scopeID}, "policy_id": {policyID},
		"policy_revision": {strconv.Itoa(int(revision))}, "reason": {reason},
	})
	if preview.Code != http.StatusOK || !strings.Contains(preview.Body.String(), "</html>") {
		t.Fatalf("設定指派預覽 = %d：%s", preview.Code, preview.Body.String())
	}
	body := preview.Body.String()
	apply := postForm(t, s, "/machines/configuration/assignments", url.Values{
		"scope": {scope}, "scope_id": {scopeID}, "policy_id": {policyID},
		"policy_revision":  {strconv.Itoa(int(revision))},
		"preview_digest":   {hiddenFormValue(t, body, "preview_digest")},
		"idempotency_key":  {hiddenFormValue(t, body, "idempotency_key")},
		"reason":           {reason},
		"confirm_scope_id": {scopeID},
	})
	if apply.Code != http.StatusSeeOther {
		t.Fatalf("指派設定原則 = %d：%s", apply.Code, apply.Body.String())
	}
	return body
}

func TestConfigurationPageShowsDefaultsBeforeAnyPolicyExists(t *testing.T) {
	s, st := newServer(t)
	id := onlineMachine(t, st, "configuration-default")

	body := get(t, s, "/machines/configuration")
	for _, want := range []string{
		"裝置組態", "目前每台都跑預設值：報到 120 秒、量測 600 秒",
		"configuration-default", "預設值", "120 秒", "600 秒",
		settingpolicy.Label(settingpolicy.VerdictUnknown),
		`action="/machines/configuration/policy-preview"`,
		"/machines/" + id,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("裝置組態頁缺少 %q", want)
		}
	}
	// 一台還沒回報設定的機器不可以畫成綠色：看起來平靜的頁面必須代表量到了。
	if !strings.Contains(body, `<span class="st grey">`) || strings.Contains(body, `<span class="st green">`) {
		t.Fatalf("沒有量到的機器被畫成健康的了：%s", tail(body, 2000))
	}
}

// TestConfigurationVerdictTonesNeverPaintUnmeasuredAsHealthy pins the colour of
// every verdict. Green is reserved for a machine that echoed the exact digest
// the Hub resolved for it.
func TestConfigurationVerdictTonesNeverPaintUnmeasuredAsHealthy(t *testing.T) {
	for verdict, want := range map[settingpolicy.Verdict]string{
		settingpolicy.VerdictApplied:       "green",
		settingpolicy.VerdictPending:       "blue",
		settingpolicy.VerdictMismatch:      "red",
		settingpolicy.VerdictUnknown:       "grey",
		settingpolicy.VerdictNeverReported: "grey",
	} {
		if got := configurationVerdictTone(verdict); got != want {
			t.Errorf("%s 的顏色是 %q，想要 %q", verdict, got, want)
		}
	}
}

func TestConfigurationWebPublishesAssignsAndReportsAppliedState(t *testing.T) {
	s, st := newServer(t)
	id := onlineMachine(t, st, "configuration-target")
	const reason = "PRIVATE_CONFIG_WEB_REASON"

	previewBody := publishConfigurationPolicy(t, s, "fast-fleet", 60, 300, reason)
	for _, want := range []string{
		"確認設定原則 fast-fleet", "revision 0 → 1", "60 秒", "300 秒", "0 台",
	} {
		if !strings.Contains(previewBody, want) {
			t.Errorf("發佈預覽缺少 %q", want)
		}
	}

	assignBody := assignConfigurationPolicy(t, s, "machine", id, "fast-fleet", 1, reason)
	for _, want := range []string{"確認 裝置 " + id + " 的設定", "預設值", "fast-fleet", "1 台"} {
		if !strings.Contains(assignBody, want) {
			t.Errorf("指派預覽缺少 %q", want)
		}
	}

	board := get(t, s, "/machines/configuration")
	for _, want := range []string{
		"fast-fleet", "裝置指派", "60 秒", "300 秒",
		settingpolicy.Label(settingpolicy.VerdictUnknown),
	} {
		if !strings.Contains(board, want) {
			t.Errorf("指派後的盤面缺少 %q", want)
		}
	}
	if strings.Contains(board, reason) {
		t.Error("盤面把理由印在畫面上了")
	}

	// 機器回報跑的就是指派的那一份，盤面才說已套用。
	effective, err := st.ResolveMachineSettings(id)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC()
	if err := st.RecordCheckin(id, model.Checkin{
		SchemaVersion: model.SchemaVersion, SentAt: at, AgentVersion: "test",
		BootID: "configuration-web", AgentSeq: at.Unix(), SettingsDigest: effective.Digest,
	}, at); err != nil {
		t.Fatal(err)
	}
	applied := get(t, s, "/machines/configuration")
	if !strings.Contains(applied, settingpolicy.Label(settingpolicy.VerdictApplied)) ||
		!strings.Contains(applied, `<span class="st green">`) {
		t.Fatalf("回報 digest 之後盤面沒有說已套用：%s", tail(applied, 2000))
	}

	page, err := st.ListAuditReads(store.AuditReadFilter{
		Actions: []store.AuditAction{store.AuditSettingPolicy, store.AuditSettingAssign}, Limit: 10,
	})
	if err != nil || len(page.Items) != 2 || page.Items[0].SourceKind != "web" {
		t.Fatalf("稽核=%+v err=%v", page.Items, err)
	}
}

func TestConfigurationWebRefusesUnusableForms(t *testing.T) {
	s, st := newServer(t)
	id := onlineMachine(t, st, "configuration-guard")
	publishConfigurationPolicy(t, s, "slow-fleet", 300, 900, "先發一版")

	for name, tc := range map[string]struct {
		path string
		form url.Values
		want int
	}{
		"量測間隔小於報到間隔": {"/machines/configuration/policy-preview", url.Values{
			"policy_id": {"bad-order"}, "checkin_interval_seconds": {"600"},
			"observation_interval_seconds": {"120"}, "reason": {"試試看"},
		}, http.StatusBadRequest},
		"間隔不是數字": {"/machines/configuration/policy-preview", url.Values{
			"policy_id": {"bad-number"}, "checkin_interval_seconds": {"一分鐘"},
			"observation_interval_seconds": {"600"}, "reason": {"試試看"},
		}, http.StatusBadRequest},
		"沒有理由": {"/machines/configuration/policy-preview", url.Values{
			"policy_id": {"no-reason"}, "checkin_interval_seconds": {"60"},
			"observation_interval_seconds": {"600"},
		}, http.StatusBadRequest},
		"指派不存在的 revision": {"/machines/configuration/assignment-preview", url.Values{
			"scope": {"machine"}, "scope_id": {id}, "policy_id": {"slow-fleet"},
			"policy_revision": {"9"}, "reason": {"試試看"},
		}, http.StatusNotFound},
		"確認欄位打錯": {"/machines/configuration/assignments", url.Values{
			"scope": {"machine"}, "scope_id": {id}, "policy_id": {"slow-fleet"},
			"policy_revision": {"1"}, "preview_digest": {store.SettingAssignmentPreviewDigest("machine", id, "slow-fleet", 1)},
			"idempotency_key": {"web-config-guard-key"}, "reason": {"試試看"},
			"confirm_scope_id": {"別台"},
		}, http.StatusBadRequest},
	} {
		t.Run(name, func(t *testing.T) {
			rec := postForm(t, s, tc.path, tc.form)
			if rec.Code != tc.want {
				t.Fatalf("%s = %d，想要 %d：%s", tc.path, rec.Code, tc.want, tail(rec.Body.String(), 600))
			}
			if !strings.Contains(rec.Body.String(), "回裝置組態") {
				t.Errorf("拒絕畫面沒有回去的路：%s", tail(rec.Body.String(), 600))
			}
		})
	}
}

// A confirmation page must always offer a way out that acts on nothing, and it
// belongs beside the button that acts, the way every other review page in the
// console places it. A page whose only control is 送出 is a page an operator
// can leave only by pressing it.
func TestConfigurationReviewPagesOfferAWayOutThatChangesNothing(t *testing.T) {
	s, st := newServer(t)
	id := onlineMachine(t, st, "configuration-escape")
	publishConfigurationPolicy(t, s, "steady-fleet", 120, 600, "先發一版")

	reviews := map[string]struct {
		body string
		back string
	}{
		"發佈": {postForm(t, s, "/machines/configuration/policy-preview", url.Values{
			"policy_id":                    {"steady-fleet"},
			"checkin_interval_seconds":     {"90"},
			"observation_interval_seconds": {"600"},
			"reason":                       {"縮短報到"},
		}).Body.String(), "/machines/configuration#publish"},
		"指派": {postForm(t, s, "/machines/configuration/assignment-preview", url.Values{
			"scope": {"machine"}, "scope_id": {id},
			"policy_id": {"steady-fleet"}, "policy_revision": {"1"}, "reason": {"先一台"},
		}).Body.String(), "/machines/configuration#policies"},
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
		Actions: []store.AuditAction{store.AuditSettingPolicy, store.AuditSettingAssign}, Limit: 10,
	})
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("預覽寫進了帳本：%+v err=%v", page.Items, err)
	}
}
