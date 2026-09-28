package operatorclient

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/deploy"
	"github.com/teddashh/AI-Intune/internal/model"
	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/store"
)

func TestJobEvidenceClientAcceptsCanonicalAndRejectsIncoherent(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Date(2026, 9, 8, 18, 0, 0, 0, time.UTC)
	if err := st.UpsertMachine(store.Machine{
		MachineID: "machine-client-evidence", DisplayName: "client-evidence", Expected: true,
		CreatedAt: now.Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	desiredID, revision, err := st.CreateDesiredState(
		"machine", "machine-client-evidence", "diagnostic", "client-evidence", `{"kind":"noop"}`, "test")
	if err != nil {
		t.Fatal(err)
	}
	jobID, err := st.CreateJob("machine-client-evidence", desiredID, revision, store.NewJob{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`UPDATE jobs
 SET state=?,lease_token=?,lease_expires_at=? WHERE job_id=?`, deploy.Claimed,
		"client-evidence-lease", now.Add(time.Hour).Format(time.RFC3339), jobID); err != nil {
		t.Fatal(err)
	}
	for seq, phase := range []string{"start", "rejected"} {
		at := now.Add(time.Duration(seq+1) * time.Second)
		payload := `{}`
		if phase == "rejected" {
			payload = `{"rejection_code":"STALE_REVISION","detail":"stale"}`
		}
		if _, err := st.DB().Exec(`INSERT INTO job_events
 (event_id,job_id,seq,phase,occurred_at,received_at,payload) VALUES (?,?,?,?,?,?,?)`,
			fmt.Sprintf("event-%d", seq+1), jobID, seq+1, phase,
			at.Format(time.RFC3339Nano), at.Add(time.Second).Format(time.RFC3339Nano), payload); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.DB().Exec(`INSERT INTO verification_results
	 (verification_id,job_id,machine_id,rule_id,command,exit_code,stdout_excerpt,stderr_excerpt,passed,verified_at,
	  producer_kind,producer_id,evidence_role,authority,provenance_recorded,received_at)
	 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, "verification-1", jobID, "machine-client-evidence", "health",
		"true", 0, strings.Repeat("\x00", 6000), "", true, now.Add(time.Minute).Format(time.RFC3339Nano),
		store.JobVerificationProducerExecutorAgent, "machine-client-evidence", store.JobVerificationRoleExecutor,
		store.JobVerificationAuthorityMachineLease, true, now.Add(2*time.Minute).Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	canonical, err := operator.New(st).JobEvidence(operator.JobEvidenceRequest{JobID: jobID}, now)
	if err != nil {
		t.Fatal(err)
	}
	canonicalJSON, err := json.Marshal(canonical)
	if err != nil {
		t.Fatal(err)
	}

	client, requests := jobEvidenceRecorderClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/operator/jobs/"+jobID+"/evidence" || r.URL.RawQuery != "" {
			t.Errorf("request URL=%s", r.URL.String())
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(canonicalJSON)
	})
	got, err := client.JobEvidence(t.Context(), jobID, 0)
	if err != nil || got.JobID != jobID || got.State != deploy.Claimed ||
		got.Disclosure.Limit != operator.JobEvidenceDefaultLimit ||
		len(got.Events.Items) != 2 || got.Rejection == nil ||
		len(got.Verifications.Items[0].StdoutExcerpt.Text) != 16383 ||
		got.Verifications.Items[0].ReceivedAt == nil ||
		!got.Verifications.Items[0].Producer.ProvenanceRecorded {
		t.Fatalf("canonical result=%+v err=%v", got, err)
	}
	if requests.Load() != 1 {
		t.Fatalf("canonical request count=%d want=1", requests.Load())
	}

	hubEvent := canonical
	for i := range hubEvent.Events.Items {
		if hubEvent.Events.Items[i].Phase == operator.JobEventRejected {
			hubEvent.Events.Items[i].Producer = operator.JobEvidenceProducer{
				Kind: operator.JobEvidenceProducerHubScheduler, ProducerID: "hub", DisplayName: "Hub",
				Authority:    operator.JobEvidenceAuthorityDependencyGraph,
				EvidenceRole: operator.JobEvidenceRoleScheduler, ProvenanceRecorded: true,
			}
			hubEvent.Rejection.Producer = hubEvent.Events.Items[i].Producer
		}
	}
	hubEventJSON, err := json.Marshal(hubEvent)
	if err != nil {
		t.Fatal(err)
	}
	client, _ = jobEvidenceRecorderClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(hubEventJSON)
	})
	got, err = client.JobEvidence(t.Context(), jobID, 0)
	if err != nil || got.Rejection == nil ||
		got.Rejection.Producer.Kind != operator.JobEvidenceProducerHubScheduler {
		t.Fatalf("Hub scheduler event result=%+v err=%v", got.Rejection, err)
	}

	var multiline operator.JobEvidenceResult
	if err := json.Unmarshal(canonicalJSON, &multiline); err != nil {
		t.Fatal(err)
	}
	multilineStdout := "ActiveState=active\n\tMainPID=2424956"
	multiline.Verifications.Items[0].StdoutExcerpt = operator.JobEvidenceText{
		Text: multilineStdout, MaxBytes: operator.JobEvidenceMaxFieldBytes,
		Bytes: len(multilineStdout), Issues: []string{},
	}
	multilineJSON, err := json.Marshal(multiline)
	if err != nil {
		t.Fatal(err)
	}
	client, _ = jobEvidenceRecorderClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(multilineJSON)
	})
	got, err = client.JobEvidence(t.Context(), jobID, 0)
	if err != nil || got.Verifications.Items[0].StdoutExcerpt.Text != multilineStdout {
		t.Fatalf("canonical multiline block text=%q err=%v", got.Verifications.Items[0].StdoutExcerpt.Text, err)
	}

	tests := []struct {
		name   string
		mutate func(*operator.JobEvidenceResult)
		raw    func([]byte) []byte
	}{
		{"truncated issue mismatch", func(value *operator.JobEvidenceResult) {
			value.Desired.Spec.Truncated = true
		}, nil},
		{"producer machine mismatch", func(value *operator.JobEvidenceResult) {
			value.Events.Items[0].Producer.ProducerID = "machine-other"
		}, nil},
		{"text over bound", func(value *operator.JobEvidenceResult) {
			value.Desired.Spec.Text = strings.Repeat("x", operator.JobEvidenceMaxFieldBytes+1)
		}, nil},
		{"field max_bytes mismatch", func(value *operator.JobEvidenceResult) {
			value.Verifications.Items[0].RuleID.MaxBytes--
		}, nil},
		{"max_bytes exceeds disclosure", func(value *operator.JobEvidenceResult) {
			value.Desired.Spec.MaxBytes = value.Disclosure.MaxFieldBytes + 1
		}, nil},
		{"issue-free bytes mismatch", func(value *operator.JobEvidenceResult) {
			value.Desired.Spec.Bytes++
		}, nil},
		{"block field escape", func(value *operator.JobEvidenceResult) {
			text := "ok\x1b[31m"
			value.Verifications.Items[0].StdoutExcerpt = operator.JobEvidenceText{
				Text: text, MaxBytes: operator.JobEvidenceMaxFieldBytes, Bytes: len(text), Issues: []string{},
			}
		}, nil},
		{"strict field newline", func(value *operator.JobEvidenceResult) {
			text := "health\ncheck"
			value.Verifications.Items[0].RuleID = operator.JobEvidenceText{
				Text: text, MaxBytes: 256, Bytes: len(text), Issues: []string{},
			}
		}, nil},
		{"rejection seq outside events", func(value *operator.JobEvidenceResult) {
			value.Rejection.Seq = 999
		}, nil},
		{"rejection event mismatch", func(value *operator.JobEvidenceResult) {
			value.Rejection.EventID = "different-event"
		}, nil},
		{"rejection clock mismatch", func(value *operator.JobEvidenceResult) {
			value.Rejection.ReceivedAt = value.Rejection.ReceivedAt.Add(time.Second)
		}, nil},
		{"schema version", func(value *operator.JobEvidenceResult) { value.SchemaVersion = 1 }, nil},
		{"independent verifier", func(value *operator.JobEvidenceResult) {
			value.Disclosure.IndependentVerifier = true
		}, nil},
		{"verification received clock without recorded provenance", func(value *operator.JobEvidenceResult) {
			value.Verifications.Items[0].Producer.ProvenanceRecorded = false
		}, nil},
		{"recorded provenance without verification received clock", func(value *operator.JobEvidenceResult) {
			value.Verifications.Items[0].ReceivedAt = nil
		}, nil},
		{"event authority mismatch", func(value *operator.JobEvidenceResult) {
			value.Events.Items[0].Producer.Authority = operator.JobEvidenceAuthorityDependencyGraph
		}, nil},
		{"event sequence", func(value *operator.JobEvidenceResult) {
			value.Events.Items[1].Seq = value.Events.Items[0].Seq
		}, nil},
		{"unknown field", nil, func(raw []byte) []byte {
			return append(raw[:len(raw)-1], []byte(`,"lease_token":"secret"}`)...)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var value operator.JobEvidenceResult
			if err := json.Unmarshal(canonicalJSON, &value); err != nil {
				t.Fatal(err)
			}
			if test.mutate != nil {
				test.mutate(&value)
			}
			raw, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			if test.raw != nil {
				raw = test.raw(raw)
			}
			client, _ := jobEvidenceRecorderClient(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Cache-Control", "no-store")
				_, _ = w.Write(raw)
			})
			if _, err := client.JobEvidence(t.Context(), jobID, 0); err == nil {
				t.Fatal("incoherent job evidence was accepted")
			}
		})
	}

	client, requests = jobEvidenceRecorderClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	if _, err := client.JobEvidence(t.Context(), jobID, 101); err == nil {
		t.Fatal("limit 101 was accepted")
	}
	if requests.Load() != 0 {
		t.Fatalf("invalid limit performed %d HTTP requests", requests.Load())
	}
}

// jobEvidenceRecorderClient uses httptest's recorder without opening a host
// socket. net.Pipe gives http.Transport a real HTTP exchange while keeping
// pre-I/O validation observable in restricted test sandboxes.
func jobEvidenceRecorderClient(t *testing.T, handler http.HandlerFunc) (*Client, *atomic.Int64) {
	t.Helper()
	requests := &atomic.Int64{}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DialContext = func(_ context.Context, _, _ string) (net.Conn, error) {
		requests.Add(1)
		clientSide, serverSide := net.Pipe()
		go func() {
			defer serverSide.Close()
			request, err := http.ReadRequest(bufio.NewReader(serverSide))
			if err != nil {
				return
			}
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)
			_ = recorder.Result().Write(serverSide)
		}()
		return clientSide, nil
	}
	client, err := NewWithHTTPClient("http://100.64.0.1:8787", &http.Client{Transport: transport})
	if err != nil {
		t.Fatal(err)
	}
	return client, requests
}

// TestJobEvidenceClientAcceptsIndependentEvidenceAndRejectsIncoherentClaims
// covers the second producer's page. The client is a strict reader of a Hub it
// does not trust to be correct: every claim the section makes about itself has
// to be checked against the rows it actually returned.
func TestJobEvidenceClientAcceptsIndependentEvidenceAndRejectsIncoherentClaims(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Date(2026, 9, 8, 18, 0, 0, 0, time.UTC)
	for _, machineID := range []string{"machine-client-independent", "peer-client-machine"} {
		if err := st.UpsertMachine(store.Machine{
			MachineID: machineID, DisplayName: machineID, Expected: true,
			CreatedAt: now.Add(-time.Hour),
		}); err != nil {
			t.Fatal(err)
		}
	}
	desiredID, revision, err := st.CreateDesiredState(
		"machine", "machine-client-independent", "app", "openclaw", `{}`, "test")
	if err != nil {
		t.Fatal(err)
	}
	jobID, err := st.CreateJob("machine-client-independent", desiredID, revision, store.NewJob{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`UPDATE desired_state
 SET resource_kind='openclaw',resource_id='openclaw',spec=? WHERE desired_id=?`,
		`{"kind":"openclaw","version":"2026.9.2"}`, desiredID); err != nil {
		t.Fatal(err)
	}
	const jobDigest = "sha256:3333333333333333333333333333333333333333333333333333333333333333"
	if _, err := st.DB().Exec(`UPDATE jobs SET artifact_digest=? WHERE job_id=?`, jobDigest, jobID); err != nil {
		t.Fatal(err)
	}
	verifier, _, err := st.RegisterVerifier(
		store.VerifierKindFleetPeerAgent, "peer-verifier", "peer-client-machine", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.RecordIndependentVerification(store.IndependentVerificationRequest{
		VerifierID: verifier.VerifierID, JobID: jobID,
		RuleID:  model.IndependentRuleOpenClawCurrentRelease,
		Command: "GET /health", StdoutExcerpt: "ok", ObservedDigest: jobDigest,
		ObservedVersion: "2026.9.2", Passed: true, VerifiedAt: now.Add(-time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordIndependentVerification(store.IndependentVerificationRequest{
		VerifierID: verifier.VerifierID, JobID: jobID,
		RuleID:  model.IndependentRuleOpenClawGatewayHTTP,
		Command: "GET /health", StdoutExcerpt: "ok", ObservedDigest: jobDigest,
		Passed: true, VerifiedAt: now.Add(-time.Minute),
	}); err != nil {
		t.Fatal(err)
	}

	serve := func(t *testing.T, raw []byte) (operator.JobEvidenceResult, error) {
		t.Helper()
		client, _ := jobEvidenceRecorderClient(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "no-store")
			_, _ = w.Write(raw)
		})
		return client.JobEvidence(t.Context(), jobID, 0)
	}
	marshal := func(t *testing.T, value operator.JobEvidenceResult) []byte {
		t.Helper()
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}

	canonical, err := operator.New(st).JobEvidence(operator.JobEvidenceRequest{JobID: jobID}, now)
	if err != nil {
		t.Fatal(err)
	}
	canonicalJSON := marshal(t, canonical)
	currentReleaseRow := func(value *operator.JobEvidenceResult) *operator.JobIndependentVerification {
		for i := range value.Independent.Items {
			if value.Independent.Items[i].RuleID.Text == model.IndependentRuleOpenClawCurrentRelease {
				return &value.Independent.Items[i]
			}
		}
		panic("canonical fixture has no current-release row")
	}
	got, err := serve(t, canonicalJSON)
	if err != nil || got.Independent == nil || !got.Disclosure.IndependentVerifier ||
		got.Independent.Verdict != string(store.IndependentPassed) ||
		got.Independent.Total != 2 || got.Independent.LiveProducers != 1 ||
		len(got.Independent.Items) != 2 ||
		!got.Independent.Items[0].DigestMatchesJob ||
		got.Independent.Items[0].Producer.VerifierID != verifier.VerifierID {
		t.Fatalf("live independent result=%+v err=%v", got.Independent, err)
	}

	// The second coherent shape: the row stays, the producer no longer speaks.
	if err := st.RevokeVerifier(verifier.VerifierID, verifier.Revision, now); err != nil {
		t.Fatal(err)
	}
	revoked, err := operator.New(st).JobEvidence(operator.JobEvidenceRequest{JobID: jobID}, now)
	if err != nil {
		t.Fatal(err)
	}
	got, err = serve(t, marshal(t, revoked))
	if err != nil || got.Disclosure.IndependentVerifier ||
		got.Independent.Verdict != string(store.IndependentProducerRevoked) ||
		got.Independent.Total != 2 || got.Independent.LiveProducers != 0 {
		t.Fatalf("revoked independent result=%+v err=%v", got.Independent, err)
	}

	for _, test := range []struct {
		name   string
		mutate func(*operator.JobEvidenceResult)
	}{
		{"section dropped while the flag still claims a producer", func(value *operator.JobEvidenceResult) {
			value.Independent = nil
		}},
		{"flag denies its own live producer", func(value *operator.JobEvidenceResult) {
			value.Disclosure.IndependentVerifier = false
		}},
		{"unknown verdict", func(value *operator.JobEvidenceResult) {
			value.Independent.Verdict = "verified"
		}},
		{"absent verdict over a returned row", func(value *operator.JobEvidenceResult) {
			value.Independent.Verdict = string(store.IndependentAbsent)
		}},
		{"null items", func(value *operator.JobEvidenceResult) {
			value.Independent.Items = nil
		}},
		{"null assignments", func(value *operator.JobEvidenceResult) {
			value.Independent.Assignments = nil
		}},
		{"assignment state outside the four", func(value *operator.JobEvidenceResult) {
			value.Independent.Assignments = []operator.JobIndependentAssignment{{
				AssignmentID: "assignment-1", VerifierID: "verifier-1",
				State: "done", AssignedAt: value.EvaluatedAt,
			}}
		}},
		{"awaiting_report beside a report time", func(value *operator.JobEvidenceResult) {
			reported := value.EvaluatedAt
			value.Independent.Assignments = []operator.JobIndependentAssignment{{
				AssignmentID: "assignment-1", VerifierID: "verifier-1",
				State:      operator.JobAssignmentAwaitingReport,
				AssignedAt: value.EvaluatedAt, ReportedAt: &reported,
			}}
		}},
		{"reported before it was assigned", func(value *operator.JobEvidenceResult) {
			reported := value.EvaluatedAt.Add(-time.Hour)
			value.Independent.Assignments = []operator.JobIndependentAssignment{{
				AssignmentID: "assignment-1", VerifierID: "verifier-1",
				State:      operator.JobAssignmentReported,
				AssignedAt: value.EvaluatedAt, ReportedAt: &reported,
			}}
		}},
		{"total under the returned rows", func(value *operator.JobEvidenceResult) {
			value.Independent.Total = 0
		}},
		{"live producers over the total", func(value *operator.JobEvidenceResult) {
			value.Independent.LiveProducers = value.Independent.Total + 1
		}},
		{"one verifier's rows claimed as several live producers", func(value *operator.JobEvidenceResult) {
			value.Independent.LiveProducers = value.Independent.Total
		}},
		{"returned producers contradict the live count", func(value *operator.JobEvidenceResult) {
			for i := range value.Independent.Items {
				value.Independent.Items[i].Producer.State = store.VerifierStateRevoked
			}
		}},
		{"no live producer without the producer_revoked verdict", func(value *operator.JobEvidenceResult) {
			for i := range value.Independent.Items {
				value.Independent.Items[i].Producer.State = store.VerifierStateRevoked
			}
			value.Independent.LiveProducers = 0
			value.Disclosure.IndependentVerifier = false
		}},
		{"producer shares the job's failure domain", func(value *operator.JobEvidenceResult) {
			domain := value.Independent.Items[0].Producer.FailureDomain
			domain.Text = value.MachineID
			domain.Bytes = len(domain.Text)
			value.Independent.Items[0].Producer.FailureDomain = domain
		}},
		{"unknown producer kind", func(value *operator.JobEvidenceResult) {
			value.Independent.Items[0].Producer.Kind = "same_host_unit"
		}},
		{"producer state outside the registry states", func(value *operator.JobEvidenceResult) {
			value.Independent.Items[0].Producer.State = "pending"
		}},
		{"executor authority on an independent row", func(value *operator.JobEvidenceResult) {
			value.Independent.Items[0].Producer.Authority = operator.JobEvidenceAuthorityMachineLease
		}},
		{"executor role on an independent row", func(value *operator.JobEvidenceResult) {
			value.Independent.Items[0].Producer.EvidenceRole = operator.JobEvidenceRoleExecutor
		}},
		{"digest match claimed against a different artifact", func(value *operator.JobEvidenceResult) {
			value.Independent.Items[0].ObservedDigest =
				"sha256:4444444444444444444444444444444444444444444444444444444444444444"
		}},
		{"digest reported without a digest", func(value *operator.JobEvidenceResult) {
			value.Independent.Items[0].ObservedDigest = ""
			value.Independent.Items[0].DigestMatchesJob = false
		}},
		{"invalid expected version", func(value *operator.JobEvidenceResult) {
			value.Independent.ExpectedVersion = "releases/2026.9.2"
		}},
		{"observed version on another rule", func(value *operator.JobEvidenceResult) {
			row := currentReleaseRow(value)
			row.RuleID.Text = model.IndependentRuleOpenClawGatewayHTTP
			row.RuleID.Bytes = len(model.IndependentRuleOpenClawGatewayHTTP)
		}},
		{"version reported without a version", func(value *operator.JobEvidenceResult) {
			currentReleaseRow(value).ObservedVersion = ""
		}},
		{"version match claimed without a comparison", func(value *operator.JobEvidenceResult) {
			value.Independent.ExpectedVersion = ""
		}},
		{"version match claimed for another version", func(value *operator.JobEvidenceResult) {
			currentReleaseRow(value).ObservedVersion = "2026.9.1"
		}},
		{"non-canonical artifact digest", func(value *operator.JobEvidenceResult) {
			value.Independent.ArtifactDigest = "sha256:NOTHEX"
		}},
		{"producer clock outside UTC", func(value *operator.JobEvidenceResult) {
			value.Independent.Items[0].ReportedVerifiedAt =
				value.Independent.Items[0].ReportedVerifiedAt.In(time.FixedZone("skew", 3600))
		}},
		{"duplicate verification id", func(value *operator.JobEvidenceResult) {
			value.Independent.Items = append(value.Independent.Items, value.Independent.Items[0])
			value.Independent.Total = 3
			value.Independent.LiveProducers = 1
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var value operator.JobEvidenceResult
			if err := json.Unmarshal(canonicalJSON, &value); err != nil {
				t.Fatal(err)
			}
			test.mutate(&value)
			if _, err := serve(t, marshal(t, value)); err == nil {
				t.Fatal("incoherent independent evidence was accepted")
			}
		})
	}
}

// ⚠⚠ 這裡守的是派工回答狀態與證據權重彼此獨立的 wire contract：
// 先回報、後撤銷必須是 reported + ReportedAt，同時 verdict 可以是 producer_revoked。
// 若 Hub 因為先看 VerifierRevoked 而送出 producer_revoked + ReportedAt，client 必須拒收整頁。
func TestJobEvidenceClientKeepsAnsweredAssignmentSeparateFromRevokedEvidenceWeight(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now().UTC().Truncate(time.Second)
	for _, machineID := range []string{"machine-client-assignment-axis", "peer-client-assignment-axis"} {
		if err := st.UpsertMachine(store.Machine{
			MachineID: machineID, DisplayName: machineID, Expected: true,
			CreatedAt: now.Add(-time.Hour),
		}); err != nil {
			t.Fatal(err)
		}
	}
	desiredID, revision, err := st.CreateDesiredState(
		"machine", "machine-client-assignment-axis", "app", "openclaw", `{}`, "test")
	if err != nil {
		t.Fatal(err)
	}
	jobID, err := st.CreateJob("machine-client-assignment-axis", desiredID, revision, store.NewJob{})
	if err != nil {
		t.Fatal(err)
	}
	const jobDigest = "sha256:6666666666666666666666666666666666666666666666666666666666666666"
	if _, err := st.DB().Exec(`UPDATE jobs SET artifact_digest=? WHERE job_id=?`, jobDigest, jobID); err != nil {
		t.Fatal(err)
	}
	verifier, _, err := st.RegisterVerifier(
		store.VerifierKindFleetPeerAgent, "peer-client-assignment-verifier", "peer-client-assignment-axis", "")
	if err != nil {
		t.Fatal(err)
	}
	preview, err := st.PreviewOperatorVerificationAssignment(verifier.VerifierID, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ApplyOperatorVerificationAssignment(store.OperatorVerificationAssignmentRequest{
		VerifierID: verifier.VerifierID, JobID: jobID, ConfirmVerifierName: preview.VerifierName,
		PreviewDigest: preview.PreviewDigest, Reason: "測試 client 回報後撤銷",
		AssignedBy: "tailscale-user:client-assignment-axis", IdempotencyKey: "client-assignment-axis",
		RequestDigest: "sha256:client-assignment-axis",
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
	page, err := operator.New(st).JobEvidence(operator.JobEvidenceRequest{JobID: jobID}, now)
	if err != nil {
		t.Fatal(err)
	}
	serve := func(t *testing.T, fixture operator.JobEvidenceResult) (operator.JobEvidenceResult, error) {
		t.Helper()
		raw, err := json.Marshal(fixture)
		if err != nil {
			t.Fatal(err)
		}
		client, _ := jobEvidenceRecorderClient(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "no-store")
			_, _ = w.Write(raw)
		})
		return client.JobEvidence(t.Context(), jobID, 0)
	}

	if page.Independent == nil || len(page.Independent.Assignments) != 1 ||
		page.Independent.Assignments[0].ReportedAt == nil ||
		page.Independent.Verdict != string(store.IndependentProducerRevoked) ||
		page.Independent.LiveProducers != 0 || page.Disclosure.IndependentVerifier ||
		page.Independent.Total == 0 {
		t.Fatalf("fixture does not represent report-then-revoke: %+v", page.Independent)
	}
	if _, err := serve(t, page); err != nil {
		t.Fatalf("reported assignment with revoked evidence weight was rejected: %v", err)
	}

	// 不能直接淺拷貝，否則共用的 Independent 指標會連帶污染原本的接受頁面。
	rawPage, err := json.Marshal(page)
	if err != nil {
		t.Fatal(err)
	}
	var contradictory operator.JobEvidenceResult
	if err := json.Unmarshal(rawPage, &contradictory); err != nil {
		t.Fatal(err)
	}
	contradictory.Independent.Assignments[0].State = operator.JobAssignmentProducerRevoked
	if page.Independent.Assignments[0].State != operator.JobAssignmentReported {
		t.Fatalf("contradictory fixture mutated original assignment state: %q", page.Independent.Assignments[0].State)
	}
	if _, err := serve(t, contradictory); err == nil ||
		!strings.Contains(err.Error(), "assignment state contradicts its report time") {
		t.Fatalf("producer_revoked assignment with ReportedAt error=%v, want state/report-time contradiction", err)
	}
	if _, err := serve(t, page); err != nil {
		t.Fatalf("original reported assignment was rejected after contradictory fixture: %v", err)
	}
}
