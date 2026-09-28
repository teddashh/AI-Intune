package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/operatorauth"
	"github.com/teddashh/AI-Intune/internal/store"
)

func TestOperatorMachineEvidenceRouteIsViewJSONNoStoreAndBounded(t *testing.T) {
	f := observedOperatorFixture(t)
	path := "/v1/operator/machines/" + f.machine.id + "/evidence"

	rec := operatorRequest(t, f.mux, http.MethodGet, path, "", "")
	if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("machine evidence status=%d headers=%v body=%s", rec.Code, rec.Header(), rec.Body.String())
	}
	client, _ := operatorClientForMuxWithoutListener(t, f.mux, "http://100.64.0.9:8787")
	result, err := client.MachineEvidence(t.Context(), f.machine.id, 0)
	if err != nil || result.SchemaVersion != operator.MachineEvidenceSchemaVersion ||
		result.MachineID != f.machine.id || result.Disclosure.Limit != operator.MachineEvidenceDefaultLimit ||
		!result.Disclosure.HostPathFieldsExcluded || !result.Disclosure.ProcessIDFieldsExcluded ||
		!result.Disclosure.OpenClawDatabaseLocationsExcluded || result.Disclosure.EvidenceTextPathRedactedByHub ||
		!result.Disclosure.SystemdMainPIDExcluded || result.Disclosure.SystemdStateIsWorkOutcome ||
		result.SystemdUnits.Items == nil || result.CLITools.Items == nil {
		t.Fatalf("strict machine evidence decode=%+v err=%v", result, err)
	}
	for _, field := range []string{`"main_pid"`, `"running_pid"`, `"unit_path"`, `"realpath"`, `"daemon_path"`} {
		if strings.Contains(rec.Body.String(), field) {
			t.Fatalf("machine evidence exposed host-coordinate field %s: %s", field, rec.Body.String())
		}
	}

	for _, suffix := range []string{"?limit=0", "?limit=101", "?limit=abc", "?foo=1"} {
		bad := operatorRequest(t, f.mux, http.MethodGet, path+suffix, "", "")
		assertAPIError(t, bad, http.StatusBadRequest, "BAD_REQUEST")
	}
	missing := operatorRequest(t, f.mux, http.MethodGet,
		"/v1/operator/machines/no-such-machine/evidence", "", "")
	assertAPIError(t, missing, http.StatusNotFound, store.OperatorCodeMachineNotFound)
	malformed := operatorRequest(t, f.mux, http.MethodGet,
		"/v1/operator/machines/%20bad/evidence", "", "")
	assertAPIError(t, malformed, http.StatusBadRequest, "BAD_REQUEST")

	authorizer := &boundaryAuthorizer{decision: operatorauth.Decision{
		HTTPStatus: http.StatusForbidden, Code: operatorauth.CapabilityRequired,
		Detail: "view capability required",
	}}
	boundary := newOperatorBoundary(f.mux, authorizer, f.store, operatorRoutePolicies, testOperatorAuthority)
	denied := httptest.NewRecorder()
	request := newBoundaryRequest(http.MethodGet, path, nil)
	boundary.ServeHTTP(denied, request)
	assertAPIError(t, denied, http.StatusForbidden, string(operatorauth.CapabilityRequired))
	if calls := authorizer.snapshotCalls(); len(calls) != 1 || calls[0] != operatorauth.View {
		t.Fatalf("machine evidence authorization calls=%v want [view]", calls)
	}
}
