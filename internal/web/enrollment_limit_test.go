package web

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/operatorauth"
	"github.com/teddashh/AI-Intune/internal/store"
)

func enrollmentLimitWebFixture(t *testing.T, machines int) (*Server, *store.Store) {
	t.Helper()
	s, st := newServer(t)
	for index := 0; index < machines; index++ {
		if _, _, err := st.CreateEnrollTokenFor("web-limit-"+string(rune('a'+index)), time.Hour); err != nil {
			t.Fatal(err)
		}
	}
	return s, st
}

// previewEnrollmentLimitForm keeps the exact digest and request key rendered by
// the review page, so the confirmation under test is the one the operator saw.
func previewEnrollmentLimitForm(t *testing.T, s *Server, form url.Values) url.Values {
	t.Helper()
	preview := postForm(t, s, "/machines/enrollment/limit-preview", form)
	if preview.Code != http.StatusOK || !strings.Contains(preview.Body.String(), "</html>") {
		t.Fatalf("上限預覽 = %d：%s", preview.Code, preview.Body.String())
	}
	body := preview.Body.String()
	confirm := url.Values{
		"reason":            {strings.TrimSpace(form.Get("reason"))},
		"expected_revision": {hiddenFormValue(t, body, "expected_revision")},
		"preview_digest":    {hiddenFormValue(t, body, "preview_digest")},
		"idempotency_key":   {hiddenFormValue(t, body, "idempotency_key")},
	}
	if form.Get("clear") != "" {
		confirm.Set("clear", "1")
	} else {
		confirm.Set("max_machines", hiddenFormValue(t, body, "max_machines"))
	}
	return confirm
}

// 上限擺在裝置註冊那一頁，因為它擋的就是那一頁上的那個按鈕。
func TestTheEnrollmentPageSaysWhetherItCanStillTakeOneMore(t *testing.T) {
	s, _ := enrollmentLimitWebFixture(t, 2)
	page := get(t, s, "/machines/enrollment")
	for _, want := range []string{"註冊上限", "沒有設註冊上限", `action="/machines/enrollment/limit-preview"`} {
		if !strings.Contains(page, want) {
			t.Fatalf("裝置註冊頁少了 %q", want)
		}
	}
	if strings.Contains(page, "上限 0") {
		t.Fatal("沒設上限被講成上限 0")
	}
}

// 到上限的時候，開票那一段自己要講出來——一個看起來能按、按下去才說不行的按鈕，
// 等於把判決藏到最後一刻。
func TestTheEnrollmentPageSaysWhyTheTicketButtonWillRefuse(t *testing.T) {
	s, _ := enrollmentLimitWebFixture(t, 2)
	form := previewEnrollmentLimitForm(t, s, url.Values{
		"max_machines": {"2"}, "reason": {"acceptance"},
	})
	applied := postForm(t, s, "/machines/enrollment/limits", form)
	if applied.Code != http.StatusOK {
		t.Fatalf("套用上限 = %d：%s", applied.Code, applied.Body.String())
	}
	page := get(t, s, "/machines/enrollment")
	for _, want := range []string{"已達註冊上限", "現在開不了新的票", "退役"} {
		if !strings.Contains(page, want) {
			t.Fatalf("到上限了，畫面少了 %q", want)
		}
	}
}

// 確認頁要先說出「這不會退役任何一台」。少了那一句，操作的人會以為送出就會少幾台。
func TestTheLimitReviewSaysALowerLimitRetiresNothing(t *testing.T) {
	s, _ := enrollmentLimitWebFixture(t, 3)
	preview := postForm(t, s, "/machines/enrollment/limit-preview", url.Values{
		"max_machines": {"1"}, "reason": {"acceptance"},
	})
	if preview.Code != http.StatusOK {
		t.Fatalf("預覽 = %d：%s", preview.Code, preview.Body.String())
	}
	body := preview.Body.String()
	for _, want := range []string{"不會退役任何一台", "確認註冊上限", `action="/machines/enrollment/limits"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("確認頁少了 %q：\n%s", want, body)
		}
	}
}

// 空白的台數不可以被讀成 0。一個把空欄位當成 0 的表單，會在有人只想清掉輸入框的
// 那一刻把整個機隊鎖死。
func TestABlankLimitIsRefusedNotReadAsZero(t *testing.T) {
	s, st := enrollmentLimitWebFixture(t, 1)
	rec := postForm(t, s, "/machines/enrollment/limit-preview", url.Values{
		"max_machines": {""}, "reason": {"acceptance"},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("空白台數 = %d：%s", rec.Code, rec.Body.String())
	}
	state, err := st.EnrollmentLimit()
	if err != nil {
		t.Fatal(err)
	}
	if state.Set {
		t.Fatalf("空白台數被寫成上限：%+v", state)
	}
}

func TestMalformedEnrollmentLimitApplyKeepsTypedOperatorAudit(t *testing.T) {
	tests := []struct {
		name string
		form url.Values
		code operator.EnrollmentLimitTransportRejectionCode
	}{
		{"invalid limit", url.Values{
			"max_machines": {"four"}, "expected_revision": {"0"},
			"idempotency_key": {"bad-limit-form"},
		}, operator.EnrollmentLimitTransportRejectionFormInvalid},
		{"invalid revision", url.Values{
			"max_machines": {"4"}, "expected_revision": {"not-a-revision"},
			"idempotency_key": {"bad-limit-revision"},
		}, operator.EnrollmentLimitTransportRejectionRevisionInvalid},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s, st := enrollmentLimitWebFixture(t, 1)
			response := postForm(t, s, "/machines/enrollment/limits", test.form)
			if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "請重新預覽") {
				t.Fatalf("malformed apply=%d body=%s", response.Code, response.Body.String())
			}
			entries, err := st.Audit("", 10)
			if err != nil || len(entries) != 1 {
				t.Fatalf("transport audit=%+v err=%v", entries, err)
			}
			entry := entries[0]
			wantDetail := store.OperatorTransportRejectionPrefix + string(test.code) +
				": canonical request digest 無法取得"
			if entry.Action != store.AuditEnrollmentLimit || entry.Subject != "註冊上限" || entry.OK ||
				entry.Detail != wantDetail || entry.IdempotencyKey != test.form.Get("idempotency_key") ||
				entry.RequestDigest != "" || entry.Reason != "" || entry.SourceKind != operator.SourceKindWeb ||
				entry.AuthSubject != "tailscale-user:42" {
				t.Fatalf("transport audit shape=%+v", entry)
			}
		})
	}
}

func TestTheLimitRefusesWhatItCannotApply(t *testing.T) {
	s, _ := enrollmentLimitWebFixture(t, 1)
	for name, form := range map[string]url.Values{
		"沒有理由":     {"max_machines": {"4"}},
		"理由前後有空白":  {"max_machines": {"4"}, "reason": {" acceptance "}},
		"台數是負數":    {"max_machines": {"-1"}, "reason": {"acceptance"}},
		"台數不是數字":   {"max_machines": {"四"}, "reason": {"acceptance"}},
		"台數大到沒有意義": {"max_machines": {"10001"}, "reason": {"acceptance"}},
		// ⚠ 這幾個 Atoi 讀得出來。少了 canonical 那一關，"04" 跟 "4" 會變成兩個
		// 看起來一樣、digest 卻不同的請求。
		"台數前面補零":  {"max_machines": {"04"}, "reason": {"acceptance"}},
		"台數帶正號":   {"max_machines": {"+4"}, "reason": {"acceptance"}},
		"台數前後有空白": {"max_machines": {" 4"}, "reason": {"acceptance"}},
	} {
		if rec := postForm(t, s, "/machines/enrollment/limit-preview", form); rec.Code == http.StatusOK {
			t.Errorf("%s：被接受了", name)
		}
	}
}

// 同一份確認送兩次不可以改第二次，而且第二次要說出它沒有再改一次。
func TestResubmittingTheSameLimitConfirmationChangesNothingTwice(t *testing.T) {
	s, st := enrollmentLimitWebFixture(t, 1)
	form := previewEnrollmentLimitForm(t, s, url.Values{
		"max_machines": {"6"}, "reason": {"acceptance"},
	})
	if rec := postForm(t, s, "/machines/enrollment/limits", form); rec.Code != http.StatusOK {
		t.Fatalf("第一次 = %d：%s", rec.Code, rec.Body.String())
	}
	second := postForm(t, s, "/machines/enrollment/limits", form)
	if second.Code != http.StatusOK {
		t.Fatalf("第二次 = %d：%s", second.Code, second.Body.String())
	}
	if !strings.Contains(second.Body.String(), "已回放原判決") {
		t.Fatalf("重送沒有講出它沒有再改一次：\n%s", second.Body.String())
	}
	state, err := st.EnrollmentLimit()
	if err != nil {
		t.Fatal(err)
	}
	if state.Revision != 1 {
		t.Fatalf("重送改了第二次：%+v", state)
	}
}

// 取消上限也是 preview → commit，而且取消之後那一頁還要說得出誰拿掉的。
func TestClearingTheLimitFromTheWebKeepsWhoClearedIt(t *testing.T) {
	s, st := enrollmentLimitWebFixture(t, 1)
	set := previewEnrollmentLimitForm(t, s, url.Values{
		"max_machines": {"9"}, "reason": {"先設一個"},
	})
	if rec := postForm(t, s, "/machines/enrollment/limits", set); rec.Code != http.StatusOK {
		t.Fatalf("設上限 = %d：%s", rec.Code, rec.Body.String())
	}
	clear := previewEnrollmentLimitForm(t, s, url.Values{
		"clear": {"1"}, "reason": {"不再限制台數"},
	})
	if rec := postForm(t, s, "/machines/enrollment/limits", clear); rec.Code != http.StatusOK {
		t.Fatalf("取消上限 = %d：%s", rec.Code, rec.Body.String())
	}
	state, err := st.EnrollmentLimit()
	if err != nil {
		t.Fatal(err)
	}
	if state.Set || state.Revision != 2 || state.Reason != "不再限制台數" {
		t.Fatalf("取消之後 state=%+v", state)
	}
	if !strings.Contains(get(t, s, "/machines/enrollment"), "沒有設註冊上限") {
		t.Fatal("取消之後畫面還說有上限")
	}
}

// 只能看的人不可以看到改上限的表單。
func TestAViewOnlyOperatorSeesNoLimitForm(t *testing.T) {
	s, _ := enrollmentLimitWebFixture(t, 1)
	names, err := operatorauth.NamesForPrefix("example.com/cap/clawctl")
	if err != nil {
		t.Fatal(err)
	}
	page := get(t, s, "/machines/enrollment")
	if !strings.Contains(page, `action="/machines/enrollment/limit-preview"`) {
		t.Fatal("admin 看不到改上限的表單")
	}
	viewOnly := renderWithCapabilities(t, s, "/machines/enrollment",
		operatorauth.CapabilityNames{View: names.View})
	if strings.Contains(viewOnly, `action="/machines/enrollment/limit-preview"`) {
		t.Fatal("只能看的人看得到改上限的表單")
	}
	if !strings.Contains(viewOnly, "註冊上限") {
		t.Fatal("只能看的人看不到現在的上限")
	}
}
