package store

import (
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"
)

func enrollmentLimitTestStore(t *testing.T, machines int) *Store {
	t.Helper()
	st := newOperatorEnrollTestStore(t)
	for index := 0; index < machines; index++ {
		if _, _, err := st.CreateEnrollTokenFor("m"+strconv.Itoa(index), time.Hour); err != nil {
			t.Fatal(err)
		}
	}
	return st
}

func setEnrollmentLimit(t *testing.T, st *Store, key string, set bool, max int) OperatorEnrollmentLimitResult {
	t.Helper()
	preview, err := st.PreviewOperatorEnrollmentLimit(set, max)
	if err != nil {
		t.Fatal(err)
	}
	result, err := st.ApplyOperatorEnrollmentLimit(OperatorEnrollmentLimitRequest{
		Set: set, MaxMachines: max, Reason: "acceptance",
		ExpectedRevision: preview.Current.Revision, PreviewDigest: preview.PreviewDigest,
		IdempotencyKey: key, RequestDigest: "sha256:" + key, UpdatedBy: "tailscale-user:1",
		Audit: AuditEntry{SourceAddr: "100.64.0.10", AuthSubject: "tailscale-user:1",
			AuthCapability: "example.com/cap/clawctl-admin", SourceKind: "operator-api"},
	})
	if err != nil {
		t.Fatalf("設上限失敗：%v", err)
	}
	return result
}

func issueUnderLimit(t *testing.T, st *Store, name, key string) error {
	t.Helper()
	preview, err := st.PreviewOperatorEnrollToken(name, 3600)
	if err != nil {
		t.Fatal(err)
	}
	req := operatorEnrollTestRequest(preview)
	req.IdempotencyKey, req.RequestDigest = key, "sha256:"+key
	_, err = st.ApplyOperatorEnrollToken(req)
	return err
}

// 沒有設上限就是沒有上限，不是上限 0。這兩件事在畫面上是完全相反的意思。
func TestNoEnrollmentLimitIsNotALimitOfZero(t *testing.T) {
	st := enrollmentLimitTestStore(t, 3)
	state, err := st.EnrollmentLimit()
	if err != nil {
		t.Fatal(err)
	}
	if state.Set || state.AtLimit || state.InDenominator != 3 {
		t.Fatalf("state=%+v", state)
	}
	if err := issueUnderLimit(t, st, "no-limit", "k-nolimit"); err != nil {
		t.Fatalf("沒有上限卻擋住開票：%v", err)
	}
}

// 上限擋的就是開票那一刻，而且它說得出當下的數字：那是這個判決的證據。
func TestOpeningATicketIsRefusedAtTheLimit(t *testing.T) {
	st := enrollmentLimitTestStore(t, 3)
	setEnrollmentLimit(t, st, "k-set", true, 3)

	err := issueUnderLimit(t, st, "over", "k-over")
	var rejection *OperatorRequestError
	if !errors.As(err, &rejection) || rejection.Code != OperatorCodeEnrollmentLimitReached {
		t.Fatalf("到上限卻開得出票：%v", err)
	}
	for _, want := range []string{"3 台", "上限 3 台", "退役"} {
		if !strings.Contains(rejection.Detail, want) {
			t.Errorf("拒絕的說明少了 %q：%s", want, rejection.Detail)
		}
	}
	state, err := st.EnrollmentLimit()
	if err != nil {
		t.Fatal(err)
	}
	if state.InDenominator != 3 {
		t.Fatalf("被拒絕的開票留下了名冊列：%+v", state)
	}
}

// 撤票不會讓一列名冊消失，所以它也不會讓出一個位子。上限跟註冊報告的分母是同一套
// 規則：離開分母只有退役一條路。
func TestRevokingATicketDoesNotMakeRoomButRetiringDoes(t *testing.T) {
	st := enrollmentLimitTestStore(t, 3)
	setEnrollmentLimit(t, st, "k-set", true, 3)

	var machineID string
	if err := st.db.QueryRow(
		`SELECT machine_id FROM machine_registry ORDER BY created_at LIMIT 1`).Scan(&machineID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RevokeEnrollToken(machineID); err != nil {
		t.Fatal(err)
	}
	if err := issueUnderLimit(t, st, "after-revoke", "k-after-revoke"); err == nil {
		t.Fatal("撤票之後就讓出了一個位子")
	}

	if err := st.RetireMachine(machineID, st.now()); err != nil {
		t.Fatal(err)
	}
	if err := issueUnderLimit(t, st, "after-retire", "k-after-retire"); err != nil {
		t.Fatalf("退役之後還是開不了票：%v", err)
	}
}

// 上限調到現在的台數以下不會退役任何一台。它只讓下一次開票被擋——一個會自己動手
// 縮小機隊的設定，會在有人手滑打錯數字的那天刪掉正在服役的機器。
func TestALimitBelowTheCurrentCountRetiresNothing(t *testing.T) {
	st := enrollmentLimitTestStore(t, 5)
	preview, err := st.PreviewOperatorEnrollmentLimit(true, 2)
	if err != nil {
		t.Fatal(err)
	}
	if !preview.AlreadyOver {
		t.Fatal("預覽沒有講出新上限已經比現在的台數小")
	}
	result := setEnrollmentLimit(t, st, "k-low", true, 2)
	if result.State.InDenominator != 5 || result.State.Headroom != 0 || !result.State.AtLimit {
		t.Fatalf("state=%+v", result.State)
	}
	if err := issueUnderLimit(t, st, "blocked", "k-blocked"); err == nil {
		t.Fatal("超過上限還開得了票")
	}
}

func TestClearingTheLimitLetsEnrollmentThroughAgain(t *testing.T) {
	st := enrollmentLimitTestStore(t, 2)
	setEnrollmentLimit(t, st, "k-set", true, 2)
	if err := issueUnderLimit(t, st, "blocked", "k-blocked"); err == nil {
		t.Fatal("到上限還開得了票")
	}
	cleared := setEnrollmentLimit(t, st, "k-clear", false, 0)
	if cleared.State.Set || !cleared.PreviousSet || cleared.PreviousMax != 2 {
		t.Fatalf("result=%+v", cleared)
	}
	if err := issueUnderLimit(t, st, "allowed", "k-allowed"); err != nil {
		t.Fatalf("取消上限之後還是開不了票：%v", err)
	}
}

// 兩個人同時改上限時，後送出的那一個必須重看一次。少了這一關，他會照著一份已經被
// 覆蓋掉的畫面做決定。
func TestChangingTheLimitNeedsTheRevisionItWasShown(t *testing.T) {
	st := enrollmentLimitTestStore(t, 1)
	stale, err := st.PreviewOperatorEnrollmentLimit(true, 9)
	if err != nil {
		t.Fatal(err)
	}
	setEnrollmentLimit(t, st, "k-first", true, 4)

	_, err = st.ApplyOperatorEnrollmentLimit(OperatorEnrollmentLimitRequest{
		Set: true, MaxMachines: 9, Reason: "stale",
		ExpectedRevision: stale.Current.Revision, PreviewDigest: stale.PreviewDigest,
		IdempotencyKey: "k-stale", RequestDigest: "sha256:k-stale", UpdatedBy: "tailscale-user:1",
		Audit: AuditEntry{SourceAddr: "100.64.0.10"},
	})
	var rejection *OperatorRequestError
	if !errors.As(err, &rejection) || rejection.Code != OperatorCodePreconditionFailed {
		t.Fatalf("過期的預覽被接受了：%v", err)
	}
	state, err := st.EnrollmentLimit()
	if err != nil {
		t.Fatal(err)
	}
	if state.MaxMachines != 4 {
		t.Fatalf("被拒絕的修改還是寫進去了：%+v", state)
	}
}

// 預覽釘住的是「改之前的上限」，不是「現在有幾台」。分母每分鐘都在變，把它放進
// digest 等於每一次預覽在按下送出之前就過期了。
func TestTheLimitPreviewDoesNotGoStaleWhenAMachineArrives(t *testing.T) {
	st := enrollmentLimitTestStore(t, 1)
	preview, err := st.PreviewOperatorEnrollmentLimit(true, 9)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.CreateEnrollTokenFor("arrived-meanwhile", time.Hour); err != nil {
		t.Fatal(err)
	}
	result, err := st.ApplyOperatorEnrollmentLimit(OperatorEnrollmentLimitRequest{
		Set: true, MaxMachines: 9, Reason: "still valid",
		ExpectedRevision: preview.Current.Revision, PreviewDigest: preview.PreviewDigest,
		IdempotencyKey: "k-later", RequestDigest: "sha256:k-later", UpdatedBy: "tailscale-user:1",
		Audit: AuditEntry{SourceAddr: "100.64.0.10"},
	})
	if err != nil {
		t.Fatalf("中間多了一台就把預覽作廢了：%v", err)
	}
	if result.State.MaxMachines != 9 || result.State.InDenominator != 2 {
		t.Fatalf("state=%+v", result.State)
	}
}

// 同一把 key 重送不可以改第二次，而且回放讀的是現在的狀態，不是收據裡那一份。
func TestReplayingALimitChangeChangesNothingTwice(t *testing.T) {
	st := enrollmentLimitTestStore(t, 1)
	preview, err := st.PreviewOperatorEnrollmentLimit(true, 6)
	if err != nil {
		t.Fatal(err)
	}
	req := OperatorEnrollmentLimitRequest{
		Set: true, MaxMachines: 6, Reason: "once",
		ExpectedRevision: preview.Current.Revision, PreviewDigest: preview.PreviewDigest,
		IdempotencyKey: "k-once", RequestDigest: "sha256:k-once", UpdatedBy: "tailscale-user:1",
		Audit: AuditEntry{SourceAddr: "100.64.0.10"},
	}
	first, err := st.ApplyOperatorEnrollmentLimit(req)
	if err != nil || first.Replayed || first.State.Revision != 1 {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	second, err := st.ApplyOperatorEnrollmentLimit(req)
	if err != nil || !second.Replayed || second.State.Revision != 1 {
		t.Fatalf("second=%+v err=%v", second, err)
	}
}

func TestTheLimitRefusesWhatItCannotStore(t *testing.T) {
	for name, edit := range map[string]func(*OperatorEnrollmentLimitRequest){
		"沒有 idempotency key": func(r *OperatorEnrollmentLimitRequest) { r.IdempotencyKey = "" },
		"沒有 request digest":  func(r *OperatorEnrollmentLimitRequest) { r.RequestDigest = "" },
		"沒有理由":               func(r *OperatorEnrollmentLimitRequest) { r.Reason = "  " },
		"理由太長":               func(r *OperatorEnrollmentLimitRequest) { r.Reason = strings.Repeat("x", 501) },
		"上限是負數":              func(r *OperatorEnrollmentLimitRequest) { r.MaxMachines = -1 },
		"沒有預覽":               func(r *OperatorEnrollmentLimitRequest) { r.PreviewDigest = "" },
	} {
		st := enrollmentLimitTestStore(t, 1)
		preview, err := st.PreviewOperatorEnrollmentLimit(true, 6)
		if err != nil {
			t.Fatal(err)
		}
		req := OperatorEnrollmentLimitRequest{
			Set: true, MaxMachines: 6, Reason: "acceptance",
			ExpectedRevision: preview.Current.Revision, PreviewDigest: preview.PreviewDigest,
			IdempotencyKey: "k", RequestDigest: "sha256:k", UpdatedBy: "tailscale-user:1",
			Audit: AuditEntry{SourceAddr: "100.64.0.10"},
		}
		edit(&req)
		if _, err := st.ApplyOperatorEnrollmentLimit(req); err == nil {
			t.Errorf("%s：被接受了", name)
		}
	}
}

// 上限 0 是一個合法的、講得出來的設定：現在誰都不准再納管。它跟「沒有設上限」
// 必須是兩件不同的事。
func TestALimitOfZeroStopsEveryNewMachine(t *testing.T) {
	st := enrollmentLimitTestStore(t, 0)
	result := setEnrollmentLimit(t, st, "k-zero", true, 0)
	if !result.State.Set || result.State.MaxMachines != 0 || !result.State.AtLimit {
		t.Fatalf("state=%+v", result.State)
	}
	if err := issueUnderLimit(t, st, "nope", "k-nope"); err == nil {
		t.Fatal("上限 0 還開得了票")
	}
}

// 改上限這件事本身要留在稽核裡，成功與被拒都要。
func TestEveryLimitChangeIsAudited(t *testing.T) {
	st := enrollmentLimitTestStore(t, 1)
	setEnrollmentLimit(t, st, "k-set", true, 4)
	var ok, failed int
	if err := st.db.QueryRow(`SELECT
	 COALESCE(SUM(CASE WHEN outcome='ok' THEN 1 ELSE 0 END),0),
	 COALESCE(SUM(CASE WHEN outcome='failed' THEN 1 ELSE 0 END),0)
	 FROM audit_log WHERE action=?`, string(AuditEnrollmentLimit)).Scan(&ok, &failed); err != nil {
		t.Fatal(err)
	}
	if ok != 1 || failed != 0 {
		t.Fatalf("設上限的稽核 ok=%d failed=%d", ok, failed)
	}
	_, err := st.ApplyOperatorEnrollmentLimit(OperatorEnrollmentLimitRequest{
		Set: true, MaxMachines: 4, Reason: "no preview",
		IdempotencyKey: "k-bad", RequestDigest: "sha256:k-bad", UpdatedBy: "tailscale-user:1",
		Audit: AuditEntry{SourceAddr: "100.64.0.10"},
	})
	if err == nil {
		t.Fatal("沒有預覽卻被接受")
	}
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM audit_log
	 WHERE action=? AND outcome='failed'`, string(AuditEnrollmentLimit)).Scan(&failed); err != nil {
		t.Fatal(err)
	}
	if failed != 1 {
		t.Fatalf("被拒的修改沒有留下稽核：failed=%d", failed)
	}
}

// 被上限擋掉的開票也要留在稽核裡，否則「為什麼我開不了票」只剩下人的記憶。
func TestARefusedEnrollmentIsAudited(t *testing.T) {
	st := enrollmentLimitTestStore(t, 2)
	setEnrollmentLimit(t, st, "k-set", true, 2)
	if err := issueUnderLimit(t, st, "blocked", "k-blocked"); err == nil {
		t.Fatal("到上限還開得了票")
	}
	var detail string
	if err := st.db.QueryRow(`SELECT COALESCE(detail,'') FROM audit_log
	 WHERE action=? AND outcome='failed' ORDER BY audit_id DESC LIMIT 1`,
		string(AuditEnrollToken)).Scan(&detail); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(detail, OperatorCodeEnrollmentLimitReached) {
		t.Fatalf("被上限擋掉的開票沒有留下可辨認的稽核：%s", detail)
	}
}

// 設了上限又取消，revision 不可以退回去。它是「你看到的還是不是現在這一份」的
// 唯一判準；一個會倒退的版本號，會讓一份設定之前就拿到的預覽在取消之後重新變成
// 有效的——而那份預覽是照著一個已經發生過兩次變更的世界做的決定。
func TestTheLimitRevisionOnlyEverGoesForward(t *testing.T) {
	st := enrollmentLimitTestStore(t, 1)
	stale, err := st.PreviewOperatorEnrollmentLimit(true, 9)
	if err != nil {
		t.Fatal(err)
	}
	setEnrollmentLimit(t, st, "k-set", true, 4)
	cleared := setEnrollmentLimit(t, st, "k-clear", false, 0)
	if cleared.State.Set || cleared.State.Revision != 2 {
		t.Fatalf("取消之後 state=%+v", cleared.State)
	}
	if cleared.State.Reason == "" || cleared.State.UpdatedBy == "" {
		t.Fatalf("取消上限沒有留下誰改的、為什麼：%+v", cleared.State)
	}

	_, err = st.ApplyOperatorEnrollmentLimit(OperatorEnrollmentLimitRequest{
		Set: true, MaxMachines: 9, Reason: "stale after clear",
		ExpectedRevision: stale.Current.Revision, PreviewDigest: stale.PreviewDigest,
		IdempotencyKey: "k-aba", RequestDigest: "sha256:k-aba", UpdatedBy: "tailscale-user:1",
		Audit: AuditEntry{SourceAddr: "100.64.0.10"},
	})
	var rejection *OperatorRequestError
	if !errors.As(err, &rejection) || rejection.Code != OperatorCodePreconditionFailed {
		t.Fatalf("設定前拿到的預覽在取消之後又被接受了：%v", err)
	}
}

// 取消上限之後那個台數不再有意義，不可以讓它繼續出現在狀態裡。
func TestAClearedLimitCarriesNoMaxMachines(t *testing.T) {
	st := enrollmentLimitTestStore(t, 1)
	setEnrollmentLimit(t, st, "k-set", true, 7)
	cleared := setEnrollmentLimit(t, st, "k-clear", false, 0)
	if cleared.State.MaxMachines != 0 || cleared.State.AtLimit || cleared.State.Headroom != 0 {
		t.Fatalf("取消之後還帶著上限的內容：%+v", cleared.State)
	}
	if !cleared.PreviousSet || cleared.PreviousMax != 7 {
		t.Fatalf("沒有講出改之前是什麼：%+v", cleared)
	}
}

// ⚠ 這一關要用「為那個過大的數字算出來的 digest」去送，否則被擋下來的是 digest
// 不符，上限本身的界線一行都沒有被走到——測試會通過，而那個界線其實不存在。
func TestALimitTooLargeToMeanAnythingIsRefusedByItsOwnBound(t *testing.T) {
	st := enrollmentLimitTestStore(t, 1)
	tooLarge := MaxEnrollmentLimitMachines + 1
	if _, err := st.PreviewOperatorEnrollmentLimit(true, tooLarge); err == nil {
		t.Fatal("預覽收下了一個大到沒有意義的上限")
	}
	current, err := st.EnrollmentLimit()
	if err != nil {
		t.Fatal(err)
	}
	_, err = st.ApplyOperatorEnrollmentLimit(OperatorEnrollmentLimitRequest{
		Set: true, MaxMachines: tooLarge, Reason: "too large",
		ExpectedRevision: current.Revision,
		PreviewDigest:    EnrollmentLimitPreviewDigest(current, true, tooLarge),
		IdempotencyKey:   "k-too-large", RequestDigest: "sha256:k-too-large",
		UpdatedBy: "tailscale-user:1", Audit: AuditEntry{SourceAddr: "100.64.0.10"},
	})
	var rejection *OperatorRequestError
	if !errors.As(err, &rejection) || rejection.Code != OperatorCodeEnrollmentLimitInvalid {
		t.Fatalf("大到沒有意義的上限被接受了：%v", err)
	}
}

// 回放讀的是現在的上限，不是收據裡當時那一份。呼叫端拿這個結果去畫畫面：重播
// 當時的數字，畫面會停在一個已經不成立的上限上。
func TestReplayingALimitChangeReportsTodaysLimitNotTheReceipts(t *testing.T) {
	st := enrollmentLimitTestStore(t, 1)
	preview, err := st.PreviewOperatorEnrollmentLimit(true, 6)
	if err != nil {
		t.Fatal(err)
	}
	req := OperatorEnrollmentLimitRequest{
		Set: true, MaxMachines: 6, Reason: "once",
		ExpectedRevision: preview.Current.Revision, PreviewDigest: preview.PreviewDigest,
		IdempotencyKey: "k-once", RequestDigest: "sha256:k-once", UpdatedBy: "tailscale-user:1",
		Audit: AuditEntry{SourceAddr: "100.64.0.10"},
	}
	if _, err := st.ApplyOperatorEnrollmentLimit(req); err != nil {
		t.Fatal(err)
	}
	setEnrollmentLimit(t, st, "k-moved", true, 20)

	replayed, err := st.ApplyOperatorEnrollmentLimit(req)
	if err != nil || !replayed.Replayed {
		t.Fatalf("replayed=%+v err=%v", replayed, err)
	}
	if replayed.State.MaxMachines != 20 || replayed.State.Revision != 2 {
		t.Fatalf("回放重播了收據裡的舊上限：%+v", replayed.State)
	}
}

func TestEnrollmentLimitReplayAndTransportMarkersAreClassifiedFromStoredEvidence(t *testing.T) {
	for _, detail := range []string{
		OperatorIdempotencyReplayPrefix + "cached decision",
		OperatorTransportRejectionPrefix + "invalid request",
	} {
		entry := AuditEntry{Action: AuditEnrollmentLimit, Detail: detail}
		record := AuditReadRecord{Action: string(AuditEnrollmentLimit), Detail: detail}
		if strings.HasPrefix(detail, OperatorIdempotencyReplayPrefix) {
			if !entry.IsOperatorReplay() || !record.IsOperatorReplay() {
				t.Fatalf("enrollment-limit replay marker was not classified: %q", detail)
			}
		} else if !entry.IsOperatorTransportRejection() || !record.IsOperatorTransportRejection() {
			t.Fatalf("enrollment-limit transport rejection was not classified: %q", detail)
		}
	}

	st := enrollmentLimitTestStore(t, 1)
	preview, err := st.PreviewOperatorEnrollmentLimit(true, 6)
	if err != nil {
		t.Fatal(err)
	}
	req := OperatorEnrollmentLimitRequest{
		Set: true, MaxMachines: 6, Reason: "classify replay",
		ExpectedRevision: preview.Current.Revision, PreviewDigest: preview.PreviewDigest,
		IdempotencyKey: "k-classified-replay", RequestDigest: "sha256:k-classified-replay",
		UpdatedBy: "tailscale-user:1", Audit: AuditEntry{SourceAddr: "100.64.0.10"},
	}
	if _, err := st.ApplyOperatorEnrollmentLimit(req); err != nil {
		t.Fatal(err)
	}
	if replayed, err := st.ApplyOperatorEnrollmentLimit(req); err != nil || !replayed.Replayed {
		t.Fatalf("replayed=%+v err=%v", replayed, err)
	}
	page, err := st.ListAuditReads(AuditReadFilter{Actions: []AuditAction{AuditEnrollmentLimit}, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, record := range page.Items {
		if strings.HasPrefix(record.Detail, OperatorIdempotencyReplayPrefix) {
			found = true
			if !record.IsOperatorReplay() {
				t.Fatalf("stored enrollment-limit replay was not classified: %+v", record)
			}
		}
	}
	if !found {
		t.Fatal("real enrollment-limit replay did not write a replay marker to the audit ledger")
	}
}

// 同一把 key 配不同的 body 是呼叫端自己搞錯了，不可以當成回放放行。
func TestTheSameKeyWithADifferentLimitIsAConflictNotAReplay(t *testing.T) {
	st := enrollmentLimitTestStore(t, 1)
	preview, err := st.PreviewOperatorEnrollmentLimit(true, 6)
	if err != nil {
		t.Fatal(err)
	}
	base := OperatorEnrollmentLimitRequest{
		Set: true, MaxMachines: 6, Reason: "once",
		ExpectedRevision: preview.Current.Revision, PreviewDigest: preview.PreviewDigest,
		IdempotencyKey: "k-once", RequestDigest: "sha256:k-once", UpdatedBy: "tailscale-user:1",
		Audit: AuditEntry{SourceAddr: "100.64.0.10"},
	}
	if _, err := st.ApplyOperatorEnrollmentLimit(base); err != nil {
		t.Fatal(err)
	}
	different := base
	different.MaxMachines, different.RequestDigest = 9, "sha256:k-different-body"
	_, err = st.ApplyOperatorEnrollmentLimit(different)
	var rejection *OperatorRequestError
	if !errors.As(err, &rejection) || rejection.Code != OperatorCodeIdempotencyConflict {
		t.Fatalf("同一把 key 配不同的上限被當成回放：%v", err)
	}
	state, err := st.EnrollmentLimit()
	if err != nil {
		t.Fatal(err)
	}
	if state.MaxMachines != 6 {
		t.Fatalf("第二份 body 寫進去了：%+v", state)
	}
}

// 「沒有上限」這件事在讀出來的那一刻就要成立，不是靠寫入時剛好把台數清成 0。
// 一份從舊快照還原回來的、或是被手動改過的列，照樣不可以讓畫面長出一個上限。
func TestAStoredLimitThatIsOffCarriesNoNumberWhenRead(t *testing.T) {
	st := enrollmentLimitTestStore(t, 2)
	if _, err := st.DB().Exec(`INSERT INTO enrollment_limit
	 (singleton,limit_set,max_machines,revision,reason,updated_at,updated_by)
	 VALUES (1,0,1,4,'restored','2026-09-01T00:00:00Z','tailscale-user:1')`); err != nil {
		t.Fatal(err)
	}
	state, err := st.EnrollmentLimit()
	if err != nil {
		t.Fatal(err)
	}
	if state.Set || state.MaxMachines != 0 || state.AtLimit || state.Headroom != 0 {
		t.Fatalf("一列關著的上限還是長出了數字：%+v", state)
	}
	if err := issueUnderLimit(t, st, "still-allowed", "k-still"); err != nil {
		t.Fatalf("關著的上限擋住了開票：%v", err)
	}
}

// 開票預覽上的那幾個數字是給人看的，但它們不可以說謊：一份說「還收得下」的預覽
// 會讓人按下去才發現開不了票。
func TestTheTicketPreviewSaysWhetherItWillBeRefused(t *testing.T) {
	st := enrollmentLimitTestStore(t, 3)
	open, err := st.PreviewOperatorEnrollToken("next", 3600)
	if err != nil {
		t.Fatal(err)
	}
	if open.LimitSet || open.AtLimit || open.InDenominator != 3 {
		t.Fatalf("沒有上限的預覽：%+v", open)
	}
	setEnrollmentLimit(t, st, "k-set", true, 4)
	room, err := st.PreviewOperatorEnrollToken("next", 3600)
	if err != nil {
		t.Fatal(err)
	}
	if !room.LimitSet || room.LimitMaxMachines != 4 || room.Headroom != 1 || room.AtLimit {
		t.Fatalf("還收得下的預覽：%+v", room)
	}
	if _, _, err := st.CreateEnrollTokenFor("fills-it", time.Hour); err != nil {
		t.Fatal(err)
	}
	full, err := st.PreviewOperatorEnrollToken("next", 3600)
	if err != nil {
		t.Fatal(err)
	}
	if !full.AtLimit || full.Headroom != 0 {
		t.Fatalf("到上限了，預覽卻說還收得下：%+v", full)
	}
	// ⚠ preview digest 不可以跟著上限變。它一旦被釘進去，把上限「調高」也會作廢
	// 一份本來成立的預覽——那是完全相反的方向。
	if full.PreviewDigest != room.PreviewDigest || room.PreviewDigest != open.PreviewDigest {
		t.Fatal("上限改變了開票的 preview digest")
	}
}

// 兩張同時進來的票不可以各自看到「還有一個位子」然後一起用掉它。
func TestTwoTicketsRacingForTheLastSlotOnlyOneWins(t *testing.T) {
	st := enrollmentLimitTestStore(t, 3)
	setEnrollmentLimit(t, st, "k-set", true, 4)

	results := make(chan error, 2)
	start := make(chan struct{})
	for index, name := range []string{"racer-a", "racer-b"} {
		go func(index int, name string) {
			<-start
			results <- issueUnderLimit(t, st, name, "k-race-"+strconv.Itoa(index))
		}(index, name)
	}
	close(start)
	won, lost := 0, 0
	for range 2 {
		if err := <-results; err == nil {
			won++
		} else {
			var rejection *OperatorRequestError
			if !errors.As(err, &rejection) || rejection.Code != OperatorCodeEnrollmentLimitReached {
				t.Fatalf("輸的那一張不是因為上限被擋：%v", err)
			}
			lost++
		}
	}
	if won != 1 || lost != 1 {
		t.Fatalf("最後一個位子被 %d 張票拿走", won)
	}
	state, err := st.EnrollmentLimit()
	if err != nil {
		t.Fatal(err)
	}
	if state.InDenominator != 4 {
		t.Fatalf("名冊上變成 %d 台，上限是 4", state.InDenominator)
	}
}

// 同一把 key 配同樣的台數、不同的理由，還是兩個不同的請求。收據裡只有台數，
// 所以擋得住它的只有 canonical request digest 那一關。
func TestTheSameKeyWithADifferentReasonIsAConflictNotAReplay(t *testing.T) {
	st := enrollmentLimitTestStore(t, 1)
	preview, err := st.PreviewOperatorEnrollmentLimit(true, 6)
	if err != nil {
		t.Fatal(err)
	}
	base := OperatorEnrollmentLimitRequest{
		Set: true, MaxMachines: 6, Reason: "第一個理由",
		ExpectedRevision: preview.Current.Revision, PreviewDigest: preview.PreviewDigest,
		IdempotencyKey: "k-once", RequestDigest: "sha256:reason-one", UpdatedBy: "tailscale-user:1",
		Audit: AuditEntry{SourceAddr: "100.64.0.10"},
	}
	if _, err := st.ApplyOperatorEnrollmentLimit(base); err != nil {
		t.Fatal(err)
	}
	different := base
	different.Reason, different.RequestDigest = "另一個理由", "sha256:reason-two"
	_, err = st.ApplyOperatorEnrollmentLimit(different)
	var rejection *OperatorRequestError
	if !errors.As(err, &rejection) || rejection.Code != OperatorCodeIdempotencyConflict {
		t.Fatalf("同一把 key 配不同的理由被當成回放：%v", err)
	}
	// ⚠ 要看清楚是哪一種 conflict。收據內容對不上會走另一條路，那一條認不出
	// 「台數一樣、理由不一樣」——兩個都回 IDEMPOTENCY_CONFLICT，只有說明分得出來。
	if !strings.Contains(rejection.Detail, "canonical request body") {
		t.Fatalf("擋下它的不是 request digest 那一關：%s", rejection.Detail)
	}
}

// 進交易之前就被擋下來的請求也要留下稽核。那一段 Store 還沒開始寫，沒有人會替
// 它記——「誰想改上限」不可以有一段看不見的區間。
func TestALimitChangeRejectedBeforeTheTransactionIsStillAudited(t *testing.T) {
	st := enrollmentLimitTestStore(t, 1)
	if _, err := st.ApplyOperatorEnrollmentLimit(OperatorEnrollmentLimitRequest{
		Set: true, MaxMachines: 6, Reason: "no key", PreviewDigest: "sha256:x",
		RequestDigest: "sha256:y", UpdatedBy: "tailscale-user:1",
		Audit: AuditEntry{SourceAddr: "100.64.0.10"},
	}); err == nil {
		t.Fatal("沒有 idempotency key 卻被接受")
	}
	var count int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE action=?`,
		string(AuditEnrollmentLimit)).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("Store 在進交易之前就自己寫了稽核 %d 筆；那一段的稽核歸 operator 層", count)
	}
}

// 預覽 digest 綁的是「你送出的，是不是你看過的那一個改法」。少了台數，一份看過
// 「上限 5 台」的預覽就能拿去送出「上限 9 台」——確認頁上寫的那個數字會變成裝飾。
func TestAPreviewOfOneLimitCannotBeSpentOnAnother(t *testing.T) {
	st := enrollmentLimitTestStore(t, 1)
	preview, err := st.PreviewOperatorEnrollmentLimit(true, 5)
	if err != nil {
		t.Fatal(err)
	}
	_, err = st.ApplyOperatorEnrollmentLimit(OperatorEnrollmentLimitRequest{
		Set: true, MaxMachines: 9, Reason: "swapped",
		ExpectedRevision: preview.Current.Revision, PreviewDigest: preview.PreviewDigest,
		IdempotencyKey: "k-swap", RequestDigest: "sha256:k-swap", UpdatedBy: "tailscale-user:1",
		Audit: AuditEntry{SourceAddr: "100.64.0.10"},
	})
	var rejection *OperatorRequestError
	if !errors.As(err, &rejection) || rejection.Code != OperatorCodeEnrollmentLimitPreviewStale {
		t.Fatalf("看過 5 台的預覽送出了 9 台：%v", err)
	}
	// 取消上限也是一個改法，不可以拿設上限的預覽去送。
	clear, err := st.PreviewOperatorEnrollmentLimit(false, 0)
	if err != nil {
		t.Fatal(err)
	}
	if clear.PreviewDigest == preview.PreviewDigest {
		t.Fatal("取消上限跟設上限 digest 成同一個")
	}
}
