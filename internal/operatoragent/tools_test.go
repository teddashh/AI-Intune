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
	"github.com/teddashh/AI-Intune/internal/state"
	"github.com/teddashh/AI-Intune/internal/store"
)

const testDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

type fakeHub struct {
	calls   []string
	detail  operator.DeploymentDetailResult
	preview operator.DeploymentCreatePreviewResult
	err     error
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
	if f.err != nil {
		return operator.MachineListResult{}, f.err
	}
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
func (f *fakeHub) DiskCleanSummaries(context.Context) ([]store.DiskCleanSummaryView, error) {
	f.call("DiskCleanSummaries")
	return []store.DiskCleanSummaryView{}, nil
}
func (f *fakeHub) DiskCleanSummary(context.Context, string) (store.DiskCleanSummaryView, error) {
	f.call("DiskCleanSummary")
	return store.DiskCleanSummaryView{}, nil
}
func (f *fakeHub) PreviewDiskCleanProfile(context.Context, operatorclient.DiskCleanProfilePreviewRequest) (store.DiskCleanProfilePreview, error) {
	f.call("PreviewDiskCleanProfile")
	return store.DiskCleanProfilePreview{PreviewDigest: testDigest}, nil
}
func (f *fakeHub) PublishDiskCleanProfile(context.Context, string, operatorclient.DiskCleanProfilePublishRequest) (store.DiskCleanProfileResult, error) {
	f.call("PublishDiskCleanProfile")
	return store.DiskCleanProfileResult{}, nil
}
func (f *fakeHub) PreviewDiskCleanDryRun(context.Context, operatorclient.DiskCleanTargetPreviewRequest) (store.DiskCleanDryRunPreview, error) {
	f.call("PreviewDiskCleanDryRun")
	return store.DiskCleanDryRunPreview{PreviewDigest: testDigest}, nil
}
func (f *fakeHub) ApplyDiskCleanDryRun(context.Context, string, operatorclient.DiskCleanTargetApplyRequest) (store.DiskCleanDryRunResult, error) {
	f.call("ApplyDiskCleanDryRun")
	return store.DiskCleanDryRunResult{}, nil
}
func (f *fakeHub) PreviewDiskCleanCanary(context.Context, operatorclient.DiskCleanCanaryPreviewRequest) (store.DiskCleanCanaryPreview, error) {
	f.call("PreviewDiskCleanCanary")
	return store.DiskCleanCanaryPreview{}, nil
}
func (f *fakeHub) ApplyDiskCleanCanary(context.Context, string, operatorclient.DiskCleanCanaryApplyRequest) (store.DiskCleanCanaryResult, error) {
	f.call("ApplyDiskCleanCanary")
	return store.DiskCleanCanaryResult{}, nil
}
func (f *fakeHub) PreviewDiskCleanContinue(context.Context, string) (store.DiskCleanControlPreview, error) {
	f.call("PreviewDiskCleanContinue")
	return store.DiskCleanControlPreview{PreviewDigest: testDigest}, nil
}
func (f *fakeHub) ContinueDiskClean(context.Context, string, operatorclient.DiskCleanControlApplyRequest) (store.DiskCleanRolloutResult, error) {
	f.call("ContinueDiskClean")
	return store.DiskCleanRolloutResult{}, nil
}
func (f *fakeHub) PreviewDiskCleanAbandon(context.Context, string) (store.DiskCleanControlPreview, error) {
	f.call("PreviewDiskCleanAbandon")
	return store.DiskCleanControlPreview{PreviewDigest: testDigest}, nil
}
func (f *fakeHub) AbandonDiskClean(context.Context, string, operatorclient.DiskCleanControlApplyRequest) (store.DiskCleanRolloutResult, error) {
	f.call("AbandonDiskClean")
	return store.DiskCleanRolloutResult{}, nil
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
		{"disk_clean_profile_publish", `{"scope_type":"channel","scope_id":"stable","profile":{"schema_version":1,"scope":"user","dry_run":true,"categories":["user_tmp"],"tmp_age_days":7,"attention_pct":90,"mount":"/"},"expected_revision":0,"confirm_scope_id":"stable","reason":"publish","idempotency_key":"k1"}`, "preview_digest_required"},
		{"disk_clean_dry_run_apply", `{"scope_type":"channel","scope_id":"stable","revision":1,"machine_ids":["machine-1"],"confirm_scope_id":"stable","reason":"preview run","idempotency_key":"k1"}`, "preview_digest_required"},
		{"disk_clean_canary_apply", `{"scope_type":"channel","scope_id":"stable","revision":1,"machine_ids":["machine-1"],"canary_machine_id":"machine-1","confirm_scope_id":"stable","reason":"canary","idempotency_key":"k1"}`, "preview_digest_required"},
		{"disk_clean_continue_apply", `{"rollout_id":"roll-1","expected_control_revision":1,"expected_opened_batch":1,"confirm_rollout_id":"roll-1","reason":"continue","idempotency_key":"k1"}`, "preview_digest_required"},
		{"disk_clean_abandon_apply", `{"rollout_id":"roll-1","expected_control_revision":1,"expected_opened_batch":1,"confirm_rollout_id":"roll-1","reason":"stop","idempotency_key":"k1"}`, "preview_digest_required"},
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

func TestDiskCleanContinueRequiresExpectedRevisionBeforeHub(t *testing.T) {
	f := &fakeHub{}
	svc := &Service{Hub: f}
	raw := `{"rollout_id":"roll-1","preview_digest":"` + testDigest + `","confirm_rollout_id":"roll-1","reason":"go","idempotency_key":"k1"}`
	_, err := svc.Call(context.Background(), "disk_clean_continue_apply", json.RawMessage(raw))
	if callErr(t, err).Code != "expected_revision_required" || len(f.calls) != 0 {
		t.Fatalf("err=%v calls=%v", err, f.calls)
	}
}

func TestDiskCleanToolsAreClosedAndFailClosed(t *testing.T) {
	f := &fakeHub{}
	svc := &Service{Hub: f}
	seen := map[string]Tool{}
	for _, tool := range Tools() {
		if !strings.HasPrefix(tool.Name, "disk_clean_") {
			continue
		}
		seen[tool.Name] = tool
		if tool.InputSchema["additionalProperties"] != false {
			t.Fatalf("%s accepts unknown fields", tool.Name)
		}
		if tool.Annotations == nil || tool.Annotations.OpenWorldHint {
			t.Fatalf("%s annotations = %+v", tool.Name, tool.Annotations)
		}
	}
	if len(seen) != 12 {
		t.Fatalf("disk-clean tools = %d", len(seen))
	}
	raw := `{"scope_type":"channel","scope_id":"stable","revision":1,"machine_ids":["machine-1"],"preview_digest":"` + testDigest + `","confirm_scope_id":"stable","reason":"preview run","idempotency_key":"k1"}`
	if _, err := svc.Call(context.Background(), "disk_clean_dry_run_apply", json.RawMessage(raw)); err != nil {
		t.Fatal(err)
	}
	if !f.called("ApplyDiskCleanDryRun") {
		t.Fatalf("calls = %v", f.calls)
	}
	f.calls = nil
	if _, err := svc.Call(context.Background(), "disk_clean_dry_run_apply", json.RawMessage(raw[:len(raw)-1]+`,"shell":"rm"}`)); callErr(t, err).Code != "invalid_arguments" || len(f.calls) != 0 {
		t.Fatalf("err=%v calls=%v", err, f.calls)
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
	got := callErr(t, err)
	if got.Code != "invalid_arguments" || !strings.Contains(got.Message, `"authorization"`) || !strings.Contains(got.Message, "(none)") || len(f.calls) != 0 {
		t.Fatalf("err=%v calls=%v", err, f.calls)
	}
	_, err = svc.Call(context.Background(), "machine_get", json.RawMessage(`{"machine_id":"m1","extra":true}`))
	got = callErr(t, err)
	if got.Code != "invalid_arguments" || !strings.Contains(got.Message, `"extra"`) || !strings.Contains(got.Message, "machine_id") {
		t.Fatalf("unknown field = %v", err)
	}
	_, err = svc.Call(context.Background(), "sudo", nil)
	got = callErr(t, err)
	if got.Code != "invalid_arguments" || !strings.Contains(got.Message, "sudo") {
		t.Fatal(err)
	}
	_, err = svc.Call(context.Background(), "fleet_overview", json.RawMessage(`{`))
	if callErr(t, err).Code != "invalid_arguments" {
		t.Fatal(err)
	}
	_, err = svc.Call(context.Background(), "machine_get", json.RawMessage(`{}`))
	if callErr(t, err).Code != "invalid_arguments" || len(f.calls) != 0 {
		t.Fatalf("missing id err=%v calls=%v", err, f.calls)
	}
}

func TestHubAPIErrorCodeIsPreserved(t *testing.T) {
	f := &fakeHub{err: &operatorclient.APIError{StatusCode: 404, Code: "MACHINE_NOT_FOUND", Message: "missing"}}
	_, err := (&Service{Hub: f}).Call(context.Background(), "fleet_overview", nil)
	if callErr(t, err).Code != "MACHINE_NOT_FOUND" || callErr(t, err).Message != "missing" {
		t.Fatal(err)
	}
	plain := &fakeHub{err: errors.New("dial failed")}
	_, err = (&Service{Hub: plain}).Call(context.Background(), "fleet_overview", nil)
	if callErr(t, err).Code != "hub_error" || !strings.Contains(callErr(t, err).Message, "dial failed") {
		t.Fatal(err)
	}
}

func TestToolSchemasAndAnnotations(t *testing.T) {
	byName := map[string]Tool{}
	for _, tool := range Tools() {
		if tool.Annotations == nil || tool.Annotations.OpenWorldHint {
			t.Fatalf("%s annotations = %+v", tool.Name, tool.Annotations)
		}
		if tool.InputSchema["additionalProperties"] != false {
			t.Fatalf("%s additionalProperties", tool.Name)
		}
		byName[tool.Name] = tool
	}
	overview := byName["fleet_overview"].Annotations
	if !overview.ReadOnlyHint || overview.DestructiveHint || !overview.IdempotentHint {
		t.Fatalf("overview annotations %+v", overview)
	}
	apply := byName["rollout_apply"].Annotations
	if apply.ReadOnlyHint || !apply.DestructiveHint || !apply.IdempotentHint {
		t.Fatalf("apply annotations %+v", apply)
	}
	enroll := byName["enroll_ticket_create"].Annotations
	if enroll.ReadOnlyHint || enroll.DestructiveHint || !enroll.IdempotentHint {
		t.Fatalf("enroll annotations %+v", enroll)
	}
	props := byName["enroll_ticket_preview"].InputSchema["properties"].(map[string]any)
	ttl := props["ttl_seconds"].(map[string]any)
	if ttl["minimum"] != int(store.OperatorEnrollTokenMinTTLSeconds) || ttl["maximum"] != int(store.OperatorEnrollTokenMaxTTLSeconds) {
		t.Fatalf("ttl = %#v", ttl)
	}
	digest := byName["rollout_apply"].InputSchema["properties"].(map[string]any)["preview_digest"].(map[string]any)
	if digest["pattern"] != digestPattern || digest["minLength"] != 71 || digest["maxLength"] != 71 {
		t.Fatalf("digest = %#v", digest)
	}
	batch := byName["deployment_create"].InputSchema["properties"].(map[string]any)["batch_size"].(map[string]any)
	if batch["maximum"] != store.MaxDeploymentBatchSize || batch["minimum"] != 0 {
		t.Fatalf("batch = %#v", batch)
	}
	rolloutBatch := byName["rollout_apply"].InputSchema["properties"].(map[string]any)["batch_size"].(map[string]any)
	if rolloutBatch["maximum"] != 1 {
		t.Fatalf("rollout batch = %#v", rolloutBatch)
	}
	states := byName["machines_list"].InputSchema["properties"].(map[string]any)["states"].(map[string]any)
	if states["uniqueItems"] != true || states["maxItems"] != len(state.AllStates) {
		t.Fatalf("states = %#v", states)
	}
	encoded, err := json.Marshal(byName["fleet_overview"])
	if err != nil || !bytes.Contains(encoded, []byte(`"openWorldHint":false`)) || !bytes.Contains(encoded, []byte(`"readOnlyHint":true`)) {
		t.Fatalf("encoded=%s err=%v", encoded, err)
	}
}

func TestMCPServerVersionUsesTheBuildVersion(t *testing.T) {
	var in, out bytes.Buffer
	in.WriteString(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}` + "\n")
	if err := Serve(context.Background(), &in, &out, &Service{Hub: &fakeHub{}, Version: "2026.10.6"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"version":"2026.10.6"`) || !strings.Contains(out.String(), `"protocolVersion":"2025-06-18"`) {
		t.Fatalf("initialize = %s", out.String())
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
	if !strings.Contains(lines[0], `"protocolVersion":"2025-03-26"`) || !strings.Contains(lines[0], "WhoIs") || !strings.Contains(lines[0], `"version":"dev"`) {
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
