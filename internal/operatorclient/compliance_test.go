package operatorclient

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/compliance"
	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/store"
)

func complianceClientServer(t *testing.T, body any) *Client {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(server.Close)
	return operatorClientForServer(t, server)
}

func complianceClientBoard(now time.Time) operator.ComplianceBoardResult {
	since := now.Add(-2 * time.Hour)
	return operator.ComplianceBoardResult{
		EvaluatedAt: now,
		Policies:    []store.CompliancePolicySummary{},
		Assignments: []store.ComplianceAssignmentRecord{},
		Counts:      map[compliance.Verdict]int{compliance.VerdictNoncompliant: 1},
		Blocked:     1,
		Machines: []operator.ComplianceBoardMachine{{
			MachineID: "m1", DisplayName: "m1", Source: compliance.SourceMachine,
			PolicyID: "floor", Revision: 1, Verdict: compliance.VerdictNoncompliant,
			VerdictLabel: compliance.Label(compliance.VerdictNoncompliant),
			ReportedAt:   &since,
			Results: []operator.ComplianceRuleResult{{
				Kind: compliance.RuleCheckinMaxAge, Label: "報到新鮮度",
				Outcome: compliance.OutcomeFail, Detail: "上次報到在 7200 秒前",
			}},
			Actions: []operator.ComplianceActionOutcome{{
				Kind: compliance.ActionBlockJobs, Label: "停發工作單",
				State: compliance.ActionStateEnforced, StateLabel: "生效中", Since: &since,
			}},
		}},
	}
}

// 用戶端自己重算一次「誰被停發」。Hub 說 3 台、列出來只有 1 台的話，那個數字
// 就不能拿來決定要不要動機隊。
func TestComplianceClientAcceptsACoherentBoardAndRefusesAMiscount(t *testing.T) {
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	board, err := complianceClientServer(t, complianceClientBoard(now)).ComplianceBoard(t.Context())
	if err != nil {
		t.Fatalf("一致的盤面被拒絕：%v", err)
	}
	if board.Blocked != 1 || len(board.Machines[0].Actions) != 1 {
		t.Fatalf("盤面=%+v", board)
	}

	miscounted := complianceClientBoard(now)
	miscounted.Blocked = 3
	if _, err := complianceClientServer(t, miscounted).ComplianceBoard(t.Context()); err == nil {
		t.Fatal("停發台數與列不符卻被接受")
	}
}

// ⚠ 這是機隊的保險絲：一台沒被判不符合的機器不可能有動作在生效。Hub 要是這樣
// 說，那份盤面就是壞的，不可以拿來解釋為什麼某台領不到工作單。
func TestComplianceClientRefusesAnActionOnAMachineThatIsNotNoncompliant(t *testing.T) {
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	board := complianceClientBoard(now)
	board.Machines[0].Verdict = compliance.VerdictNeverReported
	board.Machines[0].VerdictLabel = compliance.Label(compliance.VerdictNeverReported)
	board.Machines[0].ReportedAt = nil
	board.Machines[0].Results = nil
	board.Counts = map[compliance.Verdict]int{compliance.VerdictNeverReported: 1}
	if _, err := complianceClientServer(t, board).ComplianceBoard(t.Context()); err == nil {
		t.Fatal("從未報到的機器帶著生效中的動作卻被接受")
	}
}

// 寬限中就一定說得出什麼時候生效；已經生效就不該再講一個未來的時刻。兩者對不
// 上，畫面上的倒數就是編的。
func TestComplianceClientRefusesAGraceStateThatDoesNotMatchItsDueTime(t *testing.T) {
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	for name, mutate := range map[string]func(*operator.ComplianceBoardResult){
		"寬限中卻沒有生效時刻": func(b *operator.ComplianceBoardResult) {
			b.Machines[0].Actions[0].State = compliance.ActionStateInGrace
			b.Machines[0].Actions[0].StateLabel = "寬限中"
			b.Blocked = 0
		},
		"已生效卻還有生效時刻": func(b *operator.ComplianceBoardResult) {
			due := now.Add(time.Hour)
			b.Machines[0].Actions[0].DueAt = &due
		},
		"動作沒有說明": func(b *operator.ComplianceBoardResult) {
			b.Machines[0].Actions[0].StateLabel = ""
		},
	} {
		t.Run(name, func(t *testing.T) {
			board := complianceClientBoard(now)
			mutate(&board)
			if _, err := complianceClientServer(t, board).ComplianceBoard(t.Context()); err == nil {
				t.Fatal("被接受了")
			}
		})
	}
}

// 確認頁上那句話跟送出去的那個動作必須是同一件事：說「停發工作單」卻送出別的
// 後果，操作員按下去的就不是他讀到的東西。
func TestCompliancePreviewClientRefusesActionWordsThatDoNotMatchTheAction(t *testing.T) {
	policy := compliance.Policy{SchemaVersion: compliance.SchemaVersion,
		Rules:   []compliance.Rule{{Kind: compliance.RuleCheckinMaxAge, MaxAgeSeconds: 900}},
		Actions: []compliance.Action{{Kind: compliance.ActionBlockJobs, GraceSeconds: 3600}}}
	digest := "sha256:" + "1111111111111111111111111111111111111111111111111111111111111111"
	base := operator.CompliancePolicyPreviewResult{
		PolicyID: "floor", CurrentRev: 0, NextRev: 1, Digest: digest, PreviewDigest: digest,
		Policy: policy,
		Rules: []operator.ComplianceRule{{Kind: compliance.RuleCheckinMaxAge,
			Label: "報到新鮮度", Bound: "最久 900 秒"}},
		Actions: []operator.ComplianceAction{{Kind: compliance.ActionBlockJobs,
			Label: "停發工作單", Effect: "不再領到新的工作單", Grace: "連續不符合 1 小時後生效"}},
	}
	request := CompliancePolicyPreviewRequest{PolicyID: "floor",
		Rules: []ComplianceRule{{Kind: "checkin_max_age", MaxAgeSeconds: 900}}}
	if _, err := complianceClientServer(t, base).PreviewCompliancePolicy(t.Context(), request); err != nil {
		t.Fatalf("一致的預覽被拒絕：%v", err)
	}
	for name, mutate := range map[string]func(*operator.CompliancePolicyPreviewResult){
		"說明比動作少":  func(p *operator.CompliancePolicyPreviewResult) { p.Actions = nil },
		"說的是別的動作": func(p *operator.CompliancePolicyPreviewResult) { p.Actions[0].Kind = "wipe_device" },
		"沒說寬限期":   func(p *operator.CompliancePolicyPreviewResult) { p.Actions[0].Grace = "" },
		"沒說會失去什麼": func(p *operator.CompliancePolicyPreviewResult) { p.Actions[0].Effect = "" },
	} {
		t.Run(name, func(t *testing.T) {
			preview := base
			preview.Actions = append([]operator.ComplianceAction(nil), base.Actions...)
			mutate(&preview)
			if _, err := complianceClientServer(t, preview).PreviewCompliancePolicy(t.Context(), request); err == nil {
				t.Fatal("被接受了")
			}
		})
	}
}
