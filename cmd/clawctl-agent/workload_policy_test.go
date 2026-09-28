package main

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/model"
)

func TestObservationUsesRulesAndTokenFromOneAtomicPolicyBundle(t *testing.T) {
	dir := t.TempDir()
	oldPath := filepath.Join(dir, "old.jsonl")
	newPath := filepath.Join(dir, "new.jsonl")
	if err := os.WriteFile(oldPath, []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(newPath, []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}
	rule := func(path string) model.Expectation {
		return model.Expectation{
			Machine: "samplehub1", Unit: "proof.service", Artifact: path,
			MaxAgeSeconds: 3600, Why: "policy handoff test",
			Events: &model.EventSpec{
				TsField: "at", TypeField: "kind", NotOK: []string{"broken"}, WindowSeconds: 3600,
			},
		}
	}
	oldPolicy := &workloadPolicyBundle{expectations: []model.Expectation{rule(oldPath)}, token: "old-token"}
	newPolicy := &workloadPolicyBundle{expectations: []model.Expectation{rule(newPath)}, token: "new-token"}
	var current atomic.Pointer[workloadPolicyBundle]
	current.Store(oldPolicy)

	// 觀測只准 Load 一次。模擬 checkin 在 Load 後發布新 bundle；這一批仍須
	// 從頭到尾使用舊 rules + 舊 token，不能混成 old rules/new token。
	loaded := current.Load()
	current.Store(newPolicy)
	obs := model.ObservationBatch{MeasuredAt: time.Now().UTC()}
	applyWorkloadPolicy(&obs, loaded)

	if obs.WorkloadPolicyToken != "old-token" {
		t.Fatalf("token=%q, want old-token", obs.WorkloadPolicyToken)
	}
	if len(obs.Artifacts) != 1 || obs.Artifacts[0].Artifact != oldPath {
		t.Fatalf("artifact measurement did not use loaded bundle: %+v", obs.Artifacts)
	}
	if len(obs.Events) != 1 || obs.Events[0].Path != oldPath {
		t.Fatalf("event measurement did not use loaded bundle: %+v", obs.Events)
	}
}

func TestObservationBeforeFirstCheckinCarriesNoInventedPolicyToken(t *testing.T) {
	obs := model.ObservationBatch{MeasuredAt: time.Now().UTC()}
	applyWorkloadPolicy(&obs, nil)
	if obs.WorkloadPolicyToken != "" || obs.Artifacts != nil || obs.Events != nil {
		t.Fatalf("pre-checkin observation invented policy evidence: %+v", obs)
	}
}
