package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/operator"
)

func TestOperatorTicketsReturnsBoundedSafeSnapshotWithoutMutation(t *testing.T) {
	f := newJobsFixture(t, "ticket-api-machine")
	(&hub{store: f.store}).operatorRoutes(f.mux)
	now := time.Now().UTC().Truncate(time.Second)
	provider := "openai-codex"
	if _, err := f.store.DB().Exec(`INSERT INTO ticket_occupancy_observation
(observation_id,machine_id,provider,measured_at,received_at,occupant_evidence,agent_id,process_alive,run_status,last_error_text,source)
VALUES(?,?,?,?,?,?,?,?,?,?,?)`, "ticket-api-row", f.machine.id, provider,
		now.Add(-time.Hour).Format(time.RFC3339), now.Add(-time.Minute).Format(time.RFC3339),
		"agent:main:cron:job:run:1", "main", 0, "ok", "gateway restart", "cron_run_logs"); err != nil {
		t.Fatal(err)
	}
	var before int
	if err := f.store.DB().QueryRow(`SELECT COUNT(*) FROM ticket_occupancy_observation`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	providerRef := operator.TicketProviderRef(provider)
	response := operatorRequest(t, f.mux, http.MethodGet,
		"/v1/operator/tickets?days=14&provider_ref="+providerRef, "", "")
	if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-store" ||
		response.Header().Get("ETag") != "" || response.Header().Get("Idempotency-Replayed") != "" {
		t.Fatalf("status=%d headers=%v body=%s", response.Code, response.Header(), response.Body.String())
	}
	var result operator.TicketReadResult
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.SchemaVersion != operator.TicketReadSchemaVersion || result.Consistency != operator.TicketReadConsistency ||
		result.ProviderRef != providerRef || result.Window.Days != 14 || result.Total != 1 ||
		result.MatchedTotal != 1 || len(result.Items) != 1 || result.Items[0].Provider.Text != provider ||
		result.Items[0].Runs != 1 || result.Items[0].ErrorRuns != 1 || len(result.Items[0].Errors) != 1 {
		t.Fatalf("result=%+v", result)
	}
	var after int
	if err := f.store.DB().QueryRow(`SELECT COUNT(*) FROM ticket_occupancy_observation`).Scan(&after); err != nil || after != before {
		t.Fatalf("GET mutated ticket evidence: %d -> %d err=%v", before, after, err)
	}
}

func TestOperatorTicketsRejectsAmbiguousOrNoncanonicalQueries(t *testing.T) {
	for _, suffix := range []string{
		"?", "?unknown=value", "?days=7&days=14", "?days=", "?days=0", "?days=31",
		"?days=07", "?days=%2B7", "?provider_ref=", "?provider_ref=sha256:abc",
		"?provider_ref=SHA256:" + strings.Repeat("a", 64),
	} {
		t.Run(suffix, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/v1/operator/tickets"+suffix, nil)
			if _, err := parseOperatorTicketReadRequest(request); err == nil {
				t.Fatalf("query %q was accepted", suffix)
			}
		})
	}
}

func TestOperatorTicketsDoesNotReflectRejectedProviderReference(t *testing.T) {
	f := newJobsFixture(t, "ticket-api-reject")
	(&hub{store: f.store}).operatorRoutes(f.mux)
	secret := "do-not-reflect-ticket-secret"
	response := operatorRequest(t, f.mux, http.MethodGet,
		"/v1/operator/tickets?provider_ref="+secret, "", "")
	if response.Code != http.StatusBadRequest || strings.Contains(response.Body.String(), secret) {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}
