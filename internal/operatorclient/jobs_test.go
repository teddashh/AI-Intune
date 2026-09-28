package operatorclient

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/teddashh/AI-Intune/internal/deploy"
	"github.com/teddashh/AI-Intune/internal/operator"
)

const validJobSummaryJSON = `{
  "job_id":"job-1",
  "machine_id":"machine-1",
  "display_name":"cnode",
  "desired_id":"desired-1",
  "deployment_id":"deployment-1",
  "resource_kind":"openclaw",
  "resource_id":"openclaw",
  "revision":7,
  "state":"running",
  "irreversible":false,
  "execution_timeout_seconds":600,
  "artifact_digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
  "artifact_digest_status":"recorded",
  "created_at":"2026-09-07T12:00:00Z",
  "terminal_at":null,
  "lease_status":"active",
  "lease_expires_at":"2026-09-07T12:10:00Z",
  "event_count":1,
  "last_event_received_at":"2026-09-07T12:02:00Z",
  "verification_total":1,
  "verification_passed":1,
  "verification_failed":0
}`

func validJobListJSON(item string) string {
	return fmt.Sprintf(`{
  "schema_version":1,
  "evaluated_at":"2026-09-07T12:05:00.123456789Z",
  "total":1,
  "state_counts":[
    {"state":"not_started","count":0},{"state":"claimed","count":0},
    {"state":"running","count":1},{"state":"verifying","count":0},
    {"state":"succeeded","count":0},{"state":"failed","count":0},
    {"state":"rejected","count":0},{"state":"lease_expired","count":0},
    {"state":"manual_intervention","count":0}
  ],
  "items":[%s],
  "next_cursor":null
}`, item)
}

func validJobDetailJSON(item string) string {
	return fmt.Sprintf(`{
  "schema_version":1,
  "evaluated_at":"2026-09-07T12:05:00.123456789Z",
  "item":%s,
  "desired":{
    "desired_id":"desired-1","scope_type":"machine","scope_id":"machine-1",
    "resource_kind":"openclaw","resource_id":"openclaw","revision":7,
    "created_at":"2026-09-07T11:59:00Z"
  },
  "events":{"total":1,"truncated":false,"items":[{
    "event_id":"event-1","seq":1,"phase":"start",
    "occurred_at":"2026-09-07T12:01:59Z","received_at":"2026-09-07T12:02:00Z"
  }]},
  "verifications":{"total":1,"passed":1,"failed":0,"truncated":false,"items":[{
    "verification_id":"verification-1","passed":true,"exit_code":0,
    "reported_verified_at":"2026-09-07T12:03:00Z"
  }]}
}`, item)
}

func unprovenSucceededJobListJSON() string {
	item := strings.NewReplacer(
		`"state":"running"`, `"state":"succeeded"`,
		`"terminal_at":null`, `"terminal_at":"2026-09-07T12:04:00Z"`,
		`"lease_status":"active"`, `"lease_status":"none"`,
		`"lease_expires_at":"2026-09-07T12:10:00Z"`, `"lease_expires_at":null`,
		`"verification_total":1`, `"verification_total":0`,
		`"verification_passed":1`, `"verification_passed":0`,
	).Replace(validJobSummaryJSON)
	return strings.NewReplacer(
		`{"state":"running","count":1}`, `{"state":"running","count":0}`,
		`{"state":"succeeded","count":0}`, `{"state":"succeeded","count":1}`,
	).Replace(validJobListJSON(item))
}

func terminalJobListJSON(state deploy.JobState, irreversible bool) string {
	item := strings.NewReplacer(
		`"state":"running"`, `"state":"`+string(state)+`"`,
		`"irreversible":false`, fmt.Sprintf(`"irreversible":%t`, irreversible),
		`"terminal_at":null`, `"terminal_at":"2026-09-07T12:04:00Z"`,
		`"lease_status":"active"`, `"lease_status":"none"`,
		`"lease_expires_at":"2026-09-07T12:10:00Z"`, `"lease_expires_at":null`,
	).Replace(validJobSummaryJSON)
	return strings.NewReplacer(
		`{"state":"running","count":1}`, `{"state":"running","count":0}`,
		`{"state":"`+string(state)+`","count":0}`,
		`{"state":"`+string(state)+`","count":1}`,
	).Replace(validJobListJSON(item))
}

func jobDetailWithTwoEvents(second string) string {
	body := strings.NewReplacer(
		`"event_count":1`, `"event_count":2`,
		`"events":{"total":1`, `"events":{"total":2`,
	).Replace(validJobDetailJSON(validJobSummaryJSON))
	return strings.Replace(body,
		`  }]},
  "verifications"`, `  },`+second+`]},
  "verifications"`, 1)
}

func jobDetailWithTwoVerifications(second string) string {
	body := strings.NewReplacer(
		`"verification_total":1`, `"verification_total":2`,
		`"verification_passed":1`, `"verification_passed":2`,
		`"verifications":{"total":1,"passed":1`, `"verifications":{"total":2,"passed":2`,
	).Replace(validJobDetailJSON(validJobSummaryJSON))
	return strings.Replace(body, `  }]}
}`, `  },`+second+`]}
}`, 1)
}

func TestJobReadClientAcceptsCanonicalListDetailAndEncodesFilters(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") != "application/json" || r.UserAgent() != UserAgent {
			t.Errorf("request headers=%v", r.Header)
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "private, no-store")
		switch r.URL.Path {
		case "/v1/operator/jobs":
			if got := r.URL.Query(); got.Get("machine_id") != "machine-1" ||
				got.Get("deployment_id") != "deployment-1" || got.Get("resource_kind") != "openclaw" ||
				got.Get("resource_id") != "openclaw" || got.Get("limit") != "7" ||
				len(got["state"]) != 1 || got["state"][0] != "running" {
				t.Errorf("query=%v", got)
			}
			_, _ = w.Write([]byte(validJobListJSON(validJobSummaryJSON)))
		case "/v1/operator/jobs/job-1":
			_, _ = w.Write([]byte(validJobDetailJSON(validJobSummaryJSON)))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := operatorClientForServer(t, server)
	request := operator.JobListRequest{
		MachineID: "machine-1", States: []deploy.JobState{deploy.Running},
		DeploymentID: "deployment-1", ResourceKind: "openclaw", ResourceID: "openclaw", Limit: 7,
	}
	list, err := client.Jobs(t.Context(), request)
	if err != nil || len(list.Items) != 1 || list.Items[0].JobID != "job-1" {
		t.Fatalf("list=%+v err=%v", list, err)
	}
	if _, ok := list.StorePage(); ok {
		t.Fatal("HTTP-decoded list falsely exposed in-process Store bridge")
	}
	detail, err := client.Job(t.Context(), "job-1")
	if err != nil || detail.Item.JobID != "job-1" || len(detail.Events.Items) != 1 ||
		len(detail.Verifications.Items) != 1 {
		t.Fatalf("detail=%+v err=%v", detail, err)
	}
}

func TestJobReadClientRejectsAmbiguousOrIncompleteNestedBodies(t *testing.T) {
	duplicate := strings.Replace(validJobSummaryJSON,
		`"job_id":"job-1"`, `"job_id":"job-1","job_id":"job-1"`, 1)
	unknown := strings.Replace(validJobSummaryJSON,
		`"display_name":"cnode"`, `"display_name":"cnode","lease_token":"secret"`, 1)
	missing := strings.Replace(validJobSummaryJSON, `  "terminal_at":null,`+"\n", "", 1)
	nullState := strings.Replace(validJobSummaryJSON, `"state":"running"`, `"state":null`, 1)
	tests := []struct{ name, body, want string }{
		{"nested duplicate", validJobListJSON(duplicate), "duplicate object field"},
		{"nested unknown", validJobListJSON(unknown), "unknown field"},
		{"nested missing", validJobListJSON(missing), "missing field"},
		{"null required scalar", validJobListJSON(nullState), "null is not allowed"},
		{"null items", strings.Replace(validJobListJSON(validJobSummaryJSON), `"items":[`+validJobSummaryJSON+`]`, `"items":null`, 1), "null is not allowed"},
		{"trailing", validJobListJSON(validJobSummaryJSON) + `{}`, "trailing JSON"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := machineReadResponseServer(t, http.StatusOK, tt.body, nil)
			defer server.Close()
			_, err := operatorClientForServer(t, server).Jobs(t.Context(), operator.JobListRequest{})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error=%v want=%q", err, tt.want)
			}
		})
	}
}

func TestJobReadClientRejectsContradictoryStateCountsLeaseAndEvidence(t *testing.T) {
	tests := []struct{ name, body, want string }{
		{
			"state count order",
			strings.Replace(validJobListJSON(validJobSummaryJSON),
				`{"state":"not_started","count":0},{"state":"claimed","count":0}`,
				`{"state":"claimed","count":0},{"state":"not_started","count":0}`, 1),
			"state counts",
		},
		{
			"state count contradicts item",
			strings.NewReplacer(
				`{"state":"running","count":1}`, `{"state":"running","count":0}`,
				`{"state":"failed","count":0}`, `{"state":"failed","count":1}`,
			).Replace(validJobListJSON(validJobSummaryJSON)),
			"state counts contradict page items",
		},
		{
			"first page missing continuation",
			strings.NewReplacer(
				`"total":1`, `"total":2`,
				`{"state":"running","count":1}`, `{"state":"running","count":2}`,
			).Replace(validJobListJSON(validJobSummaryJSON)),
			"inconsistent continuation evidence",
		},
		{
			"active lease already expired",
			validJobListJSON(strings.Replace(validJobSummaryJSON,
				`"lease_expires_at":"2026-09-07T12:10:00Z"`, `"lease_expires_at":"2026-09-07T12:04:00Z"`, 1)),
			"active lease",
		},
		{
			"evidence sum",
			validJobListJSON(strings.Replace(validJobSummaryJSON,
				`"verification_failed":0`, `"verification_failed":1`, 1)),
			"evidence totals",
		},
		{
			"noncanonical digest",
			validJobListJSON(strings.Replace(validJobSummaryJSON,
				strings.Repeat("a", 64), strings.Repeat("A", 64), 1)),
			"artifact digest",
		},
		{
			"digest status mismatch",
			validJobListJSON(strings.Replace(validJobSummaryJSON,
				`"artifact_digest_status":"recorded"`, `"artifact_digest_status":"not_recorded"`, 1)),
			"artifact_digest_status",
		},
		{
			"unproven succeeded state",
			unprovenSucceededJobListJSON(),
			"all-passing verification set",
		},
		{
			"ambiguous display name",
			validJobListJSON(strings.Replace(validJobSummaryJSON,
				`"display_name":"cnode"`, `"display_name":" cnode"`, 1)),
			"display_name",
		},
		{
			"route-unsafe job id",
			validJobListJSON(strings.Replace(validJobSummaryJSON,
				`"job_id":"job-1"`, `"job_id":"jobs/1"`, 1)),
			"route-safe",
		},
		{
			"irreversible failed state",
			terminalJobListJSON(deploy.Failed, true),
			"contradicts irreversible",
		},
		{
			"reversible manual intervention",
			terminalJobListJSON(deploy.ManualIntervention, false),
			"contradicts irreversible",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := machineReadResponseServer(t, http.StatusOK, tt.body, nil)
			defer server.Close()
			_, err := operatorClientForServer(t, server).Jobs(t.Context(), operator.JobListRequest{})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error=%v want=%q", err, tt.want)
			}
		})
	}
}

func TestJobReadClientRejectsDetailTargetAndEvidencePageMismatches(t *testing.T) {
	for _, tt := range []struct{ name, body, want string }{
		{"target", strings.Replace(validJobDetailJSON(validJobSummaryJSON), `"job_id":"job-1"`, `"job_id":"other"`, 1), "does not match request"},
		{"desired revision", strings.Replace(validJobDetailJSON(validJobSummaryJSON), `"resource_id":"openclaw","revision":7,`, `"resource_id":"openclaw","revision":8,`, 1), "desired metadata"},
		{"desired machine scope", strings.Replace(validJobDetailJSON(validJobSummaryJSON), `"scope_type":"machine","scope_id":"machine-1"`, `"scope_type":"machine","scope_id":"machine-other"`, 1), "desired metadata"},
		{"event total", strings.Replace(validJobDetailJSON(validJobSummaryJSON), `"events":{"total":1`, `"events":{"total":2`, 1), "event page totals"},
		{"short event window", strings.NewReplacer(`"event_count":1`, `"event_count":2`, `"events":{"total":1,"truncated":false`, `"events":{"total":2,"truncated":true`).Replace(validJobDetailJSON(validJobSummaryJSON)), "event page totals"},
		{"duplicate event id", jobDetailWithTwoEvents(`{
    "event_id":"event-1","seq":2,"phase":"finish",
    "occurred_at":"2026-09-07T12:02:59Z","received_at":"2026-09-07T12:03:00Z"
  }`), "duplicate event_id"},
		{"raw event phase", strings.Replace(validJobDetailJSON(validJobSummaryJSON), `"phase":"start"`, `"phase":"machine-secret"`, 1), "invalid event metadata"},
		{"verification total", strings.Replace(validJobDetailJSON(validJobSummaryJSON), `"verifications":{"total":1`, `"verifications":{"total":2`, 1), "verification page totals"},
		{"short verification window", strings.NewReplacer(`"verification_total":1`, `"verification_total":2`, `"verification_passed":1`, `"verification_passed":2`, `"verifications":{"total":1,"passed":1,"failed":0,"truncated":false`, `"verifications":{"total":2,"passed":2,"failed":0,"truncated":true`).Replace(validJobDetailJSON(validJobSummaryJSON)), "verification page totals"},
		{"duplicate verification id", jobDetailWithTwoVerifications(`{
    "verification_id":"verification-1","passed":true,"exit_code":0,
    "reported_verified_at":"2026-09-07T12:04:00Z"
  }`), "duplicate verification_id"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			server := machineReadResponseServer(t, http.StatusOK, tt.body, nil)
			defer server.Close()
			_, err := operatorClientForServer(t, server).Job(t.Context(), "job-1")
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error=%v want=%q body=%s", err, tt.want, tt.body)
			}
		})
	}
}

func TestJobReadClientRejectsNonAdvancingCursor(t *testing.T) {
	body := strings.NewReplacer(
		`"total":1`, `"total":2`,
		`{"state":"running","count":1}`, `{"state":"running","count":2}`,
		`"next_cursor":null`, `"next_cursor":"cursor-1"`,
	).Replace(validJobListJSON(validJobSummaryJSON))
	server := machineReadResponseServer(t, http.StatusOK, body, nil)
	defer server.Close()
	_, err := operatorClientForServer(t, server).Jobs(t.Context(), operator.JobListRequest{
		Limit: 1, Cursor: "cursor-1",
	})
	if err == nil || !strings.Contains(err.Error(), "cannot advance") {
		t.Fatalf("error=%v want non-advancing cursor rejection", err)
	}
}

func TestJobReadClientRejectsUnsafeHeadersAndBadInputsBeforeIO(t *testing.T) {
	for _, tt := range []struct {
		name    string
		headers http.Header
		want    string
	}{
		{"missing no-store", http.Header{"Content-Type": {"application/json"}}, "no-store"},
		{"etag", http.Header{"Content-Type": {"application/json"}, "Cache-Control": {"no-store"}, "ETag": {`"unsafe"`}}, "ETag"},
		{"replay", http.Header{"Content-Type": {"application/json"}, "Cache-Control": {"no-store"}, "Idempotency-Replayed": {"true"}}, "idempotency replay"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			server := machineReadResponseServer(t, http.StatusOK, validJobListJSON(validJobSummaryJSON), tt.headers)
			defer server.Close()
			_, err := operatorClientForServer(t, server).Jobs(t.Context(), operator.JobListRequest{})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error=%v want=%q", err, tt.want)
			}
		})
	}

	requests := []operator.JobListRequest{
		{ResourceID: "openclaw"},
		{Limit: 101},
		{States: []deploy.JobState{"future"}},
		{States: []deploy.JobState{deploy.Running, deploy.Running}},
		{MachineID: "bad\nvalue"},
	}
	for _, request := range requests {
		if _, _, err := encodeJobListQuery(request); err == nil {
			t.Fatalf("unsafe request accepted: %+v", request)
		}
	}
	for _, jobID := range []string{"", ".", "..", "a/b", "bad\nvalue"} {
		client := &Client{}
		if _, err := client.Job(t.Context(), jobID); err == nil {
			t.Fatalf("unsafe job id accepted: %q", jobID)
		}
	}
}

func TestJobListQueryUsesOpaqueCursorWithoutChangingIt(t *testing.T) {
	query, _, err := encodeJobListQuery(operator.JobListRequest{Cursor: "abc_DEF-123"})
	if err != nil {
		t.Fatal(err)
	}
	encoded := query.Encode()
	decoded, err := url.ParseQuery(encoded)
	if err != nil || decoded.Get("cursor") != "abc_DEF-123" {
		t.Fatalf("query=%q decoded=%v err=%v", encoded, decoded, err)
	}
}
