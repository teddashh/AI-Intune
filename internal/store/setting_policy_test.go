package store

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/model"
	"github.com/teddashh/AI-Intune/internal/settingpolicy"
)

func settingsAt(checkin, observation int) settingpolicy.Settings {
	return settingpolicy.Settings{SchemaVersion: settingpolicy.SchemaVersion,
		CheckinIntervalSeconds: checkin, ObservationIntervalSeconds: observation}
}

// publishSetting drives the real endpoint, including its preview digest, so a
// fixture cannot pass against a store that stopped checking one.
func publishSetting(t *testing.T, s *Store, id string, expected int64,
	set settingpolicy.Settings, key string) OperatorSettingPolicyResult {
	t.Helper()
	preview, err := SettingPolicyPreviewDigest(id, expected, set)
	if err != nil {
		t.Fatalf("preview digest: %v", err)
	}
	res, err := s.ApplyOperatorSettingPolicy(OperatorSettingPolicyRequest{
		PolicyID: id, Settings: set, ExpectedRevision: &expected, PreviewDigest: preview,
		ConfirmPolicyID: id, Reason: "測試", PublishedBy: "tester",
		IdempotencyKey: key, RequestDigest: "sha256:" + strings.Repeat("a", 64),
		Audit: AuditEntry{SourceAddr: "test"},
	})
	if err != nil {
		t.Fatalf("publish %s: %v", id, err)
	}
	return res
}

func assignSetting(t *testing.T, s *Store, scope settingpolicy.Scope, scopeID, policyID string,
	rev int64, key string) OperatorSettingAssignmentResult {
	t.Helper()
	res, err := s.ApplyOperatorSettingAssignment(OperatorSettingAssignmentRequest{
		Scope: scope, ScopeID: scopeID, PolicyID: policyID, PolicyRevision: rev,
		PreviewDigest:  SettingAssignmentPreviewDigest(scope, scopeID, policyID, rev),
		ConfirmScopeID: scopeID, Reason: "測試", AssignedBy: "tester",
		IdempotencyKey: key, RequestDigest: "sha256:" + strings.Repeat("b", 64),
		Audit: AuditEntry{SourceAddr: "test"},
	})
	if err != nil {
		t.Fatalf("assign %s/%s: %v", scope, scopeID, err)
	}
	return res
}

func TestPublishSettingPolicyMintsOneRevisionPerRealChange(t *testing.T) {
	s := newTestStore(t)

	first := publishSetting(t, s, "fleet-pace", 0, settingsAt(120, 600), "k1")
	if first.Revision != 1 || first.Unchanged {
		t.Fatalf("第一次發佈應該是 revision 1：%+v", first)
	}

	// Republishing identical values is not a new decision. Minting a revision
	// here would make every assigned machine look pending until it checked in,
	// for a change nobody made.
	same := publishSetting(t, s, "fleet-pace", 1, settingsAt(120, 600), "k2")
	if same.Revision != 1 || !same.Unchanged {
		t.Fatalf("重發同一組值不該產生新 revision：%+v", same)
	}

	changed := publishSetting(t, s, "fleet-pace", 1, settingsAt(300, 900), "k3")
	if changed.Revision != 2 || changed.Unchanged {
		t.Fatalf("改了值應該產生 revision 2：%+v", changed)
	}
	if changed.Digest == first.Digest {
		t.Fatal("改了值 digest 卻沒變")
	}

	revs, err := s.SettingPolicyRevisions("fleet-pace")
	if err != nil {
		t.Fatal(err)
	}
	if len(revs) != 2 || revs[0].Revision != 2 {
		t.Fatalf("revision 歷史不對：%+v", revs)
	}
	// The older revision survives publication of the newer one, because a
	// machine may still be running it.
	if revs[1].Settings.CheckinIntervalSeconds != 120 {
		t.Fatalf("舊 revision 被改寫了：%+v", revs[1])
	}
}

func TestPublishSettingPolicyRefusesEveryIncompleteOrStaleRequest(t *testing.T) {
	s := newTestStore(t)
	publishSetting(t, s, "fleet-pace", 0, settingsAt(120, 600), "seed")

	valid := settingsAt(300, 900)
	base := func() OperatorSettingPolicyRequest {
		one := int64(1)
		preview, _ := SettingPolicyPreviewDigest("fleet-pace", 1, valid)
		return OperatorSettingPolicyRequest{
			PolicyID: "fleet-pace", Settings: valid, ExpectedRevision: &one,
			PreviewDigest: preview, ConfirmPolicyID: "fleet-pace", Reason: "測試",
			PublishedBy: "tester", RequestDigest: "sha256:" + strings.Repeat("c", 64),
			Audit: AuditEntry{SourceAddr: "test"},
		}
	}
	for name, tc := range map[string]struct {
		mutate func(*OperatorSettingPolicyRequest)
		want   string
	}{
		"沒有理由":        {func(r *OperatorSettingPolicyRequest) { r.Reason = " " }, OperatorCodeReasonRequired},
		"確認欄位打錯":      {func(r *OperatorSettingPolicyRequest) { r.ConfirmPolicyID = "fleet-pace2" }, OperatorCodeConfirmationMismatch},
		"設定值不合法":      {func(r *OperatorSettingPolicyRequest) { r.Settings = settingsAt(5, 600) }, OperatorCodeSettingPolicyInvalid},
		"policy_id 怪": {func(r *OperatorSettingPolicyRequest) { r.PolicyID, r.ConfirmPolicyID = "Fleet Pace", "Fleet Pace" }, OperatorCodeSettingPolicyInvalid},
		"少了 revision": {func(r *OperatorSettingPolicyRequest) { r.ExpectedRevision = nil }, OperatorCodePreconditionRequired},
		"revision 過期": {func(r *OperatorSettingPolicyRequest) { zero := int64(0); r.ExpectedRevision = &zero }, OperatorCodeSettingPolicyConflict},
		"預覽過期":        {func(r *OperatorSettingPolicyRequest) { r.PreviewDigest = "sha256:" + strings.Repeat("0", 64) }, OperatorCodeSettingPreviewStale},
	} {
		req := base()
		req.IdempotencyKey = "reject-" + name
		tc.mutate(&req)
		_, err := s.ApplyOperatorSettingPolicy(req)
		var rejection *OperatorRequestError
		if !errors.As(err, &rejection) {
			t.Errorf("%s：拿到 %v，應該是 operator rejection", name, err)
			continue
		}
		if rejection.Code != tc.want {
			t.Errorf("%s：code = %q，want %q（%s）", name, rejection.Code, tc.want, rejection.Detail)
		}
	}
	// None of the refusals wrote a revision.
	revs, err := s.SettingPolicyRevisions("fleet-pace")
	if err != nil || len(revs) != 1 {
		t.Fatalf("被拒絕的請求竟然改了 revision 歷史：%+v %v", revs, err)
	}
}

// The replay contract: one press, one audit row; retrying the same request
// returns the same verdict without doing the write again.
func TestSettingWritesReplayInsteadOfRunningTwice(t *testing.T) {
	s := newTestStore(t)
	set := settingsAt(240, 900)
	preview, _ := SettingPolicyPreviewDigest("pace", 0, set)
	zero := int64(0)
	req := OperatorSettingPolicyRequest{
		PolicyID: "pace", Settings: set, ExpectedRevision: &zero, PreviewDigest: preview,
		ConfirmPolicyID: "pace", Reason: "測試", PublishedBy: "tester",
		IdempotencyKey: "same-key", RequestDigest: "sha256:" + strings.Repeat("d", 64),
		Audit: AuditEntry{SourceAddr: "test"},
	}
	first, err := s.ApplyOperatorSettingPolicy(req)
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.ApplyOperatorSettingPolicy(req)
	if err != nil {
		t.Fatal(err)
	}
	if !again.Replayed || again.Revision != first.Revision {
		t.Fatalf("重送應該原樣回放：%+v", again)
	}
	revs, _ := s.SettingPolicyRevisions("pace")
	if len(revs) != 1 {
		t.Fatalf("重送寫了 %d 個 revision", len(revs))
	}

	// Same key, different body is a conflict rather than a silent overwrite.
	conflict := req
	conflict.RequestDigest = "sha256:" + strings.Repeat("e", 64)
	_, err = s.ApplyOperatorSettingPolicy(conflict)
	var rejection *OperatorRequestError
	if !errors.As(err, &rejection) || rejection.Code != OperatorCodeIdempotencyConflict {
		t.Fatalf("同一把 key 換 body 應該衝突，拿到 %v", err)
	}

	// A cached rejection replays as the same rejection, so a retry cannot pick
	// up a different answer once the fleet changes underneath it. The audit page
	// must show both failed attempts, and the replay must not appear successful.
	bad := req
	bad.IdempotencyKey = "bad-key"
	bad.ConfirmPolicyID = "wrong-confirmation"
	_, err = s.ApplyOperatorSettingPolicy(bad)
	if !errors.As(err, &rejection) {
		t.Errorf("第一次送出拿到 %v，expected *OperatorRequestError；operator 會誤以為確認欄錯誤可以直接重試", err)
		return
	}
	if rejection.Code != OperatorCodeConfirmationMismatch {
		t.Errorf("第一次送出的 code = %q，expected %q；operator 會修改錯誤的請求欄位", rejection.Code, OperatorCodeConfirmationMismatch)
	}
	original := rejection.Detail
	expectedWrongConfirmationFragment := fmt.Sprintf("%q", bad.ConfirmPolicyID)
	if !strings.Contains(original, expectedWrongConfirmationFragment) {
		t.Errorf("第一次送出的 detail = %q，expected to contain %q；operator 不知道自己實際送出了哪個錯誤 ID", original, expectedWrongConfirmationFragment)
	}
	expectedPolicyIDFragment := fmt.Sprintf("%q", bad.PolicyID)
	if !strings.Contains(original, expectedPolicyIDFragment) {
		t.Errorf("第一次送出的 detail = %q，expected to contain %q；operator 不知道確認欄應改成哪個 policy ID", original, expectedPolicyIDFragment)
	}
	_, err = s.ApplyOperatorSettingPolicy(bad)
	if !errors.As(err, &rejection) {
		t.Errorf("重送拿到 %v，expected *OperatorRequestError；operator 會把已拒絕的 key 誤認為可再次執行", err)
		return
	}
	if !rejection.Replayed {
		t.Errorf("重送 replayed = %v，expected %v；operator 會誤以為這是依目前機隊狀態產生的新判決", rejection.Replayed, true)
	}
	if rejection.Code != OperatorCodeConfirmationMismatch {
		t.Errorf("重送的 code = %q，expected %q；operator 會採取與原判決不同的修正動作", rejection.Code, OperatorCodeConfirmationMismatch)
	}
	if rejection.Detail != original {
		t.Errorf("重送的 detail = %q，expected %q；operator 會失去原判決指出的精確修正路徑", rejection.Detail, original)
	}

	page, err := s.ListAuditReads(AuditReadFilter{
		Outcome: AuditOutcomeFailed, Correlation: bad.IdempotencyKey, Limit: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 2 {
		t.Errorf("失敗稽核列數 = %d，expected %d；operator 會誤判這把 key 是否曾被拒絕後重送", len(page.Items), 2)
		return
	}
	var replayDetail string
	for _, item := range page.Items {
		if strings.HasPrefix(item.Detail, OperatorIdempotencyReplayPrefix) {
			replayDetail = item.Detail
			break
		}
	}
	if replayDetail == "" {
		t.Errorf("重放稽核 detail = %q，expected prefix %q；operator 會把原拒絕列誤當成唯一一次嘗試", replayDetail, OperatorIdempotencyReplayPrefix)
		return
	}
	if !strings.Contains(replayDetail, original) {
		t.Errorf("重放稽核 detail = %q，expected to contain %q；operator 從稽核頁無法得知重送沿用了哪一句原判決", replayDetail, original)
	}
}

func TestAssignSettingPolicyPinsScopesAndRefusesTheRest(t *testing.T) {
	s := newTestStore(t)
	now := time.Date(2026, 9, 12, 8, 0, 0, 0, time.UTC)
	machine := mustEnroll(t, s, "samplehub1", now)
	publishSetting(t, s, "pace", 0, settingsAt(120, 600), "p1")

	got := assignSetting(t, s, settingpolicy.ScopeMachine, machine, "pace", 1, "a1")
	if got.Revision != 1 || got.Unchanged {
		t.Fatalf("第一次指派應該是 revision 1：%+v", got)
	}
	// Re-assigning the digest already in force changes nothing on the wire, so
	// it must not make every machine look pending again.
	same := assignSetting(t, s, settingpolicy.ScopeMachine, machine, "pace", 1, "a2")
	if same.Revision != 1 || !same.Unchanged {
		t.Fatalf("重複指派同一份不該產生新 revision：%+v", same)
	}

	assignSetting(t, s, settingpolicy.ScopeChannel, "canary", "pace", 1, "a3")

	rows, err := s.CurrentSettingAssignments()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("目前的指派有 %d 列，want 2：%+v", len(rows), rows)
	}

	for name, tc := range map[string]struct {
		req  OperatorSettingAssignmentRequest
		want string
	}{
		"沒發佈過的 revision": {OperatorSettingAssignmentRequest{
			Scope: settingpolicy.ScopeMachine, ScopeID: machine, PolicyID: "pace", PolicyRevision: 9,
			ConfirmScopeID: machine, Reason: "x"}, OperatorCodeSettingPolicyNotFound},
		"不存在的機器": {OperatorSettingAssignmentRequest{
			Scope: settingpolicy.ScopeMachine, ScopeID: "no-such", PolicyID: "pace", PolicyRevision: 1,
			ConfirmScopeID: "no-such", Reason: "x"}, OperatorCodeMachineNotFound},
		"不是 channel 的字": {OperatorSettingAssignmentRequest{
			Scope: settingpolicy.ScopeChannel, ScopeID: "prod", PolicyID: "pace", PolicyRevision: 1,
			ConfirmScopeID: "prod", Reason: "x"}, OperatorCodeBadChannel},
		"任意 scope": {OperatorSettingAssignmentRequest{
			Scope: settingpolicy.Scope("tag"), ScopeID: "gpu", PolicyID: "pace", PolicyRevision: 1,
			ConfirmScopeID: "gpu", Reason: "x"}, OperatorCodeSettingScopeInvalid},
		"確認欄位打錯": {OperatorSettingAssignmentRequest{
			Scope: settingpolicy.ScopeMachine, ScopeID: machine, PolicyID: "pace", PolicyRevision: 1,
			ConfirmScopeID: "samplehub1", Reason: "x"}, OperatorCodeConfirmationMismatch},
		"沒有理由": {OperatorSettingAssignmentRequest{
			Scope: settingpolicy.ScopeMachine, ScopeID: machine, PolicyID: "pace", PolicyRevision: 1,
			ConfirmScopeID: machine, Reason: " "}, OperatorCodeReasonRequired},
	} {
		req := tc.req
		req.PreviewDigest = SettingAssignmentPreviewDigest(req.Scope, req.ScopeID, req.PolicyID, req.PolicyRevision)
		req.IdempotencyKey = "assign-reject-" + name
		req.RequestDigest = "sha256:" + strings.Repeat("f", 64)
		req.AssignedBy, req.Audit = "tester", AuditEntry{SourceAddr: "test"}
		_, err := s.ApplyOperatorSettingAssignment(req)
		var rejection *OperatorRequestError
		if !errors.As(err, &rejection) {
			t.Errorf("%s：拿到 %v，應該是 operator rejection", name, err)
			continue
		}
		if rejection.Code != tc.want {
			t.Errorf("%s：code = %q，want %q（%s）", name, rejection.Code, tc.want, rejection.Detail)
		}
	}
	if rows, _ := s.CurrentSettingAssignments(); len(rows) != 2 {
		t.Fatalf("被拒絕的指派改了目前生效的列數：%+v", rows)
	}
}

func TestResolveMachineSettingsPrefersMachineOverChannelOverDefault(t *testing.T) {
	s := newTestStore(t)
	now := time.Date(2026, 9, 12, 8, 0, 0, 0, time.UTC)
	a := mustEnroll(t, s, "samplehub1", now)
	b := mustEnroll(t, s, "sampleagent2", now)

	if got, err := s.ResolveMachineSettings(a); err != nil || got.Source != settingpolicy.SourceDefault {
		t.Fatalf("沒有指派時應該是預設值：%+v %v", got, err)
	}
	if got, _ := s.ResolveMachineSettings(a); got.Settings != settingpolicy.Defaults() {
		t.Fatalf("預設值不對：%+v", got.Settings)
	}

	publishSetting(t, s, "slow", 0, settingsAt(600, 1800), "p1")
	publishSetting(t, s, "fast", 0, settingsAt(60, 300), "p2")
	setChannel(t, s, a, "canary")
	setChannel(t, s, b, "canary")
	assignSetting(t, s, settingpolicy.ScopeChannel, "canary", "slow", 1, "a1")

	for _, id := range []string{a, b} {
		got, err := s.ResolveMachineSettings(id)
		if err != nil {
			t.Fatal(err)
		}
		if got.Source != settingpolicy.SourceChannel || got.Settings.CheckinIntervalSeconds != 600 {
			t.Fatalf("%s 沒有吃到 channel 指派：%+v", id, got)
		}
	}

	assignSetting(t, s, settingpolicy.ScopeMachine, a, "fast", 1, "a2")
	got, err := s.ResolveMachineSettings(a)
	if err != nil {
		t.Fatal(err)
	}
	if got.Source != settingpolicy.SourceMachine || got.Settings.CheckinIntervalSeconds != 60 {
		t.Fatalf("機器自己的指派沒有蓋過 channel：%+v", got)
	}
	// The other machine on the same channel is untouched.
	if other, _ := s.ResolveMachineSettings(b); other.Source != settingpolicy.SourceChannel {
		t.Fatalf("指派一台影響到了同 channel 的另一台：%+v", other)
	}
	// An unknown caller gets defaults rather than an error: refusing to answer
	// would stop a heartbeat over a settings question.
	if got, err := s.ResolveMachineSettings("never-enrolled"); err != nil ||
		got.Source != settingpolicy.SourceDefault {
		t.Fatalf("不認識的機器應該拿預設值：%+v %v", got, err)
	}
}

// The board reports only what machines told the Hub, and never calls an
// unmeasured machine applied.
func TestMachineSettingStatesJudgeOnlyFromWhatTheAgentReported(t *testing.T) {
	s := newTestStore(t)
	now := time.Date(2026, 9, 12, 8, 0, 0, 0, time.UTC)
	quiet := mustEnroll(t, s, "quiet", now)
	old := mustEnroll(t, s, "old-agent", now)
	good := mustEnroll(t, s, "good", now)
	behind := mustEnroll(t, s, "behind", now)
	rogue := mustEnroll(t, s, "rogue", now)

	publishSetting(t, s, "pace", 0, settingsAt(120, 600), "p1")
	v1 := settingpolicy.MustDigest(settingsAt(120, 600))
	publishSetting(t, s, "pace", 1, settingsAt(300, 900), "p2")
	v2 := settingpolicy.MustDigest(settingsAt(300, 900))

	for _, id := range []string{old, good, behind, rogue} {
		assignSetting(t, s, settingpolicy.ScopeMachine, id, "pace", 2, "a-"+id)
	}
	checkinWithSettings(t, s, old, "", now)
	checkinWithSettings(t, s, good, v2, now)
	checkinWithSettings(t, s, behind, v1, now)
	checkinWithSettings(t, s, rogue, "sha256:"+strings.Repeat("9", 64), now)

	states, err := s.MachineSettingStates()
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]MachineSettingState{}
	for _, st := range states {
		byID[st.MachineID] = st
	}
	for id, want := range map[string]settingpolicy.Verdict{
		quiet:  settingpolicy.VerdictNeverReported,
		old:    settingpolicy.VerdictUnknown,
		good:   settingpolicy.VerdictApplied,
		behind: settingpolicy.VerdictPending,
		rogue:  settingpolicy.VerdictMismatch,
	} {
		if got := byID[id].Verdict; got != want {
			t.Errorf("%s 的判決 = %q，want %q", byID[id].DisplayName, got, want)
		}
	}
	// A machine running the product defaults with nothing assigned is applied,
	// not pending: there is no operator decision it is behind on.
	checkinWithSettings(t, s, quiet, settingpolicy.MustDigest(settingpolicy.Defaults()), now)
	states, _ = s.MachineSettingStates()
	for _, st := range states {
		if st.MachineID == quiet && st.Verdict != settingpolicy.VerdictApplied {
			t.Fatalf("跑預設值的機器判成 %q", st.Verdict)
		}
	}
}

func setChannel(t *testing.T, s *Store, machineID, channel string) {
	t.Helper()
	if _, err := s.DB().Exec(`UPDATE machine_registry SET channel=? WHERE machine_id=?`,
		channel, machineID); err != nil {
		t.Fatalf("set channel: %v", err)
	}
}

// checkinWithSettings records one heartbeat carrying the digest the agent says
// it is running, which is the only applied-state evidence the Hub accepts.
func checkinWithSettings(t *testing.T, s *Store, machineID, digest string, at time.Time) {
	t.Helper()
	if err := s.RecordCheckin(machineID, model.Checkin{
		SchemaVersion: model.SchemaVersion, SentAt: at, AgentVersion: "test",
		SettingsDigest: digest,
	}, at); err != nil {
		t.Fatalf("record checkin: %v", err)
	}
}
