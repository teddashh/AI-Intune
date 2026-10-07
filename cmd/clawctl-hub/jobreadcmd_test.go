package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/deploy"
	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/operatorclient"
	"github.com/teddashh/AI-Intune/internal/store"
)

// jobIndependentConclusionCopy are words that would turn a second producer's
// report into the Hub's own conclusion about a job.
var jobIndependentConclusionCopy = []string{
	"已驗證", "驗證通過", "驗證成功", "已確認", "確認無誤",
	"healthy", "健康", "正常運作", "is current", "up to date",
}

func TestJobReadCLIUsesHTTPFirstAndEmitsSafeJSON(t *testing.T) {
	f := observedOperatorFixture(t)
	const rawMarker = "JOB_CLI_RAW_SPEC_AND_CREATED_BY_SENTINEL"
	jobID := createOperatorReadJob(t, f.store, f.machine.id, "cnode-operator", rawMarker)
	leaseToken, err := f.store.ClaimJob(jobID, f.machine.id, time.Now().UTC(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.AppendJobEvent(jobID, f.machine.id, leaseToken, 7, "rejected",
		`{"rejection_code":"PRECONDITION_FAILED","detail":"`+rawMarker+`"}`,
		time.Now().UTC(), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	type observedRequest struct {
		method    string
		path      string
		query     url.Values
		userAgent string
	}
	var mu sync.Mutex
	var calls []observedRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls = append(calls, observedRequest{
			method: r.Method, path: r.URL.Path, query: r.URL.Query(), userAgent: r.UserAgent(),
		})
		mu.Unlock()
		f.mux.ServeHTTP(w, r)
	}))
	defer server.Close()
	base, deps := machineHTTPTestDeps(t, server)
	deps.discoverHubURL = func() (string, error) { return base, nil }
	deps.validateDirectPath = func(string) error {
		t.Fatal("normal job read inspected a direct DB")
		return nil
	}
	deps.verifyHubStopped = func(context.Context, string) error {
		t.Fatal("normal job read checked the local Hub unit")
		return nil
	}
	missingDB := filepath.Join(t.TempDir(), "must-not-exist.sqlite")
	t.Setenv("CLAWCTL_DB", missingDB)

	var listOut, errOut bytes.Buffer
	if err := runJobReadCommandWithDeps(t.Context(), []string{
		"list", "--json", "--machine", f.machine.id,
		"--state", string(deploy.Claimed),
		"--resource-kind", "diagnostic", "--resource-id", "noop", "--limit", "1",
	}, &listOut, &errOut, deps); err != nil {
		t.Fatalf("HTTP job list: %v; stderr=%s", err, errOut.String())
	}
	var list operator.JobListResult
	if err := json.Unmarshal(listOut.Bytes(), &list); err != nil {
		t.Fatalf("decode list JSON: %v; body=%s", err, listOut.String())
	}
	if list.SchemaVersion != operator.JobReadSchemaVersion || list.Total != 1 ||
		len(list.Items) != 1 || list.Items[0].JobID != jobID ||
		list.Items[0].LeaseStatus != operator.JobLeaseActive {
		t.Fatalf("safe list envelope=%+v", list)
	}
	assertSafeJobCLIJSON(t, listOut.Bytes(), rawMarker, leaseToken)

	var detailOut bytes.Buffer
	if err := runJobReadCommandWithDeps(t.Context(), []string{
		"show", "--hub-url", base, "--json", jobID,
	}, &detailOut, &errOut, deps); err != nil {
		t.Fatalf("HTTP job show: %v; stderr=%s", err, errOut.String())
	}
	var detail operator.JobDetailResult
	if err := json.Unmarshal(detailOut.Bytes(), &detail); err != nil {
		t.Fatalf("decode detail JSON: %v; body=%s", err, detailOut.String())
	}
	if detail.SchemaVersion != operator.JobReadSchemaVersion || detail.Item.JobID != jobID ||
		detail.Desired.DesiredID != detail.Item.DesiredID || detail.Events.Items == nil ||
		detail.Verifications.Items == nil {
		t.Fatalf("safe detail envelope=%+v", detail)
	}
	assertSafeJobCLIJSON(t, detailOut.Bytes(), rawMarker, leaseToken)
	if _, err := os.Stat(missingDB); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("HTTP job reads touched default DB: %v", err)
	}

	mu.Lock()
	gotCalls := append([]observedRequest(nil), calls...)
	mu.Unlock()
	if len(gotCalls) != 2 || gotCalls[0].method != http.MethodGet ||
		gotCalls[0].path != "/v1/operator/jobs" || gotCalls[1].method != http.MethodGet ||
		gotCalls[1].path != "/v1/operator/jobs/"+jobID {
		t.Fatalf("job read HTTP calls=%+v", gotCalls)
	}
	if got := gotCalls[0].query; got.Get("machine_id") != f.machine.id ||
		got.Get("resource_kind") != "diagnostic" || got.Get("resource_id") != "noop" ||
		got.Get("limit") != "1" || len(got["state"]) != 1 || got["state"][0] != string(deploy.Claimed) {
		t.Fatalf("job list query=%v", got)
	}
	for _, call := range gotCalls {
		if call.userAgent != operatorclient.UserAgent {
			t.Fatalf("job read User-Agent=%q", call.userAgent)
		}
	}
}

func TestJobReadCLIExplicitDBUsesFencedDirectService(t *testing.T) {
	dbPath, machineID := directDBFixture(t)
	st, err := openExisting(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	jobID := createOperatorReadJob(t, st, machineID, "direct-machine", "DIRECT_JOB_SECRET")
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	deps := productionMachineCommandDeps()
	deps.discoverHubURL = func() (string, error) {
		t.Fatal("explicit --db invoked Hub discovery")
		return "", nil
	}
	deps.newOperatorClient = func(string) (*operatorclient.Client, error) {
		t.Fatal("explicit --db constructed an HTTP client")
		return nil, nil
	}
	stoppedChecks := 0
	deps.verifyHubStopped = func(_ context.Context, gotPath string) error {
		stoppedChecks++
		if gotPath != dbPath {
			t.Fatalf("stopped-service proof path=%q want=%q", gotPath, dbPath)
		}
		return nil
	}

	var listOut, errOut bytes.Buffer
	if err := runJobReadCommandWithDeps(t.Context(), []string{
		"list", "--db", dbPath, "--machine", "direct-machine", "--json",
	}, &listOut, &errOut, deps); err != nil {
		t.Fatalf("direct job list: %v; stderr=%s", err, errOut.String())
	}
	var list operator.JobListResult
	if err := json.Unmarshal(listOut.Bytes(), &list); err != nil || list.Total != 1 ||
		len(list.Items) != 1 || list.Items[0].JobID != jobID || list.Items[0].MachineID != machineID {
		t.Fatalf("direct list=%+v err=%v body=%s", list, err, listOut.String())
	}
	assertSafeJobCLIJSON(t, listOut.Bytes(), "DIRECT_JOB_SECRET")

	var detailOut bytes.Buffer
	if err := runJobReadCommandWithDeps(t.Context(), []string{
		"show", "--db", dbPath, "--json", jobID,
	}, &detailOut, &errOut, deps); err != nil {
		t.Fatalf("direct job show: %v; stderr=%s", err, errOut.String())
	}
	var detail operator.JobDetailResult
	if err := json.Unmarshal(detailOut.Bytes(), &detail); err != nil || detail.Item.JobID != jobID {
		t.Fatalf("direct detail=%+v err=%v body=%s", detail, err, detailOut.String())
	}
	assertSafeJobCLIJSON(t, detailOut.Bytes(), "DIRECT_JOB_SECRET")
	if stoppedChecks != 2 {
		t.Fatalf("stopped-service checks=%d want=2", stoppedChecks)
	}
}

func TestJobEvidenceCLIUsesHTTPFirstAndFencedDB(t *testing.T) {
	f := observedOperatorFixture(t)
	jobID := createOperatorReadJob(t, f.store, f.machine.id, "cnode-operator", "EVIDENCE_CLI_SPEC")
	leaseToken, err := f.store.ClaimJob(jobID, f.machine.id, time.Now().UTC(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	base := "http://100.64.0.9:8787"
	client, _ := operatorClientForMuxWithoutListener(t, f.mux, base)
	deps := productionMachineCommandDeps()
	deps.discoverHubURL = func() (string, error) { return base, nil }
	deps.newOperatorClient = func(got string) (*operatorclient.Client, error) {
		if got != base {
			return nil, errors.New("unexpected evidence operator authority")
		}
		return client, nil
	}
	deps.validateDirectPath = func(string) error {
		t.Fatal("HTTP job evidence inspected a direct DB")
		return nil
	}
	deps.verifyHubStopped = func(context.Context, string) error {
		t.Fatal("HTTP job evidence checked the local Hub unit")
		return nil
	}
	missingDB := filepath.Join(t.TempDir(), "must-not-exist.sqlite")
	t.Setenv("CLAWCTL_DB", missingDB)

	var out, errOut bytes.Buffer
	if err := runJobReadCommandWithDeps(t.Context(), []string{
		"evidence", jobID, "--json", "--limit", "1",
	}, &out, &errOut, deps); err != nil {
		t.Fatalf("HTTP job evidence: %v; stderr=%s", err, errOut.String())
	}
	var evidence operator.JobEvidenceResult
	if err := json.Unmarshal(out.Bytes(), &evidence); err != nil || evidence.JobID != jobID ||
		evidence.Disclosure.Limit != 1 || evidence.Desired.Spec.Text == "" {
		t.Fatalf("job evidence=%+v err=%v body=%s", evidence, err, out.String())
	}
	if bytes.Contains(out.Bytes(), []byte(leaseToken)) || bytes.Contains(out.Bytes(), []byte(`"lease_token"`)) {
		t.Fatalf("job evidence JSON leaked lease token: %s", out.String())
	}
	if _, err := os.Stat(missingDB); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("HTTP job evidence touched default DB: %v", err)
	}

	var invalidOut bytes.Buffer
	if err := runJobReadCommandWithDeps(t.Context(), []string{
		"evidence", "--hub-url", base, "--limit", "101", jobID,
	}, &invalidOut, &errOut, deps); err == nil {
		t.Fatal("job evidence accepted --limit 101")
	}
	if invalidOut.Len() != 0 {
		t.Fatalf("invalid job evidence wrote stdout=%q", invalidOut.String())
	}

	dbPath, machineID := directDBFixture(t)
	st, err := openExisting(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	directJobID := createOperatorReadJob(t, st, machineID, "direct-machine", "DIRECT_EVIDENCE")
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	directDeps := productionMachineCommandDeps()
	directDeps.discoverHubURL = func() (string, error) {
		t.Fatal("explicit evidence --db invoked discovery")
		return "", nil
	}
	directDeps.newOperatorClient = func(string) (*operatorclient.Client, error) {
		t.Fatal("explicit evidence --db constructed HTTP client")
		return nil, nil
	}
	directDeps.verifyHubStopped = func(context.Context, string) error {
		return errors.New("clawctl-hub.service is active")
	}
	var directOut bytes.Buffer
	err = runJobReadCommandWithDeps(t.Context(), []string{
		"evidence", "--db", dbPath, "--json", directJobID,
	}, &directOut, &errOut, directDeps)
	if err == nil || !strings.Contains(err.Error(), "clawctl-hub.service is active") || directOut.Len() != 0 {
		t.Fatalf("live-Hub evidence fence error=%v stdout=%q", err, directOut.String())
	}
}

func TestJobReadCLINeverImplicitlyFallsBackToDB(t *testing.T) {
	missingDB := filepath.Join(t.TempDir(), "must-not-exist.sqlite")
	t.Setenv("CLAWCTL_DB", missingDB)
	for _, args := range [][]string{{"list"}, {"show", "job-id"}} {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			deps := machineCommandDeps{
				discoverHubURL: func() (string, error) { return "", errors.New("discovery unavailable") },
				validateDirectPath: func(string) error {
					t.Fatal("discovery failure inspected a direct DB")
					return nil
				},
				verifyHubStopped: func(context.Context, string) error {
					t.Fatal("discovery failure checked the local Hub unit")
					return nil
				},
			}
			var out, errOut bytes.Buffer
			err := runJobReadCommandWithDeps(t.Context(), args, &out, &errOut, deps)
			if err == nil || !strings.Contains(err.Error(), "discovery unavailable") ||
				out.Len() != 0 {
				t.Fatalf("args=%q error=%v stdout=%q stderr=%q", args, err, out.String(), errOut.String())
			}
		})
	}
	if _, err := os.Stat(missingDB); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed discovery created fallback DB: %v", err)
	}
}

func TestJobReadCLIValidatesStatesAndFiltersBeforeRequest(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests.Add(1)
	}))
	defer server.Close()
	base, deps := machineHTTPTestDeps(t, server)
	for _, test := range []struct {
		name string
		args []string
	}{
		{name: "unknown state", args: []string{"list", "--hub-url", base, "--state", "unknown"}},
		{name: "duplicate state", args: []string{"list", "--hub-url", base, "--state", "running", "--state", "running"}},
		{name: "resource id without kind", args: []string{"list", "--hub-url", base, "--resource-id", "resource"}},
		{name: "zero limit", args: []string{"list", "--hub-url", base, "--limit", "0"}},
		{name: "oversized limit", args: []string{"list", "--hub-url", base, "--limit", "101"}},
		{name: "empty explicit machine", args: []string{"list", "--hub-url", base, "--machine", ""}},
		{name: "trimmed machine identity", args: []string{"list", "--hub-url", base, "--machine", " machine-id"}},
		{name: "control in machine identity", args: []string{"list", "--hub-url", base, "--machine", "machine\nid"}},
		{name: "oversized cursor", args: []string{"list", "--hub-url", base, "--cursor", strings.Repeat("a", 2049)}},
		{name: "show list filter", args: []string{"show", "--hub-url", base, "--state", "running", "job-id"}},
		{name: "show path separator", args: []string{"show", "--hub-url", base, "job/id"}},
		{name: "mixed transports", args: []string{"list", "--hub-url", base, "--db", "/not-used"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			if err := runJobReadCommandWithDeps(t.Context(), test.args, &out, &errOut, deps); err == nil {
				t.Fatalf("invalid args %q were accepted; stdout=%q stderr=%q", test.args, out.String(), errOut.String())
			}
			if out.Len() != 0 {
				t.Fatalf("invalid args %q wrote stdout=%q", test.args, out.String())
			}
		})
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("invalid job filters reached HTTP server %d times", got)
	}
}

func TestJobCreateRemainsFencedDuringUpgradeMaintenance(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "clawctl.sqlite")
	t.Setenv("CLAWCTL_DB", dbPath)
	if err := os.WriteFile(upgradeMaintenanceMarker(dbPath), []byte(upgradeMaintenanceContents), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := rejectTopLevelCLIWhileUpgradeMaintenance("job", []string{
		"job", "create", "--kind", "noop",
	}); err == nil || !strings.Contains(err.Error(), "upgrade maintenance") {
		t.Fatalf("job create escaped maintenance fence: %v", err)
	}
	for _, argv := range [][]string{
		{"job", "list", "--hub-url", "http://100.64.0.9:8787"},
		{"job", "show", "--hub-url", "http://100.64.0.9:8787", "job-id"},
	} {
		if err := rejectTopLevelCLIWhileUpgradeMaintenance("job", argv); err != nil {
			t.Fatalf("read-only argv=%q was coupled to local maintenance: %v", argv, err)
		}
	}
}

func assertSafeJobCLIJSON(t *testing.T, raw []byte, forbiddenValues ...string) {
	t.Helper()
	for _, forbidden := range forbiddenValues {
		if forbidden != "" && bytes.Contains(raw, []byte(forbidden)) {
			t.Errorf("job CLI JSON exposed forbidden value %q: %s", forbidden, raw)
		}
	}
	var document any
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatalf("decode job CLI JSON safety document: %v; body=%s", err, raw)
	}
	assertNoSensitiveJobReadKeys(t, document)
}

// TestJobIndependentEvidenceCLIStatesEvidenceNotAConclusion guards the twelve
// statements returned for eight verdicts and four assignment states. Pinning
// each verdict to its own sentence catches a copy change whose expected value
// was forgotten; checking forbidden conclusion words in all twelve statements
// catches the more dangerous drift where nicer-sounding copy and its expected
// value are changed together. The assignment statements belong here for that
// second reason: they can also turn a producer's report into the Hub's verdict.
func TestJobIndependentEvidenceCLIStatesEvidenceNotAConclusion(t *testing.T) {
	seen := map[string]string{}
	for _, verdict := range []string{
		string(store.IndependentAbsent), string(store.IndependentProducerRevoked),
		string(store.IndependentDigestMismatch), string(store.IndependentReleaseMismatch),
		string(store.IndependentStale), string(store.IndependentFailed),
		string(store.IndependentReleaseUnreported), string(store.IndependentPassed),
	} {
		statement := jobIndependentVerdictStatement(verdict)
		if statement == "" {
			t.Errorf("verdict %q got %q；expected 自己的非空句子；operator 會看不到第二個 producer 對這張工作單回報了什麼", verdict, statement)
		}
		if other, ok := seen[statement]; ok {
			t.Errorf("verdict %q got %q；expected 與 verdict %q 的句子不同；operator 會把兩種不同的證據狀態當成同一種結果", verdict, statement, other)
		}
		seen[statement] = verdict
		assertJobIndependentCopyIsNotAConclusion(t, "verdict "+verdict, statement)
	}
	unknown := jobIndependentVerdictStatement("not-a-verdict")
	if _, taken := seen[unknown]; taken {
		t.Errorf("unknown verdict got %q；expected 不得重用八種已知 verdict 的句子；operator 會把未知狀態誤認成已有明確處置的狀態", unknown)
	}

	for _, state := range []string{
		operator.JobAssignmentReported, operator.JobAssignmentProducerRevoked,
		operator.JobAssignmentWaitingForJob, operator.JobAssignmentAwaitingReport,
	} {
		statement := jobAssignmentStateStatement(state)
		assertJobIndependentCopyIsNotAConclusion(t, "assignment state "+state, statement)
	}
}

// TestJobIndependentEvidenceCLIHubHardCodedFramingStatesNoConclusion guards
// the Hub's own hard-coded framing, not rendered output. This section's job is
// to print a second producer's evidence unchanged, so healthy or verified in a
// display_name, failure_domain, or stdout excerpt is legal: it is the producer's
// report, not the Hub's conclusion. Scanning rendered output would conflate
// quoting another producer with reaching a conclusion and would fail once a
// fixture became realistic. This is the CLI counterpart to scanning
// templates/*.html on the web side. verified_at is allowed because jobreadcmd.go
// already prints VERIFIED_AT(verifier)= and reported_verified_at=.
func TestJobIndependentEvidenceCLIHubHardCodedFramingStatesNoConclusion(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "jobreadcmd.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	ast.Inspect(file, func(node ast.Node) bool {
		literal, ok := node.(*ast.BasicLit)
		if !ok || literal.Kind != token.STRING {
			return true
		}
		value, err := strconv.Unquote(literal.Value)
		if err == nil {
			assertJobIndependentCopyIsNotAConclusion(t, "jobreadcmd.go string literal", value)
		}
		return true
	})
}

func assertJobIndependentCopyIsNotAConclusion(t *testing.T, surface, value string) {
	t.Helper()
	lowered := strings.ToLower(value)
	for _, banned := range jobIndependentConclusionCopy {
		if strings.Contains(lowered, strings.ToLower(banned)) {
			t.Errorf("%s got string literal %q containing %q；expected Hub 固定文案不替第二個 producer 下結論；job show 會把 producer 的回報印成 Hub 已查過且判定沒有問題", surface, value, banned)
		}
	}
	for _, word := range verifiedWordOccurrences(lowered) {
		if word != "verified_at" {
			t.Errorf("%s got string literal %q containing word %q；expected verified 只出現在欄位名 verified_at；operator 會把 producer 提供的證據誤讀成 Hub 已驗證工作單", surface, value, word)
		}
	}
}

// verifiedWordOccurrences returns each occurrence of "verified" together with
// following identifier characters, distinguishing verified_at from verified.
func verifiedWordOccurrences(lowered string) []string {
	var found []string
	for i := 0; ; {
		at := strings.Index(lowered[i:], "verified")
		if at < 0 {
			return found
		}
		at += i
		end := at + len("verified")
		for end < len(lowered) && (lowered[end] == '_' ||
			(lowered[end] >= 'a' && lowered[end] <= 'z')) {
			end++
		}
		found = append(found, lowered[at:end])
		i = end
	}
}

// passed 也涵蓋「所有列都沒回報 digest」——那是沒有比過，不是比過而且一樣。
// 2026-09-12 第一次真機獨立驗證就是這個形狀：三條規則全通過、observed_digest 全空。
func TestThePassedStatementNeverClaimsADigestComparisonThatDidNotHappen(t *testing.T) {
	terminalAt := time.Date(2026, 9, 12, 7, 25, 0, 0, time.UTC)
	verdict := store.EvaluateIndependentVerdict(
		"sha256:"+strings.Repeat("a", 64), "", &terminalAt,
		[]store.IndependentVerdictInput{{Passed: true, ReceivedAt: terminalAt.Add(time.Minute)}})
	if verdict != store.IndependentPassed {
		t.Fatalf("沒回報 digest 的通過列算出 %q，這支測試盯的是 passed 那一句", verdict)
	}
	statement := jobIndependentVerdictStatement(string(verdict))
	for _, claim := range []string{"digest 與這張單相同", "digest 相同", "digest 一致", "digest 相符"} {
		if strings.Contains(statement, claim) {
			t.Fatalf("passed 那一句說「%s」，但落到 passed 的列可以一個 digest 都沒回報。\n實際句子：%s",
				claim, statement)
		}
	}
}

// TestJobIndependentEvidenceCLIRendersBothClocksAndTheProducer covers the
// fields an operator needs to tell the two producers apart: who wrote the row,
// which failure domain it sits in, when it says it looked, and when the Hub
// actually received it.
func TestJobIndependentEvidenceCLIRendersBothClocksAndTheProducer(t *testing.T) {
	verified := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	section := &operator.JobIndependentEvidence{
		Verdict: string(store.IndependentPassed), Total: 1, LiveProducers: 1,
		ArtifactDigest: "sha256:" + strings.Repeat("a", 64), ExpectedVersion: "2026.9.2",
		Items: []operator.JobIndependentVerification{{
			VerificationID: "independent-1",
			RuleID: operator.JobEvidenceText{
				Text: "openclaw.current_release", MaxBytes: 256, Bytes: len("openclaw.current_release"),
			},
			Command: operator.JobEvidenceText{Text: "GET /health", MaxBytes: 16384, Bytes: 11},
			Passed:  true, DigestReported: true, DigestMatchesJob: true,
			ObservedDigest:  "sha256:" + strings.Repeat("a", 64),
			ObservedVersion: "2026.9.2", VersionReported: true, VersionMatchesJob: true,
			ReportedVerifiedAt: verified, ReceivedAt: verified.Add(3 * time.Second),
			Producer: operator.JobIndependentProducer{
				Kind: store.VerifierKindFleetPeerAgent, VerifierID: "verifier-1",
				DisplayName:   operator.JobEvidenceText{Text: "peer", MaxBytes: 256, Bytes: 4},
				FailureDomain: operator.JobEvidenceText{Text: "machine-peer", MaxBytes: 256, Bytes: 12},
				Authority:     "verifier_bearer", EvidenceRole: "independent_verifier", State: "active",
			},
		}},
	}
	var out bytes.Buffer
	if err := writeJobIndependentEvidence(&out, section); err != nil {
		t.Fatal(err)
	}
	body := out.String()
	for _, want := range []string{
		"independent: 1", "1 live producers", "independent_verdict",
		"VERIFIED_AT(verifier)", "RECEIVED_AT(Hub)",
		"fleet_peer_agent", "verifier-1", "machine-peer",
		"verifier_bearer", "independent_verifier", "digest_matches_job",
		`expected_version: "2026.9.2"`, `observed_version="2026.9.2"`, "version_matches_job=true",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("independent CLI evidence missing %q in:\n%s", want, body)
		}
	}

	// A job nothing independent has written still gets the section. Printing
	// nothing there would leave the executor's own account looking corroborated.
	out.Reset()
	if err := writeJobIndependentEvidence(&out, &operator.JobIndependentEvidence{
		Verdict: string(store.IndependentAbsent),
		Items:   []operator.JobIndependentVerification{},
	}); err != nil {
		t.Fatal(err)
	}
	absent := out.String()
	for _, want := range []string{
		"independent: 0", "0 live producers", string(store.IndependentAbsent),
		jobIndependentVerdictStatement(string(store.IndependentAbsent)),
	} {
		if !strings.Contains(absent, want) {
			t.Errorf("a job with no independent evidence must still say %q, got:\n%s", want, absent)
		}
	}
}

func TestJobIndependentEvidenceCLIDistinguishesAbsentWithAwaitingAssignment(t *testing.T) {
	assignedAt := time.Date(2026, 9, 10, 13, 14, 15, 123, time.UTC)
	reportedAt := time.Date(2026, 9, 10, 14, 15, 16, 456, time.UTC)
	assignment := operator.JobIndependentAssignment{
		AssignmentID: "ASSIGNMENT_ID_MUST_NOT_APPEAR",
		VerifierID:   "verifier-2",
		DisplayName: operator.JobEvidenceText{
			Text: "peer\nX", MaxBytes: 256, Bytes: 321, Truncated: true,
		},
		FailureDomain: operator.JobEvidenceText{
			Text: "machine-peer", MaxBytes: 256, Bytes: 654, Truncated: true,
		},
		State:      operator.JobAssignmentAwaitingReport,
		AssignedAt: assignedAt,
	}
	secondAssignment := operator.JobIndependentAssignment{
		VerifierID: "verifier-3",
		DisplayName: operator.JobEvidenceText{
			Text: "peer-3", MaxBytes: 256, Bytes: len("peer-3"),
		},
		FailureDomain: operator.JobEvidenceText{
			Text: "machine-peer-3", MaxBytes: 256, Bytes: len("machine-peer-3"),
		},
		State:      operator.JobAssignmentWaitingForJob,
		AssignedAt: assignedAt.Add(time.Second),
	}
	render := func(section *operator.JobIndependentEvidence) string {
		var out bytes.Buffer
		if err := writeJobIndependentEvidence(&out, section); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}
	withAssignment := render(&operator.JobIndependentEvidence{
		Verdict:     string(store.IndependentAbsent),
		Items:       []operator.JobIndependentVerification{},
		Assignments: []operator.JobIndependentAssignment{assignment, secondAssignment},
	})
	withoutAssignment := render(&operator.JobIndependentEvidence{
		Verdict: string(store.IndependentAbsent),
		Items:   []operator.JobIndependentVerification{},
	})
	if withAssignment == withoutAssignment {
		t.Errorf("兩種 absent 的 CLI 輸出 got 有派工=%q 與無派工=%q；expected 兩者不同；operator 分不出「沒人被叫來看」和「有人被叫了沒回」，而這兩個下一步相反", withAssignment, withoutAssignment)
	}
	if !strings.Contains(withAssignment, `verifier="verifier-2"`) {
		t.Errorf("有派工的 absent 輸出 got %q；expected 包含 %q；display_name 是可重複也可為空的人為標籤，verifier_id 才是撤銷或改派的參數，缺少它會讓 operator 知道有人欠答案卻不知道要催誰", withAssignment, `verifier="verifier-2"`)
	}
	if !strings.Contains(withAssignment, `failure_domain="machine-peer"`) {
		t.Errorf("有派工的 absent 輸出 got %q；expected 包含 %q；operator 看不出跨故障域指派是否真的買到獨立性，也無法判斷下一步應改派還是等回報", withAssignment, `failure_domain="machine-peer"`)
	}
	if !strings.Contains(withAssignment, "assigned_at=2026-09-10T13:14:15.000000123Z") {
		t.Errorf("有派工的 absent 輸出 got %q；expected 包含 %q；operator 無法區分三秒前才發出而應等待，或六天前發出仍未回而應催促或改派", withAssignment, "assigned_at=2026-09-10T13:14:15.000000123Z")
	}
	if !strings.Contains(withAssignment, `display_name="peer\nX" [truncated; ledger bytes=321]`) {
		t.Errorf("截斷 display name 的輸出 got %q；expected 包含 %q；截斷值看似完整會讓 operator 拿被切掉一半的名字比對 verifier 而認錯人", withAssignment, `display_name="peer\nX" [truncated; ledger bytes=321]`)
	}
	if !strings.Contains(withAssignment, `failure_domain="machine-peer" [truncated; ledger bytes=654]`) {
		t.Errorf("截斷 failure domain 的輸出 got %q；expected 包含 %q；截斷值看似完整會讓 operator 誤判 verifier 所在故障域，因而錯估證據獨立性", withAssignment, `failure_domain="machine-peer" [truncated; ledger bytes=654]`)
	}
	if !strings.Contains(withAssignment, "assignments: 2") {
		t.Errorf("兩筆派工的輸出 got %q；expected 包含 %q；若標成 assignments: 0 卻列出兩列，腳本 grep 會把兩個 verifier 已被指派誤報成沒人被指派", withAssignment, "assignments: 2")
	}
	if !strings.Contains(withAssignment, string(operator.JobAssignmentAwaitingReport)) {
		t.Errorf("有派工的 absent 輸出 got %q；expected 包含 %q；operator 無法用 raw token 定位尚未回報的派工", withAssignment, operator.JobAssignmentAwaitingReport)
	}
	if !strings.Contains(withAssignment, "dispatched, awaiting report") {
		t.Errorf("有派工的 absent 輸出 got %q；expected 包含 %q；operator 不知道應追蹤已發出但未回報的 verifier", withAssignment, "dispatched, awaiting report")
	}
	if !strings.Contains(withoutAssignment, "assignments: 0") {
		t.Errorf("無派工的輸出 got %q；expected 包含 %q；operator 會把區塊沉默誤讀成已有 verifier 在處理", withoutAssignment, "assignments: 0")
	}
	if !strings.Contains(withoutAssignment, "no verifiers have been assigned to this job yet") {
		t.Errorf("無派工的輸出 got %q；expected 包含 %q；operator 不知道下一步是先指派 verifier", withoutAssignment, "no verifiers have been assigned to this job yet")
	}
	if !strings.Contains(withAssignment, "reported_at=not reported") {
		t.Errorf("ReportedAt nil 的輸出 got %q；expected 包含 %q；operator 可能把未回報誤當成已回報", withAssignment, "reported_at=not reported")
	}
	if strings.Contains(withAssignment, "0001-01-01") {
		t.Errorf("ReportedAt nil 的輸出 got %q；expected 不包含 %q；operator 可能把零值時間誤當真實回報時間", withAssignment, "0001-01-01")
	}
	if strings.Contains(withAssignment, "peer\nX") {
		t.Errorf("含換行的 display name 輸出 got %q；expected 不包含原始跨行值 %q；攻擊者可偽造 CLI 新行", withAssignment, "peer\nX")
	}
	if !strings.Contains(withAssignment, `"peer\nX"`) {
		t.Errorf("含換行的 display name 輸出 got %q；expected 包含終端安全形式 %q；operator 無法安全辨識 verifier", withAssignment, `"peer\nX"`)
	}
	if strings.Contains(withAssignment, "ASSIGNMENT_ID_MUST_NOT_APPEAR") {
		t.Errorf("text 輸出 got %q；expected 不包含 AssignmentID %q；operator 會看到這頁無法使用的內部 ID", withAssignment, "ASSIGNMENT_ID_MUST_NOT_APPEAR")
	}

	assignment.ReportedAt = &reportedAt
	withReport := render(&operator.JobIndependentEvidence{
		Verdict:     string(store.IndependentAbsent),
		Items:       []operator.JobIndependentVerification{},
		Assignments: []operator.JobIndependentAssignment{assignment, secondAssignment},
	})
	if !strings.Contains(withReport, "reported_at=2026-09-10T14:15:16.000000456Z") {
		t.Errorf("ReportedAt 非 nil 的輸出 got %q；expected 包含 %q；operator 無法確認 verifier 實際回報時間", withReport, "reported_at=2026-09-10T14:15:16.000000456Z")
	}
}

func TestJobAssignmentStateStatementGivesEachStateItsOwnConsequence(t *testing.T) {
	cases := []struct {
		state       string
		want        string
		consequence string
	}{
		{operator.JobAssignmentReported, "independent evidence submitted", "operator 應改讀已送出的證據"},
		{operator.JobAssignmentProducerRevoked, "assigned verifier revoked, will not report", "operator 應改指派可用的 verifier"},
		{operator.JobAssignmentWaitingForJob, "waiting for job completion before dispatch", "operator 應等工作單結束"},
		{operator.JobAssignmentAwaitingReport, "dispatched, awaiting report", "operator 應追蹤尚未回報的 verifier"},
	}
	seen := make(map[string]string, len(cases))
	for _, tc := range cases {
		got := jobAssignmentStateStatement(tc.state)
		if got != tc.want {
			t.Errorf("state %q got %q；expected %q；%s", tc.state, got, tc.want, tc.consequence)
		}
		if priorState, exists := seen[got]; exists {
			t.Errorf("state %q got %q；expected 與 state %q 的句子不同；%s，但 operator 無法區分對應行動", tc.state, got, priorState, tc.consequence)
		}
		seen[got] = tc.state
	}
	unknown := jobAssignmentStateStatement("future_state")
	for _, tc := range cases {
		if unknown == tc.want {
			t.Errorf("未知 state got %q；expected 不得套用已知 state %q 的句子 %q；operator 會執行錯誤的已知處置", unknown, tc.state, tc.want)
		}
	}
}
