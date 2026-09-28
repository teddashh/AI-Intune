package operatorclient

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/store"
)

const (
	assignmentClientVerifier = "verifier-assignment"
	assignmentClientJob      = "job-assignment"
	assignmentClientDigest   = "sha256:" +
		"1111111111111111111111111111111111111111111111111111111111111111"
)

func canonicalAssignmentPreview() store.OperatorVerificationAssignmentPreviewResult {
	return store.OperatorVerificationAssignmentPreviewResult{
		VerifierID: assignmentClientVerifier, VerifierName: "onode-peer",
		VerifierKind: store.VerifierKindFleetPeerAgent, FailureDomain: "machine-onode",
		JobID: assignmentClientJob, MachineID: "machine-cnode", MachineName: "samplehub1",
		JobState:                   "succeeded",
		PreviewedAt:                time.Date(2026, 9, 12, 1, 0, 0, 0, time.UTC),
		SeparationRule:             store.OperatorVerifierSeparationRule,
		SatisfiedBy:                store.VerificationAssignmentSatisfiedBy,
		HandoutRequiresTerminalJob: true,
		CommandsSuppliedByHub:      false,
		GrantsDeploymentGate:       true,
		PreviewDigest:              assignmentClientDigest,
	}
}

func assignmentClient(t *testing.T, status int, body any) *Client {
	t.Helper()
	client, _ := jobEvidenceRecorderClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	})
	return client
}

// TestAssignmentPreviewClientRecomputesTheSeparationRule keeps the client from
// taking the Hub's word for the one rule that makes this evidence independent.
func TestAssignmentPreviewClientRecomputesTheSeparationRule(t *testing.T) {
	got, err := assignmentClient(t, http.StatusOK, canonicalAssignmentPreview()).
		PreviewVerificationAssignment(t.Context(), assignmentClientVerifier, assignmentClientJob)
	if err != nil || got.PreviewDigest != assignmentClientDigest {
		t.Fatalf("canonical preview rejected: %+v err=%v", got, err)
	}

	for _, test := range []struct {
		name   string
		mutate func(*store.OperatorVerificationAssignmentPreviewResult)
	}{
		{"verifier shares the job's machine", func(p *store.OperatorVerificationAssignmentPreviewResult) {
			p.FailureDomain = p.MachineID
		}},
		{"separation rule renamed", func(p *store.OperatorVerificationAssignmentPreviewResult) {
			p.SeparationRule = "trust_the_hub"
		}},
		{"done redefined", func(p *store.OperatorVerificationAssignmentPreviewResult) {
			p.SatisfiedBy = "runner_said_so"
		}},
		{"hand-out no longer waits for the job", func(p *store.OperatorVerificationAssignmentPreviewResult) {
			p.HandoutRequiresTerminalJob = false
		}},
		{"hub supplies the commands", func(p *store.OperatorVerificationAssignmentPreviewResult) {
			p.CommandsSuppliedByHub = true
		}},
		{"fleet-peer assignment hides its deployment gate", func(p *store.OperatorVerificationAssignmentPreviewResult) {
			p.GrantsDeploymentGate = false
		}},
		{"unknown verifier kind", func(p *store.OperatorVerificationAssignmentPreviewResult) {
			p.VerifierKind = "same_host_unit"
		}},
		{"preview describes another job", func(p *store.OperatorVerificationAssignmentPreviewResult) {
			p.JobID = "job-other"
		}},
		{"preview describes another verifier", func(p *store.OperatorVerificationAssignmentPreviewResult) {
			p.VerifierID = "verifier-other"
		}},
		{"digest is not canonical", func(p *store.OperatorVerificationAssignmentPreviewResult) {
			p.PreviewDigest = "sha256:short"
		}},
		{"previewed_at is not UTC", func(p *store.OperatorVerificationAssignmentPreviewResult) {
			p.PreviewedAt = p.PreviewedAt.In(time.FixedZone("EDT", -4*3600))
		}},
		{"job state missing", func(p *store.OperatorVerificationAssignmentPreviewResult) {
			p.JobState = ""
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			preview := canonicalAssignmentPreview()
			test.mutate(&preview)
			if _, err := assignmentClient(t, http.StatusOK, preview).PreviewVerificationAssignment(
				t.Context(), assignmentClientVerifier, assignmentClientJob); err == nil {
				t.Fatal("預期被拒絕，但接受了")
			}
		})
	}
}

func canonicalAssignmentReceipt() store.OperatorVerificationAssignmentResult {
	return store.OperatorVerificationAssignmentResult{
		AssignmentID: "assignment-1", VerifierID: assignmentClientVerifier,
		VerifierName: "onode-peer",
		JobID:        assignmentClientJob, MachineID: "machine-cnode",
		AssignedAt: time.Date(2026, 9, 12, 1, 0, 0, 0, time.UTC),
		AssignedBy: "tailscale-user:1", PreviewDigest: assignmentClientDigest,
	}
}

func TestAssignmentClientRefusesAReceiptThatDoesNotMatchTheRequest(t *testing.T) {
	body := VerificationAssignmentRequest{
		JobID: assignmentClientJob, ConfirmVerifierName: "onode-peer",
		PreviewDigest: assignmentClientDigest, Reason: "第一次演練",
	}
	got, err := assignmentClient(t, http.StatusCreated, canonicalAssignmentReceipt()).
		AssignVerification(t.Context(), assignmentClientVerifier, "key-1", body)
	if err != nil || got.AssignmentID != "assignment-1" {
		t.Fatalf("canonical receipt rejected: %+v err=%v", got, err)
	}

	for _, test := range []struct {
		name   string
		status int
		mutate func(*store.OperatorVerificationAssignmentResult)
	}{
		{"receipt names another job", http.StatusCreated,
			func(r *store.OperatorVerificationAssignmentResult) { r.JobID = "job-other" }},
		{"receipt names another verifier", http.StatusCreated,
			func(r *store.OperatorVerificationAssignmentResult) { r.VerifierID = "verifier-other" }},
		{"receipt answers a different preview", http.StatusCreated,
			func(r *store.OperatorVerificationAssignmentResult) {
				r.PreviewDigest = "sha256:" +
					"2222222222222222222222222222222222222222222222222222222222222222"
			}},
		{"receipt has no assignment identity", http.StatusCreated,
			func(r *store.OperatorVerificationAssignmentResult) { r.AssignmentID = "" }},
		{"receipt has no machine", http.StatusCreated,
			func(r *store.OperatorVerificationAssignmentResult) { r.MachineID = "" }},
		{"receipt has no author", http.StatusCreated,
			func(r *store.OperatorVerificationAssignmentResult) { r.AssignedBy = "" }},
		{"assigned_at is not UTC", http.StatusCreated,
			func(r *store.OperatorVerificationAssignmentResult) {
				r.AssignedAt = r.AssignedAt.In(time.FixedZone("EDT", -4*3600))
			}},
		// 201 means a row was created; a replay must arrive as 200 with the
		// header, so the two must never disagree.
		{"replay claimed on a created response", http.StatusCreated,
			func(r *store.OperatorVerificationAssignmentResult) { r.Replayed = true }},
		// The receipt names the verifier the writer actually read. A receipt
		// carrying some other name means the typed confirmation was checked
		// against something other than what was assigned.
		{"receipt confirms another name", http.StatusCreated,
			func(r *store.OperatorVerificationAssignmentResult) { r.VerifierName = "cnode-peer" }},
		{"receipt has no verifier name", http.StatusCreated,
			func(r *store.OperatorVerificationAssignmentResult) { r.VerifierName = "" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			receipt := canonicalAssignmentReceipt()
			test.mutate(&receipt)
			if _, err := assignmentClient(t, test.status, receipt).AssignVerification(
				t.Context(), assignmentClientVerifier, "key-1", body); err == nil {
				t.Fatal("預期被拒絕，但接受了")
			}
		})
	}
}

func TestAssignmentClientRefusesAMissingPreviewOrKey(t *testing.T) {
	client := assignmentClient(t, http.StatusCreated, canonicalAssignmentReceipt())
	if _, err := client.AssignVerification(t.Context(), assignmentClientVerifier, "",
		VerificationAssignmentRequest{JobID: assignmentClientJob, PreviewDigest: assignmentClientDigest}); err == nil {
		t.Fatal("沒有 Idempotency-Key 應該被拒絕")
	}
	if _, err := client.AssignVerification(t.Context(), assignmentClientVerifier, "key-1",
		VerificationAssignmentRequest{JobID: assignmentClientJob,
			ConfirmVerifierName: "onode-peer"}); err == nil {
		t.Fatal("沒有 preview_digest 應該被拒絕")
	}
	if _, err := client.AssignVerification(t.Context(), assignmentClientVerifier, "key-1",
		VerificationAssignmentRequest{JobID: assignmentClientJob,
			PreviewDigest: assignmentClientDigest}); err == nil {
		t.Fatal("沒有 confirm_verifier_name 應該被拒絕")
	}
}
