package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/operatorauth"
	"github.com/teddashh/AI-Intune/internal/store"
)

func limitPost(t *testing.T, f jobsFixture, path, body, key string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	req.RemoteAddr = "100.64.0.7:41234"
	f.mux.ServeHTTP(rec, verifiedOperatorRequest(req, operatorauth.Admin))
	return rec
}

func decodeEnrollmentLimit(t *testing.T, body []byte) operator.EnrollmentLimitResult {
	t.Helper()
	var page operator.EnrollmentLimitResult
	if err := json.Unmarshal(body, &page); err != nil {
		t.Fatal(err)
	}
	return page
}

// 上限算的就是註冊報告上的那個分母。兩頁講的必須是同一件事，否則被擋下來的人
// 會看到一頁說 5 台、另一頁說「上限 5 台、已有 6 台」，而沒有一頁看起來是錯的。
func TestOperatorEnrollmentLimitAPICountsTheSameDenominatorAsTheReport(t *testing.T) {
	f, _ := reportFixture(t)
	if _, _, err := f.store.CreateEnrollTokenFor("never-came", 0); err != nil {
		t.Fatal(err)
	}
	rec := reportGet(t, f, "/v1/operator/enrollment-limit")
	if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("status=%d cache=%q body=%s", rec.Code,
			rec.Header().Get("Cache-Control"), rec.Body.String())
	}
	page := decodeEnrollmentLimit(t, rec.Body.Bytes())
	if page.SchemaVersion != operator.EnrollmentLimitSchemaVersion || page.Headline == "" {
		t.Fatalf("page=%+v", page)
	}
	report := decodeEnrollmentReport(t, reportGet(t, f, "/v1/operator/enrollment-report").Body.Bytes())
	if page.State.InDenominator != report.Denominator || page.State.Retired != report.Retired {
		t.Fatalf("上限說 %d/%d，註冊報告說 %d/%d",
			page.State.InDenominator, page.State.Retired, report.Denominator, report.Retired)
	}
}

func TestOperatorEnrollmentLimitAPIRejectsAnyQuery(t *testing.T) {
	f, _ := reportFixture(t)
	for _, target := range []string{
		"/v1/operator/enrollment-limit?max=5",
		"/v1/operator/enrollment-limit?",
	} {
		if rec := reportGet(t, f, target); rec.Code != http.StatusBadRequest {
			t.Errorf("%s status=%d，應該拒絕", target, rec.Code)
		}
	}
}

// preview → commit 走完一遍，而且被拒的每一種都說得出 code。
func TestOperatorEnrollmentLimitAPIPreviewThenCommit(t *testing.T) {
	f, _ := reportFixture(t)
	preview := limitPost(t, f, "/v1/operator/enrollment-limit/preview",
		`{"set":true,"max_machines":9}`, "")
	if preview.Code != http.StatusOK {
		t.Fatalf("preview status=%d body=%s", preview.Code, preview.Body.String())
	}
	var previewed operator.EnrollmentLimitPreview
	if err := json.Unmarshal(preview.Body.Bytes(), &previewed); err != nil {
		t.Fatal(err)
	}
	if previewed.PreviewDigest == "" || previewed.Headline == "" {
		t.Fatalf("preview=%+v", previewed)
	}

	body := `{"set":true,"max_machines":9,"expected_revision":0,"preview_digest":"` +
		previewed.PreviewDigest + `","reason":"acceptance"}`
	applied := limitPost(t, f, "/v1/operator/enrollment-limit", body, "api-limit-1")
	if applied.Code != http.StatusOK {
		t.Fatalf("apply status=%d body=%s", applied.Code, applied.Body.String())
	}
	var result store.OperatorEnrollmentLimitResult
	if err := json.Unmarshal(applied.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !result.State.Set || result.State.MaxMachines != 9 || result.State.Revision != 1 || result.Replayed {
		t.Fatalf("result=%+v", result)
	}

	replay := limitPost(t, f, "/v1/operator/enrollment-limit", body, "api-limit-1")
	if replay.Code != http.StatusOK || replay.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatalf("replay status=%d header=%q body=%s", replay.Code,
			replay.Header().Get("Idempotency-Replayed"), replay.Body.String())
	}
}

func TestOperatorEnrollmentLimitAPIRefusesWhatItCannotRun(t *testing.T) {
	f, _ := reportFixture(t)
	preview := limitPost(t, f, "/v1/operator/enrollment-limit/preview",
		`{"set":true,"max_machines":9}`, "")
	var previewed operator.EnrollmentLimitPreview
	if err := json.Unmarshal(preview.Body.Bytes(), &previewed); err != nil {
		t.Fatal(err)
	}
	good := `{"set":true,"max_machines":9,"expected_revision":0,"preview_digest":"` +
		previewed.PreviewDigest + `","reason":"acceptance"}`
	// ⚠ 每一種都要用自己的 idempotency key。共用一把會讓第二種以後全部撞成
	// IDEMPOTENCY_CONFLICT，而那個 409 看起來也像「拒絕了」——測試就會通過，
	// 卻一個真正的判決都沒有驗到。
	for name, test := range map[string]struct {
		body, key string
		want      int
	}{
		"沒有 expected_revision": {`{"set":true,"max_machines":9,"preview_digest":"x","reason":"r"}`,
			"k-no-revision", http.StatusPreconditionRequired},
		"沒有 preview digest": {`{"set":true,"max_machines":9,"expected_revision":0,"reason":"r"}`,
			"k-no-digest", http.StatusPreconditionFailed},
		"預覽已經過期": {`{"set":true,"max_machines":9,"expected_revision":0,"preview_digest":"sha256:stale","reason":"r"}`,
			"k-stale", http.StatusPreconditionFailed},
		"版本對不上": {`{"set":true,"max_machines":9,"expected_revision":7,"preview_digest":"` +
			previewed.PreviewDigest + `","reason":"r"}`, "k-revision", http.StatusPreconditionFailed},
		"沒有理由": {`{"set":true,"max_machines":9,"expected_revision":0,"preview_digest":"` +
			previewed.PreviewDigest + `"}`, "k-no-reason", http.StatusBadRequest},
		"上限是負數": {`{"set":true,"max_machines":-1,"expected_revision":0,"preview_digest":"x","reason":"r"}`,
			"k-negative", http.StatusBadRequest},
		"不認得的欄位": {`{"set":true,"max_machines":9,"expected_revision":0,"preview_digest":"x","reason":"r","extra":1}`,
			"k-extra", http.StatusBadRequest},
		"沒有 idempotency key": {good, "", http.StatusBadRequest},
	} {
		rec := limitPost(t, f, "/v1/operator/enrollment-limit", test.body, test.key)
		if rec.Code != test.want {
			t.Errorf("%s status=%d，應該是 %d：%s", name, rec.Code, test.want, rec.Body.String())
		}
	}
	state, err := f.store.EnrollmentLimit()
	if err != nil {
		t.Fatal(err)
	}
	if state.Set {
		t.Fatalf("被拒的請求還是寫進去了：%+v", state)
	}
}

// 上限擋的是開票那一刻，而且被擋下來的回應要說得出現在有幾台、上限幾台。
func TestOperatorEnrollmentLimitBlocksTheEnrollTokenAPI(t *testing.T) {
	f, _ := reportFixture(t)
	current, err := f.store.EnrollmentLimit()
	if err != nil {
		t.Fatal(err)
	}
	max := current.InDenominator
	preview := limitPost(t, f, "/v1/operator/enrollment-limit/preview",
		`{"set":true,"max_machines":`+strconv.Itoa(max)+`}`, "")
	var previewed operator.EnrollmentLimitPreview
	if err := json.Unmarshal(preview.Body.Bytes(), &previewed); err != nil {
		t.Fatal(err)
	}
	applied := limitPost(t, f, "/v1/operator/enrollment-limit",
		`{"set":true,"max_machines":`+strconv.Itoa(max)+`,"expected_revision":0,"preview_digest":"`+
			previewed.PreviewDigest+`","reason":"acceptance"}`, "api-limit-block")
	if applied.Code != http.StatusOK {
		t.Fatalf("apply status=%d body=%s", applied.Code, applied.Body.String())
	}

	tokenPreview, err := f.store.PreviewOperatorEnrollToken("blocked", 3600)
	if err != nil {
		t.Fatal(err)
	}
	if !tokenPreview.AtLimit || tokenPreview.Headroom != 0 {
		t.Fatalf("開票預覽沒說它會被擋：%+v", tokenPreview)
	}
	rec := limitPost(t, f, "/v1/operator/enrollment-tokens",
		`{"display_name":"blocked","ttl_seconds":3600,"preview_digest":"`+
			tokenPreview.PreviewDigest+`","reason":"should be refused"}`, "api-blocked")
	// ⚠ 409 而不是 400：到上限是機隊現在的事實，不是送錯的 request。回 400 會讓
	// 呼叫端以為自己的 body 有問題，去改一個沒有錯的東西。
	if rec.Code != http.StatusConflict {
		t.Fatalf("到上限還開得了票，或狀態碼講錯了：status=%d body=%s", rec.Code, rec.Body.String())
	}
	for _, want := range []string{store.OperatorCodeEnrollmentLimitReached, "上限", "退役"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("被擋的回應少了 %q：%s", want, rec.Body.String())
		}
	}
}
