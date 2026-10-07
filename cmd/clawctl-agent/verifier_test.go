package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/model"
)

// 三段實際從 sampleagent3 量回來的 systemd 輸出。它們不是憑空編的格式：
// property 的順序就是 systemd 給的順序（NRestarts 在最前面）。
const (
	measuredUnitRunning  = "NRestarts=0\nLoadState=loaded\nActiveState=active\nSubState=running\n"
	measuredUnitNotFound = "NRestarts=0\nLoadState=not-found\nActiveState=inactive\nSubState=dead\n"
)

type scriptedSSH struct {
	mu      sync.Mutex
	replies map[string]sshResult
	calls   []string
	rules   map[string]int
}

func (s *scriptedSSH) run(_ context.Context, destination, command string) sshResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, destination)
	for ruleID, reply := range s.replies {
		if strings.Contains(command, ruleCommandMarker(ruleID)) {
			if s.rules == nil {
				s.rules = make(map[string]int)
			}
			s.rules[ruleID]++
			return reply
		}
	}
	panic("測試腳本沒有涵蓋這條指令：" + command)
}

// ruleCommandMarker 把規則 ID 換成它的指令裡一定會出現的字串，
// 讓測試的腳本按規則對應，而不是按呼叫順序。
func ruleCommandMarker(ruleID string) string {
	switch ruleID {
	case "openclaw.current_release":
		return "openclaw/current"
	case "openclaw.gateway_http":
		return "18789"
	case "openclaw.unit_state":
		return "systemctl --user show"
	}
	panic("未知規則：" + ruleID)
}

func healthySSH() *scriptedSSH {
	return &scriptedSSH{replies: map[string]sshResult{
		"openclaw.current_release": {Stdout: "current=releases/2026.6.10\n"},
		"openclaw.gateway_http":    {Stdout: "http_code=200\n"},
		"openclaw.unit_state":      {Stdout: measuredUnitRunning},
	}}
}

type recordedHub struct {
	mu          sync.Mutex
	assignments []model.VerificationAssignment
	posted      []model.IndependentVerificationRequest
	auth        []string
}

func (h *recordedHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.auth = append(h.auth, r.Header.Get("Authorization"))
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/v1/verification-assignments":
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(model.VerificationAssignmentsResponse{
			SchemaVersion: model.SchemaVersion, Assignments: h.assignments,
		})
	case r.Method == http.MethodPost && r.URL.Path == "/v1/verifications":
		var req model.IndependentVerificationRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		h.posted = append(h.posted, req)
		w.WriteHeader(http.StatusCreated)
	default:
		http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusInternalServerError)
	}
}

func assignment(jobID, machineID, name string) model.VerificationAssignment {
	return model.VerificationAssignment{
		AssignmentID: "as-" + jobID, JobID: jobID, MachineID: machineID,
		MachineName: name, AssignedAt: time.Date(2026, 9, 12, 8, 0, 0, 0, time.UTC),
	}
}

func newVerifierRun(t *testing.T, hub *recordedHub, ssh sshRunFunc,
	targets map[string]string, out *strings.Builder,
) verifierRun {
	t.Helper()
	server := newHubServer(t, hub)
	return verifierRun{
		hubURL: server.URL, token: "verifier-token", targets: targets,
		targetsPath: "/tmp/targets.json", ssh: ssh,
		now:  func() time.Time { return time.Date(2026, 9, 12, 8, 30, 0, 0, time.UTC) },
		wait: func(context.Context, time.Duration) bool { return true },
		out:  out,
	}
}

func TestVerifierFleetPeerRuleSetIsTheGateContract(t *testing.T) {
	if model.IndependentRuleOpenClawCurrentRelease != "openclaw.current_release" ||
		model.IndependentRuleOpenClawGatewayHTTP != "openclaw.gateway_http" ||
		model.IndependentRuleOpenClawUnitState != "openclaw.unit_state" {
		t.Fatalf("gate rule identities changed: %q %q %q",
			model.IndependentRuleOpenClawCurrentRelease,
			model.IndependentRuleOpenClawGatewayHTTP,
			model.IndependentRuleOpenClawUnitState)
	}
	if len(verifierRules) != model.IndependentFleetPeerRequiredRules {
		t.Fatalf("verifier 有 %d 條規則，gate contract 要 %d 條", len(verifierRules), model.IndependentFleetPeerRequiredRules)
	}
	seen := make(map[string]bool, len(verifierRules))
	for _, rule := range verifierRules {
		if seen[rule.ID] {
			t.Fatalf("規則 %s 出現兩次", rule.ID)
		}
		seen[rule.ID] = true
	}
	for _, required := range []string{
		model.IndependentRuleOpenClawCurrentRelease,
		model.IndependentRuleOpenClawGatewayHTTP,
		model.IndependentRuleOpenClawUnitState,
	} {
		if !seen[required] {
			t.Errorf("verifier 少了 gate 規則 %s", required)
		}
	}
}

// 一張單量得到就三條都送，而且送的是 verifier bearer、不是 machine bearer。
func TestVerifierSendsOneRowPerRuleWhenTheTargetAnswers(t *testing.T) {
	hub := &recordedHub{assignments: []model.VerificationAssignment{
		assignment("job-1", "m-samplehub1", "samplehub1"),
	}}
	ssh := healthySSH()
	var out strings.Builder
	run := newVerifierRun(t, hub, ssh.run, map[string]string{"m-samplehub1": "example-user@100.64.200.2"}, &out)

	if err := run.once(context.Background()); err != nil {
		t.Fatalf("once() = %v，三條規則都量到就不該有錯", err)
	}
	if len(hub.posted) != len(verifierRules) {
		t.Fatalf("送出 %d 列，該是 %d 列", len(hub.posted), len(verifierRules))
	}
	for _, row := range hub.posted {
		if !row.Passed {
			t.Errorf("%s passed=false；三條都量到健康的機器", row.RuleID)
		}
		if row.ObservedDigest != "" {
			t.Errorf("%s observed_digest=%q —— 裝好的目錄不是 artifact，"+
				"填任何值都會讓 Hub 判出假的 digest_mismatch", row.RuleID, row.ObservedDigest)
		}
		if row.JobID != "job-1" || row.SchemaVersion != model.IndependentVerificationSchemaVersion {
			t.Errorf("%s 綁錯單或版本：job=%s schema=%d", row.RuleID, row.JobID, row.SchemaVersion)
		}
		if row.RuleID == model.IndependentRuleOpenClawCurrentRelease {
			if row.ObservedVersion != "2026.6.10" {
				t.Errorf("current release observed_version=%q", row.ObservedVersion)
			}
		} else if row.ObservedVersion != "" {
			t.Errorf("%s 不該帶 observed_version=%q", row.RuleID, row.ObservedVersion)
		}
		if !strings.HasPrefix(row.Command, "ssh ") || !strings.Contains(row.Command, "example-user@100.64.200.2") {
			t.Errorf("%s 的 command 沒有記下它實際怎麼量的：%q", row.RuleID, row.Command)
		}
	}
	for _, auth := range hub.auth {
		if auth != "Bearer verifier-token" {
			t.Fatalf("Hub 收到的憑證是 %q；verifier 平面只用 verifier bearer", auth)
		}
	}
	if got := out.String(); !strings.Contains(got, "Submitted 3 rows of evidence") {
		t.Errorf("輸出沒說送了幾列：\n%s", got)
	}
}

// gateway 不答是一個結論明確的觀測，要變成 passed=false 的證據，
// 不是「量不到」。這正是第二雙眼睛存在的理由：executor 說裝好了，
// 但那個 port 上沒有東西在答。
func TestVerifierReportsASilentGatewayAsFailedEvidence(t *testing.T) {
	hub := &recordedHub{assignments: []model.VerificationAssignment{
		assignment("job-2", "m-sampleagent4", "sampleagent4"),
	}}
	ssh := healthySSH()
	ssh.replies["openclaw.gateway_http"] = sshResult{Stdout: "http_code=000\n"}
	var out strings.Builder
	run := newVerifierRun(t, hub, ssh.run, map[string]string{"m-sampleagent4": "example-user-b@100.64.200.5"}, &out)

	if err := run.once(context.Background()); err != nil {
		t.Fatalf("once() = %v；量到壞消息仍然是量到了", err)
	}
	if len(hub.posted) != len(verifierRules) {
		t.Fatalf("送出 %d 列，該是 %d 列", len(hub.posted), len(verifierRules))
	}
	var failed int
	for _, row := range hub.posted {
		if row.RuleID == "openclaw.gateway_http" {
			if row.Passed {
				t.Error("gateway 沒答卻送出 passed=true")
			}
			failed++
		}
	}
	if failed != 1 {
		t.Fatalf("gateway 規則出現 %d 次", failed)
	}
	wantCalls := 1 + int(verifierGatewaySettleTimeout/verifierGatewayRetryInterval)
	if ssh.rules[model.IndependentRuleOpenClawGatewayHTTP] != wantCalls ||
		!strings.Contains(out.String(), "gateway settle window: adopting measurement after 30s") {
		t.Fatalf("gateway calls=%d want=%d output=%s",
			ssh.rules[model.IndependentRuleOpenClawGatewayHTTP], wantCalls, out.String())
	}
}

func TestVerifierLetsAnActiveGatewaySettleBeforeReporting(t *testing.T) {
	hub := &recordedHub{assignments: []model.VerificationAssignment{
		assignment("job-settle", "m-sampleagent4", "sampleagent4"),
	}}
	healthy := healthySSH()
	var gatewayCalls int
	ssh := func(ctx context.Context, destination, command string) sshResult {
		if strings.Contains(command, ruleCommandMarker(model.IndependentRuleOpenClawGatewayHTTP)) {
			gatewayCalls++
			if gatewayCalls < 4 {
				return sshResult{Stdout: "http_code=000\n"}
			}
		}
		return healthy.run(ctx, destination, command)
	}
	var waited []time.Duration
	var out strings.Builder
	run := newVerifierRun(t, hub, ssh, map[string]string{"m-sampleagent4": "example-user-b@sampleagent4"}, &out)
	run.wait = func(_ context.Context, duration time.Duration) bool {
		waited = append(waited, duration)
		return true
	}

	if err := run.once(context.Background()); err != nil {
		t.Fatalf("once() = %v", err)
	}
	if gatewayCalls != 4 || len(waited) != 3 {
		t.Fatalf("gateway calls=%d waits=%v，該在 15 秒那次恢復", gatewayCalls, waited)
	}
	for _, row := range hub.posted {
		if row.RuleID == model.IndependentRuleOpenClawGatewayHTTP &&
			(!row.Passed || row.StdoutExcerpt != "http_code=200\n") {
			t.Fatalf("gateway final evidence=%+v", row)
		}
	}
	if !strings.Contains(out.String(), "gateway settle window: adopting measurement after 15s") {
		t.Fatalf("輸出沒有說明採用哪次量測：\n%s", out.String())
	}
}

func TestVerifierDoesNotSettleAnInactiveGatewayUnit(t *testing.T) {
	hub := &recordedHub{assignments: []model.VerificationAssignment{
		assignment("job-inactive", "m-sampleagent4", "sampleagent4"),
	}}
	ssh := healthySSH()
	ssh.replies[model.IndependentRuleOpenClawGatewayHTTP] = sshResult{Stdout: "http_code=000\n"}
	ssh.replies[model.IndependentRuleOpenClawUnitState] = sshResult{Stdout: measuredUnitNotFound}
	var waits int
	var out strings.Builder
	run := newVerifierRun(t, hub, ssh.run, map[string]string{"m-sampleagent4": "example-user-b@sampleagent4"}, &out)
	run.wait = func(context.Context, time.Duration) bool { waits++; return true }

	if err := run.once(context.Background()); err != nil {
		t.Fatalf("量到 inactive 是完整的壞消息：%v", err)
	}
	if waits != 0 || len(ssh.calls) != len(verifierRules) {
		t.Fatalf("inactive unit 仍被等待：waits=%d calls=%d", waits, len(ssh.calls))
	}
	var failed int
	for _, row := range hub.posted {
		if !row.Passed {
			failed++
		}
	}
	if failed != 2 {
		t.Fatalf("inactive gateway 應有 HTTP 與 unit 兩條失敗，得到 %d：%+v", failed, hub.posted)
	}
}

func TestVerifierSendsNothingWhenGatewayBecomesUnobservableDuringSettle(t *testing.T) {
	hub := &recordedHub{assignments: []model.VerificationAssignment{
		assignment("job-settle-transport", "m-sampleagent4", "sampleagent4"),
	}}
	healthy := healthySSH()
	var gatewayCalls int
	ssh := func(ctx context.Context, destination, command string) sshResult {
		if strings.Contains(command, ruleCommandMarker(model.IndependentRuleOpenClawGatewayHTTP)) {
			gatewayCalls++
			if gatewayCalls == 1 {
				return sshResult{Stdout: "http_code=000\n"}
			}
			return sshResult{Transport: true, ExitCode: verifierSSHFailureExit, Stderr: "ssh unavailable"}
		}
		return healthy.run(ctx, destination, command)
	}
	var out strings.Builder
	run := newVerifierRun(t, hub, ssh, map[string]string{"m-sampleagent4": "example-user-b@sampleagent4"}, &out)
	if err := run.once(context.Background()); err == nil {
		t.Fatal("settle 期間量不到卻回 nil")
	}
	if len(hub.posted) != 0 || gatewayCalls != 2 || !strings.Contains(out.String(), "ssh cannot reach machine") {
		t.Fatalf("posted=%+v calls=%d output=%s", hub.posted, gatewayCalls, out.String())
	}
}

// ⚠ 這支釘的是「自證不算數」：body 說自己 ok，status 卻是 503。
// 判讀只看 status，因為「答得出 200」是觀測，「說自己好」是說法。
func TestGatewayJudgementIgnoresWhatTheGatewaySaysAboutItself(t *testing.T) {
	got := judgeGatewayHTTP(sshResult{Stdout: "http_code=503\n"})
	if !got.Observed || got.Passed {
		t.Fatalf("judgeGatewayHTTP(503) = %+v，該是量到了但沒通過", got)
	}
	if ok := judgeGatewayHTTP(sshResult{Stdout: "http_code=200\n"}); !ok.Passed {
		t.Fatalf("judgeGatewayHTTP(200) = %+v", ok)
	}
}

// 少一條規則就整張不送。一張只送了一半的派工會從「等它回報」消失，
// 看起來像做完了——那比沒有證據更糟。
func TestVerifierSendsNothingWhenOneRuleCannotBeObserved(t *testing.T) {
	hub := &recordedHub{assignments: []model.VerificationAssignment{
		assignment("job-3", "m-sampleagent2", "sampleagent2"),
	}}
	ssh := healthySSH()
	ssh.replies["openclaw.unit_state"] = sshResult{
		Stderr:   "ssh: connect to host 100.0.0.1 port 22: Connection timed out",
		ExitCode: verifierSSHFailureExit, Transport: true,
	}
	var out strings.Builder
	run := newVerifierRun(t, hub, ssh.run, map[string]string{"m-sampleagent2": "ubuntu@sampleagent2"}, &out)

	err := run.once(context.Background())
	if err == nil {
		t.Fatal("once() = nil；有規則量不到就該非零退出")
	}
	if len(hub.posted) != 0 {
		t.Fatalf("送出了 %d 列；整張單應該一列都不送", len(hub.posted))
	}
	text := out.String()
	if !strings.Contains(text, "unobserved") || !strings.Contains(text, "ssh cannot reach machine") {
		t.Errorf("輸出沒說清楚卡在哪：\n%s", text)
	}
	if !strings.Contains(err.Error(), "pending report") {
		t.Errorf("錯誤訊息 = %q；要說明那張派工現在的狀態", err)
	}
}

// 缺工具是量不到，不是壞掉。一台沒裝 curl 的機器不代表 gateway 死了。
func TestVerifierTreatsAMissingToolAsUnobservedRatherThanFailed(t *testing.T) {
	hub := &recordedHub{assignments: []model.VerificationAssignment{
		assignment("job-4", "m-sampleagent2", "sampleagent2"),
	}}
	ssh := healthySSH()
	ssh.replies["openclaw.gateway_http"] = sshResult{ExitCode: verifierToolMissingExit}
	var out strings.Builder
	run := newVerifierRun(t, hub, ssh.run, map[string]string{"m-sampleagent2": "ubuntu@sampleagent2"}, &out)

	if err := run.once(context.Background()); err == nil {
		t.Fatal("once() = nil；缺工具要讓這張單留在等它回報")
	}
	if len(hub.posted) != 0 {
		t.Fatalf("送出了 %d 列", len(hub.posted))
	}
	if !strings.Contains(out.String(), "machine is missing command required by this rule") {
		t.Errorf("輸出沒說是缺工具：\n%s", out.String())
	}
}

// 指派到一台這個 verifier 不知道怎麼走到的機器：不猜、不亂連，
// 直接說出要改哪個檔案。
func TestVerifierRefusesAMachineItWasNotToldHowToReach(t *testing.T) {
	hub := &recordedHub{assignments: []model.VerificationAssignment{
		assignment("job-5", "m-unknown", "somewhere"),
	}}
	ssh := healthySSH()
	var out strings.Builder
	run := newVerifierRun(t, hub, ssh.run, map[string]string{"m-samplehub1": "example-user@samplehub1"}, &out)

	if err := run.once(context.Background()); err == nil {
		t.Fatal("once() = nil；不知道怎麼走到就不能算成功")
	}
	if len(ssh.calls) != 0 {
		t.Fatalf("對不認得的機器發了 %d 次 ssh", len(ssh.calls))
	}
	if len(hub.posted) != 0 {
		t.Fatalf("送出了 %d 列", len(hub.posted))
	}
	if got := out.String(); !strings.Contains(got, "/tmp/targets.json") {
		t.Errorf("輸出沒說要改哪個檔：\n%s", got)
	}
}

func TestVerifierDryRunMeasuresAndSendsNothing(t *testing.T) {
	hub := &recordedHub{assignments: []model.VerificationAssignment{
		assignment("job-6", "m-samplehub1", "samplehub1"),
	}}
	ssh := healthySSH()
	var out strings.Builder
	run := newVerifierRun(t, hub, ssh.run, map[string]string{"m-samplehub1": "example-user@samplehub1"}, &out)
	run.dryRun = true

	if err := run.once(context.Background()); err != nil {
		t.Fatalf("once() = %v", err)
	}
	if len(ssh.calls) != len(verifierRules) {
		t.Fatalf("--dry-run 量了 %d 次，該量 %d 次", len(ssh.calls), len(verifierRules))
	}
	if len(hub.posted) != 0 {
		t.Fatalf("--dry-run 送出了 %d 列", len(hub.posted))
	}
	if !strings.Contains(out.String(), "--dry-run") {
		t.Errorf("輸出沒標明這是演練：\n%s", out.String())
	}
}

func TestVerifierSaysSoWhenNothingWasAssigned(t *testing.T) {
	hub := &recordedHub{}
	ssh := healthySSH()
	var out strings.Builder
	run := newVerifierRun(t, hub, ssh.run, map[string]string{"m-samplehub1": "example-user@samplehub1"}, &out)

	if err := run.once(context.Background()); err != nil {
		t.Fatalf("once() = %v；沒有派工是正常狀態", err)
	}
	if len(ssh.calls) != 0 {
		t.Fatalf("沒有派工卻連了 %d 次 ssh", len(ssh.calls))
	}
	if !strings.Contains(out.String(), "No jobs assigned to this verifier") {
		t.Errorf("輸出 = %q", out.String())
	}
}

func TestUnitStateOnlyPassesForActiveRunning(t *testing.T) {
	for _, test := range []struct {
		name     string
		res      sshResult
		observed bool
		passed   bool
	}{
		{"實測：跑著的 unit", sshResult{Stdout: measuredUnitRunning}, true, true},
		{"實測：unit 不存在", sshResult{Stdout: measuredUnitNotFound}, true, false},
		{"載入了但沒在跑", sshResult{Stdout: "LoadState=loaded\nActiveState=failed\nSubState=failed\n"}, true, false},
		{"active 但 SubState 不是 running", sshResult{Stdout: "LoadState=loaded\nActiveState=active\nSubState=exited\n"}, true, false},
		{"少了欄位", sshResult{Stdout: "ActiveState=active\n"}, false, false},
		{"同一個 key 兩個值", sshResult{Stdout: "LoadState=loaded\nActiveState=active\nActiveState=failed\nSubState=running\n"}, false, false},
		{"連不到 bus", sshResult{Stderr: "Failed to connect to bus", ExitCode: 1}, false, false},
		{"缺 systemctl", sshResult{ExitCode: verifierToolMissingExit}, false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := judgeUnitState(test.res)
			if got.Observed != test.observed || got.Passed != test.passed {
				t.Fatalf("judgeUnitState = %+v，該是 observed=%v passed=%v",
					got, test.observed, test.passed)
			}
		})
	}
}

func TestCurrentReleaseJudgementReadsTheSymlinkAndNothingElse(t *testing.T) {
	for _, test := range []struct {
		name     string
		stdout   string
		observed bool
		passed   bool
		version  string
	}{
		{"指到 release", "current=releases/2026.6.10\n", true, true, "2026.6.10"},
		{"沒有 current", "current=\n", true, false, ""},
		{"指到 releases 外面", "current=/opt/openclaw\n", true, false, ""},
		{"指到 releases 更深處", "current=releases/2026.6.10/lib\n", true, false, ""},
		{"輸出不是 key=value", "releases/2026.6.10\n", false, false, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := judgeCurrentRelease(sshResult{Stdout: test.stdout})
			if got.Observed != test.observed || got.Passed != test.passed ||
				got.ObservedVersion != test.version {
				t.Fatalf("judgeCurrentRelease = %+v，該是 observed=%v passed=%v version=%q",
					got, test.observed, test.passed, test.version)
			}
		})
	}
}

func TestTargetsFileRefusesAnythingItCannotSafelyHandToSSH(t *testing.T) {
	for _, test := range []struct {
		name string
		raw  string
	}{
		{"版本認不得", `{"schema_version":2,"targets":[{"machine_id":"m-1","ssh_destination":"a@b"}]}`},
		{"空清單", `{"schema_version":1,"targets":[]}`},
		{"destination 以 - 開頭會被 ssh 當成選項",
			`{"schema_version":1,"targets":[{"machine_id":"m-1","ssh_destination":"-oProxyCommand=id"}]}`},
		{"destination 有空白", `{"schema_version":1,"targets":[{"machine_id":"m-1","ssh_destination":"a@b -p 2222"}]}`},
		{"同一台出現兩次",
			`{"schema_version":1,"targets":[{"machine_id":"m-1","ssh_destination":"a@b"},{"machine_id":"m-1","ssh_destination":"c@d"}]}`},
		{"machine_id 空的", `{"schema_version":1,"targets":[{"machine_id":"","ssh_destination":"a@b"}]}`},
		{"多了認不得的欄位",
			`{"schema_version":1,"targets":[{"machine_id":"m-1","ssh_destination":"a@b","run_as_root":true}]}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got, err := parseVerifierTargets([]byte(test.raw)); err == nil {
				t.Fatalf("parseVerifierTargets 接受了它：%v", got)
			}
		})
	}
	good := `{"schema_version":1,"targets":[{"machine_id":"m-1","ssh_destination":"opc@100.64.200.7"}]}`
	targets, err := parseVerifierTargets([]byte(good))
	if err != nil || targets["m-1"] != "opc@100.64.200.7" {
		t.Fatalf("parseVerifierTargets(%s) = %v, %v", good, targets, err)
	}
}

// 規則寫在這個 binary 裡，不是 Hub 發的。這支測試釘住那件事的可觀察後果：
// Hub 的派工回應裡沒有任何欄位可以變成指令。
func TestRulesAreCompiledInAndNeverComeFromTheHub(t *testing.T) {
	seen := map[string]bool{}
	for _, rule := range verifierRules {
		if seen[rule.ID] {
			t.Fatalf("規則 ID %s 重複", rule.ID)
		}
		seen[rule.ID] = true
		if strings.ContainsAny(rule.Command, "\n\r") {
			t.Errorf("%s 的指令不是單行；證據裡那一行要能被貼回終端機重跑", rule.ID)
		}
		if len(rule.Command) > maxVerifierCommandBytes {
			t.Errorf("%s 的指令超過 Hub 的 command 上限", rule.ID)
		}
	}
	body, err := json.Marshal(model.VerificationAssignment{})
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"command", "rule", "digest", "script", "run"} {
		if strings.Contains(string(body), fmt.Sprintf("%q", forbidden)) {
			t.Fatalf("派工 DTO 出現 %q 欄位：verifier 一旦執行 Hub 下發的東西，"+
				"它就不是第二個判斷，而是一條用單一 bearer 控管的遠端執行管道", forbidden)
		}
	}
}

// verifier 與 verify 是兩個不同的子命令，而且都不准掉成 daemon。
func TestVerifierIsItsOwnSubcommand(t *testing.T) {
	if got := dispatch([]string{"verifier"}); got != actVerifier {
		t.Fatalf("dispatch(verifier) = %q，該是 %q", got, actVerifier)
	}
	if got := dispatch([]string{"verify"}); got != actVerify {
		t.Fatalf("dispatch(verify) = %q，該是 %q", got, actVerify)
	}
	if actVerifier == actVerify {
		t.Fatal("兩個動作不可以是同一個值")
	}
	if !strings.Contains(usage, "clawctl-agent verifier") {
		t.Error("usage 沒有列出 verifier")
	}
}

// 證據裡那一行必須能被第三個人原樣貼回終端機重跑。這支測試用真的 shell
// 把它拆回 argv，確認最後一個參數逐字等於規則的指令——引號沒有被吃掉。
func TestTheRecordedCommandSurvivesARealShell(t *testing.T) {
	for _, rule := range verifierRules {
		line := sshCommandLine("opc@100.64.200.7", rule.Command)
		out, err := exec.Command("sh", "-c",
			"set -- "+line+`; shift $(($#-1)); printf '%s' "$1"`).Output()
		if err != nil {
			t.Fatalf("%s：shell 讀不了這一行 %q：%v", rule.ID, line, err)
		}
		if string(out) != rule.Command {
			t.Errorf("%s 的指令經過引號處理後變了：\n記的  %q\n還原  %q", rule.ID, rule.Command, out)
		}
	}
}

// 一個被訊號打死的本地 ssh 從來沒有拿到遠端的狀態，所以它不可以變成
// 「這條規則沒過」。這條路在真機上是這樣被走到的：verifier unit 有
// MemoryMax／CPUQuota，被 OOM 或被 systemd 停掉的是 ssh 自己，而
// exec 對這種結束只給得出 -1——一個遠端永遠不會回的值。
//
// 測的是 runSSH 本身，所以放一支假的 ssh 在 PATH 最前面讓它真的被 exec。
func TestASignalledLocalSSHIsATransportFailureNotAFailedRule(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ssh"),
		[]byte("#!/bin/sh\nkill -TERM $$\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	res := runSSH(context.Background(), "ubuntu@example", "printf hi")
	if !res.Transport {
		t.Fatalf("被訊號中止的 ssh 沒有被當成傳輸失敗：%+v", res)
	}
	if res.ExitCode != verifierSSHFailureExit {
		t.Errorf("exit code = %d，want %d —— 負值不可以外流到判斷", res.ExitCode, verifierSSHFailureExit)
	}
	if res.Stderr == "" {
		t.Error("沒有說出卡在哪裡")
	}
	// 而判斷那一層看到 Transport 就一律是「量不到」，三條規則都一樣。
	for _, rule := range verifierRules {
		if judgement := rule.Judge(res); judgement.Observed {
			t.Errorf("規則 %s 把被訊號中止的 ssh 當成觀測到了：%+v", rule.ID, judgement)
		}
	}
}
