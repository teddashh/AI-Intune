package operator

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/deploy"
	"github.com/teddashh/AI-Intune/internal/store"
)

func TestJobReadProjectionIsSafeBoundedAndExplicit(t *testing.T) {
	st := newOperatorJobReadStore(t)
	now := time.Date(2026, 9, 7, 14, 0, 0, 0, time.UTC)
	if err := st.UpsertMachine(store.Machine{
		MachineID: "machine-safe", DisplayName: "safe-machine", Expected: true, CreatedAt: now.Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	desiredID, revision, err := st.CreateDesiredState(
		"machine", "machine-safe", "diagnostic", "safe-read",
		`{"private_path":"/private/spec/path","secret":"RAW_SPEC_SENTINEL"}`,
		"RAW_CREATED_BY_SENTINEL")
	if err != nil {
		t.Fatal(err)
	}
	jobID, err := st.CreateJob("machine-safe", desiredID, revision, store.NewJob{
		ArtifactDigest: "sha256:" + strings.Repeat("a", 64),
		Irreversible:   true, ExecutionTimeout: 321,
	})
	if err != nil {
		t.Fatal(err)
	}
	leaseToken, err := st.ClaimJob(jobID, "machine-safe", now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= JobDetailEvidenceLimit+1; i++ {
		at := now.Add(time.Duration(i) * time.Second)
		if _, err := st.DB().Exec(`INSERT INTO job_events
 (event_id,job_id,seq,phase,occurred_at,received_at,payload) VALUES (?,?,?,?,?,?,?)`,
			fmt.Sprintf("event-%03d", i), jobID, i, "RAW_PRIVATE_PHASE",
			at.Format(time.RFC3339Nano), at.Format(time.RFC3339Nano),
			fmt.Sprintf(`{"payload":"RAW_EVENT_PAYLOAD_%03d","path":"/private/event/%03d"}`, i, i)); err != nil {
			t.Fatal(err)
		}
		if _, err := st.DB().Exec(`INSERT INTO verification_results
 (verification_id,job_id,machine_id,rule_id,command,exit_code,stdout_excerpt,stderr_excerpt,passed,verified_at)
 VALUES (?,?,?,?,?,?,?,?,?,?)`, fmt.Sprintf("verification-%03d", i), jobID, "machine-safe",
			fmt.Sprintf("RAW_RULE_%03d", i), fmt.Sprintf("RAW_COMMAND_%03d", i), i,
			fmt.Sprintf("RAW_STDOUT_%03d", i), fmt.Sprintf("/private/stderr/%03d", i), i%2 == 0,
			at.Format(time.RFC3339Nano)); err != nil {
			t.Fatal(err)
		}
	}

	evaluatedAt := now.Add(time.Minute)
	list, err := New(st).ListJobs(JobListRequest{Limit: 10}, evaluatedAt)
	if err != nil {
		t.Fatal(err)
	}
	if list.SchemaVersion != JobReadSchemaVersion || !list.EvaluatedAt.Equal(evaluatedAt) ||
		list.Total != 1 || len(list.Items) != 1 || list.NextCursor != nil ||
		len(list.StateCounts) != len(deploy.AllJobStates) {
		t.Fatalf("list=%+v", list)
	}
	item := list.Items[0]
	if item.JobID != jobID || item.LeaseStatus != JobLeaseActive || item.LeaseExpiresAt == nil ||
		item.ArtifactDigest == nil || item.ArtifactDigestStatus != JobArtifactDigestRecorded ||
		item.EventCount != JobDetailEvidenceLimit+1 ||
		item.VerificationTotal != JobDetailEvidenceLimit+1 {
		t.Fatalf("summary=%+v", item)
	}
	assertOperatorJobStateCounts(t, list.StateCounts, list.Total)
	if page, ok := list.StorePage(); !ok || len(page.Items) != 1 || page.Items[0].JobID != jobID {
		t.Fatalf("trusted list bridge page=%+v ok=%v", page, ok)
	}

	detail, err := New(st).JobDetail(jobID, evaluatedAt)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Item.JobID != jobID || detail.Events.Total != JobDetailEvidenceLimit+1 ||
		len(detail.Events.Items) != JobDetailEvidenceLimit || !detail.Events.Truncated ||
		detail.Verifications.Total != JobDetailEvidenceLimit+1 ||
		len(detail.Verifications.Items) != JobDetailEvidenceLimit || !detail.Verifications.Truncated {
		t.Fatalf("detail envelope=%+v", detail)
	}
	if detail.Events.Items[0].Seq != 2 ||
		detail.Events.Items[len(detail.Events.Items)-1].Seq != JobDetailEvidenceLimit+1 {
		t.Fatalf("bounded event order first=%+v last=%+v", detail.Events.Items[0], detail.Events.Items[len(detail.Events.Items)-1])
	}
	for _, event := range detail.Events.Items {
		if event.Phase != JobEventOther {
			t.Errorf("unapproved machine phase was exposed as %q", event.Phase)
		}
	}
	for name, value := range map[string]any{"list": list, "detail": detail} {
		t.Run(name+"-json-allowlist", func(t *testing.T) {
			assertOperatorJobReadJSONSafe(t, value, leaseToken)
		})
	}
}

func TestJobListCursorIsCanonicalFilterBoundAndPinsCreationCeiling(t *testing.T) {
	st := newOperatorJobReadStore(t)
	now := time.Date(2026, 9, 7, 15, 0, 0, 0, time.UTC)
	if err := st.UpsertMachine(store.Machine{
		MachineID: "machine-page", DisplayName: "page-machine", Expected: true, CreatedAt: now.Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	original := make(map[string]bool)
	for i := 0; i < 3; i++ {
		desiredID, revision, err := st.CreateDesiredState(
			"machine", "machine-page", "diagnostic", "cursor", `{"kind":"noop"}`, fmt.Sprintf("creator-%d", i))
		if err != nil {
			t.Fatal(err)
		}
		jobID, err := st.CreateJob("machine-page", desiredID, revision, store.NewJob{})
		if err != nil {
			t.Fatal(err)
		}
		original[jobID] = true
	}
	service := New(st)
	request := JobListRequest{MachineID: "machine-page", Limit: 1}
	first, err := service.ListJobs(request, now)
	if err != nil {
		t.Fatal(err)
	}
	if first.Total != 3 || len(first.Items) != 1 || first.NextCursor == nil {
		t.Fatalf("first page=%+v", first)
	}

	desiredID, revision, err := st.CreateDesiredState(
		"machine", "machine-page", "diagnostic", "cursor", `{"kind":"noop"}`, "later-writer")
	if err != nil {
		t.Fatal(err)
	}
	lateJob, err := st.CreateJob("machine-page", desiredID, revision, store.NewJob{})
	if err != nil {
		t.Fatal(err)
	}

	// Limit is page-local and intentionally excluded from the filter digest.
	request.Limit = 2
	request.Cursor = *first.NextCursor
	second, err := service.ListJobs(request, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if second.Total != 3 || len(second.Items) != 2 || second.NextCursor != nil {
		t.Fatalf("second page=%+v", second)
	}
	seen := map[string]bool{first.Items[0].JobID: true}
	for _, item := range second.Items {
		if seen[item.JobID] {
			t.Fatalf("job %q repeated across cursor pages", item.JobID)
		}
		seen[item.JobID] = true
	}
	if len(seen) != len(original) || seen[lateJob] {
		t.Fatalf("cursor traversal=%v original=%v late=%q", seen, original, lateJob)
	}

	for name, cursor := range map[string]string{
		"padding":   *first.NextCursor + "=",
		"garbage":   "not_base64!",
		"trailing":  jobReadCursorWithTrailingDocument(t, *first.NextCursor),
		"unknown":   jobReadCursorWithUnknownField(t, *first.NextCursor),
		"duplicate": jobReadCursorWithDuplicateVersion(t, *first.NextCursor),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := service.ListJobs(JobListRequest{
				MachineID: "machine-page", Limit: 1, Cursor: cursor,
			}, now)
			if !errors.Is(err, ErrInvalidJobRead) {
				t.Fatalf("error=%v want ErrInvalidJobRead", err)
			}
		})
	}
	if _, err := service.ListJobs(JobListRequest{
		MachineID: "different-filter", Limit: 1, Cursor: *first.NextCursor,
	}, now); !errors.Is(err, ErrInvalidJobRead) {
		t.Fatalf("filter-bound cursor error=%v want ErrInvalidJobRead", err)
	}
}

func TestJobReadValidationAndDerivedStatuses(t *testing.T) {
	st := newOperatorJobReadStore(t)
	service := New(st)
	now := time.Date(2026, 9, 7, 16, 0, 0, 0, time.UTC)
	for name, request := range map[string]JobListRequest{
		"negative-limit":     {Limit: -1},
		"large-limit":        {Limit: MaxJobReadLimit + 1},
		"resource-id-alone":  {ResourceID: "resource"},
		"unknown-state":      {States: []deploy.JobState{"future"}},
		"duplicate-state":    {States: []deploy.JobState{deploy.Failed, deploy.Failed}},
		"whitespace-machine": {MachineID: " machine"},
		"bad-cursor":         {Cursor: "invalid"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := service.ListJobs(request, now); !errors.Is(err, ErrInvalidJobRead) {
				t.Fatalf("error=%v want ErrInvalidJobRead", err)
			}
		})
	}
	if _, err := service.ListJobs(JobListRequest{}, time.Time{}); !errors.Is(err, ErrInvalidJobRead) {
		t.Fatalf("zero evaluation error=%v want ErrInvalidJobRead", err)
	}
	empty, err := service.ListJobs(JobListRequest{}, now)
	if err != nil || empty.Total != 0 || empty.Items == nil || empty.StateCounts == nil {
		t.Fatalf("default empty list=%+v error=%v", empty, err)
	}
	for _, jobID := range []string{"", " job"} {
		if _, err := service.JobDetail(jobID, now); !errors.Is(err, ErrInvalidJobRead) {
			t.Fatalf("detail %q error=%v want ErrInvalidJobRead", jobID, err)
		}
	}
	if _, err := service.JobDetail("job", time.Time{}); !errors.Is(err, ErrInvalidJobRead) {
		t.Fatalf("zero detail evaluation error=%v want ErrInvalidJobRead", err)
	}
	if _, err := service.JobDetail("missing", now); !errors.Is(err, store.ErrJobNotFound) {
		t.Fatalf("missing detail error=%v want ErrJobNotFound", err)
	}

	activeExpiry := now.Add(time.Second)
	expiredAt := now
	for _, test := range rangeJobStatusTests(now, activeExpiry, expiredAt) {
		t.Run(test.name, func(t *testing.T) {
			summary, err := projectJobSummary(test.item, now)
			if err != nil {
				t.Fatal(err)
			}
			if summary.LeaseStatus != test.wantLease || summary.ArtifactDigestStatus != test.wantDigestStatus {
				t.Fatalf("summary=%+v want lease=%q digest=%q", summary, test.wantLease, test.wantDigestStatus)
			}
		})
	}
	invalidDigest := "sha256:ABC"
	if _, err := projectJobSummary(store.JobReadItem{ArtifactDigest: &invalidDigest}, now); !errors.Is(err, store.ErrJobReadCorrupt) {
		t.Fatalf("invalid digest error=%v want ErrJobReadCorrupt", err)
	}
}

type jobStatusTest struct {
	name             string
	item             store.JobReadItem
	wantLease        JobLeaseStatus
	wantDigestStatus JobArtifactDigestStatus
}

func rangeJobStatusTests(now, activeExpiry, expiredAt time.Time) []jobStatusTest {
	digest := "sha256:" + strings.Repeat("c", 64)
	return []jobStatusTest{
		{name: "unclaimed", item: store.JobReadItem{State: deploy.NotStarted}, wantLease: JobLeaseUnclaimed, wantDigestStatus: JobArtifactDigestNotRecorded},
		{name: "active", item: store.JobReadItem{State: deploy.Claimed, LeaseExpiresAt: &activeExpiry, ArtifactDigest: &digest}, wantLease: JobLeaseActive, wantDigestStatus: JobArtifactDigestRecorded},
		{name: "expiry-boundary", item: store.JobReadItem{State: deploy.Running, LeaseExpiresAt: &expiredAt}, wantLease: JobLeaseExpired, wantDigestStatus: JobArtifactDigestNotRecorded},
		{name: "unknown", item: store.JobReadItem{State: deploy.Verifying}, wantLease: JobLeaseUnknown, wantDigestStatus: JobArtifactDigestNotRecorded},
		{name: "terminal", item: store.JobReadItem{State: deploy.Succeeded}, wantLease: JobLeaseNone, wantDigestStatus: JobArtifactDigestNotRecorded},
	}
}

func newOperatorJobReadStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func assertOperatorJobStateCounts(t *testing.T, counts []JobStateCount, total int) {
	t.Helper()
	if len(counts) != len(deploy.AllJobStates) {
		t.Fatalf("state count length=%d want=%d", len(counts), len(deploy.AllJobStates))
	}
	sum := 0
	for i, state := range deploy.AllJobStates {
		if counts[i].State != state || counts[i].Count < 0 {
			t.Fatalf("state_counts[%d]=%+v want=%q", i, counts[i], state)
		}
		sum += counts[i].Count
	}
	if sum != total {
		t.Fatalf("state count sum=%d want=%d", sum, total)
	}
}

func assertOperatorJobReadJSONSafe(t *testing.T, value any, leaseToken string) {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	for _, sentinel := range []string{
		leaseToken, "RAW_SPEC_SENTINEL", "RAW_CREATED_BY_SENTINEL", "RAW_EVENT_PAYLOAD",
		"RAW_PRIVATE_PHASE", "RAW_RULE", "RAW_COMMAND", "RAW_STDOUT", "/private/",
	} {
		if bytes.Contains(raw, []byte(sentinel)) {
			t.Errorf("operator job DTO exposed forbidden value %q: %s", sentinel, raw)
		}
	}
	var document any
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	forbidden := map[string]bool{
		"lease_token": true, "spec": true, "created_by": true,
		"payload": true, "rule_id": true, "command": true,
		"stdout_excerpt": true, "stderr_excerpt": true,
		"path": true, "error": true, "detail": true,
	}
	var walk func(any)
	walk = func(node any) {
		switch typed := node.(type) {
		case map[string]any:
			for key, child := range typed {
				if forbidden[key] {
					t.Errorf("operator job DTO exposed forbidden key %q: %s", key, raw)
				}
				walk(child)
			}
		case []any:
			for _, child := range typed {
				walk(child)
			}
		}
	}
	walk(document)
}

func jobReadCursorWithTrailingDocument(t *testing.T, encoded string) string {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	raw = append(raw, []byte(`{}`)...)
	return base64.RawURLEncoding.EncodeToString(raw)
}

func jobReadCursorWithUnknownField(t *testing.T, encoded string) string {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	document["unknown"] = true
	raw, err = json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

func jobReadCursorWithDuplicateVersion(t *testing.T, encoded string) string {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) < 1 || raw[0] != '{' {
		t.Fatalf("unexpected cursor JSON %q", raw)
	}
	raw = append([]byte(`{"v":1,`), raw[1:]...)
	return base64.RawURLEncoding.EncodeToString(raw)
}
