package main

import (
	"encoding/json"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/store"
)

func TestOperatorMachineReadListAndDetailAreSafeAndReadOnly(t *testing.T) {
	f := observedOperatorFixture(t)
	f.store.DB().SetMaxOpenConns(1)
	if _, err := f.store.DB().Exec("PRAGMA query_only=ON"); err != nil {
		t.Fatal(err)
	}
	before := machineReadLedgerCounts(t, f.store)

	list := operatorRequest(t, f.mux, http.MethodGet, "/v1/operator/machines", "", "")
	if list.Code != http.StatusOK || list.Header().Get("Cache-Control") != "no-store" ||
		list.Header().Get("Idempotency-Replayed") != "" || list.Header().Get("ETag") != "" {
		t.Fatalf("list status=%d headers=%v body=%s", list.Code, list.Header(), list.Body.String())
	}
	listDoc := decodeMachineReadDocument(t, list.Body.Bytes())
	assertExactJSONKeys(t, listDoc, []string{
		"schema_version", "consistency", "evaluated_at", "creation_ceiling", "total", "matched_total", "active", "retired", "expected",
		"reporting", "state_counts", "denominator_state_counts", "items", "next_cursor",
	})
	items, ok := listDoc["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("list items=%T %+v", listDoc["items"], listDoc["items"])
	}
	item, ok := items[0].(map[string]any)
	if !ok {
		t.Fatalf("list item=%T", items[0])
	}
	assertExactJSONKeys(t, item, machineSummaryJSONKeys())
	assertNoSensitiveMachineReadKeys(t, listDoc, false)

	detailPath := "/v1/operator/machines/" + f.machine.id
	detail := operatorRequest(t, f.mux, http.MethodGet, detailPath, "", "")
	if detail.Code != http.StatusOK || detail.Header().Get("Cache-Control") != "no-store" ||
		detail.Header().Get("Idempotency-Replayed") != "" || detail.Header().Get("ETag") != "" {
		t.Fatalf("detail status=%d headers=%v body=%s", detail.Code, detail.Header(), detail.Body.String())
	}
	detailDoc := decodeMachineReadDocument(t, detail.Body.Bytes())
	assertExactJSONKeys(t, detailDoc, []string{
		"schema_version", "evaluated_at", "item", "judgement", "expectations", "disclosure", "monitor",
		"checkins", "state_history", "identity", "identity_hints", "resources",
	})
	judgement, ok := detailDoc["judgement"].(map[string]any)
	if !ok {
		t.Fatalf("detail judgement=%T", detailDoc["judgement"])
	}
	assertExactJSONKeys(t, judgement, []string{"state", "reason", "affects_fleet_state", "findings"})
	findings, ok := judgement["findings"].(map[string]any)
	if !ok {
		t.Fatalf("detail findings=%T", judgement["findings"])
	}
	assertExactJSONKeys(t, findings, []string{"total", "invalid", "truncated", "items"})
	expectations, ok := detailDoc["expectations"].(map[string]any)
	if !ok {
		t.Fatalf("detail expectations=%T", detailDoc["expectations"])
	}
	assertExactJSONKeys(t, expectations, []string{"configured", "read_failed", "error", "rules"})
	rules, ok := expectations["rules"].(map[string]any)
	if !ok {
		t.Fatalf("detail expectation rules=%T", expectations["rules"])
	}
	assertExactJSONKeys(t, rules, []string{"total", "invalid", "truncated", "items"})
	detailItem, ok := detailDoc["item"].(map[string]any)
	if !ok {
		t.Fatalf("detail item=%T", detailDoc["item"])
	}
	assertExactJSONKeys(t, detailItem, machineSummaryJSONKeys())
	if detailItem["machine_id"] != f.machine.id {
		t.Fatalf("detail machine_id=%v want=%s", detailItem["machine_id"], f.machine.id)
	}
	assertNoSensitiveMachineReadKeys(t, detailDoc, true)

	after := machineReadLedgerCounts(t, f.store)
	if before != after {
		t.Fatalf("GET machine reads mutated ledger: before=%v after=%v", before, after)
	}
}

func TestOperatorMachineReadRejectsMalformedQueriesAndMissingMachine(t *testing.T) {
	f := observedOperatorFixture(t)
	for _, path := range []string{
		"/v1/operator/machines?",
		"/v1/operator/machines?unknown=1",
		"/v1/operator/machines?limit=0",
		"/v1/operator/machines?limit=101",
		"/v1/operator/machines?limit=one",
		"/v1/operator/machines?limit=01",
		"/v1/operator/machines?limit=%2B1",
		"/v1/operator/machines?state=Online&state=Online",
		"/v1/operator/machines?state=not-a-state",
		"/v1/operator/machines?lifecycle=active&lifecycle=retired",
		"/v1/operator/machines?expected=true",
		"/v1/operator/machines?reporting=maybe",
		"/v1/operator/machines?cursor=not-base64url",
		"/v1/operator/machines/" + f.machine.id + "?include=facts",
	} {
		rec := operatorRequest(t, f.mux, http.MethodGet, path, "", "")
		if rec.Code != http.StatusBadRequest || rec.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("query %s status=%d headers=%v body=%s", path, rec.Code, rec.Header(), rec.Body.String())
		}
		var apiErr struct {
			Code string `json:"code"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &apiErr); err != nil || apiErr.Code != "BAD_REQUEST" {
			t.Fatalf("query %s error=%+v decode=%v", path, apiErr, err)
		}
	}

	missing := operatorRequest(t, f.mux, http.MethodGet, "/v1/operator/machines/not-present", "", "")
	if missing.Code != http.StatusNotFound || missing.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("missing status=%d headers=%v body=%s", missing.Code, missing.Header(), missing.Body.String())
	}
	var apiErr struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(missing.Body.Bytes(), &apiErr); err != nil ||
		apiErr.Code != store.OperatorCodeMachineNotFound {
		t.Fatalf("missing error=%+v decode=%v", apiErr, err)
	}
}

func TestOperatorMachineListAcceptsStrictFiltersAndKeysetPaging(t *testing.T) {
	st := boundaryStore(t)
	created := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	for _, machine := range []store.Machine{
		{MachineID: "machine-a", DisplayName: "Alpha", Expected: true, CreatedAt: created},
		{MachineID: "machine-b", DisplayName: "Bravo", Expected: true, CreatedAt: created.Add(time.Second)},
		{MachineID: "machine-c", DisplayName: "Charlie", Expected: false, CreatedAt: created.Add(2 * time.Second)},
	} {
		if err := st.UpsertMachine(machine); err != nil {
			t.Fatal(err)
		}
	}
	mux := http.NewServeMux()
	(&hub{store: st}).operatorRoutes(mux)

	first := operatorRequest(t, mux, http.MethodGet,
		"/v1/operator/machines?lifecycle=active&state=NeverReported&channel=none&limit=1", "", "")
	if first.Code != http.StatusOK {
		t.Fatalf("first page status=%d body=%s", first.Code, first.Body.String())
	}
	firstDoc := decodeMachineReadDocument(t, first.Body.Bytes())
	if firstDoc["total"] != float64(3) || firstDoc["matched_total"] != float64(3) {
		t.Fatalf("first page totals=%+v", firstDoc)
	}
	firstItems := firstDoc["items"].([]any)
	if len(firstItems) != 1 || firstDoc["next_cursor"] == nil {
		t.Fatalf("first page items/cursor=%+v", firstDoc)
	}
	firstID := firstItems[0].(map[string]any)["machine_id"]
	cursor := firstDoc["next_cursor"].(string)
	second := operatorRequest(t, mux, http.MethodGet,
		"/v1/operator/machines?lifecycle=active&state=NeverReported&channel=none&limit=1&cursor="+url.QueryEscape(cursor), "", "")
	if second.Code != http.StatusOK {
		t.Fatalf("second page status=%d body=%s", second.Code, second.Body.String())
	}
	secondDoc := decodeMachineReadDocument(t, second.Body.Bytes())
	secondItems := secondDoc["items"].([]any)
	if len(secondItems) != 1 || secondItems[0].(map[string]any)["machine_id"] == firstID ||
		secondDoc["creation_ceiling"] != firstDoc["creation_ceiling"] {
		t.Fatalf("second page did not advance: first=%+v second=%+v", firstDoc, secondDoc)
	}

	exact := operatorRequest(t, mux, http.MethodGet,
		"/v1/operator/machines?machine_id=machine-a&display_name="+url.QueryEscape("Alpha"), "", "")
	if exact.Code != http.StatusOK {
		t.Fatalf("exact filter status=%d body=%s", exact.Code, exact.Body.String())
	}
	exactDoc := decodeMachineReadDocument(t, exact.Body.Bytes())
	if exactDoc["matched_total"] != float64(1) || len(exactDoc["items"].([]any)) != 1 {
		t.Fatalf("exact filter response=%+v", exactDoc)
	}
}

func TestOperatorMachineListKeepsAnEmptyFleetExplicit(t *testing.T) {
	st := boundaryStore(t)
	mux := http.NewServeMux()
	(&hub{store: st}).operatorRoutes(mux)
	rec := operatorRequest(t, mux, http.MethodGet, "/v1/operator/machines", "", "")
	if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("empty list status=%d headers=%v body=%s", rec.Code, rec.Header(), rec.Body.String())
	}
	doc := decodeMachineReadDocument(t, rec.Body.Bytes())
	items, ok := doc["items"].([]any)
	if !ok || len(items) != 0 || doc["total"] != float64(0) || doc["active"] != float64(0) ||
		doc["matched_total"] != float64(0) || doc["creation_ceiling"] != float64(0) ||
		doc["retired"] != float64(0) || doc["expected"] != float64(0) || doc["reporting"] != float64(0) {
		t.Fatalf("empty list did not distinguish []/zero: %+v", doc)
	}
	for _, field := range []string{"state_counts", "denominator_state_counts"} {
		counts, ok := doc[field].([]any)
		if !ok || len(counts) != 5 {
			t.Fatalf("empty %s=%T %+v", field, doc[field], doc[field])
		}
	}
}

type machineReadCounts struct {
	Audit, Idempotency, StateHistory int
}

func machineReadLedgerCounts(t *testing.T, st *store.Store) machineReadCounts {
	t.Helper()
	var result machineReadCounts
	for _, query := range []struct {
		SQL string
		Dst *int
	}{
		{"SELECT COUNT(*) FROM audit_log", &result.Audit},
		{"SELECT COUNT(*) FROM operator_idempotency", &result.Idempotency},
		{"SELECT COUNT(*) FROM machine_state_history", &result.StateHistory},
	} {
		if err := st.DB().QueryRow(query.SQL).Scan(query.Dst); err != nil {
			t.Fatal(err)
		}
	}
	return result
}

func decodeMachineReadDocument(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var document map[string]any
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatalf("decode machine read: %v; body=%s", err, raw)
	}
	return document
}

func machineSummaryJSONKeys() []string {
	return []string{
		"machine_id", "display_name", "expected", "created_at", "enrolled_at", "retired_at",
		"channel", "channel_revision", "state", "state_since", "reporting",
		"last_checkin_received_at", "last_observation_received_at", "issues", "altered_fields",
	}
}

func assertExactJSONKeys(t *testing.T, object map[string]any, want []string) {
	t.Helper()
	wanted := make(map[string]bool, len(want))
	for _, key := range want {
		wanted[key] = true
	}
	if len(object) != len(wanted) {
		t.Fatalf("JSON keys=%v want=%v", object, want)
	}
	for key := range object {
		if !wanted[key] {
			t.Fatalf("unexpected JSON key %q in %+v", key, object)
		}
	}
}

func assertNoSensitiveMachineReadKeys(t *testing.T, document any, detail bool) {
	t.Helper()
	forbidden := map[string]bool{
		"agent_token": true, "agent_token_hash": true, "notes": true, "facts": true,
		"connect": true, "pending_token": true, "pending_enrollment": true,
		"credentials": true, "cli_tools": true, "journals": true, "run_summaries": true,
		"path": true, "argv": true, "account": true, "last_error": true,
	}
	if !detail {
		for _, key := range []string{"hostname", "unix_user", "tailscale_ip", "machine_id_hint", "reason", "message", "agent_version"} {
			forbidden[key] = true
		}
	}
	var walk func(any)
	walk = func(node any) {
		switch value := node.(type) {
		case map[string]any:
			for key, child := range value {
				if forbidden[key] {
					t.Errorf("machine read exposed forbidden key %q", key)
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
