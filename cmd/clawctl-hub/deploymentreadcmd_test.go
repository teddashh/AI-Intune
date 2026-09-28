package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/deploy"
	"github.com/teddashh/AI-Intune/internal/model"
	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/operatorclient"
	"github.com/teddashh/AI-Intune/internal/store"
)

func TestDeploymentPromotionCLIShowsIndependentJobAndNextStep(t *testing.T) {
	var out bytes.Buffer
	err := writeDeploymentPromotion(&out, operator.DeploymentPromotionPreview{
		Allowed: false, Blockers: []string{"stable_promotion_locked"}, IndependentRequired: true,
		IndependentTargets: []operator.DeploymentPromotionIndependentTargetPreview{{
			MachineID: "canary-machine", DisplayName: "canary-one", JobID: "job-independent",
			State: "release_unreported", NextStep: operator.PromotionNextStepUpgradeAndReassignVerifier,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"independent=0/1", "CANARY MACHINE", "canary-one", "job-independent",
		"release_unreported", "升級後重新指派 verifier",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("promotion CLI 缺少 %q：\n%s", want, out.String())
		}
	}
}

func TestDeploymentJobCountsLeadWithSucceeded(t *testing.T) {
	got := formatJobCounts(map[deploy.JobState]int{deploy.Succeeded: 18, deploy.NotStarted: 12})
	if got != "18 succeeded / 12 not_started" {
		t.Fatalf("Hub 不在的計數句=%q", got)
	}
}

func TestDeploymentReadCLIUsesHTTPFirstRealRoutesAndSafeJSON(t *testing.T) {
	f := observedOperatorFixture(t)
	now := time.Now().UTC()
	if err := f.store.RecordCheckin(f.machine.id, model.Checkin{SentAt: now, AgentStartedAt: now}, now); err != nil {
		t.Fatal(err)
	}
	if err := f.store.RecordObservation(f.machine.id, model.ObservationBatch{
		SchemaVersion: model.SchemaVersion, MeasuredAt: now,
		OpenClaw: model.OpenClaw{Present: true, Install: &model.OpenClawInstall{NodeVersion: "24.15.0"}},
	}, now); err != nil {
		t.Fatal(err)
	}
	if err := f.store.SetMachineChannel(f.machine.id, "canary"); err != nil {
		t.Fatal(err)
	}
	record := writeJobTestArtifact(t, f.artifactsDir, "2026.9.8", "deployment-http-read")
	record.EnginesNode = ">=24.15.0 <25"
	record.FetchedBy = "artifact-fetch-user@private-host"
	record.TarballURL = "https://signed.example.invalid/private/openclaw.tgz?token=secret"
	if err := writeArtifactSidecar(f.artifactsDir, record); err != nil {
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
		t.Fatal("normal deployment read inspected a direct DB")
		return nil
	}
	deps.verifyHubStopped = func(context.Context, string) error {
		t.Fatal("normal deployment read checked the local Hub unit")
		return nil
	}
	missingDB := filepath.Join(t.TempDir(), "must-not-exist.sqlite")
	t.Setenv("CLAWCTL_DB", missingDB)

	var previewOut, errOut bytes.Buffer
	if err := runDeploymentReadCommandWithDeps(t.Context(), []string{
		"preview", "--channel", "canary", "--version", record.Version,
		"--artifact", record.SHA256, "--batch", "1", "--timeout", "123",
		"--irreversible", "--json",
	}, &previewOut, &errOut, deps); err != nil {
		t.Fatalf("HTTP deployment preview: %v; stderr=%s", err, errOut.String())
	}
	var preview operator.DeploymentCreatePreviewResult
	if err := json.Unmarshal(previewOut.Bytes(), &preview); err != nil ||
		preview.SchemaVersion != operator.DeploymentPreviewSchemaVersion ||
		preview.Artifact.SHA256 != record.SHA256 || preview.Impact != 1 ||
		preview.BatchSize != 1 || preview.ExecutionTimeoutSeconds != 123 ||
		!preview.Irreversible || preview.PreviewDigest == "" {
		t.Fatalf("safe preview=%+v err=%v body=%s", preview, err, previewOut.String())
	}
	assertSafeDeploymentCLIJSON(t, previewOut.Bytes(), record.TarballURL, record.FetchedBy)

	const rawCreatedBy = "DEPLOYMENT_CLI_PRIVATE_CREATED_BY"
	d := createDeploymentReadCLITestDeployment(t, f.store, f.machine.id, f.artifactsDir, record, rawCreatedBy)
	var listOut bytes.Buffer
	if err := runDeploymentReadCommandWithDeps(t.Context(), []string{
		"list", "--channel", "canary", "--state", store.DeploymentRunning,
		"--stuck", "false", "--limit", "1", "--json",
	}, &listOut, &errOut, deps); err != nil {
		t.Fatalf("HTTP deployment list: %v; stderr=%s", err, errOut.String())
	}
	var list operator.DeploymentListResult
	if err := json.Unmarshal(listOut.Bytes(), &list); err != nil ||
		list.Consistency != operator.DeploymentReadConsistencyLive || list.Total != 1 ||
		len(list.Items) != 1 || list.Items[0].DeploymentID != d.DeploymentID ||
		list.Items[0].DesiredRevision <= 0 || list.Items[0].ControlRevision < 0 {
		t.Fatalf("safe list=%+v err=%v body=%s", list, err, listOut.String())
	}
	assertSafeDeploymentCLIJSON(t, listOut.Bytes(), rawCreatedBy, record.TarballURL, record.FetchedBy)

	var detailOut bytes.Buffer
	if err := runDeploymentReadCommandWithDeps(t.Context(), []string{
		"show", d.DeploymentID, "--hub-url", base, "--json",
	}, &detailOut, &errOut, deps); err != nil {
		t.Fatalf("HTTP deployment show: %v; stderr=%s", err, errOut.String())
	}
	var detail operator.DeploymentDetailResult
	if err := json.Unmarshal(detailOut.Bytes(), &detail); err != nil ||
		detail.Item.DeploymentID != d.DeploymentID || len(detail.Targets) != 1 {
		t.Fatalf("safe detail=%+v err=%v body=%s", detail, err, detailOut.String())
	}
	assertSafeDeploymentCLIJSON(t, detailOut.Bytes(), rawCreatedBy, record.TarballURL, record.FetchedBy)
	if _, err := os.Stat(missingDB); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("HTTP deployment commands touched default DB: %v", err)
	}

	mu.Lock()
	gotCalls := append([]observedRequest(nil), calls...)
	mu.Unlock()
	if len(gotCalls) != 3 || gotCalls[0].method != http.MethodPost ||
		gotCalls[0].path != "/v1/operator/deployments/preview" ||
		gotCalls[1].method != http.MethodGet || gotCalls[1].path != "/v1/operator/deployments" ||
		gotCalls[2].method != http.MethodGet || gotCalls[2].path != "/v1/operator/deployments/"+d.DeploymentID {
		t.Fatalf("deployment HTTP calls=%+v", gotCalls)
	}
	if got := gotCalls[1].query; got.Get("channel") != "canary" ||
		got.Get("stuck") != "false" || got.Get("limit") != "1" ||
		len(got["state"]) != 1 || got["state"][0] != store.DeploymentRunning {
		t.Fatalf("deployment list query=%v", got)
	}
	for _, call := range gotCalls {
		if call.userAgent != operatorclient.UserAgent {
			t.Fatalf("deployment read User-Agent=%q", call.userAgent)
		}
	}
}

func TestDeploymentReadCLIExplicitDBUsesFencedDirectServiceAndArtifacts(t *testing.T) {
	dbPath, machineID := directDBFixture(t)
	t.Setenv("CLAWCTL_EXPECTATIONS", "")
	st, err := openExisting(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := st.RecordCheckin(machineID, model.Checkin{SentAt: now, AgentStartedAt: now}, now); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordObservation(machineID, model.ObservationBatch{
		SchemaVersion: model.SchemaVersion, MeasuredAt: now,
		OpenClaw: model.OpenClaw{Present: true, Install: &model.OpenClawInstall{NodeVersion: "24.15.0"}},
	}, now); err != nil {
		t.Fatal(err)
	}
	if err := st.SetMachineChannel(machineID, "canary"); err != nil {
		t.Fatal(err)
	}
	record := writeJobTestArtifact(t, artifactsDirFor(dbPath), "2026.9.8", "deployment-direct-read")
	record.EnginesNode = ">=24.15.0 <25"
	record.FetchedBy = "private-direct-actor"
	record.TarballURL = "https://signed.example.invalid/direct.tgz?token=secret"
	if err := writeArtifactSidecar(artifactsDirFor(dbPath), record); err != nil {
		t.Fatal(err)
	}
	loadExpectations(st)
	if err := st.PublishExpectationsPolicy(now); err != nil {
		t.Fatal(err)
	}
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
	var stoppedChecks atomic.Int32
	deps.verifyHubStopped = func(_ context.Context, gotPath string) error {
		if gotPath != dbPath {
			t.Fatalf("stopped-service proof path=%q want=%q", gotPath, dbPath)
		}
		stoppedChecks.Add(1)
		return nil
	}

	var previewOut, errOut bytes.Buffer
	if err := runDeploymentReadCommandWithDeps(t.Context(), []string{
		"preview", "--db", dbPath, "--channel", "canary", "--version", record.Version,
		"--artifact", record.SHA256, "--batch", "1", "--json",
	}, &previewOut, &errOut, deps); err != nil {
		t.Fatalf("direct deployment preview: %v; stderr=%s", err, errOut.String())
	}
	var preview operator.DeploymentCreatePreviewResult
	if err := json.Unmarshal(previewOut.Bytes(), &preview); err != nil || preview.Artifact.SHA256 != record.SHA256 || preview.Impact != 1 {
		t.Fatalf("direct preview=%+v err=%v body=%s", preview, err, previewOut.String())
	}
	assertSafeDeploymentCLIJSON(t, previewOut.Bytes(), record.TarballURL, record.FetchedBy)

	st, err = openExisting(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	const rawCreatedBy = "DIRECT_DEPLOYMENT_PRIVATE_CREATED_BY"
	d := createDeploymentReadCLITestDeployment(t, st, machineID, artifactsDirFor(dbPath), record, rawCreatedBy)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	var listOut bytes.Buffer
	if err := runDeploymentReadCommandWithDeps(t.Context(), []string{
		"list", "--db", dbPath, "--channel", "canary", "--json",
	}, &listOut, &errOut, deps); err != nil {
		t.Fatalf("direct deployment list: %v; stderr=%s", err, errOut.String())
	}
	var list operator.DeploymentListResult
	if err := json.Unmarshal(listOut.Bytes(), &list); err != nil || list.Total != 1 ||
		len(list.Items) != 1 || list.Items[0].DeploymentID != d.DeploymentID {
		t.Fatalf("direct list=%+v err=%v body=%s", list, err, listOut.String())
	}
	assertSafeDeploymentCLIJSON(t, listOut.Bytes(), rawCreatedBy, record.TarballURL, record.FetchedBy)

	var detailOut bytes.Buffer
	if err := runDeploymentReadCommandWithDeps(t.Context(), []string{
		"show", "--db", dbPath, "--json", d.DeploymentID,
	}, &detailOut, &errOut, deps); err != nil {
		t.Fatalf("direct deployment show: %v; stderr=%s", err, errOut.String())
	}
	var detail operator.DeploymentDetailResult
	if err := json.Unmarshal(detailOut.Bytes(), &detail); err != nil || detail.Item.DeploymentID != d.DeploymentID {
		t.Fatalf("direct detail=%+v err=%v body=%s", detail, err, detailOut.String())
	}
	assertSafeDeploymentCLIJSON(t, detailOut.Bytes(), rawCreatedBy, record.TarballURL, record.FetchedBy)
	if got := stoppedChecks.Load(); got != 3 {
		t.Fatalf("stopped-service checks=%d want=3", got)
	}
}

func TestDeploymentReadCLINeverImplicitlyFallsBackToDB(t *testing.T) {
	missingDB := filepath.Join(t.TempDir(), "must-not-exist.sqlite")
	t.Setenv("CLAWCTL_DB", missingDB)
	for _, args := range [][]string{
		{"list"},
		{"show", "deployment-id"},
		{"preview", "--channel", "canary", "--version", "2026.9.8"},
	} {
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
			err := runDeploymentReadCommandWithDeps(t.Context(), args, &out, &errOut, deps)
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

func TestDeploymentReadCLIRejectsAmbiguousArgumentsBeforeIO(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests.Add(1)
	}))
	defer server.Close()
	base, deps := machineHTTPTestDeps(t, server)
	deps.discoverHubURL = func() (string, error) {
		t.Fatal("invalid deployment arguments reached discovery")
		return "", nil
	}
	digest := strings.Repeat("a", 64)
	upperDigest := strings.Repeat("A", 64)
	for _, test := range []struct {
		name string
		args []string
	}{
		{name: "missing action", args: nil},
		{name: "unknown action", args: []string{"wat"}},
		{name: "list positional", args: []string{"list", "extra"}},
		{name: "list channel", args: []string{"list", "--hub-url", base, "--channel", "preview"}},
		{name: "list empty channel", args: []string{"list", "--hub-url", base, "--channel", ""}},
		{name: "list unknown state", args: []string{"list", "--hub-url", base, "--state", "unknown"}},
		{name: "list duplicate state", args: []string{"list", "--hub-url", base, "--state", "running", "--state", "running"}},
		{name: "list bad stuck", args: []string{"list", "--hub-url", base, "--stuck", "yes"}},
		{name: "list empty stuck", args: []string{"list", "--hub-url", base, "--stuck", ""}},
		{name: "list zero limit", args: []string{"list", "--hub-url", base, "--limit", "0"}},
		{name: "list large limit", args: []string{"list", "--hub-url", base, "--limit", "101"}},
		{name: "list empty cursor", args: []string{"list", "--hub-url", base, "--cursor", ""}},
		{name: "list cursor whitespace", args: []string{"list", "--hub-url", base, "--cursor", " cursor"}},
		{name: "list mixed transports", args: []string{"list", "--hub-url", base, "--db", "/not-used"}},
		{name: "list empty hub URL", args: []string{"list", "--hub-url", ""}},
		{name: "list spaced hub URL", args: []string{"list", "--hub-url", " " + base}},
		{name: "list empty DB", args: []string{"list", "--db", ""}},
		{name: "show missing ID", args: []string{"show", "--hub-url", base}},
		{name: "show two IDs", args: []string{"show", "--hub-url", base, "one", "two"}},
		{name: "show two IDs ID first", args: []string{"show", "one", "--hub-url", base, "two"}},
		{name: "show slash", args: []string{"show", "--hub-url", base, "one/two"}},
		{name: "show dot", args: []string{"show", "--hub-url", base, ".."}},
		{name: "show whitespace", args: []string{"show", "--hub-url", base, " one"}},
		{name: "show list filter", args: []string{"show", "--hub-url", base, "--state", "running", "one"}},
		{name: "preview missing channel", args: []string{"preview", "--hub-url", base, "--version", "2026.9.8"}},
		{name: "preview missing version", args: []string{"preview", "--hub-url", base, "--channel", "canary"}},
		{name: "preview empty version", args: []string{"preview", "--hub-url", base, "--channel", "canary", "--version", ""}},
		{name: "preview spaced version", args: []string{"preview", "--hub-url", base, "--channel", "canary", "--version", " 2026.9.8"}},
		{name: "preview empty artifact", args: []string{"preview", "--hub-url", base, "--channel", "canary", "--version", "2026.9.8", "--artifact", ""}},
		{name: "preview short artifact", args: []string{"preview", "--hub-url", base, "--channel", "canary", "--version", "2026.9.8", "--artifact", digest[:63]}},
		{name: "preview uppercase artifact", args: []string{"preview", "--hub-url", base, "--channel", "canary", "--version", "2026.9.8", "--artifact", upperDigest}},
		{name: "preview zero batch", args: []string{"preview", "--hub-url", base, "--channel", "canary", "--version", "2026.9.8", "--batch", "0"}},
		{name: "preview large batch", args: []string{"preview", "--hub-url", base, "--channel", "canary", "--version", "2026.9.8", "--batch", "6"}},
		{name: "preview zero timeout", args: []string{"preview", "--hub-url", base, "--channel", "canary", "--version", "2026.9.8", "--timeout", "0"}},
		{name: "preview large timeout", args: []string{"preview", "--hub-url", base, "--channel", "canary", "--version", "2026.9.8", "--timeout", "86401"}},
		{name: "preview positional", args: []string{"preview", "--hub-url", base, "--channel", "canary", "--version", "2026.9.8", "extra"}},
		{name: "preview mutation flag", args: []string{"preview", "--hub-url", base, "--channel", "canary", "--version", "2026.9.8", "--by", "actor"}},
		{name: "preview mixed transports", args: []string{"preview", "--hub-url", base, "--db", "/not-used", "--channel", "canary", "--version", "2026.9.8"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			if err := runDeploymentReadCommandWithDeps(t.Context(), test.args, &out, &errOut, deps); err == nil {
				t.Fatalf("invalid args %q were accepted; stdout=%q stderr=%q", test.args, out.String(), errOut.String())
			}
			if out.Len() != 0 {
				t.Fatalf("invalid args %q wrote stdout=%q", test.args, out.String())
			}
		})
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("invalid deployment arguments reached HTTP server %d times", got)
	}
}

func createDeploymentReadCLITestDeployment(t *testing.T, st *store.Store, machineID, artifactsDir string,
	record artifactSidecar, createdBy string,
) store.Deployment {
	t.Helper()
	material, err := prepareOpenClawJob(artifactsDir, record.Version, record.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	d, jobs, err := st.CreateDeployment(store.NewDeployment{
		Channel: "canary", ResourceKind: "openclaw", ResourceID: "openclaw",
		Spec: material.Spec, BatchSize: 1, CreatedBy: createdBy,
		Targets: []store.NewDeploymentTarget{{MachineID: machineID, BatchNo: 1}},
		Job:     store.NewJob{ArtifactDigest: material.Digest, ExecutionTimeout: 600},
	})
	if err != nil || len(jobs) != 1 {
		t.Fatalf("create deployment=%+v jobs=%+v err=%v", d, jobs, err)
	}
	return d
}

func assertSafeDeploymentCLIJSON(t *testing.T, raw []byte, forbiddenValues ...string) {
	t.Helper()
	for _, forbidden := range forbiddenValues {
		if forbidden != "" && bytes.Contains(raw, []byte(forbidden)) {
			t.Errorf("deployment CLI JSON exposed forbidden value %q: %s", forbidden, raw)
		}
	}
	var document any
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatalf("decode deployment CLI JSON safety document: %v; body=%s", err, raw)
	}
	assertDeploymentCLIJSONHasNoKeys(t, document, map[string]bool{
		"spec": true, "created_by": true, "path": true, "tarball_url": true,
		"upstream_tarball_url": true, "fetched_by": true, "reason": true, "reasons": true,
	})
}

func assertDeploymentCLIJSONHasNoKeys(t *testing.T, value any, forbidden map[string]bool) {
	t.Helper()
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if forbidden[key] {
				t.Errorf("deployment CLI JSON exposed forbidden key %q", key)
			}
			assertDeploymentCLIJSONHasNoKeys(t, child, forbidden)
		}
	case []any:
		for _, child := range typed {
			assertDeploymentCLIJSONHasNoKeys(t, child, forbidden)
		}
	}
}

// TestDeploymentDetailCLIStatesIndependentEvidenceAsEvidence checks the text
// output carries the same per-target verdict the page does, prints the target
// count rather than a conclusion about the deployment, and leaves a target with
// no job blank instead of giving it a verdict.
func TestDeploymentDetailCLIStatesIndependentEvidenceAsEvidence(t *testing.T) {
	jobID := "job-alpha"
	failed := deploy.Failed
	createdAt := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	terminalAt := createdAt.Add(time.Minute)
	result := operator.DeploymentDetailResult{
		SchemaVersion: operator.DeploymentReadSchemaVersion,
		Consistency:   operator.DeploymentReadConsistencyLive,
		EvaluatedAt:   createdAt.Add(time.Hour),
		Item: operator.DeploymentSummary{
			DeploymentID: "deployment-1", Channel: "canary", State: store.DeploymentPaused,
			BatchSize: 1, CreatedAt: createdAt,
		},
		Targets: []operator.DeploymentTargetSummary{
			{
				MachineID: "machine-a", DisplayName: "alpha", BatchNo: 1, JobID: &jobID,
				JobState: &failed, JobCreatedAt: &createdAt, TerminalAt: &terminalAt,
				LastActivityAt: &terminalAt,
				Independent: &operator.DeploymentTargetIndependent{
					Verdict: "digest_mismatch", Rows: 2, LiveProducers: 2,
				},
			},
			{MachineID: "machine-b", DisplayName: "beta", BatchNo: 2},
		},
		Independent: operator.DeploymentIndependentSummary{
			OpenedTargets: 1, PassedTargets: 0, LiveProducers: 2,
			Verdicts: []operator.DeploymentIndependentVerdictCount{
				{Verdict: "absent", Targets: 0}, {Verdict: "producer_revoked", Targets: 0},
				{Verdict: "digest_mismatch", Targets: 1}, {Verdict: "release_mismatch", Targets: 0},
				{Verdict: "stale", Targets: 0}, {Verdict: "failed", Targets: 0},
				{Verdict: "release_unreported", Targets: 0}, {Verdict: "passed", Targets: 0},
			},
		},
	}

	var out bytes.Buffer
	if err := writeDeploymentDetail(&out, result, false, "test"); err != nil {
		t.Fatalf("write deployment detail: %v", err)
	}
	text := out.String()
	for _, want := range []string{
		"independent: 0/1 台已開單 target 有第二個 producer 回報通過", "可用 producer 2",
		"INDEPENDENT", "digest_mismatch (2 rows / 2 live)",
		"第二個 producer 看到的 artifact digest 與這張單的不同",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("deployment detail CLI missing %q in:\n%s", want, text)
		}
	}
	// Verdicts no target reached are not printed: a row of zeroes says nothing
	// an operator can act on and hides the one verdict that does.
	for _, absent := range []string{"absent：", "stale：", "passed："} {
		if strings.Contains(text, absent) {
			t.Errorf("deployment detail CLI printed an unreached verdict %q", absent)
		}
	}
	for _, banned := range []string{"已驗證", "驗證通過", "驗證成功", "已確認", "healthy", "健康"} {
		if strings.Contains(text, banned) {
			t.Errorf("deployment detail CLI promoted a producer report into a conclusion: %q", banned)
		}
	}
	// The unopened target must print a dash, not a verdict about a job that was
	// never created.
	beta := text[strings.Index(text, "beta"):]
	if line := beta[:strings.Index(beta, "\n")]; !strings.HasSuffix(strings.TrimRight(line, " "), `"-"`) {
		t.Errorf("unopened target line carries a verdict: %q", line)
	}
}
