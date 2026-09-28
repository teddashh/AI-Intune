package main

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/compliance"
	"github.com/teddashh/AI-Intune/internal/model"
	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/store"
)

func complianceBoard(t *testing.T, f jobsFixture) operator.ComplianceBoardResult {
	t.Helper()
	rec := operatorRequest(t, f.mux, http.MethodGet, "/v1/operator/compliance", "", "")
	if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("board=%d headers=%v body=%s", rec.Code, rec.Header(), rec.Body.String())
	}
	var board operator.ComplianceBoardResult
	if err := json.Unmarshal(rec.Body.Bytes(), &board); err != nil {
		t.Fatalf("解碼盤面失敗：%v", err)
	}
	return board
}

func complianceRuleFor(t *testing.T, board operator.ComplianceBoardMachine,
	kind compliance.RuleKind) operator.ComplianceRuleResult {
	t.Helper()
	for _, r := range board.Results {
		if r.Kind == kind {
			return r
		}
	}
	t.Fatalf("判決裡沒有 %s 這條規則：%+v", kind, board.Results)
	return operator.ComplianceRuleResult{}
}

// TestOperatorCompliancePolicyJudgesTheFleet walks the whole slice once:
// preview、發佈、指派、機器報到，判決從「未指派」一路走到「符合」。
func TestOperatorCompliancePolicyJudgesTheFleet(t *testing.T) {
	f := settingAPIFixture(t)

	board := complianceBoard(t, f)
	if len(board.Machines) != 1 || board.Machines[0].MachineID != f.machine.id ||
		board.Machines[0].Verdict != compliance.VerdictNotEvaluated ||
		board.Machines[0].Source != compliance.SourceNone ||
		board.Counts[compliance.VerdictNotEvaluated] != 1 ||
		len(board.Policies) != 0 || len(board.Assignments) != 0 ||
		board.EvaluatedAt.IsZero() {
		t.Fatalf("尚未指派時的盤面不對：%+v", board)
	}

	rules := []complianceRuleWire{
		{Kind: "checkin_max_age", MaxAgeSeconds: 900},
		{Kind: "settings_applied"},
	}
	var preview operator.CompliancePolicyPreviewResult
	decodeSettingJSON(t, settingPost(t, f, "/v1/operator/compliance-policies/preview", "",
		compliancePolicyPreviewOperatorRequest{PolicyID: "fleet-floor", Rules: rules}),
		http.StatusOK, &preview)
	if preview.CurrentRev != 0 || preview.NextRev != 1 || preview.Unchanged ||
		preview.PreviewDigest == "" || preview.Digest == "" || preview.AffectedMachines != 0 ||
		len(preview.Rules) != 2 || preview.Rules[0].Bound != "最久 900 秒" {
		t.Fatalf("第一次 preview=%+v", preview)
	}

	zero := int64(0)
	publish := compliancePolicyPublishOperatorRequest{PolicyID: "fleet-floor", Rules: rules,
		ExpectedRevision: &zero, PreviewDigest: preview.PreviewDigest,
		ConfirmPolicyID: "fleet-floor", Reason: "機隊要有一條底線"}

	var published store.OperatorCompliancePolicyResult
	fresh := settingPost(t, f, "/v1/operator/compliance-policies", "compliance-publish-key", publish)
	if fresh.Header().Get("Idempotency-Replayed") != "" {
		t.Fatalf("第一次發佈不該標成 replay：%v", fresh.Header())
	}
	decodeSettingJSON(t, fresh, http.StatusCreated, &published)
	if published.Revision != 1 || published.Digest != preview.Digest ||
		published.Unchanged || published.Replayed {
		t.Fatalf("發佈結果=%+v", published)
	}

	replay := settingPost(t, f, "/v1/operator/compliance-policies", "compliance-publish-key", publish)
	if replay.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatalf("重送同一把 key 沒有標成 replay：%v", replay.Header())
	}
	var replayed store.OperatorCompliancePolicyResult
	decodeSettingJSON(t, replay, http.StatusOK, &replayed)
	if !replayed.Replayed || replayed.Revision != 1 {
		t.Fatalf("replay 結果=%+v", replayed)
	}

	// 同一組規則換個順序寫還是同一份規則，就沒有新的決定。
	var again operator.CompliancePolicyPreviewResult
	decodeSettingJSON(t, settingPost(t, f, "/v1/operator/compliance-policies/preview", "",
		compliancePolicyPreviewOperatorRequest{PolicyID: "fleet-floor", Rules: []complianceRuleWire{
			{Kind: "settings_applied"}, {Kind: "checkin_max_age", MaxAgeSeconds: 900},
		}}), http.StatusOK, &again)
	if !again.Unchanged || again.CurrentRev != 1 || again.NextRev != 1 {
		t.Fatalf("換順序的 preview=%+v", again)
	}

	var assignPreview operator.ComplianceAssignmentPreviewResult
	decodeSettingJSON(t, settingPost(t, f, "/v1/operator/compliance-assignments/preview", "",
		complianceAssignmentPreviewOperatorRequest{Scope: "machine", ScopeID: f.machine.id,
			PolicyID: "fleet-floor", Revision: 1}),
		http.StatusOK, &assignPreview)
	if assignPreview.AffectedMachines != 1 || assignPreview.Unchanged ||
		assignPreview.CurrentPolicyID != "" || assignPreview.PreviewDigest == "" ||
		assignPreview.Digest != published.Digest || len(assignPreview.Rules) != 2 {
		t.Fatalf("指派 preview=%+v", assignPreview)
	}

	assign := complianceAssignmentOperatorRequest{Scope: "machine", ScopeID: f.machine.id,
		PolicyID: "fleet-floor", Revision: 1, PreviewDigest: assignPreview.PreviewDigest,
		ConfirmScopeID: f.machine.id, Reason: "先在這一台上線"}
	var assigned store.OperatorComplianceAssignmentResult
	decodeSettingJSON(t, settingPost(t, f, "/v1/operator/compliance-assignments",
		"compliance-assign-key", assign), http.StatusCreated, &assigned)
	if assigned.Revision != 1 || assigned.PolicyRev != 1 || assigned.Unchanged ||
		assigned.Digest != published.Digest || assigned.AssignmentID == "" {
		t.Fatalf("指派結果=%+v", assigned)
	}

	// 指派完、機器還沒報到：從未報到，不是不符合。
	board = complianceBoard(t, f)
	if board.Machines[0].Verdict != compliance.VerdictNeverReported ||
		board.Machines[0].Source != compliance.SourceMachine ||
		board.Machines[0].PolicyID != "fleet-floor" ||
		board.Machines[0].VerdictLabel != compliance.Label(compliance.VerdictNeverReported) {
		t.Fatalf("指派後、尚未報到的盤面=%+v", board.Machines[0])
	}

	// 報到但還沒回報設定 digest：那一條量不到，整台就量不到。
	base := time.Now().UTC()
	first := settingCheckin(t, f, base, "")
	board = complianceBoard(t, f)
	if board.Machines[0].Verdict != compliance.VerdictUnmeasured {
		t.Fatalf("只報到、沒回報 digest 的盤面=%+v", board.Machines[0])
	}
	if got := complianceRuleFor(t, board.Machines[0], compliance.RuleCheckinMaxAge); got.Outcome != compliance.OutcomePass {
		t.Fatalf("剛報到的新鮮度判決=%+v", got)
	}
	if got := complianceRuleFor(t, board.Machines[0], compliance.RuleSettingsApplied); got.Outcome != compliance.OutcomeUnmeasured {
		t.Fatalf("沒回報 digest 的設定判決=%+v", got)
	}

	// 回報跑的就是 Hub 指派的那一份：兩條都量到而且通過。
	settingCheckin(t, f, base.Add(time.Second), first.SettingsDigest)
	board = complianceBoard(t, f)
	if board.Machines[0].Verdict != compliance.VerdictCompliant ||
		board.Counts[compliance.VerdictCompliant] != 1 ||
		board.Machines[0].ReportedAt == nil ||
		len(board.Policies) != 1 || board.Policies[0].PolicyID != "fleet-floor" ||
		len(board.Assignments) != 1 || board.Assignments[0].ScopeID != f.machine.id {
		t.Fatalf("回報 digest 之後的盤面=%+v", board)
	}

	// 收緊到 60 秒，同一批證據就變成不符合 —— 判決是規則的函數，不是狀態機。
	one := int64(1)
	tight := []complianceRuleWire{
		{Kind: "checkin_max_age", MaxAgeSeconds: 60}, {Kind: "settings_applied"},
	}
	var tightPreview operator.CompliancePolicyPreviewResult
	decodeSettingJSON(t, settingPost(t, f, "/v1/operator/compliance-policies/preview", "",
		compliancePolicyPreviewOperatorRequest{PolicyID: "fleet-floor", Rules: tight}),
		http.StatusOK, &tightPreview)
	if tightPreview.NextRev != 2 || tightPreview.AffectedMachines != 1 {
		t.Fatalf("收緊後的 preview=%+v", tightPreview)
	}
	var republished store.OperatorCompliancePolicyResult
	decodeSettingJSON(t, settingPost(t, f, "/v1/operator/compliance-policies", "compliance-publish-key-2",
		compliancePolicyPublishOperatorRequest{PolicyID: "fleet-floor", Rules: tight,
			ExpectedRevision: &one, PreviewDigest: tightPreview.PreviewDigest,
			ConfirmPolicyID: "fleet-floor", Reason: "15 分鐘太鬆"}),
		http.StatusCreated, &republished)
	if republished.Revision != 2 {
		t.Fatalf("第二版發佈=%+v", republished)
	}

	var rev2Preview operator.ComplianceAssignmentPreviewResult
	decodeSettingJSON(t, settingPost(t, f, "/v1/operator/compliance-assignments/preview", "",
		complianceAssignmentPreviewOperatorRequest{Scope: "machine", ScopeID: f.machine.id,
			PolicyID: "fleet-floor", Revision: 2}),
		http.StatusOK, &rev2Preview)
	if rev2Preview.CurrentPolicyID != "fleet-floor" || rev2Preview.CurrentRevision != 1 ||
		rev2Preview.Unchanged {
		t.Fatalf("換版 preview=%+v", rev2Preview)
	}
	var reassigned store.OperatorComplianceAssignmentResult
	decodeSettingJSON(t, settingPost(t, f, "/v1/operator/compliance-assignments", "compliance-assign-key-2",
		complianceAssignmentOperatorRequest{Scope: "machine", ScopeID: f.machine.id,
			PolicyID: "fleet-floor", Revision: 2, PreviewDigest: rev2Preview.PreviewDigest,
			ConfirmScopeID: f.machine.id, Reason: "換到第二版"}),
		http.StatusCreated, &reassigned)
	if reassigned.PolicyRev != 2 {
		t.Fatalf("重新指派=%+v", reassigned)
	}

	// 六列：兩次發佈、兩次指派，加上各自被重送的那一次。
	page, err := f.store.ListAuditReads(store.AuditReadFilter{
		Actions: []store.AuditAction{store.AuditCompliancePolicy, store.AuditComplianceAssign}, Limit: 20,
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
	if len(page.Items) != 5 || byAction[string(store.AuditCompliancePolicy)] != 3 ||
		byAction[string(store.AuditComplianceAssign)] != 2 {
		t.Fatalf("稽核共 %d 筆，分佈=%v", len(page.Items), byAction)
	}
}

// TestOperatorComplianceRejectionsMapToStatus pins the rejections an operator
// can actually hit, because a 500 here would read as "Hub 壞了" instead of
// "你的 request 已經過期了".
func TestOperatorComplianceRejectionsMapToStatus(t *testing.T) {
	f := settingAPIFixture(t)

	for name, tc := range map[string]struct {
		path string
		body any
		want int
	}{
		"一條規則都沒有": {"/v1/operator/compliance-policies/preview",
			compliancePolicyPreviewOperatorRequest{PolicyID: "empty"}, http.StatusBadRequest},
		"門檻超出範圍": {"/v1/operator/compliance-policies/preview",
			compliancePolicyPreviewOperatorRequest{PolicyID: "tiny", Rules: []complianceRuleWire{
				{Kind: "checkin_max_age", MaxAgeSeconds: 1}}}, http.StatusBadRequest},
		"不認得的規則": {"/v1/operator/compliance-policies/preview",
			compliancePolicyPreviewOperatorRequest{PolicyID: "odd", Rules: []complianceRuleWire{
				{Kind: "reboot_daily"}}}, http.StatusBadRequest},
		"參數放錯規則": {"/v1/operator/compliance-policies/preview",
			compliancePolicyPreviewOperatorRequest{PolicyID: "mixed", Rules: []complianceRuleWire{
				{Kind: "jobs_enabled", MaxAgeSeconds: 300}}}, http.StatusBadRequest},
		"指派沒發佈過的原則": {"/v1/operator/compliance-assignments/preview",
			complianceAssignmentPreviewOperatorRequest{Scope: "machine", ScopeID: f.machine.id,
				PolicyID: "never-published", Revision: 1}, http.StatusNotFound},
	} {
		t.Run(name, func(t *testing.T) {
			rec := settingPost(t, f, tc.path, "", tc.body)
			if rec.Code != tc.want {
				t.Fatalf("%s=%d，想要 %d：%s", tc.path, rec.Code, tc.want, rec.Body.String())
			}
		})
	}

	// 預覽過期：規則在按下確認之前變了。
	rules := []complianceRuleWire{{Kind: "jobs_enabled"}}
	var preview operator.CompliancePolicyPreviewResult
	decodeSettingJSON(t, settingPost(t, f, "/v1/operator/compliance-policies/preview", "",
		compliancePolicyPreviewOperatorRequest{PolicyID: "floor", Rules: rules}),
		http.StatusOK, &preview)
	zero := int64(0)
	stale := settingPost(t, f, "/v1/operator/compliance-policies", "compliance-stale-key",
		compliancePolicyPublishOperatorRequest{PolicyID: "floor", Rules: rules,
			ExpectedRevision: &zero, PreviewDigest: "sha256:" +
				"0000000000000000000000000000000000000000000000000000000000000000",
			ConfirmPolicyID: "floor", Reason: "試試看"})
	if stale.Code != http.StatusPreconditionFailed {
		t.Fatalf("過期的預覽=%d：%s", stale.Code, stale.Body.String())
	}

	// revision 撞車：帳本上的事實，不是壞掉的 request。
	var firstVersion store.OperatorCompliancePolicyResult
	decodeSettingJSON(t, settingPost(t, f, "/v1/operator/compliance-policies", "compliance-first-key",
		compliancePolicyPublishOperatorRequest{PolicyID: "floor", Rules: rules,
			ExpectedRevision: &zero, PreviewDigest: preview.PreviewDigest,
			ConfirmPolicyID: "floor", Reason: "第一版"}),
		http.StatusCreated, &firstVersion)
	if firstVersion.Revision != 1 {
		t.Fatalf("第一版=%+v", firstVersion)
	}
	conflict := settingPost(t, f, "/v1/operator/compliance-policies", "compliance-conflict-key",
		compliancePolicyPublishOperatorRequest{PolicyID: "floor",
			Rules:            []complianceRuleWire{{Kind: "settings_applied"}},
			ExpectedRevision: &zero, PreviewDigest: preview.PreviewDigest,
			ConfirmPolicyID: "floor", Reason: "撞車"})
	if conflict.Code != http.StatusConflict {
		t.Fatalf("revision 撞車=%d：%s", conflict.Code, conflict.Body.String())
	}
}

// 一個 GET 盤面不接受 query parameters：靜靜忽略掉篩選條件會讓讀的人以為
// 他看到的是被篩過的機隊。
func TestOperatorComplianceBoardRefusesQueryParameters(t *testing.T) {
	f := settingAPIFixture(t)
	rec := operatorRequest(t, f.mux, http.MethodGet, "/v1/operator/compliance?verdict=compliant", "", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("帶 query 的盤面=%d：%s", rec.Code, rec.Body.String())
	}
}

// 動作住在原則文件裡，所以它一路繼承既有的 preview／確認／digest／稽核：這個
// 測試盯住那條路上每一段都真的帶著它，而不是只有規則過得去。
func TestOperatorComplianceActionsRideInsideThePolicyDocument(t *testing.T) {
	f := settingAPIFixture(t)
	rules := []complianceRuleWire{{Kind: "checkin_max_age", MaxAgeSeconds: 900}}
	actions := []complianceActionWire{{Kind: "block_jobs", GraceSeconds: 3600}}

	var preview operator.CompliancePolicyPreviewResult
	decodeSettingJSON(t, settingPost(t, f, "/v1/operator/compliance-policies/preview", "",
		compliancePolicyPreviewOperatorRequest{PolicyID: "job-floor", Rules: rules, Actions: actions}),
		http.StatusOK, &preview)
	if len(preview.Actions) != 1 || preview.Actions[0].Kind != compliance.ActionBlockJobs ||
		preview.Actions[0].Label != "停發工作單" ||
		preview.Actions[0].Effect != "不再領到新的工作單，也不會被算進新的部署" ||
		preview.Actions[0].Grace != "連續不符合 1 小時後生效" {
		t.Fatalf("預覽沒有說出這份原則會做什麼：%+v", preview.Actions)
	}

	zero := int64(0)
	var published store.OperatorCompliancePolicyResult
	decodeSettingJSON(t, settingPost(t, f, "/v1/operator/compliance-policies", "action-publish-key",
		compliancePolicyPublishOperatorRequest{PolicyID: "job-floor", Rules: rules, Actions: actions,
			ExpectedRevision: &zero, PreviewDigest: preview.PreviewDigest,
			ConfirmPolicyID: "job-floor", Reason: "沒在報到的就別發單了"}),
		http.StatusCreated, &published)
	if published.Revision != 1 || len(published.Policy.Actions) != 1 ||
		published.Policy.Actions[0].GraceSeconds != 3600 {
		t.Fatalf("發佈的原則沒有帶著動作：%+v", published.Policy)
	}

	// 只改寬限期就是改了一個決定：它必須是新的 revision，不是「沒有變化」。
	var shorter operator.CompliancePolicyPreviewResult
	decodeSettingJSON(t, settingPost(t, f, "/v1/operator/compliance-policies/preview", "",
		compliancePolicyPreviewOperatorRequest{PolicyID: "job-floor", Rules: rules,
			Actions: []complianceActionWire{{Kind: "block_jobs", GraceSeconds: 600}}}),
		http.StatusOK, &shorter)
	if shorter.Unchanged || shorter.NextRev != 2 {
		t.Fatalf("換了寬限期卻說沒變：%+v", shorter)
	}
	// 拿掉動作也是改決定 —— 機隊會從「會被停發」變成「只回報」。
	var reportOnly operator.CompliancePolicyPreviewResult
	decodeSettingJSON(t, settingPost(t, f, "/v1/operator/compliance-policies/preview", "",
		compliancePolicyPreviewOperatorRequest{PolicyID: "job-floor", Rules: rules}),
		http.StatusOK, &reportOnly)
	if reportOnly.Unchanged || reportOnly.NextRev != 2 || len(reportOnly.Actions) != 0 {
		t.Fatalf("拿掉動作卻說沒變：%+v", reportOnly)
	}

	var assignPreview operator.ComplianceAssignmentPreviewResult
	decodeSettingJSON(t, settingPost(t, f, "/v1/operator/compliance-assignments/preview", "",
		complianceAssignmentPreviewOperatorRequest{Scope: "machine", ScopeID: f.machine.id,
			PolicyID: "job-floor", Revision: 1}), http.StatusOK, &assignPreview)
	if len(assignPreview.Actions) != 1 || assignPreview.Actions[0].Grace != "連續不符合 1 小時後生效" {
		t.Fatalf("指派預覽沒說出會對這台做什麼：%+v", assignPreview.Actions)
	}
	decodeSettingJSON(t, settingPost(t, f, "/v1/operator/compliance-assignments", "action-assign-key",
		complianceAssignmentOperatorRequest{Scope: "machine", ScopeID: f.machine.id,
			PolicyID: "job-floor", Revision: 1, PreviewDigest: assignPreview.PreviewDigest,
			ConfirmScopeID: f.machine.id, Reason: "先在這一台上線"}),
		http.StatusCreated, &store.OperatorComplianceAssignmentResult{})

	// 從未報到的機器不會被動作碰到，盤面也不該說有人被停發。
	board := complianceBoard(t, f)
	if board.Machines[0].Verdict != compliance.VerdictNeverReported || board.Blocked != 0 ||
		len(board.Machines[0].Actions) != 1 ||
		board.Machines[0].Actions[0].State != compliance.ActionStateNotTriggered ||
		board.Machines[0].Actions[0].StateLabel != "未觸發" {
		t.Fatalf("從未報到的機器被動作碰到了：blocked=%d %+v", board.Blocked, board.Machines[0].Actions)
	}

	// 報到超過新鮮度上限、而且已經超過寬限期：動作生效，盤面數得出來。
	stale := time.Now().UTC().Add(-3 * time.Hour)
	if err := f.store.RecordCheckin(f.machine.id, model.Checkin{
		SchemaVersion: model.SchemaVersion, SentAt: stale, AgentVersion: "test",
	}, stale); err != nil {
		t.Fatalf("寫入測試 check-in 失敗：%v", err)
	}
	board = complianceBoard(t, f)
	if board.Machines[0].Verdict != compliance.VerdictNoncompliant || board.Blocked != 1 ||
		board.Machines[0].Actions[0].State != compliance.ActionStateEnforced ||
		board.Machines[0].Actions[0].StateLabel != "生效中" ||
		board.Machines[0].Actions[0].Since == nil ||
		board.Machines[0].Actions[0].DueAt != nil {
		t.Fatalf("停發工作單沒有生效：blocked=%d %+v", board.Blocked, board.Machines[0].Actions)
	}
}

// 動作跟規則一樣要在 preview 就被擋下來：一個 Hub 做不到的後果，不可以變成
// 一份已發佈的原則。
func TestOperatorComplianceRefusesActionsItCannotCarryOut(t *testing.T) {
	f := settingAPIFixture(t)
	rules := []complianceRuleWire{{Kind: "checkin_max_age", MaxAgeSeconds: 900}}
	for name, actions := range map[string][]complianceActionWire{
		"不認得的動作":  {{Kind: "wipe_device"}},
		"寬限期是負的":  {{Kind: "block_jobs", GraceSeconds: -1}},
		"寬限期超過一天": {{Kind: "block_jobs", GraceSeconds: compliance.MaxGraceSeconds + 1}},
		"同一個後果兩次": {{Kind: "block_jobs", GraceSeconds: 60}, {Kind: "block_jobs", GraceSeconds: 120}},
	} {
		t.Run(name, func(t *testing.T) {
			rec := settingPost(t, f, "/v1/operator/compliance-policies/preview", "",
				compliancePolicyPreviewOperatorRequest{PolicyID: "bad", Rules: rules, Actions: actions})
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("=%d：%s", rec.Code, rec.Body.String())
			}
		})
	}
}
