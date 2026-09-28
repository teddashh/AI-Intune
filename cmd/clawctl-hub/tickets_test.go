package main

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestTicketsCLIUsesHTTPContractForJSONAndCSV(t *testing.T) {
	f := newJobsFixture(t, "ticket-cli-machine")
	(&hub{store: f.store}).operatorRoutes(f.mux)
	now := time.Now().UTC().Truncate(time.Second)
	if _, err := f.store.DB().Exec(`INSERT INTO ticket_occupancy_observation
(observation_id,machine_id,provider,measured_at,received_at,occupant_evidence,agent_id,process_alive,run_status,last_error_text,source)
VALUES(?,?,?,?,?,?,?,?,?,?,?)`, "ticket-cli-row", f.machine.id, "=FORMULA",
		now.Add(-time.Hour).Format(time.RFC3339), now.Add(-time.Minute).Format(time.RFC3339),
		"agent:main:cron:job:run:1", "main", 0, "ok", "+ERROR", "cron_run_logs"); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(f.mux)
	defer server.Close()
	base, deps := machineHTTPTestDeps(t, server)
	deps.discoverHubURL = func() (string, error) { return "", errors.New("explicit URL invoked discovery") }

	var jsonOut, errOut bytes.Buffer
	if err := runTicketsCommandWithDeps(t.Context(), []string{
		"--hub-url", base, "--days", "14", "--json",
	}, &jsonOut, &errOut, deps); err != nil {
		t.Fatalf("tickets JSON: %v stderr=%s", err, errOut.String())
	}
	for _, wanted := range []string{`"schema_version": 1`, `"days": 14`, `"provider_ref": "sha256:`} {
		if !strings.Contains(jsonOut.String(), wanted) {
			t.Errorf("JSON missing %q: %s", wanted, jsonOut.String())
		}
	}

	var csvOut bytes.Buffer
	if err := runTicketsCommandWithDeps(t.Context(), []string{
		"--hub-url", base, "--csv",
	}, &csvOut, &errOut, deps); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(csvOut.String(), "provider_ref") || !strings.Contains(csvOut.String(), "'=FORMULA") ||
		!strings.Contains(csvOut.String(), "'+ERROR") {
		t.Fatalf("CSV did not preserve safe bounded evidence: %q", csvOut.String())
	}
}

func TestTicketsCLIRejectsInvalidInputsBeforeTransport(t *testing.T) {
	for _, args := range [][]string{
		{"--days", "31"}, {"--days", "07"}, {"--json", "--csv"},
		{"--hub-url", "http://127.0.0.1", "--db", "/tmp/not-used"},
		{"--provider-ref", "sha256:short"}, {"extra"},
	} {
		var out, errOut bytes.Buffer
		if err := runTicketsCommandWithDeps(t.Context(), args, &out, &errOut, machineCommandDeps{}); err == nil {
			t.Fatalf("arguments %q were accepted", args)
		}
	}
}

func TestTicketsRouteIsGETOnly(t *testing.T) {
	f := newJobsFixture(t, "ticket-cli-method")
	(&hub{store: f.store}).operatorRoutes(f.mux)
	recorder := httptest.NewRecorder()
	f.mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/operator/tickets", nil))
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST status=%d", recorder.Code)
	}
}
