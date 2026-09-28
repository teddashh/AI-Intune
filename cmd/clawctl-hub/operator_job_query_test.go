package main

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/teddashh/AI-Intune/internal/deploy"
)

func TestParseOperatorJobListRequestAcceptsOnlyCanonicalFilters(t *testing.T) {
	req := httptest.NewRequest("GET", "/v1/operator/jobs?machine_id=machine-1&state=running&state=failed&deployment_id=deployment-1&resource_kind=openclaw&resource_id=openclaw&limit=17&cursor=opaque", nil)
	got, err := parseOperatorJobListRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if got.MachineID != "machine-1" || got.DeploymentID != "deployment-1" ||
		got.ResourceKind != "openclaw" || got.ResourceID != "openclaw" ||
		got.Limit != 17 || got.Cursor != "opaque" || len(got.States) != 2 ||
		got.States[0] != deploy.Running || got.States[1] != deploy.Failed {
		t.Fatalf("parsed request=%+v", got)
	}
}

func TestParseOperatorJobListRequestFailsClosedBeforeServiceIO(t *testing.T) {
	for _, rawURL := range []string{
		"/v1/operator/jobs?",
		"/v1/operator/jobs?unknown=x",
		"/v1/operator/jobs?machine_id=",
		"/v1/operator/jobs?machine_id=a&machine_id=b",
		"/v1/operator/jobs?machine_id=%0Aowned",
		"/v1/operator/jobs?state=RUNNING",
		"/v1/operator/jobs?state=running&state=running",
		"/v1/operator/jobs?limit=0",
		"/v1/operator/jobs?limit=not-a-number",
		"/v1/operator/jobs?resource_id=openclaw",
		"/v1/operator/jobs?cursor=",
		"/v1/operator/jobs?machine_id=%zz",
	} {
		t.Run(strings.TrimPrefix(rawURL, "/v1/operator/jobs?"), func(t *testing.T) {
			req := httptest.NewRequest("GET", "/v1/operator/jobs", nil)
			if strings.HasSuffix(rawURL, "?") {
				req.URL.ForceQuery = true
			} else {
				req.URL.RawQuery = strings.TrimPrefix(rawURL, "/v1/operator/jobs?")
			}
			if _, err := parseOperatorJobListRequest(req); err == nil {
				t.Fatalf("query %q was accepted", rawURL)
			}
		})
	}
}
