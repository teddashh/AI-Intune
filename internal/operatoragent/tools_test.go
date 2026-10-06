package operatoragent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/teddashh/AI-Intune/internal/deploy"
	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/operatorclient"
	"github.com/teddashh/AI-Intune/internal/rollout"
)

const testDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

type fakeHub struct {
	calls   []string
	detail  operator.DeploymentDetailResult
	preview operator.DeploymentCreatePreviewResult
}

func (f *fakeHub) call(name string) { f.calls = append(f.calls, name) }

func (f *fakeHub) called(name string) bool {
	for _, got := range f.calls {
		if got == name {
			return true
		}
	}
	return false
}

func (f *fakeHub) ListMachines(context.Context, operator.MachineListRequest) (operator.MachineListResult, error) {
	f.call("ListMachines")
	next := "cursor"
	return operator.MachineListResult{Total: 2, Active: 2, NextCursor: &next}, nil
}
func (f *fakeHub) Machine(context.Context, string) (operator.MachineDetailResult, error) {
	f.call("Machine")
	return operator.MachineDetailResult{}, nil
}
func (f *fakeHub) MachineEvidence(context.Context, string, int) (operator.MachineEvidenceResult, error) {
	f.call("MachineEvidence")
	return operator.MachineEvidenceResult{}, nil
}
func (f *fakeHub) Jobs(context.Context, operator.JobListRequest) (operator.JobListResult, error) {
	f.call("Jobs")
	return operator.JobListResult{}, nil
}
func (f *fakeHub) Job(context.Context, string) (operator.JobDetailResult, error) {
	f.call("Job")
	return operator.JobDetailResult{}, nil
}
func (f *fakeHub) JobEvidence(context.Context, string, int) (operator.JobEvidenceResult, error) {
	f.call("JobEvidence")
	return operator.JobEvidenceResult{}, nil
}
func (f *fakeHub) Deployments(context.Context, operator.DeploymentListRequest) (operator.DeploymentListResult, error) {
	f.call("Deployments")
	return operator.DeploymentListResult{}, nil
}
func (f *fakeHub) Deployment(context.Context, string) (operator.DeploymentDetailResult, error) {
	f.call("Deployment")
	return f.detail, nil
}
func (f *fakeHub) SoftwareReport(context.Context) (operator.SoftwareReport, error) {
	f.call("SoftwareReport")
	return operator.SoftwareReport{}, nil
}
func (f *fakeHub) ComplianceBoard(context.Context) (operator.ComplianceBoardResult, error) {
	f.call("ComplianceBoard")
	return operator.ComplianceBoardResult{}, nil
}
func (f *fakeHub) PreviewEnrollToken(context.Context, operatorclient.EnrollmentTokenPreviewRequest) (operatorclient.EnrollmentTokenPreviewResponse, error) {
	f.call("PreviewEnrollToken")
	return operatorclient.EnrollmentTokenPreviewResponse{PreviewDigest: testDigest}, nil
}
func (f *fakeHub) CreateEnrollToken(context.Context, string, operatorclient.EnrollmentTokenCreateRequest) (operatorclient.EnrollmentTokenResponse, error) {
	f.call("CreateEnrollToken")
	return operatorclient.EnrollmentTokenResponse{}, nil
}
func (f *fakeHub) PreviewDeploymentCreate(context.Context, operator.DeploymentCreatePreviewRequest) (operator.DeploymentCreatePreviewResult, error) {
	f.call("PreviewDeploymentCreate")
	return f.preview, nil
}
func (f *fakeHub) CreateDeployment(context.Context, string, operatorclient.DeploymentCreateRequest) (operator.DeploymentMutationResult, error) {
	f.call("CreateDeployment")
	return operator.DeploymentMutationResult{}, nil
}
func (f *fakeHub) PreviewDeploymentContinue(context.Context, string) (operator.DeploymentActionPreviewResult, error) {
	f.call("PreviewDeploymentContinue")
	return operator.DeploymentActionPreviewResult{}, nil
}
func (f *fakeHub) ContinueDeployment(context.Context, string, string, operatorclient.DeploymentContinueRequest) (operator.DeploymentMutationResult, error) {
	f.call("ContinueDeployment")
	return operator.DeploymentMutationResult{}, nil
}
func (f *fakeHub) PreviewDeploymentAbandon(context.Context, string) (operator.DeploymentActionPreviewResult, error) {
	f.call("PreviewDeploymentAbandon")
	return operator.DeploymentActionPreviewResult{}, nil
}
func (f *fakeHub) AbandonDeployment(context.Context, string, string, operatorclient.DeploymentAbandonRequest) (operator.DeploymentMutationResult, error) {
	f.call("AbandonDeployment")
	return operator.DeploymentMutationResult{}, nil
}
func (f *fakeHub) PreviewMachineProfileAssignment(context.Context, operator.MachineProfileAssignmentPreviewRequest) (operator.MachineProfileAssignmentPreviewResult, error) {
	f.call("PreviewMachineProfileAssignment")
	return operator.MachineProfileAssignmentPreviewResult{}, nil
}
func (f *fakeHub) AssignMachineProfile(context.Context, string, operator.MachineProfileAssignmentRequest) (operator.MachineProfileAssignmentResult, error) {
	f.call("AssignMachineProfile")
	return operator.MachineProfileAssignmentResult{}, nil
}

func statePtr(state deploy.JobState) *deploy.JobState { return &state }
func strPtr(value string) *string                     { return &value }

func canaryDetail(state string, job deploy.JobState) operator.DeploymentDetailResult {
	return operator.DeploymentDetailResult{Item: operator.DeploymentSummary{
		DeploymentID: "dep-1", Channel: "canary", State: state,
		ControlRevision: 4, OpenedBatch: 1, TotalBatches: 2, BatchSize: 1,
	}, Targets: []operator.DeploymentTargetSummary{
		{MachineID: "m-a", DisplayName: "alpha", BatchNo: 1, JobID: strPtr("job-1"), JobState: statePtr(job)},
		{MachineID: "m-b", DisplayName: "beta", BatchNo: 2},
	}}
}

func callErr(t *testing.T, err error) *CallError {
	t.Helper()
	var call *CallError
	if !errors.As(err, &call) {
		t.Fatalf("error = %v", err)
	}
	return call
}

func TestWriteToolsDoNotCallHubWithoutPreviewDigest(t *testing.T) {
	f := &fakeHub{}
	svc := &Service{Hub: f}
	cases := []struct {
		tool string
		raw  string
		code string
	}{
		{"enroll_ticket_create", `{"display_name":"peach","ttl_seconds":3600,"reason":"enroll","idempotency_key":"k1"}`, "preview_digest_required"},
		{"deployment_create", `{"channel":"canary","version":"2026.9.8","artifact_sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","reason":"ship","confirm_channel":"canary","confirm_version":"2026.9.8","idempotency_key":"k1"}`, "preview_digest_required"},
		{"deployment_continue", `{"deployment_id":"dep-1","expected_control_revision":4,"expected_opened_batch":1,"confirm_channel":"canary","reason":"go","idempotency_key":"k1"}`, "preview_digest_required"},
		{"deployment_abandon", `{"deployment_id":"dep-1","expected_control_revision":4,"expected_opened_batch":1,"confirm_deployment_id":"dep-1","reason":"stop","idempotency_key":"k1"}`, "preview_digest_required"},
		{"profile_assignment_apply", `{"machine_id":"m-a","profile_id":"p","profile_revision":1,"confirm_display_name":"alpha","reason":"assign","idempotency_key":"k1"}`, "preview_digest_required"},
		{"rollout_apply", `{"channel":"canary","version":"2026.9.8","artifact_sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","confirm_channel":"canary","confirm_version":"2026.9.8","reason":"ship","idempotency_key":"k1"}`, "preview_digest_required"},
	}
	for _, tc := range cases {
		t.Run(tc.tool, func(t *testing.T) {
			f.calls = nil
			_, err := svc.Call(context.Background(), tc.tool, json.RawMessage(tc.raw))
			if callErr(t, err).Code != tc.code {
				t.Fatalf("code = %s", callErr(t, err).Code)
			}
			if len(f.calls) != 0 {
				t.Fatalf("Hub calls = %v", f.calls)
			}
		})
	}
}

func TestContinueRequiresExpectedRevisionBeforeHub(t *testing.T) {
	f := &fakeHub{}
	svc := &Service{Hub: f}
	raw := `{"deployment_id":"dep-1","preview_digest":"` + testDigest + `","confirm_channel":"canary","reason":"go","idempotency_key":"k1"}`
	_, err := svc.Call(context.Background(), "deployment_continue", json.RawMessage(raw))
	if callErr(t, err).Code != "expected_revision_required" || len(f.calls) != 0 {
		t.Fatalf("err=%v calls=%v", err, f.calls)
	}
}

func TestRolloutExpandStopsWithoutContinue(t *testing.T) {
	f := &fakeHub{detail: canaryDetail("paused", deploy.Failed)}
	svc := &Service{Hub: f}
	raw := `{"deployment_id":"dep-1","preview_digest":"` + testDigest + `","expected_control_revision":4,"expected_opened_batch":1,"confirm_channel":"canary","reason":"skip","idempotency_key":"k1"}`
	_, err := svc.Call(context.Background(), "rollout_expand", json.RawMessage(raw))
	if callErr(t, err).Code != "canary_blocked" {
		t.Fatal(err)
	}
	if f.called("ContinueDeployment") || !f.called("Deployment") {
		t.Fatalf("calls = %v", f.calls)
	}
}

func TestRolloutExpandWritesContinueOnlyWhenPausedAndSucceeded(t *testing.T) {
	f := &fakeHub{detail: canaryDetail("paused", deploy.Succeeded)}
	svc := &Service{Hub: f}
	body := map[string]any{
		"deployment_id": "dep-1", "preview_digest": testDigest,
		"expected_control_revision": 4, "expected_opened_batch": 1,
		"confirm_channel": "canary", "reason": "expand", "idempotency_key": "k1",
	}
	raw, _ := json.Marshal(body)
	result, err := svc.Call(context.Background(), "rollout_expand", raw)
	if err != nil {
		t.Fatal(err)
	}
	if !f.called("ContinueDeployment") {
		t.Fatalf("calls = %v", f.calls)
	}
	encoded, _ := json.Marshal(result)
	if !bytes.Contains(encoded, []byte(`"wrote":true`)) {
		t.Fatalf("result = %s", encoded)
	}

	f.calls = nil
	body["expected_control_revision"] = 9
	raw, _ = json.Marshal(body)
	_, err = svc.Call(context.Background(), "rollout_expand", raw)
	if callErr(t, err).Code != "expected_revision_mismatch" || f.called("ContinueDeployment") {
		t.Fatalf("err=%v calls=%v", err, f.calls)
	}

	f.detail.Item.State = "running"
	f.calls = nil
	body["expected_control_revision"] = 4
	raw, _ = json.Marshal(body)
	result, err = svc.Call(context.Background(), "rollout_expand", raw)
	if err != nil || f.called("ContinueDeployment") {
		t.Fatalf("err=%v calls=%v", err, f.calls)
	}
	encoded, _ = json.Marshal(result)
	if !bytes.Contains(encoded, []byte(`"wrote":false`)) {
		t.Fatalf("running expand = %s", encoded)
	}
}

func TestRolloutPreviewRejectsWideBatchBeforeHub(t *testing.T) {
	f := &fakeHub{}
	svc := &Service{Hub: f}
	_, err := svc.Call(context.Background(), "rollout_preview", json.RawMessage(`{"channel":"canary","version":"2026.9.8","artifact_sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","batch_size":5}`))
	if callErr(t, err).Code != "canary_batch_size" || len(f.calls) != 0 {
		t.Fatalf("err=%v calls=%v", err, f.calls)
	}
}

func TestRolloutPreviewAnnotatesTheServerPlan(t *testing.T) {
	f := &fakeHub{preview: operator.DeploymentCreatePreviewResult{
		BatchSize: 1, TotalBatches: 2, PreviewDigest: testDigest,
		Targets: []operator.DeploymentPlanTargetPreview{
			{MachineID: "m-a", DisplayName: "alpha", BatchNo: 1},
			{MachineID: "m-b", DisplayName: "beta", BatchNo: 2},
		},
	}}
	svc := &Service{Hub: f}
	result, err := svc.Call(context.Background(), "rollout_preview", json.RawMessage(`{"channel":"canary","version":"2026.9.8","artifact_sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`))
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(result)
	if !bytes.Contains(encoded, []byte(`"phase":"`+rollout.CanaryPending+`"`)) || !f.called("PreviewDeploymentCreate") {
		t.Fatalf("result=%s calls=%v", encoded, f.calls)
	}
}

func TestFleetOverviewMarksAPartialPage(t *testing.T) {
	f := &fakeHub{}
	result, err := (&Service{Hub: f}).Call(context.Background(), "fleet_overview", nil)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(result)
	if !bytes.Contains(encoded, []byte(`"page_truncated":true`)) || !bytes.Contains(encoded, []byte(`"total":2`)) {
		t.Fatalf("overview = %s", encoded)
	}
}

func TestUnknownFieldAndTool(t *testing.T) {
	f := &fakeHub{}
	svc := &Service{Hub: f}
	_, err := svc.Call(context.Background(), "fleet_overview", json.RawMessage(`{"authorization":"nope"}`))
	if callErr(t, err).Code != "invalid_arguments" || len(f.calls) != 0 {
		t.Fatalf("err=%v calls=%v", err, f.calls)
	}
	_, err = svc.Call(context.Background(), "sudo", nil)
	if callErr(t, err).Code != "unknown_tool" {
		t.Fatal(err)
	}
}

func TestToolNamesAreUnique(t *testing.T) {
	seen := map[string]bool{}
	for _, tool := range Tools() {
		if seen[tool.Name] || tool.Description == "" || tool.InputSchema["type"] != "object" {
			t.Fatalf("bad tool %+v", tool)
		}
		seen[tool.Name] = true
	}
	for _, name := range []string{"fleet_overview", "machine_evidence", "jobs_list", "software_report", "compliance", "rollout_expand", "enroll_ticket_create", "profile_assignment_apply"} {
		if !seen[name] {
			t.Fatalf("missing %s", name)
		}
	}
}

func TestMCPInitializeListAndRefusedWrite(t *testing.T) {
	f := &fakeHub{}
	var in, out bytes.Buffer
	messages := []string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"grok","version":"1"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"enroll_ticket_create","arguments":{"display_name":"peach","ttl_seconds":60,"reason":"enroll","idempotency_key":"k1"}}}`,
	}
	in.WriteString(strings.Join(messages, "\n") + "\n")
	if err := Serve(context.Background(), &in, &out, &Service{Hub: f}); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("responses = %d\n%s", len(lines), out.String())
	}
	if !strings.Contains(lines[0], `"protocolVersion":"2025-03-26"`) || !strings.Contains(lines[0], "WhoIs") {
		t.Fatalf("initialize = %s", lines[0])
	}
	if !strings.Contains(lines[1], "rollout_expand") || !strings.Contains(lines[1], `"id":2`) {
		t.Fatalf("list = %s", lines[1])
	}
	if !strings.Contains(lines[2], `"isError":true`) || !strings.Contains(lines[2], "preview_digest_required") {
		t.Fatalf("call = %s", lines[2])
	}
	if len(f.calls) != 0 {
		t.Fatalf("Hub calls = %v", f.calls)
	}
}

func TestMCPContentLengthPing(t *testing.T) {
	body := `{"jsonrpc":"2.0","id":"p","method":"ping"}`
	var in, out bytes.Buffer
	in.WriteString("Content-Length: " + strconv.Itoa(len(body)) + "\r\n\r\n" + body)
	if err := Serve(context.Background(), &in, &out, &Service{Hub: &fakeHub{}}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"id":"p"`) || strings.Contains(out.String(), "Content-Length") {
		t.Fatalf("ping response = %s", out.String())
	}
}
