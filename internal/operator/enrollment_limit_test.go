package operator

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/store"
)

// Headroom 欄位雖然有 client 重算把關，卻沒有任何樣板替它核對句子裡的數字。
// 「我還能再納管幾台」只有這句話會告訴人，CLI 的人類輸出旁邊也沒有別的數字可供比對。
func TestTheLimitHeadlineSaysEveryCountInEveryState(t *testing.T) {
	fleet := enrollmentFixture(t)
	service := New(fleet.store)
	applyOperatorLimit(t, service, fleet.now, true, 8, "k-room")

	room, err := service.EnrollmentLimit(fleet.now)
	if err != nil {
		t.Fatal(err)
	}
	if room.State.MaxMachines != 8 || room.State.InDenominator != 5 || room.State.Retired != 1 || room.State.Headroom != 3 {
		t.Fatalf("state=%+v", room.State)
	}
	assertFourLimitCountsDiffer(t, room.State)
	want := fmt.Sprintf("上限 %d 台，名冊上 %d 台（已退役 %d 台不算），還可以再納管 %d 台。",
		room.State.MaxMachines, room.State.InDenominator, room.State.Retired, room.State.Headroom)
	if room.Headline != want {
		t.Fatalf("實際整句：%q；期望整句：%q", room.Headline, want)
	}

	unsetState := room.State
	unsetState.Set = false
	got := EnrollmentLimitHeadline(unsetState)
	want = fmt.Sprintf("沒有設註冊上限；名冊上 %d 台（已退役 %d 台不算）。",
		unsetState.InDenominator, unsetState.Retired)
	if got != want {
		t.Fatalf("實際整句：%q；期望整句：%q", got, want)
	}

	applyOperatorLimit(t, service, fleet.now, true, 4, "k-full")
	full, err := service.EnrollmentLimit(fleet.now)
	if err != nil {
		t.Fatal(err)
	}
	if !full.State.AtLimit || full.State.MaxMachines != 4 || full.State.InDenominator != 5 || full.State.Retired != 1 || full.State.Headroom != 0 {
		t.Fatalf("state=%+v", full.State)
	}
	assertFourLimitCountsDiffer(t, full.State)
	want = fmt.Sprintf("上限 %d 台，名冊上已有 %d 台（已退役 %d 台不算）——現在開不了新的票。",
		full.State.MaxMachines, full.State.InDenominator, full.State.Retired)
	if full.Headline != want {
		t.Fatalf("實際整句：%q；期望整句：%q", full.Headline, want)
	}
}

// Headroom 欄位雖然有 client 重算把關，卻沒有任何樣板替它核對句子裡的數字。
// 「我還能再納管幾台」只有這句話會告訴人，CLI 的人類輸出旁邊也沒有別的數字可供比對。
func TestThePreviewHeadlineSaysEveryCountTheChangeWillLeave(t *testing.T) {
	fleet := enrollmentFixture(t)
	service := New(fleet.store)
	applyOperatorLimit(t, service, fleet.now, true, 8, "k-room")

	current, err := service.EnrollmentLimit(fleet.now)
	if err != nil {
		t.Fatal(err)
	}
	if current.State.MaxMachines != 8 || current.State.InDenominator != 5 || current.State.Retired != 1 || current.State.Headroom != 3 {
		t.Fatalf("state=%+v", current.State)
	}
	assertFourLimitCountsDiffer(t, current.State)

	cleared, err := service.PreviewEnrollmentLimit(EnrollmentLimitPreviewRequest{})
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("取消上限（現在是 %d 台）；之後開票不再受台數限制。", cleared.Current.MaxMachines)
	if cleared.Headline != want {
		t.Fatalf("實際整句：%q；期望整句：%q", cleared.Headline, want)
	}

	lower, err := service.PreviewEnrollmentLimit(EnrollmentLimitPreviewRequest{Set: true, MaxMachines: 2})
	if err != nil {
		t.Fatal(err)
	}
	want = fmt.Sprintf("上限設成 %d 台，名冊上已有 %d 台——不會退役任何一台，但在退役到 %d 台以下之前開不了新的票。",
		lower.MaxMachines, lower.Current.InDenominator, lower.MaxMachines)
	if lower.Headline != want {
		t.Fatalf("實際整句：%q；期望整句：%q", lower.Headline, want)
	}

	higher, err := service.PreviewEnrollmentLimit(EnrollmentLimitPreviewRequest{Set: true, MaxMachines: 12})
	if err != nil {
		t.Fatal(err)
	}
	want = fmt.Sprintf("上限設成 %d 台，名冊上 %d 台，之後還可以再納管 %d 台。",
		higher.MaxMachines, higher.Current.InDenominator, higher.MaxMachines-higher.Current.InDenominator)
	if higher.Headline != want {
		t.Fatalf("實際整句：%q；期望整句：%q", higher.Headline, want)
	}
}

func assertFourLimitCountsDiffer(t *testing.T, state EnrollmentLimitState) {
	t.Helper()
	counts := []int{state.MaxMachines, state.InDenominator, state.Retired, state.Headroom}
	for left := range counts {
		for right := left + 1; right < len(counts); right++ {
			if counts[left] == counts[right] {
				t.Fatalf("fixture 沒造出四個兩兩不同的數字：state=%+v", state)
			}
		}
	}
}

// 上限數的那個數字必須就是註冊報告上的分母。它們一旦漂開，被擋下來的人會看到
// 一頁說「5 台」、另一頁說「上限 5 台、已有 6 台」，而沒有任何一頁是錯的樣子。
func TestTheLimitCountsExactlyWhatTheEnrollmentReportCallsTheDenominator(t *testing.T) {
	fleet := enrollmentFixture(t)
	service := New(fleet.store)

	report, err := service.EnrollmentReport(fleet.now)
	if err != nil {
		t.Fatal(err)
	}
	limit, err := service.EnrollmentLimit(fleet.now)
	if err != nil {
		t.Fatal(err)
	}
	if limit.State.InDenominator != report.Denominator {
		t.Fatalf("上限數 %d 台，註冊報告的分母是 %d 台", limit.State.InDenominator, report.Denominator)
	}
	if limit.State.Retired != report.Retired {
		t.Fatalf("上限說退役 %d 台，註冊報告說 %d 台", limit.State.Retired, report.Retired)
	}
}

// 「沒有設上限」跟「上限 0」在畫面上必須是兩句不同的話。
func TestTheLimitPageNeverCallsNoLimitALimitOfZero(t *testing.T) {
	fleet := enrollmentFixture(t)
	service := New(fleet.store)

	unset, err := service.EnrollmentLimit(fleet.now)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(unset.Headline, "沒有設註冊上限") || strings.Contains(unset.Headline, "上限 0") {
		t.Fatalf("沒設上限講成：%s", unset.Headline)
	}
	if unset.NextStep != "" {
		t.Fatalf("沒設上限卻叫人做事：%s", unset.NextStep)
	}

	applyOperatorLimit(t, service, fleet.now, true, 0, "k-zero")
	zero, err := service.EnrollmentLimit(fleet.now)
	if err != nil {
		t.Fatal(err)
	}
	if !zero.State.Set || !zero.State.AtLimit {
		t.Fatalf("state=%+v", zero.State)
	}
	if strings.Contains(zero.Headline, "沒有設註冊上限") {
		t.Fatalf("上限 0 講成沒有上限：%s", zero.Headline)
	}
	if zero.NextStep == "" {
		t.Fatal("開不了票卻沒說下一步是什麼")
	}
}

// 到上限的那一頁要說出現在有幾台、上限幾台，還有怎麼讓出位子。
func TestTheLimitPageSaysWhatIsBlockingAndHowToClearIt(t *testing.T) {
	fleet := enrollmentFixture(t)
	service := New(fleet.store)
	report, err := service.EnrollmentReport(fleet.now)
	if err != nil {
		t.Fatal(err)
	}
	applyOperatorLimit(t, service, fleet.now, true, report.Denominator, "k-at")

	page, err := service.EnrollmentLimit(fleet.now)
	if err != nil {
		t.Fatal(err)
	}
	if !page.State.AtLimit || page.State.Headroom != 0 {
		t.Fatalf("state=%+v", page.State)
	}
	for _, want := range []string{"開不了新的票", "退役"} {
		if !strings.Contains(page.Headline+page.NextStep, want) {
			t.Errorf("這一頁少了 %q：%s / %s", want, page.Headline, page.NextStep)
		}
	}
}

// 預覽講的是按下去會發生什麼。把上限調到現在的台數以下不會退役任何一台，那一句
// 必須先講出來——不然操作的人會以為送出就會少幾台機器。
func TestThePreviewSaysALowerLimitRetiresNothing(t *testing.T) {
	fleet := enrollmentFixture(t)
	service := New(fleet.store)
	preview, err := service.PreviewEnrollmentLimit(EnrollmentLimitPreviewRequest{Set: true, MaxMachines: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !preview.AlreadyOver {
		t.Fatal("預覽沒說新上限已經比現在的台數小")
	}
	if !strings.Contains(preview.Headline, "不會退役任何一台") {
		t.Fatalf("預覽沒講清楚它不會退役機器：%s", preview.Headline)
	}
	if preview.ExpectedRevision != preview.Current.Revision {
		t.Fatalf("預覽帶回去的版本 %d，它看到的是 %d", preview.ExpectedRevision, preview.Current.Revision)
	}
}

func TestThePreviewOfClearingAnUnsetLimitSaysItChangesNothing(t *testing.T) {
	fleet := enrollmentFixture(t)
	service := New(fleet.store)
	preview, err := service.PreviewEnrollmentLimit(EnrollmentLimitPreviewRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(preview.Headline, "不會改變任何事") {
		t.Fatalf("preview headline=%s", preview.Headline)
	}
}

// 同一個意思用兩種寫法送進來要 digest 成同一個；取消上限時帶進來的台數沒有意義。
func TestClearingTheLimitDigestsTheSameWhateverMaxIsSent(t *testing.T) {
	base := EnrollmentLimitApplyRequest{Reason: "r", PreviewDigest: "sha256:p", ExpectedRevision: 2}
	first, second := base, base
	second.MaxMachines = 99
	if EnrollmentLimitSemanticDigest(first) != EnrollmentLimitSemanticDigest(second) {
		t.Fatal("取消上限時的台數改變了 digest")
	}
	third := base
	third.Set, third.MaxMachines = true, 99
	if EnrollmentLimitSemanticDigest(first) == EnrollmentLimitSemanticDigest(third) {
		t.Fatal("設上限跟取消上限 digest 成同一個")
	}
}

// 被上限擋掉的開票要以 client 讀得懂的 code 回來，而且要留在稽核裡。
func TestEnrollmentRefusedByTheLimitIsAuditedWithItsCode(t *testing.T) {
	fleet := enrollmentFixture(t)
	service := New(fleet.store)
	report, err := service.EnrollmentReport(fleet.now)
	if err != nil {
		t.Fatal(err)
	}
	applyOperatorLimit(t, service, fleet.now, true, report.Denominator, "k-at")

	preview, err := fleet.store.PreviewOperatorEnrollToken("blocked", 3600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.CreateEnrollToken(EnrollTokenCreateRequest{
		DisplayName: "blocked", TTLSeconds: 3600, PreviewDigest: preview.PreviewDigest,
		Reason: "should be refused", IdempotencyKey: "k-blocked",
		Actor: Actor{AuthSubject: "tailscale-user:1", SourceAddr: "100.64.0.10"},
	})
	var rejection *store.OperatorRequestError
	if !errors.As(err, &rejection) || rejection.Code != store.OperatorCodeEnrollmentLimitReached {
		t.Fatalf("到上限還開得了票：%v", err)
	}
	if !strings.Contains(rejection.Detail, "上限") {
		t.Fatalf("拒絕的說明沒講上限：%s", rejection.Detail)
	}

	entries, err := service.ListAudit(AuditListRequest{
		Actions: []store.AuditAction{store.AuditEnrollToken},
		Outcome: store.AuditOutcomeFailed, Denials: AuditDenialsAll, Limit: 50,
	}, fleet.now)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, entry := range entries.Items {
		if entry.Detail != nil && strings.Contains(*entry.Detail, store.OperatorCodeEnrollmentLimitReached) {
			found = true
		}
	}
	if !found {
		t.Fatal("被上限擋掉的開票沒有留在稽核裡")
	}
}

func applyOperatorLimit(t *testing.T, service *Service, now time.Time, set bool, max int, key string) {
	t.Helper()
	preview, err := service.PreviewEnrollmentLimit(EnrollmentLimitPreviewRequest{Set: set, MaxMachines: max})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.SetEnrollmentLimit(EnrollmentLimitApplyRequest{
		EnrollmentLimitPreviewRequest: EnrollmentLimitPreviewRequest{Set: set, MaxMachines: max},
		Reason:                        "acceptance", ExpectedRevision: preview.ExpectedRevision,
		PreviewDigest: preview.PreviewDigest, IdempotencyKey: key,
		Actor: Actor{AuthSubject: "tailscale-user:1", SourceAddr: "100.64.0.10"},
	}); err != nil {
		t.Fatalf("設上限失敗：%v", err)
	}
	_ = now
}

// Store 進交易之前就拒絕的請求，稽核要由這一層補上。少了它，「誰想改上限」會有
// 一段完全看不見的區間——而那正是送壞請求的人會落在的地方。
func TestALimitChangeRefusedBeforeTheStoreWritesIsStillAudited(t *testing.T) {
	fleet := enrollmentFixture(t)
	service := New(fleet.store)
	if _, err := service.SetEnrollmentLimit(EnrollmentLimitApplyRequest{
		EnrollmentLimitPreviewRequest: EnrollmentLimitPreviewRequest{Set: true, MaxMachines: 4},
		Reason:                        "no key", PreviewDigest: "sha256:x",
		Actor: Actor{AuthSubject: "tailscale-user:1", SourceAddr: "100.64.0.10"},
	}); err == nil {
		t.Fatal("沒有 idempotency key 卻被接受")
	}
	entries, err := service.ListAudit(AuditListRequest{
		Actions: []store.AuditAction{store.AuditEnrollmentLimit},
		Outcome: store.AuditOutcomeFailed, Denials: AuditDenialsAll, Limit: 50,
	}, fleet.now)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries.Items) != 1 {
		t.Fatalf("進交易之前被拒的請求留下 %d 筆稽核", len(entries.Items))
	}
	if entries.Items[0].Subject != "上限 4 台" {
		t.Fatalf("稽核沒說出它想改成什麼：%q", entries.Items[0].Subject)
	}
}

func TestEnrollmentLimitTransportRejectionOwnsItsAuditShape(t *testing.T) {
	fleet := enrollmentFixture(t)
	service := New(fleet.store)
	request := EnrollmentLimitTransportRejectionRequest{
		Code: EnrollmentLimitTransportRejectionRevisionInvalid, IdempotencyKey: "web-limit-malformed",
		Actor: Actor{
			SourceAddr: "100.64.0.10", AuthSubject: "tailscale-user:1",
			AuthCapability: "admin", AuthDecision: "authorized", SourceKind: SourceKindWeb,
		},
	}
	if err := service.RecordEnrollmentLimitTransportRejection(request); err != nil {
		t.Fatal(err)
	}
	entries, err := fleet.store.Audit("", 10)
	if err != nil || len(entries) != 1 {
		t.Fatalf("transport audit=%+v err=%v", entries, err)
	}
	entry := entries[0]
	wantDetail := store.OperatorTransportRejectionPrefix + string(request.Code) +
		": canonical request digest 無法取得"
	if entry.Action != store.AuditEnrollmentLimit || entry.Subject != "註冊上限" || entry.OK ||
		entry.IdempotencyKey != request.IdempotencyKey || entry.RequestDigest != "" || entry.Reason != "" ||
		entry.Detail != wantDetail || entry.SourceAddr != request.Actor.SourceAddr ||
		entry.AuthSubject != request.Actor.AuthSubject || entry.AuthCapability != request.Actor.AuthCapability ||
		entry.AuthDecision != request.Actor.AuthDecision || entry.SourceKind != SourceKindWeb {
		t.Fatalf("transport audit shape=%+v", entry)
	}

	request.Code = EnrollmentLimitTransportRejectionCode("FREE_FORM_DETAIL")
	if err := service.RecordEnrollmentLimitTransportRejection(request); err == nil {
		t.Fatal("operator service accepted an unbounded transport rejection code")
	}
	entries, err = fleet.store.Audit("", 10)
	if err != nil || len(entries) != 1 {
		t.Fatalf("invalid code wrote an audit row: %+v err=%v", entries, err)
	}
}
