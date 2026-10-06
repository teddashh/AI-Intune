package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/agentadapter"
	"github.com/teddashh/AI-Intune/internal/deploy"
	"github.com/teddashh/AI-Intune/internal/model"
)

var jobsTestNow = time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)

type executorFunc func(context.Context, model.JobResponse) ([]model.JobVerificationRequest, error)

func (f executorFunc) Run(ctx context.Context, job model.JobResponse) ([]model.JobVerificationRequest, error) {
	return f(ctx, job)
}

type requestRecord struct {
	Method string
	Path   string
	Auth   string
	Body   []byte
}

type hubStep struct {
	Method   string
	Path     string
	Status   int
	Response any
	Check    func(*testing.T, requestRecord)
}

type scriptedHub struct {
	t       *testing.T
	mu      sync.Mutex
	steps   []hubStep
	next    int
	records []requestRecord
}

func newHubServer(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()
	server := httptest.NewTestServer(t, handler)
	oldClient := httpClient
	httpClient = server.Client()
	t.Cleanup(func() { httpClient = oldClient })
	return server
}

func (h *scriptedHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	record := requestRecord{Method: r.Method, Path: r.URL.Path, Auth: r.Header.Get("Authorization"), Body: body}
	h.mu.Lock()
	h.records = append(h.records, record)
	if h.next >= len(h.steps) {
		h.mu.Unlock()
		h.t.Errorf("假 Hub 收到劇本外請求：%s %s body=%s", r.Method, r.URL.Path, body)
		http.Error(w, "unexpected request", http.StatusInternalServerError)
		return
	}
	step := h.steps[h.next]
	h.next++
	h.mu.Unlock()

	if r.Method != step.Method || r.URL.Path != step.Path {
		h.t.Errorf("假 Hub 第 %d 步 = %s %s；要的是 %s %s", h.next, r.Method, r.URL.Path, step.Method, step.Path)
	}
	if step.Check != nil {
		step.Check(h.t, record)
	}
	if step.Response != nil {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(step.Status)
	if step.Response != nil {
		if err := json.NewEncoder(w).Encode(step.Response); err != nil {
			h.t.Errorf("假 Hub 編碼回應失敗：%v", err)
		}
	}
}

func (h *scriptedHub) assertDone(t *testing.T) []requestRecord {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.next != len(h.steps) {
		t.Errorf("假 Hub 只走了 %d/%d 步", h.next, len(h.steps))
	}
	return append([]requestRecord(nil), h.records...)
}

func testJob(id string, revision int64, kind, digest string) model.JobResponse {
	spec := json.RawMessage(fmt.Sprintf(`{"kind":%q}`, kind))
	return model.JobResponse{
		JobID: id, Revision: revision, State: string(deploy.NotStarted),
		ArtifactDigest: digest, ResourceKind: agentadapter.ExecutorKindOpenClaw,
		ResourceID: agentadapter.ExecutorKindOpenClaw,
		Spec:       spec,
	}
}

func specDigest(spec []byte) string {
	sum := sha256.Sum256(spec)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func testArtifactJob(id string, revision int64, artifactSHA256, digest string) model.JobResponse {
	spec := json.RawMessage(fmt.Sprintf(
		`{"kind":"openclaw","version":"2026.6.10","artifact":{"sha256":%q,"size":123,"url":"/v1/artifacts/%s"}}`,
		artifactSHA256, artifactSHA256))
	return model.JobResponse{
		JobID: id, Revision: revision, State: string(deploy.NotStarted),
		ArtifactDigest: digest, ResourceKind: agentadapter.ExecutorKindOpenClaw,
		ResourceID: agentadapter.ExecutorKindOpenClaw, Spec: spec,
	}
}

func claimStep(id string) hubStep {
	return hubStep{
		Method: http.MethodPost, Path: "/v1/jobs/" + id + "/claims", Status: http.StatusOK,
		Response: model.JobLeaseResponse{LeaseToken: "lease-" + id, LeaseExpiresAt: jobsTestNow.Add(2 * time.Minute)},
	}
}

func eventStep(id, phase string, seq int) hubStep {
	return hubStep{
		Method: http.MethodPost, Path: "/v1/jobs/" + id + "/events", Status: http.StatusAccepted,
		Response: model.JobEventResponse{AcceptedSeq: seq},
		Check: func(t *testing.T, got requestRecord) {
			var req model.JobEventRequest
			if err := json.Unmarshal(got.Body, &req); err != nil {
				t.Fatalf("事件 body 不是 JSON：%v", err)
			}
			if req.Seq != seq || req.Phase != phase {
				t.Errorf("事件 = seq %d phase %q；要 seq %d phase %q", req.Seq, req.Phase, seq, phase)
			}
		},
	}
}

func verificationStep(id string, check func(*testing.T, model.JobVerificationRequest)) hubStep {
	return hubStep{
		Method: http.MethodPost, Path: "/v1/jobs/" + id + "/verifications", Status: http.StatusCreated,
		Check: func(t *testing.T, got requestRecord) {
			var req model.JobVerificationRequest
			if err := json.Unmarshal(got.Body, &req); err != nil {
				t.Fatalf("驗證 body 不是 JSON：%v", err)
			}
			if check != nil {
				check(t, req)
			}
		},
	}
}

func completeStep(id, state string, replayed bool) hubStep {
	return hubStep{
		Method: http.MethodPost, Path: "/v1/jobs/" + id + "/complete", Status: http.StatusOK,
		Response: model.JobStateResponse{State: state, Replayed: replayed},
	}
}

func rejectStep(id string, code deploy.RejectionCode, seq int, detailPart string) hubStep {
	return hubStep{
		Method: http.MethodPost, Path: "/v1/jobs/" + id + "/reject", Status: http.StatusOK,
		Response: model.JobStateResponse{State: string(deploy.Rejected)},
		Check: func(t *testing.T, got requestRecord) {
			var req model.JobRejectRequest
			if err := json.Unmarshal(got.Body, &req); err != nil {
				t.Fatalf("拒單 body 不是 JSON：%v", err)
			}
			if req.RejectionCode != string(code) || req.Seq != seq || !strings.Contains(req.Detail, detailPart) {
				t.Errorf("拒單 = code %q seq %d detail %q；要 %q/%d 且含 %q",
					req.RejectionCode, req.Seq, req.Detail, code, seq, detailPart)
			}
		},
	}
}

func nextStep(job model.JobResponse) hubStep {
	return hubStep{Method: http.MethodGet, Path: "/v1/jobs/next", Status: http.StatusOK, Response: job}
}

func noJobStep() hubStep {
	return hubStep{Method: http.MethodGet, Path: "/v1/jobs/next", Status: http.StatusNoContent}
}

func runScript(t *testing.T, steps []hubStep, journal string, exec executor) []requestRecord {
	return runScriptWithNudge(t, steps, journal, exec, nil)
}

func runScriptWithNudge(t *testing.T, steps []hubStep, journal string, exec executor, nudge chan<- struct{}) []requestRecord {
	t.Helper()
	hub := &scriptedHub{t: t, steps: steps}
	server := newHubServer(t, hub)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sleep := func(ctx context.Context, d time.Duration) error {
		switch {
		case d == time.Second:
			cancel()
			return context.Canceled
		case d < 10*time.Second:
			return nil
		default:
			<-ctx.Done()
			return ctx.Err()
		}
	}
	runJobs(ctx, jobsOptions{
		HubURL: server.URL, Token: "agent-token", JournalPath: journal,
		PollInterval: time.Second, Executor: exec, Now: func() time.Time { return jobsTestNow },
		Sleep: sleep, RetryBackoff: func(int) time.Duration { return time.Millisecond },
		PollJitter: func(d time.Duration) time.Duration { return d },
		Nudge:      nudge,
	})
	return hub.assertDone(t)
}

func readWatermarks(t *testing.T, path string) deploy.Watermarks {
	t.Helper()
	w, err := loadJournal(path)
	if err != nil {
		t.Fatalf("讀水位失敗：%v", err)
	}
	return w
}

func readWatermarkJournal(t *testing.T, path string) watermarkJournal {
	t.Helper()
	journal, err := loadWatermarkJournal(path)
	if err != nil {
		t.Fatalf("讀水位日誌失敗：%v", err)
	}
	return journal
}

func TestLegacyJournalMigratesToPerResourceSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent-journal.json")
	if err := os.WriteFile(path, []byte(`{"max_seen":43,"max_applied":41}`), 0o600); err != nil {
		t.Fatal(err)
	}
	journal := readWatermarkJournal(t, path)
	want := deploy.Watermarks{MaxSeen: 43, MaxApplied: 41}
	if got := journal.get(legacyOpenClawScope); got != want {
		t.Fatalf("舊日誌載入的 OpenClaw 水位 = %+v；要 %+v", got, want)
	}
	if err := saveWatermarkJournal(path, journal); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if string(got["schema_version"]) != "2" || got["resources"] == nil {
		t.Fatalf("遷移後的日誌不是 v2：%s", b)
	}
	if got["max_seen"] != nil || got["max_applied"] != nil {
		t.Fatalf("遷移後仍有舊版頂層水位：%s", b)
	}
	if reloaded := readWatermarkJournal(t, path).get(legacyOpenClawScope); reloaded != want {
		t.Fatalf("重載遷移日誌的水位 = %+v；要 %+v", reloaded, want)
	}
}

func TestResourceWatermarksAreIndependentAndPersisted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent-journal.json")
	if err := saveJournal(path, deploy.Watermarks{MaxSeen: 43, MaxApplied: 41}); err != nil {
		t.Fatal(err)
	}
	nodeScope := resourceScope{Kind: "node-runtime", ID: "nodejs"}
	nodeJob := testJob("node-rev1", 1, "noop", specDigest([]byte(`{"kind":"noop"}`)))
	nodeJob.ResourceKind, nodeJob.ResourceID = nodeScope.Kind, nodeScope.ID
	oldOpenClaw := testJob("openclaw-rev42", 42, "noop", specDigest([]byte(`{"kind":"noop"}`)))
	runScript(t, []hubStep{
		nextStep(nodeJob), claimStep(nodeJob.JobID), eventStep(nodeJob.JobID, "start", 1),
		verificationStep(nodeJob.JobID, nil), eventStep(nodeJob.JobID, "finish", 2),
		completeStep(nodeJob.JobID, string(deploy.Succeeded), false),
		nextStep(oldOpenClaw), claimStep(oldOpenClaw.JobID),
		rejectStep(oldOpenClaw.JobID, deploy.StaleRevision, 1,
			"openclaw/openclaw 已見過 revision 43，這張是 42"), noJobStep(),
	}, path, nil)

	journal := readWatermarkJournal(t, path)
	if got := journal.get(nodeScope); got != (deploy.Watermarks{MaxSeen: 1, MaxApplied: 1}) {
		t.Errorf("node-runtime 水位 = %+v；要 seen=applied=1", got)
	}
	if got := journal.get(legacyOpenClawScope); got != (deploy.Watermarks{MaxSeen: 43, MaxApplied: 41}) {
		t.Errorf("OpenClaw 水位 = %+v；要 seen=43 applied=41", got)
	}
}

func TestInvalidJobResourceIdentityRejectsBeforeJournalMutation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent-journal.json")
	job := testJob("bad-resource", 7, "noop", specDigest([]byte(`{"kind":"noop"}`)))
	job.ResourceID = ""
	var calls atomic.Int64
	exec := executorFunc(func(context.Context, model.JobResponse) ([]model.JobVerificationRequest, error) {
		calls.Add(1)
		return nil, nil
	})
	runScript(t, []hubStep{nextStep(job), claimStep(job.JobID),
		rejectStep(job.JobID, deploy.PreconditionFailed, 1, "resource identity 不合法"), noJobStep()}, path, exec)
	if calls.Load() != 0 {
		t.Fatalf("不合法 resource identity 呼叫了 executor %d 次", calls.Load())
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("不合法 resource identity 改動了日誌：%v", err)
	}
}

func TestJournalRejectsMalformedDocuments(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{"unknown field", `{"schema_version":2,"resources":{},"extra":true}`},
		{"mixed schemas", `{"schema_version":2,"resources":{},"max_seen":4,"max_applied":3}`},
		{"null mixed into legacy", `{"max_seen":4,"max_applied":3,"resources":null}`},
		{"null mixed into v2", `{"schema_version":2,"resources":{},"max_seen":null}`},
		{"duplicate top-level field", `{"schema_version":2,"schema_version":2,"resources":{}}`},
		{"duplicate resource field", `{"schema_version":2,"resources":{"openclaw":{"openclaw":{"max_seen":2,"max_seen":1,"max_applied":1}}}}`},
		{"trailing json", `{"schema_version":2,"resources":{}} {}`},
		{"negative seen", `{"schema_version":2,"resources":{"openclaw":{"openclaw":{"max_seen":-1,"max_applied":0}}}}`},
		{"applied above seen", `{"schema_version":2,"resources":{"openclaw":{"openclaw":{"max_seen":2,"max_applied":3}}}}`},
		{"empty resource map", `{"schema_version":2,"resources":{"openclaw":{}}}`},
		{"missing legacy applied", `{"max_seen":4}`},
		{"unsupported schema", `{"schema_version":3,"resources":{}}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "agent-journal.json")
			if err := os.WriteFile(path, []byte(tc.raw), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := loadWatermarkJournal(path); err == nil {
				t.Fatalf("日誌應該被拒絕：%s", tc.raw)
			}
		})
	}
}

func TestJournalRejectsOversizedAndNonRegularFiles(t *testing.T) {
	t.Run("oversized", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "agent-journal.json")
		if err := os.WriteFile(path, bytes.Repeat([]byte("x"), maxJournalBytes+1), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := loadWatermarkJournal(path); err == nil {
			t.Fatal("超過大小上限的日誌被載入")
		}
	})
	t.Run("directory", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "agent-journal.json")
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
		if _, err := loadWatermarkJournal(path); err == nil {
			t.Fatal("目錄被當成水位日誌")
		}
	})
	t.Run("symlink", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(dir, "target.json")
		if err := os.WriteFile(target, []byte(`{"schema_version":2,"resources":{}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, "agent-journal.json")
		if err := os.Symlink(target, path); err != nil {
			t.Fatal(err)
		}
		if _, err := loadWatermarkJournal(path); err == nil {
			t.Fatal("符號連結被當成水位日誌")
		}
	})
}

func TestJobsHappyPathFollowsProtocolAndPersistsBothWatermarks(t *testing.T) {
	journal := filepath.Join(t.TempDir(), "agent-journal.json")
	job := testJob("happy", 43, "noop", specDigest([]byte(`{"kind":"noop"}`)))
	steps := []hubStep{
		nextStep(job), claimStep(job.JobID), eventStep(job.JobID, "start", 1),
		verificationStep(job.JobID, func(t *testing.T, req model.JobVerificationRequest) {
			if req.LeaseToken != "lease-happy" || req.RuleID != "noop" || !req.Passed {
				t.Errorf("noop 驗證內容不對：%+v", req)
			}
		}),
		eventStep(job.JobID, "finish", 2), completeStep(job.JobID, string(deploy.Succeeded), false), noJobStep(),
	}
	records := runScript(t, steps, journal, nil)

	wantPaths := []string{
		"/v1/jobs/next", "/v1/jobs/happy/claims", "/v1/jobs/happy/events",
		"/v1/jobs/happy/verifications", "/v1/jobs/happy/events", "/v1/jobs/happy/complete", "/v1/jobs/next",
	}
	var gotPaths []string
	for _, record := range records {
		gotPaths = append(gotPaths, record.Path)
		if record.Auth != "Bearer agent-token" {
			t.Errorf("%s 沒帶正確 bearer：%q", record.Path, record.Auth)
		}
		if bytes.Contains(record.Body, []byte(`"machine_id"`)) {
			t.Errorf("%s body 不准宣告 machine_id：%s", record.Path, record.Body)
		}
	}
	if !reflect.DeepEqual(gotPaths, wantPaths) {
		t.Errorf("呼叫順序 = %v；要 %v", gotPaths, wantPaths)
	}
	if got := readWatermarks(t, journal); got != (deploy.Watermarks{MaxSeen: 43, MaxApplied: 43}) {
		t.Errorf("水位 = %+v；要 seen=applied=43", got)
	}
	if b, err := os.ReadFile(journal); err != nil || !bytes.Contains(b, []byte(`"max_seen"`)) {
		t.Errorf("日誌不是指定的 snake_case JSON：%s（err=%v）", b, err)
	}
}

func TestStaleRevisionRejectsWithoutCallingExecutor(t *testing.T) {
	journal := filepath.Join(t.TempDir(), "agent-journal.json")
	want := deploy.Watermarks{MaxSeen: 43, MaxApplied: 41}
	if err := saveJournal(journal, want); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int64
	exec := executorFunc(func(context.Context, model.JobResponse) ([]model.JobVerificationRequest, error) {
		calls.Add(1)
		return nil, nil
	})
	job := testJob("stale", 42, "noop", specDigest([]byte(`{"kind":"noop"}`)))
	runScript(t, []hubStep{nextStep(job), claimStep(job.JobID),
		rejectStep(job.JobID, deploy.StaleRevision, 1, "已見過 revision 43，這張是 42"), noJobStep()}, journal, exec)
	if calls.Load() != 0 {
		t.Errorf("舊版仍呼叫 executor %d 次", calls.Load())
	}
	if got := readWatermarks(t, journal); got != want {
		t.Errorf("拒絕舊版後水位變了：%+v；原本 %+v", got, want)
	}
}

func TestMaxSeenMovesBeforeExecutionSo43FailureStillRejects42(t *testing.T) {
	journal := filepath.Join(t.TempDir(), "agent-journal.json")
	job43 := testJob("rev43", 43, "noop", specDigest([]byte(`{"kind":"noop"}`)))
	job42 := testJob("rev42", 42, "noop", specDigest([]byte(`{"kind":"noop"}`)))
	var calls atomic.Int64
	exec := executorFunc(func(context.Context, model.JobResponse) ([]model.JobVerificationRequest, error) {
		calls.Add(1)
		return nil, errors.New("43 套用失敗")
	})
	steps := []hubStep{
		nextStep(job43), claimStep(job43.JobID), eventStep(job43.JobID, "start", 1),
		verificationStep(job43.JobID, func(t *testing.T, req model.JobVerificationRequest) {
			if req.Passed || req.RuleID != "executor" {
				t.Errorf("executor 錯誤沒有變成失敗證據：%+v", req)
			}
		}),
		eventStep(job43.JobID, "finish", 2), completeStep(job43.JobID, string(deploy.Failed), false),
		nextStep(job42), claimStep(job42.JobID),
		rejectStep(job42.JobID, deploy.StaleRevision, 1, "已見過 revision 43，這張是 42"), noJobStep(),
	}
	runScript(t, steps, journal, exec)
	if calls.Load() != 1 {
		t.Errorf("executor 呼叫 %d 次；42 不准執行", calls.Load())
	}
	if got := readWatermarks(t, journal); got != (deploy.Watermarks{MaxSeen: 43}) {
		t.Errorf("水位 = %+v；要 max_seen=43 max_applied=0", got)
	}
}

func TestMissingPinnedDigestIsPreconditionFailure(t *testing.T) {
	journal := filepath.Join(t.TempDir(), "agent-journal.json")
	job := testJob("digestless", 8, "noop", "")
	var calls atomic.Int64
	exec := executorFunc(func(context.Context, model.JobResponse) ([]model.JobVerificationRequest, error) {
		calls.Add(1)
		return nil, nil
	})
	runScript(t, []hubStep{nextStep(job), claimStep(job.JobID),
		rejectStep(job.JobID, deploy.PreconditionFailed, 1, "Hub 沒有釘 artifact digest"), noJobStep()}, journal, exec)
	if calls.Load() != 0 {
		t.Errorf("沒釘 digest 還呼叫 executor %d 次", calls.Load())
	}
}

func TestNoArtifactDigestMismatchIsPreconditionFailure(t *testing.T) {
	journal := filepath.Join(t.TempDir(), "agent-journal.json")
	job := testJob("noop-mismatch", 9, "noop", "sha256:fixed")
	var calls atomic.Int64
	exec := executorFunc(func(context.Context, model.JobResponse) ([]model.JobVerificationRequest, error) {
		calls.Add(1)
		return nil, nil
	})
	runScript(t, []hubStep{nextStep(job), claimStep(job.JobID),
		rejectStep(job.JobID, deploy.PreconditionFailed, 1,
			"沒有 artifact 的單，digest 該是 spec 本身的 sha256，對不上"), noJobStep()}, journal, exec)
	if calls.Load() != 0 {
		t.Errorf("noop digest 對不上還呼叫 executor %d 次", calls.Load())
	}
}

func TestArtifactDigestMismatchIsPreconditionFailure(t *testing.T) {
	journal := filepath.Join(t.TempDir(), "agent-journal.json")
	artifactSHA256 := strings.Repeat("a", 64)
	job := testArtifactJob("artifact-mismatch", 10, artifactSHA256, "sha256:"+strings.Repeat("b", 64))
	var calls atomic.Int64
	exec := executorFunc(func(context.Context, model.JobResponse) ([]model.JobVerificationRequest, error) {
		calls.Add(1)
		return nil, nil
	})
	runScript(t, []hubStep{nextStep(job), claimStep(job.JobID),
		rejectStep(job.JobID, deploy.PreconditionFailed, 1,
			"單上釘的 digest 跟 spec 宣告的 artifact 不一致"), noJobStep()}, journal, exec)
	if calls.Load() != 0 {
		t.Errorf("artifact digest 對不上還呼叫 executor %d 次", calls.Load())
	}
}

func TestJournalWriteFailureIsNeverSwallowed(t *testing.T) {
	base := t.TempDir()
	journal := filepath.Join(base, "agent-journal.json")
	job := testJob("writefail", 9, "noop", specDigest([]byte(`{"kind":"noop"}`)))
	var calls atomic.Int64
	hub := &scriptedHub{t: t, steps: []hubStep{nextStep(job), claimStep(job.JobID),
		rejectStep(job.JobID, deploy.PreconditionFailed, 1, "水位日誌寫不進去"), noJobStep()}}
	server := newHubServer(t, hub)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runner, err := newJobsRunner(jobsOptions{
		HubURL: server.URL, Token: "agent-token", JournalPath: journal, PollInterval: time.Second,
		Executor: executorFunc(func(context.Context, model.JobResponse) ([]model.JobVerificationRequest, error) {
			calls.Add(1)
			return nil, nil
		}),
		Now: func() time.Time { return jobsTestNow }, PollJitter: func(d time.Duration) time.Duration { return d },
		Sleep: func(_ context.Context, d time.Duration) error {
			if d == time.Second {
				cancel()
				return context.Canceled
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	// 先讓啟動讀到「不存在」，再把目標路徑變成目錄；rename 必定失敗。
	if err := os.Mkdir(journal, 0o700); err != nil {
		t.Fatal(err)
	}
	runner.run(ctx)
	hub.assertDone(t)
	if calls.Load() != 0 {
		t.Errorf("水位寫不進去還呼叫 executor %d 次", calls.Load())
	}
}

func TestCorruptJournalPreventsLoopFromStarting(t *testing.T) {
	journal := filepath.Join(t.TempDir(), "agent-journal.json")
	if err := os.WriteFile(journal, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int64
	server := newHubServer(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	runJobs(context.Background(), jobsOptions{
		HubURL: server.URL, Token: "agent-token", JournalPath: journal,
		Sleep: func(context.Context, time.Duration) error { t.Fatal("壞日誌不准進 sleep"); return nil },
	})
	if requests.Load() != 0 {
		t.Errorf("壞日誌仍拉了 %d 次單", requests.Load())
	}
}

func TestUnsupportedExecutorRejectsInsteadOfPostingFailedVerification(t *testing.T) {
	journal := filepath.Join(t.TempDir(), "agent-journal.json")
	job := testJob("unsupported", 10, "other", specDigest([]byte(`{"kind":"other"}`)))
	runScript(t, []hubStep{
		nextStep(job), claimStep(job.JobID), eventStep(job.JobID, "start", 1),
		rejectStep(job.JobID, deploy.PreconditionFailed, 2, `沒有 kind="other" 的 executor`), noJobStep(),
	}, journal, nil)
}

func TestIncompleteClaimReceiptNeverStartsExecutor(t *testing.T) {
	tests := map[string]model.JobLeaseResponse{
		"missing token":  {LeaseExpiresAt: jobsTestNow.Add(2 * time.Minute)},
		"blank token":    {LeaseToken: " lease ", LeaseExpiresAt: jobsTestNow.Add(2 * time.Minute)},
		"missing expiry": {LeaseToken: "lease-token"},
		"expired":        {LeaseToken: "lease-token", LeaseExpiresAt: jobsTestNow},
	}
	for name, receipt := range tests {
		t.Run(name, func(t *testing.T) {
			job := testJob("bad-claim", 10, "noop", specDigest([]byte(`{"kind":"noop"}`)))
			claim := claimStep(job.JobID)
			claim.Response = receipt
			var calls atomic.Int64
			exec := executorFunc(func(context.Context, model.JobResponse) ([]model.JobVerificationRequest, error) {
				calls.Add(1)
				return nil, nil
			})
			runScript(t, []hubStep{nextStep(job), claim, noJobStep()},
				filepath.Join(t.TempDir(), "agent-journal.json"), exec)
			if calls.Load() != 0 {
				t.Fatalf("invalid claim receipt started executor %d times", calls.Load())
			}
		})
	}
}

func TestExecutorErrorPostsFailedEvidenceThenCompletes(t *testing.T) {
	journal := filepath.Join(t.TempDir(), "agent-journal.json")
	job := testJob("execerr", 11, "noop", specDigest([]byte(`{"kind":"noop"}`)))
	exec := executorFunc(func(context.Context, model.JobResponse) ([]model.JobVerificationRequest, error) {
		return nil, errors.New(strings.Repeat("壞", 3000))
	})
	runScript(t, []hubStep{
		nextStep(job), claimStep(job.JobID), eventStep(job.JobID, "start", 1),
		verificationStep(job.JobID, func(t *testing.T, req model.JobVerificationRequest) {
			if req.Passed || req.RuleID != "executor" || len(req.StderrExcerpt) > 4<<10 {
				t.Errorf("executor 錯誤證據不對：passed=%v rule=%q stderr bytes=%d", req.Passed, req.RuleID, len(req.StderrExcerpt))
			}
		}),
		eventStep(job.JobID, "finish", 2), completeStep(job.JobID, string(deploy.Failed), false), noJobStep(),
	}, journal, exec)
}

func TestLeaseRenewFailureCancelsExecutor(t *testing.T) {
	tests := map[string]hubStep{
		"HTTP denial": {
			Method: http.MethodPost, Path: "/v1/jobs/renew/lease:renew", Status: http.StatusConflict,
			Response: model.APIError{Code: model.ErrLeaseInvalid, Message: "lost"},
		},
		"wrong token receipt": {
			Method: http.MethodPost, Path: "/v1/jobs/renew/lease:renew", Status: http.StatusOK,
			Response: model.JobLeaseResponse{LeaseToken: "other", LeaseExpiresAt: jobsTestNow.Add(2 * time.Minute)},
		},
		"expired receipt": {
			Method: http.MethodPost, Path: "/v1/jobs/renew/lease:renew", Status: http.StatusOK,
			Response: model.JobLeaseResponse{LeaseToken: "lease-renew", LeaseExpiresAt: jobsTestNow},
		},
	}
	for name, renew := range tests {
		t.Run(name, func(t *testing.T) {
			journal := filepath.Join(t.TempDir(), "agent-journal.json")
			job := testJob("renew", 12, "noop", specDigest([]byte(`{"kind":"noop"}`)))
			claim := claimStep(job.JobID)
			claim.Response = model.JobLeaseResponse{LeaseToken: "lease-renew", LeaseExpiresAt: jobsTestNow.Add(time.Minute)}
			hub := &scriptedHub{t: t, steps: []hubStep{nextStep(job), claim, eventStep(job.JobID, "start", 1), renew, noJobStep()}}
			server := newHubServer(t, hub)

			execStarted := make(chan struct{})
			execCanceled := make(chan struct{})
			exec := executorFunc(func(ctx context.Context, _ model.JobResponse) ([]model.JobVerificationRequest, error) {
				close(execStarted)
				<-ctx.Done()
				close(execCanceled)
				return nil, ctx.Err()
			})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			sleep := func(ctx context.Context, d time.Duration) error {
				switch d {
				case 20 * time.Second:
					select {
					case <-execStarted:
						return nil
					case <-ctx.Done():
						return ctx.Err()
					}
				case time.Second:
					cancel()
					return context.Canceled
				default:
					return nil
				}
			}
			runJobs(ctx, jobsOptions{
				HubURL: server.URL, Token: "agent-token", JournalPath: journal, PollInterval: time.Second,
				Executor: exec, Now: func() time.Time { return jobsTestNow }, Sleep: sleep,
				PollJitter: func(d time.Duration) time.Duration { return d },
			})
			hub.assertDone(t)
			select {
			case <-execCanceled:
			default:
				t.Fatal("續租失敗後 executor 沒收到 context 取消")
			}
		})
	}
}

func TestClaimConflictWaitsUntilLeaseExpiryCappedByPollThenPullsAgain(t *testing.T) {
	journal := filepath.Join(t.TempDir(), "agent-journal.json")
	job := testJob("busy", 13, "noop", specDigest([]byte(`{"kind":"noop"}`)))
	expires := jobsTestNow.Add(7 * time.Second)
	conflict := hubStep{
		Method: http.MethodPost, Path: "/v1/jobs/busy/claims", Status: http.StatusConflict,
		Response: model.JobStateError{
			APIError: model.APIError{Code: model.ErrJobStateConflict, Message: "busy"},
			State:    string(deploy.Claimed), LeaseExpiresAt: &expires,
		},
	}
	hub := &scriptedHub{t: t, steps: []hubStep{nextStep(job), conflict, noJobStep()}}
	server := newHubServer(t, hub)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var sleeps []time.Duration
	runJobs(ctx, jobsOptions{
		HubURL: server.URL, Token: "agent-token", JournalPath: journal, PollInterval: 30 * time.Second,
		Now: func() time.Time { return jobsTestNow }, PollJitter: func(d time.Duration) time.Duration { return d },
		Sleep: func(_ context.Context, d time.Duration) error {
			sleeps = append(sleeps, d)
			if d == 30*time.Second {
				cancel()
				return context.Canceled
			}
			return nil
		},
	})
	hub.assertDone(t)
	if len(sleeps) < 2 || sleeps[0] != 7*time.Second {
		t.Errorf("claims 409 後 sleeps=%v；第一個要約 7s", sleeps)
	}
}

func TestCompleteReplayIsAcceptedAsCompleted(t *testing.T) {
	journal := filepath.Join(t.TempDir(), "agent-journal.json")
	job := testJob("replay", 14, "noop", specDigest([]byte(`{"kind":"noop"}`)))
	runScript(t, []hubStep{
		nextStep(job), claimStep(job.JobID), eventStep(job.JobID, "start", 1), verificationStep(job.JobID, nil),
		eventStep(job.JobID, "finish", 2), completeStep(job.JobID, string(deploy.Succeeded), true), noJobStep(),
	}, journal, nil)
	if got := readWatermarks(t, journal); got.MaxApplied != 14 {
		t.Errorf("complete replayed succeeded 沒當成完成：%+v", got)
	}
}

func TestJobsDisabledNeverCallsNext(t *testing.T) {
	var requests atomic.Int64
	server := newHubServer(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startJobs(ctx, config{HubURL: server.URL, AgentToken: "token", JobsEnabled: false}, nil)
	if requests.Load() != 0 {
		t.Errorf("jobs_enabled=false 還呼叫 Hub %d 次", requests.Load())
	}
	var cfg config
	if err := json.Unmarshal([]byte(`{"hub_url":"x","agent_token":"y"}`), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.JobsEnabled {
		t.Error("沒寫 jobs_enabled 時預設必須是 false")
	}
}

func TestEventRetryReusesTheSameSequenceNumber(t *testing.T) {
	journal := filepath.Join(t.TempDir(), "agent-journal.json")
	job := testJob("retry", 15, "noop", specDigest([]byte(`{"kind":"noop"}`)))
	first := eventStep(job.JobID, "start", 1)
	first.Status = http.StatusInternalServerError
	first.Response = model.APIError{Code: "INTERNAL", Message: "try again"}
	steps := []hubStep{
		nextStep(job), claimStep(job.JobID), first, eventStep(job.JobID, "start", 1),
		verificationStep(job.JobID, nil), eventStep(job.JobID, "finish", 2),
		completeStep(job.JobID, string(deploy.Succeeded), false), noJobStep(),
	}
	records := runScript(t, steps, journal, nil)
	var seqs []int
	for _, record := range records {
		if record.Path == "/v1/jobs/retry/events" {
			var req model.JobEventRequest
			if err := json.Unmarshal(record.Body, &req); err != nil {
				t.Fatal(err)
			}
			seqs = append(seqs, req.Seq)
		}
	}
	if len(seqs) < 2 || seqs[0] != 1 || seqs[1] != 1 {
		t.Errorf("start 重送 seq=%v；前兩次必須都是 1", seqs)
	}
}

// ⚠ 守的是「日誌寫不進去時，記憶體裡的 MaxSeen 也要前進」。
// 43 因為寫不進日誌被拒之後，42 進來要被當成舊版（STALE_REVISION），
// 不是再拒一次 PRECONDITION_FAILED、更不是執行。executor 兩張都不准碰。
func TestJournalWriteFailureStillAdvancesMaxSeenInMemory(t *testing.T) {
	base := t.TempDir()
	journal := filepath.Join(base, "agent-journal.json")
	job43 := testJob("wf43", 43, "noop", specDigest([]byte(`{"kind":"noop"}`)))
	job42 := testJob("wf42", 42, "noop", specDigest([]byte(`{"kind":"noop"}`)))
	var calls atomic.Int64
	hub := &scriptedHub{t: t, steps: []hubStep{
		nextStep(job43), claimStep(job43.JobID),
		rejectStep(job43.JobID, deploy.PreconditionFailed, 1, "水位日誌寫不進去"),
		nextStep(job42), claimStep(job42.JobID),
		rejectStep(job42.JobID, deploy.StaleRevision, 1, "已見過 revision 43，這張是 42"),
		noJobStep(),
	}}
	server := newHubServer(t, hub)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runner, err := newJobsRunner(jobsOptions{
		HubURL: server.URL, Token: "agent-token", JournalPath: journal, PollInterval: time.Second,
		Executor: executorFunc(func(context.Context, model.JobResponse) ([]model.JobVerificationRequest, error) {
			calls.Add(1)
			return nil, nil
		}),
		Now: func() time.Time { return jobsTestNow }, PollJitter: func(d time.Duration) time.Duration { return d },
		Sleep: func(_ context.Context, d time.Duration) error {
			if d == time.Second {
				cancel()
				return context.Canceled
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(journal, 0o700); err != nil {
		t.Fatal(err)
	}
	runner.run(ctx)
	hub.assertDone(t)
	if calls.Load() != 0 {
		t.Errorf("水位寫不進去還呼叫 executor %d 次", calls.Load())
	}
}

func TestRejectErrorUsesItsOwnRejectionCode(t *testing.T) {
	journal := filepath.Join(t.TempDir(), "agent-journal.json")
	job := testJob("reject-error", 44, "noop", specDigest([]byte(`{"kind":"noop"}`)))
	exec := executorFunc(func(context.Context, model.JobResponse) ([]model.JobVerificationRequest, error) {
		return nil, &rejectError{Code: deploy.ArtifactHashMismatch, Detail: "expected abc got def"}
	})
	runScript(t, []hubStep{
		nextStep(job), claimStep(job.JobID), eventStep(job.JobID, "start", 1),
		rejectStep(job.JobID, deploy.ArtifactHashMismatch, 2, "expected abc got def"), noJobStep(),
	}, journal, exec)
}

func TestExecutionTimeoutAddsDeadlineToExecutorContext(t *testing.T) {
	journal := filepath.Join(t.TempDir(), "agent-journal.json")
	job := testJob("timeout", 45, "noop", specDigest([]byte(`{"kind":"noop"}`)))
	job.ExecutionTimeout = 7
	exec := executorFunc(func(ctx context.Context, _ model.JobResponse) ([]model.JobVerificationRequest, error) {
		deadline, ok := ctx.Deadline()
		if !ok {
			t.Fatal("executor context 沒有 deadline")
		}
		remaining := time.Until(deadline)
		if remaining <= 6*time.Second || remaining > 7*time.Second {
			t.Fatalf("executor deadline 剩 %s；要接近 7s", remaining)
		}
		return []model.JobVerificationRequest{{RuleID: "deadline", Passed: true}}, nil
	})
	runScript(t, []hubStep{
		nextStep(job), claimStep(job.JobID), eventStep(job.JobID, "start", 1), verificationStep(job.JobID, nil),
		eventStep(job.JobID, "finish", 2), completeStep(job.JobID, string(deploy.Succeeded), false), noJobStep(),
	}, journal, exec)
}

func TestTerminalSucceededNudgesObservationExactlyOnce(t *testing.T) {
	journal := filepath.Join(t.TempDir(), "agent-journal.json")
	job := testJob("nudge-success", 46, "noop", specDigest([]byte(`{"kind":"noop"}`)))
	nudge := make(chan struct{}, 4)
	runScriptWithNudge(t, []hubStep{
		nextStep(job), claimStep(job.JobID), eventStep(job.JobID, "start", 1), verificationStep(job.JobID, nil),
		eventStep(job.JobID, "finish", 2), completeStep(job.JobID, string(deploy.Succeeded), false), noJobStep(),
	}, journal, nil, nudge)
	if got := len(nudge); got != 1 {
		t.Errorf("succeeded 後 nudge=%d；要恰好 1", got)
	}
}

func TestTerminalRejectNudgesObservationExactlyOnce(t *testing.T) {
	journal := filepath.Join(t.TempDir(), "agent-journal.json")
	if err := saveJournal(journal, deploy.Watermarks{MaxSeen: 46, MaxApplied: 45}); err != nil {
		t.Fatal(err)
	}
	job := testJob("nudge-reject", 45, "noop", specDigest([]byte(`{"kind":"noop"}`)))
	nudge := make(chan struct{}, 4)
	runScriptWithNudge(t, []hubStep{
		nextStep(job), claimStep(job.JobID),
		rejectStep(job.JobID, deploy.StaleRevision, 1, "已見過 revision 46，這張是 45"), noJobStep(),
	}, journal, nil, nudge)
	if got := len(nudge); got != 1 {
		t.Errorf("rejected 後 nudge=%d；要恰好 1", got)
	}
}

func TestTwoTerminalJobsDoNotBlockOnOneBufferedNudge(t *testing.T) {
	journal := filepath.Join(t.TempDir(), "agent-journal.json")
	job1 := testJob("nudge-one", 47, "noop", specDigest([]byte(`{"kind":"noop"}`)))
	job2 := testJob("nudge-two", 48, "noop", specDigest([]byte(`{"kind":"noop"}`)))
	nudge := make(chan struct{}, 1)
	runScriptWithNudge(t, []hubStep{
		nextStep(job1), claimStep(job1.JobID), eventStep(job1.JobID, "start", 1), verificationStep(job1.JobID, nil),
		eventStep(job1.JobID, "finish", 2), completeStep(job1.JobID, string(deploy.Succeeded), false),
		nextStep(job2), claimStep(job2.JobID), eventStep(job2.JobID, "start", 1), verificationStep(job2.JobID, nil),
		eventStep(job2.JobID, "finish", 2), completeStep(job2.JobID, string(deploy.Succeeded), false), noJobStep(),
	}, journal, nil, nudge)
	if got := len(nudge); got != 1 {
		t.Errorf("兩張連續終態在一格 channel 應合併成一次排隊，got=%d", got)
	}
}

type retainingExecutor struct {
	journal string
	runErr  error
	calls   int
	applied deploy.Revision
}

func (e *retainingExecutor) Run(context.Context, model.JobResponse) ([]model.JobVerificationRequest, error) {
	if e.runErr != nil {
		return nil, e.runErr
	}
	return []model.JobVerificationRequest{{RuleID: "retainer", Passed: true}}, nil
}

func (e *retainingExecutor) AfterSucceeded(_ context.Context, _ model.JobResponse) []string {
	e.calls++
	w, err := loadJournal(e.journal)
	if err != nil {
		return []string{"測試讀不到日誌：" + err.Error()}
	}
	e.applied = w.MaxApplied
	return []string{"測試保留期已跑"}
}

func TestRetainerRunsOnlyAfterSucceededAndPersistedMaxApplied(t *testing.T) {
	for _, tc := range []struct {
		name      string
		state     deploy.JobState
		wantCalls int
	}{
		{name: "succeeded", state: deploy.Succeeded, wantCalls: 1},
		{name: "failed", state: deploy.Failed},
		{name: "manual intervention", state: deploy.ManualIntervention},
	} {
		t.Run(tc.name, func(t *testing.T) {
			journal := filepath.Join(t.TempDir(), "agent-journal.json")
			job := testJob("retain-"+strings.ReplaceAll(tc.name, " ", "-"), 49, "noop", specDigest([]byte(`{"kind":"noop"}`)))
			exec := &retainingExecutor{journal: journal}
			runScript(t, []hubStep{
				nextStep(job), claimStep(job.JobID), eventStep(job.JobID, "start", 1), verificationStep(job.JobID, nil),
				eventStep(job.JobID, "finish", 2), completeStep(job.JobID, string(tc.state), false), noJobStep(),
			}, journal, exec)
			if exec.calls != tc.wantCalls {
				t.Errorf("Hub 終態 %s 時 retainer calls=%d；要 %d", tc.state, exec.calls, tc.wantCalls)
			}
			if tc.state == deploy.Succeeded && exec.applied != deploy.Revision(job.Revision) {
				t.Errorf("retainer 看到 MaxApplied=%d；要先落地成 %d", exec.applied, job.Revision)
			}
		})
	}

	t.Run("rejected", func(t *testing.T) {
		journal := filepath.Join(t.TempDir(), "agent-journal.json")
		job := testJob("retain-rejected", 50, "noop", specDigest([]byte(`{"kind":"noop"}`)))
		exec := &retainingExecutor{journal: journal,
			runErr: &rejectError{Code: deploy.PreconditionFailed, Detail: "測試拒單"}}
		runScript(t, []hubStep{
			nextStep(job), claimStep(job.JobID), eventStep(job.JobID, "start", 1),
			rejectStep(job.JobID, deploy.PreconditionFailed, 2, "測試拒單"), noJobStep(),
		}, journal, exec)
		if exec.calls != 0 {
			t.Errorf("rejected 仍呼叫 retainer %d 次", exec.calls)
		}
	})
}

func TestKindExecutorForwardsRetentionOnlyToOpenClaw(t *testing.T) {
	retain := &retainingExecutor{journal: filepath.Join(t.TempDir(), "missing-journal.json")}
	exec := newKindExecutor(noopExecutor{}, deviceSyncExecutor{}, retain, nil, nil)
	openclaw := testArtifactJob("openclaw-retain", 1, strings.Repeat("a", 64), "sha256:"+strings.Repeat("a", 64))
	exec.AfterSucceeded(context.Background(), openclaw)
	noop := testJob("noop-retain", 2, "noop", specDigest([]byte(`{"kind":"noop"}`)))
	exec.AfterSucceeded(context.Background(), noop)
	if retain.calls != 1 {
		t.Errorf("kindExecutor 應只把 OpenClaw 成功轉給 retainer，calls=%d", retain.calls)
	}
}

func TestKindExecutorImplementsEveryRegisteredCatalogExecutorKind(t *testing.T) {
	exec := newKindExecutor(noopExecutor{}, deviceSyncExecutor{},
		executorFunc(func(context.Context, model.JobResponse) ([]model.JobVerificationRequest, error) {
			return nil, nil
		}),
		executorFunc(func(context.Context, model.JobResponse) ([]model.JobVerificationRequest, error) {
			return nil, nil
		}),
		executorFunc(func(context.Context, model.JobResponse) ([]model.JobVerificationRequest, error) {
			return nil, nil
		})).withKind(agentadapter.ExecutorKindClaudeCode, executorFunc(func(context.Context, model.JobResponse) ([]model.JobVerificationRequest, error) {
		return nil, nil
	})).withKind(agentadapter.ExecutorKindCodex, executorFunc(func(context.Context, model.JobResponse) ([]model.JobVerificationRequest, error) {
		return nil, nil
	})).withKind(agentadapter.ExecutorKindGrok, executorFunc(func(context.Context, model.JobResponse) ([]model.JobVerificationRequest, error) {
		return nil, nil
	})).withKind(agentadapter.ExecutorKindBATServer, executorFunc(func(context.Context, model.JobResponse) ([]model.JobVerificationRequest, error) {
		return nil, nil
	})).withKind(agentadapter.ExecutorKindAntigravity, executorFunc(func(context.Context, model.JobResponse) ([]model.JobVerificationRequest, error) {
		return nil, nil
	}))
	for _, kind := range agentadapter.ExecutorKinds() {
		if exec.executors[kind] == nil {
			t.Errorf("registered catalog executor kind %q has no dispatcher", kind)
		}
	}
	if exec.executors[model.DeviceSyncJobKind] == nil {
		t.Fatal("device-sync executor is not registered")
	}
}

func TestKindExecutorRejectsDuplicateSpecFieldsBeforeDispatch(t *testing.T) {
	calls := 0
	exec := newKindExecutor(noopExecutor{}, deviceSyncExecutor{}, executorFunc(func(context.Context, model.JobResponse) ([]model.JobVerificationRequest, error) {
		calls++
		return nil, nil
	}), nil, nil)
	job := testArtifactJob("duplicate-spec", 1, strings.Repeat("a", 64), "sha256:"+strings.Repeat("a", 64))
	job.Spec = []byte(`{"kind":"openclaw","kind":"openclaw"}`)
	_, err := exec.Run(t.Context(), job)
	var rejection *rejectError
	if !errors.As(err, &rejection) || rejection.Code != deploy.PreconditionFailed || calls != 0 {
		t.Fatalf("error=%v calls=%d", err, calls)
	}
}

func TestNoopExecutorReturnsConciseVerificationEvidence(t *testing.T) {
	verifiedAt := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	verification, err := (noopExecutor{now: func() time.Time { return verifiedAt }}).Run(
		context.Background(), testJob("noop-evidence", 1, "noop", specDigest([]byte(`{"kind":"noop"}`))))
	if err != nil {
		t.Fatal(err)
	}
	if len(verification) != 1 || verification[0].StdoutExcerpt != "工作單驗證完成。" ||
		verification[0].Command != "true" || !verification[0].Passed ||
		!verification[0].VerifiedAt.Equal(verifiedAt) {
		t.Fatalf("verification=%+v", verification)
	}
}

func TestDeviceSyncExecutorAcceptsOnlyTheFixedPrimitive(t *testing.T) {
	verifiedAt := time.Date(2026, 9, 14, 8, 0, 0, 0, time.UTC)
	executor := deviceSyncExecutor{now: func() time.Time { return verifiedAt }}
	job := testJob("device-sync", 1, model.DeviceSyncJobKind, specDigest([]byte(model.DeviceSyncSpecJSON)))
	job.ResourceKind, job.ResourceID = model.DeviceSyncResourceKind, model.DeviceSyncResourceID
	job.Spec, job.ExecutionTimeout = []byte(model.DeviceSyncSpecJSON), model.DeviceSyncDefaultTimeoutSeconds
	verification, err := executor.Run(t.Context(), job)
	if err != nil {
		t.Fatal(err)
	}
	if len(verification) != 1 || verification[0].RuleID != model.DeviceSyncVerificationRuleID ||
		verification[0].Command != model.DeviceSyncVerificationCommand || verification[0].ExitCode != 0 ||
		verification[0].StdoutExcerpt != model.DeviceSyncVerificationStdout ||
		!verification[0].Passed || !verification[0].VerifiedAt.Equal(verifiedAt) {
		t.Fatalf("verification=%+v", verification)
	}
}

func TestDeviceSyncExecutorRejectsEveryShapeExceptFixedV1(t *testing.T) {
	executor := deviceSyncExecutor{}
	for name, raw := range map[string]string{
		"wrong kind":    `{"kind":"noop","schema_version":1}`,
		"wrong version": `{"kind":"device-sync","schema_version":2}`,
		"unknown field": `{"kind":"device-sync","schema_version":1,"command":"true"}`,
		"trailing JSON": model.DeviceSyncSpecJSON + `{}`,
	} {
		t.Run(name, func(t *testing.T) {
			job := testJob("invalid-device-sync", 1, model.DeviceSyncJobKind, specDigest([]byte(raw)))
			job.ResourceKind, job.ResourceID = model.DeviceSyncResourceKind, model.DeviceSyncResourceID
			job.Spec, job.ExecutionTimeout = []byte(raw), model.DeviceSyncDefaultTimeoutSeconds
			if _, err := executor.Run(t.Context(), job); !errors.Is(err, errUnsupported) {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestDeviceSyncExecutorRejectsUnsafeEnvelope(t *testing.T) {
	base := testJob("device-sync-envelope", 1, model.DeviceSyncJobKind, specDigest([]byte(model.DeviceSyncSpecJSON)))
	base.ResourceKind, base.ResourceID = model.DeviceSyncResourceKind, model.DeviceSyncResourceID
	base.Spec, base.ExecutionTimeout = []byte(model.DeviceSyncSpecJSON), model.DeviceSyncDefaultTimeoutSeconds
	for name, mutate := range map[string]func(*model.JobResponse){
		"wrong resource kind": func(job *model.JobResponse) { job.ResourceKind = "openclaw" },
		"wrong resource id":   func(job *model.JobResponse) { job.ResourceID = "other" },
		"irreversible":        func(job *model.JobResponse) { job.Irreversible = true },
		"zero timeout":        func(job *model.JobResponse) { job.ExecutionTimeout = 0 },
		"oversized timeout":   func(job *model.JobResponse) { job.ExecutionTimeout = model.DeviceSyncMaxTimeoutSeconds + 1 },
	} {
		t.Run(name, func(t *testing.T) {
			job := base
			mutate(&job)
			if _, err := (deviceSyncExecutor{}).Run(t.Context(), job); !errors.Is(err, errUnsupported) {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestDeviceSyncJobSchedulesOneObservationOnlyAfterHubTerminal(t *testing.T) {
	journal := filepath.Join(t.TempDir(), "agent-journal.json")
	job := testJob("device-sync-nudge", 51, model.DeviceSyncJobKind, specDigest([]byte(model.DeviceSyncSpecJSON)))
	job.ResourceKind, job.ResourceID, job.Spec = model.DeviceSyncResourceKind, model.DeviceSyncResourceID, []byte(model.DeviceSyncSpecJSON)
	job.ExecutionTimeout = model.DeviceSyncDefaultTimeoutSeconds
	nudge := make(chan struct{}, 2)
	runScriptWithNudge(t, []hubStep{
		nextStep(job), claimStep(job.JobID), eventStep(job.JobID, "start", 1), verificationStep(job.JobID, nil),
		eventStep(job.JobID, "finish", 2), completeStep(job.JobID, string(deploy.Succeeded), false), noJobStep(),
	}, journal, newKindExecutor(noopExecutor{}, deviceSyncExecutor{}, nil, nil, nil), nudge)
	if got := len(nudge); got != 1 {
		t.Fatalf("device-sync terminal nudge=%d want=1", got)
	}
}

// launchJobs 在 runAgent 送 READY=1 之前被叫；掃描最長兩分鐘，同步等會撞 systemd 預設的 TimeoutStartSec=90s。
func TestLaunchJobsReturnsBeforeSweepFinishesAndRunsJobsOnlyAfter(t *testing.T) {
	release := make(chan struct{})
	ran := make(chan struct{}, 1)
	sweep := func(ctx context.Context) []string {
		select {
		case <-release:
		case <-ctx.Done():
			t.Error("掃描的 ctx 在放行前就到期了")
		}
		return []string{"掃描完了"}
	}
	run := func(context.Context, jobsOptions) { ran <- struct{}{} }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	returned := make(chan struct{})
	go func() {
		launchJobs(ctx, sweep, jobsOptions{}, run)
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("launchJobs 被掃描擋住了：agent 會在啟動時被 systemd 打死")
	}
	select {
	case <-ran:
		t.Fatal("掃描還沒完就起了工作單迴圈")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	select {
	case <-ran:
	case <-time.After(5 * time.Second):
		t.Fatal("掃描完了卻沒起工作單迴圈")
	}
}

func checkWindowsProductionExecutorsKeepNodeAndRejectLinuxOnlyKinds(t *testing.T) {
	openclaw, nodeRuntime, hermes, claude, codex, grok, batServer, antigravity := executorsForGOOS("windows", "http://hub.example", "token", func() time.Time { return jobsTestNow })
	if _, ok := nodeRuntime.(nodeRuntimeExecutor); !ok {
		t.Fatalf("windows node=%T", nodeRuntime)
	}
	if _, ok := claude.(claudeCodeExecutor); !ok {
		t.Fatalf("windows claude=%T", claude)
	}
	if _, ok := codex.(codexExecutor); !ok {
		t.Fatalf("windows codex=%T", codex)
	}
	if _, ok := grok.(grokExecutor); !ok {
		t.Fatalf("windows grok=%T", grok)
	}
	if _, ok := batServer.(unsupportedPlatformExecutor); !ok {
		t.Fatalf("windows bat-server=%T", batServer)
	}
	if _, ok := antigravity.(antigravityExecutor); !ok {
		t.Fatalf("windows antigravity=%T", antigravity)
	}
	jobs := map[string]model.JobResponse{
		agentadapter.ExecutorKindOpenClaw: testJob("win-openclaw", 1, agentadapter.ExecutorKindOpenClaw, "sha256:abc"),
		agentadapter.ExecutorKindHermes:   testJob("win-hermes", 1, agentadapter.ExecutorKindHermes, "sha256:abc"),
	}
	executors := map[string]executor{
		agentadapter.ExecutorKindOpenClaw: openclaw,
		agentadapter.ExecutorKindHermes:   hermes,
	}
	wired := newKindExecutor(noopExecutor{}, deviceSyncExecutor{}, openclaw, nodeRuntime, hermes)
	for kind, exec := range executors {
		if _, ok := exec.(unsupportedPlatformExecutor); !ok {
			t.Fatalf("%s on windows is %T, want unsupportedPlatformExecutor", kind, exec)
		}
		_, err := exec.Run(t.Context(), jobs[kind])
		if !errors.Is(err, errUnsupported) {
			t.Fatalf("%s err=%v", kind, err)
		}
		if !strings.Contains(err.Error(), kind) || !strings.Contains(err.Error(), "windows") {
			t.Fatalf("%s rejection=%q", kind, err)
		}
		for _, banned := range []string{"POSIX", "not implemented", "TODO", "尚未", "gap"} {
			if strings.Contains(err.Error(), banned) {
				t.Fatalf("%s rejection leaked %q: %v", kind, banned, err)
			}
		}
		if _, err := wired.Run(t.Context(), jobs[kind]); !errors.Is(err, errUnsupported) {
			t.Fatalf("kindExecutor %s err=%v", kind, err)
		}
	}
}

func checkLinuxProductionExecutorsKeepPOSIXKinds(t *testing.T) {
	openclaw, nodeRuntime, hermes, claude, codex, grok, batServer, antigravity := executorsForGOOS("linux", "http://hub.example", "token", func() time.Time { return jobsTestNow })
	if _, ok := claude.(claudeCodeExecutor); !ok {
		t.Fatalf("linux claude=%T", claude)
	}
	if _, ok := codex.(codexExecutor); !ok {
		t.Fatalf("linux codex=%T", codex)
	}
	if _, ok := grok.(grokExecutor); !ok {
		t.Fatalf("linux grok=%T", grok)
	}
	if _, ok := openclaw.(openclawExecutor); !ok {
		t.Fatalf("linux openclaw=%T", openclaw)
	}
	if _, ok := nodeRuntime.(nodeRuntimeExecutor); !ok {
		t.Fatalf("linux node=%T", nodeRuntime)
	}
	if _, ok := hermes.(hermesExecutor); !ok {
		t.Fatalf("linux hermes=%T", hermes)
	}
	if _, ok := batServer.(batServerExecutor); !ok {
		t.Fatalf("linux bat-server=%T", batServer)
	}
	if _, ok := antigravity.(antigravityExecutor); !ok {
		t.Fatalf("linux antigravity=%T", antigravity)
	}
}

func checkDarwinProductionExecutorsKeepNodeAndRejectLinuxOnlyKinds(t *testing.T) {
	openclaw, nodeRuntime, hermes, claude, codex, grok, batServer, antigravity := executorsForGOOS("darwin", "http://hub.example", "token", func() time.Time { return jobsTestNow })
	if _, ok := nodeRuntime.(nodeRuntimeExecutor); !ok {
		t.Fatalf("darwin node=%T", nodeRuntime)
	}
	if _, ok := claude.(claudeCodeExecutor); !ok {
		t.Fatalf("darwin claude=%T", claude)
	}
	if _, ok := codex.(codexExecutor); !ok {
		t.Fatalf("darwin codex=%T", codex)
	}
	if _, ok := grok.(grokExecutor); !ok {
		t.Fatalf("darwin grok=%T", grok)
	}
	if _, ok := openclaw.(unsupportedPlatformExecutor); !ok {
		t.Fatalf("darwin openclaw=%T", openclaw)
	}
	if _, ok := hermes.(unsupportedPlatformExecutor); !ok {
		t.Fatalf("darwin hermes=%T", hermes)
	}
	if _, ok := batServer.(unsupportedPlatformExecutor); !ok {
		t.Fatalf("darwin bat-server=%T", batServer)
	}
	if _, ok := antigravity.(antigravityExecutor); !ok {
		t.Fatalf("darwin antigravity=%T", antigravity)
	}
}

func TestPlatformProductionExecutors(t *testing.T) {
	t.Run("WindowsProductionExecutorsKeepNodeAndRejectLinuxOnlyKinds", checkWindowsProductionExecutorsKeepNodeAndRejectLinuxOnlyKinds)
	t.Run("LinuxProductionExecutorsKeepPOSIXKinds", checkLinuxProductionExecutorsKeepPOSIXKinds)
	t.Run("DarwinProductionExecutorsKeepNodeAndRejectLinuxOnlyKinds", checkDarwinProductionExecutorsKeepNodeAndRejectLinuxOnlyKinds)
}
