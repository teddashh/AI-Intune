package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/model"
)

// 一個認不得的參數，不准變成「那就跑 daemon 吧」。
//
// ⚠⚠ 這支測試釘的是一個實測到的六小時故障（2026-09-03，sampleagent2）：
// 部署腳本打了 `clawctl-agent --version`（這個 binary 的子命令是 version，
// 沒有 --version），而 main 那個 switch 沒有 default，於是它安靜地
// 啟動了第二個 agent，跑了 6 小時 22 分。
//
// 那台機器上因此同時有兩個 agent 在回報 —— 一個新版一個舊版 ——
// 同一個工具在資料庫裡在兩個答案之間跳，而**每一盞燈都是綠的**：
// 兩個 agent 都在送心跳，那台看起來比別台還健康。
func TestAnUnknownArgumentNeverStartsTheDaemon(t *testing.T) {
	// 這幾個都是人真的會打出來的東西。
	for _, arg := range []string{
		"--version", // ← 實際造成那六小時的那一個（現在改成認得了，見下面那支）
		"--wat", "-x", "probee", "enrol", "Version", "PROBE",
		"--hub", "http://example.com", "start", "daemon", "run", "-",
	} {
		if got := dispatch([]string{arg}); got == actAgent {
			t.Errorf("dispatch(%q) = %q —— 一個認不得的參數變成了 daemon。\n"+
				"這就是 sampleagent2 上那第二個 agent 的來源：一支腳本打錯一個字，\n"+
				"這個 binary 就替它啟動了一個背景程序，然後跑了六個小時。", arg, got)
		}
	}
}

func TestObservationNudgeWakesBeforeThePeriodicTimer(t *testing.T) {
	nudge := make(chan struct{}, 1)
	nudge <- struct{}{}
	if got := waitNextObservation(context.Background(), time.Hour, 0, nudge); got != observationNudged {
		t.Errorf("等待原因 = %v；工作單 nudge 已先排隊，應立刻醒來", got)
	}
}

// 只有「完全沒有參數」才是 daemon。那是 systemd 用的那一條。
func TestOnlyNoArgumentsMeansDaemon(t *testing.T) {
	if got := dispatch(nil); got != actAgent {
		t.Errorf("dispatch(nil) = %q，該是 %q —— systemd 起 agent 就是不帶參數，"+
			"這一條斷了全機隊的 agent 都起不來", got, actAgent)
	}
	if got := dispatch([]string{}); got != actAgent {
		t.Errorf("dispatch([]) = %q，該是 %q", got, actAgent)
	}
}

// 人會打的每一種版本／說明寫法都要認得 —— 讓它們變成錯誤是對的，
// 但讓它們**變成 daemon** 是災難，而讓它們直接work最好。
func TestTheObviousVersionAndHelpSpellingsAllWork(t *testing.T) {
	for _, arg := range []string{"version", "--version", "-v"} {
		if got := dispatch([]string{arg}); got != actVersion {
			t.Errorf("dispatch(%q) = %q，該是 %q —— "+
				"部署腳本就是打 --version，認不得它就是在等下一次故障", arg, got, actVersion)
		}
	}
	for _, arg := range []string{"help", "--help", "-h"} {
		if got := dispatch([]string{arg}); got != actHelp {
			t.Errorf("dispatch(%q) = %q，該是 %q", arg, got, actHelp)
		}
	}
}

func TestTheRealSubcommandsStillWork(t *testing.T) {
	if got := dispatch([]string{"enroll", "--hub", "x", "--token", "y"}); got != actEnroll {
		t.Errorf("enroll = %q", got)
	}
	if got := dispatch([]string{"probe"}); got != actProbe {
		t.Errorf("probe = %q", got)
	}
	if got := dispatch([]string{"verify"}); got != actVerify {
		t.Errorf("verify = %q", got)
	}
}

func TestReadinessRequiresThisVersionFreshCheckinAndJobs(t *testing.T) {
	since := time.Date(2026, 9, 10, 18, 0, 0, 0, time.UTC)
	enabled, disabled := true, false
	valid := model.AgentReadinessResponse{
		MachineID: "machine-1", LastCheckinReceivedAt: cliTimePtr(since.Add(time.Second)),
		AgentStartedAt: cliTimePtr(since), AgentVersion: "v1", JobsEnabled: &enabled,
		DeviceSyncV1: &enabled,
	}
	if got := readinessPending(valid, "machine-1", "v1", since, false); got != "" {
		t.Fatalf("valid readiness pending=%q", got)
	}
	for name, receipt := range map[string]model.AgentReadinessResponse{
		"wrong machine":    {MachineID: "machine-2", LastCheckinReceivedAt: valid.LastCheckinReceivedAt, AgentStartedAt: valid.AgentStartedAt, AgentVersion: "v1", JobsEnabled: &enabled},
		"missing receipt":  {MachineID: "machine-1", AgentVersion: "v1", JobsEnabled: &enabled},
		"old process":      {MachineID: "machine-1", LastCheckinReceivedAt: valid.LastCheckinReceivedAt, AgentStartedAt: cliTimePtr(since.Add(-time.Second)), AgentVersion: "v1", JobsEnabled: &enabled},
		"wrong version":    {MachineID: "machine-1", LastCheckinReceivedAt: valid.LastCheckinReceivedAt, AgentStartedAt: valid.AgentStartedAt, AgentVersion: "v0", JobsEnabled: &enabled},
		"jobs disabled":    {MachineID: "machine-1", LastCheckinReceivedAt: valid.LastCheckinReceivedAt, AgentStartedAt: valid.AgentStartedAt, AgentVersion: "v1", JobsEnabled: &disabled},
		"jobs unknown":     {MachineID: "machine-1", LastCheckinReceivedAt: valid.LastCheckinReceivedAt, AgentStartedAt: valid.AgentStartedAt, AgentVersion: "v1"},
		"sync unsupported": {MachineID: "machine-1", LastCheckinReceivedAt: valid.LastCheckinReceivedAt, AgentStartedAt: valid.AgentStartedAt, AgentVersion: "v1", JobsEnabled: &enabled, DeviceSyncV1: &disabled},
		"sync unreported":  {MachineID: "machine-1", LastCheckinReceivedAt: valid.LastCheckinReceivedAt, AgentStartedAt: valid.AgentStartedAt, AgentVersion: "v1", JobsEnabled: &enabled},
	} {
		if got := readinessPending(receipt, "machine-1", "v1", since, false); got == "" {
			t.Errorf("%s unexpectedly ready", name)
		}
	}
	clockSkewedReceipt := valid
	clockSkewedReceipt.LastCheckinReceivedAt = cliTimePtr(since.Add(-time.Hour))
	if got := readinessPending(clockSkewedReceipt, "machine-1", "v1", since, false); got != "" {
		t.Fatalf("Hub/client clock skew should not block a current process: %q", got)
	}
}

func TestReadinessCanRequireFreshMatchingPlatformEvidence(t *testing.T) {
	since := time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC)
	enabled := true
	valid := model.AgentReadinessResponse{
		MachineID: "machine-1", LastCheckinReceivedAt: cliTimePtr(since.Add(time.Second)),
		AgentStartedAt: cliTimePtr(since), AgentVersion: "v1", JobsEnabled: &enabled,
		DeviceSyncV1: &enabled, IdentityReceivedAt: cliTimePtr(since.Add(2 * time.Second)),
		IdentityMeasuredAt: cliTimePtr(since.Add(2 * time.Second)), IdentityOS: "Linux", IdentityArch: "x86_64",
	}
	if got := readinessPending(valid, "machine-1", "v1", since, true); got != "" {
		t.Fatalf("fresh matching evidence pending=%q", got)
	}

	missing := valid
	missing.IdentityReceivedAt = nil
	if got := readinessPending(missing, "machine-1", "v1", since, true); !strings.Contains(got, "identity evidence") {
		t.Fatalf("missing evidence pending=%q", got)
	}
	stale := valid
	stale.IdentityReceivedAt = cliTimePtr(since.Add(-time.Second))
	stale.IdentityMeasuredAt = cliTimePtr(since.Add(-time.Second))
	if got := readinessPending(stale, "machine-1", "v1", since, true); !strings.Contains(got, "identity evidence") {
		t.Fatalf("stale evidence pending=%q", got)
	}
	wrong := valid
	wrong.IdentityOS, wrong.IdentityArch = "macOS 15.7", "arm64"
	if got := readinessPending(wrong, "machine-1", "v1", since, true); !strings.Contains(got, "平台不符") {
		t.Fatalf("wrong platform evidence pending=%q", got)
	}
}

func TestAgentAdvertisesTheFixedDeviceSyncExecutor(t *testing.T) {
	var checkin model.Checkin
	advertiseJobCapabilities(&checkin)
	if !checkin.DeviceSyncV1 {
		t.Fatal("agent did not advertise device-sync v1")
	}
	if !checkin.MaintenanceDiskCleanV1 {
		t.Fatal("agent did not advertise maintenance disk-clean v1")
	}
}

func cliTimePtr(t time.Time) *time.Time { return &t }

func TestEnrollSecretFileRequiresPrivateRegularFile(t *testing.T) {
	path := t.TempDir() + "/token"
	if err := writePrivateFile(path, []byte("enroll-token\n")); err != nil {
		t.Fatal(err)
	}
	if got, err := readSecretFile(path); err != nil || got != "enroll-token" {
		t.Fatalf("private token got=%q err=%v", got, err)
	}
	if err := exposePrivateCredential(path); err != nil {
		t.Fatal(err)
	}
	if _, err := readSecretFile(path); err == nil {
		t.Fatal("world-readable token file was accepted")
	}
}

func TestEnrollmentEnablesManagedExecution(t *testing.T) {
	digest := testSettingsDigest(90, 600)
	cfg, err := enrollmentConfig("http://hub", model.EnrollResponse{
		SchemaVersion: model.SchemaVersion,
		MachineID:     "machine-1", AgentToken: "agent-token",
		CheckinIntervalSeconds: 90, ObservationIntervalSeconds: 600,
		SettingsDigest: digest,
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HubURL != "http://hub" || cfg.MachineID != "machine-1" || cfg.AgentToken != "agent-token" ||
		cfg.EnrollmentSchemaVersion != model.SchemaVersion ||
		cfg.EnrollmentSettingsDigest != digest ||
		!cfg.JobsEnabled || cfg.CheckinIntervalSeconds != 90 || cfg.ObservationIntervalSeconds != 600 {
		t.Fatalf("enrollment config=%+v", cfg)
	}
}

func TestStoredConfigRequiresEnrollmentAuthority(t *testing.T) {
	valid, err := enrollmentConfig("http://hub", model.EnrollResponse{
		SchemaVersion: model.SchemaVersion,
		MachineID:     "machine-1", AgentToken: "agent-token",
		CheckinIntervalSeconds: 90, ObservationIntervalSeconds: 600,
		SettingsDigest: testSettingsDigest(90, 600),
	})
	if err != nil {
		t.Fatal(err)
	}
	tests := map[string]func(*config){
		"missing schema":     func(c *config) { c.EnrollmentSchemaVersion = 0 },
		"wrong schema":       func(c *config) { c.EnrollmentSchemaVersion++ },
		"missing digest":     func(c *config) { c.EnrollmentSettingsDigest = "" },
		"malformed digest":   func(c *config) { c.EnrollmentSettingsDigest = "sha256:ABC" },
		"digest mismatch":    func(c *config) { c.CheckinIntervalSeconds++ },
		"blank token":        func(c *config) { c.AgentToken = "" },
		"noncanonical token": func(c *config) { c.AgentToken = " agent-token " },
		"malformed token":    func(c *config) { c.AgentToken = "agent\ntoken" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			isolateAgentHome(t)
			candidate := valid
			mutate(&candidate)
			if err := os.MkdirAll(filepath.Dir(configPath()), 0o700); err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(candidate)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(configPath(), raw, 0o600); err != nil {
				t.Fatal(err)
			}
			if got, err := loadConfig(); err == nil {
				t.Fatalf("invalid stored receipt enabled restart config: %+v", got)
			}
		})
	}

	t.Run("legacy deploy-enabled config", func(t *testing.T) {
		isolateAgentHome(t)
		if err := os.MkdirAll(filepath.Dir(configPath()), 0o700); err != nil {
			t.Fatal(err)
		}
		legacy := `{"hub_url":"http://hub","machine_id":"machine-1","agent_token":"agent-token",` +
			`"jobs_enabled":true,"checkin_interval_seconds":90,"observation_interval_seconds":600}`
		if err := os.WriteFile(configPath(), []byte(legacy), 0o600); err != nil {
			t.Fatal(err)
		}
		if got, err := loadConfig(); err == nil {
			t.Fatalf("legacy config enabled deploy without stored receipt: %+v", got)
		}
	})

	t.Run("valid receipt survives restart", func(t *testing.T) {
		isolateAgentHome(t)
		if err := saveConfig(valid); err != nil {
			t.Fatal(err)
		}
		if got, err := loadConfig(); err != nil || !got.JobsEnabled || got.AgentToken != valid.AgentToken {
			t.Fatalf("valid stored receipt did not survive restart: cfg=%+v err=%v", got, err)
		}
	})
}

func TestAgentConfigStaysPrivateAndDoesNotFollowSymlink(t *testing.T) {
	isolateAgentHome(t)
	valid, err := enrollmentConfig("http://hub", model.EnrollResponse{
		SchemaVersion: model.SchemaVersion,
		MachineID:     "machine-1", AgentToken: "agent-token",
		CheckinIntervalSeconds: 90, ObservationIntervalSeconds: 600,
		SettingsDigest: testSettingsDigest(90, 600),
	})
	if err != nil {
		t.Fatal(err)
	}
	p := configPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("legacy"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := saveConfig(valid); err != nil {
		t.Fatal(err)
	}
	if _, err := readPrivateRegularFile(p); err != nil {
		t.Fatalf("saved credential config is not a private regular file: %v", err)
	}
	if err := exposePrivateCredential(p); err != nil {
		t.Fatal(err)
	}
	if got, err := loadConfig(); err == nil {
		t.Fatalf("world-readable credential config enabled agent: %+v", got)
	}

	raw, err := json.Marshal(valid)
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, p); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("symlink unavailable: %v", err)
		}
		t.Fatal(err)
	}
	if got, err := loadConfig(); err == nil {
		t.Fatalf("symlinked credential config enabled agent: %+v", got)
	}
	if err := os.WriteFile(target, []byte("do-not-overwrite"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := saveConfig(valid); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != "do-not-overwrite" {
		t.Fatalf("save followed credential symlink: target=%q err=%v", got, err)
	}
	if info, err := os.Lstat(p); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("save did not replace symlink with a regular file: info=%v err=%v", info, err)
	}
	if _, err := readPrivateRegularFile(p); err != nil {
		t.Fatalf("save did not replace symlink with a private file: %v", err)
	}
}

func TestEnrollmentRefusesIncompleteAuthorityReceipt(t *testing.T) {
	valid := model.EnrollResponse{
		SchemaVersion: model.SchemaVersion,
		MachineID:     "machine-1", AgentToken: "agent-token",
		CheckinIntervalSeconds: 90, ObservationIntervalSeconds: 600,
		SettingsDigest: testSettingsDigest(90, 600),
	}
	tests := map[string]func(*model.EnrollResponse){
		"missing schema":        func(r *model.EnrollResponse) { r.SchemaVersion = 0 },
		"future schema":         func(r *model.EnrollResponse) { r.SchemaVersion++ },
		"missing machine":       func(r *model.EnrollResponse) { r.MachineID = "" },
		"missing credential":    func(r *model.EnrollResponse) { r.AgentToken = "" },
		"noncanonical token":    func(r *model.EnrollResponse) { r.AgentToken = " token " },
		"missing checkin clock": func(r *model.EnrollResponse) { r.CheckinIntervalSeconds = 0 },
		"missing observe clock": func(r *model.EnrollResponse) { r.ObservationIntervalSeconds = 0 },
		"missing settings bind": func(r *model.EnrollResponse) { r.SettingsDigest = "" },
		"bad settings bind":     func(r *model.EnrollResponse) { r.SettingsDigest = "sha256:ABC" },
		"wrong settings bind":   func(r *model.EnrollResponse) { r.SettingsDigest = testSettingsDigest(120, 600) },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			response := valid
			mutate(&response)
			if cfg, err := enrollmentConfig("http://hub", response); err == nil {
				t.Fatalf("invalid receipt enabled managed execution: %+v", cfg)
			}
		})
	}
}

func TestEnrollmentReceiptUsesStrictJSON(t *testing.T) {
	digest := testSettingsDigest(90, 600)
	valid := `{"schema_version":1,"machine_id":"machine-1","agent_token":"agent-token",` +
		`"checkin_interval_seconds":90,"observation_interval_seconds":600,` +
		`"settings_digest":"` + digest + `"}`
	t.Run("canonical receipt", func(t *testing.T) {
		server := newHubServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(valid))
		}))
		var receipt model.EnrollResponse
		if err := postJSONStrict(t.Context(), server.URL, "", struct{}{}, &receipt); err != nil {
			t.Fatal(err)
		}
		if cfg, err := enrollmentConfig(server.URL, receipt); err != nil || !cfg.JobsEnabled {
			t.Fatalf("canonical authority receipt did not enable jobs: cfg=%+v err=%v", cfg, err)
		}
	})
	tests := map[string]string{
		"unknown field":   strings.TrimSuffix(valid, "}") + `,"credential_ready":true}`,
		"duplicate token": strings.Replace(valid, `"agent_token":"agent-token"`, `"agent_token":"agent-token","agent_token":"other"`, 1),
		"trailing JSON":   valid + `{}`,
	}
	for name, response := range tests {
		t.Run(name, func(t *testing.T) {
			server := newHubServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(response))
			}))
			var receipt model.EnrollResponse
			if err := postJSONStrict(t.Context(), server.URL, "", struct{}{}, &receipt); err == nil {
				t.Fatalf("ambiguous enrollment response was accepted: %+v", receipt)
			}
		})
	}
}

func TestEnrollmentReceiptRequiresExactOKStatus(t *testing.T) {
	digest := testSettingsDigest(90, 600)
	valid := `{"schema_version":1,"machine_id":"machine-1","agent_token":"agent-token",` +
		`"checkin_interval_seconds":90,"observation_interval_seconds":600,` +
		`"settings_digest":"` + digest + `"}`
	for _, status := range []int{
		http.StatusCreated,
		http.StatusAccepted,
		http.StatusPartialContent,
		http.StatusNoContent,
	} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server := newHubServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				if status != http.StatusNoContent {
					_, _ = w.Write([]byte(valid))
				}
			}))
			var receipt model.EnrollResponse
			if err := postJSONStrict(t.Context(), server.URL, "", struct{}{}, &receipt); err == nil {
				t.Fatalf("HTTP %d authority response was accepted: %+v", status, receipt)
			}
		})
	}
}

// 錯誤訊息要講出「我不會替你跑 daemon」，而且要列出真正的子命令。
//
// ⚠ 一個只說「認不得的參數」的錯誤訊息，會讓人再猜一次 ——
// 而上一次猜的結果是一個跑了六小時的背景程序。
func TestTheErrorMessageSaysWhatItRefusedToDo(t *testing.T) {
	msg := unknownArgError("--version")
	if !strings.Contains(msg, "--version") {
		t.Error("錯誤訊息沒有把使用者打的那個字回述出來")
	}
	if !strings.Contains(msg, "daemon") {
		t.Error("沒有講出「我不會就這樣去跑 daemon」—— " +
			"那是這個錯誤唯一想預防的事")
	}
	for _, sub := range []string{"enroll", "probe", "version"} {
		if !strings.Contains(msg, sub) {
			t.Errorf("沒有列出子命令 %q —— 拒絕之後要告訴人正確的寫法是什麼", sub)
		}
	}
}
