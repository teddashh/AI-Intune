package web

import (
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/deploy"
	"github.com/teddashh/AI-Intune/internal/model"
	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/store"
)

func createJobForJobsPage(t *testing.T, st *store.Store, machineID string) string {
	t.Helper()
	desiredID, revision, err := st.CreateDesiredState(
		"machine", machineID, "diagnostic", "noop", `{"kind":"noop"}`, "web jobs test",
	)
	if err != nil {
		t.Fatalf("create desired state: %v", err)
	}
	jobID, err := st.CreateJob(machineID, desiredID, revision, store.NewJob{
		ArtifactDigest: "sha256:" + strings.Repeat("a", 64), ExecutionTimeout: 90,
	})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	return jobID
}

func TestGlobalJobsPageShowsCanonicalQueueWithoutLeaseToken(t *testing.T) {
	s, st := newServer(t)
	machineID := enroll(t, st, "jobs-page-machine", time.Now().UTC())
	jobID := createJobForJobsPage(t, st, machineID)
	lease, err := st.ClaimJob(jobID, machineID, time.Now().UTC(), time.Hour)
	if err != nil {
		t.Fatalf("claim job: %v", err)
	}

	tables := []string{"jobs", "job_events", "verification_results", "audit_log"}
	before := tableCounts(t, st, tables...)
	body := get(t, s, "/jobs")
	after := tableCounts(t, st, tables...)

	for _, want := range []string{
		"<h1>代理程式活動</h1>", jobID[:8], "jobs-page-machine", "diagnostic/noop",
		"claimed", "租約剩", "not_started", "manual_intervention",
		`href="/jobs" class="nav-item on" aria-current="page"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("global Jobs page missing %q", want)
		}
	}
	if strings.Contains(body, lease) {
		t.Fatal("global Jobs page leaked the fencing lease token")
	}
	for _, table := range tables {
		if before[table] != after[table] {
			t.Errorf("GET /jobs mutated %s: before=%d after=%d", table, before[table], after[table])
		}
	}
}

func TestJobDetailKeepsTheJobsSidebarEntryDiscoverable(t *testing.T) {
	s, st := newServer(t)
	machineID := enroll(t, st, "job-detail-sidebar", time.Now().UTC())
	jobID := createJobForJobsPage(t, st, machineID)
	body := get(t, s, "/jobs/"+jobID)
	if !strings.Contains(body, `href="/jobs" class="nav-item on"`) {
		t.Fatalf("job detail did not keep the Jobs sidebar entry selected: %s", body)
	}
	if strings.Contains(body, `href="/machines#machines" class="nav-item on"`) {
		t.Fatal("job detail selected Machines instead of its discoverable Jobs sidebar entry")
	}
	if !strings.Contains(body, "還沒有 verifier 被指派來看這張工作單。") {
		t.Error("job detail without assignments did not explain the empty assignment table")
	}
}

func TestGlobalJobsPageDistinguishesTrueZeroAndEmptyFilter(t *testing.T) {
	s, st := newServer(t)
	if body := get(t, s, "/jobs"); !strings.Contains(body, "還沒有任何工作單。") {
		t.Fatalf("empty job ledger did not state true zero: %s", body)
	}

	first := enroll(t, st, "job-filter-first", time.Now().UTC())
	second := enroll(t, st, "job-filter-second", time.Now().UTC())
	firstJob := createJobForJobsPage(t, st, first)
	secondJob := createJobForJobsPage(t, st, second)
	body := get(t, s, "/jobs?machine_id="+first)
	if !strings.Contains(body, firstJob[:8]) || strings.Contains(body, secondJob[:8]) ||
		!strings.Contains(body, "清除篩選") {
		t.Fatalf("machine filter did not preserve exact scope: %s", body)
	}

	body = get(t, s, "/jobs?machine_id="+first+"&state=succeeded")
	if !strings.Contains(body, "目前篩選沒有符合的工作單。") {
		t.Fatalf("empty filtered result was presented as an empty ledger: %s", body)
	}
}

func TestGlobalJobsPageCursorMovesForwardWithoutRepeatingRows(t *testing.T) {
	s, st := newServer(t)
	machineID := enroll(t, st, "job-page-cursor", time.Now().UTC())
	firstID := createJobForJobsPage(t, st, machineID)
	secondID := createJobForJobsPage(t, st, machineID)

	firstPage := get(t, s, "/jobs?machine_id="+url.QueryEscape(machineID)+
		"&state=not_started&resource_kind=diagnostic&resource_id=noop&limit=1")
	nextPattern := regexp.MustCompile(`rel="next" href="([^"]+)"`)
	match := nextPattern.FindStringSubmatch(firstPage)
	if len(match) != 2 {
		t.Fatalf("first page has no next cursor link: %s", firstPage)
	}
	nextURL := html.UnescapeString(match[1])
	parsedNext, err := url.Parse(nextURL)
	if err != nil {
		t.Fatal(err)
	}
	query := parsedNext.Query()
	if query.Get("machine_id") != machineID || query.Get("state") != "not_started" ||
		query.Get("resource_kind") != "diagnostic" || query.Get("resource_id") != "noop" ||
		query.Get("limit") != "1" || query.Get("cursor") == "" {
		t.Fatalf("next link lost filters: %s", nextURL)
	}
	secondPage := get(t, s, nextURL)

	firstOnFirst := strings.Contains(firstPage, firstID[:8])
	secondOnFirst := strings.Contains(firstPage, secondID[:8])
	if firstOnFirst == secondOnFirst {
		t.Fatalf("limit=1 rendered the wrong number of job rows: first=%t second=%t", firstOnFirst, secondOnFirst)
	}
	if strings.Contains(secondPage, firstID[:8]) == firstOnFirst ||
		strings.Contains(secondPage, secondID[:8]) == secondOnFirst {
		t.Fatalf("cursor page repeated or skipped rows: first=%s second=%s", firstPage, secondPage)
	}
}

func TestWebJobListURLPreservesEveryFilter(t *testing.T) {
	got := webJobListURL(operator.JobListRequest{
		MachineID: "machine-a", States: []deploy.JobState{deploy.Claimed, deploy.Running},
		DeploymentID: "deployment-a", ResourceKind: "openclaw", ResourceID: "openclaw",
		Limit: 7,
	}, "opaque-cursor")
	parsed, err := url.Parse(got)
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	if query.Get("machine_id") != "machine-a" || query.Get("deployment_id") != "deployment-a" ||
		query.Get("resource_kind") != "openclaw" || query.Get("resource_id") != "openclaw" ||
		query.Get("limit") != "7" || query.Get("cursor") != "opaque-cursor" ||
		strings.Join(query["state"], ",") != "claimed,running" {
		t.Fatalf("next URL lost a filter: %s", got)
	}
}

func TestGlobalJobsPageRejectsInvalidQueriesInsteadOfIgnoringThem(t *testing.T) {
	s, _ := newServer(t)
	for _, path := range []string{
		"/jobs?",
		"/jobs?stat=succeeded",
		"/jobs?machine_id=a&machine_id=b",
		"/jobs?machine_id=",
		"/jobs?state=claimed&state=claimed",
		"/jobs?state=flying",
		"/jobs?resource_id=openclaw",
		"/jobs?limit=0",
		"/jobs?limit=1&limit=2",
		"/jobs?limit=101",
		"/jobs?cursor=not-a-cursor",
		"/jobs?machine_id=machine%0Aid",
		"/jobs?machine_id=machine%E2%80%AEid",
		"/jobs?cursor=" + strings.Repeat("a", 2049),
	} {
		t.Run(path, func(t *testing.T) {
			mux := http.NewServeMux()
			s.Routes(mux)
			rec := httptest.NewRecorder()
			req := verifiedWebRequest(httptest.NewRequest(http.MethodGet, path, nil),
				"example.com/cap/clawctl-view")
			mux.ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("invalid Jobs query status=%d, want 400: %s", rec.Code, rec.Body.String())
			}
		})
	}
}

// TestJobPageStatesTheCrossDomainVerdictAsATypedState covers the section an
// operator reads before believing a deployment: whether a producer in another
// failure domain saw the same artifact, and whether it can still speak.
func TestJobPageStatesTheCrossDomainVerdictAsATypedState(t *testing.T) {
	s, st := newServer(t)
	now := time.Now().UTC()
	machineID := enroll(t, st, "independent-subject", now)
	peerID := enroll(t, st, "independent-peer", now)
	jobID, token, jobNow := createWebJob(t, st, machineID, false)
	startWebJob(t, st, machineID, jobID, token, jobNow)
	const jobDigest = "sha256:5555555555555555555555555555555555555555555555555555555555555555"
	if _, err := st.DB().Exec(`UPDATE jobs SET artifact_digest=? WHERE job_id=?`, jobDigest, jobID); err != nil {
		t.Fatal(err)
	}

	body := get(t, s, "/jobs/"+jobID)
	for _, want := range []string{
		"跨故障域證據", "absent", "沒有第二個 producer 為這張工作單寫過證據",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("job page without independent evidence missing %q", want)
		}
	}

	verifier, _, err := st.RegisterVerifier(
		store.VerifierKindFleetPeerAgent, "peer-verifier", peerID, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.RecordIndependentVerification(store.IndependentVerificationRequest{
		VerifierID: verifier.VerifierID, JobID: jobID, RuleID: "health",
		Command: "GET /health", StdoutExcerpt: "ok", ObservedDigest: jobDigest,
		Passed: true, VerifiedAt: jobNow,
	}); err != nil {
		t.Fatal(err)
	}
	body = get(t, s, "/jobs/"+jobID)
	for _, want := range []string{
		"passed", "第二個 producer 回報的規則全部通過",
		"verified_at（verifier）", "received_at（Hub）",
		"failure domain " + peerID, "state active", "與工作單相同",
		"peer-verifier",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("job page with independent evidence missing %q", want)
		}
	}

	if err := st.RevokeVerifier(verifier.VerifierID, verifier.Revision, now); err != nil {
		t.Fatal(err)
	}
	body = get(t, s, "/jobs/"+jobID)
	for _, want := range []string{
		"producer_revoked", "寫過這些證據的 producer 都已撤銷", "state revoked",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("job page after revocation missing %q", want)
		}
	}
	if strings.Contains(body, "可用 producer 1") {
		t.Error("job page still counts a revoked producer as available")
	}
}

// TestJobPageShowsADigestClashAsItsOwnVerdict keeps a passing row from reading
// as corroboration when the artifact the producer saw is a different one.
func TestJobPageShowsADigestClashAsItsOwnVerdict(t *testing.T) {
	s, st := newServer(t)
	now := time.Now().UTC()
	machineID := enroll(t, st, "clash-subject", now)
	peerID := enroll(t, st, "clash-peer", now)
	jobID, token, jobNow := createWebJob(t, st, machineID, false)
	startWebJob(t, st, machineID, jobID, token, jobNow)
	const jobDigest = "sha256:6666666666666666666666666666666666666666666666666666666666666666"
	const otherDigest = "sha256:7777777777777777777777777777777777777777777777777777777777777777"
	if _, err := st.DB().Exec(`UPDATE jobs SET artifact_digest=? WHERE job_id=?`, jobDigest, jobID); err != nil {
		t.Fatal(err)
	}
	verifier, _, err := st.RegisterVerifier(
		store.VerifierKindFleetPeerAgent, "clash-verifier", peerID, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.RecordIndependentVerification(store.IndependentVerificationRequest{
		VerifierID: verifier.VerifierID, JobID: jobID, RuleID: "health",
		Command: "GET /health", ObservedDigest: otherDigest, Passed: true, VerifiedAt: jobNow,
	}); err != nil {
		t.Fatal(err)
	}
	body := get(t, s, "/jobs/"+jobID)
	for _, want := range []string{
		"digest_mismatch", "看到的 artifact digest 與這張工作單的不同", "與工作單不同", otherDigest,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("digest clash page missing %q", want)
		}
	}
}

func TestJobPageShowsIndependentCurrentReleaseMismatch(t *testing.T) {
	s, st := newServer(t)
	now := time.Now().UTC()
	machineID := enroll(t, st, "release-subject", now)
	peerID := enroll(t, st, "release-peer", now)
	jobID, token, jobNow := createWebJob(t, st, machineID, false)
	startWebJob(t, st, machineID, jobID, token, jobNow)
	verifier, _, err := st.RegisterVerifier(
		store.VerifierKindFleetPeerAgent, "release-verifier", peerID, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.RecordIndependentVerification(store.IndependentVerificationRequest{
		VerifierID: verifier.VerifierID, JobID: jobID,
		RuleID:  model.IndependentRuleOpenClawCurrentRelease,
		Command: "readlink current", ObservedVersion: "2026.6.9",
		Passed: true, VerifiedAt: jobNow,
	}); err != nil {
		t.Fatal(err)
	}
	body := get(t, s, "/jobs/"+jobID)
	for _, want := range []string{
		"release_mismatch", "看到的 OpenClaw 版本與這張工作單的不同",
		"2026.6.9", "預期 2026.6.10",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("release mismatch page missing %q", want)
		}
	}
}

func TestTheAssignmentStateColumnSaysTheseExactWords(t *testing.T) {
	tests := []struct {
		name      string
		state     string
		wantLabel string
		wantClass string
	}{
		{
			name: "已送出獨立證據", state: operator.JobAssignmentReported,
			wantLabel: "已送出獨立證據", wantClass: "st green",
		},
		{
			name: "verifier 撤銷了不會再有人回報", state: operator.JobAssignmentProducerRevoked,
			wantLabel: "指派的 verifier 已撤銷，不會再回報", wantClass: "st amber",
		},
		{
			name: "工作單還沒結束所以還沒發出", state: operator.JobAssignmentWaitingForJob,
			wantLabel: "等這張工作單結束才會發出", wantClass: "st grey",
		},
		{
			name: "已經發出在等回報", state: operator.JobAssignmentAwaitingReport,
			wantLabel: "已發出，等它回報", wantClass: "st grey",
		},
		{
			name: "不認得的 state 原樣印出來", state: "not-a-state",
			wantLabel: "not-a-state", wantClass: "st grey",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := assignmentStateLabel(test.state); got != test.wantLabel {
				t.Errorf("assignmentStateLabel(%q)=%q, want %q", test.state, got, test.wantLabel)
			}
			// 「還沒發出」與「已經發出在等回報」現在同色是現況，不是結論。
			if got := assignmentStateClass(test.state); got != test.wantClass {
				t.Errorf("assignmentStateClass(%q)=%q, want %q", test.state, got, test.wantClass)
			}
		})
	}
}
