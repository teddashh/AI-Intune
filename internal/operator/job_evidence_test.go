package operator

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/deploy"
	"github.com/teddashh/AI-Intune/internal/model"
	"github.com/teddashh/AI-Intune/internal/store"
)

func TestJobEvidenceProjectionScrubsTruncatesAndLabelsProvenance(t *testing.T) {
	st := newOperatorJobReadStore(t)
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	if err := st.UpsertMachine(store.Machine{
		MachineID: "machine-evidence", DisplayName: "evidence-machine", Expected: true,
		CreatedAt: now.Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	rawSpec := "spec\xff\x01" + strings.Repeat("x", JobEvidenceMaxFieldBytes)
	desiredID, revision, err := st.CreateDesiredState(
		"machine", "machine-evidence", "diagnostic", "typed-evidence", rawSpec, "private-creator")
	if err != nil {
		t.Fatal(err)
	}
	jobID, err := st.CreateJob("machine-evidence", desiredID, revision, store.NewJob{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`UPDATE jobs
 SET state=?,lease_token=?,lease_expires_at=? WHERE job_id=?`, deploy.Claimed,
		"evidence-lease", now.Add(time.Hour).Format(time.RFC3339), jobID); err != nil {
		t.Fatal(err)
	}
	events := []struct {
		seq     int
		phase   string
		payload string
	}{
		{1, "start", `{}`},
		{2, "rejected", `{"rejection_code":"STALE_REVISION","detail":"unsafe\u0001detail"}`},
		{3, "phase\xff\x01" + strings.Repeat("p", 80), `{ "private": true }`},
	}
	for _, event := range events {
		at := now.Add(time.Duration(event.seq) * time.Second)
		if _, err := st.DB().Exec(`INSERT INTO job_events
 (event_id,job_id,seq,phase,occurred_at,received_at,payload) VALUES (?,?,?,?,?,?,?)`,
			fmt.Sprintf("event-%d", event.seq), jobID, event.seq, event.phase,
			at.Format(time.RFC3339Nano), at.Add(time.Second).Format(time.RFC3339Nano), event.payload); err != nil {
			t.Fatal(err)
		}
	}
	for i := 1; i <= 2; i++ {
		at := now.Add(time.Duration(i) * time.Minute)
		if _, err := st.DB().Exec(`INSERT INTO verification_results
 (verification_id,job_id,machine_id,rule_id,command,exit_code,stdout_excerpt,stderr_excerpt,passed,verified_at)
 VALUES (?,?,?,?,?,?,?,?,?,?)`, fmt.Sprintf("verification-%d", i), jobID, "machine-evidence",
			"rule\xff\x01"+strings.Repeat("r", 300), "command\x01"+strings.Repeat("c", JobEvidenceMaxFieldBytes),
			i, "stdout\xff", "stderr\u2060", i%2 == 0, at.Format(time.RFC3339Nano)); err != nil {
			t.Fatal(err)
		}
	}
	recordedAt := now.Add(2*time.Minute + time.Second)
	if _, err := st.DB().Exec(`UPDATE verification_results
 SET producer_id=machine_id,provenance_recorded=1,received_at=?
 WHERE verification_id='verification-2'`, recordedAt.Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}

	result, err := New(st).JobEvidence(JobEvidenceRequest{JobID: jobID}, now.In(time.FixedZone("test", 3600)))
	if err != nil {
		t.Fatal(err)
	}
	if result.SchemaVersion != JobEvidenceSchemaVersion || result.JobID != jobID ||
		result.MachineID != "machine-evidence" || result.DisplayName != "evidence-machine" ||
		result.State != deploy.Claimed || result.Disclosure.Limit != JobEvidenceDefaultLimit ||
		result.Disclosure.MaxFieldBytes != JobEvidenceMaxFieldBytes ||
		!result.Disclosure.EventProvenanceRecordingEnabled ||
		result.Disclosure.VerificationProducerKind != JobEvidenceProducerExecutorAgent ||
		result.Disclosure.IndependentVerifier ||
		!result.Disclosure.VerificationProvenanceRecordingEnabled ||
		!result.Disclosure.VerificationReceivedAtRecordingEnabled ||
		result.EvaluatedAt.Location() != time.UTC {
		t.Fatalf("evidence envelope=%+v", result)
	}
	assertEvidenceText(t, result.Desired.Spec, JobEvidenceMaxFieldBytes, len(rawSpec), true,
		[]string{"control_or_format_replaced", "invalid_utf8", "truncated"})
	expansionBoundary := projectJobEvidenceText(jobEvidenceDesiredSpec,
		strings.Repeat("a", JobEvidenceMaxFieldBytes-1)+"\x01")
	if !expansionBoundary.Truncated || expansionBoundary.Bytes == 0 {
		t.Fatalf("replacement expansion broke truncation invariant: %+v", expansionBoundary)
	}
	plain := projectJobEvidenceText(jobEvidenceDesiredSpec, "plain evidence")
	if len(plain.Issues) != 0 || plain.Bytes != len(plain.Text) {
		t.Fatalf("issue-free text byte count is inconsistent: %+v", plain)
	}
	controlHeavy := projectJobEvidenceText(jobEvidenceDesiredSpec, strings.Repeat("\x00", 6000))
	if !controlHeavy.Truncated || controlHeavy.Bytes != 6000 || len(controlHeavy.Text) != 16383 {
		t.Fatalf("control-heavy evidence did not fill bounded output: bytes=%d text=%d truncated=%v issues=%v",
			controlHeavy.Bytes, len(controlHeavy.Text), controlHeavy.Truncated, controlHeavy.Issues)
	}
	if len(result.Events.Items) != 3 || result.Events.Truncated || result.Events.Items[2].Phase != JobEventOther {
		t.Fatalf("events=%+v", result.Events)
	}
	assertEvidenceText(t, result.Events.Items[2].ReportedPhase, jobEvidenceReportedPhaseMaxBytes,
		len(events[2].phase), true,
		[]string{"control_or_format_replaced", "invalid_utf8", "truncated"})
	for _, event := range result.Events.Items {
		assertEvidenceProducer(t, event.Producer, result, false)
	}
	if result.Rejection == nil || !result.Rejection.PayloadDecodable ||
		result.Rejection.Code.Text != string(deploy.StaleRevision) || !result.Rejection.CodeKnown ||
		!result.Rejection.HasDetail || result.Rejection.Detail.Text != "unsafe�detail" {
		t.Fatalf("rejection=%+v", result.Rejection)
	}
	assertEvidenceText(t, result.Rejection.Code, jobEvidenceRejectionCodeMaxBytes,
		len(string(deploy.StaleRevision)), false, nil)
	assertEvidenceText(t, result.Rejection.Detail, JobEvidenceMaxFieldBytes,
		len("unsafe\x01detail"), false, []string{"control_or_format_replaced"})
	if len(result.Verifications.Items) != 2 || result.Verifications.Passed != 1 ||
		result.Verifications.Failed != 1 {
		t.Fatalf("verifications=%+v", result.Verifications)
	}
	assertEvidenceText(t, result.Verifications.Items[0].RuleID, jobEvidenceRuleIDMaxBytes,
		len("rule\xff\x01"+strings.Repeat("r", 300)), true,
		[]string{"control_or_format_replaced", "invalid_utf8", "truncated"})
	assertEvidenceText(t, result.Verifications.Items[0].Command, JobEvidenceMaxFieldBytes,
		len("command\x01"+strings.Repeat("c", JobEvidenceMaxFieldBytes)), true,
		[]string{"control_or_format_replaced", "truncated"})
	assertEvidenceText(t, result.Verifications.Items[0].StdoutExcerpt, JobEvidenceMaxFieldBytes,
		len("stdout\xff"), false, []string{"invalid_utf8"})
	assertEvidenceText(t, result.Verifications.Items[0].StderrExcerpt, JobEvidenceMaxFieldBytes,
		len("stderr\u2060"), false, []string{"control_or_format_replaced"})
	for i, verification := range result.Verifications.Items {
		wantRecorded := i == 1
		assertEvidenceProducer(t, verification.Producer, result, wantRecorded)
		if wantRecorded {
			if verification.ReceivedAt == nil || !verification.ReceivedAt.Equal(recordedAt) {
				t.Fatalf("recorded verification clock=%v want=%s", verification.ReceivedAt, recordedAt)
			}
		} else if verification.ReceivedAt != nil {
			t.Fatalf("legacy verification gained received_at=%v", verification.ReceivedAt)
		}
	}

	for _, test := range []struct {
		name      string
		payload   string
		decodable bool
		code      string
		known     bool
		hasDetail bool
	}{
		{"known", `{"rejection_code":"LEASE_INVALID","detail":"why"}`, true, "LEASE_INVALID", true, true},
		{"unknown", `{"rejection_code":"FUTURE_CODE","detail":"why"}`, true, "FUTURE_CODE", false, true},
		{"undecodable", `{"rejection_code":"LEASE_INVALID"} trailing`, false, "", false, false},
		{"without detail", `{"rejection_code":"LEASE_INVALID"}`, true, "LEASE_INVALID", true, false},
	} {
		t.Run("rejection "+test.name, func(t *testing.T) {
			if _, err := st.DB().Exec(`UPDATE job_events SET payload=? WHERE job_id=? AND seq=2`, test.payload, jobID); err != nil {
				t.Fatal(err)
			}
			got, err := New(st).JobEvidence(JobEvidenceRequest{JobID: jobID}, now)
			if err != nil {
				t.Fatal(err)
			}
			if got.Rejection == nil || got.Rejection.PayloadDecodable != test.decodable ||
				got.Rejection.Code.Text != test.code || got.Rejection.CodeKnown != test.known ||
				got.Rejection.HasDetail != test.hasDetail {
				t.Fatalf("rejection=%+v", got.Rejection)
			}
		})
	}

	for _, limit := range []int{1, 100} {
		got, err := New(st).JobEvidence(JobEvidenceRequest{JobID: jobID, Limit: limit}, now)
		if err != nil || got.Disclosure.Limit != limit || len(got.Events.Items) != min(limit, 3) ||
			len(got.Verifications.Items) != min(limit, 2) {
			t.Fatalf("limit=%d result=%+v err=%v", limit, got, err)
		}
	}
	if got, err := New(st).JobEvidence(JobEvidenceRequest{JobID: jobID, Limit: 1}, now); err != nil || got.Rejection != nil {
		t.Fatalf("limit-one rejection=%+v err=%v want nil outside returned window", got.Rejection, err)
	}
	for _, request := range []JobEvidenceRequest{{JobID: jobID, Limit: 101}, {JobID: jobID, Limit: -1}} {
		if _, err := New(st).JobEvidence(request, now); !errors.Is(err, ErrInvalidJobRead) {
			t.Errorf("request=%+v error=%v want ErrInvalidJobRead", request, err)
		}
	}
	if _, err := New(st).JobEvidence(JobEvidenceRequest{JobID: jobID}, time.Time{}); !errors.Is(err, ErrInvalidJobRead) {
		t.Fatalf("zero evaluated_at error=%v want ErrInvalidJobRead", err)
	}
	assertJobEvidenceJSONKeys(t, result)
}

func TestJobEvidenceTextPoliciesPreserveBlockWhitespaceOnly(t *testing.T) {
	raw := "ActiveState=active\n\tMainPID=2424956\x1b[31m\rExecStart=node"
	cleanBlock := projectJobEvidenceText(jobEvidenceVerificationStdout, "line one\n\tline two")
	if cleanBlock.Text != "line one\n\tline two" || len(cleanBlock.Issues) != 0 {
		t.Fatalf("clean block whitespace was altered or reported as replaced: %+v", cleanBlock)
	}

	stdout := projectJobEvidenceText(jobEvidenceVerificationStdout, raw)
	wantStdout := "ActiveState=active\n\tMainPID=2424956�[31m�ExecStart=node"
	if stdout.Text != wantStdout || stdout.Truncated ||
		strings.Join(stdout.Issues, ",") != boundedTextIssueControlOrFormatReplaced {
		t.Fatalf("block stdout=%+v want text=%q", stdout, wantStdout)
	}

	ruleID := projectJobEvidenceText(jobEvidenceVerificationRuleID, raw)
	wantRuleID := "ActiveState=active��MainPID=2424956�[31m�ExecStart=node"
	if ruleID.Text != wantRuleID || ruleID.Truncated ||
		strings.Join(ruleID.Issues, ",") != boundedTextIssueControlOrFormatReplaced {
		t.Fatalf("strict rule_id=%+v want text=%q", ruleID, wantRuleID)
	}
}

func TestJobEvidenceKnownRejectionCodesCoverDeployConstants(t *testing.T) {
	codes := []deploy.RejectionCode{
		deploy.StaleRevision, deploy.DuplicateJobID, deploy.ArtifactHashMismatch,
		deploy.LeaseInvalid, deploy.IrreversibleMigration, deploy.PreconditionFailed,
		deploy.UnspecifiedModelSwitch, deploy.DependencyFailed,
	}
	if len(knownJobEvidenceRejectionCodes) != len(codes) {
		t.Fatalf("known rejection map has %d entries, want %d", len(knownJobEvidenceRejectionCodes), len(codes))
	}
	for _, code := range codes {
		if _, ok := knownJobEvidenceRejectionCodes[string(code)]; !ok {
			t.Errorf("deploy rejection code %q is absent from evidence allowlist", code)
		}
	}
}

func TestJobEvidenceAttributesDependencyRejectionToHubScheduler(t *testing.T) {
	st := newOperatorJobReadStore(t)
	now := time.Date(2026, 9, 9, 10, 0, 0, 0, time.UTC)
	if err := st.UpsertMachine(store.Machine{
		MachineID: "machine-graph", DisplayName: "graph-machine", Expected: true, CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	desiredID, revision, err := st.CreateDesiredState(
		"machine", "machine-graph", "app", "graph", `{}`, "test")
	if err != nil {
		t.Fatal(err)
	}
	root, err := st.CreateJob("machine-graph", desiredID, revision, store.NewJob{})
	if err != nil {
		t.Fatal(err)
	}
	childDesiredID, childRevision, err := st.CreateDesiredState(
		"machine", "machine-graph", "app", "graph", `{}`, "test")
	if err != nil {
		t.Fatal(err)
	}
	child, err := st.CreateJob("machine-graph", childDesiredID, childRevision,
		store.NewJob{PrerequisiteJobIDs: []string{root}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`UPDATE jobs SET state=?,terminal_at=? WHERE job_id=?`,
		deploy.Failed, now.Format(time.RFC3339), root); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := st.NextJobForMachine("machine-graph"); err != nil || ok {
		t.Fatalf("next ok=%t err=%v want dependency cascade and empty queue", ok, err)
	}

	result, err := New(st).JobEvidence(JobEvidenceRequest{JobID: child}, now)
	if err != nil {
		t.Fatal(err)
	}
	if result.Rejection == nil || !result.Rejection.CodeKnown ||
		result.Rejection.Code.Text != string(deploy.DependencyFailed) ||
		result.Rejection.Producer.Kind != JobEvidenceProducerHubScheduler ||
		result.Rejection.Producer.ProducerID != "hub" ||
		result.Rejection.Producer.DisplayName != "Hub" ||
		result.Rejection.Producer.Authority != JobEvidenceAuthorityDependencyGraph ||
		result.Rejection.Producer.EvidenceRole != JobEvidenceRoleScheduler ||
		!result.Rejection.Producer.ProvenanceRecorded {
		t.Fatalf("rejection=%+v", result.Rejection)
	}
}

func TestJobEvidenceWireFormatGolden(t *testing.T) {
	at := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	const wireDigest = "sha256:" + "ab" + "cdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	result := JobEvidenceResult{
		SchemaVersion: JobEvidenceSchemaVersion,
		EvaluatedAt:   at,
		JobID:         "job-wire",
		MachineID:     "machine-wire",
		DisplayName:   "wire-machine",
		State:         deploy.Running,
		Disclosure: JobEvidenceDisclosure{
			Limit: 2, MaxFieldBytes: JobEvidenceMaxFieldBytes,
			EventProvenanceRecordingEnabled:        true,
			VerificationProducerKind:               JobEvidenceProducerExecutorAgent,
			IndependentVerifier:                    true,
			VerificationProvenanceRecordingEnabled: true,
			VerificationReceivedAtRecordingEnabled: true,
		},
		Desired: JobDesiredEvidence{
			DesiredID: "desired-wire", ScopeType: "machine", ScopeID: "machine-wire",
			ResourceKind: "diagnostic", ResourceID: "wire", Revision: 7, CreatedAt: at,
			Spec: EvidenceText{
				Text: "line\nvalue", MaxBytes: JobEvidenceMaxFieldBytes, Bytes: 10, Issues: []string{},
			},
		},
		Events:        JobEventEvidencePage{Items: []JobEventEvidence{}},
		Verifications: JobVerificationEvidencePage{Items: []JobVerificationEvidence{}},
		Independent: &JobIndependentEvidence{
			Verdict: "passed", ArtifactDigest: wireDigest, Total: 1, LiveProducers: 1,
			Items: []JobIndependentVerification{{
				VerificationID: "independent-wire", Passed: true,
				RuleID: EvidenceText{Text: "health", MaxBytes: jobEvidenceRuleIDMaxBytes, Bytes: 6, Issues: []string{}},
				Command: EvidenceText{Text: "GET /health", MaxBytes: JobEvidenceMaxFieldBytes,
					Bytes: 11, Issues: []string{}},
				StdoutExcerpt:  EvidenceText{Text: "ok", MaxBytes: JobEvidenceMaxFieldBytes, Bytes: 2, Issues: []string{}},
				StderrExcerpt:  EvidenceText{MaxBytes: JobEvidenceMaxFieldBytes, Issues: []string{}},
				ObservedDigest: wireDigest, DigestReported: true, DigestMatchesJob: true,
				ReportedVerifiedAt: at, ReceivedAt: at,
				Producer: JobIndependentProducer{
					Kind: "fleet_peer_agent", VerifierID: "verifier-wire",
					DisplayName: EvidenceText{Text: "peer", MaxBytes: jobEvidenceVerifierTextMaxBytes,
						Bytes: 4, Issues: []string{}},
					FailureDomain: EvidenceText{Text: "machine-peer", MaxBytes: jobEvidenceVerifierTextMaxBytes,
						Bytes: 12, Issues: []string{}},
					Authority:    JobEvidenceAuthorityVerifierBearer,
					EvidenceRole: JobEvidenceRoleIndependent, State: "active",
				},
			}},
			Assignments: []JobIndependentAssignment{{
				AssignmentID: "assignment-wire", VerifierID: "verifier-wire",
				DisplayName: EvidenceText{Text: "peer", MaxBytes: jobEvidenceVerifierTextMaxBytes,
					Bytes: 4, Issues: []string{}},
				FailureDomain: EvidenceText{Text: "machine-peer", MaxBytes: jobEvidenceVerifierTextMaxBytes,
					Bytes: 12, Issues: []string{}},
				State: JobAssignmentReported, AssignedAt: at, ReportedAt: &at,
			}},
		},
	}
	got, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"schema_version":6,"evaluated_at":"2026-09-09T12:00:00Z","job_id":"job-wire","machine_id":"machine-wire","display_name":"wire-machine","state":"running","disclosure":{"limit":2,"max_field_bytes":16384,"event_provenance_recording_enabled":true,"verification_producer_kind":"executor_agent","independent_verifier":true,"verification_provenance_recording_enabled":true,"verification_received_at_recording_enabled":true},"desired":{"desired_id":"desired-wire","scope_type":"machine","scope_id":"machine-wire","resource_kind":"diagnostic","resource_id":"wire","revision":7,"created_at":"2026-09-09T12:00:00Z","spec":{"text":"line\nvalue","max_bytes":16384,"bytes":10,"truncated":false,"issues":[]}},"events":{"total":0,"truncated":false,"items":[]},"rejection":null,"verifications":{"total":0,"passed":0,"failed":0,"truncated":false,"items":[]},"independent":{"verdict":"passed","artifact_digest":"sha256:abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789","expected_version":"","terminal_at":null,"total":1,"truncated":false,"live_producers":1,"items":[{"verification_id":"independent-wire","rule_id":{"text":"health","max_bytes":256,"bytes":6,"truncated":false,"issues":[]},"command":{"text":"GET /health","max_bytes":16384,"bytes":11,"truncated":false,"issues":[]},"exit_code":null,"passed":true,"stdout_excerpt":{"text":"ok","max_bytes":16384,"bytes":2,"truncated":false,"issues":[]},"stderr_excerpt":{"text":"","max_bytes":16384,"bytes":0,"truncated":false,"issues":[]},"observed_digest":"sha256:abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789","digest_reported":true,"digest_matches_job":true,"observed_version":"","version_reported":false,"version_matches_job":false,"reported_verified_at":"2026-09-09T12:00:00Z","received_at":"2026-09-09T12:00:00Z","producer":{"kind":"fleet_peer_agent","verifier_id":"verifier-wire","display_name":{"text":"peer","max_bytes":256,"bytes":4,"truncated":false,"issues":[]},"failure_domain":{"text":"machine-peer","max_bytes":256,"bytes":12,"truncated":false,"issues":[]},"authority":"verifier_bearer","evidence_role":"independent_verifier","state":"active"}}],"assignments":[{"assignment_id":"assignment-wire","verifier_id":"verifier-wire","display_name":{"text":"peer","max_bytes":256,"bytes":4,"truncated":false,"issues":[]},"failure_domain":{"text":"machine-peer","max_bytes":256,"bytes":12,"truncated":false,"issues":[]},"state":"reported","assigned_at":"2026-09-09T12:00:00Z","reported_at":"2026-09-09T12:00:00Z"}]}}`

	if string(got) != want {
		t.Fatalf("job evidence JSON wire changed:\n got %s\nwant %s", got, want)
	}
}

func assertEvidenceText(
	t *testing.T,
	got JobEvidenceText,
	wantMaxBytes, wantBytes int,
	truncated bool,
	issues []string,
) {
	t.Helper()
	if got.MaxBytes != wantMaxBytes || got.Bytes != wantBytes || got.Truncated != truncated ||
		!sort.StringsAreSorted(got.Issues) || strings.Join(got.Issues, ",") != strings.Join(issues, ",") ||
		len(got.Text) > got.MaxBytes {
		t.Fatalf("evidence text=%+v want max_bytes=%d bytes=%d truncated=%v issues=%v",
			got, wantMaxBytes, wantBytes, truncated, issues)
	}
}

func assertEvidenceProducer(t *testing.T, got JobEvidenceProducer, result JobEvidenceResult, recorded bool) {
	t.Helper()
	if got.Kind != JobEvidenceProducerExecutorAgent || got.Authority != JobEvidenceAuthorityMachineLease ||
		got.EvidenceRole != JobEvidenceRoleExecutor || got.ProducerID != result.MachineID ||
		got.DisplayName != result.DisplayName || got.ProvenanceRecorded != recorded {
		t.Fatalf("producer=%+v result machine=%q display=%q", got, result.MachineID, result.DisplayName)
	}
}

func assertJobEvidenceJSONKeys(t *testing.T, result JobEvidenceResult) {
	t.Helper()
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		t.Fatal(err)
	}
	assertJSONKeySet(t, top, "schema_version", "evaluated_at", "job_id", "machine_id", "display_name",
		"state", "disclosure", "desired", "events", "rejection", "verifications", "independent")
	var desired map[string]json.RawMessage
	if err := json.Unmarshal(top["desired"], &desired); err != nil {
		t.Fatal(err)
	}
	assertJSONKeySet(t, desired, "desired_id", "scope_type", "scope_id", "resource_kind", "resource_id",
		"revision", "created_at", "spec")
	var text map[string]json.RawMessage
	if err := json.Unmarshal(desired["spec"], &text); err != nil {
		t.Fatal(err)
	}
	assertJSONKeySet(t, text, "text", "max_bytes", "bytes", "truncated", "issues")
	var disclosure map[string]json.RawMessage
	if err := json.Unmarshal(top["disclosure"], &disclosure); err != nil {
		t.Fatal(err)
	}
	assertJSONKeySet(t, disclosure, "limit", "max_field_bytes", "event_provenance_recording_enabled",
		"verification_producer_kind",
		"independent_verifier", "verification_provenance_recording_enabled",
		"verification_received_at_recording_enabled")
	var events map[string]json.RawMessage
	if err := json.Unmarshal(top["events"], &events); err != nil {
		t.Fatal(err)
	}
	assertJSONKeySet(t, events, "total", "truncated", "items")
	var eventItems []map[string]json.RawMessage
	if err := json.Unmarshal(events["items"], &eventItems); err != nil || len(eventItems) == 0 {
		t.Fatalf("event items decode=%v err=%v", eventItems, err)
	}
	assertJSONKeySet(t, eventItems[0], "event_id", "seq", "phase", "reported_phase", "occurred_at",
		"received_at", "payload_bytes", "producer")
	var producer map[string]json.RawMessage
	if err := json.Unmarshal(eventItems[0]["producer"], &producer); err != nil {
		t.Fatal(err)
	}
	assertJSONKeySet(t, producer, "kind", "producer_id", "display_name", "authority", "evidence_role",
		"provenance_recorded")
	var rejection map[string]json.RawMessage
	if err := json.Unmarshal(top["rejection"], &rejection); err != nil {
		t.Fatal(err)
	}
	assertJSONKeySet(t, rejection, "event_id", "seq", "occurred_at", "received_at", "payload_decodable",
		"code", "code_known", "has_detail", "detail", "producer")
	var verifications map[string]json.RawMessage
	if err := json.Unmarshal(top["verifications"], &verifications); err != nil {
		t.Fatal(err)
	}
	assertJSONKeySet(t, verifications, "total", "passed", "failed", "truncated", "items")
	var verificationItems []map[string]json.RawMessage
	if err := json.Unmarshal(verifications["items"], &verificationItems); err != nil || len(verificationItems) == 0 {
		t.Fatalf("verification items decode=%v err=%v", verificationItems, err)
	}
	assertJSONKeySet(t, verificationItems[0], "verification_id", "rule_id", "command", "exit_code", "passed",
		"stdout_excerpt", "stderr_excerpt", "reported_verified_at", "received_at", "producer")
}

func assertJSONKeySet(t *testing.T, got map[string]json.RawMessage, want ...string) {
	t.Helper()
	sort.Strings(want)
	keys := make([]string, 0, len(got))
	for key := range got {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if strings.Join(keys, ",") != strings.Join(want, ",") {
		t.Fatalf("JSON keys=%v want=%v", keys, want)
	}
}

// TestJobEvidenceIndependentSectionFollowsLiveProducersNotRowCount pins the one
// thing a reader of this page acts on: whether anyone other than the executor
// can still speak for the job. The flag is computed from live producers, so a
// page full of revoked evidence never reads as corroborated.
func TestJobEvidenceIndependentSectionFollowsLiveProducersNotRowCount(t *testing.T) {
	st := newOperatorJobReadStore(t)
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	for _, machineID := range []string{"machine-under-test", "peer-machine"} {
		if err := st.UpsertMachine(store.Machine{
			MachineID: machineID, DisplayName: machineID, Expected: true,
			CreatedAt: now.Add(-time.Hour),
		}); err != nil {
			t.Fatal(err)
		}
	}
	desiredID, revision, err := st.CreateDesiredState(
		"machine", "machine-under-test", "app", "openclaw", `{}`, "operator")
	if err != nil {
		t.Fatal(err)
	}
	jobID, err := st.CreateJob("machine-under-test", desiredID, revision, store.NewJob{})
	if err != nil {
		t.Fatal(err)
	}
	const jobDigest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	if _, err := st.DB().Exec(`UPDATE jobs SET artifact_digest=? WHERE job_id=?`, jobDigest, jobID); err != nil {
		t.Fatal(err)
	}

	absent, err := New(st).JobEvidence(JobEvidenceRequest{JobID: jobID}, now)
	if err != nil {
		t.Fatal(err)
	}
	if absent.Independent == nil || absent.Independent.Verdict != string(store.IndependentAbsent) ||
		absent.Independent.Total != 0 || absent.Independent.LiveProducers != 0 ||
		absent.Independent.Truncated || len(absent.Independent.Items) != 0 ||
		absent.Independent.ArtifactDigest != jobDigest ||
		absent.Disclosure.IndependentVerifier {
		t.Fatalf("executor-only independent section=%+v flag=%v",
			absent.Independent, absent.Disclosure.IndependentVerifier)
	}

	verifier, _, err := st.RegisterVerifier(
		store.VerifierKindFleetPeerAgent, "peer-verifier", "peer-machine", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.RecordIndependentVerification(store.IndependentVerificationRequest{
		VerifierID: verifier.VerifierID, JobID: jobID, RuleID: "health",
		Command: "GET /health", StdoutExcerpt: "ok", ObservedDigest: jobDigest,
		Passed: true, VerifiedAt: now.Add(-90 * time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordIndependentVerification(store.IndependentVerificationRequest{
		VerifierID: verifier.VerifierID, JobID: jobID, RuleID: "unit_state",
		Command: "systemctl show", StdoutExcerpt: "active", ObservedDigest: jobDigest,
		Passed: true, VerifiedAt: now.Add(-90 * time.Second),
	}); err != nil {
		t.Fatal(err)
	}

	live, err := New(st).JobEvidence(JobEvidenceRequest{JobID: jobID}, now)
	if err != nil {
		t.Fatal(err)
	}
	if live.Independent.Verdict != string(store.IndependentPassed) ||
		live.Independent.Total != 2 || live.Independent.LiveProducers != 1 ||
		len(live.Independent.Items) != 2 || !live.Disclosure.IndependentVerifier {
		t.Fatalf("independent section=%+v flag=%v", live.Independent, live.Disclosure.IndependentVerifier)
	}
	if len(live.Verifications.Items) != 0 || live.Verifications.Total != 0 {
		t.Fatalf("independent evidence leaked into the executor page: %+v", live.Verifications)
	}
	item := live.Independent.Items[0]
	if !item.DigestReported || !item.DigestMatchesJob || item.ObservedDigest != jobDigest ||
		!item.ReportedVerifiedAt.Equal(now.Add(-90*time.Second)) || item.ReceivedAt.IsZero() ||
		item.ReceivedAt.Equal(item.ReportedVerifiedAt) {
		t.Fatalf("independent item=%+v", item)
	}
	if item.Producer.Kind != store.VerifierKindFleetPeerAgent ||
		item.Producer.VerifierID != verifier.VerifierID ||
		item.Producer.DisplayName.Text != "peer-verifier" ||
		item.Producer.FailureDomain.Text != "peer-machine" ||
		item.Producer.FailureDomain.Text == live.MachineID ||
		item.Producer.Authority != JobEvidenceAuthorityVerifierBearer ||
		item.Producer.EvidenceRole != JobEvidenceRoleIndependent ||
		item.Producer.State != store.VerifierStateActive {
		t.Fatalf("independent producer=%+v", item.Producer)
	}

	if err := st.RevokeVerifier(verifier.VerifierID, verifier.Revision, now); err != nil {
		t.Fatal(err)
	}
	revoked, err := New(st).JobEvidence(JobEvidenceRequest{JobID: jobID}, now)
	if err != nil {
		t.Fatal(err)
	}
	if revoked.Independent.Verdict != string(store.IndependentProducerRevoked) ||
		revoked.Independent.Total != 2 || revoked.Independent.LiveProducers != 0 ||
		len(revoked.Independent.Items) != 2 ||
		revoked.Independent.Items[0].Producer.State != store.VerifierStateRevoked ||
		revoked.Disclosure.IndependentVerifier {
		t.Fatalf("revoked-producer section=%+v flag=%v",
			revoked.Independent, revoked.Disclosure.IndependentVerifier)
	}
}

// ⚠⚠ 「這張派工已經被回答」與「這份證據還有權重」是兩條必須同時成立的獨立軸：
// verifier 回報後被撤銷，派工仍是 reported，但 verdict 必須排除它的權重。
// 若把 VerifierRevoked 排到 ReportedAt 前面，這裡會將派工誤報成 producer_revoked，
// HTTP client 也會依 operatorclient/job_evidence.go:271 因 state 與 ReportedAt 矛盾而拒收整頁。
func TestJobEvidenceKeepsAnsweredAssignmentSeparateFromRevokedEvidenceWeight(t *testing.T) {
	st := newOperatorJobReadStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	for _, machineID := range []string{"machine-assignment-axis", "peer-assignment-axis"} {
		if err := st.UpsertMachine(store.Machine{
			MachineID: machineID, DisplayName: machineID, Expected: true,
			CreatedAt: now.Add(-time.Hour),
		}); err != nil {
			t.Fatal(err)
		}
	}
	desiredID, revision, err := st.CreateDesiredState(
		"machine", "machine-assignment-axis", "app", "openclaw", `{}`, "operator")
	if err != nil {
		t.Fatal(err)
	}
	jobID, err := st.CreateJob("machine-assignment-axis", desiredID, revision, store.NewJob{})
	if err != nil {
		t.Fatal(err)
	}
	const jobDigest = "sha256:5555555555555555555555555555555555555555555555555555555555555555"
	if _, err := st.DB().Exec(`UPDATE jobs SET artifact_digest=? WHERE job_id=?`, jobDigest, jobID); err != nil {
		t.Fatal(err)
	}
	verifier, _, err := st.RegisterVerifier(
		store.VerifierKindFleetPeerAgent, "peer-assignment-verifier", "peer-assignment-axis", "")
	if err != nil {
		t.Fatal(err)
	}
	preview, err := st.PreviewOperatorVerificationAssignment(verifier.VerifierID, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ApplyOperatorVerificationAssignment(store.OperatorVerificationAssignmentRequest{
		VerifierID: verifier.VerifierID, JobID: jobID, ConfirmVerifierName: preview.VerifierName,
		PreviewDigest: preview.PreviewDigest, Reason: "測試回報後撤銷",
		AssignedBy: "tailscale-user:assignment-axis", IdempotencyKey: "job-evidence-assignment-axis",
		RequestDigest: "sha256:job-evidence-assignment-axis",
	}); err != nil {
		t.Fatal(err)
	}
	for _, ruleID := range []string{
		model.IndependentRuleOpenClawCurrentRelease,
		model.IndependentRuleOpenClawGatewayHTTP,
		model.IndependentRuleOpenClawUnitState,
	} {
		request := store.IndependentVerificationRequest{
			VerifierID: verifier.VerifierID, JobID: jobID, RuleID: ruleID,
			Command: "verify " + ruleID, ObservedDigest: jobDigest, Passed: true, VerifiedAt: now,
		}
		if ruleID == model.IndependentRuleOpenClawCurrentRelease {
			request.ObservedVersion = "2026.9.2"
		}
		if err := st.RecordIndependentVerification(request); err != nil {
			t.Fatalf("record %s: %v", ruleID, err)
		}
	}
	if err := st.RevokeVerifier(verifier.VerifierID, verifier.Revision, now); err != nil {
		t.Fatal(err)
	}

	got, err := New(st).JobEvidence(JobEvidenceRequest{JobID: jobID}, now)
	if err != nil {
		t.Fatal(err)
	}
	if got.Independent == nil || len(got.Independent.Assignments) != 1 ||
		len(got.Independent.Items) == 0 {
		t.Fatalf("answered axis and weight axis incomplete: independent=%+v", got.Independent)
	}
	assignment := got.Independent.Assignments[0]
	item := got.Independent.Items[0]
	answered := assignment.State == JobAssignmentReported && assignment.ReportedAt != nil &&
		!assignment.ReportedAt.Before(assignment.AssignedAt)
	weighted := got.Independent.Verdict == string(store.IndependentProducerRevoked) &&
		got.Independent.LiveProducers == 0 && !got.Disclosure.IndependentVerifier &&
		item.Producer.State == store.VerifierStateRevoked
	if !answered || !weighted {
		t.Fatalf("answered axis state=%q assigned_at=%s reported_at=%v; weight axis verdict=%q live_producers=%d disclosure=%v producer_state=%q",
			assignment.State, assignment.AssignedAt, assignment.ReportedAt,
			got.Independent.Verdict, got.Independent.LiveProducers,
			got.Disclosure.IndependentVerifier, item.Producer.State)
	}
}

// TestJobEvidenceIndependentVerdictOutranksItsOwnPassedRows covers the case the
// page exists for: the second producer reports success against a different
// artifact than the one the job deployed.
func TestJobEvidenceIndependentVerdictOutranksItsOwnPassedRows(t *testing.T) {
	st := newOperatorJobReadStore(t)
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	for _, machineID := range []string{"machine-under-test", "peer-machine"} {
		if err := st.UpsertMachine(store.Machine{
			MachineID: machineID, DisplayName: machineID, Expected: true,
			CreatedAt: now.Add(-time.Hour),
		}); err != nil {
			t.Fatal(err)
		}
	}
	desiredID, revision, err := st.CreateDesiredState(
		"machine", "machine-under-test", "app", "openclaw", `{}`, "operator")
	if err != nil {
		t.Fatal(err)
	}
	jobID, err := st.CreateJob("machine-under-test", desiredID, revision, store.NewJob{})
	if err != nil {
		t.Fatal(err)
	}
	const jobDigest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	const otherDigest = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
	if _, err := st.DB().Exec(`UPDATE jobs SET artifact_digest=? WHERE job_id=?`, jobDigest, jobID); err != nil {
		t.Fatal(err)
	}
	verifier, _, err := st.RegisterVerifier(
		store.VerifierKindFleetPeerAgent, "peer-verifier", "peer-machine", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.RecordIndependentVerification(store.IndependentVerificationRequest{
		VerifierID: verifier.VerifierID, JobID: jobID, RuleID: "health",
		Command: "GET /health", ObservedDigest: otherDigest, Passed: true, VerifiedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	result, err := New(st).JobEvidence(JobEvidenceRequest{JobID: jobID}, now)
	if err != nil {
		t.Fatal(err)
	}
	if result.Independent.Verdict != string(store.IndependentDigestMismatch) ||
		!result.Independent.Items[0].Passed ||
		!result.Independent.Items[0].DigestReported ||
		result.Independent.Items[0].DigestMatchesJob ||
		!result.Disclosure.IndependentVerifier {
		t.Fatalf("digest clash section=%+v", result.Independent)
	}
}

func TestJobEvidenceComparesStructuredCurrentReleaseWithDesiredVersion(t *testing.T) {
	for _, test := range []struct {
		name, observed string
		legacyMissing  bool
		wantVerdict    store.IndependentVerdict
		wantMatches    bool
	}{
		{"matching", "2026.9.2", false, store.IndependentPassed, true},
		{"different", "2026.9.1", false, store.IndependentReleaseMismatch, false},
		{"legacy unreported", "2026.9.2", true, store.IndependentReleaseUnreported, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			st := newOperatorJobReadStore(t)
			now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
			for _, machineID := range []string{"release-target", "release-peer"} {
				if err := st.UpsertMachine(store.Machine{
					MachineID: machineID, DisplayName: machineID, Expected: true,
					CreatedAt: now.Add(-time.Hour),
				}); err != nil {
					t.Fatal(err)
				}
			}
			desiredID, revision, err := st.CreateDesiredState("machine", "release-target",
				"app", "openclaw", `{}`, "operator")
			if err != nil {
				t.Fatal(err)
			}
			jobID, err := st.CreateJob("release-target", desiredID, revision, store.NewJob{})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := st.DB().Exec(`UPDATE desired_state
 SET resource_kind='openclaw',resource_id='openclaw',spec=? WHERE desired_id=?`,
				`{"kind":"openclaw","version":"2026.9.2"}`, desiredID); err != nil {
				t.Fatal(err)
			}
			verifier, _, err := st.RegisterVerifier(store.VerifierKindFleetPeerAgent,
				"release-verifier", "release-peer", "")
			if err != nil {
				t.Fatal(err)
			}
			if err := st.RecordIndependentVerification(store.IndependentVerificationRequest{
				VerifierID: verifier.VerifierID, JobID: jobID,
				RuleID:  model.IndependentRuleOpenClawCurrentRelease,
				Command: "readlink current", ObservedVersion: test.observed,
				Passed: true, VerifiedAt: now,
			}); err != nil {
				t.Fatal(err)
			}
			if test.legacyMissing {
				if _, err := st.DB().Exec(`UPDATE verification_results SET observed_version=''
 WHERE job_id=?`, jobID); err != nil {
					t.Fatal(err)
				}
			}
			result, err := New(st).JobEvidence(JobEvidenceRequest{JobID: jobID}, now)
			if err != nil {
				t.Fatal(err)
			}
			row := result.Independent.Items[0]
			if result.Independent.ExpectedVersion != "2026.9.2" ||
				result.Independent.Verdict != string(test.wantVerdict) ||
				row.VersionReported != !test.legacyMissing ||
				row.VersionMatchesJob != test.wantMatches {
				t.Fatalf("independent=%+v row=%+v", result.Independent, row)
			}
		})
	}
}

func TestEveryAssignmentStateNamesWhoOwesAnAnswer(t *testing.T) {
	tests := []struct {
		name      string
		suffix    string
		terminal  bool
		revoked   bool
		reported  bool
		wantState string
	}{
		{
			name: "指派了但工作單還沒終態", suffix: "waiting-for-job",
			wantState: JobAssignmentWaitingForJob,
		},
		{
			name: "工作單終態了還沒有人回報", suffix: "awaiting-report",
			terminal: true, wantState: JobAssignmentAwaitingReport,
		},
		{
			name: "verifier 被撤銷而且一份證據都沒有", suffix: "producer-revoked",
			revoked: true, wantState: JobAssignmentProducerRevoked,
		},
		{
			name: "回報過，即使工作單還沒終態", suffix: "reported",
			reported: true, wantState: JobAssignmentReported,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			st := newOperatorJobReadStore(t)
			now := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
			machineID := "machine-assignment-state-" + test.suffix
			peerID := "peer-assignment-state-" + test.suffix
			for _, id := range []string{machineID, peerID} {
				if err := st.UpsertMachine(store.Machine{
					MachineID: id, DisplayName: id, Expected: true,
					CreatedAt: now.Add(-time.Hour),
				}); err != nil {
					t.Fatal(err)
				}
			}
			desiredID, revision, err := st.CreateDesiredState(
				"machine", machineID, "app", "openclaw", `{}`, "operator")
			if err != nil {
				t.Fatal(err)
			}
			jobID, err := st.CreateJob(machineID, desiredID, revision, store.NewJob{})
			if err != nil {
				t.Fatal(err)
			}
			const jobDigest = "sha256:8888888888888888888888888888888888888888888888888888888888888888"
			if _, err := st.DB().Exec(`UPDATE jobs SET artifact_digest=? WHERE job_id=?`, jobDigest, jobID); err != nil {
				t.Fatal(err)
			}
			verifier, _, err := st.RegisterVerifier(
				store.VerifierKindFleetPeerAgent, "assignment-verifier-"+test.suffix, peerID, "")
			if err != nil {
				t.Fatal(err)
			}
			preview, err := st.PreviewOperatorVerificationAssignment(verifier.VerifierID, jobID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := st.ApplyOperatorVerificationAssignment(store.OperatorVerificationAssignmentRequest{
				VerifierID: verifier.VerifierID, JobID: jobID, ConfirmVerifierName: preview.VerifierName,
				PreviewDigest: preview.PreviewDigest, Reason: "測試派工狀態",
				AssignedBy: "tailscale-user:" + test.suffix, IdempotencyKey: "assignment-state-" + test.suffix,
				RequestDigest: "sha256:assignment-state-" + test.suffix,
			}); err != nil {
				t.Fatal(err)
			}

			if test.terminal {
				if _, err := st.DB().Exec(`UPDATE jobs SET state=?,terminal_at=? WHERE job_id=?`,
					deploy.Failed, now.Format(time.RFC3339), jobID); err != nil {
					t.Fatal(err)
				}
			}
			if test.revoked {
				// 工作單刻意維持未終態，釘住撤銷優先於「還沒終態」。
				if err := st.RevokeVerifier(verifier.VerifierID, verifier.Revision, now); err != nil {
					t.Fatal(err)
				}
			}
			if test.reported {
				// 工作單刻意維持未終態，釘住已回報優先於「還沒終態」。
				for _, ruleID := range []string{
					model.IndependentRuleOpenClawCurrentRelease,
					model.IndependentRuleOpenClawGatewayHTTP,
					model.IndependentRuleOpenClawUnitState,
				} {
					request := store.IndependentVerificationRequest{
						VerifierID: verifier.VerifierID, JobID: jobID, RuleID: ruleID,
						Command: "verify " + ruleID, ObservedDigest: jobDigest, Passed: true, VerifiedAt: now,
					}
					if ruleID == model.IndependentRuleOpenClawCurrentRelease {
						request.ObservedVersion = "2026.9.2"
					}
					if err := st.RecordIndependentVerification(request); err != nil {
						t.Fatalf("record %s: %v", ruleID, err)
					}
				}
			}

			got, err := New(st).JobEvidence(JobEvidenceRequest{JobID: jobID}, now)
			if err != nil {
				t.Fatal(err)
			}
			if got.Independent == nil || len(got.Independent.Assignments) != 1 {
				t.Fatalf("independent assignments=%+v", got.Independent)
			}
			if state := got.Independent.Assignments[0].State; state != test.wantState {
				t.Errorf("assignment state=%q, want %q", state, test.wantState)
			}
		})
	}
}
