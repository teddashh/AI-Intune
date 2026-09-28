package main

import (
	"bufio"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/teddashh/AI-Intune/internal/operatorauth"
	"github.com/teddashh/AI-Intune/internal/operatorclient"
)

func TestOperatorJobEvidenceRouteIsViewJSONNoStoreAndBounded(t *testing.T) {
	f := observedOperatorFixture(t)
	jobID := createOperatorReadJob(t, f.store, f.machine.id, "cnode-operator", "EVIDENCE_SPEC")
	path := "/v1/operator/jobs/" + jobID + "/evidence"

	rec := operatorRequest(t, f.mux, http.MethodGet, path, "", "")
	if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("job evidence status=%d headers=%v body=%s", rec.Code, rec.Header(), rec.Body.String())
	}
	client, _ := operatorClientForMuxWithoutListener(t, f.mux, "http://100.64.0.9:8787")
	result, err := client.JobEvidence(t.Context(), jobID, 0)
	if err != nil || result.JobID != jobID || result.Disclosure.Limit != 100 {
		t.Fatalf("strict evidence decode=%+v err=%v", result, err)
	}

	for _, suffix := range []string{"?limit=0", "?limit=101", "?limit=abc", "?foo=1"} {
		bad := operatorRequest(t, f.mux, http.MethodGet, path+suffix, "", "")
		assertAPIError(t, bad, http.StatusBadRequest, "BAD_REQUEST")
	}
	missing := operatorRequest(t, f.mux, http.MethodGet,
		"/v1/operator/jobs/no-such-job/evidence", "", "")
	assertAPIError(t, missing, http.StatusNotFound, "JOB_NOT_FOUND")

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
		t.Fatalf("job evidence authorization calls=%v want [view]", calls)
	}
}

func operatorClientForMuxWithoutListener(t *testing.T, handler http.Handler, base string) (*operatorclient.Client, *atomic.Int64) {
	t.Helper()
	requests := &atomic.Int64{}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DialContext = func(_ context.Context, _, _ string) (net.Conn, error) {
		requests.Add(1)
		clientSide, serverSide := net.Pipe()
		go func() {
			defer serverSide.Close()
			request, err := http.ReadRequest(bufio.NewReader(serverSide))
			if err != nil {
				return
			}
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)
			_ = recorder.Result().Write(serverSide)
		}()
		return clientSide, nil
	}
	client, err := operatorclient.NewWithHTTPClient(base, &http.Client{Transport: transport})
	if err != nil {
		t.Fatal(err)
	}
	return client, requests
}
