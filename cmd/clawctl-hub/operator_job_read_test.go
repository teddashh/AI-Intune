package main

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/deploy"
	"github.com/teddashh/AI-Intune/internal/store"
)

func createOperatorReadJob(t *testing.T, st *store.Store, machineID, displayName, marker string) string {
	t.Helper()
	if _, err := st.GetMachine(machineID); err != nil {
		if err := st.UpsertMachine(store.Machine{
			MachineID: machineID, DisplayName: displayName, Expected: true,
			CreatedAt: time.Now().UTC().Add(-time.Hour),
		}); err != nil {
			t.Fatal(err)
		}
	}
	desiredID, revision, err := st.CreateDesiredState(
		"machine", machineID, "diagnostic", "noop", `{"kind":"noop","private":"`+marker+`"}`, marker)
	if err != nil {
		t.Fatal(err)
	}
	jobID, err := st.CreateJob(machineID, desiredID, revision, store.NewJob{
		ArtifactDigest: "sha256:" + strings.Repeat("a", 64), ExecutionTimeout: 60,
	})
	if err != nil {
		t.Fatal(err)
	}
	return jobID
}

func TestOperatorJobReadsUseSafeProjectionAndDoNotMutate(t *testing.T) {
	st := boundaryStore(t)
	jobID := createOperatorReadJob(t, st, "machine-safe", "safe-machine", "RAW_SPEC_CREATED_BY_SENTINEL")
	now := time.Now().UTC().Truncate(time.Second)
	lease, err := st.ClaimJob(jobID, "machine-safe", now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AppendJobEvent(jobID, "machine-safe", lease, 1, "start",
		`{"rejection_code":"PRECONDITION_FAILED","detail":"RAW_REJECTION_DETAIL_SENTINEL","secret":"RAW_EVENT_PAYLOAD_SENTINEL"}`, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AdvanceJobByAgent(jobID, "machine-safe", lease, deploy.Start, now); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordVerification(jobID, "machine-safe", lease, "RAW_RULE_SENTINEL",
		"RAW_COMMAND_SENTINEL", 17, "RAW_STDOUT_SENTINEL", "RAW_STDERR_SENTINEL", false, now); err != nil {
		t.Fatal(err)
	}

	before := operatorJobReadCounts(t, st)
	mux := http.NewServeMux()
	(&hub{store: st}).operatorRoutes(mux)
	for _, path := range []string{"/v1/operator/jobs", "/v1/operator/jobs/" + jobID} {
		rec := operatorRequest(t, mux, http.MethodGet, path, "", "")
		if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "no-store" ||
			rec.Header().Get("ETag") != "" || rec.Header().Get("Idempotency-Replayed") != "" {
			t.Fatalf("GET %s status=%d headers=%v body=%s", path, rec.Code, rec.Header(), rec.Body.String())
		}
		for _, forbidden := range []string{
			"RAW_SPEC_CREATED_BY_SENTINEL", "RAW_EVENT_PAYLOAD_SENTINEL", "RAW_RULE_SENTINEL",
			"RAW_REJECTION_DETAIL_SENTINEL", "RAW_COMMAND_SENTINEL", "RAW_STDOUT_SENTINEL",
			"RAW_STDERR_SENTINEL", lease,
		} {
			if strings.Contains(rec.Body.String(), forbidden) {
				t.Errorf("GET %s exposed forbidden value %q: %s", path, forbidden, rec.Body.String())
			}
		}
		var document any
		if err := json.Unmarshal(rec.Body.Bytes(), &document); err != nil {
			t.Fatal(err)
		}
		assertNoSensitiveJobReadKeys(t, document)
	}
	if after := operatorJobReadCounts(t, st); after != before {
		t.Fatalf("job GETs mutated ledger: before=%v after=%v", before, after)
	}
}

func TestOperatorJobListCursorPinsCreationCeilingAndHasCanonicalCounts(t *testing.T) {
	st := boundaryStore(t)
	original := make(map[string]bool)
	for i := 0; i < 3; i++ {
		original[createOperatorReadJob(t, st, "machine-page", "page-machine", string(rune('a'+i)))] = true
	}
	mux := http.NewServeMux()
	(&hub{store: st}).operatorRoutes(mux)

	seen := make(map[string]bool)
	cursor := ""
	for page := 0; ; page++ {
		path := "/v1/operator/jobs?machine_id=machine-page&limit=1"
		if cursor != "" {
			path += "&cursor=" + url.QueryEscape(cursor)
		}
		rec := operatorRequest(t, mux, http.MethodGet, path, "", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("page %d status=%d body=%s", page, rec.Code, rec.Body.String())
		}
		var doc struct {
			Total       int `json:"total"`
			StateCounts []struct {
				State deploy.JobState `json:"state"`
				Count int             `json:"count"`
			} `json:"state_counts"`
			Items []struct {
				JobID string `json:"job_id"`
			} `json:"items"`
			NextCursor *string `json:"next_cursor"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
			t.Fatal(err)
		}
		if doc.Total != 3 || len(doc.StateCounts) != len(deploy.AllJobStates) || len(doc.Items) != 1 {
			t.Fatalf("page %d envelope=%+v body=%s", page, doc, rec.Body.String())
		}
		if seen[doc.Items[0].JobID] {
			t.Fatalf("cursor repeated job %s", doc.Items[0].JobID)
		}
		seen[doc.Items[0].JobID] = true
		if page == 0 {
			newer := createOperatorReadJob(t, st, "machine-page", "page-machine", "inserted-after-page-one")
			if original[newer] {
				t.Fatal("fixture generated duplicate job id")
			}
		}
		if doc.NextCursor == nil {
			break
		}
		cursor = *doc.NextCursor
		if page > 4 {
			t.Fatal("cursor did not terminate")
		}
	}
	if len(seen) != len(original) {
		t.Fatalf("cursor returned %d original jobs, want %d: %v", len(seen), len(original), seen)
	}
	for jobID := range seen {
		if !original[jobID] {
			t.Fatalf("cursor admitted job created after first page: %s", jobID)
		}
	}
}

func TestOperatorJobReadRejectsBadQueryCursorBindingAndMissingDetail(t *testing.T) {
	st := boundaryStore(t)
	_ = createOperatorReadJob(t, st, "machine-query", "query-machine", "query")
	_ = createOperatorReadJob(t, st, "machine-query", "query-machine", "query-2")
	mux := http.NewServeMux()
	(&hub{store: st}).operatorRoutes(mux)
	first := operatorRequest(t, mux, http.MethodGet,
		"/v1/operator/jobs?machine_id=machine-query&limit=1", "", "")
	var page struct {
		NextCursor *string `json:"next_cursor"`
	}
	if first.Code != http.StatusOK || json.Unmarshal(first.Body.Bytes(), &page) != nil {
		t.Fatalf("first page status=%d body=%s", first.Code, first.Body.String())
	}

	badPaths := []string{
		"/v1/operator/jobs?state=unknown",
		"/v1/operator/jobs?limit=101",
		"/v1/operator/jobs?cursor=not-base64url",
		"/v1/operator/jobs/does-not-exist?include=raw",
	}
	if page.NextCursor != nil {
		badPaths = append(badPaths, "/v1/operator/jobs?machine_id=other&limit=1&cursor="+url.QueryEscape(*page.NextCursor))
	}
	for _, path := range badPaths {
		rec := operatorRequest(t, mux, http.MethodGet, path, "", "")
		if rec.Code != http.StatusBadRequest || rec.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("GET %s status=%d headers=%v body=%s", path, rec.Code, rec.Header(), rec.Body.String())
		}
	}
	missing := operatorRequest(t, mux, http.MethodGet, "/v1/operator/jobs/does-not-exist", "", "")
	if missing.Code != http.StatusNotFound || !strings.Contains(missing.Body.String(), "JOB_NOT_FOUND") {
		t.Fatalf("missing detail status=%d body=%s", missing.Code, missing.Body.String())
	}
}

type operatorJobCounts struct{ Jobs, Events, Verifications, Audit, Idempotency int }

func operatorJobReadCounts(t *testing.T, st *store.Store) operatorJobCounts {
	t.Helper()
	var result operatorJobCounts
	for _, row := range []struct {
		table string
		dst   *int
	}{
		{"jobs", &result.Jobs}, {"job_events", &result.Events},
		{"verification_results", &result.Verifications}, {"audit_log", &result.Audit},
		{"operator_idempotency", &result.Idempotency},
	} {
		if err := st.DB().QueryRow("SELECT COUNT(*) FROM " + row.table).Scan(row.dst); err != nil {
			t.Fatal(err)
		}
	}
	return result
}

func assertNoSensitiveJobReadKeys(t *testing.T, document any) {
	t.Helper()
	forbidden := map[string]bool{
		"lease_token": true, "spec": true, "created_by": true, "payload": true,
		"command": true, "rule_id": true, "stdout_excerpt": true, "stderr_excerpt": true,
		"hostname": true, "unix_user": true, "tailscale_ip": true, "machine_id_hint": true,
	}
	var walk func(any)
	walk = func(node any) {
		switch value := node.(type) {
		case map[string]any:
			for key, child := range value {
				if forbidden[key] {
					t.Errorf("job read exposed forbidden key %q", key)
				}
				walk(child)
			}
		case []any:
			for _, child := range value {
				walk(child)
			}
		}
	}
	walk(document)
}
