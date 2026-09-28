package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/model"
	"github.com/teddashh/AI-Intune/internal/operatorauth"
)

func complianceCLIFixture(t *testing.T) (jobsFixture, machineCommandDeps) {
	t.Helper()
	f := settingAPIFixture(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mux.ServeHTTP(w, verifiedOperatorRequest(r, operatorauth.Admin))
	}))
	t.Cleanup(server.Close)
	base, deps := machineHTTPTestDeps(t, server)
	deps.discoverHubURL = func() (string, error) { return base, nil }
	return f, deps
}

func runComplianceCLI(t *testing.T, deps machineCommandDeps, args ...string) string {
	t.Helper()
	var out, errOut bytes.Buffer
	if err := runComplianceCommandWithDeps(t.Context(), args, &out, &errOut, deps); err != nil {
		t.Fatalf("compliance %v: %v; stderr=%s", args, err, errOut.String())
	}
	return out.String()
}

func TestComplianceCLIPublishesAssignsAndPrintsTheEvidence(t *testing.T) {
	f, deps := complianceCLIFixture(t)
	const reason = "PRIVATE_COMPLIANCE_CLI_REASON"

	empty := runComplianceCLI(t, deps, "list")
	if !strings.Contains(empty, "判決時間：") ||
		!strings.Contains(empty, "（尚未發佈任何合規性原則）") ||
		!strings.Contains(empty, "未指派合規性原則") {
		t.Fatalf("空盤面=%q", empty)
	}

	preview := runComplianceCLI(t, deps, "publish", "--policy", "fleet-floor",
		"--checkin-max-age", "15m", "--settings-applied", "--preview")
	if !strings.Contains(preview, "revision 0 → 1") ||
		!strings.Contains(preview, "checkin_max_age(最久 900 秒)") ||
		!strings.Contains(preview, "影響 0 台") || !strings.Contains(preview, "去掉 --preview") {
		t.Fatalf("發佈預覽=%q", preview)
	}

	published := runComplianceCLI(t, deps, "publish", "--policy", "fleet-floor",
		"--checkin-max-age", "15m", "--settings-applied", "--reason", reason)
	if !strings.Contains(published, "fleet-floor revision 1 已發佈") ||
		!strings.Contains(published, "compliance assign --scope machine") ||
		strings.Contains(published, reason) {
		t.Fatalf("發佈輸出=%q", published)
	}

	// 同一組規則換個順序寫還是同一份規則，不會產生新 revision。
	same := runComplianceCLI(t, deps, "publish", "--policy", "fleet-floor",
		"--settings-applied", "--checkin-max-age", "15m", "--reason", reason)
	if !strings.Contains(same, "仍是 revision 1") {
		t.Fatalf("重複發佈輸出=%q", same)
	}

	assignPreview := runComplianceCLI(t, deps, "assign", "--scope", "machine",
		"--scope-id", f.machine.id, "--policy", "fleet-floor", "--revision", "1", "--preview")
	if !strings.Contains(assignPreview, "尚未指派（不評估）") ||
		!strings.Contains(assignPreview, "fleet-floor@1") ||
		!strings.Contains(assignPreview, "影響 1 台") {
		t.Fatalf("指派預覽=%q", assignPreview)
	}

	assigned := runComplianceCLI(t, deps, "assign", "--scope", "machine",
		"--scope-id", f.machine.id, "--policy", "fleet-floor", "--revision", "1", "--reason", reason)
	if !strings.Contains(assigned, "已指派 fleet-floor@1") ||
		!strings.Contains(assigned, "compliance list") || strings.Contains(assigned, reason) {
		t.Fatalf("指派輸出=%q", assigned)
	}

	// 指派完、機器還沒報到：盤面說從未報到，不說不符合。
	silent := runComplianceCLI(t, deps, "list")
	if !strings.Contains(silent, "機器從未報到") || !strings.Contains(silent, "fleet-floor@1") {
		t.Fatalf("尚未報到的盤面=%q", silent)
	}

	// 新鮮度是對 Hub 的鐘量的，所以報到時間用真的現在。
	base := time.Now().UTC()
	first := settingCheckin(t, f, base, "")
	unmeasured := runComplianceCLI(t, deps, "list")
	if !strings.Contains(unmeasured, "有規則量不到") ||
		!strings.Contains(unmeasured, "checkin_max_age=pass settings_applied=unmeasured") {
		t.Fatalf("只報到、沒回報 digest 的盤面=%q", unmeasured)
	}

	settingCheckin(t, f, base.Add(time.Second), first.SettingsDigest)

	// 預覽一路走得通、只少了 --reason 的指派，必須在送出寫入之前就被擋下來。
	var noReason, noReasonErr bytes.Buffer
	err := runComplianceCommandWithDeps(t.Context(), []string{"assign", "--scope", "machine",
		"--scope-id", f.machine.id, "--policy", "fleet-floor", "--revision", "1"},
		&noReason, &noReasonErr, deps)
	if err == nil {
		t.Fatalf("沒有 --reason 的指派被接受了：%s", noReason.String())
	}
	if !strings.Contains(err.Error(), "--reason 必填") {
		t.Fatalf("沒有 --reason 的錯誤沒說清楚缺什麼：%v", err)
	}

	board := runComplianceCLI(t, deps, "list")
	if !strings.Contains(board, "符合") ||
		!strings.Contains(board, "checkin_max_age=pass settings_applied=pass") ||
		!strings.Contains(board, "checkin_max_age(最久 900 秒) settings_applied") {
		t.Fatalf("回報 digest 之後的盤面=%q", board)
	}
}

func TestComplianceCLIRejectsUnusableInput(t *testing.T) {
	f, deps := complianceCLIFixture(t)
	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"一條規則都沒指定": {[]string{"publish", "--policy", "p", "--reason", "r"},
			"至少要指定一條規則"},
		"新鮮度不是整秒": {[]string{"publish", "--policy", "p", "--checkin-max-age", "900500ms"},
			"--checkin-max-age 必須是正的整秒"},
		"scope 不認得": {[]string{"assign", "--scope", "tenant", "--scope-id", "x",
			"--policy", "p", "--revision", "1"}, "--scope 只接受 machine 或 channel"},
		"原則不存在": {[]string{"assign", "--scope", "machine", "--scope-id", f.machine.id,
			"--policy", "nobody", "--revision", "1", "--reason", "r"}, "預覽合規性指派失敗"},
		"寬限期不是整秒": {[]string{"publish", "--policy", "p", "--checkin-max-age", "900s",
			"--block-jobs-after", "1500ms"}, "--block-jobs-after 必須是 0 到 86400s 之間的整秒"},
		"寬限期超過一天": {[]string{"publish", "--policy", "p", "--checkin-max-age", "900s",
			"--block-jobs-after", "25h"}, "--block-jobs-after 必須是 0 到 86400s 之間的整秒"},
		"多餘的位置參數":         {[]string{"list", "extra"}, "不接受 positional arguments"},
		"不認得的動作":          {[]string{"rename"}, "不認得 subcommand"},
		"沒有指定 subcommand": {nil, "必須指定 list、publish 或 assign"},
	} {
		t.Run(name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			err := runComplianceCommandWithDeps(t.Context(), tc.args, &out, &errOut, deps)
			if err == nil {
				t.Fatalf("%v 被接受了：%s", tc.args, out.String())
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("%v 的錯誤是 %q，沒有說 %q", tc.args, err, tc.want)
			}
		})
	}
}

// CLI 這一面要說得出兩件事：這份原則不符合時會做什麼，以及現在對每一台做到
// 哪一步了。少了任何一件，操作員得回網頁才知道自己按下去會發生什麼。
func TestComplianceCLICarriesTheConsequenceThroughPreviewPublishAndBoard(t *testing.T) {
	f, deps := complianceCLIFixture(t)
	const reason = "PRIVATE_COMPLIANCE_ACTION_REASON"

	preview := runComplianceCLI(t, deps, "publish", "--policy", "job-floor",
		"--checkin-max-age", "15m", "--block-jobs-after", "1h", "--preview")
	for _, want := range []string{
		"動作 block_jobs(寬限 3600s)",
		"停發工作單：不再領到新的工作單，也不會被算進新的部署，連續不符合 1 小時後生效。",
	} {
		if !strings.Contains(preview, want) {
			t.Fatalf("發佈預覽沒說出後果 %q：%q", want, preview)
		}
	}

	published := runComplianceCLI(t, deps, "publish", "--policy", "job-floor",
		"--checkin-max-age", "15m", "--block-jobs-after", "1h", "--reason", reason)
	if !strings.Contains(published, "job-floor revision 1 已發佈") ||
		!strings.Contains(published, "動作 block_jobs(寬限 3600s)") {
		t.Fatalf("發佈輸出=%q", published)
	}

	assigned := runComplianceCLI(t, deps, "assign", "--scope", "machine",
		"--scope-id", f.machine.id, "--policy", "job-floor", "--revision", "1", "--reason", reason)
	if !strings.Contains(assigned, "停發工作單：") {
		t.Fatalf("指派輸出沒說出會對這台做什麼：%q", assigned)
	}

	// 從未報到不是不符合：動作欄要說未觸發。
	silent := runComplianceCLI(t, deps, "list")
	if !strings.Contains(silent, "block_jobs(寬限 3600s)") ||
		!strings.Contains(silent, "停發工作單=未觸發") {
		t.Fatalf("尚未報到的盤面=%q", silent)
	}

	// 連續不符合超過寬限期：動作欄要說生效中。
	stale := time.Now().UTC().Add(-3 * time.Hour)
	if err := f.store.RecordCheckin(f.machine.id, model.Checkin{
		SchemaVersion: model.SchemaVersion, SentAt: stale, AgentVersion: "test",
	}, stale); err != nil {
		t.Fatalf("寫入測試 check-in 失敗：%v", err)
	}
	enforced := runComplianceCLI(t, deps, "list")
	if !strings.Contains(enforced, "停發工作單=生效中") {
		t.Fatalf("超過寬限期的盤面=%q", enforced)
	}

	// 只回報的原則要說它只回報，而且機器那一列沒有動作可講。
	quiet, quietDeps := complianceCLIFixture(t)
	runComplianceCLI(t, quietDeps, "publish", "--policy", "watch-floor",
		"--checkin-max-age", "15m", "--reason", reason)
	runComplianceCLI(t, quietDeps, "assign", "--scope", "machine",
		"--scope-id", quiet.machine.id, "--policy", "watch-floor", "--revision", "1", "--reason", reason)
	board := runComplianceCLI(t, quietDeps, "list")
	if !strings.Contains(board, "只回報") || strings.Contains(board, "停發工作單") {
		t.Fatalf("只回報的盤面=%q", board)
	}
}
