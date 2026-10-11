package operatorclient

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/store"
)

// The orchestrator runs this real HTTP contract test outside the socket sandbox.
func TestSupervisionClientHTTPContract(t *testing.T) {
	var responseExtra atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.Header.Get("Accept") != "application/json" {
			t.Errorf("request %s %v", r.Method, r.Header)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		if responseExtra.Load() {
			_, _ = w.Write([]byte(`{"schema_version":1,"unexpected":true}`))
			return
		}
		switch r.URL.Path {
		case "/v1/operator/jobs-summary":
			if q := r.URL.Query(); q.Get("kind") != "noop" || q.Get("machine_id") != "machine-a" || q.Get("window") != "7d" || q.Get("per_machine") != "true" {
				t.Errorf("filters %v", q)
			}
			_ = json.NewEncoder(w).Encode(operator.JobsSummary{SchemaVersion: 1, Items: []store.JobSummary{}})
		case "/v1/operator/approvals-summary":
			if r.URL.Query().Get("window") != "24h" {
				t.Errorf("query %s", r.URL.RawQuery)
			}
			_ = json.NewEncoder(w).Encode(operator.ApprovalsSummary{SchemaVersion: 1, Windows: []store.ApprovalSummaryWindow{}})
		case "/v1/operator/hub-status":
			if r.URL.RawQuery != "" {
				t.Errorf("status query %s", r.URL.RawQuery)
			}
			_ = json.NewEncoder(w).Encode(operator.HubStatus{SchemaVersion: 1})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	c := operatorClientForServer(t, server)
	if r, err := c.JobsSummary(t.Context(), operator.SupervisionFilter{Kind: "noop", MachineID: "machine-a", Window: "7d", PerMachine: true}); err != nil || r.SchemaVersion != 1 {
		t.Fatalf("jobs %+v %v", r, err)
	}
	if r, err := c.ApprovalsSummary(t.Context(), "24h"); err != nil || r.SchemaVersion != 1 {
		t.Fatalf("approvals %+v %v", r, err)
	}
	if r, err := c.HubStatus(t.Context()); err != nil || r.SchemaVersion != 1 {
		t.Fatalf("status %+v %v", r, err)
	}
	responseExtra.Store(true)
	if _, err := c.HubStatus(t.Context()); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("open DTO contract: %v", err)
	}
}

func TestSupervisionClientRejectsInvalidFiltersBeforeNetwork(t *testing.T) {
	c := &Client{}
	for _, f := range []operator.SupervisionFilter{{Window: "30d"}, {MachineID: " padded "}, {Kind: "bad\nkind"}} {
		if _, err := c.JobsSummary(t.Context(), f); err == nil {
			t.Fatalf("filter reached transport %+v", f)
		}
	}
	if _, err := c.ApprovalsSummary(t.Context(), "30d"); err == nil {
		t.Fatal("invalid window reached transport")
	}
}

func TestSupervisionEmptyResponseArraysStayStrict(t *testing.T) {
	for _, test := range []struct {
		name, field      string
		response, target any
	}{
		{"jobs", "items", operator.JobsSummary{SchemaVersion: 1, Items: []store.JobSummary{}}, &operator.JobsSummary{}},
		{"approvals", "windows", operator.ApprovalsSummary{SchemaVersion: 1, Windows: []store.ApprovalSummaryWindow{}}, &operator.ApprovalsSummary{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			raw, err := json.Marshal(test.response)
			if err != nil {
				t.Fatal(err)
			}
			if err := decodeStrictJSONDocument(raw, "supervision", test.target); err != nil {
				t.Fatalf("empty arrays rejected: %v", err)
			}
			invalid := strings.Replace(string(raw), `"`+test.field+`":[]`, `"`+test.field+`":null`, 1)
			if err := decodeStrictJSONDocument([]byte(invalid), "supervision", test.target); err == nil {
				t.Fatal("null array accepted")
			}
		})
	}
}
