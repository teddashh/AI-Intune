package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
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

	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/operatorclient"
	"github.com/teddashh/AI-Intune/internal/store"
)

func TestArtifactReadCLIUsesDiscoveredAndExplicitOperatorAPIWithSafeJSON(t *testing.T) {
	f := observedOperatorFixture(t)
	first := writeJobTestArtifact(t, f.artifactsDir, "2026.9.8", "artifact-cli-first")
	second := writeJobTestArtifact(t, f.artifactsDir, "2026.9.8", "artifact-cli-second")
	first.TarballURL = "https://registry.example/private.tgz?token=RAW_ARTIFACT_URL_SECRET_ONE"
	first.FetchedBy = "RAW_ARTIFACT_FETCHED_BY_SECRET_ONE"
	second.TarballURL = "https://registry.example/private.tgz?token=RAW_ARTIFACT_URL_SECRET_TWO"
	second.FetchedBy = "RAW_ARTIFACT_FETCHED_BY_SECRET_TWO"
	for _, record := range []artifactSidecar{first, second} {
		if err := writeArtifactSidecar(f.artifactsDir, record); err != nil {
			t.Fatal(err)
		}
	}

	type observedRequest struct {
		method, path, userAgent string
		query                   url.Values
	}
	var mu sync.Mutex
	var calls []observedRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls = append(calls, observedRequest{
			method: r.Method, path: r.URL.Path, userAgent: r.UserAgent(), query: r.URL.Query(),
		})
		mu.Unlock()
		f.mux.ServeHTTP(w, r)
	}))
	defer server.Close()
	base, deps := machineHTTPTestDeps(t, server)
	deps.discoverHubURL = func() (string, error) { return base, nil }
	deps.validateDirectPath = func(string) error {
		t.Fatal("normal artifact read inspected direct DB")
		return nil
	}
	deps.verifyHubStopped = func(context.Context, string) error {
		t.Fatal("normal artifact read checked the local Hub unit")
		return nil
	}
	missingDB := filepath.Join(t.TempDir(), "must-not-exist.sqlite")
	t.Setenv("CLAWCTL_DB", missingDB)

	var firstPageOut, errOut bytes.Buffer
	if err := runArtifactReadCommandWithDeps(t.Context(), []string{
		"list", "--json", "--status", "available_unverified", "--version", "2026.9.8", "--limit", "1",
	}, &firstPageOut, &errOut, deps); err != nil {
		t.Fatalf("discovered artifact list: %v; stderr=%s", err, errOut.String())
	}
	var firstPage operator.ArtifactListResult
	if err := json.Unmarshal(firstPageOut.Bytes(), &firstPage); err != nil || firstPage.Total != 2 ||
		len(firstPage.Items) != 1 || firstPage.NextCursor == nil ||
		firstPage.Items[0].Status != operator.ArtifactAvailableUnverified || firstPage.Items[0].VerifiedAt != nil {
		t.Fatalf("first page=%+v err=%v body=%s", firstPage, err, firstPageOut.String())
	}
	assertSafeArtifactCLIOutput(t, firstPageOut.Bytes(), f.artifactsDir,
		first.TarballURL, first.FetchedBy, second.TarballURL, second.FetchedBy)

	var secondPageOut bytes.Buffer
	if err := runArtifactReadCommandWithDeps(t.Context(), []string{
		"list", "--hub-url", base, "--json", "--status", "available_unverified",
		"--version", "2026.9.8", "--limit", "1", "--cursor", *firstPage.NextCursor,
	}, &secondPageOut, &errOut, deps); err != nil {
		t.Fatalf("explicit artifact continuation: %v; stderr=%s", err, errOut.String())
	}
	var secondPage operator.ArtifactListResult
	if err := json.Unmarshal(secondPageOut.Bytes(), &secondPage); err != nil || secondPage.Total != 2 ||
		len(secondPage.Items) != 1 || secondPage.NextCursor != nil ||
		secondPage.Items[0].ArtifactID == firstPage.Items[0].ArtifactID {
		t.Fatalf("second page=%+v err=%v body=%s", secondPage, err, secondPageOut.String())
	}
	assertSafeArtifactCLIOutput(t, secondPageOut.Bytes(), f.artifactsDir,
		first.TarballURL, first.FetchedBy, second.TarballURL, second.FetchedBy)

	var detailOut bytes.Buffer
	if err := runArtifactReadCommandWithDeps(t.Context(), []string{
		"show", first.SHA256, "--hub-url", base, "--json",
	}, &detailOut, &errOut, deps); err != nil {
		t.Fatalf("explicit artifact show: %v; stderr=%s", err, errOut.String())
	}
	var detail operator.ArtifactDetailResult
	if err := json.Unmarshal(detailOut.Bytes(), &detail); err != nil || detail.Item.ArtifactID != first.SHA256 ||
		detail.Item.Status != operator.ArtifactReady || detail.Item.VerifiedAt == nil ||
		!detail.Item.VerifiedAt.Equal(detail.EvaluatedAt) {
		t.Fatalf("detail=%+v err=%v body=%s", detail, err, detailOut.String())
	}
	assertSafeArtifactCLIOutput(t, detailOut.Bytes(), f.artifactsDir,
		first.TarballURL, first.FetchedBy, second.TarballURL, second.FetchedBy)
	if _, err := os.Stat(missingDB); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("HTTP artifact reads touched default DB: %v", err)
	}

	mu.Lock()
	gotCalls := append([]observedRequest(nil), calls...)
	mu.Unlock()
	if len(gotCalls) != 3 || gotCalls[0].method != http.MethodGet ||
		gotCalls[0].path != "/v1/operator/artifacts" || gotCalls[1].method != http.MethodGet ||
		gotCalls[1].path != "/v1/operator/artifacts" || gotCalls[2].method != http.MethodGet ||
		gotCalls[2].path != "/v1/operator/artifacts/"+first.SHA256 {
		t.Fatalf("artifact CLI calls=%+v", gotCalls)
	}
	for index, call := range gotCalls {
		if call.userAgent != operatorclient.UserAgent {
			t.Fatalf("call %d User-Agent=%q", index, call.userAgent)
		}
	}
	for index, call := range gotCalls[:2] {
		if call.query.Get("status") != "available_unverified" || call.query.Get("version") != "2026.9.8" ||
			call.query.Get("limit") != "1" {
			t.Fatalf("list call %d query=%v", index, call.query)
		}
	}
	if gotCalls[0].query.Get("cursor") != "" || gotCalls[1].query.Get("cursor") != *firstPage.NextCursor ||
		len(gotCalls[2].query) != 0 {
		t.Fatalf("artifact cursor/detail queries=%+v", gotCalls)
	}
}

func TestArtifactReadCLIExplicitDBUsesFencedCanonicalService(t *testing.T) {
	dbPath, _ := directDBFixture(t)
	record := writeJobTestArtifact(t, artifactsDirFor(dbPath), "2026.9.8", "artifact-direct-read")
	record.TarballURL = "https://registry.example/private.tgz?token=RAW_DIRECT_ARTIFACT_URL"
	record.FetchedBy = "RAW_DIRECT_ARTIFACT_FETCHED_BY"
	if err := writeArtifactSidecar(artifactsDirFor(dbPath), record); err != nil {
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
	originalValidate := deps.validateDirectPath
	originalOpen := deps.openDirectDB
	validated, stopped, opened := 0, 0, 0
	deps.validateDirectPath = func(got string) error {
		if got != dbPath {
			t.Fatalf("validated DB=%q want=%q", got, dbPath)
		}
		validated++
		return originalValidate(got)
	}
	deps.verifyHubStopped = func(_ context.Context, got string) error {
		if got != dbPath || validated == 0 {
			t.Fatalf("stopped proof before path validation: path=%q validated=%d", got, validated)
		}
		stopped++
		return nil
	}
	deps.openDirectDB = func(got string) (*store.Store, error) {
		if got != dbPath || stopped <= opened {
			t.Fatalf("openExisting before stopped proof: path=%q stopped=%d opened=%d", got, stopped, opened)
		}
		opened++
		return originalOpen(got)
	}

	var listOut, errOut bytes.Buffer
	if err := runArtifactReadCommandWithDeps(t.Context(), []string{
		"list", "--db", dbPath, "--version", record.Version, "--json",
	}, &listOut, &errOut, deps); err != nil {
		t.Fatalf("direct artifact list: %v; stderr=%s", err, errOut.String())
	}
	var list operator.ArtifactListResult
	if err := json.Unmarshal(listOut.Bytes(), &list); err != nil || list.Total != 1 || len(list.Items) != 1 ||
		list.Items[0].ArtifactID != record.SHA256 || list.Items[0].Status != operator.ArtifactAvailableUnverified {
		t.Fatalf("direct list=%+v err=%v body=%s", list, err, listOut.String())
	}
	assertSafeArtifactCLIOutput(t, listOut.Bytes(), dbPath, artifactsDirFor(dbPath), record.TarballURL, record.FetchedBy)

	var detailOut bytes.Buffer
	if err := runArtifactReadCommandWithDeps(t.Context(), []string{
		"show", record.SHA256, "--db", dbPath, "--json",
	}, &detailOut, &errOut, deps); err != nil {
		t.Fatalf("direct artifact show: %v; stderr=%s", err, errOut.String())
	}
	var detail operator.ArtifactDetailResult
	if err := json.Unmarshal(detailOut.Bytes(), &detail); err != nil || detail.Item.Status != operator.ArtifactReady ||
		detail.Item.VerifiedAt == nil || !detail.Item.VerifiedAt.Equal(detail.EvaluatedAt) {
		t.Fatalf("direct detail=%+v err=%v body=%s", detail, err, detailOut.String())
	}
	assertSafeArtifactCLIOutput(t, detailOut.Bytes(), dbPath, artifactsDirFor(dbPath), record.TarballURL, record.FetchedBy)

	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	var canceledOut bytes.Buffer
	err := runArtifactReadCommandWithDeps(canceledCtx, []string{
		"show", record.SHA256, "--db", dbPath, "--json",
	}, &canceledOut, &errOut, deps)
	if !errors.Is(err, context.Canceled) || canceledOut.Len() != 0 {
		t.Fatalf("canceled direct detail err=%v stdout=%q", err, canceledOut.String())
	}
	if stopped != 3 || opened != 3 || validated < 12 {
		t.Fatalf("direct fence calls validated=%d stopped=%d opened=%d", validated, stopped, opened)
	}
}

func TestArtifactReadCLIDiscoveryFailureNeverFallsBackToDB(t *testing.T) {
	missingDB := filepath.Join(t.TempDir(), "must-not-exist.sqlite")
	t.Setenv("CLAWCTL_DB", missingDB)
	deps := machineCommandDeps{
		discoverHubURL: func() (string, error) { return "", errors.New("discovery unavailable") },
		validateDirectPath: func(string) error {
			t.Fatal("discovery failure inspected a direct DB")
			return nil
		},
		verifyHubStopped: func(context.Context, string) error {
			t.Fatal("discovery failure checked local Hub")
			return nil
		},
	}
	for _, args := range [][]string{{"list"}, {"show", strings.Repeat("a", 64)}} {
		var out, errOut bytes.Buffer
		err := runArtifactReadCommandWithDeps(t.Context(), args, &out, &errOut, deps)
		if err == nil || !strings.Contains(err.Error(), "discovery unavailable") ||
			out.Len() != 0 {
			t.Fatalf("args=%q err=%v stdout=%q stderr=%q", args, err, out.String(), errOut.String())
		}
	}
	if _, err := os.Stat(missingDB); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed discovery created fallback DB: %v", err)
	}
}

func TestArtifactReadCLIRejectsAmbiguousAndInvalidArgumentsBeforeIO(t *testing.T) {
	var calls atomic.Int32
	deps := machineCommandDeps{
		discoverHubURL: func() (string, error) {
			calls.Add(1)
			return "http://100.64.0.9:8787", nil
		},
		newOperatorClient: func(string) (*operatorclient.Client, error) {
			calls.Add(1)
			return nil, errors.New("must not construct client")
		},
		validateDirectPath: func(string) error {
			calls.Add(1)
			return errors.New("must not inspect DB")
		},
	}
	digest := strings.Repeat("a", 64)
	for _, test := range []struct {
		name string
		args []string
	}{
		{name: "missing action", args: nil},
		{name: "unknown action", args: []string{"fetch"}},
		{name: "list positional", args: []string{"list", "extra"}},
		{name: "show missing id", args: []string{"show"}},
		{name: "show extra id", args: []string{"show", digest, "other"}},
		{name: "show uppercase id", args: []string{"show", strings.ToUpper(digest)}},
		{name: "show path", args: []string{"show", "../private"}},
		{name: "show list status", args: []string{"show", "--status", "invalid", digest}},
		{name: "show list limit", args: []string{"show", "--limit", "50", digest}},
		{name: "status ready", args: []string{"list", "--status", "ready"}},
		{name: "status empty", args: []string{"list", "--status="}},
		{name: "version empty", args: []string{"list", "--version="}},
		{name: "version padded", args: []string{"list", "--version", " 2026.9.8"}},
		{name: "version path", args: []string{"list", "--version", "release/2026.9.8"}},
		{name: "version invisible", args: []string{"list", "--version", "2026.9.8\u200b"}},
		{name: "zero limit", args: []string{"list", "--limit", "0"}},
		{name: "oversized limit", args: []string{"list", "--limit", "101"}},
		{name: "empty cursor", args: []string{"list", "--cursor="}},
		{name: "oversized cursor", args: []string{"list", "--cursor", strings.Repeat("a", 2049)}},
		{name: "mixed transports", args: []string{"list", "--hub-url", "http://100.64.0.9:8787", "--db", "/tmp/not-used"}},
		{name: "empty hub", args: []string{"list", "--hub-url="}},
		{name: "control hub", args: []string{"list", "--hub-url", "http://100.64.0.9\n:8787"}},
		{name: "empty db", args: []string{"list", "--db="}},
		{name: "duplicate status", args: []string{"list", "--status", "invalid", "--status", "unavailable"}},
		{name: "duplicate json", args: []string{"list", "--json", "--json"}},
		{name: "unknown flag", args: []string{"list", "--raw"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			if err := runArtifactReadCommandWithDeps(t.Context(), test.args, &out, &errOut, deps); err == nil {
				t.Fatalf("invalid args %q were accepted", test.args)
			}
			if out.Len() != 0 {
				t.Fatalf("invalid args %q wrote stdout=%q", test.args, out.String())
			}
		})
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("invalid artifact arguments reached I/O %d times", got)
	}

	var out, help bytes.Buffer
	if err := runArtifactReadCommandWithDeps(t.Context(), []string{"list", "--help"}, &out, &help, deps); !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("help err=%v", err)
	}
	for _, want := range []string{"artifact list", "artifact show", "available_unverified", "list 驗證 metadata", "show 驗證 SHA-256", "--hub-url", "--db"} {
		if !strings.Contains(help.String(), want) {
			t.Fatalf("artifact help missing %q: %s", want, help.String())
		}
	}
	if calls.Load() != 0 || out.Len() != 0 {
		t.Fatalf("help reached I/O=%d stdout=%q", calls.Load(), out.String())
	}
}

func TestArtifactReadCLIHumanOutputIsTerminalSafeAndTruthful(t *testing.T) {
	version := "2026.9.8\r\n\x1b]2;owned\a"
	size := int64(42)
	fetchedAt := time.Date(2026, 9, 8, 12, 0, 0, 123, time.UTC)
	issue := operator.ArtifactReadIssue("invalid\x1b[31m")
	list := operator.ArtifactListResult{
		SchemaVersion: operator.ArtifactReadSchemaVersion, Consistency: "live\x1b[31m",
		EvaluatedAt: fetchedAt, Total: 1,
		Items: []operator.ArtifactSummary{{
			ArtifactID: "artifact\x1b]2;owned\a", Version: &version, SizeBytes: &size, FetchedAt: &fetchedAt,
			Status: operator.ArtifactInvalid, Issue: &issue,
		}},
	}
	var listOut bytes.Buffer
	if err := writeArtifactList(&listOut, list, false, "HTTP operator API"); err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(listOut.String(), "\x1b\r\a") || !strings.Contains(listOut.String(), `\x1b`) ||
		!strings.Contains(listOut.String(), `\r\n`) || !strings.Contains(listOut.String(), "verification: metadata") {
		t.Fatalf("unsafe or untruthful artifact list output=%q", listOut.String())
	}

	name, digest, sha := "openclaw", "sha256:"+strings.Repeat("a", 64), strings.Repeat("a", 64)
	verifiedAt := fetchedAt
	detail := operator.ArtifactDetailResult{
		SchemaVersion: operator.ArtifactReadSchemaVersion, Consistency: operator.ArtifactReadConsistencyLive,
		EvaluatedAt: fetchedAt,
		Item: operator.ArtifactSummary{
			ArtifactID: sha, Name: &name, Version: &version, SHA256: &sha, Digest: &digest,
			SizeBytes: &size, FetchedAt: &fetchedAt, VerifiedAt: &verifiedAt, Status: operator.ArtifactReady,
		},
	}
	var detailOut bytes.Buffer
	if err := writeArtifactDetail(&detailOut, detail, false, "HTTP operator API"); err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(detailOut.String(), "\x1b\r\a") ||
		!strings.Contains(detailOut.String(), "SHA-256 verification: passed") {
		t.Fatalf("unsafe or untruthful artifact detail output=%q", detailOut.String())
	}
}

func TestArtifactReadCLIMaintenanceRoutingKeepsHTTPReadsRemoteAndDirectReadsFenced(t *testing.T) {
	dbPath, _ := directDBFixture(t)
	t.Setenv("CLAWCTL_DB", dbPath)
	if err := os.WriteFile(upgradeMaintenanceMarker(dbPath), []byte(upgradeMaintenanceContents), 0o600); err != nil {
		t.Fatal(err)
	}
	digest := strings.Repeat("a", 64)
	for _, argv := range [][]string{
		{"artifact", "list", "--hub-url", "http://100.64.0.9:8787"},
		{"artifact", "show", "--hub-url", "http://100.64.0.9:8787", digest},
		{"artifact", "list", "--db", dbPath},
	} {
		if err := rejectTopLevelCLIWhileUpgradeMaintenance("artifact", argv); err != nil {
			t.Fatalf("artifact read transport was coupled to default marker argv=%q: %v", argv, err)
		}
	}
	if err := rejectTopLevelCLIWhileUpgradeMaintenance("artifact", []string{
		"artifact", "fetch", "--db", dbPath, "openclaw@2026.9.8",
	}); err == nil {
		t.Fatal("artifact fetch bypassed upgrade maintenance")
	}

	deps := productionMachineCommandDeps()
	deps.verifyHubStopped = func(context.Context, string) error {
		t.Fatal("exact-target maintenance refusal reached stopped proof")
		return nil
	}
	var out, errOut bytes.Buffer
	err := runArtifactReadCommandWithDeps(t.Context(), []string{"list", "--db", dbPath}, &out, &errOut, deps)
	if err == nil || !strings.Contains(err.Error(), "direct DB") || out.Len() != 0 {
		t.Fatalf("direct artifact read bypassed exact marker: err=%v stdout=%q stderr=%q", err, out.String(), errOut.String())
	}
}

func TestValidArtifactReadCLIID(t *testing.T) {
	digest := strings.Repeat("a", 64)
	for _, value := range []string{digest, "invalid-" + digest} {
		if !validArtifactReadCLIID(value) {
			t.Fatalf("valid artifact identity %q rejected", value)
		}
	}
	for _, value := range []string{"", strings.ToUpper(digest), "invalid-" + strings.ToUpper(digest), "../" + digest} {
		if validArtifactReadCLIID(value) {
			t.Fatalf("invalid artifact identity %q accepted", value)
		}
	}
}

func assertSafeArtifactCLIOutput(t *testing.T, raw []byte, forbidden ...string) {
	t.Helper()
	for _, value := range append(forbidden, "tarball_url", "fetched_by", `"path"`) {
		if value != "" && bytes.Contains(raw, []byte(value)) {
			t.Fatalf("artifact CLI disclosed %q: %s", value, raw)
		}
	}
}
