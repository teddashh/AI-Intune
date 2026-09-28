package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/expect"
	"github.com/teddashh/AI-Intune/internal/model"
	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/operatorauth"
	"github.com/teddashh/AI-Intune/internal/settingpolicy"
	"github.com/teddashh/AI-Intune/internal/store"
)

func settingAPIFixture(t *testing.T) jobsFixture {
	t.Helper()
	f := newJobsFixture(t, "setting-api-machine")
	f.store.SetExpectations(&expect.Set{Configured: true})
	if err := f.store.PublishExpectationsPolicy(jobsTestNow); err != nil {
		t.Fatalf("發佈 workload policy 失敗：%v", err)
	}
	(&hub{store: f.store, retention: store.DefaultRetention()}).operatorRoutes(f.mux)
	return f
}

// settingCheckin is the agent side of the loop: it sends the digest the agent
// is running and reads back what the Hub wants it to run.
func settingCheckin(t *testing.T, f jobsFixture, sentAt time.Time, reported string) model.CheckinResponse {
	t.Helper()
	raw, _ := json.Marshal(model.Checkin{
		SchemaVersion: model.SchemaVersion, SentAt: sentAt, AgentVersion: "test",
		BootID: "setting-boot", AgentSeq: sentAt.Unix(), SettingsDigest: reported,
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/checkins", bytes.NewReader(raw))
	req.Header.Set("Authorization", "Bearer "+f.machine.token)
	f.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("checkin=%d：%s", rec.Code, rec.Body.String())
	}
	var resp model.CheckinResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解碼 checkin 回應失敗：%v", err)
	}
	return resp
}

func settingBoard(t *testing.T, f jobsFixture) operator.SettingBoardResult {
	t.Helper()
	rec := operatorRequest(t, f.mux, http.MethodGet, "/v1/operator/settings", "", "")
	if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("board=%d headers=%v body=%s", rec.Code, rec.Header(), rec.Body.String())
	}
	var board operator.SettingBoardResult
	if err := json.Unmarshal(rec.Body.Bytes(), &board); err != nil {
		t.Fatalf("解碼盤面失敗：%v", err)
	}
	return board
}

func settingPost(t *testing.T, f jobsFixture, path, key string, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("編碼 %s 的 body 失敗：%v", path, err)
	}
	return operatorRequest(t, f.mux, http.MethodPost, path, key, string(raw))
}

func decodeSettingJSON(t *testing.T, rec *httptest.ResponseRecorder, want int, out any) {
	t.Helper()
	if rec.Code != want {
		t.Fatalf("status=%d，想要 %d：%s", rec.Code, want, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
		t.Fatalf("解碼回應失敗：%v：%s", err, rec.Body.String())
	}
}

// TestOperatorSettingPolicyReachesAgentAndBoard walks the whole slice once:
// preview、發佈、指派、機器報到拿到新節奏、回報 digest，盤面才說「已套用」。
func TestOperatorSettingPolicyReachesAgentAndBoard(t *testing.T) {
	f := settingAPIFixture(t)

	board := settingBoard(t, f)
	if len(board.Machines) != 1 || board.Machines[0].MachineID != f.machine.id ||
		board.Machines[0].Verdict != settingpolicy.VerdictNeverReported ||
		board.Machines[0].Source != settingpolicy.SourceDefault ||
		board.Machines[0].Settings != settingpolicy.Defaults() ||
		board.Defaults != settingpolicy.Defaults() ||
		board.Counts[settingpolicy.VerdictNeverReported] != 1 ||
		len(board.Policies) != 0 || len(board.Assignments) != 0 {
		t.Fatalf("尚未指派時的盤面不對：%+v", board)
	}

	var preview operator.SettingPolicyPreviewResult
	decodeSettingJSON(t, settingPost(t, f, "/v1/operator/setting-policies/preview", "",
		settingPolicyPreviewOperatorRequest{PolicyID: "fast-fleet",
			CheckinIntervalSeconds: 60, ObservationIntervalSeconds: 300}),
		http.StatusOK, &preview)
	if preview.CurrentRev != 0 || preview.NextRev != 1 || preview.Unchanged ||
		preview.PreviewDigest == "" || preview.Digest == "" || preview.AffectedMachines != 0 {
		t.Fatalf("第一次 preview=%+v", preview)
	}

	zero := int64(0)
	publish := settingPolicyPublishOperatorRequest{PolicyID: "fast-fleet",
		CheckinIntervalSeconds: 60, ObservationIntervalSeconds: 300,
		ExpectedRevision: &zero, PreviewDigest: preview.PreviewDigest,
		ConfirmPolicyID: "fast-fleet", Reason: "機隊要更早發現失聯"}

	var published store.OperatorSettingPolicyResult
	fresh := settingPost(t, f, "/v1/operator/setting-policies", "setting-publish-key", publish)
	if fresh.Header().Get("Idempotency-Replayed") != "" {
		t.Fatalf("第一次發佈不該標成 replay：%v", fresh.Header())
	}
	decodeSettingJSON(t, fresh, http.StatusCreated, &published)
	if published.Revision != 1 || published.Digest != preview.Digest ||
		published.Unchanged || published.Replayed {
		t.Fatalf("發佈結果=%+v", published)
	}

	replay := settingPost(t, f, "/v1/operator/setting-policies", "setting-publish-key", publish)
	if replay.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatalf("重送同一把 key 沒有標成 replay：%v", replay.Header())
	}
	var replayed store.OperatorSettingPolicyResult
	decodeSettingJSON(t, replay, http.StatusOK, &replayed)
	if !replayed.Replayed || replayed.Revision != 1 {
		t.Fatalf("replay 結果=%+v", replayed)
	}

	// 同一組值再預覽一次：沒有新的決定，就沒有新的 revision。
	var again operator.SettingPolicyPreviewResult
	decodeSettingJSON(t, settingPost(t, f, "/v1/operator/setting-policies/preview", "",
		settingPolicyPreviewOperatorRequest{PolicyID: "fast-fleet",
			CheckinIntervalSeconds: 60, ObservationIntervalSeconds: 300}),
		http.StatusOK, &again)
	if !again.Unchanged || again.CurrentRev != 1 || again.NextRev != 1 {
		t.Fatalf("重複值的 preview=%+v", again)
	}

	var assignPreview operator.SettingAssignmentPreviewResult
	decodeSettingJSON(t, settingPost(t, f, "/v1/operator/setting-assignments/preview", "",
		settingAssignmentPreviewOperatorRequest{Scope: "machine", ScopeID: f.machine.id,
			PolicyID: "fast-fleet", Revision: 1}),
		http.StatusOK, &assignPreview)
	if assignPreview.AffectedMachines != 1 || assignPreview.Unchanged ||
		assignPreview.CurrentPolicyID != "" || assignPreview.PreviewDigest == "" ||
		assignPreview.Digest != published.Digest ||
		assignPreview.Settings.CheckinIntervalSeconds != 60 {
		t.Fatalf("指派 preview=%+v", assignPreview)
	}

	assign := settingAssignmentOperatorRequest{Scope: "machine", ScopeID: f.machine.id,
		PolicyID: "fast-fleet", Revision: 1, PreviewDigest: assignPreview.PreviewDigest,
		ConfirmScopeID: f.machine.id, Reason: "先在這一台上線"}
	var assigned store.OperatorSettingAssignmentResult
	decodeSettingJSON(t, settingPost(t, f, "/v1/operator/setting-assignments",
		"setting-assign-key", assign), http.StatusCreated, &assigned)
	if assigned.Revision != 1 || assigned.PolicyRev != 1 || assigned.Unchanged ||
		assigned.Digest != published.Digest || assigned.AssignmentID == "" {
		t.Fatalf("指派結果=%+v", assigned)
	}

	assignReplay := settingPost(t, f, "/v1/operator/setting-assignments", "setting-assign-key", assign)
	if assignReplay.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatalf("重送指派沒有標成 replay：%v", assignReplay.Header())
	}
	var assignReplayed store.OperatorSettingAssignmentResult
	decodeSettingJSON(t, assignReplay, http.StatusOK, &assignReplayed)
	if !assignReplayed.Replayed || assignReplayed.AssignmentID != assigned.AssignmentID {
		t.Fatalf("指派 replay=%+v", assignReplayed)
	}

	// 機器報到：拿到新節奏，但還沒回報跑的是什麼。
	base := jobsTestNow
	first := settingCheckin(t, f, base, "")
	if first.CheckinIntervalSeconds != 60 || first.ObservationIntervalSeconds != 300 ||
		first.SettingsDigest != published.Digest {
		t.Fatalf("報到沒有拿到指派的設定：%+v", first)
	}
	board = settingBoard(t, f)
	if board.Machines[0].Verdict != settingpolicy.VerdictUnknown ||
		board.Machines[0].Source != settingpolicy.SourceMachine ||
		board.Machines[0].PolicyID != "fast-fleet" || board.Machines[0].Revision != 1 ||
		board.Machines[0].VerdictLabel != settingpolicy.Label(settingpolicy.VerdictUnknown) {
		t.Fatalf("只報到、沒回報 digest 的盤面=%+v", board.Machines[0])
	}

	// 回報跑的就是 Hub 指派的那一份，這才叫套用了。
	second := settingCheckin(t, f, base.Add(time.Minute), first.SettingsDigest)
	if second.SettingsDigest != published.Digest {
		t.Fatalf("第二次報到=%+v", second)
	}
	board = settingBoard(t, f)
	if board.Machines[0].Verdict != settingpolicy.VerdictApplied ||
		board.Machines[0].ReportedAt == nil ||
		board.Counts[settingpolicy.VerdictApplied] != 1 ||
		len(board.Policies) != 1 || board.Policies[0].PolicyID != "fast-fleet" ||
		len(board.Assignments) != 1 || board.Assignments[0].ScopeID != f.machine.id {
		t.Fatalf("回報 digest 之後的盤面=%+v", board)
	}

	// 再發一版：機器還在跑舊的，是「落後」不是「不對」。
	one := int64(1)
	var nextPreview operator.SettingPolicyPreviewResult
	decodeSettingJSON(t, settingPost(t, f, "/v1/operator/setting-policies/preview", "",
		settingPolicyPreviewOperatorRequest{PolicyID: "fast-fleet",
			CheckinIntervalSeconds: 90, ObservationIntervalSeconds: 300}),
		http.StatusOK, &nextPreview)
	if nextPreview.NextRev != 2 || nextPreview.AffectedMachines != 1 {
		t.Fatalf("第二版 preview=%+v", nextPreview)
	}
	var republished store.OperatorSettingPolicyResult
	decodeSettingJSON(t, settingPost(t, f, "/v1/operator/setting-policies", "setting-publish-key-2",
		settingPolicyPublishOperatorRequest{PolicyID: "fast-fleet",
			CheckinIntervalSeconds: 90, ObservationIntervalSeconds: 300,
			ExpectedRevision: &one, PreviewDigest: nextPreview.PreviewDigest,
			ConfirmPolicyID: "fast-fleet", Reason: "60 秒太吵"}),
		http.StatusCreated, &republished)
	if republished.Revision != 2 {
		t.Fatalf("第二版發佈=%+v", republished)
	}

	var rev2Preview operator.SettingAssignmentPreviewResult
	decodeSettingJSON(t, settingPost(t, f, "/v1/operator/setting-assignments/preview", "",
		settingAssignmentPreviewOperatorRequest{Scope: "machine", ScopeID: f.machine.id,
			PolicyID: "fast-fleet", Revision: 2}),
		http.StatusOK, &rev2Preview)
	if rev2Preview.CurrentPolicyID != "fast-fleet" || rev2Preview.CurrentRevision != 1 ||
		rev2Preview.Unchanged {
		t.Fatalf("換版 preview=%+v", rev2Preview)
	}
	var reassigned store.OperatorSettingAssignmentResult
	decodeSettingJSON(t, settingPost(t, f, "/v1/operator/setting-assignments", "setting-assign-key-2",
		settingAssignmentOperatorRequest{Scope: "machine", ScopeID: f.machine.id,
			PolicyID: "fast-fleet", Revision: 2, PreviewDigest: rev2Preview.PreviewDigest,
			ConfirmScopeID: f.machine.id, Reason: "換到第二版"}),
		http.StatusCreated, &reassigned)
	if reassigned.Revision != 2 || reassigned.PolicyRev != 2 {
		t.Fatalf("重新指派=%+v", reassigned)
	}

	// 機器還沒再報到，仍在跑 revision 1：pending，不是 mismatch。
	board = settingBoard(t, f)
	if board.Machines[0].Verdict != settingpolicy.VerdictPending ||
		board.Machines[0].Settings.CheckinIntervalSeconds != 90 {
		t.Fatalf("換版後、機器尚未報到的盤面=%+v", board.Machines[0])
	}

	// 回報一個 Hub 從來沒發過的 digest：那是這個 Hub 沒送過的設定。
	settingCheckin(t, f, base.Add(2*time.Minute), "sha256:"+
		"0000000000000000000000000000000000000000000000000000000000000000")
	board = settingBoard(t, f)
	if board.Machines[0].Verdict != settingpolicy.VerdictMismatch {
		t.Fatalf("回報未知 digest 的盤面=%+v", board.Machines[0])
	}

	// 六列：兩次發佈、兩次指派，加上各自被重送的那一次 —— 重送也是一個人按下去
	// 的動作，稽核頁要看得到誰在什麼時候又按了一次。
	page, err := f.store.ListAuditReads(store.AuditReadFilter{
		Actions: []store.AuditAction{store.AuditSettingPolicy, store.AuditSettingAssign}, Limit: 20,
	})
	if err != nil {
		t.Fatalf("讀稽核失敗：%v", err)
	}
	byAction := map[string]int{}
	for _, item := range page.Items {
		if item.AuthSubject != "tailscale-user:42" || item.Reason == "" {
			t.Fatalf("稽核列=%+v", item)
		}
		byAction[item.Action]++
	}
	if len(page.Items) != 6 || byAction[string(store.AuditSettingPolicy)] != 3 ||
		byAction[string(store.AuditSettingAssign)] != 3 {
		t.Fatalf("稽核共 %d 筆，分佈=%v", len(page.Items), byAction)
	}
}

// TestOperatorSettingRejectionsMapToStatus pins the four rejections an operator
// can actually hit, because a 500 here would read as "Hub 壞了" instead of
// "你的 request 已經過期了".
func TestOperatorSettingRejectionsMapToStatus(t *testing.T) {
	f := settingAPIFixture(t)

	// observation 不可小於 checkin：request 本身就講不通。
	bad := settingPost(t, f, "/v1/operator/setting-policies/preview", "",
		settingPolicyPreviewOperatorRequest{PolicyID: "bad-order",
			CheckinIntervalSeconds: 600, ObservationIntervalSeconds: 120})
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("順序錯的設定=%d：%s", bad.Code, bad.Body.String())
	}

	// 指派一個還沒發佈的原則：404。
	missing := settingPost(t, f, "/v1/operator/setting-assignments/preview", "",
		settingAssignmentPreviewOperatorRequest{Scope: "machine", ScopeID: f.machine.id,
			PolicyID: "never-published", Revision: 1})
	if missing.Code != http.StatusNotFound {
		t.Fatalf("未發佈的原則=%d：%s", missing.Code, missing.Body.String())
	}

	var preview operator.SettingPolicyPreviewResult
	decodeSettingJSON(t, settingPost(t, f, "/v1/operator/setting-policies/preview", "",
		settingPolicyPreviewOperatorRequest{PolicyID: "slow-fleet",
			CheckinIntervalSeconds: 300, ObservationIntervalSeconds: 900}),
		http.StatusOK, &preview)

	// 預覽過期：值在按下確認之前變了。
	zero := int64(0)
	stale := settingPost(t, f, "/v1/operator/setting-policies", "setting-stale-key",
		settingPolicyPublishOperatorRequest{PolicyID: "slow-fleet",
			CheckinIntervalSeconds: 301, ObservationIntervalSeconds: 900,
			ExpectedRevision: &zero, PreviewDigest: preview.PreviewDigest,
			ConfirmPolicyID: "slow-fleet", Reason: "改了一秒"})
	if stale.Code != http.StatusPreconditionFailed {
		t.Fatalf("過期預覽=%d：%s", stale.Code, stale.Body.String())
	}

	// 撞到別人剛發佈的 revision：409，不靜默覆蓋。
	nine := int64(9)
	conflict := settingPost(t, f, "/v1/operator/setting-policies", "setting-conflict-key",
		settingPolicyPublishOperatorRequest{PolicyID: "slow-fleet",
			CheckinIntervalSeconds: 300, ObservationIntervalSeconds: 900,
			ExpectedRevision: &nine, PreviewDigest: preview.PreviewDigest,
			ConfirmPolicyID: "slow-fleet", Reason: "讀到的是舊的"})
	if conflict.Code != http.StatusConflict {
		t.Fatalf("revision 衝突=%d：%s", conflict.Code, conflict.Body.String())
	}

	// 被快取的拒絕重送一次，仍然是同一個判決，而且標成 replay。
	replay := settingPost(t, f, "/v1/operator/setting-policies", "setting-conflict-key",
		settingPolicyPublishOperatorRequest{PolicyID: "slow-fleet",
			CheckinIntervalSeconds: 300, ObservationIntervalSeconds: 900,
			ExpectedRevision: &nine, PreviewDigest: preview.PreviewDigest,
			ConfirmPolicyID: "slow-fleet", Reason: "讀到的是舊的"})
	if replay.Code != http.StatusConflict || replay.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatalf("重送被拒的 key=%d headers=%v：%s", replay.Code, replay.Header(), replay.Body.String())
	}

	// scope 只有 machine 與 channel 兩種。
	rogue := settingPost(t, f, "/v1/operator/setting-assignments", "setting-scope-key",
		settingAssignmentOperatorRequest{Scope: "tenant", ScopeID: "everything",
			PolicyID: "slow-fleet", Revision: 1,
			PreviewDigest:  store.SettingAssignmentPreviewDigest("tenant", "everything", "slow-fleet", 1),
			ConfirmScopeID: "everything", Reason: "試試看"})
	if rogue.Code != http.StatusBadRequest {
		t.Fatalf("未知 scope=%d：%s", rogue.Code, rogue.Body.String())
	}
}

func TestAPolicyOrAssignmentRefusedAtTheTransportLayerIsStillAttributedInTheLedger(t *testing.T) {
	const (
		validBody                 = `{}`
		keyPrefix                 = "policy-transport-"
		wantAuthSubject           = "tailscale-user:42"
		rowCountConsequence       = "0 列代表已認證的提交完全沒留痕跡，稽核者會把稍後的成功列當成第一次接觸；多於 1 列則代表同一次請求被算成多次提交。"
		actionConsequence         = "action 記錯時，稽核者用四個 canonical action 查不到這次提交，會把下一次成功列當成首次提交。"
		subjectConsequence        = "policy_id 與 scope_id 只在被擋的 body 裡；虛構 subject 會讓 operator 拿它對政策清單卻對不到。"
		outcomeConsequence        = "被擋的提交記成成功會被算進成功率，也會讓稽核者誤以為權威狀態當時已被改動。"
		classificationConsequence = "缺少 transport-rejection 分類時，稽核者分不出請求尚未進 store，還是進了 store 才被 domain 擋下。"
		keyConsequence            = "idempotency key 對不上時，這次被擋的提交無法接回稍後以同一把 key 成功的提交。"
		actorConsequence          = "審計列沒有 boundary 認出的操作者時，稽核者無法說明是誰送出這次已認證的提交。"
		idempotencyConsequence    = "transport 拒絕若佔用 key，operator 修好傳輸後用同一把 key 重送會被當成重放，永遠無法改動權威狀態。"
	)
	tests := []struct {
		name        string
		path        string
		action      store.AuditAction
		key         string
		contentType string
		body        string
		wantStatus  int
		wantCode    string
	}{
		{
			name: "compliance policy content type", path: "/v1/operator/compliance-policies",
			action: store.AuditCompliancePolicy, key: "compliance-policy-type",
			contentType: "text/plain", body: validBody,
			wantStatus: http.StatusUnsupportedMediaType, wantCode: "UNSUPPORTED_MEDIA_TYPE",
		},
		{
			name: "compliance policy decode", path: "/v1/operator/compliance-policies",
			action: store.AuditCompliancePolicy, key: "compliance-policy-decode",
			contentType: "application/json", body: `{`,
			wantStatus: http.StatusBadRequest, wantCode: "BAD_REQUEST",
		},
		{
			name: "compliance assignment content type", path: "/v1/operator/compliance-assignments",
			action: store.AuditComplianceAssign, key: "compliance-assignment-type",
			contentType: "text/plain", body: validBody,
			wantStatus: http.StatusUnsupportedMediaType, wantCode: "UNSUPPORTED_MEDIA_TYPE",
		},
		{
			name: "compliance assignment decode", path: "/v1/operator/compliance-assignments",
			action: store.AuditComplianceAssign, key: "compliance-assignment-decode",
			contentType: "application/json", body: `{`,
			wantStatus: http.StatusBadRequest, wantCode: "BAD_REQUEST",
		},
		{
			name: "setting policy content type", path: "/v1/operator/setting-policies",
			action: store.AuditSettingPolicy, key: "setting-policy-type",
			contentType: "text/plain", body: validBody,
			wantStatus: http.StatusUnsupportedMediaType, wantCode: "UNSUPPORTED_MEDIA_TYPE",
		},
		{
			name: "setting policy decode", path: "/v1/operator/setting-policies",
			action: store.AuditSettingPolicy, key: "setting-policy-decode",
			contentType: "application/json", body: `{`,
			wantStatus: http.StatusBadRequest, wantCode: "BAD_REQUEST",
		},
		{
			name: "setting assignment content type", path: "/v1/operator/setting-assignments",
			action: store.AuditSettingAssign, key: "setting-assignment-type",
			contentType: "text/plain", body: validBody,
			wantStatus: http.StatusUnsupportedMediaType, wantCode: "UNSUPPORTED_MEDIA_TYPE",
		},
		{
			name: "setting assignment decode", path: "/v1/operator/setting-assignments",
			action: store.AuditSettingAssign, key: "setting-assignment-decode",
			contentType: "application/json", body: `{`,
			wantStatus: http.StatusBadRequest, wantCode: "BAD_REQUEST",
		},
		// Policy documents grow with their rules and settings, making them the
		// only family likely to reach 64 KiB in normal operator work. Assignments
		// carry only a few IDs, so they do not need an oversized-body case.
		{
			name: "compliance policy oversized", path: "/v1/operator/compliance-policies",
			action: store.AuditCompliancePolicy, key: "compliance-policy-oversized",
			contentType: "application/json",
			body:        strings.Repeat(" ", (64<<10)+1),
			wantStatus:  http.StatusRequestEntityTooLarge, wantCode: "PAYLOAD_TOO_LARGE",
		},
		{
			name: "setting policy oversized", path: "/v1/operator/setting-policies",
			action: store.AuditSettingPolicy, key: "setting-policy-oversized",
			contentType: "application/json",
			body:        strings.Repeat(" ", (64<<10)+1),
			wantStatus:  http.StatusRequestEntityTooLarge, wantCode: "PAYLOAD_TOO_LARGE",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := observedOperatorFixture(t)
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
			req.Header.Set("Content-Type", tc.contentType)
			wantKey := keyPrefix + tc.key
			req.Header.Set("Idempotency-Key", wantKey)
			req = verifiedOperatorRequest(req, operatorauth.Admin)
			f.mux.ServeHTTP(rec, req)
			assertAPIError(t, rec, tc.wantStatus, tc.wantCode)

			entries, err := f.store.Audit("", 10)
			if err != nil {
				t.Fatal(err)
			}
			if got := len(entries); got != 1 {
				t.Errorf("got %d audit rows, expected 1；%s", got, rowCountConsequence)
				return
			}
			entry := entries[0]
			if entry.Action != tc.action {
				t.Errorf("got action %q, expected %q；%s", entry.Action, tc.action, actionConsequence)
			}
			if entry.Subject != "" {
				t.Errorf("got subject %q, expected an empty subject；%s", entry.Subject, subjectConsequence)
			}
			if entry.OK {
				t.Errorf("got a successful audit outcome, expected a failed outcome；%s", outcomeConsequence)
			}
			if !entry.IsOperatorTransportRejection() {
				t.Errorf("got unclassified audit detail %q, expected a transport rejection；%s", entry.Detail, classificationConsequence)
			}
			if entry.IdempotencyKey != wantKey {
				t.Errorf("got idempotency key %q, expected %q；%s", entry.IdempotencyKey, wantKey, keyConsequence)
			}
			if entry.AuthSubject != wantAuthSubject {
				t.Errorf("got auth subject %q, expected %q；%s", entry.AuthSubject, wantAuthSubject, actorConsequence)
			}

			var receipts int
			if err := f.store.DB().QueryRow("SELECT COUNT(*) FROM operator_idempotency WHERE idempotency_key=?", wantKey).Scan(&receipts); err != nil {
				t.Fatal(err)
			}
			if receipts != 0 {
				t.Errorf("got %d idempotency rows, expected 0；%s", receipts, idempotencyConsequence)
			}
		})
	}
}
