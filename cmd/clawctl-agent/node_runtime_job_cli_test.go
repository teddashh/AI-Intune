package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/model"
	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/store"
)

// Read both original failed and recovered jobs through CLI subprocesses. The
// subprocess injects only the test Hub dial address; dispatch, HTTP and output
// use the production job show/evidence code.
func assertDarwinNodeJobCLI(t *testing.T, binary, endpoint string, f darwinNodeProfileFixture, outcome darwinNodeHTTPOutcome) {
	t.Helper()
	job, err := f.store.JobForMachine(f.job.JobID, f.job.MachineID)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := f.store.JobVerifications(f.job.JobID)
	if err != nil || len(rows) != outcome.wantEvidence {
		t.Fatalf("CLI evidence fixture count=%d err=%v", len(rows), err)
	}
	wantFailed := 0
	if outcome.failedRule != "" {
		wantFailed = 1
	}
	wantPassed := outcome.wantEvidence - wantFailed
	var show operator.JobDetailResult
	if err := json.Unmarshal(runNodeJobCLI(t, binary, endpoint, f.agentToken, "show", "--json", f.job.JobID), &show); err != nil {
		t.Fatal(err)
	}
	item := show.Item
	if item.JobID != f.job.JobID || item.MachineID != f.job.MachineID || item.State != outcome.wantState ||
		item.DesiredID != f.job.DesiredID || item.Revision != job.Revision || item.ResourceKind != "node-runtime" ||
		item.ArtifactDigest == nil || *item.ArtifactDigest != f.job.ArtifactDigest ||
		item.ArtifactDigestStatus != operator.JobArtifactDigestRecorded || item.LeaseStatus != operator.JobLeaseNone ||
		item.TerminalAt == nil || job.TerminalAt == nil || !item.TerminalAt.Equal(*job.TerminalAt) ||
		item.EventCount != 2 || item.VerificationTotal != outcome.wantEvidence ||
		item.VerificationPassed != wantPassed || item.VerificationFailed != wantFailed ||
		show.Desired.DesiredID != f.job.DesiredID || show.Desired.Revision != job.Revision {
		t.Fatal("job show changed the terminal job, pinned digest or evidence counts")
	}
	var evidence operator.JobEvidenceResult
	if err := json.Unmarshal(runNodeJobCLI(t, binary, endpoint, f.agentToken, "evidence", f.job.JobID, "--json", "--limit", "100"), &evidence); err != nil {
		t.Fatal(err)
	}
	if evidence.JobID != f.job.JobID || evidence.MachineID != f.job.MachineID || evidence.State != outcome.wantState ||
		evidence.Desired.DesiredID != f.job.DesiredID || evidence.Desired.Revision != job.Revision ||
		evidence.Verifications.Total != outcome.wantEvidence || len(evidence.Verifications.Items) != outcome.wantEvidence ||
		evidence.Verifications.Passed != wantPassed || evidence.Verifications.Failed != wantFailed || evidence.Verifications.Truncated ||
		evidence.Events.Total != 2 || evidence.Events.Truncated || evidence.Disclosure.IndependentVerifier {
		t.Fatal("job evidence changed identity, outcome, totals or executor attribution")
	}
	var gotSpec, wantSpec model.NodeRuntimeSpec
	if json.Unmarshal([]byte(evidence.Desired.Spec.Text), &gotSpec) != nil ||
		json.Unmarshal(f.job.Spec, &wantSpec) != nil || !reflect.DeepEqual(gotSpec, wantSpec) {
		t.Fatal("job evidence changed the pinned Node spec")
	}
	stored := make(map[string]store.JobVerification, len(rows))
	for _, row := range rows {
		stored[row.VerificationID] = row
	}
	for _, got := range evidence.Verifications.Items {
		want, ok := stored[got.VerificationID]
		if !ok || got.RuleID.Text != want.RuleID || got.Command.Text != want.Command ||
			got.StdoutExcerpt.Text != want.StdoutExcerpt || got.StderrExcerpt.Text != want.StderrExcerpt ||
			!reflect.DeepEqual(got.ExitCode, want.ExitCode) || got.Passed != want.Passed ||
			!got.ReportedVerifiedAt.Equal(want.VerifiedAt) || got.ReceivedAt == nil || !got.ReceivedAt.Equal(want.ReceivedAt) ||
			got.Producer.Kind != operator.JobEvidenceProducerExecutorAgent || got.Producer.ProducerID != f.job.MachineID ||
			got.Producer.EvidenceRole != operator.JobEvidenceRoleExecutor || got.Producer.Authority != operator.JobEvidenceAuthorityMachineLease ||
			!got.Producer.ProvenanceRecorded {
			t.Fatalf("job evidence did not preserve executor record %s", got.VerificationID)
		}
		delete(stored, got.VerificationID)
	}
	if len(stored) != 0 {
		t.Fatal("job evidence omitted an executor record")
	}
	textShow := string(runNodeJobCLI(t, binary, endpoint, f.agentToken, "show", f.job.JobID))
	textEvidence := string(runNodeJobCLI(t, binary, endpoint, f.agentToken, "evidence", f.job.JobID, "--limit", "100"))
	counts := fmt.Sprintf("verifications: %d (%d passed / %d failed)", outcome.wantEvidence, wantPassed, wantFailed)
	for _, output := range []string{textShow, textEvidence} {
		if !strings.Contains(output, f.job.JobID) || !strings.Contains(output, "state: "+string(outcome.wantState)) ||
			!strings.Contains(output, counts) {
			t.Fatal("CLI text omitted job identity, terminal state or evidence counts")
		}
	}
	if !strings.Contains(textShow, f.job.ArtifactDigest) || !strings.Contains(textShow, "lease: none") {
		t.Fatal("job show text omitted pinned digest or released lease")
	}
	for _, row := range rows {
		if !strings.Contains(textEvidence, fmt.Sprintf("rule=%s; exit=%d; passed=%t", strconv.Quote(row.RuleID), *row.ExitCode, row.Passed)) ||
			!strings.Contains(textEvidence, "command: "+strconv.Quote(row.Command)) ||
			!strings.Contains(textEvidence, "stdout: "+strconv.Quote(row.StdoutExcerpt)) ||
			!strings.Contains(textEvidence, "stderr: "+strconv.Quote(row.StderrExcerpt)) {
			t.Fatalf("job evidence text omitted measured result for rule %s", row.RuleID)
		}
	}
}

func runNodeJobCLI(t *testing.T, binary, endpoint, fixtureToken string, args ...string) []byte {
	t.Helper()
	input, err := json.Marshal(struct {
		URL  string   `json:"url"`
		Args []string `json:"args"`
	}{endpoint, args})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "--test-job-read-cli")
	cmd.Stdin = bytes.NewReader(input)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err = cmd.Run()
	if bytes.Contains(out.Bytes(), []byte(fixtureToken)) || bytes.Contains(errOut.Bytes(), []byte(fixtureToken)) {
		t.Fatal("job read CLI disclosed the fixture machine token")
	}
	if err != nil {
		t.Fatalf("job %s subprocess: %v; stderr=%s", args[0], err, errOut.String())
	}
	return out.Bytes()
}
