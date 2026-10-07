package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/operatorauth"
)

func settingCLIFixture(t *testing.T) (jobsFixture, machineCommandDeps) {
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

func runSettingsCLI(t *testing.T, deps machineCommandDeps, args ...string) string {
	t.Helper()
	var out, errOut bytes.Buffer
	if err := runSettingsCommandWithDeps(t.Context(), args, &out, &errOut, deps); err != nil {
		t.Fatalf("settings %v: %v; stderr=%s", args, err, errOut.String())
	}
	return out.String()
}

func TestSettingsCLIPublishesAssignsAndReadsTheBoard(t *testing.T) {
	f, deps := settingCLIFixture(t)
	const reason = "PRIVATE_SETTING_CLI_REASON"

	empty := runSettingsCLI(t, deps, "list")
	if !strings.Contains(empty, "defaults: check-in 120s, observation 600s") ||
		!strings.Contains(empty, "(no setting policies published yet)") ||
		!strings.Contains(empty, "機器從未報到") {
		t.Fatalf("空盤面=%q", empty)
	}

	preview := runSettingsCLI(t, deps, "publish", "--policy", "fast-fleet",
		"--checkin", "60s", "--observation", "5m", "--preview")
	if !strings.Contains(preview, "revision 0 → 1") || !strings.Contains(preview, "affects 0 machines") ||
		!strings.Contains(preview, "without --preview") {
		t.Fatalf("發佈預覽=%q", preview)
	}

	published := runSettingsCLI(t, deps, "publish", "--policy", "fast-fleet",
		"--checkin", "60s", "--observation", "5m", "--reason", reason)
	if !strings.Contains(published, "fast-fleet revision 1 published") ||
		!strings.Contains(published, "settings assign --scope machine") ||
		strings.Contains(published, reason) {
		t.Fatalf("發佈輸出=%q", published)
	}

	// 同一組值再發一次不會產生新 revision。
	same := runSettingsCLI(t, deps, "publish", "--policy", "fast-fleet",
		"--checkin", "60s", "--observation", "5m", "--reason", reason)
	if !strings.Contains(same, "is still revision 1") {
		t.Fatalf("重複發佈輸出=%q", same)
	}

	assignPreview := runSettingsCLI(t, deps, "assign", "--scope", "machine",
		"--scope-id", f.machine.id, "--policy", "fast-fleet", "--revision", "1", "--preview")
	if !strings.Contains(assignPreview, "unassigned (running defaults)") ||
		!strings.Contains(assignPreview, "fast-fleet@1") || !strings.Contains(assignPreview, "affects 1 machines") {
		t.Fatalf("指派預覽=%q", assignPreview)
	}

	assigned := runSettingsCLI(t, deps, "assign", "--scope", "machine",
		"--scope-id", f.machine.id, "--policy", "fast-fleet", "--revision", "1", "--reason", reason)
	if !strings.Contains(assigned, "assigned fast-fleet@1") ||
		!strings.Contains(assigned, "settings list") || strings.Contains(assigned, reason) {
		t.Fatalf("指派輸出=%q", assigned)
	}

	// 機器報到並回報 digest 之後，盤面才說已套用。
	response := settingCheckin(t, f, jobsTestNow, "")
	settingCheckin(t, f, jobsTestNow.Add(time.Minute), response.SettingsDigest)

	var noReason, noReasonErr bytes.Buffer
	if err := runSettingsCommandWithDeps(t.Context(), []string{"assign", "--scope", "machine",
		"--scope-id", f.machine.id, "--policy", "fast-fleet", "--revision", "1"},
		&noReason, &noReasonErr, deps); err == nil {
		t.Fatalf("沒有 --reason 的指派被接受了：%s", noReason.String())
	}

	board := runSettingsCLI(t, deps, "list")
	if !strings.Contains(board, "fast-fleet") || !strings.Contains(board, "已套用") ||
		!strings.Contains(board, "60s") {
		t.Fatalf("盤面=%q", board)
	}
}

func TestSettingsCLIRejectsUnusableInput(t *testing.T) {
	_, deps := settingCLIFixture(t)
	for name, args := range map[string][]string{
		"間隔不是整秒":    {"publish", "--policy", "p", "--checkin", "60500ms", "--observation", "5m"},
		"間隔沒填":      {"publish", "--policy", "p", "--observation", "5m"},
		"scope 不認得": {"assign", "--scope", "tenant", "--scope-id", "x", "--policy", "p", "--revision", "1"},
		"原則不存在":     {"assign", "--scope", "machine", "--scope-id", "x", "--policy", "p", "--revision", "1"},
		"多餘的位置參數":   {"list", "extra"},
		"不認得的動作":    {"rename"},
	} {
		t.Run(name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			if err := runSettingsCommandWithDeps(t.Context(), args, &out, &errOut, deps); err == nil {
				t.Fatalf("%v 被接受了：%s", args, out.String())
			}
		})
	}
}
