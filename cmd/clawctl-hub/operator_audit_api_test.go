package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/store"
)

func TestOperatorAuditEventsReturnsFilteredNoStoreSafeRead(t *testing.T) {
	st := boundaryStore(t)
	mux := http.NewServeMux()
	(&hub{store: st}).operatorRoutes(mux)
	if err := st.RecordAudit(store.AuditEntry{
		Action: store.AuditMachineChannel, MachineID: "machine-1", Subject: "samplehub1",
		Reason: "planned", IdempotencyKey: "audit-request-1", RequestDigest: "sha256:digest",
		SourceAddr: "100.64.0.7", WhoUser: "owner@example.com", AuthSubject: "tailscale-user:42",
		AuthCapability: "example.com/cap/clawctl-admin", SourceKind: "operator-api", OK: false,
	}); err != nil {
		t.Fatal(err)
	}
	before := auditAPIRowCount(t, st)
	path := "/v1/operator/audit-events?machine_id=machine-1&action=machine-channel&outcome=failed" +
		"&principal=tailscale-user%3A42&capability=example.com%2Fcap%2Fclawctl-admin" +
		"&source_kind=operator-api&correlation=audit-request-1&denials=all&limit=10"
	response := operatorRequest(t, mux, http.MethodGet, path, "", "")
	if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-store" ||
		response.Header().Get("ETag") != "" || response.Header().Get("Idempotency-Replayed") != "" {
		t.Fatalf("status=%d headers=%v body=%s", response.Code, response.Header(), response.Body.String())
	}
	var result operator.AuditListResult
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.SchemaVersion != operator.AuditReadSchemaVersion || result.Consistency != operator.AuditReadConsistency ||
		result.MatchedTotal != 1 || result.Total != 1 || result.Failed != 1 || len(result.Items) != 1 ||
		result.Items[0].MachineID == nil || *result.Items[0].MachineID != "machine-1" ||
		result.Items[0].Outcome == nil || *result.Items[0].Outcome != store.AuditOutcomeFailed {
		t.Fatalf("result=%+v", result)
	}
	if after := auditAPIRowCount(t, st); after != before {
		t.Fatalf("audit GET mutated ledger: %d -> %d", before, after)
	}
}

func TestOperatorAuditEventsKeepsMalformedLedgerEvidenceExplicit(t *testing.T) {
	st := boundaryStore(t)
	mux := http.NewServeMux()
	(&hub{store: st}).operatorRoutes(mux)
	if _, err := st.DB().Exec(`INSERT INTO audit_log
 (at,action,subject,source_addr,outcome) VALUES (?,?,?,?,?)`,
		"not-a-time", "future-action", "malformed", "", "maybe"); err != nil {
		t.Fatal(err)
	}
	response := operatorRequest(t, mux, http.MethodGet,
		"/v1/operator/audit-events?denials=all", "", "")
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var result operator.AuditListResult
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Items) != 1 || result.Items[0].At != nil || result.Items[0].Outcome != nil ||
		len(result.Items[0].Issues) != 4 || result.UnknownOutcome != 1 {
		t.Fatalf("malformed evidence was normalized: %+v", result)
	}
}

func TestOperatorAuditEventsRejectsAmbiguousQueries(t *testing.T) {
	for _, suffix := range []string{
		"?", "?unknown=value", "?machine_id=a&machine_id=b", "?action=unknown",
		"?action=connect&action=connect", "?outcome=maybe", "?denials=maybe",
		"?limit=0", "?limit=101", "?cursor=%20bad", "?from=2026-09-08",
		"?from=2026-09-08T12%3A00%3A00.1Z", "?from=2026-09-09T00%3A00%3A00Z&to=2026-09-08T00%3A00%3A00Z",
	} {
		t.Run(suffix, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/v1/operator/audit-events"+suffix, nil)
			if _, err := parseOperatorAuditListRequest(request); err == nil {
				t.Fatalf("query %q was accepted", suffix)
			}
		})
	}
}

func TestOperatorAuditEventsParsesUTCNormalizedWindow(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet,
		"/v1/operator/audit-events?from=2026-09-08T08%3A00%3A00-04%3A00&to=2026-09-08T13%3A00%3A00Z", nil)
	parsed, err := parseOperatorAuditListRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.From == nil || parsed.To == nil || parsed.From.Format(time.RFC3339) != "2026-09-08T12:00:00Z" ||
		parsed.To.Format(time.RFC3339) != "2026-09-08T13:00:00Z" {
		t.Fatalf("window=%v..%v", parsed.From, parsed.To)
	}
}

func auditAPIRowCount(t *testing.T, st *store.Store) int {
	t.Helper()
	var count int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM audit_log`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestOperatorAuditEventsDoesNotEchoBadQueryInGenericCursorError(t *testing.T) {
	st := boundaryStore(t)
	mux := http.NewServeMux()
	(&hub{store: st}).operatorRoutes(mux)
	secret := "do-not-reflect-this-secret"
	response := operatorRequest(t, mux, http.MethodGet,
		"/v1/operator/audit-events?cursor="+secret, "", "")
	if response.Code != http.StatusBadRequest || strings.Contains(response.Body.String(), secret) {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}
