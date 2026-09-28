package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/teddashh/AI-Intune/internal/deploy"
	"github.com/teddashh/AI-Intune/internal/model"
	"github.com/teddashh/AI-Intune/internal/store"
)

const verifierAPITestDigest = "sha256:" +
	"abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"

func registerAPITestVerifier(t *testing.T, f jobsFixture, name, domain string) (store.Verifier, string) {
	t.Helper()
	if _, err := f.store.DB().Exec(`INSERT INTO machine_registry
 (machine_id,display_name,created_at) VALUES (?,?,?)`,
		domain, domain, jobsTestNow.UTC().Format("2006-01-02T15:04:05Z")); err != nil {
		t.Fatalf("register verifier domain machine: %v", err)
	}
	verifier, credential, err := f.store.RegisterVerifier(
		store.VerifierKindFleetPeerAgent, name, domain, "")
	if err != nil {
		t.Fatalf("register verifier: %v", err)
	}
	return verifier, credential
}

// The two producer planes share a bearer header and nothing else. A credential
// accepted by the wrong plane would let one producer impersonate the other.
func TestProducerPlanesRejectEachOthersCredentials(t *testing.T) {
	f := newJobsFixture(t, "plane-machine")
	jobID := f.newJob(t, verifierAPITestDigest)
	_, credential := registerAPITestVerifier(t, f, "plane-verifier", "plane-peer")

	independent := model.IndependentVerificationRequest{
		SchemaVersion: model.IndependentVerificationSchemaVersion, JobID: jobID, RuleID: "health",
		Command: "GET /health", Passed: true, VerifiedAt: jobsTestNow,
	}
	executor := model.JobVerificationRequest{
		RuleID: "health", Command: "GET /health", Passed: true, VerifiedAt: jobsTestNow,
	}

	for _, test := range []struct {
		name   string
		method string
		path   string
		token  string
		body   any
		want   int
		detail string
	}{
		{"machine bearer on the verifier plane", http.MethodPost, "/v1/verifications",
			f.machine.token, independent, http.StatusUnauthorized, "token 無效"},
		{"verifier bearer on the machine plane", http.MethodPost,
			"/v1/jobs/" + jobID + "/verifications", credential, executor,
			http.StatusUnauthorized, "token 無效"},
		{"no bearer on the verifier plane", http.MethodPost, "/v1/verifications",
			"", independent, http.StatusUnauthorized, "缺少 bearer token"},
	} {
		t.Run(test.name, func(t *testing.T) {
			rec := requestJobAPI(t, f.mux, test.method, test.path, test.token, test.body)
			if rec.Code != test.want {
				t.Fatalf("status=%d want %d body=%s", rec.Code, test.want, rec.Body.String())
			}
			var response model.APIError
			if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
				t.Fatalf("decode error body: %v", err)
			}
			// Presenting the wrong plane's credential gets exactly the message a
			// wrong credential gets: never say which plane it belongs to.
			if response.Code != model.ErrUnauthorized || response.Message != test.detail {
				t.Fatalf("error=%+v want UNAUTHORIZED %q", response, test.detail)
			}
		})
	}

	rows, err := f.store.JobVerifications(jobID)
	if err != nil || len(rows) != 0 {
		t.Fatalf("rejected cross-plane writes left %d rows err=%v", len(rows), err)
	}
}

func TestVerifierPlaneWritesIndependentEvidenceAndRefusesItsOwnDomain(t *testing.T) {
	f := newJobsFixture(t, "domain-machine")
	jobID := f.newJob(t, verifierAPITestDigest)
	_, credential := registerAPITestVerifier(t, f, "domain-verifier", "domain-peer")

	body := model.IndependentVerificationRequest{
		SchemaVersion: model.IndependentVerificationSchemaVersion, JobID: jobID,
		RuleID:  model.IndependentRuleOpenClawCurrentRelease,
		Command: "GET /health", StdoutExcerpt: "ok", ObservedDigest: verifierAPITestDigest,
		ObservedVersion: "2026.9.2", Passed: true, VerifiedAt: jobsTestNow,
	}
	if rec := requestJobAPI(t, f.mux, http.MethodPost, "/v1/verifications",
		credential, body); rec.Code != http.StatusCreated {
		t.Fatalf("independent write status=%d body=%s", rec.Code, rec.Body.String())
	}
	rows, err := f.store.JobVerifications(jobID)
	if err != nil || len(rows) != 1 {
		t.Fatalf("independent rows=%d err=%v", len(rows), err)
	}
	if rows[0].EvidenceRole != store.JobVerificationRoleIndependent ||
		rows[0].Authority != store.JobVerificationAuthorityVerifierBearer ||
		rows[0].MachineID != f.machine.id || rows[0].ObservedDigest != verifierAPITestDigest ||
		rows[0].ObservedVersion != "2026.9.2" {
		t.Fatalf("independent provenance: %+v", rows[0])
	}

	// A verifier whose failure domain is the job's own machine is not
	// independent of it, so the write must be refused rather than recorded.
	colocated, colocatedCredential, err := f.store.RegisterVerifier(
		store.VerifierKindFleetPeerAgent, "co-located-verifier", f.machine.id, "")
	if err != nil {
		t.Fatalf("register co-located verifier: %v", err)
	}
	if rec := requestJobAPI(t, f.mux, http.MethodPost, "/v1/verifications",
		colocatedCredential, body); rec.Code != http.StatusForbidden {
		t.Fatalf("co-located write status=%d body=%s", rec.Code, rec.Body.String())
	}
	rows, err = f.store.JobVerifications(jobID)
	if err != nil || len(rows) != 1 {
		t.Fatalf("co-located verifier %s wrote a row: rows=%d err=%v", colocated.VerifierID, len(rows), err)
	}
}

func TestVerifierPlaneRejectsUnknownSchemaAndMissingJob(t *testing.T) {
	f := newJobsFixture(t, "schema-machine")
	jobID := f.newJob(t, verifierAPITestDigest)
	_, credential := registerAPITestVerifier(t, f, "schema-verifier", "schema-peer")

	for _, test := range []struct {
		name string
		body model.IndependentVerificationRequest
		want int
	}{
		{"unknown schema version", model.IndependentVerificationRequest{
			SchemaVersion: model.IndependentVerificationSchemaVersion + 1, JobID: jobID, RuleID: "health",
			Command: "GET /health", Passed: true, VerifiedAt: jobsTestNow,
		}, http.StatusBadRequest},
		{"legacy schema version", model.IndependentVerificationRequest{
			SchemaVersion: model.SchemaVersion, JobID: jobID, RuleID: "health",
			Command: "GET /health", Passed: true, VerifiedAt: jobsTestNow,
		}, http.StatusBadRequest},
		{"unknown job", model.IndependentVerificationRequest{
			SchemaVersion: model.IndependentVerificationSchemaVersion, JobID: "00000000000000000000000000000000",
			RuleID: "health", Command: "GET /health", Passed: true, VerifiedAt: jobsTestNow,
		}, http.StatusNotFound},
		{"non-canonical observed digest", model.IndependentVerificationRequest{
			SchemaVersion: model.IndependentVerificationSchemaVersion, JobID: jobID, RuleID: "health",
			Command: "GET /health", ObservedDigest: "sha256:NOTHEX", Passed: true,
			VerifiedAt: jobsTestNow,
		}, http.StatusBadRequest},
		{"passed current release without observed version", model.IndependentVerificationRequest{
			SchemaVersion: model.IndependentVerificationSchemaVersion, JobID: jobID,
			RuleID:  model.IndependentRuleOpenClawCurrentRelease,
			Command: "readlink current", Passed: true, VerifiedAt: jobsTestNow,
		}, http.StatusBadRequest},
		{"observed version on another rule", model.IndependentVerificationRequest{
			SchemaVersion: model.IndependentVerificationSchemaVersion, JobID: jobID,
			RuleID:  model.IndependentRuleOpenClawGatewayHTTP,
			Command: "GET /health", ObservedVersion: "2026.9.2", Passed: true,
			VerifiedAt: jobsTestNow,
		}, http.StatusBadRequest},
	} {
		t.Run(test.name, func(t *testing.T) {
			rec := requestJobAPI(t, f.mux, http.MethodPost, "/v1/verifications", credential, test.body)
			if rec.Code != test.want {
				t.Fatalf("status=%d want %d body=%s", rec.Code, test.want, rec.Body.String())
			}
		})
	}
	rows, err := f.store.JobVerifications(jobID)
	if err != nil || len(rows) != 0 {
		t.Fatalf("rejected writes left %d rows err=%v", len(rows), err)
	}
}

// An independent producer must not be able to decide a deployment. Neither a
// pass nor a failure from this plane may move the job out of verifying.
func TestIndependentEvidenceCannotDecideTheDeploymentGate(t *testing.T) {
	for _, passed := range []bool{true, false} {
		name := "independent pass"
		if !passed {
			name = "independent failure"
		}
		t.Run(name, func(t *testing.T) {
			f := newJobsFixture(t, "gate-machine")
			jobID := f.newJob(t, verifierAPITestDigest)
			_, credential := registerAPITestVerifier(t, f, "gate-verifier", "gate-peer")

			lease := claimJobViaHTTP(t, f, jobID)
			if rec := requestJobAPI(t, f.mux, http.MethodPost, "/v1/jobs/"+jobID+"/events",
				f.machine.token, model.JobEventRequest{
					LeaseToken: lease.LeaseToken, Seq: 1, Phase: "start",
					OccurredAt: jobsTestNow,
				}); rec.Code != http.StatusAccepted {
				t.Fatalf("start event status=%d body=%s", rec.Code, rec.Body.String())
			}
			if rec := requestJobAPI(t, f.mux, http.MethodPost, "/v1/jobs/"+jobID+"/complete",
				f.machine.token, model.JobCompleteRequest{LeaseToken: lease.LeaseToken}); rec.Code == http.StatusOK {
				t.Fatalf("job completed with no executor evidence: %s", rec.Body.String())
			}
			if rec := requestJobAPI(t, f.mux, http.MethodPost, "/v1/verifications", credential,
				model.IndependentVerificationRequest{
					SchemaVersion: model.IndependentVerificationSchemaVersion, JobID: jobID, RuleID: "health",
					Command: "GET /health", ObservedDigest: verifierAPITestDigest,
					Passed: passed, VerifiedAt: jobsTestNow,
				}); rec.Code != http.StatusCreated {
				t.Fatalf("independent write status=%d body=%s", rec.Code, rec.Body.String())
			}
			// No executor evidence exists, so the job must still be refused.
			if _, err := f.store.MarkSucceededIfVerified(jobID, jobsTestNow); err == nil {
				t.Fatal("independent evidence alone moved the job to succeeded")
			}
			job, err := f.store.Job(jobID)
			if err != nil {
				t.Fatal(err)
			}
			if job.State != deploy.Verifying {
				t.Fatalf("job state=%q want verifying", job.State)
			}
		})
	}
}

// assignAPITestJob goes through the operator preview/apply pair so the HTTP
// test cannot pass against a hand-out that disagrees with how assignments are
// actually made.
func assignAPITestJob(t *testing.T, f jobsFixture, verifierID, jobID, key string) {
	t.Helper()
	preview, err := f.store.PreviewOperatorVerificationAssignment(verifierID, jobID)
	if err != nil {
		t.Fatalf("preview assignment: %v", err)
	}
	if _, err := f.store.ApplyOperatorVerificationAssignment(store.OperatorVerificationAssignmentRequest{
		VerifierID: verifierID, JobID: jobID, ConfirmVerifierName: preview.VerifierName,
		PreviewDigest: preview.PreviewDigest,
		AssignedBy:    "tailscale-user:1", IdempotencyKey: key, RequestDigest: "sha256:" + key,
	}); err != nil {
		t.Fatalf("apply assignment: %v", err)
	}
	if _, err := f.store.DB().Exec(`UPDATE jobs SET state=?,terminal_at=? WHERE job_id=?`,
		deploy.Succeeded, jobsTestNow.UTC().Format("2006-01-02T15:04:05Z"), jobID); err != nil {
		t.Fatalf("finish assigned job: %v", err)
	}
}

// TestVerifierPlaneHandsOutOnlyAssignedWork is the security property the
// assignment table exists for. Eligibility under the separation rule covers the
// whole fleet minus one machine; the plane must expose only what was handed
// over, so a stolen verifier bearer cannot enumerate the fleet's jobs.
func TestVerifierPlaneHandsOutOnlyAssignedWork(t *testing.T) {
	f := newJobsFixture(t, "handout-machine")
	assigned := f.newJob(t, verifierAPITestDigest)
	unassigned := f.newJob(t, verifierAPITestDigest)
	verifier, credential := registerAPITestVerifier(t, f, "handout-verifier", "handout-peer")
	_, otherCredential := registerAPITestVerifier(t, f, "idle-verifier", "idle-peer")
	assignAPITestJob(t, f, verifier.VerifierID, assigned, "key-handout")
	if _, err := f.store.DB().Exec(`UPDATE jobs SET state=?,terminal_at=? WHERE job_id=?`,
		deploy.Succeeded, jobsTestNow.UTC().Format("2006-01-02T15:04:05Z"), unassigned); err != nil {
		t.Fatalf("finish unassigned job: %v", err)
	}

	rec := requestJobAPI(t, f.mux, http.MethodGet, "/v1/verification-assignments", credential, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("handout status=%d body=%s", rec.Code, rec.Body.String())
	}
	var response model.VerificationAssignmentsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode handout: %v", err)
	}
	if response.SchemaVersion != model.SchemaVersion || len(response.Assignments) != 1 ||
		response.Assignments[0].JobID != assigned ||
		response.Assignments[0].MachineID != f.machine.id {
		t.Fatalf("handout=%+v want only %s", response, assigned)
	}

	// The other job on the same machine is equally eligible and was not handed
	// over, so it must not appear for anybody.
	body := rec.Body.String()
	if strings.Contains(body, unassigned) {
		t.Fatalf("未指派的工作單不可以出現在 handout：%s", body)
	}
	// The plane names jobs. It must never carry the commands to run them.
	for _, banned := range []string{"command", "rule_id", "artifact_digest", "spec"} {
		if strings.Contains(body, banned) {
			t.Fatalf("handout 不可以帶 %q：%s", banned, body)
		}
	}

	idle := requestJobAPI(t, f.mux, http.MethodGet, "/v1/verification-assignments", otherCredential, nil)
	if idle.Code != http.StatusOK {
		t.Fatalf("idle handout status=%d body=%s", idle.Code, idle.Body.String())
	}
	var idleResponse model.VerificationAssignmentsResponse
	if err := json.Unmarshal(idle.Body.Bytes(), &idleResponse); err != nil {
		t.Fatalf("decode idle handout: %v", err)
	}
	if len(idleResponse.Assignments) != 0 {
		t.Fatalf("沒有被指派的 verifier 不該拿到工作單：%+v", idleResponse.Assignments)
	}
}

// TestVerificationAssignmentHandoutRejectsTheOtherPlanesCredential keeps the
// read on the same credential boundary as the write.
func TestVerificationAssignmentHandoutRejectsTheOtherPlanesCredential(t *testing.T) {
	f := newJobsFixture(t, "handout-boundary-machine")
	for _, test := range []struct {
		name, token, detail string
	}{
		{"machine bearer", f.machine.token, "token 無效"},
		{"no bearer", "", "缺少 bearer token"},
	} {
		t.Run(test.name, func(t *testing.T) {
			rec := requestJobAPI(t, f.mux, http.MethodGet, "/v1/verification-assignments", test.token, nil)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status=%d want 401 body=%s", rec.Code, rec.Body.String())
			}
			var response model.APIError
			if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
				t.Fatalf("decode error body: %v", err)
			}
			if response.Code != model.ErrUnauthorized || response.Message != test.detail {
				t.Fatalf("error=%+v want UNAUTHORIZED %q", response, test.detail)
			}
		})
	}
}
