package operatorclient

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/store"
)

func enrollmentLimitClientState(now time.Time) store.EnrollmentLimitState {
	return store.EnrollmentLimitState{
		Set: true, MaxMachines: 8, Revision: 3, Reason: "acceptance",
		UpdatedAt: now.Add(-time.Hour), UpdatedBy: "tailscale-user:1",
		InDenominator: 5, Retired: 2, Headroom: 3, AtLimit: false,
	}
}

func enrollmentLimitClientPage(now time.Time) operator.EnrollmentLimitResult {
	state := enrollmentLimitClientState(now)
	return operator.EnrollmentLimitResult{
		SchemaVersion: operator.EnrollmentLimitSchemaVersion, GeneratedAt: now,
		State: state, Headline: operator.EnrollmentLimitHeadline(state),
	}
}

func enrollmentLimitClientPreview(now time.Time) operator.EnrollmentLimitPreview {
	current := enrollmentLimitClientState(now)
	return operator.EnrollmentLimitPreview{
		OperatorEnrollmentLimitPreviewResult: store.OperatorEnrollmentLimitPreviewResult{
			Current: current, Set: true, MaxMachines: 12, AlreadyOver: false,
			PreviewedAt: now, PreviewDigest: "sha256:preview",
		},
		Headline: "上限設成 12 台", ExpectedRevision: current.Revision,
	}
}

func TestTheEnrollmentLimitClientAsksTheCanonicalPath(t *testing.T) {
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	client, asked := recordingReportServer(t, enrollmentLimitClientPage(now))
	page, err := client.EnrollmentLimit(t.Context())
	if err != nil {
		t.Fatalf("一致的註冊上限被拒絕：%v", err)
	}
	if *asked != "/v1/operator/enrollment-limit" {
		t.Fatalf("用戶端問的是 %q", *asked)
	}
	if page.State.Headroom != 3 || page.State.AtLimit {
		t.Fatalf("page=%+v", page.State)
	}
}

// 這一頁只有兩個數字會讓人改變行為：還可以再納管幾台、現在開不開得了票。一份說
// 「還收得下」但其實已經到頂的回應，會讓人一直重試一個永遠不會成功的動作。
func TestTheEnrollmentLimitClientRefusesAPageThatContradictsItself(t *testing.T) {
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	for name, edit := range map[string]func(*operator.EnrollmentLimitResult){
		"沒講 schema 版本": func(p *operator.EnrollmentLimitResult) { p.SchemaVersion++ },
		"沒有讀取時刻":       func(p *operator.EnrollmentLimitResult) { p.GeneratedAt = time.Time{} },
		"讀取時刻不是 UTC": func(p *operator.EnrollmentLimitResult) {
			p.GeneratedAt = p.GeneratedAt.In(time.FixedZone("CST", 8*3600))
		},
		"沒有講現在是什麼狀態": func(p *operator.EnrollmentLimitResult) { p.Headline = "" },
		"還可以納管的台數算錯": func(p *operator.EnrollmentLimitResult) { p.State.Headroom = 99 },
		"到頂了卻說還收得下": func(p *operator.EnrollmentLimitResult) {
			p.State.InDenominator, p.State.Headroom = p.State.MaxMachines, 0
		},
		"還收得下卻說到頂了": func(p *operator.EnrollmentLimitResult) { p.State.AtLimit = true },
		"已達上限卻沒有下一步": func(p *operator.EnrollmentLimitResult) {
			p.State.InDenominator, p.State.Headroom, p.State.AtLimit = p.State.MaxMachines, 0, true
		},
		"沒被擋住卻叫人做事":        func(p *operator.EnrollmentLimitResult) { p.NextStep = "去退役一台" },
		"有負數":              func(p *operator.EnrollmentLimitResult) { p.State.Retired = -1 },
		"設了上限卻沒有 revision": func(p *operator.EnrollmentLimitResult) { p.State.Revision = 0 },
		"設了上限卻不知道什麼時候改的": func(p *operator.EnrollmentLimitResult) {
			p.State.UpdatedAt = time.Time{}
		},
		"沒有上限卻帶著台數": func(p *operator.EnrollmentLimitResult) {
			p.State.Set, p.State.AtLimit, p.State.Headroom = false, false, 0
			p.Headline = "沒有設註冊上限"
		},
		"從來沒設定過卻說得出誰改的": func(p *operator.EnrollmentLimitResult) {
			p.State.Set, p.State.MaxMachines, p.State.Revision = false, 0, 0
			p.State.AtLimit, p.State.Headroom = false, 0
			p.Headline = "沒有設註冊上限"
		},
		"上限被改過卻說不出誰改的": func(p *operator.EnrollmentLimitResult) {
			p.State.Set, p.State.MaxMachines = false, 0
			p.State.AtLimit, p.State.Headroom = false, 0
			p.State.UpdatedBy, p.Headline = "", "沒有設註冊上限"
		},
	} {
		page := enrollmentLimitClientPage(now)
		edit(&page)
		client, _ := recordingReportServer(t, page)
		if _, err := client.EnrollmentLimit(t.Context()); err == nil {
			t.Errorf("%s：被接受了", name)
		}
	}
}

// 取消上限之後這一頁還是要說得出誰拿掉的、什麼時候。那是「為什麼現在沒有上限」
// 唯一不必去翻稽核就問得到的地方。
func TestTheEnrollmentLimitClientAcceptsAClearedLimitThatSaysWhoClearedIt(t *testing.T) {
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	page := enrollmentLimitClientPage(now)
	page.State.Set, page.State.MaxMachines = false, 0
	page.State.AtLimit, page.State.Headroom = false, 0
	page.Headline = operator.EnrollmentLimitHeadline(page.State)
	client, _ := recordingReportServer(t, page)
	if _, err := client.EnrollmentLimit(t.Context()); err != nil {
		t.Fatalf("一份講得出誰取消上限的回應被拒絕：%v", err)
	}
}

func TestTheEnrollmentLimitPreviewClientRefusesWhatContradictsTheRequest(t *testing.T) {
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	request := operator.EnrollmentLimitPreviewRequest{Set: true, MaxMachines: 12}
	for name, edit := range map[string]func(*operator.EnrollmentLimitPreview){
		"回的不是問的那個上限": func(p *operator.EnrollmentLimitPreview) { p.MaxMachines = 13 },
		"回的是取消上限":    func(p *operator.EnrollmentLimitPreview) { p.Set = false },
		"沒有 digest":  func(p *operator.EnrollmentLimitPreview) { p.PreviewDigest = "" },
		"沒有預覽時刻":     func(p *operator.EnrollmentLimitPreview) { p.PreviewedAt = time.Time{} },
		"帶回去的版本不是它看到的": func(p *operator.EnrollmentLimitPreview) {
			p.ExpectedRevision = p.Current.Revision + 1
		},
		"已經超過卻說沒有": func(p *operator.EnrollmentLimitPreview) {
			p.Current.InDenominator, p.Current.Headroom = p.MaxMachines+1, 0
			p.Current.AtLimit = p.Current.InDenominator >= p.Current.MaxMachines
		},
		"沒超過卻說已經超過": func(p *operator.EnrollmentLimitPreview) { p.AlreadyOver = true },
	} {
		preview := enrollmentLimitClientPreview(now)
		edit(&preview)
		client := enrollmentLimitClientServer(t, preview, nil)
		if _, err := client.PreviewEnrollmentLimit(t.Context(), request); err == nil {
			t.Errorf("%s：被接受了", name)
		}
	}
	client := enrollmentLimitClientServer(t, enrollmentLimitClientPreview(now), nil)
	if _, err := client.PreviewEnrollmentLimit(t.Context(), request); err != nil {
		t.Fatalf("一致的預覽被拒絕：%v", err)
	}
}

// ⚠ 這裡要數「有沒有打出去」，不是只看有沒有回錯誤。一個照樣送出去、然後被回應
// 的形狀擋下來的用戶端也會回錯誤——測試會通過，而那次不該發生的寫入請求已經到了
// Hub 門口。
func TestTheEnrollmentLimitClientRefusesWhatItCannotSendWithoutAsking(t *testing.T) {
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	client, calls := countingEnrollmentLimitServer(t, enrollmentLimitClientPreview(now))
	for name, request := range map[string]operator.EnrollmentLimitPreviewRequest{
		"取消上限還帶台數": {Set: false, MaxMachines: 3},
		"負數上限":     {Set: true, MaxMachines: -1},
		"上限大到沒有意義": {Set: true, MaxMachines: operator.MaxEnrollmentLimitMachines + 1},
	} {
		if _, err := client.PreviewEnrollmentLimit(t.Context(), request); err == nil {
			t.Errorf("%s：被接受了", name)
		}
	}
	good := EnrollmentLimitRequest{Set: true, MaxMachines: 12, ExpectedRevision: 3,
		PreviewDigest: "sha256:preview", Reason: "acceptance"}
	for name, edit := range map[string]func(*EnrollmentLimitRequest){
		"沒有 preview digest": func(r *EnrollmentLimitRequest) { r.PreviewDigest = "" },
		"沒有理由":              func(r *EnrollmentLimitRequest) { r.Reason = "" },
		"理由前後有空白":           func(r *EnrollmentLimitRequest) { r.Reason = " acceptance " },
		"理由太長":              func(r *EnrollmentLimitRequest) { r.Reason = strings.Repeat("x", 501) },
		"版本是負數":             func(r *EnrollmentLimitRequest) { r.ExpectedRevision = -1 },
		"取消上限還帶台數":          func(r *EnrollmentLimitRequest) { r.Set, r.MaxMachines = false, 3 },
	} {
		body := good
		edit(&body)
		if _, err := client.SetEnrollmentLimit(t.Context(), "k", body); err == nil {
			t.Errorf("%s：被接受了", name)
		}
	}
	if _, err := client.SetEnrollmentLimit(t.Context(), "", good); err == nil {
		t.Error("沒有 Idempotency-Key 卻被接受了")
	}
	if *calls != 0 {
		t.Fatalf("用戶端自己擋得下來的請求，還是打出去了 %d 次", *calls)
	}
}

func countingEnrollmentLimitServer(t *testing.T, body any) (*Client, *int) {
	t.Helper()
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(server.Close)
	return operatorClientForServer(t, server), &calls
}

// 回放帶回來的是現在的上限，不是當時改成什麼——所以回放那一次不可以拿請求去對，
// 但 header 與 body 兩邊必須說同一件事。
func TestTheEnrollmentLimitApplyClientChecksReplayEvidenceOnBothSides(t *testing.T) {
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	state := enrollmentLimitClientState(now)
	state.MaxMachines, state.Revision = 12, 4
	state.Headroom, state.AtLimit = state.MaxMachines-state.InDenominator, false
	body := EnrollmentLimitRequest{Set: true, MaxMachines: 12, ExpectedRevision: 3,
		PreviewDigest: "sha256:preview", Reason: "acceptance"}

	fresh := store.OperatorEnrollmentLimitResult{State: state, PreviousSet: true, PreviousMax: 8}
	if _, err := enrollmentLimitClientServer(t, nil, &applyResponse{body: fresh}).
		SetEnrollmentLimit(t.Context(), "k", body); err != nil {
		t.Fatalf("一致的套用結果被拒絕：%v", err)
	}
	for name, response := range map[string]*applyResponse{
		"body 說回放了、header 沒說": {body: store.OperatorEnrollmentLimitResult{
			State: state, Replayed: true}},
		"header 說回放了、body 沒說": {body: fresh, replayHeader: true},
		"套用後的上限不是送出的那一個": {body: func() store.OperatorEnrollmentLimitResult {
			other := state
			other.MaxMachines, other.Headroom = 13, 13-other.InDenominator
			return store.OperatorEnrollmentLimitResult{State: other}
		}()},
		"revision 沒有往前走": {body: func() store.OperatorEnrollmentLimitResult {
			stale := state
			stale.Revision = 3
			return store.OperatorEnrollmentLimitResult{State: stale}
		}()},
	} {
		if _, err := enrollmentLimitClientServer(t, nil, response).
			SetEnrollmentLimit(t.Context(), "k", body); err == nil {
			t.Errorf("%s：被接受了", name)
		}
	}

	// 回放是 header 與 body 都說了，而且帶回來的是現在的上限——它跟這次送出的
	// 內容可以不一樣，因為中間可能已經有別人改過。
	moved := state
	moved.MaxMachines, moved.Revision = 20, 9
	moved.Headroom = moved.MaxMachines - moved.InDenominator
	replay := applyResponse{
		body:         store.OperatorEnrollmentLimitResult{State: moved, Replayed: true},
		replayHeader: true,
	}
	if _, err := enrollmentLimitClientServer(t, nil, &replay).
		SetEnrollmentLimit(t.Context(), "k", body); err != nil {
		t.Fatalf("回放帶回現在的上限，卻被拒絕：%v", err)
	}
}

type applyResponse struct {
	body         store.OperatorEnrollmentLimitResult
	replayHeader bool
}

func enrollmentLimitClientServer(t *testing.T, preview any, apply *applyResponse) *Client {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		if apply != nil && r.URL.Path == "/v1/operator/enrollment-limit" {
			if apply.replayHeader {
				w.Header().Set("Idempotency-Replayed", "true")
			}
			_ = json.NewEncoder(w).Encode(apply.body)
			return
		}
		_ = json.NewEncoder(w).Encode(preview)
	}))
	t.Cleanup(server.Close)
	return operatorClientForServer(t, server)
}
