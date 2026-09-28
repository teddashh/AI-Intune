package main

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/model"
	"github.com/teddashh/AI-Intune/internal/settingpolicy"
)

// adoptSettings writes agent.json and the state file, so the test points both
// at a temp HOME rather than the operator's real config.
func isolateAgentHome(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, ".config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, ".local", "state"))
}

func baseAgentConfig() config {
	return config{HubURL: "http://hub.invalid", MachineID: "m1", AgentToken: "t",
		EnrollmentSchemaVersion:  model.SchemaVersion,
		EnrollmentSettingsDigest: testSettingsDigest(120, 600),
		CheckinIntervalSeconds:   120, ObservationIntervalSeconds: 600}
}

func testSettingsDigest(checkin, observation int) string {
	return settingpolicy.MustDigest(settingpolicy.Settings{
		SchemaVersion:              settingpolicy.SchemaVersion,
		CheckinIntervalSeconds:     checkin,
		ObservationIntervalSeconds: observation,
	})
}

// The Hub asks "are you running what I assigned"; an agent that reports the
// answer it was just told would be answering its own question.
func TestAgentReportsTheSettingsItIsRunningNotTheOnesItWasJustSent(t *testing.T) {
	isolateAgentHome(t)
	cfg := baseAgentConfig()
	seq := &atomic.Int64{}
	var holder atomic.Pointer[appliedSettings]
	current := &appliedSettings{checkin: 2 * time.Minute, observation: 10 * time.Minute}
	holder.Store(current)

	digest := testSettingsDigest(300, 900)
	resp := model.CheckinResponse{CheckinIntervalSeconds: 300,
		ObservationIntervalSeconds: 900, SettingsDigest: digest}
	next := adoptSettings(current, resp, &holder, cfg, seq)

	if next.checkin != 5*time.Minute || next.observation != 15*time.Minute {
		t.Fatalf("沒有採用新的間隔：%+v", next)
	}
	if next.digest != digest {
		t.Fatalf("digest 沒有跟著換：%q", next.digest)
	}
	if holder.Load() != next {
		t.Fatal("採用了卻沒有發布給其他 goroutine")
	}
	// The三個值必須一起換：回報 A 卻按 B 的節奏跑，Hub 收到的是一份它無法
	// 驗證的自述。
	live := holder.Load()
	if live.digest != next.digest || live.checkin != next.checkin {
		t.Fatalf("digest 與間隔不是同一包：%+v", live)
	}

	// Restart continuity: the adopted intervals are written back, so the agent
	// resumes at the assigned pace instead of the enrolment-time one.
	saved, err := loadConfig()
	if err != nil {
		t.Fatalf("採用後讀不回 agent.json：%v", err)
	}
	if saved.CheckinIntervalSeconds != 300 || saved.ObservationIntervalSeconds != 900 {
		t.Fatalf("新間隔沒有寫回 agent.json：%+v", saved)
	}
	if st := loadState(); st.SettingsDigest != digest {
		t.Fatalf("採用的 digest 沒有跨重啟保存：%+v", st)
	}
	if saved.EnrollmentSettingsDigest != digest {
		t.Fatalf("agent.json 沒保存 interval authority digest：%+v", saved)
	}
}

func TestAgentDoesNotRewriteConfigWhenTheHubSaysTheSameThing(t *testing.T) {
	isolateAgentHome(t)
	cfg := baseAgentConfig()
	seq := &atomic.Int64{}
	var holder atomic.Pointer[appliedSettings]
	digest := testSettingsDigest(120, 600)
	current := &appliedSettings{checkin: 2 * time.Minute, observation: 10 * time.Minute,
		digest: digest}
	holder.Store(current)

	resp := model.CheckinResponse{CheckinIntervalSeconds: 120,
		ObservationIntervalSeconds: 600, SettingsDigest: digest}
	if got := adoptSettings(current, resp, &holder, cfg, seq); got != current {
		t.Fatalf("同一份設定卻換了一包：%+v", got)
	}
	// Nothing changed, so nothing was written; a config file appearing here
	// would mean every heartbeat rewrites the disk.
	if _, err := os.Stat(configPath()); !os.IsNotExist(err) {
		t.Fatalf("沒有變更卻寫了 agent.json：%v", err)
	}
}

// A Hub that answers without intervals is a Hub too old to have settings. The
// agent keeps running what it has rather than falling back to a built-in.
func TestAgentKeepsRunningWhenTheHubSendsNoIntervals(t *testing.T) {
	isolateAgentHome(t)
	cfg := baseAgentConfig()
	seq := &atomic.Int64{}
	var holder atomic.Pointer[appliedSettings]
	current := &appliedSettings{checkin: 5 * time.Minute, observation: 15 * time.Minute,
		digest: "sha256:kept"}
	holder.Store(current)

	for _, resp := range []model.CheckinResponse{
		{},
		{CheckinIntervalSeconds: 300},
		{ObservationIntervalSeconds: 900},
	} {
		if got := adoptSettings(current, resp, &holder, cfg, seq); got != current {
			t.Fatalf("回應不完整卻換了設定：%+v", got)
		}
	}
	if holder.Load().digest != "sha256:kept" {
		t.Fatal("回應不完整卻把 digest 清掉了")
	}
}

func TestAgentRejectsSettingsDigestForDifferentIntervals(t *testing.T) {
	isolateAgentHome(t)
	cfg := baseAgentConfig()
	seq := &atomic.Int64{}
	var holder atomic.Pointer[appliedSettings]
	current := &appliedSettings{checkin: 2 * time.Minute, observation: 10 * time.Minute,
		digest: testSettingsDigest(120, 600)}
	holder.Store(current)

	resp := model.CheckinResponse{CheckinIntervalSeconds: 300,
		ObservationIntervalSeconds: 900, SettingsDigest: testSettingsDigest(120, 600)}
	if got := adoptSettings(current, resp, &holder, cfg, seq); got != current {
		t.Fatalf("mismatched digest adopted settings: %+v", got)
	}
	if holder.Load() != current {
		t.Fatal("mismatched digest replaced live settings")
	}
	if _, err := os.Stat(configPath()); !os.IsNotExist(err) {
		t.Fatalf("mismatched digest wrote agent.json: %v", err)
	}
}
