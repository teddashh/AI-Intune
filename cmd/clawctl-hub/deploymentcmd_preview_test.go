package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/teddashh/AI-Intune/internal/deploy"
	"github.com/teddashh/AI-Intune/internal/operator"
)

func TestWriteDeploymentActionPreviewShowsTerminalFailureOutcomesOnlyInHumanOutput(t *testing.T) {
	targets := []struct {
		operator.DeploymentTerminalFailurePreview
		outcome string
	}{
		{DeploymentTerminalFailurePreview: operator.DeploymentTerminalFailurePreview{MachineID: "machine-failed", DisplayName: "failed-machine", JobID: "job-failed", JobState: deploy.Failed}, outcome: "失敗"},
		{DeploymentTerminalFailurePreview: operator.DeploymentTerminalFailurePreview{MachineID: "machine-rejected", DisplayName: "rejected-machine", JobID: "job-rejected", JobState: deploy.Rejected}, outcome: "被拒絕，機器沒有改動"},
		{DeploymentTerminalFailurePreview: operator.DeploymentTerminalFailurePreview{MachineID: "machine-expired", DisplayName: "expired-machine", JobID: "job-expired", JobState: deploy.LeaseExpired}, outcome: "代理程式沒有回報，Hub 收了這張單"},
		{DeploymentTerminalFailurePreview: operator.DeploymentTerminalFailurePreview{MachineID: "machine-manual", DisplayName: "manual-machine", JobID: "job-manual", JobState: deploy.ManualIntervention}, outcome: "需要人介入，沒有回退證據"},
	}
	terminalFailureTargets := make([]operator.DeploymentTerminalFailurePreview, 0, len(targets))
	for _, target := range targets {
		terminalFailureTargets = append(terminalFailureTargets, target.DeploymentTerminalFailurePreview)
	}
	preview := operator.DeploymentActionPreviewResult{
		Action: "retry",
		Deployment: operator.DeploymentSummary{
			DeploymentID: "deployment-retry", Channel: "canary",
		},
		Eligibility:            operator.DeploymentActionEligibility{Eligible: true, Outcome: "create_retry_attempt", AffectedTargets: 4},
		TerminalFailureTargets: terminalFailureTargets,
		PreviewDigest:          "digest",
	}

	var human bytes.Buffer
	if err := writeDeploymentActionPreview(&human, preview, false, "operator API"); err != nil {
		t.Fatal(err)
	}
	for _, target := range targets {
		for _, want := range []string{
			target.DisplayName, target.JobID, target.outcome, string(target.JobState),
		} {
			if !strings.Contains(human.String(), want) {
				t.Errorf("human preview missing %q: %s", want, human.String())
			}
		}
		if target.JobState != deploy.Failed {
			for _, line := range strings.Split(human.String(), "\n") {
				if strings.Contains(line, target.DisplayName) && strings.Contains(line, "失敗") {
					t.Errorf("human preview row for %q contains 失敗: %s", target.DisplayName, line)
				}
			}
		}
	}

	var jsonOutput bytes.Buffer
	if err := writeDeploymentActionPreview(&jsonOutput, preview, true, "operator API"); err != nil {
		t.Fatal(err)
	}
	for _, target := range targets {
		if strings.Contains(jsonOutput.String(), target.outcome) {
			t.Errorf("JSON preview contains human outcome %q: %s", target.outcome, jsonOutput.String())
		}
	}
}

func TestWriteDeploymentActionPreviewAddsHumanImpactOnlyToHumanOutput(t *testing.T) {
	tests := []struct {
		name            string
		preview         operator.DeploymentActionPreviewResult
		wantHuman       string
		forbiddenInJSON []string
	}{
		{
			name: "eligible finish",
			preview: operator.DeploymentActionPreviewResult{
				Action:      "continue",
				Deployment:  operator.DeploymentSummary{DeploymentID: "deployment-finish", Channel: "canary"},
				Eligibility: operator.DeploymentActionEligibility{Eligible: true, Outcome: "finish"},
			},
			wantHuman:       "確認後會發生：\"所有批次都已開完且最後一批是 succeeded；確認後會把 deployment 收成 finished。\"",
			forbiddenInJSON: []string{"確認後會發生", "所有批次都已開完"},
		},
		{
			name: "ineligible blockers",
			preview: operator.DeploymentActionPreviewResult{
				Action:     "continue",
				Deployment: operator.DeploymentSummary{DeploymentID: "deployment-blocked", Channel: "canary"},
				Eligibility: operator.DeploymentActionEligibility{
					Eligible: false,
					Blockers: []string{"material_unavailable", "nonterminal_jobs"},
				},
			},
			wantHuman:       "目前不可執行：\"Hub 無法重新驗證原 deployment material；仍有未終態工作單\"",
			forbiddenInJSON: []string{"目前不可執行", "Hub 無法重新驗證原 deployment material", "仍有未終態工作單"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var human bytes.Buffer
			if err := writeDeploymentActionPreview(&human, tt.preview, false, "operator API"); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(human.String(), tt.wantHuman) {
				t.Fatalf("human preview 缺少 %q: %s", tt.wantHuman, human.String())
			}

			var jsonOutput bytes.Buffer
			if err := writeDeploymentActionPreview(&jsonOutput, tt.preview, true, "operator API"); err != nil {
				t.Fatal(err)
			}
			for _, forbidden := range tt.forbiddenInJSON {
				if strings.Contains(jsonOutput.String(), forbidden) {
					t.Fatalf("JSON preview 含有人話 %q: %s", forbidden, jsonOutput.String())
				}
			}
		})
	}
}
