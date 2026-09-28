package store

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/compliance"
	"github.com/teddashh/AI-Intune/internal/model"
	"github.com/teddashh/AI-Intune/internal/settingpolicy"
)

func rules(r ...compliance.Rule) compliance.Policy {
	return compliance.Policy{SchemaVersion: compliance.SchemaVersion, Rules: r}
}

func freshnessRule(seconds int) compliance.Rule {
	return compliance.Rule{Kind: compliance.RuleCheckinMaxAge, MaxAgeSeconds: seconds}
}

// publishCompliance drives the real endpoint, including its preview digest, so
// a fixture cannot pass against a store that stopped checking one.
func publishCompliance(t *testing.T, s *Store, id string, expected int64,
	p compliance.Policy, key string) OperatorCompliancePolicyResult {
	t.Helper()
	preview, err := CompliancePolicyPreviewDigest(id, expected, p)
	if err != nil {
		t.Fatalf("preview digest: %v", err)
	}
	res, err := s.ApplyOperatorCompliancePolicy(OperatorCompliancePolicyRequest{
		PolicyID: id, Policy: p, ExpectedRevision: &expected, PreviewDigest: preview,
		ConfirmPolicyID: id, Reason: "測試", PublishedBy: "tester",
		IdempotencyKey: key, RequestDigest: "sha256:" + strings.Repeat("a", 64),
		Audit: AuditEntry{SourceAddr: "test"},
	})
	if err != nil {
		t.Fatalf("publish %s: %v", id, err)
	}
	return res
}

func assignCompliance(t *testing.T, s *Store, scope compliance.Scope, scopeID, policyID string,
	rev int64, key string) OperatorComplianceAssignmentResult {
	t.Helper()
	res, err := s.ApplyOperatorComplianceAssignment(OperatorComplianceAssignmentRequest{
		Scope: scope, ScopeID: scopeID, PolicyID: policyID, PolicyRevision: rev,
		PreviewDigest:  ComplianceAssignmentPreviewDigest(scope, scopeID, policyID, rev),
		ConfirmScopeID: scopeID, Reason: "測試", AssignedBy: "tester",
		IdempotencyKey: key, RequestDigest: "sha256:" + strings.Repeat("b", 64),
		Audit: AuditEntry{SourceAddr: "test"},
	})
	if err != nil {
		t.Fatalf("assign %s/%s: %v", scope, scopeID, err)
	}
	return res
}

func TestPublishCompliancePolicyMintsOneRevisionPerRealChange(t *testing.T) {
	s := newTestStore(t)

	first := publishCompliance(t, s, "fleet-floor", 0, rules(freshnessRule(600)), "k1")
	if first.Revision != 1 || first.Unchanged {
		t.Fatalf("第一次發佈應該是 revision 1：%+v", first)
	}

	// 同一組規則換個順序寫，是同一份規則。產生 revision 只會讓看板上多一次
	// 什麼都沒改變的移動。
	reordered := rules(compliance.Rule{Kind: compliance.RuleJobsEnabled}, freshnessRule(600))
	publishCompliance(t, s, "fleet-floor", 1,
		rules(freshnessRule(600), compliance.Rule{Kind: compliance.RuleJobsEnabled}), "k2")
	same := publishCompliance(t, s, "fleet-floor", 2, reordered, "k3")
	if same.Revision != 2 || !same.Unchanged {
		t.Fatalf("換個順序寫同一組規則不該產生新 revision：%+v", same)
	}

	changed := publishCompliance(t, s, "fleet-floor", 2, rules(freshnessRule(300)), "k4")
	if changed.Revision != 3 || changed.Digest == first.Digest {
		t.Fatalf("改了門檻應該產生新 revision 與新 digest：%+v", changed)
	}

	revs, err := s.CompliancePolicyRevisions("fleet-floor")
	if err != nil {
		t.Fatal(err)
	}
	if len(revs) != 3 || revs[0].Revision != 3 {
		t.Fatalf("revision 歷史不對：%+v", revs)
	}
	// 舊 revision 留著：被它判過的機器有權知道判它的是哪一份規則。
	if revs[2].Policy.Rules[0].MaxAgeSeconds != 600 {
		t.Fatalf("舊 revision 被改寫了：%+v", revs[2])
	}
	// 存進去的是正規化之後那一份，因為 digest 算的是那一份。
	if revs[1].Policy.Rules[0].Kind != compliance.RuleCheckinMaxAge {
		t.Fatalf("存下來的規則沒有正規化：%+v", revs[1].Policy.Rules)
	}
}

func TestPublishCompliancePolicyRefusesEveryIncompleteOrStaleRequest(t *testing.T) {
	s := newTestStore(t)
	publishCompliance(t, s, "fleet-floor", 0, rules(freshnessRule(600)), "seed")

	valid := rules(freshnessRule(300))
	base := func() OperatorCompliancePolicyRequest {
		one := int64(1)
		preview, _ := CompliancePolicyPreviewDigest("fleet-floor", 1, valid)
		return OperatorCompliancePolicyRequest{
			PolicyID: "fleet-floor", Policy: valid, ExpectedRevision: &one,
			PreviewDigest: preview, ConfirmPolicyID: "fleet-floor", Reason: "測試",
			PublishedBy: "tester", RequestDigest: "sha256:" + strings.Repeat("c", 64),
			Audit: AuditEntry{SourceAddr: "test"},
		}
	}
	for name, tc := range map[string]struct {
		mutate func(*OperatorCompliancePolicyRequest)
		want   string
	}{
		"沒有理由":   {func(r *OperatorCompliancePolicyRequest) { r.Reason = " " }, OperatorCodeReasonRequired},
		"確認欄位打錯": {func(r *OperatorCompliancePolicyRequest) { r.ConfirmPolicyID = "fleet-floor2" }, OperatorCodeConfirmationMismatch},
		"一條規則都沒有": {func(r *OperatorCompliancePolicyRequest) { r.Policy = rules() },
			OperatorCodeCompliancePolicyInvalid},
		"門檻超出範圍": {func(r *OperatorCompliancePolicyRequest) { r.Policy = rules(freshnessRule(1)) },
			OperatorCodeCompliancePolicyInvalid},
		"同一種規則兩條": {func(r *OperatorCompliancePolicyRequest) {
			r.Policy = rules(freshnessRule(300), freshnessRule(600))
		}, OperatorCodeCompliancePolicyInvalid},
		"policy_id 怪": {func(r *OperatorCompliancePolicyRequest) {
			r.PolicyID, r.ConfirmPolicyID = "Fleet Floor", "Fleet Floor"
		}, OperatorCodeCompliancePolicyInvalid},
		"少了 revision": {func(r *OperatorCompliancePolicyRequest) { r.ExpectedRevision = nil }, OperatorCodePreconditionRequired},
		"revision 過期": {func(r *OperatorCompliancePolicyRequest) { zero := int64(0); r.ExpectedRevision = &zero },
			OperatorCodeCompliancePolicyConflict},
		"預覽過期": {func(r *OperatorCompliancePolicyRequest) { r.PreviewDigest = "sha256:" + strings.Repeat("0", 64) },
			OperatorCodeCompliancePreviewStale},
	} {
		req := base()
		req.IdempotencyKey = "reject-" + name
		tc.mutate(&req)
		_, err := s.ApplyOperatorCompliancePolicy(req)
		var rejection *OperatorRequestError
		if !errors.As(err, &rejection) {
			t.Errorf("%s：拿到 %v，應該是 operator rejection", name, err)
			continue
		}
		if rejection.Code != tc.want {
			t.Errorf("%s：code = %q，want %q（%s）", name, rejection.Code, tc.want, rejection.Detail)
		}
	}
	revs, err := s.CompliancePolicyRevisions("fleet-floor")
	if err != nil || len(revs) != 1 {
		t.Fatalf("被拒絕的請求竟然改了 revision 歷史：%+v %v", revs, err)
	}
}

func TestComplianceWritesReplayInsteadOfRunningTwice(t *testing.T) {
	s := newTestStore(t)
	p := rules(freshnessRule(600))
	preview, _ := CompliancePolicyPreviewDigest("floor", 0, p)
	zero := int64(0)
	req := OperatorCompliancePolicyRequest{
		PolicyID: "floor", Policy: p, ExpectedRevision: &zero, PreviewDigest: preview,
		ConfirmPolicyID: "floor", Reason: "測試", PublishedBy: "tester",
		IdempotencyKey: "same-key", RequestDigest: "sha256:" + strings.Repeat("d", 64),
		Audit: AuditEntry{SourceAddr: "test"},
	}
	first, err := s.ApplyOperatorCompliancePolicy(req)
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.ApplyOperatorCompliancePolicy(req)
	if err != nil {
		t.Fatal(err)
	}
	if !again.Replayed || again.Revision != first.Revision {
		t.Fatalf("重送應該原樣回放：%+v", again)
	}
	if revs, _ := s.CompliancePolicyRevisions("floor"); len(revs) != 1 {
		t.Fatalf("重送寫了 %d 個 revision", len(revs))
	}

	conflict := req
	conflict.RequestDigest = "sha256:" + strings.Repeat("e", 64)
	_, err = s.ApplyOperatorCompliancePolicy(conflict)
	var rejection *OperatorRequestError
	if !errors.As(err, &rejection) || rejection.Code != OperatorCodeIdempotencyConflict {
		t.Fatalf("同一把 key 換 body 應該衝突，拿到 %v", err)
	}

	bad := req
	bad.IdempotencyKey, bad.Reason = "bad-key", " "
	if _, err := s.ApplyOperatorCompliancePolicy(bad); err == nil {
		t.Fatal("空 reason 應該被拒絕")
	}
	_, err = s.ApplyOperatorCompliancePolicy(bad)
	if !errors.As(err, &rejection) || !rejection.Replayed ||
		rejection.Code != OperatorCodeReasonRequired {
		t.Fatalf("被拒絕的請求重送應該回放同一個判決，拿到 %v", err)
	}
}

func TestAssignCompliancePolicyPinsScopesAndRefusesTheRest(t *testing.T) {
	s := newTestStore(t)
	now := time.Date(2026, 9, 12, 8, 0, 0, 0, time.UTC)
	machine := mustEnroll(t, s, "samplehub1", now)
	publishCompliance(t, s, "floor", 0, rules(freshnessRule(600)), "p1")

	got := assignCompliance(t, s, compliance.ScopeMachine, machine, "floor", 1, "a1")
	if got.Revision != 1 || got.Unchanged {
		t.Fatalf("第一次指派應該是 revision 1：%+v", got)
	}
	same := assignCompliance(t, s, compliance.ScopeMachine, machine, "floor", 1, "a2")
	if same.Revision != 1 || !same.Unchanged {
		t.Fatalf("重複指派同一份不該產生新 revision：%+v", same)
	}
	assignCompliance(t, s, compliance.ScopeChannel, "canary", "floor", 1, "a3")

	rows, err := s.CurrentComplianceAssignments()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("目前的指派有 %d 列，want 2：%+v", len(rows), rows)
	}

	for name, tc := range map[string]struct {
		req  OperatorComplianceAssignmentRequest
		want string
	}{
		"沒發佈過的 revision": {OperatorComplianceAssignmentRequest{
			Scope: compliance.ScopeMachine, ScopeID: machine, PolicyID: "floor", PolicyRevision: 9,
			ConfirmScopeID: machine, Reason: "x"}, OperatorCodeCompliancePolicyNotFound},
		"不存在的機器": {OperatorComplianceAssignmentRequest{
			Scope: compliance.ScopeMachine, ScopeID: "no-such", PolicyID: "floor", PolicyRevision: 1,
			ConfirmScopeID: "no-such", Reason: "x"}, OperatorCodeMachineNotFound},
		"不是 channel 的字": {OperatorComplianceAssignmentRequest{
			Scope: compliance.ScopeChannel, ScopeID: "prod", PolicyID: "floor", PolicyRevision: 1,
			ConfirmScopeID: "prod", Reason: "x"}, OperatorCodeBadChannel},
		"任意 scope": {OperatorComplianceAssignmentRequest{
			Scope: compliance.Scope("tag"), ScopeID: "gpu", PolicyID: "floor", PolicyRevision: 1,
			ConfirmScopeID: "gpu", Reason: "x"}, OperatorCodeComplianceScopeInvalid},
		"確認欄位打錯": {OperatorComplianceAssignmentRequest{
			Scope: compliance.ScopeMachine, ScopeID: machine, PolicyID: "floor", PolicyRevision: 1,
			ConfirmScopeID: "samplehub1", Reason: "x"}, OperatorCodeConfirmationMismatch},
		"沒有理由": {OperatorComplianceAssignmentRequest{
			Scope: compliance.ScopeMachine, ScopeID: machine, PolicyID: "floor", PolicyRevision: 1,
			ConfirmScopeID: machine, Reason: " "}, OperatorCodeReasonRequired},
	} {
		req := tc.req
		req.PreviewDigest = ComplianceAssignmentPreviewDigest(req.Scope, req.ScopeID, req.PolicyID, req.PolicyRevision)
		req.IdempotencyKey = "assign-reject-" + name
		req.RequestDigest = "sha256:" + strings.Repeat("f", 64)
		req.AssignedBy, req.Audit = "tester", AuditEntry{SourceAddr: "test"}
		_, err := s.ApplyOperatorComplianceAssignment(req)
		var rejection *OperatorRequestError
		if !errors.As(err, &rejection) {
			t.Errorf("%s：拿到 %v，應該是 operator rejection", name, err)
			continue
		}
		if rejection.Code != tc.want {
			t.Errorf("%s：code = %q，want %q（%s）", name, rejection.Code, tc.want, rejection.Detail)
		}
	}
	if rows, _ := s.CurrentComplianceAssignments(); len(rows) != 2 {
		t.Fatalf("被拒絕的指派改了目前生效的列數：%+v", rows)
	}
}

// 沒有指派合規性原則的機器不是「合規」，是「沒有人替它訂過條件」。把它算成
// 合規會讓一個空的機隊看起來全綠。
func TestAnUnjudgedMachineIsNotCalledCompliant(t *testing.T) {
	s := newTestStore(t)
	now := time.Date(2026, 9, 12, 8, 0, 0, 0, time.UTC)
	id := mustEnroll(t, s, "samplehub1", now)
	complianceCheckin(t, s, id, checkinFacts{version: "test", free: 90, total: 100,
		jobs: true, digest: settingpolicy.MustDigest(settingpolicy.Defaults())}, now)

	states, err := s.MachineComplianceStates()
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 1 || states[0].Verdict != compliance.VerdictNotEvaluated {
		t.Fatalf("沒有指派的機器判成 %+v", states)
	}
	if len(states[0].Results) != 0 {
		t.Fatalf("沒有規則卻產生了判決細項：%+v", states[0].Results)
	}
}

func TestMachineComplianceStatesJudgeOnlyFromReportedFacts(t *testing.T) {
	s := newTestStore(t)
	now := time.Date(2026, 9, 12, 8, 0, 0, 0, time.UTC)
	s.nowFn = func() time.Time { return now }

	quiet := mustEnroll(t, s, "quiet", now)
	fresh := mustEnroll(t, s, "fresh", now)
	stale := mustEnroll(t, s, "stale", now)
	full := mustEnroll(t, s, "full-disk", now)
	silentDisk := mustEnroll(t, s, "no-disk", now)

	defaults := settingpolicy.MustDigest(settingpolicy.Defaults())
	policy := rules(freshnessRule(600),
		compliance.Rule{Kind: compliance.RuleDiskFreeMinPercent, MinFreePercent: 20},
		compliance.Rule{Kind: compliance.RuleSettingsApplied})
	publishCompliance(t, s, "floor", 0, policy, "p1")
	for _, id := range []string{quiet, fresh, stale, full, silentDisk} {
		assignCompliance(t, s, compliance.ScopeMachine, id, "floor", 1, "a-"+id)
	}

	complianceCheckin(t, s, fresh, checkinFacts{version: "test", free: 90, total: 100,
		jobs: true, digest: defaults}, now.Add(-time.Minute))
	complianceCheckin(t, s, stale, checkinFacts{version: "test", free: 90, total: 100,
		jobs: true, digest: defaults}, now.Add(-2*time.Hour))
	complianceCheckin(t, s, full, checkinFacts{version: "test", free: 2, total: 100,
		jobs: true, digest: defaults}, now.Add(-time.Minute))
	complianceCheckin(t, s, silentDisk, checkinFacts{version: "test",
		jobs: true, digest: defaults}, now.Add(-time.Minute))

	states, err := s.MachineComplianceStates()
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]MachineComplianceState{}
	for _, st := range states {
		byID[st.MachineID] = st
	}
	for id, want := range map[string]compliance.Verdict{
		quiet:      compliance.VerdictNeverReported,
		fresh:      compliance.VerdictCompliant,
		stale:      compliance.VerdictNoncompliant,
		full:       compliance.VerdictNoncompliant,
		silentDisk: compliance.VerdictUnmeasured,
	} {
		if got := byID[id].Verdict; got != want {
			t.Errorf("%s 的判決 = %q，want %q（%+v）", byID[id].DisplayName, got, want, byID[id].Results)
		}
	}

	// 判決要說得出是哪一條規則，不能只給一個顏色。
	for _, r := range byID[full].Results {
		if r.Kind == compliance.RuleDiskFreeMinPercent {
			if r.Outcome != compliance.OutcomeFail || !strings.Contains(r.Detail, "2%") {
				t.Fatalf("磁碟那條沒有說出量到什麼：%+v", r)
			}
		}
	}
	// 每一條規則都要有一列，包含通過的那些，否則看板上沒過的那條會看起來像
	// 唯一被檢查過的東西。
	if len(byID[fresh].Results) != 3 {
		t.Fatalf("通過的機器只列了 %d 條規則", len(byID[fresh].Results))
	}

	// ⚠ 判決是現算的。時間往前走，同一列合法地換判決 —— 這正是判決不能寫進
	// 資料表的理由。
	s.nowFn = func() time.Time { return now.Add(time.Hour) }
	later, err := s.MachineComplianceStates()
	if err != nil {
		t.Fatal(err)
	}
	for _, st := range later {
		if st.MachineID == fresh && st.Verdict != compliance.VerdictNoncompliant {
			t.Fatalf("一小時後沒再報到的機器還是 %q", st.Verdict)
		}
	}
}

// 設定那條規則吃的是設定平面自己的判決，不是它自己重新定義一次「已套用」。
func TestSettingsAppliedRuleReusesTheSettingsPlaneVerdict(t *testing.T) {
	s := newTestStore(t)
	now := time.Date(2026, 9, 12, 8, 0, 0, 0, time.UTC)
	s.nowFn = func() time.Time { return now }

	applied := mustEnroll(t, s, "applied", now)
	behind := mustEnroll(t, s, "behind", now)
	silent := mustEnroll(t, s, "silent", now)
	rogue := mustEnroll(t, s, "rogue", now)

	publishSetting(t, s, "pace", 0, settingsAt(120, 600), "s1")
	v1 := settingpolicy.MustDigest(settingsAt(120, 600))
	publishSetting(t, s, "pace", 1, settingsAt(300, 900), "s2")
	v2 := settingpolicy.MustDigest(settingsAt(300, 900))

	publishCompliance(t, s, "floor", 0, rules(compliance.Rule{Kind: compliance.RuleSettingsApplied}), "p1")
	for _, id := range []string{applied, behind, silent, rogue} {
		assignSetting(t, s, settingpolicy.ScopeMachine, id, "pace", 2, "sa-"+id)
		assignCompliance(t, s, compliance.ScopeMachine, id, "floor", 1, "ca-"+id)
	}
	complianceCheckin(t, s, applied, checkinFacts{version: "test", digest: v2}, now)
	complianceCheckin(t, s, behind, checkinFacts{version: "test", digest: v1}, now)
	complianceCheckin(t, s, silent, checkinFacts{version: "test"}, now)
	complianceCheckin(t, s, rogue, checkinFacts{version: "test",
		digest: "sha256:" + strings.Repeat("9", 64)}, now)

	states, err := s.MachineComplianceStates()
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]MachineComplianceState{}
	for _, st := range states {
		byID[st.MachineID] = st
	}
	for id, want := range map[string]compliance.Verdict{
		applied: compliance.VerdictCompliant,
		// 落後一版與還沒回報都不是「不符合」：它們是還沒到，不是錯了。
		behind: compliance.VerdictUnmeasured,
		silent: compliance.VerdictUnmeasured,
		// 在跑這個 Hub 從來沒送出去過的設定，那是確定的不符合。
		rogue: compliance.VerdictNoncompliant,
	} {
		if got := byID[id].Verdict; got != want {
			t.Errorf("%s 的判決 = %q，want %q（%+v）", byID[id].DisplayName, got, want, byID[id].Results)
		}
	}
}

func TestResolveMachineCompliancePrefersMachineOverChannel(t *testing.T) {
	s := newTestStore(t)
	now := time.Date(2026, 9, 12, 8, 0, 0, 0, time.UTC)
	s.nowFn = func() time.Time { return now }
	a := mustEnroll(t, s, "samplehub1", now)
	b := mustEnroll(t, s, "sampleagent2", now)
	for _, id := range []string{a, b} {
		setChannel(t, s, id, "canary")
		complianceCheckin(t, s, id, checkinFacts{version: "test", free: 90, total: 100}, now)
	}

	if _, err := s.ResolveMachineCompliance("never-enrolled"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("不認識的機器應該是 not found，拿到 %v", err)
	}
	got, err := s.ResolveMachineCompliance(a)
	if err != nil || got.Effective.Assigned() {
		t.Fatalf("沒有指派時不該解析出原則：%+v %v", got, err)
	}

	publishCompliance(t, s, "loose", 0, rules(freshnessRule(604800)), "p1")
	publishCompliance(t, s, "tight", 0, rules(freshnessRule(60)), "p2")
	assignCompliance(t, s, compliance.ScopeChannel, "canary", "loose", 1, "a1")
	assignCompliance(t, s, compliance.ScopeMachine, a, "tight", 1, "a2")

	got, err = s.ResolveMachineCompliance(a)
	if err != nil || got.Effective.Source != compliance.SourceMachine || got.Effective.PolicyID != "tight" {
		t.Fatalf("機器自己的指派沒有蓋過 channel：%+v %v", got, err)
	}
	other, err := s.ResolveMachineCompliance(b)
	if err != nil || other.Effective.Source != compliance.SourceChannel {
		t.Fatalf("指派一台影響到了同 channel 的另一台：%+v %v", other, err)
	}
}

type checkinFacts struct {
	version     string
	free, total int64
	jobs        bool
	digest      string
}

func complianceCheckin(t *testing.T, s *Store, machineID string, f checkinFacts, at time.Time) {
	t.Helper()
	c := model.Checkin{SchemaVersion: model.SchemaVersion, SentAt: at,
		AgentVersion: f.version, DiskFreeBytes: f.free, DiskTotalBytes: f.total,
		SettingsDigest: f.digest}
	if f.jobs {
		enabled := true
		c.JobsEnabled = &enabled
	}
	if err := s.RecordCheckin(machineID, c, at); err != nil {
		t.Fatalf("record checkin: %v", err)
	}
}
