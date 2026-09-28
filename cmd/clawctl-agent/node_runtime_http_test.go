package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/deploy"
	"github.com/teddashh/AI-Intune/internal/model"
	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/operatorclient"
	"github.com/teddashh/AI-Intune/internal/store"
)

type nodeProfileRoundTrip func(*http.Request) (*http.Response, error)

func (f nodeProfileRoundTrip) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// The Hub runs its production machine handlers in a test-only child process.
// Only the Node executable is a shell fixture; this is not Mac hardware testing.
func TestDarwinNodeJobsRunnerCompletesThroughHubHTTP(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the Hub integration helper requires a Linux build host")
	}
	binary := filepath.Join(t.TempDir(), "hub-machine-api.test")
	build := exec.CommandContext(t.Context(), "go", "test", "-c", "-o", binary, "../clawctl-hub")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build Hub test process: %v\n%s", err, output)
	}
	for _, outcome := range []darwinNodeHTTPOutcome{
		{name: "success", wantState: deploy.Succeeded, wantEvidence: 3, wantDownloads: 1},
		{name: "stage-npm-failure", failedRule: "node-runtime-stage-npm", wantState: deploy.Failed, wantEvidence: 1, wantDownloads: 1},
		{name: "activate-npm-failure", failedRule: "node-runtime-activate-npm", wantState: deploy.Failed, wantEvidence: 4, wantDownloads: 1},
	} {
		for _, arch := range []string{"arm64", "amd64"} {
			for _, dropReply := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/drop-first-responses=%t", arch, outcome.name, dropReply), func(t *testing.T) {
					fixture := assignedDarwinNodeProfile(t, arch)
					endpoint := startNodeProfileHTTPHub(t, binary, fixture.artifactsDir, dropReply)
					client := nodeProfileOperatorHTTPClient(t, endpoint)
					var priorPreview operator.MachineProfileAssignmentPreviewResult
					if outcome.failedRule != "" {
						var err error
						priorPreview, err = client.PreviewMachineProfileAssignment(t.Context(), operator.MachineProfileAssignmentPreviewRequest{
							MachineID: fixture.job.MachineID, ProfileID: fixture.assignment.ProfileID, ProfileRevision: fixture.assignment.ProfileRevision,
						})
						if err != nil || len(priorPreview.Blockers) != 1 || priorPreview.Blockers[0] != store.OperatorMachineProfileAssignmentBlockerActiveJob {
							t.Fatalf("initial HTTP preview=%+v err=%v", priorPreview, err)
						}
					}
					journalPath := filepath.Join(t.TempDir(), "agent-journal.json")
					node := nodeRuntimeExecutor{
						targetOS: "darwin", targetArch: arch,
						deps: execDeps{home: "/Users/profile-test", fsRoot: t.TempDir(), hubURL: endpoint, token: fixture.agentToken},
					}
					if !t.Run("initial", func(t *testing.T) {
						runDarwinNodeHTTPJob(t, fixture, endpoint, journalPath, node, outcome, dropReply)
					}) || outcome.failedRule == "" {
						return
					}
					failedJob, err := fixture.store.JobForMachine(fixture.job.JobID, fixture.job.MachineID)
					if err != nil {
						t.Fatal(err)
					}
					failedEvidence, err := fixture.store.JobVerifications(fixture.job.JobID)
					if err != nil {
						t.Fatal(err)
					}
					retry := reassignDarwinNodeProfile(t, fixture, client, priorPreview, dropReply)
					recovery := darwinNodeHTTPOutcome{wantState: deploy.Succeeded, wantEvidence: 3, wantDownloads: 1}
					if outcome.failedRule == "node-runtime-activate-npm" {
						recovery.wantDownloads = 0 // Recheck and reuse the exact release kept after rollback.
					}
					// Recreate the jobs runner with the same on-disk journal and runtime directory.
					if !t.Run("reassigned", func(t *testing.T) {
						runDarwinNodeHTTPJob(t, retry, endpoint, journalPath, node, recovery, dropReply)
					}) {
						return
					}
					preservedJob, err := fixture.store.JobForMachine(fixture.job.JobID, fixture.job.MachineID)
					if err != nil || !reflect.DeepEqual(preservedJob, failedJob) {
						t.Fatalf("reassignment changed the original failed job: %v", err)
					}
					preservedEvidence, err := fixture.store.JobVerifications(fixture.job.JobID)
					if err != nil || !reflect.DeepEqual(preservedEvidence, failedEvidence) {
						t.Fatalf("reassignment changed the original failure evidence: %v", err)
					}
					var spec model.NodeRuntimeSpec
					if err := json.Unmarshal(retry.job.Spec, &spec); err != nil {
						t.Fatal(err)
					}
					current := node.deps.fsPath(filepath.Join(node.deps.home, ".local", "share", "clawctl", "node-runtime", "current"))
					if target, err := os.Readlink(current); err != nil || target != filepath.Join("releases", spec.Version) {
						t.Fatalf("recovered current=%q err=%v", target, err)
					}
					assertDarwinNodeProfileAlreadyAssigned(t, retry, client)
					assertNodeProfileOperatorAudit(t, fixture)
					// Inspect the original failed job and the recovered successful
					// job through the actual operator CLI after recovery completed.
					assertDarwinNodeJobCLI(t, binary, endpoint, fixture, outcome)
					assertDarwinNodeJobCLI(t, binary, endpoint, retry, recovery)
					if got := nodeProfileGraphCounts(t, fixture); got != [3]int{2, 2, 2} {
						t.Fatalf("CLI reads changed assignment graph: %v", got)
					}
				})
			}
		}
	}
}

type darwinNodeHTTPOutcome struct {
	name          string
	failedRule    string
	wantState     deploy.JobState
	wantEvidence  int
	wantDownloads int
}

func runDarwinNodeHTTPJob(t *testing.T, fixture darwinNodeProfileFixture, endpoint, journalPath string,
	node nodeRuntimeExecutor, outcome darwinNodeHTTPOutcome, dropReply bool) {
	t.Helper()
	scope := resourceScope{Kind: fixture.job.ResourceKind, ID: fixture.job.ResourceID}
	var mu sync.Mutex
	counts := make(map[string]int)
	var sent []model.JobVerificationRequest
	var replies []model.JobStateResponse
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	t.Cleanup(transport.CloseIdleConnections)
	previousClient := httpClient
	httpClient = &http.Client{Transport: nodeProfileRoundTrip(func(req *http.Request) (*http.Response, error) {
		mu.Lock()
		defer mu.Unlock()
		counts[req.Method+" "+req.URL.Path]++
		if strings.HasSuffix(req.URL.Path, "/verifications") {
			body, err := req.GetBody()
			if err != nil {
				return nil, err
			}
			var row model.JobVerificationRequest
			err = json.NewDecoder(body).Decode(&row)
			_ = body.Close()
			if err != nil {
				return nil, err
			}
			row.LeaseToken = "" // Keep credentials out of assertion diagnostics.
			sent = append(sent, row)
		}
		response, err := transport.RoundTrip(req)
		if err != nil || !strings.HasSuffix(req.URL.Path, "/complete") || response.StatusCode != http.StatusOK {
			return response, err
		}
		body, err := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if err != nil {
			return nil, err
		}
		response.Body = io.NopCloser(bytes.NewReader(body))
		var reply model.JobStateResponse
		if err := json.Unmarshal(body, &reply); err != nil {
			return nil, err
		}
		replies = append(replies, reply)
		journal, err := loadWatermarkJournal(journalPath)
		if err != nil || journal.get(scope).MaxApplied != 0 {
			t.Errorf("applied revision advanced before completion receipt: err=%v", err)
		}
		if dropReply && len(replies) == 1 {
			_ = response.Body.Close()
			return nil, io.ErrUnexpectedEOF
		}
		return response, nil
	})}
	t.Cleanup(func() { httpClient = previousClient })

	if outcome.failedRule != "" {
		node.deps.run = func(ctx context.Context, name string, args ...string) (string, string, error) {
			// Fail the npm measurement at the selected phase; other commands run the fixture.
			staging := strings.Contains(name, string(filepath.Separator)+".staging-")
			if len(args) == 2 && staging == (outcome.failedRule == "node-runtime-stage-npm") {
				return "invalid npm version\n", "", nil
			}
			return runExecutorCommand(ctx, name, args...)
		}
	}
	nudge := make(chan struct{}, 1)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	runJobs(ctx, jobsOptions{
		HubURL: endpoint, Token: fixture.agentToken, JournalPath: journalPath,
		Executor: newKindExecutor(nil, nil, nil, node, nil), Nudge: nudge,
		PollInterval: time.Second, PollJitter: func(d time.Duration) time.Duration { return d },
		RetryBackoff: func(int) time.Duration { return 0 },
		Sleep: func(ctx context.Context, d time.Duration) error {
			if d == time.Second { // End this bounded run after the Hub returns no next job.
				return context.Canceled
			}
			return sleepWithContext(ctx, d)
		},
	})
	if ctx.Err() != nil {
		t.Fatalf("jobs runner did not finish: %v", ctx.Err())
	}
	job, err := fixture.store.JobForMachine(fixture.job.JobID, fixture.job.MachineID)
	if err != nil || job.State != outcome.wantState || job.ArtifactDigest != fixture.job.ArtifactDigest {
		rows, _ := fixture.store.JobVerifications(fixture.job.JobID)
		for _, row := range rows {
			t.Logf("rule=%s passed=%t command=%q stdout=%q stderr=%q", row.RuleID, row.Passed,
				row.Command, row.StdoutExcerpt, strings.ReplaceAll(row.StderrExcerpt, fixture.agentToken, "[redacted]"))
		}
		t.Fatalf("HTTP job completion: state=%s digest=%s err=%v", job.State, job.ArtifactDigest, err)
	}
	if job.TerminalAt == nil || job.LeaseToken != "" || job.LeaseExpiresAt != nil {
		t.Error("terminal job did not record completion and clear its lease")
	}
	journal, err := loadWatermarkJournal(journalPath)
	want := deploy.Watermarks{MaxSeen: deploy.Revision(fixture.job.Revision)}
	if outcome.wantState == deploy.Succeeded {
		want.MaxApplied = deploy.Revision(fixture.job.Revision)
	}
	if err != nil || journal.get(scope) != want || len(nudge) != 1 {
		t.Fatalf("completion receipt: watermarks=%+v nudge=%d err=%v", journal.get(scope), len(nudge), err)
	}
	mu.Lock()
	defer mu.Unlock()
	wantComplete := 1
	if dropReply {
		wantComplete = 2
	}
	prefix := "POST /v1/jobs/" + fixture.job.JobID
	for path, want := range map[string]int{
		"GET /v1/jobs/next": 2, prefix + "/claims": 1,
		prefix + "/events": 2, prefix + "/verifications": outcome.wantEvidence, prefix + "/complete": wantComplete,
		"GET /v1/artifacts/" + strings.TrimPrefix(fixture.job.ArtifactDigest, "sha256:"): outcome.wantDownloads,
	} {
		if counts[path] != want {
			t.Errorf("%s requests=%d want=%d", path, counts[path], want)
		}
	}
	if len(replies) != wantComplete {
		t.Fatalf("completion replies=%d want=%d", len(replies), wantComplete)
	}
	for i, reply := range replies {
		if reply.State != string(outcome.wantState) || reply.Replayed != (i > 0) {
			t.Errorf("completion reply %d=%+v", i, reply)
		}
	}
	evidence, err := fixture.store.JobVerifications(job.JobID)
	if err != nil || len(evidence) != outcome.wantEvidence || len(sent) != outcome.wantEvidence {
		t.Fatalf("HTTP evidence: stored=%d sent=%d err=%v", len(evidence), len(sent), err)
	}
	byRule := make(map[string]model.JobVerificationRequest, len(sent))
	for _, row := range sent {
		byRule[row.RuleID] = row
	}
	failedCount := 0
	rollbackSeen := false
	for _, got := range evidence {
		if !got.Passed {
			failedCount++
			if got.RuleID != outcome.failedRule || got.ExitCode == nil || *got.ExitCode == 0 ||
				got.StdoutExcerpt != "invalid npm version\n" {
				t.Errorf("unexpected failure evidence: rule=%s", got.RuleID)
			}
		}
		if got.RuleID == "node-runtime-rollback" {
			rollbackSeen = got.Passed
		}
		want, ok := byRule[got.RuleID]
		// The ledger stores evidence timestamps at RFC3339 second precision.
		if !ok || got.Command != want.Command || got.StdoutExcerpt != want.StdoutExcerpt ||
			got.StderrExcerpt != want.StderrExcerpt || got.ExitCode == nil || *got.ExitCode != want.ExitCode ||
			got.Passed != want.Passed || !got.VerifiedAt.Equal(want.VerifiedAt.Truncate(time.Second)) ||
			got.ProducerID != job.MachineID || got.Authority != store.JobVerificationAuthorityMachineLease {
			t.Errorf("HTTP changed executor evidence for rule %s", got.RuleID)
		}
	}
	if outcome.failedRule != "" {
		if failedCount != 1 || rollbackSeen != (outcome.failedRule == "node-runtime-activate-npm") {
			t.Errorf("failed evidence=%d rollback=%t", failedCount, rollbackSeen)
		}
		current := node.deps.fsPath(filepath.Join(node.deps.home, ".local", "share", "clawctl", "node-runtime", "current"))
		if _, err := os.Lstat(current); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("failed first install left current activation: %v", err)
		}
	}
}

func assertDarwinNodeProfileAlreadyAssigned(t *testing.T, fixture darwinNodeProfileFixture, client *operatorclient.Client) {
	t.Helper()
	preview, err := client.PreviewMachineProfileAssignment(t.Context(), operator.MachineProfileAssignmentPreviewRequest{
		MachineID: fixture.job.MachineID, ProfileID: fixture.assignment.ProfileID, ProfileRevision: fixture.assignment.ProfileRevision,
	})
	if err != nil || !preview.AlreadyAssigned || preview.CreatesJobs != 0 || preview.CreatesDesiredStates != 0 ||
		preview.CreatesAssignment || len(preview.Blockers) != 0 || len(preview.Packages) != 1 {
		t.Fatalf("recovered profile preview=%+v err=%v", preview, err)
	}
	if preview.CurrentAssignmentID != fixture.assignment.AssignmentID ||
		preview.CurrentAssignmentRevision != fixture.assignment.AssignmentRevision ||
		preview.Packages[0].CurrentRevision != deploy.Revision(fixture.job.Revision) ||
		preview.Packages[0].PlannedRevision != deploy.Revision(fixture.job.Revision) {
		t.Fatal("recovered profile preview moved the assignment or desired revision")
	}
	result := assignNodeProfileHTTPWithReplay(t, fixture, client, "node-recovered-noop", operator.MachineProfileAssignmentRequest{
		MachineID: fixture.job.MachineID, ProfileID: fixture.assignment.ProfileID, ProfileRevision: fixture.assignment.ProfileRevision,
		ConfirmDisplayName: preview.DisplayName, PreviewDigest: preview.PreviewDigest,
		Reason: "confirm recovered pinned Node",
	}, false)
	if !result.AlreadyAssigned || result.AssignmentID != fixture.assignment.AssignmentID ||
		result.AssignmentRevision != fixture.assignment.AssignmentRevision || len(result.Packages) != 1 {
		t.Fatalf("recovered profile assignment=%+v", result)
	}
	if result.Packages[0].JobID != fixture.job.JobID || result.Packages[0].Revision != deploy.Revision(fixture.job.Revision) {
		t.Fatal("assigning an already recovered profile created another job or revision")
	}
	if _, ok, err := fixture.store.NextJobForMachine(fixture.job.MachineID); err != nil || ok {
		t.Fatalf("already assigned profile left a next job: available=%t err=%v", ok, err)
	}
}

func startNodeProfileHTTPHub(t *testing.T, binary, artifactsDir string, dropAssignmentReply bool) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	cmd := exec.CommandContext(ctx, binary, "--test-machine-api-server")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	input, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		_ = input.Close()
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		cancel()
		_ = input.Close()
		_ = output.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = input.Close()
		err := cmd.Wait()
		cancel()
		if err != nil {
			t.Errorf("Hub test process: %v\n%s", err, stderr.String())
		}
	})
	if err := json.NewEncoder(input).Encode(map[string]any{
		"db_path": filepath.Join(artifactsDir, "hub.db"), "artifacts_dir": artifactsDir,
		"drop_assignment_reply": dropAssignmentReply,
	}); err != nil {
		t.Fatal(err)
	}
	var ready struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(output).Decode(&ready); err != nil || ready.URL == "" {
		t.Fatalf("Hub test process startup: %v", err)
	}
	return ready.URL
}
