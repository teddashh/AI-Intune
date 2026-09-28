package operatorclient

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/store"
)

func ticketClientTestResult() operator.TicketReadResult {
	evaluatedAt := time.Date(2026, 9, 11, 16, 0, 0, 0, time.UTC)
	provider := "openai-codex"
	text := func(value string, max int) operator.EvidenceText {
		return operator.EvidenceText{Text: value, MaxBytes: max, Bytes: len(value), Issues: []string{}}
	}
	return operator.TicketReadResult{
		SchemaVersion: operator.TicketReadSchemaVersion, Consistency: operator.TicketReadConsistency,
		EvaluatedAt: evaluatedAt,
		Window: operator.TicketReadWindow{
			From: evaluatedAt.Add(-14 * 24 * time.Hour), To: evaluatedAt, Boundary: "[from,to]",
			TimeBasis: "hub_received_at", Days: 14,
			MaximumSeconds: int64(store.TicketReadMaxWindow / time.Second),
		},
		Roster:        operator.TicketRoster{Expected: 1, Reporting: 1, ReportingRatePercent: 100, SchedulingEligible: true},
		CandidateRows: 3, CandidateBytes: 128, Total: 1, MatchedTotal: 1,
		Items: []operator.TicketProviderItem{{
			ProviderRef: operator.TicketProviderRef(provider), Provider: text(provider, 256),
			Machines: operator.TicketEvidenceList{Total: 1, Items: []operator.EvidenceText{text("samplehub1", 256)}},
			Agents:   operator.TicketEvidenceList{Total: 1, Items: []operator.EvidenceText{text("main", 256)}},
			Runs:     3, LastRunAt: ticketTimePointer(evaluatedAt.Add(-time.Hour)), PeakPerHour: 2,
			PeakHour: ticketTimePointer(evaluatedAt.Add(-2 * time.Hour)), ErrorRuns: 1,
			ErrorVariantsTotal: 1, Errors: []operator.TicketErrorEvidence{{Text: text("gateway restart", 2048), Count: 1}},
		}},
		Coverage: operator.TicketReadCoverage{Issues: []string{}},
		Limits: operator.TicketReadLimits{
			MaximumDays: operator.MaxTicketReadDays, MaximumCandidateRows: store.TicketReadMaxRows,
			MaximumCandidateBytes: store.TicketReadMaxBytes, MaximumProviders: operator.MaxTicketProviders,
			MaximumMachinesPerProvider: operator.MaxTicketMachines,
			MaximumAgentsPerProvider:   operator.MaxTicketAgents,
			MaximumErrorsPerProvider:   operator.MaxTicketErrors,
		},
		Disclosure: operator.TicketReadDisclosure{
			ProviderIdentity:    "upstream_provider_text_not_ticket_profile",
			ErrorClassification: "raw_error_text_not_429_401_classification",
			RunOutcome:          "completed_run_not_verified_success",
			SchedulingRule:      "reporting_rate_at_least_80_percent",
		},
	}
}

func ticketTimePointer(value time.Time) *time.Time { return &value }

func TestTicketsClientAcceptsCanonicalFilteredReadAndQuery(t *testing.T) {
	result := ticketClientTestResult()
	filter := result.Items[0].ProviderRef
	result.ProviderRef = filter
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/operator/tickets" ||
			r.URL.Query().Get("days") != "14" || r.URL.Query().Get("provider_ref") != filter ||
			r.Header.Get("Accept") != "application/json" || r.UserAgent() != UserAgent {
			t.Errorf("request=%s %s query=%v headers=%v", r.Method, r.URL.Path, r.URL.Query(), r.Header)
		}
		changeClientHeaders(w)
		_ = json.NewEncoder(w).Encode(result)
	}))
	defer server.Close()

	got, err := operatorClientForServer(t, server).Tickets(t.Context(), operator.TicketReadRequest{
		Days: 14, ProviderRef: filter,
	})
	if err != nil || len(got.Items) != 1 || got.Items[0].Provider.Text != "openai-codex" {
		t.Fatalf("tickets=%+v err=%v", got, err)
	}
}

func TestTicketsClientRejectsContractContradictions(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*operator.TicketReadResult)
	}{
		{"window", func(result *operator.TicketReadResult) { result.Window.Boundary = "(from,to]" }},
		{"provider binding", func(result *operator.TicketReadResult) { result.Items[0].Provider.Text = "other" }},
		{"roster", func(result *operator.TicketReadResult) { result.Roster.ReportingRatePercent = 99 }},
		{"null evidence", func(result *operator.TicketReadResult) { result.Items[0].Errors = nil }},
		{"error sum", func(result *operator.TicketReadResult) { result.Items[0].Errors[0].Count = 2 }},
	} {
		t.Run(test.name, func(t *testing.T) {
			result := ticketClientTestResult()
			test.mutate(&result)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				changeClientHeaders(w)
				_ = json.NewEncoder(w).Encode(result)
			}))
			defer server.Close()
			if _, err := operatorClientForServer(t, server).Tickets(t.Context(), operator.TicketReadRequest{Days: 14}); err == nil {
				t.Fatal("contradictory ticket response was accepted")
			}
		})
	}
}

func TestTicketsClientRejectsCacheAndReplayHeaders(t *testing.T) {
	for _, header := range []string{"ETag", "Idempotency-Replayed"} {
		t.Run(header, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				changeClientHeaders(w)
				w.Header().Set(header, map[string]string{"ETag": `"x"`, "Idempotency-Replayed": "true"}[header])
				_ = json.NewEncoder(w).Encode(ticketClientTestResult())
			}))
			defer server.Close()
			if _, err := operatorClientForServer(t, server).Tickets(t.Context(), operator.TicketReadRequest{Days: 14}); err == nil {
				t.Fatalf("%s was accepted", header)
			}
		})
	}
}

func TestTicketsClientValidatesBeforeNetwork(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer server.Close()
	_, err := operatorClientForServer(t, server).Tickets(t.Context(), operator.TicketReadRequest{
		Days: 31, ProviderRef: "sha256:" + strings.Repeat("a", 64),
	})
	if err == nil || hits.Load() != 0 {
		t.Fatalf("err=%v network hits=%d", err, hits.Load())
	}
}
