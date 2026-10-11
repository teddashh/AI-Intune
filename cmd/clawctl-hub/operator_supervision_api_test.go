package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/operatorauth"
	"github.com/teddashh/AI-Intune/internal/store"
)

func TestOperatorSupervisionAPIEmptyViewAndNoWrites(t *testing.T) {
	st := boundaryStore(t)
	mux := http.NewServeMux()
	(&hub{store: st, startedAt: time.Now().Add(-time.Hour)}).operatorRoutes(mux)
	auth := &boundaryAuthorizer{allow: true}
	boundary := newOperatorBoundary(mux, auth, st, operatorRoutePolicies, testOperatorAuthority)
	client, _ := operatorClientForMuxWithoutListener(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.RemoteAddr = "100.100.10.20:12345"
		boundary.ServeHTTP(w, r)
	}), "http://"+testOperatorAuthority)
	jobs, err := client.JobsSummary(t.Context(), operator.SupervisionFilter{})
	if err != nil || jobs.Items == nil || len(jobs.Items) != 0 || jobs.SchemaVersion != 1 {
		t.Fatalf("jobs %+v %v", jobs, err)
	}
	approvals, err := client.ApprovalsSummary(t.Context(), "24h")
	if err != nil || len(approvals.Windows) != 1 || approvals.PendingOlderThan4h != nil {
		t.Fatalf("approvals %+v %v", approvals, err)
	}
	status, err := client.HubStatus(t.Context())
	if err != nil || status.UptimeSeconds == nil || *status.UptimeSeconds < 3600 || status.DBSizeBytes == nil || status.Machines.Active != 0 {
		t.Fatalf("status %+v %v", status, err)
	}
	calls := auth.snapshotCalls()
	if len(calls) != 3 {
		t.Fatalf("authorization calls %v", calls)
	}
	for _, scope := range calls {
		if scope != operatorauth.View {
			t.Fatalf("requires %v", scope)
		}
	}
	for _, table := range []string{"audit_log", "operator_idempotency", "jobs", "restore_drill_operations"} {
		var count int
		if err := st.DB().QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("GET wrote %s: %d %v", table, count, err)
		}
	}
	for _, path := range []string{"/v1/operator/jobs-summary", "/v1/operator/approvals-summary", "/v1/operator/hub-status"} {
		rec := operatorRequest(t, mux, http.MethodGet, path, "", "")
		if rec.Code != 200 || rec.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("%s: %d %v", path, rec.Code, rec.Header())
		}
		if path == "/v1/operator/jobs-summary" && !strings.Contains(rec.Body.String(), `"items":[]`) {
			t.Fatalf("empty jobs must emit an array: %s", rec.Body.String())
		}
		if path == "/v1/operator/approvals-summary" && strings.Contains(rec.Body.String(), `"windows":null`) {
			t.Fatalf("approval windows must emit an array: %s", rec.Body.String())
		}
		for _, bad := range []string{"?unknown=x", "?window=30d", "?window=24h&window=7d", "?window="} {
			r := operatorRequest(t, mux, http.MethodGet, path+bad, "", "")
			if r.Code != 400 {
				t.Fatalf("%s%s: %d", path, bad, r.Code)
			}
		}
	}
	for _, query := range []string{"?per_machine=1", "?kind=%20noop", "?machine_id=a&machine_id=b"} {
		rec := operatorRequest(t, mux, http.MethodGet, "/v1/operator/jobs-summary"+query, "", "")
		if rec.Code != 400 {
			t.Fatalf("%s: %d", query, rec.Code)
		}
	}
}

func TestOperatorSupervisionAPIPopulatedAndDisclosure(t *testing.T) {
	st := boundaryStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	if err := st.UpsertMachine(store.Machine{MachineID: "machine-a", DisplayName: "sample-agent", Hostname: "PRIVATE_HOST_MARKER", UnixUser: "PRIVATE_USER_MARKER", Expected: true, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	desired, rev, err := st.CreateDesiredState("machine", "machine-a", "diagnostic", "noop", `{"kind":"noop","token":"PRIVATE_SECRET_MARKER","path":"PRIVATE_PATH_MARKER"}`, "PRIVATE_USER_MARKER")
	if err != nil {
		t.Fatal(err)
	}
	job, err := st.CreateJob("machine-a", desired, rev, store.NewJob{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`UPDATE jobs SET state='failed',created_at=?,terminal_at=?,lease_token='PRIVATE_LEASE_MARKER' WHERE job_id=?`, now.Add(-time.Hour).Format(time.RFC3339), now.Format(time.RFC3339), job); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordAudit(store.AuditEntry{At: now, Action: store.AuditDiagnosticNoop, OK: true, Subject: "PRIVATE_HOST_MARKER", Detail: "PRIVATE_SECRET_MARKER", IdempotencyKey: "key-a", RequestDigest: "digest-a"}); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	(&hub{store: st, startedAt: now.Add(-time.Hour)}).operatorRoutes(mux)
	rec := operatorRequest(t, mux, http.MethodGet, "/v1/operator/jobs-summary?kind=noop&machine_id=machine-a&window=24h", "", "")
	var r operator.JobsSummary
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &r) != nil || len(r.Items) != 1 || r.Items[0].Windows[0].Counts.Failed != 1 || !*r.Items[0].Flags.FailureRate24h {
		t.Fatalf("summary %d %s", rec.Code, rec.Body.String())
	}
	rec = operatorRequest(t, mux, http.MethodGet, "/v1/operator/approvals-summary", "", "")
	var a operator.ApprovalsSummary
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &a) != nil || a.Windows[0].Applied != 1 {
		t.Fatalf("approvals %s", rec.Body.String())
	}
	for _, path := range []string{"/v1/operator/jobs-summary", "/v1/operator/approvals-summary", "/v1/operator/hub-status"} {
		rec := operatorRequest(t, mux, http.MethodGet, path, "", "")
		if strings.Contains(rec.Body.String(), "PRIVATE_") || strings.Contains(rec.Body.String(), "/tmp/") {
			t.Fatalf("disclosure %s: %s", path, rec.Body.String())
		}
	}
}

func TestOperatorSupervisionRequiresView(t *testing.T) {
	st := boundaryStore(t)
	mux := http.NewServeMux()
	(&hub{store: st}).operatorRoutes(mux)
	auth := &boundaryAuthorizer{decision: operatorauth.Decision{HTTPStatus: 403, Code: operatorauth.CapabilityRequired, Detail: "view required"}}
	boundary := newOperatorBoundary(mux, auth, st, operatorRoutePolicies, testOperatorAuthority)
	for _, path := range []string{"/v1/operator/jobs-summary", "/v1/operator/approvals-summary", "/v1/operator/hub-status"} {
		rec := httptest.NewRecorder()
		boundary.ServeHTTP(rec, newBoundaryRequest(http.MethodGet, path, nil))
		if rec.Code != 403 {
			t.Fatalf("scope %s: %d", path, rec.Code)
		}
	}
	for _, scope := range auth.snapshotCalls() {
		if scope != operatorauth.View {
			t.Fatalf("scope %v", scope)
		}
	}
}
